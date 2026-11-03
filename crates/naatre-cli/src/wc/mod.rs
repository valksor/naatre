//! The client working copy: the `.naatre/` directory inside a checkout.
//!
//! Layout (plan "Client working copy"):
//! - `config.toml` — `server_url` + optional `token`, plus the F1
//!   TLS-posture keys (`insecure_http`, `ca_file`), written 0600 by
//!   `clone`, `login`, and `config set`. Precedence for server settings:
//!   `--server`/`--token` flags > `NAATRE_URL`/`NAATRE_TOKEN` > this file.
//! - `state.json` — head changeset, base (null until land, M4), the sparse
//!   prefix list (empty = full checkout), and the server repo name.
//! - `index.nai` — the binary stat index (see [`index`]).
//!
//! Single-writer per working copy: since M7, [`lock`] serializes concurrent
//! use (an `naatre mcp` session holds `.naatre/wc.lock` for its lifetime;
//! CLI verbs take it per invocation). M8 mount clients get their own
//! coordination on top.

pub mod fetch;
pub mod index;
pub mod lock;
pub mod scan;

use std::fs::OpenOptions;
use std::io::Write as _;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use serde::{Deserialize, Serialize};

use crate::client::Client;

pub const NAATRE_DIR: &str = ".naatre";
pub const CONFIG_FILE: &str = "config.toml";
pub const STATE_FILE: &str = "state.json";
pub const INDEX_FILE: &str = "index.nai";

/// The resolved remote settings: (server, token, per-remote insecure_http
/// origin, ca_file) — [`Wc::resolve_server`]'s shape.
pub type ResolvedRemote = (String, Option<String>, Option<String>, Option<String>);

/// Reserved first components: never scanned, never committed.
pub const RESERVED: &[&str] = &[".naatre", ".git"];

/// Unique temp-name counter (per process) for atomic writes.
static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// `.naatre/config.toml`. Four known keys: `server_url` (required once a
/// clone wrote the file), optional `token`, and the F1 TLS-posture keys —
/// `insecure_http` (the PER-REMOTE plain-http opt-in: the opted-in ORIGIN
/// `http://host[:port]`, absent = nothing opted in; a non-loopback plain
/// http URL is admitted only when the resolved server's origin equals it)
/// and `ca_file` (an absolute PEM CA bundle path added to the platform
/// roots). Parsing is strict: any other key or malformed line is an error
/// (never merged into defaults).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Config {
    pub server_url: String,
    pub token: Option<String>,
    /// The opted-in origin (written only when set). The old boolean form
    /// never shipped and is refused by the strict parser.
    pub insecure_http: Option<String>,
    pub ca_file: Option<String>,
}

/// `.naatre/state.json`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct State {
    /// Current changeset id (64 lowercase hex).
    pub head: String,
    /// Land base once land exists (M4); null until then. Carried for
    /// state.json compatibility: no code reads it for a decision, and a
    /// no-op fast-forward land (head == landed) intentionally writes no
    /// state at all, so it is not refreshed there either.
    pub base: Option<String>,
    /// Sparse prefix list; empty = the full tree is checked out.
    pub sparse: Vec<String>,
    /// Repo name on the server.
    pub server_repo: String,
    /// M12b: the paths of head's tree that materialize conflict markers
    /// (the conflicted paths). Recorded by every materializing verb —
    /// sync, clone, land (the record-conflict settlement's tree), sparse
    /// add, restore — based on the marker-shaped bytes each wrote;
    /// cleared by resolve and by a commit that rewrites the path (and by
    /// sparse remove pruning the removed subtree). The list is a STATUS
    /// INDEX, not a permission: `status` marks them (`C <path>`), and
    /// `resolve` keys on the marker-shaped content itself, the mark
    /// notwithstanding. `default`: state files written before M12b carry
    /// no key.
    #[serde(default)]
    pub conflicted: Vec<String>,
}

