//! M5a suite: snapshots + TTL GC. The plan's named pins live here:
//! POST presence-validation (missing blob → 422 with `missing_blobs`, zero
//! rows), snapshot blobs living in `ephemeral/` and never `objects/`
//! (history CAS byte-identical across a full expire+GC cycle), expiry
//! enforced at BOTH gates (metadata and bytes → 410 `snapshot_expired`),
//! GC deleting expired dirs AND rows, the rowless sweep (the
//! crash-between-upload-and-row convergence), the 60 s TTL floor, and the
//! own-principal GET rule (another principal's snapshot id → 404).
//!
//! The GC passes are driven directly through [`gc_snapshots`] with the
//! injected clock; the POST-time/serve background callers are off-switched
//! by that determinism (they run on the same fn).

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{HeaderValue, StatusCode, header};
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use http_body_util::BodyExt;
use rusqlite::Connection;
use serde_json::{Value, json};
use tokio::net::TcpListener;
use tokio::sync::oneshot;
use tower::ServiceExt;

use naatre_server::serve::{ServeConfig, serve};

use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::tree::{self, EntryKind, Tree, TreeEntry};
use naatre_server::snapshots_gc::{SNAPSHOT_MAX_TTL_NS, gc_snapshots};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m5a";
const DEFAULT_TTL_NS: u64 = 604_800 * 1_000_000_000;

// ---- harness ---------------------------------------------------------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
    data_dir: PathBuf,
    state: AppState,
    clock: Arc<AtomicU64>,
}

/// `build_app` with a state tweak applied before the `Arc` wrap.
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
    let data_dir = dir.path().to_path_buf();
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    TestApp {
        _dir: dir,
        app,
        db_path,
        data_dir,
        state,
        clock,
    }
}

impl TestApp {
    fn db(&self) -> Connection {
        let conn = Connection::open(&self.db_path).unwrap();
        conn.busy_timeout(std::time::Duration::from_secs(5))
            .unwrap();
        conn
    }

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
             VALUES (?1, ?2, NULL, 0, ?3)",
            rusqlite::params![principal, hash, scope_path],
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

    fn count(&self, sql: &str) -> i64 {
        self.db()
            .query_row(sql, [], |r| r.get(0))
            .expect("count query")
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

/// Raw-bytes PUT/GET that returns the raw response (status + headers + body).
async fn send_bytes(
    app: &Router,
    method: &str,
    uri: &str,
    token: &str,
    payload: Option<Vec<u8>>,
) -> (StatusCode, Option<HeaderValue>, Vec<u8>) {
    let builder = axum::http::Request::builder()
        .method(method)
        .uri(uri)
        .header(header::AUTHORIZATION, format!("Bearer {token}"))
        .header(header::CONTENT_TYPE, "application/octet-stream");
    let req = builder
        .body(Body::from(payload.unwrap_or_default()))
        .unwrap();
    let res = app.clone().oneshot(req).await.expect("in-memory request");
    let status = res.status();
    let ctype = res.headers().get(header::CONTENT_TYPE).cloned();
    let bytes = BodyExt::collect(res.into_body())
        .await
        .expect("body")
        .to_bytes()
        .to_vec();
    (status, ctype, bytes)
}

fn code_of(r: &Resp) -> String {
    r.body["error"]["code"].as_str().unwrap_or("").to_string()
}

fn blob_hex(content: &[u8]) -> String {
    object_id(Kind::Blob, content).to_hex()
}

/// A 64-hex snapshot id with a distinguishable seed byte.
fn snap_hex(seed: u8) -> String {
    format!("{:062x}{seed:02x}", 0u128)
}

/// Encodes a flat one-directory tree; returns (root id hex, [canonical b64]).
fn flat_tree(files: &[(&str, Id, u64, bool)]) -> (String, Vec<String>) {
    let entries: Vec<TreeEntry> = files
        .iter()
        .map(|(name, blob, size, exec)| TreeEntry {
            name: name.to_string(),
            kind: EntryKind::File,
            object: *blob,
            size: *size,
            exec: *exec,
        })
        .collect();
    tree_with_id(entries)
}

/// Encodes one tree from entries (any mix of files and dirs); returns
/// (id hex, canonical b64) — the building block for nested snapshot trees.
fn tree_with_id(mut entries: Vec<TreeEntry>) -> (String, Vec<String>) {
    entries.sort_by(|a, b| a.name.as_bytes().cmp(b.name.as_bytes()));
    let t = Tree { entries };
    let id = tree::tree_id(&t).unwrap();
    let canonical = tree::encode(&t).unwrap();
    (id.to_hex(), vec![STANDARD.encode(canonical)])
}

fn file_entry(name: &str, blob: Id, size: u64, exec: bool) -> TreeEntry {
    TreeEntry {
        name: name.to_string(),
        kind: EntryKind::File,
        object: blob,
        size,
        exec,
    }
}

fn dir_entry(name: &str, child_tree_hex: &str, size: u64) -> TreeEntry {
    TreeEntry {
        name: name.to_string(),
        kind: EntryKind::Dir,
        object: Id::from_hex(child_tree_hex).unwrap(),
        size,
        exec: false,
    }
}

/// The shared fixture: app + two writer tokens + a reader + repo "m5a".
struct Fixture {
    t: TestApp,
    writer_a: String,
    writer_b: String,
    reader: String,
    repo_id: i64,
}

fn fixture() -> Fixture {
    fixture_with(|_| {})
}

/// The fixture with a state tweak applied before the `Arc` wrap (the only
/// window where `ServerState` is mutable — e.g. the snapshot-GC off-switch).
fn fixture_with(tweak: impl FnOnce(&mut ServerState)) -> Fixture {
    let t = build_app_with(tweak);
    let writer_a = t.seed_token("writer-a", "writer", None);
    let writer_b = t.seed_token("writer-b", "writer", None);
    let reader = t.seed_token("reader-p", "reader", None);
    let repo_id = t.seed_repo(REPO);
    Fixture {
        t,
        writer_a,
        writer_b,
        reader,
        repo_id,
    }
}

/// PUTs one blob into the snapshot namespace, asserting success.
async fn put_snap_blob(f: &Fixture, token: &str, snap: &str, content: &[u8]) {
    let (status, _, body_bytes) = send_bytes(
        &f.t.app,
        "PUT",
        &format!(
            "/v1/repos/{REPO}/snapshots/{snap}/objects/{}",
            blob_hex(content)
        ),
        token,
        Some(content.to_vec()),
    )
    .await;
    assert!(
        status == StatusCode::CREATED || status == StatusCode::OK,
        "snapshot blob PUT failed: {status}"
    );
    let body: Value = serde_json::from_slice(&body_bytes).expect("PUT body is JSON");
    assert_eq!(
        body["id"],
        json!(blob_hex(content)),
        "PUT echoes the blob id"
    );
}

/// Creates a snapshot, returning the raw response.
async fn create_snapshot(
    f: &Fixture,
    token: &str,
    snap: &str,
    tree_hex: &str,
    trees: Value,
) -> Resp {
    let mut body = json!({ "snap_id": snap, "tree_id": tree_hex, "trees": trees });
    body["ttl_secs"] = json!(604_800u64);
    post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        token,
        &body,
    )
    .await
}

/// (path relative to the data dir → bytes) for every file under `objects/`.
fn objects_state(data_dir: &Path) -> BTreeMap<String, Vec<u8>> {
    let mut out = BTreeMap::new();
    let mut stack = vec![data_dir.join("objects")];
    while let Some(dir) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                stack.push(path);
            } else if let Ok(bytes) = std::fs::read(&path) {
                out.insert(
                    path.strip_prefix(data_dir)
                        .unwrap()
                        .to_string_lossy()
                        .into_owned(),
                    bytes,
                );
            }
        }
    }
    out
}

// ---- tests -----------------------------------------------------------------

/// The plan's named pin: a POST whose tree references a blob absent from
/// `ephemeral/<snap-id>/` is rejected with 422 and the missing ids listed,
/// and ZERO rows are written (snapshot row and tree rows alike).
#[tokio::test]
async fn post_presence_validation_lists_missing_and_writes_zero_rows() {
    let f = fixture();
    let never_uploaded = object_id(Kind::Blob, b"never uploaded bytes");
    let present = b"present bytes".to_vec();
    put_snap_blob(&f, &f.writer_a, &snap_hex(1), &present).await;
    let (tree_hex, trees) = flat_tree(&[
        ("a.txt", never_uploaded, 20, false),
        ("b.txt", object_id(Kind::Blob, &present), 13, false),
    ]);

    let resp = create_snapshot(&f, &f.writer_a, &snap_hex(1), &tree_hex, json!(trees)).await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );
    assert_eq!(code_of(&resp), "validation");
    let missing = resp.body["error"]["details"]["missing_blobs"]
        .as_array()
        .expect("missing_blobs details");
    assert_eq!(missing.len(), 1, "exactly the missing id is listed");
    assert_eq!(missing[0], json!(never_uploaded.to_hex()));
    assert_eq!(
        resp.body["error"]["details"]["missing_blobs_truncated"],
        json!(false),
        "one missing id: the list is not truncated"
    );

    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 0);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM trees"), 0);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM tree_entries"), 0);
    // The dir itself survives (its present blob is still there; GC is not
    // what rejects the POST).
    assert!(f.t.data_dir.join("ephemeral").join(snap_hex(1)).exists());
}

