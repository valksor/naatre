//! M8a in-process VFS suite: drives [`NaatreVfs`] directly against the REAL
//! server (no kernel mount — the CI-feasible harness the plan pins). The
//! named M8 checks live here: byte-exact mount-vs-materialized parity (full
//! walk + hash + exec bits), stat-never-fetches (recording source),
//! corrupt-cache recovery, daemon SIGKILL cleanup (through the real
//! binary), and ref pinning. The writable mode's EROFS default, grammar/NFC
//! create enforcement, and stage-not-land flush run against the real
//! server; hydration-error typing is pinned by the in-memory double in
//! `src/vfs.rs` (the server itself refuses to create the broken states).
//!
//! M8b adds the REAL-kernel-mount leg (where the platform allows): the NFS
//! e2e actually mounts the daemon's export with `mount_nfs` — byte parity
//! vs the materialized clone, a cold cargo build against the mount, and the
//! writable `--flush-land` flow — plus the Linux FUSE runtime mount behind
//! `#[cfg(target_os = "linux")]` (gated on /dev/fuse + fusermount3, skip
//! notice otherwise; macOS never compiles it). A mount attempt that fails
//! in a non-root/sandboxed environment SKIPS with a printed notice, never
//! silently; under root a mount failure is a real failure.

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

// ---- M8b: the REAL kernel-mount leg (where the platform allows) -------------

/// The exact flags the M8b research pinned for a non-root macOS mount:
/// `resvport` (mount_nfs's default source port) needs root, so the e2e
/// carries `noresvport`; `nfc` aligns the client's name normalization with
/// the grammar; `soft` keeps a dead daemon from wedging the test.
#[cfg(target_os = "macos")]
fn nfs_mount_flags(port: u16) -> String {
    format!("port={port},mountport={port},tcp,nfc,noresvport,soft,nfsvers=3")
}

/// Mounts the daemon's export with the REAL kernel client. Returns true on
/// success. A failure under root is a REAL failure (panic); a failure as
/// non-root reads as "this environment cannot mount" (sandboxed CI): the
/// SKIP NOTICE is printed here and false returned — never silent.
#[cfg(target_os = "macos")]
fn nfs_mount_or_skip_notice(port: u16, export_path: &str, mnt: &Path) -> bool {
    let flags = nfs_mount_flags(port);
    let out = Command::new("mount_nfs")
        .args(["-o", &flags, &format!("localhost:{export_path}")])
        .arg(mnt)
        .output()
        .expect("run mount_nfs");
    if out.status.success() {
        return true;
    }
    let stderr = String::from_utf8_lossy(&out.stderr).trim().to_string();
    if unsafe { libc::geteuid() } == 0 {
        panic!(
            "kernel mount failed under root (a real defect, not an environment gate): \
             mount_nfs -o {flags} localhost:{export_path} {}: {stderr}",
            mnt.display()
        );
    }
    println!(
        "SKIP NOTICE (nfs-e2e): this environment cannot mount (non-root/sandboxed); \
         the kernel-mount leg is skipped.\n  command: mount_nfs -o {flags} \
         localhost:{export_path} {}\n  stderr: {stderr}",
        mnt.display()
    );
    false
}

/// Stage-2 capability gate: the mount succeeding is not enough. This macOS
/// build (Darwin 25) ACCEPTS an unprivileged NFS mount but then suppresses
/// every data op client-side — readdir/read/write fail EPERM and the
/// kernel's RPC counters show zero data ops (verified with a bare
/// upstream-shape nfsserve adapter, so it is not the naatre adapter). A
/// probe readdir as non-root SKIPS with a printed notice; under root it is
/// a real failure.
#[cfg(target_os = "macos")]
fn data_ops_probe_or_skip_notice(mnt: &Path) -> bool {
    match std::fs::read_dir(mnt) {
        Ok(_) => true,
        Err(e) => {
            if unsafe { libc::geteuid() } == 0 {
                panic!(
                    "readdir on the mount failed under root (a real defect, not an \
                        environment gate): {e}"
                );
            }
            println!(
                "SKIP NOTICE (nfs-e2e): this macOS build accepts the unprivileged mount but \
                 suppresses data ops (probe readdir: {e}); the kernel-mount parity leg \
                 needs a root runner — skipping."
            );
            false
        }
    }
}

/// Unmounts. macOS `umount(8)` refuses even the mounting user (EPERM); the
/// DiskArbitration path (`diskutil unmount`) is the documented way.
#[cfg(target_os = "macos")]
fn unmount_nfs(mnt: &Path) {
    let out = Command::new("umount")
        .arg(mnt)
        .output()
        .expect("run umount");
    if !out.status.success() {
        let out = Command::new("diskutil")
            .args(["unmount", &mnt.to_string_lossy()])
            .output()
            .expect("run diskutil unmount");
        assert!(
            out.status.success(),
            "unmount {} failed: {}",
            mnt.display(),
            String::from_utf8_lossy(&out.stderr)
        );
    }
    // Sanity: really gone (mount table walk, /tmp→/private/tmp aware).
    let real = std::fs::canonicalize(mnt).expect("canonicalize");
    let table = Command::new("mount").output().expect("run mount").stdout;
    let still = String::from_utf8_lossy(&table).contains(&format!(" on {} (", real.display()));
    assert!(
        !still,
        "the mount is still alive after unmount: {}",
        real.display()
    );
}

