//! `naatre backup` / `naatre restore` (M11): consistent snapshots of a
//! whole data dir for every backend combo, under the data-dir flock (the
//! same exclusivity as `serve`/`import` — a backup is stop-and-copy by
//! construction, and the safe-ordering argument below is what makes the
//! artifacts self-consistent).
//!
//! ONE backup directory per run — created 0700, every artifact 0600 (it
//! holds the whole instance's personal data):
//!
//! ```text
//! <dest>/manifest.json   format, naatre version, backend combo {meta,
//!                        blob}, schema version, timestamp, counts, every
//!                        history-CAS object id, and the SHA256 of every
//!                        artifact — written LAST, so a torn backup has no
//!                        manifest and restore refuses it. NO credentials
//!                        and no connection config: the combo is enum
//!                        strings only (meta: sqlite|postgres, blob: fs|s3).
//! <dest>/naatre.db       SQLite meta: a consistent single-file snapshot
//!                        via `VACUUM INTO` — never an in-place file copy
//!                        of a WAL database (a plain copy of a live WAL db
//!                        can be torn; the README "Backup" section carries
//!                        the prohibition)
//! <dest>/postgres.sql    Postgres meta: one `pg_dump` plain-format run —
//!                        a single MVCC-consistent snapshot. `pg_dump`
//!                        (and restore's `psql`) are documented external
//!                        dependencies of Postgres deployments, the same
//!                        convention as git for import; a missing binary
//!                        is a clean operator error naming the tool. The
//!                        password travels via PGPASSWORD (or the
//!                        operator's connection service file) — NEVER on
//!                        a command line (argv leaks through ps).
//! <dest>/objects/…       fs CAS only: the immutable object files, copied
//!                        per the manifest
//! ```
//!
//! The safe order — consistent DB snapshot FIRST, then the immutable
//! objects — is enforced by this module's control flow, and it is safe in
//! both directions: blob fsync precedes every referencing row commit
//! (durability invariant 1), so a DB snapshot older than the object copy
//! leaves at most harmless orphans, never dangling references. Restated
//! per combo:
//!
//! - SQLite + fs: `VACUUM INTO` first, then copy `objects/`.
//! - SQLite + S3: `VACUUM INTO` first; the manifest then lists the object
//!   ids and the operator performs the bucket copy (out-of-band S3
//!   tooling) — naatre does not duplicate a whole bucket into the backup
//!   dir.
//! - Postgres + fs: the `pg_dump` snapshot first, then copy `objects/`.
//! - Postgres + S3: the `pg_dump` snapshot first; the manifest lists the
//!   ids for the operator's bucket copy.
//!
//! GC vs the copy phase: plain GC cannot race a fresh backup within
//! `gc_grace_secs` (bytes reachable at snapshot time survive the grace
//! window), and the data-dir flock keeps host-local GC/fsck/import out
//! entirely. A REDACTION PURGE between snapshot and copy CAN delete bytes
//! immediately — that is the restore-of-pre-redaction case: the copy
//! phase records the vanished ids in the report (it does not fail the
//! backup), the manifest lists only the objects actually captured, and
//! restore + re-mark + GC converge (the documented operator duty). The
//! report flags a copy phase that ran past `gc_grace_secs`.
//!
//! `restore` validates the manifest, verifies the SHA256 of EVERY artifact
//! into a staging directory BEFORE any write to the final location
//! (all-or-nothing: a corrupt or tampered backup leaves the target
//! untouched), refuses a backend-combo mismatch with the target's config
//! (the `check_data_dir_backend` convention), refuses a non-empty target
//! unless `--force` is given (an initialized Postgres schema is refused
//! regardless — dropping it is the operator's wipe), restores DB then
//! objects, and directs the operator to `naatre fsck`: restore checks
//! object PRESENCE, not reachability, and never silently skips
//! verification.
//!
//! Driver/tool error text is redacted by construction: `pg_dump`/`psql`
//! failures carry the exit status only, with the tool's own stderr left in
//! a log file inside the backup dir for the operator to inspect — the same
//! posture as the backend config surface (connection errors can embed
//! DSN, host, and user).

use std::path::{Path, PathBuf};
use std::process::Command;

use sha2::Digest;

use naatre_store::backend::{BackendError, PgConnect, check_data_dir_backend};
use naatre_store::{
    BackendConfig, BlobKind, DataDir, Db, MetaKind, Sql, Store, StoreError, data_dir::DB_FILE,
};

/// The manifest format this build writes and accepts. A backup with a
/// higher format is refused (a newer binary wrote it).
pub const MANIFEST_FORMAT: u32 = 1;

/// The SQLite snapshot file name inside the backup dir.
const SQLITE_DUMP: &str = "naatre.db";
/// The Postgres plain-format dump file name inside the backup dir.
const PG_DUMP: &str = "postgres.sql";
/// The pg_dump/psql diagnostics log, written ONLY on a tool failure.
const TOOL_LOG: &str = "tool-error.log";
/// The staging-directory prefix for restore's verify-then-move (a sibling
/// of the target, same filesystem, so the final DB write is a rename).
const STAGE_PREFIX: &str = ".naatre-restore-";

/// One artifact's integrity line: the path inside the backup dir and the
/// SHA256 of its bytes.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct ArtifactChecksum {
    /// Forward-slash path relative to the backup dir (`naatre.db`,
    /// `postgres.sql`, `objects/<2hex>/<2hex>/<62hex>`).
    pub path: String,
    /// Lowercase hex SHA256 of the file's bytes.
    pub sha256: String,
}

