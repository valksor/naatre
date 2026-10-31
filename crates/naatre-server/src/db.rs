//! All server-side SQL, as plain synchronous functions over `&Db`.
//!
//! Callers run these inside [`crate::state::ServerState::read`] /
//! [`crate::state::ServerState::write`] closures (or compose several inside
//! one transaction); no other module in the crate touches SQL. Multi-step
//! mutations (mint + audit, ref CAS + audit) run in a single
//! `transaction` here so a failure rolls the whole step back.

use std::collections::HashSet;

use naatre_core::ids::Id;
use naatre_store::{Db, DbError, P, Row, Sql, pv};

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

/// True when `err` is a UNIQUE-constraint violation (SQLite constraint
/// failure, Postgres SQLSTATE 23505) — the caller maps it to its semantic
/// error, e.g. `repo_exists`.
pub fn is_unique_violation(err: &DbError) -> bool {
    err.is_unique_violation()
}

/// True when the backend refused the statement because another writer
/// holds conflicting state — SQLite lock busy, or the Postgres MVCC
/// equivalents (serialization failure, deadlock, lock unavailable). The
/// server answers these with the writer-busy-shaped retryable 503.
pub fn is_retryable_contention(err: &DbError) -> bool {
    err.is_retryable_contention()
}

// ---- repos ---------------------------------------------------------------

/// Inserts a repo; the caller maps UNIQUE violations to `repo_exists`.
pub fn repos_create(conn: &Db, name: &str, created_ns: i64) -> Result<i64, DbError> {
    conn.insert_id(
        &Sql::plain("INSERT INTO repos(name, created_ns) VALUES (?1, ?2)"),
        &pv![name, created_ns],
    )
}

/// Lists repos with `id > after_id`, ascending, up to `limit` rows.
pub fn repos_list(conn: &Db, after_id: i64, limit: i64) -> Result<Vec<RepoRow>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT id, name, created_ns FROM repos WHERE id > ?1 ORDER BY id ASC LIMIT ?2",
        ),
        &pv![after_id, limit],
        |row| {
            Ok(RepoRow {
                id: row.get(0)?,
                name: row.get(1)?,
                created_ns: row.get(2)?,
            })
        },
    )
}

/// The id of repo `name`, or `None`.
pub fn repo_id_by_name(conn: &Db, name: &str) -> Result<Option<i64>, DbError> {
    conn.query_opt_map(
        &Sql::plain("SELECT id FROM repos WHERE name = ?1"),
        &pv![name],
        |r| r.get(0),
    )
}

/// Repo-scoped counters for `GET /v1/repos/{r}/stats`.
/// Returns `(changesets, objects, bytes, cbytes)`.
pub fn repo_stats(conn: &Db, repo_id: i64) -> Result<(i64, i64, i64, i64), DbError> {
    let changesets: i64 = conn.query_row_map(
        &Sql::plain("SELECT COUNT(*) FROM changesets WHERE repo_id = ?1"),
        &pv![repo_id],
        |r| r.get(0),
    )?;
    let (objects, bytes, cbytes): (i64, i64, i64) = conn.query_row_map(
        &Sql::plain(
            "SELECT COUNT(*), COALESCE(SUM(o.size), 0), COALESCE(SUM(o.csize), 0) \
         FROM repo_blobs rb JOIN objects o ON o.object_id = rb.object_id \
         WHERE rb.repo_id = ?1",
        ),
        &pv![repo_id],
        |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
    )?;
    Ok((changesets, objects, bytes, cbytes))
}

// ---- refs ----------------------------------------------------------------

