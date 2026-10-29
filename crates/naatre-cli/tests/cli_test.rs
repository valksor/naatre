//! Host-local lifecycle guards, run against the real `naatre` binary:
//! re-init refused, uninitialized serve refused, second serve refused while
//! the first holds the data-dir flock.

use std::net::TcpStream;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

/// Runs the binary; cargo builds it once per test binary
/// (`CARGO_BIN_EXE_naatre`).
fn run(args: &[&str]) -> std::process::Output {
    Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args(args)
        .output()
        .expect("spawn naatre")
}

fn string_of(bytes: &[u8]) -> String {
    String::from_utf8_lossy(bytes).into_owned()
}

#[test]
fn init_twice_refuses_and_names_the_path() {
    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("data");

    let out = run(&["init", "--data", data.to_str().unwrap()]);
    assert!(
        out.status.success(),
        "first init must succeed: {}",
        string_of(&out.stderr)
    );
    let stdout = string_of(&out.stdout);
    assert!(
        stdout.contains("admin token: ntk_"),
        "token printed once, with prefix: {stdout:?}"
    );

    // Second init refuses, naming the path, and never mints again.
    let out = run(&["init", "--data", data.to_str().unwrap()]);
    assert!(!out.status.success(), "second init must fail");
    let stderr = string_of(&out.stderr);
    assert!(
        stderr.contains(data.to_str().unwrap()),
        "error names the path: {stderr:?}"
    );
    assert!(stderr.contains("already initialized"), "{stderr:?}");
}

#[test]
fn serve_on_uninitialized_dir_refuses() {
    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("never-initialized");

    let out = run(&[
        "serve",
        "--data",
        data.to_str().unwrap(),
        "--listen",
        "127.0.0.1:0",
    ]);
    assert!(
        !out.status.success(),
        "serve must refuse an uninitialized dir"
    );
    let stderr = string_of(&out.stderr);
    assert!(stderr.contains("run naatre init"), "{stderr:?}");
    assert!(
        stderr.contains(data.to_str().unwrap()),
        "error names the path: {stderr:?}"
    );
}

#[test]
fn second_serve_is_refused_while_first_holds_the_flock() {
    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("data");
    let out = run(&["init", "--data", data.to_str().unwrap()]);
    assert!(out.status.success(), "init: {}", string_of(&out.stderr));

    // First serve on a fixed high port; wait until it is listening (that
    // proves the flock is held).
    let port = 47222u16;
    let mut first = Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args([
            "serve",
            "--data",
            data.to_str().unwrap(),
            "--listen",
            &format!("127.0.0.1:{port}"),
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn first serve");

    let started = Instant::now();
    let mut listening = false;
    while started.elapsed() < Duration::from_secs(10) {
        if first.try_wait().is_ok_and(|s| s.is_some()) {
            break; // first serve died (e.g. port collision) — fail below
        }
        if TcpStream::connect(("127.0.0.1", port)).is_ok() {
            listening = true;
            break;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    assert!(
        listening,
        "first serve never started listening on {port} (port in use?)"
    );

    // Second serve on the same data dir → refused, non-zero exit.
    let out = run(&[
        "serve",
        "--data",
        data.to_str().unwrap(),
        "--listen",
        &format!("127.0.0.1:{}", port + 1),
    ]);
    assert!(!out.status.success(), "second serve must be refused");
    let stderr = string_of(&out.stderr);
    assert!(
        stderr.contains("another naatre process holds the data dir"),
        "{stderr:?}"
    );

    // Cleanup: kill the first serve (SIGKILL; the OS releases the flock).
    let _ = first.kill();
    let _ = first.wait();
}

/// `token --data --new-admin` mints on an initialized dir and audits; it
/// refuses an uninitialized dir. (No server involved.)
#[test]
fn new_admin_token_requires_initialized_dir() {
    let dir = tempfile::tempdir().unwrap();
    let uninitialized = dir.path().join("missing");
    let out = run(&[
        "token",
        "--data",
        uninitialized.to_str().unwrap(),
        "--new-admin",
    ]);
    assert!(!out.status.success());
    let stderr = string_of(&out.stderr);
    assert!(stderr.contains("run naatre init"), "{stderr:?}");

    let data = dir.path().join("data");
    let out = run(&["init", "--data", data.to_str().unwrap()]);
    assert!(out.status.success());
    let out = run(&["token", "--data", data.to_str().unwrap(), "--new-admin"]);
    assert!(out.status.success(), "{}", string_of(&out.stderr));
    let stdout = string_of(&out.stdout);
    assert!(stdout.contains("admin token: ntk_"), "{stdout:?}");
}
