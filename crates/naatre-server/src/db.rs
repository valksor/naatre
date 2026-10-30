//! All server-side SQL, as plain synchronous functions over `&Connection`.
//!
//! Callers run these inside [`crate::state::ServerState::read`] /
//! [`crate::state::ServerState::write`] closures (or compose several inside
//! one transaction); no other module in the crate touches SQL. Multi-step
//! mutations (mint + audit, ref CAS + audit) run in a single
//! `unchecked_transaction` here so a failure rolls the whole step back.

use std::collections::HashSet;

use rusqlite::{Connection, OptionalExtension, params};

use naatre_core::ids::Id;

/// One row of `repos`.
pub struct RepoRow {
    pub id: i64,
    pub name: String,
    pub created_ns: i64,
}

/// One row of `refs`.
pub struct RefRow {
    pub name: String,
    pub target: Option<Vec<u8>>,
    pub version: i64,
}

/// The joined `tokens`/`principals` row an auth lookup resolves to.
pub struct AuthRow {
    pub token_id: i64,
    pub principal: String,
    pub role: String,
    pub expires_ns: Option<i64>,
    pub revoked: bool,
    pub scope_path: Option<String>,
}

/// A listed token (token material is never part of any row shape).
pub struct TokenRow {
    pub id: i64,
    pub principal: String,
    pub role: String,
    pub expires_ns: Option<i64>,
    pub scope_path: Option<String>,
    pub revoked: bool,
}

/// One row of `audit_log`.
pub struct AuditRow {
    pub id: i64,
    pub ts_ns: i64,
    pub principal: String,
    pub action: String,
    pub details_json: String,
}

/// Outcome of the compare-and-swap ref move.
pub enum RefCas {
    /// Ref created at version 1.
    Created,
    /// Ref moved to `version`.
    Updated { version: i64 },
    /// CAS lost: the ref's current version/target.
    Conflict {
        current_version: i64,
        current_target: Option<Vec<u8>>,
    },
    /// No such repo.
    RepoMissing,
    /// CAS `expected` was given but the ref does not exist (no version can
    /// match). The caller answers 404 `not_found`.
    RefMissing,
}

/// True when `err` is a UNIQUE-constraint violation.
pub fn is_unique_violation(err: &rusqlite::Error) -> bool {
    matches!(
        err,
        rusqlite::Error::SqliteFailure(ffi, Some(_))
            if ffi.code == rusqlite::ErrorCode::ConstraintViolation
    )
}

// ---- repos ---------------------------------------------------------------

/// Inserts a repo; the caller maps UNIQUE violations to `repo_exists`.
pub fn repos_create(conn: &Connection, name: &str, created_ns: i64) -> rusqlite::Result<i64> {
    conn.execute(
        "INSERT INTO repos(name, created_ns) VALUES (?1, ?2)",
        params![name, created_ns],
    )?;
    Ok(conn.last_insert_rowid())
}

/// Lists repos with `id > after_id`, ascending, up to `limit` rows.
pub fn repos_list(conn: &Connection, after_id: i64, limit: i64) -> rusqlite::Result<Vec<RepoRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT id, name, created_ns FROM repos WHERE id > ?1 ORDER BY id ASC LIMIT ?2",
    )?;
    let rows = stmt.query_map(params![after_id, limit], |row| {
        Ok(RepoRow {
            id: row.get(0)?,
            name: row.get(1)?,
            created_ns: row.get(2)?,
        })
    })?;
    rows.collect()
}

/// The id of repo `name`, or `None`.
pub fn repo_id_by_name(conn: &Connection, name: &str) -> rusqlite::Result<Option<i64>> {
    conn.query_row("SELECT id FROM repos WHERE name = ?1", params![name], |r| {
        r.get(0)
    })
    .optional()
}

/// Repo-scoped counters for `GET /v1/repos/{r}/stats`.
/// Returns `(changesets, objects, bytes, cbytes)`.
pub fn repo_stats(conn: &Connection, repo_id: i64) -> rusqlite::Result<(i64, i64, i64, i64)> {
    let changesets: i64 = conn.query_row(
        "SELECT COUNT(*) FROM changesets WHERE repo_id = ?1",
        params![repo_id],
        |r| r.get(0),
    )?;
    let (objects, bytes, cbytes): (i64, i64, i64) = conn.query_row(
        "SELECT COUNT(*), COALESCE(SUM(o.size), 0), COALESCE(SUM(o.csize), 0) \
         FROM repo_blobs rb JOIN objects o ON o.object_id = rb.object_id \
         WHERE rb.repo_id = ?1",
        params![repo_id],
        |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
    )?;
    Ok((changesets, objects, bytes, cbytes))
}

// ---- refs ----------------------------------------------------------------

/// One ref lookup.
pub fn ref_get(conn: &Connection, repo_id: i64, name: &str) -> rusqlite::Result<Option<RefRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT name, target, version FROM refs WHERE repo_id = ?1 AND name = ?2",
    )?;
    stmt.query_row(params![repo_id, name], |row| {
        Ok(RefRow {
            name: row.get(0)?,
            target: row.get(1)?,
            version: row.get(2)?,
        })
    })
    .optional()
}

/// The parameters of one ref move.
pub struct RefMove<'a> {
    pub repo_id: i64,
    pub repo_name: &'a str,
    pub name: &'a str,
    pub target: &'a Id,
    /// `None` creates the ref (conflict if it exists); `Some(v)` moves only
    /// when the current version is `v`.
    pub expected: Option<i64>,
    pub principal: &'a str,
    pub now_ns: i64,
}

/// Compare-and-swap ref move, atomic with its `ref_move` audit row
/// (successes only; conflicts are answered, not audited).
pub fn ref_cas(conn: &Connection, mv: RefMove<'_>) -> rusqlite::Result<RefCas> {
    ref_cas_inner(conn, mv, None)
}

/// [`ref_cas`] with provenance: the audit row carries `via` so an
/// import-driven ref move stays distinguishable from a land or a manual
/// move (the audit trail's whole point).
pub fn ref_cas_via(conn: &Connection, mv: RefMove<'_>, via: &str) -> rusqlite::Result<RefCas> {
    ref_cas_inner(conn, mv, Some(via))
}

fn ref_cas_inner(
    conn: &Connection,
    mv: RefMove<'_>,
    via: Option<&str>,
) -> rusqlite::Result<RefCas> {
    let RefMove {
        repo_id,
        repo_name,
        name,
        target,
        expected,
        principal,
        now_ns,
    } = mv;
    let tx = conn.unchecked_transaction()?;
    let exists: Option<i64> = tx
        .query_row(
            "SELECT id FROM repos WHERE id = ?1",
            params![repo_id],
            |r| r.get(0),
        )
        .optional()?;
    if exists.is_none() {
        return Ok(RefCas::RepoMissing);
    }

    let current: Option<RefRow> = ref_get(&tx, repo_id, name)?;
    let outcome = match (expected, current) {
        (None, None) => {
            tx.execute(
                "INSERT INTO refs(repo_id, name, target, version, last_principal) \
                 VALUES (?1, ?2, ?3, 1, ?4)",
                params![repo_id, name, target.0.as_slice(), principal],
            )?;
            RefCas::Created
        }
        (None, Some(current)) => RefCas::Conflict {
            current_version: current.version,
            current_target: current.target,
        },
        (Some(_expected), None) => {
            // CAS against a ref that does not exist: no version can match.
            RefCas::RefMissing
        }
        (Some(expected), Some(current)) => {
            if current.version != expected {
                RefCas::Conflict {
                    current_version: current.version,
                    current_target: current.target,
                }
            } else {
                let new_version = current.version + 1;
                tx.execute(
                    "UPDATE refs SET target = ?1, version = ?2, last_principal = ?3 \
                     WHERE repo_id = ?4 AND name = ?5",
                    params![target.0.as_slice(), new_version, principal, repo_id, name],
                )?;
                RefCas::Updated {
                    version: new_version,
                }
            }
        }
    };

    if matches!(outcome, RefCas::Created | RefCas::Updated { .. }) {
        // Same convention as changeset_create: the `via` key is present
        // only when the producer named itself — a bare API move leaves no
        // `via: null` behind.
        let mut details = serde_json::json!({
            "repo": repo_name,
            "ref": name,
            "target": target.to_hex(),
        });
        if let Some(via) = via {
            details["via"] = serde_json::json!(via);
        }
        audit_append(&tx, now_ns, principal, "ref_move", &details.to_string())?;
    }
    tx.commit()?;
    Ok(outcome)
}

// ---- objects -------------------------------------------------------------

