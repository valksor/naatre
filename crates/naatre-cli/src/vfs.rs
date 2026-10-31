//! The `naatre mount` VFS (plan M8): a pure, path-based filesystem view over
//! one PINNED changeset, lazily hydrated from the server.
//!
//! Layers: [`NaatreVfs`] is the in-process layer the test harness exercises
//! without any kernel mount (`lookup`/`readdir`/`read`/`stat`); the NFSv3
//! front-end in `verbs::mount` adapts it to `nfsserve`. The pin is resolved
//! ONCE at [`NaatreVfs::mount`] (ref → changeset → root tree) and the view
//! serves that tree forever — advancing the ref afterwards changes nothing
//! (pinned in the mount verb's docs and tested in `tests/vfs_test.rs`).
//!
//! Hydration: directories load on first list, files on first read. Sizes and
//! exec bits come from tree entries, so a stat-only walk never fetches a
//! blob (pinned by a counting-source test). Every blob is hash-verified
//! against its id on hydration and again when loaded from the on-disk cache
//! — a poisoned cache file is detected, evicted, and re-hydrated (the
//! corrupt-cache recovery check).
//!
//! Hydrate cache: `<cache-root>/naatre/mounts/<export>/blobs/`, one 0600
//! file per blob id; `<cache-root>` is `$XDG_CACHE_HOME` or `~/.cache` (the
//! same layout on macOS — XDG first, `~/.cache` fallback). Dirs are 0700.
//! Eviction is LRU bounded by COUNT: at most [`BLOB_CACHE_MAX_ENTRIES`]
//! (1024) blobs per mount; the oldest-touched file is deleted when the
//! bound is exceeded. A mount whose daemon was SIGKILLed leaves its dir
//! behind; the next mount evicts stale dirs whose `session.json` pid is
//! dead (`verbs::mount::prune_stale_mounts`).
//!
//! Failure reporting: hydration errors are typed ([`HydrationError`]), name
//! the object id, and surface a DISTINCT errno per class — `MissingId` →
//! `EIO` (5), `HashMismatch` → `ESTALE` (70 on macOS, 116 on Linux; stale
//! content — the client re-lookup is the right recovery). The daemon's
//! stderr names the id (`hydration_log_line`); front-ends map
//! [`VfsError::errno`] into their error space.
//!
//! Writable mode (the UNSTABLE `--writable` flag): the default is read-only
//! — every write op asserts EROFS. With `--writable`, file-create enforces
//! the path grammar + NFC at create (rejected with `EINVAL`, naming why)
//! and the case-fold sibling rule; writes buffer into the mount's shadow
//! dir; [`NaatreVfs::flush`] builds blobs and a STAGED changeset on top of
//! the pinned changeset and returns its id — nothing is landed anywhere;
//! each flush stages a new changeset and the shadow stays (re-flush
//! re-stages). v1 writable scope: file create + write only (no
//! mkdir/unlink/rename/symlink — the front-ends answer EPERM for them).

use std::collections::{BTreeMap, HashMap, HashSet, VecDeque};
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, Ordering};

use naatre_core::ids::{Id, Kind, is_hex_id, object_id};
use naatre_core::path;
use naatre_core::tree::{EntryKind, TreeEntry};
use serde_json::{Value, json};

use crate::client::{Client, HttpError, uri_segment};
use crate::wc::nfc;

/// Blob-count bound per mount's hydrate cache (LRU; supervisor judgment).
pub const BLOB_CACHE_MAX_ENTRIES: usize = 1024;

/// EIO (5): the tree references an object the server cannot serve.
pub const ERRNO_MISSING_ID: i32 = 5;
/// ESTALE: stored bytes no longer hash to their address (corrupt content).
/// 70 on macOS/BSD, 116 on Linux.
#[cfg(target_os = "macos")]
pub const ERRNO_HASH_MISMATCH: i32 = 70;
#[cfg(not(target_os = "macos"))]
pub const ERRNO_HASH_MISMATCH: i32 = 116;
/// ENOENT (2).
pub const ERRNO_NOT_FOUND: i32 = 2;
/// ENOTDIR (20).
pub const ERRNO_NOT_DIR: i32 = 20;
/// EISDIR (21).
pub const ERRNO_IS_DIR: i32 = 21;
/// EROFS (30): every write op on a read-only mount (the default).
pub const ERRNO_READ_ONLY: i32 = 30;
/// EINVAL (22): path grammar / NFC rejection at create.
pub const ERRNO_INVALID_PATH: i32 = 22;

/// A hydration failure, naming the object id (the plan's failure-reporting
/// pin). Never cached: a failed hydration leaves no trace.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum HydrationError {
    /// The tree references an object the source cannot serve.
    MissingId(Id),
    /// Bytes arrived but do not hash to the requested id.
    HashMismatch { want: Id, got: Id },
}

impl std::fmt::Display for HydrationError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            HydrationError::MissingId(id) => write!(
                f,
                "missing object {} (referenced by the mounted tree)",
                id.to_hex()
            ),
            HydrationError::HashMismatch { want, got } => write!(
                f,
                "hash mismatch for object {}: server sent bytes hashing to {}",
                want.to_hex(),
                got.to_hex()
            ),
        }
    }
}

impl HydrationError {
    /// The object id the failure names (the stderr pin).
    pub fn object_id(&self) -> Id {
        match self {
            HydrationError::MissingId(id) => *id,
            HydrationError::HashMismatch { want, .. } => *want,
        }
    }

    /// The distinct errno per class (see the module docs).
    pub fn errno(&self) -> i32 {
        match self {
            HydrationError::MissingId(_) => ERRNO_MISSING_ID,
            HydrationError::HashMismatch { .. } => ERRNO_HASH_MISMATCH,
        }
    }
}

/// The line the mount daemon prints to stderr when hydration fails for an
/// object (the plan pins: the id is NAMED on the daemon's stderr).
pub fn hydration_log_line(err: &HydrationError) -> String {
    format!(
        "naatre mount: hydration failed: {} (errno {})",
        err,
        err.errno()
    )
}

/// A VFS failure. `errno()` maps each class to a distinct errno (module
/// docs); front-ends translate.
#[derive(Debug)]
pub enum VfsError {
    /// Server answered with an error envelope, or transport failed.
    Http(HttpError),
    /// Path not in the (pinned) tree or the writable overlay.
    NotFound(String),
    /// A directory was required, a file is there.
    NotADirectory(String),
    /// A file was required, a directory is there.
    IsADirectory(String),
    /// Hydration failed (missing object / hash mismatch), naming the id.
    Hydration(HydrationError),
    /// A write op on a read-only mount (the default).
    ReadOnly,
    /// Path grammar / NFC rejection (writable create), naming why.
    InvalidPath(String),
    /// Malformed server data or local I/O.
    Local(String),
}

