//! Changesets: the delta-only change record, its canonical byte encoding,
//! strict decoder, and validation.

use crate::error::CoreError;
use crate::ids::{self, Id};
use crate::path;

/// Canonical encodings of a single object stay at or below this size.
pub(crate) const MAX_OBJECT_BYTES: usize = 16 * 1024 * 1024;
/// Maximum parents a changeset may record.
pub const MAX_PARENTS: usize = 2;
/// Maximum entries per changeset or tree.
pub const MAX_ENTRIES: usize = 100_000;

const MAGIC: &[u8] = b"naatre-changeset\x01";

/// What happened to a file.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Op {
    /// New file.
    Add,
    /// Modified file (content and/or exec bit).
    Mod,
    /// Deleted file.
    Del,
}

impl Op {
    const fn tag(self) -> u8 {
        match self {
            Op::Add => 0,
            Op::Mod => 1,
            Op::Del => 2,
        }
    }

    const fn from_tag(tag: u8) -> Option<Op> {
        match tag {
            0 => Some(Op::Add),
            1 => Some(Op::Mod),
            2 => Some(Op::Del),
            _ => None,
        }
    }
}

/// One file-level change inside a changeset.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FileChange {
    /// Repo-relative path, grammar-valid and NFC.
    pub path: String,
    /// The operation.
    pub op: Op,
    /// Content id; `Add`/`Mod` carry it, `Del` must be `None`.
    pub blob: Option<Id>,
    /// Content byte length; `Add`/`Mod` carry it, `Del` must be `None`.
    pub size: Option<u64>,
    /// Exec bit; `Add`/`Mod` carry it, `Del` must be `None`.
    pub exec: Option<bool>,
}

/// A changeset: parents plus a sorted list of file changes.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Changeset {
    /// Repository the changeset belongs to.
    pub repo_id: u64,
    /// Parent changeset ids; 0, 1, or 2 of them.
    pub parents: Vec<Id>,
    /// Client-supplied author string (`Name <email>`).
    pub author: String,
    /// Commit message.
    pub message: String,
    /// Author timestamp in nanoseconds since the Unix epoch.
    pub timestamp_ns: u64,
    /// File changes, sorted by path bytes strictly ascending.
    pub changes: Vec<FileChange>,
}

/// Canonical byte encoding.
///
/// Layout (all integers little-endian; `str` = u64le byte length + UTF-8
/// bytes): magic `"naatre-changeset\x01"`, u64le `repo_id`, u8 parent count
/// then that many raw 32-byte parent ids, `str` author, `str` message, u64le
/// `timestamp_ns`, u32le entry count, then entries in strictly ascending path
/// byte order — each `str` path, u8 op tag (Add=0, Mod=1, Del=2) and, for
/// Add/Mod, a 32-byte blob id, u64le size, u8 exec (0 or 1).
///
/// Errors: [`CoreError::EncodeTooManyParents`], [`CoreError::EncodeUnsorted`],
/// [`CoreError::EncodeMissingBlob`] (Add/Mod missing blob/size/exec; Del
/// carrying any of them), [`CoreError::EncodeTooLarge`].
pub fn encode(cs: &Changeset) -> Result<Vec<u8>, CoreError> {
    if cs.parents.len() > MAX_PARENTS {
        return Err(CoreError::EncodeTooManyParents);
    }
    if !cs
        .changes
        .windows(2)
        .all(|w| w[0].path.as_bytes() < w[1].path.as_bytes())
    {
        return Err(CoreError::EncodeUnsorted);
    }
    let mut out = Vec::with_capacity(64);
    out.extend_from_slice(MAGIC);
    out.extend_from_slice(&cs.repo_id.to_le_bytes());
    out.push(cs.parents.len() as u8);
    for p in &cs.parents {
        out.extend_from_slice(&p.0);
    }
    write_str(&mut out, &cs.author);
    write_str(&mut out, &cs.message);
    out.extend_from_slice(&cs.timestamp_ns.to_le_bytes());
    out.extend_from_slice(&(cs.changes.len() as u32).to_le_bytes());
    for c in &cs.changes {
        write_str(&mut out, &c.path);
        out.push(c.op.tag());
        if c.op != Op::Del {
            let blob = c.blob.ok_or(CoreError::EncodeMissingBlob)?;
            let size = c.size.ok_or(CoreError::EncodeMissingBlob)?;
            let exec = c.exec.ok_or(CoreError::EncodeMissingBlob)?;
            out.extend_from_slice(&blob.0);
            out.extend_from_slice(&size.to_le_bytes());
            out.push(u8::from(exec));
        } else if c.blob.is_some() || c.size.is_some() || c.exec.is_some() {
            return Err(CoreError::EncodeMissingBlob);
        }
    }
    if out.len() > MAX_OBJECT_BYTES {
        return Err(CoreError::EncodeTooLarge);
    }
    Ok(out)
}

