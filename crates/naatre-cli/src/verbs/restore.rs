//! `naatre restore <snap-id> [--force]` — materialize an ephemeral
//! snapshot's files into this working copy.
//!
//! Expiry is checked first (the metadata GET answers 410 `snapshot_expired`
//! before anything is touched — a clean refusal, nothing partial). The
//! snapshot's tree is fetched within the sparse cone, every blob is fetched
//! and hash-verified from the snapshot namespace, and only then does
//! materialization begin (temp+rename per file, so an interrupted restore
//! leaves every completed file complete and never a half-written one).
//!
//! The dirty gate is sync's, widened with sparse-add's symlink colliders
//! (plan: "`sync`/`restore` with conflicting uncommitted edits *or colliding
//! untracked files* refuse with `dirty_working_copy` (details enumerate
//! both) and preserve everything (`--force` to discard)"): locally edited or
//! deleted tracked files, untracked / normalization-folded files, and
//! symlinks on any component of a restored path are all named; `--force`
//! discards exactly those (symlinks unlinked, colliders deleted, tracked
//! edits overwritten) — and only after every blob verified.
//!
//! Paths in the working copy that the snapshot does not contain are left
//! alone (preserve-everything: they become untracked when the index moves).
//! `state.head` is UNCHANGED — snapshots are ephemeral working state, not
//! history; restore moves the files, never the branch.

use std::collections::{BTreeSet, HashMap, HashSet};
use std::path::Path;

use naatre_core::ids::Id;

use super::{Net, VerbError};
use crate::wc::fetch::{materialize, walk_snapshot_tree};
use crate::wc::{Wc, remove_and_prune};

/// Runs restore; prints the outcome and returns the exit code.
pub fn run(root: &Path, snap_id: &str, force: bool, net: &Net) -> i32 {
    match try_run(root, snap_id, force, net) {
        Ok(()) => 0,
        Err(e) => e.report(),
    }
}

