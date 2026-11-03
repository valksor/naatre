//! M4c advisor tests: the cases the M4a/M4b suites missed, extended by the
//! fix round.
//!
//! (a) An ALREADY-CONTAINED head landed under a NEW key settles done with
//! ZERO new changesets, and the lands rows record the containment result —
//! idempotent across keys (a second new key records the same landed id).
//!
//! (b) A head chain whose rebased delta becomes empty (main already
//! contains the node's exact change from a sibling with identical content)
//! drops the empty node — the canonical format forbids an empty changeset,
//! so no changeset row without file_changes rows may exist; main advances
//! to the surviving rebased chain.
//!
//! Fix-round additions: the joiner-with-the-settled-head fallback (a joiner
//! whose head equals the leader's settled head replays the settled row
//! instead of inheriting the leader's 409); the join failure modes (a
//! leader cancelled mid-flight fails its joiner promptly through the guard
//! Drop fallback, the join deadline expires as retryable 503, and a
//! cancelled leader that lands anyway heals via status + replay); and
//! corrupted-tree-store honesty (a claimed tree id with no `trees` row is
//! a 500, never a silent empty tree).

use std::time::{Duration, Instant};

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use serde_json::{Value, json};
use tower::ServiceExt;

mod m4b_common;

use m4b_common::{
    DEADLINE, Fixture, REPO, Resp, add, cat_on, code_of, conn_at, cs_body, fixture, fixture_with,
    log_chain_paged,
};

/// The recorded `{landed, rebased, note}` of a settled land row.
fn land_result(f: &Fixture, key: &str) -> Value {
    let json: String = conn_at(&f.t.db_path)
        .query_row("SELECT result_json FROM lands WHERE key = ?1", [key], |r| {
            r.get(0)
        })
        .expect("lands row for key");
    serde_json::from_str(&json).expect("recorded result json")
}

// ---- a. contained head under a new key -------------------------------------

#[tokio::test]
async fn contained_head_under_a_new_key_is_idempotent_across_keys() {
    let f = fixture();
    let b1 = f.put_blob(b"base\n").await;
    let (c1, _) = f
        .create_ok(&cs_body(json!([]), json!([add("base.txt", &b1, 5)]), "c1"))
        .await;
    let b2 = f.put_blob(b"tip\n").await;
    let (c2, _) = f
        .create_ok(&cs_body(
            json!([c1.clone()]),
            json!([add("tip.txt", &b2, 4)]),
            "c2",
        ))
        .await;
    // main = c2: landing c1 (an ancestor) is already contained.
    f.seed_main(&c2);

    let cs_before = f.count("SELECT COUNT(*) FROM changesets");
    let resp = f.land(&c1, "k-new-1").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(
        resp.body["landed"],
        c2.as_str(),
        "a contained head settles at main itself"
    );
    assert_eq!(resp.body["rebased"], 0);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM changesets"),
        cs_before,
        "a contained head creates ZERO new changesets"
    );
    let result1 = land_result(&f, "k-new-1");
    assert_eq!(result1["landed"], c2.as_str(), "the lands row records it");
    assert_eq!(result1["rebased"], 0);

    // A second NEW key over the same contained head: the same landed id,
    // still zero new changesets, one more done row.
    let resp = f.land(&c1, "k-new-2").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(
        resp.body["landed"],
        c2.as_str(),
        "idempotent across keys: the same landed id"
    );
    assert_eq!(f.count("SELECT COUNT(*) FROM changesets"), cs_before);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM lands WHERE state = 'done'"),
        2,
        "one done lands row per key"
    );
    assert_eq!(
        land_result(&f, "k-new-2"),
        result1,
        "the second key records the same containment result"
    );
}

// ---- b. empty-delta node dropped from the chain ------------------------------

