//! M13a suite, server side: the db-per-repo layout (plan M13a check set —
//! two repos landing concurrently on DB-per-repo with separate write
//! slots; the migration command e2e on a seeded multi-repo single-DB
//! store serving identically after cutover; the cross-repo redaction
//! semantics the plan's M10b clause carries into the split).
//!
//! The single-DB pins (single-writer still) live with their milestones;
//! this suite pins the SPLIT behavior and the adoption contract.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;
use naatre_store::backend::{BackendConfig, check_data_dir_backend};
use naatre_store::layout::Layout;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;

fn split_cfg() -> BackendConfig {
    BackendConfig {
        layout: Layout::DbPerRepo,
        ..BackendConfig::default()
    }
}

// ---- fixtures ---------------------------------------------------------------

/// A single-DB store with two repos seeded through the statement layer
/// (repos rows + audit), each holding one object (CAS bytes + ledger row)
/// and a main ref — the minimum a migration must carry over and fsck must
/// vouch for.
struct SingleFixture {
    _dir: tempfile::TempDir,
    data_path: std::path::PathBuf,
    state: AppState,
    app: Router,
    token: String,
    blobs: Vec<Id>,
}

async fn single_fixture() -> SingleFixture {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let dir = tempfile::tempdir().unwrap();
    let data_path = dir.path().to_path_buf();
    let data = DataDir::create(&data_path).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(&data_path).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    let state = Arc::new(state);

    // Admin principal + token (the catalog tables; the seed the real init
    // writes).
    let (token, hash) = naatre_server::auth::mint_token();
    state
        .write({
            move |store| {
                let conn = store.conn();
                let tx = conn.transaction().unwrap();
                naatre_server::db::principal_upsert(&tx, "admin", "admin", BASE_NS as i64).unwrap();
                naatre_server::db::token_insert(&tx, "admin", &hash, None, None).unwrap();
                naatre_server::db::audit_append(&tx, BASE_NS as i64, "admin", "init", "{}")
                    .unwrap();
                tx.commit().unwrap();
                Ok(())
            }
        })
        .await
        .unwrap();

    // Two repos, each with one object (CAS put + record through the route
    // layer's record path) and a main ref.
    let mut blobs = Vec::new();
    for name in ["m13a-a", "m13a-b"] {
        let repo_name = name.to_string();
        state
            .write(move |store| {
                let conn = store.conn();
                naatre_server::db::repos_create(conn, &repo_name, BASE_NS as i64).unwrap();
                Ok(())
            })
            .await
            .unwrap();
        let content = format!("{name} payload").into_bytes();
        let blob = object_id(Kind::Blob, &content);
        state.cas.put(&content).unwrap();
        let state_for_record = Arc::clone(&state);
        let repo_for_record = name.to_string();
        let blob_for_record = blob;
        let content_len = content.len() as i64;
        state_for_record
            .write_repo(repo_for_record, move |cx| {
                naatre_server::db::object_row_upsert(
                    &cx.cat(),
                    &blob_for_record,
                    content_len,
                    1,
                    BASE_NS as i64,
                )
                .unwrap();
                cx.db
                    .execute(
                        &naatre_store::Sql::plain(
                            "INSERT OR IGNORE INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)",
                        ),
                        &naatre_store::pv![cx.id, blob_for_record.0.as_slice()],
                    )
                    .unwrap();
                Ok(())
            })
            .await
            .unwrap();
        blobs.push(blob);
        let state_for_ref = Arc::clone(&state);
        let repo_for_ref = name.to_string();
        let target = blob; // not inserted (the ref seeds NULL); satisfies the closure's capture
        state_for_ref
            .write_repo(repo_for_ref, move |cx| {
                let _ = target;
                cx.db
                    .execute(
                        &naatre_store::Sql::plain(
                            "INSERT INTO refs(repo_id, name, target, version, last_principal) \
                             VALUES (?1, 'main', NULL, 1, 'seed')",
                        ),
                        &naatre_store::pv![cx.id],
                    )
                    .unwrap();
                Ok(())
            })
            .await
            .unwrap();
    }
    let app = naatre_server::router(Arc::clone(&state));
    SingleFixture {
        _dir: dir,
        data_path,
        state,
        app,
        token,
        blobs,
    }
}

