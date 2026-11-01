//! `naatre resolve <path>` — the M12 resolution verb: commits the resolved
//! content of one conflicted path as an explicit resolution changeset
//! referencing the working copy's head (the conflicted node in the pinned
//! flow: sync/land moved the head onto the conflicted node, whose markers
//! are materialized on disk). The resolution is its own changeset — never
//! a silent rewrite of the conflicted node — and landing it advances main
//! past the conflict (linear history).
//!
//! The state mark (state.json's list) is a STATUS INDEX, not a
//! permission (m12b-fix): the verb proceeds for a path whose on-disk
//! bytes are marker-shaped — with or without a mark — and for a marked
//! path whose content the user already edited. When the bytes it commits
//! are themselves marker-shaped (a conflict the index lost track of, or
//! an explicit keep-the-markers resolution), it says so on stderr and
//! records them as the explicit resolution — the ordinary-commit gate
//! stays the fence for non-explicit flows. Other local edits stay local:
//! the changeset carries exactly the one path. A conflicted mark cleared
//! here is gone from state; a subsequent `naatre land` / `naatre commit`
//! is a clean settlement.

use std::path::Path;

use naatre_core::conflict;
use naatre_core::ids::Id;
use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::wc::index::Row;
use crate::wc::{Wc, default_author, now_ns};

/// Runs resolve; prints the resolution changeset (the commit verb's JSON
/// convention) and returns the exit code.
pub fn run(root: &Path, path: &str, net: &Net) -> i32 {
    match value(root, path, net) {
        Ok(v) => {
            println!(
                "{}",
                json!({
                    "resolved": v["resolved"],
                    "id": v["id"],
                    "root_tree_id": v["root_tree_id"],
                    "generation": v["generation"],
                })
            );
            0
        }
        Err(e) => e.report(),
    }
}

/// The resolve result: `{resolved, id, root_tree_id, generation}` (the
/// created resolution changeset; no HTTP analog).
pub fn value(root: &Path, path: &str, net: &Net) -> Result<Value, VerbError> {
    let path = crate::wc::nfc(path);
    let wc = Wc::open(root)?;
    let mut state = wc.load_state()?;
    let mut index = wc.load_index()?;
    let client = wc.client(net)?;
    let head = Id::from_hex(&state.head)
        .ok_or_else(|| VerbError::Local("state.json head is not a valid changeset id".into()))?;

    // The raw disk name the scanner records (raw may be an NFD twin of
    // the NFC indexed path on a folding filesystem — commit's rule).
    let report = crate::wc::scan::classify(root, &index)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    let raw = report
        .raw_of
        .get(&path)
        .cloned()
        .unwrap_or_else(|| path.clone());
    let (blob, size, exec, bytes) = super::read_wc_file(root, &raw).map_err(|e| match e {
        VerbError::Local(m) => VerbError::Local(format!(
            "{m} — the resolved content must be on disk; \
             restore the file (or delete the conflict with a commit) first"
        )),
        other => other,
    })?;
    // The permission gate is the CONTENT, not the index (m12b-fix): the
    // path resolves when its disk bytes are marker-shaped — with or
    // without a mark — or when the mark is present (the edit-first flow).
    // The mark applies status's cone-membership filter: a mark on a path
    // the index no longer tracks (dropped by a sparse remove) is stale,
    // exactly as status refuses to print it.
    let marked = state.conflicted.iter().any(|p| p == &path) && index.rows.contains_key(&path);
    let marker_shaped = conflict::marker_shaped(&bytes);
    if !marked && !marker_shaped {
        return Err(VerbError::Usage(format!(
            "{path:?} is not a conflicted path in this working copy (see: naatre status)"
        )));
    }
    // The resolution commits the current disk bytes, whatever they are.
    // Marker-shaped content is said out loud: it enters history as the
    // explicit resolution (the ordinary-commit gate stays the fence for
    // non-explicit flows; a server-side land check is the recorded
    // follow-up).
    if marker_shaped {
        eprintln!(
            "warning: {path} still carries conflict markers; recording them as the \
             explicit resolution (edit the content and re-run first if that is \
             not what you want)"
        );
    }

    let repo = &state.server_repo;
    // The blob first (the id is the address, computed client-side).
    client
        .put_bytes(
            &format!(
                "/v1/repos/{}/objects/{}",
                crate::client::uri_segment(repo),
                blob.to_hex()
            ),
            &bytes,
        )
        .map_err(VerbError::Http)?;
    // The resolution changeset: parent = the working copy's head (the
    // conflicted node in the pinned sync-then-resolve flow), content =
    // the resolved bytes at the one path.
    let body = json!({
        "parents": [head.to_hex()],
        "author": default_author(),
        "message": format!("resolve conflict at {path}"),
        "timestamp_ns": now_ns(),
        "changes": [{
            "path": path,
            "op": "mod",
            "blob": blob.to_hex(),
            "size": size,
            "exec": exec,
        }],
    });
    let (_status, created) = client
        .post_json(
            &format!("/v1/repos/{}/changesets", crate::client::uri_segment(repo)),
            &body,
        )
        .map_err(VerbError::Http)?;
    let new_head = created["id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed changeset id".into()))?;

    // Head moves onto the resolution; the conflicted mark clears; the
    // index row tracks the resolved blob.
    let meta = std::fs::metadata(root.join(&raw))
        .map_err(|e| VerbError::Local(format!("stating {raw:?}: {e}")))?;
    index
        .rows
        .insert(path.clone(), Row::from_stat(path.clone(), blob.0, &meta));
    state.conflicted.retain(|p| p != &path);
    let new_state = crate::wc::State {
        head: new_head.to_hex(),
        base: state.base,
        sparse: state.sparse,
        server_repo: state.server_repo,
        conflicted: state.conflicted,
    };
    wc.save_state(&new_state)?;
    wc.save_index(&index)?;

    Ok(json!({
        "resolved": path,
        "id": created["id"],
        "root_tree_id": created["root_tree_id"],
        "generation": created["generation"],
    }))
}
