//! Changesets: the delta-only change record, its canonical byte encoding,
//! strict decoder, and validation.
//!
//! Two format versions exist (M12): a conflict-free changeset encodes with
//! the v1 magic and hashes under the v1 id domain — byte-for-byte what every
//! pre-M12 version wrote. A changeset carrying first-class conflict
//! representations ([`ConflictRep`]s) encodes with the v2 magic, appends the
//! conflict section, and hashes under the `naatre\0v2\0` domain; see
//! [`id_for_encoded`].

use crate::conflict::{self, ConflictRep};
use crate::error::CoreError;
use crate::ids::{self, Id};
use crate::path;

/// Canonical encodings of a single object stay at or below this size.
pub(crate) const MAX_OBJECT_BYTES: usize = 16 * 1024 * 1024;
/// Maximum parents a changeset may record.
pub const MAX_PARENTS: usize = 2;
/// Maximum entries per changeset or tree.
pub const MAX_ENTRIES: usize = 100_000;

/// The v1 frame magic (17 bytes). Public so storage and route layers can
/// probe the version byte without a decode.
pub const MAGIC_V1: &[u8; 17] = b"naatre-changeset\x01";
/// The v2 frame magic (17 bytes): the format-v2 marker the GC hold and the
/// read-side `conflicted` peek test in O(1).
pub const MAGIC_V2: &[u8; 17] = b"naatre-changeset\x02";

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
    /// First-class conflict representations (format v2), sorted by path
    /// bytes strictly ascending. Empty for every v1 changeset; non-empty
    /// switches the canonical magic, appends the conflict section, and
    /// moves the id to the v2 domain. The node's root tree then materializes
    /// each conflict's markers ([`conflict::marker_bytes`]).
    pub conflicts: Vec<ConflictRep>,
}

/// Canonical byte encoding.
///
/// Layout (all integers little-endian; `str` = u64le byte length + UTF-8
/// bytes): magic `"naatre-changeset\x01"` (or `"\x02"` when `conflicts` is
/// non-empty), u64le `repo_id`, u8 parent count then that many raw 32-byte
/// parent ids, `str` author, `str` message, u64le `timestamp_ns`, u32le
/// entry count, then entries in strictly ascending path byte order — each
/// `str` path, u8 op tag (Add=0, Mod=1, Del=2) and, for Add/Mod, a 32-byte
/// blob id, u64le size, u8 exec (0 or 1). The v2 encoding then appends the
/// conflict section (see [`conflict`] and docs/format.md): u32le conflict
/// count, each `str` path, u8 flags (bit 0 base present, bit 1 one side
/// deleted the path, bit 2 exec bits present), a 32-byte base id when bit 0
/// is set (followed by its u8 exec, 0 or 1, when bit 2 is set), u32le blob
/// side count (>= 2, or >= 1 when bit 1 is set), then per side its 32-byte
/// id (followed by its u8 exec when bit 2 is set), the `(id, exec)` pairs in
/// strictly ascending order. Bit 2 is set exactly when the base's or some
/// side's exec is 1.
///
/// A conflict-free changeset encodes to exactly the v1 bytes, whatever this
/// module's future gains — v1 ids are never recomputed under a new meaning.
///
/// Errors: [`CoreError::EncodeTooManyParents`], [`CoreError::EncodeUnsorted`],
/// [`CoreError::EncodeMissingBlob`] (Add/Mod missing blob/size/exec; Del
/// carrying any of them), [`CoreError::EncodeUnsortedConflicts`],
/// [`CoreError::EncodeConflictSides`], [`CoreError::EncodeTooLarge`].
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
    conflict::validate(&cs.conflicts)?;
    let mut out = Vec::with_capacity(64);
    out.extend_from_slice(if cs.conflicts.is_empty() {
        MAGIC_V1
    } else {
        MAGIC_V2
    });
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
    if !cs.conflicts.is_empty() {
        out.extend_from_slice(&(cs.conflicts.len() as u32).to_le_bytes());
        for rep in &cs.conflicts {
            write_str(&mut out, &rep.path);
            let exec = conflict::has_exec(rep);
            out.push(
                u8::from(rep.base.is_some()) | (u8::from(rep.deleted) << 1) | (u8::from(exec) << 2),
            );
            if let Some(base) = rep.base {
                out.extend_from_slice(&base.0);
                if exec {
                    out.push(u8::from(rep.base_exec));
                }
            }
            out.extend_from_slice(&(rep.sides.len() as u32).to_le_bytes());
            for (side, &side_exec) in rep.sides.iter().zip(&rep.sides_exec) {
                out.extend_from_slice(&side.0);
                if exec {
                    out.push(u8::from(side_exec));
                }
            }
        }
    }
    if out.len() > MAX_OBJECT_BYTES {
        return Err(CoreError::EncodeTooLarge);
    }
    Ok(out)
}

