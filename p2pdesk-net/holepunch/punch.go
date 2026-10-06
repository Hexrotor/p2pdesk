package holepunch

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"sync"
	"time"
)

// SocketArray is a set of UDP sockets bound to ephemeral local ports. All
// sockets listen for the punch TID; the first one to observe its own TID
// arriving is the winner and gets handed to the QUIC transport.
type SocketArray struct {
	tid    uint32
	peerIP net.IP
	socks  []*net.UDPConn
	hitCh  chan *PunchedSocket
	done   chan struct{}
	once   sync.Once
	listen sync.Once
	wg     sync.WaitGroup
}

// NewSocketArray binds n sockets and starts a TID listener on each.
func NewSocketArray(n int, tid uint32, peerIP net.IP) (*SocketArray, error) {
	a, err := NewSocketArrayQuiet(n, tid, peerIP)
	if err != nil {
		return nil, err
	}
	a.startListeners()
	return a, nil
}

// NewSocketArrayQuiet binds n sockets without starting TID listeners. A
// quiet array lets the caller probe a socket's live mapping (MapSocket)
// before listening begins; reading a STUN response through a socket whose
// listener is already running would race with the punch receive loop. The
// responder side needs this shape: probe the mapping of the socket that
// will later become the QUIC listener, then StartListeners.
func NewSocketArrayQuiet(n int, tid uint32, peerIP net.IP) (*SocketArray, error) {
	a := &SocketArray{
		tid:    tid,
		peerIP: peerIP,
		hitCh:  make(chan *PunchedSocket, n),
		done:   make(chan struct{}),
	}
	family := socketFamily(peerIP)
	bindIP := net.IP(net.IPv4zero)
	if family == "udp6" {
		bindIP = net.IPv6zero
	}
	for range n {
		c, err := net.ListenUDP(family, &net.UDPAddr{IP: bindIP})
		if err != nil {
			a.Close()
			return nil, err
		}
		a.socks = append(a.socks, c)
	}
	return a, nil
}

// startListeners starts the TID receive loop on every socket. Idempotent
// and race-free (sync.Once): a duplicate listener on the same socket would
// leave a second recvLoop blocked forever on a closed winner socket, and
// RetireOthers/Close wait for every loop to exit.
func (a *SocketArray) startListeners() {
	a.listen.Do(func() {
		for _, c := range a.socks {
			a.wg.Add(1)
			go a.recvLoop(c)
		}
	})
}

// StartListeners starts the TID receive loops on every socket (idempotent).
// Used with NewSocketArrayQuiet: probe mappings first, then listen.
func (a *SocketArray) StartListeners() { a.startListeners() }

// Sockets exposes the bound sockets for probing and spraying before
// StartListeners. Callers must not read from the sockets themselves — the
// receive loops own that direction.
func (a *SocketArray) Sockets() []*net.UDPConn { return a.socks }

// Hit delivers the winning socket. Only one winner is ever reported.
func (a *SocketArray) Hit() <-chan *PunchedSocket { return a.hitCh }

// SendAll sends one cycle to dstPort: socketArrayFanout copies of the punch
// packet from every socket.
func (a *SocketArray) SendAll(dstPort int) {
	dst := &net.UDPAddr{IP: a.peerIP, Port: dstPort}
	for _, c := range a.socks {
		for range socketArrayFanout {
			_ = sendPunchTo(c, a.tid, dst)
		}
	}
}

// RetireOthers closes every socket except the winner, which keeps living
// under QUIC. The array is unusable afterwards.
func (a *SocketArray) RetireOthers(winner *PunchedSocket) {
	a.once.Do(func() {
		close(a.done)
		for _, c := range a.socks {
			if c != winner.UDPConn {
				_ = c.Close()
			}
		}
		a.wg.Wait()
	})
}

// Close closes every socket, winner included.
func (a *SocketArray) Close() {
	a.once.Do(func() {
		close(a.done)
		for _, c := range a.socks {
			_ = c.Close()
		}
		a.wg.Wait()
	})
}

func (a *SocketArray) recvLoop(c *net.UDPConn) {
	defer a.wg.Done()
	buf := make([]byte, 512)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return // socket closed or failed: this socket is out of the race
		}
		// Punch packets only count from the negotiated peer address; an
		// observer on the link can forge the TID, but a forged hit would
		// just waste one QUIC dial attempt, so the cheap source check
		// closes that door entirely.
		if !from.IP.Equal(a.peerIP) {
			continue
		}
		tid, ok := ParsePunchPacket(buf[:n])
		if !ok || tid != a.tid {
			continue
		}
		select {
		case a.hitCh <- &PunchedSocket{UDPConn: c, RemoteAddr: from}:
			// Reply to the source so the sender of the winning packet learns
			// its mapping is open: on the responder side this is the
			// asymmetric-success fast path; on the spraying side the burst is
			// bounded noise for a socket that already won. The burst keeps
			// running past RetireOthers (the winner stays open under QUIC)
			// and dies early when the socket is closed on the losing side.
			go a.confirmHit(c, from)
			return
		case <-a.done:
			return
		}
	}
}

