//! Server state: the shared store handle and its async bridge.
//!
//! Hard rule: all SQLite access happens inside `spawn_blocking`, under the
//! std [`Mutex`] guard. The guard is taken and released entirely inside the
//! blocking closure — it is never held across an `.await` (the closures
//! handed to [`ServerState::read`] and [`ServerState::write`] must be fully
//! synchronous, which their `'static` + non-async signatures enforce).
//!
//! The write side is the single-writer slot: [`ServerState::write`] takes
//! the mutex with `try_lock` and reports contention as 503 `writer_busy`
//! with `retryable: true` instead of queueing. Concurrent lands on the
//! same (repo, principal, key) never reach this slot — they join in
//! flight above it (see [`crate::land_join`]).
//!
//! The read side is decoupled from that slot: [`ServerState::read`] runs on
//! a small pool of read-only connections ([`ReadPool`]). SQLite WAL lets
//! readers run beside the single writer, so auth lookups and GET handlers
//! never contend with lands for the write slot — writer-busy 503s mean
//! another WRITE, nothing else.

use std::collections::{HashMap, VecDeque};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, TryLockError};
use std::time::Duration;

use naatre_store::cas::Cas;
use naatre_store::data_dir::DataDir;
use naatre_store::ephemeral::Ephemeral;
use naatre_store::error::StoreError;
use naatre_store::store::Store;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::land_join::LandJoins;

/// Injectable wall clock: nanoseconds since the Unix epoch. Production uses
/// [`system_now_ns`]; tests inject a fixed-step clock so expiry behaviour is
/// deterministic.
pub type ClockFn = Arc<dyn Fn() -> u64 + Send + Sync>;

/// Rate-limit accounting for auth failures: per-key sliding windows, the
/// global row window, and the global counters. Keys are stable derivations
/// of the *presented* credential (never the peer address; see
/// `auth::failure_key`).
#[derive(Default)]
pub(crate) struct AuthFailInner {
    /// Key → failure timestamps inside the current 60 s window.
    buckets: HashMap<String, VecDeque<u64>>,
    /// Timestamps of audit rows WRITTEN, inside the global 60 s window —
    /// the global row budget (a flood spread over fresh keys drains it).
    row_window: VecDeque<u64>,
    /// Every observed auth failure, rowed or not — the flood-signal counter.
    global_count: u64,
    /// Auth-failure rows actually written to `audit_log`.
    rows_written: u64,
}

/// Sliding window and per-key row cap for auth-failure auditing.
pub(crate) const AUTH_FAIL_WINDOW_NS: u64 = 60 * 1_000_000_000;
pub(crate) const AUTH_FAIL_PER_KEY_LIMIT: usize = 10;
/// Global per-window cap on auth-failure ROWS: beyond it, failures are
/// counted, not rowed, under any keying (plan "Auth & route matrix").
pub(crate) const AUTH_FAIL_GLOBAL_ROW_CAP: usize = 200;

/// Registers one auth failure under `key` at time `now_ns`; returns `true`
/// when the caller must write an `audit_log` row (within the per-key limit
/// AND the global row cap) and `false` when the failure is only counted
/// (beyond either limit).
pub(crate) fn tick_auth_failure(inner: &mut AuthFailInner, key: &str, now_ns: u64) -> bool {
    let bucket = inner.buckets.entry(key.to_string()).or_default();
    bucket.retain(|ts| now_ns.saturating_sub(*ts) < AUTH_FAIL_WINDOW_NS);
    inner
        .row_window
        .retain(|ts| now_ns.saturating_sub(*ts) < AUTH_FAIL_WINDOW_NS);
    inner.global_count += 1;
    if bucket.len() >= AUTH_FAIL_PER_KEY_LIMIT || inner.row_window.len() >= AUTH_FAIL_GLOBAL_ROW_CAP
    {
        return false;
    }
    bucket.push_back(now_ns);
    inner.row_window.push_back(now_ns);
    inner.rows_written += 1;
    true
}

/// Pool of read-only SQLite connections over the same DB file. WAL keeps
/// readers lock-free beside the single writer, so GET handlers and auth
/// lookups run without touching the write slot. Each connection sits behind
/// its own std [`Mutex`]; [`ReadPool::checkout`] hands them out round-robin
/// and the guard is locked/released entirely inside the `spawn_blocking`
/// closure (never across an `.await`).
struct ReadPool {
    conns: Vec<Arc<Mutex<rusqlite::Connection>>>,
    next: AtomicUsize,
}

