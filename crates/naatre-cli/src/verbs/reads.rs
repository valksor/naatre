//! The read verbs: `cat`, `blame`, `ls`, `diff`. Every one takes `--json`
//! with a pinned shape (status's shape lives in [`crate::wc::scan::Report`];
//! ls mirrors the server's tree-entry shape; diff mirrors the server's
//! diff-entry shape; blame's shape is pinned in [`blame`]). `--at` accepts
//! a changeset id or a ref name; the default for cat/blame/ls/diff's server
//! mode is head.

use std::collections::BTreeMap;
use std::io::Write as _;
use std::os::unix::fs::MetadataExt as _;
use std::path::Path;

use naatre_core::ids::{Id, Kind, is_hex_id, object_id};
use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::client::{Client, uri_segment};
use crate::wc::fetch::walk_tree_allowing_missing_prefixes;
use crate::wc::{Wc, nfc, under_sparse};

/// A working copy resolved for reading: client + repo + head changeset.
struct Reader {
    client: Client,
    repo: String,
    head: Id,
}

fn open_reader(root: &Path, net: &Net) -> Result<Reader, VerbError> {
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    let client = wc.client(net)?;
    let head = Id::from_hex(&state.head)
        .ok_or_else(|| VerbError::Local("state.json head is not a valid changeset id".into()))?;
    Ok(Reader {
        client,
        repo: state.server_repo,
        head,
    })
}

/// `--at` resolution: a 64-hex id passes through; anything else is a ref
/// name resolved via `GET /refs/{name}`.
fn resolve_at(reader: &Reader, at: Option<&str>) -> Result<Id, VerbError> {
    match at {
        None => Ok(reader.head),
        Some(a) if is_hex_id(a) => {
            Id::from_hex(a).ok_or_else(|| VerbError::Usage(format!("bad changeset id {a:?}")))
        }
        Some(refname) => {
            let v = reader.client.get_json(
                &format!(
                    "/v1/repos/{}/refs/{}",
                    uri_segment(&reader.repo),
                    uri_segment(refname)
                ),
                &[],
            )?;
            v["target"]
                .as_str()
                .and_then(Id::from_hex)
                .ok_or_else(|| VerbError::Local(format!("ref {refname:?} is unborn")))
        }
    }
}

/// `naatre cat <path> [--at REF|CS]` — raw bytes to stdout, binary-safe.
/// Every fetched blob is hash-verified client-side before a byte reaches
/// stdout (corrupt data is a clean error, never garbage output).
pub fn cat(root: &Path, path: &str, at: Option<&str>, net: &Net) -> i32 {
    match cat_bytes(root, path, at, net) {
        Ok((_blob, _size, _exec, bytes)) => match std::io::stdout().write_all(&bytes) {
            Ok(()) => 0,
            Err(e) => VerbError::Local(format!("writing to stdout: {e}")).report(),
        },
        Err(e) => e.report(),
    }
}

/// The cat core: resolves `path` (NFC) at `at` (default head), hash-verifies
/// the blob, and returns (blob id, size, exec, raw bytes). The CLI writes
/// the bytes; the MCP `cat` tool renders them as text (binary flagged).
pub fn cat_bytes(
    root: &Path,
    path: &str,
    at: Option<&str>,
    net: &Net,
) -> Result<(Id, u64, bool, Vec<u8>), VerbError> {
    let reader = open_reader(root, net)?;
    let cs = resolve_at(&reader, at)?;
    let path = nfc(path);
    let fv = reader.client.get_json(
        &format!("/v1/repos/{}/file", uri_segment(&reader.repo)),
        &[("at", cs.to_hex().as_str()), ("path", path.as_str())],
    )?;
    let blob = fv["blob_id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed blob id".into()))?;
    let bytes = reader
        .client
        .get_bytes(&format!(
            "/v1/repos/{}/objects/{}",
            uri_segment(&reader.repo),
            blob.to_hex()
        ))
        .map_err(VerbError::Http)?;
    crate::wc::fetch::verify(&blob, &bytes).map_err(VerbError::from)?;
    let size = fv["size"].as_u64().unwrap_or(bytes.len() as u64);
    let exec = fv["exec"].as_bool().unwrap_or(false);
    Ok((blob, size, exec, bytes))
}

