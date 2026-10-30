//! The generic derived-data framework (M6a): recursive computation from
//! parents, eager-or-deferred per kind, a background derive queue with
//! leases, and poison-node quarantine — a malformed node is skipped and
//! reported, never head-of-line blocks.
//!
//! A kind implements [`Deriver`] and registers on
//! [`crate::state::ServerState::register_deriver`] (M6b registers the real
//! kinds; production M6a ships none, and `derive_kinds` stays empty until
//! then). The contract, per kind:
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
//! **Depth/size caps.** Two deriver-independent caps guard ONE derive call
//! (plan: "depth/size caps"): [`DERIVE_MAX_DEPTH`] bounds the first-parent
//! ancestor levels a node's chain offers a recursive deriver, and
//! [`DERIVE_MAX_OUTPUT_BYTES`] bounds the framework-carried output per
//! node. What they guard is runaway recursion and unbounded output, NOT
//! long history: a deep chain derives batch-by-batch THROUGH this queue —
//! each node is a batch of one, the running output carried in the
//! parents' outputs — and that iterative pattern has no depth limit. A
//! node that trips either cap is quarantined IMMEDIATELY
//! ([`DERIVE_MAX_ATTEMPTS`] does not apply — the caps are structural, so
//! retrying cannot help), rows a `derive_reports` reason, and its children
//! still derive on the empty marker (the never-head-of-line rule above).
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

use std::fmt;
use std::sync::Arc;

use rusqlite::Connection;

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::db;
use crate::state::AppState;

/// Failed attempts before a node is quarantined: three strikes — a deriver
/// `Err` or a lease expiry each count one — then poison + a
/// `derive_reports` row (the plan's "skipped and reported").
pub const DERIVE_MAX_ATTEMPTS: i64 = 3;

/// Depth cap: the first-parent ancestor levels one derive call may span.
/// A node whose first-parent chain is deeper is quarantined immediately —
/// a deriver recursing that chain in a single call could not survive it.
/// Long history is NOT capped: it derives batch-by-batch through the queue
/// (each node a batch, the running output carried in the parents'
/// outputs). The exact check is [`crate::db::derive_over_depth`]; the
/// 1,000-ancestor default is an order-of-magnitude headroom over any
/// plausible single-call recursion.
pub const DERIVE_MAX_DEPTH: u64 = 1_000;

/// Size cap on the framework-carried output of one node, matching the
/// 16 MiB canonical-encoding cap. An over-cap output is never recorded —
/// the node is quarantined immediately with the size reason.
pub const DERIVE_MAX_OUTPUT_BYTES: usize = 16 * 1024 * 1024;

/// The quarantine reason for the depth cap (shared by the queue and eager
/// paths so reports read identically wherever the cap fires).
fn depth_cap_reason() -> String {
    format!(
        "derive depth cap exceeded: first-parent chain deeper than \
         {DERIVE_MAX_DEPTH} ancestor levels; deep history must be derived \
         iteratively per node through the queue"
    )
}

