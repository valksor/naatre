//! The Linux-only FUSE front-end (`naatre mount <repo> --fuse <mountpoint>`,
//! plan M8b): adapts the SAME pinned [`NaatreVfs`] the NFS front-end serves
//! into the kernel's FUSE protocol via `fuser` (pure-Rust; it mounts through
//! the setuid `fusermount3`, which must be installed).
//!
//! Differences from the NFS front-end that the README pins:
//! - **No AUTH_SYS exposure**: the mount carries `SessionACL::Owner`, so
//!   ONLY the mounting user can access it — the kernel enforces the check,
//!   there is no network surface at all.
//! - **Stop**: SIGINT/SIGTERM unmounts first (flush still runs on writable
//!   mounts); an external `umount <mountpoint>` also stops the daemon
//!   cleanly, the same flush path runs either way. The handlers install
//!   before the banner prints, so a signal arriving while `spawn_mount`
//!   (the fusermount3 round-trip) is still running is BUFFERED and takes
//!   effect once the mount completes.
//! - Attribute TTL is 0: the writable overlay changes sizes under stable
//!   paths, so attribute caching is off (stat is cheap — tree entries only).
//!
//! v1 writable scope is the NFS one: file create + write only (grammar +
//! NFC enforced at create through [`NaatreVfs::create`]); setattr, mkdir,
//! unlink, rename, symlinks answer EPERM (EROFS on a read-only mount).

use std::ffi::OsStr;
use std::os::unix::ffi::OsStrExt as _;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use fuser::{
    BsdFileFlags, Config, Errno, FileAttr, FileHandle, FileType, Filesystem, FopenFlags,
    Generation, INodeNo, KernelConfig, LockOwner, MountOption, OpenFlags, RenameFlags, ReplyAttr,
    ReplyCreate, ReplyData, ReplyDirectory, ReplyEmpty, ReplyEntry, ReplyOpen, ReplyWrite, Request,
    SessionACL, TimeOrNow, WriteFlags,
};

use super::{Net, VerbError};
use crate::vfs::{
    ERRNO_HASH_MISMATCH, ERRNO_INVALID_PATH, ERRNO_IS_DIR, ERRNO_NOT_DIR, ERRNO_NOT_FOUND,
    ERRNO_READ_ONLY, EntryMeta, HttpMountSource, NaatreVfs, VfsError,
};
use naatre_core::tree::EntryKind;

