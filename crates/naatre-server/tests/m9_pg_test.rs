//! M9a: the NAMED Postgres subset of the M4 crash matrix + land
//! concurrency invariants (plan M9: "the M4 crash matrix + land
//! concurrency invariants re-run on Postgres — full enum on SQLite;
//! serialization-retry subset named for Postgres MVCC").
//!
//! The full crash-point enum stays SQLite-only (the crash points exercise
//! SQLite's WAL/rollback mechanics); what re-runs on Postgres are the legs
//! whose failure mode maps to MVCC serialization:
//!
//! 1. land idempotency — settled-key replay, settled-key + different head
//!    → 409, and the writer-busy-shaped 503 while a write transaction is
//!    held (`DbError::is_retryable_contention` is the Postgres
//!    serialization/deadlock/lock-unavailable mapping's contract);
//! 2. conflict → key unsettled → re-land with the SAME key (the recipe
//!    path — a conflict must not consume the idempotency key);
//! 3. concurrent same-key lands join to identical outcomes;
//! 4. the derive lease: expired-lease sweep re-derivation (idempotent
//!    after a lost lease) and the busy-abort release (a writer-busy at the
//!    release step never burns an attempt);
//! 5. the boot backfill's batch cursor over the Postgres `seq` (rowid)
//!    columns.
//!
//! Every leg SKIPS with a printed notice when `NAATRE_TEST_PG_DSN` is
//! absent (CI sets it; local machines without a Postgres server stay
//! green). The store-level per-combo conformance suite lives beside the
//! store crate's integration tests.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::{AppState, ServerState};
use naatre_server::{db, derive};
use naatre_store::backend::BackendConfig;
use naatre_store::{DataDir, Db, Sql, pv};

const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m9pg";
const KIND: &str = "m9pg-probe";

#[path = "../../naatre-store/tests/pg_provision/mod.rs"]
mod pg_provision;

/// The PG target for one test: the env DSN when set (CI), else a locally
/// provisioned throwaway cluster; `None` only when no PostgreSQL
/// installation exists (the skip notice is already printed).
fn pg_target() -> Option<pg_provision::PgTarget> {
    pg_provision::acquire()
}

fn test_clock() -> Arc<AtomicU64> {
    Arc::new(AtomicU64::new(BASE_NS))
}

/// A throwaway deferred deriver: emits a fixed output, records its calls.
struct CountingDeriver {
    calls: Arc<Mutex<Vec<Id>>>,
}

impl derive::Deriver for CountingDeriver {
    fn kind(&self) -> &'static str {
        KIND
    }

    fn derive(
        &self,
        _ctx: &derive::DeriveCtx,
        _conn: &Db,
        _repo_id: i64,
        cs: Id,
        _parents_done: &[derive::ParentOut],
    ) -> Result<derive::DerivedOut, derive::DeriveFailure> {
        self.calls.lock().unwrap().push(cs);
        Ok(derive::DerivedOut(vec![1u8]))
    }
}

struct Ctx {
    _dir: tempfile::TempDir,
    app: Router,
    state: AppState,
    writer: String,
    #[allow(dead_code)] // named for leg readability; the seed conn carries it
    pg: BackendConfig,
    /// Holds the provisioned cluster (if any) alive for the leg.
    _target: Option<pg_provision::PgTarget>,
    /// One pre-opened seed connection, shared through the same
    /// mutex-then-blocking-thread shape production uses (the sync driver
    /// runs every query on its own runtime: calls from a tokio worker
    /// thread panic, so ALL direct db:: use goes through
    /// [`Ctx::with_seed`]).
    seed: Arc<Mutex<Db>>,
    calls: Arc<Mutex<Vec<Id>>>,
    /// Kept for the lease-expiry legs that need to move the injectable
    /// clock; the current legs read it only through the state's closure.
    _clock: Arc<AtomicU64>,
}

/// Builds the state + router over a FRESH PG schema. The caller seeds
/// (metadata + changesets) through the returned seed connection BEFORE the
/// router is used; the registry is already closed (register happens on
/// the pre-`Arc` state).
fn open_ctx_blocking(register_kind: bool) -> (Ctx, i64) {
    open_ctx_blocking_cfg(register_kind, |_| {})
}

