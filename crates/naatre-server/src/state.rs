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

use naatre_store::backend::{BackendConfig, BlobKind, MetaKind};
use naatre_store::cas::Cas;
use naatre_store::data_dir::DataDir;
use naatre_store::error::StoreError;
use naatre_store::store::Store;
use naatre_store::{BlobStore, Db, EphemeralStore, S3Store};

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

/// Pool of read-only metadata connections over the same store. WAL keeps
/// SQLite readers lock-free beside the single writer, so GET handlers and
/// auth lookups run without touching the write slot; on Postgres each pool
/// connection is a session set read-only (`SET
/// default_transaction_read_only = on`), mirroring the SQLite
/// `SQLITE_OPEN_READ_ONLY` guard. Each connection sits behind its own std
/// [`Mutex`]; [`ReadPool::checkout`] hands them out round-robin and the
/// guard is locked/released entirely inside the `spawn_blocking` closure
/// (never across an `.await`).
struct ReadPool {
    conns: Vec<Arc<Mutex<Db>>>,
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
    fn open(db_path: &Path, size: usize) -> Result<ReadPool, StoreError> {
        let conns = (0..size)
            .map(|_| {
                let conn = rusqlite::Connection::open_with_flags(
                    db_path,
                    rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY,
                )
                .map_err(StoreError::Sqlite)?;
                conn.busy_timeout(Duration::from_millis(naatre_store::store::BUSY_TIMEOUT_MS))
                    .map_err(StoreError::Sqlite)?;
                Ok(Arc::new(Mutex::new(Db::Sqlite(conn))))
            })
            .collect::<Result<Vec<_>, StoreError>>()?;
        Ok(ReadPool {
            conns,
            next: AtomicUsize::new(0),
        })
    }

    /// Opens `size` read-only Postgres connections from the parsed DSN.
    /// Callers run this AFTER [`Store::open_backend`] (migrations must be
    /// current before reads start).
    fn open_postgres(cfg: &BackendConfig, size: usize) -> Result<ReadPool, StoreError> {
        let pg = cfg
            .pg
            .as_ref()
            .expect("open_postgres requires a parsed Postgres config");
        let conns = (0..size)
            .map(|_| Ok(Arc::new(Mutex::new(Db::connect_postgres(pg, true)?))))
            .collect::<Result<Vec<_>, StoreError>>()?;
        Ok(ReadPool {
            conns,
            next: AtomicUsize::new(0),
        })
    }