impl std::fmt::Display for VfsError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            VfsError::Http(e) => write!(f, "{e}"),
            VfsError::NotFound(p) => write!(f, "no such path: {p}"),
            VfsError::NotADirectory(p) => write!(f, "not a directory: {p}"),
            VfsError::IsADirectory(p) => write!(f, "is a directory: {p}"),
            VfsError::Hydration(e) => write!(f, "{e}"),
            VfsError::ReadOnly => {
                f.write_str("read-only mount (pass --writable for the unstable write mode)")
            }
            VfsError::InvalidPath(m) => write!(f, "invalid path: {m}"),
            VfsError::Local(m) => f.write_str(m),
        }
    }
}

impl VfsError {
    /// The distinct errno for this failure class (module docs). Transport
    /// and local failures share EIO — they are not data-integrity classes.
    pub fn errno(&self) -> i32 {
        match self {
            VfsError::NotFound(_) => ERRNO_NOT_FOUND,
            VfsError::NotADirectory(_) => ERRNO_NOT_DIR,
            VfsError::IsADirectory(_) => ERRNO_IS_DIR,
            VfsError::Hydration(e) => e.errno(),
            VfsError::ReadOnly => ERRNO_READ_ONLY,
            VfsError::InvalidPath(_) => ERRNO_INVALID_PATH,
            VfsError::Http(_) | VfsError::Local(_) => ERRNO_MISSING_ID,
        }
    }
}

impl From<HttpError> for VfsError {
    fn from(e: HttpError) -> Self {
        VfsError::Http(e)
    }
}

/// Stat metadata: everything a stat needs, all of it from tree entries —
/// a stat never fetches a blob.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct EntryMeta {
    pub kind: EntryKind,
    pub size: u64,
    pub exec: bool,
}

/// One child of a listed directory.
#[derive(Debug, Clone)]
pub struct ListedEntry {
    pub name: String,
    /// The VFS session fileid (root = 1; stable for the mount's lifetime).
    pub fileid: u64,
    pub meta: EntryMeta,
}

/// The mount's pin: resolved once at [`NaatreVfs::mount`], served forever.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct MountPin {
    /// The changeset id the ref resolved to at mount time.
    pub changeset: Id,
    /// That changeset's root tree id.
    pub root_tree: Id,
}

/// What [`NaatreVfs::flush`] staged (nothing landed anywhere: the plan's
/// v1 writable contract is stage-and-print).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FlushOutcome {
    /// The staged changeset's id (parent = the pinned changeset).
    pub changeset: Id,
    /// The server-derived root tree of the staged changeset.
    pub root_tree: Id,
    /// How many shadow paths the changeset carries.
    pub files: usize,
}

/// One staged file change for [`MountSource::create_changeset`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ChangeLine {
    pub path: String,
    /// "add" or "mod" (deletes are outside v1 writable scope).
    pub op: &'static str,
    pub blob: Id,
    pub size: u64,
    pub exec: bool,
}

/// The server reads a mounted repo needs. Split from [`Client`] so tests
/// can inject in-memory sources ([`MemorySource`]) and count calls
/// ([`RecordingSource`]) — the M7 value-core / recording-client pattern.
pub trait MountSource: Send + Sync {
    /// Resolves a ref name to its target changeset (unborn → error).
    fn ref_target(&self, name: &str) -> Result<Id, VfsError>;
    /// A changeset's root tree id.
    fn changeset_root(&self, cs: &Id) -> Result<Id, VfsError>;
    /// A tree's entries (metadata only — never blobs).
    fn tree(&self, id: &Id) -> Result<Vec<TreeEntry>, VfsError>;
    /// A blob's bytes (implementations hash-verify; see [`HttpMountSource`]).
    fn blob(&self, id: &Id) -> Result<Vec<u8>, VfsError>;
    /// Stores a blob (writable flush). Read-only-only sources error.
    fn put_blob(&self, id: &Id, bytes: &[u8]) -> Result<(), VfsError>;
    /// Stages a changeset (writable flush); returns (id, root_tree_id).
    fn create_changeset(
        &self,
        parents: &[Id],
        author: &str,
        message: &str,
        timestamp_ns: u64,
        changes: &[ChangeLine],
    ) -> Result<(Id, Id), VfsError>;
}

/// The real source: HTTP against one repo through the shared client. Every
/// blob is hash-verified here, so NO consumer of [`MountSource`] can skip
/// the verification gate.
pub struct HttpMountSource {
    client: Client,
    repo: String,
}

impl HttpMountSource {
    pub fn new(client: Client, repo: &str) -> HttpMountSource {
        HttpMountSource {
            client,
            repo: repo.to_string(),
        }
    }

    fn objects_base(&self) -> String {
        format!("/v1/repos/{}/objects", uri_segment(&self.repo))
    }
}

impl MountSource for HttpMountSource {
    fn ref_target(&self, name: &str) -> Result<Id, VfsError> {
        let v = self.client.get_json(
            &format!(
                "/v1/repos/{}/refs/{}",
                uri_segment(&self.repo),
                uri_segment(name)
            ),
            &[],
        )?;
        v["target"]
            .as_str()
            .and_then(Id::from_hex)
            .ok_or_else(|| VfsError::Local(format!("ref {name:?} is unborn")))
    }

    fn changeset_root(&self, cs: &Id) -> Result<Id, VfsError> {
        let v = self.client.get_json(
            &format!(
                "/v1/repos/{}/changesets/{}/root",
                uri_segment(&self.repo),
                cs.to_hex()
            ),
            &[],
        )?;
        v["root_tree_id"]
            .as_str()
            .and_then(Id::from_hex)
            .ok_or_else(|| VfsError::Local("server returned a malformed root tree id".into()))
    }

    fn tree(&self, id: &Id) -> Result<Vec<TreeEntry>, VfsError> {
        let v = self.client.get_json(
            &format!(
                "/v1/repos/{}/trees/{}",
                uri_segment(&self.repo),
                id.to_hex()
            ),
            &[],
        )?;
        parse_tree(&v)
    }

    fn blob(&self, id: &Id) -> Result<Vec<u8>, VfsError> {
        let bytes = self
            .client
            .get_bytes(&format!("{}/{}", self.objects_base(), id.to_hex()))?;
        verify(id, &bytes)?;
        Ok(bytes)
    }

    fn put_blob(&self, id: &Id, bytes: &[u8]) -> Result<(), VfsError> {
        self.client
            .put_bytes(&format!("{}/{}", self.objects_base(), id.to_hex()), bytes)?;
        Ok(())
    }

