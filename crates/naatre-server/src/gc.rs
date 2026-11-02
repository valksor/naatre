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
//! Since M13a the mark walks EVERY REPO through the catalog registry
//! ([`crate::router::ReadCx`]: the catalog connection lists the repos,
//! each repo's per-repo connection serves its primary tables) and unions
//! the reachability sets in memory; the sweep then runs against the
//! catalog's ledger once. The never-dangling invariant holds across the
//! split exactly as it held within one file: every collection batch runs
//! under a COORDINATED write ([`ServerState::write_all`], the catalog slot
//! plus every repo slot with the sweep flag up) so the batch's
//! re-verification probes and its deletes stay atomic against every
//! writer — the property the single-DB write slot used to provide.
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
//! no-references inside its coordinated batch (selection probes on the
//! locked repo files, deletes on the same lock window — a batch that races
//! an upload answers the upload's record step with the retryable 503
//! `blob_swept` after the slots free; the client re-PUTs, idempotent at a
//! content-addressed URL; a row can never name absent bytes, and committed
//! history never references collected bytes). Across the split the deletes
//! are ordered repo-rows-first, catalog-ledger-last: a crash between them
//! leaves ledger rows for already-deleted data (the re-run re-verifies and
//! clears them — idempotent), never un-ledgered deletions.
//!
//! Crash-mid-GC is idempotent: the pass runs in generation-ordered,
//! slot-holding batches (the batched-backfill pattern — the slots yield
//! between batches), and the sweep is a set-difference over the
//! `gc_pending` ledger, not a destructive cursor. A crash at any point
//! (between mark and sweep, between batches, mid-batch — bytes unlinked
//! before the row transactions commit, the M5 orphan-direction rule) leaves
//! a half-swept state the re-run converges on: candidates keep their FIRST
//! `first_ns` (INSERT OR IGNORE), live data is re-marked and repaired, and
//! the collectible re-verification runs inside each batch's lock window.
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
//! drives a background or host-local pass) — committed INSIDE the final
//! collecting batch's catalog transaction (the snapshots_gc atomic pattern;
//! the tree sweep is the pass's last collecting phase, and its loop-ending
//! batch is the pass's last write). An idle pass writes nothing: a quiet
//! server must not spam the append-only log.
//!
//! Every DESTRUCTIVE batch — the changeset, blob, and tree collection
//! batches, both blob phases, and the redaction purge — runs under
//! `write_all` (the coordinated lock window): on a single-DB store that is
//! the plain single-writer slot, on a split store the catalog slot plus
//! every repo slot, so destructive writers serialize UNIFORMLY against
//! lands, uploads, and each other in both layouts.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, OnceLock};

use naatre_core::changeset;
use naatre_core::ids::Id;
use serde_json::json;

use crate::api_error::ApiError;
use crate::db;
use crate::router::ReadCx;
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
/// batch is one coordinated-lock hold, so its size bounds how long a
/// concurrent write can 503 behind the GC — the batched-backfill tradeoff.
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
/// * `gc_after_cs_repo_rows` — INSIDE a changeset batch: every repo's row
///   transaction has committed, the batch's ledger deletes have not (the
///   phantom-strand pin: the stranded ledger rows are exactly the crash
///   window the existence-only reap converges on).
/// * `gc_mid_blob_batch` — INSIDE a blob batch: the CAS bytes are already
///   unlinked, the row transactions not yet committed (the orphan-direction
///   pin: the re-run must converge over rows claiming absent bytes).
pub const GC_CRASH_POINTS: [&str; 5] = [
    "gc_after_mark",
    "gc_after_candidates",
    "gc_mid_cs_batch",
    "gc_after_cs_repo_rows",
    "gc_mid_blob_batch",
];

/// The redaction purge phase's crash points (M10b; the same closed-enum
/// pattern, listed separately from [`GC_CRASH_POINTS`] because the phases
/// need different seed shapes — the M10a matrix's condemned-blob scenario
/// never reaches them):
///
/// * `redact_mid_cs_purge` — between redaction changeset-purge batches
///   (some tombstoned changesets already purged, the rest pending).
/// * `redact_mid_blob_purge` — INSIDE a blob purge: the CAS bytes are
///   already unlinked, the row/stamp transactions not yet committed (the
///   orphan-direction pin; the re-run converges over rows claiming absent
///   bytes because the blob store's delete skips absent ids).
pub const REDACT_CRASH_POINTS: [&str; 2] = ["redact_mid_cs_purge", "redact_mid_blob_purge"];

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
    /// Tombstoned changesets purged by the redaction phase (M10b).
    pub redactions_purged_cs: usize,
    /// Tombstoned blobs purged by the redaction phase, the tombstoned
    /// changesets' exclusively-referenced blobs included (M10b).
    pub redactions_purged_blobs: usize,
}

impl GcReport {
    /// True when the pass collected anything (the audit-row condition).
    pub fn collected_any(&self) -> bool {
        self.cs_collected > 0 || self.blobs_collected > 0 || self.trees_collected > 0
    }
}