/// The manifest of one backup directory.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct BackupManifest {
    /// [`MANIFEST_FORMAT`] when written by this build.
    pub format: u32,
    pub naatre_version: String,
    /// Unix nanos at backup time (host clock).
    pub created_ns: i64,
    /// Backend combo: `"sqlite"`/`"postgres"` and `"fs"`/`"s3"` — restore
    /// refuses a mismatch with the target's config. Enum strings ONLY:
    /// no DSN, endpoint, bucket, or credential ever appears here.
    pub meta: String,
    pub blob: String,
    /// The store schema version at backup time (a restore may
    /// forward-migrate).
    pub schema_version: i64,
    pub counts: BackupCounts,
    /// The history-CAS object ids this backup CARRIES (64-hex, sorted) —
    /// the fs copy list and the S3 bucket-copy duty list. An object the
    /// copy phase found vanished (a redaction purge raced the snapshot)
    /// is NOT listed: the report names it instead.
    pub objects: Vec<String>,
    /// The SHA256 of every artifact file in the backup dir (the DB
    /// snapshot and, for fs, every object file). Restore verifies all of
    /// them into staging before touching the target.
    pub artifacts: Vec<ArtifactChecksum>,
}

/// The manifest's sanity counts.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct BackupCounts {
    pub changesets: i64,
    pub objects: u64,
}

/// What one backup produced.
#[derive(Debug)]
pub struct BackupReport {
    pub dest: PathBuf,
    pub meta: MetaKind,
    pub blob: BlobKind,
    pub schema_version: i64,
    pub counts: BackupCounts,
    /// Set for Postgres: the dump file's name inside the backup dir.
    pub dump_file: Option<&'static str>,
    /// True for S3: the operator owes the bucket copy (README "Backup").
    pub bucket_copy_duty: bool,
    /// Object ids the copy phase found vanished (a redaction purge raced
    /// the snapshot): NOT in the backup, named for the operator duty.
    pub missing: Vec<String>,
    /// True when the copy phase started past `gc_grace_secs` after the DB
    /// snapshot — GC could have reclaimed snapshot-reachable bytes.
    pub past_grace: bool,
}

/// What one restore did.
#[derive(Debug)]
pub struct RestoreReport {
    pub meta: MetaKind,
    pub blob: BlobKind,
    pub schema_version: i64,
    pub counts: BackupCounts,
    /// Manifest ids confirmed present in the target CAS (fs: copied from
    /// the backup; S3: found in the bucket).
    pub objects_present: u64,
    /// True when the run overrode the non-empty-target refusal.
    pub forced: bool,
}

/// Everything a backup/restore can fail with. Messages name paths, rules,
/// and tools — never DSN/credential values (tool stderr goes to a log
/// file, not the error).
#[derive(Debug, thiserror::Error)]
pub enum BackupError {
    /// The store/config layer's redacted errors (lock held, backend
    /// mismatch, redacted connect failures, migration guard).
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error("SQLite VACUUM INTO failed: {0}")]
    Sqlite(#[from] rusqlite::Error),
    #[error("cannot write {}: {e}", path.display())]
    Io {
        path: PathBuf,
        #[source]
        e: std::io::Error,
    },
    #[error(
        "backup directory {} already exists; refusing to overwrite a backup",
        .0.display()
    )]
    OutputExists(PathBuf),
    #[error("backup at {}: {why}", path.display())]
    Manifest { path: PathBuf, why: &'static str },
    #[error(
        "backup manifest format {got} is not supported (this build reads {MANIFEST_FORMAT}); \
         restore with the naatre version that wrote the backup"
    )]
    UnsupportedFormat { got: u32 },
    #[error(
        "backup artifact {path}: SHA256 mismatch — the backup is corrupt or was tampered \
         with; nothing was written to the target"
    )]
    ChecksumMismatch { path: String },
    #[error(
        "backup was taken on meta = {manifest_meta}, blob = {manifest_blob} but the target \
         selects meta = {configured_meta}, blob = {configured_blob}: refusing to restore \
         across backend combos (a data dir never switches backends in place)"
    )]
    ComboMismatch {
        manifest_meta: String,
        manifest_blob: String,
        configured_meta: String,
        configured_blob: String,
    },
    #[error(
        "target data dir {path} is already initialized ({found}); restore never overwrites \
         a live store without --force — wipe it first (the e2e shape: backup → wipe → \
         restore → fsck) or pass --force to overwrite",
        path = .path.display(),
    )]
    TargetInitialized { path: PathBuf, found: &'static str },
    #[error(
        "target data dir {path} is not empty ({found}); restore requires a wiped target — \
         wipe it first or pass --force to restore over it",
        path = .path.display(),
    )]
    TargetNotEmpty { path: PathBuf, found: &'static str },
    #[error(
        "{tool} is not on PATH: Postgres deployments need the PostgreSQL client tools \
         ({tool}; a documented external dependency, the same convention as git for import)"
    )]
    MissingTool { tool: &'static str },
    #[error(
        "{tool} exited with {status}; its stderr is in {} (connection errors can name the \
         host and user, so they are not echoed here)",
        log.display()
    )]
    ToolFailed {
        tool: &'static str,
        status: std::process::ExitStatus,
        log: PathBuf,
    },
    #[error(
        "the target CAS is missing {} of the manifest's object(s) (e.g. {sample}): for \
         blob = \"s3\" restore the bucket copy FIRST — restore checks presence, then the \
         operator runs fsck",
        missing.len()
    )]
    ObjectsMissing {
        missing: Vec<String>,
        sample: String,
    },
}

