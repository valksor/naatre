//! M10a on the S3 blob backend: the GC's delete path (`BlobStore::delete`
//! by key) and presence listing (`ids()` over the `objects/` prefix) driven
//! through the same pass the fs suite pins — BOTH gates (a live blob
//! survives byte-identical; unreachable and orphaned bytes are reclaimed
//! only past the grace window), plus the re-reference race pin (an OLD
//! orphan re-PUT mid-pass re-enters grace via `objects.touched_ns` — the
//! DB-side signal is uniform across backends; on S3 the re-PUT is a true
//! byte rewrite, so the row touch is what protects it), and fsck's S3
//! external-drift note.
//!
//! Every leg SKIPS with a printed notice when no REAL S3 server can be
//! produced (`NAATRE_TEST_S3_ENDPOINT`, else the pinned minio binary —
//! see `s3_provision`).

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::fsck;
use naatre_server::gc::{GC_DEFAULT_GRACE_SECS, gc_pass};
use naatre_server::state::ServerState;
use naatre_store::{DataDir, backend::BackendConfig};

#[path = "../../naatre-store/tests/s3_provision/mod.rs"]
mod s3_provision;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const GRACE_NS: u64 = GC_DEFAULT_GRACE_SECS * 1_000_000_000;

struct S3Fixture {
    _dir: tempfile::TempDir,
    _target: s3_provision::S3Target,
    state: Arc<ServerState>,
    clock: Arc<AtomicU64>,
    db_path: PathBuf,
}

/// The `blob = "s3"` server state (SQLite metadata + the S3 CAS/ephemeral
/// pair), background tasks off for determinism.
fn fixture() -> Option<S3Fixture> {
    fixture_with(|_| {})
}

fn fixture_with(configure: impl FnOnce(&mut ServerState)) -> Option<S3Fixture> {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let target = s3_provision::acquire()?;
    let dir = tempfile::tempdir().unwrap();
    let data = DataDir::create(dir.path()).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(dir.path()).unwrap();
    let cfg: BackendConfig = target.config(); // meta = sqlite, blob = s3
    let mut state = ServerState::open_backend(data, &cfg).expect("s3-backed state opens");
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    state.derive_queue_enabled = false;
    state.derive_backfill_enabled = false;
    configure(&mut state);
    let db_path = dir.path().join("naatre.db");
    Some(S3Fixture {
        _dir: dir,
        _target: target,
        state: Arc::new(state),
        clock,
        db_path,
    })
}

impl S3Fixture {
    fn count(&self, sql: &str) -> i64 {
        rusqlite::Connection::open(&self.db_path)
            .unwrap()
            .query_row(sql, [], |r| r.get(0))
            .unwrap()
    }

    fn count_params(&self, sql: &str, params: &[&dyn rusqlite::ToSql]) -> i64 {
        rusqlite::Connection::open(&self.db_path)
            .unwrap()
            .query_row(sql, params, |r| r.get(0))
            .unwrap()
    }

    fn exec(&self, sql: &str, params: &[&dyn rusqlite::ToSql]) {
        rusqlite::Connection::open(&self.db_path)
            .unwrap()
            .execute(sql, params)
            .unwrap();
    }

    fn tick_to(&self, now: u64) {
        self.clock.store(now, Ordering::Relaxed);
    }
}