/// One ref lookup.
pub fn ref_get(conn: &Db, repo_id: i64, name: &str) -> Result<Option<RefRow>, DbError> {
    conn.query_opt_map(
        &Sql::plain("SELECT name, target, version FROM refs WHERE repo_id = ?1 AND name = ?2"),
        &pv![repo_id, name],
        |row| {
            Ok(RefRow {
                name: row.get(0)?,
                target: row.get(1)?,
                version: row.get(2)?,
            })
        },
    )
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
pub fn ref_cas(conn: &Db, mv: RefMove<'_>) -> Result<RefCas, DbError> {
    ref_cas_inner(conn, mv, None)
}

/// [`ref_cas`] with provenance: the audit row carries `via` so an
/// import-driven ref move stays distinguishable from a land or a manual
/// move (the audit trail's whole point).
pub fn ref_cas_via(conn: &Db, mv: RefMove<'_>, via: &str) -> Result<RefCas, DbError> {
    ref_cas_inner(conn, mv, Some(via))
}

fn ref_cas_inner(conn: &Db, mv: RefMove<'_>, via: Option<&str>) -> Result<RefCas, DbError> {
    let RefMove {
        repo_id,
        repo_name,
        name,
        target,
        expected,
        principal,
        now_ns,
    } = mv;
    let tx = conn.transaction()?;
    let exists: Option<i64> = tx.query_opt_map(
        &Sql::plain("SELECT id FROM repos WHERE id = ?1"),
        &pv![repo_id],
        |r| r.get(0),
    )?;
    if exists.is_none() {
        return Ok(RefCas::RepoMissing);
    }

    let current: Option<RefRow> = ref_get(&tx, repo_id, name)?;
    let outcome = match (expected, current) {
        (None, None) => {
            tx.execute(
                &Sql::plain(
                    "INSERT INTO refs(repo_id, name, target, version, last_principal) \
                 VALUES (?1, ?2, ?3, 1, ?4)",
                ),
                &pv![repo_id, name, target.0.as_slice(), principal],
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
                    &Sql::plain(
                        "UPDATE refs SET target = ?1, version = ?2, last_principal = ?3 \
                     WHERE repo_id = ?4 AND name = ?5",
                    ),
                    &pv![target.0.as_slice(), new_version, principal, repo_id, name],
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
/// fsynced, stamping `touched_ns` (M10a's upload grace clock: a content-
/// addressed re-PUT of an OLD blob is a CAS no-op, but the row touch moves
/// the blob back inside the sweep's grace window — otherwise a re-uploaded
/// orphan referenced by a concurrent changeset could be collected while
/// committed history points at it). Returns `true` when the row already
/// existed (idempotent re-PUT).
pub fn object_row_upsert(
    conn: &Db,
    object_id: &Id,
    size: i64,
    csize: i64,
    touched_ns: i64,
) -> Result<bool, DbError> {
    let existed: Option<i64> = conn.query_opt_map(
        &Sql::plain("SELECT 1 FROM objects WHERE object_id = ?1"),
        &pv![object_id.0.as_slice()],
        |r| r.get(0),
    )?;
    conn.execute(
        &Sql::plain(
            "INSERT INTO objects(object_id, size, csize, touched_ns) VALUES (?1, ?2, ?3, ?4) \
         ON CONFLICT(object_id) DO UPDATE SET size = excluded.size, csize = excluded.csize, \
           touched_ns = excluded.touched_ns",
        ),
        &pv![object_id.0.as_slice(), size, csize, touched_ns],
    )?;
    // Any touch clears the two-phase delete's condemnation mark (the GC's
    // phase B only deletes bytes+rows while the mark is still set) — an
    // upload wins over a pending condemnation in the same write slot.
    conn.execute(
        &Sql::plain("UPDATE gc_pending SET condemned_at = NULL WHERE scope = 'blob' AND id = ?1"),
        &pv![object_id.0.as_slice()],
    )?;
    Ok(existed.is_some())
}

/// Membership check: does repo `repo_id` reference `object_id` via
/// `repo_blobs`? Every object read is gated on this.
pub fn repo_blob_check(conn: &Db, repo_id: i64, object_id: &Id) -> Result<bool, DbError> {
    let hit: Option<i64> = conn.query_opt_map(
        &Sql::plain("SELECT 1 FROM repo_blobs WHERE repo_id = ?1 AND object_id = ?2"),
        &pv![repo_id, object_id.0.as_slice()],
        |r| r.get(0),
    )?;
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
#[derive(Clone)]
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
pub fn changeset_get(conn: &Db, repo_id: i64, id: &Id) -> Result<Option<ChangesetRow>, DbError> {
    conn.query_opt_map(
        &Sql::plain(
            "SELECT canonical, author, message, timestamp_ns, generation, principal, root_tree_id \
         FROM changesets WHERE repo_id = ?1 AND id = ?2",
        ),
        &pv![repo_id, id.0.as_slice()],
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
}

/// A changeset's generation, parents (in `ord` order), and root tree, or
/// `None`.
pub fn changeset_graph(conn: &Db, repo_id: i64, id: &Id) -> Result<Option<CsGraph>, DbError> {
    let row: Option<(i64, Option<Vec<u8>>)> = conn.query_opt_map(
        &Sql::plain(
            "SELECT generation, root_tree_id FROM changesets WHERE repo_id = ?1 AND id = ?2",
        ),
        &pv![repo_id, id.0.as_slice()],
        |r| Ok((r.get(0)?, r.get(1)?)),
    )?;
    let Some((generation, root_tree_id)) = row else {
        return Ok(None);
    };
    let parents = conn.query_all_map(
        &Sql::plain(
            "SELECT parent FROM changeset_parents WHERE repo_id = ?1 AND cs = ?2 ORDER BY ord ASC",
        ),
        &pv![repo_id, id.0.as_slice()],
        |row| {
            row.get::<Vec<u8>>(0)
                .map(|b| Id(b.try_into().expect("parent id is 32 bytes")))
        },
    )?;
    Ok(Some(CsGraph {
        generation,
        parents,
        root_tree_id,
    }))
}

/// All file changes of one changeset, ascending by path (the PK order).
pub fn file_changes_list(conn: &Db, repo_id: i64, cs: &Id) -> Result<Vec<FileChangeRow>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT path, op, blob_id, size, exec FROM file_changes \
         WHERE repo_id = ?1 AND cs = ?2 ORDER BY path ASC",
        ),
        &pv![repo_id, cs.0.as_slice()],
        |row| {
            Ok(FileChangeRow {
                path: row.get(0)?,
                op: row.get(1)?,
                blob_id: row.get(2)?,
                size: row.get(3)?,
                exec: row.get::<i64>(4)? != 0,
            })
        },
    )
}

/// The ids among `ids` that have no `objects` row (blob-presence check).
pub fn objects_missing(conn: &Db, ids: &[Id]) -> Result<Vec<Id>, DbError> {
    let mut missing = Vec::new();
    for id in ids {
        let hit: Option<i64> = conn.query_opt_map(
            &Sql::plain("SELECT 1 FROM objects WHERE object_id = ?1"),
            &pv![id.0.as_slice()],
            |r| r.get(0),
        )?;
        if hit.is_none() {
            missing.push(*id);
        }
    }
    Ok(missing)
}

/// All entries of one tree node, ascending by name.
pub fn tree_entries_load(conn: &Db, tree_id: &Id) -> Result<Vec<TreeEntryRow>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT name, kind, object_id, size, exec FROM tree_entries \
         WHERE tree_id = ?1 ORDER BY name ASC",
        ),
        &pv![tree_id.0.as_slice()],
        |row| {
            Ok(TreeEntryRow {
                name: row.get(0)?,
                kind: row.get(1)?,
                object_id: row.get(2)?,
                size: row.get(3)?,
                exec: row.get::<i64>(4)? != 0,
            })
        },
    )
}

/// The repo a `trees` row is attributed to (the first writer, since rows
/// are `INSERT OR IGNORE`), or `None` (unknown tree id). Informational
/// only — reads are gated on `repo_trees` membership (`repo_has_tree`),
/// never on this column.
pub fn tree_repo(conn: &Db, tree_id: &Id) -> Result<Option<i64>, DbError> {
    conn.query_opt_map(
        &Sql::plain("SELECT repo_id FROM trees WHERE id = ?1"),
        &pv![tree_id.0.as_slice()],
        |r| r.get(0),
    )
}

/// Persists HISTORY-derived trees (changeset create, land, import): one
/// `trees` row (canonical bytes), its flat `tree_entries` rows, and this
/// repo's `repo_trees` membership row each. `INSERT OR IGNORE` keyed by id
/// makes repeats idempotent (shared subtrees re-derived by a child
/// changeset are no-ops; identical id implies identical bytes). The
/// membership row is what later gates bare-id tree reads. Snapshot trees
/// must NOT go through here — use [`trees_write_snapshot`].
pub fn trees_write(conn: &Db, repo_id: i64, trees: &[TreeWrite]) -> Result<(), DbError> {
    for tree in trees {
        write_tree_rows(conn, repo_id, tree)?;
        conn.execute(
            &Sql::ignore("INSERT OR IGNORE INTO repo_trees(repo_id, tree_id) VALUES (?1, ?2)"),
            &pv![repo_id, tree.id.0.as_slice()],
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
pub fn trees_write_snapshot(conn: &Db, trees: &[TreeWrite]) -> Result<(), DbError> {
    for tree in trees {
        write_tree_rows(conn, 0, tree)?;
    }
    Ok(())
}

/// One `trees` row plus its flat `tree_entries` rows (`INSERT OR IGNORE` /
/// `INSERT OR REPLACE`, idempotent repeats). `repo_id` is informational.
fn write_tree_rows(conn: &Db, repo_id: i64, tree: &TreeWrite) -> Result<(), DbError> {
    conn.execute(
        &Sql::ignore("INSERT OR IGNORE INTO trees(id, repo_id, canonical) VALUES (?1, ?2, ?3)"),
        &pv![tree.id.0.as_slice(), repo_id, &tree.canonical],
    )?;
    for e in &tree.entries {
        conn.execute(
            &Sql::replace(
                "INSERT OR REPLACE INTO tree_entries(tree_id, name, kind, object_id, size, exec) \
             VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
                &["tree_id", "name"],
                &["kind", "object_id", "size", "exec"],
            ),
            &pv![
                tree.id.0.as_slice(),
                &e.name,
                e.kind,
                &e.object_id,
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
pub fn any_junctionless_trees(conn: &Db) -> Result<bool, DbError> {
    conn.query_opt_map(
        &Sql::plain(
            "SELECT 1 FROM trees t \
         WHERE NOT EXISTS (SELECT 1 FROM repo_trees rt WHERE rt.tree_id = t.id) LIMIT 1",
        ),
        &pv![],
        |_| Ok(()),
    )
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
pub fn unprotected_trees(conn: &Db, now_ns: i64) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
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
        ),
        &pv![now_ns],
        |row| {
            row.get::<Vec<u8>>(0)
                .map(|b| Id(b.try_into().expect("tree id is 32 bytes")))
        },
    )
}

/// Deletes the given `trees` rows and their `tree_entries` rows; returns
/// the number of `trees` rows deleted. The write-side half of the sweep.
pub fn delete_tree_rows(conn: &Db, ids: &[Id]) -> Result<usize, DbError> {
    let mut n = 0;
    for id in ids {
        conn.execute(
            &Sql::plain("DELETE FROM tree_entries WHERE tree_id = ?1"),
            &pv![id.0.as_slice()],
        )?;
        n += conn.execute(
            &Sql::plain("DELETE FROM trees WHERE id = ?1"),
            &pv![id.0.as_slice()],
        )?;
    }
    Ok(n)
}

/// Whether `repo_id` wrote `tree_id` (a `repo_trees` membership row) — the
/// read gate for the bare-id tree route. A tree is readable from every repo
/// that wrote it and from no other: content-addressing shares the row, the
/// junction shares readability only with writers.
pub fn repo_has_tree(conn: &Db, repo_id: i64, tree_id: &Id) -> Result<bool, DbError> {
    let hit: Option<i64> = conn.query_opt_map(
        &Sql::plain("SELECT 1 FROM repo_trees WHERE repo_id = ?1 AND tree_id = ?2"),
        &pv![repo_id, tree_id.0.as_slice()],
        |r| r.get(0),
    )?;
    Ok(hit.is_some())
}

/// Whether `target` sits in the subtree closure of `root` (dir entries,
/// `kind = 1`): the snapshot tree route's membership check. `root` itself
/// counts (a closure contains its root). Content-addressed trees cannot
/// cycle (an entry's id covers the bytes that name it), and the seen-set
/// keeps shared subtrees from being walked twice anyway.
pub fn tree_in_closure(conn: &Db, root: &Id, target: &Id) -> Result<bool, DbError> {
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

pub fn changeset_insert(conn: &Db, rec: ChangesetInsert<'_>) -> Result<(), DbError> {
    let tx = conn.transaction()?;
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
    conn: &Db,
    rec: ChangesetInsert<'_>,
    extras: ChangesetExtras<'_>,
) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns, \
         generation, principal, root_tree_id) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)",
        ),
        &pv![
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
            &Sql::plain(
                "INSERT INTO changeset_parents(repo_id, cs, parent, ord) VALUES (?1, ?2, ?3, ?4)",
            ),
            &pv![
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
            &Sql::plain(
                "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            ),
            &pv![
                rec.repo_id,
                rec.id.0.as_slice(),
                &c.path,
                op_tag(c.op),
                blob,
                size,
                exec,
            ],
        )?;
    }
    for blob in rec.blobs {
        conn.execute(
            &Sql::ignore("INSERT OR IGNORE INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)"),
            &pv![rec.repo_id, blob.0.as_slice()],
        )?;
    }
    if let Some(git_oid) = extras.git_map_oid {
        conn.execute(
            &Sql::plain("INSERT INTO git_map(repo_id, git_oid, cs_id) VALUES (?1, ?2, ?3)"),
            &pv![rec.repo_id, git_oid, rec.id.0.as_slice()],
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
/// rows. The chain itself is the deterministic order. Actively-tombstoned
/// changesets DROP OUT of the walk at mark time (the plan's universal gate:
/// attribution never serves after the mark — the purge only reclaims space
/// later): the walk skips the node without emitting or counting it and
/// continues to its first parent, so a page never carries a tombstoned entry
/// and cursors resume cleanly past them (one indexed redaction probe per
/// visited node, the same cost class as the graph + row lookups).
pub fn log_chain(conn: &Db, repo_id: i64, from: &Id, limit: i64) -> Result<Vec<LogRow>, DbError> {
    let mut rows = Vec::new();
    let mut current = *from;
    loop {
        let Some(graph) = changeset_graph(conn, repo_id, &current)? else {
            break;
        };
        let redacted = redaction_active(conn, repo_id, &current)?;
        let row = if redacted {
            None
        } else {
            conn.query_opt_map(
                &Sql::plain(
                    "SELECT author, message, timestamp_ns, generation, principal FROM changesets \
                 WHERE repo_id = ?1 AND id = ?2",
                ),
                &pv![repo_id, current.0.as_slice()],
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
            )?
        };
        if let Some(row) = row {
            rows.push(row);
            if rows.len() as i64 >= limit {
                break;
            }
        }
        match graph.parents.first() {
            Some(next) => current = *next,
            // The chain ended without filling the limit — every remaining
            // page is empty; nothing is skipped that a later page would
            // re-serve.
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
    conn: &Db,
    repo_id: i64,
    principal: &str,
    key: &str,
) -> Result<Option<LandRow>, DbError> {
    conn.query_opt_map(
        &Sql::plain(
            "SELECT state, head, base, result_json FROM lands \
         WHERE repo_id = ?1 AND principal = ?2 AND key = ?3",
        ),
        &pv![repo_id, principal, key],
        |row| {
            Ok(LandRow {
                state: row.get(0)?,
                head: row.get(1)?,
                base: row.get(2)?,
                result_json: row.get(3)?,
            })
        },
    )
}

/// The in-flight marker (state='pending'). It commits only together with
/// the finished land (one transaction), so every failure path — including
/// the crash points before COMMIT — leaves the key unsettled.
pub fn land_insert_pending(
    conn: &Db,
    repo_id: i64,
    principal: &str,
    key: &str,
    head: &Id,
) -> Result<(), DbError> {
    conn.execute(&Sql::plain("INSERT INTO lands(repo_id, principal, key, state, head) VALUES (?1, ?2, ?3, 'pending', ?4)"),
        &pv![repo_id, principal, key, head.0.as_slice()],
    )?;
    Ok(())
}

/// Settles the land row to state='done': `head` is the requested head (the
/// idempotency payload), `base` the main target the land rebased onto, and
/// `result_json` the recorded `{landed, rebased, note}`. Upsert form, so
/// the pending row inside the same transaction is overwritten.
#[allow(clippy::too_many_arguments)]
pub fn land_settle(
    conn: &Db,
    repo_id: i64,
    principal: &str,
    key: &str,
    head: &Id,
    base: &Id,
    result_json: &str,
) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "INSERT INTO lands(repo_id, principal, key, state, head, base, result_json) \
         VALUES (?1, ?2, ?3, 'done', ?4, ?5, ?6) \
         ON CONFLICT(repo_id, principal, key) DO UPDATE SET \
           state = 'done', head = excluded.head, base = excluded.base, \
           result_json = excluded.result_json",
        ),
        &pv![
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
    conn: &Db,
    repo_id: i64,
    expected_target: &Id,
    new_target: &Id,
    principal: &str,
) -> Result<usize, DbError> {
    conn.execute(
        &Sql::plain(
            "UPDATE refs SET target = ?3, version = version + 1, last_principal = ?4 \
         WHERE repo_id = ?1 AND name = 'main' AND target = ?2",
        ),
        &pv![
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
pub fn snapshot_get(conn: &Db, repo_id: i64, id: &Id) -> Result<Option<SnapshotRow>, DbError> {
    conn.query_opt_map(
        &Sql::plain(
            "SELECT tree_id, principal, created_ns, expires_ns FROM snapshots \
         WHERE repo_id = ?1 AND id = ?2",
        ),
        &pv![repo_id, id.0.as_slice()],
        |row| {
            Ok(SnapshotRow {
                tree_id: row.get(0)?,
                principal: row.get(1)?,
                created_ns: row.get(2)?,
                expires_ns: row.get(3)?,
            })
        },
    )
}

/// Inserts one snapshot row. Duplicate ids are rejected by the PRIMARY KEY;
/// the route checks for an existing row first (idempotent re-POST).
pub fn snapshot_insert(
    conn: &Db,
    repo_id: i64,
    id: &Id,
    tree_id: &Id,
    principal: &str,
    created_ns: i64,
    expires_ns: i64,
) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "INSERT INTO snapshots(id, repo_id, tree_id, principal, created_ns, expires_ns) \
         VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
        ),
        &pv![
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
pub fn snapshots_expired(conn: &Db, now_ns: i64) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain("SELECT id FROM snapshots WHERE expires_ns <= ?1 ORDER BY id ASC"),
        &pv![now_ns],
        |row| {
            row.get::<Vec<u8>>(0)
                .map(|b| Id(b.try_into().expect("snapshot id is 32 bytes")))
        },
    )
}

/// Deletes the given snapshot rows; returns how many existed. GC purges a
/// row only after its ephemeral dir is gone, so a crash leaves an orphan
/// row (never a dangling dir-less row that serves) that the next run purges.
pub fn snapshots_purge(conn: &Db, ids: &[Id]) -> Result<usize, DbError> {
    let mut purged = 0;
    for id in ids {
        purged += conn.execute(
            &Sql::plain("DELETE FROM snapshots WHERE id = ?1"),
            &pv![id.0.as_slice()],
        )?;
    }
    Ok(purged)
}

/// Which of `ids` have a snapshots row (the rowless-dir sweep's cross-check:
/// a dir with a row is a live or expired snapshot, not garbage).
pub fn snapshots_exist(conn: &Db, ids: &[Id]) -> Result<Vec<bool>, DbError> {
    ids.iter()
        .map(|id| {
            let hit: Option<i64> = conn.query_opt_map(
                &Sql::plain("SELECT 1 FROM snapshots WHERE id = ?1"),
                &pv![id.0.as_slice()],
                |r| r.get(0),
            )?;
            Ok(hit.is_some())
        })
        .collect()
}

// ---- principals & tokens -------------------------------------------------

/// Creates or re-roles a principal.
pub fn principal_upsert(conn: &Db, name: &str, role: &str, created_ns: i64) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "INSERT INTO principals(name, role, created_ns) VALUES (?1, ?2, ?3) \
         ON CONFLICT(name) DO UPDATE SET role = excluded.role",
        ),
        &pv![name, role, created_ns],
    )?;
    Ok(())
}

/// Inserts a token row (hash only — token material never persists).
pub fn token_insert(
    conn: &Db,
    principal: &str,
    blake3: &[u8; 32],
    expires_ns: Option<i64>,
    scope_path: Option<&str>,
) -> Result<i64, DbError> {
    conn.insert_id(
        &Sql::plain(
            "INSERT INTO tokens(principal, blake3, expires_ns, scope_path) VALUES (?1, ?2, ?3, ?4)",
        ),
        &pv![principal, blake3, expires_ns, scope_path],
    )
}

/// Mints a token row: principal upsert, token insert, and the `token_create`
/// audit row land in one transaction. Returns the new token row id.
pub fn token_create(
    conn: &Db,
    principal: &str,
    role: &str,
    blake3: &[u8; 32],
    expires_ns: Option<i64>,
    scope_path: Option<&str>,
    now_ns: i64,
) -> Result<i64, DbError> {
    let tx = conn.transaction()?;
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
pub fn tokens_list(conn: &Db) -> Result<Vec<TokenRow>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT t.id, t.principal, p.role, t.expires_ns, t.scope_path, t.revoked \
         FROM tokens t JOIN principals p ON p.name = t.principal ORDER BY t.id ASC",
        ),
        &pv![],
        |row| {
            Ok(TokenRow {
                id: row.get(0)?,
                principal: row.get(1)?,
                role: row.get(2)?,
                expires_ns: row.get(3)?,
                scope_path: row.get(4)?,
                revoked: row.get::<i64>(5)? != 0,
            })
        },
    )
}

/// Revokes token `id`. Returns `None` when no such token exists, and
/// `Some(first_revocation)` — `true` only on the 0→1 transition, so an
/// idempotent re-revoke answers 200 without writing a second audit row.
pub fn token_revoke(conn: &Db, id: i64, now_ns: i64) -> Result<Option<bool>, DbError> {
    let tx = conn.transaction()?;
    let exists: Option<String> = tx.query_opt_map(
        &Sql::plain("SELECT principal FROM tokens WHERE id = ?1"),
        &pv![id],
        |r| r.get(0),
    )?;
    let Some(principal) = exists else {
        return Ok(None);
    };
    let changed = tx.execute(
        &Sql::plain("UPDATE tokens SET revoked = 1 WHERE id = ?1 AND revoked = 0"),
        &pv![id],
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
pub fn auth_lookup(conn: &Db, blake3: &[u8; 32]) -> Result<Option<AuthRow>, DbError> {
    conn.query_opt_map(
        &Sql::plain(
            "SELECT t.id, t.principal, p.role, t.expires_ns, t.revoked, t.scope_path \
         FROM tokens t JOIN principals p ON p.name = t.principal WHERE t.blake3 = ?1",
        ),
        &pv![blake3.as_slice()],
        |row| {
            Ok(AuthRow {
                token_id: row.get(0)?,
                principal: row.get(1)?,
                role: row.get(2)?,
                expires_ns: row.get(3)?,
                revoked: row.get::<i64>(4)? != 0,
                scope_path: row.get(5)?,
            })
        },
    )
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
pub fn derive_kind_id(conn: &Db, name: &str) -> Result<Option<i64>, DbError> {
    conn.query_opt_map(
        &Sql::plain("SELECT id FROM derive_kinds WHERE name = ?1"),
        &pv![name],
        |r| r.get(0),
    )
}

/// Seeds (or finds) the registry row for kind `name`; returns its id.
/// `INSERT OR IGNORE` keeps the id stable across processes: the first
/// registrant of a name owns its id forever.
pub fn derive_kind_seed(conn: &Db, name: &str) -> Result<i64, DbError> {
    conn.execute(
        &Sql::ignore("INSERT OR IGNORE INTO derive_kinds(name) VALUES (?1)"),
        &pv![name],
    )?;
    conn.query_row_map(
        &Sql::plain("SELECT id FROM derive_kinds WHERE name = ?1"),
        &pv![name],
        |r| r.get(0),
    )
}

/// Enqueues one changeset for kind `kind_id` (state='pending', attempts=0).
/// `OR IGNORE`: an existing row (done from an eager derive, or an earlier
/// enqueue) is never clobbered.
pub fn derive_enqueue_pending(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
) -> Result<(), DbError> {
    conn.execute(
        &Sql::ignore(
            "INSERT OR IGNORE INTO derive_state(kind, repo_id, cs, state) \
         VALUES (?1, ?2, ?3, 'pending')",
        ),
        &pv![kind_id, repo_id, cs.0.as_slice()],
    )?;
    Ok(())
}

/// Records a COMPLETED eager derivation straight to state='done' (the eager
/// path runs inside the changeset's own write transaction — there is no
/// lease to take and nothing can interleave). Taint comes from the
/// parents the derivation consumed, exactly as in the queue's record step.
pub fn derive_state_insert_done(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    output: &[u8],
    tainted: bool,
    poison_cs: Option<&Id>,
) -> Result<(), DbError> {
    conn.execute(&Sql::plain("INSERT INTO derive_state(kind, repo_id, cs, state, attempts, output, tainted, poison_cs) \
         VALUES (?1, ?2, ?3, 'done', 0, ?4, ?5, ?6)"),
        &pv![
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
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
) -> Result<Option<DeriveStateRow>, DbError> {
    conn.query_opt_map(
        &Sql::plain(
            "SELECT state, lease_expires_ns, attempts, output, tainted, poison_cs \
         FROM derive_state \
         WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
        ),
        &pv![kind_id, repo_id, cs.0.as_slice()],
        |row| {
            Ok(DeriveStateRow {
                state: row.get(0)?,
                lease_expires_ns: row.get(1)?,
                attempts: row.get(2)?,
                output: row.get(3)?,
                tainted: row.get::<i64>(4)? != 0,
                poison_cs: row
                    .get::<Option<Vec<u8>>>(5)?
                    .map(|b| Id(b.try_into().expect("poison cs id is 32 bytes"))),
            })
        },
    )
}

/// The oldest pending node of `kind_id` whose parents are all out of
/// `pending`/`deriving` — done and poison parents never block (the plan's
/// never-head-of-line rule; a poison parent's children proceed on the
/// empty-marker fallback). Parents with NO row at all also never block:
/// within the framework every enqueued changeset gets a row per registered
/// kind, so a missing row means a kind registered after the fact (M6b's
/// backfill enqueues oldest-first, parents before children). Enqueue order
/// (`rowid`) is the "oldest" order.
pub fn derive_pick_next(conn: &Db, kind_id: i64) -> Result<Option<(i64, Id)>, DbError> {
    let sql = Sql::plain_owned(format!(
        "SELECT s.repo_id, s.cs FROM derive_state s \
         WHERE s.kind = ?1 AND s.state = 'pending' \
         AND NOT EXISTS (\
             SELECT 1 FROM changeset_parents p \
             WHERE p.repo_id = s.repo_id AND p.cs = s.cs \
               AND EXISTS (SELECT 1 FROM derive_state d \
                           WHERE d.kind = s.kind AND d.repo_id = p.repo_id \
                             AND d.cs = p.parent \
                             AND d.state IN ('pending','deriving'))) \
         ORDER BY {} ASC LIMIT 1",
        conn.rowid("s")
    ));
    conn.query_opt_map(&sql, &pv![kind_id], |row| {
        Ok((
            row.get::<i64>(0)?,
            row.get::<Vec<u8>>(1)
                .map(|b| Id(b.try_into().expect("cs id is 32 bytes")))?,
        ))
    })
}

/// Takes the lease on a pending node: state → 'deriving' with
/// `lease_expires_ns` set. Returns `false` when the node was no longer
/// pending (the pick was stale).
pub fn derive_lease(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    lease_expires_ns: i64,
) -> Result<bool, DbError> {
    let n = conn.execute(
        &Sql::plain(
            "UPDATE derive_state SET state = 'deriving', lease_expires_ns = ?4 \
         WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3 AND state = 'pending'",
        ),
        &pv![kind_id, repo_id, cs.0.as_slice(), lease_expires_ns],
    )?;
    Ok(n == 1)
}

/// Releases a held lease straight back to 'pending' after the pass aborted
/// on write-slot contention (`writer_busy`): the abort is not a derivation
/// failure, so `attempts` is untouched and the next tick re-picks the node
/// instead of the expiry sweep waiting out the full lease period (every
/// abort used to cost one, so repeated collisions burned attempts — and
/// three of them quarantined a healthy node). Guarded on state='deriving'
/// AND on `lease_expires_ns` matching the exact value THIS pass set at
/// lease time: a row already swept (lease cleared, attempt burned by the
/// sweep) or somehow re-leased by another holder is left alone. Returns
/// `false` when no matching row existed.
pub fn derive_release_lease(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    lease_expires_ns: i64,
) -> Result<bool, DbError> {
    let n = conn.execute(
        &Sql::plain(
            "UPDATE derive_state SET state = 'pending', lease_expires_ns = NULL \
         WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3 \
           AND state = 'deriving' AND lease_expires_ns = ?4",
        ),
        &pv![kind_id, repo_id, cs.0.as_slice(), lease_expires_ns],
    )?;
    Ok(n == 1)
}

/// One parent's contribution to a child derivation, in the parents' `ord`
/// order: the output the child receives (the empty marker for a poisoned,
/// pending, or row-less parent), whether the parent row is ITSELF poison,
/// and the parent's inherited taint (see [`DeriveStateRow::tainted`]).
pub fn derive_parent_outputs(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    parents: &[Id],
) -> Result<Vec<crate::derive::ParentOut>, DbError> {
    parents
        .iter()
        .map(|p| {
            let row = conn.query_opt_map(
                &Sql::plain(
                    "SELECT state, output, tainted, poison_cs FROM derive_state \
                     WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
                ),
                &pv![kind_id, repo_id, p.0.as_slice()],
                |row| {
                    Ok((
                        row.get::<String>(0)?,
                        row.get::<Option<Vec<u8>>>(1)?,
                        row.get::<i64>(2)? != 0,
                        row.get::<Option<Vec<u8>>>(3)?,
                    ))
                },
            )?;
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
/// `false`) — and on the changeset still existing: the reachability GC
/// collects a changeset's `derive_state` rows in its own single-writer
/// transaction, and a record for a collected changeset must not resurrect
/// a row over a dead node (the same-writer-slot serialization makes the
/// re-check honest — see [`derive_fail_attempt`]).
pub fn derive_record_done(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    output: &[u8],
    tainted: bool,
    poison_cs: Option<&Id>,
) -> Result<bool, DbError> {
    if !changeset_exists(conn, cs)? {
        return Ok(false);
    }
    let n = conn.execute(
        &Sql::plain(
            "UPDATE derive_state SET state = 'done', output = ?4, lease_expires_ns = NULL, \
         tainted = ?5, poison_cs = ?6 \
         WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3 AND state = 'deriving'",
        ),
        &pv![
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

/// Does the changeset row still exist? The GC-race guard for the derive
/// record paths: a changeset the reachability GC collected has no queue
/// row left to update — and a re-INSERT for it (the UPSERT arm of
/// [`derive_fail_attempt`]) would create a permanent `fsck` dangling
/// (`derive_state_cs_missing`). One indexed PK probe; runs inside the
/// caller's slot-held transaction, so a collection that committed before
/// this transaction took the single-writer slot is always seen.
fn changeset_exists(conn: &Db, cs: &Id) -> Result<bool, DbError> {
    let hit: Option<i64> = conn.query_opt_map(
        &Sql::plain("SELECT 1 FROM changesets WHERE id = ?1"),
        &pv![cs.0.as_slice()],
        |r| r.get(0),
    )?;
    Ok(hit.is_some())
}

/// Rows one derive diagnostic report. The POISON quarantine rows its
/// reason here ([`derive_fail_attempt`]'s cap arm); the walk-deadline
/// overruns row their MEASURED reason here per attempt too — retryable
/// events, logged so the units/sec measurement is readable without
/// waiting for a quarantine.
pub fn derive_report(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    reason: &str,
    ts_ns: i64,
) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "INSERT INTO derive_reports(kind, repo_id, cs, reason, ts_ns) VALUES (?1, ?2, ?3, ?4, ?5)",
        ),
        &pv![kind_id, repo_id, cs.0.as_slice(), reason, ts_ns],
    )?;
    Ok(())
}

/// Records one failed attempt (a deriver `Err`, or the lease sweep's
/// expiry): attempts+1; at the cap the node is QUARANTINED to 'poison' with
/// a `derive_reports` row, otherwise it goes back to 'pending'. Returns
/// `false` when nothing was recorded — including for a changeset the
/// reachability GC already collected (the guard re-checks existence in the
/// caller's slot-held transaction: the UPSERT arm below would re-INSERT a
/// queue row for a dead changeset, a permanent fsck dangling). UPSERT: the
/// EAGER path calls this before any row exists (a first-attempt `Fail`
/// inside the create's transaction — the row lands 'pending' at attempts=1
/// and the queue takes it from there); an existing pending/deriving row is
/// updated in place. Plain statements — see the module note on
/// transactions.
pub fn derive_fail_attempt(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    reason: &str,
    now_ns: i64,
) -> Result<bool, DbError> {
    if !changeset_exists(conn, cs)? {
        return Ok(false);
    }
    let attempts: Option<i64> = conn.query_opt_map(
        &Sql::plain(
            "SELECT attempts FROM derive_state WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
        ),
        &pv![kind_id, repo_id, cs.0.as_slice()],
        |r| r.get(0),
    )?;
    let attempts = attempts.unwrap_or(0) + 1;
    if attempts >= crate::derive::DERIVE_MAX_ATTEMPTS {
        conn.execute(
            &Sql::plain(
                "INSERT INTO derive_state(kind, repo_id, cs, state, attempts) \
             VALUES (?1, ?2, ?3, 'poison', ?4) \
             ON CONFLICT(kind, repo_id, cs) DO UPDATE SET \
               state = 'poison', lease_expires_ns = NULL, attempts = ?4",
            ),
            &pv![kind_id, repo_id, cs.0.as_slice(), attempts],
        )?;
        derive_report(conn, kind_id, repo_id, cs, reason, now_ns)?;
        Ok(true)
    } else {
        conn.execute(
            &Sql::plain(
                "INSERT INTO derive_state(kind, repo_id, cs, state, attempts) \
             VALUES (?1, ?2, ?3, 'pending', ?4) \
             ON CONFLICT(kind, repo_id, cs) DO UPDATE SET \
               state = 'pending', lease_expires_ns = NULL, attempts = ?4",
            ),
            &pv![kind_id, repo_id, cs.0.as_slice(), attempts],
        )?;
        Ok(false)
    }
}

/// The kind's expired leases: (repo_id, cs) of 'deriving' rows whose lease
/// ran out — a crashed or hung derivation holder. The caller feeds each
/// back through [`derive_fail_attempt`] inside one transaction.
pub fn derive_expired_leases(
    conn: &Db,
    kind_id: i64,
    now_ns: i64,
) -> Result<Vec<(i64, Id)>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT repo_id, cs FROM derive_state \
         WHERE kind = ?1 AND state = 'deriving' \
           AND lease_expires_ns IS NOT NULL AND lease_expires_ns < ?2",
        ),
        &pv![kind_id, now_ns],
        |row| {
            Ok((
                row.get::<i64>(0)?,
                row.get::<Vec<u8>>(1)
                    .map(|b| Id(b.try_into().expect("cs id is 32 bytes")))?,
            ))
        },
    )
}

/// The boot-backfill batch cursor: the (generation, changesets.rowid)
/// position the last batch scanned through. Row values compare
/// lexicographically, and (generation, rowid) is unique, so an exclusive
/// `>` seek never skips or revisits an unrowed changeset — including when
/// the LIMIT cuts inside a generation group (siblings share a generation;
/// a generation-only cursor would skip the rest of the group).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct BackfillCursor {
    pub generation: i64,
    pub rowid: i64,
}

impl BackfillCursor {
    /// The scan's start: strictly before every real (generation ≥ 0,
    /// rowid ≥ 1) row.
    pub fn start() -> Self {
        BackfillCursor {
            generation: -1,
            rowid: 0,
        }
    }
}

/// Backfills ONE BATCH of kind `kind_id`'s queue: enqueues up to `limit`
/// changesets that have no `derive_state` row for the kind (an M6b kind
/// registered after its changesets existed — bulk import through a
/// registry-less process, or an in-place upgrade of a pre-derive
/// database), in (generation, rowid) order after `cursor`. Insertion order
/// follows `generation ASC`, so the queue's `rowid`-ordered pick derives
/// parents before children with no extra sequencing — including across
/// batch boundaries (a parent's generation is always below its child's).
/// The cursor — not a NOT-EXISTS-over-everything scan — is the batch
/// boundary: each call seeks into the generation index at the cursor and
/// probes only the rows it actually scans, instead of probing every rowed
/// changeset on every batch (O(N²/2B) indexed probes over a whole backfill
/// — minutes of single-writer-slot holds at a million changesets).
/// `OR IGNORE`: a row that already exists is never clobbered. Returns the
/// number of rows enqueued (0 means the kind's backfill is complete) and
/// the advanced cursor. One batch per call is deliberate: the caller
/// (async) yields the single-writer slot between batches — a whole-repo
/// INSERT..SELECT would hold the slot, and 503 every concurrent write, for
/// the whole backfill. The two statements run on the caller's connection
/// inside one write closure: the cursor reflects exactly what this batch
/// committed.
pub fn derive_backfill_batch(
    conn: &Db,
    kind_id: i64,
    cursor: BackfillCursor,
    limit: i64,
) -> Result<(usize, BackfillCursor), DbError> {
    if limit <= 0 {
        return Ok((0, cursor));
    }
    // The batch's inserted rows are exactly those with a derive_state rowid
    // past this mark (the INSERT is one atomic statement — no concurrent
    // writer can interleave).
    let c_rowid = conn.rowid("c");
    let d_rowid = conn.rowid("d");
    // The derive_state table is unaliased in this statement: qualify the
    // rowid with the table name, not the pick query's `s` alias.
    let pre_max_rowid: i64 = conn.query_row_map(
        &Sql::plain_owned(format!(
            "SELECT COALESCE(MAX({}), 0) FROM derive_state WHERE kind = ?1",
            conn.rowid("derive_state")
        )),
        &pv![kind_id],
        |r| r.get(0),
    )?;
    let inserted = conn.execute(
        &Sql::ignore_owned(format!(
            "INSERT OR IGNORE INTO derive_state(kind, repo_id, cs, state) \
             SELECT ?1, c.repo_id, c.id, 'pending' FROM changesets c \
             WHERE (c.generation, {c_rowid}) > (?2, ?3) \
               AND NOT EXISTS (\
                 SELECT 1 FROM derive_state d \
                 WHERE d.kind = ?1 AND d.repo_id = c.repo_id AND d.cs = c.id) \
             ORDER BY c.generation ASC, {c_rowid} ASC \
             LIMIT ?4"
        )),
        &pv![kind_id, cursor.generation, cursor.rowid, limit],
    )?;
    if inserted == 0 {
        return Ok((0, cursor));
    }
    // Advance to the last scanned changeset: the scan is ascending in both
    // coordinates, so the last-inserted derive row's changeset IS it.
    let (generation, rowid) = conn.query_row_map(
        &Sql::plain_owned(format!(
            "SELECT c.generation, {c_rowid} \
             FROM derive_state d \
             JOIN changesets c ON c.repo_id = d.repo_id AND c.id = d.cs \
             WHERE d.kind = ?1 AND {d_rowid} > ?2 \
             ORDER BY {d_rowid} DESC LIMIT 1"
        )),
        &pv![kind_id, pre_max_rowid],
        |r| Ok((r.get(0)?, r.get(1)?)),
    )?;
    Ok((inserted, BackfillCursor { generation, rowid }))
}

/// True when at least one changeset still lacks its `derive_state` row for
/// the kind — the anti-join the completion marker is honest only without
/// ([`crate::derive::backfill`] sets the marker only when this returns
/// false, so the marker PROVES the anti-join empty rather than inferring
/// it from a zero-insert batch).
pub fn derive_backfill_rowless_exists(conn: &Db, kind_id: i64) -> Result<bool, DbError> {
    conn.query_row_map(
        &Sql::plain(
            "SELECT EXISTS(SELECT 1 FROM changesets c \
         WHERE NOT EXISTS (\
             SELECT 1 FROM derive_state d \
             WHERE d.kind = ?1 AND d.repo_id = c.repo_id AND d.cs = c.id))",
        ),
        &pv![kind_id],
        |r| r.get(0),
    )
}

/// Immediate quarantine for the structural caps (walk budget, output size):
/// state='poison' plus a `derive_reports` row, NO attempt arithmetic — the
/// caps are structural, so retrying cannot help (unlike
/// [`derive_fail_attempt`]). UPSERT: the eager path calls this before any
/// row exists; an existing pending/deriving row is quarantined in place.
/// Plain statements — see the module note on transactions.
pub fn derive_poison_now(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    cs: &Id,
    reason: &str,
    now_ns: i64,
) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "INSERT INTO derive_state(kind, repo_id, cs, state, attempts) \
         VALUES (?1, ?2, ?3, 'poison', 0) \
         ON CONFLICT(kind, repo_id, cs) DO UPDATE SET \
           state = 'poison', lease_expires_ns = NULL",
        ),
        &pv![kind_id, repo_id, cs.0.as_slice()],
    )?;
    conn.execute(&Sql::plain("INSERT INTO derive_reports(kind, repo_id, cs, reason, ts_ns) VALUES (?1, ?2, ?3, ?4, ?5)"),
        &pv![kind_id, repo_id, cs.0.as_slice(), reason, now_ns],
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
    conn: &Db,
    repo_id: i64,
    cs: &Id,
    entries: &[BlameStateEntry],
) -> Result<(), DbError> {
    for e in entries {
        conn.execute(
            &Sql::replace(
                "INSERT OR REPLACE INTO blame_state(repo_id, cs, path, blob_id, parent_blob_id) \
                 VALUES (?1, ?2, ?3, ?4, ?5)",
                &["repo_id", "cs", "path"],
                &["blob_id", "parent_blob_id"],
            ),
            &pv![
                repo_id,
                cs.0.as_slice(),
                &e.path,
                e.blob_id.as_ref().map(|b| b.0.as_slice()),
                e.parent_blob_id.as_ref().map(|b| b.0.as_slice()),
            ],
        )?;
    }
    Ok(())
}

/// One path's blame diff-state row: `(blob_id, parent_blob_id)`, both
/// optional (del / add).
pub struct BlameDiffState {
    pub blob_id: Option<Id>,
    pub parent_blob_id: Option<Id>,
}

/// One path's blame diff-state row for a changeset:
/// `(blob_id, parent_blob_id)`, both optional (del / add), or `None` when
/// the changeset did not touch the path. The blame walk's per-node
/// question — indexed point query, one per visited chain node.
pub fn blame_state_get(
    conn: &Db,
    repo_id: i64,
    cs: &Id,
    path: &str,
) -> Result<Option<BlameDiffState>, DbError> {
    conn.query_opt_map(
        &Sql::plain(
            "SELECT blob_id, parent_blob_id FROM blame_state \
         WHERE repo_id = ?1 AND cs = ?2 AND path = ?3",
        ),
        &pv![repo_id, cs.0.as_slice(), path],
        |row| {
            let blob: Option<Vec<u8>> = row.get(0)?;
            let parent: Option<Vec<u8>> = row.get(1)?;
            Ok(BlameDiffState {
                blob_id: blob.map(|b| Id(b.try_into().expect("blob id is 32 bytes"))),
                parent_blob_id: parent
                    .map(|b| Id(b.try_into().expect("parent blob id is 32 bytes"))),
            })
        },
    )
}

/// Writes one changeset's path-history index rows (an `OR IGNORE` per
/// path: the PK (repo_id, path, gen, cs) makes a re-run a no-op).
pub fn path_history_put(
    conn: &Db,
    repo_id: i64,
    generation: i64,
    cs: &Id,
    changes: &[FileChangeRow],
) -> Result<(), DbError> {
    for c in changes {
        conn.execute(
            &Sql::ignore(
                "INSERT OR IGNORE INTO path_history(repo_id, path, gen, cs, op) \
                 VALUES (?1, ?2, ?3, ?4, ?5)",
            ),
            &pv![repo_id, &c.path, generation, cs.0.as_slice(), c.op],
        )?;
    }
    Ok(())
}

/// One page of the derived path history for one path, newest first
/// (generation DESC, changeset id ASC), resuming after a cursor row.
/// Joins `changesets` so the page carries the log-entry fields. Actively
/// tombstoned changesets drop out at mark time (the universal gate — same
/// rule as `/log`: the derived row persists until the purge deletes it, the
/// read never serves it; the poisoned queue row the mark leaves behind also
/// flags the page degraded through the route's poison probe).
pub fn path_history_page(
    conn: &Db,
    repo_id: i64,
    path: &str,
    after: Option<(i64, Vec<u8>)>,
    limit: i64,
) -> Result<Vec<LogRow>, DbError> {
    let (after_gen, after_cs) = match after {
        Some((g, id)) => (Some(g), id),
        None => (None, Vec::new()),
    };
    conn.query_all_map(
        &Sql::plain(
            "SELECT c.id, c.author, c.message, c.timestamp_ns, c.generation, c.principal \
         FROM path_history h \
         JOIN changesets c ON c.repo_id = h.repo_id AND c.id = h.cs \
         WHERE h.repo_id = ?1 AND h.path = ?2 \
           AND (?3 IS NULL OR h.gen < ?3 OR (h.gen = ?3 AND h.cs > ?4)) \
           AND NOT EXISTS (SELECT 1 FROM redactions r \
                           WHERE r.repo_id = h.repo_id AND r.object_id = h.cs \
                             AND r.kind = 'changeset' AND r.purged_ns IS NULL) \
         ORDER BY h.gen DESC, h.cs ASC LIMIT ?5",
        ),
        &pv![repo_id, path, after_gen, after_cs, limit],
        log_row_from,
    )
}

/// The un-accelerated path history: a full `file_changes` scan by path —
/// the same rows the derived index holds, computed on the fly. This is the
/// fallback for a server on which the path_history kind is not registered,
/// and the parity oracle's brute force. Tombstoned changesets drop out with
/// the same anti-join as the derived page (both surfaces gate identically).
pub fn path_history_scan(
    conn: &Db,
    repo_id: i64,
    path: &str,
    after: Option<(i64, Vec<u8>)>,
    limit: i64,
) -> Result<Vec<LogRow>, DbError> {
    let (after_gen, after_cs) = match after {
        Some((g, id)) => (Some(g), id),
        None => (None, Vec::new()),
    };
    conn.query_all_map(
        &Sql::plain(
            "SELECT c.id, c.author, c.message, c.timestamp_ns, c.generation, c.principal \
         FROM file_changes f \
         JOIN changesets c ON c.repo_id = f.repo_id AND c.id = f.cs \
         WHERE f.repo_id = ?1 AND f.path = ?2 \
           AND (?3 IS NULL OR c.generation < ?3 OR (c.generation = ?3 AND c.id > ?4)) \
           AND NOT EXISTS (SELECT 1 FROM redactions r \
                           WHERE r.repo_id = f.repo_id AND r.object_id = f.cs \
                             AND r.kind = 'changeset' AND r.purged_ns IS NULL) \
         ORDER BY c.generation DESC, c.id ASC LIMIT ?5",
        ),
        &pv![repo_id, path, after_gen, after_cs, limit],
        log_row_from,
    )
}

fn log_row_from(row: &Row) -> Result<LogRow, DbError> {
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
pub fn derive_any_pending(conn: &Db, kind_id: i64, repo_id: i64) -> Result<bool, DbError> {
    let kind_name: Option<String> = conn.query_opt_map(
        &Sql::plain(DERIVE_PENDING_KIND_NAME_SQL),
        &pv![kind_id],
        |r| r.get(0),
    )?;
    let Some(kind_name) = kind_name else {
        // Unreachable from the routes (they resolve the id via
        // `derive_kind_id` first); unknown kind id stays on the safe side.
        return Ok(true);
    };
    if !derive_backfill_done_get(conn, &kind_name)? {
        return Ok(true);
    }
    let hit: Option<i64> = conn.query_opt_map(
        &Sql::plain(DERIVE_PENDING_PROBE_SQL),
        &pv![kind_id, repo_id],
        |r| r.get(0),
    )?;
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
    conn: &Db,
    kind_name: &str,
    completed_ns: i64,
) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "INSERT INTO meta(key, value) VALUES (?1, ?2) \
         ON CONFLICT(key) DO UPDATE SET value = excluded.value",
        ),
        &pv![
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
pub fn derive_backfill_done_clear(conn: &Db, kind_name: &str) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain("DELETE FROM meta WHERE key = ?1"),
        &pv![derive_backfill_marker_key(kind_name)],
    )?;
    Ok(())
}

/// Whether kind `name`'s boot backfill completion marker is set.
pub fn derive_backfill_done_get(conn: &Db, kind_name: &str) -> Result<bool, DbError> {
    let hit: Option<String> = conn.query_opt_map(
        &Sql::plain(DERIVE_PENDING_BACKFILL_DONE_SQL),
        &pv![derive_backfill_marker_key(kind_name)],
        |r| r.get(0),
    )?;
    Ok(hit.is_some())
}

/// The kind's quarantined nodes in the repo whose PRIMARY file_changes
/// touch `path` — the precise degraded signal for a `?path=` page: the
/// index may be missing exactly those nodes' touches, and no others. (The
/// primary rows exist regardless of derivation, so this is checkable
/// exactly.)
pub fn derive_poison_touching_path(
    conn: &Db,
    kind_id: i64,
    repo_id: i64,
    path: &str,
) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT d.cs FROM derive_state d \
         WHERE d.kind = ?1 AND d.repo_id = ?2 AND d.state = 'poison' \
           AND EXISTS (SELECT 1 FROM file_changes f \
                       WHERE f.repo_id = d.repo_id AND f.cs = d.cs AND f.path = ?3)",
        ),
        &pv![kind_id, repo_id, path],
        |row| {
            row.get::<Vec<u8>>(0)
                .map(|b| Id(b.try_into().expect("cs id is 32 bytes")))
        },
    )
}

/// One `(tree_id, name)` entry probe — the indexed point lookup under both
/// path lookups. `tree_entries`' `PRIMARY KEY(tree_id, name)` makes this an
/// O(1) B-tree descent per component, independent of the directory's entry
/// count.
fn tree_entry_lookup(conn: &Db, tree: &Id, name: &str) -> Result<Option<(i64, Vec<u8>)>, DbError> {
    conn.query_opt_map(
        &Sql::plain("SELECT kind, object_id FROM tree_entries WHERE tree_id = ?1 AND name = ?2"),
        &pv![tree.0.as_slice(), name],
        |r| Ok((r.get::<i64>(0)?, r.get::<Vec<u8>>(1)?)),
    )
}

/// Resolves one path in a tree to its blob id (`None` = absent, or the
/// path crosses a file, or names a directory). The blame route's
/// existence pre-check. One [`tree_entry_lookup`] probe per path component
/// — O(depth), independent of directory width. DERIVERS must not use this
/// variant: it never charges — see [`tree_lookup_path_charged`].
pub fn tree_lookup_path(conn: &Db, root: &Id, path: &str) -> Result<Option<Id>, DbError> {
    let mut tree = *root;
    let components: Vec<&str> = path.split('/').collect();
    for (i, name) in components.iter().enumerate() {
        let Some((kind, object_id)) = tree_entry_lookup(conn, &tree, name)? else {
            return Ok(None);
        };
        let object = Id(object_id.try_into().expect("object id is 32 bytes"));
        let last = i == components.len() - 1;
        match (kind, last) {
            (0, true) => return Ok(Some(object)),
            (1, false) => tree = object,
            _ => return Ok(None),
        }
    }
    Ok(None)
}

/// [`tree_lookup_path`] priced through the deriver's walk budget: one
/// charge per COMPONENT (an indexed [`tree_entry_lookup`] point probe on
/// `tree_entries`' PRIMARY KEY) — the walk's cost is O(depth) probes,
/// independent of directory width, which is exactly what the charge
/// prices. SQL failures are a [`DeriveFailure::Fail`] (a failed attempt);
/// an exhausted budget or a missed
/// [`crate::derive::DERIVE_WALK_DEADLINE_NS`] wall-clock deadline is
/// [`DeriveFailure::WalkBudget`] (immediate quarantine). Derivers walking
/// trees MUST use this variant.
pub fn tree_lookup_path_charged(
    conn: &Db,
    ctx: &crate::derive::DeriveCtx,
    root: &Id,
    path: &str,
) -> Result<Option<Id>, crate::derive::DeriveFailure> {
    let mut tree = *root;
    let components: Vec<&str> = path.split('/').collect();
    for (i, name) in components.iter().enumerate() {
        ctx.charge(1)?;
        let Some((kind, object_id)) = tree_entry_lookup(conn, &tree, name).map_err(|e| {
            crate::derive::DeriveFailure::Fail(crate::derive::PoisonReason::new(e.to_string()))
        })?
        else {
            return Ok(None);
        };
        let object = Id(object_id.try_into().expect("object id is 32 bytes"));
        let last = i == components.len() - 1;
        match (kind, last) {
            (0, true) => return Ok(Some(object)),
            (1, false) => tree = object,
            _ => return Ok(None),
        }
    }
    Ok(None)
}

/// A changeset's parent count (the commit-graph deriver's purity check: a
/// skip over a span may not lose a spanned node's non-first parents).
pub fn changeset_parent_count(conn: &Db, repo_id: i64, cs: &Id) -> Result<i64, DbError> {
    conn.query_row_map(
        &Sql::plain("SELECT COUNT(*) FROM changeset_parents WHERE repo_id = ?1 AND cs = ?2"),
        &pv![repo_id, cs.0.as_slice()],
        |r| r.get(0),
    )
}

// ---- reachability GC (M10a) -----------------------------------------------
//
// Statement layer for the mark-and-sweep in [`crate::gc`]. The mark reads
// PRIMARY TABLES ONLY — refs, changesets + parents + file_changes, unexpired
// snapshots, lands' head/base/result references, git_map — never the
// `repo_blobs` membership index or the M6 derived tables (advisory, never
// authoritative). The sweep works over `gc_pending` (the first-unreachable
// ledger): a candidate collects only past the grace window AND only while
// the re-verification inside the collecting transaction still finds it
// unreachable, so a concurrent write that references a candidate again
// spares it and a crash mid-sweep converges on the re-run (set-difference,
// not a destructive cursor).

/// `gc_pending.scope` value: an unreachable changeset.
pub const GC_SCOPE_CS: &str = "cs";
/// `gc_pending.scope` value: an unreachable CAS blob.
pub const GC_SCOPE_BLOB: &str = "blob";
/// `gc_pending.scope` value: an unreachable `trees` row.
pub const GC_SCOPE_TREE: &str = "tree";

/// IN-list chunk size for the id-set statements. Safely under both
/// backends' parameter ceilings and small enough that each statement's
/// plan stays a set of indexed probes.
pub(crate) const GC_IN_CHUNK: usize = 400;

fn id_params(ids: &[Id]) -> Vec<P> {
    ids.iter().map(|id| P::B(id.0.to_vec())).collect()
}

/// Every repo, ascending by id (the mark walks repo by repo; every per-repo
/// statement keeps its indexes' leading `repo_id` column constrained).
pub fn gc_repos(conn: &Db) -> Result<Vec<i64>, DbError> {
    conn.query_all_map(
        &Sql::plain("SELECT id FROM repos ORDER BY id ASC"),
        &pv![],
        |r| r.get(0),
    )
}

/// The repo's seed changesets: every ref target, every lands head/base,
/// every git_map cs_id. (The lands `result_json` "landed" id is parsed by
/// the caller — [`gc_land_result_jsons`] — JSON is not statement surface.)
pub fn gc_seed_changesets(conn: &Db, repo_id: i64) -> Result<Vec<Id>, DbError> {
    let mut out = Vec::new();
    let mut push = |raw: Option<Vec<u8>>| {
        if let Some(b) = raw
            && let Ok(arr) = <[u8; 32]>::try_from(b)
        {
            out.push(Id(arr));
        }
        Ok::<(), DbError>(())
    };
    let rows: Vec<Option<Vec<u8>>> = conn.query_all_map(
        &Sql::plain("SELECT target FROM refs WHERE repo_id = ?1 AND target IS NOT NULL"),
        &pv![repo_id],
        |r| r.get(0),
    )?;
    for raw in rows {
        push(raw)?;
    }
    let rows: Vec<Option<Vec<u8>>> = conn.query_all_map(
        &Sql::plain(
            "SELECT head FROM lands WHERE repo_id = ?1 AND head IS NOT NULL \
             UNION ALL SELECT base FROM lands WHERE repo_id = ?1 AND base IS NOT NULL",
        ),
        &pv![repo_id],
        |r| r.get(0),
    )?;
    for raw in rows {
        push(raw)?;
    }
    let rows: Vec<Vec<u8>> = conn.query_all_map(
        &Sql::plain("SELECT cs_id FROM git_map WHERE repo_id = ?1"),
        &pv![repo_id],
        |r| r.get(0),
    )?;
    for raw in rows {
        push(Some(raw))?;
    }
    Ok(out)
}

/// The repo's lands `result_json` payloads (the settled `{landed, …}`
/// records); the caller parses the landed changeset ids out of them.
pub fn gc_land_result_jsons(conn: &Db, repo_id: i64) -> Result<Vec<String>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT result_json FROM lands \
             WHERE repo_id = ?1 AND result_json IS NOT NULL",
        ),
        &pv![repo_id],
        |r| r.get(0),
    )
}