impl BackupError {
    fn io(path: &Path, e: std::io::Error) -> Self {
        BackupError::Io {
            path: path.to_path_buf(),
            e,
        }
    }
}

/// The statement layer under [`Db`] reports [`naatre_store::DbError`]
/// directly; route it through the store error's redacted `Db` shape.
fn store_db(e: naatre_store::DbError) -> BackupError {
    BackupError::Store(StoreError::Db(e))
}

/// Resolves an external tool on PATH — the shared missing-tool gate for
/// `pg_dump`/`psql`. The error names the dependency and the convention,
/// never a command line.
pub fn resolve_tool(tool: &'static str) -> Result<PathBuf, BackupError> {
    let path = std::env::var("PATH").unwrap_or_default();
    for dir in std::env::split_paths(&path) {
        let candidate = dir.join(tool);
        if candidate.is_file() {
            return Ok(candidate);
        }
    }
    Err(BackupError::MissingTool { tool })
}

/// Reads and validates one backup manifest. Every listed id must re-parse
/// as 64-hex and every artifact checksum as 64-hex SHA256: a corrupt
/// manifest is refused here, not mid-restore.
pub fn load_manifest(path: &Path) -> Result<BackupManifest, BackupError> {
    let text = std::fs::read_to_string(path).map_err(|_| BackupError::Manifest {
        path: path.to_path_buf(),
        why: "no manifest.json — a torn or foreign directory is refused (the manifest \
              is written last, so an interrupted backup has none)",
    })?;
    let manifest: BackupManifest =
        serde_json::from_str(&text).map_err(|_| BackupError::Manifest {
            path: path.to_path_buf(),
            why: "manifest.json does not parse as a naatre backup manifest",
        })?;
    if manifest.format != MANIFEST_FORMAT {
        return Err(BackupError::UnsupportedFormat {
            got: manifest.format,
        });
    }
    if manifest
        .objects
        .iter()
        .any(|hex| naatre_core::ids::Id::from_hex(hex).is_none())
    {
        return Err(BackupError::Manifest {
            path: path.to_path_buf(),
            why: "manifest lists an id that is not 64 lowercase hex characters",
        });
    }
    if manifest
        .artifacts
        .iter()
        .any(|a| a.sha256.len() != 64 || !a.sha256.bytes().all(|b| b.is_ascii_hexdigit()))
    {
        return Err(BackupError::Manifest {
            path: path.to_path_buf(),
            why: "an artifact line does not carry a 64-hex SHA256",
        });
    }
    Ok(manifest)
}

/// SHA256 of a file's bytes, lowercase hex.
fn sha256_file(path: &Path) -> Result<String, BackupError> {
    let bytes = std::fs::read(path).map_err(|e| BackupError::io(path, e))?;
    Ok(format!("{:x}", sha2::Sha256::digest(&bytes)))
}

/// The CAS shard path under `root` — `root/<2hex>/<2hex>/<62hex>`, the
/// layout both backends pin (`fs_util::shard_path` is crate-private; the
/// shape is a documented invariant asserted by the store tests).
fn shard_path(root: &Path, id: &naatre_core::ids::Id) -> PathBuf {
    let hex = id.to_hex();
    root.join(&hex[0..2]).join(&hex[2..4]).join(&hex[4..])
}

/// True when `objects/` holds at least one entry — the on-disk marker of
/// an fs CAS that has actually stored something (the same rule backend.rs's
/// mismatch guard reads; that copy is crate-private to naatre-store).
fn objects_dir_has_entries(data: &Path) -> bool {
    data.join(naatre_store::data_dir::OBJECTS_DIR)
        .read_dir()
        .map(|mut entries| entries.next().is_some())
        .unwrap_or(false)
}

#[cfg(unix)]
fn chmod(path: &Path, mode: u32) -> Result<(), BackupError> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode))
        .map_err(|e| BackupError::io(path, e))
}
#[cfg(not(unix))]
fn chmod(_path: &Path, _mode: u32) -> Result<(), BackupError> {
    Ok(())
}

/// The first half of a backup: guards, the flock, the CAS listing, the
/// store, and the consistent DB snapshot. Split from [`backup_finish`] so
/// a test can interleave a GC/redaction pass into the race window; the
/// verb itself is [`backup`] (begin, then finish — the enforced order).
pub struct BackupSession {
    store: Store,
    ids: Vec<naatre_core::ids::Id>,
    changesets: i64,
    /// When the DB snapshot completed (nanos) — the grace measurement's
    /// origin.
    snapshot_ns: i64,
    root: PathBuf,
    _lock: naatre_store::LockGuard,
}

