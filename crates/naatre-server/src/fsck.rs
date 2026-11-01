//! `naatre fsck` (M10a): the server-side consistency checker.
//!
//! Read-only by default: the mark runs cheaply over the primary tables
//! (the same [`crate::gc::mark`] the sweep uses), every dangling-reference
//! anti-join must come back zero (the never-dangling invariant), and the
//! counts — total vs marked changesets and blobs, the ledger's pending
//! candidates, the cumulative `gc` audit totals — go into one report. The
//! CAS presence side differences the DB-known inventory against what the
//! backend actually holds, per backend (fs directory walk; S3 `objects/`
//! prefix listing), with the S3 external-drift note attached: bucket-side
//! lifecycle rules or versioning can delete or resurrect objects outside
//! naatre's control, so a DB-known blob missing from the bucket is drift to
//! investigate, not proof of local corruption. The ONE write is the M11
//! `--repair` arm ([`fsck_repair`]), scoped to the pre-M11 import's
//! missing-record-row dirt class.
//!
//! Advisory mismatches (the `repo_blobs` membership index vs the repo's
//! `file_changes` references; ledger rows for data that is reachable
//! again) are REPORTED, never repaired here — the mark never reads them,
//! so they cannot affect collection; the next GC pass repairs stale ledger
//! rows as a side effect of its set-difference.
//!
//! Redaction consistency (M10b): tombstoned-but-unpurged is a NORMAL state
//! (counted, never drift — the gates stopped serving at mark time and the
//! redaction phase reclaims later); PURGED-but-still-referenced (a
//! tombstone stamped purged whose object row still exists FROM BEFORE the
//! stamp) IS drift — the purge claimed the space but a reference survived.
//! A fresh `objects` row for a purged blob id (`touched_ns` at or after
//! the stamp) is NOT drift: the write boundary is per-repo, so a repo that
//! never tombstoned the content legitimately re-uploads it after another
//! repo's purge (a changeset row needs no such arm — changeset ids hash
//! the repo id and cannot be recreated elsewhere).
//!
//! Exit contract (the CLI's pinned codes): 0 clean, 1 findings.

use std::collections::HashSet;

use naatre_core::ids::Id;
use serde::Serialize;

use crate::api_error::ApiError;
use crate::db;
use crate::gc;
use crate::state::AppState;

/// The S3 note attached to every S3-backed report: bucket-side changes are
/// external drift, not local corruption.
pub const S3_DRIFT_NOTE: &str = "S3 backend: bucket-side lifecycle rules or versioning \
     can delete or resurrect objects outside naatre's control — a DB-known blob missing \
     from the bucket may be external drift; check the bucket's lifecycle configuration \
     before treating this as corruption";

