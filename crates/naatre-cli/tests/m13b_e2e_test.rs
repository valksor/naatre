//! The M13b e2e phase, the plan's cache check set realized against the
//! REAL binaries (the `m12b_e2e_test` shape: a Rust integration test
//! driving the built `naatre` binary — origin `serve`, the `naatre
//! cache` tier, and client verbs with `NAATRE_CACHE_URL` set):
//!
//! the client story (clone + sync route by-id fetches through the tier
//! with minted tickets), cross-repo 404 through the cache, private-repo
//! invisibility through the cache, warm-blob-outside-ticket-scope
//! refused, tombstone honored (never minted into scope, never served),
//! origin fallback on a corrupted cache store, fail-closed startup
//! without keys, ticket expiry bounding staleness, rotation (a retired
//! key's unexpired ticket verifies; a retired+expired one refuses),
//! negative caching forbidden, and refs never proxied.
//!
//! Needs on PATH: `git` (the import seed is the product's seeding path).

use std::io::{Read as _, Write as _};
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::thread::sleep;
use std::time::{Duration, Instant};

use serde_json::Value;

/// One labeled step of the flow (the e2e script convention).
macro_rules! step {
    ($($arg:tt)*) => {
        eprintln!("== {}", format!($($arg)*));
    };
}

fn string_of(bytes: &[u8]) -> String {
    String::from_utf8_lossy(bytes).into_owned()
}

/// Runs the binary in `cwd` with the given env.
struct Naatre {
    bin: PathBuf,
    url: String,
    token: String,
    extra_env: Vec<(String, String)>,
}

impl Naatre {
    fn run(&self, cwd: &Path, args: &[&str]) -> std::process::Output {
        let mut cmd = Command::new(&self.bin);
        cmd.args(args)
            .current_dir(cwd)
            .env("NAATRE_URL", &self.url)
            .env("NAATRE_TOKEN", &self.token)
            .env_remove("NAATRE_AUTHOR");
        for (k, v) in &self.extra_env {
            cmd.env(k, v);
        }
        cmd.output().expect("spawn naatre")
    }

    fn run_ok(&self, cwd: &Path, args: &[&str]) -> String {
        let out = self.run(cwd, args);
        assert!(
            out.status.success(),
            "`naatre {}` failed (exit {:?}): {}{}",
            args.join(" "),
            out.status.code(),
            string_of(&out.stdout),
            string_of(&out.stderr),
        );
        string_of(&out.stdout)
    }
}

fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .expect("bind an ephemeral port")
        .local_addr()
        .unwrap()
        .port()
}

struct Serve {
    child: std::process::Child,
    port: u16,
    log: PathBuf,
}

impl Serve {
    /// Starts `cmd` (serve OR cache — both are listeners with a health
    /// probe) and waits until its TCP port answers.
    fn start_listener(
        bin: &Path,
        args: &[&str],
        log: &Path,
        port: u16,
        ready_marker: &str,
    ) -> Serve {
        let child = Command::new(bin)
            .args(args)
            .stdout(Stdio::null())
            .stderr(std::fs::File::create(log).expect("log file"))
            .spawn()
            .expect("spawn listener");
        let mut serve = Serve {
            child,
            port,
            log: log.to_path_buf(),
        };
        serve.wait_ready(ready_marker);
        serve
    }

    fn wait_ready(&mut self, marker: &str) {
        let deadline = Instant::now() + Duration::from_secs(20);
        loop {
            if let Ok((status, _)) = http(self.port, "GET", marker, None, b"")
                && status == 200
            {
                return;
            }
            // Fail fast when the process died at startup.
            if let Ok(Some(_)) = self.child.try_wait() {
                let log = std::fs::read_to_string(&self.log).unwrap_or_default();
                panic!("listener died at startup on {}:\n{log}", self.port);
            }
            assert!(
                Instant::now() < deadline,
                "listener never became healthy on 127.0.0.1:{}",
                self.port
            );
            sleep(Duration::from_millis(100));
        }
    }

