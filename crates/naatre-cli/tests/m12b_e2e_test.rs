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
    /// Runs the binary once; when it fails with `writer_busy` — the server's
    /// documented backpressure while its single write slot is held (a
    /// background derive pass under CPU load), after the CLI's own short
    /// retry budget ran out and the write was NOT applied — the identical
    /// command is run again, as the CLI's message instructs. Every retry is
    /// logged on stderr (`RETRY writer_busy …`), so the pressure stays
    /// visible in `--nocapture` runs; `NAATRE_TEST_NO_BUSY_RETRY=1` turns the
    /// retry off (the control run). At most 8 re-runs, so a command that
    /// stays busy fails within about a minute. Every other outcome
    /// (including every 409/422 a test asserts) returns as-is. The plan's
    /// F4 slice measures whether this retry can go.
    fn run(&self, cwd: &Path, args: &[&str]) -> std::process::Output {
        let mut out = self.run_once(cwd, args);
        if std::env::var("NAATRE_TEST_NO_BUSY_RETRY").as_deref() == Ok("1") {
            return out;
        }
        for attempt in 1..=8 {
            if out.status.success() || !string_of(&out.stderr).contains("writer_busy") {
                break;
            }
            eprintln!(
                "RETRY writer_busy attempt {attempt} for `naatre {}`",
                args.join(" ")
            );
            std::thread::sleep(Duration::from_millis(250));
            out = self.run_once(cwd, args);
        }
        out
    }

    fn run_once(&self, cwd: &Path, args: &[&str]) -> std::process::Output {
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

    // ---- agent A: a clean edit lands (default land, no flag). A changes
    // TWO paths — the conflicted one AND an unrelated one — so the
    // settlement B later records rebases over a main that moved keep.txt
    // as well (the advisor's pin shape: the lander's disk must match a
    // fresh sync of the new head at EVERY landed path, not only the
    // markers).
    step!("agent A lands cleanly");
    std::fs::write(wc_a.join("f.txt"), b"alpha-1\nbase-2\n").expect("A edits");
    std::fs::write(wc_a.join("keep.txt"), b"alpha-keep\n").expect("A edits keep.txt");
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
    // The lander materialized the settlement (senior-qa C1): agent B's
    // disk advanced with head — the same marker bytes every other client
    // sees after syncing — and status marks the path. (The older
    // half-state — head on the settlement, disk still on the pre-land
    // content — is gone.) Resolve works locally for the lander too.
    let b_disk = std::fs::read_to_string(wc_b.join("f.txt")).expect("B's materialized disk");
    assert!(
        b_disk.starts_with("<<<<<<< naatre side 1/2: "),
        "the lander's disk holds the settlement's markers:\n{b_disk}"
    );
    // The rebase carried main's OTHER change too: the lander's disk
    // matches what a fresh sync of the new head writes — main's keep.txt
    // present, markers at the conflicted path, nothing stale.
    let b_keep = std::fs::read_to_string(wc_b.join("keep.txt")).expect("B's keep.txt");
    assert_eq!(
        b_keep, "alpha-keep\n",
        "the lander's disk carries main's other change: {b_keep:?}"
    );
    let human = cli.run_ok(&wc_b, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(human.trim()).expect("B status JSON");
    assert_eq!(
        v["conflicted"],
        serde_json::json!(["f.txt"]),
        "B marks too: {v}"
    );
    std::fs::write(wc_b.join("f.txt"), b"b-resolved\nbase-2\n").expect("B resolves");
    let out = cli.run(&wc_b, &["resolve", "f.txt"]);
    assert!(
        out.status.success(),
        "the lander resolves locally: {}{}",
        string_of(&out.stdout),
        string_of(&out.stderr)
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

/// The shared boot: init the data dir (a 1-second GC grace, the gc task
/// off), write backend.toml 0600, start serve. Returns the running
/// server and the admin token.
fn boot(bin: &Path, root: &Path, log_name: &str) -> (Serve, String) {
    let data = root.join("data");
    let out = Command::new(bin)
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
    let serve = Serve::start(bin, &data, &root.join(log_name));
    (serve, admin)
}

/// m12b-fix item 2 + item 10's repro pin: the state mark is a status
/// index, not a permission — resolve accepts a marker-shaped file with
/// OR without a mark (the unmarked case simulates a legacy/rolled-back
/// state file); restore over a conflicted snapshot RECORDS the marks;
/// an API-stored marker-shaped blob (the server has no create-side
/// marker gate — the recorded follow-up) surfaces as a conflicted mark
/// on clone, pinned here as that follow-up's repro.
#[test]
fn resolve_unmarked_markers_restore_records_marks_and_misclassification() {
    let bin = PathBuf::from(env!("CARGO_BIN_EXE_naatre"));
    let root = tempfile::tempdir().expect("tempdir");
    let root = root.path();
    let data = root.join("data");
    let (mut serve, admin) = boot(&bin, root, "serve-fix.log");
    let mut cli = Naatre {
        bin: bin.clone(),
        url: format!("http://127.0.0.1:{}", serve.port),
        token: admin,
    };

    // Repo + seed (the import path; serve stops for the flock).
    cli.run_ok(root, &["repo", "create", "fixe2e"]);
    serve.stop();
    let seed_git = root.join("seed-git");
    let out = Command::new("git")
        .args(["-c", "init.defaultBranch=main", "init", "-q"])
        .arg(&seed_git)
        .output()
        .expect("git init");
    assert!(out.status.success(), "git init: {}", string_of(&out.stderr));
    std::fs::write(seed_git.join("f.txt"), b"base-1\nbase-2\n").unwrap();
    std::fs::write(seed_git.join("keep.txt"), b"keep\n").unwrap();
    let git = |args: &[&str]| {
        let out = Command::new("git").args(args).output().expect("git run");
        assert!(
            out.status.success(),
            "git {args:?}: {}",
            string_of(&out.stderr)
        );
    };
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
            "fixe2e",
            seed_git.to_str().unwrap(),
        ],
    );
    let mut serve = Serve::start(&bin, &data, &root.join("serve-fix2.log"));
    cli.url = format!("http://127.0.0.1:{}", serve.port);

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

    // Agents: A lands cleanly; B lands --record-conflict.
    for wc in ["wc_a", "wc_b", "wc_g"] {
        let out = cli.run(
            root,
            &[
                "clone",
                &format!("{}/fixe2e", cli.url),
                "--into",
                root.join(wc).to_str().unwrap(),
            ],
        );
        assert!(
            out.status.success(),
            "clone {wc}: {}",
            string_of(&out.stderr)
        );
    }
    let wc_a = root.join("wc_a");
    let wc_b = root.join("wc_b");
    let wc_g = root.join("wc_g");
    std::fs::write(wc_a.join("f.txt"), b"alpha-1\nbase-2\n").unwrap();
    cli.run_ok(&wc_a, &["commit", "-m", "alpha"]);
    cli.run_ok(&wc_a, &["land", "--key", "ka"]);
    std::fs::write(wc_b.join("f.txt"), b"beta-1\nbase-2\n").unwrap();
    cli.run_ok(&wc_b, &["commit", "-m", "beta"]);
    let out = cli.run(&wc_b, &["land", "--key", "kb", "--record-conflict"]);
    assert!(
        out.status.success(),
        "record land: {}",
        string_of(&out.stderr)
    );
    let conflicted_node = string_of(&out.stdout)
        .split(['"', ':'])
        .find(|s| s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()))
        .expect("the land printed the settled id")
        .to_string();

    // The repro (senior-qa A6): an API-stored MARKER-SHAPED blob as an
    // ordinary commit — the server has no create-side marker gate (the
    // recorded follow-up) — surfaces on clone as a conflicted mark.
    {
        let client = naatre_cli::client::Client::new(&cli.url, Some(cli.token.clone()));
        let marker = format!(
            "<<<<<<< naatre side 1/2: {}\nside one\n======= naatre side 2/2: {}\n\
             side two\n>>>>>>> naatre conflict: fake.txt\n",
            "1".repeat(64),
            "2".repeat(64)
        );
        let blob = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, marker.as_bytes());
        client
            .put_bytes(
                &format!("/v1/repos/fixe2e/objects/{}", blob.to_hex()),
                marker.as_bytes(),
            )
            .expect("marker blob PUT");
        let (_st, created) = client
            .post_json(
                "/v1/repos/fixe2e/changesets",
                &serde_json::json!({
                    "parents": [conflicted_node],
                    "author": "api <api@naatre.dev>",
                    "message": "api-stored markers",
                    "changes": [{"path": "fake.txt", "op": "add",
                                 "blob": blob.to_hex(), "size": marker.len(), "exec": false}],
                }),
            )
            .expect("api changeset create");
        assert!(created["id"].is_string(), "{created}");
        // Land it: main moves onto the API-stored node (fast-forward),
        // so a client's sync materializes the marker-shaped blob.
        let (_st, landed) = client
            .post_json(
                "/v1/repos/fixe2e/land",
                &serde_json::json!({ "head": created["id"], "key": "k-api" }),
            )
            .expect("api land");
        assert_eq!(landed["main"], landed["landed"], "main moved: {landed}");
    }
    cli.run_ok(&wc_g, &["sync"]);
    let status_g = cli.run_ok(&wc_g, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status_g.trim()).unwrap();
    let conflicted = v["conflicted"].as_array().unwrap();
    assert!(
        conflicted.contains(&serde_json::json!("fake.txt")),
        "the sync marks the API-stored marker-shaped blob (the follow-up's repro): {v}"
    );

    // The dirty-gate interaction (advisor's pin, the "blocks or is
    // preserved" leg): a lander with an uncommitted edit on a path its
    // own delta touches (f.txt, edited again after the commit) AND on a
    // path MAIN changes underneath it (keep.txt, which H lands). The
    // plain (non-conflicted) rebased land settles server-side; the
    // working-copy update is refused by the same dirty rules a sync
    // uses, everything preserved, the land degrades to a warning.
    {
        let wc_g = root.join("wc_g2");
        let wc_h = root.join("wc_h");
        for wc in [&wc_g, &wc_h] {
            let out = cli.run(
                root,
                &[
                    "clone",
                    &format!("{}/fixe2e", cli.url),
                    "--into",
                    wc.to_str().unwrap(),
                ],
            );
            assert!(out.status.success(), "clone: {}", string_of(&out.stderr));
        }
        std::fs::write(wc_g.join("f.txt"), b"g-side\nbase-2\n").unwrap();
        cli.run_ok(&wc_g, &["commit", "-m", "g edit"]);
        // AFTER the commit: an uncommitted edit on the delta's own path,
        // and one on a path main will change under the lander.
        // The pre-land head (the commit's parent), for the state-head
        // pin below.
        let g_head = {
            let state_g: serde_json::Value = serde_json::from_str(
                &std::fs::read_to_string(wc_g.join(".naatre/state.json")).unwrap(),
            )
            .unwrap();
            state_g["head"].as_str().unwrap().to_string()
        };
        std::fs::write(wc_g.join("f.txt"), b"h-side2\nbase-2\n").unwrap();
        std::fs::write(wc_g.join("keep.txt"), b"g-keep\n").unwrap();
        // H lands main's keep.txt change (a clean rebased land — the
        // normal-land shape goes through the same materialization).
        std::fs::write(wc_h.join("keep.txt"), b"h-keep\n").unwrap();
        cli.run_ok(&wc_h, &["commit", "-m", "h keep"]);
        cli.run_ok(&wc_h, &["land", "--key", "kh"]);
        assert_eq!(
            std::fs::read_to_string(wc_h.join("keep.txt")).unwrap(),
            "h-keep\n",
            "the clean rebased lander carries main's change"
        );
        // G's land: settles (its delta rebases cleanly), then the
        // working-copy update is refused by the dirty rules.
        let out = cli.run(&wc_g, &["land", "--key", "kg"]);
        let text = string_of(&out.stdout) + "\n" + &string_of(&out.stderr);
        assert!(
            out.status.success(),
            "the land itself settles (exit 0): {text}"
        );
        assert!(
            text.contains("refused by the dirty rules"),
            "the preservation warning fires: {text}"
        );
        assert!(
            !text.contains("do NOT re-land"),
            "a clean settlement's retry is a harmless no-op — no do-not-re-land note: {text}"
        );
        let kg_landed = string_of(&out.stdout)
            .split(['"', ':'])
            .find(|s| s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()))
            .expect("the land printed the settled id")
            .to_string();
        assert_eq!(
            std::fs::read_to_string(wc_g.join("f.txt")).unwrap(),
            "h-side2\nbase-2\n",
            "the uncommitted edit on the delta path is preserved"
        );
        assert_eq!(
            std::fs::read_to_string(wc_g.join("keep.txt")).unwrap(),
            "g-keep\n",
            "the uncommitted edit on main's changed path is preserved"
        );
        let status_g = cli.run_ok(&wc_g, &["status", "--json"]);
        let v: serde_json::Value = serde_json::from_str(status_g.trim()).unwrap();
        assert_eq!(
            v["modified"],
            serde_json::json!(["f.txt", "keep.txt"]),
            "both preserved edits show as modified: {v}"
        );
        // The blocked land reaches the fallback mark arm (no diff
        // applied): G's older mark survives while its disk bytes are
        // still marker-shaped; the resolved f.txt mark is gone.
        assert_eq!(
            v["conflicted"],
            serde_json::json!(["fake.txt"]),
            "the fallback keeps the still-true older mark: {v}"
        );
        // m12b-fix round 2 (advisor option a): the blocked land leaves
        // state.head on the OLD head — the disk and index truthfully
        // materialize that tree, so the promised sync recovery actually
        // sees a difference (sync short-circuits when target == head).
        let state_g: serde_json::Value = serde_json::from_str(
            &std::fs::read_to_string(wc_g.join(".naatre/state.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            state_g["head"],
            json_str(&g_head),
            "state stays on the old head after a blocked land"
        );
        // GAP 2 (m12b-fix round 2): a rerun land right after a blocked
        // land is a clean no-op — head still names the just-landed
        // changeset, the server re-derives its (already-contained)
        // delta, and settles done on the UNCHANGED main: no duplicate
        // node (landed == the first settlement), no self-conflict
        // (rebased == 0).
        let out = cli.run(&wc_g, &["land", "--key", "kg-retry"]);
        let text = string_of(&out.stdout) + "\n" + &string_of(&out.stderr);
        assert!(
            out.status.success(),
            "the retry land is a clean no-op (exit 0): {text}"
        );
        assert!(
            text.contains(&format!("\"landed\":\"{kg_landed}\"")),
            "the retry settles on the SAME main tip — no duplicate node: {text}"
        );
        assert!(
            text.contains("\"rebased\":0"),
            "the retry applies no delta (no self-conflict): {text}"
        );
        // The warning's PLAIN recovery path (item 3): clear the
        // colliding edit (keep.txt back to the old-head content) while
        // the non-colliding f.txt edit stays — a PLAIN `naatre sync`
        // then runs the recovery: it reaches the landed head and
        // preserves the non-colliding local edit.
        std::fs::write(wc_g.join("keep.txt"), b"keep\n").unwrap();
        let out = cli.run(&wc_g, &["sync"]);
        assert!(
            out.status.success(),
            "plain recovery sync: {}",
            string_of(&out.stderr)
        );
        assert_eq!(
            std::fs::read_to_string(wc_g.join("keep.txt")).unwrap(),
            "h-keep\n",
            "the plain sync reached the landed head at the cleared path"
        );
        assert_eq!(
            std::fs::read_to_string(wc_g.join("f.txt")).unwrap(),
            "h-side2\nbase-2\n",
            "the non-colliding local edit is preserved by the plain sync"
        );
        let state_g: serde_json::Value = serde_json::from_str(
            &std::fs::read_to_string(wc_g.join(".naatre/state.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            state_g["head"],
            json_str(&kg_landed),
            "the plain recovery reached the landed head"
        );
        // The --force recovery (up to date after the plain one — the
        // same machinery, asserted anyway): the disk then matches the
        // settled head at every landed path.
        let out = cli.run(&wc_g, &["sync", "--force"]);
        assert!(
            out.status.success(),
            "recovery sync: {}",
            string_of(&out.stderr)
        );
        assert_eq!(
            std::fs::read_to_string(wc_g.join("keep.txt")).unwrap(),
            "h-keep\n",
            "main's other change materialized"
        );
        // f.txt is NOT in the old→new difference (the settlement's f.txt
        // equals the old head's — G's own delta was already the landed
        // content), so the uncommitted edit on it persists per the dirty
        // rules: the disk matches the new head at every CHANGED path.
        assert_eq!(
            std::fs::read_to_string(wc_g.join("f.txt")).unwrap(),
            "h-side2\nbase-2\n",
            "the preserved edit persists on the unchanged path"
        );
        let state_g: serde_json::Value = serde_json::from_str(
            &std::fs::read_to_string(wc_g.join(".naatre/state.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            state_g["head"],
            json_str(&kg_landed),
            "the recovery sync reached the landed head"
        );
        // The follow-up commit parents against the recovered head, and
        // the follow-up land still works (the advisor's check).
        std::fs::write(wc_g.join("f.txt"), b"g-followup\nbase-2\n").unwrap();
        let out = cli.run(&wc_g, &["commit", "-m", "g follow-up"]);
        assert!(
            out.status.success(),
            "follow-up commit: {}",
            string_of(&out.stderr)
        );
        let followup = string_of(&out.stdout)
            .split(['"', ':'])
            .find(|s| s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()))
            .expect("the commit printed its id")
            .to_string();
        {
            let client = naatre_cli::client::Client::new(&cli.url, Some(cli.token.clone()));
            let cs = client
                .get_json(&format!("/v1/repos/fixe2e/changesets/{followup}"), &[])
                .expect("fetch the follow-up changeset");
            assert_eq!(
                cs["parents"],
                serde_json::json!([kg_landed]),
                "the follow-up commit parents against the recovered head"
            );
        }
        let out = cli.run(&wc_g, &["land", "--key", "kg-followup"]);
        assert!(
            out.status.success(),
            "the follow-up land still works: {}{}",
            string_of(&out.stdout),
            string_of(&out.stderr)
        );
        assert_eq!(
            std::fs::read_to_string(wc_g.join("f.txt")).unwrap(),
            "g-followup\nbase-2\n",
            "the follow-up lander materializes its own settlement"
        );
        let status_g = cli.run_ok(&wc_g, &["status", "--json"]);
        let v: serde_json::Value = serde_json::from_str(status_g.trim()).unwrap();
        assert_eq!(
            v["modified"],
            serde_json::json!([]),
            "the follow-up commit took the whole disk state: {v}"
        );

        // Round 2 — the fallback's RESPONSE-UNION half: a CONFLICTED
        // blocked land. Both agents commit competing f.txt edits off the
        // same base; H lands first (contained — main's f.txt becomes
        // h3); then G, holding an UNCOMMITTED f.txt edit on its own
        // delta's path, record-lands: its rebase now genuinely
        // conflicts (base g2-side vs ours h3-side vs theirs g4-side),
        // the settlement settles CONFLICTED (the response names f.txt),
        // and the gate refuses the update — the marks union the
        // response's f.txt with G's still-true fake.txt.
        std::fs::write(wc_g.join("f.txt"), b"g4-side\nbase-2\n").unwrap();
        cli.run_ok(&wc_g, &["commit", "-m", "g4 edit"]);
        std::fs::write(wc_g.join("f.txt"), b"g5-uncommitted\nbase-2\n").unwrap();
        // H syncs to main first (its head is behind G's settlement), so
        // its h3 commit sits on main's tip and its land is contained —
        // main's f.txt becomes h3-side.
        cli.run_ok(&wc_h, &["sync"]);
        std::fs::write(wc_h.join("f.txt"), b"h3-side\nbase-2\n").unwrap();
        cli.run_ok(&wc_h, &["commit", "-m", "h3 edit"]);
        let out = cli.run(&wc_h, &["land", "--key", "kh3"]);
        assert!(
            out.status.success(),
            "H's contained land: {}",
            string_of(&out.stderr)
        );
        assert_eq!(
            std::fs::read_to_string(wc_h.join("f.txt")).unwrap(),
            "h3-side\nbase-2\n",
            "H's disk carries its own settled content"
        );
        let out = cli.run(&wc_g, &["land", "--key", "kg3", "--record-conflict"]);
        let text = string_of(&out.stdout) + "\n" + &string_of(&out.stderr);
        assert!(out.status.success(), "G's record land settles: {text}");
        assert!(
            text.contains("refused by the dirty rules"),
            "the preservation warning fires again: {text}"
        );
        assert!(
            text.contains("do NOT re-land"),
            "a conflicted settlement's warning says do NOT re-land: {text}"
        );
        let kg3_landed = string_of(&out.stdout)
            .split(['"', ':'])
            .find(|s| s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()))
            .expect("the land printed the settled id")
            .to_string();
        let status_g = cli.run_ok(&wc_g, &["status", "--json"]);
        let v: serde_json::Value = serde_json::from_str(status_g.trim()).unwrap();
        assert_eq!(
            v["conflicted"],
            serde_json::json!(["fake.txt"]),
            "the blocked record land leaves the old marks and head untouched — NO new \
             mark at the conflicted path (that would show C for the user's own edit): {v}"
        );
        assert_eq!(
            v["modified"],
            serde_json::json!(["f.txt"]),
            "the conflicted path shows as the user's EDIT, not C: {v}"
        );
        // The retry pin, conflicted shape (asserted as OBSERVED — this
        // is not the clean no-op): the plain rerun MERGES G's committed
        // delta against the settlement's markers — G's g4-side is one of
        // the marker sides, so the line merge resolves instead of
        // re-raising — and advances main to a NEW node (rebased == 1,
        // landed != kg3_landed: no duplicate). The update blocks on the
        // same uncommitted edit, and the warning says do NOT re-land.
        let out = cli.run(&wc_g, &["land", "--key", "kg3-retry"]);
        let retry_text = string_of(&out.stdout) + "\n" + &string_of(&out.stderr);
        assert!(
            out.status.success(),
            "the rerun settles (exit 0): {retry_text}"
        );
        assert!(
            text.contains("do NOT re-land"),
            "the first warning already said do NOT re-land"
        );
        let retry_landed = string_of(&out.stdout)
            .split(['"', ':'])
            .find(|s| s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()))
            .expect("the retry printed its landed id")
            .to_string();
        assert_ne!(
            retry_landed, kg3_landed,
            "the retry is a NEW node, not a duplicate settlement"
        );
        let retry_status = naatre_cli::client::Client::new(&cli.url, Some(cli.token.clone()))
            .get_json("/v1/repos/fixe2e/land/kg3-retry", &[])
            .expect("the retry settled");
        assert_eq!(retry_status["state"], json_str("done"), "{retry_status}");
        assert_eq!(
            retry_status["result"]["rebased"],
            serde_json::json!(1),
            "{retry_status}"
        );
        // The observed composition, pinned: the retry node is a PLAIN
        // v1 node — the recorded conflict is DOWNGRADED to marker text
        // on main's tip (landing onto a conflicted main via a plain
        // rerun merges the marker renderings into a new marker file —
        // the missing server-side marker-shape check at land, the
        // recorded follow-up in docs/design.md) — and the ORIGINAL BASE
        // is replaced by a mislabeled section, while both sides' content
        // survives in the text.
        let cs_obs = naatre_cli::client::Client::new(&cli.url, Some(cli.token.clone()))
            .get_json(&format!("/v1/repos/fixe2e/changesets/{retry_landed}"), &[])
            .expect("fetch the retry changeset");
        assert_eq!(
            cs_obs["conflicted"],
            serde_json::json!(false),
            "the retry node is plain v1 — the recorded conflict is DOWNGRADED to marker \
             text on main's tip (nothing was resolved)"
        );
        let retry_cat = cli.run(&wc_g, &["cat", "f.txt", "--at", &retry_landed]);
        let retry_content = string_of(&retry_cat.stdout);
        assert!(
            retry_content.starts_with("<<<<<<< naatre side 1/"),
            "the served content is a marker rendering"
        );
        // H's side lives in the SIDE-1 section (scoped like side-2
        // below — an unscoped contains() would be satisfiable by the
        // mislabeled base section).
        let side1 = retry_content
            .split("<<<<<<< naatre side 1/2: ")
            .nth(1)
            .expect("a side-1 divider exists")
            .split("||||||| naatre base:")
            .next()
            .expect("the side-1 block ends at the base divider");
        assert!(
            side1.contains("h3-side"),
            "H's side survives in the side-1 section: {side1:?}"
        );
        // G's side lives in the SIDE-2 section (the base section holds
        // the mislabeled g4-side content too — contains() alone cannot
        // prove the side survived).
        let side2 = retry_content
            .split("======= naatre side 2/2: ")
            .nth(1)
            .expect("a side-2 divider exists")
            .split(">>>>>>>")
            .next()
            .expect("the side-2 block ends at the closer");
        assert!(
            side2.contains("g4-side"),
            "G's side survives in the side-2 section: {side2:?}"
        );
        assert!(
            retry_content.contains("||||||| naatre base: ")
                && retry_content.contains("g4-side\nbase-2"),
            "the base section is replaced by mislabeled side content (the recorded \
             defect): {retry_content}"
        );
        // The retry's own update was refused by the dirty rules (the
        // uncommitted g5 edit sits on its delta's path), and head stays
        // on the old side — the disk holds the user's edit
        // byte-for-byte.
        let retry_disk = std::fs::read_to_string(wc_g.join("f.txt")).unwrap();
        assert_eq!(
            retry_disk, "g5-uncommitted\nbase-2\n",
            "the user's uncommitted edit is preserved byte-for-byte: {retry_disk:?}"
        );
        // The warning's PLAIN recovery path (item 3, the record-land
        // shape): clear the colliding edit (f.txt back to the old-head
        // content g4-side) — a PLAIN `naatre sync` reaches the landed
        // head and marks f.txt by content (the settlement's re-conflict
        // markers).
        std::fs::write(wc_g.join("f.txt"), b"g4-side\nbase-2\n").unwrap();
        let out = cli.run(&wc_g, &["sync"]);
        assert!(
            out.status.success(),
            "round-2 plain recovery sync: {}",
            string_of(&out.stderr)
        );
        assert!(
            std::fs::read_to_string(wc_g.join("f.txt"))
                .unwrap()
                .starts_with("<<<<<<< naatre side 1/"),
            "the plain recovery materialized the re-conflict markers"
        );
        let state_g: serde_json::Value = serde_json::from_str(
            &std::fs::read_to_string(wc_g.join(".naatre/state.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            state_g["head"],
            serde_json::json!(retry_landed.clone()),
            "the plain recovery reached main's tip (the retry's settlement)"
        );
        // The --force recovery (up to date after the plain one — the
        // same machinery, asserted anyway): the marks union the incoming
        // marker detection (f.txt) with the carried still-true fake.txt.
        let out = cli.run(&wc_g, &["sync", "--force"]);
        assert!(
            out.status.success(),
            "round-2 recovery sync: {}",
            string_of(&out.stderr)
        );
        assert!(
            std::fs::read_to_string(wc_g.join("f.txt"))
                .unwrap()
                .starts_with("<<<<<<< naatre side 1/"),
            "the recovery materialized the re-conflict markers"
        );
        let state_g: serde_json::Value = serde_json::from_str(
            &std::fs::read_to_string(wc_g.join(".naatre/state.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            state_g["head"],
            serde_json::json!(retry_landed.clone()),
            "the recovery reached the landed head (the retry's settlement)"
        );
        let status_g = cli.run_ok(&wc_g, &["status", "--json"]);
        let v: serde_json::Value = serde_json::from_str(status_g.trim()).unwrap();
        assert_eq!(
            v["conflicted"],
            serde_json::json!(["f.txt", "fake.txt"]),
            "the recovery unions the conflicted marks: {v}"
        );
    }

    // restore marks: B (the lander, marker bytes on disk) snapshots the
    // conflicted working copy; H resolves its copy (plain commit clears
    // the mark), then RESTORES the conflicted snapshot — the restored
    // marker bytes record the mark again.
    let snap_out = cli.run_ok(&wc_b, &["snapshot"]);
    let snap = snap_out
        .split(['"', ':'])
        .find(|s| s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()))
        .expect("snapshot printed its id")
        .to_string();
    std::fs::write(wc_b.join("f.txt"), b"b-resolved\nbase-2\n").unwrap();
    let out = cli.run(&wc_b, &["resolve", "f.txt"]);
    assert!(
        out.status.success(),
        "B resolve: {}",
        string_of(&out.stderr)
    );
    // B's wc is clean-and-resolved; restore the CONFLICTED snapshot back
    // over it.
    let out = cli.run(&wc_b, &["restore", &snap]);
    assert!(out.status.success(), "restore: {}", string_of(&out.stderr));
    let disk_b = std::fs::read_to_string(wc_b.join("f.txt")).unwrap();
    assert!(disk_b.starts_with("<<<<<<< naatre side 1/2: "), "{disk_b}");
    let status_b = cli.run_ok(&wc_b, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status_b.trim()).unwrap();
    assert_eq!(
        v["conflicted"],
        serde_json::json!(["f.txt"]),
        "restore over a conflicted snapshot records the mark: {v}"
    );

    // resolve-unmarked (the index is not a permission): strip the mark
    // from state.json (a legacy/rolled-back state file shape) — resolve
    // on the marker-shaped bytes still proceeds, says so on stderr, and
    // clears whatever mark there is.
    let state_path = wc_b.join(".naatre/state.json");
    let mut state: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&state_path).unwrap()).unwrap();
    state["conflicted"] = serde_json::json!([]);
    std::fs::write(&state_path, serde_json::to_string_pretty(&state).unwrap()).unwrap();
    let out = cli.run(&wc_b, &["resolve", "f.txt"]);
    let text = string_of(&out.stdout) + "\n" + &string_of(&out.stderr);
    assert!(
        out.status.success(),
        "resolve accepts an unmarked marker-shaped file: {text}"
    );
    assert!(
        text.contains("still carries conflict markers"),
        "the honesty note names the marker content: {text}"
    );
    let status_b = cli.run_ok(&wc_b, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status_b.trim()).unwrap();
    assert_eq!(v["conflicted"], serde_json::json!([]), "{v}");

    serve.stop();
}

/// Boots a fresh data dir, creates `repo`, seeds it with `files` through
/// the import path (serve stops for the flock), restarts serve and mints
/// a writer token. Returns the running server and a CLI handle on it.
fn seeded(
    bin: &Path,
    root: &Path,
    repo: &str,
    files: &[(&str, &[u8])],
    log_prefix: &str,
) -> (Serve, Naatre) {
    let data = root.join("data");
    let (mut serve, admin) = boot(bin, root, &format!("{log_prefix}-1.log"));
    let mut cli = Naatre {
        bin: bin.to_path_buf(),
        url: format!("http://127.0.0.1:{}", serve.port),
        token: admin,
    };
    cli.run_ok(root, &["repo", "create", repo]);
    let writer = string_of(
        &cli.run(root, &["token", "create", "agent", "--role", "writer"])
            .stdout,
    )
    .lines()
    .find_map(|l| l.strip_prefix("token: "))
    .expect("token printed")
    .to_string();
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
    git(&[
        "-c",
        "init.defaultBranch=main",
        "init",
        "-q",
        seed_git.to_str().unwrap(),
    ]);
    for (name, content) in files {
        std::fs::write(seed_git.join(name), content).expect("seed file");
    }
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
            repo,
            seed_git.to_str().unwrap(),
        ],
    );
    let serve = Serve::start(bin, &data, &root.join(format!("{log_prefix}-2.log")));
    cli.url = format!("http://127.0.0.1:{}", serve.port);
    cli.token = writer;
    (serve, cli)
}