/// Pool size: four readers. A read is a bounded query; four concurrent
/// readers per connection slot keep every GET path queue-free in practice.
const READ_POOL_SIZE: usize = 4;

impl ReadPool {
    /// Opens `size` read-only connections to the migrated database. Callers
    /// run this AFTER [`Store::open`]: the schema must exist and the WAL
    /// must be recovered by the read-write connection before a read-only
    /// connection can open.
    fn open(db_path: &Path, size: usize) -> rusqlite::Result<ReadPool> {
        let conns = (0..size)
            .map(|_| {
                let conn = rusqlite::Connection::open_with_flags(
                    db_path,
                    rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY,
                )?;
                conn.busy_timeout(Duration::from_millis(naatre_store::store::BUSY_TIMEOUT_MS))?;
                Ok(Arc::new(Mutex::new(conn)))
            })
            .collect::<rusqlite::Result<Vec<_>>>()?;
        Ok(ReadPool {
            conns,
            next: AtomicUsize::new(0),
        })
    }

    /// The next connection, round-robin.
    fn checkout(&self) -> Arc<Mutex<rusqlite::Connection>> {
        let i = self.next.fetch_add(1, Ordering::Relaxed) % self.conns.len();
        Arc::clone(&self.conns[i])
    }
}

/// How long a same-key land join waits for the leader's outcome before
/// answering retryable 503 (the retry lands on the settled-row replay).
/// Independent of [`ServerState::drain_timeout`]: a request shed at the
/// join deadline must not be coupled to shutdown drain budgeting.
pub const JOIN_DEADLINE: Duration = Duration::from_secs(10);

/// Shared server state: the store plus server configuration.
pub struct ServerState {
    // `Arc` so the spawn_blocking closures own their reference (they must be
    // 'static); the lock itself is a plain std Mutex.
    store: Arc<Mutex<Store>>,
    /// History CAS handle over the same data dir (`ServerState::open`
    /// creates it). Blocking CAS work goes through [`ServerState::blocking`].
    pub cas: Arc<Cas>,
    /// Ephemeral snapshot store over the same data dir (`ephemeral/<snap>/`).
    /// Blocking filesystem work goes through [`ServerState::blocking`].
    pub ephemeral: Arc<Ephemeral>,
    /// Nanosecond clock used for token expiry, audit timestamps, and the
    /// auth-failure window. Tests replace this after [`ServerState::open`].
    pub clock: ClockFn,
    /// Rate-limit buckets for auth-failure auditing; guarded like the store
    /// (locked and released inside synchronous fns, never across `.await`).
    auth_fail: Mutex<AuthFailInner>,
    /// The in-flight land join registry: concurrent same-key lands share
    /// one outcome instead of contending for the write slot. In-memory
    /// only, no store access; the guard mutex is never held across an
    /// `.await` (see [`crate::land_join`]).
    pub(crate) land_joins: LandJoins,
    /// Read-only connection pool backing [`ServerState::read`]; WAL lets
    /// these run beside the single writer without touching the write slot.
    read_pool: ReadPool,
    /// Data dir path for the startup banner. Banner and log facts are paths
    /// and versions only — naatre has no credentials yet, and the standing
    /// rule is that none (DSNs, tokens, keys) may ever reach the banner,
    /// request logs, or error strings.
    pub data_dir_display: PathBuf,
    /// How long graceful shutdown waits for in-flight requests before
    /// forcing return. Default 30 s.
    pub drain_timeout: Duration,
    /// How long a same-key land join waits for its leader before answering
    /// retryable 503 (see [`JOIN_DEADLINE`]). Independent of
    /// [`ServerState::drain_timeout`]; tests lower it to pin the expiry
    /// behaviour.
    pub join_deadline: Duration,
    /// Snapshot GC switch: the POST-time opportunistic pass and the serve
    /// background interval task both check it. Off-switch for tests that
    /// drive [`crate::snapshots_gc::gc_snapshots`] directly and assert its
    /// effects deterministically.
    pub snapshot_gc_enabled: bool,
    /// Snapshot GC background-pass interval (`serve`'s task ticks on this).
    /// Injectable so tests can observe a background pass deterministically —
    /// e.g. a 10 ms interval plus a short-TTL snapshot, collected with no
    /// manual [`crate::snapshots_gc::gc_snapshots`] call. Default:
    /// [`crate::serve::SNAPSHOT_GC_INTERVAL`].
    pub snapshot_gc_interval: Duration,
    /// Registered derived-data kinds (M6a). Empty until
    /// [`ServerState::register_deriver`] — M6b registers the real kinds;
    /// tests register throwaway kinds.
    pub derive: Arc<crate::derive::DeriveRegistry>,
    /// Derive-queue switch: `serve`'s background pass checks it. Off-switch
    /// for tests that drive [`crate::derive::derive_pass`] directly and
    /// assert its effects deterministically (same pattern as
    /// `snapshot_gc_enabled`).
    pub derive_queue_enabled: bool,
    /// Derive-queue background-pass interval (`serve`'s task ticks on this;
    /// also the lease horizon's base — a lease lasts twice the interval).
    /// Default: [`crate::serve::DERIVE_QUEUE_INTERVAL`].
    pub derive_queue_interval: Duration,
    /// Boot-time derive-backfill switch: `serve`'s startup task checks it.
    /// Off-switch for tests that hold the backfill to build the unrowed
    /// window (changesets with no `derive_state` rows) and assert the
    /// read-during-backlog contract deterministically (same pattern as
    /// `derive_queue_enabled`).
    pub derive_backfill_enabled: bool,
    /// Test-only delay before the backfill's first batch ([`Duration`]:
    /// ZERO in production). Widens the boot race window deterministically
    /// so pins that read right after boot can be A/B-tested against a
    /// slow backfill.
    pub derive_backfill_delay: Duration,
}

