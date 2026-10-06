package holepunch

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// loopbackUDP creates a bound UDP socket and reports its address.
func loopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSocketArrayHit(t *testing.T) {
	const tid = 0x1234
	arr, err := NewSocketArray(8, tid, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer arr.Close()

	// The peer sprays our TID at one of the array sockets. The sockets are
	// bound to 0.0.0.0, so the peer must target 127.0.0.1 with the
	// socket's port.
	peer := loopbackUDP(t)
	defer peer.Close()
	target := &net.UDPAddr{
		IP:   net.IPv4(127, 0, 0, 1),
		Port: arr.socks[3].LocalAddr().(*net.UDPAddr).Port,
	}
	if err := sendPunchTo(peer, tid, target); err != nil {
		t.Fatal(err)
	}

	select {
	case ps := <-arr.Hit():
		if ps.UDPConn != arr.socks[3] {
			t.Fatalf("hit on wrong socket")
		}
		if ps.RemoteAddr.String() != peer.LocalAddr().String() {
			t.Fatalf("wrong remote: %v", ps.RemoteAddr)
		}
		arr.RetireOthers(ps)
	case <-time.After(5 * time.Second):
		t.Fatal("no hit reported")
	}

	// The winner replies to the observed source (the asymmetric-success
	// fast path): the peer must receive TID packets back from the winner
	// socket within the burst window. The burst survives RetireOthers —
	// the winner stays open under QUIC.
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	confirmed := false
	for {
		n, from, err := peer.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if from.Port != arr.socks[3].LocalAddr().(*net.UDPAddr).Port {
			continue
		}
		if tidGot, ok := ParsePunchPacket(buf[:n]); ok && tidGot == tid {
			confirmed = true
			break
		}
	}
	if !confirmed {
		t.Fatal("no hit-confirmation reply from the winner socket")
	}
}

func TestSocketArrayIgnoresForeignTID(t *testing.T) {
	arr, err := NewSocketArray(2, 0x1111, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer arr.Close()

	peer := loopbackUDP(t)
	defer peer.Close()
	target := &net.UDPAddr{
		IP:   net.IPv4(127, 0, 0, 1),
		Port: arr.socks[0].LocalAddr().(*net.UDPAddr).Port,
	}
	if err := sendPunchTo(peer, 0x2222, target); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arr.Hit():
		t.Fatal("foreign TID reported as hit")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSocketArraySendAllFanout(t *testing.T) {
	// A raw receiver counts the packets one SendAll cycle produces:
	// sockets × fanout.
	const n = 5
	recv := loopbackUDP(t)
	defer recv.Close()
	arr, err := NewSocketArray(n, 0x1, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer arr.Close()

	got := make(chan int, 1)
	go func() {
		buf := make([]byte, 64)
		count := 0
		deadline := time.Now().Add(3 * time.Second)
		_ = recv.SetReadDeadline(deadline)
		for time.Now().Before(deadline) {
			if _, _, err := recv.ReadFromUDP(buf); err != nil {
				break
			}
			count++
		}
		got <- count
	}()

	arr.SendAll(recv.LocalAddr().(*net.UDPAddr).Port)
	select {
	case count := <-got:
		if count != n*socketArrayFanout {
			t.Fatalf("received %d packets, want %d", count, n*socketArrayFanout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receiver timeout")
	}
}

// bindPortWindow binds a contiguous window of easySymSprayMaxPorts sockets
// on loopback, returning the base port and one socket per window port.
// Base ports are picked from the user range: OS ephemeral allocation
// (49k+) is fragmented by excluded ranges, so contiguous windows bind
// reliably only below the dynamic range.
func bindPortWindow(t *testing.T) (int, []*net.UDPConn) {
	t.Helper()
	for _, base := range []int{30000, 32000, 34000, 36000, 38000, 40000} {
		var socks []*net.UDPConn
		ok := true
		for i := 1; i <= easySymSprayMaxPorts; i++ {
			c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: base + i})
			if err != nil {
				ok = false
				break
			}
			socks = append(socks, c)
		}
		if ok {
			return base, socks
		}
		for _, c := range socks {
			_ = c.Close()
		}
	}
	t.Fatal("cannot bind a contiguous port window")
	return 0, nil
}

func TestSprayEasySymWindowCoverage(t *testing.T) {
	// One socket per window port: the spray must deliver 3 packets to each
	// of base+1..base+50 on both passes (6 packets per port).
	base, socks := bindPortWindow(t)
	for _, c := range socks {
		defer c.Close()
	}
	spray := loopbackUDP(t)
	defer spray.Close()

	var mu sync.Mutex
	counts := make(map[int]int, easySymSprayMaxPorts)
	var wg sync.WaitGroup
	for _, c := range socks {
		wg.Go(func() {
			buf := make([]byte, 64)
			_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
			for {
				_, _, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				mu.Lock()
				counts[c.LocalAddr().(*net.UDPAddr).Port]++
				mu.Unlock()
			}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := SprayEasySymWindow(ctx, spray, 0x1, net.IPv4(127, 0, 0, 1), base, 1); err != nil {
		t.Fatal(err)
	}
	// Let in-flight datagrams drain, then stop the readers.
	time.Sleep(300 * time.Millisecond)
	for _, c := range socks {
		_ = c.SetReadDeadline(time.Now())
	}
	wg.Wait()

	for i := 1; i <= easySymSprayMaxPorts; i++ {
		got := counts[base+i]
		if got != 2*sweepPacketsPerPort {
			t.Fatalf("port %d received %d packets, want %d",
				base+i, got, 2*sweepPacketsPerPort)
		}
	}
}

func TestSprayHardSymBirthdayContinuation(t *testing.T) {
	recv := loopbackUDP(t)
	defer recv.Close()
	spray := loopbackUDP(t)
	defer spray.Close()

	// Tiny vector so the walk order is deterministic.
	ports := []int{100, 200, 300, 400}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A count beyond the vector length caps at the vector: walking 5 from
	// index 1 covers 200,300,400,100 (the whole vector) -> next index 1.
	next, err := SprayHardSymBirthday(ctx, spray, 0x1, net.IPv4(127, 0, 0, 1), ports, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if next != 1 {
		t.Fatalf("next port index %d, want 1", next)
	}

	// Wrap-around: walk 3 starting at index 3: 400,100,200 -> next 2.
	next, err = SprayHardSymBirthday(ctx, spray, 0x1, net.IPv4(127, 0, 0, 1), ports, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if next != 2 {
		t.Fatalf("wrap next port index %d, want 2", next)
	}
}

func TestSprayConeBatchCount(t *testing.T) {
	recv := loopbackUDP(t)
	defer recv.Close()
	spray := loopbackUDP(t)
	defer spray.Close()

	count := make(chan int, 1)
	go func() {
		buf := make([]byte, 64)
		n := 0
		_ = recv.SetReadDeadline(time.Now().Add(10 * time.Second))
		for range 10 {
			if _, _, err := recv.ReadFromUDP(buf); err != nil {
				break
			}
			n++
		}
		count <- n
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := SprayConeBatch(ctx, spray, 0x1, recv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	if n := <-count; n != 10 {
		t.Fatalf("received %d packets, want 10", n)
	}
}

func TestPunchSymToConeRoundsUntilCtx(t *testing.T) {
	// No peer ever sprays our TID, so RunPunch must keep driving RPC
	// rounds until the context expires, advancing the port index each
	// round via the reply continuation.
	rpcCalls := 0
	rpc := func(ctx context.Context, req SignalMsg) (SignalMsg, error) {
		rpcCalls++
		if req.Kind != SigSpray {
			t.Errorf("unexpected request kind %q", req.Kind)
			return SignalMsg{}, errors.New("bad request kind")
		}
		if req.Round == 0 && req.MaxK2 < birthdayPacketsMin || req.MaxK2 > birthdayPacketsMax {
			t.Errorf("round 0 max_k2 %d outside [%d,%d]", req.MaxK2, birthdayPacketsMin, birthdayPacketsMax)
			return SignalMsg{}, errors.New("bad max_k2")
		}
		return SignalMsg{Kind: SigSprayRes, NextPortIndex: req.PortIndex + req.MaxK2}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	opts := PunchOptions{
		Method:   PunchSymToCone,
		PeerIP:   net.IPv4(127, 0, 0, 1),
		PeerPort: 40000,
		MyNAT:    NATSymmetricHard,
		PeerNAT:  NATCone,
		TID:      0xABCD,
		StunAddr: "127.0.0.1:1", // never used on the hard-sym path
	}
	_, err := RunPunch(ctx, opts, rpc)
	if err == nil {
		t.Fatal("RunPunch returned without a hit and without an error")
	}
	if ctx.Err() == nil {
		t.Fatalf("RunPunch failed with %v but ctx is still live", err)
	}
	// The round-0 backoff is 1 s and each round needs ~1 s of grace, so a
	// 3 s budget must produce at least 2 RPC calls.
	if rpcCalls < 2 {
		t.Fatalf("only %d RPC calls, want >= 2", rpcCalls)
	}
}

func TestPunchSymToConeEasyBranch(t *testing.T) {
	// Easy-sym path: the live port mapping is queried via STUN right
	// before each round and shipped as the spray base port.
	stun := newFakeStun(t, 0, fakePublicIP, 40000, 0, false, false, false)
	rpc := func(ctx context.Context, req SignalMsg) (SignalMsg, error) {
		if req.Kind != SigSpray {
			t.Errorf("unexpected request kind %q", req.Kind)
			return SignalMsg{}, errors.New("bad request kind")
		}
		if req.BasePort != 40000 {
			t.Errorf("base port %d, want the STUN-mapped 40000", req.BasePort)
			return SignalMsg{}, errors.New("bad base port")
		}
		// Reply without ever spraying back: the punch must keep retrying.
		return SignalMsg{Kind: SigSprayRes}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	opts := PunchOptions{
		Method:   PunchSymToCone,
		PeerIP:   net.IPv4(127, 0, 0, 1),
		PeerPort: 40000,
		MyNAT:    NATSymmetricEasyInc,
		PeerNAT:  NATCone,
		TID:      0xABCD,
		StunAddr: stun.addr.String(),
	}
	_, err := RunPunch(ctx, opts, rpc)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("want ctx expiry, got err=%v ctxErr=%v", err, ctx.Err())
	}
}

func TestPunchConeToConeNoHit(t *testing.T) {
	// The peer runs the requested spray but never lands our TID: the
	// punch fails cleanly after the grace window.
	rpc := func(ctx context.Context, req SignalMsg) (SignalMsg, error) {
		if req.Kind != SigSprayCone {
			t.Errorf("unexpected request kind %q", req.Kind)
			return SignalMsg{}, errors.New("bad request kind")
		}
		return SignalMsg{Kind: SigSprayConeRes}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := PunchOptions{
		Method:   PunchConeToCone,
		PeerIP:   net.IPv4(127, 0, 0, 1),
		PeerPort: 40000,
		MyNAT:    NATCone,
		PeerNAT:  NATCone,
		TID:      0xABCD,
	}
	if _, err := RunPunch(ctx, opts, rpc); err == nil {
		t.Fatal("want failure when the peer spray never lands")
	}
}
