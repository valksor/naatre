//! The M12 e2e phase, the plan's check realized against the REAL binary
//! (the `tests/e2e.sh` shape of the plan, in the form every milestone's
//! e2e actually ships: a Rust integration test driving the built binary
//! end to end — coldboot's flow, labeled steps, pinned exit codes):
//!
//! two agents land conflicting edits → `land --record-conflict` lands a
//! conflicted node → a second client syncs the conflicted main (markers
//! materialize, `status` shows conflicts) → a third resolves via
//! `naatre resolve` → linear history → GC + blame after the conflicted
//! land.
//!
//! Needs on PATH: `git` (the import seed is the product's seeding path —
//! the coldboot shape; import holds the data-dir flock, so serve stops
//! for the import and for the host-local `gc`/`fsck` legs).

use std::io::Read as _;
use std::io::Write as _;
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

/// One labeled step of the flow (the e2e script convention: the phase
/// name prints as it runs, so a failure names its phase).
macro_rules! step {
    ($($arg:tt)*) => {
        eprintln!("== {}", format!($($arg)*));
    };
}

fn string_of(bytes: &[u8]) -> String {
    String::from_utf8_lossy(bytes).into_owned()
}

/// Runs the binary in `cwd` with NAATRE_URL/NAATRE_TOKEN set.
struct Naatre {
    bin: PathBuf,
    url: String,
    token: String,
}

impl Naatre {
    fn run(&self, cwd: &Path, args: &[&str]) -> std::process::Output {
        Command::new(&self.bin)
            .args(args)
            .current_dir(cwd)
            .env("NAATRE_URL", &self.url)
            .env("NAATRE_TOKEN", &self.token)
            .env_remove("NAATRE_AUTHOR")
            .output()
            .expect("spawn naatre")
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

/// A port that is free right now (bind-and-drop; the standard test shape).
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
}

impl Serve {
    fn start(bin: &Path, data: &Path, log: &Path) -> Serve {
        let port = free_port();
        let child = Command::new(bin)
            .args([
                "serve",
                "--data",
                data.to_str().unwrap(),
                "--listen",
                &format!("127.0.0.1:{port}"),
            ])
            .stdout(Stdio::from(std::fs::File::create(log).expect("serve log")))
            .stderr(Stdio::null())
            .spawn()
            .expect("spawn serve");
        let serve = Serve { child, port };
        serve.wait_ready();
        serve
    }