/// Roundtrip server-half: the created snapshot's blobs live under
/// `ephemeral/<snap-id>/` and NEVER under `objects/`; the history CAS is
/// byte-identical across the whole cycle.
#[tokio::test]
async fn snapshot_blobs_live_in_ephemeral_never_objects() {
    let f = fixture();
    // One history blob for contrast.
    let history = b"history bytes".to_vec();
    let (status, _, _) = send_bytes(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/{REPO}/objects/{}", blob_hex(&history)),
        &f.writer_a,
        Some(history.clone()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let before = objects_state(&f.t.data_dir);
    assert_eq!(before.len(), 1, "one history object on disk");

    let alpha = b"alpha bytes".to_vec();
    let beta = b"beta bytes".to_vec();
    let snap = snap_hex(2);
    put_snap_blob(&f, &f.writer_a, &snap, &alpha).await;
    put_snap_blob(&f, &f.writer_a, &snap, &beta).await;
    let (tree_hex, trees) = flat_tree(&[
        ("a.txt", object_id(Kind::Blob, &alpha), 11, false),
        ("b.txt", object_id(Kind::Blob, &beta), 10, false),
    ]);

    let resp = create_snapshot(&f, &f.writer_a, &snap, &tree_hex, json!(trees)).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    assert_eq!(resp.body["id"], json!(snap));
    assert_eq!(
        resp.body["expires_ns"],
        json!(BASE_NS + DEFAULT_TTL_NS),
        "expiry = created + default 7d"
    );

    // The snapshot row and tree rows committed.
    let (row_tree, expires): (Vec<u8>, i64) =
        f.t.db()
            .query_row(
                "SELECT tree_id, expires_ns FROM snapshots WHERE repo_id = ?1 AND id = ?2",
                rusqlite::params![f.repo_id, Id::from_hex(&snap).unwrap().0.as_slice()],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .unwrap();
    assert_eq!(row_tree, Id::from_hex(&tree_hex).unwrap().0.to_vec());
    assert_eq!(expires, (BASE_NS + DEFAULT_TTL_NS) as i64);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM trees"), 1);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM tree_entries"), 2);

    // The blobs are served from the snapshot namespace and live on disk
    // under ephemeral/<snap>/objects/ — the history CAS gained nothing.
    let (status, ctype, bytes) = send_bytes(
        &f.t.app,
        "GET",
        &format!(
            "/v1/repos/{REPO}/snapshots/{snap}/objects/{}",
            blob_hex(&alpha)
        ),
        &f.writer_a,
        None,
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, alpha);
    assert_eq!(
        ctype.unwrap(),
        header::HeaderValue::from_static("application/octet-stream")
    );
    let ephemeral_objects = f.t.data_dir.join("ephemeral").join(&snap).join("objects");
    assert!(ephemeral_objects.join(&blob_hex(&alpha)[..2]).exists());
    let after = objects_state(&f.t.data_dir);
    assert_eq!(after, before, "objects/ untouched by the snapshot");
}

/// The TTL floor: `ttl_secs` below 60 is a 422 before any store work.
#[tokio::test]
async fn ttl_below_minimum_is_422() {
    let f = fixture();
    let content = b"ttl floor".to_vec();
    let snap = snap_hex(3);
    put_snap_blob(&f, &f.writer_a, &snap, &content).await;
    let (tree_hex, trees) = flat_tree(&[("f.txt", object_id(Kind::Blob, &content), 9, false)]);
    let body = json!({
        "snap_id": snap,
        "tree_id": tree_hex,
        "ttl_secs": 59u64,
        "trees": trees,
    });
    let resp = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        &f.writer_a,
        &body,
    )
    .await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 0);
}

/// The both-gates pin: past TTL, metadata GET AND object fetch answer 410
/// `snapshot_expired`; a GC pass removes the dir and the row while the
/// history CAS stays byte-identical.
#[tokio::test]
async fn expiry_enforced_at_both_gates_and_gc_deletes_dir_and_row() {
    let f = fixture_with(|state| {
        // The snapshot POST spawns an opportunistic GC task; this test
        // drives `gc_snapshots` itself with the clock pushed past expiry,
        // and the spawned pass interleaving with it makes `rows_purged`
        // nondeterministic (whichever pass runs second purges the row, the
        // other sees 0). The off-switch exists for exactly this (see
        // `ServerState::snapshot_gc_enabled`): only this test's own GC may
        // run.
        state.snapshot_gc_enabled = false;
    });
    let history = b"history before snapshot".to_vec();
    let (status, _, _) = send_bytes(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/{REPO}/objects/{}", blob_hex(&history)),
        &f.writer_a,
        Some(history.clone()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    let alpha = b"expiring alpha".to_vec();
    let snap = snap_hex(4);
    put_snap_blob(&f, &f.writer_a, &snap, &alpha).await;
    let (tree_hex, trees) = flat_tree(&[("a.txt", object_id(Kind::Blob, &alpha), 14, false)]);
    let resp = create_snapshot(&f, &f.writer_a, &snap, &tree_hex, json!(trees)).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    let before = objects_state(&f.t.data_dir);

    // Advance the injected clock past the snapshot's expiry.
    f.t.clock
        .store(BASE_NS + DEFAULT_TTL_NS + 1, Ordering::Relaxed);

    // Gate 1: metadata.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots/{snap}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(code_of(&resp), "snapshot_expired");
    // Gate 2: the bytes.
    let (status, _, body_bytes) = send_bytes(
        &f.t.app,
        "GET",
        &format!(
            "/v1/repos/{REPO}/snapshots/{snap}/objects/{}",
            blob_hex(&alpha)
        ),
        &f.writer_a,
        None,
    )
    .await;
    assert_eq!(status, StatusCode::GONE);
    let body: Value = serde_json::from_slice(&body_bytes).expect("410 envelope");
    assert_eq!(body["error"]["code"], "snapshot_expired");

    // GC: dir gone, row gone, history byte-identical.
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report.expired, 1, "the expired snapshot was collected");
    assert_eq!(report.rows_purged, 1);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 0);
    assert!(!f.t.data_dir.join("ephemeral").join(&snap).exists());
    assert_eq!(
        objects_state(&f.t.data_dir),
        before,
        "history CAS byte-identical across GC"
    );

    // The collecting pass wrote ONE `snapshot_gc` audit row — actor
    // "system" (no principal drives a background pass), the report counts
    // in details — atomic with the purge and the tree sweep.
    let (actor, details_json): (String, String) =
        f.t.db()
            .query_row(
                "SELECT principal, details_json FROM audit_log WHERE action = 'snapshot_gc'",
                [],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .expect("a snapshot_gc audit row");
    assert_eq!(actor, "system");
    let details: Value = serde_json::from_str(&details_json).unwrap();
    assert_eq!(
        details,
        json!({ "expired": 1, "rowless": 0, "rows_purged": 1, "trees_purged": 1 })
    );

    // A second pass converges (idempotent) — and writes NO audit row: an
    // idle pass must not spam the append-only log.
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report, Default::default());
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM audit_log WHERE action = 'snapshot_gc'"),
        1,
        "the idle second pass wrote no audit row"
    );
}

