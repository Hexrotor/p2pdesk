package main

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"p2pdesk-net/holepunch"
)

// startEchoStun runs a minimal STUN server on loopback: every binding
// request is answered with an XOR-MAPPED-ADDRESS carrying the source
// address. On loopback the mapping equals the socket's real address, which
// plays the role the NAT mapping plays in production — the punch protocol
// only ever targets mapped ports, never local ones. Returns the server
// address.
func startEchoStun(t *testing.T) string {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			resp := makeStunResponse(buf[:n], from)
			if resp == nil {
				continue
			}
			_, _ = c.WriteToUDP(resp, from)
		}
	}()
	return c.LocalAddr().String()
}

// makeStunResponse builds the binding response for a binding request:
// header + one XOR-MAPPED-ADDRESS attribute (RFC 5389). The transaction id
// is echoed so the caller's TID filter accepts it.
func makeStunResponse(req []byte, from *net.UDPAddr) []byte {
	if len(req) < 20 || binary.BigEndian.Uint16(req[0:2]) != 0x0001 { // binding request
		return nil
	}
	ip := from.IP.To4()
	if ip == nil {
		return nil
	}
	resp := make([]byte, 32)
	binary.BigEndian.PutUint16(resp[0:2], 0x0101) // binding response
	binary.BigEndian.PutUint16(resp[2:4], 12)     // one attribute
	copy(resp[4:20], req[4:20])                   // magic cookie + tid
	attr := resp[20:]
	binary.BigEndian.PutUint16(attr[0:2], 0x0020) // XOR-MAPPED-ADDRESS
	binary.BigEndian.PutUint16(attr[2:4], 8)
	attr[4] = 0
	attr[5] = 1 // IPv4
	binary.BigEndian.PutUint16(attr[6:8], uint16(from.Port)^0x2112)
	for i := range 4 {
		attr[8+i] = ip[i] ^ []byte{0x21, 0x12, 0xA4, 0x42}[i]
	}
	return resp
}

// TestPunchConeToConeLoopback runs the complete punch protocol between two
// hosts in one process: signaling over the loopback libp2p connection, TID
// spray targeting the mapped ports, and the token-authenticated QUIC session
// on the punched socket. The NAT classification is preset to cone on both
// sides — the echo STUN would classify the loopback host as NATOpen (no
// translation exists here), and classification itself is covered by the
// holepunch package tests. All production parameters are untouched: the
// test verifies node-layer wiring, not network robustness (that lives in
// the holepunch package tests and real-network runs).
func TestPunchConeToConeLoopback(t *testing.T) {
	stunAddr := startEchoStun(t)

	a, err := simpleNode(t.TempDir()+"/a", true)
	if err != nil {
		t.Fatalf("simpleNode A: %v", err)
	}
	defer a.stop()
	b, err := simpleNode(t.TempDir()+"/b", true)
	if err != nil {
		t.Fatalf("simpleNode B: %v", err)
	}
	defer b.stop()

	// Both sides learn socket mappings from the echo server (in production
	// this is the NAT's answer; the protocol contract is identical).
	a.stunList = []string{stunAddr}
	b.stunList = []string{stunAddr}

	// B dials A over loopback TCP; the punch signaling stream rides this
	// connection (in production it rides the relay connection).
	var loopback multiaddr.Multiaddr
	for _, m := range a.host.Addrs() {
		if s := m.String(); len(s) >= 20 && s[:9] == "/ip4/127." {
			loopback = m
			break
		}
	}
	if loopback == nil {
		t.Fatalf("no loopback addr on A: %v", a.host.Addrs())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.host.Connect(ctx, peer.AddrInfo{ID: a.host.ID(), Addrs: []multiaddr.Multiaddr{loopback}}); err != nil {
		t.Fatalf("connect A: %v", err)
	}

	// Preset cone classification on both sides (see the test comment).
	for _, n := range []*Node{a, b} {
		n.natMu.Lock()
		n.natInfo = holepunch.NATInfo{Type: holepunch.NATCone, PublicIP: "127.0.0.1"}
		n.natAt = time.Now()
		n.natMu.Unlock()
	}

	a.host.SetStreamHandler(punchProtocol, a.handlePunchInbound)

	se, err := b.punchAndOpenStream(ctx, a.host.ID())
	if err != nil {
		t.Fatalf("punch: %v", err)
	}
	defer b.closeStream(se.id)
	if se.pathKind != pathCustomQUIC {
		t.Fatalf("initiator path: want custom-quic (%d), got %d", pathCustomQUIC, se.pathKind)
	}

	// Business data over the punched QUIC session, both ways, through the
	// blocking C-ABI-shaped stream API.
	first := []byte("punch-ping")
	if n := b.streamWrite(se.id, first, 5000); n != int64(len(first)) {
		t.Fatalf("initiator write: n=%d", n)
	}
	seA, code := a.accept(5000)
	if code != errOK {
		t.Fatalf("responder accept: code %d (queue=%d)", code, len(a.inbound))
	}
	defer a.closeStream(seA.id)
	if seA.pathKind != pathCustomQUIC {
		t.Fatalf("responder path: want custom-quic (%d), got %d", pathCustomQUIC, seA.pathKind)
	}
	buf := make([]byte, 64)
	if n := a.streamRead(seA.id, buf, 5000); n != int64(len(first)) || string(buf[:n]) != string(first) {
		t.Fatalf("responder read: n=%d data=%q", n, buf[:n])
	}
	reply := []byte("punch-pong")
	if n := a.streamWrite(seA.id, reply, 5000); n != int64(len(reply)) {
		t.Fatalf("responder write: n=%d", n)
	}
	if n := b.streamRead(se.id, buf, 5000); n != int64(len(reply)) || string(buf[:n]) != string(reply) {
		t.Fatalf("initiator read: n=%d data=%q", n, buf[:n])
	}
}

// TestPunchRateLimiter: burst passes, exhaustion blocks, refill restores.
// Refill and cap are exercised by rolling lastRef back instead of sleeping:
// allow() recomputes from lastRef, so the code path under test is identical
// and the test runs in milliseconds.
func TestPunchRateLimiter(t *testing.T) {
	l := newPunchRateLimiter()
	for i := range punchRateBurst {
		if !l.allow() {
			t.Fatalf("burst token %d refused", i)
		}
	}
	if l.allow() {
		t.Fatal("bucket exhausted but allow() passed")
	}
	// A refill interval restores exactly one token.
	l.lastRef = time.Now().Add(-punchRateRefill - 50*time.Millisecond)
	if !l.allow() {
		t.Fatal("token after refill interval refused")
	}
	if l.allow() {
		t.Fatal("refill restored more than one token")
	}
	// Long idle caps at the burst, never above.
	l.lastRef = time.Now().Add(-4 * punchRateRefill)
	for range punchRateBurst {
		if !l.allow() {
			t.Fatal("capped burst token refused")
		}
	}
	if l.allow() {
		t.Fatal("bucket exceeded its cap")
	}
}

// TestPunchRateLimiterConcurrent: racing allow() calls never overdraw the
// burst — the mutex serializes refill and consumption. All calls land well
// inside one refill window, so at most punchRateBurst can be granted.
func TestPunchRateLimiterConcurrent(t *testing.T) {
	l := newPunchRateLimiter()
	var wg sync.WaitGroup
	var granted atomic.Int64
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow() {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := granted.Load(); got != int64(punchRateBurst) {
		t.Fatalf("concurrent allow() granted %d, want %d", got, punchRateBurst)
	}
}