#[tokio::test]
async fn empty_delta_node_is_dropped_and_main_advances_to_the_rebased_chain() {
    let f = fixture();
    // c0 (root): base.txt; main = c0.
    let bb = f.put_blob(b"base\n").await;
    let (c0, _) = f
        .create_ok(&cs_body(json!([]), json!([add("base.txt", &bb, 5)]), "c0"))
        .await;
    f.seed_main(&c0);

    // The head chain c1 → c2 off c0.
    let ba = f.put_blob(b"A\n").await;
    let (c1, _) = f
        .create_ok(&cs_body(
            json!([c0.clone()]),
            json!([add("a.txt", &ba, 2)]),
            "c1",
        ))
        .await;
    let b2 = f.put_blob(b"B\n").await;
    let (c2, _) = f
        .create_ok(&cs_body(
            json!([c1.clone()]),
            json!([add("b.txt", &b2, 2)]),
            "c2",
        ))
        .await;

    // main advances with a SIBLING change whose content duplicates c2's
    // exactly (same path, same blob): m1 = c0 + b.txt. Rebasing c2 against
    // it leaves an EMPTY delta, which the canonical format cannot encode —
    // the node is dropped from the chain (documented M4a behavior).
    let (m1, _) = f
        .create_ok(&cs_body(
            json!([c0.clone()]),
            json!([add("b.txt", &b2, 2)]),
            "m1",
        ))
        .await;
    f.move_main(&m1);

    let cs_before = f.count("SELECT COUNT(*) FROM changesets");
    let resp = f.land(&c2, "k-empty").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_ne!(landed, c1, "c1 was rewritten onto main");
    assert_eq!(resp.body["main"], landed.as_str());
    assert_eq!(
        resp.body["rebased"], 1,
        "only c1 rebased; c2's empty delta is dropped"
    );
    assert_eq!(
        f.main_target().as_deref(),
        Some(landed.as_str()),
        "main advanced to the rebased c1"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM changesets"),
        cs_before + 1,
        "exactly one new changeset (the rebased c1); no empty node for c2"
    );

    // Content at the new main: c1's change landed, c2's was already there.
    assert_eq!(
        cat_on(&f.t.app, &f.reader, &landed, "a.txt").await,
        b"A\n".to_vec()
    );
    assert_eq!(
        cat_on(&f.t.app, &f.reader, &landed, "b.txt").await,
        b"B\n".to_vec()
    );

    // The log from main is the correct count: landed → m1 → c0.
    let chain = log_chain_paged(&f.t.app, &f.reader, &landed, 10).await;
    assert_eq!(
        chain,
        vec![landed.clone(), m1.clone(), c0.clone()],
        "chain: {chain:?}"
    );

    // Canonical format forbids an empty changeset: no changeset row in the
    // DB may exist without file_changes rows.
    assert_eq!(
        f.count(
            "SELECT COUNT(*) FROM changesets c WHERE NOT EXISTS \
             (SELECT 1 FROM file_changes fc WHERE fc.repo_id = c.repo_id AND fc.cs = c.id)"
        ),
        0,
        "no empty changeset in the DB"
    );
}

// ---- join helpers --------------------------------------------------------------

/// A land request against the bare land router as a spawned task (so the
/// caller can cancel the leader mid-flight: `abort()` drops the handler
/// future, which drops the leader's join guard). The test runtime is
/// `#[tokio::test]`'s current-thread runtime: a spawn followed by one
/// `yield_now()` lets the task take its FIRST poll, and on the bare router
/// that poll runs the handler synchronously up to the registry `begin` —
/// registration is deterministic before the next spawn.
fn spawn_bare_land(bare: &Router, head: &str, key: &str) -> tokio::task::JoinHandle<Resp> {
    let (bare, head, key) = (bare.clone(), head.to_string(), key.to_string());
    tokio::spawn(async move {
        let req = axum::http::Request::builder()
            .method("POST")
            .uri(format!("/v1/repos/{REPO}/land"))
            .header(header::CONTENT_TYPE, "application/json")
            .extension(naatre_server::auth::AuthedPrincipal {
                principal: "writer-p".into(),
                role: naatre_server::auth::Role::Writer,
                scope_path: None,
            })
            .body(Body::from(json!({ "head": head, "key": key }).to_string()))
            .unwrap();
        let res = tokio::time::timeout(DEADLINE, bare.oneshot(req))
            .await
            .expect("land request answered inside the deadline")
            .expect("in-memory request");
        let status = res.status();
        let bytes = BodyExt::collect(res.into_body())
            .await
            .expect("body")
            .to_bytes()
            .to_vec();
        let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
        Resp { status, body }
    })
}

