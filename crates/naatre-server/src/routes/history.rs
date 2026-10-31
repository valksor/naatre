//! `/v1/repos/{r}/log`, `/diff`, `/merge-base`, `/file`: the history reads.
//! All any-authenticated, repo-scoped; unknown repo → 404 `repo_not_found`,
//! unknown ids → 404 `not_found`.

use naatre_store::Db;
use std::collections::BTreeMap;
use std::sync::Arc;

use axum::Extension;
use axum::Json;
use axum::extract::{Path, Query, State};
use axum::http::header::CONTENT_TYPE;
use axum::response::{IntoResponse, Response};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::AuthedPrincipal;
use crate::db::{self, TreeEntryRow};
use crate::derived::BlameLayer;
use crate::routes::repo_id_or_404;
use crate::state::AppState;

fn parse_id(hex: &str) -> Result<Id, ApiError> {
    Id::from_hex(hex)
        .ok_or_else(|| ApiError::new(ApiCode::Malformed, "id must be 64 lowercase hex characters"))
}

fn bad_cursor() -> ApiError {
    ApiError::new(ApiCode::Malformed, "cursor is not a valid page cursor")
}

// ---- log ------------------------------------------------------------------

/// Query params of `/log`.
#[derive(Deserialize, IntoParams)]
pub struct LogQuery {
    /// Start changeset id; default is the `main` ref target, or an empty
    /// page when `main` does not exist (the unborn-repo edge). Ignored with
    /// `path`.
    pub from: Option<String>,
    /// Page size (1–200; default 50).
    pub limit: Option<String>,
    /// Opaque `after:<last id>` cursor from the previous page.
    pub cursor: Option<String>,
    /// Path-history mode: list every changeset that touched this path (all
    /// branches, newest first) from the derived path_history index — the
    /// plan's path-history surface. Omitted: the first-parent chain.
    pub path: Option<String>,
}

/// One `/log` entry.
#[derive(Serialize, ToSchema)]
pub struct LogEntry {
    pub id: String,
    pub author: String,
    pub message: String,
    pub timestamp_ns: u64,
    pub generation: u64,
    pub principal: String,
}

/// A `/log` page: the first-parent chain, newest first. The chain itself is
/// the deterministic order.
#[derive(Serialize, ToSchema)]
pub struct LogResponse {
    pub entries: Vec<LogEntry>,
    /// `after:<id>` of this page's last entry, or null when done.
    pub next_cursor: Option<String>,
    pub has_more: bool,
}

/// Walks the first-parent chain (any authenticated principal). `from`
/// defaults to the `main` ref target; without a `main` ref the page is
/// empty (nothing is reachable). `cursor` resumes after the given id and
/// wins over `from` when both are present.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/log",
    params(
        ("r" = String, Path, description = "Repo name"),
        LogQuery
    ),
    responses(
        (status = 200, description = "One page of the first-parent chain", body = LogResponse),
        (status = 400, description = "Non-integer limit, or a malformed id/cursor"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo; `from`/`cursor` id not in this repo"),
        (status = 422, description = "limit out of the 1..=200 range")
    )
)]
pub async fn log(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    Query(q): Query<LogQuery>,
) -> Result<Json<serde_json::Value>, ApiError> {
    if let Some(path) = &q.path {
        // The path-history surface: the log-page shape plus the derived-
        // read contract fields, carried in a dedicated response type so
        // the plain first-parent log's shape stays byte-stable.
        let page = path_log(&state, &repo, path, q.limit.as_deref(), q.cursor.as_deref()).await?;
        return Ok(Json(
            serde_json::to_value(page).map_err(ApiError::log_internal)?,
        ));
    }
    let limit = super::parse_limit(&crate::routes::ListQuery {
        limit: q.limit.clone(),
        cursor: None,
    })?;

    // Resolve the walk start: explicit from, else the cursor's chain
    // position, else the main ref target (possibly absent → empty page).
    enum Start {
        Id(Id),
        /// Resume *after* this id (it was the previous page's last entry).
        After(Id),
        /// No `from`, no cursor: fall back to the main ref.
        Default,
    }
    let start = if let Some(cursor) = q.cursor.as_deref() {
        let hex = cursor.strip_prefix("after:").ok_or_else(bad_cursor)?;
        Start::After(parse_id_hex(hex, bad_cursor())?)
    } else if let Some(from) = q.from.as_deref() {
        Start::Id(parse_id(from)?)
    } else {
        Start::Default
    };

    let repo_id = repo_id_or_404(&state, &repo).await?;
    let rows = state
        .read(move |conn| {
            let from = match start {
                Start::Id(id) => {
                    // An unknown `from` is the same 404 as an unknown
                    // changeset fetch, not a silently empty page.
                    if db::changeset_graph(conn, repo_id, &id)
                        .map_err(ApiError::log_internal)?
                        .is_none()
                    {
                        return Err(ApiError::new(
                            ApiCode::NotFound,
                            "from id is not in this repo",
                        ));
                    }
                    Some(id)
                }
                Start::After(cursor) => {
                    // Resume at the cursor's first parent.
                    db::changeset_graph(conn, repo_id, &cursor)
                        .map_err(ApiError::log_internal)?
                        .ok_or_else(|| {
                            ApiError::new(ApiCode::NotFound, "cursor id is not in this repo")
                        })?
                        .parents
                        .first()
                        .copied()
                }
                Start::Default => db::ref_get(conn, repo_id, "main")
                    .map_err(ApiError::log_internal)?
                    .and_then(|r| r.target)
                    .map(|b| Id(b.try_into().expect("ref target is 32 bytes"))),
            };
            let Some(from) = from else {
                return Ok(Vec::new());
            };
            db::log_chain(conn, repo_id, &from, limit + 1).map_err(ApiError::log_internal)
        })
        .await?;

    let has_more = rows.len() as i64 > limit;
    let mut rows = rows;
    rows.truncate(limit as usize);
    let next_cursor = has_more.then(|| {
        let last = rows.last().expect("non-empty page").id.clone();
        format!(
            "after:{}",
            Id(last.try_into().expect("log id is 32 bytes")).to_hex()
        )
    });
    Ok(Json(
        serde_json::to_value(LogResponse {
            entries: rows
                .into_iter()
                .map(|r| LogEntry {
                    id: Id(r.id.try_into().expect("log id is 32 bytes")).to_hex(),
                    author: r.author,
                    message: r.message,
                    timestamp_ns: r.timestamp_ns as u64,
                    generation: r.generation as u64,
                    principal: r.principal,
                })
                .collect(),
            next_cursor,
            has_more,
        })
        .map_err(ApiError::log_internal)?,
    ))
}

/// Parses a cursor id where non-hex means "garbage cursor" (400), not the
/// generic malformed-id message.
fn parse_id_hex(hex: &str, err: ApiError) -> Result<Id, ApiError> {
    Id::from_hex(hex).ok_or(err)
}

// ---- log?path= : the derived path-history surface ---------------------------

/// A `?path=` page: the log-page shape plus the derived-read contract
/// fields. `degraded` with a non-empty `poisoned_cs` says part of the
/// index was quarantined — the page may be missing touches of those
/// changesets (never presented as complete history).
#[derive(Serialize, ToSchema)]
pub struct PathLogResponse {
    pub entries: Vec<LogEntry>,
    /// `after:<id>` of this page's last entry, or null when done.
    pub next_cursor: Option<String>,
    pub has_more: bool,
    /// True when a quarantined node's primary file changes touch this
    /// path (listed in `poisoned_cs`) — exactly the touches the derived
    /// index may be missing. A path no quarantined node touched is never
    /// flagged.
    pub degraded: bool,
    /// The quarantined changesets whose touches may be missing from this
    /// page (64-hex ids).
    pub poisoned_cs: Vec<String>,
}

