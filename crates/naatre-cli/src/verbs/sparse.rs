//! `naatre sparse add|remove|list` — the sparse prefix list.
//!
//! The list in `state.json` is an INCLUDE list: empty means the full tree,
//! non-empty means only the listed prefixes are materialized.
//! - `add P` fetches and writes the files under `P` at head, then adds `P`.
//! - `remove P` deletes the indexed files under `P` from disk, drops their
//!   index rows, and removes `P` from the list. Untracked files under the
//!   prefix are left alone (preserve-everything); removing the LAST prefix
//!   is refused (an empty list would mean "full checkout" again).

use std::path::Path;

use naatre_core::path;

use super::{Net, VerbError};
use crate::wc::fetch::{fetch_blobs_verified, materialize, walk_tree};
use crate::wc::{State, Wc, remove_and_prune, under_prefix};

pub enum Action {
    Add(String),
    Remove(String),
    List,
}

/// Runs the sparse verb; returns the exit code.
pub fn run(root: &Path, action: Action, net: &Net) -> i32 {
    match try_run(root, action, net) {
        Ok(()) => 0,
        Err(e) => e.report(),
    }
}

fn try_run(root: &Path, action: Action, net: &Net) -> Result<(), VerbError> {
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    match action {
        Action::List => {
            if state.sparse.is_empty() {
                println!("(full checkout — no sparse prefixes)");
            } else {
                for p in &state.sparse {
                    println!("{p}");
                }
            }
            Ok(())
        }
        Action::Add(prefix) => add(root, &wc, state, &prefix, net),
        Action::Remove(prefix) => remove(root, &wc, state, &prefix),
    }
}

fn add(root: &Path, wc: &Wc, mut state: State, prefix: &str, net: &Net) -> Result<(), VerbError> {
    path::validate(prefix)
        .map_err(|e| VerbError::Usage(format!("sparse prefix {prefix:?}: {e}")))?;
    if state.sparse.is_empty() {
        return Err(VerbError::Local(
            "this working copy is full (no sparse prefixes); narrow it by re-cloning with --sparse"
                .into(),
        ));
    }
    if state.sparse.iter().any(|p| p == prefix) {
        println!("sparse: {prefix} is already listed");
        return Ok(());
    }
    let client = wc.client(net)?;
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
    let ids: Vec<naatre_core::ids::Id> = files.iter().map(|f| f.blob).collect();
    let blobs = fetch_blobs_verified(&client, &state.server_repo, &ids)?;
    let mut written = 0usize;
    for file in &files {
        if index.rows.contains_key(&file.path) {
            continue; // already materialized
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
    println!("sparse: added {prefix} ({written} file(s) materialized)");
    Ok(())
}

fn remove(root: &Path, wc: &Wc, mut state: State, prefix: &str) -> Result<(), VerbError> {
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
    println!("sparse: removed {prefix} ({removed} file(s) dropped from disk and index)");
    Ok(())
}