/// One fsck run's findings.
#[derive(Debug, Default, Clone, PartialEq, Serialize)]
pub struct FsckReport {
    /// The blob backend the report's presence side came from ("fs"/"s3").
    pub blob_backend: &'static str,
    // -- inventory counts ---------------------------------------------------
    /// Surviving changeset rows.
    pub changesets: usize,
    /// Changesets the primary-table mark reaches.
    pub changesets_marked: usize,
    /// `objects` rows (the DB-known blob inventory).
    pub blobs_db: usize,
    /// Blob ids present in the CAS (per backend).
    pub blobs_cas: usize,
    /// Blob ids the mark reaches.
    pub blobs_marked: usize,
    /// `trees` rows.
    pub trees: usize,
    /// Trees the mark protects.
    pub trees_protected: usize,
    /// `gc_pending` rows (first-unreachable ledger).
    pub candidates_pending: usize,
    /// Ledger rows already past the grace cut (the next pass collects
    /// them, unless the re-verification spares them).
    pub candidates_past_grace: usize,
    /// Cumulative `gc` audit totals: (changesets, blobs, trees) across all
    /// recorded collecting passes.
    pub collected: (usize, usize, usize),
    /// How many collecting passes the audit log carries.
    pub gc_passes: usize,
    // -- findings (all zero on a clean store) --------------------------------
    /// Dangling-reference counts, named per check
    /// ([`db::fsck_danglings`]).
    pub danglings: Vec<(String, usize)>,
    /// DB-known blobs whose CAS bytes are absent (the read-breaking
    /// direction; on S3 see `external_drift`).
    pub bytes_missing: usize,
    /// CAS-present blob ids without an `objects` row (the orphaned-upload
    /// direction — collectible garbage; note the offline import records no
    /// `objects` rows, so a freshly imported repo reports its blobs here).
    pub cas_unrecorded: usize,
    /// Ledger rows whose data is reachable again (the next pass repairs).
    pub stale_candidates: usize,
    /// Advisory: `file_changes` blob references with no `repo_blobs`
    /// membership row (per repo, summed).
    pub advisory_membership_missing: usize,
    /// Advisory: `repo_blobs` membership rows no `file_changes` reference
    /// (per repo, summed; rows for unknown objects count under
    /// `danglings` instead).
    pub advisory_membership_extra: usize,
    /// Actively-tombstoned objects (M10b redactions with no `purged_ns`).
    /// Tombstoned-but-unpurged is a NORMAL state — the gates stopped
    /// serving at mark time; the redaction phase reclaims later.
    pub redactions_active: usize,
    /// Tombstones whose purge completed (`purged_ns` stamped).
    pub redactions_purged: usize,
    /// DRIFT: tombstones stamped purged whose object row STILL exists from
    /// before the stamp (a changeset row for kind `changeset`; an `objects`
    /// row for kind `blob` whose `touched_ns` predates the stamp) — the
    /// purge claimed the space but a reference survived, the purge-side
    /// violation of the never-dangling invariant. An `objects` row touched
    /// at or after the stamp is NOT drift: a repo that never tombstoned the
    /// content re-uploads it legitimately (the write boundary is per-repo).
    pub redaction_purge_drift: usize,
    /// Set when `blob_backend` is "s3": the external-drift note.
    pub external_drift: Option<&'static str>,
    /// The `--repair` arm (M11): the referenced blobs whose `objects`
    /// record row was backfilled, sorted hex. Empty for a plain scan.
    pub backfilled: Vec<String>,
}

impl FsckReport {
    /// 0 findings: every dangling count zero, no DB-known blob missing
    /// its bytes, and no purged-but-still-referenced tombstone. Advisory
    /// mismatches, unrecorded CAS bytes, stale ledger rows, and active
    /// (unpurged) tombstones do NOT dirty the store (they are repairable,
    /// informational, or the redaction design's normal state); missing
    /// BYTES for a DB-known blob and purge drift do.
    pub fn clean(&self) -> bool {
        self.danglings.iter().all(|(_, n)| *n == 0)
            && self.bytes_missing == 0
            && self.redaction_purge_drift == 0
    }
}

/// One read-pass bundle: the DB-side inventories fsck differences against
/// the CAS and the ledger.
struct Inventory {
    blobs_db: Vec<Id>,
    trees: Vec<Id>,
    changesets: usize,
    /// Per-repo `(file_changes blob references, repo_blobs membership)`.
    membership: Vec<(HashSet<Id>, HashSet<Id>)>,
}

/// Runs every check against `state` (read-only; its blob store is touched
/// only through presence reads and the id listing). The host-local verb and
/// tests call this directly.
pub async fn fsck(state: &AppState) -> Result<FsckReport, ApiError> {
    scan(state).await
}

