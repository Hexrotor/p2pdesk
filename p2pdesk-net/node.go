package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"p2pdesk-net/holepunch"
	"p2pdesk-net/nodekit"
)

// Control protocol opened by p2pd_connect and accepted server-side. Must
// match the Rust side (src/p2pdesk.rs integration).
const controlProtocol = "/p2pdesk/control/1.0.0"

// Grace is measured from a peer entry's first connection and resets only when
// that peer's last connection goes away, so it has to outlast the whole dial,
// punch and stream-setup window (the DCUtR wait included): otherwise the
// handshake of a peer this endpoint has seen before can be trimmed out from
// under it. A session arriving on an already established transport is covered
// by the session's own protection, which is applied once its control stream is
// registered.
const endpointConnGrace = 30 * time.Second

// Endpoint connection watermarks. The manager only starts trimming above the
// high watermark and stops at the low one.
const (
	endpointConnLow  = 20
	endpointConnHigh = 40
)

// Node owns the libp2p host, the DHT, and the stream handle table.
type Node struct {
	ctx    context.Context
	cancel context.CancelFunc

	host host.Host
	dht  *dht.IpfsDHT

	// Inbound control streams for p2pd_accept.
	inbound chan *StreamEntry

	streamMu   sync.Mutex
	streams    map[uint64]*StreamEntry
	nextHandle uint64

	// Punch state (holepunch integration).
	natMu    sync.Mutex
	natInfo  holepunch.NATInfo
	natAt    time.Time
	stunList []string

	// punchRate is a node-wide punch session limiter. It is global, not
	// per-peer, on purpose: PeerIds are mintable for free, so an attacker
	// can rotate identities to dodge any per-peer cooldown. The global
	// bucket caps how many punch sessions (and therefore spray rounds) the
	// node will serve in a window no matter how many identities ask.
	punchRate punchRateLimiter
}

