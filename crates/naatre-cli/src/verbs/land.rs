//! `naatre land [--key K] [--record-conflict]` — land the working copy's
//! head onto the server's `main` (server-side rebase + CAS).
//!
//! The idempotency key defaults to a random 8-hex suffix and is PRINTED so
//! a retry can reuse it (`--key`). A 503 (writer slot busy or the same key
//! in flight) prints the retry hint; a 409 `ref_conflict` prints the
//! per-path conflicts and the server's resolution recipe (the same key
//! re-lands after a conflict; `--record-conflict` instead records the
//! conflict on main as a conflicted changeset and settles once —
//! `resolve` then clears the conflicted paths); a 409
//! `idempotency_key_settled` prints the settled head and land-status
//! pointer. On success the working copy advances onto the landed history
//! through the ONE sync materialization path (the old-head → landed-head
//! tree difference: the same dirty gate, verified fetch, temp+rename,
//! and stat-index update — the lander's disk matches what a fresh sync
//! of the new head writes, on every landed path; a dirty-gate refusal
//! degrades to preserve-and-warn, the land itself cannot un-settle) and
//! records the land base and the conflicted marks (m12b-fix: the diff's
//! marker detection, unioned with a conflicted settlement's response
//! `conflicts`; a stale mark whose disk bytes are no longer markers
//! clears).

use std::path::Path;

use naatre_core::ids::Id;
use serde_json::json;

use super::{Net, VerbError};
use crate::client::{Client, HttpError};
use crate::wc::{State, Wc};

/// A settled land: the key used (for retries) and the server's response
/// fields. The MCP `land` tool returns `{key, landed, main, rebased}` built
/// from this.
pub struct LandOutcome {
    pub key: String,
    pub landed: String,
    pub main: String,
    pub rebased: u64,
    /// Paths the settlement node records as conflicted (M12b; empty unless
    /// the land settled a conflicted node — `lands.state = 'conflicted'`).
    pub conflicts: Vec<String>,
}

/// The shared land POST (`{head, key}` → `{landed, main, rebased}`): the
/// one code path `naatre land` and the mount daemon's `--flush-land` both
/// drive. `on_key` fires BEFORE the request (a 503 or a crash must still
/// show the key a retry needs) — the CLI prints it, the mount daemon
/// prints it, the MCP server passes a no-op. `record_conflict` is the M12
/// opt-in: instead of the default 409 + recipe on a rebase conflict, the
/// conflict is recorded on main as a conflicted changeset (settled ONCE,
/// `lands.state = 'conflicted'`).
pub fn land_changeset(
    client: &Client,
    repo: &str,
    head: &str,
    key: &str,
    record_conflict: bool,
    on_key: &mut dyn FnMut(&str),
) -> Result<LandOutcome, HttpError> {
    on_key(key);
    let (_status, value) = client.post_json(
        &format!("/v1/repos/{}/land", crate::client::uri_segment(repo)),
        &json!({ "head": head, "key": key, "record_conflict": record_conflict }),
    )?;
    Ok(LandOutcome {
        key: key.to_string(),
        landed: value["landed"].as_str().unwrap_or("?").to_string(),
        main: value["main"].as_str().unwrap_or("?").to_string(),
        rebased: value["rebased"].as_u64().unwrap_or(0),
        conflicts: value["conflicts"]
            .as_array()
            .map(|a| {
                a.iter()
                    .filter_map(|v| v.as_str().map(str::to_string))
                    .collect()
            })
            .unwrap_or_default(),
    })
}

/// Runs land; returns the pinned exit code (0 success, 2 conflict-class,
/// 1 otherwise).
pub fn run(root: &Path, key: Option<&str>, record_conflict: bool, net: &Net) -> i32 {
    let mut printed_key = String::new();
    match core(root, key, record_conflict, net, &mut |k| {
        printed_key = k.to_string();
        println!("land key: {k}");
    }) {
        Ok(o) => {
            println!(
                "{}",
                json!({ "landed": o.landed, "main": o.main, "rebased": o.rebased })
            );
            for c in &o.conflicts {
                println!("conflicted: {c}");
            }
            0
        }
        Err(VerbError::Http(HttpError::Envelope {
            code,
            message,
            details,
            ..
        })) => {
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
                         same key is in flight); retry with the same key: naatre land --key {printed_key}"
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
        Err(e) => e.report(),
    }
}

