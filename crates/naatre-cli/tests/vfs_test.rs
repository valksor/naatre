//! M8a in-process VFS suite: drives [`NaatreVfs`] directly against the REAL
//! server (no kernel mount — the CI-feasible harness the plan pins). The
//! named M8 checks live here: byte-exact mount-vs-materialized parity (full
//! walk + hash + exec bits), stat-never-fetches (recording source),
//! corrupt-cache recovery, daemon SIGKILL cleanup (through the real
//! binary), and ref pinning. The writable mode's EROFS default, grammar/NFC
//! create enforcement, and stage-not-land flush run against the real
//! server; hydration-error typing is pinned by the in-memory double in
//! `src/vfs.rs` (the server itself refuses to create the broken states).

use std::io::BufRead as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::Arc;
use std::time::Duration;

use naatre_cli::client::Client;
use naatre_cli::verbs::{self, Net};
use naatre_cli::vfs::{
    self, ERRNO_READ_ONLY, EntryMeta, HttpMountSource, MemorySource, NaatreVfs, RecordingSource,
    VfsError,
};
use naatre_cli::wc::Wc;
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::tree::EntryKind;
use naatre_server::state::ServerState;
use naatre_store::DataDir;
use serde_json::{Value, json};

// ---- server harness (the client_test.rs shape) -----------------------------

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

// ---- HTTP setup helpers -----------------------------------------------------

fn agent() -> ureq::Agent {
    ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .build(),
    )
}

/// True when (status, envelope code) is the single-writer contention the
/// setup retry loop exists for (writer_busy envelopes only).
fn writer_busy_503(status: u16, code: &str) -> bool {
    status == 503 && code == "writer_busy"
}

fn api(server: &TestServer, method: &str, path: &str, body: Option<Value>) -> (u16, Value) {
    let url = format!("{}{path}", server.base);
    let auth = format!("Bearer {}", server.token);
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
        _ => panic!("api helper: unsupported {method}"),
    };
    let status = res.status().as_u16();
    let value: Value = res.into_body().read_json().expect("json body");
    (status, value)
}

/// Bounded setup retry for the single-writer 503 (see client_test.rs).
fn api_setup_retry(
    server: &TestServer,
    method: &str,
    path: &str,
    body: Option<Value>,
) -> (u16, Value) {
    let mut waited = 0u64;
    loop {
        let (status, body) = api(server, method, path, body.clone());
        let code = body["error"]["code"].as_str().unwrap_or("");
        if !writer_busy_503(status, code) || waited >= 2000 {
            return (status, body);
        }
        std::thread::sleep(Duration::from_millis(20));
        waited += 20;
    }
}

fn put_blob(server: &TestServer, repo: &str, data: &[u8]) {
    let url = format!(
        "{}/v1/repos/{repo}/objects/{}",
        server.base,
        object_id(Kind::Blob, data).to_hex()
    );
    let mut waited = 0u64;
    let status = loop {
        let res = agent()
            .put(&url)
            .header("Authorization", format!("Bearer {}", server.token))
            .send(data)
            .expect("put blob");
        let status = res.status().as_u16();
        let body: Value = res.into_body().read_json().unwrap_or(Value::Null);
        let code = body["error"]["code"].as_str().unwrap_or("");
        if !writer_busy_503(status, code) || waited >= 2000 {
            break status;
        }
        std::thread::sleep(Duration::from_millis(20));
        waited += 20;
    };
    assert!((200..300).contains(&status), "blob PUT failed: {status}");
}

fn create_repo(server: &TestServer, name: &str) {
    let (status, body) =
        api_setup_retry(server, "POST", "/v1/repos", Some(json!({ "name": name })));
    assert!(
        status == 201 || (status == 409 && body["error"]["code"] == "repo_exists"),
        "repo create failed: {status} {body}"
    );
}

/// Seeds (or advances) the `main` ref row directly (M2a technique).
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

// ---- repo + VFS helpers ------------------------------------------------------

fn net(server: &TestServer) -> Net {
    Net {
        server: Some(server.base.clone()),
        token: Some(server.token.clone()),
    }
}

/// One seeded file: (path, bytes, exec).
fn change(path: &str, data: &[u8], exec: bool) -> Value {
    json!({
        "path": path,
        "op": "add",
        "blob": object_id(Kind::Blob, data).to_hex(),
        "size": data.len(),
        "exec": exec,
    })
}

