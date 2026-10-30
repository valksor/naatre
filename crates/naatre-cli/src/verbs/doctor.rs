//! `naatre doctor [--rebuild]` — index consistency checking and the
//! corruption recovery path.
//!
//! Without `--rebuild`: every indexed file is checked on disk (exists,
//! stat fields match) and every recorded blob is checked server-side via
//! the batched `/objects/present` call. Findings are printed; a clean index
//! exits 0, findings exit 1.
//!
//! `--rebuild`: rescan the disk, rehash every file's content
//! (`object_id(Blob)` is ground truth), and rewrite the index from that.
//! Rows for files no longer on disk cannot be recovered (they were only in
//! the index) — the rebuilt index covers exactly what exists.

use std::path::Path;

use naatre_core::ids::{Id, Kind, object_id};
use serde_json::json;

use super::Net;
use crate::wc::index::{self, Index};
use crate::wc::{Wc, scan};

/// Runs doctor; prints findings and returns the exit code.
pub fn run(root: &Path, rebuild: bool, net: &Net) -> i32 {
    if rebuild {
        return rebuild_index(root);
    }
    match audit(root, net) {
        Ok(findings) if findings.is_empty() => {
            println!("doctor: index OK");
            0
        }
        Ok(findings) => {
            for f in findings {
                println!("doctor: {f}");
            }
            1
        }
        Err(e) => {
            println!("doctor: {e}");
            1
        }
    }
}

fn audit(root: &Path, net: &Net) -> Result<Vec<String>, String> {
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    let index = match Index::load(&wc.index_path()) {
        Ok(idx) => idx,
        Err(e) => return Err(e.to_string()),
    };
    let mut findings = Vec::new();

    // Disk half: exists + stat fields.
    for row in index.rows.values() {
        let path = root.join(&row.path);
        match std::fs::metadata(&path) {
            Err(e) => findings.push(format!("missing on disk: {} ({e})", row.path)),
            Ok(meta) if !index::stat_matches(row, &meta) => {
                findings.push(format!("stat drift: {}", row.path));
            }
            Ok(_) => {}
        }
    }

    // Server half: batched blob presence.
    let ids: Vec<Id> = index.rows.values().map(|r| Id(r.blob)).collect();
    let missing = blob_presence(root, &state.server_repo, &ids, net)?;
    for hex in &missing {
        let paths: Vec<String> = index
            .rows
            .values()
            .filter(|r| Id(r.blob).to_hex() == *hex)
            .map(|r| r.path.clone())
            .collect();
        findings.push(format!(
            "blob not on server: {} (referenced by {})",
            hex,
            paths.join(", ")
        ));
    }
    Ok(findings)
}

/// POSTs `/objects/present` in batches of 256; returns the missing ids.
fn blob_presence(root: &Path, repo: &str, ids: &[Id], net: &Net) -> Result<Vec<String>, String> {
    let wc = Wc::open(root)?;
    let client = wc.client(net)?;
    let mut missing = Vec::new();
    for chunk in ids.chunks(256) {
        let body = json!({ "ids": chunk.iter().map(Id::to_hex).collect::<Vec<_>>() });
        let (_status, v) = client
            .post_json(
                &format!(
                    "/v1/repos/{}/objects/present",
                    crate::client::uri_segment(repo)
                ),
                &body,
            )
            .map_err(|e| format!("server presence check failed: {e}"))?;
        for m in v["missing"].as_array().map_or(&[][..], Vec::as_slice) {
            if let Some(hex) = m.as_str() {
                missing.push(hex.to_string());
            }
        }
    }
    Ok(missing)
}

/// The recovery path: rescan + rehash + rewrite the index.
fn rebuild_index(root: &Path) -> i32 {
    match try_rebuild(root) {
        Ok(n) => {
            println!("doctor: rebuilt index from disk ({n} file(s))");
            0
        }
        Err(e) => {
            eprintln!("error: {e}");
            1
        }
    }
}

fn try_rebuild(root: &Path) -> Result<usize, String> {
    let wc = Wc::open(root)?;
    let files = scan::scan_disk(root).map_err(|e| format!("scanning {}: {e}", root.display()))?;
    let mut index = Index::default();
    for f in files {
        let bytes =
            std::fs::read(root.join(&f.raw)).map_err(|e| format!("reading {}: {e}", f.raw))?;
        let blob = object_id(Kind::Blob, &bytes);
        index.rows.insert(
            f.norm.clone(),
            index::Row::from_stat(f.norm, blob.0, &f.meta),
        );
    }
    let count = index.rows.len();
    wc.save_index(&index)?;
    Ok(count)
}