/// `naatre backup --data <dir> <dest>`, phase 1 of 2 (see [`BackupSession`]).
#[doc(hidden)]
pub fn backup_begin(
    data: &Path,
    cfg: &BackendConfig,
    dest: &Path,
) -> Result<BackupSession, BackupError> {
    let data_dir = DataDir::open_any(data);
    check_data_dir_backend(data_dir.path(), cfg).map_err(BackupError::Store)?;
    let lock = data_dir.lock().map_err(BackupError::Store)?;
    let root = data_dir.path().to_path_buf();
    if dest.exists() {
        return Err(BackupError::OutputExists(dest.to_path_buf()));
    }
    std::fs::create_dir_all(dest).map_err(|e| BackupError::io(dest, e))?;
    // The backup dir holds the whole instance's personal data: 0700.
    chmod(dest, 0o700)?;

    // The CAS object universe first: one listing backs both the fs copy
    // and the manifest (the flock makes it a consistent universe — nothing
    // else can write while it is held).
    let cas = naatre_store::blob::history_cas(&data_dir, cfg).map_err(BackupError::Store)?;
    let mut ids = cas.ids().map_err(BackupError::Store)?;
    ids.sort_by_key(|id| id.to_hex());
    let store = Store::open_backend(data_dir, cfg).map_err(BackupError::Store)?;
    let changesets: i64 = store
        .conn()
        .query_row_map(&Sql::plain("SELECT COUNT(*) FROM changesets"), &[], |r| {
            r.get(0)
        })
        .map_err(store_db)?;

    // 1. The consistent DB snapshot FIRST — per meta backend.
    match cfg.meta {
        MetaKind::Sqlite => {
            dump_sqlite(store.sqlite_conn(), &dest.join(SQLITE_DUMP))?;
            chmod(&dest.join(SQLITE_DUMP), 0o600)?;
        }
        MetaKind::Postgres => {
            let pg = cfg.pg.as_ref().ok_or(BackupError::Manifest {
                path: root.clone(),
                why: "meta = postgres requires a parsed DSN (the config parser guarantees it)",
            })?;
            let sql_path = dest.join(PG_DUMP).display().to_string();
            let mut args: Vec<&str> = vec!["--file", &sql_path];
            let schema;
            if let Some(s) = &pg.schema {
                schema = s.clone();
                args.extend(["--schema", &schema]);
            }
            run_pg_tool("pg_dump", pg, &args, dest)?;
            chmod(&dest.join(PG_DUMP), 0o600)?;
        }
    }

    Ok(BackupSession {
        store,
        ids,
        changesets,
        snapshot_ns: now_ns(),
        root,
        _lock: lock,
    })
}

/// `naatre backup --data <dir> <dest>`, phase 2 of 2: copy the immutable
/// objects (tolerantly — a vanished id lands in the report, not a
/// failure), then write the manifest LAST with every artifact's SHA256.
#[doc(hidden)]
pub fn backup_finish(
    session: BackupSession,
    cfg: &BackendConfig,
    dest: &Path,
) -> Result<BackupReport, BackupError> {
    let BackupSession {
        store,
        ids,
        changesets,
        snapshot_ns,
        root,
        ..
    } = session;
    let grace_secs = cfg
        .gc_grace_secs
        .unwrap_or(crate::gc::GC_DEFAULT_GRACE_SECS);
    // At-or-past grace flags: with a zero grace window every elapsed time
    // qualifies, and sub-second equality against a real window is noise —
    // warning is the operator-safe side.
    let past_grace = now_ns().saturating_sub(snapshot_ns) >= (grace_secs as i64) * 1_000_000_000;

    // 2. THEN the immutable objects — per blob backend. A missing source
    // (a redaction purge raced the snapshot) is RECORDED, not fatal: the
    // report names it and the manifest lists only what the backup carries.
    let mut missing: Vec<String> = Vec::new();
    let mut artifacts: Vec<ArtifactChecksum> = Vec::new();
    let bucket_copy_duty = cfg.blob == BlobKind::S3;
    if cfg.blob == BlobKind::Fs {
        let out_objects = dest.join("objects");
        for id in &ids {
            let hex = id.to_hex();
            let src = shard_path(&root.join(naatre_store::data_dir::OBJECTS_DIR), id);
            let dst = shard_path(&out_objects, id);
            if let Some(parent) = dst.parent() {
                std::fs::create_dir_all(parent).map_err(|e| BackupError::io(parent, e))?;
            }
            // Read once, hash, write — the artifact checksum is the exact
            // bytes the backup carries.
            match std::fs::read(&src) {
                Ok(bytes) => {
                    let sha = format!("{:x}", sha2::Sha256::digest(&bytes));
                    std::fs::write(&dst, &bytes).map_err(|e| BackupError::io(&dst, e))?;
                    chmod(&dst, 0o600)?;
                    artifacts.push(ArtifactChecksum {
                        path: format!("objects/{}/{}/{}", &hex[0..2], &hex[2..4], &hex[4..]),
                        sha256: sha,
                    });
                }
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => missing.push(hex),
                Err(e) => return Err(BackupError::io(&src, e)),
            }
        }
    }
    // The DB artifact's checksum (whichever meta backend ran in phase 1).
    let db_artifact = match cfg.meta {
        MetaKind::Sqlite => SQLITE_DUMP,
        MetaKind::Postgres => PG_DUMP,
    };
    artifacts.push(ArtifactChecksum {
        path: db_artifact.to_string(),
        sha256: sha256_file(&dest.join(db_artifact))?,
    });
    artifacts.sort_by(|a, b| a.path.cmp(&b.path));

    // 3. The manifest LAST: an interrupted run leaves no manifest, and
    // restore refuses a manifest-less directory. For fs the list is what
    // the copy captured (a raced-away id is in the report, not here); for
    // S3 nothing is copied, so the list is the full bucket listing — the
    // restore presence gate is what catches bucket-side gaps.
    let carried: Vec<String> = match cfg.blob {
        BlobKind::Fs => artifacts
            .iter()
            .filter(|a| a.path.starts_with("objects/"))
            .map(|a| a.path.trim_start_matches("objects/").replace('/', ""))
            .collect(),
        BlobKind::S3 => ids.iter().map(|id| id.to_hex()).collect(),
    };
    let manifest = BackupManifest {
        format: MANIFEST_FORMAT,
        naatre_version: env!("CARGO_PKG_VERSION").to_string(),
        created_ns: now_ns(),
        meta: cfg.meta.as_str().to_string(),
        blob: cfg.blob.as_str().to_string(),
        schema_version: store.schema_version(),
        counts: BackupCounts {
            changesets,
            objects: carried.len() as u64,
        },
        objects: carried,
        artifacts,
    };
    let manifest_path = dest.join("manifest.json");
    let json = serde_json::to_string_pretty(&manifest).expect("manifest serializes");
    std::fs::write(&manifest_path, json).map_err(|e| BackupError::io(&manifest_path, e))?;
    chmod(&manifest_path, 0o600)?;

    Ok(BackupReport {
        dest: dest.to_path_buf(),
        meta: cfg.meta,
        blob: cfg.blob,
        schema_version: manifest.schema_version,
        counts: manifest.counts,
        dump_file: match cfg.meta {
            MetaKind::Postgres => Some(PG_DUMP),
            MetaKind::Sqlite => None,
        },
        bucket_copy_duty,
        missing,
        past_grace,
    })
}

