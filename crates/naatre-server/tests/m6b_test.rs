//! M6b suite: the derived-data surfaces. The plan's pins live here: the
//! blame hand fixture (exact per-line attribution over a 3-commit history)
//! and the blame PROPERTY (attributed ranges exactly tile the file; every
//! attribution's blob contains the attributed bytes); path-history PARITY
//! vs the full file_changes scan on randomized repos whose generator
//! covers delete→re-add, dir↔file flips, case-fold twins (distinct dirs —
//! the grammar-legal kind), exec-only Mods, and 2-parent imported nodes;
//! merge-base PARITY vs brute-force ancestor sets (randomized pairs over
//! an imported merge history; the in-memory skip-list unit tests live in
//! `routes/history.rs`); the read-during-backlog contract (?path= and
//! blame 503 `derivation_pending` + `Retry-After`, never
//! empty-as-if-no-history — including the UNROWED window, the boot
//! backfill held; merge-base never 503s — it falls back to the
//! walk, and the skip acceleration never consumes a TAINTED commit-graph
//! row); poison reads degrade with a flag and `poisoned_cs` instead of
//! lying or retrying forever; and the two deferred-backfill pins — after
//! bulk import and after an in-place upgrade of a pre-derive (M5-era)
//! database, everything derives through the SAME queue.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use proptest::prelude::*;
use rusqlite::Connection;
use serde_json::{Value, json};
use tokio::net::TcpListener;
use tokio::sync::oneshot;
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::derive::{
    BusyAbort, DeriveCtx, DeriveFailure, DeriveStatus, DerivedOut, Deriver, ParentOut,
    PoisonReason, derive_pass, status,
};
use naatre_server::serve::{ServeConfig, serve};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m6b";

// ---- harness ----------------------------------------------------------------

/// A serving context: router + state + seeded writer token, either over a
/// throwaway temp dir or over an existing data dir (the import-based
/// tests build real data dirs through `naatre_server::import`).
struct Ctx {
    _dir: Option<tempfile::TempDir>,
    app: Router,
    state: AppState,
    /// The injected clock's handle (the m6a fixture pattern): wait helpers
    /// advance it so lease expiry runs on the queue's clock.
    clock: Arc<std::sync::atomic::AtomicU64>,
    writer: String,
    db_path: PathBuf,
}

fn build_ctx(data_root: &Path, register: bool) -> Ctx {
    let data_path = data_root.join("data");
    let dd = DataDir::create(&data_path).unwrap();
    std::fs::write(dd.db_path(), b"").unwrap();
    open_ctx(&data_path, register)
}

fn in_memory_ctx(register: bool) -> Ctx {
    let dir = tempfile::tempdir().unwrap();
    let ctx = build_ctx(dir.path(), register);
    Ctx {
        _dir: Some(dir),
        ..ctx
    }
}

/// [`in_memory_ctx`] with a pre-`Arc` configuration hook (the
/// `open_ctx_with` pattern): the injectable caps/batch-size fields live on
/// `ServerState` and must be set before the state is shared.
fn in_memory_ctx_with(register: bool, configure: impl FnOnce(&mut ServerState)) -> Ctx {
    let dir = tempfile::tempdir().unwrap();
    let data_path = dir.path().join("data");
    let dd = DataDir::create(&data_path).unwrap();
    std::fs::write(dd.db_path(), b"").unwrap();
    let ctx = open_ctx_with(&data_path, register, configure);
    Ctx {
        _dir: Some(dir),
        ..ctx
    }
}

/// Opens a data dir (migrations already applied or to-be-applied), applies
/// the test clock and optional production registration, seeds a writer,
/// and builds the router. The production shape of `naatre serve`.
fn open_ctx(data_path: &Path, register: bool) -> Ctx {
    open_ctx_with(data_path, register, |_| {})
}

/// [`open_ctx`] with a pre-`Arc` configuration hook (the m5a
/// `fixture_with` pattern): the serve-task switches live on `ServerState`
/// and must be set before the state is shared.
fn open_ctx_with(
    data_path: &Path,
    register: bool,
    configure: impl FnOnce(&mut ServerState),
) -> Ctx {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let mut state = ServerState::open(DataDir::open(data_path).unwrap()).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock2 = Arc::clone(&clock);
    state.clock = Arc::new(move || clock2.load(Ordering::Relaxed));
    if register {
        for deriver in naatre_server::derived::production_kinds() {
            state.register_deriver(deriver).unwrap();
        }
    }
    configure(&mut state);
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    // Seed a writer principal + token the way the m6a harness does.
    let (writer, hash) = naatre_server::auth::mint_token();
    {
        let state2 = Arc::clone(&state);
        tokio::task::block_in_place(|| {
            tokio::runtime::Handle::current().block_on(async move {
                state2
                    .write(move |store| {
                        let conn = store.conn();
                        conn.execute(
                            &naatre_store::Sql::plain(
                                "INSERT INTO principals(name, role, created_ns) \
                             VALUES ('writer', 'admin', 0) \
                             ON CONFLICT(name) DO NOTHING",
                            ),
                            &naatre_store::pv![],
                        )
                        .unwrap();
                        conn.execute(
                            &naatre_store::Sql::plain(
                                "INSERT INTO tokens(principal, blake3, expires_ns, revoked) \
                             VALUES ('writer', ?1, NULL, 0)",
                            ),
                            &naatre_store::pv![hash],
                        )
                        .unwrap();
                        Ok(())
                    })
                    .await
                    .unwrap();
            })
        });
    }
    // Repo row.
    {
        let state2 = Arc::clone(&state);
        tokio::task::block_in_place(|| {
            tokio::runtime::Handle::current().block_on(async move {
                state2
                    .write(move |store| {
                        store
                            .conn()
                            .execute(
                                &naatre_store::Sql::ignore(
                                    "INSERT OR IGNORE INTO repos(name, created_ns) VALUES (?1, 0)",
                                ),
                                &naatre_store::pv![REPO],
                            )
                            .unwrap();
                        Ok(())
                    })
                    .await
                    .unwrap();
            })
        });
    }
    Ctx {
        _dir: None,
        app,
        state,
        clock,
        writer,
        db_path: data_path.join("naatre.db"),
    }
}

impl Ctx {
    fn db(&self) -> naatre_store::Db {
        let conn = Connection::open(&self.db_path).unwrap();
        conn.busy_timeout(Duration::from_secs(5)).unwrap();
        naatre_store::Db::Sqlite(conn)
    }

    fn count(&self, sql: &str) -> i64 {
        self.db()
            .query_row_map(
                &naatre_store::Sql::plain_owned(sql.to_string()),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .expect("count query")
    }
}

// ---- requests ---------------------------------------------------------------

struct Resp {
    status: StatusCode,
    headers: axum::http::HeaderMap,
    body: Value,
}

async fn send(app: &Router, method: &str, uri: &str, token: &str, body: Option<Value>) -> Resp {
    // The queue's write transactions are the contending writer at the
    // injected interval; retry the contract's retryable 503 like real
    // clients are told to.
    for attempt in 0..2000 {
        let resp = send_once(app, method, uri, token, body.clone()).await;
        let busy = resp.status == StatusCode::SERVICE_UNAVAILABLE
            && resp.body["error"]["code"] == "writer_busy";
        if !busy || attempt == 399 {
            return resp;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    unreachable!("retry loop always returns")
}

async fn send_once(
    app: &Router,
    method: &str,
    uri: &str,
    token: &str,
    body: Option<Value>,
) -> Resp {
    let mut builder = axum::http::Request::builder().method(method).uri(uri);
    builder = builder.header(header::AUTHORIZATION, format!("Bearer {token}"));
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

impl Ctx {
    async fn get(&self, uri: &str) -> Resp {
        send(&self.app, "GET", uri, &self.writer, None).await
    }

    async fn put_blob(&self, content: &[u8]) -> String {
        let id = object_id(Kind::Blob, content).to_hex();
        for attempt in 0..2000 {
            let req = axum::http::Request::builder()
                .method("PUT")
                .uri(format!("/v1/repos/{REPO}/objects/{id}").as_str())
                .header(header::AUTHORIZATION, format!("Bearer {}", self.writer))
                .header(header::CONTENT_TYPE, "application/octet-stream")
                .body(Body::from(content.to_vec()))
                .unwrap();
            let res = self
                .app
                .clone()
                .oneshot(req)
                .await
                .expect("in-memory request");
            let status = res.status();
            if status == StatusCode::CREATED || status == StatusCode::OK {
                return id;
            }
            if status == StatusCode::SERVICE_UNAVAILABLE && attempt < 399 {
                tokio::time::sleep(Duration::from_millis(10)).await;
                continue;
            }
            panic!("blob upload failed: {id}: {status}");
        }
        unreachable!("retry loop always returns")
    }
}

/// One change in a commit: (path, op, content for add/mod, exec).
struct Ch {
    path: &'static str,
    op: &'static str,
    content: Option<Vec<u8>>,
    exec: bool,
}

fn ch(path: &'static str, content: &[u8]) -> Ch {
    Ch {
        path,
        op: "mod",
        content: Some(content.to_vec()),
        exec: false,
    }
}

fn add_ch(path: &'static str, content: &[u8]) -> Ch {
    Ch {
        path,
        op: "add",
        content: Some(content.to_vec()),
        exec: false,
    }
}

fn del(path: &'static str) -> Ch {
    Ch {
        path,
        op: "del",
        content: None,
        exec: false,
    }
}

impl Ctx {
    /// Creates one changeset with an arbitrary change list; returns its
    /// hex id. The change list is sorted by path bytes first — the pinned
    /// client-side duty (the server rejects unsorted changes, never
    /// re-sorts).
    async fn commit(&self, parent: Option<&str>, author: &str, changes: Vec<Ch>) -> String {
        let mut changes = changes;
        changes.sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));
        let mut json_changes = Vec::new();
        for c in &changes {
            if c.op != "del" {
                let content = c.content.as_deref().expect("add/mod carries content");
                let blob = self.put_blob(content).await;
                json_changes.push(json!({
                    "path": c.path, "op": c.op, "blob": blob,
                    "size": content.len(), "exec": c.exec,
                }));
            } else {
                json_changes.push(json!({ "path": c.path, "op": "del" }));
            }
        }
        let body = json!({
            "parents": parent.map(|p| vec![p.to_string()]).unwrap_or_default(),
            "author": author,
            "message": "m6b commit",
            "changes": json_changes,
        });
        let resp = send(
            &self.app,
            "POST",
            &format!("/v1/repos/{REPO}/changesets"),
            &self.writer,
            Some(body),
        )
        .await;
        assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
        resp.body["id"].as_str().expect("created id").to_string()
    }

    /// Drives the queue directly until nothing is pending (these tests
    /// run no serve task, so one pass is enough; loop against a busy slot
    /// anyway), then marks every production kind's boot backfill complete:
    /// a drained queue is only half of the settled state these tests model
    /// — the read routes also gate on the backfill marker, which only
    /// serve's own boot task would have set.
    async fn drain(&self) {
        for _ in 0..100 {
            derive_pass(&self.state).await.expect("pass");
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
                    &naatre_store::Sql::plain(
                        "INSERT INTO meta(key, value) VALUES (?1, '0') \
                     ON CONFLICT(key) DO UPDATE SET value = excluded.value",
                    ),
                    &naatre_store::pv![format!("derive_backfill_done:{kind}")],
                )
                .unwrap();
        }
    }

    async fn blame(&self, at: &str, path: &str) -> Resp {
        self.get(&format!(
            "/v1/repos/{REPO}/blame?at={at}&path={}",
            urlencode(path)
        ))
        .await
    }

    /// Walks every `?path=` page through the derived index.
    async fn all_pages(&self, path: &str) -> Vec<String> {
        let mut out = Vec::new();
        let mut cursor: Option<String> = None;
        for _ in 0..1000 {
            let mut uri = format!("/v1/repos/{REPO}/log?path={}", urlencode(path));
            if let Some(c) = &cursor {
                uri.push_str(&format!("&cursor={c}"));
            }
            let resp = self.get(&uri).await;
            assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
            for e in resp.body["entries"].as_array().expect("entries") {
                out.push(e["id"].as_str().unwrap().to_string());
            }
            if resp.body["has_more"].as_bool() == Some(true) {
                cursor = Some(resp.body["next_cursor"].as_str().unwrap().to_string());
            } else {
                return out;
            }
        }
        panic!("pagination never ended for {path:?}");
    }

    /// The brute force: the full file_changes scan, newest first, straight
    /// from the store — the same rows the fallback route scans.
    fn brute_force_history(&self, path: &str) -> Vec<String> {
        self.db()
            .query_all_map(
                &naatre_store::Sql::plain(
                    "SELECT lower(hex(c.id)) FROM file_changes f \
                     JOIN changesets c ON c.repo_id = f.repo_id AND c.id = f.cs \
                     WHERE f.path = ?1 \
                     ORDER BY c.generation DESC, c.id ASC",
                ),
                &naatre_store::pv![path],
                |r| r.get::<String>(0),
            )
            .unwrap()
    }

    /// The blob bytes of `path` at changeset `at`, via /file + /objects.
    async fn blob_at(&self, at: &str, path: &str) -> Option<Vec<u8>> {
        let fv = self
            .get(&format!(
                "/v1/repos/{REPO}/file?at={at}&path={}",
                urlencode(path)
            ))
            .await;
        if fv.status != StatusCode::OK {
            return None;
        }
        let blob = fv.body["blob_id"].as_str().expect("blob id").to_string();
        self.get_bytes(&format!("/v1/repos/{REPO}/objects/{blob}"))
            .await
    }

    /// Raw-bytes GET (the object route serves bytes, not JSON).
    async fn get_bytes(&self, uri: &str) -> Option<Vec<u8>> {
        for _ in 0..2000 {
            let req = axum::http::Request::builder()
                .method("GET")
                .uri(uri)
                .header(header::AUTHORIZATION, format!("Bearer {}", self.writer))
                .body(Body::empty())
                .unwrap();
            let res = self
                .app
                .clone()
                .oneshot(req)
                .await
                .expect("in-memory request");
            let status = res.status();
            let bytes = BodyExt::collect(res.into_body())
                .await
                .expect("body")
                .to_bytes()
                .to_vec();
            if status == StatusCode::SERVICE_UNAVAILABLE
                && serde_json::from_slice::<Value>(&bytes)
                    .ok()
                    .map(|v| v["error"]["code"] == "writer_busy")
                    .unwrap_or(false)
            {
                tokio::time::sleep(Duration::from_millis(10)).await;
                continue;
            }
            if status.is_success() {
                return Some(bytes.to_vec());
            }
            return None;
        }
        unreachable!("retry loop always returns")
    }

    /// Flips a node to poison the way a real quarantine leaves it (state
    /// changed; the deriver's own rows untouched — the route gates on
    /// state first), optionally deleting its path_history rows the way a
    /// poison BEFORE the index write leaves the surface.
    fn force_poison(&self, kind: &str, cs_hex: &str, delete_rows: bool) {
        let conn = self.db();
        conn.execute(
            &naatre_store::Sql::plain(
                "UPDATE derive_state SET state = 'poison', lease_expires_ns = NULL \
             WHERE kind = (SELECT id FROM derive_kinds WHERE name = ?1) AND cs = ?2",
            ),
            &naatre_store::pv![kind, id_of(cs_hex).0.as_slice()],
        )
        .unwrap();
        if delete_rows {
            assert_eq!(kind, naatre_server::derived::KIND_PATH_HISTORY);
            conn.execute(
                &naatre_store::Sql::plain("DELETE FROM path_history WHERE cs = ?1"),
                &naatre_store::pv![id_of(cs_hex).0.as_slice()],
            )
            .unwrap();
        }
    }
}

fn urlencode(s: &str) -> String {
    let mut out = String::new();
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => {
                out.push(b as char)
            }
            other => out.push_str(&format!("%{other:02X}")),
        }
    }
    out
}

