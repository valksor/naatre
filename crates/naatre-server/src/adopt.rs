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
//! coercion. On ANY failure the partial targets (catalog.db, repos/) are
//! removed and the original single DB stands: the migration is all-or-
//! nothing at the layout level.

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
    #[error(
        "migration refused: {0} (run it against a single-DB data dir: `naatre.db` present, \
         no catalog.db)"
    )]
    NotSingleDb(String),
    #[error(
        "db-per-repo is a SQLite layout: the config selects meta = postgres \
         (per-repo files are SQLite; a Postgres deployment never splits)"
    )]
    PostgresUnsupported,
    #[error(
        "adoption failed at {step}: {source} (partial targets removed; \
             the original single DB is untouched)"
    )]
    Failed {
        step: &'static str,
        #[source]
        source: anyhow_source::AnyhowSource,
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
    }
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
    if catalog_path.exists() {
        return Err(AdoptError::NotSingleDb(
            "catalog.db already exists (migrated already?)".to_string(),
        ));
    }
    // The flock: a running server (or an in-flight import) refuses the
    // migration — the plan's offline clause.
    let data_dir = naatre_store::DataDir::open_any(data);
    let _lock = match data_dir.lock() {
        Ok(lock) => lock,
        Err(naatre_store::StoreError::LockHeld(_)) => return Err(AdoptError::Live),
        Err(e) => return Err(failed("flock", e)),
    };

    // The copy runs on ONE raw connection holding the source (main), the
    // catalog, and (per repo) the repo file — ATTACH + INSERT…SELECT, no
    // Rust-side row handling.
    let work = || -> Result<AdoptReport, AdoptError> {
        let conn = Connection::open(&db_path).map_err(|e| failed("open source", e))?;
        conn.execute_batch("PRAGMA journal_mode = WAL;")
            .map_err(|e| failed("source pragmas", e))?;
        // The catalog: created here, its OWN migration list runs (the
        // global tables + the registry; the per-repo files run the shared
        // list).
        {
            let catalog =
                Connection::open(&catalog_path).map_err(|e| failed("catalog create", e))?;
            naatre_store::catalog::migrate_catalog(&catalog)
                .map_err(|e| failed("catalog create", e))?;
            drop(catalog);
        }
        conn.execute_batch(&format!(
            "ATTACH DATABASE {catalog:?} AS catalog;",
            catalog = catalog_path.display().to_string()
        ))
        .map_err(|e| failed("attach catalog", e))?;

        // The repo registry first (ids must exist before the per-repo files).
        let repos: Vec<(i64, String, i64)> = {
            let mut stmt = conn
                .prepare("SELECT id, name, created_ns FROM repos ORDER BY id ASC")
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
                "ATTACH DATABASE {path:?} AS repo_db;",
                path = repo_path.display().to_string()
            ))
            .map_err(|e| failed("attach repo", e))?;
            // The registry row: the catalog's shape adds the db path.
            conn.execute(
                "INSERT INTO catalog.repos(id, name, created_ns, db_file) \
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
                        &format!("INSERT INTO repo_db.{table} SELECT * FROM main.{table} WHERE repo_id = ?1"),
                        rusqlite::params![id],
                    )
                    .map_err(|e| failed("repo copy", e))?;
                counts.push(((*table).to_string(), n));
            }
            for table in REPO_TABLES_NO_REPO_ID {
                let n = conn
                    .execute(
                        &format!("INSERT INTO repo_db.{table} SELECT * FROM main.{table}"),
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
                        &format!("SELECT COUNT(*) FROM main.{table}{where_clause}"),
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

        // The global tables: verbatim into the catalog.
        for table in CATALOG_TABLES {
            let n = conn
                .execute(
                    &format!("INSERT INTO catalog.{table} SELECT * FROM main.{table}"),
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
                    .prepare("SELECT key, value FROM main.meta WHERE key != 'schema_version'")
                    .map_err(|e| failed("meta read", e))?;
                stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))
                    .map_err(|e| failed("meta read", e))?
                    .collect::<Result<Vec<_>, _>>()
                    .map_err(|e| failed("meta read", e))?
            };
            let mut n = 0usize;
            for (key, value) in markers {
                conn.execute(
                    "INSERT INTO catalog.meta(key, value) VALUES (?1, ?2)",
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
                .query_row(&format!("SELECT COUNT(*) FROM main.{table}"), [], |r| {
                    r.get(0)
                })
                .map_err(|e| failed("parity read", e))?;
            let copied: i64 = conn
                .query_row(&format!("SELECT COUNT(*) FROM catalog.{table}"), [], |r| {
                    r.get(0)
                })
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
            // stands.
            cleanup(&catalog_path, &repos_dir);
            return Err(e);
        }
    };

    // The fsck pass against the NEW layout: the split must serve the same
    // store the single DB did (clean or with exactly the same advisory
    // states a single-DB fsck reports — the check demands clean()).
    let mut verify_cfg = cfg.clone();
    verify_cfg.layout = Layout::DbPerRepo;
    let state = naatre_server_state(data, &verify_cfg);
    let state = match state {
        Ok(s) => std::sync::Arc::new(s),
        Err(e) => {
            cleanup(&catalog_path, &repos_dir);
            return Err(failed("open split store", e));
        }
    };
    // fsck is the async read pass; the offline adopt runs it on a
    // current-thread runtime (the S3 facade and every spawn_blocking hop
    // run on it — the host verbs' block_on shape).
    // fsck is the async read pass; the offline adopt runs it OFF the
    // caller's runtime (adopt may be called from an async test or CLI
    // context — a helper thread owns its own runtime, the
    // Db::connect_postgres pattern).
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
    .unwrap_or_else(|panic| std::panic::resume_unwind(panic));
    let fsck = match fsck {
        Ok(r) => r,
        Err(e) => {
            cleanup(&catalog_path, &repos_dir);
            return Err(e);
        }
    };
    let fsck_clean = fsck.clean();
    if !fsck_clean {
        cleanup(&catalog_path, &repos_dir);
        return Err(failed(
            "fsck",
            format!(
                "the migrated layout does not fsck clean: {} dangling bytes-missing {}",
                fsck.danglings.len(),
                fsck.bytes_missing
            ),
        ));
    }
    report.fsck_clean = true;
    Ok(report)
}

