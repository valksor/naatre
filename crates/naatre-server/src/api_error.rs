//! The error envelope: one JSON shape for every API error response.

use axum::Json;
use axum::http::{HeaderValue, header};
use axum::response::{IntoResponse, Response};
use serde_json::json;

use crate::api_code::ApiCode;

// The request id lives in a task-local because `IntoResponse` has no access
// to request extensions: the middleware runs the inner service inside the
// scope, so handlers (and `into_response`) see the id that belongs to their
// request. Outside the middleware there is no id — callers get "unknown".
tokio::task_local! {
    pub(crate) static REQUEST_ID: String;
}

/// The request id in effect for the current task, or `"unknown"` when no
/// request-id middleware is running (e.g. errors outside the middleware).
pub(crate) fn current_request_id() -> String {
    REQUEST_ID
        .try_with(|id| id.clone())
        .unwrap_or_else(|_| "unknown".to_string())
}

/// An API error: a registry code, a human message, a retryability hint, and
/// structured `details`. Converts into the wire envelope:
///
/// ```json
/// {"error":{"code":"…","message":"…","retryable":bool,"details":{…},"request_id":"…"}}
/// ```
///
/// 5xx policy: internal failures never reach the client — construct
/// [`ApiError::internal`], whose message is generic and whose details are
/// empty; the specifics stay in server logs keyed by `request_id`.
#[derive(Debug, Clone, thiserror::Error)]
#[error("api error: {code:?}: {message}")]
pub struct ApiError {
    code: ApiCode,
    message: String,
    retryable: bool,
    details: serde_json::Value,
}

impl ApiError {
    /// A new error with empty `details` and `retryable == false`.
    pub fn new(code: ApiCode, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            retryable: false,
            details: json!({}),
        }
    }

    /// The 5xx-safe error: generic message, empty details. Never attach
    /// internal information to it.
    pub fn internal() -> Self {
        Self::new(ApiCode::Internal, "internal error")
    }

    /// Logs `cause` (server logs only — it never reaches the client) and
    /// returns [`ApiError::internal`]. The standard mapping for unexpected
    /// store/IO failures in handlers.
    pub fn log_internal(cause: impl std::fmt::Display) -> Self {
        tracing::error!(error = %cause, "internal error");
        Self::internal()
    }

    /// Marks the error as retryable (clients back off per the API docs).
    pub fn retryable(mut self, retryable: bool) -> Self {
        self.retryable = retryable;
        self
    }

    /// Attaches structured recovery data (e.g. `ref_conflict` carries the
    /// current version + target). `details` must be a JSON object.
    pub fn with_details(mut self, details: serde_json::Value) -> Self {
        self.details = details;
        self
    }

    /// The registry code, for callers that branch on it (tests, mapping).
    pub fn code(&self) -> ApiCode {
        self.code
    }

    /// Whether the envelope invited a retry (the `retryable` wire field).
    pub fn is_retryable(&self) -> bool {
        self.retryable
    }

    /// The structured recovery details, for callers that parse them (the
    /// land-join fallback reads `settled_head` out of a leader's 409).
    pub fn details(&self) -> &serde_json::Value {
        &self.details
    }

    /// The wire envelope for this error, tagged with `request_id`.
    ///
    /// `details` always serializes as an object: it can only be set through
    /// the constructors, which default it to `{}`.
    pub fn body(&self, request_id: &str) -> serde_json::Value {
        json!({
            "error": {
                "code": self.code.as_str(),
                "message": self.message,
                "retryable": self.retryable,
                "details": self.details,
                "request_id": request_id,
            }
        })
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let request_id = current_request_id();
        let status = self.code.status();
        let mut response = (status, Json(self.body(&request_id))).into_response();
        if self.code == ApiCode::WriterBusy || self.code == ApiCode::DerivationPending {
            // Backpressure hint: the single-writer slot, and the
            // read-during-backlog contract's derived reads (M6). The
            // README pins `Retry-After: 1` for every retryable 503.
            response
                .headers_mut()
                .insert(header::RETRY_AFTER, HeaderValue::from_static("1"));
        }
        response
    }
}
