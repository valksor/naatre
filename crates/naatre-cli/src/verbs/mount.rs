//! `naatre mount <repo> [--at REF] [--writable] [--listen ADDR]` — serve a
//! PINNED ref as a lazily-hydrated filesystem (plan M8).
//!
//! The daemon resolves the ref → changeset once at startup and serves that
//! tree for its whole life: advancing the ref afterwards is invisible to
//! the mount (documented in the README and pinned in `tests/vfs_test.rs`).
//! Defaults: loopback-only bind on an EPHEMERAL port with an unguessable
//! per-mount export path (`/naatre/<32-hex>`); a non-loopback `--listen`
//! is explicit opt-in and prints the same cleartext warning `serve` does.
//! **AUTH_SYS authenticates nothing** — anyone who can reach the port can
//! read the export (and write to a `--writable` one).
//!
//! Platform front-ends: NFSv3 via `nfsserve` (one loopback TCP port carries
//! the NFS, mount, and portmap programs — the printed mount lines pin the
//! port for both `port=` and `mountport=`). FUSE (Linux-only) is the M8b
//! follow-up; this slice ships NFS-only. The in-process suite drives
//! [`crate::vfs::NaatreVfs`] directly — no kernel mount needed (CI-feasible).
//!
//! The hydrate cache lives under the per-mount dir
//! (`$XDG_CACHE_HOME|~/.cache`/naatre/mounts/<export>/, 0700, LRU-capped
//! at 1024 blobs — `crate::vfs` documents the rules). A daemon killed with
//! SIGKILL leaves its dir; the next mount evicts stale dirs (dead
//! `session.json` pid). On graceful exit (SIGINT/SIGTERM) a `--writable`
//! mount flushes its shadow writes into a STAGED changeset (printed; never
//! landed) and removes its dir.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::SystemTime;

use async_trait::async_trait;
use nfsserve::nfs::{
    fattr3, fileid3, filename3, ftype3, nfspath3, nfsstat3, nfstime3, sattr3, specdata3,
};
use nfsserve::tcp::{NFSTcp, NFSTcpListener};
use nfsserve::vfs::{DirEntry, NFSFileSystem, ReadDirResult, VFSCapabilities};
use serde_json::json;

use super::{Net, VerbError};
use crate::client::{Client, DEFAULT_URL};
use crate::vfs::{
    ERRNO_HASH_MISMATCH, ERRNO_INVALID_PATH, ERRNO_IS_DIR, ERRNO_NOT_DIR, ERRNO_NOT_FOUND,
    ERRNO_READ_ONLY, EntryMeta, HttpMountSource, NaatreVfs, VfsError,
};

/// `naatre mount <repo> …` — the daemon. Returns the pinned exit code.
pub fn run(repo: &str, at: Option<&str>, writable: bool, listen: Option<&str>, net: &Net) -> i32 {
    let cache_root = match mount_cache_root() {
        Ok(r) => r,
        Err(m) => return VerbError::Local(m).report(),
    };
    let mounts_root = cache_root.join("naatre").join("mounts");
    let plan = match MountPlan::new(listen, mounts_root, writable) {
        Ok(p) => p,
        Err(m) => return VerbError::Usage(m).report(),
    };
    // Stale dirs from SIGKILLed mounts go BEFORE our own dir exists.
    prune_stale_mounts(&plan.mounts_root);
    // The pin resolves FIRST: a bad repo/ref is a clean error before the
    // daemon binds anything (this also creates the mount dir).
    let vfs = match NaatreVfs::mount(
        HttpMountSource::new(mount_client(net), repo),
        repo,
        at,
        plan.mount_dir.clone(),
        writable,
    ) {
        Ok(v) => v,
        Err(e) => {
            let _ = std::fs::remove_dir_all(&plan.mount_dir);
            return vfs_error_verb(e);
        }
    };
    if let Err(e) = write_session(&plan.mount_dir) {
        let _ = std::fs::remove_dir_all(&plan.mount_dir);
        return VerbError::Local(format!("writing the mount session: {e}")).report();
    }
    let runtime = match tokio::runtime::Runtime::new() {
        Ok(rt) => rt,
        Err(e) => {
            let _ = std::fs::remove_dir_all(&plan.mount_dir);
            return VerbError::Local(format!("starting the async runtime: {e}")).report();
        }
    };
    runtime.block_on(serve(plan, vfs))
}

