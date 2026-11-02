//! `naatre migrate-db-per-repo` (M13a): the OFFLINE adoption command that
//! converts an existing single-DB store to the catalog + per-repo layout.
//!
//! The contract, per the plan clause: refuses a live data dir (the flock),
//! copies each repo's rows from the single DB into fresh per-repo DBs
//! (each running the shared SQLite `MIGRATIONS` verbatim — 1..=12), builds
//! the catalog (the global tables + the repo registry with each repo's db
//! path), verifies (row counts per table per repo + a full `fsck` pass
//! against the new layout), and leaves the original single DB in place
//! UNTOUCHED — the operator deletes it after cutover, and the tool prints
//! the cutover steps (flip `backend.toml`'s `[layout] mode`, then delete
//! `naatre.db`; until then the config/data-dir guard keeps the server on
//! the single-DB files, so a crash or an abandoned migration leaves a
//! consistent, servable store).
//!
//! The copy is SQL-level (`ATTACH` + `INSERT … SELECT`), so the byte-level
//! row shapes travel verbatim — no Rust-side row decoding, no type
//! coercion. The source attaches READ-ONLY (a `file:…?mode=ro` URI —
//! SQLite makes attached databases follow main's read-only-ness, so the
//! catalog is main and the source is never opened read-write, never
//! mutated, not even its journal mode; a crashed server's hot WAL must be
//! recovered by a normal open before a read-only source can read it — the
//! adoption refuses with the open error rather than touching the file).
//! On ANY failure the partial targets (catalog.db, repos/) are removed —
//! removal failures are NAMED in the error — and the original single DB
//! stands: the migration is all-or-nothing at the layout level. Success
//! ends with the `adopted` meta row as the catalog's LAST write (after
//! fsck): a catalog without it is a crashed run's partial target, refused
//! by every guard with the incomplete-migration remedy.

use std::path::Path;

use rusqlite::Connection;

use naatre_store::backend::{BackendConfig, MetaKind};
use naatre_store::layout::{CATALOG_FILE, Layout, REPOS_DIR};
use naatre_store::store::open_repo_db;

/// The per-repo tables copied from the single DB into each repo's file
/// (everything repo-scoped, M10b's redactions included — the repo-scoped
/// tombstone semantics keep their rows with their repo).
/// The per-repo tables with a leading `repo_id` column (copied with a
/// WHERE clause).
const REPO_TABLES: &[&str] = &[
    "changesets",
    "changeset_parents",
    "file_changes",
    "repo_blobs",
    "refs",
    "lands",
    "snapshots",
    "git_map",
    "derive_state",
    "derive_reports",
    "blame_state",
    "path_history",
    "redactions",
];
/// The repo-owned tables keyed WITHOUT repo_id (tree rows and entries:
/// content-addressed, one owner file each — the single DB's rows all
/// belong to whichever repo wrote them; the copy moves every row, since
/// the tree universe belongs to the repos that hold it).
const REPO_TABLES_NO_REPO_ID: &[&str] = &["trees", "tree_entries", "repo_trees"];

/// The global tables copied into the catalog.
const CATALOG_TABLES: &[&str] = &[
    "objects",
    "principals",
    "tokens",
    "audit_log",
    "derive_kinds",
    "gc_pending",
];

/// What one repo's adoption did (the report's per-repo row).
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct RepoAdopted {
    pub id: i64,
    pub name: String,
    /// Rows copied per table (table name → row count, the parity check's
    /// per-repo half).
    pub rows: Vec<(String, usize)>,
    /// The per-repo db path, relative to the data dir (`repos/<id>.db`).
    pub db_file: String,
}

/// What one adoption did (for the CLI's printout and the tests).
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct AdoptReport {
    pub repos: Vec<RepoAdopted>,
    /// Rows copied per global table (catalog side).
    pub catalog_rows: Vec<(String, usize)>,
    /// The fsck verdict against the NEW layout (must be clean for the
    /// migration to stand).
    pub fsck_clean: bool,
}

