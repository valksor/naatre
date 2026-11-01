//! naatre core: object ids, canonical encodings, path grammar, and validation.
//!
//! This crate is pure functions and types only: no I/O, no async, no threads,
//! no global state. Persistence and transport live in other crates.

pub mod changeset;
pub mod conflict;
pub mod error;
pub mod ids;
pub mod path;
pub mod tree;

/// Format version embedded in the object-id domain separator.
///
/// `v1` ids are never reused under a different meaning.
pub const FORMAT_VERSION: &str = "v1";

/// Format version for the one v2 payload (M12): changesets that carry
/// first-class conflict representations hash under the `naatre\0v2\0`
/// domain and encode with the v2 canonical magic. Plain (conflict-free)
/// changesets keep hashing under [`FORMAT_VERSION`] — v1 ids are never
/// recomputed, so existing history is untouched.
pub const FORMAT_VERSION_V2: &str = "v2";