/// The parents of `cs_ids` in `repo_id` (one BFS level, chunked IN-lists on
/// the `(repo_id, cs, …)` PRIMARY KEY prefix).
pub fn gc_parent_ids(conn: &Db, repo_id: i64, cs_ids: &[Id]) -> Result<Vec<Id>, DbError> {
    let mut out = Vec::new();
    for chunk in cs_ids.chunks(GC_IN_CHUNK) {
        // `?1` is the repo; the IN-list numbers from ?2.
        let list = (2..=chunk.len() + 1)
            .map(|i| format!("?{i}"))
            .collect::<Vec<_>>()
            .join(", ");
        let mut params = vec![P::I(repo_id)];
        params.extend(id_params(chunk));
        let rows: Vec<Vec<u8>> = conn.query_all_map(
            &Sql::plain_owned(format!(
                "SELECT parent FROM changeset_parents \
                 WHERE repo_id = ?1 AND cs IN ({list})"
            )),
            &params,
            |r| r.get(0),
        )?;
        for raw in rows {
            if let Ok(arr) = <[u8; 32]>::try_from(raw) {
                out.push(Id(arr));
            }
        }
    }
    Ok(out)
}

/// The repo's whole changeset inventory: `(id, root_tree_id)` — the mark's
/// candidate side is the inventory minus the reachable set, and the roots
/// of the reachable set seed the protected-tree closure.
pub fn gc_all_changesets(conn: &Db, repo_id: i64) -> Result<Vec<(Id, Option<Id>)>, DbError> {
    conn.query_all_map(
        &Sql::plain("SELECT id, root_tree_id FROM changesets WHERE repo_id = ?1"),
        &pv![repo_id],
        |r| {
            let id: Vec<u8> = r.get(0)?;
            let root: Option<Vec<u8>> = r.get(1)?;
            let id = Id(<[u8; 32]>::try_from(id).map_err(|_| {
                DbError::from(std::io::Error::other("changesets.id is not 32 bytes"))
            })?);
            let root = root
                .map(|b| <[u8; 32]>::try_from(b).map(Id))
                .transpose()
                .map_err(|_| {
                    DbError::from(std::io::Error::other(
                        "changesets.root_tree_id is not 32 bytes",
                    ))
                })?;
            Ok((id, root))
        },
    )
}