/// Seeds repo 1 with a live root changeset whose file_changes + tree
/// reference `live` (the minimal reachable state the mark must find).
fn seed_live(f: &S3Fixture, live: &Id, cs: &Id, tree: &Id) {
    f.exec(
        "INSERT OR IGNORE INTO repos(id, name, created_ns) VALUES (1, 'm10s3', 1)",
        &[],
    );
    f.exec(
        "INSERT INTO objects(object_id, size, csize, touched_ns) VALUES (?1, 1, 1, ?2)",
        &[&live.0.as_slice(), &(BASE_NS as i64)],
    );
    f.exec(
        "INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns, generation, principal, root_tree_id) \
         VALUES (1, ?1, X'00', 'tester', 'seed', 1, 0, 'tester', ?2)",
        &[&cs.0.as_slice(), &tree.0.as_slice()],
    );
    f.exec(
        "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
         VALUES (1, ?1, 'live.txt', 0, ?2, 1, 0)",
        &[&cs.0.as_slice(), &live.0.as_slice()],
    );
    f.exec(
        "INSERT INTO trees(id, repo_id, canonical) VALUES (?1, 1, X'00')",
        &[&tree.0.as_slice()],
    );
    f.exec(
        "INSERT INTO tree_entries(tree_id, name, kind, object_id, size, exec) VALUES (?1, 'live.txt', 0, ?2, 1, 0)",
        &[&tree.0.as_slice(), &live.0.as_slice()],
    );
    f.exec(
        "INSERT INTO repo_blobs(repo_id, object_id) VALUES (1, ?1)",
        &[&live.0.as_slice()],
    );
    f.exec(
        "INSERT INTO refs(repo_id, name, target, version, last_principal) VALUES (1, 'main', ?1, 1, 'seed')",
        &[&cs.0.as_slice()],
    );
    f.exec(
        "INSERT INTO repo_trees(repo_id, tree_id) VALUES (1, ?1)",
        &[&tree.0.as_slice()],
    );
}

/// Both gates through the S3 backend: the live blob's key survives
/// byte-identical; the unreachable-row blob and the orphaned-upload bytes
/// (CAS only, no objects row) are reclaimed past the window by the
/// trait's delete path; the ledger + audit row behave as on fs.
#[test]
fn s3_gc_delete_path_both_gates() {
    let Some(f) = fixture() else {
        return; // s3_provision already printed the skip notice
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();

    let live_content = b"s3-live-content".to_vec();
    let live = object_id(Kind::Blob, &live_content);
    let cs = object_id(Kind::Blob, b"cs-id-derivation-unused");
    let tree = object_id(Kind::Blob, b"tree-id-derivation-unused");
    seed_live(&f, &live, &cs, &tree);
    f.state.cas.put(&live_content).unwrap();

    // Orphan 1: objects row, no references (a dead upload's row half).
    let orphan1_content = b"s3-orphan-rowed".to_vec();
    let orphan1 = object_id(Kind::Blob, &orphan1_content);
    f.state.cas.put(&orphan1_content).unwrap();
    f.exec(
        "INSERT INTO objects(object_id, size, csize, touched_ns) VALUES (?1, ?2, 1, ?3)",
        &[
            &orphan1.0.as_slice(),
            &(orphan1_content.len() as i64),
            &(BASE_NS as i64),
        ],
    );
    // Orphan 2: CAS bytes only (the crash between put and row).
    let orphan2_content = b"s3-orphan-cas-only".to_vec();
    let orphan2 = object_id(Kind::Blob, &orphan2_content);
    f.state.cas.put(&orphan2_content).unwrap();

    // The presence listing sees all three (the trait's ids()).
    let mut listed = f.state.cas.ids().unwrap();
    listed.sort_by_key(|id| id.to_hex());
    let mut want = vec![live, orphan1, orphan2];
    want.sort_by_key(|id| id.to_hex());
    assert_eq!(listed, want, "ids() lists the bucket's objects/ keys");

    // Ledger pass: orphans become candidates; nothing collects.
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(report.blobs_collected, 0, "{report:?}");
    assert_eq!(
        f.count("SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob'"),
        2,
        "both orphans ledgered, the live blob is not"
    );

    // Inside the window: untouched.
    f.tick_to(BASE_NS + GRACE_NS - 1);
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(
        report.blobs_condemned + report.blobs_collected,
        0,
        "{report:?}"
    );

    // Past the window: phase A condemns both (bytes stay)…
    f.tick_to(BASE_NS + GRACE_NS);
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(report.blobs_condemned, 2, "{report:?}");
    assert_eq!(report.blobs_collected, 0, "{report:?}");
    assert_eq!(
        f.state.cas.has(&[live, orphan1, orphan2]).unwrap(),
        vec![true, true, true],
        "phase A keeps the bytes"
    );

    // …and phase B (a later pass) deletes both orphans' keys via
    // BlobStore::delete; the live key is byte-identical.
    f.tick_to(BASE_NS + GRACE_NS + 1_000);
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(report.blobs_collected, 2, "{report:?}");
    assert_eq!(f.state.cas.has(&[live]).unwrap(), vec![true]);
    assert_eq!(
        f.state.cas.get(&live).unwrap(),
        live_content,
        "live survives byte-identical"
    );
    assert_eq!(
        f.state.cas.has(&[orphan1, orphan2]).unwrap(),
        vec![false, false],
        "both orphans' keys are gone"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&orphan1.0.as_slice()]
        ),
        0,
        "the rowed orphan's objects row went with its bytes"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM gc_pending"),
        0,
        "the ledger drained"
    );

    // One gc audit row for the collecting pass, actor "system"; the idle
    // follow-up adds none.
    let (actor, details): (String, String) = rusqlite::Connection::open(&f.db_path)
        .unwrap()
        .query_row(
            "SELECT principal, details_json FROM audit_log WHERE action = 'gc'",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("a gc audit row");
    assert_eq!(actor, "system");
    let details: serde_json::Value = serde_json::from_str(&details).unwrap();
    assert_eq!(details["blobs"], serde_json::json!(2));
    f.tick_to(BASE_NS + GRACE_NS + 1);
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(report, Default::default(), "the second pass is idle");
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'gc'"),
        1,
        "the idle pass wrote no audit row"
    );

    // fsck: clean, with the S3 external-drift note attached.
    let report = rt.block_on(fsck::fsck(&f.state)).unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.blob_backend, "s3");
    assert!(report.external_drift.is_some(), "the S3 note is attached");
}

