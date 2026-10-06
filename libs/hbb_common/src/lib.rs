pub mod compress;
pub mod platform;
pub mod protos;
pub use bytes;
use config::Config;
pub use futures;
pub use protobuf;
pub use protos::message as message_proto;
pub use protos::rendezvous as rendezvous_proto;
use std::{
    fs::File,
    io::{self, BufRead},
    net::{IpAddr, Ipv4Addr, SocketAddr, SocketAddrV4},
    path::Path,
    time::{self, SystemTime, UNIX_EPOCH},
};
pub use tokio;
pub use tokio_util;
pub mod proxy;
pub mod socket_client;
pub mod tcp;
pub mod udp;
pub use env_logger;
pub use log;
pub mod bytes_codec;
pub use anyhow::{self, bail};
pub use futures_util;
pub mod config;
pub mod fs;
pub mod mem;
pub use lazy_static;
#[cfg(not(any(target_os = "android", target_os = "ios")))]
pub use mac_address;
pub use rand;
pub use regex;
pub use sodiumoxide;
pub use tokio_socks;
pub use tokio_socks::IntoTargetAddr;
pub use tokio_socks::TargetAddr;
pub mod password_security;
pub use chrono;
pub use directories_next;
pub use libc;
pub mod keyboard;
pub use base64;
#[cfg(not(any(target_os = "android", target_os = "ios")))]
pub use dlopen;
#[cfg(not(any(target_os = "android", target_os = "ios")))]
pub use machine_uid;
pub use serde_derive;
pub use serde_json;
pub use sha2;
pub use sysinfo;
pub use thiserror;
pub use toml;
pub use uuid;
pub mod fingerprint;
pub use flexi_logger;
pub mod stream;
pub mod websocket;
#[cfg(feature = "webrtc")]
pub mod webrtc;
pub mod p2p;
#[cfg(any(target_os = "android", target_os = "ios"))]
pub use rustls_platform_verifier;
pub use stream::Stream;
pub use whoami;
pub mod tls;
pub mod verifier;
pub use async_recursion;
#[cfg(target_os = "linux")]
pub use users;
pub use libloading;
#[cfg(target_os = "linux")]
pub use x11;

pub type SessionID = uuid::Uuid;

#[inline]
pub async fn sleep(sec: f32) {
    tokio::time::sleep(time::Duration::from_secs_f32(sec)).await;
}

#[macro_export]
macro_rules! allow_err {
    ($e:expr) => {
        if let Err(err) = $e {
            log::debug!(
                "{:?}, {}:{}:{}:{}",
                err,
                module_path!(),
                file!(),
                line!(),
                column!()
            );
        } else {
        }
    };

    ($e:expr, $($arg:tt)*) => {
        if let Err(err) = $e {
            log::debug!(
                "{:?}, {}, {}:{}:{}:{}",
                err,
                format_args!($($arg)*),
                module_path!(),
                file!(),
                line!(),
                column!()
            );
        } else {
        }
    };
}

#[inline]
pub fn timeout<T: std::future::Future>(ms: u64, future: T) -> tokio::time::Timeout<T> {
    tokio::time::timeout(std::time::Duration::from_millis(ms), future)
}

pub type ResultType<F, E = anyhow::Error> = anyhow::Result<F, E>;

/// Certain router and firewalls scan the packet and if they
/// find an IP address belonging to their pool that they use to do the NAT mapping/translation, so here we mangle the ip address

pub struct AddrMangle();

#[inline]
pub fn try_into_v4(addr: SocketAddr) -> SocketAddr {
    match addr {
        SocketAddr::V6(v6) if !addr.ip().is_loopback() => {
            if let Some(v4) = v6.ip().to_ipv4() {
                SocketAddr::new(IpAddr::V4(v4), addr.port())
            } else {
                addr
            }
        }
        _ => addr,
    }
}

