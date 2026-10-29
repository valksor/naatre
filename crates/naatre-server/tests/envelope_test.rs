//! Error-envelope contract: healthz/version shapes, the 404 fallback
//! envelope, the request-id header, and the body limit.

mod common;

use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::extract::{DefaultBodyLimit, Request};
use axum::http::{StatusCode, header};
use axum::routing::post;
use http_body_util::BodyExt;
use naatre_server::api_code::ApiCode;
use naatre_server::api_error::ApiError;
use naatre_server::middleware;
use serde_json::Value;
use tower::ServiceExt;

const ENVELOPE_KEYS: [&str; 5] = ["code", "details", "message", "request_id", "retryable"];

async fn send(app: Router, req: Request<Body>) -> axum::response::Response {
    app.oneshot(req).await.expect("in-memory request")
}

async fn body_json(response: axum::response::Response) -> Value {
    let bytes = BodyExt::collect(response.into_body())
        .await
        .expect("body")
        .to_bytes();
    serde_json::from_slice(&bytes).expect("JSON body")
}

#[tokio::test]
async fn healthz_is_ok_with_request_id() {
    let dir = tempfile::tempdir().unwrap();
    let app = naatre_server::router(common::test_state(dir.path(), Duration::from_secs(30)));

    let response = send(
        app,
        Request::builder()
            .uri("/healthz")
            .body(Body::empty())
            .unwrap(),
    )
    .await;

    assert_eq!(response.status(), StatusCode::OK);
    let request_id = request_id_header(&response);
    assert!(!request_id.is_empty());
    let body = body_json(response).await;
    assert_eq!(body, serde_json::json!({"status": "ok"}));
}

#[tokio::test]
async fn version_has_name_version_protocol() {
    let dir = tempfile::tempdir().unwrap();
    let app = naatre_server::router(common::test_state(dir.path(), Duration::from_secs(30)));

    let response = send(
        app,
        Request::builder()
            .uri("/version")
            .body(Body::empty())
            .unwrap(),
    )
    .await;

    assert_eq!(response.status(), StatusCode::OK);
    let body = body_json(response).await;
    assert_eq!(
        body,
        serde_json::json!({
            "name": "naatre",
            "version": env!("CARGO_PKG_VERSION"),
            "protocol": 1,
        })
    );
}

#[tokio::test]
async fn unknown_route_gets_envelope_404() {
    let dir = tempfile::tempdir().unwrap();
    let app = naatre_server::router(common::test_state(dir.path(), Duration::from_secs(30)));

    let response = send(
        app,
        Request::builder().uri("/nope").body(Body::empty()).unwrap(),
    )
    .await;

    assert_eq!(response.status(), StatusCode::NOT_FOUND);
    let header_id = request_id_header(&response);

    let body = body_json(response).await;
    // Exact envelope shape: {"error": {…five keys…}} and nothing else.
    let top = body.as_object().expect("object body");
    assert_eq!(top.keys().collect::<Vec<_>>(), ["error"]);
    let error = body["error"].as_object().expect("error object");
    let mut keys: Vec<_> = error.keys().map(String::as_str).collect();
    keys.sort_unstable();
    assert_eq!(keys, ENVELOPE_KEYS);

    assert_eq!(body["error"]["code"], "not_found");
    assert_eq!(body["error"]["retryable"], false);
    assert_eq!(body["error"]["details"], serde_json::json!({}));
    let envelope_id = body["error"]["request_id"].as_str().unwrap();
    assert_eq!(envelope_id, header_id, "envelope and header agree");
    assert!(!envelope_id.is_empty() && envelope_id != "unknown");
}

#[tokio::test]
async fn writer_busy_maps_to_503_with_retry_after() {
    let app = Router::new()
        .route(
            "/busy",
            post(|| async { ApiError::new(ApiCode::WriterBusy, "writer busy").retryable(true) }),
        )
        .layer(axum::middleware::from_fn(middleware::request_id));

    let response = send(
        app,
        Request::builder()
            .method("POST")
            .uri("/busy")
            .body(Body::empty())
            .unwrap(),
    )
    .await;

    assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(
        response.headers().get(header::RETRY_AFTER).unwrap(),
        "1",
        "writer_busy responses carry Retry-After: 1"
    );
    let body = body_json(response).await;
    assert_eq!(body["error"]["code"], "writer_busy");
    assert_eq!(body["error"]["retryable"], true);
}

// The 413's envelope mapping lands with the body-accepting routes in M1b2;
// here we only assert the status, via a test-only router with the same
// middleware stack (request-id + DefaultBodyLimit) as the shipped router.
#[tokio::test]
async fn oversized_body_is_rejected_with_413() {
    async fn echo(body: axum::body::Bytes) -> String {
        body.len().to_string()
    }

    let app = Router::new()
        .route("/echo", post(echo))
        .layer(DefaultBodyLimit::max(naatre_server::BODY_LIMIT_BYTES))
        .layer(axum::middleware::from_fn(middleware::request_id));

    let oversized = vec![0u8; 97 * 1024 * 1024]; // 97 MiB > 96 MiB cap
    let response = send(
        app,
        Request::builder()
            .method("POST")
            .uri("/echo")
            .body(Body::from(oversized))
            .unwrap(),
    )
    .await;

    assert_eq!(response.status(), StatusCode::PAYLOAD_TOO_LARGE);
}

// ---- helpers over the response ------------------------------------------

fn request_id_header(response: &axum::response::Response) -> String {
    response
        .headers()
        .get("x-request-id")
        .expect("x-request-id header set by middleware")
        .to_str()
        .expect("request id is ASCII")
        .to_string()
}
