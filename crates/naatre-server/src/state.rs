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
//! with `retryable: true` instead of queueing.

use std::collections::{HashMap, VecDeque};
use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, TryLockError};
use std::time::Duration;

use naatre_store::cas::Cas;
use naatre_store::data_dir::DataDir;
use naatre_store::error::StoreError;
use naatre_store::store::Store;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;

/// Injectable wall clock: nanoseconds since the Unix epoch. Production uses
/// [`system_now_ns`]; tests inject a fixed-step clock so expiry behaviour is
/// deterministic.
pub type ClockFn = Arc<dyn Fn() -> u64 + Send + Sync>;

/// Rate-limit accounting for auth failures: per-key sliding windows plus the
/// global counters. Keys are stable derivations of the *presented*
/// credential (never the peer address; see `auth::failure_key`).
#[derive(Default)]
pub(crate) struct AuthFailInner {
    /// Key → failure timestamps inside the current 60 s window.
    buckets: HashMap<String, VecDeque<u64>>,
    /// Every observed auth failure, rowed or not — the flood-signal counter.
    global_count: u64,
    /// Auth-failure rows actually written to `audit_log`.
    rows_written: u64,
}

/// Sliding window and per-key row cap for auth-failure auditing.
pub(crate) const AUTH_FAIL_WINDOW_NS: u64 = 60 * 1_000_000_000;
pub(crate) const AUTH_FAIL_PER_KEY_LIMIT: usize = 10;

/// Registers one auth failure under `key` at time `now_ns`; returns `true`
/// when the caller must write an `audit_log` row (within the per-key limit)
/// and `false` when the failure is only counted (beyond the limit).
pub(crate) fn tick_auth_failure(inner: &mut AuthFailInner, key: &str, now_ns: u64) -> bool {
    let bucket = inner.buckets.entry(key.to_string()).or_default();
    bucket.retain(|ts| now_ns.saturating_sub(*ts) < AUTH_FAIL_WINDOW_NS);
    inner.global_count += 1;
    if bucket.len() >= AUTH_FAIL_PER_KEY_LIMIT {
        return false;
    }
    bucket.push_back(now_ns);
    inner.rows_written += 1;
    true
}

/// Shared server state: the store plus server configuration.
pub struct ServerState {
    // `Arc` so the spawn_blocking closures own their reference (they must be
    // 'static); the lock itself is a plain std Mutex.
    store: Arc<Mutex<Store>>,
    /// History CAS handle over the same data dir (`ServerState::open`
    /// creates it). Blocking CAS work goes through [`ServerState::blocking`].
    pub cas: Arc<Cas>,
    /// Nanosecond clock used for token expiry, audit timestamps, and the
    /// auth-failure window. Tests replace this after [`ServerState::open`].
    pub clock: ClockFn,
    /// Rate-limit buckets for auth-failure auditing; guarded like the store
    /// (locked and released inside synchronous fns, never across `.await`).
    auth_fail: Mutex<AuthFailInner>,
    /// Data dir path for the startup banner. Banner and log facts are paths
    /// and versions only — naatre has no credentials yet, and the standing
    /// rule is that none (DSNs, tokens, keys) may ever reach the banner,
    /// request logs, or error strings.
    pub data_dir_display: PathBuf,
    /// How long graceful shutdown waits for in-flight requests before
    /// forcing return. Default 30 s.
    pub drain_timeout: Duration,
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
    /// data dir's history CAS, and an empty auth-failure tracker.
    pub fn open(data: DataDir) -> Result<Self, StoreError> {
        let data_dir_display = data.path().to_path_buf();
        let cas = Arc::new(Cas::new(&data)?);
        let store = Arc::new(Mutex::new(Store::open(data)?));
        Ok(Self {
            store,
            cas,
            clock: Arc::new(system_now_ns),
            auth_fail: Mutex::new(AuthFailInner::default()),
            data_dir_display,
            drain_timeout: Duration::from_secs(30),
        })
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

    /// Runs a read-only closure against the store off the async runtime.
    ///
    /// The blocking mutex lock is bounded the same way every SQLite waiter
    /// is: by the store's `busy_timeout` (5 s) and OS lock wait.
    pub async fn read<T, F>(&self, f: F) -> Result<T, ApiError>
    where
        F: FnOnce(&Store) -> Result<T, ApiError> + Send + 'static,
        T: Send + 'static,
    {
        let store = Arc::clone(&self.store);
        tokio::task::spawn_blocking(move || {
            let guard = lock_store(&store);
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
