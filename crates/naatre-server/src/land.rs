//! Server-side rebase + CAS of `main` (M4a).
//!
//! The whole land runs inside ONE write transaction (`BEGIN IMMEDIATE` via
//! [`TxnGuard`], so an early return or panic rolls everything back — a
//! crashed land leaves zero rows and the idempotency key unsettled;
//! replaying the key after a restart returns the settled state). Durability
//! invariant (1) holds because merged blob contents are `Cas::put` (fsync
//! before return) before any row that references them can commit; the
//! `repo_blobs` rows land in the same transaction.
//!
//! Rebase (per changeset of head's first-parent chain back to the merge
//! base with `main`, oldest first): a path-level 3-way with base = the
//! node's ORIGINAL parent tree, ours = the evolving rebased side (the
//! previous rebased node's tree, initially `main`'s), theirs = the node's
//! own tree. The rebased changeset records the delta from the rebased
//! parent's tree to the merged tree, keeps the original author, message,
//! and author timestamp, takes the landing principal, and bumps generation.
//!
//! In-flight same-key handling (plan "Land algorithm") lives at the route
//! layer ([`crate::land_join`]): a concurrent request on the same
//! (repo, principal, key) JOINS the in-flight land and shares its outcome
//! instead of contending for the write slot. Any land that does reach this
//! module still runs alone — with the single-writer slot
//! ([`crate::state::ServerState::write`] `try_lock`) a second transaction
//! cannot start while one is in flight. A committed `pending` row is
//! therefore unobservable: the only writer that could create one crashes
//! out with a full rollback. The `pending` lookup below is kept as a
//! defense-in-depth retryable 503, not a reachable state.
//!
//! Deterministic crash points (`NAATRE_CRASH_POINT`, debug builds only, a
//! closed enum): `after_pending_row`, `after_blob_write`, `after_rebase`,
//! `after_cas_main`, `after_commit` (end of transaction, just before the
//! response), and `after_conflict_rollback` (after a conflict rolled
//! back). M4b's crash-matrix runner walks this enum.
//!
//! First-class conflicts (M12): the default land keeps the M4 contract —
//! any path conflict rolls everything back and answers the structured 409
//! with the resolution recipe. The explicit `--record-conflict` opt-in
//! (`LandCommand::record_conflict`) instead materializes every
//! representable conflict's markers into the rebased node: the node is a
//! format-v2 changeset (its payload carries the [`naatre_core::conflict`]
//! representations, its id hashes under the `naatre\0v2\0` domain, its
//! `root_tree_id` is the markers-materialized tree), and the land settles
//! ONCE with `lands.state = 'conflicted'` — the conflicted node IS the
//! settlement. Rebasing a chain that contains conflicted nodes succeeds:
//! they rebase like any node and their representations travel
//! ([`naatre_core::conflict::carry`]) — a path that re-conflicts under
//! `--record-conflict` takes the new sides (the conflict composes).

use std::sync::OnceLock;

use naatre_store::Db;

use naatre_core::changeset::{self, Changeset, Op};
use naatre_core::conflict::{self, ConflictRep};
use naatre_core::error::CoreError;
use naatre_core::ids::Id;

use naatre_store::BlobStore;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::authz;
use crate::db;
use crate::merge::{PathConflict, content_rep, delta_between, merge_trees, merge_trees_recording};
use crate::router::RepoCx;
use crate::tree_derive::{DeriveError, MemNode};

/// The closed crash-point enum (checked against `NAATRE_CRASH_POINT`).
pub const CRASH_POINTS: [&str; 7] = [
    "after_pending_row",
    "after_blob_write",
    "after_rebase",
    "after_cas_main",
    "before_commit",
    "after_commit",
    "after_conflict_rollback",
];

/// Panics when `NAATRE_CRASH_POINT` (read once per process) names this
/// call site. Debug builds only — the release build compiles it to a
/// no-op, so the enum is compiled out of release (plan pin).
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

/// The resolution recipe carried in every land-conflict 409 `details`
/// (plan "Merge"; `naatre land` prints it verbatim). It LEADS with the
/// CLI flag (M12b: the flag exists) and names the API request field for
/// non-CLI clients.
pub const RESOLUTION_RECIPE: &str = "to record the conflict on main as a conflicted changeset, \
re-land with the same key and `naatre land --record-conflict` (the API request \
field \"record_conflict\": true for non-CLI clients) — or resolve instead: \
materialize the conflicted main (naatre sync), edit the resolved content, \
and `naatre resolve <path>` (or fetch the conflict blobs \
GET /v1/repos/{r}/objects/{id} and build a resolution changeset on head's \
parent), then re-land with the same key";

