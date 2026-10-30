//! `/v1/repos/{r}/log`, `/diff`, `/merge-base`, `/file`: the history reads.
//! All any-authenticated, repo-scoped; unknown repo → 404 `repo_not_found`,
//! unknown ids → 404 `not_found`.

use std::collections::BTreeMap;

use axum::Extension;
use axum::Json;
use axum::extract::{Path, Query, State};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::AuthedPrincipal;
use crate::db::{self, TreeEntryRow};
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
    /// page when `main` does not exist (the unborn-repo edge).
    pub from: Option<String>,
    /// Page size (1–200; default 50).
    pub limit: Option<String>,
    /// Opaque `after:<last id>` cursor from the previous page.
    pub cursor: Option<String>,
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
) -> Result<Json<LogResponse>, ApiError> {
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
        .read(move |store| {
            let conn = store.conn();
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
    Ok(Json(LogResponse {
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
    }))
}

/// Parses a cursor id where non-hex means "garbage cursor" (400), not the
/// generic malformed-id message.
fn parse_id_hex(hex: &str, err: ApiError) -> Result<Id, ApiError> {
    Id::from_hex(hex).ok_or(err)
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
        .read(move |store| {
            let conn = store.conn();
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
fn list_files(
    conn: &rusqlite::Connection,
    root: &Id,
) -> Result<BTreeMap<String, (Id, bool)>, ApiError> {
    let mut out = BTreeMap::new();
    walk_files(conn, root, String::new(), &mut out)?;
    Ok(out)
}

fn walk_files(
    conn: &rusqlite::Connection,
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
        .read(move |store| {
            let conn = store.conn();
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
            let fetch = |id: &Id| -> Result<Option<(i64, Vec<Id>)>, ApiError> {
                db::changeset_graph(conn, repo_id, id)
                    .map(|g| g.map(|g| (g.generation, g.parents)))
                    .map_err(ApiError::log_internal)
            };
            Ok(merge_base_walk(&fetch, &a, &b))
        })
        .await?;
    Ok(Json(MergeBaseResponse {
        merge_base: base.map(|id| id.to_hex()),
    }))
}

/// The merge-base walk, parameterized over the graph lookup so the unit
/// tests below can drive it on in-memory diamonds.
///
/// Generations bound the walk: a parent's generation is always at least one
/// below its child's, so both passes strictly descend and terminate. The
/// lower-generation side is collected into an ancestor set; the other side
/// walks down over it and the highest-generation hit wins (the best common
/// ancestor). (The O(distance) skip-list version lands with the M6
/// commit-graph; this is the straightforward generation-bounded walk.)
pub(crate) fn merge_base_walk<F>(fetch: &F, a: &Id, b: &Id) -> Option<Id>
where
    F: Fn(&Id) -> Result<Option<(i64, Vec<Id>)>, ApiError>,
{
    let (gen_a, _) = fetch(a).ok().flatten()?;
    let (gen_b, _) = fetch(b).ok().flatten()?;

    // Ancestors of the lower-generation side (inclusive).
    let (lower, higher) = if gen_a <= gen_b { (a, b) } else { (b, a) };
    let mut ancestor_set: std::collections::HashSet<Id> = std::collections::HashSet::new();
    let mut queue = std::collections::VecDeque::from([*lower]);
    while let Some(id) = queue.pop_front() {
        if !ancestor_set.insert(id) {
            continue;
        }
        let Ok(Some((_, parents))) = fetch(&id) else {
            continue;
        };
        queue.extend(parents);
    }

    // Walk down from the higher side, remembering the best hit.
    let mut best: Option<(i64, Id)> = None;
    let mut seen: std::collections::HashSet<Id> = std::collections::HashSet::new();
    let mut queue = std::collections::VecDeque::from([*higher]);
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
        .read(move |store| {
            let conn = store.conn();
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
}