/// The `--repair` arm (M11): the one write fsck can do, scoped to EXACTLY
/// the dirt class the pre-M11 import left behind — blobs this store
/// REFERENCES (membership or file_changes) whose CAS bytes are present but
/// whose `objects` record row is missing. Each candidate is hash-verified
/// through the blob store's read path, the row is upserted (the same
/// idempotent statement the record step and import use), and ONE
/// `fsck_repair` audit row names the count (nothing collected → no row,
/// the gc-audit pattern). Anything else fsck flags is NOT touched — the
/// candidate query is the whole work list. The scan re-runs after the
/// backfill, so the report reflects the post-repair store.
pub async fn fsck_repair(state: &AppState) -> Result<FsckReport, ApiError> {
    let now_i = state.now_ns() as i64;
    // The work list: referenced blobs without their record row.
    let candidates = state
        .read(|conn| db::fsck_repair_candidates(conn).map_err(ApiError::log_internal))
        .await?;
    let mut backfilled: Vec<Id> = Vec::new();
    if !candidates.is_empty() {
        let cas = std::sync::Arc::clone(&state.cas);
        let candidates = candidates.clone();
        backfilled = state
            .write(move |store| {
                let conn = store.conn();
                // Hash-verified read first: a candidate whose bytes are
                // ABSENT is not this dirt class (that is `bytes_missing`,
                // a different finding) and is left alone.
                let mut done: Vec<Id> = Vec::new();
                let tx = conn
                    .transaction_immediate()
                    .map_err(ApiError::log_internal)?;
                for id in &candidates {
                    let present = {
                        let cas = std::sync::Arc::clone(&cas);
                        let ids = [*id];
                        naatre_store::BlobStore::has(cas.as_ref(), &ids)
                            .map_err(ApiError::log_internal)?
                    };
                    if !present[0] {
                        continue;
                    }
                    let bytes = {
                        let cas = std::sync::Arc::clone(&cas);
                        let id = *id;
                        state_blocking_get(&cas, &id)?
                    };
                    // The idempotent re-put returns the STORED size without
                    // touching the existing object (the CAS put contract).
                    let info = {
                        let cas = std::sync::Arc::clone(&cas);
                        state_blocking_put(&cas, &bytes)?
                    };
                    debug_assert_eq!(info.id, *id);
                    db::object_row_upsert(&tx, id, bytes.len() as i64, info.csize as i64, now_i)
                        .map_err(ApiError::log_internal)?;
                    done.push(*id);
                }
                if !done.is_empty() {
                    db::audit_append(
                        &tx,
                        now_i,
                        "system",
                        "fsck_repair",
                        &serde_json::json!({ "backfilled_objects": done.len() }).to_string(),
                    )
                    .map_err(ApiError::log_internal)?;
                }
                tx.commit().map_err(ApiError::log_internal)?;
                Ok(done)
            })
            .await?;
    }
    let mut report = scan(state).await?;
    backfilled.sort_by_key(|id| id.to_hex());
    report.backfilled = backfilled.iter().map(|id| id.to_hex()).collect();
    Ok(report)
}

/// The blocking CAS get for the repair arm (the sync facade runs on the
/// state's blocking pool, never a runtime thread).
fn state_blocking_get(
    cas: &std::sync::Arc<dyn naatre_store::BlobStore>,
    id: &naatre_core::ids::Id,
) -> Result<Vec<u8>, ApiError> {
    naatre_store::BlobStore::get(cas.as_ref(), id).map_err(ApiError::log_internal)
}

/// The blocking CAS put for the repair arm.
fn state_blocking_put(
    cas: &std::sync::Arc<dyn naatre_store::BlobStore>,
    bytes: &[u8],
) -> Result<naatre_store::blob::PutInfo, ApiError> {
    naatre_store::BlobStore::put(cas.as_ref(), bytes).map_err(ApiError::log_internal)
}

