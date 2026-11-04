//! Merge primitives for land (M4a): line-level 3-way over UTF-8 text and
//! path-level 3-way over trees.
//!
//! Pure module — no SQL, no I/O. Blob contents arrive through a fetcher
//! closure; merged contents come back as bytes for the caller to put into
//! the CAS (land does that inside its transaction, files-before-rows).
//!
//! Line merge (`merge_lines`): the one-sided and identical shortcuts first,
//! then `similar`'s line diff of base→ours and base→theirs. A hunk is a
//! `(base line range, replacement lines)` pair; hunks whose base ranges
//! overlap conflict, as do two insertions at the same base position with
//! different content (the "same-line divergent edit"). Non-overlapping
//! hunks — including merely adjacent ones — apply in base order, so the
//! disjoint-edits union property holds. Any non-UTF-8 input on the
//! both-modified path is a conflict (the binary rule: never a garbage
//! merge).
//!
//! Tree merge (`merge_trees`): the same decision per path — equal on both
//! sides, or one-sided, wins; both-modified-different files go through
//! `merge_lines`; delete-vs-modify and add-vs-add-different conflict. The
//! result is a [`MemNode`] built from the merged file map plus the list of
//! newly created blob contents. M12 adds the record mode
//! (`merge_trees_recording`, the `--record-conflict` land): every
//! representable conflict materializes its markers into the merged tree
//! instead of failing it, and the conflict list comes back alongside — a
//! modify/delete conflict records with a deletion side and an exec-only
//! conflict records with per-side exec bits (F2); only a merged result that
//! is not a consistent tree stays fatal.

use std::collections::BTreeMap;

use similar::{DiffOp, TextDiff};

use naatre_core::changeset::{FileChange, Op};
use naatre_core::conflict::{self, ConflictRep, marker_bytes};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_store::cas::BLOB_LIMIT;

use crate::tree_derive::{ApplyError, MemNode};

/// Result of a line-level 3-way merge.
#[derive(Debug, PartialEq, Eq)]
pub enum MergeOutcome {
    /// The merged content.
    Merged(Vec<u8>),
    /// The two sides diverge beyond clean merge (overlap, same-line
    /// divergent edit, or binary content).
    Conflict,
}

/// One edit hunk over the base line sequence: replaces base lines
/// `[start, end)` with `lines` (an insertion has `start == end`).
struct Hunk<'a> {
    start: usize,
    end: usize,
    lines: Vec<&'a str>,
}

impl Hunk<'_> {
    /// Two hunks overlap when their base ranges intersect strictly; empty
    /// (insertion) ranges never overlap — same-point insertions get their
    /// own rule in [`merge_lines`].
    fn overlaps(&self, other: &Hunk<'_>) -> bool {
        self.start < other.end && other.start < self.end
    }

    fn is_insertion(&self) -> bool {
        self.start == self.end
    }
}

/// Extracts the non-equal diff ops of a base→other line diff as hunks.
fn hunks_of<'a>(ops: &[DiffOp], new_lines: &[&'a str]) -> Vec<Hunk<'a>> {
    ops.iter()
        .filter_map(|op| match *op {
            DiffOp::Equal { .. } => None,
            DiffOp::Delete {
                old_index, old_len, ..
            } => Some(Hunk {
                start: old_index,
                end: old_index + old_len,
                lines: Vec::new(),
            }),
            DiffOp::Insert {
                old_index,
                new_index,
                new_len,
            } => Some(Hunk {
                start: old_index,
                end: old_index,
                lines: new_lines[new_index..new_index + new_len].to_vec(),
            }),
            DiffOp::Replace {
                old_index,
                old_len,
                new_index,
                new_len,
            } => Some(Hunk {
                start: old_index,
                end: old_index + old_len,
                lines: new_lines[new_index..new_index + new_len].to_vec(),
            }),
        })
        .collect()
}