impl AddrMangle {
    pub fn encode(addr: SocketAddr) -> Vec<u8> {
        // not work with [:1]:<port>
        let addr = try_into_v4(addr);
        match addr {
            SocketAddr::V4(addr_v4) => {
                let tm = (SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap_or(std::time::Duration::ZERO)
                    .as_micros() as u32) as u128;
                let ip = u32::from_le_bytes(addr_v4.ip().octets()) as u128;
                let port = addr.port() as u128;
                let v = ((ip + tm) << 49) | (tm << 17) | (port + (tm & 0xFFFF));
                let bytes = v.to_le_bytes();
                let mut n_padding = 0;
                for i in bytes.iter().rev() {
                    if i == &0u8 {
                        n_padding += 1;
                    } else {
                        break;
                    }
                }
                bytes[..(16 - n_padding)].to_vec()
            }
            SocketAddr::V6(addr_v6) => {
                let mut x = addr_v6.ip().octets().to_vec();
                let port: [u8; 2] = addr_v6.port().to_le_bytes();
                x.push(port[0]);
                x.push(port[1]);
                x
            }
        }
    }

    pub fn decode(bytes: &[u8]) -> SocketAddr {
        use std::convert::TryInto;

        if bytes.len() > 16 {
            if bytes.len() != 18 {
                return Config::get_any_listen_addr(false);
            }
            let tmp: [u8; 2] = bytes[16..].try_into().unwrap_or_default();
            let port = u16::from_le_bytes(tmp);
            let tmp: [u8; 16] = bytes[..16].try_into().unwrap_or_default();
            let ip = std::net::Ipv6Addr::from(tmp);
            return SocketAddr::new(IpAddr::V6(ip), port);
        }
        let mut padded = [0u8; 16];
        padded[..bytes.len()].copy_from_slice(bytes);
        let number = u128::from_le_bytes(padded);
        let tm = (number >> 17) & (u32::max_value() as u128);
        let ip = (((number >> 49) - tm) as u32).to_le_bytes();
        let port = (number & 0xFFFFFF) - (tm & 0xFFFF);
        SocketAddr::V4(SocketAddrV4::new(
            Ipv4Addr::new(ip[0], ip[1], ip[2], ip[3]),
            port as u16,
        ))
    }
}

pub fn get_version_from_url(url: &str) -> String {
    let n = url.chars().count();
    let a = url.chars().rev().position(|x| x == '-');
    if let Some(a) = a {
        let b = url.chars().rev().position(|x| x == '.');
        if let Some(b) = b {
            if a > b {
                if url
                    .chars()
                    .skip(n - b)
                    .collect::<String>()
                    .parse::<i32>()
                    .is_ok()
                {
                    return url.chars().skip(n - a).collect();
                } else {
                    return url.chars().skip(n - a).take(a - b - 1).collect();
                }
            } else {
                return url.chars().skip(n - a).collect();
            }
        }
    }
    "".to_owned()
}

pub fn gen_version() {
    println!("cargo:rerun-if-changed=Cargo.toml");
    use std::io::prelude::*;
    let mut file = File::create("./src/version.rs").unwrap();
    for line in read_lines("Cargo.toml").unwrap().flatten() {
        let ab: Vec<&str> = line.split('=').map(|x| x.trim()).collect();
        if ab.len() == 2 && ab[0] == "version" {
            file.write_all(format!("pub const VERSION: &str = {};\n", ab[1]).as_bytes())
                .ok();
            break;
        }
    }
    // generate build date
    let build_date = format!("{}", chrono::Local::now().format("%Y-%m-%d %H:%M"));
    file.write_all(
        format!("#[allow(dead_code)]\npub const BUILD_DATE: &str = \"{build_date}\";\n").as_bytes(),
    )
    .ok();
    file.sync_all().ok();
}

fn read_lines<P>(filename: P) -> io::Result<io::Lines<io::BufReader<File>>>
where
    P: AsRef<Path>,
{
    let file = File::open(filename)?;
    Ok(io::BufReader::new(file).lines())
}

// Support 1.1.10-1, the number after - is a patch version.
pub fn get_version_number(v: &str) -> i64 {
    let mut versions = v.split('-');

    let mut n = 0;

    // The first part is the version number.
    // 1.1.10 -> 1001100, 1.2.3 -> 1001030, multiple the last number by 10
    // to leave space for patch version.
    if let Some(v) = versions.next() {
        let mut last = 0;
        for x in v.split('.') {
            last = x.parse::<i64>().unwrap_or(0);
            n = n * 1000 + last;
        }
        n -= last;
        n += last * 10;
    }

    if let Some(v) = versions.next() {
        n += v.parse::<i64>().unwrap_or(0);
    }

    // Ignore the rest

    n
}

