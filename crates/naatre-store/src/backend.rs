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
//! opt-in) and for S3 (an `endpoint` is reached over HTTPS unless
//! `allow_plaintext` opts into plain HTTP for LAN minio).

use std::path::{Path, PathBuf};

use crate::error::StoreError;
use crate::layout::Layout;

/// The backend.toml file name inside the data dir.
pub const BACKEND_FILE: &str = "backend.toml";

/// Environment variables consulted when the file does not carry the value
/// (the file wins; env is the injection point for CI and secret stores).
pub const ENV_PG_DSN: &str = "NAATRE_PG_DSN";
pub const ENV_PG_USER: &str = "NAATRE_PG_USER";
pub const ENV_PG_PASSWORD: &str = "NAATRE_PG_PASSWORD";
pub const ENV_S3_ENDPOINT: &str = "NAATRE_S3_ENDPOINT";
pub const ENV_S3_BUCKET: &str = "NAATRE_S3_BUCKET";
pub const ENV_S3_REGION: &str = "NAATRE_S3_REGION";
pub const ENV_S3_ACCESS_KEY_ID: &str = "NAATRE_S3_ACCESS_KEY_ID";
pub const ENV_S3_SECRET: &str = "NAATRE_S3_SECRET";

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
    /// S3-compatible CAS (M9b): history blobs and the ephemeral snapshot
    /// namespace live in the configured bucket.
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
    /// The `sslrootcert` file key: a PEM CA bundle ADDED to the platform
    /// roots for `verify-ca`/`verify-full` — the private-CA shape of a
    /// self-hosted Postgres, which the system trust store cannot verify.
    /// 0600-checked where it is read (the connect path).
    pub sslrootcert: Option<std::path::PathBuf>,
    /// Test isolation only (per-test PG schema): never parseable from TOML
    /// — the strict surface refuses it there.
    pub schema: Option<String>,
}

impl std::fmt::Debug for S3Connect {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("S3Connect")
            .field("endpoint", &self.endpoint)
            .field("bucket", &self.bucket)
            .field("region", &self.region)
            .field("access_key_id", &self.access_key_id)
            .field("secret", &"<redacted>")
            .field("path_style", &self.path_style)
            .finish()
    }
}

/// The parsed S3 connection parameters (M9b). The secret lives here only
/// as a structured part — never in a URL — and `Debug` is REDACTED by hand.
///
/// TLS posture is not a field: an endpoint is reached over HTTPS unless the
/// config's `allow_plaintext` opt-in is set, in which case plain HTTP is
/// used (LAN minio). An absent endpoint selects the AWS default endpoints
/// (always TLS, virtual-host style).
#[derive(Clone)]
pub struct S3Connect {
    /// Bare `host[:port]` authority — no scheme, no path. `None` = the AWS
    /// default endpoints. Refused shapes (scheme, userinfo, path) name the
    /// rule, never the value.
    pub endpoint: Option<String>,
    pub bucket: String,
    /// S3 region for signing; the driver defaults to `us-east-1` when
    /// unset. Set it for real AWS buckets outside that region.
    pub region: Option<String>,
    /// File key or `NAATRE_S3_ACCESS_KEY_ID`; required when `blob = "s3"`.
    pub access_key_id: String,
    /// File key or `NAATRE_S3_SECRET`; required, `Debug`-redacted.
    pub secret: String,
    /// `true` = path-style (`https://host/bucket/key`, LAN minio's shape);
    /// `false` = virtual-host style (`https://bucket.host/key`, AWS's
    /// default). Default false.
    pub path_style: bool,
}

