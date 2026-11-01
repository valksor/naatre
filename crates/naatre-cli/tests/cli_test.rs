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

/// `naatre redact blob|changeset <id>` (host-local, M10b): the positional
/// kind + id shape, the audit actor, and the tombstone ROW's column set —
/// path + date only, no reason, no principal. Seeds a repo + a referenced
/// blob straight into the initialized SQLite file (the direct-DB seeding
/// technique the M2a tests use), then drives the verb against the data dir.
#[test]
fn redact_marks_a_blob_and_carries_path_date_only() {
    use rusqlite::Connection;

    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("data");
    let out = run(&["init", "--data", data.to_str().unwrap()]);
    assert!(out.status.success(), "init: {}", string_of(&out.stderr));

    // Seed: one repo, one objects row, one file_changes row naming a path
    // (the tombstone's `path` derives from it).
    let blob = [7u8; 32];
    {
        let conn = Connection::open(data.join("naatre.db")).unwrap();
        conn.execute_batch("INSERT INTO repos(id, name, created_ns) VALUES (1, 'seeded', 1);")
            .unwrap();
        conn.execute(
            "INSERT INTO objects(object_id, size, csize, touched_ns) VALUES (?1, 1, 1, 1)",
            rusqlite::params![&blob[..]],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
             VALUES (1, x'01', 'leaked.txt', 0, ?1, 1, 0)",
            rusqlite::params![&blob[..]],
        )
        .unwrap();
    }
    let hex: String = blob.iter().map(|b| format!("{b:02x}")).collect();
    let data_arg = data.to_str().unwrap();

    // Usage: the kind is POSITIONAL (`redact blob <id>`); a flag-shaped
    // invocation is a usage error (exit 3).
    let out = run(&[
        "redact", "--kind", "blob", &hex, "--repo", "seeded", "--data", data_arg,
    ]);
    assert_eq!(out.status.code(), Some(3), "positional kind: {out:?}");

    // Unknown repo → exit 1 naming it.
    let out = run(&[
        "redact", "blob", &hex, "--reason", "r", "--repo", "nope", "--data", data_arg,
    ]);
    assert_eq!(out.status.code(), Some(1));
    assert!(
        string_of(&out.stderr).contains("no repo named"),
        "{:?}",
        string_of(&out.stderr)
    );

    // The mark: exit 0, stdout names the object and its path.
    let out = run(&[
        "redact",
        "blob",
        &hex,
        "--reason",
        "takedown 9",
        "--repo",
        "seeded",
        "--data",
        data_arg,
    ]);
    assert!(out.status.success(), "{}", string_of(&out.stderr));
    let stdout = string_of(&out.stdout);
    assert!(stdout.contains(&hex), "{stdout:?}");
    assert!(
        stdout.contains("leaked.txt"),
        "the path is recorded: {stdout:?}"
    );

    // The tombstone ROW: exactly the path + date column set, path filled.
    let conn = Connection::open(data.join("naatre.db")).unwrap();
    let columns: String = conn
        .query_row(
            "SELECT group_concat(name, ',') FROM \
             (SELECT name FROM pragma_table_info('redactions') ORDER BY cid)",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        columns, "repo_id,object_id,kind,path,marked_ns,purged_ns",
        "no reason column, no principal column"
    );
    let (path, reason_cols, principal_cols): (String, i64, i64) = conn
        .query_row(
            "SELECT r.path, \
             (SELECT COUNT(*) FROM pragma_table_info('redactions') WHERE name = 'reason'), \
             (SELECT COUNT(*) FROM pragma_table_info('redactions') WHERE name = 'principal') \
             FROM redactions r LIMIT 1",
            [],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )
        .unwrap();
    assert_eq!(path, "leaked.txt");
    assert_eq!((reason_cols, principal_cols), (0, 0));

    // The audit row carries the actor + reason (the accountability record).
    let (actor, details): (String, String) = conn
        .query_row(
            "SELECT principal, details_json FROM audit_log WHERE action = 'redact'",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    assert_eq!(actor, "admin");
    assert!(details.contains("takedown 9"), "{details:?}");

    // Re-run: idempotent, the earliest mark stands, no second audit row.
    let out = run(&[
        "redact", "blob", &hex, "--reason", "again", "--repo", "seeded", "--data", data_arg,
    ]);
    assert!(out.status.success(), "{}", string_of(&out.stderr));
    assert!(string_of(&out.stdout).contains("already redacted"));
    let audits: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'redact'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 1);
}

/// The restore duty reminder (compliance C7): a restore from an OLDER
/// backup names the backup's age and the redaction re-apply duty. The
/// manifest's `created_ns` is backdated by a year (it is not covered by an
/// artifact checksum), so the age line has something to say.
#[test]
fn restore_prints_the_age_keyed_redaction_duty() {
    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("data");
    let backup = dir.path().join("backup");
    let restored = dir.path().join("restored");
    let out = run(&["init", "--data", data.to_str().unwrap()]);
    assert!(out.status.success(), "init: {}", string_of(&out.stderr));
    let out = run(&[
        "backup",
        "--data",
        data.to_str().unwrap(),
        backup.to_str().unwrap(),
    ]);
    assert!(out.status.success(), "backup: {}", string_of(&out.stderr));

    let manifest_path = backup.join("manifest.json");
    let mut manifest: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&manifest_path).unwrap()).unwrap();
    let year_ns: i64 = 365 * 86_400 * 1_000_000_000;
    let created = manifest["created_ns"].as_i64().unwrap() - year_ns;
    manifest["created_ns"] = serde_json::json!(created);
    std::fs::write(
        &manifest_path,
        serde_json::to_string_pretty(&manifest).unwrap(),
    )
    .unwrap();

    let out = run(&[
        "restore",
        "--data",
        restored.to_str().unwrap(),
        backup.to_str().unwrap(),
    ]);
    assert!(out.status.success(), "restore: {}", string_of(&out.stderr));
    let stdout = string_of(&out.stdout);
    let duty = stdout
        .lines()
        .find(|l| l.contains("DUTY"))
        .unwrap_or_else(|| panic!("no duty line in {stdout:?}"));
    assert!(
        duty.contains("365 day(s)"),
        "the backup's age is named: {duty}"
    );
    assert!(
        duty.contains("redaction") && duty.contains("re-applied") && duty.contains("fsck"),
        "the duty names the re-apply and fsck's blind spot: {duty}"
    );
}
