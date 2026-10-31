//! The working-copy single-writer lock: `.naatre/wc.lock` (plan M7).
//!
//! `naatre mcp` takes the exclusive flock at startup and holds it for the
//! session's lifetime; every CLI verb that opens an existing working copy
//! holds it for the verb's duration. A concurrent writer in the same tree —
//! the MCP session against a CLI verb, or vice versa — is detected and
//! refused with an error naming the lock, instead of racing on `state.json`
//! and the stat index. The lock is advisory and released on drop or process
//! death, so a SIGKILLed session cannot wedge the tree. The file lives under
//! `.naatre/` (the reserved metadata namespace: never scanned, never
//! committed).

use std::fs::{File, OpenOptions};
use std::io::ErrorKind;
use std::path::Path;

use fs2::FileExt;

pub const WC_LOCK_FILE: &str = "wc.lock";

/// A held working-copy lock; released when dropped.
pub struct WcLock {
    file: File,
}

impl std::fmt::Debug for WcLock {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("WcLock").finish_non_exhaustive()
    }
}

impl Drop for WcLock {
    fn drop(&mut self) {
        let _ = self.file.unlock();
    }
}

/// Either a held lock, or `NoNaatreDir` — the directory has no `.naatre/`
/// at all, so there is nothing to lock and the verb should run and produce
/// its own "not a naatre working copy" error (existing messages stay).
#[derive(Debug)]
pub enum Outcome {
    Locked(WcLock),
    NoNaatreDir,
}

/// Takes the exclusive flock on `<root>/.naatre/wc.lock` (non-blocking).
/// `Err` names the lock path when another process holds it.
pub fn acquire(root: &Path) -> Result<Outcome, String> {
    let dir = root.join(crate::wc::NAATRE_DIR);
    if !dir.is_dir() {
        return Ok(Outcome::NoNaatreDir);
    }
    let path = dir.join(WC_LOCK_FILE);
    let file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .write(true)
        .open(&path)
        .map_err(|e| format!("opening {}: {e}", path.display()))?;
    match file.try_lock_exclusive() {
        Ok(()) => Ok(Outcome::Locked(WcLock { file })),
        Err(e) if e.kind() == ErrorKind::WouldBlock => Err(format!(
            "working-copy lock is held by another naatre process: {} — \
             concurrent use of one working copy is not supported (a running \
             `naatre mcp` session holds it for its lifetime)",
            path.display()
        )),
        Err(e) => Err(format!("locking {}: {e}", path.display())),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lock_is_exclusive_and_names_the_path_on_refusal() {
        let dir = tempfile::tempdir().unwrap();
        let wc = dir.path().join("wc");
        std::fs::create_dir_all(wc.join(crate::wc::NAATRE_DIR)).unwrap();

        let first = acquire(&wc).unwrap();
        let Outcome::Locked(_guard) = first else {
            panic!("a .naatre dir must lock");
        };
        let err = acquire(&wc).unwrap_err();
        assert!(err.contains("wc.lock"), "refusal names the lock: {err}");
        assert!(
            err.contains(
                &wc.join(crate::wc::NAATRE_DIR)
                    .join(WC_LOCK_FILE)
                    .display()
                    .to_string()
            )
        );

        // A directory without .naatre/ is a pass-through, not an error.
        let bare = dir.path().join("bare");
        std::fs::create_dir(&bare).unwrap();
        assert!(matches!(acquire(&bare).unwrap(), Outcome::NoNaatreDir));

        drop(_guard);
        assert!(matches!(acquire(&wc).unwrap(), Outcome::Locked(_)));
    }
}