/// A handle on a working copy rooted at `root` (the checkout directory).
pub struct Wc {
    pub root: PathBuf,
}

impl Wc {
    /// Opens the working copy at `root`; errors when `.naatre/` is absent.
    pub fn open(root: &Path) -> Result<Wc, String> {
        if !root.join(NAATRE_DIR).is_dir() {
            return Err(format!(
                "not a naatre working copy (no {NAATRE_DIR}/ under {}); run naatre clone first",
                root.display()
            ));
        }
        Ok(Wc {
            root: root.to_path_buf(),
        })
    }

    pub fn naatre_dir(&self) -> PathBuf {
        self.root.join(NAATRE_DIR)
    }

    pub fn config_path(&self) -> PathBuf {
        self.naatre_dir().join(CONFIG_FILE)
    }

    pub fn state_path(&self) -> PathBuf {
        self.naatre_dir().join(STATE_FILE)
    }

    pub fn index_path(&self) -> PathBuf {
        self.naatre_dir().join(INDEX_FILE)
    }

    /// Reads `config.toml`. A missing file is an empty config (the server
    /// then comes from flags/env only).
    pub fn load_config(&self) -> Result<Config, String> {
        let bytes = match std::fs::read(self.config_path()) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Config::default()),
            Err(e) => return Err(format!("reading {}: {e}", self.config_path().display())),
        };
        parse_config(&bytes)
            .map_err(|e| format!("{} is malformed: {e}", self.config_path().display()))
    }

    /// Writes `config.toml` atomically, mode 0600 (it holds the token).
    pub fn save_config(&self, config: &Config) -> Result<(), String> {
        let mut body = String::new();
        body.push_str(&format!(
            "server_url = \"{}\"\n",
            escape_toml(&config.server_url)
        ));
        if let Some(token) = &config.token {
            body.push_str(&format!("token = \"{}\"\n", escape_toml(token)));
        }
        // The TLS-posture keys are written only when set, so configs from
        // before F1 (and opt-outs) stay byte-identical to the old shape.
        if let Some(origin) = &config.insecure_http {
            body.push_str(&format!("insecure_http = \"{}\"\n", escape_toml(origin)));
        }
        if let Some(ca_file) = &config.ca_file {
            body.push_str(&format!("ca_file = \"{}\"\n", escape_toml(ca_file)));
        }
        write_atomic(&self.config_path(), body.as_bytes(), true)
            .map_err(|e| format!("writing {}: {e}", self.config_path().display()))
    }

    pub fn load_state(&self) -> Result<State, String> {
        let bytes = std::fs::read(self.state_path())
            .map_err(|e| format!("reading {}: {e}", self.state_path().display()))?;
        serde_json::from_slice(&bytes)
            .map_err(|e| format!("{} is malformed: {e}", self.state_path().display()))
    }

    pub fn save_state(&self, state: &State) -> Result<(), String> {
        let bytes = serde_json::to_vec_pretty(state).map_err(|e| format!("encoding state: {e}"))?;
        write_atomic(&self.state_path(), &bytes, true)
            .map_err(|e| format!("writing {}: {e}", self.state_path().display()))
    }

    /// Loads the stat index; a missing file is an empty index, a corrupt
    /// one is a hard error (recovery is `naatre doctor --rebuild`).
    pub fn load_index(&self) -> Result<index::Index, String> {
        match index::Index::load(&self.index_path()) {
            Ok(idx) => Ok(idx),
            Err(index::IndexError::Missing) => Ok(index::Index::default()),
            Err(e) => Err(format!(
                "{}: {e}; run naatre doctor --rebuild",
                self.index_path().display()
            )),
        }
    }

    pub fn save_index(&self, idx: &index::Index) -> Result<(), String> {
        index::Index::save(idx, &self.index_path())
            .map_err(|e| format!("writing {}: {e}", self.index_path().display()))
    }

    /// Resolves the remote settings for a verb, in the pinned precedence:
    /// `--server`/`--token` flags, then `NAATRE_URL`/`NAATRE_TOKEN` env,
    /// then `config.toml`. Returns (server, token, per-remote insecure_http
    /// origin, ca_file) — the TLS-posture pair comes from the config only
    /// (the flags ride [`crate::verbs::Net`], which take precedence over
    /// both at the build site).
    pub fn resolve_server(
        &self,
        flag_server: Option<&str>,
        flag_token: Option<&str>,
    ) -> Result<ResolvedRemote, String> {
        let config = self.load_config()?;
        let server = flag_server
            .map(str::to_string)
            .or_else(|| env_var("NAATRE_URL"))
            .unwrap_or(config.server_url);
        if server.is_empty() {
            return Err("no server configured (use --server, NAATRE_URL, or naatre login)".into());
        }
        let token = flag_token
            .map(str::to_string)
            .or_else(|| env_var("NAATRE_TOKEN"))
            .or(config.token);
        Ok((server, token, config.insecure_http, config.ca_file))
    }

    /// A ready HTTP client for this working copy's remote — THE
    /// enforcement point for the F1 plain-http rule and the `ca_file`
    /// precedence (`--ca-file` flag > `NAATRE_CA_FILE` env > this
    /// config's value): every working-copy verb (and the mount client's
    /// working-copy arm) builds its client here, so the check and the
    /// bundle live in one place. The plain-http refusal is the usage
    /// class; the CLI pre-flight ([`check_remote_policy`]) routes it to
    /// exit 3 before the verb's own reporting (exit 1) can run.
    pub fn client(&self, net: &crate::verbs::Net) -> Result<Client, String> {
        let (server, token, insecure_origin, config_ca_file) =
            self.resolve_server(net.server.as_deref(), net.token.as_deref())?;
        let parsed = crate::url::parse_remote(&server).map_err(|e| format!("{server:?}: {e}"))?;
        admit_remote(&parsed, net.insecure_http, insecure_origin.as_deref())?;
        let ca_file = net
            .ca_file
            .clone()
            .or_else(|| env_var("NAATRE_CA_FILE"))
            .or(config_ca_file);
        let origin_key = match insecure_origin.as_deref() {
            None => None,
            // The stored origin was validated at save time; a malformed
            // one on disk is still a refusal, never a silent admit.
            Some(origin) => Some(crate::url::parse_remote(origin)?),
        };
        Ok(
            Client::new_with_ca(&parsed.origin(), token, ca_file.as_deref())?
                .with_insecure_origin(origin_key.as_ref().map(crate::url::ParsedRemote::key)),
        )
    }
}