/// `naatre backup --data <dir> <dest>`: snapshot the whole data dir for
/// the CONFIGURED backend (honors `backend.toml` exactly like
/// `serve`/`import`) into ONE new directory. Holds the data-dir flock for
/// the run (a running server or import excludes it, and vice versa).
pub fn backup(data: &Path, cfg: &BackendConfig, dest: &Path) -> Result<BackupReport, BackupError> {
    backup_finish(backup_begin(data, cfg, dest)?, cfg, dest)
}

/// Restore's staging directory: a SIBLING of the target (same filesystem,
/// so the final DB write is a rename), removed on drop — a failed restore
/// leaves nothing behind and, before the move, nothing in the target.
struct StageDir {
    path: PathBuf,
}

impl Drop for StageDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

/// Restore's staging directory, a sibling of the target.
fn stage_dir(target: &Path) -> Result<StageDir, BackupError> {
    let parent = target.parent().unwrap_or(target);
    let nanos = now_ns();
    let path = parent.join(format!("{STAGE_PREFIX}{}-{nanos}", std::process::id()));
    std::fs::create_dir_all(&path).map_err(|e| BackupError::io(&path, e))?;
    chmod(&path, 0o700)?;
    Ok(StageDir { path })
}

/// `naatre restore --data <target> <backup-dir>`: restore ONE backup
/// directory into a data dir for the CONFIGURED backend. Verifies the
/// manifest and every artifact's SHA256 into a staging directory BEFORE
/// any write to the target (all-or-nothing), refuses a combo mismatch,
/// refuses a non-empty target unless `force` (an initialized Postgres
/// schema is refused regardless — dropping it is the wipe), restores DB
/// then objects, and directs the operator to fsck.
pub fn restore(
    target: &Path,
    cfg: &BackendConfig,
    backup_dir: &Path,
    force: bool,
) -> Result<RestoreReport, BackupError> {
    let manifest = load_manifest(&backup_dir.join("manifest.json"))?;
    // Combo agreement FIRST (the check_data_dir_backend convention: name
    // both sides, refuse loudly).
    if manifest.meta != cfg.meta.as_str() || manifest.blob != cfg.blob.as_str() {
        return Err(BackupError::ComboMismatch {
            manifest_meta: manifest.meta.clone(),
            manifest_blob: manifest.blob.clone(),
            configured_meta: cfg.meta.as_str().to_string(),
            configured_blob: cfg.blob.as_str().to_string(),
        });
    }
    // The target guard — check_data_dir_backend's convention with the
    // uninitialized arms INVERTED: restore expects an uninitialized dir
    // and refuses every marker of a live or half-wiped one, naming what
    // it found and the --force remedy. `force` overrides the sqlite and
    // fs findings; a stray naatre.db under a postgres config and an
    // INITIALIZED postgres schema are refused regardless — dropping the
    // schema is the operator's wipe, not a flag's business.
    let data_dir = DataDir::open_any(target);
    let root = data_dir.path().to_path_buf();
    if root.join(DB_FILE).exists() {
        match cfg.meta {
            MetaKind::Sqlite if force => {}
            MetaKind::Sqlite => {
                return Err(BackupError::TargetInitialized {
                    path: target.to_path_buf(),
                    found: "naatre.db present",
                });
            }
            MetaKind::Postgres => {
                return Err(StoreError::Backend(BackendError::Mismatch {
                    on_disk: "sqlite (naatre.db present)",
                    configured: "postgres",
                })
                .into());
            }
        }
    }
    // The target dir itself may be gone (a wiped data dir usually is);
    // it is created AFTER staging — a refused restore leaves the target
    // untouched, not even created.
    if objects_dir_has_entries(target) {
        match cfg.blob {
            BlobKind::S3 => {
                return Err(StoreError::Backend(BackendError::Mismatch {
                    on_disk: "fs (objects/ has data)",
                    configured: "s3",
                })
                .into());
            }
            BlobKind::Fs if force => {}
            BlobKind::Fs => {
                return Err(BackupError::TargetNotEmpty {
                    path: target.to_path_buf(),
                    found: "objects/ holds data",
                });
            }
        }
    }
    match cfg.meta {
        MetaKind::Sqlite => {}
        MetaKind::Postgres => {
            if postgres_initialized(cfg, target)? {
                return Err(BackupError::TargetInitialized {
                    path: target.to_path_buf(),
                    found: "the postgres schema carries the naatre schema (drop it first — \
                            the drop IS the wipe; --force does not apply here)",
                });
            }
            drop_empty_schema(cfg);
        }
    }
    // Manifest/artifact agreement BEFORE anything is staged: every object
    // the fs backup carries must have an artifact line (the S3 combo
    // carries no object files — the bucket does).
    if cfg.blob == BlobKind::Fs {
        for hex in &manifest.objects {
            let want = format!("objects/{}/{}/{}", &hex[0..2], &hex[2..4], &hex[4..]);
            if !manifest.artifacts.iter().any(|a| a.path == want) {
                return Err(BackupError::Manifest {
                    path: backup_dir.to_path_buf(),
                    why: "the manifest lists an object without an artifact checksum",
                });
            }
        }
    }

    // STAGE + VERIFY: every manifest artifact is copied into the staging
    // directory and checksummed. Any refusal up to and including this
    // point leaves the target untouched.
    let stage = stage_dir(target)?;
    for artifact in &manifest.artifacts {
        let src = backup_dir.join(&artifact.path);
        let dst = stage.path.join(&artifact.path);
        if let Some(parent) = dst.parent() {
            std::fs::create_dir_all(parent).map_err(|e| BackupError::io(parent, e))?;
        }
        if !src.is_file() {
            return Err(BackupError::Manifest {
                path: backup_dir.to_path_buf(),
                why: "the manifest lists an artifact the backup directory does not carry",
            });
        }
        let got = sha256_file(&src)?;
        if got != artifact.sha256 {
            return Err(BackupError::ChecksumMismatch {
                path: artifact.path.clone(),
            });
        }
        std::fs::copy(&src, &dst).map_err(|e| BackupError::io(&dst, e))?;
    }

    // Every artifact verified. NOW the target: create it 0700 like init
    // (a second initialization guard against a racer with the check
    // above) and take the flock for the write phase.
    if !root.exists() {
        DataDir::create(target).map_err(BackupError::Store)?;
    } else {
        std::fs::create_dir_all(target).map_err(|e| BackupError::io(target, e))?;
    }
    let _lock = data_dir.lock().map_err(BackupError::Store)?;

    let ids: Vec<naatre_core::ids::Id> = manifest
        .objects
        .iter()
        .map(|hex| naatre_core::ids::Id::from_hex(hex).expect("load_manifest validated the ids"))
        .collect();

    // 1. The DB first — the mirror of the backup order. The sqlite write
    // is a RENAME of the verified staged file (atomic, same filesystem);
    // psql reads the verified staged dump.
    match cfg.meta {
        MetaKind::Sqlite => {
            let staged = stage.path.join(SQLITE_DUMP);
            let dst = root.join(DB_FILE);
            std::fs::rename(&staged, &dst).map_err(|e| BackupError::io(&dst, e))?;
            chmod(&dst, 0o600)?;
        }
        MetaKind::Postgres => {
            let pg = cfg.pg.as_ref().ok_or(BackupError::Manifest {
                path: backup_dir.to_path_buf(),
                why: "the target config says meta = postgres but carries no parsed DSN",
            })?;
            let staged = stage.path.join(PG_DUMP);
            let staged_path = staged.display().to_string();
            let args = [
                "--file",
                &staged_path,
                "--set",
                "ON_ERROR_STOP=1",
                "--quiet",
            ];
            run_pg_tool("psql", pg, &args, backup_dir)?;
        }
    }

    // The open validates the restored DB and forward-migrates if the
    // backup predates this binary's schema (a too-new backup fails the
    // migration guard loudly); the handle holds the connection for the
    // presence gate below.
    let cas = naatre_store::blob::history_cas(&data_dir, cfg).map_err(BackupError::Store)?;
    let _store = Store::open_backend(data_dir, cfg).map_err(BackupError::Store)?;

    // 2. THEN the objects — moved out of verified staging.
    if cfg.blob == BlobKind::Fs {
        for id in &ids {
            let hex = id.to_hex();
            let src = stage.path.join(format!(
                "objects/{}/{}/{}",
                &hex[0..2],
                &hex[2..4],
                &hex[4..]
            ));
            let dst = shard_path(&root.join(naatre_store::data_dir::OBJECTS_DIR), id);
            if let Some(parent) = dst.parent() {
                std::fs::create_dir_all(parent).map_err(|e| BackupError::io(parent, e))?;
            }
            std::fs::copy(&src, &dst).map_err(|e| BackupError::io(&src, e))?;
            chmod(&dst, 0o600)?;
        }
    }
    drop(stage);

    // 3. The presence gate — every manifest id must be in the target CAS
    // (fs: just moved; S3: the operator's bucket copy). Restore checks
    // PRESENCE, not reachability; fsck is the operator's next step.
    let have = cas.has(&ids).map_err(BackupError::Store)?;
    let missing: Vec<String> = ids
        .iter()
        .zip(&have)
        .filter(|(_, present)| !**present)
        .map(|(id, _)| id.to_hex())
        .collect();
    if !missing.is_empty() {
        let sample = missing
            .iter()
            .take(3)
            .cloned()
            .collect::<Vec<_>>()
            .join(", ");
        return Err(BackupError::ObjectsMissing { missing, sample });
    }

    Ok(RestoreReport {
        meta: cfg.meta,
        blob: cfg.blob,
        schema_version: manifest.schema_version,
        counts: manifest.counts,
        objects_present: manifest.objects.len() as u64,
        forced: force,
    })
}

