//! m13a-fix pins: the five-lens correction round over 90caa96.
//!
//! Every pin here was written FIRST and observed failing (or hanging, or
//! panicking) against the unfixed tree, per the brief's failing-test-first
//! rule for the blockers. The crash-point pins re-exec this binary in a
//! subprocess (the m10a/m10b/m4b pattern — `NAATRE_CRASH_POINT` is read
//! once per process, so a pin that needs the env owns its process); the
//! concurrency and layout pins run in-process.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::{Duration, Instant};

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::db;
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;
use naatre_store::backend::{BackendConfig, check_data_dir_backend};
use naatre_store::layout::Layout;

#[path = "m4b_common/mod.rs"]
mod m4b_common;

use m4b_common::{add, blob_hex, cs_body};

/// The fixture's repo (NOT m4b_common::REPO — this suite's stores seed
/// their own repos).
const REPO_A: &str = "m13a-a";

/// ONE logging init per test process (tracing's global subscriber
/// panics on a second `set` — every fixture/attach helper routes here).
fn init_log() {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
}
/// Fixed-step clock base: ns for 2023-11-14 (the m13a fixture's stamp).
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const DAY_NS: u64 = 24 * 3600 * 1_000_000_000;
/// The gc grace the pins step over (the default 30 days).
const GRACE: u64 = 30 * DAY_NS;
const DEADLINE: Duration = Duration::from_secs(60);
const ENV_DIR: &str = "NAATRE_M13AFIX_DIR";
const ENV_ROLE: &str = "NAATRE_M13AFIX_ROLE";
const ENV_TOKEN: &str = "NAATRE_M13AFIX_TOKEN";

fn split_cfg() -> BackendConfig {
    BackendConfig {
        layout: Layout::DbPerRepo,
        ..BackendConfig::default()
    }
}

// ---- fixtures ---------------------------------------------------------------

struct Fixture {
    _dir: tempfile::TempDir,
    data_path: PathBuf,
    state: AppState,
    app: Router,
    token: String,
    clock: Arc<AtomicU64>,
}

/// The m13a fixture: a single-DB store with two repos (each one recorded
/// blob + a NULL-target `main` ref) and an admin token. Split pins adopt it.
async fn fixture() -> Fixture {
    init_log();
    let dir = tempfile::tempdir().unwrap();
    let data_path = dir.path().to_path_buf();
    let data = DataDir::create(&data_path).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(&data_path).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    let state = Arc::new(state);

    let (token, hash) = naatre_server::auth::mint_token();
    state
        .write({
            move |store| {
                let conn = store.conn();
                let tx = conn.transaction().unwrap();
                naatre_server::db::principal_upsert(&tx, "admin", "admin", BASE_NS as i64).unwrap();
                naatre_server::db::token_insert(&tx, "admin", &hash, None, None).unwrap();
                naatre_server::db::audit_append(&tx, BASE_NS as i64, "admin", "init", "{}")
                    .unwrap();
                tx.commit().unwrap();
                Ok(())
            }
        })
        .await
        .unwrap();

    for name in ["m13a-a", "m13a-b"] {
        let repo_name = name.to_string();
        state
            .write(move |store| {
                let conn = store.conn();
                naatre_server::db::repos_create(conn, &repo_name, BASE_NS as i64).unwrap();
                Ok(())
            })
            .await
            .unwrap();
        let content = format!("{name} payload").into_bytes();
        let blob = object_id(Kind::Blob, &content);
        state.cas.put(&content).unwrap();
        let s = Arc::clone(&state);
        let repo_for_record = name.to_string();
        let blob_for_record = blob;
        let content_len = content.len() as i64;
        s.write_repo(repo_for_record, move |cx| {
            naatre_server::db::object_row_upsert(
                &cx.cat(),
                &blob_for_record,
                content_len,
                1,
                BASE_NS as i64,
            )
            .unwrap();
            cx.db
                .execute(
                    &naatre_store::Sql::plain(
                        "INSERT OR IGNORE INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)",
                    ),
                    &naatre_store::pv![cx.id, blob_for_record.0.as_slice()],
                )
                .unwrap();
            Ok(())
        })
        .await
        .unwrap();
        let s = Arc::clone(&state);
        let repo_for_ref = name.to_string();
        s.write_repo(repo_for_ref, move |cx| {
            cx.db
                .execute(
                    &naatre_store::Sql::plain(
                        "INSERT INTO refs(repo_id, name, target, version, last_principal) \
                         VALUES (?1, 'main', NULL, 1, 'seed')",
                    ),
                    &naatre_store::pv![cx.id],
                )
                .unwrap();
            Ok(())
        })
        .await
        .unwrap();
    }
    let app = naatre_server::router(Arc::clone(&state));
    Fixture {
        _dir: dir,
        data_path,
        state,
        app,
        token,
        clock,
    }
}

fn advance(f: &Fixture, to_ns: u64) {
    f.clock.store(to_ns, Ordering::Relaxed);
}

/// Opens the SPLIT state over an adopted data dir (the cutover shape).
fn open_split(data_path: &Path) -> AppState {
    let cfg = split_cfg();
    check_data_dir_backend(data_path, &cfg).unwrap();
    Arc::new(ServerState::open_backend(DataDir::open_any(data_path), &cfg).unwrap())
}

/// [`open_split`] carrying the fixture's injected clock (the gc pins drive
/// the split state through grace windows — the default clock is the wall
/// clock).
fn open_split_clk(data_path: &Path, clock: Arc<AtomicU64>) -> AppState {
    let cfg = split_cfg();
    check_data_dir_backend(data_path, &cfg).unwrap();
    let mut state = ServerState::open_backend(DataDir::open_any(data_path), &cfg).unwrap();
    let c = Arc::clone(&clock);
    state.clock = Arc::new(move || c.load(Ordering::Relaxed));
    Arc::new(state)
}

fn cat_conn(path: &Path) -> rusqlite::Connection {
    rusqlite::Connection::open(path.join("catalog.db")).unwrap()
}

fn single_conn(path: &Path) -> rusqlite::Connection {
    rusqlite::Connection::open(path.join("naatre.db")).unwrap()
}

async fn send(
    app: &Router,
    method: &str,
    uri: &str,
    token: Option<&str>,
    body: Option<Vec<u8>>,
) -> (StatusCode, Value) {
    let mut builder = axum::http::Request::builder().method(method).uri(uri);
    if let Some(t) = token {
        builder = builder.header(header::AUTHORIZATION, format!("Bearer {t}"));
    }
    let req = builder
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(body.unwrap_or_default()))
        .unwrap();
    let res = app.clone().oneshot(req).await.expect("in-memory request");
    let status = res.status();
    let bytes = BodyExt::collect(res.into_body()).await.unwrap().to_bytes();
    let parsed = if bytes.is_empty() {
        Value::Null
    } else {
        serde_json::from_slice(&bytes).unwrap_or(Value::Null)
    };
    (status, parsed)
}

async fn put_blob(app: &Router, token: &str, repo: &str, content: &[u8]) -> String {
    let id = blob_hex(content);
    let (status, body) = send(
        app,
        "PUT",
        &format!("/v1/repos/{repo}/objects/{id}"),
        Some(token),
        Some(content.to_vec()),
    )
    .await;
    assert!(status.is_success(), "blob PUT failed: {status} {body}");
    id
}