/// Snapshot trees carry NO `repo_trees` membership (own-principal, TTL'd
/// data): the bare-id tree route 404s them for every principal — owner and
/// second writer alike; they are served only through the owner-gated
/// snapshot routes. The GC pass purges their rows once no live snapshot
/// reaches them, while history trees (which have membership) survive the
/// same pass byte-for-byte.
#[tokio::test]
async fn snapshot_trees_have_no_membership_and_gc_purges_them() {
    // Only this test's own GC may run (the POST spawns an opportunistic
    // pass; see `snapshot_gc_enabled`).
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);

    // History first: a changeset whose derived trees carry junction rows.
    let hb = b"history blob".to_vec();
    let (status, _, _) = send_bytes(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/{}/objects/{}", REPO, blob_hex(&hb)),
        &f.writer_a,
        Some(hb.clone()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let cs = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer_a,
        &json!({
            "parents": [],
            "author": "A <a@naatre.dev>",
            "message": "history",
            "changes": [
                { "path": "h.txt", "op": "add", "blob": blob_hex(&hb), "size": hb.len(), "exec": false }
            ],
        }),
    )
    .await;
    assert_eq!(cs.status, StatusCode::CREATED, "{}", cs.body);
    let hist_root = cs.body["root_tree_id"].as_str().unwrap().to_string();

    // A snapshot as writer_a: one flat root tree over its own blob.
    let alpha = b"alpha snap".to_vec();
    let snap = snap_hex(9);
    put_snap_blob(&f, &f.writer_a, &snap, &alpha).await;
    let (snap_root, trees) = flat_tree(&[("s.txt", object_id(Kind::Blob, &alpha), 10, false)]);
    let resp = create_snapshot(&f, &f.writer_a, &snap, &snap_root, json!(trees)).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // (a) The trees route 404s the snapshot tree for the second writer AND
    // for the owning principal: membership is repo-level and junction-only,
    // and the snapshot wrote none.
    for (who, token) in [("second writer", &f.writer_b), ("owner", &f.writer_a)] {
        let resp = get(
            &f.t.app,
            &format!("/v1/repos/{REPO}/trees/{snap_root}"),
            token,
        )
        .await;
        assert_eq!(
            resp.status,
            StatusCode::NOT_FOUND,
            "{who} via GET /trees: {body}",
            body = resp.body
        );
        assert_eq!(code_of(&resp), "not_found");
    }

    // (c, pre-GC) The history tree is readable — membership gates it in.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/trees/{hist_root}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);

    // Expire the snapshot and run one GC pass.
    f.t.clock
        .store(BASE_NS + DEFAULT_TTL_NS + 1, Ordering::Relaxed);
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report.expired, 1);
    assert_eq!(report.rows_purged, 1);
    assert_eq!(
        report.trees_purged, 1,
        "the snapshot's single root tree row"
    );

    // (b) The snapshot-only tree rows are gone.
    let snap_root_blob = Id::from_hex(&snap_root).unwrap();
    assert_eq!(
        f.t.count(&format!(
            "SELECT COUNT(*) FROM trees WHERE id = x'{}'",
            hex_encode(snap_root_blob.0)
        )),
        0
    );
    assert_eq!(
        f.t.count(&format!(
            "SELECT COUNT(*) FROM tree_entries WHERE tree_id = x'{}'",
            hex_encode(snap_root_blob.0)
        )),
        0
    );

    // (c) The history tree survived the same pass, still readable.
    let hist = Id::from_hex(&hist_root).unwrap();
    assert_eq!(
        f.t.count(&format!(
            "SELECT COUNT(*) FROM trees WHERE id = x'{}'",
            hex_encode(hist.0)
        )),
        1
    );
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/trees/{hist_root}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);

    // A second pass converges (idempotent).
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report, Default::default());
}

/// Two snapshots share a content-addressed subtree; A expires while B
/// stays live. The mark-and-sweep GC must keep every row of B's closure
/// (B restores byte-identically — its metadata and tree walk all answer
/// through the owner-gated snapshot routes) and remove only A-unique
/// rows.
#[tokio::test]
async fn shared_snapshot_subtree_survives_gc_for_live_sibling() {
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);

    // shared/x.txt — the shared subtree (identical canonical bytes in both
    // snapshots → one tree id). aonly/ belongs to A alone, bonly/ to B.
    let xs = b"shared x".to_vec();
    let xb = object_id(Kind::Blob, &xs);
    let (shared, shared_canon) = tree_with_id(vec![file_entry("x.txt", xb, 8, false)]);
    let ys = b"a only".to_vec();
    let yb = object_id(Kind::Blob, &ys);
    let (aonly, aonly_canon) = tree_with_id(vec![file_entry("y.txt", yb, 6, false)]);
    let zs = b"b only".to_vec();
    let zb = object_id(Kind::Blob, &zs);
    let (bonly, bonly_canon) = tree_with_id(vec![file_entry("z.txt", zb, 6, false)]);
    let (root_a, root_a_canon) = tree_with_id(vec![
        dir_entry("aonly", &aonly, 6),
        dir_entry("shared", &shared, 8),
    ]);
    let (root_b, root_b_canon) = tree_with_id(vec![
        dir_entry("bonly", &bonly, 6),
        dir_entry("shared", &shared, 8),
    ]);

    let snap_a = snap_hex(11);
    let snap_b = snap_hex(12);
    for (token, snap, blob) in [
        (&f.writer_a, &snap_a, &xs),
        (&f.writer_a, &snap_a, &ys),
        (&f.writer_b, &snap_b, &xs),
        (&f.writer_b, &snap_b, &zs),
    ] {
        put_snap_blob(&f, token, snap, blob).await;
    }
    let resp = post_snapshot(
        &f,
        &f.writer_a,
        &snap_a,
        &root_a,
        [root_a_canon, shared_canon.clone(), aonly_canon].concat(),
        60,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    let resp = post_snapshot(
        &f,
        &f.writer_b,
        &snap_b,
        &root_b,
        [root_b_canon, shared_canon, bonly_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // Expire A (60s), keep B (3600s), run one pass.
    f.t.clock.store(BASE_NS + 61_000_000_000, Ordering::Relaxed);
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report.expired, 1, "A expired");
    assert_eq!(report.rows_purged, 1);
    assert_eq!(
        report.trees_purged, 2,
        "root_a and aonly swept, shared kept"
    );

    // A-unique rows are gone; the shared subtree and B's own rows remain.
    for (tree, want) in [
        (&root_a, 0),
        (&aonly, 0),
        (&shared, 1),
        (&bonly, 1),
        (&root_b, 1),
    ] {
        let id = Id::from_hex(tree).unwrap();
        assert_eq!(
            f.t.count(&format!(
                "SELECT COUNT(*) FROM trees WHERE id = x'{}'",
                hex_encode(id.0)
            )),
            want,
            "tree {tree} row count"
        );
    }

    // B restores byte-identically: its metadata and EVERY tree route its
    // restore walk needs answer 200 for its owner.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots/{snap_b}"),
        &f.writer_b,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    for tree in [&root_b, shared.as_str(), bonly.as_str()] {
        let resp = get(
            &f.t.app,
            &format!("/v1/repos/{REPO}/snapshots/{snap_b}/trees/{tree}"),
            &f.writer_b,
        )
        .await;
        assert_eq!(
            resp.status,
            StatusCode::OK,
            "B's tree walk needs {tree}: {body}",
            body = resp.body
        );
    }
    // A is gone: the pass purged its row, so its metadata 404s (the 410
    // `snapshot_expired` gate while the row still exists is pinned by the
    // expiry test above).
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots/{snap_a}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);

    // A second pass converges (idempotent).
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report, Default::default());
}

/// A history tree whose content equals a snapshot subtree carries a
/// `repo_trees` junction row, so it stays marked — and readable through
/// the repo tree route — even after every snapshot that shared the id has
/// expired and been swept.
#[tokio::test]
async fn history_tree_identical_to_snapshot_subtree_survives_purge() {
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);

    // History first: a changeset adding shared/x.txt derives the `shared`
    // tree (and writes its junction row).
    let xs = b"shared x".to_vec();
    let xb = object_id(Kind::Blob, &xs);
    let (shared, shared_canon) = tree_with_id(vec![file_entry("x.txt", xb, 8, false)]);
    let (status, _, _) = send_bytes(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/{REPO}/objects/{}", blob_hex(&xs)),
        &f.writer_a,
        Some(xs.clone()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let cs = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer_a,
        &json!({
            "parents": [],
            "author": "A <a@naatre.dev>",
            "message": "history",
            "changes": [
                { "path": "shared/x.txt", "op": "add", "blob": blob_hex(&xs), "size": 8, "exec": false }
            ],
        }),
    )
    .await;
    assert_eq!(cs.status, StatusCode::CREATED, "{}", cs.body);

    // A snapshot whose root IS that same content-addressed tree (its
    // closure is the root alone: one flat file entry).
    let snap = snap_hex(13);
    put_snap_blob(&f, &f.writer_a, &snap, &xs).await;
    let resp = create_snapshot(&f, &f.writer_a, &snap, &shared, json!(shared_canon)).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // Expire the snapshot; the pass sweeps its rows but must keep the
    // junction-marked tree.
    f.t.clock
        .store(BASE_NS + DEFAULT_TTL_NS + 1, Ordering::Relaxed);
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report.expired, 1);
    assert_eq!(report.trees_purged, 0, "the shared tree is junction-marked");

    let id = Id::from_hex(&shared).unwrap();
    assert_eq!(
        f.t.count(&format!(
            "SELECT COUNT(*) FROM trees WHERE id = x'{}'",
            hex_encode(id.0)
        )),
        1
    );
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/trees/{shared}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
}

