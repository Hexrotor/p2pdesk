package main

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// blockingStream is a streamIO whose Read blocks until Close, for
// exercising the reader-exit path deterministically (no transport).
type blockingStream struct {
	closed chan struct{}
	once   sync.Once
}

func (b *blockingStream) Read(p []byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingStream) Write(p []byte) (int, error) { return len(p), nil }

func (b *blockingStream) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// resetStream is a streamIO whose Read blocks on a gate and then returns a
// transport error — the peer-reset shape, without any transport.
type resetStream struct {
	gate   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (r *resetStream) Read(p []byte) (int, error) {
	<-r.gate
	return 0, io.ErrClosedPipe
}

func (r *resetStream) Write(p []byte) (int, error) { return len(p), nil }

func (r *resetStream) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// setupPair returns two connected nodes with an open control stream.
func setupPair(t *testing.T) (*Node, *Node, *StreamEntry, *StreamEntry) {
	t.Helper()
	a, err := simpleNode(t.TempDir()+"/a", true)
	if err != nil {
		t.Fatalf("node A: %v", err)
	}
	t.Cleanup(a.stop)
	b, err := simpleNode(t.TempDir()+"/b", false)
	if err != nil {
		t.Fatalf("node B: %v", err)
	}
	t.Cleanup(b.stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var loopback multiaddr.Multiaddr
	for _, m := range a.host.Addrs() {
		if s := m.String(); len(s) >= 20 && s[:9] == "/ip4/127." {
			loopback = m
			break
		}
	}
	if loopback == nil {
		t.Fatalf("no loopback addr on A: %v", a.host.Addrs())
	}
	if err := b.host.Connect(ctx, peer.AddrInfo{ID: a.host.ID(), Addrs: []multiaddr.Multiaddr{loopback}}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	s, err := b.host.NewStream(ctx, a.host.ID(), controlProtocol)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	if _, err := s.Write([]byte("kick")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	seB := b.newStreamEntry(s, a.host.ID())
	seA, code := a.accept(5000)
	if code != errOK {
		t.Fatalf("accept: code %d", code)
	}
	// consume the kick
	got := make([]byte, 16)
	if n := a.streamRead(seA.id, got, 5000); n != 4 {
		t.Fatalf("kick read: n=%d", n)
	}
	return a, b, seA, seB
}

// TestReadTimeout: a read with timeoutMs > 0 returns errTimeout while the
// peer is silent, and the stream remains usable afterwards.
func TestReadTimeout(t *testing.T) {
	a, b, seA, seB := setupPair(t)

	start := time.Now()
	got := make([]byte, 16)
	if code := a.streamRead(seA.id, got, 300); code != errTimeout {
		t.Fatalf("read on silent peer: want errTimeout, got %d", code)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout took too long")
	}

	// Stream still usable: B writes, A reads.
	if n := b.streamWrite(seB.id, []byte("after"), 5000); n != 5 {
		t.Fatalf("B write: %d", n)
	}
	n := a.streamRead(seA.id, got, 5000)
	if n != 5 {
		t.Fatalf("read after timeout: n=%d", n)
	}
	if string(got[:n]) != "after" {
		t.Fatalf("read after timeout: q=%q", got[:n])
	}
}

// TestCloseInterruptsBlockingRead: a blocking read (timeout 0) is woken by
// the peer closing the stream — returns an error, not a hang.
func TestCloseInterruptsBlockingRead(t *testing.T) {
	a, b, seA, seB := setupPair(t)

	done := make(chan int64, 1)
	go func() {
		got := make([]byte, 16)
		done <- a.streamRead(seA.id, got, 0) // blocks until close
	}()

	time.Sleep(200 * time.Millisecond) // let the read block
	b.closeStream(seB.id)

	select {
	case code := <-done:
		// EOF (0) or a negative error both mean the stream died.
		if code > 0 {
			t.Fatalf("read after close: want EOF/error, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("blocking read was not woken by close")
	}
}

// TestConcurrentCloseAndWrite: closing while a write is in flight must not
// panic or deadlock. The write may have been accepted by the transport
// (yamux buffers before sending) or fail with an error — either is fine;
// a hang or panic is not.
func TestConcurrentCloseAndWrite(t *testing.T) {
	a, b, seA, seB := setupPair(t)

	done := make(chan int64, 1)
	go func() {
		payload := make([]byte, 1024)
		done <- b.streamWrite(seB.id, payload, 0) // blocks (A never reads)
	}()

	time.Sleep(200 * time.Millisecond)
	b.closeStream(seB.id)

	select {
	case <-done:
		// returned (success or error) — good
	case <-time.After(5 * time.Second):
		t.Fatalf("write was not woken by close")
	}
	_ = a
	_ = seA
}

// TestWriteEnqueueCloseRace: hammer the enqueue/close race — writers race
// with closeStream; every call must return (success or error) and nothing
// may hang.
func TestWriteEnqueueCloseRace(t *testing.T) {
	a, b, seA, seB := setupPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		payload := make([]byte, 64)
		for range 2000 {
			b.streamWrite(seB.id, payload, 100) // bounded wait each
		}
	}()
	time.Sleep(10 * time.Millisecond)
	b.closeStream(seB.id)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("write loop hung after close")
	}
	_ = a
	_ = seA
}

// TestCloseWithFullReadCh: a reader blocked sending into a full readCh must
// abort on close (sendRead's closedCh escape) and exit — the exact path the
// fix guards. The channel is filled directly instead of through yamux:
// frame merging makes the network-arrival count nondeterministic, which
// made the old version flaky.
func TestCloseWithFullReadCh(t *testing.T) {
	n := bareNode()
	se := n.addEntry(&blockingStream{closed: make(chan struct{})}, peer.ID(""), "", pathDirectTCP, nil)

	// Fill readCh (cap 4) while the resident reader is blocked in Read.
	for i := range 4 {
		se.readCh <- readResult{n: 1, data: []byte{byte(i)}}
	}

	// Close: the fake stream's Read unblocks with EOF, the reader's sendRead
	// finds a full channel and a closed closedCh, aborts, and the loop exits
	// closing readCh. A subsequent read fails fast at the entry check.
	n.closeStream(se.id)
	got := make([]byte, 16)
	if code := n.streamRead(se.id, got, 1000); code != errClosed {
		t.Fatalf("read after local close: want errClosed, got %d", code)
	}
	consumed := 0
	for {
		select {
		case r, ok := <-se.readCh:
			if !ok {
				// channel closed = reader exited, not leaked
				if consumed != 4 {
					t.Fatalf("queued data incomplete: consumed %d, want 4", consumed)
				}
				return
			}
			consumed += r.n
		case <-time.After(2 * time.Second):
			t.Fatalf("readCh never closed — reader goroutine leaked (consumed %d)", consumed)
		}
	}
}

// TestReaderDiedTearsDownQuicEntry: a transport error (peer reset) on a
// punched-QUIC entry must tear the whole session down even when the
// application is not reading — the teardown has to run before the error
// is queued, or a full readCh would block the exit path forever and leak
// the conn, the socket, and the writer (the exact stall scenario the
// ordering in readLoop guards).
func TestReaderDiedTearsDownQuicEntry(t *testing.T) {
	n := bareNode()
	teardown := make(chan struct{}, 1)
	st := &resetStream{gate: make(chan struct{}), closed: make(chan struct{})}
	se := n.addEntry(st, peer.ID(""), "", pathCustomQUIC, func() {
		teardown <- struct{}{}
	})

	// Fill readCh (cap 4) while the reader is parked on the gate.
	for i := range 4 {
		se.readCh <- readResult{n: 1, data: []byte{byte(i)}}
	}
	close(st.gate) // the reader's Read now returns the reset error

	// onClose runs exactly once and the reader exits, readCh closed.
	select {
	case <-teardown:
	case <-time.After(2 * time.Second):
		t.Fatal("readerDied never ran: conn teardown missing")
	}
	consumed := 0
	for {
		select {
		case r, ok := <-se.readCh:
			if !ok {
				if consumed != 4 {
					t.Fatalf("queued data incomplete: consumed %d, want 4", consumed)
				}
				return
			}
			consumed += r.n
		case <-time.After(2 * time.Second):
			t.Fatalf("readCh never closed — reader goroutine leaked (consumed %d)", consumed)
		}
	}
}