/// The steer's race pin on S3: an OLD orphan (past-grace candidate) re-PUT
/// while the pass is between mark and sweep. On S3 the re-PUT is a true
/// byte rewrite — the CAS short-circuit does not exist — but the grace
/// clock is the DB row touch (`objects.touched_ns`, refreshed in the same
/// transaction as the upload record), uniform across backends. B exists
/// after the sweep.
#[test]
fn s3_gc_rereference_race_reput_spares_blob() {
    let Some(f) = fixture_with(|s| {
        s.gc_test_delay_after_mark = std::time::Duration::from_millis(400);
    }) else {
        return;
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();

    let content = b"s3-raced-blob".to_vec();
    let id = object_id(Kind::Blob, &content);
    // First upload: CAS + row, touched at BASE (the PUT route's shape).
    f.state.cas.put(&content).unwrap();
    f.exec(
        "INSERT INTO objects(object_id, size, csize, touched_ns) VALUES (?1, ?2, 1, ?3)",
        &[&id.0.as_slice(), &(content.len() as i64), &(BASE_NS as i64)],
    );

    // Age it into a past-grace candidate.
    rt.block_on(gc_pass(&f.state)).unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 1_000);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob'"),
        1
    );

    // The pass parks between mark and sweep; the re-PUT lands in the
    // window: CAS rewrite + row touch at the CURRENT clock.
    // NOTE: the S3 facade serializes every operation, so the parked pass
    // holds no S3 lock across its pause — the re-PUT lands freely.
    let handle = {
        let state = Arc::clone(&f.state);
        std::thread::spawn(move || {
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            rt.block_on(gc_pass(&state))
        })
    };
    std::thread::sleep(std::time::Duration::from_millis(150));
    let now_i = f.clock.load(Ordering::Relaxed) as i64;
    f.state.cas.put(&content).unwrap(); // the rewrite
    f.exec(
        "UPDATE objects SET touched_ns = ?1 WHERE object_id = ?2",
        &[&now_i, &id.0.as_slice()],
    ); // the PUT route's same-txn row touch

    let report = handle.join().unwrap().unwrap();
    assert_eq!(
        report.blobs_collected, 0,
        "the touch re-entered grace: {report:?}"
    );
    assert_eq!(
        f.state.cas.has(&[id]).unwrap(),
        vec![true],
        "B exists after the sweep"
    );
    // fsck clean afterwards.
    let report = rt.block_on(fsck::fsck(&f.state)).unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.stale_candidates, 0, "the pass repaired the ledger");
}

