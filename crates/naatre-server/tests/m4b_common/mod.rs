//! Shared harness for the M4b suites (crash matrix, concurrency, the
//! re-anchored M2a oracle). Same technique as the M4a suite: an in-process
//! router over a temp-dir store, direct-SQL seeding for principals/refs,
//! router calls for everything on the API surface. Free functions (`*_on`,
//! taking a db path or a router) serve both suite styles and the crash
//! suite's reexec child, which opens a data dir the parent created.
//!
//! `#[allow(dead_code)]` because each suite includes the module and uses
//! only the helpers it needs.

#![allow(dead_code)]

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use rusqlite::Connection;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

/// Fixed-step clock base: ns for 2023-11-14.
pub const BASE_NS: u64 = 1_700_000_000_000_000_000;
pub const REPO: &str = "m4b";

/// Every bounded wait in the M4b suites hangs off this budget.
pub const DEADLINE: Duration = Duration::from_secs(30);

// ---- app construction -------------------------------------------------------

pub struct TestApp {
    _dir: tempfile::TempDir,
    pub app: Router,
    pub db_path: PathBuf,
    /// Kept so `Arc` clones of the state outlive helper calls.
    pub state: AppState,
}

pub fn build_app() -> TestApp {
    build_app_in(tempfile::tempdir().unwrap())
}

/// Builds the app over a caller-owned temp dir (the crash suite pre-creates
/// the data dir so a spawned child can open the same path).
pub fn build_app_in(dir: tempfile::TempDir) -> TestApp {
    let data = DataDir::create(dir.path()).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let db_path = dir.path().join("naatre.db");
    let app = attach_app(dir.path());
    TestApp {
        _dir: dir,
        db_path,
        app: app.app,
        state: app.state,
    }
}

/// The app over a data dir that already exists (the reexec child: the
/// parent created and initialized the dir; the parent's re-land phase:
/// the child left it behind).
pub struct AttachedApp {
    pub app: Router,
    pub db_path: PathBuf,
    pub state: AppState,
}

pub fn attach_app(dir: &Path) -> AttachedApp {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let data = DataDir::open(dir).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    let db_path = dir.join("naatre.db");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    AttachedApp {
        app,
        db_path,
        state,
    }
}

// ---- direct-SQL seeding (db path or connection) ------------------------------

pub fn conn_at(db_path: &Path) -> Connection {
    let conn = Connection::open(db_path).unwrap();
    conn.busy_timeout(Duration::from_secs(5)).unwrap();
    conn
}

pub fn seed_token_at(
    db_path: &Path,
    principal: &str,
    role: &str,
    scope_path: Option<&str>,
) -> String {
    let (token, hash) = naatre_server::auth::mint_token();
    let conn = conn_at(db_path);
    conn.execute(
        "INSERT INTO principals(name, role, created_ns) VALUES (?1, ?2, ?3) \
         ON CONFLICT(name) DO UPDATE SET role = excluded.role",
        rusqlite::params![principal, role, BASE_NS as i64],
    )
    .unwrap();
    conn.execute(
        "INSERT INTO tokens(principal, blake3, expires_ns, revoked, scope_path) \
         VALUES (?1, ?2, ?3, 0, ?4)",
        rusqlite::params![principal, hash, Option::<i64>::None, scope_path],
    )
    .unwrap();
    token
}

pub fn seed_repo_at(db_path: &Path, name: &str) -> i64 {
    let conn = conn_at(db_path);
    conn.execute(
        "INSERT INTO repos(name, created_ns) VALUES (?1, ?2)",
        rusqlite::params![name, BASE_NS as i64],
    )
    .unwrap();
    conn.last_insert_rowid()
}

/// Seeds the `main` ref the way land leaves it (version 1).
pub fn seed_main_at(db_path: &Path, repo_id: i64, target_hex: &str) {
    let target = Id::from_hex(target_hex).unwrap();
    let conn = conn_at(db_path);
    conn.execute(
        "INSERT INTO refs(repo_id, name, target, version, last_principal) \
         VALUES (?1, 'main', ?2, 1, 'seed') \
         ON CONFLICT(repo_id, name) DO UPDATE SET target = excluded.target",
        rusqlite::params![repo_id, target.0.as_slice()],
    )
    .unwrap();
}

/// Moves main without a version bump (the head/main fork setup).
pub fn move_main_at(db_path: &Path, repo_id: i64, target_hex: &str) {
    let target = Id::from_hex(target_hex).unwrap();
    let conn = conn_at(db_path);
    conn.execute(
        "UPDATE refs SET target = ?2 WHERE repo_id = ?1 AND name = 'main'",
        rusqlite::params![repo_id, target.0.as_slice()],
    )
    .unwrap();
}

