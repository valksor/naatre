//! M10a suite: the reachability GC. The plan's pins live here (SQLite+fs):
//! BOTH gates (live objects survive byte-identical; unreachable reclaimed
//! only after the grace window — a fresh orphan is NOT collected before the
//! window and is collected after a clock advance), the ledger's
//! first-seen semantics and its repair direction, the seed set (git_map,
//! lands head), pre-rebase originals collected after the window (the
//! conflict flow orphans the first head), the advisory-only rule (a lying
//! `repo_blobs` row cannot doom a live blob and is ignored harmlessly),
//! the re-reference race (an OLD orphan re-PUT mid-pass re-enters grace via
//! `objects.touched_ns`; a changeset referencing it mid-pass is spared by
//! the in-transaction re-verification), batch convergence, unexpired
//! snapshots protecting their trees, and fsck clean after GC.
//!
//! The GC passes are driven directly through [`gc_pass`] with the injected
//! clock; the serve-side background tasks are off-switched for
//! determinism (they run the same fn).

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::http::StatusCode;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::fsck;
use naatre_server::gc::{GC_DEFAULT_GRACE_SECS, gc_pass};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

#[path = "m4b_common/mod.rs"]
mod m4b_common;
use m4b_common::{REPO, Resp, cs_body, get_on, post_json_on, seed_repo_at, seed_token_at};

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
/// The grace window as ns (30 days, the default).
const GRACE_NS: u64 = GC_DEFAULT_GRACE_SECS * 1_000_000_000;

// ---- harness (m5a-style: the clock handle stays reachable) ------------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
    state: AppState,
    clock: Arc<AtomicU64>,
}

fn build_app_with(configure: impl FnOnce(&mut ServerState)) -> TestApp {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let dir = tempfile::tempdir().unwrap();
    let data = DataDir::create(dir.path()).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(dir.path()).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    // Every background task off: the tests drive gc_pass directly.
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    state.derive_queue_enabled = false;
    state.derive_backfill_enabled = false;
    configure(&mut state);
    let db_path = dir.path().join("naatre.db");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    TestApp {
        _dir: dir,
        app,
        db_path,
        state,
        clock,
    }
}

struct Fixture {
    t: TestApp,
    writer: String,
    repo_id: i64,
}

fn fixture() -> Fixture {
    fixture_with(|_| {})
}

fn fixture_with(configure: impl FnOnce(&mut ServerState)) -> Fixture {
    let t = build_app_with(configure);
    let writer = seed_token_at(&t.db_path, "writer-p", "writer", None);
    let repo_id = seed_repo_at(&t.db_path, REPO);
    Fixture { t, writer, repo_id }
}

impl Fixture {
    async fn put_blob(&self, content: &[u8]) -> String {
        m4b_common::put_blob_on(&self.t.app, &self.writer, content).await
    }

    async fn create_ok(&self, body: &Value) -> (String, String) {
        let resp = post_json_on(
            &self.t.app,
            &format!("/v1/repos/{REPO}/changesets"),
            &self.writer,
            body,
        )
        .await;
        assert_eq!(resp.status, StatusCode::CREATED, "body: {}", resp.body);
        (
            resp.body["id"].as_str().unwrap().to_string(),
            resp.body["root_tree_id"].as_str().unwrap().to_string(),
        )
    }

    async fn land(&self, head: &str, key: &str) -> Resp {
        post_json_on(
            &self.t.app,
            &format!("/v1/repos/{REPO}/land"),
            &self.writer,
            &json!({ "head": head, "key": key }),
        )
        .await
    }

    /// Raw blob bytes of `path` at `at` through /file + /objects.
    async fn cat(&self, at: &str, path: &str) -> Vec<u8> {
        let resp = get_on(
            &self.t.app,
            &format!(
                "/v1/repos/{REPO}/file?at={at}&path={}",
                path.replace('/', "%2F")
            ),
            &self.writer,
        )
        .await;
        assert_eq!(resp.status, StatusCode::OK, "cat {path}: {}", resp.body);
        let blob = resp.body["blob_id"].as_str().unwrap().to_string();
        let req = axum::http::Request::builder()
            .method("GET")
            .uri(format!("/v1/repos/{REPO}/objects/{blob}"))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", self.writer),
            )
            .body(axum::body::Body::empty())
            .unwrap();
        let res = self.t.app.clone().oneshot(req).await.unwrap();
        assert!(res.status().is_success());
        http_body_util::BodyExt::collect(res.into_body())
            .await
            .unwrap()
            .to_bytes()
            .to_vec()
    }

    fn count(&self, sql: &str) -> i64 {
        m4b_common::count_at(&self.t.db_path, sql)
    }

    fn count_params(&self, sql: &str, params: &[&dyn rusqlite::ToSql]) -> i64 {
        m4b_common::count_params_at(&self.t.db_path, sql, params)
    }

    /// The main ref target as a JSON hex string (null when unset).
    fn main_hex(&self) -> Value {
        let hex: Option<String> = m4b_common::conn_at(&self.t.db_path)
            .query_row(
                "SELECT lower(hex(target)) FROM refs WHERE repo_id = ?1 AND name = 'main'",
                [self.repo_id],
                |r| r.get(0),
            )
            .ok();
        hex.map(Value::String).unwrap_or(Value::Null)
    }

    fn seed_main(&self, target: &str) {
        m4b_common::seed_main_at(&self.t.db_path, self.repo_id, target);
    }

    fn tick_to(&self, now: u64) {
        self.t.clock.store(now, Ordering::Relaxed);
    }

    /// A commit on `parents` adding one file; returns the cs id.
    async fn commit(&self, parents: Value, path: &str, content: &[u8]) -> String {
        let blob = self.put_blob(content).await;
        let (id, _) = self
            .create_ok(&cs_body(
                parents,
                json!([m4b_common::add(path, &blob, content.len())]),
                "commit",
            ))
            .await;
        id
    }
}

