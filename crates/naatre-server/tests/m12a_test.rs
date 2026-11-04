//! M12a suite: first-class conflicts, server side — the default land keeps
//! the M4 409 + recipe (now naming `--record-conflict`); the explicit
//! `record_conflict` opt-in settles a conflicted (format-v2) node ONCE on
//! main (`lands.state = 'conflicted'`, `root_tree_id` = the
//! markers-materialized tree); a conflicted node rebases like any node and
//! its representations travel; a modify/delete conflict records with a
//! deletion side (F2) and only a file/directory duality refuses under the
//! opt-in.

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
use naatre_core::changeset::{FileChange, Op};
use naatre_core::conflict::{ConflictRep, marker_bytes};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::merge::{MergeOutcome, merge_lines, merge_trees_recording};
use naatre_server::state::{AppState, ServerState};
use naatre_server::tree_derive::MemNode;
use naatre_store::DataDir;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m12a";

// ---- harness (the m4a pattern) ---------------------------------------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
    /// Kept reachable for the CAS oracle (`state.cas.has`) — the nothing-
    /// written pins assert the CAS holds no bytes the refusal should have
    /// prevented.
    state: AppState,
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
        state,
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

    fn count_with_param(&self, sql: &str, param: &[u8]) -> i64 {
        self.db()
            .query_row(sql, rusqlite::params![param], |r| r.get(0))
            .unwrap()
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

/// GET with the `Naatre-Conflict-Format` header (`None` = absent).
async fn get_with_format(app: &Router, uri: &str, token: &str, format: Option<&str>) -> Resp {
    let mut builder = axum::http::Request::builder()
        .method("GET")
        .uri(uri)
        .header(header::AUTHORIZATION, format!("Bearer {token}"));
    if let Some(n) = format {
        builder = builder.header("Naatre-Conflict-Format", n);
    }
    let res = app
        .clone()
        .oneshot(builder.body(Body::empty()).unwrap())
        .await
        .expect("in-memory request");
    let status = res.status();
    let bytes = BodyExt::collect(res.into_body())
        .await
        .expect("body")
        .to_bytes()
        .to_vec();
    Resp {
        status,
        body: serde_json::from_slice(&bytes).unwrap_or(Value::Null),
    }
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

// ---- 4. a modify/delete conflict records with a deletion side (F2) ---------

/// Base c1 (`f.txt` = "a\n"); main moves to c_mod (modified); c_del deletes
/// the path. Returns (c1, c_mod, c_del).
async fn modify_delete(f: &Fixture) -> (String, String, String) {
    let base = b"a\n".to_vec();
    let bb = put_blob(f, &base).await;
    let c1 = create_ok(
        f,
        cs_body(json!([]), json!([add("f.txt", &bb, base.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    let modified = b"b\n".to_vec();
    let bm = put_blob(f, &modified).await;
    let c_mod = create_ok(
        f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("f.txt", &bm, modified.len())]),
            "c-mod",
        ),
    )
    .await;
    let c_del = create_ok(
        f,
        cs_body(json!([c1.clone()]), json!([del("f.txt")]), "c-del"),
    )
    .await;
    f.t.move_main(f.repo_id, &c_mod);

    (c1, c_mod, c_del)
}

/// P17: the opt-in records a modify/delete conflict — the rep carries a
/// deletion side, main's tree holds the F2 rendering byte for byte.
#[tokio::test]
async fn record_conflict_records_a_deletion_side() {
    let f = fixture();
    let (_, c_mod, c_del) = modify_delete(&f).await;

    let resp = land_opt(&f, &c_del, "k-del", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(
        resp.body["conflicts"].as_array().unwrap(),
        &[json!("f.txt")]
    );
    assert_eq!(f.t.land_state("k-del"), "conflicted");
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_eq!(main_target(&f).await, landed);

    let decoded = changeset::decode(&f.t.canonical_of(f.repo_id, &landed)).unwrap();
    assert_eq!(decoded.conflicts.len(), 1);
    let rep = &decoded.conflicts[0];
    assert!(rep.deleted);
    assert_eq!(rep.sides, vec![Id::from_hex(&blob_hex(b"b\n")).unwrap()]);
    assert_eq!(rep.base, Some(Id::from_hex(&blob_hex(b"a\n")).unwrap()));

    // The rendering, byte for byte: side 1 is the modification, the base
    // sits before the deleted divider, the deleted side renders last.
    let expected = format!(
        "<<<<<<< naatre side 1/2: {b}\nb\n||||||| naatre base: {a}\na\n\
         ======= naatre side 2/2: deleted\n>>>>>>> naatre conflict: f.txt\n",
        b = blob_hex(b"b\n"),
        a = blob_hex(b"a\n"),
    );
    assert_eq!(cat(&f, &landed, "f.txt").await, expected.as_bytes());
    assert_ne!(main_target(&f).await, c_mod);
}

/// A change with an explicit exec bit.
fn change_exec(path: &str, op: &str, blob: &str, size: usize, exec: bool) -> Value {
    json!({ "path": path, "op": op, "blob": blob, "size": size, "exec": exec })
}

/// The v2 representation carries an exec bit per side and for the base, so
/// a conflict whose sides differ only in the exec bit records (never a 500):
/// the rep holds the bits, the marker lines show ` +x`, the marker file is
/// not executable, and `lands.state = conflicted`.
#[tokio::test]
async fn record_conflict_records_an_exec_only_conflict() {
    let content = b"same\n".to_vec();
    let id = blob_hex(&content);

    // (a) chmod +x (same blob) on main vs a delete of the path.
    let f = fixture();
    let bx = put_blob(&f, &content).await;
    let c1 = create_ok(
        &f,
        cs_body(
            json!([]),
            json!([change_exec("f.txt", "add", &bx, content.len(), false)]),
            "c1",
        ),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    let c_chmod = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([change_exec("f.txt", "mod", &bx, content.len(), true)]),
            "c-chmod",
        ),
    )
    .await;
    let c_del = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([del("f.txt")]), "c-del"),
    )
    .await;
    f.t.move_main(f.repo_id, &c_chmod);
    let resp = land_opt(&f, &c_del, "k-a", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(f.t.land_state("k-a"), "conflicted");
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    let rep = &changeset::decode(&f.t.canonical_of(f.repo_id, &landed))
        .unwrap()
        .conflicts[0];
    assert!(rep.deleted);
    assert_eq!(rep.base, Some(Id::from_hex(&id).unwrap()));
    assert!(!rep.base_exec, "the base was not executable");
    assert_eq!(rep.sides, vec![Id::from_hex(&id).unwrap()]);
    assert_eq!(rep.sides_exec, vec![true], "the survivor is the chmod +x");
    let expected = format!(
        "<<<<<<< naatre side 1/2: {id} +x\nsame\n||||||| naatre base: {id}\nsame\n\
         ======= naatre side 2/2: deleted\n>>>>>>> naatre conflict: f.txt\n"
    );
    assert_eq!(cat(&f, &landed, "f.txt").await, expected.as_bytes());
    assert!(!marker_file_exec(&f, &landed, "f.txt").await);

    // (b) add/add: the same blob with different exec bits.
    let f = fixture();
    let bx = put_blob(&f, &content).await;
    let seed = create_ok(
        &f,
        cs_body(
            json!([]),
            json!([add("seed.txt", &bx, content.len())]),
            "seed",
        ),
    )
    .await;
    f.t.seed_main(f.repo_id, &seed);
    let c_plain = create_ok(
        &f,
        cs_body(
            json!([seed.clone()]),
            json!([change_exec("f.txt", "add", &bx, content.len(), false)]),
            "c-plain",
        ),
    )
    .await;
    let c_exec = create_ok(
        &f,
        cs_body(
            json!([seed.clone()]),
            json!([change_exec("f.txt", "add", &bx, content.len(), true)]),
            "c-exec",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &c_plain);
    let resp = land_opt(&f, &c_exec, "k-b", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(f.t.land_state("k-b"), "conflicted");
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    let rep = &changeset::decode(&f.t.canonical_of(f.repo_id, &landed))
        .unwrap()
        .conflicts[0];
    assert!(!rep.deleted);
    assert_eq!(rep.base, None);
    assert_eq!(rep.sides, vec![Id::from_hex(&id).unwrap(); 2]);
    assert_eq!(
        rep.sides_exec,
        vec![false, true],
        "ascending by (blob, exec)"
    );
    let expected = format!(
        "<<<<<<< naatre side 1/2: {id}\nsame\n======= naatre side 2/2: {id} +x\nsame\n\
         >>>>>>> naatre conflict: f.txt\n"
    );
    assert_eq!(cat(&f, &landed, "f.txt").await, expected.as_bytes());
    assert!(!marker_file_exec(&f, &landed, "f.txt").await);
}

/// The exec bit of `path` at `at`, from the /file route.
async fn marker_file_exec(f: &Fixture, at: &str, path: &str) -> bool {
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/file?at={at}&path={path}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    resp.body["exec"].as_bool().unwrap()
}

/// Record mode never refuses a single-path content conflict: for every
/// combination of base, ours and theirs (absent, or one of three contents
/// with either exec bit — 7^3 cases) `merge_trees_recording` answers Ok.
/// `take_conflict` turns a rep that `conflict::validate` rejects into an
/// Err, so Ok proves no reachable shape hits that guard (the decision
/// table only raises a conflict when ours, theirs and base all differ) —
/// the file/directory duality stays the one refusal and carries its own
/// recipe text.
#[test]
fn record_mode_never_refuses_a_single_path_conflict() {
    let contents: [&[u8]; 3] = [b"a\n", b"b\n", b"c\n"];
    // 0 = absent; 1..=6 = content (n-1)/2 with exec (n-1)%2 == 1.
    let state =
        |n: usize| -> Option<(usize, bool)> { (n > 0).then(|| ((n - 1) / 2, (n - 1) % 2 == 1)) };
    let node = |n: usize| -> MemNode {
        let mut node = MemNode::empty();
        if let Some((c, exec)) = state(n) {
            node.apply(&[FileChange {
                path: "f.txt".into(),
                op: Op::Add,
                blob: Some(object_id(Kind::Blob, contents[c])),
                size: Some(contents[c].len() as u64),
                exec: Some(exec),
            }])
            .unwrap();
        }
        node
    };
    let fetch = |id: &Id| -> Result<Vec<u8>, String> {
        contents
            .iter()
            .find(|c| object_id(Kind::Blob, c) == *id)
            .map(|c| c.to_vec())
            .ok_or_else(|| "unknown blob".to_string())
    };
    let mut recorded = 0;
    for b in 0..7 {
        for o in 0..7 {
            for t in 0..7 {
                let merged = merge_trees_recording(&node(b), &node(o), &node(t), fetch);
                let merged = merged.unwrap_or_else(|e| {
                    panic!("record mode refused base={b} ours={o} theirs={t}: {e:?}")
                });
                assert!(merged.oversized.is_empty());
                recorded += merged.conflicts.len();
            }
        }
    }
    assert!(recorded > 0, "the sweep reached recorded conflicts");
}

/// The deletion flag survives a rebase: a deletion-side conflicted node
/// lands (as an ancestor of a plain head) onto a main that moved to touch
/// another path. The rebased node carries the rep verbatim, `deleted`
/// included, and the marker file is byte-identical on main.
#[tokio::test]
async fn deletion_side_conflicted_node_rebases_and_the_conflict_travels() {
    let f = fixture();
    let (_, c_mod, c_del) = modify_delete(&f).await;

    // Record the modify/delete conflict: m1 lands conflicted (parent c_mod).
    let resp = land_opt(&f, &c_del, "k-record", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let m1 = resp.body["landed"].as_str().unwrap().to_string();
    let m1_cs = changeset::decode(&f.t.canonical_of(f.repo_id, &m1)).unwrap();
    assert_eq!(m1_cs.conflicts.len(), 1);
    assert!(m1_cs.conflicts[0].deleted);

    // main moves to a divergent tip touching ANOTHER path; a child of m1
    // lands plain on top, rebasing [c3, m1].
    let fifth = b"fifth\n".to_vec();
    let bf = put_blob(&f, &fifth).await;
    let c5 = create_ok(
        &f,
        cs_body(
            json!([c_mod.clone()]),
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
    let resp = land(&f, &c3, "k-travel").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_eq!(main_target(&f).await, landed);

    // The rebased m1' carries the rep verbatim, deletion flag included.
    let landed_cs = changeset::decode(&f.t.canonical_of(f.repo_id, &landed)).unwrap();
    let m1_rebased = landed_cs.parents[0];
    assert_ne!(m1_rebased.to_hex(), m1, "the rebased node got a new id");
    let carried = changeset::decode(&f.t.canonical_of(f.repo_id, &m1_rebased.to_hex())).unwrap();
    assert_eq!(carried.conflicts, m1_cs.conflicts);
    assert!(carried.conflicts[0].deleted, "the deletion flag survived");

    // The marker file is byte-identical on main, and the other paths landed.
    let expected = format!(
        "<<<<<<< naatre side 1/2: {b}\nb\n||||||| naatre base: {a}\na\n\
         ======= naatre side 2/2: deleted\n>>>>>>> naatre conflict: f.txt\n",
        b = blob_hex(b"b\n"),
        a = blob_hex(b"a\n"),
    );
    assert_eq!(cat(&f, &landed, "f.txt").await, expected.as_bytes());
    assert_eq!(cat(&f, &landed, "fifth.txt").await, fifth);
    assert_eq!(cat(&f, &landed, "third.txt").await, third);
}

// ---- 5. recipe shapes (M12a-fix) -------------------------------------------

#[tokio::test]
async fn recipes_lead_with_the_api_field_and_name_the_refusal() {
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

    // The DEFAULT 409 recipe leads with the CLI flag (M12b: it exists)
    // and keeps the API request field named for non-CLI clients.
    let resp = land(&f, &c_del, "k-def").await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "ref_conflict");
    let recipe = resp.body["error"]["details"]["recipe"].as_str().unwrap();
    assert!(
        recipe.starts_with(
            "to record the conflict on main as a conflicted changeset, re-land with the \
             same key and `naatre land --record-conflict`"
        ),
        "{recipe}"
    );
    assert!(
        recipe.contains("the API request field \"record_conflict\": true for non-CLI clients"),
        "{recipe}"
    );
}

/// The REFUSED-under-opt-in recipe: only a file/directory duality is
/// unrecordable now — the refusal names it and advises only the
/// resolution path, never the flag just used.
#[tokio::test]
async fn record_conflict_refuses_a_file_directory_duality() {
    let f = fixture();
    let seed = b"s\n".to_vec();
    let bs = put_blob(&f, &seed).await;
    let c1 = create_ok(
        &f,
        cs_body(json!([]), json!([add("seed.txt", &bs, seed.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    // main gains the FILE `d`; the landing node adds `d/x`: no path-level
    // conflict, but the merged result straddles file and directory.
    let bd = put_blob(&f, b"d\n").await;
    let c_file = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("d", &bd, 2)]), "c-file"),
    )
    .await;
    let bx = put_blob(&f, b"x\n").await;
    let c_dir = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("d/x", &bx, 2)]), "c-dir"),
    )
    .await;
    f.t.move_main(f.repo_id, &c_file);

    let resp = land_opt(&f, &c_dir, "k-ref", true).await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "ref_conflict");
    let recipe = resp.body["error"]["details"]["recipe"].as_str().unwrap();
    assert!(recipe.contains("cannot be recorded"), "{recipe}");
    assert!(
        recipe.contains("a file and a directory at the same path"),
        "{recipe}"
    );
    assert!(recipe.contains("fetch the conflict blobs"), "{recipe}");
    assert!(
        recipe.contains("record_conflict: true does not apply here"),
        "{recipe}"
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM lands"), 0);
    assert_eq!(main_target(&f).await, c_file);
}

// ---- 6/7. gate coverage: rep arms and the clean-merge sibling ---------------

/// Marks `blob` actively tombstoned in the repo (the gate refuses active
/// and purged alike; the pins DELETE the row for the positive direction).
fn tombstone(t: &TestApp, repo_id: i64, blob: &str) {
    let conn = t.db();
    conn.execute(
        "INSERT INTO redactions(repo_id, object_id, kind, path, marked_ns) \
         VALUES (?1, ?2, 'blob', NULL, ?3)",
        rusqlite::params![
            repo_id,
            Id::from_hex(blob).unwrap().0.as_slice(),
            BASE_NS as i64
        ],
    )
    .unwrap();
}

async fn nothing_written(f: &Fixture, marker: &Id) {
    assert_eq!(
        f.t.state.cas.has(&[*marker]).unwrap(),
        vec![false],
        "the blob's bytes must not be in the CAS"
    );
    assert_eq!(
        f.t.count_with_param(
            "SELECT COUNT(*) FROM repo_blobs WHERE object_id = ?1",
            marker.0.as_slice(),
        ),
        0,
        "no membership row for the unwritten blob"
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM lands"), 0, "key unsettled");
}

#[tokio::test]
async fn tombstoned_rep_side_refuses_the_record_before_any_write() {
    let f = fixture();
    let (_, c2, _) = diverged(&f, vec![]).await;
    // Tombstone THEIRS (the landing side's content): the minted rep would
    // embed it verbatim in a fresh servable marker blob.
    tombstone(&f.t, f.repo_id, &blob_hex(b"X1-main\nl2\nl3\n"));

    let changesets_before = f.t.count("SELECT COUNT(*) FROM changesets");
    let resp = land_opt(&f, &c2, "k-gate", true).await;
    assert_eq!(resp.status, StatusCode::GONE, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "object_redacted", "{}", resp.body);
    // The rep-arm guidance: the agent's way out is named in the refusal.
    let message = resp.body["error"]["message"].as_str().unwrap();
    assert!(
        message.contains(
            "A side of this conflict is redacted in this repo, so the land can neither \
             record nor merge it"
        ),
        "{message}"
    );
    assert!(
        message.contains("resolve with the admin who holds the redaction"),
        "{message}"
    );

    // NOTHING was written: no CAS bytes, no repo_blobs, no settlement, no
    // node — the refusal happened before the marker put.
    let mut sides = vec![
        Id::from_hex(&blob_hex(b"L1-ours\nl2\nl3\n")).unwrap(),
        Id::from_hex(&blob_hex(b"X1-main\nl2\nl3\n")).unwrap(),
    ];
    sides.sort_by_key(|id| id.0);
    let rep = ConflictRep {
        path: "same.txt".into(),
        base: Some(Id::from_hex(&blob_hex(b"l1\nl2\nl3\n")).unwrap()),
        base_exec: false,
        sides_exec: vec![false; sides.len()],
        sides,
        deleted: false,
    };
    let marker = object_id(
        Kind::Blob,
        &marker_bytes(
            &rep,
            fetch_known(b"l1\nl2\nl3\n", b"L1-ours\nl2\nl3\n", b"X1-main\nl2\nl3\n"),
        )
        .unwrap(),
    );
    nothing_written(&f, &marker).await;
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM changesets"),
        changesets_before
    );

    // Positive direction: the tombstone removed, the SAME key records.
    f.t.db().execute("DELETE FROM redactions", []).unwrap();
    let resp = land_opt(&f, &c2, "k-gate", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(f.t.land_state("k-gate"), "conflicted");
}

#[tokio::test]
async fn tombstoned_carried_side_refuses_the_rebase_before_any_write() {
    let f = fixture();
    let other = b"other-side\n".to_vec();
    let bo = put_blob(&f, &other).await;
    let (_, c2, c2p) = diverged(&f, vec![add("other.txt", &bo, other.len())]).await;
    let resp = land_opt(&f, &c2, "k-record", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let m1 = resp.body["landed"].as_str().unwrap().to_string();

    // Tombstone one of the CARRIED rep's sides: every rebase that carries
    // the rep re-records those ids — the gate must see them.
    tombstone(&f.t, f.repo_id, &blob_hex(b"L1-ours\nl2\nl3\n"));

    // A child of the conflicted node; main rewound off m1's ancestry.
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

    let changesets_before = f.t.count("SELECT COUNT(*) FROM changesets");
    let resp = land(&f, &c3, "k-travel").await;
    assert_eq!(resp.status, StatusCode::GONE, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "object_redacted");
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM changesets"),
        changesets_before,
        "no rebased node was written"
    );
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM lands WHERE key = 'k-travel'"),
        0,
        "the key is unsettled"
    );
}

#[tokio::test]
async fn tombstoned_merge_base_refuses_the_clean_merge_before_any_write() {
    let f = fixture();
    // A CLEAN modify/modify: base "l1\nl2\nl3\n", ours edits line 1,
    // theirs edits line 3 — the lines merge to the union.
    let base = b"l1\nl2\nl3\n".to_vec();
    let ours = b"L1-ours\nl2\nl3\n".to_vec();
    let theirs = b"l1\nl2\nL3-theirs\n".to_vec();
    let (bb, bo, bt) = (
        put_blob(&f, &base).await,
        put_blob(&f, &ours).await,
        put_blob(&f, &theirs).await,
    );
    let c1 = create_ok(
        &f,
        cs_body(json!([]), json!([add("same.txt", &bb, base.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    let c2 = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("same.txt", &bo, ours.len())]),
            "c2",
        ),
    )
    .await;
    let c2p = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("same.txt", &bt, theirs.len())]),
            "c2p",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &c2p);

    // Tombstone the merge BASE: merge_lines reads it and its content
    // flows into the clean union blob — the gate must refuse by INPUT id.
    tombstone(&f.t, f.repo_id, &blob_hex(&base));
    let resp = land(&f, &c2, "k-base").await;
    assert_eq!(resp.status, StatusCode::GONE, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "object_redacted");
    // The clean-merge base IS a merge_lines input: content flowed toward
    // a merged blob, so the guidance fires here too.
    assert!(
        resp.body["error"]["message"]
            .as_str()
            .unwrap()
            .contains("neither record nor merge"),
        "{}",
        resp.body
    );

    // The union blob the clean merge would have written: absent.
    let union = merge_lines(&base, &ours, &theirs);
    let MergeOutcome::Merged(bytes) = union else {
        panic!("the edits merge cleanly");
    };
    let merged_blob = object_id(Kind::Blob, &bytes);
    nothing_written(&f, &merged_blob).await;

    // Positive direction: untombstoned base — the same land settles.
    f.t.db().execute("DELETE FROM redactions", []).unwrap();
    let resp = land(&f, &c2, "k-base").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(
        cat(&f, resp.body["landed"].as_str().unwrap(), "same.txt").await,
        bytes
    );
}

// ---- mixed offenders keep the redacted-side remedy --------------------------

#[tokio::test]
async fn mixed_offenders_still_name_the_redacted_side_remedy() {
    let f = fixture();
    // A delta blob (extra.txt, clean theirs-wins) AND both rep sides
    // tombstoned. The delta content is CHOSEN to sort BELOW both side
    // hexes: the lowest-hex offender the message names is provably the
    // DELTA blob, so the guidance can only have come from the OTHER
    // offenders — exactly the mixed case the pick-only rule got wrong.
    let l1 = blob_hex(b"L1-ours\nl2\nl3\n");
    let x1 = blob_hex(b"X1-main\nl2\nl3\n");
    let min_side = if l1 < x1 { l1.clone() } else { x1.clone() };
    let mut delta = b"extra-delta-0\n".to_vec();
    let mut bd = blob_hex(&delta);
    let mut i = 0u32;
    while bd.as_str() >= min_side.as_str() {
        i += 1;
        delta = format!("extra-delta-{i}\n").into_bytes();
        bd = blob_hex(&delta);
    }
    let _uploaded = put_blob(&f, &delta).await;
    // The delta blob rides in the LANDING chain's own delta: c2 changes
    // same.txt (the future rep side L1) AND adds extra.txt (the delta).
    let base = b"l1\nl2\nl3\n".to_vec();
    let ours = b"L1-ours\nl2\nl3\n".to_vec();
    let theirs = b"X1-main\nl2\nl3\n".to_vec();
    let (bc, bo, bt) = (
        put_blob(&f, &base).await,
        put_blob(&f, &ours).await,
        put_blob(&f, &theirs).await,
    );
    let c1 = create_ok(
        &f,
        cs_body(json!([]), json!([add("same.txt", &bc, base.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    let c2 = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([
                add("extra.txt", &bd, delta.len()),
                mod_("same.txt", &bo, ours.len()),
            ]),
            "c2",
        ),
    )
    .await;
    let c2p = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("same.txt", &bt, theirs.len())]),
            "c2p",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &c2p);
    tombstone(&f.t, f.repo_id, &bd);
    tombstone(&f.t, f.repo_id, &l1);

    let resp = land_opt(&f, &c2, "k-mixed", true).await;
    assert_eq!(resp.status, StatusCode::GONE, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "object_redacted");
    let message = resp.body["error"]["message"].as_str().unwrap();
    // The named offender is the delta blob (it sorts lowest)…
    assert!(message.contains(&bd), "names the delta blob: {message}");
    // …and the redacted-side remedy still fires off the OTHER offender.
    assert!(
        message.contains("neither record nor merge"),
        "guidance survives the mixed set: {message}"
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM lands"), 0);
}

// ---- 8. marker overflow refuses 413 ----------------------------------------

#[tokio::test]
async fn marker_overflow_refuses_413_before_any_write() {
    let f = fixture();
    // Two ~40 MiB sides: the marker would carry both plus framing — over
    // the 64 MiB blob limit. Pre-sized by the recording merge; 413, not a
    // CAS-put 500.
    let big_a = vec![b'a'; 40 * 1024 * 1024];
    let big_b = vec![b'b'; 40 * 1024 * 1024];
    let base = b"seed\n".to_vec();
    let bbase = put_blob(&f, &base).await;
    let c1 = create_ok(
        &f,
        cs_body(json!([]), json!([add("big.txt", &bbase, base.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    let ba = put_blob(&f, &big_a).await;
    let c2 = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("big.txt", &ba, big_a.len())]),
            "c2",
        ),
    )
    .await;
    let bb = put_blob(&f, &big_b).await;
    let c2p = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("big.txt", &bb, big_b.len())]),
            "c2p",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &c2p);

    let blobs_before = f.t.count("SELECT COUNT(*) FROM repo_blobs");
    let resp = land_opt(&f, &c2, "k-big", true).await;
    assert_eq!(
        resp.status,
        StatusCode::PAYLOAD_TOO_LARGE,
        "body: {}",
        resp.body
    );
    assert_eq!(
        resp.body["error"]["details"]["path"],
        json!("big.txt"),
        "{}",
        resp.body
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM lands"), 0);
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM repo_blobs"),
        blobs_before,
        "no membership rows written by the refused land"
    );
}

// ---- 9. read-side conflicted flag -------------------------------------------

#[tokio::test]
async fn changeset_read_reports_conflicted() {
    let f = fixture();
    let (_, c2, c2p) = diverged(&f, vec![]).await;
    // v1 nodes answer conflicted: false.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{c2p}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["conflicted"], json!(false));
    // The recorded v2 node answers true.
    let resp = land_opt(&f, &c2, "k-flag", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{landed}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["conflicted"], json!(true));
}

// ---- 10. composition and the negative direction -----------------------------

#[tokio::test]
async fn carried_rep_and_same_path_reconflict_compose() {
    let f = fixture();
    // theirs adds other.txt so the re-conflicted rebase has a clean
    // difference to carry.
    let other = b"other-side\n".to_vec();
    let bo = put_blob(&f, &other).await;
    let (_, c2, c2p) = diverged(&f, vec![add("other.txt", &bo, other.len())]).await;
    let resp = land_opt(&f, &c2, "k-record", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let m1 = resp.body["landed"].as_str().unwrap().to_string();
    let m1_marker = cat(&f, &m1, "same.txt").await;
    let m1_marker_blob = object_id(Kind::Blob, &m1_marker);

    // A divergent main that INSERTS a line at the top of same.txt (W1):
    // m1 leaves main's ancestry, so the next land rebases [c3, m1] with
    // ours inserting at base position 0 — the same position the marker
    // block inserts at, different content: the carried path RE-CONFLICTS
    // (a plain line-1 replace would compose cleanly with the marker's
    // insertion-only diff).
    let w = b"W1-pre\nX1-main\nl2\nl3\n".to_vec();
    let bw = put_blob(&f, &w).await;
    let cw = create_ok(
        &f,
        cs_body(
            json!([c2p.clone()]),
            json!([mod_("same.txt", &bw, w.len())]),
            "cw",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &cw);
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

    let resp = land_opt(&f, &c3, "k-compose", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let landed = resp.body["landed"].as_str().unwrap().to_string();

    // The rebased m1' (the landed node's parent) carries EXACTLY ONE rep:
    // the new sides (ours W1, theirs m1's markers), the base theirs' old
    // common version (X1) — the carried rep is REPLACED, not duplicated.
    let landed_cs = changeset::decode(&f.t.canonical_of(f.repo_id, &landed)).unwrap();
    let m1_rebased = landed_cs.parents[0];
    let carried = changeset::decode(&f.t.canonical_of(f.repo_id, &m1_rebased.to_hex())).unwrap();
    assert_eq!(carried.conflicts.len(), 1, "replaced, not duplicated");
    let rep = &carried.conflicts[0];
    assert_eq!(rep.path, "same.txt");
    let mut expect_sides = vec![Id::from_hex(&blob_hex(&w)).unwrap(), m1_marker_blob];
    expect_sides.sort_by_key(|id| id.0);
    assert_eq!(
        rep.base,
        Some(Id::from_hex(&blob_hex(b"X1-main\nl2\nl3\n")).unwrap())
    );
    assert_eq!(rep.sides, expect_sides);
    // The recomposed markers materialize on main.
    let expected = marker_bytes(rep, fetch_known2(&w, &m1_marker, b"X1-main\nl2\nl3\n")).unwrap();
    assert_eq!(cat(&f, &landed, "same.txt").await, expected);
}

/// Fetch closure over exactly three contents.
fn fetch_known2(a: &[u8], b: &[u8], c: &[u8]) -> impl FnMut(&Id) -> Result<Vec<u8>, String> {
    let mut map = std::collections::HashMap::new();
    map.insert(blob_hex(a), a.to_vec());
    map.insert(blob_hex(b), b.to_vec());
    map.insert(blob_hex(c), c.to_vec());
    move |id: &Id| {
        map.get(&id.to_hex())
            .cloned()
            .ok_or_else(|| format!("unexpected blob {}", id.to_hex()))
    }
}

#[tokio::test]
async fn record_conflict_true_on_a_clean_land_is_done() {
    let f = fixture();
    let base = b"a\n".to_vec();
    let bb = put_blob(&f, &base).await;
    let c1 = create_ok(
        &f,
        cs_body(json!([]), json!([add("f.txt", &bb, base.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    let c2 = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([add("g.txt", &bb, base.len())]),
            "c2",
        ),
    )
    .await;

    // The flag's negative direction: no conflict anywhere — the land
    // settles DONE with an empty conflicts list, whatever the flag says.
    let resp = land_opt(&f, &c2, "k-clean", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(resp.body["conflicts"].as_array().unwrap(), &[] as &[Value]);
    assert_eq!(f.t.land_state("k-clean"), "done");
}

// ---- P6: /root refuses a client below the node's conflict format -----------

#[tokio::test]
async fn root_refuses_a_client_below_the_nodes_conflict_format() {
    let root_uri = |id: &str| format!("/v1/repos/{REPO}/changesets/{id}/root");

    // A format-1 node (content sides only) and its v1 parent.
    let f = fixture();
    let (c1, c2, _) = diverged(&f, vec![]).await;
    let resp = land_opt(&f, &c2, "k1", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let fmt1 = resp.body["landed"].as_str().unwrap().to_string();

    // No header: refused.
    let r = get_with_format(&f.t.app, &root_uri(&fmt1), &f.reader, None).await;
    assert_eq!(r.status, StatusCode::CONFLICT, "body: {}", r.body);
    assert_eq!(code_of(&r), "version_skew_conflicted");
    assert_eq!(
        r.body["error"]["details"],
        json!({"needed": 1, "announced": 0})
    );
    assert_eq!(
        r.body["error"]["message"],
        json!(format!(
            "changeset {fmt1} carries a first-class conflict in conflict format 1; this \
             client announces conflict format 0 in its Naatre-Conflict-Format header \
             (absent or unparsable = 0) and cannot materialize it honestly — upgrade \
             the client (GET /version reports the server's protocol)"
        ))
    );
    for ok in ["1", "2"] {
        let r = get_with_format(&f.t.app, &root_uri(&fmt1), &f.reader, Some(ok)).await;
        assert_eq!(r.status, StatusCode::OK, "header {ok}: {}", r.body);
    }
    // An unparsable header counts as 0.
    let r = get_with_format(&f.t.app, &root_uri(&fmt1), &f.reader, Some("two")).await;
    assert_eq!(r.status, StatusCode::CONFLICT);
    assert_eq!(
        r.body["error"]["details"],
        json!({"needed": 1, "announced": 0})
    );
    // A v1 node never refuses, with or without the header.
    let r = get_with_format(&f.t.app, &root_uri(&c1), &f.reader, None).await;
    assert_eq!(r.status, StatusCode::OK, "body: {}", r.body);

    // A format-2 node (a deletion side).
    let f = fixture();
    let (_, _, c_del) = modify_delete(&f).await;
    let resp = land_opt(&f, &c_del, "k2", true).await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let fmt2 = resp.body["landed"].as_str().unwrap().to_string();
    let r = get_with_format(&f.t.app, &root_uri(&fmt2), &f.reader, None).await;
    assert_eq!(r.status, StatusCode::CONFLICT, "body: {}", r.body);
    assert_eq!(
        r.body["error"]["details"],
        json!({"needed": 2, "announced": 0})
    );
    let r = get_with_format(&f.t.app, &root_uri(&fmt2), &f.reader, Some("1")).await;
    assert_eq!(r.status, StatusCode::CONFLICT, "body: {}", r.body);
    assert_eq!(
        r.body["error"]["details"],
        json!({"needed": 2, "announced": 1})
    );
    let r = get_with_format(&f.t.app, &root_uri(&fmt2), &f.reader, Some("2")).await;
    assert_eq!(r.status, StatusCode::OK, "body: {}", r.body);
    assert!(r.body["root_tree_id"].is_string());
    // Header 2 never refuses (no node needs format 3): 200 is the only outcome.
}
