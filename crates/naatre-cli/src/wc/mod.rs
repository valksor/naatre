//! The client working copy: the `.naatre/` directory inside a checkout.
//!
//! Layout (plan "Client working copy"):
//! - `config.toml` — `server_url` + optional `token`, written 0600 by
//!   `clone` and `login`. Precedence for server settings:
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

/// Reserved first components: never scanned, never committed.
pub const RESERVED: &[&str] = &[".naatre", ".git"];

/// Unique temp-name counter (per process) for atomic writes.
static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// `.naatre/config.toml`. Two known keys, both optional on disk except
/// `server_url` once a clone wrote the file. Parsing is strict: any other
/// key or malformed line is an error (never merged into defaults).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Config {
    pub server_url: String,
    pub token: Option<String>,
}

/// `.naatre/state.json`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct State {
    /// Current changeset id (64 lowercase hex).
    pub head: String,
    /// Land base once land exists (M4); null until then.
    pub base: Option<String>,
    /// Sparse prefix list; empty = the full tree is checked out.
    pub sparse: Vec<String>,
    /// Repo name on the server.
    pub server_repo: String,
    /// M12b: the paths of head's tree that materialize conflict markers
    /// (the conflicted paths). Recorded by sync/clone/land, cleared by
    /// resolve and by a commit that rewrites the path; `status` marks
    /// them (`C <path>`), `resolve` refuses paths outside the list.
    /// `default`: state files written before M12b carry no key.
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

    /// Resolves the server settings for a verb, in the pinned precedence:
    /// `--server`/`--token` flags, then `NAATRE_URL`/`NAATRE_TOKEN` env,
    /// then `config.toml`.
    pub fn resolve_server(
        &self,
        flag_server: Option<&str>,
        flag_token: Option<&str>,
    ) -> Result<(String, Option<String>), String> {
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
        Ok((server, token))
    }

    /// A ready HTTP client for this working copy's server.
    pub fn client(&self, net: &crate::verbs::Net) -> Result<Client, String> {
        let (server, token) = self.resolve_server(net.server.as_deref(), net.token.as_deref())?;
        Ok(Client::new(&server, token))
    }
}

fn env_var(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.is_empty())
}

/// Strict `key = "value"` parser for the two-key config; unknown keys and
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