fn blob_id(content: &[u8]) -> Id {
    object_id(Kind::Blob, content)
}

fn hex_id(hex: &str) -> Id {
    Id::from_hex(hex).unwrap()
}

/// The base every test starts from: a ROOT commit (one file + one nested
/// file, so subtrees exist to be shared) seeded as main. Returns
/// (root cs id, root tree id).
async fn seed_base(f: &Fixture) -> (String, String) {
    let a = f.put_blob(b"live-content-A").await;
    let x = f.put_blob(b"nested-x").await;
    let (root, tree) = f
        .create_ok(&cs_body(
            json!([]),
            json!([
                m4b_common::add("a.txt", &a, b"live-content-A".len()),
                m4b_common::add("dir/x.txt", &x, b"nested-x".len()),
            ]),
            "root",
        ))
        .await;
    f.seed_main(&root);
    (root, tree)
}

// ---- 1. both gates -----------------------------------------------------------

/// Gate 1+2: live objects survive byte-identical; the unreachable orphan is
/// NOT collected before the grace window and IS collected after the clock
/// advances past it — with its advisory M6 rows, its junctioned root tree,
/// and its bytes, while the shared subtree survives.
#[tokio::test]
async fn gc_live_survive_and_orphans_reclaim_after_grace() {
    let f = fixture();
    let (root, _) = seed_base(&f).await;
    let main_before = f.main_hex();
    let a_served = f.cat(&root, "a.txt").await;
    assert_eq!(a_served, b"live-content-A".to_vec());

    // Unreachable-from-birth: an unlanded commit on the tip. Its root tree
    // is unique; its dir subtree is shared with the root commit.
    let orphan_content = b"orphan-content-B".to_vec();
    let orphan = f
        .commit(json!([root.clone()]), "b.txt", &orphan_content)
        .await;
    let orphan_root: Vec<u8> = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT root_tree_id FROM changesets WHERE repo_id = ?1 AND id = ?2",
            rusqlite::params![f.repo_id, hex_id(&orphan).0.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    // Advisory M6 rows for the orphan, seeded the way a deriver leaves them.
    {
        let conn = m4b_common::conn_at(&f.t.db_path);
        conn.execute(
            "INSERT INTO derive_state(kind, repo_id, cs, state) VALUES (1, ?1, ?2, 'done')",
            rusqlite::params![f.repo_id, hex_id(&orphan).0.as_slice()],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO derive_reports(kind, repo_id, cs, reason, ts_ns) VALUES (1, ?1, ?2, 'seed', 1)",
            rusqlite::params![f.repo_id, hex_id(&orphan).0.as_slice()],
        )
        .unwrap();
    }
    let changesets_before = f.count("SELECT COUNT(*) FROM changesets");

    // Pass 1 at the base clock: the orphan (cs + blob + unique root tree)
    // becomes CANDIDATES; nothing collects; no audit row.
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.cs_collected, 0, "{report:?}");
    assert_eq!(report.blobs_collected, 0, "{report:?}");
    assert_eq!(report.trees_collected, 0, "{report:?}");
    assert!(report.candidates_new >= 3, "{report:?}");
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'gc'"),
        0
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM gc_pending WHERE scope = 'cs'"),
        1
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob'"),
        1
    );

    // Gate 1: one nanosecond inside the window — NOT collected, and the
    // ledger keeps its FIRST-seen timestamp (no new rows).
    f.tick_to(BASE_NS + GRACE_NS - 1);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.cs_collected + report.blobs_collected + report.trees_collected,
        0,
        "{report:?}"
    );
    assert_eq!(
        report.candidates_new, 0,
        "the first-seen stamp is kept: {report:?}"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM changesets"),
        changesets_before
    );
    assert_eq!(
        f.t.state.cas.has(&[blob_id(&orphan_content)]).unwrap(),
        vec![true],
        "the orphan's bytes survive the window"
    );

    // Gate 2: past the window — the orphan collects (rows, advisory M6
    // rows, root tree); the live chain is byte-identical; the shared dir
    // tree survives. The unique BLOB is still referenced by the orphan
    // root tree's entries inside this same pass (trees collect after
    // blobs), so it follows one pass later — never dangling.
    f.tick_to(BASE_NS + GRACE_NS);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.cs_collected, 1, "{report:?}");
    assert!(
        report.trees_collected >= 1,
        "the orphan's root tree goes: {report:?}"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM changesets"),
        changesets_before - 1
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM file_changes WHERE cs = ?1",
            &[&hex_id(&orphan).0.as_slice()]
        ),
        0
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM derive_state WHERE cs = ?1",
            &[&hex_id(&orphan).0.as_slice()]
        ),
        0,
        "the advisory queue row goes with the changeset"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM derive_reports WHERE cs = ?1",
            &[&hex_id(&orphan).0.as_slice()]
        ),
        0,
        "the advisory report row goes with the changeset"
    );
    assert_eq!(
        f.count_params("SELECT COUNT(*) FROM trees WHERE id = ?1", &[&orphan_root]),
        0,
        "the junctioned root tree of the collected changeset goes"
    );
    f.tick_to(BASE_NS + GRACE_NS + 1);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_condemned, 1,
        "the orphan blob is condemned once its tree is gone: {report:?}"
    );
    f.tick_to(BASE_NS + GRACE_NS + 2);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_collected, 1,
        "phase B deletes the condemned blob: {report:?}"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&blob_id(&orphan_content).0.as_slice()]
        ),
        0,
        "the objects row goes with the bytes"
    );
    assert_eq!(
        f.t.state.cas.has(&[blob_id(&orphan_content)]).unwrap(),
        vec![false],
        "the orphan's bytes are reclaimed"
    );
    assert_eq!(
        f.cat(&root, "a.txt").await,
        a_served,
        "live survives byte-identical"
    );
    assert_eq!(f.cat(&root, "dir/x.txt").await, b"nested-x".to_vec());
    assert_eq!(main_before, f.main_hex(), "main did not move");

    // One audit row per collecting pass, actor "system": the changeset
    // pass, then the blob follow-up. An idle pass adds none.
    let (count, actor, details): (i64, String, String) = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT COUNT(*), MAX(principal), MAX(details_json) FROM audit_log WHERE action = 'gc'",
            [],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )
        .unwrap();
    assert_eq!(count, 2, "one row per collecting pass");
    assert_eq!(actor, "system");
    let details: Value = serde_json::from_str(&details).unwrap();
    assert_eq!(details["blobs"], json!(1), "the follow-up row: {details}");
    assert_eq!(details["grace_secs"], json!(GC_DEFAULT_GRACE_SECS));
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report, Default::default(), "the third pass is idle");
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'gc'"),
        2
    );
}

