//! One-way git import (M2b): a local git repository becomes naatre
//! changesets, offline, under the data dir flock.
//!
//! The flow, in order: the source is a LOCAL git repository opened through
//! gix (URLs are refused — import never touches the network); the default
//! branch is HEAD's symbolic target (falling back to the sole branch;
//! ambiguous → error); every ancestor of its tip not already in `git_map`
//! imports in topological order (parents before children), one changeset
//! per commit: parents = mapped parent ids in git parent order, author
//! `Name <email>` from the git signature, `timestamp_ns` from the author
//! time, message with `\r\n` → `\n` (the one documented mutation), changes
//! = tree diff against the first parent (a root commit diffs against the
//! empty tree). Each commit's root tree is built directly from the git tree
//! via [`MemNode`] — never by replaying deltas — and every write lands in a
//! per-commit transaction that also carries the commit's `git_map` row,
//! after all of the commit's blobs were put (the
//! fsync-before-commit invariant: a crash leaves orphans, never dangling
//! rows).
//!
//! Unsupported content (octopus parents, symlink/gitlink entries, non-UTF-8
//! or grammar-invalid paths, blobs over the CAS cap) is reported per object.
//! Default mode aborts the whole import — no import state is written (the
//! audit trail's `import` row records that the run happened and refused).
//! With `--skip-unsupported`, offending commits are skipped and their
//! descendants cascade (a descendant whose git parent is missing from the
//! map cannot import); the report lists both groups. Author or message text
//! that violates the Cc+Cf rule is never a skip: after `\r\n`-normalization
//! every offending character becomes U+FFFD and the commit is listed in
//! `sanitized`.
//!
//! The `main` ref moves only when import is the sole writer: absent → set
//! to the new head; present and mapping (via `git_map`) to a git commit
//! that is an ancestor of the source's new HEAD → fast-forward (in skip
//! mode, to the head of the longest importable chain that still descends
//! from it); present and pointing at native naatre changesets → refused
//! (imported commits are dangling heads, fetchable by id); pointing at a
//! git commit that is NOT an ancestor of the new HEAD (force-pushed
//! source) → abort with `git_history_diverged` semantics before anything is
//! imported.
//!
//! Simplicity notes (deliberate at M2b): the two-way tree diff is computed
//! here over plain tree iteration (the facade's diff platform would pull in
//! the attributes/excludes/index dependency chain); blob contents are read
//! on demand without a cross-commit cache; oversized blobs are detected
//! through the object database header so hostile content never enters
//! memory; and per-commit work is O(tree), not incremental.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::path::{Path, PathBuf};

use gix::bstr::{BStr, ByteSlice};
use gix::hash::ObjectId;
use gix::prelude::ObjectIdExt;
use naatre_store::{Db, DbError, Sql, pv};

use naatre_core::changeset::{self, Changeset, FileChange, Op};
use naatre_core::error::CoreError;
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::path;
use naatre_store::error::StoreError;
use naatre_store::{DataDir, Store};

use crate::db;
use crate::tree_derive::{self, MemEntry, MemNode};

/// The audit principal for host-local import (same trust boundary as
/// `init`/`token --new-admin`: the 0700 data dir).
const PRINCIPAL: &str = "admin";

/// Blob cap in bytes, mirroring `naatre_store::BLOB_LIMIT`: `BlobStore::put`
/// enforces the cap on write, but the scan must reject first and report a
/// per-object reason instead of failing the run.
const BLOB_LIMIT: u64 = 64 * 1024 * 1024;

const TREE_MODE: u16 = 0o040000;
const SYMLINK_MODE: u16 = 0o120000;
const GITLINK_MODE: u16 = 0o160000;
const EXEC_MODE: u16 = 0o100755;
const REGULAR_MODE: u16 = 0o100644;

// ---- public surface --------------------------------------------------------

/// Parameters of one import run.
#[derive(Debug, Clone)]
pub struct ImportOptions {
    /// Path of the LOCAL git repository (checkout or bare). URLs are
    /// refused.
    pub git_path: PathBuf,
    /// Target naatre repo name; `None` derives it from the git dir's
    /// basename, sanitized to the repo-name grammar.
    pub into: Option<String>,
    /// Skip unsupported commits (and cascade to their descendants) instead
    /// of aborting the whole import.
    pub skip_unsupported: bool,
}

/// One skipped commit: the git oid plus the reason that disqualified it.
#[derive(Debug, Clone, serde::Serialize)]
pub struct Skipped {
    pub oid: String,
    pub reason: String,
}

/// The outcome of one import run, rendered as the report's final JSON line.
///
/// `status` is `"ok"` or `"aborted"`; `main` is `"set"`, `"fast-forwarded"`,
/// `"unchanged"`, or `"refused"`.
#[derive(Debug, Clone)]
pub struct Report {
    pub status: &'static str,
    pub repo: String,
    /// Whether the target repo row was created by this run.
    pub repo_created: bool,
    pub imported: u64,
    pub skipped: Vec<Skipped>,
    pub cascaded: Vec<String>,
    pub sanitized: Vec<String>,
    pub main: &'static str,
    pub reason: Option<String>,
}

impl Report {
    /// The final JSON line of the stdout report.
    pub fn to_json_line(&self) -> String {
        serde_json::json!({
            "imported": self.imported,
            "skipped": self.skipped,
            "cascaded": self.cascaded,
            "sanitized": self.sanitized,
            "main": self.main,
            "reason": self.reason,
        })
        .to_string()
    }
}