/// Line-level 3-way merge over UTF-8 text (see the module doc). Line
/// terminators stay attached to their line, so reassembly is byte-exact.
pub fn merge_lines(base: &[u8], ours: &[u8], theirs: &[u8]) -> MergeOutcome {
    // The shortcuts take whole sides; no merge (and so no UTF-8 or binary
    // question) happens on a one-sided or identical change.
    if ours == base {
        return MergeOutcome::Merged(theirs.to_vec());
    }
    if theirs == base {
        return MergeOutcome::Merged(ours.to_vec());
    }
    if ours == theirs {
        return MergeOutcome::Merged(ours.to_vec());
    }

    // Both sides modified differently: binary content is a structured
    // conflict, never a garbage merge.
    let (Ok(base), Ok(ours), Ok(theirs)) = (
        std::str::from_utf8(base),
        std::str::from_utf8(ours),
        std::str::from_utf8(theirs),
    ) else {
        return MergeOutcome::Conflict;
    };

    let diff_ours = TextDiff::from_lines(base, ours);
    // The hunks index base lines; the old side of the same diff is the base
    // tokenized identically.
    let base_lines: Vec<&str> = diff_ours.old_slices().to_vec();
    let ours_hunks = hunks_of(diff_ours.ops(), diff_ours.new_slices());
    let diff_theirs = TextDiff::from_lines(base, theirs);
    let theirs_hunks = hunks_of(diff_theirs.ops(), diff_theirs.new_slices());

    // Conflict detection: a base-range overlap between the sides, or two
    // insertions at the same base position with different content.
    for ho in &ours_hunks {
        for ht in &theirs_hunks {
            if ho.overlaps(ht) {
                return MergeOutcome::Conflict;
            }
            if ho.is_insertion()
                && ht.is_insertion()
                && ho.start == ht.start
                && ho.lines != ht.lines
            {
                return MergeOutcome::Conflict;
            }
        }
    }

    // Apply every hunk in base order; insertions sort before a replacement
    // starting at the same line (an insertion "before base line N" happens
    // first). Identical same-point insertions apply once.
    let mut all: Vec<(&Hunk, bool)> = ours_hunks.iter().map(|h| (h, false)).collect();
    all.extend(theirs_hunks.iter().map(|h| (h, true)));
    all.sort_by_key(|(h, from_theirs)| (h.start, !h.is_insertion(), *from_theirs));

    let mut merged: Vec<&str> = Vec::new();
    let mut pos = 0usize;
    let mut i = 0usize;
    while i < all.len() {
        let (hunk, _) = all[i];
        merged.extend_from_slice(&base_lines[pos..hunk.start]);
        merged.extend_from_slice(&hunk.lines);
        pos = hunk.end;
        i += 1;
        // Skip a duplicate insertion the other side contributed verbatim.
        while i < all.len() {
            let (next, _) = all[i];
            if next.is_insertion()
                && hunk.is_insertion()
                && next.start == hunk.start
                && next.lines == hunk.lines
            {
                i += 1;
            } else {
                break;
            }
        }
    }
    merged.extend_from_slice(&base_lines[pos..]);
    MergeOutcome::Merged(merged.concat().into_bytes())
}

/// One file entry in a flattened tree: the bits that distinguish entries.
/// Size is content-derived (blobs are addressed by hash), so it is not part
/// of the comparison.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FileMeta {
    pub blob: Id,
    pub size: u64,
    pub exec: bool,
}

impl FileMeta {
    /// The distinguishing pair (blob, exec).
    fn key(self) -> (Id, bool) {
        (self.blob, self.exec)
    }
}

/// A per-path merge failure: who is on each side, by blob id and exec bit
/// (`false` for an absent side).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PathConflict {
    pub path: String,
    pub reason: String,
    pub base_blob: Option<Id>,
    pub base_exec: bool,
    pub ours_blob: Option<Id>,
    pub ours_exec: bool,
    pub theirs_blob: Option<Id>,
    pub theirs_exec: bool,
}