/// Mount reads its server/token from `--server`/`--token` flags, then
/// `NAATRE_URL`/`NAATRE_TOKEN` env (no working copy — config.toml does not
/// apply; the default is the pinned default URL).
fn mount_client(net: &Net) -> Client {
    let base = net
        .server
        .clone()
        .or_else(|| std::env::var("NAATRE_URL").ok().filter(|v| !v.is_empty()))
        .unwrap_or_else(|| DEFAULT_URL.to_string());
    let token = net
        .token
        .clone()
        .or_else(|| std::env::var("NAATRE_TOKEN").ok().filter(|v| !v.is_empty()));
    Client::new(&base, token)
}

/// Maps a VFS failure from mount-time setup into the pinned exit codes.
fn vfs_error_verb(e: VfsError) -> i32 {
    match e {
        VfsError::Http(e) => VerbError::Http(e).report(),
        other => VerbError::Local(other.to_string()).report(),
    }
}

/// The OS cache root: `$XDG_CACHE_HOME`, else `~/.cache` (the same fallback
/// on macOS — documented in the README's mount section).
pub fn mount_cache_root() -> Result<PathBuf, String> {
    if let Ok(x) = std::env::var("XDG_CACHE_HOME")
        && !x.is_empty()
    {
        return Ok(PathBuf::from(x));
    }
    if let Ok(h) = std::env::var("HOME")
        && !h.is_empty()
    {
        return Ok(PathBuf::from(h).join(".cache"));
    }
    Err("no cache location: set XDG_CACHE_HOME or HOME".into())
}

/// A 32-hex CSPRNG export id (16 bytes — unguessable per the plan's bind
/// contract; the export path is `/naatre/<id>`).
pub fn new_export_id() -> String {
    let mut bytes = [0u8; 16];
    getrandom::fill(&mut bytes).expect("CSPRNG available");
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// The daemon's bind/export/cache plan (testable without a server).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MountPlan {
    /// The bind address (default `127.0.0.1:0` — loopback, ephemeral).
    pub listen_addr: String,
    /// The export name nfsserve wants (no leading slash): `naatre/<hex>`.
    pub export: String,
    /// `<cache-root>/naatre/mounts` — all mounts' parent (0700 enforced
    /// per dir).
    pub mounts_root: PathBuf,
    /// This mount's dir: `mounts_root/<hex>` (session.json, blobs/, shadow/).
    pub mount_dir: PathBuf,
    /// The UNSTABLE writable mode.
    pub writable: bool,
    non_loopback: bool,
}

impl MountPlan {
    pub fn new(
        listen: Option<&str>,
        mounts_root: PathBuf,
        writable: bool,
    ) -> Result<MountPlan, String> {
        Self::with_export(listen, mounts_root, writable, new_export_id())
    }

    /// [`MountPlan::new`] with an injected export id (tests pin the shape).
    pub fn with_export(
        listen: Option<&str>,
        mounts_root: PathBuf,
        writable: bool,
        export_id: String,
    ) -> Result<MountPlan, String> {
        let listen = listen.unwrap_or("127.0.0.1:0");
        let addr: std::net::SocketAddr = listen
            .parse()
            .map_err(|_| format!("invalid listen address {listen:?}"))?;
        let export_id = export_id.trim_matches('/').to_string();
        Ok(MountPlan {
            listen_addr: addr.to_string(),
            export: format!("naatre/{export_id}"),
            mounts_root: mounts_root.clone(),
            mount_dir: mounts_root.join(&export_id),
            writable,
            non_loopback: !addr.ip().is_loopback(),
        })
    }