/// The repo's `file_changes` blob references: `(cs, blob_id)` rows — the
/// mark filters this inventory by the reachable set in memory (one full
/// read of the table per pass is the mark's honest cost, per the plan
/// clause: the mark reads `file_changes`).
pub fn gc_file_change_blobs(conn: &Db, repo_id: i64) -> Result<Vec<(Id, Id)>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT cs, blob_id FROM file_changes \
             WHERE repo_id = ?1 AND blob_id IS NOT NULL",
        ),
        &pv![repo_id],
        |r| {
            let cs: Vec<u8> = r.get(0)?;
            let blob: Vec<u8> = r.get(1)?;
            Ok((
                Id(<[u8; 32]>::try_from(cs).map_err(|_| {
                    DbError::from(std::io::Error::other("file_changes.cs is not 32 bytes"))
                })?),
                Id(<[u8; 32]>::try_from(blob).map_err(|_| {
                    DbError::from(std::io::Error::other(
                        "file_changes.blob_id is not 32 bytes",
                    ))
                })?),
            ))
        },
    )
}

/// The child entries of `tree_ids`: `(kind, object_id)` per entry (one BFS
/// level of the tree closure, chunked IN-lists on the `(tree_id, name)`
/// PRIMARY KEY prefix).
pub fn gc_tree_children(conn: &Db, tree_ids: &[Id]) -> Result<Vec<(i64, Id)>, DbError> {
    let mut out = Vec::new();
    for chunk in tree_ids.chunks(GC_IN_CHUNK) {
        let list = (1..=chunk.len())
            .map(|i| format!("?{i}"))
            .collect::<Vec<_>>()
            .join(", ");
        let rows: Vec<(i64, Vec<u8>)> = conn.query_all_map(
            &Sql::plain_owned(format!(
                "SELECT kind, object_id FROM tree_entries WHERE tree_id IN ({list})"
            )),
            &id_params(chunk),
            |r| Ok((r.get(0)?, r.get(1)?)),
        )?;
        for (kind, raw) in rows {
            if let Ok(arr) = <[u8; 32]>::try_from(raw) {
                out.push((kind, Id(arr)));
            }
        }
    }
    Ok(out)
}

