//! Host-local verbs: `init`, `serve`, `token --new-admin`, and `import` run
//! directly against the data dir, under its flock. The 0700 data dir is the
//! trust boundary — anything that can read it can mint admin tokens.

use std::path::Path;
use std::sync::Arc;

use naatre_server::api_error::ApiError;
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
/// [--sanitize] <git-path>`: one-way git import, offline, under the data
/// dir flock. Prints plain progress lines and the final JSON report line;
/// exits 0 on success and 1 on a hard error or an aborted import.
pub fn import(
    data: &Path,
    into: Option<&str>,
    skip_unsupported: bool,
    sanitize: bool,
    git_path: &str,
) -> i32 {
    let opts = ImportOptions {
        git_path: git_path.into(),
        into: into.map(str::to_string),
        skip_unsupported,
        sanitize,
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
    for e in &report.escaped {
        println!(
            "escaped: {} {} ({}): {}",
            e.commit,
            e.id,
            e.field,
            e.chars.join(", ")
        );
    }
    match &report.reason {
        Some(reason) => println!("main: {} — {reason}", report.main),
        None => println!("main: {}", report.main),
    }
    println!("{}", report.to_json_line());
}

/// The host-local verbs that run async server passes against the store:
/// flock first, then the backend open (migrations), then one
/// current-thread runtime with blocking+io+time enabled (the S3 facade's
/// client and every spawn_blocking hop run on it).
fn block_on_state<T, Fut>(
    data: &Path,
    with: impl FnOnce(Arc<naatre_server::state::ServerState>) -> Fut,
) -> Result<T, StoreError>
where
    Fut: std::future::Future<Output = T>,
{
    let cfg = backend_cfg::load(data)?;
    let data_dir = DataDir::open_any(data);
    check_data_dir_backend(data_dir.path(), &cfg)?;
    // Flock first (serve/import hold it across their whole run; a GC or
    // fsck against a live data dir is refused the same way).
    let _lock = data_dir.lock()?;
    let state = naatre_server::state::ServerState::open_backend(data_dir, &cfg)?;
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|_| StoreError::S3 { op: "runtime" })?;
    Ok(rt.block_on(with(Arc::new(state))))
}

/// `naatre gc --data <dir> [--json]`: one reachability GC pass (to
/// convergence) against the data dir, flocked like import. The grace
/// window and the serve-side interval come from backend.toml
/// (`gc_grace_secs`, `gc_interval_secs`); this verb runs regardless of
/// `gc_interval_secs = 0`.
pub fn gc(data: &Path, json: bool) -> i32 {
    match block_on_state(data, |state| async move {
        naatre_server::gc::gc_pass(&state).await
    }) {
        Ok(Ok(report)) => {
            if json {
                println!(
                    "{}",
                    serde_json::to_string(&report).expect("report serializes")
                );
            } else {
                println!(
                    "gc: ledger +{} -{}",
                    report.candidates_new, report.candidates_repaired
                );
                println!(
                    "gc: collected changesets {}, blobs {}, trees {}",
                    report.cs_collected, report.blobs_collected, report.trees_collected
                );
                if report.blobs_condemned > 0 {
                    println!(
                        "gc: condemned blobs {} (phase A; a later pass deletes the bytes)",
                        report.blobs_condemned
                    );
                }
            }
            0
        }
        Ok(Err(e)) => fail(e),
        Err(e) => fail(e),
    }
}

