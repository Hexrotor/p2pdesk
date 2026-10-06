package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"p2pdesk-net/holepunch"
)

// punchProtocol is the signaling channel for hole punching. The stream is
// opened over the existing libp2p connection (relay included) — signaling
// only; punch datagrams never traverse it. Must match the Rust side's
// expectations for inbound session streams (none: punch streams are
// internal to the Go layer).
const punchProtocol = "/p2pdesk/punch/1.0.0"

// Global punch-session rate budget. Burst covers rapid legitimate retries;
// the refill rate bounds how much spray traffic a Sybil attacker can drive
// out of this node per minute: with ~0.5 MB of spray packets per session,
// the sustained ceiling is ~1 MB/min on top of the 1.5 MB burst. The bucket
// is a damage cap, not access control: an attacker who keeps it drained can
// starve legitimate sessions, at the bounded cost of their own spray.
const (
	punchRateBurst  = 3
	punchRateRefill = 30 * time.Second
)

// punchRateLimiter is a tiny token bucket shared by all peers (see Node).
// The zero value starts full: an idle bucket refills from its zero lastRef,
// so it is not a locked-by-default gate. Construct via newPunchRateLimiter
// (or drain it explicitly) where the limit should apply from the first call.
type punchRateLimiter struct {
	mu      sync.Mutex
	tokens  float64
	lastRef time.Time
}

func newPunchRateLimiter() punchRateLimiter {
	return punchRateLimiter{tokens: punchRateBurst, lastRef: time.Now()}
}

