//! M6a suite: the generic derived-data framework and its queue. The plan's
//! pins live here: enqueue-on-write for every registered kind (create, land
//! — import is pinned in `import_test.rs`), the background queue deriving
//! parents first with the parents' outputs actually flowing (a child's
//! count is only reachable if its parent was done first), lease-expiry
//! re-derivation and crash-mid-derivation both idempotent, poison
//! quarantine with a report row whose descendants STILL derive (never
//! head-of-line blocks — the plan's exact words), the per-call WALK-BUDGET
//! and SIZE caps (a deriver that over-walks its budget and an over-cap
//! output each quarantine immediately, children still deriving) with the
//! advisor-named regression pin: a 1,100-commit linear chain derives fully
//! — long history is never capped, only per-call walks are —, eager-vs-
//! deferred per kind, enqueue never blocking a create (1k changesets with
//! the queue stopped), and the per-(kind, changeset) is-derived/is-pending
//! query the M6b read routes build the 503 `derivation_pending` contract
//! on.
//!
//! Test deriver: counts ancestors — output = sum(parent counts) + 1, so
//! every assertion below proves real data flow through the parents_done
//! channel, not just state transitions. Background behavior is observed
//! through the REAL serve task (10 ms interval); lease/poison/crash/cap
//! paths drive `derive_pass` directly on the injected clock (same pattern
//! as the M5a GC suite).

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use rusqlite::Connection;
use serde_json::{Value, json};
use tokio::net::TcpListener;
use tokio::sync::oneshot;
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::derive::{
    BusyAbort, DERIVE_MAX_ATTEMPTS, DERIVE_MAX_OUTPUT_BYTES, DeriveCtx, DeriveFailure,
    DeriveStatus, DerivedOut, Deriver, ParentOut, PoisonReason, derive_pass, status,
};
use naatre_server::serve::{ServeConfig, serve};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m6a";
const KIND: &str = "test-ancestors";

// ---- the test deriver ------------------------------------------------------

/// Ancestor counter: output = sum(parent outputs, empty = 0) + 1, in
/// 8-byte big-endian. Its own storage is a keyed upsert — the deriver half
/// of the idempotency contract the framework re-runs against.
struct CountingDeriver {
    name: &'static str,
    eager: bool,
    /// Fail THIS changeset on every attempt (the poison path); shared so
    /// the test can aim it after the target's id exists.
    fail_for: Arc<Mutex<Option<Id>>>,
    /// Attempt numbers ≤ this value panic (a crash while holding the
    /// lease). 0 disables.
    panic_on_attempt: usize,
    /// Panics AFTER the output upsert instead of before it (the
    /// crash-between-output-write-and-state-write shape).
    panic_after_write: bool,
    /// Every `derive` entry, in order — the derivation-order log.
    calls: Arc<Mutex<Vec<Id>>>,
}

impl Deriver for CountingDeriver {
    fn kind(&self) -> &'static str {
        self.name
    }

    fn eager(&self) -> bool {
        self.eager
    }

    fn derive(
        &self,
        _ctx: &DeriveCtx,
        conn: &naatre_store::Db,
        repo_id: i64,
        cs: Id,
        parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        self.calls.lock().unwrap().push(cs);
        if *self.fail_for.lock().unwrap() == Some(cs) {
            return Err(PoisonReason::new("boom: this node always fails").into());
        }
        if self.calls.lock().unwrap().len() <= self.panic_on_attempt && !self.panic_after_write {
            panic!("crash while holding the lease (before any write)");
        }
        let sum: u64 = parents_done.iter().map(|p| out_count(&p.output)).sum();
        let n = sum + 1;
        conn.execute(
            &naatre_store::Sql::plain(
                "CREATE TABLE IF NOT EXISTS derive_test_outputs(\
             repo_id INTEGER NOT NULL, cs BLOB NOT NULL, n INTEGER NOT NULL, \
             PRIMARY KEY(repo_id, cs))",
            ),
            &naatre_store::pv![],
        )
        .map_err(sql_poison)?;
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT OR REPLACE INTO derive_test_outputs(repo_id, cs, n) VALUES (?1, ?2, ?3)",
            ),
            &naatre_store::pv![repo_id, cs.0.as_slice(), n as i64],
        )
        .map_err(sql_poison)?;
        if self.calls.lock().unwrap().len() <= self.panic_on_attempt {
            panic!("crash after the output write, before the framework's done-record");
        }
        Ok(DerivedOut(n.to_be_bytes().to_vec()))
    }
}

/// A deriver that deliberately over-walks: charges its whole (own, tiny)
/// budget on the first call for its aimed-at changeset (the walk-budget
/// poison path), passes everything else through like the plain counter.
struct BudgetHog {
    hog_for: Arc<Mutex<Option<Id>>>,
}

/// The hog's per-kind budget — deliberately tiny so the test proves the
/// per-kind value binds (no shared constant to lean on).
const HOG_BUDGET: u64 = 100;

impl Deriver for BudgetHog {
    fn kind(&self) -> &'static str {
        "budget-hog"
    }

    fn walk_budget(&self) -> u64 {
        HOG_BUDGET
    }

    fn derive(
        &self,
        ctx: &DeriveCtx,
        _conn: &naatre_store::Db,
        _repo_id: i64,
        cs: Id,
        parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        if *self.hog_for.lock().unwrap() == Some(cs) {
            // Simulates a runaway walk: charge the entire budget.
            ctx.charge(HOG_BUDGET)?;
        }
        let n = parents_done
            .iter()
            .map(|p| out_count(&p.output))
            .sum::<u64>()
            + 1;
        Ok(DerivedOut((n as u8).to_be_bytes().to_vec()))
    }
}

fn sql_poison(e: naatre_store::DbError) -> PoisonReason {
    PoisonReason::new(e.to_string())
}

/// A deriver that fails while `armed` is set — the eager create-time Fail
/// shape, armable BEFORE the create (the eager derive runs inside the
/// create's own transaction, so the node's id is not knowable beforehand).
struct ArmedFailer {
    name: &'static str,
    armed: Arc<Mutex<bool>>,
}

impl Deriver for ArmedFailer {
    fn kind(&self) -> &'static str {
        self.name
    }

    fn eager(&self) -> bool {
        true
    }

    fn derive(
        &self,
        _ctx: &DeriveCtx,
        _conn: &naatre_store::Db,
        _repo_id: i64,
        _cs: Id,
        _parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        if *self.armed.lock().unwrap() {
            return Err(PoisonReason::new("boom: armed").into());
        }
        Ok(DerivedOut::empty())
    }
}

/// A stored output as its ancestor count (the empty marker reads as 0).
fn out_count(out: &DerivedOut) -> u64 {
    let b = out.as_bytes();
    if b.is_empty() {
        return 0;
    }
    u64::from_be_bytes(b.try_into().expect("8-byte count output"))
}

fn count_out(n: u64) -> DerivedOut {
    DerivedOut(n.to_be_bytes().to_vec())
}

fn id_of_hex(hex: &str) -> Id {
    Id::from_hex(hex).expect("64-hex id")
}

/// The deriver's shared knobs a test holds onto while the deriver itself
/// lives behind the registry's `Arc<dyn Deriver>`.
struct CounterHandles {
    /// Every `derive` entry, in order — the derivation-order log.
    calls: Arc<Mutex<Vec<Id>>>,
    /// The poison aim (see [`CountingDeriver::fail_for`]).
    fail_for: Arc<Mutex<Option<Id>>>,
}

