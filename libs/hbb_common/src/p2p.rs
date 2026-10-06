//! Go-libp2p c-shared transport stream integration (p2pdesk).
//!
//! `GoP2pStream` wraps `tcp::FramedStream` around a blocking C-ABI stream
//! handle (`p2pd_stream_t` from `p2pdesk_net.dll` / `libp2pdesk_net.so`), so
//! the p2p transport reuses rustdesk's length-prefixed framing + secretbox
//! encryption as-is. Zero duplication: all framing/encryption/timeout logic
//! lives in `tcp::FramedStream`.
//!
//! The blocking FFI read/write never runs on the Tokio runtime: each stream
//! gets a reader worker thread (blocking read → bounded tokio channel) and
//! a writer worker thread (bounded tokio channel → blocking write → oneshot
//! ack). The reader is single-consumer; `poll_write` accepts bytes once
//! channel capacity is reserved. A full queue retains its pending capacity
//! reservation so draining the queue wakes the sender.
//!
//! A `Mock` backend (in-process byte pipe, no library needed) backs the same
//! stream type for local two-instance loops before the Go module exists.

use crate::{tcp, ResultType};
use bytes::{Bytes, BytesMut};
use protobuf::Message;
use sodiumoxide::crypto::secretbox::Key;
use std::io;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::mpsc as std_mpsc;
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio_util::sync::PollSender;

// ============================ C ABI ============================
// The Go module (`p2pdesk_net.dll` / `libp2pdesk_net.so`) is loaded at
// runtime (dlopen) by the main crate's `src/p2pffi.rs`, so the stream-level
// functions below cannot be link-time `extern` symbols — the loader resolves
// them with libloading and registers them here. Node-level functions
// (start/stop/status/connect/accept/sign/peer_id) stay in `src/p2pffi.rs`.

pub type P2pdStream = u64;

#[repr(C)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct P2pdResult {
    pub code: i32,       /* 0 = ok; stable negative error code */
    pub detail_len: u32, /* UTF-8 diagnostic copied into caller buffer */
}

// stream path kinds (p2pd_stream_path_kind)
pub const PATH_DIRECT_QUIC: u32 = 0;
pub const PATH_DIRECT_TCP: u32 = 1;
pub const PATH_DCUTR: u32 = 2;
pub const PATH_CUSTOM_QUIC: u32 = 3;
pub const PATH_RELAY: u32 = 4;

// p2pd_stream_read/write return values (<0 = error, 0 = EOF for read)
pub const IO_ERR_TIMEOUT: i64 = -1;
pub const IO_ERR_CLOSED: i64 = -2;
pub const IO_ERR_INVALID: i64 = -3;
pub const IO_ERR_INTERNAL: i64 = -4;

/// Resolved stream-level symbols of the loaded Go module.
pub struct StreamFns {
    pub peer_id: unsafe extern "C" fn(P2pdStream, *mut u8, u32, *mut u32) -> P2pdResult,
    pub remote_addr: unsafe extern "C" fn(P2pdStream, *mut u8, u32, *mut u32) -> P2pdResult,
    pub path_kind: unsafe extern "C" fn(P2pdStream, *mut u32) -> P2pdResult,
    pub read: unsafe extern "C" fn(P2pdStream, *mut u8, u32, u32) -> i64,
    pub write: unsafe extern "C" fn(P2pdStream, *const u8, u32, u32) -> i64,
    pub close_write: unsafe extern "C" fn(P2pdStream) -> P2pdResult,
    pub close: unsafe extern "C" fn(P2pdStream) -> P2pdResult,
}

static STREAM_FNS: std::sync::OnceLock<StreamFns> = std::sync::OnceLock::new();

/// Register the stream-level symbols after the Go module is loaded (main
/// crate). Fails if already registered — a second load of the same library
/// yields the same addresses, so registering once is correct.
pub fn register_stream_fns(fns: StreamFns) -> Result<(), ()> {
    STREAM_FNS.set(fns).map_err(|_| ())
}

/// The registered stream symbols; `None` when no Go module is loaded (mock
/// mode or missing library).
pub fn stream_fns() -> Option<&'static StreamFns> {
    STREAM_FNS.get()
}

// ============================ backend ============================

