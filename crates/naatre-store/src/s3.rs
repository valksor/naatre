//! The S3-compatible blob backend (M9b): history blobs and the ephemeral
//! snapshot namespace live in one configured bucket, behind the same
//! [`crate::blob::BlobStore`] and [`crate::ephemeral::EphemeralStore`]
//! seams as the filesystem CAS.
//!
//! # Bucket layout (chosen: mirror the fs CAS)
//!
//! ```text
//! objects/ab/cd/<62hex>                  history blob (zstd, as on fs)
//! ephemeral/<snap-id>/objects/ab/cd/<62hex>  snapshot blob (TTL'd)
//! ```
//!
//! The object_store S3 "virtual prefix" convention is not used — the
//! naatre layout is identical on every backend, which makes fsck/repo-stats
//! presence scans (M10) uniform and a backend migration a straight,
//! byte-for-byte copy: the zstd compression and id derivation are shared
//! with the fs CAS, so `objects/ab/cd/<hex>` on disk corresponds 1:1 to the
//! S3 key of the same shape. An existing fs-CAS data dir is NOT
//! auto-migrated: switching `blob` in a data dir holding objects is
//! refused by [`crate::backend::check_data_dir_backend`]; a deployment
//! either starts fresh or copies `objects/` into the bucket out-of-band.
//!
//! # Snapshot namespace + GC
//!
//! Snapshot blobs sit under `ephemeral/<snap-id>/objects/…` in the same
//! bucket. There is no staging namespace: an S3 PUT is atomic, so a failed
//! upload never leaves partial bytes at a key (the fs backend stages in
//! `tmp/` because a partial FILE is possible there). GC semantics match
//! the fs backend: `delete_snapshot` empties the whole
//! `ephemeral/<snap>/` prefix (idempotent, converging by re-listing), and
//! `rowless_dirs` lists the `ephemeral/` first-level prefixes whose newest
//! object is older than the threshold (age = the newest object's
//! LastModified, the closest S3 analog of the fs dir mtime).
//!
//! # Sync facade
//!
//! The store crate is synchronous (`BlobStore` has no async); object_store
//! is async. Each `S3Store` owns a small current-thread tokio runtime and
//! drives every operation through it under a mutex — S3 operations are
//! therefore SERIALIZED per store instance. That is a deliberate M9b
//! tradeoff (correctness over concurrency; the fs CAS has no such limit):
//! the durability contract is unaffected, and lifting it later means
//! handing futures to a dedicated runtime thread, not changing the seam.
//!
//! # Redaction
//!
//! object_store errors carry the full request URL (endpoint included) and
//! response excerpts, so they are never surfaced: a failed operation
//! reduces to [`StoreError::S3`] naming only the operation. The same rule
//! as the Postgres driver redaction.

use std::sync::Mutex;

use futures::{StreamExt, stream};
use naatre_core::ids::{Id, Kind, object_id};
use object_store::aws::AmazonS3Builder;
use object_store::path::Path;
use object_store::{ObjectStore, ObjectStoreExt, PutPayload};

use crate::backend::S3Connect;
use crate::cas::BLOB_LIMIT;
use crate::error::StoreError;
use crate::fs_util::{compress_payload, decompress_verify, is_hex64};

/// The S3-compatible CAS + ephemeral store (one bucket, one client).
pub struct S3Store {
    store: object_store::aws::AmazonS3,
    /// object_store futures are driven here, serialized (see module docs).
    rt: Mutex<tokio::runtime::Runtime>,
}

impl S3Store {
    /// Builds the S3 backend from the parsed [`S3Connect`]. The scheme is
    /// derived from the TLS posture, never stored: HTTPS unless
    /// `allow_plaintext` (plain HTTP for LAN minio).
    pub fn new(cfg: &S3Connect, allow_plaintext: bool) -> Result<S3Store, StoreError> {
        let mut builder = AmazonS3Builder::new()
            .with_bucket_name(&cfg.bucket)
            .with_access_key_id(&cfg.access_key_id)
            .with_secret_access_key(&cfg.secret)
            .with_virtual_hosted_style_request(!cfg.path_style)
            .with_allow_http(allow_plaintext);
        if let Some(region) = &cfg.region {
            builder = builder.with_region(region);
        }
        if let Some(endpoint) = &cfg.endpoint {
            let scheme = if allow_plaintext { "http" } else { "https" };
            builder = builder.with_endpoint(format!("{scheme}://{endpoint}"));
        } else {
            // AWS default endpoints are TLS-only; the plaintext opt-in has
            // nothing to relax there. The region still matters for signing
            // and addressing.
            builder = builder.with_region(cfg.region.as_deref().unwrap_or("us-east-1"));
        }
        let store = builder
            .build()
            .map_err(|_| StoreError::S3 { op: "build" })?;
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .map_err(|_| StoreError::S3 { op: "runtime" })?;
        Ok(S3Store {
            store,
            rt: Mutex::new(rt),
        })
    }