/// A plain deferred ancestor counter with the given knobs.
fn counter(
    name: &'static str,
    eager: bool,
    panic_on_attempt: usize,
    panic_after_write: bool,
) -> (CountingDeriver, CounterHandles) {
    let calls = Arc::new(Mutex::new(Vec::new()));
    let fail_for = Arc::new(Mutex::new(None));
    (
        CountingDeriver {
            name,
            eager,
            fail_for: Arc::clone(&fail_for),
            panic_on_attempt,
            panic_after_write,
            calls: Arc::clone(&calls),
        },
        CounterHandles { calls, fail_for },
    )
}

// ---- harness ---------------------------------------------------------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
    state: AppState,
    clock: Arc<AtomicU64>,
}

/// `build_app` with a state tweak applied before the `Arc` wrap (deriver
/// registration and interval injection happen here).
fn build_app_with(tweak: impl FnOnce(&mut ServerState)) -> TestApp {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let dir = tempfile::tempdir().unwrap();
    let data = DataDir::create(dir.path()).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(dir.path()).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    tweak(&mut state);
    let db_path = dir.path().join("naatre.db");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    TestApp {
        _dir: dir,
        app,
        db_path,
        state,
        clock,
    }
}

struct Fixture {
    t: TestApp,
    writer: String,
}

fn fixture_with(tweak: impl FnOnce(&mut ServerState)) -> Fixture {
    let t = build_app_with(tweak);
    let writer = t.seed_token("writer", "writer", None);
    t.seed_repo(REPO);
    Fixture { t, writer }
}

impl TestApp {
    fn db(&self) -> naatre_store::Db {
        let conn = Connection::open(&self.db_path).unwrap();
        conn.busy_timeout(Duration::from_secs(5)).unwrap();
        naatre_store::Db::Sqlite(conn)
    }

    fn seed_token(&self, principal: &str, role: &str, scope_path: Option<&str>) -> String {
        let (token, hash) = naatre_server::auth::mint_token();
        let conn = self.db();
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO principals(name, role, created_ns) VALUES (?1, ?2, ?3) \
             ON CONFLICT(name) DO UPDATE SET role = excluded.role",
            ),
            &naatre_store::pv![principal, role, BASE_NS as i64],
        )
        .unwrap();
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO tokens(principal, blake3, expires_ns, revoked, scope_path) \
             VALUES (?1, ?2, NULL, 0, ?3)",
            ),
            &naatre_store::pv![principal, hash, scope_path],
        )
        .unwrap();
        token
    }

    fn seed_repo(&self, name: &str) -> i64 {
        let conn = self.db();
        conn.insert_id(
            &naatre_store::Sql::plain("INSERT INTO repos(name, created_ns) VALUES (?1, ?2)"),
            &naatre_store::pv![name, BASE_NS as i64],
        )
        .unwrap()
    }

    fn count(&self, sql: &str) -> i64 {
        self.db()
            .query_row_map(
                &naatre_store::Sql::plain_owned(sql.to_string()),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .expect("count query")
    }

    fn attempts_of(&self, cs: Id) -> i64 {
        self.db()
            .query_row_map(
                &naatre_store::Sql::plain(
                    "SELECT s.attempts FROM derive_state s \
                 JOIN derive_kinds k ON k.id = s.kind \
                 WHERE k.name = ?1 AND s.cs = ?2",
                ),
                &naatre_store::pv![KIND, cs.0.as_slice()],
                |r| r.get(0),
            )
            .expect("derive_state row")
    }
}

struct Resp {
    status: StatusCode,
    body: Value,
}