/// Creates a repo with one changeset carrying `files` (sorted, exec-aware),
/// seeds main, returns the changeset id hex.
fn seed_repo(server: &TestServer, repo: &str, files: &[(&str, &[u8], bool)]) -> String {
    create_repo(server, repo);
    for (_, data, _) in files {
        put_blob(server, repo, data);
    }
    let mut changes: Vec<Value> = files.iter().map(|(p, d, e)| change(p, d, *e)).collect();
    changes.sort_by(|a, b| a["path"].as_str().cmp(&b["path"].as_str()));
    let body = json!({
        "parents": [],
        "author": "test <test@naatre.local>",
        "message": "seed",
        "changes": changes,
    });
    let (status, created) = api_setup_retry(
        server,
        "POST",
        &format!("/v1/repos/{repo}/changesets"),
        Some(body),
    );
    assert_eq!(status, 201, "changeset create failed: {created}");
    let cs = created["id"].as_str().expect("changeset id").to_string();
    seed_main(server, repo, &cs);
    cs
}

fn tmp_mount_dir(tag: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!(
        "naatre-vfs-it-{tag}-{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    ));
    std::fs::create_dir_all(&dir).expect("mkdir");
    dir
}

/// The demo tree used across the suite: nested dirs, an exec file, binary
/// content.
fn demo_files() -> Vec<(&'static str, Vec<u8>, bool)> {
    vec![
        ("README.md", b"# demo\n".to_vec(), false),
        ("a/b/c/deep.txt", b"deep\n".to_vec(), false),
        ("data/bin.bin", vec![0u8, 0xFF, 0xFE, 1, 2, 3], false),
        ("scripts/run.sh", b"#!/bin/sh\necho hi\n".to_vec(), true),
        ("src/lib/mod.rs", b"pub mod x;\n".to_vec(), false),
        ("src/main.rs", b"fn main() {}\n".to_vec(), false),
    ]
}

/// Full recursive VFS walk: every file path with its stat metadata.
fn walk_vfs_files<S: vfs::MountSource>(v: &NaatreVfs<S>) -> Vec<(String, EntryMeta)> {
    let mut out = Vec::new();
    fn rec<S: vfs::MountSource>(v: &NaatreVfs<S>, dir: &str, out: &mut Vec<(String, EntryMeta)>) {
        for e in v.readdir(dir).expect("readdir") {
            let full = if dir.is_empty() {
                e.name.clone()
            } else {
                format!("{dir}/{}", e.name)
            };
            if e.meta.kind == EntryKind::Dir {
                rec(v, &full, out);
            } else {
                out.push((full, e.meta));
            }
        }
    }
    rec(v, "", &mut out);
    out.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    out
}

// ---- 1. byte-exact parity: mount view vs materialized clone -----------------

#[test]
fn mount_parity_with_materialized_clone_byte_for_byte() {
    let server = start_server();
    let files = demo_files();
    let refs: Vec<(&str, &[u8], bool)> = files
        .iter()
        .map(|(p, d, e)| (*p, d.as_slice(), *e))
        .collect();
    seed_repo(&server, "parity", &refs);

    // The materialized side: a real `naatre clone`.
    let out = tempfile::tempdir().expect("tempdir");
    let dest = out.path().join("wc");
    verbs::clone::value(
        &format!("{}/parity", server.base),
        &[],
        Some(dest.to_str().expect("utf8")),
        &net(&server),
    )
    .expect("clone");

    // The mounted side: the VFS over the same pinned ref.
    let dir = tmp_mount_dir("parity");
    let v = NaatreVfs::mount(
        HttpMountSource::new(
            Client::new(&server.base, Some(server.token.clone())),
            "parity",
        ),
        "parity",
        None,
        dir.clone(),
        false,
    )
    .expect("mount");

    // Same path sets.
    let index = Wc::open(&dest).expect("wc").load_index().expect("index");
    let clone_paths: Vec<&str> = index.rows.keys().map(String::as_str).collect();
    let walked = walk_vfs_files(&v);
    let mount_paths: Vec<&str> = walked.iter().map(|(p, _)| p.as_str()).collect();
    assert_eq!(clone_paths, mount_paths, "identical full walk");

    // Byte-exact per file, exec bit included, on BOTH sides' terms.
    for (path, meta) in &walked {
        let row = &index.rows[path];
        let bytes = v.read(path).expect("read");
        assert_eq!(
            bytes,
            std::fs::read(dest.join(path)).expect("disk bytes"),
            "{path}"
        );
        // Every byte hashes to its content address (the walk's "hash" leg).
        assert_eq!(object_id(Kind::Blob, &bytes).0, row.blob, "{path}");
        assert_eq!(meta.size, bytes.len() as u64, "{path}");
        assert_eq!(meta.exec, row.exec, "{path} exec via index");
        let disk_mode = std::fs::metadata(dest.join(path))
            .expect("stat")
            .permissions();
        use std::os::unix::fs::PermissionsExt as _;
        let disk_exec = disk_mode.mode() & 0o111 != 0;
        assert_eq!(meta.exec, disk_exec, "{path} exec via disk mode");
    }
    // The exec file is REALLY exec on disk and in the view.
    assert!(
        index.rows["scripts/run.sh"].exec,
        "fixture has an exec file"
    );
    let _ = std::fs::remove_dir_all(&dir);
}

// ---- 2. stat-never-fetches (recording source) -------------------------------

#[test]
fn stat_only_walk_issues_zero_blob_gets() {
    let server = start_server();
    let files = demo_files();
    let refs: Vec<(&str, &[u8], bool)> = files
        .iter()
        .map(|(p, d, e)| (*p, d.as_slice(), *e))
        .collect();
    seed_repo(&server, "statonly", &refs);

    let dir = tmp_mount_dir("statonly");
    let counter = Arc::new(RecordingSource::new(HttpMountSource::new(
        Client::new(&server.base, Some(server.token.clone())),
        "statonly",
    )));
    let v = NaatreVfs::mount(counter.clone(), "statonly", None, dir.clone(), false).expect("mount");

    // A stat-only walk of the whole tree: every dir listed, every file
    // statted — sizes and exec bits come from tree entries.
    let walked = walk_vfs_files(&v);
    assert_eq!(walked.len(), files.len());
    assert_eq!(
        counter.blob_gets(),
        0,
        "a stat-only walk must not fetch a single blob"
    );
    assert!(counter.tree_gets() > 0, "trees were fetched");

    // Reading then hydrates — and only once per blob even across repeats.
    assert_eq!(v.read("README.md").expect("read"), b"# demo\n");
    let after_first = counter.blob_gets();
    assert_eq!(after_first, 1, "one blob GET for one file");
    let _ = v.read("README.md").expect("read again (cache hit)");
    assert_eq!(
        counter.blob_gets(),
        after_first,
        "cached reads do not re-fetch"
    );
    let _ = std::fs::remove_dir_all(&dir);
}

// ---- 3. corrupt-cache recovery ----------------------------------------------

#[test]
fn corrupt_cache_evicts_and_rehydrates_correct_bytes() {
    let server = start_server();
    let files = demo_files();
    let refs: Vec<(&str, &[u8], bool)> = files
        .iter()
        .map(|(p, d, e)| (*p, d.as_slice(), *e))
        .collect();
    seed_repo(&server, "corrupt", &refs);

    let dir = tmp_mount_dir("corrupt");
    let v = NaatreVfs::mount(
        HttpMountSource::new(
            Client::new(&server.base, Some(server.token.clone())),
            "corrupt",
        ),
        "corrupt",
        None,
        dir.clone(),
        false,
    )
    .expect("mount");
    let original = v.read("src/main.rs").expect("read");

    // Exactly one blob cached, named by its id.
    let blobs_dir = dir.join("blobs");
    let cached: Vec<PathBuf> = std::fs::read_dir(&blobs_dir)
        .expect("cache dir")
        .flatten()
        .map(|e| e.path())
        .collect();
    assert_eq!(cached.len(), 1, "one hydrated blob is cached");
    assert_eq!(
        cached[0].file_name().unwrap().to_str().unwrap(),
        object_id(Kind::Blob, &original).to_hex(),
        "the cache file IS the blob id"
    );

    // Poison it (what a crashed write or disk rot leaves behind).
    std::fs::write(&cached[0], b"poisoned bytes").expect("poison");
    // The next read detects the mismatch, evicts, re-hydrates: correct
    // bytes come back and the cache file is restored.
    assert_eq!(v.read("src/main.rs").expect("read after poison"), original);
    assert_eq!(
        std::fs::read(&cached[0]).expect("cache file"),
        original,
        "the cache was repaired by re-hydration"
    );
    let _ = std::fs::remove_dir_all(&dir);
}

// ---- 4. ref pinned at mount time ---------------------------------------------

#[test]
fn ref_is_pinned_at_mount_time() {
    let server = start_server();
    let cs1 = seed_repo(&server, "pinned", &[("a.txt", b"one".as_slice(), false)]);

    let dir = tmp_mount_dir("pinned");
    let v = NaatreVfs::mount(
        HttpMountSource::new(
            Client::new(&server.base, Some(server.token.clone())),
            "pinned",
        ),
        "pinned",
        None,
        dir.clone(),
        false,
    )
    .expect("mount");
    assert_eq!(
        v.pin().changeset.to_hex(),
        cs1,
        "the mount pins main's target"
    );

    // Advance main: a second changeset adding b.txt.
    put_blob(&server, "pinned", b"two");
    let (status, created) = api_setup_retry(
        &server,
        "POST",
        "/v1/repos/pinned/changesets",
        Some(json!({
            "parents": [cs1],
            "author": "test <test@naatre.local>",
            "message": "advance",
            "changes": [change("b.txt", b"two", false)],
        })),
    );
    assert_eq!(status, 201, "{created}");
    seed_main(&server, "pinned", created["id"].as_str().expect("id"));

    // The running mount still serves the PINNED tree — the advance is
    // invisible (the documented pin).
    assert_eq!(v.read("a.txt").expect("pinned file"), b"one");
    assert!(
        matches!(v.stat("b.txt"), Err(VfsError::NotFound(_))),
        "new main's file is not in the pinned view"
    );
    let names: Vec<String> = v
        .readdir("")
        .expect("root")
        .into_iter()
        .map(|e| e.name)
        .collect();
    assert_eq!(names, vec!["a.txt".to_string()]);
    assert_eq!(v.pin().changeset.to_hex(), cs1, "pin never moves");

    // A NEW mount pins the advanced ref — pin-at-mount-time, not forever.
    let dir2 = tmp_mount_dir("pinned2");
    let v2 = NaatreVfs::mount(
        HttpMountSource::new(
            Client::new(&server.base, Some(server.token.clone())),
            "pinned",
        ),
        "pinned",
        None,
        dir2.clone(),
        false,
    )
    .expect("mount 2");
    assert_eq!(
        v2.pin().changeset.to_hex(),
        created["id"].as_str().expect("id")
    );
    assert_eq!(v2.read("b.txt").expect("new head's file"), b"two");
    let _ = std::fs::remove_dir_all(&dir);
    let _ = std::fs::remove_dir_all(&dir2);
}

// ---- 5. EROFS default ---------------------------------------------------------

#[test]
fn read_only_mount_asserts_erofs_on_every_write_op() {
    // Over the real server AND over the in-memory double: the gate is the
    // VFS's, not a source property.
    let server = start_server();
    seed_repo(&server, "rofs", &[("a.txt", b"x".as_slice(), false)]);
    let dir = tmp_mount_dir("rofs");
    let v = NaatreVfs::mount(
        HttpMountSource::new(
            Client::new(&server.base, Some(server.token.clone())),
            "rofs",
        ),
        "rofs",
        None,
        dir.clone(),
        false,
    )
    .expect("mount");
    assert!(!v.writable());

    let attempts: Vec<Box<dyn Fn() -> Result<(), VfsError>>> = vec![
        Box::new(|| v.create("", "new.txt").map(|_| ())),
        Box::new(|| v.write("a.txt", b"nope").map(|_| ())),
        Box::new(|| v.flush().map(|_| ())),
    ];
    for attempt in attempts {
        match attempt() {
            Err(e @ VfsError::ReadOnly) => {
                assert_eq!(e.errno(), ERRNO_READ_ONLY, "EROFS is errno 30");
                assert!(e.to_string().contains("--writable"), "{e} names the flag");
            }
            other => panic!("expected ReadOnly, got {other:?}"),
        }
    }
    // Reads still work.
    assert_eq!(v.read("a.txt").expect("read"), b"x");
    let _ = std::fs::remove_dir_all(&dir);

    // The in-memory double agrees (no HTTP involved).
    let (source, _cs) = MemorySource::with_files(&[("a.txt", b"x".as_slice(), false)]);
    let dir2 = tmp_mount_dir("rofs2");
    let v2 = NaatreVfs::mount(source, "r", None, dir2.clone(), false).expect("mount");
    assert!(matches!(v2.write("a.txt", b"y"), Err(VfsError::ReadOnly)));
    assert!(matches!(v2.create("", "n.txt"), Err(VfsError::ReadOnly)));
    assert!(matches!(v2.flush(), Err(VfsError::ReadOnly)));
    let _ = std::fs::remove_dir_all(&dir2);
}

// ---- 6. writable mode: grammar/NFC at create, stage-not-land flush -----------

#[test]
fn writable_mode_enforces_grammar_nfc_and_stages_without_landing() {
    let server = start_server();
    let cs1 = seed_repo(&server, "writable", &[("a.txt", b"one".as_slice(), false)]);

    let dir = tmp_mount_dir("writable");
    let v = NaatreVfs::mount(
        HttpMountSource::new(
            Client::new(&server.base, Some(server.token.clone())),
            "writable",
        ),
        "writable",
        None,
        dir.clone(),
        true,
    )
    .expect("mount");
    assert!(v.writable());

    // Grammar + NFC rejected at create, naming why.
    for (bad_dir, bad_name, why) in [
        ("", "../evil", "component"),
        ("", ".git", "reserved"),
        (".naatre", "x.txt", "reserved"),
        ("", "cafe\u{301}.txt", "NFC"),
        ("", "has\\backslash.txt", "backslash"),
    ] {
        match v.create(bad_dir, bad_name) {
            Err(e @ VfsError::InvalidPath(_)) => {
                let text = e.to_string().to_lowercase();
                assert!(
                    text.contains(why.to_lowercase().as_str()),
                    "create {bad_dir:?}/{bad_name:?} must name {why:?}: {e}"
                );
                assert_eq!(e.errno(), vfs::ERRNO_INVALID_PATH);
            }
            other => panic!("create {bad_dir:?}/{bad_name:?} should be invalid, got {other:?}"),
        }
    }
    // A missing parent directory is NotFound (ENOENT), not a grammar error.
    match v.create("no/such/dir", "x.txt") {
        Err(e @ VfsError::NotFound(_)) => assert!(e.to_string().contains("no such path"), "{e}"),
        other => panic!("missing parent should be NotFound, got {other:?}"),
    }

    // Case-fold twins within one directory are rejected (the grammar's rule).
    v.create("", "README").expect("first name");
    match v.create("", "readme") {
        Err(e @ VfsError::InvalidPath(_)) => {
            assert!(e.to_string().contains("case folding"), "{e}");
        }
        other => panic!("case-fold twin should be rejected, got {other:?}"),
    }

    // A valid create + write, and a write to an existing pinned file.
    v.write("README", b"hello mount\n")
        .expect("write staged file");
    v.create("", "notes.txt").expect("create");
    v.write("notes.txt", b"fresh\n").expect("write new file");
    v.write("a.txt", b"changed\n")
        .expect("overwrite pinned file");

    // The overlay answers stat/read for staged paths.
    assert_eq!(v.read("notes.txt").expect("overlay read"), b"fresh\n");
    assert_eq!(v.stat("notes.txt").expect("stat").size, 6);

    // Flush stages ONE changeset on top of the pin.
    let outcome = v.flush().expect("flush").expect("something staged");
    assert_eq!(outcome.files, 3);
    // The staged changeset exists server-side, parented on the pin, with
    // the daemon author.
    let (status, body) = api(
        &server,
        "GET",
        &format!(
            "/v1/repos/writable/changesets/{}",
            outcome.changeset.to_hex()
        ),
        None,
    );
    assert_eq!(status, 200, "{body}");
    assert_eq!(body["parents"].as_array().expect("parents").len(), 1);
    assert_eq!(
        body["parents"][0].as_str().unwrap(),
        cs1,
        "parent = the pin"
    );
    assert_eq!(body["author"].as_str().unwrap(), "naatre mount");

    // The staged tree carries the new + modified files.
    let (status, tree) = api(
        &server,
        "GET",
        &format!("/v1/repos/writable/trees/{}", outcome.root_tree.to_hex()),
        None,
    );
    assert_eq!(status, 200, "{tree}");
    let names: Vec<&str> = tree["entries"]
        .as_array()
        .expect("entries")
        .iter()
        .filter_map(|e| e["name"].as_str())
        .collect();
    assert!(names.contains(&"notes.txt"), "{names:?}");
    assert!(names.contains(&"README"), "{names:?}");
    assert!(names.contains(&"a.txt"), "{names:?}");

    // NOTHING is landed: main still points at the pin.
    let (status, refv) = api(&server, "GET", "/v1/repos/writable/refs/main", None);
    assert_eq!(status, 200);
    assert_eq!(
        refv["target"].as_str().unwrap(),
        cs1,
        "flush never moves the ref"
    );

    // An empty-shadow... no: the shadow stays after flush (documented); a
    // re-flush stages a fresh changeset.
    let again = v.flush().expect("flush again").expect("re-staged");
    assert_eq!(again.files, 3);
    assert_ne!(again.changeset, outcome.changeset);
    let _ = std::fs::remove_dir_all(&dir);
}

// ---- 7. the real daemon binary: banner, loopback, export, SIGKILL cleanup ----

#[test]
fn mount_binary_loopback_export_pin_and_sigkill_cleanup() {
    let server = start_server();
    let cs1 = seed_repo(&server, "daemon", &[("a.txt", b"one".as_slice(), false)]);

    let cache = tempfile::tempdir().expect("cache tempdir");
    let daemon = || {
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_naatre"));
        cmd.args([
            "mount",
            "daemon",
            "--server",
            &server.base,
            "--token",
            &server.token,
        ])
        .env("XDG_CACHE_HOME", cache.path())
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
        cmd
    };

    // First daemon: banner pins the loopback + unguessable-export contract.
    let mut child = daemon().spawn().expect("spawn naatre mount");
    let stdout = child.stdout.take().expect("stdout");
    // ONE reader for the whole banner: a per-line BufReader would swallow
    // the buffered remainder on drop and the next read_line would block.
    let mut reader = std::io::BufReader::new(stdout);
    let mut lines = Vec::new();
    for _ in 0..7 {
        let mut line = String::new();
        reader.read_line(&mut line).expect("banner line");
        lines.push(line.trim_end().to_string());
    }
    let line_pinned = &lines[0];
    let line_serving = &lines[1];
    let line_auth = &lines[2];
    let line_macos = &lines[3];
    let line_linux = &lines[4];
    let line_cache = &lines[5];
    assert!(
        line_pinned.contains(&format!("pinned at {cs1}")) && line_pinned.contains("read-only"),
        "{line_pinned}"
    );
    let export_id: String = line_serving
        .split_once("/naatre/")
        .expect("export path in banner")
        .1
        .split(' ')
        .next()
        .expect("export id")
        .to_string();
    assert_eq!(
        export_id.len(),
        32,
        "unguessable 32-hex export: {line_serving}"
    );
    assert!(
        line_serving.starts_with("serving /naatre/") && line_serving.contains("127.0.0.1:"),
        "{line_serving}"
    );
    assert!(
        line_auth.contains("AUTH_SYS authenticates nothing"),
        "{line_auth}"
    );
    assert!(
        line_macos.contains("mount_nfs") && line_macos.contains("nfsvers=3"),
        "{line_macos}"
    );
    assert!(
        line_linux.contains("mount -t nfs") && line_linux.contains("vers=3"),
        "{line_linux}"
    );
    assert!(
        line_cache.contains("0700") && line_cache.contains("1024"),
        "{line_cache}"
    );
    let port: u16 = line_serving
        .rsplit(':')
        .next()
        .unwrap()
        .parse()
        .expect("ephemeral port");
    assert!(port > 0, "ephemeral port printed");

    let mounts_root = cache.path().join("naatre").join("mounts");
    let dirs = |root: &Path| -> Vec<String> {
        std::fs::read_dir(root)
            .expect("mounts root")
            .flatten()
            .filter(|e| e.path().is_dir())
            .filter_map(|e| e.file_name().into_string().ok())
            .collect()
    };
    assert_eq!(
        dirs(&mounts_root),
        vec![export_id.clone()],
        "exactly our dir"
    );

    // SIGKILL: no cleanup runs — the stale dir stays behind…
    let stale_dir = mounts_root.join(&export_id);
    child.kill().expect("SIGKILL");
    child.wait().expect("wait");
    assert!(
        stale_dir.exists(),
        "SIGKILL leaves the dir (nothing to run)"
    );

    // …and the NEXT mount evicts it before making its own.
    let mut child2 = daemon().spawn().expect("spawn second naatre mount");
    let stdout2 = child2.stdout.take().expect("stdout");
    let mut reader2 = std::io::BufReader::new(stdout2);
    for _ in 0..7 {
        let mut line = String::new();
        reader2.read_line(&mut line).expect("banner line 2");
    }
    let dirs2 = dirs(&mounts_root);
    assert_eq!(dirs2.len(), 1, "the stale dir was evicted: {dirs2:?}");
    assert_ne!(dirs2[0], export_id, "a fresh unguessable export id");
    // Graceful SIGTERM: exit 0.
    unsafe {
        libc::kill(child2.id() as libc::pid_t, libc::SIGTERM);
    }
    let status = child2.wait().expect("wait for graceful exit");
    assert!(status.success(), "graceful SIGTERM exits 0: {status}");
}

// ---- 8. cargo build against a cold mount (one platform; skips w/o cargo) ----

#[test]
fn cargo_build_against_cold_mount() {
    // The toolchain gate: skip gracefully when no cargo exists (CI).
    if Command::new("cargo").arg("--version").output().is_err() {
        println!("skipping cargo-build-against-cold-mount: no cargo toolchain");
        return;
    }

    let server = start_server();
    // Seed a repo with THIS workspace's sources (the VFS view of naatre's
    // own code): root manifests + every crate's manifest and sources.
    let ws = Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(Path::parent)
        .expect("workspace root")
        .to_path_buf();
    let mut files: Vec<(String, Vec<u8>, bool)> = Vec::new();
    let skip_dirs = [".git", "target", ".research", "node_modules"];
    fn collect(root: &Path, dir: &Path, files: &mut Vec<(String, Vec<u8>, bool)>, skip: &[&str]) {
        for entry in std::fs::read_dir(dir).expect("walk").flatten() {
            let path = entry.path();
            let name = entry.file_name().to_string_lossy().into_owned();
            if path.is_dir() {
                if !skip.contains(&name.as_str()) {
                    collect(root, &path, files, skip);
                }
            } else if path.extension().is_some_and(|e| e == "rs")
                || path.file_name().is_some_and(|n| n == "Cargo.toml")
                || path.file_name().is_some_and(|n| n == "Cargo.lock")
            {
                let bytes = std::fs::read(&path).expect("read source");
                if bytes.len() > 1024 * 1024 {
                    continue;
                }
                let rel = path
                    .strip_prefix(root)
                    .expect("relative")
                    .to_string_lossy()
                    .replace('\\', "/");
                let exec = path
                    .metadata()
                    .map(|m| {
                        use std::os::unix::fs::PermissionsExt as _;
                        m.permissions().mode() & 0o111 != 0
                    })
                    .unwrap_or(false);
                files.push((rel, bytes, exec));
            }
        }
    }
    collect(&ws, &ws, &mut files, &skip_dirs);
    assert!(
        files.iter().any(|(p, _, _)| p == "Cargo.lock")
            && files
                .iter()
                .any(|(p, _, _)| p == "crates/naatre-core/src/lib.rs"),
        "the walk must carry the manifests and sources: {} files",
        files.len()
    );

    let refs: Vec<(&str, &[u8], bool)> = files
        .iter()
        .map(|(p, d, e)| (p.as_str(), d.as_slice(), *e))
        .collect();
    seed_repo(&server, "selfhost", &refs);

    // Hydrate EVERY file through a COLD mount (fresh dir = empty cache:
    // each byte arrives via a hash-verified first read) into a fresh tree.
    let dir = tmp_mount_dir("cold");
    let v = NaatreVfs::mount(
        HttpMountSource::new(
            Client::new(&server.base, Some(server.token.clone())),
            "selfhost",
        ),
        "selfhost",
        None,
        dir.clone(),
        false,
    )
    .expect("mount");
    let copy = tempfile::tempdir().expect("copy tempdir");
    for (path, meta) in walk_vfs_files(&v) {
        let dest = copy.path().join(&path);
        std::fs::create_dir_all(dest.parent().expect("parent")).expect("mkdirs");
        std::fs::write(&dest, v.read(&path).expect("hydrated bytes")).expect("write");
        if meta.exec {
            use std::os::unix::fs::PermissionsExt as _;
            std::fs::set_permissions(&dest, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        }
    }

    // Build naatre-core against the cold-hydrated sources.
    let status = Command::new("cargo")
        .args(["build", "--locked", "-p", "naatre-core"])
        .current_dir(copy.path())
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .status()
        .expect("run cargo");
    assert!(
        status.success(),
        "cargo build -p naatre-core failed against the cold mount"
    );
    let _ = std::fs::remove_dir_all(&dir);
}