/// Records (or refreshes) the `objects` row for a CAS blob that was already
/// fsynced. Returns `true` when the row already existed (idempotent re-PUT).
pub fn object_row_upsert(
    conn: &Connection,
    object_id: &Id,
    size: i64,
    csize: i64,
) -> rusqlite::Result<bool> {
    let existed: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM objects WHERE object_id = ?1",
            params![object_id.0.as_slice()],
            |r| r.get(0),
        )
        .optional()?;
    conn.execute(
        "INSERT INTO objects(object_id, size, csize) VALUES (?1, ?2, ?3) \
         ON CONFLICT(object_id) DO UPDATE SET size = excluded.size, csize = excluded.csize",
        params![object_id.0.as_slice(), size, csize],
    )?;
    Ok(existed.is_some())
}

/// Membership check: does repo `repo_id` reference `object_id` via
/// `repo_blobs`? Every object read is gated on this.
pub fn repo_blob_check(conn: &Connection, repo_id: i64, object_id: &Id) -> rusqlite::Result<bool> {
    let hit: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM repo_blobs WHERE repo_id = ?1 AND object_id = ?2",
            params![repo_id, object_id.0.as_slice()],
            |r| r.get(0),
        )
        .optional()?;
    Ok(hit.is_some())
}

// ---- changesets & trees (M2a) ---------------------------------------------

/// One row of `changesets`.
pub struct ChangesetRow {
    pub canonical: Vec<u8>,
    pub author: String,
    pub message: String,
    pub timestamp_ns: i64,
    pub generation: i64,
    pub principal: String,
    pub root_tree_id: Option<Vec<u8>>,
}

/// One row of `file_changes`, with `op` as the canonical op tag (Add=0,
/// Mod=1, Del=2 — the same numbering as the byte format).
pub struct FileChangeRow {
    pub path: String,
    pub op: i64,
    pub blob_id: Option<Vec<u8>>,
    pub size: Option<i64>,
    pub exec: bool,
}

/// One row of `tree_entries`; `kind` 0 = file, 1 = dir.
pub struct TreeEntryRow {
    pub name: String,
    pub kind: i64,
    pub object_id: Vec<u8>,
    pub size: i64,
    pub exec: bool,
}

/// A changeset's graph position: generation, parent ids in `ord` order,
/// and its derived root-tree pointer.
pub struct CsGraph {
    pub generation: i64,
    pub parents: Vec<Id>,
    pub root_tree_id: Option<Vec<u8>>,
}

/// Minimal projection for `/log` entries.
pub struct LogRow {
    pub id: Vec<u8>,
    pub author: String,
    pub message: String,
    pub timestamp_ns: i64,
    pub generation: i64,
    pub principal: String,
}

/// One derived tree to persist: canonical bytes plus flat entries.
pub struct TreeWrite {
    pub id: Id,
    pub canonical: Vec<u8>,
    pub entries: Vec<TreeEntryRow>,
}

/// Canonical op tag for a [`naatre_core::changeset::Op`] (Add=0, Mod=1,
/// Del=2, matching the byte format).
pub fn op_tag(op: naatre_core::changeset::Op) -> i64 {
    match op {
        naatre_core::changeset::Op::Add => 0,
        naatre_core::changeset::Op::Mod => 1,
        naatre_core::changeset::Op::Del => 2,
    }
}

/// One changeset row (metadata only), or `None` when the id is not a
/// changeset of this repo.
pub fn changeset_get(
    conn: &Connection,
    repo_id: i64,
    id: &Id,
) -> rusqlite::Result<Option<ChangesetRow>> {
    conn.query_row(
        "SELECT canonical, author, message, timestamp_ns, generation, principal, root_tree_id \
         FROM changesets WHERE repo_id = ?1 AND id = ?2",
        params![repo_id, id.0.as_slice()],
        |row| {
            Ok(ChangesetRow {
                canonical: row.get(0)?,
                author: row.get(1)?,
                message: row.get(2)?,
                timestamp_ns: row.get(3)?,
                generation: row.get(4)?,
                principal: row.get(5)?,
                root_tree_id: row.get(6)?,
            })
        },
    )
    .optional()
}

/// A changeset's generation, parents (in `ord` order), and root tree, or
/// `None`.
pub fn changeset_graph(
    conn: &Connection,
    repo_id: i64,
    id: &Id,
) -> rusqlite::Result<Option<CsGraph>> {
    let row: Option<(i64, Option<Vec<u8>>)> = conn
        .query_row(
            "SELECT generation, root_tree_id FROM changesets WHERE repo_id = ?1 AND id = ?2",
            params![repo_id, id.0.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .optional()?;
    let Some((generation, root_tree_id)) = row else {
        return Ok(None);
    };
    let mut stmt = conn.prepare(
        "SELECT parent FROM changeset_parents WHERE repo_id = ?1 AND cs = ?2 ORDER BY ord ASC",
    )?;
    let parents = stmt
        .query_map(params![repo_id, id.0.as_slice()], |r| {
            r.get::<_, Vec<u8>>(0)
                .map(|b| Id(b.try_into().expect("parent id is 32 bytes")))
        })?
        .collect::<rusqlite::Result<Vec<Id>>>()?;
    Ok(Some(CsGraph {
        generation,
        parents,
        root_tree_id,
    }))
}

/// All file changes of one changeset, ascending by path (the PK order).
pub fn file_changes_list(
    conn: &Connection,
    repo_id: i64,
    cs: &Id,
) -> rusqlite::Result<Vec<FileChangeRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT path, op, blob_id, size, exec FROM file_changes \
         WHERE repo_id = ?1 AND cs = ?2 ORDER BY path ASC",
    )?;
    let rows = stmt.query_map(params![repo_id, cs.0.as_slice()], |row| {
        Ok(FileChangeRow {
            path: row.get(0)?,
            op: row.get(1)?,
            blob_id: row.get(2)?,
            size: row.get(3)?,
            exec: row.get::<_, i64>(4)? != 0,
        })
    })?;
    rows.collect()
}

/// The ids among `ids` that have no `objects` row (blob-presence check).
pub fn objects_missing(conn: &Connection, ids: &[Id]) -> rusqlite::Result<Vec<Id>> {
    let mut missing = Vec::new();
    for id in ids {
        let hit: Option<i64> = conn
            .query_row(
                "SELECT 1 FROM objects WHERE object_id = ?1",
                params![id.0.as_slice()],
                |r| r.get(0),
            )
            .optional()?;
        if hit.is_none() {
            missing.push(*id);
        }
    }
    Ok(missing)
}

/// All entries of one tree node, ascending by name.
pub fn tree_entries_load(conn: &Connection, tree_id: &Id) -> rusqlite::Result<Vec<TreeEntryRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT name, kind, object_id, size, exec FROM tree_entries \
         WHERE tree_id = ?1 ORDER BY name ASC",
    )?;
    let rows = stmt.query_map(params![tree_id.0.as_slice()], |row| {
        Ok(TreeEntryRow {
            name: row.get(0)?,
            kind: row.get(1)?,
            object_id: row.get(2)?,
            size: row.get(3)?,
            exec: row.get::<_, i64>(4)? != 0,
        })
    })?;
    rows.collect()
}

/// The repo a `trees` row is attributed to (the first writer, since rows
/// are `INSERT OR IGNORE`), or `None` (unknown tree id). Informational
/// only — reads are gated on `repo_trees` membership (`repo_has_tree`),
/// never on this column.
pub fn tree_repo(conn: &Connection, tree_id: &Id) -> rusqlite::Result<Option<i64>> {
    conn.query_row(
        "SELECT repo_id FROM trees WHERE id = ?1",
        params![tree_id.0.as_slice()],
        |r| r.get(0),
    )
    .optional()
}

/// Persists HISTORY-derived trees (changeset create, land, import): one
/// `trees` row (canonical bytes), its flat `tree_entries` rows, and this
/// repo's `repo_trees` membership row each. `INSERT OR IGNORE` keyed by id
/// makes repeats idempotent (shared subtrees re-derived by a child
/// changeset are no-ops; identical id implies identical bytes). The
/// membership row is what later gates bare-id tree reads. Snapshot trees
/// must NOT go through here — use [`trees_write_snapshot`].
pub fn trees_write(conn: &Connection, repo_id: i64, trees: &[TreeWrite]) -> rusqlite::Result<()> {
    for tree in trees {
        write_tree_rows(conn, repo_id, tree)?;
        conn.execute(
            "INSERT OR IGNORE INTO repo_trees(repo_id, tree_id) VALUES (?1, ?2)",
            params![repo_id, tree.id.0.as_slice()],
        )?;
    }
    Ok(())
}

/// Persists SNAPSHOT trees (the snapshot POST path): `trees` +
/// `tree_entries` rows with NO `repo_trees` membership. Snapshot trees are
/// own-principal data served only through the owner-gated snapshot routes;
/// a membership row would expose their path names and blob ids to every
/// repo reader via the bare-id tree route, and those rows carry no TTL. GC
/// purges snapshot-only tree rows once no live snapshot reaches them
/// ([`purge_snapshot_only_trees`]).
pub fn trees_write_snapshot(conn: &Connection, trees: &[TreeWrite]) -> rusqlite::Result<()> {
    for tree in trees {
        write_tree_rows(conn, 0, tree)?;
    }
    Ok(())
}