/// Strict decoder: accepts exactly the canonical encoding.
///
/// Rejects trailing bytes, length claims beyond the remaining input or the
/// 16 MiB cap, unknown op or exec tags, invalid UTF-8, and short input. The
/// decoder is structural only: ordering and cross-field coherence are
/// [`validate`]'s job.
pub fn decode(bytes: &[u8]) -> Result<Changeset, CoreError> {
    let mut r = Cursor::new(bytes);
    let magic = r.take(MAGIC.len())?;
    if magic != MAGIC {
        return Err(CoreError::DecodeBadMagic);
    }
    let repo_id = r.u64_le()?;
    let parent_count = r.u8()? as usize;
    if parent_count > MAX_PARENTS {
        return Err(CoreError::DecodeTooLarge);
    }
    let mut parents = Vec::with_capacity(parent_count);
    for _ in 0..parent_count {
        parents.push(Id(r.take32()?));
    }
    let author = r.str_field()?;
    let message = r.str_field()?;
    let timestamp_ns = r.u64_le()?;
    let entry_count = r.u32_le()? as usize;
    // Every entry occupies at least 9 bytes (a u64le path length + 1 op byte).
    if entry_count.saturating_mul(9) > r.remaining() {
        return Err(CoreError::DecodeTooLarge);
    }
    let mut changes = Vec::with_capacity(entry_count.min(MAX_ENTRIES));
    for _ in 0..entry_count {
        let path = r.str_field()?;
        let tag = r.u8()?;
        let op = Op::from_tag(tag).ok_or(CoreError::DecodeInvalidTag(tag))?;
        let (blob, size, exec) = if op == Op::Del {
            (None, None, None)
        } else {
            let blob = Id(r.take32()?);
            let size = r.u64_le()?;
            let exec = match r.u8()? {
                0 => false,
                1 => true,
                other => return Err(CoreError::DecodeInvalidTag(other)),
            };
            (Some(blob), Some(size), Some(exec))
        };
        changes.push(FileChange {
            path,
            op,
            blob,
            size,
            exec,
        });
    }
    if r.remaining() != 0 {
        return Err(CoreError::DecodeTrailingBytes);
    }
    Ok(Changeset {
        repo_id,
        parents,
        author,
        message,
        timestamp_ns,
        changes,
    })
}

/// The object id of a changeset: BLAKE3 over its canonical bytes.
pub fn changeset_id(cs: &Changeset) -> Result<Id, CoreError> {
    Ok(ids::object_id(ids::Kind::Changeset, &encode(cs)?))
}

/// Validates semantic rules beyond the byte encoding: path grammar on every
/// path, per-directory case-fold duplicates, the author/message character
/// rule, parent and entry caps.
///
/// The character rule rejects Unicode categories Cc and Cf everywhere in
/// author and message except `'\n'` and `'\t'`. Path length (4096 bytes) is
/// enforced by [`path::validate`] itself.
pub fn validate(cs: &Changeset) -> Result<(), CoreError> {
    for c in &cs.changes {
        path::validate(&c.path)?;
    }
    let paths: Vec<String> = cs.changes.iter().map(|c| c.path.clone()).collect();
    path::check_case_fold_duplicates(&paths)?;
    check_text(&cs.author).map_err(|index| CoreError::AuthorChar { index })?;
    check_text(&cs.message).map_err(|index| CoreError::MessageChar { index })?;
    if cs.parents.len() > MAX_PARENTS {
        return Err(CoreError::EncodeTooManyParents);
    }
    if cs.changes.is_empty() {
        return Err(CoreError::EncodeEmptyChangeset);
    }
    if cs.changes.len() > MAX_ENTRIES {
        return Err(CoreError::EncodeTooManyEntries);
    }
    Ok(())
}

