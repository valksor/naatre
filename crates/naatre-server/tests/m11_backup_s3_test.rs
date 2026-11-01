//! M11 backup/restore on the S3 blob combos (SQLite+S3 and Postgres+S3):
//! the manifest lists the bucket's history ids and carries the BUCKET-COPY
//! duty (naatre does not duplicate a bucket into the backup dir), the DB
//! snapshot (VACUUM INTO / pg_dump) goes first, and restore checks object
//! PRESENCE against the bucket — a manifest id the bucket lacks refuses
//! with the duty named. The bucket itself stays intact across the wipe (it
//! IS the operator's copy), so the e2e shape runs for real against the
//! pinned minio. Each provisioner skip-notices its own absence.

use std::sync::Arc;

use naatre_core::changeset::{self, Changeset, FileChange, Op};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::tree::{self as tree_mod, EntryKind, Tree, TreeEntry};
use naatre_server::backup;
use naatre_server::db;
use naatre_server::db::{ChangesetInsert, TreeEntryRow, TreeWrite};
use naatre_server::fsck;
use naatre_server::state::ServerState;
use naatre_store::backend::{BackendConfig, BlobKind};
use naatre_store::{DataDir, Sql, Store};

#[path = "../../naatre-store/tests/pg_provision/mod.rs"]
mod pg_provision;
#[path = "../../naatre-store/tests/s3_provision/mod.rs"]
mod s3_provision;

const BASE_NS: i64 = 1_700_000_000_000_000_000;

struct Seed {
    blob: Id,
    cs: Id,
}

