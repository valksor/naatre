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

/// `--server`/`--token`/`--ca-file`/`--insecure-http` flags shared by the
/// server-talking verbs (the working-copy precedence resolves them against
/// env and config.toml; `ca_file` beats the config's value; the
/// invocation-level plain-http opt-in admits any host for this invocation
/// and combines freely with the per-remote config origin).
#[derive(Debug, Default, Clone)]
pub struct Net {
    pub server: Option<String>,
    pub token: Option<String>,
    /// PEM CA bundle added to the platform roots (private CAs).
    pub ca_file: Option<String>,
    /// The INVOCATION-level plain-http opt-in (`--insecure-http` flag or
    /// `NAATRE_INSECURE_HTTP=1`, folded by `NetOpts::net()`): admitted for
    /// any host for this invocation. The per-remote opt-in lives in the
    /// working-copy config (`insecure_http = "<origin>"`) and is applied
    /// only to its own origin.
    pub insecure_http: bool,
}

/// The invocation-level plain-http opt-in: the `--insecure-http` flag OR
/// `NAATRE_INSECURE_HTTP=1`. Admin verbs have no working copy, so this is
/// their only opt-in; working-copy verbs may combine it with the
/// per-remote config origin. `pub`: the binary's `NetOpts` folds the env
/// form in when it builds the verb [`Net`].
pub fn invocation_optin(flag: bool) -> bool {
    flag || std::env::var("NAATRE_INSECURE_HTTP").is_ok_and(|v| v == "1")
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
            // The cache-route plain-http refusal is the usage class, like
            // every other plain-http refusal (exit 3).
            FetchError::Usage(m) => VerbError::Usage(m),
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

/// `naatre config set <key> <value>` — the working-copy-scoped settings
/// writer (F1). Strict: the only keys are `insecure_http` (the PER-REMOTE
/// opted-in origin `http://host[:port]` — it must parse through the one
/// parser and be plain http; `none` clears) and `ca_file` (a PEM CA
/// bundle path — validated readable with at least one certificate and
/// canonicalized to an absolute path before saving; `none` clears). The
/// never-shipped boolean form (`true`/`false`) is refused like any other
/// unknown value. Unknown keys and invalid values exit 3 (usage). Writes
/// through `save_config` (0600, atomic) so every later verb in this
/// working copy inherits the setting.
pub fn config_set(root: &Path, key: &str, value: &str) -> i32 {
    let wc = match crate::wc::Wc::open(root) {
        Ok(w) => w,
        Err(e) => {
            eprintln!("error: {e}");
            return 1;
        }
    };
    let mut config = match wc.load_config() {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: {e}");
            return 1;
        }
    };
    const KEYS: &str = "the keys are insecure_http (the opted-in origin \
                        \"http://host[:port]\", or none to clear) and ca_file (a PEM \
                        bundle path, or none to clear)";
    match key {
        "insecure_http" => {
            if value == "none" {
                config.insecure_http = None;
            } else {
                let parsed = match crate::url::parse_remote(value) {
                    Ok(p) => p,
                    Err(e) => {
                        eprintln!(
                            "error: invalid value {value:?} for insecure_http: must be the \
                             opted-in origin \"http://host[:port]\" ({e}) ({KEYS})"
                        );
                        return 3;
                    }
                };
                if parsed.scheme != "http" {
                    eprintln!(
                        "error: invalid value {value:?} for insecure_http: must be a \
                         plain-http origin \"http://host[:port]\" ({KEYS})"
                    );
                    return 3;
                }
                config.insecure_http = Some(parsed.origin());
            }
        }
        "ca_file" => {
            if value == "none" {
                config.ca_file = None;
            } else {
                // Validate at save time: readable PEM bundle with at
                // least one certificate, then persist the ABSOLUTE path.
                if let Err(e) = crate::client::read_ca_bundle(value) {
                    eprintln!("error: invalid value {value:?} for ca_file: {e} ({KEYS})");
                    return 3;
                }
                match std::fs::canonicalize(value) {
                    Ok(absolute) => config.ca_file = Some(absolute.to_string_lossy().into_owned()),
                    Err(e) => {
                        eprintln!(
                            "error: invalid value {value:?} for ca_file: cannot resolve an \
                             absolute path: {e}"
                        );
                        return 3;
                    }
                }
            }
        }
        other => {
            eprintln!("error: unknown config key {other:?}: {KEYS}");
            return 3;
        }
    }
    // Print what was STORED (canonicalized paths, the normalized origin,
    // `none` for cleared keys).
    let stored_value = if key == "insecure_http" {
        config
            .insecure_http
            .clone()
            .unwrap_or_else(|| "none".into())
    } else {
        config.ca_file.clone().unwrap_or_else(|| "none".into())
    };
    match wc.save_config(&config) {
        Ok(()) => {
            println!(
                "config: {key} = {stored_value} ({})",
                wc.config_path().display()
            );
            0
        }
        Err(e) => {
            eprintln!("error: {e}");
            1
        }
    }
}