/// A minimal land router (no auth layer; the principal extension attached
/// directly — the M4a writer-busy technique): the handler's registry
/// `begin` runs on the future's FIRST poll, with no auth read in between,
/// so `tokio::join!`'s in-order polling deterministically makes the FIRST
/// request the leader and the SECOND one the joiner.
fn bare_router(f: &Fixture) -> Router {
    axum::Router::new()
        .route(
            "/v1/repos/{r}/land",
            axum::routing::post(naatre_server::routes::land::land),
        )
        .with_state(std::sync::Arc::clone(&f.t.state))
}

async fn bare_land(bare: &Router, head: &str, key: &str) -> Resp {
    let req = axum::http::Request::builder()
        .method("POST")
        .uri(format!("/v1/repos/{REPO}/land"))
        .header(header::CONTENT_TYPE, "application/json")
        .extension(naatre_server::auth::AuthedPrincipal {
            principal: "writer-p".into(),
            role: naatre_server::auth::Role::Writer,
            scope_path: None,
        })
        .body(Body::from(json!({ "head": head, "key": key }).to_string()))
        .unwrap();
    let res = tokio::time::timeout(DEADLINE, bare.clone().oneshot(req))
        .await
        .expect("land request answered inside the deadline")
        .expect("in-memory request");
    let status = res.status();
    let bytes = BodyExt::collect(res.into_body())
        .await
        .expect("body")
        .to_bytes()
        .to_vec();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Resp { status, body }
}

/// A separate connection holding the SQLite write lock (`BEGIN IMMEDIATE`).
/// This blocks a land's write closure INSIDE `TxnGuard::begin` (busy
/// timeout 5 s) — the only place a leader can block, since `state.write`'s
/// `try_lock` 503s instead of queueing. `hold_write_txn` cannot play this
/// role: it holds the STORE mutex, which would 503 the leader outright.
///
/// The hold stands until the CALLER releases it (`rollback()` — the d/e/f
/// tests roll back once their cancel has landed, so the detached closure
/// is provably blocked-in-BEGIN when they abort). A releaser thread
/// self-releases after 2.5 s as a pure SAFETY FALLBACK: above any test flow
/// (the tests cancel within milliseconds of the signal), below SQLite's
/// 5 s busy timeout — so a test that fails BEFORE its rollback still lets
/// the detached BEGIN (parked in its busy wait) find the lock gone and
/// finish before the fixture's temp dir drops. (The original 300 ms
/// self-release was itself the bug: it raced the leader's closure spawn —
/// a closure starting after the release committed instead of blocking,
/// and the "blocked / not yet committed" premise broke. See the d/e/f
/// block.) Dropping the connection rolls the lock back.
fn hold_sqlite_write(f: &Fixture) -> HoldGuard {
    let conn = conn_at(&f.t.db_path);
    conn.execute_batch("BEGIN IMMEDIATE")
        .expect("hold: BEGIN IMMEDIATE");
    // The guard owns the HOLDING connection through a shared mutex; the
    // releaser thread rolls back ON THAT CONNECTION — a genuine
    // self-release (a second connection's ROLLBACK would be a
    // per-connection no-op: the M13b-fix senior-qa C3 finding).
    let shared = std::sync::Arc::new(std::sync::Mutex::new(conn));
    let releaser = std::sync::Arc::clone(&shared);
    std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(2500));
        if let Ok(c) = releaser.lock() {
            let _ = c.execute_batch("ROLLBACK");
        }
    });
    HoldGuard { shared }
}

/// The write-lock hold: shared so the releaser thread (and the caller)
/// can roll the holding connection's transaction back.
struct HoldGuard {
    shared: std::sync::Arc<std::sync::Mutex<rusqlite::Connection>>,
}

