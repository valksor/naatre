//! Reachability GC (M10a): mark-and-sweep over the primary tables.
//!
//! The mark reads PRIMARY TABLES ONLY — refs targets, the changeset graph
//! (changesets + parents + file_changes), unexpired snapshots' tree
//! closures, `lands` rows' head/base/result references, `git_map` — and
//! never the `repo_blobs` membership index or the M6 derived tables: those
//! are advisory, never authoritative (a lying refcount cannot preserve a
//! dead blob, and cannot doom a live one — the mark does not consult it).
//! Reachability is the transitive closure: every changeset reachable via
//! parents from a seed (ref target, lands head/base/landed, git_map cs),
//! every blob in the marked changesets' `file_changes` and in the protected
//! trees' entries, every tree under a marked changeset root or an unexpired
//! snapshot.
//!
//! Pre-rebase originals are NOT roots: a changeset is garbage the moment no
//! seed reaches it (a re-land overwrites the lands row and orphans the old
//! head; a commit that never lands is garbage from birth). Collection waits
//! out the grace window ([`GC_DEFAULT_GRACE_SECS`], `backend.toml`
//! `gc_grace_secs`): a candidate ledger row (`gc_pending`, scoped cs/blob/
//! tree) records when the data was FIRST observed unreachable, and only
//! rows older than the window collect — so in-flight client work (an
//! uploaded blob about to be referenced, a pre-rebase head a conflict retry
//! still points at) survives a pass that races it.
//!
//! Three defenses close the byte-deletion races with the upload path: every
//! PUT refreshes `objects.touched_ns` in the same transaction as the row (a
//! touched blob re-enters grace; the sweep's selection refuses candidates
//! touched inside the window); the blob sweep is TWO-PHASE — phase A only
//! CONDEMNS a candidate (`gc_pending.condemned_at`, committed, bytes
//! untouched), and a LATER pass deletes bytes+rows only while the mark is
//! still set, while any PUT's row touch clears it, so the byte-delete
//! window is a full pass interval wide; and each phase-B batch re-verifies
//! no-references INSIDE its deleting transaction (the single-writer slot
//! serializes it against the upload record step, which checks presence
//! before writing the row and answers a retryable 503 `blob_swept` for
//! bytes a batch removed mid-upload — the client re-PUTs, idempotent at a
//! content-addressed URL; a row can never name absent bytes, and committed
//! history never references collected bytes).
//!
//! Crash-mid-GC is idempotent: the pass runs in generation-ordered,
//! write-txn batches (the batched-backfill pattern — the slot yields
//! between batches), and the sweep is a set-difference over the
//! `gc_pending` ledger, not a destructive cursor. A crash at any point
//! (between mark and sweep, between batches, mid-batch — bytes unlinked
//! before the row transaction commits, the M5 orphan-direction rule) leaves
//! a half-swept state the re-run converges on: candidates keep their FIRST
//! `first_ns` (INSERT OR IGNORE), live data is re-marked and repaired, and
//! the collectible re-verification runs inside each deleting transaction.
//! Debug builds expose the pin points ([`GC_CRASH_POINTS`], the M4
//! `NAATRE_CRASH_POINT` pattern).
//!
//! The trees side leaves the M5 snapshot-GC walk alone (zero-junction
//! snapshot rows stay its business; the existing transitive walk stays).
//! This sweep collects JUNCTIONED history trees whose changesets were
//! collected — a tree row survives while any surviving changeset root or
//! unexpired snapshot reaches it, dies with the last one, and its
//! `repo_trees` junction rows die with it (`repo_has_tree` must never admit
//! a tree whose bytes are gone).
//!
//! A pass that collected anything writes ONE `gc` audit row — action, the
//! report counts and the grace in `details`, actor `"system"` (no principal
//! drives a background or host-local pass) — after its DB effects. An idle
//! pass writes nothing: a quiet server must not spam the append-only log.