/// One `trees` row plus its flat `tree_entries` rows (`INSERT OR IGNORE` /
/// `INSERT OR REPLACE`, idempotent repeats). `repo_id` is informational.
fn write_tree_rows(conn: &Connection, repo_id: i64, tree: &TreeWrite) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT OR IGNORE INTO trees(id, repo_id, canonical) VALUES (?1, ?2, ?3)",
        params![tree.id.0.as_slice(), repo_id, tree.canonical],
    )?;
    for e in &tree.entries {
        conn.execute(
            "INSERT OR REPLACE INTO tree_entries(tree_id, name, kind, object_id, size, exec) \
             VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            params![
                tree.id.0.as_slice(),
                e.name,
                e.kind,
                e.object_id,
                e.size,
                e.exec as i64,
            ],
        )?;
    }
    Ok(())
}

/// Whether any `trees` row lacks a `repo_trees` junction row. Read-only
/// idle probe for the GC pass: with no junction-less rows there is nothing
/// snapshot-created to sweep, so the pass can skip the write slot entirely
/// (a quiet server's periodic pass must never occupy the single-writer
/// slot — that would 503 concurrent writes).
pub fn any_junctionless_trees(conn: &Connection) -> rusqlite::Result<bool> {
    conn.query_row(
        "SELECT 1 FROM trees t \
         WHERE NOT EXISTS (SELECT 1 FROM repo_trees rt WHERE rt.tree_id = t.id) LIMIT 1",
        [],
        |_| Ok(()),
    )
    .optional()
    .map(|hit| hit.is_some())
}

/// The `trees` rows to sweep: the ZERO-JUNCTION candidates (no
/// `repo_trees` row — indexed probe) that no retention path protects.
/// Protection is TRANSITIVE through candidates: a candidate is protected
/// if it (a) sits in an unexpired snapshot's closure, (b) is a changeset
/// root, (c) has a RETAINED parent — a tree with a junction row or a
/// changeset root pointing at it as a child (reverse lookup, indexed) —
/// or is a candidate child of an already-protected candidate. (b)/(c) and
/// the recursion are the safety net for junction completeness: a future
/// write path that forgets junction rows for a whole SUBTREE cannot get
/// its live history silently swept — a junction-less chain under a
/// retained parent survives in full (depth-one protection would orphan
/// the chain's children; see `m5a_test.rs::gc_spares_a_deeper_
/// junctionless_chain`).
///
/// Zero-junction candidacy is sound because every history-tree write path
/// inserts junction rows and migration 3's backfill covered pre-migration
/// rows — which is why no provenance marker column is needed (and why
/// half-adding one later would be wrong: the invariant lives in junction
/// completeness). The walk stays inside the candidate set, but it grows
/// with TOTAL tree rows, not only with snapshots: one indexed junction
/// probe (`idx_repo_trees_tree`) per tree row finds the candidates, the
/// `live` CTE recursively walks the full closures of unexpired snapshots,
/// and each candidate costs indexed lookups (`idx_changesets_root`,
/// `idx_tree_entries_object`) — matching `docs/design.md`'s "grows with
/// total tree rows". The caller runs this inside the same transaction as
/// the expired-row purge, so a snapshot POST reusing a shared subtree
/// cannot land between mark and delete (both go through the
/// single-writer slot).
pub fn unprotected_trees(conn: &Connection, now_ns: i64) -> rusqlite::Result<Vec<Id>> {
    let mut stmt = conn.prepare_cached(
        "WITH RECURSIVE
         live(tree_id) AS (
             SELECT tree_id FROM snapshots WHERE expires_ns > ?1
             UNION
             SELECT te.object_id FROM live l
             JOIN tree_entries te ON te.tree_id = l.tree_id AND te.kind = 1
         ),
         candidates AS (
             SELECT t.id FROM trees t
             WHERE NOT EXISTS (SELECT 1 FROM repo_trees rt WHERE rt.tree_id = t.id)
         ),
         protected(tree_id) AS (
             SELECT c.id FROM candidates c
             WHERE c.id IN (SELECT tree_id FROM live)
                OR EXISTS (SELECT 1 FROM changesets cs WHERE cs.root_tree_id = c.id)
                OR EXISTS (SELECT 1 FROM tree_entries te
                           WHERE te.kind = 1 AND te.object_id = c.id
                           AND (EXISTS (SELECT 1 FROM repo_trees rt
                                        WHERE rt.tree_id = te.tree_id)
                                OR EXISTS (SELECT 1 FROM changesets cs
                                           WHERE cs.root_tree_id = te.tree_id)))
             UNION
             SELECT te.object_id
             FROM protected p
             JOIN tree_entries te ON te.tree_id = p.tree_id AND te.kind = 1
             WHERE EXISTS (SELECT 1 FROM candidates c WHERE c.id = te.object_id)
         )
         SELECT c.id FROM candidates c
         WHERE c.id NOT IN (SELECT tree_id FROM protected)",
    )?;
    let rows = stmt.query_map(params![now_ns], |row| {
        row.get::<_, Vec<u8>>(0)
            .map(|b| Id(b.try_into().expect("tree id is 32 bytes")))
    })?;
    rows.collect()
}

/// Deletes the given `trees` rows and their `tree_entries` rows; returns
/// the number of `trees` rows deleted. The write-side half of the sweep.
pub fn delete_tree_rows(conn: &Connection, ids: &[Id]) -> rusqlite::Result<usize> {
    let mut n = 0;
    for id in ids {
        conn.execute(
            "DELETE FROM tree_entries WHERE tree_id = ?1",
            params![id.0.as_slice()],
        )?;
        n += conn.execute("DELETE FROM trees WHERE id = ?1", params![id.0.as_slice()])?;
    }
    Ok(n)
}

/// Whether `repo_id` wrote `tree_id` (a `repo_trees` membership row) — the
/// read gate for the bare-id tree route. A tree is readable from every repo
/// that wrote it and from no other: content-addressing shares the row, the
/// junction shares readability only with writers.
pub fn repo_has_tree(conn: &Connection, repo_id: i64, tree_id: &Id) -> rusqlite::Result<bool> {
    let hit: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM repo_trees WHERE repo_id = ?1 AND tree_id = ?2",
            params![repo_id, tree_id.0.as_slice()],
            |r| r.get(0),
        )
        .optional()?;
    Ok(hit.is_some())
}

/// Whether `target` sits in the subtree closure of `root` (dir entries,
/// `kind = 1`): the snapshot tree route's membership check. `root` itself
/// counts (a closure contains its root). Content-addressed trees cannot
/// cycle (an entry's id covers the bytes that name it), and the seen-set
/// keeps shared subtrees from being walked twice anyway.
pub fn tree_in_closure(conn: &Connection, root: &Id, target: &Id) -> rusqlite::Result<bool> {
    if root == target {
        return Ok(true);
    }
    let mut seen = HashSet::new();
    let mut stack = vec![*root];
    while let Some(tree_id) = stack.pop() {
        if !seen.insert(tree_id) {
            continue;
        }
        for e in tree_entries_load(conn, &tree_id)? {
            if e.kind != 1 {
                continue;
            }
            let child = Id(e.object_id.try_into().expect("tree id is 32 bytes"));
            if child == *target {
                return Ok(true);
            }
            stack.push(child);
        }
    }
    Ok(false)
}

/// Everything one successful changeset create commits, atomically: the
/// changeset row, its parents, file changes, repo blob membership, derived
/// trees, and the `changeset_create` audit row. Any failure rolls the whole
/// step back — a rejected create leaves zero rows.
pub struct ChangesetInsert<'a> {
    pub repo_id: i64,
    pub repo_name: &'a str,
    pub id: &'a Id,
    pub canonical: &'a [u8],
    pub author: &'a str,
    pub message: &'a str,
    pub timestamp_ns: i64,
    pub generation: i64,
    pub principal: &'a str,
    pub root_tree_id: &'a Id,
    pub parents: &'a [Id],
    pub changes: &'a [naatre_core::changeset::FileChange],
    /// Add/Mod blob ids for `repo_blobs` membership (deduplicated).
    pub blobs: &'a [Id],
    pub trees: &'a [TreeWrite],
    pub now_ns: i64,
}

pub fn changeset_insert(conn: &Connection, rec: ChangesetInsert<'_>) -> rusqlite::Result<()> {
    let tx = conn.unchecked_transaction()?;
    changeset_insert_rows(&tx, rec, ChangesetExtras::NONE)?;
    tx.commit()?;
    Ok(())
}

