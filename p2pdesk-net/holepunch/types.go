// Package holepunch implements UDP NAT hole punching between two peers.
//
// The algorithm and its parameters follow the design that EasyTier has
// production-verified for years: STUN-based NAT classification, birthday
// sweeps for hard-symmetric NATs, port prediction for predictable ones,
// and a post-punch QUIC transport authenticated by a session token. This
// is an independent implementation; the parameters are frozen in the
// constants below with the rationale for each value.
package holepunch

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	mrand "math/rand/v2"
	"net"
	"time"
)

// NATType classifies the local NAT behavior as seen by the STUN detection.
// Cone subtypes (full / restricted / port-restricted) collapse into NATCone
// for the punch matrix; the finer grade is kept in NATInfo.Subtype for
// diagnostics.
type NATType uint8

const (
	NATUnknown NATType = iota
	NATOpen            // open internet (or no NAT at all): direct dial works
	NATCone            // endpoint-independent mapping
	NATSymmetricEasyInc
	NATSymmetricEasyDec
	NATSymmetricHard
)

func (t NATType) String() string {
	switch t {
	case NATOpen:
		return "open"
	case NATCone:
		return "cone"
	case NATSymmetricEasyInc:
		return "symmetric-easy-inc"
	case NATSymmetricEasyDec:
		return "symmetric-easy-dec"
	case NATSymmetricHard:
		return "symmetric-hard"
	}
	return "unknown"
}

// IsCone reports endpoint-independent mapping.
func (t NATType) IsCone() bool { return t == NATCone }

// IsSymmetric reports endpoint-dependent mapping.
func (t NATType) IsSymmetric() bool {
	return t == NATSymmetricEasyInc || t == NATSymmetricEasyDec || t == NATSymmetricHard
}

// IsEasySymmetric reports a symmetric NAT whose port allocation direction is
// predictable (increment or decrement), which enables port prediction.
func (t NATType) IsEasySymmetric() bool {
	return t == NATSymmetricEasyInc || t == NATSymmetricEasyDec
}

// PortDelta is the port increment direction of an easy-symmetric NAT.
func (t NATType) PortDelta() int {
	if t == NATSymmetricEasyInc {
		return 1
	}
	if t == NATSymmetricEasyDec {
		return -1
	}
	return 0
}

// Cone subtypes (diagnostic only; the punch matrix treats them all as cone).
const (
	ConeFullCone       = "full-cone"
	ConeRestricted     = "restricted"
	ConePortRestricted = "port-restricted"
	ConeNoPat          = "no-pat"
)

// NATInfo is the local NAT description exchanged over signaling.
type NATInfo struct {
	Type       NATType `json:"type"`
	Subtype    string  `json:"subtype,omitempty"`
	PublicIP   string  `json:"public_ip"`
	PublicPort int     `json:"public_port"`
}

// PunchMethod is the strategy selected for a punch attempt.
type PunchMethod uint8

const (
	PunchNone PunchMethod = iota
	PunchConeToCone
	PunchSymToCone
	PunchEasySymToEasySym
)

func (m PunchMethod) String() string {
	switch m {
	case PunchConeToCone:
		return "cone-to-cone"
	case PunchSymToCone:
		return "sym-to-cone"
	case PunchEasySymToEasySym:
		return "easy-sym-to-easy-sym"
	}
	return "none"
}

// DeterminePunchMethod selects the strategy for the two peers' NAT types.
// Cone and unknown sides pair as cone-to-cone (an unknown is graded as
// cone, mirroring the detection's optimistic default); any symmetric side
// against a cone runs the birthday sweep; two predictable symmetric sides
// run bidirectional port prediction. An open side expects direct
// connectivity, so any Open combination punches nothing; symmetric pairs
// with a hard side are left to the relay-only path.
func DeterminePunchMethod(myNAT, peerNAT NATType) PunchMethod {
	if myNAT == NATOpen || peerNAT == NATOpen {
		return PunchNone
	}
	cone := func(t NATType) bool { return t == NATCone || t == NATUnknown }
	if cone(myNAT) && cone(peerNAT) {
		return PunchConeToCone
	}
	if myNAT.IsEasySymmetric() && peerNAT.IsEasySymmetric() {
		return PunchEasySymToEasySym
	}
	sym := func(t NATType) bool { return t.IsSymmetric() || t == NATUnknown }
	if sym(myNAT) && cone(peerNAT) || cone(myNAT) && sym(peerNAT) {
		return PunchSymToCone
	}
	return PunchNone
}

