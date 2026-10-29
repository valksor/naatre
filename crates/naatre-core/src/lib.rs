//! naatre core: object ids, canonical encodings, path grammar, and validation.
//!
//! This crate is pure functions and types only: no I/O, no async, no threads,
//! no global state. Persistence and transport live in other crates.

pub mod changeset;
pub mod error;
pub mod ids;
pub mod path;
pub mod tree;

/// Format version embedded in the object-id domain separator.
///
/// Future format changes move to `"v2"`; `v1` ids are never reused under a
/// different meaning.
pub const FORMAT_VERSION: &str = "v1";