/// The repo's unexpired snapshot roots (`expires_ns > now_ns`).
pub fn gc_snapshot_roots(conn: &Db, repo_id: i64, now_ns: i64) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain("SELECT tree_id FROM snapshots WHERE repo_id = ?1 AND expires_ns > ?2"),
        &pv![repo_id, now_ns],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other("snapshots.tree_id is not 32 bytes"))
            })?))
        },
    )
}

/// Every `objects` row (the DB-known blob inventory).
pub fn gc_all_object_ids(conn: &Db) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(&Sql::plain("SELECT object_id FROM objects"), &pv![], |r| {
        let b: Vec<u8> = r.get(0)?;
        Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
            DbError::from(std::io::Error::other("objects.object_id is not 32 bytes"))
        })?))
    })
}

/// Every `trees` row id (the tree inventory the sweep differs against).
pub fn gc_all_tree_ids(conn: &Db) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(&Sql::plain("SELECT id FROM trees"), &pv![], |r| {
        let b: Vec<u8> = r.get(0)?;
        Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
            DbError::from(std::io::Error::other("trees.id is not 32 bytes"))
        })?))
    })
}

// -- gc_pending ledger -------------------------------------------------------

/// Records `ids` as first-observed-unreachable at `first_ns`. `INSERT OR
/// IGNORE` keeps the FIRST timestamp: a candidate that stays unreachable
/// ages from when it actually became garbage, not from when a pass last
/// looked. Returns the rows actually added.
pub fn gc_candidates_upsert(
    conn: &Db,
    scope: &str,
    ids: &[Id],
    first_ns: i64,
) -> Result<usize, DbError> {
    let mut added = 0;
    for chunk in ids.chunks(GC_IN_CHUNK) {
        for id in chunk {
            added += conn.execute(
                &Sql::ignore(
                    "INSERT OR IGNORE INTO gc_pending(scope, id, first_ns) VALUES (?1, ?2, ?3)",
                ),
                &pv![scope, id.0.as_slice(), first_ns],
            )?;
        }
    }
    Ok(added)
}

