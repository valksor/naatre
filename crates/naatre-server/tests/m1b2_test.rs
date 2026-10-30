//! M1b2 suite: the full authz matrix (status + error code per cell),
//! rate-limited auth-failure auditing, object round-trips and caps with
//! membership, ref CAS, writer_busy backpressure, token lifecycle (roles,
//! revocation, TTL via the injected clock), cursor pagination, and the
//! access-log policy. Driven in-memory via `tower::ServiceExt::oneshot`
//! with an injected fixed-step clock and direct-DB seeded tokens.

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{HeaderMap, StatusCode, header};
use base64::Engine as _;
use http_body_util::BodyExt;
use rusqlite::Connection;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Kind, object_id};
use naatre_server::auth::mint_token;
use naatre_server::state::{AppState, ServerState};
use naatre_store::{BLOB_LIMIT, DataDir};

/// Fixed-step clock base: ns for 2023-11-14, before any expiry.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const DAY_NS: u64 = 86_400_000_000_000;

// ---- harness ---------------------------------------------------------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    state: AppState,
    clock: Arc<AtomicU64>,
    db_path: PathBuf,
    admin_token: String,
}

/// Builds the full router over a fresh temp data dir with the injected
/// fixed-step clock. No tokens seeded — callers do that per test.
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
        state,
        clock,
        db_path,
        admin_token: String::new(),
    }
}

impl TestApp {
    fn db(&self) -> Connection {
        let conn = Connection::open(&self.db_path).unwrap();
        conn.busy_timeout(Duration::from_secs(5)).unwrap();
        conn
    }

    /// Direct-DB seed: principal + token row, returning (token id, token).
    fn seed_token(
        &self,
        principal: &str,
        role: &str,
        expires_ns: Option<i64>,
        revoked: bool,
    ) -> (i64, String) {
        let (token, hash) = mint_token();
        let conn = self.db();
        conn.execute(
            "INSERT INTO principals(name, role, created_ns) VALUES (?1, ?2, ?3) \
             ON CONFLICT(name) DO UPDATE SET role = excluded.role",
            rusqlite::params![principal, role, BASE_NS as i64],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO tokens(principal, blake3, expires_ns, revoked) VALUES (?1, ?2, ?3, ?4)",
            rusqlite::params![principal, hash, expires_ns, revoked as i64],
        )
        .unwrap();
        (conn.last_insert_rowid(), token)
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

    /// Direct-DB seed: a minimal changeset row (M2a's ref fast-forward gate
    /// resolves ref targets against this table), returning its hex id.
    fn seed_changeset(&self, repo_id: i64, tag: u8, generation: i64, parents: &[u8]) -> String {
        let mut id = [0u8; 32];
        id[0] = tag;
        let conn = self.db();
        conn.execute(
            "INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns, \
             generation, principal, root_tree_id) VALUES (?1, ?2, ?3, 'a', 'm', 1, ?4, 'seed', NULL)",
            rusqlite::params![repo_id, id, b"c", generation],
        )
        .unwrap();
        for (ord, parent) in parents.iter().enumerate() {
            let mut p = [0u8; 32];
            p[0] = *parent;
            conn.execute(
                "INSERT INTO changeset_parents(repo_id, cs, parent, ord) VALUES (?1, ?2, ?3, ?4)",
                rusqlite::params![repo_id, id, p, ord as i64],
            )
            .unwrap();
        }
        id.iter().map(|b| format!("{b:02x}")).collect()
    }

    fn admin(&self) -> &str {
        &self.admin_token
    }
}

struct Resp {
    status: StatusCode,
    headers: HeaderMap,
    body: Value,
    bytes: Vec<u8>,
}

async fn send(
    app: &Router,
    method: &str,
    uri: &str,
    token: Option<&str>,
    content_type: Option<&str>,
    body: Body,
) -> Resp {
    let mut builder = axum::http::Request::builder().method(method).uri(uri);
    if let Some(t) = token {
        builder = builder.header(header::AUTHORIZATION, format!("Bearer {t}"));
    }
    if let Some(ct) = content_type {
        builder = builder.header(header::CONTENT_TYPE, ct);
    }
    let req = builder.body(body).unwrap();
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
        bytes,
    }
}

async fn get(app: &Router, uri: &str, token: Option<&str>) -> Resp {
    send(app, "GET", uri, token, None, Body::empty()).await
}

async fn post_json(app: &Router, uri: &str, token: Option<&str>, value: &Value) -> Resp {
    send(
        app,
        "POST",
        uri,
        token,
        Some("application/json"),
        Body::from(value.to_string()),
    )
    .await
}

async fn put_bytes(app: &Router, uri: &str, token: Option<&str>, bytes: Vec<u8>) -> Resp {
    send(app, "PUT", uri, token, None, Body::from(bytes)).await
}

async fn delete(app: &Router, uri: &str, token: Option<&str>) -> Resp {
    send(app, "DELETE", uri, token, None, Body::empty()).await
}

fn code_of(r: &Resp) -> String {
    r.body["error"]["code"].as_str().unwrap_or("").to_string()
}

fn count_rows(t: &TestApp, sql: &str) -> i64 {
    t.db()
        .query_row(sql, [], |r| r.get(0))
        .expect("count query")
}

// ---- 1. authz matrix -------------------------------------------------------

/// Matrix fixtures shared by the sweep: object ids, batch payloads, victim
/// token id.
struct Matrix {
    x_hex: String, // stored + objects row + repo_blobs membership
    y_hex: String, // stored in the CAS only (no rows)
    w_hex: String, // fresh id for the PUT-raw cells
    z_hex: String, // fresh id for the batch-PUT cells
    z_b64: String,
    victim_id: i64,
}

