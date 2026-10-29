//! `GET /v1/openapi.json`: the machine-readable API contract (any
//! authenticated principal — a pinned matrix cell). CI regenerates and
//! diffs the committed artifact in M5.

use axum::Extension;
use axum::Json;
use axum::extract::State;
use serde_json::Value;

use utoipa::OpenApi as _;

use crate::auth::AuthedPrincipal;
use crate::openapi::ApiDoc;
use crate::state::AppState;

/// Serves the utoipa OpenAPI document.
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
    Json(serde_json::to_value(ApiDoc::openapi()).expect("ApiDoc serializes to JSON"))
}