    fn create_changeset(
        &self,
        parents: &[Id],
        author: &str,
        message: &str,
        timestamp_ns: u64,
        changes: &[ChangeLine],
    ) -> Result<(Id, Id), VfsError> {
        let body = json!({
            "parents": parents.iter().map(Id::to_hex).collect::<Vec<_>>(),
            "author": author,
            "message": message,
            "timestamp_ns": timestamp_ns,
            "changes": changes.iter().map(|c| json!({
                "path": c.path, "op": c.op, "blob": c.blob.to_hex(),
                "size": c.size, "exec": c.exec,
            })).collect::<Vec<_>>(),
        });
        let (_status, created) = self.client.post_json(
            &format!("/v1/repos/{}/changesets", uri_segment(&self.repo)),
            &body,
        )?;
        let id = created["id"]
            .as_str()
            .and_then(Id::from_hex)
            .ok_or_else(|| VfsError::Local("server returned a malformed changeset id".into()))?;
        let root = created["root_tree_id"]
            .as_str()
            .and_then(Id::from_hex)
            .ok_or_else(|| VfsError::Local("server returned a malformed root tree id".into()))?;
        Ok((id, root))
    }
}

/// Parses a server tree listing into core entries (untrusted data: every
/// field checked, ids hex-validated).
fn parse_tree(v: &Value) -> Result<Vec<TreeEntry>, VfsError> {
    let entries = v["entries"]
        .as_array()
        .ok_or_else(|| VfsError::Local("server sent a malformed tree listing".into()))?;
    let mut out = Vec::with_capacity(entries.len());
    for e in entries {
        let name = e["name"]
            .as_str()
            .ok_or_else(|| VfsError::Local("tree entry without a name".into()))?;
        let kind = match e["kind"].as_str() {
            Some("file") => EntryKind::File,
            Some("dir") => EntryKind::Dir,
            _ => {
                return Err(VfsError::Local(format!(
                    "tree entry {name:?} has a malformed kind"
                )));
            }
        };
        let object = e["object_id"]
            .as_str()
            .and_then(Id::from_hex)
            .ok_or_else(|| VfsError::Local(format!("tree entry {name:?} has a malformed id")))?;
        out.push(TreeEntry {
            name: name.to_string(),
            kind,
            object,
            size: e["size"].as_u64().unwrap_or(0),
            exec: e["exec"].as_bool().unwrap_or(false),
        });
    }
    Ok(out)
}

/// The pinned client-side integrity gate (same rule as `wc::fetch::verify`,
/// typed for the VFS).
fn verify(id: &Id, bytes: &[u8]) -> Result<(), VfsError> {
    let actual = object_id(Kind::Blob, bytes);
    if actual != *id {
        return Err(VfsError::Hydration(HydrationError::HashMismatch {
            want: *id,
            got: actual,
        }));
    }
    Ok(())
}

/// The on-disk hydrate cache: one 0600 file per blob id under the mount's
/// `blobs/` dir, LRU-evicted by count. Recency is in-memory (rebuilt from
/// file mtimes at open); the files themselves are what survives a SIGKILL.
pub struct BlobCache {
    dir: PathBuf,
    max_entries: usize,
    /// Blob ids on disk, least-recently-touched first.
    order: VecDeque<Id>,
    present: HashSet<Id>,
}

impl BlobCache {
    /// Opens (creating, 0700) the cache dir and indexes what a previous
    /// mount left there, oldest-first by mtime.
    pub fn open(dir: PathBuf, max_entries: usize) -> std::io::Result<BlobCache> {
        std::fs::create_dir_all(&dir)?;
        set_private_dir(&dir)?;
        let mut mtimed: Vec<(std::time::SystemTime, Id)> = Vec::new();
        for entry in std::fs::read_dir(&dir)? {
            let entry = entry?;
            let Ok(name) = entry.file_name().into_string() else {
                continue; // not ours (or mangled): leave it, ignore it
            };
            let Some(id) = Id::from_hex(&name) else {
                continue;
            };
            let mtime = entry
                .metadata()
                .and_then(|m| m.modified())
                .unwrap_or(std::time::SystemTime::UNIX_EPOCH);
            mtimed.push((mtime, id));
        }
        mtimed.sort_by_key(|(t, _)| *t);
        let order: VecDeque<Id> = mtimed.into_iter().map(|(_, id)| id).collect();
        let present = order.iter().copied().collect();
        Ok(BlobCache {
            dir,
            max_entries,
            order,
            present,
        })
    }

    /// Loads a cached blob, refreshing its recency. A directory entry whose
    /// file vanished is dropped from the index (not an error — re-hydrate).
    pub fn get(&mut self, id: &Id) -> Option<Vec<u8>> {
        if !self.present.contains(id) {
            return None;
        }
        match std::fs::read(self.file(id)) {
            Ok(bytes) => {
                self.touch(id);
                Some(bytes)
            }
            Err(_) => {
                self.evict(id);
                None
            }
        }
    }

    /// Writes a blob into the cache (0600, temp+rename) and evicts the
    /// oldest entries while over the count bound.
    pub fn insert(&mut self, id: &Id, bytes: &[u8]) -> std::io::Result<()> {
        if self.present.contains(id) {
            self.touch(id);
            return Ok(());
        }
        crate::wc::write_atomic(&self.file(id), bytes, true)?;
        self.order.push_back(*id);
        self.present.insert(*id);
        while self.order.len() > self.max_entries {
            let Some(oldest) = self.order.pop_front() else {
                break;
            };
            let _ = std::fs::remove_file(self.file(&oldest));
            self.present.remove(&oldest);
        }
        Ok(())
    }

    /// Drops a blob from the cache (the corrupt-cache recovery path).
    pub fn evict(&mut self, id: &Id) {
        let _ = std::fs::remove_file(self.file(id));
        self.present.remove(id);
        self.order.retain(|x| x != id);
    }

    pub fn len(&self) -> usize {
        self.order.len()
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.order.is_empty()
    }

    fn file(&self, id: &Id) -> PathBuf {
        self.dir.join(id.to_hex())
    }

    fn touch(&mut self, id: &Id) {
        self.order.retain(|x| x != id);
        self.order.push_back(*id);
    }
}

/// Sets a directory's mode to 0700 (the hydrate cache and shadow dir are
/// private to the mounting user).
fn set_private_dir(dir: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::PermissionsExt as _;
    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))
}

/// One read may recover a corrupt cache file (evict + re-hydrate); this
/// bounds the loop so a persistently poisoned cache surfaces as an error.
const CACHE_RETRY: usize = 2;

/// The VFS over one pinned changeset. Shared behind `&self` (the NFS
/// front-end hands out `Arc`); caches are interior-mutable.
pub struct NaatreVfs<S: MountSource> {
    source: S,
    repo: String,
    pin: MountPin,
    writable: bool,
    /// The mount dir holding session.json, blobs/, shadow/ (0700).
    mount_dir: PathBuf,
    inner: Mutex<Inner>,
    blobs: Mutex<BlobCache>,
}

