//! The binary stat index, `.naatre/index.nai` (the M3 pin: a hand-rolled
//! little-endian format with a magic + version byte, never JSON).
//!
//! Byte format (documented here; it is the on-disk contract):
//!
//! ```text
//! header:
//!   magic            6 bytes  b"NAIidx"
//!   format version   1 byte   0x01
//!   record count     u32 LE
//! records, exactly `count` of them, strictly ascending by path bytes:
//!   path length      u32 LE
//!   path bytes       (UTF-8, NFC-normalized, no NUL)
//!   blob id          32 raw bytes (BLAKE3 id, not hex)
//!   size             u64 LE
//!   mtime_ns         i64 LE
//!   ctime_ns         i64 LE
//!   ino              u64 LE
//!   exec             1 byte  (0 or 1)
//! ```
//!
//! Every read is bounds-checked; any violation (bad magic/version, short
//! read, non-UTF-8 or non-ascending path, exec byte outside {0,1}, trailing
//! bytes) is [`IndexError::Corrupt`]. Recovery is `naatre doctor --rebuild`.

use std::collections::BTreeMap;
use std::os::unix::fs::MetadataExt;
use std::path::Path;

use crate::wc::{INDEX_FILE, write_atomic};

pub const MAGIC: &[u8; 6] = b"NAIidx";
pub const FORMAT_VERSION: u8 = 1;

/// One indexed file: its NFC path, content hash, and the stat fields the
/// racily-clean rule compares (size, mtime_ns, ctime_ns, ino) plus exec.
#[derive(Clone, Debug)]
pub struct Row {
    pub path: String,
    pub blob: [u8; 32],
    pub size: u64,
    pub mtime_ns: i64,
    pub ctime_ns: i64,
    pub ino: u64,
    pub exec: bool,
}

impl Row {
    /// Captures the stat fields from `meta` (a regular file under the
    /// checkout root).
    pub fn from_stat(path: String, blob: [u8; 32], meta: &std::fs::Metadata) -> Row {
        Row {
            path,
            blob,
            size: meta.size(),
            mtime_ns: meta.mtime_nsec(),
            ctime_ns: meta.ctime_nsec(),
            ino: meta.ino(),
            exec: meta.mode() & 0o111 != 0,
        }
    }
}

/// True when all four stat fields match (the "definitely untouched" fast
/// path; anything else goes through a rehash in [`crate::wc::scan`]).
pub fn stat_matches(row: &Row, meta: &std::fs::Metadata) -> bool {
    row.size == meta.size()
        && row.mtime_ns == meta.mtime_nsec()
        && row.ctime_ns == meta.ctime_nsec()
        && row.ino == meta.ino()
}

/// Index load failures: the file is absent, or present but not a valid
/// index of the pinned format.
#[derive(Debug)]
pub enum IndexError {
    Missing,
    Corrupt(String),
}

impl std::fmt::Display for IndexError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            IndexError::Missing => f.write_str("index file is missing"),
            IndexError::Corrupt(why) => write!(f, "index is corrupt: {why}"),
        }
    }
}

/// The stat index: rows keyed by NFC path, so iteration is ascending by
/// path bytes (the file format's order).
#[derive(Default, Debug)]
pub struct Index {
    pub rows: BTreeMap<String, Row>,
}

impl Index {
    pub fn load(path: &Path) -> Result<Index, IndexError> {
        let bytes = std::fs::read(path).map_err(|e| match e.kind() {
            std::io::ErrorKind::NotFound => IndexError::Missing,
            _ => IndexError::Corrupt(format!("unreadable ({e})")),
        })?;
        Self::decode(&bytes)
    }

    pub fn save(idx: &Index, path: &Path) -> std::io::Result<()> {
        let bytes = idx.encode();
        write_atomic(path, &bytes, true)
            .map_err(|e| std::io::Error::other(format!("{INDEX_FILE}: {e}")))
    }

