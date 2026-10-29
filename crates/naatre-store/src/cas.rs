//! The history CAS: zstd-compressed, content-addressed blobs under
//! `objects/<2hex>/<2hex>/<62hex>`.

use std::path::PathBuf;

use naatre_core::ids::Id;

use crate::data_dir::DataDir;
use crate::error::StoreError;
use crate::fs_util::{read_blob_verified, shard_path, write_compressed};

/// Maximum uncompressed payload size: 64 MiB, enforced both on put (input
/// size) and get (expanded size).
pub const BLOB_LIMIT: usize = 64 * 1024 * 1024;

/// Content-addressed store for history blobs.
pub struct Cas {
    objects_root: PathBuf,
    tmp_root: PathBuf,
}

impl Cas {
    /// Builds the CAS handles for `data`, creating `objects/` and `tmp/`
    /// (0700) if needed.
    pub fn new(data: &DataDir) -> Result<Cas, StoreError> {
        Ok(Cas {
            objects_root: data.objects_dir()?,
            tmp_root: data.tmp_dir()?,
        })
    }

    /// Stores `payload` and returns its blob id. Idempotent: a second put
    /// of identical content returns the same id without touching the file.
    ///
    /// Durability invariant: every blob is fsynced into the CAS *before*
    /// any row referencing it commits — callers must call `put` before (or
    /// inside, but before the commit of) the transaction that references
    /// the id. A crash then leaves at most an unreferenced orphan object,
    /// never a row pointing at missing bytes. This also makes db-then-
    /// objects the safe backup ordering.
    ///
    /// `payload` must be at most [`BLOB_LIMIT`] uncompressed bytes.
    pub fn put(&self, payload: &[u8]) -> Result<Id, StoreError> {
        write_compressed(&self.objects_root, &self.tmp_root, payload, BLOB_LIMIT)
    }

    /// Fetches and decompresses the blob for `id`, verifying its hash.
    ///
    /// Returns [`StoreError::BlobMissing`] when the object file does not
    /// exist and [`StoreError::HashMismatch`] when the stored content does
    /// not hash to `id` (the file is left in place for diagnosis).
    /// Decompression is capped at [`BLOB_LIMIT`] expanded bytes, so a small
    /// crafted compressed input cannot balloon memory.
    pub fn get(&self, id: &Id) -> Result<Vec<u8>, StoreError> {
        read_blob_verified(&self.objects_root, id, BLOB_LIMIT)
    }

