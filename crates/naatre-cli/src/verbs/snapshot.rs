//! `naatre snapshot [--ttl DURATION]` — capture the working copy as an
//! ephemeral server-side snapshot (plan "Snapshots": client-built trees,
//! `ephemeral/` namespace, TTL default 7d, min 1 minute).
//!
//! The verb scans the working copy with the status machinery, builds
//! `naatre_core` Trees bottom-up over changed AND unchanged files under the
//! sparse prefixes, and uploads every blob (unique per run) into
//! `ephemeral/<snap-id>/` — a fresh CSPRNG id per snapshot, so nothing is
//! ever already there and every PUT is new. Clean files reuse the stat
//! index's blob ids: clean content is never rehashed. The snapshot POST
//! carries the canonical tree bytes; the server validates blob presence and
//! commits rows atomically. Snapshots are ephemeral working state — the
//! client's head and index do not move.

use std::collections::HashSet;
use std::os::unix::fs::MetadataExt as _;
use std::path::Path;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::tree::{EntryKind, Tree, TreeEntry};
use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::wc::{Wc, under_sparse};

/// The TTL floor, enforced here and again server-side (422 below it).
pub const SNAPSHOT_MIN_TTL_SECS: u64 = 60;

/// Runs snapshot; prints the created snapshot as a JSON line and returns
/// the exit code.
pub fn run(root: &Path, ttl: Option<&str>, net: &Net) -> i32 {
    match value(root, ttl, net) {
        Ok(v) => {
            println!("{v}");
            0
        }
        Err(e) => e.report(),
    }
}

/// Parses `--ttl`: a bare number is seconds; `<n><s|m|h|d>` spells the
/// unit. Below the 60 s floor is a usage error (the server would 422 it).
pub fn parse_ttl_secs(raw: &str) -> Result<u64, VerbError> {
    let bad = || {
        VerbError::Usage(format!(
            "--ttl {raw:?} is not a duration (try 90s, 5m, 2h, 7d; min 60s)"
        ))
    };
    let (digits, unit) = match raw.find(|c: char| !c.is_ascii_digit()) {
        None => (raw, "s"),
        Some(0) => return Err(bad()),
        Some(i) => (&raw[..i], &raw[i..]),
    };
    let n: u64 = digits.parse().map_err(|_| bad())?;
    let secs = match unit {
        "s" | "S" => Some(n),
        "m" | "M" => n.checked_mul(60),
        "h" | "H" => n.checked_mul(3600),
        "d" | "D" => n.checked_mul(86_400),
        _ => return Err(bad()),
    }
    .ok_or_else(|| VerbError::Usage(format!("--ttl {raw:?} overflows")))?;
    if secs < SNAPSHOT_MIN_TTL_SECS {
        return Err(VerbError::Usage(format!(
            "--ttl must be at least {SNAPSHOT_MIN_TTL_SECS}s (got {secs}s)"
        )));
    }
    Ok(secs)
}

/// One file that will be in the snapshot tree.
struct SnapFile {
    path: String,
    /// Raw disk path to read bytes from when uploading (a
    /// normalization-folding filesystem may store the NFC path under an NFD
    /// name).
    raw: String,
    blob: Id,
    size: u64,
    exec: bool,
}