/// Everything an import can fail with. Aborts that produce a report
/// (unsupported content, diverged history) are `Ok(Report)` with
/// `status: "aborted"`; `ImportError` is for hard failures (lock
/// contention, bad source, broken store).
#[derive(Debug, thiserror::Error)]
pub enum ImportError {
    /// Another naatre process (a running server, an in-flight import) holds
    /// the data dir flock. The remedy is part of the message.
    #[error(
        "data dir is locked by another naatre process (lockfile: {0}); \
         stop naatre serve before importing"
    )]
    LockHeld(PathBuf),
    /// The data dir could not be opened or locked.
    #[error(transparent)]
    DataDir(#[from] StoreError),
    /// The git source could not be opened or read.
    #[error("git source: {0}")]
    Source(String),
    /// The argument is a URL; import is offline and takes a local path.
    #[error(
        "{0:?} looks like a URL; naatre import is offline and takes the path \
         of a local git repository"
    )]
    UrlSource(PathBuf),
    /// Bad invocation: repo-name grammar, unnameable target, ambiguous or
    /// unborn default branch.
    #[error("{0}")]
    Config(String),
    #[error(transparent)]
    Sql(#[from] DbError),
    #[error(transparent)]
    Core(#[from] CoreError),
    /// An internal invariant broke (never client input); names the breach.
    #[error("internal error: {0}")]
    Internal(String),
}

/// Runs one import against the data dir at `data`, holding its exclusive
/// flock for the duration. Silent: the caller renders [`Report`]. `derive`
/// is the caller's derived-data registry: every imported commit is
/// enqueued (or eagerly derived) for every registered kind, inside its own
/// commit transaction — the serving process's `naatre import` passes an
/// EMPTY registry (kinds are registered by `serve`/M6b; the backlog
/// backfills through the same queue on the serving side).
pub fn run(
    data: &Path,
    opts: &ImportOptions,
    derive: &crate::derive::DeriveRegistry,
) -> Result<Report, ImportError> {
    run_backend(data, &naatre_store::backend::load(data)?, opts, derive)
}

/// [`run`] for the CONFIGURED backend: `naatre import` is offline but
/// writes metadata, so it honors `backend.toml` exactly like `serve` —
/// same open guard, same mismatch refusal, same migrations.
pub fn run_backend(
    data: &Path,
    cfg: &naatre_store::BackendConfig,
    opts: &ImportOptions,
    derive: &crate::derive::DeriveRegistry,
) -> Result<Report, ImportError> {
    reject_url(&opts.git_path)?;
    let repo_name = match &opts.into {
        Some(name) => {
            if !valid_repo_name(name) {
                return Err(ImportError::Config(format!(
                    "--into {name:?} does not match ^[a-z0-9][a-z0-9-]{{0,62}}$"
                )));
            }
            name.clone()
        }
        None => derived_repo_name(&opts.git_path)?,
    };

    let data_dir = DataDir::open_any(data);
    naatre_store::backend::check_data_dir_backend(data_dir.path(), cfg)
        .map_err(ImportError::DataDir)?;
    let _lock = match data_dir.lock() {
        Ok(lock) => lock,
        Err(StoreError::LockHeld(path)) => return Err(ImportError::LockHeld(path)),
        Err(e) => return Err(ImportError::DataDir(e)),
    };

    let git = gix::discover(&opts.git_path).map_err(|e| {
        ImportError::Source(format!("cannot open {}: {e}", opts.git_path.display()))
    })?;
    let head_oid = resolve_default_branch_head(&git)?;
    let git_head_hex = head_oid.to_string();

    // The CONFIGURED CAS (fs, or the S3 bucket when blob = "s3") — the
    // import's blob writes must land where the server reads them.
    let cas = naatre_store::blob::history_cas(&data_dir, cfg)?;
    let store = Store::open_backend(data_dir, cfg)?;
    let conn = store.conn();

    // Target repo, created (and audited) when absent.
    let existing = db::repo_id_by_name(conn, &repo_name)?;
    let (repo_id, repo_created) = match existing {
        Some(id) => (id, false),
        None => {
            let now = now_ns();
            let tx = conn.transaction()?;
            let id = db::repos_create(&tx, &repo_name, now)?;
            db::audit_append(
                &tx,
                now,
                PRINCIPAL,
                "repo_create",
                &serde_json::json!({ "repo": repo_name }).to_string(),
            )?;
            tx.commit()?;
            (id, true)
        }
    };

    // Import state so far: git oid → changeset id (and back).
    let (git_map, reverse_map) = load_git_map(conn, repo_id)?;

    // The to-import set: every ancestor of HEAD that is not yet mapped,
    // parents before children.
    let batch = collect_ancestors(&git, head_oid, &git_map)?;

    let mut report = Report {
        status: "ok",
        repo: repo_name.clone(),
        repo_created,
        imported: 0,
        skipped: Vec::new(),
        cascaded: Vec::new(),
        sanitized: Vec::new(),
        main: "unchanged",
        reason: None,
    };

    if batch.is_empty() {
        finish(conn, &repo_name, &git_head_hex, &report)?;
        return Ok(report);
    }

    // DIVERGENCE: when main maps to a git oid (import was its writer), that
    // commit must still be an ancestor of the source's new HEAD — otherwise
    // the source's history was rewritten and importing would graft onto a
    // sibling. Checked before anything is imported.
    let main_row = db::ref_get(conn, repo_id, "main")?;
    let main_git_oid: Option<ObjectId> = main_row
        .as_ref()
        .and_then(|row| row.target.as_deref())
        .and_then(|target| {
            let cs = Id(target.try_into().ok()?);
            reverse_map.get(&cs).copied()
        });
    if let Some(old_oid) = main_git_oid {
        let ancestor = is_ancestor(&git, old_oid, head_oid).map_err(|e| {
            ImportError::Source(format!(
                "cannot check ancestry of previously imported commit {old_oid}: {e}"
            ))
        })?;
        if !ancestor {
            report.status = "aborted";
            report.reason = Some(format!(
                "git_history_diverged: main's git commit {old_oid} is not an ancestor \
                 of the source's HEAD {git_head_hex}; the source history was rewritten"
            ));
            finish(conn, &repo_name, &git_head_hex, &report)?;
            return Ok(report);
        }
    }

    // Scan: decode and validate every commit of the batch (no writes), in
    // topological order so parents resolve before their children.
    let mut importable: Vec<PreparedCommit> = Vec::with_capacity(batch.len());
    let mut batch_cs: HashMap<ObjectId, (Id, i64)> = HashMap::new(); // git oid → (cs id, generation)
    let mut offending: Vec<(ObjectId, String)> = Vec::new();
    let mut parent_missing: Vec<ObjectId> = Vec::new();
    for commit in &batch {
        match prepare_commit(&git, &store, repo_id, commit, &git_map, &batch_cs)? {
            Prepared::Ready(p) => {
                batch_cs.insert(commit.oid, (p.cs_id, p.generation));
                importable.push(p);
            }
            Prepared::Unsupported(reasons) => {
                for reason in reasons {
                    offending.push((commit.oid, reason));
                }
            }
            Prepared::ParentMissing => parent_missing.push(commit.oid),
        }
    }

    // Default mode: any unsupported content aborts the whole import; the
    // report lists every offending oid + reason.
    if !offending.is_empty() && !opts.skip_unsupported {
        report.status = "aborted";
        report.skipped = offending
            .iter()
            .map(|(oid, reason)| Skipped {
                oid: oid.to_string(),
                reason: reason.clone(),
            })
            .collect();
        report.reason = Some(format!(
            "{} unsupported commit ent{}; nothing was imported. \
             Re-run with --skip-unsupported to import the rest",
            offending.len(),
            if offending.len() == 1 { "y" } else { "ies" },
        ));
        finish(conn, &repo_name, &git_head_hex, &report)?;
        return Ok(report);
    }

    // Skip mode: offending commits are skipped; their descendants cascade
    // (ParentMissing — a git parent that is neither mapped nor importable
    // leaves the child without a base).
    for oid in &parent_missing {
        report.cascaded.push(oid.to_string());
    }
    // One report entry per skipped oid (its first reason), topological order.
    let mut seen = HashSet::new();
    for (oid, reason) in &offending {
        if seen.insert(*oid) {
            report.skipped.push(Skipped {
                oid: oid.to_string(),
                reason: reason.clone(),
            });
        }
    }

    // Import: blobs are put before the per-commit transaction (durability
    // invariant 1); the transaction carries the changeset, its parents,
    // file changes, blob membership, git_map row, derived trees, and audit.
    let mut git_map = git_map;
    for p in &importable {
        import_commit(&git, cas.as_ref(), conn, repo_id, &repo_name, derive, p)?;
        git_map.insert(p.git_oid, p.cs_id);
        report.imported += 1;
        if p.sanitized {
            report.sanitized.push(p.git_oid.to_string());
        }
    }

    // Graft main, but only when at least one new commit arrived.
    if report.imported >= 1 {
        graft_main(
            conn,
            repo_id,
            &repo_name,
            main_row.as_ref(),
            main_git_oid,
            &git_map,
            &importable,
            &head_oid,
            &mut report,
        )?;
    }

    finish(conn, &repo_name, &git_head_hex, &report)?;
    Ok(report)
}

/// Writes the closing `import` audit row. Runs for successful imports AND
/// aborts — the audit trail records that the run happened and what it did;
/// "nothing written" for aborts refers to import state (changesets, trees,
/// blobs, refs, git_map), not the ops journal.
fn finish(
    conn: &Db,
    repo_name: &str,
    git_head_hex: &str,
    report: &Report,
) -> Result<(), ImportError> {
    let tx = conn.transaction()?;
    db::audit_append(
        &tx,
        now_ns(),
        PRINCIPAL,
        "import",
        &serde_json::json!({
            "repo": repo_name,
            "git_head": git_head_hex,
            "status": report.status,
            "imported": report.imported,
            "skipped": report.skipped.len(),
            "cascaded": report.cascaded.len(),
            "main": report.main,
        })
        .to_string(),
    )?;
    tx.commit()?;
    Ok(())
}

// ---- naming ----------------------------------------------------------------

/// `^[a-z0-9][a-z0-9-]{0,62}$` — mirrors `routes::repos::validate_repo_name`
/// (that validator is route-private and db.rs is out of this slice's scope).
fn valid_repo_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 63
        && name.chars().enumerate().all(|(i, c)| {
            let alnum = c.is_ascii_lowercase() || c.is_ascii_digit();
            if i == 0 { alnum } else { alnum || c == '-' }
        })
}