    /// The export path clients mount (leading slash).
    pub fn export_path(&self) -> String {
        format!("/{}", self.export)
    }

    /// A non-loopback bind is explicit opt-in (the plan's security pin).
    pub fn non_loopback(&self) -> bool {
        self.non_loopback
    }
}

/// The cleartext warning — the SAME text `serve` prints for non-loopback
/// binds (host.rs), plus nothing mount-specific (the AUTH_SYS line prints
/// unconditionally in the banner).
pub fn cleartext_warning(addr: &str) -> String {
    format!(
        "warning: {addr} is non-loopback: bearer tokens will cross the network in cleartext unless fronted by TLS"
    )
}

/// The per-platform mount command lines (printed with the resolved port).
/// Both `port=` and `mountport=` carry the daemon's single port (nfsserve
/// multiplexes the NFS and MOUNT programs on one TCP listener).
pub fn mount_commands(port: u16, export_path: &str) -> (String, String) {
    let src = format!("127.0.0.1:{export_path}");
    let opts = format!("port={port},mountport={port},tcp,soft");
    (
        format!("mkdir -p <mountpoint> && mount_nfs -o {opts},nfsvers=3 {src} <mountpoint>"),
        format!("mkdir -p <mountpoint> && mount -t nfs -o {opts},vers=3 {src} <mountpoint>"),
    )
}

/// Writes this mount's session file (0600): the pid stale-dir eviction
/// checks on the next mount.
fn write_session(mount_dir: &Path) -> std::io::Result<()> {
    let body = json!({
        "pid": std::process::id(),
        "created_ns": crate::wc::now_ns(),
    });
    crate::wc::write_atomic(
        &mount_dir.join("session.json"),
        body.to_string().as_bytes(),
        true,
    )
}

/// Evicts mount dirs whose owning daemon is gone (SIGKILL cleanup): a dir
/// with no readable session, or one whose pid no longer exists (or is not
/// ours to probe — EPERM reads as alive), is removed. Live mounts' dirs
/// stay. Idempotent; missing root is fine.
pub fn prune_stale_mounts(mounts_root: &Path) {
    let Ok(entries) = std::fs::read_dir(mounts_root) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        if !path.is_dir() {
            continue;
        }
        let stale = std::fs::read(path.join("session.json"))
            .ok()
            .and_then(|bytes| serde_json::from_slice::<serde_json::Value>(&bytes).ok())
            .and_then(|v| v["pid"].as_u64())
            .is_none_or(|pid| !pid_alive(pid as u32));
        if stale {
            let _ = std::fs::remove_dir_all(&path);
        }
    }
}

/// `kill(pid, 0)`: rc 0 = alive; EPERM = alive (someone else's); ESRCH =
/// gone.
fn pid_alive(pid: u32) -> bool {
    let rc = unsafe { libc::kill(pid as libc::pid_t, 0) };
    if rc == 0 {
        return true;
    }
    std::io::Error::last_os_error().raw_os_error() == Some(libc::EPERM)
}