// confirmHit replies to the observed punch source for 1 s @ 50 ms, bounded
// by the frozen constants. It never closes the socket — the winner's socket
// is handed to QUIC while the burst is still in flight.
func (a *SocketArray) confirmHit(c *net.UDPConn, from *net.UDPAddr) {
	for range hitConfirmPackets {
		if err := sendPunchTo(c, a.tid, from); err != nil {
			return
		}
		time.Sleep(hitConfirmInterval)
	}
}

// sendPunchTo sends one punch datagram carrying tid from socket c to dst.
func sendPunchTo(c *net.UDPConn, tid uint32, dst *net.UDPAddr) error {
	body, err := randomBody()
	if err != nil {
		return err
	}
	pkt := MakePunchPacket(tid, body)
	_, err = c.WriteToUDP(pkt[:], dst)
	return err
}

// socketFamily picks the UDP network family matching the peer address.
func socketFamily(peer net.IP) string {
	if peer.To4() != nil {
		return "udp4"
	}
	return "udp6"
}

// SprayConeBatch sends the cone-side spray: 2 packets per batch, 5 batches,
// 400 ms apart (10 packets total), to the peer's mapped address.
func SprayConeBatch(ctx context.Context, c *net.UDPConn, tid uint32, peer *net.UDPAddr) error {
	for range 5 {
		for range 2 {
			if err := sendPunchTo(c, tid, peer); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return nil
}

// SprayEasySymWindow sprays the predictable 51-port window around basePort:
// [base+1 .. base+50] for an incrementing sym peer, [base-50 .. base-1] for
// a decrementing one, two passes, 3 packets per port, 1 ms between ports.
func SprayEasySymWindow(ctx context.Context, c *net.UDPConn, tid uint32, peerIP net.IP, basePort, dir int) error {
	if dir >= 0 {
		dir = 1
	} else {
		dir = -1
	}
	for range 2 {
		for i := 1; i <= easySymSprayMaxPorts; i++ {
			port := basePort + dir*i
			if port < 1 || port > 65535 {
				continue
			}
			dst := &net.UDPAddr{IP: peerIP, Port: port}
			for range sweepPacketsPerPort {
				if err := sendPunchTo(c, tid, dst); err != nil {
					return err
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sweepPortInterval):
			}
		}
	}
	return nil
}

// SprayHardSymBirthday walks maxK2 ports of the shared shuffled vector
// starting at portIndex (wrapping around), 3 packets per port, 1 ms between
// ports. Returns the next port index so later rounds continue without
// re-hitting dead ports. Negative indices and counts clamp to zero: they
// arrive from the peer via signaling and must never panic. A maxK2 larger
// than the vector would wrap back onto already-sprayed ports within one
// round, so it caps at the vector length.
func SprayHardSymBirthday(ctx context.Context, c *net.UDPConn, tid uint32, peerIP net.IP, ports []int, portIndex, maxK2 int) (int, error) {
	if len(ports) == 0 {
		return portIndex, errors.New("empty port vector")
	}
	if maxK2 < 0 {
		maxK2 = 0
	}
	if maxK2 > len(ports) {
		maxK2 = len(ports)
	}
	idx := ((portIndex % len(ports)) + len(ports)) % len(ports)
	for range maxK2 {
		dst := &net.UDPAddr{IP: peerIP, Port: ports[idx]}
		for range sweepPacketsPerPort {
			if err := sendPunchTo(c, tid, dst); err != nil {
				return idx, err
			}
		}
		select {
		case <-ctx.Done():
			return idx, ctx.Err()
		case <-time.After(sweepPortInterval):
		}
		idx = (idx + 1) % len(ports)
	}
	return idx, nil
}

// PunchOptions configures an initiator-side punch.
type PunchOptions struct {
	Method PunchMethod
	PeerIP net.IP // peer's observed public IP, reported by the peer over signaling.
	// WARNING: never substitute the locally observed remote address — over a
	// relay connection that is the relay's address, and spraying the relay
	// with punch packets is both useless and abusive.
	PeerPort int // peer QUIC listener's mapped port (updated per easy-sym round)
	MyNAT    NATType
	PeerNAT  NATType
	TID      uint32
	StunAddr string // live port-mapping queries (cone punch, easy-sym rounds)
}

// RunPunch orchestrates the initiator side of a punch session. rpc sends
// one signaling message over the established libp2p stream and blocks for
// the reply. The returned socket already observed the TID arriving, so the
// NAT mapping is open in both directions; hand it to QUIC directly.
func RunPunch(ctx context.Context, opts PunchOptions, rpc PunchRPC) (*PunchedSocket, error) {
	switch opts.Method {
	case PunchConeToCone:
		return punchConeToCone(ctx, opts, rpc)
	case PunchSymToCone:
		return punchSymToCone(ctx, opts, rpc)
	case PunchEasySymToEasySym:
		return punchBothEasySym(ctx, opts, rpc)
	}
	return nil, fmt.Errorf("punch method %s not supported", opts.Method)
}

// punchConeToCone: one socket, one initial packet, ask the peer to spray my
// live mapped port (10 packets), keep sending every 200 ms until the RPC
// finishes plus a 1000 ms grace. The mapped port is probed on the punch
// socket itself: the NAT-detection mapping belongs to a different socket
// and would point the peer at a dead port.
func punchConeToCone(ctx context.Context, opts PunchOptions, rpc PunchRPC) (*PunchedSocket, error) {
	arr, err := NewSocketArrayQuiet(1, opts.TID, opts.PeerIP)
	if err != nil {
		return nil, err
	}
	defer arr.Close()
	// Probe before the listener starts: both would otherwise read the same
	// socket and race over the STUN response.
	myMapped, err := MapSocket(ctx, opts.StunAddr, arr.socks[0])
	if err != nil {
		return nil, fmt.Errorf("cone punch port mapping: %w", err)
	}
	arr.startListeners()

	arr.SendAll(opts.PeerPort)

	rpcDone := make(chan error, 1)
	go func() {
		reply, err := rpc(ctx, SignalMsg{
			Kind:     SigSprayCone,
			TID:      opts.TID,
			Method:   PunchConeToCone,
			PeerPort: myMapped,
		})
		if err != nil {
			rpcDone <- err
			return
		}
		if reply.Kind == SigError {
			rpcDone <- fmt.Errorf("peer spray error: %s", reply.Error)
			return
		}
		rpcDone <- nil
	}()

	ticker := time.NewTicker(coneToConeSendInterval)
	defer ticker.Stop()
	grace := 1000 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ps := <-arr.Hit():
			arr.RetireOthers(ps)
			return ps, nil
		case <-ticker.C:
			arr.SendAll(opts.PeerPort)
		case err := <-rpcDone:
			if err != nil {
				return nil, fmt.Errorf("cone spray rpc: %w", err)
			}
			// RPC finished: keep sending for the grace window, then fail.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case ps := <-arr.Hit():
				arr.RetireOthers(ps)
				return ps, nil
			case <-time.After(grace):
				return nil, errors.New("cone-to-cone punch: no TID received")
			}
		}
	}
}

