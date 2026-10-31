//! The generic derived-data framework (M6a): recursive computation from
//! parents, eager-or-deferred per kind, a background derive queue with
//! leases, and poison-node quarantine — a malformed node is skipped and
//! reported, never head-of-line blocks.
//!
//! A kind implements [`Deriver`] and registers on
//! [`crate::state::ServerState::register_deriver`] (M6b registers the real
//! kinds — blame, path_history, commit_graph, see [`crate::derived`] — in
//! the CLI's `serve` verb; tests register throwaway kinds). The contract,
//! per kind:
//!
//! - **Recursive from parents.** [`Deriver::derive`] receives every
//!   parent's framework-stored output ([`DerivedOut`], in `ord` order) and
//!   returns this node's output. A parent that is POISONED — or has no row
//!   at all, which within the framework happens only for kinds registered
//!   after their changesets (M6b's backfill enqueues parents first) — is
//!   handed to the child as the EMPTY MARKER ([`DerivedOut::empty`]):
//!   documented fallback, so a bad node can never head-of-line block its
//!   descendants (the plan's exact wording).
//! - **Idempotent.** Same input → same output. The framework enforces this
//!   by re-running: a lost lease (crash mid-derivation) puts the node back
//!   to pending and the deriver runs again from scratch. The deriver's OWN
//!   storage writes must therefore be idempotent too (keyed upserts).
//! - **Eager or deferred.** An eager kind derives synchronously inside the
//!   changeset's write transaction (create, land, import) — done or, on
//!   failure, straight back to the queue as a failed first attempt. A
//!   deferred kind only enqueues an O(1) pending row: the write never
//!   blocks on derivation, and the queue table grows at most with the
//!   changeset count (the depth/size caps already in place) — enqueue
//!   never drops or rejects.
//!
//! The queue ([`derive_pass`], driven by `serve`'s background task beside
//! the snapshot-GC task on the same injectable-interval pattern) per kind:
//! sweep expired leases (a lost lease = a crash mid-derivation — the row
//! returns to pending, or to poison once [`DERIVE_MAX_ATTEMPTS`] attempts
//! have failed), then drain: pick the oldest pending node whose parents are
//! all done-or-poison, lease it (`state='deriving'`,
//! `lease_expires = now + 2 × interval`), run the deriver, record
//! `state='done'` with the output the children will receive. The deriver's
//! output writes and the framework's done-record are deliberately TWO
//! write transactions: a crash between them leaves the output written and
//! the lease held — the expiry sweep re-runs the whole derivation, which
//! is why the idempotency contract above exists.
//!
//! **Per-call caps.** Two deriver-independent caps guard ONE derive call
//! (plan: "depth/size caps"): the per-call COOPERATIVE walk budget —
//! a per-kind value ([`Deriver::walk_budget`]) handed to every deriver
//! as a [`DeriveCtx`] — and [`DERIVE_MAX_OUTPUT_BYTES`] on the
//! framework-carried output per node. The budget binds derivers that
//! charge their visits through `ctx.charge`; a deriver issuing raw SQL
//! past the ctx is not capped by it. What they guard is a deriver's
//! unbounded walking inside one call and unbounded output — NOT long
//! history: a deep or long chain derives batch-by-batch THROUGH this
//! queue, each node a batch of one, the running output carried in the
//! parents' outputs, so a 10,000-commit linear repo derives fully (the
//! regression pin in `m6a_test.rs`). A node that trips either cap is
//! quarantined IMMEDIATELY ([`DERIVE_MAX_ATTEMPTS`] does not apply — the
//! caps are structural, so retrying cannot help), rows a `derive_reports`
//! reason, and its children still derive on the empty marker (the
//! never-head-of-line rule above). The wall-clock deadline behind the
//! budget ([`DERIVE_WALK_DEADLINE_NS`]) is deliberately NOT structural:
//! an overrun releases the lease back to pending as a FAILED ATTEMPT
//! (bounded by [`DERIVE_MAX_ATTEMPTS`], a distinct `derive_reports`
//! reason recording the measured units/sec) — a budget mis-sized against
//! real probe cost, or a slow store, must not permanently quarantine a
//! legitimate node.
//!
//! **Poison taint.** A node that derived on an empty-marker parent
//! (poisoned, or tainted itself) records `tainted = 1` with `poison_cs`
//! naming the NEAREST poisoned ancestor; descendants inherit both through
//! the parents' contributions ([`ParentOut`]). Done-with-taint is still
//! done — the read routes raise their degraded flag off the row — so a
//! poisoned mid-chain node never head-of-line blocks, and nothing
//! downstream presents itself as exact.
//!
//! Single-process note: the lease exists for crash recovery, not for
//! multi-writer safety — there is exactly one queue task per process and
//! the sweep runs in it, so a derivation can never be double-run while its
//! holder lives. A second serving process on the same data dir is already
//! excluded by the data-dir flock.
//!
//! All SQL lives in [`crate::db`]; this module is policy and orchestration.
//! The read-during-backlog contract (503 `derivation_pending` +
//! `Retry-After`) lands with M6b's read routes; what they need from the
//! framework — the per-(kind, changeset) is-derived/is-pending query — is
//! [`status`], tested in `m6a_test.rs`.

use std::cell::Cell;
use std::fmt;
use std::future::Future;
use std::sync::Arc;
use std::time::Duration;

use naatre_store::{Db, DbError};

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::db;
use crate::state::{AppState, ClockFn};

/// Failed attempts before a node is quarantined: three strikes — a deriver
/// `Err` or a lease expiry each count one — then poison + a
/// `derive_reports` row (the plan's "skipped and reported").
pub const DERIVE_MAX_ATTEMPTS: i64 = 3;

