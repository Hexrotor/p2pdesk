//! p2pdesk headless connect verification tool: controller → target PeerId
//! over the go-libp2p c-shared module, full handshake + password login.
//!
//! Usage: `p2pdesk-connect <peer_id> [password]`
//!
//! Link check (no UI): stream established → SignedId verified (target
//! PeerId public key) → PublicKey exchange (secretbox symmetric key) → Hash
//! challenge → LoginRequest → login OK → counts video frames received in 5
//! seconds (screen data path live).
//! bench: stage timers (warmup/connect/handshake/login/first frame), one
//! `BENCH_RESULT {json}` line at the end for tooling.
//!
//! Modes: `P2PDESK_MOCK=1` runs against the in-memory mock (no DLL, for a
//! same-machine dual-instance loop); otherwise the `p2pdesk_net.dll` /
//! `libp2pdesk_net.so` next to the executable is loaded (`P2PDESK_NET_LIB`
//! overrides the path).

use hbb_common::{
    anyhow::{anyhow, Result},
    config::Config,
    log,
    message_proto::{self, message::Union, LoginRequest, PublicKey},
    protobuf::Message as _,
    sha2::{Digest, Sha256},
    stream::Stream,
};
use std::time::Instant;

fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().collect();
    if args.len() < 2 {
        // Mock self-check: print our own (mock) PeerId so a one-process
        // loop can connect to itself (`p2pdesk-connect <that id>`).
        if std::env::var("P2PDESK_MOCK").map(|v| v == "1").unwrap_or(false) {
            let config = librustdesk::p2pdesk::node_config_json(
                &std::env::temp_dir(),
                "client",
            );
            if librustdesk::p2pffi::init(&config) {
                if let Some(id) = librustdesk::p2pffi::self_peer_id() {
                    println!("{id}");
                    return Ok(());
                }
            }
        }
        eprintln!("usage: p2pdesk-connect <peer_id> [password]");
        std::process::exit(2);
    }
    let filter = std::env::var("RUST_LOG").unwrap_or_else(|_| "info".to_owned());
    let _ = hbb_common::env_logger::Builder::new()
        .parse_filters(&filter)
        .try_init();
    let peer = args[1].clone();
    if !librustdesk::p2pffi::is_peer_id(&peer) {
        return Err(anyhow!("bad peer id: {peer}"));
    }
    let password = args.get(2).cloned().unwrap_or_default();

    let rt = tokio::runtime::Runtime::new()?;
    rt.block_on(async { run(&peer, &password).await })
}