async fn create_cs(app: &Router, token: &str, repo: &str, body: Value) -> String {
    let (status, resp) = send(
        app,
        "POST",
        &format!("/v1/repos/{repo}/changesets"),
        Some(token),
        Some(serde_json::to_vec(&body).unwrap()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{resp}");
    resp["id"].as_str().unwrap().to_string()
}

// ---- crash subprocess plumbing (the m10b pattern) ---------------------------

fn spawn_child(dir: &Path, role: &str, crash: &str, token: Option<&str>) -> Child {
    let mut cmd = Command::new(std::env::current_exe().unwrap());
    cmd.args(["--exact", "m13a_fix_child", "--nocapture"])
        .env(ENV_DIR, dir)
        .env(ENV_ROLE, role)
        .env("NAATRE_CRASH_POINT", crash)
        .env_remove("RUST_LOG")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if let Some(t) = token {
        cmd.env(ENV_TOKEN, t);
    }
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

struct Attached {
    app: Router,
    db_path: PathBuf,
    data_path: PathBuf,
    state: AppState,
    token: Option<String>,
    clock: Arc<AtomicU64>,
}

/// Single-mode attach for the child (the m10a shape). `token` is passed by
/// the parent through the env (the token row was minted there); `clock_ns`
/// fixes the injected clock (the child owns its process).
fn attach(dir: &Path, token: Option<&str>, clock_ns: u64) -> Attached {
    init_log();
    let data = DataDir::open(dir).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(clock_ns));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    let db_path = dir.join("naatre.db");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    Attached {
        app,
        db_path,
        data_path: dir.to_path_buf(),
        state,
        token: token.map(str::to_string),
        clock,
    }
}

/// The re-exec child entry. No role env → a plain test-listing no-op (the
/// parent's `--exact` filter never lands here uninvited).
#[test]
fn m13a_fix_child() {
    let Ok(role) = std::env::var(ENV_ROLE) else {
        return;
    };
    let dir = PathBuf::from(std::env::var(ENV_DIR).expect("child dir env"));
    let token = std::env::var(ENV_TOKEN).ok();
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("child runtime");
    match role.as_str() {
        "gc-cs-crash" => rt.block_on(child_gc_cs_crash(&dir)),
        "land-audit" => rt.block_on(child_land_audit(&dir, token.as_deref())),
        "ref-atomic" => rt.block_on(child_ref_atomic(&dir)),
        "redact-atomic" => rt.block_on(child_redact_atomic(&dir)),
        "import-atomic" => rt.block_on(child_import_atomic(&dir)),
        "adopt-copy" => {
            let report =
                naatre_server::adopt::adopt_db_per_repo(&dir, &BackendConfig::default()).unwrap();
            assert!(report.fsck_clean, "the fixture adopts clean");
        }
        other => panic!("unknown child role {other:?}"),
    }
}

/// Seeds one orphan changeset (root, never landed, no ref) + its unique
/// blob into `app`'s repo — the collectible garbage the crash pins need.
async fn seed_orphan_cs(app: &Router, token: &str) -> String {
    let content = b"m13a-fix orphan content".to_vec();
    let blob = put_blob(app, token, REPO_A, &content).await;
    create_cs(
        app,
        token,
        REPO_A,
        cs_body(
            json!([]),
            json!([add("orphan.txt", &blob, content.len())]),
            "orphan",
        ),
    )
    .await
}

async fn child_gc_cs_crash(dir: &Path) {
    let a = attach(dir, None, BASE_NS + GRACE + DAY_NS);
    // Pass 1: the orphan only LEDGERS (its first_ns is this pass's now —
    // never past the same pass's grace cut).
    let r1 = naatre_server::gc::gc_pass(&a.state).await;
    assert!(r1.is_ok(), "pass 1 only ledgers: {r1:?}");
    // Pass 2: the orphan is past grace and collects — the crash point
    // fires after the repo deletes, before the ledger deletes.
    a.clock
        .store(BASE_NS + 2 * (GRACE + DAY_NS), Ordering::Relaxed);
    let report = naatre_server::gc::gc_pass(&a.state).await;
    assert!(
        report.is_err(),
        "the crash point must fire mid-batch (got {report:?})"
    );
}

async fn child_land_audit(dir: &Path, token: Option<&str>) {
    let cfg = split_cfg();
    check_data_dir_backend(dir, &cfg).unwrap();
    let state = Arc::new(ServerState::open_backend(DataDir::open_any(dir), &cfg).unwrap());
    let app = naatre_server::router(Arc::clone(&state));
    let token = token.expect("land child carries the fixture token");
    // A landable repo: root commit, main seeded to it, a child orphan on
    // top (the m10a shape — landing the child rebases onto main's tip).
    let root_content = b"m13a-fix land root".to_vec();
    let root_blob = put_blob(&app, token, REPO_A, &root_content).await;
    let root = create_cs(
        &app,
        token,
        REPO_A,
        cs_body(
            json!([]),
            json!([add("root.txt", &root_blob, root_content.len())]),
            "root",
        ),
    )
    .await;
    m4b_common::seed_main_at(&dir.join("repos/1.db"), 1, &root);
    let head_content = b"m13a-fix land head".to_vec();
    let head_blob = put_blob(&app, token, REPO_A, &head_content).await;
    let head = create_cs(
        &app,
        token,
        REPO_A,
        cs_body(
            json!([root.as_str()]),
            json!([add("head.txt", &head_blob, head_content.len())]),
            "head",
        ),
    )
    .await;
    let (status, body) = send(
        &app,
        "POST",
        &format!("/v1/repos/{REPO_A}/land"),
        Some(token),
        Some(serde_json::to_vec(&json!({ "head": head, "key": "k1" })).unwrap()),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "the before-commit crash point must fail the land: {status} {body}"
    );
    let cat = rusqlite::Connection::open(dir.join("catalog.db")).unwrap();
    let lands: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'land'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(lands, 0, "a rolled-back land must leave NO audit row");
    let repo_file = rusqlite::Connection::open(dir.join("repos/1.db")).unwrap();
    let rows: i64 = repo_file
        .query_row("SELECT COUNT(*) FROM lands", [], |r| r.get(0))
        .unwrap();
    assert_eq!(rows, 0, "the key stays unsettled");
}

async fn child_ref_atomic(dir: &Path) {
    let writer = m4b_common::seed_token_at(&dir.join("naatre.db"), "admin", "admin", None);
    let a = attach(dir, Some(&writer), BASE_NS);
    let token = a.token.as_deref().expect("ref child carries a token");
    let head = seed_orphan_cs(&a.app, token).await;
    let (status, body) = send(
        &a.app,
        "POST",
        &format!("/v1/repos/{REPO_A}/refs/feature"),
        Some(token),
        Some(serde_json::to_vec(&json!({ "target": head })).unwrap()),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "the before-audit crash point must fail the ref move: {status} {body}"
    );
    let conn = rusqlite::Connection::open(&a.db_path).unwrap();
    let refs: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM refs WHERE name = 'feature'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(refs, 0, "the ref move rolled back with its audit row");
    let audits: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'ref_move'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 0, "no audit row for a rolled-back move");
}

async fn child_redact_atomic(dir: &Path) {
    let writer = m4b_common::seed_token_at(&dir.join("naatre.db"), "admin", "admin", None);
    let a = attach(dir, Some(&writer), BASE_NS);
    let token = a.token.as_deref().expect("redact child carries a token");
    let content = b"m13a-fix redact target".to_vec();
    let blob = put_blob(&a.app, token, REPO_A, &content).await;
    let (status, body) = send(
        &a.app,
        "POST",
        &format!("/v1/repos/{REPO_A}/redactions"),
        Some(token),
        Some(serde_json::to_vec(&json!({ "id": blob, "kind": "blob", "reason": "pin" })).unwrap()),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "the before-audit crash point must fail the mark: {status} {body}"
    );
    let (get, _) = send(
        &a.app,
        "GET",
        &format!("/v1/repos/{REPO_A}/objects/{blob}"),
        Some(token),
        None,
    )
    .await;
    assert_ne!(
        get,
        StatusCode::GONE,
        "the rolled-back mark must not gate serving (got {get})"
    );
    let conn = rusqlite::Connection::open(&a.db_path).unwrap();
    let audits: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'redact'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 0, "no audit row for a rolled-back mark");
}