fn open_ctx_blocking_cfg(
    register_kind: bool,
    configure: impl FnOnce(&mut ServerState) + Send + 'static,
) -> (Ctx, i64) {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let Some(target) = pg_target() else {
        unreachable!("callers gate on pg_target() first")
    };
    let cfg = target.config();
    // ALL driver work happens on a plain thread: the sync client runs
    // every query on its internal runtime, which refuses to start inside
    // a tokio worker (the production rule: DB work goes through
    // spawn_blocking).
    let connect_cfg = cfg.clone();
    let dir = tempfile::tempdir().unwrap();
    let clock = test_clock();
    let calls = Arc::new(Mutex::new(Vec::new()));
    // ALL driver work happens on a plain thread: the sync client runs
    // every query on its internal runtime, which refuses to start inside
    // a tokio worker (the production rule: DB work goes through
    // spawn_blocking). The state opens FIRST (migrations), then the seed.
    let dir_for_thread = dir.path().to_path_buf();
    let calls_for_thread = Arc::clone(&calls);
    let clock_for_thread = Arc::clone(&clock);
    let (thread_state, thread_seed, repo_id, writer_token) = std::thread::spawn(move || {
        let conn =
            Db::connect_postgres(connect_cfg.pg.as_ref().unwrap(), false).expect("seed conn");
        let mut state = ServerState::open_backend(DataDir::open_any(&dir_for_thread), &connect_cfg)
            .expect("pg state opens");
        let clock_for_closure = Arc::clone(&clock_for_thread);
        state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
        state.snapshot_gc_enabled = false;
        state.derive_queue_enabled = false;
        state.derive_backfill_enabled = false;
        if register_kind {
            state
                .register_deriver(Arc::new(CountingDeriver {
                    calls: Arc::clone(&calls_for_thread),
                }))
                .expect("kind registers");
        }
        configure(&mut state);
        let repo_id = db::repos_create(&conn, REPO, BASE_NS as i64).expect("repo");
        // main points at a REAL (rootless, empty) changeset: create-path
        // parent validation requires known changeset ids.
        let root_id = Id({
            let mut b = [0u8; 32];
            b[0] = 0x8F;
            b
        });
        db::changeset_insert(
            &conn,
            db::ChangesetInsert {
                repo_id,
                repo_name: REPO,
                id: &root_id,
                canonical: &[0u8],
                author: "Seeder",
                message: "root",
                timestamp_ns: BASE_NS as i64,
                generation: 0,
                principal: "seed",
                root_tree_id: &root_id,
                parents: &[],
                changes: &[],
                blobs: &[],
                trees: &[db::TreeWrite {
                    id: root_id,
                    canonical: vec![0u8],
                    entries: vec![],
                }],
                now_ns: BASE_NS as i64,
            },
        )
        .expect("root changeset");
        db::ref_cas(
            &conn,
            &conn,
            db::RefMove {
                repo_id,
                repo_name: REPO,
                name: "main",
                target: &root_id,
                expected: None,
                principal: "seed",
                now_ns: BASE_NS as i64,
            },
        )
        .expect("main created");
        let (token, hash) = naatre_server::auth::mint_token();
        db::principal_upsert(&conn, "writer-p", "writer", BASE_NS as i64).expect("principal");
        db::token_insert(&conn, "writer-p", &hash, None, None).expect("token");
        (state, Arc::new(Mutex::new(conn)), repo_id, token)
    })
    .join()
    .unwrap();
    let mut state = thread_state;
    let seed = thread_seed;
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    state.snapshot_gc_enabled = false;
    state.derive_queue_enabled = false;
    state.derive_backfill_enabled = false;
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    let ctx = Ctx {
        _dir: dir,
        app,
        state,
        writer: writer_token,
        pg: cfg,
        _target: Some(target),
        seed,
        calls,
        _clock: clock,
    };
    (ctx, repo_id)
}