/// One end of the blocking IO, shared by the main object and the two worker
/// threads (each holds a copy).
enum GoIo {
    /// Real c-shared stream handle (u64 copy).
    Ffi(P2pdStream),
    /// In-process mock. The main object holds the writer half; the reader
    /// half was moved into the reader worker. Dropping the writer delivers
    /// EOF to the peer's reader — that is the mock's close.
    Mock {
        write: std_mpsc::Sender<io::Result<Vec<u8>>>,
    },
}

impl Clone for GoIo {
    fn clone(&self) -> Self {
        match self {
            GoIo::Ffi(h) => GoIo::Ffi(*h),
            GoIo::Mock { write } => GoIo::Mock {
                write: write.clone(),
            },
        }
    }
}

/// Map a negative C-ABI return value to a stable io::Error.
fn io_err(code: i64) -> io::Error {
    let kind = match code {
        IO_ERR_TIMEOUT => io::ErrorKind::TimedOut,
        IO_ERR_CLOSED => io::ErrorKind::BrokenPipe,
        _ => io::ErrorKind::Other,
    };
    io::Error::new(kind, format!("p2p stream io error {code}"))
}

/// One blocking read on the FFI backend (`timeout_ms = 0` = block until
/// close; rustdesk's own read timeouts wrap the async side via
/// `next_timeout`).
fn ffi_blocking_read(handle: P2pdStream, buf: &mut [u8]) -> io::Result<usize> {
    let fns = stream_fns().ok_or_else(|| {
        io::Error::new(io::ErrorKind::Unsupported, "p2p module not loaded")
    })?;
    let n = unsafe { (fns.read)(handle, buf.as_mut_ptr(), buf.len() as u32, 0) };
    if n >= 0 {
        Ok(n as usize)
    } else {
        Err(io_err(n))
    }
}

/// One blocking write on the FFI backend. `timeout_ms = 0` = block forever
/// (the ABI contract: a timed-out write must return `IO_ERR_TIMEOUT`, and
/// the stream is then closed by the caller — see the writer worker).
fn ffi_blocking_write(handle: P2pdStream, data: &[u8], timeout_ms: u64) -> io::Result<()> {
    let fns = stream_fns().ok_or_else(|| {
        io::Error::new(io::ErrorKind::Unsupported, "p2p module not loaded")
    })?;
    let n = unsafe { (fns.write)(handle, data.as_ptr(), data.len() as u32, timeout_ms as u32) };
    if n == data.len() as i64 {
        Ok(())
    } else if n < 0 {
        Err(io_err(n))
    } else {
        Err(io::Error::new(
            io::ErrorKind::WriteZero,
            format!("p2p stream short write: {n}"),
        ))
    }
}

// ============================ io adapter ============================

/// An `AsyncRead + AsyncWrite` adapter over a blocking backend, driven by two
/// worker threads. Implements `TcpStreamTrait`, so it drops straight into
/// `tcp::FramedStream`.
pub struct GoIoStream {
    io: GoIo,
    /// Bytes taken from the channel but not yet consumed by poll_read.
    pending: Option<Bytes>,
    read_rx: tokio::sync::mpsc::Receiver<io::Result<Bytes>>,
    write_tx: PollSender<(Bytes, tokio::sync::oneshot::Sender<io::Result<()>>)>,
    _reader: std::thread::JoinHandle<()>,
    _writer: std::thread::JoinHandle<()>,
    closed: std::sync::Arc<AtomicBool>,
}

impl GoIoStream {
    /// Wrap a real stream handle.
    pub fn from_handle(handle: P2pdStream, send_timeout: std::sync::Arc<AtomicU64>) -> Self {
        Self::new(GoIo::Ffi(handle), None, send_timeout)
    }

    /// Mock backend: one end of an in-process pipe. `write` forwards our
    /// writes to the peer; `mock_rx` (moved into the reader worker) receives
    /// the peer's writes.
    pub fn mock_inner(
        write: std_mpsc::Sender<io::Result<Vec<u8>>>,
        mock_rx: Option<std_mpsc::Receiver<io::Result<Vec<u8>>>>,
        send_timeout: std::sync::Arc<AtomicU64>,
    ) -> Self {
        Self::new(GoIo::Mock { write }, mock_rx, send_timeout)
    }