async fn child_import_atomic(dir: &Path) {
    let a = attach(dir, None, BASE_NS);
    let git = dir.parent().unwrap().join("git-src");
    let opts = naatre_server::import::ImportOptions {
        git_path: git,
        into: Some("m13a-imp".to_string()),
        skip_unsupported: true,
        sanitize: false,
    };
    let run = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        naatre_server::import::run(&a.data_path, &opts, &Default::default())
    }));
    let outcome = match run {
        Ok(r) => r,
        Err(panic) => {
            let msg = panic
                .downcast_ref::<String>()
                .cloned()
                .or_else(|| panic.downcast_ref::<&str>().map(|s| s.to_string()))
                .unwrap_or_default();
            assert!(
                msg.contains("crash point"),
                "the import died on the crash point, not something else: {msg}"
            );
            // The atomicity assertions below run either way.
            check_import_rollback(&a);
            return;
        }
    };
    let report = outcome.expect("the import must crash, not fail cleanly");
    unreachable!("the crash point must fire before the ref move commits: {report:?}");
}

fn check_import_rollback(a: &Attached) {
    let conn = rusqlite::Connection::open(&a.db_path).unwrap();
    let main: i64 = conn
        .query_row("SELECT COUNT(*) FROM refs WHERE name = 'main'", [], |r| {
            r.get(0)
        })
        .unwrap();
    assert_eq!(main, 0, "the ref move rolled back with its audit row");
    let audits: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'ref_move'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 0, "no audit row for a rolled-back ref move");
}

// ---- blocker 1: the split read deadlock --------------------------------------