/// The quarantine reason for the size cap.
fn size_cap_reason(len: usize) -> String {
    format!(
        "derive output cap exceeded: {len} bytes exceeds the \
         {DERIVE_MAX_OUTPUT_BYTES}-byte per-node output cap"
    )
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

    /// Derives this node from its parents' outputs, in parent `ord` order.
    /// `Err` poisons the node after [`DERIVE_MAX_ATTEMPTS`] attempts.
    fn derive(
        &self,
        conn: &Connection,
        repo_id: i64,
        cs: Id,
        parents_done: &[DerivedOut],
    ) -> Result<DerivedOut, PoisonReason>;
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

/// Enqueues (and for eager kinds, derives) one freshly written changeset
/// for every registered kind. Runs INSIDE the changeset's write
/// transaction: a rollback takes the pending rows back with it, and eager
/// derivations see the changeset atomically. A failed eager attempt is a
/// failed first attempt (back to pending for the queue), never a create
/// failure — the plan's never-blocks rule holds for every kind.
pub fn enqueue_changeset(
    conn: &Connection,
    registry: &DeriveRegistry,
    repo_id: i64,
    cs: Id,
    parents: &[Id],
    now_ns: i64,
) -> rusqlite::Result<()> {
    for kind in &registry.kinds {
        if kind.deriver.eager() {
            // The caps gate the eager path exactly as they gate the queue:
            // an over-depth chain or an over-cap output quarantines the
            // node immediately instead of deriving (inside the create's
            // transaction, so the quarantine is atomic with the write).
            if db::derive_over_depth(conn, repo_id, &cs, DERIVE_MAX_DEPTH)? {
                db::derive_poison_now(conn, kind.id, repo_id, &cs, &depth_cap_reason(), now_ns)?;
                continue;
            }
            let outs = db::derive_parent_outputs(conn, kind.id, repo_id, parents)?
                .into_iter()
                .map(|o| o.map(DerivedOut).unwrap_or_else(DerivedOut::empty))
                .collect::<Vec<_>>();
            match kind.deriver.derive(conn, repo_id, cs, &outs) {
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
                Ok(out) => {
                    db::derive_state_insert_done(conn, kind.id, repo_id, &cs, out.as_bytes())?
                }
                Err(reason) => {
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
    /// the structural caps (depth, output size) immediately; the reason is
    /// in `derive_reports`.
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
}

impl std::ops::AddAssign for PassReport {
    fn add_assign(&mut self, rhs: Self) {
        self.swept += rhs.swept;
        self.derived += rhs.derived;
        self.poisoned += rhs.poisoned;
    }
}

/// Runs one derive pass over every registered kind: sweep expired leases,
/// then drain the eligible set (oldest pending with parents done-or-poison,
/// one at a time). A busy write slot (`writer_busy`) ends the pass early —
/// the queue never fights a request for the single writer; the next tick
/// resumes. A deriver PANIC propagates as an error (the lease stays held
/// and the expiry sweep re-runs the node — that is the crash-mid-derivation
/// path, and it is idempotent by contract).
pub async fn derive_pass(state: &AppState) -> Result<PassReport, ApiError> {
    let mut report = PassReport::default();
    for kind in state.derive.kinds() {
        report += sweep_expired(state, kind).await?;
        drain(state, kind, &mut report).await?;
    }
    Ok(report)
}

/// Sweeps the kind's expired leases inside one write transaction: each
/// expired holder loses its lease and burns one attempt (`lease expired`
/// as the reason when it poisons).
async fn sweep_expired(
    state: &AppState,
    kind: &Arc<RegisteredKind>,
) -> Result<PassReport, ApiError> {
    let now = state.now_ns() as i64;
    let kind = Arc::clone(kind);
    state
        .write(move |store| {
            let conn = store.conn();
            let tx = conn
                .unchecked_transaction()
                .map_err(ApiError::log_internal)?;
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

        // Depth cap, BEFORE the lease (no attempt burned — the caps are
        // structural): a first-parent chain deeper than DERIVE_MAX_DEPTH is
        // exactly what a recursive deriver could not survive in one call.
        // Quarantine immediately; the children proceed on the empty marker.
        // The generation pre-check inside keeps the common short-chain case
        // O(1); the bounded exact walk runs only on generation-over-cap
        // nodes. Long history itself derives batch-by-batch through this
        // very loop.
        let over_depth = {
            let cs_for_depth = cs;
            state
                .read(move |conn| {
                    db::derive_over_depth(conn, repo_id, &cs_for_depth, DERIVE_MAX_DEPTH)
                        .map_err(ApiError::log_internal)
                })
                .await?
        };
        if over_depth {
            let now = state.now_ns() as i64;
            let reason = depth_cap_reason();
            let kind_for_poison = Arc::clone(kind);
            match state
                .write(move |store| {
                    let conn = store.conn();
                    let tx = conn
                        .unchecked_transaction()
                        .map_err(ApiError::log_internal)?;
                    db::derive_poison_now(&tx, kind_for_poison.id, repo_id, &cs, &reason, now)
                        .map_err(ApiError::log_internal)?;
                    tx.commit().map_err(ApiError::log_internal)?;
                    Ok(())
                })
                .await
            {
                Ok(()) => (),
                Err(e) if e.code() == ApiCode::WriterBusy => return Ok(()),
                Err(e) => return Err(e),
            };
            report.poisoned += 1;
            continue;
        }

        // Lease. A stale pick (someone else took it — impossible within one
        // process, tolerated anyway) just falls through to the next pick.
        let now = state.now_ns() as i64;
        let kind_for_lease = Arc::clone(kind);
        let leased = match state
            .write(move |store| {
                db::derive_lease(
                    store.conn(),
                    kind_for_lease.id,
                    repo_id,
                    &cs,
                    now.saturating_add(lease_ns),
                )
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
        let derived = state
            .write(move |store| {
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
                    .map_err(ApiError::log_internal)?
                    .into_iter()
                    .map(|o| o.map(DerivedOut).unwrap_or_else(DerivedOut::empty))
                    .collect::<Vec<_>>();
                // Inner result on purpose: a deriver `Err` is a FAILED
                // ATTEMPT (attempted below), not an API error — the outer
                // error type stays the write-slot's.
                match kind_for_derive.deriver.derive(conn, repo_id, cs, &outs) {
                    Ok(out) => Ok(Ok((cs, out))),
                    Err(reason) => Ok(Err(reason)),
                }
            })
            .await;
        let outcome = match derived {
            Ok(outcome) => outcome,
            Err(e) if e.code() == ApiCode::WriterBusy => {
                // Lease still held; the sweep re-runs the node after expiry.
                return Ok(());
            }
            // A deriver PANIC lands here as a join error: the lease stays
            // held, the expiry sweep re-runs the node (idempotency
            // contract), and the pass reports the failure to its caller.
            Err(e) => return Err(e),
        };
        let (cs, out) = match outcome {
            Ok(pair) => pair,
            Err(reason) => {
                // Failed attempt: attempts+1, poison + report at the cap —
                // one transaction so the row and its report commit together.
                let now = state.now_ns() as i64;
                let kind_for_fail = Arc::clone(kind);
                let poisoned = match state
                    .write(move |store| {
                        let conn = store.conn();
                        let tx = conn
                            .unchecked_transaction()
                            .map_err(ApiError::log_internal)?;
                        let poisoned = db::derive_fail_attempt(
                            &tx,
                            kind_for_fail.id,
                            repo_id,
                            &cs,
                            &reason.0,
                            now,
                        )
                        .map_err(ApiError::log_internal)?;
                        tx.commit().map_err(ApiError::log_internal)?;
                        Ok(poisoned)
                    })
                    .await
                {
                    Ok(poisoned) => poisoned,
                    Err(e) if e.code() == ApiCode::WriterBusy => return Ok(()),
                    Err(e) => return Err(e),
                };
                report.poisoned += usize::from(poisoned);
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
                    let tx = conn
                        .unchecked_transaction()
                        .map_err(ApiError::log_internal)?;
                    db::derive_poison_now(&tx, kind_for_poison.id, repo_id, &cs, &reason, now)
                        .map_err(ApiError::log_internal)?;
                    tx.commit().map_err(ApiError::log_internal)?;
                    Ok(())
                })
                .await
            {
                Ok(()) => (),
                Err(e) if e.code() == ApiCode::WriterBusy => return Ok(()),
                Err(e) => return Err(e),
            };
            report.poisoned += 1;
            continue;
        }

        // Record done (the second transaction — see the module doc). A busy
        // slot stops the pass with the lease held; the sweep re-runs the
        // node after expiry (the same idempotent re-derivation path).
        let kind_for_record = Arc::clone(kind);
        let recorded = match state
            .write(move |store| {
                db::derive_record_done(
                    store.conn(),
                    kind_for_record.id,
                    repo_id,
                    &cs,
                    out.as_bytes(),
                )
                .map_err(ApiError::log_internal)
            })
            .await
        {
            Ok(recorded) => recorded,
            Err(e) if e.code() == ApiCode::WriterBusy => return Ok(()),
            Err(e) => return Err(e),
        };
        if recorded {
            report.derived += 1;
        }
    }
}