/// `naatre redact blob|changeset <id> --reason R --repo <name>
/// --data <dir>`: mark one object tombstoned in one repo (M10b, host-local
/// admin verb under the data dir flock). The mark, the tombstoned
/// changeset's derived-row invalidation, and the `redact` audit row (actor
/// `admin`, reason in its details — the tombstone ROW carries path + date
/// only) commit in one immediate transaction; the GC's redaction phase
/// reclaims the bytes/rows later. An idempotent re-mark stands pat; a
/// re-mark on a PURGED tombstone re-arms it (new marked_ns, purged state
/// cleared, a new audit row); a re-mark naming the other kind is refused.
/// Remote administration goes through
/// `POST /v1/repos/{r}/redactions` (the same statement layer). Exit 0
/// marked, 1 error.
pub fn redact(data: &Path, repo: &str, kind: &str, id_hex: &str, reason: &str) -> i32 {
    let kind = match kind {
        "blob" => "blob".to_string(),
        "changeset" => "changeset".to_string(),
        other => {
            return fail(format!("kind must be `blob` or `changeset`, got {other:?}"));
        }
    };
    let Some(id) = naatre_core::ids::Id::from_hex(id_hex) else {
        return fail("id must be 64 lowercase hex characters");
    };
    if reason.trim().is_empty() {
        return fail("reason must not be empty");
    }
    // The async closures are 'static: own the verb's strings.
    let (repo, reason) = (repo.to_string(), reason.to_string());
    match block_on_state(data, |state| async move {
        // The target must be a real object of this repo, or an already-
        // tombstoned id (the re-arm target) — the route's contract, the
        // import-aware presence arms included. The probe, split across the
        // two connections: the repo-side shapes on the repo's connection,
        // the plain `objects` row (blob kind only) on the catalog.
        let kind_for_probe = kind.clone();
        let repo_for_probe = repo.clone();
        let exists = state
            .read_repo(repo_for_probe, move |cx| {
                let present = db::redaction_target_present_repo(cx.db, cx.id, &id, &kind_for_probe)
                    .map_err(ApiError::log_internal)?;
                if present {
                    return Ok(true);
                }
                if kind_for_probe == "blob" {
                    return db::object_row_exists(&cx.cat(), &id).map_err(ApiError::log_internal);
                }
                Ok(false)
            })
            .await?;
        if !exists {
            return Err(ApiError::new(
                naatre_server::api_code::ApiCode::ObjectNotFound,
                format!("no {kind} {} in this repo", id.to_hex()),
            ));
        }
        let now_ns = state.now_ns() as i64;
        let repo_for_mark = repo.clone();
        let marked = state
            .write_repo(repo_for_mark, move |cx| {
                let repo_id = cx.id;
                let conn = cx.db;
                // Immediate: the same destructive-write lock class as the
                // route's mark (the repo's slot). The audit row follows the
                // route's connection shape: single-DB mode (the catalog IS
                // the work connection) appends INSIDE the mark's
                // transaction — a crash rolls the tombstone and the row
                // back together; the split appends after the commit (the
                // documented window).
                let tx = conn
                    .transaction_immediate()
                    .map_err(ApiError::log_internal)?;
                let outcome = db::redaction_mark(&tx, repo_id, &id, &kind, now_ns)
                    .map_err(ApiError::log_internal)?;
                if matches!(outcome, db::RedactionMarkOutcome::Inserted(_)) {
                    db::redaction_invalidate_derived(&tx, repo_id, &id)
                        .map_err(ApiError::log_internal)?;
                }
                let rearmed = matches!(outcome, db::RedactionMarkOutcome::Rearmed(_));
                // The audit row rides the 0→1 transition and the re-arm.
                if matches!(outcome, db::RedactionMarkOutcome::Inserted(_)) || rearmed {
                    let mut details = serde_json::json!({
                        "repo": repo,
                        "id": id.to_hex(),
                        "kind": kind,
                        "reason": reason,
                    });
                    if rearmed {
                        details["rearmed"] = serde_json::json!(true);
                    }
                    let details = details.to_string();
                    if cx.cat_is_db() {
                        db::audit_append(&tx, now_ns, "admin", "redact", &details)
                            .map_err(ApiError::log_internal)?;
                        tx.commit().map_err(ApiError::log_internal)?;
                    } else {
                        tx.commit().map_err(ApiError::log_internal)?;
                        db::audit_append(&cx.cat(), now_ns, "admin", "redact", &details)
                            .map_err(ApiError::log_internal)?;
                    }
                } else {
                    tx.commit().map_err(ApiError::log_internal)?;
                }
                Ok(outcome)
            })
            .await?;
        Ok::<_, ApiError>(marked)
    }) {
        Ok(Ok(outcome)) => {
            match outcome {
                db::RedactionMarkOutcome::Inserted(row) => {
                    print!("redacted {} {}", row.kind, row.object_id.to_hex());
                    if let Some(path) = &row.path {
                        print!(" at {path}");
                    }
                    println!(" (marked_ns {})", row.marked_ns);
                }
                db::RedactionMarkOutcome::Existing(row) => {
                    print!("already redacted {} {}", row.kind, row.object_id.to_hex());
                    if let Some(path) = &row.path {
                        print!(" at {path}");
                    }
                    println!(" (marked_ns {} stands)", row.marked_ns);
                }
                db::RedactionMarkOutcome::Rearmed(row) => {
                    print!("re-armed {} {}", row.kind, row.object_id.to_hex());
                    if let Some(path) = &row.path {
                        print!(" at {path}");
                    }
                    println!(
                        " (marked_ns {}, purged state cleared — the purge reclaims again)",
                        row.marked_ns
                    );
                }
                db::RedactionMarkOutcome::KindMismatch { existing } => {
                    return fail(format!(
                        "{} is already tombstoned with kind {:?}; refusing the re-mark",
                        existing.object_id.to_hex(),
                        existing.kind
                    ));
                }
            }
            0
        }
        Ok(Err(e)) => fail(e),
        Err(e) => fail(e),
    }
}