/// Spawns the real daemon binary against `server` for `repo` with
/// `extra_args`, reads the 7-line banner, and returns (child, keepalive
/// reader, port, export path). The reader MUST stay bound (never dropped)
/// until the daemon exits: dropping it closes the stdout pipe and the
/// daemon's next println dies on EPIPE/SIGPIPE. The cache goes to an
/// isolated XDG_CACHE_HOME.
#[cfg(any(target_os = "macos", target_os = "linux"))]
fn spawn_mount_daemon(
    server: &TestServer,
    repo: &str,
    cache: &Path,
    extra_args: &[&str],
) -> (
    std::process::Child,
    std::io::BufReader<std::process::ChildStdout>,
    u16,
    String,
) {
    let mut cmd = Command::new(env!("CARGO_BIN_EXE_naatre"));
    cmd.args([
        "mount",
        repo,
        "--server",
        &server.base,
        "--token",
        &server.token,
    ])
    .args(extra_args)
    .env("XDG_CACHE_HOME", cache)
    .env_remove("NAATRE_URL")
    .env_remove("NAATRE_TOKEN")
    .stdout(Stdio::piped())
    .stderr(Stdio::piped());
    let mut child = cmd.spawn().expect("spawn naatre mount");
    let stdout = child.stdout.take().expect("stdout");
    let mut reader = std::io::BufReader::new(stdout);
    let mut lines = Vec::new();
    for _ in 0..7 {
        let mut line = String::new();
        reader.read_line(&mut line).expect("banner line");
        lines.push(line.trim_end().to_string());
    }
    let serving = &lines[1];
    assert!(serving.starts_with("serving /naatre/"), "{serving}");
    let port: u16 = serving
        .rsplit(':')
        .next()
        .unwrap()
        .parse()
        .expect("ephemeral port");
    let export_path: String = serving
        .split_once("serving ")
        .expect("export prefix")
        .1
        .split(' ')
        .next()
        .expect("export path")
        .to_string();
    (child, reader, port, export_path)
}

/// True when the kernel mount table carries `path` (Linux /proc/mounts —
/// no dependence on an external `mountpoint` binary).
#[cfg(target_os = "linux")]
fn linux_mount_table_has(path: &str) -> bool {
    std::fs::read_to_string("/proc/mounts")
        .map(|table| table.contains(path))
        .unwrap_or(false)
}

/// Full recursive walk of a mounted tree: (path, abs path, is-exec) rows,
/// name-byte sorted.
#[cfg(any(target_os = "macos", target_os = "linux"))]
fn walk_mounted_files(root: &Path) -> Vec<(String, PathBuf, bool)> {
    fn rec(dir: &Path, rel: &str, out: &mut Vec<(String, PathBuf, bool)>) {
        for entry in std::fs::read_dir(dir).expect("readdir on mount") {
            let entry = entry.expect("dir entry");
            let name = entry.file_name().into_string().expect("utf8 name");
            let full = if rel.is_empty() {
                name.clone()
            } else {
                format!("{rel}/{name}")
            };
            let path = dir.join(&name);
            let meta = entry.metadata().expect("stat on mount");
            if meta.is_dir() {
                rec(&path, &full, out);
            } else {
                use std::os::unix::fs::PermissionsExt as _;
                out.push((full, path, meta.permissions().mode() & 0o111 != 0));
            }
        }
    }
    let mut out = Vec::new();
    rec(root, "", &mut out);
    out.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    out
}

// ---- M8b item 1+4: real-mount byte parity + EROFS through the kernel -------

#[cfg(target_os = "macos")]
#[test]
fn nfs_kernel_mount_walk_parity_bytes_sizes_exec_and_erofs() {
    let server = start_server();
    let files = demo_files();
    let refs: Vec<(&str, &[u8], bool)> = files
        .iter()
        .map(|(p, d, e)| (*p, d.as_slice(), *e))
        .collect();
    seed_repo(&server, "nfsparity", &refs);

    // The materialized side: a real `naatre clone`.
    let out = tempfile::tempdir().expect("tempdir");
    let dest = out.path().join("wc");
    verbs::clone::value(
        &format!("{}/nfsparity", server.base),
        &[],
        Some(dest.to_str().expect("utf8")),
        &net(&server),
    )
    .expect("clone");

    let cache = tempfile::tempdir().expect("cache tempdir");
    let (mut child, _reader, port, export_path) =
        spawn_mount_daemon(&server, "nfsparity", cache.path(), &[]);

    let mnt_parent = tempfile::tempdir().expect("mnt tempdir");
    let mnt = mnt_parent.path().join("m");
    std::fs::create_dir_all(&mnt).expect("mkdir mnt");
    let mnt = std::fs::canonicalize(&mnt).expect("canonicalize mnt");

    if !nfs_mount_or_skip_notice(port, &export_path, &mnt) {
        // Environment cannot mount (the SKIP NOTICE is printed above);
        // stop the daemon and skip.
        unsafe {
            libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
        }
        let _ = child.wait();
        return;
    }
    if !data_ops_probe_or_skip_notice(&mnt) {
        // The mount works but data ops are OS-suppressed (notice printed).
        unmount_nfs(&mnt);
        unsafe {
            libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
        }
        let _ = child.wait();
        return;
    }

    // Same path sets as the materialized clone (full walk through the kernel).
    let index = Wc::open(&dest).expect("wc").load_index().expect("index");
    let walked = walk_mounted_files(&mnt);
    let mount_paths: Vec<&str> = walked.iter().map(|(p, _, _)| p.as_str()).collect();
    let clone_paths: Vec<&str> = index.rows.keys().map(String::as_str).collect();
    assert_eq!(
        clone_paths, mount_paths,
        "identical full walk through the kernel"
    );

    // Byte-exact per file; stat sizes and exec bits match the clone.
    for (path, abs, exec) in &walked {
        let row = &index.rows[path];
        let bytes = std::fs::read(abs).expect("read on mount");
        assert_eq!(
            bytes,
            std::fs::read(dest.join(path)).expect("disk bytes"),
            "{path}"
        );
        assert_eq!(object_id(Kind::Blob, &bytes).0, row.blob, "{path} hash");
        let size = std::fs::metadata(abs).expect("stat").len();
        assert_eq!(size, row.size, "{path} stat size");
        assert_eq!(*exec, row.exec, "{path} exec bit");
    }
    assert!(
        index.rows["scripts/run.sh"].exec,
        "fixture has an exec file"
    );

    // The read-only default holds THROUGH the kernel: a write attempt on
    // the mounted tree fails (EROFS surfaces as a failed create).
    assert!(
        std::fs::write(mnt.join("ro.txt"), b"nope").is_err(),
        "writing to a read-only mount must fail"
    );

    unmount_nfs(&mnt);
    unsafe {
        libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
    }
    let status = child.wait().expect("wait daemon");
    assert!(status.success(), "graceful SIGTERM exits 0: {status}");
}

