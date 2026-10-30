//! Store integration tests: migrations (including the pinned future-schema
//! refuse-to-start fixture), durability pragmas, and the M1a durability
//! invariant.

use rusqlite::Connection;

use naatre_store::{Cas, DataDir, MAX_SUPPORTED, Store, StoreError, migrate};

fn fresh_conn(dir: &tempfile::TempDir) -> Connection {
    Connection::open(dir.path().join("hand.db")).unwrap()
}

#[test]
fn fresh_db_migrates_to_current() {
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
    assert_eq!(MAX_SUPPORTED, 3);

    // Migration 2 spot-check: the tokens table carries the scope column.
    conn.execute_batch("SELECT scope_path FROM tokens LIMIT 0;")
        .unwrap();

    // Migration 3 spot-check: the membership junction exists.
    conn.execute_batch("SELECT repo_id, tree_id FROM repo_trees LIMIT 0;")
        .unwrap();

    // Spot-check the table set landed.
    let tables: i64 = conn
        .query_row(
            "SELECT count(*) FROM sqlite_master WHERE type = 'table' \
             AND name IN ('meta','repos','changesets','changeset_parents','file_changes',\
             'repo_blobs','refs','trees','tree_entries','objects','principals','tokens',\
             'lands','snapshots','audit_log','git_map','repo_trees')",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(tables, 17);
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
    assert_eq!(v, MAX_SUPPORTED);
}

/// The migration-3 backfill: a pre-3 data dir (dropped here to version 2
/// with trees written the old way — one shared `trees` row, no membership)
/// comes back from `migrate` with `repo_trees` rows for every writing repo,
/// derived from changesets' root trees plus the `tree_entries` closure.
#[test]
fn migration3_backfills_tree_membership_from_existing_data() {
    let dir = tempfile::tempdir().unwrap();
    let conn = fresh_conn(&dir);
    migrate(&conn).unwrap();
    conn.execute_batch(
        // Roll the schema back to the pre-3 world (drop everything
        // migration 3 creates) and stage v2-era data.
        "DROP INDEX IF EXISTS idx_repo_trees_tree;
         DROP INDEX IF EXISTS idx_changesets_root;
         DROP INDEX IF EXISTS idx_tree_entries_object;
         DROP TABLE repo_trees;
         INSERT OR REPLACE INTO meta(key, value) VALUES ('schema_version', '2');
         INSERT INTO repos(id, name, created_ns) VALUES (1, 'one', 1), (7, 'seven', 1);
         -- One tree shared by both repos (content-addressed id), containing
         -- a dir entry whose child tree is a second tree.
         INSERT INTO trees(id, repo_id, canonical)
             VALUES (x'0101', 1, x'00'), (x'0202', 1, x'00');
         INSERT INTO tree_entries(tree_id, name, kind, object_id, size, exec)
             VALUES (x'0101', 'd', 1, x'0202', 0, 0);
         -- Two changesets, one per repo, both rooted at the shared tree.
         INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns,
                                generation, principal, root_tree_id)
             VALUES (1, x'aa', x'00', 'a', 'm', 1, 1, 'p', x'0101'),
                    (7, x'bb', x'00', 'a', 'm', 1, 1, 'p', x'0101');",
    )
    .unwrap();

    migrate(&conn).unwrap();

    let rows: i64 = conn
        .query_row("SELECT COUNT(*) FROM repo_trees", [], |r| r.get(0))
        .unwrap();
    assert_eq!(rows, 4, "both repos get membership for root AND child tree");
    let beta_missing: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM repo_trees WHERE repo_id = 7 \
             AND tree_id IN (x'0101', x'0202')",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        beta_missing, 2,
        "the later repo (first writer was 1) still backfills"
    );

    // Trust condition for the GC sweep: "zero junction rows = snapshot-
    // only" holds only because the backfill is COMPLETE — a pre-migration
    // history tree gets its junction row here and is therefore never a
    // sweep candidate. Assert exactly that: no junction-less trees remain.
    let junctionless: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM trees t \
             WHERE NOT EXISTS (SELECT 1 FROM repo_trees rt WHERE rt.tree_id = t.id)",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        junctionless, 0,
        "backfill completeness: every pre-migration history tree is junctioned"
    );
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
    assert_eq!(store.schema_version(), MAX_SUPPORTED);
}

#[test]
fn invariant_blob_durable_before_reference() {
    let dir = tempfile::tempdir().unwrap();
    let dd = DataDir::create(dir.path()).unwrap();
    // In the real flow `naatre init` writes naatre.db; the placeholder empty
    // file is a valid empty SQLite database for Store::open to initialize.
    std::fs::write(dd.db_path(), b"").unwrap();
    let store1 = Store::open(DataDir::open(dir.path()).unwrap()).unwrap();
    assert_eq!(store1.schema_version(), MAX_SUPPORTED);
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
    assert_eq!(store2.schema_version(), MAX_SUPPORTED);
}
