//! M2a suite: changeset create with the full validation matrix (every
//! failure asserts its code AND that zero rows appeared), tree derivation
//! with the replay-from-empty oracle, the history reads (log pagination,
//! diff, merge-base, file), and the refs fast-forward gate.
//!
//! Note: `main` stays land-only (the refs route refuses it, per the pinned
//! route matrix), so the log default-`from` test seeds the `main` ref row
//! directly — the same row M4's land will move — and the non-FF gate tests
//! run on a `release/dev` ref.

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{HeaderMap, StatusCode, header};
use http_body_util::BodyExt;
use rusqlite::Connection;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::ServerState;
use naatre_server::tree_derive::MemNode;
use naatre_store::DataDir;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;

// ---- harness ---------------------------------------------------------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
}

fn build_app() -> TestApp {
    let dir = tempfile::tempdir().unwrap();
    let data = DataDir::create(dir.path()).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(dir.path()).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    let db_path = dir.path().join("naatre.db");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    TestApp {
        _dir: dir,
        app,
        db_path,
    }
}

impl TestApp {
    fn db(&self) -> Connection {
        let conn = Connection::open(&self.db_path).unwrap();
        conn.busy_timeout(Duration::from_secs(5)).unwrap();
        conn
    }

    /// Direct-DB seed: principal + token row, returning the token.
    fn seed_token(&self, principal: &str, role: &str, scope_path: Option<&str>) -> String {
        let (token, hash) = naatre_server::auth::mint_token();
        let conn = self.db();
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

    /// Direct-DB seed: repo, returning its id.
    fn seed_repo(&self, name: &str) -> i64 {
        let conn = self.db();
        conn.execute(
            "INSERT INTO repos(name, created_ns) VALUES (?1, ?2)",
            rusqlite::params![name, BASE_NS as i64],
        )
        .unwrap();
        conn.last_insert_rowid()
    }

    /// Direct-DB seed: a ref row (the route refuses `main`; M4's land will
    /// be its only writer, so tests seed the row the way land will leave it).
    fn seed_ref(&self, repo_id: i64, name: &str, target_hex: &str) {
        let target = Id::from_hex(target_hex).unwrap();
        let conn = self.db();
        conn.execute(
            "INSERT INTO refs(repo_id, name, target, version, last_principal) \
             VALUES (?1, ?2, ?3, 1, 'seed')",
            rusqlite::params![repo_id, name, target.0.as_slice()],
        )
        .unwrap();
    }

    fn count(&self, sql: &str) -> i64 {
        self.db()
            .query_row(sql, [], |r| r.get(0))
            .expect("count query")
    }
}

struct Resp {
    status: StatusCode,
    headers: HeaderMap,
    body: Value,
}

async fn send(
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
    let headers = res.headers().clone();
    let bytes = BodyExt::collect(res.into_body())
        .await
        .expect("body")
        .to_bytes()
        .to_vec();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Resp {
        status,
        headers,
        body,
    }
}

async fn get(app: &Router, uri: &str, token: &str) -> Resp {
    send(app, "GET", uri, Some(token), None).await
}

async fn post_json(app: &Router, uri: &str, token: &str, value: &Value) -> Resp {
    send(app, "POST", uri, Some(token), Some(value.clone())).await
}

fn code_of(r: &Resp) -> String {
    r.body["error"]["code"].as_str().unwrap_or("").to_string()
}

fn blob_hex(content: &[u8]) -> String {
    object_id(Kind::Blob, content).to_hex()
}

/// The shared fixture: app + admin/writer/reader tokens + repo "m2a".
struct Fixture {
    t: TestApp,
    admin: String,
    writer: String,
    reader: String,
}

fn fixture() -> Fixture {
    let t = build_app();
    let admin = t.seed_token("admin-p", "admin", None);
    let writer = t.seed_token("writer-p", "writer", None);
    let reader = t.seed_token("reader-p", "reader", None);
    t.seed_repo("m2a");
    Fixture {
        t,
        admin,
        writer,
        reader,
    }
}

/// Uploads one blob through the raw PUT, returning its hex id.
async fn put_blob(f: &Fixture, content: &[u8]) -> String {
    let id = blob_hex(content);
    let resp = send(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/m2a/objects/{id}"),
        Some(&f.writer),
        None,
    )
    .await;
    // `send` always sets a JSON content type; the raw PUT ignores it.
    let _ = resp;
    // Re-send with the raw body (send() takes a Value; use the builder here).
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/m2a/objects/{id}"))
        .header(header::AUTHORIZATION, format!("Bearer {}", f.writer))
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .body(Body::from(content.to_vec()))
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert!(
        res.status().is_success(),
        "blob PUT failed: {}",
        res.status()
    );
    id
}

/// Creates a changeset, asserting success, and returns (id, root_tree_id).
async fn create_ok(f: &Fixture, body: Value) -> (String, String, u64) {
    let resp = post_json(&f.t.app, "/v1/repos/m2a/changesets", &f.writer, &body).await;
    assert_eq!(resp.status, StatusCode::CREATED, "body: {}", resp.body);
    let id = resp.body["id"].as_str().unwrap().to_string();
    let root = resp.body["root_tree_id"].as_str().unwrap().to_string();
    let generation = resp.body["generation"].as_u64().unwrap();
    (id, root, generation)
}

fn add(path: &str, blob: &str, size: u64, exec: bool) -> Value {
    json!({ "path": path, "op": "add", "blob": blob, "size": size, "exec": exec })
}

fn cs_body(parents: Value, changes: Value) -> Value {
    json!({
        "parents": parents,
        "author": "Agent <agent@naatre.dev>",
        "message": "m",
        "changes": changes,
    })
}

// ---- 1. validation matrix ---------------------------------------------------

#[tokio::test]
async fn create_success_shape_location_and_audit() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;