/// Shared handle to the server state.
pub type AppState = Arc<ServerState>;

/// The production clock: system time as ns since the epoch; 0 if the clock
/// is somehow before the epoch (a broken clock must not panic the server).
fn system_now_ns() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(0)
}

impl ServerState {
    /// Opens the store inside `data` (pragmas + migrations via
    /// [`Store::open`]) with the default configuration: system clock, the
    /// data dir's history CAS, a read-only connection pool, and an empty
    /// auth-failure tracker.
    pub fn open(data: DataDir) -> Result<Self, StoreError> {
        let data_dir_display = data.path().to_path_buf();
        let db_path = data.db_path();
        let cas = Arc::new(Cas::new(&data)?);
        let ephemeral = Arc::new(Ephemeral::new(&data)?);
        // The write store opens first: migrations run there, and its
        // connection recovers the WAL before any read-only connection
        // opens (a read-only connection cannot run recovery).
        let store = Arc::new(Mutex::new(Store::open(data)?));
        let read_pool = ReadPool::open(&db_path, READ_POOL_SIZE)?;
        Ok(Self {
            store,
            read_pool,
            cas,
            ephemeral,
            clock: Arc::new(system_now_ns),
            auth_fail: Mutex::new(AuthFailInner::default()),
            land_joins: LandJoins::default(),
            data_dir_display,
            drain_timeout: Duration::from_secs(30),
            join_deadline: JOIN_DEADLINE,
            snapshot_gc_enabled: true,
            snapshot_gc_interval: crate::serve::SNAPSHOT_GC_INTERVAL,
            derive: Arc::new(crate::derive::DeriveRegistry::default()),
            derive_queue_enabled: true,
            derive_queue_interval: crate::serve::DERIVE_QUEUE_INTERVAL,
            derive_backfill_enabled: true,
            derive_backfill_delay: Duration::ZERO,
        })
    }

    /// Registers one derived-data kind: seeds its `derive_kinds` row (the
    /// id is stable across processes — `INSERT OR IGNORE`) and adds the
    /// deriver to the registry. Called before the state is shared (boot, or
    /// a test's pre-`Arc` tweak); a duplicate kind name panics in the
    /// registry (two derivers must never share one id).
    pub fn register_deriver(
        &mut self,
        deriver: Arc<dyn crate::derive::Deriver>,
    ) -> Result<(), StoreError> {
        let name = deriver.kind();
        let store = lock_store(&self.store);
        let id = crate::db::derive_kind_seed(store.conn(), name).map_err(StoreError::Sqlite)?;
        let registry = Arc::make_mut(&mut self.derive);
        registry.register(crate::derive::RegisteredKind { name, id, deriver });
        Ok(())
    }

    /// Current time in ns since the epoch, read through the injected clock.
    pub fn now_ns(&self) -> u64 {
        (self.clock)()
    }