/// What a tree merge does at a conflicting path: refuse the merge with the
/// conflict list (the M4 land contract), or record — materialize the
/// conflict's markers into the merged tree and report the conflict
/// alongside it (the `--record-conflict` land).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum OnConflict {
    Refuse,
    Record,
}

/// Why a tree merge failed. A blob-fetch failure is NOT a path conflict —
/// a missing or unreadable blob of a stored tree is corruption or a storage
/// fault — so it travels as its own variant with the failing blob id, and
/// the caller never answers it with a conflict recipe (a tombstoned id
/// answers 410 `object_redacted`, any other id a server error).
#[derive(Debug)]
pub enum MergeError {
    /// Real refusals: every path conflict (refuse mode), or the
    /// unrecordable ones (record mode: a file/directory duality, the
    /// `content_rep` guard).
    Conflicts(Vec<PathConflict>),
    /// The blob fetcher failed: the id it was asked for and its own message.
    Fetch(Id, String),
}

/// A recording merge ([`merge_trees_recording`]): the merged tree — with
/// marker content at every conflicting path — plus the conflict list the
/// markers were materialized from, and any marker whose bytes would exceed
/// the blob limit (pre-sized; the caller refuses 413 before any write).
#[derive(Debug, Default)]
pub struct RecordedMerge {
    pub tree: MergedTree,
    pub conflicts: Vec<PathConflict>,
    pub oversized: Vec<(String, usize)>,
}

/// A successful tree merge: the merged node and the blob contents that are
/// new to this merge (content-addressed; the caller puts them into the CAS
/// and records `repo_blobs` rows before its transaction commits).
#[derive(Debug, Default)]
pub struct MergedTree {
    pub node: MemNode,
    pub new_blobs: Vec<(Id, Vec<u8>)>,
    /// Every input blob id whose CONTENT flowed into a merged or marker
    /// blob (line-merge base + both sides; marker sides + base): the
    /// caller's tombstone gate probes these — new bytes are not
    /// recognizable by output id alone.
    pub input_ids: Vec<Id>,
}

/// Flattens a tree node to `full path → file meta` (directories are
/// implicit in the paths).
pub fn flatten_files(node: &MemNode) -> BTreeMap<String, FileMeta> {
    let mut out = BTreeMap::new();
    flatten_into(node, String::new(), &mut out);
    out
}

fn flatten_into(node: &MemNode, prefix: String, out: &mut BTreeMap<String, FileMeta>) {
    for (name, entry) in &node.entries {
        let path = if prefix.is_empty() {
            name.clone()
        } else {
            format!("{prefix}/{name}")
        };
        match entry {
            crate::tree_derive::MemEntry::File { blob, size, exec } => {
                out.insert(
                    path,
                    FileMeta {
                        blob: *blob,
                        size: *size,
                        exec: *exec,
                    },
                );
            }
            crate::tree_derive::MemEntry::Dir(child) => {
                flatten_into(child, path, out);
            }
        }
    }
}

/// Path-level 3-way merge of two trees against their common base. All path
/// conflicts are collected (the 409 names every one of them, not the
/// first). `fetch_blob` resolves blob contents for line merging; a fetch
/// failure aborts as [`MergeError::Fetch`] (a missing blob of a stored
/// tree is corruption, not a merge conflict).
pub fn merge_trees(
    base: &MemNode,
    ours: &MemNode,
    theirs: &MemNode,
    fetch_blob: impl FnMut(&Id) -> Result<Vec<u8>, String>,
) -> Result<MergedTree, MergeError> {
    merge_trees_mode(base, ours, theirs, fetch_blob, OnConflict::Refuse).map(|(tree, _, _)| tree)
}