pub fn count_at(db_path: &Path, sql: &str) -> i64 {
    conn_at(db_path)
        .query_row(sql, [], |r| r.get(0))
        .expect("count query")
}

pub fn count_params_at(db_path: &Path, sql: &str, params: &[&dyn rusqlite::ToSql]) -> i64 {
    conn_at(db_path)
        .query_row(sql, params, |r| r.get(0))
        .expect("count query")
}

/// The `main` ref target as hex, when the ref exists.
pub fn main_target_at(db_path: &Path, repo_id: i64) -> Option<String> {
    conn_at(db_path)
        .query_row(
            "SELECT target FROM refs WHERE repo_id = ?1 AND name = 'main'",
            [repo_id],
            |r| r.get::<_, Vec<u8>>(0),
        )
        .ok()
        .map(|b| Id(b.try_into().expect("32 bytes")).to_hex())
}

// ---- HTTP helpers ------------------------------------------------------------

pub struct Resp {
    pub status: StatusCode,
    pub body: Value,
}

pub async fn send(
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

pub async fn get_on(app: &Router, uri: &str, token: &str) -> Resp {
    send(app, "GET", uri, Some(token), None).await
}

pub async fn post_json_on(app: &Router, uri: &str, token: &str, value: &Value) -> Resp {
    send(app, "POST", uri, Some(token), Some(value.clone())).await
}

pub fn code_of(r: &Resp) -> String {
    r.body["error"]["code"].as_str().unwrap_or("").to_string()
}

pub fn blob_hex(content: &[u8]) -> String {
    object_id(Kind::Blob, content).to_hex()
}

/// Uploads one blob through the raw PUT, returning its hex id.
pub async fn put_blob_on(app: &Router, token: &str, content: &[u8]) -> String {
    let id = blob_hex(content);
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/{REPO}/objects/{id}"))
        .header(header::AUTHORIZATION, format!("Bearer {token}"))
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .body(Body::from(content.to_vec()))
        .unwrap();
    let res = app.clone().oneshot(req).await.unwrap();
    assert!(
        res.status().is_success(),
        "blob PUT failed: {}",
        res.status()
    );
    id
}

/// Creates a changeset, asserting success, returning (id, root_tree_id).
pub async fn create_ok_on(app: &Router, token: &str, body: &Value) -> (String, String) {
    let resp = post_json_on(app, &format!("/v1/repos/{REPO}/changesets"), token, body).await;
    assert_eq!(resp.status, StatusCode::CREATED, "body: {}", resp.body);
    (
        resp.body["id"].as_str().unwrap().to_string(),
        resp.body["root_tree_id"].as_str().unwrap().to_string(),
    )
}

pub fn add(path: &str, blob: &str, size: usize) -> Value {
    json!({ "path": path, "op": "add", "blob": blob, "size": size, "exec": false })
}

pub fn mod_(path: &str, blob: &str, size: usize) -> Value {
    json!({ "path": path, "op": "mod", "blob": blob, "size": size, "exec": false })
}

pub fn cs_body(parents: Value, changes: Value, message: &str) -> Value {
    json!({
        "parents": parents,
        "author": "Agent <agent@naatre.dev>",
        "message": message,
        "changes": changes,
    })
}

pub async fn land_on(app: &Router, token: &str, head: &str, key: &str) -> Resp {
    post_json_on(
        app,
        &format!("/v1/repos/{REPO}/land"),
        token,
        &json!({ "head": head, "key": key }),
    )
    .await
}

/// Raw blob bytes at `at`/`path` through /file + /objects.
pub async fn cat_on(app: &Router, token: &str, at: &str, path: &str) -> Vec<u8> {
    let resp = get_on(
        app,
        &format!(
            "/v1/repos/{REPO}/file?at={at}&path={}",
            path.replace('/', "%2F")
        ),
        token,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "cat {path}: {}", resp.body);
    let blob = resp.body["blob_id"].as_str().unwrap().to_string();
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(format!("/v1/repos/{REPO}/objects/{blob}"))
        .header(header::AUTHORIZATION, format!("Bearer {token}"))
        .body(Body::empty())
        .unwrap();
    let res = app.clone().oneshot(req).await.unwrap();
    assert!(res.status().is_success());
    BodyExt::collect(res.into_body())
        .await
        .unwrap()
        .to_bytes()
        .to_vec()
}

/// First-parent chain from `from`, following cursors with `limit`-sized
/// pages, until exhaustion (the chain is finite and /log always answers).
pub async fn log_chain_paged(app: &Router, token: &str, from: &str, limit: i64) -> Vec<String> {
    let mut ids = Vec::new();
    let mut cursor: Option<String> = None;
    loop {
        let uri = match &cursor {
            None => format!("/v1/repos/{REPO}/log?from={from}&limit={limit}"),
            Some(c) => format!("/v1/repos/{REPO}/log?limit={limit}&cursor={c}"),
        };
        let resp = get_on(app, &uri, token).await;
        assert_eq!(resp.status, StatusCode::OK, "log page: {}", resp.body);
        for e in resp.body["entries"].as_array().unwrap() {
            ids.push(e["id"].as_str().unwrap().to_string());
        }
        match resp.body["next_cursor"].as_str() {
            Some(c) => cursor = Some(c.to_string()),
            None => break,
        }
    }
    ids
}

/// Recursively flattens the tree at `root` into `(path, blob, size, exec)`
/// through the /trees surface — the materialization direction of the M2a
/// oracle.
pub async fn materialize_tree(
    app: &Router,
    token: &str,
    root: &str,
) -> Vec<(String, String, u64, bool)> {
    let mut out = Vec::new();
    materialize_into(app, token, root, String::new(), &mut out).await;
    out.sort_by(|a, b| a.0.cmp(&b.0));
    out
}

async fn materialize_into(
    app: &Router,
    token: &str,
    tree: &str,
    prefix: String,
    out: &mut Vec<(String, String, u64, bool)>,
) {
    let resp = get_on(app, &format!("/v1/repos/{REPO}/trees/{tree}"), token).await;
    assert_eq!(resp.status, StatusCode::OK, "trees {tree}: {}", resp.body);
    for e in resp.body["entries"].as_array().unwrap() {
        let name = e["name"].as_str().unwrap().to_string();
        let path = if prefix.is_empty() {
            name
        } else {
            format!("{prefix}/{name}")
        };
        match e["kind"].as_str().unwrap() {
            "dir" => {
                let sub = e["object_id"].as_str().unwrap().to_string();
                Box::pin(materialize_into(app, token, &sub, path, out)).await;
            }
            _ => out.push((
                path,
                e["object_id"].as_str().unwrap().to_string(),
                e["size"].as_u64().unwrap(),
                e["exec"].as_bool().unwrap(),
            )),
        }
    }
}

// ---- suite-side fixture ------------------------------------------------------

/// Fixture: app + writer/reader tokens + repo.
pub struct Fixture {
    pub t: TestApp,
    pub writer: String,
    pub reader: String,
    pub repo_id: i64,
}

pub fn fixture() -> Fixture {
    let t = build_app();
    let writer = seed_token_at(&t.db_path, "writer-p", "writer", None);
    let reader = seed_token_at(&t.db_path, "reader-p", "reader", None);
    let repo_id = seed_repo_at(&t.db_path, REPO);
    Fixture {
        t,
        writer,
        reader,
        repo_id,
    }
}

impl Fixture {
    pub fn seed_main(&self, target_hex: &str) {
        seed_main_at(&self.t.db_path, self.repo_id, target_hex);
    }

    pub fn move_main(&self, target_hex: &str) {
        move_main_at(&self.t.db_path, self.repo_id, target_hex);
    }

    pub async fn put_blob(&self, content: &[u8]) -> String {
        put_blob_on(&self.t.app, &self.writer, content).await
    }

    pub async fn create_ok(&self, body: &Value) -> (String, String) {
        create_ok_on(&self.t.app, &self.writer, body).await
    }

    pub async fn land(&self, head: &str, key: &str) -> Resp {
        land_on(&self.t.app, &self.writer, head, key).await
    }

    pub async fn get(&self, uri: &str) -> Resp {
        get_on(&self.t.app, uri, &self.reader).await
    }

    pub async fn cat(&self, at: &str, path: &str) -> Vec<u8> {
        cat_on(&self.t.app, &self.reader, at, path).await
    }

    pub fn count(&self, sql: &str) -> i64 {
        count_at(&self.t.db_path, sql)
    }

    pub fn count_params(&self, sql: &str, params: &[&dyn rusqlite::ToSql]) -> i64 {
        count_params_at(&self.t.db_path, sql, params)
    }

    pub fn main_target(&self) -> Option<String> {
        main_target_at(&self.t.db_path, self.repo_id)
    }
}