fn check_text(s: &str) -> Result<(), usize> {
    for (index, c) in s.char_indices() {
        if (path::is_cc(c) || path::is_cf(c)) && c != '\n' && c != '\t' {
            return Err(index);
        }
    }
    Ok(())
}

/// Appends a u64le length prefix plus the raw UTF-8 bytes of `s`.
pub(crate) fn write_str(out: &mut Vec<u8>, s: &str) {
    out.extend_from_slice(&(s.len() as u64).to_le_bytes());
    out.extend_from_slice(s.as_bytes());
}

/// Bounds-checked byte cursor shared by the strict decoders; every short
/// read maps to [`CoreError::DecodeUnexpectedEof`].
pub(crate) struct Cursor<'a> {
    data: &'a [u8],
    pos: usize,
}

impl<'a> Cursor<'a> {
    pub(crate) fn new(data: &'a [u8]) -> Self {
        Cursor { data, pos: 0 }
    }

    pub(crate) fn remaining(&self) -> usize {
        self.data.len() - self.pos
    }

    pub(crate) fn take(&mut self, n: usize) -> Result<&'a [u8], CoreError> {
        if self.remaining() < n {
            return Err(CoreError::DecodeUnexpectedEof);
        }
        let out = &self.data[self.pos..self.pos + n];
        self.pos += n;
        Ok(out)
    }

    pub(crate) fn take32(&mut self) -> Result<[u8; 32], CoreError> {
        let b = self.take(32)?;
        Ok(b.try_into().expect("take(32) yields 32 bytes"))
    }

    pub(crate) fn u8(&mut self) -> Result<u8, CoreError> {
        Ok(self.take(1)?[0])
    }

    pub(crate) fn u32_le(&mut self) -> Result<u32, CoreError> {
        let b = self.take(4)?;
        Ok(u32::from_le_bytes(
            b.try_into().expect("take(4) yields 4 bytes"),
        ))
    }

    pub(crate) fn u64_le(&mut self) -> Result<u64, CoreError> {
        let b = self.take(8)?;
        Ok(u64::from_le_bytes(
            b.try_into().expect("take(8) yields 8 bytes"),
        ))
    }

    /// A u64le-length-prefixed string field with the 16 MiB cap.
    pub(crate) fn str_field(&mut self) -> Result<String, CoreError> {
        let len = self.u64_le()? as usize;
        if len > MAX_OBJECT_BYTES {
            return Err(CoreError::DecodeLengthOverflow);
        }
        if len > self.remaining() {
            return Err(CoreError::DecodeTooLarge);
        }
        let bytes = self.take(len)?;
        String::from_utf8(bytes.to_vec()).map_err(|_| CoreError::DecodeInvalidUtf8)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base_cs() -> Changeset {
        Changeset {
            repo_id: 1,
            parents: vec![],
            author: "A <a@b.c>".into(),
            message: "m".into(),
            timestamp_ns: 1,
            changes: vec![FileChange {
                path: "a.txt".into(),
                op: Op::Add,
                blob: Some(Id([7u8; 32])),
                size: Some(3),
                exec: Some(false),
            }],
        }
    }

    #[test]
    fn roundtrip_and_reject_del_with_payload() {
        let cs = base_cs();
        let bytes = encode(&cs).unwrap();
        assert_eq!(decode(&bytes).unwrap(), cs);
        let mut bad = cs.clone();
        bad.changes[0] = FileChange {
            path: "a.txt".into(),
            op: Op::Del,
            blob: Some(Id([7u8; 32])),
            size: None,
            exec: None,
        };
        assert_eq!(encode(&bad), Err(CoreError::EncodeMissingBlob));
    }
}
