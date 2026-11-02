//! `/v1/repos/{r}/access`: the repo read/write ACL surface (M13b, admin).
//! A PUT grants (or updates) one principal's flags on one repo; a DELETE
//! removes the grant; a GET lists the repo's grants. Both mutations are
//! audited (`access_grant` / `access_revoke`, the acting admin is the
//! audit row's principal — the check list's "audit rows asserted for ACL
//! grants/revocations"). The invariant `write implies read` is refused,
//! not silently corrected: a body with `write = true, read = false` is a
//! 422.
//!
//! Visibility: a repo with at least one `repo_access` row is PRIVATE —
//! non-readers meet the uniform 404 on every repo-scoped route and the
//! listing drops it (see [`crate::routes::repo_read_gate`]); a repo with
//! no rows is PUBLIC (every authenticated principal reads — the pre-M13
//! behavior). Revoking a repo's LAST grant returns it to public; that is
//! the documented flip, not a leak (the operator revoked access on
//! purpose; README "Repo access").

use axum::extract::{Path, State};
use axum::response::IntoResponse;
use axum::{Extension, Json};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db;
use crate::extract::AppJson;
use crate::state::AppState;

/// Request body of `PUT /v1/repos/{r}/access`.
#[derive(Deserialize, ToSchema)]
pub struct SetAccessRequest {
    /// The (existing) principal to grant.
    pub principal: String,
    /// Grant read (see the module doc: a repo's first grant makes it
    /// private; write implies read).
    pub read: bool,
    /// Grant write (implies read).
    pub write: bool,
}

/// One grant as served by the list and the PUT response.
#[derive(Serialize, ToSchema)]
pub struct AccessEntry {
    pub principal: String,
    pub read: bool,
    pub write: bool,
}

/// List body.
#[derive(Serialize, ToSchema)]
pub struct AccessListResponse {
    pub access: Vec<AccessEntry>,
}

/// Response of PUT and DELETE.
#[derive(Serialize, ToSchema)]
pub struct AccessSetResponse {
    pub repo: String,
    pub principal: String,
    pub read: bool,
    pub write: bool,
}

/// Lists the repo's grants (admin). Empty = the repo is public.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/access",
    params(("r" = String, Path, description = "Repo name")),
    responses(
        (status = 200, description = "The repo's grants (empty = the repo is public)", body = AccessListResponse),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No such repo")
    )
)]
pub async fn list(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
) -> Result<Json<AccessListResponse>, ApiError> {
    // The M13b read gate first: a private repo's non-reader gets the
    // uniform 404 even on this admin surface; the admin matrix follows.
    crate::routes::repo_read_gate(&state, &auth, &repo).await?;
    require_role(&auth, Role::Admin)?;
    let repo_id = crate::routes::repo_id_or_404(&state, &repo).await?;
    let rows = state
        .read(move |conn| db::access_list(conn, repo_id).map_err(ApiError::log_internal))
        .await?;
    Ok(Json(AccessListResponse {
        access: rows
            .into_iter()
            .map(|row| AccessEntry {
                principal: row.principal,
                read: row.read,
                write: row.write,
            })
            .collect(),
    }))
}

/// Grants (or updates) one principal's read/write flags on the repo
/// (admin, audited `access_grant`). The principal must exist (422
/// otherwise — grants name global principals); `write = true` implies
/// read (a `read = false, write = true` body is refused 422, never
/// silently upgraded). A repo's FIRST grant makes it private: from this
/// call on, non-readers meet the uniform 404 everywhere and the listing
/// drops the repo.
#[utoipa::path(
    put,
    path = "/v1/repos/{r}/access",
    params(("r" = String, Path, description = "Repo name")),
    request_body = SetAccessRequest,
    responses(
        (status = 200, description = "Grant recorded (the stored flags; write implies read)", body = AccessSetResponse),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No such repo"),
        (status = 422, description = "Unknown principal, empty principal name, or write-without-read")
    )
)]
pub async fn set(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<SetAccessRequest>,
) -> Result<axum::response::Response, ApiError> {
    // The read gate first (see list): invisible repos are invisible to
    // their ACL surface too; the admin matrix follows.
    crate::routes::repo_read_gate(&state, &auth, &repo).await?;
    require_role(&auth, Role::Admin)?;
    let AppJson(body) = body;
    if body.principal.trim().is_empty() {
        return Err(ApiError::new(
            ApiCode::Validation,
            "principal must not be empty",
        ));
    }
    if body.write && !body.read {
        return Err(ApiError::new(
            ApiCode::Validation,
            "write implies read: a grant with write = true carries read = true",
        ));
    }
    let repo_id = crate::routes::repo_id_or_404(&state, &repo).await?;
    let now_ns = state.now_ns() as i64;
    let actor = auth.principal.clone();
    let repo_name = repo.clone();
    let principal = body.principal.clone();
    let granted = state
        .write(move |store| {
            db::access_grant(
                store.conn(),
                repo_id,
                &repo_name,
                &principal,
                body.read,
                body.write,
                now_ns,
                &actor,
            )
            .map_err(ApiError::log_internal)
        })
        .await?;
    let Some((read, write)) = granted else {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("no principal named {:?} on this server", body.principal),
        ));
    };
    Ok(Json(AccessSetResponse {
        repo,
        principal: body.principal,
        read,
        write,
    })
    .into_response())
}

/// Removes one principal's grant (admin, audited `access_revoke` — an
/// absent grant is a silent 200, nothing happened so nothing is rowed).
/// Removing the repo's LAST grant returns it to PUBLIC (the documented
/// flip; README "Repo access"). Revoking read removes write with it
/// (write without read cannot stand).
#[utoipa::path(
    delete,
    path = "/v1/repos/{r}/access/{principal}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("principal" = String, Path, description = "Principal whose grant is removed")
    ),
    responses(
        (status = 200, description = "Grant removed (or none stood)", body = AccessSetResponse),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No such repo")
    )
)]
pub async fn revoke(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, principal)): Path<(String, String)>,
) -> Result<Json<AccessSetResponse>, ApiError> {
    // The read gate first (see list).
    crate::routes::repo_read_gate(&state, &auth, &repo).await?;
    require_role(&auth, Role::Admin)?;
    let repo_id = crate::routes::repo_id_or_404(&state, &repo).await?;
    let now_ns = state.now_ns() as i64;
    let actor = auth.principal.clone();
    let repo_name = repo.clone();
    let principal_name = principal.clone();
    let removed = state
        .write(move |store| {
            db::access_revoke(
                store.conn(),
                repo_id,
                &repo_name,
                &principal_name,
                now_ns,
                &actor,
            )
            .map_err(ApiError::log_internal)
        })
        .await?;
    let _ = removed;
    Ok(Json(AccessSetResponse {
        repo,
        principal,
        read: false,
        write: false,
    }))
}
