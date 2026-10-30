//! `naatre login --server URL --token TOKEN` — writes/updates this working
//! copy's `.naatre/config.toml` (mode 0600). Flags beat it, env beats it;
//! this is the file the precedence chain ends at.

use std::path::Path;

use crate::wc::Wc;

/// Runs login; returns the exit code.
pub fn run(root: &Path, server: &str, token: &str) -> i32 {
    let wc = match Wc::open(root) {
        Ok(w) => w,
        Err(e) => {
            eprintln!("error: {e}");
            return 1;
        }
    };
    let mut config = match wc.load_config() {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: {e}");
            return 1;
        }
    };
    config.server_url = server.trim_end_matches('/').to_string();
    config.token = Some(token.to_string());
    match wc.save_config(&config) {
        Ok(()) => {
            println!(
                "login: wrote {} (server {server})",
                wc.config_path().display()
            );
            0
        }
        Err(e) => {
            eprintln!("error: {e}");
            1
        }
    }
}
