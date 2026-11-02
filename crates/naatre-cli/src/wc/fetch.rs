//! Fetching trees and blobs from the server for materialization.
//!
//! Server data is untrusted here: every file path is validated against the
//! core path grammar before anything is written, and every blob's bytes are
//! hash-verified client-side with `object_id(Blob)` — a mismatch aborts the
//! verb, nothing partial is kept (clone works in a staging directory;
//! sync/sparse write per-file through temp+rename).

use std::collections::{HashMap, HashSet, VecDeque};
use std::path::Path;
use std::time::Duration;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::path;
use serde_json::json;

use crate::client::{Client, HttpError, bytes_as_envelope};
use crate::wc::index::Row;

/// A fetch/verify failure: either HTTP or a local data-integrity error.
#[derive(Debug)]
pub enum FetchError {
    Http(HttpError),
    Local(String),
}

impl From<HttpError> for FetchError {
    fn from(e: HttpError) -> Self {
        FetchError::Http(e)
    }
}

impl std::fmt::Display for FetchError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            FetchError::Http(e) => write!(f, "{e}"),
            FetchError::Local(m) => f.write_str(m),
        }
    }
}

/// One file of the (sparse-filtered) server tree.
#[derive(Clone, Debug)]
pub struct TreeFile {
    pub path: String,
    pub blob: Id,
    pub size: u64,
    pub exec: bool,
}

/// A tree walk's result: the cone's available files, and the paths the
/// server reports redacted (masked file entries — unavailable by policy,
/// never an abort).
#[derive(Clone, Debug, Default)]
pub struct TreeWalk {
    pub files: Vec<TreeFile>,
    /// Paths whose masked entry (`object_id: null, size: null,
    /// redacted: true`) the walk met inside the cone: the blob is
    /// tombstoned in this repo, the name serves, the content never will.
    pub redacted: Vec<String>,
}

/// Walks the server tree rooted at `root`, fetching only the trees on the
/// paths to the sparse `prefixes` (an empty list walks everything — full
/// clone is allowed at this size). Blob ids arrive via each parent's entry
/// listing: a chain of custody from the changeset's declared root id.
///
/// Errors when a prefix does not exist in the tree (as a dir or file) —
/// the strict form clone and `sparse add` widen through. A masked file
/// entry is not an error: it is recorded in [`TreeWalk::redacted`].
pub fn walk_tree(
    client: &Client,
    repo: &str,
    root: &Id,
    prefixes: &[String],
) -> Result<TreeWalk, FetchError> {
    walk_tree_inner(
        client,
        &format!("/v1/repos/{repo}/trees"),
        root,
        prefixes,
        true,
    )
}

/// The uniform rule for walks over an EXISTING checkout's cone (sync's
/// target walk, local diff's head walk): a prefix the tree no longer
/// contains (the server deleted its whole directory — a deleted directory
/// is normal history) is an EMPTY subtree, so the verb completes on normal
/// history instead of failing forever. Widening operations (clone, sparse
/// add) keep the strict [`walk_tree`].
pub fn walk_tree_allowing_missing_prefixes(
    client: &Client,
    repo: &str,
    root: &Id,
    prefixes: &[String],
) -> Result<TreeWalk, FetchError> {
    walk_tree_inner(
        client,
        &format!("/v1/repos/{repo}/trees"),
        root,
        prefixes,
        false,
    )
}

/// The snapshot-tree walk: lists through the owner-gated snapshot routes
/// (`/v1/repos/{repo}/snapshots/{sid}/trees/{tid}`) — snapshot trees carry
/// no `repo_trees` membership, so the bare-id repo tree route 404s them.
/// A prefix the snapshot lacks is an empty subtree (restore's rule).
pub fn walk_snapshot_tree(
    client: &Client,
    repo: &str,
    snap_id: &Id,
    root: &Id,
    prefixes: &[String],
) -> Result<TreeWalk, FetchError> {
    walk_tree_inner(
        client,
        &format!("/v1/repos/{repo}/snapshots/{}/trees", snap_id.to_hex()),
        root,
        prefixes,
        false,
    )
}

