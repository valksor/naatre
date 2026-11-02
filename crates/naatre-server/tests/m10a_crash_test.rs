//! M10a crash matrix: every [`GC_CRASH_POINTS`] name fires inside a
//! SPAWNED SUBPROCESS (this test binary re-execs itself — the env var is
//! read once per process), the child dies at the panic (explicit
//! `std::process::exit(101)` for the in-closure points; the test failure's
//! own nonzero exit for the direct ones), and the PARENT reopens the data
//! dir and runs one converging pass: the live chain survives
//! byte-identical, the orphan is fully collected (rows, tree, bytes,
//! objects row), and fsck is clean — crash-mid-GC is idempotent on re-run,
//! including the orphan-direction state (`gc_mid_blob_batch` leaves DB
//! rows claiming already-unlinked bytes).
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
use naatre_server::gc::{GC_CRASH_POINTS, gc_pass};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

const BASE_NS: u64 = 1_700_000_000_000_000_000;
const DEADLINE: Duration = Duration::from_secs(60);
const ENV_DIR: &str = "NAATRE_M10A_DIR";

#[path = "m4b_common/mod.rs"]
mod m4b_common;
use m4b_common::{REPO, blob_hex, cs_body, get_on, seed_repo_at, seed_token_at};

// ---- child plumbing ---------------------------------------------------------

