package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/quic-go/quic-go"
)

// errStreamClosed is answered to requests drained by a closing writer.
var errStreamClosed = errors.New("stream closed")

// Path kinds (mirror libs/hbb_common/src/p2p.rs PATH_* constants).
const (
	pathDirectQUIC = 0
	pathDirectTCP  = 1
	pathDCUtR      = 2
	pathCustomQUIC = 3
	pathRelay      = 4
)

// streamIO is the transport-agnostic stream handle. Both libp2p's
// network.Stream and quic-go's quic.Stream satisfy it, so a punched QUIC
// session's streams share the handle table, the reader/writer workers,
// and the close semantics with libp2p streams.
type streamIO interface {
	io.Reader
	io.Writer
	Close() error
}

// StreamEntry is one open control stream plus its metadata. Handles are
// unique monotonic ids; closing removes the entry and interrupts any
// in-flight blocking read/write (stream.Close() unblocks them).
type StreamEntry struct {
	id       uint64
	s        streamIO
	peer     peer.ID
	remote   string
	pathKind uint32
	// onClose releases resources connmgr cannot see: the punched QUIC
	// connection and its UDP socket (closed explicitly — quic-go never
	// closes a caller-provided packet conn). It runs exactly once,
	// whichever path first observes the stream death — closeStream, a
	// worker panic, or a reader transport error (closed.Swap/CAS
	// arbitrates).
	onClose func()
	// releaseProtection removes the connmgr protection acquired for this
	// control stream. It is installed after the handle gets its id and is
	// called exactly once by closeStream/reader failure paths.
	releaseProtection func()

	// One resident reader goroutine and one resident writer goroutine per
	// stream (mirroring the Rust side's worker threads). A blocking
	// read/write never leaks a goroutine: a timeout only abandons waiting
	// for the result; the worker keeps running and the stream stays usable.
	readCh   chan readResult
	writeCh  chan writeReq
	closed   atomic.Bool
	closedCh chan struct{} // closed once; wakes the workers
}

type readResult struct {
	n    int
	err  error
	data []byte // non-nil when n > 0
}

type writeReq struct {
	data []byte
	res  chan writeResult
}

type writeResult struct {
	n   int
	err error
}

// Path kind of a stream's connection: Limited -> relay, otherwise the
// transport of the remote multiaddr (quic vs tcp).
func connPathKind(conn network.Conn) uint32 {
	if conn == nil {
		return pathDirectTCP // defensive; a scheduled stream always has a conn
	}
	if conn.Stat().Limited {
		return pathRelay
	}
	for _, p := range conn.RemoteMultiaddr().Protocols() {
		switch p.Name {
		case "quic", "quic-v1":
			return pathDirectQUIC
		case "tcp":
			return pathDirectTCP
		}
	}
	return pathDirectTCP
}

// newStreamEntry registers a handle for an open libp2p stream and starts
// its reader/writer workers.
func (n *Node) newStreamEntry(s network.Stream, peerID peer.ID) *StreamEntry {
	return n.addEntry(s, peerID, remoteMultiaddrString(s), connPathKind(s.Conn()), nil)
}

// newQuicStreamEntry registers the first (authenticated) stream of a
// punched QUIC session. pathKind is the custom-QUIC kind so the Rust side
// reports the punched path; onClose tears the bare QUIC connection down —
// connmgr does not manage it, and without this hook the session and its
// UDP socket would leak until process exit.
func (n *Node) newQuicStreamEntry(qs *quic.Stream, quicConn *quic.Conn, sock *net.UDPConn, peerID peer.ID) *StreamEntry {
	remote := ""
	if addr := quicConn.RemoteAddr(); addr != nil {
		remote = addr.String()
	}
	return n.addEntry(qs, peerID, remote, pathCustomQUIC, func() {
		// Error code 0 = clean shutdown of this session; the peer sees a
		// normal close. Closing the conn ends its single-use transport, but
		// quic-go never closes a caller-provided packet conn — the socket
		// must be closed here explicitly or the fd leaks.
		_ = quicConn.CloseWithError(0, "stream closed")
		_ = sock.Close()
	})
}