/// The parsed backend configuration.
#[derive(Debug, Clone, Default)]
pub struct BackendConfig {
    pub meta: MetaKind,
    pub blob: BlobKind,
    /// The metadata layout (M13a): `Single` (the zero-config default) or
    /// `DbPerRepo` (catalog.db + one SQLite file per repo; SQLite meta
    /// only — the parser refuses it under Postgres).
    pub layout: Layout,
    /// Present when `meta = "postgres"`.
    pub pg: Option<PgConnect>,
    /// Present when `blob = "s3"`.
    pub s3: Option<S3Connect>,
    /// The plaintext opt-in: without it, `sslmode=disable` (Postgres) and
    /// plain-HTTP S3 endpoints are refused.
    pub allow_plaintext: bool,
    /// The reachability GC's serve-side pass interval in seconds (M10a);
    /// `None` = the default (3600), `Some(0)` = the background task off.
    pub gc_interval_secs: Option<u64>,
    /// The reachability GC's collection grace window in seconds (M10a);
    /// `None` = the default (30 days). A candidate collects only after
    /// this long past its first-unreachable mark.
    pub gc_grace_secs: Option<u64>,
    /// The `sslrootcert` file key (a PEM CA bundle for Postgres
    /// `verify-ca`/`verify-full`); carried here from the file, consumed by
    /// the connect path.
    pub sslrootcert: Option<std::path::PathBuf>,
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
    /// Programmatic `blob = "s3"` construction from structured parts
    /// (tests, embedded use) — the same shape the strict TOML surface
    /// produces, over the SQLite default meta. `allow_plaintext` is false:
    /// a LAN-minio test target constructs the struct directly (as the test
    /// harness does).
    pub fn s3(connect: S3Connect) -> BackendConfig {
        BackendConfig {
            meta: MetaKind::Sqlite,
            blob: BlobKind::S3,
            layout: Layout::Single,
            pg: None,
            s3: Some(connect),
            allow_plaintext: false,
            gc_interval_secs: None,
            gc_grace_secs: None,
            sslrootcert: None,
        }
    }

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
            layout: Layout::Single,
            pg: Some(PgConnect {
                host: parsed.host,
                port: parsed.port,
                dbname: parsed.dbname,
                user: user.or(parsed.user),
                password,
                tls: parsed.tls,
                sslrootcert: None,
                schema: None,
            }),
            s3: None,
            allow_plaintext,
            gc_interval_secs: None,
            gc_grace_secs: None,
            sslrootcert: None,
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
    // Top level: [backend] and (M13a) the optional [layout] table.
    for key in table.keys() {
        if key != "backend" && key != "layout" {
            return Err(BackendError::UnknownKey {
                key: format!("[{key}]"),
                allowed: "[backend], [layout]".to_string(),
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

    const ALLOWED: &[&str] = &[
        "meta",
        "blob",
        "dsn",
        "user",
        "password",
        "sslrootcert",
        "allow_plaintext",
        "endpoint",
        "bucket",
        "region",
        "access_key_id",
        "secret",
        "path_style",
        "gc_interval_secs",
        "gc_grace_secs",
    ];
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
            Some("s3") => BlobKind::S3,
            _ => {
                return Err(BackendError::BadValue {
                    key: "backend.blob".to_string(),
                    want: "\"fs\" or \"s3\"".to_string(),
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
    // ---- [layout] table (M13a) ---------------------------------------------
    // Optional; an absent table is the zero-config `single-db`. The mode
    // key is the only key, and `db-per-repo` is SQLite-only: the per-repo
    // files are SQLite, so a Postgres metadata backend never splits.
    let layout = match table.get("layout") {
        None => Layout::Single,
        Some(v) => {
            let t = v.as_table().ok_or(BackendError::BadValue {
                key: "[layout]".to_string(),
                want: "a table".to_string(),
            })?;
            const LAYOUT_ALLOWED: &[&str] = &["mode"];
            for key in t.keys() {
                if !LAYOUT_ALLOWED.contains(&key.as_str()) {
                    return Err(BackendError::UnknownKey {
                        key: format!("layout.{key}"),
                        allowed: LAYOUT_ALLOWED.join(", "),
                    });
                }
            }
            match t.get("mode") {
                None => Layout::Single,
                Some(v) => {
                    let s = v.as_str().ok_or(BackendError::BadValue {
                        key: "layout.mode".to_string(),
                        want: "\"single-db\" or \"db-per-repo\"".to_string(),
                    })?;
                    Layout::parse(s).ok_or(BackendError::BadValue {
                        key: "layout.mode".to_string(),
                        want: "\"single-db\" or \"db-per-repo\"".to_string(),
                    })?
                }
            }
        }
    };
    if layout == Layout::DbPerRepo && meta == MetaKind::Postgres {
        return Err(BackendError::LayoutNeedsSqlite);
    }
    // The M10a GC knobs: seconds; 0 for the interval switches the serve-side
    // background task off (the host-local `naatre gc` verb still runs).
    let gc_interval_secs = match backend.get("gc_interval_secs") {
        None => None,
        Some(v) => Some(u64_key(v, "backend.gc_interval_secs")?),
    };
    let gc_grace_secs = match backend.get("gc_grace_secs") {
        None => None,
        Some(v) => Some(u64_key(v, "backend.gc_grace_secs")?),
    };

    let file_dsn = str_key(backend, "dsn")?;
    let has_file_dsn = file_dsn.is_some();
    // The file wins over the environment — the SAME precedence every other
    // key follows (the S3 keys below: "(The file wins over the
    // environment, same as the Postgres keys.)"). Env-wins made a file's
    // dsn a lie whenever NAATRE_PG_DSN happened to be set.
    let dsn = file_dsn
        .clone()
        .unwrap_or_else(|| std::env::var(ENV_PG_DSN).unwrap_or_default());
    let file_user = str_key(backend, "user")?;
    let file_password = str_key(backend, "password")?;
    // The `verify-ca`/`verify-full` CA bundle (a private-CA Postgres is
    // the shape the platform roots cannot verify). File-only — no env
    // var: a trust anchor is a deployment decision, not an injection
    // point.
    let sslrootcert = str_key(backend, "sslrootcert")?;

    let pg = if meta == MetaKind::Postgres {
        let source = if has_file_dsn {
            PgDsnSource::File
        } else if std::env::var(ENV_PG_DSN).is_ok() {
            PgDsnSource::Env
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
            sslrootcert: sslrootcert.clone().map(std::path::PathBuf::from),
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
        if sslrootcert.is_some() {
            return Err(BackendError::KeyWithoutPostgres {
                key: "sslrootcert".to_string(),
            });
        }
        None
    };

    // ---- S3 keys ------------------------------------------------------------
    // (The file wins over the environment, same as the Postgres keys.)
    let file_endpoint = str_key(backend, "endpoint")?;
    let file_bucket = str_key(backend, "bucket")?;
    let file_region = str_key(backend, "region")?;
    let file_access_key_id = str_key(backend, "access_key_id")?;
    let file_secret = str_key(backend, "secret")?;
    let path_style = match backend.get("path_style") {
        None => false,
        Some(v) => v.as_bool().ok_or(BackendError::BadValue {
            key: "backend.path_style".to_string(),
            want: "true or false".to_string(),
        })?,
    };

    let s3 = if blob == BlobKind::S3 {
        // Values that only make sense for s3 are REFUSED under blob = fs —
        // a bucket key sitting unused in the file is a misconfiguration,
        // not an inert one (same rule as dsn under sqlite).
        let s3 = S3Connect {
            endpoint: file_endpoint
                .or_else(|| std::env::var(ENV_S3_ENDPOINT).ok())
                .map(|e| parse_endpoint(&e))
                .transpose()?,
            bucket: file_bucket
                .or_else(|| std::env::var(ENV_S3_BUCKET).ok())
                .ok_or(BackendError::S3KeyMissing {
                    key: "bucket",
                    env: ENV_S3_BUCKET,
                })?,
            region: file_region.or_else(|| std::env::var(ENV_S3_REGION).ok()),
            access_key_id: file_access_key_id
                .or_else(|| std::env::var(ENV_S3_ACCESS_KEY_ID).ok())
                .ok_or(BackendError::S3KeyMissing {
                    key: "access_key_id",
                    env: ENV_S3_ACCESS_KEY_ID,
                })?,
            secret: file_secret
                .or_else(|| std::env::var(ENV_S3_SECRET).ok())
                .ok_or(BackendError::S3KeyMissing {
                    key: "secret",
                    env: ENV_S3_SECRET,
                })?,
            path_style,
        };
        validate_bucket(&s3.bucket)?;
        Some(s3)
    } else {
        for (present, key) in [
            (
                file_endpoint.is_some() || std::env::var(ENV_S3_ENDPOINT).is_ok(),
                "endpoint",
            ),
            (
                file_bucket.is_some() || std::env::var(ENV_S3_BUCKET).is_ok(),
                "bucket",
            ),
            (
                file_region.is_some() || std::env::var(ENV_S3_REGION).is_ok(),
                "region",
            ),
            (
                file_access_key_id.is_some() || std::env::var(ENV_S3_ACCESS_KEY_ID).is_ok(),
                "access_key_id",
            ),
            (
                file_secret.is_some() || std::env::var(ENV_S3_SECRET).is_ok(),
                "secret",
            ),
            (backend.contains_key("path_style"), "path_style"),
        ] {
            if present {
                return Err(BackendError::KeyWithoutBlobS3 {
                    key: key.to_string(),
                });
            }
        }
        None
    };

    Ok(BackendConfig {
        meta,
        blob,
        layout,
        pg,
        s3,
        allow_plaintext,
        gc_interval_secs,
        gc_grace_secs,
        sslrootcert: sslrootcert.map(std::path::PathBuf::from),
    })
}

/// An optional non-negative integer config value (`u64`; seconds).
fn u64_key(v: &toml::Value, key: &str) -> Result<u64, BackendError> {
    v.as_integer()
        .and_then(|i| u64::try_from(i).ok())
        .ok_or(BackendError::BadValue {
            key: key.to_string(),
            want: "a non-negative integer (seconds)".to_string(),
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

/// Validates the `endpoint` shape: a BARE `host[:port]` authority. The
/// scheme is never written in the file (HTTPS is the default; plain HTTP
/// is what `allow_plaintext` opts into), so anything URL-flavored is
/// refused by rule, never echoed back.
fn parse_endpoint(raw: &str) -> Result<String, BackendError> {
    let refuse = |why: &str| BackendError::EndpointInvalid {
        why: why.to_string(),
    };
    if raw.is_empty() || raw.chars().any(|c| c.is_whitespace()) {
        return Err(refuse(
            "endpoint must be a bare host[:port] (no scheme, no path, no spaces)",
        ));
    }
    if raw.contains("://") {
        return Err(refuse(
            "endpoint takes a bare host[:port] — no scheme: https is the default, \
             and plain HTTP is chosen by the allow_plaintext opt-in",
        ));
    }
    if raw.contains('@') {
        return Err(refuse(
            "endpoint takes a bare host[:port] — no userinfo; access keys go in \
             the access_key_id/secret keys or NAATRE_S3_ACCESS_KEY_ID/NAATRE_S3_SECRET",
        ));
    }
    if raw.contains(['/', '?', '#']) {
        return Err(refuse(
            "endpoint takes a bare host[:port] — no path, query, or fragment",
        ));
    }
    // host[:port] — the port side must be numeric (bracketed IPv6 hosts
    // skip the port check; the driver owns the URL build).
    if !raw.starts_with('[')
        && let Some(colon) = raw.rfind(':')
    {
        let p = &raw[colon + 1..];
        if p.is_empty() || !p.bytes().all(|b| b.is_ascii_digit()) {
            return Err(refuse("the port must be a number"));
        }
    }
    Ok(raw.to_string())
}

/// S3 bucket naming: `[a-z0-9][a-z0-9-]{1,62}` — the modern S3 grammar
/// (no dots), applied to the config key too. Errors name the rule, never
/// the value.
fn validate_bucket(bucket: &str) -> Result<(), BackendError> {
    let ok = bucket.len() >= 3
        && bucket.len() <= 63
        && bucket
            .chars()
            .next()
            .is_some_and(|c| c.is_ascii_lowercase() || c.is_ascii_digit())
        && bucket
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-');
    if ok {
        Ok(())
    } else {
        Err(BackendError::BadValue {
            key: "backend.bucket".to_string(),
            want: "3-63 chars: lowercase letters, digits, hyphens (starting with \
                   a letter or digit)"
                .to_string(),
        })
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
        "backend.toml key {key:?} only applies when blob = \"s3\"; \
         remove it or switch the blob backend"
    )]
    KeyWithoutBlobS3 { key: String },

    #[error(
        "backend.toml key {key:?} must be set when blob = \"s3\" \
         (the file key or the {env} environment variable)"
    )]
    S3KeyMissing {
        key: &'static str,
        env: &'static str,
    },

    #[error("backend endpoint: {why}")]
    EndpointInvalid { why: String },

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

    #[error(
        "data dir blob store is {on_disk} but the backend config selects {configured}: \
         refusing to open (a data dir never switches backends in place; an fs-CAS \
         data dir is NOT auto-migrated — start a fresh data dir or migrate the \
         objects out-of-band, see the README's backend section)"
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

    #[error(
        "layout mode \"db-per-repo\" requires meta = \"sqlite\": the per-repo \
         files are SQLite, so a Postgres metadata backend never splits"
    )]
    LayoutNeedsSqlite,

    #[error(
        "data dir layout is {on_disk} but the config selects {configured}: \
         refusing to open — {remedy}"
    )]
    LayoutMismatch {
        on_disk: &'static str,
        configured: &'static str,
        remedy: &'static str,
    },
}

/// The mismatch guard (plan M9): refuses a data dir whose existing
/// metadata store or blob CAS disagrees with the configured backend,
/// naming both sides.
///
/// * `naatre.db` present + `meta = postgres` → [`BackendError::Mismatch`].
/// * `meta = sqlite` + `naatre.db` absent → the standard
///   `DataDirUninitialized` (run `naatre init`).
/// * `meta = postgres` + uninitialized server → checked at connect time by
///   `init` (a schema present there is `AlreadyInitialized`).
/// * an fs CAS with DATA (`objects/` holds at least one shard) +
///   `blob = s3` → [`BackendError::Mismatch`] — an fs-CAS data dir is NOT
///   auto-migrated into a bucket (M9b); the operator starts a fresh data
///   dir or migrates out-of-band. An EMPTY `objects/` (created by a mere
///   open) is not a marker and passes.
/// * layout (M13a): `layout.mode = "db-per-repo"` requires `catalog.db`
///   (run `naatre migrate-db-per-repo` first — the config flip without the
///   migration is refused); `single-db` (the default) refuses a data dir
///   where `catalog.db` exists — the migration ran and the config was not
///   flipped. A stale `naatre.db` BESIDE a migrated catalog is accepted:
///   the cutover steps leave the original single DB in place for the
///   operator to delete after verifying the split serves.
///
/// The reverse (a bucket holding data + `blob = fs`) cannot be checked
/// locally: there is no on-disk marker for a remote bucket, and the bucket
/// may legitimately belong to a different deployment — the README's
/// erasure enumeration covers what that means for an operator who points a
/// fresh data dir at a used bucket.
pub fn check_data_dir_backend(data: &Path, cfg: &BackendConfig) -> Result<(), StoreError> {
    let db = data.join(crate::data_dir::DB_FILE);
    match cfg.meta {
        MetaKind::Sqlite => {
            // The layout guards run FIRST (M13a): post-cutover a
            // db-per-repo data dir has no naatre.db, so the
            // single-DB-presence rule below must not fire there.
            let on_disk = crate::layout::detect(data);
            match (cfg.layout, on_disk) {
                (Layout::DbPerRepo, Layout::Single) => {
                    return Err(crate::layout::layout_mismatch(on_disk, cfg.layout));
                }
                (Layout::Single, Layout::DbPerRepo) => {
                    return Err(crate::layout::layout_mismatch(on_disk, cfg.layout));
                }
                // db-per-repo on disk: catalog.db present; naatre.db may
                // sit beside it (pre-cutover, the migrate tool leaves it)
                // or be gone (post-cutover) — both open.
                (Layout::DbPerRepo, Layout::DbPerRepo) => {}
                (Layout::Single, Layout::Single) => {
                    if !db.exists() {
                        return Err(StoreError::DataDirUninitialized(data.to_path_buf()));
                    }
                }
            }
        }
        MetaKind::Postgres => {
            if db.exists() {
                return Err(StoreError::Backend(BackendError::Mismatch {
                    on_disk: "sqlite (naatre.db present)",
                    configured: "postgres",
                }));
            }
        }
    }
    if cfg.blob == BlobKind::S3 && fs_cas_has_data(data) {
        return Err(StoreError::Backend(BackendError::Mismatch {
            on_disk: "fs (objects/ has data)",
            configured: "s3",
        }));
    }
    Ok(())
}

/// True when `objects/` holds at least one entry — the on-disk marker of
/// an fs CAS that has actually stored something (a bare `objects/` dir is
/// created by opening the store; it marks nothing).
fn fs_cas_has_data(data: &Path) -> bool {
    data.join(crate::data_dir::OBJECTS_DIR)
        .read_dir()
        .map(|mut entries| entries.next().is_some())
        .unwrap_or(false)
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
    fn sslrootcert_key_parses_into_pg_and_is_refused_under_sqlite() {
        let cfg = parse(
            "[backend]\nmeta = \"postgres\"\ndsn = \"postgres://db.example/naatre?sslmode=verify-full\"\nsslrootcert = \"/etc/naatre/ca.pem\"\n",
        )
        .unwrap();
        let pg = cfg.pg.unwrap();
        assert_eq!(
            pg.sslrootcert.as_deref(),
            Some(std::path::Path::new("/etc/naatre/ca.pem"))
        );
        // PG-only, like user/password: sitting unused under sqlite is a
        // misconfiguration, not an inert key.
        let e = err_of("[backend]\nsslrootcert = \"/etc/naatre/ca.pem\"\n");
        assert!(
            matches!(e, BackendError::KeyWithoutPostgres { ref key } if key == "sslrootcert"),
            "{e}"
        );
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
    fn minimal_s3_file_parses() {
        let cfg = parse(
            "[backend]\nblob = \"s3\"\nbucket = \"naatre-test\"\nendpoint = \"127.0.0.1:9000\"\nregion = \"us-east-1\"\naccess_key_id = \"naatre\"\nsecret = \"naatre-secret\"\npath_style = true\nallow_plaintext = true\n",
        )
        .unwrap();
        let s3 = cfg.s3.unwrap();
        assert_eq!(cfg.blob, BlobKind::S3);
        assert_eq!(s3.endpoint.as_deref(), Some("127.0.0.1:9000"));
        assert_eq!(s3.bucket, "naatre-test");
        assert_eq!(s3.region.as_deref(), Some("us-east-1"));
        assert_eq!(s3.access_key_id, "naatre");
        assert_eq!(s3.secret, "naatre-secret");
        assert!(s3.path_style);
        // Default meta rides along: the backends select independently.
        assert_eq!(cfg.meta, MetaKind::Sqlite);
    }

    #[test]
    fn s3_defaults_and_aws_shape() {
        // No endpoint = AWS defaults; path_style defaults to false
        // (virtual-host style); region is optional.
        let cfg = parse(
            "[backend]\nblob = \"s3\"\nbucket = \"naatre-test\"\naccess_key_id = \"AKIAEXAMPLE\"\nsecret = \"s3cret\"\n",
        )
        .unwrap();
        let s3 = cfg.s3.unwrap();
        assert_eq!(s3.endpoint, None);
        assert_eq!(s3.region, None);
        assert!(!s3.path_style);
        // Debug redacts the secret only.
        let dbg = format!("{s3:?}");
        assert!(!dbg.contains("s3cret"), "secret leaked in Debug: {dbg}");
        assert!(dbg.contains("<redacted>"));
    }

    #[test]
    fn s3_missing_required_keys_refused_naming_remedy() {
        let e = err_of("[backend]\nblob = \"s3\"\n");
        assert!(matches!(
            e,
            BackendError::S3KeyMissing {
                key: "bucket",
                env: "NAATRE_S3_BUCKET"
            }
        ));
        let msg = e.to_string();
        assert!(msg.contains("NAATRE_S3_BUCKET"), "{msg}");
        let e = err_of("[backend]\nblob = \"s3\"\nbucket = \"naatre-test\"\n");
        assert!(matches!(
            e,
            BackendError::S3KeyMissing {
                key: "access_key_id",
                ..
            }
        ));
        let e =
            err_of("[backend]\nblob = \"s3\"\nbucket = \"naatre-test\"\naccess_key_id = \"a\"\n");
        assert!(matches!(
            e,
            BackendError::S3KeyMissing { key: "secret", .. }
        ));
    }

    #[test]
    fn s3_endpoint_shapes_refused_by_rule_not_value() {
        for (bad, why) in [
            ("http://10.0.0.5:9000", "no scheme"),
            ("https://minio.lan", "no scheme"),
            ("minio@host:9000", "no userinfo"),
            ("u:p@host:9000", "no userinfo"),
            ("host:9000/", "no path"),
            ("host:9000?x=1", "no path"),
            ("host:notaport", "port must be a number"),
            ("", "bare host[:port]"),
        ] {
            let e = err_of(&format!(
                "[backend]\nblob = \"s3\"\nbucket = \"naatre-test\"\naccess_key_id = \"a\"\nsecret = \"s\"\nendpoint = \"{bad}\"\n"
            ));
            assert!(
                matches!(e, BackendError::EndpointInvalid { .. }),
                "{bad:?}: {e}"
            );
            let msg = e.to_string();
            assert!(
                !msg.contains(bad) || bad.is_empty(),
                "endpoint value echoed: {msg}"
            );
            let _ = why; // shape notes only
        }
        // The bracketed IPv6 literal with a port passes the shape check.
        let cfg = parse(
            "[backend]\nblob = \"s3\"\nbucket = \"naatre-test\"\naccess_key_id = \"a\"\nsecret = \"s\"\nendpoint = \"[::1]:9000\"\n",
        )
        .unwrap();
        assert_eq!(cfg.s3.unwrap().endpoint.as_deref(), Some("[::1]:9000"));
    }

    #[test]
    fn s3_bucket_grammar_enforced() {
        let long = "a".repeat(64);
        for bad in ["ab", "UPPER", "with.dot", "-leading", long.as_str()] {
            let e = err_of(&format!(
                "[backend]\nblob = \"s3\"\nbucket = \"{bad}\"\naccess_key_id = \"a\"\nsecret = \"s\"\n"
            ));
            assert!(
                matches!(e, BackendError::BadValue { ref key, .. } if key == "backend.bucket"),
                "{bad:?}: {e}"
            );
            // The refused value is never echoed.
            assert!(!e.to_string().contains(bad), "bucket value echoed: {e}");
        }
    }

    #[test]
    fn s3_keys_refused_under_blob_fs() {
        for (key, value) in [
            ("endpoint", "\"host:9000\""),
            ("bucket", "\"naatre-test\""),
            ("region", "\"us-east-1\""),
            ("access_key_id", "\"a\""),
            ("secret", "\"s\""),
            ("path_style", "true"),
        ] {
            let e = err_of(&format!("[backend]\nblob = \"fs\"\n{key} = {value}\n"));
            assert!(
                matches!(e, BackendError::KeyWithoutBlobS3 { key: ref k } if *k == *key),
                "{key}: {e}"
            );
        }
        // A single well-formed s3 file under fs is refused the same way.
        let e = err_of("[backend]\nblob = \"fs\"\nbucket = \"naatre-test\"\n");
        assert!(matches!(e, BackendError::KeyWithoutBlobS3 { key: ref k } if k == "bucket"));
    }

    // NOTE: the env-fallback rule (NAATRE_S3_* consulted when the file
    // does not carry a key; the file wins) is pinned in
    // tests/backend_env_test.rs — env mutation is process-global, so it
    // must not run in this binary beside the parallel parse tests.

    #[test]
    fn mismatch_guard_refuses_fs_data_dir_switched_to_s3() {
        // The real shape: a sqlite+fs data dir (naatre.db AND objects/)
        // whose operator switches blob to s3 in backend.toml.
        let dir = tempfile::tempdir().unwrap();
        let data = dir.path().join("data");
        std::fs::create_dir_all(data.join("objects/ab")).unwrap();
        std::fs::write(data.join("objects/ab/cd"), b"a shard dir marks data").unwrap();
        std::fs::write(data.join(crate::data_dir::DB_FILE), b"").unwrap();
        let cfg = BackendConfig::s3(S3Connect {
            endpoint: Some("127.0.0.1:9000".into()),
            bucket: "naatre-test".into(),
            region: None,
            access_key_id: "a".into(),
            secret: "s".into(),
            path_style: true,
        });
        let e = check_data_dir_backend(&data, &cfg).unwrap_err();
        let msg = e.to_string();
        assert!(msg.contains("fs") && msg.contains("s3"), "{msg}");
        assert!(msg.contains("NOT auto-migrated"), "{msg}");
        // A bare objects/ dir (created by a mere open) is not a marker.
        let bare = dir.path().join("bare");
        std::fs::create_dir_all(bare.join("objects")).unwrap();
        std::fs::write(bare.join(crate::data_dir::DB_FILE), b"").unwrap();
        assert!(check_data_dir_backend(&bare, &cfg).is_ok());
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