struct Inner {
    /// Loaded directories by full path ("" = the root).
    dirs: HashMap<String, DirNode>,
    /// Every named path's session fileid (files and dirs), assigned on
    /// first sight.
    fileid_of: HashMap<String, u64>,
    path_of: HashMap<u64, String>,
    next_fileid: u64,
    /// The unstable writable overlay: staged paths → (shadow file, size).
    staged: BTreeMap<String, (PathBuf, u64)>,
}

struct DirNode {
    children: BTreeMap<String, (Id, EntryMeta)>,
}

impl<S: MountSource> NaatreVfs<S> {
    /// Mounts `repo` at `at` (a changeset id or ref name; default `main`),
    /// resolving the pin ONCE. Everything served afterwards comes from this
    /// pin — later ref advances are invisible (documented + tested).
    pub fn mount(
        source: S,
        repo: &str,
        at: Option<&str>,
        mount_dir: PathBuf,
        writable: bool,
    ) -> Result<NaatreVfs<S>, VfsError> {
        let cs = match at {
            None => source.ref_target("main")?,
            Some(a) if is_hex_id(a) => {
                Id::from_hex(a).ok_or_else(|| VfsError::Local(format!("bad changeset id {a:?}")))?
            }
            Some(refname) => source.ref_target(refname)?,
        };
        let root_tree = source.changeset_root(&cs)?;
        let blobs = BlobCache::open(mount_dir.join("blobs"), BLOB_CACHE_MAX_ENTRIES)
            .map_err(|e| VfsError::Local(format!("opening the hydrate cache: {e}")))?;
        let mut inner = Inner {
            dirs: HashMap::new(),
            fileid_of: HashMap::new(),
            path_of: HashMap::new(),
            next_fileid: 2,
            staged: BTreeMap::new(),
        };
        // Fileid 1 is the root (nfsserve reserves 0).
        inner.fileid_of.insert(String::new(), 1);
        inner.path_of.insert(1, String::new());
        Ok(NaatreVfs {
            source,
            repo: repo.to_string(),
            pin: MountPin {
                changeset: cs,
                root_tree,
            },
            writable,
            mount_dir,
            inner: Mutex::new(inner),
            blobs: Mutex::new(blobs),
        })
    }

    /// The mount-time pin (the changeset the ref resolved to, its root
    /// tree) — served for the daemon's whole life.
    pub fn pin(&self) -> MountPin {
        self.pin
    }

    pub fn repo(&self) -> &str {
        &self.repo
    }

    pub fn mount_dir(&self) -> &Path {
        &self.mount_dir
    }

    pub fn writable(&self) -> bool {
        self.writable
    }

    /// The root fileid (nfsserve reserves 0).
    pub fn root_fileid(&self) -> u64 {
        1
    }

    /// The path a session fileid was assigned to (NFS front-end handle
    /// resolution); `None` = never seen (the front-end answers STALE).
    pub fn path_of_fileid(&self, id: u64) -> Option<String> {
        self.inner
            .lock()
            .expect("vfs lock")
            .path_of
            .get(&id)
            .cloned()
    }

    /// Stat: kind/size/exec from tree entries — never a blob fetch.
    /// `""` stats the root directory.
    pub fn stat(&self, path: &str) -> Result<EntryMeta, VfsError> {
        let path = nfc(path);
        if path.is_empty() {
            return Ok(EntryMeta {
                kind: EntryKind::Dir,
                size: 0,
                exec: false,
            });
        }
        let (parent, name) = split_last(&path);
        let mut inner = self.inner.lock().expect("vfs lock");
        self.ensure_dir_locked(&mut inner, parent)?;
        // The unstable overlay answers first for staged paths.
        if let Some((_file, size)) = inner.staged.get(&path) {
            let meta = EntryMeta {
                kind: EntryKind::File,
                size: *size,
                exec: false,
            };
            inner.assign_fileid(&path);
            return Ok(meta);
        }
        let node = inner
            .dirs
            .get(parent)
            .ok_or_else(|| VfsError::Local(format!("internal: {parent:?} not loaded")))?;
        let meta = node
            .children
            .get(name)
            .map(|(_, m)| *m)
            .ok_or_else(|| VfsError::NotFound(path.clone()))?;
        inner.assign_fileid(&path);
        Ok(meta)
    }

    /// Lookup: one component resolved inside one dir (dir "" = the root).
    pub fn lookup(&self, dir: &str, name: &str) -> Result<ListedEntry, VfsError> {
        let dir = nfc(dir);
        let name = nfc(name);
        path::validate_component(&name)
            .map_err(|e| VfsError::InvalidPath(format!("{name:?}: {e}")))?;
        let full = join(&dir, &name);
        let meta = self.stat(&full)?;
        let inner = self.inner.lock().expect("vfs lock");
        let fileid = inner
            .fileid_of
            .get(&full)
            .copied()
            .ok_or_else(|| VfsError::Local(format!("internal: {full:?} has no fileid")))?;
        Ok(ListedEntry { name, fileid, meta })
    }

    /// Lists a directory (loading it on first list): names + fileids +
    /// stat metadata, deterministic (name-byte order — the tree's).
    pub fn readdir(&self, dir: &str) -> Result<Vec<ListedEntry>, VfsError> {
        let dir = nfc(dir);
        let mut inner = self.inner.lock().expect("vfs lock");
        self.ensure_dir_locked(&mut inner, &dir)?;
        let node = inner
            .dirs
            .get(&dir)
            .ok_or_else(|| VfsError::NotADirectory(dir.clone()))?;
        // Snapshot the (small) listings so fileid assignment can mutate
        // `inner` while we walk.
        let children: Vec<(String, EntryMeta)> = node
            .children
            .iter()
            .map(|(name, (_, meta))| (name.clone(), *meta))
            .collect();
        let staged_snapshot: Vec<(String, u64)> = inner
            .staged
            .iter()
            .map(|(p, (_, size))| (p.clone(), *size))
            .collect();
        let prefix = if dir.is_empty() {
            String::new()
        } else {
            format!("{dir}/")
        };
        let mut out: Vec<ListedEntry> = Vec::with_capacity(children.len());
        for (name, pinned_meta) in children {
            let full = join(&dir, &name);
            // A staged write shadows the pinned entry's stat.
            let meta = match inner.staged.get(&full) {
                Some((_file, size)) => EntryMeta {
                    kind: EntryKind::File,
                    size: *size,
                    exec: false,
                },
                None => pinned_meta,
            };
            let fileid = inner.assign_fileid(&full);
            out.push(ListedEntry { name, fileid, meta });
        }
        // The writable overlay adds staged paths not in the pinned tree.
        let listed: HashSet<&str> = out.iter().map(|e| e.name.as_str()).collect();
        let mut extra: Vec<ListedEntry> = Vec::new();
        for (staged_path, size) in staged_snapshot {
            let Some(name) = staged_path.strip_prefix(&prefix) else {
                continue;
            };
            if name.contains('/') || listed.contains(name) {
                continue;
            }
            extra.push(ListedEntry {
                name: name.to_string(),
                fileid: inner.assign_fileid(&staged_path),
                meta: EntryMeta {
                    kind: EntryKind::File,
                    size,
                    exec: false,
                },
            });
        }
        extra.sort_by(|a, b| a.name.as_bytes().cmp(b.name.as_bytes()));
        out.extend(extra);
        Ok(out)
    }

