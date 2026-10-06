package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/multiformats/go-multiaddr"
)

// dcutrWaitTimeout is how long to wait for libp2p's DCUtR to upgrade
// a relay connection to direct before giving up and using the relay path.
const dcutrWaitTimeout = 15 * time.Second

var pendingConnectID atomic.Uint64

// connect performs the full controller path: dial the peerstore addresses
// first (LAN discovery seeds them, identify refreshes them after every
// connect), and only when that fails or nothing is known, refresh via a DHT
// find and dial again -> DCUtR upgrade wait -> open the control stream.
// timeoutMs covers the whole operation (0 = none).
func (n *Node) connect(peerStr string, timeoutMs uint32) (*StreamEntry, int) {
	peerID, err := peer.Decode(peerStr)
	if err != nil {
		setLastError("invalid peer id: %v", err)
		return nil, errNotFound
	}
	// Cover dialing and the DCUtR wait, before newStreamEntry takes over.
	// A score tag alone does not prevent trimming. Unique tags also keep
	// concurrent attempts to the same peer from unprotecting one another.
	protectTag := fmt.Sprintf("pending-session-%d", pendingConnectID.Add(1))
	n.host.ConnManager().Protect(peerID, protectTag)
	defer n.host.ConnManager().Unprotect(peerID, protectTag)

	ctx := n.ctx
	var cancel context.CancelFunc
	if timeoutMs > 0 {
		ctx, cancel = context.WithTimeout(n.ctx, time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()
	}

	// Fresh attempt: drop stale connections to the peer so the new stream
	// never rides one left by a previous cycle.
	_ = n.host.Network().ClosePeer(peerID)

	// 1. Dial on the addresses already in the peerstore first — LAN
	// discovery seeds them and identify refreshes them after every connect,
	// so on the happy path the swarm races them and skips the DHT round
	// trip entirely. FindPeer below is only the refresh path: it runs when
	// the dial fails (NAT remap / IP change) or nothing is known yet.
	cached := peer.AddrInfo{ID: peerID, Addrs: n.host.Peerstore().Addrs(peerID)}
	dialed := false
	if len(cached.Addrs) > 0 {
		if err := n.dialClearBackoff(ctx, cached); err == nil {
			dialed = true
		} else {
			slog.Info("dial on cached addresses failed, refreshing via DHT",
				"peer", peerID.ShortString())
		}
	}

	if !dialed {
		// 2. DHT refresh path: cold-start gate, then the lookup. The gate
		// moved here so a dial on cached addresses (LAN in particular) no
		// longer waits on DHT readiness.
		n.waitForDHTReady(ctx)
		peerInfo, err := n.dht.FindPeer(ctx, peerID)
		if err != nil {
			if len(cached.Addrs) == 0 {
				setLastError("peer %s not found on DHT", peerID.ShortString())
				return nil, errNotFound
			}
			// The dial error above is already in lastError and is the more
			// accurate failure: the peer is known but unreachable.
			return nil, errInternal
		}
		if err := n.dialClearBackoff(ctx, peerInfo); err != nil {
			return nil, errInternal
		}
	}
	// 3. If the only connection is relayed, race libp2p's DCUtR upgrade
	// against the custom punch: DCUtR stays the fast path when it works,
	// the punch covers the combinations it cannot. Data must never traverse
	// a relay, so when neither wins, fail — NewStream with default options
	// would block on the direct upgrade until the remaining budget runs out.
	if n.isRelayOnly(peerID) {
		punchCtx, punchCancel := context.WithCancel(ctx)
		// Unbuffered handshake: the goroutine only completes the send when
		// the caller takes the result. If the caller cancels instead, the
		// goroutine's ctx case wins deterministically and it closes the
		// session itself — no send-after-drain race can orphan a live
		// punched session (conn + socket are invisible to connmgr).
		type punchResult struct {
			se  *StreamEntry
			err error
		}
		punchCh := make(chan punchResult)
		go func() {
			se, err := n.punchAndOpenStream(punchCtx, peerID)
			if err != nil && punchCtx.Err() != nil {
				// The connect attempt is gone; nobody listens. A live
				// session cannot coexist with an error, so nothing leaks.
				return
			}
			select {
			case punchCh <- punchResult{se, err}:
				// Delivered: the caller owns the session from here.
			case <-punchCtx.Done():
				// The connect attempt is gone; do not leak the session.
				if se != nil {
					n.closeStream(se.id)
				}
			}
		}()

		waitPunch := func() (*StreamEntry, int) {
			select {
			case r := <-punchCh:
				if r.err != nil {
					// The punch failed while the connect attempt is live:
					// report instead of idling out the whole budget.
					setLastError("custom punch failed: %v", r.err)
					return nil, errInternal
				}
				slog.Info("custom punch established the direct path",
					"peer", peerID.ShortString(), "remote", r.se.remote)
				return r.se, errOK
			case <-ctx.Done():
				punchCancel()
				setLastError("no direct connection to %s (relay path not used)", peerID.ShortString())
				return nil, errInternal
			}
		}

		if dcutrDisabled() {
			// NAT4 testing: libp2p's own hole punching is switched off so
			// only the custom punch can establish the direct path.
			return waitPunch()
		}

		waitCtx, waitCancel := context.WithTimeout(ctx, dcutrWaitTimeout)
		upgraded := n.waitForDirect(waitCtx, peerID)
		waitCancel()
		if upgraded {
			// DCUtR won: stop the punch. A session already handed over is
			// taken and dropped; one still in flight is closed by the
			// goroutine when the cancel lands.
			punchCancel()
			select {
			case r := <-punchCh:
				if r.se != nil {
					n.closeStream(r.se.id)
				}
			default:
			}
			slog.Info("DCUtR upgraded connection to direct", "peer", peerID.ShortString())
		} else {
			return waitPunch()
		}
	}

	// 4. Open the control stream on the (possibly upgraded) connection.
	s, err := n.host.NewStream(ctx, peerID, controlProtocol)
	if err != nil {
		setLastError("open stream to %s: %v", peerID.ShortString(), err)
		return nil, errInternal
	}
	se := n.newStreamEntry(s, peerID)
	slog.Info("control stream open",
		"peer", peerID.ShortString(),
		"path", pathKindName(se.pathKind))
	return se, errOK
}

// dialClearBackoff dials one AddrInfo with the peer's dial backoff cleared,
// so a retry after a failed attempt is never penalized. The swarm races
// every address; a dial failure leaves the detailed error in lastError.
func (n *Node) dialClearBackoff(ctx context.Context, pi peer.AddrInfo) error {
	if swrm, ok := n.host.Network().(*swarm.Swarm); ok {
		swrm.Backoff().Clear(pi.ID)
	}
	if err := n.host.Connect(ctx, pi); err != nil {
		setLastError("connect to %s: %v", pi.ID.ShortString(), err)
		return err
	}
	return nil
}

// waitForDHTReady blocks until the DHT routing table has at least one peer
// (bounded by ctx). FindPeer on an empty routing table fails immediately.
func (n *Node) waitForDHTReady(ctx context.Context) {
	for {
		if n.dht.RoutingTable().Size() > 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// isRelayOnly reports whether every connection to the peer is relayed.
func (n *Node) isRelayOnly(peerID peer.ID) bool {
	conns := n.host.Network().ConnsToPeer(peerID)
	if len(conns) == 0 {
		return false
	}
	for _, c := range conns {
		if !isRelayedConn(c) {
			return false
		}
	}
	return true
}

// isRelayedConn reports whether the connection rides a relay circuit. The
// circuit check is mandatory: a relay negotiated with WithInfiniteLimits
// does not set the Limited stat flag, and trusting the flag alone would
// misroute session data through such a relay.
func isRelayedConn(c network.Conn) bool {
	if c.Stat().Limited {
		return true
	}
	for _, p := range c.RemoteMultiaddr().Protocols() {
		if p.Code == multiaddr.P_CIRCUIT {
			return true
		}
	}
	return false
}

// dcutrDisabled reports whether the DCUtR wait is switched off
// (P2PDESK_DISABLE_DCUTR=1): NAT4 punch testing needs the punch path to
// run unmasked by libp2p's own hole punching.
func dcutrDisabled() bool { return os.Getenv("P2PDESK_DISABLE_DCUTR") == "1" }

// waitForDirect polls until a non-relay connection to the peer appears,
// bounded by ctx (the caller's remaining budget) and the node lifetime.
func (n *Node) waitForDirect(ctx context.Context, peerID peer.ID) bool {
	deadline := time.Now().Add(dcutrWaitTimeout)
	for {
		if !n.isRelayOnly(peerID) {
			return true
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return false
		case <-n.ctx.Done():
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// addPeerAddress seeds a peer's address into the peerstore (LAN discovery).
func (n *Node) addPeerAddress(peerStr, multiaddrStr string) int {
	peerID, err := peer.Decode(peerStr)
	if err != nil {
		setLastError("invalid peer id: %v", err)
		return errNotFound
	}
	addr, err := multiaddr.NewMultiaddr(multiaddrStr)
	if err != nil {
		setLastError("invalid multiaddr: %v", err)
		return errNotFound
	}
	n.host.Peerstore().AddAddr(peerID, addr, peerstore.PermanentAddrTTL)
	return errOK
}

// sign signs msg with the node identity; returns the raw 64-byte ed25519
// signature (the Rust side assembles signature||message for SignedId).
func (n *Node) sign(msg []byte, out []byte) (int, int) {
	priv := n.host.Peerstore().PrivKey(n.host.ID())
	if priv == nil {
		return errInternal, 0
	}
	sig, err := priv.Sign(msg)
	if err != nil {
		setLastError("sign: %v", err)
		return errInternal, 0
	}
	return errOK, copyLen(out, string(sig))
}

func pathKindName(kind uint32) string {
	switch kind {
	case pathDirectQUIC:
		return "direct-quic"
	case pathDirectTCP:
		return "direct-tcp"
	case pathCustomQUIC:
		return "custom-quic"
	case pathRelay:
		return "relay"
	}
	return "unknown"
}