    // Server fills timestamp from the clock when the client omits it.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!([add("a.txt", &b1, 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);
    let id = resp.body["id"].as_str().unwrap().to_string();
    assert_eq!(id.len(), 64);
    assert_eq!(resp.body["generation"], 0);
    assert_eq!(
        resp.headers[header::LOCATION],
        format!("/v1/repos/m2a/changesets/{id}")
    );
    let root1 = resp.body["root_tree_id"].as_str().unwrap().to_string();

    // Audited exactly once.
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM audit_log WHERE action = 'changeset_create'"),
        1
    );

    // Fetch echoes the record, with principal = the authenticated one and
    // the server-filled timestamp from the clock.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/changesets/{id}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["author"], "Agent <agent@naatre.dev>");
    assert_eq!(resp.body["principal"], "writer-p");
    assert_eq!(resp.body["parents"], json!([]));
    assert_eq!(
        resp.body["timestamp_ns"].as_u64().unwrap(),
        BASE_NS,
        "omitted timestamp_ns must come from the server clock"
    );
    assert_eq!(
        resp.body["changes"],
        json!([{ "path": "a.txt", "op": "add", "blob": b1, "size": 5, "exec": false }])
    );
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/changesets/{id}/root"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.body["root_tree_id"], root1.as_str());

    // Child changeset: generation 1, client timestamp kept verbatim.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &json!({
            "parents": [id],
            "author": "Agent <agent@naatre.dev>",
            "message": "m",
            "timestamp_ns": 42,
            "changes": [add("a.txt", &b1, 5, true)]
        }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);
    assert_eq!(resp.body["generation"], 1);
    let child = resp.body["id"].as_str().unwrap().to_string();
    // Client timestamp kept verbatim (checked on the fetch echo).
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/changesets/{child}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.body["timestamp_ns"], 42);
}