// addEntry registers a handle for an open streamIO and starts its
// reader/writer workers.
func (n *Node) addEntry(s streamIO, peerID peer.ID, remote string, pathKind uint32, onClose func()) *StreamEntry {
	se := &StreamEntry{
		s:        s,
		peer:     peerID,
		remote:   remote,
		pathKind: pathKind,
		onClose:  onClose,
		readCh:   make(chan readResult, 4),
		writeCh:  make(chan writeReq, 4),
		closedCh: make(chan struct{}),
	}
	n.streamMu.Lock()
	n.nextHandle++
	se.id = n.nextHandle
	// ConnManager is allowed to trim connections above its high watermark.
	// A live RustDesk control stream is an application session, so protect
	// its peer for the lifetime of this stream. Use a per-stream tag so two
	// concurrent sessions do not unprotect one another when one closes.
	if peerID != "" && n.host != nil && n.host.ConnManager() != nil {
		tag := fmt.Sprintf("p2pdesk-control-%d", se.id)
		n.host.ConnManager().Protect(peerID, tag)
		se.releaseProtection = func() {
			n.host.ConnManager().Unprotect(peerID, tag)
		}
	}
	// Publish only after the close hooks are initialized; a worker failure
	// or concurrent close must be able to release the peer protection.
	n.streams[se.id] = se
	n.streamMu.Unlock()
	go se.readLoop()
	go se.writeLoop()
	// Session liveness follows the control stream's transport errors and
	// RustDesk's receive timeout. An independent ping must not close an
	// otherwise active session. QUIC/yamux retain their own keepalives.
	return se
}

// readLoop is the resident reader: it owns the only Read call on the
// stream. Panics inside the transport are converted to a result, never
// propagated (the DLL shares the host process).
func (se *StreamEntry) readLoop() {
	defer func() {
		if rec := recover(); rec != nil {
			// Mirror the writer's panic path: mark the stream closed so a
			// full readCh can never leave sendRead stuck (which would hang
			// every reader forever).
			if se.closed.CompareAndSwap(false, true) {
				close(se.closedCh)
				_ = se.s.Close()
				if se.onClose != nil {
					se.onClose()
				}
				if se.releaseProtection != nil {
					se.releaseProtection()
				}
			}
			se.sendRead(readResult{err: fmt.Errorf("read panic: %v", rec)})
		}
		close(se.readCh)
	}()
	buf := make([]byte, 64*1024)
	for {
		m, err := se.s.Read(buf)
		if m > 0 {
			if err != nil {
				// Stream died with trailing data (QUIC returns (n>0, err)
				// when a reset lands mid-read): tear the session down first
				// so the sendRead calls below cannot block on a full channel
				// (closedCh is closed by then), then surface the data.
				se.readerDied(err)
				se.sendRead(readResult{n: m, data: append([]byte(nil), buf[:m]...)})
				se.sendRead(readResult{err: err})
				return
			}
			data := append([]byte(nil), buf[:m]...)
			if !se.sendRead(readResult{n: m, data: data}) {
				return // closed while the channel was full
			}
			continue
		}
		if err != nil {
			// Tear down before sendRead for the same reason as above: a
			// full readCh would otherwise block this exit path forever,
			// leaking the QUIC conn, the socket, and the writer.
			se.readerDied(err)
			se.sendRead(readResult{err: err})
			return
		}
		// (0, nil) — defensive; yamux never produces this.
		time.Sleep(time.Millisecond)
	}
}