fn id_of(hex: &str) -> Id {
    Id::from_hex(hex).expect("64-hex id")
}

/// Progress-based wait until at least `min_done` rows are done, the
/// queue is quiet, and every kind's backfill marker is set (the m6a
/// `wait_queue_done` pattern, for the tests that run the REAL serve
/// task — boot backfill plus background queue drain while the test
/// watches; a frozen state fails fast with the counts). Each poll also
/// ADVANCES the injected clock (+50 ms): a lease lost to a WriterBusy
/// mid-pass recovers by lease expiry — on the queue's clock. A frozen
/// test clock would make that recovery impossible and turn one lost
/// lease into an artificial permanent wedge (the sweep can never fire;
/// every node blocked behind one lost lease waits forever). The done
/// floor keeps an empty pre-backfill state from passing vacuously.
async fn wait_done(ctx: &Ctx, min_done: i64) {
    let state = &ctx.state;
    let mut stalled = 0u32;
    let mut last = None;
    loop {
        let snap = {
            let state = Arc::clone(state);
            state
                .read(move |conn| {
                    let done: i64 = conn
                        .query_row_map(
                            &naatre_store::Sql::plain(
                                "SELECT COUNT(*) FROM derive_state WHERE state = 'done'",
                            ),
                            &naatre_store::pv![],
                            |r| r.get(0),
                        )
                        .unwrap();
                    let pending: i64 = conn
                        .query_row_map(
                            &naatre_store::Sql::plain(
                                "SELECT COUNT(*) FROM derive_state \
                             WHERE state IN ('pending','deriving')",
                            ),
                            &naatre_store::pv![],
                            |r| r.get(0),
                        )
                        .unwrap();
                    let markers: i64 = conn
                        .query_row_map(
                            &naatre_store::Sql::plain(
                                "SELECT COUNT(*) FROM meta \
                             WHERE key LIKE 'derive_backfill_done:%'",
                            ),
                            &naatre_store::pv![],
                            |r| r.get(0),
                        )
                        .unwrap();
                    Ok((done, pending, markers))
                })
                .await
                .unwrap()
        };
        // Settled = everything derived AND every kind's backfill marker
        // set (a drained queue alone can pass while a batch is still in
        // flight — then the route reads would 503 on the missing marker).
        if snap.1 == 0 && snap.0 >= min_done && snap.2 >= 3 {
            return;
        }
        if last == Some(snap) {
            stalled += 1;
        } else {
            stalled = 0;
        }
        last = Some(snap);
        assert!(
            stalled < 300,
            "derive backlog stalled: done={} pending={} markers={} now={} stuck={:?}",
            snap.0,
            snap.1,
            snap.2,
            state.now_ns(),
            {
                let state = Arc::clone(state);
                state
                    .read(move |conn| {
                        conn.query_all_map(
                            &naatre_store::Sql::plain(
                                "SELECT lower(hex(cs)), state, lease_expires_ns \
                                 FROM derive_state \
                                 WHERE state IN ('pending','deriving')",
                            ),
                            &naatre_store::pv![],
                            |r| {
                                Ok((
                                    r.get::<String>(0)?,
                                    r.get::<String>(1)?,
                                    r.get::<Option<i64>>(2)?,
                                ))
                            },
                        )
                        .map_err(|_| naatre_server::api_error::ApiError::internal())
                    })
                    .await
                    .unwrap()
            }
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
        ctx.clock
            .fetch_add(50_000_000, std::sync::atomic::Ordering::Relaxed);
    }
}

// ---- git fixture (the import_test pattern, minimal) --------------------------

struct GitFixture {
    _dir: tempfile::TempDir,
    path: PathBuf,
    commits: usize,
}

fn git_works() -> bool {
    match Command::new("git").arg("--version").output() {
        Ok(out) if out.status.success() => true,
        _ => {
            eprintln!("skipping: git not available");
            false
        }
    }
}

impl GitFixture {
    fn new(tag: &str) -> GitFixture {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(tag);
        let out = Command::new("git")
            .args(["-c", "init.defaultBranch=main", "init", "-q"])
            .arg(&path)
            .output()
            .expect("git init");
        assert!(out.status.success(), "git init failed");
        GitFixture {
            _dir: dir,
            path,
            commits: 0,
        }
    }

    fn git(&self, args: &[&str]) -> String {
        let stamp = 1_767_225_600 + self.commits * 60;
        let out = Command::new("git")
            .args([
                "-c",
                "user.name=Fix Tur e",
                "-c",
                "user.email=fix@example.com",
            ])
            .arg("-C")
            .arg(&self.path)
            .args(args)
            .env("GIT_AUTHOR_DATE", format!("{stamp} +0000"))
            .env("GIT_COMMITTER_DATE", format!("{stamp} +0000"))
            .output()
            .expect("git");
        assert!(
            out.status.success(),
            "git {args:?} failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

    fn write(&self, rel: &str, content: &[u8]) {
        let full = self.path.join(rel);
        std::fs::create_dir_all(full.parent().unwrap()).unwrap();
        std::fs::write(full, content).unwrap();
    }

    fn commit(&mut self, msg: &str) -> String {
        self.git(&["add", "-A"]);
        self.git(&["commit", "-q", "--cleanup=verbatim", "-m", msg]);
        self.commits += 1;
        self.git(&["rev-parse", "HEAD"])
    }

    /// root → main edit → side-branch edit → merge (the 2-parent node).
    fn with_merge(tag: &str) -> GitFixture {
        let mut f = GitFixture::new(tag);
        f.write("hello.txt", b"hello\n");
        f.write("a.txt", b"a0\n");
        f.commit("root");
        f.write("hello.txt", b"hello main\n");
        f.commit("main edit");
        f.git(&["checkout", "-q", "-b", "side", "HEAD~1"]);
        f.write("side.txt", b"side\n");
        f.commit("side edit");
        f.git(&["checkout", "-q", "main"]);
        f.git(&["merge", "--no-ff", "-q", "-m", "merge side", "side"]);
        f.commits += 1;
        f
    }
}

/// Imports a git fixture into a fresh data dir with NO registered kinds
/// (the `naatre import` shape); returns the data dir path.
fn import_fixture(git: &GitFixture) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("data");
    let dd = DataDir::create(&data).unwrap();
    std::fs::write(dd.db_path(), b"").unwrap();
    let report = naatre_server::import::run(
        &data,
        &naatre_server::import::ImportOptions {
            git_path: git.path.clone(),
            into: Some(REPO.into()),
            skip_unsupported: false,
        },
        &Default::default(),
    )
    .unwrap();
    assert_eq!(report.status, "ok", "{}", report.to_json_line());
    dir
}

// ---- 1. the blame hand fixture ---------------------------------------------

/// A 3-commit history with edits at known lines blames to the exact
/// expected per-line attribution: each line's changeset is the commit that
/// introduced it (first-parent semantics), authors flow from the
/// changesets, and the texts reconstruct the file at `at` in order.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn blame_hand_fixture_attributes_known_lines() {
    let ctx = in_memory_ctx(true);
    let c1 = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("story.txt", b"alpha\nbeta\ngamma\n")],
        )
        .await;
    let c2 = ctx
        .commit(
            Some(&c1),
            "Bob <bob@example.com>",
            vec![ch("story.txt", b"alpha\nBETA\ngamma\ndelta\n")],
        )
        .await;
    let c3 = ctx
        .commit(
            Some(&c2),
            "Cid <cid@example.com>",
            vec![ch("story.txt", b"ALPHA\nBETA\ngamma\ndelta\n")],
        )
        .await;
    ctx.drain().await;

    let resp = ctx.blame(&c3, "story.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["binary"], json!(false));
    assert_eq!(resp.body["degraded"], json!(false));
    assert_eq!(resp.body["poisoned_cs"], json!([]));
    let lines = resp.body["lines"].as_array().expect("lines");
    let got: Vec<(String, String, String)> = lines
        .iter()
        .map(|l| {
            (
                l["cs"].as_str().unwrap().to_string(),
                l["author"].as_str().unwrap().to_string(),
                l["text"].as_str().unwrap().to_string(),
            )
        })
        .collect();
    assert_eq!(
        got,
        vec![
            (c3.clone(), "Cid <cid@example.com>".into(), "ALPHA".into()),
            (c2.clone(), "Bob <bob@example.com>".into(), "BETA".into()),
            (c1.clone(), "Ann <ann@example.com>".into(), "gamma".into()),
            (c2.clone(), "Bob <bob@example.com>".into(), "delta".into()),
        ],
        "exact hand-computed attribution"
    );

    // Same walk at the intermediate commit: c2's view is alpha→c1,
    // BETA→c2, gamma→c1, delta→c2.
    let resp = ctx.blame(&c2, "story.txt").await;
    let got: Vec<(String, String)> = resp.body["lines"]
        .as_array()
        .unwrap()
        .iter()
        .map(|l| {
            (
                l["cs"].as_str().unwrap().to_string(),
                l["text"].as_str().unwrap().to_string(),
            )
        })
        .collect();
    let expect_order = |texts: &[&str]| -> Vec<(String, String)> {
        texts
            .iter()
            .map(|t| {
                (
                    match *t {
                        "alpha" | "gamma" => c1.clone(),
                        _ => c2.clone(),
                    },
                    t.to_string(),
                )
            })
            .collect()
    };
    // The assembly is oldest-first; assert as a set-with-order modulo the
    // pinned ordering: the walk serves the file's order.
    let mut want = expect_order(&["alpha", "BETA", "gamma", "delta"]);
    want.sort();
    let mut got_sorted = got.clone();
    got_sorted.sort();
    assert_eq!(got_sorted, want, "alpha→c1, BETA→c2, gamma→c1, delta→c2");
    assert_eq!(
        got.iter().map(|(_, t)| t.as_str()).collect::<Vec<_>>(),
        vec!["alpha", "BETA", "gamma", "delta"],
        "the file at c2, in order"
    );
}

// ---- 2. the blame property ---------------------------------------------------

proptest! {
    #![proptest_config(ProptestConfig::with_cases(24))]

    /// The plan's blame property: attributed ranges exactly tile the file
    /// (the served lines ARE the file at `at`, in order — no gaps, no
    /// overlaps) and every attribution's blob contains the attributed
    /// bytes (the named changeset's version of the path has the line).
    #[test]
    fn blame_property_attributions_tile_and_contain(
        edits in proptest::collection::vec((0u8..3, 0usize..8, 0u8..8), 2..7),
    ) {
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(async move {
            let ctx = in_memory_ctx(true);
            const POOL: [&str; 8] = [
                "alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
            ];
            let mut lines: Vec<String> =
                vec!["alpha".into(), "delta".into(), "golf".into()];
            let file = |lines: &[String]| {
                let mut s = lines.join("\n");
                s.push('\n');
                s.into_bytes()
            };
            let mut parent = ctx
                .commit(None, "Gen <gen@example.com>", vec![add_ch("f.txt", &file(&lines))])
                .await;
            for &(kind, pos, line_idx) in &edits {
                match kind {
                    0 if lines.len() > 1 => {
                        let i = pos.min(lines.len() - 1);
                        lines.remove(i);
                    }
                    1 => {
                        let i = pos.min(lines.len());
                        lines.insert(i, POOL[line_idx as usize].to_string());
                    }
                    _ => {
                        let i = pos.min(lines.len() - 1);
                        lines[i] = POOL[line_idx as usize].to_string();
                    }
                }
                parent = ctx
                    .commit(
                        Some(&parent),
                        "Gen <gen@example.com>",
                        vec![add_ch("f.txt", &file(&lines))],
                    )
                    .await;
            }
            ctx.drain().await;

            // (a) tiling: the served lines are exactly the file at `at`.
            let resp = ctx.blame(&parent, "f.txt").await;
            assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
            let served: Vec<String> = resp.body["lines"].as_array().unwrap()
                .iter()
                .map(|l| l["text"].as_str().unwrap().to_string())
                .collect();
            assert_eq!(served.len(), lines.len(), "line count tiles the file");
            assert_eq!(served, lines, "texts match the file in order");

            // (b) containment: every attribution's blob contains the line.
            let mut per_cs: std::collections::HashMap<String, Vec<String>> =
                std::collections::HashMap::new();
            for l in resp.body["lines"].as_array().unwrap() {
                per_cs
                    .entry(l["cs"].as_str().unwrap().to_string())
                    .or_default()
                    .push(l["text"].as_str().unwrap().to_string());
            }
            for (cs, texts) in &per_cs {
                let blob = ctx.blob_at(cs, "f.txt").await
                    .expect("every attribution's changeset has the path");
                let blob_text = String::from_utf8(blob).expect("text fixture");
                let blob_lines: std::collections::HashSet<&str> =
                    blob_text.split('\n').filter(|l| !l.is_empty()).collect();
                for t in texts {
                    assert!(
                        blob_lines.contains(t.as_str()),
                        "line {t:?} attributed to {cs} must be in its blob"
                    );
                }
            }
        })
    }
}