/// The sweep's safety net, independent of write-path discipline: a history
/// subtree whose junction row is HAND-DELETED still survives GC, because a
/// RETAINED tree (its parent: junctioned and a changeset root) has a
/// tree_entries row pointing at it. Junction completeness is the cheap
/// first filter; the retention lookups are what protects live history.
#[tokio::test]
async fn gc_spares_history_trees_whose_junction_row_was_lost() {
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);

    // History with a nested subtree: root and `shared`, both junctioned.
    let xs = b"shared x".to_vec();
    let xb = object_id(Kind::Blob, &xs);
    let (shared, _) = tree_with_id(vec![file_entry("x.txt", xb, 8, false)]);
    let (status, _, _) = send_bytes(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/{REPO}/objects/{}", blob_hex(&xs)),
        &f.writer_a,
        Some(xs.clone()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let cs = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer_a,
        &json!({
            "parents": [],
            "author": "A <a@naatre.dev>",
            "message": "history",
            "changes": [
                { "path": "shared/x.txt", "op": "add", "blob": blob_hex(&xs), "size": 8, "exec": false }
            ],
        }),
    )
    .await;
    assert_eq!(cs.status, StatusCode::CREATED, "{}", cs.body);
    let root = cs.body["root_tree_id"].as_str().unwrap().to_string();

    // Hand-delete the CHILD subtree's junction row (simulating a write path
    // that forgot it). The row now looks exactly like a snapshot-created
    // tree: zero junction.
    let shared_id = Id::from_hex(&shared).unwrap();
    let deleted =
        f.t.db()
            .execute(
                "DELETE FROM repo_trees WHERE tree_id = ?1",
                [shared_id.0.as_slice()],
            )
            .unwrap();
    assert_eq!(deleted, 1, "the subtree had a junction row to lose");

    // GC runs (the junction-less row trips the idle probe) — and spares it.
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(
        report.trees_purged, 0,
        "the retained parent still points at the de-junctioned subtree"
    );
    for tree in [&root, &shared] {
        let id = Id::from_hex(tree).unwrap();
        assert_eq!(
            f.t.count(&format!(
                "SELECT COUNT(*) FROM trees WHERE id = x'{}'",
                hex_encode(id.0)
            )),
            1,
            "history tree {tree} survives the sweep"
        );
        assert_eq!(
            f.t.count(&format!(
                "SELECT COUNT(*) FROM tree_entries WHERE tree_id = x'{}'",
                hex_encode(id.0)
            )),
            1,
            "history tree {tree} keeps its entries"
        );
    }

    // A second pass converges (idempotent — the net still catches it).
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report, Default::default());
}

/// Depth-three safety: a JUNCTION-LESS CHAIN (S1 → S2 → S3) under a
/// retained parent survives in full. Depth-one protection alone would
/// orphan S2/S3 (their parents are not "retained"); protection recurses
/// through candidate children.
#[tokio::test]
async fn gc_spares_a_deeper_junctionless_chain() {
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);

    // History: changeset adding deep/a/b/c.txt derives root → deep →
    // deep/a → deep/a/b (three junction-less levels once the junctions are
    // hand-deleted below).
    let cs_bytes = b"c leaf".to_vec();
    let cb = object_id(Kind::Blob, &cs_bytes);
    let (s3, s3_canon) = tree_with_id(vec![file_entry("c.txt", cb, 6, false)]);
    let (s2, s2_canon) = tree_with_id(vec![dir_entry("b", &s3, 6)]);
    let (s1, s1_canon) = tree_with_id(vec![dir_entry("a", &s2, 6)]);
    let (root, _) = tree_with_id(vec![dir_entry("deep", &s1, 6)]);
    let (status, _, _) = send_bytes(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/{REPO}/objects/{}", blob_hex(&cs_bytes)),
        &f.writer_a,
        Some(cs_bytes.clone()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let cs = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer_a,
        &json!({
            "parents": [],
            "author": "A <a@naatre.dev>",
            "message": "history",
            "changes": [
                { "path": "deep/a/b/c.txt", "op": "add", "blob": blob_hex(&cs_bytes), "size": 6, "exec": false }
            ],
        }),
    )
    .await;
    assert_eq!(cs.status, StatusCode::CREATED, "{}", cs.body);
    let root_hex = cs.body["root_tree_id"].as_str().unwrap().to_string();
    assert_eq!(
        root_hex, root,
        "the derived chain matches the hand-built one"
    );

    // Hand-delete the junction rows for ALL THREE chain levels (simulating
    // a write path that forgot a whole subtree). Root stays junctioned.
    for tree in [&s1, &s2, &s3] {
        let id = Id::from_hex(tree).unwrap();
        let deleted =
            f.t.db()
                .execute(
                    "DELETE FROM repo_trees WHERE tree_id = ?1",
                    [id.0.as_slice()],
                )
                .unwrap();
        assert_eq!(deleted, 1, "{tree} had a junction row to lose");
    }

    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(
        report.trees_purged, 0,
        "the retained root protects the whole 3-deep junction-less chain"
    );
    for tree in [&root, &s1, &s2, &s3] {
        let id = Id::from_hex(tree).unwrap();
        assert_eq!(
            f.t.count(&format!(
                "SELECT COUNT(*) FROM trees WHERE id = x'{}'",
                hex_encode(id.0)
            )),
            1,
            "chain tree {tree} survives"
        );
    }
    let _ = (s1_canon, s2_canon, s3_canon);
}

/// Inverse of the safety net: a junction-less chain whose ONLY reference
/// is an EXPIRED snapshot sweeps fully — the expired owner protects
/// nothing, children included.
#[tokio::test]
async fn gc_sweeps_an_expired_snapshot_chain_fully() {
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);

    let fs = b"chain file".to_vec();
    let fb = object_id(Kind::Blob, &fs);
    let (s2, s2_canon) = tree_with_id(vec![file_entry("f.txt", fb, 10, false)]);
    let (root_s, root_s_canon) = tree_with_id(vec![dir_entry("sdir", &s2, 10)]);

    let snap = snap_hex(14);
    put_snap_blob(&f, &f.writer_a, &snap, &fs).await;
    let resp = post_snapshot(
        &f,
        &f.writer_a,
        &snap,
        &root_s,
        [root_s_canon, s2_canon].concat(),
        60,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    f.t.clock.store(BASE_NS + 61_000_000_000, Ordering::Relaxed);
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report.expired, 1);
    assert_eq!(
        report.trees_purged, 2,
        "the expired chain's root AND child sweep"
    );
    for tree in [&root_s, &s2] {
        let id = Id::from_hex(tree).unwrap();
        assert_eq!(
            f.t.count(&format!(
                "SELECT COUNT(*) FROM trees WHERE id = x'{}'",
                hex_encode(id.0)
            )),
            0,
            "expired-chain tree {tree} is gone"
        );
    }

    // A second pass converges (idempotent).
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report, Default::default());
}

/// Lowercase-hex SQL blob literal (`x'…'`) for id lookups in raw SQL.
fn hex_encode(bytes: [u8; 32]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// POSTs a snapshot with an explicit TTL (the shared helper pins the
/// default).
async fn post_snapshot(
    f: &Fixture,
    token: &str,
    snap: &str,
    tree: &str,
    canons: Vec<String>,
    ttl_secs: u64,
) -> Resp {
    post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        token,
        &json!({
            "snap_id": snap,
            "tree_id": tree,
            "trees": canons,
            "ttl_secs": ttl_secs,
        }),
    )
    .await
}

/// The rowless-sweep pin: blobs uploaded straight into
/// `ephemeral/<fake-id>/` with no snapshot POST (the crash-between-upload-
/// and-row shape) are swept once the dir ages past the max TTL, via the
/// threshold parameter — the injected clock pushed past `now + max TTL`
/// makes the fresh dir older than `now - max TTL`.
#[tokio::test]
async fn rowless_dir_older_than_max_ttl_is_swept() {
    let f = fixture();
    let orphan = b"uploaded but never POSTed".to_vec();
    let fake = snap_hex(5);
    f.t.state
        .ephemeral
        .put_blob(&fake, &orphan)
        .expect("direct ephemeral put");
    assert!(f.t.data_dir.join("ephemeral").join(&fake).exists());
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM snapshots"),
        0,
        "no row: the crash left the dir rowless"
    );

    // Push the clock beyond the dir's (real) mtime by more than the max
    // TTL; the sweep threshold `now - max TTL` then lands after the mtime
    // and the rowless dir counts as garbage.
    let real_now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos() as u64;
    f.t.clock.store(
        real_now + SNAPSHOT_MAX_TTL_NS + 60_000_000_000,
        Ordering::Relaxed,
    );

    let before = objects_state(&f.t.data_dir);
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report.rowless, 1, "the rowless dir was swept");
    assert!(!f.t.data_dir.join("ephemeral").join(&fake).exists());
    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 0);
    assert_eq!(
        objects_state(&f.t.data_dir),
        before,
        "history CAS untouched by the sweep"
    );
}