// relayPeerSource feeds AutoRelay with relay candidates found via the DHT
// (closest peers are probed for relay support).
func relayPeerSource(ctx context.Context, numPeers int) <-chan peer.AddrInfo {
	ch := make(chan peer.AddrInfo, numPeers)
	go func() {
		defer close(ch)
		nodeMu.Lock()
		n := node
		nodeMu.Unlock()
		if n == nil || n.dht == nil {
			return
		}
		closestPeers, err := n.dht.GetClosestPeers(ctx, n.host.ID().String())
		if err != nil {
			slog.Debug("relay peer source: GetClosestPeers failed", "error", err)
			return
		}
		count := 0
		for _, peerID := range closestPeers {
			if numPeers > 0 && count >= numPeers {
				break
			}
			if peerID == n.host.ID() {
				continue
			}
			addrs := n.host.Peerstore().Addrs(peerID)
			if len(addrs) == 0 {
				continue
			}
			select {
			case ch <- peer.AddrInfo{ID: peerID, Addrs: addrs}:
				count++
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// startNode loads the identity, builds the host + DHT, and starts the
// inbound handler. Blocking until the host is listening.
func startNode(cfg *nodekit.NodeConfig) (*Node, error) {
	ctx, cancel := context.WithCancel(context.Background())

	isServer := cfg.Mode == "server"

	priv, err := nodekit.LoadIdentity(cfg.IdentityDir)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("load identity: %w", err)
	}

	hostOpts := nodekit.HostOpts{
		// P2PDESK_DISABLE_DCUTR=1: NAT4 punch testing. Without the
		// holepunch service this side registers no /libp2p/dcutr handler
		// and never dials back, so DCUtR negotiation fails on the wire
		// and the peer's custom punch takes over. For the punch path to
		// surface, set it on both ends: the initiator side (connect.go)
		// otherwise still waits out its DCUtR window first.
		HolePunch: os.Getenv("P2PDESK_DISABLE_DCUTR") != "1",
		// These are soft connection watermarks. Protected DHT/relay/session
		// peers and connections in their grace period can exceed them.
		//
		// The grace period has to outlast a peer's reconnect cycle: a session
		// that reuses an already established transport while the node sits
		// above the high watermark would otherwise have that transport trimmed
		// out from under it, and the reconnect would repeat the same way.
		ConnMgrLow:     endpointConnLow,
		ConnMgrHigh:    endpointConnHigh,
		ConnMgrGrace:   endpointConnGrace,
		ConnMgrSilence: 30 * time.Second,
	}
	// Address filtering. P2PDESK_DISABLE_IPV6=1 drops v6 addresses at the
	// dial layer: v6 has no NAT, so a v6 path would mask NAT4 punch testing.
	if os.Getenv("P2PDESK_DISABLE_IPV6") == "1" {
		hostOpts.AddrFilter = nodekit.FilterIPv6
	}
	if isServer {
		hostOpts.AutoRelay = true
		hostOpts.RelayPeerSource = relayPeerSource
	}
	host, err := nodekit.NewHost(priv, cfg, hostOpts)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create host: %w", err)
	}

	d, err := nodekit.NewDHT(ctx, host, nodekit.DHTConfig{
		BootstrapPeers: cfg.BootstrapPeers,
		ProtocolPrefix: cfg.ProtocolPrefix,
	})
	if err != nil {
		cancel()
		_ = host.Close()
		return nil, fmt.Errorf("create dht: %w", err)
	}
	// kad-dht owns periodic refresh and low-table recovery. A second ticker
	// here used to queue redundant self lookups and liveness checks.

	n := &Node{
		ctx:       ctx,
		cancel:    cancel,
		host:      host,
		dht:       d,
		inbound:   make(chan *StreamEntry, 32),
		streams:   make(map[uint64]*StreamEntry),
		stunList:  parseStunServers(),
		punchRate: newPunchRateLimiter(),
	}
	host.SetStreamHandler(controlProtocol, n.handleInbound)
	go n.monitorConnections(host)
	if isServer {
		// Responder side of hole punching: only servers accept punch
		// sessions, and the initial NAT classification warms up here so the
		// first punch request does not pay the detection latency.
		host.SetStreamHandler(punchProtocol, n.handlePunchInbound)
		go n.warmNAT()
	}
	slog.Info("p2pdesk-net node ready", "peer_id", host.ID(), "addrs", host.Addrs())
	return n, nil
}

func (n *Node) stop() {
	n.cancel()
	// Closing the host wakes every blocking read/write/accept/connect.
	_ = n.host.Close()
	// Punched QUIC streams are invisible to the host; close them (and their
	// sockets) explicitly so a repeated node lifetime in this process
	// cannot leak them.
	n.closeAllStreams()
	if n.dht != nil {
		_ = n.dht.Close()
	}
}

// handleInbound queues an inbound control stream for p2pd_accept. Runs on
// the swarm's handler goroutine — a panic here would kill the host process
// (the DLL shares it), so guard it like every other goroutine.
func (n *Node) handleInbound(s network.Stream) {
	defer func() {
		if rec := recover(); rec != nil {
			setLastError("inbound handler panic: %v", rec)
			_ = s.Close()
		}
	}()
	conn := s.Conn()
	var peerID peer.ID
	if conn != nil {
		peerID = conn.RemotePeer()
	}
	se := n.newStreamEntry(s, peerID)
	select {
	case n.inbound <- se:
	case <-n.ctx.Done():
		_ = s.Close()
	}
}

// accept blocks (up to timeoutMs; 0 = forever) for an inbound stream.
func (n *Node) accept(timeoutMs uint32) (*StreamEntry, int) {
	if timeoutMs == 0 {
		select {
		case se := <-n.inbound:
			return se, errOK
		case <-n.ctx.Done():
			return nil, errClosed
		}
	}
	select {
	case se := <-n.inbound:
		return se, errOK
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil, errTimeout
	case <-n.ctx.Done():
		return nil, errClosed
	}
}

// node singleton (one host per process, like the Rust side).
var nodeMu sync.Mutex
var node *Node
