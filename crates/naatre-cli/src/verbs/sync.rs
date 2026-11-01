//! `naatre sync [--force]` — advance the working copy to the server's
//! `main`.
//!
//! Dirty check FIRST (the preserve-everything rule): the sync refuses with
//! exit 2 and `dirty_working_copy` only when materialization would clobber
//! local work — a locally modified or deleted tracked file whose path a
//! cone-included incoming change touches (add/mod or del: a locally
//! modified file that an incoming del removes would be lost too), or an
//! untracked/normalization-folded disk file colliding with a cone-included
//! incoming path (upsert or del: an incoming del must not silently eat an
//! untracked file at the path either) — listing BOTH lists. Out-of-cone
//! incoming paths are never applied here, so they never block. Unrelated
//! local edits survive the sync untouched (materialization only touches
//! cone-included incoming paths; the head advances; status afterwards still
//! lists them). `--force` discards exactly the colliding files (deletes
//! untracked colliders, overwrites colliding tracked edits) and proceeds —
//! but only after every blob is fetched and hash-verified, so a failed
//! fetch discards nothing. Belt and braces: the del loop removes only
//! paths still present in the index (tracked), so a tracked-absent path
//! can never be deleted from disk as if it were ours.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;

use naatre_core::ids::Id;
use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::wc::fetch::{fetch_blobs_verified, materialize, walk_tree_allowing_missing_prefixes};
use crate::wc::{State, Wc, remove_and_prune, under_sparse};

/// Runs sync; prints the outcome and returns the exit code.
pub fn run(root: &Path, force: bool, net: &Net) -> i32 {
    match value(root, force, net) {
        Ok(v) => {
            if v["up_to_date"].as_bool() == Some(true) {
                println!("up to date");
            } else {
                println!("synced: {}", v["head"].as_str().unwrap_or("?"));
                // Output honesty: only paths this sync actually applied
                // (cone-included) print as A/D/M — the core already filters.
                for entry in v["applied"].as_array().map_or(&[][..], Vec::as_slice) {
                    let c = match entry["kind"].as_str().unwrap_or("?") {
                        "add" => "A",
                        "del" => "D",
                        _ => "M",
                    };
                    println!("{c} {}", entry["path"].as_str().unwrap_or("?"));
                }
            }
            0
        }
        Err(e) => e.report(),
    }
}

