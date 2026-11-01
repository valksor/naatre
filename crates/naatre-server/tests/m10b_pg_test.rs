//! M10b on Postgres: the named PG subset of the redaction slice — the
//! universal GATE (by-id changeset fetch → 410 `object_redacted`, `/log`
//! drops the tombstoned node) and the reference-aware PURGE (the GC's
//! redaction phase stamps `purged_ns` on PG, the purge audit row lands,
//! and fsck's redaction counts read the same schema). The full gate matrix
//! and the crash pins stay SQLite-side (they exercise the same statement
//! layer; the PG twin proves the schema + statements, including migration
//! 10's PG half). Every leg SKIPS with a printed notice when no PostgreSQL
//! target can be produced.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use axum::Router;
use axum::http::StatusCode;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::fsck;
use naatre_server::gc::gc_pass;
use naatre_server::state::{AppState, ServerState};
use naatre_server::{db, derive};
use naatre_store::backend::BackendConfig;
use naatre_store::{DataDir, Db, Sql, pv};

const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m10bpg";
const KIND: &str = "m10bpg-probe";

#[path = "../../naatre-store/tests/pg_provision/mod.rs"]
mod pg_provision;

/// A throwaway deferred deriver (the m9pg shape): emits a fixed output.
struct CountingDeriver;

impl derive::Deriver for CountingDeriver {
    fn kind(&self) -> &'static str {
        KIND
    }

    fn derive(
        &self,
        _ctx: &derive::DeriveCtx,
        _conn: &Db,
        _repo_id: i64,
        _cs: Id,
        _parents_done: &[derive::ParentOut],
    ) -> Result<derive::DerivedOut, derive::DeriveFailure> {
        Ok(derive::DerivedOut(vec![1u8]))
    }
}

struct Ctx {
    _dir: tempfile::TempDir,
    app: Router,
    state: AppState,
    admin: String,
    writer: String,
    /// Holds the provisioned cluster (if any) alive for the leg.
    _target: Option<pg_provision::PgTarget>,
    _clock: Arc<AtomicU64>,
    /// One pre-opened seed connection: the sync Postgres client runs every
    /// query on its own runtime, so ALL direct db:: use goes through
    /// [`Ctx::with_seed`] (a plain thread, the production shape).
    seed: Arc<Mutex<Db>>,
}

impl Ctx {
    async fn with_seed<T, F>(&self, f: F) -> T
    where
        F: FnOnce(&Db) -> T + Send + 'static,
        T: Send + 'static,
    {
        let seed = Arc::clone(&self.seed);
        tokio::task::spawn_blocking(move || {
            let guard = seed.lock().unwrap();
            f(&guard)
        })
        .await
        .expect("seed thread")
    }
}