/// The recipe variant for the refused-under-opt-in case: the rebase hit a
/// conflict the v2 representation cannot record (a deletion side has no
/// blob id to carry), so `record_conflict: true` cannot answer this 409 —
/// the recipe names the conflict class and advises only the resolution
/// path, never the flag just used.
pub const RESOLUTION_RECIPE_REFUSED: &str = "this conflict cannot be recorded: a deletion side \
has no blob id to carry in the conflict representation — resolve it instead: \
materialize the conflicted main (naatre sync), edit the resolved content, \
and `naatre resolve <path>` (or fetch the conflict blobs \
GET /v1/repos/{r}/objects/{id} and build a resolution changeset on head's \
parent), then re-land with the same key \
(record_conflict: true does not apply here)";

/// One land request, resolved by the route (role + hex checks done). The
/// repo id resolves INSIDE the write closure: any store touch before
/// `state.write` would block on the mutex instead of answering 503, which
/// would make the single-writer backpressure unobservable on this route.
pub(crate) struct LandCommand {
    pub repo_name: String,
    pub principal: String,
    /// Segment-aligned token scope (`None` = whole repo).
    pub scope: Option<String>,
    pub head: Id,
    pub key: String,
    /// Advisory base hint from the request body (ignored for the rebase
    /// itself; a mismatch with head's parent becomes a response note).
    pub base_hint: Option<Id>,
    /// M12: record conflicts on main as a conflicted (format-v2) changeset
    /// instead of refusing with the M4 409. The land still settles once;
    /// `lands.state = 'conflicted'`.
    pub record_conflict: bool,
    pub now_ns: i64,
    /// The state's injectable wall clock, carried for the eager-derive
    /// path's wall-clock deadline ([`crate::derive::DERIVE_WALK_DEADLINE_NS`]).
    pub clock: crate::state::ClockFn,
    /// The state's injectable deadline WINDOW (ns) for the same path — an
    /// overrun is a retryable failed attempt, not a structural quarantine.
    pub walk_deadline: u64,
}

/// A settled land.
#[derive(Clone)]
pub(crate) struct LandDone {
    pub landed: Id,
    pub main: Id,
    pub rebased: u64,
    /// Present when the advisory `base` disagreed with head's actual parent.
    pub note: Option<String>,
    /// Paths the LANDED node records as conflicted (format v2). Non-empty
    /// exactly when the settlement node is a conflicted node — the land
    /// then settles with `lands.state = 'conflicted'`, whether the reps
    /// were minted by this land's `--record-conflict` or carried through a
    /// clean rebase of a conflicted chain.
    pub conflicts: Vec<String>,
}

/// `BEGIN IMMEDIATE` … `COMMIT` with rollback-on-drop: every early return
/// or panic inside the transaction leaves zero rows (durability invariant
/// 2). Disarmed only after a successful COMMIT, so the post-commit crash
/// point cannot roll back settled state.
struct TxnGuard<'a> {
    /// The transaction guard: BEGIN has run on the connection; DROP (or
    /// `commit`) ends it.
    guard: Option<naatre_store::DbTx<'a>>,
    armed: bool,
}

impl<'a> TxnGuard<'a> {
    fn begin(conn: &'a Db) -> Result<TxnGuard<'a>, ApiError> {
        // BEGIN IMMEDIATE takes the SQLite write lock up front; Postgres
        // has no immediate/deferred distinction and takes locks at first
        // write — under the single-writer slot the two behave identically.
        let guard = conn
            .transaction_immediate()
            .map_err(ApiError::log_internal)?;
        Ok(TxnGuard {
            guard: Some(guard),
            armed: true,
        })
    }

    fn commit(mut self) -> Result<(), ApiError> {
        // The guard's COMMIT; the DbTx's own drop-rollback is disarmed by
        // the successful commit inside.
        self.guard
            .take()
            .expect("txn guard present until commit")
            .commit()
            .map_err(ApiError::log_internal)?;
        self.armed = false;
        Ok(())
    }
}

impl Drop for TxnGuard<'_> {
    fn drop(&mut self) {
        if self.armed {
            // DbTx's Drop issues the ROLLBACK (SQLite engine-level, or the
            // Postgres statement).
            self.guard.take();
        }
    }
}

/// `Id` from raw column bytes.
fn id_of(bytes: &[u8]) -> Option<Id> {
    <[u8; 32]>::try_from(bytes).ok().map(Id)
}

/// The split-mode land's audit payload, appended AFTER the repo
/// transaction commits (the documented order — the reverse would let a
/// crash fabricate a `land` audit row for a land that never stood).
struct PendingAudit {
    now_ns: i64,
    principal: String,
    details_json: String,
}