// readerDied handles a transport-level read failure that is not a clean
// half-close (EOF). A stream reset on a punched QUIC connection leaves the
// conn alive and keepalived forever, and connmgr cannot see it — the
// reader must tear it down itself. This also releases the connmgr protection
// for ordinary libp2p control streams, so a dead entry cannot pin a peer.
func (se *StreamEntry) readerDied(err error) {
	if errors.Is(err, io.EOF) {
		return
	}
	if se.closed.CompareAndSwap(false, true) {
		close(se.closedCh)
		_ = se.s.Close()
		if se.onClose != nil {
			se.onClose()
		}
		if se.releaseProtection != nil {
			se.releaseProtection()
		}
	}
}

// sendRead pushes a result to readCh, aborting when the stream is closed —
// a full channel must never leave the reader stuck (closeStream cannot
// interrupt a channel send).
func (se *StreamEntry) sendRead(r readResult) bool {
	select {
	case se.readCh <- r:
		return true
	case <-se.closedCh:
		return false
	}
}

// writeLoop is the resident writer: it owns the only Write call on the
// stream, so a timed-out caller can never block the next writer. On exit it
// drains pending requests with an error so no caller waits forever.
func (se *StreamEntry) writeLoop() {
	defer func() {
		if rec := recover(); rec != nil {
			// A transport panic kills the writer: mark the stream closed so
			// no caller can enqueue and hang on a dead writer, and wake the
			// peer.
			if se.closed.CompareAndSwap(false, true) {
				close(se.closedCh)
				_ = se.s.Close()
				if se.onClose != nil {
					se.onClose()
				}
				if se.releaseProtection != nil {
					se.releaseProtection()
				}
			}
			se.drainWrites(fmt.Errorf("write panic: %v", rec))
		}
	}()
	for {
		select {
		case req := <-se.writeCh:
			m, err := se.s.Write(req.data)
			req.res <- writeResult{n: m, err: err}
		case <-se.closedCh:
			se.drainWrites(errStreamClosed)
			return
		}
	}
}

// closeWrite half-closes the write direction. libp2p streams expose
// CloseWrite; quic.Stream's Close sends FIN on the write direction and
// keeps the read direction open, which is the same half-close.
func (se *StreamEntry) closeWrite() error {
	if cw, ok := se.s.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return se.s.Close()
}

// drainWrites answers every queued request with an error (writer exiting).
func (se *StreamEntry) drainWrites(err error) {
	for {
		select {
		case req := <-se.writeCh:
			req.res <- writeResult{err: err}
		default:
			return
		}
	}
}

// remoteMultiaddrString is the stream's best remote address for
// diagnostics (direct multiaddr, or the relay-circuit multiaddr on relayed
// streams). Returns "" when no connection info exists — never a bogus
// multiaddr (StringCast panics on unknown protocols).
func remoteMultiaddrString(s network.Stream) string {
	if c := s.Conn(); c != nil {
		if m := c.RemoteMultiaddr(); m != nil {
			return m.String()
		}
	}
	return ""
}

// get returns the entry by handle (nil when unknown/closed).
func (n *Node) getStream(id uint64) *StreamEntry {
	n.streamMu.Lock()
	defer n.streamMu.Unlock()
	return n.streams[id]
}

// close removes the handle and closes the underlying stream. Idempotent;
// also wakes the resident workers (blocked Read returns an error; the
// writer exits via closedCh).
func (n *Node) closeStream(id uint64) {
	n.streamMu.Lock()
	se, ok := n.streams[id]
	if ok {
		delete(n.streams, id)
	}
	n.streamMu.Unlock()
	if !ok {
		return
	}
	if se.closed.Swap(true) {
		return
	}
	close(se.closedCh)
	_ = se.s.Close()
	if se.onClose != nil {
		se.onClose()
	}
	if se.releaseProtection != nil {
		se.releaseProtection()
	}
}

// closeAllStreams closes every open handle. Called from stop: the host
// close wakes its own libp2p streams, but punched QUIC connections are
// invisible to the host and must be closed explicitly or they leak (with
// their sockets) into the next node lifetime in this process.
func (n *Node) closeAllStreams() {
	n.streamMu.Lock()
	ids := make([]uint64, 0, len(n.streams))
	for id := range n.streams {
		ids = append(ids, id)
	}
	n.streamMu.Unlock()
	for _, id := range ids {
		n.closeStream(id)
	}
}