#[tokio::test]
async fn reject_non_nfc_path_without_rows() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    // "cafe" + combining acute accent (NFD) is not NFC.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!([add("cafe\u{0301}.txt", &b1, 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "path_not_normalized");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_control_char_in_path_and_format_char_in_author() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;

    // U+0007 (Cc) inside a path.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!([add("bell\u{0007}.txt", &b1, 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");

    // U+200B (Cf zero-width space) inside the author.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &json!({
            "parents": [],
            "author": "a\u{200B}b",
            "message": "m",
            "changes": [add("a.txt", &b1, 5, false)]
        }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_empty_changeset_without_rows() {
    let f = fixture();
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!([])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "empty_changeset");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_duplicate_paths_without_rows() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(
            json!([]),
            json!([add("a.txt", &b1, 5, false), add("a.txt", &b1, 5, false)]),
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "duplicate_path");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_unsorted_changes_without_rows() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    let b2 = put_blob(&f, b"beta").await;
    // Distinct paths, wrong order: rejected, never re-sorted server-side.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(
            json!([]),
            json!([add("b.txt", &b2, 4, false), add("a.txt", &b1, 5, false)]),
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "unsorted_changes");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_two_parents_without_rows() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    let (c1, _, _) = create_ok(&f, cs_body(json!([]), json!([add("a.txt", &b1, 5, false)]))).await;
    let (c2, _, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("b.txt", &b1, 5, false)])),
    )
    .await;
    let rows_before = f.t.count("SELECT COUNT(*) FROM changesets");
    // Public creates take at most one parent — imports build merges through
    // the db layer, not this route.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([c1, c2]), json!([add("c.txt", &b1, 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), rows_before);
}

#[tokio::test]
async fn reject_unknown_parent_and_list_missing() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    let ghost = object_id(Kind::Changeset, b"ghost").to_hex();
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([ghost]), json!([add("a.txt", &b1, 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "unknown_parent");
    assert_eq!(
        resp.body["error"]["details"]["missing_parents"],
        json!([ghost])
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_missing_blob_and_list_missing() {
    let f = fixture();
    let ghost_blob = object_id(Kind::Blob, b"never-uploaded").to_hex();
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!([add("a.txt", &ghost_blob, 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
    assert_eq!(
        resp.body["error"]["details"]["missing_blobs"],
        json!([ghost_blob])
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_duality_add_under_file_mod_dir_del_absent() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    let (c1, _, _) = create_ok(
        &f,
        cs_body(
            json!([]),
            // Sorted by path bytes: "d/x.txt" < "file.txt".
            json!([
                add("d/x.txt", &b1, 5, false),
                add("file.txt", &b1, 5, false)
            ]),
        ),
    )
    .await;
    let rows_before = f.t.count("SELECT COUNT(*) FROM changesets");

    // add under a file.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(
            json!([c1.clone()]),
            json!([add("file.txt/under", &b1, 5, false)]),
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "duality");

    // mod of a directory.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(
            json!([c1.clone()]),
            json!([json!({"path": "d", "op": "mod", "blob": b1, "size": 5, "exec": false})]),
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "duality");

    // del of an absent path.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([c1]), json!([{ "path": "nope.txt", "op": "del" }])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "duality");

    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), rows_before);
}

#[tokio::test]
async fn reject_case_fold_duplicate_pairs() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;

    for (a, b) in [
        ("README.md", "readme.md"),
        ("caf\u{00e9}.txt", "CAF\u{00c9}.TXT"),
    ] {
        let resp = post_json(
            &f.t.app,
            "/v1/repos/m2a/changesets",
            &f.writer,
            &cs_body(
                json!([]),
                json!([add(a, &b1, 5, false), add(b, &b1, 5, false)]),
            ),
        )
        .await;
        assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY, "{a}/{b}");
        assert_eq!(code_of(&resp), "validation", "{a}/{b}");
    }
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_over_entry_cap_without_rows() {
    let f = fixture();
    let blob = object_id(Kind::Blob, b"cap").to_hex();
    let entries: Vec<Value> = (0..100_001u32)
        .map(|i| add(&format!("f{i:06}.txt"), &blob, 1, false))
        .collect();
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!(entries)),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn reject_shape_broken_changes() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    // del carrying a blob.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(
            json!([]),
            json!([{ "path": "a.txt", "op": "del", "blob": b1 }]),
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
    // add without a blob.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(
            json!([]),
            json!([{ "path": "a.txt", "op": "add", "size": 5, "exec": false }]),
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
    // blob that is not hex.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!([add("a.txt", "nothex", 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);
    assert_eq!(code_of(&resp), "malformed");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);
}