#[derive(Debug, thiserror::Error)]
pub enum AdoptError {
    #[error("data dir is live (another naatre process holds the lockfile)")]
    Live,
    #[error("migration refused: {0}")]
    NotSingleDb(String),
    #[error(
        "db-per-repo is a SQLite layout: the config selects meta = postgres \
         (per-repo files are SQLite; a Postgres deployment never splits)"
    )]
    PostgresUnsupported,
    #[error(
        "adoption failed at {step}: {source} ({cleanup}; \
             the original single DB is untouched)"
    )]
    Failed {
        step: &'static str,
        #[source]
        source: anyhow_source::AnyhowSource,
        /// What the partial-target cleanup actually did: "partial targets
        /// removed" on success, or the named removal failures (the old
        /// text claimed removal unconditionally — a cleanup that could not
        /// remove catalog.db left a marker-less partial target and the
        /// error lied about it).
        cleanup: String,
    },
}

/// A `Display`-able, `Error`-able wrapper over any error text (the copy
/// steps fail with SQL or io errors of several shapes; the report names
/// the step and keeps the message).
pub mod anyhow_source {
    #[derive(Debug)]
    pub struct AnyhowSource(pub String);

    impl std::fmt::Display for AnyhowSource {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            f.write_str(&self.0)
        }
    }

    impl std::error::Error for AnyhowSource {}

    impl AnyhowSource {
        pub fn from_err(e: impl std::fmt::Display) -> Self {
            AnyhowSource(e.to_string())
        }
    }
}

use anyhow_source::AnyhowSource;

fn failed(step: &'static str, e: impl std::fmt::Display) -> AdoptError {
    AdoptError::Failed {
        step,
        source: AnyhowSource::from_err(e),
        cleanup: "partial targets removed".to_string(),
    }
}

/// [`failed`] carrying the cleanup's own failures (the error names what
/// could not be removed — the operator needs to know a partial target
/// remains).
fn failed_with_cleanup(
    step: &'static str,
    e: impl std::fmt::Display,
    cleanup_failures: Vec<String>,
) -> AdoptError {
    AdoptError::Failed {
        step,
        source: AnyhowSource::from_err(e),
        cleanup: if cleanup_failures.is_empty() {
            "partial targets removed".to_string()
        } else {
            format!(
                "PARTIAL TARGETS REMAIN: {} — remove them by hand before re-running",
                cleanup_failures.join("; ")
            )
        },
    }
}

/// The adoption's crash point (debug builds only, the M4
/// `NAATRE_CRASH_POINT` pattern, its own OnceLock — same contract as
/// [`crate::gc::crash_point`]):
///
/// * `adopt_mid_copy` — mid-copy, after the catalog schema and the first
///   repo's rows have landed: the fault-injected partial-migration shape
///   the refusal pins need (a marker-less `catalog.db` the guards refuse).
#[cfg(debug_assertions)]
pub fn crash_point(name: &str) {
    use std::sync::OnceLock;
    static POINT: OnceLock<Option<String>> = OnceLock::new();
    let want = POINT.get_or_init(|| std::env::var("NAATRE_CRASH_POINT").ok());
    if want.as_deref() == Some(name) {
        panic!("crash point {name} triggered");
    }
}

/// Release no-op (see the debug build above).
#[cfg(not(debug_assertions))]
pub fn crash_point(_name: &str) {}

/// A SQL string literal with `''` escaping (the ATTACH targets' paths are
/// operator-supplied and may contain quotes; the old `{:?}`-formatted
/// ATTACH broke on them and was never a real SQL literal).
fn sql_literal(s: &str) -> String {
    format!("'{}'", s.replace('\'', "''"))
}

/// Percent-escapes the URI-special bytes of a filesystem path for a
/// `file:` URI (the read-only source ATTACH).
fn uri_escape(path: &str) -> String {
    let mut out = String::with_capacity(path.len());
    for b in path.bytes() {
        match b {
            b'?' | b'#' | b'%' => out.push_str(&format!("%{b:02X}")),
            _ => out.push(b as char),
        }
    }
    out
}