async fn send(
    app: &Router,
    method: &str,
    uri: &str,
    token: Option<&str>,
    body: Option<Value>,
) -> Resp {
    // The 503 `writer_busy` backpressure is retryable BY CONTRACT; in this
    // suite the queue's write transactions (10 ms injected interval) are
    // the contending writer, so the test client retries like the real
    // clients are told to (Retry-After).
    for attempt in 0..2000 {
        let resp = send_once(app, method, uri, token, body.clone()).await;
        let busy = resp.status == StatusCode::SERVICE_UNAVAILABLE
            && resp.body["error"]["code"] == "writer_busy";
        if !busy || attempt == 399 {
            return resp;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    unreachable!("retry loop always returns")
}

async fn send_once(
    app: &Router,
    method: &str,
    uri: &str,
    token: Option<&str>,
    body: Option<Value>,
) -> Resp {
    let mut builder = axum::http::Request::builder().method(method).uri(uri);
    if let Some(t) = token {
        builder = builder.header(header::AUTHORIZATION, format!("Bearer {t}"));
    }
    let req = builder
        .header(
            header::CONTENT_TYPE,
            if body.is_some() {
                "application/json"
            } else {
                "application/octet-stream"
            },
        )
        .body(match body {
            Some(v) => Body::from(v.to_string()),
            None => Body::empty(),
        })
        .unwrap();
    let res = app.clone().oneshot(req).await.expect("in-memory request");
    let status = res.status();
    let bytes = BodyExt::collect(res.into_body())
        .await
        .expect("body")
        .to_bytes()
        .to_vec();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Resp { status, body }
}

async fn post_json(app: &Router, uri: &str, token: &str, value: &Value) -> Resp {
    send(app, "POST", uri, Some(token), Some(value.clone())).await
}

/// Raw-bytes request (the object PUT); returns the status only. Retries
/// the contract's retryable 503 `writer_busy` (see `send`).
async fn send_bytes(
    app: &Router,
    method: &str,
    uri: &str,
    token: &str,
    payload: Vec<u8>,
) -> StatusCode {
    for attempt in 0..2000 {
        let req = axum::http::Request::builder()
            .method(method)
            .uri(uri)
            .header(header::AUTHORIZATION, format!("Bearer {token}"))
            .header(header::CONTENT_TYPE, "application/octet-stream")
            .body(Body::from(payload.clone()))
            .unwrap();
        let res = app.clone().oneshot(req).await.expect("in-memory request");
        let status = res.status();
        if status != StatusCode::SERVICE_UNAVAILABLE || attempt == 399 {
            return status;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    unreachable!("retry loop always returns")
}

/// Uploads one blob, returns its hex id.
async fn put_blob(f: &Fixture, content: &[u8]) -> String {
    let id = object_id(Kind::Blob, content).to_hex();
    let status = send_bytes(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/{REPO}/objects/{id}"),
        &f.writer,
        content.to_vec(),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "blob upload failed: {id}");
    id
}

/// Creates one single-file changeset; returns its hex id. `gen` participates
/// in the content (changeset ids cover parents + author + message + changes,
/// so callers must keep (name, gen) pairs unique).
async fn create_cs(f: &Fixture, parent: Option<&str>, name: &str, rev: u64) -> String {
    let content = format!("{name}-{rev}\n");
    let blob = put_blob(f, content.as_bytes()).await;
    let body = json!({
        "parents": parent.map(|p| vec![p.to_string()]).unwrap_or_default(),
        "author": "M6a Test <m6a@example.com>",
        "message": format!("create {name}"),
        "changes": [
            { "path": format!("{name}.txt"), "op": "add", "blob": blob, "size": content.len(), "exec": false }
        ],
    });
    let resp = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer,
        &body,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    resp.body["id"].as_str().expect("created id").to_string()
}

/// Lands `head` under `key`; asserts success and returns the landed id.
async fn land(f: &Fixture, head: &str, key: &str) -> String {
    let resp = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/land"),
        &f.writer,
        &json!({ "head": head, "key": key }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    resp.body["landed"].as_str().expect("landed id").to_string()
}

/// The framework status for the fixture's repo and kind.
async fn st(f: &Fixture, cs_hex: &str) -> Option<DeriveStatus> {
    status(&f.t.state, KIND, REPO, id_of_hex(cs_hex))
        .await
        .expect("status query")
}

/// Waits for the background queue to reach `expected` done rows —
/// PROGRESS-based, not a flat deadline. Every poll reads the done and
/// pending counts; a count that keeps changing proves the background task
/// is alive, so the wait continues however loaded the box is. A count
/// FROZEN for 30 s fails immediately, with the row states in the message —
/// a real queue stall surfaces fast and no larger cap can mask it.
///
/// Each poll also ADVANCES the injected clock (+50 ms): a lease lost to a
/// busy write slot (the queue aborts the pass rather than fight another
/// writer, and a pass can lose its lease inside itself) is recovered by
/// the sweep at lease expiry — on the queue's clock. A frozen test clock would make
/// that recovery impossible and turn one lost lease into an artificial
/// permanent stall (the flake this replaces: `deriving:1 pending:3`,
/// every node blocked behind one lost lease, no expiry ever possible).
/// Production clocks advance; the test now models that instead of an
/// impossible world.
async fn wait_queue_done(f: &Fixture, expected: i64) {
    let mut last: Option<(i64, i64)> = None;
    let mut stalled = 0u32;
    loop {
        f.t.clock.fetch_add(50_000_000, Ordering::Relaxed); // +50 ms per poll
        let done =
            f.t.count("SELECT COUNT(*) FROM derive_state WHERE state = 'done'");
        if done == expected {
            return;
        }
        let pending =
            f.t.count("SELECT COUNT(*) FROM derive_state WHERE state = 'pending'");
        let snapshot = (done, pending);
        if last == Some(snapshot) {
            stalled += 1;
        } else {
            stalled = 0;
        }
        last = Some(snapshot);
        assert!(
            stalled < 300, // 300 × 100 ms without ANY state change
            "derive queue stalled: done={done} pending={pending} (states: {})",
            f.t.db()
                .query_row_map(
                    &naatre_store::Sql::plain(
                        "SELECT COALESCE(group_concat(state || ':' || n, ' '), '<no rows>') \
                         FROM (SELECT state, COUNT(*) AS n FROM derive_state GROUP BY state)",
                    ),
                    &naatre_store::pv![],
                    |r| r.get::<String>(0),
                )
                .unwrap_or_else(|_| "<query failed>".into()),
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

// ---- caps: per-call walk budget + size ---------------------------------------

/// The advisor-named regression pin: LONG LINEAR HISTORY DERIVES FULLY.
/// A 1,100-commit first-parent chain (a repo shape the product targets)
/// runs through the queue with zero quarantines and correct outputs — the
/// old absolute-generation depth cap poisoned every node past generation
/// 1,000 here; the per-call walk budget never fires on it because each
/// node is its own queue batch. The budget guards one deriver CALL, not
/// history length.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn linear_chain_derives_to_the_end() {
    let (deriver, h) = counter(KIND, false, 0, false);
    let f = build_app_with(|state| {
        state.derive_queue_enabled = false; // derive_pass is driven directly
        state.register_deriver(Arc::new(deriver)).unwrap();
    });
    let repo_id = f.seed_repo(REPO);
    let kind_id: i64 = f
        .db()
        .query_row_map(
            &naatre_store::Sql::plain("SELECT id FROM derive_kinds WHERE name = ?1"),
            &naatre_store::pv![KIND],
            |r| r.get(0),
        )
        .unwrap();

    // Deterministic synthetic ids; d(0)..d(1100) chained first-parent.
    let d = |k: u64| {
        let mut b = [0u8; 32];
        b[..8].copy_from_slice(&k.to_be_bytes());
        b[8] = 0xDD;
        Id(b)
    };
    let conn = f.db();
    conn.execute_batch("BEGIN").unwrap();
    for k in 0..=1100u64 {
        seed_node(
            &conn,
            repo_id,
            kind_id,
            d(k),
            k as i64,
            k.checked_sub(1).map(d),
        );
    }
    conn.execute_batch("COMMIT").unwrap();

    let report = derive_pass(&f.state).await.expect("pass");
    assert_eq!(report.derived, 1101, "the whole chain derived");
    assert_eq!(report.poisoned, 0, "no quarantine on long linear history");
    assert_eq!(
        f.count("SELECT COUNT(*) FROM derive_reports"),
        0,
        "zero quarantine reports"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM derive_state WHERE state != 'done'"),
        0
    );
    // Outputs prove the parents-done flow through the full depth.
    assert_eq!(
        status(&f.state, KIND, REPO, d(0)).await.expect("status"),
        Some(DeriveStatus::Done(count_out(1)))
    );
    assert_eq!(
        status(&f.state, KIND, REPO, d(1100)).await.expect("status"),
        Some(DeriveStatus::Done(count_out(1101)))
    );
    assert_eq!(h.calls.lock().unwrap().len(), 1101);
}

/// The walk-budget cap: a deriver that over-walks its per-call budget is
/// quarantined IMMEDIATELY (no attempt burned — retrying the same node
/// would walk the same graph), rows a report naming the budget, and its
/// child still derives on the empty marker (never head-of-line blocks).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn walk_budget_exhaustion_poisons_immediately() {
    let hog_for = Arc::new(Mutex::new(None::<Id>));
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state
            .register_deriver(Arc::new(BudgetHog {
                hog_for: Arc::clone(&hog_for),
            }))
            .unwrap();
    });
    let hog_kind = "budget-hog";

    let parent = create_cs(&f, None, "hogged", 1).await;
    let child = create_cs(&f, Some(&parent), "after", 2).await;
    *hog_for.lock().unwrap() = Some(id_of_hex(&parent));

    let report = derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(report.poisoned, 1, "the over-walking node quarantined");
    assert_eq!(report.derived, 1, "only the child derived");

    assert_eq!(
        st_kind(&f, hog_kind, &parent).await,
        Some(DeriveStatus::Poison)
    );
    assert_eq!(
        f.t.count(&format!(
            "SELECT s.attempts FROM derive_state s JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = 'budget-hog' AND s.cs = x'{}'",
            parent
        )),
        0,
        "immediate poison burns no attempt"
    );
    let (reason, count): (String, i64) =
        f.t.db()
            .query_row_map(
                &naatre_store::Sql::plain("SELECT reason, COUNT(*) FROM derive_reports"),
                &naatre_store::pv![],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .unwrap();
    assert_eq!(count, 1, "one quarantine report");
    assert!(
        reason.contains("walk budget"),
        "report names the cap: {reason}"
    );

    // The child derived on the empty marker.
    assert_eq!(
        st_kind(&f, hog_kind, &child).await,
        Some(DeriveStatus::Done(DerivedOut(vec![1])))
    );
}

/// A deriver with an effectively unbounded budget that, once armed, jumps
/// the ctx's clock past the call's deadline window and keeps charging —
/// only the wall-clock backstop can stop this call. Disarmed, it derives
/// normally: the shape whose overrun must RETRY, not poison.
struct DeadlineHog {
    armed_for: Arc<Mutex<Option<Id>>>,
    now_ns: Arc<AtomicU64>,
}

impl Deriver for DeadlineHog {
    fn kind(&self) -> &'static str {
        "deadline-hog"
    }

    fn walk_budget(&self) -> u64 {
        u64::MAX
    }

    fn derive(
        &self,
        ctx: &DeriveCtx,
        _conn: &naatre_store::Db,
        _repo_id: i64,
        cs: Id,
        _parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        if *self.armed_for.lock().unwrap() == Some(cs) {
            self.now_ns.fetch_add(60_000_000_000, Ordering::Relaxed);
        }
        ctx.charge(1)?;
        Ok(DerivedOut(vec![1u8]))
    }
}

/// The wall-clock backstop's RETRY contract (the deadline fix): an
/// overrun is NOT the structural walk-budget poison — it fails the call,
/// RELEASES THE LEASE back to pending as a FAILED ATTEMPT (attempts+1),
/// and rows a DISTINCT reason carrying the measured units/sec; the next
/// pass re-derives the node and records done. The deadline window is
/// injected (60 s of fake clock in one step against a 1 ms window) so the
/// whole retry runs deterministically on the fixed clock.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn walk_deadline_overrun_releases_the_lease_and_retries_to_done() {
    let armed_for = Arc::new(Mutex::new(None::<Id>));
    let now_ns = Arc::new(AtomicU64::new(BASE_NS));
    let f = fixture_with(|state| {
        let n = Arc::clone(&now_ns);
        state.clock = Arc::new(move || n.load(Ordering::Relaxed));
        state.derive_queue_interval = Duration::from_millis(10);
        // The injected (tiny) deadline window: one charge inside it fails
        // as soon as the armed hog's 60 s clock jump lands.
        state.derive_walk_deadline = Duration::from_millis(1);
        state
            .register_deriver(Arc::new(DeadlineHog {
                armed_for: Arc::clone(&armed_for),
                now_ns: Arc::clone(&now_ns),
            }))
            .unwrap();
    });
    let hog_kind = "deadline-hog";

    let target = create_cs(&f, None, "deadline", 1).await;
    *armed_for.lock().unwrap() = Some(id_of_hex(&target));

    // Pass 1: the overrun — a failed attempt, NOT a quarantine.
    let report = derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(report.poisoned, 0, "an overrun is retryable, not poison");
    assert_eq!(report.deadline_overruns, 1, "the overrun is counted");
    assert_eq!(
        st_kind(&f, hog_kind, &target).await,
        Some(DeriveStatus::Pending),
        "the lease is released: back to pending"
    );
    assert_eq!(
        f.t.count(&format!(
            "SELECT s.attempts FROM derive_state s JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = '{hog_kind}' AND s.cs = x'{}'",
            target
        )),
        1,
        "the overrun burns ONE attempt (bounded retries)"
    );
    let (reason, count): (String, i64) =
        f.t.db()
            .query_row_map(
                &naatre_store::Sql::plain("SELECT reason, COUNT(*) FROM derive_reports"),
                &naatre_store::pv![],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .unwrap();
    assert_eq!(count, 1, "one report row");
    assert!(
        reason.contains("walk deadline") && reason.contains("units/s"),
        "the report carries the DISTINCT reason with the measured \
         units/sec: {reason}"
    );

    // Pass 2 (disarmed): the retry re-derives and records done.
    *armed_for.lock().unwrap() = None;
    let report = derive_pass(&f.t.state).await.expect("second pass");
    assert_eq!(report.derived, 1, "the retry completed the node");
    assert_eq!(
        st_kind(&f, hog_kind, &target).await,
        Some(DeriveStatus::Done(DerivedOut(vec![1]))),
        "eventually done — the overrun never poisoned"
    );
}

/// A deriver that spins charging (with an effectively unbounded budget)
/// until the ctx stops it, with a spin bound far past the deadline: the
/// real production clock advances while it charges, so the ~2 s deadline
/// fires. If the deadline check regresses away, the spin runs to its
/// bound, the node derives done, and the pin fails.
struct RunawayHog {
    armed_for: Arc<Mutex<Option<Id>>>,
}

impl Deriver for RunawayHog {
    fn kind(&self) -> &'static str {
        "runaway-hog"
    }

    fn walk_budget(&self) -> u64 {
        u64::MAX
    }

    fn derive(
        &self,
        ctx: &DeriveCtx,
        _conn: &naatre_store::Db,
        _repo_id: i64,
        cs: Id,
        _parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        if *self.armed_for.lock().unwrap() == Some(cs) {
            // Far more charges than the deadline window can span; every
            // charge re-reads the state clock.
            for _ in 0..1_000_000_000u64 {
                ctx.charge(1)?;
            }
        }
        Ok(DerivedOut::empty())
    }
}

/// The wall-clock backstop, real-clock half: a runaway walk with budget
/// left is stopped by the ~2 s deadline on the production clock — the
/// call fails while it is still spinning, as a RETRYABLE overrun (the
/// lease is released, one attempt burned, the measured reason rowed), so
/// a mis-sized budget cannot hold the write slot past the deadline but a
/// legitimate node is never structurally quarantined for slowness. (The
/// fixture's default clock is frozen, so the production clock is
/// installed explicitly.)
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn wall_clock_deadline_stops_a_real_runaway_walk() {
    let armed_for = Arc::new(Mutex::new(None::<Id>));
    let f = fixture_with(|state| {
        state.clock = naatre_server::state::system_clock();
        state.derive_queue_interval = Duration::from_millis(10);
        state
            .register_deriver(Arc::new(RunawayHog {
                armed_for: Arc::clone(&armed_for),
            }))
            .unwrap();
    });
    let hog_kind = "runaway-hog";

    let target = create_cs(&f, None, "runaway", 1).await;
    *armed_for.lock().unwrap() = Some(id_of_hex(&target));

    let report = derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(
        report.deadline_overruns, 1,
        "the deadline stopped the runaway walk on the real clock"
    );
    assert_eq!(report.poisoned, 0, "an overrun retries, it does not poison");
    assert_eq!(
        st_kind(&f, hog_kind, &target).await,
        Some(DeriveStatus::Pending),
        "released back to pending for the retry"
    );
    assert_eq!(
        f.t.count(&format!(
            "SELECT s.attempts FROM derive_state s JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = '{hog_kind}' AND s.cs = x'{}'",
            target
        )),
        1,
        "one attempt burned"
    );
    let (reason, count): (String, i64) =
        f.t.db()
            .query_row_map(
                &naatre_store::Sql::plain("SELECT reason, COUNT(*) FROM derive_reports"),
                &naatre_store::pv![],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .unwrap();
    assert_eq!(count, 1, "one report row");
    assert!(
        reason.contains("walk deadline") && reason.contains("units/s"),
        "the measured reason: {reason}"
    );
    // The measurement itself is the point of this pin: print the real
    // clock's charged-walk rate (visible under --nocapture) so the budget
    // values stay calibrated against actual probe cost.
    println!("walk-deadline measurement: {reason}");
}

/// A minimal deriver for the size-cap pin: returns an over-cap output for
/// its aimed-at changeset, a parent-count byte for everything else.
struct HugeOutDeriver {
    huge_for: Arc<Mutex<Option<Id>>>,
    calls: Arc<Mutex<Vec<Id>>>,
}

impl Deriver for HugeOutDeriver {
    fn kind(&self) -> &'static str {
        "huge-out"
    }

    fn derive(
        &self,
        _ctx: &DeriveCtx,
        _conn: &naatre_store::Db,
        _repo_id: i64,
        cs: Id,
        parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        self.calls.lock().unwrap().push(cs);
        if *self.huge_for.lock().unwrap() == Some(cs) {
            return Ok(DerivedOut(vec![0u8; DERIVE_MAX_OUTPUT_BYTES + 1]));
        }
        Ok(DerivedOut(vec![(parents_done.len() + 1) as u8]))
    }
}

/// Seeds one synthetic changeset (dummy canonical — the depth gate and the
/// test derivers never read it) plus its pending `derive_state` row for the
/// registered kind; `parent` becomes the single first parent (ord 0).
fn seed_node(
    conn: &naatre_store::Db,
    repo_id: i64,
    kind_id: i64,
    id: Id,
    generation: i64,
    parent: Option<Id>,
) {
    conn.execute(&naatre_store::Sql::plain(
"INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns, \
         generation, principal, root_tree_id) VALUES (?1, ?2, x'00', 'seed', 'seed', 0, ?3, 'seed', NULL)"),
        &naatre_store::pv![repo_id, id.0.as_slice(), generation],
    )
    .unwrap();
    if let Some(p) = parent {
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO changeset_parents(repo_id, cs, parent, ord) VALUES (?1, ?2, ?3, 0)",
            ),
            &naatre_store::pv![repo_id, id.0.as_slice(), p.0.as_slice()],
        )
        .unwrap();
    }
    conn.execute(
        &naatre_store::Sql::plain(
            "INSERT INTO derive_state(kind, repo_id, cs, state) VALUES (?1, ?2, ?3, 'pending')",
        ),
        &naatre_store::pv![kind_id, repo_id, id.0.as_slice()],
    )
    .unwrap();
}

/// The framework status under an explicit kind name (the size-cap pin runs
/// its own deriver kind).
async fn st_kind(f: &Fixture, kind: &str, cs_hex: &str) -> Option<DeriveStatus> {
    status(&f.t.state, kind, REPO, id_of_hex(cs_hex))
        .await
        .expect("status query")
}

/// The size cap: a deriver's over-cap output is never recorded — the node
/// is quarantined immediately with the size reason (no attempt burned),
/// and its child still derives on the empty marker.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn size_cap_poisons_oversized_outputs() {
    let calls = Arc::new(Mutex::new(Vec::new()));
    let huge_for = Arc::new(Mutex::new(None));
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state
            .register_deriver(Arc::new(HugeOutDeriver {
                huge_for: Arc::clone(&huge_for),
                calls: Arc::clone(&calls),
            }))
            .unwrap();
    });

    let huge_kind = "huge-out";

    let parent = create_cs(&f, None, "huge", 1).await;
    let child = create_cs(&f, Some(&parent), "after", 2).await;
    *huge_for.lock().unwrap() = Some(id_of_hex(&parent));

    let report = derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(report.poisoned, 1, "the over-cap node quarantined");
    assert_eq!(report.derived, 1, "only the child derived");

    assert_eq!(
        st_kind(&f, huge_kind, &parent).await,
        Some(DeriveStatus::Poison)
    );
    assert_eq!(
        f.t.count(&format!(
            "SELECT s.attempts FROM derive_state s JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = 'huge-out' AND s.cs = x'{}'",
            parent
        )),
        0,
        "immediate poison burns no attempt"
    );
    let (reason, count): (String, i64) =
        f.t.db()
            .query_row_map(
                &naatre_store::Sql::plain("SELECT reason, COUNT(*) FROM derive_reports"),
                &naatre_store::pv![],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .unwrap();
    assert_eq!(count, 1, "one quarantine report");
    assert!(
        reason.contains("output cap"),
        "report names the cap: {reason}"
    );
    assert_eq!(
        f.t.count(&format!(
            "SELECT COUNT(*) FROM derive_state s JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = 'huge-out' AND s.cs = x'{}' AND s.output IS NULL",
            parent
        )),
        1,
        "the over-cap output was never recorded"
    );

    // The child derived on the empty marker: one parent (the poisoned
    // head, contributing nothing) — the probe's output is parent-count
    // based, so [2].
    assert_eq!(
        st_kind(&f, huge_kind, &child).await,
        Some(DeriveStatus::Done(DerivedOut(vec![2])))
    );
    assert_eq!(calls.lock().unwrap().len(), 2);
}