/// The async daemon: bind, print the mount lines, serve until SIGINT or
/// SIGTERM, flush (writable), clean up.
async fn serve(plan: MountPlan, vfs: NaatreVfs<HttpMountSource>) -> i32 {
    let vfs = Arc::new(vfs);
    let adapter = NfsAdapter {
        vfs: vfs.clone(),
        started_secs: SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as u32)
            .unwrap_or(0),
    };
    let mut listener = match NFSTcpListener::bind(&plan.listen_addr, adapter).await {
        Ok(l) => l,
        Err(e) => {
            eprintln!("error: binding {}: {e}", plan.listen_addr);
            let _ = std::fs::remove_dir_all(&plan.mount_dir);
            return 1;
        }
    };
    listener.with_export_name(&plan.export);
    let port = listener.get_listen_port();
    print_banner(&plan, port, vfs.repo(), vfs.pin().changeset.to_hex());
    tokio::select! {
        res = listener.handle_forever() => {
            if let Err(e) = res {
                eprintln!("error: serving NFS: {e}");
            }
        }
        _ = naatre_server::serve::unix_shutdown_signal() => {}
    }
    // Graceful exit: the unstable writable mode stages its shadow writes
    // (printed, never landed); then the mount dir goes away.
    let mut code = 0;
    if plan.writable {
        match vfs.flush() {
            Ok(Some(outcome)) => println!(
                "staged changeset: {} ({} file(s), root tree {}) — staged only, NOT landed",
                outcome.changeset.to_hex(),
                outcome.files,
                outcome.root_tree.to_hex()
            ),
            Ok(None) => println!("flush: nothing staged"),
            Err(e) => {
                eprintln!("error: flush failed: {e}");
                code = 1;
            }
        }
    }
    let _ = std::fs::remove_dir_all(&plan.mount_dir);
    code
}

fn print_banner(plan: &MountPlan, port: u16, repo: &str, pinned_cs: String) {
    let pin_note = if plan.writable {
        "writable (UNSTABLE)"
    } else {
        "read-only"
    };
    let export_path = plan.export_path();
    println!("naatre mount: {repo} pinned at {pinned_cs} ({pin_note})");
    if plan.non_loopback() {
        eprintln!("{}", cleartext_warning(&plan.listen_addr));
    }
    let bind_ip = plan
        .listen_addr
        .rsplit_once(':')
        .map(|(ip, _)| ip)
        .unwrap_or("127.0.0.1");
    println!("serving {export_path} on {bind_ip}:{port}");
    println!(
        "  AUTH_SYS authenticates nothing: anyone who can reach {} reads this export (and writes, when --writable).",
        plan.listen_addr
    );
    let (macos, linux) = mount_commands(port, &export_path);
    println!("macOS:   {macos}");
    println!("Linux:   {linux}");
    println!(
        "cache:   {} (0700; hydrate cache LRU-capped at {} blobs; stale dirs evicted on next mount)",
        plan.mount_dir.display(),
        crate::vfs::BLOB_CACHE_MAX_ENTRIES
    );
    println!(
        "Ctrl-C or SIGTERM stops the daemon (writable mounts stage their shadow writes first)."
    );
}

// ---- the nfsserve front-end -------------------------------------------------

/// Bridges the sync [`NaatreVfs`] into nfsserve's async trait: every VFS
/// call runs under `block_in_place` (the daemon runs a multi-thread
/// runtime; the VFS blocks on HTTP + file I/O).
struct NfsAdapter {
    vfs: Arc<NaatreVfs<HttpMountSource>>,
    started_secs: u32,
}

/// Runs one blocking VFS call, printing hydration failures (they name the
/// object id — the plan's failure-reporting pin) and mapping the errno.
fn run_sync<T>(f: impl FnOnce() -> Result<T, VfsError>) -> Result<T, nfsstat3> {
    tokio::task::block_in_place(move || f().map_err(|e| report_vfs_error(&e)))
}

fn report_vfs_error(e: &VfsError) -> nfsstat3 {
    if let VfsError::Hydration(h) = e {
        eprintln!("{}", crate::vfs::hydration_log_line(h));
    }
    match e.errno() {
        ERRNO_NOT_FOUND => nfsstat3::NFS3ERR_NOENT,
        ERRNO_NOT_DIR => nfsstat3::NFS3ERR_NOTDIR,
        ERRNO_IS_DIR => nfsstat3::NFS3ERR_ISDIR,
        ERRNO_READ_ONLY => nfsstat3::NFS3ERR_ROFS,
        ERRNO_INVALID_PATH => nfsstat3::NFS3ERR_INVAL,
        ERRNO_HASH_MISMATCH => nfsstat3::NFS3ERR_STALE,
        _ => nfsstat3::NFS3ERR_IO,
    }
}

