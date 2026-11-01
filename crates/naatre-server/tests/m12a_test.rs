//! M12a suite: first-class conflicts, server side — the default land keeps
//! the M4 409 + recipe (now naming `--record-conflict`); the explicit
//! `record_conflict` opt-in settles a conflicted (format-v2) node ONCE on
//! main (`lands.state = 'conflicted'`, `root_tree_id` = the
//! markers-materialized tree); a conflicted node rebases like any node and
//! its representations travel; unrepresentable conflicts (a deletion side)
//! refuse even under the opt-in.

use std::path::PathBuf;
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

use naatre_core::changeset;
use naatre_core::conflict::marker_bytes;
use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::ServerState;
use naatre_store::DataDir;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m12a";

// ---- harness (the m4a pattern) ---------------------------------------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
}

fn build_app() -> TestApp {
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

    fn seed_token(&self, principal: &str, role: &str) -> String {
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
            rusqlite::params![principal, hash, Option::<i64>::None, Option::<String>::None],
        )
        .unwrap();
        token
    }

    fn seed_repo(&self, name: &str) -> i64 {
        let conn = self.db();
        conn.execute(
            "INSERT INTO repos(name, created_ns) VALUES (?1, ?2)",
            rusqlite::params![name, BASE_NS as i64],
        )
        .unwrap();
        conn.last_insert_rowid()
    }

    /// Seeds the `main` ref the way land leaves it (version 1).
    fn seed_main(&self, repo_id: i64, target_hex: &str) {
        let target = Id::from_hex(target_hex).unwrap();
        let conn = self.db();
        conn.execute(
            "INSERT INTO refs(repo_id, name, target, version, last_principal) \
             VALUES (?1, 'main', ?2, 1, 'seed') \
             ON CONFLICT(repo_id, name) DO UPDATE SET target = excluded.target",
            rusqlite::params![repo_id, target.0.as_slice()],
        )
        .unwrap();
    }

    /// Moves main without a version bump (test scaffolding).
    fn move_main(&self, repo_id: i64, target_hex: &str) {
        let target = Id::from_hex(target_hex).unwrap();
        let conn = self.db();
        conn.execute(
            "UPDATE refs SET target = ?2 WHERE repo_id = ?1 AND name = 'main'",
            rusqlite::params![repo_id, target.0.as_slice()],
        )
        .unwrap();
    }

    fn count(&self, sql: &str) -> i64 {
        self.db().query_row(sql, [], |r| r.get(0)).unwrap()
    }

    fn canonical_of(&self, repo_id: i64, id_hex: &str) -> Vec<u8> {
        let id = Id::from_hex(id_hex).unwrap();
        self.db()
            .query_row(
                "SELECT canonical FROM changesets WHERE repo_id = ?1 AND id = ?2",
                rusqlite::params![repo_id, id.0.as_slice()],
                |r| r.get(0),
            )
            .unwrap()
    }

    fn root_tree_of(&self, repo_id: i64, id_hex: &str) -> Id {
        let id = Id::from_hex(id_hex).unwrap();
        let bytes: Vec<u8> = self
            .db()
            .query_row(
                "SELECT root_tree_id FROM changesets WHERE repo_id = ?1 AND id = ?2",
                rusqlite::params![repo_id, id.0.as_slice()],
                |r| r.get(0),
            )
            .unwrap();
        Id(bytes.try_into().expect("root tree id is 32 bytes"))
    }

    fn land_state(&self, key: &str) -> String {
        self.db()
            .query_row(
                "SELECT state FROM lands WHERE key = ?1",
                rusqlite::params![key],
                |r| r.get(0),
            )
            .unwrap()
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

/// The shared fixture: app + writer/reader tokens + repo "m12a".
struct Fixture {
    t: TestApp,
    writer: String,
    reader: String,
    repo_id: i64,
}

fn fixture() -> Fixture {
    let t = build_app();
    let writer = t.seed_token("writer-p", "writer");
    let reader = t.seed_token("reader-p", "reader");
    let repo_id = t.seed_repo(REPO);
    Fixture {
        t,
        writer,
        reader,
        repo_id,
    }
}

async fn put_blob(f: &Fixture, content: &[u8]) -> String {
    let id = blob_hex(content);
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/{REPO}/objects/{id}"))
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

async fn create_ok(f: &Fixture, body: Value) -> String {
    let resp = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer,
        &body,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "body: {}", resp.body);
    resp.body["id"].as_str().unwrap().to_string()
}

fn add(path: &str, blob: &str, size: usize) -> Value {
    json!({ "path": path, "op": "add", "blob": blob, "size": size, "exec": false })
}

fn mod_(path: &str, blob: &str, size: usize) -> Value {
    json!({ "path": path, "op": "mod", "blob": blob, "size": size, "exec": false })
}

fn del(path: &str) -> Value {
    json!({ "path": path, "op": "del" })
}

fn cs_body(parents: Value, changes: Value, message: &str) -> Value {
    json!({
        "parents": parents,
        "author": "Agent <agent@naatre.dev>",
        "message": message,
        "changes": changes,
    })
}

async fn land(f: &Fixture, head: &str, key: &str) -> Resp {
    land_opt(f, head, key, false).await
}

async fn land_opt(f: &Fixture, head: &str, key: &str, record_conflict: bool) -> Resp {
    post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/land"),
        &f.writer,
        &json!({ "head": head, "key": key, "record_conflict": record_conflict }),
    )
    .await
}