/// The per-remote admission behind the F1 plain-http rule: `https` is
/// always allowed; plain `http` is allowed to LOOPBACK hosts only, and to
/// a non-loopback host only when the INVOCATION opted in (`--insecure-http`
/// / `NAATRE_INSECURE_HTTP=1` — any host, for that invocation) or the
/// working copy's stored per-remote origin EQUALS this origin (compared
/// through the one parser's origin key, and stored only for plain http).
/// The refusal names every opt-in and the https alternative — it never
/// silently proceeds (a cache-route refusal is a hard error for the
/// invocation, exactly like the clone-time one).
pub(crate) fn admit_remote(
    parsed: &crate::url::ParsedRemote,
    invocation_optin: bool,
    stored: Option<&str>,
) -> Result<(), String> {
    if parsed.scheme == "https" || parsed.is_loopback() || invocation_optin {
        return Ok(());
    }
    if let Some(origin) = stored
        && let Ok(stored_parsed) = crate::url::parse_remote(origin)
        && stored_parsed.scheme == "http"
        && stored_parsed.key() == parsed.key()
    {
        return Ok(());
    }
    Err(format!(
        "refusing plain http:// to the non-loopback host {}: use https://, or opt in for \
         this invocation with --insecure-http (or NAATRE_INSECURE_HTTP=1), or for this \
         origin in this working copy with `naatre config set insecure_http \"{}\"`",
        parsed.host,
        parsed.origin()
    ))
}