// ---- a. queue lifecycle (background task, parents first) --------------------

/// Creates via the API, then starts a serving process with a 10 ms injected
/// queue interval: pending rows appear for the test kind with zero manual
/// passes, the background task derives them parents-first, and the counts
/// prove the parents' outputs actually flowed (a child can only reach 2/3
/// if its parent was DONE when it derived — an empty-marker run reads 1).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn queue_lifecycle_derives_parents_first() {
    let (deriver, h) = counter(KIND, false, 0, false);
    // The derive queue is the only background task: the reachability GC,
    // the boot backfill and the snapshot GC are off. The creates run
    // BEFORE `serve` starts, so no background pass can hold the write slot
    // while they commit (a create meeting a held slot gets the correct,
    // retryable 503 `writer_busy`, which failed this test's sibling on a
    // slow CI runner). The commits enqueue the rows; `serve` then derives
    // them with zero manual passes. This test deliberately leaves out a
    // commit arriving while a pass runs; the direct `derive_pass`
    // writer-busy tests below cover that interleaving.
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state.gc_enabled = false;
        state.derive_backfill_enabled = false;
        state.snapshot_gc_enabled = false;
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;
    let c1 = create_cs(&f, Some(&r1), "c1", 2).await;
    let c2 = create_cs(&f, Some(&c1), "c2", 3).await;
    let x = create_cs(&f, Some(&r1), "branch", 4).await;

    // Queue rows exist for the kind as soon as the creates commit.
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state"),
        4,
        "one queue row per created changeset"
    );

    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let (send_shutdown, recv_shutdown) = oneshot::channel::<()>();
    let serve_task = tokio::spawn(serve(
        ServeConfig {
            listener,
            state: Arc::clone(&f.t.state),
        },
        async move {
            let _ = recv_shutdown.await;
        },
    ));

    wait_queue_done(&f, 4).await;

    // Derivation order respected parents: root before both children, c1
    // before its child c2.
    let order = h.calls.lock().unwrap().clone();
    let pos = |id: &str| {
        order
            .iter()
            .position(|c| *c == id_of_hex(id))
            .unwrap_or_else(|| panic!("never derived: {id}"))
    };
    assert!(pos(&r1) < pos(&c1), "root must derive before c1: {order:?}");
    assert!(pos(&c1) < pos(&c2), "c1 must derive before c2: {order:?}");
    assert!(
        pos(&r1) < pos(&x),
        "root must derive before the branch: {order:?}"
    );

    // The outputs prove parents-done flow, not just ordering.
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(st(&f, &c1).await, Some(DeriveStatus::Done(count_out(2))));
    assert_eq!(st(&f, &c2).await, Some(DeriveStatus::Done(count_out(3))));
    assert_eq!(st(&f, &x).await, Some(DeriveStatus::Done(count_out(2))));
    // First-try derivations burn no attempts.
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE attempts = 0"),
        4
    );

    send_shutdown.send(()).expect("serve task alive");
    tokio::time::timeout(Duration::from_secs(5), serve_task)
        .await
        .expect("serve shut down within 5 s")
        .expect("join")
        .expect("serve ok");
}

