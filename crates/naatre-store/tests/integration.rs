//! Store integration tests: migrations (including the pinned future-schema
//! refuse-to-start fixture), durability pragmas, the M1a durability
//! invariant, and the M9 backend CONFORMANCE MATRIX — the same suites over
//! every (meta, blob) combo, each combo REAL when its dependency (a
//! PostgreSQL server / a REAL S3 server) can be produced, skip-noticed
//! with the reason when it cannot.

use std::sync::Arc;

use rusqlite::Connection;

use naatre_store::{
    BlobStore, Cas, DataDir, EphemeralStore, MAX_SUPPORTED, S3Store, Store, StoreError, migrate,
};

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

    // Migration 10 spot-check: the redaction tombstones exist.
    conn.execute_batch(
        "SELECT repo_id, object_id, kind, path, marked_ns, purged_ns \
         FROM redactions LIMIT 0;",
    )
    .unwrap();

    // Migration 11 spot-check: the tombstone probe index exists (it moved
    // here from 010, whose shipped SQL is immutable).
    let probe: i64 = conn
        .query_row(
            "SELECT count(*) FROM sqlite_master WHERE type = 'index' \
             AND name = 'redactions_object_id'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(probe, 1);
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
         DROP INDEX IF EXISTS idx_file_changes_blob;
         DROP TABLE IF EXISTS gc_pending;
         DROP INDEX IF EXISTS idx_refs_target;
         DROP INDEX IF EXISTS idx_lands_head;
         DROP INDEX IF EXISTS idx_lands_base;
         DROP INDEX IF EXISTS idx_git_map_cs;
         DROP INDEX IF EXISTS idx_cs_parents_parent_id;
         DROP INDEX IF EXISTS redactions_object_id;
         DROP TABLE IF EXISTS redactions;
         ALTER TABLE objects DROP COLUMN touched_ns;
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

/// Migration 011's forward pin (fix-m10b2): a version-10 database in 010's
/// AS-SHIPPED form (the tombstones table, no `redactions_object_id` index —
/// a shipped migration's SQL is immutable, so the index fix-m10b appended
/// to 010 was reverted into 011) migrates forward to version 11 and gains
/// the probe index, on BOTH dialects the suite exercises.
#[test]
fn migration11_forward_from_shipped_v10_recreates_the_probe_index() {
    // SQLite: rewind a migrated database to the shipped-form v10 (drop the
    // index, stamp 10), then forward.
    {
        let dir = tempfile::tempdir().unwrap();
        let conn = fresh_conn(&dir);
        migrate(&conn).unwrap();
        conn.execute_batch(
            "DROP INDEX redactions_object_id;
             UPDATE meta SET value = '10' WHERE key = 'schema_version';",
        )
        .unwrap();
        migrate(&conn).unwrap();
        let v: i64 = conn
            .query_row(
                "SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(v, 11);
        let idx: i64 = conn
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE type = 'index' \
                 AND name = 'redactions_object_id'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(idx, 1, "migration 11 recreates the tombstone probe index");
    }

    // Postgres: the same shipped-form rewind and forward on the real
    // backend. `IF NOT EXISTS` is what lets a database the interim binary
    // indexed under version 10 take migration 11 without a unique-violation
    // 500 — this pin's fresh v10 form is the other side.
    let Some((target, _store)) = open_pg() else {
        return; // pg_provision already printed the skip notice
    };
    let cfg = target.config();
    let schema = cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let dir = tempfile::tempdir().unwrap();
    {
        let store = Store::open_backend(DataDir::create(dir.path()).unwrap(), &cfg)
            .expect("pg store opens");
        store
            .conn()
            .execute_batch(
                "DROP INDEX redactions_object_id;
                 UPDATE meta SET value = '10' WHERE key = 'schema_version';",
            )
            .unwrap();
    }
    let store = Store::open_backend(DataDir::create(dir.path()).unwrap(), &cfg)
        .expect("pg store reopens and migrates forward");
    assert_eq!(store.schema_version(), 11);
    let idx: i64 = store
        .conn()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT count(*) FROM pg_indexes \
                 WHERE schemaname = current_schema() AND indexname = 'redactions_object_id'",
            ),
            &naatre_store::pv![],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(idx, 1, "migration 11 recreates the tombstone probe index");
    drop(store);
    // Drop the stamped schema so the scratch database does not grow one
    // per run.
    let cleanup = target.config();
    let conn = naatre_store::Db::connect_postgres(cleanup.pg.as_ref().unwrap(), false).unwrap();
    conn.execute_batch(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"))
        .unwrap();
}

/// Migration 011 tolerates the INTERIM binary's databases (round 2): a
/// version-10 stamp that already carries `redactions_object_id` — exactly
/// what the cb4a5b2 interim binary left behind, having created the index
/// inside its appended 010 — migrates forward to 11 with EXACTLY ONE
/// index of that name (`IF NOT EXISTS` must never duplicate), on BOTH
/// dialects the suite exercises.
#[test]
fn migration11_tolerates_the_interim_binary_index() {
    // SQLite: the current schema has the index; stamping the version back
    // to 10 reproduces the interim dir exactly.
    {
        let dir = tempfile::tempdir().unwrap();
        let conn = fresh_conn(&dir);
        migrate(&conn).unwrap();
        conn.execute_batch("UPDATE meta SET value = '10' WHERE key = 'schema_version';")
            .unwrap();
        migrate(&conn).unwrap();
        let v: i64 = conn
            .query_row(
                "SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(v, 11);
        let idx: i64 = conn
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE type = 'index'                  AND name = 'redactions_object_id'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(idx, 1, "no duplicate index: {idx}");
    }

    // Postgres: the same interim shape on the real backend.
    let Some((target, _store)) = open_pg() else {
        return; // pg_provision already printed the skip notice
    };
    let cfg = target.config();
    let schema = cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let dir = tempfile::tempdir().unwrap();
    {
        let store = Store::open_backend(DataDir::create(dir.path()).unwrap(), &cfg)
            .expect("pg store opens");
        store
            .conn()
            .execute_batch("UPDATE meta SET value = '10' WHERE key = 'schema_version';")
            .unwrap();
    }
    let store = Store::open_backend(DataDir::create(dir.path()).unwrap(), &cfg)
        .expect("pg store reopens and migrates forward");
    assert_eq!(store.schema_version(), 11);
    let idx: i64 = store
        .conn()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT count(*) FROM pg_indexes                  WHERE schemaname = current_schema() AND indexname = 'redactions_object_id'",
            ),
            &naatre_store::pv![],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(idx, 1, "no duplicate index: {idx}");
    drop(store);
    // Drop the stamped schema so the scratch database does not grow one
    // per run.
    let cleanup = target.config();
    let conn = naatre_store::Db::connect_postgres(cleanup.pg.as_ref().unwrap(), false).unwrap();
    conn.execute_batch(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"))
        .unwrap();
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
    let id = cas.put(payload).unwrap().id;

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
// The same store-level suite runs against every (meta, blob) combo. The
// matrix is REAL wherever its dependency can be produced:
// SQLite+fs always; Postgres+fs wherever a PostgreSQL ≥16 server exists
// (NAATRE_TEST_PG_DSN, else a provisioned throwaway cluster); *+S3
// wherever a REAL S3 server exists (NAATRE_TEST_S3_ENDPOINT, else the
// pinned minio binary). The MVCC/serialization legs stay PG-meta-only
// (naatre-server's m9_pg_test).

mod pg_provision;
mod s3_provision;

use pg_provision::PgTarget;
use s3_provision::S3Target;

fn open_pg() -> Option<(PgTarget, Store)> {
    let target = pg_provision::acquire()?;
    let cfg = target.config();
    let data = DataDir::create(tempfile::tempdir().unwrap().path().join("data")).unwrap();
    match Store::open_backend(data, &cfg) {
        Ok(store) => Some((target, store)),
        // The store error is redacted by construction (no DSN in its
        // text), so the panic prints it honestly — no "(details
        // withheld)" label contradicting the payload.
        Err(e) => panic!("postgres backend open failed: {e:?}"),
    }
}

fn open_s3() -> Option<(S3Target, Arc<S3Store>)> {
    let target = s3_provision::acquire()?;
    let cfg = target.config();
    let store = S3Store::new(cfg.s3.as_ref().unwrap(), cfg.allow_plaintext)
        .expect("S3 backend builds against a live server");
    Some((target, Arc::new(store)))
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

/// The backend-neutral BLOB suite: the [`BlobStore`] contract's shapes
/// every combo must satisfy — roundtrip, idempotent put, missing-blob
/// error, the size cap, and batch existence.
fn blob_suite(cas: &dyn BlobStore) {
    // (1) roundtrip: empty, small, and 1 MiB of hard-to-compress bytes.
    let mut x = 0x243f_6a88_85a3_08d3u64;
    let mut lcg = || {
        x = x
            .wrapping_mul(6_364_136_223_846_793_005)
            .wrapping_add(1_442_695_040_888_963_407);
        (x >> 33) as u8
    };
    let big: Vec<u8> = (0..1024 * 1024).map(|_| lcg()).collect();
    for payload in [&b""[..], b"conformance blob", big.as_slice()] {
        let id = cas.put(payload).unwrap().id;
        assert_eq!(cas.get(&id).unwrap(), payload);
    }

    // (2) idempotent put: identical content returns the same id, and a
    // get still verifies.
    let id = cas.put(b"same bytes").unwrap().id;
    let id2 = cas.put(b"same bytes").unwrap().id;
    assert_eq!(id, id2);
    assert_eq!(cas.get(&id).unwrap(), b"same bytes");

    // (3) missing blob is BlobMissing naming the id.
    let absent = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, b"never put");
    match cas.get(&absent) {
        Err(StoreError::BlobMissing(hex)) => assert_eq!(hex, absent.to_hex()),
        other => panic!("expected BlobMissing, got {other:?}"),
    }

    // (4) the cap holds before any bytes move (input size, both backends).
    let too_big = vec![0u8; naatre_store::BLOB_LIMIT + 1];
    match cas.put(&too_big) {
        Err(StoreError::TooLarge { limit, got }) => {
            assert_eq!(limit, naatre_store::BLOB_LIMIT);
            assert_eq!(got, naatre_store::BLOB_LIMIT + 1);
        }
        other => panic!("expected TooLarge, got {other:?}"),
    }

    // (5) batch existence, aligned with ids, and the empty batch.
    let res = cas.has(&[id, absent, id]).unwrap();
    assert_eq!(res, vec![true, false, true]);
    assert!(cas.has(&[]).unwrap().is_empty());
}

/// The backend-neutral SNAPSHOT-BLOB suite (the store-level snapshot
/// primitive per blob backend): put/get/has inside one snapshot
/// namespace, isolation between namespaces and from the history CAS, the
/// non-hex id refusals, wholesale delete (idempotent, empties the
/// namespace), and the aged-namespace listing the GC sweeps by (the
/// brief's pinned future-threshold shape: a threshold beyond now makes
/// even fresh namespaces count as aged).
fn snapshot_blob_suite(eph: &dyn EphemeralStore) {
    let snap_hex = |seed: u8| format!("{:062x}{:02x}", 0u128, seed);
    let snap = snap_hex(0xB1);

    // (1) roundtrip + has within one namespace.
    let id = eph.put_blob(&snap, b"snapshot bytes").unwrap();
    assert_eq!(eph.get_blob(&snap, &id).unwrap(), b"snapshot bytes");
    assert_eq!(eph.has(&snap, &[id]).unwrap(), vec![true]);

    // (2) isolation: another snapshot does not see it.
    let other = snap_hex(0xB2);
    assert_eq!(eph.has(&other, &[id]).unwrap(), vec![false]);
    match eph.get_blob(&other, &id) {
        Err(StoreError::BlobMissing(_)) => {}
        other => panic!("expected BlobMissing in another snapshot, got {other:?}"),
    }

    // (3) non-hex snapshot ids are refused on every op.
    let long_a = "a".repeat(63);
    let long_g = "g".repeat(64);
    for bad in ["", "abc", "../escape", long_a.as_str(), long_g.as_str()] {
        assert!(
            matches!(
                eph.put_blob(bad, b"x"),
                Err(StoreError::InvalidSnapshotId(_))
            ),
            "put must reject {bad:?}"
        );
        assert!(
            matches!(
                eph.get_blob(bad, &naatre_core::ids::Id([0u8; 32])),
                Err(StoreError::InvalidSnapshotId(_))
            ),
            "get must reject {bad:?}"
        );
        assert!(matches!(
            eph.has(bad, &[]),
            Err(StoreError::InvalidSnapshotId(_))
        ));
        assert!(matches!(
            eph.delete_snapshot(bad),
            Err(StoreError::InvalidSnapshotId(_))
        ));
    }

    // (4) wholesale delete removes the namespace and is idempotent.
    eph.delete_snapshot(&snap).unwrap();
    assert_eq!(eph.has(&snap, &[id]).unwrap(), vec![false]);
    eph.delete_snapshot(&snap).unwrap(); // converges

    // (5) the aged listing: a fresh namespace is not aged by a past
    // threshold, and a future threshold (now + large) ages it.
    let snap = snap_hex(0xB3);
    let id = eph.put_blob(&snap, b"aged?").unwrap();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos() as i64;
    let past = now - 60 * 1_000_000_000;
    assert!(
        !eph.rowless_dirs(past, now).unwrap().contains(&snap),
        "a fresh namespace is not aged by a past threshold"
    );
    let aged = eph.rowless_dirs(now + 10_000_000_000, now).unwrap();
    assert!(
        aged.contains(&snap),
        "a future threshold ages every namespace: {aged:?}"
    );
    // The listed name deletes as a namespace (what the GC does with it).
    eph.delete_snapshot(aged.last().unwrap()).unwrap();
    assert_eq!(eph.has(&snap, &[id]).unwrap(), vec![false]);
}

/// The S3 tamper legs: hash verification and the zstd-bomb cap against a
/// REAL bucket, by writing foreign bytes at a content-addressed key
/// through the maintenance hatch — the same integrity guarantees the fs
/// unit tests pin by touching files.
fn s3_integrity_suite(store: &S3Store) {
    use object_store::path::Path;
    use object_store::{ObjectStoreExt, PutPayload};
    use std::io::Read as _;

    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let shard_key = |id: &naatre_core::ids::Id| {
        let hex = id.to_hex();
        Path::from(format!(
            "objects/{}/{}/{}",
            &hex[0..2],
            &hex[2..4],
            &hex[4..]
        ))
    };

    // (1) HashMismatch: bytes at the key that hash elsewhere are refused,
    // and the foreign object is left in place for diagnosis.
    let a = store.put(b"contents of A").unwrap().id;
    let b = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, b"not what A holds");
    let actual_of_b = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, b"contents of B");
    let compressed_b = zstd::bulk::compress(b"contents of B", 3).unwrap();
    let key_b = shard_key(&b);
    rt.block_on(store.raw().put(&key_b, PutPayload::from(compressed_b)))
        .expect("raw put");
    match store.get(&b) {
        Err(StoreError::HashMismatch { expected, actual }) => {
            assert_eq!(expected, b.to_hex());
            assert_eq!(actual, actual_of_b.to_hex());
            // Still there (nothing deleted for diagnosis).
            assert!(
                rt.block_on(store.raw().head(&key_b)).is_ok(),
                "foreign object left in place"
            );
        }
        other => panic!("expected HashMismatch, got {other:?}"),
    }
    let _ = a;

    // (2) The expanded-size cap stops a zstd bomb within the read loop:
    // ~96 MiB of zeros compresses to a few KiB but decompresses past the
    // 64 MiB limit.
    let bomb_payload =
        zstd::stream::encode_all(std::io::repeat(0u8).take(96 * 1024 * 1024), 3).unwrap();
    let bomb_id = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, b"bomb id");
    let key_bomb = shard_key(&bomb_id);
    rt.block_on(store.raw().put(&key_bomb, PutPayload::from(bomb_payload)))
        .expect("raw put");
    match store.get(&bomb_id) {
        Err(StoreError::TooLarge { limit, got }) => {
            assert_eq!(limit, naatre_store::BLOB_LIMIT);
            assert!(got > limit, "cap must stop after passing the limit");
        }
        other => panic!("expected TooLarge, got {other:?}"),
    }
}

#[test]
fn conformance_sqlite_fs() {
    let (_dir, store) = open_sqlite();
    conformance(&store);

    // The blob/ephemeral side of the combo (fs CAS).
    let dir = tempfile::tempdir().unwrap();
    let dd = DataDir::create(dir.path()).unwrap();
    let cas: Arc<dyn BlobStore> = Arc::new(Cas::new(&dd).unwrap());
    blob_suite(cas.as_ref());
    let eph: Arc<dyn EphemeralStore> = Arc::new(naatre_store::Ephemeral::new(&dd).unwrap());
    snapshot_blob_suite(eph.as_ref());
}

#[test]
fn conformance_postgres_fs() {
    let Some((_target, store)) = open_pg() else {
        return; // pg_provision already printed the skip notice
    };
    conformance(&store);

    let dir = tempfile::tempdir().unwrap();
    let dd = DataDir::create(dir.path()).unwrap();
    let cas: Arc<dyn BlobStore> = Arc::new(Cas::new(&dd).unwrap());
    blob_suite(cas.as_ref());
    let eph: Arc<dyn EphemeralStore> = Arc::new(naatre_store::Ephemeral::new(&dd).unwrap());
    snapshot_blob_suite(eph.as_ref());
}

/// The full store suite on SQLite+S3: needs only the REAL S3 server.
#[test]
fn conformance_sqlite_s3() {
    let Some((_target, s3)) = open_s3() else {
        return; // s3_provision already printed the skip notice
    };
    let (_dir, store) = open_sqlite();
    conformance(&store);

    blob_suite(s3.as_ref());
    snapshot_blob_suite(s3.as_ref());
    s3_integrity_suite(&s3);
}

/// The full store suite on Postgres+S3: needs BOTH dependencies; each
/// provisioner skip-notices its own absence (the printed reason names
/// which dependency is missing).
#[test]
fn conformance_postgres_s3() {
    let Some((_target, store)) = open_pg() else {
        return; // pg_provision printed the reason (no PostgreSQL server)
    };
    let Some((_s3target, s3)) = open_s3() else {
        return; // s3_provision printed the reason (no S3 server)
    };
    conformance(&store);

    blob_suite(s3.as_ref());
    snapshot_blob_suite(s3.as_ref());
    s3_integrity_suite(&s3);
}

/// The bucket layout pin: history keys are `objects/ab/cd/<62hex>` — the
/// fs CAS layout byte-for-byte (the documented migration shape) — and
/// snapshot keys sit under `ephemeral/<snap>/objects/…`, a namespace the
/// history CAS never reads and the GC deletes wholesale by prefix.
#[test]
fn s3_layout_mirrors_fs() {
    let Some((_target, s3)) = open_s3() else {
        return; // s3_provision already printed the skip notice
    };
    use futures::StreamExt;
    use object_store::path::Path;

    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let list_keys = |prefix: Path| -> Vec<String> {
        rt.block_on(async {
            s3.raw()
                .list(Some(&prefix))
                .map(|m| m.expect("list item").location.to_string())
                .collect::<Vec<_>>()
                .await
        })
    };

    // History namespace: exactly the fs shard shape.
    let id = s3.put(b"layout pin").unwrap().id;
    let hex = id.to_hex();
    let expected = format!("objects/{}/{}/{}", &hex[0..2], &hex[2..4], &hex[4..]);
    let keys = list_keys(Path::from("objects"));
    assert!(
        keys.contains(&expected),
        "history key {expected} missing from {keys:?}"
    );
    assert!(
        keys.iter().all(|k| k.starts_with("objects/")),
        "no foreign keys under objects/: {keys:?}"
    );

    // Snapshot namespace: ephemeral/<snap>/objects/…, disjoint from
    // history, deletable wholesale by prefix.
    let snap = format!("{:062x}{:02x}", 0u128, 0xE1);
    let sid = s3.put_blob(&snap, b"snapshot layout pin").unwrap();
    let shex = sid.to_hex();
    let expected_snap = format!(
        "ephemeral/{snap}/objects/{}/{}/{}",
        &shex[0..2],
        &shex[2..4],
        &shex[4..]
    );
    assert_eq!(
        list_keys(Path::from(format!("ephemeral/{snap}"))),
        vec![expected_snap]
    );
    assert!(
        matches!(s3.get(&sid), Err(StoreError::BlobMissing(_))),
        "a history get of a snapshot id must miss"
    );
    s3.delete_snapshot(&snap).unwrap();
    assert!(list_keys(Path::from(format!("ephemeral/{snap}"))).is_empty());
}

/// The `history_cas` constructor (what the offline import and the server
/// state share) follows `cfg.blob`: the fs CAS by default, the S3 backend
/// for `blob = "s3"` — and two instances (the import-writes / serve-reads
/// shape) interoperate through the bucket, leaving the data dirs empty.
#[test]
fn history_cas_follows_the_configured_backend() {
    // Default config → fs CAS bytes under the data dir.
    let dir = tempfile::tempdir().unwrap();
    let dd = DataDir::create(dir.path()).unwrap();
    let cas =
        naatre_store::blob::history_cas(&dd, &naatre_store::BackendConfig::default()).unwrap();
    let id = cas.put(b"fs through the constructor").unwrap().id;
    assert!(dir.path().join("objects").join(&id.to_hex()[0..2]).exists());

    // s3 config → bucket keys, readable through a second, independent
    // instance; neither data dir holds the bytes.
    let Some(target) = s3_provision::acquire() else {
        return; // s3_provision already printed the skip notice
    };
    let cfg = target.config();
    let writer_dir = tempfile::tempdir().unwrap();
    let reader_dir = tempfile::tempdir().unwrap();
    let writer = naatre_store::blob::history_cas(
        &DataDir::create(writer_dir.path().join("d")).unwrap(),
        &cfg,
    )
    .unwrap();
    let reader = naatre_store::blob::history_cas(
        &DataDir::create(reader_dir.path().join("d")).unwrap(),
        &cfg,
    )
    .unwrap();
    let payload = b"s3 through the constructor";
    let sid = writer.put(payload).unwrap().id;
    assert_eq!(reader.get(&sid).unwrap(), payload);
    assert!(
        !writer_dir
            .path()
            .join("objects")
            .join(&sid.to_hex()[0..2])
            .exists()
    );
    assert!(
        !reader_dir
            .path()
            .join("objects")
            .join(&sid.to_hex()[0..2])
            .exists()
    );
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
