//! M10b crash matrix: every [`REDACT_CRASH_POINTS`] name fires inside a
//! SPAWNED SUBPROCESS (this test binary re-execs itself — the env var is
//! read once per process), the child dies at the panic, and the PARENT
//! reopens the data dir and runs converging passes: the tombstoned
//! changeset + the tombstoned unreferenced blob purge exactly once across
//! the re-runs, the exclusively-referenced blob follows one grace-gated
//! pass later (the dying tree spares it), fsck is clean, and the live chain
//! survives byte-identical — crash-mid-purge is idempotent on re-run,
//! including the orphan-direction state (`redact_mid_blob_purge` leaves
//! DB rows claiming already-unlinked bytes).
//!
//! Concurrency note: every test owns a private temp dir and child process.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use axum::Router;
use serde_json::{Value, json};

use naatre_core::ids::{Kind, object_id};
use naatre_server::fsck;
use naatre_server::gc::{REDACT_CRASH_POINTS, gc_pass};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

const BASE_NS: u64 = 1_700_000_000_000_000_000;
const GRACE_NS: u64 = 30 * 24 * 3600 * 1_000_000_000;
const DEADLINE: Duration = Duration::from_secs(60);
const ENV_DIR: &str = "NAATRE_M10B_DIR";

#[path = "m4b_common/mod.rs"]
mod m4b_common;
use m4b_common::{REPO, blob_hex, cs_body, post_json_on, seed_repo_at, seed_token_at};

// ---- child plumbing (the m10a pattern) ---------------------------------------

fn spawn_child(dir: &Path, crash: &str) -> Child {
    let mut cmd = Command::new(std::env::current_exe().unwrap());
    cmd.args(["--exact", "m10b_crash_child", "--nocapture"])
        .env(ENV_DIR, dir)
        .env("NAATRE_CRASH_POINT", crash)
        .env_remove("RUST_LOG")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    cmd.spawn().expect("spawn reexec child")
}

struct Collected {
    status: std::process::ExitStatus,
    stderr: String,
}

fn wait_bounded(child: Child) -> Collected {
    let mut child = child;
    let mut out_pipe = child.stdout.take().expect("stdout piped");
    let mut err_pipe = child.stderr.take().expect("stderr piped");
    let err_reader = std::thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = err_pipe.read_to_end(&mut buf);
        String::from_utf8_lossy(&buf).into_owned()
    });
    let out_reader = std::thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = out_pipe.read_to_end(&mut buf);
    });
    let started = Instant::now();
    let status = loop {
        if let Some(status) = child.try_wait().expect("try_wait") {
            break status;
        }
        if started.elapsed() >= DEADLINE {
            let _ = child.kill();
            let _ = child.wait();
            panic!("crash child exceeded {DEADLINE:?}; killed");
        }
        std::thread::sleep(Duration::from_millis(5));
    };
    out_reader.join().expect("stdout drainer");
    Collected {
        status,
        stderr: err_reader.join().expect("stderr drainer"),
    }
}

// ---- the shared state shape (parent + child) ---------------------------------

struct Attached {
    app: Router,
    db_path: PathBuf,
    state: AppState,
    clock: Arc<AtomicU64>,
}

fn attach(dir: &Path) -> Attached {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let data = DataDir::open(dir).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    state.derive_queue_enabled = false;
    state.derive_backfill_enabled = false;
    let db_path = dir.join("naatre.db");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    Attached {
        app,
        db_path,
        state,
        clock,
    }
}

use tower::ServiceExt;

async fn put_blob(a: &Attached, token: &str, content: &[u8]) -> String {
    let id = blob_hex(content);
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/{REPO}/objects/{id}"))
        .header(axum::http::header::AUTHORIZATION, format!("Bearer {token}"))
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(content.to_vec()))
        .unwrap();
    let res = a.app.clone().oneshot(req).await.unwrap();
    assert!(res.status().is_success(), "PUT: {}", res.status());
    id
}

fn count_at_dir(dir: &Path, sql: &str) -> i64 {
    m4b_common::count_at(&dir.join("naatre.db"), sql)
}

async fn redact_on(a: &Attached, admin: &str, kind: &str, id: &str) {
    let resp = post_json_on(
        &a.app,
        &format!("/v1/repos/{REPO}/redactions"),
        admin,
        &json!({ "id": id, "kind": kind, "reason": "crash pin" }),
    )
    .await;
    assert_eq!(
        resp.status,
        axum::http::StatusCode::CREATED,
        "redact {id}: {}",
        resp.body
    );
}