/// Runs one land inside its single write transaction (the REPO's
/// transaction; the route resolved the repo and holds the repo's write
/// slot). The incidental CATALOG writes — the merged blobs' `objects`
/// rows and the `land` audit row — follow the connection shape: on the
/// split, the object rows go through `cx.cat()` BEFORE the repo rows that
/// reference them (the orphan direction — a crash leaves orphan rows the
/// GC collects, never rows naming absent bytes), and the audit row
/// commits AFTER the repo transaction ([`PendingAudit`]) — a crash in
/// that window loses the audit row for a landed key, the documented
/// split-mode atomicity boundary. In single-DB mode the catalog IS the
/// work connection, so the audit joins the land's one transaction
/// (nothing is lost, and a rolled-back land leaves NO audit row); a
/// post-commit audit-insert FAILURE on the split is logged and answered
/// success — the land stood, and answering failure would invite a retry
/// against a settled key.
pub(crate) fn execute(
    cx: &RepoCx<'_>,
    cas: &dyn BlobStore,
    derive: &crate::derive::DeriveRegistry,
    cmd: &LandCommand,
) -> Result<LandDone, ApiError> {
    let guard = TxnGuard::begin(cx.db)?;
    match land_in_txn(cx, cas, derive, cmd) {
        Ok((done, pending_audit)) => {
            crash_point("before_commit");
            guard.commit()?;
            // Split mode only (`Some`): the land HAS STOOD — a post-commit
            // audit-insert failure must not answer failure (the client
            // would retry a landed key). The audit loss is the documented
            // split-mode window; log it for ops and answer success.
            // Single mode never reaches this arm: its audit rode the
            // transaction (a failed audit rolls the land back).
            if let Some(audit) = pending_audit
                && let Err(e) = db::audit_append(
                    &cx.cat(),
                    audit.now_ns,
                    &audit.principal,
                    "land",
                    &audit.details_json,
                )
            {
                tracing::error!(
                    error = %e,
                    "land audit row lost after commit (the documented split-mode window)"
                );
            }
            crash_point("after_commit");
            Ok(done)
        }
        Err(e) => {
            let is_conflict = e.code() == ApiCode::RefConflict;
            drop(guard); // rollback first: the key stays unsettled
            if is_conflict {
                crash_point("after_conflict_rollback");
            }
            Err(e)
        }
    }
}