/// Own-principal rule: another writer's snapshot id is 404 on both the
/// metadata and the bytes fetches, and a foreign re-POST of the id is
/// rejected.
#[tokio::test]
async fn another_principals_snapshot_is_404() {
    let f = fixture();
    let alpha = b"only a's".to_vec();
    let snap = snap_hex(6);
    put_snap_blob(&f, &f.writer_a, &snap, &alpha).await;
    let (tree_hex, trees) = flat_tree(&[("a.txt", object_id(Kind::Blob, &alpha), 8, false)]);
    let resp = create_snapshot(&f, &f.writer_a, &snap, &tree_hex, json!(trees)).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // Writer B: 404 on metadata, 404 on bytes — indistinguishable from a
    // nonexistent id.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots/{snap}"),
        &f.writer_b,
    )
    .await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND, "{}", resp.body);
    let (status, _, _) = send_bytes(
        &f.t.app,
        "GET",
        &format!(
            "/v1/repos/{REPO}/snapshots/{snap}/objects/{}",
            blob_hex(&alpha)
        ),
        &f.writer_b,
        None,
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    // B re-POSTing the same id (different tree) is rejected, not hijacked.
    let other = b"b's content".to_vec();
    put_snap_blob(&f, &f.writer_b, &snap, &other).await;
    let (other_tree, other_trees) =
        flat_tree(&[("b.txt", object_id(Kind::Blob, &other), 11, false)]);
    let resp = create_snapshot(&f, &f.writer_b, &snap, &other_tree, json!(other_trees)).await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );

    // A's own metadata still works, and an identical re-POST is idempotent
    // 200 (same id, same tree, same principal).
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots/{snap}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["tree_id"], json!(tree_hex));
    let resp = create_snapshot(&f, &f.writer_a, &snap, &tree_hex, json!(trees)).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
}

/// Matrix cell: snapshots POST is writer-class — a reader is 403.
#[tokio::test]
async fn reader_cannot_create_snapshots() {
    let f = fixture();
    let (tree_hex, trees) = flat_tree(&[("a.txt", object_id(Kind::Blob, b"x"), 1, false)]);
    let body = json!({
        "snap_id": snap_hex(7),
        "tree_id": tree_hex,
        "trees": trees,
    });
    let resp = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        &f.reader,
        &body,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN);
    assert_eq!(code_of(&resp), "forbidden");
}

// ---- fix3a: re-POST idempotency (recorded expiry, not recomputed) ----------

/// An identical re-POST answers 200 with the RECORDED row expiry — the
/// injectable clock advances between the POSTs, so a recomputed expiry
/// would differ. A re-POST with a different ttl_secs is 422 with the
/// recorded expiry in details, and the row is untouched by both.
#[tokio::test]
async fn repost_returns_recorded_expiry_and_rejects_different_ttl() {
    // The POST spawns an opportunistic GC pass that can hold the write slot
    // while this test's rapid re-POSTs arrive; GC is not under test here.
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);
    let alpha = b"repost alpha".to_vec();
    let snap = snap_hex(21);
    put_snap_blob(&f, &f.writer_a, &snap, &alpha).await;
    let (tree_hex, trees) = flat_tree(&[("a.txt", object_id(Kind::Blob, &alpha), 12, false)]);
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, trees.clone(), 3600).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    let recorded = resp.body["expires_ns"].as_u64().unwrap();
    assert_eq!(recorded, BASE_NS + 3600 * 1_000_000_000);

    // The clock moves between the POSTs: only the row value can come back.
    f.t.clock.store(BASE_NS + 60_000_000_000, Ordering::Relaxed);

    // Same ttl → 200 with the RECORDED expiry.
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, trees.clone(), 3600).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["expires_ns"], json!(recorded));

    // Different ttl → 422, the recorded expiry in details.
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, trees.clone(), 7200).await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );
    assert_eq!(code_of(&resp), "validation");
    assert_eq!(
        resp.body["error"]["details"]["recorded_expires_ns"],
        json!(recorded)
    );

    // The row still carries exactly the recorded expiry.
    let row_expires: i64 =
        f.t.db()
            .query_row(
                "SELECT expires_ns FROM snapshots WHERE repo_id = ?1 AND id = ?2",
                rusqlite::params![f.repo_id, Id::from_hex(&snap).unwrap().0.as_slice()],
                |r| r.get(0),
            )
            .unwrap();
    assert_eq!(row_expires, recorded as i64);
}

/// Re-POSTing an EXPIRED snapshot's id is 410 `snapshot_expired` on every
/// ttl — never a resurrect-200 with a fresh expiry — and the expired row
/// stays (GC owns its removal, not the POST).
#[tokio::test]
async fn repost_of_expired_snapshot_is_410() {
    // Same reason as the sibling test: the opportunistic pass must not
    // contend with the rapid re-POSTs.
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);
    let alpha = b"expiring repost".to_vec();
    let snap = snap_hex(22);
    put_snap_blob(&f, &f.writer_a, &snap, &alpha).await;
    let (tree_hex, trees) = flat_tree(&[("a.txt", object_id(Kind::Blob, &alpha), 15, false)]);
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, trees.clone(), 60).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    f.t.clock.store(BASE_NS + 61_000_000_000, Ordering::Relaxed);

    // Identical re-POST → 410, not 200.
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, trees.clone(), 60).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(code_of(&resp), "snapshot_expired");
    // A different ttl on the expired id → 410 too (expiry outranks the ttl
    // check — nothing about a dead snapshot is negotiable).
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, trees, 3600).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(code_of(&resp), "snapshot_expired");

    // The row stays expired — unpurged (no GC ran) and un-resurrected.
    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 1);
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots/{snap}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE);
    assert_eq!(code_of(&resp), "snapshot_expired");
}

/// An id collision is 422 naming the conflict — never a resurrect, a
/// hijack, or a second row. Both dimensions pin independently: the same
/// principal re-POSTing the id over a DIFFERENT tree, and a different
/// principal re-POSTing the id over the SAME tree, both answer 422
/// "already used by a different snapshot".
#[tokio::test]
async fn snapshot_id_collision_is_422_for_tree_and_principal() {
    // Rapid POSTs; the opportunistic GC pass is not under test here.
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);
    let alpha = b"collision alpha".to_vec();
    let beta = b"collision beta".to_vec();
    let snap = snap_hex(33);
    put_snap_blob(&f, &f.writer_a, &snap, &alpha).await;
    put_snap_blob(&f, &f.writer_b, &snap, &beta).await;
    let (tree_a, trees_a) = flat_tree(&[("a.txt", object_id(Kind::Blob, &alpha), 15, false)]);
    let (tree_b, trees_b) = flat_tree(&[("b.txt", object_id(Kind::Blob, &beta), 15, false)]);
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_a, trees_a.clone(), 3600).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // Same principal, same id, different tree → 422 with the collision
    // message (never a silent tree swap).
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_b, trees_b.clone(), 3600).await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );
    assert!(
        resp.body["error"]["message"]
            .as_str()
            .unwrap()
            .contains("already used by a different snapshot"),
        "{}",
        resp.body
    );

    // Different principal, same id, same tree → 422 with the collision
    // message (never a hijack through the identical-tree shortcut).
    let resp = post_snapshot(&f, &f.writer_b, &snap, &tree_a, trees_a.clone(), 3600).await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );
    assert!(
        resp.body["error"]["message"]
            .as_str()
            .unwrap()
            .contains("already used by a different snapshot"),
        "{}",
        resp.body
    );

    // The original row stands untouched, owned by its creator.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots/{snap}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["tree_id"], json!(tree_a));
}

// ---- fix3a: snapshot tree route closure membership -------------------------