/// The F1 plain-http rule over a URL STRING: parse ONCE with the one
/// parser ([`crate::url::parse_remote`] — the parser that connects), then
/// [`admit_remote`]. Every client path goes through this or the parsed
/// form; a URL that fails the parse is refused, never guessed.
pub(crate) fn ensure_scheme_allowed(
    base: &str,
    invocation_optin: bool,
    stored: Option<&str>,
) -> Result<(), String> {
    let parsed = crate::url::parse_remote(base)?;
    admit_remote(&parsed, invocation_optin, stored)
}

/// The CLI pre-flight (exit-3 routing): resolves the effective server
/// exactly as the verb's own client build will and runs the SAME
/// admission, so the plain-http refusal reaches the user as the usage
/// class (exit 3) even though the verb's own [`Wc::client`] build reports
/// through the generic error class. Any OTHER configuration problem (no
/// working copy, malformed config, no server configured) is left to the
/// verb's own reporting — this pre-flight refuses ONLY the plain-http
/// policy.
pub fn check_remote_policy(root: &Path, net: &crate::verbs::Net) -> Result<(), String> {
    let (server, stored) = match Wc::open(root) {
        Ok(wc) => match wc.load_config() {
            Ok(config) => {
                let server = net
                    .server
                    .clone()
                    .or_else(|| env_var("NAATRE_URL"))
                    .unwrap_or(config.server_url);
                (server, config.insecure_http)
            }
            Err(_) => return Ok(()),
        },
        // Outside a working copy (`naatre mount` takes a repo name): the
        // resolved flag/env/default base is what the client will build on.
        Err(_) => (
            net.server
                .clone()
                .or_else(|| env_var("NAATRE_URL"))
                .unwrap_or_else(|| crate::client::DEFAULT_URL.to_string()),
            None,
        ),
    };
    if server.is_empty() {
        return Ok(());
    }
    ensure_scheme_allowed(&server, net.insecure_http, stored.as_deref())
}

fn env_var(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.is_empty())
}

/// Strict `key = "value"` parser for the four-key config; unknown keys and
/// malformed lines are errors (read-modify-write never merges into defaults).
fn parse_config(bytes: &[u8]) -> Result<Config, String> {
    let text = std::str::from_utf8(bytes).map_err(|_| "not UTF-8".to_string())?;
    let mut config = Config::default();
    for (lineno, line) in text.lines().enumerate() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let bad = || format!("line {}: expected key = \"value\"", lineno + 1);
        let (key, value) = line.split_once('=').ok_or_else(bad)?;
        let key = key.trim();
        let value = value.trim();
        let Some(value) = value.strip_prefix('"') else {
            return Err(bad());
        };
        let Some(value) = value.strip_suffix('"') else {
            return Err(bad());
        };
        match key {
            "server_url" => config.server_url = unescape(value),
            "token" => config.token = Some(unescape(value)),
            "insecure_http" => {
                // The PER-REMOTE form: the opted-in ORIGIN, plain http
                // only, parsed by the one parser. The old boolean form
                // never shipped in a release — refused like any other
                // unknown value.
                let parsed = crate::url::parse_remote(value).map_err(|e| {
                    format!(
                        "line {}: insecure_http must be the opted-in origin \
                         \"http://host[:port]\" (got {value:?}: {e})",
                        lineno + 1
                    )
                })?;
                if parsed.scheme != "http" {
                    return Err(format!(
                        "line {}: insecure_http must be a plain-http origin \
                         \"http://host[:port]\" (got {value:?})",
                        lineno + 1
                    ));
                }
                config.insecure_http = Some(parsed.origin());
            }
            "ca_file" => config.ca_file = Some(unescape(value)),
            other => return Err(format!("line {}: unknown key {other:?}", lineno + 1)),
        }
    }
    Ok(config)
}