// ---- raw-RPC LOOKUP/READDIR/READ parity walk --------------------------------

const NFS3_LOOKUP: u32 = 3;
const NFS3_READ: u32 = 6;
const NFS3_READDIR: u32 = 16;
/// fattr3 offsets in the reply body (after the 28-byte RPC head): type
/// u32, mode u32, nlink u32, uid u32, gid u32, size u64.
const ATTR_MODE_AT: usize = 28 + 4;
const ATTR_SIZE_AT: usize = 28 + 20;

fn xdr_u64(v: u64) -> Vec<u8> {
    v.to_be_bytes().to_vec()
}

/// NFSv3 LOOKUP → the child's file handle.
fn nfs_lookup(port: u16, dir: &[u8], name: &str) -> Vec<u8> {
    let mut payload = xdr_opaque(dir);
    payload.extend(xdr_opaque(name.as_bytes()));
    let reply = raw_rpc(port, NFS_PROGRAM, 3, NFS3_LOOKUP, &payload);
    assert_eq!(nfs_status(&reply), NFS3_OK, "LOOKUP {name}");
    let fh_len = u32::from_be_bytes(reply[28..32].try_into().expect("fh len")) as usize;
    reply[32..32 + fh_len].to_vec()
}

/// NFSv3 GETATTR → (is_dir, size, exec-bits).
fn nfs_getattr(port: u16, fh: &[u8]) -> (bool, u64, bool) {
    let reply = raw_rpc(port, NFS_PROGRAM, 3, 1, &xdr_opaque(fh));
    assert_eq!(nfs_status(&reply), NFS3_OK, "GETATTR");
    let ftype = u32::from_be_bytes(reply[28..32].try_into().expect("type"));
    let mode = u32::from_be_bytes(
        reply[ATTR_MODE_AT..ATTR_MODE_AT + 4]
            .try_into()
            .expect("mode"),
    );
    let size = u64::from_be_bytes(
        reply[ATTR_SIZE_AT..ATTR_SIZE_AT + 8]
            .try_into()
            .expect("size"),
    );
    (ftype == 2, size, mode & 0o111 != 0)
}

/// NFSv3 READDIR → the directory's entries. One call with a generous
/// count; the eof flag is asserted so pagination cannot hide entries.
fn nfs_readdir(port: u16, dir: &[u8]) -> Vec<String> {
    let mut payload = xdr_opaque(dir);
    payload.extend(xdr_u64(0)); // cookie
    payload.extend_from_slice(&[0u8; 8]); // cookieverf (first call)
    payload.extend((8192u32).to_be_bytes()); // count
    let reply = raw_rpc(port, NFS_PROGRAM, 3, NFS3_READDIR, &payload);
    assert_eq!(nfs_status(&reply), NFS3_OK, "READDIR");
    let mut at = 28 + 88; // dir attrs
    at += 8; // cookieverf
    let mut names = Vec::new();
    loop {
        let follows = u32::from_be_bytes(reply[at..at + 4].try_into().expect("follows"));
        at += 4;
        if follows == 0 {
            break;
        }
        let _fileid = u64::from_be_bytes(reply[at..at + 8].try_into().expect("fileid"));
        at += 8;
        let name_len = u32::from_be_bytes(reply[at..at + 4].try_into().expect("name len")) as usize;
        at += 4;
        names.push(String::from_utf8(reply[at..at + name_len].to_vec()).expect("utf8 name"));
        at += name_len.div_ceil(4) * 4;
        let _cookie = u64::from_be_bytes(reply[at..at + 8].try_into().expect("cookie"));
        at += 8;
    }
    let eof = u32::from_be_bytes(reply[at..at + 4].try_into().expect("eof"));
    assert_eq!(eof, 1, "READDIR single call returned the whole directory");
    names
}

/// NFSv3 READ (offset loop until eof) → the file bytes.
fn nfs_read_all(port: u16, fh: &[u8], size: u64) -> Vec<u8> {
    let mut out = Vec::new();
    let mut offset = 0u64;
    loop {
        let mut payload = xdr_opaque(fh);
        payload.extend(xdr_u64(offset));
        payload.extend((65536u32).to_be_bytes());
        let reply = raw_rpc(port, NFS_PROGRAM, 3, NFS3_READ, &payload);
        assert_eq!(nfs_status(&reply), NFS3_OK, "READ at {offset}");
        let at = 28 + 88; // attrs
        let count = u32::from_be_bytes(reply[at..at + 4].try_into().expect("count")) as usize;
        let eof = u32::from_be_bytes(reply[at + 4..at + 8].try_into().expect("eof"));
        let data_len =
            u32::from_be_bytes(reply[at + 8..at + 12].try_into().expect("data len")) as usize;
        assert_eq!(data_len, count, "READ count matches the opaque length");
        out.extend_from_slice(&reply[at + 12..at + 12 + data_len]);
        offset += count as u64;
        if eof == 1 || offset >= size {
            break;
        }
    }
    out
}

