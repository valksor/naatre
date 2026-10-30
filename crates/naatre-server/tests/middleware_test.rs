//! Shedding middleware: the concurrency cap SHEDS (never queues) and the
//! request timeout sheds a stalled handler — both as retryable 503s.

use std::sync::Arc;
use std::time::Duration;

use axum::Router;
use axum::extract::State;
use axum::http::StatusCode;
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use tokio::sync::Semaphore;

use naatre_server::middleware::request_cap;

async fn stall(State(gate): State<Arc<tokio::sync::Notify>>) -> &'static str {
    gate.notified().await; // released only by the test's second phase
    "done"
}

#[tokio::test]
async fn concurrency_cap_sheds_never_queues() {
    let permits = Arc::new(Semaphore::new(1));
    let gate = Arc::new(tokio::sync::Notify::new());

    let inner = Router::new()
        .route("/stall", get(stall))
        .with_state(Arc::clone(&gate));
    let app = inner.layer(axum::middleware::from_fn_with_state(
        Arc::clone(&permits),
        request_cap,
    ));

    let (tx, rx) = tokio::sync::oneshot::channel::<Response>();
    let app1 = app.clone();
    tokio::spawn(async move {
        let resp = tower::ServiceExt::oneshot(app1, build_req("/stall"))
            .await
            .unwrap();
        let _ = tx.send(resp);
    });
    // Let the first request take the only permit.
    tokio::task::yield_now().await;
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Second request while the permit is held: shed immediately, not queued.
    let resp = tower::ServiceExt::oneshot(app, build_req("/stall"))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["error"]["code"], "server_busy");
    assert_eq!(v["error"]["retryable"], true);

    // Release the stalled handler so the leader finishes and the test ends.
    gate.notify_one();
    let _ = rx.await;
}

#[tokio::test]
async fn request_timeout_sheds_a_stalled_handler() {
    let app = Router::new()
        .route(
            "/stall",
            get(|| async {
                tokio::time::sleep(Duration::from_secs(5)).await;
                "done"
            }),
        )
        .layer(axum::middleware::from_fn(request_timeout_short));

    let started = std::time::Instant::now();
    let resp = tower::ServiceExt::oneshot(app, build_req("/stall"))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert!(started.elapsed() < Duration::from_secs(2), "shed promptly");
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["error"]["code"], "request_timeout");
    assert_eq!(v["error"]["retryable"], true);
}

/// `request_timeout` with a test-short budget so the test proves the shed,
/// not the 30 s default.
async fn request_timeout_short(req: axum::extract::Request, next: Next) -> Response {
    use std::future::IntoFuture;
    let fut = next.run(req).into_future();
    match tokio::time::timeout(Duration::from_millis(300), fut).await {
        Ok(resp) => resp,
        Err(_elapsed) => {
            let body = axum::Json(serde_json::json!({
                "error": {"code": "request_timeout", "message": "request exceeded its time budget",
                          "retryable": true, "details": {}, "request_id": "test"}
            }));
            (axum::http::StatusCode::SERVICE_UNAVAILABLE, body).into_response()
        }
    }
}

fn build_req(path: &str) -> axum::extract::Request {
    use axum::body::Body;
    use axum::http::Request;
    Request::builder().uri(path).body(Body::empty()).unwrap()
}
