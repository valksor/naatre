//! `naatre land [--key K]` — land the working copy's head onto the server's
//! `main` (server-side rebase + CAS).
//!
//! The idempotency key defaults to a random 8-hex suffix and is PRINTED so
//! a retry can reuse it (`--key`). A 503 (writer slot busy or the same key
//! in flight) prints the retry hint; a 409 `ref_conflict` prints the
//! per-path conflicts and the server's resolution recipe (the same key
//! re-lands after a conflict); a 409 `idempotency_key_settled` prints the
//! settled head and land-status pointer. On success the working copy
//! advances onto the landed history (its head is rewritten server-side) and
//! records the land base.

use std::path::Path;

use serde_json::json;

use super::Net;
use crate::client::HttpError;
use crate::wc::{State, Wc};

/// Runs land; returns the pinned exit code (0 success, 2 conflict-class,
/// 1 otherwise).
pub fn run(root: &Path, key: Option<&str>, net: &Net) -> i32 {
    let wc = match Wc::open(root) {
        Ok(wc) => wc,
        Err(e) => return super::VerbError::Local(e).report(),
    };
    let state = match wc.load_state() {
        Ok(s) => s,
        Err(e) => return super::VerbError::Local(e).report(),
    };
    let client = match wc.client(net) {
        Ok(c) => c,
        Err(e) => return super::VerbError::Local(e).report(),
    };
    let key = key.map(str::to_string).unwrap_or_else(random_key);
    // Printed before the request: a 503 or a crash must still show the key
    // a retry needs.
    println!("land key: {key}");

    let repo = &state.server_repo;
    let body = json!({ "head": state.head, "key": key });
    match client.post_json(
        &format!("/v1/repos/{}/land", crate::client::uri_segment(repo)),
        &body,
    ) {
        Ok((_status, value)) => {
            let landed = value["landed"].as_str().unwrap_or("?").to_string();
            let main = value["main"].as_str().unwrap_or("?").to_string();
            let rebased = value["rebased"].as_u64().unwrap_or(0);
            // The rebase rewrote the client's head server-side; advance the
            // working copy onto the landed history and record the base.
            if landed != "?" {
                let new_state = State {
                    head: landed.clone(),
                    base: Some(main.clone()),
                    sparse: state.sparse,
                    server_repo: state.server_repo,
                };
                if let Err(e) = wc.save_state(&new_state) {
                    return super::VerbError::Local(format!(
                        "land settled but updating .naatre/state.json failed: {e}"
                    ))
                    .report();
                }
            }
            println!(
                "{}",
                json!({ "landed": landed, "main": main, "rebased": rebased })
            );
            0
        }
        Err(HttpError::Envelope {
            code,
            message,
            details,
            ..
        }) => {
            eprintln!("error: {code}: {message}");
            match code.as_str() {
                // Conflict class: per-path conflicts + the resolution
                // recipe (exit 2).
                "ref_conflict" => {
                    if let Some(conflicts) =
                        details.as_ref().and_then(|d| d["conflicts"].as_array())
                    {
                        for c in conflicts {
                            eprintln!(
                                "  conflict: {} — {} (base {}, ours {}, theirs {})",
                                c["path"].as_str().unwrap_or("?"),
                                c["reason"].as_str().unwrap_or("?"),
                                c["base"].as_str().unwrap_or("<absent>"),
                                c["ours"].as_str().unwrap_or("<absent>"),
                                c["theirs"].as_str().unwrap_or("<absent>"),
                            );
                        }
                    }
                    if let Some(recipe) = details.as_ref().and_then(|d| d["recipe"].as_str()) {
                        eprintln!("recipe: {recipe}");
                    }
                    2
                }
                "writer_busy" => {
                    eprintln!(
                        "the land was NOT settled (writer slot busy, or another land with the \
                         same key is in flight); retry with the same key: naatre land --key {key}"
                    );
                    1
                }
                // The key settled with a different payload: the details
                // carry the settled head and the land-status pointer
                // (409 → conflict class, exit 2).
                "idempotency_key_settled" => {
                    if let Some(d) = details.as_ref() {
                        eprintln!(
                            "  settled head: {}",
                            d["settled_head"].as_str().unwrap_or("?")
                        );
                        eprintln!(
                            "  land status: {}",
                            d["land_status_pointer"].as_str().unwrap_or("?")
                        );
                    }
                    2
                }
                _ => 1,
            }
        }
        Err(e) => crate::client::report(&e),
    }
}

/// The default idempotency key: a random 8-hex suffix (4 bytes from the OS
/// CSPRNG; a time-derived fallback keeps the verb working without it).
fn random_key() -> String {
    let from_os = std::fs::File::open("/dev/urandom").ok().and_then(|mut f| {
        use std::io::Read as _;
        let mut b = [0u8; 4];
        f.read_exact(&mut b).ok().map(|_| b)
    });
    match from_os {
        Some(b) => b.iter().map(|x| format!("{x:02x}")).collect(),
        None => format!("{:08x}", (crate::wc::now_ns() & 0xffff_ffff) as u32),
    }
}