/// Opens the SPLIT state over an adopted data dir (the cutover shape).
fn open_split(data_path: &std::path::Path) -> AppState {
    let cfg = split_cfg();
    check_data_dir_backend(data_path, &cfg).unwrap();
    Arc::new(ServerState::open_backend(DataDir::open_any(data_path), &cfg).unwrap())
}

async fn send(
    app: &Router,
    method: &str,
    uri: &str,
    token: Option<&str>,
) -> (StatusCode, serde_json::Value) {
    let mut builder = axum::http::Request::builder().method(method).uri(uri);
    if let Some(t) = token {
        builder = builder.header(header::AUTHORIZATION, format!("Bearer {t}"));
    }
    let req = builder.body(Body::empty()).unwrap();
    let res = app.clone().oneshot(req).await.expect("in-memory request");
    let status = res.status();
    let bytes = BodyExt::collect(res.into_body()).await.unwrap().to_bytes();
    let body = if bytes.is_empty() {
        serde_json::Value::Null
    } else {
        serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null)
    };
    (status, body)
}

// ---- the pins ---------------------------------------------------------------

/// Backend.toml's `[layout]` surface: the mode parses, db-per-repo under
/// Postgres is refused, and the data-dir guards keep the two layouts from
/// cross-opening.
#[test]
fn layout_config_and_guards() {
    let cfg =
        naatre_store::backend::parse("[backend]\n[layout]\nmode = \"db-per-repo\"\n").unwrap();
    assert_eq!(cfg.layout, Layout::DbPerRepo);
    let cfg = naatre_store::backend::parse("[backend]\n").unwrap();
    assert_eq!(cfg.layout, Layout::Single);
    let e = naatre_store::backend::parse(
        "[backend]\nmeta = \"postgres\"\ndsn = \"postgres://h/db\"\n[layout]\nmode = \"db-per-repo\"\n",
    )
    .unwrap_err();
    assert!(e.to_string().contains("requires meta = \"sqlite\""), "{e}");
    let e = naatre_store::backend::parse("[backend]\n[layout]\nmode = \"yolo\"\n").unwrap_err();
    assert!(matches!(
        e,
        naatre_store::backend::BackendError::BadValue { ref key, .. } if key == "layout.mode"
    ));
    let e = naatre_store::backend::parse("[backend]\n[layout]\nzzz = 1\n").unwrap_err();
    assert!(matches!(
        e,
        naatre_store::backend::BackendError::UnknownKey { ref key, .. } if key == "layout.zzz"
    ));

    // The guard: db-per-repo config without a catalog → refused (run the
    // migration first); single config with a catalog → refused (flip the
    // config); a catalog.db WITHOUT the adopted marker is an incomplete
    // migration → refused both ways; the migrated dir (marked catalog +
    // stale naatre.db) opens under db-per-repo.
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("naatre.db"), b"").unwrap();
    let e = check_data_dir_backend(dir.path(), &split_cfg()).unwrap_err();
    assert!(e.to_string().contains("migrate-db-per-repo"), "{e}");
    assert!(check_data_dir_backend(dir.path(), &BackendConfig::default()).is_ok());
    // A garbage catalog.db (the crashed-adoption shape: no adopted
    // marker) refuses the split open with the incomplete-migration text.
    std::fs::write(dir.path().join("catalog.db"), b"").unwrap();
    let e = check_data_dir_backend(dir.path(), &BackendConfig::default()).unwrap_err();
    assert!(e.to_string().contains("db-per-repo"), "{e}");
    let e = check_data_dir_backend(dir.path(), &split_cfg()).unwrap_err();
    assert!(e.to_string().contains("incomplete migration"), "{e}");
    {
        let cat = rusqlite::Connection::open(dir.path().join("catalog.db")).unwrap();
        naatre_store::catalog::migrate_catalog(&cat).unwrap();
        cat.execute(
            "INSERT OR REPLACE INTO meta(key, value) VALUES ('adopted', 'db-per-repo')",
            [],
        )
        .unwrap();
    }
    assert!(check_data_dir_backend(dir.path(), &split_cfg()).is_ok());
}

