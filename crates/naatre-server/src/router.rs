//! The metadata routing layer (M13a): every statement's connection is
//! either the CATALOG (global tables: principals, tokens, audit_log, the
//! repo registry, the `objects/` CAS ledger, `gc_pending`, `derive_kinds`,
//! `meta`) or one REPO's per-repo SQLite file (everything repo-scoped,
//! redactions included).
//!
//! Two connection disciplines, both preserved from the single-DB design:
//!
//! * **Reads** run on read-only pooled connections (WAL keeps them
//!   lock-free beside writers). [`ReadCx`] is the multi-DB read pass: the
//!   catalog connection plus per-repo checkouts ([`ReadCx::repo`]), so
//!   the GC mark, fsck, and the purge probes can walk every repo and
//!   aggregate in memory.
//! * **Writes** run behind per-connection std mutexes taken with
//!   `try_lock` — the single-writer slot, scoped per database. The
//!   catalog has its slot (global writes: repo create, principals,
//!   tokens, the middleware's audit appends); each repo has its own (two
//!   repos land CONCURRENTLY on separate slots — the plan's M13 check),
//!   and a repo-slot holder's incidental catalog writes (audit rows,
//!   `objects` upserts) go through the one aux connection
//!   ([`RepoCx::cat`]), checked out per STATEMENT so a holder never pins
//!   it across a transaction. SQLite's busy timeout (5 s) absorbs the
//!   resulting cross-slot statement contention; the Postgres backend
//!   never splits and never reaches the split arms.
//!
//! [`Router::with_all`] is the coordinated batch: the sweep-side writes
//! (GC collection, redaction purge, snapshot-tree sweep) must
//! re-verify references and delete atomically ACROSS databases — the
//! property the single-DB write slot gave them for free. A coordinated
//! batch sets the sweep flag first (repo-scoped writes answer retryable
//! `writer_busy` while it is held — exactly the shape a single-DB GC
//! batch produced by holding THE slot), then takes the catalog slot and
//! every repo slot. Any slot busy → everything released and
//! `Err(writer_busy)` — the batched passes already treat that as
//! end-of-pass.
//!
//! In [`Layout::Single`] mode every context collapses onto the one store:
//! `RepoCx::cat()` hands back `cx.db`, `write_repo` takes the store slot,
//! `with_all` is the plain write — behaviorally identical to the
//! pre-M13a server (statement routing disappears; single-DB is still
//! single-writer, the documented profile limit).

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, TryLockError};

use naatre_store::layout::{Layout, repo_db_path};
use naatre_store::store::{open_repo_conn, open_repo_db};
use naatre_store::{Db, Store, StoreError};

use crate::api_code::ApiCode;
use crate::api_error::ApiError;

/// Read-pool size for the catalog (and the single-DB store): four
/// readers, the pre-M13a pool shape.
pub(crate) const CATALOG_READ_POOL_SIZE: usize = 4;
/// Read-pool size PER REPO (split mode): two readers per repo file. The
/// per-repo read traffic is that repo's GET paths; two connection slots
/// keep them queue-free without one-big-pool fd pressure as repos grow
/// (the design.md per-repo connection-cache cost row).
pub(crate) const REPO_READ_POOL_SIZE: usize = 2;

fn writer_busy() -> ApiError {
    ApiError::new(ApiCode::WriterBusy, "writer busy").retryable(true)
}

fn lock_or_busy<T>(mutex: &Mutex<T>) -> Result<MutexGuard<'_, T>, ApiError> {
    match mutex.try_lock() {
        Ok(guard) => Ok(guard),
        Err(TryLockError::WouldBlock) => Err(writer_busy()),
        Err(TryLockError::Poisoned(poisoned)) => Ok(poisoned.into_inner()),
    }
}

/// Pooled read-only connections: the catalog's pool plus one lazily-built
/// pool per repo file. Single mode owns just the catalog pool (it IS the
/// store's pool, over `naatre.db`).
pub(crate) struct ReadPools {
    cat: Vec<Arc<Mutex<Db>>>,
    next: AtomicUsize,
    /// repo id → its read pool. The guard covers map access and pool
    /// construction only (a per-repo pool is built once).
    repos: Mutex<HashMap<i64, Vec<Arc<Mutex<Db>>>>>,
    /// The data dir the repo files live under (split mode); `None` in
    /// single mode (repo checkouts return catalog-pool connections).
    data: Option<PathBuf>,
}