    /// The raw object client — the maintenance/inspection hatch (M10's
    /// fsck presence scan, out-of-band migrations, the conformance tests'
    /// tamper legs). NOT part of the [`crate::blob::BlobStore`] seam.
    pub fn raw(&self) -> &dyn ObjectStore {
        &self.store
    }

    /// Drives one object_store future on this store's runtime (the mutex
    /// is the serialization point, see module docs).
    fn block_now<T>(
        &self,
        fut: impl std::future::Future<Output = Result<T, object_store::Error>>,
    ) -> Result<T, object_store::Error> {
        let rt = self.rt.lock().map_err(|_| object_store::Error::Generic {
            store: "naatre",
            source: "S3 runtime lock poisoned".into(),
        })?;
        rt.block_on(fut)
    }

    /// Drives one object_store future on this store's runtime (the mutex
    /// is the serialization point, see module docs) and lets `reduce`
    /// interpret its result — including mapping a recognized error (e.g.
    /// not-found → `BlobMissing`, or a `false` from `has`). Errors left
    /// unmapped are REDACTED to an op-named [`StoreError::S3`]:
    /// object_store errors can embed the endpoint.
    fn op_map<T, R>(
        &self,
        fut: impl std::future::Future<Output = Result<T, object_store::Error>>,
        reduce: impl FnOnce(Result<T, object_store::Error>) -> Result<R, StoreError>,
    ) -> Result<R, StoreError> {
        let rt = self
            .rt
            .lock()
            .map_err(|_| StoreError::S3 { op: "runtime" })?;
        reduce(rt.block_on(fut))
    }

    /// [`S3Store::op_map`] with the default reduction: pass the value
    /// through, redact every error to an op-named [`StoreError::S3`].
    fn op<T>(
        &self,
        op: &'static str,
        fut: impl std::future::Future<Output = Result<T, object_store::Error>>,
    ) -> Result<T, StoreError> {
        match self.block_now(fut) {
            Ok(v) => Ok(v),
            Err(_) => Err(StoreError::S3 { op }),
        }
    }

    /// `objects/<2hex>/<2hex>/<62hex>` — the fs CAS layout, byte-for-byte
    /// (see the module docs' migration note).
    fn objects_key(id: &Id) -> Path {
        Path::from(format!("objects/{}", shard_suffix(id)))
    }

    /// `ephemeral/<snap>/objects/<2hex>/<2hex>/<62hex>` after validating
    /// the snapshot id — every key below is built only from validated ids.
    fn ephemeral_key(snap_id_hex: &str, id: &Id) -> Result<Path, StoreError> {
        if !is_hex64(snap_id_hex) {
            return Err(StoreError::InvalidSnapshotId(snap_id_hex.to_string()));
        }
        Ok(Path::from(format!(
            "ephemeral/{snap_id_hex}/objects/{}",
            shard_suffix(id)
        )))
    }

    fn snap_prefix(snap_id_hex: &str) -> Result<Path, StoreError> {
        if !is_hex64(snap_id_hex) {
            return Err(StoreError::InvalidSnapshotId(snap_id_hex.to_string()));
        }
        Ok(Path::from(format!("ephemeral/{snap_id_hex}")))
    }
}

/// `<2hex>/<2hex>/<62hex>` — the shared shard shape of both namespaces.
fn shard_suffix(id: &Id) -> String {
    let hex = id.to_hex();
    format!("{}/{}/{}", &hex[0..2], &hex[2..4], &hex[4..])
}