/// `naatre blame <path> [--at REF|CS]` — per-line attribution from the
/// server's derived blame. Pinned human line format:
/// `<8-hex cs> <author> | <line>` (the first 8 hex of the changeset id).
/// `--json` prints exactly `{"lines":[{cs,author,text}]}` with the full
/// 64-hex ids. A binary file is the server's structured empty result:
/// exit 0, `{"lines":[]}` on stdout under `--json` (nothing in the human
/// format), the stable code noted on stderr. A degraded (quarantined
/// chain) blame prints a stderr warning and exits 0.
/// The stderr warning for a degraded blame (pinned: names every quarantined
/// changeset; the verb still exits 0 — degraded results are results, not errors).
pub(crate) fn degraded_blame_warning(poisoned: &[&str]) -> String {
    format!(
        "warning: blame degraded by quarantined changeset(s): {} — lines below the boundary are attributed to the boundary changeset",
        poisoned.join(", ")
    )
}

pub fn blame(root: &Path, path: &str, at: Option<&str>, json_out: bool, net: &Net) -> i32 {
    match blame_value(root, path, at, net) {
        Ok(result) => {
            if result.binary {
                eprintln!("blame: {path}: binary content (blame_binary)");
            }
            if let Some(warning) = &result.degraded_warning {
                eprintln!("{warning}");
            }
            if json_out {
                println!("{}", json!({ "lines": result.lines }));
            } else {
                for l in result.lines.as_array().map_or(&[][..], Vec::as_slice) {
                    let cs8: String = l["cs"].as_str().unwrap_or("?").chars().take(8).collect();
                    let author = l["author"].as_str().unwrap_or("?");
                    let text = l["text"].as_str().unwrap_or("");
                    println!("{cs8} {author} | {text}");
                }
            }
            0
        }
        Err(e) => e.report(),
    }
}

/// The blame core's non-error payload: the attributed lines (the pinned
/// `--json` shape's array) plus the stderr notes the CLI surfaces (a binary
/// file blames as a structured EMPTY result; a degraded chain warns).
pub struct BlameResult {
    /// The `lines` array of the server's blame response.
    pub lines: Value,
    /// The file is not valid UTF-8: `lines` is empty.
    pub binary: bool,
    /// The degraded-blame warning text (names every quarantined changeset).
    pub degraded_warning: Option<String>,
}

/// The blame core: resolves `path` (NFC) at `at` (default head) and returns
/// the attributed lines with the binary/degraded notes.
pub fn blame_value(
    root: &Path,
    path: &str,
    at: Option<&str>,
    net: &Net,
) -> Result<BlameResult, VerbError> {
    let reader = open_reader(root, net)?;
    let cs = resolve_at(&reader, at)?;
    let path = nfc(path);
    let v = reader.client.get_json(
        &format!("/v1/repos/{}/blame", uri_segment(&reader.repo)),
        &[("at", cs.to_hex().as_str()), ("path", path.as_str())],
    )?;
    let lines = v["lines"].as_array().cloned().unwrap_or_default();
    let binary = v["binary"].as_bool() == Some(true);
    if binary {
        // The published shape: a binary blame is a structured EMPTY
        // result, not an error — the same `{"lines":[]}` an empty file
        // blames as.
        return Ok(BlameResult {
            lines: json!([]),
            binary: true,
            degraded_warning: None,
        });
    }
    let degraded_warning = if v["degraded"].as_bool() == Some(true) {
        let poisoned: Vec<&str> = v["poisoned_cs"]
            .as_array()
            .map(|a| a.iter().filter_map(|x| x.as_str()).collect())
            .unwrap_or_default();
        Some(degraded_blame_warning(&poisoned))
    } else {
        None
    };
    Ok(BlameResult {
        lines: Value::Array(lines),
        binary: false,
        degraded_warning,
    })
}