/// The raw-RPC parity walk the M8b commit message claims: LOOKUP down the
/// export from the mountd root handle, READDIR every directory, READ every
/// file byte-for-byte through the wire protocol, and compare paths, bytes,
/// and exec bits against a real `naatre clone`. GETATTR supplies sizes and
/// exec bits; no kernel mount needed (runs on every platform).
#[test]
fn raw_rpc_readdir_read_parity_with_materialized_clone() {
    let server = start_server();
    let files = demo_files();
    let refs: Vec<(&str, &[u8], bool)> = files
        .iter()
        .map(|(p, d, e)| (*p, d.as_slice(), *e))
        .collect();
    seed_repo(&server, "rawparity", &refs);

    // The materialized side: a real `naatre clone`.
    let out = tempfile::tempdir().expect("tempdir");
    let dest = out.path().join("wc");
    verbs::clone::value(
        &format!("{}/rawparity", server.base),
        &[],
        Some(dest.to_str().expect("utf8")),
        &net(&server),
    )
    .expect("clone");

    let cache = tempfile::tempdir().expect("cache tempdir");
    let (mut child, _reader, port, export_path) =
        spawn_mount_daemon(&server, "rawparity", cache.path(), &[]);

    let root = mnt_real_handle(port, &export_path);

    // The clone side: (relative path → bytes, exec) + dir paths, walked on
    // disk.
    use std::os::unix::fs::PermissionsExt as _;
    fn collect_clone(
        dir: &Path,
        prefix: &str,
        files: &mut Vec<(String, Vec<u8>, bool)>,
        dirs: &mut Vec<String>,
    ) {
        for e in std::fs::read_dir(dir).expect("read_dir") {
            let e = e.expect("dir entry");
            let name = e.file_name().to_string_lossy().to_string();
            // The working copy's own metadata namespace is client-local —
            // never part of the ref tree the export serves.
            if prefix.is_empty() && name == ".naatre" {
                continue;
            }
            let path = e.path();
            let rel = if prefix.is_empty() {
                name.clone()
            } else {
                format!("{prefix}/{name}")
            };
            if path.is_dir() {
                dirs.push(rel.clone());
                collect_clone(&path, &rel, files, dirs);
            } else {
                let bytes = std::fs::read(&path).expect("read file");
                let exec =
                    std::fs::metadata(&path).expect("stat").permissions().mode() & 0o111 != 0;
                files.push((rel, bytes, exec));
            }
        }
    }
    let mut clone_files = Vec::new();
    let mut clone_dirs = Vec::new();
    collect_clone(&dest, "", &mut clone_files, &mut clone_dirs);
    clone_files.sort_by(|a, b| a.0.cmp(&b.0));
    clone_dirs.sort();

    // The wire side: LOOKUP/READDIR walk from the root handle.
    fn walk(
        port: u16,
        dir_fh: &[u8],
        prefix: &str,
        files: &mut Vec<(String, Vec<u8>, bool)>,
        dirs: &mut Vec<String>,
    ) {
        for name in nfs_readdir(port, dir_fh) {
            if name == "." || name == ".." {
                continue;
            }
            let rel = if prefix.is_empty() {
                name.clone()
            } else {
                format!("{prefix}/{name}")
            };
            let fh = nfs_lookup(port, dir_fh, &name);
            let (is_dir, size, exec) = nfs_getattr(port, &fh);
            if is_dir {
                dirs.push(rel.clone());
                walk(port, &fh, &rel, files, dirs);
            } else {
                let bytes = nfs_read_all(port, &fh, size);
                assert_eq!(
                    bytes.len() as u64,
                    size,
                    "{rel}: READ assembled the advertised size"
                );
                files.push((rel, bytes, exec));
            }
        }
    }
    let mut wire_files = Vec::new();
    let mut wire_dirs = Vec::new();
    walk(port, &root, "", &mut wire_files, &mut wire_dirs);
    wire_files.sort_by(|a, b| a.0.cmp(&b.0));
    wire_dirs.sort();

    // Parity: identical path sets, identical bytes, identical exec bits.
    assert_eq!(
        clone_files.iter().map(|f| &f.0).collect::<Vec<_>>(),
        wire_files.iter().map(|f| &f.0).collect::<Vec<_>>(),
        "identical file path sets"
    );
    assert_eq!(clone_dirs, wire_dirs, "identical directory path sets");
    for ((cp, cbytes, cexec), (wp, wbytes, wexec)) in clone_files.iter().zip(wire_files.iter()) {
        assert_eq!(cp, wp);
        assert_eq!(cbytes, wbytes, "{cp}: byte-exact over raw READ");
        assert_eq!(cexec, wexec, "{cp}: exec bits match the clone's mode");
    }
    // The fixture actually exercised nesting and the exec bit.
    assert!(wire_files.iter().any(|(p, _, _)| p == "a/b/c/deep.txt"));
    assert!(
        wire_files
            .iter()
            .any(|(p, _, e)| p == "scripts/run.sh" && *e),
        "the exec file's exec bit came through the wire"
    );

    unsafe {
        libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
    }
    let status = child.wait().expect("wait daemon");
    assert!(status.success(), "graceful SIGTERM exits 0: {status}");
}

// ---- M8b item 4: cold cargo build against the real mount -------------------