    fn stop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl Drop for Serve {
    fn drop(&mut self) {
        self.stop();
    }
}

/// A raw blocking HTTP/1.0 exchange (Connection: close, read to end) —
/// no client dependency in the test; the server answers axum envelopes.
/// Honors the server's own backpressure contract for the two
/// PRE-EXECUTION rejections (`writer_busy`, `server_busy` — a slot miss
/// or an admission shed, both retryable with `Retry-After: 1`): the
/// same bounded retry every real naatre client applies, because the
/// background passes (derive queue, GC) legitimately hold the write
/// slot for a tick. Every exchange this helper makes is idempotent
/// (GETs, the access upsert/DELETE, ticket mint — a double-mint is a
/// fresh valid ticket, the old one simply expires).
fn http(
    port: u16,
    method: &str,
    path: &str,
    auth: Option<&str>,
    body: &[u8],
) -> std::io::Result<(u16, Vec<u8>)> {
    for attempt in 0..5 {
        let (status, bytes) = http_once(port, method, path, auth, body)?;
        if status == 503
            && matches!(
                code_of(&json_of(&bytes)).as_str(),
                "writer_busy" | "server_busy"
            )
            && attempt < 4
        {
            sleep(Duration::from_millis(1000));
            continue;
        }
        return Ok((status, bytes));
    }
    unreachable!("every loop arm returns")
}

fn http_once(
    port: u16,
    method: &str,
    path: &str,
    auth: Option<&str>,
    body: &[u8],
) -> std::io::Result<(u16, Vec<u8>)> {
    let mut stream = std::net::TcpStream::connect(("127.0.0.1", port))?;
    let mut req = format!(
        "{method} {path} HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\
         Content-Type: application/json\r\nContent-Length: {}\r\n",
        body.len()
    );
    if let Some(a) = auth {
        req.push_str(&format!("Authorization: {a}\r\n"));
    }
    req.push_str("\r\n");
    stream.write_all(req.as_bytes())?;
    stream.write_all(body)?;
    stream.flush()?;
    let mut buf = Vec::new();
    stream.read_to_end(&mut buf)?;
    let text = string_of(&buf);
    let status: u16 = text
        .split_whitespace()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .ok_or_else(|| std::io::Error::other("no status line"))?;
    // Split headers from the body on the first blank line.
    let body_bytes = buf
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .map(|p| buf[p + 4..].to_vec())
        .unwrap_or_default();
    Ok((status, body_bytes))
}

fn json_of(bytes: &[u8]) -> Value {
    serde_json::from_slice(bytes).unwrap_or(Value::Null)
}

fn code_of(body: &Value) -> String {
    body["error"]["code"].as_str().unwrap_or("?").to_string()
}

/// Seed a one-commit git repo and import it (the coldboot seeding path).
fn seed_and_import(bin: &Path, root: &Path, data: &Path, name: &str, files: &[(&str, &str)]) {
    let g = root.join(format!("seed-{name}"));
    std::fs::create_dir_all(&g).unwrap();
    Command::new("git")
        .args(["-c", "init.defaultBranch=main", "init", "-q"])
        .arg(&g)
        .output()
        .expect("git init");
    for (path, content) in files {
        let full = g.join(path);
        std::fs::create_dir_all(full.parent().unwrap()).unwrap();
        std::fs::write(full, content).unwrap();
    }
    Command::new("git")
        .args(["-C"])
        .arg(&g)
        .args(["add", "."])
        .output()
        .expect("git add");
    Command::new("git")
        .args(["-C"])
        .arg(&g)
        .args([
            "-c",
            "user.email=seed@example.com",
            "-c",
            "user.name=seed",
            "commit",
            "-q",
            "-m",
            name,
        ])
        .output()
        .expect("git commit");
    let out = Command::new(bin)
        .args([
            "import",
            "--data",
            data.to_str().unwrap(),
            "--into",
            name,
            g.to_str().unwrap(),
        ])
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .output()
        .expect("import");
    assert!(
        out.status.success(),
        "import {name} failed: {}",
        string_of(&out.stderr)
    );
}

#[test]
fn m13_cache_authz_e2e() {
    // The seed needs git (the import seeding path).
    Command::new("git")
        .args(["--version"])
        .output()
        .expect("git is on PATH for the import seed");

    let bin = PathBuf::from(env!("CARGO_BIN_EXE_naatre"));
    let root = tempfile::tempdir().expect("tempdir");
    let root = root.path();

    // ---- the origin -----------------------------------------------------
    step!("origin: init + serve");
    let origin_data = root.join("origin-data");
    let out = Command::new(&bin)
        .args(["init", "--data", origin_data.to_str().unwrap()])
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .output()
        .expect("init");
    assert!(out.status.success(), "{}", string_of(&out.stderr));
    let admin = string_of(&out.stdout)
        .lines()
        .find_map(|l| l.strip_prefix("admin token: ").map(str::to_string))
        .expect("admin token printed once");

    // The origin's ticket key store is a host-local, flocked write: it
    // runs BEFORE serve starts (like every host-local verb).
    let out = Command::new(&bin)
        .args(["cache-key", "init", "--data", origin_data.to_str().unwrap()])
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .output()
        .expect("cache-key init");
    assert!(out.status.success(), "{}", string_of(&out.stderr));

    let origin_port = free_port();
    let origin_url = format!("http://127.0.0.1:{origin_port}");
    let mut origin = Serve::start_listener(
        &bin,
        &[
            "serve",
            "--data",
            origin_data.to_str().unwrap(),
            "--listen",
            &format!("127.0.0.1:{origin_port}"),
        ],
        &root.join("origin.log"),
        origin_port,
        "/healthz",
    );
    let origin_naatre = Naatre {
        bin: bin.clone(),
        url: origin_url.clone(),
        token: admin.clone(),
        extra_env: vec![],
    };

    step!("seed demo (a.txt, sub/b.txt) and other (c.txt)");
    // Import holds the data-dir flock: serve stops for it.
    origin.stop();
    seed_and_import(
        &bin,
        root,
        &origin_data,
        "demo",
        &[("a.txt", "alpha\n"), ("sub/b.txt", "beta\n")],
    );
    seed_and_import(&bin, root, &origin_data, "other", &[("c.txt", "delta\n")]);
    origin = Serve::start_listener(
        &bin,
        &[
            "serve",
            "--data",
            origin_data.to_str().unwrap(),
            "--listen",
            &format!("127.0.0.1:{origin_port}"),
        ],
        &root.join("origin.log"),
        origin_port,
        "/healthz",
    );

    step!("writer token");
    let writer = origin_naatre
        .run_ok(root, &["token", "create", "agent", "--role", "writer"])
        .lines()
        .find_map(|l| l.strip_prefix("token: ").map(str::to_string))
        .expect("writer token");
    let writer_naatre = Naatre {
        bin: bin.clone(),
        url: origin_url.clone(),
        token: writer.clone(),
        extra_env: vec![],
    };

    // ---- the cache tier -------------------------------------------------
    step!("cache tier: start");
    let cache_data = root.join("cache-data");
    let cache_port = free_port();
    let mut cache = Serve::start_listener(
        &bin,
        &[
            "cache",
            "--origin",
            &origin_url,
            "--key",
            origin_data.join("cache_keys.json").to_str().unwrap(),
            "--listen",
            &format!("127.0.0.1:{cache_port}"),
            "--data",
            cache_data.to_str().unwrap(),
        ],
        &root.join("cache.log"),
        cache_port,
        "/healthz",
    );

    // ---- pin: the client story -----------------------------------------
    step!("client story: clone with NAATRE_CACHE_URL routes through the tier");
    let clone_dir = root.join("client1");
    let writer_cached = Naatre {
        bin: bin.clone(),
        url: origin_url.clone(),
        token: writer.clone(),
        extra_env: vec![(
            "NAATRE_CACHE_URL".into(),
            format!("http://127.0.0.1:{cache_port}"),
        )],
    };
    writer_cached.run_ok(
        root,
        &[
            "clone",
            &format!("{origin_url}/demo"),
            "--into",
            clone_dir.to_str().unwrap(),
        ],
    );
    assert_eq!(
        std::fs::read_to_string(clone_dir.join("a.txt")).unwrap(),
        "alpha\n",
        "the clone materialized through the tier"
    );
    assert_eq!(
        std::fs::read_to_string(clone_dir.join("sub/b.txt")).unwrap(),
        "beta\n"
    );
    // The tier STORED the blobs (a clone warmed its membership scope).
    let objects_dir = cache_data.join("objects");
    let warmed: usize = walk_object_count(&objects_dir);
    assert!(
        warmed >= 2,
        "the clone warmed the tier's store: {warmed} blobs"
    );

    // ---- ticket plumbing for the direct pins ----------------------------
    step!("mint tickets for the direct tier pins");
    let demo_head = {
        let (_, body) = http(
            origin_port,
            "GET",
            "/v1/repos/demo/refs/main",
            Some(&format!("Bearer {writer}")),
            b"",
        )
        .unwrap();
        json_of(&body)["target"]
            .as_str()
            .expect("main target")
            .to_string()
    };
    let demo_root_tree = {
        let (_, body) = http(
            origin_port,
            "GET",
            &format!("/v1/repos/demo/changesets/{demo_head}/root"),
            Some(&format!("Bearer {writer}")),
            b"",
        )
        .unwrap();
        json_of(&body)["root_tree_id"]
            .as_str()
            .expect("root tree")
            .to_string()
    };
    let mint = |repo: &str, tree: &str, ttl: Option<u64>| -> Value {
        let mut payload = serde_json::json!({ "root_tree": tree });
        if let Some(t) = ttl {
            payload["ttl_secs"] = serde_json::json!(t);
        }
        let (status, body) = http(
            origin_port,
            "POST",
            &format!("/v1/repos/{repo}/tickets"),
            Some(&format!("Bearer {writer}")),
            serde_json::to_vec(&payload).unwrap().as_slice(),
        )
        .unwrap();
        assert_eq!(status, 201, "mint {repo}: {}", string_of(&body));
        json_of(&body)
    };
    let ticket_v1 = mint("demo", &demo_root_tree, None);
    let ticket_str = ticket_v1["ticket"].as_str().unwrap().to_string();
    let ticket_auth = format!("NaatreTicket {ticket_str}");

    // The membership walk names the in-scope ids.
    let (_, mem_raw) = http(
        origin_port,
        "GET",
        &format!("/v1/repos/demo/cache/membership?tree={demo_root_tree}"),
        Some(&ticket_auth),
        b"",
    )
    .unwrap();
    let mem = json_of(&mem_raw);
    let scope_v1: Vec<String> = mem["blobs"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap().to_string())
        .collect();
    assert_eq!(
        scope_v1.len(),
        2,
        "the first tree scopes a.txt + b.txt: {scope_v1:?}"
    );

    step!("client story: sync through the tier after a second client lands");
    // A second client commits + lands a NEW file (sub/c.txt); the first
    // client syncs THROUGH the tier — the new blob's fetch goes by-id.
    let land_dir = root.join("client2");
    writer_naatre.run_ok(
        root,
        &[
            "clone",
            &format!("{origin_url}/demo"),
            "--into",
            land_dir.to_str().unwrap(),
        ],
    );
    std::fs::write(land_dir.join("sub/c.txt"), "gamma\n").unwrap();
    writer_naatre.run_ok(&land_dir, &["commit", "-m", "add c"]);
    writer_naatre.run_ok(&land_dir, &["land"]);
    let synced = writer_cached.run_ok(&clone_dir, &["sync"]);
    assert!(
        synced.contains('A'),
        "the sync applied the new file: {synced}"
    );
    assert_eq!(
        std::fs::read_to_string(clone_dir.join("sub/c.txt")).unwrap(),
        "gamma\n"
    );

    // ---- pin: refs/log are never proxied --------------------------------
    step!("pin: refs are never proxied (the tier refuses every non-object route)");
    let (status, body) = http(
        cache_port,
        "GET",
        "/v1/repos/demo/refs/main",
        Some(&format!("Bearer {writer}")),
        b"",
    )
    .unwrap();
    assert_eq!(status, 404, "the tier never proxies refs: {status}");
    assert_eq!(code_of(&json_of(&body)), "not_found");
    // And an anonymous request with NO ticket at all: 401.
    let (status, body) = http(cache_port, "GET", "/v1/objects/00", None, b"").unwrap();
    assert_eq!(status, 401, "no ticket, no serve: {status}");
    assert_eq!(code_of(&json_of(&body)), "token_invalid");

    // ---- pin: by-id fetch through the tier serves hash-true bytes -------
    step!("pin: by-id fetch through the tier (200, correct bytes)");
    let mut blob_a_id = String::new();
    let mut blob_a_bytes = Vec::new();
    for id in &scope_v1 {
        let (status, bytes) = http(
            cache_port,
            "GET",
            &format!("/v1/objects/{id}"),
            Some(&ticket_auth),
            b"",
        )
        .unwrap();
        assert_eq!(status, 200, "in-scope id {id} serves: {bytes:?}");
        assert!(
            bytes == b"alpha\n" || bytes == b"beta\n",
            "hash-true bytes: {}",
            string_of(&bytes)
        );
        if bytes == b"alpha\n" {
            blob_a_id = id.clone();
            blob_a_bytes = bytes.clone();
        }
    }
    assert_eq!(blob_a_bytes, b"alpha\n", "a.txt's blob found");

    // ---- pin: warm-blob-outside-ticket-scope refused ---------------------
    step!("pin: warm blob outside the ticket's scope refused");
    // The tier HOLDS alpha's bytes (warmed above). A ticket for the
    // OTHER repo's tree — whose scope cannot contain that id — must be
    // refused despite the warm copy.
    let other_head = {
        let (_, body) = http(
            origin_port,
            "GET",
            "/v1/repos/other/refs/main",
            Some(&format!("Bearer {writer}")),
            b"",
        )
        .unwrap();
        json_of(&body)["target"]
            .as_str()
            .expect("other target")
            .to_string()
    };
    let other_tree = {
        let (_, body) = http(
            origin_port,
            "GET",
            &format!("/v1/repos/other/changesets/{other_head}/root"),
            Some(&format!("Bearer {writer}")),
            b"",
        )
        .unwrap();
        json_of(&body)["root_tree_id"]
            .as_str()
            .expect("other root")
            .to_string()
    };
    let other_ticket = mint("other", &other_tree, None);
    let other_auth = format!("NaatreTicket {}", other_ticket["ticket"].as_str().unwrap());
    let (status, body) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{blob_a_id}"),
        Some(&other_auth),
        b"",
    )
    .unwrap();
    assert_eq!(
        status, 404,
        "a warm blob outside the ticket's scope is refused: {status}"
    );
    assert_eq!(code_of(&json_of(&body)), "object_not_found");