/// The 503 of the read-during-backlog contract (plan M6): a derived read
/// answered while a region is still queued or deriving could silently miss
/// matches — the empty-as-if-no-history lie. Retryable, `Retry-After: 1`.
fn derivation_pending(what: &str) -> ApiError {
    ApiError::new(
        ApiCode::DerivationPending,
        format!("{what} is still being derived; retry shortly"),
    )
    .retryable(true)
}

/// `GET /log?path=<p>`: every changeset touching the path, newest first
/// (generation DESC, changeset id ASC). When the path_history kind is
/// registered the page comes off the derived index, gated by the
/// read-during-backlog contract: any queued/deriving node in the repo →
/// 503 `derivation_pending` (on-demand compute within the request timeout
/// is permitted by the plan's contract but not implemented — the queue
/// drains in the background and the 503 + `Retry-After` shapes the client
/// retry). Quarantined nodes degrade the answer (200 + flag), never
/// silently truncate it. With the kind unregistered, the same rows are
/// scanned out of `file_changes` — the fallback, and the parity oracle's
/// brute force.
async fn path_log(
    state: &AppState,
    repo: &str,
    path: &str,
    limit_raw: Option<&str>,
    cursor_raw: Option<&str>,
) -> Result<PathLogResponse, ApiError> {
    let limit = super::parse_limit(&crate::routes::ListQuery {
        limit: limit_raw.map(str::to_string),
        cursor: None,
    })?;
    let repo_id = repo_id_or_404(state, repo).await?;
    // The cursor resumes after a (generation, cs) row: resolve the cursor
    // id's generation. Unknown cursor → the same 404 as the plain log.
    let cursor = match cursor_raw {
        None => None,
        Some(raw) => {
            let hex = raw.strip_prefix("after:").ok_or_else(bad_cursor)?;
            let id = parse_id_hex(hex, bad_cursor())?;
            let cursor_gen = state
                .read(move |conn| {
                    Ok::<_, ApiError>(
                        db::changeset_graph(conn, repo_id, &id)
                            .map_err(ApiError::log_internal)?
                            .map(|g| g.generation),
                    )
                })
                .await?;
            Some((
                id,
                cursor_gen.ok_or_else(|| {
                    ApiError::new(ApiCode::NotFound, "cursor id is not in this repo")
                })?,
            ))
        }
    };
    let path = path.to_string();
    let page = state
        .read(move |conn| {
            let after = cursor.map(|(id, g)| (g, id.0.to_vec()));
            match db::derive_kind_id(conn, crate::derived::KIND_PATH_HISTORY)
                .map_err(ApiError::log_internal)?
            {
                Some(kind_id) => {
                    if db::derive_any_pending(conn, kind_id, repo_id)
                        .map_err(ApiError::log_internal)?
                    {
                        return Err(derivation_pending("path history"));
                    }
                    // Degraded exactly when a quarantined node's PRIMARY
                    // file_changes touch this path — those are the touches
                    // the index may be missing, and no others (an untouched
                    // branch is never flagged).
                    let poisoned: Vec<String> =
                        db::derive_poison_touching_path(conn, kind_id, repo_id, &path)
                            .map_err(ApiError::log_internal)?
                            .iter()
                            .map(Id::to_hex)
                            .collect();
                    let rows = db::path_history_page(conn, repo_id, &path, after, limit + 1)
                        .map_err(ApiError::log_internal)?;
                    Ok::<_, ApiError>((rows, !poisoned.is_empty(), poisoned))
                }
                None => {
                    let rows = db::path_history_scan(conn, repo_id, &path, after, limit + 1)
                        .map_err(ApiError::log_internal)?;
                    Ok((rows, false, Vec::new()))
                }
            }
        })
        .await?;
    let (rows, degraded, poisoned_cs) = page;
    let has_more = rows.len() as i64 > limit;
    let mut rows = rows;
    rows.truncate(limit as usize);
    let next_cursor = has_more.then(|| {
        let last = rows.last().expect("non-empty page").id.clone();
        format!(
            "after:{}",
            Id(last.try_into().expect("log id is 32 bytes")).to_hex()
        )
    });
    Ok(PathLogResponse {
        entries: to_log_entries(rows),
        next_cursor,
        has_more,
        degraded,
        poisoned_cs,
    })
}

fn to_log_entries(rows: Vec<db::LogRow>) -> Vec<LogEntry> {
    rows.into_iter()
        .map(|r| LogEntry {
            id: Id(r.id.try_into().expect("log id is 32 bytes")).to_hex(),
            author: r.author,
            message: r.message,
            timestamp_ns: r.timestamp_ns as u64,
            generation: r.generation as u64,
            principal: r.principal,
        })
        .collect()
}

// ---- diff -----------------------------------------------------------------

/// Query params of `/diff`.
#[derive(Deserialize, IntoParams)]
pub struct DiffQuery {
    /// Base changeset id.
    pub a: String,
    /// Target changeset id.
    pub b: String,
}

/// One path-level diff entry.
#[derive(Serialize, ToSchema)]
pub struct DiffEntry {
    pub path: String,
    /// `add` | `mod` | `del`.
    pub kind: String,
    /// Hex blob id on the `a` side; null for adds.
    pub a_blob: Option<String>,
    /// Hex blob id on the `b` side; null for deletes.
    pub b_blob: Option<String>,
}

/// A path-level diff between two changesets' root trees, sorted by path.
#[derive(Serialize, ToSchema)]
pub struct DiffResponse {
    pub changes: Vec<DiffEntry>,
}

/// Diffs two changesets' root trees (any authenticated principal): a
/// blob/exec difference is a `mod`; the output is sorted by path.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/diff",
    params(
        ("r" = String, Path, description = "Repo name"),
        DiffQuery
    ),
    responses(
        (status = 200, description = "Path-level changes between the two root trees", body = DiffResponse),
        (status = 400, description = "An id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo, or a or b is not a changeset of this repo")
    )
)]
pub async fn diff(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    Query(q): Query<DiffQuery>,
) -> Result<Json<DiffResponse>, ApiError> {
    let a = parse_id(&q.a)?;
    let b = parse_id(&q.b)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let entries = state
        .read(move |conn| {
            let root = |cs: &Id| -> Result<Id, ApiError> {
                let row = db::changeset_get(conn, repo_id, cs)
                    .map_err(ApiError::log_internal)?
                    .ok_or_else(|| {
                        ApiError::new(ApiCode::NotFound, "changeset is not in this repo")
                    })?;
                row.root_tree_id
                    .map(|bytes| Id(bytes.try_into().expect("root tree id is 32 bytes")))
                    .ok_or_else(|| ApiError::new(ApiCode::NotFound, "changeset has no root tree"))
            };
            let a_root = root(&a)?;
            let b_root = root(&b)?;
            let a_files = list_files(conn, &a_root)?;
            let b_files = list_files(conn, &b_root)?;
            // BTreeMap iteration gives the sorted-by-path contract.
            let mut changes = Vec::new();
            for (path, a_meta) in &a_files {
                match b_files.get(path) {
                    Some(b_meta) => {
                        if a_meta != b_meta {
                            changes.push(DiffEntry {
                                path: path.clone(),
                                kind: "mod".into(),
                                a_blob: Some(a_meta.0.to_hex()),
                                b_blob: Some(b_meta.0.to_hex()),
                            });
                        }
                    }
                    None => changes.push(DiffEntry {
                        path: path.clone(),
                        kind: "del".into(),
                        a_blob: Some(a_meta.0.to_hex()),
                        b_blob: None,
                    }),
                }
            }
            for (path, b_meta) in &b_files {
                if !a_files.contains_key(path) {
                    changes.push(DiffEntry {
                        path: path.clone(),
                        kind: "add".into(),
                        a_blob: None,
                        b_blob: Some(b_meta.0.to_hex()),
                    });
                }
            }
            Ok(changes)
        })
        .await?;
    Ok(Json(DiffResponse { changes: entries }))
}

