//! `GET /v1/openapi.json`: the machine-readable API contract (any
//! authenticated principal — a pinned matrix cell). CI regenerates and
//! diffs the committed artifact in M5.

use axum::Extension;
use axum::Json;
use axum::extract::State;
use serde_json::Value;

use crate::auth::AuthedPrincipal;
use crate::openapi::ApiDoc;
use crate::state::AppState;

/// Serves the OpenAPI document — the same [`ApiDoc::document_json`] render
/// the checked `docs/openapi.json` artifact uses, including the error-code
/// registry components.
#[utoipa::path(
    get,
    path = "/v1/openapi.json",
    responses(
        (status = 200, description = "The OpenAPI document"),
        (status = 401, description = "Missing, unknown, revoked, or expired token")
    )
)]
pub async fn serve(
    _state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
) -> Json<Value> {
    Json(ApiDoc::document_json())
}
