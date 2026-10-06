package main

import (
	"encoding/json"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"p2pdesk-net/nodekit"
)

// Explicit public-network experiment, isolated from the installed node and its
// identity. Age only this test node's routing-table timestamps to reproduce the
// maintenance after a long idle period without waiting fifteen minutes.
func TestDHTMaintenanceResearch(t *testing.T) {
	if os.Getenv("P2PDESK_DHT_RESEARCH") != "1" {
		t.Skip("explicit public DHT experiment")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := startNode(&nodekit.NodeConfig{IdentityDir: t.TempDir(), Mode: "server", Port: port})
	if err != nil {
		t.Fatal(err)
	}
	nodeMu.Lock()
	if node != nil {
		nodeMu.Unlock()
		n.stop()
		t.Fatal("experiment requires no other in-process node")
	}
	node = n
	nodeMu.Unlock()
	t.Cleanup(func() { nodeMu.Lock(); node = nil; nodeMu.Unlock(); n.stop() })
	remote, err := simpleNode(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.stop)
	n.host.Peerstore().AddAddrs(remote.host.ID(), remote.host.Addrs(), peerstore.TempAddrTTL)
	se, code := n.connect(remote.host.ID().String(), 10000)
	if code != errOK {
		t.Fatalf("control connect: %d", code)
	}
	if n.streamWrite(se.id, []byte("frame"), 1000) != 5 {
		t.Fatal("control handshake")
	}
	other, code := remote.accept(1000)
	if code != errOK {
		t.Fatalf("control accept: %d", code)
	}
	buf := make([]byte, 5)
	if remote.streamRead(other.id, buf, 1000) != 5 {
		t.Fatal("control read")
	}
	started := time.Now()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var pending <-chan error
	var refreshStarted time.Time
	var settledAt time.Time
	refreshed, peak, lastReported := false, 0, -1
	phase := "startup"
	for time.Since(started) < 240*time.Second && (settledAt.IsZero() || time.Since(settledAt) < 30*time.Second || time.Since(started) < 120*time.Second) {
		select {
		case err := <-pending:
			t.Logf("DHT_REFRESH done_ms=%d err=%v", time.Since(refreshStarted).Milliseconds(), err)
			pending = nil
			phase = "settling"
			settledAt = time.Now()
		case <-ticker.C:
			if n.streamWrite(se.id, []byte("frame"), 1000) != 5 || remote.streamRead(other.id, buf, 1000) != 5 || string(buf) != "frame" {
				t.Fatal("control stream interrupted by DHT maintenance")
			}
			connections := n.host.Network().Conns()
			if len(connections) > peak {
				peak = len(connections)
			}
			age := int(time.Since(started).Seconds())
			if age != lastReported && (age%5 == 0 || len(connections) >= 60) {
				protected, dhtStreams, relayStreams := 0, 0, 0
				for _, p := range n.host.Network().Peers() {
					if n.host.ConnManager().IsProtected(p, "") {
						protected++
					}
				}
				for _, c := range connections {
					for _, s := range c.GetStreams() {
						if strings.Contains(string(s.Protocol()), "/kad/") {
							dhtStreams++
						}
						if strings.Contains(string(s.Protocol()), "circuit") {
							relayStreams++
						}
					}
				}
				data, err := json.Marshal(map[string]any{"seconds": age, "phase": phase, "connections": len(connections), "peers": len(n.host.Network().Peers()), "rt": n.dht.RoutingTable().Size(), "protected": protected, "dht_streams": dhtStreams, "relay_streams": relayStreams, "peak": peak})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("DHT_SAMPLE %s", data)
				lastReported = age
			}
			if !refreshed && age >= 55 {
				if n.dht.RoutingTable().Size() == 0 {
					t.Fatal("public DHT did not bootstrap")
				}
				for _, p := range n.dht.RoutingTable().ListPeers() {
					n.dht.RoutingTable().UpdateLastSuccessfulOutboundQueryAt(p, time.Now().Add(-24*time.Hour))
				}
				refreshed, phase = true, "aged_refresh"
				refreshStarted = time.Now()
				pending = n.dht.RefreshRoutingTable()
				t.Logf("DHT_REFRESH start rt=%d connections=%d", n.dht.RoutingTable().Size(), len(connections))
			}
		}
	}
	if pending != nil {
		t.Fatal("refresh did not finish within the experiment")
	}
	t.Logf("DHT_RESULT peak=%d final_connections=%d final_rt=%d elapsed_ms=%d", peak, len(n.host.Network().Conns()), n.dht.RoutingTable().Size(), time.Since(started).Milliseconds())
	if peak >= 100 {
		t.Errorf("maintenance still reached %d connections", peak)
	}
	t.Log("DHT_CONTROL stream survived startup, stale-peer refresh and settling")
}