/// `naatre mount <repo> --fuse <mountpoint>` — the Linux daemon. Returns
/// the pinned exit code (same contract as the NFS daemon).
pub fn run(
    repo: &str,
    at: Option<&str>,
    writable: bool,
    flush_land: bool,
    mountpoint: &str,
    net: &Net,
) -> i32 {
    if flush_land && !writable {
        return VerbError::Usage(
            "--flush-land requires --writable (the read-only default has \
                                 nothing to flush)"
                .into(),
        )
        .report();
    }
    let cache_root = match super::mount::mount_cache_root() {
        Ok(r) => r,
        Err(m) => return VerbError::Local(m).report(),
    };
    let mounts_root = cache_root.join("naatre").join("mounts");
    // The plan carries the per-mount dir naming + cache layout (the bind
    // address is NFS-specific and unused here).
    let plan = match super::mount::MountPlan::new(None, mounts_root, writable, flush_land) {
        Ok(p) => p,
        Err(m) => return VerbError::Usage(m).report(),
    };
    super::mount::prune_stale_mounts(&plan.mounts_root);
    // The client resolves BEFORE the pin (same clean-error order as NFS).
    let client = match super::mount::mount_client(net) {
        Ok(c) => c,
        Err(m) => return VerbError::Local(m).report(),
    };
    let vfs = match NaatreVfs::mount(
        HttpMountSource::new(client, repo),
        repo,
        at,
        plan.mount_dir.clone(),
        writable,
    ) {
        Ok(v) => v,
        Err(e) => {
            let _ = std::fs::remove_dir_all(&plan.mount_dir);
            return super::mount::vfs_error_verb(e);
        }
    };
    if let Err(e) = super::mount::write_session(&plan.mount_dir) {
        let _ = std::fs::remove_dir_all(&plan.mount_dir);
        return VerbError::Local(format!("writing the mount session: {e}")).report();
    }
    // fusermount3 requires an existing mountpoint.
    if let Err(e) = std::fs::create_dir_all(mountpoint) {
        let _ = std::fs::remove_dir_all(&plan.mount_dir);
        return VerbError::Local(format!("creating the mountpoint: {e}")).report();
    }

    let vfs = Arc::new(vfs);
    let started = SystemTime::now();
    let adapter = FuseAdapter {
        vfs: vfs.clone(),
        uid: unsafe { libc::getuid() },
        gid: unsafe { libc::getgid() },
        started,
    };
    // Config is #[non_exhaustive]: default + field assignment, not a literal.
    let mut config = Config::default();
    config.mount_options = vec![
        MountOption::FSName("naatre".to_string()),
        MountOption::Subtype("naatre".to_string()),
    ];
    config.acl = SessionACL::Owner;
    config.n_threads = Some(4);
    // The async runtime comes first: the shutdown handlers install only
    // inside a runtime context, and they must exist before the banner —
    // the banner's last line promises "SIGINT/SIGTERM … stops the daemon",
    // and a client that reads that line and signals immediately must find
    // the handlers in place, never the default terminating action (the
    // vfs_test graceful-exit pin).
    let rt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            let _ = std::fs::remove_dir_all(&plan.mount_dir);
            return VerbError::Local(format!("starting the async runtime: {e}")).report();
        }
    };
    let shutdown = rt.block_on(async { naatre_server::serve::unix_shutdown_signal() });
    print_banner(&plan, repo, vfs.pin().changeset.to_hex(), mountpoint);
    // The FUSE ordering has NO runtime pin yet: the vfs ordering gate is
    // NFS-only because FUSE needs /dev/fuse (absent in the verification
    // container and on macOS), and the NFS pin covers the shared
    // `unix_shutdown_signal` contract the two front-ends install
    // identically — before their banners, which is what this placement
    // preserves.
    eprintln!(
        "mounting {mountpoint}: Ctrl-C/SIGTERM are caught now (buffered) and take effect once \
         the mount completes"
    );
    let session = match fuser::spawn_mount(adapter, mountpoint, &config) {
        Ok(s) => s,
        Err(e) => {
            eprintln!("error: mounting {mountpoint} through fusermount3: {e}");
            eprintln!("  (the FUSE front-end needs /dev/fuse and the fusermount3 setuid helper)");
            let _ = std::fs::remove_dir_all(&plan.mount_dir);
            return 1;
        }
    };

    // SIGINT/SIGTERM unmounts first; an external umount ends the session
    // on its own and the unmount below then errors harmlessly.
    rt.block_on(shutdown);
    if let Err(e) = session.umount_and_join() {
        eprintln!("note: FUSE session ended: {e}");
    }

    // The shared graceful-exit tail: flush (staged, or landed with
    // --flush-land), remove the mount dir — unless the flush failed (the
    // dir is then the recovery location) — and the exit code.
    let client = match super::mount::mount_client(net) {
        Ok(c) => c,
        Err(m) => {
            eprintln!("error: {m}");
            return 1;
        }
    };
    super::mount::flush_and_cleanup(&plan, &vfs, &client)
}

fn print_banner(plan: &super::mount::MountPlan, repo: &str, pinned_cs: String, mountpoint: &str) {
    let pin_note = if plan.flush_land {
        "writable (UNSTABLE; --flush-land lands staged writes onto main at exit)"
    } else if plan.writable {
        "writable (UNSTABLE)"
    } else {
        "read-only"
    };
    println!("naatre mount (FUSE): {repo} pinned at {pinned_cs} ({pin_note})");
    println!(
        "mounted at {mountpoint} — only the mounting user can access it (no network, no AUTH_SYS)"
    );
    println!(
        "cache:   {} (0700; hydrate cache LRU-capped at {} blobs; stale dirs evicted on next mount)",
        plan.mount_dir.display(),
        crate::vfs::BLOB_CACHE_MAX_ENTRIES
    );
    if plan.flush_land {
        println!(
            "SIGINT/SIGTERM or `umount {mountpoint}` stops the daemon (this mount LANDS its staged writes onto main)."
        );
    } else {
        println!(
            "SIGINT/SIGTERM or `umount {mountpoint}` stops the daemon (writable mounts stage their shadow writes first)."
        );
    }
}