async fn authz_matrix_sweep(t: &TestApp, m: &Matrix) {
    let (reader_id, reader_token) = t.seed_token("matrix-reader", "reader", None, false);
    let (_wid, writer_token) = t.seed_token("matrix-writer", "writer", None, false);
    let (_aid, admin_token) = t.seed_token("matrix-admin", "admin", None, false);
    let (_rid, revoked_token) = t.seed_token("matrix-revoked", "admin", None, true);
    let (_eid, expired_token) =
        t.seed_token("matrix-expired", "admin", Some(BASE_NS as i64 - 1), false);
    let _ = reader_id;
    let victim_id = m.victim_id;

    // (identity, token, expected cells) — order matters: writer PUTs before
    // admin re-PUTs (201 → 200), admin deletes the victim last.
    let identities: Vec<(&str, Option<&str>)> = vec![
        ("anon", None),
        ("reader", Some(&reader_token)),
        ("writer", Some(&writer_token)),
        ("admin", Some(&admin_token)),
        ("revoked-admin", Some(&revoked_token)),
        ("expired-admin", Some(&expired_token)),
        ("garbage", Some("ntk_garbagegarbagegarbagegarbagegarbage1")),
    ];

    for (identity, token) in identities {
        let expected: &[(&str, u16, &str)] = match identity {
            "anon" => &[
                (cell::REPOS_GET, 401, "token_invalid"),
                (cell::REPOS_POST, 401, "token_invalid"),
                (cell::REFS_GET, 401, "token_invalid"),
                (cell::REFS_POST, 401, "token_invalid"),
                (cell::PRESENT, 401, "token_invalid"),
                (cell::PUT_RAW, 401, "token_invalid"),
                (cell::BATCH_PUT, 401, "token_invalid"),
                (cell::GET_BATCH, 401, "token_invalid"),
                (cell::GET_RAW, 401, "token_invalid"),
                (cell::TOKENS_POST, 401, "token_invalid"),
                (cell::TOKENS_GET, 401, "token_invalid"),
                (cell::TOKENS_DELETE, 401, "token_invalid"),
                (cell::WHOAMI, 401, "token_invalid"),
                (cell::AUDIT, 401, "token_invalid"),
                (cell::OPENAPI, 401, "token_invalid"),
                (cell::STATS, 401, "token_invalid"),
            ],
            "reader" => &[
                (cell::REPOS_GET, 200, ""),
                (cell::REPOS_POST, 403, "forbidden"),
                (cell::REFS_GET, 200, ""),
                (cell::REFS_POST, 403, "forbidden"),
                (cell::PRESENT, 200, ""),
                (cell::PUT_RAW, 403, "forbidden"),
                (cell::BATCH_PUT, 403, "forbidden"),
                (cell::GET_BATCH, 200, ""),
                (cell::GET_RAW, 200, ""),
                (cell::TOKENS_POST, 403, "forbidden"),
                (cell::TOKENS_GET, 403, "forbidden"),
                (cell::TOKENS_DELETE, 403, "forbidden"),
                (cell::WHOAMI, 200, ""),
                (cell::AUDIT, 403, "forbidden"),
                (cell::OPENAPI, 200, ""),
                (cell::STATS, 403, "forbidden"),
            ],
            "writer" => &[
                (cell::REPOS_GET, 200, ""),
                (cell::REPOS_POST, 403, "forbidden"),
                (cell::REFS_GET, 200, ""),
                (cell::REFS_POST, 403, "forbidden"),
                (cell::PRESENT, 200, ""),
                (cell::PUT_RAW, 201, ""),
                (cell::BATCH_PUT, 200, ""),
                (cell::GET_BATCH, 200, ""),
                (cell::GET_RAW, 200, ""),
                (cell::TOKENS_POST, 403, "forbidden"),
                (cell::TOKENS_GET, 403, "forbidden"),
                (cell::TOKENS_DELETE, 403, "forbidden"),
                (cell::WHOAMI, 200, ""),
                (cell::AUDIT, 403, "forbidden"),
                (cell::OPENAPI, 200, ""),
                (cell::STATS, 403, "forbidden"),
            ],
            "admin" => &[
                (cell::REPOS_GET, 200, ""),
                (cell::REPOS_POST, 201, ""),
                (cell::REFS_GET, 200, ""),
                (cell::REFS_POST, 200, ""),
                (cell::PRESENT, 200, ""),
                (cell::PUT_RAW, 200, ""), // idempotent re-PUT of the writer's blob
                (cell::BATCH_PUT, 200, ""),
                (cell::GET_BATCH, 200, ""),
                (cell::GET_RAW, 200, ""),
                (cell::TOKENS_POST, 201, ""),
                (cell::TOKENS_GET, 200, ""),
                (cell::TOKENS_DELETE, 200, ""),
                (cell::WHOAMI, 200, ""),
                (cell::AUDIT, 200, ""),
                (cell::OPENAPI, 200, ""),
                (cell::STATS, 200, ""),
            ],
            "revoked-admin" => &[(cell::ALL, 401, "token_revoked")],
            "expired-admin" => &[(cell::ALL, 401, "token_expired")],
            "garbage" => &[(cell::ALL, 401, "token_invalid")],
            other => panic!("unknown identity {other}"),
        };

        for (route, status, code) in expected {
            let routes: &[&str] = if *route == cell::ALL {
                &[
                    cell::REPOS_GET,
                    cell::REPOS_POST,
                    cell::REFS_GET,
                    cell::REFS_POST,
                    cell::PRESENT,
                    cell::PUT_RAW,
                    cell::BATCH_PUT,
                    cell::GET_BATCH,
                    cell::GET_RAW,
                    cell::TOKENS_POST,
                    cell::TOKENS_GET,
                    cell::TOKENS_DELETE,
                    cell::WHOAMI,
                    cell::AUDIT,
                    cell::OPENAPI,
                    cell::STATS,
                ]
            } else {
                std::slice::from_ref(route)
            };
            for route in routes {
                let resp = fire_route(t, m, route, token, victim_id).await;
                assert_eq!(
                    resp.status.as_u16(),
                    *status,
                    "matrix cell {identity} x {route}: wrong status (body: {})",
                    String::from_utf8_lossy(&resp.bytes)
                );
                if !code.is_empty() {
                    assert_eq!(
                        code_of(&resp),
                        *code,
                        "matrix cell {identity} x {route}: wrong error code"
                    );
                }
            }
        }
    }
}