/// `naatre fsck --data <dir> [--json] [--repair]`: the consistency checker
/// — dangling references, reachability counts, CAS presence vs the DB
/// inventory — read-only under the data dir flock, except the `--repair`
/// arm: backfilling the `objects` record row for referenced blobs whose
/// bytes are present (the dirt pre-M11 imports left; audited, scoped to
/// exactly that class). Exit 0 clean, 1 findings, 2 error.
pub fn fsck(data: &Path, json: bool, repair: bool) -> i32 {
    match block_on_state(data, |state| async move {
        if repair {
            naatre_server::fsck::fsck_repair(&state).await
        } else {
            naatre_server::fsck::fsck(&state).await
        }
    }) {
        Ok(Ok(report)) => {
            let clean = report.clean();
            if json {
                println!(
                    "{}",
                    serde_json::to_string(&report).expect("report serializes")
                );
            } else {
                println!("fsck: blob backend {}", report.blob_backend);
                println!(
                    "fsck: changesets {} total, {} marked",
                    report.changesets, report.changesets_marked
                );
                println!(
                    "fsck: blobs {} db, {} cas, {} marked",
                    report.blobs_db, report.blobs_cas, report.blobs_marked
                );
                println!(
                    "fsck: trees {} total, {} protected",
                    report.trees, report.trees_protected
                );
                println!(
                    "fsck: candidates {} pending, {} past grace",
                    report.candidates_pending, report.candidates_past_grace
                );
                println!(
                    "fsck: collected (cs {}, blobs {}, trees {}) across {} pass(es)",
                    report.collected.0, report.collected.1, report.collected.2, report.gc_passes
                );
                for (check, count) in &report.danglings {
                    if *count > 0 {
                        println!("fsck: DANGLING {check}: {count}");
                    }
                }
                if report.bytes_missing > 0 {
                    println!(
                        "fsck: MISSING BYTES for {} db-known blob(s)",
                        report.bytes_missing
                    );
                }
                if let Some(note) = report.external_drift {
                    println!("fsck: note: {note}");
                }
                if report.cas_unrecorded > 0 {
                    println!(
                        "fsck: {} cas-present blob(s) without an objects row (orphaned uploads)",
                        report.cas_unrecorded
                    );
                }
                if !report.backfilled.is_empty() {
                    println!(
                        "fsck: repair backfilled {} objects record row(s): {}",
                        report.backfilled.len(),
                        report.backfilled.join(", ")
                    );
                }
                if report.stale_candidates > 0 {
                    println!(
                        "fsck: {} stale candidate row(s) for reachable data (the next pass repairs)",
                        report.stale_candidates
                    );
                }
                if report.advisory_membership_missing > 0 || report.advisory_membership_extra > 0 {
                    println!(
                        "fsck: advisory membership -{} +{} (missing/extra repo_blobs rows; ignored by GC)",
                        report.advisory_membership_missing, report.advisory_membership_extra
                    );
                }
                if report.redactions_active > 0 || report.redactions_purged > 0 {
                    println!(
                        "fsck: redactions {} active (tombstoned-but-unpurged is normal), {} purged",
                        report.redactions_active, report.redactions_purged
                    );
                }
                if report.redaction_purge_drift > 0 {
                    println!(
                        "fsck: REDACTION DRIFT: {} purged tombstone(s) whose object row still exists",
                        report.redaction_purge_drift
                    );
                }
                println!("fsck: {}", if clean { "clean" } else { "FINDINGS" });
            }
            if clean { 0 } else { 1 }
        }
        Ok(Err(e)) => {
            fail(e);
            2
        }
        Err(e) => {
            fail(e);
            2
        }
    }
}

