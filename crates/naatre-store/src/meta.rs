//! The metadata connection boundary (M9): one statement surface over two
//! real backends — embedded SQLite (the zero-config default) and Postgres
//! 16+ (selectable via `backend.toml`).
//!
//! Shape (chosen for least duplication): the SQL stays in the server crate
//! as single-source strings written in the SQLite dialect, and this module
//! parameterizes the CONNECTION behind the [`Db`] enum with the handful of
//! primitives that surface needs — execute, one-row query, multi-row query,
//! id-returning insert, batch execute, and transactions. Per-backend work
//! is mechanical and lives here, not at every statement site:
//!
//! * placeholders: `?N` (SQLite) is rewritten to `$N` (Postgres) when the
//!   statement runs on PG — naatre SQL contains no other `?` (enforced:
//!   a stray `?` is a [`DbError::BadSql`], not a silent mis-bind);
//! * `INSERT OR IGNORE` / `INSERT OR REPLACE` become
//!   `… ON CONFLICT DO NOTHING` / `… ON CONFLICT (…) DO UPDATE …`
//!   ([`Sql::ignore`] / [`Sql::replace`] carry the conflict target);
//! * SQLite `rowid` ordering points ([`Db::rowid`]) map to the explicit
//!   `seq` identity columns the Postgres migrations add to `changesets`
//!   and `derive_state`;
//! * `last_insert_rowid()` becomes `RETURNING id` ([`Db::insert_id`]).
//!
//! Everything else in the schema surface — `ON CONFLICT (…) DO UPDATE`
//! upserts, recursive CTEs, the `meta.schema_version` guard — is already
//! portable and runs verbatim on both backends.
//!
//! Synchronous by design, like the rest of the crate: both clients are
//! blocking, and the async server reaches them through `spawn_blocking`.
//! The Postgres client sits behind a `RefCell` because every operation
//! materializes its rows before returning, so no borrow ever outlives a
//! call (the single-writer slot is the server's mutex, one level up).
//!
//! Transactions are connection-state, matching SQLite's model: BEGIN /
//! COMMIT / ROLLBACK run through the same [`Db`] handle, a nested begin is
//! refused ([`DbError::NestedTransaction`]), and the [`DbTx`] guard rolls
//! back on drop — the shape `land`'s `TxnGuard` already relies on.
//!
//! Driver-error redaction: Postgres error TEXT can embed connection
//! parameters (host, user, even the DSN in some connect paths), so
//! [`DbError::Postgres`] never carries it — only a classified
//! [`PgErrKind`]. SQLite error text names at most the database file, which
//! the banner already names.

use std::cell::{Cell, RefCell};

use rusqlite::Connection;

/// The two real metadata backends.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dialect {
    Sqlite,
    Postgres,
}

// ---- statement shapes ------------------------------------------------------

/// How a statement's `INSERT` conflicts resolve, per backend.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SqlShape {
    /// Verbatim (the common case: plain SELECT/INSERT/UPDATE/DELETE, and
    /// the already-portable `ON CONFLICT (…) DO UPDATE` upserts).
    Plain,
    /// `INSERT OR IGNORE` → PG `INSERT … ON CONFLICT DO NOTHING`.
    Ignore,
    /// `INSERT OR REPLACE` → PG
    /// `INSERT … ON CONFLICT (conflict) DO UPDATE SET set = EXCLUDED.set …`.
    Replace {
        conflict: &'static [&'static str],
        set: &'static [&'static str],
    },
}

/// A statement in the single-source (SQLite-flavored) surface, with the
/// shape marker the Postgres conversion needs.
#[derive(Debug, Clone)]
pub struct Sql {
    text: std::borrow::Cow<'static, str>,
    shape: SqlShape,
}

impl Sql {
    pub const fn plain(text: &'static str) -> Sql {
        Sql {
            text: std::borrow::Cow::Borrowed(text),
            shape: SqlShape::Plain,
        }
    }

    /// Owned variant for statements assembled at call time (the rowid
    /// ordering fragments).
    pub fn plain_owned(text: String) -> Sql {
        Sql {
            text: std::borrow::Cow::Owned(text),
            shape: SqlShape::Plain,
        }
    }

    /// [`Sql::plain_owned`] for the `INSERT OR IGNORE` shape.
    pub fn ignore_owned(text: String) -> Sql {
        Sql {
            text: std::borrow::Cow::Owned(text),
            shape: SqlShape::Ignore,
        }
    }

    pub const fn ignore(text: &'static str) -> Sql {
        Sql {
            text: std::borrow::Cow::Borrowed(text),
            shape: SqlShape::Ignore,
        }
    }

    pub const fn replace(
        text: &'static str,
        conflict: &'static [&'static str],
        set: &'static [&'static str],
    ) -> Sql {
        Sql {
            text: std::borrow::Cow::Borrowed(text),
            shape: SqlShape::Replace { conflict, set },
        }
    }

    fn text(&self) -> &str {
        &self.text
    }

    /// Postgres text: placeholder rewrite + shape-specific conflict clause.
    fn postgres_sql(&self) -> Result<String, DbError> {
        let body = placeholders_to_pg(self.text())?;
        Ok(match self.shape {
            SqlShape::Plain => body,
            SqlShape::Ignore => {
                let Some(rest) = body.strip_prefix("INSERT OR IGNORE") else {
                    return Err(DbError::BadSql(
                        "Sql::ignore statement must start with `INSERT OR IGNORE`".into(),
                    ));
                };
                format!("INSERT{rest} ON CONFLICT DO NOTHING")
            }
            SqlShape::Replace { conflict, set } => {
                let Some(rest) = body.strip_prefix("INSERT OR REPLACE") else {
                    return Err(DbError::BadSql(
                        "Sql::replace statement must start with `INSERT OR REPLACE`".into(),
                    ));
                };
                let sets = set
                    .iter()
                    .map(|c| format!("{c} = EXCLUDED.{c}"))
                    .collect::<Vec<_>>()
                    .join(", ");
                format!(
                    "INSERT{rest} ON CONFLICT ({}) DO UPDATE SET {sets}",
                    conflict.join(", ")
                )
            }
        })
    }
}