#[rustfmt::skip]
mod cell {
    pub const REPOS_GET: &str = "repos_get";
    pub const REPOS_POST: &str = "repos_post";
    pub const REFS_GET: &str = "refs_get";
    pub const REFS_POST: &str = "refs_post";
    pub const PRESENT: &str = "present";
    pub const PUT_RAW: &str = "put_raw";
    pub const BATCH_PUT: &str = "batch_put";
    pub const GET_BATCH: &str = "get_batch";
    pub const GET_RAW: &str = "get_raw";
    pub const TOKENS_POST: &str = "tokens_post";
    pub const TOKENS_GET: &str = "tokens_get";
    pub const TOKENS_DELETE: &str = "tokens_delete";
    pub const WHOAMI: &str = "whoami";
    pub const AUDIT: &str = "audit";
    pub const OPENAPI: &str = "openapi";
    pub const STATS: &str = "stats";
    pub const ALL: &str = "all";
}

async fn fire_route(
    t: &TestApp,
    m: &Matrix,
    route: &str,
    token: Option<&str>,
    victim_id: i64,
) -> Resp {
    match route {
        cell::REPOS_GET => get(&t.app, "/v1/repos", token).await,
        cell::REPOS_POST => {
            post_json(
                &t.app,
                "/v1/repos",
                token,
                &json!({ "name": "matrix-repo" }),
            )
            .await
        }
        cell::REFS_GET => get(&t.app, "/v1/repos/m1b/refs/dev", token).await,
        cell::REFS_POST => {
            post_json(
                &t.app,
                "/v1/repos/m1b/refs/matrix-ref",
                token,
                &json!({ "target": m.x_hex, "expected": null }),
            )
            .await
        }
        cell::PRESENT => {
            post_json(
                &t.app,
                "/v1/repos/m1b/objects/present",
                token,
                &json!({ "ids": [m.x_hex] }),
            )
            .await
        }
        cell::PUT_RAW => {
            put_bytes(
                &t.app,
                &format!("/v1/repos/m1b/objects/{}", m.w_hex),
                token,
                b"writer-put".to_vec(),
            )
            .await
        }
        cell::BATCH_PUT => {
            post_json(
                &t.app,
                "/v1/repos/m1b/objects",
                token,
                &json!({ "objects": [{ "id": m.z_hex, "data_b64": m.z_b64 }] }),
            )
            .await
        }
        cell::GET_BATCH => {
            post_json(
                &t.app,
                "/v1/repos/m1b/objects/get",
                token,
                &json!({ "ids": [m.x_hex] }),
            )
            .await
        }
        cell::GET_RAW => get(&t.app, &format!("/v1/repos/m1b/objects/{}", m.x_hex), token).await,
        cell::TOKENS_POST => {
            post_json(
                &t.app,
                "/v1/tokens",
                token,
                &json!({ "principal": "matrix-token-user", "role": "reader" }),
            )
            .await
        }
        cell::TOKENS_GET => get(&t.app, "/v1/tokens", token).await,
        cell::TOKENS_DELETE => delete(&t.app, &format!("/v1/tokens/{victim_id}"), token).await,
        cell::WHOAMI => get(&t.app, "/v1/whoami", token).await,
        cell::AUDIT => get(&t.app, "/v1/audit", token).await,
        cell::OPENAPI => get(&t.app, "/v1/openapi.json", token).await,
        cell::STATS => get(&t.app, "/v1/repos/m1b/stats", token).await,
        other => panic!("unknown route cell {other}"),
    }
}

