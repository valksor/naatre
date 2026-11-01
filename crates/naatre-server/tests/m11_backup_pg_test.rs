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

/// Recursive directory copy (a backup dir is plain files).
fn copy_dir(src: &std::path::Path, dst: &std::path::Path) {
    std::fs::create_dir_all(dst).unwrap();
    for entry in std::fs::read_dir(src).unwrap() {
        let entry = entry.unwrap();
        let to = dst.join(entry.file_name());
        if entry.file_type().unwrap().is_dir() {
            copy_dir(&entry.path(), &to);
        } else {
            std::fs::copy(entry.path(), &to).unwrap();
        }
    }
}

/// Rewrites a backup dir's `postgres.sql` and re-signs the manifest so the
/// checksum gate passes (the failure under test must land elsewhere).
fn rewrite_dump(dir: &std::path::Path, sql: &str) {
    std::fs::write(dir.join("postgres.sql"), sql).unwrap();
    let mut manifest = backup::load_manifest(&dir.join("manifest.json")).unwrap();
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
        dir.join("manifest.json"),
        serde_json::to_string_pretty(&manifest).unwrap(),
    )
    .unwrap();
}

/// Whether the configured schema exists, and whether the probe table
/// exists in ANY schema — the post-failure "target is as it started" check.
fn schema_and_probe_state(cfg: &BackendConfig) -> (i64, i64) {
    let schema = cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let conn =
        naatre_store::Db::connect_postgres_without_schema_setup(cfg.pg.as_ref().unwrap()).unwrap();
    let schemas: i64 = conn
        .query_row_map(
            &Sql::plain_owned(format!(
                "SELECT COUNT(*) FROM pg_namespace WHERE nspname = '{schema}'"
            )),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    let probes: i64 = conn
        .query_row_map(
            &Sql::plain(
                "SELECT COUNT(*) FROM information_schema.tables \
                 WHERE table_name = 'naatre_fix_probe'",
            ),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    (schemas, probes)
}

/// chmod for the read-only-backup-dir pin (no-op off unix).
fn set_mode(path: &std::path::Path, mode: u32) {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode)).unwrap();
    }
    #[cfg(not(unix))]
    let _ = (path, mode);
}

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
    match backup::restore(&ctx.data, &ctx.cfg, &dest, false, None) {
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
    match backup::restore(&fresh_dir, &ctx.cfg, &dest, false, None) {
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
    let poisoned_first = ctx._root.path().join("backup-poison-first");
    {
        let original = std::fs::read_to_string(dest.join("postgres.sql")).unwrap();
        // The FIRST-STATEMENT poison (the strictest "nothing replays"
        // reading — kept from the original pin).
        backup::backup(&ctx.data, &ctx.cfg, &poisoned_first).unwrap();
        rewrite_dump(&poisoned_first, &format!("NAATRE IS NOT SQL;\n{original}"));
        // The poison AFTER successful DDL: a probe table up front and the
        // bad statement right behind the dump's own CREATE SCHEMA. Without
        // -1 both the table and the schema would survive the failure — the
        // single transaction is what rolls them back.
        backup::backup(&ctx.data, &ctx.cfg, &poisoned).unwrap();
        let mut lines: Vec<String> = original.lines().map(str::to_string).collect();
        let at = lines
            .iter()
            .position(|l| l.starts_with("CREATE SCHEMA"))
            .expect("the schema-scoped dump carries CREATE SCHEMA");
        lines.insert(at + 1, "NAATRE IS NOT SQL;".to_string());
        rewrite_dump(
            &poisoned,
            &format!(
                "CREATE TABLE naatre_fix_probe (ignored int);\n{}\n",
                lines.join("\n")
            ),
        );
    }

    // WIPE: schema dropped, data dir gone.
    wipe(&ctx);

    // Both poisoned restores run against an UNINITIALIZED database: psql
    // fails mid-replay, and the target database is exactly as it started —
    // NO schema, NO probe table — with an immediate re-run succeeding.
    // The backup dir is READ-ONLY while it runs: the failure surfaces as
    // the psql ToolFailed (not an io error), the tool log lands in the
    // writable target PARENT (0600), and the backup dir gains no file.
    for (name, dir) in [("first", &poisoned_first), ("after-ddl", &poisoned)] {
        let target = ctx._root.path().join(format!("target-poison-{name}"));
        set_mode(dir, 0o500);
        let before: Vec<_> = std::fs::read_dir(dir)
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        let result = backup::restore(&target, &ctx.cfg, dir, false, None);
        set_mode(dir, 0o700);
        match result {
            Err(backup::BackupError::ToolFailed { tool, log, .. }) => {
                assert_eq!(tool, "psql", "{name}");
                assert_eq!(
                    log.as_deref(),
                    Some(ctx._root.path().join("tool-error.log").as_path()),
                    "{name}: the log lands beside the target, not in the backup"
                );
            }
            other => panic!("{name}: expected ToolFailed from psql, got {other:?}"),
        }
        let after: Vec<_> = std::fs::read_dir(dir)
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        assert_eq!(
            before.len(),
            after.len(),
            "{name}: the backup dir gained a file"
        );
        assert!(!dir.join("tool-error.log").exists(), "{name}");
        assert_eq!(
            schema_and_probe_state(&ctx.cfg),
            (0, 0),
            "{name}: a failed psql left schema or tables behind"
        );
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(ctx._root.path().join("tool-error.log"))
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600, "the tool log is 0600");
    }

    // The IMMEDIATE RE-RUN with the fixed dump succeeds (the two-step the
    // error teaches): restore the original dump bytes and re-sign.
    {
        let original = std::fs::read_to_string(dest.join("postgres.sql")).unwrap();
        rewrite_dump(&poisoned, &original);
        let target = ctx._root.path().join("target-poison-2");
        let report = backup::restore(&target, &ctx.cfg, &poisoned, false, None).unwrap();
        assert_eq!(report.meta, naatre_store::MetaKind::Postgres);
    }

    // The re-run re-created the schema (the fixed dump restored it): drop
    // it again — the operator's wipe — before the final restore.
    {
        let schema = ctx.cfg.pg.as_ref().unwrap().schema.clone().unwrap();
        let conn = naatre_store::Db::connect_postgres(ctx.cfg.pg.as_ref().unwrap(), false).unwrap();
        conn.execute(
            &Sql::plain_owned(format!("DROP SCHEMA \"{schema}\" CASCADE")),
            &[],
        )
        .unwrap();
    }

    // RESTORE: psql replays the dump, objects copy back, presence gate
    // passes, fsck clean.
    let report = match backup::restore(&ctx.data, &ctx.cfg, &dest, false, None) {
        Ok(r) => r,
        Err(e) => {
            // The tool log is a local-trust artifact (loopback cluster);
            // print it so the failure is diagnosable.
            if let Ok(log) = std::fs::read_to_string(ctx._root.path().join("tool-error.log")) {
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
    assert!(backup::restore(&ctx.data, &ctx.cfg, &dest, false, None).is_err());
}

/// Round 4 fix items: backup refuses an UNINITIALIZED PG target (no
/// schema silently migrated into existence), and a dump/restore
/// round-trip under a DIFFERENT role than the one that dumped
/// (--no-owner --no-privileges: the replay belongs to the restoring
/// role).
#[test]
fn backup_refuses_uninitialized_pg_and_round_trips_cross_role() {
    let Some(target) = pg_provision::acquire() else {
        return;
    };
    // An UNINITIALIZED target: a data dir + a config whose schema has
    // never been created. Backup must refuse naming `naatre init` — and
    // must NOT leave the schema behind (the probe ran without the
    // schema-setup side effect).
    let cfg = target.config();
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    std::fs::create_dir_all(&data).unwrap();
    let dest = root.path().join("backup");
    match backup::backup(&data, &cfg, &dest) {
        Err(backup::BackupError::Uninitialized { path }) => {
            let msg = backup::BackupError::Uninitialized { path: path.clone() }.to_string();
            assert!(msg.contains("naatre init"), "{msg}");
            assert_eq!(path, data);
        }
        other => panic!("expected Uninitialized, got {other:?}"),
    }
    let schema = cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let probe_conn =
        naatre_store::Db::connect_postgres_without_schema_setup(cfg.pg.as_ref().unwrap()).unwrap();
    let schema_rows: i64 = probe_conn
        .query_row_map(
            &Sql::plain_owned(format!(
                "SELECT COUNT(*) FROM pg_namespace WHERE nspname = '{schema}'"
            )),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        schema_rows, 0,
        "the probe created the schema as a side effect"
    );

    // Seed a real store (the schema comes into existence properly via the
    // store open), back it up as the "naatre" role…
    let seeded = root.path().join("seeded");
    std::fs::create_dir_all(&seeded).unwrap();
    let store = Store::open_backend(DataDir::open_any(&seeded), &cfg).unwrap();
    db::repos_create(store.conn(), "demo", 1_700_000_000_000_000_000).unwrap();
    drop(store);
    let backup_dir = root.path().join("backup-role");
    backup::backup(&seeded, &cfg, &backup_dir).unwrap();

    // …then restore under a DIFFERENT role: the replay must succeed and
    // the restored objects must belong to the restoring role
    // (--no-owner --no-privileges makes the dump role-clean).
    let admin = naatre_store::Db::connect_postgres(cfg.pg.as_ref().unwrap(), false).unwrap();
    admin
        .execute(
            &Sql::plain("DROP SCHEMA IF EXISTS naatre_fix_role CASCADE"),
            &[],
        )
        .expect("drop probe schema");
    admin
        .execute(&Sql::plain("CREATE ROLE naatre_fix_role LOGIN"), &[])
        .expect("create role");
    admin
        .execute(
            &Sql::plain("GRANT CREATE ON DATABASE postgres TO naatre_fix_role"),
            &[],
        )
        .expect("grant create");
    let mut role_cfg = cfg.clone();
    role_cfg.pg.as_mut().unwrap().user = Some("naatre_fix_role".into());
    let schema = role_cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let _ = admin.execute(
        &Sql::plain_owned(format!("DROP SCHEMA \"{schema}\" CASCADE")),
        &[],
    );
    let restored = root.path().join("restored-as-role");
    backup::restore(&restored, &role_cfg, &backup_dir, false, None).unwrap();
    let role_conn =
        naatre_store::Db::connect_postgres(role_cfg.pg.as_ref().unwrap(), false).unwrap();
    let repos: i64 = role_conn
        .query_row_map(
            &Sql::plain("SELECT COUNT(*) FROM repos WHERE name = 'demo'"),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(repos, 1, "the cross-role replay carries the data");
    let schema = role_cfg.pg.as_ref().unwrap().schema.clone().unwrap();
    let owner: String = role_conn
        .query_row_map(
            &Sql::plain_owned(format!(
                "SELECT tableowner FROM pg_tables \
                 WHERE tablename = 'repos' AND schemaname = '{schema}'"
            )),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        owner, "naatre_fix_role",
        "the restored objects belong to the restoring role"
    );
    // No role-drop cleanup: the role owns the restored objects, and the
    // throwaway cluster dies with the test process anyway.
}

/// Too-new / unreadable-version dumps are refused from the VERIFIED staged
/// dump BEFORE psql runs: the Postgres target stays uninitialized and the
/// target dir is never created. The version parse FAILS CLOSED — a dump
/// with no readable `schema_version` row is refused, never assumed old.
#[test]
fn pg_too_new_and_unreadable_version_dumps_refused_before_any_write() {
    let Some(target) = pg_provision::acquire() else {
        return;
    };
    let ctx = seed(target);
    let dest = ctx._root.path().join("backup");
    backup::backup(&ctx.data, &ctx.cfg, &dest).unwrap();
    let original = std::fs::read_to_string(dest.join("postgres.sql")).unwrap();
    let version_line = format!("schema_version\t{}", naatre_store::MAX_SUPPORTED);
    assert!(
        original.lines().any(|l| l == version_line),
        "the dump's meta COPY block carries the version line"
    );
    wipe(&ctx);

    let too_new = ctx._root.path().join("backup-too-new");
    let no_version = ctx._root.path().join("backup-no-version");
    let garbled = ctx._root.path().join("backup-garbled");
    for (dir, sql) in [
        (
            &too_new,
            original.replace(&version_line, "schema_version\t99"),
        ),
        (
            &no_version,
            original
                .lines()
                .filter(|l| *l != version_line)
                .collect::<Vec<_>>()
                .join("\n")
                + "\n",
        ),
        (
            &garbled,
            original.replace(&version_line, "schema_version\tnew"),
        ),
    ] {
        copy_dir(&dest, dir);
        rewrite_dump(dir, &sql);
    }

    let restore_target = ctx._root.path().join("restore-target");
    match backup::restore(&restore_target, &ctx.cfg, &too_new, false, None) {
        Err(backup::BackupError::TooNew { found, supported }) => {
            assert_eq!(found, 99);
            assert_eq!(supported, naatre_store::MAX_SUPPORTED);
        }
        other => panic!("expected TooNew, got {other:?}"),
    }
    for (name, dir) in [("no version", &no_version), ("garbled version", &garbled)] {
        match backup::restore(&restore_target, &ctx.cfg, dir, false, None) {
            Err(backup::BackupError::Manifest { why, .. }) => {
                assert!(why.contains("schema_version"), "{name}: {why}")
            }
            other => panic!("{name}: expected the fail-closed refusal, got {other:?}"),
        }
    }
    assert!(!restore_target.exists(), "no refusal created the target");
    assert_eq!(
        schema_and_probe_state(&ctx.cfg).0,
        0,
        "no refusal reached psql (the schema never came back)"
    );
}