    fn new(
        io: GoIo,
        mock_rx: Option<std_mpsc::Receiver<io::Result<Vec<u8>>>>,
        send_timeout: std::sync::Arc<AtomicU64>,
    ) -> Self {
        // Bounded read path: a fast peer cannot grow memory without bound;
        // the worker parks in blocking_send and the Go side's own flow
        // control eventually applies backpressure.
        let (read_tx, read_rx) = tokio::sync::mpsc::channel::<io::Result<Bytes>>(64);
        let (write_tx, mut write_rx) = tokio::sync::mpsc::channel::<(
            Bytes,
            tokio::sync::oneshot::Sender<io::Result<()>>,
        )>(8);
        let closed = std::sync::Arc::new(AtomicBool::new(false));

        // Reader worker: block on the backend until data / EOF / error, then
        // forward (blocking_send parks when the bounded channel is full).
        //
        // IMPORTANT: the worker must NOT hold a `GoIo` copy — in mock mode
        // that copy keeps the peer-facing writer sender alive, so the peer's
        // reader would never see EOF when this stream is dropped. It takes
        // exactly what it needs: the FFI handle (u64 copy) or the mock
        // receiver.
        let reader_handle = match &io {
            GoIo::Ffi(h) => Some(*h),
            GoIo::Mock { .. } => None,
        };
        let reader = std::thread::Builder::new()
            .name("p2pd-read".into())
            .spawn(move || {
                let mut buf = vec![0u8; 64 * 1024];
                let mut mock_rx = mock_rx;
                let res = loop {
                    let r = if let Some(h) = reader_handle {
                        ffi_blocking_read(h, &mut buf).map(Some)
                    } else {
                        let Some(rx) = mock_rx.as_mut() else {
                            break Err(io::Error::new(
                                io::ErrorKind::Unsupported,
                                "p2p mock reader not wired",
                            ));
                        };
                        match rx.recv() {
                            Ok(Ok(v)) => {
                                if v.is_empty() {
                                    break Ok(0);
                                }
                                if read_tx.blocking_send(Ok(Bytes::from(v))).is_err() {
                                    break Err(io::Error::new(
                                        io::ErrorKind::BrokenPipe,
                                        "p2p stream reader dropped",
                                    ));
                                }
                                continue;
                            }
                            Ok(Err(e)) => break Err(e),
                            Err(_) => break Ok(0), // peer closed = EOF
                        }
                    };
                    match r {
                        Ok(Some(0)) => break Ok(0), // EOF
                        Ok(Some(n)) => {
                            if read_tx
                                .blocking_send(Ok(Bytes::copy_from_slice(&buf[..n])))
                                .is_err()
                            {
                                break Err(io::Error::new(
                                    io::ErrorKind::BrokenPipe,
                                    "p2p stream reader dropped",
                                ));
                            }
                        }
                        Ok(None) => break Ok(0), // defensive; cannot happen
                        Err(e) => break Err(e),
                    }
                };
                // EOF surfaces as an io error so rustdesk's io_loop sees a
                // dead stream ("Reset by the peer" path) instead of a silent
                // hang.
                let final_res = match res {
                    Ok(0) => Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        "p2p stream closed by peer",
                    )),
                    Ok(_) => Ok(Bytes::new()),
                    Err(e) => Err(e),
                };
                let _ = read_tx.send(final_res);
            })
            .expect("spawn p2pd-read");

        // Writer worker: take (data, ack) and block on the backend write.
        // `send_timeout` (set via GoP2pStream::set_send_timeout) is passed
        // into the FFI write so rustdesk's send timeout actually covers the
        // blocking write, not just the channel wait .
        let writer_io = io.clone();
        let writer_timeout = send_timeout.clone();
        let writer = std::thread::Builder::new()
            .name("p2pd-write".into())
            .spawn(move || {
                while let Some((data, ack)) = write_rx.blocking_recv() {
                    let timeout_ms = writer_timeout.load(Ordering::SeqCst);
                    let res = match &writer_io {
                        GoIo::Ffi(h) => ffi_blocking_write(*h, &data, timeout_ms),
                        GoIo::Mock { write } => write
                            .send(Ok(data.to_vec()))
                            .map_err(|_| {
                                io::Error::new(io::ErrorKind::BrokenPipe, "mock peer closed")
                            }),
                    };
                    if res.is_err() {
                        // Failed write = dead stream: close it so the read
                        // side surfaces EOF (the peer notices like a dropped
                        // TCP connection).
                        if let GoIo::Ffi(h) = &writer_io {
                            if let Some(fns) = stream_fns() {
                                let _ = unsafe { (fns.close)(*h) };
                            }
                        }
                    }
                    let _ = ack.send(res);
                }
            })
            .expect("spawn p2pd-write");

        Self {
            io,
            pending: None,
            read_rx,
            write_tx: PollSender::new(write_tx),
            _reader: reader,
            _writer: writer,
            closed,
        }
    }

    /// Close once (idempotent): the FFI close wakes the blocking reader; the
    /// mock needs no action (peer sees EOF when our writer half drops).
    pub fn close(&self) {
        if self.closed.swap(true, Ordering::SeqCst) {
            return;
        }
        if let GoIo::Ffi(h) = &self.io {
            if let Some(fns) = stream_fns() {
                let _ = unsafe { (fns.close)(*h) };
            }
        }
    }
}