/// Derives the target repo name from the git dir's basename: a trailing
/// `.git` is stripped (covers `foo.git` checkouts; bare `foo.git` repos
/// named on the command line keep their stem), the rest is lowercased with
/// every character outside `[a-z0-9-]` mapped to `-`, leading `-`s trimmed,
/// and the result truncated to 63 bytes.
fn derived_repo_name(git_path: &Path) -> Result<String, ImportError> {
    let raw = git_path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .ok_or_else(|| {
            ImportError::Config(format!(
                "cannot derive a repo name from {} (no file name); pass --into <name>",
                git_path.display()
            ))
        })?;
    let base = raw.strip_suffix(".git").unwrap_or(&raw);
    let name: String = base
        .to_lowercase()
        .chars()
        .map(|c| {
            if c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' {
                c
            } else {
                '-'
            }
        })
        .skip_while(|c| *c == '-')
        .collect();
    let name = name.chars().take(63).collect::<String>();
    if !valid_repo_name(&name) {
        return Err(ImportError::Config(format!(
            "cannot derive a repo name from {raw:?}; pass --into <name>"
        )));
    }
    Ok(name)
}

/// Import is offline: refuse anything that looks like a URL instead of
/// letting gix fail somewhere deeper. Recognized: scheme spellings
/// (`https://…`, `https:…`, `ssh:…`) and scp-like `user@host:path`.
/// Single-letter drive prefixes and plain paths are left alone.
fn reject_url(git_path: &Path) -> Result<(), ImportError> {
    let s = git_path.to_string_lossy();
    let has_scheme = match s.split_once(':') {
        Some((scheme, _)) if scheme.len() >= 2 => {
            let mut chars = scheme.chars();
            chars.next().is_some_and(|c| c.is_ascii_alphabetic())
                && chars.all(|c| c.is_ascii_alphanumeric() || matches!(c, '+' | '.' | '-'))
        }
        _ => false,
    };
    let scp_like = !s.starts_with(['/', '.']) && s.contains('@') && s.contains(':');
    if s.contains("://") || has_scheme || scp_like {
        return Err(ImportError::UrlSource(git_path.to_path_buf()));
    }
    Ok(())
}

// ---- git side --------------------------------------------------------------

/// One decoded git commit of the to-import batch.
struct GitCommit {
    oid: ObjectId,
    parents: Vec<ObjectId>,
    tree: ObjectId,
}

/// Resolves the default branch tip: HEAD's symbolic target, falling back to
/// the sole local branch when HEAD is detached or unborn; several branches
/// and no symbolic HEAD → error naming the branches.
fn resolve_default_branch_head(git: &gix::Repository) -> Result<ObjectId, ImportError> {
    let head = git
        .head()
        .map_err(|e| ImportError::Source(format!("cannot read HEAD: {e}")))?;
    if !head.is_unborn()
        && !head.is_detached()
        && let Some(id) = head.id()
    {
        return Ok(id.detach());
    }
    let platform = git
        .references()
        .map_err(|e| ImportError::Source(format!("cannot list references: {e}")))?;
    let iter = platform
        .local_branches()
        .map_err(|e| ImportError::Source(format!("cannot list local branches: {e}")))?;
    let mut branches: Vec<String> = Vec::new();
    let mut ids: Vec<ObjectId> = Vec::new();
    for r in iter {
        let r = r.map_err(|e| ImportError::Source(format!("cannot read a branch: {e}")))?;
        if let Some(id) = r.try_id() {
            branches.push(r.name().to_string());
            ids.push(id.detach());
        }
    }
    match branches.len() {
        1 => Ok(ids[0]),
        0 => Err(ImportError::Config(
            "source repository has no commits on any branch".into(),
        )),
        _ => Err(ImportError::Config(format!(
            "HEAD is not symbolic and the source has {} branches; import a \
             repository whose HEAD names its default branch (branches: {})",
            branches.len(),
            branches.join(", ")
        ))),
    }
}