/// `?N` → `$N`. A `?` NOT followed by a digit is refused: every statement
/// on this surface uses numbered placeholders, so a bare `?` would mis-bind
/// silently on PG (SQLite would treat it as anonymous and also break the
/// rewrite).
fn placeholders_to_pg(sql: &str) -> Result<String, DbError> {
    let mut out = String::with_capacity(sql.len() + 8);
    let mut chars = sql.chars().peekable();
    while let Some(c) = chars.next() {
        if c == '?' {
            match chars.peek() {
                Some(d) if d.is_ascii_digit() => {
                    out.push('$');
                    while let Some(d) = chars.peek() {
                        if d.is_ascii_digit() {
                            out.push(*d);
                            chars.next();
                        } else {
                            break;
                        }
                    }
                }
                _ => {
                    return Err(DbError::BadSql(
                        "bare `?` in SQL: this surface uses numbered `?N` placeholders only".into(),
                    ));
                }
            }
        } else {
            out.push(c);
        }
    }
    Ok(out)
}

// ---- params ----------------------------------------------------------------

/// One bound parameter. The surface's value domain: 64-bit integers, UTF-8
/// text, bytes, NULL — exactly what the schema's column types hold.
#[derive(Debug, Clone, PartialEq)]
pub enum P {
    Null,
    I(i64),
    S(String),
    B(Vec<u8>),
}

/// Conversion into a [`P`], so call sites stay terse (`pv![a, b]`).
pub trait IntoP {
    fn into_p(self) -> P;
}

impl IntoP for P {
    fn into_p(self) -> P {
        self
    }
}
impl IntoP for i64 {
    fn into_p(self) -> P {
        P::I(self)
    }
}
impl IntoP for i32 {
    fn into_p(self) -> P {
        P::I(i64::from(self))
    }
}
impl IntoP for u8 {
    fn into_p(self) -> P {
        P::I(i64::from(self))
    }
}
impl IntoP for usize {
    fn into_p(self) -> P {
        P::I(i64::try_from(self).expect("parameter fits i64: the surface binds counts and ids"))
    }
}
impl IntoP for bool {
    fn into_p(self) -> P {
        P::I(i64::from(self))
    }
}
impl IntoP for &str {
    fn into_p(self) -> P {
        P::S(self.to_string())
    }
}
impl IntoP for String {
    fn into_p(self) -> P {
        P::S(self)
    }
}
impl IntoP for &String {
    fn into_p(self) -> P {
        P::S(self.clone())
    }
}
impl IntoP for &[u8] {
    fn into_p(self) -> P {
        P::B(self.to_vec())
    }
}
impl<const N: usize> IntoP for &[u8; N] {
    fn into_p(self) -> P {
        P::B(self.to_vec())
    }
}
impl IntoP for [u8; 32] {
    fn into_p(self) -> P {
        P::B(self.to_vec())
    }
}
impl IntoP for Vec<u8> {
    fn into_p(self) -> P {
        P::B(self)
    }
}
impl IntoP for &Vec<u8> {
    fn into_p(self) -> P {
        P::B(self.clone())
    }
}
impl IntoP for &naatre_core::ids::Id {
    fn into_p(self) -> P {
        P::B(self.0.to_vec())
    }
}
impl IntoP for naatre_core::ids::Id {
    fn into_p(self) -> P {
        P::B(self.0.to_vec())
    }
}
impl<T: IntoP> IntoP for Option<T> {
    fn into_p(self) -> P {
        match self {
            Some(v) => v.into_p(),
            None => P::Null,
        }
    }
}

/// Builds the parameter vector for a statement (`pv![a, b, c]`).
#[macro_export]
macro_rules! pv {
    () => { ::std::vec::Vec::<$crate::meta::P>::new() };
    ($($e:expr),+ $(,)?) => {
        ::std::vec![$($crate::meta::IntoP::into_p($e)),+]
    };
}

// ---- rows ------------------------------------------------------------------

/// One materialized column value.
#[derive(Debug, Clone, PartialEq)]
pub enum Val {
    Null,
    I(i64),
    S(String),
    B(Vec<u8>),
}

/// One materialized row. Typed access by column index, mirroring the
/// `row.get(i)` shape the single-source SQL surface already uses.
#[derive(Debug, Clone)]
pub struct Row(Vec<Val>);

impl Row {
    pub fn get<T: FromVal>(&self, idx: usize) -> Result<T, DbError> {
        T::from_val(
            self.0
                .get(idx)
                .ok_or_else(|| DbError::ColumnIndex(idx, self.0.len()))?,
        )
    }
}

/// Typed extraction from a [`Val`], mirroring the types the schema holds.
pub trait FromVal: Sized {
    fn from_val(v: &Val) -> Result<Self, DbError>;
}

impl FromVal for i64 {
    fn from_val(v: &Val) -> Result<Self, DbError> {
        match v {
            Val::I(i) => Ok(*i),
            other => Err(DbError::ColumnType {
                want: "i64",
                got: other.clone(),
            }),
        }
    }
}

impl FromVal for String {
    fn from_val(v: &Val) -> Result<Self, DbError> {
        match v {
            Val::S(s) => Ok(s.clone()),
            other => Err(DbError::ColumnType {
                want: "String",
                got: other.clone(),
            }),
        }
    }
}