/// Runs one direct-DB closure on the seed connection, off the runtime.
async fn with_seed<R>(ctx: &Ctx, f: impl FnOnce(&Db) -> R + Send + 'static) -> R
where
    R: Send + 'static,
{
    let seed = Arc::clone(&ctx.seed);
    tokio::task::spawn_blocking(move || f(&seed.lock().unwrap()))
        .await
        .expect("seed task")
}

// ---- HTTP helpers ------------------------------------------------------------

struct Resp {
    status: StatusCode,
    body: Value,
}

async fn send(app: &Router, method: &str, uri: &str, token: &str, body: Option<Value>) -> Resp {
    let req = axum::http::Request::builder()
        .method(method)
        .uri(uri)
        .header(header::AUTHORIZATION, format!("Bearer {token}"))
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

fn code_of(r: &Resp) -> String {
    r.body["error"]["code"].as_str().unwrap_or("").to_string()
}

/// Uploads one blob, creates a changeset adding `path` on `parents`,
/// returns its id (all through the API surface).
async fn add_cs(ctx: &Ctx, parents: Value, path: &str, content: &[u8]) -> String {
    let blob = object_id(Kind::Blob, content);
    let raw = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/{REPO}/objects/{}", blob.to_hex()))
        .header(header::AUTHORIZATION, format!("Bearer {}", ctx.writer))
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .body(Body::from(content.to_vec()))
        .unwrap();
    let res = ctx.app.clone().oneshot(raw).await.unwrap();
    assert!(res.status().is_success(), "blob PUT failed");
    let resp = send(
        &ctx.app,
        "POST",
        &format!("/v1/repos/{REPO}/changesets"),
        &ctx.writer,
        Some(json!({
            "parents": parents,
            "author": "Pg <pg@naatre.dev>",
            "message": path,
            "changes": [
                { "path": path, "op": "add", "blob": blob.to_hex(), "size": content.len(), "exec": false }
            ],
        })),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "create: {}", resp.body);
    resp.body["id"].as_str().unwrap().to_string()
}

async fn main_head(ctx: &Ctx) -> String {
    let resp = send(
        &ctx.app,
        "GET",
        &format!("/v1/repos/{REPO}/refs/main"),
        &ctx.writer,
        None,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    resp.body["target"].as_str().unwrap().to_string()
}

async fn land(ctx: &Ctx, head: &str, key: &str) -> Resp {
    send(
        &ctx.app,
        "POST",
        &format!("/v1/repos/{REPO}/land"),
        &ctx.writer,
        Some(json!({ "head": head, "key": key })),
    )
    .await
}

impl Ctx {
    fn derive_calls(&self) -> Vec<Id> {
        self.calls.lock().unwrap().clone()
    }
}

// ---- 1. land idempotency + the writer-busy-shaped retry ----------------------

/// Settled-key replay, settled-key + different head → 409, and the 503
/// `writer_busy` while a write transaction is held. On Postgres the
/// in-process slot is what the test holds; cross-process MVCC contention
/// (serialization failure / deadlock / lock unavailable) maps to the same
/// retryable error shape via `DbError::is_retryable_contention`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn land_idempotency_and_writer_busy_on_postgres() {
    let Some(_) = pg_target() else {
        return; // pg_provision already printed the skip notice
    };
    let (ctx, _repo) = open_ctx_blocking(false);

    let main = main_head(&ctx).await;
    let c1 = add_cs(&ctx, json!([main.clone()]), "a.txt", b"one\n").await;

    // First land settles the key.
    let r1 = land(&ctx, &c1, "k1").await;
    assert_eq!(r1.status, StatusCode::OK, "{}", r1.body);
    let landed = r1.body["main"].as_str().unwrap().to_string();

    // Replay: same key, same head → identical settled answer.
    let r2 = land(&ctx, &c1, "k1").await;
    assert_eq!(r2.status, StatusCode::OK, "{}", r2.body);
    assert_eq!(r2.body["main"].as_str(), Some(landed.as_str()));

    // A different head under the settled key → 409 idempotency_key_settled.
    let new_main = main_head(&ctx).await;
    let c2 = add_cs(&ctx, json!([new_main.clone()]), "b.txt", b"two\n").await;
    let r3 = land(&ctx, &c2, "k1").await;
    assert_eq!(r3.status, StatusCode::CONFLICT, "{}", r3.body);
    assert_eq!(code_of(&r3), "idempotency_key_settled");

    // The writer-busy shape: a held write transaction → 503 retryable;
    // release → the land lands.
    let held = Arc::clone(&ctx.state);
    let holder = tokio::spawn(async move { held.hold_write_txn(400).await });
    tokio::time::sleep(std::time::Duration::from_millis(80)).await;
    let busy = land(&ctx, &c2, "k2").await;
    assert_eq!(
        busy.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        busy.body
    );
    assert_eq!(code_of(&busy), "writer_busy");
    assert_eq!(busy.body["error"]["retryable"], json!(true));
    holder.await.unwrap();

    let r4 = land(&ctx, &c2, "k2").await;
    assert_eq!(r4.status, StatusCode::OK, "{}", r4.body);
}

// ---- 2. conflict → unsettled key → re-land with the SAME key ------------------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conflict_leaves_key_unsettle_then_same_key_relands() {
    let Some(_) = pg_target() else {
        return; // pg_provision already printed the skip notice
    };
    let (ctx, _repo) = open_ctx_blocking(false);

    let main = main_head(&ctx).await;
    // Two children of the same base, both adding the SAME path.
    let a = add_cs(&ctx, json!([main.clone()]), "p.txt", b"from a\n").await;
    let b = add_cs(&ctx, json!([main.clone()]), "p.txt", b"from b\n").await;

    let ra = land(&ctx, &a, "a-1").await;
    assert_eq!(ra.status, StatusCode::OK, "{}", ra.body);

    // B conflicts: 409 with the structured payload; B's key is NOT
    // consumed by the conflict.
    let rb = land(&ctx, &b, "b-1").await;
    assert_eq!(rb.status, StatusCode::CONFLICT, "{}", rb.body);
    assert_eq!(code_of(&rb), "ref_conflict");
    assert!(
        rb.body["error"]["details"]["conflicts"].is_array(),
        "conflict payload carries the conflicts: {}",
        rb.body
    );

    // The resolution: a fresh changeset on TOP of the new main, then the
    // SAME key re-lands it (plan: "a conflict leaves the key unsettled —
    // same-key re-land after a conflict is legal and is the tested recipe
    // path").
    let new_main = main_head(&ctx).await;
    let resolution = add_cs(&ctx, json!([new_main.clone()]), "q.txt", b"resolved\n").await;
    let rr = land(&ctx, &resolution, "b-1").await;
    assert_eq!(rr.status, StatusCode::OK, "{}", rr.body);
}

// ---- 3. concurrent same-key lands join ----------------------------------------

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_same_key_lands_join_identically() {
    let Some(_) = pg_target() else {
        return; // pg_provision already printed the skip notice
    };
    let (ctx, _repo) = open_ctx_blocking(false);

    let main = main_head(&ctx).await;
    let c1 = add_cs(&ctx, json!([main.clone()]), "j.txt", b"join\n").await;

    let app = Arc::new(ctx.app.clone());
    let token = ctx.writer.clone();
    let land_once = |app: Arc<Router>, token: String, head: String| async move {
        send(
            &app,
            "POST",
            &format!("/v1/repos/{REPO}/land"),
            &token,
            Some(json!({ "head": head, "key": "join-key" })),
        )
        .await
    };
    let (ra, rb) = tokio::join!(
        land_once(Arc::clone(&app), token.clone(), c1.clone()),
        land_once(app, token, c1),
    );
    assert_eq!(ra.status, StatusCode::OK, "{}", ra.body);
    assert_eq!(rb.status, StatusCode::OK, "{}", rb.body);
    assert_eq!(
        ra.body["main"].as_str(),
        rb.body["main"].as_str(),
        "the join produces identical settled outcomes"
    );
}

// ---- 4. derive lease: expiry sweep + busy-abort release ------------------------

/// An expired lease sweeps back to pending and the node re-derives
/// (idempotent after a lost lease); a writer-busy at the lease-release
/// step releases WITHOUT burning an attempt (the busy-abort leg).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn derive_lease_expiry_and_busy_abort_release() {
    let Some(_) = pg_target() else {
        return; // pg_provision already printed the skip notice
    };
    let (ctx, repo_id) = open_ctx_blocking(true);
    let kind: i64 = with_seed(&ctx, move |conn| {
        db::derive_kind_id(conn, KIND)
            .unwrap()
            .expect("kind registered")
    })
    .await;

    let main = main_head(&ctx).await;
    let c1 = add_cs(&ctx, json!([main.clone()]), "d.txt", b"derive\n").await;
    let c1_id = Id::from_hex(&c1).unwrap();

    // One pass: the create enqueued it (deferred kind) → derived.
    let rep = derive::derive_pass(&ctx.state).await.unwrap();
    assert_eq!(
        rep.derived, 1,
        "swept={} poisoned={}",
        rep.swept, rep.poisoned
    );
    assert_eq!(ctx.derive_calls().len(), 1);

    // Lost-lease shape: the row is forced back to 'deriving' with an
    // EXPIRED lease (a crashed holder); the sweep burns one attempt and
    // the next pass re-derives.
    let n: usize = with_seed(&ctx, move |conn| {
        conn.execute(
            &Sql::plain(
                "UPDATE derive_state SET state = 'deriving', lease_expires_ns = ?4 \
                 WHERE kind = ?1 AND repo_id = ?2 AND cs = ?3",
            ),
            &pv![kind, repo_id, &c1_id, BASE_NS as i64 - 1],
        )
        .unwrap()
    })
    .await;
    assert_eq!(n, 1, "the probe row exists");
    // One pass does BOTH: the expired lease sweeps (attempt burns) and
    // the drain re-picks the node and re-derives it — idempotent after a
    // lost lease.
    let rep = derive::derive_pass(&ctx.state).await.unwrap();
    assert_eq!(rep.swept, 1, "the expired lease swept");
    assert_eq!(rep.derived, 1, "re-derived in the same pass");
    assert_eq!(
        ctx.derive_calls().len(),
        2,
        "the node derived exactly twice (initial + re-derivation)"
    );

    // Busy-abort: the next pass's mid-derivation write fails writer-busy
    // (the same error a lost single-writer-slot race produces); the lease
    // RELEASES without burning an attempt.
    let c2 = add_cs(&ctx, json!([c1.clone()]), "e.txt", b"more\n").await;
    let c2_id = Id::from_hex(&c2).unwrap();
    *ctx.state.derive_test_busy_abort.lock().unwrap() = Some(derive::BusyAbort::Derive(c2_id));
    let rep = derive::derive_pass(&ctx.state).await.unwrap();
    assert_eq!(rep.derived, 0, "the pass aborted at the busy write");
    *ctx.state.derive_test_busy_abort.lock().unwrap() = None;
    let attempts_before: i64 = with_seed(&ctx, move |conn| {
        conn.query_row_map(
            &Sql::plain("SELECT attempts FROM derive_state WHERE kind = ?1 AND cs = ?2"),
            &pv![kind, &c2_id],
            |r| r.get(0),
        )
        .unwrap()
    })
    .await;
    let rep = derive::derive_pass(&ctx.state).await.unwrap();
    assert_eq!(rep.derived, 1, "the released node re-picks and derives");
    let attempts_after: i64 = with_seed(&ctx, move |conn| {
        conn.query_row_map(
            &Sql::plain("SELECT attempts FROM derive_state WHERE kind = ?1 AND cs = ?2"),
            &pv![kind, &c2_id],
            |r| r.get(0),
        )
        .unwrap()
    })
    .await;
    assert_eq!(
        attempts_before, attempts_after,
        "a busy-abort release must not burn an attempt"
    );
}

