//! The disk scanner and status classification (the M3 pin).
//!
//! - Every scanned path is normalized to NFC **before** index comparison:
//!   a disk NFD twin of an indexed NFC path matches its NFC twin — it never
//!   appears as a phantom delete plus an untracked file. When two on-disk
//!   files fold onto one path, the fold is reported (the path goes dirty
//!   and sync refuses to overwrite either file).
//! - Stat comparison per file: size + mtime_ns + ctime_ns + ino. All four
//!   equal → clean. Any other stat drift → rehash: the file's content is
//!   hashed with `object_id(Blob)` and compared to the index blob — the
//!   content hash is ground truth (this is the racily-clean rule's
//!   adversarial case: size+mtime forged back, ctime/ino moved).
//! - `status` makes zero server calls by construction (no client exists in
//!   this module).
//! - Symlinks are not part of the v1 object model (import rejects them);
//!   the scanner skips them rather than silently dereferencing them into
//!   commits. Non-UTF-8 file names are skipped for the same reason.

use std::collections::{BTreeMap, HashSet};
use std::path::Path;

use naatre_core::ids::{Id, Kind, object_id};
use serde_json::{Value, json};

use crate::wc::index::{self, Index};
use crate::wc::{is_reserved, nfc};

/// One regular file found on disk: raw path (as on disk, `/`-separated
/// relative to the checkout root) and its NFC normalization.
pub struct DiskFile {
    pub raw: String,
    pub norm: String,
    pub meta: std::fs::Metadata,
}

/// Walks the checkout root (depth-first, hand-rolled: no walker dep),
/// skipping the reserved `.naatre/` and `.git/` directories at the root.
/// Output is sorted by raw path bytes.
pub fn scan_disk(root: &Path) -> std::io::Result<Vec<DiskFile>> {
    let mut out = Vec::new();
    let mut stack = vec![(root.to_path_buf(), String::new())];
    while let Some((dir, rel)) = stack.pop() {
        let entries = std::fs::read_dir(&dir)?;
        for entry in entries {
            let entry = entry?;
            let ft = entry.file_type()?;
            let name = match entry.file_name().into_string() {
                Ok(n) => n,
                Err(_) => continue, // non-UTF-8 names cannot be naatre paths
            };
            let child_rel = if rel.is_empty() {
                name.clone()
            } else {
                format!("{rel}/{name}")
            };
            let child = dir.join(&name);
            if ft.is_dir() {
                if rel.is_empty() && is_reserved(&child_rel) {
                    continue;
                }
                stack.push((child, child_rel));
            } else if ft.is_file() {
                let meta = entry.metadata()?;
                out.push(DiskFile {
                    norm: nfc(&child_rel),
                    raw: child_rel,
                    meta,
                });
            }
            // symlinks (and anything else): skipped, see module docs
        }
    }
    out.sort_by(|a, b| a.raw.as_bytes().cmp(b.raw.as_bytes()));
    Ok(out)
}

/// The working-copy status relative to the stat index.
#[derive(Debug, Default)]
pub struct Report {
    /// NFC paths whose content hash differs from the index (or that fold
    /// ambiguously).
    pub modified: Vec<String>,
    /// Indexed paths absent from disk.
    pub deleted: Vec<String>,
    /// NFC-normalized paths present on disk but absent from the index.
    pub untracked: Vec<String>,
    /// Normalization folds: NFC path → every raw disk path that folded onto
    /// it (≥ 2 entries when a fold exists).
    pub folds: BTreeMap<String, Vec<String>>,
    /// The raw disk path of each untracked path (for `--force` cleanup).
    pub raw_of_untracked: BTreeMap<String, String>,
    /// The first raw disk path for every classified path, tracked or not —
    /// the path to actually read/write on a possibly normalization-folding
    /// filesystem (raw may be NFD while the indexed name is NFC).
    pub raw_of: BTreeMap<String, String>,
}

impl Report {
    pub fn is_clean(&self) -> bool {
        self.modified.is_empty() && self.deleted.is_empty() && self.untracked.is_empty()
    }

    pub fn status_word(&self) -> &'static str {
        if self.is_clean() { "clean" } else { "dirty" }
    }

    /// The pinned `status --json` shape.
    pub fn to_json(&self) -> Value {
        json!({
            "status": self.status_word(),
            "modified": self.modified,
            "deleted": self.deleted,
            "untracked": self.untracked,
        })
    }
}

/// Classifies the disk against the index (zero server calls).
pub fn classify(root: &Path, index: &Index) -> std::io::Result<Report> {
    let mut report = Report::default();
    let mut seen_tracked: HashSet<String> = HashSet::new();
    let mut first_tracked: BTreeMap<String, String> = BTreeMap::new();
    for file in scan_disk(root)? {
        if index.rows.contains_key(&file.norm) {
            if seen_tracked.contains(&file.norm) {
                // Second on-disk file folding onto an indexed path: the
                // materialization target is ambiguous — report dirty and
                // remember every raw path involved.
                let entry = report
                    .folds
                    .entry(file.norm.clone())
                    .or_insert_with(|| vec![first_tracked[&file.norm].clone()]);
                entry.push(file.raw);
                if !report.modified.contains(&file.norm) {
                    report.modified.push(file.norm);
                }
                continue;
            }
            seen_tracked.insert(file.norm.clone());
            first_tracked.insert(file.norm.clone(), file.raw.clone());
            report.raw_of.insert(file.norm.clone(), file.raw.clone());
            let Some(row) = index.rows.get(&file.norm) else {
                unreachable!("membership checked above");
            };
            if index::stat_matches(row, &file.meta) {
                continue;
            }
            // Stat drift — the racily-clean adversarial case lives here:
            // rehash; the content hash decides.
            let bytes = std::fs::read(root.join(&file.raw))?;
            if object_id(Kind::Blob, &bytes) == Id(row.blob) {
                continue; // stale stat, identical content
            }
            report.modified.push(file.norm);
        } else if report.raw_of_untracked.contains_key(&file.norm) {
            // Untracked twin fold: keep the first path as the raw one and
            // record the fold so sync can refuse to clobber either file.
            let first = report.raw_of_untracked[&file.norm].clone();
            report
                .folds
                .entry(file.norm.clone())
                .or_insert_with(|| vec![first])
                .push(file.raw);
        } else {
            report
                .raw_of_untracked
                .insert(file.norm.clone(), file.raw.clone());
            report.raw_of.insert(file.norm.clone(), file.raw);
            report.untracked.push(file.norm);
        }
    }
    for path in index.rows.keys() {
        if !seen_tracked.contains(path) {
            report.deleted.push(path.clone());
        }
    }
    for list in [
        &mut report.modified,
        &mut report.deleted,
        &mut report.untracked,
    ] {
        list.sort();
        list.dedup();
    }
    Ok(report)
}

/// Human output: `clean`, or `dirty:` plus one line per change.
pub fn print_report(report: &Report) {
    if report.is_clean() {
        println!("clean");
        return;
    }
    println!("dirty:");
    for path in &report.modified {
        println!("M {path}");
    }
    for path in &report.deleted {
        println!("D {path}");
    }
    for path in &report.untracked {
        println!("? {path}");
    }
}
