//! M4b crash matrix: every `NAATRE_CRASH_POINT` name of the closed enum
//! fires inside a SPAWNED SUBPROCESS (this test binary re-execs itself —
//! the env var is read once per process), the child dies hard at the panic
//! (`std::process::exit(101)` after the marker files are written; no
//! `catch_unwind`), and the PARENT reopens the data dir and asserts the
//! post-crash state: rollback points leave zero land rows, main unmoved,
//! the key unsettled and re-landable; `after_commit` leaves the land
//! durable with an idempotent replay. Plus the randomized SIGKILL fuzz:
//! main is never torn, the key re-lands, and no changeset has a dangling
//! parent (fsck-equivalent walk).
//!
//! Concurrency note: every test owns private temp dirs and child processes
//! — no shared state — so the default parallel test harness is safe and no
//! `--test-threads` pin is needed.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::time::{Duration, Instant};

use serde_json::{Value, json};

use naatre_core::ids::Id;
use naatre_server::land::CRASH_POINTS;

mod m4b_common;

use m4b_common::{
    DEADLINE, REPO, add, attach_app, count_at, count_params_at, create_ok_on, cs_body, land_on,
    main_target_at, mod_, put_blob_on, seed_main_at, seed_repo_at, seed_token_at,
};

// ---- parent-side child plumbing ---------------------------------------------

/// Creates and initializes one data dir + marker paths under `base`.
fn prepare_dir(base: &Path, tag: &str) -> (PathBuf, PathBuf, PathBuf) {
    let dir = base.join(format!("{tag}-data"));
    let markers = base.join(format!("{tag}-markers"));
    std::fs::create_dir_all(&markers).unwrap();
    let data = naatre_store::DataDir::create(&dir).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let state = markers.join("state.json");
    let ready = markers.join("ready");
    (dir, state, ready)
}

/// Re-execs this test binary as the land child.
fn spawn_child(dir: &Path, state: &Path, ready: &Path, crash: Option<&str>) -> Child {
    let mut cmd = Command::new(std::env::current_exe().unwrap());
    cmd.args(["--exact", "m4b_child_entry", "--nocapture"])
        .env("NAATRE_M4B_DIR", dir)
        .env("NAATRE_M4B_STATE", state)
        .env("NAATRE_M4B_READY", ready)
        .env_remove("NAATRE_CRASH_POINT")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if let Some(name) = crash {
        cmd.env("NAATRE_CRASH_POINT", name);
    }
    cmd.spawn().expect("spawn reexec child")
}

struct Collected {
    status: ExitStatus,
    stderr: String,
}

/// Waits for the child with a hard deadline; `kill_after` (the SIGKILL fuzz
/// window) kills instead of failing at the deadline. Pipe drainer threads
/// keep the child from ever blocking on a full pipe while we poll.
fn wait_bounded(child: Child, kill_after: Option<Duration>, what: &str) -> Collected {
    let mut child = child;
    let mut out_pipe = child.stdout.take().expect("stdout piped");
    let mut err_pipe = child.stderr.take().expect("stderr piped");
    let err_reader = std::thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = err_pipe.read_to_end(&mut buf);
        String::from_utf8_lossy(&buf).into_owned()
    });
    let out_reader = std::thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = out_pipe.read_to_end(&mut buf);
    });

    let started = Instant::now();
    let limit = kill_after.unwrap_or(DEADLINE);
    let status = loop {
        if let Some(status) = child.try_wait().expect("try_wait") {
            break status;
        }
        if started.elapsed() >= limit {
            let _ = child.kill(); // SIGKILL (fuzz) or runaway-child stop
            if kill_after.is_none() {
                let _ = child.wait();
                panic!("child ({what}) exceeded {DEADLINE:?}; killed");
            }
            break child.wait().expect("wait after kill");
        }
        std::thread::sleep(Duration::from_millis(2));
    };
    out_reader.join().expect("stdout drainer");
    Collected {
        status,
        stderr: err_reader.join().expect("stderr drainer"),
    }
}

