//! `naatre` — one binary, multi-call (plan "Client"): host-local admin verbs
//! (`init`, `serve`, `token --new-admin`, `import`) that run directly
//! against the 0700 data dir, thin HTTP client verbs (`repo`, `token`
//! subcommands), the working-copy verbs (`clone`, `status`, `sparse`,
//! `sync`, `commit`, `cat`, `ls`, `diff`, `doctor`, `login`, `snapshot`,
//! `restore`, `mcp`), and the mount daemon (`mount`, M8).
//!
//! Pinned exit codes across client verbs: 0 success, 1 generic error,
//! 2 conflict-class (409 envelopes), 3 usage.

use std::path::PathBuf;

use clap::{Args, Parser, Subcommand};

use naatre_cli::verbs::{self, Net};
use naatre_cli::wc::lock as verbs_lock;
use naatre_cli::{client, host};

/// naatre: a self-hosted, single-binary Scion-style VCS.
#[derive(Parser)]
#[command(
    name = "naatre",
    version,
    about = "naatre: self-hosted single-binary VCS (server + thin client)",
    after_help = "Client verbs read NAATRE_URL (default http://127.0.0.1:7400) and NAATRE_TOKEN; \
                  inside a working copy, .naatre/config.toml is the fallback."
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
    /// Principal verbs (HTTP client, admin).
    Principal {
        #[command(subcommand)]
        cmd: PrincipalCmd,
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
    /// Write this working copy's .naatre/config.toml (server_url + token).
    Login {
        /// Server origin, e.g. http://127.0.0.1:7400.
        #[arg(long)]
        server: String,
        /// Bearer token (argv is visible in ps; prefer NAATRE_TOKEN).
        #[arg(long)]
        token: String,
    },
    /// Clone a repo into a new working copy.
    Clone {
        /// http://host[:port]/<repo> or just <repo> (uses NAATRE_URL).
        url: String,
        /// Materialize only the given path prefixes (repeatable).
        #[arg(long)]
        sparse: Vec<String>,
        /// Destination directory (default: the repo name).
        #[arg(long)]
        into: Option<String>,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Sparse prefix list: add materializes a prefix, remove drops it.
    Sparse {
        #[command(subcommand)]
        cmd: SparseCmd,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Working-copy status: disk vs the stat index. Zero server calls.
    Status {
        /// Emit {"status","modified","deleted","untracked"}.
        #[arg(long)]
        json: bool,
    },
    /// Advance the working copy to the server's main (refuses only when
    /// incoming paths collide with local edits/untracked files).
    Sync {
        /// Discard local changes to incoming-colliding files, then proceed;
        /// symlinks are never removed.
        #[arg(long)]
        force: bool,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Commit all changes under the sparse prefixes (add-all semantics).
    Commit {
        /// Commit message.
        #[arg(short, long)]
        message: String,
        /// Author string (default: NAATRE_AUTHOR, else user@host).
        #[arg(long)]
        author: Option<String>,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Land the working copy's head onto the server's main (server-side
    /// rebase). The random default key is printed; pass it back with --key
    /// to retry or to re-land after a conflict.
    Land {
        /// Idempotency key (default: a random 8-hex suffix, printed).
        #[arg(long)]
        key: Option<String>,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Print a file's raw bytes (default: at head).
    Cat {
        /// Repo-relative path.
        path: String,
        /// Changeset id or ref name (default: head).
        #[arg(long)]
        at: Option<String>,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Blame a file: per-line attribution to the changeset that introduced
    /// it (first-parent semantics; default: at head).
    Blame {
        /// Repo-relative path.
        path: String,
        /// Changeset id or ref name (default: head).
        #[arg(long)]
        at: Option<String>,
        /// Emit {"lines":[{cs,author,text}]}.
        #[arg(long)]
        json: bool,
        #[command(flatten)]
        net: NetOpts,
    },
    /// List a tree's entries (default: the root at head).
    Ls {
        /// Directory path (default: the root).
        path: Option<String>,
        /// Changeset id or ref name (default: head).
        #[arg(long)]
        at: Option<String>,
        /// Emit the entries array as JSON.
        #[arg(long)]
        json: bool,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Diff head vs disk by default; two changesets with --at A B.
    Diff {
        /// Two changeset ids or ref names: --at A B.
        #[arg(long, num_args = 2, value_names = ["A", "B"])]
        at: Option<Vec<String>>,
        /// Emit {"changes":[…]}.
        #[arg(long)]
        json: bool,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Check the stat index (and blobs server-side); --rebuild rebuilds it.
    Doctor {
        /// Rescan disk, rehash everything, rewrite the index.
        #[arg(long)]
        rebuild: bool,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Run one reachability GC pass against a data dir (host-local, under
    /// the data dir flock; the serve-side task runs the same pass on an
    /// interval). Exit 0 on success, 1 on error.
    Gc {
        /// Existing data directory (uninitialized → run naatre init first).
        #[arg(long)]
        data: PathBuf,
        /// Emit the report as JSON.
        #[arg(long)]
        json: bool,
    },
    /// Check a data dir's consistency: dangling references, reachability
    /// counts, CAS presence vs the DB inventory (host-local, flocked,
    /// read-only). Exit 0 clean, 1 findings, 2 error.
    Fsck {
        /// Existing data directory (uninitialized → run naatre init first).
        #[arg(long)]
        data: PathBuf,
        /// Emit the report as JSON.
        #[arg(long)]
        json: bool,
    },
    /// Mark an object redacted in a repo (host-local admin verb, M10b: the
    /// tombstone drops the object from every serving path at mark time;
    /// the GC's redaction phase reclaims the bytes/rows later). Audited as
    /// actor `admin`. Remote administration goes through
    /// `POST /v1/repos/{r}/redactions` instead.
    Redact {
        /// What the id names: `blob` or `changeset`.
        kind: String,
        /// The object id, 64 lowercase hex.
        id: String,
        /// Why the object is redacted (audited as the `redact` action's
        /// details; the tombstone ROW itself carries path + date only).
        #[arg(long)]
        reason: String,
        /// The repo to tombstone the object in (redactions are
        /// repo-scoped: another repo referencing the same content keeps
        /// serving it).
        #[arg(long)]
        repo: String,
        /// Existing data directory (uninitialized → run naatre init first).
        #[arg(long)]
        data: PathBuf,
    },
    /// Capture the working copy as an ephemeral server-side snapshot
    /// (default TTL 7d). Prints {"id","expires_ns"}; head and index stay.
    Snapshot {
        /// Snapshot lifetime, e.g. 90s, 5m, 2h, 7d (min 60s; default 7d).
        #[arg(long)]
        ttl: Option<String>,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Restore a snapshot's files into this working copy (head unchanged).
    Restore {
        /// Snapshot id (64 hex, printed by `naatre snapshot`).
        snap_id: String,
        /// Discard local edits and colliding untracked files on the
        /// snapshot's paths (symlinks on them are unlinked).
        #[arg(long)]
        force: bool,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Run the stdio MCP server bound to this working copy (one checkout
    /// per session; holds .naatre/wc.lock for its lifetime). Diagnostics go
    /// to stderr; MCP_LOG=1 enables per-request lines.
    Mcp {
        /// Bearer token override for every tool call (argv is visible in
        /// ps; prefer the token store — use a least-privilege scoped token).
        #[arg(long)]
        token: Option<String>,
        /// Tool-allowlist mode: only the pure reads (status, log, diff, cat,
        /// ls, blame); everything else answers a tool error naming the flag.
        #[arg(long)]
        read_only: bool,
    },
    /// Serve a repo's ref as a lazily-hydrated filesystem over NFSv3 on
    /// loopback (M8; AUTH_SYS authenticates nothing). The ref is pinned at
    /// startup — later ref advances are invisible to this mount. Ctrl-C or
    /// SIGTERM stops the daemon.
    Mount {
        /// Repo name on the server.
        repo: String,
        /// Changeset id or ref name to pin (default: main).
        #[arg(long)]
        at: Option<String>,
        /// UNSTABLE: stage writes in a shadow dir; a changeset is staged
        /// (never landed) on flush at shutdown. Read-only by default.
        #[arg(long)]
        writable: bool,
        /// UNSTABLE (with --writable): land the staged changeset onto main
        /// at shutdown through the same land path `naatre land` uses; the
        /// idempotency key is printed for retries.
        #[arg(long)]
        flush_land: bool,
        /// Listen address (default 127.0.0.1:0; non-loopback prints the
        /// cleartext warning).
        #[arg(long)]
        listen: Option<String>,
        /// UNSTABLE (Linux only): serve through the kernel FUSE front-end
        /// at this mountpoint instead of NFSv3. Only the mounting user can
        /// access the mount; SIGINT/SIGTERM unmounts and flushes first.
        #[cfg(target_os = "linux")]
        #[arg(long)]
        fuse: Option<String>,
        #[command(flatten)]
        net: NetOpts,
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
        /// Segment-aligned path scope (one trailing slash is trimmed by
        /// the server).
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

#[derive(Subcommand)]
enum PrincipalCmd {
    /// Delete a principal (admin): token + principal rows go, the audit
    /// actor is pseudonymized to `deleted:<name>`; history attribution
    /// survives unless the changesets are themselves tombstoned.
    Delete {
        /// Principal name.
        name: String,
    },
}

#[derive(Subcommand)]
enum SparseCmd {
    /// Fetch and materialize a path prefix at head.
    Add {
        /// Path prefix to materialize (must exist in the tree).
        prefix: String,
        /// Discard local edits and colliding untracked files on the
        /// widened paths (symlinks on them are unlinked).
        #[arg(long)]
        force: bool,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Delete the indexed files under a prefix; drop it from the list.
    Remove {
        /// Path prefix currently in the sparse list.
        prefix: String,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Print the sparse prefixes (empty = full checkout).
    List,
}

/// `--server`/`--token` overrides (precedence: flags > env > config.toml).
#[derive(Args)]
struct NetOpts {
    /// Server origin override.
    #[arg(long)]
    server: Option<String>,
    /// Bearer token override.
    #[arg(long)]
    token: Option<String>,
}

impl NetOpts {
    fn net(&self) -> Net {
        Net {
            server: self.server.clone(),
            token: self.token.clone(),
        }
    }
}

fn main() {
    // Clap usage errors exit 3 (pinned usage code) — never 2, which is the
    // conflict class. --help/--version print and exit 0 as before.
    let cli = match Cli::try_parse() {
        Ok(cli) => cli,
        Err(e) => {
            let _ = e.print();
            std::process::exit(if e.use_stderr() { 3 } else { 0 });
        }
    };
    let cwd = std::path::Path::new(".");
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
            (false, _, Some(TokenCmd::List)) => client::token_list(),
            (
                false,
                _,
                Some(TokenCmd::Create {
                    principal,
                    role,
                    ttl_days,
                    scope,
                }),
            ) => client::token_create(&principal, &role, ttl_days, scope.as_deref()),
            (false, _, Some(TokenCmd::Revoke { id })) => client::token_revoke(id),
        },
        Cmd::Repo { cmd } => match cmd {
            RepoCmd::Create { name } => client::repo_create(&name),
            RepoCmd::Stats { name } => client::repo_stats(&name),
        },
        Cmd::Import {
            data,
            into,
            skip_unsupported,
            git_path,
        } => host::import(&data, into.as_deref(), skip_unsupported, &git_path),
        Cmd::Login { server, token } => wc_locked(cwd, || verbs::login::run(cwd, &server, &token)),
        Cmd::Clone {
            url,
            sparse,
            into,
            net,
        } => verbs::clone::run(&url, &sparse, into.as_deref(), &net.net()),
        Cmd::Sparse { cmd, net } => match cmd {
            SparseCmd::Add {
                prefix,
                force,
                net: n,
            } => wc_locked(cwd, || {
                verbs::sparse::run(cwd, verbs::sparse::Action::Add { prefix, force }, &n.net())
            }),
            SparseCmd::Remove { prefix, net: n } => wc_locked(cwd, || {
                verbs::sparse::run(cwd, verbs::sparse::Action::Remove(prefix), &n.net())
            }),
            SparseCmd::List => wc_locked(cwd, || {
                verbs::sparse::run(cwd, verbs::sparse::Action::List, &net.net())
            }),
        },
        Cmd::Status { json } => wc_locked(cwd, || verbs::status::run(cwd, json)),
        Cmd::Sync { force, net } => wc_locked(cwd, || verbs::sync::run(cwd, force, &net.net())),
        Cmd::Commit {
            message,
            author,
            net,
        } => wc_locked(cwd, || {
            verbs::commit::run(cwd, &message, author.as_deref(), &net.net())
        }),
        Cmd::Land { key, net } => {
            wc_locked(cwd, || verbs::land::run(cwd, key.as_deref(), &net.net()))
        }
        Cmd::Cat { path, at, net } => wc_locked(cwd, || {
            verbs::reads::cat(cwd, &path, at.as_deref(), &net.net())
        }),
        Cmd::Blame {
            path,
            at,
            json,
            net,
        } => wc_locked(cwd, || {
            verbs::reads::blame(cwd, &path, at.as_deref(), json, &net.net())
        }),
        Cmd::Ls {
            path,
            at,
            json,
            net,
        } => wc_locked(cwd, || {
            verbs::reads::ls(cwd, path.as_deref(), at.as_deref(), json, &net.net())
        }),
        Cmd::Diff { at, json, net } => {
            let at_pair = at.as_deref().and_then(|v| match v {
                [a, b] => Some((a.as_str(), b.as_str())),
                _ => None,
            });
            wc_locked(cwd, || verbs::reads::diff(cwd, at_pair, json, &net.net()))
        }
        Cmd::Doctor { rebuild, net } => {
            wc_locked(cwd, || verbs::doctor::run(cwd, rebuild, &net.net()))
        }
        Cmd::Gc { data, json } => host::gc(&data, json),
        Cmd::Fsck { data, json } => host::fsck(&data, json),
        Cmd::Redact {
            kind,
            id,
            reason,
            repo,
            data,
        } => host::redact(&data, &repo, &kind, &id, &reason),
        Cmd::Principal { cmd } => match cmd {
            PrincipalCmd::Delete { name } => client::principal_delete(&name),
        },
        Cmd::Snapshot { ttl, net } => wc_locked(cwd, || {
            verbs::snapshot::run(cwd, ttl.as_deref(), &net.net())
        }),
        Cmd::Restore {
            snap_id,
            force,
            net,
        } => wc_locked(cwd, || {
            verbs::restore::run(cwd, &snap_id, force, &net.net())
        }),
        Cmd::Mcp { token, read_only } => verbs::mcp::run(cwd, read_only, token),
        Cmd::Mount {
            repo,
            at,
            writable,
            flush_land,
            listen,
            net,
            #[cfg(target_os = "linux")]
            fuse,
        } => {
            // The Linux-only FUSE front-end takes over when --fuse is set;
            // exactly one of the two `let code` bindings exists per platform.
            #[cfg(target_os = "linux")]
            let code = match fuse.as_deref() {
                Some(mountpoint) => verbs::fuse::run(
                    &repo,
                    at.as_deref(),
                    writable,
                    flush_land,
                    mountpoint,
                    &net.net(),
                ),
                None => verbs::mount::run(
                    &repo,
                    at.as_deref(),
                    writable,
                    flush_land,
                    listen.as_deref(),
                    &net.net(),
                ),
            };
            #[cfg(not(target_os = "linux"))]
            let code = verbs::mount::run(
                &repo,
                at.as_deref(),
                writable,
                flush_land,
                listen.as_deref(),
                &net.net(),
            );
            code
        }
    };
    std::process::exit(code);
}

/// Serializes working-copy use (plan M7): every verb that opens an existing
/// working copy holds `.naatre/wc.lock` for its duration, so a concurrent
/// `naatre mcp` session — which holds the same lock for its whole life — is
/// detected and refused with an error naming the lock, and vice versa (the
/// server refuses to start while a CLI verb holds it). `clone` is exempt:
/// it creates a new tree, it does not open one.
fn wc_locked(root: &std::path::Path, f: impl FnOnce() -> i32) -> i32 {
    match verbs_lock::acquire(root) {
        Ok(verbs_lock::Outcome::NoNaatreDir) => f(), // the verb reports "not a naatre working copy"
        Ok(verbs_lock::Outcome::Locked(_guard)) => f(),
        Err(message) => {
            eprintln!("error: {message}");
            1
        }
    }
}

fn usage(message: &str) -> i32 {
    eprintln!("error: {message}");
    3
}