/// The snapshot tree route serves only the snapshot's own subtree: a tree
/// id that exists on the server but outside the snapshot's closure (here:
/// a second repo's changeset root) is 404, as the route doc promises — and
/// the expiry gate still outranks the closure check (expired + closure
/// member → 410, not 404).
#[tokio::test]
async fn snapshot_tree_route_serves_only_closure_members() {
    // The POST spawns an opportunistic GC pass that, once the clock passes
    // expiry below, could purge the row to a 404 before the final 410
    // assert; GC is not under test here (same off-switch as the re-POST
    // tests).
    let f = fixture_with(|state| state.snapshot_gc_enabled = false);

    // A foreign history tree: a second repo's changeset puts a tree row in
    // the global `trees` table, outside this snapshot's closure.
    f.t.seed_repo("m5a-foreign");
    let foreign_blob = b"foreign tree blob".to_vec();
    let (status, _, _) = send_bytes(
        &f.t.app,
        "PUT",
        &format!("/v1/repos/m5a-foreign/objects/{}", blob_hex(&foreign_blob)),
        &f.writer_a,
        Some(foreign_blob.clone()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let cs = post_json(
        &f.t.app,
        "/v1/repos/m5a-foreign/changesets",
        &f.writer_a,
        &json!({
            "parents": [],
            "author": "A <a@naatre.dev>",
            "message": "foreign history",
            "changes": [
                { "path": "f.txt", "op": "add", "blob": blob_hex(&foreign_blob), "size": 17, "exec": false }
            ],
        }),
    )
    .await;
    assert_eq!(cs.status, StatusCode::CREATED, "{}", cs.body);
    let foreign_tree = cs.body["root_tree_id"].as_str().unwrap().to_string();

    // Own snapshot: root → dir "d" → sub.
    let sub_bytes = b"sub blob".to_vec();
    let snap = snap_hex(23);
    put_snap_blob(&f, &f.writer_a, &snap, &sub_bytes).await;
    let (sub_hex, sub_canon) = tree_with_id(vec![file_entry(
        "u.txt",
        object_id(Kind::Blob, &sub_bytes),
        8,
        false,
    )]);
    let (root_hex, root_canon) = tree_with_id(vec![dir_entry("d", &sub_hex, 8)]);
    let resp = post_snapshot(
        &f,
        &f.writer_a,
        &snap,
        &root_hex,
        [root_canon, sub_canon].concat(),
        60,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // Closure members serve: the root (a closure contains its root) and
    // the nested subtree.
    for tree in [&root_hex, &sub_hex] {
        let resp = get(
            &f.t.app,
            &format!("/v1/repos/{REPO}/snapshots/{snap}/trees/{tree}"),
            &f.writer_a,
        )
        .await;
        assert_eq!(
            resp.status,
            StatusCode::OK,
            "closure member {tree}: {body}",
            body = resp.body
        );
    }

    // The foreign tree id exists in `trees` but is not in the closure →
    // 404; so is a well-formed id with no row at all.
    for tree in [&foreign_tree, &snap_hex(99)] {
        let resp = get(
            &f.t.app,
            &format!("/v1/repos/{REPO}/snapshots/{snap}/trees/{tree}"),
            &f.writer_a,
        )
        .await;
        assert_eq!(
            resp.status,
            StatusCode::NOT_FOUND,
            "non-member {tree}: {body}",
            body = resp.body
        );
        assert_eq!(code_of(&resp), "not_found");
    }

    // Expire: the closure-member tree GET is 410 (the expiry gate fires
    // first), never a 404 and never a serve.
    f.t.clock.store(BASE_NS + 61_000_000_000, Ordering::Relaxed);
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots/{snap}/trees/{sub_hex}"),
        &f.writer_a,
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(code_of(&resp), "snapshot_expired");
}

// ---- fix3a: path-scope enforcement on the POST ------------------------------

/// The plan's writes row ("path-prefix scope gates these — segment-aligned")
/// covers snapshots POST: a writer scoped to docs/ posting a closure that
/// contains src/x is 403 with the first offending path in details; the same
/// token posting a docs/-only closure succeeds. The blobs PUT stayed
/// repo-level (scope does not gate it — a blob has no path yet).
#[tokio::test]
async fn snapshot_post_enforces_path_scope() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("docs"));

    let srcb = b"src bytes".to_vec();
    let docb = b"doc bytes".to_vec();
    let snap = snap_hex(24);
    put_snap_blob(&f, &scoped, &snap, &srcb).await;
    put_snap_blob(&f, &scoped, &snap, &docb).await;
    let (src_tree, src_canon) = tree_with_id(vec![file_entry(
        "x",
        object_id(Kind::Blob, &srcb),
        9,
        false,
    )]);
    let (docs_tree, docs_canon) = tree_with_id(vec![file_entry(
        "a.txt",
        object_id(Kind::Blob, &docb),
        9,
        false,
    )]);
    let (root, root_canon) = tree_with_id(vec![
        dir_entry("docs", &docs_tree, 9),
        dir_entry("src", &src_tree, 9),
    ]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root,
        [root_canon, src_canon, docs_canon.clone()].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(code_of(&resp), "forbidden");
    assert_eq!(resp.body["error"]["details"]["path"], json!("src/x"));
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM snapshots"),
        0,
        "the rejected POST wrote no rows"
    );

    // Positive half: same scoped token, closure fully inside docs/.
    let (docs_root, docs_root_canon) = tree_with_id(vec![dir_entry("docs", &docs_tree, 9)]);
    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &docs_root,
        [docs_root_canon, docs_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
}

// ---- fix4: the scope walk vs shared subtrees --------------------------------

/// The shared-subtree bypass, closed: one subtree id mounted at app/ AND
/// docs/ contributes its files at BOTH mounts — a docs/-scoped token gets
/// 403 naming the app/... path, not a 201 that silently swallowed the
/// second mount's paths.
#[tokio::test]
async fn shared_subtree_out_of_scope_is_403() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("docs"));

    let shared = b"shared subtree blob".to_vec();
    let snap = snap_hex(34);
    put_snap_blob(&f, &scoped, &snap, &shared).await;
    let (sub_hex, sub_canon) = tree_with_id(vec![file_entry(
        "t.txt",
        object_id(Kind::Blob, &shared),
        18,
        false,
    )]);
    // Same tree id under two names — the shape the tree-id seen-set used
    // to dedupe away.
    let (root_hex, root_canon) = tree_with_id(vec![
        dir_entry("app", &sub_hex, 18),
        dir_entry("docs", &sub_hex, 18),
    ]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root_hex,
        [root_canon, sub_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(code_of(&resp), "forbidden");
    assert_eq!(resp.body["error"]["details"]["path"], json!("app/t.txt"));
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM snapshots"),
        0,
        "the rejected POST wrote no rows"
    );
}

/// The in-scope mirror of the bypass shape: the same double mount under an
/// unscoped token is a normal 201 — sharing subtrees stays legal.
#[tokio::test]
async fn shared_subtree_in_scope_is_created() {
    let f = fixture();
    let shared = b"shared subtree blob".to_vec();
    let snap = snap_hex(35);
    put_snap_blob(&f, &f.writer_a, &snap, &shared).await;
    let (sub_hex, sub_canon) = tree_with_id(vec![file_entry(
        "t.txt",
        object_id(Kind::Blob, &shared),
        18,
        false,
    )]);
    let (root_hex, root_canon) = tree_with_id(vec![
        dir_entry("app", &sub_hex, 18),
        dir_entry("docs", &sub_hex, 18),
    ]);

    let resp = post_snapshot(
        &f,
        &f.writer_a,
        &snap,
        &root_hex,
        [root_canon, sub_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
}

/// A straddling subtree — mounted above the scope, holding one file under
/// the scope and one outside — is 403 naming the outside file (the
/// per-file checks, not a blanket dir refusal).
#[tokio::test]
async fn straddling_subtree_names_outside_file() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("docs/sub"));

    let inside = b"inside the scope".to_vec();
    let outside = b"outside the scope".to_vec();
    let snap = snap_hex(36);
    put_snap_blob(&f, &scoped, &snap, &inside).await;
    put_snap_blob(&f, &scoped, &snap, &outside).await;
    let (sub_hex, sub_canon) = tree_with_id(vec![file_entry(
        "in.txt",
        object_id(Kind::Blob, &inside),
        16,
        false,
    )]);
    // T straddles at docs/: docs/sub/in.txt is inside, docs/out.txt is not.
    let (mid_hex, mid_canon) = tree_with_id(vec![
        file_entry("out.txt", object_id(Kind::Blob, &outside), 17, false),
        dir_entry("sub", &sub_hex, 16),
    ]);
    let (root_hex, root_canon) = tree_with_id(vec![dir_entry("docs", &mid_hex, 17)]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root_hex,
        [root_canon, mid_canon, sub_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(resp.body["error"]["details"]["path"], json!("docs/out.txt"));
}

/// The exponential shape: `depth` diamond levels (each tree points two
/// names, a and b, at the next) over one leaf file f.txt — 2^depth file
/// paths across depth+1 distinct trees. Returns (root id hex, canonical
/// b64 list).
fn diamond_chain(depth: usize) -> (String, Vec<String>) {
    let leaf = b"deep chain leaf".to_vec();
    let (mut tree_hex, mut canons) = tree_with_id(vec![file_entry(
        "f.txt",
        object_id(Kind::Blob, &leaf),
        15,
        false,
    )]);
    for _ in 0..depth {
        let (next_hex, next_canon) = tree_with_id(vec![
            dir_entry("a", &tree_hex, 15),
            dir_entry("b", &tree_hex, 15),
        ]);
        canons = [canons, next_canon].concat();
        tree_hex = next_hex;
    }
    (tree_hex, canons)
}

/// The exponential shape, functionally pinned: ~20 trees each pointing two
/// names at the next span 2^20 file paths, and the POST completes — the
/// scope walk classifies instead of enumerating, so the bound is linear in
/// distinct trees, not occurrences.
#[tokio::test]
async fn deep_shared_subtree_chain_stays_linear() {
    let f = fixture();
    let leaf = b"deep chain leaf".to_vec();
    let snap = snap_hex(37);
    put_snap_blob(&f, &f.writer_a, &snap, &leaf).await;
    // 40 levels: an occurrence-enumerating walk spans 2^40 paths and cannot
    // finish — the wall-clock bound turns that regression into a test failure
    // instead of a hang. 20 levels would NOT discriminate (2^20 ~ 1M finishes
    // in seconds).
    let (tree_hex, canons) = diamond_chain(40);
    let started = std::time::Instant::now();

    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, canons, 3600).await;
    assert!(
        started.elapsed() < std::time::Duration::from_secs(2),
        "scope walk took {:?} — regressed to occurrence enumeration?",
        started.elapsed()
    );
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
}

/// The same 2^20 chain under a scope: the a/ chain is covered (skipped),
/// the b/ branch is disjoint — the walk still completes in linear time
/// (one memoized scan over the 21 trees) and names the first file on the
/// b/ branch: b/ followed by the a-chain down to the leaf.
#[tokio::test]
async fn deep_shared_subtree_chain_under_scope_is_403() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("a"));
    let leaf = b"deep chain leaf".to_vec();
    let snap = snap_hex(44);
    put_snap_blob(&f, &scoped, &snap, &leaf).await;
    let (tree_hex, canons) = diamond_chain(20);

    let resp = post_snapshot(&f, &scoped, &snap, &tree_hex, canons, 3600).await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(
        resp.body["error"]["details"]["path"],
        json!(format!("b/{}f.txt", "a/".repeat(19)))
    );
}