/// TEST 1: AUTHZ MATRIX — every identity × route class asserts the exact
/// status + error code from the route table.
#[tokio::test]
async fn authz_matrix() {
    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };
    t.seed_repo("m1b");

    // Object fixtures.
    let x_payload = b"matrix-object-x";
    let x = object_id(Kind::Blob, x_payload);
    t.state.cas.put(x_payload).unwrap();
    let y_payload = b"matrix-object-y";
    let y = object_id(Kind::Blob, y_payload);
    t.state.cas.put(y_payload).unwrap();
    let w = object_id(Kind::Blob, b"writer-put");
    let z_payload = b"matrix-batch-z";
    let z = object_id(Kind::Blob, z_payload);

    {
        let conn = t.db();
        // X: objects row + repo membership; ref "dev" targets X.
        conn.execute(
            "INSERT INTO objects(object_id, size, csize) VALUES (?1, 1, 1)",
            rusqlite::params![x.0.as_slice()],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO repo_blobs(repo_id, object_id) VALUES (1, ?1)",
            rusqlite::params![x.0.as_slice()],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO refs(repo_id, name, target, version, last_principal) VALUES (1, 'dev', ?1, 1, 'seed')",
            rusqlite::params![x.0.as_slice()],
        )
        .unwrap();
    }
    let (victim_id, _) = t.seed_token("matrix-victim", "reader", None, false);

    let m = Matrix {
        x_hex: x.to_hex(),
        y_hex: y.to_hex(),
        w_hex: w.to_hex(),
        z_hex: z.to_hex(),
        z_b64: base64::engine::general_purpose::STANDARD.encode(z_payload),
        victim_id,
    };

    authz_matrix_sweep(&t, &m).await;

    // Spot-check the seeded membership actually served reads: the reader's
    // GET-raw cell returned the X bytes.
    let resp = get(
        &t.app,
        &format!("/v1/repos/m1b/objects/{}", m.x_hex),
        Some(t.admin()),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.bytes, b"matrix-object-x");

    // present consults the CAS only: Y (file exists, no membership) is
    // present but not fetchable; that isolation is asserted in the
    // membership test below.
    let resp = post_json(
        &t.app,
        "/v1/repos/m1b/objects/present",
        Some(t.admin()),
        &json!({ "ids": [m.x_hex, m.y_hex] }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["missing"], json!([]));
}

// ---- 3. auth-failure auditing ---------------------------------------------

/// TEST 3: AUDIT — within limit rows, beyond limit counts only.
#[tokio::test]
async fn auth_failure_auditing_is_rate_limited() {
    // Within limit: one bad token → one auth_failure row, principal "unknown".
    let t = build_app();
    let bad = "ntk_badbadbadbadbadbadbadbadbad";
    let resp = get(&t.app, "/v1/whoami", Some(bad)).await;
    assert_eq!(resp.status, StatusCode::UNAUTHORIZED);
    assert_eq!(code_of(&resp), "token_invalid");
    assert_eq!(t.state.auth_fail_stats(), (1, 1));
    assert_eq!(
        count_rows(
            &t,
            "SELECT COUNT(*) FROM audit_log WHERE action = 'auth_failure'"
        ),
        1
    );
    let (principal, details): (String, String) = t
        .db()
        .query_row(
            "SELECT principal, details_json FROM audit_log WHERE action = 'auth_failure'",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    assert_eq!(principal, "unknown");
    let details: Value = serde_json::from_str(&details).unwrap();
    assert!(details["key"].as_str().unwrap().starts_with("tk:"));

    // Beyond limit: 15 failures under one key → 10 rows, global count 15.
    let t = build_app();
    for _ in 0..15 {
        let resp = get(&t.app, "/v1/whoami", Some(bad)).await;
        assert_eq!(resp.status, StatusCode::UNAUTHORIZED);
    }
    assert_eq!(t.state.auth_fail_stats(), (15, 10));
    assert_eq!(
        count_rows(
            &t,
            "SELECT COUNT(*) FROM audit_log WHERE action = 'auth_failure'"
        ),
        10
    );

    // A header that parses as Bearer carries a token key; a bare garbage
    // header and a missing header land on the "anon" key.
    let t = build_app();
    let resp = send(&t.app, "GET", "/v1/whoami", None, None, Body::empty()).await;
    assert_eq!(resp.status, StatusCode::UNAUTHORIZED);
    let anon_details: String = t
        .db()
        .query_row(
            "SELECT details_json FROM audit_log WHERE action = 'auth_failure'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        serde_json::from_str::<Value>(&anon_details).unwrap()["key"],
        "anon"
    );
}

// ---- 4. objects ------------------------------------------------------------

/// TEST 4: OBJECTS — raw PUT roundtrip, mis-addressing, 64 MiB cap boundary,
/// idempotent re-PUT, membership-gated fetches.
#[tokio::test]
async fn object_raw_put_roundtrip_and_caps() {
    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };
    let repo_id = t.seed_repo("objrepo");

    let payload = b"roundtrip-payload";
    let id = object_id(Kind::Blob, payload);
    let hex = id.to_hex();

    // First PUT → 201 {id}, JSON content type.
    let resp = put_bytes(
        &t.app,
        &format!("/v1/repos/objrepo/objects/{hex}"),
        Some(t.admin()),
        payload.to_vec(),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);
    assert_eq!(resp.body["id"], json!(hex));
    assert_eq!(
        resp.headers
            .get(header::CONTENT_TYPE)
            .and_then(|v| v.to_str().ok()),
        Some("application/json")
    );

    // present: file exists → nothing missing (membership not consulted).
    let resp = post_json(
        &t.app,
        "/v1/repos/objrepo/objects/present",
        Some(t.admin()),
        &json!({ "ids": [hex] }),
    )
    .await;
    assert_eq!(resp.body["missing"], json!([]));

    // get-batch without membership → missing (cross-repo isolation gate).
    let resp = post_json(
        &t.app,
        "/v1/repos/objrepo/objects/get",
        Some(t.admin()),
        &json!({ "ids": [hex] }),
    )
    .await;
    assert_eq!(resp.body["objects"], json!([]));
    assert_eq!(resp.body["missing"], json!([hex]));

    // Seed membership (the documented reference-time flow until M2a).
    t.db()
        .execute(
            "INSERT INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)",
            rusqlite::params![repo_id, id.0.as_slice()],
        )
        .unwrap();

    // get-batch with membership → served, base64 decodes to the payload.
    let resp = post_json(
        &t.app,
        "/v1/repos/objrepo/objects/get",
        Some(t.admin()),
        &json!({ "ids": [hex] }),
    )
    .await;
    assert_eq!(resp.body["missing"], json!([]));
    let data_b64 = resp.body["objects"][0]["data_b64"].as_str().unwrap();
    assert_eq!(
        base64::engine::general_purpose::STANDARD
            .decode(data_b64)
            .unwrap(),
        payload
    );

    // get-raw → exact bytes, octet-stream + nosniff.
    let resp = get(
        &t.app,
        &format!("/v1/repos/objrepo/objects/{hex}"),
        Some(t.admin()),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.bytes, payload);
    assert_eq!(
        resp.headers.get(header::CONTENT_TYPE).unwrap(),
        "application/octet-stream"
    );
    assert_eq!(
        resp.headers.get("x-content-type-options").unwrap(),
        "nosniff"
    );

    // Mis-addressed PUT → 400 malformed.
    let wrong = object_id(Kind::Blob, b"not the payload");
    let resp = put_bytes(
        &t.app,
        &format!("/v1/repos/objrepo/objects/{}", wrong.to_hex()),
        Some(t.admin()),
        payload.to_vec(),
    )
    .await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);
    assert_eq!(code_of(&resp), "malformed");

    // +1 byte over the cap → 413 too_large.
    let oversized = vec![0u8; BLOB_LIMIT + 1];
    let oversized_id = object_id(Kind::Blob, &oversized).to_hex();
    let resp = put_bytes(
        &t.app,
        &format!("/v1/repos/objrepo/objects/{oversized_id}"),
        Some(t.admin()),
        oversized,
    )
    .await;
    assert_eq!(resp.status, StatusCode::PAYLOAD_TOO_LARGE);
    assert_eq!(code_of(&resp), "too_large");

    // Exactly at the cap → 201 (zeros: compressible, so the test stays fast).
    let exact = vec![0u8; BLOB_LIMIT];
    let exact_id = object_id(Kind::Blob, &exact).to_hex();
    let resp = put_bytes(
        &t.app,
        &format!("/v1/repos/objrepo/objects/{exact_id}"),
        Some(t.admin()),
        exact,
    )
    .await;
    assert_eq!(
        resp.status,
        StatusCode::CREATED,
        "exact-cap put must succeed"
    );

    // Idempotent re-PUT → 200.
    let resp = put_bytes(
        &t.app,
        &format!("/v1/repos/objrepo/objects/{hex}"),
        Some(t.admin()),
        payload.to_vec(),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
}