/// Best-effort drop of an EMPTY configured test schema. `Db`'s session
/// setup runs `CREATE SCHEMA IF NOT EXISTS` on every connect, so the
/// initialized-probe above leaves an empty schema behind; the dump's own
/// `CREATE SCHEMA` would then fail to replay. NO CASCADE: a non-empty
/// schema (foreign tables, a half-initialized store) fails the plain drop
/// and the `psql` run fails loudly right after — restore never destroys
/// unknown objects. Production (schema `None`) connects without the
/// schema setup and needs none of this.
fn drop_empty_schema(cfg: &BackendConfig) {
    let Some(pg) = &cfg.pg else {
        return;
    };
    let Some(schema) = &pg.schema else {
        return;
    };
    if schema.is_empty()
        || !schema
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '_')
    {
        return;
    }
    if let Ok(conn) = Db::connect_postgres(pg, false) {
        let _ = conn.execute(&Sql::plain_owned(format!("DROP SCHEMA \"{schema}\"")), &[]);
    }
}

/// Whether the configured Postgres database/schema already carries the
/// naatre schema (the restore clobber guard). A probe error (schema
/// absent, permissions) reads as uninitialized — `psql` then gives the
/// loud, logged failure if the target is truly unrestorable.
fn postgres_initialized(cfg: &BackendConfig, target: &Path) -> Result<bool, BackupError> {
    let pg = cfg.pg.as_ref().ok_or(BackupError::Manifest {
        path: target.to_path_buf(),
        why: "meta = postgres requires a parsed DSN (the config parser guarantees it)",
    })?;
    let conn = Db::connect_postgres(pg, false)
        .map_err(StoreError::Db)
        .map_err(BackupError::Store)?;
    let sql = match &pg.schema {
        Some(schema) => {
            // Test-isolation schemas (pg_provision generates them): quote
            // the identifier, refusing anything outside the generated
            // grammar rather than trying to escape it.
            if schema.is_empty()
                || !schema
                    .chars()
                    .all(|c| c.is_ascii_alphanumeric() || c == '_')
            {
                return Err(BackupError::Manifest {
                    path: target.to_path_buf(),
                    why: "the configured postgres schema name is not a plain identifier",
                });
            }
            Sql::plain_owned(format!(
                "SELECT value FROM \"{schema}\".meta WHERE key = 'schema_version'"
            ))
        }
        None => Sql::plain("SELECT value FROM meta WHERE key = 'schema_version'"),
    };
    Ok(conn
        .query_opt(&sql, &[])
        .map(|row| row.is_some())
        .unwrap_or(false))
}