impl FromVal for Vec<u8> {
    fn from_val(v: &Val) -> Result<Self, DbError> {
        match v {
            Val::B(b) => Ok(b.clone()),
            other => Err(DbError::ColumnType {
                want: "Vec<u8>",
                got: other.clone(),
            }),
        }
    }
}

impl FromVal for bool {
    fn from_val(v: &Val) -> Result<Self, DbError> {
        Ok(i64::from_val(v)? != 0)
    }
}

impl<T: FromVal> FromVal for Option<T> {
    fn from_val(v: &Val) -> Result<Self, DbError> {
        match v {
            Val::Null => Ok(None),
            other => T::from_val(other).map(Some),
        }
    }
}

// ---- errors ----------------------------------------------------------------

/// The classified Postgres failure kinds that survive redaction. Only the
/// SQLSTATE class matters to callers (the unique-violation mapping and the
/// serialization-retry shape); everything else the driver would print —
/// including connection parameters — is withheld.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PgErrKind {
    /// Constraint or other statement-level rejection.
    Query,
    /// Could not establish (or lost) the session.
    Connect,
}

/// Errors from the metadata surface. `Display` is safe for banners and
/// logs by construction: the Postgres arm never carries driver text.
#[derive(Debug)]
pub enum DbError {
    Sqlite(rusqlite::Error),
    Postgres {
        kind: PgErrKind,
        state: Option<&'static str>,
    },
    BadSql(String),
    NoRows,
    NestedTransaction,
    ColumnIndex(usize, usize),
    ColumnType {
        want: &'static str,
        got: Val,
    },
    /// A column arrived in a type the surface does not model — a schema
    /// mismatch, never a runtime input.
    ColumnDomain {
        col: String,
        backend: &'static str,
    },
    Io(std::io::Error),
}

impl From<rusqlite::Error> for DbError {
    fn from(e: rusqlite::Error) -> Self {
        DbError::Sqlite(e)
    }
}

impl From<std::io::Error> for DbError {
    fn from(e: std::io::Error) -> Self {
        DbError::Io(e)
    }
}

impl std::fmt::Display for DbError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            // SQLite error text names at most the database file (the banner
            // names it too); it never carries connection parameters.
            DbError::Sqlite(e) => write!(f, "sqlite error: {e}"),
            DbError::Postgres { kind, state: _ } => match kind {
                PgErrKind::Connect => write!(
                    f,
                    "postgres connection failed (driver details withheld: \
                     connect errors can embed DSN, host, and user)"
                ),
                PgErrKind::Query => write!(
                    f,
                    "postgres statement failed (driver details withheld: \
                     driver errors can embed connection parameters)"
                ),
            },
            DbError::BadSql(why) => write!(f, "bad SQL on the statement surface: {why}"),
            DbError::NoRows => write!(f, "query returned no rows"),
            DbError::NestedTransaction => write!(
                f,
                "nested transaction refused: the previous transaction on \
                 this connection must commit or roll back first"
            ),
            DbError::ColumnIndex(idx, len) => {
                write!(f, "column index {idx} out of range ({len} columns)")
            }
            DbError::ColumnType { want, got } => {
                write!(f, "column type mismatch: expected {want}, got {got:?}")
            }
            DbError::ColumnDomain { col, backend } => write!(
                f,
                "column {col:?} has a type {backend} does not map onto the \
                 statement surface's value domain"
            ),
            DbError::Io(e) => write!(f, "io error: {e}"),
        }
    }
}

impl std::error::Error for DbError {}

impl DbError {
    /// True when `err` is a UNIQUE-constraint violation (the caller maps it
    /// to its semantic error, e.g. `repo_exists`).
    pub fn is_unique_violation(&self) -> bool {
        match self {
            DbError::Sqlite(e) => matches!(
                e,
                rusqlite::Error::SqliteFailure(ffi, Some(_))
                    if ffi.code == rusqlite::ErrorCode::ConstraintViolation
            ),
            DbError::Postgres { state, .. } => *state == Some("23505"),
            _ => false,
        }
    }

    /// True when the backend refused this statement because another writer
    /// holds conflicting state — SQLite lock busy, or the Postgres MVCC
    /// equivalents (serialization failure, deadlock, lock unavailable).
    /// The server answers these with the writer-busy-shaped retryable 503.
    pub fn is_retryable_contention(&self) -> bool {
        match self {
            DbError::Sqlite(e) => matches!(
                e,
                rusqlite::Error::SqliteFailure(ffi, _)
                    if ffi.code == rusqlite::ErrorCode::DatabaseBusy
            ),
            DbError::Postgres { state, .. } => {
                matches!(state, Some("40001") | Some("40P01") | Some("55P03"))
            }
            _ => false,
        }
    }
}

// ---- the Postgres handle ----------------------------------------------------

#[doc(hidden)]
pub struct PgDb {
    /// `Option` so `Drop` can MOVE the client out (its own Drop talks to
    /// the server and must run off runtime threads); present until then.
    client: RefCell<Option<Box<postgres::Client>>>,
    in_tx: Cell<bool>,
}

// Manual Debug: the driver's client has none, and a derive would be
// skipped anyway — connection internals must never reach a Debug print.
impl std::fmt::Debug for PgDb {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PgDb")
            .field("in_tx", &self.in_tx.get())
            .finish_non_exhaustive()
    }
}

impl Drop for PgDb {
    fn drop(&mut self) {
        // The client's Drop talks to the server through its internal
        // runtime — which refuses to start on a tokio worker thread, and
        // ServerState drops wherever its last handle dies (often a
        // runtime worker at shutdown). Park the client on a helper
        // thread and drop it there.
        if let Some(client) = self.client.get_mut().take() {
            std::thread::spawn(move || drop(client));
        }
    }
}