/// Clones `repo` into `root/<name>` and returns the path.
fn clone_into(cli: &Naatre, root: &Path, repo: &str, name: &str) -> PathBuf {
    let wc = root.join(name);
    let out = cli.run(
        root,
        &[
            "clone",
            &format!("{}/{repo}", cli.url),
            "--into",
            wc.to_str().unwrap(),
        ],
    );
    assert!(
        out.status.success(),
        "clone {name}: {}",
        string_of(&out.stderr)
    );
    wc
}

/// P4: a modify/delete conflict recorded under `--record-conflict` is
/// detected by `status` and `resolve` on a second client. A modifies
/// `f.txt`, B deletes it and lands with the opt-in; C syncs and sees the
/// deletion-side markers; `resolve` treats the path as conflicted; after C
/// edits the file, `resolve` succeeds and `land` advances main.
#[test]
fn deletion_side_conflict_status_and_resolve() {
    let bin = PathBuf::from(env!("CARGO_BIN_EXE_naatre"));
    let root = tempfile::tempdir().expect("tempdir");
    let root = root.path();
    let (mut serve, cli) = seeded(
        &bin,
        root,
        "delside",
        &[("f.txt", b"base-1\nbase-2\n"), ("keep.txt", b"keep\n")],
        "serve-del",
    );
    let wc_a = clone_into(&cli, root, "delside", "wc_a");
    let wc_b = clone_into(&cli, root, "delside", "wc_b");
    let wc_c = clone_into(&cli, root, "delside", "wc_c");
    let wc_d = clone_into(&cli, root, "delside", "wc_d");

    step!("A modifies f.txt and lands; B deletes it");
    std::fs::write(wc_a.join("f.txt"), b"alpha-1\nbase-2\n").unwrap();
    cli.run_ok(&wc_a, &["commit", "-m", "alpha"]);
    cli.run_ok(&wc_a, &["land", "--key", "ka"]);
    std::fs::remove_file(wc_b.join("f.txt")).unwrap();
    cli.run_ok(&wc_b, &["commit", "-m", "delete f"]);
    let out = cli.run(&wc_b, &["land", "--key", "kb"]);
    assert_eq!(out.status.code(), Some(2), "the default land 409s");
    assert!(string_of(&out.stderr).contains("ref_conflict"));
    step!("B lands --record-conflict (a modify/delete conflict records)");
    let out = cli.run(&wc_b, &["land", "--key", "kb", "--record-conflict"]);
    assert!(
        out.status.success(),
        "the opt-in records a modify/delete conflict: {}{}",
        string_of(&out.stdout),
        string_of(&out.stderr)
    );
    assert!(string_of(&out.stdout).contains("conflicted: f.txt"));

    step!("C syncs: status marks the deletion-side conflict");
    let out = cli.run(&wc_c, &["sync"]);
    assert!(out.status.success(), "C sync: {}", string_of(&out.stderr));
    assert!(string_of(&out.stdout).contains("C f.txt"));
    let human = cli.run_ok(&wc_c, &["status"]);
    assert!(human.contains("C f.txt"), "{human}");
    let disk = std::fs::read_to_string(wc_c.join("f.txt")).unwrap();
    assert!(
        disk.ends_with("======= naatre side 2/2: deleted\n>>>>>>> naatre conflict: f.txt\n"),
        "the deleted side renders last:\n{disk}"
    );
    assert!(disk.starts_with("<<<<<<< naatre side 1/2: "), "{disk}");
    assert!(
        disk.contains("alpha-1\n"),
        "the surviving side is shown:\n{disk}"
    );

    step!("resolve treats the path as conflicted (D clone, unchanged markers)");
    cli.run_ok(&wc_d, &["sync"]);
    let out = cli.run(&wc_d, &["resolve", "f.txt"]);
    let text = string_of(&out.stdout) + &string_of(&out.stderr);
    assert!(
        !text.contains("is not a conflicted path"),
        "the deletion-side markers count as a conflicted path: {text}"
    );
    // `resolve`'s own `marker_shaped` check recognized the deletion-side
    // rendering: the honesty warning names the marker content.
    assert!(
        text.contains("still carries conflict markers"),
        "resolve detects the deletion-side markers: {text}"
    );

    step!("C edits, resolves, lands");
    std::fs::write(wc_c.join("f.txt"), b"resolved-1\nbase-2\n").unwrap();
    let out = cli.run(&wc_c, &["resolve", "f.txt"]);
    assert!(out.status.success(), "resolve: {}", string_of(&out.stderr));
    let out = cli.run(&wc_c, &["land"]);
    assert!(out.status.success(), "land: {}", string_of(&out.stderr));
    let cat = cli.run_ok(&wc_c, &["cat", "f.txt", "--at", "main"]);
    assert_eq!(cat, "resolved-1\nbase-2\n");
    let status = cli.run_ok(&wc_c, &["status", "--json"]);
    let v: serde_json::Value = serde_json::from_str(status.trim()).unwrap();
    assert_eq!(v["conflicted"], serde_json::json!([]), "{v}");
    serve.stop();
}