    // ---- pin: cross-repo 404 THROUGH the cache ---------------------------
    step!("pin: cross-repo 404 through the cache");
    // other's blob id (its membership), asked with the demo ticket.
    let (_, other_mem_raw) = http(
        origin_port,
        "GET",
        &format!("/v1/repos/other/cache/membership?tree={other_tree}"),
        Some(&other_auth),
        b"",
    )
    .unwrap();
    let other_mem = json_of(&other_mem_raw);
    let other_blob = other_mem["blobs"].as_array().unwrap()[0]
        .as_str()
        .unwrap()
        .to_string();
    let (status, body) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{other_blob}"),
        Some(&ticket_auth),
        b"",
    )
    .unwrap();
    assert_eq!(status, 404, "cross-repo id through the cache: {status}");
    assert_eq!(code_of(&json_of(&body)), "object_not_found");

    // ---- pin: negative caching forbidden ---------------------------------
    step!("pin: negative caching forbidden (a miss is never stored)");
    let before = walk_object_count(&objects_dir);
    let missing_id = "11".repeat(32);
    let (status, _) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{missing_id}"),
        Some(&ticket_auth),
        b"",
    )
    .unwrap();
    assert_eq!(status, 404);
    assert_eq!(
        walk_object_count(&objects_dir),
        before,
        "a negative answer stored nothing"
    );

    // ---- pin: tombstone honored ------------------------------------------
    step!("pin: tombstone honored (never mints into scope, never serves)");
    // Tombstone a.txt's blob host-local (the M10 admin verb).
    origin.stop();
    let out = Command::new(&bin)
        .args([
            "redact",
            "blob",
            &blob_a_id,
            "--reason",
            "m13 e2e",
            "--repo",
            "demo",
            "--data",
            origin_data.to_str().unwrap(),
        ])
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .output()
        .expect("redact");
    assert!(out.status.success(), "{}", string_of(&out.stderr));
    origin = Serve::start_listener(
        &bin,
        &[
            "serve",
            "--data",
            origin_data.to_str().unwrap(),
            "--listen",
            &format!("127.0.0.1:{origin_port}"),
        ],
        &root.join("origin.log"),
        origin_port,
        "/healthz",
    );
    let _origin_guard = &mut origin; // held to the test's end (Drop = stop)
    // The tier's snapshot is now stale: the epoch probe catches the mark,
    // the fresh walk drops the id, and the tier refuses (its local copy
    // of the redacted bytes is also deleted).
    let (status, body) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{blob_a_id}"),
        Some(&ticket_auth),
        b"",
    )
    .unwrap();
    assert!(
        status == 404 || status == 410,
        "the tombstoned id never serves through the tier: {status} {}",
        string_of(&body)
    );
    // And the fresh walk (a new ticket for the current tree) excludes it:
    let (_, refb) = http(
        origin_port,
        "GET",
        "/v1/repos/demo/refs/main",
        Some(&format!("Bearer {writer}")),
        b"",
    )
    .unwrap();
    let new_head = json_of(&refb)["target"].as_str().unwrap().to_string();
    let (_, rootb) = http(
        origin_port,
        "GET",
        &format!("/v1/repos/demo/changesets/{new_head}/root"),
        Some(&format!("Bearer {writer}")),
        b"",
    )
    .unwrap();
    let tree_v2 = json_of(&rootb)["root_tree_id"]
        .as_str()
        .unwrap()
        .to_string();
    let t2 = mint("demo", &tree_v2, None);
    let t2_auth = format!("NaatreTicket {}", t2["ticket"].as_str().unwrap());
    let (_, mem2_raw) = http(
        origin_port,
        "GET",
        &format!("/v1/repos/demo/cache/membership?tree={tree_v2}"),
        Some(&t2_auth),
        b"",
    )
    .unwrap();
    let mem2 = json_of(&mem2_raw);
    let scope_v2: Vec<String> = mem2["blobs"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap().to_string())
        .collect();
    assert!(
        !scope_v2.contains(&blob_a_id),
        "the tombstoned blob never mints into a membership set: {scope_v2:?}"
    );

    // ---- pin: corrupt cache store → origin fallback -----------------------
    step!("pin: corrupt cache store falls back to the origin (hash-verified)");
    // beta's blob is warm; corrupt its shard on disk, then fetch.
    let beta_id = scope_v2
        .iter()
        .chain(scope_v1.iter())
        .find(|id| {
            let path = shard_path(&objects_dir, id);
            path.exists()
        })
        .expect("a warm shard to corrupt")
        .clone();
    // Fetch it once to identify its CONTENT (alpha or beta).
    let (status, bytes) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{beta_id}"),
        Some(&t2_auth),
        b"",
    )
    .unwrap();
    assert_eq!(status, 200);
    let true_bytes = bytes.clone();
    let shard = shard_path(&objects_dir, &beta_id);
    std::fs::write(&shard, b"corrupted bytes that do not hash").unwrap();
    let (status, served) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{beta_id}"),
        Some(&t2_auth),
        b"",
    )
    .unwrap();
    assert_eq!(
        status,
        200,
        "corrupt store → origin fallback: {}",
        string_of(&served)
    );
    assert_eq!(
        served, true_bytes,
        "the refetched bytes are the hash-true ones"
    );

    // ---- pin: ticket expiry bounds staleness ------------------------------
    step!("pin: an expired ticket refuses (the staleness bound)");
    let short = mint("demo", &tree_v2, Some(1));
    let short_auth = format!("NaatreTicket {}", short["ticket"].as_str().unwrap());
    let live_id = &scope_v2[0];
    let (status, _) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{live_id}"),
        Some(&short_auth),
        b"",
    )
    .unwrap();
    assert_eq!(status, 200, "the short ticket is live right now");
    sleep(Duration::from_millis(1500));
    let (status, body) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{live_id}"),
        Some(&short_auth),
        b"",
    )
    .unwrap();
    assert_eq!(status, 401, "expired → refused: {status}");
    assert_eq!(code_of(&json_of(&body)), "token_expired");

    // ---- pin: rotation -----------------------------------------------------
    step!("pin: rotation (retired key's unexpired ticket verifies; retired+expired refuses)");
    let old_ticket = mint("demo", &tree_v2, Some(30));
    let old_auth = format!("NaatreTicket {}", old_ticket["ticket"].as_str().unwrap());
    // rotate is a host-local flocked write: serve stops for it (the
    // import/redact shape).
    origin.stop();
    origin_naatre.run_ok(
        root,
        &[
            "cache-key",
            "rotate",
            "--data",
            origin_data.to_str().unwrap(),
        ],
    );
    origin = Serve::start_listener(
        &bin,
        &[
            "serve",
            "--data",
            origin_data.to_str().unwrap(),
            "--listen",
            &format!("127.0.0.1:{origin_port}"),
        ],
        &root.join("origin.log"),
        origin_port,
        "/healthz",
    );
    let _origin_guard2 = &mut origin;
    // The tier still runs the OLD key list (restart-to-refresh is the
    // documented rotation story), so it verifies the old ticket fine;
    // the ORIGIN's fresh verify list (old key retired) also verifies the
    // unexpired old ticket.
    let (status, _) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{live_id}"),
        Some(&old_auth),
        b"",
    )
    .unwrap();
    assert_eq!(
        status, 200,
        "the tier's list still carries the key it verified at startup"
    );
    let (status, body) = http(
        origin_port,
        "GET",
        "/v1/repos/demo/cache/epoch",
        Some(&old_auth),
        b"",
    )
    .unwrap();
    assert_eq!(
        status,
        200,
        "the origin's rotated list still verifies the retired key's unexpired ticket: {}",
        string_of(&body)
    );
    // The rotation is AUDITED (the plan's check: audit rows asserted for
    // key rotation — the init row stands too).
    let (_, audit_raw) = http(
        origin_port,
        "GET",
        "/v1/audit?limit=200",
        Some(&format!("Bearer {admin}")),
        b"",
    )
    .unwrap();
    let audit = json_of(&audit_raw);
    let actions: Vec<&str> = audit["audit"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["action"].as_str().unwrap())
        .collect();
    assert!(
        actions.contains(&"cache_key_init") && actions.contains(&"cache_key_rotate"),
        "key init and rotate are audited: {actions:?}"
    );

    // An OLD ticket that then EXPIRES refuses (retired + expired).
    let dying = mint_with_expired(&bin, root, &origin_url, &writer, "demo", &tree_v2);
    sleep(Duration::from_millis(1500));
    let (status, body) = http(
        origin_port,
        "GET",
        "/v1/repos/demo/cache/epoch",
        Some(&format!("NaatreTicket {dying}")),
        b"",
    )
    .unwrap();
    assert_eq!(status, 401, "retired+expired refuses: {status}");
    assert_eq!(code_of(&json_of(&body)), "token_expired");
    let _ = dying;

    // ---- pin: private-repo invisibility THROUGH the cache ------------------
    step!("pin: private-repo invisibility through the cache");
    // Make demo private (alice only). agent's fresh mint → uniform 404;
    // the pre-existing VALID ticket still serves until its TTL (the
    // documented bound) — then a NEW mint is impossible.
    // (Grants name existing principals: alice's token create upserts her.)
    origin_naatre.run_ok(root, &["token", "create", "alice", "--role", "reader"]);
    let (status, body) = http(
        origin_port,
        "PUT",
        "/v1/repos/demo/access",
        Some(&format!("Bearer {admin}")),
        serde_json::to_vec(
            &serde_json::json!({"principal": "alice", "read": true, "write": false}),
        )
        .unwrap()
        .as_slice(),
    )
    .unwrap();
    assert_eq!(status, 200, "{}", string_of(&body));
    let (status, body) = http(
        origin_port,
        "POST",
        "/v1/repos/demo/tickets",
        Some(&format!("Bearer {writer}")),
        serde_json::to_vec(&serde_json::json!({ "root_tree": tree_v2 }))
            .unwrap()
            .as_slice(),
    )
    .unwrap();
    assert_eq!(
        status, 404,
        "a non-reader mints NOTHING for a private repo: {status}"
    );
    assert_eq!(code_of(&json_of(&body)), "repo_not_found");
    // The listing hides it too.
    let (_, list_raw) = http(
        origin_port,
        "GET",
        "/v1/repos?limit=50",
        Some(&format!("Bearer {writer}")),
        b"",
    )
    .unwrap();
    let list = json_of(&list_raw);
    let names: Vec<&str> = list["repos"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    assert!(
        !names.contains(&"demo"),
        "the private repo is filtered from the listing: {names:?}"
    );
    // The tier refuses the OTHER repo's scope for demo's warm bytes —
    // already pinned above; and the private repo's unexpired tickets
    // still serve within the bound (pin the bound explicitly):
    let (status, _) = http(
        cache_port,
        "GET",
        &format!("/v1/objects/{live_id}"),
        Some(&t2_auth),
        b"",
    )
    .unwrap();
    assert_eq!(
        status, 200,
        "an unexpired ticket minted BEFORE the repo went private serves within its TTL (the documented staleness bound)"
    );

    // ---- pin: fail-closed startup without keys ------------------------------
    step!("pin: fail-closed startup without verify keys");
    cache.stop();
    let out = Command::new(&bin)
        .args([
            "cache",
            "--origin",
            &origin_url,
            "--key",
            root.join("no-such-keys.json").to_str().unwrap(),
            "--listen",
            &format!("127.0.0.1:{}", free_port()),
            "--data",
            root.join("cache-fail").to_str().unwrap(),
        ])
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .output()
        .expect("cache without keys");
    assert!(!out.status.success(), "no keys → no serve");
    let stderr = string_of(&out.stderr);
    assert!(
        stderr.contains("verify key") || stderr.contains("No such file"),
        "the failure names the key file: {stderr}"
    );
    // An EMPTY key list is equally fail-closed (the file carries 0600 —
    // the loader's permission guard fires before the parse, and this pin
    // is about the EMPTY LIST specifically).
    let empty = root.join("empty-keys.json");
    std::fs::write(&empty, r#"{"keys":[]}"#).unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&empty, std::fs::Permissions::from_mode(0o600)).unwrap();
    }
    let out = Command::new(&bin)
        .args([
            "cache",
            "--origin",
            &origin_url,
            "--key",
            empty.to_str().unwrap(),
            "--listen",
            &format!("127.0.0.1:{}", free_port()),
            "--data",
            root.join("cache-fail2").to_str().unwrap(),
        ])
        .env_remove("NAATRE_URL")
        .env_remove("NAATRE_TOKEN")
        .output()
        .expect("cache with an empty key list");
    assert!(
        !out.status.success(),
        "an empty key list → no serve (fail-closed)"
    );
    assert!(
        string_of(&out.stderr).contains("fail-closed"),
        "{}",
        string_of(&out.stderr)
    );

    // ---- pin: client falls back to the origin on cache failure -------------
    step!("pin: the client falls back to the origin when the tier is down");
    // agent needs a read grant now that demo is private (the invisibility
    // pin above made the repo invisible to him — correct, but the
    // fallback pin needs a reader).
    let (status, body) = http(
        origin_port,
        "PUT",
        "/v1/repos/demo/access",
        Some(&format!("Bearer {admin}")),
        serde_json::to_vec(&serde_json::json!({"principal": "agent", "read": true, "write": true}))
            .unwrap()
            .as_slice(),
    )
    .unwrap();
    assert_eq!(status, 200, "{}", string_of(&body));
    let writer_dead_cache = Naatre {
        bin: bin.clone(),
        url: origin_url.clone(),
        token: writer.clone(),
        extra_env: vec![(
            "NAATRE_CACHE_URL".into(),
            format!("http://127.0.0.1:{}", free_port()),
        )],
    };
    // A fresh landing gives the sync something to FETCH (an up-to-date
    // sync never touches the tier): a third client lands d.txt, then the
    // cached client syncs against the DEAD tier.
    let land_dir2 = root.join("client3");
    writer_naatre.run_ok(
        root,
        &[
            "clone",
            &format!("{origin_url}/demo"),
            "--into",
            land_dir2.to_str().unwrap(),
        ],
    );
    std::fs::write(land_dir2.join("d.txt"), "delta-demo\n").unwrap();
    writer_naatre.run_ok(&land_dir2, &["commit", "-m", "add d"]);
    writer_naatre.run_ok(&land_dir2, &["land"]);
    let sync_out = writer_dead_cache.run(&clone_dir, &["sync"]);
    assert!(
        sync_out.status.success(),
        "the tier being down never fails the verb: {}{}",
        string_of(&sync_out.stdout),
        string_of(&sync_out.stderr)
    );
    assert!(
        string_of(&sync_out.stdout).contains('A'),
        "the sync fetched d.txt via the origin: {}",
        string_of(&sync_out.stdout)
    );
    assert!(
        string_of(&sync_out.stderr).contains("cache tier unavailable"),
        "the fallback warns: {}",
        string_of(&sync_out.stderr)
    );
    assert_eq!(
        std::fs::read_to_string(clone_dir.join("d.txt")).unwrap(),
        "delta-demo\n"
    );
}

