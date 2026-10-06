package main

import (
	"log/slog"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
)

// Low-volume evidence for bursts: one entry at onset, at most one every 30s
// while busy, and one after settling. This does not dial or trim any peer.
func (n *Node) monitorConnections(h host.Host) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var busy bool
	var lastLog time.Time
	peak := 0
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
		}
		connections := h.Network().Conns()
		peak = max(peak, len(connections))
		onset := !busy && len(connections) >= 48
		settled := busy && len(connections) < 24
		if !onset && !settled && !(busy && time.Since(lastLog) >= 30*time.Second) {
			continue
		}
		if onset {
			busy = true
		}
		state := "busy"
		if settled {
			busy, state = false, "settled"
		}
		peers := h.Network().Peers()
		protected, dhtStreams, relayStreams := 0, 0, 0
		for _, p := range peers {
			if h.ConnManager().IsProtected(p, "") {
				protected++
			}
		}
		for _, c := range connections {
			for _, s := range c.GetStreams() {
				p := string(s.Protocol())
				if strings.Contains(p, "/kad/") {
					dhtStreams++
				}
				if strings.Contains(p, "circuit") {
					relayStreams++
				}
			}
		}
		routingTable := 0
		if n.dht != nil {
			routingTable = n.dht.RoutingTable().Size()
		}
		slog.Info("p2p connection maintenance", "state", state, "connections", len(connections),
			"peers", len(peers), "peak_sampled", peak, "routing_table", routingTable,
			"protected", protected, "dht_streams", dhtStreams, "relay_streams", relayStreams)
		lastLog = time.Now()
		if settled {
			peak = len(connections)
		}
	}
}
