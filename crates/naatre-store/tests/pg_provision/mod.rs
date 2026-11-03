//! Test-only Postgres provisioning (M9a): the PG legs run against a REAL
//! server wherever one can be produced —
//!
//! 1. `NAATRE_TEST_PG_DSN` set → use that server as-is (CI's service
//!    container; password via `NAATRE_TEST_PG_PASSWORD`);
//! 2. unset → PROVISION a throwaway cluster locally: locate a PostgreSQL
//!    ≥ 16 server installation (server binary required — client-only
//!    installs like Homebrew's libpq are correctly refused), `initdb` a
//!    cluster in a temp dir, start it on an ephemeral loopback port, and
//!    tear it down on drop;
//! 3. neither possible → the caller prints a skip notice (the only
//!    legitimate skip: no binaries).
//!
//! Binaries present but broken (initdb fails) is NOT a skip — it panics,
//! loudly. Trust: the cluster binds 127.0.0.1 only, `trust` auth, lives
//! under a private temp dir, and dies with the test process.

use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use naatre_store::backend::BackendConfig;

const MIN_MAJOR: u32 = 16;

/// Per-test schema names: pid + a process-local counter + nanos, so
/// concurrent tests in one process can never collide (a collision shows up
/// as a catalog race on CREATE TABLE) and cross-process runs stay distinct.
static SCHEMA_SEQ: AtomicU64 = AtomicU64::new(0);

pub fn fresh_schema() -> String {
    let n = SCHEMA_SEQ.fetch_add(1, Ordering::Relaxed);
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos();
    format!("naatre_t_{}_{n}_{nanos}", std::process::id())
}

/// A live test server. `Drop` stops the provisioned cluster (external
/// servers are left alone).
pub enum PgTarget {
    External { dsn: String },
    Provisioned(PgCluster),
}

pub struct PgCluster {
    dsn: String,
    bin: PathBuf,
    data: PathBuf,
    /// The temp dir holding data/ + logs; removed on drop.
    dir: Option<tempfile::TempDir>,
}

impl PgTarget {
    pub fn dsn(&self) -> &str {
        match self {
            PgTarget::External { dsn } => dsn,
            PgTarget::Provisioned(c) => &c.dsn,
        }
    }

    /// The BackendConfig for one test: strict DSN parse (password via
    /// `NAATRE_TEST_PG_PASSWORD` when external), plaintext allowed (CI's
    /// service and the throwaway cluster have no TLS), fresh per-test
    /// schema.
    pub fn config(&self) -> BackendConfig {
        let password = std::env::var("NAATRE_TEST_PG_PASSWORD").ok();
        let mut cfg = BackendConfig::postgres(self.dsn(), None, password, true)
            .expect("test DSN parses (password rides in its own variable)");
        cfg.pg.as_mut().unwrap().schema = Some(fresh_schema());
        // The TLS legs (F1 CI): the DSN carries sslmode, the CA bundle
        // rides in its own variable — the same shape as the password.
        if let Ok(rootcert) = std::env::var("NAATRE_TEST_PG_SSLROOTCERT") {
            cfg.pg.as_mut().unwrap().sslrootcert = Some(rootcert.into());
        }
        cfg
    }
}

impl Drop for PgCluster {
    fn drop(&mut self) {
        let _ = std::process::Command::new(self.bin.join("pg_ctl"))
            .arg("-D")
            .arg(&self.data)
            .args(["stop", "-m", "fast", "-w"])
            .output();
        if let Some(dir) = self.dir.take() {
            let _ = dir.close(); // best-effort: /tmp cleans up anyway
        }
    }
}

/// The test server: the env DSN, or a provisioned throwaway, or `None`
/// (skip — no usable PostgreSQL installation).
pub fn acquire() -> Option<PgTarget> {
    if let Ok(dsn) = std::env::var("NAATRE_TEST_PG_DSN") {
        return Some(PgTarget::External { dsn });
    }
    let bin = find_server_dir()?;
    Some(PgTarget::Provisioned(provision(&bin)))
}