/// Bridges a session fileid to its VFS path (unknown inos answer ENOENT —
/// the kernel drops the stale dentry, the right recovery).
fn path_of(vfs: &NaatreVfs<HttpMountSource>, ino: INodeNo) -> Result<String, Errno> {
    vfs.path_of_fileid(ino.0).ok_or(Errno::ENOENT)
}

/// Non-UTF8 names cannot be naatre paths.
fn utf8_name(name: &OsStr) -> Result<String, Errno> {
    std::str::from_utf8(name.as_bytes())
        .map(str::to_string)
        .map_err(|_| Errno::EINVAL)
}

/// Runs one VFS call, printing hydration failures (they name the object id
/// — the plan's failure-reporting pin, same as the NFS front-end) and
/// mapping the errno table into fuser's [`Errno`].
fn vfs_call<T>(f: impl FnOnce() -> Result<T, VfsError>) -> Result<T, Errno> {
    f().map_err(|e| {
        if let VfsError::Hydration(h) = &e {
            eprintln!("{}", crate::vfs::hydration_log_line(h));
        }
        match e.errno() {
            ERRNO_NOT_FOUND => Errno::ENOENT,
            ERRNO_NOT_DIR => Errno::ENOTDIR,
            ERRNO_IS_DIR => Errno::EISDIR,
            ERRNO_READ_ONLY => Errno::EROFS,
            ERRNO_INVALID_PATH => Errno::EINVAL,
            ERRNO_HASH_MISMATCH => Errno::ESTALE,
            _ => Errno::EIO,
        }
    })
}

/// A write-path op the VFS layer itself rejects up front (read-only mounts
/// answer ROFS — the EROFS-default pin — writable v1 answers PERM for the
/// ops outside its create+write scope).
fn unsupported_op(writable: bool) -> Errno {
    if writable { Errno::EPERM } else { Errno::EROFS }
}

/// A write may land mid-flight while a writable mount serves; the overlay
/// changes sizes under stable paths, so attributes are never cached.
const ATTR_TTL: Duration = Duration::new(0, 0);

/// The FUSE adapter over one pinned [`NaatreVfs`].
struct FuseAdapter {
    vfs: Arc<NaatreVfs<HttpMountSource>>,
    uid: u32,
    gid: u32,
    started: SystemTime,
}

impl FuseAdapter {
    /// Attributes: kind/size/exec from the tree entry; times are the
    /// daemon's start (a pinned tree carries no timestamps — the same
    /// convention the NFS front-end documents).
    fn attr(&self, ino: INodeNo, meta: EntryMeta) -> FileAttr {
        let (kind, perm) = match meta.kind {
            EntryKind::File => (FileType::RegularFile, if meta.exec { 0o755 } else { 0o644 }),
            EntryKind::Dir => (FileType::Directory, 0o755),
        };
        FileAttr {
            ino,
            size: meta.size,
            blocks: meta.size.div_ceil(512),
            atime: self.started,
            mtime: self.started,
            ctime: self.started,
            crtime: self.started,
            kind,
            perm,
            nlink: 1,
            uid: self.uid,
            gid: self.gid,
            rdev: 0,
            blksize: 4096,
            flags: 0,
        }
    }
}

impl Filesystem for FuseAdapter {
    fn init(&mut self, _req: &Request, _config: &mut KernelConfig) -> Result<(), std::io::Error> {
        Ok(())
    }

    fn lookup(&self, _req: &Request, parent: INodeNo, name: &OsStr, reply: ReplyEntry) {
        let dir = match path_of(&self.vfs, parent) {
            Ok(d) => d,
            Err(e) => return reply.error(e),
        };
        let name = match utf8_name(name) {
            Ok(n) => n,
            Err(e) => return reply.error(e),
        };
        match vfs_call(|| self.vfs.lookup(&dir, &name)) {
            Ok(entry) => reply.entry(
                &ATTR_TTL,
                &self.attr(INodeNo(entry.fileid), entry.meta),
                Generation(0),
            ),
            Err(e) => reply.error(e),
        }
    }