impl PgDb {
    fn with_client<R>(
        &self,
        f: impl FnOnce(&mut postgres::Client) -> Result<R, postgres::Error>,
    ) -> Result<R, DbError> {
        let mut client = self.client.borrow_mut();
        f(client.as_mut().expect("client present until drop")).map_err(pg_err)
    }
}

/// The fixed transaction-scoped advisory lock key for IMMEDIATE
/// transactions on Postgres — the cross-process single-writer mechanism
/// (ASCII "naatre_wr" as an i64; any two naatre processes agree on it).
pub const PG_WRITER_ADVISORY_KEY: i64 = 0x6E61_6174_5F77_5249;

/// How long an IMMEDIATE transaction waits for the advisory lock before
/// failing as retryable contention — the Postgres mirror of the SQLite
/// `BUSY_TIMEOUT_MS` pin.
pub const PG_LOCK_TIMEOUT_MS: u64 = 5000;

/// The SQLSTATE codes the surface's callers distinguish (unique-violation
/// mapping, contention retry). Any other code — and any non-DB error —
/// classifies as `None`; the Display text never differs by code beyond
/// this, so nothing else leaks.
const PG_STATES_CARRIED: [&str; 4] = ["23505", "40001", "40P01", "55P03"];

/// Classifies a driver error, keeping only a recognized SQLSTATE code —
/// never the driver's message text (which can embed connection parameters).
fn pg_err(e: postgres::Error) -> DbError {
    let kind = if e.as_db_error().is_some() {
        PgErrKind::Query
    } else {
        PgErrKind::Connect
    };
    let state = e.as_db_error().and_then(|db| {
        let code = db.code().code();
        PG_STATES_CARRIED.iter().find(|c| **c == code).copied()
    });
    DbError::Postgres { kind, state }
}

// ---- the connection ---------------------------------------------------------

/// An open metadata connection: SQLite or Postgres.
#[derive(Debug)]
pub enum Db {
    Sqlite(Connection),
    Postgres(PgDb),
}

impl Db {
    /// Connects to Postgres from the parsed [`PgConnect`] parameters
    /// ([`crate::backend`]). The DSN never re-forms here: the driver gets
    /// the structured parts, and a connect failure is REDACTED to
    /// [`DbError::Postgres`] with kind [`PgErrKind::Connect`] — driver
    /// connect errors can embed the host, user, and DSN.
    ///
    /// `read_only` mirrors the SQLite read pool's `SQLITE_OPEN_READ_ONLY`:
    /// the session refuses writes, so a bug that routes a write to a pooled
    /// read connection fails loudly on both backends.
    pub fn connect_postgres(
        pg: &crate::backend::PgConnect,
        read_only: bool,
    ) -> Result<Db, DbError> {
        // The sync driver builds its per-connection runtime at connect
        // time and refuses to start one from inside a tokio worker
        // thread. `serve` opens the store on its async main, so the
        // connect runs on a helper thread — making this constructor
        // thread-agnostic for every caller.
        let pg = pg.clone();
        std::thread::spawn(move || Self::connect_postgres_on_this_thread(&pg, read_only))
            .join()
            .unwrap_or_else(|panic| std::panic::resume_unwind(panic))
    }

    /// The connect itself; must run OFF tokio runtime threads (see
    /// [`Db::connect_postgres`]).
    fn connect_postgres_on_this_thread(
        pg: &crate::backend::PgConnect,
        read_only: bool,
    ) -> Result<Db, DbError> {
        let mut cfg = postgres::Config::new();
        cfg.host(&pg.host);
        cfg.port(pg.port);
        cfg.dbname(&pg.dbname);
        if let Some(user) = &pg.user {
            cfg.user(user);
        }
        if let Some(password) = &pg.password {
            cfg.password(password);
        }
        cfg.ssl_mode(match pg.tls {
            crate::backend::PgTls::Disable => postgres::config::SslMode::Disable,
            // The driver only distinguishes require/not; the verification
            // DEPTH lives in the TLS connector below.
            crate::backend::PgTls::Require
            | crate::backend::PgTls::VerifyCa
            | crate::backend::PgTls::VerifyFull => postgres::config::SslMode::Require,
        });
        cfg.connect_timeout(std::time::Duration::from_secs(10));

        let pg_redacted = |_: postgres::Error| DbError::Postgres {
            kind: PgErrKind::Connect,
            state: None,
        };
        let tls_redacted = |_: native_tls::Error| DbError::Postgres {
            kind: PgErrKind::Connect,
            state: None,
        };

        let client = match pg.tls {
            crate::backend::PgTls::Disable => cfg.connect(postgres::NoTls).map_err(pg_redacted)?,
            tls_mode => {
                let mut builder = native_tls::TlsConnector::builder();
                match tls_mode {
                    // `require`: encryption without certificate verification.
                    crate::backend::PgTls::Require => {
                        builder.danger_accept_invalid_certs(true);
                    }
                    // `verify-ca`: chain verified, hostname not checked.
                    crate::backend::PgTls::VerifyCa => {
                        builder.danger_accept_invalid_hostnames(true);
                    }
                    // `verify-full`: full chain + hostname verification.
                    crate::backend::PgTls::VerifyFull => {}
                    crate::backend::PgTls::Disable => unreachable!("handled above"),
                }
                let connector = builder.build().map_err(tls_redacted)?;
                cfg.connect(postgres_native_tls::MakeTlsConnector::new(connector))
                    .map_err(pg_redacted)?
            }
        };

        let db = Db::Postgres(PgDb {
            client: RefCell::new(Some(Box::new(client))),
            in_tx: Cell::new(false),
        });
        if let Some(schema) = &pg.schema {
            // Test isolation only (never parseable from backend.toml):
            // every connection for this store lands in the schema, and the
            // migration set runs inside it. BEFORE the read-only SET — a
            // read-only session cannot CREATE SCHEMA.
            db.execute_batch(&format!(
                "CREATE SCHEMA IF NOT EXISTS \"{schema}\"; SET search_path TO \"{schema}\", public"
            ))?;
        }
        if read_only {
            // A failed SET is connect-class for the pool's purposes; keep
            // it redacted. LAST, so the schema setup above stays writable.
            db.execute_batch("SET default_transaction_read_only = on")
                .map_err(|_| DbError::Postgres {
                    kind: PgErrKind::Connect,
                    state: None,
                })?;
        }
        Ok(db)
    }

