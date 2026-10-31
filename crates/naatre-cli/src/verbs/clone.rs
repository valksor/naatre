//! `naatre clone <url-or-name> [--sparse PREFIX]… [--into DIR]`.
//!
//! Resolves `main`, fetches the head changeset's root tree, walks only the
//! trees on the paths to the sparse prefixes (default: the whole tree),
//! fetches and hash-verifies every blob, and materializes into a staging
//! directory renamed atomically onto the destination. `.naatre/` (config,
//! state, index) is written inside staging so an interrupted clone leaves
//! no half-written working copy.

use std::path::Path;

use naatre_core::ids::Id;
use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::client::{Client, DEFAULT_URL, uri_segment};
use crate::wc::fetch::{fetch_blobs_verified, materialize, walk_tree};
use crate::wc::index::Index;
use crate::wc::{Config, StageDir, State};

/// Splits `http://host[:port]/<repo>` into (origin, repo), or treats a bare
/// name as the repo on `NAATRE_URL` / `--server` / the default URL. Any
/// other scheme (https included) is a usage error — the client never
/// downgrades a URL to plain http behind the user's back.
fn split_url(url: &str, net: &Net) -> Result<(String, String), VerbError> {
    if let Some(rest) = url.strip_suffix('/')
        && !rest.is_empty()
    {
        return split_url(rest, net);
    }
    if let Some((scheme, after)) = url.split_once("://") {
        if !scheme.eq_ignore_ascii_case("http") {
            return Err(VerbError::Usage(format!(
                "URL scheme {scheme:?} is not supported: the naatre client speaks plain HTTP \
                 only (deploy behind a TLS-terminating proxy that forwards over http to loopback)"
            )));
        }
        let (authority, repo) = match after.split_once('/') {
            Some((a, r)) => (a, r),
            None => (after, ""),
        };
        if repo.is_empty() {
            return Err(VerbError::Usage(format!(
                "URL {url:?} has no repo path (expected http://host[:port]/<repo>)"
            )));
        }
        return Ok((format!("http://{authority}"), repo.to_string()));
    }
    if url.contains('/') {
        return Err(VerbError::Usage(format!(
            "{url:?} is neither http://host[:port]/<repo> nor a bare repo name"
        )));
    }
    let base = net
        .server
        .clone()
        .or_else(|| std::env::var("NAATRE_URL").ok().filter(|v| !v.is_empty()))
        .unwrap_or_else(|| DEFAULT_URL.to_string());
    Ok((base.trim_end_matches('/').to_string(), url.to_string()))
}

/// Runs clone; prints the summary and returns the exit code.
pub fn run(url: &str, sparse: &[String], into: Option<&str>, net: &Net) -> i32 {
    match value(url, sparse, into, net) {
        Ok(v) => {
            let sparse_list: Vec<&str> = v["sparse"]
                .as_array()
                .map(|a| a.iter().filter_map(|s| s.as_str()).collect())
                .unwrap_or_default();
            let sparse_note = if sparse_list.is_empty() {
                String::new()
            } else {
                format!(" (sparse: {})", sparse_list.join(", "))
            };
            println!(
                "cloned {} at {} into {} ({} file(s)){sparse_note}",
                v["repo"].as_str().unwrap_or("?"),
                v["head"].as_str().unwrap_or("?"),
                v["dest"].as_str().unwrap_or("?"),
                v["files"].as_u64().unwrap_or(0),
            );
            0
        }
        Err(e) => e.report(),
    }
}

/// The clone result in `--json`-style shape: `{repo, head, dest, files,
/// sparse}` (no HTTP analog; the MCP `clone` tool returns the same value).
pub fn value(
    url: &str,
    sparse: &[String],
    into: Option<&str>,
    net: &Net,
) -> Result<Value, VerbError> {
    let (base, repo) = split_url(url, net)?;
    let dest = Path::new(into.unwrap_or(&repo));
    // Token precedence: --token flag > NAATRE_TOKEN env (no config yet —
    // clone writes it).
    let token = net
        .token
        .clone()
        .or_else(|| std::env::var("NAATRE_TOKEN").ok().filter(|v| !v.is_empty()));
    let client = Client::new(&base, token.clone());

    // main → head changeset → root tree id.
    let refv = client.get_json(&format!("/v1/repos/{}/refs/main", uri_segment(&repo)), &[])?;
    let head = refv["target"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| {
            VerbError::Local("repo has no main ref (unborn); nothing to clone".into())
        })?;
    let rootv = client.get_json(
        &format!(
            "/v1/repos/{}/changesets/{}/root",
            uri_segment(&repo),
            head.to_hex()
        ),
        &[],
    )?;
    let root_tree = rootv["root_tree_id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed root tree id".into()))?;

    // Walk (sparse-filtered), then fetch and verify every blob.
    let files = walk_tree(&client, &repo, &root_tree, sparse)?;
    let blob_ids: Vec<Id> = files.iter().map(|f| f.blob).collect();
    let blobs = fetch_blobs_verified(&client, &repo, &blob_ids)?;

    // Materialize into staging; atomic rename onto the destination.
    let stage = StageDir::beside(dest)
        .map_err(|e| VerbError::Local(format!("cannot stage beside {}: {e}", dest.display())))?;
    let mut index = Index::default();
    for file in &files {
        let bytes = &blobs[&file.blob];
        let row = materialize(stage.path(), file, bytes)?;
        index.rows.insert(file.path.clone(), row);
    }
    let mut listed: Vec<String> = Vec::new();
    for p in sparse {
        if !listed.contains(p) {
            listed.push(p.clone());
        }
    }
    let state = State {
        head: head.to_hex(),
        base: None,
        sparse: listed,
        server_repo: repo.clone(),
    };
    {
        let wc = crate::wc::Wc::open(stage.path()).map_err(VerbError::Local)?;
        wc.save_config(&Config {
            server_url: client.base.clone(),
            token,
        })?;
        wc.save_state(&state)?;
        wc.save_index(&index)?;
    }
    stage
        .commit(dest)
        .map_err(|e| VerbError::Local(format!("cannot move the clone into place: {e}")))?;

    Ok(json!({
        "repo": repo,
        "head": head.to_hex(),
        "dest": dest.display().to_string(),
        "files": files.len(),
        "sparse": sparse,
    }))
}