/// `naatre ls [path] [--at REF|CS]` — one tree node's entries.
pub fn ls(root: &Path, path: Option<&str>, at: Option<&str>, json_out: bool, net: &Net) -> i32 {
    match ls_value(root, path, at, net) {
        Ok(v) => {
            if json_out {
                println!("{v}");
            } else {
                for e in v["entries"].as_array().map_or(&[][..], Vec::as_slice) {
                    let name = e["name"].as_str().unwrap_or("?");
                    if e["kind"].as_str() == Some("dir") {
                        println!("{name}/");
                    } else {
                        println!("{name}");
                    }
                }
            }
            0
        }
        Err(e) => e.report(),
    }
}

/// The ls core: resolves the tree node at `path` (default: the root) under
/// `at` (default head) and returns `{"entries": [...]}` (the server's
/// tree-entry shape, as `ls --json` prints).
pub fn ls_value(
    root: &Path,
    path: Option<&str>,
    at: Option<&str>,
    net: &Net,
) -> Result<Value, VerbError> {
    let reader = open_reader(root, net)?;
    let cs = resolve_at(&reader, at)?;
    let rootv = reader.client.get_json(
        &format!(
            "/v1/repos/{}/changesets/{}/root",
            uri_segment(&reader.repo),
            cs.to_hex()
        ),
        &[],
    )?;
    let mut tree = rootv["root_tree_id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed root tree id".into()))?;
    if let Some(p) = path {
        let p = nfc(p);
        for name in p.split('/').filter(|c| !c.is_empty()) {
            let v = reader.client.get_json(
                &format!(
                    "/v1/repos/{}/trees/{}",
                    uri_segment(&reader.repo),
                    tree.to_hex()
                ),
                &[],
            )?;
            let entry = v["entries"]
                .as_array()
                .ok_or_else(|| VerbError::Local("server sent a malformed tree listing".into()))?
                .iter()
                .find(|e| e["name"].as_str() == Some(name))
                .ok_or_else(|| VerbError::Local(format!("no such path {p:?}")))?;
            if entry["kind"].as_str() != Some("dir") {
                return Err(VerbError::Local(format!(
                    "{p:?} is a file, not a directory"
                )));
            }
            tree = entry["object_id"]
                .as_str()
                .and_then(Id::from_hex)
                .ok_or_else(|| VerbError::Local("tree entry with a malformed object id".into()))?;
        }
    }
    let v = reader.client.get_json(
        &format!(
            "/v1/repos/{}/trees/{}",
            uri_segment(&reader.repo),
            tree.to_hex()
        ),
        &[],
    )?;
    Ok(json!({ "entries": v["entries"] }))
}

/// `naatre diff [--at A B]` — server-side two-changeset diff, or (default)
/// head vs disk: local modifications as path-level add/mod/del against the
/// head root tree, restricted to the sparse prefixes.
pub fn diff(root: &Path, at: Option<(&str, &str)>, json_out: bool, net: &Net) -> i32 {
    match diff_value(root, at, net) {
        Ok(v) => {
            if json_out {
                println!("{v}");
            } else {
                for c in v["changes"].as_array().map_or(&[][..], Vec::as_slice) {
                    let kind = c["kind"].as_str().unwrap_or("?");
                    let path = c["path"].as_str().unwrap_or("?");
                    let c = match kind {
                        "add" => "A",
                        "del" => "D",
                        _ => "M",
                    };
                    println!("{c} {path}");
                }
            }
            0
        }
        Err(e) => e.report(),
    }
}

