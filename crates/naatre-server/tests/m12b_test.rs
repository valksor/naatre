//! M12b suite, server side: blame reports conflicted regions as conflicted
//! (the GC walker's byte-level pins live in `m10a_test.rs`, the CLI-level
//! conflicted-flow pins in `naatre-cli/tests/m12b_e2e_test.rs`).
//!
//! A walked node whose canonical carries a conflict representation at the
//! blamed path contributes the marker-rendering lines it introduced under
//! `conflicted: true` — the conflict, never a phantom author. The
//! detection is per path (a rep at another path does not taint this one)
//! and survives a resolution descendant (blame at the child still names
//! the conflicted introducer).

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

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::derive::derive_pass;
use naatre_server::fsck;
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m12b";

// ---- harness (the m12a pattern, with the derive queue driven) ---------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
    state: AppState,
    /// The injected clock (the GC pin moves it past the grace window).
    clock: Arc<AtomicU64>,
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
    // The production derive kinds registered, the m6b shape — blame's
    // read routes gate on the registration.
    for deriver in naatre_server::derived::production_kinds() {
        state.register_deriver(deriver).unwrap();
    }
    // No serve task runs here: the queue is driven by hand (`drain`).
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
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

    /// Drives the derive queue until nothing is pending, then marks every
    /// production kind's boot backfill complete (the m6b pattern — no
    /// serve task runs here, so `serve`'s boot task would never set it).
    async fn drain(&self) {
        for _ in 0..100 {
            derive_pass(&self.state).await.expect("derive pass");
            let pending = self.count("SELECT COUNT(*) FROM derive_state WHERE state = 'pending'");
            if pending == 0 {
                break;
            }
        }
        assert_eq!(
            self.count("SELECT COUNT(*) FROM derive_state WHERE state = 'pending'"),
            0,
            "queue drained"
        );
        for kind in [
            naatre_server::derived::KIND_BLAME,
            naatre_server::derived::KIND_PATH_HISTORY,
            naatre_server::derived::KIND_COMMIT_GRAPH,
        ] {
            self.db()
                .execute(
                    "INSERT INTO meta(key, value) VALUES (?1, '0') \
                     ON CONFLICT(key) DO UPDATE SET value = excluded.value",
                    rusqlite::params![format!("derive_backfill_done:{kind}")],
                )
                .unwrap();
        }
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

fn blob_hex(content: &[u8]) -> String {
    object_id(Kind::Blob, content).to_hex()
}

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
    let resp = send(
        &f.t.app,
        "POST",
        &format!("/v1/repos/{REPO}/changesets"),
        Some(&f.writer),
        Some(body),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "body: {}", resp.body);
    resp.body["id"].as_str().unwrap().to_string()
}

fn change(path: &str, op: &str, blob: &str, size: usize) -> Value {
    json!({ "path": path, "op": op, "blob": blob, "size": size, "exec": false })
}

fn cs_body(parents: Value, changes: Value, message: &str) -> Value {
    json!({
        "parents": parents,
        "author": "Agent <agent@naatre.dev>",
        "message": message,
        "changes": changes,
    })
}

