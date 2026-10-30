//! Snapshot GC (M5a): one pass deletes expired snapshot rows together with
//! their whole `ephemeral/<snap>/` dirs, and sweeps rowless dirs older than
//! the max TTL (the convergence rule for an upload that crashed between the
//! blob PUTs and the snapshot POST — no row will ever come for it).
//!
//! The pass never touches `objects/` (the history CAS is not TTL'd), and
//! its ordering follows the orphan-direction rule: dirs are deleted first,
//! rows purged after, so a crash mid-pass leaves an orphan ROW (expired —
//! every fetch answers 410; the next pass purges it), never an unexpired
//! row whose blobs are gone.

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

/// Max TTL in nanoseconds — the rowless-dir sweep threshold. A dir under
/// `ephemeral/` with no snapshot row and an mtime older than
/// `now - SNAPSHOT_MAX_TTL_NS` is garbage from an interrupted upload.
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
}

/// Runs one full GC pass against `state` (its clock, its ephemeral store,
/// its store connection). Callers gate on
/// [`AppState::snapshot_gc_enabled`](crate::state::ServerState::snapshot_gc_enabled):
/// the POST route runs it opportunistically and `serve` runs it on an
/// interval; tests call it directly.
pub async fn gc_snapshots(state: &AppState) -> Result<GcReport, ApiError> {
    let now = state.now_ns();

    // Expired rows and rowless dir candidates, both by the injected clock.
    let expired = {
        let now_i = now as i64;
        state
            .read(move |conn| db::snapshots_expired(conn, now_i).map_err(ApiError::log_internal))
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
            .read(move |conn| db::snapshots_exist(conn, &ids).map_err(ApiError::log_internal))
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

    // Then the rows of the expired snapshots whose dirs are gone.
    let rows_purged = if expired.is_empty() {
        0
    } else {
        let purgable: Vec<Id> = expired
            .iter()
            .filter(|id| deleted.contains(*id))
            .copied()
            .collect();
        if purgable.is_empty() {
            0
        } else {
            state
                .write(move |store| {
                    db::snapshots_purge(store.conn(), &purgable).map_err(ApiError::log_internal)
                })
                .await?
        }
    };

    let report = GcReport {
        expired: expired.iter().filter(|id| deleted.contains(*id)).count(),
        rowless: rowless.iter().filter(|id| deleted.contains(*id)).count(),
        rows_purged,
    };
    if report.expired > 0 || report.rowless > 0 {
        tracing::info!(
            expired = report.expired,
            rowless = report.rowless,
            rows_purged = report.rows_purged,
            "snapshot GC pass"
        );
    }
    Ok(report)
}