    /// The next connection, round-robin.
    fn checkout(&self) -> Arc<Mutex<Db>> {
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
    /// History blob store over the same data dir (`ServerState::open`
    /// creates the filesystem CAS; `open_backend` swaps in the configured
    /// backend behind the trait). Blocking CAS work goes through
    /// [`ServerState::blocking`].
    pub cas: Arc<dyn BlobStore>,
    /// Ephemeral snapshot store (`ephemeral/<snap>/` on fs; the
    /// `ephemeral/<snap>/objects/…` key namespace in the bucket when
    /// `blob = s3` — the same [`EphemeralStore`] contract on both).
    /// Blocking storage work goes through [`ServerState::blocking`].
    pub ephemeral: Arc<dyn EphemeralStore>,
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
    /// Which metadata backend is open — a banner fact (the kind name only,
    /// never the DSN/endpoint).
    pub meta_backend: &'static str,
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
    /// Which blob backend is open — an fsck fact (the kind name only, no
    /// endpoint; the S3 backend's CAS-vs-DB mismatches get the
    /// external-drift note, see `fsck`).
    pub blob_backend: &'static str,
    /// Reachability GC switch (M10a): `serve`'s background interval task
    /// checks it. Off-switch for tests that drive
    /// [`crate::gc::gc_pass`] directly and assert its effects
    /// deterministically (same pattern as `snapshot_gc_enabled`).
    pub gc_enabled: bool,
    /// Reachability GC background-pass interval (`serve`'s task ticks on
    /// this). From `backend.toml` `gc_interval_secs` (0 = task off);
    /// default [`crate::gc::GC_DEFAULT_INTERVAL_SECS`].
    pub gc_interval: Duration,
    /// The collection grace window: a `gc_pending` candidate collects only
    /// when its FIRST-unreachable timestamp is at least this old AND no
    /// upload touched it inside the window. From `backend.toml`
    /// `gc_grace_secs`; default [`crate::gc::GC_DEFAULT_GRACE_SECS`].
    /// Injectable so tests can run the whole window with the injected
    /// clock instead of 30 real days.
    pub gc_grace: Duration,
    /// Test-only pause between the reachability GC's mark and its sweep
    /// (ZERO in production). Widens the mark→sweep race window
    /// deterministically so the re-reference pin can re-PUT an old orphan
    /// and reference it while the pass is between phases.
    pub gc_test_delay_after_mark: Duration,
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
    /// The blame read's streamed-blob-bytes cap ([`crate::routes::history`]'s
    /// assembly). Injectable so tests can pin the cap's both directions with
    /// tiny values instead of 256 MiB of blob loads. Default:
    /// [`crate::routes::history::BLAME_MAX_BLOB_BYTES`].
    pub blame_max_blob_bytes: usize,
    /// Rows per boot-backfill batch ([`crate::derive::backfill`]).
    /// Injectable so tests can force the multi-batch path with a handful of
    /// changesets. Default: [`crate::derive::BACKFILL_BATCH`].
    pub derive_backfill_batch_size: i64,
    /// Test-only ONE-SHOT writer_busy injection at the derive queue's
    /// mid-derivation write ([`crate::derive`]'s drain step): when armed to
    /// `Some(BusyAbort::Derive(cs))`, the queue's next write FOR that
    /// changeset fails with 503 `writer_busy` — the same error a lost
    /// single-writer-slot race produces — and the hook disarms
    /// ([`crate::derive::BusyAbort::DeriveThenRelease`] additionally makes
    /// the first lease-release attempt busy, exercising the release's
    /// retry). Never armed in production; it exists so the busy-abort lease
    /// handling is pinnable deterministically (the real collision is a
    /// timing race). Same aiming pattern as the test derivers' `fail_for`
    /// handles.
    pub derive_test_busy_abort: Arc<Mutex<Option<crate::derive::BusyAbort>>>,
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

/// The production clock as a [`ClockFn`], for offline paths with no
/// [`ServerState`] in scope (the import process).
pub fn system_clock() -> ClockFn {
    Arc::new(system_now_ns)
}

impl ServerState {
    /// Opens the store inside `data` (pragmas + migrations via
    /// [`Store::open`]) with the default configuration: system clock, the
    /// data dir's history CAS, a read-only connection pool, and an empty
    /// auth-failure tracker.
    pub fn open(data: DataDir) -> Result<Self, StoreError> {
        let data_dir_display = data.path().to_path_buf();
        let db_path = data.db_path();
        let cas: Arc<dyn BlobStore> = Arc::new(Cas::new(&data)?);
        let ephemeral: Arc<dyn EphemeralStore> = Arc::new(naatre_store::Ephemeral::new(&data)?);
        // The write store opens first: migrations run there, and its
        // connection recovers the WAL before any read-only connection
        // opens (a read-only connection cannot run recovery).
        let store = Arc::new(Mutex::new(Store::open(data)?));
        let read_pool = ReadPool::open(&db_path, READ_POOL_SIZE)?;
        Ok(Self {
            meta_backend: MetaKind::Sqlite.as_str(),
            blob_backend: BlobKind::Fs.as_str(),
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
            gc_enabled: true,
            gc_interval: Duration::from_secs(crate::gc::GC_DEFAULT_INTERVAL_SECS),
            gc_grace: Duration::from_secs(crate::gc::GC_DEFAULT_GRACE_SECS),
            gc_test_delay_after_mark: Duration::ZERO,
            derive: Arc::new(crate::derive::DeriveRegistry::default()),
            derive_queue_enabled: true,
            derive_queue_interval: crate::serve::DERIVE_QUEUE_INTERVAL,
            derive_backfill_enabled: true,
            derive_backfill_delay: Duration::ZERO,
            derive_backfill_batch_size: crate::derive::BACKFILL_BATCH,
            blame_max_blob_bytes: crate::routes::history::BLAME_MAX_BLOB_BYTES,
            derive_test_busy_abort: Arc::new(Mutex::new(None)),
        })
    }

    /// [`ServerState::open`] for the CONFIGURED backend: the metadata
    /// connection comes from `cfg` (SQLite default, or Postgres via the
    /// parsed DSN), and the blob/ephemeral pair follows `cfg.blob` — the
    /// data-dir CAS + ephemeral dirs, or the S3 bucket's `objects/` and
    /// `ephemeral/` key namespaces (one S3Store behind both traits; its
    /// operations serialize on the store's runtime, see
    /// [`naatre_store::s3`]). The caller must have run
    /// [`naatre_store::backend::check_data_dir_backend`] — the
    /// config/data-dir agreement is refused upstream, not here.
    pub fn open_backend(data: DataDir, cfg: &BackendConfig) -> Result<Self, StoreError> {
        let data_dir_display = data.path().to_path_buf();
        let db_path = data.db_path();
        // Blob + ephemeral backends are selected together (the brief's
        // documented decision: on S3 deployments the ephemeral namespace
        // lives in the same bucket — a split backend would break snapshot
        // restore on any blob=s3 deployment).
        let (cas, ephemeral): (Arc<dyn BlobStore>, Arc<dyn EphemeralStore>) = match cfg.blob {
            BlobKind::Fs => (
                Arc::new(Cas::new(&data)?),
                Arc::new(naatre_store::Ephemeral::new(&data)?),
            ),
            BlobKind::S3 => {
                let s3 = cfg.s3.as_ref().ok_or_else(|| {
                    StoreError::Backend(naatre_store::backend::BackendError::S3KeyMissing {
                        key: "bucket",
                        env: naatre_store::backend::ENV_S3_BUCKET,
                    })
                })?;
                let s3 = Arc::new(S3Store::new(s3, cfg.allow_plaintext)?);
                (s3.clone(), s3)
            }
        };
        // The write store opens first: migrations run there (and SQLite's
        // WAL is recovered) before any read-only connection opens.
        let store = Arc::new(Mutex::new(Store::open_backend(data, cfg)?));
        let read_pool = match cfg.meta {
            MetaKind::Sqlite => ReadPool::open(&db_path, READ_POOL_SIZE)?,
            MetaKind::Postgres => ReadPool::open_postgres(cfg, READ_POOL_SIZE)?,
        };
        Ok(Self {
            meta_backend: cfg.meta.as_str(),
            blob_backend: cfg.blob.as_str(),
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
            gc_enabled: cfg.gc_interval_secs != Some(0),
            gc_interval: Duration::from_secs(
                cfg.gc_interval_secs
                    .unwrap_or(crate::gc::GC_DEFAULT_INTERVAL_SECS),
            ),
            gc_grace: Duration::from_secs(
                cfg.gc_grace_secs
                    .unwrap_or(crate::gc::GC_DEFAULT_GRACE_SECS),
            ),
            gc_test_delay_after_mark: Duration::ZERO,
            derive: Arc::new(crate::derive::DeriveRegistry::default()),
            derive_queue_enabled: true,
            derive_queue_interval: crate::serve::DERIVE_QUEUE_INTERVAL,
            derive_backfill_enabled: true,
            derive_backfill_delay: Duration::ZERO,
            derive_backfill_batch_size: crate::derive::BACKFILL_BATCH,
            blame_max_blob_bytes: crate::routes::history::BLAME_MAX_BLOB_BYTES,
            derive_test_busy_abort: Arc::new(Mutex::new(None)),
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
        let id = crate::db::derive_kind_seed(store.conn(), name)?;
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
        F: FnOnce(&Db) -> Result<T, ApiError> + Send + 'static,
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
    /// so tests never mistake a broken hook for a passing one. Postgres has
    /// no BEGIN IMMEDIATE — the in-process slot is what the test observes,
    /// so a plain transaction holds it there.
    #[cfg(any(test, debug_assertions))]
    pub async fn hold_write_txn(&self, ms: u64) {
        let store = Arc::clone(&self.store);
        tokio::task::spawn_blocking(move || {
            let guard = lock_store(&store);
            let conn = guard.conn();
            let begin = match conn {
                Db::Sqlite(_) => "BEGIN IMMEDIATE; SELECT 1;",
                Db::Postgres(_) => "BEGIN; SELECT 1;",
            };
            conn.execute_batch(begin).expect("hold_write_txn: BEGIN");
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

#[cfg(test)]
mod tests {
    use super::*;

    /// The banner's "metadata backend" line reads this field off the state;
    /// the README promises the kind name per backend. SQLite side here
    /// (the Postgres side is pinned by m9_pg_test's route leg against a
    /// live server).
    #[test]
    fn meta_backend_is_the_configured_kind_name_sqlite() {
        let dir = tempfile::tempdir().unwrap();
        let data = DataDir::create(dir.path()).unwrap();
        let state = ServerState::open(data).unwrap();
        assert_eq!(state.meta_backend, "sqlite");
    }
}
