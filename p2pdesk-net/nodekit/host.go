package nodekit

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	noise "github.com/libp2p/go-libp2p/p2p/security/noise"
	tls "github.com/libp2p/go-libp2p/p2p/security/tls"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/multiformats/go-multiaddr"
)

// HostOpts selects the optional host features. The DLL's server mode
// enables relay + AutoRelay + hole punching; the DLL's client mode enables
// none of them (the relay transport is on by default in go-libp2p, so
// dialing /p2p-circuit addresses works regardless); bootstrapd enables the
// relay service with resource limits so a custom-network node can serve as
// the signaling relay.
type HostOpts struct {
	// RelayService runs a circuitv2 relay (for other peers).
	RelayService   bool
	RelayResources *relayv2.Resources // limits; nil = relayv2 defaults

	// AutoRelay discovers relays on the DHT and advertises the /p2p-circuit
	// addresses. RelayPeerSource feeds it candidates (required when on).
	AutoRelay       bool
	RelayPeerSource autorelay.PeerSource

	// HolePunch enables libp2p's DCUtR hole punching.
	HolePunch bool

	// AddrFilter optionally rewrites the advertised address set (the DLL
	// uses it for the P2PDESK_DISABLE_IPV6 test switch).
	AddrFilter func([]multiaddr.Multiaddr) []multiaddr.Multiaddr

	// ConnManager watermarks. 0/0 disables the manager.
	ConnMgrLow  int
	ConnMgrHigh int
	// Endpoint-specific trimming; zero retains the relay/server defaults.
	ConnMgrGrace   time.Duration
	ConnMgrSilence time.Duration
}

// NewHost builds the libp2p host. Options follow the combination that
// was verified stable in production runs: hole punching + autonatv2 + NAT
// service + port map, noise+TLS security, 5s dial timeout, ConnManager,
// and an auto-scaled resource manager.
func NewHost(privKey crypto.PrivKey, cfg *NodeConfig, o HostOpts) (host.Host, error) {
	listenAddrs := ListenMultiaddrs(cfg.Port)
	opts := []libp2p.Option{
		libp2p.Identity(privKey),
		libp2p.ListenAddrs(listenAddrs...),
		// TCP + QUIC only. The default set also builds WebRTC/WebTransport,
		// which drags in pion; pion's ICE network monitor keeps rebinding a
		// netlink route socket that SELinux denies to untrusted apps on
		// Android, spamming avc denials. WebRTC targets browser interop, so
		// nothing is lost between two native peers. (The stdlib netlink
		// bind denial itself is fixed separately by the Android-only
		// -overlay patch, see docs/BUILD.md.)
		libp2p.NoTransports,
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Transport(libp2pquic.NewTransport),
		libp2p.EnableAutoNATv2(),
		libp2p.EnableNATService(),
		libp2p.NATPortMap(),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Security(tls.ID, tls.New),
		libp2p.SwarmOpts(swarm.WithDialTimeout(5 * time.Second)),
		libp2p.UserAgent("p2pdesk-net/1.0"),
	}
	if o.HolePunch {
		opts = append(opts, libp2p.EnableHolePunching())
	}
	if cfg.ExternalAddr != "" {
		if ext, err := multiaddr.NewMultiaddr(cfg.ExternalAddr); err == nil {
			base := o.AddrFilter
			opts = append(opts, libp2p.AddrsFactory(func(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
				out := append(addrs, ext)
				if base != nil {
					out = base(out)
				}
				return out
			}))
		} else {
			slog.Warn("Invalid external addr, ignoring", "addr", cfg.ExternalAddr, "error", err)
		}
	} else if o.AddrFilter != nil {
		opts = append(opts, libp2p.AddrsFactory(o.AddrFilter))
	}
	if o.RelayService {
		var relayOpts []relayv2.Option
		if o.RelayResources != nil {
			relayOpts = append(relayOpts, relayv2.WithResources(*o.RelayResources))
		}
		opts = append(opts, libp2p.EnableRelayService(relayOpts...))
	}
	if o.AutoRelay {
		opts = append(opts,
			libp2p.EnableAutoRelayWithPeerSource(
				o.RelayPeerSource,
				autorelay.WithMaxCandidateAge(30*time.Minute),
				autorelay.WithMinInterval(15*time.Minute),
				autorelay.WithNumRelays(3),
			),
		)
	}

	// ConnManager: trim idle connections above the high-water mark.
	if o.ConnMgrLow > 0 && o.ConnMgrHigh > 0 {
		// A shorter grace period prevents failed/short-lived DHT and relay
		// attempts from occupying the high-water mark for a full minute.
		grace := o.ConnMgrGrace
		if grace == 0 {
			grace = 30 * time.Second
		}
		cmOpts := []connmgr.Option{connmgr.WithGracePeriod(grace)}
		if o.ConnMgrSilence > 0 {
			cmOpts = append(cmOpts, connmgr.WithSilencePeriod(o.ConnMgrSilence))
		}
		cm, err := connmgr.NewConnManager(o.ConnMgrLow, o.ConnMgrHigh, cmOpts...)
		if err != nil {
			slog.Warn("ConnManager unavailable", "error", err)
		} else {
			opts = append(opts, libp2p.ConnectionManager(cm))
		}
	}

	// ResourceManager: auto-scaled limits.
	limiter := rcmgr.NewFixedLimiter(rcmgr.DefaultLimits.AutoScale())
	rm, err := rcmgr.NewResourceManager(limiter)
	if err != nil {
		slog.Warn("ResourceManager unavailable", "error", err)
	} else {
		opts = append(opts, libp2p.ResourceManager(rm))
	}

	return libp2p.New(opts...)
}

// FilterIPv6 strips IPv6 multiaddrs (P2PDESK_DISABLE_IPV6=1, NAT4 punch
// testing).
func FilterIPv6(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
	out := make([]multiaddr.Multiaddr, 0, len(addrs))
	for _, a := range addrs {
		if !strings.Contains(a.String(), "/ip6/") {
			out = append(out, a)
		}
	}
	return out
}

// ListenMultiaddrs: both TCP and QUIC, v4+v6. A fixed port keeps LAN
// discovery multiaddrs valid (the Rust side seeds them with P2PDESK_PORT).
func ListenMultiaddrs(port int) []multiaddr.Multiaddr {
	if port == 0 {
		port = 28431
	}
	var out []multiaddr.Multiaddr
	for _, s := range []string{
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
		fmt.Sprintf("/ip6/::/tcp/%d", port),
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
		fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port),
	} {
		if m, err := multiaddr.NewMultiaddr(s); err == nil {
			out = append(out, m)
		}
	}
	return out
}