#[cfg(target_os = "macos")]
#[test]
fn cargo_build_against_nfs_kernel_mount() {
    // The toolchain gate (the in-process test's shape): skip when no cargo.
    if Command::new("cargo").arg("--version").output().is_err() {
        println!("skipping cargo-build-against-nfs-mount: no cargo toolchain");
        return;
    }

    let server = start_server();
    // Seed a repo with THIS workspace's sources (the cold-build fixture's
    // shape): manifests + sources, ≤1 MiB each.
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
        files
            .iter()
            .any(|(p, _, _)| p == "crates/naatre-core/src/lib.rs"),
        "the walk must carry the crate sources: {} files",
        files.len()
    );
    let refs: Vec<(&str, &[u8], bool)> = files
        .iter()
        .map(|(p, d, e)| (p.as_str(), d.as_slice(), *e))
        .collect();
    seed_repo(&server, "selfhost", &refs);

    let cache = tempfile::tempdir().expect("cache tempdir");
    let (mut child, _reader, port, export_path) =
        spawn_mount_daemon(&server, "selfhost", cache.path(), &[]);

    let mnt_parent = tempfile::tempdir().expect("mnt tempdir");
    let mnt = mnt_parent.path().join("m");
    std::fs::create_dir_all(&mnt).expect("mkdir mnt");
    let mnt = std::fs::canonicalize(&mnt).expect("canonicalize mnt");
    if !nfs_mount_or_skip_notice(port, &export_path, &mnt) {
        // Environment cannot mount (the SKIP NOTICE is printed above);
        // stop the daemon and skip.
        unsafe {
            libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
        }
        let _ = child.wait();
        return;
    }
    if !data_ops_probe_or_skip_notice(&mnt) {
        // The mount works but data ops are OS-suppressed (notice printed).
        unmount_nfs(&mnt);
        unsafe {
            libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
        }
        let _ = child.wait();
        return;
    }

    // Build AGAINST the mount: cwd on the mount reads every source through
    // the kernel NFS client; artifacts go to a LOCAL target dir (the mount
    // is read-only). --offline pins the warm-registry contract.
    let target = tempfile::tempdir().expect("target tempdir");
    let status = Command::new("cargo")
        .args(["build", "--locked", "--offline", "-p", "naatre-core"])
        .current_dir(&mnt)
        .env("CARGO_TARGET_DIR", target.path())
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .status()
        .expect("run cargo");
    assert!(
        status.success(),
        "cargo build -p naatre-core failed against the real NFS mount"
    );
    assert!(
        target.path().join("debug").exists(),
        "artifacts landed in the local target dir"
    );

    unmount_nfs(&mnt);
    unsafe {
        libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
    }
    let status = child.wait().expect("wait daemon");
    assert!(status.success(), "graceful SIGTERM exits 0: {status}");
}

// ---- M8b item 3: writable mount, create+write, flush-land advances main ----

#[cfg(target_os = "macos")]
#[test]
fn writable_kernel_mount_create_write_flush_land_advances_main() {
    let server = start_server();
    seed_repo(&server, "wland", &[("a.txt", b"one".as_slice(), false)]);

    let cache = tempfile::tempdir().expect("cache tempdir");
    let (mut child, _reader, port, export_path) = spawn_mount_daemon(
        &server,
        "wland",
        cache.path(),
        &["--writable", "--flush-land"],
    );

    let mnt_parent = tempfile::tempdir().expect("mnt tempdir");
    let mnt = mnt_parent.path().join("m");
    std::fs::create_dir_all(&mnt).expect("mkdir mnt");
    let mnt = std::fs::canonicalize(&mnt).expect("canonicalize mnt");
    if !nfs_mount_or_skip_notice(port, &export_path, &mnt) {
        // Environment cannot mount (the SKIP NOTICE is printed above);
        // stop the daemon and skip.
        unsafe {
            libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
        }
        let _ = child.wait();
        return;
    }
    if !data_ops_probe_or_skip_notice(&mnt) {
        // The mount works but data ops are OS-suppressed (notice printed).
        unmount_nfs(&mnt);
        unsafe {
            libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
        }
        let _ = child.wait();
        return;
    }

    // Grammar enforced at create THROUGH the kernel mount: the reserved
    // name is refused (the create answer maps to a failed create).
    assert!(
        std::fs::write(mnt.join(".git"), b"x").is_err(),
        "creating .git through the mount must fail (grammar)"
    );

    // Create + write THROUGH the mount, then unmount, then the daemon's
    // SIGTERM flush-lands the staged changeset onto main.
    let payload = b"landed through the kernel mount\n".to_vec();
    std::fs::write(mnt.join("notes.txt"), &payload).expect("create+write through the mount");
    // Read-back through the mount shows the overlay.
    assert_eq!(
        std::fs::read(mnt.join("notes.txt")).expect("overlay read"),
        payload,
        "the overlay answers reads through the mount"
    );

    unmount_nfs(&mnt);
    unsafe {
        libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
    }
    let out = child.wait_with_output().expect("wait daemon");
    assert!(
        out.status.success(),
        "graceful SIGTERM exits 0: {:?}",
        out.status
    );
    let stdout = String::from_utf8_lossy(&out.stdout).to_string();
    assert!(
        stdout.contains("land key: "),
        "the flush-land key is printed: {stdout}"
    );
    assert!(
        stdout.contains("\"landed\""),
        "the landed/main/rebased summary is printed: {stdout}"
    );

    // The staged changeset id (from the stdout json) IS the new main.
    let landed = stdout
        .lines()
        .find_map(|l| {
            serde_json::from_str::<Value>(l)
                .ok()
                .filter(|v| v["landed"].is_string())
        })
        .expect("landed json line");
    let landed_id = landed["landed"].as_str().expect("landed id").to_string();
    assert_eq!(
        landed["main"].as_str().expect("main"),
        landed_id,
        "main advanced onto the landed changeset"
    );
    let (status, refv) = api(&server, "GET", "/v1/repos/wland/refs/main", None);
    assert_eq!(status, 200, "{refv}");
    assert_eq!(
        refv["target"].as_str().unwrap(),
        landed_id,
        "refs/main is the flush-landed changeset"
    );

    // A fresh clone sees the file, byte-exact.
    let out2 = tempfile::tempdir().expect("tempdir");
    let dest2 = out2.path().join("wc2");
    verbs::clone::value(
        &format!("{}/wland", server.base),
        &[],
        Some(dest2.to_str().expect("utf8")),
        &net(&server),
    )
    .expect("fresh clone");
    assert_eq!(
        std::fs::read(dest2.join("notes.txt")).expect("landed bytes"),
        payload,
        "the fresh clone sees the mounted write"
    );
    assert_eq!(
        std::fs::read(dest2.join("a.txt")).expect("pinned file"),
        b"one",
        "the pinned file survives the land"
    );
}