/// Recursively flattens a tree into `full path → (blob, exec)`. Dir sizes
/// and tree ids are not part of a path-level diff.
fn list_files(conn: &Db, root: &Id) -> Result<BTreeMap<String, (Id, bool)>, ApiError> {
    let mut out = BTreeMap::new();
    walk_files(conn, root, String::new(), &mut out)?;
    Ok(out)
}

fn walk_files(
    conn: &Db,
    tree: &Id,
    prefix: String,
    out: &mut BTreeMap<String, (Id, bool)>,
) -> Result<(), ApiError> {
    for row in db::tree_entries_load(conn, tree).map_err(ApiError::log_internal)? {
        let object = Id(row
            .object_id
            .clone()
            .try_into()
            .expect("object id is 32 bytes"));
        let path = if prefix.is_empty() {
            row.name.clone()
        } else {
            format!("{prefix}/{}", row.name)
        };
        if row.kind == 1 {
            walk_files(conn, &object, path, out)?;
        } else {
            out.insert(path, (object, row.exec));
        }
    }
    Ok(())
}

// ---- merge-base -----------------------------------------------------------

/// Response of `/merge-base`.
#[derive(Serialize, ToSchema)]
pub struct MergeBaseResponse {
    /// Hex id of the best common ancestor, or null when the two changesets
    /// share no history.
    pub merge_base: Option<String>,
}

/// Finds the best common ancestor of two changesets (any authenticated
/// principal). Unknown ids → 404; unrelated histories → `merge_base: null`.
///
/// Acceleration contract: when both endpoints' commit_graph rows are done
/// and untainted, the walk runs on the derived skip lists
/// (`merge_base_skip`); anything else — kind unregistered, a pending,
/// quarantined, or tainted endpoint, undecodable output — falls back to
/// the plain generation-bounded walk. Merge-base never answers 503
/// `derivation_pending`: the fallback walk is exact and cheap, so pending
/// derivation costs only the acceleration, not the read (a quarantined or
/// tainted node degrades it the same way).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/merge-base",
    params(
        ("r" = String, Path, description = "Repo name"),
        DiffQuery
    ),
    responses(
        (status = 200, description = "The best common ancestor, or null", body = MergeBaseResponse),
        (status = 400, description = "An id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo, or a or b is not a changeset of this repo")
    )
)]
pub async fn merge_base(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    Query(q): Query<DiffQuery>,
) -> Result<Json<MergeBaseResponse>, ApiError> {
    let a = parse_id(&q.a)?;
    let b = parse_id(&q.b)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let base = state
        .read(move |conn| {
            // Both endpoints must exist before the walk (404 contract).
            for id in [&a, &b] {
                if db::changeset_graph(conn, repo_id, id)
                    .map_err(ApiError::log_internal)?
                    .is_none()
                {
                    return Err(ApiError::new(
                        ApiCode::NotFound,
                        "changeset is not in this repo",
                    ));
                }
            }
            // The derived fetch: generation + parents (primary rows,
            // always sound) plus skip entries only off a DONE and UNTAINTED
            // row. A tainted row derived over a poisoned ancestor's empty
            // marker — the plan's taint contract says such data is not
            // exact, so the exact acceleration must not consume it; the
            // plain-walk fallback covers that span.
            let graph_fetch = |id: &Id| -> Result<Option<SkipNode>, ApiError> {
                let Some(g) =
                    db::changeset_graph(conn, repo_id, id).map_err(ApiError::log_internal)?
                else {
                    return Ok(None);
                };
                let skips = db::derive_kind_id(conn, crate::derived::KIND_COMMIT_GRAPH)
                    .map_err(ApiError::log_internal)?
                    .and_then(|kind_id| {
                        db::derive_get(conn, kind_id, repo_id, id)
                            .map_err(ApiError::log_internal)
                            .ok()
                            .flatten()
                    })
                    .filter(|row| row.state == "done" && !row.tainted)
                    .and_then(|row| row.output)
                    .and_then(|out| crate::derived::decode_skip_entries(&out))
                    .unwrap_or_default()
                    .into_iter()
                    .map(|e| SkipEdge {
                        target: e.target,
                        hop: e.hop,
                        generation: e.generation,
                        pure: e.pure,
                    })
                    .collect();
                Ok(Some(SkipNode {
                    generation: g.generation,
                    parents: g.parents,
                    skips,
                }))
            };
            Ok(merge_base_skip(&graph_fetch, &a, &b))
        })
        .await?;
    Ok(Json(MergeBaseResponse {
        merge_base: base.map(|id| id.to_hex()),
    }))
}

/// A node's graph position for the skip-list merge-base: generation,
/// parents in `ord` order, and its commit-graph skip entries.
#[derive(Debug, Clone)]
pub(crate) struct SkipNode {
    pub generation: i64,
    pub parents: Vec<Id>,
    pub skips: Vec<SkipEdge>,
}

/// One skip entry (route-side mirror of [`crate::derived::SkipEntry`]).
#[derive(Debug, Clone)]
pub(crate) struct SkipEdge {
    pub target: Id,
    pub hop: u64,
    pub generation: i64,
    pub pure: bool,
}