#[tokio::test]
async fn scope_path_gates_writes() {
    let t = build_app();
    let scoped = t.seed_token("scoped-writer", "writer", Some("src"));
    t.seed_token("reader-p", "reader", None);
    t.seed_repo("m2a");
    let f = Fixture {
        admin: String::new(),
        writer: scoped,
        reader: String::new(),
        t,
    };

    let b1 = put_blob(&f, b"alpha").await;
    // Out of scope: 403 with the first offending path in details.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!([add("docs/x.txt", &b1, 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN);
    assert_eq!(code_of(&resp), "forbidden");
    assert_eq!(resp.body["error"]["details"]["path"], "docs/x.txt");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), 0);

    // In scope: the same writer succeeds.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/changesets",
        &f.writer,
        &cs_body(json!([]), json!([add("src/in.txt", &b1, 5, false)])),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);

    // A reader can commit nothing anywhere.
    let t2 = fixture();
    let resp = post_json(
        &t2.t.app,
        "/v1/repos/m2a/changesets",
        &t2.reader,
        &cs_body(
            json!([]),
            json!([add("a.txt", &blob_hex(b"alpha"), 5, false)]),
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN);
    assert_eq!(code_of(&resp), "forbidden");
}

// ---- 2. derivation oracle ---------------------------------------------------

#[tokio::test]
async fn derivation_oracle_chain_and_tree_reads() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    let b2 = put_blob(&f, b"alphabeta").await;
    let bd = put_blob(&f, b"deep").await;

    let (c1, r1, _) = create_ok(
        &f,
        cs_body(
            json!([]),
            json!([
                add("a/b.txt", &b1, 5, false),
                add("a/c/d.txt", &bd, 4, false)
            ]),
        ),
    )
    .await;
    let (c2, r2, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("a/b.txt", &b2, 9, false)])),
    )
    .await;
    let (c3, r3, _) = create_ok(
        &f,
        cs_body(json!([c2.clone()]), json!([add("a/b.txt", &b2, 9, true)])),
    )
    .await;
    assert_ne!(r2, r3, "exec flip must change root_tree_id");
    assert_ne!(r1, r2, "size change must change root_tree_id");
    let (c4, r4, _) = create_ok(
        &f,
        cs_body(
            json!([c3.clone()]),
            json!([{ "path": "a/c/d.txt", "op": "del" }]),
        ),
    )
    .await;
    let (c5, r5, _) = create_ok(
        &f,
        cs_body(
            json!([c4.clone()]),
            json!([add("a/c/d.txt", &bd, 4, false)]),
        ),
    )
    .await;

    // Tree reads at every level, with dir sizes as recursive totals.
    async fn tree_entries(f: &Fixture, root: &str) -> Value {
        let resp = get(&f.t.app, &format!("/v1/repos/m2a/trees/{root}"), &f.reader).await;
        assert_eq!(resp.status, StatusCode::OK);
        resp.body["entries"].clone()
    }

    // c1: a = dir(size 9) containing b.txt(5) and c(dir, size 4).
    let root1 = tree_entries(&f, &r1).await;
    assert_eq!(root1.as_array().unwrap().len(), 1);
    assert_eq!(root1[0]["name"], "a");
    assert_eq!(root1[0]["kind"], "dir");
    assert_eq!(root1[0]["size"], 9);
    let a1 = root1[0]["object_id"].as_str().unwrap().to_string();
    let entries = tree_entries(&f, &a1).await;
    assert_eq!(
        entries,
        json!([
            { "name": "b.txt", "kind": "file", "object_id": b1, "size": 5, "exec": false },
            { "name": "c", "kind": "dir", "object_id": entries[1]["object_id"], "size": 4, "exec": false },
        ])
    );

    // c2 recomputes sizes: a = 13.
    let root2 = tree_entries(&f, &r2).await;
    assert_eq!(root2[0]["size"], 13);

    // c4: c/ pruned away, a = 9 containing only b.txt.
    let root4 = tree_entries(&f, &r4).await;
    let a4 = root4[0]["object_id"].as_str().unwrap();
    let entries4 = tree_entries(&f, a4).await;
    assert_eq!(entries4.as_array().unwrap().len(), 1);
    assert_eq!(entries4[0]["name"], "b.txt");
    assert_eq!(entries4[0]["exec"], true, "c4 inherits c3's exec bit");
    assert_eq!(root4[0]["size"], 9);

    // Oracle: replaying every file change in order from an empty tree must
    // reproduce head's root_tree_id exactly.
    let mut node = MemNode::empty();
    for cs in [&c1, &c2, &c3, &c4, &c5] {
        let resp = get(
            &f.t.app,
            &format!("/v1/repos/m2a/changesets/{cs}"),
            &f.reader,
        )
        .await;
        let changes: Vec<naatre_core::changeset::FileChange> = resp.body["changes"]
            .as_array()
            .unwrap()
            .iter()
            .map(|c| naatre_core::changeset::FileChange {
                path: c["path"].as_str().unwrap().to_string(),
                op: match c["op"].as_str().unwrap() {
                    "add" => naatre_core::changeset::Op::Add,
                    "mod" => naatre_core::changeset::Op::Mod,
                    _ => naatre_core::changeset::Op::Del,
                },
                blob: c["blob"].as_str().map(|h| Id::from_hex(h).unwrap()),
                size: c["size"].as_u64(),
                exec: c["exec"].as_bool(),
            })
            .collect();
        node.apply(&changes).unwrap();
    }
    let (replayed_root, _, _) = node.collect().unwrap();
    assert_eq!(
        replayed_root.to_hex(),
        r5,
        "delta-chain replay must equal head"
    );
}