// ---- b. lease expiry ---------------------------------------------------------

/// A deriver that crashes on its first attempt leaves the lease held; the
/// sweep expires it (one attempt burned), the re-derivation runs from
/// scratch, and the recorded output is identical to the (pure)
/// recomputation — the plan's "lease-expiry re-derivation … idempotent".
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn lease_expiry_rederives_idempotently() {
    let (deriver, h) = counter(KIND, false, 1, false);
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Pending));

    // Attempt 1 crashes while holding the lease: the pass fails, the row
    // stays 'deriving' with the lease set and no attempt burned yet.
    let res = derive_pass(&f.t.state).await;
    assert!(res.is_err(), "the crashed pass must surface as an error");
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Deriving));
    assert_eq!(f.t.attempts_of(id_of_hex(&r1)), 0);

    // Past the lease (2 × 10 ms), the next pass sweeps and re-derives.
    f.t.clock.store(BASE_NS + 100_000_000, Ordering::Relaxed);
    let report = derive_pass(&f.t.state).await.expect("recovery pass");
    assert_eq!(report.swept, 1, "exactly the crashed lease");
    assert_eq!(report.derived, 1);

    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(f.t.attempts_of(id_of_hex(&r1)), 1, "one expiry burned");
    assert_eq!(
        h.calls.lock().unwrap().len(),
        2,
        "derived twice, once per attempt"
    );
}