/// The skip-list merge-base: an alignment climb over the derived skip
/// lists, then the plain seeded walk for the intersection.
///
/// Phase 1 climbs the higher-generation side's FIRST-parent chain while it
/// sits strictly above the lower side's generation. Every climbed node has
/// generation > gen(lower) ≥ gen(z) for any lower-side ancestor z, so no
/// climbed node can be a common ancestor and none needs marking; but a
/// climbed node's NON-first parents can still be common, so each visited
/// node enqueues them into the phase-2 seeds. A jump may only skip PURE
/// spans (`pure`, from the deriver: every spanned node single-parent) — a
/// skipped node is never processed, so its non-first parents would be
/// lost. Jump targets keep generation ≥ gen(lower), so skipped nodes
/// (strictly between target and source in generation) all sit above the
/// lower side and cannot be common. The climb ends at the first chain node
/// at or below gen(lower), which joins the phase-2 seeds.
///
/// Phase 2 is [`merge_base_walk_seeded`]: the full ancestor set of the
/// lower side against a walk from (the alignment endpoint + the collected
/// side-branch frontier). The alignment guarantees every common ancestor
/// reachable through the skipped region enters via those seeds, so the
/// result equals the plain walk — the oracle test (`m6b_test.rs`) checks
/// exactly that against brute-force ancestor sets.
///
/// Any mid-climb fetch miss just stops the climb early: the seeds cover a
/// smaller pre-climbed region and the seeded walk stays exact. The climb is
/// BOUNDED against corrupt-but-untainted skip rows (a cycle among clean
/// rows is undetectable by design — see the tainted-skip pin in
/// `m6b_test.rs`): every step must strictly descend in generation
/// (ancestors always do), a jump that refuses falls back to the plain
/// first-parent step, and a hard iteration cap ends the climb outright —
/// the seeded walk below is exact from any stopping point and is itself
/// cycle-safe (seen-set).
pub(crate) fn merge_base_skip<F>(fetch: &F, a: &Id, b: &Id) -> Option<Id>
where
    F: Fn(&Id) -> Result<Option<SkipNode>, ApiError>,
{
    /// The phase-1 climb's iteration cap (defense in depth; every accepted
    /// step already strictly descends, so honest climbs end at
    /// gen(lo) long before it).
    const CLIMB_MAX_STEPS: usize = 100_000;

    let na = fetch(a).ok().flatten()?;
    let nb = fetch(b).ok().flatten()?;
    let (lo_id, lo, mut hi_id, mut hi) = if na.generation >= nb.generation {
        (*b, nb, *a, na)
    } else {
        (*a, na, *b, nb)
    };

    // Phase 1: climb hi's first-parent chain above lo's generation.
    let mut frontier: Vec<Id> = Vec::new();
    let mut steps = 0usize;
    while hi.generation > lo.generation {
        steps += 1;
        if steps > CLIMB_MAX_STEPS {
            break;
        }
        frontier.extend(hi.parents.iter().skip(1).copied());
        let jump = hi
            .skips
            .iter()
            .filter(|e| e.pure && e.generation >= lo.generation)
            .max_by_key(|e| e.hop);
        let mut jumped = true;
        let mut next = match jump {
            Some(e) => e.target,
            None => {
                jumped = false;
                match hi.parents.first() {
                    Some(p) => *p,
                    None => break,
                }
            }
        };
        // One accepted step: `next` fetched AND strictly descending. A
        // refused JUMP falls back to the plain first-parent step (the
        // corrupt row steers no climb); a refused first-parent step —
        // corrupt too — ends the climb.
        let stepped = loop {
            match fetch(&next) {
                Ok(Some(node)) if node.generation < hi.generation => {
                    break Some((next, node));
                }
                Ok(Some(_)) if jumped => match hi.parents.first() {
                    Some(p) if *p != next => {
                        jumped = false;
                        next = *p;
                        continue;
                    }
                    _ => break None,
                },
                _ => break None,
            }
        };
        match stepped {
            Some((id, node)) => {
                hi_id = id;
                hi = node;
            }
            None => break,
        }
    }

    // Phase 2: the plain walk, lower side's full set vs (alignment
    // endpoint + frontier).
    let base_fetch = |id: &Id| -> Result<Option<(i64, Vec<Id>)>, ApiError> {
        Ok(fetch(id)?.map(|n| (n.generation, n.parents)))
    };
    let mut hi_seeds = frontier;
    hi_seeds.push(hi_id);
    merge_base_walk_seeded(&base_fetch, std::slice::from_ref(&lo_id), &hi_seeds)
}

/// The generation-bounded merge-base walk over explicit seeds:
/// `set_seeds` build the ancestor set (one side), `walk_seeds` are BFS'd
/// (the other side), and the highest-generation node of the walk found in
/// the set wins (the best common ancestor). Deterministic: the queue order
/// is seeds-then-parents-in-`ord`-order, and the first hit with a strictly
/// greater generation wins ties.
///
/// Generations bound the walk: a parent's generation is always at least
/// one below its child's, so both passes strictly descend and terminate.
pub(crate) fn merge_base_walk_seeded<F>(
    fetch: &F,
    set_seeds: &[Id],
    walk_seeds: &[Id],
) -> Option<Id>
where
    F: Fn(&Id) -> Result<Option<(i64, Vec<Id>)>, ApiError>,
{
    let mut ancestor_set: std::collections::HashSet<Id> = std::collections::HashSet::new();
    let mut queue = std::collections::VecDeque::from(set_seeds.to_vec());
    while let Some(id) = queue.pop_front() {
        if !ancestor_set.insert(id) {
            continue;
        }
        let Ok(Some((_, parents))) = fetch(&id) else {
            continue;
        };
        queue.extend(parents);
    }

    let mut best: Option<(i64, Id)> = None;
    let mut seen: std::collections::HashSet<Id> = std::collections::HashSet::new();
    let mut queue = std::collections::VecDeque::from(walk_seeds.to_vec());
    while let Some(id) = queue.pop_front() {
        if !seen.insert(id) {
            continue;
        }
        let Ok(Some((generation, parents))) = fetch(&id) else {
            continue;
        };
        if ancestor_set.contains(&id) && best.is_none_or(|(best_gen, _)| generation > best_gen) {
            best = Some((generation, id));
        }
        queue.extend(parents);
    }
    best.map(|(_, id)| id)
}

/// The plain two-endpoint walk (the fallback and the unit-test shape),
/// parameterized over the graph lookup so the unit tests below can drive
/// it on in-memory diamonds.
pub(crate) fn merge_base_walk<F>(fetch: &F, a: &Id, b: &Id) -> Option<Id>
where
    F: Fn(&Id) -> Result<Option<(i64, Vec<Id>)>, ApiError>,
{
    merge_base_walk_seeded(fetch, std::slice::from_ref(a), std::slice::from_ref(b))
}

// ---- blame -----------------------------------------------------------------

/// The blame read's walked-chain cap: past this many chain nodes the read
/// errors with 503 `blame_too_large` instead of walking unbounded history
/// on the read pool.
const BLAME_MAX_CHAIN_NODES: usize = 10_000;

/// The blame read's streamed-blob-bytes cap, enforced across the assembly's
/// layer loads (see the streaming task in [`blame`]); the default of
/// [`ServerState::blame_max_blob_bytes`].
pub(crate) const BLAME_MAX_BLOB_BYTES: usize = 256 * 1024 * 1024;

/// The blame read's serialized-response cap. The JSON body is encoded in
/// memory before it goes on the wire, so its encoded size is capped too:
/// a line-cap-compliant shape can still encode past a sane response via
/// long lines or long author strings. Checked on the encoded bytes — a
/// body past this never reaches the response writer.
const BLAME_MAX_RESPONSE_BYTES: usize = 32 * 1024 * 1024;

/// The structural-cap error: the same read trips it every time, so it is
/// not retryable — 503 `blame_too_large`, no `Retry-After`.
fn blame_too_large() -> ApiError {
    ApiError::new(
        ApiCode::BlameTooLarge,
        "blame exceeds the read caps (chain length / blob bytes / line count / response bytes)",
    )
}

/// Query params of `/blame`.
#[derive(Deserialize, IntoParams)]
pub struct BlameQuery {
    /// Changeset id to blame at.
    pub at: String,
    /// Repo-relative path (must be a file at `at`).
    pub path: String,
}

/// Encodes the blame response and enforces the serialized-size cap; the
/// encoded bytes go out as-is — never re-encoded after the check.
fn blame_json(response: BlameResponse) -> Result<Response, ApiError> {
    let body = serde_json::to_vec(&response).map_err(ApiError::log_internal)?;
    if body.len() > BLAME_MAX_RESPONSE_BYTES {
        return Err(blame_too_large());
    }
    Ok(([(CONTENT_TYPE, "application/json")], body).into_response())
}

/// One attributed line.
#[derive(Serialize, ToSchema)]
pub struct BlameLine {
    /// 64-hex id of the changeset whose edit introduced the line
    /// (first-parent semantics: compared against the first parent).
    pub cs: String,
    pub author: String,
    /// Line text without its trailing newline.
    pub text: String,
}

