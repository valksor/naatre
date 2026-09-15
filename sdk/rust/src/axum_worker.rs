use crate::server::valid_identifier;
use crate::{
    CancelRequest, Registration, ServerError, WORKER_PROTOCOL, WorkerCore, WorkerInvocation,
    decode_worker_frame, encode_worker_frame,
};
use axum::Router;
use axum::body::{Body, Bytes};
use axum::extract::rejection::BytesRejection;
use axum::extract::{DefaultBodyLimit, State};
use axum::http::header::{ACCEPT, CONTENT_TYPE};
use axum::http::{HeaderMap, HeaderName, HeaderValue, Method, StatusCode};
use axum::response::Response;
use axum::routing::any;
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use std::error::Error;
use std::fmt::{self, Display, Formatter};
use std::future::Future;
use std::sync::Arc;
use tokio::net::TcpListener;

pub const AXUM_WORKER_MEDIA_TYPE: &str = "application/naatre-worker+json";
pub const AXUM_WORKER_PROTOCOL_HEADER: &str = "naatre-worker-protocol";
pub const AXUM_REGISTER_PATH: &str = "/naatre.remote-worker.v1.Worker/Register";
pub const AXUM_INVOKE_PATH: &str = "/naatre.remote-worker.v1.Worker/Invoke";
pub const AXUM_CANCEL_PATH: &str = "/naatre.remote-worker.v1.Worker/Cancel";

const MAXIMUM_FRAME_BYTES: usize = 64 << 20;
const FRAME_HEADER_BYTES: usize = 5;

/// Axum adapter configuration owned by the embedding application.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AxumWorkerConfig {
    pub session_id: String,
    pub maximum_frame_bytes: usize,
}

/// Stable, redacted Axum adapter construction failure.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AxumWorkerError {
    code: &'static str,
    message: &'static str,
}

impl AxumWorkerError {
    #[must_use]
    pub const fn code(&self) -> &'static str {
        self.code
    }
}

impl Display for AxumWorkerError {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl Error for AxumWorkerError {}

#[derive(Clone)]
struct AxumState {
    worker: Arc<WorkerCore>,
    session_id: Arc<str>,
    maximum_frame_bytes: usize,
}

/// Cloneable Axum integration for the executor-neutral Rust worker core.
///
/// `router` performs no bind and starts no background task. `serve` accepts an
/// application-bound Tokio listener and an application-owned shutdown future.
/// Dropping an in-flight route future drops the core invocation future, which
/// requests cooperative cancellation and releases request-local ownership.
#[derive(Clone)]
pub struct AxumWorker {
    state: AxumState,
}

impl AxumWorker {
    /// Builds an adapter with one finite strict-frame limit.
    ///
    /// # Errors
    ///
    /// Returns `WORKER_CONFIG_INVALID` for an invalid session identity or a
    /// zero/oversized frame limit.
    pub fn new(worker: Arc<WorkerCore>, config: AxumWorkerConfig) -> Result<Self, AxumWorkerError> {
        if !valid_identifier(&config.session_id)
            || !(1..=MAXIMUM_FRAME_BYTES).contains(&config.maximum_frame_bytes)
        {
            return Err(AxumWorkerError {
                code: "WORKER_CONFIG_INVALID",
                message: "Axum worker configuration is invalid",
            });
        }
        Ok(Self {
            state: AxumState {
                worker,
                session_id: Arc::from(config.session_id),
                maximum_frame_bytes: config.maximum_frame_bytes,
            },
        })
    }

    /// Returns an in-memory-testable router for the three normative unary
    /// remote-worker RPC paths.
    #[must_use]
    pub fn router(&self) -> Router {
        Router::new()
            .route(AXUM_REGISTER_PATH, any(register))
            .route(AXUM_INVOKE_PATH, any(invoke))
            .route(AXUM_CANCEL_PATH, any(cancel))
            .fallback(route_not_found)
            .layer(DefaultBodyLimit::max(
                self.state.maximum_frame_bytes + FRAME_HEADER_BYTES,
            ))
            .with_state(self.state.clone())
    }