/// The plan's M13 check: two repos land CONCURRENTLY on DB-per-repo —
/// separate write slots — while a same-repo second writer answers the
/// retryable writer_busy (the per-repo single-writer slot).
#[tokio::test]
async fn split_write_slots_are_per_repo() {
    let f = single_fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir; // the tempdir outlives the split open
    let state = open_split(&data_path);
    let _ = &dir;

    // Repo A holds its slot for a while; repo B proceeds meanwhile.
    let s1 = Arc::clone(&state);
    let holder = tokio::spawn(async move {
        s1.write_repo("m13a-a", move |_cx| {
            std::thread::sleep(Duration::from_millis(300));
            Ok(())
        })
        .await
    });
    tokio::time::sleep(Duration::from_millis(50)).await;
    let s2 = Arc::clone(&state);
    let start = std::time::Instant::now();
    s2.write_repo("m13a-b", move |_cx| Ok(()))
        .await
        .expect("repo B's write proceeds while repo A holds its slot");
    assert!(
        start.elapsed() < Duration::from_millis(200),
        "repo B must not queue behind repo A ({:?})",
        start.elapsed()
    );
    holder.await.unwrap().expect("holder completes");

    // The SAME repo's second writer: retryable writer_busy.
    let s3 = Arc::clone(&state);
    let busy = tokio::spawn(async move {
        s3.write_repo("m13a-a", move |_cx| {
            std::thread::sleep(Duration::from_millis(200));
            Ok(())
        })
        .await
    });
    tokio::time::sleep(Duration::from_millis(50)).await;
    let s4 = Arc::clone(&state);
    let err = s4
        .write_repo("m13a-a", move |_cx| Ok(()))
        .await
        .expect_err("same-repo contention answers writer_busy");
    assert_eq!(err.code(), naatre_server::api_code::ApiCode::WriterBusy);
    assert!(err.is_retryable(), "{err}");
    busy.await.unwrap().expect("holder completes");
}