/// Optional extras for [`changeset_insert_rows`]: the rows and audit
/// wording only the importer writes. Plain creates and lands pass
/// [`ChangesetExtras::NONE`].
#[derive(Clone, Copy)]
pub struct ChangesetExtras<'a> {
    /// Added to the `changeset_create` audit details as `"via"` (import:
    /// `"import"`).
    pub audit_via: Option<&'a str>,
    /// When present, a `git_map(repo_id, git_oid, cs_id)` row (cs_id =
    /// `rec.id`) is written in the same transaction — the importer's oid
    /// map, atomic with the changeset it maps to.
    pub git_map_oid: Option<&'a [u8]>,
}

impl ChangesetExtras<'_> {
    /// No extras: the plain create/land shape.
    pub const NONE: Self = Self {
        audit_via: None,
        git_map_oid: None,
    };
}

/// The row writes of [`changeset_insert`] without a transaction of their
/// own: land runs inside one enclosing transaction and calls this directly
/// (nesting a second transaction would fail), and the importer calls it
/// with [`ChangesetExtras`].
pub fn changeset_insert_rows(
    conn: &Connection,
    rec: ChangesetInsert<'_>,
    extras: ChangesetExtras<'_>,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns, \
         generation, principal, root_tree_id) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)",
        params![
            rec.repo_id,
            rec.id.0.as_slice(),
            rec.canonical,
            rec.author,
            rec.message,
            rec.timestamp_ns,
            rec.generation,
            rec.principal,
            rec.root_tree_id.0.as_slice(),
        ],
    )?;
    for (ord, parent) in rec.parents.iter().enumerate() {
        conn.execute(
            "INSERT INTO changeset_parents(repo_id, cs, parent, ord) VALUES (?1, ?2, ?3, ?4)",
            params![
                rec.repo_id,
                rec.id.0.as_slice(),
                parent.0.as_slice(),
                ord as i64
            ],
        )?;
    }
    for c in rec.changes {
        let (blob, size): (Option<&[u8]>, Option<i64>) = match c.op {
            naatre_core::changeset::Op::Del => (None, None),
            // Request validation already guarantees Add/Mod carry both.
            _ => match (&c.blob, c.size) {
                (Some(b), Some(s)) => (Some(b.0.as_slice()), Some(s as i64)),
                _ => (None, None),
            },
        };
        let exec = i64::from(
            matches!(
                c.op,
                naatre_core::changeset::Op::Add | naatre_core::changeset::Op::Mod
            ) && c.exec == Some(true),
        );
        conn.execute(
            "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            params![
                rec.repo_id,
                rec.id.0.as_slice(),
                c.path,
                op_tag(c.op),
                blob,
                size,
                exec,
            ],
        )?;
    }
    for blob in rec.blobs {
        conn.execute(
            "INSERT OR IGNORE INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)",
            params![rec.repo_id, blob.0.as_slice()],
        )?;
    }
    if let Some(git_oid) = extras.git_map_oid {
        conn.execute(
            "INSERT INTO git_map(repo_id, git_oid, cs_id) VALUES (?1, ?2, ?3)",
            params![rec.repo_id, git_oid, rec.id.0.as_slice()],
        )?;
    }
    trees_write(conn, rec.repo_id, rec.trees)?;
    let mut details = serde_json::json!({
        "repo": rec.repo_name,
        "changeset": rec.id.to_hex(),
        "generation": rec.generation,
    });
    if let Some(via) = extras.audit_via {
        details["via"] = serde_json::json!(via);
    }
    audit_append(
        conn,
        rec.now_ns,
        rec.principal,
        "changeset_create",
        &details.to_string(),
    )?;
    Ok(())
}

/// Walks the first-parent chain from `from`, newest first, up to `limit`
/// rows. The chain itself is the deterministic order.
pub fn log_chain(
    conn: &Connection,
    repo_id: i64,
    from: &Id,
    limit: i64,
) -> rusqlite::Result<Vec<LogRow>> {
    let mut rows = Vec::new();
    let mut current = *from;
    loop {
        let Some(graph) = changeset_graph(conn, repo_id, &current)? else {
            break;
        };
        let row = conn
            .query_row(
                "SELECT author, message, timestamp_ns, generation, principal FROM changesets \
                 WHERE repo_id = ?1 AND id = ?2",
                params![repo_id, current.0.as_slice()],
                |r| {
                    Ok(LogRow {
                        id: current.0.to_vec(),
                        author: r.get(0)?,
                        message: r.get(1)?,
                        timestamp_ns: r.get(2)?,
                        generation: r.get(3)?,
                        principal: r.get(4)?,
                    })
                },
            )
            .optional()?;
        let Some(row) = row else {
            break;
        };
        rows.push(row);
        if rows.len() as i64 >= limit {
            break;
        }
        match graph.parents.first() {
            Some(next) => current = *next,
            None => break,
        }
    }
    Ok(rows)
}

// ---- land (M4a) ------------------------------------------------------------

/// One row of `lands`.
pub struct LandRow {
    pub state: String,
    pub head: Option<Vec<u8>>,
    pub base: Option<Vec<u8>>,
    pub result_json: Option<String>,
}

/// The land row for `(repo, principal, key)`, or `None`.
pub fn land_get(
    conn: &Connection,
    repo_id: i64,
    principal: &str,
    key: &str,
) -> rusqlite::Result<Option<LandRow>> {
    conn.query_row(
        "SELECT state, head, base, result_json FROM lands \
         WHERE repo_id = ?1 AND principal = ?2 AND key = ?3",
        params![repo_id, principal, key],
        |row| {
            Ok(LandRow {
                state: row.get(0)?,
                head: row.get(1)?,
                base: row.get(2)?,
                result_json: row.get(3)?,
            })
        },
    )
    .optional()
}

/// The in-flight marker (state='pending'). It commits only together with
/// the finished land (one transaction), so every failure path — including
/// the crash points before COMMIT — leaves the key unsettled.
pub fn land_insert_pending(
    conn: &Connection,
    repo_id: i64,
    principal: &str,
    key: &str,
    head: &Id,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO lands(repo_id, principal, key, state, head) VALUES (?1, ?2, ?3, 'pending', ?4)",
        params![repo_id, principal, key, head.0.as_slice()],
    )?;
    Ok(())
}

/// Settles the land row to state='done': `head` is the requested head (the
/// idempotency payload), `base` the main target the land rebased onto, and
/// `result_json` the recorded `{landed, rebased, note}`. Upsert form, so
/// the pending row inside the same transaction is overwritten.
#[allow(clippy::too_many_arguments)]
pub fn land_settle(
    conn: &Connection,
    repo_id: i64,
    principal: &str,
    key: &str,
    head: &Id,
    base: &Id,
    result_json: &str,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO lands(repo_id, principal, key, state, head, base, result_json) \
         VALUES (?1, ?2, ?3, 'done', ?4, ?5, ?6) \
         ON CONFLICT(repo_id, principal, key) DO UPDATE SET \
           state = 'done', head = excluded.head, base = excluded.base, \
           result_json = excluded.result_json",
        params![
            repo_id,
            principal,
            key,
            head.0.as_slice(),
            base.0.as_slice(),
            result_json
        ],
    )?;
    Ok(())
}

/// The land-side CAS of `main`: moves it only from `expected_target` (the
/// target the rebase was computed against) with a version bump. Returns the
/// updated-row count — 0 means the target moved underneath (unreachable
/// under the single-writer slot; the caller reports a ref conflict).
pub fn main_cas_move(
    conn: &Connection,
    repo_id: i64,
    expected_target: &Id,
    new_target: &Id,
    principal: &str,
) -> rusqlite::Result<usize> {
    conn.execute(
        "UPDATE refs SET target = ?3, version = version + 1, last_principal = ?4 \
         WHERE repo_id = ?1 AND name = 'main' AND target = ?2",
        params![
            repo_id,
            expected_target.0.as_slice(),
            new_target.0.as_slice(),
            principal
        ],
    )
}

// ---- snapshots (M5a) -------------------------------------------------------

/// One row of `snapshots`.
pub struct SnapshotRow {
    pub tree_id: Vec<u8>,
    pub principal: String,
    pub created_ns: i64,
    pub expires_ns: i64,
}

/// The snapshot row for `(repo_id, id)`, or `None` (unknown id, or another
/// repo's snapshot — ids are only meaningful within their repo).
pub fn snapshot_get(
    conn: &Connection,
    repo_id: i64,
    id: &Id,
) -> rusqlite::Result<Option<SnapshotRow>> {
    conn.query_row(
        "SELECT tree_id, principal, created_ns, expires_ns FROM snapshots \
         WHERE repo_id = ?1 AND id = ?2",
        params![repo_id, id.0.as_slice()],
        |row| {
            Ok(SnapshotRow {
                tree_id: row.get(0)?,
                principal: row.get(1)?,
                created_ns: row.get(2)?,
                expires_ns: row.get(3)?,
            })
        },
    )
    .optional()
}

