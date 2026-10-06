package main

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	ma "github.com/multiformats/go-multiaddr"

	"p2pdesk-net/nodekit"
)

// Exercise the production node setup over loopback: the remote peer accepts
// ping streams but never replies, while its control stream exchanges data.
// The old watchdog kills this healthy session after its 10s interval + 10s
// timeout. Actual connection loss must still wake the control stream reader.
func TestControlStreamSurvivesUnansweredPing(t *testing.T) {
	remote, err := simpleNode(t.TempDir()+"/remote", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.stop)
	var probes atomic.Int32
	remote.host.SetStreamHandler(ping.ID, func(s network.Stream) {
		probes.Add(1)
		defer s.Close()
		_, _ = io.Copy(io.Discard, s) // deliberately consume without replying
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	bootstrap := []string{}
	n, err := startNode(&nodekit.NodeConfig{
		IdentityDir: t.TempDir() + "/local", Mode: "client", Port: port,
		BootstrapPeers: &bootstrap, ProtocolPrefix: "/p2pdesk-ping-regression",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.stop)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	addr, err := ma.NewMultiaddr(remote.host.Addrs()[0].String())
	if err != nil {
		t.Fatal(err)
	}
	if err := n.host.Connect(ctx, peer.AddrInfo{ID: remote.host.ID(), Addrs: []ma.Multiaddr{addr}}); err != nil {
		t.Fatal(err)
	}
	s, err := n.host.NewStream(ctx, remote.host.ID(), controlProtocol)
	if err != nil {
		t.Fatal(err)
	}
	localStream := n.newStreamEntry(s, remote.host.ID())
	if count := n.streamWrite(localStream.id, []byte("kick"), 1000); count != 4 {
		t.Fatalf("handshake write: %d", count)
	}
	remoteStream, code := remote.accept(1000)
	if code != errOK {
		t.Fatalf("accept: %d", code)
	}
	buf := make([]byte, 65536)
	if count := remote.streamRead(remoteStream.id, buf, 1000); count != 4 {
		t.Fatalf("handshake read: %d", count)
	}
	if len(n.host.Network().ConnsToPeer(remote.host.ID())) != 1 {
		t.Fatal("test requires control and ping to share one connection")
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	end := time.NewTimer(23 * time.Second)
	defer end.Stop()
	for running := true; running; {
		select {
		case <-end.C:
			running = false
		case <-ticker.C:
			if count := remote.streamWrite(remoteStream.id, []byte("frame"), 1000); count != 5 {
				t.Fatalf("active control write failed after %d ping probes: %d", probes.Load(), count)
			}
			if count := n.streamRead(localStream.id, buf, 1000); count != 5 || string(buf[:count]) != "frame" {
				t.Fatalf("active control read failed after %d ping probes: %d", probes.Load(), count)
			}
		}
	}
	if count := probes.Load(); count != 0 {
		t.Fatalf("control session still creates background ping streams: %d", count)
	}
	// Retain the standard libp2p ping responder for other peers.
	probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
	results := ping.Ping(probeCtx, remote.host, n.host.ID())
	result, ok := <-results
	probeCancel()
	if !ok || result.Error != nil {
		t.Fatalf("standard ping responder failed: result=%+v open=%v", result, ok)
	}
	if err := remote.host.Network().ClosePeer(n.host.ID()); err != nil {
		t.Fatal(err)
	}
	if count := n.streamRead(localStream.id, buf, 1000); count > 0 || count == errTimeout {
		t.Fatalf("real connection loss was not surfaced: %d", count)
	}
	t.Log("PASS active data survived 23s; no session ping probes; standard ping responds; real connection loss detected")
}
