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
    // No literal here: every migration bump must update the rollback fixture
    // below (drop the new objects), not this pin — a literal rots exactly as
    // it did at M6b (6 vs 4).

    // Migration 2 spot-check: the tokens table carries the scope column.
    conn.execute_batch("SELECT scope_path FROM tokens LIMIT 0;")
        .unwrap();

    // Migration 3 spot-check: the membership junction exists.
    conn.execute_batch("SELECT repo_id, tree_id FROM repo_trees LIMIT 0;")
        .unwrap();

    // Migration 4 spot-check: the derived-data framework's tables exist and
    // the state CHECK admits exactly the four framework states.
    conn.execute_batch(
        "SELECT kind, repo_id, cs, state, lease_expires_ns, attempts, output \
         FROM derive_state LIMIT 0;
         SELECT id, name FROM derive_kinds LIMIT 0;
         SELECT kind, repo_id, cs, reason, ts_ns FROM derive_reports LIMIT 0;",
    )
    .unwrap();
    // Spot-check the table set landed.
    let tables: i64 = conn
        .query_row(
            "SELECT count(*) FROM sqlite_master WHERE type = 'table' \
             AND name IN ('meta','repos','changesets','changeset_parents','file_changes',\
             'repo_blobs','refs','trees','tree_entries','objects','principals','tokens',\
             'lands','snapshots','audit_log','git_map','repo_trees',\
             'derive_kinds','derive_state','derive_reports')",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(tables, 20);
    let indexes: i64 = conn
        .query_row(
            "SELECT count(*) FROM sqlite_master WHERE type = 'index' \
             AND name IN ('idx_cs_parents_parent','idx_snapshots_expiry',\
             'idx_derive_state_kind_state','idx_derive_reports_node')",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(indexes, 4);
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
        // migrations 3 and 4 create — an M5-era data dir predates both)
        // and stage v2-era data.
        "DROP INDEX IF EXISTS idx_repo_trees_tree;
         DROP INDEX IF EXISTS idx_changesets_root;
         DROP INDEX IF EXISTS idx_tree_entries_object;
         DROP TABLE repo_trees;
         DROP INDEX IF EXISTS idx_derive_state_kind_state;
         DROP INDEX IF EXISTS idx_derive_reports_node;
         DROP TABLE IF EXISTS derive_reports;
         DROP TABLE IF EXISTS derive_state;
         DROP TABLE IF EXISTS derive_kinds;
         DROP TABLE IF EXISTS blame_state;
         DROP TABLE IF EXISTS path_history;
         DROP INDEX IF EXISTS idx_changesets_generation;
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

// ---- M9: backend conformance -------------------------------------------------
//
// The same store-level suite runs against every (meta, blob) combo that
// exists. M9a ships two: SQLite+fs (always) and Postgres+fs (a REAL
// server wherever one can be produced: NAATRE_TEST_PG_DSN, else a locally
// provisioned throwaway cluster; the legs skip-notice only when no
// PostgreSQL installation exists). SQLite+S3 and Postgres+S3
// join the matrix in M9b when the S3 CAS is real.

mod pg_provision;

use pg_provision::PgTarget;

fn open_pg() -> Option<(PgTarget, Store)> {
    let target = pg_provision::acquire()?;
    let cfg = target.config();
    let data = DataDir::create(tempfile::tempdir().unwrap().path().join("data")).unwrap();
    match Store::open_backend(data, &cfg) {
        Ok(store) => Some((target, store)),
        Err(e) => panic!("postgres backend open failed (details withheld): {e:?}"),
    }
}

/// The backend-neutral store suite: migrations, schema guard, and the
/// statement surface's shapes on the REAL store API — upserts
/// (ignore/replace), id-returning inserts, transactions with rollback,
/// the rowid-ordered queue pick, and the backfill cursor math.
fn conformance(store: &Store) {
    use naatre_store::meta::{P, Sql};
    use naatre_store::pv;

    assert_eq!(store.schema_version(), MAX_SUPPORTED);

    let conn = store.conn();
    let ignore = Sql::ignore("INSERT OR IGNORE INTO repos(name, created_ns) VALUES (?1, ?2)");
    let replace = Sql::replace(
        "INSERT OR REPLACE INTO meta(key, value) VALUES (?1, ?2)",
        &["key"],
        &["value"],
    );

    // (1) id-returning insert + ignore-shape idempotence.
    let id = conn
        .insert_id(
            &Sql::plain("INSERT INTO repos(name, created_ns) VALUES (?1, ?2)"),
            &pv!["conform-repo", 1i64],
        )
        .expect("insert_id");
    assert!(id >= 1);
    let n = conn
        .execute(&ignore, &pv!["conform-repo", 2i64])
        .expect("ignore insert");
    assert_eq!(n, 0, "OR IGNORE must no-op the UNIQUE conflict");
    let n = conn
        .execute(&ignore, &pv!["conform-repo-other", 2i64])
        .expect("ignore insert new");
    assert_eq!(n, 1);

    // (2) replace-shape upsert.
    conn.execute(&replace, &pv!["conform_key", "v1"]).unwrap();
    conn.execute(&replace, &pv!["conform_key", "v2"]).unwrap();
    let v: String = conn
        .query_row_map(
            &Sql::plain("SELECT value FROM meta WHERE key = 'conform_key'"),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(v, "v2");

    // (3) transactions: commit lands, drop-without-commit rolls back.
    {
        let tx = conn.transaction().unwrap();
        tx.execute(
            &Sql::plain("INSERT INTO audit_log(ts_ns, principal, action, details_json) VALUES (1, 'c', 'x', '{}')"),
            &[],
        )
        .unwrap();
        tx.commit().unwrap();
    }
    {
        let tx = conn.transaction().unwrap();
        tx.execute(
            &Sql::plain("INSERT INTO audit_log(ts_ns, principal, action, details_json) VALUES (2, 'c', 'rolled-back', '{}')"),
            &[],
        )
        .unwrap();
        drop(tx); // rollback
    }
    let audits: i64 = conn
        .query_row_map(
            &Sql::plain("SELECT COUNT(*) FROM audit_log WHERE principal = 'c'"),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 1, "the un-committed transaction rolled back");

    // (4) the rowid-ordered queue pick (s.rowid / s.seq ordering) and the
    // backfill cursor's (generation, rowid) arithmetic.
    conn.execute(
        &Sql::plain(
            "INSERT INTO principals(name, role, created_ns) VALUES ('c-admin', 'admin', 0)",
        ),
        &[],
    )
    .unwrap();
    let kinds = conn
        .insert_id(
            &Sql::plain("INSERT INTO derive_kinds(name) VALUES ('conform-kind')"),
            &[],
        )
        .unwrap();
    let mut first_cs = None;
    for k in 0..3i64 {
        let mut cs = [0u8; 32];
        cs[0] = 0xC0 + k as u8;
        conn.execute(
            &Sql::plain(
                "INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns, generation, principal, root_tree_id) \
                 VALUES (?1, ?2, ?3, 'a', 'm', 0, 1, 'p', NULL)",
            ),
            &pv![id, &cs, 0u8],
        )
        .unwrap();
        conn.execute(
            &Sql::plain(
                "INSERT INTO derive_state(kind, repo_id, cs, state) VALUES (?1, ?2, ?3, 'pending')",
            ),
            &pv![kinds, id, &cs],
        )
        .unwrap();
        if first_cs.is_none() {
            first_cs = Some(cs.to_vec());
        }
    }
    let picked: Option<(i64, Vec<u8>)> = conn
        .query_opt_map(
            &Sql::plain_owned(format!(
                "SELECT s.repo_id, s.cs FROM derive_state s WHERE s.kind = ?1 AND s.state = 'pending' \
                 ORDER BY {} ASC LIMIT 1",
                conn.rowid("s")
            )),
            &pv![kinds],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    // The pick orders by INSERTION order (rowid / seq): the oldest seed
    // comes out first — the ordering the derive queue relies on.
    assert_eq!(
        picked.map(|(_, cs)| cs),
        first_cs,
        "pick is insertion-ordered"
    );
    let cursor: (i64, i64) = conn
        .query_row_map(
            &Sql::plain_owned(format!(
                "SELECT c.generation, {} FROM changesets c ORDER BY {} DESC LIMIT 1",
                conn.rowid("c"),
                conn.rowid("c")
            )),
            &[],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    assert_eq!(cursor.0, 1);

    // (5) unique-violation and contention classification exist on both
    // backends (the repo_name UNIQUE hit).
    let dup = conn.insert_id(
        &Sql::plain("INSERT INTO repos(name, created_ns) VALUES (?1, ?2)"),
        &pv!["conform-repo", 3i64],
    );
    let err = dup.unwrap_err();
    assert!(
        err.is_unique_violation(),
        "expected unique violation: {err}"
    );
    assert!(!err.is_retryable_contention());

    // (6) P::Null round-trips through both drivers (the cursor-shape
    // queries bind Option params).
    let hit: Option<i64> = conn
        .query_opt_map(
            &Sql::plain("SELECT id FROM repos WHERE name = ?1 AND created_ns < ?2"),
            &pv!["conform-repo", P::Null],
            |r| r.get(0),
        )
        .unwrap();
    assert!(hit.is_none(), "NULL bound param never matches");

    // (7) The store-level SNAPSHOT primitive per backend: insert, get,
    // expiry listing, purge — the rows the M5 snapshot verbs serve from
    // and the M11 backup verbs will copy (verbs arrive in M11; the rows
    // are the store's contract).
    // Snapshot ids are CSPRNG 64-hex (not content-addressed) — a fixed
    // id is fine for the store primitive.
    let snap_id = naatre_core::ids::Id([0xA5u8; 32]);
    let snap_tree = naatre_core::ids::object_id(naatre_core::ids::Kind::Tree, b"conform-tree");
    conn.execute(
        &Sql::plain(
            "INSERT INTO snapshots(id, repo_id, tree_id, principal, created_ns, expires_ns) \
             VALUES (?1, ?2, ?3, 'c', 1000, 2000)",
        ),
        &pv![&snap_id, id, &snap_tree],
    )
    .unwrap();
    let snap: (Vec<u8>, String, i64, i64) = conn
        .query_row_map(
            &Sql::plain(
                "SELECT tree_id, principal, created_ns, expires_ns FROM snapshots \
                 WHERE repo_id = ?1 AND id = ?2",
            ),
            &pv![id, &snap_id],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .unwrap();
    assert_eq!(snap.0, snap_tree.0.to_vec());
    assert_eq!(snap.1, "c");
    let expired: Vec<Vec<u8>> = conn
        .query_all_map(
            &Sql::plain("SELECT id FROM snapshots WHERE expires_ns <= ?1 ORDER BY id ASC"),
            &pv![2000i64],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(expired, vec![snap_id.0.to_vec()], "expiry listing finds it");
    let n = conn
        .execute(
            &Sql::plain("DELETE FROM snapshots WHERE id = ?1"),
            &pv![&snap_id],
        )
        .unwrap();
    assert_eq!(n, 1, "purge removes the expired row");
}

fn open_sqlite() -> (tempfile::TempDir, Store) {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("naatre.db"), b"").unwrap();
    let store = Store::open(DataDir::open(dir.path()).unwrap()).unwrap();
    (dir, store)
}

#[test]
fn conformance_sqlite_fs() {
    let (_dir, store) = open_sqlite();
    conformance(&store);
}

#[test]
fn conformance_postgres_fs() {
    let Some((_target, store)) = open_pg() else {
        return; // pg_provision already printed the skip notice
    };
    conformance(&store);
}

/// Migration PARITY: the Postgres schema after its migration set carries
/// the same table and column NAME set as the SQLite one (modulo the two
/// documented `seq` identity columns and the rowid internals).
#[test]
fn migration_parity_postgres() {
    let Some((_target, pg)) = open_pg() else {
        return; // pg_provision already printed the skip notice
    };
    let (_sdir, sqlite) = open_sqlite();

    let sqlite_tables: Vec<String> = sqlite
        .conn()
        .query_all_map(
            &naatre_store::Sql::plain(
                "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
            ),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    let pg_tables: Vec<String> = pg
        .conn()
        .query_all_map(
            &naatre_store::Sql::plain(
                "SELECT table_name FROM information_schema.tables \
                 WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name",
            ),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        sqlite_tables, pg_tables,
        "table sets must match across backends"
    );

    // Column NAMES per table must match too (sorted; type parity is the
    // migration files' reviewed contract — the seq columns and identity
    // ids are the documented PG-side deltas).
    for t in &pg_tables {
        let pg_cols: Vec<String> = pg
            .conn()
            .query_all_map(
                &naatre_store::Sql::plain(
                    "SELECT column_name FROM information_schema.columns \
                     WHERE table_schema = current_schema() AND table_name = ?1 ORDER BY column_name",
                ),
                &naatre_store::pv![t],
                |r| r.get(0),
            )
            .unwrap();
        assert!(!pg_cols.is_empty(), "{t} has columns");
        let mut sqlite_cols: Vec<String> = sqlite
            .conn()
            .sqlite_raw()
            .expect("parity's SQLite side is SQLite")
            .prepare(&format!("SELECT name FROM pragma_table_info('{t}')"))
            .unwrap()
            .query_map([], |r| r.get(0))
            .unwrap()
            .collect::<rusqlite::Result<Vec<String>>>()
            .unwrap();
        sqlite_cols.sort();
        let mut pg_sorted = pg_cols.clone();
        pg_sorted.sort();
        // Documented PG-side deltas (migrations/postgres/001_init.sql
        // header): the `seq` identity columns stand in for SQLite's
        // implicit rowid on the two tables whose insertion order the code
        // reads. Everything else must match exactly.
        if t == "changesets" || t == "derive_state" {
            pg_sorted.retain(|c| c != "seq");
        }
        assert_eq!(sqlite_cols, pg_sorted, "column parity for {t}");
    }
}

/// The schema-authority guard on Postgres: a future schema_version refuses
/// to start, exactly like SQLite.
#[test]
fn future_schema_refuses_on_postgres() {
    let Some(target) = pg_provision::acquire() else {
        return; // pg_provision already printed the skip notice
    };
    let cfg = target.config();
    let schema = cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let dir = tempfile::tempdir().unwrap();
    let store =
        Store::open_backend(DataDir::create(dir.path()).unwrap(), &cfg).expect("pg store opens");
    store
        .conn()
        .execute(
            &naatre_store::Sql::plain(
                "INSERT INTO meta(key, value) VALUES ('schema_version', '999') \
                 ON CONFLICT(key) DO UPDATE SET value = EXCLUDED.value",
            ),
            &[],
        )
        .unwrap();
    drop(store);
    // A fresh open re-runs migrate → must refuse exactly like SQLite.
    let err = Store::open_backend(DataDir::create(dir.path()).unwrap(), &cfg).unwrap_err();
    assert!(
        matches!(err, StoreError::SchemaFuture { found: 999, .. }),
        "expected SchemaFuture, got {err:?}"
    );
    // Drop the stamped schema so the scratch database does not grow one
    // per run.
    let cleanup = target.config();
    let conn = naatre_store::Db::connect_postgres(cleanup.pg.as_ref().unwrap(), false).unwrap();
    conn.execute_batch(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"))
        .unwrap();
}