/// Escapes the two characters that would break the one-line format.
fn escape_toml(value: &str) -> String {
    value.replace('\\', "\\\\").replace('"', "\\\"")
}

fn unescape(value: &str) -> String {
    let mut out = String::with_capacity(value.len());
    let mut chars = value.chars();
    while let Some(c) = chars.next() {
        if c == '\\' {
            match chars.next() {
                Some('"') => out.push('"'),
                Some('\\') => out.push('\\'),
                Some(other) => {
                    out.push('\\');
                    out.push(other);
                }
                None => out.push('\\'),
            }
        } else {
            out.push(c);
        }
    }
    out
}

/// Writes `bytes` to `path` through a unique temp sibling + rename (the
/// pinned materialization rule: interrupted runs leave no half-written
/// file). Creates parent directories; `private` writes mode 0600.
pub fn write_atomic(path: &Path, bytes: &[u8], private: bool) -> std::io::Result<()> {
    let parent = path
        .parent()
        .ok_or_else(|| std::io::Error::other("path has no parent"))?;
    std::fs::create_dir_all(parent)?;
    let tmp = parent.join(format!(
        ".{}.naatre-tmp-{}-{}",
        path.file_name()
            .map_or_else(|| "file".to_string(), |n| n.to_string_lossy().into_owned()),
        std::process::id(),
        TEMP_COUNTER.fetch_add(1, Ordering::Relaxed),
    ));
    let write = || -> std::io::Result<()> {
        let mode = if private { 0o600 } else { 0o644 };
        let mut f = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(mode)
            .open(&tmp)?;
        f.write_all(bytes)?;
        f.sync_all()?;
        std::fs::rename(&tmp, path)?;
        Ok(())
    };
    match write() {
        Ok(()) => Ok(()),
        Err(e) => {
            let _ = std::fs::remove_file(&tmp);
            Err(e)
        }
    }
}

/// Writes a checkout file (relative NFC path under `root`) atomically and
/// returns its stat index row.
pub fn write_checkout_file(
    root: &Path,
    rel: &str,
    bytes: &[u8],
    blob: [u8; 32],
    exec: bool,
) -> std::io::Result<index::Row> {
    let path = root.join(rel);
    write_atomic(&path, bytes, false)?;
    if exec {
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755))?;
    }
    let meta = std::fs::metadata(&path)?;
    Ok(index::Row::from_stat(rel.to_string(), blob, &meta))
}

/// Removes a file and prunes now-empty parent directories up to `root`
/// (best effort; a non-empty dir simply stays).
pub fn remove_and_prune(root: &Path, rel: &str) -> std::io::Result<()> {
    let path = root.join(rel);
    match std::fs::remove_file(&path) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    }
    let mut dir = path.parent();
    while let Some(d) = dir {
        if d == root {
            break;
        }
        // An empty dir removes; anything else stops the prune.
        if std::fs::remove_dir(d).is_ok() {
            dir = d.parent();
        } else {
            break;
        }
    }
    Ok(())
}

/// True when `path` is materialized under the sparse `prefixes` (an empty
/// list = the full tree). A prefix matches itself and everything under it.
pub fn under_sparse(prefixes: &[String], path: &str) -> bool {
    prefixes.is_empty() || prefixes.iter().any(|p| under_prefix(p, path))
}

/// Single-prefix form of [`under_sparse`].
pub fn under_prefix(prefix: &str, path: &str) -> bool {
    path == prefix
        || path
            .strip_prefix(prefix)
            .is_some_and(|rest| rest.starts_with('/'))
}

/// Normalizes to NFC (the index and every compared path are NFC).
pub fn nfc(s: &str) -> String {
    use unicode_normalization::UnicodeNormalization as _;
    s.nfc().collect()
}

