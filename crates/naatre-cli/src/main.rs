//! `naatre` — one binary, multi-call (plan "Client"): host-local admin verbs
//! (`init`, `serve`, `token --new-admin`, `import`) that run directly
//! against the 0700 data dir, thin HTTP client verbs (`repo`, `token`
//! subcommands), the working-copy verbs (`clone`, `status`, `sparse`,
//! `sync`, `commit`, `resolve`, `cat`, `ls`, `diff`, `doctor`, `login`,
//! `snapshot`, `restore`, `mcp`), and the mount daemon (`mount`, M8).
//!
//! Pinned exit codes across client verbs: 0 success, 1 generic error,
//! 2 conflict-class (409 envelopes), 3 usage.

use std::path::PathBuf;

use clap::{Args, Parser, Subcommand};

use naatre_cli::verbs::{self, Net};
use naatre_cli::wc::lock as verbs_lock;
use naatre_cli::{client, host};

/// `naatre backup --help`'s safe-ordering restatement, per backend combo
/// (plan M11: the argument travels with the verb, not just the README).
const BACKUP_AFTER_HELP: &str = "\
SAFE ORDER (per combo): the consistent DB snapshot is always taken FIRST,
then the immutable objects — blob fsync precedes every referencing row
commit, so a DB snapshot older than the object copy leaves at most harmless
orphans, never dangling references.

  sqlite + fs    VACUUM INTO (a consistent single-file snapshot — NEVER an
                 in-place file copy of the WAL database), then objects/
                 is copied per the manifest.
  postgres + fs  one pg_dump run (a documented external dependency, the
                 same convention as git for import), then objects/.
  * + s3         the DB snapshot first; the operator then syncs the
                 WHOLE BUCKET (README \"Backup\" — incremental sync
                 between backups works: objects are immutable; fsck's
                 bytes_missing catches stragglers afterwards).

