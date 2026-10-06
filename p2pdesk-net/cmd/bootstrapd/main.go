// bootstrapd is the root node of a self-hosted p2pdesk network. It runs a
// server-mode kad DHT on a custom protocol prefix plus a circuitv2 relay,
// and dials no bootstrap peers of its own — the DHT it seeds never touches
// the public IPFS network.
//
// Usage:
//
//	bootstrapd -port 28432 -identity /var/lib/p2pdesk-bootstrap \
//	    -external-addr /ip4/PUBLIC_IP/tcp/28432 -protocol /p2pdesk
//
// It prints one line for clients to paste into the Custom Network setting:
//
//	/ip4/PUBLIC_IP/tcp/28432/p2p/12D3KooW...,/ip4/PUBLIC_IP/udp/28432/quic-v1/p2p/12D3KooW...
//
// The relay is the signaling path of the network (hole-punch signaling
// rides relay streams), so -relay stays on unless the operator replaces it
// with a dedicated relay elsewhere. Session data never traverses relays —
// the connect path refuses them.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/multiformats/go-multiaddr"

	"p2pdesk-net/nodekit"
)

func main() {
	port := flag.Int("port", 28432, "listen port (fixed; firewall must allow tcp+udp)")
	identity := flag.String("identity", "", "identity directory (default ~/.p2pdesk/bootstrapd)")
	externalAddr := flag.String("external-addr", "", "comma-separated public multiaddrs (no /p2p/ component)")
	bootstrap := flag.String("bootstrap", "", "comma-separated bootstrap multiaddrs; empty = root node (dial nothing)")
	protocol := flag.String("protocol", "/p2pdesk", "DHT protocol prefix (all devices must match)")
	relay := flag.Bool("relay", true, "run a circuitv2 relay (signaling path of the network)")
	flag.Parse()

	if *identity == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = "."
		}
		*identity = home + "/.p2pdesk/bootstrapd"
	}
	if err := nodekit.ValidateProtocolPrefix(*protocol); err != nil {
		slog.Error("invalid protocol", "error", err)
		os.Exit(1)
	}

	var bsPeers *[]string
	if strings.TrimSpace(*bootstrap) != "" {
		list := splitList(*bootstrap)
		bsPeers = &list
	} else {
		// Root node: an explicit empty list tells NewDHT to dial nothing.
		empty := []string{}
		bsPeers = &empty
	}

	cfg := &nodekit.NodeConfig{
		IdentityDir:    *identity,
		Mode:           "server",
		ExternalAddr:   *externalAddr,
		Port:           *port,
		BootstrapPeers: bsPeers,
		ProtocolPrefix: *protocol,
	}

	priv, err := nodekit.LoadIdentity(*identity)
	if err != nil {
		slog.Error("identity", "error", err)
		os.Exit(1)
	}

	hostOpts := nodekit.HostOpts{
		ConnMgrLow:  64,
		ConnMgrHigh: 128,
	}
	if *relay {
		hostOpts.RelayService = true
		// Small-network limits, looser than the relayv2 defaults where a
		// NAT behind a shared carrier exit would otherwise starve peers.
		// Session data never traverses relays, so abuse surface stays at
		// signaling volume; tighten here for larger networks.
		hostOpts.RelayResources = &relayv2.Resources{
			MaxReservations:        256,
			MaxCircuits:            16,
			MaxReservationsPerPeer: 4,
			MaxReservationsPerIP:   32,
			MaxReservationsPerASN:  64,
			ReservationTTL:         time.Hour,
		}
	}
	host, err := nodekit.NewHost(priv, cfg, hostOpts)
	if err != nil {
		slog.Error("host", "error", err)
		os.Exit(1)
	}
	defer host.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := nodekit.NewDHT(ctx, host, nodekit.DHTConfig{
		BootstrapPeers: cfg.BootstrapPeers,
		ProtocolPrefix: cfg.ProtocolPrefix,
		ServerMode:     true,
	})
	if err != nil {
		slog.Error("dht", "error", err)
		os.Exit(1)
	}
	defer d.Close()

	// Periodic refresh keeps the routing table healthy (root nodes have no
	// bootstrap to re-seed from).
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = d.RefreshRoutingTable()
			}
		}
	}()

	printInfo(host, *protocol, *relay, cfg.Port, *externalAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
}

// splitList splits comma/whitespace-separated entries, tolerating paste
// artifacts from the bootstrap output line.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	}) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// printInfo emits the fixed-format status line plus the paste-ready
// bootstrap value. The bootstrap line prefers the -external-addr addresses
// (what remote clients can actually dial) and appends the host's local
// addresses for LAN debugging.
func printInfo(host host.Host, protocol string, relayOn bool, port int, externalAddr string) {
	peerID := host.ID().String()
	slog.Info("bootstrap node ready",
		"peer", peerID,
		"protocol", protocol,
		"relay", relayOn,
		"port", port,
	)
	var addrs []string
	if strings.TrimSpace(externalAddr) != "" {
		for _, a := range splitList(externalAddr) {
			if m, err := multiaddr.NewMultiaddr(a); err == nil {
				addrs = append(addrs, m.Encapsulate(peerMA(peerID)).String())
			} else {
				slog.Warn("invalid external addr, ignoring", "addr", a, "error", err)
			}
		}
	}
	// Local addresses (real interface enumeration, loopback included) as
	// fallback / LAN reference.
	for _, a := range host.Addrs() {
		s := a.String()
		found := false
		for _, existing := range addrs {
			// existing is this address with a /p2p/ component appended.
			if strings.HasPrefix(existing, s) {
				found = true
				break
			}
		}
		if !found {
			if m, err := multiaddr.NewMultiaddr(s); err == nil {
				addrs = append(addrs, m.Encapsulate(peerMA(peerID)).String())
			}
		}
	}
	fmt.Println(strings.Join(addrs, ","))
}

func peerMA(peerID string) multiaddr.Multiaddr {
	m, _ := multiaddr.NewMultiaddr("/p2p/" + peerID)
	return m
}