// ---- M8b item 3: flush-land lands via the SHARED land path -----------------

/// The flush-land pin, driven at the VFS level (the transport-independent
/// chain the NFS/FUSE front-ends call into): create (grammar+NFC enforced),
/// write, flush, LAND through `verbs::land::land_changeset` — the same
/// function `naatre land` and the daemon's `--flush-land` exit share — then
/// main has advanced and a fresh clone sees the file. The kernel-mount
/// leg of this pin lives in `writable_kernel_mount_create_write_flush_land_
/// advances_main` (runs where the OS allows data ops).
#[test]
fn flush_land_lands_the_staged_writes_via_the_shared_land_path() {
    let server = start_server();
    let cs1 = seed_repo(&server, "wland2", &[("a.txt", b"one".as_slice(), false)]);

    let dir = tmp_mount_dir("wland2");
    let v = NaatreVfs::mount(
        HttpMountSource::new(
            Client::new(&server.base, Some(server.token.clone())),
            "wland2",
        ),
        "wland2",
        None,
        dir.clone(),
        true,
    )
    .expect("mount");
    assert!(v.writable());

    // Grammar + NFC enforced at create (the pin's gate).
    match v.create("", ".git") {
        Err(e @ VfsError::InvalidPath(_)) => {
            assert_eq!(e.errno(), vfs::ERRNO_INVALID_PATH);
        }
        other => panic!("creating .git must be rejected, got {other:?}"),
    }

    // Create + write (the ops the front-ends forward).
    let payload = b"landed through the flush-land path\n".to_vec();
    v.create("", "notes.txt").expect("create");
    v.write("notes.txt", &payload).expect("write");
    v.write("a.txt", b"changed\n")
        .expect("overwrite pinned file");

    // Flush stages; LAND via the shared path with a printed-then-used key.
    let outcome = v.flush().expect("flush").expect("something staged");
    let key = verbs::land::random_key();
    let client = Client::new(&server.base, Some(server.token.clone()));
    let landed = verbs::land::land_changeset(
        &client,
        "wland2",
        &outcome.changeset.to_hex(),
        &key,
        &mut |k| println!("land key: {k}"),
    )
    .expect("land");
    assert_eq!(landed.key, key);
    assert_eq!(landed.main, landed.landed, "main advanced onto the land");
    let (status, refv) = api(&server, "GET", "/v1/repos/wland2/refs/main", None);
    assert_eq!(status, 200, "{refv}");
    assert_eq!(
        refv["target"].as_str().unwrap(),
        landed.landed,
        "refs/main is the flush-landed changeset"
    );
    assert_ne!(landed.landed, cs1, "a NEW changeset landed");

    // A fresh clone sees the mounted writes, byte-exact.
    let out2 = tempfile::tempdir().expect("tempdir");
    let dest2 = out2.path().join("wc2");
    verbs::clone::value(
        &format!("{}/wland2", server.base),
        &[],
        Some(dest2.to_str().expect("utf8")),
        &net(&server),
    )
    .expect("fresh clone");
    assert_eq!(
        std::fs::read(dest2.join("notes.txt")).expect("landed bytes"),
        payload
    );
    assert_eq!(
        std::fs::read(dest2.join("a.txt")).expect("edited bytes"),
        b"changed\n"
    );
    let _ = std::fs::remove_dir_all(&dir);
}

// ---- M8b item 2: FUSE runtime mount (Linux CI with /dev/fuse only) ---------

