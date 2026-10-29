//! Trees: directory listings, canonical byte encoding, strict decoder, and
//! validation.

use crate::changeset::{Cursor, MAX_ENTRIES, MAX_OBJECT_BYTES, write_str};
use crate::error::CoreError;
use crate::ids::{self, Id};
use crate::path;

const MAGIC: &[u8] = b"naatre-tree\x01";

/// Whether a tree entry is a file or a subdirectory.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum EntryKind {
    /// File content; `object` is a blob id.
    File,
    /// Subdirectory; `object` is the child tree id.
    Dir,
}

impl EntryKind {
    const fn tag(self) -> u8 {
        match self {
            EntryKind::File => 0,
            EntryKind::Dir => 1,
        }
    }

    const fn from_tag(tag: u8) -> Option<EntryKind> {
        match tag {
            0 => Some(EntryKind::File),
            1 => Some(EntryKind::Dir),
            _ => None,
        }
    }
}

/// One entry in a tree.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TreeEntry {
    /// Entry name, a single grammar-valid NFC component (no `/`).
    pub name: String,
    /// File or directory.
    pub kind: EntryKind,
    /// Blob id for files, child tree id for directories.
    pub object: Id,
    /// File: content byte length. Dir: recursive total of contained files.
    pub size: u64,
    /// Exec bit; only meaningful for files, always `false` for dirs.
    pub exec: bool,
}

/// A directory listing.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Tree {
    /// Entries, sorted by name bytes strictly ascending.
    pub entries: Vec<TreeEntry>,
}

/// Canonical byte encoding.
///
/// Layout (all integers little-endian; `str` = u64le byte length + UTF-8
/// bytes): magic `"naatre-tree\x01"`, u32le entry count, then entries in
/// strictly ascending name byte order — each `str` name, u8 kind tag
/// (File=0, Dir=1), 32-byte object id, u64le size, u8 exec; directory
/// entries always write exec byte 0.
///
/// Errors: [`CoreError::EncodeUnsorted`], [`CoreError::EncodeTooLarge`].
pub fn encode(tree: &Tree) -> Result<Vec<u8>, CoreError> {
    if !tree
        .entries
        .windows(2)
        .all(|w| w[0].name.as_bytes() < w[1].name.as_bytes())
    {
        return Err(CoreError::EncodeUnsorted);
    }
    let mut out = Vec::with_capacity(16);
    out.extend_from_slice(MAGIC);
    out.extend_from_slice(&(tree.entries.len() as u32).to_le_bytes());
    for e in &tree.entries {
        write_str(&mut out, &e.name);
        out.push(e.kind.tag());
        out.extend_from_slice(&e.object.0);
        out.extend_from_slice(&e.size.to_le_bytes());
        out.push(match e.kind {
            EntryKind::File => u8::from(e.exec),
            EntryKind::Dir => 0,
        });
    }
    if out.len() > MAX_OBJECT_BYTES {
        return Err(CoreError::EncodeTooLarge);
    }
    Ok(out)
}

/// Strict decoder: accepts exactly the canonical encoding.
///
/// Rejects trailing bytes, entry counts that cannot fit in the remaining
/// input, unknown kind tags, exec bytes outside 0/1 or nonzero on directory
/// entries, invalid UTF-8, and short input. Structural only: name rules and
/// case-fold duplicates are [`validate`]'s job.
pub fn decode(bytes: &[u8]) -> Result<Tree, CoreError> {
    let mut r = Cursor::new(bytes);
    let magic = r.take(MAGIC.len())?;
    if magic != MAGIC {
        return Err(CoreError::DecodeBadMagic);
    }
    let entry_count = r.u32_le()? as usize;
    // Every entry occupies at least 50 bytes (name length prefix, kind tag,
    // 32-byte id, size, exec byte).
    if entry_count.saturating_mul(50) > r.remaining() {
        return Err(CoreError::DecodeTooLarge);
    }
    let mut entries = Vec::with_capacity(entry_count.min(MAX_ENTRIES));
    for _ in 0..entry_count {
        let name = r.str_field()?;
        let tag = r.u8()?;
        let kind = EntryKind::from_tag(tag).ok_or(CoreError::DecodeInvalidTag(tag))?;
        let object = Id(r.take32()?);
        let size = r.u64_le()?;
        let exec = match r.u8()? {
            0 => false,
            1 => {
                if kind == EntryKind::Dir {
                    return Err(CoreError::DecodeInvalidTag(1));
                }
                true
            }
            other => return Err(CoreError::DecodeInvalidTag(other)),
        };
        entries.push(TreeEntry {
            name,
            kind,
            object,
            size,
            exec,
        });
    }
    if r.remaining() != 0 {
        return Err(CoreError::DecodeTrailingBytes);
    }
    Ok(Tree { entries })
}

/// The object id of a tree: BLAKE3 over its canonical bytes.
pub fn tree_id(tree: &Tree) -> Result<Id, CoreError> {
    Ok(ids::object_id(ids::Kind::Tree, &encode(tree)?))
}

/// Validates semantic rules beyond the byte encoding: each name is a single
/// grammar-valid component, names are case-fold-distinct within the
/// directory, exec only on file entries, and the entry count cap.
pub fn validate(tree: &Tree) -> Result<(), CoreError> {
    for e in &tree.entries {
        path::validate_component(&e.name)?;
        if e.kind == EntryKind::Dir && e.exec {
            return Err(CoreError::EncodeExecOnDir);
        }
    }
    let names: Vec<String> = tree.entries.iter().map(|e| e.name.clone()).collect();
    path::check_case_fold_duplicates(&names)?;
    if tree.entries.len() > MAX_ENTRIES {
        return Err(CoreError::EncodeTooManyEntries);
    }
    Ok(())
}