fn land_in_txn(
    cx: &RepoCx<'_>,
    cas: &dyn BlobStore,
    derive: &crate::derive::DeriveRegistry,
    cmd: &LandCommand,
) -> Result<(LandDone, Option<PendingAudit>), ApiError> {
    let repo_id = cx.id;
    let conn = cx.db;
    // 1. Idempotency: a settled key replays its recorded result; a
    // different head under the same key is a 409 with the settled state
    // pointer. (A committed pending row is unobservable — see the module
    // doc.) Both settled states replay: `done` and M12's `conflicted`
    // (a recorded conflicted node IS a single settlement).
    if let Some(row) =
        db::land_get(conn, repo_id, &cmd.principal, &cmd.key).map_err(ApiError::log_internal)?
    {
        match row.state.as_str() {
            "done" | "conflicted" => {
                let settled = row
                    .head
                    .as_deref()
                    .and_then(id_of)
                    .ok_or_else(ApiError::internal)?;
                if settled != cmd.head {
                    return Err(ApiError::new(
                        ApiCode::IdempotencyKeySettled,
                        "this land key is settled with a different head",
                    )
                    .with_details(serde_json::json!({
                        "settled_head": settled.to_hex(),
                        "land_status_pointer":
                            format!("/v1/repos/{}/land/{}", cmd.repo_name, cmd.key),
                    })));
                }
                return replay(row.result_json).map(|done| (done, None));
            }
            "pending" => {
                return Err(
                    ApiError::new(ApiCode::WriterBusy, "land for this key is in flight")
                        .retryable(true),
                );
            }
            other => return Err(ApiError::log_internal(format!("lands.state {other:?}"))),
        }
    }

    // M10b tombstone gate: a redacted head, a redacted `main` tip, or any
    // redacted node on the rebase chain — the 3-way base included — is a
    // STRUCTURED 409 conflict, never a silent hole (the rebase would copy
    // redacted attribution and build on redacted content). Probed before
    // any write: a refused land leaves zero rows and the key unsettled.
    let redacted_conflict = |id: &Id| -> ApiError {
        ApiError::new(
            ApiCode::RedactedBase,
            "the land's base chain crosses a redacted changeset",
        )
        .with_details(serde_json::json!({ "redacted": id.to_hex() }))
    };
    if db::redaction_active(conn, repo_id, &cmd.head).map_err(ApiError::log_internal)? {
        return Err(redacted_conflict(&cmd.head));
    }

    // Pending row first (plan: BEGIN IMMEDIATE → pending lands row → load
    // main). It commits only together with the finished land.
    db::land_insert_pending(conn, repo_id, &cmd.principal, &cmd.key, &cmd.head)
        .map_err(ApiError::log_internal)?;
    crash_point("after_pending_row");

    // 2. `main` must exist (imports seed it; land is its only writer).
    let old_main: Id = match db::ref_get(conn, repo_id, "main").map_err(ApiError::log_internal)? {
        Some(row) => row
            .target
            .as_deref()
            .and_then(id_of)
            .ok_or_else(ApiError::internal)?,
        None => {
            return Err(ApiError::new(
                ApiCode::Validation,
                "repo has no main to land onto",
            ));
        }
    };
    if db::redaction_active(conn, repo_id, &old_main).map_err(ApiError::log_internal)? {
        return Err(redacted_conflict(&old_main));
    }

    let fetch_graph = |id: &Id| -> Result<Option<(i64, Vec<Id>)>, ApiError> {
        db::changeset_graph(conn, repo_id, id)
            .map(|g| g.map(|g| (g.generation, g.parents)))
            .map_err(ApiError::log_internal)
    };

    // Everything reachable from main (inclusive) — the containment oracle
    // and the rebase chain's stop condition. A SQL failure propagates like
    // every sibling call; a referenced-but-missing changeset is broken
    // state (main points into a hole) — an internal error, never a silent
    // truncation of the containment set.
    let mut in_main = std::collections::HashSet::new();
    let mut queue = std::collections::VecDeque::from([old_main]);
    while let Some(id) = queue.pop_front() {
        if !in_main.insert(id) {
            continue;
        }
        let Some((_, parents)) = fetch_graph(&id)? else {
            return Err(ApiError::log_internal(format!(
                "main's ancestry is broken: changeset {} is referenced but missing",
                id.to_hex()
            )));
        };
        queue.extend(parents);
    }

    // 3. head == main → idempotent success.
    if cmd.head == old_main {
        let reps = rep_paths_of(conn, repo_id, &old_main)?;
        return settle(cx, conn, cmd, repo_id, old_main, old_main, 0, None, &reps);
    }

    // 4. Already contained (merge-base(head, main) == head): settled, no
    // new changesets — rebased ids may differ, containment is by
    // reachability.
    if in_main.contains(&cmd.head) {
        let reps = rep_paths_of(conn, repo_id, &old_main)?;
        return settle(cx, conn, cmd, repo_id, old_main, old_main, 0, None, &reps);
    }

    // Advisory base note: the request's base is ignored for the rebase; if
    // it disagrees with head's actual parent, say so in the response.
    let note = match cmd.base_hint {
        Some(hint) => {
            match db::changeset_graph(conn, repo_id, &cmd.head).map_err(ApiError::log_internal)? {
                Some(g) if g.parents.first() != Some(&hint) => Some(format!(
                    "request base {} is not head's parent ({}); the server rebased onto its merge-base with main",
                    hint.to_hex(),
                    g.parents
                        .first()
                        .map(Id::to_hex)
                        .unwrap_or_else(|| "<none>".into()),
                )),
                _ => None,
            }
        }
        None => None,
    };

    // 5. Rebase the head-side changes (oldest first): the first-parent
    // chain from head down to the first changeset CONTAINED in main. For a
    // linear history that is exactly the merge-base; across an imported
    // 2-parent merge node whose first parent is on the head side, the
    // first-parent chain passes through an ancestor of main instead of the
    // merge-base itself — stopping at containment handles both, and never
    // includes a node main already has. A head with no path to main cannot
    // be rebased onto it.
    let mut chain: Vec<Id> = Vec::new();
    let mut cursor = cmd.head;
    while !in_main.contains(&cursor) {
        if db::redaction_active(conn, repo_id, &cursor).map_err(ApiError::log_internal)? {
            return Err(redacted_conflict(&cursor));
        }
        chain.push(cursor);
        let graph = fetch_graph(&cursor)?.ok_or_else(|| {
            ApiError::new(ApiCode::NotFound, "head is not a changeset of this repo")
        })?;
        match graph.1.first() {
            Some(next) => cursor = *next,
            None => {
                return Err(ApiError::new(
                    ApiCode::Validation,
                    "head shares no history with main",
                ));
            }
        }
    }
    // The walk's exit node is the rebase's 3-way base (the first changeset
    // contained in main): tombstoned base → the same structured conflict.
    if db::redaction_active(conn, repo_id, &cursor).map_err(ApiError::log_internal)? {
        return Err(redacted_conflict(&cursor));
    }
    chain.reverse();

    // Scope: every change path of the rebase chain must sit inside the
    // token's scope (checked before any merge work; 403 names the path).
    for cs in &chain {
        for c in db::file_changes_list(conn, repo_id, cs).map_err(ApiError::log_internal)? {
            if !authz::scope_allows(cmd.scope.as_deref(), &c.path) {
                return Err(ApiError::new(
                    ApiCode::Forbidden,
                    "path is outside this token's scope",
                )
                .with_details(serde_json::json!({ "path": c.path })));
            }
        }
    }

    // The evolving main-side tip: main's changeset, tree, generation.
    let main_graph = db::changeset_graph(conn, repo_id, &old_main)
        .map_err(ApiError::log_internal)?
        .ok_or_else(ApiError::internal)?;
    let mut tip_id = old_main;
    let mut tip_gen = main_graph.generation;
    let mut tip_tree = load_tree(conn, main_graph.root_tree_id)?;

    let mut rebased: u64 = 0;
    let mut written_blobs: Vec<Id> = Vec::new();

    for node in &chain {
        let node_row = db::changeset_get(conn, repo_id, node)
            .map_err(ApiError::log_internal)?
            .ok_or_else(ApiError::internal)?;
        let node_graph = db::changeset_graph(conn, repo_id, node)
            .map_err(ApiError::log_internal)?
            .ok_or_else(ApiError::internal)?;
        let node_tree = load_tree(conn, node_row.root_tree_id.clone())?;
        // Base = the node's ORIGINAL first parent's tree (the delta being
        // re-applied is the node's own change against it); a parentless
        // node rebases from the empty tree.
        let parent_tree = match node_graph.parents.first() {
            Some(p) => {
                let p_row = db::changeset_get(conn, repo_id, p)
                    .map_err(ApiError::log_internal)?
                    .ok_or_else(ApiError::internal)?;
                load_tree(conn, p_row.root_tree_id.clone())?
            }
            None => MemNode::empty(),
        };

        // M12: the conflict reps this node already carries (a v1 node
        // carries none). They travel through the rebase — see `carry`
        // below.
        let node_reps = changeset::decode(&node_row.canonical)
            .map_err(ApiError::log_internal)?
            .conflicts;

        // Path-level 3-way: base = original parent, ours = the evolving
        // rebased side, theirs = the node's own tree. Any path conflict
        // aborts the whole land — nothing below has been written yet, and
        // the guard rolls the (pending-row) transaction back — unless the
        // land opted into recording, where every representable conflict
        // materializes its markers into the merged tree instead and the
        // unrepresentable ones (a deletion side) still refuse.
        let fetch_blob = |id: &Id| -> Result<Vec<u8>, String> {
            cas.get(id)
                .map_err(|e| format!("blob {} unavailable: {e}", id.to_hex()))
        };
        let (merged, reconflicted) = if cmd.record_conflict {
            let rec = merge_trees_recording(&parent_tree, &tip_tree, &node_tree, fetch_blob)
                .map_err(|c| conflict_error(c, RESOLUTION_RECIPE_REFUSED))?;
            // Pre-sized marker overflow: refuse 413 BEFORE any CAS write
            // (the marker bytes existed only in memory so far).
            if let Some((path, size)) = rec.oversized.first() {
                return Err(ApiError::new(
                    ApiCode::TooLarge,
                    format!(
                        "materialized conflict markers for {path:?} would be {size} bytes — \
                         over the blob limit; resolve the conflict instead"
                    ),
                )
                .with_details(serde_json::json!({ "path": path, "size": size })));
            }
            (rec.tree, rec.conflicts)
        } else {
            let merged = merge_trees(&parent_tree, &tip_tree, &node_tree, fetch_blob)
                .map_err(|c| conflict_error(c, RESOLUTION_RECIPE))?;
            (merged, Vec::new())
        };

        // The reps the rebased changeset carries: this node's reps for
        // paths the rebase did not re-conflict, plus one rep per
        // re-conflicted path — the new sides (the conflict composes: the
        // previous materialization is itself a side of the new conflict).
        let new_reps: Vec<ConflictRep> = reconflicted
            .iter()
            .map(|c| content_rep(c).expect("recorded conflicts are representable"))
            .collect();
        let reps = conflict::carry(&node_reps, &new_reps);

        // The rebased node's stored delta: rebased parent tree → merged
        // tree. A node whose change is already contained produces an empty
        // delta, and the canonical format cannot encode an empty changeset
        // — it is dropped from the chain (its content is on main either
        // way; descendants parent onto the surviving tip). A node carrying
        // conflict reps is never dropped: the reps ARE its payload.
        let delta = delta_between(&tip_tree, &merged.node);
        if delta.is_empty() && reps.is_empty() {
            continue;
        }

        // The rebased changeset: new parent, same delta content, original
        // author/message/timestamp, landing principal, bumped generation.
        // Non-empty `conflicts` makes it a format-v2 node.
        let cs = Changeset {
            repo_id: repo_id as u64,
            parents: vec![tip_id],
            author: node_row.author.clone(),
            message: node_row.message.clone(),
            timestamp_ns: node_row.timestamp_ns as u64,
            changes: delta,
            conflicts: reps,
        };
        let canonical = changeset::encode(&cs).map_err(|e| match e {
            CoreError::EncodeTooLarge => ApiError::new(
                ApiCode::TooLarge,
                "rebased changeset exceeds the 16 MiB canonical cap",
            ),
            other => ApiError::log_internal(other),
        })?;
        // The id domain follows the content: conflicted nodes hash under
        // the `naatre\0v2\0` domain; v1 ids are never recomputed.
        let new_id = changeset::id_for_encoded(&canonical, !cs.conflicts.is_empty());

        let mut blobs: Vec<Id> = Vec::new();
        for c in &cs.changes {
            if let (Op::Add | Op::Mod, Some(blob)) = (c.op, c.blob)
                && !blobs.contains(&blob)
            {
                blobs.push(blob);
            }
        }

        // The rebase-body tombstone gate (M10b), INSIDE the land's write
        // transaction and before any CAS write or row: a rebased changeset
        // id — or any blob id the rebase writes or references — that is
        // tombstoned in this repo (active or purged) refuses the whole
        // land with 410 `object_redacted`, the same shape POST /changesets
        // refuses. Without it a byte-identical rebase of a purged
        // changeset (the original timestamp reproducing the exact id)
        // would recreate its body, and a rebase carrying redacted content
        // would write its bytes. The chain nodes themselves were probed on
        // the walk above. One batched probe per node (the changesets
        // route's masking probe; chunked IN-lists) over the new id and
        // every written/referenced blob id — the kinds hash disjoint
        // domains, so one probe answers for both arms.
        let mut gate_ids: Vec<Id> = blobs.clone();
        gate_ids.extend(merged.new_blobs.iter().map(|(id, _)| *id));
        // M12a-fix (the blocker): every blob id whose CONTENT flows into
        // what this rebase produces joins the batch — the clean line-merge
        // inputs (base + both sides; the merged blob is new bytes the gate
        // cannot recognize by output id) and every conflict-representation
        // id, minted AND carried (record mode fetches side/base bytes raw
        // and embeds them into a fresh servable marker blob; a carried rep
        // re-records its ids on every rebase). A tombstoned one refuses
        // 410 BEFORE the marker write below — nothing reaches the CAS or
        // repo_blobs first.
        gate_ids.extend(merged.input_ids.iter().copied());
        for rep in &cs.conflicts {
            if let Some(base) = rep.base {
                gate_ids.push(base);
            }
            gate_ids.extend(rep.sides.iter().copied());
        }
        gate_ids.push(new_id);
        gate_ids.sort_by_key(|id| id.0);
        gate_ids.dedup();
        let offenders = db::redactions_active_among(conn, repo_id, &gate_ids)
            .map_err(ApiError::log_internal)?;
        if !offenders.contains(&new_id)
            && let Some(blob) = offenders.iter().copied().min_by_key(|id| id.to_hex())
        {
            let count = offenders.len();
            // The guidance fires when ANY offender is content that flows
            // into a conflict representation or a merged blob — a rep
            // side/base or a merge_lines input — NOT just the single
            // lowest-hex id this message names: a mixed-offender land (a
            // delta blob AND a rep side both tombstoned) must not silently
            // lose the redacted-side remedy because the pick landed on the
            // delta blob. The message still names the lowest-hex id; the
            // remedy is the same either way.
            let conflicted_content = offenders.iter().any(|id| {
                merged.input_ids.contains(id)
                    || cs
                        .conflicts
                        .iter()
                        .any(|rep| rep.base == Some(*id) || rep.sides.contains(id))
            });
            let guidance = if conflicted_content {
                " A side of this conflict is redacted in this repo, so the land can \
                 neither record nor merge it — remove or rewrite the conflicting edit \
                 at that path, or resolve with the admin who holds the redaction."
            } else {
                ""
            };
            return Err(ApiError::new(
                ApiCode::ObjectRedacted,
                if count == 1 {
                    format!(
                        "blob {} is redacted in {repo}; referencing it is refused{guidance}",
                        blob.to_hex(),
                        repo = cmd.repo_name
                    )
                } else {
                    format!(
                        "blob {} is redacted in {repo}; referencing it is refused \
                         (+{} more redacted ids){guidance}",
                        blob.to_hex(),
                        count - 1,
                        repo = cmd.repo_name
                    )
                },
            ));
        }
        if offenders.contains(&new_id) {
            return Err(ApiError::new(
                ApiCode::ObjectRedacted,
                format!(
                    "changeset {} is redacted in {repo}; recreating it is refused",
                    new_id.to_hex(),
                    repo = cmd.repo_name
                ),
            ));
        }

        // Merged blob contents go into the CAS before any row commits
        // (files before rows; Cas::put fsyncs before returning), and their
        // objects rows record in the SAME transaction (M12b fix, caught by
        // the e2e's fsck-after-conflicted-land leg): a rebase's merged
        // content is byte-identical to what the client PUT, so its row
        // already exists and the upsert is a no-op — but a recorded
        // conflict's MARKER blob is server-rendered, never client-PUT, so
        // without the row the blob is cas_unrecorded and every fsck after
        // a conflicted land finds it.
        for (blob_id, bytes) in &merged.new_blobs {
            if written_blobs.contains(blob_id) {
                continue;
            }
            let put = cas.put(bytes).map_err(ApiError::log_internal)?;
            if put.id != *blob_id {
                return Err(ApiError::log_internal(format!(
                    "merged blob id mismatch: computed {}, CAS wrote {}",
                    blob_id.to_hex(),
                    put.id.to_hex()
                )));
            }
            // The ledger row is catalog-side, written BEFORE the repo rows
            // that reference it (the orphan direction; see `execute`).
            db::object_row_upsert(
                &cx.cat(),
                blob_id,
                bytes.len() as i64,
                put.csize as i64,
                cmd.now_ns,
            )
            .map_err(ApiError::log_internal)?;
            written_blobs.push(*blob_id);
        }
        crash_point("after_blob_write");

        if new_id == *node {
            // Fast-forward: the node is already exactly on top of the tip
            // (same parent, same bytes → same id). It IS the rebased node —
            // adopt it and walk on instead of re-inserting a duplicate.
            tip_id = *node;
            tip_tree = node_tree;
            continue;
        }
        tip_gen += 1;
        let (root_tree_id, _, trees) = merged.node.collect().map_err(map_derive)?;

        db::changeset_insert_rows(
            conn,
            db::ChangesetInsert {
                repo_id,
                repo_name: &cmd.repo_name,
                id: &new_id,
                canonical: &canonical,
                author: &cs.author,
                message: &cs.message,
                timestamp_ns: cs.timestamp_ns as i64,
                generation: tip_gen,
                principal: &cmd.principal,
                root_tree_id: &root_tree_id,
                parents: &cs.parents,
                changes: &cs.changes,
                blobs: &blobs,
                trees: &trees,
                now_ns: cmd.now_ns,
            },
            db::ChangesetExtras::NONE,
        )
        .map_err(ApiError::log_internal)?;
        // Derived data (M6a): the rebased changeset is a brand-new node —
        // enqueue (or eagerly derive) it for every registered kind, inside
        // the land's single transaction like every other row here.
        crate::derive::enqueue_changeset(
            conn,
            derive,
            repo_id,
            new_id,
            &cs.parents,
            crate::derive::EagerEnv {
                now_ns: cmd.now_ns,
                clock: cmd.clock.clone(),
                walk_deadline_ns: cmd.walk_deadline,
            },
        )
        .map_err(ApiError::log_internal)?;

        tip_id = new_id;
        tip_tree = merged.node;
        rebased += 1;
    }
    crash_point("after_rebase");

    // 6/7/8. CAS main from the target we rebased onto (version bump). Zero
    // updated rows cannot happen under the single-writer slot; report it as
    // the ref conflict it would be rather than silently proceeding.
    let moved = db::main_cas_move(conn, repo_id, &old_main, &tip_id, &cmd.principal)
        .map_err(ApiError::log_internal)?;
    if moved == 0 {
        return Err(ApiError::new(
            ApiCode::RefConflict,
            "main moved while landing (stale CAS)",
        ));
    }
    crash_point("after_cas_main");

    // The settlement node's conflict paths decide `lands.state` (see
    // `settle`): the landed node IS the settlement, whether its reps were
    // minted here or carried through.
    let landed_reps = rep_paths_of(conn, repo_id, &tip_id)?;
    settle(
        cx,
        conn,
        cmd,
        repo_id,
        old_main,
        tip_id,
        rebased,
        note,
        &landed_reps,
    )
}