/// Deletes the ledger rows for `ids` (the repair direction: reachable
/// again, or collected).
pub fn gc_candidates_delete(conn: &Db, scope: &str, ids: &[Id]) -> Result<usize, DbError> {
    let mut n = 0;
    for chunk in ids.chunks(GC_IN_CHUNK) {
        // `?1` is the scope; the IN-list numbers from ?2.
        let list = (2..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let mut params = vec![P::S(scope.to_string())];
        params.extend(id_params(chunk));
        n += conn.execute(
            &Sql::plain_owned(format!(
                "DELETE FROM gc_pending WHERE scope = ?1 AND id IN ({list})"
            )),
            &params,
        )?;
    }
    Ok(n)
}

/// Counts the ledger rows (fsck's pending total), optionally split by
/// whether they are already past `cut_ns`.
pub fn gc_candidate_counts(conn: &Db, cut_ns: i64) -> Result<(i64, i64), DbError> {
    let pending: i64 = conn.query_row_map(
        &Sql::plain("SELECT COUNT(*) FROM gc_pending"),
        &pv![],
        |r| r.get(0),
    )?;
    let past: i64 = conn.query_row_map(
        &Sql::plain("SELECT COUNT(*) FROM gc_pending WHERE first_ns <= ?1"),
        &pv![cut_ns],
        |r| r.get(0),
    )?;
    Ok((pending, past))
}

/// Every ledger row id for `scope` (fsck's stale-candidate check).
pub fn gc_pending_ids(conn: &Db, scope: &str) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain("SELECT id FROM gc_pending WHERE scope = ?1"),
        &pv![scope],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other("gc_pending.id is not 32 bytes"))
            })?))
        },
    )
}

// -- collectible selection (re-verified inside the collecting transaction) ----

/// The changeset candidates past the grace cut that are STILL unreferenced
/// at read time: no ref target, no lands head/base, no git_map row, no
/// changeset child — each probe an indexed point lookup (migration 9's
/// `idx_refs_target`, `idx_lands_head`, `idx_lands_base`, `idx_git_map_cs`,
/// and the repo-less `idx_cs_parents_parent_id` — the probe is deliberately
/// repo-scoped-off: `gc_pending` carries no repo_id, a collected changeset
/// is gone from every repo). Actively-tombstoned changesets are EXCLUDED:
/// they are the redaction purge phase's to collect, the only path that
/// stamps `redactions.purged_ns` and rows the purge audit — the normal sweep
/// must never orphan a tombstone's completion mark. The JSON `landed` side
/// is the caller's in-memory set; a landed id is main's tip and thereby also
/// under a ref, but the caller filters it explicitly to honor the plan
/// clause verbatim. Generation-ordered like the batched backfill (children
/// collect after parents' chains, deterministic across crash re-runs). Runs
/// inside the collecting transaction — selection and deletion are one atomic
/// step.
pub fn gc_collectible_cs(conn: &Db, cut_ns: i64, limit: usize) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT p.id FROM gc_pending p \
             JOIN changesets c ON c.id = p.id \
             WHERE p.scope = 'cs' AND p.first_ns <= ?1 \
               AND NOT EXISTS (SELECT 1 FROM refs r WHERE r.target = p.id) \
               AND NOT EXISTS (SELECT 1 FROM lands l WHERE l.head = p.id OR l.base = p.id) \
               AND NOT EXISTS (SELECT 1 FROM git_map g WHERE g.cs_id = p.id) \
               AND NOT EXISTS (SELECT 1 FROM changeset_parents par WHERE par.parent = p.id) \
               AND NOT EXISTS (SELECT 1 FROM redactions r \
                               WHERE r.object_id = p.id AND r.kind = 'changeset' \
                                 AND r.purged_ns IS NULL) \
             ORDER BY c.generation ASC, p.id ASC LIMIT ?2",
        ),
        &pv![cut_ns, limit as i64],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other("gc_pending.id is not 32 bytes"))
            })?))
        },
    )
}

/// Phase A's selector: the blob candidates past the grace cut that are
/// STILL unreferenced at read time AND not yet condemned (`condemned_at IS
/// NULL` — a stamped row belongs to phase B; a touch cleared the stamp, so
/// a re-PUT's blob re-enters here honestly): no `file_changes` row and no
/// blob `tree_entries` row — both probes indexed (migration 8's
/// `idx_file_changes_blob`, migration 3's `idx_tree_entries_object`) — and
/// no upload touch inside the grace window (`objects.touched_ns`: a
/// content-addressed re-PUT of an old orphan is a CAS no-op, and the row
/// touch is what re-enters it into grace, so a re-referenced blob can
/// never collect under a concurrent pass). Actively-tombstoned blobs are
/// EXCLUDED (any repo's tombstone): the redaction purge phase owns them —
/// only it stamps `redactions.purged_ns` and rows the purge audit. Any live
/// reference spares the blob for this transaction; a tree row of a tree
/// that is itself dying spares it too and the blob follows one phase later
/// (never dangling, always converging).
pub fn gc_collectible_blobs(conn: &Db, cut_ns: i64, limit: usize) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT p.id FROM gc_pending p \
             WHERE p.scope = 'blob' AND p.first_ns <= ?1 \
               AND p.condemned_at IS NULL \
               AND NOT EXISTS (SELECT 1 FROM objects o \
                               WHERE o.object_id = p.id AND o.touched_ns > ?1) \
               AND NOT EXISTS (SELECT 1 FROM file_changes f WHERE f.blob_id = p.id) \
               AND NOT EXISTS (SELECT 1 FROM tree_entries te \
                               WHERE te.kind = 0 AND te.object_id = p.id) \
               AND NOT EXISTS (SELECT 1 FROM redactions r \
                               WHERE r.object_id = p.id AND r.kind = 'blob' \
                                 AND r.purged_ns IS NULL) \
             ORDER BY p.first_ns ASC, p.id ASC LIMIT ?2",
        ),
        &pv![cut_ns, limit as i64],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other("gc_pending.id is not 32 bytes"))
            })?))
        },
    )
}

/// The tree candidates past the grace cut that the CURRENT protective
/// closure still does not reach: the recursive `live` set is computed from
/// every surviving changeset root plus every unexpired snapshot, inside the
/// collecting transaction — a land or snapshot POST that commits a
/// reference mid-pass is already committed here (the single-writer slot
/// serializes the two) and spares its shared subtrees.
pub fn gc_collectible_trees(
    conn: &Db,
    now_ns: i64,
    cut_ns: i64,
    limit: usize,
) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "WITH RECURSIVE live(tree_id) AS ( \
                 SELECT root_tree_id FROM changesets WHERE root_tree_id IS NOT NULL \
                 UNION \
                 SELECT tree_id FROM snapshots WHERE expires_ns > ?1 \
                 UNION \
                 SELECT te.object_id FROM live l \
                 JOIN tree_entries te ON te.tree_id = l.tree_id AND te.kind = 1 \
             ) \
             SELECT p.id FROM gc_pending p \
             WHERE p.scope = 'tree' AND p.first_ns <= ?2 \
               AND NOT EXISTS (SELECT 1 FROM live l WHERE l.tree_id = p.id) \
             ORDER BY p.first_ns ASC, p.id ASC LIMIT ?3",
        ),
        &pv![now_ns, cut_ns, limit as i64],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other("gc_pending.id is not 32 bytes"))
            })?))
        },
    )
}

/// The two-phase delete's PHASE A: stamps `condemned_at` on the past-grace,
/// unreferenced blob candidates selected by [`gc_collectible_blobs`] — the
/// bytes are NOT touched this pass. A later pass's phase B
/// ([`gc_condemned_blobs`]) deletes them only while the mark is still set;
/// any PUT's row touch ([`object_row_upsert`]) clears it first. Returns
/// the rows condemned.
pub fn gc_condemn_blobs(conn: &Db, ids: &[Id], condemned_at: i64) -> Result<usize, DbError> {
    let mut n = 0;
    for chunk in ids.chunks(GC_IN_CHUNK) {
        let list = (2..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let mut params = vec![P::I(condemned_at)];
        params.extend(id_params(chunk));
        n += conn.execute(
            &Sql::plain_owned(format!(
                "UPDATE gc_pending SET condemned_at = ?1 \
                 WHERE scope = 'blob' AND id IN ({list})"
            )),
            &params,
        )?;
    }
    Ok(n)
}

/// The two-phase delete's PHASE B selector: blob candidates condemned by an
/// EARLIER pass (`condemned_at < now_ns`, so this pass's own phase-A
/// stamps never qualify), still unreferenced at read time, and still
/// condemned — the mark is the proof no upload touched the row since the
/// stamp. Actively-tombstoned blobs are EXCLUDED (the redaction purge phase
/// owns them, as in [`gc_collectible_blobs`]). Runs inside the deleting
/// transaction (the slot serializes it against the record step, whose touch
/// clears the mark in the same slot).
pub fn gc_condemned_blobs(
    conn: &Db,
    now_ns: i64,
    cut_ns: i64,
    limit: usize,
) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT p.id FROM gc_pending p \
             WHERE p.scope = 'blob' AND p.first_ns <= ?1 \
               AND p.condemned_at IS NOT NULL AND p.condemned_at < ?2 \
               AND NOT EXISTS (SELECT 1 FROM file_changes f WHERE f.blob_id = p.id) \
               AND NOT EXISTS (SELECT 1 FROM tree_entries te \
                               WHERE te.kind = 0 AND te.object_id = p.id) \
               AND NOT EXISTS (SELECT 1 FROM redactions r \
                               WHERE r.object_id = p.id AND r.kind = 'blob' \
                                 AND r.purged_ns IS NULL) \
             ORDER BY p.first_ns ASC, p.id ASC LIMIT ?3",
        ),
        &pv![cut_ns, now_ns, limit as i64],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other("gc_pending.id is not 32 bytes"))
            })?))
        },
    )
}

// -- collection (the deleting half of a batch) --------------------------------