// ---- 2. repair direction ------------------------------------------------------

/// A candidate that becomes reachable again is repaired (its ledger row is
/// removed) and never collects, no matter how long it then lives.
#[tokio::test]
async fn gc_repair_spares_a_revived_candidate() {
    let f = fixture();
    let (root, _) = seed_base(&f).await;
    let content = b"revived".to_vec();
    let orphan = f.commit(json!([root]), "r.txt", &content).await;
    gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        f.count("SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob'"),
        1
    );

    // Revive: main moves onto the orphan (the cas_ref path's shape, direct
    // for setup determinism).
    m4b_common::conn_at(&f.t.db_path)
        .execute(
            "UPDATE refs SET target = ?1 WHERE repo_id = ?2 AND name = 'main'",
            rusqlite::params![hex_id(&orphan).0.as_slice(), f.repo_id],
        )
        .unwrap();
    f.tick_to(BASE_NS + GRACE_NS * 2);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.cs_collected + report.blobs_collected,
        0,
        "{report:?}"
    );
    assert!(
        report.candidates_repaired >= 2,
        "cs + blob repaired: {report:?}"
    );
    assert_eq!(f.count("SELECT COUNT(*) FROM gc_pending"), 0);
    assert_eq!(
        f.t.state.cas.has(&[blob_id(&content)]).unwrap(),
        vec![true],
        "the revived blob is untouched"
    );
}

// ---- 3. seeds ------------------------------------------------------------------

/// git_map rows seed reachability: an imported commit that nothing else
/// references survives grace on the strength of its mapping.
#[tokio::test]
async fn gc_git_map_seed_survives() {
    let f = fixture();
    let (root, _) = seed_base(&f).await;
    let content = b"imported-history".to_vec();
    let cs = f.commit(json!([root]), "g.txt", &content).await;
    m4b_common::conn_at(&f.t.db_path)
        .execute(
            "INSERT INTO git_map(repo_id, git_oid, cs_id) VALUES (?1, ?2, ?3)",
            rusqlite::params![
                f.repo_id,
                hex_id(&cs).0.as_slice(), // the import mapping
                hex_id(&cs).0.as_slice(),
            ],
        )
        .unwrap();
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS * 2);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.cs_collected, 0, "the git_map seed holds: {report:?}");
    assert_eq!(f.t.state.cas.has(&[blob_id(&content)]).unwrap(), vec![true]);
}

/// Pre-rebase originals are NOT roots: a land attempt that CONFLICTS
/// commits no lands row, so the attempted head (uploaded before the land)
/// is unreachable from the conflict onward — collected after the window,
/// while the settled resolution and the main chain stay byte-identical.
#[tokio::test]
async fn gc_pre_rebase_original_collects_after_grace() {
    let f = fixture();
    let (root, _) = seed_base(&f).await; // a.txt + dir/x.txt

    // Rival lands first: adds y.txt.
    let rival = f.commit(json!([root.clone()]), "y.txt", b"rival-y").await;
    let resp = f.land(&rival, "kr").await;
    assert_eq!(resp.status, StatusCode::OK, "{:?}", resp.body);
    let main_after_rival = f.main_hex();

    // The doomed attempt: same path, different content, parent behind main
    // — a real rebase that must conflict.
    let original = f
        .commit(json!([root.clone()]), "y.txt", b"original-y")
        .await;
    let resp = f.land(&original, "k2").await;
    assert_eq!(
        resp.status,
        StatusCode::CONFLICT,
        "expected the y.txt clash: {:?}",
        resp.body
    );
    // The conflict settles no row: nothing references the original head.
    assert_eq!(
        f.count("SELECT COUNT(*) FROM lands"),
        1,
        "only the rival's row"
    );

    // The client resolves on the original's parent and re-lands the key.
    let resolution = f.commit(json!([root]), "z.txt", b"resolution-z").await;
    let resp = f.land(&resolution, "k2").await;
    assert_eq!(resp.status, StatusCode::OK, "{:?}", resp.body);
    let lands_head: Vec<u8> = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT head FROM lands WHERE repo_id = ?1 AND key = 'k2'",
            [f.repo_id],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        lands_head,
        hex_id(&resolution).0.to_vec(),
        "the row records the resolution"
    );

    // Inside the window: the original survives as a candidate only.
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS - 1);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.cs_collected, 0, "inside the window: {report:?}");

    // Past the window: the pre-rebase original collects (its unique tree
    // goes with it); the unique BLOB is still referenced by that dying
    // tree's entries inside the same pass, so it collects one pass later —
    // never dangling, always converging. The landed chain is intact.
    f.tick_to(BASE_NS + GRACE_NS);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.cs_collected, 1, "{report:?}");
    assert!(report.trees_collected >= 1, "{report:?}");
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM changesets WHERE id = ?1",
            &[&hex_id(&original).0.as_slice()]
        ),
        0,
        "the pre-rebase original is collected"
    );
    f.tick_to(BASE_NS + GRACE_NS + 1);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_condemned, 1,
        "the blob is condemned once its tree is gone: {report:?}"
    );
    f.tick_to(BASE_NS + GRACE_NS + 2);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_collected, 1,
        "phase B deletes the condemned blob: {report:?}"
    );
    // The landed chain is intact: the rival's tip still serves y.txt, and
    // main has legitimately advanced onto the resolution.
    assert_eq!(
        f.cat(main_after_rival.as_str().unwrap(), "y.txt").await,
        b"rival-y".to_vec()
    );
    let main_now = f.main_hex();
    assert_ne!(main_now, main_after_rival, "the resolution landed on top");
    assert_eq!(
        f.cat(main_now.as_str().unwrap(), "z.txt").await,
        b"resolution-z".to_vec()
    );
}