use std::collections::HashSet;
use std::sync::{Arc, OnceLock};

use naatre_core::ids::Id;
use serde_json::json;

use crate::api_error::ApiError;
use crate::db;
use crate::state::AppState;

/// Default collection grace window: 30 days (`backend.toml`
/// `gc_grace_secs` overrides). Long enough that a conflict retry parked on
/// a pre-rebase head, or a client between blob-PUT and changeset-POST, is
/// never overtaken by a pass.
pub const GC_DEFAULT_GRACE_SECS: u64 = 30 * 24 * 60 * 60;

/// Default serve-side background pass interval: one hour (the M5 snapshot
/// GC runs every 60 s; the reachability mark is a full-table walk and does
/// not belong on a minute cadence). `backend.toml` `gc_interval_secs`
/// overrides; 0 disables the background task.
pub const GC_DEFAULT_INTERVAL_SECS: u64 = 3_600;

/// Changeset/tree candidates per collection batch (row-only work). One
/// batch is one write-slot hold, so its size bounds how long a concurrent
/// write can 503 behind the GC — the batched-backfill tradeoff.
pub const GC_BATCH_ROWS: usize = 10_000;

/// Blob candidates per collection batch (CAS-bound: unlinks on fs,
/// serialized delete_stream batches on S3 — the batch size bounds the S3
/// facade's stall, see `s3.rs`'s serialization note).
pub const GC_BATCH_BLOBS: usize = 256;

/// The GC pass's closed crash-point enum (debug builds; the M4
/// `NAATRE_CRASH_POINT` pattern — the env var is read once per process, so
/// pins run in their own re-exec'd subprocess or binary):
///
/// * `gc_after_mark` — between mark and sweep (nothing has been written).
/// * `gc_after_candidates` — the ledger is updated, nothing collected yet.
/// * `gc_mid_cs_batch` — between changeset collection batches.
/// * `gc_mid_blob_batch` — INSIDE a blob batch: the CAS bytes are already
///   unlinked, the row transaction not yet committed (the orphan-direction
///   pin: the re-run must converge over rows claiming absent bytes).
pub const GC_CRASH_POINTS: [&str; 4] = [
    "gc_after_mark",
    "gc_after_candidates",
    "gc_mid_cs_batch",
    "gc_mid_blob_batch",
];

/// Panics when `NAATRE_CRASH_POINT` (read once per process) names this
/// call site; debug builds only, release no-op — same contract as
/// [`crate::land::crash_point`], its own OnceLock so land's closed enum
/// stays land's.
#[cfg(debug_assertions)]
pub fn crash_point(name: &str) {
    static POINT: OnceLock<Option<String>> = OnceLock::new();
    let want = POINT.get_or_init(|| std::env::var("NAATRE_CRASH_POINT").ok());
    if want.as_deref() == Some(name) {
        panic!("crash point {name} triggered");
    }
}

/// Release no-op (see the debug build above).
#[cfg(not(debug_assertions))]
pub fn crash_point(_name: &str) {}

/// What one GC pass did (for logs, the CLI, and tests).
#[derive(Debug, Default, Clone, PartialEq, Eq, serde::Serialize)]
pub struct GcReport {
    /// Ledger rows added (first-observed-unreachable this pass).
    pub candidates_new: usize,
    /// Ledger rows removed because the data is reachable again (repair).
    pub candidates_repaired: usize,
    /// Changesets collected (rows + parents + file changes + advisory M6
    /// rows).
    pub cs_collected: usize,
    /// Blobs collected in phase B (CAS bytes + objects row + advisory
    /// membership + ledger row).
    pub blobs_collected: usize,
    /// Blobs CONDEMNED in phase A this pass (marked for a later pass's
    /// delete; the bytes are untouched until phase B and any upload clears
    /// the mark first).
    pub blobs_condemned: usize,
    /// History tree rows collected (with entries + junction rows).
    pub trees_collected: usize,
}