// ---- 3. path-history parity --------------------------------------------------

/// The random repo generator's op: which legal mutation to apply to the
/// tracked tree.
#[derive(Debug, Clone)]
enum GenOp {
    /// Edit an existing file.
    Mod(u8),
    /// Flip exec on an existing file with the SAME blob.
    ExecFlip(u8),
    /// Delete an existing file.
    Del(u8),
    /// Re-add a deleted path.
    ReAdd(u8),
    /// Dir↔file flip between "flip" and "flip/inner.txt" (one multi-change
    /// commit — the only legal shape across the duality boundary).
    Flip,
}

fn file_universe() -> [&'static str; 5] {
    ["a.txt", "b.txt", "Dir/x.txt", "dir/y.txt", "flip"]
}

fn op_strategy() -> impl Strategy<Value = GenOp> {
    prop_oneof![
        3 => (0u8..5).prop_map(GenOp::Mod),
        1 => (0u8..5).prop_map(GenOp::ExecFlip),
        2 => (0u8..5).prop_map(GenOp::Del),
        2 => (0u8..5).prop_map(GenOp::ReAdd),
        1 => Just(GenOp::Flip),
    ]
}

/// Tracked generator state: path → present (rev, exec). `flip/inner.txt`
/// is tracked implicitly: present when "flip" is absent.
struct GenRepo {
    present: std::collections::BTreeMap<String, Option<(u64, bool)>>,
    rev: u64,
}

fn static_content(path: &str, rev: u64) -> Vec<u8> {
    format!("content of {path} at rev {rev}\n").into_bytes()
}

impl GenRepo {
    fn new(rooted: bool) -> Self {
        let mut present = std::collections::BTreeMap::new();
        for p in file_universe() {
            present.insert(p.to_string(), None);
        }
        present.insert("flip/inner.txt".to_string(), None);
        if rooted {
            for p in ["a.txt", "b.txt", "Dir/x.txt", "dir/y.txt"] {
                present.insert(p.to_string(), Some((0, false)));
            }
        }
        GenRepo { present, rev: 1 }
    }

    fn ch_of(path: &str, op: &'static str, rev: u64, exec: bool) -> Ch {
        // The universe paths are 'static; flip/inner.txt is the one
        // non-static name — leak it (test-lifetime strings).
        let leaked: &'static str = Box::leak(path.to_string().into_boxed_str());
        Ch {
            path: leaked,
            op,
            content: Some(static_content(path, rev)),
            exec,
        }
    }

    /// Applies one op, returning the change list (empty = nothing legal).
    fn apply(&mut self, op: &GenOp) -> Vec<Ch> {
        let universe = file_universe();
        match op {
            GenOp::Mod(i) => {
                let path = universe[*i as usize];
                match self.present.get(path).copied().flatten() {
                    Some((_, exec)) => {
                        self.rev += 1;
                        self.present
                            .insert(path.to_string(), Some((self.rev, exec)));
                        vec![Self::ch_of(path, "mod", self.rev, exec)]
                    }
                    None => vec![],
                }
            }
            GenOp::ExecFlip(i) => {
                let path = universe[*i as usize];
                match self.present.get(path).copied().flatten() {
                    Some((rev, exec)) => {
                        self.present.insert(path.to_string(), Some((rev, !exec)));
                        vec![Self::ch_of(path, "mod", rev, !exec)]
                    }
                    None => vec![],
                }
            }
            GenOp::Del(i) => {
                let path = universe[*i as usize];
                if self.present.get(path).copied().flatten().is_some() {
                    self.present.insert(path.to_string(), None);
                    let leaked: &'static str = Box::leak(path.to_string().into_boxed_str());
                    vec![del(leaked)]
                } else {
                    vec![]
                }
            }
            GenOp::ReAdd(i) => {
                let path = universe[*i as usize];
                if self.present.get(path).copied().flatten().is_none() && path != "flip" {
                    self.rev += 1;
                    self.present
                        .insert(path.to_string(), Some((self.rev, false)));
                    vec![Self::ch_of(path, "add", self.rev, false)]
                } else {
                    vec![]
                }
            }
            GenOp::Flip => match self.present.get("flip").copied().flatten() {
                Some(_) => {
                    self.present.insert("flip".into(), None);
                    self.rev += 1;
                    self.present
                        .insert("flip/inner.txt".into(), Some((self.rev, false)));
                    vec![
                        del("flip"),
                        Self::ch_of("flip/inner.txt", "add", self.rev, false),
                    ]
                }
                None => {
                    if self
                        .present
                        .get("flip/inner.txt")
                        .copied()
                        .flatten()
                        .is_some()
                    {
                        self.present.insert("flip/inner.txt".into(), None);
                        self.rev += 1;
                        self.present.insert("flip".into(), Some((self.rev, false)));
                        vec![
                            del("flip/inner.txt"),
                            Self::ch_of("flip", "add", self.rev, false),
                        ]
                    } else {
                        vec![]
                    }
                }
            },
        }
    }
}

proptest! {
    #![proptest_config(ProptestConfig::with_cases(16))]

    /// The plan's path-history parity oracle: on a randomized repo whose
    /// generator covers delete→re-add, dir↔file flips, case-fold twins,
    /// and exec-only Mods, the derived `?path=` history equals a
    /// brute-force full file_changes scan for every path the generator
    /// touched.
    #[test]
    fn path_history_parity_on_generated_repos(
        ops in proptest::collection::vec(op_strategy(), 6..18),
    ) {
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(async move {
            let ctx = in_memory_ctx(true);
            // Root: the case-fold twin dirs AND both plain files (the twins
            // coexist — distinct dirs is the grammar-legal kind).
            let root = ctx
                .commit(
                    None,
                    "Gen <gen@example.com>",
                    vec![
                        Ch { path: "a.txt", op: "add", content: Some(static_content("a.txt", 0)), exec: false },
                        Ch { path: "b.txt", op: "add", content: Some(static_content("b.txt", 0)), exec: false },
                        Ch { path: "Dir/x.txt", op: "add", content: Some(static_content("Dir/x.txt", 0)), exec: false },
                        Ch { path: "dir/y.txt", op: "add", content: Some(static_content("dir/y.txt", 0)), exec: false },
                    ],
                )
                .await;
            let mut parent = root;
            let mut repo = GenRepo::new(true);
            for op in &ops {
                let changes = repo.apply(op);
                if !changes.is_empty() {
                    parent = ctx.commit(Some(&parent), "Gen <gen@example.com>", changes).await;
                }
            }
            ctx.drain().await;

            let mut paths: Vec<String> = file_universe()
                .iter()
                .map(|p| p.to_string())
                .chain(["flip/inner.txt".to_string()])
                .collect();
            paths.sort();
            paths.dedup();
            for path in &paths {
                let derived = ctx.all_pages(path).await;
                let brute = ctx.brute_force_history(path);
                assert_eq!(derived, brute, "parity for {path:?}");
            }
        })
    }
}

// ---- 4. merge-base parity after an imported merge ----------------------------

proptest! {
    #![proptest_config(ProptestConfig::with_cases(6))]

    /// Randomized repos THROUGH an imported 2-parent node: path-history
    /// parity still holds on the merged history, and merge-base equals the
    /// brute-force ancestor sets for every pair (the derived commit-graph
    /// skip lists in real use — the in-memory unit tests cover the
    /// algorithm's shapes).
    #[test]
    fn imported_merge_parity(
        ops in proptest::collection::vec(op_strategy(), 4..10),
    ) {
        if git_works() {
            let rt = tokio::runtime::Builder::new_multi_thread()
                .worker_threads(2)
                .enable_all()
                .build()
                .unwrap();
            rt.block_on(async move {
                let git = GitFixture::with_merge("m6bmerge");
            let dir_holder = import_fixture(&git);
            let data = dir_holder.path().join("data");

            // Serve with the production kinds: the boot-time backfill
            // derives the imported backlog through the SAME queue.
            let imported: i64 = {
                naatre_store::Db::Sqlite(Connection::open(data.join("naatre.db")).unwrap())
                    .query_row_map(
                        &naatre_store::Sql::plain("SELECT COUNT(*) FROM changesets"),
                        &naatre_store::pv![],
                        |r| r.get(0),
                    )
                    .unwrap()
            };
            let ctx = open_ctx(&data, true);
            serve_until_quiet(Arc::clone(&ctx.state)).await;
            wait_done(&ctx, 3 * imported).await;

            // Continue the generator on TOP of the imported main.
            let main_hex = {
                let conn = ctx.db();
                let repo_id: i64 = conn
                    .query_row_map(
                        &naatre_store::Sql::plain("SELECT id FROM repos WHERE name = ?1"),
                        &naatre_store::pv![REPO],
                        |r| r.get(0),
                    )
                    .unwrap();
                let main: Vec<u8> = conn
                    .query_row_map(
                        &naatre_store::Sql::plain(
                            "SELECT target FROM refs WHERE repo_id = ?1 AND name = 'main'",
                        ),
                        &naatre_store::pv![repo_id],
                        |r| r.get(0),
                    )
                    .unwrap();
                Id(main.try_into().unwrap()).to_hex()
            };
            let mut repo = GenRepo::new(false);
            repo.present.insert("hello.txt".into(), Some((0, false)));
            repo.present.insert("a.txt".into(), Some((0, false)));
            repo.present.insert("side.txt".into(), Some((0, false)));
            repo.rev = 100;
            let mut parent = main_hex;
            for op in &ops {
                let changes = repo.apply(op);
                if !changes.is_empty() {
                    parent = ctx.commit(Some(&parent), "Gen <gen@example.com>", changes).await;
                }
            }
            ctx.drain().await;

            // Path-history parity for the universe paths + the imported
            // ones.
            let paths: Vec<String> = file_universe()
                .iter().map(|p| p.to_string())
                .chain([
                    "flip/inner.txt".to_string(),
                    "hello.txt".to_string(),
                    "side.txt".to_string(),
                ])
                .collect();
            for path in &paths {
                let derived = ctx.all_pages(path).await;
                let brute = ctx.brute_force_history(path);
                assert_eq!(derived, brute, "parity for {path:?}");
            }

            // Merge-base parity for EVERY pair: the route (derived skip
            // lists) vs the brute-force ancestor sets.
            let cs_ids: Vec<String> = ctx
                .db()
                .query_all_map(
                    &naatre_store::Sql::plain("SELECT lower(hex(id)) FROM changesets"),
                    &naatre_store::pv![],
                    |r| r.get::<String>(0),
                )
                .unwrap();
            for i in 0..cs_ids.len() {
                for j in 0..cs_ids.len() {
                    let resp = ctx
                        .get(&format!(
                            "/v1/repos/{REPO}/merge-base?a={}&b={}",
                            cs_ids[i], cs_ids[j]
                        ))
                        .await;
                    assert_eq!(resp.status, StatusCode::OK);
                    let route_gen = resp.body["merge_base"].as_str()
                        .and_then(|r| cs_gen_db_from(&ctx.db(), r));
                    let brute_gen = brute_force_merge_base_gen(&ctx.db(), &cs_ids[i], &cs_ids[j]);
                    assert_eq!(route_gen, brute_gen, "merge-base parity for {}/{}", cs_ids[i], cs_ids[j]);
                }
            }
            })
        }
    }
}

/// Spawns the real serve task (the backfill runs at boot); the caller
/// shuts it down via [`serve_shutdown`].
async fn serve_until_quiet(state: AppState) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let (tx, rx) = oneshot::channel::<()>();
    // Shutdown never fires: the task dies with the test's runtime.
    let task = tokio::spawn(serve(ServeConfig { listener, state }, async move {
        let _ = rx.await;
    }));
    std::mem::forget((tx, task));
}

/// One full serve boot that STOPS before returning (shutdown fires, the
/// task is awaited, the state — and its data-dir flock — drops). For the
/// multi-boot pins: a forgotten [`serve_until_quiet`] task would hold the
/// flock forever and the next boot could not open the data dir.
async fn serve_shutdown(state: AppState) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let (tx, rx) = oneshot::channel::<()>();
    let task = tokio::spawn(serve(ServeConfig { listener, state }, async move {
        let _ = rx.await;
    }));
    let _ = tx.send(());
    task.await.unwrap().expect("serve stopped cleanly");
}

fn cs_gen_db_from(conn: &naatre_store::Db, cs_hex: &str) -> Option<i64> {
    let mut bytes = [0u8; 32];
    hex_decode32(cs_hex, &mut bytes)?;
    conn.query_row_map(
        &naatre_store::Sql::plain("SELECT generation FROM changesets WHERE id = ?1"),
        &naatre_store::pv![&bytes],
        |r| r.get(0),
    )
    .ok()
}

fn hex_decode32(hex: &str, out: &mut [u8; 32]) -> Option<()> {
    if hex.len() != 64 {
        return None;
    }
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&hex[i * 2..i * 2 + 2], 16).ok()?;
    }
    Some(())
}