// ---- 4. the re-reference race (the steer's pin) ---------------------------------

/// The steer's pin, touch leg: upload B; age it into a past-grace
/// candidate; START a pass (held between mark and sweep by the test hook);
/// re-PUT B (a CAS no-op) while the pass is parked; let the sweep run.
/// The row touch re-enters B into grace — B EXISTS after the sweep, and a
/// later changeset referencing B commits fine.
#[tokio::test]
async fn gc_rereference_race_reput_touch_spares_blob() {
    let f = fixture_with(|s| {
        s.gc_test_delay_after_mark = Duration::from_millis(300);
    });
    let content = b"raced-blob".to_vec();
    let id = f.put_blob(&content).await; // objects row touched at BASE

    // Age it into a past-grace candidate.
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 1_000);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob'"),
        1
    );

    // The pass parks between mark and sweep (300 ms hook); the re-PUT
    // lands in that window (the mark is one indexed walk over a handful of
    // rows — long since done by 100 ms).
    let state = Arc::clone(&f.t.state);
    let handle = tokio::spawn(async move { gc_pass(&state).await });
    tokio::time::sleep(Duration::from_millis(100)).await;
    let _ = f.put_blob(&content).await; // CAS no-op; touches objects.touched_ns

    let report = handle.await.unwrap().unwrap();
    assert_eq!(
        report.blobs_collected, 0,
        "the touch re-entered grace: {report:?}"
    );
    assert_eq!(
        f.t.state.cas.has(&[blob_id(&content)]).unwrap(),
        vec![true],
        "B EXISTS after the sweep"
    );

    // A changeset referencing B commits cleanly afterwards.
    let main_hex = f.main_hex();
    let parents = match &main_hex {
        Value::Null => json!([]),
        h => json!([h]),
    };
    let _ = f
        .create_ok(&cs_body(
            parents,
            json!([m4b_common::add("race.txt", &id, content.len())]),
            "race",
        ))
        .await;
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM file_changes WHERE blob_id = ?1",
            &[&blob_id(&content).0.as_slice()]
        ),
        1,
        "committed history references the spared blob"
    );
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.stale_candidates, 0, "the pass repaired the ledger");
}

/// The steer's pin, reference leg: a changeset referencing B commits WHILE
/// the pass is parked between mark and sweep — the in-transaction
/// re-verification (the selecting statement runs in the deleting
/// transaction, under the same single-writer slot as the commit) spares B.
#[tokio::test]
async fn gc_rereference_race_concurrent_changeset_spares_blob() {
    let f = fixture_with(|s| {
        s.gc_test_delay_after_mark = Duration::from_millis(300);
    });
    let content = b"raced-blob-2".to_vec();
    let id = f.put_blob(&content).await;
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 1_000);

    let state = Arc::clone(&f.t.state);
    let handle = tokio::spawn(async move { gc_pass(&state).await });
    tokio::time::sleep(Duration::from_millis(100)).await;
    let main_hex = f.main_hex();
    let parents = match &main_hex {
        Value::Null => json!([]),
        h => json!([h]),
    };
    let _ = f
        .create_ok(&cs_body(
            parents,
            json!([m4b_common::add("race2.txt", &id, content.len())]),
            "race2",
        ))
        .await;

    let report = handle.await.unwrap().unwrap();
    assert_eq!(
        report.blobs_collected, 0,
        "the reference spares B: {report:?}"
    );
    assert_eq!(f.t.state.cas.has(&[blob_id(&content)]).unwrap(), vec![true]);
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
}

// ---- 5. snapshots ----------------------------------------------------------------

