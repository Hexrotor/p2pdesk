package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	noise "github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/multiformats/go-multiaddr"

	"p2pdesk-net/holepunch"
	"p2pdesk-net/nodekit"
)

// simpleNode is a minimal Node without the full option set. `listen`
// true = ephemeral loopback TCP (port 0, never conflicts with real
// instances); false = NoListenAddrs (controller-style).
func simpleNode(dir string, listen bool) (*Node, error) {
	priv, err := nodekit.LoadIdentity(dir)
	if err != nil {
		return nil, err
	}
	var opts []libp2p.Option
	if listen {
		opts = append(opts, libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	} else {
		opts = append(opts, libp2p.NoListenAddrs)
	}
	opts = append(opts,
		libp2p.Identity(priv),
		libp2p.Security(noise.ID, noise.New),
	)
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, err
	}
	n := &Node{
		ctx:     context.Background(),
		cancel:  func() {},
		host:    h,
		inbound: make(chan *StreamEntry, 32),
		streams: make(map[uint64]*StreamEntry),
	}
	h.SetStreamHandler(controlProtocol, n.handleInbound)
	return n, nil
}

// TestTwoNodesDialAllAddrs: B dials A over loopback TCP and opens the
// control stream; A accepts and both sides exchange data through the
// blocking C-ABI-shaped stream API.
//
// NOTE: the peer's stream handler fires once the opening side writes — the
// yamux stream only materializes on the far side after the first write.
// Real rustdesk flows always write first (SignedId handshake), so this is
// the natural contract; p2pd_connect callers must not expect accept() to
// return before any bytes have been written.
func TestTwoNodesDialAllAddrs(t *testing.T) {
	privA, err := nodekit.LoadIdentity(t.TempDir() + "/a")
	if err != nil {
		t.Fatalf("identity A: %v", err)
	}
	privB, err := nodekit.LoadIdentity(t.TempDir() + "/b")
	if err != nil {
		t.Fatalf("identity B: %v", err)
	}
	ha, err := libp2p.New(
		libp2p.Identity(privA),
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.Security(noise.ID, noise.New),
	)
	if err != nil {
		t.Fatalf("host A: %v", err)
	}
	defer ha.Close()
	hb, err := libp2p.New(
		libp2p.Identity(privB),
		libp2p.NoListenAddrs,
		libp2p.Security(noise.ID, noise.New),
	)
	if err != nil {
		t.Fatalf("host B: %v", err)
	}
	defer hb.Close()

	a := &Node{ctx: context.Background(), cancel: func() {}, host: ha,
		inbound: make(chan *StreamEntry, 32), streams: make(map[uint64]*StreamEntry)}
	b := &Node{ctx: context.Background(), cancel: func() {}, host: hb,
		inbound: make(chan *StreamEntry, 32), streams: make(map[uint64]*StreamEntry)}

	ha.SetStreamHandler(controlProtocol, func(s network.Stream) {
		se := a.newStreamEntry(s, s.Conn().RemotePeer())
		a.inbound <- se
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var loopback multiaddr.Multiaddr
	for _, m := range ha.Addrs() {
		if s := m.String(); len(s) >= 20 && s[:9] == "/ip4/127." {
			loopback = m
			break
		}
	}
	if loopback == nil {
		t.Fatalf("no loopback addr on A: %v", ha.Addrs())
	}
	if err := hb.Connect(ctx, peer.AddrInfo{ID: ha.ID(), Addrs: []multiaddr.Multiaddr{loopback}}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	s, err := hb.NewStream(ctx, ha.ID(), controlProtocol)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	// First write materializes the stream on the far side (yamux) and is
	// consumed by the peer (like the SignedId handshake in rustdesk).
	if _, err := s.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	seB := b.newStreamEntry(s, ha.ID())
	seA, code := a.accept(5000)
	if code != errOK {
		t.Fatalf("accept: code %d (queue=%d)", code, len(a.inbound))
	}
	got := make([]byte, 16)
	if n := a.streamRead(seA.id, got, 5000); n != 4 || string(got[:n]) != "ping" {
		t.Fatalf("read handshake: n=%d q=%q", n, got[:n])
	}
	// Business data both ways.
	payload := []byte("hi")
	if n := b.streamWrite(seB.id, payload, 5000); n != int64(len(payload)) {
		t.Fatalf("write: %d", n)
	}
	if n := a.streamRead(seA.id, got, 5000); n != int64(len(payload)) || string(got[:n]) != "hi" {
		t.Fatalf("read: n=%d q=%q", n, got[:n])
	}
	b.closeStream(seB.id)
	a.closeStream(seA.id)
}

// TestTwoNodesStreamEcho: two real hosts in one process, connected over
// loopback; exercises connect-side stream open, server-side accept, framed
// echo both ways through the C-ABI-shaped stream API, and EOF on close.
func TestTwoNodesStreamEcho(t *testing.T) {
	dirA := t.TempDir() + "/a"
	dirB := t.TempDir() + "/b"
	a, err := simpleNode(dirA, true)
	if err != nil {
		t.Fatalf("simpleNode A: %v", err)
	}
	defer a.stop()
	b, err := simpleNode(dirB, true)
	if err != nil {
		t.Fatalf("simpleNode B: %v", err)
	}
	defer b.stop()

	// B dials A over loopback TCP.
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
	ai := peer.AddrInfo{ID: a.host.ID(), Addrs: []multiaddr.Multiaddr{loopback}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.host.Connect(ctx, ai); err != nil {
		t.Fatalf("connect A: %v", err)
	}

	// B opens the control stream; A accepts it. Both sides register
	// handles (this is what p2pd_connect / p2pd_accept do). The first write
	// materializes the stream on the far side (yamux) and is consumed by the
	// peer like the SignedId handshake.
	streamB, err := b.host.NewStream(ctx, a.host.ID(), controlProtocol)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	first := []byte("hello from B")
	if _, err := streamB.Write(first); err != nil {
		t.Fatalf("first write: %v", err)
	}
	seB := b.newStreamEntry(streamB, a.host.ID())
	seA, code := a.accept(5000)
	if code != errOK {
		t.Fatalf("accept: code %d", code)
	}
	if seA.peer.String() != b.host.ID().String() {
		t.Fatalf("accepted peer mismatch: %s != %s", seA.peer, b.host.ID())
	}
	if seA.pathKind != pathDirectTCP {
		t.Fatalf("path kind: want direct-tcp, got %d", seA.pathKind)
	}

	// A consumes the handshake, then both sides exchange business data
	// through the blocking stream API (mirrors the Rust worker calls).
	got := make([]byte, 64)
	if n := a.streamRead(seA.id, got, 5000); n != int64(len(first)) || string(got[:n]) != string(first) {
		t.Fatalf("A read handshake: n=%d data=%q", n, got[:n])
	}
	payload := []byte("second message")
	if n := b.streamWrite(seB.id, payload, 5000); n != int64(len(payload)) {
		t.Fatalf("B write: %d", n)
	}
	if n := a.streamRead(seA.id, got, 5000); n != int64(len(payload)) || string(got[:n]) != string(payload) {
		t.Fatalf("A read: n=%d data=%q", n, got[:n])
	}

	// A -> B.
	reply := []byte("ack from A")
	if n := a.streamWrite(seA.id, reply, 5000); n != int64(len(reply)) {
		t.Fatalf("A write: %d", n)
	}
	if n := b.streamRead(seB.id, got, 5000); n != int64(len(reply)) || string(got[:n]) != string(reply) {
		t.Fatalf("B read: n=%d data=%q", n, got[:n])
	}

	// Stream death: B closes; A's read must surface it (EOF or a negative
	// error code — libp2p Close() resets the stream, so the far side sees an
	// error rather than a clean EOF), and the handle table must forget the
	// stream (close is idempotent).
	b.closeStream(seB.id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if eof := a.streamRead(seA.id, got, 1000); eof <= 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("A never saw stream death after B closed")
		}
	}
	if code := b.streamWrite(seB.id, []byte("x"), 1000); code != errClosed {
		t.Fatalf("write after close: want errClosed, got %d", code)
	}
	var metaOut []byte
	if code, _ := b.streamPeerID(seB.id, metaOut); code != errClosed {
		t.Fatalf("metadata after close: want errClosed, got %d", code)
	}
	a.closeStream(seA.id) // idempotent double close
	a.closeStream(seA.id)
}

// bareNode is a Node without a host: enough for the handle table and the
// blocking stream API (newQuicStreamEntry never touches the host).
func bareNode() *Node {
	return &Node{
		ctx:     context.Background(),
		cancel:  func() {},
		inbound: make(chan *StreamEntry, 8),
		streams: make(map[uint64]*StreamEntry),
	}
}

// TestQuicStreamEntryRoundtrip: the first stream of a punched-QUIC session
// enters the handle table as pathCustomQUIC and exchanges data through the
// blocking stream API; closing one side tears the whole connection down via
// onClose (connmgr cannot see it), which the far side must observe.
func TestQuicStreamEntryRoundtrip(t *testing.T) {
	lnSock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer lnSock.Close()
	dSock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer dSock.Close()
	ln, err := holepunch.DirectListen(lnSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	tok, err := holepunch.GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	serverNode := bareNode()
	clientNode := bareNode()

	serverDone := make(chan error, 1)
	serverID := make(chan uint64, 1)
	go func() {
		conn, st, err := holepunch.AcceptAuthQUIC(ctx, ln, tok)
		if err != nil {
			serverDone <- err
			return
		}
		se := serverNode.newQuicStreamEntry(st, conn, lnSock, peer.ID("server-peer"))
		serverID <- se.id
		buf := make([]byte, 64)
		if n := serverNode.streamRead(se.id, buf, 5000); n != 3 {
			serverDone <- fmt.Errorf("server read: n=%d", n)
			return
		}
		if w := serverNode.streamWrite(se.id, []byte("ack"), 5000); w != 3 {
			serverDone <- fmt.Errorf("server write: %d", w)
			return
		}
		// The client closes below: conn-wide teardown must surface here.
		// A timeout only means the close has not landed yet — keep waiting
		// (breaking on errTimeout would let the test pass without ever
		// observing the teardown).
		deadline := time.Now().Add(5 * time.Second)
		for {
			r := serverNode.streamRead(se.id, buf, 1000)
			if r == errTimeout {
				if time.Now().After(deadline) {
					serverDone <- fmt.Errorf("server never saw the close")
					return
				}
				continue
			}
			if r <= 0 {
				break
			}
		}
		serverNode.closeStream(se.id)
		serverDone <- nil
	}()

	conn, st, err := holepunch.DirectDialQUIC(ctx, dSock, lnSock.LocalAddr(), tok)
	if err != nil {
		t.Fatal(err)
	}
	se := clientNode.newQuicStreamEntry(st, conn, dSock, peer.ID("client-peer"))
	if se.pathKind != pathCustomQUIC {
		t.Fatalf("path kind: want custom-quic (%d), got %d", pathCustomQUIC, se.pathKind)
	}
	if w := clientNode.streamWrite(se.id, []byte("hi!"), 5000); w != 3 {
		t.Fatalf("client write: %d", w)
	}
	buf := make([]byte, 64)
	if n := clientNode.streamRead(se.id, buf, 5000); n != 3 || string(buf[:n]) != "ack" {
		t.Fatalf("client read: n=%d q=%q", n, buf[:n])
	}
	clientNode.closeStream(se.id)
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	// The handle is gone from both tables; closeAllStreams is idempotent.
	clientNode.closeAllStreams()
	if got := clientNode.getStream(se.id); got != nil {
		t.Fatal("handle still present after close")
	}
	if got := serverNode.getStream(<-serverID); got != nil {
		t.Fatal("server handle still present after close")
	}
}