impl Drop for GoIoStream {
    fn drop(&mut self) {
        self.close();
        // Dropping write_tx ends the writer worker loop.
    }
}

impl AsyncRead for GoIoStream {
    fn poll_read(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        let this = self.get_mut();
        // Local close must surface as an error on the read side too (the
        // peer's EOF comes later via the channel) — matches TCP semantics
        // and keeps mock/FFI behavior aligned .
        if this.closed.load(Ordering::SeqCst) {
            return std::task::Poll::Ready(Err(io::Error::new(
                io::ErrorKind::BrokenPipe,
                "p2p stream closed",
            )));
        }
        // Serve leftover bytes first (a 64 KiB worker block may exceed one
        // poll_read's buffer space).
        if let Some(data) = this.pending.take() {
            let n = std::cmp::min(data.len(), buf.remaining());
            buf.put_slice(&data[..n]);
            if n < data.len() {
                this.pending = Some(data.slice(n..));
            }
            return std::task::Poll::Ready(Ok(()));
        }
        match this.read_rx.poll_recv(cx) {
            std::task::Poll::Ready(Some(Ok(data))) => {
                let n = std::cmp::min(data.len(), buf.remaining());
                buf.put_slice(&data[..n]);
                if n < data.len() {
                    this.pending = Some(data.slice(n..));
                }
                std::task::Poll::Ready(Ok(()))
            }
            std::task::Poll::Ready(Some(Err(e))) => std::task::Poll::Ready(Err(e)),
            std::task::Poll::Ready(None) => std::task::Poll::Ready(Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "p2p stream reader closed",
            ))),
            std::task::Poll::Pending => std::task::Poll::Pending,
        }
    }
}

impl AsyncWrite for GoIoStream {
    fn poll_write(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> std::task::Poll<io::Result<usize>> {
        let this = self.get_mut();
        if this.closed.load(Ordering::SeqCst) {
            return std::task::Poll::Ready(Err(io::Error::new(
                io::ErrorKind::BrokenPipe,
                "p2p stream closed",
            )));
        }
        // PollSender retains the capacity waiter across Pending. A temporary
        // Notify::notified() would unregister its waker when poll_write returns,
        // leaving the sender asleep even after the worker drains the queue.
        //
        // IMPORTANT: once the data is queued we return Ready(Ok(n)) right
        // away — TCP semantics (accepted = written). Waiting for the worker
        // ack here returns Pending, and futures' poll_flush re-polls with the
        // SAME buffer (it only advances on Ok), duplicating the frame.
        match this.write_tx.poll_reserve(cx) {
            std::task::Poll::Pending => return std::task::Poll::Pending,
            std::task::Poll::Ready(Err(_)) => {
                return std::task::Poll::Ready(Err(io::Error::new(
                    io::ErrorKind::BrokenPipe,
                    "p2p stream writer dropped",
                )));
            }
            std::task::Poll::Ready(Ok(())) => {}
        }
        // Preserve accepted-write semantics: each buffer is queued exactly once.
        let (ack_tx, _ack_rx) = tokio::sync::oneshot::channel();
        std::task::Poll::Ready(
            this.write_tx
                .send_item((Bytes::copy_from_slice(buf), ack_tx))
                .map(|_| buf.len())
                .map_err(|_| io::Error::new(io::ErrorKind::BrokenPipe, "p2p stream writer dropped")),
        )
    }

    fn poll_flush(
        self: std::pin::Pin<&mut Self>,
        _cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        // poll_write accepts data immediately (TCP semantics) and the worker
        // drains the queue independently — nothing to flush here.
        std::task::Poll::Ready(Ok(()))
    }

    fn poll_shutdown(
        self: std::pin::Pin<&mut Self>,
        _cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        self.get_mut().close();
        std::task::Poll::Ready(Ok(()))
    }
}

// `TcpStreamTrait` has a blanket impl for `AsyncRead + AsyncWrite + Unpin`,
// which GoIoStream satisfies — no explicit impl needed.

// ============================ framed stream ============================

/// A rustdesk `FramedStream` (length-prefixed + secretbox) over a p2p stream,
/// plus the remote identity queried from the handle.
pub struct GoP2pStream {
    inner: tcp::FramedStream,
    peer: String,
    remote: String,
    path_kind: u32,
    local: SocketAddr,
    /// Mirrors `FramedStream`'s send timeout so the writer worker can pass it
    /// into the blocking FFI write .
    send_timeout: std::sync::Arc<AtomicU64>,
}

impl GoP2pStream {
    /// Wrap a stream handle from the Go module, querying its metadata
    /// (peer id / remote multiaddr / path kind).
    pub fn from_handle(handle: P2pdStream) -> ResultType<Self> {
        let (peer, remote, path_kind) = stream_meta(handle)?;
        let placeholder =
            SocketAddr::new(std::net::IpAddr::V4(std::net::Ipv4Addr::UNSPECIFIED), 0);
        // The timeout cell is created ONCE and shared with the writer worker
        // (the worker reads it on every write; set_send_timeout writes it).
        let send_timeout = std::sync::Arc::new(AtomicU64::new(0));
        Ok(Self {
            inner: tcp::FramedStream::from(
                GoIoStream::from_handle(handle, send_timeout.clone()),
                placeholder,
            ),
            peer,
            remote,
            path_kind,
            local: placeholder,
            send_timeout,
        })
    }

