//! Open handle to the store: the metadata connection (SQLite or Postgres)
//! with its durability setup applied and migrations run.

use std::time::Duration;

use rusqlite::Connection;

use crate::backend::{BackendConfig, MetaKind};
use crate::data_dir::DataDir;
use crate::error::StoreError;
use crate::meta::Db;
use crate::migrate;

/// The pinned SQLite busy timeout (ms), applied to every SQLite connection
/// the store opens — the write connection in [`Store::open`] and the
/// server's read-pool connections alike. Postgres has no busy timeout: the
/// single-writer slot serializes writers in-process, and cross-process
/// MVCC contention surfaces as serialization errors that the server maps
/// to the same writer-busy-shaped retry.
pub const BUSY_TIMEOUT_MS: u64 = 5000;

/// An open store: the metadata handle plus its data dir.
///
/// Synchronous by design — the future server wraps calls in
/// `spawn_blocking`. The metadata surface lives in [`Db`]; M9 added the
/// Postgres backend ([`Store::open_backend`]) beside the zero-config
/// SQLite default.
#[derive(Debug)]
pub struct Store {
    db: Db,
    /// Present for SQLite (the data dir holds the db file); kept for every
    /// backend so the server can still reach `objects/`/`ephemeral/` paths.
    #[allow(dead_code)] // kept for the server layer; the conn is what M1a exposes
    data: DataDir,
}

impl Store {
    /// Opens `naatre.db` inside `data`, sets the durability pragmas
    /// (WAL, `synchronous=FULL`, busy timeout 5000 ms, foreign keys on),
    /// and brings the schema up to date. The zero-config SQLite path.
    pub fn open(data: DataDir) -> Result<Store, StoreError> {
        let conn = Connection::open(data.db_path())?;

        // journal_mode returns a row, so it is queried rather than set
        // via pragma_update.
        let _mode: String = conn.query_row("PRAGMA journal_mode = WAL", [], |r| r.get(0))?;
        conn.pragma_update(None, "synchronous", "FULL")?;
        conn.pragma_update(None, "foreign_keys", "ON")?;
        conn.busy_timeout(Duration::from_millis(BUSY_TIMEOUT_MS))?;

        migrate(&conn)?;

        Ok(Store {
            db: Db::Sqlite(conn),
            data,
        })
    }

    /// Opens the store for the configured backend ([`BackendConfig`]).
    /// SQLite dispatches to [`Store::open`]; Postgres connects, applies the
    /// test schema (if any), and runs the parallel Postgres migration set.
    /// The caller must have run
    /// [`crate::backend::check_data_dir_backend`] first — this constructor
    /// trusts the config/data-dir agreement.
    pub fn open_backend(data: DataDir, cfg: &BackendConfig) -> Result<Store, StoreError> {
        match cfg.meta {
            MetaKind::Sqlite => Self::open(data),
            MetaKind::Postgres => {
                let pg = cfg
                    .pg
                    .as_ref()
                    .ok_or(StoreError::Db(crate::meta::DbError::BadSql(
                        "meta = postgres requires a parsed DSN: run the config parser first".into(),
                    )))?;
                let db = Db::connect_postgres(pg, false)?;
                migrate::migrate_postgres(&db)?;
                Ok(Store { db, data })
            }
        }
    }

    /// The metadata connection. The server layer builds query APIs on this.
    pub fn conn(&self) -> &Db {
        &self.db
    }

    /// The raw SQLite connection, for SQLite-only callers (the host-local
    /// test seeding in the server suites, the pragma pins). Panics on the
    /// Postgres backend — those callers are SQLite-only by nature and must
    /// take their PG-analog paths.
    pub fn sqlite_conn(&self) -> &Connection {
        self.db
            .sqlite_raw()
            .expect("sqlite_conn is the SQLite-only callers' accessor")
    }

    /// Current schema version. `migrate` during open guarantees the row
    /// exists, so an absent row is a broken invariant, not an expected
    /// condition.
    pub fn schema_version(&self) -> i64 {
        schema_version(&self.db)
    }

    /// Which metadata backend this store opened.
    pub fn meta_kind(&self) -> MetaKind {
        match self.db {
            Db::Sqlite(_) => MetaKind::Sqlite,
            Db::Postgres(_) => MetaKind::Postgres,
        }
    }
}

/// Current schema version of the connection's database, for callers that
/// hold a bare connection (the server's read pool). `migrate` guarantees
/// the row exists, so an absent row is a broken invariant, not an expected
/// condition.
pub fn schema_version(db: &Db) -> i64 {
    db.query_opt(
        &crate::meta::Sql::plain("SELECT value FROM meta WHERE key = 'schema_version'"),
        &[],
    )
    .expect("schema_version query runs")
    .map(|row| {
        row.get::<String>(0)
            .expect("schema_version value is text")
            .parse()
            .expect("schema_version parses: migrate wrote it")
    })
    .expect("schema_version row exists: migrate guarantees it")
}

#[cfg(test)]
mod tests {
    use super::*;

    use crate::MAX_SUPPORTED;

    #[test]
    fn pragmas_stick() {
        let dir = tempfile::tempdir().unwrap();
        // naatre.db must exist before Store::open.
        std::fs::write(dir.path().join("naatre.db"), b"").unwrap();
        let store = Store::open(DataDir::open(dir.path()).unwrap()).unwrap();
        let conn = store.sqlite_conn();
        let mode: String = conn
            .query_row("PRAGMA journal_mode", [], |r| r.get(0))
            .unwrap();
        assert_eq!(mode, "wal");
        let sync: i64 = conn
            .query_row("PRAGMA synchronous", [], |r| r.get(0))
            .unwrap();
        assert_eq!(sync, 2); // FULL
        let busy: i64 = conn
            .query_row("PRAGMA busy_timeout", [], |r| r.get(0))
            .unwrap();
        assert_eq!(busy, 5000);
        let fk: i64 = conn
            .query_row("PRAGMA foreign_keys", [], |r| r.get(0))
            .unwrap();
        assert_eq!(fk, 1);
        assert_eq!(store.schema_version(), MAX_SUPPORTED);
        assert_eq!(store.meta_kind(), MetaKind::Sqlite);
    }
}
