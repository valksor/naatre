//! M9b: the snapshot GC's BOTH gates pinned on the S3 blob backend —
//! `blob = "s3"` puts the ephemeral namespace in the bucket
//! (`ephemeral/<snap>/objects/…` keys), and the GC must drive it exactly
//! like the fs dirs: expired snapshots' key prefixes deleted (then their
//! rows purged — the orphan-direction ordering), rowless aged prefixes
//! swept, other snapshots' keys untouched, and a second pass converging
//! silently.
//!
//! The `snapshot_expired` 410 gates at both fetch layers are pinned by
//! m5a on the fs backend; that code paths' only backend seam is
//! `state.ephemeral` (the same `EphemeralStore` contract), so what is
//! backend-specific here is the GC's prefix deletion and age sweep.
//!
//! Every leg SKIPS with a printed notice when no REAL S3 server can be
//! produced (`NAATRE_TEST_S3_ENDPOINT`, else the pinned minio binary —
//! see `s3_provision`).

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use naatre_server::snapshots_gc::{SNAPSHOT_MAX_TTL_NS, gc_snapshots};
use naatre_server::state::ServerState;
use naatre_store::DataDir;
use naatre_store::backend::BackendConfig;

#[path = "../../naatre-store/tests/s3_provision/mod.rs"]
mod s3_provision;

/// Fixed-step clock base: ns for 2023-11-14. Real S3 LastModified (the
/// sweep's age signal) is always AFTER this, so a fresh namespace is never
/// aged at the base clock — only a clock pushed past the TTL ages one.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
/// 64 lowercase hex chars.
fn snap_hex(seed: u8) -> String {
    format!("{:062x}{:02x}", 0u128, seed)
}

struct S3Fixture {
    _dir: tempfile::TempDir,
    _target: s3_provision::S3Target,
    state: Arc<ServerState>,
    clock: Arc<AtomicU64>,
    db_path: PathBuf,
}

/// The `blob = "s3"` server state: SQLite metadata + the S3 CAS/ephemeral
/// pair (the S3 legs' meta side is pinned by the store suite and
/// m9_pg_test; this fixture exists for the GC's ephemeral gates).
fn fixture() -> Option<S3Fixture> {
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
    // Only this test's own GC may run (the POST-time/background passes
    // would make the report counts nondeterministic — the m5a rationale).
    state.snapshot_gc_enabled = false;
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
    fn seed_repo(&self) -> i64 {
        let conn = rusqlite::Connection::open(&self.db_path).unwrap();
        conn.execute(
            "INSERT OR IGNORE INTO repos(id, name, created_ns) VALUES (1, 'm9s3', 1)",
            [],
        )
        .unwrap();
        1
    }

    fn seed_snapshot(&self, snap: &str, expires_ns: i64) {
        let conn = rusqlite::Connection::open(&self.db_path).unwrap();
        // True binary decode of the hex id — the GC's row cross-check
        // probes with Id::from_hex(dir name), so the seeded row must match
        // that decode exactly.
        let id: Vec<u8> = (0..32)
            .map(|i| u8::from_str_radix(&snap[2 * i..2 * i + 2], 16).unwrap())
            .collect();
        let tree: Vec<u8> = (0..32).map(|i| (i as u8) | 0x40).collect();
        conn.execute(
            "INSERT INTO snapshots(id, repo_id, tree_id, principal, created_ns, expires_ns) \
             VALUES (?1, 1, ?2, 'tester', 1, ?3)",
            rusqlite::params![id, tree, expires_ns],
        )
        .unwrap();
    }

    fn count(&self, sql: &str) -> i64 {
        let conn = rusqlite::Connection::open(&self.db_path).unwrap();
        conn.query_row(sql, [], |r| r.get(0)).unwrap()
    }
}

