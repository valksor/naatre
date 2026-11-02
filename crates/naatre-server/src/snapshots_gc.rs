//! Snapshot GC (M5a): one pass deletes expired snapshot rows together with
//! their whole `ephemeral/<snap>/` dirs, and sweeps rowless dirs older than
//! the default TTL, 7 days (the convergence rule for an upload that crashed
//! between the blob PUTs and the snapshot POST — no row will ever come for
//! it).
//!
//! The pass also sweeps snapshot-created tree rows: the `trees`/
//! `tree_entries` rows a snapshot POST wrote carry no `repo_trees`
//! membership, and ZERO-JUNCTION means snapshot-only by definition — every
//! history-tree write path inserts a junction row and migration 3's
//! backfill covered pre-migration rows, so no provenance marker is needed
//! (do not half-add one later; the invariant lives in junction
//! completeness). A zero-junction candidate survives if PROTECTED, and
//! protection is TRANSITIVE through candidates: seeds are candidates in an
//! unexpired snapshot's closure, candidates that are changeset roots, and
//! candidates with a RETAINED parent (junctioned tree or changeset root
//! pointing at them as a child); then candidate children of protected
//! candidates, recursively — a junction-less chain under a retained
//! parent survives in full. The walk stays inside the candidate set, but
//! the cost grows with TOTAL tree rows, not only with snapshots: one
//! indexed probe per tree row finds the candidates, the live CTE
//! recursively walks the full closures of unexpired snapshots, and each
//! candidate costs indexed lookups (the accurate wording lives in
//! `docs/design.md` and `db::unprotected_trees`). Mark and sweep run in
//! ONE write-slot transaction with the expired-row purge, so a snapshot
//! POST reusing a shared subtree cannot land between mark and delete;
//! because the sweep is stated over reachability (not this pass's expired
//! set), a crash between a row purge and the sweep also converges on the
//! next pass.
//!
//! The pass never touches `objects/` (the history CAS is not TTL'd), and
//! its ordering follows the orphan-direction rule: dirs are deleted first,
//! rows purged after, so a crash mid-pass leaves an orphan ROW (expired —
//! every fetch answers 410; the next pass purges it), never an unexpired
//! row whose blobs are gone.
//!
//! A pass that collected something (expired dirs, rowless dirs, or swept
//! tree rows) writes ONE `snapshot_gc` audit row — action and the report
//! counts in `details`, actor `"system"` (no principal drives a background
//! pass) — inside the same write-slot transaction as its DB effects. An
//! idle pass writes nothing: a quiet server must not spam the append-only
//! log.

use std::collections::HashSet;
use std::sync::Arc;

use naatre_core::ids::Id;

use crate::api_error::ApiError;
use crate::db;
use crate::state::AppState;

/// Default snapshot TTL in seconds: 7 days (the `--ttl` default).
pub const SNAPSHOT_DEFAULT_TTL_SECS: u64 = 604_800;

/// Snapshot TTL floor in seconds (the plan's "min 1 minute"), enforced
/// client-side and again at the POST route (422 below it).
pub const SNAPSHOT_MIN_TTL_SECS: u64 = 60;

/// The rowless-dir sweep threshold: the DEFAULT TTL (7 days) in nanoseconds,
/// not the 366-day TTL ceiling (`SNAPSHOT_TTL_MAX_SECS`, enforced at the POST
/// route). A dir under `ephemeral/` with no snapshot row and an mtime older
/// than `now - SNAPSHOT_MAX_TTL_NS` is garbage from an interrupted upload.
pub const SNAPSHOT_MAX_TTL_NS: u64 = SNAPSHOT_DEFAULT_TTL_SECS * 1_000_000_000;

/// What one GC pass deleted (for logs and tests).
#[derive(Debug, Default, PartialEq, Eq)]
pub struct GcReport {
    /// Expired snapshots whose dirs were removed.
    pub expired: usize,
    /// Rowless dirs swept.
    pub rowless: usize,
    /// Snapshot rows purged.
    pub rows_purged: usize,
    /// Snapshot-only `trees`/`tree_entries` rows purged (no junction
    /// membership, no changeset reference, unreachable from live snapshots).
    pub trees_purged: usize,
}

