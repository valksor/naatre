//! M11 backup/restore on Postgres + fs: the plan's e2e shape against the
//! REAL PG harness — one `pg_dump` consistent snapshot (a documented
//! external dependency of PG deployments), the wipe (drop schema + remove
//! the data dir), the `psql` restore, fsck clean, and the restore's
//! clobber guard on an initialized target. Skips with a printed notice
//! when no PostgreSQL target can be produced (the only legitimate skip).

use std::path::PathBuf;
use std::sync::Arc;

use naatre_core::changeset::{self, Changeset, FileChange, Op};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::tree::{self as tree_mod, EntryKind, Tree, TreeEntry};
use naatre_server::backup;
use naatre_server::db;
use naatre_server::db::{ChangesetInsert, TreeEntryRow, TreeWrite};
use naatre_server::fsck;
use naatre_server::state::ServerState;
use naatre_store::backend::BackendConfig;
use naatre_store::{DataDir, Sql, Store};

#[path = "../../naatre-store/tests/pg_provision/mod.rs"]
mod pg_provision;

const BASE_NS: i64 = 1_700_000_000_000_000_000;

struct Ctx {
    _target: pg_provision::PgTarget,
    cfg: BackendConfig,
    _root: tempfile::TempDir,
    data: PathBuf,
    blob: Id,
    cs: Id,
}

fn seed(target: pg_provision::PgTarget) -> Ctx {
    let cfg = target.config();
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    let store = Store::open_backend(DataDir::open_any(&data), &cfg).expect("pg store opens");
    let conn = store.conn();

    let repo_id = db::repos_create(conn, "demo", BASE_NS).unwrap();
    let content = b"pg backup me\n";
    let blob = object_id(Kind::Blob, content);
    let cas = naatre_store::blob::history_cas(&DataDir::open_any(&data), &cfg).unwrap();
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
        message: "pg seed".into(),
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
    Ctx {
        _target: target,
        cfg,
        _root: root,
        data,
        blob,
        cs: cs_id,
    }
}

