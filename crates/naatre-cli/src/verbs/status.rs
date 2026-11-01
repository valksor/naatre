//! `naatre status [--json]` — disk scan vs the stat index. Makes ZERO
//! server calls by construction: this module never builds a client. The
//! conflicted paths (M12b) come from `.naatre/state.json` — recorded by
//! the verbs that materialized the markers (sync/clone/land) — so the
//! zero-server-call invariant holds.

use std::path::Path;

use crate::wc::scan::{self, Report};

/// Computes the report (library entry point; tests assert on this). The
/// conflicted marks are the state.json paths still present in the index —
/// a path whose index row is gone (deleted by a sync, dropped by a sparse
/// remove) no longer materializes and is not marked.
pub fn report(root: &Path) -> Result<Report, String> {
    let wc = crate::wc::Wc::open(root)?;
    let index = wc.load_index()?;
    let mut rep =
        scan::classify(root, &index).map_err(|e| format!("scanning {}: {e}", root.display()))?;
    let state = wc.load_state()?;
    rep.conflicted = state
        .conflicted
        .into_iter()
        .filter(|p| index.rows.contains_key(p))
        .collect();
    Ok(rep)
}

/// Runs status; prints human or JSON output and returns the exit code.
pub fn run(root: &Path, json: bool) -> i32 {
    match report(root) {
        Ok(rep) => {
            if json {
                println!("{}", rep.to_json());
            } else {
                scan::print_report(&rep);
            }
            0
        }
        Err(e) => {
            eprintln!("error: {e}");
            1
        }
    }
}