/// The SQLite consistent snapshot: `VACUUM INTO` a fresh file (single
/// artifact, no WAL juggling). The target must not exist — the caller
/// created the backup dir empty.
fn dump_sqlite(conn: &rusqlite::Connection, dest: &Path) -> Result<(), BackupError> {
    // The path rides as a bound string parameter (an expression in the
    // INTO clause), never spliced into SQL text.
    let target = dest.to_string_lossy().into_owned();
    conn.execute("VACUUM INTO ?1", rusqlite::params![target])?;
    Ok(())
}

/// Builds the pg_dump/psql command: the parsed [`PgConnect`] parameters as
/// libpq ENVIRONMENT (never argv — no DSN, no password on a command line;
/// argv leaks through ps). Split from [`run_pg_tool`] so the pin can
/// assert on the built command.
fn pg_command(program: &Path, pg: &PgConnect, args: &[&str]) -> Command {
    let mut cmd = Command::new(program);
    cmd.env("PGHOST", &pg.host)
        .env("PGPORT", pg.port.to_string())
        .env("PGDATABASE", &pg.dbname);
    if let Some(user) = &pg.user {
        cmd.env("PGUSER", user);
    }
    if let Some(password) = &pg.password {
        cmd.env("PGPASSWORD", password);
    }
    cmd.env(
        "PGSSLMODE",
        match pg.tls {
            naatre_store::backend::PgTls::Disable => "disable",
            naatre_store::backend::PgTls::Require => "require",
            naatre_store::backend::PgTls::VerifyCa => "verify-ca",
            naatre_store::backend::PgTls::VerifyFull => "verify-full",
        },
    );
    if let Some(cert) = &pg.sslrootcert {
        cmd.env("PGSSLROOTCERT", cert);
    }
    cmd.args(args);
    cmd
}

/// Runs one PostgreSQL client tool. On failure the tool's stderr is
/// written to `<context>/tool-error.log` and the error carries the exit
/// status only.
fn run_pg_tool(
    tool: &'static str,
    pg: &PgConnect,
    args: &[&str],
    context: &Path,
) -> Result<(), BackupError> {
    let program = resolve_tool(tool)?;
    let mut cmd = pg_command(&program, pg, args);
    let out = cmd.output().map_err(|e| BackupError::io(context, e))?;
    if !out.status.success() {
        let log_path = context.join(TOOL_LOG);
        std::fs::write(&log_path, &out.stderr).map_err(|e| BackupError::io(&log_path, e))?;
        return Err(BackupError::ToolFailed {
            tool,
            status: out.status,
            log: log_path,
        });
    }
    Ok(())
}