async fn run(peer: &str, password: &str) -> Result<()> {
    // Same-machine dual-instance linking needs a separate identity directory.
    let identity_dir = std::env::var("P2PDESK_IDENTITY_DIR")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|_| Config::get_home());
    // bench: stage timers (ms, from process start). Printed as one
    // `BENCH_RESULT` JSON line at the end for tooling.
    let t0 = Instant::now();
    let mut bench = BenchTimers::new();

    // Start the p2p module (mock or the real Go DLL). The Go module owns
    // find_peer + dial + stream open.
    let config = librustdesk::p2pdesk::node_config_json(&identity_dir, "client");
    if !librustdesk::p2pffi::init(&config) {
        return Err(anyhow!("p2p module unavailable"));
    }
    let warmup_deadline = Instant::now() + std::time::Duration::from_secs(15);
    while librustdesk::p2pffi::connected_peers() == 0 && Instant::now() < warmup_deadline {
        tokio::time::sleep(std::time::Duration::from_millis(300)).await;
    }
    bench.mark("warmup", t0);
    let stream = librustdesk::p2pffi::connect(peer, 30_000).await?;
    bench.mark("connect", t0);
    log::info!(
        "[p2pdesk] stream established in {:?} (warmup {:?})",
        t0.elapsed(),
        bench.elapsed("warmup")
    );
    let mut stream = Stream::from_p2p(stream);

    // 1. SignedId from the target (create_tcp_connection sends it first)
    let pk = librustdesk::p2pffi::pubkey_from_peer_id(peer)
        .ok_or_else(|| anyhow!("peer id is not an inline ed25519 key"))?;
    let msg = next_message(&mut stream).await?;
    let si = match msg.union {
        Some(Union::SignedId(si)) => si,
        other => return Err(anyhow!("expected SignedId, got {other:?}")),
    };
    let (id, their_pk_b) = librustdesk::common::decode_id_pk(&si.id, &pk)?;
    if id != peer {
        return Err(anyhow!("SignedId id mismatch: {id} != {peer}"));
    }
    log::info!("[p2pdesk] SignedId verified for {id}");

    // 2. PublicKey exchange → secretbox symmetric key
    let (asymmetric_value, symmetric_value, key) = librustdesk::common::create_symmetric_key_msg(their_pk_b);
    let mut msg_out = message_proto::Message::new();
    msg_out.set_public_key(PublicKey {
        asymmetric_value,
        symmetric_value,
        ..Default::default()
    });
    stream.send(&msg_out).await?;
    stream.set_key(key);
    bench.mark("handshake", t0);
    log::info!("[p2pdesk] secure channel established in {:?}", t0.elapsed());

    // 3. Hash challenge (salt + challenge)
    let msg = next_message(&mut stream).await?;
    let hash = match msg.union {
        Some(Union::Hash(h)) => h,
        other => return Err(anyhow!("expected Hash, got {other:?}")),
    };
    log::info!("[p2pdesk] challenge received");

    // 4. LoginRequest: sha256(sha256(password + salt) + challenge)
    let mut h = Sha256::new();
    h.update(password.as_bytes());
    h.update(&hash.salt);
    let h1 = h.finalize();
    let mut h2 = Sha256::new();
    h2.update(&h1[..]);
    h2.update(&hash.challenge);
    let h2 = h2.finalize()[..].to_vec();

    let mut lr = LoginRequest::new();
    lr.password = h2.into(); // bytes password = sha256(sha256(pw+salt)+challenge)
    lr.my_id = peer.to_string();
    lr.username = peer.to_string(); // server checks this against its own id / peer id
    lr.version = librustdesk::VERSION.to_string();
    let mut msg_out = message_proto::Message::new();
    msg_out.set_login_request(lr);
    stream.send(&msg_out).await?;
    log::info!("[p2pdesk] login request sent in {:?}", t0.elapsed());

    // 5. Login result (LoginResponse: PeerInfo = success, error = failure).
    // The target may send TestDelay etc. first — skip until LoginResponse.
    loop {
        let msg = next_message(&mut stream).await?;
        match msg.union {
            Some(Union::LoginResponse(r)) => {
                match r.union {
                    Some(hbb_common::message_proto::login_response::Union::PeerInfo(_)) => {
                        bench.mark("login", t0);
                        log::info!("[p2pdesk] LOGIN OK in {:?}", t0.elapsed());
                    }
                    Some(hbb_common::message_proto::login_response::Union::Error(e)) => {
                        return Err(anyhow!("login rejected: {e}"));
                    }
                    other => return Err(anyhow!("unexpected login response: {other:?}")),
                }
                break;
            }
            _ => continue,
        }
    }

    // 6. Session data-channel check: count messages for 5 s (video frames = screen stream live)
    let mut counts: std::collections::HashMap<&str, u32> = Default::default();
    let mut first_frame: Option<std::time::Duration> = None;
    let deadline = Instant::now() + std::time::Duration::from_secs(5);
    while Instant::now() < deadline {
        match stream.next_timeout(1000).await {
            Some(Ok(bytes)) => {
                if let Ok(m) = message_proto::Message::parse_from_bytes(&bytes) {
                    let name = match m.union {
                        Some(Union::VideoFrame(_)) => "video_frame",
                        Some(Union::KeyEvent(_)) => "key_event",
                        Some(Union::TestDelay(_)) => "test_delay",
                        Some(Union::CursorData(_)) => "cursor_data",
                        Some(Union::Clipboard(_)) => "clipboard",
                        Some(Union::Misc(_)) => "misc",
                        _ => "other",
                    };
                    if name == "video_frame" && first_frame.is_none() {
                        first_frame = Some(t0.elapsed());
                    }
                    *counts.entry(name).or_default() += 1;
                }
            }
            Some(Err(e)) => return Err(anyhow!("read error: {e}")),
            None => {}
        }
    }
    log::info!("[p2pdesk] data channel stats (5s): {counts:?}");
    if counts.get("video_frame").copied().unwrap_or(0) == 0 {
        log::warn!("[p2pdesk] no video frames observed — screen stream may not have started");
    }

    // One machine-readable line for the bench tooling.
    println!(
        "BENCH_RESULT {{\"warmup_ms\":{},\"connect_ms\":{},\
\"handshake_ms\":{},\"login_ms\":{},\"addrs\":0,\"first_frame_after_login_ms\":{}}}",
        bench.elapsed("warmup").as_millis(),
        bench.elapsed("connect").as_millis(),
        bench.elapsed("handshake").as_millis(),
        bench.elapsed("login").as_millis(),
        first_frame
            .map(|d| d.saturating_sub(bench.elapsed("login")).as_millis())
            .unwrap_or(0),
    );
    Ok(())
}

/// Named stage timers (cumulative ms from a shared start).
struct BenchTimers {
    marks: std::collections::HashMap<&'static str, Instant>,
}

impl BenchTimers {
    fn new() -> Self {
        BenchTimers {
            marks: Default::default(),
        }
    }

    fn mark(&mut self, name: &'static str, start: Instant) {
        self.marks.insert(name, start);
    }

    fn elapsed(&self, name: &'static str) -> std::time::Duration {
        self.marks.get(name).map(|m| m.elapsed()).unwrap_or_default()
    }
}

async fn next_message(stream: &mut Stream) -> Result<message_proto::Message> {
    let bytes = stream
        .next_timeout(10_000)
        .await
        .ok_or_else(|| anyhow!("timeout waiting for message"))??;
    message_proto::Message::parse_from_bytes(&bytes)
        .map_err(|e| anyhow!("parse message: {e}"))
}
