//! Fetching trees and blobs from the server for materialization.
//!
//! Server data is untrusted here: every file path is validated against the
//! core path grammar before anything is written, and every blob's bytes are
//! hash-verified client-side with `object_id(Blob)` — a mismatch aborts the
//! verb, nothing partial is kept (clone works in a staging directory;
//! sync/sparse write per-file through temp+rename).

use std::collections::{HashMap, HashSet, VecDeque};
use std::path::Path;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::path;
use serde_json::json;

use crate::client::{Client, HttpError};
use crate::wc::index::Row;

/// A fetch/verify failure: either HTTP or a local data-integrity error.
#[derive(Debug)]
pub enum FetchError {
    Http(HttpError),
    Local(String),
}

impl From<HttpError> for FetchError {
    fn from(e: HttpError) -> Self {
        FetchError::Http(e)
    }
}

impl std::fmt::Display for FetchError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            FetchError::Http(e) => write!(f, "{e}"),
            FetchError::Local(m) => f.write_str(m),
        }
    }
}

/// One file of the (sparse-filtered) server tree.
#[derive(Clone, Debug)]
pub struct TreeFile {
    pub path: String,
    pub blob: Id,
    pub size: u64,
    pub exec: bool,
}

/// Walks the server tree rooted at `root`, fetching only the trees on the
/// paths to the sparse `prefixes` (an empty list walks everything — full
/// clone is allowed at this size). Blob ids arrive via each parent's entry
/// listing: a chain of custody from the changeset's declared root id.
///
/// Errors when a prefix does not exist in the tree (as a dir or file).
pub fn walk_tree(
    client: &Client,
    repo: &str,
    root: &Id,
    prefixes: &[String],
) -> Result<Vec<TreeFile>, FetchError> {
    for p in prefixes {
        path::validate(p).map_err(|e| FetchError::Local(format!("sparse prefix {p:?}: {e}")))?;
    }
    let mut files = Vec::new();
    let mut visited: HashSet<Id> = HashSet::new();
    let mut descended: HashSet<String> = HashSet::new();
    let mut queue: VecDeque<(String, Id)> = VecDeque::from([(String::new(), *root)]);
    while let Some((dir, tree_id)) = queue.pop_front() {
        if !visited.insert(tree_id) {
            continue;
        }
        let v = client.get_json(&format!("/v1/repos/{repo}/trees/{}", tree_id.to_hex()), &[])?;
        let entries = v["entries"]
            .as_array()
            .ok_or_else(|| FetchError::Local("server sent a malformed tree listing".into()))?;
        for e in entries {
            let Some(name) = e["name"].as_str() else {
                return Err(FetchError::Local("tree entry without a name".into()));
            };
            let kind = e["kind"].as_str().unwrap_or("");
            let Some(oid) = e["object_id"].as_str().and_then(Id::from_hex) else {
                return Err(FetchError::Local(format!(
                    "tree entry {name:?} has a malformed object id"
                )));
            };
            let full = if dir.is_empty() {
                name.to_string()
            } else {
                format!("{dir}/{name}")
            };
            if kind == "file" {
                if under(prefixes, &full) {
                    // Grammar enforcement of untrusted server data.
                    path::validate(&full)
                        .map_err(|e| FetchError::Local(format!("server path {full:?}: {e}")))?;
                    files.push(TreeFile {
                        path: full,
                        blob: oid,
                        size: e["size"].as_u64().unwrap_or(0),
                        exec: e["exec"].as_bool().unwrap_or(false),
                    });
                }
            } else {
                // Descend when the dir is ON the path to a prefix (prefix
                // under dir) or UNDER a prefix (dir inside the prefix's
                // subtree — its files materialize).
                let descend = prefixes.is_empty()
                    || prefixes.iter().any(|p| {
                        p == &full
                            || p.starts_with(&format!("{full}/"))
                            || full.starts_with(&format!("{p}/"))
                    });
                if descend {
                    descended.insert(full.clone());
                    queue.push_back((full, oid));
                }
            }
        }
    }
    for p in prefixes {
        let found = descended.contains(p) || files.iter().any(|f| f.path == *p);
        if !found {
            return Err(FetchError::Local(format!(
                "sparse prefix {p:?} does not exist in the tree"
            )));
        }
    }
    Ok(files)
}

fn under(prefixes: &[String], path: &str) -> bool {
    crate::wc::under_sparse(prefixes, path)
}

/// Fetches blobs by id in batch, decoding and hash-verifying EVERY payload
/// client-side (`object_id(Blob)` must equal the requested id). Any missing
/// id or mismatch aborts with a [`FetchError::Local`].
pub fn fetch_blobs_verified(
    client: &Client,
    repo: &str,
    ids: &[Id],
) -> Result<HashMap<Id, Vec<u8>>, FetchError> {
    let mut out = HashMap::new();
    for chunk in ids.chunks(256) {
        let body = json!({ "ids": chunk.iter().map(Id::to_hex).collect::<Vec<_>>() });
        let (_status, v) = client.post_json(&format!("/v1/repos/{repo}/objects/get"), &body)?;
        for obj in v["objects"].as_array().map_or(&[][..], Vec::as_slice) {
            let Some(hex) = obj["id"].as_str().and_then(Id::from_hex) else {
                return Err(FetchError::Local(
                    "batch fetch returned a malformed object id".into(),
                ));
            };
            let Some(data_b64) = obj["data_b64"].as_str() else {
                return Err(FetchError::Local(format!(
                    "batch fetch returned no payload for {}",
                    hex.to_hex()
                )));
            };
            let bytes = STANDARD.decode(data_b64.as_bytes()).map_err(|e| {
                FetchError::Local(format!("payload of {} is not base64: {e}", hex.to_hex()))
            })?;
            verify(&hex, &bytes)?;
            out.insert(hex, bytes);
        }
        let missing: Vec<String> = v["missing"]
            .as_array()
            .map_or(&[][..], Vec::as_slice)
            .iter()
            .filter_map(|m| m.as_str().map(str::to_string))
            .collect();
        if !missing.is_empty() {
            return Err(FetchError::Local(format!(
                "server cannot serve {} object(s): {}",
                missing.len(),
                missing.join(", ")
            )));
        }
    }
    Ok(out)
}

/// The pinned client-side integrity gate: payload must hash to its id.
pub fn verify(id: &Id, bytes: &[u8]) -> Result<(), FetchError> {
    let actual = object_id(Kind::Blob, bytes);
    if actual != *id {
        return Err(FetchError::Local(format!(
            "hash mismatch: object {} does not hash to its id (got {})",
            id.to_hex(),
            actual.to_hex()
        )));
    }
    Ok(())
}

/// Materializes one fetched file into the checkout (grammar-checked again
/// here, so every caller is covered) and returns its stat row.
pub fn materialize(root: &Path, file: &TreeFile, bytes: &[u8]) -> Result<Row, FetchError> {
    path::validate(&file.path)
        .map_err(|e| FetchError::Local(format!("server path {:?}: {}", file.path, e)))?;
    verify(&file.blob, bytes)?;
    crate::wc::write_checkout_file(root, &file.path, bytes, file.blob.0, file.exec)
        .map_err(|e| FetchError::Local(format!("writing {}: {e}", file.path)))
}
