//! `naatre sync [--force]` — advance the working copy to the server's
//! `main`.
//!
//! Dirty check FIRST (the preserve-everything rule): any locally modified
//! or deleted tracked file, or any untracked/normalization-folded disk file
//! colliding with an incoming path, refuses with exit 2 and
//! `dirty_working_copy`, listing BOTH lists. `--force` discards exactly the
//! incoming-colliding files (deletes untracked colliders, resets modified
//! ones) and proceeds; local edits to non-colliding files survive.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;

use naatre_core::ids::Id;

use super::{Net, VerbError};
use crate::wc::fetch::{fetch_blobs_verified, materialize, walk_tree};
use crate::wc::{State, Wc, remove_and_prune, under_sparse};

/// Runs sync; prints the outcome and returns the exit code.
pub fn run(root: &Path, force: bool, net: &Net) -> i32 {
    match try_run(root, force, net) {
        Ok(()) => 0,
        Err(e) => e.report(),
    }
}

fn try_run(root: &Path, force: bool, net: &Net) -> Result<(), VerbError> {
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
        println!("up to date");
        return Ok(());
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

    // The dirty gate: classify disk vs index, intersect with incoming.
    let report = crate::wc::scan::classify(root, &index)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    let mut dirty: BTreeSet<String> = BTreeSet::new();
    dirty.extend(report.modified.iter().cloned());
    dirty.extend(report.deleted.iter().cloned());
    let incoming_paths: BTreeSet<&String> = incoming
        .iter()
        .filter(|(_, kind)| kind != "del")
        .map(|(path, _)| path)
        .collect();
    let mut colliding: Vec<String> = Vec::new();
    for (norm, raws) in &report.folds {
        if incoming_paths.contains(norm) {
            colliding.extend(raws.iter().cloned());
        }
    }
    for norm in &report.untracked {
        if incoming_paths.contains(norm) {
            colliding.push(report.raw_of_untracked[norm].clone());
        }
    }
    colliding.sort();
    colliding.dedup();

    if (!dirty.is_empty() || !colliding.is_empty()) && !force {
        let dirty_list = dirty.iter().cloned().collect::<Vec<_>>().join(", ");
        let collide_list = colliding.join(", ");
        return Err(VerbError::Http(crate::client::HttpError::Envelope {
            status: 409,
            code: "dirty_working_copy".into(),
            message: format!(
                "refusing to sync (nothing changed): local changes: [{dirty_list}]; \
                 colliding untracked: [{collide_list}]; use --force to discard them"
            ),
        }));
    }
    for raw in &colliding {
        remove_and_prune(root, raw)
            .map_err(|e| VerbError::Local(format!("removing colliding untracked {raw:?}: {e}")))?;
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
    let target_files = walk_tree(&client, &repo, &target_root, &state.sparse)?;
    let by_path: BTreeMap<&str, &_> = target_files.iter().map(|f| (f.path.as_str(), f)).collect();

    let mut to_fetch: Vec<Id> = Vec::new();
    let mut adds: Vec<(String, Id)> = Vec::new();
    for (path, kind) in &incoming {
        if !under_sparse(&state.sparse, path) || kind == "del" {
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
    let blobs = fetch_blobs_verified(&client, &repo, &to_fetch)?;

    // Apply, then update the index and state together at the end.
    for (path, blob) in &adds {
        let bytes = &blobs[blob];
        let file = by_path[path.as_str()];
        let row = materialize(root, file, bytes)?;
        index.rows.insert(path.clone(), row);
    }
    for (path, kind) in &incoming {
        if kind != "del" || !under_sparse(&state.sparse, path) {
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

    println!("synced: {}", target.to_hex());
    for (path, kind) in &incoming {
        let c = match kind.as_str() {
            "add" => "A",
            "del" => "D",
            _ => "M",
        };
        println!("{c} {path}");
    }
    Ok(())
}
