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

use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, TryLockError};
use std::time::Duration;

use naatre_store::data_dir::DataDir;
use naatre_store::error::StoreError;
use naatre_store::store::Store;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;

/// Shared server state: the store plus server configuration.
pub struct ServerState {
    // `Arc` so the spawn_blocking closures own their reference (they must be
    // 'static); the lock itself is a plain std Mutex.
    store: Arc<Mutex<Store>>,
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

impl ServerState {
    /// Opens the store inside `data` (pragmas + migrations via
    /// [`Store::open`]) with the default configuration.
    pub fn open(data: DataDir) -> Result<Self, StoreError> {
        let data_dir_display = data.path().to_path_buf();
        let store = Arc::new(Mutex::new(Store::open(data)?));
        Ok(Self {
            store,
            data_dir_display,
            drain_timeout: Duration::from_secs(30),
        })
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