// punchSymToCone: 84-socket array spraying the cone peer's stable listener
// port, while the cone peer sprays my ports per round. Easy-sym rounds use
// the predictable window around my live mapped port; hard-sym rounds walk
// the shuffled birthday vector. Rounds pace on the frozen backoff.
func punchSymToCone(ctx context.Context, opts PunchOptions, rpc PunchRPC) (*PunchedSocket, error) {
	arr, err := NewSocketArray(hardSymSocketCount, opts.TID, opts.PeerIP)
	if err != nil {
		return nil, err
	}
	defer arr.Close()

	portIndex := 0
	maxK2 := mrand.IntN(birthdayPacketsMax-birthdayPacketsMin+1) + birthdayPacketsMin

	for round := 0; ; round++ {
		// Ask the cone peer to spray my ports. The base port for an
		// easy-sym round is my live mapping, queried right before the
		// request so the peer aims at the freshest allocation.
		rpcDone := make(chan SignalMsg, 1)
		rpcErr := make(chan error, 1)
		go func() {
			var req SignalMsg
			if opts.MyNAT.IsEasySymmetric() {
				live, err := GetUDPPortMapping(ctx, opts.StunAddr)
				if err != nil {
					rpcErr <- fmt.Errorf("live port mapping: %w", err)
					return
				}
				req = SignalMsg{
					Kind:     SigSpray,
					TID:      opts.TID,
					Method:   PunchSymToCone,
					BasePort: live,
				}
			} else {
				req = SignalMsg{
					Kind:      SigSpray,
					TID:       opts.TID,
					Method:    PunchSymToCone,
					Round:     round,
					PortIndex: portIndex,
					MaxK2:     maxK2,
				}
			}
			reply, err := rpc(ctx, req)
			if err != nil {
				rpcErr <- err
				return
			}
			rpcDone <- reply
		}()

		// Initial blast, then the result loop: send from all sockets every
		// 200 ms for 1000 ms past the RPC completion.
		arr.SendAll(opts.PeerPort)
		ticker := time.NewTicker(symToConeSendInterval)
		rpcOK := false
		var reply SignalMsg
		rpcFinished := false
		var finishAt time.Time
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return nil, ctx.Err()
			case ps := <-arr.Hit():
				ticker.Stop()
				arr.RetireOthers(ps)
				return ps, nil
			case <-ticker.C:
				arr.SendAll(opts.PeerPort)
			case r := <-rpcDone:
				reply, rpcOK = r, true
				rpcFinished = true
				finishAt = time.Now().Add(1000 * time.Millisecond)
			case <-rpcErr:
				rpcFinished = true
				finishAt = time.Now().Add(1000 * time.Millisecond)
			}
			if rpcFinished && time.Now().After(finishAt) {
				ticker.Stop()
				break
			}
		}

		if rpcOK {
			if reply.Kind == SigError {
				return nil, fmt.Errorf("peer spray error: %s", reply.Error)
			}
			portIndex = reply.NextPortIndex
			if round > 2 {
				maxK2 = max(maxK2*2/round, birthdayRoundShrinkMin)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoffDelay(round)):
		}
	}
}