impl ReadPools {
    /// Opens `size` read-only connections to `db_path`. Callers run this
    /// AFTER the write connection opened the file: the schema must exist
    /// and the WAL must be recovered before a read-only connection can
    /// open.
    fn open_ro(db_path: &Path, size: usize) -> Result<Vec<Arc<Mutex<Db>>>, StoreError> {
        (0..size)
            .map(|_| {
                let conn = rusqlite::Connection::open_with_flags(
                    db_path,
                    rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY,
                )
                .map_err(StoreError::Sqlite)?;
                conn.busy_timeout(std::time::Duration::from_millis(
                    naatre_store::store::BUSY_TIMEOUT_MS,
                ))
                .map_err(StoreError::Sqlite)?;
                Ok(Arc::new(Mutex::new(Db::Sqlite(conn))))
            })
            .collect()
    }

    /// The single-DB pool over `db_path` (`naatre.db`).
    pub(crate) fn open(db_path: &Path) -> Result<ReadPools, StoreError> {
        Ok(ReadPools {
            cat: Self::open_ro(db_path, CATALOG_READ_POOL_SIZE)?,
            next: AtomicUsize::new(0),
            repos: Mutex::new(HashMap::new()),
            data: None,
        })
    }

    /// The split-mode pools: catalog pool over `db_path` (`catalog.db`)
    /// plus lazily-built per-repo pools under `data`.
    pub(crate) fn open_split(db_path: &Path, data: PathBuf) -> Result<ReadPools, StoreError> {
        Ok(ReadPools {
            cat: Self::open_ro(db_path, CATALOG_READ_POOL_SIZE)?,
            next: AtomicUsize::new(0),
            repos: Mutex::new(HashMap::new()),
            data: Some(data),
        })
    }

    /// The Postgres pool (never split): the sessions front the one
    /// database, and repo checkouts return catalog-pool connections.
    pub(crate) fn from_pg(conns: Vec<Arc<Mutex<Db>>>) -> ReadPools {
        ReadPools {
            cat: conns,
            next: AtomicUsize::new(0),
            repos: Mutex::new(HashMap::new()),
            data: None,
        }
    }

    /// The next catalog connection, round-robin.
    pub(crate) fn checkout_cat(&self) -> Arc<Mutex<Db>> {
        let i = self.next.fetch_add(1, Ordering::Relaxed) % self.cat.len();
        Arc::clone(&self.cat[i])
    }

    /// One of the repo's read connections, building the repo's pool on
    /// first use (read-only connections to the repo's file; the write
    /// connection created the WAL at repo-create/migration time, so the
    /// read-only opens succeed). Load balances across the pool with the
    /// shared round-robin counter.
    pub(crate) fn checkout_repo(&self, repo_id: i64) -> Result<Arc<Mutex<Db>>, StoreError> {
        let mut repos = self.repos.lock().unwrap_or_else(PoisonError::into_inner);
        repos.entry(repo_id).or_insert_with(|| -> _ {
            let data = self
                .data
                .as_ref()
                .expect("a repo connection checkout never runs on a single-DB store");
            Self::open_ro(&repo_db_path(data, repo_id), REPO_READ_POOL_SIZE)
                .expect("the repo file was migrated when its rows were written")
        });
        let pool = &repos[&repo_id];
        let i = self.next.fetch_add(1, Ordering::Relaxed) % pool.len();
        Ok(Arc::clone(&pool[i]))
    }

    /// Drops a repo's read pool (used by the migration tests' teardown
    /// symmetry; production never deletes a repo DB in this slice).
    #[allow(dead_code)]
    pub(crate) fn drop_repo_pool(&self, repo_id: i64) {
        self.repos
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(&repo_id);
    }
}