/// `naatre migrate-db-per-repo --data <dir>`: the offline adoption (M13a).
/// Prints the per-repo copy report (rows per table), the catalog totals,
/// the fsck verdict, and the cutover steps. Exit 0 migrated, 1 error.
pub fn migrate_db_per_repo(data: &Path) -> i32 {
    let cfg = match load_backend_or_fail(data) {
        Ok(c) => c,
        Err(code) => return code,
    };
    match naatre_server::adopt::adopt_db_per_repo(data, &cfg) {
        Ok(report) => {
            println!(
                "migrate-db-per-repo: catalog built at {}/catalog.db",
                data.display()
            );
            for repo in &report.repos {
                let total: usize = repo.rows.iter().map(|(_, n)| n).sum();
                println!(
                    "migrate-db-per-repo: repo {:?} (id {}) -> {} ({} rows, {} tables)",
                    repo.name,
                    repo.id,
                    repo.db_file,
                    total,
                    repo.rows.len(),
                );
            }
            for (table, n) in &report.catalog_rows {
                println!("migrate-db-per-repo: catalog {table}: {n} row(s)");
            }
            println!(
                "migrate-db-per-repo: row-count parity verified; fsck: {}",
                if report.fsck_clean {
                    "clean"
                } else {
                    "FINDINGS"
                }
            );
            println!("migrate-db-per-repo: the original naatre.db is UNTOUCHED — cutover steps:");
            for step in naatre_server::adopt::CUTOVER_STEPS {
                println!("  {step}");
            }
            0
        }
        Err(e) => fail(e),
    }
}

/// `naatre backup --data <dir> <dest>`: snapshot the whole data dir (every
/// backend combo) into ONE new backup directory — manifest.json, the
/// consistent DB snapshot FIRST (`VACUUM INTO` for SQLite, one `pg_dump`
/// run for Postgres), then the immutable objects — under the data-dir
/// flock. Exit 0 on success, 1 on error.
pub fn backup(data: &Path, dest: &Path) -> i32 {
    println!("backup: data {}", data.display());
    let cfg = match load_backend_or_fail(data) {
        Ok(c) => c,
        Err(code) => return code,
    };
    match naatre_server::backup::backup(data, &cfg, dest) {
        Ok(report) => {
            println!("backup: wrote {}", report.dest.display());
            println!(
                "backup: meta {}, blob {} (schema v{})",
                report.meta.as_str(),
                report.blob.as_str(),
                report.schema_version
            );
            println!(
                "backup: changesets {}, objects {}",
                report.counts.changesets, report.counts.objects
            );
            if let Some(dump) = report.dump_file {
                println!("backup: DB snapshot {dump} (pg_dump ran first — the safe order)");
            } else {
                println!("backup: DB snapshot naatre.db (VACUUM INTO ran first — the safe order)");
            }
            if report.bucket_copy_duty {
                println!(
                    "backup: S3 — sync the WHOLE BUCKET (README \"Backup\"); \
                     objects are immutable, so incremental syncs between backups \
                     work, and fsck's bytes_missing catches stragglers afterwards"
                );
            }
            if !report.missing.is_empty() {
                println!(
                    "backup: WARNING — {} object(s) vanished between the DB snapshot and \
                     the copy (a redaction purge raced the snapshot): {}",
                    report.missing.len(),
                    report.missing.join(", ")
                );
                println!(
                    "backup: the manifest lists only the captured objects; a restore of \
                     this backup needs the redactions re-applied and a GC pass (the \
                     documented operator duty)"
                );
            }
            if report.past_grace {
                println!(
                    "backup: WARNING — the copy phase started past gc_grace_secs after the \
                     DB snapshot; GC could have reclaimed snapshot-reachable bytes (see \
                     the missing list above, if any)"
                );
            }
            0
        }
        Err(e) => fail(e),
    }
}