/// Every ancestor of `head` that is not in `git_map`, parents before
/// children (iterative DFS post-order). Descent stops at mapped commits:
/// their ancestry is already imported.
fn collect_ancestors(
    git: &gix::Repository,
    head: ObjectId,
    git_map: &HashMap<ObjectId, Id>,
) -> Result<Vec<GitCommit>, ImportError> {
    enum Frame {
        Enter(ObjectId),
        Emit(ObjectId),
    }
    let mut out = Vec::new();
    let mut visited: HashSet<ObjectId> = HashSet::new();
    let mut decoded: HashMap<ObjectId, GitCommit> = HashMap::new();
    let mut stack = vec![Frame::Enter(head)];
    while let Some(frame) = stack.pop() {
        match frame {
            Frame::Emit(oid) => {
                if let Some(c) = decoded.remove(&oid) {
                    out.push(c);
                }
            }
            Frame::Enter(oid) => {
                if visited.contains(&oid) || git_map.contains_key(&oid) {
                    continue;
                }
                visited.insert(oid);
                let commit = git
                    .find_commit(oid)
                    .map_err(|e| ImportError::Source(format!("cannot read commit {oid}: {e}")))?;
                let parents: Vec<ObjectId> = commit.parent_ids().map(|id| id.detach()).collect();
                stack.push(Frame::Emit(oid));
                for parent in &parents {
                    stack.push(Frame::Enter(*parent));
                }
                let tree = commit
                    .tree_id()
                    .map_err(|e| ImportError::Source(format!("commit {oid} has no tree: {e}")))?
                    .detach();
                decoded.insert(oid, GitCommit { oid, parents, tree });
            }
        }
    }
    Ok(out)
}

/// True when `ancestor` is `head` or reachable from `head` through git
/// parent links. A missing object aborts with [`ImportError::Source`] —
/// ancestry that cannot be verified must not be assumed.
fn is_ancestor(
    git: &gix::Repository,
    ancestor: ObjectId,
    head: ObjectId,
) -> Result<bool, ImportError> {
    if ancestor == head {
        return Ok(true);
    }
    let mut seen: HashSet<ObjectId> = HashSet::new();
    let mut stack = vec![head];
    while let Some(oid) = stack.pop() {
        let commit = git
            .find_commit(oid)
            .map_err(|e| ImportError::Source(format!("cannot read commit {oid}: {e}")))?;
        for parent in commit.parent_ids() {
            let parent = parent.detach();
            if parent == ancestor {
                return Ok(true);
            }
            if seen.insert(parent) {
                stack.push(parent);
            }
        }
    }
    Ok(false)
}

// ---- tree diff (two-way, over plain tree iteration) ------------------------

/// One changed path between two git trees, with both worlds' ids: the git
/// blob (for reading) and — after the scan — the naatre blob id the
/// content will land under.
#[derive(Debug)]
struct DiffEntry {
    path: String,
    kind: DiffKind,
}

#[derive(Debug)]
enum DiffKind {
    /// New file (the first-parent tree is absent for a root commit).
    Add { mode: u16, blob: ObjectId },
    /// Changed content and/or mode.
    Mod { mode: u16, blob: ObjectId },
    /// Removed file.
    Del,
}

impl DiffKind {
    fn mode(&self) -> Option<u16> {
        match self {
            DiffKind::Add { mode, .. } | DiffKind::Mod { mode, .. } => Some(*mode),
            DiffKind::Del => None,
        }
    }

    fn blob(&self) -> Option<ObjectId> {
        match self {
            DiffKind::Add { blob, .. } | DiffKind::Mod { blob, .. } => Some(*blob),
            DiffKind::Del => None,
        }
    }

    fn op(&self) -> Op {
        match self {
            DiffKind::Add { .. } => Op::Add,
            DiffKind::Mod { .. } => Op::Mod,
            DiffKind::Del => Op::Del,
        }
    }
}

/// Collects the file-level delta that turns `old` into `new` (both git tree
/// ids; `old` `None` = empty tree, the root-commit case), sorted by path
/// bytes. Directory add/remove recurse so only file entries are emitted.
fn tree_diff(
    git: &gix::Repository,
    old: Option<ObjectId>,
    new: ObjectId,
) -> Result<Vec<DiffEntry>, ImportError> {
    let old_tree = match old {
        Some(id) => Some(find_tree(git, id)?),
        None => None,
    };
    let new_tree = find_tree(git, new)?;
    let mut out = Vec::new();
    diff_node(git, old_tree.as_ref(), &new_tree, "", &mut out)?;
    // Recursive emission is not path-sorted (a deleted directory `a/`
    // emits `a/x` before the sibling file `a.txt`, yet `a.txt` < `a/x` in
    // byte order — git sorts directories as `name/`); the canonical format
    // needs strict byte order, so sort at the boundary.
    out.sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));
    debug_assert!(
        out.windows(2)
            .all(|w| w[0].path.as_bytes() < w[1].path.as_bytes())
    );
    Ok(out)
}

fn diff_node(
    git: &gix::Repository,
    old: Option<&gix::Tree<'_>>,
    new: &gix::Tree<'_>,
    prefix: &str,
    out: &mut Vec<DiffEntry>,
) -> Result<(), ImportError> {
    let old_entries = tree_entries(old)?;
    let new_entries = tree_entries(Some(new))?;
    // Both maps iterate in name-byte order, so a merged walk of the two key
    // sets yields byte-ordered paths.
    let mut names: Vec<&BStr> = old_entries.keys().copied().collect();
    names.extend(new_entries.keys().copied());
    names.sort_unstable();
    names.dedup();
    for name in names {
        let path = format!("{prefix}{}", name.to_str_lossy());
        match (old_entries.get(name), new_entries.get(name)) {
            (None, None) => unreachable!("name came from one of the maps"),
            (Some(old_e), None) => delete_entry(git, old_e.mode.value(), old_e.oid, &path, out)?,
            (None, Some(new_e)) => {
                emit_entry(git, new_e.mode.value(), new_e.oid, &path, out, true)?
            }
            (Some(old_e), Some(new_e)) => {
                let old_is_dir = old_e.mode.value() == TREE_MODE;
                let new_is_dir = new_e.mode.value() == TREE_MODE;
                match (old_is_dir, new_is_dir) {
                    (true, true) => {
                        if old_e.oid != new_e.oid {
                            let old_sub = find_tree(git, old_e.oid)?;
                            let new_sub = find_tree(git, new_e.oid)?;
                            diff_node(git, Some(&old_sub), &new_sub, &format!("{path}/"), out)?;
                        }
                    }
                    // A file↔directory flip is a delete plus an add: the
                    // paths never collide because the directory side
                    // recurses to its leaves.
                    (true, false) | (false, true) => {
                        delete_entry(git, old_e.mode.value(), old_e.oid, &path, out)?;
                        emit_entry(git, new_e.mode.value(), new_e.oid, &path, out, true)?;
                    }
                    (false, false) => {
                        if old_e.oid != new_e.oid || old_e.mode != new_e.mode {
                            emit_entry(git, new_e.mode.value(), new_e.oid, &path, out, false)?;
                        }
                    }
                }
            }
        }
    }
    Ok(())
}

