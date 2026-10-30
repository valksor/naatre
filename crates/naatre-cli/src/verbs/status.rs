//! `naatre status [--json]` — disk scan vs the stat index. Makes ZERO
//! server calls by construction: this module never builds a client.

use std::path::Path;

use crate::wc::scan::{self, Report};

/// Computes the report (library entry point; tests assert on this).
pub fn report(root: &Path) -> Result<Report, String> {
    let wc = crate::wc::Wc::open(root)?;
    let index = wc.load_index()?;
    scan::classify(root, &index).map_err(|e| format!("scanning {}: {e}", root.display()))
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