// punchBothEasySym: true bidirectional port prediction. Both sides are
// easy-symmetric; each tells the other its live mapped port and aims at the
// peer's predicted next allocation.
func punchBothEasySym(ctx context.Context, opts PunchOptions, rpc PunchRPC) (*PunchedSocket, error) {
	arr, err := NewSocketArray(bothEasySymSocketCount, opts.TID, opts.PeerIP)
	if err != nil {
		return nil, err
	}
	defer arr.Close()

	// My live mapped port, queried at punch time; the peer will aim at
	// my predicted next allocation (my_port ± offset by my direction).
	live, err := GetUDPPortMapping(ctx, opts.StunAddr)
	if err != nil {
		return nil, fmt.Errorf("live port mapping: %w", err)
	}
	dst := live + opts.MyNAT.PortDelta()*easySymPortOffset
	if dst < 1 || dst > 65535 {
		return nil, fmt.Errorf("my predicted port %d out of range", dst)
	}

	reply, err := rpc(ctx, SignalMsg{
		Kind:           SigBothEasy,
		TID:            opts.TID,
		Method:         PunchEasySymToEasySym,
		DstPortNum:     dst,
		UDPSocketCount: bothEasySymSocketCount,
		WaitTimeMs:     bothEasySymWaitMs,
	})
	if err != nil {
		return nil, fmt.Errorf("both-easy rpc: %w", err)
	}
	if reply.Kind == SigError {
		return nil, fmt.Errorf("peer both-easy error: %s", reply.Error)
	}
	if reply.IsBusy {
		return nil, errors.New("peer already serving a both-easy punch")
	}

	// The peer's current mapped port, adjusted by its direction to the
	// predicted next allocation. The base arrives from the peer via
	// signaling, so clamp it: an out-of-range port would make every
	// SendAll silently fail instead of erroring.
	peerPort := reply.BaseMappedPort + opts.PeerNAT.PortDelta()*easySymPortOffset
	if peerPort < 1 || peerPort > 65535 {
		return nil, fmt.Errorf("peer predicted port %d out of range", peerPort)
	}

	// Send from all 25 sockets every 100 ms for the peer's wait window
	// plus a 1000 ms grace.
	window := time.Duration(reply.WaitTimeMs) * time.Millisecond
	if window <= 0 {
		window = bothEasySymWaitMs * time.Millisecond
	}
	if window > bothEasySymWaitCapMs*time.Millisecond {
		window = bothEasySymWaitCapMs * time.Millisecond
	}
	deadline := time.Now().Add(window + 1000*time.Millisecond)
	ticker := time.NewTicker(bothEasySymSendInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ps := <-arr.Hit():
			arr.RetireOthers(ps)
			return ps, nil
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, errors.New("both-easy punch: no TID received")
			}
			arr.SendAll(peerPort)
		}
	}
}

// PeerBothEasySym runs the easy-symmetric peer side of a both-easy punch:
// its own 25-socket array spraying the initiator's predicted port every
// 100 ms for min(wait, cap), returning the socket that observed the TID.
func PeerBothEasySym(ctx context.Context, tid uint32, peerIP net.IP, peerPort int, wait time.Duration) (*PunchedSocket, error) {
	if peerPort < 1 || peerPort > 65535 {
		return nil, fmt.Errorf("peer port %d out of range", peerPort)
	}
	arr, err := NewSocketArray(bothEasySymSocketCount, tid, peerIP)
	if err != nil {
		return nil, err
	}
	defer arr.Close()

	if wait > bothEasySymWaitCapMs*time.Millisecond {
		wait = bothEasySymWaitCapMs * time.Millisecond
	}
	deadline := time.Now().Add(wait)
	ticker := time.NewTicker(bothEasySymSendInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ps := <-arr.Hit():
			arr.RetireOthers(ps)
			return ps, nil
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, errors.New("both-easy punch: no TID received")
			}
			arr.SendAll(peerPort)
		}
	}
}