/// Records the settled row (`done`, or M12's `conflicted` when the landed
/// node itself is a conflicted node — still ONE settlement: the conflicted
/// node IS the settlement) + the `land` audit entry, and returns the
/// response payload. `base` is the `main` target the land rebased onto.
/// Runs inside the caller's transaction. The audit row: single-DB mode
/// (the catalog IS the work connection) appends it INSIDE the land's
/// transaction — atomic; the split hands the payload back and `execute`
/// appends it AFTER the commit (the documented after-commit window; an
/// audit that beat the commit would fabricate a land row on a crash).
#[allow(clippy::too_many_arguments)]
fn settle(
    cx: &RepoCx<'_>,
    conn: &Db,
    cmd: &LandCommand,
    repo_id: i64,
    base: Id,
    landed: Id,
    rebased: u64,
    note: Option<String>,
    conflicts: &[String],
) -> Result<(LandDone, Option<PendingAudit>), ApiError> {
    let state = if conflicts.is_empty() {
        "done"
    } else {
        "conflicted"
    };
    let result = serde_json::json!({
        "landed": landed.to_hex(),
        "rebased": rebased,
        "note": note,
        "conflicts": conflicts,
    });
    db::land_settle(
        conn,
        repo_id,
        &cmd.principal,
        &cmd.key,
        &cmd.head,
        &base,
        state,
        &result.to_string(),
    )
    .map_err(ApiError::log_internal)?;
    let details_json = serde_json::json!({
        "repo": cmd.repo_name,
        "key": cmd.key,
        "head": cmd.head.to_hex(),
        "landed": landed.to_hex(),
        "rebased": rebased,
        "via": "land",
        "conflicts": conflicts,
    })
    .to_string();
    let done = LandDone {
        landed,
        main: landed,
        rebased,
        note,
        conflicts: conflicts.to_vec(),
    };
    if cx.cat_is_db() {
        db::audit_append(&cx.cat(), cmd.now_ns, &cmd.principal, "land", &details_json)
            .map_err(ApiError::log_internal)?;
        Ok((done, None))
    } else {
        Ok((
            done,
            Some(PendingAudit {
                now_ns: cmd.now_ns,
                principal: cmd.principal.clone(),
                details_json,
            }),
        ))
    }
}

