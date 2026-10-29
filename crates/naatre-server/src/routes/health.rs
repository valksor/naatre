//! `GET /healthz` — anon liveness probe (no store access).

use axum::Json;
use serde::Serialize;
use utoipa::ToSchema;

/// Liveness response body.
#[derive(Serialize, ToSchema)]
pub struct HealthResponse {
    pub status: String,
}

/// Report server liveness.
#[utoipa::path(
    get,
    path = "/healthz",
    responses((status = 200, description = "Server is healthy", body = HealthResponse))
)]
pub async fn healthz() -> Json<HealthResponse> {
    Json(HealthResponse {
        status: "ok".to_string(),
    })
}