/// Emits `Del` rows for every file under a removed (sub)tree.
fn delete_entry(
    git: &gix::Repository,
    mode: u16,
    oid: ObjectId,
    path: &str,
    out: &mut Vec<DiffEntry>,
) -> Result<(), ImportError> {
    if mode == TREE_MODE {
        for (name, entry) in tree_entries(Some(&find_tree(git, oid)?))? {
            let sub = format!("{path}/{}", name.to_str_lossy());
            delete_entry(git, entry.mode.value(), entry.oid, &sub, out)?;
        }
        return Ok(());
    }
    out.push(DiffEntry {
        path: path.to_string(),
        kind: DiffKind::Del,
    });
    Ok(())
}

/// Emits one file entry, recursing for directories. `added` selects `Add`
/// (new subtree/root-commit side) or `Mod` (both sides present).
fn emit_entry(
    git: &gix::Repository,
    mode: u16,
    oid: ObjectId,
    path: &str,
    out: &mut Vec<DiffEntry>,
    added: bool,
) -> Result<(), ImportError> {
    if mode == TREE_MODE {
        for (name, entry) in tree_entries(Some(&find_tree(git, oid)?))? {
            let sub = format!("{path}/{}", name.to_str_lossy());
            emit_entry(git, entry.mode.value(), entry.oid, &sub, out, added)?;
        }
        return Ok(());
    }
    let kind = if added {
        DiffKind::Add { mode, blob: oid }
    } else {
        DiffKind::Mod { mode, blob: oid }
    };
    out.push(DiffEntry {
        path: path.to_string(),
        kind,
    });
    Ok(())
}

/// Flattens a tree into name → (mode, oid). The map makes the merge-join in
/// [`diff_node`] order-guaranteed and dedup-safe.
fn tree_entries<'a>(
    tree: Option<&'a gix::Tree<'a>>,
) -> Result<BTreeMap<&'a BStr, TreeEntry>, ImportError> {
    let mut map = BTreeMap::new();
    let Some(tree) = tree else {
        return Ok(map);
    };
    for entry in tree.iter() {
        let entry = entry.map_err(|e| ImportError::Source(format!("malformed tree entry: {e}")))?;
        map.insert(
            entry.inner.filename,
            TreeEntry {
                mode: entry.inner.mode,
                oid: entry.inner.oid.to_owned(),
            },
        );
    }
    Ok(map)
}

struct TreeEntry {
    mode: gix::objs::tree::EntryMode,
    oid: ObjectId,
}

fn find_tree(git: &gix::Repository, oid: ObjectId) -> Result<gix::Tree<'_>, ImportError> {
    git.find_tree(oid)
        .map_err(|e| ImportError::Source(format!("cannot read tree {oid}: {e}")))
}

// ---- scan: prepare commits without writing ---------------------------------

/// One validated change of a prepared commit, carrying both worlds: the git
/// blob (to read and put at import time) and the naatre blob id the content
/// will land under.
struct PreparedEntry {
    path: String,
    op: Op,
    git_blob: Option<ObjectId>,
    blob: Option<Id>,
    size: Option<u64>,
    exec: Option<bool>,
}

impl PreparedEntry {
    fn to_file_change(&self) -> FileChange {
        FileChange {
            path: self.path.clone(),
            op: self.op,
            blob: self.blob,
            size: self.size,
            exec: self.exec,
        }
    }
}

/// A batch commit that passed the scan: everything needed to import it.
struct PreparedCommit {
    git_oid: ObjectId,
    git_tree: ObjectId,
    cs_id: Id,
    cs: Changeset,
    entries: Vec<PreparedEntry>,
    generation: i64,
    /// The commit's message or author needed U+FFFD replacement.
    sanitized: bool,
}

/// The scan's verdict on one batch commit. `Unsupported` carries the
/// commit's own content problems; `ParentMissing` marks the cascade case
/// (a git parent that is neither mapped nor importable — in default mode
/// this only ever sits under an `Unsupported` ancestor, which aborts).
enum Prepared {
    Ready(PreparedCommit),
    Unsupported(Vec<String>),
    ParentMissing,
}