/// TEST 4: OBJECTS — batch PUT stores verified entries and reports rejected ones
/// per-entry (including a just-over-cap entry).
#[tokio::test]
async fn object_batch_put_reports_rejected_entries() {
    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };
    t.seed_repo("batchrepo");

    let good = b"batch-good";
    let good_id = object_id(Kind::Blob, good);
    let hash_mismatch = object_id(Kind::Blob, b"different content").to_hex();
    let oversized = vec![0u8; BLOB_LIMIT + 1];
    let oversized_id = object_id(Kind::Blob, &oversized);

    let body = json!({
        "objects": [
            { "id": good_id.to_hex(), "data_b64": base64::engine::general_purpose::STANDARD.encode(good) },
            { "id": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "data_b64": "not base64!!" },
            { "id": hash_mismatch, "data_b64": base64::engine::general_purpose::STANDARD.encode(good) },
            { "id": oversized_id.to_hex(), "data_b64": base64::engine::general_purpose::STANDARD.encode(&oversized) },
        ]
    });
    let resp = post_json(
        &t.app,
        "/v1/repos/batchrepo/objects",
        Some(t.admin()),
        &body,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["stored"], json!([good_id.to_hex()]));
    let rejected = resp.body["rejected"].as_array().unwrap();
    assert_eq!(rejected.len(), 3);
    assert_eq!(rejected[0]["code"], "malformed"); // bad base64
    assert_eq!(rejected[1]["code"], "malformed"); // hash mismatch
    assert_eq!(rejected[2]["code"], "too_large"); // over the 64 MiB cap

    // Only the good entry reached the CAS.
    let resp = post_json(
        &t.app,
        "/v1/repos/batchrepo/objects/present",
        Some(t.admin()),
        &json!({ "ids": [good_id.to_hex(), oversized_id.to_hex()] }),
    )
    .await;
    assert_eq!(resp.body["missing"], json!([oversized_id.to_hex()]));

    // Batch cap: 257 entries → 422.
    let entries: Vec<Value> = (0..257)
        .map(|_| json!({ "id": good_id.to_hex(), "data_b64": "" }))
        .collect();
    let resp = post_json(
        &t.app,
        "/v1/repos/batchrepo/objects",
        Some(t.admin()),
        &json!({ "objects": entries }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
}

// ---- 5. refs ---------------------------------------------------------------

/// TEST 5: REFS — create/get, stale-version CAS, ref_conflict details, `main`
/// guard, malformed target.
#[tokio::test]
async fn refs_cas_lifecycle() {
    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };
    t.seed_repo("rf");

    // M2a's fast-forward gate resolves ref targets as changesets: seed a
    // two-node chain and move along it (child contains parent).
    let t1 = t.seed_changeset(1, 0xA1, 0, &[]);
    let t2 = t.seed_changeset(1, 0xA2, 1, &[0xA1]);

    // Create (expected null) → 200 version 1.
    let resp = post_json(
        &t.app,
        "/v1/repos/rf/refs/newref",
        Some(t.admin()),
        &json!({ "target": t1, "expected": null }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(
        resp.body,
        json!({ "name": "newref", "target": t1, "version": 1 })
    );

    // GET → 200 with the target.
    let resp = get(&t.app, "/v1/repos/rf/refs/newref", Some(t.admin())).await;
    assert_eq!(
        resp.body,
        json!({ "name": "newref", "target": t1, "version": 1 })
    );

    // Create over an existing ref → 409 with current version/target.
    let resp = post_json(
        &t.app,
        "/v1/repos/rf/refs/newref",
        Some(t.admin()),
        &json!({ "target": t2, "expected": null }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CONFLICT);
    assert_eq!(code_of(&resp), "ref_conflict");
    assert_eq!(resp.body["error"]["details"]["current_version"], 1);
    assert_eq!(resp.body["error"]["details"]["current_target"], t1);

    // Stale expected version → 409 with the current version in details.
    let resp = post_json(
        &t.app,
        "/v1/repos/rf/refs/newref",
        Some(t.admin()),
        &json!({ "target": t2, "expected": { "version": 5 } }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CONFLICT);
    assert_eq!(code_of(&resp), "ref_conflict");
    assert_eq!(resp.body["error"]["details"]["current_version"], 1);

    // Fresh expected version → move to version 2.
    let resp = post_json(
        &t.app,
        "/v1/repos/rf/refs/newref",
        Some(t.admin()),
        &json!({ "target": t2, "expected": { "version": 1 } }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["version"], 2);
    assert_eq!(resp.body["target"], t2);

    // Both successful moves are audited as ref_move.
    assert_eq!(
        count_rows(
            &t,
            "SELECT COUNT(*) FROM audit_log WHERE action = 'ref_move'"
        ),
        2
    );

    // main moves only via land.
    let resp = post_json(
        &t.app,
        "/v1/repos/rf/refs/main",
        Some(t.admin()),
        &json!({ "target": t1, "expected": null }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");

    // Target must be 64-hex.
    let resp = post_json(
        &t.app,
        "/v1/repos/rf/refs/bad",
        Some(t.admin()),
        &json!({ "target": "not-hex", "expected": null }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);
    assert_eq!(code_of(&resp), "malformed");

    // Unknown ref on GET → 404 not_found; unknown repo → 404 repo_not_found;
    // CAS expected on an absent ref → 404 not_found.
    let resp = get(&t.app, "/v1/repos/rf/refs/nope", Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    assert_eq!(code_of(&resp), "not_found");
    let resp = get(&t.app, "/v1/repos/nope/refs/x", Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    assert_eq!(code_of(&resp), "repo_not_found");
    let resp = post_json(
        &t.app,
        "/v1/repos/rf/refs/ghost",
        Some(t.admin()),
        &json!({ "target": t1, "expected": { "version": 9 } }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
    assert_eq!(code_of(&resp), "not_found");
}

// ---- 6. writer_busy --------------------------------------------------------

/// TEST 6: WRITER_BUSY — refs POST while a write txn is held → 503 writer_busy
/// with Retry-After: 1 and retryable: true.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn refs_post_while_write_held_is_503_writer_busy() {
    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };
    t.seed_repo("wb");
    let target = object_id(Kind::Blob, b"wb-target").to_hex();

    // The request must be in flight while the single-writer slot is held,
    // so its handler write is what contends. The holder takes the mutex in
    // short back-to-back stints, leaving brief gaps for the auth read to
    // pass; the write issued microseconds later lands on a held mutex and
    // is answered 503. Whether the read or the hold wins the mutex first is
    // a coin flip, so retry with a fresh ref name (bounded, like the
    // drain-cap test).
    let mut contended = false;
    for attempt in 0..12u32 {
        let app = t.app.clone();
        let admin = t.admin().to_string();
        let body = json!({ "target": target, "expected": null });
        let post = tokio::spawn(async move {
            post_json(
                &app,
                &format!("/v1/repos/wb/refs/contend-{attempt}"),
                Some(&admin),
                &body,
            )
            .await
        });
        let holder = {
            let state = Arc::clone(&t.state);
            tokio::spawn(async move {
                for _ in 0..3 {
                    state.hold_write_txn(25).await;
                }
            })
        };
        let resp = post.await.expect("refs POST task");
        holder.await.expect("holder task");

        if resp.status == StatusCode::SERVICE_UNAVAILABLE {
            assert_eq!(code_of(&resp), "writer_busy");
            assert_eq!(resp.body["error"]["retryable"], true);
            assert_eq!(resp.headers.get(header::RETRY_AFTER).unwrap(), "1");
            contended = true;
            break;
        }
        // Definitive outcome (created or conflicted): the write beat the
        // hold — try again.
    }
    assert!(
        contended,
        "refs POST never contended with the hold in 12 attempts"
    );

    // With the slot free, a fresh move succeeds normally.
    let resp = post_json(
        &t.app,
        "/v1/repos/wb/refs/after-hold",
        Some(t.admin()),
        &json!({ "target": target, "expected": null }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
}

// ---- 7. tokens -------------------------------------------------------------

/// TEST 7: TOKENS — a writer token passes object PUTs and fails repo creation;
/// revocation and TTL expiry are enforced with distinct codes; validation
/// rejections fire on bad role/scope.
#[tokio::test]
async fn token_lifecycle_roles_ttl() {
    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };
    t.seed_repo("tlrepo");

    // Mint a writer token.
    let resp = post_json(
        &t.app,
        "/v1/tokens",
        Some(t.admin()),
        &json!({ "principal": "writer-2", "role": "writer" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);
    assert!(resp.body["token"].as_str().unwrap().starts_with("ntk_"));
    let writer_token = resp.body["token"].as_str().unwrap().to_string();
    let writer_id = resp.body["id"].as_i64().unwrap();
    assert_eq!(resp.body["expires_ns"], Value::Null);
    assert_eq!(resp.body["role"], "writer");

    // The listed shape excludes the token material.
    let resp = get(&t.app, "/v1/tokens", Some(t.admin())).await;
    let listed = resp.body["tokens"].as_array().unwrap();
    assert!(listed.iter().any(|t| t["id"].as_i64() == Some(writer_id)));
    assert!(listed[0].get("token").is_none());

    // Writer can PUT objects…
    let payload = b"writer-allowed";
    let id = object_id(Kind::Blob, payload).to_hex();
    let resp = put_bytes(
        &t.app,
        &format!("/v1/repos/tlrepo/objects/{id}"),
        Some(&writer_token),
        payload.to_vec(),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);

    // …but not create repos.
    let resp = post_json(
        &t.app,
        "/v1/repos",
        Some(&writer_token),
        &json!({ "name": "nope" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN);
    assert_eq!(code_of(&resp), "forbidden");

    // Revoke → next use 401 token_revoked.
    let resp = delete(&t.app, &format!("/v1/tokens/{writer_id}"), Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body, json!({ "revoked": true }));
    let resp = put_bytes(
        &t.app,
        &format!("/v1/repos/tlrepo/objects/{id}"),
        Some(&writer_token),
        payload.to_vec(),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNAUTHORIZED);
    assert_eq!(code_of(&resp), "token_revoked");

    // Idempotent re-revoke → still 200.
    let resp = delete(&t.app, &format!("/v1/tokens/{writer_id}"), Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::OK);

    // TTL: expires_ns = now + 1 day, enforced through the injected clock.
    let resp = post_json(
        &t.app,
        "/v1/tokens",
        Some(t.admin()),
        &json!({ "principal": "shortlived", "role": "reader", "ttl_days": 1 }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);
    assert_eq!(resp.body["expires_ns"], json!(BASE_NS + DAY_NS));
    let ttl_token = resp.body["token"].as_str().unwrap().to_string();
    let resp = get(&t.app, "/v1/whoami", Some(&ttl_token)).await;
    assert_eq!(resp.status, StatusCode::OK);

    t.clock.store(BASE_NS + 2 * DAY_NS, Ordering::Relaxed);
    let resp = get(&t.app, "/v1/whoami", Some(&ttl_token)).await;
    assert_eq!(resp.status, StatusCode::UNAUTHORIZED);
    assert_eq!(code_of(&resp), "token_expired");

    // Validation: unknown role, trailing-slash scope, negative ttl.
    let resp = post_json(
        &t.app,
        "/v1/tokens",
        Some(t.admin()),
        &json!({ "principal": "x", "role": "wizard" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    let resp = post_json(
        &t.app,
        "/v1/tokens",
        Some(t.admin()),
        &json!({ "principal": "x", "role": "writer", "scope_path": "a/" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    let resp = post_json(
        &t.app,
        "/v1/tokens",
        Some(t.admin()),
        &json!({ "principal": "x", "role": "writer", "ttl_days": -1 }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);

    // A valid scope round-trips through whoami.
    let resp = post_json(
        &t.app,
        "/v1/tokens",
        Some(t.admin()),
        &json!({ "principal": "scoped", "role": "reader", "scope_path": "a/b" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);
    let scoped = resp.body["token"].as_str().unwrap().to_string();
    let resp = get(&t.app, "/v1/whoami", Some(&scoped)).await;
    assert_eq!(
        resp.body,
        json!({ "principal": "scoped", "role": "reader", "scope_path": "a/b" })
    );
}

// ---- 8. pagination ---------------------------------------------------------

/// TEST 8: PAGINATION — repos cursor pages ascend and resolve; limits and cursors
/// validate; audit pages descend.
#[tokio::test]
async fn repo_pagination() {
    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };
    for name in ["alpha", "beta", "gamma"] {
        let resp = post_json(
            &t.app,
            "/v1/repos",
            Some(t.admin()),
            &json!({ "name": name }),
        )
        .await;
        assert_eq!(resp.status, StatusCode::CREATED);
    }

    // First page of 2.
    let resp = get(&t.app, "/v1/repos?limit=2", Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::OK);
    let names: Vec<&str> = resp.body["repos"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, ["alpha", "beta"]);
    assert_eq!(resp.body["has_more"], true);
    let cursor = resp.body["next_cursor"].as_str().unwrap();
    assert!(
        cursor.starts_with("after:"),
        "cursor format must be after:<id>: {cursor}"
    );

    // The cursor resolves to the third repo.
    let resp = get(
        &t.app,
        &format!("/v1/repos?limit=2&cursor={cursor}"),
        Some(t.admin()),
    )
    .await;
    let names: Vec<&str> = resp.body["repos"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, ["gamma"]);
    assert_eq!(resp.body["has_more"], false);
    assert_eq!(resp.body["next_cursor"], Value::Null);

    // Invalid inputs: 201 over-cap → 422, non-integer → 400, garbage cursor
    // → 400, zero limit → 422.
    let resp = get(&t.app, "/v1/repos?limit=201", Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
    let resp = get(&t.app, "/v1/repos?limit=abc", Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);
    assert_eq!(code_of(&resp), "malformed");
    let resp = get(&t.app, "/v1/repos?cursor=bogus", Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::BAD_REQUEST);
    assert_eq!(code_of(&resp), "malformed");
    let resp = get(&t.app, "/v1/repos?limit=0", Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);

    // Repo creation left 3 audited rows; audit pages descend.
    let resp = get(&t.app, "/v1/audit?limit=2", Some(t.admin())).await;
    assert_eq!(resp.status, StatusCode::OK);
    let page1 = resp.body["audit"].as_array().unwrap();
    assert_eq!(page1.len(), 2);
    assert!(
        page1[0]["id"].as_i64() > page1[1]["id"].as_i64(),
        "descending"
    );
    assert_eq!(page1[0]["action"], "repo_create");
    let cursor = resp.body["next_cursor"].as_str().unwrap();
    assert!(cursor.starts_with("before:"));
    let resp = get(
        &t.app,
        &format!("/v1/audit?limit=2&cursor={cursor}"),
        Some(t.admin()),
    )
    .await;
    let page2 = resp.body["audit"].as_array().unwrap();
    assert_eq!(page2.len(), 1);
    assert_eq!(resp.body["has_more"], false);
}

// ---- 9. access-log policy --------------------------------------------------

/// TEST 9: ACCESS-LOG POLICY — at info the request is silent; at debug exactly
/// one line carries method/path/status/request_id and no peer address.
#[test]
fn access_log_policy() {
    use tracing::dispatcher::{Dispatch, with_default};
    use tracing_subscriber::filter::LevelFilter;
    use tracing_subscriber::fmt;

    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();

    let capture = Capture::default();

    // Default (info): no output.
    let info_dispatch = Dispatch::new(
        fmt()
            .with_ansi(false)
            .with_max_level(LevelFilter::INFO)
            .with_writer(capture.clone())
            .finish(),
    );
    with_default(&info_dispatch, || {
        rt.block_on(async {
            let resp = get(&t.app, "/v1/whoami", Some(t.admin())).await;
            assert_eq!(resp.status, StatusCode::OK);
        });
    });
    assert!(
        capture.data().is_empty(),
        "request log must be silent at info; got: {}",
        String::from_utf8_lossy(&capture.data())
    );

    // Debug: exactly one line, no peer address field.
    capture.clear();
    let debug_dispatch = Dispatch::new(
        fmt()
            .with_ansi(false)
            .with_max_level(LevelFilter::DEBUG)
            .with_writer(capture.clone())
            .finish(),
    );
    with_default(&debug_dispatch, || {
        rt.block_on(async {
            let resp = get(&t.app, "/v1/whoami", Some(t.admin())).await;
            assert_eq!(resp.status, StatusCode::OK);
        });
    });
    let out = String::from_utf8(capture.data()).expect("log is UTF-8");
    let lines: Vec<&str> = out.lines().filter(|l| !l.trim().is_empty()).collect();
    assert_eq!(
        lines.len(),
        1,
        "exactly one request line at debug; got:\n{out}"
    );
    let line = lines[0];
    assert!(line.contains("GET"), "method missing: {line}");
    assert!(line.contains("/v1/whoami"), "path missing: {line}");
    assert!(line.contains("status=200"), "status missing: {line}");
    assert!(line.contains("request_id="), "request_id missing: {line}");
    assert!(
        !line.contains("peer") && !line.contains("remote_addr") && !line.contains("addr="),
        "peer address must never appear: {line}"
    );
}

/// A `MakeWriter` capturing subscriber output for assertions.
#[derive(Clone, Default)]
struct Capture(Arc<std::sync::Mutex<Vec<u8>>>);

impl Capture {
    fn data(&self) -> Vec<u8> {
        self.0.lock().unwrap().clone()
    }

    fn clear(&self) {
        self.0.lock().unwrap().clear();
    }
}

impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for Capture {
    type Writer = CaptureWriter;

    fn make_writer(&'a self) -> Self::Writer {
        CaptureWriter(Arc::clone(&self.0))
    }
}

struct CaptureWriter(Arc<std::sync::Mutex<Vec<u8>>>);

impl std::io::Write for CaptureWriter {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(buf);
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

// ---- repo create validation ------------------------------------------------

/// Repo name grammar and duplicate handling (repo_exists is the new code).
#[tokio::test]
async fn repo_create_validation_and_duplicates() {
    let t = build_app();
    let (_, admin) = t.seed_token("seed-admin", "admin", None, false);
    let t = TestApp {
        admin_token: admin,
        ..t
    };

    let resp = post_json(
        &t.app,
        "/v1/repos",
        Some(t.admin()),
        &json!({ "name": "demo" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);
    assert_eq!(resp.body, json!({ "id": 1, "name": "demo" }));
    assert_eq!(
        resp.headers.get(header::LOCATION).unwrap(),
        "/v1/repos/demo"
    );

    // Duplicate → 409 repo_exists.
    let resp = post_json(
        &t.app,
        "/v1/repos",
        Some(t.admin()),
        &json!({ "name": "demo" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CONFLICT);
    assert_eq!(code_of(&resp), "repo_exists");

    // Bad names → 422: uppercase, leading dash, empty, 64 chars.
    for name in ["Demo", "-demo", "", &"a".repeat(64)] {
        let resp = post_json(
            &t.app,
            "/v1/repos",
            Some(t.admin()),
            &json!({ "name": name }),
        )
        .await;
        assert_eq!(
            resp.status,
            StatusCode::UNPROCESSABLE_ENTITY,
            "name {name:?}"
        );
        assert_eq!(code_of(&resp), "validation");
    }
    // 63 chars is the max → 201.
    let resp = post_json(
        &t.app,
        "/v1/repos",
        Some(t.admin()),
        &json!({ "name": "a".repeat(63) }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED);
}