/// Inserts one snapshot row. Duplicate ids are rejected by the PRIMARY KEY;
/// the route checks for an existing row first (idempotent re-POST).
pub fn snapshot_insert(
    conn: &Connection,
    repo_id: i64,
    id: &Id,
    tree_id: &Id,
    principal: &str,
    created_ns: i64,
    expires_ns: i64,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO snapshots(id, repo_id, tree_id, principal, created_ns, expires_ns) \
         VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
        params![
            id.0.as_slice(),
            repo_id,
            tree_id.0.as_slice(),
            principal,
            created_ns,
            expires_ns
        ],
    )?;
    Ok(())
}

/// Ids of expired snapshot rows (`expires_ns <= now_ns`), ascending.
pub fn snapshots_expired(conn: &Connection, now_ns: i64) -> rusqlite::Result<Vec<Id>> {
    let mut stmt =
        conn.prepare_cached("SELECT id FROM snapshots WHERE expires_ns <= ?1 ORDER BY id ASC")?;
    let rows = stmt.query_map(params![now_ns], |row| {
        row.get::<_, Vec<u8>>(0)
            .map(|b| Id(b.try_into().expect("snapshot id is 32 bytes")))
    })?;
    rows.collect()
}

/// Deletes the given snapshot rows; returns how many existed. GC purges a
/// row only after its ephemeral dir is gone, so a crash leaves an orphan
/// row (never a dangling dir-less row that serves) that the next run purges.
pub fn snapshots_purge(conn: &Connection, ids: &[Id]) -> rusqlite::Result<usize> {
    let mut purged = 0;
    for id in ids {
        purged += conn.execute(
            "DELETE FROM snapshots WHERE id = ?1",
            params![id.0.as_slice()],
        )?;
    }
    Ok(purged)
}

/// Which of `ids` have a snapshots row (the rowless-dir sweep's cross-check:
/// a dir with a row is a live or expired snapshot, not garbage).
pub fn snapshots_exist(conn: &Connection, ids: &[Id]) -> rusqlite::Result<Vec<bool>> {
    ids.iter()
        .map(|id| {
            let hit: Option<i64> = conn
                .query_row(
                    "SELECT 1 FROM snapshots WHERE id = ?1",
                    params![id.0.as_slice()],
                    |r| r.get(0),
                )
                .optional()?;
            Ok(hit.is_some())
        })
        .collect()
}

// ---- principals & tokens -------------------------------------------------

/// Creates or re-roles a principal.
pub fn principal_upsert(
    conn: &Connection,
    name: &str,
    role: &str,
    created_ns: i64,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO principals(name, role, created_ns) VALUES (?1, ?2, ?3) \
         ON CONFLICT(name) DO UPDATE SET role = excluded.role",
        params![name, role, created_ns],
    )?;
    Ok(())
}

/// Inserts a token row (hash only — token material never persists).
pub fn token_insert(
    conn: &Connection,
    principal: &str,
    blake3: &[u8; 32],
    expires_ns: Option<i64>,
    scope_path: Option<&str>,
) -> rusqlite::Result<i64> {
    conn.execute(
        "INSERT INTO tokens(principal, blake3, expires_ns, scope_path) VALUES (?1, ?2, ?3, ?4)",
        params![principal, blake3, expires_ns, scope_path],
    )?;
    Ok(conn.last_insert_rowid())
}

/// Mints a token row: principal upsert, token insert, and the `token_create`
/// audit row land in one transaction. Returns the new token row id.
pub fn token_create(
    conn: &Connection,
    principal: &str,
    role: &str,
    blake3: &[u8; 32],
    expires_ns: Option<i64>,
    scope_path: Option<&str>,
    now_ns: i64,
) -> rusqlite::Result<i64> {
    let tx = conn.unchecked_transaction()?;
    principal_upsert(&tx, principal, role, now_ns)?;
    let token_id = token_insert(&tx, principal, blake3, expires_ns, scope_path)?;
    audit_append(
        &tx,
        now_ns,
        principal,
        "token_create",
        &serde_json::json!({
            "token_id": token_id,
            "principal": principal,
            "role": role,
        })
        .to_string(),
    )?;
    tx.commit()?;
    Ok(token_id)
}

/// Lists tokens joined with their principal's role, ascending by id.
pub fn tokens_list(conn: &Connection) -> rusqlite::Result<Vec<TokenRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT t.id, t.principal, p.role, t.expires_ns, t.scope_path, t.revoked \
         FROM tokens t JOIN principals p ON p.name = t.principal ORDER BY t.id ASC",
    )?;
    let rows = stmt.query_map([], |row| {
        Ok(TokenRow {
            id: row.get(0)?,
            principal: row.get(1)?,
            role: row.get(2)?,
            expires_ns: row.get(3)?,
            scope_path: row.get(4)?,
            revoked: row.get::<_, i64>(5)? != 0,
        })
    })?;
    rows.collect()
}

/// Revokes token `id`. Returns `None` when no such token exists, and
/// `Some(first_revocation)` — `true` only on the 0→1 transition, so an
/// idempotent re-revoke answers 200 without writing a second audit row.
pub fn token_revoke(conn: &Connection, id: i64, now_ns: i64) -> rusqlite::Result<Option<bool>> {
    let tx = conn.unchecked_transaction()?;
    let exists: Option<String> = tx
        .query_row(
            "SELECT principal FROM tokens WHERE id = ?1",
            params![id],
            |r| r.get(0),
        )
        .optional()?;
    let Some(principal) = exists else {
        return Ok(None);
    };
    let changed = tx.execute(
        "UPDATE tokens SET revoked = 1 WHERE id = ?1 AND revoked = 0",
        params![id],
    )?;
    let first_revocation = changed == 1;
    if first_revocation {
        audit_append(
            &tx,
            now_ns,
            &principal,
            "token_revoke",
            &serde_json::json!({ "token_id": id }).to_string(),
        )?;
    }
    tx.commit()?;
    Ok(Some(first_revocation))
}

/// Token lookup by stored BLAKE3 hash, joined with the principal's role.
pub fn auth_lookup(conn: &Connection, blake3: &[u8; 32]) -> rusqlite::Result<Option<AuthRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT t.id, t.principal, p.role, t.expires_ns, t.revoked, t.scope_path \
         FROM tokens t JOIN principals p ON p.name = t.principal WHERE t.blake3 = ?1",
    )?;
    stmt.query_row(params![blake3.as_slice()], |row| {
        Ok(AuthRow {
            token_id: row.get(0)?,
            principal: row.get(1)?,
            role: row.get(2)?,
            expires_ns: row.get(3)?,
            revoked: row.get::<_, i64>(4)? != 0,
            scope_path: row.get(5)?,
        })
    })
    .optional()
}

// ---- derived data (M6a) ----------------------------------------------------
//
// The generic derived-data framework's SQL: the kind registry rows, the
// per-(kind, repo, changeset) queue/state machine, and the poison reports.
// Policy (leases, attempt caps, the parents-done pick semantics) lives in
// [`crate::derive`]; every statement is here. Multi-statement steps
// (`derive_fail_attempt`) are transaction-free on purpose: callers either
// run them standalone (autocommit per statement) or inside their own
// enclosing transaction (the lease sweep).

/// One row of `derive_state`.
pub struct DeriveStateRow {
    pub state: String,
    pub lease_expires_ns: Option<i64>,
    pub attempts: i64,
    pub output: Option<Vec<u8>>,
    /// Poison taint (migration 6): the node derived on an empty-marker
    /// parent somewhere up its ancestor set. `poison_cs` names the NEAREST
    /// poisoned ancestor. Reads raise their degraded flag off these two in
    /// O(1).
    pub tainted: bool,
    pub poison_cs: Option<Id>,
}

/// The id of registered kind `name`, or `None`.
pub fn derive_kind_id(conn: &Connection, name: &str) -> rusqlite::Result<Option<i64>> {
    conn.query_row(
        "SELECT id FROM derive_kinds WHERE name = ?1",
        params![name],
        |r| r.get(0),
    )
    .optional()
}

/// Seeds (or finds) the registry row for kind `name`; returns its id.
/// `INSERT OR IGNORE` keeps the id stable across processes: the first
/// registrant of a name owns its id forever.
pub fn derive_kind_seed(conn: &Connection, name: &str) -> rusqlite::Result<i64> {
    conn.execute(
        "INSERT OR IGNORE INTO derive_kinds(name) VALUES (?1)",
        params![name],
    )?;
    conn.query_row(
        "SELECT id FROM derive_kinds WHERE name = ?1",
        params![name],
        |r| r.get(0),
    )
}

/// Enqueues one changeset for kind `kind_id` (state='pending', attempts=0).
/// `OR IGNORE`: an existing row (done from an eager derive, or an earlier
/// enqueue) is never clobbered.
pub fn derive_enqueue_pending(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT OR IGNORE INTO derive_state(kind, repo_id, cs, state) \
         VALUES (?1, ?2, ?3, 'pending')",
        params![kind_id, repo_id, cs.0.as_slice()],
    )?;
    Ok(())
}