    /// Waits until the anon liveness probe answers (the coldboot shape:
    /// TCP-first, then the real HTTP read).
    fn wait_ready(&self) {
        let deadline = Instant::now() + Duration::from_secs(20);
        loop {
            {
                // TCP probe first: fail fast when serve died at startup.
                if let Ok(mut s) = std::net::TcpStream::connect(("127.0.0.1", self.port)) {
                    let _ = s.write_all(b"GET /healthz HTTP/1.0\r\n\r\n");
                    let _ = s.flush();
                    let mut buf = [0u8; 128];
                    let _ = s.read(&mut buf);
                    if buf.starts_with(b"HTTP/1.1 200") || buf.starts_with(b"HTTP/1.0 200") {
                        return;
                    }
                }
            }
            assert!(
                Instant::now() < deadline,
                "serve never became healthy on 127.0.0.1:{}",
                self.port
            );
            std::thread::sleep(Duration::from_millis(100));
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

#[test]
fn m12_conflicted_land_sync_resolve_and_gc_blame_e2e() {
    // The seed needs git (the import seeding path).
    Command::new("git")
        .args(["--version"])
        .output()
        .expect("git is on PATH for the import seed");

    let bin = PathBuf::from(env!("CARGO_BIN_EXE_naatre"));
    let root = tempfile::tempdir().expect("tempdir");
    let root = root.path();
    let data = root.join("data");
    let naatre = |token: &str, port: u16| Naatre {
        bin: bin.clone(),
        url: format!("http://127.0.0.1:{port}"),
        token: token.to_string(),
    };

    // ---- init + serve (a 1-second GC grace, the gc task off — the GC
    // ---- leg drives `naatre gc` by hand under the flock).
    step!("init + serve (gc_grace_secs = 1)");
    let out = Command::new(&bin)
        .args(["init", "--data", data.to_str().unwrap()])
        .output()
        .expect("init");
    assert!(out.status.success(), "init: {}", string_of(&out.stderr));
    let admin = string_of(&out.stdout)
        .lines()
        .find_map(|l| l.strip_prefix("admin token: "))
        .expect("init printed the admin token")
        .to_string();
    {
        // The operator file is 0600 (serve refuses anything else).
        use std::io::Write as _;
        #[cfg(unix)]
        let mut f = {
            use std::os::unix::fs::OpenOptionsExt as _;
            std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(data.join("backend.toml"))
                .expect("write backend.toml")
        };
        #[cfg(not(unix))]
        let mut f = std::fs::File::create(data.join("backend.toml")).expect("write backend.toml");
        write!(
            f,
            "[backend]\nmeta = \"sqlite\"\nblob = \"fs\"\ngc_interval_secs = 0\ngc_grace_secs = 1\n"
        )
        .expect("write backend.toml");
    }
    let mut serve = Serve::start(&bin, &data, &root.join("serve.log"));
    let mut cli = naatre(&admin, serve.port);

    // ---- seed main via import (the product seeding path; serve stops
    // ---- for the flock).
    step!("repo create + import seed");
    cli.run_ok(root, &["repo", "create", "conflicte2e"]);

    // A writer principal for the agents.
    let writer = {
        let out = cli.run(root, &["token", "create", "agent", "--role", "writer"]);
        assert!(
            out.status.success(),
            "token create: {}",
            string_of(&out.stderr)
        );
        string_of(&out.stdout)
            .lines()
            .find_map(|l| l.strip_prefix("token: "))
            .expect("token printed")
            .to_string()
    };
    cli.token = writer;
    serve.stop();
    let seed_git = root.join("seed-git");
    let git = |args: &[&str]| {
        let out = Command::new("git").args(args).output().expect("git run");
        assert!(
            out.status.success(),
            "git {args:?}: {}",
            string_of(&out.stderr)
        );
    };
    let out = Command::new("git")
        .args(["-c", "init.defaultBranch=main", "init", "-q"])
        .arg(&seed_git)
        .output()
        .expect("git init");
    assert!(out.status.success(), "git init: {}", string_of(&out.stderr));
    std::fs::write(seed_git.join("f.txt"), b"base-1\nbase-2\n").expect("seed f.txt");
    std::fs::write(seed_git.join("keep.txt"), b"keep\n").expect("seed keep.txt");
    git(&["-C", seed_git.to_str().unwrap(), "add", "."]);
    git(&[
        "-C",
        seed_git.to_str().unwrap(),
        "-c",
        "user.email=seed@example.com",
        "-c",
        "user.name=seed",
        "commit",
        "-q",
        "-m",
        "seed",
    ]);
    cli.run_ok(
        root,
        &[
            "import",
            "--data",
            data.to_str().unwrap(),
            "--into",
            "conflicte2e",
            seed_git.to_str().unwrap(),
        ],
    );
    let mut serve = Serve::start(&bin, &data, &root.join("serve2.log"));
    cli.url = format!("http://127.0.0.1:{}", serve.port);

    // ---- three agents clone the seed.
    step!("three agents clone");
    let wc_a = root.join("wc_a");
    let wc_b = root.join("wc_b");
    let wc_c = root.join("wc_c");
    for wc in [&wc_a, &wc_b, &wc_c] {
        let out = cli.run(
            root,
            &[
                "clone",
                &format!("{}/conflicte2e", cli.url),
                "--into",
                wc.to_str().unwrap(),
            ],
        );
        assert!(out.status.success(), "clone: {}", string_of(&out.stderr));
        assert!(
            !string_of(&out.stdout).contains("C "),
            "the seed is not conflicted: {}",
            string_of(&out.stdout)
        );
    }

    // ---- agent A: a clean edit lands (default land, no flag).
    step!("agent A lands cleanly");
    std::fs::write(wc_a.join("f.txt"), b"alpha-1\nbase-2\n").expect("A edits");
    let out = cli.run(&wc_a, &["commit", "-m", "alpha edit"]);
    assert!(out.status.success(), "A commit: {}", string_of(&out.stderr));
    let out = cli.run(&wc_a, &["land", "--key", "ka"]);
    assert!(out.status.success(), "A land: {}", string_of(&out.stderr));
    let a_out = string_of(&out.stdout);
    assert!(
        !a_out.contains("conflicted:"),
        "A's land is not conflicted: {a_out}"
    );

    // ---- agent B: the conflicting edit — the DEFAULT land 409s with the
    // ---- recipe (exit 2, the M4 contract unchanged by M12).
    step!("agent B hits the default 409 + recipe");
    std::fs::write(wc_b.join("f.txt"), b"beta-1\nbase-2\n").expect("B edits");
    cli.run_ok(&wc_b, &["commit", "-m", "beta edit"]);
    let out = cli.run(&wc_b, &["land", "--key", "kb"]);
    assert_eq!(out.status.code(), Some(2), "the default land 409s (exit 2)");
    let b_err = string_of(&out.stderr);
    assert!(b_err.contains("ref_conflict"), "{b_err}");
    assert!(
        b_err.contains("--record-conflict"),
        "the recipe names the CLI flag: {b_err}"
    );
    assert!(
        b_err.contains("record_conflict"),
        "the recipe names the API field for non-CLI clients: {b_err}"
    );

    // ---- agent B opts in: the conflicted land settles ONCE.
    step!("agent B lands --record-conflict");
    let out = cli.run(&wc_b, &["land", "--key", "kb", "--record-conflict"]);
    assert!(
        out.status.success(),
        "the opt-in land settles: {}{}",
        string_of(&out.stdout),
        string_of(&out.stderr)
    );
    let b_out = string_of(&out.stdout);
    assert!(b_out.contains("conflicted: f.txt"), "{b_out}");
    let conflicted_node = b_out
        .split(['"', ':'])
        .find(|s| s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()))
        .expect("the land printed the landed 64-hex id")
        .to_string();
    assert_ne!(conflicted_node, "", "conflicted node id parsed");

    // ---- the second client syncs the conflicted main: markers
    // ---- materialize, status shows the conflicted path.
    step!("agent C syncs the conflicted main");
    let out = cli.run(&wc_c, &["sync"]);
    assert!(out.status.success(), "C sync: {}", string_of(&out.stderr));
    let c_out = string_of(&out.stdout);
    assert!(
        c_out.contains("C f.txt"),
        "sync names the conflict: {c_out}"
    );
    let disk = std::fs::read_to_string(wc_c.join("f.txt")).expect("marker file on disk");
    assert!(
        disk.starts_with("<<<<<<< naatre side 1/2: "),
        "the marker rendering materialized:\n{disk}"
    );
    assert!(disk.contains(">>>>>>> naatre conflict: f.txt"), "{disk}");
    let status = cli.run_ok(&wc_c, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status.trim()).expect("status JSON");
    assert_eq!(v["status"], json_str("conflicted"), "{v}");
    assert_eq!(v["conflicted"], serde_json::json!(["f.txt"]), "{v}");
    let human = cli.run_ok(&wc_c, &["status"]);
    assert!(human.contains("C f.txt"), "{human}");
    // The conflicted working copy is preserved: a sync of OTHER content
    // still refuses to clobber a manual resolution (the dirty gate) — the
    // disk still carries markers, matching the index (clean).
    let human = cli.run_ok(&wc_b, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(human.trim()).expect("B status JSON");
    assert_eq!(
        v["conflicted"],
        serde_json::json!(["f.txt"]),
        "B marks too: {v}"
    );

    // ---- a FRESH clone of the conflicted main materializes the same
    // ---- markers (clone surface of item 2).
    step!("a fresh clone materializes the markers");
    let wc_d = root.join("wc_d");
    let out = cli.run(
        root,
        &[
            "clone",
            &format!("{}/conflicte2e", cli.url),
            "--into",
            wc_d.to_str().unwrap(),
        ],
    );
    assert!(out.status.success(), "clone: {}", string_of(&out.stderr));
    assert!(
        string_of(&out.stdout).contains("C f.txt"),
        "clone names the conflict: {}",
        string_of(&out.stdout)
    );
    let disk_d = std::fs::read_to_string(wc_d.join("f.txt")).expect("clone marker file");
    assert_eq!(disk_d, disk, "clone and sync render identical markers");
    let status_d = cli.run_ok(&wc_d, &["status"]);
    assert!(status_d.contains("C f.txt"), "{status_d}");

    // ---- sparse widening onto the conflicted path: `sparse add` runs the
    // ---- same marker detection clone/sync run, so the widened path is
    // ---- marked (status `C`) and `resolve` accepts it.
    step!("sparse add records the conflicted path");
    let wc_e = root.join("wc_e");
    let out = cli.run(
        root,
        &[
            "clone",
            &format!("{}/conflicte2e", cli.url),
            "--sparse",
            "keep.txt",
            "--into",
            wc_e.to_str().unwrap(),
        ],
    );
    assert!(
        out.status.success(),
        "sparse clone: {}",
        string_of(&out.stderr)
    );
    assert!(
        !wc_e.join("f.txt").exists(),
        "the conflicted path is out of cone before the widen"
    );
    let status_e = cli.run_ok(&wc_e, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status_e.trim()).expect("status JSON");
    assert_eq!(
        v["conflicted"],
        serde_json::json!([]),
        "an out-of-cone conflicted path is not marked before it materializes: {v}"
    );
    let out = cli.run(&wc_e, &["sparse", "add", "f.txt"]);
    assert!(
        out.status.success(),
        "sparse add: {}",
        string_of(&out.stderr)
    );
    let disk_e = std::fs::read_to_string(wc_e.join("f.txt")).expect("widened marker file");
    assert!(
        disk_e.starts_with("<<<<<<< naatre side 1/2: "),
        "the widen materialized the markers:\n{disk_e}"
    );
    let status_e = cli.run_ok(&wc_e, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status_e.trim()).expect("status JSON");
    assert_eq!(
        v["conflicted"],
        serde_json::json!(["f.txt"]),
        "the widen recorded the conflicted mark: {v}"
    );
    assert!(cli.run_ok(&wc_e, &["status"]).contains("C f.txt"));
    // The widened path is resolvable: edit to the resolved content and
    // resolve against the conflicted head.
    std::fs::write(wc_e.join("f.txt"), b"resolved-e\nbase-2\n").expect("wc_e resolves");
    let out = cli.run(&wc_e, &["resolve", "f.txt"]);
    assert!(
        out.status.success(),
        "resolve accepts the widened path: {}{}",
        string_of(&out.stdout),
        string_of(&out.stderr)
    );
    let status_e = cli.run_ok(&wc_e, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status_e.trim()).expect("status JSON");
    assert_eq!(
        v["conflicted"],
        serde_json::json!([]),
        "the mark cleared: {v}"
    );

    // ---- the commit gate: a marker file refuses with
    // ---- `conflict_markers_present`; resolved content commits.
    step!("the marker gate + resolution");
    std::fs::copy(wc_c.join("f.txt"), wc_c.join("copied-marker.txt")).expect("copy marker");
    let out = cli.run(&wc_c, &["commit", "-m", "try to commit markers"]);
    assert_eq!(
        out.status.code(),
        Some(2),
        "the marker gate refuses (exit 2)"
    );
    let gate_err = string_of(&out.stderr);
    assert!(gate_err.contains("conflict_markers_present"), "{gate_err}");
    assert!(
        gate_err.contains("naatre resolve"),
        "the error points at resolve: {gate_err}"
    );
    std::fs::remove_file(wc_c.join("copied-marker.txt")).expect("remove the stray copy");

    // ---- agent C resolves: the explicit resolution changeset referencing
    // ---- the conflicted head, then land → linear history.
    std::fs::write(wc_c.join("f.txt"), b"resolved-1\nbase-2\n").expect("C resolves");
    let out = cli.run(&wc_c, &["resolve", "f.txt"]);
    assert!(out.status.success(), "resolve: {}", string_of(&out.stderr));
    let r_out = string_of(&out.stdout);
    let v: serde_json::Value = serde_json::from_str(r_out.trim()).expect("resolve JSON");
    assert_eq!(v["resolved"], json_str("f.txt"), "{v}");
    let resolution = v["id"].as_str().expect("resolution id").to_string();
    let status = cli.run_ok(&wc_c, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status.trim()).expect("status JSON");
    assert_eq!(v["status"], json_str("clean"), "the conflict cleared: {v}");
    assert_eq!(v["conflicted"], serde_json::json!([]), "{v}");
    // ---- land the resolution: main advances past the conflict.
    step!("land the resolution (linear history)");
    let out = cli.run(&wc_c, &["land"]);
    assert!(
        out.status.success(),
        "resolution land: {}",
        string_of(&out.stderr)
    );
    let r_out = string_of(&out.stdout);
    assert!(
        !r_out.contains("conflicted:"),
        "the resolution settlement is done, not conflicted: {r_out}"
    );
    // Linear: main's f.txt is the resolved content; the conflicted node
    // is still readable AT its own id (history kept, markers intact).
    let cat = cli.run_ok(&wc_c, &["cat", "f.txt", "--at", "main"]);
    assert_eq!(cat.trim(), "resolved-1\nbase-2\n".trim_end(), "{cat}");
    let cat = cli.run_ok(&wc_c, &["cat", "f.txt", "--at", &conflicted_node]);
    assert!(
        cat.starts_with("<<<<<<< naatre side 1/2: "),
        "the conflicted node still serves its markers: {cat}"
    );
    assert_ne!(
        resolution, conflicted_node,
        "the resolution is its own changeset"
    );

    // A PLAIN commit of resolved-world content is clean now (the gate
    // bites only marker files).
    std::fs::write(wc_c.join("g.txt"), b"after resolve\n").expect("post-resolve edit");
    let out = cli.run(&wc_c, &["commit", "-m", "after resolve"]);
    assert!(
        out.status.success(),
        "post-resolve commit: {}",
        string_of(&out.stderr)
    );
    let out = cli.run(&wc_c, &["land"]);
    assert!(
        out.status.success(),
        "second land: {}",
        string_of(&out.stderr)
    );
    let cat = cli.run_ok(&wc_c, &["cat", "f.txt", "--at", "main"]);
    assert_eq!(cat.trim(), "resolved-1\nbase-2\n".trim_end(), "{cat}");

    // ---- blame after the conflicted land: the conflicted region reports
    // ---- as conflict, not a phantom author. (The server answers 503
    // ---- `derivation_pending` + Retry-After while the index computes —
    // ---- the pinned retry shape, honored here.)
    step!("blame the conflicted region");
    let blame_at = |at: &str| -> String {
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            let out = cli.run(&wc_c, &["blame", "f.txt", "--at", at]);
            let text = string_of(&out.stdout) + &string_of(&out.stderr);
            if out.status.success() {
                return text;
            }
            assert!(
                text.contains("derivation_pending") && Instant::now() < deadline,
                "blame at {at} never derived: {text}"
            );
            std::thread::sleep(Duration::from_millis(500));
        }
    };
    let blame = blame_at(&conflicted_node);
    assert!(
        blame.contains("(conflict)"),
        "the conflicted region names the conflict:\n{blame}"
    );
    let blame = blame_at("main");
    assert!(
        blame.contains("(conflict)"),
        "blame at the new main still names the conflicted introducer:\n{blame}"
    );

    // ---- GC + fsck after the conflicted land (the flock: serve stops).
    step!("GC passes + fsck after the conflicted land");
    serve.stop();
    // Pass 1: ledger + condemn (b1, agent B's refused land's original
    // commit, is the garbage; its beta blob is a rep side — the walker's
    // business).
    let out = cli.run(root, &["gc", "--data", data.to_str().unwrap()]);
    assert!(
        out.status.success(),
        "gc pass 1: {}",
        string_of(&out.stderr)
    );
    std::thread::sleep(Duration::from_millis(1500));
    let out = cli.run(root, &["gc", "--data", data.to_str().unwrap()]);
    assert!(
        out.status.success(),
        "gc pass 2: {}",
        string_of(&out.stderr)
    );
    let out = cli.run(root, &["fsck", "--data", data.to_str().unwrap()]);
    assert!(
        out.status.success(),
        "fsck clean: {} {}",
        string_of(&out.stdout),
        string_of(&out.stderr)
    );
    // The history survived collection: the conflicted node still serves
    // its markers and main still serves the resolution.
    let mut serve = Serve::start(&bin, &data, &root.join("serve3.log"));
    cli.url = format!("http://127.0.0.1:{}", serve.port);
    let cat = cli.run_ok(&wc_c, &["cat", "f.txt", "--at", &conflicted_node]);
    assert!(
        cat.starts_with("<<<<<<< naatre side 1/2: "),
        "the conflicted node's markers survive GC: {cat}"
    );
    let blame = cli.run_ok(&wc_c, &["blame", "f.txt", "--at", &conflicted_node]);
    assert!(blame.contains("(conflict)"), "blame after GC:\n{blame}");
    serve.stop();
}

fn json_str(s: &str) -> serde_json::Value {
    serde_json::Value::String(s.to_string())
}