/// A backup's age in whole days/hours/minutes/seconds (largest unit with
/// the next one for context) — the restore duty line's key.
fn backup_age(elapsed_ns: i64) -> String {
    let secs = (elapsed_ns / 1_000_000_000).max(0);
    let (d, h, m) = (secs / 86_400, secs % 86_400 / 3_600, secs % 3_600 / 60);
    if d > 0 {
        format!("{d} day(s) {h} hour(s)")
    } else if h > 0 {
        format!("{h} hour(s) {m} minute(s)")
    } else if m > 0 {
        format!("{m} minute(s) {}s", secs % 60)
    } else {
        format!("{secs}s")
    }
}

/// `naatre restore --data <target-dir> <backup-dir>`: restore one backup
/// directory into a data dir for the configured backend. Verifies the
/// manifest and every artifact's SHA256 into staging BEFORE any write to
/// the target; refuses a combo mismatch and a non-empty target unless
/// `--force`; restores DB then objects; checks object PRESENCE (not
/// reachability) and points the operator at fsck. Exit 0 on success, 1 on
/// error.
pub fn restore_backup(
    target: &Path,
    backup_dir: &Path,
    force: bool,
    allow_missing: Option<&str>,
) -> i32 {
    println!("restore: target {}", target.display());
    let cfg = match load_backend_or_fail(target) {
        Ok(c) => c,
        Err(code) => return code,
    };
    match naatre_server::backup::restore(target, &cfg, backup_dir, force, allow_missing) {
        Ok(report) => {
            println!(
                "restore: manifest format 1 (naatre backup, schema v{}, meta {}, blob {})",
                report.schema_version,
                report.meta.as_str(),
                report.blob.as_str()
            );
            if report.forced {
                println!("restore: --force overrode the non-empty-target refusal");
            }
            println!(
                "restore: artifacts SHA256-verified, then DB restored first, objects second (the safe order)"
            );
            println!(
                "restore: changesets {}, objects {} present in the target CAS",
                report.counts.changesets, report.objects_present
            );
            if !report.waived.is_empty() {
                println!(
                    "restore: --allow-missing waived {} object(s): {}",
                    report.waived.len(),
                    report.waived.join(", ")
                );
            }
            // The pre-redaction duty, keyed to the backup's age (the
            // compliance item): redactions recorded only in the destroyed
            // store must be re-applied — fsck cannot detect their absence.
            let now_ns = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_nanos() as i64)
                .unwrap_or(0);
            println!(
                "restore: DUTY — this backup is {} old (taken {}): any redaction recorded \
                 only in the destroyed store since then must be re-applied (fsck cannot \
                 detect them)",
                backup_age(now_ns.saturating_sub(report.created_ns)),
                format_args!("unix {}", report.created_ns / 1_000_000_000),
            );
            println!(
                "restore: now run `naatre fsck --data {}` — restore does not \
                 silently skip verification",
                target.display()
            );
            0
        }
        Err(e) => fail(e),
    }
}