fn walk_tree_inner(
    client: &Client,
    list_base: &str,
    root: &Id,
    prefixes: &[String],
    require_prefixes: bool,
) -> Result<TreeWalk, FetchError> {
    for p in prefixes {
        path::validate(p).map_err(|e| FetchError::Local(format!("sparse prefix {p:?}: {e}")))?;
    }
    let mut walk = TreeWalk::default();
    let mut visited: HashSet<Id> = HashSet::new();
    let mut descended: HashSet<String> = HashSet::new();
    let mut queue: VecDeque<(String, Id)> = VecDeque::from([(String::new(), *root)]);
    while let Some((dir, tree_id)) = queue.pop_front() {
        if !visited.insert(tree_id) {
            continue;
        }
        let v = client.get_json(&format!("{list_base}/{}", tree_id.to_hex()), &[])?;
        let entries = v["entries"]
            .as_array()
            .ok_or_else(|| FetchError::Local("server sent a malformed tree listing".into()))?;
        for e in entries {
            let Some(name) = e["name"].as_str() else {
                return Err(FetchError::Local("tree entry without a name".into()));
            };
            let kind = e["kind"].as_str().unwrap_or("");
            let oid = e["object_id"].as_str().and_then(Id::from_hex);
            let full = if dir.is_empty() {
                name.to_string()
            } else {
                format!("{dir}/{name}")
            };
            if kind == "file" {
                let Some(oid) = oid else {
                    // A masked file entry is valid server data (M10b):
                    // object_id null, size null, redacted true — the blob
                    // is tombstoned in this repo. The path is recorded as
                    // unavailable and the walk proceeds; a null object id
                    // WITHOUT the mask is still malformed server data.
                    if e["redacted"].as_bool() == Some(true) {
                        if under(prefixes, &full) {
                            walk.redacted.push(full);
                        }
                        continue;
                    }
                    return Err(FetchError::Local(format!(
                        "tree entry {name:?} has a malformed object id"
                    )));
                };
                if under(prefixes, &full) {
                    // Grammar enforcement of untrusted server data.
                    path::validate(&full)
                        .map_err(|e| FetchError::Local(format!("server path {full:?}: {e}")))?;
                    walk.files.push(TreeFile {
                        path: full,
                        blob: oid,
                        size: e["size"].as_u64().unwrap_or(0),
                        exec: e["exec"].as_bool().unwrap_or(false),
                    });
                }
            } else {
                // A masked dir entry cannot exist (a dir's object id is a
                // tree id, never tombstoned content) — keep the strict
                // abort.
                let Some(oid) = oid else {
                    return Err(FetchError::Local(format!(
                        "tree entry {name:?} has a malformed object id"
                    )));
                };
                // Descend when the dir is ON the path to a prefix (prefix
                // under dir) or UNDER a prefix (dir inside the prefix's
                // subtree — its files materialize).
                let descend = prefixes.is_empty()
                    || prefixes.iter().any(|p| {
                        p == &full
                            || p.starts_with(&format!("{full}/"))
                            || full.starts_with(&format!("{p}/"))
                    });
                if descend {
                    descended.insert(full.clone());
                    queue.push_back((full, oid));
                }
            }
        }
    }
    if require_prefixes {
        for p in prefixes {
            // A masked file path EXISTS in the tree (the name serves by
            // design) — it satisfies the prefix check even though its
            // content will never materialize.
            let found = descended.contains(p)
                || walk.files.iter().any(|f| f.path == *p)
                || walk.redacted.iter().any(|r| r == p);
            if !found {
                return Err(FetchError::Local(format!(
                    "sparse prefix {p:?} does not exist in the tree"
                )));
            }
        }
    }
    Ok(walk)
}

fn under(prefixes: &[String], path: &str) -> bool {
    crate::wc::under_sparse(prefixes, path)
}