/// The recording variant of [`merge_trees`]: every representable conflict
/// materializes its marker content (the representation's deterministic
/// rendering) into the merged tree, and [`RecordedMerge::conflicts`] lists
/// them all. `Err` is reserved for refusals: tree duality violations (a
/// file and a directory at one path, and the `content_rep` guard) as
/// [`MergeError::Conflicts`], and blob fetch failures as
/// [`MergeError::Fetch`]; a modify/delete
/// conflict records with a deletion side, an exec-only conflict with the
/// sides' exec bits. A marker whose bytes would exceed
/// the blob limit is PRE-SIZED here and reported in
/// [`RecordedMerge::oversized`] — the caller refuses 413 before any bytes
/// reach the CAS.
pub fn merge_trees_recording(
    base: &MemNode,
    ours: &MemNode,
    theirs: &MemNode,
    fetch_blob: impl FnMut(&Id) -> Result<Vec<u8>, String>,
) -> Result<RecordedMerge, MergeError> {
    merge_trees_mode(base, ours, theirs, fetch_blob, OnConflict::Record).map(
        |(tree, conflicts, oversized)| RecordedMerge {
            tree,
            conflicts,
            oversized,
        },
    )
}

/// The representation of a path conflict, or `None` when the conflict is
/// not recordable. When both competing sides carry content the rep holds
/// the sides sorted by `(blob, exec)` — a rep is orderless; when one side
/// deleted the path the rep holds the surviving `(blob, exec)` and
/// `deleted` (a deletion side has no blob id). The exec bits ride along, so
/// a conflict whose sides differ only in the exec bit records. A rep that
/// [`conflict::validate`] rejects is `None`, so none reaches `encode`. The
/// merge's decision table only raises a content conflict when ours, theirs
/// and the base all differ by `(blob, exec)`, so no reachable shape is
/// rejected (the single-path sweep in `m12a_test` pins it); the guard keeps
/// a future table change from reaching `encode` and answering 500. Also the
/// land's rep builder for recorded conflicts.
pub(crate) fn content_rep(c: &PathConflict) -> Option<ConflictRep> {
    let mut sides: Vec<(Id, bool)> = c
        .ours_blob
        .map(|id| (id, c.ours_exec))
        .into_iter()
        .chain(c.theirs_blob.map(|id| (id, c.theirs_exec)))
        .collect();
    sides.sort_by_key(|(id, exec)| (id.0, *exec));
    let rep = ConflictRep {
        path: c.path.clone(),
        base: c.base_blob,
        base_exec: c.base_blob.is_some() && c.base_exec,
        deleted: sides.len() < 2,
        sides: sides.iter().map(|(id, _)| *id).collect(),
        sides_exec: sides.iter().map(|(_, exec)| *exec).collect(),
    };
    conflict::validate(std::slice::from_ref(&rep)).ok()?;
    Some(rep)
}

/// The exec bit of a merged file or a conflict's marker file: agreeing sides win, one
/// side matching the base defers to the other, otherwise ours. A deleted
/// side (None) leaves the surviving side's bit.
fn resolve_exec(b: Option<bool>, o: Option<bool>, t: Option<bool>) -> bool {
    match (o, t) {
        (Some(oe), Some(te)) => {
            if oe == te {
                oe
            } else if b == Some(oe) {
                te
            } else {
                oe
            }
        }
        (Some(e), None) | (None, Some(e)) => e,
        // Unreachable in the both-changed table: both-deleted is handled by
        // the sides-agree branch before any conflict is raised.
        (None, None) => false,
    }
}