// ---- M13b: the cache tier and its origin key store -------------------------

/// `naatre cache-key init --data <dir>`: generate the origin's Ed25519
/// ticket-signing pair (host-local, flocked, audited `cache_key_init` —
/// actor `admin`, the key ID in the details, never key material).
/// Refuses when the key store already exists (rotation, not re-init).
/// The verify list (`cache_keys.json`) carries PUBLIC keys only and is
/// the file the cache operator copies to the tier.
pub fn cache_key_init(data: &Path) -> i32 {
    key_store_write(data, "init", || {
        if data.join(naatre_server::ticket::SIGNING_FILE).exists()
            || data.join(naatre_server::ticket::VERIFY_FILE).exists()
        {
            return Err(format!(
                "the cache key store already exists at {} (rotate, don't re-init)",
                data.join(naatre_server::ticket::SIGNING_FILE).display()
            ));
        }
        let (der, public_hex) = naatre_server::ticket::generate_keypair()?;
        let id = naatre_server::ticket::key_id_from_hex_checked(&public_hex)?;
        let keys = naatre_server::ticket::VerifyKeys::parse(&format!(
            r#"{{"keys":[{{"id":"{id}","public":"{public_hex}","current":true}}]}}"#
        ))?;
        naatre_server::ticket::write_signing_file(data, &der)?;
        naatre_server::ticket::write_verify_file(data, &keys)?;
        Ok((id, None))
    })
}

/// `naatre cache-key rotate --data <dir>`: a fresh current pair; the old
/// key moves to the RETIRED list with its public half — its unexpired
/// tickets still verify until their ≤15-min expiry (the staleness bound),
/// then stop. Audited `cache_key_rotate` with both key ids.
pub fn cache_key_rotate(data: &Path) -> i32 {
    key_store_write(data, "rotate", || {
        let old = naatre_server::ticket::load_verify_keys(data)?;
        let (der, public_hex) = naatre_server::ticket::generate_keypair()?;
        let new_id = naatre_server::ticket::key_id_from_hex_checked(&public_hex)?;
        // Rebuild the list: the new key current, every previous key
        // retired (id + public preserved; a key id never changes).
        let mut keys_json = String::from("{\"keys\":[");
        keys_json.push_str(&format!(
            r#"{{"id":"{new_id}","public":"{public_hex}","current":true}}"#
        ));
        for k in old.keys() {
            let hex: String = k.public.iter().map(|b| format!("{b:02x}")).collect();
            let id = k.id.as_str();
            keys_json.push_str(&format!(
                r#",{{"id":"{id}","public":"{hex}","current":false}}"#
            ));
        }
        keys_json.push_str("]}");
        let keys = naatre_server::ticket::VerifyKeys::parse(&keys_json)?;
        naatre_server::ticket::write_signing_file(data, &der)?;
        naatre_server::ticket::write_verify_file(data, &keys)?;
        let retired: Vec<String> = old.keys().iter().map(|k| k.id.clone()).collect();
        Ok((new_id, Some(retired)))
    })
}

/// The shared body of the two key verbs: flock, write the files, audit.
fn key_store_write(
    data: &Path,
    action: &str,
    write: impl FnOnce() -> Result<(String, Option<Vec<String>>), String>,
) -> i32 {
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
    let (new_id, retired) = match write() {
        Ok(pair) => pair,
        Err(e) => return fail(e),
    };
    // The audit row (actor admin; ids only — never key material).
    let store = match Store::open_backend(data_dir, &cfg) {
        Ok(s) => s,
        Err(e) => return fail(e),
    };
    let conn = store.conn();
    let mut details = serde_json::json!({ "key_id": new_id });
    if let Some(retired) = &retired {
        details["retired"] = serde_json::json!(retired);
    }
    let write_row = || -> Result<(), StoreError> {
        let tx = conn.transaction()?;
        let action = if action == "init" {
            "cache_key_init"
        } else {
            "cache_key_rotate"
        };
        db::audit_append(&tx, now_ns(), "admin", action, &details.to_string())?;
        tx.commit()?;
        Ok(())
    };
    if let Err(e) = write_row() {
        return fail(e);
    }
    match &retired {
        Some(retired) => {
            println!(
                "rotated cache signing key: new {new_id}, retired {}",
                retired.join(", ")
            );
            println!(
                "the verify list {} now carries both keys — copy it to every cache \
                 tier and restart it (the retired key's unexpired tickets verify until \
                 their ≤15-min expiry)",
                data.join(naatre_server::ticket::VERIFY_FILE).display()
            );
        }
        None => {
            println!("cache signing key initialized: key id {new_id}");
            println!(
                "verify list (public keys only): {} — copy it to the cache tier \
                 (`naatre cache --key …`); the secret stays at {}",
                data.join(naatre_server::ticket::VERIFY_FILE).display(),
                data.join(naatre_server::ticket::SIGNING_FILE).display()
            );
        }
    }
    0
}

