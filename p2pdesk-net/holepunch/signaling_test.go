package holepunch

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

// errTestHandler forces the ServeSignaling error-reply path.
var errTestHandler = errors.New("test handler failure")

// pipeAddr is a minimal net.Addr for the fake stream.
type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// fakeStream wraps a net.Pipe with a working deadlineSetter (net.Pipe does
// not implement deadlines), so the client Call path exercises its deadline
// branch.
type fakeStream struct {
	*deadlinePipe
}

func newFakeStreamPair(t *testing.T) (client, server *fakeStream) {
	t.Helper()
	c1, c2 := net.Pipe()
	return &fakeStream{deadlinePipe: &deadlinePipe{Conn: c1}},
		&fakeStream{deadlinePipe: &deadlinePipe{Conn: c2}}
}

func (f *fakeStream) LocalAddr() net.Addr  { return pipeAddr{} }
func (f *fakeStream) RemoteAddr() net.Addr { return pipeAddr{} }

// deadlinePipe forwards deadline calls; the underlying pipe ignores them,
// which is fine: deadlines are advisory here.
type deadlinePipe struct{ net.Conn }

func (d *deadlinePipe) SetReadDeadline(t time.Time) error  { return nil }
func (d *deadlinePipe) SetWriteDeadline(t time.Time) error { return nil }

func TestSignalFrameRoundtrip(t *testing.T) {
	client, server := newFakeStreamPair(t)
	msg := SignalMsg{Kind: SigHello, TID: 7, BasePort: 40000, Token: []byte("tok")}
	go func() {
		if err := WriteSignal(client, msg); err != nil {
			t.Errorf("WriteSignal: %v", err)
		}
	}()
	got, err := ReadSignal(server)
	if err != nil {
		t.Fatalf("ReadSignal: %v", err)
	}
	if got.Kind != SigHello || got.TID != 7 || got.BasePort != 40000 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestSignalFrameTooLarge(t *testing.T) {
	client, _ := newFakeStreamPair(t)
	big := make([]byte, signalingFrameLimit+1)
	msg := SignalMsg{Kind: SigHello, Token: big}
	if err := WriteSignal(client, msg); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestReadSignalRejectsBadLength(t *testing.T) {
	client, server := newFakeStreamPair(t)
	go func() {
		hdr := make([]byte, 4)
		binary.BigEndian.PutUint32(hdr, signalingFrameLimit+1)
		_, _ = client.Write(hdr)
	}()
	if _, err := ReadSignal(server); err == nil {
		t.Fatal("bad frame length accepted")
	}
}

func TestSignalingClientCall(t *testing.T) {
	client, server := newFakeStreamPair(t)
	sc := NewSignalingClient(client)

	// Server side: read the request, echo back a reply.
	go func() {
		req, err := ReadSignal(server)
		if err != nil {
			t.Errorf("server ReadSignal: %v", err)
			return
		}
		if req.Kind != SigSpray || req.BasePort != 40000 {
			t.Errorf("unexpected request: %+v", req)
		}
		reply := SignalMsg{Kind: SigSprayRes, NextPortIndex: 123}
		if err := WriteSignal(server, reply); err != nil {
			t.Errorf("server WriteSignal: %v", err)
		}
	}()

	reply, err := sc.Call(t.Context(), SignalMsg{Kind: SigSpray, BasePort: 40000})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply.Kind != SigSprayRes || reply.NextPortIndex != 123 {
		t.Fatalf("unexpected reply: %+v", reply)
	}
}

func TestServeSignalingDialogue(t *testing.T) {
	client, server := newFakeStreamPair(t)

	handled := make(chan SignalMsg, 2)
	handler := func(msg SignalMsg) (SignalMsg, error) {
		handled <- msg
		if msg.Kind == SigBothEasy {
			return SignalMsg{Kind: SigBothEasyRes, BaseMappedPort: 50000, WaitTimeMs: bothEasySymWaitMs}, nil
		}
		return SignalMsg{Kind: SigSprayRes, NextPortIndex: 99}, nil
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeSignaling(server, handler)
	}()

	sc := NewSignalingClient(client)
	// Round 1: spray request.
	reply, err := sc.Call(t.Context(), SignalMsg{Kind: SigSpray, Round: 1, MaxK2: 700})
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if reply.NextPortIndex != 99 {
		t.Fatalf("round 1 reply: %+v", reply)
	}
	// Round 2: both_easy.
	reply, err = sc.Call(t.Context(), SignalMsg{Kind: SigBothEasy, DstPortNum: 40020})
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if reply.BaseMappedPort != 50000 {
		t.Fatalf("round 2 reply: %+v", reply)
	}
	// Finish: punch_ready ends the serve loop.
	if err := WriteSignal(client, SignalMsg{Kind: SigPunchReady}); err != nil {
		t.Fatalf("punch_ready write: %v", err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("ServeSignaling: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeSignaling did not exit after punch_ready")
	}
	if len(handled) != 2 {
		t.Fatalf("handler called %d times, want 2", len(handled))
	}
}

func TestServeSignalingHandlerError(t *testing.T) {
	client, server := newFakeStreamPair(t)
	handler := func(msg SignalMsg) (SignalMsg, error) {
		return SignalMsg{}, errTestHandler
	}
	go func() { _ = ServeSignaling(server, handler) }()
	sc := NewSignalingClient(client)
	reply, err := sc.Call(t.Context(), SignalMsg{Kind: SigSpray})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply.Kind != SigError {
		t.Fatalf("want SigError reply, got %+v", reply)
	}
}