/// The diff core: `{"changes": [...]}` — the server's diff-entry shape for
/// the two-changeset form (`--at A B`), or the local head-vs-disk diff.
/// The same value `diff --json` prints; the MCP `diff` tool returns it.
pub fn diff_value(root: &Path, at: Option<(&str, &str)>, net: &Net) -> Result<Value, VerbError> {
    let reader = open_reader(root, net)?;
    let changes = match at {
        Some((a, b)) => {
            let cs_a = resolve_at(&reader, Some(a))?;
            let cs_b = resolve_at(&reader, Some(b))?;
            let v = reader.client.get_json(
                &format!("/v1/repos/{}/diff", uri_segment(&reader.repo)),
                &[("a", cs_a.to_hex().as_str()), ("b", cs_b.to_hex().as_str())],
            )?;
            v["changes"]
                .as_array()
                .cloned()
                .ok_or_else(|| VerbError::Local("server sent a malformed diff".into()))?
        }
        None => local_diff(root, &reader)?,
    };
    Ok(json!({ "changes": changes }))
}

/// Head vs disk, computed client-side: head root tree (sparse-filtered)
/// against rehashed disk content.
fn local_diff(root: &Path, reader: &Reader) -> Result<Vec<Value>, VerbError> {
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    let rootv = reader.client.get_json(
        &format!(
            "/v1/repos/{}/changesets/{}/root",
            uri_segment(&reader.repo),
            reader.head.to_hex()
        ),
        &[],
    )?;
    let head_root = rootv["root_tree_id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed root tree id".into()))?;
    // The uniform client rule for reads over an existing checkout's cone:
    // a prefix the head tree no longer contains (the server deleted its
    // whole directory — normal history) is an EMPTY subtree, exactly as in
    // sync's target walk. The strict form would wedge diff forever.
    let files = walk_tree_allowing_missing_prefixes(
        &reader.client,
        &reader.repo,
        &head_root,
        &state.sparse,
    )?;
    let mut head_map: BTreeMap<String, (Id, bool)> = BTreeMap::new();
    for f in files {
        head_map.insert(f.path, (f.blob, f.exec));
    }

    let mut disk_map: BTreeMap<String, (Id, bool)> = BTreeMap::new();
    let disk = crate::wc::scan::scan_disk(root)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    for f in disk {
        if !under_sparse(&state.sparse, &f.norm) {
            continue;
        }
        let bytes = std::fs::read(root.join(&f.raw))
            .map_err(|e| VerbError::Local(format!("reading {}: {e}", f.raw)))?;
        disk_map.insert(
            f.norm,
            (object_id(Kind::Blob, &bytes), f.meta.mode() & 0o111 != 0),
        );
    }

    let mut changes = Vec::new();
    for (path, (blob, exec)) in &head_map {
        match disk_map.get(path) {
            Some((disk_blob, disk_exec)) => {
                if disk_blob != blob || disk_exec != exec {
                    changes.push(json!({
                        "path": path, "kind": "mod",
                        "a_blob": blob.to_hex(), "b_blob": disk_blob.to_hex(),
                    }));
                }
            }
            None => changes.push(json!({
                "path": path, "kind": "del",
                "a_blob": blob.to_hex(), "b_blob": Value::Null,
            })),
        }
    }
    for (path, (blob, _)) in &disk_map {
        if !head_map.contains_key(path) {
            changes.push(json!({
                "path": path, "kind": "add",
                "a_blob": Value::Null, "b_blob": blob.to_hex(),
            }));
        }
    }
    Ok(changes)
}

#[cfg(test)]
mod tests {
    #[test]
    fn degraded_warning_names_the_poison_and_exit_stays_zero() {
        let w = super::degraded_blame_warning(&["aa11", "bb22"]);
        assert!(w.starts_with("warning: blame degraded"));
        assert!(w.contains("aa11, bb22"));
        // Exit-code contract: the caller returns 0 on a degraded 200 — the
        // warning is on stderr, never a failure exit.
    }
}
