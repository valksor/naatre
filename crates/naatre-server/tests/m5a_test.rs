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

use axum::Router;
use axum::body::Body;
use axum::http::{HeaderValue, StatusCode, header};
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use http_body_util::BodyExt;
use rusqlite::Connection;
use serde_json::{Value, json};
use tower::ServiceExt;

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
            "INSERT INTO tokens(principal, blake3, expires_ns, revoked) \
             VALUES (?1, ?2, NULL, 0)",
            rusqlite::params![principal, hash],
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
    let mut entries: Vec<TreeEntry> = files
        .iter()
        .map(|(name, blob, size, exec)| TreeEntry {
            name: name.to_string(),
            kind: EntryKind::File,
            object: *blob,
            size: *size,
            exec: *exec,
        })
        .collect();
    entries.sort_by(|a, b| a.name.as_bytes().cmp(b.name.as_bytes()));
    let t = Tree { entries };
    let id = tree::tree_id(&t).unwrap();
    let canonical = tree::encode(&t).unwrap();
    (id.to_hex(), vec![STANDARD.encode(canonical)])
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
    let t = build_app();
    let writer_a = t.seed_token("writer-a", "writer");
    let writer_b = t.seed_token("writer-b", "writer");
    let reader = t.seed_token("reader-p", "reader");
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
    let f = fixture();
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
    // A second pass converges (idempotent).
    let report = gc_snapshots(&f.t.state).await.unwrap();
    assert_eq!(report, Default::default());
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