    /// Runs `sql` (INSERT/UPDATE/DELETE), returning affected-row count.
    pub fn execute(&self, sql: &Sql, params: &[P]) -> Result<usize, DbError> {
        match self {
            Db::Sqlite(conn) => {
                let refs = sqlite_params(params);
                Ok(conn.prepare_cached(sql.text())?.execute(refs.as_slice())?)
            }
            Db::Postgres(pg) => {
                let pg_sql = sql.postgres_sql()?;
                let refs = pg_refs(params);
                let n = pg.with_client(|c| c.execute(pg_sql.as_str(), &refs))?;
                Ok(usize::try_from(n).unwrap_or(usize::MAX))
            }
        }
    }

    /// Runs `sql`, materializing the first row, or `None` when the query
    /// returns no rows.
    pub fn query_opt(&self, sql: &Sql, params: &[P]) -> Result<Option<Row>, DbError> {
        match self {
            Db::Sqlite(conn) => {
                let refs = sqlite_params(params);
                let mut stmt = conn.prepare_cached(sql.text())?;
                let mut rows = stmt.query(refs.as_slice())?;
                match rows.next()? {
                    Some(row) => Ok(Some(sqlite_row(row)?)),
                    None => Ok(None),
                }
            }
            Db::Postgres(pg) => {
                let pg_sql = sql.postgres_sql()?;
                let refs = pg_refs(params);
                let row = pg.with_client(|c| c.query_opt(pg_sql.as_str(), &refs))?;
                row.map(|r| pg_row(&r)).transpose()
            }
        }
    }

    /// [`Db::query_opt`] with the row mapped through `f` — the shape the
    /// statement surface's single-row lookups use.
    pub fn query_opt_map<T>(
        &self,
        sql: &Sql,
        params: &[P],
        f: impl FnOnce(&Row) -> Result<T, DbError>,
    ) -> Result<Option<T>, DbError> {
        self.query_opt(sql, params)?.map(|row| f(&row)).transpose()
    }

    /// [`Db::query_row_strict`] with the row mapped through `f`.
    pub fn query_row_map<T>(
        &self,
        sql: &Sql,
        params: &[P],
        f: impl FnOnce(&Row) -> Result<T, DbError>,
    ) -> Result<T, DbError> {
        self.query_row_strict(sql, params).and_then(|row| f(&row))
    }

    /// Runs `sql` and requires exactly the first row: no rows is
    /// [`DbError::NoRows`] (the statement surface's "must exist" reads —
    /// COUNT(*) aggregates, the just-written row re-read).
    pub fn query_row_strict(&self, sql: &Sql, params: &[P]) -> Result<Row, DbError> {
        self.query_opt(sql, params)?.ok_or(DbError::NoRows)
    }

    /// Runs `sql`, mapping every row through `f`, in result order — the
    /// shape `stmt.query_map(...)` had on the raw connection.
    pub fn query_all_map<T>(
        &self,
        sql: &Sql,
        params: &[P],
        f: impl FnMut(&Row) -> Result<T, DbError>,
    ) -> Result<Vec<T>, DbError> {
        let rows = self.query_all(sql, params)?;
        rows.iter().map(f).collect()
    }

    /// Runs `sql`, materializing every row in result order.
    pub fn query_all(&self, sql: &Sql, params: &[P]) -> Result<Vec<Row>, DbError> {
        match self {
            Db::Sqlite(conn) => {
                let refs = sqlite_params(params);
                let mut stmt = conn.prepare_cached(sql.text())?;
                let mut rows = stmt.query(refs.as_slice())?;
                let mut out = Vec::new();
                while let Some(row) = rows.next()? {
                    out.push(sqlite_row(row)?);
                }
                Ok(out)
            }
            Db::Postgres(pg) => {
                let pg_sql = sql.postgres_sql()?;
                let refs = pg_refs(params);
                let rows = pg.with_client(|c| c.query(pg_sql.as_str(), &refs))?;
                rows.iter().map(pg_row).collect()
            }
        }
    }

    /// Runs an INSERT and returns the new row's generated id: SQLite reads
    /// `last_insert_rowid()`; Postgres appends `RETURNING id` (the identity
    /// columns' value). Only for statements whose table generates an id.
    pub fn insert_id(&self, sql: &Sql, params: &[P]) -> Result<i64, DbError> {
        match self {
            Db::Sqlite(conn) => {
                let refs = sqlite_params(params);
                conn.prepare_cached(sql.text())?.execute(refs.as_slice())?;
                Ok(conn.last_insert_rowid())
            }
            Db::Postgres(pg) => {
                let pg_sql = sql.postgres_sql()? + " RETURNING id";
                let refs = pg_refs(params);
                let row = pg.with_client(|c| c.query_one(pg_sql.as_str(), &refs))?;
                pg_row(&row)?.get(0)
            }
        }
    }