/// Fetches blobs by id in batch, decoding and hash-verifying EVERY payload
/// client-side (`object_id(Blob)` must equal the requested id). Any missing
/// id or mismatch aborts with a [`FetchError::Local`].
///
/// M13b: with a [`CacheRoute`] active (`NAATRE_CACHE_URL` set) every id is
/// fetched BY-ID through the tier first (ticket-authenticated, tier-side
/// membership + tombstone gates apply); the origin's batch endpoint is the
/// FALLBACK — a cache transport failure (or 5xx) disables the tier for the
/// rest of the call and the remaining ids go direct (bounded: one cache
/// attempt per id, one disable per run). The origin's own negative answers
/// (404/410 passed through by the tier) are authoritative errors, exactly
/// as when fetching direct.
pub fn fetch_blobs_verified(
    client: &Client,
    repo: &str,
    root_tree: &Id,
    ids: &[Id],
    cache: &mut Option<CacheRoute>,
) -> Result<HashMap<Id, Vec<u8>>, FetchError> {
    let mut out = HashMap::new();
    let mut pending: Vec<Id> = Vec::new();
    let mut cache_enabled = cache.is_some();
    for id in ids {
        if cache_enabled {
            let route = cache.as_mut().expect("cache_enabled implies a route");
            match route.fetch_one(client, repo, root_tree, id) {
                Ok(Some(bytes)) => {
                    out.insert(*id, bytes);
                    continue;
                }
                Ok(None) => {
                    // The tier is unreachable or unhealthy: fall back to
                    // the origin for everything left (bounded — one
                    // disable per run).
                    cache_enabled = false;
                    eprintln!("warning: cache tier unavailable; fetching from the origin instead");
                }
                Err(e) => return Err(e),
            }
        }
        pending.push(*id);
    }
    for chunk in pending.chunks(256) {
        let body = json!({ "ids": chunk.iter().map(Id::to_hex).collect::<Vec<_>>() });
        // A POST-shaped read (batch object fetch) — the retry contract
        // applies.
        let (_status, v) =
            client.post_json_idempotent(&format!("/v1/repos/{repo}/objects/get"), &body)?;
        for obj in v["objects"].as_array().map_or(&[][..], Vec::as_slice) {
            let Some(hex) = obj["id"].as_str().and_then(Id::from_hex) else {
                return Err(FetchError::Local(
                    "batch fetch returned a malformed object id".into(),
                ));
            };
            let Some(data_b64) = obj["data_b64"].as_str() else {
                return Err(FetchError::Local(format!(
                    "batch fetch returned no payload for {}",
                    hex.to_hex()
                )));
            };
            let bytes = STANDARD.decode(data_b64.as_bytes()).map_err(|e| {
                FetchError::Local(format!("payload of {} is not base64: {e}", hex.to_hex()))
            })?;
            verify(&hex, &bytes)?;
            out.insert(hex, bytes);
        }
        let missing: Vec<String> = v["missing"]
            .as_array()
            .map_or(&[][..], Vec::as_slice)
            .iter()
            .filter_map(|m| m.as_str().map(str::to_string))
            .collect();
        if !missing.is_empty() {
            return Err(FetchError::Local(format!(
                "server cannot serve {} object(s): {}",
                missing.len(),
                missing.join(", ")
            )));
        }
    }
    Ok(out)
}

/// The client's route to the `naatre cache` tier (M13b): `NAATRE_CACHE_URL`
/// set means by-id object fetches go through the tier with a ticket minted
/// from the origin (`POST /v1/repos/{r}/tickets`, the plan's mint-on-
/// demand client story). Only by-id fetches route here — refs, diffs,
/// trees, and writes stay direct-to-origin (the plan: refs/log never
/// proxied). One route per verb run; the ticket lives in memory only
/// (the token never touches disk either) and re-mints when it nears
/// expiry or binds a different tree.
pub struct CacheRoute {
    /// The tier's base URL, no trailing slash.
    base: String,
    /// A minimal agent for the tier (the same no-TLS posture as the
    /// origin client: LAN/LAN-localhost).
    agent: ureq::Agent,
    /// The live ticket, if minted (None until the first by-id fetch).
    ticket: Option<TicketState>,
}

struct TicketState {
    ticket: String,
    expires_ns: u64,
    repo: String,
    root_tree: Id,
}

impl CacheRoute {
    /// The `NAATRE_CACHE_URL` route, or `None` when unset/empty.
    pub fn from_env() -> Option<CacheRoute> {
        let base = std::env::var("NAATRE_CACHE_URL").ok()?;
        let base = base.trim().trim_end_matches('/').to_string();
        if base.is_empty() {
            return None;
        }
        Some(CacheRoute {
            base,
            agent: ureq::Agent::new_with_config(
                ureq::config::Config::builder()
                    .http_status_as_error(false)
                    .timeout_connect(Some(Duration::from_secs(5)))
                    .timeout_global(Some(Duration::from_secs(120)))
                    .build(),
            ),
            ticket: None,
        })
    }

    /// One by-id fetch through the tier. `Ok(Some(bytes))` served;
    /// `Ok(None)` = the tier failed transport-side (caller falls back to
    /// the origin); `Err` = an AUTHORITATIVE answer (the tier's 404/410 —
    /// itself the origin's gated answer — or a client-side hash
    /// mismatch), which must surface, never fall back. One re-mint is
    /// attempted when the tier answers 401 (an expired ticket mid-run).
    fn fetch_one(
        &mut self,
        client: &Client,
        repo: &str,
        root_tree: &Id,
        id: &Id,
    ) -> Result<Option<Vec<u8>>, FetchError> {
        for attempt in 0..2 {
            // A mint failure is an origin-side problem: the cache path
            // is unusable — fall back to the origin batch (Ok(None)).
            let ticket = match self.ticket_for(client, repo, root_tree) {
                Ok(t) => t,
                Err(e) => {
                    eprintln!("warning: cache ticket unavailable ({e}); using the origin");
                    return Ok(None);
                }
            };
            let url = format!("{}/v1/objects/{}", self.base, id.to_hex());
            let res = self
                .agent
                .get(&url)
                .header("Authorization", format!("NaatreTicket {ticket}"))
                .call();
            match res {
                Ok(res) => {
                    let status = res.status().as_u16();
                    let mut body = res.into_body();
                    if (200..300).contains(&status) {
                        let bytes = body.read_to_vec().map_err(|e| {
                            FetchError::Local(format!("cache tier body read failed: {e}"))
                        })?;
                        // The pinned integrity gate — the tier's bytes
                        // are untrusted exactly like the origin's.
                        verify(id, &bytes)?;
                        return Ok(Some(bytes));
                    }
                    let bytes = body.read_to_vec().unwrap_or_default();
                    let err = bytes_as_envelope(&bytes, status, None);
                    if status == 401 && attempt == 0 {
                        // Expired (or rotated) mid-run: re-mint once.
                        self.ticket = None;
                        continue;
                    }
                    // 404 (not in ticket scope / origin-missing) and 410
                    // (origin-redacted) are authoritative negatives.
                    return Err(FetchError::Http(err));
                }
                Err(e) => {
                    // Transport: bounded fallback (the caller disables
                    // the tier for this run).
                    let _ = e;
                    return Ok(None);
                }
            }
        }
        unreachable!("both loop arms return")
    }