/// The brute-force oracle: the HIGHEST generation in the ancestor-set
/// intersection (a generation, not an id — several max-generation common
/// ancestors may exist and the walks' tie-breaks legitimately differ).
fn brute_force_merge_base_gen(conn: &naatre_store::Db, a: &str, b: &str) -> Option<i64> {
    let ancestors = |seed: &str| -> std::collections::HashSet<String> {
        let mut set = std::collections::HashSet::new();
        let mut queue = std::collections::VecDeque::from([seed.to_string()]);
        while let Some(id) = queue.pop_front() {
            if !set.insert(id.clone()) {
                continue;
            }
            let mut bytes = [0u8; 32];
            if hex_decode32(&id, &mut bytes).is_none() {
                continue;
            }
            let parents: Vec<String> = conn
                .query_all_map(
                    &naatre_store::Sql::plain(
                        "SELECT hex(parent) FROM changeset_parents WHERE cs = ?1",
                    ),
                    &naatre_store::pv![&bytes],
                    |r| r.get::<String>(0),
                )
                .unwrap();
            for p in parents {
                queue.push_back(p.to_lowercase());
            }
        }
        set
    };
    let in_a = ancestors(a);
    ancestors(b)
        .into_iter()
        .filter(|id| in_a.contains(id))
        .filter_map(|id| cs_gen_db_from(conn, &id))
        .max()
}

// ---- 5. read-during-backlog + poison ----------------------------------------

/// With the queue stopped, ?path= and blame answer 503
/// `derivation_pending` + `Retry-After` (NEVER empty-as-if-no-history);
/// merge-base does NOT 503 — the walk fallback answers it. After the
/// queue drains, the same reads are correct.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn read_during_backlog_503s_and_recovers() {
    let ctx = in_memory_ctx(true);
    let c1 = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("f.txt", b"one\ntwo\n")],
        )
        .await;
    let c2 = ctx
        .commit(
            Some(&c1),
            "Ann <ann@example.com>",
            vec![ch("f.txt", b"one\nTWO\n")],
        )
        .await;

    // Queue never driven: the backlog is pending.
    let resp = ctx.blame(&c2, "f.txt").await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], "derivation_pending");
    assert_eq!(
        resp.headers
            .get(header::RETRY_AFTER)
            .map(|v| v.to_str().unwrap()),
        Some("1"),
        "the contract's Retry-After"
    );

    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(resp.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(resp.body["error"]["code"], "derivation_pending");
    assert!(resp.headers.get(header::RETRY_AFTER).is_some());

    // Merge-base NEVER 503s: the walk fallback answers.
    let resp = ctx
        .get(&format!("/v1/repos/{REPO}/merge-base?a={c1}&b={c2}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["merge_base"], json!(c1));

    // After the queue drains: correct derived answers.
    ctx.drain().await;
    let resp = ctx.blame(&c2, "f.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let got: Vec<(String, String)> = resp.body["lines"]
        .as_array()
        .unwrap()
        .iter()
        .map(|l| {
            (
                l["cs"].as_str().unwrap().to_string(),
                l["text"].as_str().unwrap().to_string(),
            )
        })
        .collect();
    assert_eq!(
        got,
        vec![(c1.clone(), "one".to_string()), (c2, "TWO".to_string())],
        "the file at c2, in order, with per-line attribution"
    );
    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["entries"].as_array().unwrap().len(), 2);
    assert_eq!(resp.body["degraded"], json!(false));
}

/// Poison in the chain, BEFORE the descendants derive: every descendant
/// derives DONE WITH TAINT (tainted=1, poison_cs = the poisoned node) and
/// every descendant read comes back 200 + `degraded: true` +
/// `poisoned_cs` naming that node — the flag comes off the stored row, in
/// O(1), not from re-walking. An untouched sibling branch is NOT flagged:
/// its rows are clean and its reads exact. Pending anywhere keeps the 503
/// contract.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn poison_degrades_blame_reads() {
    let ctx = in_memory_ctx(true);
    let c1 = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("f.txt", b"one\ntwo\nthree\n")],
        )
        .await;
    let c2 = ctx
        .commit(
            Some(&c1),
            "Bob <bob@example.com>",
            vec![ch("f.txt", b"one\nTWO\nthree\n")],
        )
        .await;
    // The sibling branch: off c1, touches its own file — never downstream
    // of the poison.
    let s1 = ctx
        .commit(
            Some(&c1),
            "Sib <sib@example.com>",
            vec![add_ch("sib.txt", b"sibling\n")],
        )
        .await;
    let c3 = ctx
        .commit(
            Some(&c2),
            "Cid <cid@example.com>",
            vec![ch("f.txt", b"ONE\nTWO\nthree\n")],
        )
        .await;
    let c4 = ctx
        .commit(
            Some(&c3),
            "Ann <ann@example.com>",
            vec![ch("f.txt", b"ONE\nTWO\nthree\nfour\n")],
        )
        .await;

    // Poison c2 while everything below it is still queued.
    ctx.force_poison(naatre_server::derived::KIND_BLAME, &c2, false);
    ctx.drain().await;

    // The stored taint: every descendant done-with-taint naming c2; the
    // sibling and the root clean.
    let taint_of = |cs: &str| -> (i64, Option<String>) {
        let conn = ctx.db();
        conn.query_row_map(
            &naatre_store::Sql::plain(
                "SELECT s.tainted, CASE WHEN s.poison_cs IS NULL THEN NULL \
             ELSE lower(hex(s.poison_cs)) END FROM derive_state s \
             JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = 'blame' AND s.cs = ?1",
            ),
            &naatre_store::pv![id_of(cs).0.as_slice()],
            |r| {
                Ok((
                    r.get::<i64>(0).unwrap(),
                    r.get::<Option<String>>(1).unwrap(),
                ))
            },
        )
        .unwrap()
    };
    let taint_hex = |cs: &str| -> Option<String> { taint_of(cs).1.map(|h| h.to_lowercase()) };
    assert_eq!(
        taint_of(&c2),
        (0, None),
        "the poisoned row itself is poison, not tainted-done"
    );
    assert_eq!(
        (taint_of(&c3).0, taint_hex(&c3)),
        (1, Some(c2.clone())),
        "the child derived on the empty marker WITH taint"
    );
    assert_eq!(
        (taint_of(&c4).0, taint_hex(&c4)),
        (1, Some(c2.clone())),
        "taint inherits through generations"
    );
    assert_eq!(
        taint_of(&s1),
        (0, None),
        "the untouched sibling stays clean"
    );
    assert_eq!(taint_of(&c1), (0, None));

    // Blame at the deep descendant: degraded off the stored taint, lines
    // complete, boundary attribution at c2 (its version of the file is
    // what the walk can still prove).
    let resp = ctx.blame(&c4, "f.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(true));
    assert_eq!(resp.body["poisoned_cs"], json!([c2.clone()]));
    let got: Vec<(String, String)> = resp.body["lines"]
        .as_array()
        .unwrap()
        .iter()
        .map(|l| {
            (
                l["cs"].as_str().unwrap().to_string(),
                l["text"].as_str().unwrap().to_string(),
            )
        })
        .collect();
    assert_eq!(got.len(), 4, "every line attributed, none dropped: {got:?}");
    assert!(
        got.contains(&(c4.clone(), "four".to_string())),
        "lines above the poison are exact: {got:?}"
    );
    assert!(
        got.iter()
            .all(|(cs, _)| cs == &c4 || cs == &c3 || cs == &c2),
        "nothing is attributed below the poisoned boundary: {got:?}"
    );

    // The untouched sibling: exact, NOT flagged (the pin).
    let resp = ctx.blame(&s1, "sib.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(false), "{}", resp.body);
    assert_eq!(resp.body["poisoned_cs"], json!([]));

    // The path-history kind is a different kind: its rows for f.txt are
    // all derived and exact.
    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["degraded"], json!(false));
    assert_eq!(resp.body["entries"].as_array().unwrap().len(), 4);

    // A pending node outranks the degraded answer: the 503 contract holds.
    let c5 = ctx
        .commit(
            Some(&c4),
            "Ann <ann@example.com>",
            vec![ch("f.txt", b"ONE\nTWO\nthree\nfour\nfive\n")],
        )
        .await;
    let resp = ctx.blame(&c5, "f.txt").await;
    assert_eq!(resp.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(resp.body["error"]["code"], "derivation_pending");
    // The steady-state pending leg for ?path= too: the backfill marker is
    // SET (drained above), but a pending/deriving row exists — the read
    // 503s, never answers from the index with a hole.
    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(resp.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(resp.body["error"]["code"], "derivation_pending");
}

/// Poison in the path_history kind: ?path= degrades (200 + flag +
/// poisoned_cs, the quarantined node's touch missing) and never presents
/// the hole as complete history.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn poison_degrades_path_history_reads() {
    let ctx = in_memory_ctx(true);
    let c1 = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("f.txt", b"one\n")],
        )
        .await;
    let c2 = ctx
        .commit(
            Some(&c1),
            "Ann <ann@example.com>",
            vec![ch("f.txt", b"one\ntwo\n")],
        )
        .await;
    let c3 = ctx
        .commit(
            Some(&c2),
            "Ann <ann@example.com>",
            vec![add_ch("g.txt", b"g\n")],
        )
        .await;
    ctx.drain().await;

    ctx.force_poison(naatre_server::derived::KIND_PATH_HISTORY, &c2, true);

    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(true));
    assert_eq!(resp.body["poisoned_cs"], json!([c2.clone()]));
    let ids: Vec<&str> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert_eq!(
        ids,
        vec![c1.as_str()],
        "the poisoned touch is absent, and flagged"
    );

    // merge-base ignores the poison entirely (walk fallback).
    let resp = ctx
        .get(&format!("/v1/repos/{REPO}/merge-base?a={c3}&b={c2}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["merge_base"], json!(c2));
}

/// The merge-base skip acceleration must not consume TAINTED commit-graph
/// rows. Poison a mid-chain node (direct db manipulation, the pins above),
/// let the descendants derive done-with-taint (the natural quarantine
/// shape), and the route must still equal the brute-force base across the
/// poison. Then hand the tainted endpoint a WRONG skip entry (direct db
/// manipulation): the answer must not move — tainted skips are not used —
/// and flipping ONLY the taint bit off (same bogus bytes) moves it,
/// proving the harness really exercises the skip path and the taint
/// filter is what holds it exact.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn merge_base_ignores_tainted_commit_graph_skips() {
    let ctx = in_memory_ctx(true);
    // Linear mainline c0..c19 (generations 0..19); s1 branches off c10
    // (generation 11): the best common ancestor of c19 and s1 is c10.
    let mut prev = ctx
        .commit(None, "Ann <ann@example.com>", vec![add_ch("f.txt", b"0\n")])
        .await;
    let mut chain = vec![prev.clone()];
    for i in 1..=19u32 {
        prev = ctx
            .commit(
                Some(&prev),
                "Ann <ann@example.com>",
                vec![ch("f.txt", format!("{i}\n").as_bytes())],
            )
            .await;
        chain.push(prev.clone());
    }
    let (c10, c19) = (chain[10].clone(), chain[19].clone());
    let s1 = ctx
        .commit(
            Some(&c10),
            "Ann <ann@example.com>",
            vec![add_ch("s.txt", b"s\n")],
        )
        .await;

    // Poison c10 on the commit-graph kind BEFORE the descendants derive;
    // the queue then derives c11..c19 and s1 done-with-taint.
    ctx.force_poison(naatre_server::derived::KIND_COMMIT_GRAPH, &c10, false);
    ctx.drain().await;
    let graph_row = |cs: &str| -> (String, i64) {
        ctx.db()
            .query_row_map(
                &naatre_store::Sql::plain(
                    "SELECT s.state, s.tainted FROM derive_state s \
                 JOIN derive_kinds k ON k.id = s.kind \
                 WHERE k.name = ?1 AND s.cs = ?2",
                ),
                &naatre_store::pv![
                    naatre_server::derived::KIND_COMMIT_GRAPH,
                    id_of(cs).0.as_slice()
                ],
                |r| Ok((r.get::<String>(0).unwrap(), r.get::<i64>(1).unwrap())),
            )
            .unwrap()
    };
    assert_eq!(
        graph_row(&c19),
        ("done".to_string(), 1),
        "the endpoint derived done-with-taint over the poison"
    );

    // Natural leg: every row below the poison is tainted, the skips are
    // degraded — the route still equals the brute force across it.
    let resp = ctx
        .get(&format!("/v1/repos/{REPO}/merge-base?a={c19}&b={s1}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let natural = resp.body["merge_base"].as_str().expect("base");
    assert_eq!(natural, c10, "the true base across the poison");
    assert_eq!(
        cs_gen_db_from(&ctx.db(), natural),
        brute_force_merge_base_gen(&ctx.db(), &c19, &s1),
        "route equals brute force"
    );

    // Crafted leg: a tainted-done row carrying a WRONG but usable skip
    // (target s1, pure, generation at the lower side — exactly the jump
    // that would steer the climb onto the WRONG side). The answer must
    // stay c10: tainted skips are not used.
    let bogus = naatre_server::derived::encode_skip_entries(&[naatre_server::derived::SkipEntry {
        target: id_of(&s1),
        hop: 9,
        generation: 11,
        pure: true,
    }]);
    ctx.db()
        .execute(
            &naatre_store::Sql::plain(
                "UPDATE derive_state SET output = ?1 \
             WHERE kind = (SELECT id FROM derive_kinds WHERE name = ?2) AND cs = ?3",
            ),
            &naatre_store::pv![
                bogus,
                naatre_server::derived::KIND_COMMIT_GRAPH,
                id_of(&c19).0.as_slice()
            ],
        )
        .unwrap();
    let resp = ctx
        .get(&format!("/v1/repos/{REPO}/merge-base?a={c19}&b={s1}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(
        resp.body["merge_base"],
        json!(c10),
        "the tainted skip is not used: the answer cannot move"
    );

    // Control: flip ONLY the taint bit off — same bogus bytes, now a
    // clean-looking row the acceleration is allowed to trust — and the
    // bogus skip steers the climb to s1. (A corrupt-but-clean row is
    // undetectable by design; this leg documents that the pin
    // discriminates: the taint filter, not the output bytes, is what the
    // fix reads.)
    ctx.db()
        .execute(
            &naatre_store::Sql::plain(
                "UPDATE derive_state SET tainted = 0 \
             WHERE kind = (SELECT id FROM derive_kinds WHERE name = ?1) AND cs = ?2",
            ),
            &naatre_store::pv![
                naatre_server::derived::KIND_COMMIT_GRAPH,
                id_of(&c19).0.as_slice()
            ],
        )
        .unwrap();
    let resp = ctx
        .get(&format!("/v1/repos/{REPO}/merge-base?a={c19}&b={s1}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(
        resp.body["merge_base"],
        json!(s1),
        "the same skip, untainted, IS taken — the pin discriminates"
    );
}

// ---- 6. binary + empty blame --------------------------------------------------

/// A binary blob blames as the structured empty result: HTTP 200, empty
/// lines, `binary: true` with the stable `blame_binary` code. An empty
/// file blames as empty lines with `binary: false`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn binary_and_empty_files_blame_structured() {
    let ctx = in_memory_ctx(true);
    let bin = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![Ch {
                path: "blob.bin",
                op: "add",
                content: Some(vec![0x00, 0xFF, 0xFE, 0x01]),
                exec: false,
            }],
        )
        .await;
    let empty = ctx
        .commit(
            Some(&bin),
            "Ann <ann@example.com>",
            vec![add_ch("empty.txt", b"")],
        )
        .await;
    ctx.drain().await;

    let resp = ctx.blame(&bin, "blob.bin").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["binary"], json!(true));
    assert_eq!(resp.body["binary_code"], json!("blame_binary"));
    assert_eq!(resp.body["lines"], json!([]));

    let resp = ctx.blame(&empty, "empty.txt").await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["binary"], json!(false));
    assert_eq!(resp.body["binary_code"], json!(null));
    assert_eq!(resp.body["lines"], json!([]));

    // A nonexistent path is the plain 404, not an empty blame.
    let resp = ctx.blame(&bin, "nope.txt").await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
}