    /// Reads a file: overlay first, then the hydrate cache (hash-verified
    /// on load — a poisoned file is evicted), then the source (hash-verified
    /// on hydration). A hydration failure names the object id and never
    /// leaves a cached trace.
    pub fn read(&self, path: &str) -> Result<Vec<u8>, VfsError> {
        let path = nfc(path);
        let meta = self.stat(&path)?;
        if meta.kind == EntryKind::Dir {
            return Err(VfsError::IsADirectory(path));
        }
        {
            let inner = self.inner.lock().expect("vfs lock");
            if let Some((file, _size)) = inner.staged.get(&path) {
                return std::fs::read(file)
                    .map_err(|e| VfsError::Local(format!("reading the shadow copy: {e}")));
            }
        }
        let id = {
            let (parent, name) = split_last(&path);
            let inner = self.inner.lock().expect("vfs lock");
            let node = inner
                .dirs
                .get(parent)
                .ok_or_else(|| VfsError::Local(format!("internal: {parent:?} not loaded")))?;
            node.children
                .get(name)
                .map(|(o, _)| *o)
                .ok_or_else(|| VfsError::NotFound(path.clone()))?
        };
        let mut cache = self.blobs.lock().expect("blob cache lock");
        for _ in 0..CACHE_RETRY {
            if let Some(bytes) = cache.get(&id) {
                match verify(&id, &bytes) {
                    Ok(()) => return Ok(bytes),
                    // Corrupt cache file: evict, re-hydrate (the
                    // corrupt-cache recovery contract).
                    Err(_) => {
                        cache.evict(&id);
                        continue;
                    }
                }
            }
            let bytes = self.source.blob(&id)?;
            verify(&id, &bytes)?;
            cache
                .insert(&id, &bytes)
                .map_err(|e| VfsError::Local(format!("writing the hydrate cache: {e}")))?;
            return Ok(bytes);
        }
        Err(VfsError::Local(format!(
            "cache for {} failed verification twice",
            id.to_hex()
        )))
    }

    // ---- writable mode (UNSTABLE; read-only by default) -------------------

    /// Creates (or truncates) a file. Enforces the path grammar + NFC at
    /// create (the plan's writable pin: non-NFC is REJECTED, naming why —
    /// never silently normalized) and the case-fold sibling rule, rejecting
    /// with `InvalidPath`. The parent directory must exist in the pinned
    /// tree (v1 has no mkdir).
    pub fn create(&self, dir: &str, name: &str) -> Result<EntryMeta, VfsError> {
        let dir = dir.to_string();
        let name = name.to_string();
        self.writable_gate()?;
        if nfc(&dir) != dir {
            return Err(VfsError::InvalidPath(format!(
                "directory {dir:?} is not in NFC normalization form"
            )));
        }
        if nfc(&name) != name {
            return Err(VfsError::InvalidPath(format!(
                "name {name:?} is not in NFC normalization form"
            )));
        }
        path::validate_component(&name).map_err(|e| VfsError::InvalidPath(format!("{e}")))?;
        let full = join(&dir, &name);
        path::validate(&full).map_err(|e| VfsError::InvalidPath(format!("{e}")))?;
        let mut inner = self.inner.lock().expect("vfs lock");
        self.ensure_dir_locked(&mut inner, &dir)?;
        let node = inner
            .dirs
            .get(&dir)
            .ok_or_else(|| VfsError::NotADirectory(dir.clone()))?;
        // Case-fold siblings (pinned children + staged names in this dir):
        // two names equal under case folding in one directory are rejected.
        let folded = name.to_lowercase();
        let prefix = if dir.is_empty() {
            String::new()
        } else {
            format!("{dir}/")
        };
        let staged_sibling = inner.staged.keys().find_map(|p| {
            let sibling = p.strip_prefix(&prefix)?.to_string();
            if sibling.contains('/') {
                return None;
            }
            if sibling != name && sibling.to_lowercase() == folded {
                Some(sibling)
            } else {
                None
            }
        });
        if let Some(sibling) = staged_sibling {
            return Err(VfsError::InvalidPath(format!(
                "{name:?} is equal under case folding to {sibling:?} in the same directory"
            )));
        }
        let pinned_sibling = node
            .children
            .keys()
            .find(|s| *s != &name && s.to_lowercase() == folded)
            .cloned();
        if let Some(sibling) = pinned_sibling {
            return Err(VfsError::InvalidPath(format!(
                "{name:?} is equal under case folding to {sibling:?} in the same directory"
            )));
        }
        drop(inner);
        // The overlay answers for `full` from here on (empty file, POSIX
        // creat truncation semantics).
        self.stage(&full, &[])?;
        Ok(EntryMeta {
            kind: EntryKind::File,
            size: 0,
            exec: false,
        })
    }

    /// Stages bytes for `path` (which must exist in the pinned tree or the
    /// overlay — POSIX write-without-create is ENOENT). Non-NFC paths are
    /// rejected (the writable create contract, same as [`NaatreVfs::create`]).
    pub fn write(&self, path: &str, bytes: &[u8]) -> Result<EntryMeta, VfsError> {
        let path = path.to_string();
        self.writable_gate()?;
        if nfc(&path) != path {
            return Err(VfsError::InvalidPath(format!(
                "path {path:?} is not in NFC normalization form"
            )));
        }
        path::validate(&path).map_err(|e| VfsError::InvalidPath(format!("{e}")))?;
        let staged = {
            let inner = self.inner.lock().expect("vfs lock");
            inner.staged.contains_key(&path)
        };
        if !staged {
            match self.stat(&path) {
                Ok(meta) if meta.kind == EntryKind::File => {}
                Ok(_) => return Err(VfsError::IsADirectory(path)),
                Err(VfsError::NotFound(_)) => return Err(VfsError::NotFound(path)),
                Err(e) => return Err(e),
            }
        }
        self.stage(&path, bytes)?;
        Ok(EntryMeta {
            kind: EntryKind::File,
            size: bytes.len() as u64,
            exec: false,
        })
    }

