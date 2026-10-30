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
use naatre_store::{DataDir, Store, StoreError};

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

fn open_or_fail(data: &Path) -> Result<DataDir, i32> {
    DataDir::open(data).map_err(fail)
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

/// `naatre init --data <dir>`: create the dir, run migrations, upsert the
/// admin principal, mint the first admin token (printed exactly once), and
/// audit `init`. Refuses an already-initialized dir — never re-mints.
pub fn init(data: &Path) -> i32 {
    let data_dir = match DataDir::create(data) {
        Ok(d) => d,
        Err(e) => return fail(e),
    };
    // The real flow: init writes naatre.db, Store::open applies pragmas +
    // migrations (schema authority lives in the store).
    if let Err(e) = std::fs::write(data_dir.db_path(), b"") {
        return fail(e);
    }
    let data_dir = match open_or_fail(data) {
        Ok(d) => d,
        Err(code) => return code,
    };
    let store = match Store::open(data_dir) {
        Ok(s) => s,
        Err(e) => return fail(e),
    };
    let (token, hash) = mint_token();
    let conn = store.conn();
    let now = now_ns();
    let write = || -> Result<(), StoreError> {
        let tx = conn.unchecked_transaction().map_err(StoreError::Sqlite)?;
        db::principal_upsert(&tx, "admin", "admin", now)?;
        db::token_insert(&tx, "admin", &hash, None, None)?;
        db::audit_append(&tx, now, "admin", "init", "{}")?;
        tx.commit().map_err(StoreError::Sqlite)
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
    let data_dir = match open_or_fail(data) {
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
    let state = match ServerState::open(data_dir) {
        Ok(s) => s,
        Err(e) => return fail(e),
    };
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
    let data_dir = match open_or_fail(data) {
        Ok(d) => d,
        Err(code) => return code,
    };
    let _lock = match lock_or_fail(&data_dir) {
        Ok(l) => l,
        Err(code) => return code,
    };
    let store = match Store::open(data_dir) {
        Ok(s) => s,
        Err(e) => return fail(e),
    };
    let (token, hash) = mint_token();
    let conn = store.conn();
    let now = now_ns();
    let write = || -> Result<(), StoreError> {
        let tx = conn.unchecked_transaction().map_err(StoreError::Sqlite)?;
        db::principal_upsert(&tx, "admin", "admin", now)?;
        db::token_insert(&tx, "admin", &hash, None, None)?;
        db::audit_append(&tx, now, "admin", "new_admin", "{}")?;
        tx.commit().map_err(StoreError::Sqlite)
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
    match naatre_server::import::run(data, &opts) {
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