/// Builds the state + router over a FRESH PG schema; seeds the repo, a
/// root changeset on main, an admin and a writer token.
fn open_ctx() -> Option<Ctx> {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let target = pg_provision::acquire()?;
    let cfg: BackendConfig = target.config();
    let dir = tempfile::tempdir().unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let connect_cfg = cfg.clone();
    let dir_for_thread = dir.path().to_path_buf();
    let clock_for_thread = Arc::clone(&clock);
    let (state, seed, admin, writer) = std::thread::spawn(move || {
        let conn =
            Db::connect_postgres(connect_cfg.pg.as_ref().unwrap(), false).expect("seed conn");
        let mut state = ServerState::open_backend(DataDir::open_any(&dir_for_thread), &connect_cfg)
            .expect("pg state opens");
        let clock_for_closure = Arc::clone(&clock_for_thread);
        state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
        state.snapshot_gc_enabled = false;
        state.gc_enabled = false;
        state.derive_queue_enabled = false;
        state.derive_backfill_enabled = false;
        state
            .register_deriver(Arc::new(CountingDeriver))
            .expect("kind registers");
        let repo_id = db::repos_create(&conn, REPO, BASE_NS as i64).expect("repo");
        let root_id = Id({
            let mut b = [0u8; 32];
            b[0] = 0x8E;
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
        .expect("main");
        let (admin_token, admin_hash) = naatre_server::auth::mint_token();
        db::token_create(
            &conn,
            "admin",
            "admin",
            &admin_hash,
            None,
            None,
            BASE_NS as i64,
        )
        .expect("admin token");
        let (writer_token, writer_hash) = naatre_server::auth::mint_token();
        db::token_create(
            &conn,
            "writer-p",
            "writer",
            &writer_hash,
            None,
            None,
            BASE_NS as i64,
        )
        .expect("writer token");
        (state, conn, admin_token, writer_token)
    })
    .join()
    .expect("seed thread");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    Some(Ctx {
        _dir: dir,
        app,
        state,
        admin,
        writer,
        _target: Some(target),
        _clock: clock,
        seed: Arc::new(Mutex::new(seed)),
    })
}

async fn get_on(app: &Router, uri: &str, token: &str) -> (StatusCode, Value) {
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(uri)
        .header(axum::http::header::AUTHORIZATION, format!("Bearer {token}"))
        .body(axum::body::Body::empty())
        .unwrap();
    let res = app.clone().oneshot(req).await.unwrap();
    let status = res.status();
    let body = http_body_util::BodyExt::collect(res.into_body())
        .await
        .unwrap()
        .to_bytes();
    let body: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
    (status, body)
}

async fn post_json_on(app: &Router, uri: &str, token: &str, value: &Value) -> (StatusCode, Value) {
    let req = axum::http::Request::builder()
        .method("POST")
        .uri(uri)
        .header(axum::http::header::AUTHORIZATION, format!("Bearer {token}"))
        .header(axum::http::header::CONTENT_TYPE, "application/json")
        .body(axum::body::Body::from(value.to_string()))
        .unwrap();
    let res = app.clone().oneshot(req).await.unwrap();
    let status = res.status();
    let body = http_body_util::BodyExt::collect(res.into_body())
        .await
        .unwrap()
        .to_bytes();
    (status, serde_json::from_slice(&body).unwrap_or(Value::Null))
}

/// The gate leg: redact a changeset through the admin route on PG → the
/// by-id fetch answers 410 `object_redacted`, `/log` drops the node, and
/// the tombstone row is the repo-scoped PG row.
#[tokio::test]
async fn pg_redaction_gate() {
    let Some(ctx) = open_ctx() else {
        return;
    };
    let root = ctx
        .with_seed(|conn| {
            let hex: Vec<u8> = conn
                .query_row_map(
                    &Sql::plain("SELECT target FROM refs WHERE name = 'main'"),
                    &pv![],
                    |r| r.get(0),
                )
                .expect("main target");
            let arr: [u8; 32] = hex.try_into().expect("32 bytes");
            Id(arr).to_hex()
        })
        .await;
    let child = {
        // A child changeset through the route (the m9pg shape: the writer
        // posts; no blob needed — an empty-delta commit is refused, so add
        // one file via a real PUT).
        let content = b"pg-redact-content".to_vec();
        let blob_hex = object_id(Kind::Blob, &content).to_hex();
        let put = axum::http::Request::builder()
            .method("PUT")
            .uri(format!("/v1/repos/{REPO}/objects/{blob_hex}"))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", ctx.writer),
            )
            .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
            .body(axum::body::Body::from(content.clone()))
            .unwrap();
        let res = ctx.app.clone().oneshot(put).await.unwrap();
        assert_eq!(res.status(), StatusCode::CREATED);
        let (status, body) = post_json_on(
            &ctx.app,
            &format!("/v1/repos/{REPO}/changesets"),
            &ctx.writer,
            &json!({
                "parents": [root],
                "author": "Agent <agent@naatre.dev>",
                "message": "child",
                "changes": [{"path": "c.txt", "op": "add", "blob": blob_hex, "size": content.len(), "exec": false}],
            }),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED, "{body}");
        body["id"].as_str().unwrap().to_string()
    };

    // Mark through the admin route (PG).
    let (status, body) = post_json_on(
        &ctx.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &ctx.admin,
        &json!({ "id": child, "kind": "changeset", "reason": "pg gate leg" }),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{body}");

    // Idempotent re-POST: 200, the earliest mark stands.
    let (status, body2) = post_json_on(
        &ctx.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &ctx.admin,
        &json!({ "id": child, "kind": "changeset", "reason": "again" }),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body2}");
    assert_eq!(
        body2["redaction"]["marked_ns"],
        body["redaction"]["marked_ns"]
    );

    // The gate: by-id fetch 410, /log drops the node.
    let (status, body) = get_on(
        &ctx.app,
        &format!("/v1/repos/{REPO}/changesets/{child}"),
        &ctx.writer,
    )
    .await;
    assert_eq!(status, StatusCode::GONE, "{body}");
    assert_eq!(body["error"]["code"], "object_redacted");
    let (status, body) = get_on(&ctx.app, &format!("/v1/repos/{REPO}/log"), &ctx.writer).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let ids: Vec<&str> = body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids, vec![root.as_str()], "{body}");

    // The PG tombstone's column set is EXACTLY the clause's path + date
    // set — no reason, no principal, never author; the descriptive payload
    // is the object's first path.
    let columns = ctx
        .with_seed(|conn| {
            conn.query_all_map(
                &Sql::plain(
                    "SELECT column_name FROM information_schema.columns \
                     WHERE table_name = 'redactions' ORDER BY ordinal_position",
                ),
                &pv![],
                |r| r.get::<String>(0),
            )
            .expect("pg columns")
        })
        .await;
    assert_eq!(
        columns,
        vec![
            "repo_id".to_string(),
            "object_id".to_string(),
            "kind".to_string(),
            "path".to_string(),
            "marked_ns".to_string(),
            "purged_ns".to_string(),
        ],
        "the PG tombstone column set is exactly the plan's path + date set"
    );
    let (kind, path, audit_principal) = ctx
        .with_seed(move |conn| {
            let row = conn
                .query_row_map(
                    &Sql::plain("SELECT kind, path FROM redactions WHERE object_id = ?1"),
                    &pv![Id::from_hex(&child).unwrap().0.as_slice()],
                    |r| Ok((r.get::<String>(0)?, r.get::<Option<String>>(1)?)),
                )
                .expect("redaction row");
            let actor: String = conn
                .query_row_map(
                    &Sql::plain("SELECT principal FROM audit_log WHERE action = 'redact'"),
                    &pv![],
                    |r| r.get(0),
                )
                .expect("redact audit row");
            (row.0, row.1, actor)
        })
        .await;
    assert_eq!(kind, "changeset");
    assert_eq!(path.as_deref(), Some("c.txt"), "the tombstone's path");
    assert_eq!(audit_principal, "admin", "the actor lives in the audit row");
}

/// The purge leg: an unlanded tombstoned changeset purges on the first
/// `gc_pass` (PG's redaction phase): rows gone, `purged_ns` stamped, ONE
/// `redact_purge` audit row, fsck clean.
#[tokio::test]
async fn pg_redaction_purge() {
    let Some(ctx) = open_ctx() else {
        return;
    };
    let root = ctx
        .with_seed(|conn| {
            let hex: Vec<u8> = conn
                .query_row_map(
                    &Sql::plain("SELECT target FROM refs WHERE name = 'main'"),
                    &pv![],
                    |r| r.get(0),
                )
                .expect("main target");
            let arr: [u8; 32] = hex.try_into().expect("32 bytes");
            Id(arr).to_hex()
        })
        .await;
    // An unlanded child: no ref, no children → purgeable at once.
    let orphan = {
        let content = b"pg-orphan-content".to_vec();
        let blob_hex = object_id(Kind::Blob, &content).to_hex();
        let put = axum::http::Request::builder()
            .method("PUT")
            .uri(format!("/v1/repos/{REPO}/objects/{blob_hex}"))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", ctx.writer),
            )
            .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
            .body(axum::body::Body::from(content.clone()))
            .unwrap();
        let res = ctx.app.clone().oneshot(put).await.unwrap();
        assert_eq!(res.status(), StatusCode::CREATED);
        let (status, body) = post_json_on(
            &ctx.app,
            &format!("/v1/repos/{REPO}/changesets"),
            &ctx.writer,
            &json!({
                "parents": [root],
                "author": "Agent <agent@naatre.dev>",
                "message": "orphan",
                "changes": [{"path": "o.txt", "op": "add", "blob": blob_hex, "size": content.len(), "exec": false}],
            }),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED, "{body}");
        body["id"].as_str().unwrap().to_string()
    };
    let (status, _) = post_json_on(
        &ctx.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &ctx.admin,
        &json!({ "id": orphan, "kind": "changeset", "reason": "pg purge leg" }),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    let report = gc_pass(&ctx.state).await.unwrap();
    assert_eq!(report.redactions_purged_cs, 1, "{report:?}");

    let (rows, stamped): (i64, Option<i64>) = ctx
        .with_seed(move |conn| {
            let orphan_bytes = Id::from_hex(&orphan).unwrap().0.to_vec();
            conn.query_row_map(
                &Sql::plain(
                    "SELECT \
                     (SELECT COUNT(*) FROM changesets WHERE id = ?1), \
                     (SELECT purged_ns FROM redactions WHERE object_id = ?1)",
                ),
                &pv![orphan_bytes],
                |r| Ok((r.get::<i64>(0)?, r.get::<Option<i64>>(1)?)),
            )
            .expect("purge state")
        })
        .await;
    assert_eq!(rows, 0, "the tombstoned changeset row is gone");
    assert!(stamped.is_some(), "purged_ns stamped on PG");

    let audit_rows: i64 = ctx
        .with_seed(|conn| {
            conn.query_row_map(
                &Sql::plain("SELECT COUNT(*) FROM audit_log WHERE action = 'redact_purge'"),
                &pv![],
                |r| r.get(0),
            )
            .expect("audit count")
        })
        .await;
    assert_eq!(audit_rows, 1, "one purge audit row");

    let report = fsck::fsck(&ctx.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.redactions_purged, 1, "{report:?}");
    assert_eq!(report.redaction_purge_drift, 0);
}

/// The dir-aggregate mask leg on real Postgres (pre-commit correction):
/// the recursive-CTE probe behind `/trees` listings must run on the PG
/// dialect — the seed selects from `trees` (typed bytea), never bare
/// VALUES params, which the recursion would reject as text-vs-bytea. A
/// repo whose tree carries a dir with a redacted descendant plus a clean
/// sibling dir: the dirty dir serves size-null, the sibling keeps its
/// size, and the masked file entry serves object_id/size/exec null with
/// `redacted: true`.
#[tokio::test]
async fn pg_tree_dir_aggregate_mask() {
    let Some(ctx) = open_ctx() else {
        return;
    };
    let root = ctx
        .with_seed(|conn| {
            let hex: Vec<u8> = conn
                .query_row_map(
                    &Sql::plain("SELECT target FROM refs WHERE name = 'main'"),
                    &pv![],
                    |r| r.get(0),
                )
                .expect("main target");
            let arr: [u8; 32] = hex.try_into().expect("32 bytes");
            Id(arr).to_hex()
        })
        .await;

    let secret = b"pg-dir-secret!".to_vec();
    let secret_hex = object_id(Kind::Blob, &secret).to_hex();
    let clean = b"pg-dir-clean!".to_vec();
    let clean_hex = object_id(Kind::Blob, &clean).to_hex();
    for hex in [&secret_hex, &clean_hex] {
        let content = if *hex == secret_hex { &secret } else { &clean };
        let put = axum::http::Request::builder()
            .method("PUT")
            .uri(format!("/v1/repos/{REPO}/objects/{hex}"))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", ctx.writer),
            )
            .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
            .body(axum::body::Body::from(content.clone()))
            .unwrap();
        let res = ctx.app.clone().oneshot(put).await.unwrap();
        assert_eq!(res.status(), StatusCode::CREATED);
    }
    let (status, body) = post_json_on(
        &ctx.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &ctx.writer,
        &json!({
            "parents": [root],
            "author": "Agent <agent@naatre.dev>",
            "message": "dirs",
            "changes": [
                {"path": "d/clean.txt", "op": "add", "blob": clean_hex, "size": clean.len(), "exec": false},
                {"path": "d/secret.txt", "op": "add", "blob": secret_hex, "size": secret.len(), "exec": false},
                {"path": "sib/other.txt", "op": "add", "blob": clean_hex, "size": clean.len(), "exec": false},
            ],
        }),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{body}");
    let root_tree = body["root_tree_id"].as_str().unwrap().to_string();

    let (status, body) = post_json_on(
        &ctx.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &ctx.admin,
        &json!({ "id": secret_hex, "kind": "blob", "reason": "pg dir-aggregate leg" }),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{body}");

    // The listing itself: a PG-side statement failure would 500 here.
    let (status, body) = get_on(
        &ctx.app,
        &format!("/v1/repos/{REPO}/trees/{root_tree}"),
        &ctx.writer,
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let entries = body["entries"].as_array().unwrap();
    let d = entries.iter().find(|e| e["name"] == "d").unwrap();
    assert_eq!(d["kind"], "dir");
    assert_eq!(d["size"], Value::Null, "the aggregate mask: {body}");
    assert_eq!(
        d["redacted"], false,
        "the dir's own tree id is not redacted"
    );
    assert!(d["object_id"].is_string(), "the child tree pointer serves");
    let sib = entries.iter().find(|e| e["name"] == "sib").unwrap();
    assert_eq!(
        sib["size"],
        json!(clean.len()),
        "the sibling keeps its size"
    );
    assert_eq!(sib["redacted"], false);

    // Inside the dirty dir: the masked file entry, all three fields null.
    let d_tree = d["object_id"].as_str().unwrap().to_string();
    let (status, body) = get_on(
        &ctx.app,
        &format!("/v1/repos/{REPO}/trees/{d_tree}"),
        &ctx.writer,
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let entries = body["entries"].as_array().unwrap();
    let masked = entries.iter().find(|e| e["name"] == "secret.txt").unwrap();
    assert_eq!(masked["object_id"], Value::Null, "{body}");
    assert_eq!(masked["size"], Value::Null);
    assert_eq!(masked["exec"], Value::Null);
    assert_eq!(masked["redacted"], true);
    let ok = entries.iter().find(|e| e["name"] == "clean.txt").unwrap();
    assert_eq!(ok["size"], json!(clean.len()));
    assert_eq!(ok["exec"], json!(false));
    assert_eq!(ok["redacted"], false);
}