fn try_run(root: &Path, snap_id: &str, force: bool, net: &Net) -> Result<(), VerbError> {
    let sid = Id::from_hex(snap_id).ok_or_else(|| {
        VerbError::Usage(format!(
            "snapshot id {snap_id:?} is not 64 lowercase hex characters"
        ))
    })?;
    let wc = Wc::open(root)?;
    let state = wc.load_state()?;
    let index = wc.load_index()?;
    let client = wc.client(net)?;
    let repo = &state.server_repo;
    let seg = crate::client::uri_segment(repo);

    // Metadata first: an expired or foreign snapshot is refused before
    // anything on disk or in the index is touched.
    let meta = client.get_json(&format!("/v1/repos/{seg}/snapshots/{sid}"), &[])?;
    let tree_id = meta["tree_id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server sent a malformed snapshot tree id".into()))?;

    // The snapshot's files, within the sparse cone (a prefix the snapshot
    // lacks is an empty subtree — restore brings what it has). Walks the
    // owner-gated snapshot tree route: snapshot trees carry no repo_trees
    // membership, so the bare-id repo tree route 404s them.
    let files = walk_snapshot_tree(&client, repo, &sid, &tree_id, &state.sparse)?;

    // The dirty gate (sync's, plus symlink colliders): local work at a
    // restored path would be clobbered by materialization — edited or
    // deleted tracked files, untracked / folded files, symlinks.
    let report = crate::wc::scan::classify(root, &index)
        .map_err(|e| VerbError::Local(format!("scanning {}: {e}", root.display())))?;
    let incoming: BTreeSet<&str> = files.iter().map(|f| f.path.as_str()).collect();
    let blocked: Vec<String> = report
        .modified
        .iter()
        .chain(report.deleted.iter())
        .filter(|path| incoming.contains(path.as_str()))
        .cloned()
        .collect();
    let mut colliding: Vec<String> = Vec::new();
    for (norm, raws) in &report.folds {
        if incoming.contains(norm.as_str()) {
            colliding.extend(raws.iter().cloned());
        }
    }
    for norm in &report.untracked {
        if incoming.contains(norm.as_str()) {
            colliding.push(report.raw_of_untracked[norm].clone());
        }
    }
    colliding.sort();
    colliding.dedup();
    let mut symlinks: Vec<String> = Vec::new();
    for path in &incoming {
        let mut rel = String::new();
        for comp in path.split('/') {
            rel = if rel.is_empty() {
                comp.to_string()
            } else {
                format!("{rel}/{comp}")
            };
            let is_symlink = root
                .join(&rel)
                .symlink_metadata()
                .map(|m| m.file_type().is_symlink())
                .unwrap_or(false); // absent: nothing to refuse. NOTE: a stat error fails OPEN here (reads as "not a symlink"); the safety net is the subsequent write, which fails loudly on the same error — an EACCES is never mistaken for a planted link
            if is_symlink {
                symlinks.push(rel.clone());
            }
        }
    }
    symlinks.sort();
    symlinks.dedup();

    if (!blocked.is_empty() || !colliding.is_empty() || !symlinks.is_empty()) && !force {
        let blocked_list = blocked.join(", ");
        let collide_list = colliding.join(", ");
        let symlink_list = symlinks.join(", ");
        return Err(VerbError::Http(crate::client::HttpError::Envelope {
            status: 409,
            code: "dirty_working_copy".into(),
            message: format!(
                "refusing to restore (nothing changed): local edits on the snapshot's \
                 paths: [{blocked_list}]; colliding untracked: [{collide_list}]; \
                 symlinks on the snapshot's paths: [{symlink_list}]; \
                 use --force to discard them"
            ),
            details: None,
            retry_after: None,
        }));
    }

    // Fetch and hash-verify EVERY blob before anything is discarded or
    // written: a failed fetch costs the user nothing.
    let ids: Vec<Id> = files.iter().map(|f| f.blob).collect();
    let blobs = fetch_snapshot_blobs(&client, repo, &sid.to_hex(), &ids)?;

    // Fetch verified: discard exactly the colliders the refusal named.
    for raw in &symlinks {
        remove_and_prune(root, raw)
            .map_err(|e| VerbError::Local(format!("removing colliding symlink {raw:?}: {e}")))?;
    }
    for raw in &colliding {
        remove_and_prune(root, raw)
            .map_err(|e| VerbError::Local(format!("removing colliding untracked {raw:?}: {e}")))?;
    }

    // Materialize, then move the index to the restored state. The head is
    // deliberately untouched (see the module docs).
    let mut new_index = crate::wc::index::Index::default();
    for file in &files {
        let bytes = &blobs[&file.blob];
        let row = materialize(root, file, bytes)?;
        new_index.rows.insert(file.path.clone(), row);
    }
    wc.save_index(&new_index)?;

    println!(
        "restored {} ({} file(s)); head unchanged at {}",
        sid.to_hex(),
        files.len(),
        state.head,
    );
    Ok(())
}

/// Fetches blobs from the snapshot namespace (`GET …/snapshots/{sid}/
/// objects/{oid}`) instead of the history batch route, hash-verifying every
/// payload.
fn fetch_snapshot_blobs(
    client: &crate::client::Client,
    repo: &str,
    sid: &str,
    ids: &[Id],
) -> Result<HashMap<Id, Vec<u8>>, VerbError> {
    let mut seen: HashSet<Id> = HashSet::new();
    let mut out = HashMap::new();
    for id in ids {
        if !seen.insert(*id) {
            continue;
        }
        let bytes = client.get_bytes(&format!(
            "/v1/repos/{}/snapshots/{}/objects/{}",
            crate::client::uri_segment(repo),
            sid,
            id.to_hex()
        ))?;
        crate::wc::fetch::verify(id, &bytes)?;
        out.insert(*id, bytes);
    }
    Ok(out)
}