/// The default commit author: `NAATRE_AUTHOR`, else `user@host` from the
/// environment, else the pinned fallback string. A derived identity prints
/// one stderr notice (git-style); the commit still succeeds.
pub fn default_author() -> String {
    if let Some(a) = env_var("NAATRE_AUTHOR") {
        return a;
    }
    let user = env_var("USER").or_else(|| env_var("LOGNAME"));
    let host = env_var("HOSTNAME").or_else(|| env_var("HOST"));
    let author = match (user, host) {
        (Some(u), Some(h)) => format!("{u}@{h}"),
        _ => "unknown <unknown@naatre.local>".to_string(),
    };
    eprintln!("identity not configured; using {author} — set NAATRE_AUTHOR to override");
    author
}

/// System clock in nanoseconds since the epoch (commit timestamps).
pub fn now_ns() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(0)
}

/// Directory staged beside the destination and renamed into place at commit
/// time (the clone atomicity rule). Dropped unstaged → removed.
pub struct StageDir {
    path: PathBuf,
    done: bool,
}

impl StageDir {
    /// Creates `.name.naatre-clone-<pid>` beside `dest`.
    pub fn beside(dest: &Path) -> std::io::Result<StageDir> {
        let parent = dest
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or_else(|| Path::new("."));
        let path = parent.join(format!(
            ".{}.naatre-clone-{}",
            dest.file_name()
                .map_or_else(|| "repo".to_string(), |n| n.to_string_lossy().into_owned()),
            std::process::id(),
        ));
        let _ = std::fs::remove_dir_all(&path); // stale sibling from a killed run
        std::fs::create_dir(&path)?;
        std::fs::create_dir_all(path.join(NAATRE_DIR))?;
        // The working copy is private to its owner, like the data dir.
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o700))?;
        std::fs::set_permissions(
            path.join(NAATRE_DIR),
            std::fs::Permissions::from_mode(0o700),
        )?;
        Ok(StageDir { path, done: false })
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Renames the staging dir onto `dest`; fails cleanly when it exists.
    pub fn commit(mut self, dest: &Path) -> std::io::Result<()> {
        if dest.symlink_metadata().is_ok() {
            return Err(std::io::Error::new(
                std::io::ErrorKind::AlreadyExists,
                format!("destination {} already exists", dest.display()),
            ));
        }
        std::fs::rename(&self.path, dest)?;
        self.done = true;
        Ok(())
    }
}

impl Drop for StageDir {
    fn drop(&mut self) {
        if !self.done {
            let _ = std::fs::remove_dir_all(&self.path);
        }
    }
}