// allow consumes one token if the bucket has one.
func (l *punchRateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens += now.Sub(l.lastRef).Seconds() / punchRateRefill.Seconds()
	if l.tokens > punchRateBurst {
		l.tokens = punchRateBurst
	}
	l.lastRef = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

const (
	// punchHelloTimeout bounds the hello/hello_ack exchange.
	punchHelloTimeout = 10 * time.Second
	// punchSignalOpenTimeout bounds opening the signaling stream itself:
	// a half-dead relay would otherwise hang the NewStream until the whole
	// connect budget runs out, silently killing the punch. A fast failure
	// here retries (fresh stream), which also re-probes the relay path.
	punchSignalOpenTimeout = 4 * time.Second
	// punchSessionTimeout bounds the responder's wait for a TID hit and
	// the QUIC accept after punch_ready.
	punchSessionTimeout = 30 * time.Second
	// punchRetryWindow: a failed attempt is retried once only when it died
	// this fast (fresh NAT data / warming peer). A slow failure means the
	// NAT combination does not work; another full round wastes the budget.
	punchRetryWindow = 10 * time.Second
)

// defaultStunServers are used unless P2PDESK_STUN_SERVERS overrides them
// (comma-separated list). Detection needs >= 2 responding servers, so the
// list mixes global and region-local providers: global endpoints can be
// unreachable or slow on some mobile networks, where the region-local
// server (stun.miwifi.com, best-effort public service) carries the
// detection instead. NOTE: punch-time mapping queries use [0] only.
var defaultStunServers = []string{
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
	"stun.cloudflare.com:3478",
	"stun.miwifi.com:3478",
}

// stunServers returns the configured STUN server list. Parsed once at node
// start; a config error falls back to the defaults.
func (n *Node) stunServers() []string {
	if len(n.stunList) > 0 {
		return n.stunList
	}
	return defaultStunServers
}

// parseStunServers reads P2PDESK_STUN_SERVERS (comma-separated) or the
// defaults.
func parseStunServers() []string {
	raw := strings.TrimSpace(os.Getenv("P2PDESK_STUN_SERVERS"))
	if raw == "" {
		return defaultStunServers
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// natFresh reports whether the cached classification is still valid
// (caller holds natMu). Unknown classifications retry eagerly.
func (n *Node) natFreshLocked() (holepunch.NATInfo, bool) {
	ttl := holepunch.ReDetectInterval
	if n.natInfo.Type == holepunch.NATUnknown {
		ttl = holepunch.ReDetectUnknownInterval
	}
	if !n.natAt.IsZero() && time.Since(n.natAt) < ttl {
		return n.natInfo, true
	}
	return holepunch.NATInfo{}, false
}

// detectNAT returns the NAT classification, cached for the frozen TTL.
// Fast path reads under the lock; the slow path detects outside it so
// concurrent callers never serialize on a 3 s STUN round trip (the last
// write wins — detection is idempotent).
func (n *Node) detectNAT(ctx context.Context) (holepunch.NATInfo, error) {
	n.natMu.Lock()
	if info, ok := n.natFreshLocked(); ok {
		n.natMu.Unlock()
		return info, nil
	}
	n.natMu.Unlock()

	info, err := holepunch.DetectNAT(ctx, n.stunServers())
	if err != nil {
		return holepunch.NATInfo{}, err
	}
	n.natMu.Lock()
	n.natInfo, n.natAt = info, time.Now()
	n.natMu.Unlock()
	slog.Info("NAT classification",
		"type", info.Type, "subtype", info.Subtype,
		"public", fmt.Sprintf("%s:%d", info.PublicIP, info.PublicPort))
	return info, nil
}

// warmNAT runs the initial detection in the background so a server's first
// punch request does not pay the detection latency.
func (n *Node) warmNAT() {
	ctx, cancel := context.WithTimeout(n.ctx, 15*time.Second)
	defer cancel()
	if _, err := n.detectNAT(ctx); err != nil {
		slog.Debug("NAT warmup failed", "error", err)
	}
}

// punchAndOpenStream runs the custom UDP punch for a peer reachable only
// via relay and returns the punched QUIC stream entry. Retried once after
// a fast failure; a slow failure aborts immediately.
func (n *Node) punchAndOpenStream(ctx context.Context, peerID peer.ID) (*StreamEntry, error) {
	start := time.Now()
	for attempt := range 2 {
		se, err := n.punchAttempt(ctx, peerID)
		if err == nil {
			return se, nil
		}
		if attempt == 0 && time.Since(start) < punchRetryWindow {
			slog.Info("punch attempt failed fast, retrying",
				"peer", peerID.ShortString(), "error", err)
			continue
		}
		return nil, err
	}
	return nil, errors.New("punch failed")
}

// punchAttempt is one full punch: NAT classification, hello exchange over
// the signaling stream, the spray rounds, punch_ready, and the QUIC dial.
func (n *Node) punchAttempt(ctx context.Context, peerID peer.ID) (*StreamEntry, error) {
	myNAT, err := n.detectNAT(ctx)
	if err != nil {
		return nil, fmt.Errorf("NAT detection: %w", err)
	}
	myIP := net.ParseIP(myNAT.PublicIP)
	if myIP == nil || myIP.To4() == nil {
		return nil, errors.New("punch requires a v4 public mapping")
	}

	tok, err := holepunch.GenerateSessionToken()
	if err != nil {
		return nil, err
	}
	tid := mrand.Uint32()

	// Signaling rides the established (possibly relayed) connection. The
	// open has its own timeout: on a half-dead relay NewStream can hang
	// for the entire remaining connect budget, killing the punch silently
	// instead of letting it retry.
	sigCtx, sigCancel := context.WithTimeout(ctx, punchSignalOpenTimeout)
	defer sigCancel()
	s, err := n.host.NewStream(network.WithAllowLimitedConn(sigCtx, "punch"), peerID, punchProtocol)
	if err != nil {
		return nil, fmt.Errorf("punch signaling stream: %w", err)
	}
	defer s.Close()
	sig := holepunch.NewSignalingClient(s)
	defer sig.Close()

	// Hello exchange: NAT info both ways; the ack carries the responder's
	// punch socket mapped port, which becomes the spray target.
	_ = s.SetDeadline(time.Now().Add(punchHelloTimeout))
	if err := holepunch.WriteSignal(s, holepunch.SignalMsg{
		Kind:  holepunch.SigHello,
		MyNAT: myNAT,
		TID:   tid,
		Token: tok,
	}); err != nil {
		return nil, fmt.Errorf("punch hello: %w", err)
	}
	ack, err := holepunch.ReadSignal(s)
	if err != nil {
		return nil, fmt.Errorf("punch hello ack: %w", err)
	}
	if ack.Kind == holepunch.SigError {
		return nil, fmt.Errorf("peer refused punch: %s", ack.Error)
	}
	if ack.Kind != holepunch.SigHelloAck {
		return nil, fmt.Errorf("expected hello_ack, got %s", ack.Kind)
	}
	peerIP := net.ParseIP(ack.MyNAT.PublicIP)
	if peerIP == nil || peerIP.To4() == nil {
		return nil, errors.New("peer punch requires a v4 public mapping")
	}

	method := holepunch.DeterminePunchMethod(myNAT.Type, ack.MyNAT.Type)
	if method == holepunch.PunchNone {
		return nil, fmt.Errorf("NAT combination %s/%s not punchable", myNAT.Type, ack.MyNAT.Type)
	}

	ps, err := holepunch.RunPunch(ctx, holepunch.PunchOptions{
		Method:   method,
		PeerIP:   peerIP,
		PeerPort: ack.PeerPort,
		MyNAT:    myNAT.Type,
		PeerNAT:  ack.MyNAT.Type,
		TID:      tid,
		StunAddr: n.stunServers()[0],
	}, sig.Call)
	if err != nil {
		return nil, fmt.Errorf("punch: %w", err)
	}

	// Hand the punched socket to QUIC. The responder's socket becomes the
	// QUIC listener after punch_ready; our dial targets the address the
	// TID datagrams came from, which is that socket's mapping. On every
	// failure past this point the socket is ours to close: quic-go never
	// closes caller-provided packet conns, and the entry's onClose (which
	// would) only exists once the stream entry is created.
	_ = s.SetDeadline(time.Now().Add(punchHelloTimeout))
	if err := holepunch.WriteSignal(s, holepunch.SignalMsg{Kind: holepunch.SigPunchReady}); err != nil {
		_ = ps.UDPConn.Close()
		return nil, fmt.Errorf("punch_ready: %w", err)
	}
	quicConn, qs, err := holepunch.DirectDialQUIC(ctx, ps.UDPConn, ps.RemoteAddr, tok)
	if err != nil {
		_ = ps.UDPConn.Close()
		return nil, fmt.Errorf("punched QUIC dial: %w", err)
	}
	se := n.newQuicStreamEntry(qs, quicConn, ps.UDPConn, peerID)
	slog.Info("punched QUIC session established",
		"peer", peerID.ShortString(), "method", method, "remote", se.remote)
	return se, nil
}

// handlePunchInbound serves the responder side of one punch session:
// hello -> spray requests -> punch_ready -> QUIC accept -> the session
// stream enters the inbound queue (p2pd_accept sees it like any other).
// Runs on the swarm handler goroutine; the QUIC accept runs in its own
// goroutine after punch_ready.
func (n *Node) handlePunchInbound(s network.Stream) {
	defer func() {
		if rec := recover(); rec != nil {
			setLastError("punch handler panic: %v", rec)
			_ = s.Close()
		}
	}()
	var peerID peer.ID
	if c := s.Conn(); c != nil {
		peerID = c.RemotePeer()
		protectTag := fmt.Sprintf("pending-punch-%d", pendingConnectID.Add(1))
		n.host.ConnManager().Protect(peerID, protectTag)
		defer n.host.ConnManager().Unprotect(peerID, protectTag)
	}
	ctx := n.ctx

	// The node-wide rate gate runs before anything else — before reading
	// the hello even: an attacker rotating PeerIds must pay the same global
	// budget a legitimate retry does, and a rate-limited session costs the
	// node nothing but the refusal frame.
	if !n.punchRate.allow() {
		_ = s.SetDeadline(time.Now().Add(punchHelloTimeout))
		_ = holepunch.WriteSignal(s, holepunch.SignalMsg{
			Kind:  holepunch.SigError,
			Error: fmt.Sprintf("punch rate limited, retry in about %ds", int(punchRateRefill.Seconds())),
		})
		// CloseWrite (not Reset) so the refusal frame is flushed and
		// delivered; the initiator's teardown closes the read side.
		_ = s.CloseWrite()
		slog.Info("punch refused by rate limiter", "peer", peerID.ShortString())
		return
	}

	_ = s.SetDeadline(time.Now().Add(punchHelloTimeout))
	hello, err := holepunch.ReadSignal(s)
	if err != nil {
		slog.Debug("punch hello read failed", "error", err)
		return
	}
	refuse := func(reason string) {
		_ = holepunch.WriteSignal(s, holepunch.SignalMsg{Kind: holepunch.SigError, Error: reason})
	}
	if hello.Kind != holepunch.SigHello {
		refuse("expected hello")
		return
	}
	slog.Info("punch hello received",
		"peer", peerID.ShortString(),
		"initiator_nat", hello.MyNAT.Type,
		"initiator_public", hello.MyNAT.PublicIP)
	peerIP := net.ParseIP(hello.MyNAT.PublicIP)
	if peerIP == nil || peerIP.To4() == nil {
		refuse("initiator has no v4 public mapping")
		return
	}

	myNAT, err := n.detectNAT(ctx)
	if err != nil {
		refuse(fmt.Sprintf("NAT detection: %v", err))
		return
	}
	method := holepunch.DeterminePunchMethod(myNAT.Type, hello.MyNAT.Type)
	if method == holepunch.PunchNone {
		refuse(fmt.Sprintf("NAT combination %s/%s not punchable", myNAT.Type, hello.MyNAT.Type))
		return
	}

	// sessionCtx bounds every asynchronous piece of this punch session: the
	// both-easy spray goroutine and the QUIC accept. It dies either with
	// the handler (premature end — the deferred cleanup below) or with
	// acceptPunchedQUIC (normal handoff or session timeout), so no piece
	// outlives the session.
	sessionCtx, sessionCancel := context.WithCancel(ctx)

	// Responder state: a cone responder listens on one socket whose mapped
	// port goes into the ack (the initiator sprays it). The both-easy
	// responder builds its array per request. hitResult is unbuffered: the
	// spray goroutine only completes its delivery into a live receiver, and
	// on sessionCtx death it closes the winner itself — no delivery window
	// can orphan a socket.
	st := &punchPeerState{
		hitResult: make(chan *holepunch.PunchedSocket),
	}
	// Array ownership: every failure path below closes it via the deferred
	// cleanup; on success the ownership moves to acceptPunchedQUIC (the
	// handler must not close it after punch_ready — the QUIC listener
	// needs the winning socket).
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		// Premature end: kill the async spray first (its delivery either
		// completes into a receiver or it closes the winner), then retire
		// the array.
		sessionCancel()
		if st.arr != nil {
			st.arr.Close()
		}
	}()
	if method != holepunch.PunchEasySymToEasySym {
		arr, err := holepunch.NewSocketArrayQuiet(1, hello.TID, peerIP)
		if err != nil {
			refuse(fmt.Sprintf("punch socket: %v", err))
			return
		}
		st.arr = arr
		mapped, err := holepunch.MapSocket(ctx, n.stunServers()[0], arr.Sockets()[0])
		if err != nil {
			refuse(fmt.Sprintf("punch socket mapping: %v", err))
			return
		}
		st.mapped = mapped
		arr.StartListeners()
	} else {
		// Both-easy: the per-request socket array is built inside
		// PeerBothEasySym, but the initiator aims at our live base port
		// (hello_ack.PeerPort, then SigBothEasyRes.BaseMappedPort). One
		// fresh-socket mapping here keeps the reply inside the hello
		// budget; querying it lazily would race the 2 s both-easy
		// signaling deadline against the STUN round trip.
		base, err := holepunch.GetUDPPortMapping(ctx, n.stunServers()[0])
		if err != nil {
			refuse(fmt.Sprintf("punch base mapping: %v", err))
			return
		}
		st.mapped = base
	}

	if err := holepunch.WriteSignal(s, holepunch.SignalMsg{
		Kind:     holepunch.SigHelloAck,
		MyNAT:    myNAT,
		PeerPort: st.mapped,
	}); err != nil {
		slog.Debug("punch hello ack write failed", "error", err)
		return
	}
	// ServeSignaling manages its own per-round deadlines.
	_ = s.SetDeadline(time.Time{})

	if err := holepunch.ServeSignaling(s, st.sprayHandler(sessionCtx, hello, peerIP)); err != nil {
		slog.Debug("punch signaling ended", "peer", peerID.ShortString(), "error", err)
		return
	}

	// punch_ready: the QUIC accept (and the remaining hit wait) runs
	// detached — the swarm handler must not block on the network.
	cleanup = false
	go n.acceptPunchedQUIC(ctx, hello, st, peerID, sessionCancel)
}

// punchPeerState is the responder-side session state.
type punchPeerState struct {
	arr       *holepunch.SocketArray
	mapped    int
	ports     []int // birthday sweep vector, created lazily, per session
	hitResult chan *holepunch.PunchedSocket

	bothEasyMu   sync.Mutex
	bothEasyBusy bool
}

// sprayHandler answers spray requests from the initiator. Synchronous
// sprays stay inside the signaling round budget (birthday sweeps cap at
// ~2.4 s < the 4 s round timeout); the both-easy spray runs in its own
// goroutine and replies immediately.
func (st *punchPeerState) sprayHandler(ctx context.Context, hello *holepunch.SignalMsg, peerIP net.IP) holepunch.SignalHandler {
	return func(msg holepunch.SignalMsg) (holepunch.SignalMsg, error) {
		switch msg.Kind {
		case holepunch.SigSpray:
			// The initiator is symmetric; we spray its ports.
			sock := st.arr.Sockets()[0]
			if msg.BasePort != 0 {
				// Predictable window around the initiator's live mapped
				// port, in the direction of its allocation.
				if err := holepunch.SprayEasySymWindow(ctx, sock, msg.TID, peerIP, msg.BasePort, hello.MyNAT.Type.PortDelta()); err != nil {
					return holepunch.SignalMsg{}, err
				}
				return holepunch.SignalMsg{Kind: holepunch.SigSprayRes}, nil
			}
			// Birthday sweep over the shared shuffled vector.
			if st.ports == nil {
				st.ports = holepunch.ShufflePorts()
			}
			next, err := holepunch.SprayHardSymBirthday(ctx, sock, msg.TID, peerIP, st.ports, msg.PortIndex, msg.MaxK2)
			if err != nil {
				return holepunch.SignalMsg{}, err
			}
			return holepunch.SignalMsg{Kind: holepunch.SigSprayRes, NextPortIndex: next}, nil

		case holepunch.SigSprayCone:
			// Cone-to-cone: spray the initiator's mapped port.
			sock := st.arr.Sockets()[0]
			if err := holepunch.SprayConeBatch(ctx, sock, msg.TID, &net.UDPAddr{IP: peerIP, Port: msg.PeerPort}); err != nil {
				return holepunch.SignalMsg{}, err
			}
			return holepunch.SignalMsg{Kind: holepunch.SigSprayConeRes}, nil

		case holepunch.SigBothEasy:
			st.bothEasyMu.Lock()
			if st.bothEasyBusy {
				st.bothEasyMu.Unlock()
				return holepunch.SignalMsg{Kind: holepunch.SigBothEasyRes, IsBusy: true}, nil
			}
			st.bothEasyBusy = true
			st.bothEasyMu.Unlock()
			go func() {
				ps, err := holepunch.PeerBothEasySym(ctx, msg.TID, peerIP, msg.DstPortNum,
					time.Duration(msg.WaitTimeMs)*time.Millisecond)
				if err == nil {
					select {
					case st.hitResult <- ps:
						// Delivered: acceptPunchedQUIC owns the socket.
					case <-ctx.Done():
						// The session died before the accept took the
						// winner; close it so it cannot leak.
						_ = ps.UDPConn.Close()
					}
				}
			}()
			return holepunch.SignalMsg{Kind: holepunch.SigBothEasyRes, BaseMappedPort: st.mapped}, nil
		}
		return holepunch.SignalMsg{}, fmt.Errorf("unexpected punch kind %s", msg.Kind)
	}
}

// acceptPunchedQUIC waits for the TID hit, then turns the winning socket
// into the QUIC listener, authenticates the initiator's token, and queues
// the session's first stream for p2pd_accept. sessionCancel closes the
// punch session (waking the both-easy spray goroutine out of its delivery)
// when the accept ends, whatever the outcome.
func (n *Node) acceptPunchedQUIC(ctx context.Context, hello *holepunch.SignalMsg, st *punchPeerState, peerID peer.ID, sessionCancel context.CancelFunc) {
	acceptCtx, cancel := context.WithTimeout(ctx, punchSessionTimeout)
	defer cancel()
	defer sessionCancel()

	// The goroutine owns the array from here: close it on every path that
	// does not end with the winning socket living under QUIC.
	closeArr := func() {
		if st.arr != nil {
			st.arr.Close()
		}
	}
	var ps *holepunch.PunchedSocket
	if st.arr != nil {
		select {
		case ps = <-st.arr.Hit():
		case <-acceptCtx.Done():
			slog.Info("punch: no TID hit on responder", "peer", peerID.ShortString())
			closeArr()
			return
		}
	} else {
		select {
		case ps = <-st.hitResult:
		case <-acceptCtx.Done():
			// Take the winner if it is being delivered right now; if the
			// spray is still running, the deferred sessionCancel aborts it
			// and its own cleanup closes the array.
			slog.Info("punch: no both-easy hit on responder", "peer", peerID.ShortString())
			select {
			case ps := <-st.hitResult:
				_ = ps.UDPConn.Close()
			default:
			}
			return
		}
	}

	// The hit already triggered the source-reply burst inside the socket
	// array (the asymmetric-success fast path), so nothing extra is needed
	// here. On the both-easy path the array lives inside PeerBothEasySym,
	// which retired its own losers; the winner is ours from here on.
	if st.arr != nil {
		st.arr.RetireOthers(ps)
	}
	ln, err := holepunch.DirectListen(ps.UDPConn)
	if err != nil {
		slog.Warn("punched QUIC listen failed", "peer", peerID.ShortString(), "error", err)
		_ = ps.UDPConn.Close() // not yet owned by QUIC
		return
	}
	quicConn, qs, err := holepunch.AcceptAuthQUIC(acceptCtx, ln, hello.Token)
	if err != nil {
		// The failed session's conn is closed inside AcceptAuthQUIC, but
		// the listener and the socket are caller-owned here.
		_ = ln.Close()
		_ = ps.UDPConn.Close()
		slog.Warn("punched QUIC accept failed", "peer", peerID.ShortString(), "error", err)
		return
	}
	se := n.newQuicStreamEntry(qs, quicConn, ps.UDPConn, peerID)
	slog.Info("punched QUIC session accepted", "peer", peerID.ShortString(), "remote", se.remote)
	select {
	case n.inbound <- se:
	case <-n.ctx.Done():
		_ = quicConn.CloseWithError(0, "node stopping")
	}
}