/// The advisor's cross-occurrence shape, multi-segment scope: scope a/b;
/// U = {dir b -> V}, V = {file f}; U mounted at a/ (straddling — its b
/// child lands on a/b/, covered and skipped) and x/ (disjoint — every
/// file below is outside). The 403 names the DISJOINT mount's path.
#[tokio::test]
async fn multisegment_dual_mount_disjoint_occurrence_is_403() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("a/b"));
    let fblob = b"dual mount f".to_vec();
    let snap = snap_hex(38);
    put_snap_blob(&f, &scoped, &snap, &fblob).await;
    let (v_hex, v_canon) = tree_with_id(vec![file_entry(
        "f",
        object_id(Kind::Blob, &fblob),
        12,
        false,
    )]);
    let (u_hex, u_canon) = tree_with_id(vec![dir_entry("b", &v_hex, 12)]);
    let (root_hex, root_canon) =
        tree_with_id(vec![dir_entry("a", &u_hex, 12), dir_entry("x", &u_hex, 12)]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root_hex,
        [root_canon, u_canon, v_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(code_of(&resp), "forbidden");
    assert_eq!(resp.body["error"]["details"]["path"], json!("x/b/f"));
}

/// The memo-hit direction, both ways: the same dual mount with the scope
/// mirrored to x/b, so the a/ mount is now the disjoint one and the
/// verdict must name a/b/f. Between the two dual-mount tests the memo is
/// populated from the second-sorted entry (scope a/b names x/b/f) and
/// from the first-sorted entry here — order must not matter. (The
/// literal [x -> U, a -> U] byte order is not expressible: `tree::encode`
/// refuses unsorted entries (`EncodeUnsorted`), and pinning behavior over
/// hand-rolled non-canonical bytes would pin nothing the contract
/// admits.)
#[tokio::test]
async fn dual_mount_verdict_is_order_independent() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("x/b"));
    let fblob = b"dual mount f".to_vec();
    let snap = snap_hex(39);
    put_snap_blob(&f, &scoped, &snap, &fblob).await;
    let (v_hex, v_canon) = tree_with_id(vec![file_entry(
        "f",
        object_id(Kind::Blob, &fblob),
        12,
        false,
    )]);
    let (u_hex, u_canon) = tree_with_id(vec![dir_entry("b", &v_hex, 12)]);
    let (root_hex, root_canon) =
        tree_with_id(vec![dir_entry("a", &u_hex, 12), dir_entry("x", &u_hex, 12)]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root_hex,
        [root_canon, u_canon, v_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(resp.body["error"]["details"]["path"], json!("a/b/f"));
}

/// The closure_first_file regression pin: the disjoint subtree's only
/// file sits TWO dirs deep (src/x/f), so the scan must descend AND fold
/// the child's verdict back into the parked parent frame — a consuming
/// resume would memoize a wrong None and 201 an out-of-scope tree.
#[tokio::test]
async fn nested_file_in_disjoint_subtree_is_403() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("docs"));
    let fblob = b"nested f".to_vec();
    let snap = snap_hex(40);
    put_snap_blob(&f, &scoped, &snap, &fblob).await;
    let (x_hex, x_canon) = tree_with_id(vec![file_entry(
        "f",
        object_id(Kind::Blob, &fblob),
        8,
        false,
    )]);
    let (s_hex, s_canon) = tree_with_id(vec![dir_entry("x", &x_hex, 8)]);
    let (root_hex, root_canon) = tree_with_id(vec![dir_entry("src", &s_hex, 8)]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root_hex,
        [root_canon, s_canon, x_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(resp.body["error"]["details"]["path"], json!("src/x/f"));
}

/// Memo poisoning: the SAME tree (nested file only, S = {dir x -> X})
/// mounted under two disjoint names. Whichever occurrence is scanned
/// first computes the verdict and the second must reuse it — a poisoned
/// wrong None on the first mount would silently clear the second. Both
/// mounts are outside docs/; the first (alt/, sorted before src/) is
/// named.
#[tokio::test]
async fn dual_disjoint_mounts_share_one_unpoisoned_verdict() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("docs"));
    let fblob = b"poison f".to_vec();
    let snap = snap_hex(41);
    put_snap_blob(&f, &scoped, &snap, &fblob).await;
    let (x_hex, x_canon) = tree_with_id(vec![file_entry(
        "f",
        object_id(Kind::Blob, &fblob),
        8,
        false,
    )]);
    let (s_hex, s_canon) = tree_with_id(vec![dir_entry("x", &x_hex, 8)]);
    let (root_hex, root_canon) = tree_with_id(vec![
        dir_entry("alt", &s_hex, 8),
        dir_entry("src", &s_hex, 8),
    ]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root_hex,
        [root_canon, s_canon, x_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(resp.body["error"]["details"]["path"], json!("alt/x/f"));
}

/// Mixed tree under a disjoint mount, dir FIRST and a direct file later:
/// the verdict must fold the dir child's nested file (DFS pre-order:
/// src/a_dir/g), not fall through to the later direct file (src/z_file).
#[tokio::test]
async fn disjoint_scan_prefers_dfs_order_over_later_file() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("docs"));
    let g = b"nested under dir".to_vec();
    let h = b"direct later file".to_vec();
    let snap = snap_hex(42);
    put_snap_blob(&f, &scoped, &snap, &g).await;
    put_snap_blob(&f, &scoped, &snap, &h).await;
    let (x_hex, x_canon) =
        tree_with_id(vec![file_entry("g", object_id(Kind::Blob, &g), 16, false)]);
    // tree_with_id sorts: "a_dir" < "z_file" stores the dir first.
    let (s_hex, s_canon) = tree_with_id(vec![
        dir_entry("a_dir", &x_hex, 16),
        file_entry("z_file", object_id(Kind::Blob, &h), 17, false),
    ]);
    let (root_hex, root_canon) = tree_with_id(vec![dir_entry("src", &s_hex, 17)]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root_hex,
        [root_canon, s_canon, x_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(resp.body["error"]["details"]["path"], json!("src/a_dir/g"));
}

/// A diamond under a scoped token: root [a -> D, b -> D], D = {dir deep
/// -> L}, L = {file f}, scope a. The a/ mount is covered and skipped,
/// the b/ mount is disjoint — 403 naming b/deep/f, never the in-scope
/// a/... path.
#[tokio::test]
async fn diamond_under_scope_names_disjoint_branch() {
    let f = fixture();
    let scoped = f.t.seed_token("scoped-w", "writer", Some("a"));
    let fblob = b"diamond f".to_vec();
    let snap = snap_hex(43);
    put_snap_blob(&f, &scoped, &snap, &fblob).await;
    let (l_hex, l_canon) = tree_with_id(vec![file_entry(
        "f",
        object_id(Kind::Blob, &fblob),
        9,
        false,
    )]);
    let (d_hex, d_canon) = tree_with_id(vec![dir_entry("deep", &l_hex, 9)]);
    let (root_hex, root_canon) =
        tree_with_id(vec![dir_entry("a", &d_hex, 9), dir_entry("b", &d_hex, 9)]);

    let resp = post_snapshot(
        &f,
        &scoped,
        &snap,
        &root_hex,
        [root_canon, d_canon, l_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    assert_eq!(resp.body["error"]["details"]["path"], json!("b/deep/f"));
}

// ---- fix3a: TTL ceiling -----------------------------------------------------

/// ttl_secs at the 366-day cap creates; one second past it (and a hostile
/// 9.3e9 that would overflow `now + ttl*1e9`) is 422 — validation, not a
/// panic.
#[tokio::test]
async fn ttl_above_maximum_is_422_not_a_panic() {
    let f = fixture();
    const MAX: u64 = 366 * 24 * 60 * 60; // 31_622_400

    // Boundary: exactly the cap is a normal create.
    let content = b"ttl ceiling".to_vec();
    let snap = snap_hex(25);
    put_snap_blob(&f, &f.writer_a, &snap, &content).await;
    let (tree_hex, trees) = flat_tree(&[("c.txt", object_id(Kind::Blob, &content), 11, false)]);
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, trees.clone(), MAX).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    assert_eq!(
        resp.body["expires_ns"],
        json!(BASE_NS + MAX * 1_000_000_000)
    );

    // Beyond the cap → 422, before any store work (no uploads needed).
    for ttl in [MAX + 1, 9_300_000_000] {
        let body = json!({
            "snap_id": snap_hex(26),
            "tree_id": tree_hex,
            "trees": trees.clone(),
            "ttl_secs": ttl,
        });
        let resp = post_json(
            &f.t.app,
            &format!("/v1/repos/{REPO}/snapshots"),
            &f.writer_a,
            &body,
        )
        .await;
        assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY, "ttl {ttl}");
        assert_eq!(code_of(&resp), "validation");
    }
    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 1);
}

