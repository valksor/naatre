//! `/v1/repos/{r}/refs/{name}`: ref read and the admin compare-and-swap
//! move. `main` never moves through this route (land-only, M4).

use axum::Extension;
use axum::Json;
use axum::extract::{Path, State};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db::{self, RefCas};
use crate::extract::AppJson;
use crate::routes::repo_id_or_404;
use crate::state::AppState;

/// A ref read or move result.
#[derive(Serialize, ToSchema)]
pub struct RefResponse {
    pub name: String,
    /// Lowercase-hex target id, or null for an unborn ref.
    pub target: Option<String>,
    /// Monotonic version — supply as `expected.version` on move (CAS).
    pub version: i64,
}

/// Request body of the ref move.
#[derive(Deserialize, ToSchema)]
pub struct CreateRefRequest {
    /// Lowercase-hex target id (64 chars).
    pub target: String,
    /// `null` = create the ref (409 `ref_conflict` if it exists);
    /// `{"version": v}` = move only while the current version is `v`.
    pub expected: Option<ExpectedRef>,
}

/// The CAS expectation.
#[derive(Deserialize, ToSchema)]
pub struct ExpectedRef {
    pub version: i64,
}

/// Read one ref (any authenticated principal). Unknown repo →
/// `repo_not_found`; unknown ref → `not_found`.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/refs/{name}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("name" = String, Path, description = "Ref name")
    ),
    responses(
        (status = 200, description = "The ref", body = RefResponse),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo or ref")
    )
)]
pub async fn get_ref(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path((repo, name)): Path<(String, String)>,
) -> Result<Json<RefResponse>, ApiError> {
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let name_for_lookup = name.clone();
    let row = state
        .read(move |store| {
            db::ref_get(store.conn(), repo_id, &name_for_lookup).map_err(ApiError::log_internal)
        })
        .await?;
    let row = row.ok_or_else(|| {
        ApiError::new(
            ApiCode::NotFound,
            format!("no ref named {name:?} in {repo:?}"),
        )
    })?;
    Ok(Json(RefResponse {
        name: row.name,
        target: row.target.map(|bytes| {
            let id = Id(bytes.try_into().expect("ref target is a 32-byte id"));
            id.to_hex()
        }),
        version: row.version,
    }))
}

/// Create or compare-and-swap move a ref (admin, audited as `ref_move`).
///
/// Rules: `main` is rejected with 422 (`main` moves only via land, M4);
/// `target` must be 64 lowercase hex (else 400); `expected: null` creates
/// the ref (409 `ref_conflict` if it exists); `expected: {"version": v}`
/// moves only while the current version is `v` (stale → 409
/// `ref_conflict` with `current_version`/`current_target` details).
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/refs/{name}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("name" = String, Path, description = "Ref name (never `main`)")
    ),
    request_body = CreateRefRequest,
    responses(
        (status = 200, description = "Ref created or moved", body = RefResponse),
        (status = 400, description = "Target is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No such repo, or CAS expected a ref that does not exist"),
        (status = 409, description = "CAS lost (`ref_conflict`); details carry the current version and target"),
        (status = 422, description = "`main` moves only via land")
    )
)]
pub async fn cas_ref(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, name)): Path<(String, String)>,
    body: AppJson<CreateRefRequest>,
) -> Result<Json<RefResponse>, ApiError> {
    require_role(&auth, Role::Admin)?;
    if name == "main" {
        return Err(ApiError::new(
            ApiCode::Validation,
            "main moves only via land",
        ));
    }
    let target = Id::from_hex(&body.0.target).ok_or_else(|| {
        ApiError::new(
            ApiCode::Malformed,
            "target must be 64 lowercase hex characters",
        )
    })?;
    let expected = body.0.expected.as_ref().map(|e| e.version);
    let now_ns = state.now_ns() as i64;
    let principal = auth.principal.clone();
    // One state.write for repo check + CAS: everything that needs the
    // single-writer slot happens under one try_lock, so contention surfaces
    // as 503 writer_busy instead of queueing behind the holder.
    let repo_name = repo.clone();
    let ref_name = name.clone();
    let outcome = state
        .write(move |store| {
            let repo_id = db::repo_id_by_name(store.conn(), &repo_name)
                .map_err(ApiError::log_internal)?
                .ok_or_else(|| {
                    ApiError::new(
                        ApiCode::RepoNotFound,
                        format!("no repo named {repo_name:?} on this server"),
                    )
                })?;
            db::ref_cas(
                store.conn(),
                db::RefMove {
                    repo_id,
                    repo_name: &repo_name,
                    name: &ref_name,
                    target: &target,
                    expected,
                    principal: &principal,
                    now_ns,
                },
            )
            .map_err(ApiError::log_internal)
        })
        .await?;

    match outcome {
        RefCas::Created => Ok(Json(RefResponse {
            name,
            target: Some(target.to_hex()),
            version: 1,
        })),
        RefCas::Updated { version } => Ok(Json(RefResponse {
            name,
            target: Some(target.to_hex()),
            version,
        })),
        RefCas::Conflict {
            current_version,
            current_target,
        } => {
            let current_target = current_target
                .map(|bytes| Id(bytes.try_into().expect("ref target is a 32-byte id")).to_hex());
            Err(ApiError::new(
                ApiCode::RefConflict,
                "ref moved since your read: re-fetch and retry with the current version",
            )
            .with_details(serde_json::json!({
                "current_version": current_version,
                "current_target": current_target,
            })))
        }
        RefCas::RefMissing => Err(ApiError::new(
            ApiCode::NotFound,
            format!("no ref named {name:?} in {repo:?}: CAS expected an existing ref"),
        )),
        RefCas::RepoMissing => Err(ApiError::new(
            ApiCode::RepoNotFound,
            format!("no repo named {repo:?} on this server"),
        )),
    }
}