/// Idempotent replay of a settled land's recorded result. Also the join
/// fallback's replay (`routes::land::settled_replay`). Rows recorded
/// before M12 carry no `conflicts` key — replayed as empty.
pub(crate) fn replay(result_json: Option<String>) -> Result<LandDone, ApiError> {
    let Some(json) = result_json else {
        return Err(ApiError::internal());
    };
    let value: serde_json::Value = serde_json::from_str(&json).map_err(ApiError::log_internal)?;
    let landed = value["landed"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(ApiError::internal)?;
    let rebased = value["rebased"].as_u64().ok_or_else(ApiError::internal)?;
    let note = value["note"].as_str().map(str::to_string);
    let conflicts = value["conflicts"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(str::to_string))
                .collect::<Vec<String>>()
        })
        .unwrap_or_default();
    Ok(LandDone {
        landed,
        main: landed,
        rebased,
        note,
        conflicts,
    })
}

/// The conflict paths a changeset carries (empty for every v1 node).
fn rep_paths_of(conn: &Db, repo_id: i64, id: &Id) -> Result<Vec<String>, ApiError> {
    let reps = match db::changeset_get(conn, repo_id, id).map_err(ApiError::log_internal)? {
        Some(row) => {
            changeset::decode(&row.canonical)
                .map_err(ApiError::log_internal)?
                .conflicts
        }
        None => Vec::new(),
    };
    Ok(reps.iter().map(|r| r.path.clone()).collect())
}

