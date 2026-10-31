//! Host-local verbs: `init`, `serve`, `token --new-admin`, and `import` run
//! directly against the data dir, under its flock. The 0700 data dir is the
//! trust boundary — anything that can read it can mint admin tokens.

use std::path::Path;
use std::sync::Arc;

use naatre_server::auth::mint_token;
use naatre_server::db;
use naatre_server::import::{ImportOptions, Report};
use naatre_server::serve::{ServeConfig, unix_shutdown_signal};
use naatre_server::state::ServerState;
use naatre_store::backend::{self as backend_cfg, check_data_dir_backend};
use naatre_store::{BackendConfig, DataDir, MetaKind, Store, StoreError};

/// Nanoseconds since the epoch (host-local verbs use the system clock).
fn now_ns() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0)
}

fn fail(e: impl std::fmt::Display) -> i32 {
    eprintln!("error: {e}");
    1
}

/// Takes the data dir flock; a second serve (or an import) holding it is
/// the "another naatre process" error.
fn lock_or_fail(data_dir: &DataDir) -> Result<naatre_store::LockGuard, i32> {
    match data_dir.lock() {
        Ok(guard) => Ok(guard),
        Err(StoreError::LockHeld(path)) => Err(fail(format!(
            "another naatre process holds the data dir (lockfile: {})",
            path.display()
        ))),
        Err(e) => Err(fail(e)),
    }
}

/// Reads `<data>/backend.toml` (absent = the zero-config SQLite/fs
/// default). Any config refusal — malformed file, loose permissions,
/// inline credentials, plaintext without the opt-in — is a loud failure
/// BEFORE the store opens.
fn load_backend_or_fail(data: &Path) -> Result<BackendConfig, i32> {
    backend_cfg::load(data).map_err(fail)
}

/// DataDir open + the backend/data-dir mismatch guard (plan M9: a data dir
/// whose existing DB disagrees with the configured backend is refused,
/// naming both).
fn open_or_fail_backend(data: &Path, cfg: &BackendConfig) -> Result<DataDir, i32> {
    let data_dir = DataDir::open_any(data);
    check_data_dir_backend(data_dir.path(), cfg).map_err(fail)?;
    Ok(data_dir)
}

/// `naatre init --data <dir>`: create the dir, run migrations, upsert the
/// admin principal, mint the first admin token (printed exactly once), and
/// audit `init`. Refuses an already-initialized dir — never re-mints. The
/// backend selection (`backend.toml`, read FIRST) decides what
/// "initialized" means: a `naatre.db` file for SQLite, a migrated
/// `schema_version` row for Postgres.
pub fn init(data: &Path) -> i32 {
    let cfg = match load_backend_or_fail(data) {
        Ok(c) => c,
        Err(code) => return code,
    };
    // DataDir::create refuses a dir with naatre.db (a SQLite data dir is
    // never re-initialized; a stale naatre.db under a postgres config is
    // the mismatch case, refused with the same error).
    let data_dir = match DataDir::create(data) {
        Ok(d) => d,
        Err(e) => return fail(e),
    };
    match cfg.meta {
        MetaKind::Sqlite => {
            // The real flow: init writes naatre.db, Store::open applies
            // pragmas + migrations (schema authority lives in the store).
            if let Err(e) = std::fs::write(data_dir.db_path(), b"") {
                return fail(e);
            }
        }
        MetaKind::Postgres => {
            // "Already initialized" is a schema in the configured database
            // — checked before any migration runs, so init never re-mints.
            let pg = cfg.pg.as_ref().expect("parser guarantees the DSN");
            let conn = match naatre_store::Db::connect_postgres(pg, false) {
                Ok(c) => c,
                Err(e) => return fail(StoreError::Db(e)),
            };
            let initialized = conn
                .query_opt(
                    &naatre_store::Sql::plain(
                        "SELECT value FROM meta WHERE key = 'schema_version'",
                    ),
                    &[],
                )
                .map(|row| row.is_some())
                .unwrap_or(true);
            if initialized {
                return fail(StoreError::Backend(
                    backend_cfg::BackendError::AlreadyInitialized,
                ));
            }
        }
    }
    let data_dir = match open_or_fail_backend(data, &cfg) {
        Ok(d) => d,
        Err(code) => return code,
    };
    let store = match Store::open_backend(data_dir, &cfg) {
        Ok(s) => s,
        Err(e) => return fail(e),
    };
    let (token, hash) = mint_token();
    let conn = store.conn();
    let now = now_ns();
    let write = || -> Result<(), StoreError> {
        let tx = conn.transaction()?;
        db::principal_upsert(&tx, "admin", "admin", now)?;
        db::token_insert(&tx, "admin", &hash, None, None)?;
        db::audit_append(&tx, now, "admin", "init", "{}")?;
        tx.commit()?;
        Ok(())
    };
    if let Err(e) = write() {
        return fail(e);
    }
    println!("admin token: {token}");
    0
}

