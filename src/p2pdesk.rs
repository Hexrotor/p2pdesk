//! p2pdesk: go-libp2p module wiring into rustdesk.
//!
//! Server side: start the node (listen + DHT join) and feed inbound control
//! streams into the existing `create_tcp_connection` handshake. Client side:
//! connect by PeerId goes through `crate::p2pffi::connect` (find_peer + dial
//! happen inside the Go module). All network ownership (host, DHT,
//! peerstore, relay/DCUtR) lives in the c-shared module — see
//! `src/p2pffi.rs`.

use crate::server::{self, ServerPtr};
#[cfg(target_os = "android")]
use hbb_common::config::{self, option2bool, Config};
#[cfg(not(target_os = "android"))]
use hbb_common::config::{option2bool, Config};
use hbb_common::{log, Stream};
use std::net::SocketAddr;

/// Our own PeerId (empty when the p2p module is not initialized).
pub fn self_peer_id() -> Option<String> {
    crate::p2pffi::self_peer_id()
}

/// Directory holding the persistent p2p identity.
///
/// Android: the app-private dir (`APP_DIR`, e.g. `.../app_flutter`) — NOT
/// `get_home()`. Flutter's `mainSetHomeDir` sets the Android home to the
/// external-storage root (`/storage/emulated/0`); under scoped storage
/// (API 30+) that root is read-only for apps, so the identity write fails
/// with `Operation not permitted (os error 1)` and the node silently runs a
/// fresh random keypair on every start .
/// Desktop Windows: a machine-wide location (ProgramData). The PeerId is
/// the DEVICE identity, shared by the service node and a portable GUI
/// alike, so it must not fork per process account. Per-account config
/// (passwords, options, peers) stays where rustdesk puts it; only the
/// identity is machine-scoped. No legacy migration — the fork has not
/// shipped, so older per-account identity.key files are simply orphaned.
///
/// Note on key confidentiality: any local user must be able to read the
/// key (a portable GUI under any account loads it), so per-user
/// read-restriction is impossible by design. Session security does not
/// rest on this key — the connection password and the per-session
/// secretbox key do; a stolen identity key only lets someone impersonate
/// this device's PeerId toward other peers.
/// Desktop non-Windows: the rustdesk config directory (per-user; a
/// machine-wide layout is not wired up there yet).
pub fn p2p_identity_dir() -> std::path::PathBuf {
    #[cfg(target_os = "android")]
    {
        std::path::PathBuf::from(config::APP_DIR.read().unwrap().as_str())
    }
    #[cfg(target_os = "windows")]
    {
        // Debugging/acceptance escape hatch: point at an explicit directory
        // (same-machine dual-instance tests must not share the identity).
        if let Ok(dir) = std::env::var("P2PDESK_IDENTITY_DIR") {
            if !dir.is_empty() {
                return dir.into();
            }
        }
        let base = std::env::var("ProgramData")
            .unwrap_or_else(|_| "C:\\ProgramData".to_owned());
        std::path::PathBuf::from(base).join("P2PDesk").join("config")
    }
    #[cfg(not(any(target_os = "android", target_os = "windows")))]
    {
        Config::path("")
    }
}

/// Regenerate the p2p identity — a brand-new keypair and PeerId. The running
/// node keeps the old keypair until the process restarts, so callers must
/// surface the new PeerId and ask the user to restart.
pub fn reset_identity() -> Result<String, String> {
    crate::p2pffi::reset_identity(&p2p_identity_dir())
}