/// Routes one path conflict per the mode: `Refuse` collects it to fail the
/// merge; `Record` materializes the sides' markers into the merged map. A
/// fetch failure under `Record` returns the failing blob id and the
/// fetcher's error (corruption, not a conflict). Every input blob id whose content flows into the
/// result (marker sides + base) is recorded for the caller's tombstone
/// gate; an over-limit marker is pre-sized into `oversized` instead of
/// recorded.
#[allow(clippy::too_many_arguments)]
fn take_conflict(
    on: OnConflict,
    c: PathConflict,
    recorded: &mut Vec<PathConflict>,
    refused: &mut Vec<PathConflict>,
    merged_files: &mut BTreeMap<String, FileMeta>,
    new_blobs: &mut Vec<(Id, Vec<u8>)>,
    input_ids: &mut Vec<Id>,
    oversized: &mut Vec<(String, usize)>,
    fetch_blob: &mut impl FnMut(&Id) -> Result<Vec<u8>, String>,
) -> Result<(), FetchFailure> {
    match on {
        OnConflict::Refuse => {
            refused.push(c);
            Ok(())
        }
        OnConflict::Record => match content_rep(&c) {
            Some(rep) => {
                // The sides' and base's CONTENT flows into the fresh
                // marker blob: their ids join the gate batch.
                if let Some(base) = rep.base {
                    input_ids.push(base);
                }
                input_ids.extend(rep.sides.iter().copied());
                let mut failed: Option<Id> = None;
                let bytes = marker_bytes(&rep, |id| {
                    fetch_blob(id).inspect_err(|_| failed = Some(*id))
                })
                // `marker_bytes` fails only through its fetcher.
                .map_err(|e| (failed.expect("a marker_bytes error is a fetch error"), e))?;
                // Pre-size: a would-be blob-limit overflow refuses 413 at
                // the caller (before any CAS write), not a 500 from a
                // rejected CAS put.
                if bytes.len() > BLOB_LIMIT {
                    oversized.push((c.path.clone(), bytes.len()));
                    return Ok(());
                }
                let blob = object_id(Kind::Blob, &bytes);
                let meta = FileMeta {
                    blob,
                    size: bytes.len() as u64,
                    // The marker file takes the exec bit a clean merge
                    // would (three-way; with a deletion side the
                    // surviving side's bit); each side's own bit is also
                    // recorded in the representation and shown on the
                    // marker line.
                    exec: resolve_exec(
                        c.base_blob.map(|_| c.base_exec),
                        c.ours_blob.map(|_| c.ours_exec),
                        c.theirs_blob.map(|_| c.theirs_exec),
                    ),
                };
                if !new_blobs.iter().any(|(id, _)| *id == blob) {
                    new_blobs.push((blob, bytes));
                }
                merged_files.insert(c.path.clone(), meta);
                recorded.push(c);
                Ok(())
            }
            // `content_rep` built no valid representation (a guard; see its
            // doc): refuse rather than record a lossy non-resolution.
            None => {
                refused.push(c);
                Ok(())
            }
        },
    }
}

/// A fetch failure with the id that was asked for.
type FetchFailure = (Id, String);

/// Pre-sized over-limit markers: (path, byte size) — the caller refuses
/// 413 before any CAS write.
type OversizedMarkers = Vec<(String, usize)>;

