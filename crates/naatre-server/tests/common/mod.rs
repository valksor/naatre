//! Shared helpers for the integration tests.

use std::path::Path;
use std::time::Duration;

use naatre_server::state::{AppState, ServerState};
use naatre_store::data_dir::DataDir;

/// Builds a temp-dir-backed [`AppState`] over a freshly initialized data
/// dir, with `drain` as the graceful-shutdown drain timeout.
pub fn test_state(dir: &Path, drain: Duration) -> AppState {
    let data = DataDir::create(dir).expect("create data dir");
    // `naatre init` writes naatre.db; tests do the same, then open.
    std::fs::write(data.db_path(), b"").expect("touch naatre.db");
    let data = DataDir::open(dir).expect("open data dir");
    let mut state = ServerState::open(data).expect("open store");
    state.drain_timeout = drain;
    std::sync::Arc::new(state)
}