async fn main_target(f: &Fixture) -> String {
    let resp = get(&f.t.app, &format!("/v1/repos/{REPO}/refs/main"), &f.reader).await;
    resp.body["target"].as_str().unwrap().to_string()
}

/// Raw blob bytes at `at`/`path` through /file + /objects.
async fn cat(f: &Fixture, at: &str, path: &str) -> Vec<u8> {
    let resp = get(
        &f.t.app,
        &format!(
            "/v1/repos/{REPO}/file?at={at}&path={}",
            path.replace('/', "%2F")
        ),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "cat {path}: {}", resp.body);
    let blob = resp.body["blob_id"].as_str().unwrap().to_string();
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(format!("/v1/repos/{REPO}/objects/{blob}"))
        .header(header::AUTHORIZATION, format!("Bearer {}", f.reader))
        .body(Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert!(res.status().is_success());
    BodyExt::collect(res.into_body())
        .await
        .unwrap()
        .to_bytes()
        .to_vec()
}

/// The single conflicted main-line node: base c1 on main, competing edits
/// c2 (ours) and c2p (theirs) on top of c1; main moves to c2p. `extra`
/// carries additional THEIRS-side changes (sorted into the request).
async fn diverged(f: &Fixture, mut extra: Vec<Value>) -> (String, String, String) {
    let base = b"l1\nl2\nl3\n".to_vec();
    let ours = b"L1-ours\nl2\nl3\n".to_vec();
    let theirs = b"X1-main\nl2\nl3\n".to_vec();
    let (bb, bo, bt) = (
        put_blob(f, &base).await,
        put_blob(f, &ours).await,
        put_blob(f, &theirs).await,
    );
    let c1 = create_ok(
        f,
        cs_body(json!([]), json!([add("same.txt", &bb, base.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    let c2 = create_ok(
        f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("same.txt", &bo, ours.len())]),
            "c2",
        ),
    )
    .await;
    extra.push(mod_("same.txt", &bt, theirs.len()));
    extra.sort_by_key(|c| c["path"].as_str().unwrap_or("").as_bytes().to_vec());
    let c2p = create_ok(f, cs_body(json!([c1.clone()]), json!(extra), "c2-prime")).await;
    f.t.move_main(f.repo_id, &c2p);
    (c1, c2, c2p)
}

/// A fetch closure over the three known contents (base/ours/theirs).
fn fetch_known(
    base: &'static [u8],
    ours: &'static [u8],
    theirs: &'static [u8],
) -> impl FnMut(&Id) -> Result<Vec<u8>, String> {
    let mut map = std::collections::HashMap::new();
    map.insert(blob_hex(base), base.to_vec());
    map.insert(blob_hex(ours), ours.to_vec());
    map.insert(blob_hex(theirs), theirs.to_vec());
    move |id: &Id| {
        map.get(&id.to_hex())
            .cloned()
            .ok_or_else(|| format!("unexpected blob {}", id.to_hex()))
    }
}

// ---- 1. default 409 keeps its shape; the recipe names the opt-in -----------

#[tokio::test]
async fn default_land_still_409s_and_recipe_names_record_conflict() {
    let f = fixture();
    let (_, c2, c2p) = diverged(&f, vec![]).await;

    let resp = land(&f, &c2, "k").await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "ref_conflict");
    let details = &resp.body["error"]["details"];
    assert_eq!(details["conflicts"].as_array().unwrap().len(), 1);
    assert_eq!(details["conflicts"][0]["path"], "same.txt");
    let recipe = details["recipe"].as_str().unwrap();
    assert!(recipe.contains("re-land with the same key"), "{recipe}");
    // The opt-in is named in BOTH spellings: the CLI flag and the API field.
    assert!(recipe.contains("--record-conflict"), "{recipe}");
    assert!(recipe.contains("record_conflict"), "{recipe}");

    // Nothing persisted: the key stays unsettled, main unmoved.
    assert_eq!(f.t.count("SELECT COUNT(*) FROM lands"), 0);
    assert_eq!(main_target(&f).await, c2p);
}

// ---- 2. the opt-in settles a conflicted node ONCE --------------------------

#[tokio::test]
async fn record_conflict_lands_a_conflicted_node_once() {
    let f = fixture();
    let (_, c2, _) = diverged(&f, vec![]).await;

    // The default contract first: 409, key unsettled.
    let resp = land(&f, &c2, "k-record").await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "body: {}", resp.body);

    // The opt-in on the SAME key: the land settles once, conflicted.
    let resp = land_opt(&f, &c2, "k-record", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(
        resp.body["conflicts"].as_array().unwrap(),
        &[json!("same.txt")]
    );
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_eq!(resp.body["main"], json!(landed));
    assert_eq!(main_target(&f).await, landed);
    assert_eq!(f.t.land_state("k-record"), "conflicted");
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM lands WHERE key = 'k-record'"),
        1
    );

    // The landed node IS a format-v2 changeset: v2 magic, reps in payload,
    // id hashing under the v2 domain.
    let canonical = f.t.canonical_of(f.repo_id, &landed);
    assert!(canonical.starts_with(b"naatre-changeset\x02"));
    let decoded = changeset::decode(&canonical).unwrap();
    assert_eq!(decoded.conflicts.len(), 1);
    let rep = &decoded.conflicts[0];
    assert_eq!(rep.path, "same.txt");
    assert_eq!(
        rep.base,
        Some(Id::from_hex(&blob_hex(b"l1\nl2\nl3\n")).unwrap())
    );
    let mut sides = vec![
        Id::from_hex(&blob_hex(b"L1-ours\nl2\nl3\n")).unwrap(),
        Id::from_hex(&blob_hex(b"X1-main\nl2\nl3\n")).unwrap(),
    ];
    sides.sort_by_key(|id| id.0);
    assert_eq!(rep.sides, sides);
    assert_eq!(changeset::changeset_id(&decoded).unwrap().to_hex(), landed);

    // root_tree_id is the markers-materialized tree, recorded as such: the
    // node's recorded root tree's same.txt entry IS the marker blob, and
    // the /file read through the node serves its deterministic rendering.
    let expected = marker_bytes(
        rep,
        fetch_known(b"l1\nl2\nl3\n", b"L1-ours\nl2\nl3\n", b"X1-main\nl2\nl3\n"),
    )
    .unwrap();
    assert_eq!(cat(&f, &landed, "same.txt").await, expected);
    let root = f.t.root_tree_of(f.repo_id, &landed);
    let tree = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/trees/{}", root.to_hex()),
        &f.reader,
    )
    .await;
    assert_eq!(tree.status, StatusCode::OK, "{}", tree.body);
    let marker_blob = tree.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .find(|e| e["name"] == "same.txt")
        .expect("same.txt in the recorded root tree")["object_id"]
        .as_str()
        .unwrap();
    assert_eq!(marker_blob, object_id(Kind::Blob, &expected).to_hex());

    // Idempotent replay of the settled key: same result, still one row.
    let resp = land_opt(&f, &c2, "k-record", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(resp.body["landed"], json!(landed));
    assert_eq!(f.t.land_state("k-record"), "conflicted");
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM lands WHERE key = 'k-record'"),
        1
    );
    assert_eq!(main_target(&f).await, landed);
}