    /// Flush: staged shadow writes → blobs → one STAGED changeset on top
    /// of the pinned changeset. Nothing is landed (no ref moves); each
    /// flush stages a fresh changeset and the shadow stays. Empty shadow →
    /// `Ok(None)` (nothing to stage).
    pub fn flush(&self) -> Result<Option<FlushOutcome>, VfsError> {
        self.writable_gate()?;
        let staged: Vec<(String, PathBuf)> = {
            let inner = self.inner.lock().expect("vfs lock");
            inner
                .staged
                .iter()
                .map(|(p, (f, _))| (p.clone(), f.clone()))
                .collect()
        };
        if staged.is_empty() {
            return Ok(None);
        }
        let mut changes: Vec<ChangeLine> = Vec::with_capacity(staged.len());
        let mut uploaded: HashSet<Id> = HashSet::new();
        for (path, file) in &staged {
            let bytes = std::fs::read(file)
                .map_err(|e| VfsError::Local(format!("reading the shadow copy of {path}: {e}")))?;
            let blob = object_id(Kind::Blob, &bytes);
            let op = if self.pinned_contains(path)? {
                "mod"
            } else {
                "add"
            };
            if uploaded.insert(blob) {
                self.source.put_blob(&blob, &bytes)?;
            }
            changes.push(ChangeLine {
                path: path.clone(),
                op,
                blob,
                size: bytes.len() as u64,
                exec: false,
            });
        }
        let (id, root) = self.source.create_changeset(
            &[self.pin.changeset],
            "naatre mount",
            &format!(
                "mount flush: {} file(s) staged from a writable mount of {} at {}",
                changes.len(),
                self.repo,
                self.pin.changeset.to_hex()
            ),
            crate::wc::now_ns(),
            &changes,
        )?;
        Ok(Some(FlushOutcome {
            changeset: id,
            root_tree: root,
            files: changes.len(),
        }))
    }

    /// True when the PINNED tree contains `path` as a file (overlay
    /// ignored — flush's add-vs-mod decision).
    fn pinned_contains(&self, path: &str) -> Result<bool, VfsError> {
        let (parent, name) = split_last(path);
        let mut inner = self.inner.lock().expect("vfs lock");
        match self.ensure_dir_locked(&mut inner, parent) {
            Ok(()) => {}
            Err(VfsError::NotFound(_)) => return Ok(false),
            Err(e) => return Err(e),
        }
        Ok(inner
            .dirs
            .get(parent)
            .and_then(|n| n.children.get(name))
            .is_some())
    }

    fn stage(&self, path: &str, bytes: &[u8]) -> Result<(), VfsError> {
        let shadow_dir = self.mount_dir.join("shadow");
        std::fs::create_dir_all(&shadow_dir)
            .map_err(|e| VfsError::Local(format!("creating the shadow dir: {e}")))?;
        set_private_dir(&shadow_dir)
            .map_err(|e| VfsError::Local(format!("hardening the shadow dir: {e}")))?;
        let mut inner = self.inner.lock().expect("vfs lock");
        let file = match inner.staged.get(path) {
            Some((f, _)) => f.clone(),
            None => shadow_dir.join(format!("{:06}.shadow", inner.staged.len() + 1)),
        };
        crate::wc::write_atomic(&file, bytes, true)
            .map_err(|e| VfsError::Local(format!("writing the shadow copy: {e}")))?;
        inner
            .staged
            .insert(path.to_string(), (file, bytes.len() as u64));
        Ok(())
    }

    fn writable_gate(&self) -> Result<(), VfsError> {
        if self.writable {
            Ok(())
        } else {
            Err(VfsError::ReadOnly)
        }
    }

    // ---- lazy tree loading -------------------------------------------------

    /// Loads `dir`'s tree listing if not loaded (parents first; every level
    /// is one tree GET — no blob fetches, ever). Caller holds the lock.
    fn ensure_dir_locked(&self, inner: &mut Inner, dir: &str) -> Result<(), VfsError> {
        if inner.dirs.contains_key(dir) {
            return Ok(());
        }
        let (parent, name) = split_last(dir);
        if parent.is_empty() && name.is_empty() {
            // The root: load the pinned root tree.
            let entries = self.source.tree(&self.pin.root_tree)?;
            inner.dirs.insert(String::new(), self.dir_node(entries));
            return Ok(());
        }
        self.ensure_dir_locked(inner, parent)?;
        let (object, meta) = {
            let node = inner.dirs.get(parent).ok_or_else(|| {
                VfsError::Local(format!("internal: parent {parent:?} not loaded"))
            })?;
            node.children
                .get(name)
                .map(|(o, m)| (*o, *m))
                .ok_or_else(|| VfsError::NotFound(dir.to_string()))?
        };
        if meta.kind != EntryKind::Dir {
            return Err(VfsError::NotADirectory(dir.to_string()));
        }
        let entries = self.source.tree(&object)?;
        let _fileid = inner.assign_fileid(dir);
        inner.dirs.insert(dir.to_string(), self.dir_node(entries));
        Ok(())
    }

    fn dir_node(&self, entries: Vec<TreeEntry>) -> DirNode {
        DirNode {
            children: entries
                .into_iter()
                .map(|e| {
                    (
                        e.name,
                        (
                            e.object,
                            EntryMeta {
                                kind: e.kind,
                                size: e.size,
                                exec: e.exec,
                            },
                        ),
                    )
                })
                .collect(),
        }
    }
}

impl Inner {
    fn assign_fileid(&mut self, path: &str) -> u64 {
        if let Some(id) = self.fileid_of.get(path) {
            return *id;
        }
        let id = self.next_fileid;
        self.next_fileid += 1;
        self.fileid_of.insert(path.to_string(), id);
        self.path_of.insert(id, path.to_string());
        id
    }
}

/// Splits `a/b/c` into ("a/b", "c"); a bare name stays ("", name).
fn split_last(path: &str) -> (&str, &str) {
    match path.rsplit_once('/') {
        Some((dir, name)) => (dir, name),
        None => ("", path),
    }
}

/// Joins a dir and a name ("" , "x" → "x"; "a", "b" → "a/b").
fn join(dir: &str, name: &str) -> String {
    if dir.is_empty() {
        name.to_string()
    } else {
        format!("{dir}/{name}")
    }
}

// ---- test doubles (the M7 recording-client pattern) ------------------------

/// Shares one source between the VFS and whoever counts its calls (the
/// recording-client pattern needs the counter AFTER the source has moved
/// into [`NaatreVfs`]).
impl<S: MountSource + ?Sized> MountSource for std::sync::Arc<S> {
    fn ref_target(&self, name: &str) -> Result<Id, VfsError> {
        (**self).ref_target(name)
    }

    fn changeset_root(&self, cs: &Id) -> Result<Id, VfsError> {
        (**self).changeset_root(cs)
    }

    fn tree(&self, id: &Id) -> Result<Vec<TreeEntry>, VfsError> {
        (**self).tree(id)
    }

    fn blob(&self, id: &Id) -> Result<Vec<u8>, VfsError> {
        (**self).blob(id)
    }

    fn put_blob(&self, id: &Id, bytes: &[u8]) -> Result<(), VfsError> {
        (**self).put_blob(id, bytes)
    }

