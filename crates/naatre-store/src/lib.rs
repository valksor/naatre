//! naatre store: SQLite metadata, content-addressed blob CAS, and ephemeral
//! snapshot storage.
//!
//! The store is synchronous by design: rusqlite is blocking, so the future
//! server crate reaches it through `spawn_blocking`. There is no async in
//! this crate.
//!
//! Layout of a data dir (all of it 0700 — it is the host-local trust
//! boundary):
//!
//! ```text
//! <data>/
//!   naatre.db                  SQLite (WAL, synchronous=FULL)
//!   objects/ab/cd/<id-hex>     history CAS (zstd blobs)
//!   ephemeral/<snap-id>/…      snapshot CAS (TTL'd, deleted wholesale)
//!   tmp/                       staging for tmp+rename writes
//!   lockfile                   flock held by serve and import
//! ```
//!
//! Durability invariant: every blob is fsynced into the CAS before any row
//! referencing it commits — callers must `Cas::put` before (or inside, but
//! before the commit of) the transaction that references the id. Failure
//! leaves an orphan object, never a dangling reference.

pub mod cas;
pub mod data_dir;
pub mod ephemeral;
pub mod error;
pub mod fs_util;
pub mod migrate;
pub mod store;

pub use cas::{BLOB_LIMIT, Cas};
pub use data_dir::{DataDir, LockGuard};
pub use ephemeral::Ephemeral;
pub use error::StoreError;
pub use migrate::{MAX_SUPPORTED, migrate};
pub use store::Store;
