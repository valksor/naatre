//! `/v1/whoami`: the authenticated principal's own identity.

use axum::Extension;
use axum::Json;
use axum::extract::State;
use serde::Serialize;
use utoipa::ToSchema;

use crate::auth::AuthedPrincipal;
use crate::state::AppState;

/// The caller's identity.
#[derive(Serialize, ToSchema)]
pub struct WhoamiResponse {
    pub principal: String,
    pub role: String,
    /// Segment-aligned path scope, or null (whole repo).
    pub scope_path: Option<String>,
}

/// Reports the principal behind the presented token (any authenticated
/// principal). Cannot fail after the auth middleware has passed.
#[utoipa::path(
    get,
    path = "/v1/whoami",
    responses(
        (status = 200, description = "The authenticated principal", body = WhoamiResponse),
        (status = 401, description = "Missing, unknown, revoked, or expired token")
    )
)]
pub async fn whoami(
    _state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
) -> Json<WhoamiResponse> {
    Json(WhoamiResponse {
        principal: auth.principal,
        role: auth.role.as_str().to_string(),
        scope_path: auth.scope_path,
    })
}
