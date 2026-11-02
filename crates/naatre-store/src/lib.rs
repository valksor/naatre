//! naatre store: pluggable metadata (SQLite default, Postgres 16+ from M9)
//! behind one statement surface, a content-addressed history CAS behind
//! the [`BlobStore`] seam (filesystem default, S3-compatible since M9b),
//! and the ephemeral snapshot storage behind [`EphemeralStore`].
//!
//! The store is synchronous by design: both metadata clients are blocking,
//! so the future server crate reaches them through `spawn_blocking`. There
//! is no async in this crate.
//!
//! Layout of a data dir (all of it 0700 — it is the host-local trust
//! boundary):
//!
//! ```text
//! <data>/
//!   backend.toml               backend selection + credentials (0600;
//!                              absent = sqlite/fs zero-config default)
//!   naatre.db                  SQLite (WAL, synchronous=FULL) — meta=sqlite
//!   objects/ab/cd/<id-hex>     history CAS (zstd blobs) — blob=fs
//!   ephemeral/<snap-id>/…      snapshot CAS (TTL'd, deleted wholesale)
//!   tmp/                       staging for tmp+rename writes
//!   lockfile                   flock held by serve and import
//! ```
//!
//! With `meta = postgres` there is no `naatre.db`: the metadata lives in
//! the configured Postgres 16+ database (same schema via the parallel
//! migration set), while the CAS, ephemeral namespace, and lockfile stay
//! in the data dir. With `blob = s3` (M9b) the history CAS and the
//! ephemeral namespace move into the configured bucket —
//! `objects/ab/cd/<hex>` and `ephemeral/<snap>/objects/…` keys of the same
//! shape as the fs layout (see [`s3`]); a data dir holding fs objects is
//! never switched in place, and see the README erasure enumeration for
//! what a bucket outliving the data dir means for deletion.
//!
//! Durability invariant: every blob is durably in the CAS before any row
//! referencing it commits — callers must `put` before (or inside, but
//! before the commit of) the transaction that references the id. Failure
//! leaves an orphan object, never a dangling reference.

pub mod backend;
pub mod blob;
pub mod cas;
pub mod catalog;
pub mod data_dir;
pub mod ephemeral;
pub mod error;
pub mod fs_util;
pub mod layout;
pub mod meta;
pub mod migrate;
pub mod s3;
pub mod store;

pub use backend::{BackendConfig, BlobKind, MetaKind, S3Connect};
pub use blob::BlobStore;
pub use cas::{BLOB_LIMIT, Cas};
pub use catalog::CATALOG_MAX_SUPPORTED;
pub use data_dir::{DataDir, LockGuard};
pub use ephemeral::{Ephemeral, EphemeralStore};
pub use error::StoreError;
pub use layout::Layout;
pub use meta::{Db, DbError, DbTx, Dialect, IntoP, P, Row, Sql, Val};
pub use migrate::{MAX_SUPPORTED, migrate, migrate_postgres};
pub use s3::S3Store;
pub use store::{Store, open_repo_db};
