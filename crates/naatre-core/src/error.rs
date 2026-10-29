//! Crate-wide error type.

use crate::path::PathError;

/// Errors from canonical encoding, strict decoding, and validation.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum CoreError {
    #[error("encode: entries not sorted by strictly ascending path/name bytes")]
    EncodeUnsorted,
    #[error("encode: more than 2 parents")]
    EncodeTooManyParents,
    #[error(
        "encode: Add/Mod entry missing blob, size, or exec; or Del entry carrying blob/size/exec"
    )]
    EncodeMissingBlob,
    #[error("encode: changeset has no entries")]
    EncodeEmptyChangeset,
    #[error("encode: canonical encoding exceeds the 16 MiB cap")]
    EncodeTooLarge,
    #[error("encode: more than 100000 entries")]
    EncodeTooManyEntries,
    #[error("encode: directory entry carries an exec bit")]
    EncodeExecOnDir,
    #[error("decode: bad magic bytes")]
    DecodeBadMagic,
    #[error("decode: trailing bytes after the object")]
    DecodeTrailingBytes,
    #[error("decode: unexpected end of input")]
    DecodeUnexpectedEof,
    #[error("decode: declared length exceeds the remaining input")]
    DecodeTooLarge,
    #[error("decode: invalid tag byte {0:#04x}")]
    DecodeInvalidTag(u8),
    #[error("decode: string field is not valid UTF-8")]
    DecodeInvalidUtf8,
    #[error("decode: length field exceeds the 16 MiB cap")]
    DecodeLengthOverflow,
    #[error(
        "author contains a forbidden control/format character (Cc or Cf) at byte index {index}"
    )]
    AuthorChar { index: usize },
    #[error(
        "message contains a forbidden control/format character (Cc or Cf) at byte index {index}"
    )]
    MessageChar { index: usize },
    #[error("path: {0}")]
    Path(#[from] PathError),
}