impl GcReport {
    /// True when the pass collected anything (the audit-row condition).
    pub fn collected_any(&self) -> bool {
        self.cs_collected > 0 || self.blobs_collected > 0 || self.trees_collected > 0
    }
}

/// One repo's primary-table mark.
#[derive(Default)]
struct RepoMark {
    cs: HashSet<Id>,
    /// Blobs referenced by the marked changesets' `file_changes`.
    blobs: HashSet<Id>,
    /// Trees protected: the closure of the marked roots plus the repo's
    /// unexpired snapshots.
    trees: HashSet<Id>,
    /// Blob entries inside the protected trees.
    tree_blobs: HashSet<Id>,
}

/// The whole-pass mark (the union across repos).
#[derive(Default)]
pub(crate) struct Mark {
    cs: HashSet<Id>,
    blobs: HashSet<Id>,
    trees: HashSet<Id>,
}

impl Mark {
    /// Is `id` reachable under ledger `scope` (a [`db::GC_SCOPE_*`]
    /// value)? fsck's stale-candidate check.
    pub fn reachable(&self, scope: &str, id: &Id) -> bool {
        match scope {
            db::GC_SCOPE_CS => self.cs.contains(id),
            db::GC_SCOPE_BLOB => self.blobs.contains(id),
            _ => self.trees.contains(id),
        }
    }

    /// Marked-changeset count (fsck's totals).
    pub fn cs_len(&self) -> usize {
        self.cs.len()
    }

    /// Marked-blob count (fsck's totals).
    pub fn blobs_len(&self) -> usize {
        self.blobs.len()
    }

    /// Protected-tree count (fsck's totals).
    pub fn trees_len(&self) -> usize {
        self.trees.len()
    }
}

/// Parses the settled lands' recorded `landed` changeset ids out of one
/// repo's `result_json` payloads (the plan's "lands rows' … result
/// references"; JSON is not statement surface).
fn landed_ids(results: Vec<String>, out: &mut HashSet<Id>) {
    for result in results {
        if let Ok(v) = serde_json::from_str::<serde_json::Value>(&result)
            && let Some(hex) = v.get("landed").and_then(|l| l.as_str())
            && let Some(id) = Id::from_hex(hex)
        {
            out.insert(id);
        }
    }
}

/// Computes the mark over `conn` at `now_ns`. Plain statement reads plus
/// chunked, index-led BFS (the changeset closure walks the
/// `(repo_id, cs, …)` PRIMARY KEY prefix, the tree closure the
/// `(tree_id, name)` prefix) — every pass costs O(primary rows), the same
/// cost class the plan's mark clause names.
pub(crate) fn mark(conn: &naatre_store::Db, now_ns: i64) -> Result<Mark, naatre_store::DbError> {
    let mut mark = Mark::default();
    for repo_id in db::gc_repos(conn)? {
        let mut m = RepoMark::default();

        // Seeds: refs, lands head/base, git_map — plus the lands
        // result_json "landed" ids.
        let mut seeds = db::gc_seed_changesets(conn, repo_id)?;
        let mut landed = HashSet::new();
        landed_ids(db::gc_land_result_jsons(conn, repo_id)?, &mut landed);
        seeds.extend(landed);

        // The parent closure, chunked BFS over the PK prefix (the seeds
        // themselves are reachable — they enter the set before the walk).
        let mut frontier = Vec::new();
        for s in seeds {
            if m.cs.insert(s) {
                frontier.push(s);
            }
        }
        while !frontier.is_empty() {
            let parents = db::gc_parent_ids(conn, repo_id, &frontier)?;
            let mut next = Vec::new();
            for p in parents {
                if m.cs.insert(p) {
                    next.push(p);
                }
            }
            frontier = next;
        }

        // The marked changesets' file_changes blobs (one full read of the
        // repo's rows; the reachable filter runs in memory).
        for (cs, blob) in db::gc_file_change_blobs(conn, repo_id)? {
            if m.cs.contains(&cs) {
                m.blobs.insert(blob);
            }
        }

        // Protected trees: the closure of the marked changesets' roots plus
        // the repo's unexpired snapshots, chunked BFS over tree_entries.
        let mut roots: Vec<Id> = db::gc_all_changesets(conn, repo_id)?
            .into_iter()
            .filter(|(id, _)| m.cs.contains(id))
            .filter_map(|(_, root)| root)
            .collect();
        roots.extend(db::gc_snapshot_roots(conn, repo_id, now_ns)?);
        let mut frontier = Vec::new();
        for r in roots {
            if m.trees.insert(r) {
                frontier.push(r);
            }
        }
        while !frontier.is_empty() {
            let mut next = Vec::new();
            for chunk in frontier.chunks(db::GC_IN_CHUNK) {
                for (kind, object) in db::gc_tree_children(conn, chunk)? {
                    if kind == 1 {
                        if m.trees.insert(object) {
                            next.push(object);
                        }
                    } else {
                        m.tree_blobs.insert(object);
                    }
                }
            }
            frontier = next;
        }

        mark.cs.extend(&m.cs);
        mark.blobs.extend(&m.blobs);
        mark.blobs.extend(&m.tree_blobs);
        mark.trees.extend(&m.trees);
    }
    Ok(mark)
}

