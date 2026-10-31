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
    /// Blob id for files, child tree id for dirs — null with
    /// `redacted: true` when the entry's blob is tombstoned in this repo
    /// (M10b SEC gate: the name serves, the pointer to redacted content,
    /// its size, and its hash do not).
    pub object_id: Option<String>,
    /// File: content bytes. Dir: recursive total. Null on a redacted
    /// entry (the size of redacted content does not serve).
    pub size: Option<u64>,
    pub exec: bool,
    /// True when this entry's blob is tombstoned in this repo and its
    /// pointer/size are withheld.
    pub redacted: bool,
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
/// `docs/design.md`, honest-scope section). Entries whose blob is
/// tombstoned in this repo — active or purged — serve masked (M10b SEC
/// gate, the diff route's per-entry shape: file names/sizes/hashes of
/// redacted objects never serve, including via a live root tree).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/trees/{id}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Tree id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "The tree's entries; this repo has membership for it. Entries whose blob is redacted in this repo serve with `redacted: true` and null object_id/size", body = TreeResponse),
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
            let rows = db::tree_entries_load(conn, &id).map_err(ApiError::log_internal)?;
            // One batch tombstone probe over the entries' object ids (the
            // diff route's masking probe; chunked IN-lists).
            let gated: std::collections::HashSet<Id> = db::redactions_active_among(
                conn,
                repo_id,
                &rows
                    .iter()
                    .filter_map(|e| {
                        let arr: [u8; 32] = e.object_id.clone().try_into().ok()?;
                        Some(Id(arr))
                    })
                    .collect::<Vec<_>>(),
            )
            .map_err(ApiError::log_internal)?
            .into_iter()
            .collect();
            Ok::<_, ApiError>(Some((rows, gated)))
        })
        .await?;
    let Some((rows, gated)) = rows else {
        return Err(ApiError::new(
            ApiCode::NotFound,
            format!("no tree {id_hex} written in {repo:?}"),
        ));
    };
    Ok(Json(TreeResponse {
        entries: rows
            .into_iter()
            .map(|e| {
                let object = Id(e.object_id.try_into().expect("object id is 32 bytes"));
                let redacted = gated.contains(&object);
                TreeEntryOut {
                    name: e.name,
                    kind: match e.kind {
                        0 => "file".into(),
                        _ => "dir".into(),
                    },
                    object_id: (!redacted).then(|| object.to_hex()),
                    size: (!redacted).then_some(e.size as u64),
                    exec: e.exec,
                    redacted,
                }
            })
            .collect(),
    }))
}