    /// Batch existence check, aligned with `ids`.
    pub fn has(&self, ids: &[Id]) -> Result<Vec<bool>, StoreError> {
        Ok(ids
            .iter()
            .map(|id| shard_path(&self.objects_root, id).exists())
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write as _;

    fn cas() -> (tempfile::TempDir, Cas) {
        let dir = tempfile::tempdir().unwrap();
        let dd = DataDir::create(dir.path()).unwrap();
        let cas = Cas::new(&dd).unwrap();
        (dir, cas)
    }

    /// Small LCG-based byte generator: deterministic, cheap, and incompressible
    /// enough to exercise real compression work without pulling in a rand dep.
    fn pseudo_random(len: usize) -> Vec<u8> {
        let mut x = 0x243f_6a88_85a3_08d3u64;
        (0..len)
            .map(|_| {
                x = x
                    .wrapping_mul(6_364_136_223_846_793_005)
                    .wrapping_add(1_442_695_040_888_963_407);
                (x >> 33) as u8
            })
            .collect()
    }

    #[test]
    fn roundtrip_empty_small_and_1mib() {
        let (_d, cas) = cas();
        for payload in [&b""[..], b"hello naatre", &pseudo_random(1024 * 1024)[..]] {
            let id = cas.put(payload).unwrap();
            assert_eq!(cas.get(&id).unwrap(), payload);
        }
    }

    #[test]
    fn put_is_idempotent_and_untouched() {
        let (_d, cas) = cas();
        let payload = b"same bytes";
        let id = cas.put(payload).unwrap();
        let path = shard_path(&cas.objects_root, &id);
        let before = std::fs::read(&path).unwrap();
        let id2 = cas.put(payload).unwrap();
        assert_eq!(id, id2);
        assert_eq!(std::fs::read(&path).unwrap(), before);
    }

    #[test]
    fn too_large_rejected() {
        let (_d, cas) = cas();
        let payload = vec![0u8; BLOB_LIMIT + 1];
        match cas.put(&payload) {
            Err(StoreError::TooLarge { limit, got }) => {
                assert_eq!(limit, BLOB_LIMIT);
                assert_eq!(got, BLOB_LIMIT + 1);
            }
            other => panic!("expected TooLarge, got {other:?}"),
        }
        // Exactly at the limit is fine — but keep the test fast by using a
        // small compressible payload at the boundary only in structure.
        // (A real 64 MiB put is exercised by the cap-boundary server tests
        // in M1b.)
    }

    #[test]
    fn missing_blob_errors() {
        let (_d, cas) = cas();
        let id = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, b"never put");
        match cas.get(&id) {
            Err(StoreError::BlobMissing(hex)) => assert_eq!(hex, id.to_hex()),
            other => panic!("expected BlobMissing, got {other:?}"),
        }
    }

    #[test]
    fn file_for_a_requested_as_b_is_hash_mismatch() {
        let (_d, cas) = cas();
        let a_payload = b"contents of A";
        let id_a = cas.put(a_payload).unwrap();
        // Construct B: a different id whose file is a copy of A's file.
        let id_b = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, b"not what A holds");
        assert_ne!(id_a, id_b);
        let path_b = shard_path(&cas.objects_root, &id_b);
        std::fs::create_dir_all(path_b.parent().unwrap()).unwrap();
        std::fs::copy(shard_path(&cas.objects_root, &id_a), &path_b).unwrap();

        match cas.get(&id_b) {
            Err(StoreError::HashMismatch { expected, actual }) => {
                assert_eq!(expected, id_b.to_hex());
                assert_eq!(actual, id_a.to_hex());
                // Nothing is deleted: both files remain for diagnosis.
                assert!(path_b.exists());
            }
            other => panic!("expected HashMismatch, got {other:?}"),
        }
    }

    #[test]
    fn zstd_bomb_is_too_large_not_a_huge_allocation() {
        let (_d, cas) = cas();
        // Compress ~100 MiB of zeros via streaming so the test itself never
        // holds the expanded bytes in memory; the compressed file is tiny.
        let bomb = cas.tmp_root.join("bomb.zst");
        let f = std::fs::File::create(&bomb).unwrap();
        let mut enc = zstd::stream::write::Encoder::new(f, 3).unwrap();
        let chunk = vec![0u8; 1024 * 1024];
        for _ in 0..100 {
            enc.write_all(&chunk).unwrap();
        }
        enc.finish().unwrap();

        // Place it where get() will find it, then fetch with a bounded loop.
        let fake_id = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, b"bomb id");
        let path = shard_path(&cas.objects_root, &fake_id);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::rename(&bomb, &path).unwrap();

        match cas.get(&fake_id) {
            Err(StoreError::TooLarge { limit, got }) => {
                assert_eq!(limit, BLOB_LIMIT);
                assert!(got > BLOB_LIMIT, "expanded size should have passed the cap");
                assert!(
                    got <= BLOB_LIMIT + 64 * 1024,
                    "cap must stop within one chunk"
                );
            }
            other => panic!("expected TooLarge, got {other:?}"),
        }
    }

    #[test]
    fn has_batch_mixed() {
        let (_d, cas) = cas();
        let id_yes = cas.put(b"present").unwrap();
        let id_no = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, b"absent");
        let res = cas.has(&[id_yes, id_no, id_yes]).unwrap();
        assert_eq!(res, vec![true, false, true]);
        assert!(cas.has(&[]).unwrap().is_empty());
    }

    #[test]
    fn no_tmp_leftover_after_puts() {
        let (_d, cas) = cas();
        cas.put(b"x").unwrap();
        cas.put(b"y").unwrap();
        let leftovers: Vec<_> = std::fs::read_dir(&cas.tmp_root).unwrap().collect();
        assert!(
            leftovers.is_empty(),
            "tmp must be clean after successful puts"
        );
    }
}