// ---- 5. the boot backfill's batch cursor ---------------------------------------

/// The backfill enqueues every rowless changeset in (generation, seq)
/// order through SMALL batches — the cursor arithmetic over the Postgres
/// `seq` (rowid-analog) columns — and its completion marker proves the
/// anti-join empty. The changesets are seeded through the REAL row-writing
/// fn (`changeset_insert_rows`) with no registry: the offline-import
/// shape the backfill exists for.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn backfill_cursor_batches_on_postgres() {
    let Some(target) = pg_target() else {
        return; // pg_provision already printed the skip notice
    };
    let cfg = target.config();
    let dir = tempfile::tempdir().unwrap();
    // Seed BEFORE the state opens (the registry must be empty at seed
    // time — the offline-import shape): the seed connection runs the
    // migrations itself, the state's open then finds them current.
    let connect_cfg = cfg.clone();
    let dir_for_thread = dir.path().to_path_buf();
    let (seed, repo_id, main_target) = std::thread::spawn(move || {
        let seed =
            Db::connect_postgres(connect_cfg.pg.as_ref().unwrap(), false).expect("seed conn");
        // The state opens FIRST (its migrations create the tables); the
        // seed then runs against the migrated schema.
        let _state = ServerState::open_backend(DataDir::open_any(&dir_for_thread), &connect_cfg)
            .expect("pg state opens");
        let (_token, hash) = naatre_server::auth::mint_token();
        db::principal_upsert(&seed, "writer-p", "writer", BASE_NS as i64).unwrap();
        db::token_insert(&seed, "writer-p", &hash, None, None).unwrap();
        let repo_id = db::repos_create(&seed, REPO, BASE_NS as i64).unwrap();
        // main points at a REAL (rootless, empty) changeset: create-path
        // parent validation requires known changeset ids.
        let root_id = Id({
            let mut b = [0u8; 32];
            b[0] = 0x90;
            b
        });
        db::changeset_insert(
            &seed,
            db::ChangesetInsert {
                repo_id,
                repo_name: REPO,
                id: &root_id,
                canonical: &[0u8],
                author: "Seeder",
                message: "root",
                timestamp_ns: BASE_NS as i64,
                generation: 0,
                principal: "seed",
                root_tree_id: &root_id,
                parents: &[],
                changes: &[],
                blobs: &[],
                trees: &[db::TreeWrite {
                    id: root_id,
                    canonical: vec![0u8],
                    entries: vec![],
                }],
                now_ns: BASE_NS as i64,
            },
        )
        .unwrap();
        db::ref_cas(
            &seed,
            &seed,
            db::RefMove {
                repo_id,
                repo_name: REPO,
                name: "main",
                target: &root_id,
                expected: None,
                principal: "seed",
                now_ns: BASE_NS as i64,
            },
        )
        .unwrap();
        (seed, repo_id, root_id)
    })
    .join()
    .unwrap();
    let seed = Arc::new(Mutex::new(seed));

    // Seven changesets, straight through the row-writing fn: no derive
    // rows (the registry is empty at seed time).
    // Unwrap the shared handle for the seeding thread (nothing else uses
    // it in this window) and re-wrap after the join.
    let seed_owned = Arc::try_unwrap(seed)
        .map_err(|_| ())
        .expect("seed handle unique here")
        .into_inner()
        .unwrap();
    let seed = std::thread::spawn(move || {
        let mut prev_target = main_target;
        for i in 0..7i64 {
            let mut id_bytes = [0u8; 32];
            id_bytes[0] = 0xB1;
            id_bytes[1..9].copy_from_slice(&i.to_be_bytes());
            let id = Id(id_bytes);
            let _blob = object_id(Kind::Blob, format!("blob{i}").as_bytes());
            db::changeset_insert(
                &seed_owned,
                db::ChangesetInsert {
                    repo_id,
                    repo_name: REPO,
                    id: &id,
                    canonical: &[0u8],
                    author: "Seeder",
                    message: "backfill seed",
                    timestamp_ns: BASE_NS as i64,
                    generation: i,
                    principal: "seed",
                    root_tree_id: &prev_target,
                    parents: if i == 0 {
                        &[]
                    } else {
                        std::slice::from_ref(&prev_target)
                    },
                    changes: &[],
                    blobs: &[],
                    trees: &[],
                    now_ns: BASE_NS as i64,
                },
            )
            .unwrap();
            prev_target = id;
        }
        seed_owned
    })
    .join()
    .unwrap();
    let seed = Arc::new(Mutex::new(seed));

    // NOW open the state and register the kind: its boot-backfill sees
    // the rowless backlog. (Open runs on a blocking thread — driver work
    // must not happen on a runtime worker.)
    let open_cfg = cfg.clone();
    let open_dir = dir.path().to_path_buf();
    let calls = Arc::new(Mutex::new(Vec::new()));
    let register_calls = Arc::clone(&calls);
    let mut state = tokio::task::spawn_blocking(move || {
        let mut state = ServerState::open_backend(DataDir::open_any(&open_dir), &open_cfg)
            .expect("pg state opens");
        state.snapshot_gc_enabled = false;
        state.derive_queue_enabled = false;
        state.derive_backfill_enabled = false;
        state
            .register_deriver(Arc::new(CountingDeriver {
                calls: register_calls,
            }))
            .expect("kind registers");
        state
    })
    .await
    .unwrap();
    state.derive_backfill_batch_size = 3;
    let state = Arc::new(state);
    // Hold the provisioned cluster (if any) until the leg ends.
    let _target = target;
    let kind: i64 = tokio::task::spawn_blocking({
        let seed = Arc::clone(&seed);
        move || {
            let seed = seed.lock().unwrap();
            db::derive_kind_id(&seed, KIND).unwrap().expect("kind row")
        }
    })
    .await
    .unwrap();

    let rows_before: i64 = tokio::task::spawn_blocking({
        let seed = Arc::clone(&seed);
        move || {
            let seed = seed.lock().unwrap();
            seed.query_row_map(
                &Sql::plain("SELECT COUNT(*) FROM derive_state WHERE kind = ?1"),
                &pv![kind],
                |r| r.get(0),
            )
            .unwrap()
        }
    })
    .await
    .unwrap();
    assert_eq!(rows_before, 0, "the seeded backlog is rowless");

    derive::backfill(&state).await.expect("backfill runs");

    let rows_after: i64 = tokio::task::spawn_blocking({
        let seed = Arc::clone(&seed);
        move || {
            let seed = seed.lock().unwrap();
            seed.query_row_map(
                &Sql::plain("SELECT COUNT(*) FROM derive_state WHERE kind = ?1"),
                &pv![kind],
                |r| r.get(0),
            )
            .unwrap()
        }
    })
    .await
    .unwrap();
    assert_eq!(
        rows_after, 8,
        "every rowless changeset enqueued (7 seeds + the seeded root)"
    );

    // The insertion order matches (generation, seq): the picks, replayed
    // in the queue's own order, are the seeds oldest-first.
    let ordered: Vec<i64> = tokio::task::spawn_blocking({
        let seed = Arc::clone(&seed);
        move || {
            let seed = seed.lock().unwrap();
            seed.query_all_map(
                &Sql::plain_owned(format!(
                    "SELECT c.generation FROM derive_state s \
                     JOIN changesets c ON c.repo_id = s.repo_id AND c.id = s.cs \
                     WHERE s.kind = ?1 ORDER BY {} ASC",
                    seed.rowid("s")
                )),
                &pv![kind],
                |r| r.get(0),
            )
            .unwrap()
        }
    })
    .await
    .unwrap();
    assert_eq!(
        ordered,
        vec![0, 0, 1, 2, 3, 4, 5, 6],
        "cursor order holds: (generation, seq), root first"
    );

    // The completion marker: the anti-join is provably empty.
    let done: bool = tokio::task::spawn_blocking({
        let seed = Arc::clone(&seed);
        move || {
            let seed = seed.lock().unwrap();
            db::derive_backfill_done_get(&seed, KIND).unwrap()
        }
    })
    .await
    .unwrap();
    assert!(done, "backfill set the completion marker");
}