// ---- 3. reads ---------------------------------------------------------------

/// The 5-chain over distinct single-file changes, returning the ids oldest
/// to newest.
async fn log_chain(f: &Fixture) -> Vec<String> {
    let mut ids = Vec::new();
    let mut parent = json!([]);
    for i in 0..5u32 {
        let b = put_blob(f, format!("content-{i}").as_bytes()).await;
        let (id, _, _) = create_ok(
            f,
            cs_body(
                parent,
                json!([add(&format!("f{i}.txt"), &b, (i + 1) as u64, false)]),
            ),
        )
        .await;
        parent = json!([id.clone()]);
        ids.push(id);
    }
    ids
}

#[tokio::test]
async fn log_pagination_and_defaults() {
    let f = fixture();
    let chain = log_chain(&f).await;

    // Page 1 from the head.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/log?from={}&limit=2", chain[4]),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    let page: Vec<String> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap().to_string())
        .collect();
    assert_eq!(page, vec![chain[4].clone(), chain[3].clone()]);
    assert_eq!(resp.body["has_more"], true);
    let cursor1 = resp.body["next_cursor"].as_str().unwrap().to_string();
    assert_eq!(cursor1, format!("after:{}", chain[3]));

    // Page 2 via the cursor; the newest-first chain is the deterministic order.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/log?limit=2&cursor={cursor1}"),
        &f.reader,
    )
    .await;
    let page2: Vec<String> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap().to_string())
        .collect();
    assert_eq!(page2, vec![chain[2].clone(), chain[1].clone()]);
    let cursor2 = resp.body["next_cursor"].as_str().unwrap().to_string();

    // Page 3: last entry, exhausted.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/log?limit=2&cursor={cursor2}"),
        &f.reader,
    )
    .await;
    let page3: Vec<String> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap().to_string())
        .collect();
    assert_eq!(page3, vec![chain[0].clone()]);
    assert_eq!(resp.body["has_more"], false);
    assert_eq!(resp.body["next_cursor"], Value::Null);

    // Garbage cursors → 400.
    for cursor in ["junk", "before:x", &format!("after:{}", "zz")] {
        let resp = get(
            &f.t.app,
            &format!("/v1/repos/m2a/log?cursor={cursor}"),
            &f.reader,
        )
        .await;
        assert_eq!(resp.status, StatusCode::BAD_REQUEST, "{cursor}");
    }

    // Limit bounds: 422 out of range, 400 non-integer.
    let resp = get(&f.t.app, "/v1/repos/m2a/log?limit=201", &f.reader).await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    let resp = get(&f.t.app, "/v1/repos/m2a/log?limit=nope", &f.reader).await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);

    // Default `from` with no main ref (the repo has changesets but no
    // main): the unborn-ref edge is an empty page.
    let resp = get(&f.t.app, "/v1/repos/m2a/log", &f.reader).await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(
        resp.body,
        json!({ "entries": [], "next_cursor": null, "has_more": false })
    );

    // Seed main (what M4's land will move) → the default walk starts there.
    f.t.seed_ref(1, "main", &chain[4]);
    let resp = get(&f.t.app, "/v1/repos/m2a/log", &f.reader).await;
    let walked: Vec<String> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap().to_string())
        .collect();
    // The chain itself is the deterministic order: newest first.
    let expected: Vec<String> = chain.iter().rev().cloned().collect();
    assert_eq!(walked, expected);
}