/// What one redaction-purge phase did (M10b; for logs, the CLI, and tests).
#[derive(Debug, Default, Clone, PartialEq, Eq, serde::Serialize)]
pub struct RedactionPurgeReport {
    /// Tombstoned changesets purged (rows + advisory M6 rows + ledger row;
    /// the changeset row last) with `purged_ns` stamped.
    pub cs_purged: usize,
    /// Tombstoned blobs purged (bytes + objects/membership/ledger rows),
    /// the purged changesets' exclusively-referenced blobs included, with
    /// `purged_ns` stamped on every tombstone row of each id.
    pub blobs_purged: usize,
}

impl RedactionPurgeReport {
    /// True when the phase purged anything (the audit-row condition).
    pub fn purged_any(&self) -> bool {
        self.cs_purged > 0 || self.blobs_purged > 0
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

/// Marks ONE repo's primary tables into `mark`. The per-repo half of the
/// pass: plain statement reads plus chunked, index-led BFS (the changeset
/// closure walks the `(repo_id, cs, …)` PRIMARY KEY prefix, the tree
/// closure the `(tree_id, name)` prefix) — every pass costs O(primary
/// rows), the same cost class the plan's mark clause names.
fn mark_repo(
    conn: &naatre_store::Db,
    repo_id: i64,
    now_ns: i64,
    mark: &mut Mark,
) -> Result<(), naatre_store::DbError> {
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

    // The M12b walker through conflict representations: a marked
    // format-v2 changeset's rep side/base blobs join the mark — the
    // losing sides' content lives in no tree (the root tree holds the
    // MARKER blob) and in no delta of the conflicted node (the delta
    // carries the markers, not the inputs), so the representation is
    // their only reference. The canonical probe in
    // `gc_conflict_rep_canonicals` confines the reads to v2 rows; a
    // pure-v1 store decodes nothing. (The M12a alternative — a
    // primary-table mirror of the rep ids — would be migration 013,
    // the second one-way schema lockout the M12a notes refused; the
    // conflicted rows are rare enough that decoding them in the mark
    // is the cheaper design.)
    let marked: Vec<Id> = m.cs.iter().copied().collect();
    for canonical in db::gc_conflict_rep_canonicals(conn, repo_id, &marked)? {
        let cs = changeset::decode(&canonical).map_err(|e| {
            naatre_store::DbError::from(std::io::Error::other(format!(
                "a format-v2 changeset row failed to decode: {e}"
            )))
        })?;
        for rep in cs.conflicts {
            if let Some(base) = rep.base {
                m.blobs.insert(base);
            }
            for side in rep.sides {
                m.blobs.insert(side);
            }
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
    Ok(())
}

/// Computes the whole-pass mark over the multi-repo read context at
/// `now_ns`: the catalog lists the repos, each repo's connection serves
/// its primary tables, the reachability sets union in memory.
pub(crate) fn mark(cx: &ReadCx, now_ns: i64) -> Result<Mark, ApiError> {
    let mut mark = Mark::default();
    for repo_id in db::gc_repos(cx.cat()).map_err(ApiError::log_internal)? {
        cx.with_repo(repo_id, |conn| {
            mark_repo(conn, repo_id, now_ns, &mut mark).map_err(ApiError::log_internal)
        })?;
    }
    Ok(mark)
}

/// The settled lands' recorded `landed` ids, unioned across every repo
/// (read side; the in-memory filter the sweeps honor — JSON is not
/// statement surface).
async fn landed_everywhere(state: &AppState) -> Result<HashSet<Id>, ApiError> {
    state
        .read_all(|cx| {
            let mut landed = HashSet::new();
            for repo_id in db::gc_repos(cx.cat()).map_err(ApiError::log_internal)? {
                cx.with_repo(repo_id, |conn| {
                    landed_ids(
                        db::gc_land_result_jsons(conn, repo_id).map_err(ApiError::log_internal)?,
                        &mut landed,
                    );
                    Ok(())
                })
                .map_err(ApiError::log_internal)?;
            }
            Ok(landed)
        })
        .await
}

/// The exclusively-referenced followup of a purge batch, split-aware: the
/// ids among `blobs` with no reference left in ANY repo
/// ([`db::gc_blob_referenced_among`] per repo connection) and no upload
/// touch inside the grace window ([`db::blobs_touched_after`] on the
/// catalog; an imported blob carries no `objects` row and no touch, and
/// still qualifies). Runs inside the coordinated batch's lock window —
/// the probes and the deletes that follow are atomic against every
/// writer, the property the single-DB version got from the shared
/// transaction.
fn exclusively_unreferenced(
    cx: &mut crate::router::AllCx<'_>,
    blobs: &[Id],
    cut_ns: i64,
) -> Result<Vec<Id>, ApiError> {
    let touched: HashSet<Id> = db::blobs_touched_after(cx.cat(), blobs, cut_ns)
        .map_err(ApiError::log_internal)?
        .into_iter()
        .collect();
    let mut referenced: HashSet<Id> = HashSet::new();
    // Single mode: the one connection IS every repo — one whole-DB probe.
    if cx.is_single() {
        referenced
            .extend(db::gc_blob_referenced_among(cx.cat(), blobs).map_err(ApiError::log_internal)?);
    } else {
        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
            let conn = cx.repo(repo_id)?;
            referenced
                .extend(db::gc_blob_referenced_among(conn, blobs).map_err(ApiError::log_internal)?);
        }
    }
    Ok(blobs
        .iter()
        .filter(|id| !touched.contains(id) && !referenced.contains(*id))
        .copied()
        .collect())
}

/// Runs one full GC pass against `state` (its clock, its blob store, its
/// routing layer). Callers gate on `state.gc_enabled`: `serve` runs it
/// on an interval, the host-local `naatre gc` verb runs it once, tests call
/// it directly.
pub async fn gc_pass(state: &AppState) -> Result<GcReport, ApiError> {
    let now_i = state.now_ns() as i64;
    let cut = now_i.saturating_sub(state.gc_grace.as_nanos() as i64);

    // 1. Mark (read-only; primary tables only, every repo via the
    //    registry).
    let mark = {
        let now = now_i;
        state
            .read_all(move |cx| mark(cx, now).map_err(ApiError::log_internal))
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

    // 2. The blob universe: catalog-known rows plus everything the CAS
    //    holds (an upload that crashed between the bytes and the row has no
    //    objects row — it is still garbage the sweep owns).
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
    //    transactions on the catalog): unreachable data gets its FIRST-seen
    //    row, reachable data has stale rows repaired away.
    let all_cs: Vec<Id> = state
        .read_all(|cx| {
            let mut out = Vec::new();
            for repo_id in db::gc_repos(cx.cat()).map_err(ApiError::log_internal)? {
                cx.with_repo(repo_id, |conn| {
                    for (id, _) in
                        db::gc_all_changesets(conn, repo_id).map_err(ApiError::log_internal)?
                    {
                        out.push(id);
                    }
                    Ok(())
                })
                .map_err(ApiError::log_internal)?;
            }
            Ok(out)
        })
        .await?;
    let all_trees: Vec<Id> = state
        .read_all(|cx| {
            let mut out = Vec::new();
            for repo_id in db::gc_repos(cx.cat()).map_err(ApiError::log_internal)? {
                cx.with_repo(repo_id, |conn| {
                    out.extend(db::gc_all_tree_ids(conn).map_err(ApiError::log_internal)?);
                    Ok(())
                })
                .map_err(ApiError::log_internal)?;
            }
            Ok(out)
        })
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
    //    blob re-verification reads), then blobs (bytes unlinked before the
    //    row transactions — the orphan-direction rule), then the junctioned
    //    history trees the collected changesets left behind. Every batch
    //    runs under the coordinated lock: selection probes and deletes are
    //    atomic against every writer in both layouts. The mark's walker
    //    already carried every conflict representation's side/base blobs
    //    into the reachable set (M12b — the M12a mechanical hold is gone;
    //    tombstoned blobs purge in the redaction phase below exactly as
    //    before).
    {
        let landed: HashSet<Id> = landed_everywhere(state).await?;

        loop {
            crash_point("gc_mid_cs_batch");
            let (collected, raw_len) = {
                let landed = landed.clone();
                state
                    .write_all(move |cx| {
                        // The candidate page (catalog ledger, past grace).
                        let candidates = db::gc_cs_candidates(cx.cat(), cut, GC_BATCH_ROWS)
                            .map_err(ApiError::log_internal)?;
                        let raw_len = candidates.len();
                        if candidates.is_empty() {
                            return Ok((0usize, raw_len));
                        }
                        // The per-repo probes: existence + reference
                        // + tombstone exclusion, with each repo's
                        // generation. A changeset id can only be
                        // referenced within its own repo, so the probes
                        // are complete per repo.
                        let mut collectible: HashMap<i64, Vec<(Id, i64)>> = HashMap::new();
                        let mut present: HashSet<Id> = HashSet::new();
                        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                            let conn = cx.repo(repo_id)?;
                            let rows = db::gc_cs_collectible_probe(conn, repo_id, &candidates)
                                .map_err(ApiError::log_internal)?;
                            present.extend(
                                db::gc_cs_present_among(conn, repo_id, &candidates)
                                    .map_err(ApiError::log_internal)?,
                            );
                            collectible.insert(repo_id, rows);
                        }
                        // The PHANTOM reap: candidates no repo's
                        // changesets table contains. A crash between a
                        // batch's repo deletes and its ledger deletes
                        // strands ledger rows the collectible probe (which
                        // requires the changeset row) never re-enters — at
                        // a full phantom page the `first_ns ASC` head would
                        // stall all cs collection forever. Deleting them
                        // here converges the orphans on THIS pass.
                        let phantoms: Vec<Id> = candidates
                            .iter()
                            .filter(|id| !present.contains(*id))
                            .copied()
                            .collect();
                        if !phantoms.is_empty() {
                            db::gc_candidates_delete(cx.cat(), db::GC_SCOPE_CS, &phantoms)
                                .map_err(ApiError::log_internal)?;
                        }
                        // Generation-ordered like the batched backfill
                        // (children collect after parents' chains,
                        // deterministic across crash re-runs), then the
                        // lands filter (the in-memory side — JSON is not
                        // statement surface; a batch entry that IS a
                        // settled land's recorded result never collects).
                        let mut merged: Vec<(Id, i64)> = collectible
                            .values()
                            .flat_map(|rows| rows.iter().cloned())
                            .collect();
                        merged.sort_by(|a, b| a.1.cmp(&b.1).then(a.0.0.cmp(&b.0.0)));
                        let batch: Vec<Id> = merged
                            .into_iter()
                            .map(|(id, _)| id)
                            .filter(|id| !landed.contains(id))
                            .take(GC_BATCH_ROWS)
                            .collect();
                        if batch.is_empty() {
                            return Ok((0usize, raw_len));
                        }
                        // Repo rows first (each repo's file in its own
                        // transaction; the batch is partitioned by the
                        // probes — a batch id came from exactly one repo),
                        // then the catalog ledger rows.
                        let batch_set: HashSet<Id> = batch.iter().copied().collect();
                        let mut collected = 0usize;
                        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                            let mine: Vec<Id> = collectible
                                .get(&repo_id)
                                .map(|rows| {
                                    rows.iter()
                                        .map(|(id, _)| *id)
                                        .filter(|id| batch_set.contains(id))
                                        .collect()
                                })
                                .unwrap_or_default();
                            if mine.is_empty() {
                                continue;
                            }
                            let conn = cx.repo(repo_id)?;
                            let tx = conn
                                .transaction_immediate()
                                .map_err(ApiError::log_internal)?;
                            let n = db::gc_collect_cs_repo(&tx, &mine)
                                .map_err(ApiError::log_internal)?;
                            tx.commit().map_err(ApiError::log_internal)?;
                            collected += n;
                        }
                        // The crash point sits between the repo deletes
                        // and the ledger deletes: a crash here strands
                        // ledger rows whose data is already gone (the
                        // phantom-reap pin).
                        crash_point("gc_after_cs_repo_rows");
                        db::gc_candidates_delete(cx.cat(), db::GC_SCOPE_CS, &batch)
                            .map_err(ApiError::log_internal)?;
                        Ok((collected, raw_len))
                    })
                    .await?
            };
            report.cs_collected += collected;
            if collected == 0 {
                // Loop guard (the invariant: selection is deterministic and
                // NOTHING was deleted). A zero-collect batch means every
                // probe found a reference or a settled land — or the page is
                // simply exhausted. Either way the next iteration's
                // selection returns the SAME rows (the deletions that
                // advance a paging cursor did not happen), so re-looping
                // would spin forever. Defer the residue to the next pass;
                // nothing collectible is skipped that that pass would not
                // re-select.
                break;
            }
            if raw_len < GC_BATCH_ROWS {
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
            let stamped = state
                .write_all(move |cx| {
                    let candidates = db::gc_blob_candidates(cx.cat(), cut, GC_BATCH_BLOBS)
                        .map_err(ApiError::log_internal)?;
                    if candidates.is_empty() {
                        return Ok(0usize);
                    }
                    // Per-repo reference + tombstone probes (the batch's
                    // re-verification, inside the lock window).
                    let batch = subtract_blob_probes(cx, &candidates)?;
                    if batch.is_empty() {
                        return Ok(0usize);
                    }
                    let n = db::gc_condemn_blobs(cx.cat(), &batch, now_i)
                        .map_err(ApiError::log_internal)?;
                    Ok(n)
                })
                .await?;
            report.blobs_condemned += stamped;
            if stamped == 0 || stamped < GC_BATCH_BLOBS {
                break;
            }
        }
        loop {
            let batch = {
                let cas = Arc::clone(&state.cas);
                state
                    .write_all(move |cx| {
                        let candidates =
                            db::gc_condemned_blob_candidates(cx.cat(), now_i, cut, GC_BATCH_BLOBS)
                                .map_err(ApiError::log_internal)?;
                        if candidates.is_empty() {
                            return Ok(Vec::new());
                        }
                        let batch = subtract_blob_probes(cx, &candidates)?;
                        if batch.is_empty() {
                            return Ok(Vec::new());
                        }
                        // Bytes first (blocking CAS work inside the
                        // coordinated lock window — the slots are the
                        // serialization point; a crash before the row
                        // transactions below leaves rows claiming absent
                        // bytes for UNREACHABLE data, still condemned, and
                        // the re-run's phase B converges on them).
                        BlobStore::delete(cas.as_ref(), &batch).map_err(ApiError::log_internal)?;
                        // The mid-batch crash point fires HERE — bytes
                        // unlinked, row transactions not yet committed (the
                        // doc claim; the hook lives inside the batch so the
                        // pin exercises the orphan-direction state it
                        // describes).
                        crash_point("gc_mid_blob_batch");
                        // Repo membership rows first, then the catalog
                        // rows (objects + ledger) in ONE catalog
                        // transaction (the same atomic shape as every
                        // other call site — not two autocommitted deletes).
                        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                            let conn = cx.repo(repo_id)?;
                            let tx = conn
                                .transaction_immediate()
                                .map_err(ApiError::log_internal)?;
                            db::gc_collect_blobs_repo(&tx, &batch)
                                .map_err(ApiError::log_internal)?;
                            tx.commit().map_err(ApiError::log_internal)?;
                        }
                        let tx = cx
                            .cat()
                            .transaction_immediate()
                            .map_err(ApiError::log_internal)?;
                        db::gc_collect_blobs_catalog(&tx, &batch)
                            .map_err(ApiError::log_internal)?;
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

        // 5. The tree sweep is the pass's last REACHABILITY-collecting phase,
        //    so its loop-ending batch carries the pass's `gc` audit row (the
        //    snapshots_gc atomic pattern: one audit row per collecting pass,
        //    actor "system", committed with the ledger deletes it reports).
        //    Nothing collected → no row — a quiet server must not spam the
        //    append-only log. A crash between an earlier collecting batch and
        //    this one loses the audit row for the already-committed deletes
        //    (the deletes stand; the next collecting pass writes its own
        //    row). The REDACTION purge phase (M10b) follows as its own phase
        //    with its own `redact_purge` audit row.
        loop {
            let collected = {
                let cs = report.cs_collected;
                let blobs = report.blobs_collected;
                let trees_so_far = report.trees_collected;
                let grace_secs = state.gc_grace.as_secs();
                state
                    .write_all(move |cx| {
                        // The candidate page may be EMPTY here — an empty
                        // loop-ending batch still carries the pass's audit
                        // row (the original shape: the audit condition is
                        // `cs + blobs + trees_so_far + batch.len() > 0`,
                        // satisfied by the earlier phases' collections).
                        let candidates = db::gc_tree_candidates(cx.cat(), cut, GC_BATCH_ROWS)
                            .map_err(ApiError::log_internal)?;
                        // The per-repo protective-closure probe: which
                        // candidates ANY repo's current closure reaches.
                        // Single mode: ONE whole-DB call (every repo
                        // routes to the same connection — the per-repo
                        // loop would be R identical queries).
                        let mut protected: HashSet<Id> = HashSet::new();
                        if !candidates.is_empty() {
                            if cx.is_single() {
                                protected.extend(
                                    db::gc_tree_protected_among(cx.cat(), &candidates, now_i)
                                        .map_err(ApiError::log_internal)?,
                                );
                            } else {
                                for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                                    let conn = cx.repo(repo_id)?;
                                    protected.extend(
                                        db::gc_tree_protected_among(conn, &candidates, now_i)
                                            .map_err(ApiError::log_internal)?,
                                    );
                                }
                            }
                        }
                        let batch: Vec<Id> = candidates
                            .into_iter()
                            .filter(|id| !protected.contains(id))
                            .take(GC_BATCH_ROWS)
                            .collect();
                        // Repo rows first (each repo's file in its own
                        // transaction — a tree id shared across repos dies
                        // from every file that holds it), then the catalog
                        // ledger; the loop-ending batch's audit row commits
                        // with the ledger deletes.
                        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                            if batch.is_empty() {
                                break;
                            }
                            let conn = cx.repo(repo_id)?;
                            let tx = conn
                                .transaction_immediate()
                                .map_err(ApiError::log_internal)?;
                            db::gc_collect_trees_repo(&tx, &batch)
                                .map_err(ApiError::log_internal)?;
                            tx.commit().map_err(ApiError::log_internal)?;
                        }
                        let tx = cx
                            .cat()
                            .transaction_immediate()
                            .map_err(ApiError::log_internal)?;
                        db::gc_candidates_delete(&tx, db::GC_SCOPE_TREE, &batch)
                            .map_err(ApiError::log_internal)?;
                        // The loop-ENDING batch carries the pass's audit
                        // row (`batch.len() < GC_BATCH_ROWS` is the loop's
                        // break condition): a full batch continues the loop,
                        // and every batch writing its own row would row the
                        // pass once per batch — ONE row per pass.
                        if cs + blobs + trees_so_far + batch.len() > 0
                            && batch.len() < GC_BATCH_ROWS
                        {
                            let details = json!({
                                "cs": cs,
                                "blobs": blobs,
                                "trees": trees_so_far + batch.len(),
                                "grace_secs": grace_secs,
                            })
                            .to_string();
                            db::audit_append(&tx, now_i, "system", "gc", &details)
                                .map_err(ApiError::log_internal)?;
                        }
                        tx.commit().map_err(ApiError::log_internal)?;
                        Ok(batch.len())
                    })
                    .await?
            };
            report.trees_collected += collected;
            if collected < GC_BATCH_ROWS {
                break;
            }
        }
    }

    if report.collected_any() {
        tracing::info!(
            cs = report.cs_collected,
            blobs = report.blobs_collected,
            trees = report.trees_collected,
            "reachability GC pass"
        );
    }

    // 6. The redaction purge phase (M10b): reclaim the bytes/rows of every
    //    actively-tombstoned object the gates have long stopped serving.
    //    Runs AFTER the reachability sweep (the sweep's tree `live` set is
    //    computed from the surviving changeset roots; a purged tombstone's
    //    trees become sweepable the NEXT pass — the same one-pass
    //    convergence lag the blob phases already have).
    let redact = redaction_purge_pass(state).await?;
    report.redactions_purged_cs = redact.cs_purged;
    report.redactions_purged_blobs = redact.blobs_purged;

    Ok(report)
}

/// The blob batch's per-repo re-verification (both blob phases): the
/// candidates still unreferenced (no `file_changes` row, no blob
/// `tree_entries` row, in ANY repo) and not actively tombstoned (the
/// redaction purge phase owns those — any repo's tombstone). Runs inside
/// the coordinated batch's lock window.
fn subtract_blob_probes(
    cx: &crate::router::AllCx<'_>,
    candidates: &[Id],
) -> Result<Vec<Id>, ApiError> {
    let mut spared: HashSet<Id> = HashSet::new();
    // Single mode: one connection, whole-DB probes (the per-repo loop
    // would run one identical query per registry id).
    if cx.is_single() {
        let conn = cx.cat();
        spared.extend(
            db::gc_blob_referenced_among(conn, candidates).map_err(ApiError::log_internal)?,
        );
        spared.extend(
            db::gc_tombstoned_among(conn, None, "blob", candidates)
                .map_err(ApiError::log_internal)?,
        );
    } else {
        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
            let conn = cx.repo(repo_id)?;
            spared.extend(
                db::gc_blob_referenced_among(conn, candidates).map_err(ApiError::log_internal)?,
            );
            spared.extend(
                db::gc_tombstoned_among(conn, Some(repo_id), "blob", candidates)
                    .map_err(ApiError::log_internal)?,
            );
        }
    }
    Ok(candidates
        .iter()
        .filter(|id| !spared.contains(*id))
        .copied()
        .collect())
}

/// The redaction purge phase (M10b): reclaim the bytes and rows behind every
/// ACTIVE tombstone and stamp `redactions.purged_ns`. Tombstoned-but-
/// unpurged is a normal state — the universal gate stopped serving the
/// object at mark time; this phase only reclaims space, so it needs NO grace
/// window of its own: selection re-verifies inside each coordinated batch
/// (the slots serialize it against every referencing write), the blob probes
/// honor the same upload touch grace as the reachability sweep (a client
/// between a blob PUT and its referencing create is never overtaken; a
/// record step racing the delete answers the retryable 503 `blob_swept`),
/// and a crash mid-purge converges on the re-run (bytes first — the blob
/// store's delete skips absent ids — then rows+stamps; the changeset batch
/// re-selects the same rows). A tombstoned changeset is purgeable ONLY when
/// nothing references it (no ref, lands, git_map, or child): a tombstone
/// under a serving ref or with children stays until history moves past it.
/// Never dangles across repos: a blob another repo still references is
/// spared by the same probe set the reachability sweep uses (the
/// shared-subtree pin).
///
/// One `redact_purge` audit row per purging pass, actor `"system"` (a
/// background or host-local pass drives it, never a principal) — committed
/// WITH the loop-ending batch's catalog deletes (the gc-audit pattern: the
/// row and the work it reports commit atomically; a crash between an earlier
/// batch and the loop-ending one loses the row for the already-committed
/// deletes, and the next purging pass writes its own). Counts stay in
/// their units: changeset batches feed `cs`, exclusive-blob reclaims and
/// blob batches feed `blobs`. An idle phase (no active tombstones) never
/// takes the slots and never rows anything.
pub async fn redaction_purge_pass(state: &AppState) -> Result<RedactionPurgeReport, ApiError> {
    let now_i = state.now_ns() as i64;
    let cut = now_i.saturating_sub(state.gc_grace.as_nanos() as i64);

    // Read-probe first: a quiet tombstone table never occupies the slots
    // (the derive-queue quiet-pass pattern).
    let pending = state
        .read_all(|cx| {
            let mut any = false;
            for repo_id in db::gc_repos(cx.cat()).map_err(ApiError::log_internal)? {
                cx.with_repo(repo_id, |conn| {
                    if db::redactions_active_exist_repo(conn).map_err(ApiError::log_internal)? {
                        any = true;
                    }
                    Ok(())
                })
                .map_err(ApiError::log_internal)?;
            }
            Ok(any)
        })
        .await?;
    if !pending {
        return Ok(RedactionPurgeReport::default());
    }

    // The settled lands' recorded results (the in-memory filter — JSON is
    // not statement surface; a landed id is main's tip and thereby also
    // under a ref, but the filter is kept for the clause's letter, the same
    // as the reachability sweep's).
    let landed: HashSet<Id> = landed_everywhere(state).await?;

    let mut report = RedactionPurgeReport::default();

    // Phase 1: tombstoned CHANGESETS — rows first (their file_changes rows
    // are the references the blob probes read), the changeset row last, and
    // the batch's exclusively-referenced blobs via the blob rule in the same
    // batch (an erasure act must not wait a grace window to make the
    // content vanish). Counts stay in their units: the batch's changeset
    // count feeds `cs_purged`, the exclusive blobs' count `blobs_purged`
    // (the M10b closure defect: the blob count used to inflate the
    // changeset unit).
    loop {
        crash_point("redact_mid_cs_purge");
        let (cs_purged, blob_purged) = {
            let landed = landed.clone();
            let cas = Arc::clone(&state.cas);
            state
                .write_all(move |cx| {
                    // The per-repo purgeable selection (every probed table
                    // lives in the tombstone's own file).
                    let mut purgeable: HashMap<i64, Vec<Id>> = HashMap::new();
                    let mut total = 0usize;
                    for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                        let conn = cx.repo(repo_id)?;
                        let rows = db::redact_purgeable_cs_repo(conn, repo_id, GC_BATCH_ROWS)
                            .map_err(ApiError::log_internal)?;
                        total += rows.len();
                        purgeable.insert(repo_id, rows);
                    }
                    if total == 0 {
                        return Ok((0usize, 0usize));
                    }
                    // The lands filter (in-memory).
                    for rows in purgeable.values_mut() {
                        rows.retain(|id| !landed.contains(id));
                    }
                    let total: usize = purgeable.values().map(|rows| rows.len()).sum();
                    if total == 0 {
                        return Ok((0usize, 0usize));
                    }
                    // The blobs the batches' file_changes carry, read
                    // BEFORE the rows go.
                    let mut blobs: Vec<Id> = Vec::new();
                    for (repo_id, rows) in &purgeable {
                        if rows.is_empty() {
                            continue;
                        }
                        let conn = cx.repo(*repo_id)?;
                        blobs.extend(
                            db::file_change_blobs_of_cs(conn, rows)
                                .map_err(ApiError::log_internal)?,
                        );
                    }
                    blobs.sort_by_key(|id| id.0);
                    blobs.dedup();
                    // Repo rows first (each repo's file in its own
                    // transaction), stamps on every tombstone-holding file.
                    let mut all_purged: Vec<Id> = Vec::new();
                    for (repo_id, rows) in &purgeable {
                        if rows.is_empty() {
                            continue;
                        }
                        let conn = cx.repo(*repo_id)?;
                        let tx = conn
                            .transaction_immediate()
                            .map_err(ApiError::log_internal)?;
                        db::gc_collect_cs_repo(&tx, rows).map_err(ApiError::log_internal)?;
                        for id in rows {
                            db::redaction_stamp_purged(&tx, "changeset", id, now_i)
                                .map_err(ApiError::log_internal)?;
                        }
                        tx.commit().map_err(ApiError::log_internal)?;
                        all_purged.extend(rows.iter().copied());
                    }
                    // The purged changesets' ledger rows (catalog side).
                    db::gc_candidates_delete(cx.cat(), db::GC_SCOPE_CS, &all_purged)
                        .map_err(ApiError::log_internal)?;
                    // The exclusively-referenced ones: no reference left
                    // anywhere and no upload touch inside the grace window.
                    let exclusive = exclusively_unreferenced(cx, &blobs, cut)?;
                    if !exclusive.is_empty() {
                        BlobStore::delete(cas.as_ref(), &exclusive)
                            .map_err(ApiError::log_internal)?;
                        crash_point("redact_mid_blob_purge");
                        // The stamps cover EVERY repo file (the loop runs
                        // all of them, not only the purging repos): a blob
                        // tombstoned in several repos carries one row per
                        // repo, and leaving any row active would re-select
                        // the blob in phase 2 for a double count (the
                        // cross-repo pin). Repo membership rows die in the
                        // same transactions (advisory rows — no reference
                        // remains anywhere by the probe above).
                        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                            let conn = cx.repo(repo_id)?;
                            let tx = conn
                                .transaction_immediate()
                                .map_err(ApiError::log_internal)?;
                            db::gc_collect_blobs_repo(&tx, &exclusive)
                                .map_err(ApiError::log_internal)?;
                            for id in &exclusive {
                                db::redaction_stamp_purged(&tx, "blob", id, now_i)
                                    .map_err(ApiError::log_internal)?;
                            }
                            tx.commit().map_err(ApiError::log_internal)?;
                        }
                        let tx = cx
                            .cat()
                            .transaction_immediate()
                            .map_err(ApiError::log_internal)?;
                        db::gc_collect_blobs_catalog(&tx, &exclusive)
                            .map_err(ApiError::log_internal)?;
                        tx.commit().map_err(ApiError::log_internal)?;
                    }
                    Ok((total, exclusive.len()))
                })
                .await?
        };
        report.cs_purged += cs_purged;
        report.blobs_purged += blob_purged;
        if cs_purged == 0 {
            // Loop guard (the invariant: selection is deterministic and
            // NOTHING was purged — every entry was a settled land's recorded
            // result, or the work list drained). Defer the residue to the
            // next pass.
            break;
        }
        if cs_purged < GC_BATCH_ROWS {
            break;
        }
    }

    // Phase 2: tombstoned BLOBS (bytes first, the orphan-direction rule; the
    // stamp covers every repo whose tombstone names the id). The loop-ending
    // batch carries the pass's `redact_purge` audit row (the gc-audit
    // pattern: one audit row per purging pass, committed with the catalog
    // deletes it reports — the post-commit audit write this replaces could
    // lose the row to a crash between the purge and the audit). A crash
    // between an earlier batch and the loop-ending one loses the row for the
    // already-committed deletes; the next purging pass writes its own.
    loop {
        let purged = {
            let cs_so_far = report.cs_purged;
            let blobs_so_far = report.blobs_purged;
            let cas = Arc::clone(&state.cas);
            state
                .write_all(move |cx| {
                    // The work list: every repo's active blob tombstones,
                    // merged oldest-mark-first. An EMPTY list still falls
                    // through: the loop-ending catalog transaction carries
                    // the pass's audit row when phase 1 purged (the
                    // original's no-early-return shape).
                    let mut work: Vec<(Id, i64)> = Vec::new();
                    for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                        let conn = cx.repo(repo_id)?;
                        work.extend(
                            db::redact_blob_tombstones_repo(conn, repo_id, GC_BATCH_BLOBS)
                                .map_err(ApiError::log_internal)?,
                        );
                    }
                    work.sort_by(|a, b| a.1.cmp(&b.1).then(a.0.0.cmp(&b.0.0)));
                    work.truncate(GC_BATCH_BLOBS);
                    // One row per DISTINCT id: a blob tombstoned in several
                    // repos contributes one (id, oldest-mark) row per repo —
                    // the merge dedups (the original's single-table GROUP BY
                    // answered one row per id).
                    let mut seen = HashSet::new();
                    work.retain(|(id, _)| seen.insert(*id));
                    let batch: Vec<Id> = work.iter().map(|(id, _)| *id).collect();
                    // Re-verification inside the lock window: no REFERENCE
                    // in any repo (the tombstones themselves are the work
                    // list — never an exclusion here, unlike the reachability
                    // sweep's subtract) and no in-window upload touch on the
                    // catalog. Single mode: one whole-DB probe.
                    let mut referenced: HashSet<Id> = HashSet::new();
                    if cx.is_single() {
                        referenced.extend(
                            db::gc_blob_referenced_among(cx.cat(), &batch)
                                .map_err(ApiError::log_internal)?,
                        );
                    } else {
                        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                            let conn = cx.repo(repo_id)?;
                            referenced.extend(
                                db::gc_blob_referenced_among(conn, &batch)
                                    .map_err(ApiError::log_internal)?,
                            );
                        }
                    }
                    let batch: Vec<Id> = batch
                        .into_iter()
                        .filter(|id| !referenced.contains(id))
                        .collect();
                    let touched: HashSet<Id> = db::blobs_touched_after(cx.cat(), &batch, cut)
                        .map_err(ApiError::log_internal)?
                        .into_iter()
                        .collect();
                    let batch: Vec<Id> = batch
                        .into_iter()
                        .filter(|id| !touched.contains(id))
                        .collect();
                    if !batch.is_empty() {
                        BlobStore::delete(cas.as_ref(), &batch).map_err(ApiError::log_internal)?;
                        crash_point("redact_mid_blob_purge");
                        // Repo membership rows + tombstone stamps first,
                        // then the catalog rows (objects + ledger); the
                        // loop-ending batch's audit row commits with the
                        // catalog deletes.
                        for repo_id in cx.repo_ids().collect::<Vec<_>>() {
                            let conn = cx.repo(repo_id)?;
                            let tx = conn
                                .transaction_immediate()
                                .map_err(ApiError::log_internal)?;
                            db::gc_collect_blobs_repo(&tx, &batch)
                                .map_err(ApiError::log_internal)?;
                            for id in &batch {
                                db::redaction_stamp_purged(&tx, "blob", id, now_i)
                                    .map_err(ApiError::log_internal)?;
                            }
                            tx.commit().map_err(ApiError::log_internal)?;
                        }
                    }
                    let tx = cx
                        .cat()
                        .transaction_immediate()
                        .map_err(ApiError::log_internal)?;
                    db::gc_collect_blobs_catalog(&tx, &batch).map_err(ApiError::log_internal)?;
                    if batch.len() < GC_BATCH_BLOBS && cs_so_far + blobs_so_far + batch.len() > 0 {
                        let details = serde_json::json!({
                            "cs": cs_so_far,
                            "blobs": blobs_so_far + batch.len(),
                        })
                        .to_string();
                        db::audit_append(&tx, now_i, "system", "redact_purge", &details)
                            .map_err(ApiError::log_internal)?;
                    }
                    tx.commit().map_err(ApiError::log_internal)?;
                    Ok(batch.len())
                })
                .await?
        };
        report.blobs_purged += purged;
        if purged < GC_BATCH_BLOBS {
            break;
        }
    }

    if report.purged_any() {
        tracing::info!(
            cs = report.cs_purged,
            blobs = report.blobs_purged,
            "redaction purge pass"
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