/// Strict decoder: accepts exactly the canonical encoding, v1 or v2.
///
/// Rejects trailing bytes, length claims beyond the remaining input or the
/// 16 MiB cap, unknown op/exec/flags tags, fewer than two sides per conflict
/// (one with a deletion side),
/// invalid UTF-8, and short input — a v1-era binary fed v2 bytes fails the
/// magic check loudly (it has no v2 arm to misparse into). The decoder is
/// structural only: ordering and cross-field coherence are [`validate`]'s
/// job.
pub fn decode(bytes: &[u8]) -> Result<Changeset, CoreError> {
    let mut r = Cursor::new(bytes);
    let magic = r.take(MAGIC_V1.len())?;
    let is_v2 = if magic == MAGIC_V1 {
        false
    } else if magic == MAGIC_V2 {
        true
    } else {
        return Err(CoreError::DecodeBadMagic);
    };
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
    let mut conflicts = Vec::new();
    if is_v2 {
        let conflict_count = r.u32_le()? as usize;
        // Strict canonical acceptance: a v2 frame ALWAYS carries at least
        // one conflict (encode only picks the v2 magic when the list is
        // non-empty); zero is not a canonical shape and never parses.
        if conflict_count == 0 {
            return Err(CoreError::DecodeEmptyConflictSection);
        }
        // Every conflict occupies at least 45 bytes (a u64le path length, 1
        // flags byte, a u32le side count, and the one minimum 32-byte side
        // a deletion conflict carries).
        if conflict_count.saturating_mul(45) > r.remaining() {
            return Err(CoreError::DecodeTooLarge);
        }
        conflicts = Vec::with_capacity(conflict_count.min(MAX_ENTRIES));
        for _ in 0..conflict_count {
            let path = r.str_field()?;
            let flags = r.u8()?;
            if flags > 7 {
                return Err(CoreError::DecodeInvalidTag(flags));
            }
            let has_exec = flags & 4 == 4;
            let mut any_exec = false;
            let mut read_exec = |r: &mut Cursor| -> Result<bool, CoreError> {
                if !has_exec {
                    return Ok(false);
                }
                match r.u8()? {
                    0 => Ok(false),
                    1 => {
                        any_exec = true;
                        Ok(true)
                    }
                    other => Err(CoreError::DecodeInvalidTag(other)),
                }
            };
            let (base, base_exec) = if flags & 1 == 1 {
                let id = Id(r.take32()?);
                (Some(id), read_exec(&mut r)?)
            } else {
                (None, false)
            };
            let deleted = flags & 2 == 2;
            let side_count = r.u32_le()? as usize;
            if side_count < if deleted { 1 } else { 2 } {
                return Err(CoreError::DecodeConflictSides);
            }
            let mut sides = Vec::with_capacity(side_count.min(MAX_ENTRIES));
            let mut sides_exec = Vec::with_capacity(side_count.min(MAX_ENTRIES));
            for _ in 0..side_count {
                sides.push(Id(r.take32()?));
                sides_exec.push(read_exec(&mut r)?);
            }
            // Strict canonical acceptance: the exec flag is set exactly
            // when some exec byte is 1 (encode never writes it otherwise).
            if has_exec && !any_exec {
                return Err(CoreError::DecodeInvalidTag(flags));
            }
            conflicts.push(ConflictRep {
                path,
                base,
                base_exec,
                sides,
                sides_exec,
                deleted,
            });
        }
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
        conflicts,
    })
}

/// The object id of a changeset: BLAKE3 over its canonical bytes under the
/// domain its content selects — v1 for a conflict-free changeset, the
/// `naatre\0v2\0` domain when it carries conflict representations.
pub fn changeset_id(cs: &Changeset) -> Result<Id, CoreError> {
    Ok(id_for_encoded(&encode(cs)?, !cs.conflicts.is_empty()))
}

/// The id for already-encoded canonical bytes: the v1 domain for
/// conflict-free changesets, the `naatre\0v2\0` domain when it carries
/// conflict representations (format v2 — the two domains never collide,
/// and a v1 id is never recomputed under v2).
pub fn id_for_encoded(canonical: &[u8], has_conflicts: bool) -> Id {
    if has_conflicts {
        ids::object_id_v2(ids::Kind::Changeset, canonical)
    } else {
        ids::object_id(ids::Kind::Changeset, canonical)
    }
}

