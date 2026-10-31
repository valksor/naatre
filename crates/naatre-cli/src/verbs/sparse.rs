//! `naatre sparse add|remove|list` — the sparse prefix list.
//!
//! The list in `state.json` is an INCLUDE list: empty means the full tree,
//! non-empty means only the listed prefixes are materialized.
//! - `add P` fetches and writes the files under `P` at HEAD (the working
//!   copy's anchor), then adds `P`. A widen that would clobber local work
//!   refuses with `dirty_working_copy` — a dirty tracked file (edit or
//!   deletion) or an untracked / normalization-folded disk file at a path
//!   it would materialize, or a symlink (file or directory) at any
//!   component of such a path; `--force` discards exactly the named
//!   colliders (unlinks symlinks, deletes colliding untracked files),
//!   materializes over the rest, and proceeds. Blobs are fetched and
//!   hash-verified BEFORE anything is discarded, so a failed fetch costs
//!   the user nothing.
//! - `remove P` deletes the indexed files under `P` from disk, drops their
//!   index rows, and removes `P` from the list. Untracked files under the
//!   prefix are left alone (preserve-everything); removing the LAST prefix
//!   is refused (an empty list would mean "full checkout" again).

use std::collections::BTreeSet;
use std::path::Path;

use naatre_core::path;
use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::wc::fetch::{fetch_blobs_verified, materialize, walk_tree};
use crate::wc::{State, Wc, remove_and_prune, under_prefix};

pub enum Action {
    Add { prefix: String, force: bool },
    Remove(String),
    List,
}

/// Runs the sparse verb; returns the exit code.
pub fn run(root: &Path, action: Action, net: &Net) -> i32 {
    match value(root, action, net) {
        Ok(v) => {
            match v["action"].as_str().unwrap_or("") {
                "list" => {
                    let prefixes: Vec<&str> = v["prefixes"]
                        .as_array()
                        .map(|a| a.iter().filter_map(|p| p.as_str()).collect())
                        .unwrap_or_default();
                    if prefixes.is_empty() {
                        println!("(full checkout — no sparse prefixes)");
                    } else {
                        for p in prefixes {
                            println!("{p}");
                        }
                    }
                }
                "add" => {
                    if v["already_listed"].as_bool() == Some(true) {
                        println!(
                            "sparse: {} is already listed",
                            v["prefix"].as_str().unwrap_or("?")
                        );
                    } else {
                        println!(
                            "sparse: added {} ({} file(s) materialized)",
                            v["prefix"].as_str().unwrap_or("?"),
                            v["materialized"].as_u64().unwrap_or(0),
                        );
                    }
                }
                "remove" => println!(
                    "sparse: removed {} ({} file(s) dropped from disk and index)",
                    v["prefix"].as_str().unwrap_or("?"),
                    v["removed"].as_u64().unwrap_or(0),
                ),
                _ => unreachable!("value() emits only list/add/remove actions"),
            }
            0
        }
        Err(e) => e.report(),
    }
}

/// The sparse result per action: `list` → `{action, prefixes}` (empty =
/// full checkout), `add` → `{action, prefix, materialized, already_listed}`,
/// `remove` → `{action, prefix, removed}`. No HTTP analog; the MCP `sparse`
/// tool returns the same values.
pub fn value(root: &Path, action: Action, net: &Net) -> Result<Value, VerbError> {
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    match action {
        Action::List => Ok(json!({ "action": "list", "prefixes": state.sparse })),
        Action::Add { prefix, force } => add(root, &wc, state, &prefix, force, net),
        Action::Remove(prefix) => remove(root, &wc, state, &prefix),
    }
}