/// Runs the offline adoption. `data` is the data dir; `cfg` its parsed
/// backend config (the CAS backend carries over — the blob store is
/// untouched by the layout split).
pub fn adopt_db_per_repo(data: &Path, cfg: &BackendConfig) -> Result<AdoptReport, AdoptError> {
    if cfg.meta == MetaKind::Postgres {
        return Err(AdoptError::PostgresUnsupported);
    }
    let db_path = data.join(naatre_store::data_dir::DB_FILE);
    let catalog_path = data.join(CATALOG_FILE);
    let repos_dir = data.join(REPOS_DIR);
    if !db_path.exists() {
        return Err(AdoptError::NotSingleDb(format!(
            "no naatre.db in {}",
            data.display()
        )));
    }
    // A stale target refuses with the shape it is in: a COMPLETE catalog
    // (the adopted marker) is a finished migration — re-running means
    // deleting it first; a MARKER-LESS catalog is a crashed adoption's
    // partial target — never served, never mistaken for complete, and a
    // re-run against it refuses with the incomplete-migration remedy.
    if catalog_path.exists() {
        let complete = naatre_store::store::store_has_adopted_marker(data);
        return Err(AdoptError::NotSingleDb(if complete {
            "catalog.db already exists and the adoption COMPLETED (the adopted \
             marker is present) — the split is servable; delete catalog.db and \
             repos/ only if you mean to abandon db-per-repo (post-cutover writes \
             are lost) and start over"
                .to_string()
        } else {
            "incomplete migration — catalog.db carries no adopted marker: the \
             previous adoption died mid-copy. Delete catalog.db and repos/ and \
             re-run `naatre migrate-db-per-repo`"
                .to_string()
        }));
    }
    // A pre-existing repos/ refuses up front: stale per-repo files from an
    // earlier run would be silently overwritten by id-keyed copies (the
    // operator must clear them consciously).
    if repos_dir.exists() {
        return Err(AdoptError::NotSingleDb(format!(
            "refusing to adopt: {} already exists (a previous run's or another \
             store's per-repo files) — remove it first; the adoption writes \
             every repo file itself",
            repos_dir.display()
        )));
    }
    // The flock: a running server (or an in-flight import) refuses the
    // migration — the plan's offline clause.
    let data_dir = naatre_store::DataDir::open_any(data);
    let _lock = match data_dir.lock() {
        Ok(lock) => lock,
        Err(naatre_store::StoreError::LockHeld(_)) => return Err(AdoptError::Live),
        Err(e) => return Err(failed("flock", e)),
    };

    // The copy runs on ONE raw connection holding the CATALOG (main,
    // read-write) with the source attached READ-ONLY — the original single
    // DB is never opened read-write, never mutated, not even its journal
    // mode. (SQLite makes ATTACHed databases follow main's read-only-ness,
    // so the source cannot BE the read-only main.) The ATTACH is a file:
    // URI with mode=ro; a crashed server's hot WAL must be recovered
    // before a read-only open can read it — the adoption refuses with the
    // open error rather than touching the source. Per repo the repo file
    // attaches read-write for its copy. ATTACH + INSERT…SELECT, no
    // Rust-side row handling.
    let work = || -> Result<AdoptReport, AdoptError> {
        // The catalog: created here (main), its OWN migration list runs
        // (the global tables + the registry; the per-repo files run the
        // shared list). The adopted marker is NOT here — it is the
        // adoption's LAST write, after fsck passes.
        let conn = Connection::open(&catalog_path).map_err(|e| failed("catalog create", e))?;
        naatre_store::catalog::migrate_catalog(&conn).map_err(|e| failed("catalog create", e))?;
        conn.execute_batch(&format!(
            "ATTACH DATABASE {} AS src;",
            sql_literal(&format!(
                "file:{}?mode=ro",
                uri_escape(&db_path.display().to_string())
            )),
        ))
        .map_err(|e| failed("open source", e))?;

        // The repo registry first (ids must exist before the per-repo files).
        let repos: Vec<(i64, String, i64)> = {
            let mut stmt = conn
                .prepare("SELECT id, name, created_ns FROM src.repos ORDER BY id ASC")
                .map_err(|e| failed("registry read", e))?;
            stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))
                .map_err(|e| failed("registry read", e))?
                .collect::<Result<Vec<_>, _>>()
                .map_err(|e| failed("registry read", e))?
        };
        let mut report = AdoptReport {
            repos: Vec::new(),
            catalog_rows: Vec::new(),
            fsck_clean: false,
        };
        std::fs::create_dir_all(&repos_dir).map_err(|e| failed("repos dir", e))?;

        for (id, name, created_ns) in &repos {
            let db_file = format!("{REPOS_DIR}/{id}.db");
            let repo_path = data.join(&db_file);
            // The per-repo file: the shared migrations run verbatim
            // (1..=12 — the global tables exist in it and stay empty).
            {
                let _repo_db = open_repo_db(&repo_path).map_err(|e| failed("repo db create", e));
            }
            conn.execute_batch(&format!(
                "ATTACH DATABASE {} AS repo_db;",
                sql_literal(&repo_path.display().to_string())
            ))
            .map_err(|e| failed("attach repo", e))?;
            // The registry row: the catalog's shape adds the db path.
            conn.execute(
                "INSERT INTO repos(id, name, created_ns, db_file) \
                 VALUES (?1, ?2, ?3, ?4)",
                rusqlite::params![id, name, created_ns, db_file],
            )
            .map_err(|e| failed("registry write", e))?;
            // The repo's rows, verbatim. The repo_id-led tables copy with
            // a WHERE; the tree tables (keyed by tree id, no repo_id
            // column) copy in full — the single DB's tree rows belong to
            // the repos that wrote them, and the tree universe moves
            // wholesale.
            let mut counts: Vec<(String, usize)> = Vec::new();
            for table in REPO_TABLES {
                let n = conn
                    .execute(
                        &format!("INSERT INTO repo_db.{table} SELECT * FROM src.{table} WHERE repo_id = ?1"),
                        rusqlite::params![id],
                    )
                    .map_err(|e| failed("repo copy", e))?;
                counts.push(((*table).to_string(), n));
            }
            for table in REPO_TABLES_NO_REPO_ID {
                let n = conn
                    .execute(
                        &format!("INSERT INTO repo_db.{table} SELECT * FROM src.{table}"),
                        [],
                    )
                    .map_err(|e| failed("repo copy", e))?;
                counts.push(((*table).to_string(), n));
            }
            // Parity check: the source's counts for this repo must equal
            // what landed in the file (the tree tables: the source's GLOBAL
            // count — every repo's file carries the full set; the counts
            // are asserted identical per repo file, which pins the
            // wholesale copy).
            for (table, _) in &counts {
                let scoped = !REPO_TABLES_NO_REPO_ID.contains(&table.as_str());
                let where_clause = if scoped {
                    format!(" WHERE repo_id = {id}")
                } else {
                    String::new()
                };
                let source: i64 = conn
                    .query_row(
                        &format!("SELECT COUNT(*) FROM src.{table}{where_clause}"),
                        [],
                        |r| r.get(0),
                    )
                    .map_err(|e| failed("parity read", e))?;
                let copied: i64 = conn
                    .query_row(&format!("SELECT COUNT(*) FROM repo_db.{table}"), [], |r| {
                        r.get(0)
                    })
                    .map_err(|e| failed("parity read", e))?;
                if source != copied {
                    return Err(failed(
                        "parity",
                        format!("repo {name:?} table {table}: source {source} != copied {copied}"),
                    ));
                }
            }
            report.repos.push(RepoAdopted {
                id: *id,
                name: name.clone(),
                rows: counts,
                db_file,
            });
            conn.execute_batch("DETACH DATABASE repo_db;")
                .map_err(|e| failed("detach repo", e))?;
        }

        // The fault-injection point: every repo file has landed, the
        // global copies have not (the crash pin's partial-migration shape
        // — a marker-less catalog the guards refuse).
        crash_point("adopt_mid_copy");

        // The global tables: verbatim into the catalog.
        for table in CATALOG_TABLES {
            let n = conn
                .execute(
                    &format!("INSERT INTO {table} SELECT * FROM src.{table}"),
                    [],
                )
                .map_err(|e| failed("catalog copy", e))?;
            report.catalog_rows.push(((*table).to_string(), n));
        }
        // The catalog's meta: everything except the schema version (the
        // per-file marker) — the boot-backfill completion markers are
        // GLOBAL state and must survive the split.
        {
            let markers: Vec<(String, String)> = {
                let mut stmt = conn
                    .prepare("SELECT key, value FROM src.meta WHERE key != 'schema_version'")
                    .map_err(|e| failed("meta read", e))?;
                stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))
                    .map_err(|e| failed("meta read", e))?
                    .collect::<Result<Vec<_>, _>>()
                    .map_err(|e| failed("meta read", e))?
            };
            let mut n = 0usize;
            for (key, value) in markers {
                conn.execute(
                    "INSERT INTO meta(key, value) VALUES (?1, ?2)",
                    rusqlite::params![key, value],
                )
                .map_err(|e| failed("meta copy", e))?;
                n += 1;
            }
            report.catalog_rows.push(("meta".to_string(), n));
        }
        // Final parity: the catalog's global counts equal the source's.
        for (table, _) in &report.catalog_rows {
            if table == "meta" {
                continue;
            }
            let source: i64 = conn
                .query_row(&format!("SELECT COUNT(*) FROM src.{table}"), [], |r| {
                    r.get(0)
                })
                .map_err(|e| failed("parity read", e))?;
            let copied: i64 = conn
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |r| r.get(0))
                .map_err(|e| failed("parity read", e))?;
            if source != copied {
                return Err(failed(
                    "parity",
                    format!("catalog table {table}: source {source} != copied {copied}"),
                ));
            }
        }
        Ok(report)
    };

    let mut report = match work() {
        Ok(r) => r,
        Err(e) => {
            // All-or-nothing: remove the partial targets, the original
            // stands. A cleanup that cannot remove a target says so (the
            // marker-less partial target is refused by every guard, but
            // the error must not claim removal that never happened).
            let failures = cleanup(&catalog_path, &repos_dir);
            return Err(failed_with_cleanup(
                match &e {
                    AdoptError::Failed { step, .. } => step,
                    _ => "adoption",
                },
                match &e {
                    AdoptError::Failed { source, .. } => source.to_string(),
                    other => other.to_string(),
                },
                failures,
            ));
        }
    };

    // The fsck pass against the NEW layout: the split must serve the same
    // store the single DB did (clean or with exactly the same advisory
    // states a single-DB fsck reports — the check demands clean()). The
    // open bypasses the adopted-marker guard ([ServerState::open_backend_adopt]):
    // the marker is this function's LAST write, after fsck — a crash
    // anywhere above leaves a marker-less catalog every guard refuses.
    let mut verify_cfg = cfg.clone();
    verify_cfg.layout = Layout::DbPerRepo;
    let state = naatre_server_state(data, &verify_cfg);
    let state = match state {
        Ok(s) => std::sync::Arc::new(s),
        Err(e) => {
            let failures = cleanup(&catalog_path, &repos_dir);
            return Err(failed_with_cleanup("open split store", e, failures));
        }
    };
    // fsck is the async read pass; the offline adopt runs it OFF the
    // caller's runtime (adopt may be called from an async test or CLI
    // context — a helper thread owns its own runtime, the
    // Db::connect_postgres pattern). A PANIC inside the pass used to
    // resume unwind past the cleanup — a marker-less catalog left behind
    // that the old re-run refusal invited serving ("migrated already?");
    // the panic is caught, the partial targets removed, THEN resumed.
    let state_for_fsck = std::sync::Arc::clone(&state);
    let fsck = std::thread::spawn(move || {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .map_err(|e| failed("fsck runtime", e))?;
        match rt.block_on(crate::fsck::fsck(&state_for_fsck)) {
            Ok(r) => Ok(r),
            Err(e) => Err(failed("fsck", e)),
        }
    })
    .join()
    .unwrap_or_else(|panic| {
        let failures = cleanup(&catalog_path, &repos_dir);
        let _ = failures; // a panic resume names the panic; the targets are gone
        std::panic::resume_unwind(panic);
    });
    let fsck = match fsck {
        Ok(r) => r,
        Err(e) => {
            let failures = cleanup(&catalog_path, &repos_dir);
            return Err(failed_with_cleanup(
                match &e {
                    AdoptError::Failed { step, .. } => step,
                    _ => "fsck",
                },
                match &e {
                    AdoptError::Failed { source, .. } => source.to_string(),
                    other => other.to_string(),
                },
                failures,
            ));
        }
    };
    let fsck_clean = fsck.clean();
    if !fsck_clean {
        let failures = cleanup(&catalog_path, &repos_dir);
        return Err(failed_with_cleanup(
            "fsck",
            format!(
                "the migrated layout does not fsck clean: {} dangling bytes-missing {}",
                fsck.danglings.len(),
                fsck.bytes_missing
            ),
            failures,
        ));
    }
    report.fsck_clean = true;

    // The adopted marker: the adoption's LAST catalog write, after fsck
    // passes. From here the catalog is complete — every guard (the layout
    // check, Store::open_catalog, a re-run) accepts it; above this point a
    // crash leaves a marker-less catalog the same guards refuse with the
    // incomplete-migration remedy.
    {
        let catalog = Connection::open(&catalog_path).map_err(|e| {
            let failures = cleanup(&catalog_path, &repos_dir);
            failed_with_cleanup("adopted marker", e, failures)
        })?;
        catalog
            .execute(
                "INSERT OR REPLACE INTO meta(key, value) VALUES (?1, ?2)",
                rusqlite::params![
                    naatre_store::catalog::ADOPTED_MARKER,
                    format!("db-per-repo {}", chrono_now_secs()),
                ],
            )
            .map_err(|e| {
                let failures = cleanup(&catalog_path, &repos_dir);
                failed_with_cleanup("adopted marker", e, failures)
            })?;
    }
    Ok(report)
}