/// Records a COMPLETED eager derivation straight to state='done' (the eager
/// path runs inside the changeset's own write transaction — there is no
/// lease to take and nothing can interleave). Taint comes from the
/// parents the derivation consumed, exactly as in the queue's record step.
pub fn derive_state_insert_done(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    output: &[u8],
    tainted: bool,
    poison_cs: Option<&Id>,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO derive_state(kind, repo_id, cs, state, attempts, output, tainted, poison_cs) \
         VALUES (?1, ?2, ?3, 'done', 0, ?4, ?5, ?6)",
        params![
            kind_id,
            repo_id,
            cs.0.as_slice(),
            output,
            tainted as i64,
            poison_cs.map(|c| c.0.as_slice()),
        ],
    )?;
    Ok(())
}

/// One row of `derive_state`, or `None` (unknown kind/cs — the M6b read
/// routes' "is derived / is pending" lookup goes through here).
pub fn derive_get(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
) -> rusqlite::Result<Option<DeriveStateRow>> {
    conn.query_row(
        "SELECT state, lease_expires_ns, attempts, output, tainted, poison_cs \
         FROM derive_state \
         WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
        params![kind_id, repo_id, cs.0.as_slice()],
        |row| {
            Ok(DeriveStateRow {
                state: row.get(0)?,
                lease_expires_ns: row.get(1)?,
                attempts: row.get(2)?,
                output: row.get(3)?,
                tainted: row.get::<_, i64>(4)? != 0,
                poison_cs: row
                    .get::<_, Option<Vec<u8>>>(5)?
                    .map(|b| Id(b.try_into().expect("poison cs id is 32 bytes"))),
            })
        },
    )
    .optional()
}

/// The oldest pending node of `kind_id` whose parents are all out of
/// `pending`/`deriving` — done and poison parents never block (the plan's
/// never-head-of-line rule; a poison parent's children proceed on the
/// empty-marker fallback). Parents with NO row at all also never block:
/// within the framework every enqueued changeset gets a row per registered
/// kind, so a missing row means a kind registered after the fact (M6b's
/// backfill enqueues oldest-first, parents before children). Enqueue order
/// (`rowid`) is the "oldest" order.
pub fn derive_pick_next(conn: &Connection, kind_id: i64) -> rusqlite::Result<Option<(i64, Id)>> {
    let mut stmt = conn.prepare_cached(
        "SELECT s.repo_id, s.cs FROM derive_state s \
         WHERE s.kind = ?1 AND s.state = 'pending' \
         AND NOT EXISTS (\
             SELECT 1 FROM changeset_parents p \
             WHERE p.repo_id = s.repo_id AND p.cs = s.cs \
               AND EXISTS (SELECT 1 FROM derive_state d \
                           WHERE d.kind = s.kind AND d.repo_id = p.repo_id \
                             AND d.cs = p.parent \
                             AND d.state IN ('pending','deriving'))) \
         ORDER BY s.rowid ASC LIMIT 1",
    )?;
    let row = stmt
        .query_row(params![kind_id], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                row.get::<_, Vec<u8>>(1)
                    .map(|b| Id(b.try_into().expect("cs id is 32 bytes")))?,
            ))
        })
        .optional()?;
    Ok(row)
}

/// Takes the lease on a pending node: state → 'deriving' with
/// `lease_expires_ns` set. Returns `false` when the node was no longer
/// pending (the pick was stale).
pub fn derive_lease(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    lease_expires_ns: i64,
) -> rusqlite::Result<bool> {
    let n = conn.execute(
        "UPDATE derive_state SET state = 'deriving', lease_expires_ns = ?4 \
         WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3 AND state = 'pending'",
        params![kind_id, repo_id, cs.0.as_slice(), lease_expires_ns],
    )?;
    Ok(n == 1)
}

/// One parent's contribution to a child derivation, in the parents' `ord`
/// order: the output the child receives (the empty marker for a poisoned,
/// pending, or row-less parent), whether the parent row is ITSELF poison,
/// and the parent's inherited taint (see [`DeriveStateRow::tainted`]).
pub fn derive_parent_outputs(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    parents: &[Id],
) -> rusqlite::Result<Vec<crate::derive::ParentOut>> {
    parents
        .iter()
        .map(|p| {
            let row = conn
                .query_row(
                    "SELECT state, output, tainted, poison_cs FROM derive_state \
                     WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
                    params![kind_id, repo_id, p.0.as_slice()],
                    |row| {
                        Ok((
                            row.get::<_, String>(0)?,
                            row.get::<_, Option<Vec<u8>>>(1)?,
                            row.get::<_, i64>(2)? != 0,
                            row.get::<_, Option<Vec<u8>>>(3)?,
                        ))
                    },
                )
                .optional()?;
            Ok(match row {
                Some((state, output, tainted, poison_cs)) if state == "done" => {
                    crate::derive::ParentOut {
                        output: crate::derive::DerivedOut(output.unwrap_or_default()),
                        poisoned: false,
                        tainted,
                        poison_cs: poison_cs
                            .map(|b| Id(b.try_into().expect("poison cs id is 32 bytes"))),
                    }
                }
                // A poisoned parent hands down the empty marker and IS the
                // nearest poisoned ancestor for this child.
                Some((state, _, _, _)) if state == "poison" => crate::derive::ParentOut {
                    output: crate::derive::DerivedOut::empty(),
                    poisoned: true,
                    tainted: true,
                    poison_cs: Some(*p),
                },
                // Pending, deriving, or no row: the documented empty-marker
                // fallback with no taint (the framework re-runs the child
                // if the parent's real output lands later — see the lease
                // re-derivation contract).
                _ => crate::derive::ParentOut {
                    output: crate::derive::DerivedOut::empty(),
                    poisoned: false,
                    tainted: false,
                    poison_cs: None,
                },
            })
        })
        .collect()
}

/// Records a finished queue derivation: state → 'done' with the output
/// bytes, lease released. Guarded on state='deriving' — a node swept to
/// pending between derive and record is left to the next attempt (returns
/// `false`).
pub fn derive_record_done(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    output: &[u8],
    tainted: bool,
    poison_cs: Option<&Id>,
) -> rusqlite::Result<bool> {
    let n = conn.execute(
        "UPDATE derive_state SET state = 'done', output = ?4, lease_expires_ns = NULL, \
         tainted = ?5, poison_cs = ?6 \
         WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3 AND state = 'deriving'",
        params![
            kind_id,
            repo_id,
            cs.0.as_slice(),
            output,
            tainted as i64,
            poison_cs.map(|c| c.0.as_slice()),
        ],
    )?;
    Ok(n == 1)
}

/// Records one failed attempt (a deriver `Err`, or the lease sweep's
/// expiry): attempts+1; at the cap the node is QUARANTINED to 'poison' with
/// a `derive_reports` row, otherwise it goes back to 'pending'. Returns
/// `true` when the node was poisoned. Plain statements — see the module
/// note on transactions.
pub fn derive_fail_attempt(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    reason: &str,
    now_ns: i64,
) -> rusqlite::Result<bool> {
    let attempts: i64 = conn.query_row(
        "SELECT attempts FROM derive_state WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
        params![kind_id, repo_id, cs.0.as_slice()],
        |r| r.get(0),
    )?;
    let attempts = attempts + 1;
    if attempts >= crate::derive::DERIVE_MAX_ATTEMPTS {
        conn.execute(
            "UPDATE derive_state SET state = 'poison', lease_expires_ns = NULL, attempts = ?4 \
             WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
            params![kind_id, repo_id, cs.0.as_slice(), attempts],
        )?;
        conn.execute(
            "INSERT INTO derive_reports(kind, repo_id, cs, reason, ts_ns) VALUES (?1, ?2, ?3, ?4, ?5)",
            params![kind_id, repo_id, cs.0.as_slice(), reason, now_ns],
        )?;
        Ok(true)
    } else {
        conn.execute(
            "UPDATE derive_state SET state = 'pending', lease_expires_ns = NULL, attempts = ?4 \
             WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
            params![kind_id, repo_id, cs.0.as_slice(), attempts],
        )?;
        Ok(false)
    }
}

/// The kind's expired leases: (repo_id, cs) of 'deriving' rows whose lease
/// ran out — a crashed or hung derivation holder. The caller feeds each
/// back through [`derive_fail_attempt`] inside one transaction.
pub fn derive_expired_leases(
    conn: &Connection,
    kind_id: i64,
    now_ns: i64,
) -> rusqlite::Result<Vec<(i64, Id)>> {
    let mut stmt = conn.prepare_cached(
        "SELECT repo_id, cs FROM derive_state \
         WHERE kind = ?1 AND state = 'deriving' \
           AND lease_expires_ns IS NOT NULL AND lease_expires_ns < ?2",
    )?;
    let rows = stmt.query_map(params![kind_id, now_ns], |row| {
        Ok((
            row.get::<_, i64>(0)?,
            row.get::<_, Vec<u8>>(1)
                .map(|b| Id(b.try_into().expect("cs id is 32 bytes")))?,
        ))
    })?;
    rows.collect()
}