/// Re-execs this test binary as the crash child.
fn spawn_child(dir: &Path, crash: &str) -> Child {
    let mut cmd = Command::new(std::env::current_exe().unwrap());
    cmd.args(["--exact", "m10a_crash_child", "--nocapture"])
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

/// Waits for the child with a hard deadline; pipe drainers keep the child
/// from ever blocking on a full pipe.
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

// ---- the shared state shape (parent + child) --------------------------------

struct Attached {
    app: Router,
    db_path: PathBuf,
    state: AppState,
    clock: Arc<AtomicU64>,
}

/// Attaches to an EXISTING initialized data dir with the fixed clock (the
/// caller owns the temp dir).
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

async fn create_ok(a: &Attached, token: &str, body: &Value) -> String {
    let resp =
        m4b_common::post_json_on(&a.app, &format!("/v1/repos/{REPO}/changesets"), token, body)
            .await;
    assert_eq!(
        resp.status,
        axum::http::StatusCode::CREATED,
        "{}",
        resp.body
    );
    resp.body["id"].as_str().unwrap().to_string()
}

fn count_at_dir(dir: &Path, sql: &str) -> i64 {
    m4b_common::count_at(&dir.join("naatre.db"), sql)
}

fn main_hex_at(dir: &Path, repo_id: i64) -> Value {
    let hex: Option<String> = m4b_common::conn_at(&dir.join("naatre.db"))
        .query_row(
            "SELECT lower(hex(target)) FROM refs WHERE repo_id = ?1 AND name = 'main'",
            [repo_id],
            |r| r.get(0),
        )
        .ok();
    hex.map(Value::String).unwrap_or(Value::Null)
}

/// The child body: runs ONE pass over the pre-built state — which already
/// holds a CONDEMNED blob (the seed's phase A), so the phase-B batch has
/// real bytes to delete — and dies at `NAATRE_CRASH_POINT` (the hook reads
/// the env once per process; this pass is the process's first). The
/// in-closure point (`gc_mid_blob_batch`) fires between the CAS unlink and
/// the row transaction — the orphan-direction state. Normal suite runs
/// (env markers absent) pass trivially — the M4b pattern.
#[tokio::test]
async fn m10a_crash_child() {
    let Ok(dir) = std::env::var(ENV_DIR) else {
        return;
    };
    let dir = PathBuf::from(dir);
    let a = attach(&dir);
    a.clock.store(
        BASE_NS + 30 * 24 * 3600 * 1_000_000_000 + 3,
        Ordering::Relaxed,
    );
    let result = gc_pass(&a.state).await;
    if result.is_ok() {
        panic!(
            "crash point {} did not fire (pass finished cleanly)",
            std::env::var("NAATRE_CRASH_POINT").unwrap_or_default()
        );
    }
    // In-closure points surface as an Err (the panic died in
    // spawn_blocking with the transaction rolled back); die hard like the
    // M4b children so the parent sees a crash, not a clean exit.
    std::process::exit(101);
}

/// The live+orphan state the crash matrix runs against: a landed root
/// commit and an unlanded orphan commit with a unique blob + root tree,
/// plus the ledger pass at the base clock (the candidates are in place
/// before the child ages past the window).
async fn seed_live_and_orphan(dir: &Path, stage_for_blob_child: bool) {
    let a = attach(dir);
    let writer = seed_token_at(&a.db_path, "writer-p", "writer", None);
    let repo_id = seed_repo_at(&a.db_path, REPO);

    // Live: a root commit landed on main.
    let live = b"live-content".to_vec();
    let a_blob = put_blob(&a, &writer, &live).await;
    let created = m4b_common::post_json_on(
        &a.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &writer,
        &cs_body(
            json!([]),
            json!([m4b_common::add("a.txt", &a_blob, live.len())]),
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
    m4b_common::seed_main_at(&a.db_path, repo_id, &root);
    let land = m4b_common::post_json_on(
        &a.app,
        &format!("/v1/repos/{REPO}/land"),
        &writer,
        &json!({ "head": root, "key": "kc" }),
    )
    .await;
    assert_eq!(land.status, axum::http::StatusCode::OK, "{}", land.body);

    // Orphan: an unlanded commit with a unique blob + unique root tree.
    let orphan_content = b"orphan-content".to_vec();
    let orphan_blob = put_blob(&a, &writer, &orphan_content).await;
    let main_hex = main_hex_at(dir, repo_id);
    let orphan = create_ok(
        &a,
        &writer,
        &cs_body(
            json!([main_hex.as_str().unwrap()]),
            json!([m4b_common::add("b.txt", &orphan_blob, orphan_content.len())]),
            "orphan",
        ),
    )
    .await;

    // The derive queue is NON-EMPTY around the crash (the plan's M10 leg:
    // "GC — incl. crash-mid-GC re-run — while the derive queue is
    // non-empty"): a probe kind's row for the orphan, held under a LIVE
    // LEASE (state 'deriving') so the crash-and-converge pass also proves
    // the advisory queue row cannot block collection and cannot survive
    // it as a dangling.
    {
        let conn = m4b_common::conn_at(&a.db_path);
        conn.execute(
            "INSERT OR IGNORE INTO derive_kinds(id, name) VALUES (77, 'm10a-crash-probe')",
            [],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO derive_state(kind, repo_id, cs, state, lease_expires_ns) \
             VALUES (77, ?1, ?2, 'deriving', ?3)",
            rusqlite::params![
                repo_id,
                hex_of(&orphan).0.as_slice(),
                (BASE_NS as i64) + 10 * 24 * 3600 * 1_000_000_000
            ],
        )
        .unwrap();
    }

    // The ledger pass: candidates at first_ns = BASE.
    gc_pass(&a.state).await.unwrap();
    assert_eq!(
        m4b_common::count_at(&a.db_path, "SELECT COUNT(*) FROM gc_pending"),
        3,
        "orphan cs + blob + root tree ledgered"
    );

    // Age past the window and run the pass that COLLECTS the changeset and
    // its root tree (the blob stays — the blob phase runs before the trees
    // phase of the same pass, while the dying tree's entries still
    // reference it), then one more so phase A CONDEMNS the blob (bytes
    // stay). The child's pass then crashes INSIDE phase B — bytes unlinked,
    // row transaction rolled back — the orphan-direction state. STAGED ONLY
    // for `gc_mid_blob_batch`: its crash needs a CONDEMNED blob at the
    // child. Every other point fires before or outside the blob phases;
    // `gc_after_cs_repo_rows` additionally needs the orphan CHANGESET still
    // present past grace at the child (its crash sits between the cs
    // batch's repo deletes and its ledger deletes — the phantom-strand
    // window — and staging would collect the cs before the child runs).
    if !stage_for_blob_child {
        return;
    }
    a.clock.store(
        BASE_NS + 30 * 24 * 3600 * 1_000_000_000 + 1,
        Ordering::Relaxed,
    );
    gc_pass(&a.state).await.unwrap();
    a.clock.store(
        BASE_NS + 30 * 24 * 3600 * 1_000_000_000 + 2,
        Ordering::Relaxed,
    );
    let report = gc_pass(&a.state).await.unwrap();
    assert_eq!(report.blobs_condemned, 1, "the seed's phase A stamps");
    assert_eq!(
        m4b_common::count_at(&a.db_path, "SELECT COUNT(*) FROM gc_pending"),
        1,
        "only the condemned blob remains ledgered"
    );
    // `a` drops: the store conns close before the child process opens them.
}

// ---- the parent-side pins -----------------------------------------------------

/// The converging re-run after each crash point: live survives
/// byte-identical, the orphan is fully collected, fsck is clean.
#[tokio::test]
async fn crash_points_converge_on_rerun() {
    for point in GC_CRASH_POINTS {
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("data");
        std::fs::create_dir_all(&dir).unwrap();
        let data = DataDir::create(&dir).unwrap();
        std::fs::write(data.db_path(), b"").unwrap();
        seed_live_and_orphan(&dir, point == "gc_mid_blob_batch").await;

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
        // passes (the two-phase delete needs pass separation for
        // `condemned_at < now`). What each pass collects varies per crash
        // point (the cs batch runs before the blob phases; the mid-batch
        // crash leaves rows claiming absent bytes), so the pins are on the
        // CONVERGED state: across these passes the orphan blob collects
        // EXACTLY ONCE, total.
        let a = attach(&dir);
        let writer = seed_token_at(&a.db_path, "writer-p", "writer", None);
        let mut blobs_collected_total = 0usize;
        for step in 1..=4usize {
            a.clock.store(
                BASE_NS + 30 * 24 * 3600 * 1_000_000_000 + 3 + step as u64,
                Ordering::Relaxed,
            );
            let report = gc_pass(&a.state).await.unwrap();
            blobs_collected_total += report.blobs_collected;
        }
        assert_eq!(
            blobs_collected_total, 1,
            "crash point {point}: the orphan blob collects exactly once \
             across the converging passes (mid-batch state included)"
        );

        // Converged: no orphan remains, nothing else moved.
        let orphan_blob = object_id(Kind::Blob, b"orphan-content");
        assert_eq!(
            a.state.cas.has(&[orphan_blob]).unwrap(),
            vec![false],
            "crash point {point}: the orphan bytes are gone"
        );
        assert_eq!(
            count_at_dir(&dir, "SELECT COUNT(*) FROM gc_pending"),
            0,
            "crash point {point}: the ledger drained"
        );
        assert_eq!(
            count_at_dir(&dir, "SELECT COUNT(*) FROM changesets"),
            1,
            "crash point {point}: only the live changeset remains"
        );
        // The non-empty derive queue converged too: the leased probe row
        // went with the collected changeset — no resurrection, no dangling
        // (fsck's clean() below asserts the same from the anti-joins).
        assert_eq!(
            count_at_dir(&dir, "SELECT COUNT(*) FROM derive_state"),
            0,
            "crash point {point}: the advisory queue row did not survive \
             the collection"
        );

        // The live chain serves byte-identically.
        let main_hex = main_hex_at(&dir, repo_id_of(&dir));
        let resp = get_on(
            &a.app,
            &format!(
                "/v1/repos/{REPO}/file?at={}&path=a.txt",
                main_hex.as_str().unwrap()
            ),
            &writer,
        )
        .await;
        assert_eq!(resp.status, axum::http::StatusCode::OK, "{}", resp.body);
        let blob = resp.body["blob_id"].as_str().unwrap().to_string();
        let req = axum::http::Request::builder()
            .method("GET")
            .uri(format!("/v1/repos/{REPO}/objects/{blob}"))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {writer}"),
            )
            .body(axum::body::Body::empty())
            .unwrap();
        let res = a.app.clone().oneshot(req).await.unwrap();
        assert!(res.status().is_success());
        let served = http_body_util::BodyExt::collect(res.into_body())
            .await
            .unwrap()
            .to_bytes()
            .to_vec();
        assert_eq!(served, b"live-content".to_vec(), "crash point {point}");

        // fsck clean.
        let report = fsck::fsck(&a.state).await.unwrap();
        assert!(report.clean(), "crash point {point}: {report:?}");
        assert_eq!(report.bytes_missing, 0, "crash point {point}");
        assert_eq!(
            report.changesets_marked, report.changesets,
            "crash point {point}"
        );
    }
}

fn repo_id_of(dir: &Path) -> i64 {
    m4b_common::conn_at(&dir.join("naatre.db"))
        .query_row("SELECT id FROM repos LIMIT 1", [], |r| r.get(0))
        .unwrap()
}

fn hex_of(hex: &str) -> naatre_core::ids::Id {
    naatre_core::ids::Id::from_hex(hex).unwrap()
}