// ---- 6b. the walk-budget pricing + read caps -----------------------------------

/// A tree-walking deriver probe: per file-change row, resolves the path in
/// the parent's tree through the budgeted lookup — the exact contract the
/// blame deriver follows. Its tiny budget makes a DEEP path trip the
/// budget in one pass (each component is one charge).
struct DepthWalkProbe {
    budget: u64,
}

impl Deriver for DepthWalkProbe {
    fn kind(&self) -> &'static str {
        "deep-walk-probe"
    }

    fn walk_budget(&self) -> u64 {
        self.budget
    }

    fn derive(
        &self,
        ctx: &DeriveCtx,
        conn: &naatre_store::Db,
        repo_id: i64,
        cs: Id,
        _parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        let sql_fail =
            |e: naatre_store::DbError| DeriveFailure::Fail(PoisonReason::new(e.to_string()));
        let parent_root = match naatre_server::db::changeset_graph(conn, repo_id, &cs)
            .map_err(sql_fail)?
        {
            Some(graph) => match graph.parents.first() {
                Some(p1) => naatre_server::db::changeset_graph(conn, repo_id, p1)
                    .map_err(sql_fail)?
                    .ok_or_else(|| DeriveFailure::Fail(PoisonReason::new("parent row vanished")))?
                    .root_tree_id
                    .map(|b| Id(b.try_into().expect("tree id is 32 bytes"))),
                None => None,
            },
            None => return Err(DeriveFailure::Fail(PoisonReason::new("cs row vanished"))),
        };
        let changes = naatre_server::db::file_changes_list(conn, repo_id, &cs).map_err(sql_fail)?;
        for c in &changes {
            ctx.charge(1)?;
            if let Some(root) = &parent_root {
                naatre_server::db::tree_lookup_path_charged(conn, ctx, root, &c.path)?;
            }
        }
        Ok(DerivedOut::empty())
    }
}

/// The walk budget prices COMPONENT probes (the writer-role DoS fix): a
/// deep path (41 components) costs 42 charges through the budgeted lookup,
/// so a 1-row changeset trips even a small budget mid-derive — quarantined
/// in one pass, with the report rowed, while the create is untouched (the
/// kind is deferred) and the REAL blame kind (budget sized to the
/// component pricing) derives the same shape done. The root parent, whose
/// derive walks no parent tree, derives done — the pin discriminates the
/// charged walk, not the shape alone.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn deep_path_changeset_poisons_on_the_charged_walk_budget() {
    let dir = tempfile::tempdir().unwrap();
    let data_path = dir.path().join("data");
    let dd = DataDir::create(&data_path).unwrap();
    std::fs::write(dd.db_path(), b"").unwrap();
    let probe = DepthWalkProbe { budget: 30 };
    let ctx = Ctx {
        _dir: Some(dir),
        ..open_ctx_with(&data_path, true, |state| {
            state.register_deriver(Arc::new(probe)).unwrap();
        })
    };

    let deep: &'static str = {
        let mut p = String::new();
        for _ in 0..40 {
            p.push_str("a/");
        }
        p.push_str("deep.txt");
        Box::leak(p.into_boxed_str())
    };
    let parent = ctx
        .commit(None, "Ann <ann@example.com>", vec![add_ch(deep, b"deep\n")])
        .await;
    let child = ctx
        .commit(
            Some(&parent),
            "Bob <bob@example.com>",
            vec![ch(deep, b"deep2\n")],
        )
        .await;
    ctx.drain().await;

    let probe_state = |cs: &str| {
        ctx.db()
            .query_row_map(
                &naatre_store::Sql::plain(
                    "SELECT s.state FROM derive_state s \
                 JOIN derive_kinds k ON k.id = s.kind \
                 WHERE k.name = 'deep-walk-probe' AND s.cs = ?1",
                ),
                &naatre_store::pv![id_of(cs).0.as_slice()],
                |r| r.get::<String>(0),
            )
            .unwrap()
    };
    assert_eq!(probe_state(&parent), "done", "the root derive passes");
    assert_eq!(
        probe_state(&child),
        "poison",
        "the deep derive tripped the charged budget"
    );
    let (reports, reason): (i64, String) = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT COUNT(*), MAX(r.reason) FROM derive_reports r \
                 JOIN derive_kinds k ON k.id = r.kind \
                 WHERE k.name = 'deep-walk-probe'",
            ),
            &naatre_store::pv![],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    assert_eq!(reports, 1, "quarantined exactly once");
    assert!(reason.contains("walk budget"), "{reason:?}");
    // The real blame kind, whose budget is sized to the component pricing,
    // derives the SAME shape without tripping: the pin discriminates —
    // the probe's budget bound it, not the shape alone.
    let blame_state: String = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT s.state FROM derive_state s \
             JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = 'blame' AND s.cs = ?1",
            ),
            &naatre_store::pv![id_of(&child).0.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(blame_state, "done");
}

/// The blame kind's OWN budget binds its honest pricing — the QA pin the
/// probe kinds cannot see: a changeset whose priced walk (1 per row + 1
/// per path component) exceeds blame's 1,000,000 budget quarantines with
/// the walk-budget reason. 3,400 rows at depth 300 cost 3,400 × 301 >
/// 1,000,000; the root parent (no parent-tree walk) derives done. If the
/// deriver's charging or its budget reverts to anything that lets a
/// million-probe walk run unpriced, this pin fails. (The depth is bounded
/// by the create path's per-level tree recursion, not by the budget.)
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn blame_kind_own_budget_quarantines_a_priced_deep_shape() {
    let ctx = in_memory_ctx(true);
    // 256 components: AT the grammar's component cap — the deepest shape
    // the grammar admits, so this is the honest worst case per row
    // (1 charge per row + 256 probe charges). 4,200 rows x 257 charges
    // ≈ 1.08M exceeds blame's own 1,000,000 budget; the pin fails if the
    // budget is raised past that without re-pricing this shape.
    const DEPTH: usize = 256;
    const ROWS: usize = 4_200;
    let deep = |i: usize| -> &'static str {
        let mut p = String::new();
        for _ in 0..DEPTH - 1 {
            p.push_str("a/");
        }
        p.push_str(&format!("f{i:04}.txt"));
        Box::leak(p.into_boxed_str())
    };
    let parent = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            (0..ROWS).map(|i| add_ch(deep(i), b"x\n")).collect(),
        )
        .await;
    ctx.drain().await;
    let child = ctx
        .commit(
            Some(&parent),
            "Bob <bob@example.com>",
            (0..ROWS).map(|i| ch(deep(i), b"y\n")).collect(),
        )
        .await;
    ctx.drain().await;

    let blame_state = |cs: &str| {
        ctx.db()
            .query_row_map(
                &naatre_store::Sql::plain(
                    "SELECT s.state FROM derive_state s \
                 JOIN derive_kinds k ON k.id = s.kind \
                 WHERE k.name = 'blame' AND s.cs = ?1",
                ),
                &naatre_store::pv![id_of(cs).0.as_slice()],
                |r| r.get::<String>(0),
            )
            .unwrap()
    };
    assert_eq!(blame_state(&parent), "done", "the root derive passes");
    assert_eq!(
        blame_state(&child),
        "poison",
        "the priced walk exceeded blame's own budget"
    );
    let (_, reason): (i64, String) = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT COUNT(*), MAX(r.reason) FROM derive_reports r \
                 JOIN derive_kinds k ON k.id = r.kind \
                 WHERE k.name = 'blame'",
            ),
            &naatre_store::pv![],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    assert!(reason.contains("walk budget"), "{reason:?}");
}

/// The pricing is width-INDEPENDENT: a 12,000-entry directory (the crafted
/// false-quarantine shape) touched by 100 rows costs 100 × 3 charges under
/// the per-component point-probe pricing and derives done — under the
/// removed per-entry-row pricing the same shape cost 1.2 million charges
/// and would have quarantined, so the pin fails if the width charge
/// returns. The blame read stays exact (not degraded).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn wide_directory_touch_derives_done_under_component_pricing() {
    let ctx = in_memory_ctx(true);
    const WIDE: usize = 12_000;
    const ROWS: usize = 100;
    let wide = |i: usize| -> &'static str { Box::leak(format!("w/f{i:05}.txt").into_boxed_str()) };
    let parent = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            (0..WIDE).map(|i| add_ch(wide(i), b"x\n")).collect(),
        )
        .await;
    ctx.drain().await;
    let child = ctx
        .commit(
            Some(&parent),
            "Bob <bob@example.com>",
            (0..ROWS).map(|i| ch(wide(i), b"y\n")).collect(),
        )
        .await;
    ctx.drain().await;

    let blame_state: String = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT s.state FROM derive_state s \
             JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = 'blame' AND s.cs = ?1",
            ),
            &naatre_store::pv![id_of(&child).0.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(blame_state, "done", "the wide directory touch derives done");

    let resp = ctx.blame(&child, "w/f00000.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(false));
    assert_eq!(resp.body["lines"].as_array().unwrap().len(), 1);
}

/// The line cap fires BEFORE the assembler's per-line allocation, both
/// directions: `BLAME_MAX_LINES + 1` lines is a 503 `blame_too_large`;
/// `BLAME_MAX_LINES` exactly is a full 200 with every line present (no
/// silent truncation).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn blame_line_cap_trips_at_the_boundary_not_below_it() {
    let ctx = in_memory_ctx(true);
    let cap = naatre_server::derived::BLAME_MAX_LINES;

    let over_txt = "a\n".repeat(cap + 1);
    let over = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("over.txt", over_txt.as_bytes())],
        )
        .await;
    ctx.drain().await;
    let resp = ctx.blame(&over, "over.txt").await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], json!("blame_too_large"));

    let under_txt = "a\n".repeat(cap);
    let under = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("under.txt", under_txt.as_bytes())],
        )
        .await;
    ctx.drain().await;
    let resp = ctx.blame(&under, "under.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["binary"], json!(false));
    assert_eq!(
        resp.body["lines"].as_array().unwrap().len(),
        cap,
        "the under-cap blame is complete, never truncated"
    );
}

/// The serialized-response cap, both directions: a line-count-compliant
/// shape whose ENCODED JSON exceeds `BLAME_MAX_RESPONSE_BYTES` is a 503
/// `blame_too_large`; the same shape comfortably under it is a full 200.
/// Both shapes stay under the line cap — this pin trips the response cap,
/// not the line cap.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn blame_response_cap_trips_on_encoded_size_not_below_it() {
    let ctx = in_memory_ctx(true);

    // 190,000 lines of 80 bytes: a ~15 MiB file (admissible under the
    // 16 MiB blob cap), ~37 MB encoded (over the 32 MiB response cap).
    let over_line = format!("{}\n", "x".repeat(79));
    let over_txt = over_line.repeat(190_000);
    let over = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("over.txt", over_txt.as_bytes())],
        )
        .await;
    ctx.drain().await;
    let resp = ctx.blame(&over, "over.txt").await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], json!("blame_too_large"));

    // 120,000 lines of 80 bytes: ~9.6 MiB file, ~23 MB encoded (under).
    let under_txt = over_line.repeat(120_000);
    let under = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("under.txt", under_txt.as_bytes())],
        )
        .await;
    ctx.drain().await;
    let resp = ctx.blame(&under, "under.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(
        resp.body["lines"].as_array().unwrap().len(),
        120_000,
        "the under-cap blame is complete, never truncated"
    );
}

/// The streamed-blob-bytes cap, both directions, with the cap injected
/// small (the state-field pattern — 256 MiB of real loads is not a test):
/// a 100-byte file's single-add blame loads it twice (final + base) = 200
/// bytes, exactly AT the cap → full 200. The three-layer file loads
/// 100 + 110 + 120 = 330 > 200 → 503 `blame_too_large`. Both shapes sit
/// far under the line and response caps — this pin trips the bytes cap
/// alone.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn blame_blob_bytes_cap_trips_at_the_boundary_not_below_it() {
    let ctx = in_memory_ctx_with(true, |s| s.blame_max_blob_bytes = 200);

    let under = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("under.txt", &[b'a'; 100])],
        )
        .await;
    ctx.drain().await;
    let resp = ctx.blame(&under, "under.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(
        resp.body["lines"].as_array().unwrap().len(),
        1,
        "at-cap blame is complete, never truncated"
    );

    let base = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("over.txt", &[b'x'; 100])],
        )
        .await;
    let m1 = ctx
        .commit(
            Some(&base),
            "Bob <bob@example.com>",
            vec![ch("over.txt", &[b'x'; 110])],
        )
        .await;
    let tip = ctx
        .commit(
            Some(&m1),
            "Bob <bob@example.com>",
            vec![ch("over.txt", &[b'x'; 120])],
        )
        .await;
    ctx.drain().await;
    let resp = ctx.blame(&tip, "over.txt").await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], json!("blame_too_large"));
}