/// The created snapshot: `{id, expires_ns}` (the POST response's snapshot
/// fields; mirrors the snapshots API shape; the MCP `snapshot` tool returns
/// the same value).
pub fn value(root: &Path, ttl: Option<&str>, net: &Net) -> Result<Value, VerbError> {
    let ttl_secs = ttl.map(parse_ttl_secs).transpose()?;
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    let index = wc.load_index()?;
    let client = wc.client(net)?;
    let repo = &state.server_repo;

    // Gather the file set from the same classification status uses: clean
    // tracked files keep their index blob id (no rehash, no read),
    // modified and untracked files are hashed from disk, deleted files
    // drop out.
    let report = crate::wc::scan::classify(root, &index)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    let mut files: Vec<SnapFile> = Vec::new();
    for (path, row) in &index.rows {
        if !under_sparse(&state.sparse, path)
            || report.modified.contains(path)
            || report.deleted.contains(path)
        {
            continue;
        }
        files.push(SnapFile {
            path: path.clone(),
            raw: report
                .raw_of
                .get(path)
                .cloned()
                .unwrap_or_else(|| path.clone()),
            blob: Id(row.blob),
            size: row.size,
            exec: row.exec,
        });
    }
    for path in report.modified.iter().chain(report.untracked.iter()) {
        if !under_sparse(&state.sparse, path) {
            continue;
        }
        let raw = report
            .raw_of
            .get(path)
            .cloned()
            .unwrap_or_else(|| report.raw_of_untracked[path].clone());
        let bytes = std::fs::read(root.join(&raw))
            .map_err(|e| VerbError::Local(format!("reading {raw:?}: {e}")))?;
        let meta = std::fs::metadata(root.join(&raw))
            .map_err(|e| VerbError::Local(format!("stating {raw:?}: {e}")))?;
        files.push(SnapFile {
            path: path.clone(),
            raw,
            blob: object_id(Kind::Blob, &bytes),
            size: bytes.len() as u64,
            exec: meta.mode() & 0o111 != 0,
        });
    }
    files.sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));

    // Build the trees bottom-up: one canonical encoding per directory, ids
    // computed client-side, the root's id tops the POST.
    let (root_tree, trees_b64) = build_trees(&files)?;
    let snap_id = new_snap_id();

    // Blobs first: raw PUT into the snapshot namespace, hash-verified
    // server-side. Each unique blob uploads once (the fresh snap id means
    // nothing is ever already there; a retried PUT short-circuits).
    let mut uploaded: HashSet<Id> = HashSet::new();
    for f in &files {
        if !uploaded.insert(f.blob) {
            continue;
        }
        let bytes = std::fs::read(root.join(&f.raw))
            .map_err(|e| VerbError::Local(format!("reading {}: {e}", f.path)))?;
        client.put_bytes(
            &format!(
                "/v1/repos/{}/snapshots/{}/objects/{}",
                crate::client::uri_segment(repo),
                snap_id,
                f.blob.to_hex()
            ),
            &bytes,
        )?;
    }

    let mut body = json!({
        "snap_id": snap_id,
        "tree_id": root_tree.to_hex(),
        "trees": trees_b64,
    });
    if let Some(secs) = ttl_secs {
        body["ttl_secs"] = json!(secs);
    }
    // The snap id is client-generated per attempt (see the PUTs above): a
    // retried POST re-PUTs the same trees against the same id — idempotent,
    // so the retry contract applies.
    let (_status, created) = client.post_json_idempotent(
        &format!("/v1/repos/{}/snapshots", crate::client::uri_segment(repo)),
        &body,
    )?;
    Ok(json!({
        "id": created["id"],
        "expires_ns": created["expires_ns"],
    }))
}

/// A 64-hex CSPRNG snapshot id (generated client-side per the plan).
fn new_snap_id() -> String {
    let mut bytes = [0u8; 32];
    getrandom::fill(&mut bytes).expect("CSPRNG available");
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// Builds the directory tree bottom-up and returns (root id, canonical
/// bytes of every tree, base64-encoded) — the full closure the server's
/// create transaction writes rows for. Pure: sizes ride in on [`SnapFile`].
fn build_trees(files: &[SnapFile]) -> Result<(Id, Vec<String>), VerbError> {
    #[derive(Default)]
    struct Node {
        files: Vec<(String, Id, u64, bool)>,
        dirs: std::collections::BTreeMap<String, Node>,
    }
    let mut root = Node::default();
    for f in files {
        let mut node = &mut root;
        let mut comps = f.path.split('/');
        let Some(last) = comps.next_back() else {
            return Err(VerbError::Local(format!(
                "path {:?} has no file component",
                f.path
            )));
        };
        for dir in comps {
            node = node.dirs.entry(dir.to_string()).or_default();
        }
        node.files.push((last.to_string(), f.blob, f.size, f.exec));
    }

    // Dir entry sizes are the recursive file-byte totals.
    fn subtree_size(node: &Node) -> u64 {
        node.files.iter().map(|(_, _, size, _)| *size).sum::<u64>()
            + node.dirs.values().map(subtree_size).sum::<u64>()
    }

    fn encode_node(node: &Node, out: &mut Vec<String>) -> Result<Id, VerbError> {
        let mut entries = Vec::with_capacity(node.files.len() + node.dirs.len());
        for (name, blob, size, exec) in &node.files {
            entries.push(TreeEntry {
                name: name.clone(),
                kind: EntryKind::File,
                object: *blob,
                size: *size,
                exec: *exec,
            });
        }
        for (name, child) in &node.dirs {
            // Post-order: children first (bottom-up).
            let child_id = encode_node(child, out)?;
            entries.push(TreeEntry {
                name: name.clone(),
                kind: EntryKind::Dir,
                object: child_id,
                size: subtree_size(child),
                exec: false,
            });
        }
        // BTreeMap iteration is already byte-ascending; the sort makes the
        // file/dir interleave canonical explicitly.
        entries.sort_by(|a, b| a.name.as_bytes().cmp(b.name.as_bytes()));
        let tree = Tree { entries };
        let id = naatre_core::tree::tree_id(&tree)
            .map_err(|e| VerbError::Local(format!("encoding snapshot tree: {e}")))?;
        let canonical = naatre_core::tree::encode(&tree)
            .map_err(|e| VerbError::Local(format!("encoding snapshot tree: {e}")))?;
        out.push(STANDARD.encode(canonical));
        Ok(id)
    }

    let mut out = Vec::new();
    let id = encode_node(&root, &mut out)?;
    Ok((id, out))
}
