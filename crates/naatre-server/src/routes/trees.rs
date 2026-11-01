//! `/v1/repos/{r}/trees/{id}`: read one derived tree node's entries.

use axum::Extension;
use axum::Json;
use axum::extract::{Path, State};
use serde::Serialize;
use utoipa::ToSchema;

use naatre_core::ids::Id;
use naatre_store::Db;

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
    /// its size, exec bit, and hash do not).
    pub object_id: Option<String>,
    /// File: content bytes. Dir: recursive total. Null on a redacted
    /// entry, and null on a dir entry under which ANY descendant blob is
    /// tombstoned in this repo (the aggregate would embed the redacted
    /// file's exact byte length — listing the parent and subtracting the
    /// children would recover it).
    pub size: Option<u64>,
    /// Exec bit; null on a redacted entry (a content attribute of
    /// redacted content does not serve).
    pub exec: Option<bool>,
    /// True when this entry's own blob is tombstoned in this repo and its
    /// pointer/size/exec are withheld. A size-null dir with
    /// `redacted: false` carries the aggregate mask above.
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
/// redacted objects never serve, including via a live root tree), and a
/// dir entry whose recursive blob closure carries a tombstone serves
/// size-null (the aggregate mask).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/trees/{id}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Tree id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "The tree's entries; this repo has membership for it. Entries whose blob is redacted in this repo serve with `redacted: true` and null object_id/size/exec; a dir entry with a redacted descendant serves size-null", body = TreeResponse),
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
    let entries = state
        .read(move |conn| {
            if !db::repo_has_tree(conn, repo_id, &id).map_err(ApiError::log_internal)? {
                return Ok(None);
            }
            let rows = db::tree_entries_load(conn, &id).map_err(ApiError::log_internal)?;
            Ok::<_, ApiError>(Some(mask_tree_entries(conn, repo_id, rows)?))
        })
        .await?;
    let Some(entries) = entries else {
        return Err(ApiError::new(
            ApiCode::NotFound,
            format!("no tree {id_hex} written in {repo:?}"),
        ));
    };
    Ok(Json(TreeResponse { entries }))
}

/// The masked listing shared by the repo tree route and the snapshot-tree
/// route (the same response shape, the same gate, one implementation): the
/// plan's universal gate covers every serving path, so both listings mask
/// identically.
///
/// Two bounded probes per listing, both set-based: (1) one chunked
/// IN-list over the entries' own object ids (a dir entry's object id is a
/// tree id, in a disjoint hash domain — only file entries can hit);
/// (2) one recursive CTE per chunk of dir ids
/// ([`db::redactions_dirty_dirs`], the `gc_collectible_trees` precedent)
/// returning the dirs with any tombstoned blob descendant. A file entry
/// whose blob is tombstoned in this repo serves with `redacted: true` and
/// null object_id/size/exec; a dir entry with a tombstoned descendant
/// serves size-null (the stored size is the recursive total, so an
/// unmasked aggregate would leak the redacted file's exact byte length
/// through parent-minus-children arithmetic). The name serves by design.
pub(crate) fn mask_tree_entries(
    conn: &Db,
    repo_id: i64,
    rows: Vec<db::TreeEntryRow>,
) -> Result<Vec<TreeEntryOut>, ApiError> {
    let entry_ids: Vec<Id> = rows
        .iter()
        .filter_map(|e| {
            let arr: [u8; 32] = e.object_id.clone().try_into().ok()?;
            Some(Id(arr))
        })
        .collect();
    let gated: std::collections::HashSet<Id> =
        db::redactions_active_among(conn, repo_id, &entry_ids)
            .map_err(ApiError::log_internal)?
            .into_iter()
            .collect();
    let dir_ids: Vec<Id> = rows
        .iter()
        .filter(|e| e.kind != 0)
        .map(|e| {
            Id(e.object_id
                .clone()
                .try_into()
                .expect("object id is 32 bytes"))
        })
        .collect();
    let dirty_dirs: std::collections::HashSet<Id> =
        db::redactions_dirty_dirs(conn, repo_id, &dir_ids)
            .map_err(ApiError::log_internal)?
            .into_iter()
            .collect();
    Ok(rows
        .into_iter()
        .map(|e| {
            let object = Id(e.object_id.try_into().expect("object id is 32 bytes"));
            let redacted = gated.contains(&object);
            let aggregate_redacted = e.kind != 0 && dirty_dirs.contains(&object);
            TreeEntryOut {
                name: e.name,
                kind: match e.kind {
                    0 => "file".into(),
                    _ => "dir".into(),
                },
                object_id: (!redacted).then(|| object.to_hex()),
                size: if redacted || aggregate_redacted {
                    None
                } else {
                    Some(e.size as u64)
                },
                exec: if redacted { None } else { Some(e.exec) },
                redacted,
            }
        })
        .collect())
}
