package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	noise "github.com/libp2p/go-libp2p/p2p/security/noise"

	"p2pdesk-net/nodekit"
)

// TestMinimalLibp2pEcho: bare libp2p hosts (no p2pdesk wrapper) — isolates
// whether the inbound-handler problem lives in libp2p usage or in our Node
// wrapper.
func TestMinimalLibp2pEcho(t *testing.T) {
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

	ha.SetStreamHandler(controlProtocol, func(s network.Stream) {
		_, _ = io.Copy(s, s) // echo
		_ = s.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hb.Connect(ctx, peer.AddrInfo{ID: ha.ID(), Addrs: ha.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	s, err := hb.NewStream(ctx, ha.ID(), controlProtocol)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	defer s.Close()
	if _, err := s.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 8)
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := s.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	t.Logf("echo received: %q", buf[:n])
}