#[tokio::test]
async fn log_unknown_from_is_404() {
    let f = fixture();
    let ghost = object_id(Kind::Changeset, b"ghost").to_hex();
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/log?from={ghost}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    assert_eq!(code_of(&resp), "not_found");
}

#[tokio::test]
async fn diff_between_roots() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    let b2 = put_blob(&f, b"alphabeta").await;
    let bd = put_blob(&f, b"deep").await;

    let (c1, _, _) = create_ok(
        &f,
        cs_body(
            json!([]),
            json!([
                add("a/b.txt", &b1, 5, false),
                add("a/c/d.txt", &bd, 4, false)
            ]),
        ),
    )
    .await;
    let (c4, _, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("a/b.txt", &b2, 9, true)])),
    )
    .await;
    let (c4, _, _) = {
        // c4 deletes d.txt on top.
        let (id, root, generation) = create_ok(
            &f,
            cs_body(json!([c4]), json!([{ "path": "a/c/d.txt", "op": "del" }])),
        )
        .await;
        (id, root, generation)
    };

    // c1..c4: b.txt modified (blob and exec), d.txt deleted; sorted by path.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/diff?a={c1}&b={c4}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(
        resp.body["changes"],
        json!([
            { "path": "a/b.txt", "kind": "mod", "a_blob": b1, "b_blob": b2 },
            { "path": "a/c/d.txt", "kind": "del", "a_blob": bd, "b_blob": null },
        ])
    );

    // A changeset against itself is an empty diff.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/diff?a={c1}&b={c1}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.body["changes"], json!([]));

    // Unknown side → 404.
    let ghost = object_id(Kind::Changeset, b"ghost").to_hex();
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/diff?a={c1}&b={ghost}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    // Non-hex → 400.
    let resp = get(&f.t.app, "/v1/repos/m2a/diff?a=nothex&b=nothex", &f.reader).await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn merge_base_diamond_and_disconnected() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;

    // A fork entirely inside one repo: x1 rootless, two children d1/d1b,
    // and a child of d1.
    let (x1, _, _) = create_ok(&f, cs_body(json!([]), json!([add("x.txt", &b1, 1, false)]))).await;
    let (d1, _, _) = create_ok(
        &f,
        cs_body(json!([x1.clone()]), json!([add("d1.txt", &b1, 1, false)])),
    )
    .await;
    let (d1b, _, _) = create_ok(
        &f,
        cs_body(json!([x1.clone()]), json!([add("d1b.txt", &b1, 1, false)])),
    )
    .await;
    let (d2, _, _) = create_ok(
        &f,
        cs_body(json!([d1.clone()]), json!([add("d2.txt", &b1, 1, false)])),
    )
    .await;

    async fn base(f: &Fixture, a: &str, b: &str) -> Value {
        get(
            &f.t.app,
            &format!("/v1/repos/m2a/merge-base?a={a}&b={b}"),
            &f.reader,
        )
        .await
        .body["merge_base"]
            .clone()
    }

    // Diamond: both fork children meet at x1; symmetric; ancestor cases.
    assert_eq!(base(&f, &d1, &d1b).await, json!(x1));
    assert_eq!(base(&f, &d1b, &d1).await, json!(x1));
    assert_eq!(base(&f, &d2, &d1b).await, json!(x1));
    assert_eq!(base(&f, &d2, &d1).await, json!(d1));
    assert_eq!(base(&f, &d2, &d2).await, json!(d2));

    // Disconnected histories within one repo: a second rootless root.
    let (y1, _, _) = create_ok(&f, cs_body(json!([]), json!([add("y.txt", &b1, 1, false)]))).await;
    assert_eq!(base(&f, &x1, &y1).await, Value::Null);
    assert_eq!(base(&f, &d2, &y1).await, Value::Null);

    // Unknown side → 404; non-hex → 400.
    let ghost = object_id(Kind::Changeset, b"ghost").to_hex();
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/merge-base?a={d2}&b={ghost}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    let resp = get(&f.t.app, "/v1/repos/m2a/merge-base?a=zz&b=zz", &f.reader).await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn file_lookup_deep_absent_and_crossing() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;
    let bd = put_blob(&f, b"deep").await;
    let (c1, _, _) = create_ok(
        &f,
        cs_body(
            json!([]),
            json!([
                add("a/b.txt", &b1, 5, true),
                add("a/c/d.txt", &bd, 4, false)
            ]),
        ),
    )
    .await;

    async fn lookup(f: &Fixture, at: &str, path: &str) -> Resp {
        get(
            &f.t.app,
            &format!(
                "/v1/repos/m2a/file?at={at}&path={}",
                path.replace('/', "%2F")
            ),
            &f.reader,
        )
        .await
    }

    let resp = lookup(&f, &c1, "a/b.txt").await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body, json!({ "blob_id": b1, "size": 5, "exec": true }));
    let resp = lookup(&f, &c1, "a/c/d.txt").await;
    assert_eq!(resp.body["blob_id"], bd.as_str());

    // Absent leaf, directory path, path crossing a file, absent `at`.
    let resp = lookup(&f, &c1, "a/missing.txt").await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    assert_eq!(code_of(&resp), "not_found");
    let resp = lookup(&f, &c1, "a").await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    let resp = lookup(&f, &c1, "a/b.txt/deeper").await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    let ghost = object_id(Kind::Changeset, b"ghost").to_hex();
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/m2a/file?at={ghost}&path=a/b.txt"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    let resp = get(
        &f.t.app,
        "/v1/repos/m2a/file?at=nothex&path=a/b.txt",
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);
    // Unknown repo → repo_not_found (the reads are repo-scoped).
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/nope/file?at={c1}&path=a"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    assert_eq!(code_of(&resp), "repo_not_found");
}