// ---- 3. a conflicted node rebases like any node; the conflict travels ------

#[tokio::test]
async fn conflicted_node_rebases_and_the_conflict_travels() {
    let f = fixture();
    // theirs ALSO adds other.txt, so the later re-rebase has a non-conflict
    // difference that keeps the new id distinct.
    let other = b"other-side\n".to_vec();
    let bo = put_blob(&f, &other).await;
    let (_, c2, c2p) = diverged(&f, vec![add("other.txt", &bo, other.len())]).await;

    // Record the conflict: m1 lands conflicted on main (parent c2p).
    let resp = land_opt(&f, &c2, "k-record", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let m1 = resp.body["landed"].as_str().unwrap().to_string();
    let m1_cs = changeset::decode(&f.t.canonical_of(f.repo_id, &m1)).unwrap();
    assert_eq!(m1_cs.conflicts.len(), 1);

    // A child of the conflicted node, and a divergent main that does NOT
    // descend from m1 (rewound by scaffold): m1 leaves main's ancestry.
    let fifth = b"fifth\n".to_vec();
    let bf = put_blob(&f, &fifth).await;
    let c5 = create_ok(
        &f,
        cs_body(
            json!([c2p.clone()]),
            json!([add("fifth.txt", &bf, fifth.len())]),
            "c5",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &c5);
    let third = b"third\n".to_vec();
    let bt = put_blob(&f, &third).await;
    let c3 = create_ok(
        &f,
        cs_body(
            json!([m1.clone()]),
            json!([add("third.txt", &bt, third.len())]),
            "c3",
        ),
    )
    .await;

    // The plain land rebases [c3, m1]: m1 carries its rep through (it is
    // NOT the settlement tip — c3 is), c3 goes on top. The settlement node
    // is clean, so the state is `done` and the response names no conflicts;
    // the conflicted node travels in main's ancestry.
    let resp = land(&f, &c3, "k-travel").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(resp.body["conflicts"].as_array().unwrap(), &[] as &[Value]);
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_eq!(f.t.land_state("k-travel"), "done");
    assert_eq!(main_target(&f).await, landed);

    // The carried rep is m1's rep, verbatim (pure carry), on the rebased
    // m1' (the landed node's parent) — and the markers re-materialize to
    // exactly the same content on main.
    let landed_cs = changeset::decode(&f.t.canonical_of(f.repo_id, &landed)).unwrap();
    let m1_rebased = landed_cs.parents[0];
    let carried = changeset::decode(&f.t.canonical_of(f.repo_id, &m1_rebased.to_hex())).unwrap();
    assert_eq!(carried.conflicts, m1_cs.conflicts);
    assert_ne!(m1_rebased.to_hex(), m1, "the rebased m1 got a new id");
    let rep = &carried.conflicts[0];
    let expected = marker_bytes(
        rep,
        fetch_known(b"l1\nl2\nl3\n", b"L1-ours\nl2\nl3\n", b"X1-main\nl2\nl3\n"),
    )
    .unwrap();
    assert_eq!(cat(&f, &landed, "same.txt").await, expected);
    // Every side's non-conflicted content survived the rebase.
    assert_eq!(cat(&f, &landed, "fifth.txt").await, fifth);
    assert_eq!(cat(&f, &landed, "third.txt").await, third);
    assert_eq!(cat(&f, &landed, "other.txt").await, other);
}

// ---- 4. unrepresentable conflicts refuse even under the opt-in -------------

#[tokio::test]
async fn record_conflict_still_refuses_a_deletion_side() {
    let f = fixture();
    let base = b"a\n".to_vec();
    let bb = put_blob(&f, &base).await;
    let c1 = create_ok(
        &f,
        cs_body(json!([]), json!([add("f.txt", &bb, base.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    let modified = b"b\n".to_vec();
    let bm = put_blob(&f, &modified).await;
    let c_mod = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("f.txt", &bm, modified.len())]),
            "c-mod",
        ),
    )
    .await;
    let c_del = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([del("f.txt")]), "c-del"),
    )
    .await;
    f.t.move_main(f.repo_id, &c_mod);

    // A deletion side has no blob id: the v2 representation cannot encode
    // it, so the opt-in refuses loudly instead of silently resolving.
    let resp = land_opt(&f, &c_del, "k-del", true).await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "ref_conflict");
    assert_eq!(
        resp.body["error"]["details"]["conflicts"][0]["reason"],
        json!("deleted by one side, modified by the other")
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM lands"), 0);
    assert_eq!(main_target(&f).await, c_mod);
}