impl HoldGuard {
    /// The caller's release — the REAL one under the d/e/f contract (the
    /// 2.5 s releaser is only the failure fallback, so during a passing run
    /// this fires first). A no-op once the fallback has run (rolling back
    /// no transaction is fine).
    fn rollback(&self) {
        if let Ok(c) = self.shared.lock() {
            let _ = c.execute_batch("ROLLBACK");
        }
    }
}

/// Bounded cooperative handoff: lets a just-spawned land task take its
/// first poll (see [`spawn_bare_land`]).
async fn first_poll() {
    tokio::task::yield_now().await;
}

/// Base c0 on main plus one child head: the smallest real land. `configure`
/// hooks the state before it is Arc-shared (e.g. a low join deadline).
async fn base_with_child_with(
    configure: impl FnOnce(&mut naatre_server::state::ServerState),
) -> (Fixture, String, String) {
    let f = fixture_with(configure);
    let bb = f.put_blob(b"base\n").await;
    let (c0, _) = f
        .create_ok(&cs_body(json!([]), json!([add("base.txt", &bb, 5)]), "c0"))
        .await;
    f.seed_main(&c0);
    let ba = f.put_blob(b"A\n").await;
    let (h1, _) = f
        .create_ok(&cs_body(
            json!([c0.clone()]),
            json!([add("a.txt", &ba, 2)]),
            "h1",
        ))
        .await;
    (f, c0, h1)
}

async fn base_with_child() -> (Fixture, String, String) {
    base_with_child_with(|_| {}).await
}