/// The full read-only scan behind [`fsck`] and [`fsck_repair`].
async fn scan(state: &AppState) -> Result<FsckReport, ApiError> {
    let now_i = state.now_ns() as i64;
    let cut = now_i.saturating_sub(state.gc_grace.as_nanos() as i64);

    // The mark + the anti-joins + the ledger counts + the stale-candidate
    // check + the redaction counts, in one read pass.
    let (mark_counts, danglings, candidates, stale_candidates, redactions) = state
        .read(move |conn| {
            let mark = gc::mark(conn, now_i).map_err(ApiError::log_internal)?;
            let danglings = db::fsck_danglings(conn).map_err(ApiError::log_internal)?;
            let candidates = db::gc_candidate_counts(conn, cut).map_err(ApiError::log_internal)?;
            let mut stale = 0usize;
            for scope in [db::GC_SCOPE_CS, db::GC_SCOPE_BLOB, db::GC_SCOPE_TREE] {
                for id in db::gc_pending_ids(conn, scope).map_err(ApiError::log_internal)? {
                    if mark.reachable(scope, &id) {
                        stale += 1;
                    }
                }
            }
            let redactions = db::fsck_redactions(conn).map_err(ApiError::log_internal)?;
            Ok((
                (mark.cs_len(), mark.blobs_len(), mark.trees_len()),
                danglings,
                candidates,
                stale,
                redactions,
            ))
        })
        .await?;

    // The inventories (one read pass).
    let inventory = state
        .read(|conn| {
            let blobs_db = db::gc_all_object_ids(conn).map_err(ApiError::log_internal)?;
            let trees = db::gc_all_tree_ids(conn).map_err(ApiError::log_internal)?;
            let mut changesets = 0usize;
            let mut membership = Vec::new();
            for repo_id in db::gc_repos(conn).map_err(ApiError::log_internal)? {
                changesets += db::gc_all_changesets(conn, repo_id)
                    .map_err(ApiError::log_internal)?
                    .len();
                let expected: HashSet<Id> = db::gc_file_change_blobs(conn, repo_id)
                    .map_err(ApiError::log_internal)?
                    .into_iter()
                    .map(|(_, blob)| blob)
                    .collect();
                let actual: HashSet<Id> = db::gc_repo_blobs(conn, repo_id)
                    .map_err(ApiError::log_internal)?
                    .into_iter()
                    .collect();
                membership.push((expected, actual));
            }
            Ok(Inventory {
                blobs_db,
                trees,
                changesets,
                membership,
            })
        })
        .await?;

    // CAS presence (blocking): what the DB-known ids hold bytes for, and
    // what the CAS holds beyond the rows.
    let cas = std::sync::Arc::clone(&state.cas);
    let cas_ids = state
        .blocking(move || cas.ids().map_err(ApiError::log_internal))
        .await?;
    let cas_set: HashSet<Id> = cas_ids.iter().copied().collect();
    let blobs_db = inventory.blobs_db;
    let missing: Vec<usize> = {
        let cas = std::sync::Arc::clone(&state.cas);
        let known = blobs_db.clone();
        state
            .blocking(move || {
                let mut counts = Vec::new();
                for chunk in known.chunks(1000) {
                    let hits = naatre_store::BlobStore::has(cas.as_ref(), chunk)
                        .map_err(ApiError::log_internal)?;
                    counts.push(hits.iter().filter(|h| !**h).count());
                }
                Ok(counts)
            })
            .await?
    };
    let bytes_missing: usize = missing.into_iter().sum();
    let cas_unrecorded = cas_set.iter().filter(|id| !blobs_db.contains(*id)).count();

    // Advisory membership mismatches, per repo.
    let (mut missing_membership, mut extra_membership) = (0usize, 0usize);
    for (expected, actual) in &inventory.membership {
        missing_membership += expected.difference(actual).count();
        extra_membership += actual.difference(expected).count();
    }

    // Cumulative gc audit totals (the append-only log is the record of
    // what past passes collected). Paged by ACTION, not by a capped
    // unfiltered read: a busy audit log would otherwise crowd the `gc`
    // rows out of one 10k page and silently under-count every older pass.
    // The JSON details parse in memory (JSON is not statement surface).
    let page: i64 = 10_000;
    let (mut collected, mut gc_passes) = ((0usize, 0usize, 0usize), 0usize);
    let mut cursor = i64::MAX;
    loop {
        let rows = {
            let cursor = cursor;
            state
                .read(move |conn| {
                    db::audit_page_action(conn, "gc", cursor, page).map_err(ApiError::log_internal)
                })
                .await?
        };
        let done = rows.len() < page as usize;
        for (id, details_json) in rows {
            cursor = cursor.min(id);
            gc_passes += 1;
            if let Ok(v) = serde_json::from_str::<serde_json::Value>(&details_json) {
                collected.0 += v.get("cs").and_then(|x| x.as_u64()).unwrap_or(0) as usize;
                collected.1 += v.get("blobs").and_then(|x| x.as_u64()).unwrap_or(0) as usize;
                collected.2 += v.get("trees").and_then(|x| x.as_u64()).unwrap_or(0) as usize;
            }
        }
        if done {
            break;
        }
    }

    Ok(FsckReport {
        blob_backend: state.blob_backend,
        changesets: inventory.changesets,
        changesets_marked: mark_counts.0,
        blobs_db: blobs_db.len(),
        blobs_cas: cas_set.len(),
        blobs_marked: mark_counts.1,
        trees: inventory.trees.len(),
        trees_protected: mark_counts.2,
        candidates_pending: candidates.0 as usize,
        candidates_past_grace: candidates.1 as usize,
        collected,
        gc_passes,
        danglings: danglings
            .into_iter()
            .map(|(name, count)| (name.to_string(), count.max(0) as usize))
            .collect(),
        bytes_missing,
        cas_unrecorded,
        stale_candidates,
        advisory_membership_missing: missing_membership,
        advisory_membership_extra: extra_membership,
        redactions_active: redactions.0.max(0) as usize,
        redactions_purged: redactions.1.max(0) as usize,
        redaction_purge_drift: redactions.2.max(0) as usize,
        external_drift: (state.blob_backend == "s3").then_some(S3_DRIFT_NOTE),
        backfilled: Vec::new(),
    })
}