/// Locates a directory holding a REAL server (postgres + initdb + pg_ctl)
/// of major version ≥ 16. Returns `None` (with a notice) when absent.
fn find_server_dir() -> Option<PathBuf> {
    let mut candidates: Vec<PathBuf> = Vec::new();
    if let Ok(from_env) = std::env::var("NAATRE_TEST_PG_BIN") {
        candidates.push(from_env.into());
    }
    if let Ok(path) = std::env::var("PATH") {
        for p in std::env::split_paths(&path) {
            candidates.push(p);
        }
    }
    // Common install layouts the PATH search misses (versioned kegs and
    // deb-style dirs).
    fn keg_name(name: &str) -> bool {
        name.starts_with("postgresql@") || name == "postgresql"
    }
    fn deb_name(name: &str) -> bool {
        name.chars().next().is_some_and(|c| c.is_ascii_digit())
    }
    type NameFilter = fn(&str) -> bool;
    let layouts: [(&str, NameFilter); 2] = [
        ("/opt/homebrew/opt", keg_name),
        ("/usr/lib/postgresql", deb_name),
    ];
    for (base, filter) in layouts {
        let Ok(entries) = std::fs::read_dir(base) else {
            continue;
        };
        for e in entries.flatten() {
            if filter(&e.file_name().to_string_lossy()) {
                candidates.push(e.path().join("bin"));
            }
        }
    }
    for dir in candidates {
        if dir.join("postgres").is_file()
            && dir.join("initdb").is_file()
            && dir.join("pg_ctl").is_file()
            && pg_ctl_major(&dir).is_some_and(|major| major >= MIN_MAJOR)
        {
            return Some(dir);
        }
    }
    eprintln!(
        "skip: PG legs need NAATRE_TEST_PG_DSN, or a PostgreSQL >= {MIN_MAJOR} server \
         installation (postgres + initdb + pg_ctl in one bin dir; client-only installs \
         such as Homebrew's libpq do not count — set NAATRE_TEST_PG_BIN to point at one)"
    );
    None
}

fn pg_ctl_major(dir: &Path) -> Option<u32> {
    let out = std::process::Command::new(dir.join("pg_ctl"))
        .arg("--version")
        .output()
        .ok()?;
    let text = String::from_utf8_lossy(&out.stdout);
    // "pg_ctl (PostgreSQL) 18.6 (Homebrew)" — the version token is the
    // FIRST digit-led one, wherever the vendor tag sits.
    text.split_whitespace()
        .find(|t| t.chars().next().is_some_and(|c| c.is_ascii_digit()))?
        .split('.')
        .next()?
        .parse()
        .ok()
}

/// Bootstraps and starts a throwaway cluster. Binaries present but broken
/// is a panic, not a skip.
fn provision(bin: &Path) -> PgCluster {
    let dir = tempfile::tempdir().expect("temp dir for the throwaway cluster");
    let data = dir.path().join("data");

    let initdb = || {
        std::process::Command::new(bin.join("initdb"))
            .args(["-U", "naatre", "-A", "trust"])
            .arg("-D")
            .arg(&data)
            .env_remove("PGDATA")
            .output()
    };
    let out = initdb().expect("initdb spawns");
    if !out.status.success() {
        eprintln!("initdb failed; retrying with an explicit -L share dir");
        // Keg-only installs keep their share files outside initdb's
        // compiled-in path.
        for share in [
            bin.parent().map(|p| p.join("share/postgresql")),
            bin.parent().map(|p| p.join("share")),
        ]
        .into_iter()
        .flatten()
        {
            if share.join("postgres.bki").is_file() {
                let out = std::process::Command::new(bin.join("initdb"))
                    .args(["-L"])
                    .arg(&share)
                    .args(["-U", "naatre", "-A", "trust"])
                    .arg("-D")
                    .arg(&data)
                    .env_remove("PGDATA")
                    .output()
                    .expect("initdb spawns");
                if out.status.success() {
                    eprintln!("initdb ok via -L {}", share.display());
                    break;
                }
                eprintln!(
                    "initdb failed (share dir {}): {}",
                    share.display(),
                    String::from_utf8_lossy(&out.stderr).trim()
                );
            }
        }
        if !data.join("PG_VERSION").exists() {
            panic!(
                "initdb could not bootstrap a cluster with the binaries at {} — \
                 the PG legs must RUN when a server installation exists",
                bin.display()
            );
        }
    }

    // Ephemeral port: bind :0, read the port, release (a tiny race window,
    // acceptable for a throwaway test cluster).
    let port = TcpListener::bind("127.0.0.1:0")
        .expect("ephemeral port bind")
        .local_addr()
        .unwrap()
        .port();

    let start = std::process::Command::new(bin.join("pg_ctl"))
        .arg("-D")
        .arg(&data)
        .arg("-o")
        .arg(format!(
            "-p {port} -c listen_addresses=127.0.0.1 -k {}",
            dir.path().display()
        ))
        .arg("-l")
        .arg(dir.path().join("pg.log"))
        .args(["start", "-w"])
        .output()
        .expect("pg_ctl spawns");
    if !start.status.success() {
        panic!(
            "pg_ctl start failed: {}",
            String::from_utf8_lossy(&start.stderr).trim()
        );
    }
    let dsn = format!("postgres://naatre@127.0.0.1:{port}/postgres?sslmode=disable");
    eprintln!(
        "provisioned throwaway PostgreSQL at 127.0.0.1:{port} ({})",
        bin.display()
    );
    PgCluster {
        dsn,
        bin: bin.to_path_buf(),
        data,
        dir: Some(dir),
    }
}