    fn decode(bytes: &[u8]) -> Result<Index, IndexError> {
        let corrupt = |why: &str| IndexError::Corrupt(why.to_string());
        if bytes.len() < 11 {
            return Err(corrupt("shorter than the header"));
        }
        if &bytes[0..6] != MAGIC {
            return Err(corrupt("bad magic"));
        }
        if bytes[6] != FORMAT_VERSION {
            return Err(corrupt(
                format!("unknown format version {}", bytes[6]).as_str(),
            ));
        }
        let count = u32::from_le_bytes(bytes[7..11].try_into().expect("count is 4 bytes")) as usize;
        let mut rows = BTreeMap::new();
        let mut cur = &bytes[11..];
        let mut prev_path: Option<&[u8]> = None;
        for _ in 0..count {
            if cur.len() < 4 {
                return Err(corrupt("truncated path length"));
            }
            let plen = u32::from_le_bytes(cur[0..4].try_into().expect("plen is 4 bytes")) as usize;
            cur = &cur[4..];
            if cur.len() < plen + 32 + 8 + 8 + 8 + 8 + 1 {
                return Err(corrupt("truncated record"));
            }
            let path_bytes = &cur[..plen];
            cur = &cur[plen..];
            let Ok(path) = std::str::from_utf8(path_bytes) else {
                return Err(corrupt("path is not UTF-8"));
            };
            if path.contains('\0') {
                return Err(corrupt("path contains NUL"));
            }
            if let Some(prev) = prev_path
                && path_bytes <= prev
            {
                return Err(corrupt("records not strictly ascending by path"));
            }
            prev_path = Some(path_bytes);
            let mut blob = [0u8; 32];
            blob.copy_from_slice(&cur[..32]);
            cur = &cur[32..];
            let u64_at =
                |off: usize| u64::from_le_bytes(cur[off..off + 8].try_into().expect("8 bytes"));
            let size = u64_at(0);
            let mtime_ns = u64_at(8) as i64;
            let ctime_ns = u64_at(16) as i64;
            let ino = u64_at(24);
            cur = &cur[32..];
            let exec = match cur[0] {
                0 => false,
                1 => true,
                other => return Err(corrupt(format!("exec byte {other} not 0/1").as_str())),
            };
            cur = &cur[1..];
            rows.insert(
                path.to_string(),
                Row {
                    path: path.to_string(),
                    blob,
                    size,
                    mtime_ns,
                    ctime_ns,
                    ino,
                    exec,
                },
            );
        }
        if !cur.is_empty() {
            return Err(corrupt("trailing bytes after the last record"));
        }
        Ok(Index { rows })
    }

    fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.extend_from_slice(MAGIC);
        out.push(FORMAT_VERSION);
        out.extend_from_slice(&(self.rows.len() as u32).to_le_bytes());
        for row in self.rows.values() {
            out.extend_from_slice(&(row.path.len() as u32).to_le_bytes());
            out.extend_from_slice(row.path.as_bytes());
            out.extend_from_slice(&row.blob);
            out.extend_from_slice(&row.size.to_le_bytes());
            out.extend_from_slice(&(row.mtime_ns as u64).to_le_bytes());
            out.extend_from_slice(&(row.ctime_ns as u64).to_le_bytes());
            out.extend_from_slice(&row.ino.to_le_bytes());
            out.push(u8::from(row.exec));
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn row(path: &str, exec: bool) -> Row {
        Row {
            path: path.to_string(),
            blob: [7u8; 32],
            size: 12,
            mtime_ns: 1_000,
            ctime_ns: 2_000,
            ino: 42,
            exec,
        }
    }

    #[test]
    fn roundtrip_preserves_rows_and_order() {
        let mut idx = Index::default();
        idx.rows.insert("a.txt".into(), row("a.txt", false));
        idx.rows.insert("b/c.txt".into(), row("b/c.txt", true));
        let bytes = idx.encode();
        let back = Index::decode(&bytes).expect("decodes");
        assert_eq!(back.rows.len(), 2);
        assert_eq!(back.rows["a.txt"].ino, 42);
        assert!(back.rows["b/c.txt"].exec);
        assert_eq!(back.rows["a.txt"].blob, [7u8; 32]);
    }

    #[test]
    fn corruption_is_reported_never_panics() {
        let mut idx = Index::default();
        idx.rows.insert("a.txt".into(), row("a.txt", false));
        idx.rows.insert("b.txt".into(), row("b.txt", false));
        let bytes = idx.encode();

        // bad magic
        let mut bad = bytes.clone();
        bad[2] = b'X';
        assert!(matches!(Index::decode(&bad), Err(IndexError::Corrupt(_))));

        // truncated record
        assert!(matches!(
            Index::decode(&bytes[..bytes.len() - 3]),
            Err(IndexError::Corrupt(_))
        ));

        // unsorted records: hand-assemble the byte stream out of order
        let rec = |path: &str| {
            let mut out = Vec::new();
            out.extend_from_slice(&(path.len() as u32).to_le_bytes());
            out.extend_from_slice(path.as_bytes());
            out.extend_from_slice(&[7u8; 32]);
            out.extend_from_slice(&12u64.to_le_bytes());
            out.extend_from_slice(&1000i64.to_le_bytes());
            out.extend_from_slice(&2000i64.to_le_bytes());
            out.extend_from_slice(&42u64.to_le_bytes());
            out.push(0);
            out
        };
        let mut raw = Vec::new();
        raw.extend_from_slice(MAGIC);
        raw.push(FORMAT_VERSION);
        raw.extend_from_slice(&2u32.to_le_bytes());
        raw.extend_from_slice(&rec("b.txt"));
        raw.extend_from_slice(&rec("a.txt"));
        assert!(matches!(Index::decode(&raw), Err(IndexError::Corrupt(_))));

        // trailing bytes
        let mut trailing = bytes.clone();
        trailing.push(0);
        assert!(matches!(
            Index::decode(&trailing),
            Err(IndexError::Corrupt(_))
        ));

        // garbage never panics
        assert!(matches!(
            Index::decode(&[0u8; 40]),
            Err(IndexError::Corrupt(_))
        ));
    }
}