/// Gate 1 of the GC: an EXPIRED snapshot's key prefix is deleted and the
/// row purged, another (live) snapshot's keys are untouched, the history
/// CAS is untouched, the audit row lands, and a second pass converges.
#[test]
fn s3_gc_expired_gate_deletes_prefix_and_purges_row() {
    // Plain test thread: the harness's reqwest::blocking clients must not
    // be built inside a tokio runtime. The async GC calls get this test's
    // own current-thread runtime.
    let Some(f) = fixture() else {
        return; // s3_provision already printed the skip notice
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    f.seed_repo();

    let expired = snap_hex(0xC1);
    let live = snap_hex(0xC2);
    let e1 = f.state.ephemeral.put_blob(&expired, b"expiring").unwrap();
    let l1 = f.state.ephemeral.put_blob(&live, b"staying").unwrap();
    f.seed_snapshot(&expired, 100); // expired long before BASE_NS
    f.seed_snapshot(&live, i64::MAX / 2);

    let report = rt.block_on(gc_snapshots(&f.state)).unwrap();
    assert_eq!(
        report.expired, 1,
        "the expired snapshot was collected: {report:?}"
    );
    assert_eq!(
        report.rows_purged, 1,
        "the row went with the keys: {report:?}"
    );
    // Dirs-then-rows ordering shows up here: the keys were deletable, so
    // the purge was allowed to run (a failed delete leaves the row for the
    // next pass — the orphan-direction rule).
    assert_eq!(report.rowless, 0);

    // The expired namespace is EMPTY; the live one is byte-identical.
    assert_eq!(
        f.state.ephemeral.has(&expired, &[e1]).unwrap(),
        vec![false],
        "the expired snapshot's keys are gone"
    );
    assert_eq!(
        f.state.ephemeral.has(&live, &[l1]).unwrap(),
        vec![true],
        "a live snapshot's keys are untouched"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM snapshots"),
        1,
        "only the expired row went"
    );

    // The collecting pass wrote ONE snapshot_gc audit row, actor "system".
    let (actor, details_json): (String, String) = rusqlite::Connection::open(&f.db_path)
        .unwrap()
        .query_row(
            "SELECT principal, details_json FROM audit_log WHERE action = 'snapshot_gc'",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("a snapshot_gc audit row");
    assert_eq!(actor, "system");
    let details: serde_json::Value = serde_json::from_str(&details_json).unwrap();
    assert_eq!(
        details,
        serde_json::json!({ "expired": 1, "rowless": 0, "rows_purged": 1, "trees_purged": 0 })
    );

    // A second pass converges (idempotent) and writes no audit row.
    let report = rt.block_on(gc_snapshots(&f.state)).unwrap();
    assert_eq!(report, Default::default(), "the second pass is idle");
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'snapshot_gc'"),
        1,
        "the idle second pass wrote no audit row"
    );
}

/// Gate 2 of the GC: a KEY PREFIX with no snapshot row (a POST that died
/// between the PUTs and the row insert) is swept once its newest object is
/// older than the sweep threshold — on S3 the age signal is the objects'
/// LastModified, so the injected clock must move past the TTL for a fresh
/// prefix to age (the honest analog of the fs mtime sweep).
#[test]
fn s3_gc_rowless_gate_sweeps_aged_prefix() {
    let Some(f) = fixture() else {
        return; // s3_provision already printed the skip notice
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    f.seed_repo();

    let orphan = snap_hex(0xC3);
    let o1 = f
        .state
        .ephemeral
        .put_blob(&orphan, b"orphaned upload")
        .unwrap();

    // At the base clock the prefix is brand-new (its LastModified is
    // "now"): the sweep threshold (clock - 7d) is far below it.
    let report = rt.block_on(gc_snapshots(&f.state)).unwrap();
    assert_eq!(
        report.rowless, 0,
        "a fresh namespace is not garbage: {report:?}"
    );
    assert_eq!(f.state.ephemeral.has(&orphan, &[o1]).unwrap(), vec![true]);

    // The sweep threshold is (clock - 7d) against the objects' REAL
    // LastModified, so the clock must move past real now + TTL — not just
    // past the fixture's 2023 base — for the fresh prefix to age. (The fs
    // sweep's dir mtime behaves the same way; there the pinned shape uses
    // a future threshold directly.)
    let real_now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos() as u64;
    f.clock.store(
        real_now + SNAPSHOT_MAX_TTL_NS + 60_000_000_000,
        Ordering::Relaxed,
    );
    let report = rt.block_on(gc_snapshots(&f.state)).unwrap();
    assert_eq!(
        report.rowless, 1,
        "the aged rowless prefix was swept: {report:?}"
    );
    assert_eq!(
        f.state.ephemeral.has(&orphan, &[o1]).unwrap(),
        vec![false],
        "the orphaned keys are gone"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM snapshots"),
        0,
        "nothing rowed was touched"
    );
}