/// The not-found probe every backend maps to `BlobMissing`.
fn is_not_found(e: &object_store::Error) -> bool {
    matches!(e, object_store::Error::NotFound { .. })
}

impl crate::blob::BlobStore for S3Store {
    fn put(&self, payload: &[u8]) -> Result<Id, StoreError> {
        if payload.len() > BLOB_LIMIT {
            return Err(StoreError::TooLarge {
                limit: BLOB_LIMIT,
                got: payload.len(),
            });
        }
        let id = object_id(Kind::Blob, payload);
        let key = S3Store::objects_key(&id);
        // Same content encoding as the fs CAS (zstd level 3) — the bucket
        // keys are byte-for-byte copies of the fs object files. And no
        // existence probe: an S3 PUT of identical bytes to the same
        // content-addressed key IS the idempotent no-op the contract
        // demands (the fs backend's short-circuit is only a disk-space
        // optimization, not a semantics difference).
        let compressed = compress_payload(payload)?;
        self.op("put", self.store.put(&key, PutPayload::from(compressed)))?;
        Ok(id)
    }

    fn get(&self, id: &Id) -> Result<Vec<u8>, StoreError> {
        let key = S3Store::objects_key(id);
        let compressed = self.op_map(
            async {
                let payload = self.store.get(&key).await?;
                payload.bytes().await
            },
            |r| match r {
                Ok(b) => Ok(b),
                Err(e) if is_not_found(&e) => Err(StoreError::BlobMissing(id.to_hex())),
                Err(_) => Err(StoreError::S3 { op: "get" }),
            },
        )?;
        decompress_verify(&compressed, id, BLOB_LIMIT)
    }

    fn has(&self, ids: &[Id]) -> Result<Vec<bool>, StoreError> {
        let mut out = Vec::with_capacity(ids.len());
        for id in ids {
            let key = S3Store::objects_key(id);
            out.push(self.op_map(self.store.head(&key), |r| match r {
                Ok(_) => Ok(true),
                Err(e) if is_not_found(&e) => Ok(false),
                Err(_) => Err(StoreError::S3 { op: "head" }),
            })?);
        }
        Ok(out)
    }
}

impl crate::ephemeral::EphemeralStore for S3Store {
    fn put_blob(&self, snap_id_hex: &str, payload: &[u8]) -> Result<Id, StoreError> {
        if payload.len() > BLOB_LIMIT {
            return Err(StoreError::TooLarge {
                limit: BLOB_LIMIT,
                got: payload.len(),
            });
        }
        let id = object_id(Kind::Blob, payload);
        let key = S3Store::ephemeral_key(snap_id_hex, &id)?;
        let compressed = compress_payload(payload)?;
        self.op("put", self.store.put(&key, PutPayload::from(compressed)))?;
        Ok(id)
    }

    fn get_blob(&self, snap_id_hex: &str, id: &Id) -> Result<Vec<u8>, StoreError> {
        let key = S3Store::ephemeral_key(snap_id_hex, id)?;
        let compressed = self.op_map(
            async {
                let payload = self.store.get(&key).await?;
                payload.bytes().await
            },
            |r| match r {
                Ok(b) => Ok(b),
                Err(e) if is_not_found(&e) => Err(StoreError::BlobMissing(id.to_hex())),
                Err(_) => Err(StoreError::S3 { op: "get" }),
            },
        )?;
        decompress_verify(&compressed, id, BLOB_LIMIT)
    }

    fn has(&self, snap_id_hex: &str, ids: &[Id]) -> Result<Vec<bool>, StoreError> {
        // Upfront validation: the fs backend refuses a non-hex snapshot id
        // even for an empty batch; the per-id key build below covers only
        // non-empty ones.
        S3Store::snap_prefix(snap_id_hex)?;
        let mut out = Vec::with_capacity(ids.len());
        for id in ids {
            let key = S3Store::ephemeral_key(snap_id_hex, id)?;
            out.push(self.op_map(self.store.head(&key), |r| match r {
                Ok(_) => Ok(true),
                Err(e) if is_not_found(&e) => Ok(false),
                Err(_) => Err(StoreError::S3 { op: "head" }),
            })?);
        }
        Ok(out)
    }