/// Seconds since the epoch for the marker's value (a plain i64 timestamp —
/// the marker's PRESENCE is the check; the value names when the adoption
/// completed).
fn chrono_now_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

fn cleanup(catalog_path: &Path, repos_dir: &Path) -> Vec<String> {
    let mut failures = Vec::new();
    let mut rm = |p: &Path| {
        if let Err(e) = std::fs::remove_file(p)
            && e.kind() != std::io::ErrorKind::NotFound
        {
            failures.push(format!("{}: {e}", p.display()));
        }
    };
    rm(catalog_path);
    // WAL/SHM siblings, then the directory.
    rm(&catalog_path.with_extension("db-wal"));
    rm(&catalog_path.with_extension("db-shm"));
    if repos_dir.exists()
        && let Err(e) = std::fs::remove_dir_all(repos_dir)
    {
        failures.push(format!("{}: {e}", repos_dir.display()));
    }
    failures
}

/// Opens a split-layout ServerState for the verification fsck — the
/// unchecked-catalog open ([ServerState::open_backend_adopt]): the
/// adopted marker is this module's LAST write, so the just-built catalog
/// does not carry it yet (the guard's marker check is for everything
/// EXCEPT this pass; the config is NOT written to disk — the cutover
/// steps are the operator's).
fn naatre_server_state(
    data: &Path,
    cfg: &BackendConfig,
) -> Result<crate::state::ServerState, naatre_store::StoreError> {
    let data_dir = naatre_store::DataDir::open_any(data);
    crate::state::ServerState::open_backend_adopt(data_dir, cfg)
}

/// The cutover steps the tool prints (the operator runs them; the tool
/// never flips the config itself).
pub const CUTOVER_STEPS: &[&str] = &[
    "1. stop naatre (the migration ran offline; keep it stopped for the cutover)",
    "2. add to backend.toml:  a [layout] table with mode = \"db-per-repo\"",
    "3. delete the old single DB: rm naatre.db (keep a `naatre backup` copy until fsck passes)",
    "4. start naatre — the banner names layout db-per-repo; run `naatre fsck` once more",
];
