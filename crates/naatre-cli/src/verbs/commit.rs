//! `naatre commit -m <msg> [--author <a>]` — the git-add-all commit.
//!
//! Collects the change set from a status scan: modified (content hash
//! differs from the index) and deleted tracked paths, PLUS every untracked
//! file under the sparse prefixes — add-all semantics, documented in the
//! plan. Paths are normalized to NFC and sorted by path bytes; changed
//! blobs are PUT first (the id is the address, computed client-side), then
//! the changeset POST references them. An empty change set prints
//! "nothing to commit" and exits 0 — the server's `empty_changeset`
//! rejection never fires from this verb.

use std::path::Path;

use naatre_core::ids::{Id, Kind, object_id};
use serde_json::json;

use super::{Net, VerbError};
use crate::wc::index::Row;
use crate::wc::{Wc, default_author, now_ns, under_sparse};

/// One pending change gathered from the scan.
struct Pending {
    path: String,
    raw: String,
    op: &'static str,
    blob: Option<Id>,
    size: Option<u64>,
    exec: Option<bool>,
}

/// Runs commit; prints the created changeset as a JSON line and returns the
/// exit code.
pub fn run(root: &Path, message: &str, author: Option<&str>, net: &Net) -> i32 {
    match try_run(root, message, author, net) {
        Ok(()) => 0,
        Err(e) => e.report(),
    }
}

fn try_run(root: &Path, message: &str, author: Option<&str>, net: &Net) -> Result<(), VerbError> {
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    let mut index = wc.load_index()?;
    let client = wc.client(net)?;
    let head = Id::from_hex(&state.head)
        .ok_or_else(|| VerbError::Local("state.json head is not a valid changeset id".into()))?;

    // Gather the change set from the same classification status uses.
    let report = crate::wc::scan::classify(root, &index)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    let raw_of = |path: &str| -> String {
        report
            .raw_of
            .get(path)
            .cloned()
            .unwrap_or_else(|| path.to_string())
    };
    let mut pending: Vec<Pending> = Vec::new();
    for path in &report.deleted {
        pending.push(Pending {
            path: path.clone(),
            raw: raw_of(path),
            op: "del",
            blob: None,
            size: None,
            exec: None,
        });
    }
    for path in &report.modified {
        let raw = raw_of(path);
        let (blob, size, exec) = read_file(root, &raw)?;
        pending.push(Pending {
            path: path.clone(),
            raw,
            op: "mod",
            blob: Some(blob),
            size: Some(size),
            exec: Some(exec),
        });
    }
    for path in &report.untracked {
        if !under_sparse(&state.sparse, path) {
            continue; // add-all applies under the sparse prefixes only
        }
        let raw = raw_of(path);
        let (blob, size, exec) = read_file(root, &raw)?;
        pending.push(Pending {
            path: path.clone(),
            raw,
            op: "add",
            blob: Some(blob),
            size: Some(size),
            exec: Some(exec),
        });
    }
    // Fold duplicates (a modified path can also appear via a fold) and
    // order strictly by path bytes — the server rejects anything else and
    // the changeset id covers this order.
    pending.sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));
    pending.dedup_by(|a, b| a.path == b.path);
    if pending.is_empty() {
        println!("nothing to commit");
        return Ok(());
    }

    let repo = &state.server_repo;

    // Blobs first: raw PUT, hash computed client-side (the id IS the
    // address). 201 first store, 200 idempotent re-PUT — both fine.
    let mut uploaded: Vec<Id> = Vec::new();
    for p in &pending {
        let Some(blob) = p.blob else { continue };
        if uploaded.contains(&blob) {
            continue;
        }
        let bytes = std::fs::read(root.join(&p.raw))
            .map_err(|e| VerbError::Local(format!("reading {}: {e}", p.path)))?;
        client.put_bytes(
            &format!(
                "/v1/repos/{}/objects/{}",
                crate::client::uri_segment(repo),
                blob.to_hex()
            ),
            &bytes,
        )?;
        uploaded.push(blob);
    }

    let changes: Vec<serde_json::Value> = pending
        .iter()
        .map(|p| {
            json!({
                "path": p.path,
                "op": p.op,
                "blob": p.blob.as_ref().map(Id::to_hex),
                "size": p.size,
                "exec": p.exec,
            })
        })
        .collect();
    let body = json!({
        "parents": [head.to_hex()],
        "author": author.map(str::to_string).unwrap_or_else(default_author),
        "message": message,
        "timestamp_ns": now_ns(),
        "changes": changes,
    });
    let (_status, created) = client.post_json(
        &format!("/v1/repos/{}/changesets", crate::client::uri_segment(repo)),
        &body,
    )?;
    let new_head = created["id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed changeset id".into()))?;

    // Update state head and the index rows for committed paths.
    for p in &pending {
        match p.op {
            "del" => {
                index.rows.remove(&p.path);
            }
            _ => {
                let meta = std::fs::metadata(root.join(&p.raw))
                    .map_err(|e| VerbError::Local(format!("stating {}: {e}", p.path)))?;
                let blob = p.blob.expect("add/mod carries a blob");
                index.rows.insert(
                    p.path.clone(),
                    Row::from_stat(p.path.clone(), blob.0, &meta),
                );
            }
        }
    }
    let new_state = crate::wc::State {
        head: new_head.to_hex(),
        base: state.base,
        sparse: state.sparse,
        server_repo: state.server_repo,
    };
    wc.save_state(&new_state)?;
    wc.save_index(&index)?;

    println!(
        "{}",
        json!({
            "id": created["id"],
            "root_tree_id": created["root_tree_id"],
            "generation": created["generation"],
        })
    );
    Ok(())
}

/// Reads a pending file's bytes and derives (blob, size, exec).
fn read_file(root: &Path, raw: &str) -> Result<(Id, u64, bool), VerbError> {
    use std::os::unix::fs::MetadataExt as _;
    let bytes = std::fs::read(root.join(raw))
        .map_err(|e| VerbError::Local(format!("reading {raw:?}: {e}")))?;
    let meta = std::fs::metadata(root.join(raw))
        .map_err(|e| VerbError::Local(format!("stating {raw:?}: {e}")))?;
    let exec = meta.mode() & 0o111 != 0;
    Ok((object_id(Kind::Blob, &bytes), bytes.len() as u64, exec))
}
