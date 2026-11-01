//! M11 on the zero-config combo (SQLite + fs): the plan's backup → wipe →
//! restore → fsck-clean e2e as a REAL run, the manifest/artifact contract,
//! every restore refusal (combo mismatch, live target, incomplete backup,
//! bucket duty n/a here), and the restore-of-pre-redaction-backup operator
//! duty — the restored DB predates the tombstones, so the content comes
//! back and the operator re-marks the redactions, which gate again and the
//! next GC pass re-purges. The PG/S3 combos run the same shape in
//! m11_backup_pg_test / m11_backup_s3_test.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Arc;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::backup::{self, BackupError};
use naatre_server::fsck;
use naatre_server::gc::gc_pass;
use naatre_server::state::ServerState;
use naatre_server::{db, import};
use naatre_store::{BackendConfig, DataDir, Store};

// ---- git availability (the import_test rule: never pass silently) ---------

fn git_works() -> bool {
    match Command::new("git").arg("--version").output() {
        Ok(out) if out.status.success() => true,
        Ok(out) => {
            eprintln!(
                "skipping m11 backup tests: git --version failed: {}",
                String::from_utf8_lossy(&out.stderr)
            );
            false
        }
        Err(e) => {
            eprintln!("skipping m11 backup tests: git not available: {e}");
            false
        }
    }
}

// ---- fixture: a real git repo + an imported data dir -----------------------

struct Fixture {
    _dir: tempfile::TempDir,
    path: PathBuf,
}