// ---- 4. refs non-fast-forward ------------------------------------------------

#[tokio::test]
async fn ref_moves_must_be_fast_forward() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha").await;

    let (x1, _, _) = create_ok(&f, cs_body(json!([]), json!([add("x.txt", &b1, 1, false)]))).await;
    let (d1, _, _) = create_ok(
        &f,
        cs_body(json!([x1.clone()]), json!([add("d1.txt", &b1, 1, false)])),
    )
    .await;
    let (d1b, _, _) = create_ok(
        &f,
        cs_body(json!([x1.clone()]), json!([add("d1b.txt", &b1, 1, false)])),
    )
    .await;
    let (d2, _, _) = create_ok(
        &f,
        cs_body(json!([d1.clone()]), json!([add("d2.txt", &b1, 1, false)])),
    )
    .await;

    async fn move_ref(f: &Fixture, target: &str, expected: Value) -> Resp {
        post_json(
            &f.t.app,
            "/v1/repos/m2a/refs/dev",
            &f.admin,
            &json!({ "target": target, "expected": expected }),
        )
        .await
    }

    // First move: the ref has no prior target → allowed (create at version 1).
    let resp = move_ref(&f, &d1, Value::Null).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(resp.body["version"], 1);

    // Non-descendant move → 422 not_fast_forward.
    let resp = move_ref(&f, &d1b, json!({ "version": 1 })).await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "not_fast_forward");
    // The ref did not move.
    let resp = get(&f.t.app, "/v1/repos/m2a/refs/dev", &f.reader).await;
    assert_eq!(resp.body["target"], d1.as_str());
    assert_eq!(resp.body["version"], 1);

    // Descendant move → 200, version bumps.
    let resp = move_ref(&f, &d2, json!({ "version": 1 })).await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["version"], 2);
    assert_eq!(resp.body["target"], d2.as_str());

    // A move onto the current target itself contains it → allowed (idempotent).
    let resp = move_ref(&f, &d2, json!({ "version": 2 })).await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["version"], 3);

    // `main` is still refused on this route (land-only, per the pinned
    // route matrix) — including its first move.
    let resp = post_json(
        &f.t.app,
        "/v1/repos/m2a/refs/main",
        &f.admin,
        &json!({ "target": x1, "expected": null }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
}