/// `naatre cache-key show --data <dir>`: the verify ids and which is
/// current (the operator's rotation checklist). Never prints secrets.
pub fn cache_key_show(data: &Path) -> i32 {
    let keys = match naatre_server::ticket::load_verify_keys(data) {
        Ok(k) => k,
        Err(e) => return fail(e),
    };
    for k in keys.keys() {
        println!("{} {}", if k.current { "current" } else { "retired" }, k.id);
    }
    println!(
        "verify list: {}",
        data.join(naatre_server::ticket::VERIFY_FILE).display()
    );
    0
}

/// `naatre cache --origin <url> --key <path|env:NAME> [--listen ADDR]
/// --data <dir>`: the LAN read tier (M13b). Fail-closed: the verify keys
/// load BEFORE the listener binds — zero keys (or a malformed entry) is
/// exit 1 with the reason, never a serving cache.
#[tokio::main]
pub async fn cache_run(
    origin: &str,
    key_source: &str,
    listen: &str,
    data: &Path,
    log_level: &str,
) -> i32 {
    // `--key <path>` or `--key env:NAME` (the brief's path-or-env shape;
    // the env form keeps the file out of argv-adjacent paths).
    // `--key <path>` or `--key env:NAME` (the brief's path-or-env shape;
    // the env form carries no file-permission semantics to check). The
    // PATH arm loads through the same 0600-at-load guard the origin's
    // own loader uses — a widened key file is refused, never loaded.
    let keys = match key_source.strip_prefix("env:") {
        Some(name) => {
            let json = std::env::var(name).unwrap_or_else(|_| {
                eprintln!("error: env var {name:?} (from --key env:{name}) is not set");
                std::process::exit(1);
            });
            naatre_server::ticket::VerifyKeys::parse(&json)
                .map_err(|e| format!("--key env:{name}: {e}"))
        }
        None => naatre_server::ticket::load_verify_key_file(std::path::Path::new(key_source))
            .map_err(|e| format!("--key {key_source:?}: {e}")),
    };
    let keys = match keys {
        Ok(k) => k,
        Err(e) => return fail(e),
    };
    let addr: std::net::SocketAddr = match listen.parse() {
        Ok(a) => a,
        Err(_) => return fail(format!("invalid listen address {listen:?}")),
    };
    if !addr.ip().is_loopback() {
        eprintln!(
            "warning: {addr} is non-loopback: cache tickets cross the network in cleartext unless fronted by TLS"
        );
    }
    let state = match naatre_server::cache_tier::CacheState::new(keys, origin, data) {
        Ok(s) => s,
        Err(e) => return fail(e),
    };
    naatre_server::middleware::logging_setup(log_level);
    let listener = match tokio::net::TcpListener::bind(addr).await {
        Ok(l) => l,
        Err(e) => return fail(e),
    };
    match naatre_server::cache_tier::run(
        listener,
        state,
        naatre_server::serve::unix_shutdown_signal(),
    )
    .await
    {
        Ok(()) => 0,
        Err(e) => fail(e),
    }
}