    /// Serves the router on a caller-bound Tokio listener until the
    /// caller-owned shutdown future completes. Axum drains admitted requests
    /// during graceful shutdown.
    ///
    /// # Errors
    ///
    /// Returns the listener/server I/O error to the embedding application. I/O
    /// details are never serialized into a worker response.
    pub async fn serve<F>(self, listener: TcpListener, shutdown: F) -> std::io::Result<()>
    where
        F: Future<Output = ()> + Send + 'static,
    {
        axum::serve(listener, self.router())
            .with_graceful_shutdown(shutdown)
            .await
    }
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Envelope<T> {
    protocol: String,
    kind: String,
    payload: T,
}

#[derive(Serialize)]
struct FailureBody {
    code: &'static str,
}

async fn register(
    State(state): State<AxumState>,
    method: Method,
    headers: HeaderMap,
    body: Result<Bytes, BytesRejection>,
) -> Response {
    let registration = match request::<Registration>(&state, &method, &headers, body, "register") {
        Ok(registration) => registration,
        Err(response) => return response,
    };
    if &registration != state.worker.registration() {
        return failure("REMOTE_REGISTRATION_INVALID");
    }
    success(
        &state,
        "registered",
        state.worker.registration_ack(state.session_id.as_ref()),
    )
}

async fn invoke(
    State(state): State<AxumState>,
    method: Method,
    headers: HeaderMap,
    body: Result<Bytes, BytesRejection>,
) -> Response {
    let invocation = match request::<WorkerInvocation>(&state, &method, &headers, body, "invoke") {
        Ok(invocation) => invocation,
        Err(response) => return response,
    };
    match state.worker.invoke(invocation) {
        Ok(future) => match future.await {
            Ok(result) => success(&state, "result", result),
            Err(error) => server_failure(&error),
        },
        Err(error) => server_failure(&error),
    }
}

async fn cancel(
    State(state): State<AxumState>,
    method: Method,
    headers: HeaderMap,
    body: Result<Bytes, BytesRejection>,
) -> Response {
    let cancellation = match request::<CancelRequest>(&state, &method, &headers, body, "cancel") {
        Ok(cancellation) => cancellation,
        Err(response) => return response,
    };
    match state.worker.cancel(&cancellation) {
        Ok(ack) => success(&state, "cancelled", ack),
        Err(error) => server_failure(&error),
    }
}

async fn route_not_found() -> Response {
    failure("WORKER_ROUTE_NOT_FOUND")
}

fn request<T: DeserializeOwned>(
    state: &AxumState,
    method: &Method,
    headers: &HeaderMap,
    body: Result<Bytes, BytesRejection>,
    kind: &str,
) -> Result<T, Response> {
    if method != Method::POST || !negotiated(headers) {
        return Err(failure("REMOTE_WORKER_MALFORMED"));
    }
    let body = body.map_err(|_| failure("REMOTE_WORKER_MALFORMED"))?;
    let envelope = decode_worker_frame::<Envelope<T>>(&body, state.maximum_frame_bytes)
        .map_err(|_| failure("REMOTE_WORKER_MALFORMED"))?;
    if envelope.protocol != WORKER_PROTOCOL || envelope.kind != kind {
        return Err(failure("REMOTE_WORKER_MALFORMED"));
    }
    Ok(envelope.payload)
}

fn negotiated(headers: &HeaderMap) -> bool {
    headers
        .get(CONTENT_TYPE)
        .is_some_and(|value| value == AXUM_WORKER_MEDIA_TYPE)
        && headers
            .get(ACCEPT)
            .is_some_and(|value| value == AXUM_WORKER_MEDIA_TYPE)
        && headers
            .get(AXUM_WORKER_PROTOCOL_HEADER)
            .is_some_and(|value| value == WORKER_PROTOCOL)
}

fn success<T: Serialize>(state: &AxumState, kind: &str, payload: T) -> Response {
    let envelope = Envelope {
        protocol: WORKER_PROTOCOL.to_owned(),
        kind: kind.to_owned(),
        payload,
    };
    match encode_worker_frame(&envelope, state.maximum_frame_bytes) {
        Ok(frame) => response(StatusCode::OK, frame),
        Err(_) => failure("OUTPUT_COMPLETION"),
    }
}

fn server_failure(error: &ServerError) -> Response {
    failure(error.code())
}

fn failure(code: &'static str) -> Response {
    let status = match code {
        "REMOTE_HANDLER_UNKNOWN" | "WORKER_ROUTE_NOT_FOUND" => StatusCode::NOT_FOUND,
        "REMOTE_UNAUTHENTICATED" => StatusCode::UNAUTHORIZED,
        "UNAUTHORIZED" => StatusCode::FORBIDDEN,
        "OVERLOADED" => StatusCode::TOO_MANY_REQUESTS,
        "INTERNAL" | "OUTPUT_COMPLETION" => StatusCode::INTERNAL_SERVER_ERROR,
        _ => StatusCode::BAD_REQUEST,
    };
    let body = serde_json::to_vec(&FailureBody { code })
        .unwrap_or_else(|_| br#"{"code":"INTERNAL"}"#.to_vec());
    response(status, body)
}

fn response(status: StatusCode, body: Vec<u8>) -> Response {
    let mut response = Response::new(Body::from(body));
    *response.status_mut() = status;
    response.headers_mut().insert(
        CONTENT_TYPE,
        HeaderValue::from_static(AXUM_WORKER_MEDIA_TYPE),
    );
    response.headers_mut().insert(
        HeaderName::from_static(AXUM_WORKER_PROTOCOL_HEADER),
        HeaderValue::from_static(WORKER_PROTOCOL),
    );
    response
}
