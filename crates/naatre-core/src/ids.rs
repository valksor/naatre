//! Content-addressed object ids.
//!
//! An id is `BLAKE3("naatre\0" + FORMAT_VERSION + "\0" + kind_label + "\0" +
//! payload_len_le64 + "\0" + payload)` rendered as lowercase hex. The version
//! inside the domain separator means a future format change takes new ids
//! instead of silently reusing `v1` ids.

use blake3::Hasher;
use std::fmt;

const KIND_LABEL_BLOB: &[u8] = b"blob";
const KIND_LABEL_TREE: &[u8] = b"tree";
const KIND_LABEL_CHANGESET: &[u8] = b"changeset";

const HEX_DIGITS: &[u8; 16] = b"0123456789abcdef";

/// Object kind, mixed into the id domain separator.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Kind {
    /// File content.
    Blob,
    /// Directory listing.
    Tree,
    /// Commit-like change record.
    Changeset,
}

impl Kind {
    const fn label(self) -> &'static [u8] {
        match self {
            Kind::Blob => KIND_LABEL_BLOB,
            Kind::Tree => KIND_LABEL_TREE,
            Kind::Changeset => KIND_LABEL_CHANGESET,
        }
    }
}

/// A 32-byte object id.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
pub struct Id(pub [u8; 32]);

impl Id {
    /// Lowercase hex rendering, always 64 characters.
    pub fn to_hex(&self) -> String {
        let mut s = String::with_capacity(64);
        for b in &self.0 {
            s.push(HEX_DIGITS[(b >> 4) as usize] as char);
            s.push(HEX_DIGITS[(b & 0x0f) as usize] as char);
        }
        s
    }

    /// Parses exactly 64 lowercase hex characters; `None` otherwise.
    pub fn from_hex(s: &str) -> Option<Id> {
        if s.len() != 64 {
            return None;
        }
        let bytes = s.as_bytes();
        let mut out = [0u8; 32];
        for i in 0..32 {
            out[i] = (hex_val(bytes[2 * i])? << 4) | hex_val(bytes[2 * i + 1])?;
        }
        Some(Id(out))
    }
}

/// True when `s` is exactly 64 lowercase hex characters.
pub fn is_hex_id(s: &str) -> bool {
    Id::from_hex(s).is_some()
}

/// Computes the id of a payload under the versioned, kind-tagged domain.
pub fn object_id(kind: Kind, payload: &[u8]) -> Id {
    object_id_in(crate::FORMAT_VERSION, kind, payload)
}

/// Computes the id of a payload under the `naatre\0v2\0` domain. Used only
/// for changesets that carry conflict representations (format v2); a v1 id
/// is never recomputed under it.
pub fn object_id_v2(kind: Kind, payload: &[u8]) -> Id {
    object_id_in(crate::FORMAT_VERSION_V2, kind, payload)
}

fn object_id_in(version: &str, kind: Kind, payload: &[u8]) -> Id {
    let mut h = Hasher::new();
    h.update(b"naatre\x00");
    h.update(version.as_bytes());
    h.update(b"\x00");
    h.update(kind.label());
    h.update(b"\x00");
    h.update(&(payload.len() as u64).to_le_bytes());
    h.update(b"\x00");
    h.update(payload);
    Id(*h.finalize().as_bytes())
}

fn hex_val(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        _ => None,
    }
}

impl fmt::Display for Id {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.to_hex())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Pins the id domain: any accidental change to the separator, kind
    /// labels, length framing, or hash breaks this.
    #[test]
    fn empty_blob_id_is_pinned() {
        let expected = object_id(Kind::Blob, b"");
        assert_eq!(
            expected.to_hex(),
            "9ecab71b0beadc307272926dd80527b866f73767e3e0f7cbd37ea26ffe17b250",
        );
    }

    #[test]
    fn hex_roundtrip() {
        let id = object_id(Kind::Tree, b"x");
        assert_eq!(Id::from_hex(&id.to_hex()), Some(id));
        assert!(is_hex_id(&id.to_hex()));
        assert!(!is_hex_id(&id.to_hex().to_uppercase()));
        assert!(!is_hex_id(&id.to_hex()[..63]));
        assert!(!is_hex_id("g0"));
    }
}