/// Collects `ids` (changesets): the changeset row, its parents and file
/// changes, and every advisory row keyed by it — the M6 queue/reports and
/// the blame/path-history stores — plus the ledger row. The advisory
/// deletes are the "M6 indexes are advisory" clause taken to its sweep
/// consequence: rows keyed by a collected changeset would dangle forever.
/// Returns the changesets collected.
pub fn gc_collect_cs(conn: &Db, ids: &[Id]) -> Result<usize, DbError> {
    let mut n = 0;
    for chunk in ids.chunks(GC_IN_CHUNK) {
        let list = (1..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let params = id_params(chunk);
        for table in [
            "changesets",
            "changeset_parents",
            "file_changes",
            "derive_state",
            "derive_reports",
            "blame_state",
            "path_history",
        ] {
            let key = match table {
                "changesets" => "id",
                _ => "cs",
            };
            conn.execute(
                &Sql::plain_owned(format!("DELETE FROM {table} WHERE {key} IN ({list})")),
                &params,
            )?;
        }
        n += conn.execute(
            &Sql::plain_owned(format!(
                "DELETE FROM gc_pending WHERE scope = 'cs' AND id IN ({list})"
            )),
            &params,
        )?;
    }
    Ok(n)
}

/// Collects `ids` (blobs): the `objects` row, the advisory `repo_blobs`
/// membership rows, and the ledger row. The CAS bytes were already unlinked
/// by the caller (bytes first — the orphan-direction rule: a crash leaves
/// DB rows claiming absent bytes for UNREACHABLE data, which the re-run
/// converges on, never reachable data missing bytes).
pub fn gc_collect_blobs(conn: &Db, ids: &[Id]) -> Result<(), DbError> {
    for chunk in ids.chunks(GC_IN_CHUNK) {
        let list = (1..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let params = id_params(chunk);
        for sql in [
            format!("DELETE FROM objects WHERE object_id IN ({list})"),
            format!("DELETE FROM repo_blobs WHERE object_id IN ({list})"),
            format!("DELETE FROM gc_pending WHERE scope = 'blob' AND id IN ({list})"),
        ] {
            conn.execute(&Sql::plain_owned(sql), &params)?;
        }
    }
    Ok(())
}

/// Collects `ids` (trees): the `trees` row, its entries, the `repo_trees`
/// junction rows (the tree is unreachable from every surviving changeset,
/// so every junction row is stale — `repo_has_tree` must never admit a
/// tree whose bytes are gone), and the ledger row. Returns the trees
/// collected.
pub fn gc_collect_trees(conn: &Db, ids: &[Id]) -> Result<usize, DbError> {
    let mut n = 0;
    for chunk in ids.chunks(GC_IN_CHUNK) {
        let list = (1..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let params = id_params(chunk);
        for sql in [
            format!("DELETE FROM tree_entries WHERE tree_id IN ({list})"),
            format!("DELETE FROM repo_trees WHERE tree_id IN ({list})"),
            format!("DELETE FROM trees WHERE id IN ({list})"),
            format!("DELETE FROM gc_pending WHERE scope = 'tree' AND id IN ({list})"),
        ] {
            conn.execute(&Sql::plain_owned(sql), &params)?;
        }
        n += chunk.len();
    }
    Ok(n)
}

// ---- fsck (M10a) -----------------------------------------------------------
//
// The consistency checker's statement layer: every check is one read-only
// anti-join over the primary tables (dangling-reference counts), plus the
// inventory loads the caller differences against the CAS and the ledger.

/// The dangling-reference checks, each as `(name, count)`. A nonzero count
/// anywhere is fsck dirt: a reference the primary tables carry but the
/// referenced row cannot be found for (broken graph, missing blob row,
/// missing tree row — the never-dangling invariant violated).
pub fn fsck_danglings(conn: &Db) -> Result<Vec<(&'static str, i64)>, DbError> {
    const CHECKS: &[(&str, &str)] = &[
        (
            "changeset_parent_missing",
            "SELECT COUNT(*) FROM changeset_parents p \
             WHERE NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = p.parent)",
        ),
        (
            "file_change_blob_row_missing",
            "SELECT COUNT(*) FROM file_changes f WHERE f.blob_id IS NOT NULL \
             AND NOT EXISTS (SELECT 1 FROM objects o WHERE o.object_id = f.blob_id)",
        ),
        (
            "tree_blob_row_missing",
            "SELECT COUNT(*) FROM tree_entries te WHERE te.kind = 0 \
             AND NOT EXISTS (SELECT 1 FROM objects o WHERE o.object_id = te.object_id)",
        ),
        (
            "tree_child_row_missing",
            "SELECT COUNT(*) FROM tree_entries te WHERE te.kind = 1 \
             AND NOT EXISTS (SELECT 1 FROM trees t WHERE t.id = te.object_id)",
        ),
        (
            "changeset_root_tree_missing",
            "SELECT COUNT(*) FROM changesets c WHERE c.root_tree_id IS NOT NULL \
             AND NOT EXISTS (SELECT 1 FROM trees t WHERE t.id = c.root_tree_id)",
        ),
        (
            "ref_target_missing",
            "SELECT COUNT(*) FROM refs r WHERE r.target IS NOT NULL \
             AND NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = r.target)",
        ),
        (
            "land_head_missing",
            "SELECT COUNT(*) FROM lands l WHERE l.head IS NOT NULL \
             AND NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = l.head)",
        ),
        (
            "land_base_missing",
            "SELECT COUNT(*) FROM lands l WHERE l.base IS NOT NULL \
             AND NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = l.base)",
        ),
        (
            "git_map_cs_missing",
            "SELECT COUNT(*) FROM git_map g \
             WHERE NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = g.cs_id)",
        ),
        (
            "membership_row_object_missing",
            "SELECT COUNT(*) FROM repo_blobs rb \
             WHERE NOT EXISTS (SELECT 1 FROM objects o WHERE o.object_id = rb.object_id)",
        ),
        (
            "derive_state_cs_missing",
            "SELECT COUNT(*) FROM derive_state d \
             WHERE NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = d.cs)",
        ),
        (
            "derive_report_cs_missing",
            "SELECT COUNT(*) FROM derive_reports d \
             WHERE NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = d.cs)",
        ),
        (
            "blame_state_cs_missing",
            "SELECT COUNT(*) FROM blame_state b \
             WHERE NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = b.cs)",
        ),
        (
            "path_history_cs_missing",
            "SELECT COUNT(*) FROM path_history h \
             WHERE NOT EXISTS (SELECT 1 FROM changesets c WHERE c.id = h.cs)",
        ),
    ];
    CHECKS
        .iter()
        .map(|(name, sql)| {
            let count: i64 = conn.query_row_map(&Sql::plain(sql), &pv![], |r| r.get(0))?;
            Ok((*name, count))
        })
        .collect()
}

/// One repo's `repo_blobs` membership rows (fsck's advisory comparison:
/// the index vs the blob references the repo's `file_changes` carry).
pub fn gc_repo_blobs(conn: &Db, repo_id: i64) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain("SELECT object_id FROM repo_blobs WHERE repo_id = ?1"),
        &pv![repo_id],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(b.try_into().expect("object id is 32 bytes")))
        },
    )
}

// ---- redactions (M10b) ------------------------------------------------------
//
// The tombstone table and its statement layer. A redaction marks one object
// (a CAS blob or a changeset) redacted IN ONE REPO; every serving path gates
// on [`redaction_active`] (one indexed PRIMARY KEY probe), and the GC's
// redaction phase ([`crate::gc`]) reclaims the bytes/rows and stamps
// `purged_ns`. Tombstone metadata records path + date only, never author:
// the `principal` column is the ACTING admin, and nothing in a row or its
// audit details names the redacted content's author.

/// One row of `redactions`.
pub struct RedactionRow {
    pub repo_id: i64,
    pub object_id: Id,
    /// `'blob'` or `'changeset'`.
    pub kind: String,
    /// The repo path the object sits at (its first, lexicographically),
    /// NULL when the object carries none. The tombstone's only descriptive
    /// metadata: path + date, never author — no reason, no actor (both
    /// live only in the audited `redact` action).
    pub path: Option<String>,
    pub marked_ns: i64,
    /// Set once the GC's redaction phase reclaimed the object; the row
    /// itself stays as the tombstone record.
    pub purged_ns: Option<i64>,
}

/// Marks `(repo_id, object_id)` redacted. `INSERT OR IGNORE` keeps the
/// EARLIEST `marked_ns` on a re-mark (the idempotent 200 re-POST
/// contract), and returns the row as it now stands plus whether THIS call
/// inserted it (the audit row — actor + reason — rides the 0→1 transition
/// only, the token_revoke convention). The tombstone's `path` is derived
/// here: the object's first file_changes path in this repo, NULL when it
/// carries none.
pub fn redaction_mark(
    conn: &Db,
    repo_id: i64,
    object_id: &Id,
    kind: &str,
    marked_ns: i64,
) -> Result<(RedactionRow, bool), DbError> {
    let path: Option<String> = if kind == "blob" {
        conn.query_opt_map(
            &Sql::plain(
                "SELECT path FROM file_changes \
                 WHERE repo_id = ?1 AND blob_id = ?2 ORDER BY path ASC LIMIT 1",
            ),
            &pv![repo_id, object_id.0.as_slice()],
            |r| r.get(0),
        )?
    } else {
        conn.query_opt_map(
            &Sql::plain(
                "SELECT path FROM file_changes \
                 WHERE repo_id = ?1 AND cs = ?2 ORDER BY path ASC LIMIT 1",
            ),
            &pv![repo_id, object_id.0.as_slice()],
            |r| r.get(0),
        )?
    };
    let inserted = conn.execute(
        &Sql::ignore(
            "INSERT OR IGNORE INTO redactions(repo_id, object_id, kind, path, marked_ns) \
             VALUES (?1, ?2, ?3, ?4, ?5)",
        ),
        &pv![repo_id, object_id.0.as_slice(), kind, path, marked_ns],
    )? == 1;
    let row = redaction_get(conn, repo_id, object_id)?.ok_or_else(|| {
        DbError::from(std::io::Error::other("redaction row vanished after insert"))
    })?;
    Ok((row, inserted))
}

/// The tombstone row for `(repo_id, object_id)`, or `None`.
pub fn redaction_get(
    conn: &Db,
    repo_id: i64,
    object_id: &Id,
) -> Result<Option<RedactionRow>, DbError> {
    conn.query_opt_map(
        &Sql::plain(
            "SELECT repo_id, object_id, kind, path, marked_ns, purged_ns \
         FROM redactions WHERE repo_id = ?1 AND object_id = ?2",
        ),
        &pv![repo_id, object_id.0.as_slice()],
        redaction_row_from,
    )
}

fn redaction_row_from(row: &Row) -> Result<RedactionRow, DbError> {
    Ok(RedactionRow {
        repo_id: row.get(0)?,
        object_id: Id(row.get::<Vec<u8>>(1)?.try_into().map_err(|_| {
            DbError::from(std::io::Error::other(
                "redactions.object_id is not 32 bytes",
            ))
        })?),
        kind: row.get(2)?,
        path: row.get(3)?,
        marked_ns: row.get(4)?,
        purged_ns: row.get(5)?,
    })
}

/// THE UNIVERSAL GATE probe: is `object_id` actively tombstoned in
/// `repo_id` (a redaction row with no `purged_ns`)? One indexed PRIMARY KEY
/// point lookup; every serving path in the plan's gate list calls this.
/// An id names exactly one object kind (blob and changeset ids live in
/// disjoint hash domains), so no kind filter is needed at the gate.
pub fn redaction_active(conn: &Db, repo_id: i64, object_id: &Id) -> Result<bool, DbError> {
    let hit: Option<i64> = conn.query_opt_map(
        &Sql::plain(
            "SELECT 1 FROM redactions \
         WHERE repo_id = ?1 AND object_id = ?2 AND purged_ns IS NULL",
        ),
        &pv![repo_id, object_id.0.as_slice()],
        |r| r.get(0),
    )?;
    Ok(hit.is_some())
}

/// The actively-tombstoned ids among `ids` (the batch gates: batch object
/// fetch, diff masking). Chunked IN-lists on the PK prefix.
pub fn redactions_active_among(conn: &Db, repo_id: i64, ids: &[Id]) -> Result<Vec<Id>, DbError> {
    let mut out = Vec::new();
    for chunk in ids.chunks(GC_IN_CHUNK) {
        let list = (2..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let mut params = vec![P::I(repo_id)];
        params.extend(id_params(chunk));
        let rows: Vec<Vec<u8>> = conn.query_all_map(
            &Sql::plain_owned(format!(
                "SELECT object_id FROM redactions \
             WHERE repo_id = ?1 AND purged_ns IS NULL AND object_id IN ({list})"
            )),
            &params,
            |r| r.get(0),
        )?;
        for raw in rows {
            if let Ok(arr) = <[u8; 32]>::try_from(raw) {
                out.push(Id(arr));
            }
        }
    }
    Ok(out)
}

/// The repo's actively-tombstoned CHANGESET ids (the merge-base walk's
/// exclusion set: a tombstoned node is never NAMED as the answer, while the
/// walk still traverses through it — the ancestors above it stay reachable).
pub fn redactions_active_changesets(conn: &Db, repo_id: i64) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT object_id FROM redactions \
         WHERE repo_id = ?1 AND kind = 'changeset' AND purged_ns IS NULL",
        ),
        &pv![repo_id],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other(
                    "redactions.object_id is not 32 bytes",
                ))
            })?))
        },
    )
}

/// Every ACTIVE tombstone across all repos: `(repo_id, object_id, kind)` —
/// the GC redaction phase's work list.
pub fn redactions_active_all(conn: &Db) -> Result<Vec<(i64, Id, String)>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT repo_id, object_id, kind FROM redactions \
         WHERE purged_ns IS NULL ORDER BY repo_id ASC, marked_ns ASC, object_id ASC",
        ),
        &pv![],
        |r| {
            let b: Vec<u8> = r.get(1)?;
            Ok((
                r.get::<i64>(0)?,
                Id(<[u8; 32]>::try_from(b).map_err(|_| {
                    DbError::from(std::io::Error::other(
                        "redactions.object_id is not 32 bytes",
                    ))
                })?),
                r.get(2)?,
            ))
        },
    )
}

/// One page of the repo's tombstones, ascending by object id (the PK order —
/// the same `after:`-cursor shape as the repos list), purged rows included.
pub fn redactions_page(
    conn: &Db,
    repo_id: i64,
    after: Option<&Id>,
    limit: i64,
) -> Result<Vec<RedactionRow>, DbError> {
    let after = after.map(|id| id.0.to_vec());
    conn.query_all_map(
        &Sql::plain(
            "SELECT repo_id, object_id, kind, path, marked_ns, purged_ns \
         FROM redactions \
         WHERE repo_id = ?1 AND (?2 IS NULL OR object_id > ?2) \
         ORDER BY object_id ASC LIMIT ?3",
        ),
        &pv![repo_id, after, limit],
        redaction_row_from,
    )
}

/// Stamps `purged_ns` on every ACTIVE tombstone of `kind` for `object_id`
/// (the redaction phase's completion mark; a blob tombstoned in several
/// repos purges once and stamps all its rows). Returns the rows stamped.
pub fn redaction_stamp_purged(
    conn: &Db,
    kind: &str,
    object_id: &Id,
    purged_ns: i64,
) -> Result<usize, DbError> {
    conn.execute(
        &Sql::plain(
            "UPDATE redactions SET purged_ns = ?3 \
         WHERE kind = ?1 AND object_id = ?2 AND purged_ns IS NULL",
        ),
        &pv![kind, object_id.0.as_slice(), purged_ns],
    )
}

/// Mark-time invalidation of the tombstoned changeset's derived rows: every
/// `derive_state` row for it goes to the POISON terminal state (queue,
/// lease, and reports included in spirit — the read side degrades off the
/// row and the queue never re-picks poison). The taint machinery then
/// carries the hole to re-derived descendants: a child deriving after the
/// mark consumes the poisoned parent's empty marker and records done-WITH-
/// taint naming the tombstoned node ([`derive_parent_outputs`]'s poison
/// arm — no redaction probe needed there). Runs in the mark's transaction;
/// a changeset with no derive rows yet (the backfill has not reached it)
/// matches zero rows, so the read-side gate probes redactions per node too.
pub fn redaction_invalidate_derived(conn: &Db, repo_id: i64, cs: &Id) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "UPDATE derive_state SET state = 'poison', lease_expires_ns = NULL \
         WHERE repo_id = ?1 AND cs = ?2 AND state IN ('pending','deriving','done')",
        ),
        &pv![repo_id, cs.0.as_slice()],
    )?;
    Ok(())
}

