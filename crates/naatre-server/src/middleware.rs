//! Transport middleware: request ids, the per-request time budget, the
//! concurrent-request cap, and the ops logging setup.
//!
//! The two shedding guards are deliberately dependency-free (no
//! `tower-http`): [`request_timeout`] wraps the inner service in
//! [`tokio::time::timeout`], and [`request_cap`] admits requests through a
//! shared [`tokio::sync::Semaphore`]. Both answer with the standard error
//! envelope — 503, retryable, `Retry-After: 1` — so agents apply the same
//! backoff as for `writer_busy`.

use std::sync::Arc;
use std::time::Duration;

use axum::extract::{Request, State};
use axum::http::{HeaderName, HeaderValue, header};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use tokio::sync::{Semaphore, TryAcquireError};

use crate::api_code::ApiCode;
use crate::api_error::{ApiError, REQUEST_ID};

const REQUEST_ID_HEADER: HeaderName = HeaderName::from_static("x-request-id");

/// Per-request time budget: a request that outlives it is shed with 503
/// `request_timeout` (its inner future is dropped, so in-flight lands fail
/// fast for their joiners via the join guard's Drop fallback).
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);

/// The per-request budget as middleware state, so tests drive the REAL
/// [`request_timeout`] with a tiny budget; the binary installs
/// `TimeoutBudget(REQUEST_TIMEOUT)` (the 30 s default).
#[derive(Clone, Copy, Debug)]
pub struct TimeoutBudget(pub Duration);

/// Concurrent-request cap: axum's `serve` has no connection limit of its
/// own, so this semaphore is the process's admission control. A request
/// that would exceed it is shed before any work starts with 503
/// `server_busy`.
pub const MAX_IN_FLIGHT_REQUESTS: usize = 256;

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

/// The shedding envelope for the transport guards: 503 with a registry
/// code, `retryable: true`, and `Retry-After: 1`.
fn shed(code: ApiCode, message: &'static str) -> Response {
    let mut response = ApiError::new(code, message).retryable(true).into_response();
    response
        .headers_mut()
        .insert(header::RETRY_AFTER, HeaderValue::from_static("1"));
    response
}

/// Wraps every request in the state's [`TimeoutBudget`]: a request that
/// outlives the budget is answered 503 `request_timeout` and its inner
/// future dropped. Install with `from_fn_with_state(TimeoutBudget(
/// REQUEST_TIMEOUT), request_timeout)` — the 30 s default; tests install a
/// tiny budget through the same function.
pub async fn request_timeout(
    State(TimeoutBudget(budget)): State<TimeoutBudget>,
    req: Request,
    next: Next,
) -> Response {
    match tokio::time::timeout(budget, next.run(req)).await {
        Ok(response) => response,
        Err(_) => {
            tracing::warn!(
                timeout_secs = budget.as_secs(),
                "request shed: per-request timeout"
            );
            shed(
                ApiCode::RequestTimeout,
                "request exceeded the server time budget",
            )
        }
    }
}

/// Admits every request through the shared [`MAX_IN_FLIGHT_REQUESTS`]
/// semaphore; beyond the cap, requests are answered 503 `server_busy`
/// without starting any work. The permit releases when this future drops.
pub async fn request_cap(
    State(permits): State<Arc<Semaphore>>,
    req: Request,
    next: Next,
) -> Response {
    // try_acquire, never wait: the cap sheds rather than queues (a queued
    // request would pile up behind the very load the cap exists to bound).
    // The two failure modes are distinct facts for ops (the closed
    // semaphore means shutdown, not load); the shed response is the same
    // either way.
    match permits.clone().try_acquire_owned() {
        Ok(permit) => {
            let _permit = permit;
            next.run(req).await
        }
        Err(TryAcquireError::NoPermits) => {
            tracing::warn!("request shed: concurrency cap reached");
            shed(ApiCode::ServerBusy, "server is at its request limit")
        }
        Err(TryAcquireError::Closed) => {
            tracing::warn!("request shed: request semaphore is closed");
            shed(ApiCode::ServerBusy, "server is at its request limit")
        }
    }
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
