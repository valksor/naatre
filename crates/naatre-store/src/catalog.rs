//! The catalog database (M13a, `DbPerRepo` layout): the global tables plus
//! the repo registry, in one SQLite file beside the per-repo databases.
//!
//! The catalog carries ONLY what is genuinely global — the plan's pin
//! ("principals/tokens/audit/repo registry — stay in the catalog") plus
//! the tables whose scope is already cross-repo today:
//!
//! * `repos` — the repo registry: name, created-on, and each repo's
//!   `db_file` (the per-repo SQLite file this row owns).
//! * `objects` — the shared `objects/` CAS ledger with `touched_ns` (the
//!   upload grace clock). Bytes stay global; refcounts stay cross-repo.
//! * `gc_pending` — the reachability ledger (repo-less by design: a
//!   collected changeset is gone from every repo). The GC mark walks the
//!   per-repo files and unions reachability; the ledger and the sweep
//!   stay here.
//! * `principals`, `tokens` (with `scope_path`), `audit_log` — the auth
//!   and accountability surface, global by the plan's letter.
//! * `derive_kinds` — the kind registry (ids are stable across processes
//!   and repos; per-repo `derive_state` rows carry the id numerically).
//! * `meta` — the schema version and the boot-backfill completion
//!   markers (global state: a marker covers every repo).
//!
//! The schema has its OWN migration list (`CATALOG_MIGRATIONS`) with the
//! same contract as the per-repo list: refuses a future version, applies
//! each missing migration in its own transaction with the version bump.
//! The per-repo files run the shared `MIGRATIONS` verbatim and never see
//! this module.
//!
//! Everything NOT listed above is per-repo (including `redactions` — the
//! M10b tombstones keep their repo-scoped semantics, one row per repo
//! that marked the object).

use rusqlite::{Connection, OptionalExtension};

use crate::error::StoreError;

/// Highest catalog schema version this binary understands.
pub const CATALOG_MAX_SUPPORTED: i64 = 1;

/// `(version, sql)` for the catalog schema, in order. Once shipped, a
/// migration's SQL is immutable — fixes go in a new, higher version.
const CATALOG_MIGRATIONS: &[(i64, &str)] = &[
    // `meta` is created by the runner (the same shape as the shared list,
    // whose migration 1 also assumes the runner-made `meta`).
    (
        1,
        r#"
CREATE TABLE repos(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_ns INTEGER NOT NULL, db_file TEXT NOT NULL);
CREATE TABLE objects(object_id BLOB PRIMARY KEY, size INTEGER NOT NULL, csize INTEGER NOT NULL, touched_ns INTEGER NOT NULL DEFAULT 0);
CREATE TABLE principals(name TEXT PRIMARY KEY, role TEXT NOT NULL CHECK(role IN ('admin','writer','reader')), created_ns INTEGER NOT NULL);
CREATE TABLE tokens(id INTEGER PRIMARY KEY, principal TEXT NOT NULL, blake3 BLOB UNIQUE NOT NULL, expires_ns INTEGER, revoked INTEGER NOT NULL DEFAULT 0, scope_path TEXT);
CREATE TABLE audit_log(id INTEGER PRIMARY KEY, ts_ns INTEGER NOT NULL, principal TEXT NOT NULL, action TEXT NOT NULL, details_json TEXT NOT NULL);
CREATE TABLE derive_kinds(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE gc_pending(scope TEXT NOT NULL CHECK(scope IN ('cs','blob','tree')), id BLOB NOT NULL, first_ns INTEGER NOT NULL, condemned_at INTEGER, PRIMARY KEY(scope, id));
"#,
    ),
];

/// Brings the catalog database at `conn` up to [`CATALOG_MAX_SUPPORTED`],
/// with the same contract as the per-repo runner ([`crate::migrate`]):
/// refuses a future version, no-ops when current, and otherwise applies
/// each missing migration inside its own transaction with the version
/// bump in the same transaction.
pub fn migrate_catalog(conn: &Connection) -> Result<(), StoreError> {
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
                    "catalog meta.schema_version is not an integer: {v:?}"
                )))
            })
        })
        .transpose()?;

    let Some(current) = current else {
        return apply_catalog_from(conn, 0);
    };
    if current > CATALOG_MAX_SUPPORTED {
        return Err(StoreError::SchemaFuture {
            found: current,
            supported: CATALOG_MAX_SUPPORTED,
        });
    }
    apply_catalog_from(conn, current)
}

fn apply_catalog_from(conn: &Connection, current: i64) -> Result<(), StoreError> {
    for (version, sql) in CATALOG_MIGRATIONS.iter().filter(|(v, _)| *v > current) {
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
    fn catalog_migrations_are_sequential_from_one() {
        for (expected, (v, _)) in (1..).zip(CATALOG_MIGRATIONS.iter()) {
            assert_eq!(*v, expected, "catalog migration versions must be 1,2,3…");
        }
        assert_eq!(
            CATALOG_MAX_SUPPORTED,
            CATALOG_MIGRATIONS.last().map_or(0, |(v, _)| *v)
        );
    }

    #[test]
    fn migrate_catalog_creates_the_global_tables() {
        let conn = Connection::open_in_memory().unwrap();
        migrate_catalog(&conn).unwrap();
        let version: i64 = conn
            .query_row(
                "SELECT value FROM meta WHERE key = 'schema_version'",
                [],
                |r| r.get::<_, String>(0),
            )
            .unwrap()
            .parse()
            .unwrap();
        assert_eq!(version, CATALOG_MAX_SUPPORTED);
        // The registry carries the per-repo db path column; the global
        // tables match their single-DB shapes.
        for table in [
            "repos",
            "objects",
            "principals",
            "tokens",
            "audit_log",
            "derive_kinds",
            "gc_pending",
        ] {
            let n: i64 = conn
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |r| r.get(0))
                .unwrap();
            assert_eq!(n, 0, "{table}");
        }
        let col: String = conn
            .query_row(
                "SELECT name FROM pragma_table_info('repos') WHERE name = 'db_file'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(col, "db_file");
        // Re-run is a no-op (idempotent migration contract).
        migrate_catalog(&conn).unwrap();
    }
}
