//! `/v1/principals/{name}`: the admin principal-deletion surface (M10b) —
//! the erasure end-state: principal + token rows go, the audit ACTOR is
//! pseudonymized, history attribution survives (the honest whole-changeset
//! rule the README states).

use axum::extract::{Path, State};
use axum::{Extension, Json};
use serde::Serialize;
use utoipa::ToSchema;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db;
use crate::state::AppState;

/// Deletion result.
#[derive(Serialize, ToSchema)]
pub struct PrincipalDeleted {
    pub principal: String,
    /// Token rows deleted with the principal (revoked or not — deletion is
    /// stronger than revocation).
    pub tokens_deleted: u64,
}

/// Deletes a principal (admin, audited `principal_delete`): its token rows
/// and principals row are deleted, and every `audit_log` row it acted on is
/// pseudonymized to `deleted:<name>` (the log is append-only on a
/// security-accountability basis — rows are rewritten in the actor column
/// only; `details_json` payloads are history and stay). History attribution
/// (`changesets.principal`, `refs.last_principal`, `lands.*`,
/// `snapshots.principal`) is left as-is: attribution survives unless the
/// changesets are themselves tombstoned. Unknown principal → 404.
#[utoipa::path(
    delete,
    path = "/v1/principals/{name}",
    params(("name" = String, Path, description = "Principal name")),
    responses(
        (status = 200, description = "Principal deleted", body = PrincipalDeleted),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No principal with that name")
    )
)]
pub async fn delete(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(name): Path<String>,
) -> Result<Json<PrincipalDeleted>, ApiError> {
    require_role(&auth, Role::Admin)?;
    if name.is_empty() {
        return Err(ApiError::new(
            ApiCode::Validation,
            "principal name must not be empty",
        ));
    }
    let now_ns = state.now_ns() as i64;
    let actor = auth.principal.clone();
    let target = name.clone();
    let deleted = state
        .write(move |store| {
            db::principal_delete(store.conn(), &target, now_ns, &actor)
                .map_err(ApiError::log_internal)
        })
        .await?;
    match deleted {
        Some(tokens) => Ok(Json(PrincipalDeleted {
            principal: name,
            tokens_deleted: tokens as u64,
        })),
        None => Err(ApiError::new(
            ApiCode::NotFound,
            format!("no principal named {name:?}"),
        )),
    }
}