/// The 409 payload: every path conflict plus the resolution recipe —
/// [`RESOLUTION_RECIPE`] for the default contract, [`RESOLUTION_RECIPE_REFUSED`]
/// for the record-mode refusal of an unrecordable conflict.
fn conflict_error(conflicts: Vec<PathConflict>, recipe: &'static str) -> ApiError {
    let list: Vec<serde_json::Value> = conflicts
        .iter()
        .map(|c| {
            let hex = |o: Option<Id>| o.map(|id| id.to_hex());
            serde_json::json!({
                "path": c.path,
                "reason": c.reason,
                "base": hex(c.base_blob),
                "ours": hex(c.ours_blob),
                "theirs": hex(c.theirs_blob),
            })
        })
        .collect();
    ApiError::new(
        ApiCode::RefConflict,
        format!(
            "{} conflicting path(s) while rebasing onto main",
            list.len()
        ),
    )
    .with_details(serde_json::json!({ "conflicts": list, "recipe": recipe }))
}

fn load_tree(conn: &Db, root_tree_id: Option<Vec<u8>>) -> Result<MemNode, ApiError> {
    let root = root_tree_id.as_deref().and_then(id_of);
    MemNode::load(conn, root).map_err(map_derive)
}

fn map_derive(e: DeriveError) -> ApiError {
    match e {
        DeriveError::Core(CoreError::EncodeTooLarge) => ApiError::new(
            ApiCode::TooLarge,
            "merged tree exceeds the 16 MiB canonical cap",
        ),
        other => ApiError::log_internal(other),
    }
}
