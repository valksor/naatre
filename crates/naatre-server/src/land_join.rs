//! The in-flight land join registry (M4c).
//!
//! The plan's M4 invariant: two concurrent identical (key, payload) land
//! requests JOIN the in-flight land and return identical results (one
//! settlement); a same-key different-head request joins too, then gets the
//! settled rules; a different key finds the write slot contended only while
//! another WRITE is in flight and is answered 503 `writer_busy` (unchanged
//! `try_lock` path). On the production router this holds end to end —
//! reads (auth lookups, GETs) run on the server's read pool and never
//! contend for the write slot, so two concurrent identical lands both get
//! 200 with no 503. The registry is a plain `HashMap<(repo, principal,
//! key)>` of in-flight lands guarded by a std [`Mutex`] that is locked and
//! released inside synchronous fns — never held across an `.await`. The
//! registry itself never touches SQLite: the route reaches the store only
//! through [`crate::state::ServerState::write`] (the leader) and pool
//! reads (the settled-row replay in the join fallback), keeping the
//! single-writer backpressure observable on this route.
//!
//! Mechanism: each in-flight land publishes its outcome on a
//! [`tokio::sync::watch`] channel (`None` while in flight, so joiners can
//! `wait_for` it; the channel retains the value for subscribers that arrive
//! after publication). The leader holds a [`LandJoinGuard`] whose `Drop`
//! deregisters the entry — panic-safe: a guard dropped WITHOUT a published
//! outcome (the route task panicked or was cancelled mid-land) publishes an
//! internal error so joiners fail fast instead of waiting out their join
//! deadline ([`crate::state::JOIN_DEADLINE`]). A joiner that arrives after
//! deregistration simply runs its own land and replays the settled row
//! inside the transaction.
//!
//! The repo is keyed by NAME (unique per server): the route must not touch
//! the store before the write slot (see [`crate::land::LandCommand`]), so
//! the numeric repo id is not available at registration time.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};

use tokio::sync::watch;

use naatre_core::ids::Id;

use crate::api_error::ApiError;
use crate::land::LandDone;

/// Registry key: land keys are scoped to (repo, principal).
#[derive(Clone, PartialEq, Eq, Hash)]
struct LandJoinKey {
    repo: String,
    principal: String,
    key: String,
}

/// The shared outcome of one in-flight land: the settled result, or the
/// error it failed with (a failed land leaves the key unsettled; joiners
/// may retry into the settled rules).
#[derive(Clone)]
pub(crate) enum LandOutcome {
    Done(LandDone),
    Err(ApiError),
}

/// One in-flight land: the head it was requested with, and the watch
/// channel carrying the outcome. The kept receiver guarantees `send`
/// always succeeds — the published value outlives every joiner's
/// subscription.
pub(crate) struct InFlightLand {
    pub(crate) head: Id,
    tx: watch::Sender<Option<LandOutcome>>,
    _keep_rx: watch::Receiver<Option<LandOutcome>>,
}

impl InFlightLand {
    fn new(head: Id) -> Self {
        let (tx, rx) = watch::channel(None);
        InFlightLand {
            head,
            tx,
            _keep_rx: rx,
        }
    }

    /// A receiver over the outcome: `None` while in flight, the settled
    /// `LandOutcome` after publication (watch retains the value).
    pub(crate) fn subscribe(&self) -> watch::Receiver<Option<LandOutcome>> {
        self.tx.subscribe()
    }
}

/// The per-server registry of in-flight lands (see the module doc).
#[derive(Default)]
pub(crate) struct LandJoins {
    map: Mutex<HashMap<LandJoinKey, Arc<InFlightLand>>>,
}

/// The outcome of [`LandJoins::begin`]: either this request joins an
/// existing in-flight land, or it leads and holds the deregistering guard.
pub(crate) enum JoinSlot<'a> {
    Join(Arc<InFlightLand>),
    Lead(LandJoinGuard<'a>),
}

/// Leads one land: created by [`LandJoins::begin`] when no in-flight land
/// exists for the key. [`LandJoinGuard::publish`] shares the outcome with
/// joiners; the subsequent `Drop` deregisters the entry.
pub(crate) struct LandJoinGuard<'a> {
    registry: &'a LandJoins,
    slot_key: LandJoinKey,
    in_flight: Arc<InFlightLand>,
}

impl LandJoinGuard<'_> {
    /// Publishes the land's outcome to every joiner, then deregisters the
    /// entry (`Drop`). Publishing strictly before deregistration means a
    /// late arrival either finds the entry with its outcome set, or finds
    /// it gone and replays the settled row inside its own transaction.
    pub(crate) fn publish(self, outcome: LandOutcome) {
        let _ = self.in_flight.tx.send(Some(outcome));
    }
}

impl Drop for LandJoinGuard<'_> {
    fn drop(&mut self) {
        {
            let mut map = self.registry.lock();
            if map
                .get(&self.slot_key)
                .is_some_and(|e| Arc::ptr_eq(e, &self.in_flight))
            {
                map.remove(&self.slot_key);
            }
        }
        // Dropped without a published outcome: the land died mid-flight
        // (the route task panicked unwinding). Fail the joiners now instead
        // of leaving them to their own drain timeout.
        self.in_flight.tx.send_if_modified(|slot| {
            if slot.is_none() {
                *slot = Some(LandOutcome::Err(ApiError::internal()));
                true
            } else {
                false
            }
        });
    }
}

impl LandJoins {
    /// Locks the registry; never held across an `.await`.
    fn lock(&self) -> MutexGuard<'_, HashMap<LandJoinKey, Arc<InFlightLand>>> {
        self.map.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// Registers this request's land, or finds the in-flight one to join.
    /// Check and insert are atomic under the registry mutex: exactly one
    /// concurrent request per (repo, principal, key) becomes the leader.
    pub(crate) fn begin(&self, repo: &str, principal: &str, key: &str, head: Id) -> JoinSlot<'_> {
        let slot_key = LandJoinKey {
            repo: repo.to_string(),
            principal: principal.to_string(),
            key: key.to_string(),
        };
        let mut map = self.lock();
        if let Some(in_flight) = map.get(&slot_key) {
            return JoinSlot::Join(Arc::clone(in_flight));
        }
        let in_flight = Arc::new(InFlightLand::new(head));
        map.insert(slot_key.clone(), Arc::clone(&in_flight));
        JoinSlot::Lead(LandJoinGuard {
            registry: self,
            slot_key,
            in_flight,
        })
    }
}