/// A write-path op the VFS layer itself rejects up front (read-only mounts
/// answer ROFS — the EROFS-default pin — writable v1 answers PERM for the
/// ops outside its create+write scope).
fn unsupported_op(writable: bool) -> nfsstat3 {
    if writable {
        nfsstat3::NFS3ERR_PERM
    } else {
        nfsstat3::NFS3ERR_ROFS
    }
}

/// Non-UTF8 names cannot be naatre paths.
fn utf8_name(name: &[u8]) -> Result<String, nfsstat3> {
    std::str::from_utf8(name)
        .map(str::to_string)
        .map_err(|_| nfsstat3::NFS3ERR_INVAL)
}

impl NfsAdapter {
    /// The VFS path for a session fileid (nfsserve answers STALE for
    /// unknown handles, which is exactly the right client recovery).
    fn path_of(&self, id: fileid3) -> Result<String, nfsstat3> {
        self.vfs.path_of_fileid(id).ok_or(nfsstat3::NFS3ERR_STALE)
    }

    /// Attributes: kind/size/exec from the tree entry; times are the
    /// daemon's start (a pinned tree carries no timestamps — documented).
    fn fattr(&self, fileid: fileid3, meta: EntryMeta) -> fattr3 {
        let (ftype, mode) = match meta.kind {
            naatre_core::tree::EntryKind::File => {
                (ftype3::NF3REG, if meta.exec { 0o755 } else { 0o644 })
            }
            naatre_core::tree::EntryKind::Dir => (ftype3::NF3DIR, 0o755),
        };
        let time = nfstime3 {
            seconds: self.started_secs,
            nseconds: 0,
        };
        fattr3 {
            ftype,
            mode,
            nlink: 1,
            uid: 0,
            gid: 0,
            size: meta.size,
            used: meta.size,
            rdev: specdata3::default(),
            fsid: 0,
            fileid,
            atime: time,
            mtime: time,
            ctime: time,
        }
    }
}

#[async_trait]
impl NFSFileSystem for NfsAdapter {
    fn capabilities(&self) -> VFSCapabilities {
        if self.vfs.writable() {
            VFSCapabilities::ReadWrite
        } else {
            VFSCapabilities::ReadOnly
        }
    }

    fn root_dir(&self) -> fileid3 {
        self.vfs.root_fileid()
    }

    async fn lookup(&self, dirid: fileid3, filename: &filename3) -> Result<fileid3, nfsstat3> {
        let dir = self.path_of(dirid)?;
        let name = utf8_name(filename)?;
        let entry = run_sync(|| self.vfs.lookup(&dir, &name))?;
        Ok(entry.fileid)
    }

    async fn getattr(&self, id: fileid3) -> Result<fattr3, nfsstat3> {
        let path = self.path_of(id)?;
        let meta = run_sync(|| self.vfs.stat(&path))?;
        Ok(self.fattr(id, meta))
    }

    async fn setattr(&self, _id: fileid3, _setattr: sattr3) -> Result<fattr3, nfsstat3> {
        // v1 writable scope: no mode/time changes (exec is tree metadata;
        // flush stages exec:false). Read-only mounts answer ROFS.
        Err(unsupported_op(self.vfs.writable()))
    }

    async fn read(
        &self,
        id: fileid3,
        offset: u64,
        count: u32,
    ) -> Result<(Vec<u8>, bool), nfsstat3> {
        let path = self.path_of(id)?;
        let bytes = run_sync(|| self.vfs.read(&path))?;
        let len = bytes.len() as u64;
        let start = offset.min(len) as usize;
        let end = offset.saturating_add(count as u64).min(len) as usize;
        Ok((bytes[start..end].to_vec(), end as u64 >= len))
    }

