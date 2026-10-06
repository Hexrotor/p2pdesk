//! p2pdesk: go-libp2p c-shared module (`p2pdesk_net`) FFI bridge.
//!
//! Owns the node lifecycle (load library / mock, start/stop) and the
//! connect/accept/sign/status surface used by the rustdesk integration.
//! Stream IO lives in `hbb_common::p2p` (`GoP2pStream`), whose stream-level
//! symbols are resolved here and registered once.
//!
//! ABI v1, extended with `p2pd_add_peer_address` (LAN discovery),
//! `p2pd_node_sign` (SignedId signing — the private key never enters Rust
//! memory), and `p2pd_set_log_callback` (Go logs bridged into the rustdesk
//! log; optional symbol, absent on older DLLs).
//!
//! Modes:
//!   * `P2PDESK_MOCK=1` — in-process mock node (byte pipes), no library
//!     needed; used for local two-instance loops before the Go module exists.
//!   * default — load `p2pdesk_net.dll` / `libp2pdesk_net.so` from the
//!     executable directory (or `P2PDESK_NET_LIB`), verify `p2pd_abi_version`.
//!     Missing library = not initialized; every API returns a clear error —
//!     never a panic.

use hbb_common::{
    anyhow::{anyhow, bail, Context},
    config::Config,
    log,
    p2p::{
        GoP2pStream, P2pdResult, P2pdStream, StreamFns, PATH_DIRECT_QUIC, PATH_DIRECT_TCP,
    },
    sodiumoxide::crypto::sign as nsign,
    tokio,
};
use std::collections::VecDeque;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, OnceLock};

// ============================ ABI v1 ============================

pub type P2pdNode = u64;

#[repr(C)]
#[derive(Clone, Copy)]
pub struct P2pdStatus {
    pub connected_peers: u32,
    pub relay_connections: u32,
    pub direct_connections: u32,
    pub dht_ready: u8,
    pub relay_ready: u8,
    // Tail-appended fields (ABI v1 append-only). The struct is always
    // zero-initialized before the FFI call, so an older module that writes
    // only the first five fields leaves safe defaults here.
    pub routing_table_size: u32,
    pub relay_reserved: u8,
}

/// Error codes returned by p2pd_* (negative). Positive read returns are
/// byte counts; 0 from read is EOF.
pub const P2PD_OK: i32 = 0;
pub const P2PD_ERR_TIMEOUT: i32 = -1;
pub const P2PD_ERR_CLOSED: i32 = -2;
pub const P2PD_ERR_NOT_FOUND: i32 = -3; // peer not found / invalid peer id
pub const P2PD_ERR_INTERNAL: i32 = -4;
pub const P2PD_ERR_UNSUPPORTED: i32 = -5;

/// The resolved node-level symbols of the Go module. Kept together with the
/// `Library` handle so the addresses stay valid for the module's lifetime.
struct LoadedModule {
    _lib: libloading::Library,
    node_start: unsafe extern "C" fn(*const u8, u32, *mut P2pdNode) -> P2pdResult,
    node_stop: unsafe extern "C" fn(P2pdNode) -> P2pdResult,
    node_status: unsafe extern "C" fn(P2pdNode, *mut P2pdStatus) -> P2pdResult,
    node_peer_id: unsafe extern "C" fn(P2pdNode, *mut u8, u32, *mut u32) -> P2pdResult,
    connect: unsafe extern "C" fn(P2pdNode, *const u8, u32, u32, *mut P2pdStream) -> P2pdResult,
    accept: unsafe extern "C" fn(P2pdNode, u32, *mut P2pdStream) -> P2pdResult,
    add_peer_address: unsafe extern "C" fn(
        P2pdNode,
        *const u8,
        u32,
        *const u8,
        u32,
    ) -> P2pdResult,
    node_sign: unsafe extern "C" fn(
        P2pdNode,
        *const u8,
        u32,
        *mut u8,
        u32,
        *mut u32,
    ) -> P2pdResult,
    connected_peers: unsafe extern "C" fn(
        P2pdNode,
        *mut u8,
        u32,
        *mut u32,
    ) -> P2pdResult,
    last_error: unsafe extern "C" fn(*mut u8, u32, *mut u32) -> P2pdResult,
    set_log_callback: Option<unsafe extern "C" fn(*mut std::ffi::c_void)>,
}

/// The node: a live Go host or the in-process mock.
enum NodeState {
    Ffi { node: P2pdNode, module: Arc<LoadedModule> },
    Mock(MockNode),
}

static NODE: OnceLock<Mutex<Option<Arc<NodeState>>>> = OnceLock::new();

/// Serializes `init()` so two concurrent callers (UI warmup + server
/// accept loop) cannot double-start the node .
static INIT_LOCK: OnceLock<Mutex<()>> = OnceLock::new();

/// Set by `reset_identity` (real module): after the DLL is unloaded the
/// registered stream function pointers are stale, so re-initializing in the
/// same process must be refused — the rustdesk reset flow restarts the
/// process anyway .
static RESET_FLAG: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

/// Reason the most recent init attempt failed, surfaced by `status_json`.
/// Mutex-wrapped so a later successful init can clear it.
static INIT_ERROR: OnceLock<Mutex<String>> = OnceLock::new();

fn node_state() -> hbb_common::ResultType<Arc<NodeState>> {
    NODE.get()
        .and_then(|m| m.lock().ok().and_then(|g| g.clone()))
        .ok_or_else(|| anyhow!("p2p module not initialized"))
}