/// A concurrent `read_repo` (which uses the catalog inside the closure)
/// and `read_all` pass complete without hanging. Pre-fix these two take
/// the shared pooled connections in OPPOSITE orders (repo→cat vs cat→repo)
/// and deadlock on the std mutexes once both pool sides saturate: the pin
/// saturates them deliberately — 8 `read_repo` tasks grab the repo
/// connections and then need catalog ones while 4 `read_all` tasks hold
/// every catalog connection and need the repo ones — with a watchdog that
/// fails the run on a hang.
#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
async fn split_concurrent_read_repo_and_read_all_complete() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir;
    let state = open_split(&data_path);
    let _ = &dir;

    let stop = Arc::new(AtomicBool::new(false));
    let stop_w = Arc::clone(&stop);
    let watchdog = std::thread::spawn(move || {
        let started = Instant::now();
        while started.elapsed() < Duration::from_secs(45) {
            if stop_w.load(Ordering::Relaxed) {
                return;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        eprintln!("m13a-fix: read passes DEADLOCKED (watchdog)");
        std::process::exit(101);
    });

    for round in 0..3 {
        // First wave: read_repo tasks take the repo connections and then
        // need catalog ones.
        let mut repos = Vec::new();
        for _ in 0..8 {
            let s = Arc::clone(&state);
            repos.push(tokio::spawn(async move {
                s.read_repo("m13a-a", move |cx| {
                    let cat_hit = db::repo_id_by_name(&cx.cat(), "m13a-a")
                        .map_err(naatre_server::api_error::ApiError::log_internal)?;
                    let n = db::repo_changeset_count(cx.db, cx.id)
                        .map_err(naatre_server::api_error::ApiError::log_internal)?;
                    Ok((cat_hit, n))
                })
                .await
            }));
        }
        // A beat so the wave's repo connections are held before the
        // catalog side arrives.
        tokio::time::sleep(Duration::from_millis(10)).await;
        // Second wave: read_all tasks take the catalog connections (4 in
        // the pool — all of them) and then need the repo ones.
        let mut alls = Vec::new();
        for _ in 0..4 {
            let s = Arc::clone(&state);
            alls.push(tokio::spawn(async move {
                s.read_all(move |cx| {
                    let mut total = 0i64;
                    for repo_id in db::gc_repos(cx.cat())
                        .map_err(naatre_server::api_error::ApiError::log_internal)?
                    {
                        cx.with_repo(repo_id, |conn| {
                            total += db::repo_changeset_count(conn, repo_id)
                                .map_err(naatre_server::api_error::ApiError::log_internal)?;
                            Ok(())
                        })
                        .map_err(naatre_server::api_error::ApiError::log_internal)?;
                    }
                    Ok(total)
                })
                .await
            }));
        }
        for j in repos {
            j.await
                .expect("read_repo task joins")
                .expect("read_repo completes");
        }
        for j in alls {
            j.await
                .expect("read_all task joins")
                .expect("read_all completes");
        }
        let _ = round;
    }
    stop.store(true, Ordering::Relaxed);
    watchdog.join().expect("watchdog joins");
}

// ---- blocker 2: split-mode repo creation -------------------------------------

/// POST /v1/repos on a split store: the catalog's `db_file` column is NOT
/// NULL, so the route's plain INSERT 500s pre-fix. The route must answer
/// 201 and materialize the repo's file before the registry row commits.
#[tokio::test]
async fn split_repo_create_route_serves_201() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir;
    let state = open_split(&data_path);
    let app = naatre_server::router(Arc::clone(&state));
    let _ = &dir;

    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos",
        Some(&f.token),
        Some(serde_json::to_vec(&json!({ "name": "m13a-new" })).unwrap()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{body}");
    assert_eq!(body["name"], "m13a-new");

    // The registry names the file and the file EXISTS (create-then-read is
    // coherent — the blocker 4 shape).
    let cat = cat_conn(&data_path);
    let db_file: String = cat
        .query_row(
            "SELECT db_file FROM repos WHERE name = 'm13a-new'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(db_file, "repos/3.db", "{db_file}");
    assert!(
        data_path.join(&db_file).exists(),
        "the repo file is materialized: {}",
        data_path.join(&db_file).display()
    );

    // The created repo serves reads (a fresh repo has no refs yet — the
    // unborn-main shape; stats routes to its file).
    let (status, body) = send(
        &app,
        "GET",
        "/v1/repos/m13a-new/stats",
        Some(&f.token),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
}

/// Import's new-repo path against a split store: the same NOT NULL insert
/// pre-fix. The import must create the repo (with its db_file and file)
/// and import the git history.
#[tokio::test]
async fn split_import_new_repo_succeeds() {
    if !git_works() {
        return;
    }
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir;

    let git = data_path.parent().unwrap().join("git-src");
    let _ = std::fs::remove_dir_all(&git);
    std::fs::create_dir_all(&git).unwrap();
    run_git(&git, &["init", "-q", "--template="]); // no template copy: the fixture needs no hooks, and the copy races under full-suite parallelism
    std::fs::write(git.join("a.txt"), b"one\n").unwrap();
    run_git(&git, &["add", "."]);
    run_git(
        &git,
        &[
            "-c",
            "user.name=t",
            "-c",
            "user.email=t@x",
            "commit",
            "-qm",
            "one",
        ],
    );

    let opts = naatre_server::import::ImportOptions {
        git_path: git,
        into: Some("m13a-imp".to_string()),
        skip_unsupported: true,
        sanitize: false,
    };
    let rep =
        naatre_server::import::run_backend(&data_path, &split_cfg(), &opts, &Default::default())
            .unwrap_or_else(|e| panic!("split import into a NEW repo failed: {e}"));
    assert!(rep.repo_created, "{rep:?}");
    assert_eq!(rep.imported, 1, "{rep:?}");

    let cat = cat_conn(&data_path);
    let db_file: String = cat
        .query_row(
            "SELECT db_file FROM repos WHERE name = 'm13a-imp'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(db_file, "repos/3.db", "{db_file}");
    assert!(data_path.join(&db_file).exists());
    let _ = dir;
}

fn git_works() -> bool {
    match Command::new("git").arg("--version").output() {
        Ok(out) if out.status.success() => true,
        _ => {
            eprintln!("skipping: git not available");
            false
        }
    }
}

fn run_git(dir: &Path, args: &[&str]) {
    let out = Command::new("git")
        .args(args)
        .current_dir(dir)
        .output()
        .unwrap();
    assert!(
        out.status.success(),
        "git {args:?} failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
}

// ---- blocker 3: the CS-ledger phantom starvation ------------------------------

/// A page full of PHANTOM `cs` ledger rows (changesets already gone from
/// every repo — the crash-window shape) must not stall collection: the
/// existence-only reap clears them and the next pass collects the real
/// candidate behind them. Pre-fix the probes require the changeset row to
/// exist, the phantom page never re-enters a batch, and the real candidate
/// starves behind the `first_ns ASC` head forever.
#[tokio::test]
async fn split_cs_phantom_page_does_not_stall_collection() {
    let f = fixture().await;
    let state = Arc::clone(&f.state);
    let token = f.token.clone();
    let app = f.app.clone();

    // One REAL unreachable changeset (collectible garbage) in repo A.
    let _head = seed_orphan_cs(&app, &token).await;

    // 10,000 phantoms: ledger rows for ids no repo's changesets hold,
    // stamped OLDER than the real candidate (they take the page head).
    let phantoms: Vec<Id> = (0..10_000u32)
        .map(|i| {
            let mut b = [0u8; 32];
            b[0..4].copy_from_slice(&i.to_be_bytes());
            b[4] = 0xAA;
            Id(b)
        })
        .collect();
    let phantoms_for_write = phantoms.clone();
    state
        .write(move |store| {
            db::gc_candidates_upsert(
                store.conn(),
                db::GC_SCOPE_CS,
                &phantoms_for_write,
                BASE_NS as i64 - 40 * DAY_NS as i64,
            )
            .map_err(naatre_server::api_error::ApiError::log_internal)
        })
        .await
        .unwrap();

    // Pass 1: the phantom page is past grace; the real changeset gets its
    // ledger row now (too fresh to collect this pass).
    advance(&f, BASE_NS + GRACE + DAY_NS);
    let rep1 = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(rep1.cs_collected, 0, "the real cs is too fresh: {rep1:?}");
    let single = single_conn(&f.data_path);
    let ledgered: i64 = single
        .query_row(
            "SELECT COUNT(*) FROM gc_pending WHERE scope = 'cs'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(ledgered, 1, "only the real candidate remains: {ledgered}");

    // Pass 2: the real candidate is past grace — collected.
    advance(&f, BASE_NS + 2 * (GRACE + DAY_NS));
    let rep2 = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(
        rep2.cs_collected, 1,
        "the candidate behind the phantom page collects: {rep2:?}"
    );
    let ledgered: i64 = single
        .query_row(
            "SELECT COUNT(*) FROM gc_pending WHERE scope = 'cs'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(ledgered, 0, "the ledger drains: {ledgered}");
}

/// Crash between the per-repo deletes and the ledger delete: the stranded
/// ledger row is a phantom the probes never re-enter — the NEXT pass must
/// clear it (the existence-only reap). Pre-fix the row strands forever.
#[tokio::test]
async fn crash_between_cs_repo_delete_and_ledger_delete_converges() {
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("data");
    std::fs::create_dir_all(&dir).unwrap();
    let data = DataDir::create(&dir).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    {
        let f_seed = fixture_at(&dir).await;
        let app = f_seed.app.clone();
        let token = f_seed.token.clone();
        seed_orphan_cs(&app, &token).await;
        // The fixture state must drop before the child opens the files.
    }
    drop(data);

    let child = spawn_child(&dir, "gc-cs-crash", "gc_after_cs_repo_rows", None);
    let collected = wait_bounded(child);
    assert!(
        collected.status.success(),
        "the child must reach and honor the crash point: {}",
        collected.stderr
    );

    // Parent: reopen and converge. The stranded ledger row (the changeset
    // rows are already gone) must clear on the next pass.
    // The stranded row's first_ns is the child's pass stamp (BASE + 2
    // grace steps); the parent runs far enough past it for the reap.
    let a = attach(&dir, None, BASE_NS + 3 * (GRACE + DAY_NS));
    let conn = rusqlite::Connection::open(&a.db_path).unwrap();
    let stranded: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM gc_pending WHERE scope = 'cs'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(stranded, 1, "the crash stranded exactly the ledger row");
    let cs_rows: i64 = conn
        .query_row("SELECT COUNT(*) FROM changesets", [], |r| r.get(0))
        .unwrap();
    assert_eq!(cs_rows, 0, "the repo rows are already gone");

    let report = naatre_server::gc::gc_pass(&a.state).await.unwrap();
    assert_eq!(
        report.cs_collected, 0,
        "nothing left to collect: {report:?}"
    );
    let ledgered: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM gc_pending WHERE scope = 'cs'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        ledgered, 0,
        "the next pass clears the stranded ledger row (the phantom reap): {report:?}"
    );
}

/// The fixture pinned to an EXPLICIT dir (the crash pins' parent-side
/// seed). Self-sufficient: an absent naatre.db is created first.
async fn fixture_at(dir: &Path) -> Fixture {
    let data_path = dir.to_path_buf();
    if !data_path.join("naatre.db").exists() {
        let data = DataDir::create(&data_path).unwrap();
        std::fs::write(data.db_path(), b"").unwrap();
    }
    let data = DataDir::open(&data_path).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    let state = Arc::new(state);
    let (token, hash) = naatre_server::auth::mint_token();
    state
        .write({
            move |store| {
                let conn = store.conn();
                let tx = conn.transaction().unwrap();
                naatre_server::db::principal_upsert(&tx, "admin", "admin", BASE_NS as i64).unwrap();
                naatre_server::db::token_insert(&tx, "admin", &hash, None, None).unwrap();
                naatre_server::db::audit_append(&tx, BASE_NS as i64, "admin", "init", "{}")
                    .unwrap();
                tx.commit().unwrap();
                Ok(())
            }
        })
        .await
        .unwrap();
    for name in [REPO_A] {
        let repo_name = name.to_string();
        state
            .write(move |store| {
                let conn = store.conn();
                naatre_server::db::repos_create(conn, &repo_name, BASE_NS as i64).unwrap();
                Ok(())
            })
            .await
            .unwrap();
    }
    let app = naatre_server::router(Arc::clone(&state));
    Fixture {
        _dir: tempfile::tempdir().unwrap(),
        data_path,
        state,
        app,
        token,
        clock,
    }
}

// ---- blocker 4: hot-WAL recovery + the missing-file inconsistency -------------

/// A repo file left with a HOT WAL (frames uncheckpointed, the -shm lost —
/// the crashed-server shape) must read fine after boot: boot opens each
/// repo file read-write once and runs the recovery the read-only pool
/// cannot. Pre-fix the read pool's read-only open fails and the checkout
/// panics.
#[tokio::test]
async fn split_hot_wal_reads_after_boot() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let _dir = f._dir;

    // Simulate the crashed server: one raw read-write connection commits a
    // ref move into repos/1.db and is LEAKED (never closed), leaving WAL
    // frames; the -shm is then deleted so the next open must run recovery.
    let repo1 = data_path.join("repos/1.db");
    {
        let conn = rusqlite::Connection::open(&repo1).unwrap();
        conn.busy_timeout(Duration::from_millis(5000)).unwrap();
        conn.execute(
            "UPDATE refs SET version = 77, last_principal = 'hot-wal' \
             WHERE repo_id = 1 AND name = 'main'",
            [],
        )
        .unwrap();
        std::mem::forget(conn); // the crash: no clean close, no checkpoint
    }
    let _ = std::fs::remove_file(data_path.join("repos/1.db-shm"));
    let wal = data_path.join("repos/1.db-wal");
    let wal_bytes = std::fs::metadata(&wal).map(|m| m.len()).unwrap_or(0);

    // Boot the split state: recovery must run BEFORE any read-only open.
    let state = open_split(&data_path);
    let (status, body) = {
        let app = naatre_server::router(Arc::clone(&state));
        send(
            &app,
            "GET",
            "/v1/repos/m13a-a/refs/main",
            Some(&f.token),
            None,
        )
        .await
    };
    assert_eq!(
        status,
        StatusCode::OK,
        "the hot WAL reads after boot: {body}"
    );
    assert_eq!(
        body["version"], 77,
        "the recovered WAL frame's data serves: {body} (wal was {wal_bytes} bytes)"
    );
}

/// A registry row whose repo file is MISSING: reads answer an error (never
/// a panic), and writes REFUSE with the named inconsistency instead of
/// silently recreating an empty repo. Pre-fix the read path panics inside
/// the pool checkout and the write path silently recreates the file.
#[tokio::test]
async fn split_missing_repo_file_errors_not_panics() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir;
    let state = open_split(&data_path);
    let _ = &dir;

    let repo1 = data_path.join("repos/1.db");
    let _ = std::fs::remove_file(&repo1);
    let _ = std::fs::remove_file(data_path.join("repos/1.db-wal"));
    let _ = std::fs::remove_file(data_path.join("repos/1.db-shm"));

    // Read: an error (pre-fix this line PANICS — the checkout's .expect).
    let read = state
        .read_repo("m13a-a", move |cx| {
            db::repo_changeset_count(cx.db, cx.id)
                .map_err(naatre_server::api_error::ApiError::log_internal)
        })
        .await;
    assert!(
        read.is_err(),
        "a missing repo file answers an error: {read:?}"
    );

    // Write: the named refusal (pre-fix: Ok — the file is silently
    // recreated as an empty repo).
    let write = state.write_repo("m13a-a", move |_cx| Ok(())).await;
    assert!(
        write.is_err(),
        "repo_write_conn must refuse the vanished file, not recreate it: {write:?}"
    );

    // Boot with the file missing: refused loudly (the inconsistency is
    // named, not served).
    let e = {
        let cfg = split_cfg();
        check_data_dir_backend(&data_path, &cfg).ok();
        ServerState::open_backend(DataDir::open_any(&data_path), &cfg)
    };
    assert!(e.is_err(), "boot refuses a registry row without its file");
}

// ---- blocker 5: the phantom land audit row ------------------------------------

/// A split-mode land whose repo transaction never commits (the new
/// before-commit crash point) must leave NO audit row: the audit append
/// runs AFTER the commit (the documented order). Pre-fix the audit
/// autocommits on the aux catalog connection INSIDE the guard — the crash
/// fabricates a `land` audit row for a land that never stood.
#[tokio::test]
async fn split_rolled_back_land_leaves_no_audit_row() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir;
    let _ = &dir;

    let child = spawn_child(&data_path, "land-audit", "before_commit", Some(&f.token));
    let collected = wait_bounded(child);
    assert!(
        collected.status.success(),
        "the land child's pin failed: {}",
        collected.stderr
    );

    let cat = cat_conn(&data_path);
    let lands: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'land'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(lands, 0, "no audit row for a land that never stood");
    let _ = dir;
}

// ---- blocker 6: adoption crash-unsafety ---------------------------------------

/// A crash mid-copy (the fault-injected adoption) leaves a marker-less
/// catalog that the layout guard, `Store::open_catalog`, and a re-run all
/// REFUSE with the incomplete-migration text — never a "migrated already?"
/// invitation to serve a partial store. Pre-fix the crash leaves the same
/// marker-less catalog and every guard happily opens it.
#[tokio::test]
async fn split_adoption_crash_leaves_refused_catalog() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let _dir = f._dir;

    let child = spawn_child(&data_path, "adopt-copy", "adopt_mid_copy", None);
    let collected = wait_bounded(child);
    assert!(
        !collected.status.success(),
        "the fault-injected adoption must die mid-copy"
    );

    assert!(
        data_path.join("catalog.db").exists(),
        "the partial catalog remains"
    );
    assert!(data_path.join("naatre.db").exists(), "the original stands");

    let text = |e: &dyn std::fmt::Display| e.to_string();
    let e = check_data_dir_backend(&data_path, &split_cfg()).unwrap_err();
    assert!(
        text(&e).contains("incomplete migration"),
        "the layout guard refuses the marker-less catalog: {e}"
    );
    let e = naatre_store::Store::open_catalog(DataDir::open_any(&data_path)).unwrap_err();
    assert!(
        text(&e).contains("incomplete migration"),
        "open_catalog refuses the marker-less catalog: {e}"
    );
    let e =
        naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default()).unwrap_err();
    assert!(
        text(&e).contains("incomplete migration"),
        "the re-run refuses with the incomplete-migration text: {e}"
    );
}

/// The COMPLETE adoption (fsck passed, the marker written) still opens —
/// the marker check refuses only the incomplete shape.
#[tokio::test]
async fn complete_adoption_still_opens() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let cat = cat_conn(&data_path);
    let marker: String = cat
        .query_row("SELECT value FROM meta WHERE key = 'adopted'", [], |r| {
            r.get(0)
        })
        .expect("the complete adoption wrote the adopted marker");
    assert!(!marker.is_empty());
    let dir = f._dir;
    let state = open_split(&data_path);
    let app = naatre_server::router(Arc::clone(&state));
    let (status, body) = send(&app, "GET", "/v1/repos", Some(&f.token), None).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let _ = dir;
}