/// The sync result: `{up_to_date, head, applied: [{path, kind}]}` — `applied`
/// lists only the cone-included paths this sync actually applied (no HTTP
/// analog; the MCP `sync` tool returns the same value).
pub fn value(root: &Path, force: bool, net: &Net) -> Result<Value, VerbError> {
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    let mut index = wc.load_index()?;
    let client = wc.client(net)?;

    let repo = state.server_repo.clone();
    let head = Id::from_hex(&state.head)
        .ok_or_else(|| VerbError::Local("state.json head is not a valid changeset id".into()))?;

    // Fetch current main; up to date → zero writes, done.
    let refv = client.get_json(
        &format!("/v1/repos/{}/refs/main", crate::client::uri_segment(&repo)),
        &[],
    )?;
    let target = refv["target"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server main is unborn; nothing to sync to".into()))?;
    if target == head {
        return Ok(json!({ "up_to_date": true, "head": target.to_hex(), "applied": [] }));
    }

    // Incoming path-level diff.
    let diffv = client.get_json(
        &format!("/v1/repos/{}/diff", crate::client::uri_segment(&repo)),
        &[
            ("a", head.to_hex().as_str()),
            ("b", target.to_hex().as_str()),
        ],
    )?;
    let incoming = diffv["changes"]
        .as_array()
        .ok_or_else(|| VerbError::Local("server sent a malformed diff".into()))?
        .iter()
        .map(|c| {
            (
                c["path"].as_str().unwrap_or_default().to_string(),
                c["kind"].as_str().unwrap_or_default().to_string(),
            )
        })
        .collect::<Vec<_>>();

    // The dirty gate (plan wording): only local work whose path a
    // cone-included incoming change touches can be clobbered by
    // materialization — a dirty tracked file on such a path (add/mod or
    // del: a locally modified file that an incoming del removes would be
    // lost too), or an untracked / normalization-folded disk file on a
    // cone-included incoming path (upsert or del: an incoming del removes
    // an untracked file at the path just as silently). Unrelated local
    // edits survive the sync untouched.
    let report = crate::wc::scan::classify(root, &index)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    let mut dirty: BTreeSet<String> = BTreeSet::new();
    dirty.extend(report.modified.iter().cloned());
    dirty.extend(report.deleted.iter().cloned());
    // Colliders and the --force discard set only consider incoming paths
    // INSIDE the sparse cone: the sync never materializes (or deletes)
    // outside it, so an untracked file at an out-of-cone incoming path is
    // not something this sync would touch — deleting it under --force would
    // discard a file the sync has no business with.
    let incoming_paths: BTreeSet<&str> = incoming
        .iter()
        .map(|(p, _)| p.as_str())
        .filter(|p| under_sparse(&state.sparse, p))
        .collect();
    let blocked: Vec<String> = dirty
        .iter()
        .filter(|path| incoming_paths.contains(path.as_str()))
        .cloned()
        .collect();
    let mut colliding: Vec<String> = Vec::new();
    for (norm, raws) in &report.folds {
        if incoming_paths.contains(norm.as_str()) {
            colliding.extend(raws.iter().cloned());
        }
    }
    for norm in &report.untracked {
        if incoming_paths.contains(norm.as_str()) {
            colliding.push(report.raw_of_untracked[norm].clone());
        }
    }
    colliding.sort();
    colliding.dedup();

    if (!blocked.is_empty() || !colliding.is_empty()) && !force {
        let blocked_list = blocked.join(", ");
        let collide_list = colliding.join(", ");
        return Err(VerbError::Http(crate::client::HttpError::Envelope {
            status: 409,
            code: "dirty_working_copy".into(),
            message: format!(
                "refusing to sync (nothing changed): local edits on incoming paths: \
                 [{blocked_list}]; colliding untracked: [{collide_list}]; \
                 use --force to discard them"
            ),
            details: None,
            retry_after: None,
        }));
    }

    // Target tree metadata for the incoming add/mod paths (sparse-filtered;
    // paths outside the sparse prefixes stay unmaterialized by design).
    let rootv = client.get_json(
        &format!(
            "/v1/repos/{}/changesets/{}/root",
            crate::client::uri_segment(&repo),
            target.to_hex()
        ),
        &[],
    )?;
    let target_root = rootv["root_tree_id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed root tree id".into()))?;
    // A cone prefix the target tree no longer contains (the server deleted
    // its whole directory) is an EMPTY subtree: the incoming dels below
    // remove the indexed files under it and the sync completes, instead of
    // failing forever on normal history.
    let target_walk =
        walk_tree_allowing_missing_prefixes(&client, &repo, &target_root, &state.sparse)?;
    let target_files = target_walk.files;
    let by_path: BTreeMap<&str, &_> = target_files.iter().map(|f| (f.path.as_str(), f)).collect();

    let mut to_fetch: Vec<Id> = Vec::new();
    let mut adds: Vec<(String, Id)> = Vec::new();
    let mut unavailable: Vec<String> = Vec::new();
    for (path, kind) in &incoming {
        if !under_sparse(&state.sparse, path) || kind == "del" {
            continue;
        }
        if target_walk.redacted.iter().any(|r| r == path) {
            // The target tree masks this path (the blob is tombstoned in
            // this repo): unavailable by policy — the sync proceeds
            // without it and reports it.
            unavailable.push(path.clone());
            continue;
        }
        let file = by_path.get(path.as_str()).ok_or_else(|| {
            VerbError::Local(format!(
                "incoming path {path:?} is missing from the target tree"
            ))
        })?;
        to_fetch.push(file.blob);
        adds.push((path.clone(), file.blob));
    }
    unavailable.sort();
    unavailable.dedup();
    let blobs = fetch_blobs_verified(&client, &repo, &to_fetch)?;

    // Every incoming blob is fetched and hash-verified, so --force can now
    // discard exactly the colliders the gate named — a failed fetch above
    // left the working copy completely untouched.
    for raw in &colliding {
        remove_and_prune(root, raw)
            .map_err(|e| VerbError::Local(format!("removing colliding untracked {raw:?}: {e}")))?;
    }

    // Apply, then update the index and state together at the end.
    for (path, blob) in &adds {
        let bytes = &blobs[blob];
        let file = by_path[path.as_str()];
        let row = materialize(root, file, bytes)?;
        index.rows.insert(path.clone(), row);
    }
    for (path, kind) in &incoming {
        // Belt and braces: only a tracked path can be deleted from disk —
        // the gate above already refused any untracked file at an incoming
        // del path, so a tracked-absent path here has nothing of ours.
        if kind != "del" || !under_sparse(&state.sparse, path) || !index.rows.contains_key(path) {
            continue;
        }
        remove_and_prune(root, path)
            .map_err(|e| VerbError::Local(format!("removing {path:?}: {e}")))?;
        index.rows.remove(path);
    }
    let new_state = State {
        head: target.to_hex(),
        base: state.base,
        sparse: state.sparse,
        server_repo: repo,
    };
    wc.save_state(&new_state)?;
    wc.save_index(&index)?;

    // Output honesty: only paths this sync actually applied (cone-included)
    // count as applied — an out-of-cone incoming path was NOT applied here
    // and must not pose as one, and a redacted path was reported
    // unavailable, never applied.
    let applied: Vec<Value> = incoming
        .iter()
        .filter(|(p, _)| under_sparse(&new_state.sparse, p) && !unavailable.contains(p))
        .map(|(path, kind)| json!({ "path": path, "kind": kind }))
        .collect();
    Ok(json!({
        "up_to_date": false,
        "head": target.to_hex(),
        "applied": applied,
        "unavailable": unavailable,
    }))
}