fn fsck_clean(ctx: &Ctx) -> bool {
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    // The sync Postgres client refuses query work from inside a runtime
    // (the open runs on a blocking thread, the m9pg shape) and must not be
    // DROPPED inside one either — the state comes out of the block.
    let (state, clean) = rt.block_on(async {
        let data = ctx.data.clone();
        let cfg = ctx.cfg.clone();
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

/// The wipe: the metadata schema goes (the PG deployment's duty) and the
/// data dir's local files go with it.
fn wipe(ctx: &Ctx) {
    let schema = ctx.cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let conn = naatre_store::Db::connect_postgres(ctx.cfg.pg.as_ref().unwrap(), false).unwrap();
    conn.execute(
        &Sql::plain_owned(format!("DROP SCHEMA \"{schema}\" CASCADE")),
        &[],
    )
    .unwrap();
    std::fs::remove_dir_all(&ctx.data).unwrap();
}

#[test]
fn backup_wipe_restore_fsck_clean_postgres_fs() {
    let Some(target) = pg_provision::acquire() else {
        return; // pg_provision printed the skip notice
    };
    let ctx = seed(target);
    let dest = ctx._root.path().join("backup");

    // BACKUP: pg_dump FIRST (the safe order), then the objects.
    let report = backup::backup(&ctx.data, &ctx.cfg, &dest).unwrap();
    assert_eq!(report.meta, naatre_store::MetaKind::Postgres);
    assert_eq!(report.blob, naatre_store::BlobKind::Fs);
    assert_eq!(report.dump_file, Some("postgres.sql"));
    assert_eq!(report.counts.changesets, 1);
    let manifest = backup::load_manifest(&dest.join("manifest.json")).unwrap();
    assert_eq!(manifest.meta, "postgres");
    let dump = std::fs::read_to_string(dest.join("postgres.sql")).unwrap();
    assert!(dump.contains("CREATE TABLE"), "a plain-format schema dump");
    for hex in &manifest.objects {
        let shard = dest
            .join("objects")
            .join(&hex[0..2])
            .join(&hex[2..4])
            .join(&hex[4..]);
        assert!(shard.is_file(), "backup object {hex} missing");
    }

    // The restore's clobber guard on the live target: the local objects
    // make it TargetNotEmpty — and with the local dir EMPTY (a fresh dir,
    // same DB) the schema probe answers TargetInitialized. The refusals
    // name what they found and the remedy.
    match backup::restore(&ctx.data, &ctx.cfg, &dest, false) {
        Err(
            e @ (backup::BackupError::TargetNotEmpty { .. }
            | backup::BackupError::TargetInitialized { .. }),
        ) => {
            let msg = e.to_string();
            assert!(msg.contains("wipe") || msg.contains("--force"), "{msg}");
        }
        other => panic!("expected a live-target refusal, got {other:?}"),
    }
    let fresh_dir = ctx._root.path().join("fresh-empty");
    std::fs::create_dir_all(&fresh_dir).unwrap();
    match backup::restore(&fresh_dir, &ctx.cfg, &dest, false) {
        Err(backup::BackupError::TargetInitialized { found, .. }) => {
            assert!(found.contains("schema"), "{found}");
        }
        other => panic!("expected the schema-probe refusal, got {other:?}"),
    }

    // SECRETS (round 2): the manifest carries NO credential material. The
    // config here carries a password (it rides PGPASSWORD to pg_dump);
    // the manifest's every string value must be free of it and of any
    // DSN/endpoint shape.
    {
        let mut cred_cfg = ctx.cfg.clone();
        cred_cfg.pg.as_mut().unwrap().password = Some("naatre-cred-marker".into());
        let dest2 = ctx._root.path().join("backup-cred");
        backup::backup(&ctx.data, &cred_cfg, &dest2).unwrap();
        let raw = std::fs::read_to_string(dest2.join("manifest.json")).unwrap();
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
                "naatre-cred-marker",
                "postgres://",
                "password",
                "secret",
                "dsn",
                "endpoint",
                "127.0.0.1",
            ] {
                assert!(
                    !s.to_lowercase().contains(shaped),
                    "credential-shaped manifest string {s:?} (matched {shaped:?})"
                );
            }
        }
    }

    // FAILED MID-PSQL (round 2, item 1): a dump that passes its SHA256
    // gate but fails REPLAY leaves the target database as it was —
    // uninitialized (restore refuses initialized targets BEFORE any psql
    // write, so a failure can never strand a half-restored live store).
    let poisoned = ctx._root.path().join("backup-poison");
    {
        backup::backup(&ctx.data, &ctx.cfg, &poisoned).unwrap();
        let mut sql = std::fs::read_to_string(poisoned.join("postgres.sql")).unwrap();
        // PREPEND: the poison fails on psql's first statement, so NOTHING
        // of the dump replays — the strictest reading of "the target is
        // untouched by a failed restore".
        sql = "NAATRE IS NOT SQL;\n".to_string() + &sql;
        std::fs::write(poisoned.join("postgres.sql"), &sql).unwrap();
        // Re-sign the poisoned artifact so the checksum gate passes and
        // the failure lands INSIDE psql.
        let mut manifest = backup::load_manifest(&poisoned.join("manifest.json")).unwrap();
        let sha = {
            use sha2::Digest as _;
            format!("{:x}", sha2::Sha256::digest(sql.as_bytes()))
        };
        for a in &mut manifest.artifacts {
            if a.path == "postgres.sql" {
                a.sha256 = sha.clone();
            }
        }
        std::fs::write(
            poisoned.join("manifest.json"),
            serde_json::to_string_pretty(&manifest).unwrap(),
        )
        .unwrap();
    }

    // WIPE: schema dropped, data dir gone.
    wipe(&ctx);

    // The poisoned restore now runs against an UNINITIALIZED database:
    // psql fails mid-replay, and the target stays uninitialized.
    {
        let target = ctx._root.path().join("target-poison");
        match backup::restore(&target, &ctx.cfg, &poisoned, false) {
            Err(backup::BackupError::ToolFailed { tool, .. }) => assert_eq!(tool, "psql"),
            other => panic!("expected ToolFailed from psql, got {other:?}"),
        }
        // The target database is UNINITIALIZED after the failure: the
        // probe answers false (no naatre schema_version anywhere).
        let conn = naatre_store::Db::connect_postgres(ctx.cfg.pg.as_ref().unwrap(), false).unwrap();
        let schema = ctx.cfg.pg.as_ref().unwrap().schema.clone().unwrap();
        // An absent schema makes the probe itself error — that error IS
        // "uninitialized" (the same reading postgres_initialized uses).
        let probe = conn
            .query_opt(
                &Sql::plain_owned(format!(
                    "SELECT value FROM \"{schema}\".meta WHERE key = 'schema_version'"
                )),
                &[],
            )
            .ok()
            .flatten();
        assert!(probe.is_none(), "a failed psql left naatre schema behind");
    }

    // RESTORE: psql replays the dump, objects copy back, presence gate
    // passes, fsck clean.
    let report = match backup::restore(&ctx.data, &ctx.cfg, &dest, false) {
        Ok(r) => r,
        Err(e) => {
            // The tool log is a local-trust artifact (loopback cluster);
            // print it so the failure is diagnosable.
            if let Ok(log) = std::fs::read_to_string(dest.join("tool-error.log")) {
                panic!("restore failed: {e}\npsql log:\n{log}");
            }
            panic!("restore failed: {e}");
        }
    };
    assert_eq!(report.counts, manifest.counts);
    let cas = naatre_store::blob::history_cas(&DataDir::open_any(&ctx.data), &ctx.cfg).unwrap();
    let payload = cas.get(&ctx.blob).unwrap();
    assert_eq!(payload, b"pg backup me\n");
    let store = Store::open_backend(DataDir::open_any(&ctx.data), &ctx.cfg).unwrap();
    let author: String = store
        .conn()
        .query_row_map(
            &Sql::plain("SELECT author FROM changesets WHERE id = ?1"),
            &naatre_store::pv![ctx.cs.0.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(author, "seed <seed@example.com>");
    assert!(fsck_clean(&ctx), "fsck clean after the PG restore");

    // The schema exists again (the dump carried it) — a second restore is
    // refused again (local objects back in place).
    assert!(backup::restore(&ctx.data, &ctx.cfg, &dest, false).is_err());
}