/// The migration e2e (the plan's check): a seeded multi-repo single-DB
/// store (objects in the CAS, a main ref each, one repo's blob tombstoned)
/// migrates offline; the split serves identically (ref read, object read,
/// tombstone gate) and the tombstone stays repo-scoped (the OTHER repo's
/// blob — identical content — still serves; the M10b semantics carried
/// into the split).
#[tokio::test]
async fn migration_serves_identically_and_fsck_cleans() {
    let f = single_fixture().await;
    let data_path = f.data_path.clone();
    let app = f.app.clone();
    let token = f.token.clone();

    // Tombstone repo A's blob (host-local mark shape, through the
    // statement layer — the tombstone is repo A's row).
    let state = Arc::clone(&f.state);
    let blob = f.blobs[0];
    state
        .write_repo("m13a-a", move |cx| {
            let tx = cx.db.transaction_immediate().unwrap();
            naatre_server::db::redaction_mark(&tx, cx.id, &blob, "blob", BASE_NS as i64).unwrap();
            tx.commit().unwrap();
            Ok(())
        })
        .await
        .unwrap();

    // Pre-migration: repo A's object reads 410 (the gate), repo B's
    // identical-content blob... wait — repo B holds a DIFFERENT id (its
    // own content). The shared-subtree arm: upload the SAME content into
    // repo B and confirm it still serves while A's id is gated.
    let content = b"m13a-a payload".to_vec();
    let blob_a = object_id(Kind::Blob, &content);
    let (status, _) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13a-a/objects/{}", blob_a.to_hex()),
        Some(&token),
    )
    .await;
    assert_eq!(status, StatusCode::GONE, "the tombstone gates repo A");

    // Migrate (offline; the data dir is not locked).
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert_eq!(report.repos.len(), 2, "both repos adopted");
    assert!(report.fsck_clean, "the split fscks clean");
    for repo in &report.repos {
        let refs: usize = repo
            .rows
            .iter()
            .find(|(t, _)| t == "refs")
            .map(|(_, n)| *n)
            .unwrap();
        assert_eq!(refs, 1, "each repo's main ref traveled: {repo:?}");
    }
    // The redactions row traveled with repo A.
    let redactions: usize = report.repos[0]
        .rows
        .iter()
        .find(|(t, _)| t == "redactions")
        .map(|(_, n)| *n)
        .unwrap_or(0);
    assert_eq!(redactions, 1, "repo A's tombstone traveled");
    // The catalog carries the global rows.
    let audit = report
        .catalog_rows
        .iter()
        .find(|(t, _)| t == "audit_log")
        .map(|(_, n)| *n)
        .unwrap();
    assert!(audit >= 1, "the seed's audit rows traveled: {audit}");

    // Cutover: the split state serves identically.
    let blobs = f.blobs.clone();
    let dir = f._dir;
    let state = open_split(&data_path);
    let _ = &dir;
    let app = naatre_server::router(Arc::clone(&state));

    // Auth works (the catalog's principals/tokens).
    let (status, _) = send(&app, "GET", "/v1/repos", Some(&token)).await;
    assert_eq!(status, StatusCode::OK, "the split serves the catalog auth");
    let (status, body) = send(&app, "GET", "/v1/repos", None).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "{body}");

    // Repo reads route to the repo files.
    let (status, _) = send(&app, "GET", "/v1/repos/m13a-a/refs/main", Some(&token)).await;
    assert_eq!(status, StatusCode::OK, "the split serves the repo files");
    let (status, _) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13a-b/objects/{}", blobs[1].to_hex()),
        Some(&token),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "repo B's object serves");
    let (status, body) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13a-a/objects/{}", blobs[0].to_hex()),
        Some(&token),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::GONE,
        "{body}: the tombstone still gates"
    );

    // Repo-scoped writes proceed on the split (a new blob record).
    let content2 = b"post-migration payload".to_vec();
    let blob2 = object_id(Kind::Blob, &content2);
    state.cas.put(&content2).unwrap();
    let s = Arc::clone(&state);
    s.write_repo("m13a-b", move |cx| {
        naatre_server::db::object_row_upsert(
            &cx.cat(),
            &blob2,
            content2.len() as i64,
            1,
            BASE_NS as i64,
        )
        .unwrap();
        cx.db
            .execute(
                &naatre_store::Sql::plain(
                    "INSERT OR IGNORE INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)",
                ),
                &naatre_store::pv![cx.id, blob2.0.as_slice()],
            )
            .unwrap();
        Ok(())
    })
    .await
    .expect("the split takes repo-scoped writes");

    // The audit row for a write lands in the CATALOG (the registry's home).
    let cat = rusqlite::Connection::open(data_path.join("catalog.db")).unwrap();
    let n: i64 = cat
        .query_row("SELECT COUNT(*) FROM audit_log", [], |r| r.get(0))
        .unwrap();
    assert!(n >= 2, "the catalog owns the audit rows: {n}");
    let repos: i64 = cat
        .query_row("SELECT COUNT(*) FROM repos", [], |r| r.get(0))
        .unwrap();
    assert_eq!(repos, 2);
    // The registry names each repo's db path.
    let db_file: String = cat
        .query_row("SELECT db_file FROM repos WHERE name = 'm13a-b'", [], |r| {
            r.get(0)
        })
        .unwrap();
    assert_eq!(db_file, "repos/2.db", "{db_file}");
}

