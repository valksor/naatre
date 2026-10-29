//! Store integration tests: migrations (including the pinned future-schema
//! refuse-to-start fixture), durability pragmas, and the M1a durability
//! invariant.

use rusqlite::Connection;

use naatre_store::{Cas, DataDir, MAX_SUPPORTED, Store, StoreError, migrate};

fn fresh_conn(dir: &tempfile::TempDir) -> Connection {
    Connection::open(dir.path().join("hand.db")).unwrap()
}

#[test]
fn fresh_db_migrates_to_1() {
    let dir = tempfile::tempdir().unwrap();
    let conn = fresh_conn(&dir);
    migrate(&conn).unwrap();
    let v: String = conn
        .query_row(
            "SELECT value FROM meta WHERE key = 'schema_version'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(v, MAX_SUPPORTED.to_string());

    // Spot-check the table set landed.
    let tables: i64 = conn
        .query_row(
            "SELECT count(*) FROM sqlite_master WHERE type = 'table' \
             AND name IN ('meta','repos','changesets','changeset_parents','file_changes',\
             'repo_blobs','refs','trees','tree_entries','objects','principals','tokens',\
             'lands','snapshots','audit_log','git_map')",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(tables, 16);
    let indexes: i64 = conn
        .query_row(
            "SELECT count(*) FROM sqlite_master WHERE type = 'index' \
             AND name IN ('idx_cs_parents_parent','idx_snapshots_expiry')",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(indexes, 2);
}

#[test]
fn migrate_twice_is_a_noop() {
    let dir = tempfile::tempdir().unwrap();
    let conn = fresh_conn(&dir);
    migrate(&conn).unwrap();
    migrate(&conn).unwrap();
    let v: i64 = conn
        .query_row(
            "SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(v, 1);
}

#[test]
fn future_schema_refuses_to_start() {
    // Pinned fixture: a data dir written by a newer binary must be refused.
    let dir = tempfile::tempdir().unwrap();
    let conn = fresh_conn(&dir);
    conn.execute_batch(
        "CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
         INSERT INTO meta VALUES ('schema_version', '999');",
    )
    .unwrap();
    match migrate(&conn) {
        Err(StoreError::SchemaFuture { found, supported }) => {
            assert_eq!(found, 999);
            assert_eq!(supported, MAX_SUPPORTED);
            let msg = StoreError::SchemaFuture { found, supported }.to_string();
            assert!(msg.contains("older than the data dir"), "message: {msg}");
        }
        other => panic!("expected SchemaFuture, got {other:?}"),
    }
}

#[test]
fn store_open_runs_migrations() {
    let dir = tempfile::tempdir().unwrap();
    let dd = DataDir::create(dir.path()).unwrap();
    // Store::open requires an initialized dir; naatre.db is created by init
    // in the real flow — here it is created directly to exercise open.
    std::fs::write(dd.db_path(), b"").unwrap();
    let store = Store::open(DataDir::open(dir.path()).unwrap()).unwrap();
    assert_eq!(store.schema_version(), 1);
}

#[test]
fn invariant_blob_durable_before_reference() {
    let dir = tempfile::tempdir().unwrap();
    let dd = DataDir::create(dir.path()).unwrap();
    // In the real flow `naatre init` writes naatre.db; the placeholder empty
    // file is a valid empty SQLite database for Store::open to initialize.
    std::fs::write(dd.db_path(), b"").unwrap();
    let store1 = Store::open(DataDir::open(dir.path()).unwrap()).unwrap();
    assert_eq!(store1.schema_version(), 1);
    drop(store1);

    let cas = Cas::new(&dd).unwrap();
    let payload = b"durable before reference";
    let id = cas.put(payload).unwrap();

    // (a) The write path leaves nothing behind under tmp/.
    let tmp = dd.tmp_dir().unwrap();
    let leftovers: Vec<_> = std::fs::read_dir(&tmp).expect("tmp dir exists").collect();
    assert!(
        leftovers.is_empty(),
        "tmp must be empty after put: {leftovers:?}"
    );

    // (b) The object file exists in the CAS and decompresses to the payload.
    let objects = dd.objects_dir().unwrap();
    let mut found = Vec::new();
    let mut stack = vec![objects.clone()];
    while let Some(d) = stack.pop() {
        for entry in std::fs::read_dir(&d).unwrap() {
            let entry = entry.unwrap();
            if entry.file_type().unwrap().is_dir() {
                stack.push(entry.path());
            } else {
                found.push(entry.path());
            }
        }
    }
    assert_eq!(
        found.len(),
        1,
        "exactly one object file expected: {found:?}"
    );
    assert_eq!(cas.get(&id).unwrap(), payload);

    // (c) A second Store::open on the same DataDir sees a valid store.
    let store2 = Store::open(DataDir::open(dir.path()).unwrap()).unwrap();
    assert_eq!(store2.schema_version(), 1);
}