/// Unexpired snapshots protect their (zero-junction) tree rows: the
/// reachability sweep never touches them (the M5 walk stays their owner).
/// An EXPIRED snapshot's tree collects after the grace window — the
/// expired snapshot's own row stays the M5 GC's business, not this
/// sweep's.
#[tokio::test]
async fn gc_unexpired_snapshot_trees_survive_expired_ones_collect() {
    use naatre_core::tree::{self, EntryKind, Tree, TreeEntry};

    let f = fixture();
    let (root, tree) = seed_base(&f).await;
    let resp = f.land(&root, "ks").await;
    assert_eq!(resp.status, StatusCode::OK, "{:?}", resp.body);

    // Rebuild the landed root's canonical bytes bottom-up (deterministic
    // encoding → the same tree id), the shape the snapshot POST carries.
    let file = |name: &str, blob: Id, size: u64| TreeEntry {
        name: name.to_string(),
        kind: EntryKind::File,
        object: blob,
        size,
        exec: false,
    };
    let dir = Tree {
        entries: vec![file("x.txt", blob_id(b"nested-x"), 8)],
    };
    let dir_id = tree::tree_id(&dir).unwrap();
    let root_tree = Tree {
        entries: vec![
            file("a.txt", blob_id(b"live-content-A"), 14),
            TreeEntry {
                name: "dir".to_string(),
                kind: EntryKind::Dir,
                object: dir_id,
                size: 8, // the dir entry's size is the recursive total
                exec: false,
            },
        ],
    };
    let root_id = tree::tree_id(&root_tree).unwrap();
    assert_eq!(
        root_id.to_hex(),
        tree,
        "the rebuilt tree IS the landed root"
    );
    let canonics: Vec<String> = [
        tree::encode(&root_tree).unwrap(),
        tree::encode(&dir).unwrap(),
    ]
    .into_iter()
    .map(|c| STANDARD.encode(c))
    .collect();

    // The snapshot POST validates every referenced blob under
    // ephemeral/<snap>/ — both blobs go up through the snapshot route.
    let snap_id = format!("{:062x}{:02x}", 0u128, 0xAB);
    for (blob, content) in [
        (blob_id(b"live-content-A"), b"live-content-A".to_vec()),
        (blob_id(b"nested-x"), b"nested-x".to_vec()),
    ] {
        let put = axum::http::Request::builder()
            .method("PUT")
            .uri(format!(
                "/v1/repos/{REPO}/snapshots/{snap_id}/objects/{}",
                blob.to_hex()
            ))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", f.writer),
            )
            .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
            .body(axum::body::Body::from(content))
            .unwrap();
        let res = f.t.app.clone().oneshot(put).await.unwrap();
        assert!(
            res.status().is_success(),
            "snapshot blob PUT: {}",
            res.status()
        );
    }
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        &f.writer,
        &json!({
            "snap_id": snap_id,
            "tree_id": tree,
            "trees": canonics,
            "ttl_secs": 3_600u64,
        }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{:?}", resp.body);
    let trees_before = f.count("SELECT COUNT(*) FROM trees");

    // Unexpired: the pass leaves every tree row alone.
    gc_pass(&f.t.state).await.unwrap();
    assert_eq!(f.count("SELECT COUNT(*) FROM trees"), trees_before);
    f.tick_to(BASE_NS + GRACE_NS * 2);
    gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        f.count("SELECT COUNT(*) FROM trees"),
        trees_before,
        "unexpired snapshot trees are never this sweep's business"
    );

    // A second snapshot whose trees history NEVER shared (a WIP state with
    // an extra file): zero-junction rows only this snapshot reaches.
    let wip_blob = blob_id(b"wip-content");
    let wip = Tree {
        entries: vec![
            file("a.txt", blob_id(b"live-content-A"), 14),
            TreeEntry {
                name: "dir".to_string(),
                kind: EntryKind::Dir,
                object: dir_id,
                size: 8,
                exec: false,
            },
            file("wip.txt", wip_blob, 11),
        ],
    };
    let wip_id = tree::tree_id(&wip).unwrap();
    let wip_canonics: Vec<String> = [tree::encode(&wip).unwrap(), tree::encode(&dir).unwrap()]
        .into_iter()
        .map(|c| STANDARD.encode(c))
        .collect();
    let snap2 = format!("{:062x}{:02x}", 0u128, 0xAC);
    // Each snapshot namespace is separate: every blob the WIP tree
    // references goes up under snap2.
    for (blob, content) in [
        (blob_id(b"live-content-A"), b"live-content-A".to_vec()),
        (blob_id(b"nested-x"), b"nested-x".to_vec()),
        (wip_blob, b"wip-content".to_vec()),
    ] {
        let put = axum::http::Request::builder()
            .method("PUT")
            .uri(format!(
                "/v1/repos/{REPO}/snapshots/{snap2}/objects/{}",
                blob.to_hex()
            ))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", f.writer),
            )
            .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
            .body(axum::body::Body::from(content))
            .unwrap();
        let res = f.t.app.clone().oneshot(put).await.unwrap();
        assert!(res.status().is_success(), "wip blob PUT: {}", res.status());
    }
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        &f.writer,
        &json!({
            "snap_id": snap2,
            "tree_id": wip_id.to_hex(),
            "trees": wip_canonics,
            "ttl_secs": 3_600u64,
        }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{:?}", resp.body);
    let trees_with_snapshots = f.count("SELECT COUNT(*) FROM trees");

    // Both snapshots expire; the M5 GC has not run. The unshared WIP
    // trees collect after the window; the history-shared rows stay.
    m4b_common::conn_at(&f.t.db_path)
        .execute("UPDATE snapshots SET expires_ns = 1", [])
        .unwrap();
    gc_pass(&f.t.state).await.unwrap(); // candidates (first_ns = BASE + 2G)
    f.tick_to(BASE_NS + GRACE_NS * 3 + 5);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert!(
        report.trees_collected >= 1,
        "the WIP trees collect: {report:?}"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM trees"),
        trees_with_snapshots - report.trees_collected as i64
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM trees WHERE id = ?1",
            &[&dir_id.0.as_slice()]
        ),
        1,
        "the history-shared dir tree stays"
    );
}

// ---- 6. advisory-only refcount ----------------------------------------------------

/// The lying-refcount pin: a repo_blobs row deleted for a LIVE blob cannot
/// doom it — the mark found it via file_changes — and the GC ignores the
/// broken advisory row harmlessly (fsck reports it; clean() stays true).
#[tokio::test]
async fn gc_lying_refcount_is_ignored_harmlessly() {
    let f = fixture();
    let (root, _) = seed_base(&f).await;
    let content = b"live-blob-lying-refcount".to_vec();
    let cs = f.commit(json!([root]), "l.txt", &content).await;
    let resp = f.land(&cs, "kl").await;
    assert_eq!(resp.status, StatusCode::OK, "{:?}", resp.body);

    // Lie: drop the membership row for the live blob.
    let oid = blob_id(&content);
    m4b_common::conn_at(&f.t.db_path)
        .execute(
            "DELETE FROM repo_blobs WHERE object_id = ?1",
            rusqlite::params![oid.0.as_slice()],
        )
        .unwrap();

    f.tick_to(BASE_NS + GRACE_NS * 2);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_collected, 0,
        "the mark never reads repo_blobs: {report:?}"
    );
    assert_eq!(
        f.t.state.cas.has(&[oid]).unwrap(),
        vec![true],
        "the live blob survives"
    );
    // The lie is NOT harmless to the READ path (the M1 membership gate
    // refuses the object) — exactly what fsck exists to surface.
    let main_hex = f.main_hex();
    let resp = get_on(
        &f.t.app,
        &format!(
            "/v1/repos/{REPO}/file?at={}&path=l.txt",
            main_hex.as_str().unwrap()
        ),
        &f.writer,
    )
    .await;
    let blob = resp.body["blob_id"].as_str().unwrap().to_string();
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(format!("/v1/repos/{REPO}/objects/{blob}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .body(axum::body::Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(
        res.status(),
        StatusCode::NOT_FOUND,
        "the membership gate refuses the lied-about blob"
    );

    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(
        report.advisory_membership_missing >= 1,
        "fsck reports the lie: {report:?}"
    );
    assert!(
        report.clean(),
        "advisory mismatches do not dirty the store: {report:?}"
    );
}