/// The binary short-circuit still PAYS the bytes charge: the final blob's
/// load happens before the UTF-8 check, so a binary blob over the (injected
/// small) cap is a 503 `blame_too_large` — not the structured binary
/// result; the same shape under the cap is the structured empty binary 200.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn blame_binary_short_circuit_still_charges_the_bytes_cap() {
    let ctx = in_memory_ctx_with(true, |s| s.blame_max_blob_bytes = 200);

    let over = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("big.bin", &[0xffu8; 300])],
        )
        .await;
    ctx.drain().await;
    let resp = ctx.blame(&over, "big.bin").await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], json!("blame_too_large"));

    let under = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("small.bin", &[0xffu8; 60])],
        )
        .await;
    ctx.drain().await;
    let resp = ctx.blame(&under, "small.bin").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["binary"], json!(true));
    assert_eq!(resp.body["binary_code"], json!("blame_binary"));
    assert_eq!(resp.body["lines"], json!([]));
}

/// A first-parent chain past the read cap degrades with `blame_too_large`
/// (retryable false, no Retry-After) — never a hang, never a silent
/// truncation. The chain is crafted in SQL: one real root commit carries
/// the tree; 10,002 synthetic single-parent descendants share its root
/// tree, each with a done blame row (no touches), so the walk does honest
/// point-query work per node until the cap fires.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn blame_chain_past_the_cap_degrades_not_hangs() {
    let ctx = in_memory_ctx(true);
    let c0 = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("f.txt", b"one\n")],
        )
        .await;
    let repo_id: i64 = ctx.count(&format!("SELECT id FROM repos WHERE name = '{REPO}'"));
    let root_tree: Vec<u8> = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT root_tree_id FROM changesets WHERE repo_id = ?1 AND id = ?2",
            ),
            &naatre_store::pv![repo_id, id_of(&c0).0.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    let blame_kind: i64 = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain("SELECT id FROM derive_kinds WHERE name = 'blame'"),
            &naatre_store::pv![],
            |r| r.get(0),
        )
        .unwrap();

    const OVER_CAP: i64 = 10_002;
    let conn = ctx.db();
    conn.execute_batch("BEGIN IMMEDIATE").unwrap();
    let mut prev = id_of(&c0).0;
    let mut tip = prev;
    for i in 1..=OVER_CAP {
        let mut id = [0u8; 32];
        id[..8].copy_from_slice(&(0xC5u64 << 32 | i as u64).to_be_bytes());
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO changesets(repo_id, id, canonical, author, message, \
             timestamp_ns, generation, principal, root_tree_id) \
             VALUES (?1, ?2, ?3, 'Ann', 'bulk', 0, ?4, 'm6b-test', ?5)",
            ),
            &naatre_store::pv![repo_id, id, b"bulk", i, &root_tree],
        )
        .unwrap();
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO changeset_parents(repo_id, cs, parent, ord) VALUES (?1, ?2, ?3, 0)",
            ),
            &naatre_store::pv![repo_id, id, &prev],
        )
        .unwrap();
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT OR IGNORE INTO derive_state(kind, repo_id, cs, state, attempts, output) \
             VALUES (?1, ?2, ?3, 'done', 0, ?4)",
            ),
            &naatre_store::pv![blame_kind, repo_id, id, b""],
        )
        .unwrap();
        prev = id;
        tip = id;
    }
    conn.execute_batch("COMMIT").unwrap();
    let tip_hex: String = tip.iter().map(|b| format!("{b:02x}")).collect();

    let resp = ctx.blame(&tip_hex, "f.txt").await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], "blame_too_large");
    assert_eq!(resp.body["error"]["retryable"], json!(false));
    assert!(
        resp.headers.get("retry-after").is_none(),
        "not retryable, so no Retry-After"
    );
}

/// The chain cap's under-leg, same crafting as the over-leg above: a
/// 9,999-node first-parent chain stays under the 10,000 cap — the walk
/// (9,999 synthetic nodes + the real root) reaches the root's add row and
/// answers 200 with the one true line, exact (degraded false). One more
/// synthetic node would trip the cap; this is the boundary's cheap side.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn blame_chain_under_the_cap_answers_in_full() {
    let ctx = in_memory_ctx(true);
    let c0 = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("f.txt", b"one\n")],
        )
        .await;
    // The walk terminates at c0's derived add row — it must exist.
    ctx.drain().await;
    let repo_id: i64 = ctx.count(&format!("SELECT id FROM repos WHERE name = '{REPO}'"));
    let root_tree: Vec<u8> = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT root_tree_id FROM changesets WHERE repo_id = ?1 AND id = ?2",
            ),
            &naatre_store::pv![repo_id, id_of(&c0).0.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    let blame_kind: i64 = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain("SELECT id FROM derive_kinds WHERE name = 'blame'"),
            &naatre_store::pv![],
            |r| r.get(0),
        )
        .unwrap();

    const UNDER_CAP: i64 = 9_999;
    let conn = ctx.db();
    conn.execute_batch("BEGIN IMMEDIATE").unwrap();
    let mut prev = id_of(&c0).0;
    let mut tip = prev;
    for i in 1..=UNDER_CAP {
        let mut id = [0u8; 32];
        id[..8].copy_from_slice(&(0xC6u64 << 32 | i as u64).to_be_bytes());
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO changesets(repo_id, id, canonical, author, message, \
             timestamp_ns, generation, principal, root_tree_id) \
             VALUES (?1, ?2, ?3, 'Ann', 'bulk', 0, ?4, 'm6b-test', ?5)",
            ),
            &naatre_store::pv![repo_id, id, b"bulk", i, &root_tree],
        )
        .unwrap();
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO changeset_parents(repo_id, cs, parent, ord) VALUES (?1, ?2, ?3, 0)",
            ),
            &naatre_store::pv![repo_id, id, &prev],
        )
        .unwrap();
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT OR IGNORE INTO derive_state(kind, repo_id, cs, state, attempts, output) \
             VALUES (?1, ?2, ?3, 'done', 0, ?4)",
            ),
            &naatre_store::pv![blame_kind, repo_id, id, b""],
        )
        .unwrap();
        prev = id;
        tip = id;
    }
    conn.execute_batch("COMMIT").unwrap();
    let tip_hex: String = tip.iter().map(|b| format!("{b:02x}")).collect();

    let resp = ctx.blame(&tip_hex, "f.txt").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(false));
    assert_eq!(
        resp.body["lines"].as_array().unwrap().len(),
        1,
        "the under-cap blame is the full, exact answer"
    );
}

/// A corrupt-but-untainted SELF-REFERENTIAL skip row cannot hang the
/// merge-base (the unbounded-climb fix): the non-descending jump is
/// refused, the climb falls back to the plain first-parent step, and the
/// answer is still the true base.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn merge_base_survives_a_self_referential_skip_row() {
    let ctx = in_memory_ctx(true);
    let c0 = ctx
        .commit(None, "Ann <ann@example.com>", vec![add_ch("f.txt", b"0\n")])
        .await;
    let mut prev = c0.clone();
    let mut chain = vec![c0.clone()];
    for i in 1..=6u32 {
        prev = ctx
            .commit(
                Some(&prev),
                "Ann <ann@example.com>",
                vec![ch("f.txt", format!("{i}\n").as_bytes())],
            )
            .await;
        chain.push(prev.clone());
    }
    let tip = chain[6].clone();
    ctx.drain().await;

    // Overwrite the TIP's commit-graph output with one clean-looking
    // self-referential entry (target = itself, max hop so the climb picks
    // it first). Pre-fix this spun the climb forever.
    let bogus = naatre_server::derived::encode_skip_entries(&[naatre_server::derived::SkipEntry {
        target: id_of(&tip),
        hop: 15,
        generation: 6,
        pure: true,
    }]);
    ctx.db()
        .execute(
            &naatre_store::Sql::plain(
                "UPDATE derive_state SET output = ?1 \
             WHERE kind = (SELECT id FROM derive_kinds WHERE name = ?2) AND cs = ?3",
            ),
            &naatre_store::pv![
                bogus,
                naatre_server::derived::KIND_COMMIT_GRAPH,
                id_of(&tip).0.as_slice()
            ],
        )
        .unwrap();

    let resp = ctx
        .get(&format!(
            "/v1/repos/{REPO}/merge-base?a={tip}&b={}",
            chain[0]
        ))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(
        resp.body["merge_base"],
        json!(chain[0]),
        "the refused jump fell back to first-parent steps; the base is exact"
    );
}

// ---- 7. the backfill pins ------------------------------------------------------

/// The multi-batch pin: with the batch size injected to 4, twelve ROWLESS
/// changesets (the boot-backfill shape, crafted in SQL like the chain pins)
/// force the loop through at least three batches — and the completion
/// marker lands only over a FULL anti-join. A one-batch loop exit or a
/// dropped batch cursor would set `derive_backfill_done` with rows
/// unrowed (reads answer 200 over a hole); the marker-honesty probe
/// ([`naatre_server::derive::backfill`]'s final
/// `derive_backfill_rowless_exists` check) fails the backfill instead, so
/// the marker-set assertion below cannot pass over a hole.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn backfill_marker_lands_only_over_a_full_anti_join() {
    let ctx = in_memory_ctx_with(true, |s| s.derive_backfill_batch_size = 4);
    let c0 = ctx
        .commit(None, "Ann <ann@example.com>", vec![add_ch("f.txt", b"0\n")])
        .await;
    let repo_id: i64 = ctx.count(&format!("SELECT id FROM repos WHERE name = '{REPO}'"));
    let root_tree: Vec<u8> = ctx
        .db()
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT root_tree_id FROM changesets WHERE repo_id = ?1 AND id = ?2",
            ),
            &naatre_store::pv![repo_id, id_of(&c0).0.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    // Twelve changesets with NO derive rows, in distinct generations.
    const ROWLESS: i64 = 12;
    {
        let conn = ctx.db();
        conn.execute_batch("BEGIN IMMEDIATE").unwrap();
        for i in 1..=ROWLESS {
            let mut id = [0u8; 32];
            id[..8].copy_from_slice(&(0xC7u64 << 32 | i as u64).to_be_bytes());
            conn.execute(
                &naatre_store::Sql::plain(
                    "INSERT INTO changesets(repo_id, id, canonical, author, message, \
                 timestamp_ns, generation, principal, root_tree_id) \
                 VALUES (?1, ?2, ?3, 'Ann', 'bulk', 0, ?4, 'm6b-test', ?5)",
                ),
                &naatre_store::pv![repo_id, id, b"bulk", 1_000 + i, &root_tree],
            )
            .unwrap();
        }
        conn.execute_batch("COMMIT").unwrap();
    }
    let rowless_before: i64 = ctx.count(
        "SELECT COUNT(*) FROM changesets c WHERE NOT EXISTS (\
         SELECT 1 FROM derive_state d WHERE d.repo_id = c.repo_id AND d.cs = c.id)",
    );
    assert!(rowless_before >= ROWLESS, "the unrowed window exists");

    naatre_server::derive::backfill(&ctx.state)
        .await
        .expect("the backfill completes over the injected batch size");

    // Every production kind's marker is set — and the anti-join is empty
    // for every kind: no changeset anywhere lacks its row.
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM meta WHERE key LIKE 'derive_backfill_done:%'"),
        3,
        "every kind's completion marker set"
    );
    for kind in [
        naatre_server::derived::KIND_BLAME,
        naatre_server::derived::KIND_PATH_HISTORY,
        naatre_server::derived::KIND_COMMIT_GRAPH,
    ] {
        assert_eq!(
            ctx.count(&format!(
                "SELECT COUNT(*) FROM changesets c WHERE NOT EXISTS (\
                 SELECT 1 FROM derive_state d JOIN derive_kinds k ON k.id = d.kind \
                 WHERE k.name = '{kind}' AND d.repo_id = c.repo_id AND d.cs = c.id)"
            )),
            0,
            "{kind}: unrowed changesets survived the backfill"
        );
    }
}