    /// Mock backend: one end of an in-process pipe with a given identity.
    /// The other end is returned for the peer side.
    pub fn mock_pair(
        peer_a: String,
        remote_a: String,
        peer_b: String,
        remote_b: String,
    ) -> (Self, Self) {
        let (tx_a2b, rx_a2b) = std_mpsc::channel::<io::Result<Vec<u8>>>();
        let (tx_b2a, rx_b2a) = std_mpsc::channel::<io::Result<Vec<u8>>>();
        let placeholder =
            SocketAddr::new(std::net::IpAddr::V4(std::net::Ipv4Addr::UNSPECIFIED), 0);
        let send_timeout_a = std::sync::Arc::new(AtomicU64::new(0));
        let send_timeout_b = std::sync::Arc::new(AtomicU64::new(0));
        let a = Self {
            inner: tcp::FramedStream::from(
                GoIoStream::mock_inner(tx_a2b, Some(rx_b2a), send_timeout_a.clone()),
                placeholder,
            ),
            peer: peer_a,
            remote: remote_a,
            path_kind: PATH_DIRECT_TCP,
            local: placeholder,
            send_timeout: send_timeout_a,
        };
        let b = Self {
            inner: tcp::FramedStream::from(
                GoIoStream::mock_inner(tx_b2a, Some(rx_a2b), send_timeout_b.clone()),
                placeholder,
            ),
            peer: peer_b,
            remote: remote_b,
            path_kind: PATH_DIRECT_TCP,
            local: placeholder,
            send_timeout: send_timeout_b,
        };
        (a, b)
    }

    pub fn peer_id(&self) -> &str {
        &self.peer
    }

    pub fn remote(&self) -> &str {
        &self.remote
    }

    pub fn path_kind(&self) -> u32 {
        self.path_kind
    }

    pub fn set_send_timeout(&mut self, ms: u64) {
        self.inner.set_send_timeout(ms);
        self.send_timeout.store(ms, Ordering::SeqCst);
    }

    pub fn set_raw(&mut self) {
        self.inner.set_raw();
    }

    pub async fn send_bytes(&mut self, bytes: Bytes) -> ResultType<()> {
        self.inner.send_bytes(bytes).await
    }

    pub async fn send_raw(&mut self, bytes: Vec<u8>) -> ResultType<()> {
        self.inner.send_raw(bytes).await
    }

    pub fn set_key(&mut self, key: Key) {
        self.inner.set_key(key);
    }

    pub fn is_secured(&self) -> bool {
        self.inner.is_secured()
    }

