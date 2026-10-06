package nodekit

import (
	"context"
	"fmt"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// DHTConfig selects the bootstrap and protocol namespace.
type DHTConfig struct {
	// BootstrapPeers: nil = public IPFS list, empty = root node (dial
	// nothing), list = custom network.
	BootstrapPeers *[]string
	// ProtocolPrefix: empty = /ipfs (public DHT); non-empty = /<prefix>,
	// which isolates this DHT from the public one in both directions:
	// peers advertising a different kad protocol fail validRTPeer and
	// never enter the routing table.
	ProtocolPrefix string
	// ServerMode pins the DHT to server mode (bootstrapd). The DLL relies
	// on ModeAuto, which switches on AutoNAT reachability.
	ServerMode bool
}

// NewDHT builds the kad DHT. A custom network with all-invalid bootstrap
// entries fails the node start instead of silently falling back to the
// public DHT — a node that was configured for a private network must not
// end up on the public one.
func NewDHT(ctx context.Context, h host.Host, cfg DHTConfig) (*dht.IpfsDHT, error) {
	var opts []dht.Option
	switch {
	case cfg.BootstrapPeers == nil:
		opts = append(opts, dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...))
	case len(*cfg.BootstrapPeers) == 0:
		// Root node: an empty list makes fixLowPeers skip dialing entirely,
		// so the node never reaches out to the public DHT.
		opts = append(opts, dht.BootstrapPeers())
	default:
		infos, bad := ParseBootstrapPeers(*cfg.BootstrapPeers)
		if len(infos) == 0 && len(bad) > 0 {
			return nil, fmt.Errorf("all %d bootstrap peers invalid", len(bad))
		}
		opts = append(opts, dht.BootstrapPeers(infos...))
	}
	if cfg.ProtocolPrefix != "" {
		if err := ValidateProtocolPrefix(cfg.ProtocolPrefix); err != nil {
			return nil, err
		}
		opts = append(opts, dht.ProtocolPrefix(protocol.ID(cfg.ProtocolPrefix)))
	}
	// The public /ipfs DHT requires Amino's K=20 for protocol compatibility.
	// K limits each bucket, not the total peer count. The DHT protects peers
	// in selected buckets; other routing peers are tagged. Alpha/beta and
	// lookup-check concurrency bound lookup bursts. Candidate validation is
	// separate from a lookup's alpha: its upstream default of 256 can dial
	// far more peers than the endpoint connection watermarks.
	if !cfg.ServerMode {
		opts = append(opts, dht.LookupCheckConcurrency(8), dht.RoutingTableCheckConcurrency(16), dht.RoutingTableRefreshWithoutFollowup())
	}
	if cfg.ProtocolPrefix != "" && cfg.ProtocolPrefix != "/ipfs" {
		alpha := 4
		if cfg.ServerMode {
			alpha = 2
			opts = append(opts, dht.LookupCheckConcurrency(1))
		}
		opts = append(opts,
			dht.BucketSize(10),
			dht.Concurrency(alpha),
			dht.Resiliency(2),
		)
	}
	// The stock kad-dht refresh period is intentionally aggressive for a
	// general-purpose node. Desktop/mobile endpoints only need to stay
	// discoverable, and their connect path performs an on-demand lookup.
	opts = append(opts, dht.RoutingTableRefreshPeriod(15*time.Minute))
	if cfg.ServerMode {
		opts = append(opts, dht.Mode(dht.ModeServer))
	}
	d, err := dht.New(ctx, h, opts...)
	if err != nil {
		return nil, err
	}
	// Bootstrap queues the first fixLowPeers round; it is the same
	// fail-fast gate the DLL always applied.
	if err := d.Bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	return d, nil
}
