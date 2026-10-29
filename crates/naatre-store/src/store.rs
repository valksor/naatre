//! Open handle to the store: SQLite connection with the durability pragmas
//! applied and migrations run.

use std::time::Duration;

use rusqlite::Connection;

use crate::data_dir::DataDir;
use crate::error::StoreError;
use crate::migrate;

const BUSY_TIMEOUT_MS: u64 = 5000;

/// An open store: the SQLite metadata handle plus its data dir.
///
/// Synchronous by design — the future server wraps calls in
/// `spawn_blocking`. Later milestones build real query APIs on
/// [`Store::conn`]; M1a only needs open, pragmas, and migrations.
#[derive(Debug)]
pub struct Store {
    conn: Connection,
    #[allow(dead_code)] // kept for the server layer; the conn is what M1a exposes
    data: DataDir,
}

impl Store {
    /// Opens `naatre.db` inside `data`, sets the durability pragmas
    /// (WAL, `synchronous=FULL`, busy timeout 5000 ms, foreign keys on),
    /// and brings the schema up to date.
    pub fn open(data: DataDir) -> Result<Store, StoreError> {
        let conn = Connection::open(data.db_path())?;

        // journal_mode returns a row, so it is queried rather than set
        // via pragma_update.
        let _mode: String = conn.query_row("PRAGMA journal_mode = WAL", [], |r| r.get(0))?;
        conn.pragma_update(None, "synchronous", "FULL")?;
        conn.pragma_update(None, "foreign_keys", "ON")?;
        conn.busy_timeout(Duration::from_millis(BUSY_TIMEOUT_MS))?;

        migrate(&conn)?;

        Ok(Store { conn, data })
    }

    /// The metadata connection. The server layer builds query APIs on this.
    pub fn conn(&self) -> &Connection {
        &self.conn
    }

    /// Current schema version. `migrate` during [`Store::open`] guarantees
    /// the row exists, so an absent row is a broken invariant, not an
    /// expected condition.
    pub fn schema_version(&self) -> i64 {
        self.conn
            .query_row(
                "SELECT value FROM meta WHERE key = 'schema_version'",
                [],
                |row| row.get::<_, String>(0),
            )
            .map(|v| v.parse().expect("schema_version parses: migrate wrote it"))
            .expect("schema_version row exists: migrate guarantees it")
    }
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
        let conn = store.conn();
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
    }
}