/// Pin 1: bulk import. `naatre import` runs with NO registered kinds; on
/// the serving side (`naatre serve`'s boot-time backfill over the SAME
/// queue), every imported changeset derives for every kind — no re-import,
/// no quarantine, and the blame / path-history / commit-graph surfaces
/// actually hold the imported data.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn backfill_after_bulk_import() {
    if !git_works() {
        return;
    }
    let mut git = GitFixture::with_merge("m6bbackfill");
    git.write("late.txt", b"late\n");
    git.commit("late");

    let dir_holder = import_fixture(&git);
    let data = dir_holder.path().join("data");
    let imported: i64 = {
        naatre_store::Db::Sqlite(Connection::open(data.join("naatre.db")).unwrap())
            .query_row_map(
                &naatre_store::Sql::plain("SELECT COUNT(*) FROM changesets"),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .unwrap()
    };
    assert!(imported >= 4, "root, main edit, side, merge: {imported}");

    // The M5-era shape on disk: changesets, and NOT ONE derive row.
    {
        let conn = naatre_store::Db::Sqlite(Connection::open(data.join("naatre.db")).unwrap());
        assert_eq!(
            conn.query_row_map(
                &naatre_store::Sql::plain("SELECT COUNT(*) FROM derive_state"),
                &naatre_store::pv![],
                |r| r.get::<i64>(0),
            )
            .unwrap(),
            0
        );
    }

    // Serve (the CLI's production registration): boot-time backfill +
    // background queue drain.
    let ctx = open_ctx(&data, true);
    serve_until_quiet(Arc::clone(&ctx.state)).await;
    wait_done(&ctx, 3 * imported).await;

    let conn = ctx.db();
    assert_eq!(
        conn.query_row_map(
            &naatre_store::Sql::plain("SELECT COUNT(*) FROM derive_reports"),
            &naatre_store::pv![],
            |r| r.get::<i64>(0)
        )
        .unwrap(),
        0,
        "zero poison after backfill"
    );
    let blame_rows: i64 = conn
        .query_row_map(
            &naatre_store::Sql::plain("SELECT COUNT(*) FROM blame_state"),
            &naatre_store::pv![],
            |r| r.get(0),
        )
        .unwrap();
    assert!(
        blame_rows >= imported,
        "diff-state rows for the imports: {blame_rows}"
    );
    let history_rows: i64 = conn
        .query_row_map(
            &naatre_store::Sql::plain("SELECT COUNT(*) FROM path_history"),
            &naatre_store::pv![],
            |r| r.get(0),
        )
        .unwrap();
    assert!(
        history_rows >= imported,
        "index rows for the imports: {history_rows}"
    );
    let graph_rows: i64 = conn
        .query_row_map(
            &naatre_store::Sql::plain(
                "SELECT COUNT(*) FROM derive_state s JOIN derive_kinds k ON k.id = s.kind \
             WHERE k.name = 'commit_graph' AND s.state = 'done'",
            ),
            &naatre_store::pv![],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(graph_rows, imported, "commit_graph output on every node");

    // The purity flags exist and are well-formed: every node's entry 0 is
    // the pure hop-1 first-parent entry; a node whose FIRST parent is the
    // merge carries an impure higher entry (its span contains the branch).
    let outputs: Vec<(Vec<u8>, Vec<u8>)> = conn
        .query_all_map(
            &naatre_store::Sql::plain(
                "SELECT s.cs, s.output FROM derive_state s \
                 JOIN derive_kinds k ON k.id = s.kind \
                 WHERE k.name = 'commit_graph' AND s.state = 'done' AND s.output IS NOT NULL",
            ),
            &naatre_store::pv![],
            |r| Ok((r.get::<Vec<u8>>(0)?, r.get::<Vec<u8>>(1)?)),
        )
        .unwrap();
    let mut pure_entry0 = 0;
    let mut impure_any = 0;
    for (_, output) in &outputs {
        if output.is_empty() {
            continue; // the root's empty handoff marker
        }
        let entries = naatre_server::derived::decode_skip_entries(output).expect("decodable");
        if let Some(e0) = entries.first() {
            assert_eq!(e0.hop, 1, "entry 0 is hop 1");
            assert!(e0.pure, "entry 0 spans nothing");
            pure_entry0 += 1;
        }
        if entries.iter().skip(1).any(|e| !e.pure) {
            impure_any += 1;
        }
        // Hops strictly increase (chained doubling: 1, 3, 7, …).
        for w in entries.windows(2) {
            assert!(w[1].hop > w[0].hop);
        }
    }
    assert_eq!(pure_entry0 as i64, imported - 1, "all but the root");
    assert!(impure_any >= 1, "the merge's children flag impure spans");
}

/// Pin 2: the in-place upgrade. The database is an M5-era one — schema 3,
/// changesets in it, not a single derived-data table or row — exactly what
/// a pre-M6 server leaves behind. Opening it with the current binary
/// migrates in place and `serve`'s boot-time backfill derives everything
/// through the SAME queue. No re-import, no special path.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn backfill_after_in_place_upgrade_of_pre_derive_db() {
    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("data");
    // Build the pre-derive DB with the CURRENT binary but an EMPTY
    // registry (creates enqueue nothing).
    {
        let ctx = build_ctx(dir.path(), false);
        let mut parent: Option<String> = None;
        let mut seen: Vec<&str> = Vec::new();
        for (path, content) in [
            ("f.txt", "v1\n"),
            ("g.txt", "g1\n"),
            ("f.txt", "v2\n"),
            ("g.txt", "g2\n"),
        ] {
            ctx.put_blob(content.as_bytes()).await;
            let op = if seen.contains(&path) { "mod" } else { "add" };
            seen.push(path);
            parent = Some(
                ctx.commit(
                    parent.as_deref(),
                    "Ann <ann@example.com>",
                    vec![Ch {
                        path,
                        op,
                        content: Some(content.as_bytes().to_vec()),
                        exec: false,
                    }],
                )
                .await,
            );
        }
    }
    // Rewind to the M5-era shape: migrations 4/5 only ADDED the derived
    // tables, so dropping them and stamping version 3 reproduces that
    // state exactly.
    let db_path = data.join("naatre.db");
    {
        let conn = naatre_store::Db::Sqlite(Connection::open(&db_path).unwrap());
        conn.execute_batch(
            "DROP TABLE derive_state; DROP TABLE derive_reports; DROP TABLE derive_kinds;
             DROP TABLE blame_state; DROP TABLE path_history;
             DROP INDEX IF EXISTS idx_changesets_generation;
             UPDATE meta SET value = '3' WHERE key = 'schema_version';",
        )
        .unwrap();
        let version: String = conn
            .query_row_map(
                &naatre_store::Sql::plain("SELECT value FROM meta WHERE key = 'schema_version'"),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(version, "3", "now an M5-era database");
        let cs: i64 = conn
            .query_row_map(
                &naatre_store::Sql::plain("SELECT COUNT(*) FROM changesets"),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(cs, 4, "the changesets survived");
    }

    // Open with the current binary: migrations run in place, kinds
    // register, the boot-time backfill enqueues everything, the SAME
    // queue derives it.
    let ctx = open_ctx(&data, true);
    {
        let conn = naatre_store::Db::Sqlite(Connection::open(&db_path).unwrap());
        let version: String = conn
            .query_row_map(
                &naatre_store::Sql::plain("SELECT value FROM meta WHERE key = 'schema_version'"),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(
            version,
            naatre_store::MAX_SUPPORTED.to_string(),
            "migrated in place"
        );
    }
    serve_until_quiet(Arc::clone(&ctx.state)).await;
    wait_done(&ctx, 12).await;
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_reports"),
        0,
        "zero poison after the upgrade backfill"
    );
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_state WHERE state = 'done'"),
        3 * 4,
        "every kind derived every changeset"
    );

    // And the upgraded data serves: the derived ?path= page lists all
    // four touches of f.txt, newest first.
    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(false));
    assert_eq!(resp.body["entries"].as_array().unwrap().len(), 2);
}

// ---- 7b. the writer_busy abort A/B ------------------------------------------
//
// The deterministic form of the busy collision behind the queue_lifecycle
// flake (an ATTEMPTS-ACCOUNTING failure: all four nodes derived, but one
// burned an attempt via lease expiry after a `writer_busy` collision —
// see the m6a pin): a queue pass aborted by `writer_busy` — the test's
// own API writes colliding with the queue's writes on the single writer
// slot. The hook (`ServerState::derive_test_busy_abort`) produces that
// abort ONCE, at a chosen changeset's mid-derivation write, so the lease
// handling can be A/B-tested without any timing race.

/// The pre-fix5 `wait_done`, restored VERBATIM (from 6fc556c) for the A/B:
/// progress-based, NO clock advance — a lease lost mid-pass can never
/// expire on a frozen clock, so a held lease is a permanent wedge and the
/// frozen snapshot trips the stall assert. The current [`wait_done`]
/// advances the clock (+50 ms per poll) precisely so the expiry sweep can
/// recover a lost lease on the queue's clock; this copy exists to prove
/// the busy-abort lease release makes even the frozen helper safe.
async fn wait_done_frozen_clock(state: &AppState, min_done: i64) {
    let mut stalled = 0u32;
    let mut last = None;
    loop {
        let snap = {
            let state = Arc::clone(state);
            state
                .read(move |conn| {
                    let done: i64 = conn
                        .query_row_map(
                            &naatre_store::Sql::plain(
                                "SELECT COUNT(*) FROM derive_state WHERE state = 'done'",
                            ),
                            &naatre_store::pv![],
                            |r| r.get(0),
                        )
                        .unwrap();
                    let pending: i64 = conn
                        .query_row_map(
                            &naatre_store::Sql::plain(
                                "SELECT COUNT(*) FROM derive_state \
                             WHERE state IN ('pending','deriving')",
                            ),
                            &naatre_store::pv![],
                            |r| r.get(0),
                        )
                        .unwrap();
                    Ok((done, pending))
                })
                .await
                .unwrap()
        };
        if snap.1 == 0 && snap.0 >= min_done {
            return;
        }
        if last == Some(snap) {
            stalled += 1;
        } else {
            stalled = 0;
        }
        last = Some(snap);
        assert!(
            stalled < 300,
            "derive backlog stalled: done={} pending={}",
            snap.0,
            snap.1
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

/// The upgrade pin's scenario at A/B scale (three chained commits instead
/// of four two-file ones — the smallest shape that shows the stall:
/// aborting the root's derivation wedges its TWO descendants behind a
/// 'deriving' row, the parents-first pick never passing a deriving
/// parent), built and rewound exactly like
/// [`backfill_after_in_place_upgrade_of_pre_derive_db`]: registry-less
/// creates, schema stamped back to the M5 era, then booted with the
/// production kinds. The one-shot writer_busy hook is armed on the ROOT
/// changeset — the queue's first derive write of the boot (the
/// first-registered kind's oldest row). Returns (ctx, root, mid, leaf).
async fn upgrade_ab_ctx(dir: &Path) -> (Ctx, String, String, String) {
    // Registry-less creates: the changesets land with no derive rows.
    let (root, mid, _leaf) = {
        let ctx = build_ctx(dir, false);
        let mut parent: Option<String> = None;
        let mut seen: Vec<&str> = Vec::new();
        let mut ids = Vec::new();
        for (path, content) in [("f.txt", "v1\n"), ("f.txt", "v2\n"), ("f.txt", "v3\n")] {
            let op = if seen.contains(&path) { "mod" } else { "add" };
            seen.push(path);
            parent = Some(
                ctx.commit(
                    parent.as_deref(),
                    "Ann <ann@example.com>",
                    vec![Ch {
                        path,
                        op,
                        content: Some(content.as_bytes().to_vec()),
                        exec: false,
                    }],
                )
                .await,
            );
            ids.push(parent.clone().unwrap());
        }
        (ids[0].clone(), ids[1].clone(), ids[2].clone())
    };
    // Rewind to the M5-era shape (the upgrade pin's exact stamp).
    let data = dir.join("data");
    {
        let conn = Connection::open(data.join("naatre.db")).unwrap();
        conn.execute_batch(
            "DROP TABLE derive_state; DROP TABLE derive_reports; DROP TABLE derive_kinds;
             DROP TABLE blame_state; DROP TABLE path_history;
             DROP INDEX IF EXISTS idx_changesets_generation;
             UPDATE meta SET value = '3' WHERE key = 'schema_version';",
        )
        .unwrap();
    }
    // Boot with the production kinds; arm the hook on the root BEFORE the
    // backfill/queue tasks can derive anything.
    let ctx = open_ctx(&data, true);
    *ctx.state.derive_test_busy_abort.lock().unwrap() = Some(BusyAbort::Derive(id_of(&root)));
    (ctx, root, mid, _leaf)
}

/// A/B leg OLD: the same one-shot writer_busy abort under the OLD wait
/// helper (frozen clock). The held lease this setup produces is the same
/// root cause as the flake's burned attempt, taken to its frozen-clock
/// limit: pre-fix, the aborted pass held blame's root lease to expiry —
/// on a real (advancing) clock the sweep expires it and burns an attempt
/// (the flake); on this FROZEN clock it can never expire at all, so the
/// two descendants stay blocked behind the 'deriving' row forever
/// (observed pre-fix: `derive backlog stalled: done=6 pending=3`, i.e.
/// pending=2 + 1 deriving). With the fix the pass RELEASES the aborted
/// lease straight back to pending (contention is not a derivation
/// failure), the next tick re-derives the root, and the upgrade completes
/// on a helper that cannot wait a lease out — no attempt burned, nothing
/// in the stall shape left to recover from.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn upgrade_writer_busy_abort_completes_under_the_old_frozen_clock_helper() {
    let dir = tempfile::tempdir().unwrap();
    let (ctx, _root, _mid, _leaf) = upgrade_ab_ctx(dir.path()).await;
    serve_until_quiet(Arc::clone(&ctx.state)).await;
    wait_done_frozen_clock(&ctx.state, 9).await;
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_state WHERE state = 'done'"),
        9,
        "every kind derived every changeset"
    );
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_state WHERE attempts != 0"),
        0,
        "contention burns no attempt"
    );
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_reports"),
        0,
        "zero poison after the aborted pass"
    );
}

/// A/B leg NEW: the same abort under the current [`wait_done`] (advancing
/// clock). Even pre-fix this recovered — the +50 ms polls let the sweep
/// expire the held lease on the queue's clock — but the recovery cost a
/// full lease period and burned one ATTEMPT on pure contention (the
/// pre-fix run of this test: done=9 reached, then `attempts != 0` = 1).
/// With the fix the row is back to pending before the first poll and no
/// attempt burns at all.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn upgrade_writer_busy_abort_recovers_under_the_advancing_clock_helper() {
    let dir = tempfile::tempdir().unwrap();
    let (ctx, _root, _mid, _leaf) = upgrade_ab_ctx(dir.path()).await;
    serve_until_quiet(Arc::clone(&ctx.state)).await;
    wait_done(&ctx, 9).await;
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_state WHERE state = 'done'"),
        9,
        "every kind derived every changeset"
    );
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_state WHERE attempts != 0"),
        0,
        "contention burns no attempt"
    );
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_reports"),
        0,
        "zero poison after the aborted pass"
    );
}