    /// Runs a raw multi-statement batch. Reserved for migrations and the
    /// SQLite pragmas — the statement surface uses the typed methods.
    pub fn execute_batch(&self, sql: &str) -> Result<(), DbError> {
        match self {
            Db::Sqlite(conn) => Ok(conn.execute_batch(sql)?),
            Db::Postgres(pg) => Ok(pg.with_client(|c| c.batch_execute(sql))?),
        }
    }

    /// Begins a transaction on this connection; the [`DbTx`] guard commits
    /// explicitly and rolls back on drop. A nested begin is refused (SQLite
    /// refuses at the engine; Postgres only warns, so the check is explicit).
    pub fn transaction(&self) -> Result<DbTx<'_>, DbError> {
        self.begin(false)
    }

    /// [`Db::transaction`], taking the write lock UP FRONT — land's
    /// transaction shape (SQLite `BEGIN IMMEDIATE`).
    ///
    /// On Postgres the immediate semantic lands as the fixed
    /// [`PG_WRITER_ADVISORY_KEY`] transaction-scoped advisory lock, taken
    /// right after BEGIN with a [`PG_LOCK_TIMEOUT_MS`] statement timeout:
    /// a second writer (in another PROCESS — the in-process slot already
    /// serializes same-process writers) blocks until the holder commits,
    /// or fails with `lock_not_available` (55P03) after the timeout, which
    /// classifies as retryable contention — the exact `busy_timeout`
    /// behavior the SQLite path gets from the engine.
    pub fn transaction_immediate(&self) -> Result<DbTx<'_>, DbError> {
        self.begin(true)
    }

    fn begin(&self, immediate: bool) -> Result<DbTx<'_>, DbError> {
        match self {
            Db::Sqlite(conn) => {
                conn.execute_batch(if immediate {
                    "BEGIN IMMEDIATE"
                } else {
                    "BEGIN"
                })?;
            }
            Db::Postgres(pg) => {
                if pg.in_tx.get() {
                    return Err(DbError::NestedTransaction);
                }
                // Postgres has no immediate/deferred BEGIN distinction;
                // write locks arrive at first write either way.
                pg.with_client(|c| c.batch_execute("BEGIN"))?;
                if immediate {
                    pg.with_client(|c| {
                        c.batch_execute(&format!(
                            "SET LOCAL lock_timeout = '{}ms'; \
                             SELECT pg_advisory_xact_lock({PG_WRITER_ADVISORY_KEY})",
                            PG_LOCK_TIMEOUT_MS
                        ))
                    })?;
                }
                pg.in_tx.set(true);
            }
        }
        Ok(DbTx {
            db: self,
            committed: false,
        })
    }

    /// The backend's `rowid`-equivalent, qualified by `qual` (the table
    /// alias): SQLite's implicit rowid, or the `seq` identity column the
    /// Postgres migrations add. Backs the derive queue's insertion-order
    /// pick and the backfill cursor.
    pub fn rowid(&self, qual: &str) -> String {
        match self {
            Db::Sqlite(_) => format!("{qual}.rowid"),
            Db::Postgres(_) => format!("{qual}.seq"),
        }
    }

    /// The raw SQLite connection, for callers that must speak SQLite
    /// directly (the pragmas in `Store::open`, the server's test hooks,
    /// `EXPLAIN QUERY PLAN` pins). `None` on Postgres — those callers are
    /// SQLite-only by nature and take their PG-analog paths.
    pub fn sqlite_raw(&self) -> Option<&Connection> {
        match self {
            Db::Sqlite(conn) => Some(conn),
            Db::Postgres(_) => None,
        }
    }

    pub fn dialect(&self) -> Dialect {
        match self {
            Db::Sqlite(_) => Dialect::Sqlite,
            Db::Postgres(_) => Dialect::Postgres,
        }
    }
}

/// Transaction guard over a [`Db`]: statements keep flowing through the
/// same handle (deref), `commit` finishes explicitly, and a drop without
/// commit rolls back — including through a panic unwind.
#[derive(Debug)]
pub struct DbTx<'a> {
    db: &'a Db,
    committed: bool,
}

impl std::ops::Deref for DbTx<'_> {
    type Target = Db;

    fn deref(&self) -> &Db {
        self.db
    }
}

impl DbTx<'_> {
    pub fn commit(mut self) -> Result<(), DbError> {
        match self.db {
            Db::Sqlite(conn) => conn.execute_batch("COMMIT")?,
            Db::Postgres(pg) => {
                pg.with_client(|c| c.batch_execute("COMMIT"))?;
                pg.in_tx.set(false);
            }
        }
        self.committed = true;
        Ok(())
    }
}

impl Drop for DbTx<'_> {
    fn drop(&mut self) {
        if self.committed {
            return;
        }
        match self.db {
            Db::Sqlite(conn) => {
                let _ = conn.execute_batch("ROLLBACK");
            }
            Db::Postgres(pg) => {
                let _ = pg.with_client(|c| c.batch_execute("ROLLBACK"));
                pg.in_tx.set(false);
            }
        }
    }
}

// ---- per-backend adapters ---------------------------------------------------

fn sqlite_params(params: &[P]) -> Vec<&dyn rusqlite::types::ToSql> {
    params
        .iter()
        .map(|p| p as &dyn rusqlite::types::ToSql)
        .collect()
}

impl rusqlite::types::ToSql for P {
    fn to_sql<'a>(&'a self) -> Result<rusqlite::types::ToSqlOutput<'a>, rusqlite::Error> {
        use rusqlite::types::{ToSqlOutput, ValueRef};
        Ok(match self {
            P::Null => ToSqlOutput::Borrowed(ValueRef::Null),
            P::I(i) => ToSqlOutput::Borrowed(ValueRef::Integer(*i)),
            P::S(s) => ToSqlOutput::Borrowed(ValueRef::Text(s.as_bytes())),
            P::B(b) => ToSqlOutput::Borrowed(ValueRef::Blob(b)),
        })
    }
}

