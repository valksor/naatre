//! The `[backend]` config surface (M9): `backend.toml` in the data dir,
//! 0600, selecting the metadata and blob backends.
//!
//! Zero-config default is SQLite + filesystem — an absent file selects
//! them. A present file is parsed STRICTLY (unknown keys, unknown values,
//! and malformed structure are loud refusals that name the key and the
//! expected shape, never the file's values — parse errors can otherwise
//! quote file contents, and this file carries credentials).
//!
//! Redaction contract (plan M9): DSN/endpoint/credentials never appear in
//! the startup banner, request logs, or error strings. The parsed config
//! keeps the DSN only as its structured parts; driver errors are reduced
//! to a classified kind ([`crate::meta::DbError::Postgres`]); and this
//! module's own errors name rules and keys, not URLs.
//!
//! Inline credentials are rejected at parse: a `postgres://user:pass@host`
//! DSN is refused ("put the password in the `password` key or
//! NAATRE_PG_PASSWORD"). TLS is the default for Postgres (`sslmode`
//! defaults to `require`; `disable` needs the explicit `allow_plaintext`
//! opt-in), and will be for S3 in M9b (LAN-minio plaintext opt-in, same
//! key).

use std::path::{Path, PathBuf};

use crate::error::StoreError;

/// The backend.toml file name inside the data dir.
pub const BACKEND_FILE: &str = "backend.toml";

/// Environment variables consulted when the file does not carry the value
/// (the file wins; env is the injection point for CI and secret stores).
pub const ENV_PG_DSN: &str = "NAATRE_PG_DSN";
pub const ENV_PG_USER: &str = "NAATRE_PG_USER";
pub const ENV_PG_PASSWORD: &str = "NAATRE_PG_PASSWORD";

/// Which metadata backend the store opens.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum MetaKind {
    /// Embedded SQLite in the data dir (the zero-config default).
    #[default]
    Sqlite,
    /// Postgres 16+ over a configured DSN.
    Postgres,
}

impl MetaKind {
    pub fn as_str(self) -> &'static str {
        match self {
            MetaKind::Sqlite => "sqlite",
            MetaKind::Postgres => "postgres",
        }
    }
}

/// Which blob CAS the store uses.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum BlobKind {
    /// Filesystem CAS under `<data>/objects` (the zero-config default).
    #[default]
    Fs,
    /// S3-compatible CAS — arrives in M9b.
    S3,
}

impl BlobKind {
    pub fn as_str(self) -> &'static str {
        match self {
            BlobKind::Fs => "fs",
            BlobKind::S3 => "s3",
        }
    }
}

impl std::fmt::Debug for PgConnect {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PgConnect")
            .field("host", &self.host)
            .field("port", &self.port)
            .field("dbname", &self.dbname)
            .field("user", &self.user)
            .field("password", &"<redacted>")
            .field("tls", &self.tls)
            .finish_non_exhaustive()
    }
}

/// Postgres TLS posture, parsed from the DSN's `sslmode` parameter.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PgTls {
    /// Plaintext — only with `allow_plaintext = true`.
    Disable,
    /// Encrypted, no certificate verification (libpq's `require`).
    Require,
    /// Encrypted, certificate chain verified, hostname not checked.
    VerifyCa,
    /// Encrypted, chain and hostname verified.
    VerifyFull,
}

/// The parsed Postgres connection parameters. The DSN string itself is
/// consumed by the parser and kept nowhere. `Debug` is REDACTED by hand —
/// a derive would print the password.
#[derive(Clone)]
pub struct PgConnect {
    /// Host name, IP, or (starting with `/`) a unix-socket directory.
    pub host: String,
    pub port: u16,
    pub dbname: String,
    /// From URL userinfo (`postgres://user@…`), the `user` key, or
    /// `NAATRE_PG_USER`; `None` lets the driver default to the OS user.
    pub user: Option<String>,
    /// From the `password` key or `NAATRE_PG_PASSWORD` — never from the URL.
    pub password: Option<String>,
    pub tls: PgTls,
    /// Test isolation only (per-test PG schema): never parseable from TOML
    /// — the strict surface refuses it there.
    pub schema: Option<String>,
}