// Punch packet format: 8-byte UDPTunnelHeader + 16 random bytes = 24 bytes.
// The header is shared with a post-punch session layer in the reference
// design; only the hole-punch variant (msg_type 5) is produced here.
const (
	punchPacketSize = 24
	punchMsgType    = 5
	punchBodyLen    = 16
)

// MakePunchPacket builds a 24-byte punch datagram carrying the transaction
// id. The body is random so the packet carries no recognizable fingerprint.
func MakePunchPacket(tid uint32, body [punchBodyLen]byte) [punchPacketSize]byte {
	var p [punchPacketSize]byte
	binary.LittleEndian.PutUint32(p[0:4], tid)
	p[4] = punchMsgType
	p[5] = 0 // padding
	binary.LittleEndian.PutUint16(p[6:8], punchBodyLen)
	copy(p[8:], body[:])
	return p
}

// ParsePunchPacket extracts the transaction id from a punch datagram.
// The exact-length + field check is the anti-spoof filter: lenient parsing
// would admit random noise as punches and destabilize the caller.
func ParsePunchPacket(data []byte) (tid uint32, ok bool) {
	if len(data) != punchPacketSize {
		return 0, false
	}
	if data[4] != punchMsgType {
		return 0, false
	}
	if binary.LittleEndian.Uint16(data[6:8]) != punchBodyLen {
		return 0, false
	}
	return binary.LittleEndian.Uint32(data[0:4]), true
}

// Frozen parameters. The values are the production-verified defaults of
// the design this package follows; each one carries the rationale for
// deviating from them being a deliberate decision, not a typo.
const (
	// Socket arrays.
	hardSymSocketCount     = 84 // birthday attack: sockets on the sym side
	bothEasySymSocketCount = 25 // port prediction: sockets per side

	// Port prediction.
	easySymPortOffset = 20 // predicted next-port delta, per peer direction
	easySymDiffMax    = 100

	// SymToCone spray, predictable window (sym side is easy-symmetric).
	easySymSprayMaxPorts = 50 // ports around the live base port, 2 passes

	// SymToCone spray, birthday sweep (sym side is hard-symmetric).
	birthdayPacketsMin     = 600
	birthdayPacketsMax     = 800
	birthdayRoundShrinkMin = 180 // max_k2 floor for round > 2

	// Sending fanout and cadence.
	socketArrayFanout       = 3 // copies per socket per send cycle
	sweepPacketsPerPort     = 3 // copies per (port, ip) during a sweep
	sweepPortInterval       = time.Millisecond
	symToConeSendInterval   = 200 * time.Millisecond
	coneToConeSendInterval  = 200 * time.Millisecond
	bothEasySymSendInterval = 100 * time.Millisecond
	bothEasySymWaitMs       = 5000
	bothEasySymWaitCapMs    = 8000
	// Hit confirmation: a socket that observes the TID replies to the
	// observed source for 1 s @ 50 ms, so the sender of the winning packet
	// learns its mapping is open without waiting for the spray to guess its
	// port (the asymmetric-success fast path on the responder side).
	hitConfirmPackets  = 20
	hitConfirmInterval = 50 * time.Millisecond

	// STUN detection.
	stunTimeout            = 3 * time.Second
	stunRequestRepeat      = 2  // probes per server, with one retry
	hardSymSpreadThreshold = 15 // mapped-port spread that grades hard-sym
	// ReDetectInterval is how long a NAT classification stays valid before
	// the node integration layer triggers a fresh detection.
	ReDetectInterval = 600 * time.Second
	// ReDetectUnknownInterval is the retry period after an unknown
	// classification (the node retries eagerly instead of waiting the
	// full interval).
	ReDetectUnknownInterval = 10 * time.Second

	// Signaling.
	signalingTimeout    = 4 * time.Second // per round-trip, relay-tolerant
	bothEasySignaling   = 2 * time.Second
	signalingFrameLimit = 64 * 1024
	// Per-peer cooldown after a failed punch, applied by the node
	// integration layer; a fresh probe is wasted minutes after a miss.
	blacklistTimeout = 3600 * time.Second
)

