//! M3 client suite: drives the REAL server (spawned in-process on an
//! ephemeral port with a oneshot shutdown) through the client verbs, plus
//! one binary-level test per verb family to pin CLI arg parsing and exit
//! codes. Seeding uses the M2a technique: principals/tokens/refs rows go in
//! via direct SQL (main is land-only and land is M4).

use std::os::unix::fs::MetadataExt as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};
use std::sync::Arc;
use std::time::Duration;

use naatre_cli::verbs::{self, Net};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::ServerState;
use naatre_store::DataDir;
use serde_json::{Value, json};

// ---- server harness --------------------------------------------------------

struct TestServer {
    _dir: tempfile::TempDir,
    base: String,
    token: String,
    db_path: PathBuf,
    shutdown: Option<tokio::sync::oneshot::Sender<()>>,
    handle: Option<std::thread::JoinHandle<()>>,
}

impl Drop for TestServer {
    fn drop(&mut self) {
        if let Some(tx) = self.shutdown.take() {
            let _ = tx.send(());
        }
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

fn start_server() -> TestServer {
    start_server_with(|_| {})
}

/// The serve shape with the M6b production kinds registered — the blame
/// test's server. Kept out of [`start_server`]: the background queue's
/// write transactions change the write-slot contention profile of every
/// request, and the empty-registry shape keeps the rest of the suite
/// exactly as deterministic as before M6b.
fn start_server_with(register: impl FnOnce(&mut ServerState) + Send + 'static) -> TestServer {
    let dir = tempfile::tempdir().expect("tempdir");
    let data = DataDir::create(dir.path()).expect("create data dir");
    std::fs::write(data.db_path(), b"").expect("touch naatre.db");

    let (port_tx, port_rx) = std::sync::mpsc::channel();
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel::<()>();
    let data_path = dir.path().to_path_buf();
    let handle = std::thread::spawn(move || {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        rt.block_on(async move {
            let data = DataDir::open(&data_path).expect("open data dir");
            let mut state = ServerState::open(data).expect("server state");
            register(&mut state);
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("bind ephemeral port");
            let port = listener.local_addr().unwrap().port();
            port_tx.send(port).expect("send port");
            naatre_server::serve::serve(
                naatre_server::serve::ServeConfig {
                    listener,
                    state: Arc::new(state),
                },
                async move {
                    let _ = shutdown_rx.await;
                },
            )
            .await
            .expect("serve");
        });
    });
    let port = port_rx
        .recv_timeout(Duration::from_secs(10))
        .expect("server started");

    // Admin principal + token row, seeded directly (M2a technique); the
    // schema exists now that the server applied its migrations.
    let (token, hash) = naatre_server::auth::mint_token();
    {
        let conn = rusqlite::Connection::open(dir.path().join("naatre.db")).expect("open db");
        conn.busy_timeout(Duration::from_secs(5)).unwrap();
        conn.execute(
            "INSERT INTO principals(name, role, created_ns) VALUES ('admin', 'admin', 0)",
            [],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO tokens(principal, blake3, expires_ns, revoked) \
             VALUES ('admin', ?1, NULL, 0)",
            [&hash],
        )
        .unwrap();
    }

    let db_path = dir.path().join("naatre.db");
    TestServer {
        _dir: dir,
        base: format!("http://127.0.0.1:{port}"),
        token,
        db_path,
        shutdown: Some(shutdown_tx),
        handle: Some(handle),
    }
}

// ---- HTTP helpers (real requests against the in-process server) -----------

fn agent() -> ureq::Agent {
    ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .build(),
    )
}

fn api(server: &TestServer, method: &str, path: &str, body: Option<Value>) -> (u16, Value) {
    api_as(server, &server.token, method, path, body)
}

/// Setup-call retry for the single-writer 503: the test config runs the
/// derive queue at a 10 ms tick (default 5 s), so SETUP writes (repo create,
/// blob puts, changeset seeds) collide with derive passes far more often
/// than production ever will. Verbs UNDER TEST are never retried — their
/// 503s are assertions. Bounded, fixed 20ms backoff.
fn api_setup_retry(
    server: &TestServer,
    method: &str,
    path: &str,
    body: Option<Value>,
) -> (u16, Value) {
    let mut waited = 0u64;
    loop {
        let (status, body) = api(server, method, path, body.clone());
        if status != 503 || body["error"]["code"] != "writer_busy" || waited >= 2000 {
            return (status, body);
        }
        std::thread::sleep(Duration::from_millis(20));
        waited += 20;
    }
}

fn api_as(
    server: &TestServer,
    token: &str,
    method: &str,
    path: &str,
    body: Option<Value>,
) -> (u16, Value) {
    let url = format!("{}{path}", server.base);
    let auth = format!("Bearer {token}");
    let a = agent();
    let res = match (method, &body) {
        ("GET", _) => a
            .get(&url)
            .header("Authorization", auth)
            .call()
            .expect("request"),
        ("POST", Some(v)) => a
            .post(&url)
            .header("Authorization", auth)
            .send_json(v)
            .expect("request"),
        ("PUT", Some(v)) => a
            .put(&url)
            .header("Authorization", auth)
            .send_json(v)
            .expect("request"),
        _ => panic!("api helper: unsupported {method}"),
    };
    let status = res.status().as_u16();
    let value: Value = res.into_body().read_json().expect("json body");
    (status, value)
}

fn blob_id(data: &[u8]) -> String {
    object_id(Kind::Blob, data).to_hex()
}

/// PUTs a blob (raw, hash-addressed) and asserts acceptance.
fn put_blob(server: &TestServer, repo: &str, data: &[u8]) {
    let url = format!("{}/v1/repos/{repo}/objects/{}", server.base, blob_id(data));
    // Same bounded writer_busy retry as api_setup_retry: raw ureq here, so
    // inline the loop (setup write; the 503 is contention, not an assertion).
    let mut waited = 0u64;
    let res = loop {
        let res = agent()
            .put(&url)
            .header("Authorization", format!("Bearer {}", server.token))
            .send(data)
            .expect("put blob");
        let retry = res.status().as_u16() == 503 && waited < 2000;
        if !retry {
            break res;
        }
        std::thread::sleep(Duration::from_millis(20));
        waited += 20;
    };
    assert!(
        (200..300).contains(&res.status().as_u16()),
        "blob PUT failed: {}",
        res.status()
    );
}

/// One file change for [`changeset`].
fn change(op: &str, path: &str, data: &[u8]) -> (String, Value) {
    match op {
        "del" => (path.to_string(), json!({ "path": path, "op": "del" })),
        _ => (
            path.to_string(),
            json!({
                "path": path,
                "op": op,
                "blob": blob_id(data),
                "size": data.len(),
                "exec": false,
            }),
        ),
    }
}

/// Creates a changeset via the API (blobs are PUT by the caller first;
/// changes are sorted by path bytes) and returns the created body
/// {id, root_tree_id, generation}.
fn changeset(
    server: &TestServer,
    repo: &str,
    parents: &[String],
    mut changes: Vec<(String, Value)>,
    message: &str,
) -> Value {
    changes.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    let body = json!({
        "parents": parents,
        "author": "test <test@naatre.local>",
        "message": message,
        "changes": changes.into_iter().map(|(_, c)| c).collect::<Vec<_>>(),
    });
    let (status, created) = api_setup_retry(
        server,
        "POST",
        &format!("/v1/repos/{repo}/changesets"),
        Some(body),
    );
    assert_eq!(status, 201, "changeset create failed: {created}");
    created
}

/// Creates a repo; returns the name.
fn create_repo(server: &TestServer, name: &str) {
    let (status, body) =
        api_setup_retry(server, "POST", "/v1/repos", Some(json!({ "name": name })));
    assert!(
        status == 201 || (status == 409 && body["error"]["code"] == "repo_exists"),
        "repo create failed: {status} {body}"
    );
}

/// Seeds (or advances) the `main` ref row directly — land, its only
/// writer, is M4; the row is what land will leave behind.
fn seed_main(server: &TestServer, repo: &str, target_hex: &str) {
    let repo_id: i64 = rusqlite::Connection::open(&server.db_path)
        .and_then(|conn| {
            conn.busy_timeout(Duration::from_secs(5))?;
            conn.query_row("SELECT id FROM repos WHERE name = ?1", [repo], |r| r.get(0))
        })
        .expect("repo id");
    let target = Id::from_hex(target_hex).expect("target hex");
    let conn = rusqlite::Connection::open(&server.db_path).expect("open db");
    conn.busy_timeout(Duration::from_secs(5)).unwrap();
    conn.execute(
        "INSERT INTO refs(repo_id, name, target, version, last_principal) \
         VALUES (?1, 'main', ?2, 1, 'seed') \
         ON CONFLICT(repo_id, name) DO UPDATE SET target = excluded.target, \
         version = version + 1",
        rusqlite::params![repo_id, target.0.as_slice()],
    )
    .expect("seed main ref");
}

// ---- verb + binary helpers -------------------------------------------------

fn net(server: &TestServer) -> Net {
    Net {
        server: Some(server.base.clone()),
        token: Some(server.token.clone()),
    }
}

/// Runs the real binary with the server env; strips any ambient NAATRE_*
/// values so tests are hermetic.
fn run_binary(server: &TestServer, cwd: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args(args)
        .current_dir(cwd)
        .env("NAATRE_URL", &server.base)
        .env("NAATRE_TOKEN", &server.token)
        .env_remove("NAATRE_AUTHOR")
        .env_remove("NAATRE_HOSTNAME")
        .output()
        .expect("spawn naatre")
}

fn stdout_of(out: &Output) -> String {
    String::from_utf8_lossy(&out.stdout).into_owned()
}

fn stderr_of(out: &Output) -> String {
    String::from_utf8_lossy(&out.stderr).into_owned()
}

fn write_file(root: &Path, rel: &str, bytes: &[u8]) {
    let path = root.join(rel);
    std::fs::create_dir_all(path.parent().expect("parent")).unwrap();
    std::fs::write(path, bytes).unwrap();
}

// ---- tests -----------------------------------------------------------------

/// Test 1 — CLONE/STATUS/COMMIT LOOP: sparse clone, status clean with an
/// unreachable server (zero server calls), edit + untracked → M + ?,
/// commit, status clean, server log shows both changesets.
#[test]
fn clone_status_commit_loop() {
    let server = start_server();
    create_repo(&server, "loop");
    let v1 = b"fn main() {}\n";
    let docs = b"# loop\n";
    put_blob(&server, "loop", v1);
    put_blob(&server, "loop", docs);
    let c1 = changeset(
        &server,
        "loop",
        &[],
        vec![
            change("add", "src/main.rs", v1),
            change("add", "docs/readme.md", docs),
        ],
        "c1",
    );
    seed_main(&server, "loop", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    let code = verbs::clone::run(
        &format!("{}/loop", server.base),
        &["src".to_string()],
        Some(wc_dir.to_str().unwrap()),
        &net(&server),
    );
    assert_eq!(code, 0, "clone failed");
    assert_eq!(
        std::fs::read(wc_dir.join("src/main.rs")).unwrap(),
        v1,
        "sparse clone materialized the file"
    );
    assert!(
        !wc_dir.join("docs").exists(),
        "sparse clone must not materialize other prefixes"
    );

    // Status with an unreachable server: clean AND zero server calls (a
    // call would fail against http://127.0.0.1:9 and the verb would error).
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    let mut config = wc.load_config().unwrap();
    config.server_url = "http://127.0.0.1:9".into();
    wc.save_config(&config).unwrap();
    assert_eq!(verbs::status::run(&wc_dir, false), 0);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert!(
        report.is_clean(),
        "fresh sparse clone must be clean: {report:?}"
    );

    // Edit one file + add an untracked one → M + ?.
    let v2 = b"fn main() { loop }\n";
    write_file(&wc_dir, "src/main.rs", v2);
    write_file(&wc_dir, "src/extra.txt", b"new\n");
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(report.modified, vec!["src/main.rs".to_string()]);
    assert_eq!(report.untracked, vec!["src/extra.txt".to_string()]);
    assert!(report.deleted.is_empty());

    // Commit (flags override the unreachable config), then clean again.
    assert_eq!(
        verbs::commit::run(&wc_dir, "edit main", None, &net(&server)),
        0
    );
    let report = verbs::status::report(&wc_dir).unwrap();
    assert!(report.is_clean(), "post-commit must be clean: {report:?}");

    // Server log: first-parent chain from the new head shows 2 changesets.
    let (_, head_cs) = {
        let state = wc.load_state().unwrap();
        api(
            &server,
            "GET",
            &format!("/v1/repos/loop/log?from={}", state.head),
            None,
        )
    };
    assert_eq!(head_cs["entries"].as_array().unwrap().len(), 2);
    assert_eq!(head_cs["entries"][1]["id"], c1["id"]);
}

/// Test 2 — RACILY-CLEAN ADVERSARIAL: rewrite to the SAME length, restore mtime
/// via std FileTimes (set_modified) — ctime/ino moved, so the file is
/// rehashed and the content change is caught.
#[test]
fn racily_clean_rewrite_is_caught_by_rehash() {
    let server = start_server();
    create_repo(&server, "racy");
    let v1 = b"fn main() {}\n"; // 13 bytes
    let same_len = b"fn ma1n() {}\n"; // also 13 bytes
    put_blob(&server, "racy", v1);
    let c1 = changeset(&server, "racy", &[], vec![change("add", "m.rs", v1)], "c1");
    seed_main(&server, "racy", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/racy", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    let path = wc_dir.join("m.rs");
    let before = std::fs::metadata(&path).unwrap();
    let mtime = std::time::SystemTime::UNIX_EPOCH
        + Duration::from_nanos(before.mtime_nsec() as u64 + before.mtime() as u64 * 1_000_000_000);

    std::fs::write(&path, same_len).unwrap();
    // Forge mtime back; ctime cannot be forged by userspace.
    let f = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
    f.set_modified(mtime).unwrap();

    let after = std::fs::metadata(&path).unwrap();
    assert_eq!(after.size(), before.size(), "test setup: same length");
    assert_eq!(
        after.mtime_nsec(),
        before.mtime_nsec(),
        "test setup: mtime restored"
    );
    assert_ne!(
        after.ctime_nsec(),
        before.ctime_nsec(),
        "ctime must move on write"
    );

    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(
        report.modified,
        vec!["m.rs".to_string()],
        "racily-clean forgery must be caught by the rehash: {report:?}"
    );
}

/// Test 3 — NFD TWIN: a disk file created under the NFD spelling of an indexed
/// NFC path is neither a phantom delete nor an untracked twin (normalized
/// compare matches); a sync that would materialize the NFC path refuses
/// with dirty_working_copy instead of overwriting.
#[test]
fn nfd_twin_matches_its_nfc_twin() {
    let server = start_server();
    create_repo(&server, "nfd");
    let v1 = b"cafe content\n";
    put_blob(&server, "nfd", v1);
    let c1 = changeset(
        &server,
        "nfd",
        &[],
        vec![change("add", "caf\u{e9}.txt", v1)],
        "c1",
    );
    seed_main(&server, "nfd", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/nfd", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    let report = verbs::status::report(&wc_dir).unwrap();
    assert!(report.is_clean());

    // Write through the NFD spelling with different content. On a
    // normalization-insensitive FS this rewrites the same file; on a
    // byte-sensitive FS it creates a second file that folds onto the NFC
    // path. Either way the normalized compare must not split it into
    // delete + untracked.
    let nfd = "cafe\u{301}.txt".to_string(); // NFD of "café.txt": e + U+0301
    write_file(&wc_dir, &nfd, b"twin was here\n");
    let report = verbs::status::report(&wc_dir).unwrap();
    assert!(
        !report.deleted.iter().any(|p| p.contains("caf")),
        "no phantom delete: {report:?}"
    );
    assert!(
        report.untracked.is_empty(),
        "the twin must match its NFC twin, not appear untracked: {report:?}"
    );
    assert_eq!(
        report.modified,
        vec!["caf\u{e9}.txt".to_string()],
        "the folded path is dirty: {report:?}"
    );

    // A sync that would materialize the NFC path refuses (exit 2), never
    // overwrites: binary run so stderr can be asserted.
    let v2 = b"server side change\n";
    put_blob(&server, "nfd", v2);
    let c2 = changeset(
        &server,
        "nfd",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("mod", "caf\u{e9}.txt", v2)],
        "c2",
    );
    seed_main(&server, "nfd", c2["id"].as_str().unwrap());
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "dirty sync must refuse: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("caf"),
        "the conflicting path is listed: {stderr:?}"
    );
    assert_ne!(
        std::fs::read(wc_dir.join(nfd)).unwrap(),
        v2,
        "sync must not overwrite the twin"
    );
}

/// Test 4 — SYNC: up-to-date fast path; server-side advance (API changeset +
/// seeded main) syncs cleanly; dirty tracked → refuse exit 2 with both
/// lists; --force discards the collider; untracked collider → refuse;
/// --force removes it.
#[test]
fn sync_advances_refuses_dirty_and_forces() {
    let server = start_server();
    create_repo(&server, "syn");
    let a1 = b"a=1\n";
    let m1 = b"fn m() {}\n";
    put_blob(&server, "syn", a1);
    put_blob(&server, "syn", m1);
    let c1 = changeset(
        &server,
        "syn",
        &[],
        vec![change("add", "a.txt", a1), change("add", "src/main.rs", m1)],
        "c1",
    );
    seed_main(&server, "syn", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/syn", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    assert_eq!(
        verbs::sync::run(&wc_dir, false, &net(&server)),
        0,
        "up to date"
    );

    // Server-side c2: mod src/main.rs + add new.txt; main moved by seed.
    let m2 = b"fn m() { two }\n";
    let nt = b"new file\n";
    put_blob(&server, "syn", m2);
    put_blob(&server, "syn", nt);
    let c2 = changeset(
        &server,
        "syn",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![
            change("add", "new.txt", nt),
            change("mod", "src/main.rs", m2),
        ],
        "c2",
    );
    seed_main(&server, "syn", c2["id"].as_str().unwrap());

    assert_eq!(verbs::sync::run(&wc_dir, false, &net(&server)), 0);
    assert_eq!(std::fs::read(wc_dir.join("src/main.rs")).unwrap(), m2);
    assert_eq!(std::fs::read(wc_dir.join("new.txt")).unwrap(), nt);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());

    // Dirty-sync refusal: modify a tracked file the incoming set changes.
    let a2 = b"a=2\n";
    let m3 = b"fn m() { three }\n";
    put_blob(&server, "syn", a2);
    put_blob(&server, "syn", m3);
    let c3 = changeset(
        &server,
        "syn",
        &[c2["id"].as_str().unwrap().to_string()],
        vec![change("mod", "a.txt", a2), change("mod", "src/main.rs", m3)],
        "c3",
    );
    seed_main(&server, "syn", c3["id"].as_str().unwrap());
    let local_edit = b"my local work\n";
    write_file(&wc_dir, "src/main.rs", local_edit);
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(out.status.code(), Some(2), "dirty sync refuses: {out:?}");
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("src/main.rs"),
        "dirty list present: {stderr:?}"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("src/main.rs")).unwrap(),
        local_edit,
        "refusal preserves everything"
    );

    // --force discards the colliding local edit and completes.
    assert_eq!(verbs::sync::run(&wc_dir, true, &net(&server)), 0);
    assert_eq!(std::fs::read(wc_dir.join("src/main.rs")).unwrap(), m3);
    assert_eq!(std::fs::read(wc_dir.join("a.txt")).unwrap(), a2);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());

    // Untracked collider: local file at an incoming path → refuse listing
    // it; --force removes it and materializes the server's version.
    let nt2 = b"server extra\n";
    put_blob(&server, "syn", nt2);
    let c4 = changeset(
        &server,
        "syn",
        &[c3["id"].as_str().unwrap().to_string()],
        vec![change("add", "collide.bin", nt2)],
        "c4",
    );
    seed_main(&server, "syn", c4["id"].as_str().unwrap());
    write_file(&wc_dir, "collide.bin", b"untracked collider\n");
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "untracked collider refuses: {out:?}"
    );
    assert!(
        stderr_of(&out).contains("collide.bin"),
        "{:?}",
        stderr_of(&out)
    );
    assert_eq!(verbs::sync::run(&wc_dir, true, &net(&server)), 0);
    assert_eq!(std::fs::read(wc_dir.join("collide.bin")).unwrap(), nt2);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
}

