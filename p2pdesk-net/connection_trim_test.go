package main

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"p2pdesk-net/nodekit"
)

type delayedStreamHost struct {
	host.Host
	open func(context.Context, peer.ID, ...protocol.ID) (network.Stream, error)
}

func (h delayedStreamHost) NewStream(ctx context.Context, p peer.ID, protos ...protocol.ID) (network.Stream, error) {
	return h.open(ctx, p, protos...)
}

func TestPendingAndActiveSessionSurviveTrimming(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	bootstrap := []string{}
	n, err := startNode(&nodekit.NodeConfig{IdentityDir: t.TempDir(), Mode: "client", Port: port,
		BootstrapPeers: &bootstrap, ProtocolPrefix: "/p2pdesk-trim-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.stop)
	remote, err := simpleNode(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.stop)
	n.host.Peerstore().AddAddrs(remote.host.ID(), remote.host.Addrs(), peerstore.TempAddrTTL)
	base := n.host
	entered, release := make(chan struct{}), make(chan struct{})
	var failOpen atomic.Bool
	n.host = delayedStreamHost{Host: base, open: func(ctx context.Context, p peer.ID, protos ...protocol.ID) (network.Stream, error) {
		if failOpen.Load() {
			return nil, errors.New("injected stream negotiation failure")
		}
		close(entered)
		select {
		case <-release:
			return base.NewStream(ctx, p, protos...)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	type result struct {
		se   *StreamEntry
		code int
	}
	finished := make(chan result, 1)
	// The connect has to stay pending for longer than the endpoint grace period,
	// since the trim below only happens once that has elapsed.
	go func() {
		se, code := n.connect(remote.host.ID().String(), uint32(endpointConnGrace/time.Millisecond)+30000)
		finished <- result{se, code}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("pending connect did not reach stream open")
	}
	if !base.ConnManager().IsProtected(remote.host.ID(), "") {
		t.Fatal("pending session is not protected")
	}
	for i := 0; i < 28; i++ {
		idle, err := simpleNode(t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(idle.stop)
		if err := base.Connect(t.Context(), peer.AddrInfo{ID: idle.host.ID(), Addrs: idle.host.Addrs()}); err != nil {
			t.Fatal(err)
		}
	}
	// Let the endpoint grace expire, then invoke the same normal trim used
	// by the connection manager (never emergency ForceTrim).
	timer := time.NewTimer(endpointConnGrace + 500*time.Millisecond)
	defer timer.Stop()
	<-timer.C
	base.ConnManager().TrimOpenConns(t.Context())
	// The manager trims unprotected, out-of-grace connections down to its low
	// watermark, and the pending session stays protected, so that one peer is
	// expected on top of it.
	if got := len(base.Network().Conns()); got > endpointConnLow+1 {
		t.Fatalf("idle connections did not trim: %d (low watermark %d)", got, endpointConnLow)
	}
	if len(base.Network().ConnsToPeer(remote.host.ID())) == 0 {
		t.Fatal("pending session was trimmed")
	}
	close(release)
	r := <-finished
	if r.code != errOK {
		t.Fatalf("connect failed: %d", r.code)
	}
	if n.streamWrite(r.se.id, []byte("frame"), 1000) != 5 {
		t.Fatal("write after trim")
	}
	other, code := remote.accept(1000)
	if code != errOK {
		t.Fatal("accept after trim")
	}
	base.ConnManager().TrimOpenConns(t.Context())
	buf := make([]byte, 5)
	if remote.streamRead(other.id, buf, 1000) != 5 || string(buf) != "frame" {
		t.Fatal("active stream interrupted")
	}
	n.closeStream(r.se.id)
	if base.ConnManager().IsProtected(remote.host.ID(), "") {
		t.Fatal("session protection leaked")
	}
	// A failed attempt must release its temporary protection as well.
	failOpen.Store(true)
	if _, code := n.connect(remote.host.ID().String(), 5000); code == errOK {
		t.Fatal("expected stream failure")
	}
	if base.ConnManager().IsProtected(remote.host.ID(), "") {
		t.Fatal("failed attempt protection leaked")
	}
	t.Log("pending and active sessions survived idle trim; success/failure protection released")
}