/// Config JSON handed to `p2pd_node_start` (the Go side parses it).
pub fn node_config_json(identity_dir: &std::path::Path, mode: &str) -> String {
    let mut cfg = serde_json::json!({
        "identity_dir": identity_dir.to_string_lossy(),
        "mode": mode,
    });
    // Optional override: an operator-provided public address (port-forwarded
    // router etc.). Private addresses are rejected by the Go side.
    if let Ok(addr) = std::env::var("P2PDESK_EXTERNAL_ADDR") {
        if !addr.is_empty() {
            cfg["external_addr"] = serde_json::json!(addr);
        }
    }
    // Listen-port selection: the p2p-listen-port option overrides the 28431
    // default (LAN discovery seeds multiaddrs with it). Applies to any node
    // started from this config — same-machine dual nodes must pick distinct
    // ports, mirroring the P2PDESK_PORT env semantics.
    let port = if let Ok(v) = std::env::var("P2PDESK_PORT") {
        if let Ok(p) = v.parse::<i64>() {
            p
        } else {
            0
        }
    } else {
        Config::get_option("p2p-listen-port")
            .parse::<i64>()
            .ok()
            .filter(|&p| (1..=65535).contains(&p))
            .unwrap_or(0)
    };
    if port != 0 {
        cfg["port"] = serde_json::json!(port);
    }
    // Self-hosted network (custom DHT): the UI settings are the persistent
    // truth, the env vars are a debugging/acceptance fallback that leaves
    // config2.toml untouched. Both empty = no fields = the public DHT,
    // exactly the pre-custom-network behavior.
    let peers = {
        let from_cfg = Config::get_option("custom-bootstrap-peers");
        if from_cfg.trim().is_empty() {
            std::env::var("P2PDESK_BOOTSTRAP_PEERS").unwrap_or_default()
        } else {
            from_cfg
        }
    };
    let mut protocol = {
        let from_cfg = Config::get_option("custom-bootstrap-protocol");
        if from_cfg.trim().is_empty() {
            std::env::var("P2PDESK_PROTOCOL_PREFIX").unwrap_or_default()
        } else {
            from_cfg
        }
    };
    // The custom network engages only when at least one peer survives
    // parsing. An all-whitespace peer list must fall back to the public
    // DHT — never the root-node empty list, which would strand the device
    // on an empty private DHT. A protocol without peers is dropped too:
    // it would still dial the public bootstrap list while being invisible
    // to it, leaking the node's identity for nothing.
    let list: Vec<String> = peers
        .split(|c: char| c.is_whitespace() || c == ',' || c == ';')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
        .collect();
    if !list.is_empty() {
        cfg["bootstrap_peers"] = serde_json::json!(list);
        // Convenience default: bootstrap peers without a protocol get the
        // bootstrapd default, so filling one field is enough to join.
        if protocol.trim().is_empty() {
            protocol = "/p2pdesk".to_owned();
        }
        cfg["protocol_prefix"] = serde_json::json!(protocol.trim());
    }
    serde_json::to_string(&cfg).unwrap_or_else(|_| "{}".to_owned())
}

/// Server mode: init the p2p module, then loop inbound streams into the
/// rustdesk connection layer. Idempotent via `p2pffi::init`.
pub fn start_incoming(server: ServerPtr) {
    let identity_dir = p2p_identity_dir();
    let config = node_config_json(&identity_dir, "server");
    if !crate::p2pffi::init(&config) {
        log::error!("p2p module unavailable — inbound p2p connections disabled");
        return;
    }
    if let Some(id) = crate::p2pffi::self_peer_id() {
        log::info!("p2p node ready, peer id: {id}");
    }
    // p2p streams carry no socket address; keep a placeholder for audit/log
    // display (consumers only log it).
    let placeholder =
        SocketAddr::new(std::net::IpAddr::V4(std::net::Ipv4Addr::UNSPECIFIED), 0);
    tokio::spawn(async move {
        loop {
            match crate::p2pffi::accept(0).await {
                Ok(Some(stream)) => {
                    let peer = stream.peer_id().to_owned();
                    let path = crate::p2pffi::path_kind_label(stream.path_kind());
                    log::info!("p2p inbound rustdesk connection from {peer} ({path})");
                    // The tray "stop service" toggle must gate the p2p channel
                    // too: with no hbbs, nothing else keeps new sessions out
                    // while the service is stopped.
                    if option2bool("stop-service", &Config::get_option("stop-service")) {
                        log::info!(
                            "p2p inbound rustdesk connection from {peer} rejected: service stopped"
                        );
                        continue;
                    }
                    let server = server.clone();
                    tokio::spawn(async move {
                        if let Err(e) = server::create_tcp_connection(
                            server,
                            Stream::from_p2p(stream),
                            placeholder,
                            true,
                            Default::default(),
                        )
                        .await
                        {
                            log::error!("p2p connection from {peer} failed: {e}");
                        }
                    });
                }
                Ok(None) => {
                    // timeout — accept(0) never times out; defensive.
                    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
                }
                Err(e) => {
                    log::error!("p2p accept failed: {e}");
                    // Module gone (e.g. identity reset): stop the loop
                    // instead of spinning forever .
                    if !crate::p2pffi::initialized() {
                        log::warn!("p2p module no longer initialized — stopping inbound loop");
                        break;
                    }
                    tokio::time::sleep(std::time::Duration::from_secs(1)).await;
                }
            }
        }
    });
}
