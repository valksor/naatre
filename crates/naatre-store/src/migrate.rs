//! Embedded schema migrations.
//!
//! Schema authority (plan: Architecture / Durability invariants): `serve`
//! applies migrations on boot; a binary older than the data dir's schema
//! refuses to start with a clear error rather than guessing.

use rusqlite::{Connection, OptionalExtension};

use crate::error::StoreError;

/// Highest schema version this binary understands.
pub const MAX_SUPPORTED: i64 = 2;

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