/// A pre-existing `repos/` refuses the adoption up front (a stale partial
/// target must not be silently overwritten by a re-run).
#[tokio::test]
async fn adoption_refuses_preexisting_repos_dir() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    std::fs::create_dir_all(data_path.join("repos")).unwrap();
    naatre_store::store::open_repo_db(&data_path.join("repos/9.db"))
        .expect("seed a valid stale repo file");
    let e =
        naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default()).unwrap_err();
    let text = e.to_string();
    assert!(
        text.contains("already exists"),
        "the pre-existing repos/ is refused: {text}"
    );
    assert!(data_path.join("naatre.db").exists(), "the original stands");
}

// ---- item 5, the failure shapes (fault-injected by trigger) -----------------

/// Single mode: the `land` audit row rides the land's transaction, so a
/// FAILING audit insert (the trigger aborts it) rolls the WHOLE land back
/// — the key stays unsettled, main unmoved, no audit row. The same-tx
/// observable for the atomicity half of the item-7 shape.
#[tokio::test]
async fn single_land_audit_failure_rolls_the_land_back() {
    let f = fixture().await;
    let app = f.app.clone();
    let token = f.token.clone();

    // The fault: audit_log refuses `land` inserts. Every other action
    // (changeset_create) passes, so the land reaches settle.
    {
        let conn = single_conn(&f.data_path);
        conn.execute_batch(
            "CREATE TRIGGER pin_fail_land_audit BEFORE INSERT ON audit_log \
             WHEN new.action = 'land' \
             BEGIN SELECT RAISE(ABORT, 'pin: land audit refused'); END;",
        )
        .unwrap();
    }

    let head = seed_orphan_cs(&app, &token).await;
    m4b_common::seed_main_at(&f.data_path.join("naatre.db"), 1, &head);
    let head_content = b"m13a-fix single land head".to_vec();
    let head_blob = put_blob(&app, &token, REPO_A, &head_content).await;
    let head2 = create_cs(
        &app,
        &token,
        REPO_A,
        cs_body(
            json!([head.as_str()]),
            json!([add("head.txt", &head_blob, head_content.len())]),
            "head",
        ),
    )
    .await;
    let (status, body) = send(
        &app,
        "POST",
        &format!("/v1/repos/{REPO_A}/land"),
        Some(&token),
        Some(serde_json::to_vec(&json!({ "head": head2, "key": "k1" })).unwrap()),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "the failed audit rolls the land back: {status} {body}"
    );

    // Same-transaction observable: NOTHING from the land stands.
    let conn = single_conn(&f.data_path);
    let lands: i64 = conn
        .query_row("SELECT COUNT(*) FROM lands", [], |r| r.get(0))
        .unwrap();
    assert_eq!(lands, 0, "the key stays unsettled");
    let audits: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'land'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 0, "no audit row for a rolled-back land");
    let main: Option<Vec<u8>> = conn
        .query_row(
            "SELECT target FROM refs WHERE repo_id = 1 AND name = 'main'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        main,
        Some(Id::from_hex(&head).unwrap().0.to_vec()),
        "main still points at the seeded target"
    );
}

