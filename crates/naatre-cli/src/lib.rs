//! naatre CLI library: everything the `naatre` binary dispatches to.
//!
//! - [`host`]: host-local admin verbs (`init`, `serve`, `token --new-admin`,
//!   `import`) that run directly against the 0700 data dir.
//! - [`client`]: the thin HTTP layer (bearer auth, error envelope, version
//!   skew warning) shared by all client verbs.
//! - [`wc`]: the client working copy — `.naatre/` layout, the binary stat
//!   index, the disk scanner, and server tree/blob fetchers.
//! - [`verbs`]: the working-copy verbs (clone, status, sparse, sync, commit,
//!   cat, ls, diff, doctor, login).

pub mod client;
pub mod host;
pub mod verbs;
pub mod wc;
