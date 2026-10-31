//! The store's error type. Every display message names the relevant paths
//! and values so a failing operation is diagnosable from logs alone.

use std::path::PathBuf;

use crate::backend::BackendError;
use crate::meta::DbError;

/// Errors produced by the data dir, migrations, CAS, and snapshot storage.
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),

    #[error("sqlite error: {0}")]
    Sqlite(#[from] rusqlite::Error),

    #[error("{0}")]
    Db(#[from] DbError),

    #[error("{0}")]
    Backend(#[from] BackendError),

    #[error("zstd error: {0}")]
    Zstd(String),

    #[error(
        "hash mismatch: object file for id {expected} decompressed to content hashing to {actual}"
    )]
    HashMismatch { expected: String, actual: String },

    #[error("payload too large: limit is {limit} bytes, got {got} bytes")]
    TooLarge { limit: usize, got: usize },

    #[error("blob not found in CAS: {0}")]
    BlobMissing(String),

    #[error(
        "data dir schema version {found} is newer than supported version {supported}: \
         the binary is older than the data dir; upgrade the binary before using this data dir"
    )]
    SchemaFuture { found: i64, supported: i64 },

    #[error("data dir {0:?} is not initialized (naatre.db missing); run naatre init")]
    DataDirUninitialized(PathBuf),

    #[error("data dir {0:?} is already initialized (naatre.db exists); refusing to re-initialize")]
    DataDirAlreadyInitialized(PathBuf),

    #[error("data dir lock is already held by another process: {0:?}")]
    LockHeld(PathBuf),

    #[error("invalid snapshot id {0:?}: must be 64 lowercase hex characters")]
    InvalidSnapshotId(String),
}
