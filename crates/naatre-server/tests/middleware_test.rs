//! Shedding middleware: the concurrency cap SHEDS (never queues) and the
//! request timeout sheds a stalled handler — both as retryable 503s with
//! Retry-After: 1. Both tests drive the REAL middleware (the timeout
//! budget is injectable state), and the cap test's leader-held state is
//! signaled deterministically — no yield/sleep race.

use std::sync::Arc;
use std::time::Duration;

use axum::Router;
use axum::extract::State;
use axum::http::{StatusCode, header};
use axum::response::Response;
use axum::routing::get;
use tokio::sync::Semaphore;

use naatre_server::middleware::{TimeoutBudget, request_cap, request_timeout};

/// The stalled leader's two signals. `started` fires (via notify_one, whose
/// permit is stored even when the test has not awaited yet) when the
/// handler runs — and request_cap only dispatches the handler after
/// acquiring its permit, so `started` PROVES the cap is held. `release`
/// parks the handler until the test is done with the held permit.
#[derive(Default)]
struct Gates {
    started: tokio::sync::Notify,
    release: tokio::sync::Notify,
}

async fn stall(State(gates): State<Arc<Gates>>) -> &'static str {
    gates.started.notify_one();
    gates.release.notified().await; // released only by the test's last phase
    "done"
}

#[tokio::test]
async fn concurrency_cap_sheds_never_queues() {
    let permits = Arc::new(Semaphore::new(1));
    let gates = Arc::new(Gates::default());

    let inner = Router::new()
        .route("/stall", get(stall))
        .with_state(Arc::clone(&gates));
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
    // Deterministic leader-held signal: the handler runs only after
    // request_cap has taken the only permit.
    gates.started.notified().await;

    // Second request while the permit is held: shed immediately, not queued.
    let resp = tower::ServiceExt::oneshot(app, build_req("/stall"))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(
        resp.headers()
            .get(header::RETRY_AFTER)
            .map(|v| v.to_str().unwrap()),
        Some("1"),
        "the shed carries Retry-After: 1"
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["error"]["code"], "server_busy");
    assert_eq!(v["error"]["retryable"], true);

    // Release the stalled handler so the leader finishes and the test ends.
    gates.release.notify_one();
    let _ = rx.await;
}

#[tokio::test]
async fn closed_semaphore_sheds_server_busy_deterministically() {
    // The Closed arm (shutdown, not load): a closed semaphore answers
    // try_acquire with Closed regardless of free permits, so closing it is
    // a deterministic trigger — no leader, no held permit, no timing.
    let permits = Arc::new(Semaphore::new(8));
    permits.close();

    let app = Router::new()
        .route("/stall", get(|| async { "done" }))
        .layer(axum::middleware::from_fn_with_state(permits, request_cap));

    let resp = tower::ServiceExt::oneshot(app, build_req("/stall"))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(
        resp.headers()
            .get(header::RETRY_AFTER)
            .map(|v| v.to_str().unwrap()),
        Some("1"),
        "the shed carries Retry-After: 1"
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["error"]["code"], "server_busy");
    assert_eq!(v["error"]["retryable"], true);
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
        .layer(axum::middleware::from_fn_with_state(
            // The REAL middleware with a tiny injected budget: the test
            // proves the shed envelope, not the 30 s default.
            TimeoutBudget(Duration::from_millis(300)),
            request_timeout,
        ));

    let started = std::time::Instant::now();
    let resp = tower::ServiceExt::oneshot(app, build_req("/stall"))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert!(started.elapsed() < Duration::from_secs(2), "shed promptly");
    assert_eq!(
        resp.headers()
            .get(header::RETRY_AFTER)
            .map(|v| v.to_str().unwrap()),
        Some("1"),
        "the shed carries Retry-After: 1"
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["error"]["code"], "request_timeout");
    assert_eq!(v["error"]["retryable"], true);
}

fn build_req(path: &str) -> axum::extract::Request {
    use axum::body::Body;
    use axum::http::Request;
    Request::builder().uri(path).body(Body::empty()).unwrap()
}