// ---- 6. the route legs the M9 matrix owed Postgres: objects PUT
//         roundtrip, refs CAS, and stats (SUM(bigint) → NUMERIC) -------------

/// GET returning raw bytes (the object fetch leg).
async fn send_bytes(app: &Router, uri: &str, token: &str) -> (StatusCode, axum::body::Bytes) {
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(uri)
        .header(header::AUTHORIZATION, format!("Bearer {token}"))
        .body(Body::empty())
        .unwrap();
    let res = app.clone().oneshot(req).await.unwrap();
    let status = res.status();
    let bytes = BodyExt::collect(res.into_body())
        .await
        .expect("body")
        .to_bytes();
    (status, bytes)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn stats_objects_and_refs_routes_on_postgres() {
    let Some(_) = pg_target() else {
        return; // pg_provision already printed the skip notice
    };
    let (ctx, _repo) = open_ctx_blocking(false);

    // The startup banner's "metadata backend" line reads the kind off the
    // state — postgres on this backend (sqlite side pinned in state.rs).
    assert_eq!(ctx.state.meta_backend, "postgres");

    // objects PUT roundtrip: add_cs uploads through the raw PUT route and
    // creates the referencing changeset; the blob then reads back
    // byte-identical through the membership-gated fetch.
    let content = b"pg route roundtrip\n";
    let blob = object_id(Kind::Blob, content);
    let main = main_head(&ctx).await;
    add_cs(&ctx, json!([main]), "pg-route.txt", content).await;
    let (status, bytes) = send_bytes(
        &ctx.app,
        &format!("/v1/repos/{REPO}/objects/{}", blob.to_hex()),
        &ctx.writer,
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(&bytes[..], content);

    // An admin principal for the admin-only routes (refs CAS, stats).
    let admin = with_seed(&ctx, |conn| {
        let (token, hash) = naatre_server::auth::mint_token();
        db::principal_upsert(conn, "admin-p", "admin", BASE_NS as i64).unwrap();
        db::token_insert(conn, "admin-p", &hash, None, None).unwrap();
        token
    })
    .await;

    // refs CAS on a non-main ref: create (expected null) → v1; a stale
    // expectation → 409 ref_conflict naming the current version; the
    // current one moves it → v2.
    let head = main_head(&ctx).await;
    let create = send(
        &ctx.app,
        "POST",
        &format!("/v1/repos/{REPO}/refs/release"),
        &admin,
        Some(json!({ "target": head, "expected": null })),
    )
    .await;
    assert_eq!(create.status, StatusCode::OK, "{}", create.body);
    assert_eq!(create.body["version"], 1);
    let stale = send(
        &ctx.app,
        "POST",
        &format!("/v1/repos/{REPO}/refs/release"),
        &admin,
        Some(json!({ "target": head, "expected": { "version": 999 } })),
    )
    .await;
    assert_eq!(stale.status, StatusCode::CONFLICT, "{}", stale.body);
    assert_eq!(code_of(&stale), "ref_conflict");
    assert_eq!(stale.body["error"]["details"]["current_version"], 1);
    let moved = send(
        &ctx.app,
        "POST",
        &format!("/v1/repos/{REPO}/refs/release"),
        &admin,
        Some(json!({ "target": head, "expected": { "version": 1 } })),
    )
    .await;
    assert_eq!(moved.status, StatusCode::OK, "{}", moved.body);
    assert_eq!(moved.body["version"], 2);

    // stats: SUM(bigint) answers NUMERIC on Postgres — the mapper arm this
    // fix slice added; the route 500ed before it existed.
    let stats = send(
        &ctx.app,
        "GET",
        &format!("/v1/repos/{REPO}/stats"),
        &admin,
        None,
    )
    .await;
    assert_eq!(stats.status, StatusCode::OK, "{}", stats.body);
    assert!(stats.body["changesets"].as_u64().unwrap() >= 1);
    assert!(stats.body["objects"].as_u64().unwrap() >= 1);
    assert_eq!(stats.body["bytes"].as_u64(), Some(content.len() as u64));
    assert!(stats.body["cbytes"].as_u64().unwrap() >= 1);
}

// ---- 7. the reachability GC leg (M10a on Postgres) ---------------------------

const DAY_NS: u64 = 24 * 3600 * 1_000_000_000;

/// The reachability GC pass on Postgres (the M10a PG leg): mark + batched
/// collection against the PG metadata backend — whose destructive
/// collection transactions take the `BEGIN IMMEDIATE`/advisory-lock writer
/// section on this backend too. Both gates: the landed chain survives
/// byte-identical; the unlanded orphan collects across the phases —
/// changeset + root tree, then the blob's phase-A condemnation, then phase
/// B's delete — and fsck is clean afterwards.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn gc_pass_collects_orphans_on_postgres() {
    let Some(_) = pg_target() else {
        return; // pg_provision already printed the skip notice
    };
    let (ctx, repo_id) = open_ctx_blocking_cfg(false, |state| {
        state.gc_enabled = false;
        // The grace window collapses to zero: candidates collect on the
        // next pass, and the injected clock's ticks give the two-phase
        // delete its pass separation.
        state.gc_grace = std::time::Duration::ZERO;
    });

    // Live: a landed commit (main moves onto it). Orphan: an unlanded
    // commit with a unique blob + unique root tree.
    let main = main_head(&ctx).await;
    let live = b"pg-gc-live\n";
    let live_cs = add_cs(&ctx, json!([main.clone()]), "gc-live.txt", live).await;
    let r = land(&ctx, &live_cs, "k-gc").await;
    assert_eq!(r.status, StatusCode::OK, "{}", r.body);
    let orphan = add_cs(&ctx, json!([main]), "gc-orphan.txt", b"pg-gc-orphan\n").await;
    let orphan_blob = object_id(Kind::Blob, b"pg-gc-orphan\n");
    let orphan_id = Id::from_hex(&orphan).unwrap();

    // Pass 1 (t0): the grace collapse means LEDGER + COLLECT are the same
    // pass — first_ns == cut — so the changeset and its root tree collect
    // immediately (the blob stays: the blob phase runs BEFORE the trees
    // phase of the same pass, and the dying tree's entries still reference
    // it).
    let report = naatre_server::gc::gc_pass(&ctx.state).await.unwrap();
    assert_eq!(report.cs_collected, 1, "{report:?}");
    assert!(report.trees_collected >= 1, "{report:?}");
    // Pass 2 (t1): the now-unreferenced blob is CONDEMNED (bytes stay).
    ctx._clock.store(BASE_NS + DAY_NS, Ordering::Relaxed);
    let report = naatre_server::gc::gc_pass(&ctx.state).await.unwrap();
    assert_eq!(report.blobs_condemned, 1, "{report:?}");
    // Pass 3 (t2): phase B deletes bytes + rows; the ledger drains.
    ctx._clock.store(BASE_NS + 2 * DAY_NS, Ordering::Relaxed);
    let report = naatre_server::gc::gc_pass(&ctx.state).await.unwrap();
    assert_eq!(report.blobs_collected, 1, "{report:?}");

    let orphan_gone = {
        let cas = Arc::clone(&ctx.state.cas);
        with_seed(&ctx, move |conn| {
            db::changeset_get(conn, repo_id, &orphan_id)
                .unwrap()
                .is_none()
                && !naatre_store::BlobStore::has(cas.as_ref(), &[orphan_blob]).unwrap()[0]
        })
        .await
    };
    assert!(orphan_gone, "the orphan collected fully");

    // The landed chain survives byte-identical.
    let (status, bytes) = send_bytes(
        &ctx.app,
        &format!(
            "/v1/repos/{REPO}/objects/{}",
            object_id(Kind::Blob, live).to_hex()
        ),
        &ctx.writer,
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(&bytes[..], live);

    let report = naatre_server::fsck::fsck(&ctx.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.bytes_missing, 0);
}

// ---- 8. the NUMERIC wire-format leg: large sums ------------------------------

/// SUM(bigint) answers NUMERIC on Postgres, and PG strips trailing
/// base-10000 zero groups from the numeric wire form — a total like
/// 40_019_999 used to parse as 14_000 (the stripped scale factor dropped:
/// 40_000_000 wired as the single digit 4000 parsed as 4_000). Rows seeded
/// straight into `objects`/`repo_blobs` pin the exact total through
/// [`db::repo_stats`].
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn repo_stats_parses_large_numeric_sums_on_postgres() {
    let Some(_) = pg_target() else {
        return; // pg_provision already printed the skip notice
    };
    let (ctx, repo_id) = open_ctx_blocking(false);

    let id = |tag: u8| {
        let mut b = [0u8; 32];
        b[0] = tag;
        b
    };
    // 40_000_000 (the stripped shape), 10_000 (one trailing zero group),
    // 9_999 (no stripping) — the sum asserts the factor survives.
    let rows: Vec<([u8; 32], i64)> = vec![
        (id(0xA1), 40_000_000),
        (id(0xA2), 10_000),
        (id(0xA3), 9_999),
    ];
    with_seed(&ctx, move |conn| {
        for (oid, size) in &rows {
            conn.execute(
                &Sql::plain(
                    "INSERT INTO objects(object_id, size, csize, touched_ns) \
                     VALUES (?1, ?2, 1, 0)",
                ),
                &pv![oid.as_slice(), *size],
            )
            .unwrap();
            conn.execute(
                &Sql::plain("INSERT INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)"),
                &pv![repo_id, oid.as_slice()],
            )
            .unwrap();
        }
    })
    .await;

    let (changesets, objects, bytes, _cbytes) = with_seed(&ctx, move |conn| {
        let changesets = db::repo_changeset_count(conn, repo_id).unwrap();
        let members = db::gc_repo_blobs(conn, repo_id).unwrap();
        let (objects, bytes, cbytes) = db::repo_object_sums(conn, &members).unwrap();
        (changesets, objects, bytes, cbytes)
    })
    .await;
    assert_eq!(
        bytes,
        40_000_000 + 10_000 + 9_999,
        "the scale factor survives the wire"
    );
    assert_eq!(objects, 3);
    assert!(changesets >= 1, "the seeded root changeset");
}