pub fn get_modified_time(path: &std::path::Path) -> SystemTime {
    std::fs::metadata(path)
        .map(|m| m.modified().unwrap_or(UNIX_EPOCH))
        .unwrap_or(UNIX_EPOCH)
}

pub fn get_created_time(path: &std::path::Path) -> SystemTime {
    std::fs::metadata(path)
        .map(|m| m.created().unwrap_or(UNIX_EPOCH))
        .unwrap_or(UNIX_EPOCH)
}

pub fn get_exe_time() -> SystemTime {
    std::env::current_exe().map_or(UNIX_EPOCH, |path| {
        let m = get_modified_time(&path);
        let c = get_created_time(&path);
        if m > c {
            m
        } else {
            c
        }
    })
}

/// Known cases where machine_uid::get() may fail:
/// - Windows shutdown: "The media is write protected. (os error 19)"
/// - macOS (hard to reproduce, reproduced at login screen): "No matching IOPlatformUUID in `ioreg -rd1 -c IOPlatformExpertDevice` command"
pub fn get_uuid() -> Vec<u8> {
    #[cfg(not(any(target_os = "android", target_os = "ios")))]
    {
        use std::sync::atomic::{AtomicUsize, Ordering};

        static CACHED_MACHINE_UID: std::sync::OnceLock<Vec<u8>> = std::sync::OnceLock::new();
        // Throttle only applies to the fallback machine_uid::get() log below, not the Once::call_once retry logs.
        static LOG_COUNT: AtomicUsize = AtomicUsize::new(0);

        // Only macOS needs retry logic here because:
        // - macOS: in testing, only one failure occurred when reading at 50ms intervals, so retry helps
        // - Windows: failures during shutdown are persistent, retrying is pointless
        #[cfg(target_os = "macos")]
        {
            static INIT: std::sync::Once = std::sync::Once::new();
            INIT.call_once(|| {
                // Keep in sync with upstream handling:
                // https://github.com/rustdesk/rustdesk/blob/85db6779828349b23ca3eba91cc7cd36c5337797/src/common.rs#L822
                let username = whoami::username().trim_end_matches('\0').to_owned();
                let max_retries = if username == "root" { 16 } else { 8 };
                for i in 0..max_retries {
                    match machine_uid::get() {
                        Ok(id) => {
                            let _ = CACHED_MACHINE_UID.set(id.into());
                            return;
                        }
                        Err(e) => {
                            log::error!("Failed to get machine uid in macOS retry #{i}: {e}");
                        }
                    }
                    std::thread::sleep(std::time::Duration::from_millis(50));
                }
            });
        }

        if let Some(uid) = CACHED_MACHINE_UID.get() {
            return uid.clone();
        }

        match machine_uid::get() {
            Ok(id) => {
                let uid: Vec<u8> = id.into();
                let _ = CACHED_MACHINE_UID.set(uid.clone());
                return uid;
            }
            Err(e) => {
                if LOG_COUNT
                    .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |count| {
                        (count < 30).then_some(count + 1)
                    })
                    .is_ok()
                {
                    log::error!("Failed to get machine uid: {e}");
                }
            }
        }
    }
    Config::get_key_pair().1
}

#[inline]
pub fn get_time() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or(0) as _
}

#[inline]
pub fn is_ipv4_str(id: &str) -> bool {
    if let Ok(reg) = regex::Regex::new(
        r"^(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)(:\d+)?$",
    ) {
        reg.is_match(id)
    } else {
        false
    }
}

#[inline]
pub fn is_ipv6_str(id: &str) -> bool {
    if let Ok(reg) = regex::Regex::new(
        r"^((([a-fA-F0-9]{1,4}:{1,2})+[a-fA-F0-9]{1,4})|(\[([a-fA-F0-9]{1,4}:{1,2})+[a-fA-F0-9]{1,4}\]:\d+))$",
    ) {
        reg.is_match(id)
    } else {
        false
    }
}

#[inline]
pub fn is_ip_str(id: &str) -> bool {
    is_ipv4_str(id) || is_ipv6_str(id)
}