/// A pass aborted by write-slot contention — the `writer_busy` try_lock
/// failure INSIDE the pass, here the test hook's one-shot injection at the
/// mid-derivation write — must RELEASE its lease straight back to pending
/// WITHOUT burning an attempt: the abort is contention, not a derivation
/// failure. Regression pin (fix6): the queue_lifecycle flake was an
/// ATTEMPTS-ACCOUNTING failure, not a node failing to derive —
/// `wait_queue_done(4)` had passed and all four nodes derived; the
/// captured `left: 3, right: 4` was the `attempts = 0` COUNT assertion,
/// one node having burned an attempt via lease expiry after a busy
/// collision (the old behavior held the lease to expiry, so the sweep
/// burned an attempt on pure contention and each collision cost a full
/// lease period). Here the chain completes in two passes — one lost to
/// the abort, one re-picking the released node — never a lease period.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn writer_busy_abort_releases_the_lease_without_burning_an_attempt() {
    let (deriver, h) = counter(KIND, false, 0, false);
    let f = fixture_with(|state| {
        state.derive_queue_enabled = false; // derive_pass driven directly
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;
    let c1 = create_cs(&f, Some(&r1), "c1", 2).await;
    let c2 = create_cs(&f, Some(&c1), "c2", 3).await;

    // Arm the one-shot hook at the root's mid-derivation write: the pass
    // leases the root, then loses the write slot exactly as it would to a
    // colliding API write.
    *f.t.state.derive_test_busy_abort.lock().unwrap() = Some(BusyAbort::Derive(id_of_hex(&r1)));

    let report = derive_pass(&f.t.state).await.expect("busy pass still Ok");
    assert_eq!(report.derived, 0, "the abort ended the pass early");
    assert_eq!(
        st(&f, &r1).await,
        Some(DeriveStatus::Pending),
        "the lease went straight back to pending"
    );
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE lease_expires_ns IS NOT NULL"),
        0,
        "no lease left held"
    );
    assert_eq!(
        f.t.attempts_of(id_of_hex(&r1)),
        0,
        "contention burns no attempt"
    );

    // The next tick re-picks the released node and the whole chain
    // completes — two passes total, no lease period burned.
    let report = derive_pass(&f.t.state).await.expect("recovery pass");
    assert_eq!(report.derived, 3, "the whole chain derived");
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(st(&f, &c1).await, Some(DeriveStatus::Done(count_out(2))));
    assert_eq!(st(&f, &c2).await, Some(DeriveStatus::Done(count_out(3))));
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE attempts = 0"),
        3,
        "no node burned an attempt"
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM derive_reports"), 0);
    assert_eq!(
        h.calls.lock().unwrap().len(),
        3,
        "the aborted write never ran the deriver"
    );
}

