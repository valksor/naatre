//! `/v1/repos/{r}/trees/{id}`: read one derived tree node's entries.

use axum::Extension;
use axum::Json;
use axum::extract::{Path, State};
use serde::Serialize;
use utoipa::ToSchema;

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::AuthedPrincipal;
use crate::db;
use crate::routes::repo_id_or_404;
use crate::state::AppState;

/// One entry of a tree listing.
#[derive(Serialize, ToSchema)]
pub struct TreeEntryOut {
    pub name: String,
    /// `file` | `dir`.
    pub kind: String,
    /// Blob id for files, child tree id for dirs.
    pub object_id: String,
    /// File: content bytes. Dir: recursive total.
    pub size: u64,
    pub exec: bool,
}

/// A tree listing response.
#[derive(Serialize, ToSchema)]
pub struct TreeResponse {
    pub entries: Vec<TreeEntryOut>,
}

/// Reads one tree node's entries (any authenticated principal with repo
/// access). Tree ids are content-addressed, so identical subtrees share one
/// row across repos; the read is gated on `repo_trees` membership — 200 iff
/// *this* repo wrote the tree (directly or as part of its derivation), 404
/// for any repo that never wrote it, even though the id exists elsewhere.
/// Shared subtrees are readable by every repo that wrote them (see
/// `docs/design.md`, honest-scope section).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/trees/{id}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Tree id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "The tree's entries; this repo has membership for it", body = TreeResponse),
        (status = 400, description = "Id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo, or no tree membership for this repo")
    )
)]
pub async fn get_tree(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex)): Path<(String, String)>,
) -> Result<Json<TreeResponse>, ApiError> {
    let id = Id::from_hex(&id_hex).ok_or_else(|| {
        ApiError::new(ApiCode::Malformed, "id must be 64 lowercase hex characters")
    })?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let rows = state
        .read(move |conn| {
            if !db::repo_has_tree(conn, repo_id, &id).map_err(ApiError::log_internal)? {
                return Ok(None);
            }
            db::tree_entries_load(conn, &id)
                .map_err(ApiError::log_internal)
                .map(Some)
        })
        .await?;
    let Some(rows) = rows else {
        return Err(ApiError::new(
            ApiCode::NotFound,
            format!("no tree {id_hex} written in {repo:?}"),
        ));
    };
    Ok(Json(TreeResponse {
        entries: rows
            .into_iter()
            .map(|e| TreeEntryOut {
                name: e.name,
                kind: match e.kind {
                    0 => "file".into(),
                    _ => "dir".into(),
                },
                object_id: Id(e.object_id.try_into().expect("object id is 32 bytes")).to_hex(),
                size: e.size as u64,
                exec: e.exec,
            })
            .collect(),
    }))
}