/// A blame of one file at one changeset. Pinned shape:
/// `{at, path, binary, binary_code, degraded, poisoned_cs, lines}` —
/// `binary: true` carries the stable code `blame_binary` in
/// `binary_code` and empty lines (HTTP 200: a binary file is a valid,
/// structured empty blame, not an error); an empty file blames as empty
/// `lines` with `binary: false`.
#[derive(Serialize, ToSchema)]
pub struct BlameResponse {
    pub at: String,
    pub path: String,
    /// True when the file at `at` is not valid UTF-8: blame does not run.
    pub binary: bool,
    /// `blame_binary` when `binary`; else null.
    pub binary_code: Option<String>,
    /// Part of the walked chain could not be resolved exactly: a quarantined
    /// node, or the walk reaching a row-less ancestor of root (the
    /// root-boundary leg — degraded without any quarantine). Lines below
    /// the boundary are attributed to the boundary changeset itself (named
    /// in `poisoned_cs`) — attribution bounded below by the boundary,
    /// never silently presented as exact.
    pub degraded: bool,
    /// The changesets the read degraded at: quarantined nodes on the walked
    /// chain, or the boundary node of the root leg (64-hex ids).
    pub poisoned_cs: Vec<String>,
    pub lines: Vec<BlameLine>,
}

/// One walked changeset that touched the path (the blame diff-state row).
struct BlameWalkNode {
    cs: Id,
    author: String,
    blob_new: Id,
    blob_parent: Option<Id>,
}

/// The degraded boundary: the quarantined (or row-less ancestor-of-root)
/// node whose blob carries every line the walk could not resolve past.
struct BlameBoundary {
    cs: Id,
    author: String,
    blob: Id,
}

/// One walked layer's routing metadata: (changeset, author, blob id) —
/// the bytes stream separately, in the blocking assembly task.
type BlameLayerMeta = (Id, String, Id);

/// The walk's harvest, all resolved inside one read-pool connection.
struct BlameWalk {
    final_blob: Id,
    /// Touched nodes, NEWEST first.
    nodes: Vec<BlameWalkNode>,
    boundary: Option<BlameBoundary>,
    /// The nearest poisoned ancestors named by the walked rows' own taint
    /// (O(1) off each row, no walk past them needed).
    tainted_poison_cs: Vec<Id>,
    /// True when the walk stopped at a poison boundary OR any walked row
    /// carried taint.
    degraded: bool,
}

