//! Shared filesystem helpers: the single tmp+rename+fsync write path used by
//! both the history CAS and the ephemeral snapshot store.

use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use naatre_core::ids::{Id, Kind, object_id};

use crate::error::StoreError;

pub(crate) const DIR_PERMS: u32 = 0o700;
pub(crate) const FILE_PERMS: u32 = 0o600;

/// Attach a path to an io error so the display message names the location.
pub(crate) fn io_ctx(err: std::io::Error, path: &Path) -> StoreError {
    StoreError::Io(std::io::Error::other(format!("{}: {err}", path.display())))
}

/// Creates `path` (and parents) with 0700; ok if it already exists.
pub(crate) fn ensure_dir(path: &Path) -> Result<(), StoreError> {
    fs::DirBuilder::new()
        .mode(DIR_PERMS)
        .recursive(true)
        .create(path)
        .map_err(|e| io_ctx(e, path))?;
    Ok(())
}

/// True when `s` is exactly 64 lowercase hex characters.
pub(crate) fn is_hex64(s: &str) -> bool {
    naatre_core::ids::is_hex_id(s)
}

/// A unique-enough staging name: pid, nanos, and a per-process counter.
/// Uniqueness is what matters — no path is ever derived from this name.
fn tmp_unique() -> String {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!(
        "{}-{}-{}",
        std::process::id(),
        nanos,
        COUNTER.fetch_add(1, Ordering::Relaxed)
    )
}

/// CAS shard path for `id` under `root`: `root/<2hex>/<2hex>/<62hex>`.
pub(crate) fn shard_path(root: &Path, id: &Id) -> PathBuf {
    let hex = id.to_hex();
    root.join(&hex[0..2]).join(&hex[2..4]).join(&hex[4..])
}

/// Zstd (level 3) compression of one CAS payload — the single content
/// encoding shared by the fs CAS and the S3 backend (the S3 keys are
/// byte-for-byte copies of the fs object files, see `s3.rs`).
pub(crate) fn compress_payload(payload: &[u8]) -> Result<Vec<u8>, StoreError> {
    let mut encoder = zstd::stream::write::Encoder::new(Vec::new(), 3).map_err(zstr)?;
    encoder.write_all(payload).map_err(zstr)?;
    encoder.finish().map_err(zstr)
}

/// Writes `payload` into the CAS at `objects_root`, compressed with zstd
/// level 3, through `tmp_root` staging, fsyncing the file before the rename.
///
/// Durability invariant: after the rename, the bytes are on disk. Returns
/// the object id and the STORED (compressed) byte size — the server's
/// `objects.csize` column reads it from the backend, never by re-statting a
/// local shard (the S3 backend has none). An existing object file
/// short-circuits to the same `(id, size)` without touching it.
pub(crate) fn write_compressed(
    objects_root: &Path,
    tmp_root: &Path,
    payload: &[u8],
    limit: usize,
) -> Result<(Id, u64), StoreError> {
    if payload.len() > limit {
        return Err(StoreError::TooLarge {
            limit,
            got: payload.len(),
        });
    }
    let id = object_id(Kind::Blob, payload);
    let hex = id.to_hex();
    let final_path = shard_path(objects_root, &id);
    if let Ok(meta) = std::fs::metadata(&final_path) {
        // Identical content-addressed bytes: the size on disk IS this
        // payload's stored size.
        return Ok((id, meta.len()));
    }

    ensure_dir(tmp_root)?;
    let tmp_path = tmp_root.join(format!("{}-{hex}", tmp_unique()));

    let file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .mode(FILE_PERMS)
        .open(&tmp_path)
        .map_err(|e| io_ctx(e, &tmp_path))?;
    let mut encoder = zstd::stream::write::Encoder::new(file, 3).map_err(zstr)?;
    encoder.write_all(payload).map_err(zstr)?;
    // try_finish hands the encoder back on failure so writing could resume;
    // we treat any failure as fatal for this object.
    let file = match encoder.try_finish() {
        Ok(w) => w,
        Err((_encoder, e)) => return Err(zstr(e)),
    };
    file.sync_all().map_err(|e| io_ctx(e, &tmp_path))?;

    // shard_path always ends in the object file name, so both parents exist.
    let parent = final_path.parent().expect("shard path has a parent");
    let grandparent = parent.parent().expect("shard parent has a parent");
    ensure_dir(grandparent)?;
    ensure_dir(parent)?;
    fs::rename(&tmp_path, &final_path).map_err(|e| io_ctx(e, &tmp_path))?;
    // Best-effort fsync of the two shard parent dirs; some platforms refuse
    // to open directories, and that failure is ignored.
    for dir in [grandparent, parent] {
        if let Ok(d) = File::open(dir) {
            let _ = d.sync_all();
        }
    }

    let csize = std::fs::metadata(&final_path)
        .map(|m| m.len())
        .map_err(|e| io_ctx(e, &final_path))?;
    Ok((id, csize))
}

/// Reads and zstd-decompresses `reader` with a hard expanded-size cap. The
/// chunked read loop never allocates beyond `limit` plus one chunk, so a
/// small crafted compressed input cannot expand into a huge allocation.
/// Shared by the fs CAS (`File`) and the S3 backend (compressed bytes).
pub(crate) fn read_decompressed_bounded(
    reader: impl std::io::Read,
    limit: usize,
) -> Result<Vec<u8>, StoreError> {
    let mut decoder = zstd::stream::read::Decoder::new(reader).map_err(zstr)?;
    let mut out = Vec::new();
    let mut buf = [0u8; 64 * 1024];
    let mut total = 0usize;
    loop {
        let n = decoder.read(&mut buf).map_err(zstr)?;
        if n == 0 {
            break;
        }
        total += n;
        if total > limit {
            return Err(StoreError::TooLarge { limit, got: total });
        }
        out.extend_from_slice(&buf[..n]);
    }
    Ok(out)
}

/// Decompresses `compressed` under the expanded-size cap and verifies it
/// hashes to `id`. A content hashing differently is `HashMismatch`
/// (nothing is deleted — the caller decides).
pub(crate) fn decompress_verify(
    compressed: &[u8],
    id: &Id,
    limit: usize,
) -> Result<Vec<u8>, StoreError> {
    let out = read_decompressed_bounded(compressed, limit)?;
    let actual = object_id(Kind::Blob, &out);
    if actual != *id {
        return Err(StoreError::HashMismatch {
            expected: id.to_hex(),
            actual: actual.to_hex(),
        });
    }
    Ok(out)
}

/// Reads the blob for `id` from the CAS at `objects_root`, decompresses it
/// under the expanded-size cap, and verifies its hash. A missing file is
/// `BlobMissing`; a file whose content hashes differently is
/// `HashMismatch` (nothing is deleted — the caller decides).
pub(crate) fn read_blob_verified(
    objects_root: &Path,
    id: &Id,
    limit: usize,
) -> Result<Vec<u8>, StoreError> {
    let path = shard_path(objects_root, id);
    let file = match File::open(&path) {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Err(StoreError::BlobMissing(id.to_hex()));
        }
        Err(e) => return Err(io_ctx(e, &path)),
    };
    let out = read_decompressed_bounded(file, limit)?;
    let actual = object_id(Kind::Blob, &out);
    if actual != *id {
        return Err(StoreError::HashMismatch {
            expected: id.to_hex(),
            actual: actual.to_hex(),
        });
    }
    Ok(out)
}

fn zstr(e: std::io::Error) -> StoreError {
    StoreError::Zstd(e.to_string())
}
