//! naatre server: the axum HTTP transport over [`naatre_store`].
//!
//! M1b1 laid the transport sliver: the error envelope and its closed code
//! registry ([`api_error`], [`api_code`]), the spawn_blocking/mutex bridge
//! to the store ([`state`]), request-id middleware and ops logging
//! ([`middleware`]), the anon unversioned routes ([`routes`]), the OpenAPI
//! skeleton ([`openapi`]), and serve-with-drain ([`serve`]).
//!
//! M1b2 completes M1b: bearer-token auth ([`auth`]), the segment-aligned
//! scope matcher ([`authz`]), all SQL in one module ([`db`]), envelope-safe
//! body extraction ([`extract`]), and the versioned `/v1/…` routes —
//! repos, refs, objects, tokens, whoami, audit, openapi — wired into
//! [`router`] behind the auth middleware with the route→role matrix.
//!
//! Conventions (plan "API conventions"): every error is the same JSON
//! envelope; 5xx responses carry a generic message and a `request_id` only,
//! with specifics kept in server logs under the same id; request logs are
//! `debug`-level, i.e. access logs are off by default.

pub mod adopt;
pub mod api_code;
pub mod api_error;
pub mod auth;
pub mod authz;
pub mod backup;
pub mod cache_tier;
pub mod db;
pub mod derive;
pub mod derived;
pub mod extract;
pub mod fsck;
pub mod gc;
pub mod import;
pub mod land;
pub mod land_join;
pub mod merge;
pub mod middleware;
pub mod openapi;
pub mod router;
pub mod routes;
pub mod serve;
pub mod snapshots_gc;
pub mod state;
pub mod ticket;
pub mod tree_derive;

/// Wire protocol version reported by `GET /version` and the startup banner.
/// Bumped only on a breaking protocol change.
pub const PROTOCOL_VERSION: u32 = 1;

/// Request body cap: 96 MiB, headroom over the 64 MiB blob cap so envelope
/// and framing errors fit inside the limit.
pub const BODY_LIMIT_BYTES: usize = 96 * 1024 * 1024;

use std::sync::Arc;

use axum::extract::DefaultBodyLimit;
use axum::routing::{delete, get, post};

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::state::AppState;

/// The application router: the unversioned anon routes (`/healthz`,
/// `/version`), the authenticated `/v1/…` API behind the auth middleware
/// (each handler enforces its own role from the route→role matrix), the
/// body limit, request-id middleware, and a fallback so unknown routes get
/// the same error envelope as everything else.
pub fn router(state: AppState) -> axum::Router {
    let v1: axum::Router<AppState> = axum::Router::new()
        .route(
            "/v1/repos",
            get(routes::repos::list).post(routes::repos::create),
        )
        .route("/v1/repos/{r}/stats", get(routes::repos::stats))
        .route(
            "/v1/repos/{r}/access",
            get(routes::access::list).put(routes::access::set),
        )
        .route(
            "/v1/repos/{r}/access/{principal}",
            delete(routes::access::revoke),
        )
        .route("/v1/repos/{r}/tickets", post(routes::tickets::mint))
        .route(
            "/v1/repos/{r}/cache/membership",
            get(routes::tickets::membership),
        )
        .route("/v1/repos/{r}/cache/epoch", get(routes::tickets::epoch))
        .route("/v1/repos/{r}/changesets", post(routes::changesets::create))
        .route(
            "/v1/repos/{r}/changesets/{id}",
            get(routes::changesets::get_changeset),
        )
        .route(
            "/v1/repos/{r}/changesets/{id}/root",
            get(routes::changesets::get_root),
        )
        .route("/v1/repos/{r}/trees/{id}", get(routes::trees::get_tree))
        .route("/v1/repos/{r}/log", get(routes::history::log))
        .route("/v1/repos/{r}/blame", get(routes::history::blame))
        .route("/v1/repos/{r}/diff", get(routes::history::diff))
        .route("/v1/repos/{r}/merge-base", get(routes::history::merge_base))
        .route("/v1/repos/{r}/file", get(routes::history::file))
        .route("/v1/repos/{r}/snapshots", post(routes::snapshots::create))
        .route(
            "/v1/repos/{r}/snapshots/{id}",
            get(routes::snapshots::get_metadata),
        )
        .route(
            "/v1/repos/{r}/snapshots/{id}/objects/{oid}",
            get(routes::snapshots::get_blob).put(routes::snapshots::put_blob),
        )
        .route(
            "/v1/repos/{r}/snapshots/{id}/trees/{tid}",
            get(routes::snapshots::get_snapshot_tree),
        )
        .route("/v1/repos/{r}/land", post(routes::land::land))
        .route("/v1/repos/{r}/land/{key}", get(routes::land::land_status))
        .route(
            "/v1/repos/{r}/refs/{name}",
            get(routes::refs::get_ref).post(routes::refs::cas_ref),
        )
        .route(
            "/v1/repos/{r}/objects/present",
            post(routes::objects::present),
        )
        .route("/v1/repos/{r}/objects", post(routes::objects::create_batch))
        .route(
            "/v1/repos/{r}/objects/get",
            post(routes::objects::get_batch),
        )
        .route(
            "/v1/repos/{r}/objects/{id}",
            get(routes::objects::get_raw).put(routes::objects::put_raw),
        )
        .route(
            "/v1/tokens",
            get(routes::tokens::list).post(routes::tokens::create),
        )
        .route("/v1/tokens/{id}", delete(routes::tokens::revoke))
        .route("/v1/principals/{name}", delete(routes::principals::delete))
        .route(
            "/v1/repos/{r}/redactions",
            get(routes::redactions::list).post(routes::redactions::create),
        )
        .route("/v1/whoami", get(routes::whoami::whoami))
        .route("/v1/audit", get(routes::audit::list))
        .route("/v1/openapi.json", get(routes::openapi::serve))
        .route_layer(axum::middleware::from_fn_with_state(
            state.clone(),
            auth::authenticate,
        ));

    // Transport guards, outermost last: request ids (so shed responses
    // still carry one), the 30 s per-request timeout, the 256-permit
    // concurrency cap, then the body limit (see `middleware`).
    let request_permits = Arc::new(tokio::sync::Semaphore::new(
        middleware::MAX_IN_FLIGHT_REQUESTS,
    ));
    axum::Router::new()
        .route("/healthz", get(routes::health::healthz))
        .route("/version", get(routes::version::version))
        .merge(v1)
        .fallback(fallback)
        .layer(DefaultBodyLimit::max(BODY_LIMIT_BYTES))
        .layer(axum::middleware::from_fn_with_state(
            request_permits,
            middleware::request_cap,
        ))
        .layer(axum::middleware::from_fn_with_state(
            middleware::TimeoutBudget(middleware::REQUEST_TIMEOUT),
            middleware::request_timeout,
        ))
        .layer(axum::middleware::from_fn(middleware::request_id))
        .with_state(state)
}

/// Unknown routes answer with the standard envelope, not a bare 404.
async fn fallback() -> ApiError {
    ApiError::new(ApiCode::NotFound, "route not found")
}