/// The parsed backend configuration.
#[derive(Debug, Clone, Default)]
pub struct BackendConfig {
    pub meta: MetaKind,
    pub blob: BlobKind,
    /// Present when `meta = "postgres"`.
    pub pg: Option<PgConnect>,
    /// The plaintext opt-in: without it, `sslmode=disable` is refused.
    pub allow_plaintext: bool,
}

/// Reads and parses `<data>/backend.toml`; an absent file selects the
/// zero-config default (SQLite + fs). The file must be 0600 when present.
pub fn load(data: &Path) -> Result<BackendConfig, StoreError> {
    let path = data.join(BACKEND_FILE);
    let text = match std::fs::read_to_string(&path) {
        Ok(t) => t,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(BackendConfig::default()),
        Err(e) => return Err(StoreError::Backend(BackendError::Load(path, e))),
    };
    let cfg = parse(&text).map_err(StoreError::Backend)?;
    check_file_perms(&path)?;
    Ok(cfg)
}

/// Builds the default config plus explicit overrides (used by tests and by
/// `Store::open_backend` callers that construct the config in code).
pub fn explicit(meta: MetaKind, blob: BlobKind, pg: Option<PgConnect>) -> BackendConfig {
    BackendConfig {
        meta,
        blob,
        pg,
        allow_plaintext: false,
    }
}

fn check_file_perms(path: &Path) -> Result<(), StoreError> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(path)
            .map_err(|e| StoreError::Backend(BackendError::Load(path.to_path_buf(), e)))?
            .permissions()
            .mode();
        if mode & 0o177 != 0 {
            return Err(StoreError::Backend(BackendError::FilePerms {
                path: path.to_path_buf(),
                mode: mode & 0o777,
            }));
        }
    }
    Ok(())
}

impl BackendConfig {
    /// Programmatic construction with the same strict DSN parse the TOML
    /// surface uses (tests, embedded use). Credentials stay out of the URL
    /// here too; `allow_plaintext` opts into `sslmode=disable` (CI's
    /// TLS-less Postgres).
    pub fn postgres(
        dsn: &str,
        user: Option<String>,
        password: Option<String>,
        allow_plaintext: bool,
    ) -> Result<BackendConfig, BackendError> {
        let parsed = parse_dsn(dsn, PgDsnSource::File)?;
        if parsed.tls == PgTls::Disable && !allow_plaintext {
            return Err(BackendError::PlaintextNotOptedIn);
        }
        Ok(BackendConfig {
            meta: MetaKind::Postgres,
            blob: BlobKind::Fs,
            pg: Some(PgConnect {
                host: parsed.host,
                port: parsed.port,
                dbname: parsed.dbname,
                user: user.or(parsed.user),
                password,
                tls: parsed.tls,
                schema: None,
            }),
            allow_plaintext,
        })
    }
}