/// The bootstrap per-call walk budget for kinds that do not override
/// [`Deriver::walk_budget`]: the CHARGES one derive call may spend through
/// its [`DeriveCtx`]. The budget is COOPERATIVE — it binds derivers that
/// charge their visits through `ctx.charge`; a deriver issuing raw SQL
/// past the ctx is not capped by it. It bounds a deriver's per-call work,
/// NOT history depth: long history derives batch-by-batch through the
/// queue (each node one call), so a linear repo of any length derives
/// fully. The default covers a worst legitimate node — one visit per entry
/// of a 100,000-entry (MAX_ENTRIES) changeset — with 2× headroom; kinds
/// with heavier per-call shapes override with their own justified value.
///
/// **Pricing convention.** A charge is one unit of ACTUAL work, so budget
/// units admit no shortcut pricing: one per row visited, and one per
/// (tree_id, name) component PROBE ([`crate::db::tree_lookup_path_charged`]
/// is the priced tree-walk every tree-walking deriver must use — each
/// component is an indexed point lookup on `tree_entries`' PRIMARY KEY, so
/// one component costs one charge regardless of the directory's entry
/// count; the read-side [`crate::db::tree_lookup_path`] shares the engine
/// unpriced). A kind's budget value is justified against this per-component
/// cost in its [`Deriver::walk_budget`] override.
pub const DEFAULT_WALK_BUDGET: u64 = 200_000;

/// The per-call wall-clock backstop behind the walk budget: a derive call
/// still running this many ns past its [`DeriveCtx`]'s creation fails with
/// a [`DeriveFailure::WalkDeadline`] on its next charge — a RETRYABLE
/// failure (the lease is released, the node re-queued as a FAILED ATTEMPT
/// with a measured units/sec reason row, quarantined only at
/// [`DERIVE_MAX_ATTEMPTS`]) — even with budget left. The budget bounds
/// CHARGED work; the deadline bounds the wall clock those charges
/// actually take, so a budget mis-sized against real probe cost (or a
/// slow store) cannot hold the write slot indefinitely. Read through the
/// state's injectable clock ([`crate::state::ServerState::clock`]), so
/// expiry behavior stays deterministic under test; the window itself is
/// injectable per state (`ServerState::derive_walk_deadline`) for the
/// retry-not-poison pin.
pub const DERIVE_WALK_DEADLINE_NS: u64 = 2_000_000_000;

/// Size cap on the framework-carried output of one node, matching the
/// 16 MiB canonical-encoding cap. An over-cap output is never recorded —
/// the node is quarantined immediately with the size reason.
pub const DERIVE_MAX_OUTPUT_BYTES: usize = 16 * 1024 * 1024;

/// Rows per boot-backfill batch ([`backfill`]): one batch is one
/// single-writer-slot hold, so its size bounds how long a concurrent write
/// can 503 behind the backfill. 10,000 indexed inserts are milliseconds;
/// a whole-repo INSERT..SELECT is not.
pub const BACKFILL_BATCH: i64 = 10_000;

/// The test-only one-shot [`crate::state::ServerState::derive_test_busy_abort`]
/// hook's injection points (see that field; never armed in production).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BusyAbort {
    /// The next mid-derivation write for this changeset fails with 503
    /// `writer_busy` — the lease-held abort shape.
    Derive(Id),
    /// [`BusyAbort::Derive`], and THEN the first attempt of the lease
    /// release that follows is busy too (the double-collision shape: the
    /// release's busy-retry must still land with no attempt burned). The
    /// release shot re-arms the hook as [`BusyAbort::Release`].
    DeriveThenRelease(Id),
    /// The next lease-release write for this changeset is busy (normally
    /// armed by [`BusyAbort::DeriveThenRelease`]'s transition, not by
    /// tests directly).
    Release(Id),
}

/// The quarantine reason for the walk budget (shared by the queue and
/// eager paths so reports read identically wherever the cap fires).
fn walk_budget_reason(budget: u64) -> String {
    format!(
        "derive walk budget exhausted: a deriver's per-call budget of \
         {budget} visits ran out"
    )
}

/// The quarantine reason for the size cap.
fn size_cap_reason(len: usize) -> String {
    format!(
        "derive output cap exceeded: {len} bytes exceeds the \
         {DERIVE_MAX_OUTPUT_BYTES}-byte per-node output cap"
    )
}

/// One derive call's walk budget (the kind's [`Deriver::walk_budget`]),
/// handed to the deriver as part of the call. The budget is COOPERATIVE:
/// it binds derivers that charge the rows, tree nodes, and outputs they
/// visit through [`DeriveCtx::charge`] — a deriver issuing raw SQL past
/// the ctx is not capped by it. An exhausted budget fails the call with
/// [`DeriveFailure::WalkBudget`] — an immediate structural quarantine,
/// since re-running the same node would walk the same graph. The wall
/// clock fails it differently: a call still running its deadline window
/// (default [`DERIVE_WALK_DEADLINE_NS`] ns past its creation) fails with
/// [`DeriveFailure::WalkDeadline`] on the next charge — RETRYABLE, the
/// measured units/sec riding on the error for the report row.
pub struct DeriveCtx {
    remaining: Cell<u64>,
    /// Units actually charged so far this call (the measurement the
    /// deadline reason records — budget units per wall-clock second).
    spent: Cell<u64>,
    /// The ctx's creation instant on the injected clock (the elapsed
    /// measurement's base).
    created_ns: u64,
    /// Absolute deadline on the state's injectable clock; the next charge
    /// once `(clock)() >= deadline_ns` fails the call.
    deadline_ns: u64,
    clock: ClockFn,
}

impl DeriveCtx {
    fn new(walk_budget: u64, clock: ClockFn, deadline_ns_window: u64) -> Self {
        let created_ns = clock();
        DeriveCtx {
            remaining: Cell::new(walk_budget),
            spent: Cell::new(0),
            created_ns,
            deadline_ns: created_ns.saturating_add(deadline_ns_window),
            clock,
        }
    }

    /// The budget left (tests and derivers that adapt their strategy).
    pub fn remaining(&self) -> u64 {
        self.remaining.get()
    }

    /// Units charged so far this call (the deadline overrun's measurement).
    pub fn spent(&self) -> u64 {
        self.spent.get()
    }

    /// Ns elapsed since the ctx's creation, on the injected clock.
    pub fn elapsed_ns(&self) -> u64 {
        (self.clock)().saturating_sub(self.created_ns)
    }