/// Bridge for Go-side slog records: the module calls back with
/// (level, msg bytes); levels map 0=debug, 1=info, 2=warn, 3=error. Runs on
/// arbitrary Go threads — must never call back into the module (reentrancy).
unsafe extern "C" fn go_log_bridge(level: i32, msg: *const u8, len: i32) {
    let text = unsafe {
        if len <= 0 || msg.is_null() {
            return;
        }
        std::str::from_utf8(std::slice::from_raw_parts(msg, len as usize))
            .unwrap_or("<invalid utf8>")
            .to_string()
    };
    match level {
        0 => log::debug!("[go] {text}"),
        1 => log::info!("[go] {text}"),
        2 => log::warn!("[go] {text}"),
        _ => log::error!("[go] {text}"),
    }
}

/// Diagnostic text for a failed call: `p2pd_last_error` (caller buffer).
fn err_detail(m: &LoadedModule, r: &P2pdResult) -> String {
    let mut buf = [0u8; 512];
    let mut written: u32 = 0;
    let r2 = unsafe { (m.last_error)(buf.as_mut_ptr(), buf.len() as u32, &mut written) };
    let n = (written as usize).min(buf.len()); // clamp
    if r2.code == P2PD_OK && n > 0 {
        String::from_utf8_lossy(&buf[..n]).into_owned()
    } else {
        format!("code {}", r.code)
    }
}

// ============================ loader ============================

/// Default library path: the executable's directory. `P2PDESK_NET_LIB`
/// overrides. A missing library simply leaves the module uninitialized.
fn default_lib_path() -> Result<PathBuf, String> {
    let name = if cfg!(target_os = "windows") {
        "p2pdesk_net.dll"
    } else if cfg!(target_os = "macos") {
        "libp2pdesk_net.dylib"
    } else {
        "libp2pdesk_net.so"
    };
    #[cfg(target_os = "android")]
    {
        // Android: the Kotlin side loads libp2pdesk_net.so first
        // (System.loadLibrary), so dlopen by bare name resolves the
        // already-loaded library (bionic matches loaded sonames).
        Ok(PathBuf::from(name))
    }
    #[cfg(not(target_os = "android"))]
    {
        let exe = std::env::current_exe()
            .map_err(|e| format!("current_exe: {e}"))?;
        Ok(exe.parent().map(|p| p.join(name)).unwrap_or(PathBuf::from(name)))
    }
}

