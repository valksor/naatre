//! The blob-CAS backend seam (M9): history blobs go through one trait, so
//! the filesystem CAS (the zero-config default) and the S3-compatible CAS
//! (M9b) are interchangeable behind it.
//!
//! The M9a slice ships the trait with exactly one real implementation —
//! the filesystem [`Cas`]; `backend.toml`'s `blob = "s3"` refuses with a
//! message naming M9b. The seam is what makes the S3 backend a drop-in:
//! the server holds a `Arc<dyn BlobStore>` and every durability argument
//! stays put — `put` fsyncs (or otherwise durably lands) the bytes before
//! returning, and callers write referencing rows only after `put`.
//!
//! NOTE (M9a): `Cas::put` returning the id derives it from the payload, so
//! the filesystem backend is content-addressed end to end. The trait
//! keeps that contract (`put` RETURNS the id); the M9b S3 implementation
//! uses the same id derivation as its object key, preserving hash-verified
//! reads on every backend.

use naatre_core::ids::Id;

use crate::cas::Cas;
use crate::error::StoreError;

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