    /// A live ticket for (repo, tree): reuses the cached one while it has
    /// ≥30 s of headroom and matches the binding, else mints a fresh one
    /// from the origin. The 30 s margin keeps a ticket from dying between
    /// mint and use on a slow fetch.
    fn ticket_for(
        &mut self,
        client: &Client,
        repo: &str,
        root_tree: &Id,
    ) -> Result<String, String> {
        let now_ns = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos() as u64)
            .unwrap_or(0);
        if let Some(t) = &self.ticket
            && t.repo == repo
            && t.root_tree == *root_tree
            && now_ns + 30_000_000_000 < t.expires_ns
        {
            return Ok(t.ticket.clone());
        }
        let (_status, v) = client
            .post_json(
                &format!("/v1/repos/{}/tickets", crate::client::uri_segment(repo)),
                &json!({ "root_tree": root_tree.to_hex() }),
            )
            .map_err(|e| e.to_string())?;
        let ticket = v["ticket"]
            .as_str()
            .ok_or_else(|| "the origin's mint response has no ticket".to_string())?
            .to_string();
        let expires_ns = v["expires_ns"]
            .as_u64()
            .ok_or_else(|| "the origin's mint response has no expiry".to_string())?;
        self.ticket = Some(TicketState {
            ticket,
            expires_ns,
            repo: repo.to_string(),
            root_tree: *root_tree,
        });
        Ok(self.ticket.as_ref().expect("just stored").ticket.clone())
    }
}

/// The pinned client-side integrity gate: payload must hash to its id.
pub fn verify(id: &Id, bytes: &[u8]) -> Result<(), FetchError> {
    let actual = object_id(Kind::Blob, bytes);
    if actual != *id {
        return Err(FetchError::Local(format!(
            "hash mismatch: object {} does not hash to its id (got {})",
            id.to_hex(),
            actual.to_hex()
        )));
    }
    Ok(())
}

/// Materializes one fetched file into the checkout (grammar-checked again
/// here, so every caller is covered) and returns its stat row.
///
/// Defense in depth against symlink redirection: every on-disk path
/// component under the checkout root is checked with `symlink_metadata`
/// (a scan-independent stat, no dereference) and any symlink — file or
/// directory — refuses the write with an error, never a clobber and never
/// a write-through. This covers clone, sync, restore, and `sparse add`
/// regardless of what their higher-level gates caught. The walk is
/// accepted defense-in-depth, not airtight: a symlink planted between the
/// last stat and the write still wins that residual stat-then-write
/// TOCTOU race (an openat/O_NOFOLLOW walk would close it airtight).
pub fn materialize(root: &Path, file: &TreeFile, bytes: &[u8]) -> Result<Row, FetchError> {
    path::validate(&file.path)
        .map_err(|e| FetchError::Local(format!("server path {:?}: {}", file.path, e)))?;
    verify(&file.blob, bytes)?;
    let mut component = root.to_path_buf();
    for name in file.path.split('/') {
        component.push(name);
        let is_symlink = component
            .symlink_metadata()
            .map(|m| m.file_type().is_symlink())
            .unwrap_or(false); // absent: nothing to refuse. NOTE: a stat error fails OPEN here (reads as "not a symlink"); the safety net is the subsequent write, which fails loudly on the same error — an EACCES is never mistaken for a planted link
        if is_symlink {
            return Err(FetchError::Local(format!(
                "refusing to write {:?}: path component {:?} is a symlink",
                file.path,
                component.strip_prefix(root).unwrap_or(&component).display()
            )));
        }
    }
    crate::wc::write_checkout_file(root, &file.path, bytes, file.blob.0, file.exec)
        .map_err(|e| FetchError::Local(format!("writing {}: {e}", file.path)))
}