/// Scans one commit: signature, message (the `\r\n`→`\n` mutation plus
/// Cc+Cf cleanup), and the tree diff against the first parent, with every
/// entry validated. Content problems come back as per-object reasons;
/// unreadable git objects abort the import. Parents resolve only after the
/// content checks — an unsupported commit must report its own reasons, not
/// a cascade artifact.
fn prepare_commit(
    git: &gix::Repository,
    store: &Store,
    repo_id: i64,
    commit: &GitCommit,
    git_map: &HashMap<ObjectId, Id>,
    batch_cs: &HashMap<ObjectId, (Id, i64)>,
) -> Result<Prepared, ImportError> {
    let mut reasons: Vec<String> = Vec::new();

    // Parents: at most two, distinct.
    if commit.parents.len() > 2 {
        reasons.push(format!(
            "octopus commit: {} parents (naatre allows 2)",
            commit.parents.len()
        ));
    }
    for pair in commit.parents.windows(2) {
        if pair[0] == pair[1] {
            reasons.push("duplicate parent entry".to_string());
        }
    }

    // Author and message. Name/email/message are raw bytes: lossy UTF-8,
    // and any replacement counts as sanitization.
    let commit_obj = git
        .find_commit(commit.oid)
        .map_err(|e| ImportError::Source(format!("cannot read commit {}: {e}", commit.oid)))?;
    let sig = commit_obj
        .author()
        .map_err(|e| ImportError::Source(format!("malformed author signature: {e}")))?
        .trim();
    let mut sanitized = false;
    let (name, email) = (
        String::from_utf8_lossy(sig.name),
        String::from_utf8_lossy(sig.email),
    );
    if name.contains('\u{FFFD}') || email.contains('\u{FFFD}') {
        sanitized = true;
    }
    let author = format!("{} <{}>", name.trim(), email.trim());
    let raw_message = commit_obj
        .message_raw()
        .map_err(|e| ImportError::Source(format!("malformed commit message: {e}")))?;
    // The one documented mutation: \r\n → \n.
    let message = String::from_utf8_lossy(raw_message).replace("\r\n", "\n");
    let seconds = sig
        .time()
        .map_err(|e| ImportError::Source(format!("malformed author timestamp: {e}")))?
        .seconds;
    let timestamp_ns = if seconds < 0 {
        0u64
    } else {
        (seconds as u64).saturating_mul(1_000_000_000)
    };

    // Tree diff against the first parent (none for a root commit).
    let first_parent_tree = match commit.parents.first() {
        Some(p) => Some(
            git.find_commit(*p)
                .map_err(|e| ImportError::Source(format!("cannot read parent commit {p}: {e}")))?
                .tree_id()
                .map_err(|e| ImportError::Source(format!("parent has no tree: {e}")))?
                .detach(),
        ),
        None => None,
    };
    let diff = tree_diff(git, first_parent_tree, commit.tree)
        .map_err(|e| ImportError::Source(e.to_string()))?;

    // Per-entry validation. Every offender is reported. The naatre blob id
    // is content-addressed, so it is computable here — reading content only
    // after the header-based cap check passed.
    let mut entries: Vec<PreparedEntry> = Vec::with_capacity(diff.len());
    for entry in &diff {
        if let Err(reason) = validate_entry(git, entry) {
            reasons.push(reason);
            continue;
        }
        let (git_blob, blob, size) = match entry.kind.blob() {
            Some(git_blob) => {
                let content =
                    read_blob(git, git_blob).map_err(|e| ImportError::Source(e.to_string()))?;
                (
                    Some(git_blob),
                    Some(object_id(Kind::Blob, &content)),
                    Some(content.len() as u64),
                )
            }
            None => (None, None, None),
        };
        entries.push(PreparedEntry {
            path: entry.path.clone(),
            op: entry.kind.op(),
            git_blob,
            blob,
            size,
            exec: entry.kind.mode().map(|m| m == EXEC_MODE),
        });
    }
    if let Err(e) = path::check_case_fold_duplicates(
        &entries.iter().map(|e| e.path.clone()).collect::<Vec<_>>(),
    ) {
        reasons.push(e.to_string());
    }
    if entries.len() > changeset::MAX_ENTRIES {
        reasons.push(format!(
            "{} entries in one commit (limit {})",
            entries.len(),
            changeset::MAX_ENTRIES
        ));
    }
    if !reasons.is_empty() {
        return Ok(Prepared::Unsupported(reasons));
    }

    // Parents resolve only after the content checks passed: a git parent
    // that is neither mapped nor a ready batch commit means an unsupported
    // or cascade-skipped ancestor — the descendant cannot import.
    let mut parent_cs: Vec<Id> = Vec::with_capacity(commit.parents.len());
    for parent in &commit.parents {
        match git_map.get(parent) {
            Some(cs) => parent_cs.push(*cs),
            None => match batch_cs.get(parent) {
                Some((cs, _)) => parent_cs.push(*cs),
                None => return Ok(Prepared::ParentMissing),
            },
        }
    }

    // Cc+Cf cleanup on author/message (never a skip): naatre's own validator
    // is the oracle; each reported character becomes U+FFFD until clean.
    let (author, message, text_sanitized) = sanitize_pair(author, message);
    sanitized |= text_sanitized;

    // Merge-join order is already byte-sorted; the sort is a cheap pin.
    entries.sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));
    let cs = Changeset {
        repo_id: repo_id as u64,
        parents: parent_cs,
        author,
        message,
        timestamp_ns,
        changes: entries.iter().map(PreparedEntry::to_file_change).collect(),
    };
    let canonical = changeset::encode(&cs)
        .map_err(|e| ImportError::Source(format!("canonical encoding: {e}")))?;
    let cs_id = object_id(Kind::Changeset, &canonical);

    // Generation: topological (max parent gen + 1). Mapped parents take
    // their generation from the store; batch parents from the scan.
    let mut generation = 0i64;
    for parent in &commit.parents {
        let parent_gen = match batch_cs.get(parent) {
            Some((_, parent_gen)) => *parent_gen,
            None => {
                let cs = git_map.get(parent).expect("parent resolved above");
                db::changeset_graph(store.conn(), repo_id, cs)
                    .map_err(ImportError::from)?
                    .map(|g| g.generation)
                    .ok_or_else(|| {
                        ImportError::Internal("git_map points at a missing changeset".into())
                    })?
            }
        };
        generation = generation.max(parent_gen + 1);
    }

    Ok(Prepared::Ready(PreparedCommit {
        git_oid: commit.oid,
        git_tree: commit.tree,
        cs_id,
        cs,
        entries,
        generation,
        sanitized,
    }))
}

/// Validates one diff entry: path must be grammar-valid (UTF-8 by
/// construction of the lossy decode, NFC included), the mode must be a
/// plain file, and the blob must fit the CAS cap — checked through the
/// object database header so oversized content is never read. Returns the
/// offender's reason.
fn validate_entry(git: &gix::Repository, entry: &DiffEntry) -> Result<(), String> {
    path::validate(&entry.path).map_err(|e| format!("{:?}: {e}", entry.path))?;
    let Some(mode) = entry.kind.mode() else {
        return Ok(()); // deletes have nothing more to check
    };
    match mode {
        EXEC_MODE | REGULAR_MODE => {}
        SYMLINK_MODE => {
            return Err(format!(
                "{:?}: symlink entry (mode 120000) is not supported",
                entry.path
            ));
        }
        GITLINK_MODE => {
            return Err(format!(
                "{:?}: gitlink (submodule) entry (mode 160000) is not supported",
                entry.path
            ));
        }
        other => {
            return Err(format!("{:?}: unsupported mode {other:o}", entry.path));
        }
    }
    let blob = entry.kind.blob().expect("Add/Mod carry a blob");
    let header = oid_header(git, blob).map_err(|e| format!("cannot look up blob {blob}: {e}"))?;
    let size = header.size();
    if size > BLOB_LIMIT {
        Err(format!(
            "blob {blob} for path {:?} is {size} bytes (limit {BLOB_LIMIT})",
            entry.path
        ))
    } else {
        Ok(())
    }
}

/// Object kind and size without decoding the payload (`None` = absent).
fn oid_header(git: &gix::Repository, oid: ObjectId) -> Result<gix::odb::find::Header, ImportError> {
    oid.attach(git)
        .try_header()
        .map_err(|e| ImportError::Source(format!("cannot read object header {oid}: {e}")))?
        .ok_or_else(|| {
            ImportError::Source(format!(
                "object {oid} is missing from the source repository"
            ))
        })
}

fn read_blob(git: &gix::Repository, oid: ObjectId) -> Result<Vec<u8>, ImportError> {
    let blob = git
        .find_blob(oid)
        .map_err(|e| ImportError::Source(format!("cannot read blob {oid}: {e}")))?;
    // `Blob` owns its repo handle (hence Drop), so the data must be copied
    // out; contents are cap-bounded.
    Ok(blob.data.clone())
}