#[inline]
pub fn is_domain_port_str(id: &str) -> bool {
    // modified regex for RFC1123 hostname. check https://stackoverflow.com/a/106223 for original version for hostname.
    // according to [TLD List](https://data.iana.org/TLD/tlds-alpha-by-domain.txt) version 2023011700,
    // there is no digits in TLD, and length is 2~63.
    if let Ok(reg) = regex::Regex::new(
        r"(?i)^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z-]{0,61}[a-z]:\d{1,5}$",
    ) {
        reg.is_match(id)
    } else {
        false
    }
}

/// p2pdesk: silent by default — the global level is `warn`, and only our own
/// crates opt up to info. Connection churn (DHT scanners open/close hundreds
/// of connections a minute) is `trace` in the p2p layer, so the default
/// info stream contains only real rustdesk sessions: inbound rustdesk
/// connection / DCUtR upgrades / the 60s status line / relay lifecycle.
/// For diagnosis, set P2PDESK_LOG_FILTER, e.g. `p2p=trace` for full
/// connection-level detail.
///
/// A `warn` global also suppresses module_path-less records from foreign
/// dependency trees (which never match a module-prefix filter); our own
/// modules are whitelisted by prefix (bin = `rustdesk`, lib = `librustdesk`,
/// p2p layer = `p2p*`).
const P2PDESK_LOG_FILTER: &str = "warn,rustdesk=info,librustdesk=info,p2p=info,scrap::common::aom=info,scrap::common::codec=info";

pub fn init_log(_is_async: bool, _name: &str) -> Option<flexi_logger::LoggerHandle> {
    static INIT: std::sync::Once = std::sync::Once::new();
    #[allow(unused_mut)]
    let mut logger_holder: Option<flexi_logger::LoggerHandle> = None;
    INIT.call_once(|| {
        #[cfg(debug_assertions)]
        {
            use env_logger::*;
            init_from_env(Env::default().filter_or(DEFAULT_FILTER_ENV, P2PDESK_LOG_FILTER));
        }
        #[cfg(not(debug_assertions))]
        {
            // https://docs.rs/flexi_logger/latest/flexi_logger/error_info/index.html#write
            // though async logger more efficient, but it also causes more problems, disable it for now
            let mut path = config::Config::log_path();
            #[cfg(target_os = "android")]
            if !config::Config::get_home().exists() {
                return;
            }
            if !_name.is_empty() {
                path.push(_name);
            }
            use flexi_logger::*;
            // p2pdesk: `try_with_str` instead of `try_with_env_or_str` — the
            // latter is `LogSpecification::env_or_parse` and lets RUST_LOG
            // (e.g. RUST_LOG=debug, set by a user or a debugging session)
            // override our filter, silently re-enabling the flood.
            // RUST_LOG is ignored in release; our own P2PDESK_LOG_FILTER env
            // var still works as a diagnosis switch (e.g. "p2p=trace").
            let filter = std::env::var("P2PDESK_LOG_FILTER")
                .unwrap_or_else(|_| P2PDESK_LOG_FILTER.to_string());
            let build = |dir: &std::path::Path| {
                Logger::try_with_str(filter.as_str()).map(|x| {
                    x.log_to_file(FileSpec::default().directory(dir))
                        .write_mode(if _is_async {
                            WriteMode::Async
                        } else {
                            WriteMode::Direct
                        })
                        .format(opt_format)
                        // p2pdesk: original rotated by age only (1 file/day, 31
                        // kept) — no size cap, so a debug flood grew 999MB single
                        // files. Cap at 200MB with 5 kept.
                        .rotate(
                            Criterion::Size(200_000_000),
                            Naming::Timestamps,
                            Cleanup::KeepLogFiles(5),
                        )
                })
            };
            // flexi_logger validates the directory when it starts but opens the
            // log file only on the first record, so "started" alone says nothing
            // about whether the records can be written. Probe the directory too.
            let mut status = format!("primary={} probe={}", path.display(), probe_dir(&path));
            match build(&path).and_then(|x| x.start()) {
                Ok(handle) => logger_holder = Some(handle),
                Err(e) => {
                    // Never fail silently. The processes the Windows service
                    // spawns run under the SYSTEM token, whose profile directory
                    // does not always resolve, and a process without logs cannot
                    // be diagnosed.
                    let fallback = {
                        let mut p = config::Config::fallback_log_path();
                        // Keep the same per-role split as the primary path, or a
                        // service process and its server child would write into
                        // one file under the same name. Extending an empty base
                        // would turn the path into a CWD-relative one instead.
                        if p.is_absolute() && !_name.is_empty() {
                            p.push(_name);
                        }
                        p
                    };
                    match build(&fallback).and_then(|x| x.start()) {
                        Ok(handle) => {
                            logger_holder = Some(handle);
                            status.push_str(&format!(
                                "; primary failed ({e}); fallback={} probe={}",
                                fallback.display(),
                                probe_dir(&fallback)
                            ));
                            log::error!(
                                "log directory {} is unusable ({e}), logging to {} instead",
                                path.display(),
                                fallback.display()
                            );
                        }
                        Err(e2) => {
                            let msg = format!(
                                "no file logging: {} ({e}); {} ({e2})",
                                path.display(),
                                fallback.display()
                            );
                            status.push_str(&format!(
                                "; primary failed ({e}); fallback failed ({e2})"
                            ));
                            // No logger exists to report this to, so leave the
                            // reason where a human can find it.
                            eprintln!("p2pdesk: {msg}");
                            if fallback.is_absolute() {
                                std::fs::write(fallback.join("logging-failure.txt"), msg).ok();
                            }
                        }
                    }
                }
            }
            record_log_status(_name, &status);
        }
    });
    logger_holder
}