/// Blames one file at one changeset (any authenticated principal) by
/// walking the first-parent chain over the derived blame diff-state:
/// per node one `derive_state` lookup and one `blame_state` point query;
/// at each node that touched the path, the line diff between its blob and
/// the parent's blob attributes the introduced lines. First touched node
/// with no parent blob = the add; the walk stops there.
///
/// Read-during-backlog: any chain node still queued or deriving → 503
/// `derivation_pending` + `Retry-After` (never an empty-as-if-no-history
/// answer). Quarantine degrades, never lies: the walk stops at the
/// quarantined node and every unresolvable line is attributed to it (its
/// blob provably contains those lines), with `degraded: true` and the node
/// in `poisoned_cs`.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/blame",
    params(
        ("r" = String, Path, description = "Repo name"),
        BlameQuery
    ),
    responses(
        (status = 200, description = "The blame (possibly empty or degraded; binary is a structured empty result)", body = BlameResponse),
        (status = 400, description = "`at` is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo/changeset, or the path is absent at `at`"),
        (status = 503, description = "The blame index is still being derived (`derivation_pending`, `Retry-After: 1`), or the read exceeded a read cap (chain length / blob bytes / line count / response bytes — `blame_too_large`, not retryable)")
    )
)]
pub async fn blame(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    Query(q): Query<BlameQuery>,
) -> Result<Response, ApiError> {
    let at = parse_id(&q.at)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let path = q.path.clone();
    let walk = state
        .read(move |conn| {
            let row = db::changeset_get(conn, repo_id, &at)
                .map_err(ApiError::log_internal)?
                .ok_or_else(|| ApiError::new(ApiCode::NotFound, "changeset is not in this repo"))?;
            let Some(root_bytes) = row.root_tree_id else {
                return Err(ApiError::new(
                    ApiCode::NotFound,
                    "changeset has no root tree",
                ));
            };
            let final_blob = db::tree_lookup_path(
                conn,
                &Id(root_bytes.try_into().expect("root tree id is 32 bytes")),
                &path,
            )
            .map_err(ApiError::log_internal)?
            .ok_or_else(|| ApiError::new(ApiCode::NotFound, format!("no such file {path:?}")))?;

            let Some(kind_id) = db::derive_kind_id(conn, crate::derived::KIND_BLAME)
                .map_err(ApiError::log_internal)?
            else {
                // Not registered on this server: no derivation is coming —
                // the honest answer is still "pending", never an empty
                // blame-as-if-no-history.
                return Err(derivation_pending("blame index (kind not registered)"));
            };

            let mut nodes: Vec<BlameWalkNode> = Vec::new();
            let mut boundary: Option<BlameBoundary> = None;
            // Taint read straight off the queue rows: a node that derived
            // on a poisoned parent's empty marker is done-with-taint, and
            // `poison_cs` names the nearest poisoned ancestor — the
            // degraded flag needs no walk past it.
            let mut tainted_poison_cs: Vec<Id> = Vec::new();
            let mut current = at;
            // The chain cap: the walk is one point query per node, but the
            // chain itself is unbounded — past the cap the read errors with
            // 503 `blame_too_large` (retryable false — the same chain trips
            // it every time) instead of monopolizing the read pool.
            let mut chain_nodes = 0usize;
            loop {
                chain_nodes += 1;
                if chain_nodes > BLAME_MAX_CHAIN_NODES {
                    return Err(blame_too_large());
                }
                let Some(row) = db::derive_get(conn, kind_id, repo_id, &current)
                    .map_err(ApiError::log_internal)?
                else {
                    // No queue row at all: the kind predates this
                    // changeset's enqueue (backfill has not reached it) —
                    // the same contract as pending.
                    return Err(derivation_pending("blame index"));
                };
                match row.state.as_str() {
                    "pending" | "deriving" => return Err(derivation_pending("blame index")),
                    "poison" => {
                        let cs_row = db::changeset_get(conn, repo_id, &current)
                            .map_err(ApiError::log_internal)?
                            .ok_or_else(|| ApiError::log_internal("poisoned node vanished"))?;
                        let blob = match nodes.last() {
                            // The quarantined node's own version of the
                            // path, as recorded by its nearest derived
                            // child.
                            Some(n) => n
                                .blob_parent
                                .expect("walk only advances past a parent-blob node"),
                            None => final_blob,
                        };
                        boundary = Some(BlameBoundary {
                            cs: current,
                            author: cs_row.author,
                            blob,
                        });
                        break;
                    }
                    // The CHECK constraint admits exactly these four states.
                    _ => {
                        if row.tainted
                            && let Some(poison_cs) = row.poison_cs
                            && !tainted_poison_cs.contains(&poison_cs)
                        {
                            tainted_poison_cs.push(poison_cs);
                        }
                        if let Some((blob_new, blob_parent)) =
                            db::blame_state_get(conn, repo_id, &current, &path)
                                .map_err(ApiError::log_internal)?
                                .map(|row| (row.blob_id, row.parent_blob_id))
                        {
                            let blob_new = blob_new.ok_or_else(|| {
                                ApiError::log_internal(
                                    "blame row without a blob for an existing path",
                                )
                            })?;
                            let cs_row = db::changeset_get(conn, repo_id, &current)
                                .map_err(ApiError::log_internal)?
                                .ok_or_else(|| ApiError::log_internal("walked node vanished"))?;
                            nodes.push(BlameWalkNode {
                                cs: current,
                                author: cs_row.author,
                                blob_new,
                                blob_parent,
                            });
                            if blob_parent.is_none() {
                                break; // the add: nothing older to resolve
                            }
                        }
                        // No row: the path is unchanged at this node —
                        // same blob as the parent, skip upward.
                    }
                }
                // Advance along the first parent. Reaching the root here
                // means the add row was never derived into blame_state —
                // derived data is missing, so degrade at the root rather
                // than present a hole as history.
                let graph =
                    db::changeset_graph(conn, repo_id, &current).map_err(ApiError::log_internal)?;
                match graph.and_then(|g| g.parents.into_iter().next()) {
                    Some(p) => current = p,
                    None => {
                        let cs_row = db::changeset_get(conn, repo_id, &current)
                            .map_err(ApiError::log_internal)?
                            .ok_or_else(|| ApiError::log_internal("walked node vanished"))?;
                        let blob = nodes
                            .last()
                            .and_then(|n| n.blob_parent)
                            .unwrap_or(final_blob);
                        boundary = Some(BlameBoundary {
                            cs: current,
                            author: cs_row.author,
                            blob,
                        });
                        break;
                    }
                }
            }
            let degraded = boundary.is_some() || !tainted_poison_cs.is_empty();
            Ok(BlameWalk {
                final_blob,
                nodes,
                boundary,
                tainted_poison_cs,
                degraded,
            })
        })
        .await?;

    // The degraded flag and its poisoned set: the walk's poison boundary
    // plus the nearest poisoned ancestors named by the rows' own taint.
    let degraded = walk.degraded;
    let mut poisoned_ids = walk.tainted_poison_cs.clone();
    if let Some(b) = &walk.boundary
        && !poisoned_ids.contains(&b.cs)
    {
        poisoned_ids.push(b.cs);
    }
    let poisoned_cs: Vec<String> = poisoned_ids.iter().map(Id::to_hex).collect();

    // Membership-gate every blob the assembly will stream.
    let mut blob_ids: Vec<Id> = vec![walk.final_blob];
    blob_ids.extend(walk.nodes.iter().map(|n| n.blob_new));
    if let Some(b) = &walk.boundary {
        blob_ids.push(b.blob);
    }
    blob_ids.dedup();
    let ids_for_check = blob_ids.clone();
    let missing = state
        .read(move |conn| {
            let mut out = Vec::new();
            for id in &ids_for_check {
                match db::repo_blob_check(conn, repo_id, id) {
                    Ok(false) => out.push(*id),
                    Ok(true) => {}
                    Err(e) => return Err(ApiError::log_internal(e)),
                }
            }
            Ok(out)
        })
        .await?;
    if let Some(id) = missing.first() {
        return Err(ApiError::new(
            ApiCode::ObjectNotFound,
            format!("blob {} is not in this repo", id.to_hex()),
        ));
    }

    // Layer metadata oldest-first: the boundary (or the oldest touched
    // node) is the base; the touched nodes walk newer from there. The
    // bytes themselves are NOT collected here — the assembly below
    // streams them one layer at a time.
    let (base_meta, mut step_meta): (BlameLayerMeta, Vec<BlameLayerMeta>) = match &walk.boundary {
        Some(b) => (
            (b.cs, b.author.clone(), b.blob),
            walk.nodes
                .iter()
                .map(|n| (n.cs, n.author.clone(), n.blob_new))
                .collect(),
        ),
        None => {
            let (base_node, rest) = walk.nodes.split_last().ok_or_else(|| {
                ApiError::log_internal("blame walk with neither add nor boundary")
            })?;
            (
                (base_node.cs, base_node.author.clone(), base_node.blob_new),
                rest.iter()
                    .map(|n| (n.cs, n.author.clone(), n.blob_new))
                    .collect(),
            )
        }
    };
    step_meta.reverse();

    // The assembly (CAS blob loads + Myers diffs) runs off the async
    // runtime, and each layer's bytes are loaded, diffed, and dropped
    // before the next is read — never all versions materialized at once.
    // Every load counts toward the read's blob-bytes cap, and every
    // layer's raw line count toward the line cap (checked in the
    // assembler before its per-line allocation).
    let cas = Arc::clone(&state.cas);
    let final_blob = walk.final_blob;
    let max_blob_bytes = state.blame_max_blob_bytes;
    let assembled = state
        .blocking(move || {
            let mut total_blob_bytes = 0usize;
            let mut load = |id: &Id| -> Result<Vec<u8>, ApiError> {
                let raw = cas.get(id).map_err(ApiError::log_internal)?;
                total_blob_bytes += raw.len();
                if total_blob_bytes > max_blob_bytes {
                    return Err(blame_too_large());
                }
                Ok(raw)
            };
            let final_bytes = load(&final_blob)?;
            // Binary at `at` is a structured empty result, never an error.
            if std::str::from_utf8(&final_bytes).is_err() {
                return Ok(None);
            }
            let base_bytes = load(&base_meta.2)?;
            let mut asm = crate::derived::BlameAssembler::new(BlameLayer {
                cs: base_meta.0,
                author: base_meta.1.clone(),
                bytes: &base_bytes,
            })
            .map_err(|_| blame_too_large())?;
            for (cs, author, blob) in &step_meta {
                let bytes = load(blob)?;
                asm.push_step(BlameLayer {
                    cs: *cs,
                    author: author.clone(),
                    bytes: &bytes,
                })
                .map_err(|_| blame_too_large())?;
            }
            Ok(Some(
                asm.finish()
                    .into_iter()
                    .map(|l| BlameLine {
                        cs: l.cs.to_hex(),
                        author: l.author,
                        text: l.text,
                    })
                    .collect::<Vec<BlameLine>>(),
            ))
        })
        .await?;
    let lines = match assembled {
        Some(lines) => lines,
        None => {
            return blame_json(BlameResponse {
                at: at.to_hex(),
                path: q.path,
                binary: true,
                binary_code: Some("blame_binary".into()),
                degraded,
                poisoned_cs,
                lines: Vec::new(),
            });
        }
    };
    blame_json(BlameResponse {
        at: at.to_hex(),
        path: q.path,
        binary: false,
        binary_code: None,
        degraded,
        poisoned_cs,
        lines,
    })
}

// ---- file -----------------------------------------------------------------

/// Query params of `/file`.
#[derive(Deserialize, IntoParams)]
pub struct FileQuery {
    /// Changeset id to read at.
    pub at: String,
    /// Repo-relative path.
    pub path: String,
}

/// A file lookup response.
#[derive(Serialize, ToSchema)]
pub struct FileResponse {
    pub blob_id: String,
    pub size: u64,
    pub exec: bool,
}