fn now_ns() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_tool_is_a_clean_named_error() {
        let e = resolve_tool("naatre-definitely-not-a-tool").unwrap_err();
        let msg = e.to_string();
        assert!(matches!(
            e,
            BackupError::MissingTool {
                tool: "naatre-definitely-not-a-tool"
            }
        ));
        assert!(msg.contains("PostgreSQL client tools"), "{msg}");
        assert!(msg.contains("documented external dependency"), "{msg}");
    }

    #[test]
    fn pg_password_never_rides_argv() {
        let pg = PgConnect {
            host: "db.example".into(),
            port: 5433,
            dbname: "naatre".into(),
            user: Some("naatre".into()),
            password: Some("naatre-secret-marker".into()),
            tls: naatre_store::backend::PgTls::VerifyFull,
            sslrootcert: None,
            schema: Some("naatre_t_probe".into()),
        };
        let sql_path = "/tmp/naatre-probe/postgres.sql";
        let cmd = pg_command(
            Path::new("/usr/bin/pg_dump"),
            &pg,
            &["--file", sql_path, "--schema", "naatre_t_probe"],
        );
        // The password is in the environment, NEVER an argument.
        let argv: Vec<String> = cmd
            .get_args()
            .map(|a| a.to_string_lossy().into_owned())
            .collect();
        assert!(
            argv.iter().all(|a| !a.contains("naatre-secret-marker")),
            "password leaked into argv: {argv:?}"
        );
        let envs: Vec<(String, String)> = cmd
            .get_envs()
            .filter_map(|(k, v)| {
                v.map(|v| {
                    (
                        k.to_string_lossy().into_owned(),
                        v.to_string_lossy().into_owned(),
                    )
                })
            })
            .collect();
        assert!(
            envs.iter()
                .any(|(k, v)| k == "PGPASSWORD" && v == "naatre-secret-marker"),
            "PGPASSWORD env missing: {envs:?}"
        );
        // No env value carries the password under another name either
        // (PGHOST/PGUSER/PGDATABASE are the only connection carriers).
        for (k, v) in &envs {
            if k != "PGPASSWORD" {
                assert!(
                    !v.contains("naatre-secret-marker"),
                    "{k} leaks the password"
                );
            }
        }
        // The TLS posture and the database ride the env, not argv.
        assert!(
            envs.iter()
                .any(|(k, v)| k == "PGSSLMODE" && v == "verify-full")
        );
        assert!(
            !argv
                .iter()
                .any(|a| a.contains("naatre-secret-marker") || a.contains("postgres://"))
        );
    }

    #[test]
    fn manifest_roundtrips_and_refuses_bad_format_and_ids() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("manifest.json");
        let manifest = BackupManifest {
            format: MANIFEST_FORMAT,
            naatre_version: "0.1.0".into(),
            created_ns: 1,
            meta: "sqlite".into(),
            blob: "fs".into(),
            schema_version: naatre_store::MAX_SUPPORTED,
            counts: BackupCounts {
                changesets: 2,
                objects: 1,
            },
            objects: vec!["0".repeat(64)],
            artifacts: vec![ArtifactChecksum {
                path: "naatre.db".into(),
                sha256: "a".repeat(64),
            }],
        };
        std::fs::write(&path, serde_json::to_string_pretty(&manifest).unwrap()).unwrap();
        let read = load_manifest(&path).unwrap();
        assert_eq!(read.objects, vec!["0".repeat(64)]);
        assert_eq!(read.counts.changesets, 2);
        assert_eq!(read.artifacts.len(), 1);

        // A future format: every field present, only the format bumped
        // (a bare {"format":99} is a PARSE refusal — fields are required).
        let mut future_manifest = manifest.clone();
        future_manifest.format = 99;
        std::fs::write(&path, serde_json::to_string(&future_manifest).unwrap()).unwrap();
        assert!(matches!(
            load_manifest(&path).unwrap_err(),
            BackupError::UnsupportedFormat { got: 99 }
        ));

        std::fs::write(&path, "not json").unwrap();
        assert!(matches!(
            load_manifest(&path).unwrap_err(),
            BackupError::Manifest { .. }
        ));

        std::fs::write(
            &path,
            serde_json::to_string(&BackupManifest {
                objects: vec!["zz".into()],
                ..manifest.clone()
            })
            .unwrap(),
        )
        .unwrap();
        assert!(matches!(
            load_manifest(&path).unwrap_err(),
            BackupError::Manifest { .. }
        ));

        // A short/non-hex artifact checksum is refused.
        std::fs::write(
            &path,
            serde_json::to_string(&BackupManifest {
                artifacts: vec![ArtifactChecksum {
                    path: "naatre.db".into(),
                    sha256: "xyz".into(),
                }],
                ..manifest.clone()
            })
            .unwrap(),
        )
        .unwrap();
        assert!(matches!(
            load_manifest(&path).unwrap_err(),
            BackupError::Manifest { .. }
        ));

        // A torn backup (no manifest at all) is refused.
        assert!(matches!(
            load_manifest(&dir.path().join("absent.json")).unwrap_err(),
            BackupError::Manifest { .. }
        ));
    }

    #[test]
    fn stage_dirs_are_siblings_and_cleaned_on_drop() {
        let root = tempfile::tempdir().unwrap();
        let target = root.path().join("data");
        std::fs::create_dir_all(&target).unwrap();
        let stage = stage_dir(&target).unwrap();
        let name = stage
            .path
            .file_name()
            .unwrap()
            .to_string_lossy()
            .into_owned();
        assert!(name.starts_with(STAGE_PREFIX), "{name}");
        assert_eq!(stage.path.parent(), Some(target.parent().unwrap()));
        std::fs::write(stage.path.join("x"), b"staged").unwrap();
        let path = stage.path.clone();
        drop(stage);
        assert!(!path.exists(), "the stage dir is removed on drop");
    }
}