// -- the redaction purge's selection (the GC phase; re-verified in-txn) --------

/// The actively-tombstoned changesets that are PURGEABLE at read time: the
/// changeset row still exists AND nothing references it — no ref target, no
/// lands head/base, no git_map row, no changeset child (the exact probe set
/// of [`gc_collectible_cs`]; a tombstoned node still under a ref or with
/// children stays tombstoned-but-unpurged, a normal state). Runs inside the
/// purging transaction.
pub fn redact_purgeable_cs(conn: &Db, limit: usize) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT r.object_id FROM redactions r \
             JOIN changesets c ON c.id = r.object_id \
             WHERE r.kind = 'changeset' AND r.purged_ns IS NULL \
               AND NOT EXISTS (SELECT 1 FROM refs re WHERE re.target = r.object_id) \
               AND NOT EXISTS (SELECT 1 FROM lands l WHERE l.head = r.object_id OR l.base = r.object_id) \
               AND NOT EXISTS (SELECT 1 FROM git_map g WHERE g.cs_id = r.object_id) \
               AND NOT EXISTS (SELECT 1 FROM changeset_parents par WHERE par.parent = r.object_id) \
             ORDER BY c.generation ASC, r.object_id ASC LIMIT ?1",
        ),
        &pv![limit as i64],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other("redactions.object_id is not 32 bytes"))
            })?))
        },
    )
}

/// The actively-tombstoned blobs with NO reference left anywhere and no
/// upload touch inside the grace window: no `file_changes` row, no blob
/// `tree_entries` row, and no `objects.touched_ns` past `cut_ns` (the same
/// upload grace [`gc_collectible_blobs`] honors — a client between the blob
/// PUT and its referencing changeset create is never overtaken; the record
/// step's row touch re-enters grace). A blob referenced by an UNPURGED
/// tombstoned changeset stays — its file_changes row is a live reference
/// until the changeset's own purge drops it (never dangling; the blob
/// follows in the same pass, right after). Runs inside the purging
/// transaction.
pub fn redact_purgeable_blobs(conn: &Db, cut_ns: i64, limit: usize) -> Result<Vec<Id>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT r.object_id FROM redactions r \
             WHERE r.kind = 'blob' AND r.purged_ns IS NULL \
               AND NOT EXISTS (SELECT 1 FROM objects o \
                               WHERE o.object_id = r.object_id AND o.touched_ns > ?1) \
               AND NOT EXISTS (SELECT 1 FROM file_changes f WHERE f.blob_id = r.object_id) \
               AND NOT EXISTS (SELECT 1 FROM tree_entries te \
                               WHERE te.kind = 0 AND te.object_id = r.object_id) \
             ORDER BY r.marked_ns ASC, r.object_id ASC LIMIT ?2",
        ),
        &pv![cut_ns, limit as i64],
        |r| {
            let b: Vec<u8> = r.get(0)?;
            Ok(Id(<[u8; 32]>::try_from(b).map_err(|_| {
                DbError::from(std::io::Error::other(
                    "redactions.object_id is not 32 bytes",
                ))
            })?))
        },
    )
}

/// The distinct blob ids a purged changeset batch's `file_changes` carried —
/// read BEFORE the rows go (the tombstoned changeset's exclusively-
/// referenced blobs follow the blob rule in the same transaction).
pub fn file_change_blobs_of_cs(conn: &Db, cs_ids: &[Id]) -> Result<Vec<Id>, DbError> {
    let mut out = Vec::new();
    for chunk in cs_ids.chunks(GC_IN_CHUNK) {
        let list = (1..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let rows: Vec<Vec<u8>> = conn.query_all_map(
            &Sql::plain_owned(format!(
                "SELECT DISTINCT blob_id FROM file_changes \
                 WHERE cs IN ({list}) AND blob_id IS NOT NULL"
            )),
            &id_params(chunk),
            |r| r.get(0),
        )?;
        for raw in rows {
            if let Ok(arr) = <[u8; 32]>::try_from(raw) {
                out.push(Id(arr));
            }
        }
    }
    Ok(out)
}

/// The ids among `blobs` with no reference left anywhere and no upload touch
/// inside the grace window (`cut_ns`) — the exclusively-referenced followup
/// of a tombstoned changeset purge, and the same probe set
/// [`redact_purgeable_blobs`] runs. Point probes per id (an imported blob
/// carries no `objects` row and no touch, and still qualifies); runs inside
/// the purging transaction.
pub fn blobs_without_references(conn: &Db, blobs: &[Id], cut_ns: i64) -> Result<Vec<Id>, DbError> {
    let mut out = Vec::new();
    for blob in blobs {
        let touched: Option<i64> = conn.query_opt_map(
            &Sql::plain("SELECT touched_ns FROM objects WHERE object_id = ?1"),
            &pv![blob.0.as_slice()],
            |r| r.get(0),
        )?;
        if touched.is_some_and(|t| t > cut_ns) {
            continue; // an in-window upload touch: the blob re-enters grace
        }
        let fref: Option<i64> = conn.query_opt_map(
            &Sql::plain("SELECT 1 FROM file_changes WHERE blob_id = ?1 LIMIT 1"),
            &pv![blob.0.as_slice()],
            |r| r.get(0),
        )?;
        if fref.is_some() {
            continue;
        }
        let tref: Option<i64> = conn.query_opt_map(
            &Sql::plain("SELECT 1 FROM tree_entries WHERE kind = 0 AND object_id = ?1 LIMIT 1"),
            &pv![blob.0.as_slice()],
            |r| r.get(0),
        )?;
        if tref.is_none() {
            out.push(*blob);
        }
    }
    Ok(out)
}

/// Purges `ids` (tombstoned changesets): every row keyed by them — parents,
/// file changes, the advisory M6 rows (queue, reports, blame, path-history),
/// the ledger rows — and the changeset row LAST. One transaction with the
/// caller's stamp (a crash rolls the whole batch back; the re-run re-selects
/// the same rows). Returns the changesets purged.
pub fn redact_collect_cs(conn: &Db, ids: &[Id]) -> Result<usize, DbError> {
    let mut n = 0;
    for chunk in ids.chunks(GC_IN_CHUNK) {
        let list = (1..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let params = id_params(chunk);
        // Changeset rows LAST (the brief's ordering; within one transaction
        // the order is crash-neutral — every statement commits or none does).
        for table in [
            "changeset_parents",
            "file_changes",
            "derive_state",
            "derive_reports",
            "blame_state",
            "path_history",
            "changesets",
        ] {
            let key = match table {
                "changesets" => "id",
                _ => "cs",
            };
            conn.execute(
                &Sql::plain_owned(format!("DELETE FROM {table} WHERE {key} IN ({list})")),
                &params,
            )?;
        }
        conn.execute(
            &Sql::plain_owned(format!(
                "DELETE FROM gc_pending WHERE scope = 'cs' AND id IN ({list})"
            )),
            &params,
        )?;
        n += chunk.len();
    }
    Ok(n)
}

/// Purges `ids` (tombstoned blobs): the `objects` row, the advisory
/// `repo_blobs` membership rows, and the ledger row — the CAS bytes were
/// already unlinked by the caller (bytes first, the orphan-direction rule;
/// the blob store's `delete` skips already-absent ids, so the re-run after a
/// crash mid-purge converges).
pub fn redact_collect_blobs(conn: &Db, ids: &[Id]) -> Result<(), DbError> {
    for chunk in ids.chunks(GC_IN_CHUNK) {
        let list = (1..)
            .map(|i| format!("?{i}"))
            .take(chunk.len())
            .collect::<Vec<_>>()
            .join(", ");
        let params = id_params(chunk);
        for sql in [
            format!("DELETE FROM objects WHERE object_id IN ({list})"),
            format!("DELETE FROM repo_blobs WHERE object_id IN ({list})"),
            format!("DELETE FROM gc_pending WHERE scope = 'blob' AND id IN ({list})"),
        ] {
            conn.execute(&Sql::plain_owned(sql), &params)?;
        }
    }
    Ok(())
}

// -- fsck (M10b) ---------------------------------------------------------------

/// The redaction-consistency counts for fsck: (active, purged,
/// purged-but-still-referenced). Tombstoned-but-unpurged is a NORMAL state
/// (counted, not drift); a PURGED tombstone whose object row still exists is
/// DRIFT — the purge claimed the space but a reference survived (the
/// never-dangling invariant violated from the purge side).
pub fn fsck_redactions(conn: &Db) -> Result<(i64, i64, i64), DbError> {
    let active: i64 = conn.query_row_map(
        &Sql::plain("SELECT COUNT(*) FROM redactions WHERE purged_ns IS NULL"),
        &pv![],
        |r| r.get(0),
    )?;
    let purged: i64 = conn.query_row_map(
        &Sql::plain("SELECT COUNT(*) FROM redactions WHERE purged_ns IS NOT NULL"),
        &pv![],
        |r| r.get(0),
    )?;
    let drift: i64 = conn.query_row_map(
        &Sql::plain(
            "SELECT COUNT(*) FROM redactions r WHERE r.purged_ns IS NOT NULL AND ( \
             (r.kind = 'changeset' AND EXISTS \
               (SELECT 1 FROM changesets c WHERE c.id = r.object_id)) OR \
             (r.kind = 'blob' AND EXISTS \
               (SELECT 1 FROM objects o WHERE o.object_id = r.object_id)))",
        ),
        &pv![],
        |r| r.get(0),
    )?;
    Ok((active, purged, drift))
}

// -- principal deletion (M10b) --------------------------------------------------
//
// The erasure end-state for a principal: the principal and token rows go,
// the audit ACTOR is pseudonymized, and history attribution
// (`changesets.principal`, `refs.last_principal`, `lands.*`,
// `snapshots.principal`) is left as-is — the honest whole-changeset rule the
// README states (attribution survives unless the changesets are themselves
// tombstoned).

/// Deletes principal `name`: its token rows, its principals row, and its
/// `audit_log` ACTOR pseudonymized to `deleted:<name>` — one transaction.
/// `details_json` payloads are append-only history and stay untouched. The
/// tombstones the principal marked keep their `principal` value: they are
/// the audit trail of admin actions, retained on the same
/// security-accountability basis as the log itself. Returns
/// `None` when no such principal exists, else the deleted token count.
pub fn principal_delete(
    conn: &Db,
    name: &str,
    now_ns: i64,
    actor: &str,
) -> Result<Option<usize>, DbError> {
    let tx = conn.transaction()?;
    let exists: Option<i64> = tx.query_opt_map(
        &Sql::plain("SELECT 1 FROM principals WHERE name = ?1"),
        &pv![name],
        |r| r.get(0),
    )?;
    if exists.is_none() {
        return Ok(None);
    }
    let tokens = tx.execute(
        &Sql::plain("DELETE FROM tokens WHERE principal = ?1"),
        &pv![name],
    )?;
    tx.execute(
        &Sql::plain("DELETE FROM principals WHERE name = ?1"),
        &pv![name],
    )?;
    let pseudo = format!("deleted:{name}");
    tx.execute(
        &Sql::plain("UPDATE audit_log SET principal = ?1 WHERE principal = ?2"),
        &pv![pseudo, name],
    )?;
    audit_append(
        &tx,
        now_ns,
        actor,
        "principal_delete",
        &serde_json::json!({ "principal": name, "tokens_deleted": tokens }).to_string(),
    )?;
    tx.commit()?;
    Ok(Some(tokens))
}

// ---- audit ---------------------------------------------------------------

/// Appends one audit row.
pub fn audit_append(
    conn: &Db,
    ts_ns: i64,
    principal: &str,
    action: &str,
    details_json: &str,
) -> Result<(), DbError> {
    conn.execute(
        &Sql::plain(
            "INSERT INTO audit_log(ts_ns, principal, action, details_json) VALUES (?1, ?2, ?3, ?4)",
        ),
        &pv![ts_ns, principal, action, details_json],
    )?;
    Ok(())
}

/// Lists audit rows with `id < before_id`, descending (id order matches ts
/// order — ids are assigned at insert), up to `limit` rows.
pub fn audit_list(conn: &Db, before_id: i64, limit: i64) -> Result<Vec<AuditRow>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT id, ts_ns, principal, action, details_json FROM audit_log \
         WHERE id < ?1 ORDER BY id DESC LIMIT ?2",
        ),
        &pv![before_id, limit],
        |row| {
            Ok(AuditRow {
                id: row.get(0)?,
                ts_ns: row.get(1)?,
                principal: row.get(2)?,
                action: row.get(3)?,
                details_json: row.get(4)?,
            })
        },
    )
}

/// One action-filtered page of `(id, details_json)` pairs, descending by
/// id (id order matches ts order — ids are assigned at insert). fsck's
/// cumulative gc totals page over THIS instead of the unfiltered
/// [`audit_list`]: an action filter in the statement keeps a busy audit
/// log from crowding the `gc` rows out of a bounded page (a 10k-row page
/// of other actions used to silently under-count every older gc pass).
pub fn audit_page_action(
    conn: &Db,
    action: &str,
    before_id: i64,
    limit: i64,
) -> Result<Vec<(i64, String)>, DbError> {
    conn.query_all_map(
        &Sql::plain(
            "SELECT id, details_json FROM audit_log \
         WHERE action = ?1 AND id < ?2 ORDER BY id DESC LIMIT ?3",
        ),
        &pv![action, before_id, limit],
        |row| Ok((row.get(0)?, row.get(1)?)),
    )
}
