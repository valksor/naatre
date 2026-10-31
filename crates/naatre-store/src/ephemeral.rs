//! Ephemeral snapshot storage: a per-snapshot CAS under
//! `ephemeral/<snap-id>/`, deleted wholesale at TTL expiry.
//!
//! This namespace never shares files with the history CAS (`objects/`) —
//! expiry deletes whole snapshot dirs, so no refcounting is possible or
//! needed.
//!
//! M9b: the same namespace exists on the S3 backend
//! (`ephemeral/<snap-id>/objects/…` keys in the configured bucket, deleted
//! by prefix), behind the [`EphemeralStore`] trait — the filesystem
//! [`Ephemeral`] and the S3 store ([`crate::s3::S3Store`]) are
//! interchangeable, so the snapshot GC's both gates work unchanged on
//! either backend.

use std::fs;
use std::path::PathBuf;
use std::time::UNIX_EPOCH;

use naatre_core::ids::Id;

use crate::data_dir::DataDir;
use crate::error::StoreError;
use crate::fs_util::{io_ctx, is_hex64, read_blob_verified, shard_path, write_compressed};

/// Snapshot-scoped blob storage (M9b trait): the per-snapshot CAS the
/// snapshot verbs and the snapshot GC drive, with the same contract on
/// every backend.
pub trait EphemeralStore: Send + Sync {
    /// Puts a blob into snapshot `snap_id_hex`'s namespace.
    fn put_blob(&self, snap_id_hex: &str, payload: &[u8]) -> Result<Id, StoreError>;

    /// Fetches and hash-verifies a blob from snapshot `snap_id_hex`.
    fn get_blob(&self, snap_id_hex: &str, id: &Id) -> Result<Vec<u8>, StoreError>;

    /// Batch existence check within snapshot `snap_id_hex`.
    fn has(&self, snap_id_hex: &str, ids: &[Id]) -> Result<Vec<bool>, StoreError>;

    /// Deletes the whole snapshot namespace. Idempotent: an already-absent
    /// namespace is `Ok`, so GC re-runs converge. Refuses non-hex ids.
    fn delete_snapshot(&self, snap_id_hex: &str) -> Result<(), StoreError>;

    /// Lists snapshot namespaces aged past `older_than_ns` (an absolute
    /// epoch-ns threshold; the sweeper passes `now - max_ttl`). Age is the
    /// dir mtime on fs, the newest object's LastModified on S3. Names are
    /// never used to build paths here; the GC cross-checks them against
    /// `snapshots` rows and filters non-hex ones.
    fn rowless_dirs(&self, older_than_ns: i64, now_ns: i64) -> Result<Vec<String>, StoreError>;
}

/// Snapshot-scoped blob store (the filesystem backend).
pub struct Ephemeral {
    root: PathBuf, // <data>/ephemeral
}

impl Ephemeral {
    /// Builds the ephemeral handle for `data`, creating `ephemeral/` (0700)
    /// if needed.
    pub fn new(data: &DataDir) -> Result<Ephemeral, StoreError> {
        let root = data.ephemeral_dir()?;
        Ok(Ephemeral { root })
    }

    /// `ephemeral/<snap>/objects` after validating the snapshot id. Every
    /// path below this module is built only from a validated id.
    fn snap_objects(&self, snap_id: &str) -> Result<PathBuf, StoreError> {
        if !is_hex64(snap_id) {
            return Err(StoreError::InvalidSnapshotId(snap_id.to_string()));
        }
        Ok(self.root.join(snap_id).join("objects"))
    }

    /// Puts a blob into snapshot `snap_id_hex`'s namespace. Same mechanics
    /// as the history CAS (zstd, tmp+rename+fsync, hash-verified reads),
    /// but under `ephemeral/<snap>/objects/` with per-snapshot staging in
    /// `ephemeral/<snap>/tmp/` so a wholesale delete takes the leftovers too.
    pub fn put_blob(&self, snap_id_hex: &str, payload: &[u8]) -> Result<Id, StoreError> {
        let objects = self.snap_objects(snap_id_hex)?;
        let tmp = objects
            .parent()
            .expect("objects has ephemeral/<snap> parent")
            .join("tmp");
        write_compressed(&objects, &tmp, payload, crate::cas::BLOB_LIMIT)
    }