/// The child body: runs ONE pass over the pre-built tombstones and dies at
/// `NAATRE_CRASH_POINT`. Normal suite runs (env markers absent) pass
/// trivially — the M4b pattern.
#[tokio::test]
async fn m10b_crash_child() {
    let Ok(dir) = std::env::var(ENV_DIR) else {
        return;
    };
    let dir = PathBuf::from(dir);
    let a = attach(&dir);
    a.clock.store(BASE_NS + GRACE_NS + 3, Ordering::Relaxed);
    let result = gc_pass(&a.state).await;
    if result.is_ok() {
        panic!(
            "crash point {} did not fire (pass finished cleanly)",
            std::env::var("NAATRE_CRASH_POINT").unwrap_or_default()
        );
    }
    std::process::exit(101);
}

/// The live + tombstone state the crash matrix runs against: a landed root
/// commit, an unlanded orphan commit (tombstoned, purgeable the moment the
/// phase runs), and a LONE uploaded blob referenced by nothing (tombstoned;
/// its purge is the in-closure crash point's target — bytes unlinked, row
/// transaction not yet committed).
async fn seed_live_and_tombstones(dir: &Path) {
    let a = attach(dir);
    let admin = seed_token_at(&a.db_path, "admin", "admin", None);
    let writer = seed_token_at(&a.db_path, "writer-p", "writer", None);
    let _repo_id = seed_repo_at(&a.db_path, REPO);

    // Live: a root commit landed on main.
    let live = b"live-content".to_vec();
    let live_hex = put_blob(&a, &writer, &live).await;
    let created = post_json_on(
        &a.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &writer,
        &cs_body(
            json!([]),
            json!([m4b_common::add("a.txt", &live_hex, live.len())]),
            "root",
        ),
    )
    .await;
    assert_eq!(
        created.status,
        axum::http::StatusCode::CREATED,
        "{}",
        created.body
    );
    let root = created.body["id"].as_str().unwrap().to_string();
    m4b_common::seed_main_at(&a.db_path, 1, &root);
    let land = post_json_on(
        &a.app,
        &format!("/v1/repos/{REPO}/land"),
        &writer,
        &json!({ "head": root, "key": "kc" }),
    )
    .await;
    assert_eq!(land.status, axum::http::StatusCode::OK, "{}", land.body);

    // Orphan: an unlanded commit with a unique blob + unique root tree,
    // tombstoned (changeset + blob).
    let orphan_content = b"orphan-redact-content".to_vec();
    let orphan_hex = put_blob(&a, &writer, &orphan_content).await;
    let main_hex = m4b_common::main_target_at(&a.db_path, 1).unwrap();
    let orphan = {
        let resp = post_json_on(
            &a.app,
            &format!("/v1/repos/{REPO}/changesets"),
            &writer,
            &cs_body(
                json!([main_hex]),
                json!([m4b_common::add("b.txt", &orphan_hex, orphan_content.len())]),
                "orphan",
            ),
        )
        .await;
        assert_eq!(resp.status, axum::http::StatusCode::CREATED);
        resp.body["id"].as_str().unwrap().to_string()
    };
    // LONE blob: uploaded, referenced by nothing, tombstoned — the in-closure
    // point's target (its purge deletes bytes then rows in one transaction).
    let lone_hex = put_blob(&a, &writer, b"lone-redact-blob").await;

    // The LEDGER pass at the base clock (the m10a seed shape): the orphan's
    // cs + blob + dying tree and the lone blob all stamp first_ns = BASE,
    // so the child's pass (and the parent's convergence passes at
    // BASE + grace) find them past the window.
    a.clock.store(BASE_NS, Ordering::Relaxed);
    gc_pass(&a.state).await.unwrap();

    redact_on(&a, &admin, "changeset", &orphan).await;
    redact_on(&a, &admin, "blob", &orphan_hex).await;
    redact_on(&a, &admin, "blob", &lone_hex).await;
    // `a` drops: the store conns close before the child process opens them.
}

// ---- the parent-side pins -----------------------------------------------------