/// Like the fs suite's drift leg: a DB-known blob whose bucket object was
/// externally deleted reports `bytes_missing` (dirtying fsck, with the S3
/// note) and collects only its rows past grace.
#[test]
fn s3_gc_missing_bytes_collect_rows_with_drift_note() {
    let Some(f) = fixture() else {
        return;
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let id = object_id(Kind::Blob, b"s3-drifted");
    f.exec(
        "INSERT INTO objects(object_id, size, csize, touched_ns) VALUES (?1, 7, 1, ?2)",
        &[&id.0.as_slice(), &(BASE_NS as i64)],
    );
    let report = rt.block_on(fsck::fsck(&f.state)).unwrap();
    assert_eq!(report.bytes_missing, 1, "{report:?}");
    assert!(!report.clean());

    rt.block_on(gc_pass(&f.state)).unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 3);
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(report.blobs_condemned, 1, "{report:?}");
    f.tick_to(BASE_NS + GRACE_NS + 4);
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(report.blobs_collected, 1, "{report:?}");
    let report = rt.block_on(fsck::fsck(&f.state)).unwrap();
    assert!(report.clean(), "{report:?}");
}

/// The steer's pin on S3: PUT existing B; the whole sweep runs over TWO
/// passes (phase A condemns — bytes stay; phase B deletes the key + row);
/// THEN the upload's record step answers the RETRYABLE 503 `blob_swept`
/// (the row is never written over absent bytes — on S3 the answer saves a
/// 64 MiB in-slot re-write), and a successful re-PUT leaves B present +
/// recorded.
#[test]
fn s3_gc_record_step_answers_blob_swept_and_reput_lands() {
    let Some(f) = fixture() else {
        return;
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();

    let content = b"s3-record-step-blob".to_vec();
    let oid = object_id(Kind::Blob, &content);
    f.state.cas.put(&content).unwrap();
    f.exec(
        "INSERT INTO objects(object_id, size, csize, touched_ns) VALUES (?1, ?2, 1, ?3)",
        &[
            &oid.0.as_slice(),
            &(content.len() as i64),
            &(BASE_NS as i64),
        ],
    );

    // Age, condemn, delete: the full sweep.
    rt.block_on(gc_pass(&f.state)).unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 1_000);
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(report.blobs_condemned, 1, "{report:?}");
    assert_eq!(
        f.state.cas.has(&[oid]).unwrap(),
        vec![true],
        "phase A keeps the bytes"
    );
    f.tick_to(BASE_NS + GRACE_NS + 2_000);
    let report = rt.block_on(gc_pass(&f.state)).unwrap();
    assert_eq!(report.blobs_collected, 1, "{report:?}");
    assert_eq!(f.state.cas.has(&[oid]).unwrap(), vec![false]);
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&oid.0.as_slice()]
        ),
        0,
        "the row went with the key"
    );

    // The racing upload's record step: retryable 503 blob_swept, row never
    // written.
    let err = rt
        .block_on(naatre_server::routes::objects::record_object(
            &f.state,
            1,
            "m10s3",
            oid,
            content.len() as i64,
            1,
        ))
        .unwrap_err();
    assert_eq!(
        err.code(),
        naatre_server::api_code::ApiCode::BlobSwept,
        "{err:?}"
    );
    assert!(err.is_retryable(), "{err:?}");
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&oid.0.as_slice()]
        ),
        0,
        "NEVER a row referencing missing bytes"
    );

    // The client's re-PUT (bytes to the bucket + the record step, the PUT
    // route's shape): B exists and is recorded; fsck clean.
    f.state.cas.put(&content).unwrap();
    let existed = rt
        .block_on(naatre_server::routes::objects::record_object(
            &f.state,
            1,
            "m10s3",
            oid,
            content.len() as i64,
            1,
        ))
        .unwrap();
    assert!(!existed, "the sweep deleted the row");
    assert_eq!(
        f.state.cas.has(&[oid]).unwrap(),
        vec![true],
        "B EXISTS after the re-PUT"
    );
    let report = rt.block_on(fsck::fsck(&f.state)).unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.bytes_missing, 0);
}