The DB-per-repo coordinated snapshot lands with M13a.";

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
    /// Adopt the db-per-repo layout on an existing single-DB data dir
    /// (M13a; offline — refuses a live server). Copies each repo's rows
    /// into fresh per-repo SQLite files, builds the catalog, verifies (row
    /// counts + fsck), and leaves the original single DB in place; prints
    /// the cutover steps.
    MigrateDbPerRepo {
        /// Existing data directory (single-DB layout required).
        #[arg(long)]
        data: PathBuf,
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
        #[command(flatten)]
        net: NetOpts,
    },
    /// Repo verbs (HTTP client, admin).
    Repo {
        #[command(subcommand)]
        cmd: RepoCmd,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Principal verbs (HTTP client, admin).
    Principal {
        #[command(subcommand)]
        cmd: PrincipalCmd,
        #[command(flatten)]
        net: NetOpts,
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
        /// Escape control-char/Cf author and message content LOSSLESSLY
        /// (\\u{hex} escapes + a per-object `escaped` report entry naming
        /// each escaped commit and code point) instead of the default's
        /// U+FFFD replacement.
        #[arg(long)]
        sanitize: bool,
        /// Path of the local git repository to import.
        git_path: String,
    },
    /// Snapshot a whole data dir (every backend combo) into ONE new backup
    /// directory: manifest.json + a consistent DB snapshot + the objects.
    /// Host-local, under the data-dir flock (stop-and-copy: a running
    /// server holds the lock).
    #[command(after_help = BACKUP_AFTER_HELP)]
    Backup {
        /// Existing data directory (uninitialized → run naatre init first).
        #[arg(long)]
        data: PathBuf,
        /// Backup directory to create (must not exist; restore refuses a
        /// directory without its manifest).
        dest: PathBuf,
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
    /// Working-copy config keys (F1 TLS posture: insecure_http, ca_file).
    Config {
        #[command(subcommand)]
        cmd: ConfigCmd,
    },

    /// Clone a repo into a new working copy.
    Clone {
        /// https://host[:port]/<repo> (or http:// to a loopback host, or
        /// just <repo> to use NAATRE_URL).
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
    /// to retry or to re-land after a conflict. A rebase conflict answers
    /// the structured 409 + recipe; --record-conflict opts into recording
    /// the conflict on main instead (a conflicted changeset, settled once).
    Land {
        /// Idempotency key (default: a random 8-hex suffix, printed).
        #[arg(long)]
        key: Option<String>,
        /// M12 opt-in (default off): instead of the 409 + recipe on a
        /// rebase conflict, record the conflict on main as a conflicted
        /// changeset (format v2, markers materialized in its tree,
        /// `lands.state = 'conflicted'` — still ONE settlement; the same
        /// key re-lands after the 409). Only file/directory conflicts
        /// still refuse. Resolve afterwards with
        /// `naatre resolve <path>`.
        #[arg(long)]
        record_conflict: bool,
        #[command(flatten)]
        net: NetOpts,
    },
    /// Commit the resolved content of one conflicted path as an explicit
    /// resolution changeset referencing the conflicted head (M12): the
    /// path must be one `status` marks conflicted, and the file on disk
    /// must no longer be a marker file. Follow with `naatre land`.
    Resolve {
        /// Repo-relative path of the conflicted file (as `status` shows it).
        path: String,
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
        /// The ONE repair fsck can do: backfill the `objects` record row
        /// for blobs this store REFERENCES whose CAS bytes are present
        /// (the dirt pre-M11 imports left). Audited; nothing else is
        /// touched — every other finding stays a finding.
        #[arg(long)]
        repair: bool,
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
    /// Restore a snapshot's files into this working copy (head unchanged) —
    /// or, with --data, restore a DATA-DIR BACKUP (M11) into a data dir.
    Restore {
        /// Snapshot id (64 hex, printed by `naatre snapshot`) — or, with
        /// --data, the path of the backup directory (`naatre backup`)
        /// whose manifest drives the restore.
        snap_id: String,
        /// Snapshot mode: discard local edits and colliding untracked
        /// files on the snapshot's paths (symlinks on them are unlinked).
        /// Backup mode (--data): overwrite a NON-EMPTY target data dir
        /// (an initialized Postgres schema is still refused — dropping it
        /// is the wipe).
        #[arg(long)]
        force: bool,
        /// Data-dir backup restore: <arg> is the backup directory, and
        /// this is the target data dir. Every artifact is SHA256-verified
        /// into staging BEFORE any write to the target; a non-empty target
        /// is refused without --force. Postgres targets are refused while
        /// the schema is initialized EVEN WITH --force (dropping the
        /// schema is the wipe) — because the refusal happens before any
        /// psql write and the replay is ONE transaction, a failed Postgres
        /// restore leaves the uninitialized target it started on. Run `naatre fsck --data <dir>`
        /// afterwards (restore checks object presence, not reachability).
        #[arg(long)]
        data: Option<PathBuf>,
        /// The S3 purge-race escape (two-step): after a presence-gate
        /// refusal named the missing ids and printed a waiver digest,
        /// re-run with `--allow-missing <digest-or-ids>` to accept EXACTLY
        /// that set (anything else missing refuses again; waived ids land
        /// in the report and the restore audit row, and the redaction duty
        /// applies to them).
        #[arg(long, value_name = "DIGEST_OR_IDS", requires = "data")]
        allow_missing: Option<String>,
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
    /// Run the LAN cache read tier (M13b): ticket-verified by-id object
    /// reads only (refs/logs/writes are never proxied). Fail-closed: no
    /// verify keys, no serve. Ctrl-C/SIGTERM drains gracefully.
    Cache {
        /// The origin's base URL — https://host[:port] (plain http:// is
        /// accepted for loopback origins only).
        #[arg(long)]
        origin: String,
        /// The origin's PUBLIC verify-key list (the origin data dir's
        /// cache_keys.json, copied here): a path, or env:NAME to read it
        /// from an environment variable.
        #[arg(long)]
        key: String,
        /// Listen address; non-loopback warns that tickets cross the
        /// network in cleartext unless fronted by TLS.
        #[arg(long, default_value = "127.0.0.1:7500")]
        listen: String,
        /// The tier's OWN data directory for its content store (created
        /// on first run; never a naatre data dir).
        #[arg(long)]
        data: PathBuf,
        /// Ops log level (access logs are debug-level: off by default).
        #[arg(long, default_value = "info")]
        log_level: String,
        #[command(flatten)]
        net: NetOpts,
    },
    /// The origin's cache-ticket signing keys (host-local, M13b): init
    /// once, rotate to retire (retired keys verify unexpired tickets
    /// until their ≤15-min expiry). Both audited.
    CacheKey {
        #[command(subcommand)]
        cmd: CacheKeyCmd,
    },
}

#[derive(Subcommand)]
enum CacheKeyCmd {
    /// Generate the signing pair (refuses an existing key store).
    Init {
        /// Existing data directory (uninitialized → run naatre init first).
        #[arg(long)]
        data: PathBuf,
    },
    /// Rotate: a new current key; the old key retires (public half kept,
    /// unexpired tickets still verify until expiry).
    Rotate {
        /// Existing data directory.
        #[arg(long)]
        data: PathBuf,
    },
    /// Containment (M13b-fix): remove a RETIRED key id from the verify
    /// list (the current key refuses — rotate first). Tickets under the
    /// dropped key fail at their next verify. Audited.
    Drop {
        /// The retired key id (see `cache-key show`).
        id: String,
        /// Existing data directory.
        #[arg(long)]
        data: PathBuf,
    },
    /// Print the verify key ids (current/retired) and the list's path.
    Show {
        /// Existing data directory.
        #[arg(long)]
        data: PathBuf,
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
    /// The repo read/write ACL surface (M13b, admin): a repo's FIRST
    /// grant makes it private (uniform 404 for non-readers everywhere,
    /// dropped from the listing); revoking the LAST grant returns it to
    /// public.
    Access {
        #[command(subcommand)]
        cmd: AccessCmd,
    },
}

#[derive(Subcommand)]
enum AccessCmd {
    /// Grant a principal read or write on the repo (write implies read).
    Grant {
        /// Repo name.
        repo: String,
        /// Principal name (must exist).
        principal: String,
        /// Grant read.
        #[arg(long)]
        read: bool,
        /// Grant write (implies read).
        #[arg(long)]
        write: bool,
    },
    /// Revoke: `--write` downgrades to read-only; `--read` (or no flag)
    /// removes the grant entirely.
    Revoke {
        /// Repo name.
        repo: String,
        /// Principal whose grant goes.
        principal: String,
        /// Remove the whole grant (write goes with it).
        #[arg(long)]
        read: bool,
        /// Downgrade to read-only.
        #[arg(long)]
        write: bool,
    },
    /// List the repo's grants (empty = the repo is public).
    List {
        /// Repo name.
        repo: String,
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

/// `naatre config set <key> <value>` — the working-copy TLS-posture keys
/// (F1). Strict: only `insecure_http` and `ca_file` exist.
#[derive(Subcommand)]
enum ConfigCmd {
    /// Set one working-copy config key (`naatre config set insecure_http
    /// "http://host:7400"` opts this working copy's origin in to plain
    /// http; `none` clears).
    Set {
        /// Key: insecure_http (the opted-in origin "http://host[:port]",
        /// or none to clear) or ca_file (a PEM bundle path, or none to
        /// clear it).
        key: String,
        /// Value: the http origin for insecure_http; a path or `none`
        /// for ca_file.
        value: String,
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

/// `--server`/`--token`/`--ca-file`/`--insecure-http` overrides
/// (precedence: flags > env > config.toml). Every server-talking verb
/// flattens this, so `--ca-file` and `--insecure-http` are GLOBAL options
/// — admin verbs included (their working-copy-less opt-in for plain http
/// is exactly `--insecure-http` / `NAATRE_INSECURE_HTTP=1`).
#[derive(Args)]
struct NetOpts {
    /// Server origin override.
    #[arg(long)]
    server: Option<String>,
    /// Bearer token override.
    #[arg(long)]
    token: Option<String>,
    /// PEM CA bundle added to the platform roots (private CAs); beats
    /// NAATRE_CA_FILE and the working-copy config's ca_file.
    #[arg(long)]
    ca_file: Option<String>,
    /// Opt in to plain http:// for this invocation (any host — admin
    /// verbs have no working copy to carry a per-remote opt-in; env:
    /// NAATRE_INSECURE_HTTP=1). https:// is always preferred.
    #[arg(long)]
    insecure_http: bool,
}

impl NetOpts {
    fn net(&self) -> Net {
        Net {
            server: self.server.clone(),
            token: self.token.clone(),
            ca_file: self.ca_file.clone(),
            insecure_http: naatre_cli::verbs::invocation_optin(self.insecure_http),
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
            net,
        } => match (new_admin, data, cmd) {
            (true, Some(data), None) => host::new_admin_token(&data),
            (true, None, _) => usage("token --new-admin requires --data <dir>"),
            (true, Some(_), Some(_)) => usage("token --new-admin takes no subcommand"),
            (false, _, None) => {
                usage("token requires --data <dir> --new-admin (host-local) or a subcommand (HTTP)")
            }
            (false, _, Some(TokenCmd::List)) => client::token_list(&net.net()),
            (
                false,
                _,
                Some(TokenCmd::Create {
                    principal,
                    role,
                    ttl_days,
                    scope,
                }),
            ) => client::token_create(&principal, &role, ttl_days, scope.as_deref(), &net.net()),
            (false, _, Some(TokenCmd::Revoke { id })) => client::token_revoke(id, &net.net()),
        },
        Cmd::Repo { cmd, net } => match cmd {
            RepoCmd::Create { name } => client::repo_create(&name, &net.net()),
            RepoCmd::Stats { name } => client::repo_stats(&name, &net.net()),
            RepoCmd::Access { cmd } => match cmd {
                AccessCmd::Grant {
                    repo,
                    principal,
                    read,
                    write,
                } => {
                    if !read && !write {
                        usage("repo access grant takes --read and/or --write")
                    } else {
                        client::repo_access_grant(
                            &repo,
                            &principal,
                            read || write,
                            write,
                            &net.net(),
                        )
                    }
                }
                AccessCmd::Revoke {
                    repo,
                    principal,
                    read,
                    write,
                } => {
                    // The documented mapping: `--write` downgrades to
                    // read-only; `--read` or NO FLAG removes the grant
                    // entirely (write cannot stand without read — the
                    // full-removal DELETE path).
                    client::repo_access_revoke(&repo, &principal, read, write, &net.net())
                }
                AccessCmd::List { repo } => client::repo_access_list(&repo, &net.net()),
            },
        },
        Cmd::Import {
            data,
            into,
            skip_unsupported,
            sanitize,
            git_path,
        } => host::import(
            &data,
            into.as_deref(),
            skip_unsupported,
            sanitize,
            &git_path,
        ),
        Cmd::MigrateDbPerRepo { data } => host::migrate_db_per_repo(&data),
        Cmd::Backup { data, dest } => host::backup(&data, &dest),
        Cmd::Login { server, token } => wc_locked(cwd, || verbs::login::run(cwd, &server, &token)),
        Cmd::Config { cmd } => match cmd {
            ConfigCmd::Set { key, value } => {
                wc_locked(cwd, || verbs::config_set(cwd, &key, &value))
            }
        },
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
            } => wc_checked(cwd, &n, |net| {
                verbs::sparse::run(cwd, verbs::sparse::Action::Add { prefix, force }, &net)
            }),
            SparseCmd::Remove { prefix, net: n } => wc_checked(cwd, &n, |net| {
                verbs::sparse::run(cwd, verbs::sparse::Action::Remove(prefix), &net)
            }),
            // List is local-only (no server calls): no pre-flight.
            SparseCmd::List => wc_locked(cwd, || {
                verbs::sparse::run(cwd, verbs::sparse::Action::List, &net.net())
            }),
        },
        Cmd::Status { json } => wc_locked(cwd, || verbs::status::run(cwd, json)),
        Cmd::Sync { force, net } => wc_checked(cwd, &net, |net| verbs::sync::run(cwd, force, &net)),
        Cmd::Commit {
            message,
            author,
            net,
        } => wc_checked(cwd, &net, |net| {
            verbs::commit::run(cwd, &message, author.as_deref(), &net)
        }),
        Cmd::Land {
            key,
            record_conflict,
            net,
        } => wc_checked(cwd, &net, |net| {
            verbs::land::run(cwd, key.as_deref(), record_conflict, &net)
        }),
        Cmd::Resolve { path, net } => {
            wc_checked(cwd, &net, |net| verbs::resolve::run(cwd, &path, &net))
        }
        Cmd::Cat { path, at, net } => wc_checked(cwd, &net, |net| {
            verbs::reads::cat(cwd, &path, at.as_deref(), &net)
        }),
        Cmd::Blame {
            path,
            at,
            json,
            net,
        } => wc_checked(cwd, &net, |net| {
            verbs::reads::blame(cwd, &path, at.as_deref(), json, &net)
        }),
        Cmd::Ls {
            path,
            at,
            json,
            net,
        } => wc_checked(cwd, &net, |net| {
            verbs::reads::ls(cwd, path.as_deref(), at.as_deref(), json, &net)
        }),
        Cmd::Diff { at, json, net } => {
            let at_pair = at.as_deref().and_then(|v| match v {
                [a, b] => Some((a.as_str(), b.as_str())),
                _ => None,
            });
            wc_checked(cwd, &net, |net| {
                verbs::reads::diff(cwd, at_pair, json, &net)
            })
        }
        Cmd::Doctor { rebuild, net } => {
            wc_checked(cwd, &net, |net| verbs::doctor::run(cwd, rebuild, &net))
        }
        Cmd::Gc { data, json } => host::gc(&data, json),
        Cmd::Fsck { data, json, repair } => host::fsck(&data, json, repair),
        Cmd::Redact {
            kind,
            id,
            reason,
            repo,
            data,
        } => host::redact(&data, &repo, &kind, &id, &reason),
        Cmd::Principal { cmd, net } => match cmd {
            PrincipalCmd::Delete { name } => client::principal_delete(&name, &net.net()),
        },
        Cmd::Snapshot { ttl, net } => wc_checked(cwd, &net, |net| {
            verbs::snapshot::run(cwd, ttl.as_deref(), &net)
        }),
        Cmd::Restore {
            snap_id,
            force,
            allow_missing,
            data,
            net,
        } => match data {
            Some(data) => host::restore_backup(
                &data,
                std::path::Path::new(&snap_id),
                force,
                allow_missing.as_deref(),
            ),
            None => wc_checked(cwd, &net, |net| {
                verbs::restore::run(cwd, &snap_id, force, &net)
            }),
        },
        Cmd::Mcp { token, read_only } => verbs::mcp::run(cwd, read_only, token),
        Cmd::Cache {
            origin,
            key,
            listen,
            data,
            log_level,
            net,
        } => host::cache_run(
            &origin,
            &key,
            &listen,
            &data,
            &log_level,
            net.net().insecure_http,
        ),
        Cmd::CacheKey { cmd } => match cmd {
            CacheKeyCmd::Init { data } => host::cache_key_init(&data),
            CacheKeyCmd::Rotate { data } => host::cache_key_rotate(&data),
            CacheKeyCmd::Drop { id, data } => host::cache_key_drop(&data, &id),
            CacheKeyCmd::Show { data } => host::cache_key_show(&data),
        },
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
                Some(mountpoint) => wc_checked(cwd, &net, |net| {
                    verbs::fuse::run(&repo, at.as_deref(), writable, flush_land, mountpoint, &net)
                }),
                None => wc_checked(cwd, &net, |net| {
                    verbs::mount::run(
                        &repo,
                        at.as_deref(),
                        writable,
                        flush_land,
                        listen.as_deref(),
                        &net,
                    )
                }),
            };
            #[cfg(not(target_os = "linux"))]
            let code = wc_checked(cwd, &net, |net| {
                verbs::mount::run(
                    &repo,
                    at.as_deref(),
                    writable,
                    flush_land,
                    listen.as_deref(),
                    &net,
                )
            });
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

/// The server-talking working-copy verbs' dispatch, with the plain-http
/// PRE-FLIGHT (pinned exit code): the refusal must be the usage class
/// (exit 3) on every path, but the verb cores report their client-build
/// errors generically (exit 1) — so the SAME admission
/// (`wc::check_remote_policy`) runs here first, printing the refusal and
/// exiting 3 before the verb opens anything. Otherwise identical to
/// [`wc_locked`].
fn wc_checked(root: &std::path::Path, opts: &NetOpts, f: impl FnOnce(Net) -> i32) -> i32 {
    let net = opts.net();
    if let Err(message) = naatre_cli::wc::check_remote_policy(root, &net) {
        eprintln!("error: {message}");
        return 3;
    }
    wc_locked(root, || f(net))
}

fn usage(message: &str) -> i32 {
    eprintln!("error: {message}");
    3
}