fn sqlite_row(row: &rusqlite::Row<'_>) -> Result<Row, DbError> {
    let mut vals = Vec::with_capacity(row.as_ref().column_count());
    for i in 0..row.as_ref().column_count() {
        let val = match row.get_ref(i)? {
            rusqlite::types::ValueRef::Null => Val::Null,
            rusqlite::types::ValueRef::Integer(i) => Val::I(i),
            rusqlite::types::ValueRef::Real(_) => {
                return Err(DbError::ColumnDomain {
                    col: row.as_ref().column_name(i).unwrap_or("").to_string(),
                    backend: "sqlite",
                });
            }
            rusqlite::types::ValueRef::Text(t) => Val::S(
                std::str::from_utf8(t)
                    .map_err(|_| DbError::ColumnDomain {
                        col: row.as_ref().column_name(i).unwrap_or("").to_string(),
                        backend: "sqlite",
                    })?
                    .to_string(),
            ),
            rusqlite::types::ValueRef::Blob(b) => Val::B(b.to_vec()),
        };
        vals.push(val);
    }
    Ok(Row(vals))
}

fn pg_refs(params: &[P]) -> Vec<&(dyn postgres::types::ToSql + Sync)> {
    params
        .iter()
        .map(|p| p as &(dyn postgres::types::ToSql + Sync))
        .collect()
}

impl postgres::types::ToSql for P {
    fn to_sql(
        &self,
        ty: &postgres::types::Type,
        out: &mut postgres::types::private::BytesMut,
    ) -> Result<postgres::types::IsNull, Box<dyn std::error::Error + Sync + Send>> {
        match self {
            P::Null => Option::<i64>::None.to_sql(ty, out),
            P::I(i) => i.to_sql(ty, out),
            P::S(s) => s.to_sql(ty, out),
            P::B(b) => b.to_sql(ty, out),
        }
    }

    fn accepts(_ty: &postgres::types::Type) -> bool {
        true
    }

    postgres::types::to_sql_checked!();
}

fn pg_row(row: &postgres::Row) -> Result<Row, DbError> {
    use postgres::types::Type;
    let mut vals = Vec::with_capacity(row.len());
    for i in 0..row.len() {
        let col = row.columns()[i].name().to_string();
        let null = Val::Null;
        let val = match row.columns()[i].type_().clone() {
            Type::INT8 => opt(row.try_get::<_, Option<i64>>(i), &col)?
                .map(Val::I)
                .unwrap_or(null),
            Type::INT4 => opt(row.try_get::<_, Option<i32>>(i), &col)?
                .map(|v| Val::I(i64::from(v)))
                .unwrap_or(null),
            Type::INT2 => opt(row.try_get::<_, Option<i16>>(i), &col)?
                .map(|v| Val::I(i64::from(v)))
                .unwrap_or(null),
            Type::TEXT | Type::VARCHAR | Type::NAME => {
                opt(row.try_get::<_, Option<String>>(i), &col)?
                    .map(Val::S)
                    .unwrap_or(null)
            }
            Type::BYTEA => opt(row.try_get::<_, Option<Vec<u8>>>(i), &col)?
                .map(Val::B)
                .unwrap_or(null),
            Type::BOOL => opt(row.try_get::<_, Option<bool>>(i), &col)?
                .map(|b| Val::I(i64::from(b)))
                .unwrap_or(null),
            t => {
                let _ = t;
                return Err(DbError::ColumnDomain {
                    col,
                    backend: "postgres",
                });
            }
        };
        vals.push(val);
    }
    Ok(Row(vals))
}

