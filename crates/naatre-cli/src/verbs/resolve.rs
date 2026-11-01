//! `naatre resolve <path>` — the M12 resolution verb: commits the resolved
//! content of one conflicted path as an explicit resolution changeset
//! referencing the working copy's head (the conflicted node in the pinned
//! flow: sync/land moved the head onto the conflicted node, whose markers
//! are materialized on disk). The resolution is its own changeset — never
//! a silent rewrite of the conflicted node — and landing it advances main
//! past the conflict (linear history).
//!
//! The path must be one `status` marks conflicted (state.json's list).
//! The disk content must not itself be a marker file any more — editing
//! the resolved content is the user's half of the contract; the verb
//! refuses markers-as-resolution. Other local edits stay local: the
//! changeset carries exactly the one path. A conflicted mark cleared here
//! is gone from state; a subsequent `naatre land` / `naatre commit` is a
//! clean settlement.

use std::path::Path;

use naatre_core::conflict;
use naatre_core::ids::{Id, Kind, object_id};
use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::client::HttpError;
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

    // Contract: the verb resolves conflicted paths, not arbitrary files.
    if !state.conflicted.iter().any(|p| p == &path) {
        return Err(VerbError::Usage(format!(
            "{path:?} is not a conflicted path in this working copy (see: naatre status)"
        )));
    }
    // The raw disk name the scanner records (raw may be an NFD twin of
    // the NFC indexed path on a folding filesystem — commit's rule).
    let report = crate::wc::scan::classify(root, &index)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    let raw = report
        .raw_of
        .get(&path)
        .cloned()
        .unwrap_or_else(|| path.clone());
    let bytes = std::fs::read(root.join(&raw)).map_err(|e| {
        VerbError::Local(format!(
            "reading {raw:?}: {e} — the resolved content must be on disk; \
             restore the file (or delete the conflict with a commit) first"
        ))
    })?;
    // The user's half: the resolution content must not be markers again.
    if conflict::marker_shaped(&bytes) {
        return Err(VerbError::Http(HttpError::Envelope {
            status: 409,
            code: "conflict_markers_present".into(),
            message: format!(
                "{path} still contains conflict markers — edit the resolved content \
                 first, then run `naatre resolve {path}`"
            ),
            details: None,
            retry_after: None,
        }));
    }
    let blob = object_id(Kind::Blob, &bytes);
    let size = bytes.len() as u64;
    let exec = {
        use std::os::unix::fs::MetadataExt as _;
        std::fs::metadata(root.join(&raw))
            .map_err(|e| VerbError::Local(format!("stating {raw:?}: {e}")))?
            .mode()
            & 0o111
            != 0
    };

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
