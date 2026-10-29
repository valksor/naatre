//! Extractors that keep the error envelope contract: axum's stock `Json`
//! rejection answers `text/plain`, so request bodies go through
//! [`AppJson`], which maps every rejection onto the standard envelope.

use axum::extract::rejection::JsonRejection;
use axum::extract::{FromRequest, Request};
use serde::de::DeserializeOwned;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;

/// JSON body extractor that fails into the standard error envelope.
///
/// Rejection mapping: syntactic breakage (bad JSON, missing content type,
/// unreadable bytes) → 400 `malformed`; a valid-JSON-but-wrong-shape body →
/// 422 `validation`. The rejection's own text is deliberately dropped —
/// serializer details stay in server logs, not in client responses.
pub struct AppJson<T>(pub T);

impl<S, T> FromRequest<S> for AppJson<T>
where
    S: Send + Sync,
    T: DeserializeOwned,
{
    type Rejection = ApiError;

    async fn from_request(req: Request, state: &S) -> Result<Self, Self::Rejection> {
        match axum::Json::<T>::from_request(req, state).await {
            Ok(axum::Json(value)) => Ok(AppJson(value)),
            Err(rejection) => Err(map_rejection(rejection)),
        }
    }
}

fn map_rejection(rejection: JsonRejection) -> ApiError {
    tracing::debug!(rejection = %rejection, "request body rejected");
    match rejection {
        JsonRejection::JsonDataError(_) => ApiError::new(
            ApiCode::Validation,
            "request body does not match the expected schema",
        ),
        JsonRejection::JsonSyntaxError(_)
        | JsonRejection::MissingJsonContentType(_)
        | JsonRejection::BytesRejection(_) => {
            ApiError::new(ApiCode::Malformed, "request body is not readable JSON")
        }
        _ => ApiError::new(ApiCode::Malformed, "request body is not readable JSON"),
    }
}