/// Validates semantic rules beyond the byte encoding: path grammar on every
/// path (change and conflict paths alike), per-directory case-fold
/// duplicates, the author/message character rule, parent and entry caps,
/// and the conflict-list rules ([`conflict::validate`]).
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
    if cs.changes.is_empty() && cs.conflicts.is_empty() {
        return Err(CoreError::EncodeEmptyChangeset);
    }
    if cs.changes.len() > MAX_ENTRIES {
        return Err(CoreError::EncodeTooManyEntries);
    }
    conflict::validate(&cs.conflicts)?;
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
    use crate::conflict::ConflictRep;

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
            conflicts: vec![],
        }
    }

    fn side(tag: u8) -> Id {
        let mut b = [0u8; 32];
        b[0] = tag;
        Id(b)
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

    /// A conflict-bearing changeset: v2 magic, the conflict section
    /// round-trips, and the id moves to the v2 domain while the identical
    /// logical content without conflicts keeps the v1 domain.
    #[test]
    fn conflicted_roundtrip_under_v2_domain() {
        let mut cs = base_cs();
        cs.parents = vec![Id([9u8; 32])];
        cs.conflicts = vec![ConflictRep {
            path: "a.txt".into(),
            base: Some(side(1)),
            base_exec: false,
            sides: vec![side(2), side(3)],
            sides_exec: vec![false; 2],
            deleted: false,
        }];
        let bytes = encode(&cs).unwrap();
        assert!(bytes.starts_with(MAGIC_V2));
        assert_eq!(decode(&bytes).unwrap(), cs);
        assert_eq!(validate(&cs), Ok(()));
        let id = changeset_id(&cs).unwrap();
        assert_eq!(id, ids::object_id_v2(ids::Kind::Changeset, &bytes));
        assert_ne!(id, ids::object_id(ids::Kind::Changeset, &bytes));
        // Same logical content without the rep: v1 magic and v1 domain.
        let mut plain = cs.clone();
        plain.conflicts = vec![];
        assert!(encode(&plain).unwrap().starts_with(MAGIC_V1));
        assert_eq!(
            changeset_id(&plain).unwrap(),
            ids::object_id(ids::Kind::Changeset, &encode(&plain).unwrap())
        );
    }

    /// A conflict-free changeset encodes to exactly the v1 bytes — the
    /// v1 format is frozen (the golden fixtures pin this byte-for-byte).
    #[test]
    fn conflict_free_encoding_is_v1_unchanged() {
        let cs = base_cs();
        let bytes = encode(&cs).unwrap();
        assert!(bytes.starts_with(MAGIC_V1));
        assert_eq!(decode(&bytes).unwrap(), cs);
    }

    /// Decoder strictness (M12a-fix): encode only picks the v2 magic when
    /// the conflict list is non-empty, so a v2 frame with an empty
    /// conflict section is not a canonical shape — the strict decoder
    /// refuses it instead of accepting a zero-conflict v2 frame.
    #[test]
    fn v2_frame_with_empty_conflict_section_is_a_decode_error() {
        let bytes = encode(&base_cs()).unwrap();
        let mut forged = bytes.clone();
        // The forged frame carries a well-formed (empty) conflict section:
        // count 0 — exactly the shape encode never produces.
        forged.extend_from_slice(&0u32.to_le_bytes());
        forged[16] = 0x02; // the version byte: v1 -> v2, zero conflicts
        assert_eq!(decode(&forged), Err(CoreError::DecodeEmptyConflictSection));
        // The genuine v2 frame (non-empty section) decodes fine.
        let mut cs = base_cs();
        cs.conflicts = vec![ConflictRep {
            path: "a.txt".into(),
            base: Some(side(1)),
            base_exec: false,
            sides: vec![side(2), side(3)],
            sides_exec: vec![false; 2],
            deleted: false,
        }];
        assert!(decode(&encode(&cs).unwrap()).is_ok());
    }

    /// Cap parity (M12a-fix): the conflict list and each rep's sides carry
    /// the same MAX_ENTRIES cap the file-change list does.
    #[test]
    fn conflict_caps_match_the_entries_cap() {
        let side = |t: u8| {
            let mut b = [0u8; 32];
            b[0] = t;
            Id(b)
        };
        // Rep list over the cap.
        let mut cs = base_cs();
        cs.conflicts = (0..=MAX_ENTRIES)
            .map(|i| ConflictRep {
                path: format!("p{i}"),
                base: None,
                base_exec: false,
                sides: vec![side(1), side(2)],
                sides_exec: vec![false; 2],
                deleted: false,
            })
            .collect();
        cs.conflicts
            .sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));
        assert_eq!(validate(&cs), Err(CoreError::EncodeTooManyEntries));
        // Per-rep side count over the cap.
        let mut cs = base_cs();
        cs.conflicts = vec![ConflictRep {
            path: "a.txt".into(),
            base: None,
            base_exec: false,
            sides: (0..=MAX_ENTRIES)
                .map(|i| {
                    let mut id = side(1);
                    id.0[1] = (i % 251) as u8;
                    id.0[2] = (i / 251) as u8;
                    id
                })
                .collect(),
            sides_exec: vec![false; MAX_ENTRIES + 1],
            deleted: false,
        }];
        cs.conflicts[0].sides.sort_by_key(|s| s.0);
        assert_eq!(validate(&cs), Err(CoreError::EncodeTooManyEntries));
        // One over the cap in the OTHER direction still encodes (parity:
        // the cap is MAX_ENTRIES, not anything tighter).
        let mut cs = base_cs();
        cs.conflicts = (0..MAX_ENTRIES)
            .map(|i| ConflictRep {
                path: format!("p{i}"),
                base: None,
                base_exec: false,
                sides: vec![side(1), side(2)],
                sides_exec: vec![false; 2],
                deleted: false,
            })
            .collect();
        cs.conflicts
            .sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));
        assert_eq!(validate(&cs), Ok(()));
    }

    fn exec_cs(base_exec: bool, sides_exec: Vec<bool>) -> Changeset {
        let mut cs = base_cs();
        cs.conflicts = vec![ConflictRep {
            path: "a.txt".into(),
            base: Some(side(1)),
            base_exec,
            sides: vec![side(2), side(3)],
            sides_exec,
            deleted: false,
        }];
        cs
    }

    /// The exec encoding: flags bit 2, an exec byte after the base id and
    /// after each side id; a rep without exec bits keeps the pre-F2 bytes
    /// (flags 1, no exec bytes).
    #[test]
    fn exec_bits_encode_in_the_documented_layout() {
        let plain = encode(&exec_cs(false, vec![false, false])).unwrap();
        let exec = encode(&exec_cs(true, vec![false, true])).unwrap();
        assert_eq!(exec.len(), plain.len() + 3, "base byte + two side bytes");
        // The conflict section ends: flags, base(32)+exec, count, then
        // side(32)+exec per side.
        let tail = &exec[exec.len() - (1 + 32 + 1 + 4 + 2 * 33)..];
        assert_eq!(tail[0], 0b101, "base present + exec bits present");
        assert_eq!(&tail[1..33], &side(1).0);
        assert_eq!(tail[33], 1, "base exec");
        assert_eq!(&tail[34..38], &2u32.to_le_bytes());
        assert_eq!(&tail[38..70], &side(2).0);
        assert_eq!(tail[70], 0);
        assert_eq!(&tail[71..103], &side(3).0);
        assert_eq!(tail[103], 1);
        assert_eq!(decode(&exec).unwrap(), exec_cs(true, vec![false, true]));
        // No exec bit set anywhere: the pre-F2 frame, flags byte 1.
        let tail = &plain[plain.len() - (1 + 32 + 4 + 2 * 32)..];
        assert_eq!(tail[0], 1);
    }

    /// Strict decoding of the exec bytes: only 0 or 1, flags above 7 are
    /// invalid, and the flag cannot be set when no exec byte is 1 (encode
    /// never writes that, so it is not a canonical frame).
    #[test]
    fn decode_rejects_bad_exec_bytes_and_flags() {
        let good = encode(&exec_cs(true, vec![false, true])).unwrap();
        let flags_at = good.len() - (1 + 32 + 1 + 4 + 2 * 33);
        let mut bad = good.clone();
        bad[flags_at + 33] = 2; // base exec byte
        assert_eq!(decode(&bad), Err(CoreError::DecodeInvalidTag(2)));
        let mut bad = good.clone();
        bad[flags_at + 103] = 7; // last side's exec byte
        assert_eq!(decode(&bad), Err(CoreError::DecodeInvalidTag(7)));
        let mut bad = good.clone();
        bad[flags_at] = 8;
        assert_eq!(decode(&bad), Err(CoreError::DecodeInvalidTag(8)));
        // Flag set, every exec byte 0: not canonical.
        let mut bad = good.clone();
        bad[flags_at + 33] = 0;
        bad[flags_at + 103] = 0;
        assert_eq!(decode(&bad), Err(CoreError::DecodeInvalidTag(0b101)));
    }
}