/// Pin 3: the UNROWED window. The kinds are registered, the changesets
/// exist, and NOT ONE derive row does — the creates ran registry-less
/// (the import/upgrade shape, so create-time enqueue wrote nothing) and
/// the boot backfill is held (`derive_backfill_enabled = false`). A
/// changeset with no row counts as pending exactly because the index
/// provably holds nothing for it: ?path= and blame must answer 503
/// `derivation_pending`, never an empty-as-if-no-history page.
/// (Regression pin: `derive_any_pending` saw only pending/deriving rows,
/// so `?path=` answered an empty 200 over the missing index.)
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn unrowed_backlog_503s_never_empty_as_if_no_history() {
    let dir = tempfile::tempdir().unwrap();
    // Registry-less creates: the changesets land with no derive rows.
    let (c1, c2, data_path) = {
        let ctx = build_ctx(dir.path(), false);
        let c1 = ctx
            .commit(
                None,
                "Ann <ann@example.com>",
                vec![add_ch("f.txt", b"one\n")],
            )
            .await;
        let c2 = ctx
            .commit(
                Some(&c1),
                "Ann <ann@example.com>",
                vec![ch("f.txt", b"one\ntwo\n")],
            )
            .await;
        (c1, c2, dir.path().join("data"))
    };
    // Serve with the production kinds registered and the boot backfill
    // HELD: the unrowed window, indefinitely. (The queue is off too —
    // there is nothing for it to drain: the unrowed backlog is invisible
    // to the queue by construction, which is the point of the pin.)
    let ctx = open_ctx_with(&data_path, true, |state| {
        state.derive_backfill_enabled = false;
        state.derive_queue_enabled = false;
    });
    serve_until_quiet(Arc::clone(&ctx.state)).await;
    assert_eq!(
        ctx.count("SELECT COUNT(*) FROM derive_state"),
        0,
        "the unrowed precondition: no derive rows at all"
    );

    // ?path=: 503 derivation_pending, NOT an empty 200.
    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], "derivation_pending");
    assert!(resp.headers.get(header::RETRY_AFTER).is_some());

    // Blame: the same contract.
    let resp = ctx.blame(&c2, "f.txt").await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], "derivation_pending");

    // Merge-base never 503s: the plain walk answers, exactly.
    let resp = ctx
        .get(&format!("/v1/repos/{REPO}/merge-base?a={c1}&b={c2}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["merge_base"], json!(c1));
}

/// The settled pending gate is O(1): on a 20k-changeset repo, the three
/// exported statements behind `derive_any_pending` plan as indexed point
/// lookups and NEVER scan `changesets` (the deleted anti-join form probed
/// `derive_state` once per changeset of the repo on every gated read —
/// it cannot short-circuit when nothing is missing, so a deep repo pays
/// the full scan per read). End to end, both gate legs: marker set →
/// settled 200; marker deleted → 503 `derivation_pending`, however quiet
/// the queue.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn settled_pending_check_never_scans_changesets() {
    let ctx = in_memory_ctx(true);
    // 20k changesets inserted DIRECTLY (the offline-import shape: no
    // create-time enqueue, so no derive rows at all).
    {
        let conn = ctx.db();
        let tx = conn.transaction().unwrap();
        let insert = &naatre_store::Sql::plain(
            "INSERT INTO changesets(repo_id, id, canonical, author, message, \
             timestamp_ns, generation, principal, root_tree_id) \
             SELECT (SELECT id FROM repos WHERE name = ?1), ?2, x'', 'A', 'm', 0, ?3, 'A', NULL",
        );
        for i in 0..20_000i64 {
            let mut id = [0u8; 32];
            id[..8].copy_from_slice(&i.to_be_bytes());
            tx.execute(insert, &naatre_store::pv![REPO, id, i]).unwrap();
        }
        tx.commit().unwrap();
    }
    // The settled state: drain marks the backfill complete, nothing is
    // pending — the read must answer from the (empty) index, 200.
    ctx.drain().await;
    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(false));
    assert_eq!(resp.body["entries"], json!([]));

    // The pin: none of the gate's statements may scan changesets. (EXPLAIN
    // does not execute, but rusqlite still checks the bind count — bind a
    // NULL per parameter.)
    let plan_of = |sql: &str| -> Vec<String> {
        let conn = ctx.db();
        // SQLite-only pin (EXPLAIN QUERY PLAN has no Postgres analog).
        let raw = conn.sqlite_raw().expect("EXPLAIN pin runs on SQLite");
        let mut stmt = raw.prepare(&format!("EXPLAIN QUERY PLAN {sql}")).unwrap();
        let nulls = vec![None::<i64>; stmt.parameter_count()];
        stmt.query_map(rusqlite::params_from_iter(nulls.iter()), |r| {
            r.get::<_, String>(3)
        })
        .unwrap()
        .collect::<rusqlite::Result<Vec<String>>>()
        .unwrap()
    };
    for sql in [
        naatre_server::db::DERIVE_PENDING_KIND_NAME_SQL,
        naatre_server::db::DERIVE_PENDING_BACKFILL_DONE_SQL,
        naatre_server::db::DERIVE_PENDING_PROBE_SQL,
    ] {
        let plans = plan_of(sql);
        // Any SCAN at all is a violation — SQLite prints the alias for a
        // scanned table ("SCAN c"), so a table-name match would not catch
        // a re-aliased anti-join. The gate must be all index searches.
        assert!(
            plans.iter().all(|p| !p.contains("SCAN")),
            "the settled pending gate must not scan: {sql} -> {plans:?}"
        );
    }
    // Positive: the pending probe searches the derive_state index.
    let probe_plans = plan_of(naatre_server::db::DERIVE_PENDING_PROBE_SQL);
    assert!(
        probe_plans
            .iter()
            .any(|p| p.contains("SEARCH derive_state")),
        "the pending probe must search the derive_state index: {probe_plans:?}"
    );

    // Gate leg 1, end to end: marker gone → 503 derivation_pending, even
    // with the queue fully drained.
    ctx.db()
        .execute(
            &naatre_store::Sql::plain("DELETE FROM meta WHERE key LIKE 'derive_backfill_done:%'"),
            &naatre_store::pv![],
        )
        .unwrap();
    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(
        resp.status,
        StatusCode::SERVICE_UNAVAILABLE,
        "{}",
        resp.body
    );
    assert_eq!(resp.body["error"]["code"], "derivation_pending");
}

/// The stale-marker pin: a boot whose backfill completes leaves the
/// completion marker set; an OFFLINE import (registry-less, server down —
/// the flock makes "while the server runs" impossible) then adds
/// changesets with no derive rows. The next boot must clear the marker
/// BEFORE the accept loop: reads during its backfill 503
/// `derivation_pending`, never answer from the incomplete index. A held
/// backfill (the flag off) freezes that pre-completion state
/// deterministically; a full boot + drain recovers the exact history.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn stale_backfill_marker_cleared_at_boot() {
    let dir = tempfile::tempdir().unwrap();
    let data_path = dir.path().join("data");
    // Boot 1, settled: the queue drains and the markers are set.
    let c1 = {
        let ctx = build_ctx(dir.path(), true);
        let c1 = ctx
            .commit(
                None,
                "Ann <ann@example.com>",
                vec![add_ch("f.txt", b"one\n")],
            )
            .await;
        ctx.drain().await;
        let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
        assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
        c1
    };
    // Offline import: changeset c2 with NO derive rows (the exact store
    // effect of a registry-less import while the server is down).
    let c2 = {
        let mut bytes = [0u8; 32];
        bytes[0] = 0xC2;
        let conn = naatre_store::Db::Sqlite(Connection::open(data_path.join("naatre.db")).unwrap());
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO changesets(repo_id, id, canonical, author, message, \
             timestamp_ns, generation, principal, root_tree_id) \
             SELECT id, ?2, x'', 'Bob', 'imported', 0, 1, 'Bob', NULL \
             FROM repos WHERE name = ?1",
            ),
            &naatre_store::pv![REPO, bytes],
        )
        .unwrap();
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO changeset_parents(repo_id, cs, parent, ord) \
             SELECT (SELECT id FROM repos WHERE name = ?1), ?2, ?3, 0",
            ),
            &naatre_store::pv![REPO, bytes, id_of(&c1).0.as_slice()],
        )
        .unwrap();
        conn.execute(
            &naatre_store::Sql::plain(
                "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
             SELECT id, ?2, 'f.txt', 0, NULL, NULL, 0 \
             FROM repos WHERE name = ?1",
            ),
            &naatre_store::pv![REPO, bytes],
        )
        .unwrap();
        Id(bytes).to_hex()
    };

    // Boot 2 with the backfill HELD (the flag off): the pre-completion
    // state, frozen. The marker must be GONE — cleared before the accept
    // loop — and the read must 503, never answer from the index that
    // cannot know about c2.
    {
        let ctx = open_ctx_with(&data_path, true, |state| {
            state.derive_backfill_enabled = false;
            state.derive_queue_enabled = false;
        });
        // Managed shutdown: boot 3 opens the same data dir, and a
        // forgotten serve task would hold the flock forever.
        serve_shutdown(Arc::clone(&ctx.state)).await;
        let markers: i64 = ctx
            .db()
            .query_row_map(
                &naatre_store::Sql::plain(
                    "SELECT COUNT(*) FROM meta WHERE key LIKE 'derive_backfill_done:%'",
                ),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(markers, 0, "the stale marker was cleared at boot");
        let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
        assert_eq!(
            resp.status,
            StatusCode::SERVICE_UNAVAILABLE,
            "{}",
            resp.body
        );
        assert_eq!(resp.body["error"]["code"], "derivation_pending");
    }

    // Boot 3, full: backfill + queue run — the imported changeset derives,
    // the marker returns, and the read is exact (newest first).
    {
        let ctx = open_ctx_with(&data_path, true, |_| {});
        serve_until_quiet(Arc::clone(&ctx.state)).await;
        let mut stalled = 0u32;
        loop {
            let snap = {
                let state = Arc::clone(&ctx.state);
                state
                    .read(move |conn| {
                        let markers: i64 = conn
                            .query_row_map(
                                &naatre_store::Sql::plain(
                                    "SELECT COUNT(*) FROM meta \
                                 WHERE key LIKE 'derive_backfill_done:%'",
                                ),
                                &naatre_store::pv![],
                                |r| r.get(0),
                            )
                            .unwrap();
                        let pending: i64 = conn
                            .query_row_map(
                                &naatre_store::Sql::plain(
                                    "SELECT COUNT(*) FROM derive_state \
                                 WHERE state IN ('pending','deriving')",
                                ),
                                &naatre_store::pv![],
                                |r| r.get(0),
                            )
                            .unwrap();
                        Ok((markers, pending))
                    })
                    .await
                    .unwrap()
            };
            if snap.0 >= 3 && snap.1 == 0 {
                break;
            }
            stalled += 1;
            assert!(stalled < 300, "boot 3 never settled: {snap:?}");
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
        assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
        assert_eq!(resp.body["degraded"], json!(false));
        let ids: Vec<&str> = resp.body["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|e| e["id"].as_str().unwrap())
            .collect();
        assert_eq!(ids, vec![c2.as_str(), c1.as_str()], "newest first");
    }
}

// ---- 8. the fallback surface ---------------------------------------------------

/// With NO kinds registered, `?path=` still answers — the full-scan
/// fallback — so the surface exists on servers that predate the kind
/// (this is also the exact scan the parity oracle compares against).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn log_path_falls_back_to_scan_without_the_kind() {
    let ctx = in_memory_ctx(false);
    let c1 = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("f.txt", b"one\n")],
        )
        .await;
    let c2 = ctx
        .commit(
            Some(&c1),
            "Ann <ann@example.com>",
            vec![ch("f.txt", b"one\ntwo\n")],
        )
        .await;

    let resp = ctx.get(&format!("/v1/repos/{REPO}/log?path=f.txt")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], json!(false));
    let ids: Vec<&str> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids, vec![c2.as_str(), c1.as_str()], "newest first");

    // Blame without the kind: the pending contract — the index cannot
    // ever arrive on this server, and a fake-empty blame would be the
    // empty-as-if-no-history lie.
    let resp = ctx.blame(&c2, "f.txt").await;
    assert_eq!(resp.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(resp.body["error"]["code"], "derivation_pending");
}

/// The framework status of the production kinds tracks the lifecycle
/// (the read routes' contract input), and the commit-graph handoff
/// output decodes.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn production_kinds_track_the_lifecycle() {
    let ctx = in_memory_ctx(true);
    let c1 = ctx
        .commit(
            None,
            "Ann <ann@example.com>",
            vec![add_ch("f.txt", b"one\n")],
        )
        .await;
    for kind in [
        naatre_server::derived::KIND_BLAME,
        naatre_server::derived::KIND_PATH_HISTORY,
        naatre_server::derived::KIND_COMMIT_GRAPH,
    ] {
        assert_eq!(
            status(&ctx.state, kind, REPO, id_of(&c1)).await.unwrap(),
            Some(DeriveStatus::Pending),
            "{kind} pending right after create"
        );
    }
    ctx.drain().await;
    // Blame and path_history carry empty handoff outputs (the served
    // surface is their own tables).
    for kind in [
        naatre_server::derived::KIND_BLAME,
        naatre_server::derived::KIND_PATH_HISTORY,
    ] {
        assert_eq!(
            status(&ctx.state, kind, REPO, id_of(&c1)).await.unwrap(),
            Some(DeriveStatus::Done(DerivedOut(Vec::new()))),
            "{kind} done after drain"
        );
    }
    // commit_graph's output carries the skip list — empty for a root.
    let graph = status(
        &ctx.state,
        naatre_server::derived::KIND_COMMIT_GRAPH,
        REPO,
        id_of(&c1),
    )
    .await
    .unwrap();
    match graph {
        Some(DeriveStatus::Done(out)) => {
            // A root carries the EMPTY handoff marker (no first parent —
            // nothing to skip to), which decodes as no entries at all.
            if !out.as_bytes().is_empty() {
                let entries = naatre_server::derived::decode_skip_entries(out.as_bytes())
                    .expect("decodable skip list");
                assert!(entries.is_empty(), "a root has no skip entries");
            }
        }
        other => panic!("expected done commit_graph output: {other:?}"),
    }
}
