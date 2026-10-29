//! The OpenAPI document. Grows with each slice's utoipa-annotated routes;
//! the `/v1/openapi.json` serving route (and its checked-artifact CI diff)
//! lands with auth in M1b2/M5.

use utoipa::OpenApi;

use crate::routes;

/// The naatre API document.
#[derive(OpenApi)]
#[openapi(
    paths(routes::health::healthz, routes::version::version),
    info(title = "naatre")
)]
pub struct ApiDoc;
