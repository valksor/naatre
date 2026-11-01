//! The working-copy verbs: clone, status, sparse, sync, commit, cat, ls,
//! diff, doctor, login, snapshot, restore, mcp — plus the mount daemon
//! (M8), which serves a pinned ref over NFS from a repo name + server
//! (no working copy). Every verb prints its own output and returns the
//! pinned exit code (0 success, 1 generic, 2 conflict-class, 3 usage).
//! Each also exposes a value-returning core (`value`/`*_value`/`core`)
//! the `mcp` server drives — the cores never print, so MCP tool results
//! can carry the verbs' `--json` shapes while stdout stays the MCP
//! protocol channel.

pub mod clone;
pub mod commit;
pub mod doctor;
// The Linux-only FUSE front-end for `naatre mount --fuse` (M8b).
#[cfg(target_os = "linux")]
pub mod fuse;
pub mod land;
pub mod login;
pub mod mcp;
pub mod mount;
pub mod reads;
pub mod resolve;
pub mod restore;
pub mod snapshot;
pub mod sparse;
pub mod status;
pub mod sync;

use std::path::Path;

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

/// The ONE source of the `conflict_markers_present` envelope text (M12b):
/// both commit arms build it here, so the e2e's asserted message has a
/// single home. Exit 2, the conflict class.
pub(crate) fn marker_gate_envelope(path: &str) -> VerbError {
    VerbError::Http(HttpError::Envelope {
        status: 409,
        code: "conflict_markers_present".into(),
        message: format!(
            "{path} contains conflict markers (a \">>>>>>> naatre conflict:\" \
             file rendered from a recorded conflict); committing it would store \
             the conflict as anonymous content — resolve it instead: edit the \
             resolved content, then run `naatre resolve {path}`"
        ),
        details: None,
        retry_after: None,
    })
}

/// Reads a working-copy file and derives the (blob, size, exec, bytes)
/// pending-file triplet — commit's and resolve's shared read (the id is
/// the address, computed client-side).
pub(crate) fn read_wc_file(
    root: &Path,
    raw: &str,
) -> Result<(naatre_core::ids::Id, u64, bool, Vec<u8>), VerbError> {
    use std::os::unix::fs::MetadataExt as _;
    let bytes = std::fs::read(root.join(raw))
        .map_err(|e| VerbError::Local(format!("reading {raw:?}: {e}")))?;
    let meta = std::fs::metadata(root.join(raw))
        .map_err(|e| VerbError::Local(format!("stating {raw:?}: {e}")))?;
    let exec = meta.mode() & 0o111 != 0;
    Ok((
        naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, &bytes),
        bytes.len() as u64,
        exec,
        bytes,
    ))
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