fn load_module() -> Result<Arc<LoadedModule>, String> {
    let path = match std::env::var("P2PDESK_NET_LIB") {
        Ok(p) if !p.is_empty() => PathBuf::from(p),
        _ => default_lib_path()?,
    };
    // Android resolves the library by bare soname (already loaded by the
    // Kotlin side) — no filesystem check possible, dlopen handles it. Other
    // platforms check the file exists for a clear error.
    #[cfg(target_os = "android")]
    let missing = false;
    #[cfg(not(target_os = "android"))]
    let missing = !path.exists();
    if missing {
        return Err(format!("p2p module not found: {}", path.display()));
    }
    unsafe {
        let lib = libloading::Library::new(&path)
            .map_err(|e| format!("load {}: {e}", path.display()))?;
        macro_rules! sym {
            ($name:literal, $t:ty) => {{
                *lib.get::<$t>(concat!($name, "\0").as_bytes())
                    .map_err(|e| format!("symbol {}: {e}", $name))?
            }};
        }
        // Resolve every symbol before moving `lib` into the struct (field
        // order would otherwise move it first).
        let abi_version_fn: unsafe extern "C" fn() -> u32 =
            sym!("p2pd_abi_version", unsafe extern "C" fn() -> u32);
        if abi_version_fn() < 1 {
            return Err(format!("p2pd_abi_version {} < 1", abi_version_fn()));
        }
        let node_start: unsafe extern "C" fn(*const u8, u32, *mut P2pdNode) -> P2pdResult =
            sym!("p2pd_node_start", unsafe extern "C" fn(*const u8, u32, *mut P2pdNode) -> P2pdResult);
        let node_stop: unsafe extern "C" fn(P2pdNode) -> P2pdResult =
            sym!("p2pd_node_stop", unsafe extern "C" fn(P2pdNode) -> P2pdResult);
        let node_status: unsafe extern "C" fn(P2pdNode, *mut P2pdStatus) -> P2pdResult =
            sym!("p2pd_node_status", unsafe extern "C" fn(P2pdNode, *mut P2pdStatus) -> P2pdResult);
        let node_peer_id: unsafe extern "C" fn(P2pdNode, *mut u8, u32, *mut u32) -> P2pdResult =
            sym!("p2pd_node_peer_id", unsafe extern "C" fn(P2pdNode, *mut u8, u32, *mut u32) -> P2pdResult);
        let connect: unsafe extern "C" fn(P2pdNode, *const u8, u32, u32, *mut P2pdStream) -> P2pdResult =
            sym!("p2pd_connect", unsafe extern "C" fn(P2pdNode, *const u8, u32, u32, *mut P2pdStream) -> P2pdResult);
        let accept: unsafe extern "C" fn(P2pdNode, u32, *mut P2pdStream) -> P2pdResult =
            sym!("p2pd_accept", unsafe extern "C" fn(P2pdNode, u32, *mut P2pdStream) -> P2pdResult);
        let add_peer_address: unsafe extern "C" fn(P2pdNode, *const u8, u32, *const u8, u32) -> P2pdResult =
            sym!("p2pd_add_peer_address", unsafe extern "C" fn(P2pdNode, *const u8, u32, *const u8, u32) -> P2pdResult);
        let node_sign: unsafe extern "C" fn(P2pdNode, *const u8, u32, *mut u8, u32, *mut u32) -> P2pdResult =
            sym!("p2pd_node_sign", unsafe extern "C" fn(P2pdNode, *const u8, u32, *mut u8, u32, *mut u32) -> P2pdResult);
        let connected_peers: unsafe extern "C" fn(P2pdNode, *mut u8, u32, *mut u32) -> P2pdResult =
            sym!("p2pd_connected_peers", unsafe extern "C" fn(P2pdNode, *mut u8, u32, *mut u32) -> P2pdResult);
        let last_error: unsafe extern "C" fn(*mut u8, u32, *mut u32) -> P2pdResult =
            sym!("p2pd_last_error", unsafe extern "C" fn(*mut u8, u32, *mut u32) -> P2pdResult);
        // Optional extension (older DLLs lack it): without the callback the
        // Go logs stay on stderr, which GUI builds never see.
        let set_log_callback: Option<unsafe extern "C" fn(*mut std::ffi::c_void)> = lib
            .get::<unsafe extern "C" fn(*mut std::ffi::c_void)>(b"p2pd_set_log_callback\0")
            .ok()
            .map(|s| *s);

        // Register the stream-level symbols for hbb_common::p2p (once).
        let stream_peer_id: unsafe extern "C" fn(P2pdStream, *mut u8, u32, *mut u32) -> P2pdResult =
            sym!("p2pd_stream_peer_id", unsafe extern "C" fn(P2pdStream, *mut u8, u32, *mut u32) -> P2pdResult);
        let stream_remote_addr: unsafe extern "C" fn(P2pdStream, *mut u8, u32, *mut u32) -> P2pdResult =
            sym!("p2pd_stream_remote_addr", unsafe extern "C" fn(P2pdStream, *mut u8, u32, *mut u32) -> P2pdResult);
        let stream_path_kind: unsafe extern "C" fn(P2pdStream, *mut u32) -> P2pdResult =
            sym!("p2pd_stream_path_kind", unsafe extern "C" fn(P2pdStream, *mut u32) -> P2pdResult);
        let stream_read: unsafe extern "C" fn(P2pdStream, *mut u8, u32, u32) -> i64 =
            sym!("p2pd_stream_read", unsafe extern "C" fn(P2pdStream, *mut u8, u32, u32) -> i64);
        let stream_write: unsafe extern "C" fn(P2pdStream, *const u8, u32, u32) -> i64 =
            sym!("p2pd_stream_write", unsafe extern "C" fn(P2pdStream, *const u8, u32, u32) -> i64);
        let stream_close_write: unsafe extern "C" fn(P2pdStream) -> P2pdResult =
            sym!("p2pd_stream_close_write", unsafe extern "C" fn(P2pdStream) -> P2pdResult);
        let stream_close: unsafe extern "C" fn(P2pdStream) -> P2pdResult =
            sym!("p2pd_stream_close", unsafe extern "C" fn(P2pdStream) -> P2pdResult);

        let module = Arc::new(LoadedModule {
            _lib: lib,
            node_start,
            node_stop,
            node_status,
            node_peer_id,
            connect,
            accept,
            add_peer_address,
            node_sign,
            connected_peers,
            last_error,
            set_log_callback,
        });
        // Bridge the Go logs into the rustdesk log before the node starts so
        // startup failures are visible too.
        if let Some(cb) = module.set_log_callback {
            (cb)(go_log_bridge as *const () as *mut std::ffi::c_void);
        }
        let _ = hbb_common::p2p::register_stream_fns(StreamFns {
            peer_id: stream_peer_id,
            remote_addr: stream_remote_addr,
            path_kind: stream_path_kind,
            read: stream_read,
            write: stream_write,
            close_write: stream_close_write,
            close: stream_close,
        });
        Ok(module)
    }
}

// ============================ mock node ============================

/// In-process mock: connect() dials our own PeerId over a byte pipe, sign()
/// uses a real ed25519 key, status is static. No Go module involved.
struct MockNode {
    inner: Mutex<MockInner>,
}

struct MockInner {
    peer_id: String,
    sk: nsign::SecretKey,
    pk: nsign::PublicKey,
    pending: VecDeque<GoP2pStream>,
}

impl MockNode {
    fn new() -> Result<MockNode, String> {
        let _ = hbb_common::sodiumoxide::init();
        let (pk, sk) = nsign::gen_keypair();
        let peer_id = peer_id_from_pubkey(pk.as_ref());
        log::info!("p2p mock node ready, peer id: {peer_id}");
        Ok(MockNode {
            inner: Mutex::new(MockInner {
                peer_id,
                sk,
                pk,
                pending: VecDeque::new(),
            }),
        })
    }

    fn connect(&self, peer: &str) -> hbb_common::ResultType<GoP2pStream> {
        let mut g = self.inner.lock().unwrap();
        if peer == g.peer_id {
            let (a, b) = GoP2pStream::mock_pair(
                format!("{peer}-mock-client"),
                "/mock/direct".into(),
                g.peer_id.clone(),
                "/mock/direct".into(),
            );
            g.pending.push_back(b);
            Ok(a)
        } else {
            bail!("peer {peer} not found on DHT (mock)");
        }
    }

    /// The raw (unframed) path is only used by the desktop IPC bridge; the
    /// mock node cannot pair a raw stream with its framed accept queue, so
    /// refuse loudly instead of half-working.
    fn connect_raw(&self, peer: &str) -> hbb_common::ResultType<hbb_common::p2p::GoIoStream> {
        let _ = peer;
        bail!("mock node does not support the IPC bridge (connect_raw)")
    }