/// The mode-parameterized merge core (see the two wrappers). Ok carries the
/// merged tree, the conflicts materialized in record mode (empty in refuse
/// mode — those come back as Err instead), and any pre-sized over-limit
/// markers (record mode only).
fn merge_trees_mode(
    base: &MemNode,
    ours: &MemNode,
    theirs: &MemNode,
    mut fetch_blob: impl FnMut(&Id) -> Result<Vec<u8>, String>,
    on: OnConflict,
) -> Result<(MergedTree, Vec<PathConflict>, OversizedMarkers), MergeError> {
    let base_files = flatten_files(base);
    let ours_files = flatten_files(ours);
    let theirs_files = flatten_files(theirs);

    let mut merged_files: BTreeMap<String, FileMeta> = BTreeMap::new();
    let mut new_blobs: Vec<(Id, Vec<u8>)> = Vec::new();
    // Every input blob id whose content flowed into a merged or marker
    // blob: the caller's tombstone gate probes these alongside the delta
    // blobs (the gate cannot recognize new bytes by output id alone).
    let mut input_ids: Vec<Id> = Vec::new();
    let mut oversized: OversizedMarkers = Vec::new();
    // Refuse mode: every conflict (the Err). Record mode: materialized
    // conflicts (Ok) and refusals (Err) respectively.
    let mut conflicts: Vec<PathConflict> = Vec::new();
    let mut recorded: Vec<PathConflict> = Vec::new();

    let paths: BTreeMap<&String, ()> = base_files
        .keys()
        .chain(ours_files.keys())
        .chain(theirs_files.keys())
        .map(|p| (p, ()))
        .collect();
    for path in paths.into_keys() {
        let b = base_files.get(path).copied();
        let o = ours_files.get(path).copied();
        let t = theirs_files.get(path).copied();
        let conflict = |reason: &str| PathConflict {
            path: path.clone(),
            reason: reason.to_string(),
            base_blob: b.map(|m| m.blob),
            base_exec: b.is_some_and(|m| m.exec),
            ours_blob: o.map(|m| m.blob),
            ours_exec: o.is_some_and(|m| m.exec),
            theirs_blob: t.map(|m| m.blob),
            theirs_exec: t.is_some_and(|m| m.exec),
        };

        // Decision table (entry-level equality, so `o == t` covers
        // add/add-same and modify/modify-same; `x == b` means "did not
        // touch the path"):
        if o == t {
            // Both sides agree — take it (both-deleted stays absent).
            if let Some(m) = o {
                merged_files.insert(path.clone(), m);
            }
            continue;
        }
        if o == b {
            // Ours did not touch it: theirs' add/mod/del wins.
            if let Some(m) = t {
                merged_files.insert(path.clone(), m);
            }
            continue;
        }
        if t == b {
            // Theirs did not touch it: ours' add/mod/del wins.
            if let Some(m) = o {
                merged_files.insert(path.clone(), m);
            }
            continue;
        }

        // Both sides changed the path, differently.
        match (b, o, t) {
            (Some(bm), Some(om), Some(tm)) => {
                // modify/modify: line-merge the blob contents.
                let merged = (|| -> Result<Option<(FileMeta, Vec<u8>)>, FetchFailure> {
                    let mut fetch = |id: &Id| fetch_blob(id).map_err(|e| (*id, e));
                    let base_bytes = fetch(&bm.blob)?;
                    let ours_bytes = fetch(&om.blob)?;
                    let theirs_bytes = fetch(&tm.blob)?;
                    // The inputs' content flowed into the merged blob
                    // (clean merge) or would have (conflict): their ids
                    // join the gate batch either way.
                    input_ids.extend_from_slice(&[bm.blob, om.blob, tm.blob]);
                    let outcome = merge_lines(&base_bytes, &ours_bytes, &theirs_bytes);
                    match outcome {
                        MergeOutcome::Conflict => Ok(None),
                        MergeOutcome::Merged(bytes) => {
                            let blob = object_id(Kind::Blob, &bytes);
                            let exec = resolve_exec(
                                b.map(|m| m.exec),
                                o.map(|m| m.exec),
                                t.map(|m| m.exec),
                            );
                            Ok(Some((
                                FileMeta {
                                    blob,
                                    size: bytes.len() as u64,
                                    exec,
                                },
                                bytes,
                            )))
                        }
                    }
                })();
                match merged {
                    Ok(Some((meta, bytes))) => {
                        if !new_blobs.iter().any(|(id, _)| *id == meta.blob) {
                            new_blobs.push((meta.blob, bytes));
                        }
                        merged_files.insert(path.clone(), meta);
                    }
                    Ok(None) => take_conflict(
                        on,
                        conflict("both modified, line merge conflict"),
                        &mut recorded,
                        &mut conflicts,
                        &mut merged_files,
                        &mut new_blobs,
                        &mut input_ids,
                        &mut oversized,
                        &mut fetch_blob,
                    )
                    .map_err(|(id, e)| MergeError::Fetch(id, e))?,
                    Err((id, e)) => return Err(MergeError::Fetch(id, e)),
                }
            }
            (None, Some(_), Some(_)) => {
                take_conflict(
                    on,
                    conflict("both added, different content"),
                    &mut recorded,
                    &mut conflicts,
                    &mut merged_files,
                    &mut new_blobs,
                    &mut input_ids,
                    &mut oversized,
                    &mut fetch_blob,
                )
                .map_err(|(id, e)| MergeError::Fetch(id, e))?;
            }
            (Some(_), None, Some(_)) | (Some(_), Some(_), None) => {
                take_conflict(
                    on,
                    conflict("deleted by one side, modified by the other"),
                    &mut recorded,
                    &mut conflicts,
                    &mut merged_files,
                    &mut new_blobs,
                    &mut input_ids,
                    &mut oversized,
                    &mut fetch_blob,
                )
                .map_err(|(id, e)| MergeError::Fetch(id, e))?;
            }
            // o == t == None with b present is both-deleted: absent, handled
            // by the o == t branch above.
            _ => unreachable!("both-changed-differently table is total"),
        }
    }

    if !conflicts.is_empty() {
        return Err(MergeError::Conflicts(conflicts));
    }

    // Build the result node with Add changes — `MemNode::apply` enforces the
    // same file/dir duality the derivation does, so a merged map that would
    // straddle a file/dir flip surfaces as a path conflict.
    let mut node = MemNode::empty();
    let adds: Vec<FileChange> = merged_files
        .iter()
        .map(|(path, m)| FileChange {
            path: path.clone(),
            op: Op::Add,
            blob: Some(m.blob),
            size: Some(m.size),
            exec: Some(m.exec),
        })
        .collect();
    if let Err(e) = node.apply(&adds) {
        let path = match &e {
            ApplyError::FileInPath { path } => path.clone(),
            ApplyError::ModNotFile { path } | ApplyError::DelNotFile { path } => path.clone(),
            // Unreachable for merged paths: every merged path passed the
            // grammar's depth cap on the way in.
            ApplyError::TooDeep { path } => path.clone(),
        };
        return Err(MergeError::Conflicts(vec![PathConflict {
            reason: "merged result is not a consistent tree (file/directory duality)".into(),
            base_blob: None,
            base_exec: false,
            ours_blob: None,
            ours_exec: false,
            theirs_blob: None,
            theirs_exec: false,
            path,
        }]));
    }
    Ok((
        MergedTree {
            node,
            new_blobs,
            input_ids,
        },
        recorded,
        oversized,
    ))
}