    async fn write(&self, id: fileid3, offset: u64, data: &[u8]) -> Result<fattr3, nfsstat3> {
        let path = self.path_of(id)?;
        let data = data.to_vec();
        let meta = run_sync(|| {
            let mut bytes = if offset == 0 {
                Vec::new()
            } else {
                self.vfs.read(&path)?
            };
            let off = offset as usize;
            if bytes.len() < off {
                bytes.resize(off, 0);
            }
            bytes.truncate(off);
            bytes.extend_from_slice(&data);
            self.vfs.write(&path, &bytes)
        })?;
        Ok(self.fattr(id, meta))
    }

    async fn create(
        &self,
        dirid: fileid3,
        filename: &filename3,
        _attr: sattr3,
    ) -> Result<(fileid3, fattr3), nfsstat3> {
        let dir = self.path_of(dirid)?;
        let name = utf8_name(filename)?;
        // POSIX creat: truncate when it exists. Attrs are fixed (0o644);
        // requested modes are not honored in v1 (setattr is unsupported).
        let meta = run_sync(|| self.vfs.create(&dir, &name))?;
        let entry = run_sync(|| self.vfs.lookup(&dir, &name))?;
        Ok((entry.fileid, self.fattr(entry.fileid, meta)))
    }

    async fn create_exclusive(
        &self,
        dirid: fileid3,
        filename: &filename3,
    ) -> Result<fileid3, nfsstat3> {
        let dir = self.path_of(dirid)?;
        let name = utf8_name(filename)?;
        if run_sync(|| self.vfs.lookup(&dir, &name)).is_ok() {
            return Err(nfsstat3::NFS3ERR_EXIST);
        }
        run_sync(|| self.vfs.create(&dir, &name))?;
        let entry = run_sync(|| self.vfs.lookup(&dir, &name))?;
        Ok(entry.fileid)
    }

    async fn mkdir(
        &self,
        _dirid: fileid3,
        _dirname: &filename3,
    ) -> Result<(fileid3, fattr3), nfsstat3> {
        // v1 writable scope: file create + write only. New directories are
        // out (documented); read-only mounts answer ROFS.
        Err(unsupported_op(self.vfs.writable()))
    }

    async fn remove(&self, _dirid: fileid3, _filename: &filename3) -> Result<(), nfsstat3> {
        // Deletes are outside v1 writable scope; read-only answers ROFS.
        Err(unsupported_op(self.vfs.writable()))
    }

    async fn rename(
        &self,
        _from_dirid: fileid3,
        _from_filename: &filename3,
        _to_dirid: fileid3,
        _to_filename: &filename3,
    ) -> Result<(), nfsstat3> {
        Err(unsupported_op(self.vfs.writable()))
    }

    async fn readdir(
        &self,
        dirid: fileid3,
        start_after: fileid3,
        max_entries: usize,
    ) -> Result<ReadDirResult, nfsstat3> {
        let dir = self.path_of(dirid)?;
        let listed = run_sync(|| self.vfs.readdir(&dir))?;
        // nfsserve's contract: start_after is the LAST entry of the previous
        // page — resume at its positional successor (0 = from the start).
        let mut entries = Vec::with_capacity(max_entries.min(listed.len()));
        let mut started = start_after == 0;
        let mut end = true;
        for e in listed {
            if !started {
                if e.fileid == start_after {
                    started = true;
                }
                continue;
            }
            if entries.len() >= max_entries {
                end = false;
                break;
            }
            entries.push(DirEntry {
                fileid: e.fileid,
                name: e.name.into_bytes().into(),
                attr: self.fattr(e.fileid, e.meta),
            });
        }
        Ok(ReadDirResult { entries, end })
    }

    async fn symlink(
        &self,
        _dirid: fileid3,
        _linkname: &filename3,
        _symlink: &nfspath3,
        _attr: &sattr3,
    ) -> Result<(fileid3, fattr3), nfsstat3> {
        // naatre trees carry no symlinks (import rejects them).
        Err(unsupported_op(self.vfs.writable()))
    }