// ---- fix3a: presence-validation caps ----------------------------------------

/// A closure referencing more than 8192 distinct blobs is 422 with the
/// limit in details — before any store work, so none of the blobs need to
/// exist. The missing-blobs list of a rejected POST is capped at 256 with a
/// `missing_blobs_truncated` flag; zero rows are written on every path.
#[tokio::test]
async fn blob_cap_is_422_and_missing_list_is_truncated() {
    let f = fixture();

    // 8193 distinct referenced blobs → 422 with the limit.
    let entries: Vec<TreeEntry> = (0..8_193u32)
        .map(|i| {
            file_entry(
                &format!("f{i:05}.bin"),
                object_id(Kind::Blob, i.to_le_bytes().as_slice()),
                1,
                false,
            )
        })
        .collect();
    let (tree_hex, trees) = tree_with_id(entries);
    let resp = post_snapshot(
        &f,
        &f.writer_a,
        &snap_hex(27),
        &tree_hex,
        trees.clone(),
        3600,
    )
    .await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );
    assert_eq!(code_of(&resp), "validation");
    assert_eq!(resp.body["error"]["details"]["limit"], json!(8192));

    // 300 distinct never-uploaded blobs → the missing list truncates.
    let entries: Vec<TreeEntry> = (0..300u32)
        .map(|i| {
            file_entry(
                &format!("g{i:04}.bin"),
                object_id(Kind::Blob, &format!("miss {i}").into_bytes()),
                1,
                false,
            )
        })
        .collect();
    let (tree_hex, trees) = tree_with_id(entries);
    let resp = post_snapshot(&f, &f.writer_a, &snap_hex(28), &tree_hex, trees, 3600).await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );
    let missing = resp.body["error"]["details"]["missing_blobs"]
        .as_array()
        .expect("missing_blobs details");
    assert_eq!(missing.len(), 256, "the list is capped at 256");
    assert_eq!(
        resp.body["error"]["details"]["missing_blobs_truncated"],
        json!(true)
    );

    // Every rejected path above left zero rows.
    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 0);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM trees"), 0);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM tree_entries"), 0);
}

/// Presence-validation recursion: a MISSING blob deep in a nested tree
/// (root → dir → subtree) is the id named in details, with no truncation.
#[tokio::test]
async fn presence_validation_reports_a_deep_missing_blob() {
    let f = fixture();
    let okb = b"deep ok".to_vec();
    let never = object_id(Kind::Blob, b"deep missing");
    let snap = snap_hex(29);
    put_snap_blob(&f, &f.writer_a, &snap, &okb).await;
    let (sub_hex, sub_canon) = tree_with_id(vec![
        file_entry("bad.bin", never, 12, false),
        file_entry("ok.bin", object_id(Kind::Blob, &okb), 7, false),
    ]);
    let (root_hex, root_canon) = tree_with_id(vec![dir_entry("deep", &sub_hex, 19)]);
    let resp = post_snapshot(
        &f,
        &f.writer_a,
        &snap,
        &root_hex,
        [root_canon, sub_canon].concat(),
        3600,
    )
    .await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );
    assert_eq!(
        resp.body["error"]["details"]["missing_blobs"],
        json!([never.to_hex()]),
        "the deep missing id is listed"
    );
    assert_eq!(
        resp.body["error"]["details"]["missing_blobs_truncated"],
        json!(false)
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 0);
}

// ---- fix3a: rowless-sweep age gate (the survive side) -----------------------

/// A rowless dir YOUNGER than the max TTL survives a GC pass: with the
/// injected clock at real now, `now − max TTL` sits before the fresh dir's
/// mtime, so the age gate keeps it (the swept side is pinned by
/// `rowless_dir_older_than_max_ttl_is_swept`).
#[tokio::test]
async fn rowless_dir_younger_than_max_ttl_survives_gc() {
    let f = fixture();
    let orphan = b"young rowless".to_vec();
    let fake = snap_hex(30);
    f.t.state
        .ephemeral
        .put_blob(&fake, &orphan)
        .expect("direct ephemeral put");
    assert!(f.t.data_dir.join("ephemeral").join(&fake).exists());

    let real_now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos() as u64;
    f.t.clock.store(real_now, Ordering::Relaxed);

    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(
        report,
        Default::default(),
        "a young rowless dir is not garbage"
    );
    assert!(
        f.t.data_dir.join("ephemeral").join(&fake).exists(),
        "the age gate kept the young dir"
    );
    assert_eq!(f.t.count("SELECT COUNT(*) FROM snapshots"), 0);
}

// ---- fix3a: misc pins --------------------------------------------------------

/// The snapshot objects PUT hash-verifies like every PUT: a payload that
/// does not hash to the URL id is 400 `malformed`.
#[tokio::test]
async fn misaddressed_snapshot_blob_put_is_400() {
    let f = fixture();
    let payload = b"real bytes".to_vec();
    let wrong = object_id(Kind::Blob, b"other bytes");
    let (status, _, body_bytes) = send_bytes(
        &f.t.app,
        "PUT",
        &format!(
            "/v1/repos/{REPO}/snapshots/{}/objects/{}",
            snap_hex(31),
            wrong.to_hex()
        ),
        &f.writer_a,
        Some(payload),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    let body: Value = serde_json::from_slice(&body_bytes).unwrap();
    assert_eq!(body["error"]["code"], "malformed");
}

/// The background pass is observable: serve runs with a 10 ms injected GC
/// interval (the off-switch stays ON), a 60 s-TTL snapshot expires, and a
/// background pass collects it — metadata flips 410 → 404 and the dir is
/// gone — with NO manual gc_snapshots call in this test (the POST-time
/// opportunistic pass fired before the clock moved, so only the interval
/// pass can have collected it). Bounded poll.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn background_gc_pass_collects_an_expired_snapshot() {
    let f = fixture_with(|state| {
        state.snapshot_gc_interval = Duration::from_millis(10);
    });

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

    let content = b"background gc victim".to_vec();
    let snap = snap_hex(32);
    put_snap_blob(&f, &f.writer_a, &snap, &content).await;
    let (tree_hex, trees) = flat_tree(&[("b.txt", object_id(Kind::Blob, &content), 20, false)]);
    let resp = post_snapshot(&f, &f.writer_a, &snap, &tree_hex, trees, 60).await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // Expire: the row survives, metadata answers 410.
    f.t.clock.store(BASE_NS + 61_000_000_000, Ordering::Relaxed);
    let uri = format!("/v1/repos/{REPO}/snapshots/{snap}");
    let resp = get(&f.t.app, &uri, &f.writer_a).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);

    // A background pass purges the row (410 → 404) and deletes the dir.
    let mut collected = false;
    for _ in 0..200 {
        let resp = get(&f.t.app, &uri, &f.writer_a).await;
        if resp.status == StatusCode::NOT_FOUND {
            collected = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(
        collected,
        "no background GC pass collected the expired snapshot"
    );
    assert!(
        !f.t.data_dir.join("ephemeral").join(&snap).exists(),
        "the background pass left the dir on disk"
    );

    send_shutdown.send(()).expect("serve task alive");
    tokio::time::timeout(Duration::from_secs(5), serve_task)
        .await
        .expect("serve shut down within 5 s")
        .expect("join")
        .expect("serve ok");
}