impl Fixture {
    fn new(tag: &str) -> Fixture {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(tag);
        let out = Command::new("git")
            .args(["-c", "init.defaultBranch=main", "init"])
            .arg("-q")
            .arg(&path)
            .output()
            .expect("run git init");
        assert!(
            out.status.success(),
            "git init failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        Fixture { _dir: dir, path }
    }

    fn git(&self, args: &[&str]) -> String {
        let out = Command::new("git")
            .args([
                "-c",
                "user.name=Fix Tur e",
                "-c",
                "user.email=fix@example.com",
            ])
            .arg("-C")
            .arg(&self.path)
            .args(args)
            .env("GIT_AUTHOR_DATE", "1767225600 +0000")
            .env("GIT_COMMITTER_DATE", "1767225600 +0000")
            .output()
            .expect("run git");
        assert!(
            out.status.success(),
            "git {args:?} failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

    fn commit(&self, rel: &str, content: &[u8], msg: &str) {
        std::fs::create_dir_all(self.path.join(rel).parent().unwrap()).unwrap();
        std::fs::write(self.path.join(rel), content).unwrap();
        self.git(&["add", "-A"]);
        self.git(&["commit", "-q", "--cleanup=verbatim", "-m", msg]);
    }
}

/// A seeded data dir: `naatre init`'s shape (empty db file + migrations via
/// the store open) plus one imported commit carrying a file.
struct Data {
    _root: tempfile::TempDir,
    data: PathBuf,
    blob: Id,
}

fn seed(tag: &str) -> Data {
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    let f = Fixture::new(tag);
    f.commit("dir/file.txt", b"backup me\n", "seed commit");
    std::fs::create_dir_all(&data).unwrap();
    std::fs::write(data.join("naatre.db"), b"").unwrap();
    let cfg = BackendConfig::default();
    let opts = import::ImportOptions {
        git_path: f.path.clone(),
        into: Some("demo".into()),
        skip_unsupported: false,
        sanitize: false,
    };
    let report = import::run_backend(&data, &cfg, &opts, &Default::default()).unwrap();
    assert_eq!(
        report.status, "ok",
        "seed import aborted: {:?}",
        report.reason
    );
    let dd = DataDir::open_any(&data);
    let store = Store::open_backend(dd, &cfg).unwrap();
    let blob = object_id(Kind::Blob, b"backup me\n");
    assert!(
        store
            .conn()
            .query_opt(
                &naatre_store::Sql::plain("SELECT id FROM repos WHERE name = 'demo'"),
                &[],
            )
            .unwrap()
            .is_some()
    );
    Data {
        _root: root,
        data,
        blob,
    }
}

fn cfg() -> BackendConfig {
    BackendConfig::default()
}

async fn fsck_clean(data: &Path) -> bool {
    let state =
        Arc::new(ServerState::open_backend(DataDir::open_any(data), &cfg()).expect("state opens"));
    fsck::fsck(&state).await.expect("fsck runs").clean()
}

fn cas(data: &Path, config: &BackendConfig) -> Arc<dyn naatre_store::BlobStore> {
    naatre_store::blob::history_cas(&DataDir::open_any(data), config).unwrap()
}

// ---- pins ------------------------------------------------------------------

#[tokio::test]
async fn backup_manifest_and_artifacts() {
    if !git_works() {
        return;
    }
    let d = seed("m11bk-artifacts");
    let dest = d._root.path().join("backup");

    let report = backup::backup(&d.data, &cfg(), &dest).unwrap();
    assert_eq!(report.meta, naatre_store::MetaKind::Sqlite);
    assert_eq!(report.blob, naatre_store::BlobKind::Fs);
    assert_eq!(report.schema_version, naatre_store::MAX_SUPPORTED);
    assert!(!report.bucket_copy_duty);
    assert!(report.counts.changesets >= 1);
    assert!(report.counts.objects >= 1);

    // The manifest: format, version, combo, schema version, counts, and
    // every object id — matching the CAS exactly.
    let manifest = backup::load_manifest(&dest.join("manifest.json")).unwrap();
    assert_eq!(manifest.format, backup::MANIFEST_FORMAT);
    assert!(!manifest.naatre_version.is_empty());
    assert_eq!(manifest.meta, "sqlite");
    assert_eq!(manifest.blob, "fs");
    assert_eq!(manifest.counts, report.counts);
    let mut ids: Vec<String> = cas(&d.data, &cfg())
        .ids()
        .unwrap()
        .iter()
        .map(|id| id.to_hex())
        .collect();
    ids.sort();
    assert_eq!(manifest.objects, ids, "the manifest lists the whole CAS");

    // The SQLite artifact is a self-contained consistent snapshot: it opens
    // read-only (no WAL sidecars) and carries the migrated schema.
    let snap = rusqlite::Connection::open_with_flags(
        dest.join("naatre.db"),
        rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY,
    )
    .unwrap();
    let version: String = snap
        .query_row(
            "SELECT value FROM meta WHERE key = 'schema_version'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(version, naatre_store::MAX_SUPPORTED.to_string());
    // The fs artifact: every manifest id's shard file is in the backup.
    for hex in &manifest.objects {
        let id = Id::from_hex(hex).unwrap();
        let shard = dest
            .join("objects")
            .join(&hex[0..2])
            .join(&hex[2..4])
            .join(&hex[4..]);
        assert!(shard.is_file(), "backup object {hex} missing");
        // The copied bytes are the CAS files, hash-verifiable via get().
        let payload = cas(&dest, &cfg()).get(&id).unwrap();
        let _ = payload;
    }
    assert!(fsck_clean(&d.data).await, "the source stays clean");

    // SECRETS (round 2): the manifest carries enum strings + ids + hashes
    // ONLY — a structural scan of every string value finds no
    // credential-shaped content, and the manifest lists an artifact line
    // for every file it carries.
    let raw = std::fs::read_to_string(dest.join("manifest.json")).unwrap();
    let json: serde_json::Value = serde_json::from_str(&raw).unwrap();
    fn collect_strings(v: &serde_json::Value, out: &mut Vec<String>) {
        match v {
            serde_json::Value::String(s) => out.push(s.clone()),
            serde_json::Value::Array(a) => a.iter().for_each(|x| collect_strings(x, out)),
            serde_json::Value::Object(o) => {
                o.iter().for_each(|(k, x)| {
                    out.push(k.clone());
                    collect_strings(x, out)
                });
            }
            _ => {}
        }
    }
    let mut strings = Vec::new();
    collect_strings(&json, &mut strings);
    for s in &strings {
        for shaped in [
            "postgres://",
            "password",
            "secret",
            "dsn",
            "endpoint",
            "access_key",
        ] {
            assert!(
                !s.to_lowercase().contains(shaped),
                "credential-shaped manifest string {s:?} (matched {shaped:?})"
            );
        }
    }
    let mut want: Vec<String> = vec!["naatre.db".into()];
    for hex in &manifest.objects {
        want.push(format!(
            "objects/{}/{}/{}",
            &hex[0..2],
            &hex[2..4],
            &hex[4..]
        ));
    }
    want.sort();
    let mut got: Vec<String> = manifest.artifacts.iter().map(|a| a.path.clone()).collect();
    got.sort();
    assert_eq!(got, want, "every carried file has an artifact line");

    // PERMISSIONS (round 2): the backup dir is 0700 and every artifact
    // 0600 — it holds the whole instance's personal data.
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode(&dest), 0o700, "backup dir mode");
        assert_eq!(mode(&dest.join("manifest.json")), 0o600);
        assert_eq!(mode(&dest.join("naatre.db")), 0o600);
        for hex in &manifest.objects {
            let shard = dest
                .join("objects")
                .join(&hex[0..2])
                .join(&hex[2..4])
                .join(&hex[4..]);
            assert_eq!(mode(&shard), 0o600, "object {hex} mode");
        }
    }
}

#[tokio::test]
async fn backup_wipe_restore_fsck_clean() {
    if !git_works() {
        return;
    }
    let d = seed("m11bk-e2e");
    let dest = d._root.path().join("backup");
    backup::backup(&d.data, &cfg(), &dest).unwrap();
    let counts = backup::load_manifest(&dest.join("manifest.json"))
        .unwrap()
        .counts;

    // WIPE: the whole data dir goes (the e2e shape — not a selective edit).
    std::fs::remove_dir_all(&d.data).unwrap();
    std::fs::create_dir_all(&d.data).unwrap();

    let report = backup::restore(&d.data, &cfg(), &dest, false).unwrap();
    assert_eq!(
        report.counts, counts,
        "the restored store carries the backup's counts"
    );
    assert_eq!(report.objects_present, counts.objects);
    assert!(fsck_clean(&d.data).await, "fsck is clean after restore");

    // The content is really back: the seeded blob reads back verified.
    let payload = cas(&d.data, &cfg()).get(&d.blob).unwrap();
    assert_eq!(payload, b"backup me\n");
    let store = Store::open_backend(DataDir::open_any(&d.data), &cfg()).unwrap();
    let changesets: i64 = store
        .conn()
        .query_row_map(
            &naatre_store::Sql::plain("SELECT COUNT(*) FROM changesets"),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(changesets, counts.changesets);

    // A second restore refuses: the target is now a live store. The
    // refusal names what it found and the --force remedy.
    match backup::restore(&d.data, &cfg(), &dest, false) {
        Err(BackupError::TargetInitialized { path, found }) => {
            assert_eq!(path, d.data);
            assert!(found.contains("naatre.db"), "{found}");
            let msg = BackupError::TargetInitialized { path, found }.to_string();
            assert!(msg.contains("--force"), "{msg}");
        }
        other => panic!("expected TargetInitialized, got {other:?}"),
    }

    // With --force the live store IS overwritten — and stays consistent.
    backup::restore(&d.data, &cfg(), &dest, true).unwrap();
    assert!(
        fsck_clean(&d.data).await,
        "the forced overwrite restores clean"
    );
}

#[tokio::test]
async fn restore_refuses_mismatched_combo_and_incomplete_backup() {
    if !git_works() {
        return;
    }
    let d = seed("m11bk-refuse");
    let dest = d._root.path().join("backup");
    backup::backup(&d.data, &cfg(), &dest).unwrap();

    // Combo mismatch (the check_data_dir_backend convention: both sides
    // named): the manifest says fs, the target config says s3.
    let s3_cfg = BackendConfig::s3(naatre_store::S3Connect {
        endpoint: Some("127.0.0.1:9000".into()),
        bucket: "naatre-test".into(),
        region: None,
        access_key_id: "a".into(),
        secret: "s".into(),
        path_style: true,
    });
    let target = d._root.path().join("target-s3");
    let e = backup::restore(&target, &s3_cfg, &dest, false).unwrap_err();
    let msg = e.to_string();
    assert!(matches!(e, BackupError::ComboMismatch { .. }), "{msg}");
    assert!(msg.contains("fs") && msg.contains("s3"), "{msg}");

    // Backup refuses to overwrite an existing backup directory.
    match backup::backup(&d.data, &cfg(), &dest) {
        Err(BackupError::OutputExists(p)) => assert_eq!(p, dest),
        other => panic!("expected OutputExists, got {other:?}"),
    }

    // A TAMPERED artifact (one flipped byte in the DB snapshot) is refused
    // by the SHA256 gate BEFORE any write to the target — the target stays
    // untouched (not even created). A refused restore does not wedge the
    // operator: a fresh backup + a wiped target restores clean.
    let db_snapshot = dest.join("naatre.db");
    let mut bytes = std::fs::read(&db_snapshot).unwrap();
    let mid = bytes.len() / 2;
    bytes[mid] ^= 0x01;
    std::fs::write(&db_snapshot, &bytes).unwrap();
    let target2 = d._root.path().join("target2");
    match backup::restore(&target2, &cfg(), &dest, false) {
        Err(BackupError::ChecksumMismatch { path }) => {
            assert_eq!(path, "naatre.db");
            let msg = BackupError::ChecksumMismatch { path }.to_string();
            assert!(
                msg.contains("tampered") && msg.contains("nothing was written"),
                "{msg}"
            );
        }
        other => panic!("expected ChecksumMismatch, got {other:?}"),
    }
    assert!(
        !target2.exists(),
        "the tampered restore wrote nothing to the target"
    );
    let repaired = d._root.path().join("backup2");
    backup::backup(&d.data, &cfg(), &repaired).unwrap();
    backup::restore(&target2, &cfg(), &repaired, false).unwrap();
    assert!(
        fsck_clean(&target2).await,
        "the repaired path restores clean"
    );
}

/// A seeded UNLANDED changeset (the m10b purge shape: no ref, no lands,
/// no git_map, no children — purgeable the moment it is tombstoned, its
/// exclusively-referenced blob following a pass later). One repo, one
/// changeset, one file; seeded through the plain statement layer (no ref
/// to move, exactly the redaction-purge scenario).
struct Redactable {
    _root: tempfile::TempDir,
    data: PathBuf,
    blob: Id,
    cs: Id,
    repo_id: i64,
}

fn seed_redactable() -> Redactable {
    use naatre_core::changeset::{self, Changeset, FileChange, Op};
    use naatre_core::tree::{self as tree_mod, EntryKind, Tree, TreeEntry};
    use naatre_server::db::{ChangesetInsert, TreeEntryRow, TreeWrite};

    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    std::fs::create_dir_all(&data).unwrap();
    std::fs::write(data.join("naatre.db"), b"").unwrap();
    let cfg = cfg();
    let store = Store::open_backend(DataDir::open_any(&data), &cfg).unwrap();
    let conn = store.conn();

    const BASE_NS: i64 = 1_700_000_000_000_000_000;
    let repo_id = db::repos_create(conn, "demo", BASE_NS).unwrap();
    let content = b"redact me\n";
    let blob = object_id(Kind::Blob, content);
    cas(&data, &cfg).put(content).unwrap();
    // The record step's row (what every upload and import writes): a
    // referenced blob without it is fsck dirt.
    db::object_row_upsert(
        conn,
        &blob,
        content.len() as i64,
        content.len() as i64,
        BASE_NS,
    )
    .unwrap();

    let tree = Tree {
        entries: vec![TreeEntry {
            name: "f.txt".into(),
            kind: EntryKind::File,
            object: blob,
            size: content.len() as u64,
            exec: false,
        }],
    };
    let tree_canonical = tree_mod::encode(&tree).unwrap();
    let tree_id = naatre_core::ids::object_id(naatre_core::ids::Kind::Tree, &tree_canonical);
    let cs = Changeset {
        repo_id: 0,
        parents: vec![],
        author: "seed <seed@example.com>".into(),
        message: "redact seed".into(),
        timestamp_ns: BASE_NS as u64,
        changes: vec![FileChange {
            path: "f.txt".into(),
            op: Op::Add,
            blob: Some(blob),
            size: Some(content.len() as u64),
            exec: Some(false),
        }],
    };
    let canonical = changeset::encode(&cs).unwrap();
    let cs_id = object_id(Kind::Changeset, &canonical);
    db::changeset_insert(
        conn,
        ChangesetInsert {
            repo_id,
            repo_name: "demo",
            id: &cs_id,
            canonical: &canonical,
            author: &cs.author,
            message: &cs.message,
            timestamp_ns: BASE_NS,
            generation: 0,
            principal: "seed",
            root_tree_id: &tree_id,
            parents: &[],
            changes: &cs.changes,
            blobs: &[blob],
            trees: &[TreeWrite {
                id: tree_id,
                canonical: tree_canonical,
                entries: vec![TreeEntryRow {
                    name: "f.txt".into(),
                    kind: 0,
                    object_id: blob.0.to_vec(),
                    size: content.len() as i64,
                    exec: false,
                }],
            }],
            now_ns: BASE_NS,
        },
    )
    .unwrap();
    Redactable {
        _root: root,
        data,
        blob,
        cs: cs_id,
        repo_id,
    }
}

/// Drives GC passes (grace 0 — the injected-clock equivalent for a
/// one-shot verb) until `gone` reports the blob reclaimed, or panics.
async fn gc_until_purged(data: &Path, blob: Id, what: &str) {
    let mut state = ServerState::open_backend(DataDir::open_any(data), &cfg()).unwrap();
    state.gc_grace = std::time::Duration::ZERO;
    let state = Arc::new(state);
    for _ in 0..6 {
        gc_pass(&state).await.unwrap();
        if !cas(data, &cfg()).has(&[blob]).unwrap()[0] {
            return;
        }
    }
    panic!("the {what} purge never reclaimed the blob");
}

#[tokio::test]
async fn restore_of_pre_redaction_backup_repurges() {
    let d = seed_redactable();
    let dest = d._root.path().join("backup");
    backup::backup(&d.data, &cfg(), &dest).unwrap();

    // Redact the changeset and its blob (the m10b pair), then drive the GC
    // until the purge reclaims the bytes: the changeset goes in pass one
    // (no grace for erasure), its tree and blob converge a pass later.
    let store = Store::open_backend(DataDir::open_any(&d.data), &cfg()).unwrap();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos() as i64;
    let tx = store.conn().transaction().unwrap();
    let cs_outcome = db::redaction_mark(&tx, d.repo_id, &d.cs, "changeset", now).unwrap();
    let blob_outcome = db::redaction_mark(&tx, d.repo_id, &d.blob, "blob", now).unwrap();
    assert!(matches!(cs_outcome, db::RedactionMarkOutcome::Inserted(_)));
    assert!(matches!(
        blob_outcome,
        db::RedactionMarkOutcome::Inserted(_)
    ));
    tx.commit().unwrap();
    drop(store);
    gc_until_purged(&d.data, d.blob, "first").await;
    assert!(fsck_clean(&d.data).await, "fsck clean after the purge");

    // RESTORE the pre-redaction backup: the content comes back and the
    // restored DB predates the tombstones — the resurrection the operator
    // duty covers.
    std::fs::remove_dir_all(&d.data).unwrap();
    backup::restore(&d.data, &cfg(), &dest, false).unwrap();
    assert!(
        cas(&d.data, &cfg()).has(&[d.blob]).unwrap()[0],
        "the restored backup carries the content again"
    );
    assert!(fsck_clean(&d.data).await, "fsck clean after the restore");
    let store = Store::open_backend(DataDir::open_any(&d.data), &cfg()).unwrap();
    let tombstones = db::redaction_get(store.conn(), d.repo_id, &d.blob).unwrap();
    assert!(
        tombstones.is_none(),
        "a pre-redaction backup carries no tombstones"
    );

    // The operator duty: re-mark the redactions — the tombstones gate again
    // (the write boundary refuses this content from now on) — and the next
    // GC pass re-purges.
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos() as i64;
    let tx = store.conn().transaction().unwrap();
    let cs_outcome = db::redaction_mark(&tx, d.repo_id, &d.cs, "changeset", now).unwrap();
    let blob_outcome = db::redaction_mark(&tx, d.repo_id, &d.blob, "blob", now).unwrap();
    assert!(matches!(cs_outcome, db::RedactionMarkOutcome::Inserted(_)));
    assert!(matches!(
        blob_outcome,
        db::RedactionMarkOutcome::Inserted(_)
    ));
    assert!(db::redaction_active(store.conn(), d.repo_id, &d.blob).unwrap());
    tx.commit().unwrap();
    drop(store);
    gc_until_purged(&d.data, d.blob, "re-").await;
    assert!(fsck_clean(&d.data).await, "fsck clean after the re-purge");
}

// ---- round 2: the GC/purge race against the copy phase ----------------------

#[tokio::test]
async fn gc_between_snapshot_and_copy_grace_holds() {
    if !git_works() {
        return;
    }
    // A GC pass in the window between the DB snapshot and the object copy
    // collects NOTHING (the content is reachable and within the grace
    // window) — the backup stays complete.
    let d = seed("m11bk-race-gc");
    let dest = d._root.path().join("backup");
    let session = backup::backup_begin(&d.data, &cfg(), &dest).unwrap();
    {
        let mut state = ServerState::open_backend(DataDir::open_any(&d.data), &cfg()).unwrap();
        state.gc_grace = std::time::Duration::ZERO;
        let state = Arc::new(state);
        gc_pass(&state).await.unwrap();
    }
    let report = backup::backup_finish(session, &cfg(), &dest).unwrap();
    assert!(
        report.missing.is_empty(),
        "grace holds: no object vanished, got {:?}",
        report.missing
    );
    assert!(!report.past_grace);
    let manifest = backup::load_manifest(&dest.join("manifest.json")).unwrap();
    let mut ids: Vec<String> = cas(&d.data, &cfg())
        .ids()
        .unwrap()
        .iter()
        .map(|id| id.to_hex())
        .collect();
    ids.sort();
    assert_eq!(manifest.objects, ids, "the backup is complete");
    std::fs::remove_dir_all(&d.data).unwrap();
    backup::restore(&d.data, &cfg(), &dest, false).unwrap();
    assert!(fsck_clean(&d.data).await, "the raced backup restores clean");
}

#[tokio::test]
async fn redaction_between_snapshot_and_copy_names_and_repurges() {
    // A redaction purge CAN delete bytes immediately in the window: the
    // copy phase records the vanished id (no wholesale failure), the
    // manifest lists only what the backup carries, restore succeeds — and
    // the restored store is dirty (the snapshot's rows reference the
    // vanished bytes) until the operator re-marks the redactions and the
    // GC pass re-purges (the documented duty).
    let d = seed_redactable();
    let dest = d._root.path().join("backup");
    let session = backup::backup_begin(&d.data, &cfg(), &dest).unwrap();

    let store = Store::open_backend(DataDir::open_any(&d.data), &cfg()).unwrap();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos() as i64;
    let tx = store.conn().transaction().unwrap();
    db::redaction_mark(&tx, d.repo_id, &d.cs, "changeset", now).unwrap();
    db::redaction_mark(&tx, d.repo_id, &d.blob, "blob", now).unwrap();
    tx.commit().unwrap();
    drop(store);
    gc_until_purged(&d.data, d.blob, "raced").await;

    let report = backup::backup_finish(session, &cfg(), &dest).unwrap();
    assert_eq!(
        report.missing,
        vec![d.blob.to_hex()],
        "the report names the vanished id"
    );
    assert!(
        !report
            .dest
            .join("objects")
            .join(&d.blob.to_hex()[0..2])
            .exists()
            || backup::load_manifest(&dest.join("manifest.json"))
                .unwrap()
                .objects
                .iter()
                .all(|hex| *hex != d.blob.to_hex()),
        "the manifest lists only the captured objects"
    );

    // RESTORE succeeds; the restored store is DIRTY (the snapshot's rows
    // reference bytes the purge reclaimed); the duty converges it.
    std::fs::remove_dir_all(&d.data).unwrap();
    backup::restore(&d.data, &cfg(), &dest, false).unwrap();
    assert!(
        !fsck_clean(&d.data).await,
        "the restored store references the vanished bytes (dirty until the duty runs)"
    );
    let store = Store::open_backend(DataDir::open_any(&d.data), &cfg()).unwrap();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos() as i64;
    let tx = store.conn().transaction().unwrap();
    db::redaction_mark(&tx, d.repo_id, &d.cs, "changeset", now).unwrap();
    db::redaction_mark(&tx, d.repo_id, &d.blob, "blob", now).unwrap();
    tx.commit().unwrap();
    drop(store);
    // The bytes are ALREADY gone (the purge raced the backup), so the
    // byte-gone break condition is true at entry — drive a fixed number
    // of passes: pass one purges the changeset, the sweep collects the
    // dying tree, the next pass reclaims the blob's rows.
    {
        let mut state = ServerState::open_backend(DataDir::open_any(&d.data), &cfg()).unwrap();
        state.gc_grace = std::time::Duration::ZERO;
        let state = Arc::new(state);
        for _ in 0..6 {
            gc_pass(&state).await.unwrap();
        }
    }
    assert!(fsck_clean(&d.data).await, "the duty converges the store");
}

#[tokio::test]
async fn copy_past_grace_is_flagged() {
    if !git_works() {
        return;
    }
    // A copy phase that starts past gc_grace_secs after the snapshot is
    // flagged in the report (gc_grace_secs = 0 makes every elapsed time
    // past grace).
    let d = seed("m11bk-race-grace");
    let dest = d._root.path().join("backup");
    let mut warned = cfg();
    warned.gc_grace_secs = Some(0);
    let report = backup::backup(&d.data, &warned, &dest).unwrap();
    assert!(report.past_grace, "the zero grace window is exceeded");
    assert!(
        report.missing.is_empty(),
        "the normal path still captures everything"
    );
    // The default grace (30 days) is not exceeded by a fast backup.
    let dest2 = d._root.path().join("backup2");
    let report = backup::backup(&d.data, &cfg(), &dest2).unwrap();
    assert!(!report.past_grace);
}