    /// Fetches and hash-verifies a blob from snapshot `snap_id_hex`.
    pub fn get_blob(&self, snap_id_hex: &str, id: &Id) -> Result<Vec<u8>, StoreError> {
        let objects = self.snap_objects(snap_id_hex)?;
        read_blob_verified(&objects, id, crate::cas::BLOB_LIMIT)
    }

    /// Batch existence check within snapshot `snap_id_hex`.
    pub fn has(&self, snap_id_hex: &str, ids: &[Id]) -> Result<Vec<bool>, StoreError> {
        let objects = self.snap_objects(snap_id_hex)?;
        Ok(ids
            .iter()
            .map(|id| shard_path(&objects, id).exists())
            .collect())
    }

    /// Deletes the whole snapshot dir `ephemeral/<snap>` (including any
    /// staging leftovers). Idempotent: an already-absent dir is `Ok`, so
    /// GC re-runs converge. Refuses non-hex snapshot ids.
    pub fn delete_snapshot(&self, snap_id_hex: &str) -> Result<(), StoreError> {
        if !is_hex64(snap_id_hex) {
            return Err(StoreError::InvalidSnapshotId(snap_id_hex.to_string()));
        }
        let dir = self.root.join(snap_id_hex);
        match fs::remove_dir_all(&dir) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(e) => return Err(io_ctx(e, &dir)),
        }
        // Best-effort fsync of the parent; platforms that refuse to open
        // directories make this a no-op.
        if let Ok(d) = fs::File::open(&self.root) {
            let _ = d.sync_all();
        }
        Ok(())
    }

    /// Lists snapshot dir names whose mtime is older than `older_than_ns`
    /// (an absolute epoch-ns threshold: the sweeper passes `now - max_ttl`).
    ///
    /// The sweeper (M5) cross-checks the returned names against `snapshots`
    /// rows and deletes the rowless ones. `now_ns` is the caller's clock —
    /// kept in the signature so the sweeper passes its (injectable, in
    /// tests) time here for future use; the listing rule itself is purely
    /// `mtime < older_than_ns`. Names are never used to build paths inside
    /// this method.
    pub fn rowless_dirs(&self, older_than_ns: i64, now_ns: i64) -> Result<Vec<String>, StoreError> {
        let _ = now_ns;
        let mut out = Vec::new();
        for entry in fs::read_dir(&self.root)? {
            let entry = entry?;
            let name = entry.file_name();
            let Some(name) = name.to_str() else {
                continue; // not UTF-8: cannot be a snapshot id
            };
            if !entry.file_type()?.is_dir() {
                continue;
            }
            let mtime_ns = entry
                .metadata()?
                .modified()?
                .duration_since(UNIX_EPOCH)
                .map(|d| i64::try_from(d.as_nanos()).unwrap_or(i64::MAX))
                .unwrap_or(0); // pre-epoch clock: treat as ancient
            if mtime_ns < older_than_ns {
                out.push(name.to_string());
            }
        }
        out.sort();
        Ok(out)
    }
}

impl EphemeralStore for Ephemeral {
    fn put_blob(&self, snap_id_hex: &str, payload: &[u8]) -> Result<Id, StoreError> {
        Ephemeral::put_blob(self, snap_id_hex, payload)
    }

    fn get_blob(&self, snap_id_hex: &str, id: &Id) -> Result<Vec<u8>, StoreError> {
        Ephemeral::get_blob(self, snap_id_hex, id)
    }

    fn has(&self, snap_id_hex: &str, ids: &[Id]) -> Result<Vec<bool>, StoreError> {
        Ephemeral::has(self, snap_id_hex, ids)
    }

    fn delete_snapshot(&self, snap_id_hex: &str) -> Result<(), StoreError> {
        Ephemeral::delete_snapshot(self, snap_id_hex)
    }

