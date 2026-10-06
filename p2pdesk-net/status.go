package main

import (
	ma "github.com/multiformats/go-multiaddr"
)

// StatusSnapshot mirrors the Rust P2pdStatus struct.
type StatusSnapshot struct {
	ConnectedPeers    uint32
	RelayConnections  uint32
	DirectConnections uint32
	DHTReady          uint8
	RelayReady        uint8
	RoutingTableSize  uint32
	RelayReserved     uint8
}

// statusSnapshot is a cheap (lock-free-ish) snapshot for p2pd_node_status.
func (n *Node) statusSnapshot() StatusSnapshot {
	connected := 0
	relay := 0
	direct := 0
	for _, c := range n.host.Network().Conns() {
		connected++
		if c.Stat().Limited {
			relay++
		} else {
			direct++
		}
	}
	dhtReady := uint8(0)
	if n.dht.RoutingTable().Size() > 0 {
		dhtReady = 1
	}
	relayReady := uint8(0)
	if relay > 0 {
		relayReady = 1
	}
	// A relay slot is reserved when autorelay announces circuit addresses,
	// so their presence in the advertised address set is the reservation
	// signal (go-libp2p v0.49 exposes no direct reservation status API).
	relayReserved := uint8(0)
	for _, a := range n.host.Addrs() {
		if _, err := a.ValueForProtocol(ma.P_CIRCUIT); err == nil {
			relayReserved = 1
			break
		}
	}
	return StatusSnapshot{
		ConnectedPeers:    uint32(connected),
		RelayConnections:  uint32(relay),
		DirectConnections: uint32(direct),
		DHTReady:          dhtReady,
		RelayReady:        relayReady,
		RoutingTableSize:  uint32(n.dht.RoutingTable().Size()),
		RelayReserved:     relayReserved,
	}
}