/// The delta between two trees, sorted by path — the file changes a rebased
/// changeset records (tree(parent') → tree(node')). The canonical format
/// requires strictly ascending path order, so the union is walked in one
/// sorted pass rather than "parent changes then child-only adds".
pub fn delta_between(parent: &MemNode, child: &MemNode) -> Vec<FileChange> {
    let a = flatten_files(parent);
    let c = flatten_files(child);
    let paths: std::collections::BTreeSet<&String> = a.keys().chain(c.keys()).collect();
    let mut out = Vec::new();
    for path in paths {
        match (a.get(path), c.get(path)) {
            (Some(ae), Some(ce)) => {
                if ae.key() != ce.key() {
                    out.push(FileChange {
                        path: path.clone(),
                        op: Op::Mod,
                        blob: Some(ce.blob),
                        size: Some(ce.size),
                        exec: Some(ce.exec),
                    });
                }
            }
            (Some(_), None) => out.push(FileChange {
                path: path.clone(),
                op: Op::Del,
                blob: None,
                size: None,
                exec: None,
            }),
            (None, Some(ce)) => out.push(FileChange {
                path: path.clone(),
                op: Op::Add,
                blob: Some(ce.blob),
                size: Some(ce.size),
                exec: Some(ce.exec),
            }),
            (None, None) => unreachable!("union path"),
        }
    }
    out
}