    fn rowless_dirs(&self, older_than_ns: i64, now_ns: i64) -> Result<Vec<String>, StoreError> {
        Ephemeral::rowless_dirs(self, older_than_ns, now_ns)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ephemeral() -> (tempfile::TempDir, Ephemeral) {
        let dir = tempfile::tempdir().unwrap();
        let dd = DataDir::create(dir.path()).unwrap();
        let e = Ephemeral::new(&dd).unwrap();
        (dir, e)
    }

    fn snap_hex(seed: u8) -> String {
        format!("{:062x}{:02x}", 0u128, seed) // 64 lowercase hex chars
    }

    #[test]
    fn roundtrip_under_hex_id() {
        let (_d, e) = ephemeral();
        let snap = snap_hex(1);
        let payload = b"snapshot bytes";
        let id = e.put_blob(&snap, payload).unwrap();
        assert_eq!(e.get_blob(&snap, &id).unwrap(), payload);
        assert_eq!(e.has(&snap, &[id]).unwrap(), vec![true]);

        // Snapshots are isolated from each other and from the history CAS.
        let other = snap_hex(2);
        assert_eq!(e.has(&other, &[id]).unwrap(), vec![false]);
        match e.get_blob(&other, &id) {
            Err(StoreError::BlobMissing(_)) => {}
            other => panic!("expected BlobMissing in other snapshot, got {other:?}"),
        }
    }

    #[test]
    fn non_hex_snap_ids_rejected_for_all_ops() {
        let (_d, e) = ephemeral();
        let long_a = "a".repeat(63); // wrong length
        let long_g = "g".repeat(64); // right length, not hex
        for bad in [
            "",
            "abc",
            "../escape",
            "ABCDEF0123",
            long_a.as_str(),
            long_g.as_str(),
        ] {
            assert!(
                matches!(e.put_blob(bad, b"x"), Err(StoreError::InvalidSnapshotId(_))),
                "put must reject {bad:?}"
            );
            assert!(
                matches!(
                    e.get_blob(bad, &Id([0u8; 32])),
                    Err(StoreError::InvalidSnapshotId(_))
                ),
                "get must reject {bad:?}"
            );
            assert!(
                matches!(e.has(bad, &[]), Err(StoreError::InvalidSnapshotId(_))),
                "has must reject {bad:?}"
            );
            assert!(
                matches!(
                    e.delete_snapshot(bad),
                    Err(StoreError::InvalidSnapshotId(_))
                ),
                "delete must reject {bad:?}"
            );
        }
        // No dir was ever created for a rejected id.
        let created: Vec<_> = fs::read_dir(&e.root)
            .unwrap()
            .map(|entry| entry.unwrap().file_name())
            .collect();
        assert!(
            created.is_empty(),
            "rejected ids must not create dirs: {created:?}"
        );
    }

    #[test]
    fn delete_removes_whole_dir_and_is_idempotent() {
        let (_d, e) = ephemeral();
        let snap = snap_hex(3);
        let id = e.put_blob(&snap, b"data").unwrap();
        assert!(e.has(&snap, &[id]).unwrap() == vec![true]);
        e.delete_snapshot(&snap).unwrap();
        assert_eq!(e.has(&snap, &[id]).unwrap(), vec![false]);
        assert!(!e.root.join(&snap).exists());
        // Second delete is Ok (converges).
        e.delete_snapshot(&snap).unwrap();
    }

    #[test]
    fn rowless_dirs_lists_aged_dirs() {
        let (_d, e) = ephemeral();
        let snap = snap_hex(4);
        e.put_blob(&snap, b"payload").unwrap();
        let now = std::time::SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos() as i64;

        // Fresh dir is not older than an absolute threshold in the past
        // (the sweeper's real call shape: now - max_ttl would be below mtime).
        let past_threshold = now - 60 * 1_000_000_000;
        assert!(e.rowless_dirs(past_threshold, now).unwrap().is_empty());

        // The brief's pinned test shape: a future threshold (now + large)
        // makes even the fresh dir count as "older".
        let old = e.rowless_dirs(now + 10_000_000_000, now).unwrap();
        assert_eq!(old, vec![snap]);
    }
}