// read blocks until data / EOF / error / timeout / close. Returns:
// >=0 bytes read, 0 = EOF, negative error codes. A timeout abandons only
// the wait — the resident reader keeps the stream usable.
func (n *Node) streamRead(id uint64, buf []byte, timeoutMs uint32) int64 {
	se := n.getStream(id)
	if se == nil || se.closed.Load() {
		return errClosed
	}
	var r readResult
	var ok bool
	if timeoutMs == 0 {
		select {
		case r, ok = <-se.readCh:
		case <-n.ctx.Done():
			return errClosed
		}
	} else {
		select {
		case r, ok = <-se.readCh:
		case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
			return errTimeout
		case <-n.ctx.Done():
			return errClosed
		}
	}
	if !ok {
		return errClosed // reader exited
	}
	if r.err != nil {
		if r.err == io.EOF {
			return 0
		}
		if se.closed.Load() {
			return errClosed
		}
		setLastError("read: %v", r.err)
		return errInternal
	}
	// INVARIANT: the caller buffer must be >= the read chunk size (64 KiB,
	// matching the Rust reader worker's buffer). A smaller buffer would
	// silently drop bytes — fail loudly instead.
	if r.n > len(buf) {
		setLastError("read buffer too small: %d < %d", len(buf), r.n)
		return errInternal
	}
	copy(buf, r.data)
	return int64(r.n)
}

// write blocks until the whole buffer is sent / error / timeout / close.
// Returns bytes written (>=0) or a negative error code.
func (n *Node) streamWrite(id uint64, buf []byte, timeoutMs uint32) int64 {
	se := n.getStream(id)
	if se == nil || se.closed.Load() {
		return errClosed
	}
	// The caller buffer is copied immediately — the writer goroutine may
	// hold it while blocked in the transport (yamux copies from the slice
	// once its send window opens), and the Rust caller must be free to
	// reuse its memory right away.
	req := writeReq{
		data: append([]byte(nil), buf...),
		res:  make(chan writeResult, 1),
	}
	select {
	case se.writeCh <- req:
	case <-se.closedCh:
		return errClosed
	}
	var r writeResult
	if timeoutMs == 0 {
		select {
		case r = <-req.res:
		case <-se.closedCh:
			// The writer may have exited between enqueue and here (close
			// race): it drains queued requests, so this can only happen if
			// the enqueue lost the race — treat as closed.
			return errClosed
		case <-n.ctx.Done():
			return errClosed
		}
	} else {
		select {
		case r = <-req.res:
		case <-se.closedCh:
			return errClosed
		case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
			return errTimeout
		case <-n.ctx.Done():
			return errClosed
		}
	}
	if r.err != nil {
		if se.closed.Load() {
			return errClosed
		}
		setLastError("write: %v", r.err)
		return errInternal
	}
	return int64(r.n)
}

// streamPeerID copies the authenticated peer id into the caller buffer.
func (n *Node) streamPeerID(id uint64, out []byte) (int, int) {
	se := n.getStream(id)
	if se == nil {
		return errClosed, 0
	}
	s := se.peer.String()
	return errOK, copyLen(out, s)
}

// streamRemoteAddr copies the remote multiaddr into the caller buffer.
func (n *Node) streamRemoteAddr(id uint64, out []byte) (int, int) {
	se := n.getStream(id)
	if se == nil {
		return errClosed, 0
	}
	return errOK, copyLen(out, se.remote)
}

// streamPathKind reports the connection path kind.
func (n *Node) streamPathKind(id uint64) (int, uint32) {
	se := n.getStream(id)
	if se == nil {
		return errClosed, 0
	}
	return errOK, se.pathKind
}

func copyLen(dst []byte, s string) int {
	n := min(len(s), len(dst))
	copy(dst, s[:n])
	return n
}