/// True when `rel` sits under a reserved first component (never scanned,
/// never committed).
pub fn is_reserved(rel: &str) -> bool {
    let first = rel.split('/').next().unwrap_or("");
    RESERVED.contains(&first)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_round_trips_the_new_tls_keys() {
        let config = Config {
            server_url: "https://naatre.example.com:7443".into(),
            token: Some("t".into()),
            insecure_http: Some("http://naatre.lan:7400".into()),
            ca_file: Some("/etc/pki/naatre-ca.pem".into()),
        };
        let dir = tempfile::tempdir().expect("tempdir");
        let wc = Wc {
            root: dir.path().to_path_buf(),
        };
        std::fs::create_dir_all(wc.naatre_dir()).expect("naatre dir");
        wc.save_config(&config).expect("save");
        // 0600 preserved.
        let mode = std::fs::metadata(wc.config_path())
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600);
        assert_eq!(wc.load_config().expect("load"), config);
        // No opt-in and no bundle write nothing — the pre-F1 shape.
        let old = Config {
            server_url: "http://127.0.0.1:7400".into(),
            ..Config::default()
        };
        wc.save_config(&old).expect("save");
        let text = std::fs::read_to_string(wc.config_path()).expect("read");
        assert_eq!(text, "server_url = \"http://127.0.0.1:7400\"\n");
        assert_eq!(wc.load_config().expect("load"), old);
    }

    #[test]
    fn config_parsing_is_strict() {
        // Old configs without the new keys parse unchanged.
        let old = b"server_url = \"http://127.0.0.1:7400\"\ntoken = \"t\"\n";
        let parsed = parse_config(old).expect("old config");
        assert_eq!(parsed.insecure_http, None);
        assert_eq!(parsed.ca_file, None);
        // The per-remote origin form parses (and canonicalizes).
        let parsed =
            parse_config(b"server_url = \"x\"\ninsecure_http = \"http://Naatre.lan:7400\"\n")
                .expect("origin form");
        assert_eq!(parsed.insecure_http, Some("http://naatre.lan:7400".into()));
        // The never-shipped boolean form is refused, naming the shape.
        for bad in ["true", "false", "yes"] {
            let line = format!("server_url = \"x\"\ninsecure_http = \"{bad}\"\n");
            let err = parse_config(line.as_bytes()).expect_err(bad);
            assert!(err.contains("insecure_http"), "{err:?}");
            assert!(err.contains("http://host[:port]"), "{err:?}");
        }
        // Unknown keys are errors.
        let err = parse_config(b"server_url = \"x\"\nproxy = \"y\"\n").expect_err("unknown key");
        assert!(err.contains("unknown key"), "{err:?}");
        // An https origin is not a plain-http opt-in.
        let err =
            parse_config(b"server_url = \"x\"\ninsecure_http = \"https://host.example.com\"\n")
                .expect_err("https origin");
        assert!(err.contains("plain-http"), "{err:?}");
    }

    #[test]
    fn scheme_rule_four_outcomes() {
        // https: always Ok.
        assert_eq!(
            ensure_scheme_allowed("https://host.example.com", false, None),
            Ok(())
        );
        // http loopback: Ok regardless of any opt-in.
        assert_eq!(
            ensure_scheme_allowed("http://127.0.0.1:7400", false, None),
            Ok(())
        );
        assert_eq!(
            ensure_scheme_allowed("http://localhost:7400", false, None),
            Ok(())
        );
        // http non-loopback without any opt-in: refused, naming every
        // opt-in and the https alternative.
        let err =
            ensure_scheme_allowed("http://naatre.lan:7400", false, None).expect_err("refused");
        assert!(err.contains("--insecure-http"), "{err:?}");
        assert!(err.contains("NAATRE_INSECURE_HTTP"), "{err:?}");
        assert!(err.contains("naatre config set insecure_http"), "{err:?}");
        assert!(err.contains("https://"), "{err:?}");
        // The invocation-level opt-in admits any host.
        assert_eq!(
            ensure_scheme_allowed("http://naatre.lan:7400", true, None),
            Ok(())
        );
        // The per-remote stored origin admits EXACTLY its own origin: the
        // resolved server on that origin passes; an override to another
        // host (or scheme, or port) does not inherit it.
        let stored = Some("http://naatre.lan:7400");
        assert_eq!(
            ensure_scheme_allowed("http://naatre.lan:7400", false, stored),
            Ok(())
        );
        assert_eq!(
            ensure_scheme_allowed("http://naatre.lan:7400/", false, stored),
            Ok(())
        );
        assert!(
            ensure_scheme_allowed("http://other.lan:7400", false, stored).is_err(),
            "a --server override to another host does not inherit the opt-in"
        );
        assert!(
            ensure_scheme_allowed("http://naatre.lan:7500", false, stored).is_err(),
            "a different port is a different origin"
        );
        assert!(
            ensure_scheme_allowed("https://naatre.lan:7400", false, stored).is_ok(),
            "https never needed the opt-in anyway"
        );
        // A stored origin that fails the one parser admits nothing.
        assert!(
            ensure_scheme_allowed("http://naatre.lan:7400", false, Some("naatre.lan")).is_err()
        );
    }
}
