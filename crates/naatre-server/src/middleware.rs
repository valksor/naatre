//! Transport middleware: request ids and the ops logging setup.

use axum::extract::Request;
use axum::http::{HeaderName, HeaderValue};
use axum::middleware::Next;
use axum::response::Response;

use crate::api_error::REQUEST_ID;

const REQUEST_ID_HEADER: HeaderName = HeaderName::from_static("x-request-id");

/// Per-request id carrier, inserted into request extensions for handlers
/// that need to echo it (envelope bodies read the task-local instead).
#[derive(Clone, Debug)]
pub struct RequestId(pub String);

/// Assigns a uuid v4 to every request, exposes it to handlers (extension +
/// task-local) and clients (`x-request-id` response header), and emits the
/// request log.
///
/// Access-log policy: the request line — method, path, status, elapsed_ms,
/// request_id — is a `tracing::debug!` event, so it is silent at the
/// default `info` level. Access logs are off by default; operators opt in
/// per environment with e.g. `NAATRE_LOG=debug` (`--log-level`). No peer
/// address is recorded.
pub async fn request_id(req: Request, next: Next) -> Response {
    let id = uuid::Uuid::new_v4().to_string();
    let method = req.method().clone();
    let path = req.uri().path().to_string();
    let started = std::time::Instant::now();

    let mut req = req;
    req.extensions_mut().insert(RequestId(id.clone()));
    let response = REQUEST_ID.scope(id.clone(), next.run(req)).await;

    let elapsed_ms = u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX);
    tracing::debug!(
        method = %method,
        path = %path,
        status = response.status().as_u16(),
        elapsed_ms,
        request_id = %id,
        "request"
    );

    let mut response = response;
    if let Ok(value) = HeaderValue::from_str(&id) {
        response.headers_mut().insert(REQUEST_ID_HEADER, value);
    }
    response
}

/// Installs the process-wide tracing subscriber: `fmt` output filtered by
/// `level` (an invalid directive falls back to `info`). Call once at
/// startup, before the first log-emitting call.
///
/// Logging rule: `Authorization` headers and token material are never
/// logged — nothing in this crate emits header or credential values, and
/// new log sites must keep it that way.
pub fn logging_setup(level: &str) {
    let filter = tracing_subscriber::EnvFilter::try_new(level)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(filter).init();
}