/// The single conflicted main-line node (the m12a shape): c1 seeds
/// `f.txt` on main; c2 (ours) and c2p (theirs) diverge; main moves to
/// c2p; the record-conflict land of c2 settles the conflicted node.
/// Returns (v2 node id, the id of c1).
async fn conflicted_main(f: &Fixture) -> (String, String) {
    let base = b"l1\nl2\nl3\n".to_vec();
    let ours = b"L1\nl2\nl3\n".to_vec();
    let theirs = b"X1\nl2\nl3\n".to_vec();
    let (bc, bo, bt) = (
        put_blob(f, &base).await,
        put_blob(f, &ours).await,
        put_blob(f, &theirs).await,
    );
    let c1 = create_ok(
        f,
        cs_body(
            json!([]),
            json!([change("f.txt", "add", &bc, base.len())]),
            "c1",
        ),
    )
    .await;
    let c2 = create_ok(
        f,
        cs_body(
            json!([c1.clone()]),
            json!([change("f.txt", "mod", &bo, ours.len())]),
            "c2",
        ),
    )
    .await;
    let c2p = create_ok(
        f,
        cs_body(
            json!([c1.clone()]),
            json!([change("f.txt", "mod", &bt, theirs.len())]),
            "c2p",
        ),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    f.t.move_main(f.repo_id, &c2p);
    let resp = send(
        &f.t.app,
        "POST",
        &format!("/v1/repos/{REPO}/land"),
        Some(&f.writer),
        Some(json!({ "head": c2, "key": "k-m12b", "record_conflict": true })),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let v2 = resp.body["landed"].as_str().unwrap().to_string();
    (v2, c1)
}

async fn blame(f: &Fixture, at: &str, path: &str) -> Resp {
    send(
        &f.t.app,
        "GET",
        &format!("/v1/repos/{REPO}/blame?at={at}&path={}", urlencode(path)),
        Some(&f.reader),
        None,
    )
    .await
}

fn urlencode(s: &str) -> String {
    s.bytes()
        .map(|b| {
            if b.is_ascii_alphanumeric() || b"-.~_".contains(&b) {
                (b as char).to_string()
            } else {
                format!("%{b:02X}")
            }
        })
        .collect()
}

// ---- blame: conflicted regions report as conflicted -------------------------

/// Blame at the conflicted node: the marker-rendering lines the v2 node
/// introduced carry `conflicted: true` (their cs is the v2 node — the
/// conflict, never a phantom author); the seed content's lines stay
/// plain. At a resolution descendant, the marker lines STILL name the
/// conflicted introducer.
#[tokio::test]
async fn blame_reports_conflicted_regions_as_conflicted() {
    let f = fixture();
    let (v2, c1) = conflicted_main(&f).await;
    f.t.drain().await;

    let resp = blame(&f, &v2, "f.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["binary"], json!(false));
    let lines = resp.body["lines"].as_array().unwrap();
    assert!(!lines.is_empty(), "the marker file blames to lines");
    // The marker rendering's divider lines exist in the file and are the
    // v2 node's introductions — every one reports conflicted, attributed
    // to the v2 node.
    let conflicted: Vec<&Value> = lines
        .iter()
        .filter(|l| l["conflicted"].as_bool() == Some(true))
        .collect();
    assert!(
        conflicted.len() >= 3,
        "the dividers + side content report conflicted: {lines:?}"
    );
    for l in &conflicted {
        assert_eq!(
            l["cs"],
            json!(v2),
            "a conflicted line attributes to the conflicted node: {l}"
        );
        assert_eq!(
            l["author"],
            json!("Agent <agent@naatre.dev>"),
            "the JSON keeps the author; the CLI renders the conflict name"
        );
    }
    let texts: Vec<&str> = conflicted
        .iter()
        .map(|l| l["text"].as_str().unwrap_or(""))
        .collect();
    assert!(
        texts.iter().any(|t| t.starts_with("<<<<<<< naatre side")),
        "the opener line is in the conflicted region: {texts:?}"
    );
    assert!(
        texts
            .iter()
            .any(|t| t.starts_with(">>>>>>> naatre conflict:")),
        "the closer line is in the conflicted region: {texts:?}"
    );
    // The untouched seed lines (l2/l3 survive inside the base section)
    // blame to c1, plain.
    let plain: Vec<&Value> = lines
        .iter()
        .filter(|l| l["conflicted"].as_bool() == Some(false) && l["cs"] == json!(c1))
        .collect();
    assert!(
        !plain.is_empty(),
        "the seed content's lines stay unconflicted: {lines:?}"
    );

    // A resolution descendant: the resolution node introduces only the
    // resolved content (its lines blame to it, plain); the marker lines
    // STILL attribute to the conflicted introducer — walking past the
    // resolution keeps the conflict named.
    let resolved = b"resolved\nl2\nl3\n".to_vec();
    let br = put_blob(&f, &resolved).await;
    let r = create_ok(
        &f,
        cs_body(
            json!([v2.clone()]),
            json!([change("f.txt", "mod", &br, resolved.len())]),
            "resolve conflict at f.txt",
        ),
    )
    .await;
    f.t.drain().await;
    let resp = blame(&f, &r, "f.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let lines = resp.body["lines"].as_array().unwrap();
    let still_conflicted: Vec<&Value> = lines
        .iter()
        .filter(|l| l["conflicted"].as_bool() == Some(true))
        .collect();
    assert!(
        !still_conflicted.is_empty(),
        "the marker region still reports conflicted past the resolution: {lines:?}"
    );
    for l in &still_conflicted {
        assert_eq!(l["cs"], json!(v2), "still attributed to the v2 node: {l}");
    }
    assert!(
        lines
            .iter()
            .any(|l| l["cs"] == json!(r) && l["conflicted"].as_bool() == Some(false)),
        "the resolution's own lines are plain: {lines:?}"
    );
}

/// The detection is per path: at the conflicted node, a path with no rep
/// (untouched by the conflict) blames with no conflicted lines at all.
#[tokio::test]
async fn blame_conflicted_flag_is_per_path() {
    let f = fixture();
    // other.txt is seeded on c1 alongside f.txt, so it exists at the v2
    // node untouched by any representation.
    let base_content = b"l1\nl2\nl3\n".to_vec();
    let other_content = b"other\n".to_vec();
    let (bc, bt) = (
        put_blob(&f, &base_content).await,
        put_blob(&f, &other_content).await,
    );
    let c1 = create_ok(
        &f,
        cs_body(
            json!([]),
            json!([
                change("f.txt", "add", &bc, base_content.len()),
                change("other.txt", "add", &bt, other_content.len()),
            ]),
            "c1",
        ),
    )
    .await;
    let ours = b"L1\nl2\nl3\n".to_vec();
    let theirs = b"X1\nl2\nl3\n".to_vec();
    let (bo, bt2) = (put_blob(&f, &ours).await, put_blob(&f, &theirs).await);
    let c2 = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([change("f.txt", "mod", &bo, ours.len())]),
            "c2",
        ),
    )
    .await;
    let c2p = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([change("f.txt", "mod", &bt2, theirs.len())]),
            "c2p",
        ),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    f.t.move_main(f.repo_id, &c2p);
    let resp = send(
        &f.t.app,
        "POST",
        &format!("/v1/repos/{REPO}/land"),
        Some(&f.writer),
        Some(json!({ "head": c2, "key": "k-per-path", "record_conflict": true })),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let v2 = resp.body["landed"].as_str().unwrap().to_string();
    f.t.drain().await;

    // f.txt: conflicted region reported.
    let resp = blame(&f, &v2, "f.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let lines = resp.body["lines"].as_array().unwrap();
    assert!(
        lines
            .iter()
            .any(|l| l["conflicted"].as_bool() == Some(true)),
        "the conflicted path reports conflict: {lines:?}"
    );
    // other.txt: the same v2 node, a path with no rep — nothing
    // conflicted, and its lines attribute to the seed commit.
    let resp = blame(&f, &v2, "other.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let lines = resp.body["lines"].as_array().unwrap();
    assert!(
        !lines.is_empty(),
        "other.txt exists at the conflicted node: {lines:?}"
    );
    assert!(
        lines
            .iter()
            .all(|l| l["conflicted"].as_bool() == Some(false)),
        "a rep-less path never reports conflicted: {lines:?}"
    );
}

// ---- legacy conflicted stores: fsck --repair backfills the marker rows ------

/// The m12a defect (fixed in m12b: land now records its merged blobs'
/// objects rows) left legacy conflicted stores whose MARKER blobs have CAS
/// bytes and live references (repo_blobs membership, file_changes, and the
/// materialized root tree's tree_entries) but NO objects record row. Pin:
/// the repair arm's candidate query covers all three reference arms — the
/// marker blob is backfilled by `fsck --repair`, gains EXACTLY that row,
/// and the store exits fsck-clean.
#[tokio::test]
async fn fsck_repair_backfills_legacy_marker_blob_rows() {
    let f = fixture();
    let (v2, _) = conflicted_main(&f).await;

    // The marker blob: the file entry of the conflicted node's
    // markers-materialized root tree.
    let marker: Id = {
        let conn = f.t.db();
        let root: Vec<u8> = conn
            .query_row(
                "SELECT root_tree_id FROM changesets WHERE repo_id = ?1 AND id = ?2",
                rusqlite::params![f.repo_id, Id::from_hex(&v2).unwrap().0.as_slice()],
                |r| r.get(0),
            )
            .unwrap();
        let hex: Vec<u8> = conn
            .query_row(
                "SELECT object_id FROM tree_entries WHERE tree_id = ?1 \
                 AND name = 'f.txt' AND kind = 0",
                rusqlite::params![root],
                |r| r.get(0),
            )
            .unwrap();
        Id(<[u8; 32]>::try_from(hex).unwrap())
    };
    // The legacy shape (the m12a defect): the store references the
    // marker blob (membership + file_changes + tree entry) and holds the
    // bytes, but the objects record row is missing. The m12b land
    // records the row, so the pin deletes it by hand to reproduce the
    // defect before exercising the repair.
    let conn = f.t.db();
    assert_eq!(
        conn.query_row(
            "SELECT COUNT(*) FROM repo_blobs WHERE object_id = ?1",
            rusqlite::params![marker.0.as_slice()],
            |r| r.get::<_, i64>(0),
        )
        .unwrap(),
        1,
        "membership references the marker blob"
    );
    assert_eq!(
        conn.query_row(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            rusqlite::params![marker.0.as_slice()],
            |r| r.get::<_, i64>(0),
        )
        .unwrap(),
        1,
        "the m12b land recorded the row (the delete simulates the defect)"
    );
    conn.execute(
        "DELETE FROM objects WHERE object_id = ?1",
        rusqlite::params![marker.0.as_slice()],
    )
    .unwrap();
    drop(conn);

    // The scan flags the unrecorded CAS blob; repair backfills EXACTLY
    // the marker blob's row.
    let before = fsck::fsck(&f.t.state).await.unwrap();
    assert!(before.cas_unrecorded >= 1, "{before:?}");
    assert!(before.backfilled.is_empty(), "{before:?}");
    let repaired = fsck::fsck_repair(&f.t.state).await.unwrap();
    assert_eq!(
        repaired.backfilled,
        vec![marker.to_hex()],
        "exactly the marker blob's row is backfilled: {repaired:?}"
    );
    assert_eq!(
        f.t.db()
            .query_row(
                "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
                rusqlite::params![marker.0.as_slice()],
                |r| r.get::<_, i64>(0),
            )
            .unwrap(),
        1,
        "the record row is backfilled"
    );
    // The post-repair scan is clean: no unrecorded CAS blobs, no missing
    // bytes, no dangling references of the blob-row class.
    let after = fsck::fsck(&f.t.state).await.unwrap();
    assert_eq!(after.cas_unrecorded, 0, "{after:?}");
    assert_eq!(after.bytes_missing, 0, "{after:?}");
    assert!(
        after
            .danglings
            .iter()
            .all(|(name, n)| *n == 0 || name == "changeset_parent_missing"),
        "fsck-clean after repair: {after:?}"
    );
}

// ---- the poison boundary applies the conflicted check (m12b-fix) ------------

/// Blame at a resolution child whose CONFLICTED ancestor's derive row is
/// poisoned (the quarantine path): the walk stops at the poisoned node,
/// the marker-rendering lines attribute to the boundary — and they report
/// conflicted (compliance A3), degraded, with the node named in
/// poisoned_cs. The same check the touched-path branch applies.
#[tokio::test]
async fn blame_poisoned_conflicted_boundary_reports_conflict() {
    let f = fixture();
    let (v2, _) = conflicted_main(&f).await;
    // A resolution child of the conflicted node.
    let resolved = b"resolved\nl2\nl3\n".to_vec();
    let br = put_blob(&f, &resolved).await;
    let r = create_ok(
        &f,
        cs_body(
            json!([v2.clone()]),
            json!([change("f.txt", "mod", &br, resolved.len())]),
            "resolve",
        ),
    )
    .await;
    f.t.drain().await;
    // Quarantine the conflicted node (test scaffolding: the poison state
    // a real quarantine produces; the drained queue keeps it).
    f.t.db()
        .execute(
            "UPDATE derive_state SET state = 'poison' \
             WHERE repo_id = ?1 AND cs = ?2 AND kind = \
             (SELECT id FROM derive_kinds WHERE name = 'blame')",
            rusqlite::params![f.repo_id, Id::from_hex(&v2).unwrap().0.as_slice()],
        )
        .unwrap();

    let resp = blame(&f, &r, "f.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(true), "{}", resp.body);
    assert!(
        resp.body["poisoned_cs"]
            .as_array()
            .unwrap()
            .iter()
            .any(|x| x == &json!(v2)),
        "the conflicted node is the poison boundary: {}",
        resp.body
    );
    let lines = resp.body["lines"].as_array().unwrap();
    let boundary_conflicted: Vec<&Value> = lines
        .iter()
        .filter(|l| l["cs"] == json!(v2) && l["conflicted"].as_bool() == Some(true))
        .collect();
    assert!(
        !boundary_conflicted.is_empty(),
        "the boundary's marker lines report conflicted: {lines:?}"
    );
}

// ---- P5: GC and blame after a deletion-side land ----------------------------

/// A modify/delete conflict recorded on main (the F2 deletion side): c1
/// seeds `f.txt` = "a\n"; main moves to c_mod ("b\n"); the record-conflict
/// land of c_del settles the v2 node. Returns (v2 id, c1, c_mod).
async fn deletion_side_main(f: &Fixture) -> (String, String, String) {
    let (a, b) = (b"a\n".to_vec(), b"b\n".to_vec());
    let (ba, bb) = (put_blob(f, &a).await, put_blob(f, &b).await);
    let c1 = create_ok(
        f,
        cs_body(
            json!([]),
            json!([change("f.txt", "add", &ba, a.len())]),
            "c1",
        ),
    )
    .await;
    let c_mod = create_ok(
        f,
        cs_body(
            json!([c1.clone()]),
            json!([change("f.txt", "mod", &bb, b.len())]),
            "c-mod",
        ),
    )
    .await;
    let c_del = create_ok(
        f,
        cs_body(
            json!([c1.clone()]),
            json!([json!({ "path": "f.txt", "op": "del" })]),
            "c-del",
        ),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);
    f.t.move_main(f.repo_id, &c_mod);
    let resp = send(
        &f.t.app,
        "POST",
        &format!("/v1/repos/{REPO}/land"),
        Some(&f.writer),
        Some(json!({ "head": c_del, "key": "k-del", "record_conflict": true })),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    (resp.body["landed"].as_str().unwrap().to_string(), c1, c_mod)
}

/// P5. After a deletion-side land: GC past the grace window keeps the
/// surviving side's blob and the base blob byte-identical (also with every
/// other reference to them stripped, so the representation is provably the
/// only one), `fsck` is clean, and blame of the marker file reports the
/// marker lines as conflicted.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn gc_and_blame_after_a_deletion_side_land() {
    use naatre_server::gc::{GC_DEFAULT_GRACE_SECS, gc_pass};
    const GRACE_NS: u64 = GC_DEFAULT_GRACE_SECS * 1_000_000_000;
    let f = fixture();
    let (v2, c1, c_mod) = deletion_side_main(&f).await;
    let surviving = Id::from_hex(&blob_hex(b"b\n")).unwrap();
    let base = Id::from_hex(&blob_hex(b"a\n")).unwrap();

    // Blame of the marker file at main: the marker lines report as
    // conflicted (the deletion divider included).
    f.t.drain().await;
    let resp = blame(&f, &v2, "f.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let texts: Vec<(&str, bool)> = resp.body["lines"]
        .as_array()
        .unwrap()
        .iter()
        .map(|l| {
            (
                l["text"].as_str().unwrap_or(""),
                l["conflicted"].as_bool() == Some(true),
            )
        })
        .collect();
    for needle in [
        "<<<<<<< naatre side 1/2:",
        "======= naatre side 2/2: deleted",
        ">>>>>>> naatre conflict: f.txt",
    ] {
        assert!(
            texts.iter().any(|(t, c)| t.starts_with(needle) && *c),
            "{needle} blames as conflicted: {texts:?}"
        );
    }

    // Garbage to prove the passes collect: an unlanded commit.
    let bo = put_blob(&f, b"orphan\n").await;
    create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([change("o.txt", "add", &bo, 7)]),
            "orphan",
        ),
    )
    .await;
    let collect = |ticks: u64| {
        let f = &f;
        async move {
            f.t.clock
                .store(BASE_NS + ticks * GRACE_NS, Ordering::Relaxed);
            gc_pass(&f.t.state).await.unwrap()
        }
    };
    collect(2).await;
    let report = collect(3).await;
    assert!(report.cs_collected >= 1, "garbage collects: {report:?}");
    for (name, id, content) in [("surviving", surviving, b"b\n"), ("base", base, b"a\n")] {
        assert_eq!(
            f.t.state.cas.get(&id).unwrap(),
            content.to_vec(),
            "the {name} blob is byte-identical after GC"
        );
    }
    let after = fsck::fsck(&f.t.state).await.unwrap();
    assert!(after.clean(), "fsck is clean after GC: {after:?}");

    // The representation as the ONLY reference: strip the lands seeds and
    // the side changesets' rows (m10a's scaffolding), then collect again.
    // Negative control: a blob nothing references. The same passes must
    // collect it, so a surviving side blob below cannot be a pass that
    // simply never collects anything.
    let control = Id::from_hex(&put_blob(&f, b"control-unreferenced\n").await).unwrap();
    let trees_before = f.t.count("SELECT COUNT(*) FROM trees");
    {
        let conn = f.t.db();
        conn.execute(
            "UPDATE lands SET head = NULL, base = NULL WHERE repo_id = ?1",
            rusqlite::params![f.repo_id],
        )
        .unwrap();
        for cs in [&c1, &c_mod] {
            let id = Id::from_hex(cs).unwrap();
            for sql in [
                "DELETE FROM file_changes WHERE repo_id = ?1 AND cs = ?2",
                "DELETE FROM changesets WHERE repo_id = ?1 AND id = ?2",
            ] {
                conn.execute(sql, rusqlite::params![f.repo_id, id.0.as_slice()])
                    .unwrap();
            }
        }
    }
    let first = collect(5).await;
    let second = collect(6).await;
    // Blob collection is two-phase (condemn, then delete on a later pass):
    // pass 7 condemns the unreferenced blobs, pass 8 deletes what is
    // still condemned. The rep-only blobs below must outlive pass 8.
    let third = collect(7).await;
    let fourth = collect(8).await;
    // The strip made the side changesets' root trees garbage and the
    // passes collected them (and the control blob), while the rep-only
    // blobs below stay: they survive because of the representation.
    assert!(
        first.trees_collected
            + second.trees_collected
            + third.trees_collected
            + fourth.trees_collected
            >= 1,
        "the strip left collectable trees and the passes collected them: {first:?} {second:?} {third:?} {fourth:?}"
    );
    assert!(
        f.t.count("SELECT COUNT(*) FROM trees") < trees_before,
        "tree rows were removed by the passes"
    );
    assert!(
        first.blobs_collected
            + second.blobs_collected
            + third.blobs_collected
            + fourth.blobs_collected
            >= 1,
        "the passes collected blobs: {first:?} {second:?} {third:?} {fourth:?}"
    );
    assert_eq!(
        f.t.state.cas.has(&[control]).unwrap(),
        vec![false],
        "the unreferenced control blob was collected"
    );
    for (name, id, content) in [("surviving", surviving, b"b\n"), ("base", base, b"a\n")] {
        assert_eq!(
            f.t.state
                .cas
                .get(&id)
                .unwrap_or_else(|e| panic!("the {name} blob was collected: {e}")),
            content.to_vec(),
            "the {name} blob survives through the representation alone"
        );
    }
}