/// Split mode: the land HAS STOOD when its post-commit audit insert fails
/// (the catalog trigger aborts it) — the route answers SUCCESS and logs
/// the loss; the lands row stands in the repo file and no audit row lies.
#[tokio::test]
async fn split_post_commit_audit_failure_still_answers_ok() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir;
    let state = open_split(&data_path);
    let app = naatre_server::router(Arc::clone(&state));
    let token = f.token.clone();

    // The fault: the catalog's audit_log refuses `land` inserts — exactly
    // the row the split-mode post-commit append writes.
    {
        let conn = cat_conn(&data_path);
        conn.execute_batch(
            "CREATE TRIGGER pin_fail_land_audit BEFORE INSERT ON audit_log \
             WHEN new.action = 'land' \
             BEGIN SELECT RAISE(ABORT, 'pin: land audit refused'); END;",
        )
        .unwrap();
    }

    let root_content = b"m13a-fix split land root".to_vec();
    let root_blob = put_blob(&app, &token, REPO_A, &root_content).await;
    let root = create_cs(
        &app,
        &token,
        REPO_A,
        cs_body(
            json!([]),
            json!([add("root.txt", &root_blob, root_content.len())]),
            "root",
        ),
    )
    .await;
    m4b_common::seed_main_at(&data_path.join("repos/1.db"), 1, &root);
    let head_content = b"m13a-fix split land head".to_vec();
    let head_blob = put_blob(&app, &token, REPO_A, &head_content).await;
    let head = create_cs(
        &app,
        &token,
        REPO_A,
        cs_body(
            json!([root.as_str()]),
            json!([add("head.txt", &head_blob, head_content.len())]),
            "head",
        ),
    )
    .await;
    let (status, body) = send(
        &app,
        "POST",
        &format!("/v1/repos/{REPO_A}/land"),
        Some(&token),
        Some(serde_json::to_vec(&json!({ "head": head, "key": "k1" })).unwrap()),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::OK,
        "the land answered success despite the audit loss: {status} {body}"
    );
    assert_eq!(body["landed"], head.as_str(), "{body}");

    // The land stood in the repo file; no audit row lies in the catalog.
    let repo_file = rusqlite::Connection::open(data_path.join("repos/1.db")).unwrap();
    let lands: i64 = repo_file
        .query_row("SELECT COUNT(*) FROM lands WHERE key = 'k1'", [], |r| {
            r.get(0)
        })
        .unwrap();
    assert_eq!(lands, 1, "the settled row stands");
    let cat = cat_conn(&data_path);
    let audits: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'land'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 0, "the lost audit row is absent, not fabricated");
    let _ = dir;
}

// ---- concern 7: single-DB audit atomicity --------------------------------------