/// Strict parse of the `backend.toml` text. Value contents never appear in
/// errors — keys and expected shapes only.
pub fn parse(text: &str) -> Result<BackendConfig, BackendError> {
    // Document parse (a [table] document is not a bare "value"); parse
    // errors can quote file contents — withheld behind NotToml.
    let value: toml::Value = std::str::FromStr::from_str(text)
        .or_else(|_| toml::from_str::<toml::Value>(text))
        .map_err(|_| BackendError::NotToml)?;

    let table = value.as_table().ok_or(BackendError::NotATable)?;
    // Top level: only the [backend] table.
    for key in table.keys() {
        if key != "backend" {
            return Err(BackendError::UnknownKey {
                key: format!("[{key}]"),
                allowed: "[backend]".to_string(),
            });
        }
    }
    let backend = table
        .get("backend")
        .ok_or(BackendError::MissingBackendTable)?;
    let backend = backend.as_table().ok_or(BackendError::BadValue {
        key: "[backend]".to_string(),
        want: "a table".to_string(),
    })?;

    const ALLOWED: &[&str] = &["meta", "blob", "dsn", "user", "password", "allow_plaintext"];
    for key in backend.keys() {
        if !ALLOWED.contains(&key.as_str()) {
            return Err(BackendError::UnknownKey {
                key: format!("backend.{key}"),
                allowed: ALLOWED.join(", "),
            });
        }
    }

    let meta = match backend.get("meta") {
        None => MetaKind::Sqlite,
        Some(v) => match v.as_str() {
            Some("sqlite") => MetaKind::Sqlite,
            Some("postgres") => MetaKind::Postgres,
            _ => {
                return Err(BackendError::BadValue {
                    key: "backend.meta".to_string(),
                    want: "\"sqlite\" or \"postgres\"".to_string(),
                });
            }
        },
    };
    let blob = match backend.get("blob") {
        None => BlobKind::Fs,
        Some(v) => match v.as_str() {
            Some("fs") => BlobKind::Fs,
            Some("s3") => return Err(BackendError::S3NotYet),
            _ => {
                return Err(BackendError::BadValue {
                    key: "backend.blob".to_string(),
                    want: "\"fs\" (\"s3\" arrives in M9b)".to_string(),
                });
            }
        },
    };
    let allow_plaintext = match backend.get("allow_plaintext") {
        None => false,
        Some(v) => v.as_bool().ok_or(BackendError::BadValue {
            key: "backend.allow_plaintext".to_string(),
            want: "true or false".to_string(),
        })?,
    };

    let file_dsn = str_key(backend, "dsn")?;
    let has_file_dsn = file_dsn.is_some();
    let dsn = std::env::var(ENV_PG_DSN).unwrap_or_else(|_| file_dsn.unwrap_or_default());
    let file_user = str_key(backend, "user")?;
    let file_password = str_key(backend, "password")?;

    let pg = if meta == MetaKind::Postgres {
        let source = if std::env::var(ENV_PG_DSN).is_ok() {
            PgDsnSource::Env
        } else if has_file_dsn {
            PgDsnSource::File
        } else {
            PgDsnSource::Missing
        };
        let parsed = parse_dsn(&dsn, source)?;
        let user = file_user
            .or_else(|| std::env::var(ENV_PG_USER).ok())
            .or(parsed.user);
        let password = file_password.or_else(|| std::env::var(ENV_PG_PASSWORD).ok());
        if parsed.tls == PgTls::Disable && !allow_plaintext {
            return Err(BackendError::PlaintextNotOptedIn);
        }
        Some(PgConnect {
            host: parsed.host,
            port: parsed.port,
            dbname: parsed.dbname,
            user,
            password,
            tls: parsed.tls,
            schema: None,
        })
    } else {
        // Values that only make sense for postgres are refused under
        // sqlite — a dsn sitting unused in the file is a misconfiguration,
        // not an inert one.
        if has_file_dsn || std::env::var(ENV_PG_DSN).is_ok() {
            return Err(BackendError::KeyWithoutPostgres {
                key: "dsn".to_string(),
            });
        }
        if file_user.is_some() || file_password.is_some() {
            return Err(BackendError::KeyWithoutPostgres {
                key: "user/password".to_string(),
            });
        }
        None
    };

    Ok(BackendConfig {
        meta,
        blob,
        pg,
        allow_plaintext,
    })
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum PgDsnSource {
    File,
    Env,
    Missing,
}

fn str_key(table: &toml::Table, key: &str) -> Result<Option<String>, BackendError> {
    match table.get(key) {
        None => Ok(None),
        Some(v) => v
            .as_str()
            .map(|s| Some(s.to_string()))
            .ok_or(BackendError::BadValue {
                key: format!("backend.{key}"),
                want: "a string".to_string(),
            }),
    }
}

struct ParsedDsn {
    host: String,
    port: u16,
    dbname: String,
    /// A user-only userinfo (`postgres://user@…`); passwords are refused at
    /// parse, so userinfo never carries one past this point.
    user: Option<String>,
    tls: PgTls,
}

/// Parses the `postgres://` DSN strictly. Errors name the RULE violated,
/// never the URL. The default `sslmode` is `require` (TLS by default).
fn parse_dsn(dsn: &str, source: PgDsnSource) -> Result<ParsedDsn, BackendError> {
    let rest = dsn
        .strip_prefix("postgres://")
        .or_else(|| dsn.strip_prefix("postgresql://"))
        .ok_or_else(|| BackendError::DsnInvalid {
            why: "must start with postgres:// or postgresql://".to_string(),
        })?;

    // Authority = everything before the first '/', '?', or '#'.
    let authority_end = rest
        .find(|c: char| ['/', '?', '#'].contains(&c))
        .unwrap_or(rest.len());
    let (authority, rest) = rest.split_at(authority_end);

    // Userinfo (before the LAST '@'; two '@'s are ambiguous → refuse).
    let mut user = None;
    let hostport = match authority.rfind('@') {
        None => authority,
        Some(at) => {
            if authority.contains('@') && authority.find('@') != Some(at) {
                return Err(BackendError::DsnInvalid {
                    why: "more than one '@' in the authority".to_string(),
                });
            }
            let userinfo = &authority[..at];
            if userinfo.contains(':') {
                return Err(BackendError::InlineCredentials);
            }
            if userinfo.is_empty() {
                return Err(BackendError::DsnInvalid {
                    why: "empty userinfo before '@'".to_string(),
                });
            }
            user = Some(userinfo.to_string());
            &authority[at + 1..]
        }
    };

    // host[:port]
    let (host, port) = match hostport.rfind(':') {
        None => (hostport, None),
        Some(colon) => {
            let p = &hostport[colon + 1..];
            if p.is_empty() || !p.bytes().all(|b| b.is_ascii_digit()) {
                return Err(BackendError::DsnInvalid {
                    why: "port must be a number".to_string(),
                });
            }
            let port = p.parse::<u16>().map_err(|_| BackendError::DsnInvalid {
                why: "port out of range".to_string(),
            })?;
            (&hostport[..colon], Some(port))
        }
    };
    if host.contains('%') {
        return Err(BackendError::DsnInvalid {
            why: "percent-encoding is not supported; use plain host names \
                  (a unix-socket directory may be given as a path)"
                .to_string(),
        });
    }
    if host.is_empty() && !dsn.starts_with("postgres:///") && !dsn.starts_with("postgresql:///") {
        return Err(BackendError::DsnInvalid {
            why: "empty host".to_string(),
        });
    }

    // /dbname
    let after_path;
    let dbname = if let Some(path) = rest.strip_prefix('/') {
        let end = path.find(['?', '#']).unwrap_or(path.len());
        let name = &path[..end];
        after_path = &path[end..];
        if name.contains('%') {
            return Err(BackendError::DsnInvalid {
                why: "percent-encoding is not supported in the database name".to_string(),
            });
        }
        name.to_string()
    } else {
        after_path = rest;
        String::new()
    };

    // ?query — sslmode only.
    let mut tls = PgTls::Require;
    if let Some(query) = after_path.strip_prefix('?') {
        let query = query.split('#').next().unwrap_or(query);
        for pair in query.split('&') {
            let (k, v) = pair
                .split_once('=')
                .ok_or_else(|| BackendError::DsnInvalid {
                    why: "query parameters must be key=value".to_string(),
                })?;
            if k != "sslmode" {
                return Err(BackendError::DsnInvalid {
                    why: "the only supported DSN query parameter is sslmode \
                         (got another; user, password, and certificate paths go in \
                         backend.toml or the NAATRE_PG_* environment variables)"
                        .to_string(),
                });
            }
            tls = match v {
                "require" => PgTls::Require,
                "verify-ca" => PgTls::VerifyCa,
                "verify-full" => PgTls::VerifyFull,
                "disable" => PgTls::Disable,
                "allow" | "prefer" => {
                    return Err(BackendError::DsnInvalid {
                        why: "sslmode allow/prefer is not supported: pick require, \
                              verify-ca, verify-full, or disable"
                            .to_string(),
                    });
                }
                _ => {
                    return Err(BackendError::DsnInvalid {
                        why: "sslmode must be require, verify-ca, verify-full, or disable"
                            .to_string(),
                    });
                }
            };
        }
    }
    if after_path.contains('#') {
        return Err(BackendError::DsnInvalid {
            why: "URL fragments are not supported".to_string(),
        });
    }
    if dbname.is_empty() {
        return Err(BackendError::DsnInvalid {
            why: match source {
                PgDsnSource::Missing => {
                    "no DSN: set meta = \"postgres\" with a dsn key or NAATRE_PG_DSN".to_string()
                }
                _ => "the DSN must name a database (postgres://host/dbname)".to_string(),
            },
        });
    }

    Ok(ParsedDsn {
        host: host.to_string(),
        port: port.unwrap_or(5432),
        dbname,
        user,
        tls,
    })
}

/// Errors from the backend config surface. Every message names rules,
/// keys, and paths — never DSN/credential VALUES.
#[derive(Debug, thiserror::Error)]
pub enum BackendError {
    #[error("cannot read backend config {0:?}: {1}")]
    Load(PathBuf, std::io::Error),

    #[error(
        "backend.toml is not valid TOML (details withheld: parse errors can quote file contents, and this file carries credentials)"
    )]
    NotToml,

    #[error("backend.toml must contain a top-level table")]
    NotATable,

    #[error(
        "backend.toml must contain a [backend] table (meta = \"sqlite\"|\"postgres\", blob = \"fs\")"
    )]
    MissingBackendTable,

    #[error("unknown key {key:?} in backend.toml (allowed: {allowed})")]
    UnknownKey { key: String, allowed: String },

    #[error("backend.toml key {key:?}: expected {want}")]
    BadValue { key: String, want: String },

    #[error(
        "backend.toml key {key:?} only applies when meta = \"postgres\"; \
         remove it or switch the metadata backend"
    )]
    KeyWithoutPostgres { key: String },

    #[error(
        "inline credentials in the DSN are rejected: use `postgres://user@host/db` \
         and put the password in the backend.toml `password` key or NAATRE_PG_PASSWORD"
    )]
    InlineCredentials,

    #[error("backend dsn: {why}")]
    DsnInvalid { why: String },

    #[error(
        "sslmode = disable refused: naatre requires TLS for Postgres unless the \
         backend.toml opt-in `allow_plaintext = true` is set"
    )]
    PlaintextNotOptedIn,

    #[error("backend.toml {path:?} must be 0600 (found mode {mode:o}); chmod 600 it")]
    FilePerms { path: PathBuf, mode: u32 },

    #[error("blob = \"s3\" is not available yet: the S3-compatible CAS backend arrives in M9b")]
    S3NotYet,

    #[error(
        "data dir metadata store is {on_disk} but the backend config selects {configured}: \
         refusing to open (a data dir never switches backends in place)"
    )]
    Mismatch {
        on_disk: &'static str,
        configured: &'static str,
    },

    #[error(
        "postgres connection failed (driver details withheld: connect errors can \
         embed DSN, host, and user; check the DSN, credentials, and TLS mode)"
    )]
    Connect,

    #[error(
        "metadata store already initialized (schema present): refusing to \
         re-initialize — naatre never re-mints an initialized store"
    )]
    AlreadyInitialized,
}

