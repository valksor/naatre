//! Serve + graceful shutdown: the server keeps serving while a write
//! transaction is in flight, drains on signal, and stops listening after.
//! Bounded waits everywhere — no sleeps-as-sync, no unbounded retries.

mod common;

use std::io::ErrorKind;
use std::net::SocketAddr;
use std::time::Duration;

use naatre_server::api_code::ApiCode;
use naatre_server::api_error::ApiError;
use naatre_server::serve::{ServeConfig, serve};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::oneshot;
use tokio::time::timeout;

/// Minimal HTTP/1.1 GET over a raw socket; reads to EOF (`Connection:
/// close`). Bounded by the caller's `timeout`.
async fn http_get(addr: SocketAddr, path: &str) -> std::io::Result<String> {
    let mut stream = TcpStream::connect(addr).await?;
    let request = format!("GET {path} HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n");
    stream.write_all(request.as_bytes()).await?;
    let mut raw = Vec::new();
    stream.read_to_end(&mut raw).await?;
    Ok(String::from_utf8_lossy(&raw).into_owned())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn serve_stays_up_through_a_write_txn_then_drains() {
    let dir = tempfile::tempdir().unwrap();
    // Drain window short enough to bound the test; far above real drain time.
    let state = common::test_state(dir.path(), Duration::from_secs(2));

    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let (send_shutdown, recv_shutdown) = oneshot::channel::<()>();
    let serve_task = tokio::spawn(serve(
        ServeConfig {
            listener,
            state: state.clone(),
        },
        async move {
            let _ = recv_shutdown.await;
        },
    ));

    // 1. A slow write transaction takes the single-writer slot…
    let hold_task = {
        let state = state.clone();
        tokio::spawn(async move { state.hold_write_txn(2000).await })
    };
    // …observable: writes fail fast with writer_busy (polling loop bounded
    // at 100 × 20 ms; the hold lasts 2 s, so the window always exists).
    let mut contended = false;
    for _ in 0..100 {
        match state.write(|_store| Ok::<(), ApiError>(())).await {
            Err(e) if e.code() == ApiCode::WriterBusy => {
                contended = true;
                break;
            }
            _ => tokio::time::sleep(Duration::from_millis(20)).await,
        }
    }
    assert!(contended, "hold_write_txn never took the store mutex");

    // 2. …and the server still serves a route that does not touch the store.
    let response = timeout(Duration::from_secs(2), http_get(addr, "/version"))
        .await
        .expect("version responded while the write txn was held")
        .expect("http exchange");
    assert!(response.starts_with("HTTP/1.1 200"), "got: {response}");
    assert!(response.contains("\"protocol\":1"), "got: {response}");

    // 3. Shutdown signal → serve returns within the drain window (drain of
    // an idle server is immediate; the 2 s cap must not be needed).
    send_shutdown.send(()).expect("serve task alive");
    let drain_started = std::time::Instant::now();
    timeout(Duration::from_secs(10), serve_task)
        .await
        .expect("serve returned within 10 s")
        .expect("join")
        .expect("serve ok");
    assert!(
        drain_started.elapsed() <= Duration::from_secs(2) + Duration::from_millis(500),
        "serve took {:?} to return after shutdown",
        drain_started.elapsed()
    );
    hold_task.await.expect("hold task");

    // 4. The listener is gone: new connections are refused. Refusal is
    // expected immediately; the bounded retry only guards scheduling.
    let mut refused = false;
    for _ in 0..20 {
        match TcpStream::connect(addr).await {
            Err(e) if e.kind() == ErrorKind::ConnectionRefused => {
                refused = true;
                break;
            }
            _ => tokio::time::sleep(Duration::from_millis(50)).await,
        }
    }
    assert!(refused, "listener still accepting after serve returned");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn drain_timeout_forces_return() {
    // A request stalled mid-headers never completes, so once the server has
    // accepted it, the only way serve can finish is the drain cap. connect()
    // merely queues the connection in the kernel backlog, so "accepted
    // before shutdown" is a race: retry the whole scenario until the cap
    // branch is the one that fired (a cap-fire needs >= drain_timeout).
    for _ in 0..5 {
        let dir = tempfile::tempdir().unwrap();
        let state = common::test_state(dir.path(), Duration::from_millis(300));
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let (send_shutdown, recv_shutdown) = oneshot::channel::<()>();
        let serve_task = tokio::spawn(serve(ServeConfig { listener, state }, async move {
            let _ = recv_shutdown.await;
        }));

        let mut stalled = TcpStream::connect(addr).await.unwrap();
        // Headers without the terminating blank line: the server waits for
        // the rest of the request for as long as the connection lives.
        stalled
            .write_all(b"GET /version HTTP/1.1\r\nHost: test\r\n")
            .await
            .unwrap();
        // Acceptance margin for the stalled connection (bounded).
        tokio::time::sleep(Duration::from_millis(50)).await;

        send_shutdown.send(()).expect("serve task alive");
        let started = std::time::Instant::now();
        let outcome = timeout(Duration::from_secs(5), serve_task)
            .await
            .expect("serve returned within 5 s")
            .expect("join");
        let took = started.elapsed();

        if took >= Duration::from_millis(300) {
            // The cap branch: waits out the full drain window, then returns.
            outcome.expect("serve ok");
            return;
        }
        // Stalled connection was not accepted before shutdown; the server
        // drained nothing in-flight and returned early. Drop it and retry.
        drop(stalled);
    }
    panic!("drain cap branch never fired within 5 attempts");
}