/// Where a [`RepoCx`]'s catalog access comes from. The `Arc` is borrowed
/// (the caller's closure owns it), so the per-call guard's lifetime rides
/// on `'a`.
pub(crate) enum CatSource<'a> {
    /// Single mode: the catalog IS the repo connection.
    Same,
    /// Split mode: one pooled catalog connection, locked per call (the
    /// read path pools read-only catalog connections; the write path
    /// locks the one aux read-write connection per STATEMENT, so a
    /// repo-slot holder never pins it across a transaction).
    Pooled(&'a Arc<Mutex<Db>>),
}

/// The scope a repo-scoped closure works against: the repo's id (the
/// registry row, resolved before the closure ran), its connection, and
/// the catalog. Single mode: `cat()` hands back `db` — one connection,
/// exactly the pre-M13a closure view.
pub struct RepoCx<'a> {
    /// The repo's id.
    pub id: i64,
    /// The repo's connection: the write slot's connection in
    /// [`ServerState::write_repo`](crate::state::ServerState::write_repo),
    /// a pooled read-only connection in
    /// [`ServerState::read_repo`](crate::state::ServerState::read_repo).
    pub db: &'a Db,
    source: CatSource<'a>,
}

impl<'a> RepoCx<'a> {
    pub(crate) fn new(id: i64, db: &'a Db, source: CatSource<'a>) -> RepoCx<'a> {
        RepoCx { id, db, source }
    }

    /// The catalog connection. Per-STATEMENT hold in split mode; the same
    /// connection as [`RepoCx::db`] in single mode. Global-statement
    /// calls (`audit_append`, `object_row_upsert`, `derive_kind_seed`,
    /// the `repos`-table reads) route here. The std [`Mutex`] lock never
    /// fails (poisoning is recovered past — the store stays usable), so
    /// this is infallible; pass `&cx.cat()` to statement calls (deref
    /// coercion hands the statement layer its `&Db`).
    pub fn cat(&self) -> CatGuard<'a> {
        match &self.source {
            CatSource::Same => CatGuard::Borrowed(self.db),
            CatSource::Pooled(arc) => {
                let guard = arc.lock().unwrap_or_else(PoisonError::into_inner);
                CatGuard::Held(guard)
            }
        }
    }
}

/// A held catalog connection: derefs to [`Db`].
pub enum CatGuard<'a> {
    Borrowed(&'a Db),
    Held(MutexGuard<'a, Db>),
}

impl std::ops::Deref for CatGuard<'_> {
    type Target = Db;
    fn deref(&self) -> &Db {
        match self {
            CatGuard::Borrowed(db) => db,
            CatGuard::Held(guard) => guard,
        }
    }
}

/// The multi-repo read pass: the catalog connection (held for the pass)
/// plus per-repo checkouts ([`ReadCx::with_repo`], one statement group at
/// a time — the same per-statement WAL snapshot model the single-DB read
/// pool has always had). Constructed inside the blocking closure (its
/// guards live on that stack — never across an `.await`).
pub struct ReadCx<'a> {
    cat: MutexGuard<'a, Db>,
    pools: &'a ReadPools,
}

