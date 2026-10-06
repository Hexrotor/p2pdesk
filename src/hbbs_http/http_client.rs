use hbb_common::{
    config::{Config, Socks5Server},
    log::info,
    proxy::{Proxy, ProxyScheme},
    tls::{is_plain, TlsType},
};
use reqwest::{blocking::Client as SyncClient, Client as AsyncClient};

macro_rules! configure_http_client {
    ($builder:expr, $tls_type:expr, $danger_accept_invalid_cert:expr, $Client: ty) => {{
        // https://github.com/rustdesk/rustdesk/issues/11569
        // https://docs.rs/reqwest/latest/reqwest/struct.ClientBuilder.html#method.no_proxy
        let mut builder = $builder.no_proxy();

        match $tls_type {
            TlsType::Plain => {}
            TlsType::NativeTls => {
                builder = builder.use_native_tls();
                if $danger_accept_invalid_cert {
                    builder = builder.danger_accept_invalid_certs(true);
                }
            }
            TlsType::Rustls => {
                #[cfg(any(target_os = "android", target_os = "ios"))]
                match hbb_common::verifier::client_config($danger_accept_invalid_cert) {
                    Ok(client_config) => {
                        builder = builder.use_preconfigured_tls(client_config);
                    }
                    Err(e) => {
                        hbb_common::log::error!("Failed to get client config: {}", e);
                    }
                }
                #[cfg(not(any(target_os = "android", target_os = "ios")))]
                {
                    builder = builder.use_rustls_tls();
                    if $danger_accept_invalid_cert {
                        builder = builder.danger_accept_invalid_certs(true);
                    }
                }
            }
        }

        let client = if let Some(conf) = Config::get_socks() {
            let proxy_result = Proxy::from_conf(&conf, None);

            match proxy_result {
                Ok(proxy) => {
                    let proxy_setup = match &proxy.intercept {
                        ProxyScheme::Http { host, .. } => {
                            reqwest::Proxy::all(format!("http://{}", host))
                        }
                        ProxyScheme::Https { host, .. } => {
                            reqwest::Proxy::all(format!("https://{}", host))
                        }
                        ProxyScheme::Socks5 { addr, .. } => {
                            reqwest::Proxy::all(&format!("socks5://{}", addr))
                        }
                    };

                    match proxy_setup {
                        Ok(mut p) => {
                            if let Some(auth) = proxy.intercept.maybe_auth() {
                                if !auth.username().is_empty() && !auth.password().is_empty() {
                                    p = p.basic_auth(auth.username(), auth.password());
                                }
                            }
                            builder = builder.proxy(p);
                            builder.build().unwrap_or_else(|e| {
                                info!("Failed to create a proxied client: {}", e);
                                <$Client>::new()
                            })
                        }
                        Err(e) => {
                            info!("Failed to set up proxy: {}", e);
                            <$Client>::new()
                        }
                    }
                }
                Err(e) => {
                    info!("Failed to configure proxy: {}", e);
                    <$Client>::new()
                }
            }
        } else {
            builder.build().unwrap_or_else(|e| {
                info!("Failed to create a client: {}", e);
                <$Client>::new()
            })
        };

        client
    }};
}

// Only referenced by the plugin framework (flutter + plugin_framework builds).
#[cfg_attr(not(all(feature = "flutter", feature = "plugin_framework")), allow(dead_code))]
pub fn create_http_client(tls_type: TlsType, danger_accept_invalid_cert: bool) -> SyncClient {
    let builder = SyncClient::builder();
    configure_http_client!(builder, tls_type, danger_accept_invalid_cert, SyncClient)
}

pub fn create_http_client_async(
    tls_type: TlsType,
    danger_accept_invalid_cert: bool,
) -> AsyncClient {
    let builder = AsyncClient::builder();
    configure_http_client!(builder, tls_type, danger_accept_invalid_cert, AsyncClient)
}

pub fn get_url_for_tls<'a>(url: &'a str, proxy_conf: &'a Option<Socks5Server>) -> &'a str {
    if is_plain(url) {
        if let Some(conf) = proxy_conf {
            if conf.proxy.starts_with("https://") {
                return &conf.proxy;
            }
        }
    }
    url
}

