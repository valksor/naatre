//! Embedded schema migrations.
//!
//! Schema authority (plan: Architecture / Durability invariants): `serve`
//! applies migrations on boot; a binary older than the data dir's schema
//! refuses to start with a clear error rather than guessing.

use rusqlite::{Connection, OptionalExtension};

use crate::error::StoreError;

/// Highest schema version this binary understands.
pub const MAX_SUPPORTED: i64 = 6;

/// `(version, sql)` for every migration, in order. Version 1 is the full
/// initial schema. Once shipped, a migration's SQL is immutable — fixes go
/// in a new, higher version.
const MIGRATIONS: &[(i64, &str)] = &[
    (
        1,
        r#"
CREATE TABLE repos(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_ns INTEGER NOT NULL);
CREATE TABLE changesets(repo_id INTEGER NOT NULL, id BLOB PRIMARY KEY, canonical BLOB NOT NULL, author TEXT NOT NULL, message TEXT NOT NULL, timestamp_ns INTEGER NOT NULL, generation INTEGER NOT NULL, principal TEXT NOT NULL, root_tree_id BLOB);
CREATE TABLE changeset_parents(repo_id INTEGER NOT NULL, cs BLOB NOT NULL, parent BLOB NOT NULL, ord INTEGER NOT NULL, PRIMARY KEY(repo_id, cs, ord));
CREATE INDEX idx_cs_parents_parent ON changeset_parents(repo_id, parent);
CREATE TABLE file_changes(repo_id INTEGER NOT NULL, cs BLOB NOT NULL, path TEXT NOT NULL, op INTEGER NOT NULL, blob_id BLOB, size INTEGER, exec INTEGER, PRIMARY KEY(repo_id, cs, path));
CREATE TABLE repo_blobs(repo_id INTEGER NOT NULL, object_id BLOB NOT NULL, PRIMARY KEY(repo_id, object_id));
CREATE TABLE refs(repo_id INTEGER NOT NULL, name TEXT NOT NULL, target BLOB, version INTEGER NOT NULL DEFAULT 1, last_principal TEXT, PRIMARY KEY(repo_id, name));
CREATE TABLE trees(id BLOB PRIMARY KEY, repo_id INTEGER NOT NULL, canonical BLOB NOT NULL);
CREATE TABLE tree_entries(tree_id BLOB NOT NULL, name TEXT NOT NULL, kind INTEGER NOT NULL, object_id BLOB NOT NULL, size INTEGER NOT NULL, exec INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(tree_id, name));
CREATE TABLE objects(object_id BLOB PRIMARY KEY, size INTEGER NOT NULL, csize INTEGER NOT NULL);
CREATE TABLE principals(name TEXT PRIMARY KEY, role TEXT NOT NULL CHECK(role IN ('admin','writer','reader')), created_ns INTEGER NOT NULL);
CREATE TABLE tokens(id INTEGER PRIMARY KEY, principal TEXT NOT NULL, blake3 BLOB UNIQUE NOT NULL, expires_ns INTEGER, revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE lands(repo_id INTEGER NOT NULL, principal TEXT NOT NULL, key TEXT NOT NULL, state TEXT NOT NULL, head BLOB, base BLOB, result_json TEXT, PRIMARY KEY(repo_id, principal, key));
CREATE TABLE snapshots(id BLOB PRIMARY KEY, repo_id INTEGER NOT NULL, tree_id BLOB NOT NULL, principal TEXT NOT NULL, created_ns INTEGER NOT NULL, expires_ns INTEGER NOT NULL);
CREATE INDEX idx_snapshots_expiry ON snapshots(expires_ns);
CREATE TABLE audit_log(id INTEGER PRIMARY KEY, ts_ns INTEGER NOT NULL, principal TEXT NOT NULL, action TEXT NOT NULL, details_json TEXT NOT NULL);
CREATE TABLE git_map(repo_id INTEGER NOT NULL, git_oid BLOB NOT NULL, cs_id BLOB NOT NULL, PRIMARY KEY(repo_id, git_oid));
"#,
    ),
    // M1b2: per-token path-prefix scope (`None` = whole repo). Existing
    // tokens get NULL, which the scope matcher treats as unrestricted.
    (2, "ALTER TABLE tokens ADD COLUMN scope_path TEXT;"),
    // M5b redo: per-repo tree membership for HISTORY trees. Tree ids are
    // content-addressed, so identical subtrees share one `trees` row across
    // repos; bare-id reads are gated on this junction (repo-membership-
    // gated, the plan's cross-repo posture — trees carry path names and
    // blob ids), not on `trees.repo_id` (informational). Snapshot trees get
    // NO junction rows: they are own-principal data, TTL'd, served only
    // through the owner-gated snapshot routes. One-time backfill inside the
    // migration: membership for every history tree is derivable from each
    // changeset's root tree plus the `tree_entries` closure below it (kind=1
    // is a dir entry whose object_id is a child tree id). UNION dedups the
    // DAG walk, so the recursion terminates. The indexes back the GC
    // sweep's per-candidate point-lookups (candidates are zero-junction
    // rows; each is kept if any indexed lookup retains it): the junction
    // probe ("no junction row = only a snapshot ever wrote this tree" —
    // the PK alone leads on repo_id), changeset roots, and child entries
    // pointing at the candidate from retained trees.
    (
        3,
        r#"
CREATE TABLE repo_trees(repo_id INTEGER NOT NULL, tree_id BLOB NOT NULL, PRIMARY KEY(repo_id, tree_id));
CREATE INDEX idx_repo_trees_tree ON repo_trees(tree_id);
CREATE INDEX idx_changesets_root ON changesets(root_tree_id);
CREATE INDEX idx_tree_entries_object ON tree_entries(object_id);
WITH RECURSIVE reach(repo_id, tree_id) AS (
    SELECT repo_id, root_tree_id FROM changesets WHERE root_tree_id IS NOT NULL
    UNION
    SELECT r.repo_id, te.object_id FROM reach r
    JOIN tree_entries te ON te.tree_id = r.tree_id AND te.kind = 1
)
INSERT INTO repo_trees(repo_id, tree_id)
SELECT DISTINCT repo_id, tree_id FROM reach;
"#,
    ),
    // M6a: the generic derived-data framework's tables. `derive_kinds` is
    // seeded EMPTY — kinds are registered in code (the `Deriver` trait) and
    // seed their row at registration (M6b registers the real kinds; tests
    // register throwaway kinds). `derive_state` is the per-(kind, repo,
    // changeset) queue and state machine: pending → deriving (under a lease)
    // → done, with poison as the quarantine terminal state for a node that
    // failed `attempts` times; `output` carries the framework-owned opaque
    // output handed to CHILD derivations (kind-specific served-surface
    // storage stays the deriver's job). `derive_reports` rows the poison
    // quarantine records (one per poison event). The index backs the
    // queue's oldest-pending pick (a scan over (kind, state) must stay
    // indexed — the table grows with the changeset count).
    (
        4,
        r#"
CREATE TABLE derive_kinds(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE derive_state(kind INTEGER NOT NULL, repo_id INTEGER NOT NULL, cs BLOB NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','deriving','done','poison')), lease_expires_ns INTEGER, attempts INTEGER NOT NULL DEFAULT 0, output BLOB, PRIMARY KEY(kind, repo_id, cs));
CREATE INDEX idx_derive_state_kind_state ON derive_state(kind, state);
CREATE TABLE derive_reports(kind INTEGER NOT NULL, repo_id INTEGER NOT NULL, cs BLOB NOT NULL, reason TEXT NOT NULL, ts_ns INTEGER NOT NULL);
CREATE INDEX idx_derive_reports_node ON derive_reports(kind, repo_id, cs);
"#,
    ),
    // M6b: the served-surface tables of the blame and path_history derived
    // kinds. `derive_state.output` carries the framework's inter-node
    // handoff; these are the kinds' own keyed, idempotent stores the read
    // routes query (commit_graph keeps its state entirely in the handoff
    // output and needs no table). `blame_state` is the per-changeset
    // DIFF-STATE: for every path the changeset touched, its blob id and the
    // PARENT's blob id — never whole-file annotations — so a blame walk is
    // two point lookups per visited chain node. `path_history` is the
    // by-path inverted index (path → the (generation, changeset, op)
    // touches), newest-first by the PK order.
    (
        5,
        r#"
CREATE TABLE blame_state(repo_id INTEGER NOT NULL, cs BLOB NOT NULL, path TEXT NOT NULL, blob_id BLOB, parent_blob_id BLOB, PRIMARY KEY(repo_id, cs, path));
CREATE TABLE path_history(repo_id INTEGER NOT NULL, path TEXT NOT NULL, gen INTEGER NOT NULL, cs BLOB NOT NULL, op INTEGER NOT NULL, PRIMARY KEY(repo_id, path, gen, cs));
"#,
    ),
    // M6b round 2: poison TAINT on the queue rows. A node that derived on
    // an empty-marker parent (poison, or a poison-bearing ancestor) records
    // tainted=1 with poison_cs = the NEAREST poisoned ancestor's changeset
    // id; descendants inherit both. Reads raise their degraded flag off
    // this row in O(1) instead of re-deriving where the poison sits.
    (
        6,
        r#"
ALTER TABLE derive_state ADD COLUMN tainted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE derive_state ADD COLUMN poison_cs BLOB;
"#,
    ),
];

/// Brings the database at `conn` up to [`MAX_SUPPORTED`].
///
/// Reads `meta.schema_version`; refuses a version newer than
/// [`MAX_SUPPORTED`] (binary older than data dir), no-ops when current, and
/// otherwise applies each missing migration inside its own transaction with
/// the version bump in the same transaction.
pub fn migrate(conn: &Connection) -> Result<(), StoreError> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)",
    )?;

    let current: Option<i64> = conn
        .query_row(
            "SELECT value FROM meta WHERE key = 'schema_version'",
            [],
            |row| row.get::<_, String>(0),
        )
        .optional()?
        .map(|v| {
            v.parse::<i64>().map_err(|_| {
                StoreError::Io(std::io::Error::other(format!(
                    "meta.schema_version is not an integer: {v:?}"
                )))
            })
        })
        .transpose()?;

    let Some(current) = current else {
        return apply_from(conn, 0);
    };
    if current > MAX_SUPPORTED {
        return Err(StoreError::SchemaFuture {
            found: current,
            supported: MAX_SUPPORTED,
        });
    }
    apply_from(conn, current)
}

fn apply_from(conn: &Connection, current: i64) -> Result<(), StoreError> {
    for (version, sql) in MIGRATIONS.iter().filter(|(v, _)| *v > current) {
        // Each migration is atomic together with its version bump: a crash
        // mid-migration leaves the previous version recorded, and the next
        // boot replays from there.
        let tx = conn.unchecked_transaction()?;
        tx.execute_batch(sql)?;
        tx.execute(
            "INSERT OR REPLACE INTO meta(key, value) VALUES ('schema_version', ?1)",
            [version.to_string()],
        )?;
        tx.commit()?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn migrations_are_sequential_from_one() {
        for (expected, (v, _)) in (1..).zip(MIGRATIONS.iter()) {
            assert_eq!(*v, expected, "migration versions must be 1,2,3…");
        }
        assert_eq!(MAX_SUPPORTED, MIGRATIONS.last().map_or(0, |(v, _)| *v));
    }
}