/// Backfills ONE BATCH of kind `kind_id`'s queue: enqueues up to `limit`
/// changesets that have no `derive_state` row for the kind (an M6b kind
/// registered after its changesets existed — bulk import through a
/// registry-less process, or an in-place upgrade of a pre-derive
/// database), oldest generation first. Insertion order follows
/// `generation ASC`, so the queue's `rowid`-ordered pick derives parents
/// before children with no extra sequencing — including across batch
/// boundaries (a parent's generation is always below its child's). The
/// `NOT EXISTS` predicate is the batch cursor: each call takes the first
/// not-yet-rowed changesets in that order, so the caller simply loops
/// until zero rows come back. `OR IGNORE`: a row that already exists
/// (eager-derived, previously enqueued) is never clobbered. Returns the
/// number of rows enqueued; 0 means the kind's backfill is complete. One
/// batch per call is deliberate: the caller (async) yields the
/// single-writer slot between batches — a whole-repo INSERT..SELECT would
/// hold the slot, and 503 every concurrent write, for the whole backfill.
pub fn derive_backfill_batch(
    conn: &Connection,
    kind_id: i64,
    limit: i64,
) -> rusqlite::Result<usize> {
    conn.execute(
        "INSERT OR IGNORE INTO derive_state(kind, repo_id, cs, state) \
         SELECT ?1, c.repo_id, c.id, 'pending' FROM changesets c \
         WHERE NOT EXISTS (\
             SELECT 1 FROM derive_state d \
             WHERE d.kind = ?1 AND d.repo_id = c.repo_id AND d.cs = c.id) \
         ORDER BY c.generation ASC, c.rowid ASC \
         LIMIT ?2",
        params![kind_id, limit],
    )
}

/// Immediate quarantine for the structural caps (walk budget, output size):
/// state='poison' plus a `derive_reports` row, NO attempt arithmetic — the
/// caps are structural, so retrying cannot help (unlike
/// [`derive_fail_attempt`]). UPSERT: the eager path calls this before any
/// row exists; an existing pending/deriving row is quarantined in place.
/// Plain statements — see the module note on transactions.
pub fn derive_poison_now(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    reason: &str,
    now_ns: i64,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO derive_state(kind, repo_id, cs, state, attempts) \
         VALUES (?1, ?2, ?3, 'poison', 0) \
         ON CONFLICT(kind, repo_id, cs) DO UPDATE SET \
           state = 'poison', lease_expires_ns = NULL",
        params![kind_id, repo_id, cs.0.as_slice()],
    )?;
    conn.execute(
        "INSERT INTO derive_reports(kind, repo_id, cs, reason, ts_ns) VALUES (?1, ?2, ?3, ?4, ?5)",
        params![kind_id, repo_id, cs.0.as_slice(), reason, now_ns],
    )?;
    Ok(())
}

// ---- derived kinds' served-surface storage (M6b) ---------------------------
//
// The blame and path_history kinds keep their read-surface rows here (the
// migration-5 tables); commit_graph keeps its state entirely in the
// framework handoff output. All statements are keyed upserts — the
// framework re-runs derivations after lost leases, so a repeat write must
// be a no-op, never a duplicate.

/// One path's entry of a changeset's blame DIFF-STATE: the path's blob id
/// in this changeset and its blob id in the FIRST parent (`None` = the
/// path does not exist there — an add at this node, or the node's del).
pub struct BlameStateEntry {
    pub path: String,
    pub blob_id: Option<Id>,
    pub parent_blob_id: Option<Id>,
}

/// Writes one changeset's blame diff-state (a keyed `OR REPLACE` per path:
/// a re-run after a lost lease replaces, never duplicates).
pub fn blame_state_put(
    conn: &Connection,
    repo_id: i64,
    cs: &Id,
    entries: &[BlameStateEntry],
) -> rusqlite::Result<()> {
    let mut stmt = conn.prepare_cached(
        "INSERT OR REPLACE INTO blame_state(repo_id, cs, path, blob_id, parent_blob_id) \
         VALUES (?1, ?2, ?3, ?4, ?5)",
    )?;
    for e in entries {
        stmt.execute(params![
            repo_id,
            cs.0.as_slice(),
            e.path,
            e.blob_id.as_ref().map(|b| b.0.as_slice()),
            e.parent_blob_id.as_ref().map(|b| b.0.as_slice()),
        ])?;
    }
    Ok(())
}

/// One path's blame diff-state row for a changeset:
/// `(blob_id, parent_blob_id)`, both optional (del / add), or `None` when
/// the changeset did not touch the path. The blame walk's per-node
/// question — indexed point query, one per visited chain node.
pub fn blame_state_get(
    conn: &Connection,
    repo_id: i64,
    cs: &Id,
    path: &str,
) -> rusqlite::Result<Option<(Option<Id>, Option<Id>)>> {
    conn.query_row(
        "SELECT blob_id, parent_blob_id FROM blame_state \
         WHERE repo_id = ?1 AND cs = ?2 AND path = ?3",
        params![repo_id, cs.0.as_slice(), path],
        |row| {
            let blob: Option<Vec<u8>> = row.get(0)?;
            let parent: Option<Vec<u8>> = row.get(1)?;
            Ok((
                blob.map(|b| Id(b.try_into().expect("blob id is 32 bytes"))),
                parent.map(|b| Id(b.try_into().expect("parent blob id is 32 bytes"))),
            ))
        },
    )
    .optional()
}

/// Writes one changeset's path-history index rows (an `OR IGNORE` per
/// path: the PK (repo_id, path, gen, cs) makes a re-run a no-op).
pub fn path_history_put(
    conn: &Connection,
    repo_id: i64,
    generation: i64,
    cs: &Id,
    changes: &[FileChangeRow],
) -> rusqlite::Result<()> {
    let mut stmt = conn.prepare_cached(
        "INSERT OR IGNORE INTO path_history(repo_id, path, gen, cs, op) \
         VALUES (?1, ?2, ?3, ?4, ?5)",
    )?;
    for c in changes {
        stmt.execute(params![repo_id, c.path, generation, cs.0.as_slice(), c.op,])?;
    }
    Ok(())
}

/// One page of the derived path history for one path, newest first
/// (generation DESC, changeset id ASC), resuming after a cursor row.
/// Joins `changesets` so the page carries the log-entry fields.
pub fn path_history_page(
    conn: &Connection,
    repo_id: i64,
    path: &str,
    after: Option<(i64, Vec<u8>)>,
    limit: i64,
) -> rusqlite::Result<Vec<LogRow>> {
    let (after_gen, after_cs) = match after {
        Some((g, id)) => (Some(g), id),
        None => (None, Vec::new()),
    };
    let mut stmt = conn.prepare_cached(
        "SELECT c.id, c.author, c.message, c.timestamp_ns, c.generation, c.principal \
         FROM path_history h \
         JOIN changesets c ON c.repo_id = h.repo_id AND c.id = h.cs \
         WHERE h.repo_id = ?1 AND h.path = ?2 \
           AND (?3 IS NULL OR h.gen < ?3 OR (h.gen = ?3 AND h.cs > ?4)) \
         ORDER BY h.gen DESC, h.cs ASC LIMIT ?5",
    )?;
    let rows = stmt
        .query_map(
            params![repo_id, path, after_gen, after_cs, limit],
            log_row_from,
        )?
        .collect::<rusqlite::Result<Vec<LogRow>>>()?;
    Ok(rows)
}

/// The un-accelerated path history: a full `file_changes` scan by path —
/// the same rows the derived index holds, computed on the fly. This is the
/// fallback for a server on which the path_history kind is not registered,
/// and the parity oracle's brute force.
pub fn path_history_scan(
    conn: &Connection,
    repo_id: i64,
    path: &str,
    after: Option<(i64, Vec<u8>)>,
    limit: i64,
) -> rusqlite::Result<Vec<LogRow>> {
    let (after_gen, after_cs) = match after {
        Some((g, id)) => (Some(g), id),
        None => (None, Vec::new()),
    };
    let mut stmt = conn.prepare_cached(
        "SELECT c.id, c.author, c.message, c.timestamp_ns, c.generation, c.principal \
         FROM file_changes f \
         JOIN changesets c ON c.repo_id = f.repo_id AND c.id = f.cs \
         WHERE f.repo_id = ?1 AND f.path = ?2 \
           AND (?3 IS NULL OR c.generation < ?3 OR (c.generation = ?3 AND c.id > ?4)) \
         ORDER BY c.generation DESC, c.id ASC LIMIT ?5",
    )?;
    let rows = stmt
        .query_map(
            params![repo_id, path, after_gen, after_cs, limit],
            log_row_from,
        )?
        .collect::<rusqlite::Result<Vec<LogRow>>>()?;
    Ok(rows)
}