    fn accept(&self, timeout_ms: u32) -> hbb_common::ResultType<Option<GoP2pStream>> {
        let deadline = std::time::Instant::now() + std::time::Duration::from_millis(timeout_ms as u64);
        loop {
            {
                let mut g = self.inner.lock().unwrap();
                if let Some(s) = g.pending.pop_front() {
                    return Ok(Some(s));
                }
            }
            if timeout_ms > 0 && std::time::Instant::now() >= deadline {
                return Ok(None);
            }
            std::thread::sleep(std::time::Duration::from_millis(50));
        }
    }
}

// ============================ base58 / PeerId ============================
// Minimal RFC-4648-less base58 (Bitcoin alphabet) over the identity
// multihash encoding used for PeerIds: `0x00 0x24 | protobuf(ed25519 pk)`.
// Round-trips with `pubkey_from_peer_id` (mirrors `PeerId::from_bytes`).

const B58: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";

fn b58_encode(bytes: &[u8]) -> String {
    let mut digits: Vec<u8> = vec![0];
    for &b in bytes {
        let mut carry = b as u32;
        for d in digits.iter_mut() {
            let v = *d as u32 * 256 + carry;
            *d = (v % 58) as u8;
            carry = v / 58;
        }
        while carry > 0 {
            digits.push((carry % 58) as u8);
            carry /= 58;
        }
    }
    let zeros = bytes.iter().take_while(|&&b| b == 0).count();
    let mut s = String::with_capacity(zeros + digits.len());
    s.extend(std::iter::repeat('1').take(zeros));
    s.extend(digits.iter().rev().map(|&d| B58[d as usize] as char));
    s
}

fn b58_decode(s: &str) -> Option<Vec<u8>> {
    let mut bytes: Vec<u8> = vec![0];
    for c in s.bytes() {
        let idx = B58.iter().position(|&b| b == c)?;
        let mut carry = idx as u32;
        for d in bytes.iter_mut() {
            let v = *d as u32 * 58 + carry;
            *d = (v % 256) as u8;
            carry = v / 256;
        }
        while carry > 0 {
            bytes.push((carry % 256) as u8);
            carry /= 256;
        }
    }
    let zeros = s.bytes().take_while(|&b| b == b'1').count();
    let mut out = vec![0u8; zeros];
    out.extend(bytes.iter().rev());
    Some(out)
}

/// PeerId (base58 identity multihash) of an ed25519 public key.
fn peer_id_from_pubkey(pk: &[u8]) -> String {
    let mut proto = Vec::with_capacity(38);
    proto.extend_from_slice(&[0x08, 0x01, 0x12, 0x20]);
    proto.extend_from_slice(pk);
    let mut mh = Vec::with_capacity(40);
    mh.extend_from_slice(&[0x00, 0x24]);
    mh.extend_from_slice(&proto);
    b58_encode(&mh)
}

/// Extract the ed25519 public key from an inline (identity) PeerId — the
/// controller verifies the server's SignedId against this, no hbbs involved.
pub fn pubkey_from_peer_id(peer: &str) -> Option<nsign::PublicKey> {
    let b = b58_decode(peer)?;
    if b.len() == 38 && b[..6] == [0x00, 0x24, 0x08, 0x01, 0x12, 0x20] {
        let mut pk = [0u8; 32];
        pk.copy_from_slice(&b[6..]);
        Some(nsign::PublicKey(pk))
    } else {
        None
    }
}

/// Whether `s` looks like a PeerId (base58 identity multihash).
pub fn is_peer_id(s: &str) -> bool {
    pubkey_from_peer_id(s).is_some()
}

// ============================ public API ============================

/// Start the p2p module (idempotent). `config_json` is passed verbatim to
/// `p2pd_node_start` (Go side parses: identity dir, mode, listen, bootstrap).
/// Returns whether the node is initialized. Never panics: on any failure the
/// module stays uninitialized and every API reports a clear error.
pub fn init(config_json: &str) -> bool {
    // Double-checked init under a global lock: the first caller loads the
    // module and starts the node; concurrent callers must not start a second
    // node.
    let guard = INIT_LOCK.get_or_init(|| Mutex::new(())).lock();
    let _guard = match guard {
        Ok(g) => g,
        // A panicked holder still serialized everyone up to that point; the
        // module state is checked below anyway.
        Err(poisoned) => poisoned.into_inner(),
    };
    if initialized() {
        return true;
    }
    if RESET_FLAG.load(std::sync::atomic::Ordering::SeqCst) {
        let msg = "identity was reset in this process — restart required";
        log::error!("p2p re-init refused: {msg}");
        if let Ok(mut g) = INIT_ERROR.get_or_init(|| Mutex::new(String::new())).lock() {
            *g = msg.to_string();
        }
        return false;
    }
    let mock = std::env::var("P2PDESK_MOCK").map(|v| v == "1").unwrap_or(false);
    let state = if mock {
        MockNode::new().map(|m| Arc::new(NodeState::Mock(m)))
    } else {
        load_module().and_then(|module| {
            let mut node: P2pdNode = 0;
            let r = unsafe {
                (module.node_start)(config_json.as_ptr(), config_json.len() as u32, &mut node)
            };
            if r.code != P2PD_OK {
                return Err(format!("p2pd_node_start: {}", err_detail(&module, &r)));
            }
            Ok(Arc::new(NodeState::Ffi { node, module }))
        })
    };
    match state {
        Ok(s) => {
            if let Ok(mut g) = INIT_ERROR.get_or_init(|| Mutex::new(String::new())).lock() {
                g.clear();
            }
            let cell = NODE.get_or_init(|| Mutex::new(None));
            if let Ok(mut g) = cell.lock() {
                *g = Some(s);
            }
            true
        }
        Err(e) => {
            log::error!("p2p module init failed: {e}");
            // Keep the reason queryable: the UI otherwise shows a forever
            // "starting p2p…" with no clue why the module is dead.
            if let Ok(mut g) = INIT_ERROR.get_or_init(|| Mutex::new(String::new())).lock() {
                *g = e.to_string();
            }
            false
        }
    }
}