/// On Linux CI with /dev/fuse + fusermount3 present this runs the REAL FUSE
/// mount; without them it prints a SKIP notice (cfg-compile-only
/// elsewhere — macOS never compiles this test).
#[cfg(target_os = "linux")]
#[test]
fn fuse_kernel_mount_walk_parity() {
    let fuse_ok = Path::new("/dev/fuse").exists()
        && Command::new("fusermount3")
            .arg("--version")
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()
            .map(|s| s.success())
            .unwrap_or(false);
    if !fuse_ok {
        println!(
            "SKIP NOTICE (fuse-e2e): no /dev/fuse or no fusermount3 — the FUSE front-end \
             is cfg-compile-checked only on this runner"
        );
        return;
    }

    let server = start_server();
    let files = demo_files();
    let refs: Vec<(&str, &[u8], bool)> = files
        .iter()
        .map(|(p, d, e)| (*p, d.as_slice(), *e))
        .collect();
    seed_repo(&server, "selffuse", &refs);

    let out = tempfile::tempdir().expect("tempdir");
    let dest = out.path().join("wc");
    verbs::clone::value(
        &format!("{}/selffuse", server.base),
        &[],
        Some(dest.to_str().expect("utf8")),
        &net(&server),
    )
    .expect("clone");

    let cache = tempfile::tempdir().expect("cache tempdir");
    let mnt_parent = tempfile::tempdir().expect("mnt tempdir");
    let mnt = mnt_parent.path().join("m");
    std::fs::create_dir_all(&mnt).expect("mkdir mnt");

    let child = Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args([
            "mount",
            "selffuse",
            "--server",
            &server.base,
            "--token",
            &server.token,
            "--fuse",
            &mnt.to_string_lossy(),
        ])
        .env("XDG_CACHE_HOME", cache.path())
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn naatre mount --fuse");

    // The mountpoint appears in the kernel mount table when the daemon
    // mounted it (bounded wait; /proc/mounts — no external binary).
    let mut filled = false;
    let mnt_str = std::fs::canonicalize(&mnt)
        .expect("canonicalize mnt")
        .to_string_lossy()
        .to_string();
    for _ in 0..100 {
        if linux_mount_table_has(&mnt_str) {
            filled = true;
            break;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    if !filled {
        let e = child.wait_with_output().expect("daemon output");
        panic!(
            "the FUSE mount never appeared: stdout {} stderr {}",
            String::from_utf8_lossy(&e.stdout),
            String::from_utf8_lossy(&e.stderr)
        );
    }

    // Byte parity vs the materialized clone through the kernel FUSE mount.
    let index = Wc::open(&dest).expect("wc").load_index().expect("index");
    let walked = walk_mounted_files(&mnt);
    let mount_paths: Vec<&str> = walked.iter().map(|(p, _, _)| p.as_str()).collect();
    let clone_paths: Vec<&str> = index.rows.keys().map(String::as_str).collect();
    assert_eq!(clone_paths, mount_paths, "identical full walk through FUSE");
    for (path, abs, exec) in &walked {
        let row = &index.rows[path];
        let bytes = std::fs::read(abs).expect("read on fuse mount");
        assert_eq!(
            bytes,
            std::fs::read(dest.join(path)).expect("disk bytes"),
            "{path}"
        );
        assert_eq!(object_id(Kind::Blob, &bytes).0, row.blob, "{path} hash");
        let size = std::fs::metadata(abs).expect("stat").len();
        assert_eq!(size as u64, row.size, "{path} stat size");
        assert_eq!(*exec, row.exec, "{path} exec bit");
    }

    // EROFS default through FUSE too.
    assert!(
        std::fs::write(mnt.join("ro.txt"), b"nope").is_err(),
        "writing to a read-only FUSE mount must fail"
    );

    // SIGTERM unmounts + cleans up.
    unsafe {
        libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
    }
    let out = child.wait_with_output().expect("wait daemon");
    assert!(
        out.status.success(),
        "graceful SIGTERM exits 0: {:?}",
        out.status
    );
    assert!(
        !linux_mount_table_has(&mnt_str),
        "the FUSE mount is gone after SIGTERM"
    );
}

// ---- M8b security pin: NFS handles are unforgeable without the secret -------
//
// The unguessable export path gates only MOUNT; GETATTR/READ need nothing
// but a structurally valid handle, and nfsserve 0.11's default encoding is
// `[u64 epoch-millis-at-daemon-start LE || u64 fileid LE]` (`vfs.rs::
// get_generation_number`) — brute-forceable from a rough start time. The
// daemon overrides id_to_fh/fh_to_id to embed a per-mount 128-bit CSPRNG
// secret: this test drives RAW NFS RPCs at the real daemon binary and pins
// that the REAL handle (mountd MNT hands it out only for the secret path)
// serves GETATTR while both the forged-secret handle and the old default
// layout are rejected as bad handles. No kernel mount needed — runs on
// every platform.

const NFS_PROGRAM: u32 = 100003;
const MOUNT_PROGRAM: u32 = 100005;
const NFS3_OK: u32 = 0;
const NFS3ERR_BADHANDLE: u32 = 10001;

/// One raw RPC over a fresh TCP connection (record-marked, AUTH_SYS
/// credential), returning the raw reply body.
fn raw_rpc(port: u16, prog: u32, vers: u32, proc_num: u32, payload: &[u8]) -> Vec<u8> {
    use std::io::{Read as _, Write as _};
    let mut stream = std::net::TcpStream::connect(("127.0.0.1", port)).expect("connect daemon");
    let mut call = Vec::new();
    call.extend_from_slice(&0x1a2b_3c4du32.to_be_bytes()); // xid
    call.extend_from_slice(&0u32.to_be_bytes()); // CALL
    call.extend_from_slice(&2u32.to_be_bytes()); // rpcvers
    call.extend_from_slice(&prog.to_be_bytes());
    call.extend_from_slice(&vers.to_be_bytes());
    call.extend_from_slice(&proc_num.to_be_bytes());
    // cred: AUTH_SYS (flavor 1) — stamp, machinename "tst" (padded), uid,
    // gid, zero auxiliary gids.
    let mut cred = Vec::new();
    cred.extend_from_slice(&0u32.to_be_bytes());
    cred.extend_from_slice(&3u32.to_be_bytes());
    cred.extend_from_slice(b"tst");
    cred.push(0);
    cred.extend_from_slice(&1000u32.to_be_bytes());
    cred.extend_from_slice(&1000u32.to_be_bytes());
    cred.extend_from_slice(&0u32.to_be_bytes());
    call.extend_from_slice(&1u32.to_be_bytes()); // cred flavor
    call.extend_from_slice(&(cred.len() as u32).to_be_bytes());
    call.extend_from_slice(&cred);
    call.extend_from_slice(&0u32.to_be_bytes()); // verf flavor AUTH_NULL
    call.extend_from_slice(&0u32.to_be_bytes()); // verf length
    call.extend_from_slice(payload);
    // One record-marked fragment: last-fragment bit + length.
    let mut frame = Vec::new();
    frame.extend_from_slice(&((call.len() as u32) | 0x8000_0000).to_be_bytes());
    frame.extend_from_slice(&call);
    stream.write_all(&frame).expect("send rpc");
    stream
        .set_read_timeout(Some(Duration::from_secs(10)))
        .expect("read timeout");
    // Replies may arrive as MULTIPLE record-marked fragments (long READ /
    // READDIR bodies): reassemble until the last-fragment bit.
    let mut reply = Vec::new();
    loop {
        let mut hdr = [0u8; 4];
        stream.read_exact(&mut hdr).expect("reply header");
        let marked = u32::from_be_bytes(hdr);
        let len = (marked & 0x7fff_ffff) as usize;
        let start = reply.len();
        reply.resize(start + len, 0);
        stream.read_exact(&mut reply[start..]).expect("reply body");
        if marked & 0x8000_0000 != 0 {
            break;
        }
    }
    reply
}

/// The nfsstat3 of an accepted RPC reply (msg_type REPLY, accepted, empty
/// verifier, success) — panics otherwise.
fn nfs_status(reply: &[u8]) -> u32 {
    assert!(reply.len() >= 28, "short reply: {} bytes", reply.len());
    assert_eq!(&reply[4..8], &1u32.to_be_bytes(), "msg_type = REPLY");
    assert_eq!(&reply[8..12], &0u32.to_be_bytes(), "accepted");
    assert_eq!(&reply[16..20], &0u32.to_be_bytes(), "empty verifier");
    assert_eq!(&reply[20..24], &0u32.to_be_bytes(), "success");
    u32::from_be_bytes(reply[24..28].try_into().expect("status"))
}

/// XDR opaque for `bytes` (length-prefixed, padded to 4).
fn xdr_opaque(bytes: &[u8]) -> Vec<u8> {
    let mut out = (bytes.len() as u32).to_be_bytes().to_vec();
    out.extend_from_slice(bytes);
    while !out.len().is_multiple_of(4) {
        out.push(0);
    }
    out
}

/// mountd MNT with the export path → the REAL root file handle (mountd
/// refuses any other dirpath with MNT3ERR_NOENT, so this also proves the
/// handle only falls out of the secret path).
fn mnt_real_handle(port: u16, export_path: &str) -> Vec<u8> {
    let reply = raw_rpc(
        port,
        MOUNT_PROGRAM,
        3,
        1,
        &xdr_opaque(export_path.as_bytes()),
    );
    let status = u32::from_be_bytes(reply[24..28].try_into().expect("mnt status"));
    assert_eq!(status, 0, "MNT must succeed for the secret export path");
    let fh_len = u32::from_be_bytes(reply[28..32].try_into().expect("fh len")) as usize;
    reply[32..32 + fh_len].to_vec()
}

#[test]
fn forged_nfs_handles_are_rejected_real_handle_serves() {
    let server = start_server();
    seed_repo(
        &server,
        "handles",
        &[("a.txt", b"secret bytes".as_slice(), false)],
    );
    let cache = tempfile::tempdir().expect("cache tempdir");
    let (mut child, _reader, port, export_path) =
        spawn_mount_daemon(&server, "handles", cache.path(), &[]);

    // The REAL handle: 24 bytes = [secret(16) || fileid(8)].
    let real = mnt_real_handle(port, &export_path);
    assert_eq!(real.len(), 24, "handle = [secret(16) || fileid(8)]");
    assert_eq!(
        nfs_status(&raw_rpc(port, NFS_PROGRAM, 3, 1, &xdr_opaque(&real))),
        NFS3_OK,
        "the real handle serves GETATTR"
    );

    // Forged: right layout, WRONG secret → bad handle (no distinguishing
    // error — same code for wrong length and wrong secret, no oracle).
    let mut forged = vec![0xAAu8; 16];
    forged.extend_from_slice(&1u64.to_le_bytes());
    assert_eq!(
        nfs_status(&raw_rpc(port, NFS_PROGRAM, 3, 1, &xdr_opaque(&forged))),
        NFS3ERR_BADHANDLE,
        "a forged-secret handle is a bad handle"
    );

    // The old nfsserve DEFAULT layout ([epoch-ms || fileid]) is dead.
    let mut default_layout = 1_700_000_000_000u64.to_le_bytes().to_vec();
    default_layout.extend_from_slice(&1u64.to_le_bytes());
    assert_eq!(
        nfs_status(&raw_rpc(
            port,
            NFS_PROGRAM,
            3,
            1,
            &xdr_opaque(&default_layout)
        )),
        NFS3ERR_BADHANDLE,
        "the predictable default layout is rejected"
    );

    // And mountd still refuses non-secret export paths.
    let noent = raw_rpc(
        port,
        MOUNT_PROGRAM,
        3,
        1,
        &xdr_opaque(b"/naatre/00000000000000000000000000000000"),
    );
    assert_eq!(
        u32::from_be_bytes(noent[24..28].try_into().expect("mnt status")),
        2, // MNT3ERR_NOENT
        "the wrong export path never yields a handle"
    );

    unsafe {
        libc::kill(child.id() as libc::pid_t, libc::SIGTERM);
    }
    let status = child.wait().expect("wait daemon");
    assert!(status.success(), "graceful SIGTERM exits 0: {status}");
}