// ---- 7. batches + fsck --------------------------------------------------------------

/// Multi-batch convergence: more orphan blobs than one batch holds, all
/// collected across batches, live data untouched, fsck clean with the
/// audit totals matching.
#[tokio::test]
async fn gc_multi_batch_convergence_and_fsck_clean() {
    let f = fixture();
    let (root, _) = seed_base(&f).await;
    let live_content = b"the-live-one".to_vec();
    let live_cs = f.commit(json!([root]), "live.txt", &live_content).await;
    let resp = f.land(&live_cs, "kb").await;
    assert_eq!(resp.status, StatusCode::OK, "{:?}", resp.body);

    // 300 orphans (2+ blob batches at the 256-batch size): CAS bytes plus
    // objects rows, no references. Ledgered at the base clock, then aged
    // past the window.
    let mut ids = Vec::new();
    for i in 0..300u32 {
        let content = format!("orphan-{i:04}").into_bytes();
        let id = blob_id(&content);
        f.t.state.cas.put(&content).unwrap();
        m4b_common::conn_at(&f.t.db_path)
            .execute(
                "INSERT OR IGNORE INTO objects(object_id, size, csize, touched_ns) VALUES (?1, ?2, 1, ?3)",
                rusqlite::params![id.0.as_slice(), content.len() as i64, BASE_NS as i64],
            )
            .unwrap();
        ids.push(id);
    }
    gc_pass(&f.t.state).await.unwrap(); // ledger rows at first_ns = BASE

    f.tick_to(BASE_NS + GRACE_NS + 7);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_condemned, 300,
        "phase A stamps across batches: {report:?}"
    );
    assert_eq!(
        f.t.state.cas.has(&ids).unwrap(),
        vec![true; 300],
        "phase A keeps the bytes"
    );
    f.tick_to(BASE_NS + GRACE_NS + 8);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_collected, 300,
        "phase B deletes across batches: {report:?}"
    );
    assert_eq!(f.t.state.cas.has(&ids).unwrap(), vec![false; 300]);
    let objects_total = f.count("SELECT COUNT(*) FROM objects");
    let objects_with_pending: i64 = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT COUNT(*) FROM objects o WHERE EXISTS \
             (SELECT 1 FROM gc_pending p WHERE p.scope = 'blob' AND p.id = o.object_id)",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        objects_with_pending, 0,
        "no surviving objects row is still a candidate ({objects_total} rows)"
    );
    let main_hex = f.main_hex();
    let served = f.cat(main_hex.as_str().unwrap(), "live.txt").await;
    assert_eq!(served, live_content, "live survives the batches");

    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.gc_passes, 1);
    assert_eq!(report.collected, (0, 300, 0), "audit totals match the pass");
    assert_eq!(report.changesets_marked, report.changesets);
    assert_eq!(report.bytes_missing, 0);
}

// ---- 8. the missing-bytes direction -------------------------------------------------

/// A DB-known blob whose bytes are already absent (external drift or a
/// pre-M10 manual delete) reports in fsck as dirty, collects only its ROWS
/// past grace, and leaves fsck clean afterwards.
#[tokio::test]
async fn gc_missing_bytes_collect_rows_and_fsck_sees_the_drift() {
    let f = fixture();
    let id = object_id(Kind::Blob, b"drifted");
    // Row without bytes (as if the bucket lost it).
    m4b_common::conn_at(&f.t.db_path)
        .execute(
            "INSERT INTO objects(object_id, size, csize, touched_ns) VALUES (?1, 7, 1, ?2)",
            rusqlite::params![id.0.as_slice(), BASE_NS as i64],
        )
        .unwrap();
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert_eq!(report.bytes_missing, 1, "{report:?}");
    assert_eq!(report.cas_unrecorded, 0);
    assert!(!report.clean(), "missing bytes dirty the store");

    // Past grace the row is condemned, then collected (the CAS delete is
    // an idempotent no-op).
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 3);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.blobs_condemned, 1, "{report:?}");
    f.tick_to(BASE_NS + GRACE_NS + 4);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.blobs_collected, 1, "{report:?}");
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
}

// ---- 9. the byte-deletion races (two-phase delete + retryable record) --------

/// The upload racing the TWO-PHASE sweep survives without any error at all:
/// a full route PUT landing between phase A's condemnation and phase B's
/// delete (here: during the mark→sweep pause of the condemning pass)
/// refreshes the row touch — phase A's own selection then refuses the
/// touched candidate, and even a mark that did get stamped would be
/// cleared by the touch. B survives every pass; fsck is clean.
#[tokio::test]
async fn gc_route_put_between_phases_survives() {
    let f = fixture_with(|s| {
        s.gc_test_delay_after_mark = Duration::from_millis(300);
    });
    let content = b"between-phases-blob".to_vec();
    let oid = blob_id(&content);
    let id = f.put_blob(&content).await; // row touched at BASE
    let _ = id;

    // Age it into a past-grace candidate.
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 1_000);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob'"),
        1
    );

    // The condemning pass parks between mark and sweep; the route PUT
    // lands whole in that window (CAS step + record step).
    let state = Arc::clone(&f.t.state);
    let handle = tokio::spawn(async move { gc_pass(&state).await });
    tokio::time::sleep(Duration::from_millis(100)).await;
    let republished = f.put_blob(&content).await; // the real PUT route
    let _ = republished;
    let report = handle.await.unwrap().unwrap();
    assert_eq!(
        report.blobs_condemned, 0,
        "the touch refused phase A: {report:?}"
    );
    assert_eq!(report.blobs_collected, 0, "{report:?}");

    // A later pass does not resurrect the race: still touched-inside-grace.
    f.tick_to(BASE_NS + GRACE_NS * 2);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_condemned + report.blobs_collected,
        0,
        "{report:?}"
    );
    assert_eq!(f.t.state.cas.has(&[oid]).unwrap(), vec![true], "B EXISTS");
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
}