/// Looks one path up in a changeset's root tree (any authenticated
/// principal). Absent path, or a path that crosses a file, or a directory
/// path → 404 `not_found`.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/file",
    params(
        ("r" = String, Path, description = "Repo name"),
        FileQuery
    ),
    responses(
        (status = 200, description = "The file's blob pointer", body = FileResponse),
        (status = 400, description = "`at` is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo/changeset, path absent, or path crosses a file")
    )
)]
pub async fn file(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    Query(q): Query<FileQuery>,
) -> Result<Json<FileResponse>, ApiError> {
    let at = parse_id(&q.at)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let path = q.path.clone();
    let found = state
        .read(move |conn| {
            let path = q.path;
            let Some(row) =
                db::changeset_get(conn, repo_id, &at).map_err(ApiError::log_internal)?
            else {
                return Err(ApiError::new(
                    ApiCode::NotFound,
                    "changeset is not in this repo",
                ));
            };
            let Some(root_bytes) = row.root_tree_id else {
                return Ok(None);
            };
            let mut tree = Id(root_bytes.try_into().expect("root tree id is 32 bytes"));
            let components: Vec<&str> = path.split('/').collect();
            for (i, name) in components.iter().enumerate() {
                let entries = db::tree_entries_load(conn, &tree).map_err(ApiError::log_internal)?;
                let entry: &TreeEntryRow = entries
                    .iter()
                    .find(|e| e.name.as_str() == *name)
                    .ok_or_else(|| {
                        ApiError::new(ApiCode::NotFound, format!("no such path {path:?}"))
                    })?;
                let object = Id(entry
                    .object_id
                    .clone()
                    .try_into()
                    .expect("object id is 32 bytes"));
                let last = i == components.len() - 1;
                match (entry.kind, last) {
                    (0, true) => {
                        return Ok(Some((object, entry.size as u64, entry.exec)));
                    }
                    // Descend through intermediate directories; a file
                    // crossed mid-path, or a directory named as the file,
                    // is 404.
                    (1, false) => tree = object,
                    _ => {
                        return Err(ApiError::new(
                            ApiCode::NotFound,
                            format!("no such file {path:?}"),
                        ));
                    }
                }
            }
            unreachable!("every loop iteration returns; dirs continue")
        })
        .await?;
    match found {
        Some((blob, size, exec)) => Ok(Json(FileResponse {
            blob_id: blob.to_hex(),
            size,
            exec,
        })),
        None => Err(ApiError::new(
            ApiCode::NotFound,
            format!("no such file {path:?}"),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Test ids are single-byte-tagged.
    fn cid(tag: u8) -> Id {
        let mut b = [0u8; 32];
        b[0] = tag;
        Id(b)
    }

    /// Builds a fetch closure over an in-memory graph: (node tag, gen,
    /// parent tags) → lookup.
    fn graph(
        edges: Vec<(u8, i64, Vec<u8>)>,
    ) -> impl Fn(&Id) -> Result<Option<(i64, Vec<Id>)>, ApiError> {
        move |id: &Id| {
            Ok(edges.iter().find(|(name, _, _)| cid(*name) == *id).map(
                |(_, generation, parents)| (*generation, parents.iter().map(|p| cid(*p)).collect()),
            ))
        }
    }

    /// Diamond: c1 ← {c2a, c2b}; merge-base(c2a, c2b) == c1 in both orders.
    #[test]
    fn diamond_forks_meet_at_the_fork_point() {
        let edges = vec![
            (1u8, 0i64, vec![]), // c1
            (2u8, 1, vec![1u8]), // c2a
            (3u8, 1, vec![1u8]), // c2b
        ];
        let fetch = graph(edges);
        assert_eq!(merge_base_walk(&fetch, &cid(2), &cid(3)), Some(cid(1)));
        assert_eq!(merge_base_walk(&fetch, &cid(3), &cid(2)), Some(cid(1)));
        // A node with itself (and with its ancestor) resolves directly.
        assert_eq!(merge_base_walk(&fetch, &cid(2), &cid(2)), Some(cid(2)));
        assert_eq!(merge_base_walk(&fetch, &cid(2), &cid(1)), Some(cid(1)));
    }

    /// Disconnected roots share nothing → null.
    #[test]
    fn disconnected_histories_are_null() {
        let edges = vec![
            (1u8, 0i64, vec![]),
            (2u8, 1, vec![1u8]),
            (9u8, 0i64, vec![]),
            (10u8, 1, vec![9u8]),
        ];
        let fetch = graph(edges);
        assert_eq!(merge_base_walk(&fetch, &cid(2), &cid(10)), None);
    }

    /// Uneven generations: a long chain meeting a short branch — the base
    /// sits below the short side's generation and must still be found.
    #[test]
    fn uneven_sides_find_the_deep_base() {
        // chain c1→c2→c3→c4; branch off c2.
        let edges = vec![
            (1u8, 0i64, vec![]),
            (2u8, 1, vec![1u8]),
            (3u8, 2, vec![2u8]),
            (4u8, 3, vec![3u8]),
            (5u8, 2, vec![2u8]),
        ];
        let fetch = graph(edges);
        assert_eq!(merge_base_walk(&fetch, &cid(4), &cid(5)), Some(cid(2)));
    }

    // ---- merge_base_skip (the derived commit-graph path) -------------------

    /// An in-memory graph with skip entries built by the SAME chained-
    /// doubling rule the deriver uses (entry 0 = first parent, hop 1,
    /// pure; entry k = the parent's entry k−1 composed with the midpoint's
    /// entry k−1, hop = 2·d(k−1)+1, pure only when the parent and midpoint
    /// are single-parent and both sub-spans are pure). An independent
    /// reimplementation on purpose: the deriver and the route meet here.
    fn skip_graph(
        edges: Vec<(u8, i64, Vec<u8>)>,
    ) -> impl Fn(&Id) -> Result<Option<SkipNode>, ApiError> {
        let nodes: std::collections::HashMap<u8, (i64, Vec<u8>)> = edges
            .into_iter()
            .map(|(n, generation, parents)| (n, (generation, parents)))
            .collect();
        // Compute entries per node in generation order (parents first).
        let mut skips: std::collections::HashMap<u8, Vec<(u8, u64, i64, bool)>> =
            std::collections::HashMap::new();
        let mut order: Vec<u8> = nodes.keys().copied().collect();
        order.sort_by_key(|n| nodes[n].0);
        for name in order {
            let (_node_gen, parents) = nodes[&name].clone();
            let mut entries: Vec<(u8, u64, i64, bool)> = Vec::new();
            if let Some(&p1) = parents.first() {
                entries.push((p1, 1, nodes[&p1].0, true));
                let p1_skips = skips.get(&p1).cloned().unwrap_or_default();
                for k in 1.. {
                    let Some(mid) = p1_skips.get(k - 1) else {
                        break;
                    };
                    let mid_skips = skips.get(&mid.0).cloned().unwrap_or_default();
                    let Some(far) = mid_skips.get(k - 1) else {
                        break;
                    };
                    let p1_single = nodes[&p1].1.len() <= 1;
                    let mid_single = nodes[&mid.0].1.len() <= 1;
                    let pure = p1_single && mid_single && mid.3 && far.3;
                    entries.push((far.0, far.1 * 2 + 1, far.2, pure));
                }
            }
            skips.insert(name, entries);
        }
        move |id: &Id| {
            let (b, _rest) = id.0.split_first().expect("test ids are byte-tagged");
            let Some((generation, parents)) = nodes.get(b) else {
                return Ok(None);
            };
            Ok(Some(SkipNode {
                generation: *generation,
                parents: parents.iter().map(|p| cid(*p)).collect(),
                skips: skips
                    .get(b)
                    .cloned()
                    .unwrap_or_default()
                    .into_iter()
                    .map(|(t, hop, generation, pure)| SkipEdge {
                        target: cid(t),
                        hop,
                        generation,
                        pure,
                    })
                    .collect(),
            }))
        }
    }

    /// The brute-force oracle: the HIGHEST generation in the ancestor-set
    /// intersection (a generation, not a node — several max-generation
    /// common ancestors may exist, and the walks' tie-breaks legitimately
    /// differ).
    fn brute_force_gen<F>(fetch: &F, a: &Id, b: &Id) -> Option<i64>
    where
        F: Fn(&Id) -> Result<Option<(i64, Vec<Id>)>, ApiError>,
    {
        let ancestors = |seed: &Id| -> std::collections::HashSet<Id> {
            let mut set = std::collections::HashSet::new();
            let mut queue = std::collections::VecDeque::from([*seed]);
            while let Some(id) = queue.pop_front() {
                if !set.insert(id) {
                    continue;
                }
                if let Ok(Some((_, parents))) = fetch(&id) {
                    queue.extend(parents);
                }
            }
            set
        };
        let in_a = ancestors(a);
        ancestors(b)
            .into_iter()
            .filter(|id| in_a.contains(id))
            .map(|id| fetch(&id).ok().flatten().map(|(generation, _)| generation))
            .fold(None::<i64>, |best, g| match (best, g) {
                (Some(b), Some(g)) => Some(b.max(g)),
                (None, Some(g)) => Some(g),
                (best, None) => best,
            })
    }

    /// Checks every ordered pair of a graph: the plain walk, the skip-list
    /// walk, and the brute-force ancestor sets agree on the best common
    /// ancestor's generation.
    fn check_all_pairs(edges: Vec<(u8, i64, Vec<u8>)>) {
        let fetch = graph(edges.clone());
        let skip_fetch = skip_graph(edges);
        for a in 1u8..=250 {
            for b in 1u8..=250 {
                if fetch(&cid(a)).unwrap().is_none() || fetch(&cid(b)).unwrap().is_none() {
                    continue;
                }
                let gen_of = |id: Option<Id>| -> Option<i64> {
                    id.and_then(|id| fetch(&id).unwrap().map(|(generation, _)| generation))
                };
                let plain = merge_base_walk(&fetch, &cid(a), &cid(b));
                let skipped = merge_base_skip(&skip_fetch, &cid(a), &cid(b));
                let brute = brute_force_gen(&fetch, &cid(a), &cid(b));
                assert_eq!(gen_of(plain), brute, "plain vs brute for ({a},{b})");
                assert_eq!(
                    gen_of(skipped),
                    gen_of(plain),
                    "skip vs plain for pair ({a},{b}) — the alignment must stay exact"
                );
            }
        }
    }

    /// A 200-node linear mainline with a short branch: the alignment's
    /// jumps fire (200 spans several hops) and the base is the fork point.
    #[test]
    fn skip_merge_base_on_a_long_mainline() {
        let mut edges = vec![(0u8, 0i64, vec![])];
        for i in 1..=200u8 {
            edges.push((i, i as i64, vec![i - 1]));
        }
        // branch off the root
        edges.push((201u8, 1, vec![0u8]));
        let skip_fetch = skip_graph(edges);
        assert_eq!(
            merge_base_skip(&skip_fetch, &cid(200), &cid(201)),
            Some(cid(0))
        );
        assert_eq!(
            merge_base_skip(&skip_fetch, &cid(201), &cid(200)),
            Some(cid(0)),
            "argument order must not matter"
        );
        // ancestor case: base is the lower endpoint itself
        assert_eq!(
            merge_base_skip(&skip_fetch, &cid(200), &cid(5)),
            Some(cid(5))
        );
    }

    /// The frontier case: the only common ancestor through a climbed
    /// node's NON-first parent — the phase-1 side-branch enqueuing must
    /// keep it reachable, and the answer is the merge side itself.
    #[test]
    fn skip_merge_base_through_a_side_branch() {
        // c1→c2→c3→c4 linear; m = merge(c4, s1); s1 branches off c1.
        let edges = vec![
            (1u8, 0i64, vec![]),
            (2u8, 1, vec![1u8]),
            (3u8, 2, vec![2u8]),
            (4u8, 3, vec![3u8]),
            (5u8, 1, vec![1u8]),      // s1
            (6u8, 4, vec![4u8, 5u8]), // m: first parent c4
        ];
        let skip_fetch = skip_graph(edges.clone());
        assert_eq!(
            merge_base_skip(&skip_fetch, &cid(6), &cid(5)),
            Some(cid(5)),
            "the base sits on m's second parent — the frontier must carry it"
        );
        check_all_pairs(edges);
    }

    /// Deep diamonds: merges on BOTH sides, several of them, branchy
    /// spans forcing the purity degradation to single-parent steps — the
    /// skip path must stay exact (all pairs vs plain walk and brute
    /// force).
    #[test]
    fn skip_merge_base_stays_exact_on_branchy_graphs() {
        let edges = vec![
            (1u8, 0i64, vec![]),
            (2u8, 1, vec![1u8]),
            (3u8, 1, vec![1u8]),
            (4u8, 2, vec![2u8, 3u8]), // merge
            (5u8, 2, vec![2u8, 3u8]), // merge (same parents, different node)
            (6u8, 3, vec![4u8]),
            (7u8, 3, vec![5u8]),
            (8u8, 4, vec![6u8, 7u8]), // merge of merges
            (9u8, 2, vec![4u8, 5u8]),
        ];
        check_all_pairs(edges);
    }

    /// A corrupt-but-clean SELF-REFERENTIAL skip entry (target = itself,
    /// so the jump never descends) cannot hang or missteer the climb: the
    /// jump is refused, the plain first-parent step takes over, and the
    /// answer matches the honest graph.
    #[test]
    fn climb_refuses_a_self_referential_skip() {
        let edges = linear_chain(6);
        let skip_fetch = skip_graph(edges);
        let corrupt = |id: &Id| -> Result<Option<SkipNode>, ApiError> {
            let mut node = skip_fetch(id)?.expect("graph has the node");
            if *id == cid(6) {
                node.skips = vec![SkipEdge {
                    target: cid(6),
                    hop: 15,
                    generation: 6,
                    pure: true,
                }];
            }
            Ok(Some(node))
        };
        assert_eq!(merge_base_skip(&corrupt, &cid(6), &cid(0)), Some(cid(0)));
        assert_eq!(merge_base_skip(&corrupt, &cid(0), &cid(6)), Some(cid(0)));
    }

    /// A corrupt CYCLIC first-parent edge (a node its own parent, equal
    /// generation) ends the climb — the seeded walk below is seen-set
    /// safe and answers what the graph honestly holds (no common
    /// ancestor).
    #[test]
    fn climb_breaks_on_a_cyclic_first_parent() {
        let edges = vec![
            (1u8, 5i64, vec![1u8]), // corrupt: parent = itself
            (2u8, 0i64, vec![]),
        ];
        let fetch = graph(edges);
        assert_eq!(merge_base_walk(&fetch, &cid(1), &cid(2)), None);
        // The corrupt edge is only reachable through the graph() shape
        // (no skips): drive merge_base_skip over the same closure by
        // wrapping it with empty skips.
        let as_skip = |id: &Id| -> Result<Option<SkipNode>, ApiError> {
            Ok(fetch(id)?.map(|(generation, parents)| SkipNode {
                generation,
                parents,
                skips: Vec::new(),
            }))
        };
        assert_eq!(merge_base_skip(&as_skip, &cid(1), &cid(2)), None);
    }

    /// A linear chain `0 → 1 → … → n`, generation = tag.
    fn linear_chain(n: u8) -> Vec<(u8, i64, Vec<u8>)> {
        let mut edges = vec![(0u8, 0i64, vec![])];
        for i in 1..=n {
            edges.push((i, i as i64, vec![i - 1]));
        }
        edges
    }
}