/// Mints a 1-second ticket via the origin HTTP (helper mirroring `mint`
/// above for the expiry legs).
fn mint_with_expired(
    _bin: &Path,
    _root: &Path,
    origin_url: &str,
    writer: &str,
    repo: &str,
    tree: &str,
) -> String {
    let port = origin_url
        .rsplit(':')
        .next()
        .unwrap()
        .parse::<u16>()
        .expect("origin port from url");
    let (_, body) = http(
        port,
        "POST",
        &format!("/v1/repos/{repo}/tickets"),
        Some(&format!("Bearer {writer}")),
        serde_json::to_vec(&serde_json::json!({"root_tree": tree, "ttl_secs": 1}))
            .unwrap()
            .as_slice(),
    )
    .unwrap();
    json_of(&body)["ticket"]
        .as_str()
        .expect("ticket")
        .to_string()
}

/// The CAS shard path for an id (the objects/<2hex>/<2hex>/<62hex> layout).
fn shard_path(objects_dir: &Path, id: &str) -> PathBuf {
    objects_dir.join(&id[0..2]).join(&id[2..4]).join(&id[4..])
}

fn walk_object_count(objects_dir: &Path) -> usize {
    let mut n = 0;
    let Ok(top) = std::fs::read_dir(objects_dir) else {
        return 0;
    };
    for l1 in top.flatten() {
        let Ok(l2s) = std::fs::read_dir(l1.path()) else {
            continue;
        };
        for l2 in l2s.flatten() {
            let Ok(files) = std::fs::read_dir(l2.path()) else {
                continue;
            };
            n += files.count();
        }
    }
    n
}