/// The converging re-run after each crash point: the tombstoned changeset
/// and the lone blob purge EXACTLY ONCE across the re-runs, the orphan
/// blob follows once its dying tree is collected (past grace), fsck is
/// clean, and the live chain serves byte-identically.
#[tokio::test]
async fn crash_points_converge_on_rerun() {
    for point in REDACT_CRASH_POINTS {
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("data");
        std::fs::create_dir_all(&dir).unwrap();
        let data = DataDir::create(&dir).unwrap();
        std::fs::write(data.db_path(), b"").unwrap();
        seed_live_and_tombstones(&dir).await;

        let child = spawn_child(&dir, point);
        let collected = wait_bounded(child);
        assert!(
            !collected.status.success(),
            "crash point {point}: the child exited cleanly; stderr: {}",
            collected.stderr
        );
        assert!(
            !collected.stderr.contains("panicked at") || collected.stderr.contains("crash point"),
            "crash point {point}: the child panicked outside the crash point; stderr: {}",
            collected.stderr
        );

        // The parent reopens and converges, advancing the clock between
        // passes (the orphan blob's dying tree needs the grace window).
        let a = attach(&dir);
        let writer = seed_token_at(&a.db_path, "writer-p", "writer", None);
        let mut lone_purges = 0usize;
        for step in 1..=4usize {
            a.clock
                .store(BASE_NS + GRACE_NS + 3 + step as u64, Ordering::Relaxed);
            let report = gc_pass(&a.state).await.unwrap();
            lone_purges += report.redactions_purged_blobs;
        }
        // Both tombstoned blobs purge EXACTLY ONCE each across the
        // converged passes (the lone blob in the first pass; the orphan
        // blob once its dying tree is collected past grace — the
        // mid-blob-purge state included: bytes already absent, the re-run's
        // delete skips them and the rows+stamp commit once).
        assert_eq!(
            lone_purges, 2,
            "crash point {point}: each tombstoned blob purges exactly once"
        );

        // Converged state: the tombstoned changeset is fully purged, and
        // its exclusively-referenced blob followed once the dying tree was
        // collected (past grace, within these passes).
        let orphan_blob = object_id(Kind::Blob, b"orphan-redact-content");
        assert_eq!(
            a.state.cas.has(&[orphan_blob]).unwrap(),
            vec![false],
            "crash point {point}: the orphan blob's bytes are gone"
        );
        assert_eq!(
            count_at_dir(&dir, "SELECT COUNT(*) FROM changesets"),
            1,
            "crash point {point}: only the live changeset remains"
        );
        assert_eq!(
            count_at_dir(
                &dir,
                "SELECT COUNT(*) FROM redactions WHERE kind = 'changeset' AND purged_ns IS NULL"
            ),
            0,
            "crash point {point}: the changeset tombstone is stamped"
        );
        assert_eq!(
            count_at_dir(
                &dir,
                "SELECT COUNT(*) FROM redactions WHERE kind = 'blob' AND purged_ns IS NULL"
            ),
            0,
            "crash point {point}: every blob tombstone is stamped"
        );
        let lone_id = object_id(Kind::Blob, b"lone-redact-blob");
        assert_eq!(
            a.state.cas.has(&[lone_id]).unwrap(),
            vec![false],
            "crash point {point}: the lone blob's bytes are gone"
        );
        assert_eq!(
            count_at_dir(
                &dir,
                &format!(
                    "SELECT COUNT(*) FROM objects WHERE object_id = x'{}'",
                    lone_id.to_hex()
                )
            ),
            0,
            "crash point {point}: the lone blob's row is gone"
        );

        // The live chain serves byte-identically.
        let main_hex = m4b_common::main_target_at(&a.db_path, 1).unwrap();
        let served = {
            let req = axum::http::Request::builder()
                .method("GET")
                .uri(format!("/v1/repos/{REPO}/file?at={main_hex}&path=a.txt"))
                .header(
                    axum::http::header::AUTHORIZATION,
                    format!("Bearer {writer}"),
                )
                .body(axum::body::Body::empty())
                .unwrap();
            let res = a.app.clone().oneshot(req).await.unwrap();
            assert_eq!(res.status(), axum::http::StatusCode::OK);
            let body = http_body_util::BodyExt::collect(res.into_body())
                .await
                .unwrap()
                .to_bytes();
            let file: Value = serde_json::from_slice(&body).unwrap();
            let blob_id = file["blob_id"].as_str().unwrap().to_string();
            let req = axum::http::Request::builder()
                .method("GET")
                .uri(format!("/v1/repos/{REPO}/objects/{blob_id}"))
                .header(
                    axum::http::header::AUTHORIZATION,
                    format!("Bearer {writer}"),
                )
                .body(axum::body::Body::empty())
                .unwrap();
            let res = a.app.clone().oneshot(req).await.unwrap();
            assert_eq!(res.status(), axum::http::StatusCode::OK);
            http_body_util::BodyExt::collect(res.into_body())
                .await
                .unwrap()
                .to_bytes()
                .to_vec()
        };
        assert_eq!(served, b"live-content".to_vec(), "crash point {point}");

        // fsck clean.
        let report = fsck::fsck(&a.state).await.unwrap();
        assert!(report.clean(), "crash point {point}: {report:?}");
        assert_eq!(report.redaction_purge_drift, 0, "crash point {point}");
    }
}