/// Bounded poll for a marker file the child writes.
fn wait_for_file(path: &Path, what: &str) {
    let started = Instant::now();
    while !path.exists() {
        if started.elapsed() > DEADLINE {
            panic!("child never wrote {what} at {}", path.display());
        }
        std::thread::sleep(Duration::from_millis(5));
    }
}

/// tmp + rename so the parent never reads a half-written marker.
fn atomic_write(path: &Path, bytes: &[u8]) {
    let tmp = path.with_extension("tmp");
    std::fs::write(&tmp, bytes).unwrap();
    std::fs::rename(&tmp, path).unwrap();
}

/// Pseudo-random 1..=30 ms kill delay, mixed per iteration (no rand crate —
/// the fuzz wants variety, not statistics).
fn kill_delay_ms(iteration: u32) -> u64 {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .subsec_nanos() as u64;
    1 + nanos
        .rotate_left(iteration ^ 17)
        .wrapping_mul(0x9E37_79B9_7F4A_7C15)
        % 30
}

// ---- the reexec child --------------------------------------------------------

/// Normal suite runs: env markers absent → trivial pass (the M4a pattern).
#[tokio::test]
async fn m4b_child_entry() {
    let Ok(dir) = std::env::var("NAATRE_M4B_DIR") else {
        return;
    };
    child_main(Path::new(&dir)).await;
}

/// Seeds the repo, writes the state + ready markers, fires the land. A
/// `NAATRE_CRASH_POINT` name makes the land panic inside the write slot
/// (surfaced to the route as a 500); the child then dies with exit 101.
/// Without the env var (SIGKILL fuzz) the land simply completes.
async fn child_main(dir: &Path) {
    let crash = std::env::var("NAATRE_CRASH_POINT").ok();
    let state_path = PathBuf::from(std::env::var("NAATRE_M4B_STATE").unwrap());
    let ready_path = PathBuf::from(std::env::var("NAATRE_M4B_READY").unwrap());
    let conflict = crash.as_deref() == Some("after_conflict_rollback");

    let attached = attach_app(dir);
    let app = &attached.app;
    let db = &attached.db_path;
    let writer = seed_token_at(db, "writer-p", "writer", None);
    let repo_id = seed_repo_at(db, REPO);

    let key = "k-crash";
    let (head, old_main, res_head, cs_count) = if conflict {
        // Both sides rewrite line 1 of one file → the rebase conflicts.
        let b1 = put_blob_on(app, &writer, b"l1\nl2\nl3\n").await;
        let (c1, _) = create_ok_on(
            app,
            &writer,
            &cs_body(json!([]), json!([add("same.txt", &b1, 9)]), "c1"),
        )
        .await;
        let ours = put_blob_on(app, &writer, b"C1-ours\nl2\nl3\n").await;
        let (c2, _) = create_ok_on(
            app,
            &writer,
            &cs_body(
                json!([c1.clone()]),
                json!([mod_("same.txt", &ours, 14)]),
                "c2",
            ),
        )
        .await;
        let theirs = put_blob_on(app, &writer, b"C1-theirs\nl2\nl3\n").await;
        let (c2p, _) = create_ok_on(
            app,
            &writer,
            &cs_body(
                json!([c1.clone()]),
                json!([mod_("same.txt", &theirs, 16)]),
                "c2p",
            ),
        )
        .await;
        // The recipe's resolution changeset, pre-built on the fork base.
        let br = put_blob_on(app, &writer, b"resolved\n").await;
        let (res, _) = create_ok_on(
            app,
            &writer,
            &cs_body(
                json!([c1.clone()]),
                json!([add("resolved.txt", &br, 9)]),
                "resolve",
            ),
        )
        .await;
        seed_main_at(db, repo_id, &c2p);
        (c2, c2p, Some(res), 4)
    } else {
        // Adjacent-region edits: the rebase merges cleanly and inserts a
        // rebased changeset (so "no changesets from the land exist" has
        // something real to assert about).
        let b1 = put_blob_on(app, &writer, b"alpha\nbeta\ngamma\n").await;
        let (c1, _) = create_ok_on(
            app,
            &writer,
            &cs_body(json!([]), json!([add("a.txt", &b1, 18)]), "c1"),
        )
        .await;
        let head_blob = put_blob_on(app, &writer, b"HEAD-1\nbeta\ngamma\n").await;
        let (c2, _) = create_ok_on(
            app,
            &writer,
            &cs_body(
                json!([c1.clone()]),
                json!([mod_("a.txt", &head_blob, 19)]),
                "c2",
            ),
        )
        .await;
        let main_blob = put_blob_on(app, &writer, b"alpha\nbeta\nMAIN-3\n").await;
        let (c2p, _) = create_ok_on(
            app,
            &writer,
            &cs_body(
                json!([c1.clone()]),
                json!([mod_("a.txt", &main_blob, 19)]),
                "c2p",
            ),
        )
        .await;
        seed_main_at(db, repo_id, &c2p);
        (c2, c2p, None, 3)
    };

    let state = json!({
        "repo": REPO,
        "principal": "writer-p",
        "writer": writer,
        "key": key,
        "head": head,
        "old_main": old_main,
        "res_head": res_head,
        "cs_count": cs_count,
        "scenario": if conflict { "conflict" } else { "rebase" },
    });
    atomic_write(&state_path, state.to_string().as_bytes());
    atomic_write(&ready_path, b"ready");

    let resp = land_on(app, &writer, &head, key).await;
    match crash.as_deref() {
        Some(name) => {
            assert!(
                CRASH_POINTS.contains(&name),
                "child asked to crash at unknown point {name:?}"
            );
            // The panic happened on the blocking write thread; the route
            // surface observes it as an internal error.
            assert_eq!(
                resp.status,
                axum::http::StatusCode::INTERNAL_SERVER_ERROR,
                "crash point {name} did not crash the land: {}",
                resp.body
            );
            std::process::exit(101);
        }
        None => {
            assert_eq!(
                resp.status,
                axum::http::StatusCode::OK,
                "fuzz child's plain land failed: {}",
                resp.body
            );
        }
    }
}