    fn getattr(&self, _req: &Request, ino: INodeNo, _fh: Option<FileHandle>, reply: ReplyAttr) {
        let path = match path_of(&self.vfs, ino) {
            Ok(p) => p,
            Err(e) => return reply.error(e),
        };
        match vfs_call(|| self.vfs.stat(&path)) {
            Ok(meta) => reply.attr(&ATTR_TTL, &self.attr(ino, meta)),
            Err(e) => reply.error(e),
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn setattr(
        &self,
        _req: &Request,
        _ino: INodeNo,
        _mode: Option<u32>,
        _uid: Option<u32>,
        _gid: Option<u32>,
        _size: Option<u64>,
        _atime: Option<TimeOrNow>,
        _mtime: Option<TimeOrNow>,
        _ctime: Option<SystemTime>,
        _fh: Option<FileHandle>,
        _crtime: Option<SystemTime>,
        _chgtime: Option<SystemTime>,
        _bkuptime: Option<SystemTime>,
        _flags: Option<BsdFileFlags>,
        reply: ReplyAttr,
    ) {
        // v1 writable scope: no mode/time changes (exec is tree metadata;
        // flush stages exec:false). Read-only mounts answer ROFS.
        reply.error(unsupported_op(self.vfs.writable()));
    }

    fn readlink(&self, _req: &Request, _ino: INodeNo, reply: ReplyData) {
        // naatre trees carry no symlinks (import rejects them).
        reply.error(Errno::EINVAL)
    }

    fn mknod(
        &self,
        _req: &Request,
        _parent: INodeNo,
        _name: &OsStr,
        _mode: u32,
        _umask: u32,
        _rdev: u32,
        reply: ReplyEntry,
    ) {
        // v1 covers regular-file create only (devices/fifos are out).
        reply.error(unsupported_op(self.vfs.writable()))
    }

    fn mkdir(
        &self,
        _req: &Request,
        _parent: INodeNo,
        _name: &OsStr,
        _mode: u32,
        _umask: u32,
        reply: ReplyEntry,
    ) {
        // v1 writable scope: file create + write only. New directories are
        // out (documented); read-only mounts answer ROFS.
        reply.error(unsupported_op(self.vfs.writable()))
    }

    fn unlink(&self, _req: &Request, _parent: INodeNo, _name: &OsStr, reply: ReplyEmpty) {
        // Deletes are outside v1 writable scope; read-only answers ROFS.
        reply.error(unsupported_op(self.vfs.writable()))
    }

    fn rmdir(&self, _req: &Request, _parent: INodeNo, _name: &OsStr, reply: ReplyEmpty) {
        reply.error(unsupported_op(self.vfs.writable()))
    }

    fn symlink(
        &self,
        _req: &Request,
        _parent: INodeNo,
        _link_name: &OsStr,
        _target: &std::path::Path,
        reply: ReplyEntry,
    ) {
        reply.error(unsupported_op(self.vfs.writable()))
    }

    fn rename(
        &self,
        _req: &Request,
        _parent: INodeNo,
        _name: &OsStr,
        _newparent: INodeNo,
        _newname: &OsStr,
        _flags: RenameFlags,
        reply: ReplyEmpty,
    ) {
        reply.error(unsupported_op(self.vfs.writable()))
    }

    fn link(
        &self,
        _req: &Request,
        _ino: INodeNo,
        _newparent: INodeNo,
        _newname: &OsStr,
        reply: ReplyEntry,
    ) {
        reply.error(unsupported_op(self.vfs.writable()))
    }

    fn open(&self, _req: &Request, ino: INodeNo, _flags: OpenFlags, reply: ReplyOpen) {
        // Validate the path exists (and is a file); the handle itself
        // carries nothing (reads re-resolve the path).
        let path = match path_of(&self.vfs, ino) {
            Ok(p) => p,
            Err(e) => return reply.error(e),
        };
        match vfs_call(|| self.vfs.stat(&path)) {
            Ok(_) => reply.opened(FileHandle(0), FopenFlags::empty()),
            Err(e) => reply.error(e),
        }
    }

    fn read(
        &self,
        _req: &Request,
        ino: INodeNo,
        _fh: FileHandle,
        offset: u64,
        size: u32,
        _flags: OpenFlags,
        _lock_owner: Option<LockOwner>,
        reply: ReplyData,
    ) {
        let path = match path_of(&self.vfs, ino) {
            Ok(p) => p,
            Err(e) => return reply.error(e),
        };
        match vfs_call(|| self.vfs.read(&path)) {
            Ok(bytes) => {
                let len = bytes.len() as u64;
                let start = offset.min(len) as usize;
                let end = offset.saturating_add(size as u64).min(len) as usize;
                reply.data(&bytes[start..end]);
            }
            Err(e) => reply.error(e),
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn write(
        &self,
        _req: &Request,
        ino: INodeNo,
        _fh: FileHandle,
        offset: u64,
        data: &[u8],
        _write_flags: WriteFlags,
        _flags: OpenFlags,
        _lock_owner: Option<LockOwner>,
        reply: ReplyWrite,
    ) {
        let path = match path_of(&self.vfs, ino) {
            Ok(p) => p,
            Err(e) => return reply.error(e),
        };
        let data = data.to_vec();
        match vfs_call(|| {
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
        }) {
            Ok(_meta) => reply.written(data.len() as u32),
            Err(e) => reply.error(e),
        }
    }

    fn flush(
        &self,
        _req: &Request,
        _ino: INodeNo,
        _fh: FileHandle,
        _lock: LockOwner,
        reply: ReplyEmpty,
    ) {
        // Every staged write is already durable in the shadow file (atomic
        // rename per write); per-handle flush has nothing to push.
        reply.ok()
    }

    fn fsync(
        &self,
        _req: &Request,
        _ino: INodeNo,
        _fh: FileHandle,
        _datasync: bool,
        reply: ReplyEmpty,
    ) {
        reply.ok()
    }

    fn readdir(
        &self,
        _req: &Request,
        ino: INodeNo,
        _fh: FileHandle,
        offset: u64,
        mut reply: ReplyDirectory,
    ) {
        let dir = match path_of(&self.vfs, ino) {
            Ok(d) => d,
            Err(e) => return reply.error(e),
        };
        let listed = match vfs_call(|| self.vfs.readdir(&dir)) {
            Ok(l) => l,
            Err(e) => return reply.error(e),
        };
        for (index, entry) in listed.iter().skip(offset as usize).enumerate() {
            let kind = match entry.meta.kind {
                EntryKind::File => FileType::RegularFile,
                EntryKind::Dir => FileType::Directory,
            };
            let buffer_full = reply.add(
                INodeNo(entry.fileid),
                offset + index as u64 + 1,
                kind,
                OsStr::new(&entry.name),
            );
            if buffer_full {
                break;
            }
        }
        reply.ok();
    }

    fn create(
        &self,
        _req: &Request,
        parent: INodeNo,
        name: &OsStr,
        _mode: u32,
        _umask: u32,
        _flags: i32,
        reply: ReplyCreate,
    ) {
        let dir = match path_of(&self.vfs, parent) {
            Ok(d) => d,
            Err(e) => return reply.error(e),
        };
        let name = match utf8_name(name) {
            Ok(n) => n,
            Err(e) => return reply.error(e),
        };
        // POSIX creat: truncate when it exists. Attrs are fixed (0o644);
        // requested modes are not honored in v1 (setattr is unsupported) —
        // the same contract the NFS front-end documents.
        let created = match vfs_call(|| self.vfs.create(&dir, &name)) {
            Ok(m) => m,
            Err(e) => return reply.error(e),
        };
        let entry = match vfs_call(|| self.vfs.lookup(&dir, &name)) {
            Ok(e) => e,
            Err(e) => return reply.error(e),
        };
        reply.created(
            &ATTR_TTL,
            &self.attr(INodeNo(entry.fileid), created),
            Generation(0),
            FileHandle(0),
            FopenFlags::empty(),
        );
    }
}