/// Test 5 — HASH VERIFICATION: a corrupted server blob file makes `cat` fail
/// cleanly — non-zero exit, error on stderr, NO garbage bytes on stdout.
#[test]
fn corrupt_server_blob_is_a_clean_error() {
    let server = start_server();
    create_repo(&server, "hash");
    let data = b"precious bytes\n";
    put_blob(&server, "hash", data);
    let c1 = changeset(
        &server,
        "hash",
        &[],
        vec![change("add", "bin/data.bin", data)],
        "c1",
    );
    seed_main(&server, "hash", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/hash", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // Corrupt the CAS file on disk (the clone already verified its copy).
    let hex = blob_id(data);
    let shard = server
        ._dir
        .path()
        .join("objects")
        .join(&hex[0..2])
        .join(&hex[2..4])
        .join(&hex[4..]);
    std::fs::write(&shard, b"totally different corrupted bytes").unwrap();

    let out = run_binary(&server, &wc_dir, &["cat", "bin/data.bin"]);
    assert_ne!(out.status.code(), Some(0), "cat must fail: {out:?}");
    assert!(
        out.stdout.is_empty(),
        "no garbage output: {:?}",
        stdout_of(&out)
    );
    let stderr = stderr_of(&out);
    assert!(stderr.starts_with("error:"), "{stderr:?}");
}

/// Test 6 — DOCTOR: missing index → reported; --rebuild reconstructs from disk
/// and status is clean after; corrupt index bytes → reported; --rebuild
/// fixes.
#[test]
fn doctor_rebuilds_a_broken_index() {
    let server = start_server();
    create_repo(&server, "doctor");
    let x = b"x\n";
    let z = b"z\n";
    put_blob(&server, "doctor", x);
    put_blob(&server, "doctor", z);
    let c1 = changeset(
        &server,
        "doctor",
        &[],
        vec![change("add", "x.txt", x), change("add", "y/z.txt", z)],
        "c1",
    );
    seed_main(&server, "doctor", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/doctor", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // Delete the index → doctor reports (exit 1), --rebuild reconstructs.
    std::fs::remove_file(naatre_cli::wc::Wc::open(&wc_dir).unwrap().index_path()).unwrap();
    let out = run_binary(&server, &wc_dir, &["doctor"]);
    assert_ne!(out.status.code(), Some(0), "missing index must be reported");
    assert_eq!(verbs::doctor::run(&wc_dir, true, &net(&server)), 0);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());

    // Corrupt index bytes → doctor reports; --rebuild fixes.
    let index_path = naatre_cli::wc::Wc::open(&wc_dir).unwrap().index_path();
    let mut garbage = b"NAIidx\x01".to_vec();
    garbage.extend_from_slice(&99u32.to_le_bytes()); // count with no records
    std::fs::write(&index_path, &garbage).unwrap();
    assert_eq!(
        verbs::status::run(&wc_dir, false),
        1,
        "status refuses a corrupt index"
    );
    let out = run_binary(&server, &wc_dir, &["doctor"]);
    assert_ne!(out.status.code(), Some(0));
    // A corrupt index is an error path: reported on stderr.
    assert!(stderr_of(&out).contains("corrupt"), "{}", stdout_of(&out));
    assert_eq!(verbs::doctor::run(&wc_dir, true, &net(&server)), 0);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert!(report.is_clean(), "status clean after rebuild: {report:?}");
    assert_eq!(report.to_json()["status"], "clean");
}

/// Test 7 — EXIT CODES + --json: pinned status JSON shape, commit conflict-free
/// path exit 0, cat of a missing path exit 1 with the envelope code, clone
/// of a missing repo exit 1 with repo_not_found, bad usage exit 3.
#[test]
fn exit_codes_and_json_shapes() {
    let server = start_server();
    create_repo(&server, "codes");
    let v1 = b"hello\n";
    put_blob(&server, "codes", v1);
    let c1 = changeset(
        &server,
        "codes",
        &[],
        vec![change("add", "hello.txt", v1)],
        "c1",
    );
    seed_main(&server, "codes", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/codes", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // status --json shape, against an unreachable server (zero calls).
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    let mut config = wc.load_config().unwrap();
    config.server_url = "http://127.0.0.1:9".into();
    wc.save_config(&config).unwrap();
    let out = run_binary(&server, &wc_dir, &["status", "--json"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let value: Value = serde_json::from_str(stdout_of(&out).trim()).expect("json stdout");
    assert_eq!(
        value,
        json!({
            "status": "clean",
            "modified": [],
            "deleted": [],
            "untracked": [],
        })
    );

    // commit with nothing to commit: exit 0, "nothing to commit".
    let out = run_binary(&server, &wc_dir, &["commit", "-m", "no-op"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(stdout_of(&out).contains("nothing to commit"));

    // cat of a missing path: exit 1 with the envelope code.
    let out = run_binary(&server, &wc_dir, &["cat", "nope.txt"]);
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(
        stderr_of(&out).contains("error: not_found"),
        "{:?}",
        stderr_of(&out)
    );

    // clone of a missing repo: exit 1, repo_not_found.
    let out = run_binary(
        &server,
        &wc_dir,
        &["clone", &format!("{}/nope", server.base)],
    );
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(
        stderr_of(&out).contains("error: repo_not_found"),
        "{:?}",
        stderr_of(&out)
    );

    // usage error: exit 3.
    let out = run_binary(&server, &wc_dir, &["clone", "http://127.0.0.1:1"]);
    assert_eq!(
        out.status.code(),
        Some(3),
        "URL without a repo path is usage: {out:?}"
    );
}

/// Test 8 — SPARSE: add materializes a second prefix; remove drops its files
/// from disk AND index; a full (non-sparse) checkout refuses sparse add.
#[test]
fn sparse_add_and_remove() {
    let server = start_server();
    create_repo(&server, "sparse");
    let a = b"a\n";
    let c = b"c\n";
    let d = b"d\n";
    put_blob(&server, "sparse", a);
    put_blob(&server, "sparse", c);
    put_blob(&server, "sparse", d);
    let c1 = changeset(
        &server,
        "sparse",
        &[],
        vec![
            change("add", "docs/d.txt", d),
            change("add", "src/a.txt", a),
            change("add", "src/b/c.txt", c),
        ],
        "c1",
    );
    seed_main(&server, "sparse", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/sparse", server.base),
            &["src".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );
    assert!(wc_dir.join("src/a.txt").exists());
    assert!(wc_dir.join("src/b/c.txt").exists());
    assert!(!wc_dir.join("docs").exists());

    // sparse add docs → d.txt materializes; status clean.
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Add {
                prefix: "docs".into(),
                force: false,
            },
            &net(&server)
        ),
        0
    );
    assert_eq!(std::fs::read(wc_dir.join("docs/d.txt")).unwrap(), d);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert!(report.is_clean(), "after sparse add: {report:?}");

    // sparse remove src → gone from disk and index; docs remain.
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Remove("src".into()),
            &net(&server)
        ),
        0
    );
    assert!(
        !wc_dir.join("src").exists(),
        "removed prefix leaves the disk"
    );
    let report = verbs::status::report(&wc_dir).unwrap();
    assert!(report.is_clean(), "after sparse remove: {report:?}");
    assert_eq!(std::fs::read(wc_dir.join("docs/d.txt")).unwrap(), d);
    let index = naatre_cli::wc::Wc::open(&wc_dir)
        .unwrap()
        .load_index()
        .unwrap();
    assert_eq!(index.rows.len(), 1, "index holds only docs/d.txt");
    assert!(index.rows.contains_key("docs/d.txt"));

    // A full checkout refuses sparse add (narrowing needs re-clone).
    let full = server._dir.path().join("full");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/sparse", server.base),
            &[],
            Some(full.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    assert_eq!(
        verbs::sparse::run(
            &full,
            verbs::sparse::Action::Add {
                prefix: "docs".into(),
                force: false,
            },
            &net(&server)
        ),
        1,
        "sparse add on a full checkout is refused"
    );
}

// ---- binary-level per verb family ------------------------------------------

/// Binary sync family: up-to-date, advance, dirty refusal exit 2.
#[test]
fn binary_sync_family() {
    let server = start_server();
    create_repo(&server, "bsync");
    let m1 = b"one\n";
    put_blob(&server, "bsync", m1);
    let c1 = changeset(
        &server,
        "bsync",
        &[],
        vec![change("add", "m.txt", m1)],
        "c1",
    );
    seed_main(&server, "bsync", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/bsync", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(stdout_of(&out).contains("up to date"));

    let m2 = b"two\n";
    put_blob(&server, "bsync", m2);
    let c2 = changeset(
        &server,
        "bsync",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("mod", "m.txt", m2)],
        "c2",
    );
    seed_main(&server, "bsync", c2["id"].as_str().unwrap());

    write_file(&wc_dir, "m.txt", b"local\n");
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(out.status.code(), Some(2), "{out:?}");
    let out = run_binary(&server, &wc_dir, &["sync", "--force"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(std::fs::read(wc_dir.join("m.txt")).unwrap(), m2);
}

/// Binary reads family: ls, ls --json, cat at a ref, diff head-vs-disk and
/// diff --at A B.
#[test]
fn binary_reads_family() {
    let server = start_server();
    create_repo(&server, "reads");
    let v1 = b"v1\n";
    let v2 = b"v2\n";
    put_blob(&server, "reads", v1);
    put_blob(&server, "reads", v2);
    let c1 = changeset(
        &server,
        "reads",
        &[],
        vec![change("add", "src/f.txt", v1)],
        "c1",
    );
    seed_main(&server, "reads", c1["id"].as_str().unwrap());
    let c2 = changeset(
        &server,
        "reads",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("mod", "src/f.txt", v2)],
        "c2",
    );

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/reads", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    let out = run_binary(&server, &wc_dir, &["ls"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(stdout_of(&out), "src/\n");
    let out = run_binary(&server, &wc_dir, &["ls", "src"]);
    assert_eq!(out.status.code(), Some(0));
    assert_eq!(stdout_of(&out), "f.txt\n");
    let out = run_binary(&server, &wc_dir, &["ls", "--json"]);
    assert_eq!(out.status.code(), Some(0));
    let v: Value = serde_json::from_str(stdout_of(&out).trim()).unwrap();
    assert_eq!(v["entries"][0]["name"], "src");
    assert_eq!(v["entries"][0]["kind"], "dir");

    let out = run_binary(&server, &wc_dir, &["cat", "src/f.txt"]);
    assert_eq!(out.status.code(), Some(0));
    assert_eq!(out.stdout, v1);

    // diff --at A B: server-side two-changeset diff.
    let c1hex = c1["id"].as_str().unwrap();
    let c2hex = c2["id"].as_str().unwrap();
    let out = run_binary(&server, &wc_dir, &["diff", "--at", c1hex, c2hex]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(
        stdout_of(&out).contains("M src/f.txt"),
        "{}",
        stdout_of(&out)
    );

    // Default diff: head vs disk.
    write_file(&wc_dir, "src/f.txt", b"local\n");
    let out = run_binary(&server, &wc_dir, &["diff"]);
    assert_eq!(out.status.code(), Some(0));
    assert!(
        stdout_of(&out).contains("M src/f.txt"),
        "{}",
        stdout_of(&out)
    );
    let out = run_binary(&server, &wc_dir, &["diff", "--json"]);
    let v: Value = serde_json::from_str(stdout_of(&out).trim()).unwrap();
    assert_eq!(v["changes"][0]["kind"], "mod");
    assert_eq!(v["changes"][0]["path"], "src/f.txt");
}

/// Binary sparse family: add, list, remove through the real CLI.
#[test]
fn binary_sparse_family() {
    let server = start_server();
    create_repo(&server, "bsparse");
    let s = b"s\n";
    let d = b"d\n";
    put_blob(&server, "bsparse", s);
    put_blob(&server, "bsparse", d);
    let c1 = changeset(
        &server,
        "bsparse",
        &[],
        vec![
            change("add", "keep/k.txt", s),
            change("add", "drop/x.txt", d),
        ],
        "c1",
    );
    seed_main(&server, "bsparse", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/bsparse", server.base),
            &["keep".to_string(), "drop".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    let out = run_binary(&server, &wc_dir, &["sparse", "list"]);
    assert_eq!(out.status.code(), Some(0));
    assert!(stdout_of(&out).contains("keep") && stdout_of(&out).contains("drop"));

    let out = run_binary(&server, &wc_dir, &["sparse", "remove", "drop"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(!wc_dir.join("drop").exists());
    assert!(wc_dir.join("keep/k.txt").exists());

    let out = run_binary(&server, &wc_dir, &["sparse", "add", "nope"]);
    assert_eq!(
        out.status.code(),
        Some(1),
        "unknown prefix refused: {out:?}"
    );
}

/// Binary login family: writes config.toml 0600 with the given settings.
#[test]
fn binary_login_family() {
    let server = start_server();
    create_repo(&server, "blogin");
    let v = b"v\n";
    put_blob(&server, "blogin", v);
    let c1 = changeset(
        &server,
        "blogin",
        &[],
        vec![change("add", "v.txt", v)],
        "c1",
    );
    seed_main(&server, "blogin", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/blogin", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    let other = "http://127.0.0.1:65000";
    let out = run_binary(
        &server,
        &wc_dir,
        &["login", "--server", other, "--token", "ntk_other"],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    let config = wc.load_config().unwrap();
    assert_eq!(config.server_url, other);
    assert_eq!(config.token.as_deref(), Some("ntk_other"));
    let mode =
        std::os::unix::fs::MetadataExt::mode(&std::fs::metadata(wc.config_path()).unwrap()) & 0o777;
    assert_eq!(mode, 0o600, "config.toml must be 0600");

    // login outside a working copy is refused.
    let elsewhere = server._dir.path().join("elsewhere");
    std::fs::create_dir_all(&elsewhere).unwrap();
    let out = run_binary(
        &server,
        &elsewhere,
        &["login", "--server", other, "--token", "ntk_x"],
    );
    assert_ne!(out.status.code(), Some(0));
    assert!(stderr_of(&out).contains("not a naatre working copy"));
}

/// Binary clone/status/commit family: full round trip through the real
/// CLI, including the unreachable-server status proof.
#[test]
fn binary_clone_status_commit_family() {
    let server = start_server();
    create_repo(&server, "bfull");
    let v1 = b"one\n";
    put_blob(&server, "bfull", v1);
    let c1 = changeset(
        &server,
        "bfull",
        &[],
        vec![change("add", "a/b.txt", v1)],
        "c1",
    );
    seed_main(&server, "bfull", c1["id"].as_str().unwrap());

    let parent = server._dir.path().join("checkouts");
    std::fs::create_dir_all(&parent).unwrap();
    let out = run_binary(
        &server,
        &parent,
        &["clone", &format!("{}/bfull", server.base)],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let wc_dir = parent.join("bfull");
    assert_eq!(std::fs::read(wc_dir.join("a/b.txt")).unwrap(), v1);

    // status with an unreachable server in config (flags/env absent for
    // status anyway — it builds no client at all).
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    let mut config = wc.load_config().unwrap();
    config.server_url = "http://127.0.0.1:9".into();
    wc.save_config(&config).unwrap();
    let out = run_binary_no_env(&wc_dir, &["status"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(stdout_of(&out).contains("clean"));

    write_file(&wc_dir, "a/b.txt", b"changed\n");
    let out = run_binary(
        &server,
        &wc_dir,
        &[
            "commit",
            "-m",
            "via binary",
            "--author",
            "bot <bot@naatre.local>",
        ],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let created: Value = serde_json::from_str(stdout_of(&out).trim()).expect("json line");
    assert!(created["id"].as_str().is_some());
    assert!(created["root_tree_id"].as_str().is_some());
    assert_eq!(created["generation"], 1);
    let out = run_binary_no_env(&wc_dir, &["status"]);
    assert_eq!(out.status.code(), Some(0));
    assert!(stdout_of(&out).contains("clean"));
}

/// Runs the binary with all NAATRE_* env removed (hermetic status checks).
fn run_binary_no_env(cwd: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args(args)
        .current_dir(cwd)
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .env_remove("NAATRE_AUTHOR")
        .output()
        .expect("spawn naatre")
}

/// The scanner's unit-ish corner: status JSON mirrors the classified
/// report exactly (library-level shape pin for the Report type).
#[test]
fn report_json_shape_tracks_classification() {
    let server = start_server();
    create_repo(&server, "shape");
    let v = b"v\n";
    put_blob(&server, "shape", v);
    let c1 = changeset(&server, "shape", &[], vec![change("add", "f.txt", v)], "c1");
    seed_main(&server, "shape", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/shape", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    std::fs::remove_file(wc_dir.join("f.txt")).unwrap();
    write_file(&wc_dir, "g.txt", b"untracked\n");
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(
        report.to_json(),
        json!({
            "status": "dirty",
            "modified": [],
            "deleted": ["f.txt"],
            "untracked": ["g.txt"],
        })
    );
}

/// Test 9 — TWO-CLIENT SYNC over the REAL land route: A clones, commits,
/// lands (FF); B — a working copy cloned from the same base — syncs up to
/// A's content cleanly. B's local edit to a NON-colliding file (a path the
/// incoming set does not touch) does NOT refuse the sync: it succeeds, the
/// head advances, the edit survives untouched, and status afterwards still
/// lists it as modified. A colliding edit — a path the incoming set DOES
/// touch — refuses with `dirty_working_copy` (exit 2) naming the file;
/// --force discards exactly that edit and completes.
#[test]
fn two_client_sync_after_a_real_land() {
    let server = start_server();
    create_repo(&server, "duo");
    let a1 = b"a=1\n";
    let b1 = b"b=1\n";
    put_blob(&server, "duo", a1);
    put_blob(&server, "duo", b1);
    let c1 = changeset(
        &server,
        "duo",
        &[],
        vec![change("add", "a.txt", a1), change("add", "b.txt", b1)],
        "c1",
    );
    seed_main(&server, "duo", c1["id"].as_str().unwrap());

    // Two working copies off the same base.
    let wc_a = server._dir.path().join("wc-a");
    let wc_b = server._dir.path().join("wc-b");
    for dir in [&wc_a, &wc_b] {
        assert_eq!(
            verbs::clone::run(
                &format!("{}/duo", server.base),
                &[],
                Some(dir.to_str().unwrap()),
                &net(&server)
            ),
            0
        );
    }

    // A: edit, commit, LAND (real land route, deterministic key).
    let a2 = b"a=2 (A's work)\n";
    write_file(&wc_a, "a.txt", a2);
    let out = run_binary(
        &server,
        &wc_a,
        &["commit", "-m", "a edit", "--author", "a <a@naatre.local>"],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let out = run_binary(&server, &wc_a, &["land", "--key", "duo-a1"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let landed: Value = serde_json::from_str(
        stdout_of(&out)
            .lines()
            .rev()
            .find(|l| l.starts_with('{'))
            .expect("land prints its JSON result"),
    )
    .expect("land result json");
    let landed1 = landed["landed"].as_str().unwrap().to_string();
    let (_, refs) = api(&server, "GET", "/v1/repos/duo/refs/main", None);
    assert_eq!(refs["target"], landed1.as_str(), "land moved main");
    assert_eq!(landed["rebased"], 0, "first land is a fast-forward");

    // B (clean) syncs: advances to A's landed content, status clean.
    let out = run_binary(&server, &wc_b, &["sync"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(
        std::fs::read(wc_b.join("a.txt")).unwrap(),
        a2,
        "B advanced to A's landed content"
    );
    assert!(
        verbs::status::report(&wc_b).unwrap().is_clean(),
        "clean sync leaves B clean"
    );
    let wc = naatre_cli::wc::Wc::open(&wc_b).unwrap();
    assert_eq!(wc.load_state().unwrap().head, landed1, "B's head advanced");

    // B's local edit to a NON-colliding file, then A lands again.
    let b2 = b"b=2 (B's local work)\n";
    write_file(&wc_b, "b.txt", b2);
    let a3 = b"a=3 (A's second work)\n";
    write_file(&wc_a, "a.txt", a3);
    let out = run_binary(
        &server,
        &wc_a,
        &["commit", "-m", "a edit 2", "--author", "a <a@naatre.local>"],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let out = run_binary(&server, &wc_a, &["land", "--key", "duo-a2"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let (_, refs2) = api(&server, "GET", "/v1/repos/duo/refs/main", None);
    let landed2 = refs2["target"].as_str().unwrap().to_string();

    // The non-colliding edit no longer refuses: B's sync SUCCEEDS, the head
    // advances to A's second land, and the edit survives untouched —
    // status afterwards still lists it as modified.
    let out = run_binary(&server, &wc_b, &["sync"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(
        std::fs::read(wc_b.join("a.txt")).unwrap(),
        a3,
        "B advanced to A's second landed content"
    );
    assert_eq!(
        std::fs::read(wc_b.join("b.txt")).unwrap(),
        b2,
        "B's non-colliding edit survives the sync"
    );
    let report = verbs::status::report(&wc_b).unwrap();
    assert_eq!(report.modified, vec!["b.txt".to_string()], "{report:?}");
    let wc = naatre_cli::wc::Wc::open(&wc_b).unwrap();
    assert_eq!(wc.load_state().unwrap().head, landed2, "B's head advanced");

    // Scenario 2: B edits the SAME file A landed → refusal names it;
    // --force discards exactly that edit and completes.
    let a4 = b"a=4 (A's third work)\n";
    write_file(&wc_a, "a.txt", a4);
    let out = run_binary(
        &server,
        &wc_a,
        &["commit", "-m", "a edit 3", "--author", "a <a@naatre.local>"],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let out = run_binary(&server, &wc_a, &["land", "--key", "duo-a3"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));

    let collide = b"a.txt collided by B\n";
    write_file(&wc_b, "a.txt", collide);
    let out = run_binary(&server, &wc_b, &["sync"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "colliding sync refuses: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("a.txt"),
        "the collider is listed: {stderr:?}"
    );
    assert_eq!(
        std::fs::read(wc_b.join("a.txt")).unwrap(),
        collide,
        "refusal preserves B's colliding edit"
    );
    assert_eq!(std::fs::read(wc_b.join("b.txt")).unwrap(), b2);

    let out = run_binary(&server, &wc_b, &["sync", "--force"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(
        std::fs::read(wc_b.join("a.txt")).unwrap(),
        a4,
        "B's collider discarded for A's landed content"
    );
    assert_eq!(
        std::fs::read(wc_b.join("b.txt")).unwrap(),
        b2,
        "--force keeps non-colliding edits"
    );
    let report = verbs::status::report(&wc_b).unwrap();
    assert_eq!(report.modified, vec!["b.txt".to_string()], "{report:?}");
}

/// Test 10 — SYNC DEL HOLE: an untracked file at an incoming DELETION path
/// must refuse (exit 2, `dirty_working_copy`, the path listed) and survive
/// on disk; --force discards exactly it. An incoming del requires the path
/// in the head tree, so the untracked state here is reached the way the
/// public verbs leave it: `doctor --rebuild` rebuilds the index from disk
/// only, so the locally-removed file loses its index row and the new file
/// the user writes at that path is untracked (the same state a lost index
/// file leaves). Full checkout = empty sparse list, so the del loop applies.
#[test]
fn sync_refuses_untracked_file_at_an_incoming_deletion() {
    let server = start_server();
    create_repo(&server, "delhole");
    let k = b"k\n";
    let p = b"server content\n";
    put_blob(&server, "delhole", k);
    put_blob(&server, "delhole", p);
    let c1 = changeset(
        &server,
        "delhole",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "delhole", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/delhole", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // Server-side deletion on top of the client's head; main moves by seed.
    let c2 = changeset(
        &server,
        "delhole",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("del", "gone/p.txt", b"")],
        "c2 deletes p",
    );
    seed_main(&server, "delhole", c2["id"].as_str().unwrap());

    // The user's own new file at the path, untracked: drop the old row the
    // documented way (rebuild covers exactly what exists on disk), then
    // write fresh content.
    std::fs::remove_file(wc_dir.join("gone/p.txt")).unwrap();
    assert_eq!(verbs::doctor::run(&wc_dir, true, &net(&server)), 0);
    let mine = b"my own untracked work\n";
    write_file(&wc_dir, "gone/p.txt", mine);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(
        report.untracked,
        vec!["gone/p.txt".to_string()],
        "test setup: the file is untracked: {report:?}"
    );

    // Non-force sync must refuse, name the path, and preserve the file.
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "untracked file at an incoming del path refuses: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("gone/p.txt"),
        "the collider is listed: {stderr:?}"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        mine,
        "the untracked file survives the refusal"
    );

    // --force discards exactly it and completes the sync.
    assert_eq!(verbs::sync::run(&wc_dir, true, &net(&server)), 0);
    assert!(
        !wc_dir.join("gone/p.txt").exists(),
        "--force removes the colliding untracked file"
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());
}

/// NFD TWIN ON A DEL PATH: a disk file written under the NFD spelling folds
/// onto an incoming DELETION path's NFC form. The sync refuses (exit 2,
/// `dirty_working_copy`) and LISTS THE TWIN (its raw NFD path); the file is
/// preserved. `--force` removes exactly the twin — no phantom delete of
/// anything else — and completes clean.
#[test]
fn sync_refuses_nfd_twin_at_an_incoming_deletion() {
    let server = start_server();
    create_repo(&server, "nfddel");
    let v1 = b"cafe content\n";
    let k = b"k\n";
    put_blob(&server, "nfddel", v1);
    put_blob(&server, "nfddel", k);
    let c1 = changeset(
        &server,
        "nfddel",
        &[],
        vec![
            change("add", "gone/caf\u{e9}.txt", v1),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "nfddel", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/nfddel", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // Incoming del of the NFC path; main moves by seed.
    let c2 = changeset(
        &server,
        "nfddel",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("del", "gone/caf\u{e9}.txt", b"")],
        "c2 dels the café path",
    );
    seed_main(&server, "nfddel", c2["id"].as_str().unwrap());

    // The user's own file at the path, under the NFD spelling (e + U+0301):
    // the tracked row is lost the documented way (doctor --rebuild indexes
    // exactly what exists on disk), then the twin is written fresh.
    std::fs::remove_file(wc_dir.join("gone/caf\u{e9}.txt")).unwrap();
    assert_eq!(verbs::doctor::run(&wc_dir, true, &net(&server)), 0);
    let nfd = "gone/cafe\u{301}.txt".to_string();
    let mine = b"twin was here\n";
    write_file(&wc_dir, &nfd, mine);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(
        report.untracked,
        vec!["gone/caf\u{e9}.txt".to_string()],
        "test setup: the twin classifies at its NFC norm, not as a phantom \
         delete plus an untracked twin: {report:?}"
    );

    // The refusal lists the TWIN (the raw NFD path) and preserves it.
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "NFD twin at an incoming del path refuses: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(stderr.contains(&nfd), "the twin is listed: {stderr:?}");
    assert_eq!(
        std::fs::read(wc_dir.join(&nfd)).unwrap(),
        mine,
        "the twin survives the refusal"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("keep/k.txt")).unwrap(),
        k,
        "the refusal preserves everything"
    );

    // --force removes exactly the twin and completes the sync; no phantom
    // delete of anything else.
    assert_eq!(verbs::sync::run(&wc_dir, true, &net(&server)), 0);
    assert!(!wc_dir.join(&nfd).exists(), "--force removes the twin");
    assert!(
        !wc_dir.join("gone/caf\u{e9}.txt").exists(),
        "the NFC del path is gone too"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("keep/k.txt")).unwrap(),
        k,
        "no phantom delete: the unrelated file survives"
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());
}

/// The widen's own gate: `sparse add` refuses when local work sits on a path
/// it would materialize — here an untracked collider at a path the HEAD tree
/// still has (the widen walks the head, so main-only paths never enter its
/// set) — listing it and touching nothing. `sparse add --force` discards
/// exactly the collider, materializes head content, and the following sync
/// completes clean.
#[test]
fn sparse_add_refuses_untracked_colliders_and_forces() {
    let server = start_server();
    create_repo(&server, "widen");
    let k = b"k\n";
    let p = b"p content\n";
    let r = b"r from server\n";
    put_blob(&server, "widen", k);
    put_blob(&server, "widen", p);
    put_blob(&server, "widen", r);
    let c1 = changeset(
        &server,
        "widen",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "widen", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/widen", server.base),
            &["gone".to_string(), "keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    // Server-side c2 ADDS gone/r.txt at main; the client head is still c1.
    let c2 = changeset(
        &server,
        "widen",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("add", "gone/r.txt", r)],
        "c2 adds r",
    );
    seed_main(&server, "widen", c2["id"].as_str().unwrap());

    // Shed the cone; the user's own untracked file sits at gone/p.txt, a
    // path the widen would materialize (p is in the head tree).
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Remove("gone".into()),
            &net(&server)
        ),
        0
    );
    let mine = b"my own p\n";
    write_file(&wc_dir, "gone/p.txt", mine);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(
        report.untracked,
        vec!["gone/p.txt".to_string()],
        "test setup: {report:?}"
    );

    // The widen refuses (exit 2), lists the collider, materializes nothing.
    let out = run_binary(&server, &wc_dir, &["sparse", "add", "gone"]);
    assert_eq!(out.status.code(), Some(2), "widen must refuse: {out:?}");
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("gone/p.txt"),
        "the collider is listed: {stderr:?}"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        mine,
        "refusal preserves the file"
    );
    assert!(
        !wc_dir.join("gone/r.txt").exists(),
        "refusal materializes nothing"
    );

    // --force discards exactly the collider and materializes head content.
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Add {
                prefix: "gone".into(),
                force: true,
            },
            &net(&server)
        ),
        0
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        p,
        "head's p materialized over the discarded collider"
    );
    assert!(
        !wc_dir.join("gone/r.txt").exists(),
        "the widen walks the head: main-only r is not materialized"
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());

    // The following sync completes cleanly (it brings main's r).
    assert_eq!(verbs::sync::run(&wc_dir, false, &net(&server)), 0);
    assert_eq!(std::fs::read(wc_dir.join("gone/r.txt")).unwrap(), r);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());
}

/// LITERAL SPARSE-ROUTE REPRO (head-consistent): clone the full cone →
/// `sparse remove gone` sheds it → a server-side changeset deletes
/// gone/p.txt (API changeset + seeded main) while the user places an
/// UNTRACKED file at gone/p.txt → `sparse add gone` REFUSES (exit 2,
/// `dirty_working_copy`, the collider listed, the file preserved): the
/// widen walks the HEAD, which still contains p, so p is a path it would
/// materialize → `sparse add gone --force` discards exactly that untracked
/// file and materializes head gone content (incl. p) → `sync` then applies
/// the server del of p cleanly (exit 0, p gone, q intact, status clean).
#[test]
fn sparse_route_untracked_file_at_incoming_del_refuses() {
    let server = start_server();
    create_repo(&server, "sroute");
    let k = b"k\n";
    let p = b"server content\n";
    let q = b"q\n";
    put_blob(&server, "sroute", k);
    put_blob(&server, "sroute", p);
    put_blob(&server, "sroute", q);
    let c1 = changeset(
        &server,
        "sroute",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "gone/q.txt", q),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "sroute", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/sroute", server.base),
            &["gone".to_string(), "keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    // Shed the cone around p first.
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Remove("gone".into()),
            &net(&server)
        ),
        0
    );
    assert!(!wc_dir.join("gone").exists());

    // Server-side c2 deletes p; main moves by seed. The user's own file
    // lands at gone/p.txt while it is out of the cone.
    let c2 = changeset(
        &server,
        "sroute",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("del", "gone/p.txt", b"")],
        "c2 deletes p",
    );
    seed_main(&server, "sroute", c2["id"].as_str().unwrap());
    let mine = b"my own untracked work\n";
    write_file(&wc_dir, "gone/p.txt", mine);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(
        report.untracked,
        vec!["gone/p.txt".to_string()],
        "test setup: the file is untracked: {report:?}"
    );

    // Re-widen refuses: the head tree still contains p, so the widen would
    // materialize over the user's file.
    let out = run_binary(&server, &wc_dir, &["sparse", "add", "gone"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "widen must refuse the collider: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("gone/p.txt"),
        "the collider is listed: {stderr:?}"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        mine,
        "the file survives the refusal"
    );
    assert!(
        !wc_dir.join("gone/q.txt").exists(),
        "the refusal materializes nothing"
    );

    // `sparse add --force` discards exactly that untracked file and
    // materializes head gone content (incl. p at the head's content).
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Add {
                prefix: "gone".into(),
                force: true,
            },
            &net(&server)
        ),
        0
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        p,
        "head's p materialized over the discarded collider"
    );
    assert_eq!(std::fs::read(wc_dir.join("gone/q.txt")).unwrap(), q);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());

    // The sync then applies the server del of p cleanly.
    assert_eq!(verbs::sync::run(&wc_dir, false, &net(&server)), 0);
    assert!(
        !wc_dir.join("gone/p.txt").exists(),
        "the server del applies"
    );
    assert_eq!(std::fs::read(wc_dir.join("gone/q.txt")).unwrap(), q);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());
}

/// OUT-OF-CONE COLLIDER (the cone-filter pin): gone/ is never in the cone,
/// so the sync must not touch an untracked file at a server-deleted path
/// there — plain sync advances (exit 0) and `sync --force` advances (exit
/// 0), and the file survives both with its bytes intact. Without the
/// cone filter in the sync gate, the collider set would include the
/// out-of-cone path: plain sync would refuse and --force would DELETE the
/// user's file.
#[test]
fn sync_and_force_leave_out_of_cone_untracked_files_alone() {
    let server = start_server();
    create_repo(&server, "ocone");
    let k = b"k\n";
    let p = b"p out there\n";
    let n = b"new in keep\n";
    put_blob(&server, "ocone", k);
    put_blob(&server, "ocone", p);
    put_blob(&server, "ocone", n);
    let c1 = changeset(
        &server,
        "ocone",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "ocone", c1["id"].as_str().unwrap());

    // The cone is keep ONLY: gone/ is never materialized in this checkout.
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/ocone", server.base),
            &["keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );
    assert!(!wc_dir.join("gone").exists());

    // Server-side c2 deletes gone/p.txt; the user's own untracked file
    // sits there, out of cone.
    let c2 = changeset(
        &server,
        "ocone",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("del", "gone/p.txt", b"")],
        "c2 deletes p",
    );
    seed_main(&server, "ocone", c2["id"].as_str().unwrap());
    let mine = b"my out-of-cone file\n";
    write_file(&wc_dir, "gone/p.txt", mine);

    // Plain sync advances; the out-of-cone file is untouched.
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(
        out.status.code(),
        Some(0),
        "plain sync must not refuse an out-of-cone collider: {out:?}"
    );
    // The summary's cone filter: the out-of-cone deletion was NOT applied
    // here, so no `D gone/p.txt` line may pose as one on stdout.
    let summary = stdout_of(&out);
    assert!(
        !summary.contains("D gone/p.txt"),
        "an out-of-cone del must not print as applied: {summary:?}"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        mine,
        "plain sync leaves the out-of-cone file alone"
    );
    assert_eq!(std::fs::read(wc_dir.join("keep/k.txt")).unwrap(), k);
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());

    // A second advance with --force: still exit 0, file still intact.
    let c3 = changeset(
        &server,
        "ocone",
        &[c2["id"].as_str().unwrap().to_string()],
        vec![change("add", "keep/new.txt", n)],
        "c3 adds new.txt",
    );
    seed_main(&server, "ocone", c3["id"].as_str().unwrap());
    let out = run_binary(&server, &wc_dir, &["sync", "--force"]);
    assert_eq!(
        out.status.code(),
        Some(0),
        "--force must not eat the out-of-cone collider: {out:?}"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        mine,
        "--force leaves the out-of-cone file alone"
    );
    assert_eq!(std::fs::read(wc_dir.join("keep/new.txt")).unwrap(), n);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(
        report.untracked,
        vec!["gone/p.txt".to_string()],
        "the file is still untracked, not deleted: {report:?}"
    );
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c3["id"].as_str().unwrap());
}

/// DELETED CONE PREFIX: the server deletes the LAST file under a cone
/// prefix, so the prefix's whole directory vanishes from the target tree.
/// A deleted directory is normal history: the sync treats the absent
/// prefix as an EMPTY subtree — the incoming del removes the indexed file
/// from disk and index, the emptied dir is pruned, the sync completes
/// (exit 0), status is clean, and later syncs keep working. (Pre-fix this
/// failed forever with `error: sparse prefix "gone" does not exist in the
/// tree`.)
#[test]
fn sync_applies_a_fully_deleted_cone_prefix() {
    let server = start_server();
    create_repo(&server, "goneprefix");
    let k = b"k\n";
    let p = b"p\n";
    let n = b"n\n";
    put_blob(&server, "goneprefix", k);
    put_blob(&server, "goneprefix", p);
    put_blob(&server, "goneprefix", n);
    let c1 = changeset(
        &server,
        "goneprefix",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "goneprefix", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/goneprefix", server.base),
            &["gone".to_string(), "keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    // Server-side c2 deletes the only file under gone/ — the whole prefix
    // directory vanishes from the target tree.
    let c2 = changeset(
        &server,
        "goneprefix",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("del", "gone/p.txt", b"")],
        "c2 deletes p",
    );
    seed_main(&server, "goneprefix", c2["id"].as_str().unwrap());

    // The sync completes: exit 0, disk+index cleaned, gone/ pruned.
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(
        out.status.code(),
        Some(0),
        "a fully deleted cone prefix must not fail the sync: {out:?}"
    );
    assert!(
        !wc_dir.join("gone").exists(),
        "the emptied prefix dir is pruned"
    );
    let index = naatre_cli::wc::Wc::open(&wc_dir)
        .unwrap()
        .load_index()
        .unwrap();
    assert_eq!(index.rows.len(), 1, "only keep/k.txt stays indexed");
    assert!(index.rows.contains_key("keep/k.txt"));
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());

    // The uniform client rule extends to reads: `naatre diff` (local mode)
    // walks the same vanished prefix at head — it must complete (exit 0)
    // with an empty diff, and status stays clean. (Pre-fix this wedged
    // with the strict walk's `does not exist in the tree` error forever.)
    let out = run_binary(&server, &wc_dir, &["diff"]);
    assert_eq!(
        out.status.code(),
        Some(0),
        "local diff must not wedge on a vanished cone prefix: {out:?}"
    );
    assert!(
        !stdout_of(&out).contains("D "),
        "head and disk agree: no phantom dels: {:?}",
        stdout_of(&out)
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());

    // Later syncs keep working (the vanished prefix stays an empty subtree).
    let c3 = changeset(
        &server,
        "goneprefix",
        &[c2["id"].as_str().unwrap().to_string()],
        vec![change("add", "keep/new.txt", n)],
        "c3 adds new.txt",
    );
    seed_main(&server, "goneprefix", c3["id"].as_str().unwrap());
    assert_eq!(verbs::sync::run(&wc_dir, false, &net(&server)), 0);
    assert_eq!(std::fs::read(wc_dir.join("keep/new.txt")).unwrap(), n);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(out.status.code(), Some(0), "up-to-date sync fine: {out:?}");
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c3["id"].as_str().unwrap());
}

/// Binary clone usage: a URL whose scheme is not http (https included) is a
/// usage error (exit 3) naming the scheme and stating the client speaks
/// plain HTTP only — never silently downgraded.
#[test]
fn binary_clone_rejects_non_http_schemes() {
    let dir = tempfile::tempdir().unwrap();
    for url in ["https://example.test/repo", "ftp://example.test/repo"] {
        let out = run_binary_no_env(dir.path(), &["clone", url]);
        assert_eq!(out.status.code(), Some(3), "{url}: {out:?}");
        let stderr = stderr_of(&out);
        assert!(stderr.contains("plain HTTP"), "{url}: {stderr:?}");
        let scheme = url.split(':').next().unwrap();
        assert!(stderr.contains(scheme), "{url}: {stderr:?}");
    }
}

/// Binary usage errors: clap flag/argument mistakes exit 3 (never 2 — that
/// is the conflict class), with clap's message on stderr; `sync --help`
/// still exits 0 and carries the relaxed-gate wording.
#[test]
fn binary_clap_usage_errors_exit_3() {
    let dir = tempfile::tempdir().unwrap();
    let out = run_binary_no_env(dir.path(), &["sync", "--bogus"]);
    assert_eq!(out.status.code(), Some(3), "{out:?}");
    assert!(stderr_of(&out).contains("--bogus"), "{:?}", stderr_of(&out));
    let out = run_binary_no_env(dir.path(), &["commit"]);
    assert_eq!(out.status.code(), Some(3), "{out:?}");
    assert!(
        stderr_of(&out).contains("--message"),
        "{:?}",
        stderr_of(&out)
    );
    let out = run_binary_no_env(dir.path(), &["sync", "--help"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(
        stdout_of(&out).contains("collide"),
        "help names the relaxed gate: {:?}",
        stdout_of(&out)
    );
    assert!(
        stdout_of(&out).contains("symlinks are never removed"),
        "help states the scope asymmetry with sparse add --force: {:?}",
        stdout_of(&out)
    );
}

/// Commit without NAATRE_AUTHOR prints the derived-identity notice on
/// stderr and still succeeds; an explicit --author prints no notice.
#[test]
fn commit_without_author_prints_the_identity_notice() {
    let server = start_server();
    create_repo(&server, "author");
    let v1 = b"v1\n";
    put_blob(&server, "author", v1);
    let c1 = changeset(
        &server,
        "author",
        &[],
        vec![change("add", "f.txt", v1)],
        "c1",
    );
    seed_main(&server, "author", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/author", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // run_binary strips NAATRE_AUTHOR: the fallback fires.
    write_file(&wc_dir, "f.txt", b"v2\n");
    let out = run_binary(&server, &wc_dir, &["commit", "-m", "fallback author"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let stderr = stderr_of(&out);
    assert!(stderr.contains("identity not configured"), "{stderr:?}");
    assert!(stderr.contains("set NAATRE_AUTHOR"), "{stderr:?}");

    write_file(&wc_dir, "f.txt", b"v3\n");
    let out = run_binary(
        &server,
        &wc_dir,
        &["commit", "-m", "explicit", "--author", "a <a@naatre.local>"],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(
        !stderr_of(&out).contains("identity not configured"),
        "{:?}",
        stderr_of(&out)
    );
}

/// sparse add/remove honor --server/--token: they beat a poisoned config
/// (flags > env > config.toml), like every other server-talking verb.
#[test]
fn binary_sparse_flags_beat_a_poisoned_config() {
    let server = start_server();
    create_repo(&server, "sflag");
    let s = b"s\n";
    let d = b"d\n";
    put_blob(&server, "sflag", s);
    put_blob(&server, "sflag", d);
    let c1 = changeset(
        &server,
        "sflag",
        &[],
        vec![
            change("add", "keep/k.txt", s),
            change("add", "drop/x.txt", d),
        ],
        "c1",
    );
    seed_main(&server, "sflag", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/sflag", server.base),
            &["keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // Point the config at a dead port; without flags the verb must fail.
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    let mut config = wc.load_config().unwrap();
    config.server_url = "http://127.0.0.1:9".into();
    wc.save_config(&config).unwrap();
    let out = run_binary_no_env(&wc_dir, &["sparse", "add", "drop"]);
    assert_ne!(out.status.code(), Some(0), "dead config must fail: {out:?}");

    // With the flags the same command succeeds — and remove takes them too.
    let out = run_binary_no_env(
        &wc_dir,
        &[
            "sparse",
            "add",
            "drop",
            "--server",
            &server.base,
            "--token",
            &server.token,
        ],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(wc_dir.join("drop/x.txt").exists());
    let out = run_binary_no_env(
        &wc_dir,
        &[
            "sparse",
            "remove",
            "keep",
            "--server",
            &server.base,
            "--token",
            &server.token,
        ],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(!wc_dir.join("keep").exists());
}

/// Doctor's error path (no working copy) prints to stderr, not stdout.
#[test]
fn binary_doctor_errors_go_to_stderr() {
    let dir = tempfile::tempdir().unwrap();
    let out = run_binary_no_env(dir.path(), &["doctor"]);
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(
        stderr_of(&out).contains("not a naatre working copy"),
        "{:?}",
        stderr_of(&out)
    );
    assert!(
        stdout_of(&out).is_empty(),
        "errors are not stdout: {:?}",
        stdout_of(&out)
    );
}

/// Binary admin-HTTP family: `token create` prints the id revoke needs and
/// `token list` renders expiry as ISO-8601 (or "never"); `repo stats` works.
#[test]
fn binary_admin_http_family() {
    let server = start_server();
    create_repo(&server, "adm");
    let dir = server._dir.path().join("anywhere");
    std::fs::create_dir_all(&dir).unwrap();

    let out = run_binary(
        &server,
        &dir,
        &["token", "create", "alice", "--role", "reader"],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let stdout = stdout_of(&out);
    assert!(stdout.contains("token: ntk_"), "{stdout:?}");
    assert!(stdout.contains("token id:"), "{stdout:?}");

    let out = run_binary(
        &server,
        &dir,
        &[
            "token",
            "create",
            "bob",
            "--role",
            "reader",
            "--ttl-days",
            "30",
        ],
    );
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));

    let out = run_binary(&server, &dir, &["token", "list"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let stdout = stdout_of(&out);
    assert!(stdout.contains("never"), "admin never expires: {stdout:?}");
    assert!(
        stdout.split_whitespace().any(|f| f.len() == 20
            && f.as_bytes()[4] == b'-'
            && f.as_bytes()[7] == b'-'
            && f.as_bytes()[10] == b'T'
            && f.as_bytes()[13] == b':'
            && f.as_bytes()[16] == b':'
            && f.as_bytes()[19] == b'Z'),
        "an ISO-8601 expiry is listed: {stdout:?}"
    );

    let out = run_binary(&server, &dir, &["repo", "stats", "adm"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(
        stdout_of(&out).contains("changesets:"),
        "{:?}",
        stdout_of(&out)
    );
}

/// Re-landing a settled key with a different payload: exit 2 (conflict
/// class) and the details — settled head + land-status pointer — printed.
#[test]
fn land_settled_key_prints_details_and_exits_2() {
    let server = start_server();
    create_repo(&server, "settled");
    let v1 = b"v1\n";
    put_blob(&server, "settled", v1);
    let c1 = changeset(
        &server,
        "settled",
        &[],
        vec![change("add", "f.txt", v1)],
        "c1",
    );
    seed_main(&server, "settled", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/settled", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // First land with the key settles it.
    write_file(&wc_dir, "f.txt", b"v2\n");
    let out = run_binary(&server, &wc_dir, &["commit", "-m", "one"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let out = run_binary(&server, &wc_dir, &["land", "--key", "settled-1"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));

    // Same key, new head → idempotency_key_settled (409, conflict class).
    write_file(&wc_dir, "f.txt", b"v3\n");
    let out = run_binary(&server, &wc_dir, &["commit", "-m", "two"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let out = run_binary(&server, &wc_dir, &["land", "--key", "settled-1"]);
    assert_eq!(out.status.code(), Some(2), "{out:?}");
    let stderr = stderr_of(&out);
    assert!(stderr.contains("idempotency_key_settled"), "{stderr:?}");
    assert!(stderr.contains("settled head:"), "{stderr:?}");
    assert!(
        stderr.contains("/v1/repos/settled/land/settled-1"),
        "land-status pointer printed: {stderr:?}"
    );
}

/// Test 11 — DELETION GATE ARMS, end to end (qa-B1): every dirty-tracked ×
/// incoming-kind pair refuses — locally-modified vs incoming mod AND vs
/// incoming del (the module doc's own callout), locally-deleted vs incoming
/// mod AND vs incoming del (the fixed gate's chosen behavior: refuse) — each
/// path listed, nothing on disk touched by the refusal. A clean file with an
/// incoming del is NOT dirty (no refusal of unrelated work). --force then
/// applies exactly the incoming set: edits discarded, deletions applied in
/// both directions, and the checkout ends clean.
#[test]
fn sync_gate_refuses_deletion_collisions_and_force_applies_them() {
    let server = start_server();
    create_repo(&server, "delgate");
    let files: [(&str, &[u8]); 5] = [
        ("d.txt", b"d1\n"),   // clean; incoming del → applies (no refuse)
        ("dd.txt", b"dd1\n"), // locally deleted; incoming del → refuse
        ("l.txt", b"l1\n"),   // locally modified; incoming del → refuse
        ("m.txt", b"m1\n"),   // locally modified; incoming mod → refuse
        ("x.txt", b"x1\n"),   // locally deleted; incoming mod → refuse
    ];
    let mut changes = Vec::new();
    for (path, data) in &files {
        put_blob(&server, "delgate", data);
        changes.push(change("add", path, data));
    }
    let c1 = changeset(&server, "delgate", &[], changes, "c1");
    seed_main(&server, "delgate", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/delgate", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // Incoming: mod m.txt + x.txt; del d.txt + l.txt + dd.txt.
    let m2 = b"m2 (server)\n";
    let x2 = b"x2 (server)\n";
    put_blob(&server, "delgate", m2);
    put_blob(&server, "delgate", x2);
    let c2 = changeset(
        &server,
        "delgate",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![
            change("del", "d.txt", b""),
            change("del", "dd.txt", b""),
            change("del", "l.txt", b""),
            change("mod", "m.txt", m2),
            change("mod", "x.txt", x2),
        ],
        "c2",
    );
    seed_main(&server, "delgate", c2["id"].as_str().unwrap());

    // Local: edit l.txt + m.txt; delete dd.txt + x.txt.
    let mine_l = b"l local work\n";
    let mine_m = b"m local work\n";
    write_file(&wc_dir, "l.txt", mine_l);
    write_file(&wc_dir, "m.txt", mine_m);
    std::fs::remove_file(wc_dir.join("dd.txt")).unwrap();
    std::fs::remove_file(wc_dir.join("x.txt")).unwrap();
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(
        report.modified,
        vec!["l.txt".to_string(), "m.txt".to_string()]
    );
    assert_eq!(
        report.deleted,
        vec!["dd.txt".to_string(), "x.txt".to_string()]
    );

    // Every colliding path refuses; the clean d.txt is not listed.
    let out = run_binary(&server, &wc_dir, &["sync"]);
    assert_eq!(out.status.code(), Some(2), "{out:?}");
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    for path in ["dd.txt", "l.txt", "m.txt", "x.txt"] {
        assert!(stderr.contains(path), "{path} listed: {stderr:?}");
    }
    assert_eq!(std::fs::read(wc_dir.join("l.txt")).unwrap(), mine_l);
    assert_eq!(std::fs::read(wc_dir.join("m.txt")).unwrap(), mine_m);
    assert!(
        wc_dir.join("d.txt").exists(),
        "refusal preserves everything"
    );

    // --force applies the incoming set exactly; local colliders are gone.
    assert_eq!(verbs::sync::run(&wc_dir, true, &net(&server)), 0);
    assert_eq!(std::fs::read(wc_dir.join("m.txt")).unwrap(), m2);
    assert_eq!(std::fs::read(wc_dir.join("x.txt")).unwrap(), x2);
    assert!(
        !wc_dir.join("d.txt").exists()
            && !wc_dir.join("dd.txt").exists()
            && !wc_dir.join("l.txt").exists(),
        "incoming deletions applied"
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());
}

// ---- fix2: strict-direction pins, symlink gate, sparse dirty-tracked arm ----

/// STRICT DIRECTION (clone): `clone --sparse <typo>` fails with the strict
/// walk's error and exit 1 — the lenient absent-prefix-is-empty rule
/// (sync/local-diff only) never leaks into a widening operation, and a
/// failed clone leaves no working copy behind.
#[test]
fn clone_sparse_refuses_a_missing_prefix() {
    let server = start_server();
    create_repo(&server, "strictclone");
    let k = b"k\n";
    put_blob(&server, "strictclone", k);
    let c1 = changeset(
        &server,
        "strictclone",
        &[],
        vec![change("add", "keep/k.txt", k)],
        "c1",
    );
    seed_main(&server, "strictclone", c1["id"].as_str().unwrap());

    let wc_dir = server._dir.path().join("wc");
    let out = run_binary(
        &server,
        server._dir.path(),
        &[
            "clone",
            "--sparse",
            "nope",
            &format!("{}/strictclone", server.base),
            "--into",
            "wc",
        ],
    );
    assert_eq!(out.status.code(), Some(1), "strict walk errors: {out:?}");
    let stderr = stderr_of(&out);
    assert!(stderr.contains("nope"), "the prefix is named: {stderr:?}");
    assert!(
        stderr.contains("does not exist in the tree"),
        "the strict walk's error: {stderr:?}"
    );
    assert!(
        !wc_dir.exists(),
        "a failed clone leaves no working copy behind"
    );
}

/// STRICT DIRECTION (sparse add): `sparse add <missing-prefix>` fails with
/// the strict walk's error and exit 1 — never the lenient form (which would
/// "succeed" with zero files), and the prefix list is unchanged.
#[test]
fn sparse_add_refuses_a_missing_prefix() {
    let server = start_server();
    create_repo(&server, "strictadd");
    let k = b"k\n";
    put_blob(&server, "strictadd", k);
    let c1 = changeset(
        &server,
        "strictadd",
        &[],
        vec![change("add", "keep/k.txt", k)],
        "c1",
    );
    seed_main(&server, "strictadd", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/strictadd", server.base),
            &["keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    let out = run_binary(&server, &wc_dir, &["sparse", "add", "nope"]);
    assert_eq!(out.status.code(), Some(1), "strict walk errors: {out:?}");
    let stderr = stderr_of(&out);
    assert!(stderr.contains("nope"), "the prefix is named: {stderr:?}");
    assert!(
        stderr.contains("does not exist in the tree"),
        "the strict walk's error: {stderr:?}"
    );
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(
        wc.load_state().unwrap().sparse,
        vec!["keep".to_string()],
        "the failed add changed nothing"
    );
    assert_eq!(std::fs::read(wc_dir.join("keep/k.txt")).unwrap(), k);
}

/// SYMLINK GATE (file form): a symlink at a widening path is invisible to
/// the scanner (it skips symlinks) but would be clobbered by the widen —
/// the gate refuses with `dirty_working_copy` naming it as a symlink,
/// preserving link and target; `--force` unlinks exactly the link and
/// materializes head content over it.
#[test]
fn sparse_add_refuses_a_file_symlink_at_a_widening_path() {
    let server = start_server();
    create_repo(&server, "symfile");
    let k = b"k\n";
    let p = b"p from head\n";
    put_blob(&server, "symfile", k);
    put_blob(&server, "symfile", p);
    let c1 = changeset(
        &server,
        "symfile",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "symfile", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/symfile", server.base),
            &["keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    // The user's own symlink at the widening path, pointing at a tracked
    // file inside the checkout. The scanner cannot see it (symlinks are
    // skipped) — status is clean, which is exactly why the gate must
    // classify it itself.
    std::fs::create_dir_all(wc_dir.join("gone")).unwrap();
    std::os::unix::fs::symlink("keep/k.txt", wc_dir.join("gone/p.txt")).unwrap();
    assert!(
        verbs::status::report(&wc_dir).unwrap().is_clean(),
        "test setup: the scanner is symlink-blind: {:?}",
        verbs::status::report(&wc_dir).unwrap()
    );

    // The widen refuses (exit 2), names the path as a symlink, touches
    // nothing.
    let out = run_binary(&server, &wc_dir, &["sparse", "add", "gone"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "the symlink must refuse: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("gone/p.txt") && stderr.contains("symlink"),
        "the collider is named as a symlink: {stderr:?}"
    );
    assert!(
        wc_dir
            .join("gone/p.txt")
            .symlink_metadata()
            .unwrap()
            .file_type()
            .is_symlink(),
        "the refusal preserves the link"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("keep/k.txt")).unwrap(),
        k,
        "the refusal preserves the link's target"
    );

    // --force unlinks exactly the link and materializes head content; the
    // target file is untouched.
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Add {
                prefix: "gone".into(),
                force: true,
            },
            &net(&server),
        ),
        0
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        p,
        "head's p materialized where the link was"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("keep/k.txt")).unwrap(),
        k,
        "--force removes the link, never its target"
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
}

/// SYMLINK GATE (dir form): a directory symlink at a component of a
/// widened path cannot redirect the widen's writes outside the checkout —
/// the gate refuses naming the link; `--force` removes the LINK itself and
/// the materialization lands inside the checkout (the redirect target
/// receives nothing).
#[test]
fn sparse_add_refuses_a_dir_symlink_under_a_widened_prefix() {
    let server = start_server();
    create_repo(&server, "symdir");
    let k = b"k\n";
    let x = b"x from head\n";
    put_blob(&server, "symdir", k);
    put_blob(&server, "symdir", x);
    let c1 = changeset(
        &server,
        "symdir",
        &[],
        vec![
            change("add", "gone/sub/x.txt", x),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "symdir", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/symdir", server.base),
            &["keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    // gone/sub is a symlink out of the checkout; head materializes
    // gone/sub/x.txt, which would land in the target directory.
    let outsider = server._dir.path().join("outsider");
    std::fs::create_dir_all(&outsider).unwrap();
    std::fs::create_dir_all(wc_dir.join("gone")).unwrap();
    std::os::unix::fs::symlink("../../outsider", wc_dir.join("gone/sub")).unwrap();

    // The widen refuses: gone/sub is named as a symlink.
    let out = run_binary(&server, &wc_dir, &["sparse", "add", "gone"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "the dir symlink must refuse: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("gone/sub") && stderr.contains("symlink"),
        "the component link is named: {stderr:?}"
    );
    assert!(
        wc_dir
            .join("gone/sub")
            .symlink_metadata()
            .unwrap()
            .file_type()
            .is_symlink(),
        "the refusal preserves the link"
    );
    assert!(
        std::fs::read_dir(&outsider).unwrap().next().is_none(),
        "the refusal writes nothing through the link"
    );

    // --force removes the LINK and materializes INSIDE the checkout; the
    // redirect target stays empty.
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Add {
                prefix: "gone".into(),
                force: true,
            },
            &net(&server),
        ),
        0
    );
    assert!(
        !wc_dir
            .join("gone/sub")
            .symlink_metadata()
            .map(|m| m.file_type().is_symlink())
            .unwrap_or(false),
        "--force removed the link"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/sub/x.txt")).unwrap(),
        x,
        "head content materialized inside the checkout"
    );
    assert!(
        std::fs::read_dir(&outsider).unwrap().next().is_none(),
        "--force never writes through the old link"
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
}

/// SPARSE GATE'S DIRTY-TRACKED ARM (qa-C2), both directions: an
/// ancestor/overlapping widen (the cone holds `gone/sub`; the widen is
/// `gone`) with a dirty tracked file under the widened prefix refuses
/// (exit 2, the path listed, everything preserved); `--force` materializes
/// over it (edit discarded) and the next sync completes clean.
#[test]
fn sparse_add_refuses_dirty_tracked_under_an_overlapping_widen_and_forces() {
    let server = start_server();
    create_repo(&server, "overlap");
    let k = b"k\n";
    let p = b"p from head\n";
    let x = b"x from head\n";
    let n = b"n\n";
    put_blob(&server, "overlap", k);
    put_blob(&server, "overlap", p);
    put_blob(&server, "overlap", x);
    put_blob(&server, "overlap", n);
    let c1 = changeset(
        &server,
        "overlap",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "gone/sub/x.txt", x),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "overlap", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/overlap", server.base),
            &["gone/sub".to_string(), "keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    // The user edits the tracked file under the to-be-widened ancestor.
    let mine = b"my local x work\n";
    write_file(&wc_dir, "gone/sub/x.txt", mine);
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(report.modified, vec!["gone/sub/x.txt".to_string()]);

    // The overlapping widen refuses (exit 2) and lists the dirty file.
    let out = run_binary(&server, &wc_dir, &["sparse", "add", "gone"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "the dirty tracked arm must refuse: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("gone/sub/x.txt"),
        "the dirty tracked file is listed: {stderr:?}"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/sub/x.txt")).unwrap(),
        mine,
        "the refusal preserves the edit"
    );
    assert!(
        !wc_dir.join("gone/p.txt").exists(),
        "the refusal materializes nothing"
    );

    // --force materializes over the edit: head content everywhere under the
    // widened ancestor, index consistent, status clean.
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Add {
                prefix: "gone".into(),
                force: true,
            },
            &net(&server),
        ),
        0
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/sub/x.txt")).unwrap(),
        x,
        "--force discards the edit (materialized over)"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        p,
        "the widen materialized the rest of the ancestor"
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());

    // The next sync completes clean on top of the widened cone.
    let c2 = changeset(
        &server,
        "overlap",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("add", "keep/new.txt", n)],
        "c2 adds new.txt",
    );
    seed_main(&server, "overlap", c2["id"].as_str().unwrap());
    assert_eq!(verbs::sync::run(&wc_dir, false, &net(&server)), 0);
    assert_eq!(std::fs::read(wc_dir.join("keep/new.txt")).unwrap(), n);
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());
}

/// Index snapshot for the unchanged pins: (path, blob, size, exec) rows.
fn index_rows(wc: &naatre_cli::wc::Wc) -> Vec<(String, [u8; 32], u64, bool)> {
    let mut v: Vec<_> = wc
        .load_index()
        .unwrap()
        .rows
        .iter()
        .map(|(p, r)| (p.clone(), r.blob, r.size, r.exec))
        .collect();
    v.sort();
    v
}

/// State snapshot for the unchanged pins: every state.json field.
fn state_sig(wc: &naatre_cli::wc::Wc) -> (String, Option<String>, Vec<String>, String) {
    let s = wc.load_state().unwrap();
    (
        s.head.clone(),
        s.base.clone(),
        s.sparse.clone(),
        s.server_repo.clone(),
    )
}

/// FAILED FETCH DISCARDS NOTHING, EVEN WITH --force (qa-C1): both force
/// verbs are driven into a failed `fetch_blobs_verified` (a corrupted
/// server blob file, the corrupt_server_blob_is_a_clean_error technique)
/// while a collider sits on the discard list: each verb exits non-zero,
/// the colliding untracked file survives byte-identical, nothing is
/// materialized, and index/state are unchanged. Restoring the blob bytes
/// then lets the SAME command succeed, pinning the fetch-before-discard
/// ordering — the fetch failure, not the gate, stopped the discard.
#[test]
fn failed_fetch_discards_nothing_even_with_force() {
    let server = start_server();

    // -- sync --force: untracked collider at an incoming add path. --
    create_repo(&server, "fchsync");
    let a1 = b"a1\n";
    let n2 = b"server new.txt\n";
    put_blob(&server, "fchsync", a1);
    put_blob(&server, "fchsync", n2);
    let c1 = changeset(
        &server,
        "fchsync",
        &[],
        vec![change("add", "a.txt", a1)],
        "c1",
    );
    seed_main(&server, "fchsync", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/fchsync", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    let c2 = changeset(
        &server,
        "fchsync",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("add", "new.txt", n2)],
        "c2",
    );
    seed_main(&server, "fchsync", c2["id"].as_str().unwrap());
    let mine = b"my untracked collider\n";
    write_file(&wc_dir, "new.txt", mine);

    // Corrupt the incoming blob's CAS file so the client hash gate fails.
    let hex = blob_id(n2);
    let shard = server
        ._dir
        .path()
        .join("objects")
        .join(&hex[0..2])
        .join(&hex[2..4])
        .join(&hex[4..]);
    let saved = std::fs::read(&shard).unwrap();
    std::fs::write(&shard, b"corrupted bytes that fail the hash gate").unwrap();

    let wc = naatre_cli::wc::Wc::open(&wc_dir).unwrap();
    let state_before = state_sig(&wc);
    let rows_before = index_rows(&wc);

    let out = run_binary(&server, &wc_dir, &["sync", "--force"]);
    assert_ne!(
        out.status.code(),
        Some(0),
        "the failed fetch must fail the sync: {out:?}"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("new.txt")).unwrap(),
        mine,
        "--force must not discard the collider when the fetch failed"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("a.txt")).unwrap(),
        a1,
        "nothing materialized over tracked content either"
    );
    assert_eq!(state_sig(&wc), state_before, "state.json unchanged");
    assert_eq!(index_rows(&wc), rows_before, "index unchanged");

    // Positive control: restored blob bytes let the SAME command succeed
    // and discard exactly the named collider.
    std::fs::write(&shard, &saved).unwrap();
    let out = run_binary(&server, &wc_dir, &["sync", "--force"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(std::fs::read(wc_dir.join("new.txt")).unwrap(), n2);
    assert_eq!(wc.load_state().unwrap().head, c2["id"].as_str().unwrap());

    // -- sparse add --force: untracked collider on the widened path. --
    create_repo(&server, "fchsparse");
    let k = b"k\n";
    let p = b"p from head\n";
    put_blob(&server, "fchsparse", k);
    put_blob(&server, "fchsparse", p);
    let sc1 = changeset(
        &server,
        "fchsparse",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "fchsparse", sc1["id"].as_str().unwrap());
    let sp_dir = server._dir.path().join("sp");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/fchsparse", server.base),
            &["keep".to_string()],
            Some(sp_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );
    let mine_p = b"my out-of-cone file\n";
    write_file(&sp_dir, "gone/p.txt", mine_p);

    let hex_p = blob_id(p);
    let shard_p = server
        ._dir
        .path()
        .join("objects")
        .join(&hex_p[0..2])
        .join(&hex_p[2..4])
        .join(&hex_p[4..]);
    let saved_p = std::fs::read(&shard_p).unwrap();
    std::fs::write(&shard_p, b"corrupted bytes that fail the hash gate").unwrap();

    let sp_wc = naatre_cli::wc::Wc::open(&sp_dir).unwrap();
    let sp_state_before = state_sig(&sp_wc);
    let sp_rows_before = index_rows(&sp_wc);

    let out = run_binary(&server, &sp_dir, &["sparse", "add", "gone", "--force"]);
    assert_ne!(
        out.status.code(),
        Some(0),
        "the failed fetch must fail the widen: {out:?}"
    );
    assert_eq!(
        std::fs::read(sp_dir.join("gone/p.txt")).unwrap(),
        mine_p,
        "the collider survives the failed fetch byte-identical"
    );
    assert_eq!(
        std::fs::read(sp_dir.join("keep/k.txt")).unwrap(),
        k,
        "nothing else materialized either"
    );
    assert_eq!(state_sig(&sp_wc), sp_state_before, "state.json unchanged");
    assert_eq!(index_rows(&sp_wc), sp_rows_before, "index unchanged");

    std::fs::write(&shard_p, &saved_p).unwrap();
    let out = run_binary(&server, &sp_dir, &["sparse", "add", "gone", "--force"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(
        std::fs::read(sp_dir.join("gone/p.txt")).unwrap(),
        p,
        "the restored fetch lets the widen discard the collider and materialize"
    );
    assert_eq!(
        state_sig(&sp_wc),
        (
            sc1["id"].as_str().unwrap().to_string(),
            None,
            vec!["keep".to_string(), "gone".to_string()],
            "fchsparse".to_string()
        )
    );
}

/// SPARSE GATE'S DELETED-TRACKED ARM (qa-A2), the missing half of qa-C2's
/// modified arm: a locally DELETED tracked file under an overlapping widen
/// (the cone holds `gone/sub`; the widen is `gone`) refuses — exit 2,
/// `dirty_working_copy`, the path listed, nothing materialized — and
/// `--force` proceeds, materializing head content over the deletion.
#[test]
fn sparse_add_refuses_a_locally_deleted_tracked_file_under_a_widen_and_forces() {
    let server = start_server();
    create_repo(&server, "overdel");
    let k = b"k\n";
    let p = b"p from head\n";
    let x = b"x from head\n";
    put_blob(&server, "overdel", k);
    put_blob(&server, "overdel", p);
    put_blob(&server, "overdel", x);
    let c1 = changeset(
        &server,
        "overdel",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "gone/sub/x.txt", x),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "overdel", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/overdel", server.base),
            &["gone/sub".to_string(), "keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );

    // The user DELETES the tracked file under the to-be-widened ancestor.
    std::fs::remove_file(wc_dir.join("gone/sub/x.txt")).unwrap();
    let report = verbs::status::report(&wc_dir).unwrap();
    assert_eq!(report.deleted, vec!["gone/sub/x.txt".to_string()]);

    // The overlapping widen refuses (exit 2) and lists the deleted file.
    let out = run_binary(&server, &wc_dir, &["sparse", "add", "gone"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "the deleted tracked arm must refuse: {out:?}"
    );
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(
        stderr.contains("gone/sub/x.txt"),
        "the deleted tracked file is listed: {stderr:?}"
    );
    assert!(
        !wc_dir.join("gone/p.txt").exists(),
        "the refusal materializes nothing"
    );
    assert!(
        !wc_dir.join("gone/sub/x.txt").exists(),
        "the refusal does not resurrect the deletion either"
    );

    // --force proceeds: head content everywhere under the widened ancestor,
    // index consistent, status clean.
    assert_eq!(
        verbs::sparse::run(
            &wc_dir,
            verbs::sparse::Action::Add {
                prefix: "gone".into(),
                force: true,
            },
            &net(&server),
        ),
        0
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/sub/x.txt")).unwrap(),
        x,
        "--force materializes over the deletion"
    );
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        p,
        "the widen materialized the rest of the ancestor"
    );
    assert!(verbs::status::report(&wc_dir).unwrap().is_clean());
}

/// BINARY PARSE PIN (qa-A3): the real binary accepts `sparse add <prefix>
/// --force` — sync's binary --force is already pinned in
/// binary_sync_family; sparse add's was not. Without the flag the collider
/// refuses (exit 2); with it the same invocation proceeds and discards
/// exactly the collider.
#[test]
fn binary_sparse_add_force_parses_and_proceeds() {
    let server = start_server();
    create_repo(&server, "binforce");
    let k = b"k\n";
    let p = b"p from head\n";
    put_blob(&server, "binforce", k);
    put_blob(&server, "binforce", p);
    let c1 = changeset(
        &server,
        "binforce",
        &[],
        vec![
            change("add", "gone/p.txt", p),
            change("add", "keep/k.txt", k),
        ],
        "c1",
    );
    seed_main(&server, "binforce", c1["id"].as_str().unwrap());
    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/binforce", server.base),
            &["keep".to_string()],
            Some(wc_dir.to_str().unwrap()),
            &net(&server),
        ),
        0
    );
    let mine = b"my untracked collider\n";
    write_file(&wc_dir, "gone/p.txt", mine);

    // Plain add refuses the collider...
    let out = run_binary(&server, &wc_dir, &["sparse", "add", "gone"]);
    assert_eq!(out.status.code(), Some(2), "collider refuses: {out:?}");
    assert_eq!(std::fs::read(wc_dir.join("gone/p.txt")).unwrap(), mine);

    // ...`--force` parses through the real binary and proceeds.
    let out = run_binary(&server, &wc_dir, &["sparse", "add", "gone", "--force"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(
        std::fs::read(wc_dir.join("gone/p.txt")).unwrap(),
        p,
        "the collider was discarded and head content materialized"
    );
    assert_eq!(std::fs::read(wc_dir.join("keep/k.txt")).unwrap(), k);
    let out = run_binary(&server, &wc_dir, &["sparse", "list"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(stdout_of(&out).contains("gone"), "{}", stdout_of(&out));
}

// ---- M5a: snapshot + restore ------------------------------------------------

/// Runs the real binary's `snapshot` verb and parses its JSON line.
fn run_snapshot(server: &TestServer, wc_dir: &Path, extra: &[&str]) -> Value {
    let mut args = vec!["snapshot"];
    args.extend_from_slice(extra);
    let out = run_binary(server, wc_dir, &args);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    serde_json::from_str(stdout_of(&out).trim()).expect("snapshot prints {\"id\",\"expires_ns\"}")
}

/// (relative path → bytes) for every file under the data dir's `objects/`.
fn objects_dir(server: &TestServer) -> std::collections::BTreeMap<String, Vec<u8>> {
    let mut out = std::collections::BTreeMap::new();
    let mut stack = vec![server._dir.path().join("objects")];
    while let Some(dir) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                stack.push(path);
            } else {
                out.insert(
                    path.strip_prefix(server._dir.path())
                        .unwrap()
                        .to_string_lossy()
                        .into_owned(),
                    std::fs::read(&path).expect("object bytes"),
                );
            }
        }
    }
    out
}

fn set_exec(path: &Path) {
    use std::os::unix::fs::PermissionsExt as _;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755)).unwrap();
}

/// Test — SNAPSHOT/RESTORE ROUNDTRIP: the snapshot's blobs live under
/// `ephemeral/<sid>/` and never under `objects/`; a fresh working copy
/// restores the exact bytes and exec bits; the history CAS is untouched and
/// the restored copy's head is unchanged (snapshots are working state, not
/// history).
#[test]
fn snapshot_restore_roundtrip_is_byte_identical_and_spares_history_cas() {
    let server = start_server();
    create_repo(&server, "snap");
    let base = b"base content\n";
    put_blob(&server, "snap", base);
    let c1 = changeset(
        &server,
        "snap",
        &[],
        vec![change("add", "base.txt", base)],
        "c1",
    );
    seed_main(&server, "snap", c1["id"].as_str().unwrap());

    let wc_a = server._dir.path().join("wc-a");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/snap", server.base),
            &[],
            Some(wc_a.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    let a_txt = b"alpha bytes\n";
    let f_txt = b"#!/bin/sh\necho hi\n";
    let nested = b"nested payload\n";
    write_file(&wc_a, "a.txt", a_txt);
    write_file(&wc_a, "sub/f.txt", f_txt);
    write_file(&wc_a, "sub/n.bin", nested);
    set_exec(&wc_a.join("sub/f.txt"));

    let before = objects_dir(&server);
    let snap = run_snapshot(&server, &wc_a, &[]);
    let sid = snap["id"].as_str().unwrap().to_string();
    assert_eq!(sid.len(), 64, "64-hex snapshot id");
    assert!(snap["expires_ns"].as_u64().unwrap() > 0);

    // Blobs under ephemeral/<sid>/objects/, absent from objects/.
    let eph = server
        ._dir
        .path()
        .join("ephemeral")
        .join(&sid)
        .join("objects");
    assert!(eph.is_dir(), "snapshot namespace exists");
    for content in [&a_txt[..], &f_txt[..], &nested[..]] {
        let hex = blob_id(content);
        assert!(
            eph.join(&hex[0..2])
                .join(&hex[2..4])
                .join(&hex[4..])
                .exists(),
            "blob {hex} stored in the snapshot namespace"
        );
    }
    assert_eq!(
        objects_dir(&server),
        before,
        "history CAS untouched by snapshot"
    );

    // Fresh dir: a second clone restores the snapshot byte-identically.
    let wc_b = server._dir.path().join("wc-b");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/snap", server.base),
            &[],
            Some(wc_b.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    let out = run_binary(&server, &wc_b, &["restore", &sid]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(std::fs::read(wc_b.join("a.txt")).unwrap(), a_txt);
    assert_eq!(std::fs::read(wc_b.join("sub/f.txt")).unwrap(), f_txt);
    assert_eq!(std::fs::read(wc_b.join("sub/n.bin")).unwrap(), nested);
    use std::os::unix::fs::MetadataExt as _;
    assert!(
        std::fs::metadata(wc_b.join("sub/f.txt")).unwrap().mode() & 0o111 != 0,
        "exec bit restored"
    );
    assert!(
        std::fs::metadata(wc_b.join("a.txt")).unwrap().mode() & 0o111 == 0,
        "non-exec stays non-exec"
    );
    // Head unchanged; the index moved to the restored state → clean status.
    let state_b = naatre_cli::wc::Wc::open(&wc_b)
        .unwrap()
        .load_state()
        .unwrap();
    assert_eq!(state_b.head, c1["id"].as_str().unwrap(), "head unchanged");
    assert!(verbs::status::report(&wc_b).unwrap().is_clean());
    assert_eq!(objects_dir(&server), before, "restore never writes history");
}

/// Test — RESTORE DIRTY GATE (plan quote): conflicting uncommitted edits OR
/// colliding untracked files refuse with `dirty_working_copy` (details
/// enumerate both), everything is preserved, and `--force` discards exactly
/// the colliders.
#[test]
fn restore_dirty_gate_refuses_both_lists_and_force_discards_exactly_colliders() {
    let server = start_server();
    create_repo(&server, "gate");
    let base = b"base v1\n";
    put_blob(&server, "gate", base);
    let c1 = changeset(
        &server,
        "gate",
        &[],
        vec![change("add", "base.txt", base)],
        "c1",
    );
    seed_main(&server, "gate", c1["id"].as_str().unwrap());

    let wc = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/gate", server.base),
            &[],
            Some(wc.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    // Untracked file that rides into the snapshot (add-all semantics).
    let a1 = b"alpha v1\n";
    write_file(&wc, "a.txt", a1);
    let snap = run_snapshot(&server, &wc, &[]);
    let sid = snap["id"].as_str().unwrap().to_string();

    // Conflicting edit (tracked, on a snapshot path) + the untracked
    // collider (a.txt is still untracked — snapshot moves no metadata) +
    // an unrelated untracked file that must survive.
    let edit = b"my local edit\n";
    write_file(&wc, "base.txt", edit);
    write_file(&wc, "keep.txt", b"unrelated untracked\n");

    let out = run_binary(&server, &wc, &["restore", &sid]);
    assert_eq!(out.status.code(), Some(2), "gate refuses: {out:?}");
    let stderr = stderr_of(&out);
    assert!(stderr.contains("dirty_working_copy"), "{stderr:?}");
    assert!(stderr.contains("base.txt"), "edits listed: {stderr:?}");
    assert!(stderr.contains("a.txt"), "colliders listed: {stderr:?}");
    // Preserve everything.
    assert_eq!(std::fs::read(wc.join("base.txt")).unwrap(), edit);
    assert_eq!(std::fs::read(wc.join("a.txt")).unwrap(), a1);
    assert_eq!(
        std::fs::read(wc.join("keep.txt")).unwrap(),
        b"unrelated untracked\n"
    );

    // --force discards exactly the colliders: base.txt and a.txt come back
    // as snapshotted, keep.txt survives.
    let out = run_binary(&server, &wc, &["restore", "--force", &sid]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert_eq!(std::fs::read(wc.join("base.txt")).unwrap(), base);
    assert_eq!(std::fs::read(wc.join("a.txt")).unwrap(), a1);
    assert_eq!(
        std::fs::read(wc.join("keep.txt")).unwrap(),
        b"unrelated untracked\n",
        "--force never touches unrelated files"
    );
    let report = verbs::status::report(&wc).unwrap();
    assert_eq!(report.untracked, vec!["keep.txt".to_string()]);
    assert!(report.modified.is_empty() && report.deleted.is_empty());
    let state = naatre_cli::wc::Wc::open(&wc).unwrap().load_state().unwrap();
    assert_eq!(state.head, c1["id"].as_str().unwrap(), "head unchanged");
}

/// Test — INTERRUPTED RESTORE: a materialization failure mid-restore (the
/// target path turned into a directory) aborts the verb; every completed
/// file is complete, nothing half-written or temp-staged survives, and the
/// metadata (head, index) is untouched — saves happen only at the end.
#[test]
fn interrupted_restore_leaves_nothing_partial() {
    let server = start_server();
    create_repo(&server, "part");
    let base = b"base\n";
    put_blob(&server, "part", base);
    let c1 = changeset(
        &server,
        "part",
        &[],
        vec![change("add", "base.txt", base)],
        "c1",
    );
    seed_main(&server, "part", c1["id"].as_str().unwrap());

    let wc = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/part", server.base),
            &[],
            Some(wc.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    let a_txt = b"complete alpha\n";
    let z_txt = b"will fail to materialize\n";
    write_file(&wc, "a.txt", a_txt);
    write_file(&wc, "z.txt", z_txt);
    let snap = run_snapshot(&server, &wc, &[]);
    let sid = snap["id"].as_str().unwrap().to_string();

    // Sabotage: z.txt becomes a directory — the snapshot still contains it
    // as a file, and the write fails mid-restore (after a.txt landed).
    std::fs::remove_file(wc.join("z.txt")).unwrap();
    std::fs::create_dir(wc.join("z.txt")).unwrap();
    let index_before = naatre_cli::wc::Wc::open(&wc).unwrap().load_index().unwrap();

    let out = run_binary(&server, &wc, &["restore", "--force", &sid]);
    assert_eq!(
        out.status.code(),
        Some(1),
        "the failed write is a generic error: {out:?}"
    );

    // Nothing partial: the earlier file is complete, the sabotaged path is
    // untouched, no temp leftovers, and metadata moved nowhere.
    assert_eq!(
        std::fs::read(wc.join("a.txt")).unwrap(),
        a_txt,
        "completed file is whole"
    );
    assert!(
        wc.join("z.txt").is_dir(),
        "the failed target is exactly as it was"
    );
    fn has_temp_leftovers(dir: &Path) -> bool {
        for entry in std::fs::read_dir(dir).unwrap().flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            if name.contains(".naatre-tmp-") {
                return true;
            }
            if entry.file_type().unwrap().is_dir()
                && !name.starts_with('.')
                && has_temp_leftovers(&entry.path())
            {
                return true;
            }
        }
        false
    }
    assert!(
        !has_temp_leftovers(&wc),
        "the atomic-write error path cleaned its temp file"
    );
    let state = naatre_cli::wc::Wc::open(&wc).unwrap().load_state().unwrap();
    assert_eq!(state.head, c1["id"].as_str().unwrap(), "head unchanged");
    let index_after = naatre_cli::wc::Wc::open(&wc).unwrap().load_index().unwrap();
    assert_eq!(
        index_after.rows.keys().collect::<Vec<_>>(),
        index_before.rows.keys().collect::<Vec<_>>(),
        "index saved only at the end: untouched by the abort"
    );
}

/// BINARY PARSE PIN: `snapshot --ttl` below the 60 s floor (and a
/// non-duration) is a usage error (exit 3), enforced client-side before any
/// server traffic.
#[test]
fn binary_snapshot_rejects_ttl_below_the_minute() {
    let server = start_server();
    let cwd = server._dir.path();
    for bad in [
        ["snapshot", "--ttl", "30s"],
        ["snapshot", "--ttl", "0"],
        ["snapshot", "--ttl", "nonsense"],
    ] {
        let out = run_binary(&server, cwd, &bad);
        assert_eq!(out.status.code(), Some(3), "{bad:?}: {out:?}");
        let stderr = stderr_of(&out);
        assert!(stderr.contains("--ttl"), "{bad:?}: {stderr:?}");
    }
}

/// PIN (fix3b): CLIENT-side expired restore — with the snapshot past its
/// TTL (the server clock advanced past `expires_ns`), the `restore` verb
/// exits non-zero with the `snapshot_expired` message. The server-side gate
/// itself (both fetch gates answering 410) is pinned in naatre-server's
/// `m5a_test.rs` with the injected clock; this pins the client surfacing.
/// Expiry is advanced by backdating `expires_ns` — the exact column the
/// fetch-time gate compares against `now_ns` — instead of sleeping out a
/// 60 s TTL floor.
#[test]
fn restore_expired_snapshot_exits_non_zero_with_snapshot_expired() {
    let server = start_server();
    create_repo(&server, "exp");
    let base = b"base content\n";
    put_blob(&server, "exp", base);
    let c1 = changeset(
        &server,
        "exp",
        &[],
        vec![change("add", "base.txt", base)],
        "c1",
    );
    seed_main(&server, "exp", c1["id"].as_str().unwrap());

    let wc = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/exp", server.base),
            &[],
            Some(wc.to_str().unwrap()),
            &net(&server)
        ),
        0
    );
    let snap = run_snapshot(&server, &wc, &[]);
    let sid = snap["id"].as_str().unwrap().to_string();

    let set_expiry = |secs_from_now: i64| {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos() as i64;
        let conn = rusqlite::Connection::open(&server.db_path).unwrap();
        conn.busy_timeout(Duration::from_secs(5)).unwrap();
        conn.execute(
            "UPDATE snapshots SET expires_ns = ?1 WHERE id = ?2",
            rusqlite::params![
                now + secs_from_now * 1_000_000_000,
                (0..sid.len())
                    .step_by(2)
                    .map(|i| u8::from_str_radix(&sid[i..i + 2], 16).unwrap())
                    .collect::<Vec<u8>>()
            ],
        )
        .unwrap();
    };

    // Clock past the TTL: the verb refuses with the stable code, cleanly.
    set_expiry(-3600);
    let out = run_binary(&server, &wc, &["restore", &sid]);
    assert_ne!(
        out.status.code(),
        Some(0),
        "expired restore must exit non-zero: {}",
        stdout_of(&out)
    );
    let stderr = stderr_of(&out);
    assert!(
        stderr.contains("snapshot_expired"),
        "expired restore must name the code: {stderr:?}"
    );

    // Positive control: the same command succeeds once the snapshot is
    // un-expired again — the failure was the expiry gate, nothing else.
    set_expiry(3600);
    let out = run_binary(&server, &wc, &["restore", &sid]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(
        stdout_of(&out).contains("restored"),
        "un-expired restore succeeds: {}",
        stdout_of(&out)
    );
}

/// M6b — BLAME CLI: the pinned human line format (`<8-hex cs> <author> | <line>`),
/// the pinned `--json` shape (`{"lines":[{cs,author,text}]}` with full hex
/// ids), and the binary structured-empty note. Runs against the real
/// server; the queue drains in the background, so the read waits for
/// derivation with a bounded progress poll.
#[test]
fn blame_cli_prints_the_pinned_formats() {
    let server = start_server_with(|state| {
        for deriver in naatre_server::derived::production_kinds() {
            state.register_deriver(deriver).expect("register deriver");
        }
    });
    create_repo(&server, "blamecli");
    let v1 = b"alpha\nbeta\ngamma\n";
    let v2 = b"alpha\nBETA\ngamma\ndelta\n";
    put_blob(&server, "blamecli", v1);
    put_blob(&server, "blamecli", v2);
    let c1 = changeset(
        &server,
        "blamecli",
        &[],
        vec![change("add", "story.txt", v1)],
        "c1",
    );
    let c2 = changeset(
        &server,
        "blamecli",
        &[c1["id"].as_str().unwrap().to_string()],
        vec![change("mod", "story.txt", v2)],
        "c2",
    );
    let c1_hex = c1["id"].as_str().unwrap();
    let c2_hex = c2["id"].as_str().unwrap();
    seed_main(&server, "blamecli", c2_hex);

    let wc_dir = server._dir.path().join("wc");
    assert_eq!(
        verbs::clone::run(
            &format!("{}/blamecli", server.base),
            &[],
            Some(wc_dir.to_str().unwrap()),
            &net(&server)
        ),
        0
    );

    // Wait for the background queue: blame 503s (derivation_pending) until
    // the derived index covers the chain; poll with progress, then act.
    let mut derived = false;
    for _ in 0..600 {
        let (status, _) = api(
            &server,
            "GET",
            &format!("/v1/repos/blamecli/blame?at={c2_hex}&path=story.txt"),
            None,
        );
        if status == 200 {
            derived = true;
            break;
        }
        assert_eq!(status, 503, "pending until derived");
        std::thread::sleep(Duration::from_millis(50));
    }
    assert!(derived, "queue never drained the blame index");

    // Human format, pinned: <8-hex cs> <author> | <line>.
    let out = run_binary(&server, &wc_dir, &["blame", "story.txt"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let c1_8 = &c1_hex[..8];
    let c2_8 = &c2_hex[..8];
    let expected = format!(
        "{c1_8} test <test@naatre.local> | alpha\n\
         {c2_8} test <test@naatre.local> | BETA\n\
         {c1_8} test <test@naatre.local> | gamma\n\
         {c2_8} test <test@naatre.local> | delta\n"
    );
    assert_eq!(stdout_of(&out), expected, "pinned human line format");

    // --json shape, pinned: {"lines":[{cs,author,text}]} with full hex ids.
    let out = run_binary(&server, &wc_dir, &["blame", "story.txt", "--json"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    let v: Value = serde_json::from_str(stdout_of(&out).trim()).expect("json line");
    let lines = v["lines"].as_array().expect("lines array");
    assert_eq!(lines.len(), 4);
    assert_eq!(lines[0]["cs"], json!(c1_hex));
    assert_eq!(lines[0]["author"], json!("test <test@naatre.local>"));
    assert_eq!(lines[0]["text"], json!("alpha"));
    assert_eq!(lines[1]["cs"], json!(c2_hex));
    assert_eq!(lines[1]["text"], json!("BETA"));
    assert_eq!(v.as_object().unwrap().len(), 1, "exactly the lines key");

    // A binary file is the structured empty result: exit 0, no stdout,
    // the stable code on stderr.
    let bin = [0x00u8, 0xFF, 0xFE, 0x01];
    put_blob(&server, "blamecli", &bin);
    let c3 = changeset(
        &server,
        "blamecli",
        &[c2_hex.to_string()],
        vec![change("add", "blob.bin", &bin)],
        "c3",
    );
    seed_main(&server, "blamecli", c3["id"].as_str().unwrap());
    let mut binary_ready = false;
    for _ in 0..600 {
        let (status, _) = api(
            &server,
            "GET",
            &format!(
                "/v1/repos/blamecli/blame?at={}&path=blob.bin",
                c3["id"].as_str().unwrap()
            ),
            None,
        );
        if status == 200 {
            binary_ready = true;
            break;
        }
        assert_eq!(status, 503);
        std::thread::sleep(Duration::from_millis(50));
    }
    assert!(binary_ready, "queue never drained the binary blame");
    let out = run_binary(&server, &wc_dir, &["blame", "blob.bin", "--at", "main"]);
    assert_eq!(out.status.code(), Some(0), "{}", stderr_of(&out));
    assert!(
        out.stdout.is_empty(),
        "no blame lines for binary: {:?}",
        stdout_of(&out)
    );
    let stderr = stderr_of(&out);
    assert!(
        stderr.contains("blame_binary"),
        "the stable binary code is noted: {stderr:?}"
    );
}