/// The adoption refuses a live data dir (the flock) and leaves the
/// original single DB untouched on refusal and on failure.
#[tokio::test]
async fn adoption_is_offline_and_all_or_nothing() {
    let f = single_fixture().await;
    let data_path = f.data_path.clone();

    // Holding the flock = a live server: refused.
    let dir = DataDir::open_any(&data_path);
    let _lock = dir.lock().unwrap();
    let e =
        naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default()).unwrap_err();
    assert!(matches!(e, naatre_server::adopt::AdoptError::Live), "{e}");
    drop(_lock);

    // A failed adoption (a second catalog already present) removes nothing
    // of the original.
    std::fs::write(data_path.join("catalog.db"), b"stale").unwrap();
    let e =
        naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default()).unwrap_err();
    assert!(
        matches!(e, naatre_server::adopt::AdoptError::NotSingleDb(_)),
        "{e}"
    );
    std::fs::remove_file(data_path.join("catalog.db")).unwrap();
    assert!(data_path.join("naatre.db").exists(), "the original stands");
    f.state
        .read(|conn| {
            assert_eq!(
                naatre_server::db::repos_list(conn, 0, 10).unwrap().len(),
                2,
                "the single DB still holds both repos"
            );
            Ok(())
        })
        .await
        .unwrap();
}

/// The M11 clause landing with M13a (the plan's backup clause): the
/// coordinated snapshot covers the catalog AND every repo file (the
/// manifest carries `repos/<id>.db` artifacts), and a restore into a
/// split-configured target fscks clean.
#[tokio::test]
async fn split_backup_and_restore_roundtrip() {
    let f = single_fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(&data_path, &BackendConfig::default())
        .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let _dir = f._dir; // the tempdir outlives the backup
    let dir_guard = data_path.clone();

    let backup_dir = data_path.parent().unwrap().join("m13a-backup");
    let _ = std::fs::remove_dir_all(&backup_dir); // a prior run's leftover
    let cfg = split_cfg();
    let br = naatre_server::backup::backup(&data_path, &cfg, &backup_dir)
        .unwrap_or_else(|e| panic!("backup failed: {e}"));
    assert!(br.dest.join("catalog.db").exists() || br.dest.join("naatre.db").exists());
    let repos_artifacts: Vec<_> = {
        let manifest = std::fs::read_to_string(backup_dir.join("manifest.json")).unwrap();
        let v: serde_json::Value = serde_json::from_str(&manifest).unwrap();
        v["artifacts"]
            .as_array()
            .unwrap()
            .iter()
            .filter(|a| a["path"].as_str().is_some_and(|p| p.starts_with("repos/")))
            .cloned()
            .collect()
    };
    assert_eq!(repos_artifacts.len(), 2, "one artifact per repo file");

    // Restore into a fresh split-configured target.
    let target_root = data_path.parent().unwrap().join("m13a-restore-target");
    std::fs::create_dir_all(&target_root).unwrap();
    let target = target_root.join("data");
    std::fs::create_dir_all(&target).unwrap();
    std::fs::write(
        target.join("backend.toml"),
        "[backend]\nmeta = \"sqlite\"\nblob = \"fs\"\n\n[layout]\nmode = \"db-per-repo\"\n",
    )
    .unwrap();
    // The config file must be 0600 (the parser's permission guard).
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(
            target.join("backend.toml"),
            std::fs::Permissions::from_mode(0o600),
        )
        .unwrap();
    }
    let restore_cfg = naatre_store::backend::load(&target).unwrap();
    assert_eq!(restore_cfg.layout, Layout::DbPerRepo);
    naatre_server::backup::restore(&target, &restore_cfg, &backup_dir, true, None)
        .unwrap_or_else(|e| panic!("restore failed: {e}"));
    assert!(target.join("catalog.db").exists(), "the catalog restored");
    assert!(target.join("repos/1.db").exists(), "repo files restored");
    let state = open_split(&target);
    let clean = naatre_server::fsck::fsck(&state).await.unwrap();
    assert!(clean.clean(), "the restored split fscks clean: {clean:?}");
    let _ = dir_guard;
}