    /// Deletes every object under `ephemeral/<snap>/` — the wholesale
    /// delete the fs backend does with one `remove_dir_all`. Converges by
    /// re-listing: S3 prefix deletion is not atomic, so the loop continues
    /// while any key remains (a competing PUT of the same snapshot is not
    /// a shape naatre produces — snapshot ids are single-writer CSPRNG).
    fn delete_snapshot(&self, snap_id_hex: &str) -> Result<(), StoreError> {
        let prefix = S3Store::snap_prefix(snap_id_hex)?;
        loop {
            let paths: Vec<Path> = self.op("list", async {
                let mut out: Vec<Path> = Vec::new();
                let mut stream = self.store.list(Some(&prefix));
                while let Some(meta) = stream.next().await {
                    out.push(meta?.location);
                    if out.len() == 1000 {
                        break; // batch cap; the outer loop re-lists
                    }
                }
                Ok(out)
            })?;
            if paths.is_empty() {
                return Ok(());
            }
            self.op("delete", async {
                let batch = stream::iter(paths.into_iter().map(Ok::<_, object_store::Error>));
                let mut results = self.store.delete_stream(Box::pin(batch));
                while let Some(r) = results.next().await {
                    r?;
                }
                Ok(())
            })?;
        }
    }

    /// Lists `ephemeral/` first-level prefixes whose NEWEST object is
    /// older than `older_than_ns` — the S3 analog of the fs dir-mtime
    /// sweep. `now_ns` is the caller's clock, carried for signature parity
    /// with the fs backend; the age signal itself is S3's LastModified.
    /// Non-hex prefixes are returned like fs returns non-UTF-8 dir names —
    /// the GC's row cross-check filters them.
    fn rowless_dirs(&self, older_than_ns: i64, now_ns: i64) -> Result<Vec<String>, StoreError> {
        let _ = now_ns;
        let root = Path::from("ephemeral");
        let listing = self.op("list", self.store.list_with_delimiter(Some(&root)))?;
        let mut out = Vec::new();
        for prefix in listing.common_prefixes {
            let full = prefix.to_string();
            let name = match full.strip_prefix("ephemeral/") {
                // A common prefix always carries the trailing '/'.
                Some(stripped) => stripped.strip_suffix('/').unwrap_or(stripped),
                None => continue,
            };
            let newest: i64 = self.op("list", async {
                let mut newest: i64 = 0;
                let mut stream = self.store.list(Some(&prefix));
                while let Some(meta) = stream.next().await {
                    let ns = meta?
                        .last_modified
                        .timestamp_nanos_opt()
                        .unwrap_or(i64::MAX);
                    if ns > newest {
                        newest = ns;
                    }
                }
                Ok(newest)
            })?;
            if newest < older_than_ns {
                out.push(name.to_string());
            }
        }
        out.sort();
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keys_mirror_the_fs_layout() {
        let id = object_id(Kind::Blob, b"layout check");
        let hex = id.to_hex();
        assert_eq!(
            S3Store::objects_key(&id).to_string(),
            format!("objects/{}/{}/{}", &hex[0..2], &hex[2..4], &hex[4..])
        );
        let snap = format!("{:062x}{:02x}", 0u128, 7);
        assert_eq!(
            S3Store::ephemeral_key(&snap, &id).unwrap().to_string(),
            format!(
                "ephemeral/{snap}/objects/{}/{}/{}",
                &hex[0..2],
                &hex[2..4],
                &hex[4..]
            )
        );
        assert_eq!(
            S3Store::snap_prefix(&snap).unwrap().to_string(),
            format!("ephemeral/{snap}")
        );
    }

    #[test]
    fn non_hex_snap_ids_rejected_for_every_key_shape() {
        let id = object_id(Kind::Blob, b"x");
        let long_a = "a".repeat(63);
        let long_g = "g".repeat(64);
        for bad in ["", "ABC", long_g.as_str(), long_a.as_str(), "../x"] {
            assert!(S3Store::ephemeral_key(bad, &id).is_err(), "{bad:?}");
            assert!(S3Store::snap_prefix(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn s3_error_is_redacted_and_names_the_op() {
        let e = StoreError::S3 { op: "put" };
        let msg = e.to_string();
        assert!(msg.contains("put"), "{msg}");
        assert!(msg.contains("withheld"), "{msg}");
    }
}
