//! naatre server: the axum HTTP transport over [`naatre_store`].
//!
//! This slice (M1b1) is the transport sliver: the error envelope and its
//! closed code registry ([`api_error`], [`api_code`]), the
//! spawn_blocking/mutex bridge to the store ([`state`]), request-id
//! middleware and ops logging ([`middleware`]), the anon unversioned routes
//! ([`routes`]), the OpenAPI skeleton ([`openapi`]), and serve-with-drain
//! ([`serve`]). Versioned `/v1/…` routes and auth land in M1b2.
//!
//! Conventions (plan "API conventions"): every error is the same JSON
//! envelope; 5xx responses carry a generic message and a `request_id` only,
//! with specifics kept in server logs under the same id; request logs are
//! `debug`-level, i.e. access logs are off by default.

pub mod api_code;
pub mod api_error;
pub mod middleware;
pub mod openapi;
pub mod routes;
pub mod serve;
pub mod state;

/// Wire protocol version reported by `GET /version` and the startup banner.
/// Bumped only on a breaking protocol change.
pub const PROTOCOL_VERSION: u32 = 1;

/// Request body cap: 96 MiB, headroom over the 64 MiB blob cap so envelope
/// and framing errors fit inside the limit.
pub const BODY_LIMIT_BYTES: usize = 96 * 1024 * 1024;

use axum::extract::DefaultBodyLimit;
use axum::routing::get;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::state::AppState;

/// The application router: routes, body limit, request-id middleware, and a
/// fallback so unknown routes get the same error envelope as everything else.
pub fn router(state: AppState) -> axum::Router {
    let routes: axum::Router<AppState> = axum::Router::new()
        .route("/healthz", get(routes::health::healthz))
        .route("/version", get(routes::version::version))
        .fallback(fallback);
    routes
        .layer(DefaultBodyLimit::max(BODY_LIMIT_BYTES))
        .layer(axum::middleware::from_fn(middleware::request_id))
        .with_state(state)
}

/// Unknown routes answer with the standard envelope, not a bare 404.
async fn fallback() -> ApiError {
    ApiError::new(ApiCode::NotFound, "route not found")
}