/// A modify/delete conflict whose surviving side is executable (A: edit and
/// `chmod +x`; B: delete) recorded under `--record-conflict` and read through
/// the real client (the client does not see a chmod-only edit, so the exec
/// bit rides on a content change here): the marker
/// opener carries the ` +x` token and the marker file is executable (the
/// surviving side's bit);
/// `status` marks it, the commit gate refuses a copy of the marker file,
/// and `resolve` recognizes the markers (so `marker_shaped` and the opener
/// screen agree on the token end to end).
#[cfg(unix)]
#[test]
fn exec_side_conflict_markers_status_gate_and_resolve() {
    use std::os::unix::fs::PermissionsExt as _;
    let bin = PathBuf::from(env!("CARGO_BIN_EXE_naatre"));
    let root = tempfile::tempdir().expect("tempdir");
    let root = root.path();
    let (mut serve, cli) = seeded(
        &bin,
        root,
        "execside",
        &[("f.txt", b"base-1\nbase-2\n"), ("keep.txt", b"keep\n")],
        "serve-exec",
    );
    let wc_a = clone_into(&cli, root, "execside", "wc_a");
    let wc_b = clone_into(&cli, root, "execside", "wc_b");
    let wc_c = clone_into(&cli, root, "execside", "wc_c");
    let wc_d = clone_into(&cli, root, "execside", "wc_d");

    step!("A edits f.txt and chmod +x and lands; B deletes it");
    std::fs::write(wc_a.join("f.txt"), b"alpha-1\nbase-2\n").unwrap();
    std::fs::set_permissions(wc_a.join("f.txt"), std::fs::Permissions::from_mode(0o755)).unwrap();
    cli.run_ok(&wc_a, &["commit", "-m", "edit and chmod +x"]);
    cli.run_ok(&wc_a, &["land", "--key", "ka"]);
    std::fs::remove_file(wc_b.join("f.txt")).unwrap();
    cli.run_ok(&wc_b, &["commit", "-m", "delete f"]);
    let out = cli.run(&wc_b, &["land", "--key", "kb", "--record-conflict"]);
    assert!(
        out.status.success(),
        "the opt-in records an exec-only conflict: {}{}",
        string_of(&out.stdout),
        string_of(&out.stderr)
    );
    assert!(string_of(&out.stdout).contains("conflicted: f.txt"));

    step!("C syncs: ` +x` on the opener, an executable marker file");
    let out = cli.run(&wc_c, &["sync"]);
    assert!(out.status.success(), "C sync: {}", string_of(&out.stderr));
    assert!(string_of(&out.stdout).contains("C f.txt"));
    let disk = std::fs::read_to_string(wc_c.join("f.txt")).unwrap();
    let first = disk.lines().next().unwrap();
    assert!(
        first.starts_with("<<<<<<< naatre side 1/2: ") && first.ends_with(" +x"),
        "the executable side shows the exec token: {first}"
    );
    assert!(disk.ends_with("======= naatre side 2/2: deleted\n>>>>>>> naatre conflict: f.txt\n"));
    let mode = std::fs::metadata(wc_c.join("f.txt"))
        .unwrap()
        .permissions()
        .mode();
    assert_ne!(
        mode & 0o111,
        0,
        "the surviving side is +x, so the marker file is executable: {mode:o}"
    );
    assert!(cli.run_ok(&wc_c, &["status"]).contains("C f.txt"));

    step!("the commit gate refuses a copy of the exec-token marker file");
    std::fs::copy(wc_c.join("f.txt"), wc_c.join("copied-marker.txt")).unwrap();
    let out = cli.run(&wc_c, &["commit", "-m", "try to commit markers"]);
    assert_eq!(out.status.code(), Some(2), "the marker gate refuses");
    assert!(string_of(&out.stderr).contains("conflict_markers_present"));
    std::fs::remove_file(wc_c.join("copied-marker.txt")).unwrap();

    step!("resolve recognizes the markers; an edit resolves");
    cli.run_ok(&wc_d, &["sync"]);
    let out = cli.run(&wc_d, &["resolve", "f.txt"]);
    let text = string_of(&out.stdout) + &string_of(&out.stderr);
    assert!(
        text.contains("still carries conflict markers"),
        "resolve detects the exec-token markers: {text}"
    );
    std::fs::write(wc_c.join("f.txt"), b"resolved\n").unwrap();
    // The marker file is executable (the surviving side's bit) and resolve
    // takes the exec bit from disk, so no chmod is needed.
    let out = cli.run(&wc_c, &["resolve", "f.txt"]);
    assert!(out.status.success(), "resolve: {}", string_of(&out.stderr));
    let out = cli.run(&wc_c, &["land"]);
    assert!(out.status.success(), "land: {}", string_of(&out.stderr));
    assert_eq!(
        cli.run_ok(&wc_c, &["cat", "f.txt", "--at", "main"]),
        "resolved\n"
    );
    let wc_e = clone_into(&cli, root, "execside", "wc_e");
    let mode = std::fs::metadata(wc_e.join("f.txt"))
        .unwrap()
        .permissions()
        .mode();
    assert_ne!(
        mode & 0o111,
        0,
        "resolve took the exec bit from disk: main's f.txt is executable ({mode:o})"
    );
    serve.stop();
}

