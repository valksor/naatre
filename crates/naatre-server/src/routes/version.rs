//! `GET /version` — anon version report (no store access).

use axum::Json;
use serde::Serialize;
use utoipa::ToSchema;

use crate::PROTOCOL_VERSION;

/// Version response body: server name, crate version, wire protocol version.
#[derive(Serialize, ToSchema)]
pub struct VersionResponse {
    pub name: String,
    pub version: String,
    pub protocol: u32,
}

/// Report server and protocol versions (clients warn on version skew).
#[utoipa::path(
    get,
    path = "/version",
    responses((status = 200, description = "Server and protocol versions", body = VersionResponse))
)]
pub async fn version() -> Json<VersionResponse> {
    Json(VersionResponse {
        // The product name, not the crate name (`naatre-server`).
        name: "naatre".to_string(),
        version: env!("CARGO_PKG_VERSION").to_string(),
        protocol: PROTOCOL_VERSION,
    })
}