/// Waits (bounded) until the detached write closure of a cancelled leader
/// has committed its land, so the temp dir never drops under it.
async fn wait_key_done(f: &Fixture, key: &str) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while f.count(&format!(
        "SELECT COUNT(*) FROM lands WHERE key = '{key}' AND state = 'done'"
    )) == 0
    {
        assert!(Instant::now() < deadline, "detached leader never settled");
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

/// Arms [`ServerState::land_test_write_started`] and returns the receiver:
/// a land's write closure fires it on the blocking pool right before its
/// `BEGIN IMMEDIATE` (see the d/e/f comment block — the only deterministic
/// "the detached-capable closure exists" edge a test can wait on, since
/// nothing observable happens before the transaction opens). One message
/// per armed land write; only (d) and (f) arm it, and each disarms right
/// after its cancel — (e) never arms the hook (nothing is detached there).
fn arm_write_started(f: &Fixture) -> tokio::sync::mpsc::UnboundedReceiver<()> {
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    *f.t.state.land_test_write_started.lock().unwrap() = Some(tx);
    rx
}

/// Bounded wait for the armed write-closure signal: aborting only after it
/// guarantees the cancelled leader leaves its detached closure behind.
async fn wait_write_started(mut rx: tokio::sync::mpsc::UnboundedReceiver<()>) {
    tokio::time::timeout(DEADLINE, rx.recv())
        .await
        .expect("the leader's write closure started inside the deadline")
        .expect("the armed sender lives until disarm");
}

// ---- c. the joiner-with-the-settled-head fallback ------------------------------

#[tokio::test]
async fn joiner_with_the_settled_head_replays_despite_the_leaders_409() {
    let f = fixture();
    let bb = f.put_blob(b"base\n").await;
    let (c0, _) = f
        .create_ok(&cs_body(json!([]), json!([add("base.txt", &bb, 5)]), "c0"))
        .await;
    f.seed_main(&c0);
    let ba = f.put_blob(b"A\n").await;
    let (h0, _) = f
        .create_ok(&cs_body(
            json!([c0.clone()]),
            json!([add("a.txt", &ba, 2)]),
            "h0",
        ))
        .await;
    // Settle K with H0 (main == H0's parent → no rebase, landed == H0)…
    let settled = f.land(&h0, "k-settled").await;
    assert_eq!(settled.status, StatusCode::OK, "body: {}", settled.body);
    let bb1 = f.put_blob(b"B\n").await;
    let (h1, _) = f
        .create_ok(&cs_body(
            json!([c0.clone()]),
            json!([add("b.txt", &bb1, 2)]),
            "h1",
        ))
        .await;

    // …then fire land(K, H1) and land(K, H0) concurrently on the bare land
    // router. join! polls in order on one task: H1 registers first (its
    // registry begin runs on the first poll) and leads, H0 joins. The
    // leader's outcome is ITS OWN settled-rules 409 (settled head = H0);
    // the H0 joiner's head EQUALS that settled head, so the fallback must
    // replay the settled row → 200, not inherit the leader's 409.
    let bare = bare_router(&f);
    let (r_h1, r_h0) = tokio::join!(
        bare_land(&bare, &h1, "k-settled"),
        bare_land(&bare, &h0, "k-settled"),
    );
    assert_eq!(r_h0.status, StatusCode::OK, "body: {}", r_h0.body);
    assert_eq!(
        r_h0.body, settled.body,
        "the joiner with the settled head gets the identical recorded result"
    );
    assert_eq!(r_h1.status, StatusCode::CONFLICT, "body: {}", r_h1.body);
    assert_eq!(code_of(&r_h1), "idempotency_key_settled");
    assert_eq!(
        r_h1.body["error"]["details"]["settled_head"],
        h0.as_str(),
        "the leader's 409 names H0 — the joiner's head, not the leader's"
    );

    // One settlement: the fallback replay wrote nothing new.
    assert_eq!(
        f.count("SELECT COUNT(*) FROM lands WHERE key = 'k-settled'"),
        1
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'land'"),
        1
    );
}

// ---- d/e/f. join failure modes --------------------------------------------------
//
// Deterministic mid-flight control, no probe loops: the tests run on
// `#[tokio::test]`'s current-thread runtime against the BARE land router.
// The leader task is spawned and given one `yield_now()` — its first poll
// runs the handler synchronously through the registry `begin`, so the
// entry exists before the joiner is even spawned, and the joiner (spawned
// next) joins it. That first poll does NOT reach the write closure: the
// handler's next await is `write_repo`'s repo-resolve pool read, and the
// closure only spawns once that read returns — scheduler timing (this
// raced on CI: an `abort()` landing before the spawn left no detached
// closure, and nothing ever settled). So the cancel tests arm
// `land_test_write_started` and abort only after its signal: the closure
// fires it on the blocking pool, past every await, right before
// `BEGIN IMMEDIATE` — where it then blocks on the held SQLite lock (busy
// timeout 5 s), the only place a leader can block, since `state.write`'s
// `try_lock` 503s instead of queueing (so `hold_write_txn`, which holds
// the STORE mutex, cannot play this role: it would 503 the leader
// outright).

/// (d) Leader cancelled mid-flight: its guard's Drop fallback publishes an
/// internal error, so the joiner fails PROMPTLY instead of waiting out the
/// join deadline (10 s) or the drain timeout (30 s).
#[tokio::test]
async fn cancelled_leader_fails_its_joiner_promptly() {
    let (f, _c0, h1) = base_with_child().await;
    let hold = hold_sqlite_write(&f);
    let write_started = arm_write_started(&f);
    let bare = bare_router(&f);
    let leader = spawn_bare_land(&bare, &h1, "k-drop");
    first_poll().await;
    let joiner = spawn_bare_land(&bare, &h1, "k-drop");
    first_poll().await;

    // Deterministic mid-flight point: the leader's write closure is
    // running now, a synchronous step from `BEGIN IMMEDIATE`, which then
    // blocks on the held lock (the hold stands until this test's rollback
    // — the 2.5 s fallback is failure-only) — the abort cannot race the
    // spawn, and the land is provably uncommitted.
    wait_write_started(write_started).await;
    leader.abort();
    *f.t.state.land_test_write_started.lock().unwrap() = None;
    let started = Instant::now();
    let resp = tokio::time::timeout(Duration::from_secs(3), joiner)
        .await
        .expect("joiner answered promptly")
        .expect("joiner task");
    assert_eq!(
        resp.status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "body: {}",
        resp.body
    );
    assert_eq!(code_of(&resp), "internal");
    assert!(
        started.elapsed() < Duration::from_secs(3),
        "the joiner must fail fast, took {:?}",
        started.elapsed()
    );

    // Release the SQLite lock so the cancelled leader's detached blocking
    // closure finishes before the temp dir drops.
    hold.rollback();
    wait_key_done(&f, "k-drop").await;
}

/// (e) Join-deadline expiry: a joiner whose leader stays in flight past
/// `join_deadline` gets retryable 503 `writer_busy` quickly (the retry
/// lands on the settled-row replay), and the leader itself stays healthy.
#[tokio::test]
async fn join_deadline_expiry_is_a_prompt_retryable_503() {
    let (f, _c0, h1) =
        base_with_child_with(|state| state.join_deadline = Duration::from_millis(250)).await;
    let hold = hold_sqlite_write(&f);
    let bare = bare_router(&f);
    let leader = spawn_bare_land(&bare, &h1, "k-deadline");
    first_poll().await;
    let joiner = spawn_bare_land(&bare, &h1, "k-deadline");
    first_poll().await;

    let started = Instant::now();
    let resp = tokio::time::timeout(Duration::from_secs(3), joiner)
        .await
        .expect("joiner answered well within the bound")
        .expect("joiner task");
    assert_eq!(resp.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(code_of(&resp), "writer_busy");
    assert_eq!(resp.body["error"]["retryable"], json!(true));
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "the join deadline must fire fast, took {:?}",
        started.elapsed()
    );

    // The leader was never touched: release the lock, it lands normally.
    hold.rollback();
    let done = tokio::time::timeout(Duration::from_secs(10), leader)
        .await
        .expect("leader completes")
        .expect("leader task");
    assert_eq!(done.status, StatusCode::OK, "body: {}", done.body);
}

/// (f) Cancel-after-land heal: the joiner is told "internal error" by the
/// Drop fallback, but the cancelled leader's detached write closure still
/// commits — GET /land/{key} then shows done, and a retry replays the
/// recorded 200.
#[tokio::test]
async fn cancelled_leader_that_lands_anyway_heals_via_status_and_retry() {
    let (f, _c0, h1) = base_with_child().await;
    let hold = hold_sqlite_write(&f);
    let write_started = arm_write_started(&f);
    let bare = bare_router(&f);
    let leader = spawn_bare_land(&bare, &h1, "k-heal");
    first_poll().await;
    let joiner = spawn_bare_land(&bare, &h1, "k-heal");
    first_poll().await;

    // Same deterministic mid-flight point as (d): the closure is a step
    // from BEGIN IMMEDIATE and blocked by the hold — it exists and is
    // uncommitted when the abort lands.
    wait_write_started(write_started).await;
    leader.abort();
    *f.t.state.land_test_write_started.lock().unwrap() = None;
    let resp = tokio::time::timeout(Duration::from_secs(3), joiner)
        .await
        .expect("joiner answered promptly")
        .expect("joiner task");
    assert_eq!(resp.status, StatusCode::INTERNAL_SERVER_ERROR);
    assert_eq!(code_of(&resp), "internal");

    // The land settles anyway once the lock is gone: the client's error was
    // a lie the status surface can heal.
    hold.rollback();
    wait_key_done(&f, "k-heal").await;
    let status = m4b_common::get_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/land/k-heal"),
        &f.writer,
    )
    .await;
    assert_eq!(status.status, StatusCode::OK, "body: {}", status.body);
    assert_eq!(status.body["state"], "done");
    assert_eq!(status.body["head"], h1.as_str());

    // And the retry replays the recorded result.
    let retry = f.land(&h1, "k-heal").await;
    assert_eq!(retry.status, StatusCode::OK, "body: {}", retry.body);
    assert_eq!(
        retry.body,
        status.body["result"]
            .as_object()
            .map(|r| json!({
                "landed": r["landed"],
                "main": r["landed"],
                "rebased": r["rebased"],
                "note": Value::Null,
                "conflicts": r["conflicts"],
            }))
            .unwrap()
    );
    assert_eq!(
        f.main_target().as_deref(),
        Some(retry.body["landed"].as_str().unwrap()),
        "main carries the landed tip"
    );
}