/// `DeriveError` splits into the import error's existing Sql/Core variants;
/// a claimed-but-missing tree row is store corruption, not client input.
fn map_derive(e: tree_derive::DeriveError) -> ImportError {
    match e {
        tree_derive::DeriveError::Sql(err) => ImportError::Sql(err),
        tree_derive::DeriveError::Core(err) => ImportError::Core(err),
        tree_derive::DeriveError::CorruptRow(kind) => ImportError::Internal(format!(
            "corrupt derived row: kind byte {kind} is not a file/dir tag"
        )),
        tree_derive::DeriveError::MissingTreeRow(hex) => ImportError::Internal(format!(
            "corrupt tree store: no trees row for claimed id {hex}"
        )),
    }
}

// ---- sanitization ----------------------------------------------------------

/// Replaces every Cc/Cf character of author and message (after the
/// `\r\n`→`\n` mutation) with U+FFFD, using naatre's own validator as the
/// oracle: a carrier changeset isolates the char rule, and each reported
/// byte index is replaced until the rule passes. Returns the cleaned pair
/// and whether anything changed.
fn sanitize_pair(author: String, message: String) -> (String, String, bool) {
    let mut author = author;
    let mut message = message;
    let mut changed = false;
    for _ in 0..=(author.len() + message.len() + 1) {
        match changeset::validate(&carrier_changeset(&author, &message)) {
            Ok(()) => return (author, message, changed),
            Err(CoreError::AuthorChar { index }) => {
                replace_with_replacement_char(&mut author, index);
                changed = true;
            }
            Err(CoreError::MessageChar { index }) => {
                replace_with_replacement_char(&mut message, index);
                changed = true;
            }
            // The scan validated everything else in the same order core
            // checks it; anything else here is a broken invariant.
            Err(e) => unreachable!("import carrier validation hit an unexpected rule: {e}"),
        }
    }
    unreachable!("sanitize_pair did not converge");
}

/// A minimal valid changeset carrying exactly the strings under test (core
/// validates paths before text, and rejects empty changesets).
fn carrier_changeset(author: &str, message: &str) -> Changeset {
    Changeset {
        repo_id: 0,
        parents: vec![],
        author: author.to_string(),
        message: message.to_string(),
        timestamp_ns: 0,
        changes: vec![FileChange {
            path: "x".into(),
            op: Op::Add,
            blob: Some(Id([0u8; 32])),
            size: Some(0),
            exec: Some(false),
        }],
    }
}

fn replace_with_replacement_char(s: &mut String, index: usize) {
    let c = s[index..]
        .chars()
        .next()
        .expect("validate's index is a char boundary");
    s.replace_range(index..index + c.len_utf8(), "\u{FFFD}");
}

// ---- naatre side: import one commit ----------------------------------------

/// Imports one prepared commit: puts every referenced blob (diff and full
/// tree), then commits the changeset (via [`db::changeset_insert_rows`] —
/// the single changeset-row writer — with the import extras: the git_map
/// row and the `via: "import"` audit field), derived trees, and audit in
/// one transaction.
fn import_commit(
    git: &gix::Repository,
    cas: &dyn naatre_store::BlobStore,
    conn: &Db,
    repo_id: i64,
    repo_name: &str,
    derive: &crate::derive::DeriveRegistry,
    p: &PreparedCommit,
) -> Result<(), ImportError> {
    // Blobs first (durability invariant 1): the diff's Add/Mod blobs.
    let mut blobs: Vec<Id> = Vec::new();
    for e in &p.entries {
        if let (Op::Add | Op::Mod, Some(git_blob), Some(id)) = (e.op, e.git_blob, e.blob)
            && !blobs.contains(&id)
        {
            let content = read_blob(git, git_blob)?;
            let put = cas.put(&content)?;
            if put != id {
                return Err(ImportError::Internal(format!(
                    "blob id mismatch for path {:?}: scan computed {}, CAS put {}",
                    e.path, id, put
                )));
            }
            blobs.push(id);
        }
    }

    // Full root tree, built directly from the git tree (never by replaying
    // deltas), reusing the MemNode persist/collect machinery.
    let node = build_tree_node(git, cas, p.git_tree, "")?;
    let (root_tree_id, _, trees) = node.collect().map_err(map_derive)?;

    let canonical = changeset::encode(&p.cs)?;
    let tx = conn.transaction()?;
    db::changeset_insert_rows(
        &tx,
        db::ChangesetInsert {
            repo_id,
            repo_name,
            id: &p.cs_id,
            canonical: &canonical,
            author: &p.cs.author,
            message: &p.cs.message,
            timestamp_ns: p.cs.timestamp_ns as i64,
            generation: p.generation,
            principal: PRINCIPAL,
            root_tree_id: &root_tree_id,
            parents: &p.cs.parents,
            changes: &p.cs.changes,
            blobs: &blobs,
            trees: &trees,
            now_ns: now_ns(),
        },
        db::ChangesetExtras {
            audit_via: Some("import"),
            git_map_oid: Some(p.git_oid.as_bytes()),
        },
    )?;
    // Derived data (M6a): the imported commit enqueues (or eagerly
    // derives) for every registered kind, in the commit's own transaction.
    crate::derive::enqueue_changeset(
        &tx,
        derive,
        repo_id,
        p.cs_id,
        &p.cs.parents,
        now_ns(),
        crate::state::system_clock(),
    )?;
    tx.commit()?;
    Ok(())
}

/// Recursively turns a git tree into a [`MemNode`], putting every blob it
/// references.
fn build_tree_node(
    git: &gix::Repository,
    cas: &dyn naatre_store::BlobStore,
    tree_oid: ObjectId,
    prefix: &str,
) -> Result<MemNode, ImportError> {
    let mut node = MemNode::default();
    for (name, entry) in tree_entries(Some(&find_tree(git, tree_oid)?))? {
        let name = name.to_str_lossy().into_owned();
        let path = format!("{prefix}{name}");
        match entry.mode.value() {
            TREE_MODE => {
                let sub = build_tree_node(git, cas, entry.oid, &format!("{path}/"))?;
                node.entries.insert(name, MemEntry::Dir(Box::new(sub)));
            }
            mode @ (EXEC_MODE | REGULAR_MODE) => {
                let content = read_blob(git, entry.oid)?;
                let blob = cas.put(&content)?;
                node.entries.insert(
                    name,
                    MemEntry::File {
                        blob,
                        size: content.len() as u64,
                        exec: mode == EXEC_MODE,
                    },
                );
            }
            other => {
                // Unreachable for importable commits: unsupported entries
                // make their commit (and every descendant) skip or abort,
                // so an imported commit's tree cannot contain one. A hit
                // means that invariant broke — abort loudly rather than
                // import a wrong tree.
                return Err(ImportError::Internal(format!(
                    "imported tree contains unsupported entry {path:?} (mode {other:o})"
                )));
            }
        }
    }
    Ok(node)
}

// ---- graft -----------------------------------------------------------------