    /// Charges `n` visits; `Err` once the budget is exhausted (structural,
    /// [`DeriveFailure::WalkBudget`]) OR the call has run past its deadline
    /// window ([`DeriveFailure::WalkDeadline`], retryable — the failure
    /// carries the units/sec measurement the report row records). Charging
    /// zero still checks the deadline (a missed deadline fails any charge)
    /// but never fails on count: charging nothing cannot exhaust anything.
    pub fn charge(&self, n: u64) -> Result<(), DeriveFailure> {
        // The wall-clock backstop first: with the deadline missed, budget
        // headroom no longer matters — the slot has been held long enough.
        if (self.clock)() >= self.deadline_ns {
            self.remaining.set(0);
            return Err(DeriveFailure::WalkDeadline {
                units: self.spent.get(),
                elapsed_ns: self.elapsed_ns(),
            });
        }
        if n == 0 {
            return Ok(());
        }
        let left = self.remaining.get();
        if n >= left {
            self.remaining.set(0);
            return Err(DeriveFailure::WalkBudget);
        }
        self.remaining.set(left - n);
        self.spent.set(self.spent.get() + n);
        Ok(())
    }
}

/// The deadline-overrun report reason: the MEASURED leg of the retry
/// contract — units spent and wall-clock elapsed as units/sec, so a
/// mis-sized budget is diagnosed from the report row instead of guessed.
fn walk_deadline_reason(units: u64, elapsed_ns: u64) -> String {
    let secs = elapsed_ns as f64 / 1e9;
    let rate = if secs > 0.0 { units as f64 / secs } else { 0.0 };
    format!(
        "derive walk deadline exceeded: {units} units in {secs:.3}s \
         ({rate:.0} units/s); the lease is released and the node re-queued \
         (a deadline overrun retries, bounded — it is not the structural cap)"
    )
}

/// Why a derivation failed. `Fail` counts toward
/// [`DERIVE_MAX_ATTEMPTS`] (a transient deriver error may succeed on
/// retry) — and so does [`DeriveFailure::WalkDeadline`], the retryable
/// deadline overrun; `WalkBudget` is structural and quarantines
/// immediately.
#[derive(Debug, Clone)]
pub enum DeriveFailure {
    /// A deriver-reported failure — the reason rowed into
    /// `derive_reports` when the node is quarantined.
    Fail(PoisonReason),
    /// The per-call walk budget ran out (see [`DeriveCtx`]).
    WalkBudget,
    /// The wall-clock deadline overran with budget left — RETRYABLE
    /// (bounded attempts), carrying the measurement for the report row.
    WalkDeadline {
        /// Units charged before the deadline tripped.
        units: u64,
        /// Ns the call had run when the deadline tripped.
        elapsed_ns: u64,
    },
}

impl From<PoisonReason> for DeriveFailure {
    fn from(reason: PoisonReason) -> Self {
        DeriveFailure::Fail(reason)
    }
}

/// One parent's derived output, opaque to the framework (the bytes are
/// kind-specific — a kind's served-surface storage stays its own job; this
/// is the inter-node handoff channel). An empty payload is the documented
/// marker for "parent poison or unknown — proceed without it".
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DerivedOut(pub Vec<u8>);

impl DerivedOut {
    /// The empty marker handed to children of poison/unknown parents.
    pub fn empty() -> Self {
        DerivedOut(Vec::new())
    }

    pub fn as_bytes(&self) -> &[u8] {
        &self.0
    }
}

/// Why a derivation failed — the reason rowed into `derive_reports` when
/// the node is quarantined.
#[derive(Debug, Clone)]
pub struct PoisonReason(pub String);

impl PoisonReason {
    pub fn new(reason: impl Into<String>) -> Self {
        PoisonReason(reason.into())
    }
}

impl fmt::Display for PoisonReason {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

/// One derived-data kind. `derive` runs on the write-slot connection; it
/// may read any committed row and the enclosing transaction's own writes
/// (eager derivation sees the changeset being created), and it writes its
/// own kind-specific storage idempotently (keyed upserts — the framework
/// re-runs derivations after lost leases).
pub trait Deriver: Send + Sync {
    /// The registry name — stable across processes (it keys the
    /// `derive_kinds` row and the kind id).
    fn kind(&self) -> &'static str;

    /// Eager kinds derive inside the changeset's write transaction;
    /// deferred (the default) enqueue a pending row for the queue.
    fn eager(&self) -> bool {
        false
    }

    /// This kind's per-call COOPERATIVE walk budget (see
    /// [`DEFAULT_WALK_BUDGET`] for the bootstrap default and its
    /// justification). It binds derivers that charge their visits through
    /// `ctx.charge`; a deriver issuing raw SQL past the ctx is not capped
    /// by it. Override with a comment justifying the value.
    fn walk_budget(&self) -> u64 {
        DEFAULT_WALK_BUDGET
    }

    /// Derives this node from its parents' contributions, in parent `ord`
    /// order. A [`DeriveFailure::Fail`] or [`DeriveFailure::WalkDeadline`]
    /// (the wall-clock backstop — retryable, measured) burns one attempt
    /// and quarantines the node after [`DERIVE_MAX_ATTEMPTS`]; a
    /// [`DeriveFailure::WalkBudget`] (the [`DeriveCtx`] budget ran out)
    /// quarantines immediately. The framework reads the poison taint off
    /// the same contributions — a node that derived on an empty-marker
    /// parent records done WITH taint, never as silently-exact data.
    fn derive(
        &self,
        ctx: &DeriveCtx,
        conn: &Db,
        repo_id: i64,
        cs: Id,
        parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure>;
}

/// One parent's contribution to a child derivation: the output the child
/// receives (the empty marker when the parent is poisoned, pending, or
/// row-less), whether the parent row is ITSELF poison, and the parent's
/// inherited taint. The framework derives the child's own taint from this
/// slice — see [`collapse_taint`] — and the deriver may read it too.
#[derive(Debug, Clone)]
pub struct ParentOut {
    pub output: DerivedOut,
    /// The parent row is state='poison' (this parent is itself the
    /// nearest poisoned ancestor for the child).
    pub poisoned: bool,
    /// The parent derived on an empty-marker parent somewhere up ITS
    /// ancestor set.
    pub tainted: bool,
    /// The parent's nearest poisoned ancestor, when tainted.
    pub poison_cs: Option<Id>,
}

/// Collapses the parents' contributions into the child's own taint:
/// tainted when ANY parent carried taint (directly poisoned parents are
/// tainted by definition); `poison_cs` names the NEAREST poisoned
/// ancestor — a directly poisoned first parent (in `ord` order) outranks
/// an inherited one, being one edge closer.
pub(crate) fn collapse_taint(parents: &[ParentOut]) -> (bool, Option<Id>) {
    if let Some(p) = parents.iter().find(|p| p.poisoned) {
        return (true, p.poison_cs);
    }
    match parents.iter().find(|p| p.tainted) {
        Some(p) => (true, p.poison_cs),
        None => (false, None),
    }
}

/// A registered kind: the deriver plus its stable `derive_kinds` id
/// (resolved at registration).
pub struct RegisteredKind {
    pub name: &'static str,
    pub id: i64,
    pub deriver: Arc<dyn Deriver>,
}

/// The process's registered kinds, in registration order. Built empty; a
/// kind name registers at most once (a collision would mean two derivers
/// sharing one id — a boot-time programming error, not a runtime event).
/// `Clone` is cheap (Arc per kind) — write closures carry their own handle.
#[derive(Clone, Default)]
pub struct DeriveRegistry {
    kinds: Vec<Arc<RegisteredKind>>,
}

impl DeriveRegistry {
    /// Registers `kind`; panics on a duplicate name (see above).
    pub fn register(&mut self, kind: RegisteredKind) {
        if self.kinds.iter().any(|k| k.name == kind.name) {
            panic!("derive kind {:?} registered twice", kind.name);
        }
        self.kinds.push(Arc::new(kind));
    }