// ---- g. corrupted tree store is a 500, never a silent empty tree ---------------

#[tokio::test]
async fn missing_tree_row_is_corruption_not_silent_empty() {
    let f = fixture();
    let bb = f.put_blob(b"base\n").await;
    let (c0, root0) = f
        .create_ok(&cs_body(json!([]), json!([add("base.txt", &bb, 5)]), "c0"))
        .await;
    f.seed_main(&c0);
    let ba = f.put_blob(b"A\n").await;
    let (h1, _) = f
        .create_ok(&cs_body(
            json!([c0.clone()]),
            json!([add("a.txt", &ba, 2)]),
            "h1",
        ))
        .await;

    // Store corruption, applied directly: the trees row main's changeset
    // claims is gone. A land that must load that tree fails loudly (500)
    // — the missing row must never read as a legitimate empty tree.
    let conn = conn_at(&f.t.db_path);
    conn.execute(
        "DELETE FROM trees WHERE id = ?1",
        [naatre_core::ids::Id::from_hex(&root0).unwrap().0.as_slice()],
    )
    .unwrap();

    let resp = f.land(&h1, "k-corrupt").await;
    assert_eq!(
        resp.status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "body: {}",
        resp.body
    );
    assert_eq!(code_of(&resp), "internal");

    // Nothing settled: the transaction rolled back (key unset, main
    // unmoved, no changesets written).
    assert_eq!(
        f.count("SELECT COUNT(*) FROM lands WHERE key = 'k-corrupt'"),
        0,
        "the failed land leaves the key unsettled"
    );
    assert_eq!(f.main_target().as_deref(), Some(c0.as_str()));
}

