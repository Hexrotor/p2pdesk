package holepunch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// Wire format: 4-byte big-endian length prefix + JSON body, capped at
// signalingFrameLimit. The dialogue runs over the already-established
// libp2p stream (relay included), so frames never carry punch traffic.
func WriteSignal(w io.Writer, m SignalMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > signalingFrameLimit {
		return errors.New("signal frame too large")
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(data)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// ReadSignal reads and decodes one frame. io.ReadFull blocks until the whole
// frame arrives; the caller is expected to bound the wait via stream
// deadlines or context cancellation.
func ReadSignal(r io.Reader) (*SignalMsg, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr)
	if n == 0 || n > signalingFrameLimit {
		return nil, errors.New("bad signal frame length")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	var m SignalMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// deadlineSetter is implemented by streams that support deadlines
// (network.Stream and quic.Stream both do; net.Pipe does not).
type deadlineSetter interface {
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// SignalingClient is the initiator side of the dialogue. Every punch round
// is one request/response exchange; Call serializes them because the
// dialogue is strictly sequential.
type SignalingClient struct {
	rw io.ReadWriter
	mu sync.Mutex
}

func NewSignalingClient(rw io.ReadWriter) *SignalingClient {
	return &SignalingClient{rw: rw}
}

// Call sends one message and waits for the peer's reply, bounded by
// signalingTimeout when the stream supports deadlines. It satisfies the
// PunchRPC signature, so it plugs directly into RunPunch.
func (s *SignalingClient) Call(ctx context.Context, req SignalMsg) (SignalMsg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The both-easy request asks the peer to bind and spray 25 sockets
	// before answering; its reply budget is shorter by design so a stalled
	// peer fails fast and the initiator can move on.
	timeout := signalingTimeout
	if req.Kind == SigBothEasy {
		timeout = bothEasySignaling
	}
	if ds, ok := s.rw.(deadlineSetter); ok {
		_ = ds.SetReadDeadline(time.Now().Add(timeout))
		_ = ds.SetWriteDeadline(time.Now().Add(timeout))
	}
	if err := WriteSignal(s.rw, req); err != nil {
		return SignalMsg{}, err
	}
	reply, err := ReadSignal(s.rw)
	if err != nil {
		if ctx.Err() != nil {
			return SignalMsg{}, ctx.Err()
		}
		return SignalMsg{}, err
	}
	return *reply, nil
}

// Close signals the end of the dialogue by resetting any deadline support
// so the stream survives for other uses.
func (s *SignalingClient) Close() {
	if ds, ok := s.rw.(deadlineSetter); ok {
		_ = ds.SetReadDeadline(time.Time{})
		_ = ds.SetWriteDeadline(time.Time{})
	}
}

// SignalHandler processes one peer request and returns its reply.
type SignalHandler func(msg SignalMsg) (SignalMsg, error)

// ServeSignaling runs the peer side of the dialogue: read a message,
// dispatch to the handler, write the reply. It returns when the initiator
// sends punch_ready or the stream dies. The handler runs synchronously, so
// a handler that needs to keep spraying after replying (both_easy) must
// spawn its own goroutine. Each round is bounded by signalingTimeout when
// the stream supports deadlines, so a peer that stalls mid-frame cannot
// hang the serve loop forever.
func ServeSignaling(rw io.ReadWriter, handler SignalHandler) error {
	ds, _ := rw.(deadlineSetter)
	// Clear any deadline on the way out: the stream may be handed to the
	// QUIC session layer after punch_ready, and a leftover deadline there
	// would kill the session with a spurious timeout.
	if ds != nil {
		defer ds.SetReadDeadline(time.Time{})
		defer ds.SetWriteDeadline(time.Time{})
	}
	for {
		if ds != nil {
			_ = ds.SetReadDeadline(time.Now().Add(signalingTimeout))
		}
		msg, err := ReadSignal(rw)
		if err != nil {
			return err
		}
		if msg.Kind == SigPunchReady {
			return nil
		}
		reply, err := handler(*msg)
		if err != nil {
			reply = SignalMsg{Kind: SigError, Error: err.Error()}
		}
		if ds != nil {
			_ = ds.SetWriteDeadline(time.Now().Add(signalingTimeout))
		}
		if err := WriteSignal(rw, reply); err != nil {
			return err
		}
	}
}