/// Check whether a log directory can be written to at all.
///
/// flexi_logger only creates the log file when the first record is written, so
/// nothing else on this path can tell a usable directory from one that will
/// swallow every record.
#[cfg(not(debug_assertions))]
fn probe_dir(dir: &std::path::Path) -> String {
    if dir.as_os_str().is_empty() {
        return "<empty>".to_owned();
    }
    // flexi_logger creates the directory itself before opening a file in it, so
    // the probe must do the same or it would report a directory that logging
    // would have created as unusable.
    if let Err(e) = std::fs::create_dir_all(dir) {
        return format!("Err({e})");
    }
    match std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(dir.join("write-probe.txt"))
    {
        Ok(mut file) => {
            // Write a line, so a full or quota-limited volume shows up here and
            // not only in the log file that never appears, and so the file
            // explains itself.
            use std::io::Write;
            match file.write_all(b"p2pdesk log write probe\n") {
                Ok(()) => "ok".to_owned(),
                Err(e) => format!("Err({e})"),
            }
        }
        Err(e) => format!("Err({e})"),
    }
}

/// Append where this process logs and whether that worked.
///
/// Written with plain file I/O rather than through the logger, so the record
/// exists even when the logging setup itself is what failed — the
/// service-spawned roles have no console, and an earlier silent failure left
/// them undiagnosable on the machine where it mattered. Losing this record is
/// acceptable and it is not retried anywhere else.
#[cfg(not(debug_assertions))]
fn record_log_status(name: &str, status: &str) {
    let dir = config::Config::fallback_log_path();
    if !dir.is_absolute() {
        return;
    }
    std::fs::create_dir_all(&dir).ok();
    let line = format!(
        "[ts_ms={}] role={} pid={} {status}\n",
        crate::get_time(),
        if name.is_empty() { "<none>" } else { name },
        std::process::id()
    );
    if let Ok(mut file) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(dir.join("logging-status.txt"))
    {
        // One write, so concurrent roles cannot interleave inside a line.
        use std::io::Write;
        let _ = file.write_all(line.as_bytes());
    }
}

pub fn time_based_rand() -> u32 {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos();

    let mut x = nanos as u64;
    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;

    (x % 32768) as u32
}

#[cfg(test)]
mod test {
    use super::*;

    #[test]
    fn test_mangle() {
        let addr = SocketAddr::V4(SocketAddrV4::new(Ipv4Addr::new(192, 168, 16, 32), 21116));
        assert_eq!(addr, AddrMangle::decode(&AddrMangle::encode(addr)));

        let addr = "[2001:db8::1]:8080".parse::<SocketAddr>().unwrap();
        assert_eq!(addr, AddrMangle::decode(&AddrMangle::encode(addr)));

        let addr = "[2001:db8:ff::1111]:80".parse::<SocketAddr>().unwrap();
        assert_eq!(addr, AddrMangle::decode(&AddrMangle::encode(addr)));
    }