fn log_row_from(row: &rusqlite::Row<'_>) -> rusqlite::Result<LogRow> {
    Ok(LogRow {
        id: row.get(0)?,
        author: row.get(1)?,
        message: row.get(2)?,
        timestamp_ns: row.get(3)?,
        generation: row.get(4)?,
        principal: row.get(5)?,
    })
}

/// The read-during-backlog gate's statements, exported for the scan-proof
/// pin (`m6b_test` EXPLAIN QUERY PLAN: none may scan `changesets`). An
/// earlier anti-join form — "OR any changeset of the repo has no derive
/// row" — probed `derive_state` once per changeset of the repo on EVERY
/// gated read, and cannot short-circuit when nothing is missing: seconds
/// per read on a deep repo. The helpers run exactly these strings.
pub const DERIVE_PENDING_KIND_NAME_SQL: &str = "SELECT name FROM derive_kinds WHERE id = ?1";
pub const DERIVE_PENDING_BACKFILL_DONE_SQL: &str = "SELECT value FROM meta WHERE key = ?1";
pub const DERIVE_PENDING_PROBE_SQL: &str = "SELECT 1 FROM derive_state \
     WHERE kind = ?1 AND repo_id = ?2 AND state IN ('pending','deriving') LIMIT 1";

/// Whether ANY node of `kind_id` in the repo is still gated: the kind's
/// boot backfill has not completed, or a node is queued or deriving — the
/// read-during-backlog probe. A derived read answered while this is true
/// could silently miss matches (the empty-as-if-no-history lie), so the
/// routes answer 503 `derivation_pending` first. O(1) by design — three
/// indexed point lookups ([`DERIVE_PENDING_KIND_NAME_SQL`],
/// [`DERIVE_PENDING_BACKFILL_DONE_SQL`], [`DERIVE_PENDING_PROBE_SQL`]):
///
/// 1. the kind's completion marker ([`derive_backfill_done_set`]) unset →
///    this boot's backfill is unfinished, or the previous one FAILED, or
///    an offline import added changesets the current boot has not
///    re-enqueued yet — changesets may have no derive row at all, and the
///    index provably holds nothing for them;
/// 2. marker set → every changeset has a row (the backfill enqueued
///    everything that predated it; every write path enqueues its own
///    changeset inside the writer's own transaction), so pending/deriving
///    rows are the only possible gap, and the indexed probe catches them.
pub fn derive_any_pending(conn: &Connection, kind_id: i64, repo_id: i64) -> rusqlite::Result<bool> {
    let kind_name: Option<String> = conn
        .query_row(DERIVE_PENDING_KIND_NAME_SQL, params![kind_id], |r| r.get(0))
        .optional()?;
    let Some(kind_name) = kind_name else {
        // Unreachable from the routes (they resolve the id via
        // `derive_kind_id` first); unknown kind id stays on the safe side.
        return Ok(true);
    };
    if !derive_backfill_done_get(conn, &kind_name)? {
        return Ok(true);
    }
    let hit: Option<i64> = conn
        .query_row(DERIVE_PENDING_PROBE_SQL, params![kind_id, repo_id], |r| {
            r.get(0)
        })
        .optional()?;
    Ok(hit.is_some())
}

/// The meta key of kind `name`'s boot-backfill completion marker.
fn derive_backfill_marker_key(kind_name: &str) -> String {
    format!("derive_backfill_done:{kind_name}")
}

/// Marks kind `name`'s boot backfill COMPLETE (the meta row
/// `derive_backfill_done:<name>`, value = completion ts): the batch loop
/// ended on a zero scan, so every changeset in the database now has a
/// derive row for the kind. The read gate ([`derive_any_pending`]) trusts
/// this — which is why a FAILED backfill never sets it and every boot
/// clears it before re-running ([`derive_backfill_done_clear`]).
pub fn derive_backfill_done_set(
    conn: &Connection,
    kind_name: &str,
    completed_ns: i64,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO meta(key, value) VALUES (?1, ?2) \
         ON CONFLICT(key) DO UPDATE SET value = excluded.value",
        params![
            derive_backfill_marker_key(kind_name),
            completed_ns.to_string()
        ],
    )?;
    Ok(())
}

/// Clears kind `name`'s backfill completion marker. Each boot's backfill
/// clears FIRST: while it runs, changesets written since the last boot —
/// in particular an offline import's — may have no derive row, and a
/// marker left from the previous boot would let reads answer from the
/// incomplete index.
pub fn derive_backfill_done_clear(conn: &Connection, kind_name: &str) -> rusqlite::Result<()> {
    conn.execute(
        "DELETE FROM meta WHERE key = ?1",
        params![derive_backfill_marker_key(kind_name)],
    )?;
    Ok(())
}

/// Whether kind `name`'s boot backfill completion marker is set.
pub fn derive_backfill_done_get(conn: &Connection, kind_name: &str) -> rusqlite::Result<bool> {
    let hit: Option<String> = conn
        .query_row(
            DERIVE_PENDING_BACKFILL_DONE_SQL,
            params![derive_backfill_marker_key(kind_name)],
            |r| r.get(0),
        )
        .optional()?;
    Ok(hit.is_some())
}

/// The kind's quarantined nodes in the repo whose PRIMARY file_changes
/// touch `path` — the precise degraded signal for a `?path=` page: the
/// index may be missing exactly those nodes' touches, and no others. (The
/// primary rows exist regardless of derivation, so this is checkable
/// exactly.)
pub fn derive_poison_touching_path(
    conn: &Connection,
    kind_id: i64,
    repo_id: i64,
    path: &str,
) -> rusqlite::Result<Vec<Id>> {
    let mut stmt = conn.prepare_cached(
        "SELECT d.cs FROM derive_state d \
         WHERE d.kind = ?1 AND d.repo_id = ?2 AND d.state = 'poison' \
           AND EXISTS (SELECT 1 FROM file_changes f \
                       WHERE f.repo_id = d.repo_id AND f.cs = d.cs AND f.path = ?3)",
    )?;
    let rows = stmt
        .query_map(params![kind_id, repo_id, path], |row| {
            row.get::<_, Vec<u8>>(0)
                .map(|b| Id(b.try_into().expect("cs id is 32 bytes")))
        })?
        .collect::<rusqlite::Result<Vec<Id>>>()?;
    Ok(rows)
}

/// Resolves one path in a tree to its blob id (`None` = absent, or the
/// path crosses a file, or names a directory). The blame deriver's
/// parent-blob resolution and the blame route's existence pre-check.
pub fn tree_lookup_path(conn: &Connection, root: &Id, path: &str) -> rusqlite::Result<Option<Id>> {
    let mut tree = *root;
    let components: Vec<&str> = path.split('/').collect();
    for (i, name) in components.iter().enumerate() {
        let entries = tree_entries_load(conn, &tree)?;
        let Some(entry) = entries.iter().find(|e| e.name.as_str() == *name) else {
            return Ok(None);
        };
        let object = Id(entry
            .object_id
            .clone()
            .try_into()
            .expect("object id is 32 bytes"));
        let last = i == components.len() - 1;
        match (entry.kind, last) {
            (0, true) => return Ok(Some(object)),
            (1, false) => tree = object,
            _ => return Ok(None),
        }
    }
    Ok(None)
}

/// A changeset's parent count (the commit-graph deriver's purity check: a
/// skip over a span may not lose a spanned node's non-first parents).
pub fn changeset_parent_count(conn: &Connection, repo_id: i64, cs: &Id) -> rusqlite::Result<i64> {
    conn.query_row(
        "SELECT COUNT(*) FROM changeset_parents WHERE repo_id = ?1 AND cs = ?2",
        params![repo_id, cs.0.as_slice()],
        |r| r.get(0),
    )
}

// ---- audit ---------------------------------------------------------------

/// Appends one audit row.
pub fn audit_append(
    conn: &Connection,
    ts_ns: i64,
    principal: &str,
    action: &str,
    details_json: &str,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO audit_log(ts_ns, principal, action, details_json) VALUES (?1, ?2, ?3, ?4)",
        params![ts_ns, principal, action, details_json],
    )?;
    Ok(())
}

/// Lists audit rows with `id < before_id`, descending (id order matches ts
/// order — ids are assigned at insert), up to `limit` rows.
pub fn audit_list(
    conn: &Connection,
    before_id: i64,
    limit: i64,
) -> rusqlite::Result<Vec<AuditRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT id, ts_ns, principal, action, details_json FROM audit_log \
         WHERE id < ?1 ORDER BY id DESC LIMIT ?2",
    )?;
    let rows = stmt.query_map(params![before_id, limit], |row| {
        Ok(AuditRow {
            id: row.get(0)?,
            ts_ns: row.get(1)?,
            principal: row.get(2)?,
            action: row.get(3)?,
            details_json: row.get(4)?,
        })
    })?;
    rows.collect()
}