/// A typed column read: a driver failure here means the column's declared
/// type disagreed with the read — the schema/domain mismatch arm.
fn opt<V, E>(r: Result<Option<V>, E>, col: &str) -> Result<Option<V>, DbError>
where
    E: std::fmt::Display,
{
    r.map_err(|_| DbError::ColumnDomain {
        col: col.to_string(),
        backend: "postgres",
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn placeholder_rewrite() {
        let s = Sql::plain("SELECT a FROM t WHERE b = ?1 AND c < ?2 OR d > ?10");
        assert_eq!(
            s.postgres_sql().unwrap(),
            "SELECT a FROM t WHERE b = $1 AND c < $2 OR d > $10"
        );
    }

    #[test]
    fn bare_question_mark_refused() {
        let s = Sql::plain("SELECT * FROM t WHERE a LIKE '%?%'");
        assert!(matches!(s.postgres_sql(), Err(DbError::BadSql(_))));
    }

    #[test]
    fn ignore_shape_becomes_on_conflict_nothing() {
        let s = Sql::ignore("INSERT OR IGNORE INTO repo_trees(repo_id, tree_id) VALUES (?1, ?2)");
        assert_eq!(
            s.postgres_sql().unwrap(),
            "INSERT INTO repo_trees(repo_id, tree_id) VALUES ($1, $2) ON CONFLICT DO NOTHING"
        );
    }

    #[test]
    fn replace_shape_becomes_on_conflict_update() {
        let s = Sql::replace(
            "INSERT OR REPLACE INTO tree_entries(tree_id, name, kind) VALUES (?1, ?2, ?3)",
            &["tree_id", "name"],
            &["kind"],
        );
        assert_eq!(
            s.postgres_sql().unwrap(),
            "INSERT INTO tree_entries(tree_id, name, kind) VALUES ($1, $2, $3) \
             ON CONFLICT (tree_id, name) DO UPDATE SET kind = EXCLUDED.kind"
        );
    }

    #[test]
    fn wrong_shape_prefix_refused() {
        let s = Sql::ignore("INSERT INTO t(a) VALUES (?1)");
        assert!(matches!(s.postgres_sql(), Err(DbError::BadSql(_))));
    }

    #[test]
    fn params_roundtrip_values() {
        let ps = pv![1i64, "txt", &b"bytes"[..], Option::<i64>::None, true];
        assert_eq!(
            ps,
            vec![
                P::I(1),
                P::S("txt".into()),
                P::B(b"bytes".to_vec()),
                P::Null,
                P::I(1)
            ]
        );
        let empty = pv![];
        assert!(empty.is_empty());
    }

    #[test]
    fn row_get_typing() {
        let row = Row(vec![
            Val::I(7),
            Val::Null,
            Val::S("x".into()),
            Val::B(vec![1u8]),
        ]);
        assert_eq!(row.get::<i64>(0).unwrap(), 7);
        assert_eq!(row.get::<Option<i64>>(1).unwrap(), None);
        assert_eq!(row.get::<Option<i64>>(0).unwrap(), Some(7));
        assert!(row.get::<bool>(0).unwrap());
        assert_eq!(row.get::<String>(2).unwrap(), "x");
        assert_eq!(row.get::<Vec<u8>>(3).unwrap(), vec![1u8]);
        assert!(matches!(
            row.get::<String>(0),
            Err(DbError::ColumnType { want: "String", .. })
        ));
        assert!(matches!(row.get::<i64>(9), Err(DbError::ColumnIndex(9, 4))));
    }

    // ---- in-memory SQLite exercises the full primitive set ------------------

    fn mem_db() -> Db {
        let conn = Connection::open_in_memory().unwrap();
        Db::Sqlite(conn)
    }

    #[test]
    fn primitives_roundtrip_on_sqlite() {
        let db = mem_db();
        db.execute_batch(
            "CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT NOT NULL, data BLOB, n INTEGER);
             CREATE TABLE u(a INTEGER PRIMARY KEY, b INTEGER);",
        )
        .unwrap();
        let id = db
            .insert_id(
                &Sql::plain("INSERT INTO t(name, data, n) VALUES (?1, ?2, ?3)"),
                &pv!["x", &b"raw"[..], Option::<i64>::None],
            )
            .unwrap();
        assert_eq!(id, 1);

        let row = db
            .query_opt(
                &Sql::plain("SELECT id, name, data, n FROM t WHERE id = ?1"),
                &pv![id],
            )
            .unwrap()
            .expect("row");
        assert_eq!(row.get::<i64>(0).unwrap(), 1);
        assert_eq!(row.get::<String>(1).unwrap(), "x");
        assert_eq!(row.get::<Vec<u8>>(2).unwrap(), b"raw");
        assert_eq!(row.get::<Option<i64>>(3).unwrap(), None);

        let n = db
            .execute(
                &Sql::plain("UPDATE t SET n = ?1 WHERE id = ?2"),
                &pv![5i64, id],
            )
            .unwrap();
        assert_eq!(n, 1);

        db.execute(
            &Sql::ignore("INSERT OR IGNORE INTO u(a, b) VALUES (?1, ?2)"),
            &pv![1i64, 2],
        )
        .unwrap();
        let n = db
            .execute(
                &Sql::ignore("INSERT OR IGNORE INTO u(a, b) VALUES (?1, ?2)"),
                &pv![1i64, 2],
            )
            .unwrap();
        assert_eq!(n, 0, "OR IGNORE is a no-op on repeat");

        let s = Sql::replace(
            "INSERT OR REPLACE INTO u(a, b) VALUES (?1, ?2)",
            &["a"],
            &["b"],
        );
        db.execute(&s, &pv![1i64, 3]).unwrap();
        let row = db
            .query_opt(&Sql::plain("SELECT b FROM u WHERE a = 1"), &pv![])
            .unwrap()
            .unwrap();
        assert_eq!(row.get::<i64>(0).unwrap(), 3);

        let all = db
            .query_all(&Sql::plain("SELECT id FROM t ORDER BY id"), &pv![])
            .unwrap();
        assert_eq!(all.len(), 1);
        assert!(
            db.query_opt(&Sql::plain("SELECT id FROM t WHERE id = 999"), &pv![])
                .unwrap()
                .is_none()
        );

        assert_eq!(db.rowid("t"), "t.rowid");
        assert_eq!(db.dialect(), Dialect::Sqlite);
        assert!(db.sqlite_raw().is_some());
    }

    #[test]
    fn transaction_commits_and_rolls_back() {
        let db = mem_db();
        db.execute_batch("CREATE TABLE t(a INTEGER)").unwrap();
        {
            let tx = db.transaction().unwrap();
            tx.execute(&Sql::plain("INSERT INTO t VALUES (1)"), &pv![])
                .unwrap();
            tx.commit().unwrap();
        }
        {
            let tx = db.transaction().unwrap();
            tx.execute(&Sql::plain("INSERT INTO t VALUES (2)"), &pv![])
                .unwrap();
            // dropped without commit → rollback
        }
        let n = db
            .query_opt(&Sql::plain("SELECT COUNT(*) FROM t"), &pv![])
            .unwrap()
            .unwrap();
        assert_eq!(n.get::<i64>(0).unwrap(), 1);

        // Nested begin is refused.
        let tx = db.transaction().unwrap();
        assert!(matches!(db.transaction(), Err(DbError::Sqlite(_))));
        tx.commit().unwrap();

        let tx = db.transaction_immediate().unwrap();
        tx.commit().unwrap();
    }
}
