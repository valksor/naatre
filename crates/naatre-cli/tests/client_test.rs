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
            let state = ServerState::open(data).expect("server state");
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
    let res = agent()
        .put(&url)
        .header("Authorization", format!("Bearer {}", server.token))
        .send(data)
        .expect("put blob");
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
    let (status, created) = api(
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
    let (status, body) = api(server, "POST", "/v1/repos", Some(json!({ "name": name })));
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
    assert!(stdout_of(&out).contains("corrupt"), "{}", stdout_of(&out));
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
            verbs::sparse::Action::Add("docs".into()),
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
            verbs::sparse::Action::Add("docs".into()),
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