/// Reason the last init attempt failed (empty = never failed / recovered).
pub fn p2p_init_error() -> String {
    INIT_ERROR
        .get()
        .and_then(|m| m.lock().ok())
        .map(|g| g.clone())
        .unwrap_or_default()
}

/// Whether the module is initialized (node started).
pub fn initialized() -> bool {
    NODE.get()
        .and_then(|m| m.lock().ok())
        .map(|g| g.is_some())
        .unwrap_or(false)
}

/// Our own PeerId (None = not initialized).
pub fn self_peer_id() -> Option<String> {
    let state = node_state().ok()?;
    match &*state {
        NodeState::Ffi { node, module } => {
            let mut buf = Vec::with_capacity(64);
            loop {
                let mut written: u32 = 0;
                let r = unsafe {
                    (module.node_peer_id)(*node, buf.as_mut_ptr(), buf.capacity() as u32, &mut written)
                };
                if r.code != P2PD_OK {
                    log::warn!("p2pd_node_peer_id failed: {}", err_detail(module, &r));
                    return None;
                }
                let n = (written as usize).min(buf.capacity()); // clamp
                unsafe {
                    buf.set_len(n);
                }
                if n < buf.capacity() {
                    break;
                }
                buf.reserve(32);
            }
            Some(String::from_utf8_lossy(&buf).into_owned())
        }
        NodeState::Mock(n) => Some(n.inner.lock().unwrap().peer_id.clone()),
    }
}

/// One `p2pd_node_status` call with safe zero defaults. The struct is
/// zero-initialized before the FFI call, so an older module writing only
/// the original five fields leaves the tail-appended ones at 0. The mock
/// returns the historical mock semantics (one connection, DHT ready).
fn node_status_snapshot(state: &NodeState) -> P2pdStatus {
    let zero = || P2pdStatus {
        connected_peers: 0,
        relay_connections: 0,
        direct_connections: 0,
        dht_ready: 0,
        relay_ready: 0,
        routing_table_size: 0,
        relay_reserved: 0,
    };
    match state {
        NodeState::Ffi { node, module } => {
            let mut st = zero();
            let r = unsafe { (module.node_status)(*node, &mut st) };
            if r.code != P2PD_OK {
                log::warn!("p2pd_node_status failed: {}", err_detail(module, &r));
                zero()
            } else {
                st
            }
        }
        NodeState::Mock(_) => P2pdStatus {
            connected_peers: 1,
            dht_ready: 1,
            ..zero()
        },
    }
}

/// Live count of established connections (DHT peers, relays, direct links).
pub fn connected_peers() -> usize {
    match node_state() {
        Ok(s) => node_status_snapshot(&s).connected_peers as usize,
        Err(_) => 0,
    }
}

/// DHT routing table size — the real peer scale of the network, as
/// opposed to the live-connection count above.
pub fn routing_table_size() -> usize {
    match node_state() {
        Ok(s) => node_status_snapshot(&s).routing_table_size as usize,
        Err(_) => 0,
    }
}

/// Whether a relay reservation is active. Autorelay announces circuit
/// addresses only while a reservation is held, so their presence is the
/// signal (go-libp2p v0.49 has no direct reservation query).
pub fn relay_reserved() -> bool {
    match node_state() {
        Ok(s) => node_status_snapshot(&s).relay_reserved != 0,
        Err(_) => false,
    }
}

/// Established relay connections.
pub fn relay_connections() -> usize {
    match node_state() {
        Ok(s) => node_status_snapshot(&s).relay_connections as usize,
        Err(_) => 0,
    }
}

/// Connect-phase snapshot for the controller UI ("finding peer…",
/// "dialing…" etc.). Written by the connect path; a failure resets to Idle
/// so the UI never shows a stale phase. One connection at a time on the
/// controller, so a single global is fine.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ConnectPhase {
    /// No connection attempt in flight (also the state after a failure).
    Idle,
    /// `p2pd_connect` running (find_peer + dial inside the Go module).
    Searching,
    /// Transport connection established; opening the control stream.
    Dialing,
    /// Control stream open; rustdesk handshake/login running.
    Handshake,
}

static CONNECT_PHASE: OnceLock<Mutex<(ConnectPhase, u64)>> = OnceLock::new();

/// Advance the connect-phase snapshot.
pub fn set_connect_phase(phase: ConnectPhase) {
    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0);
    let cell = CONNECT_PHASE.get_or_init(|| Mutex::new((ConnectPhase::Idle, 0)));
    if let Ok(mut guard) = cell.lock() {
        *guard = (phase, now_ms);
    }
}