/// Single mode: a ref move that crashes between the work and the audit
/// loses NOTHING — the audit rides the refs transaction (same connection),
/// so the crash rolls both back. Pre-fix the audit committed after the
/// refs transaction: the crash left a moved ref with no audit row.
#[tokio::test]
async fn single_ref_move_crash_is_atomic() {
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("data");
    std::fs::create_dir_all(&dir).unwrap();
    {
        // The seeded single-DB store (schema + repo + admin) the child
        // attaches to; the fixture state drops before the child opens.
        let _f = fixture_at(&dir).await;
    }

    let child = spawn_child(&dir, "ref-atomic", "ref_cas_before_audit", None);
    let collected = wait_bounded(child);
    assert!(
        collected.status.success(),
        "the ref atomicity pin failed: {}",
        collected.stderr
    );
}

/// Single mode: a redact mark that crashes between the tombstone commit
/// and the audit leaves NEITHER — the audit rides the mark's transaction.
#[tokio::test]
async fn single_redact_crash_is_atomic() {
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("data");
    std::fs::create_dir_all(&dir).unwrap();
    {
        let _f = fixture_at(&dir).await;
    }

    let child = spawn_child(&dir, "redact-atomic", "redact_before_audit", None);
    let collected = wait_bounded(child);
    assert!(
        collected.status.success(),
        "the redact atomicity pin failed: {}",
        collected.stderr
    );
}

/// Single mode: an import that crashes at its ref move (the
/// main-graft + ref_move audit pair) rolls the pair back together — the
/// changesets (earlier transactions) stand, `main` does not, and no
/// audit row lies about a graft that never stood.
#[tokio::test]
async fn single_import_crash_is_atomic() {
    if !git_works() {
        return;
    }
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("data");
    std::fs::create_dir_all(&dir).unwrap();
    {
        let _f = fixture_at(&dir).await;
    }

    let git = base.path().join("git-src");
    std::fs::create_dir_all(&git).unwrap();
    run_git(&git, &["init", "-q", "--template="]); // no template copy: the fixture needs no hooks, and the copy races under full-suite parallelism
    std::fs::write(git.join("a.txt"), b"one\n").unwrap();
    run_git(&git, &["add", "."]);
    run_git(
        &git,
        &[
            "-c",
            "user.name=t",
            "-c",
            "user.email=t@x",
            "commit",
            "-qm",
            "one",
        ],
    );

    let child = spawn_child(&dir, "import-atomic", "ref_cas_before_audit", None);
    let collected = wait_bounded(child);
    assert!(
        collected.status.success(),
        "the import atomicity pin failed: {}",
        collected.stderr
    );
}

// ---- concern 8: one gc audit row per pass ---------------------------------------

/// A >10,000-tree collection spans two coordinated batches and writes
/// exactly ONE `gc` audit row (the loop-ending batch's). Pre-fix every
/// full batch also wrote one — the pass rowed twice.
#[tokio::test]
async fn multi_batch_tree_collection_writes_one_gc_row() {
    let f = fixture().await;
    let state = Arc::clone(&f.state);

    // 10,001 orphan tree rows (no changeset root, no snapshot, no
    // junction) — garbage the reachability sweep owns.
    {
        let conn = single_conn(&f.data_path);
        for i in 0..10_001u32 {
            let mut id = [0u8; 32];
            id[0..4].copy_from_slice(&i.to_be_bytes());
            id[4] = 0xBB;
            conn.execute(
                "INSERT INTO trees(id, repo_id, canonical) VALUES (?1, 1, ?2)",
                rusqlite::params![&id[..], &b""[..]],
            )
            .unwrap();
        }
    }

    // Pass 1 at t1: ledger the trees; pass 2 past grace: collect.
    advance(&f, BASE_NS + GRACE + DAY_NS);
    let rep1 = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(rep1.trees_collected, 0, "{rep1:?}");
    advance(&f, BASE_NS + 2 * (GRACE + DAY_NS));
    let rep2 = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(rep2.trees_collected, 10_001, "{rep2:?}");

    let conn = single_conn(&f.data_path);
    let gc_rows: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'gc'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(gc_rows, 1, "one audit row per collecting pass: {gc_rows}");
    let trees: i64 = conn
        .query_row("SELECT COUNT(*) FROM trees", [], |r| r.get(0))
        .unwrap();
    assert_eq!(trees, 0, "every tree row collected");
}

// ---- concern 9: per-repo stats ---------------------------------------------------