/// The steer's pin: PUT existing B; the whole sweep runs over TWO passes
/// (phase A condemns, phase B deletes bytes + row); THEN the upload's
/// record step runs — it answers the RETRYABLE 503 `blob_swept` (the row
/// is never written over absent bytes), and a successful re-PUT through
/// the real route leaves B present + recorded.
#[tokio::test]
async fn gc_record_step_after_full_sweep_answers_blob_swept() {
    let f = fixture();
    let content = b"record-step-blob".to_vec();
    let oid = blob_id(&content);
    let _id = f.put_blob(&content).await; // row touched at BASE

    // Age, condemn, delete: the full sweep.
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 1_000);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.blobs_condemned, 1, "phase A stamps: {report:?}");
    assert_eq!(
        f.t.state.cas.has(&[oid]).unwrap(),
        vec![true],
        "phase A keeps the bytes"
    );
    f.tick_to(BASE_NS + GRACE_NS + 2_000);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.blobs_collected, 1, "phase B deletes: {report:?}");
    assert_eq!(f.t.state.cas.has(&[oid]).unwrap(), vec![false]);
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&oid.0.as_slice()]
        ),
        0,
        "the row went with the bytes"
    );

    // The racing upload's record step: retryable 503 blob_swept, row never
    // written.
    let err = naatre_server::routes::objects::record_object(
        &f.t.state,
        f.repo_id,
        REPO,
        oid,
        content.len() as i64,
        1,
    )
    .await
    .unwrap_err();
    assert_eq!(
        err.code(),
        naatre_server::api_code::ApiCode::BlobSwept,
        "{err:?}"
    );
    assert!(err.is_retryable(), "{err:?}");
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&oid.0.as_slice()]
        ),
        0,
        "NEVER a row referencing missing bytes"
    );

    // The client's re-PUT (the real route): bytes land, the row records,
    // fsck is clean.
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/{REPO}/objects/{}", oid.to_hex()))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(content.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(
        res.status(),
        axum::http::StatusCode::CREATED,
        "the re-PUT lands fresh"
    );
    assert_eq!(f.t.state.cas.has(&[oid]).unwrap(), vec![true]);
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.bytes_missing, 0);
}

/// The batch route's record leg answers the same retryable 503 `blob_swept`
/// naming the swept ids, records NO entry's row, and a raw re-PUT of the
/// swept blob lands it (the batch is all-or-nothing on the row side).
#[tokio::test]
async fn gc_batch_put_after_sweep_answers_blob_swept() {
    let f = fixture();
    let content = b"batch-swept-blob".to_vec();
    let oid = blob_id(&content);
    let _id = f.put_blob(&content).await;

    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 1_000);
    gc_pass(&f.t.state).await.unwrap(); // condemns
    f.tick_to(BASE_NS + GRACE_NS + 2_000);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.blobs_collected, 1, "{report:?}");

    // The batch route carrying the DATA re-stores the bytes by
    // construction — its CAS step runs before the record leg's has() check,
    // so a post-sweep batch PUT answers 200 with fresh bytes (the honest
    // recovery), never a stale 200 over the OLD bytes.
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/objects"),
        &f.writer,
        &json!({ "objects": [ { "id": oid.to_hex(),
                                "data_b64": STANDARD.encode(&content) } ] }),
    )
    .await;
    assert_eq!(resp.status, axum::http::StatusCode::OK, "{}", resp.body);

    // The record LEG's race boundary, driven at its function: bytes absent
    // (sweep again, and this time skip the route's CAS step) → one
    // retryable 503 `blob_swept` naming the id, NO row written.
    m4b_common::conn_at(&f.t.db_path)
        .execute(
            "DELETE FROM objects WHERE object_id = ?1",
            rusqlite::params![oid.0.as_slice()],
        )
        .unwrap();
    f.t.state.cas.delete(&[oid]).unwrap();
    gc_pass(&f.t.state).await.unwrap(); // ledger
    f.tick_to(BASE_NS + GRACE_NS + 3_000);
    gc_pass(&f.t.state).await.unwrap(); // condemn
    f.tick_to(BASE_NS + GRACE_NS + 4_000);
    gc_pass(&f.t.state).await.unwrap(); // delete
    let err = naatre_server::routes::objects::record_batch(
        &f.t.state,
        f.repo_id,
        REPO,
        vec![(oid, content.len() as i64, 1)],
    )
    .await
    .unwrap_err();
    assert_eq!(
        err.code(),
        naatre_server::api_code::ApiCode::BlobSwept,
        "{err:?}"
    );
    assert!(err.is_retryable(), "{err:?}");
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&oid.0.as_slice()]
        ),
        0,
        "the batch recorded no row over absent bytes"
    );

    // The raw re-PUT lands it; fsck clean.
    let _ = f.put_blob(&content).await;
    assert_eq!(f.t.state.cas.has(&[oid]).unwrap(), vec![true]);
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
}

// ---- 10. the phase-B protection pin (condemn → PUT → no collect) ------------

