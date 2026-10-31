//! The blob-CAS backend seam (M9): history blobs go through one trait, so
//! the filesystem CAS (the zero-config default) and the S3-compatible CAS
//! ([`crate::s3::S3Store`], M9b) are interchangeable behind it.
//!
//! The server holds an `Arc<dyn BlobStore>` and every durability argument
//! stays put — `put` fsyncs (or otherwise durably lands) the bytes before
//! returning, and callers write referencing rows only after `put`.
//!
//! NOTE: `Cas::put` returning the id derives it from the payload, so the
//! filesystem backend is content-addressed end to end. The trait keeps
//! that contract (`put` RETURNS the id); the S3 implementation uses the
//! same id derivation as its object key, preserving hash-verified reads on
//! every backend.

use naatre_core::ids::Id;

use crate::cas::Cas;
use crate::data_dir::DataDir;
use crate::error::StoreError;

/// The CONFIGURED history CAS: the fs [`Cas`] or the S3 backend
/// ([`crate::s3::S3Store`]), per `cfg.blob` — the one constructor every
/// blob writer goes through (the server state, the offline import).
/// Import and serve are separate processes and each own their instance;
/// the data-dir flock keeps them exclusive.
pub fn history_cas(
    data: &DataDir,
    cfg: &crate::backend::BackendConfig,
) -> Result<std::sync::Arc<dyn BlobStore>, StoreError> {
    match cfg.blob {
        crate::backend::BlobKind::Fs => Ok(std::sync::Arc::new(Cas::new(data)?)),
        crate::backend::BlobKind::S3 => {
            let s3 = cfg.s3.as_ref().ok_or(StoreError::Backend(
                crate::backend::BackendError::S3KeyMissing {
                    key: "bucket",
                    env: crate::backend::ENV_S3_BUCKET,
                },
            ))?;
            Ok(std::sync::Arc::new(crate::s3::S3Store::new(
                s3,
                cfg.allow_plaintext,
            )?))
        }
    }
}

/// Content-addressed store for HISTORY blobs.
pub trait BlobStore: Send + Sync {
    /// Stores `payload` and returns its blob id. Idempotent: a second put
    /// of identical content must be a no-op. Durable on return — callers
    /// may commit referencing rows afterwards (durability invariant 1).
    fn put(&self, payload: &[u8]) -> Result<Id, StoreError>;

    /// Fetches, decompresses, and hash-verifies the blob for `id`.
    /// [`StoreError::BlobMissing`] when absent; [`StoreError::HashMismatch`]
    /// when the stored bytes do not hash to `id`.
    fn get(&self, id: &Id) -> Result<Vec<u8>, StoreError>;

    /// Batch existence check, aligned with `ids`.
    fn has(&self, ids: &[Id]) -> Result<Vec<bool>, StoreError>;
}

impl BlobStore for Cas {
    fn put(&self, payload: &[u8]) -> Result<Id, StoreError> {
        Cas::put(self, payload)
    }

    fn get(&self, id: &Id) -> Result<Vec<u8>, StoreError> {
        Cas::get(self, id)
    }

    fn has(&self, ids: &[Id]) -> Result<Vec<bool>, StoreError> {
        Cas::has(self, ids)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::data_dir::DataDir;

    #[test]
    fn fs_cas_behind_the_trait() {
        let dir = tempfile::tempdir().unwrap();
        let dd = DataDir::create(dir.path()).unwrap();
        let cas: std::sync::Arc<dyn BlobStore> = std::sync::Arc::new(Cas::new(&dd).unwrap());
        let payload = b"through the seam";
        let id = cas.put(payload).unwrap();
        assert_eq!(cas.get(&id).unwrap(), payload);
        assert_eq!(cas.has(&[id]).unwrap(), vec![true]);
    }
}