    #[test]
    fn test_allow_err() {
        allow_err!(Err("test err") as Result<(), &str>);
        allow_err!(
            Err("test err with msg") as Result<(), &str>,
            "prompt {}",
            "failed"
        );
    }

    #[test]
    fn test_ipv6() {
        assert!(is_ipv6_str("1:2:3"));
        assert!(is_ipv6_str("[ab:2:3]:12"));
        assert!(is_ipv6_str("[ABEF:2a:3]:12"));
        assert!(!is_ipv6_str("[ABEG:2a:3]:12"));
        assert!(!is_ipv6_str("1[ab:2:3]:12"));
        assert!(!is_ipv6_str("1.1.1.1"));
        assert!(is_ip_str("1.1.1.1"));
        assert!(!is_ipv6_str("1:2:"));
        assert!(is_ipv6_str("1:2::0"));
        assert!(is_ipv6_str("[1:2::0]:1"));
        assert!(!is_ipv6_str("[1:2::0]:"));
        assert!(!is_ipv6_str("1:2::0]:1"));
    }

    #[test]
    fn test_ipv4() {
        assert!(is_ipv4_str("1.2.3.4"));
        assert!(is_ipv4_str("1.2.3.4:90"));
        assert!(is_ipv4_str("192.168.0.1"));
        assert!(is_ipv4_str("0.0.0.0"));
        assert!(is_ipv4_str("255.255.255.255"));
        assert!(!is_ipv4_str("256.0.0.0"));
        assert!(!is_ipv4_str("256.256.256.256"));
        assert!(!is_ipv4_str("1:2:"));
        assert!(!is_ipv4_str("192.168.0.256"));
        assert!(!is_ipv4_str("192.168.0.1/24"));
        assert!(!is_ipv4_str("192.168.0."));
        assert!(!is_ipv4_str("192.168..1"));
    }

    #[test]
    fn test_hostname_port() {
        assert!(!is_domain_port_str("a:12"));
        assert!(!is_domain_port_str("a.b.c:12"));
        assert!(is_domain_port_str("test.com:12"));
        assert!(is_domain_port_str("test-UPPER.com:12"));
        assert!(is_domain_port_str("some-other.domain.com:12"));
        assert!(!is_domain_port_str("under_score:12"));
        assert!(!is_domain_port_str("a@bc:12"));
        assert!(!is_domain_port_str("1.1.1.1:12"));
        assert!(!is_domain_port_str("1.2.3:12"));
        assert!(!is_domain_port_str("1.2.3.45:12"));
        assert!(!is_domain_port_str("a.b.c:123456"));
        assert!(!is_domain_port_str("---:12"));
        assert!(!is_domain_port_str(".:12"));
        // todo: should we also check for these edge cases?
        // out-of-range port
        assert!(is_domain_port_str("test.com:0"));
        assert!(is_domain_port_str("test.com:98989"));
    }

    #[test]
    fn test_mangle2() {
        let addr = "[::ffff:127.0.0.1]:8080".parse().unwrap();
        let addr_v4 = "127.0.0.1:8080".parse().unwrap();
        assert_eq!(AddrMangle::decode(&AddrMangle::encode(addr)), addr_v4);
        assert_eq!(
            AddrMangle::decode(&AddrMangle::encode("[::127.0.0.1]:8080".parse().unwrap())),
            addr_v4
        );
        assert_eq!(AddrMangle::decode(&AddrMangle::encode(addr_v4)), addr_v4);
        let addr_v6 = "[ef::fe]:8080".parse().unwrap();
        assert_eq!(AddrMangle::decode(&AddrMangle::encode(addr_v6)), addr_v6);
        let addr_v6 = "[::1]:8080".parse().unwrap();
        assert_eq!(AddrMangle::decode(&AddrMangle::encode(addr_v6)), addr_v6);
    }

    #[test]
    fn test_get_version_number() {
        assert_eq!(get_version_number("1.1.10"), 1001100);
        assert_eq!(get_version_number("1.1.10-1"), 1001101);
        assert_eq!(get_version_number("1.1.11-1"), 1001111);
        assert_eq!(get_version_number("1.2.3"), 1002030);
    }
}
