//! `naatre` — one binary, multi-call (plan "Client"): host-local admin verbs
//! (`init`, `serve`, `token --new-admin`) that run directly against the
//! 0700 data dir, and thin HTTP client verbs (`repo`, `token` subcommands)
//! that talk to a running server via `NAATRE_URL` + `NAATRE_TOKEN`.

mod client;
mod host;

use std::path::PathBuf;

use clap::{Parser, Subcommand};

/// naatre: a self-hosted, single-binary Scion-style VCS.
#[derive(Parser)]
#[command(
    name = "naatre",
    version,
    about = "naatre: self-hosted single-binary VCS (server + thin client)",
    after_help = "Client verbs read NAATRE_URL (default http://127.0.0.1:7400) and NAATRE_TOKEN."
)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Create a data dir, run migrations, mint the first admin token.
    Init {
        /// Data directory to create (refuses an already-initialized dir).
        #[arg(long)]
        data: PathBuf,
    },
    /// Serve the API from a data dir (Ctrl-C / SIGTERM drains gracefully).
    Serve {
        /// Existing data directory (uninitialized → run naatre init first).
        #[arg(long)]
        data: PathBuf,
        /// Listen address; non-loopback warns that bearer tokens cross the
        /// network in cleartext unless fronted by TLS.
        #[arg(long, default_value = "127.0.0.1:7400")]
        listen: String,
        /// Ops log level (access logs are debug-level: off by default).
        #[arg(long, default_value = "info")]
        log_level: String,
    },
    /// Token verbs: host-local mint (`--data --new-admin`) or HTTP client
    /// subcommands (list / create / revoke) against a running server.
    Token {
        /// Data directory for the host-local `--new-admin` mint.
        #[arg(long)]
        data: Option<PathBuf>,
        /// Mint a new admin token for the "admin" principal, host-local
        /// (trust boundary: the 0700 data dir). Requires --data.
        #[arg(long)]
        new_admin: bool,
        #[command(subcommand)]
        cmd: Option<TokenCmd>,
    },
    /// Repo verbs (HTTP client, admin).
    Repo {
        #[command(subcommand)]
        cmd: RepoCmd,
    },
    /// Import a LOCAL git repository into a naatre repo (one-way, offline,
    /// under the data dir flock; refuses URLs).
    Import {
        /// Existing data directory (uninitialized → run naatre init first).
        #[arg(long)]
        data: PathBuf,
        /// Target repo name (defaults to the git dir's basename, sanitized
        /// to ^[a-z0-9][a-z0-9-]{0,62}$).
        #[arg(long)]
        into: Option<String>,
        /// Skip unsupported commits (and cascade to their descendants)
        /// instead of aborting the whole import.
        #[arg(long)]
        skip_unsupported: bool,
        /// Path of the local git repository to import.
        git_path: String,
    },
}

#[derive(Subcommand)]
enum TokenCmd {
    /// List active tokens (admin; revoked tokens drop out).
    List,
    /// Mint a token for a principal (admin). Prints the token once.
    Create {
        /// Principal name (created or re-roled to --role).
        principal: String,
        /// Role: admin, writer, or reader.
        #[arg(long)]
        role: String,
        /// Token lifetime in days (omit for never-expiring).
        #[arg(long)]
        ttl_days: Option<u64>,
        /// Segment-aligned path scope (no trailing slash).
        #[arg(long)]
        scope: Option<String>,
    },
    /// Revoke a token by id (admin; idempotent).
    Revoke {
        /// Numeric token id (see `token list`).
        id: u64,
    },
}

#[derive(Subcommand)]
enum RepoCmd {
    /// Create a repo (admin).
    Create {
        /// Repo name, ^[a-z0-9][a-z0-9-]{0,62}$.
        name: String,
    },
    /// Repo counters: changesets, objects, bytes, cbytes (admin).
    Stats {
        /// Repo name.
        name: String,
    },
}

fn main() {
    let cli = Cli::parse();
    let code = match cli.cmd {
        Cmd::Init { data } => host::init(&data),
        Cmd::Serve {
            data,
            listen,
            log_level,
        } => host::serve(&data, &listen, &log_level),
        Cmd::Token {
            data,
            new_admin,
            cmd,
        } => match (new_admin, data, cmd) {
            (true, Some(data), None) => host::new_admin_token(&data),
            (true, None, _) => usage("token --new-admin requires --data <dir>"),
            (true, Some(_), Some(_)) => usage("token --new-admin takes no subcommand"),
            (false, _, None) => {
                usage("token requires --data <dir> --new-admin (host-local) or a subcommand (HTTP)")
            }
            (false, _, Some(cmd)) => client::token(cmd),
        },
        Cmd::Repo { cmd } => client::repo(cmd),
        Cmd::Import {
            data,
            into,
            skip_unsupported,
            git_path,
        } => host::import(&data, into.as_deref(), skip_unsupported, &git_path),
    };
    std::process::exit(code);
}

fn usage(message: &str) -> i32 {
    eprintln!("error: {message}");
    2
}
