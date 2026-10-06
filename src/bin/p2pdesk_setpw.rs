//! Sets the local rustdesk permanent password (target side).
//!
//! Usage: `p2pdesk-setpw <password>`
//!
//! NOTE: must use Config::set_permanent_password (writes the CONFIG layer's
//! `password` field — RustDesk.toml), NOT set_option("password", ...) which
//! writes CONFIG2.options (RustDesk2.toml) that the login flow never reads.

use hbb_common::config::Config;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() < 2 {
        eprintln!("usage: p2pdesk-setpw <password>");
        std::process::exit(2);
    }
    let ok = Config::set_permanent_password(&args[1]);
    println!("permanent password set: {ok}");
}