fn cleanup(catalog_path: &Path, repos_dir: &Path) {
    let _ = std::fs::remove_file(catalog_path);
    // WAL/SHM siblings, then the directory.
    let _ = std::fs::remove_file(catalog_path.with_extension("db-wal"));
    let _ = std::fs::remove_file(catalog_path.with_extension("db-shm"));
    if repos_dir.exists() {
        let _ = std::fs::remove_dir_all(repos_dir);
    }
}

/// Opens a split-layout ServerState for the verification fsck (the same
/// open path `serve` uses, programmatic — the config is NOT written to
/// disk; the cutover steps are the operator's).
fn naatre_server_state(
    data: &Path,
    cfg: &BackendConfig,
) -> Result<crate::state::ServerState, naatre_store::StoreError> {
    let data_dir = naatre_store::DataDir::open_any(data);
    naatre_store::backend::check_data_dir_backend(data_dir.path(), cfg)?;
    crate::state::ServerState::open_backend(data_dir, cfg)
}

/// The cutover steps the tool prints (the operator runs them; the tool
/// never flips the config itself).
pub const CUTOVER_STEPS: &[&str] = &[
    "1. stop naatre (the migration ran offline; keep it stopped for the cutover)",
    "2. add to backend.toml:  a [layout] table with mode = \"db-per-repo\"",
    "3. delete the old single DB: rm naatre.db (keep a `naatre backup` copy until fsck passes)",
    "4. start naatre — the banner names layout db-per-repo; run `naatre fsck` once more",
];