/// F2a review blocker: the marker file takes the exec bit the sides agree
/// on (three-way, as a clean merge would), so a content conflict on a
/// script whose base and both sides are `+x` materializes an EXECUTABLE
/// marker file. `resolve` records the exec bit from disk, so editing the
/// markers out without any `chmod` keeps the script executable on main.
#[cfg(unix)]
#[test]
fn exec_script_content_conflict_resolves_executable() {
    use std::os::unix::fs::PermissionsExt as _;
    let bin = PathBuf::from(env!("CARGO_BIN_EXE_naatre"));
    let root = tempfile::tempdir().expect("tempdir");
    let root = root.path();
    let (mut serve, cli) = seeded(
        &bin,
        root,
        "execscript",
        &[
            ("run.sh", b"#!/bin/sh\nbase-1\nbase-2\n"),
            ("keep.txt", b"keep\n"),
        ],
        "serve-script",
    );
    let exec_of = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o111;

    step!("a setup clone makes the base run.sh executable (edit + chmod +x)");
    let wc_0 = clone_into(&cli, root, "execscript", "wc_0");
    std::fs::write(wc_0.join("run.sh"), b"#!/bin/sh\nbase-1\nbase-2b\n").unwrap();
    std::fs::set_permissions(wc_0.join("run.sh"), std::fs::Permissions::from_mode(0o755)).unwrap();
    cli.run_ok(&wc_0, &["commit", "-m", "script is executable"]);
    cli.run_ok(&wc_0, &["land", "--key", "k0"]);
    let wc_a = clone_into(&cli, root, "execscript", "wc_a");
    let wc_b = clone_into(&cli, root, "execscript", "wc_b");
    let wc_c = clone_into(&cli, root, "execscript", "wc_c");
    assert_ne!(exec_of(&wc_a.join("run.sh")), 0, "the base is executable");

    step!("A and B edit the same line (both stay +x); B lands with the opt-in");
    std::fs::write(wc_a.join("run.sh"), b"#!/bin/sh\nalpha-1\nbase-2b\n").unwrap();
    cli.run_ok(&wc_a, &["commit", "-m", "alpha"]);
    cli.run_ok(&wc_a, &["land", "--key", "ka"]);
    std::fs::write(wc_b.join("run.sh"), b"#!/bin/sh\nbeta-1\nbase-2b\n").unwrap();
    cli.run_ok(&wc_b, &["commit", "-m", "beta"]);
    let out = cli.run(&wc_b, &["land", "--key", "kb", "--record-conflict"]);
    assert!(
        out.status.success(),
        "the opt-in records the content conflict: {}{}",
        string_of(&out.stdout),
        string_of(&out.stderr)
    );
    assert!(string_of(&out.stdout).contains("conflicted: run.sh"));

    step!("C syncs: the marker file is executable");
    let out = cli.run(&wc_c, &["sync"]);
    assert!(out.status.success(), "C sync: {}", string_of(&out.stderr));
    let disk = std::fs::read_to_string(wc_c.join("run.sh")).unwrap();
    assert!(disk.starts_with("<<<<<<< naatre side 1/2: "), "{disk}");
    let mode = exec_of(&wc_c.join("run.sh"));
    assert_ne!(
        mode, 0,
        "the marker file takes the agreed exec bit: {mode:o}"
    );

    step!("C edits the markers out WITHOUT chmod, resolves and lands");
    std::fs::write(wc_c.join("run.sh"), b"#!/bin/sh\nresolved-1\nbase-2b\n").unwrap();
    let out = cli.run(&wc_c, &["resolve", "run.sh"]);
    assert!(out.status.success(), "resolve: {}", string_of(&out.stderr));
    let out = cli.run(&wc_c, &["land"]);
    assert!(out.status.success(), "land: {}", string_of(&out.stderr));
    assert_eq!(
        cli.run_ok(&wc_c, &["cat", "run.sh", "--at", "main"]),
        "#!/bin/sh\nresolved-1\nbase-2b\n"
    );
    let wc_e = clone_into(&cli, root, "execscript", "wc_e");
    let mode = exec_of(&wc_e.join("run.sh"));
    assert_ne!(mode, 0, "a fresh clone's run.sh is executable ({mode:o})");
    serve.stop();
}
