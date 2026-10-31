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
//! newly created blob contents.

use std::collections::BTreeMap;

use similar::{DiffOp, TextDiff};

use naatre_core::changeset::{FileChange, Op};
use naatre_core::ids::{Id, Kind, object_id};

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

/// A per-path merge failure: who is on each side, by blob id.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PathConflict {
    pub path: String,
    pub reason: String,
    pub base_blob: Option<Id>,
    pub ours_blob: Option<Id>,
    pub theirs_blob: Option<Id>,
}

/// A successful tree merge: the merged node and the blob contents that are
/// new to this merge (content-addressed; the caller puts them into the CAS
/// and records `repo_blobs` rows before its transaction commits).
#[derive(Debug, Default)]
pub struct MergedTree {
    pub node: MemNode,
    pub new_blobs: Vec<(Id, Vec<u8>)>,
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
/// failure aborts with the fetcher's own error (a missing blob of a stored
/// tree is corruption, not a merge conflict).
pub fn merge_trees(
    base: &MemNode,
    ours: &MemNode,
    theirs: &MemNode,
    mut fetch_blob: impl FnMut(&Id) -> Result<Vec<u8>, String>,
) -> Result<MergedTree, Vec<PathConflict>> {
    let base_files = flatten_files(base);
    let ours_files = flatten_files(ours);
    let theirs_files = flatten_files(theirs);

    let mut merged_files: BTreeMap<String, FileMeta> = BTreeMap::new();
    let mut new_blobs: Vec<(Id, Vec<u8>)> = Vec::new();
    let mut conflicts: Vec<PathConflict> = Vec::new();

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
            ours_blob: o.map(|m| m.blob),
            theirs_blob: t.map(|m| m.blob),
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
                let merged = (|| -> Result<Option<(FileMeta, Vec<u8>)>, String> {
                    let outcome = merge_lines(
                        &fetch_blob(&bm.blob)?,
                        &fetch_blob(&om.blob)?,
                        &fetch_blob(&tm.blob)?,
                    );
                    match outcome {
                        MergeOutcome::Conflict => Ok(None),
                        MergeOutcome::Merged(bytes) => {
                            let blob = object_id(Kind::Blob, &bytes);
                            let exec = if om.exec == tm.exec {
                                om.exec
                            } else if om.exec == bm.exec {
                                tm.exec
                            } else {
                                om.exec
                            };
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
                    Ok(None) => conflicts.push(conflict("both modified, line merge conflict")),
                    Err(e) => return Err(vec![conflict(&e)]),
                }
            }
            (None, Some(_), Some(_)) => {
                conflicts.push(conflict("both added, different content"));
            }
            (Some(_), None, Some(_)) | (Some(_), Some(_), None) => {
                conflicts.push(conflict("deleted by one side, modified by the other"));
            }
            // o == t == None with b present is both-deleted: absent, handled
            // by the o == t branch above.
            _ => unreachable!("both-changed-differently table is total"),
        }
    }

    if !conflicts.is_empty() {
        return Err(conflicts);
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
        return Err(vec![PathConflict {
            reason: "merged result is not a consistent tree (file/directory duality)".into(),
            base_blob: None,
            ours_blob: None,
            theirs_blob: None,
            path,
        }]);
    }
    Ok(MergedTree { node, new_blobs })
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