/// Repo A's stats are unchanged when repo B tombstones content A shares —
/// in BOTH layouts (pre-fix the single-DB tombstone probe read the whole
/// store's redactions). The shared content is REFERENCED in both repos (a
/// committed changeset each — membership rides the changeset rows).
#[tokio::test]
async fn repo_stats_exclude_only_their_own_tombstones() {
    let f = fixture().await;
    let app = f.app.clone();
    let token = f.token.clone();

    // Shared content, uploaded and REFERENCED in both repos.
    let shared = b"m13a-fix shared content".to_vec();
    let blob = put_blob(&app, &token, "m13a-a", &shared).await;
    create_cs(
        &app,
        &token,
        "m13a-a",
        cs_body(
            json!([]),
            json!([add("shared.txt", &blob, shared.len())]),
            "shared-in-a",
        ),
    )
    .await;
    let _ = put_blob(&app, &token, "m13a-b", &shared).await;
    create_cs(
        &app,
        &token,
        "m13a-b",
        cs_body(
            json!([]),
            json!([add("shared.txt", &blob, shared.len())]),
            "shared-in-b",
        ),
    )
    .await;

    let stats = |app: &Router, token: &str, repo: &str| {
        let app = app.clone();
        let token = token.to_string();
        let repo = repo.to_string();
        async move {
            let (status, body) = send(
                &app,
                "GET",
                &format!("/v1/repos/{repo}/stats"),
                Some(&token),
                None,
            )
            .await;
            assert_eq!(status, StatusCode::OK, "{body}");
            body
        }
    };
    let before = stats(&app, &token, "m13a-a").await;

    // Tombstone the shared content in repo B only.
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13a-b/redactions",
        Some(&token),
        Some(serde_json::to_vec(&json!({ "id": blob, "kind": "blob", "reason": "pin" })).unwrap()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{body}");

    // B's own stats drop (its tombstone excludes its membership); A's do
    // not move.
    let after_b = stats(&app, &token, "m13a-b").await;
    assert!(
        after_b["bytes"].as_i64().unwrap() < before["bytes"].as_i64().unwrap(),
        "repo B's own tombstone trims B's stats: {before} vs {after_b}"
    );
    let after_single = stats(&app, &token, "m13a-a").await;
    assert_eq!(
        before["bytes"], after_single["bytes"],
        "single-DB: repo A's stats ignore repo B's tombstone ({before} vs {after_single})"
    );
    assert_eq!(before["objects"], after_single["objects"]);

    // The split: adopt and re-assert (the tombstone travels with B's file).
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir;
    let state = open_split(&data_path);
    let app_split = naatre_server::router(Arc::clone(&state));
    let after_split = stats(&app_split, &token, "m13a-a").await;
    assert_eq!(
        before["bytes"], after_split["bytes"],
        "split: repo A's stats ignore repo B's tombstone ({before} vs {after_split})"
    );
    let _ = dir;
}

// ---- concern 12: split-mode GC / purge / snapshot / fsck coverage -----------------

/// The plan's own check: gc_pass, redaction_purge_pass, gc_snapshots, and
/// fsck all run against a SPLIT store — the coordinated batches, the
/// cross-repo purge probe, and the audit-after-work ordering on the split
/// shapes.
#[tokio::test]
async fn split_gc_purge_snapshots_fsck_coverage() {
    let f = fixture().await;
    let token = f.token.clone();

    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let state = open_split_clk(&data_path, Arc::clone(&f.clock));
    let app = naatre_server::router(Arc::clone(&state));

    // fsck on the fresh split: clean.
    let fsck = naatre_server::fsck::fsck(&state).await.unwrap();
    assert!(fsck.clean(), "the fresh split fscks clean: {fsck:?}");

    // gc_pass over live data only: nothing collected, nothing ledgered.
    let rep = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(
        (rep.cs_collected, rep.blobs_collected, rep.trees_collected),
        (0, 0, 0),
        "live data survives the pass: {rep:?}"
    );

    // Orphan changeset + blob in repo A: two-pass collection across the
    // coordinated batches (cs first; its exclusive blob — plus the two
    // unreferenced fixture blobs — follows one pass later, and the
    // orphan's root tree dies with it).
    let head = seed_orphan_cs(&app, &token).await;
    let _ = head;
    advance(&f, BASE_NS + GRACE + DAY_NS);
    let rep1 = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(
        rep1.cs_collected, 0,
        "the orphan only ledgers at t1 (its first_ns is this pass's now): {rep1:?}"
    );
    assert_eq!(
        rep1.blobs_condemned, 2,
        "the two fixture blobs condemn in phase A: {rep1:?}"
    );
    advance(&f, BASE_NS + 2 * (GRACE + DAY_NS));
    let rep2 = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(
        rep2.cs_collected, 1,
        "the split cs batch collects at t2: {rep2:?}"
    );
    assert_eq!(
        rep2.blobs_collected, 2,
        "phase B collects the condemned fixture blobs: {rep2:?}"
    );
    assert_eq!(
        rep2.blobs_condemned, 0,
        "the orphan's blob is still spared by its dying tree's entries: {rep2:?}"
    );
    assert_eq!(
        rep2.trees_collected, 1,
        "the collected changeset's root tree dies with it: {rep2:?}"
    );
    // t3: the orphan's tree is gone, so its blob finally condemns (the
    // dying-tree spare at t2: a tree_entries row of a tree that is itself
    // dying spares the blob, which follows one phase later).
    advance(&f, BASE_NS + 3 * (GRACE + DAY_NS));
    let rep3 = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(
        rep3.blobs_condemned, 1,
        "the orphan's exclusive blob condemns once its tree is gone: {rep3:?}"
    );
    assert_eq!(rep3.blobs_collected, 0, "the condemn is phase A: {rep3:?}");
    advance(&f, BASE_NS + 4 * (GRACE + DAY_NS));
    let rep4 = naatre_server::gc::gc_pass(&state).await.unwrap();
    assert_eq!(
        rep4.blobs_collected, 1,
        "phase B reclaims the condemned orphan blob: {rep4:?}"
    );

    // Cross-repo purge probe on the split: the SAME content referenced by a
    // committed changeset in repo A survives repo B's tombstone purge.
    let shared = b"m13a-fix cross-repo content".to_vec();
    let shared_blob = put_blob(&app, &token, "m13a-a", &shared).await;
    let _ = put_blob(&app, &token, "m13a-b", &shared).await;
    let a_cs = create_cs(
        &app,
        &token,
        "m13a-a",
        cs_body(
            json!([]),
            json!([add("shared.txt", &shared_blob, shared.len())]),
            "shared-in-a",
        ),
    )
    .await;
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13a-b/redactions",
        Some(&token),
        Some(
            serde_json::to_vec(&json!({ "id": shared_blob, "kind": "blob", "reason": "pin" }))
                .unwrap(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{body}");
    advance(&f, BASE_NS + 3 * (GRACE + DAY_NS));
    let purge = naatre_server::gc::redaction_purge_pass(&state)
        .await
        .unwrap();
    assert_eq!(
        purge.blobs_purged, 0,
        "repo A's reference spares the blob: {purge:?}"
    );
    let (status, _) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13a-a/objects/{shared_blob}"),
        Some(&token),
        None,
    )
    .await;
    assert_eq!(
        status,
        StatusCode::OK,
        "repo A still serves the shared blob"
    );
    let (status, _) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13a-b/objects/{shared_blob}"),
        Some(&token),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::GONE, "repo B's tombstone still gates");
    let _ = a_cs;

    // An unshared tombstoned blob purges on the split, and the audit row
    // lands AFTER the work (one `redact_purge` row in the catalog).
    let solo = b"m13a-fix solo purge content".to_vec();
    let solo_blob = put_blob(&app, &token, "m13a-b", &solo).await;
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13a-b/redactions",
        Some(&token),
        Some(
            serde_json::to_vec(&json!({ "id": solo_blob, "kind": "blob", "reason": "pin" }))
                .unwrap(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{body}");
    let cat = cat_conn(&data_path);
    let audits_before: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'redact_purge'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    advance(&f, BASE_NS + 4 * (GRACE + DAY_NS));
    let purge = naatre_server::gc::redaction_purge_pass(&state)
        .await
        .unwrap();
    assert_eq!(
        purge.blobs_purged, 1,
        "the unshared tombstoned blob purges: {purge:?}"
    );
    let audits_after: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'redact_purge'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        audits_after,
        audits_before + 1,
        "one redact_purge audit row for the purging pass"
    );

    // gc_snapshots on the split: an expired snapshot row (repo A's file)
    // with its ephemeral dir purges; the audit row lands in the catalog.
    let snap_id: [u8; 32] = {
        let mut b = [0u8; 32];
        b[0] = 0xCC;
        b
    };
    let snap_hex = Id(snap_id).to_hex();
    let eph = data_path.join("ephemeral").join(&snap_hex);
    std::fs::create_dir_all(eph.join("objects")).unwrap();
    std::fs::write(eph.join("objects/x"), b"snapshot blob").unwrap();
    {
        let conn = rusqlite::Connection::open(data_path.join("repos/1.db")).unwrap();
        conn.execute(
            "INSERT INTO snapshots(id, repo_id, tree_id, principal, created_ns, expires_ns) \
             VALUES (?1, 1, ?1, 'admin', ?2, ?3)",
            rusqlite::params![
                &snap_id[..],
                (BASE_NS as i64) - DAY_NS as i64,
                (BASE_NS as i64) + 1, // expired long ago
            ],
        )
        .unwrap();
    }
    let snap = naatre_server::snapshots_gc::gc_snapshots(&state)
        .await
        .unwrap();
    assert_eq!(
        snap.expired, 1,
        "the expired snapshot purges on the split: {snap:?}"
    );
    assert_eq!(snap.rows_purged, 1, "{snap:?}");
    let audits: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'snapshot_gc'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 1, "the snapshot_gc audit row is in the catalog");

    // fsck on the post-collection split: still clean.
    let fsck = naatre_server::fsck::fsck(&state).await.unwrap();
    assert!(fsck.clean(), "the collected split fscks clean: {fsck:?}");
    let dir = f._dir;
    let _ = &dir;
}
