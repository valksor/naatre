//! Path grammar: relative, `/`-separated, NFC-normalized Unicode paths.

use std::collections::HashMap;
use unicode_normalization::UnicodeNormalization;

/// Maximum UTF-8 byte length of a single path component.
pub const MAX_COMPONENT_LEN: usize = 255;
/// Maximum UTF-8 byte length of a whole path.
pub const MAX_PATH_LEN: usize = 4096;
/// Maximum number of `/`-separated components in a whole path (git's
/// 256-leading-directory ceiling). Tree structures recurse per component —
/// derivation, load, and drop all walk the depth — so the grammar bounds
/// the depth the same way it bounds the byte length.
pub const MAX_PATH_COMPONENTS: usize = 256;
/// Reserved first components: the client metadata namespace and git's.
pub const RESERVED_PREFIXES: &[&str] = &[".naatre", ".git"];

/// A rejected path, naming the first rule the path violates.
///
/// Checks run in a fixed order (see [`validate`]); the reported variant is the
/// first failing rule in that order.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum PathError {
    #[error("path is empty")]
    Empty,
    #[error("path is absolute (leading '/')")]
    Absolute,
    #[error("path contains a backslash")]
    Backslash,
    #[error("path contains a NUL byte")]
    NulByte,
    #[error("path contains a control (Cc) or format (Cf) character at byte index {index}")]
    ControlChar { index: usize },
    #[error("path is not in NFC normalization form")]
    NotNormalized,
    #[error("component {index} is \".\" or \"..\"")]
    ComponentInvalid { index: usize },
    #[error("component {index} is {len} bytes long (max 255)")]
    ComponentTooLong { index: usize, len: usize },
    #[error("component {index} is empty (\"//\")")]
    EmptyComponent { index: usize },
    #[error("path has {count} components (max 256)")]
    TooManyComponents { count: usize },
    #[error("path is {len} bytes long (max 4096)")]
    TooLong { len: usize },
    #[error("first component is reserved: {prefix}")]
    Reserved { prefix: &'static str },
    #[error("paths {a:?} and {b:?} share a directory and are equal under case folding")]
    CaseFoldDuplicate { a: String, b: String },
    #[error("component contains '/'")]
    ComponentSlash,
}

/// Validates a whole path against the grammar.
///
/// Rules, in check order: non-empty; relative; no `\`; no NUL; no Unicode
/// category Cc or Cf character anywhere; NFC-normalized; every component
/// not `.`/`..`, at most 255 bytes, and non-empty; at most 256 components;
/// total at most 4096 bytes; first component not reserved
/// ([`.naatre`]/[`.git`]).
pub fn validate(path: &str) -> Result<(), PathError> {
    if path.is_empty() {
        return Err(PathError::Empty);
    }
    if path.starts_with('/') {
        return Err(PathError::Absolute);
    }
    if path.contains('\\') {
        return Err(PathError::Backslash);
    }
    if path.contains('\0') {
        return Err(PathError::NulByte);
    }
    for (index, c) in path.char_indices() {
        if is_cc(c) || is_cf(c) {
            return Err(PathError::ControlChar { index });
        }
    }
    if path.nfc().collect::<String>() != path {
        return Err(PathError::NotNormalized);
    }
    let mut components = 0usize;
    for (index, component) in path.split('/').enumerate() {
        if component == "." || component == ".." {
            return Err(PathError::ComponentInvalid { index });
        }
        if component.len() > MAX_COMPONENT_LEN {
            return Err(PathError::ComponentTooLong {
                index,
                len: component.len(),
            });
        }
        if component.is_empty() {
            return Err(PathError::EmptyComponent { index });
        }
        components += 1;
    }
    if components > MAX_PATH_COMPONENTS {
        return Err(PathError::TooManyComponents { count: components });
    }
    if path.len() > MAX_PATH_LEN {
        return Err(PathError::TooLong { len: path.len() });
    }
    let first = path.split('/').next().unwrap_or(path);
    if let Some(prefix) = RESERVED_PREFIXES.iter().find(|p| **p == first) {
        return Err(PathError::Reserved { prefix });
    }
    Ok(())
}

/// Validates a single path component (a name with no `/`).
///
/// Applies the character rules of [`validate`] (non-empty, no `/`, no `\`,
/// no NUL, no Cc/Cf, NFC, not `.`/`..`, at most 255 bytes) without the
/// whole-path checks (total length, reserved first component).
pub fn validate_component(name: &str) -> Result<(), PathError> {
    if name.is_empty() {
        return Err(PathError::Empty);
    }
    if name.contains('/') {
        return Err(PathError::ComponentSlash);
    }
    if name.contains('\\') {
        return Err(PathError::Backslash);
    }
    if name.contains('\0') {
        return Err(PathError::NulByte);
    }
    for (index, c) in name.char_indices() {
        if is_cc(c) || is_cf(c) {
            return Err(PathError::ControlChar { index });
        }
    }
    if name.nfc().collect::<String>() != name {
        return Err(PathError::NotNormalized);
    }
    if name == "." || name == ".." {
        return Err(PathError::ComponentInvalid { index: 0 });
    }
    if name.len() > MAX_COMPONENT_LEN {
        return Err(PathError::ComponentTooLong {
            index: 0,
            len: name.len(),
        });
    }
    Ok(())
}

/// Detects two entries in the same directory whose final names are equal
/// under `to_lowercase()`.
///
/// Comparison is per-directory only: names in different directories (or one
/// at root and one nested) may fold-equal without error. Errors name the two
/// colliding paths in input order.
pub fn check_case_fold_duplicates(paths: &[String]) -> Result<(), PathError> {
    let mut seen: HashMap<(&str, String), &str> = HashMap::with_capacity(paths.len());
    for path in paths {
        let (parent, name) = match path.rsplit_once('/') {
            Some((dir, name)) => (dir, name),
            None => ("", path.as_str()),
        };
        let key = (parent, name.to_lowercase());
        if let Some(prev) = seen.insert(key, path) {
            return Err(PathError::CaseFoldDuplicate {
                a: prev.to_string(),
                b: path.clone(),
            });
        }
    }
    Ok(())
}

/// Unicode general category Cc (control characters).
pub(crate) fn is_cc(c: char) -> bool {
    c.is_control()
}

/// Unicode general category Cf (format characters), Unicode 16.0 set.
pub(crate) fn is_cf(c: char) -> bool {
    matches!(c as u32,
        0x00AD
        | 0x0600..=0x0605
        | 0x061C
        | 0x06DD
        | 0x070F
        | 0x0890..=0x0891
        | 0x08E2
        | 0x180E
        | 0x200B..=0x200F
        | 0x202A..=0x202E
        | 0x2060..=0x2064
        | 0x2066..=0x206F
        | 0xFEFF
        | 0xFFF9..=0xFFFB
        | 0x110BD
        | 0x110CD
        | 0x13430..=0x1343F
        | 0x1BCA0..=0x1BCA3
        | 0x1D173..=0x1D17A
        | 0xE0001
        | 0xE0020..=0xE007F
    )
}