    fn create_changeset(
        &self,
        parents: &[Id],
        author: &str,
        message: &str,
        timestamp_ns: u64,
        changes: &[ChangeLine],
    ) -> Result<(Id, Id), VfsError> {
        (**self).create_changeset(parents, author, message, timestamp_ns, changes)
    }
}

/// In-memory [`MountSource`]: trees and blobs in maps, refs by name, and
/// every staged changeset recorded. Test harness only.
#[derive(Default)]
pub struct MemorySource {
    pub refs: HashMap<String, Id>,
    pub roots: HashMap<Id, Id>,
    pub trees: HashMap<Id, Vec<TreeEntry>>,
    pub blobs: HashMap<Id, Vec<u8>>,
    pub staged: Mutex<Vec<(Vec<Id>, Vec<ChangeLine>)>>,
    /// When set, `blob` serves these bytes instead of the map (the
    /// hash-mismatch fixture swaps content under a live id).
    pub blob_override: Mutex<HashMap<Id, Vec<u8>>>,
    /// Ids `blob` reports missing (the missing-object fixture).
    pub missing: Mutex<HashSet<Id>>,
}

impl MemorySource {
    /// Builds a source serving one tree built from (path, bytes, exec)
    /// rows, with refs/main → a root changeset. Returns (source, cs id).
    pub fn with_files(files: &[(&str, &[u8], bool)]) -> (MemorySource, Id) {
        // Build bottom-up like the snapshot verb does.
        #[derive(Default)]
        struct Node {
            files: Vec<TreeEntry>,
            dirs: BTreeMap<String, Node>,
        }
        fn encode(node: &Node, source: &mut MemorySource) -> (Id, u64) {
            let mut entries = node.files.clone();
            let mut subtree = node.files.iter().map(|f| f.size).sum::<u64>();
            for (name, child) in &node.dirs {
                let (id, size) = encode(child, source);
                subtree += size;
                entries.push(TreeEntry {
                    name: name.clone(),
                    kind: EntryKind::Dir,
                    object: id,
                    size,
                    exec: false,
                });
            }
            entries.sort_by(|a, b| a.name.as_bytes().cmp(b.name.as_bytes()));
            let tree = naatre_core::tree::Tree { entries };
            let id = naatre_core::tree::tree_id(&tree).expect("tree id");
            source.trees.insert(id, tree.entries);
            (id, subtree)
        }
        let mut root = Node::default();
        for (path, bytes, exec) in files {
            let mut node = &mut root;
            let mut comps = path.split('/');
            let last = comps.next_back().expect("non-empty path");
            for dir in comps {
                node = node.dirs.entry(dir.to_string()).or_default();
            }
            node.files.push(TreeEntry {
                name: last.to_string(),
                kind: EntryKind::File,
                object: object_id(Kind::Blob, bytes),
                size: bytes.len() as u64,
                exec: *exec,
            });
        }
        let mut source = MemorySource::default();
        let (root_tree, _) = encode(&root, &mut source);
        for (_, bytes, _) in files {
            source
                .blobs
                .insert(object_id(Kind::Blob, bytes), bytes.to_vec());
        }
        let cs = Id([7u8; 32]);
        source.refs.insert("main".into(), cs);
        source.roots.insert(cs, root_tree);
        (source, cs)
    }
}

impl MountSource for MemorySource {
    fn ref_target(&self, name: &str) -> Result<Id, VfsError> {
        self.refs
            .get(name)
            .copied()
            .ok_or_else(|| VfsError::Local(format!("ref {name:?} is unborn")))
    }

    fn changeset_root(&self, cs: &Id) -> Result<Id, VfsError> {
        self.roots
            .get(cs)
            .copied()
            .ok_or_else(|| VfsError::Local("no such changeset".into()))
    }

    fn tree(&self, id: &Id) -> Result<Vec<TreeEntry>, VfsError> {
        self.trees
            .get(id)
            .cloned()
            .ok_or_else(|| VfsError::Local("no such tree".into()))
    }

    fn blob(&self, id: &Id) -> Result<Vec<u8>, VfsError> {
        if self.missing.lock().expect("lock").contains(id) {
            return Err(VfsError::Hydration(HydrationError::MissingId(*id)));
        }
        if let Some(bytes) = self.blob_override.lock().expect("lock").get(id) {
            return Ok(bytes.clone());
        }
        self.blobs
            .get(id)
            .cloned()
            .ok_or(VfsError::Hydration(HydrationError::MissingId(*id)))
    }

    fn put_blob(&self, _id: &Id, _bytes: &[u8]) -> Result<(), VfsError> {
        Ok(())
    }

    fn create_changeset(
        &self,
        parents: &[Id],
        _author: &str,
        _message: &str,
        _timestamp_ns: u64,
        changes: &[ChangeLine],
    ) -> Result<(Id, Id), VfsError> {
        self.staged
            .lock()
            .expect("lock")
            .push((parents.to_vec(), changes.to_vec()));
        Ok((Id([9u8; 32]), Id([8u8; 32])))
    }
}

/// Counts calls through to an inner source — the stat-never-fetches pin
/// counts `blob` GETs on a stat-only walk.
pub struct RecordingSource<S: MountSource> {
    pub inner: S,
    blob_gets: AtomicU64,
    tree_gets: AtomicU64,
    ref_gets: AtomicU64,
    root_gets: AtomicU64,
}

impl<S: MountSource> RecordingSource<S> {
    pub fn new(inner: S) -> RecordingSource<S> {
        RecordingSource {
            inner,
            blob_gets: AtomicU64::new(0),
            tree_gets: AtomicU64::new(0),
            ref_gets: AtomicU64::new(0),
            root_gets: AtomicU64::new(0),
        }
    }

    pub fn blob_gets(&self) -> u64 {
        self.blob_gets.load(Ordering::Relaxed)
    }

    pub fn tree_gets(&self) -> u64 {
        self.tree_gets.load(Ordering::Relaxed)
    }

    pub fn ref_gets(&self) -> u64 {
        self.ref_gets.load(Ordering::Relaxed)
    }

    pub fn root_gets(&self) -> u64 {
        self.root_gets.load(Ordering::Relaxed)
    }
}

impl<S: MountSource> MountSource for RecordingSource<S> {
    fn ref_target(&self, name: &str) -> Result<Id, VfsError> {
        self.ref_gets.fetch_add(1, Ordering::Relaxed);
        self.inner.ref_target(name)
    }

    fn changeset_root(&self, cs: &Id) -> Result<Id, VfsError> {
        self.root_gets.fetch_add(1, Ordering::Relaxed);
        self.inner.changeset_root(cs)
    }

    fn tree(&self, id: &Id) -> Result<Vec<TreeEntry>, VfsError> {
        self.tree_gets.fetch_add(1, Ordering::Relaxed);
        self.inner.tree(id)
    }