// ---- 1. the matrix ------------------------------------------------------------

/// The whole closed enum: the child crashes hard at each point; the parent
/// reopens the data dir and checks the invariants.
#[cfg(debug_assertions)]
#[tokio::test]
async fn crash_matrix_walks_the_enum() {
    let base = tempfile::tempdir().unwrap();
    for (i, name) in CRASH_POINTS.into_iter().enumerate() {
        let (dir, state_path, ready_path) = prepare_dir(base.path(), &format!("cp{i:02}"));
        let child = spawn_child(&dir, &state_path, &ready_path, Some(name));
        wait_for_file(&ready_path, "the ready marker");
        let out = wait_bounded(child, None, name);

        assert_eq!(
            out.status.code(),
            Some(101),
            "crash point {name}: child must die at the panic"
        );
        assert!(
            out.stderr
                .contains(&format!("crash point {name} triggered")),
            "crash point {name}: expected its panic on stderr, got: {}",
            out.stderr
        );

        let state: Value =
            serde_json::from_str(&std::fs::read_to_string(&state_path).unwrap()).unwrap();
        assert_eq!(
            state["scenario"],
            if name == "after_conflict_rollback" {
                "conflict"
            } else {
                "rebase"
            }
        );
        let (head, old_main, key) = (
            state["head"].as_str().unwrap(),
            state["old_main"].as_str().unwrap(),
            state["key"].as_str().unwrap(),
        );
        let cs_before = state["cs_count"].as_i64().unwrap();
        let principal = state["principal"].as_str().unwrap().to_string();
        let writer = state["writer"].as_str().unwrap().to_string();

        let lands_rows = || {
            count_params_at(
                &dir.join("naatre.db"),
                "SELECT COUNT(*) FROM lands WHERE principal = ?1 AND key = ?2",
                &[&principal, &key],
            )
        };

        if name == "after_commit" {
            // The land IS durable: main moved, the rebased changeset exists,
            // the key is settled done.
            let main = main_target_at(&dir.join("naatre.db"), 1).expect("main after commit");
            assert_ne!(main, old_main, "after_commit: main must have moved");
            assert_eq!(
                count_at(&dir.join("naatre.db"), "SELECT COUNT(*) FROM changesets"),
                cs_before + 1,
                "after_commit: the rebased changeset persisted"
            );
            assert_eq!(lands_rows(), 1, "after_commit: exactly one lands row");
            assert_eq!(
                count_params_at(
                    &dir.join("naatre.db"),
                    "SELECT COUNT(*) FROM lands WHERE principal = ?1 AND key = ?2 AND state = 'done'",
                    &[&principal, &key]
                ),
                1
            );
            // Re-land same key+head: the SAME idempotent result, no double
            // land.
            let attached = attach_app(&dir);
            let resp = land_on(&attached.app, &writer, head, key).await;
            assert_eq!(
                resp.status,
                axum::http::StatusCode::OK,
                "body: {}",
                resp.body
            );
            assert_eq!(resp.body["landed"], main.as_str());
            assert_eq!(resp.body["main"], main.as_str());
            assert_eq!(lands_rows(), 1, "replay must not add a lands row");
        } else if name == "after_conflict_rollback" {
            // Rolled back: nothing persisted, key unsettled.
            assert_eq!(
                count_at(&dir.join("naatre.db"), "SELECT COUNT(*) FROM changesets"),
                cs_before,
                "{name}: no changesets from the land"
            );
            assert_eq!(
                main_target_at(&dir.join("naatre.db"), 1).as_deref(),
                Some(old_main),
                "{name}: main unmoved"
            );
            assert_eq!(lands_rows(), 0, "{name}: key unsettled");
            // Re-land same key+head deterministically conflicts again (main
            // is untouched) — and still leaves the key free.
            let attached = attach_app(&dir);
            let resp = land_on(&attached.app, &writer, head, key).await;
            assert_eq!(resp.status, axum::http::StatusCode::CONFLICT);
            assert_eq!(lands_rows(), 0);
            // The recipe: the resolution changeset re-lands with the SAME
            // key.
            let res_head = state["res_head"].as_str().unwrap();
            let resp = land_on(&attached.app, &writer, res_head, key).await;
            assert_eq!(
                resp.status,
                axum::http::StatusCode::OK,
                "body: {}",
                resp.body
            );
            let landed = resp.body["landed"].as_str().unwrap();
            assert_eq!(
                main_target_at(&dir.join("naatre.db"), 1).as_deref(),
                Some(landed)
            );
            assert_eq!(lands_rows(), 1);
        } else {
            // Rollback points: zero rows from the land, main unmoved, key
            // unsettled — and a fresh land with the same key+head succeeds.
            assert_eq!(
                count_at(&dir.join("naatre.db"), "SELECT COUNT(*) FROM changesets"),
                cs_before,
                "{name}: no changesets from the land"
            );
            assert_eq!(
                main_target_at(&dir.join("naatre.db"), 1).as_deref(),
                Some(old_main),
                "{name}: main unmoved"
            );
            assert_eq!(lands_rows(), 0, "{name}: key unsettled");
            let attached = attach_app(&dir);
            let resp = land_on(&attached.app, &writer, head, key).await;
            assert_eq!(
                resp.status,
                axum::http::StatusCode::OK,
                "{name}: re-land must succeed, body: {}",
                resp.body
            );
            let landed = resp.body["landed"].as_str().unwrap();
            assert_ne!(
                landed, head,
                "{name}: the fresh land rebases (main sits on a sibling)"
            );
            assert_eq!(
                main_target_at(&dir.join("naatre.db"), 1).as_deref(),
                Some(landed),
                "{name}: re-land moved main"
            );
            assert_eq!(lands_rows(), 1, "{name}: re-land settled exactly once");
        }
    }
}