impl<'a> ReadCx<'a> {
    pub(crate) fn new(pools: &'a ReadPools) -> ReadCx<'a> {
        // Borrow the pool slot THROUGH the 'a reference, so the guard's
        // lifetime is the pass's (the Arc outlives it inside `pools`).
        let i = pools.next.fetch_add(1, Ordering::Relaxed) % pools.cat.len();
        let arc: &'a Arc<Mutex<Db>> = &pools.cat[i];
        let cat = arc.lock().unwrap_or_else(PoisonError::into_inner);
        ReadCx { cat, pools }
    }

    /// The catalog connection (single mode: the store).
    pub fn cat(&self) -> &Db {
        &self.cat
    }

    /// Whether this pass runs against a split (db-per-repo) store — the
    /// per-FILE reads (fsck's `tree_entries` inventory) need the file
    /// boundary: split mode reads once per repo file, single mode once
    /// against the whole DB.
    pub fn is_split(&self) -> bool {
        self.pools.data.is_some()
    }

    /// Runs one read against repo `repo_id`'s pooled read-only
    /// connection. The connection is checked out for the closure's
    /// duration only. SINGLE mode: the catalog connection this pass
    /// already holds IS the repo's file — reuse it. Checking out another
    /// pool connection here would round-robin onto the one this pass
    /// holds (the rotation counter is shared with every other read path)
    /// and self-deadlock.
    pub fn with_repo<R>(
        &self,
        _repo_id: i64,
        f: impl FnOnce(&Db) -> Result<R, ApiError>,
    ) -> Result<R, ApiError> {
        if self.pools.data.is_none() {
            return f(&self.cat);
        }
        let conn = self
            .pools
            .checkout_repo(_repo_id)
            .map_err(ApiError::log_internal)?;
        let guard = conn.lock().unwrap_or_else(PoisonError::into_inner);
        f(&guard)
    }
}

/// The coordinated write batch's scope: the catalog slot plus every repo
/// slot, held together so a sweep-side re-verify + delete stays atomic
/// across databases. Single mode: `cat()` is the store connection,
/// `repo()` returns it for every id, and `repo_ids()` lists the REGISTRY
/// ids (the aggregation passes iterate them exactly as in split mode —
/// every per-repo statement lands on the one store connection; without
/// this the sweep's probe and collect loops would silently skip in
/// single-DB stores).
pub struct AllCx<'a> {
    store: MutexGuard<'a, Store>,
    repos: Vec<(i64, MutexGuard<'a, Db>)>,
    ids: Vec<i64>,
}

impl AllCx<'_> {
    /// The catalog connection (the batch's catalog transaction conn; the
    /// slot is held for the batch).
    pub fn cat(&self) -> &Db {
        self.store.conn()
    }

    /// One repo's connection (single mode: the store).
    pub fn repo(&self, repo_id: i64) -> Result<&Db, ApiError> {
        match self.repos.iter().find(|(id, _)| *id == repo_id) {
            Some((_, guard)) => Ok(guard),
            None if self.repos.is_empty() => Ok(self.store.conn()),
            None => Err(ApiError::log_internal(format!(
                "coordinated batch: repo {repo_id} has no locked connection"
            ))),
        }
    }

    /// The repo ids this batch covers (the registry listing taken under
    /// the catalog slot; the aggregation passes iterate these).
    pub fn repo_ids(&self) -> impl Iterator<Item = i64> + '_ {
        if self.repos.is_empty() {
            Box::new(self.ids.iter().copied()) as Box<dyn Iterator<Item = i64> + '_>
        } else {
            Box::new(self.repos.iter().map(|(id, _)| *id))
        }
    }
}

/// The per-database connection machinery behind the
/// [`ServerState`](crate::state::ServerState) entry points.
pub(crate) struct Router {
    pub(crate) layout: Layout,
    /// Per-repo write connections (split mode): one mutex per repo file,
    /// opened on first use. Single mode: never touched (repo writes take
    /// the store slot).
    pub(crate) repo_write: Mutex<HashMap<i64, Arc<Mutex<Db>>>>,
    /// The aux catalog connection (split mode): repo-slot holders'
    /// per-statement catalog access. Single mode: never touched.
    aux: Mutex<Option<Arc<Mutex<Db>>>>,
    /// Set for the duration of a coordinated batch: repo-scoped writes
    /// answer retryable `writer_busy` while it is held (the single-DB
    /// contention shape — a sweep batch blocked ALL writes by holding the
    /// one slot).
    pub(crate) sweep_holding: AtomicBool,
    data: PathBuf,
}

impl Router {
    pub(crate) fn new(layout: Layout, data: &Path) -> Router {
        Router {
            layout,
            repo_write: Mutex::new(HashMap::new()),
            aux: Mutex::new(None),
            sweep_holding: AtomicBool::new(false),
            data: data.to_path_buf(),
        }
    }

    pub(crate) fn is_split(&self) -> bool {
        self.layout == Layout::DbPerRepo
    }

    /// The repo's write connection, opened on first use (the file runs
    /// the shared migrations on open — idempotent for an existing file).
    /// The caller then `try_lock`s the returned mutex (the repo slot).
    pub(crate) fn repo_write_conn(&self, repo_id: i64) -> Result<Arc<Mutex<Db>>, StoreError> {
        let mut map = self
            .repo_write
            .lock()
            .unwrap_or_else(PoisonError::into_inner);
        if let Some(conn) = map.get(&repo_id) {
            return Ok(Arc::clone(conn));
        }
        let conn = Arc::new(Mutex::new(open_repo_db(&repo_db_path(
            &self.data, repo_id,
        ))?));
        map.insert(repo_id, Arc::clone(&conn));
        Ok(conn)
    }