/// Runs one full GC pass against `state` (its clock, its blob store, its
/// store connection). Callers gate on `state.gc_enabled`: `serve` runs it
/// on an interval, the host-local `naatre gc` verb runs it once, tests call
/// it directly.
pub async fn gc_pass(state: &AppState) -> Result<GcReport, ApiError> {
    let now_i = state.now_ns() as i64;
    let cut = now_i.saturating_sub(state.gc_grace.as_nanos() as i64);

    // 1. Mark (read-only; primary tables only).
    let mark = {
        let now = now_i;
        state
            .read(move |conn| mark(conn, now).map_err(ApiError::log_internal))
            .await?
    };

    // The race-pin pause (test-only; ZERO in production): hold the pass
    // between mark and sweep while the test re-PUTs an old orphan and
    // references it.
    let pause = state.gc_test_delay_after_mark;
    if !pause.is_zero() {
        tokio::time::sleep(pause).await;
    }
    crash_point("gc_after_mark");

    // 2. The blob universe: DB-known rows plus everything the CAS holds
    // (an upload that crashed between the bytes and the row has no objects
    // row — it is still garbage the sweep owns).
    let universe: HashSet<Id> = {
        let rows = state
            .read(|conn| db::gc_all_object_ids(conn).map_err(ApiError::log_internal))
            .await?;
        let cas = Arc::clone(&state.cas);
        let cas_ids = state
            .blocking(move || cas.ids().map_err(ApiError::log_internal))
            .await?;
        rows.into_iter().chain(cas_ids).collect()
    };

    let mut report = GcReport::default();

    // 3. Ledger update (set-difference both directions, chunked write
    // transactions): unreachable data gets its FIRST-seen row, reachable
    // data has stale rows repaired away.
    let all_cs: Vec<Id> = state
        .read(|conn| {
            let mut out = Vec::new();
            for repo_id in db::gc_repos(conn).map_err(ApiError::log_internal)? {
                for (id, _) in
                    db::gc_all_changesets(conn, repo_id).map_err(ApiError::log_internal)?
                {
                    out.push(id);
                }
            }
            Ok(out)
        })
        .await?;
    let all_trees = state
        .read(|conn| db::gc_all_tree_ids(conn).map_err(ApiError::log_internal))
        .await?;

    let unreachable: [(&'static str, Vec<Id>); 3] = [
        (db::GC_SCOPE_CS, diff(all_cs.iter(), &mark.cs)),
        (db::GC_SCOPE_BLOB, diff(universe.iter(), &mark.blobs)),
        (db::GC_SCOPE_TREE, diff(all_trees.iter(), &mark.trees)),
    ];
    for (scope, ids) in unreachable {
        for chunk in ids.chunks(GC_BATCH_ROWS) {
            let scope = scope.to_string();
            let chunk: Vec<Id> = chunk.to_vec();
            report.candidates_new += state
                .write(move |store| {
                    db::gc_candidates_upsert(store.conn(), &scope, &chunk, now_i)
                        .map_err(ApiError::log_internal)
                })
                .await?;
        }
    }
    let reachable: [(&'static str, &HashSet<Id>); 3] = [
        (db::GC_SCOPE_CS, &mark.cs),
        (db::GC_SCOPE_BLOB, &mark.blobs),
        (db::GC_SCOPE_TREE, &mark.trees),
    ];
    for (scope, ids) in reachable {
        let ids: Vec<Id> = ids.iter().copied().collect();
        for chunk in ids.chunks(GC_BATCH_ROWS) {
            let scope = scope.to_string();
            let chunk: Vec<Id> = chunk.to_vec();
            report.candidates_repaired += state
                .write(move |store| {
                    db::gc_candidates_delete(store.conn(), &scope, &chunk)
                        .map_err(ApiError::log_internal)
                })
                .await?;
        }
    }
    crash_point("gc_after_candidates");

    // 4. Collection. Changesets first (their rows are the references the
    // blob re-verification reads), then blobs (bytes unlinked before the
    // row transaction — the orphan-direction rule), then the junctioned
    // history trees the collected changesets left behind.
    let landed: HashSet<Id> = state
        .read(|conn| {
            let mut out = HashSet::new();
            for repo_id in db::gc_repos(conn).map_err(ApiError::log_internal)? {
                landed_ids(
                    db::gc_land_result_jsons(conn, repo_id).map_err(ApiError::log_internal)?,
                    &mut out,
                );
            }
            Ok(out)
        })
        .await?;

    loop {
        crash_point("gc_mid_cs_batch");
        let (collected, raw_len) = {
            let landed = landed.clone();
            state
                .write(move |store| {
                    let conn = store.conn();
                    let tx = conn.transaction().map_err(ApiError::log_internal)?;
                    let batch = db::gc_collectible_cs(&tx, cut, GC_BATCH_ROWS)
                        .map_err(ApiError::log_internal)?;
                    let raw_len = batch.len();
                    // The lands result_json side is the in-memory filter
                    // (JSON is not statement surface): a batch entry that IS
                    // a settled land's recorded result never collects. A
                    // full batch that filters to nothing re-loops for the
                    // next page.
                    let batch: Vec<Id> = batch
                        .into_iter()
                        .filter(|id| !landed.contains(id))
                        .collect();
                    let collected = if batch.is_empty() {
                        0
                    } else {
                        let n = db::gc_collect_cs(&tx, &batch).map_err(ApiError::log_internal)?;
                        tx.commit().map_err(ApiError::log_internal)?;
                        n
                    };
                    Ok((collected, raw_len))
                })
                .await?
        };
        report.cs_collected += collected;
        if collected == 0 && raw_len < GC_BATCH_ROWS {
            break;
        }
    }

    // Blob collection is TWO-PHASE (defense in depth for the byte-deletion
    // race): phase A CONDEMNS the past-grace, unreferenced candidates
    // (`gc_pending.condemned_at` stamped, committed — the bytes stay); a
    // LATER pass's phase B deletes bytes+rows only while the mark is STILL
    // set, and any PUT's row touch clears it in the same slot — so the
    // byte-delete window is a full pass interval wide, and an upload
    // racing the delete answers a retryable 503 `blob_swept` from its
    // record step (which checks presence before writing the row; the row
    // can never name absent bytes).
    loop {
        let batch = state
            .write(move |store| {
                let conn = store.conn();
                let tx = conn.transaction().map_err(ApiError::log_internal)?;
                let batch = db::gc_collectible_blobs(&tx, cut, GC_BATCH_BLOBS)
                    .map_err(ApiError::log_internal)?;
                if batch.is_empty() {
                    return Ok(Vec::new());
                }
                let n = db::gc_condemn_blobs(&tx, &batch, now_i).map_err(ApiError::log_internal)?;
                tx.commit().map_err(ApiError::log_internal)?;
                Ok(vec![n])
            })
            .await?;
        let stamped = batch.first().copied().unwrap_or(0);
        report.blobs_condemned += stamped;
        if stamped == 0 || stamped < GC_BATCH_BLOBS {
            break;
        }
    }
    loop {
        crash_point("gc_mid_blob_batch");
        let batch = {
            let cas = Arc::clone(&state.cas);
            state
                .write(move |store| {
                    let conn = store.conn();
                    let tx = conn.transaction().map_err(ApiError::log_internal)?;
                    // Selection is the re-verification, still-condemned
                    // edition: unreferenced, past grace, and the mark still
                    // set from an EARLIER pass (`condemned_at < now` — this
                    // pass's own stamps never qualify), read in the same
                    // transaction that deletes the rows.
                    let batch = db::gc_condemned_blobs(&tx, now_i, cut, GC_BATCH_BLOBS)
                        .map_err(ApiError::log_internal)?;
                    if batch.is_empty() {
                        return Ok(Vec::new());
                    }
                    // Bytes first (blocking CAS work inside the write-slot
                    // closure — the slot is the serialization point; a crash
                    // before the commit below leaves rows claiming absent
                    // bytes for UNREACHABLE data, still condemned, and the
                    // re-run's phase B converges on them).
                    BlobStore::delete(cas.as_ref(), &batch).map_err(ApiError::log_internal)?;
                    db::gc_collect_blobs(&tx, &batch).map_err(ApiError::log_internal)?;
                    tx.commit().map_err(ApiError::log_internal)?;
                    Ok(batch)
                })
                .await?
        };
        report.blobs_collected += batch.len();
        if batch.len() < GC_BATCH_BLOBS {
            break;
        }
    }

    loop {
        let collected = state
            .write(move |store| {
                let conn = store.conn();
                let tx = conn.transaction().map_err(ApiError::log_internal)?;
                let batch = db::gc_collectible_trees(&tx, now_i, cut, GC_BATCH_ROWS)
                    .map_err(ApiError::log_internal)?;
                if batch.is_empty() {
                    return Ok(0usize);
                }
                let n = db::gc_collect_trees(&tx, &batch).map_err(ApiError::log_internal)?;
                tx.commit().map_err(ApiError::log_internal)?;
                Ok(n)
            })
            .await?;
        report.trees_collected += collected;
        if collected < GC_BATCH_ROWS {
            break;
        }
    }

    // 5. One audit row per collecting pass, actor "system" (the
    // snapshot_gc pattern; nothing collected → nothing written).
    if report.collected_any() {
        let details = json!({
            "cs": report.cs_collected,
            "blobs": report.blobs_collected,
            "trees": report.trees_collected,
            "grace_secs": state.gc_grace.as_secs(),
        })
        .to_string();
        state
            .write(move |store| {
                db::audit_append(store.conn(), now_i, "system", "gc", &details)
                    .map_err(ApiError::log_internal)
            })
            .await?;
        tracing::info!(
            cs = report.cs_collected,
            blobs = report.blobs_collected,
            trees = report.trees_collected,
            "reachability GC pass"
        );
    }
    Ok(report)
}

use naatre_store::BlobStore;

/// `a` minus `b` (into a Vec; statement chunks and the ledger calls want
/// slices, the marks are sets).
fn diff<'a>(a: impl Iterator<Item = &'a Id>, b: &HashSet<Id>) -> Vec<Id> {
    a.filter(|id| !b.contains(*id)).copied().collect()
}