/// The connect-phase JSON object (`{"phase":...,"phase_since":ms}`),
/// embedded into `status_json` and served to the desktop connecting box.
pub fn connect_phase_json() -> String {
    let cell = CONNECT_PHASE.get_or_init(|| Mutex::new((ConnectPhase::Idle, 0)));
    let (phase, since) = cell.lock().map(|g| *g).unwrap_or((ConnectPhase::Idle, 0));
    let name = match phase {
        ConnectPhase::Idle => "idle",
        ConnectPhase::Searching => "searching",
        ConnectPhase::Dialing => "dialing",
        ConnectPhase::Handshake => "handshake",
    };
    format!(r#"{{"phase":"{name}","phase_since":{since}}}"#)
}

/// UI status snapshot as JSON:
/// `{"peer_id":"...","connected":N,"relay":M,"ready":bool,"init":bool,...,"init_error":"..."}`.
/// Polled by the mobile UI every few seconds — never blocks (atomics only).
pub fn status_json() -> String {
    let (connected, relay, ready, init, dht_peers, relay_reserved) = match node_state() {
        Ok(s) => {
            let st = node_status_snapshot(&s);
            (
                st.connected_peers as usize,
                st.relay_connections as usize,
                st.dht_ready != 0,
                true,
                st.routing_table_size as usize,
                st.relay_reserved != 0,
            )
        }
        Err(_) => (0, 0, false, false, 0, false),
    };
    let peer_id = serde_json::to_string(&self_peer_id().unwrap_or_default())
        .unwrap_or_else(|_| "\"\"".into());
    // Comma-separated PeerIds with live connections — the peer-list dots
    // (green = this node has an active swarm connection to that peer).
    let peers = connected_peer_ids();
    // The init-failure reason, JSON-escaped for embedding (may contain
    // quotes, backslashes, and platform paths).
    let init_error = serde_json::to_string(&p2p_init_error()).unwrap_or_else(|_| "\"\"".into());
    // connect_phase_json returns a complete object for the desktop
    // handler; strip its braces and embed the inner fields so the mobile
    // flat layout stays unchanged.
    let phase = connect_phase_json();
    let phase_fields = &phase[1..phase.len() - 1];
    format!(
        r#"{{"peer_id":{peer_id},"connected":{connected},"relay":{relay},"ready":{ready},"init":{init},"peers":"{peers}","dht_peers":{dht_peers},"relay_reserved":{relay_reserved},"init_error":{init_error},{phase_fields}}}"#
    )
}

/// Live connected PeerIds (comma-separated), for the peer-list status dots.
pub fn connected_peer_ids() -> String {
    match node_state() {
        Ok(s) => match &*s {
            NodeState::Ffi { node, module } => {
                let mut buf = Vec::with_capacity(256);
                loop {
                    let mut written: u32 = 0;
                    let r = unsafe {
                        (module.connected_peers)(
                            *node,
                            buf.as_mut_ptr(),
                            buf.capacity() as u32,
                            &mut written,
                        )
                    };
                    if r.code != P2PD_OK {
                        return String::new();
                    }
                    let n = (written as usize).min(buf.capacity());
                    unsafe {
                        buf.set_len(n);
                    }
                    if n < buf.capacity() {
                        break;
                    }
                    buf.reserve(128);
                }
                String::from_utf8_lossy(&buf).into_owned()
            }
            NodeState::Mock(_) => String::new(),
        },
        Err(_) => String::new(),
    }
}

/// Connect to a PeerId: the Go module runs find_peer + dial + stream open
/// internally (blocking). Runs on a blocking thread so the Tokio runtime
/// never stalls. `timeout_ms = 0` = no timeout.
pub async fn connect(peer: &str, timeout_ms: u32) -> hbb_common::ResultType<GoP2pStream> {
    let state = node_state()?;
    let peer = peer.to_owned();
    tokio::task::spawn_blocking(move || connect_blocking(&state, &peer, timeout_ms))
        .await
        .map_err(|e| anyhow!("p2p connect task failed: {e}"))?
}

fn connect_blocking(
    state: &NodeState,
    peer: &str,
    timeout_ms: u32,
) -> hbb_common::ResultType<GoP2pStream> {
    match state {
        NodeState::Ffi { node, module } => {
            let mut stream: P2pdStream = 0;
            let r = unsafe {
                (module.connect)(*node, peer.as_ptr(), peer.len() as u32, timeout_ms, &mut stream)
            };
            if r.code != P2PD_OK {
                let detail = err_detail(module, &r);
                bail!("p2pd_connect: {detail}");
            }
            GoP2pStream::from_handle(stream).context("p2pd stream metadata")
        }
        NodeState::Mock(n) => n.connect(peer),
    }
}

/// Connect to a PeerId and return the raw unframed byte adapter. The IPC
/// bridge (server process dialing on behalf of the GUI) pumps raw bytes, so
/// framing + encryption stay on the GUI side — the server must not wrap the
/// stream in rustdesk's length-prefixed/secretbox layer.
pub async fn connect_raw(
    peer: &str,
    timeout_ms: u32,
) -> hbb_common::ResultType<hbb_common::p2p::GoIoStream> {
    let state = node_state()?;
    let peer = peer.to_owned();
    tokio::task::spawn_blocking(move || connect_raw_blocking(&state, &peer, timeout_ms))
        .await
        .map_err(|e| anyhow!("p2p connect task failed: {e}"))?
}

fn connect_raw_blocking(
    state: &NodeState,
    peer: &str,
    timeout_ms: u32,
) -> hbb_common::ResultType<hbb_common::p2p::GoIoStream> {
    match state {
        NodeState::Ffi { node, module } => {
            let mut stream: P2pdStream = 0;
            let r = unsafe {
                (module.connect)(*node, peer.as_ptr(), peer.len() as u32, timeout_ms, &mut stream)
            };
            if r.code != P2PD_OK {
                let detail = err_detail(module, &r);
                bail!("p2pd_connect: {detail}");
            }
            // The timeout cell mirrors GoP2pStream::set_send_timeout; the
            // bridge pump uses the default (blocking) writes, so a fresh
            // cell is enough.
            let send_timeout = std::sync::Arc::new(std::sync::atomic::AtomicU64::new(0));
            Ok(hbb_common::p2p::GoIoStream::from_handle(
                stream,
                send_timeout,
            ))
        }
        NodeState::Mock(n) => n.connect_raw(peer),
    }
}

/// Accept one inbound control stream (blocking). `timeout_ms = 0` = block
/// forever. Returns `None` on timeout.
pub async fn accept(timeout_ms: u32) -> hbb_common::ResultType<Option<GoP2pStream>> {
    let state = node_state()?;
    tokio::task::spawn_blocking(move || accept_blocking(&state, timeout_ms))
        .await
        .map_err(|e| anyhow!("p2p accept task failed: {e}"))?
}

fn accept_blocking(
    state: &NodeState,
    timeout_ms: u32,
) -> hbb_common::ResultType<Option<GoP2pStream>> {
    match state {
        NodeState::Ffi { node, module } => {
            let mut stream: P2pdStream = 0;
            let r = unsafe { (module.accept)(*node, timeout_ms, &mut stream) };
            if r.code != P2PD_OK {
                if r.code == P2PD_ERR_TIMEOUT {
                    return Ok(None);
                }
                let detail = err_detail(module, &r);
                bail!("p2pd_accept: {detail}");
            }
            let s = GoP2pStream::from_handle(stream).context("p2pd stream metadata")?;
            Ok(Some(s))
        }
        NodeState::Mock(n) => n.accept(timeout_ms),
    }
}

/// Sign `msg` with the node's identity key (ed25519). Returns the raw 64-byte
/// signature — callers assemble `signature || message` themselves (the
/// rustdesk SignedId format). `None` = module not initialized / sign failed.
pub fn sign(msg: &[u8]) -> Option<Vec<u8>> {
    let state = node_state().ok()?;
    match &*state {
        NodeState::Ffi { node, module } => {
            let mut sig = [0u8; 64];
            let mut sig_len: u32 = 0;
            let r = unsafe {
                (module.node_sign)(
                    *node,
                    msg.as_ptr(),
                    msg.len() as u32,
                    sig.as_mut_ptr(),
                    sig.len() as u32,
                    &mut sig_len,
                )
            };
            if r.code != P2PD_OK {
                log::warn!("p2pd_node_sign failed: {}", err_detail(module, &r));
                None
            } else {
                let n = (sig_len as usize).min(sig.len()); // clamp
                Some(sig[..n].to_vec())
            }
        }
        NodeState::Mock(n) => {
            let g = n.inner.lock().unwrap();
            // sodiumoxide sign() returns signature||message; take the raw
            // 64-byte signature only (ABI contract).
            let signed = nsign::sign(msg, &g.sk);
            Some(signed[..64].to_vec())
        }
    }
}

/// Seed `peer`'s address (multiaddr string, e.g.
/// `/ip4/192.168.3.10/tcp/28431`) into the Go module's peerstore so a click
/// connects directly instead of waiting on a DHT lookup. Idempotent.
pub async fn add_peer_address(peer: &str, multiaddr: &str) -> hbb_common::ResultType<()> {
    let state = node_state()?;
    let peer = peer.to_owned();
    let multiaddr = multiaddr.to_owned();
    tokio::task::spawn_blocking(move || match &*state {
        NodeState::Ffi { node, module } => {
            let r = unsafe {
                (module.add_peer_address)(
                    *node,
                    peer.as_ptr(),
                    peer.len() as u32,
                    multiaddr.as_ptr(),
                    multiaddr.len() as u32,
                )
            };
            if r.code != P2PD_OK {
                let detail = err_detail(module, &r);
                bail!("p2pd_add_peer_address: {detail}");
            }
            Ok(())
        }
        NodeState::Mock(_) => Ok(()),
    })
    .await
    .map_err(|e| anyhow!("p2p add_peer_address task failed: {e}"))?
}

/// Regenerate the identity. Mock: a brand-new keypair + PeerId (persisted by
/// the caller). Real module: stop the node, delete the identity file, leave
/// the next start to mint a fresh key — returns the current PeerId (the new
/// one appears after the caller restarts the process, like the rustdesk
/// reset flow).
pub fn reset_identity(identity_dir: &std::path::Path) -> Result<String, String> {
    let current = self_peer_id().ok_or("p2p module not initialized")?;
    let state = node_state().map_err(|e| e.to_string())?;
    match &*state {
        NodeState::Mock(n) => {
            let _ = hbb_common::sodiumoxide::init();
            let (pk, sk) = nsign::gen_keypair();
            let peer_id = peer_id_from_pubkey(pk.as_ref());
            let mut g = n.inner.lock().unwrap();
            g.pk = pk;
            g.sk = sk;
            g.peer_id = peer_id.clone();
            Ok(peer_id)
        }
        NodeState::Ffi { node, module } => {
            // Detach the Go→Rust log callback before the node stops and the
            // module unloads — afterwards the DLL (and its Go runtime) is
            // gone, and the callback must not be reachable from it.
            if let Some(cb) = module.set_log_callback {
                unsafe { (cb)(std::ptr::null_mut()) };
            }
            let r = unsafe { (module.node_stop)(*node) };
            if r.code != P2PD_OK {
                return Err(format!("p2pd_node_stop: {}", err_detail(module, &r)));
            }
            let key_file = identity_dir.join("identity.key");
            match std::fs::remove_file(&key_file) {
                Ok(()) => log::info!("p2p identity file removed: {}", key_file.display()),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                Err(e) => return Err(format!("remove identity file: {e}")),
            }
            // The node is stopped, its handle is stale, and the DLL is about
            // to unload — the registered stream function pointers become
            // dangling. Mark the module uninitialized AND refuse re-init in
            // this process ; the rustdesk reset flow restarts
            // the process anyway.
            if let Some(m) = NODE.get() {
                if let Ok(mut g) = m.lock() {
                    *g = None;
                }
            }
            RESET_FLAG.store(true, std::sync::atomic::Ordering::SeqCst);
            // The node is gone and re-init is refused, so the status line
            // would otherwise hang on "starting p2p…" with no explanation
            // until the restart actually happens. Tell the UI what to do.
            if let Ok(mut g) = INIT_ERROR.get_or_init(|| Mutex::new(String::new())).lock() {
                *g = "identity reset — restart the app to activate the new PeerId".to_string();
            }
            Ok(current)
        }
    }
}

/// The p2p listen port LAN discovery seeds multiaddrs with. `P2PDESK_PORT`
/// overrides, then the p2p-listen-port option, else the 28431 default —
/// must match the Go node's listen config.
pub fn lan_listen_port() -> u16 {
    std::env::var("P2PDESK_PORT")
        .ok()
        .and_then(|s| s.parse().ok())
        .or_else(|| {
            Config::get_option("p2p-listen-port")
                .parse::<u16>()
                .ok()
                .filter(|&p| p != 0)
        })
        .unwrap_or(28431)
}

/// The remote path kind of a stream as a short label (diagnostics).
#[cfg(test)]
mod tests {
    use super::*;

    /// Mock node end-to-end: init → self peer id → connect(self) → accept
    /// hands out the peer end → framed echo both ways.
    #[test]
    fn mock_node_roundtrip() {
        std::env::set_var("P2PDESK_MOCK", "1");
        let rt = tokio::runtime::Runtime::new().unwrap();
        rt.block_on(async {
            let cfg = crate::p2pdesk::node_config_json(&std::env::temp_dir(), "server");
            assert!(init(&cfg));
            let me = self_peer_id().unwrap();
            assert!(is_peer_id(&me), "mock peer id must be a real PeerId: {me}");

            let client = connect(&me, 5000);
            let server = accept(1000);
            let (c, s) = tokio::join!(client, server);
            let mut c = c.expect("connect to own mock peer id");
            let mut s = s.expect("accept").expect("accept must not time out");
            assert_eq!(s.peer_id(), me.as_str());

            // framed echo, both directions
            c.send_raw(b"ping".to_vec()).await.unwrap();
            let data = s.next().await.unwrap().unwrap();
            assert_eq!(&data[..], b"ping");
            s.send_raw(b"pong".to_vec()).await.unwrap();
            let data = c.next().await.unwrap().unwrap();
            assert_eq!(&data[..], b"pong");

            // EOF: dropping the client end surfaces as a read error server-side
            drop(c);
            assert!(s.next().await.unwrap().is_err());
        });
    }

    /// The status JSON must carry the readiness fields the UIs render,
    /// with safe defaults under the mock.
    #[test]
    fn mock_status_json_fields() {
        std::env::set_var("P2PDESK_MOCK", "1");
        let rt = tokio::runtime::Runtime::new().unwrap();
        rt.block_on(async {
            let cfg = crate::p2pdesk::node_config_json(&std::env::temp_dir(), "server");
            let _ = init(&cfg); // may already be initialized from another test
            let raw = status_json();
            let j: serde_json::Value = serde_json::from_str(&raw).unwrap();
            for key in ["connected", "relay", "ready", "init", "peers", "dht_peers", "relay_reserved", "init_error", "phase", "phase_since"] {
                assert!(j.get(key).is_some(), "status_json missing key {key}: {j}");
            }
            assert_eq!(j["relay_reserved"], false);
            assert_eq!(j["dht_peers"], 0);
        });
    }

    #[test]
    fn mock_connect_unknown_peer_fails() {
        std::env::set_var("P2PDESK_MOCK", "1");
        let rt = tokio::runtime::Runtime::new().unwrap();
        rt.block_on(async {
            let cfg = crate::p2pdesk::node_config_json(&std::env::temp_dir(), "client");
            if !init(&cfg) {
                return; // already initialized as server in the other test
            }
            let err = connect("12D3KooWAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 500)
                .await
                .err()
                .expect("connect to unknown peer must fail");
            assert!(err.to_string().contains("not found"), "{err}");
        });
    }

    #[test]
    fn mock_sign_and_pubkey_roundtrip() {
        std::env::set_var("P2PDESK_MOCK", "1");
        let rt = tokio::runtime::Runtime::new().unwrap();
        rt.block_on(async {
            let cfg = crate::p2pdesk::node_config_json(&std::env::temp_dir(), "server");
            if !init(&cfg) {
                return;
            }
            let me = self_peer_id().unwrap();
            let pk = pubkey_from_peer_id(&me).expect("own peer id must carry the key");
            let msg = b"signed identity";
            let sig = sign(msg).expect("mock sign");
            assert_eq!(sig.len(), 64);
            let sig_bytes: [u8; 64] = sig.as_slice().try_into().expect("64-byte signature");
            let sig_obj = hbb_common::sodiumoxide::crypto::sign::Signature::from_bytes(&sig_bytes)
                .expect("valid signature bytes");
            assert!(hbb_common::sodiumoxide::crypto::sign::verify_detached(
                &sig_obj,
                msg,
                &pk,
            ));
        });
    }
}
pub fn path_kind_label(kind: u32) -> &'static str {
    match kind {
        PATH_DIRECT_QUIC => "direct-quic",
        PATH_DIRECT_TCP => "direct-tcp",
        hbb_common::p2p::PATH_DCUTR => "dcutr",
        hbb_common::p2p::PATH_CUSTOM_QUIC => "custom-quic",
        hbb_common::p2p::PATH_RELAY => "relay",
        _ => "unknown",
    }
}