    /// The aux catalog connection, opened on first use (read-write; the
    /// catalog read pool stays read-only).
    pub(crate) fn aux_conn(&self) -> Result<Arc<Mutex<Db>>, StoreError> {
        let mut slot = self.aux.lock().unwrap_or_else(PoisonError::into_inner);
        if let Some(conn) = &*slot {
            return Ok(Arc::clone(conn));
        }
        let path = self.data.join(naatre_store::layout::CATALOG_FILE);
        let conn = Arc::new(Mutex::new(open_repo_conn(&path)?));
        *slot = Some(Arc::clone(&conn));
        Ok(conn)
    }

    /// True while a coordinated batch holds the slots: repo-scoped writes
    /// refuse up front (retryable `writer_busy`), so no new writer can
    /// slip between the batch's re-verification reads and its deletes.
    pub(crate) fn sweep_in_flight(&self) -> bool {
        self.sweep_holding.load(Ordering::SeqCst)
    }

    /// Runs `f` under a coordinated batch. Single mode: the plain store
    /// slot (the old `state.write` shape; repo ids route to the store).
    /// Split mode: the sweep flag first (new repo writers refuse), then
    /// the catalog slot and every repo slot — ANY slot busy releases
    /// everything and answers retryable `writer_busy`.
    pub(crate) fn with_all<T, F>(&self, store: &Mutex<Store>, f: F) -> Result<T, ApiError>
    where
        F: FnOnce(&mut AllCx<'_>) -> Result<T, ApiError>,
    {
        if !self.is_split() {
            let guard = lock_or_busy(store)?;
            // The registry listing under the slot (create/delete serialize
            // here — none can commit mid-batch), so the aggregation loops
            // cover every repo exactly as the split arms do.
            let ids = crate::db::gc_repos(guard.conn()).map_err(ApiError::log_internal)?;
            let mut cx = AllCx {
                store: guard,
                repos: Vec::new(),
                ids,
            };
            return f(&mut cx);
        }
        self.sweep_holding.store(true, Ordering::SeqCst);
        let out = self.with_all_split(store, f);
        self.sweep_holding.store(false, Ordering::SeqCst);
        out
    }

    /// The split-mode coordinated batch body. Slot order: catalog first
    /// (repo create/delete serialize on it — none can commit mid-batch),
    /// then the repos in ascending id order (a fixed order, so the
    /// all-locks pattern cannot deadlock against another all-locker).
    fn with_all_split<T, F>(&self, store: &Mutex<Store>, f: F) -> Result<T, ApiError>
    where
        F: FnOnce(&mut AllCx<'_>) -> Result<T, ApiError>,
    {
        let store_guard = lock_or_busy(store)?;
        // The registry listing comes from the batch's own catalog
        // connection (the slot is held; the listing cannot race a create).
        let repo_ids = crate::db::gc_repos(store_guard.conn()).map_err(ApiError::log_internal)?;
        let mut conns: Vec<Arc<Mutex<Db>>> = Vec::with_capacity(repo_ids.len());
        for id in &repo_ids {
            match self.repo_write_conn(*id) {
                Ok(conn) => conns.push(conn),
                Err(e) => {
                    drop(store_guard);
                    return Err(ApiError::log_internal(e));
                }
            }
        }
        let mut guards = Vec::with_capacity(conns.len());
        for (id, conn) in repo_ids.iter().zip(&conns) {
            match conn.try_lock() {
                Ok(guard) => guards.push((*id, guard)),
                Err(TryLockError::WouldBlock) => {
                    // Release everything (guards + store slot), unset the
                    // flag in the caller, answer the retryable busy.
                    drop(guards);
                    drop(store_guard);
                    return Err(writer_busy());
                }
                Err(TryLockError::Poisoned(poisoned)) => guards.push((*id, poisoned.into_inner())),
            }
        }
        let mut cx = AllCx {
            store: store_guard,
            repos: guards,
            ids: repo_ids,
        };
        f(&mut cx)
    }
}
