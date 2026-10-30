//! README-rot check (SUPERVISOR JUDGMENT, fix3b — NOT a plan clause): the
//! fenced `sh` blocks of README.md's Quickstart section are extracted and
//! run end-to-end against a throwaway data dir, so the quickstart cannot
//! silently drift from the tested path. The flow mirrors
//! `scripts/coldboot.sh` (init → serve → token+URL → repo create → stop
//! serve → import seed → serve → clone from a second directory → commit →
//! land), asserted afterwards by reading the committed file back at `main`
//! and by the server actually being stopped.
//!
//! The harness stands in for the human:
//! - it fabricates `./my-git-repo`, the quickstart's "any local git repo"
//!   seed for step 5, the same way coldboot seeds its import source;
//! - it answers the step-2 `read -s` token prompt by watching the script's
//!   stdout for `admin token: ` and typing the minted token back on stdin;
//! - it never rewrites the extracted fences — they run byte-identical to
//!   what a reader copies.
//!
//! Needs on PATH: `bash`, `git`, `curl` (the fence's readiness probe uses
//! curl against `/healthz`). The quickstart pins port 7400; the test
//! refuses to start when that port is already taken.

use std::io::{BufRead, BufReader, Write};
use std::path::Path;
use std::process::{Command, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

const PORT: u16 = 7400;

/// Runs the whole check as one test: extract → stage → execute → assert.
#[test]
fn readme_quickstart_fences_run_end_to_end() {
    require_tool("bash", &["-c", "true"]);
    require_tool("git", &["--version"]);
    require_tool("curl", &["--version"]);

    let script = quickstart_fences();
    let root = tempfile::tempdir().expect("tempdir");
    let work = root.path().join("work");
    std::fs::create_dir(&work).expect("mkdir work");

    // Step 5's premise: the user already has a local git repo. One commit
    // on the default branch, exactly like coldboot's seed.
    let seed = work.join("my-git-repo");
    run_ok(
        Command::new("git")
            .args(["-c", "init.defaultBranch=main", "init", "-q"])
            .arg(&seed),
    );
    std::fs::write(seed.join("seed.txt"), b"seed\n").expect("write seed file");
    run_ok(
        Command::new("git")
            .args(["add", "seed.txt"])
            .current_dir(&seed),
    );
    run_ok(
        Command::new("git")
            .args([
                "-c",
                "user.email=seed@example.com",
                "-c",
                "user.name=seed",
                "commit",
                "-q",
                "-m",
                "seed",
            ])
            .current_dir(&seed),
    );

    let script_path = root.path().join("quickstart.sh");
    std::fs::write(&script_path, &script).expect("write extracted fences");

    // The fences call bare `naatre`: put the built binary on the child's
    // PATH (symlink under a temp bin dir, prepended — nothing ambient is
    // replaced).
    let bin_dir = root.path().join("bin");
    std::fs::create_dir(&bin_dir).expect("mkdir bin");
    #[cfg(unix)]
    std::os::unix::fs::symlink(env!("CARGO_BIN_EXE_naatre"), bin_dir.join("naatre"))
        .expect("symlink naatre onto PATH");
    let path = {
        let mut paths = vec![bin_dir.clone()];
        paths.extend(std::env::split_paths(
            &std::env::var_os("PATH").unwrap_or_default(),
        ));
        std::env::join_paths(paths).expect("join PATH")
    };

    port_free_or_panic();

    // The EXIT trap is harness glue (interpreter invocation, not a rewrite
    // of the fences): a mid-script failure would otherwise orphan the
    // backgrounded `serve` and leave port 7400 occupied for later runs.
    let runner = "trap 'if [ -n \"${SERVE_PID:-}\" ]; then kill \"$SERVE_PID\" 2>/dev/null || true; fi' EXIT; source \"$1\"";
    let mut child = Command::new("bash")
        .args(["-eu", "-o", "pipefail", "-c", runner])
        .arg("readme-quickstart")
        .arg(&script_path)
        .current_dir(&work)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .env_remove("NAATRE_AUTHOR")
        .env_remove("NAATRE_HOSTNAME")
        .env("PATH", &path)
        .spawn()
        .expect("spawn bash");

    // Watchdog: the fences' readiness loops spin forever if serve never
    // binds (regression or busy port raced the pre-check); bound the run.
    let done = Arc::new(AtomicBool::new(false));
    {
        let done = Arc::clone(&done);
        let pid = child.id();
        std::thread::spawn(move || {
            for _ in 0..360 {
                if done.load(Ordering::Relaxed) {
                    return;
                }
                std::thread::sleep(Duration::from_millis(500));
            }
            let _ = Command::new("kill")
                .arg("-TERM")
                .arg(pid.to_string())
                .output();
            std::thread::sleep(Duration::from_secs(5));
            let _ = Command::new("kill")
                .arg("-KILL")
                .arg(pid.to_string())
                .output();
        });
    }

    // Drain stderr while the script runs (a piped, unread stderr can block
    // the child once the pipe fills).
    let stderr_thread = {
        let err = child.stderr.take().expect("stderr");
        std::thread::spawn(move || {
            let mut buf = String::new();
            let mut reader = BufReader::new(err);
            loop {
                let mut line = String::new();
                match reader.read_line(&mut line) {
                    Ok(0) | Err(_) => break,
                    Ok(_) => buf.push_str(&line),
                }
            }
            buf
        })
    };

    // Read stdout; when init prints the once-only admin token, type it
    // back on stdin at the `read -s` prompt — what the human does.
    let mut stdin = Some(child.stdin.take().expect("stdin"));
    let mut stdout_reader = BufReader::new(child.stdout.take().expect("stdout"));
    let mut token = String::new();
    let mut transcript = String::new();
    let mut line = String::new();
    loop {
        line.clear();
        let n = stdout_reader.read_line(&mut line).expect("read stdout");
        if n == 0 {
            break;
        }
        transcript.push_str(&line);
        if let Some(rest) = line.strip_prefix("admin token: ") {
            token = rest.trim().to_string();
            if let Some(pipe) = stdin.as_mut() {
                pipe.write_all(token.as_bytes()).expect("write token");
                pipe.write_all(b"\n").expect("write newline");
                pipe.flush().expect("flush stdin");
            }
            stdin = None; // the prompt is answered: no further input
        }
    }
    drop(stdin);

    let status = child.wait().expect("wait bash");
    done.store(true, Ordering::Relaxed);
    let stderr = stderr_thread.join().expect("join stderr reader");

    assert!(
        status.success(),
        "quickstart fences failed (exit {status:?})\n--- stdout ---\n{transcript}--- stderr ---\n{stderr}"
    );
    assert!(
        !token.is_empty(),
        "no admin token captured from init's stdout:\n{transcript}"
    );

    // The fences stopped their server (their final step) — the read-back
    // below only succeeds because the port is closed until the harness
    // restarts serve. That restart on the same data dir is also a stop/
    // start durability probe in coldboot's spirit.
    let demo = root.path().join("quickstart-clone").join("demo");
    assert!(demo.is_dir(), "fences created no clone at {demo:?}");
    let mut serve = Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args(["serve", "--data"])
        .arg(work.join("naatre-data"))
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("restart serve for post-asserts");
    let became_ready = (0..100).any(|_| {
        if std::net::TcpStream::connect(("127.0.0.1", PORT)).is_ok() {
            true
        } else {
            std::thread::sleep(Duration::from_millis(100));
            false
        }
    });
    if !became_ready {
        let _ = serve.kill();
        let _ = serve.wait();
        panic!("restarted serve never accepted connections on port {PORT}");
    }

    // Outcome 1: the land landed — the committed file reads back at main
    // from the clone directory the fences created.
    let cat = Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args(["cat", "README.md", "--at", "main"])
        .current_dir(&demo)
        .env("NAATRE_URL", format!("http://127.0.0.1:{PORT}"))
        .env("NAATRE_TOKEN", &token)
        .output()
        .expect("spawn naatre cat");
    let _ = serve.kill();
    let _ = serve.wait();
    assert_eq!(
        cat.status.code(),
        Some(0),
        "cat at main failed: {}",
        String::from_utf8_lossy(&cat.stderr)
    );
    assert_eq!(
        String::from_utf8_lossy(&cat.stdout),
        "hi\n",
        "committed content missing at main"
    );

    // Outcome 2: the harness's own restart serve is stopped too — nothing
    // listens on the pinned port anymore (the fences' server was already
    // gone before the restart, or the cat above would have been redundant).
    assert!(
        std::net::TcpStream::connect(("127.0.0.1", PORT)).is_err(),
        "serve still listening after the post-assert cleanup"
    );
}

/// Extracts README.md's Quickstart-section ```sh fences, in document
/// order, joined as one script body. Prose between fences is skipped — a
/// fence that secretly depends on prose-only state fails the run, which is
/// the rot this check exists to catch.
fn quickstart_fences() -> String {
    let readme = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../README.md");
    let text = std::fs::read_to_string(&readme).expect("read README.md");
    let mut in_section = false;
    let mut in_fence = false;
    let mut blocks: Vec<String> = Vec::new();
    let mut current = String::new();
    for line in text.lines() {
        if line.starts_with("## ") {
            in_section = line == "## Quickstart";
            continue;
        }
        if !in_section {
            continue;
        }
        if let Some(info) = line.strip_prefix("```") {
            if in_fence {
                blocks.push(std::mem::take(&mut current));
                in_fence = false;
            } else if info.starts_with("sh") {
                in_fence = true;
            }
            continue;
        }
        if in_fence {
            current.push_str(line);
            current.push('\n');
        }
    }
    assert!(
        !blocks.is_empty(),
        "README.md's Quickstart section has no ```sh fences — the rot check has nothing to run"
    );
    blocks.join("\n")
}

fn require_tool(name: &str, args: &[&str]) {
    let ok = Command::new(name)
        .args(args)
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false);
    assert!(
        ok,
        "{name} must be on PATH: the README quickstart itself uses it"
    );
}

fn port_free_or_panic() {
    match std::net::TcpListener::bind(("127.0.0.1", PORT)) {
        Ok(listener) => drop(listener),
        Err(e) => panic!(
            "port {PORT} is unavailable ({e}); the README quickstart pins http://127.0.0.1:{PORT}"
        ),
    }
}

fn run_ok(cmd: &mut Command) {
    let out = cmd.output().expect("spawn git");
    assert!(
        out.status.success(),
        "seed-repo setup failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
}