/// The two-phase delete's protection gate, PINNED END TO END: phase A
/// condemns in pass N; a full ROUTE PUT then lands (its record step's
/// `UPDATE gc_pending SET condemned_at = NULL` is the ONLY thing that
/// unmarks a condemned row); pass N+1 must NOT collect — no phase A
/// re-stamp (the touch re-entered grace) and no phase B delete (the mark
/// is cleared). Without the clear, this exact sequence deletes live
/// bytes.
#[tokio::test]
async fn gc_route_put_between_condemn_and_phase_b_spares_the_blob() {
    let f = fixture();
    let content = b"condemn-then-put".to_vec();
    let oid = blob_id(&content);
    let _id = f.put_blob(&content).await; // row touched at BASE

    // Pass 1 ledgers the orphan; the aged pass CONDEMNS it (bytes stay).
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS + 1_000);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.blobs_condemned, 1, "phase A stamps: {report:?}");
    assert_eq!(
        f.count(
            "SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob' \
             AND condemned_at IS NOT NULL"
        ),
        1,
        "the mark is set"
    );

    // The route PUT lands whole BETWEEN the phases (pass N's condemn vs a
    // later pass's phase B): the record step clears the mark and refreshes
    // the touch in one transaction.
    let _ = f.put_blob(&content).await;
    assert_eq!(
        f.count(
            "SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob' \
             AND condemned_at IS NULL"
        ),
        1,
        "the PUT's condemned-clear UPDATE is the gate"
    );

    // Pass N+1 must NOT collect: no re-stamp (touched inside the window)
    // and no delete (nothing is condemned anymore).
    f.tick_to(BASE_NS + GRACE_NS + 2_000);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.blobs_condemned + report.blobs_collected,
        0,
        "the cleared mark and the fresh touch spare B: {report:?}"
    );
    assert_eq!(
        f.t.state.cas.has(&[oid]).unwrap(),
        vec![true],
        "the bytes survive"
    );
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
}

// ---- 11. GC while the derive queue is non-empty (the plan's leg) -------------

/// A do-nothing deferred kind: its pending row exists (queue non-empty)
/// without any derivation changing the store.
struct NullDeriver;

impl naatre_server::derive::Deriver for NullDeriver {
    fn kind(&self) -> &'static str {
        "m10a-queue-probe"
    }

    fn derive(
        &self,
        _ctx: &naatre_server::derive::DeriveCtx,
        _conn: &naatre_store::Db,
        _repo_id: i64,
        _cs: Id,
        _parents: &[naatre_server::derive::ParentOut],
    ) -> Result<naatre_server::derive::DerivedOut, naatre_server::derive::DeriveFailure> {
        Ok(naatre_server::derive::DerivedOut::empty())
    }
}

/// The plan's M10 check leg — "GC … while the derive queue is non-empty":
/// a pending probe row AND a LIVE LEASE (state='deriving') on the changeset
/// the pass collects. The lease is an advisory M6 row and protects nothing:
/// the pass collects the changeset, its queue rows go with it, and the
/// queue's LATE record steps ([`naatre_server::db::derive_record_done`] /
/// [`naatre_server::db::derive_fail_attempt`]) must neither resurrect a
/// queue row for the dead changeset nor leave an fsck dangling — each
/// re-checks the changeset's existence in its own slot-held transaction.
#[tokio::test]
async fn gc_collects_while_derive_queue_holds_leases() {
    let f = fixture_with(|s| {
        s.register_deriver(std::sync::Arc::new(NullDeriver))
            .unwrap();
    });
    let (root, _) = seed_base(&f).await;
    let orphan = f.commit(json!([root]), "q.txt", b"queued-content").await;
    // The create enqueued the probe kind's pending row for the orphan (and
    // one for the root).
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM derive_state ds \
             JOIN derive_kinds k ON k.id = ds.kind \
             WHERE k.name = 'm10a-queue-probe' AND ds.state = 'pending' AND ds.cs = ?1",
            &[&hex_id(&orphan).0.as_slice()]
        ),
        1,
        "the queue is non-empty"
    );
    // A LIVE lease on the orphan (a derivation in flight): lease_expires
    // far in the future, so no sweep would reclaim it during the pass.
    m4b_common::conn_at(&f.t.db_path)
        .execute(
            "UPDATE derive_state SET state = 'deriving', lease_expires_ns = ?1 \
             WHERE cs = ?2",
            rusqlite::params![
                (BASE_NS as i64) + (GRACE_NS * 10) as i64,
                hex_id(&orphan).0.as_slice()
            ],
        )
        .unwrap();

    // The ledger pass runs at the base clock (first-seen = BASE); the
    // collecting pass runs past the grace window.
    gc_pass(&f.t.state).await.unwrap();
    f.tick_to(BASE_NS + GRACE_NS);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.cs_collected, 1,
        "the lease is advisory and protects nothing: {report:?}"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM derive_state WHERE cs = ?1",
            &[&hex_id(&orphan).0.as_slice()]
        ),
        0,
        "the queue row went with the changeset"
    );

    // The queue's late record steps, replayed against the collected node:
    // each returns false (nothing recorded) and creates NO row.
    let db = naatre_store::Db::Sqlite(m4b_common::conn_at(&f.t.db_path));
    let kind_id: i64 = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT id FROM derive_kinds WHERE name = 'm10a-queue-probe'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert!(
        !naatre_server::db::derive_record_done(
            &db,
            kind_id,
            f.repo_id,
            &hex_id(&orphan),
            b"out",
            false,
            None,
        )
        .unwrap(),
        "no done-record over a collected changeset"
    );
    assert!(
        !naatre_server::db::derive_fail_attempt(
            &db,
            kind_id,
            f.repo_id,
            &hex_id(&orphan),
            "late attempt against a collected node",
            BASE_NS as i64,
        )
        .unwrap(),
        "no attempt recorded over a collected changeset"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM derive_state WHERE cs = ?1",
            &[&hex_id(&orphan).0.as_slice()]
        ),
        0,
        "no resurrection"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM derive_reports"),
        0,
        "no poison report"
    );

    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
}