    async fn readlink(&self, _id: fileid3) -> Result<nfspath3, nfsstat3> {
        Err(nfsstat3::NFS3ERR_INVAL)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp_root(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "naatre-mount-ut-{tag}-{}-{}",
            std::process::id(),
            crate::wc::now_ns()
        ));
        std::fs::create_dir_all(&dir).expect("mkdir");
        dir
    }

    #[test]
    fn export_ids_are_32_hex_and_unguessable() {
        let a = new_export_id();
        let b = new_export_id();
        assert_eq!(a.len(), 32);
        assert!(a.chars().all(|c| c.is_ascii_hexdigit()));
        assert_ne!(a, b, "fresh CSPRNG bytes per mount");
    }

    #[test]
    fn mount_plan_defaults_to_loopback_ephemeral_with_hidden_export() {
        let root = tmp_root("plan");
        let plan = MountPlan::new(None, root.clone(), false).expect("plan");
        assert_eq!(plan.listen_addr, "127.0.0.1:0");
        assert!(!plan.non_loopback(), "loopback by default");
        let id = plan.export.strip_prefix("naatre/").expect("export prefix");
        assert_eq!(id.len(), 32);
        assert_eq!(plan.export_path(), format!("/naatre/{id}"));
        assert_eq!(plan.mount_dir, root.join(id));
        assert!(!plan.writable);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn non_loopback_listen_is_opt_in_with_the_serve_warning() {
        let root = tmp_root("plan2");
        let plan = MountPlan::new(Some("0.0.0.0:20490"), root, false).expect("plan");
        assert!(plan.non_loopback());
        let warning = cleartext_warning(&plan.listen_addr);
        // The same cleartext warning `serve` prints (host.rs), verbatim shape.
        assert!(warning.contains("0.0.0.0:20490 is non-loopback"));
        assert!(warning.contains("cleartext"));
    }

    #[test]
    fn invalid_listen_is_a_usage_error() {
        let root = tmp_root("plan3");
        assert!(MountPlan::new(Some("nope"), root, false).is_err());
    }

    #[test]
    fn mount_commands_pin_port_and_flavor() {
        let (macos, linux) = mount_commands(51412, "/naatre/abc");
        assert!(macos.contains("mount_nfs"), "{macos}");
        assert!(
            macos.contains("port=51412") && macos.contains("mountport=51412"),
            "{macos}"
        );
        assert!(macos.contains("nfsvers=3"), "{macos}");
        assert!(macos.contains("127.0.0.1:/naatre/abc"), "{macos}");
        assert!(linux.contains("mount -t nfs"), "{linux}");
        assert!(linux.contains("vers=3"), "{linux}");
        assert!(linux.contains("port=51412"), "{linux}");
    }

    #[test]
    fn stale_mount_dirs_are_evicted_live_ones_kept() {
        let root = tmp_root("prune");
        // A dead pid: spawn a child, wait for its exit, reuse its pid.
        let mut child = std::process::Command::new("true")
            .stdout(std::process::Stdio::null())
            .spawn()
            .expect("spawn");
        let dead_pid = child.id();
        child.wait().expect("wait");

        let mk = |name: &str, pid: Option<u32>| {
            let dir = root.join(name);
            std::fs::create_dir_all(&dir).expect("mkdir");
            if let Some(pid) = pid {
                std::fs::write(
                    dir.join("session.json"),
                    format!(r#"{{"pid": {pid}, "created_ns": 0}}"#),
                )
                .expect("session");
            }
            dir
        };
        let dead_dir = mk("dead", Some(dead_pid));
        let live_dir = mk("live", Some(std::process::id()));
        let corrupt_dir = mk("corrupt", None);
        std::fs::write(corrupt_dir.join("session.json"), b"not json").expect("corrupt");

        prune_stale_mounts(&root);
        assert!(!dead_dir.exists(), "dead-pid dir evicted");
        assert!(!corrupt_dir.exists(), "corrupt-session dir evicted");
        assert!(live_dir.exists(), "this process's dir stays");
        let _ = std::fs::remove_dir_all(&root);
    }
}