/// Runs one full GC pass against `state` (its clock, its ephemeral store,
/// its store connection). Callers gate on
/// [`AppState::snapshot_gc_enabled`](crate::state::ServerState::snapshot_gc_enabled):
/// the POST route runs it opportunistically and `serve` runs it on an
/// interval; tests call it directly.
pub async fn gc_snapshots(state: &AppState) -> Result<GcReport, ApiError> {
    let now = state.now_ns();

    // Expired rows and rowless dir candidates, both by the injected clock.
    // The expired union walks every repo's file through the registry
    // (the M13a aggregation shape; the single-DB pass reads the one file).
    let expired: Vec<Id> = {
        let now_i = now as i64;
        state
            .read_all(move |cx| {
                let mut out = Vec::new();
                for repo_id in db::gc_repos(cx.cat()).map_err(ApiError::log_internal)? {
                    cx.with_repo(repo_id, |conn| {
                        out.extend(
                            db::snapshots_expired(conn, now_i).map_err(ApiError::log_internal)?,
                        );
                        Ok(())
                    })
                    .map_err(ApiError::log_internal)?;
                }
                Ok(out)
            })
            .await?
    };
    let candidates = {
        let ephemeral = Arc::clone(&state.ephemeral);
        let threshold = now.saturating_sub(SNAPSHOT_MAX_TTL_NS) as i64;
        let now_i = now as i64;
        state
            .blocking(move || {
                ephemeral
                    .rowless_dirs(threshold, now_i)
                    .map_err(ApiError::log_internal)
            })
            .await?
    };

    // Cross-check: a dir with a row (live or expired) is not garbage — the
    // expired path owns it. Non-hex dir names can never be snapshot ids and
    // are skipped (delete_snapshot refuses them; nothing naatre wrote).
    let hex_ids: Vec<Id> = candidates.iter().filter_map(|s| Id::from_hex(s)).collect();
    let rowless: Vec<Id> = if hex_ids.is_empty() {
        Vec::new()
    } else {
        let ids = hex_ids.clone();
        let exists = state
            .read_all(move |cx| {
                let mut any = vec![false; ids.len()];
                for repo_id in db::gc_repos(cx.cat()).map_err(ApiError::log_internal)? {
                    cx.with_repo(repo_id, |conn| {
                        for (i, hit) in db::snapshots_exist(conn, &ids)
                            .map_err(ApiError::log_internal)?
                            .into_iter()
                            .enumerate()
                        {
                            any[i] = any[i] || hit;
                        }
                        Ok(())
                    })
                    .map_err(ApiError::log_internal)?;
                }
                Ok(any)
            })
            .await?;
        hex_ids
            .iter()
            .zip(exists)
            .filter(|(_, exists)| !*exists)
            .map(|(id, _)| *id)
            .collect()
    };

    // Dirs first (idempotent; a real IO error is logged and retried by the
    // next pass — the row stays so nothing is lost).
    let deleted: HashSet<Id> = if expired.is_empty() && rowless.is_empty() {
        HashSet::new()
    } else {
        let ephemeral = Arc::clone(&state.ephemeral);
        let all: Vec<Id> = expired.iter().chain(rowless.iter()).copied().collect();
        state
            .blocking(move || {
                let mut deleted = HashSet::new();
                for id in &all {
                    match ephemeral.delete_snapshot(&id.to_hex()) {
                        Ok(()) => {
                            deleted.insert(*id);
                        }
                        Err(e) => {
                            tracing::error!(snapshot = %id.to_hex(), error = %e, "snapshot GC dir delete failed");
                        }
                    }
                }
                Ok(deleted)
            })
            .await?
    };

    // All remaining DB-side work — the expired-row purge AND the
    // snapshot-tree sweep — runs in ONE write-slot transaction: mark
    // (reachable from live snapshots ∪ junction ∪ changeset roots), purge
    // expired rows, sweep unmarked tree rows, commit atomically. A
    // snapshot POST reusing a shared subtree cannot land between mark and
    // delete: both paths go through `state.write`, the single-writer slot.
    // The idle probe (below) is what keeps a quiet server's periodic pass
    // off the write slot entirely.
    let purgable: Vec<Id> = expired
        .iter()
        .filter(|id| deleted.contains(*id))
        .copied()
        .collect();
    let rowless_swept = rowless.iter().filter(|id| deleted.contains(*id)).count();
    // A rowless-only sweep still owes its audit row, so it counts as
    // needing the write slot even when no expired rows and no sweepable
    // tree rows exist.
    let needs_write = !purgable.is_empty() || rowless_swept > 0 || {
        state
            .read_all(move |cx| {
                let mut any = false;
                for repo_id in db::gc_repos(cx.cat()).map_err(ApiError::log_internal)? {
                    cx.with_repo(repo_id, |conn| {
                        if db::any_junctionless_trees(conn).map_err(ApiError::log_internal)? {
                            any = true;
                        }
                        Ok(())
                    })
                    .map_err(ApiError::log_internal)?;
                }
                Ok(any)
            })
            .await?
    };
    let (rows_purged, trees_purged) = if !needs_write {
        (0, 0)
    } else {
        let now_i = now as i64;
        // The sweep runs under the coordinated batch: the mark (the
        // unprotected-tree selection) and the deletes must be atomic
        // against a snapshot POST reusing a shared subtree — the property
        // the single-DB write slot provided, now the catalog + repo slots
        // with the sweep flag up.
        let purgable = purgable.clone();
        state
            .write_all(move |cx| {
                // Per-repo: the expired-row purge AND the tree sweep in
                // this repo's file (its own transaction — the repo file is
                // the atomicity unit in the split; single-DB every repo
                // routes to the one connection and the last transaction's
                // audit row rides it).
                let mut total_rows = 0usize;
                let mut total_trees = 0usize;
                for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                    let conn = cx.repo(repo_id)?;
                    let tx = conn.transaction().map_err(ApiError::log_internal)?;
                    let rows_purged =
                        db::snapshots_purge(&tx, &purgable).map_err(ApiError::log_internal)?;
                    let garbage =
                        db::unprotected_trees(&tx, now_i).map_err(ApiError::log_internal)?;
                    let trees_purged =
                        db::delete_tree_rows(&tx, &garbage).map_err(ApiError::log_internal)?;
                    tx.commit().map_err(ApiError::log_internal)?;
                    total_rows += rows_purged;
                    total_trees += trees_purged;
                }
                // One audit row per collecting pass, committed with the
                // catalog-side delete bookkeeping (the loop's last
                // transaction). A pass that reached the slots but collected
                // nothing (only protected junction-less rows alive) writes
                // none — a quiet server must not spam the append-only log.
                if !purgable.is_empty() || rowless_swept > 0 || total_trees > 0 {
                    let tx = cx
                        .cat()
                        .transaction_immediate()
                        .map_err(ApiError::log_internal)?;
                    db::audit_append(
                        &tx,
                        now_i,
                        "system",
                        "snapshot_gc",
                        &serde_json::json!({
                            "expired": purgable.len(),
                            "rowless": rowless_swept,
                            "rows_purged": total_rows,
                            "trees_purged": total_trees,
                        })
                        .to_string(),
                    )
                    .map_err(ApiError::log_internal)?;
                    tx.commit().map_err(ApiError::log_internal)?;
                }
                Ok((total_rows, total_trees))
            })
            .await?
    };

    let report = GcReport {
        expired: expired.iter().filter(|id| deleted.contains(*id)).count(),
        rowless: rowless.iter().filter(|id| deleted.contains(*id)).count(),
        rows_purged,
        trees_purged,
    };
    if report.expired > 0 || report.rowless > 0 || report.trees_purged > 0 {
        tracing::info!(
            expired = report.expired,
            rowless = report.rowless,
            rows_purged = report.rows_purged,
            trees_purged = report.trees_purged,
            "snapshot GC pass"
        );
    }
    Ok(report)
}