/// The mismatch guard (plan M9): refuses a data dir whose existing
/// metadata store disagrees with the configured backend, naming both.
///
/// * `naatre.db` present + `meta = postgres` → [`BackendError::Mismatch`].
/// * `meta = sqlite` + `naatre.db` absent → the standard
///   `DataDirUninitialized` (run `naatre init`).
/// * `meta = postgres` + uninitialized server → checked at connect time by
///   `init` (a schema present there is `AlreadyInitialized`).
pub fn check_data_dir_backend(data: &Path, cfg: &BackendConfig) -> Result<(), StoreError> {
    let db = data.join(crate::data_dir::DB_FILE);
    match (db.exists(), cfg.meta) {
        (true, MetaKind::Postgres) => Err(StoreError::Backend(BackendError::Mismatch {
            on_disk: "sqlite (naatre.db present)",
            configured: "postgres",
        })),
        (false, MetaKind::Sqlite) => Err(StoreError::DataDirUninitialized(data.to_path_buf())),
        _ => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn err_of(text: &str) -> BackendError {
        parse(text).unwrap_err()
    }

    #[test]
    fn absent_file_is_zero_config_default() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = load(dir.path()).unwrap();
        assert_eq!(cfg.meta, MetaKind::Sqlite);
        assert_eq!(cfg.blob, BlobKind::Fs);
        assert!(cfg.pg.is_none());
    }

    #[test]
    fn minimal_postgres_file() {
        let cfg = parse("[backend]\nmeta = \"postgres\"\ndsn = \"postgres://db.example:5433/naatre?sslmode=verify-full\"\nuser = \"naatre\"\n").unwrap();
        let pg = cfg.pg.unwrap();
        assert_eq!(pg.host, "db.example");
        assert_eq!(pg.port, 5433);
        assert_eq!(pg.dbname, "naatre");
        assert_eq!(pg.user.as_deref(), Some("naatre"));
        assert_eq!(pg.tls, PgTls::VerifyFull);
        assert!(pg.password.is_none());
    }

    #[test]
    fn defaults_tls_require_and_default_port_and_userinfo_user() {
        let cfg =
            parse("[backend]\nmeta = \"postgres\"\ndsn = \"postgres://svc@db.example/naatre\"\n")
                .unwrap();
        let pg = cfg.pg.unwrap();
        assert_eq!(pg.tls, PgTls::Require);
        assert_eq!(pg.port, 5432);
        assert_eq!(pg.user.as_deref(), Some("svc"));
    }

    #[test]
    fn inline_credentials_refused_without_echo() {
        let e = err_of(
            "[backend]\nmeta = \"postgres\"\ndsn = \"postgres://u:sekret@dbhost.example/db\"\n",
        );
        assert!(matches!(e, BackendError::InlineCredentials));
        let msg = e.to_string();
        assert!(!msg.contains("sekret"), "credential leaked: {msg}");
        assert!(!msg.contains("dbhost.example"), "host leaked: {msg}");
        assert!(msg.contains("password"), "must name the remedy: {msg}");
    }

    #[test]
    fn unknown_keys_and_values_refused_loudly() {
        let e = err_of("[backend]\nmeta = \"postgres\"\nzzz = 1\n");
        assert!(matches!(e, BackendError::UnknownKey { ref key, .. } if key == "backend.zzz"));
        let e = err_of("[backend]\nmeta = \"sqlite3\"\n");
        assert!(matches!(e, BackendError::BadValue { ref key, .. } if key == "backend.meta"));
        let e = err_of("[other]\nx = 1\n");
        assert!(matches!(e, BackendError::UnknownKey { ref key, .. } if key == "[other]"));
        let e = err_of("[backend]\ndsn = 5\n");
        assert!(matches!(e, BackendError::BadValue { ref key, .. } if key == "backend.dsn"));
        // A type error must not quote the value either.
        assert!(!e.to_string().contains('5'));
    }

    #[test]
    fn parse_error_text_withheld() {
        let e = err_of("[backend]\nmeta = = = broken");
        assert!(matches!(e, BackendError::NotToml));
        assert!(
            !e.to_string().contains("broken"),
            "parse error echoed content: {e}"
        );
    }

    #[test]
    fn sslmode_disable_needs_opt_in() {
        let e =
            err_of("[backend]\nmeta = \"postgres\"\ndsn = \"postgres://h/db?sslmode=disable\"\n");
        assert!(matches!(e, BackendError::PlaintextNotOptedIn));
        let cfg = parse(
            "[backend]\nmeta = \"postgres\"\ndsn = \"postgres://h/db?sslmode=disable\"\nallow_plaintext = true\n",
        )
        .unwrap();
        assert_eq!(cfg.pg.unwrap().tls, PgTls::Disable);
    }

    #[test]
    fn allow_and_prefer_refused() {
        for mode in ["allow", "prefer"] {
            let e = err_of(&format!(
                "[backend]\nmeta = \"postgres\"\ndsn = \"postgres://h/db?sslmode={mode}\"\n"
            ));
            assert!(matches!(e, BackendError::DsnInvalid { .. }), "{mode}: {e}");
        }
    }

    #[test]
    fn unknown_query_params_refused() {
        let e = err_of(
            "[backend]\nmeta = \"postgres\"\ndsn = \"postgres://dbhost.example/db?password=sekret\"\n",
        );
        assert!(matches!(e, BackendError::DsnInvalid { .. }));
        let msg = e.to_string();
        assert!(!msg.contains("sekret"), "query value leaked: {msg}");
    }

    #[test]
    fn sqlite_rejects_pg_only_keys() {
        let e = err_of("[backend]\ndsn = \"postgres://h/db\"\n");
        assert!(matches!(e, BackendError::KeyWithoutPostgres { ref key } if key == "dsn"));
    }

    #[test]
    fn blob_s3_refused_naming_m9b() {
        let e = err_of("[backend]\nblob = \"s3\"\n");
        let msg = e.to_string();
        assert!(msg.contains("M9b"), "{msg}");
    }

    #[test]
    fn dsn_shapes() {
        // Unix socket dir as host.
        let cfg = parse(
            "[backend]\nmeta = \"postgres\"\ndsn = \"postgres:///naatre?host=/var/run/postgresql\"\n",
        );
        assert!(cfg.is_err(), "host via query param is not whitelisted");
        let cfg =
            parse("[backend]\nmeta = \"postgres\"\ndsn = \"postgresql:///naatre\"\n").unwrap();
        assert_eq!(cfg.pg.unwrap().host, "");
        // Missing dbname.
        assert!(parse("[backend]\nmeta = \"postgres\"\ndsn = \"postgres://h\"\n").is_err());
        // Fragment refused.
        assert!(parse("[backend]\nmeta = \"postgres\"\ndsn = \"postgres://h/db#frag\"\n").is_err());
        // Bad port.
        assert!(
            parse("[backend]\nmeta = \"postgres\"\ndsn = \"postgres://h:notaport/db\"\n").is_err()
        );
    }

    #[test]
    fn ci_dsn_shape_parses_with_separate_password() {
        // The CI job's exact env pair: user-in-URL, password in its own
        // variable — the inline form must stay rejected.
        let cfg = BackendConfig::postgres(
            "postgres://naatre@localhost:5432/naatre_test?sslmode=disable",
            None,
            Some("naatre".to_string()),
            true,
        )
        .expect("the CI DSN shape parses");
        let pg = cfg.pg.unwrap();
        assert_eq!(pg.host, "localhost");
        assert_eq!(pg.port, 5432);
        assert_eq!(pg.dbname, "naatre_test");
        assert_eq!(pg.user.as_deref(), Some("naatre"));
        assert_eq!(pg.password.as_deref(), Some("naatre"));
        assert_eq!(pg.tls, PgTls::Disable);
        assert!(cfg.allow_plaintext);
        // And the inline form of the same DSN stays rejected.
        let e = BackendConfig::postgres(
            "postgres://naatre:naatre@localhost:5432/naatre_test?sslmode=disable",
            None,
            None,
            true,
        )
        .unwrap_err();
        assert!(matches!(e, BackendError::InlineCredentials));
    }

    #[test]
    fn mismatch_guard() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("naatre.db"), b"").unwrap();
        let cfg = BackendConfig {
            meta: MetaKind::Postgres,
            ..BackendConfig::default()
        };
        let e = check_data_dir_backend(dir.path(), &cfg).unwrap_err();
        let msg = e.to_string();
        assert!(msg.contains("sqlite") && msg.contains("postgres"), "{msg}");
        // The reverse: configured sqlite, no naatre.db → run-init error.
        let empty = tempfile::tempdir().unwrap();
        let e = check_data_dir_backend(empty.path(), &BackendConfig::default()).unwrap_err();
        assert!(matches!(e, StoreError::DataDirUninitialized(_)));
        // Matching combos pass.
        let cfg = BackendConfig {
            meta: MetaKind::Postgres,
            ..BackendConfig::default()
        };
        let empty2 = tempfile::tempdir().unwrap();
        assert!(check_data_dir_backend(empty2.path(), &cfg).is_ok());
        assert!(check_data_dir_backend(dir.path(), &BackendConfig::default()).is_ok());
    }

    #[test]
    fn file_perms_enforced() {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(BACKEND_FILE);
            std::fs::write(&path, "[backend]\n").unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
            let e = load(dir.path()).unwrap_err();
            assert!(e.to_string().contains("0600"), "{e}");
        }
    }
}
