//! The working-copy verbs: clone, status, sparse, sync, commit, cat, ls,
//! diff, doctor, login, snapshot, restore. Every verb prints its own output
//! and returns the pinned exit code (0 success, 1 generic, 2 conflict-class,
//! 3 usage).

pub mod clone;
pub mod commit;
pub mod doctor;
pub mod land;
pub mod login;
pub mod reads;
pub mod restore;
pub mod snapshot;
pub mod sparse;
pub mod status;
pub mod sync;

use crate::client::HttpError;
use crate::wc::fetch::FetchError;

/// `--server`/`--token` flags shared by the server-talking verbs (the
/// working-copy precedence resolves them against env and config.toml).
#[derive(Debug, Default, Clone)]
pub struct Net {
    pub server: Option<String>,
    pub token: Option<String>,
}

/// A verb failure with its pinned exit code.
#[derive(Debug)]
pub enum VerbError {
    /// The server answered with an error envelope, or transport failed.
    Http(HttpError),
    /// A local error (working copy, untrusted data, io) — always exit 1.
    Local(String),
    /// Bad invocation — exit 3.
    Usage(String),
}

impl From<HttpError> for VerbError {
    fn from(e: HttpError) -> Self {
        VerbError::Http(e)
    }
}

impl From<FetchError> for VerbError {
    fn from(e: FetchError) -> Self {
        match e {
            FetchError::Http(e) => VerbError::Http(e),
            FetchError::Local(m) => VerbError::Local(m),
        }
    }
}

impl From<String> for VerbError {
    fn from(m: String) -> Self {
        VerbError::Local(m)
    }
}

impl std::fmt::Display for VerbError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            VerbError::Http(e) => write!(f, "{e}"),
            VerbError::Local(m) => f.write_str(m),
            VerbError::Usage(m) => f.write_str(m),
        }
    }
}

impl VerbError {
    /// Prints `error: …` and returns the pinned exit code.
    pub fn report(self) -> i32 {
        eprintln!("error: {}", self);
        match self {
            VerbError::Http(e) => e.exit_code(),
            VerbError::Local(_) => 1,
            VerbError::Usage(_) => 3,
        }
    }
}