/// The contention/real-failure contrast, same pass machinery: three
/// CONSECUTIVE busy aborts must NOT poison the node — the release burns no
/// attempt, so contention however repeated can never reach
/// [`DERIVE_MAX_ATTEMPTS`], and it derives on the next tick — while three
/// REAL derive errors still DO quarantine at the cap (attempts 3 → poison
/// + report; pre-fix each abort held the lease to expiry and the sweep
/// burned one attempt, so three collisions quarantined a healthy node
/// outright). The contrast leg needs a still-pending node — the busy leg
/// leaves its node DONE, and done is terminal — so it runs on a fresh
/// child.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn three_writer_busy_aborts_never_poison_but_three_real_errors_do() {
    let (deriver, h) = counter(KIND, false, 0, false);
    let f = fixture_with(|state| {
        state.derive_queue_enabled = false; // derive_pass driven directly
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;

    // Three passes, each aborted at the root's mid-derivation write (the
    // hook re-arms per pass — one shot per arm).
    for _ in 0..3 {
        *f.t.state.derive_test_busy_abort.lock().unwrap() = Some(BusyAbort::Derive(id_of_hex(&r1)));
        let report = derive_pass(&f.t.state).await.expect("busy pass still Ok");
        assert_eq!(report.derived, 0, "the abort ended the pass early");
        assert_eq!(
            st(&f, &r1).await,
            Some(DeriveStatus::Pending),
            "released straight back to pending, every time"
        );
    }
    assert_eq!(
        f.t.attempts_of(id_of_hex(&r1)),
        0,
        "contention burns no attempt, even three times"
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM derive_reports"), 0);
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE state = 'poison'"),
        0
    );

    // Contention never touched it: the next tick derives normally.
    let report = derive_pass(&f.t.state).await.expect("recovery pass");
    assert_eq!(report.derived, 1);
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(f.t.attempts_of(id_of_hex(&r1)), 0);
    assert_eq!(
        h.calls.lock().unwrap().len(),
        1,
        "only the real pass ran the deriver"
    );

    // The contrast: a REAL derive failure on a still-pending node — one
    // pass burns all three attempts (it stays the oldest eligible) and
    // quarantines with a report.
    let c1 = create_cs(&f, Some(&r1), "child", 2).await;
    *h.fail_for.lock().unwrap() = Some(id_of_hex(&c1));
    let report = derive_pass(&f.t.state).await.expect("failing pass");
    assert_eq!(report.derived, 0);
    assert_eq!(report.poisoned, 1, "three real errors quarantine the node");
    assert_eq!(st(&f, &c1).await, Some(DeriveStatus::Poison));
    assert_eq!(f.t.attempts_of(id_of_hex(&c1)), DERIVE_MAX_ATTEMPTS);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM derive_reports"), 1);
}

/// The release write's OWN busy path: the pass aborts at the
/// mid-derivation write AND the first release attempt loses the slot race
/// too ([`BusyAbort::DeriveThenRelease`]) — the release helper's
/// busy-retry takes the second attempt and the lease still lands back on
/// pending with no attempt burned, nothing for the expiry sweep to touch.
/// (Residual, stated for the record: a writer holding the slot
/// CONTINUOUSLY past `LEASE_RELEASE_BUSY_RETRIES` × 10 ms makes the
/// release give up — the expiry sweep then burns one attempt per lease
/// period, so three full lease periods of uninterrupted slot loss to
/// poison a node. The give-up arm itself is not pinned: injecting 100
/// consecutive losses needs a slot holder outliving the whole retry
/// window.)
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn busy_first_release_attempt_retries_and_still_burns_no_attempt() {
    let (deriver, h) = counter(KIND, false, 0, false);
    let f = fixture_with(|state| {
        state.derive_queue_enabled = false; // derive_pass driven directly
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;

    // Arm the double collision: the root's mid-derivation write aborts,
    // and the release's first attempt is busy right behind it.
    *f.t.state.derive_test_busy_abort.lock().unwrap() =
        Some(BusyAbort::DeriveThenRelease(id_of_hex(&r1)));

    let report = derive_pass(&f.t.state)
        .await
        .expect("double-busy pass still Ok");
    assert_eq!(report.derived, 0, "the abort ended the pass early");
    assert_eq!(
        st(&f, &r1).await,
        Some(DeriveStatus::Pending),
        "the retried release still put the row back to pending"
    );
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE lease_expires_ns IS NOT NULL"),
        0,
        "no lease left held"
    );
    assert_eq!(
        f.t.attempts_of(id_of_hex(&r1)),
        0,
        "two collisions, still no attempt burned"
    );
    assert!(
        f.t.state.derive_test_busy_abort.lock().unwrap().is_none(),
        "both one-shot shots fired"
    );

    // The next tick derives it normally.
    let report = derive_pass(&f.t.state).await.expect("recovery pass");
    assert_eq!(report.derived, 1);
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(f.t.attempts_of(id_of_hex(&r1)), 0);
    assert_eq!(
        h.calls.lock().unwrap().len(),
        1,
        "neither aborted write ran the deriver"
    );
}

// ---- c. poison quarantine, never head-of-line --------------------------------

/// A deriver that returns Err every time is quarantined after
/// [`DERIVE_MAX_ATTEMPTS`] attempts with a `derive_reports` row — and its
/// child STILL derives (empty-marker fallback), so a malformed node never
/// head-of-line blocks (the plan's exact words).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn poison_quarantines_but_children_still_derive() {
    let (deriver, h) = counter(KIND, false, 0, false);
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let p = create_cs(&f, None, "poisoned", 1).await;
    let k = create_cs(&f, Some(&p), "child", 2).await;

    // Aim the deriver at p (its id exists only after the create).
    *h.fail_for.lock().unwrap() = Some(id_of_hex(&p));

    // One pass: p burns all three attempts (pick is oldest-first and it
    // stays eligible), quarantines, and THEN k becomes eligible and derives.
    let report = derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(report.derived, 1, "only the child derived");
    assert_eq!(report.poisoned, 1, "the failing node quarantined");

    assert_eq!(st(&f, &p).await, Some(DeriveStatus::Poison));
    assert_eq!(f.t.attempts_of(id_of_hex(&p)), DERIVE_MAX_ATTEMPTS);
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_reports"),
        1,
        "one quarantine report"
    );
    let (reason, ts): (String, i64) =
        f.t.db()
            .query_row_map(
                &naatre_store::Sql::plain("SELECT reason, ts_ns FROM derive_reports"),
                &naatre_store::pv![],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .unwrap();
    assert!(
        reason.contains("boom"),
        "report carries the deriver's reason: {reason}"
    );
    assert_eq!(ts, BASE_NS as i64);

    // The plan's never-head-of-line rule: the poison node's child derived,
    // on the empty-marker fallback (its count reads 1, not 2).
    assert_eq!(st(&f, &k).await, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(
        h.calls.lock().unwrap().len(),
        (DERIVE_MAX_ATTEMPTS + 1) as usize,
        "three failing tries + one clean child derivation"
    );
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE state = 'pending'"),
        0,
        "nothing left queued"
    );
}

// ---- d. crash between output-write and state-write ---------------------------

/// The deriver writes its output, THEN crashes: the output survives, the
/// framework's done-record never ran, the expired lease re-runs the whole
/// derivation, and the keyed upsert leaves exactly one output row and one
/// done row — the plan's "crash-mid-derivation is idempotent (tested)".
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn crash_between_output_and_state_write_is_idempotent() {
    let (deriver, h) = counter(KIND, false, 1, true);
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;

    // Attempt 1: output written, then the crash. The output row survives;
    // the framework state is still 'deriving' (lease held, no output).
    let res = derive_pass(&f.t.state).await;
    assert!(res.is_err(), "the crashed pass must surface as an error");
    let out_rows: i64 =
        f.t.db()
            .query_row_map(
                &naatre_store::Sql::plain("SELECT COUNT(*) FROM derive_test_outputs"),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .unwrap();
    assert_eq!(out_rows, 1, "the deriver's output write survived the crash");
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Deriving));
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE output IS NULL"),
        1,
        "the framework's done-record never ran"
    );

    // Attempt 2 (after lease expiry): same input, same output — the keyed
    // upsert replaces, never duplicates.
    f.t.clock.store(BASE_NS + 100_000_000, Ordering::Relaxed);
    let report = derive_pass(&f.t.state).await.expect("recovery pass");
    assert_eq!(report.swept, 1);
    assert_eq!(report.derived, 1);

    let (out_rows, n): (i64, i64) =
        f.t.db()
            .query_row_map(
                &naatre_store::Sql::plain("SELECT COUNT(*), MAX(n) FROM derive_test_outputs"),
                &naatre_store::pv![],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .unwrap();
    assert_eq!(out_rows, 1, "no duplicate output after the re-run");
    assert_eq!(n, 1);
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state"),
        1,
        "single derive_state row"
    );
    assert_eq!(
        h.calls.lock().unwrap().len(),
        2,
        "one crash + one clean run"
    );
}

// ---- e. eager vs deferred ------------------------------------------------------

