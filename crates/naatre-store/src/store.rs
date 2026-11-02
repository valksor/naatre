//! Open handle to the store: the metadata connection (SQLite or Postgres)
//! with its durability setup applied and migrations run.

use std::path::Path;
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
        let conn = open_sqlite_file(&data.db_path())?;
        migrate(&conn)?;
        Ok(Store {
            db: Db::Sqlite(conn),
            data,
        })
    }

    /// Opens (creating if absent) the CATALOG database for the
    /// `DbPerRepo` layout ([`crate::layout`]) inside `data`, applying the
    /// same durability pragmas and the catalog's own migration list
    /// ([`crate::catalog`]). The catalog must carry the adoption's
    /// `adopted` marker ([`crate::catalog::ADOPTED_MARKER`]) — a
    /// marker-less catalog is a crashed adoption's partial target and is
    /// refused with the incomplete-migration remedy (the operator deletes
    /// `catalog.db` + `repos/` and re-runs; the server never serves a
    /// partial store). The adoption's own verification pass opens the
    /// not-yet-marked catalog it just built via [`open_catalog_raw`]. The
    /// `Store` handle wraps the catalog connection: the server's
    /// global-statement paths (`state.write` / `state.read`) run against
    /// it, while the per-repo connections come from [`open_repo_db`] via
    /// the server's routing layer.
    pub fn open_catalog(data: DataDir) -> Result<Store, StoreError> {
        let conn = open_sqlite_file(&data.path().join(crate::layout::CATALOG_FILE))?;
        crate::catalog::migrate_catalog(&conn)?;
        require_adopted_marker(&conn, &data)?;
        Ok(Store {
            db: Db::Sqlite(conn),
            data,
        })
    }

    /// [`open_catalog`] WITHOUT the adoption-marker check: the offline
    /// adoption's verification pass opens the catalog it JUST built, before
    /// the marker exists (the marker is the adoption's LAST write, after
    /// fsck). Every other entry point goes through [`open_catalog`], which
    /// refuses a marker-less catalog.
    pub fn open_catalog_raw(data: DataDir) -> Result<Store, StoreError> {
        let conn = open_sqlite_file(&data.path().join(crate::layout::CATALOG_FILE))?;
        crate::catalog::migrate_catalog(&conn)?;
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

/// Opens one SQLite metadata file with the store's durability setup: WAL,
/// `synchronous=FULL`, busy timeout [`BUSY_TIMEOUT_MS`], foreign keys on.
/// Shared by [`Store::open`] (`naatre.db`), [`Store::open_catalog`]
/// (`catalog.db`), and [`open_repo_db`] (the per-repo files) — every
/// SQLite connection the store family opens carries the same pragmas.
fn open_sqlite_file(path: &Path) -> Result<Connection, StoreError> {
    let conn = Connection::open(path)?;

    // journal_mode returns a row, so it is queried rather than set
    // via pragma_update.
    let _mode: String = conn.query_row("PRAGMA journal_mode = WAL", [], |r| r.get(0))?;
    conn.pragma_update(None, "synchronous", "FULL")?;
    conn.pragma_update(None, "foreign_keys", "ON")?;
    conn.busy_timeout(Duration::from_millis(BUSY_TIMEOUT_MS))?;
    Ok(conn)
}

/// Opens (creating if absent) ONE per-repo database file for the
/// `DbPerRepo` layout and brings its schema up to date with the SHARED
/// SQLite `MIGRATIONS` list, verbatim (a fresh per-repo file runs the
/// same 1..=12 the single-DB file runs; the global tables exist in it but
/// stay empty — the catalog owns the global rows). The server's routing
/// layer keeps one of these open per repo (write) plus a small read-only
/// pool; the file's WAL + busy settings come from the same pragma helper
/// as every other store connection.
///
/// Creating is the CREATE path's job (repo create, the adoption's fresh
/// files). A registry row whose file vanished must NOT be silently
/// masked by an empty repo — those callers use [`open_repo_db_existing`].
pub fn open_repo_db(path: &Path) -> Result<Db, StoreError> {
    let conn = open_sqlite_file(path)?;
    migrate(&conn)?;
    Ok(Db::Sqlite(conn))
}

/// [`open_repo_db`] for a file that MUST already exist: refuses an absent
/// file with the named inconsistency instead of silently creating an empty
/// repo over a live registry row (the boot walk's recovery open, and the
/// routing layer's per-repo write connection).
pub fn open_repo_db_existing(path: &Path) -> Result<Db, StoreError> {
    if !path.exists() {
        return Err(StoreError::RepoFileMissing(path.to_path_buf()));
    }
    let conn = open_sqlite_file(path)?;
    migrate(&conn)?;
    Ok(Db::Sqlite(conn))
}

/// True when `catalog.db` at `data` carries the adoption's `adopted`
/// marker ([`crate::catalog::ADOPTED_MARKER`]). An absent or unreadable
/// catalog is not adopted — the caller refuses.
pub fn store_has_adopted_marker(data: &Path) -> bool {
    let conn = match rusqlite::Connection::open_with_flags(
        data.join(crate::layout::CATALOG_FILE),
        rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY,
    ) {
        Ok(conn) => conn,
        Err(_) => return false,
    };
    conn.query_row(
        "SELECT 1 FROM meta WHERE key = ?1",
        [crate::catalog::ADOPTED_MARKER],
        |r| r.get::<_, i64>(0),
    )
    .is_ok()
}

/// The marker check behind [`Store::open_catalog`] and the layout guard.
fn require_adopted_marker(conn: &Connection, data: &DataDir) -> Result<(), StoreError> {
    let adopted: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM meta WHERE key = ?1",
            [crate::catalog::ADOPTED_MARKER],
            |r| r.get(0),
        )
        .ok();
    if adopted.is_none() {
        return Err(StoreError::Backend(
            crate::backend::BackendError::IncompleteMigration {
                path: data.path().join(crate::layout::CATALOG_FILE),
            },
        ));
    }
    Ok(())
}

/// [`open_repo_db`] WITHOUT the migration run: an additional read-write
/// connection to an already-migrated store file (the routing layer's aux
/// catalog connection). A file that was never migrated must not be
/// silently created by an aux open — callers open migrated files only
/// (the catalog exists because the layout check passed).
pub fn open_repo_conn(path: &Path) -> Result<Db, StoreError> {
    let conn = open_sqlite_file(path)?;
    Ok(Db::Sqlite(conn))
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