fn add(
    root: &Path,
    wc: &Wc,
    mut state: State,
    prefix: &str,
    force: bool,
    net: &Net,
) -> Result<Value, VerbError> {
    path::validate(prefix)
        .map_err(|e| VerbError::Usage(format!("sparse prefix {prefix:?}: {e}")))?;
    if state.sparse.is_empty() {
        return Err(VerbError::Local(
            "this working copy is full (no sparse prefixes); narrow it by re-cloning with --sparse"
                .into(),
        ));
    }
    if state.sparse.iter().any(|p| p == prefix) {
        return Ok(json!({
            "action": "add", "prefix": prefix, "materialized": 0, "already_listed": true,
        }));
    }
    let client = wc.client(net)?;
    // The widen walks the working copy's HEAD: the index rows it writes
    // must describe the tree the checkout is anchored at, or status and
    // commit mislabel the working copy until the next sync.
    let head = client.get_json(
        &format!(
            "/v1/repos/{}/changesets/{}/root",
            crate::client::uri_segment(&state.server_repo),
            state.head
        ),
        &[],
    )?["root_tree_id"]
        .as_str()
        .and_then(naatre_core::ids::Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed root tree id".into()))?;
    let mut index = wc.load_index()?;
    let prefixes = vec![prefix.to_string()];
    let files = walk_tree(&client, &state.server_repo, &head, &prefixes)?;

    // The preserve-everything gate, the same dirty_working_copy shape as
    // sync: local work at a path the widen would materialize — a dirty
    // tracked file (edit or deletion) or an untracked / normalization-folded
    // disk file — would be clobbered, so the widen refuses and lists BOTH
    // lists. Unrelated local edits never block. `--force` discards exactly
    // the colliding untracked files, materializes over the rest, proceeds.
    let report = crate::wc::scan::classify(root, &index)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    let widening: BTreeSet<&str> = files.iter().map(|f| f.path.as_str()).collect();
    let blocked: Vec<String> = report
        .modified
        .iter()
        .chain(report.deleted.iter())
        .filter(|path| widening.contains(path.as_str()))
        .cloned()
        .collect();
    let mut colliding: Vec<String> = Vec::new();
    for (norm, raws) in &report.folds {
        if widening.contains(norm.as_str()) {
            colliding.extend(raws.iter().cloned());
        }
    }
    for norm in &report.untracked {
        if widening.contains(norm.as_str()) {
            colliding.push(report.raw_of_untracked[norm].clone());
        }
    }
    colliding.sort();
    colliding.dedup();

    // Symlinks are invisible to the scanner (it skips them), so the
    // tracked/untracked arms above cannot see one. A symlink at a widening
    // path — or at any directory component of one — would be clobbered
    // (file) or redirect the widen's writes outside the checkout (dir), so
    // every symlink component is a collider in its own right, named
    // "(symlink)". `--force` removes the link itself (never its target).
    let mut symlinks: Vec<String> = Vec::new();
    for path in &widening {
        let mut rel = String::new();
        for comp in path.split('/') {
            rel = if rel.is_empty() {
                comp.to_string()
            } else {
                format!("{rel}/{comp}")
            };
            let is_symlink = root
                .join(&rel)
                .symlink_metadata()
                .map(|m| m.file_type().is_symlink())
                .unwrap_or(false); // absent: nothing to refuse. NOTE: a stat error fails OPEN here (reads as "not a symlink"); the safety net is the subsequent write, which fails loudly on the same error — an EACCES is never mistaken for a planted link
            if is_symlink {
                symlinks.push(rel.clone());
            }
        }
    }
    symlinks.sort();
    symlinks.dedup();

    if (!blocked.is_empty() || !colliding.is_empty() || !symlinks.is_empty()) && !force {
        let blocked_list = blocked.join(", ");
        let collide_list = colliding.join(", ");
        let symlink_list = symlinks.join(", ");
        return Err(VerbError::Http(crate::client::HttpError::Envelope {
            status: 409,
            code: "dirty_working_copy".into(),
            message: format!(
                "refusing to widen (nothing changed): local edits on the paths it would \
                 materialize: [{blocked_list}]; colliding untracked: [{collide_list}]; \
                 symlinks on the widened paths: [{symlink_list}]; \
                 use --force to discard them"
            ),
            details: None,
            retry_after: None,
        }));
    }

    // Fetch and hash-verify every blob BEFORE anything is discarded: a
    // failed fetch must cost the user nothing (nothing materialized,
    // nothing removed).
    let ids: Vec<naatre_core::ids::Id> = files.iter().map(|f| f.blob).collect();
    let blobs = fetch_blobs_verified(&client, &state.server_repo, &ids)?;

    // Fetch verified: only now discard exactly the colliders the refusal
    // named — untracked files are deleted, symlinks are unlinked (never
    // their targets).
    for raw in &symlinks {
        remove_and_prune(root, raw)
            .map_err(|e| VerbError::Local(format!("removing colliding symlink {raw:?}: {e}")))?;
    }
    for raw in &colliding {
        remove_and_prune(root, raw)
            .map_err(|e| VerbError::Local(format!("removing colliding untracked {raw:?}: {e}")))?;
    }

    let mut written = 0usize;
    for file in &files {
        // Reachable only for CLEAN tracked files under an overlapping
        // widen (the list already materialized part of this prefix):
        // without --force they stay exactly as the user has them (the next
        // sync reconciles them); with --force they are materialized over.
        // A dirty tracked file never reaches this line — the gate above
        // refused it.
        if !force && index.rows.contains_key(&file.path) {
            continue;
        }
        materialize(root, file, &blobs[&file.blob])?;
        let meta = std::fs::metadata(root.join(&file.path))
            .map_err(|e| VerbError::Local(format!("stating {}: {e}", file.path)))?;
        index.rows.insert(
            file.path.clone(),
            crate::wc::index::Row::from_stat(file.path.clone(), file.blob.0, &meta),
        );
        written += 1;
    }
    state.sparse.push(prefix.to_string());
    wc.save_state(&state)?;
    wc.save_index(&index)?;
    Ok(json!({
        "action": "add", "prefix": prefix, "materialized": written, "already_listed": false,
    }))
}

fn remove(root: &Path, wc: &Wc, mut state: State, prefix: &str) -> Result<Value, VerbError> {
    if !state.sparse.iter().any(|p| p == prefix) {
        return Err(VerbError::Local(format!(
            "sparse prefix {prefix:?} is not in the list (see: naatre sparse list)"
        )));
    }
    if state.sparse.len() == 1 {
        return Err(VerbError::Local(
            "cannot remove the last sparse prefix (the list would mean \"full checkout\"); \
             re-clone without --sparse instead"
                .into(),
        ));
    }
    let mut index = wc.load_index()?;
    let under: Vec<String> = index
        .rows
        .keys()
        .filter(|p| under_prefix(prefix, p))
        .cloned()
        .collect();
    let mut removed = 0usize;
    for p in under {
        remove_and_prune(root, &p).map_err(|e| VerbError::Local(format!("removing {p:?}: {e}")))?;
        index.rows.remove(&p);
        removed += 1;
    }
    state.sparse.retain(|p| p != prefix);
    wc.save_state(&state)?;
    wc.save_index(&index)?;
    Ok(json!({ "action": "remove", "prefix": prefix, "removed": removed }))
}