/// An eager kind derives inside the create's write transaction (done with
/// zero attempts before any pass runs); a deferred kind only enqueues and
/// is derived solely by the queue.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn eager_derives_synchronously_deferred_via_queue() {
    let (eager_deriver, eager_h) = counter("eager-anc", true, 0, false);
    let (defer_deriver, defer_h) = counter("defer-anc", false, 0, false);
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state.register_deriver(Arc::new(eager_deriver)).unwrap();
        state.register_deriver(Arc::new(defer_deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;

    // No pass has run: the eager kind is DONE (derived inside the create's
    // transaction), the deferred kind is only PENDING.
    let eager = status(&f.t.state, "eager-anc", REPO, id_of_hex(&r1))
        .await
        .expect("status");
    assert_eq!(eager, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(*eager_h.calls.lock().unwrap(), vec![id_of_hex(&r1)]);
    let defer = status(&f.t.state, "defer-anc", REPO, id_of_hex(&r1))
        .await
        .expect("status");
    assert_eq!(defer, Some(DeriveStatus::Pending));
    assert!(
        defer_h.calls.lock().unwrap().is_empty(),
        "queue has not run"
    );
    assert_eq!(
        f.t.count(
            "SELECT attempts FROM derive_kinds k JOIN derive_state s ON s.kind = k.id \
                   WHERE k.name = 'eager-anc'"
        ),
        0,
        "eager derivation was first-try"
    );

    // The queue derives ONLY the deferred kind; the eager row is untouched.
    let report = derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(report.derived, 1);
    let defer = status(&f.t.state, "defer-anc", REPO, id_of_hex(&r1))
        .await
        .expect("status");
    assert_eq!(defer, Some(DeriveStatus::Done(count_out(1))));
    assert_eq!(defer_h.calls.lock().unwrap().len(), 1);
    assert_eq!(
        eager_h.calls.lock().unwrap().len(),
        1,
        "no queue re-run of eager"
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM derive_state"), 2);
}

/// An eager kind's first `Fail` is a failed ATTEMPT, never a create
/// failure (the never-blocks rule): the create still returns 201, the row
/// lands 'pending' at attempts=1 (rowless UPSERT — no pending row existed
/// before the eager derive ran), and the queue completes the node once the
/// failure clears.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn eager_fail_is_a_failed_attempt_not_a_create_failure() {
    let armed = Arc::new(Mutex::new(true));
    let deriver = ArmedFailer {
        name: "eager-fail",
        armed: Arc::clone(&armed),
    };
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    // The eager derive runs INSIDE this create's transaction and fails;
    // the create must still succeed. (Pre-fix the Fail hit a rowless
    // derive_fail_attempt and 500'd the whole create.)
    let r1 = create_cs(&f, None, "root", 1).await;

    let row = || -> String {
        f.t.db()
            .query_row_map(
                &naatre_store::Sql::plain(
                    "SELECT state || '/' || attempts FROM derive_kinds k \
                 JOIN derive_state s ON s.kind = k.id WHERE k.name = 'eager-fail'",
                ),
                &naatre_store::pv![],
                |r| r.get::<String>(0),
            )
            .unwrap()
    };
    assert_eq!(
        row(),
        "pending/1",
        "the failed first attempt, back to pending"
    );

    // The failure clears; the queue takes the pending row and completes it.
    *armed.lock().unwrap() = false;
    let report = derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(report.derived, 1);
    let done = status(&f.t.state, "eager-fail", REPO, id_of_hex(&r1))
        .await
        .expect("status");
    assert_eq!(done, Some(DeriveStatus::Done(DerivedOut::empty())));
}

// ---- f. enqueue never blocks the create ----------------------------------------

/// The depth/size cap story, pinned behaviorally: with the queue STOPPED,
/// 1000 changesets are created one by one — every create is an O(1)
/// enqueue (one row), nothing blocks, nothing drops; the queue table just
/// holds the backlog for the (deferred) drain.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn enqueue_never_blocks_the_create() {
    let (deriver, _) = counter(KIND, false, 0, false);
    let f = fixture_with(|state| {
        state.derive_queue_enabled = false; // the queue never runs
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let mut prev: Option<String> = None;
    for i in 0..1000 {
        let parent = prev.as_deref();
        let id = create_cs(&f, parent, &format!("c{i}"), i).await;
        prev = Some(id);
    }

    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 1000);
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE state = 'pending'"),
        1000,
        "every create enqueued exactly one O(1) row"
    );
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state WHERE state != 'pending'"),
        0,
        "nothing derived, nothing dropped: the whole backlog waits"
    );
}

// ---- g. the is-derived / is-pending query --------------------------------------

/// The framework query M6b's read routes build the 503
/// `derivation_pending` contract on: None for unregistered kinds and
/// unknown repos, Pending right after create, Done with the output after
/// the queue drains, Poison after quarantine.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn status_query_tracks_the_lifecycle() {
    let (deriver, h) = counter(KIND, false, 0, false);
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;
    let cs = id_of_hex(&r1);

    // Unregistered kind and unknown repo read as None, never as a state.
    assert_eq!(
        status(&f.t.state, "no-such-kind", REPO, cs)
            .await
            .expect("status"),
        None
    );
    assert_eq!(
        status(&f.t.state, KIND, "no-such-repo", cs)
            .await
            .expect("status"),
        None
    );
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Pending));

    derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(st(&f, &r1).await, Some(DeriveStatus::Done(count_out(1))));

    // A quarantined node reads Poison, not Done and not Pending.
    let p = create_cs(&f, None, "poisoned", 2).await;
    *h.fail_for.lock().unwrap() = Some(id_of_hex(&p));
    for _ in 0..DERIVE_MAX_ATTEMPTS {
        derive_pass(&f.t.state).await.expect("pass");
    }
    assert_eq!(st(&f, &p).await, Some(DeriveStatus::Poison));
}

// ---- land enqueue ----------------------------------------------------------------

/// The land hook: a rebased changeset (a brand-new node the land inserts)
/// is enqueued like a created one and derives from its rebased parent's
/// output; a fast-forward land (no new changesets) enqueues nothing.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn land_enqueues_the_rebased_changeset() {
    let (deriver, h) = counter(KIND, false, 0, false);
    let f = fixture_with(|state| {
        state.derive_queue_interval = Duration::from_millis(10);
        state.register_deriver(Arc::new(deriver)).unwrap();
    });

    let r1 = create_cs(&f, None, "root", 1).await;
    // main seeds at the root (refs route refuses main; the ref row is
    // land's only writer, seeded the way M2a's log tests seed it).
    let repo_id =
        f.t.count(&format!("SELECT id FROM repos WHERE name = '{REPO}'"));
    f.t.db()
        .execute(
            &naatre_store::Sql::plain(
                "INSERT INTO refs(repo_id, name, target, version, last_principal) \
         VALUES (?1, 'main', ?2, 1, 'm6a-test')",
            ),
            &naatre_store::pv![repo_id, id_of_hex(&r1).0.as_slice()],
        )
        .unwrap();

    let a = create_cs(&f, Some(&r1), "a", 2).await;
    let landed_a = land(&f, &a, "k1").await;
    assert_eq!(landed_a, a, "fast-forward land: no new changeset");
    let rows_after_ff: i64 = f.t.count("SELECT COUNT(*) FROM derive_state");
    assert_eq!(rows_after_ff, 2, "a fast-forward land enqueues nothing new");

    // A diverged branch: b shares the root but not main's tip. The land
    // must rebase b into a NEW changeset — which the hook enqueues.
    let b = create_cs(&f, Some(&r1), "b", 3).await;
    let landed_b = land(&f, &b, "k2").await;
    assert_ne!(landed_b, b, "the land rebased b onto a's tip");
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM derive_state"),
        4,
        "r1, a, b, plus the land's rebased changeset — all enqueued"
    );
    assert_eq!(st(&f, &landed_b).await, Some(DeriveStatus::Pending));

    derive_pass(&f.t.state).await.expect("pass");
    assert_eq!(
        st(&f, &landed_b).await,
        Some(DeriveStatus::Done(count_out(3)))
    );
    // r1=1, a=2, rebased b = 2 + 1 = 3: the rebased parent's output flowed.
    // (The pre-land branch commit b also queued from its own create.)
    assert_eq!(h.calls.lock().unwrap().len(), 4);
}