/// `naatre serve --data <dir>`: open guards (uninitialized → `run naatre
/// init`; flock held elsewhere → refusal), non-loopback cleartext warning,
/// ops logging, serve until Ctrl-C/SIGTERM with graceful drain.
#[tokio::main]
pub async fn serve(data: &Path, listen: &str, log_level: &str) -> i32 {
    let cfg = match load_backend_or_fail(data) {
        Ok(c) => c,
        Err(code) => return code,
    };
    let data_dir = match open_or_fail_backend(data, &cfg) {
        Ok(d) => d,
        Err(code) => return code,
    };
    let _lock = match lock_or_fail(&data_dir) {
        Ok(l) => l,
        Err(code) => return code,
    };
    let addr: std::net::SocketAddr = match listen.parse() {
        Ok(a) => a,
        Err(_) => return fail(format!("invalid listen address {listen:?}")),
    };
    if !addr.ip().is_loopback() {
        eprintln!(
            "warning: {addr} is non-loopback: bearer tokens will cross the network in cleartext unless fronted by TLS"
        );
    }
    // The store open runs migrations and connects — driver work must not
    // happen on a runtime worker thread (the sync Postgres client's
    // contract).
    let state = match tokio::task::spawn_blocking(move || ServerState::open_backend(data_dir, &cfg))
        .await
    {
        Ok(Ok(s)) => s,
        Ok(Err(e)) => return fail(e),
        Err(join) => return fail(format!("store open task panicked: {join}")),
    };
    // The production derived-data kinds (M6b): blame, path_history,
    // commit_graph. The offline `import` verb registers none — the backlog
    // backfills on the serving side (`serve`'s boot-time backfill).
    let mut state = state;
    for deriver in naatre_server::derived::production_kinds() {
        if let Err(e) = state.register_deriver(deriver) {
            return fail(e);
        }
    }
    naatre_server::middleware::logging_setup(log_level);
    let listener = match tokio::net::TcpListener::bind(addr).await {
        Ok(l) => l,
        Err(e) => return fail(e),
    };
    let shutdown = async {
        unix_shutdown_signal().await;
        println!("draining…");
    };
    match naatre_server::serve::serve(
        ServeConfig {
            listener,
            state: Arc::new(state),
        },
        shutdown,
    )
    .await
    {
        Ok(()) => 0,
        Err(e) => fail(e),
    }
}

/// `naatre token --data <dir> --new-admin`: mint a fresh admin token
/// host-local on an existing data dir (flocked), print once, audit
/// `new_admin`. Refuses an uninitialized dir.
pub fn new_admin_token(data: &Path) -> i32 {
    let cfg = match load_backend_or_fail(data) {
        Ok(c) => c,
        Err(code) => return code,
    };
    let data_dir = match open_or_fail_backend(data, &cfg) {
        Ok(d) => d,
        Err(code) => return code,
    };
    let _lock = match lock_or_fail(&data_dir) {
        Ok(l) => l,
        Err(code) => return code,
    };
    let store = match Store::open_backend(data_dir, &cfg) {
        Ok(s) => s,
        Err(e) => return fail(e),
    };
    let (token, hash) = mint_token();
    let conn = store.conn();
    let now = now_ns();
    let write = || -> Result<(), StoreError> {
        let tx = conn.transaction()?;
        db::principal_upsert(&tx, "admin", "admin", now)?;
        db::token_insert(&tx, "admin", &hash, None, None)?;
        db::audit_append(&tx, now, "admin", "new_admin", "{}")?;
        tx.commit()?;
        Ok(())
    };
    if let Err(e) = write() {
        return fail(e);
    }
    println!("admin token: {token}");
    0
}

/// `naatre import --data <dir> [--into <name>] [--skip-unsupported]
/// <git-path>`: one-way git import, offline, under the data dir flock.
/// Prints plain progress lines and the final JSON report line; exits 0 on
/// success and 1 on a hard error or an aborted import.
pub fn import(data: &Path, into: Option<&str>, skip_unsupported: bool, git_path: &str) -> i32 {
    let opts = ImportOptions {
        git_path: git_path.into(),
        into: into.map(str::to_string),
        skip_unsupported,
    };
    println!("import: source {}", opts.git_path.display());
    // No derived-data kinds are registered in the offline import process
    // (kinds belong to `serve`); the backlog backfills through the queue on
    // the serving side (M6b's registration step).
    let cfg = match load_backend_or_fail(data) {
        Ok(c) => c,
        Err(code) => return code,
    };
    let derive = naatre_server::derive::DeriveRegistry::default();
    match naatre_server::import::run_backend(data, &cfg, &opts, &derive) {
        Ok(report) => {
            print_report(&report);
            i32::from(report.status != "ok")
        }
        Err(e) => fail(e),
    }
}

fn print_report(report: &Report) {
    let created = if report.repo_created {
        "created"
    } else {
        "existing"
    };
    println!("import: repo {} ({})", report.repo, created);
    println!("imported: {} commit(s)", report.imported);
    for s in &report.skipped {
        println!("skipped: {} ({})", s.oid, s.reason);
    }
    for oid in &report.cascaded {
        println!("cascaded: {oid}");
    }
    for oid in &report.sanitized {
        println!("sanitized: {oid}");
    }
    match &report.reason {
        Some(reason) => println!("main: {} — {reason}", report.main),
        None => println!("main: {}", report.main),
    }
    println!("{}", report.to_json_line());
}