    fn blob(&self, id: &Id) -> Result<Vec<u8>, VfsError> {
        self.blob_gets.fetch_add(1, Ordering::Relaxed);
        self.inner.blob(id)
    }

    fn put_blob(&self, id: &Id, bytes: &[u8]) -> Result<(), VfsError> {
        self.inner.put_blob(id, bytes)
    }

    fn create_changeset(
        &self,
        parents: &[Id],
        author: &str,
        message: &str,
        timestamp_ns: u64,
        changes: &[ChangeLine],
    ) -> Result<(Id, Id), VfsError> {
        self.inner
            .create_changeset(parents, author, message, timestamp_ns, changes)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A fresh 0700 mount dir under the OS temp dir; the caller cleans up.
    fn tmp_mount_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "naatre-vfs-ut-{tag}-{}-{}",
            std::process::id(),
            crate::wc::now_ns()
        ));
        std::fs::create_dir_all(&dir).expect("mkdir");
        set_private_dir(&dir).expect("chmod");
        dir
    }

    fn cleanup(dir: &Path) {
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn hydration_errors_are_typed_and_name_the_id() {
        let (source, _cs) = MemorySource::with_files(&[("f.txt", b"hello".as_slice(), false)]);
        let blob = object_id(Kind::Blob, b"hello");
        let dir = tmp_mount_dir("typed");
        let vfs = NaatreVfs::mount(source, "r", None, dir.clone(), false).expect("mount");
        assert_eq!(vfs.read("f.txt").expect("read"), b"hello");

        // Poison the source under the live id → hash mismatch (want/got).
        let (source2, _) = MemorySource::with_files(&[("f.txt", b"hello".as_slice(), false)]);
        source2
            .blob_override
            .lock()
            .expect("lock")
            .insert(blob, b"HELLO".to_vec());
        let dir2 = tmp_mount_dir("typed2");
        let vfs2 = NaatreVfs::mount(source2, "r", None, dir2.clone(), false).expect("mount 2");
        match vfs2.read("f.txt") {
            Err(VfsError::Hydration(HydrationError::HashMismatch { want, got })) => {
                assert_eq!(want, blob);
                assert_eq!(got, object_id(Kind::Blob, b"HELLO"));
            }
            other => panic!("expected HashMismatch, got {other:?}"),
        }
        // And the failed hydration left no cached trace to poison later reads.
        assert!(vfs2.stat("f.txt").is_ok());

        // A missing object names its id.
        let (source3, _) = MemorySource::with_files(&[("f.txt", b"hello".as_slice(), false)]);
        source3.missing.lock().expect("lock").insert(blob);
        let dir3 = tmp_mount_dir("typed3");
        let vfs3 = NaatreVfs::mount(source3, "r", None, dir3.clone(), false).expect("mount 3");
        match vfs3.read("f.txt") {
            Err(VfsError::Hydration(HydrationError::MissingId(id))) => assert_eq!(id, blob),
            other => panic!("expected MissingId, got {other:?}"),
        }
        cleanup(&dir);
        cleanup(&dir2);
        cleanup(&dir3);
    }

    #[test]
    fn blob_cache_is_lru_bounded_by_count() {
        let dir = tmp_mount_dir("lru");
        let mut cache = BlobCache::open(dir.join("blobs"), 3).expect("open");
        let ids: Vec<Id> = (0u8..5)
            .map(|i| {
                let mut b = [0u8; 32];
                b[0] = i;
                Id(b)
            })
            .collect();
        for (i, id) in ids.iter().enumerate() {
            cache.insert(id, format!("payload-{i}").as_bytes()).unwrap();
        }
        // 5 inserted into a 3-slot cache: the two oldest are gone from disk.
        assert_eq!(cache.len(), 3);
        assert!(cache.get(&ids[0]).is_none(), "oldest evicted");
        assert!(cache.get(&ids[1]).is_none(), "second oldest evicted");
        assert_eq!(cache.get(&ids[4]).expect("newest present"), b"payload-4");
        // Touch ids[2], insert one more: ids[3] (now oldest) is evicted.
        assert!(cache.get(&ids[2]).is_some());
        cache.insert(&ids[0], b"reinserted").unwrap();
        assert!(cache.get(&ids[3]).is_none());
        assert_eq!(
            cache.get(&ids[0]).expect("touched+reinserted"),
            b"reinserted"
        );
        cleanup(&dir);
    }

    #[test]
    fn blob_cache_rebuilds_its_index_from_disk_on_open() {
        let dir = tmp_mount_dir("reopen");
        let mut cache = BlobCache::open(dir.join("blobs"), 2).expect("open 1");
        let a = Id([1u8; 32]);
        let b = Id([2u8; 32]);
        let c = Id([3u8; 32]);
        cache.insert(&a, b"a").unwrap();
        cache.insert(&b, b"b").unwrap();
        cache.insert(&c, b"c").unwrap(); // evicts a
        assert!(cache.get(&a).is_none());
        // Reopen (what the next mount does): b and c are still on disk.
        let mut reopened = BlobCache::open(dir.join("blobs"), 2).expect("open 2");
        assert_eq!(reopened.len(), 2);
        assert!(reopened.get(&b).is_some() && reopened.get(&c).is_some());
        cleanup(&dir);
    }

    #[test]
    fn hydration_log_line_names_the_object_and_the_errno() {
        let id = object_id(Kind::Blob, b"x");
        let missing = HydrationError::MissingId(id);
        let line = hydration_log_line(&missing);
        assert!(line.contains(&id.to_hex()), "{line}");
        assert!(line.contains(&ERRNO_MISSING_ID.to_string()), "{line}");
        let mismatch = HydrationError::HashMismatch {
            want: id,
            got: Id([3u8; 32]),
        };
        assert!(hydration_log_line(&mismatch).contains(&id.to_hex()));
        // The two hydration classes carry DISTINCT errnos (the plan's pin).
        assert_ne!(missing.errno(), mismatch.errno());
    }

    #[test]
    fn errno_mapping_covers_every_class() {
        assert_eq!(VfsError::ReadOnly.errno(), ERRNO_READ_ONLY);
        assert_eq!(VfsError::NotFound("p".into()).errno(), ERRNO_NOT_FOUND);
        assert_eq!(VfsError::NotADirectory("p".into()).errno(), ERRNO_NOT_DIR);
        assert_eq!(VfsError::IsADirectory("p".into()).errno(), ERRNO_IS_DIR);
        assert_eq!(
            VfsError::InvalidPath("why".into()).errno(),
            ERRNO_INVALID_PATH
        );
    }

    #[test]
    fn split_and_join_are_root_aware() {
        assert_eq!(split_last("a/b/c"), ("a/b", "c"));
        assert_eq!(split_last("c"), ("", "c"));
        assert_eq!(join("", "x"), "x");
        assert_eq!(join("a/b", "c"), "a/b/c");
    }
}