/// The land core: resolves the working copy, generates (or takes) the
/// idempotency key, POSTs the land, and advances the working copy onto the
/// landed history on success. `on_key` fires BEFORE the request (the CLI
/// prints the key there — a 503 or a crash must still show the key a retry
/// needs); the MCP server passes a no-op.
pub fn core(
    root: &Path,
    key: Option<&str>,
    record_conflict: bool,
    net: &Net,
    on_key: &mut dyn FnMut(&str),
) -> Result<LandOutcome, VerbError> {
    let wc = Wc::open(root).map_err(VerbError::Local)?;
    let state = wc.load_state().map_err(VerbError::Local)?;
    let client = wc.client(net).map_err(VerbError::Local)?;
    let key = key.map(str::to_string).unwrap_or_else(random_key);

    let outcome = land_changeset(
        &client,
        &state.server_repo,
        &state.head,
        &key,
        record_conflict,
        on_key,
    )
    .map_err(VerbError::Http)?;
    // The rebase rewrote the client's head server-side; advance the
    // working copy onto the landed history through the ONE sync
    // materialization path (m12b-fix, advisor): the tree difference
    // OLD head → landed head applies with the same dirty gate,
    // verified fetch, temp+rename, tracked-del loop, and stat-index
    // update a sync uses — so the lander's disk matches what a fresh
    // sync of the new head would write, at EVERY landed path (a record
    // land rebases onto current main; the settlement tree can differ
    // from the local tree in many paths, and a normal rebased land has
    // the same shape — both go through this shared path, so neither is
    // left stale-on-disk). The settlement's conflicted marks come from
    // the diff's marker detection, unioned with the response's
    // authoritative `conflicts` for a conflicted settlement.
    //
    // m12b-fix round 2 (advisor, option a): a REFUSED difference leaves
    // the working copy COMPLETELY untouched — state.head stays on the
    // OLD head, whose tree the disk and index truthfully still
    // materialize. Advancing the head anyway would strand the working
    // copy (sync short-circuits when target == head, so the promised
    // recovery would never apply the difference); with head on the old
    // side the later sync — once the local edit is cleared or stashed —
    // sees a real difference and applies it through sync's own dirty
    // rules. The same no-state-change rule covers a hard
    // fetch/materialization failure (the settlement stands server-side;
    // `naatre doctor --rebuild` + `naatre sync` recover locally).
    if outcome.landed != "?" {
        let old_head = Id::from_hex(&state.head).ok_or_else(|| {
            VerbError::Local("state.json head is not a valid changeset id".into())
        })?;
        let new_head = Id::from_hex(&outcome.landed)
            .ok_or_else(|| VerbError::Local("the server landed an invalid changeset id".into()))?;
        if new_head != old_head {
            let old_index = wc.load_index().map_err(VerbError::Local)?;
            match super::sync::apply_tree_diff(
                root, &state, old_index, &client, &old_head, &new_head, false,
            ) {
                Ok(Ok(applied)) => {
                    let mut conflicted = applied.conflicted.clone();
                    for p in &outcome.conflicts {
                        if !conflicted.contains(p) {
                            conflicted.push(p.clone());
                        }
                    }
                    let new_state = State {
                        head: outcome.landed.clone(),
                        base: Some(outcome.main.clone()),
                        sparse: state.sparse.clone(),
                        server_repo: state.server_repo.clone(),
                        conflicted,
                    };
                    wc.save_state(&new_state).map_err(|e| {
                        VerbError::Local(format!(
                            "land settled but updating .naatre/state.json failed: {e}"
                        ))
                    })?;
                    wc.save_index(&applied.index).map_err(|e| {
                        VerbError::Local(format!(
                            "land settled but updating .naatre/index.nai failed: {e}"
                        ))
                    })?;
                }
                Ok(Err(blocked)) => {
                    // GAP 1 (m12b-fix round 2): with head back on the old
                    // side, the disk holds the USER'S bytes at every path
                    // — carrying the response's conflicts as marks would
                    // show `C` for the user's own edit and let resolve
                    // settle blind. Keep only the older marks whose disk
                    // bytes are still marker-shaped; the recovery sync
                    // marks by content once it materializes the
                    // settlement.
                    let new_state = State {
                        head: old_head.to_string(),
                        base: state.base.clone(),
                        sparse: state.sparse.clone(),
                        server_repo: state.server_repo.clone(),
                        conflicted: stale_marks(root, &state.conflicted),
                    };
                    wc.save_state(&new_state).map_err(|e| {
                        VerbError::Local(format!(
                            "land settled but updating .naatre/state.json failed: {e}"
                        ))
                    })?;
                    let blocked_list = blocked.blocked.join(", ");
                    let collide_list = blocked.colliding.join(", ");
                    // The retry hazard, per settlement kind (m12b-fix
                    // round 2): a rerun land after a blocked CLEAN
                    // settlement is a harmless no-op (the server
                    // re-derives the already-contained delta and settles
                    // done on the unchanged main — pinned). After a
                    // blocked CONFLICTED settlement the rerun MERGES:
                    // your delta goes through the line merge against the
                    // marker content and lands a NEW PLAIN node — main
                    // advances silently and the recorded conflict is
                    // DOWNGRADED to marker text on the tip (observed +
                    // pinned; this is the missing server-side
                    // marker-shape check at land, the recorded
                    // follow-up) — so that shape's warning says do NOT
                    // re-land.
                    let (conflicts_note, retry_note) = if outcome.conflicts.is_empty() {
                        (String::new(), String::new())
                    } else {
                        (
                            format!(
                                "; the landed head is conflicted at: [{}]",
                                outcome.conflicts.join(", ")
                            ),
                            "; do NOT re-land — a rerun merges your delta against the \
                             marker content and lands a new plain node, advancing main \
                             without the conflict representation"
                                .to_string(),
                        )
                    };
                    eprintln!(
                        "warning: the land settled, but the working-copy update was refused \
                         by the dirty rules — local edits on incoming paths: [{blocked_list}]; \
                         colliding untracked: [{collide_list}]; everything preserved at the \
                         old head{conflicts_note}. Clear or stash the edits, then run \
                         `naatre sync` (`--force` discards) to reach the landed \
                         head{retry_note}"
                    );
                }
                // A hard failure fetching/applying the difference: the
                // settlement stands; the working copy may be partially
                // written (per-file atomic renames, index not advanced) —
                // the marks filter the same way, doctor rebuilds the
                // index, sync converges on the re-run.
                Err(e) => {
                    let new_state = State {
                        head: old_head.to_string(),
                        base: state.base.clone(),
                        sparse: state.sparse.clone(),
                        server_repo: state.server_repo.clone(),
                        conflicted: stale_marks(root, &state.conflicted),
                    };
                    wc.save_state(&new_state).map_err(|e| {
                        VerbError::Local(format!(
                            "land settled but updating .naatre/state.json failed: {e}"
                        ))
                    })?;
                    eprintln!(
                        "warning: the land settled, but the working-copy update failed \
                         ({e}); run `naatre doctor --rebuild` if status looks wrong, \
                         then `naatre sync` to reach the landed head"
                    );
                }
            }
        }
    }
    Ok(outcome)
}

/// The older conflicted marks whose disk bytes are still a rendered
/// marker file — the marks a degraded land (blocked gate, hard failure)
/// keeps: with head back on the old side, the disk holds the user's own
/// bytes, and a mark on them would show `C` for the user's edit and let
/// resolve settle blind.
fn stale_marks(root: &Path, marks: &[String]) -> Vec<String> {
    marks
        .iter()
        .filter(|p| {
            std::fs::read(root.join(p))
                .map(|b| naatre_core::conflict::marker_shaped(&b))
                .unwrap_or(false)
        })
        .cloned()
        .collect()
}

/// The default idempotency key: a random 8-hex suffix (4 bytes from the OS
/// CSPRNG; a time-derived fallback keeps the verb working without it).
pub fn random_key() -> String {
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