    /// The registered kinds, registration order.
    pub fn kinds(&self) -> &[Arc<RegisteredKind>] {
        &self.kinds
    }

    pub fn by_name(&self, name: &str) -> Option<&Arc<RegisteredKind>> {
        self.kinds.iter().find(|k| k.name == name)
    }

    pub fn is_empty(&self) -> bool {
        self.kinds.is_empty()
    }
}

/// The eager derive path's environment, threaded through
/// [`enqueue_changeset`] as one bundle: the write's timestamp, the
/// injected clock, and the wall-clock deadline window (ns) the eager
/// [`DeriveCtx`] runs under.
pub struct EagerEnv {
    pub now_ns: i64,
    pub clock: ClockFn,
    pub walk_deadline_ns: u64,
}

/// Enqueues (and for eager kinds, derives) one freshly written changeset
/// for every registered kind. Runs INSIDE the changeset's write
/// transaction: a rollback takes the pending rows back with it, and eager
/// derivations see the changeset atomically. A failed eager attempt is a
/// failed first attempt (back to pending for the queue), never a create
/// failure — the plan's never-blocks rule holds for every kind.
pub fn enqueue_changeset(
    conn: &Db,
    registry: &DeriveRegistry,
    repo_id: i64,
    cs: Id,
    parents: &[Id],
    env: EagerEnv,
) -> Result<(), DbError> {
    for kind in &registry.kinds {
        if kind.deriver.eager() {
            // The caps gate the eager path exactly as they gate the queue:
            // an exhausted walk budget or an over-cap output quarantines
            // the node immediately instead of deriving; a missed
            // wall-clock deadline is a FAILED ATTEMPT (the queue retries
            // it, bounded), and an eager `Err` likewise. All inside the
            // create's transaction, so the record is atomic with the
            // write.
            let outs = db::derive_parent_outputs(conn, kind.id, repo_id, parents)?;
            let (tainted, poison_cs) = collapse_taint(&outs);
            let ctx = DeriveCtx::new(
                kind.deriver.walk_budget(),
                env.clock.clone(),
                env.walk_deadline_ns,
            );
            let now_ns = env.now_ns;
            match kind.deriver.derive(&ctx, conn, repo_id, cs, &outs) {
                Ok(out) if out.as_bytes().len() > DERIVE_MAX_OUTPUT_BYTES => {
                    db::derive_poison_now(
                        conn,
                        kind.id,
                        repo_id,
                        &cs,
                        &size_cap_reason(out.as_bytes().len()),
                        now_ns,
                    )?;
                }
                Ok(out) => db::derive_state_insert_done(
                    conn,
                    kind.id,
                    repo_id,
                    &cs,
                    out.as_bytes(),
                    tainted,
                    poison_cs.as_ref(),
                )?,
                Err(DeriveFailure::WalkBudget) => {
                    let budget = kind.deriver.walk_budget();
                    db::derive_poison_now(
                        conn,
                        kind.id,
                        repo_id,
                        &cs,
                        &walk_budget_reason(budget),
                        now_ns,
                    )?;
                }
                Err(DeriveFailure::WalkDeadline { units, elapsed_ns }) => {
                    // Retryable: a failed attempt (bounded), and the measured
                    // reason rowed — an eager overrun is a create-time fact
                    // worth the report row even when it never quarantines.
                    let reason = walk_deadline_reason(units, elapsed_ns);
                    let poisoned =
                        db::derive_fail_attempt(conn, kind.id, repo_id, &cs, &reason, now_ns)?;
                    if !poisoned {
                        db::derive_report(conn, kind.id, repo_id, &cs, &reason, now_ns)?;
                    }
                }
                Err(DeriveFailure::Fail(reason)) => {
                    db::derive_fail_attempt(conn, kind.id, repo_id, &cs, &reason.0, now_ns)?;
                }
            }
        } else {
            db::derive_enqueue_pending(conn, kind.id, repo_id, &cs)?;
        }
    }
    Ok(())
}

/// The per-(kind, changeset) derivation state — the query M6b's read
/// routes build the 503 `derivation_pending` / `Retry-After` contract on.
/// `None`: the kind is not registered, or the repo/changeset has no row.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DeriveStatus {
    /// Queued, not yet derived — the read-during-backlog 503 case.
    Pending,
    /// A lease is held (derivation in flight, or its holder just died and
    /// the sweep has not run yet — also a 503 for M6b's routes).
    Deriving,
    /// Derived; carries the framework-stored output.
    Done(DerivedOut),
    /// Quarantined — [`DERIVE_MAX_ATTEMPTS`] failed attempts, or one of
    /// the structural caps (walk budget, output size) immediately; the
    /// reason is in `derive_reports`.
    Poison,
}

/// The framework's "is derived / is pending" lookup for one (kind, repo,
/// changeset).
pub async fn status(
    state: &AppState,
    kind: &str,
    repo_name: &str,
    cs: Id,
) -> Result<Option<DeriveStatus>, ApiError> {
    let Some(registered) = state.derive.by_name(kind) else {
        return Ok(None);
    };
    let kind_id = registered.id;
    let repo_name = repo_name.to_string();
    state
        .read(move |conn| {
            let Some(repo_id) =
                db::repo_id_by_name(conn, &repo_name).map_err(ApiError::log_internal)?
            else {
                return Ok(None);
            };
            let row =
                db::derive_get(conn, kind_id, repo_id, &cs).map_err(ApiError::log_internal)?;
            Ok(row.map(|row| match row.state.as_str() {
                "pending" => DeriveStatus::Pending,
                "deriving" => DeriveStatus::Deriving,
                "done" => DeriveStatus::Done(DerivedOut(row.output.unwrap_or_default())),
                // The CHECK constraint admits exactly these four states.
                _ => DeriveStatus::Poison,
            }))
        })
        .await
}

/// Boot-time backfill (the plan's deferred-backfill check): for every
/// registered kind, enqueue every changeset that has no row yet — the
/// changesets a registry-less process wrote (offline import) or a
/// pre-derive database carried (in-place upgrade). `serve` runs this as a
/// background task beside the queue (a large repo must not delay accepting
/// connections), in batches ([`BACKFILL_BATCH`], or the state's injected
/// `derive_backfill_batch_size`) — each batch one write slot, the slot
/// yielded between batches so requests interleave with the backfill. Every
/// batch scans forward from a (generation, rowid) cursor
/// ([`crate::db::derive_backfill_batch`]), so a queue draining
/// concurrently always sees parents rowed before children.
///
/// Per kind the task batches, then sets the completion marker
/// ([`crate::db::derive_backfill_done_set`]): until the marker is set,
/// derived reads stay on the 503 `derivation_pending` contract
/// ([`crate::db::derive_any_pending`]) — a FAILED backfill never sets it,
/// so those reads stay 503 until a boot completes the backfill. The marker
/// lands only when the anti-join is empty: one final
/// [`crate::db::derive_backfill_rowless_exists`] probe proves every
/// changeset has its row before the marker is written — a batch loop that
/// exited early (or a dropped cursor) leaves the marker unset and the
/// reads on the 503 contract, never answering over a hole. The markers are
/// cleared before this task runs ([`backfill_clear_markers`], `serve`'s
/// synchronous pre-accept step). Idempotent: a reboot with a drained queue
/// re-enqueues nothing and re-sets the marker.
pub async fn backfill(state: &AppState) -> Result<(), ApiError> {
    if state.derive.is_empty() {
        return Ok(());
    }
    // Test-only A/B hook (ZERO in production): widen the boot race window
    // deterministically before the first batch lands.
    let delay = state.derive_backfill_delay;
    if !delay.is_zero() {
        tokio::time::sleep(delay).await;
    }
    let batch_limit = state.derive_backfill_batch_size;
    let kinds: Vec<(i64, &'static str)> = state
        .derive
        .kinds()
        .iter()
        .map(|k| (k.id, k.name))
        .collect();
    for (kind_id, kind_name) in kinds {
        let mut cursor = db::BackfillCursor::start();
        loop {
            let (inserted, next) = backfill_step(|| async move {
                state
                    .write(move |store| {
                        db::derive_backfill_batch(store.conn(), kind_id, cursor, batch_limit)
                            .map_err(ApiError::log_internal)
                    })
                    .await
            })
            .await?;
            cursor = next;
            if inserted == 0 {
                break;
            }
        }
        // The marker's honesty check: the marker lands only when the
        // anti-join is EMPTY. A zero-insert batch means the cursor range is
        // exhausted; this probe proves no changeset anywhere lacks its row
        // (it would catch a one-batch loop exit or a dropped cursor, which
        // would otherwise set the marker over unrowed changesets and let
        // reads answer 200 over a hole). Fail-safe direction: no marker,
        // reads stay 503 until a boot completes the backfill.
        let rowless = backfill_step(|| async move {
            state
                .read(move |conn| {
                    db::derive_backfill_rowless_exists(conn, kind_id)
                        .map_err(ApiError::log_internal)
                })
                .await
        })
        .await?;
        if rowless {
            return Err(ApiError::log_internal(format!(
                "backfill for kind {kind_name:?} finished with unrowed changesets; \
                 the completion marker is withheld"
            )));
        }
        // The anti-join is empty: publish the marker the reads gate on.
        let completed_ns = state.now_ns() as i64;
        backfill_step(|| async {
            state
                .write(move |store| {
                    db::derive_backfill_done_set(store.conn(), kind_name, completed_ns)
                        .map_err(ApiError::log_internal)
                })
                .await
        })
        .await?;
    }
    Ok(())
}

/// Clears every registered kind's boot-backfill completion marker (one
/// transaction, one DELETE per kind). `serve` runs this SYNCHRONOUSLY,
/// before the accept loop and the backfill task: a marker left from a
/// previous boot must never survive into the new boot's unrowed window —
/// an offline import (which runs under the same data-dir flock, i.e. only
/// while the server is down) may have added changesets with no derive
/// rows, and a stale marker would let reads skip the 503
/// `derivation_pending` contract and answer from the incomplete index. A
/// kind registered for the first time has no marker at all — same rule:
/// its reads stay 503 until its first backfill completes.
pub async fn backfill_clear_markers(state: &AppState) -> Result<(), ApiError> {
    if state.derive.is_empty() {
        return Ok(());
    }
    let names: Vec<&'static str> = state.derive.kinds().iter().map(|k| k.name).collect();
    // Busy-retried like every backfill step: `serve` propagates an error
    // here as a failed boot, and serve_until_quiet-style callers would
    // never see it — ordinary boot contention must not cost the queue and
    // backfill tasks their spawn.
    backfill_step(|| {
        let names = names.clone();
        async move {
            state
                .write(move |store| {
                    let conn = store.conn();
                    for name in &names {
                        db::derive_backfill_done_clear(conn, name)
                            .map_err(ApiError::log_internal)?;
                    }
                    Ok(())
                })
                .await
        }
    })
    .await
}

/// Busy-write retries before a backfill step gives up and propagates (the
/// step has waited 10 s at 1,000 × 10 ms — serve's failure log fires and
/// reads stay 503 until a boot completes the backfill).
const BACKFILL_BUSY_RETRIES: u32 = 1_000;

/// One backfill write under the backfill's busy policy: ordinary
/// contention is waited out (once rows exist, the queue drains
/// concurrently, and the backfill must survive it to finish), and only
/// then propagated. `make` rebuilds the write future per attempt — the
/// `state.write` future is single-shot.
async fn backfill_step<T, F, Fut>(mut make: F) -> Result<T, ApiError>
where
    T: Send + 'static,
    Fut: Future<Output = Result<T, ApiError>> + Send,
    F: FnMut() -> Fut,
{
    let mut attempts = 0u32;
    loop {
        match make().await {
            Ok(v) => return Ok(v),
            Err(e) if e.code() == ApiCode::WriterBusy => {
                attempts += 1;
                if attempts > BACKFILL_BUSY_RETRIES {
                    return Err(e);
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            Err(e) => return Err(e),
        }
    }
}

/// What one queue pass did (for logs and tests).
#[derive(Debug, Default, PartialEq, Eq)]
pub struct PassReport {
    /// Expired leases swept (back to pending or on to poison).
    pub swept: usize,
    /// Nodes whose derivation recorded done.
    pub derived: usize,
    /// Nodes quarantined this pass (deriver `Err` at the cap, or a sweep
    /// expiry at the cap).
    pub poisoned: usize,
    /// Deadline overruns this pass (the retryable walk-deadline failures;
    /// each released its lease and burned one attempt, reason rowed with
    /// the measured units/sec — quarantining only at the attempt cap).
    pub deadline_overruns: usize,
}

impl std::ops::AddAssign for PassReport {
    fn add_assign(&mut self, rhs: Self) {
        self.swept += rhs.swept;
        self.derived += rhs.derived;
        self.poisoned += rhs.poisoned;
        self.deadline_overruns += rhs.deadline_overruns;
    }
}

/// Runs one derive pass over every registered kind: sweep expired leases,
/// then drain the eligible set (oldest pending with parents done-or-poison,
/// one at a time). A busy write slot (`writer_busy`) ends the pass early —
/// the queue never fights a request for the single writer; the next tick
/// resumes. A pass that already holds a lease when the slot turns busy
/// releases it straight back to pending first ([`drain`] →
/// [`release_lease_after_busy`]): contention is not a derivation failure,
/// so no attempt burns and no lease period is lost. A deriver PANIC
/// propagates as an error (the lease stays held and the expiry sweep
/// re-runs the node — that is the crash-mid-derivation path, and it is
/// idempotent by contract).
pub async fn derive_pass(state: &AppState) -> Result<PassReport, ApiError> {
    let mut report = PassReport::default();
    for kind in state.derive.kinds() {
        report += sweep_expired(state, kind).await?;
        drain(state, kind, &mut report).await?;
    }
    Ok(report)
}

/// Sweeps the kind's expired leases: each expired holder loses its lease
/// and burns one attempt (`lease expired` as the reason when it poisons).
/// Read-probe first, exactly like the snapshot-GC pass: a quiet server's
/// periodic sweep must never occupy the single-writer slot (that would
/// 503 concurrent writes); the write transaction runs only when there is
/// something to sweep.
async fn sweep_expired(
    state: &AppState,
    kind: &Arc<RegisteredKind>,
) -> Result<PassReport, ApiError> {
    let now = state.now_ns() as i64;
    let probe_kind = Arc::clone(kind);
    let has_expired = {
        let kind_id = probe_kind.id;
        state
            .read(move |conn| {
                Ok::<bool, ApiError>(
                    !db::derive_expired_leases(conn, kind_id, now)
                        .map_err(ApiError::log_internal)?
                        .is_empty(),
                )
            })
            .await?
    };
    if !has_expired {
        return Ok(PassReport::default());
    }
    let kind = Arc::clone(kind);
    state
        .write(move |store| {
            let conn = store.conn();
            let tx = conn.transaction().map_err(ApiError::log_internal)?;
            let expired =
                db::derive_expired_leases(&tx, kind.id, now).map_err(ApiError::log_internal)?;
            let mut poisoned = 0usize;
            for (repo_id, cs) in &expired {
                if db::derive_fail_attempt(&tx, kind.id, *repo_id, cs, "lease expired", now)
                    .map_err(ApiError::log_internal)?
                {
                    poisoned += 1;
                }
            }
            tx.commit().map_err(ApiError::log_internal)?;
            Ok(PassReport {
                swept: expired.len(),
                poisoned,
                ..PassReport::default()
            })
        })
        .await
        .or_else(|e| {
            if e.code() == ApiCode::WriterBusy {
                Ok(PassReport::default())
            } else {
                Err(e)
            }
        })
}

/// Busy-write retries before a post-abort lease release gives up and
/// leaves the lease to the expiry sweep (the backstop, now genuinely for
/// crashed holders). The release is one tiny UPDATE and contention
/// episodes are transaction-length, so 100 × 10 ms is far past anything
/// real; the bound exists so a pathological writer can never spin the
/// queue task here forever.
const LEASE_RELEASE_BUSY_RETRIES: u32 = 100;

/// Releases the pass's held lease after a mid-pass `writer_busy` abort:
/// state → pending, lease cleared, attempts UNCHANGED. The abort is
/// write-slot contention, not a derivation failure — the old behavior
/// held the lease to expiry, so every collision cost a full lease period
/// of queue latency and the expiry sweep burned an ATTEMPT on pure
/// contention: one collision was the queue_lifecycle flake (a derived
/// node showing attempts=1 against the attempts==0 pin), and three
/// consecutive collisions quarantined a healthy node outright. The
/// release write itself can lose the slot race that aborted the pass, so
/// it busy-retries; a final give-up leaves the lease held for the expiry
/// sweep (the idempotent re-run). `lease_expires` is the exact value this
/// pass set at lease time — the release matches on it, so a row swept or
/// re-leased in between is left alone.
async fn release_lease_after_busy(
    state: &AppState,
    kind: &Arc<RegisteredKind>,
    repo_id: i64,
    cs: &Id,
    lease_expires: i64,
) {
    let mut tries = 0u32;
    loop {
        let kind = Arc::clone(kind);
        let cs = *cs;
        let busy_release = Arc::clone(&state.derive_test_busy_abort);
        match state
            .write(move |store| {
                // Test-only one-shot busy injection on the release itself
                // (the `BusyAbort::Release` shot armed by
                // `DeriveThenRelease`): the helper's own retry loop is the
                // path under test — the second attempt must land with no
                // attempt burned.
                let fire = {
                    let mut hook = busy_release
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner);
                    if matches!(*hook, Some(BusyAbort::Release(id)) if id == cs) {
                        *hook = None;
                        true
                    } else {
                        false
                    }
                };
                if fire {
                    return Err(ApiError::new(ApiCode::WriterBusy, "writer busy").retryable(true));
                }
                db::derive_release_lease(store.conn(), kind.id, repo_id, &cs, lease_expires)
                    .map_err(ApiError::log_internal)
            })
            .await
        {
            Ok(_) => return,
            Err(e) if e.code() == ApiCode::WriterBusy => {
                tries += 1;
                if tries > LEASE_RELEASE_BUSY_RETRIES {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            // Any other error is traced by `log_internal`; the expiry
            // sweep recovers the row the same way it recovers a crash.
            Err(_) => return,
        }
    }
}

/// Drains the kind's currently-eligible set: pick → lease → derive →
/// record, until nothing is eligible or the write slot turns busy. Each
/// node costs its own write transactions (lease; the deriver's own writes;
/// the done-record) so requests interleave between them.
async fn drain(
    state: &AppState,
    kind: &Arc<RegisteredKind>,
    report: &mut PassReport,
) -> Result<(), ApiError> {
    let lease_ns = (state.derive_queue_interval * 2).as_nanos() as i64;
    loop {
        // Pick on the read pool: no write-slot contention for the scan.
        let kind_for_pick = Arc::clone(kind);
        let Some((repo_id, cs)) = state
            .read(move |conn| {
                db::derive_pick_next(conn, kind_for_pick.id).map_err(ApiError::log_internal)
            })
            .await?
        else {
            return Ok(());
        };

        // Lease. A stale pick (someone else took it — impossible within one
        // process, tolerated anyway) just falls through to the next pick.
        let now = state.now_ns() as i64;
        let lease_expires = now.saturating_add(lease_ns);
        let kind_for_lease = Arc::clone(kind);
        let leased = match state
            .write(move |store| {
                db::derive_lease(store.conn(), kind_for_lease.id, repo_id, &cs, lease_expires)
                    .map_err(ApiError::log_internal)
            })
            .await
        {
            Ok(leased) => leased,
            // Write slot mid-request: end the pass, the next tick resumes
            // (the queue never fights a request for the single writer).
            Err(e) if e.code() == ApiCode::WriterBusy => return Ok(()),
            Err(e) => return Err(e),
        };
        if !leased {
            continue;
        }

        // Derive: the deriver runs on the write-slot connection, writes its
        // own storage, and returns the output — the seam between its writes
        // and the framework's done-record is a real crash point, covered by
        // the re-run idempotency contract.
        let kind_for_derive = Arc::clone(kind);
        let busy_abort = Arc::clone(&state.derive_test_busy_abort);
        let clock_for_derive = Arc::clone(&state.clock);
        let deadline_window = state.derive_walk_deadline.as_nanos() as u64;
        let derived = state
            .write(move |store| {
                // Test-only one-shot writer_busy injection (see
                // `ServerState::derive_test_busy_abort`): the pass sees
                // exactly what a lost write-slot race produces, lease held.
                let fire = {
                    let mut hook = busy_abort
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner);
                    match *hook {
                        Some(BusyAbort::Derive(id)) if id == cs => {
                            *hook = None;
                            true
                        }
                        // The double-collision shape: arm the release shot
                        // before aborting, so the release's FIRST attempt
                        // loses the slot race too.
                        Some(BusyAbort::DeriveThenRelease(id)) if id == cs => {
                            *hook = Some(BusyAbort::Release(id));
                            true
                        }
                        _ => false,
                    }
                };
                if fire {
                    return Err(ApiError::new(ApiCode::WriterBusy, "writer busy").retryable(true));
                }
                let conn = store.conn();
                let parents = db::changeset_graph(conn, repo_id, &cs)
                    .map_err(ApiError::log_internal)?
                    .map(|g| g.parents)
                    // The changeset row is append-only and existed at
                    // enqueue time; an absent graph would mean a broken
                    // store, and an empty parent list is the honest input
                    // then (the same shape a parentless node has).
                    .unwrap_or_default();
                let outs = db::derive_parent_outputs(conn, kind_for_derive.id, repo_id, &parents)
                    .map_err(ApiError::log_internal)?;
                let ctx = crate::derive::DeriveCtx::new(
                    kind_for_derive.deriver.walk_budget(),
                    clock_for_derive,
                    deadline_window,
                );
                // Inner result on purpose: a deriver failure is a FAILED
                // NODE (handled below), not an API error — the outer error
                // type stays the write-slot's.
                let taint = collapse_taint(&outs);
                match kind_for_derive
                    .deriver
                    .derive(&ctx, conn, repo_id, cs, &outs)
                {
                    Ok(out) => Ok(Ok((cs, out, taint))),
                    Err(failure) => Ok(Err(failure)),
                }
            })
            .await;
        let outcome = match derived {
            Ok(outcome) => outcome,
            Err(e) if e.code() == ApiCode::WriterBusy => {
                // Contention, not a derivation failure: release the lease
                // straight back to pending (no attempt burned) and end the
                // pass — the next tick re-picks the node.
                release_lease_after_busy(state, kind, repo_id, &cs, lease_expires).await;
                return Ok(());
            }
            // A deriver PANIC lands here as a join error: the lease stays
            // held, the expiry sweep re-runs the node (idempotency
            // contract), and the pass reports the failure to its caller.
            Err(e) => return Err(e),
        };
        let (cs, out, taint) = match outcome {
            Ok(triple) => triple,
            Err(failure) => {
                // Structural failure (walk budget): immediate quarantine —
                // retrying the same node would walk the same graph. A
                // plain `Fail`, or the retryable deadline overrun, is a
                // failed attempt: attempts+1, poison + report at the cap
                // (the deadline reason carries the measured units/sec).
                // Either way one transaction puts the row and (when
                // poisoned) its report in together.
                let now = state.now_ns() as i64;
                let kind_for_fail = Arc::clone(kind);
                let is_deadline = matches!(failure, DeriveFailure::WalkDeadline { .. });
                let outcome = match &failure {
                    DeriveFailure::WalkBudget => {
                        let reason = walk_budget_reason(kind_for_fail.deriver.walk_budget());
                        state
                            .write(move |store| {
                                let conn = store.conn();
                                let tx = conn.transaction().map_err(ApiError::log_internal)?;
                                db::derive_poison_now(
                                    &tx,
                                    kind_for_fail.id,
                                    repo_id,
                                    &cs,
                                    &reason,
                                    now,
                                )
                                .map_err(ApiError::log_internal)?;
                                tx.commit().map_err(ApiError::log_internal)?;
                                Ok(None::<bool>)
                            })
                            .await
                    }
                    failure => {
                        let reason = match failure {
                            DeriveFailure::WalkDeadline { units, elapsed_ns } => {
                                walk_deadline_reason(*units, *elapsed_ns)
                            }
                            DeriveFailure::Fail(reason) => reason.0.clone(),
                            DeriveFailure::WalkBudget => unreachable!("matched above"),
                        };
                        let is_deadline_row = is_deadline;
                        state
                            .write(move |store| {
                                let conn = store.conn();
                                let tx = conn.transaction().map_err(ApiError::log_internal)?;
                                let poisoned = db::derive_fail_attempt(
                                    &tx,
                                    kind_for_fail.id,
                                    repo_id,
                                    &cs,
                                    &reason,
                                    now,
                                )
                                .map_err(ApiError::log_internal)?;
                                if is_deadline_row && !poisoned {
                                    // A non-terminal overrun rows its measured
                                    // reason too (retryable, but the units/sec
                                    // measurement must be readable without
                                    // waiting for a quarantine); the cap arm
                                    // of the fail-attempt rows its own.
                                    db::derive_report(
                                        &tx,
                                        kind_for_fail.id,
                                        repo_id,
                                        &cs,
                                        &reason,
                                        now,
                                    )
                                    .map_err(ApiError::log_internal)?;
                                }
                                tx.commit().map_err(ApiError::log_internal)?;
                                Ok(Some(poisoned))
                            })
                            .await
                    }
                };
                match outcome {
                    // `None` = the immediate WalkBudget quarantine; `Some`
                    // = a failed attempt that poisons only at the cap.
                    Ok(quarantined) => {
                        let quarantined = quarantined.unwrap_or(true);
                        report.poisoned += usize::from(quarantined);
                        if is_deadline {
                            report.deadline_overruns += 1;
                            if !quarantined {
                                // The failed attempt already released the
                                // lease back to pending: END THE PASS. The
                                // next tick re-picks the node (retries are
                                // bounded by the attempt cap); staying in
                                // the drain would re-lease the same slow
                                // walk immediately and burn all three
                                // attempts — three deadline windows of
                                // write-slot hold — inside one pass.
                                return Ok(());
                            }
                        }
                    }
                    Err(e) if e.code() == ApiCode::WriterBusy => {
                        // The real failure could not be recorded yet:
                        // release to pending (no attempt burned) — the
                        // next tick re-derives, re-fails, and records the
                        // attempt then. (No direct pin: the hook aborts
                        // only the mid-derivation write; this arm is the
                        // same `release_lease_after_busy` call that pin
                        // exercises, at a different write.)
                        release_lease_after_busy(state, kind, repo_id, &cs, lease_expires).await;
                        return Ok(());
                    }
                    Err(e) => return Err(e),
                };
                continue;
            }
        };

        // Size cap (immediate, structural — see the module doc): an output
        // over DERIVE_MAX_OUTPUT_BYTES is never recorded; the node is
        // quarantined with the size reason and the children proceed on the
        // empty marker.
        if out.as_bytes().len() > DERIVE_MAX_OUTPUT_BYTES {
            let now = state.now_ns() as i64;
            let reason = size_cap_reason(out.as_bytes().len());
            let kind_for_poison = Arc::clone(kind);
            match state
                .write(move |store| {
                    let conn = store.conn();
                    let tx = conn.transaction().map_err(ApiError::log_internal)?;
                    db::derive_poison_now(&tx, kind_for_poison.id, repo_id, &cs, &reason, now)
                        .map_err(ApiError::log_internal)?;
                    tx.commit().map_err(ApiError::log_internal)?;
                    Ok(())
                })
                .await
            {
                Ok(()) => (),
                Err(e) if e.code() == ApiCode::WriterBusy => {
                    // The quarantine could not be written: release to
                    // pending — the next tick re-derives, trips the cap
                    // again, and quarantines then. (No direct pin: the
                    // hook aborts only the mid-derivation write; this arm
                    // is the same `release_lease_after_busy` call that pin
                    // exercises, at a different write.)
                    release_lease_after_busy(state, kind, repo_id, &cs, lease_expires).await;
                    return Ok(());
                }
                Err(e) => return Err(e),
            };
            report.poisoned += 1;
            continue;
        }

        // Record done (the second transaction — see the module doc). A busy
        // slot releases the lease back to pending; the next tick re-runs
        // the (idempotent) derivation and records then.
        let kind_for_record = Arc::clone(kind);
        let recorded = match state
            .write(move |store| {
                db::derive_record_done(
                    store.conn(),
                    kind_for_record.id,
                    repo_id,
                    &cs,
                    out.as_bytes(),
                    taint.0,
                    taint.1.as_ref(),
                )
                .map_err(ApiError::log_internal)
            })
            .await
        {
            Ok(recorded) => recorded,
            Err(e) if e.code() == ApiCode::WriterBusy => {
                // The output is derived (the deriver's own writes are
                // idempotent upserts) but not recorded: release to pending
                // — the next tick re-derives and records, instead of the
                // node holding a lease for a full lease period. (No direct
                // pin: the hook aborts only the mid-derivation write; this
                // arm is the same `release_lease_after_busy` call that pin
                // exercises, at a different write.)
                release_lease_after_busy(state, kind, repo_id, &cs, lease_expires).await;
                return Ok(());
            }
            Err(e) => return Err(e),
        };
        if recorded {
            report.derived += 1;
        }
    }
}