// RetryBackoff is the spray-round pacing on the sym side: short early
// retries, growing to a 64 s plateau. The punch is unbounded in rounds;
// the caller's context bounds the total window.
var RetryBackoff = []time.Duration{
	time.Second, time.Second, 2 * time.Second, 4 * time.Second,
	4 * time.Second, 8 * time.Second, 8 * time.Second, 16 * time.Second,
	64 * time.Second,
}

// backoffDelay returns the retry delay for round n (0-based), plateauing at
// the last entry.
func backoffDelay(round int) time.Duration {
	if round < len(RetryBackoff) {
		return RetryBackoff[round]
	}
	return RetryBackoff[len(RetryBackoff)-1]
}

// PunchedSocket is a socket that observed its own TID arriving: the punch
// succeeded and this datagram socket now reaches the peer. The socket is
// handed to the QUIC transport directly; connmgr cannot see it.
type PunchedSocket struct {
	UDPConn    *net.UDPConn
	RemoteAddr net.Addr
}

// SignalKind enumerates the punch signaling dialogue states. The dialogue
// runs over the already-established libp2p connection (relay included), so
// these messages never carry punch traffic themselves.
type SignalKind string

const (
	SigHello        SignalKind = "hello"
	SigHelloAck     SignalKind = "hello_ack"
	SigSpray        SignalKind = "spray"      // sym side asks the cone to spray
	SigSprayRes     SignalKind = "spray_res"  // cone side reports continuation state
	SigSprayCone    SignalKind = "spray_cone" // cone side asks the peer to spray its mapped port
	SigSprayConeRes SignalKind = "spray_cone_res"
	SigBothEasy     SignalKind = "both_easy"
	SigBothEasyRes  SignalKind = "both_easy_res"
	SigPunchReady   SignalKind = "punch_ready"
	SigError        SignalKind = "error"
)

// SignalMsg is one signaling message. Requests carry the fields the peer
// needs to spray; replies carry continuation state. JSON-encoded over a
// 4-byte big-endian length-prefixed frame.
type SignalMsg struct {
	Kind  SignalKind `json:"kind"`
	Error string     `json:"error,omitempty"`

	// hello exchange
	MyNAT    NATInfo     `json:"nat,omitzero"`
	Method   PunchMethod `json:"method,omitempty"`
	TID      uint32      `json:"tid,omitempty"`
	Token    []byte      `json:"token,omitempty"`     // QUIC session token, 16 bytes
	PeerPort int         `json:"peer_port,omitempty"` // my QUIC listener's mapped port

	// spray (sym side asking the cone side to spray):
	// predictable window: BasePort carries the sym side's live mapped port
	BasePort int `json:"base_port,omitempty"`
	// birthday sweep: cone walks the shared shuffled port vector
	Round     int `json:"round,omitempty"`
	PortIndex int `json:"port_index,omitempty"`
	MaxK2     int `json:"max_k2,omitempty"`
	// spray_res
	NextPortIndex int `json:"next_port_index,omitempty"`

	// both_easy
	DstPortNum     int  `json:"dst_port_num,omitempty"`
	UDPSocketCount int  `json:"udp_socket_count,omitempty"`
	WaitTimeMs     int  `json:"wait_time_ms,omitempty"`
	IsBusy         bool `json:"is_busy,omitempty"`
	BaseMappedPort int  `json:"base_mapped_port,omitempty"`
}

// PunchRPC sends one signaling message to the peer and blocks for its
// reply. Implemented by the signaling layer over the libp2p stream.
type PunchRPC func(ctx context.Context, req SignalMsg) (SignalMsg, error)

// randomBody fills a 16-byte punch body from crypto randomness.
func randomBody() ([punchBodyLen]byte, error) {
	var b [punchBodyLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return b, err
	}
	return b, nil
}

// ShufflePorts returns a Fisher-Yates shuffled 1..65535 port vector, the
// walking order for the birthday sweep. The vector is shared across rounds
// via the port-index continuation, so consecutive rounds never re-hit the
// same dead ports until a full cycle completes. The responder side keeps
// one vector per punch session.
func ShufflePorts() []int {
	ports := make([]int, 65535)
	for i := range ports {
		ports[i] = i + 1
	}
	mrand.Shuffle(len(ports), func(i, j int) {
		ports[i], ports[j] = ports[j], ports[i]
	})
	return ports
}
