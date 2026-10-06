use hbb_common::{
    allow_err,
    config::{self, keys::*, option2bool, Config, RENDEZVOUS_PORT},
    log,
    sleep,
};

use crate::server::{check_zombie, new as new_server, ServerPtr};

// p2pdesk: this module used to be the hbbs rendezvous client — it registered
// the device with the rustdesk public servers (rs-ny.rustdesk.com) and handled
// their punch/relay/intranet coordination messages. All of that is gone:
// discovery, NAT classification and hole punching live in the go-libp2p
// module. What remains is the direct-access TCP listener and the startup
// bundle for the p2p accept loop and the LAN listener.

#[derive(Clone)]
pub struct RendezvousMediator {}

impl RendezvousMediator {
    // p2pdesk: there is no register loop to restart anymore. Kept as a no-op
    // because several option-change paths still call it (they used to bounce
    // the hbbs registration after e.g. a proxy change); removing those call
    // sites is left to the UI cleanup.
    pub fn restart() {}

    pub async fn start_all() {
        if config::is_outgoing_only() {
            loop {
                sleep(1.).await;
            }
        }
        check_zombie();
        let server = new_server();
        // Direct-access TCP listener (the stock rustdesk direct port).
        let server_cloned = server.clone();
        tokio::spawn(async move {
            direct_server(server_cloned).await;
        });
        // p2pdesk: p2p listen + inbound loop (the hbbs-free control channel)
        crate::p2pdesk::start_incoming(server.clone());
        #[cfg(target_os = "android")]
        let start_lan_listening = true;
        #[cfg(not(any(target_os = "android", target_os = "ios")))]
        let start_lan_listening = crate::platform::is_installed();
        if start_lan_listening {
            std::thread::spawn(move || {
                allow_err!(super::lan::start_listening());
            });
        }
        // It is ok to run xdesktop manager when the headless function is not allowed.
        #[cfg(target_os = "linux")]
        if crate::is_server() {
            crate::platform::linux_desktop_manager::start_xdesktop();
        }
        scrap::codec::test_av1();
        // Sweep existing sessions only when the service *transitions* into the
        // stopped state. Sweeping every iteration would also kill sessions
        // established after the transition: the p2p accept loop keeps serving
        // while stopped, so without this guard every fresh session is closed
        // at the next tick.
        let mut service_was_stopped = false;
        loop {
            let service_stopped =
                config::option2bool("stop-service", &Config::get_option("stop-service"))
                    || crate::platform::installing_service();
            if service_stopped && !service_was_stopped {
                server.write().unwrap().close_connections();
            }
            service_was_stopped = service_stopped;
            sleep(1.).await;
        }
    }
}

fn get_direct_port() -> i32 {
    let mut port = Config::get_option("direct-access-port")
        .parse::<i32>()
        .unwrap_or(0);
    if port <= 0 {
        port = RENDEZVOUS_PORT + 2;
    }
    port
}

async fn direct_server(server: ServerPtr) {
    let mut listener = None;
    let mut port = 0;
    loop {
        let disabled = !option2bool(
            OPTION_DIRECT_SERVER,
            &Config::get_option(OPTION_DIRECT_SERVER),
        ) || option2bool("stop-service", &Config::get_option("stop-service"));
        if !disabled && listener.is_none() {
            port = get_direct_port();
            match hbb_common::tcp::listen_any(port as _).await {
                Ok(l) => {
                    listener = Some(l);
                    log::info!(
                        "Direct server listening on: {:?}",
                        listener.as_ref().map(|l| l.local_addr())
                    );
                }
                Err(err) => {
                    // to-do: pass to ui
                    log::error!(
                        "Failed to start direct server on port: {}, error: {}",
                        port,
                        err
                    );
                    loop {
                        if port != get_direct_port() {
                            break;
                        }
                        sleep(1.).await;
                    }
                }
            }
        }
        if let Some(l) = listener.as_mut() {
            if disabled || port != get_direct_port() {
                log::info!("Exit direct access listen");
                listener = None;
                continue;
            }
            if let Ok(Ok((stream, addr))) = hbb_common::timeout(1000, l.accept()).await {
                stream.set_nodelay(true).ok();
                log::info!("direct access from {}", addr);
                let local_addr = stream
                    .local_addr()
                    .unwrap_or(Config::get_any_listen_addr(true));
                let server = server.clone();
                tokio::spawn(async move {
                    allow_err!(
                        crate::server::create_tcp_connection(
                            server,
                            hbb_common::Stream::from(stream, local_addr),
                            addr,
                            false,
                            crate::server::ConnectionMeta::default(), // Direct connections don't have server-side user context.
                        )
                        .await
                    );
                });
            } else {
                sleep(0.1).await;
            }
        } else {
            sleep(1.).await;
        }
    }
}