    /// Records one auth failure under `key`; returns `true` when an
    /// `audit_log` row must be written. Synchronous by design — the bucket
    /// update never interleaves with an `.await`.
    pub(crate) fn tick_auth_failure(&self, key: &str) -> bool {
        let now = self.now_ns();
        let mut inner = self
            .auth_fail
            .lock()
            .unwrap_or_else(PoisonError::into_inner);
        tick_auth_failure(&mut inner, key, now)
    }

    /// `(global auth-failure count, auth-failure rows written)` — the
    /// flood-signal pair for tests and ops alerting.
    pub fn auth_fail_stats(&self) -> (u64, u64) {
        let inner = self
            .auth_fail
            .lock()
            .unwrap_or_else(PoisonError::into_inner);
        (inner.global_count, inner.rows_written)
    }

    /// Runs a read-only closure against the store off the async runtime, on
    /// a pooled read-only connection ([`ReadPool`]): reads never take the
    /// single-writer slot.
    ///
    /// The blocking wait is pool-wait only: the checkout lands on one of
    /// `READ_POOL_SIZE` connections round-robin, and the std [`Mutex`] on
    /// that connection is held for the length of one read. It is NOT
    /// bounded by the store's `busy_timeout` — that governs SQLite-level
    /// WRITE contention, which this pool never enters.
    pub async fn read<T, F>(&self, f: F) -> Result<T, ApiError>
    where
        F: FnOnce(&rusqlite::Connection) -> Result<T, ApiError> + Send + 'static,
        T: Send + 'static,
    {
        let conn = self.read_pool.checkout();
        tokio::task::spawn_blocking(move || {
            let guard = conn.lock().unwrap_or_else(PoisonError::into_inner);
            f(&guard)
        })
        .await
        .map_err(|join| {
            tracing::error!(error = %join, "store blocking task panicked");
            ApiError::internal()
        })?
    }

    /// Runs a mutating closure against the store off the async runtime.
    ///
    /// Contention (the single-writer slot already held) returns
    /// `writer_busy` immediately rather than queueing — the caller turns
    /// that into 503 + `Retry-After`.
    pub async fn write<T, F>(&self, f: F) -> Result<T, ApiError>
    where
        F: FnOnce(&mut Store) -> Result<T, ApiError> + Send + 'static,
        T: Send + 'static,
    {
        let store = Arc::clone(&self.store);
        tokio::task::spawn_blocking(move || {
            let mut guard = match store.try_lock() {
                Ok(guard) => guard,
                Err(TryLockError::WouldBlock) => {
                    return Err(ApiError::new(ApiCode::WriterBusy, "writer busy").retryable(true));
                }
                Err(TryLockError::Poisoned(poisoned)) => poisoned.into_inner(),
            };
            f(&mut guard)
        })
        .await
        .map_err(|join| {
            tracing::error!(error = %join, "store blocking task panicked");
            ApiError::internal()
        })?
    }

    /// Runs an arbitrary blocking closure (CAS filesystem work) off the
    /// async runtime, with the same panic policy as [`ServerState::read`].
    pub async fn blocking<T, F>(&self, f: F) -> Result<T, ApiError>
    where
        F: FnOnce() -> Result<T, ApiError> + Send + 'static,
        T: Send + 'static,
    {
        tokio::task::spawn_blocking(f).await.map_err(|join| {
            tracing::error!(error = %join, "blocking task panicked");
            ApiError::internal()
        })?
    }

    /// Test hook: holds a write transaction (`BEGIN IMMEDIATE`) on the store
    /// for `ms` milliseconds, simulating a slow writer. Panic on SQL failure
    /// so tests never mistake a broken hook for a passing one.
    #[cfg(any(test, debug_assertions))]
    pub async fn hold_write_txn(&self, ms: u64) {
        let store = Arc::clone(&self.store);
        tokio::task::spawn_blocking(move || {
            let guard = lock_store(&store);
            let conn = guard.conn();
            conn.execute_batch("BEGIN IMMEDIATE; SELECT 1;")
                .expect("hold_write_txn: BEGIN IMMEDIATE");
            std::thread::sleep(Duration::from_millis(ms));
            conn.execute_batch("COMMIT;")
                .expect("hold_write_txn: COMMIT");
        })
        .await
        .expect("hold_write_txn task panicked");
    }
}

/// Locks the store mutex, continuing past poisoning: a panic while some
/// closure held the guard leaves no broken in-memory invariant (rusqlite
/// rolls its transaction back on drop), so the store stays usable.
fn lock_store(store: &Mutex<Store>) -> MutexGuard<'_, Store> {
    store.lock().unwrap_or_else(PoisonError::into_inner)
}