/// The shared seed: one repo, one changeset, one file — CAS bytes into the
/// BUCKET (whatever `cfg.blob` selects), the record row, the changeset.
fn seed(data: &std::path::Path, cfg: &BackendConfig) -> Seed {
    // init's shape: the data dir itself exists (a Postgres combo writes
    // nothing local until the CAS does, so the seed must not rely on it).
    std::fs::create_dir_all(data).unwrap();
    let store = Store::open_backend(DataDir::open_any(data), cfg).expect("store opens");
    let conn = store.conn();
    let repo_id = db::repos_create(conn, "demo", BASE_NS).unwrap();
    let content = b"s3 backup me\n";
    let blob = object_id(Kind::Blob, content);
    let cas = naatre_store::blob::history_cas(&DataDir::open_any(data), cfg).unwrap();
    cas.put(content).unwrap();
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
    let tree_id = object_id(Kind::Tree, &tree_canonical);
    let cs = Changeset {
        repo_id: 0,
        parents: vec![],
        author: "seed <seed@example.com>".into(),
        message: "s3 seed".into(),
        timestamp_ns: BASE_NS as u64,
        changes: vec![FileChange {
            path: "f.txt".into(),
            op: Op::Add,
            blob: Some(blob),
            size: Some(content.len() as u64),
            exec: Some(false),
        }],
        conflicts: Vec::new(),
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
    Seed { blob, cs: cs_id }
}

fn assert_content(data: &std::path::Path, cfg: &BackendConfig, seeded: &Seed) {
    let cas = naatre_store::blob::history_cas(&DataDir::open_any(data), cfg).unwrap();
    assert!(
        cas.has(&[seeded.blob]).unwrap()[0],
        "the blob is in the bucket"
    );
    assert_eq!(cas.get(&seeded.blob).unwrap(), b"s3 backup me\n");
    let store = Store::open_backend(DataDir::open_any(data), cfg).unwrap();
    let author: String = store
        .conn()
        .query_row_map(
            &Sql::plain("SELECT author FROM changesets WHERE id = ?1"),
            &naatre_store::pv![seeded.cs.0.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(author, "seed <seed@example.com>");
}

fn fsck_clean(data: &std::path::Path, cfg: &BackendConfig) -> bool {
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    // The state (holding the sync Postgres client) comes OUT of the async
    // block: dropping it inside one would drop a blocking runtime from an
    // async context.
    let (state, clean) = rt.block_on(async {
        let data = data.to_path_buf();
        let cfg = cfg.clone();
        let state = tokio::task::spawn_blocking(move || {
            Arc::new(ServerState::open_backend(DataDir::open_any(&data), &cfg).unwrap())
        })
        .await
        .unwrap();
        let clean = fsck::fsck(&state).await.unwrap().clean();
        (state, clean)
    });
    drop(state);
    clean
}

/// A copy of the backup whose manifest carries one PHANTOM id (the
/// negative: the bucket copy is incomplete).
fn manifest_with_phantom(backup_dir: &std::path::Path, dest: &std::path::Path) {
    let mut manifest = backup::load_manifest(&backup_dir.join("manifest.json")).unwrap();
    manifest.objects.push("f".repeat(64));
    // The DB snapshot rides along (only the object set is corrupted).
    std::fs::create_dir_all(dest).unwrap();
    for name in ["naatre.db", "postgres.sql"] {
        let src = backup_dir.join(name);
        if src.is_file() {
            std::fs::copy(&src, dest.join(name)).unwrap();
        }
    }
    let json = serde_json::to_string_pretty(&manifest).unwrap();
    std::fs::write(dest.join("manifest.json"), json).unwrap();
}

/// The SQLite+S3 leg: meta sqlite, blob s3 — needs only the REAL S3 server.
#[test]
fn backup_restore_sqlite_s3() {
    let Some(target) = s3_provision::acquire() else {
        return; // s3_provision printed the skip notice
    };
    let cfg: BackendConfig = target.config(); // meta = sqlite, blob = s3
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    std::fs::create_dir_all(&data).unwrap();
    std::fs::write(data.join("naatre.db"), b"").unwrap();
    let seeded = seed(&data, &cfg);
    let dest = root.path().join("backup");

    // BACKUP: the DB snapshot first; the manifest lists the bucket's
    // history ids and the report carries the BUCKET-COPY duty; no objects/
    // tree is duplicated into the backup dir.
    let report = backup::backup(&data, &cfg, &dest).unwrap();
    assert_eq!(report.meta, naatre_store::MetaKind::Sqlite);
    assert_eq!(report.blob, BlobKind::S3);
    assert!(report.bucket_copy_duty, "the operator owes the bucket copy");
    assert!(!dest.join("objects").exists(), "no bucket duplication");
    assert!(dest.join("naatre.db").is_file());
    let manifest = backup::load_manifest(&dest.join("manifest.json")).unwrap();
    assert_eq!(manifest.blob, "s3");
    assert!(
        manifest.objects.contains(&seeded.blob.to_hex()),
        "the seeded blob is listed"
    );

    // The restore's presence gate refuses an incomplete bucket copy — and
    // it runs BEFORE ANY target write: the refused restore leaves the
    // target untouched (not even created). That gate is the S3 target's
    // documented refusal: the restore answers only when the target holds
    // every manifest object.
    let phantom_dir = root.path().join("phantom");
    manifest_with_phantom(&dest, &phantom_dir);
    let fresh = root.path().join("fresh");
    match backup::restore(&fresh, &cfg, &phantom_dir, false, None) {
        Err(backup::BackupError::ObjectsMissing { missing, .. }) => {
            assert!(missing.contains(&"f".repeat(64)))
        }
        other => panic!("expected ObjectsMissing, got {other:?}"),
    }
    assert!(
        !fresh.exists(),
        "the presence-gate refusal wrote nothing to the target"
    );

    // The purge-race escape (two-step): a bucket copy that is genuinely
    // PARTIAL (the seeded object deleted bucket-side) refuses naming the
    // id AND a waiver argument, with the target still unwritten.
    let bucket = |data: &std::path::Path| {
        naatre_store::blob::history_cas(&DataDir::open_any(data), &cfg).unwrap()
    };
    naatre_store::BlobStore::delete(bucket(&data).as_ref(), &[seeded.blob]).unwrap();
    let partial_target = root.path().join("partial-target");
    let (first_missing, first_waiver) =
        match backup::restore(&partial_target, &cfg, &dest, false, None) {
            Err(e @ backup::BackupError::ObjectsMissing { .. }) => {
                let msg = e.to_string();
                let backup::BackupError::ObjectsMissing {
                    missing, waiver, ..
                } = e
                else {
                    unreachable!()
                };
                assert_eq!(missing, vec![seeded.blob.to_hex()]);
                assert!(
                    msg.contains(&seeded.blob.to_hex()) && msg.contains(&waiver),
                    "the refusal lists the ids AND the waiver argument: {msg}"
                );
                (missing, waiver)
            }
            other => panic!("expected the partial-copy refusal, got {other:?}"),
        };
    assert!(!partial_target.exists(), "the refusal wrote nothing");

    // The RE-RUN leg after the operator's bucket copy: the bytes return,
    // the same restore now succeeds with no waiver (a fresh target).
    let rerun_target = root.path().join("rerun-target");
    naatre_store::BlobStore::put(bucket(&data).as_ref(), b"s3 backup me\n").unwrap();
    let report = backup::restore(&rerun_target, &cfg, &dest, false, None).unwrap();
    assert!(report.waived.is_empty());
    naatre_store::BlobStore::delete(bucket(&data).as_ref(), &[seeded.blob]).unwrap();

    // The race: an EXTRA id goes missing between the two runs (here the
    // phantom id, absent from the bucket). The OLD waiver must be refused
    // again, the refusal printing the NEW set and its own waiver.
    let racing_target = root.path().join("racing-target");
    match backup::restore(
        &racing_target,
        &cfg,
        &phantom_dir,
        false,
        Some(&first_waiver),
    ) {
        Err(e @ backup::BackupError::ObjectsMissing { .. }) => {
            let msg = e.to_string();
            let backup::BackupError::ObjectsMissing {
                missing,
                waiver,
                rejected_waiver,
            } = e
            else {
                unreachable!()
            };
            let mut both = vec![seeded.blob.to_hex(), "f".repeat(64)];
            both.sort();
            assert_eq!(missing, both, "the NEW set is what the refusal prints");
            assert_ne!(waiver, first_waiver);
            assert!(rejected_waiver, "{msg}");
            assert!(msg.contains(&"f".repeat(64)), "{msg}");
        }
        other => panic!("a stale waiver must be refused, got {other:?}"),
    }
    assert!(!racing_target.exists(), "the stale waiver wrote nothing");

    // The id-list form of the argument covers the new set exactly.
    let both_ids = format!("{},{}", "f".repeat(64), seeded.blob.to_hex());
    let report =
        backup::restore(&racing_target, &cfg, &phantom_dir, false, Some(&both_ids)).unwrap();
    assert_eq!(report.waived.len(), 2);

    // The re-run with the digest accepts exactly the first refusal's set:
    // the report names the waived id, the restored store misses those
    // bytes (the redaction duty applies).
    let report = backup::restore(&partial_target, &cfg, &dest, false, Some(&first_waiver)).unwrap();
    assert_eq!(report.waived, first_missing);
    let cas = naatre_store::blob::history_cas(&DataDir::open_any(&partial_target), &cfg).unwrap();
    assert!(
        !cas.has(&[seeded.blob]).unwrap()[0],
        "the waived id's bytes are genuinely absent"
    );
    // The waived id is in the restore audit row.
    let store = Store::open_backend(DataDir::open_any(&partial_target), &cfg).unwrap();
    let audits: Vec<String> = store
        .conn()
        .query_all_map(
            &Sql::plain("SELECT details_json FROM audit_log WHERE action = 'restore'"),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert!(
        audits
            .iter()
            .any(|d| d.contains(&format!("\"waived_ids\":[\"{}\"]", seeded.blob.to_hex()))),
        "the audit row names the waived ids: {audits:?}"
    );

    // An S3-configured target whose LOCAL objects/ already holds data is
    // refused (the fs-CAS marker under blob = "s3" — the backend
    // mismatch), with or without --force: never bypassed.
    let marker = root.path().join("s3-with-fs-data");
    std::fs::create_dir_all(marker.join("objects/ab")).unwrap();
    std::fs::write(marker.join("objects/ab/cd"), b"a shard dir marks data").unwrap();
    for forced in [false, true] {
        match backup::restore(&marker, &cfg, &dest, forced, None) {
            Err(e) => {
                let msg = e.to_string();
                assert!(msg.contains("fs") && msg.contains("s3"), "{msg}");
            }
            other => panic!("expected the mismatch refusal (force={forced}), got {other:?}"),
        }
    }

    // WIPE: the data dir goes; the BUCKET stays (it is the operator's
    // copy) — the duty is fulfilled out-of-band. The seeded object's
    // bytes return first (the partial-copy leg deleted them).
    naatre_store::BlobStore::put(
        naatre_store::blob::history_cas(&DataDir::open_any(&data), &cfg)
            .unwrap()
            .as_ref(),
        b"s3 backup me\n",
    )
    .unwrap();
    std::fs::remove_dir_all(&data).unwrap();

    // RESTORE: DB back, presence gate over the bucket passes, fsck clean
    // (the S3 external-drift note is informational, not dirt).
    let report = backup::restore(&data, &cfg, &dest, false, None).unwrap();
    assert_eq!(report.blob, BlobKind::S3);
    assert!(report.objects_present >= 1);
    assert_content(&data, &cfg, &seeded);
    assert!(fsck_clean(&data, &cfg), "fsck clean after the S3 restore");
    assert!(
        backup::restore(&data, &cfg, &dest, false, None).is_err(),
        "a live target refuses"
    );
}

/// The Postgres+S3 leg: needs BOTH dependencies (each provisioner
/// skip-notices its own absence).
#[test]
fn backup_restore_postgres_s3() {
    let Some(pg_target) = pg_provision::acquire() else {
        return;
    };
    let Some(s3_target) = s3_provision::acquire() else {
        return;
    };
    let mut cfg: BackendConfig = pg_target.config();
    let s3cfg = s3_target.config();
    cfg.blob = BlobKind::S3;
    cfg.s3 = s3cfg.s3;
    cfg.allow_plaintext = true;
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    let seeded = seed(&data, &cfg);
    let dest = root.path().join("backup");

    // BACKUP: pg_dump first, the manifest lists the bucket ids, the duty
    // rides the report.
    let report = backup::backup(&data, &cfg, &dest).unwrap();
    assert_eq!(report.meta, naatre_store::MetaKind::Postgres);
    assert_eq!(report.blob, BlobKind::S3);
    assert_eq!(report.dump_file, Some("postgres.sql"));
    assert!(report.bucket_copy_duty);
    assert!(dest.join("postgres.sql").is_file());
    let manifest = backup::load_manifest(&dest.join("manifest.json")).unwrap();
    assert_eq!(manifest.meta, "postgres");
    assert_eq!(manifest.blob, "s3");
    assert!(manifest.objects.contains(&seeded.blob.to_hex()));

    // WIPE: schema dropped, data dir gone, bucket intact.
    let schema = cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let conn = naatre_store::Db::connect_postgres(cfg.pg.as_ref().unwrap(), false).unwrap();
    conn.execute(
        &Sql::plain_owned(format!("DROP SCHEMA \"{schema}\" CASCADE")),
        &[],
    )
    .unwrap();
    std::fs::remove_dir_all(&data).unwrap();

    // RESTORE: psql replays, presence gate over the bucket, fsck clean.
    let report = match backup::restore(&data, &cfg, &dest, false, None) {
        Ok(r) => r,
        Err(e) => {
            if let Ok(log) = std::fs::read_to_string(dest.join("tool-error.log")) {
                panic!("restore failed: {e}\npsql log:\n{log}");
            }
            panic!("restore failed: {e}");
        }
    };
    assert_eq!(report.meta, naatre_store::MetaKind::Postgres);
    assert_content(&data, &cfg, &seeded);
    assert!(
        fsck_clean(&data, &cfg),
        "fsck clean after the PG+S3 restore"
    );
}