/// Moves `main` per the graft rules. Only runs after ≥1 new commit.
#[allow(clippy::too_many_arguments)]
fn graft_main(
    conn: &Db,
    repo_id: i64,
    repo_name: &str,
    main_row: Option<&db::RefRow>,
    main_git_oid: Option<ObjectId>,
    git_map: &HashMap<ObjectId, Id>,
    importable: &[PreparedCommit],
    head_oid: &ObjectId,
    report: &mut Report,
) -> Result<(), ImportError> {
    let Some(ref_row) = main_row else {
        // Absent → set to the head of the longest importable chain.
        let target = longest_chain_head(importable, head_oid);
        ref_move(conn, repo_id, repo_name, None, &target)?;
        report.main = "set";
        return Ok(());
    };
    let _ = ref_row;
    let Some(old_oid) = main_git_oid else {
        // Native naatre changesets on main: refuse; the imports stay as
        // dangling heads, fetchable by id.
        report.main = "refused";
        report.reason = Some(
            "native naatre changesets on main; import refuses to move it \
             (the imported commits are dangling heads, fetchable by id)"
                .to_string(),
        );
        return Ok(());
    };
    // main maps to a git commit; the divergence check proved it is an
    // ancestor of the source's new HEAD. Fast-forward to the head of the
    // longest importable chain that descends from it.
    let previous_tip = git_map
        .get(&old_oid)
        .copied()
        .ok_or_else(|| ImportError::Internal("main's git oid lost from git_map".into()))?;
    let version: i64 = conn.query_row_map(
        &Sql::plain("SELECT version FROM refs WHERE repo_id = ?1 AND name = 'main'"),
        &pv![repo_id],
        |r| r.get(0),
    )?;
    match chain_descending_from(conn, repo_id, importable, previous_tip, head_oid)? {
        Some(target) => {
            ref_move(conn, repo_id, repo_name, Some(version), &target)?;
            report.main = "fast-forwarded";
        }
        None => {
            report.reason =
                Some("no imported commit descends from main's target; main left unmoved".into());
        }
    }
    Ok(())
}

/// The importable commit with the maximal generation; ties resolve toward
/// the commit closest to the source HEAD (later in topological order).
fn longest_chain_head(importable: &[PreparedCommit], head_oid: &ObjectId) -> Id {
    importable
        .iter()
        .enumerate()
        .max_by_key(|(i, p)| (p.generation, p.git_oid == *head_oid, *i))
        .map(|(_, p)| p.cs_id)
        .expect("graft runs only with >=1 imported commit")
}

/// The maximal-generation importable commit whose naatre ancestry contains
/// `previous_tip` (`None` when nothing importable descends from it).
fn chain_descending_from(
    conn: &Db,
    repo_id: i64,
    importable: &[PreparedCommit],
    previous_tip: Id,
    head_oid: &ObjectId,
) -> Result<Option<Id>, ImportError> {
    let mut best: Option<(i64, bool, usize, Id)> = None;
    for (i, p) in importable.iter().enumerate() {
        if !descends_from(conn, repo_id, p, previous_tip)? {
            continue;
        }
        let key = (p.generation, p.git_oid == *head_oid, i, p.cs_id);
        if best
            .as_ref()
            .is_none_or(|b| (key.0, key.1, key.2) > (b.0, b.1, b.2))
        {
            best = Some(key);
        }
    }
    Ok(best.map(|b| b.3))
}

/// Naatre-ancestry check: does `p` reach `target` through changeset parent
/// links? Ancestors inside this batch resolve in memory; already-imported
/// ones resolve through the store.
fn descends_from(
    conn: &Db,
    repo_id: i64,
    p: &PreparedCommit,
    target: Id,
) -> Result<bool, ImportError> {
    let mut seen: HashSet<Id> = HashSet::new();
    let mut stack: Vec<Id> = p.cs.parents.clone();
    while let Some(cs) = stack.pop() {
        if cs == target {
            return Ok(true);
        }
        if !seen.insert(cs) {
            continue;
        }
        let graph = db::changeset_graph(conn, repo_id, &cs)
            .map_err(ImportError::from)?
            .ok_or_else(|| {
                ImportError::Internal(format!(
                    "changeset {} referenced as parent but missing",
                    cs.to_hex()
                ))
            })?;
        stack.extend(graph.parents);
    }
    Ok(false)
}

/// Moves `main` through [`db::ref_cas`] — the single ref-move
/// implementation (CAS or create, atomic with its `ref_move` audit row).
/// Under the import flock the CAS cannot lose: import checked it is the
/// sole writer, so any non-success outcome is a broken invariant.
fn ref_move(
    conn: &Db,
    repo_id: i64,
    repo_name: &str,
    expected_version: Option<i64>,
    target: &Id,
) -> Result<(), ImportError> {
    let outcome = db::ref_cas_via(
        conn,
        db::RefMove {
            repo_id,
            repo_name,
            name: "main",
            target,
            expected: expected_version,
            principal: PRINCIPAL,
            now_ns: now_ns(),
        },
        "import",
    )?;
    match outcome {
        db::RefCas::Created | db::RefCas::Updated { .. } => Ok(()),
        db::RefCas::Conflict {
            current_version, ..
        } => Err(ImportError::Internal(format!(
            "ref main changed under the import lock \
             (expected version {expected_version:?}, found {current_version})"
        ))),
        db::RefCas::RepoMissing => Err(ImportError::Internal(
            "repo vanished under the import lock".into(),
        )),
        db::RefCas::RefMissing => Err(ImportError::Internal(
            "ref main vanished under the import lock".into(),
        )),
    }
}

// ---- helpers ---------------------------------------------------------------

fn now_ns() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0)
}

/// The import state for one repo: git oid → changeset id, and back.
type GitMap = (HashMap<ObjectId, Id>, HashMap<Id, ObjectId>);

fn load_git_map(conn: &Db, repo_id: i64) -> Result<GitMap, ImportError> {
    let rows = conn.query_all_map(
        &Sql::plain("SELECT git_oid, cs_id FROM git_map WHERE repo_id = ?1"),
        &pv![repo_id],
        |row| Ok((row.get::<Vec<u8>>(0)?, row.get::<Vec<u8>>(1)?)),
    )?;
    let mut forward = HashMap::new();
    let mut reverse = HashMap::new();
    for (git_oid, cs_id) in rows {
        let git_oid = ObjectId::try_from(&git_oid[..])
            .map_err(|e| ImportError::Internal(format!("corrupt git_map oid: {e}")))?;
        let cs_id = Id(cs_id.try_into().map_err(|_| {
            ImportError::Internal("corrupt git_map changeset id (not 32 bytes)".into())
        })?);
        forward.insert(git_oid, cs_id);
        reverse.insert(cs_id, git_oid);
    }
    Ok((forward, reverse))
}
