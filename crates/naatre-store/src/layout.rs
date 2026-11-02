//! The data-dir metadata layout (M13a): one SQLite file for everything
//! (the zero-config default) or a catalog database plus one SQLite file
//! per repo behind it.
//!
//! `Single` is today's shape, unchanged: every table lives in `naatre.db`.
//! `DbPerRepo` splits the metadata across files — the CATALOG holds the
//! global tables (principals, tokens, audit_log, the repo registry with
//! each repo's db path, the global `objects/` CAS ledger, `gc_pending`,
//! `derive_kinds`, `meta`), and each per-repo file under `repos/` holds
//! that repo's rows (changesets/parents/file_changes/repo_blobs/trees/
//! tree_entries/repo_trees/refs/lands/snapshots/git_map, the M6 derived
//! tables, and its redactions). A fresh per-repo file runs the shared
//! SQLite `MIGRATIONS` list verbatim (1..=12) — the global tables exist
//! in it but stay empty; the catalog runs its own schema
//! ([`crate::catalog`]). The `objects/` CAS is untouched by the split:
//! bytes stay global, refcounts stay cross-repo.
//!
//! DB-per-repo is SQLite-only: per-repo files are SQLite, so a Postgres
//! metadata backend only ever runs `Single` (the config parser refuses
//! the combination). The data-dir flock keeps serializing same-dir
//! processes in both layouts.

use std::path::{Path, PathBuf};

use crate::error::StoreError;

/// The catalog database file name inside the data dir (`DbPerRepo` only).
pub const CATALOG_FILE: &str = "catalog.db";
/// The per-repo database directory inside the data dir (`DbPerRepo` only).
pub const REPOS_DIR: &str = "repos";

/// Which metadata layout a data dir runs.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Layout {
    /// One `naatre.db` holds every table (the zero-config default).
    #[default]
    Single,
    /// `catalog.db` for the global tables + one `repos/<id>.db` per repo.
    DbPerRepo,
}

impl Layout {
    pub fn as_str(self) -> &'static str {
        match self {
            Layout::Single => "single-db",
            Layout::DbPerRepo => "db-per-repo",
        }
    }

    /// Parses the `backend.toml` `[layout] mode` value.
    pub fn parse(s: &str) -> Option<Layout> {
        match s {
            "single-db" => Some(Layout::Single),
            "db-per-repo" => Some(Layout::DbPerRepo),
            _ => None,
        }
    }
}

/// The per-repo database file for `repo_id`: `<data>/repos/<id>.db` (the
/// id is never recycled, so the name never collides or gets rewritten).
pub fn repo_db_path(data: &Path, repo_id: i64) -> PathBuf {
    data.join(REPOS_DIR).join(format!("{repo_id}.db"))
}

/// Detects the on-disk layout from the marker files: `catalog.db` present
/// means the data dir was migrated to [`Layout::DbPerRepo`]; otherwise
/// `naatre.db` is [`Layout::Single`]. Neither marker present is the
/// caller's uninitialized/mismatch case (`check_data_dir_backend`).
pub fn detect(data: &Path) -> Layout {
    if data.join(CATALOG_FILE).exists() {
        Layout::DbPerRepo
    } else {
        Layout::Single
    }
}

/// The layout disagreement refusal (`check_data_dir_backend`): the config
/// selects one layout, the data dir's marker files say another.
pub(crate) fn layout_mismatch(on_disk: Layout, configured: Layout) -> StoreError {
    let remedy = match (on_disk, configured) {
        (Layout::DbPerRepo, Layout::Single) => {
            "this data dir was migrated to db-per-repo: set `[layout] mode = \
             \"db-per-repo\"` in backend.toml (or delete catalog.db to abandon \
             the migration and keep serving from naatre.db)"
        }
        (Layout::Single, Layout::DbPerRepo) => {
            "no catalog.db: run `naatre migrate-db-per-repo` first (the config \
             selects db-per-repo but the data dir has never been migrated)"
        }
        _ => "the data dir's files disagree with the configured layout",
    };
    StoreError::Backend(crate::backend::BackendError::LayoutMismatch {
        on_disk: on_disk.as_str(),
        configured: configured.as_str(),
        remedy,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detect_reads_marker_files() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(detect(dir.path()), Layout::Single);
        std::fs::write(dir.path().join(CATALOG_FILE), b"").unwrap();
        assert_eq!(detect(dir.path()), Layout::DbPerRepo);
    }

    #[test]
    fn repo_db_path_is_id_keyed() {
        let p = repo_db_path(Path::new("/data"), 42);
        assert_eq!(p, Path::new("/data/repos/42.db"));
    }

    #[test]
    fn layout_parse_and_names() {
        assert_eq!(Layout::parse("single-db"), Some(Layout::Single));
        assert_eq!(Layout::parse("db-per-repo"), Some(Layout::DbPerRepo));
        assert_eq!(Layout::parse("other"), None);
        assert_eq!(Layout::Single.as_str(), "single-db");
        assert_eq!(Layout::DbPerRepo.as_str(), "db-per-repo");
    }
}