    pub async fn next_timeout(&mut self, timeout: u64) -> Option<io::Result<BytesMut>> {
        self.inner.next_timeout(timeout).await
    }

    pub async fn send(&mut self, msg: &impl Message) -> ResultType<()> {
        self.inner.send(msg).await
    }

    pub async fn next(&mut self) -> Option<io::Result<BytesMut>> {
        let res = self.inner.next().await;
        // rustdesk's io_loop shows only "bytes remaining
        // on stream(0)" when a p2p substream dies — the underlying error is
        // swallowed. Log it here (peer context makes it actionable).
        if let Some(Err(e)) = &res {
            log::warn!("p2p stream read error (peer {}): {e}", self.peer);
        }
        res
    }

    pub fn local_addr(&self) -> SocketAddr {
        self.local
    }
}

/// Query peer id + remote multiaddr + path kind from a fresh handle.
fn stream_meta(handle: P2pdStream) -> ResultType<(String, String, u32)> {
    let fns = stream_fns()
        .ok_or_else(|| anyhow::anyhow!("p2p module not loaded"))?;
    let mut peer = Vec::with_capacity(128);
    loop {
        let mut written: u32 = 0;
        let r = unsafe { (fns.peer_id)(handle, peer.as_mut_ptr(), peer.capacity() as u32, &mut written) };
        if r.code != 0 {
            return Err(anyhow::anyhow!("p2pd_stream_peer_id failed: code {}", r.code));
        }
        // Clamp: a misbehaving module must not drive set_len past capacity
        // (UB) or panic slice indexing .
        let n = (written as usize).min(peer.capacity());
        unsafe {
            peer.set_len(n);
        }
        if n < peer.capacity() {
            break;
        }
        peer.reserve(64);
    }
    let peer = String::from_utf8_lossy(&peer).into_owned();

    let mut remote = Vec::with_capacity(128);
    loop {
        let mut written: u32 = 0;
        let r = unsafe {
            (fns.remote_addr)(handle, remote.as_mut_ptr(), remote.capacity() as u32, &mut written)
        };
        if r.code != 0 {
            return Err(anyhow::anyhow!("p2pd_stream_remote_addr failed: code {}", r.code));
        }
        let n = (written as usize).min(remote.capacity());
        unsafe {
            remote.set_len(n);
        }
        if n < remote.capacity() {
            break;
        }
        remote.reserve(64);
    }
    let remote = String::from_utf8_lossy(&remote).into_owned();

    let mut kind: u32 = PATH_DIRECT_TCP;
    let r = unsafe { (fns.path_kind)(handle, &mut kind) };
    if r.code != 0 {
        log::warn!("p2pd_stream_path_kind failed: code {}", r.code);
    }
    Ok((peer, remote, kind))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Default)]
    struct WakeCounter(std::sync::atomic::AtomicUsize);

    impl std::task::Wake for WakeCounter {
        fn wake(self: std::sync::Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }

        fn wake_by_ref(self: &std::sync::Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    // Drive the production AsyncWrite adapter while controlling when its worker
    // drains the queue. No timer or unrelated IO can hide a missing wakeup.
    fn queued_writer() -> (
        GoIoStream,
        tokio::sync::mpsc::Receiver<(Bytes, tokio::sync::oneshot::Sender<io::Result<()>>)>,
    ) {
        let (write, _) = std_mpsc::channel();
        let (_, read_rx) = tokio::sync::mpsc::channel(1);
        let (write_tx, write_rx) = tokio::sync::mpsc::channel(8);
        let stream = GoIoStream {
            io: GoIo::Mock { write },
            pending: None,
            read_rx,
            write_tx: PollSender::new(write_tx),
            _reader: std::thread::spawn(|| {}),
            _writer: std::thread::spawn(|| {}),
            closed: std::sync::Arc::new(AtomicBool::new(false)),
        };
        (stream, write_rx)
    }

    #[test]
    fn write_queue_capacity_wakes_and_preserves_bytes() {
        let (mut stream, mut receiver) = queued_writer();
        let counter = std::sync::Arc::new(WakeCounter::default());
        let waker = std::task::Waker::from(counter.clone());
        let mut cx = std::task::Context::from_waker(&waker);
        let mut observed = Vec::new();
        // Repeated full/drain cycles must wake without relying on send timeout.
        for cycle in 0..32u8 {
            for offset in 0..8u8 {
                let byte = cycle.wrapping_mul(9).wrapping_add(offset);
                assert!(matches!(std::pin::Pin::new(&mut stream).poll_write(&mut cx, &[byte]),
                    std::task::Poll::Ready(Ok(1))));
            }
            let last = cycle.wrapping_mul(9).wrapping_add(8);
            assert!(std::pin::Pin::new(&mut stream).poll_write(&mut cx, &[last]).is_pending());
            let before = counter.0.load(Ordering::SeqCst);
            observed.extend_from_slice(&receiver.try_recv().unwrap().0);
            assert!(counter.0.load(Ordering::SeqCst) > before, "full writer lost its wakeup");
            assert!(matches!(std::pin::Pin::new(&mut stream).poll_write(&mut cx, &[last]),
                std::task::Poll::Ready(Ok(1))));
            while let Ok((bytes, _)) = receiver.try_recv() {
                observed.extend_from_slice(&bytes);
            }
        }
        let expected: Vec<u8> = (0..288u16).map(|i| i as u8).collect();
        assert_eq!(observed, expected, "writes must not be lost, duplicated or reordered");
    }

    #[test]
    fn write_queue_close_wakes_pending_writer() {
        let (mut stream, receiver) = queued_writer();
        let counter = std::sync::Arc::new(WakeCounter::default());
        let waker = std::task::Waker::from(counter.clone());
        let mut cx = std::task::Context::from_waker(&waker);
        for _ in 0..8 {
            assert!(matches!(std::pin::Pin::new(&mut stream).poll_write(&mut cx, b"x"),
                std::task::Poll::Ready(Ok(1))));
        }
        assert!(std::pin::Pin::new(&mut stream).poll_write(&mut cx, b"y").is_pending());
        let before = counter.0.load(Ordering::SeqCst);
        drop(receiver);
        assert!(counter.0.load(Ordering::SeqCst) > before, "closed worker must wake its writer");
        assert!(matches!(std::pin::Pin::new(&mut stream).poll_write(&mut cx, b"y"),
            std::task::Poll::Ready(Err(e)) if e.kind() == io::ErrorKind::BrokenPipe));
    }

    fn pair() -> (GoP2pStream, GoP2pStream) {
        GoP2pStream::mock_pair(
            "peer-a".into(),
            "/mock/a".into(),
            "peer-b".into(),
            "/mock/b".into(),
        )
    }

    #[tokio::test]
    async fn mock_pair_bidirectional() {
        let (mut a, mut b) = pair();
        assert_eq!(a.peer_id(), "peer-a");
        assert_eq!(b.peer_id(), "peer-b");
        let t = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            a.send_raw(b"hello".to_vec()),
        );
        t.await.expect("a.send_raw timed out").unwrap();
        let t = tokio::time::timeout(std::time::Duration::from_secs(5), b.next());
        let data = t.await.expect("b.next timed out").unwrap().unwrap();
        assert_eq!(&data[..], b"hello");
        let t = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            b.send_raw(b"world".to_vec()),
        );
        t.await.expect("b.send_raw timed out").unwrap();
        let t = tokio::time::timeout(std::time::Duration::from_secs(5), a.next());
        let data = t.await.expect("a.next timed out").unwrap().unwrap();
        assert_eq!(&data[..], b"world");
    }

    #[tokio::test]
    async fn mock_pair_eof_on_drop() {
        let (a, mut b) = pair();
        drop(a);
        let res = b.next().await.unwrap();
        assert!(res.is_err(), "peer close must surface as read error");
    }

    #[tokio::test]
    async fn mock_pair_backpressure_and_close_write() {
        let (mut a, mut b) = pair();
        // several frames through the bounded write channel (capacity 8)
        for i in 0..20u32 {
            let t = tokio::time::timeout(
                std::time::Duration::from_secs(5),
                a.send_raw(format!("frame-{i}").into_bytes()),
            );
            t.await
                .unwrap_or_else(|_| panic!("send_raw frame-{i} timed out"))
                .unwrap();
        }
        for i in 0..20u32 {
            let t = tokio::time::timeout(std::time::Duration::from_secs(5), b.next());
            let data = t
                .await
                .unwrap_or_else(|_| panic!("b.next frame-{i} timed out"))
                .unwrap()
                .unwrap();
            assert_eq!(&data[..], format!("frame-{i}").as_bytes());
        }
    }
}