/// The M13b-fix senior-qa C3 pin, updated to the fallback contract: the
/// hold SELF-releases — no caller rollback — at the documented 2.5 s
/// fallback, inside the 5 s busy window a detached land BEGIN works under.
/// The observer is a write_repo whose closure takes the SQLite write lock
/// (`BEGIN IMMEDIATE`): it is blocked exactly as long as the hold stands,
/// so its completion measures the self-release. The elapsed measurement is
/// anchored at the instant BEFORE the hold's creation: the 2.5 s fallback
/// starts there, so `elapsed >= 2 s` can never fire "too fast" on a slow
/// runner (a clock taken after the hold could). (The pre-fix
/// second-connection ROLLBACK — a per-connection no-op — could never
/// deliver this, and a read-only closure could never SEE it either: a
/// gate-observing read does not contend with the hold.)
#[tokio::test]
async fn the_hold_self_releases_without_a_caller_rollback() {
    let f = fixture();
    let started = Instant::now();
    let hold = hold_sqlite_write(&f);
    let state = std::sync::Arc::clone(&f.t.state);
    let done = tokio::spawn(async move {
        state
            .write_repo(m4b_common::REPO, move |cx| {
                let tx = cx
                    .db
                    .transaction_immediate()
                    .map_err(naatre_server::api_error::ApiError::log_internal)?;
                tx.commit()
                    .map_err(naatre_server::api_error::ApiError::log_internal)?;
                Ok(())
            })
            .await
    });
    // Generous upper bound: the release lands ~2.5 s after the hold's
    // creation; the timeout only catches a fallback that never fired. The
    // UNDER-5 s property (the fallback must fire inside the detached
    // BEGIN's 5 s busy window) is enforced by the observer itself: a late
    // release hits the closure's `BEGIN IMMEDIATE` busy timeout and turns
    // the result into an `Err` — which fails the `Ok(Ok(Ok(())))` match
    // below — so no second wall-clock assertion is needed.
    let outcome = tokio::time::timeout(Duration::from_secs(10), done).await;
    match outcome {
        Ok(Ok(Ok(()))) => {}
        other => panic!("the hold did not self-release without a caller rollback: {other:?}"),
    }
    let elapsed = started.elapsed();
    assert!(
        elapsed >= Duration::from_secs(2),
        "the release must be the 2.5 s fallback (anchored at the hold's \
         creation), not an early one: {elapsed:?}"
    );
    // The caller's release is a no-op after the self-release (rolling
    // back no transaction is fine).
    hold.rollback();
}
