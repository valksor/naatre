//! The naatre data dir: creation, open guards, and subdirectory paths.

use std::fs::{File, OpenOptions};
use std::io::ErrorKind;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};

use fs2::FileExt;

use crate::error::StoreError;
use crate::fs_util::{FILE_PERMS, ensure_dir, io_ctx};

/// The SQLite database file name (present only when `meta = sqlite`).
pub const DB_FILE: &str = "naatre.db";
/// The history CAS directory name under the data dir (populated only when
/// `blob = fs`; its EXISTENCE is created by opening the store, its
/// CONTENT is the fs-CAS marker the backend mismatch guard reads).
pub const OBJECTS_DIR: &str = "objects";
const LOCK_FILE: &str = "lockfile";

/// Handle to an initialized naatre data directory (0700 — the host-local
/// trust boundary for admin verbs).
#[derive(Clone, Debug)]
pub struct DataDir(PathBuf);

/// Holds the data dir's exclusive flock on `<data>/lockfile`.
///
/// Both `naatre serve` and `naatre import` take this lock, so two servers,
/// or a server and an import, cannot run against the same data dir at once.
/// The lock is released when the guard is dropped.
#[derive(Debug)]
pub struct LockGuard {
    file: File,
}

impl Drop for LockGuard {
    fn drop(&mut self) {
        let _ = self.file.unlock();
    }
}

impl DataDir {
    /// Creates a new data dir at `path` with 0700 permissions.
    ///
    /// Fails with [`StoreError::DataDirAlreadyInitialized`] when `naatre.db`
    /// already exists there — re-initialization must never re-mint state.
    pub fn create(path: impl Into<PathBuf>) -> Result<DataDir, StoreError> {
        let path = path.into();
        ensure_dir(&path)?;
        let db = path.join(DB_FILE);
        if db.exists() {
            return Err(StoreError::DataDirAlreadyInitialized(path));
        }
        Ok(DataDir(path))
    }

    /// Opens an existing data dir. Fails with
    /// [`StoreError::DataDirUninitialized`] when `naatre.db` is missing
    /// (`run naatre init`).
    pub fn open(path: impl Into<PathBuf>) -> Result<DataDir, StoreError> {
        let path = path.into();
        let db = path.join(DB_FILE);
        if !db.exists() {
            return Err(StoreError::DataDirUninitialized(path));
        }
        Ok(DataDir(path))
    }

    /// Opens a data dir WITHOUT requiring `naatre.db` — the backend-aware
    /// entry point (with `meta = postgres` there is no db file; the
    /// initialized check is the caller's backend-specific guard, and the
    /// config/data-dir mismatch check runs via
    /// [`crate::backend::check_data_dir_backend`]).
    pub fn open_any(path: impl Into<PathBuf>) -> DataDir {
        DataDir(path.into())
    }

    /// Path of the SQLite database file.
    pub fn db_path(&self) -> PathBuf {
        self.0.join(DB_FILE)
    }

    /// The history CAS root, created 0700 on first use.
    pub fn objects_dir(&self) -> Result<PathBuf, StoreError> {
        let p = self.0.join(OBJECTS_DIR);
        ensure_dir(&p)?;
        Ok(p)
    }

    /// The ephemeral snapshot namespace root, created 0700 on first use.
    pub fn ephemeral_dir(&self) -> Result<PathBuf, StoreError> {
        let p = self.0.join("ephemeral");
        ensure_dir(&p)?;
        Ok(p)
    }

    /// Staging area for tmp+rename writes, created 0700 on first use.
    pub fn tmp_dir(&self) -> Result<PathBuf, StoreError> {
        let p = self.0.join("tmp");
        ensure_dir(&p)?;
        Ok(p)
    }

    /// Takes the exclusive flock on `<data>/lockfile`.
    ///
    /// Returns [`StoreError::LockHeld`] naming the lockfile path when
    /// another process (a running server, or an in-flight import) holds it.
    pub fn lock(&self) -> Result<LockGuard, StoreError> {
        let path = self.0.join(LOCK_FILE);
        let file = OpenOptions::new()
            .create(true)
            .write(true)
            .mode(FILE_PERMS)
            .open(&path)
            .map_err(|e| io_ctx(e, &path))?;
        match file.try_lock_exclusive() {
            Ok(()) => Ok(LockGuard { file }),
            Err(e) if e.kind() == ErrorKind::WouldBlock => Err(StoreError::LockHeld(path)),
            Err(e) => Err(io_ctx(e, &path)),
        }
    }

    pub fn path(&self) -> &Path {
        &self.0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn create_open_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("data");
        let dd = DataDir::create(&p).unwrap();
        assert!(p.is_dir());
        // Subdirs are ensured on first use with 0700.
        let objects = dd.objects_dir().unwrap();
        assert!(objects.is_dir());
        assert_eq!(dd.db_path(), p.join(DB_FILE));

        // In the real flow, `naatre init` writes naatre.db; open requires it.
        std::fs::write(dd.db_path(), b"").unwrap();
        let dd2 = DataDir::open(&p).unwrap();
        assert_eq!(dd2.db_path(), dd.db_path());
    }

    #[test]
    fn second_create_errors() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("data");
        DataDir::create(&p).unwrap();
        // Write a naatre.db so the second create sees initialization.
        std::fs::write(p.join(DB_FILE), b"").unwrap();
        let err = DataDir::create(&p).unwrap_err();
        assert!(matches!(err, StoreError::DataDirAlreadyInitialized(ref got) if *got == p));
        assert!(err.to_string().contains(&p.display().to_string()));
    }

    #[test]
    fn open_requires_initialized_dir() {
        let dir = tempfile::tempdir().unwrap();
        let err = DataDir::open(dir.path()).unwrap_err();
        assert!(matches!(err, StoreError::DataDirUninitialized(_)));
        assert!(err.to_string().contains("run naatre init"));
    }

    #[test]
    fn flock_exclusive_across_handles() {
        let dir = tempfile::tempdir().unwrap();
        let dd1 = DataDir::create(dir.path().join("d")).unwrap();
        let dd2 = dd1.clone(); // second handle on the same data dir
        // Hold one lock; a second handle's attempt must fail with LockHeld.
        let _g1 = dd1.lock().unwrap();
        let err = dd2.lock().unwrap_err();
        assert!(matches!(err, StoreError::LockHeld(ref p) if p.file_name().unwrap() == LOCK_FILE));

        // After release (drop), the lock is acquirable again.
        drop(_g1);
        let _g2 = dd2.lock().unwrap();
    }
}