// ---- 2. SIGKILL fuzz ------------------------------------------------------------

/// 5 iterations: kill -9 at a random 1–30 ms after the child signals "land
/// in flight", then reopen and assert main is old-or-new (never torn), the
/// key re-lands, and no changeset has a missing parent.
#[tokio::test]
async fn sigkill_fuzz_never_tears_main() {
    let base = tempfile::tempdir().unwrap();
    for i in 0..5 {
        let tag = format!("kill{i:02}");
        let (dir, state_path, ready_path) = prepare_dir(base.path(), &tag);
        let db_path = dir.join("naatre.db");
        let child = spawn_child(&dir, &state_path, &ready_path, None);
        wait_for_file(&ready_path, "the ready marker");
        let out = wait_bounded(child, Some(Duration::from_millis(kill_delay_ms(i))), &tag);

        let state: Value =
            serde_json::from_str(&std::fs::read_to_string(&state_path).unwrap()).unwrap();
        let (head, old_main, key) = (
            state["head"].as_str().unwrap(),
            state["old_main"].as_str().unwrap(),
            state["key"].as_str().unwrap(),
        );
        let principal = state["principal"].as_str().unwrap().to_string();
        let writer = state["writer"].as_str().unwrap().to_string();

        // main is at one of {old, new} — and "new" only ever means a
        // changeset row that fully exists (the ref never dangles).
        let pre_kill_main = main_target_at(&db_path, 1);
        if let Some(main) = &pre_kill_main {
            let exists: i64 = count_params_at(
                &db_path,
                "SELECT COUNT(*) FROM changesets WHERE id = ?1",
                &[&Id::from_hex(main).unwrap().0.as_slice()],
            );
            assert_eq!(
                exists, 1,
                "iteration {i}: main points at a changeset that does not exist (torn)"
            );
        }

        // fsck-equivalent: every changeset's parents exist.
        let orphans: i64 = count_at(
            &db_path,
            "SELECT COUNT(*) FROM changeset_parents cp \
             WHERE NOT EXISTS (SELECT 1 FROM changesets c \
                               WHERE c.repo_id = cp.repo_id AND c.id = cp.parent)",
        );
        assert_eq!(orphans, 0, "iteration {i}: dangling parents");

        // The key re-lands (fresh land, or the idempotent replay of a land
        // the SIGKILL arrived too late to prevent).
        let attached = attach_app(&dir);
        let resp = land_on(&attached.app, &writer, head, key).await;
        assert_eq!(
            resp.status,
            axum::http::StatusCode::OK,
            "iteration {i}: re-land failed: {} (child exit {:?})",
            resp.body,
            out.status.code()
        );
        let landed = resp.body["landed"].as_str().unwrap().to_string();
        assert_eq!(
            main_target_at(&db_path, 1).as_deref(),
            Some(landed.as_str()),
            "iteration {i}: main at the re-landed tip"
        );
        assert!(
            pre_kill_main.as_deref() == Some(old_main)
                || pre_kill_main.as_deref() == Some(landed.as_str()),
            "iteration {i}: pre-kill main {pre_kill_main:?} is neither old ({old_main}) nor the landed tip ({landed})"
        );
        let rows = count_params_at(
            &db_path,
            "SELECT COUNT(*) FROM lands WHERE principal = ?1 AND key = ?2",
            &[&principal, &key],
        );
        assert!(
            rows == 0 || rows == 1,
            "iteration {i}: key settled {rows} times"
        );
        if rows == 1 {
            assert_eq!(
                count_params_at(
                    &db_path,
                    "SELECT COUNT(*) FROM lands WHERE principal = ?1 AND key = ?2 AND state = 'done'",
                    &[&principal, &key]
                ),
                1,
                "iteration {i}: a present lands row is done, never pending"
            );
        }
    }
}
