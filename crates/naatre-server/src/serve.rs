//! Process lifecycle: startup banner, serve loop, graceful shutdown with a
//! bounded drain.
//!
//! Transport guards (router-wide, dependency-free middleware in
//! [`crate::middleware`], installed by [`crate::router`]): every request
//! runs under a 30 s time budget (`REQUEST_TIMEOUT`, shed as 503
//! `request_timeout`) and through a 256-permit concurrent-request
//! semaphore (`MAX_IN_FLIGHT_REQUESTS`, shed as 503 `server_busy`). axum's
//! `serve` has no connection cap of its own, so the semaphore is the
//! process's admission control.

use std::future::{Future, IntoFuture};
use std::io;
use std::sync::Arc;

use tokio::net::TcpListener;
use tokio::sync::watch;

use crate::api_error::ApiError;
use crate::state::AppState;

/// Everything [`serve`] needs: the bound listener and the shared state.
pub struct ServeConfig {
    pub listener: TcpListener,
    pub state: AppState,
}

/// Serves the API on `cfg.listener` until `shutdown` resolves, then drains.
///
/// Shutdown behavior: when `shutdown` resolves, the server stops accepting
/// new connections and waits for in-flight requests. If draining takes
/// longer than `state.drain_timeout`, the wait is cut off with a warn log
/// and this function returns, dropping remaining connections. Returns the
/// server's own result otherwise (e.g. listener failure).
pub async fn serve(
    cfg: ServeConfig,
    shutdown: impl Future<Output = ()> + Send + 'static,
) -> io::Result<()> {
    let addr = cfg.listener.local_addr()?;
    let schema_version = cfg
        .state
        .read(|conn| Ok::<i64, ApiError>(naatre_store::store::schema_version(conn)))
        .await
        .map_err(|e| io::Error::other(e.to_string()))?;

    // Startup banner: one info line per fact (grep- and diff-friendly).
    // Facts are name/version/paths only — never credentials (see
    // `ServerState::data_dir_display` for the rule).
    tracing::info!(
        name = env!("CARGO_PKG_NAME"),
        version = env!("CARGO_PKG_VERSION"),
        "starting naatre"
    );
    tracing::info!(bind = %addr, "listening");
    tracing::info!(
        data_dir = %cfg.state.data_dir_display.display(),
        "data dir"
    );
    tracing::info!(schema_version, "schema version");
    tracing::info!(
        protocol_version = crate::PROTOCOL_VERSION,
        "protocol version"
    );

    let app = crate::router(Arc::clone(&cfg.state));
    let drain_timeout = cfg.state.drain_timeout;

    // The caller hands us a one-shot shutdown future, but two consumers need
    // to observe it (the graceful-shutdown hook and the drain-timeout race).
    // Fan it out through a watch channel; receivers are subscribed before the
    // sender can fire, so the signal cannot be missed.
    let (signal_tx, signal_rx) = watch::channel(false);
    tokio::spawn(async move {
        shutdown.await;
        let _ = signal_tx.send(true);
    });

    let graceful_rx = signal_rx.clone();
    let graceful = async move {
        let mut rx = graceful_rx;
        let _ = rx.changed().await;
    };
    let deadline_rx = signal_rx.clone();
    let drain_deadline = async move {
        let mut rx = deadline_rx;
        if rx.changed().await.is_ok() {
            tokio::time::sleep(drain_timeout).await;
        }
    };

    let mut server = std::pin::pin!(
        axum::serve(cfg.listener, app)
            .with_graceful_shutdown(graceful)
            .into_future()
    );
    tokio::select! {
        result = &mut server => result,
        _ = drain_deadline => {
            tracing::warn!(
                drain_timeout_secs = drain_timeout.as_secs(),
                "graceful drain timeout exceeded; forcing shutdown, in-flight requests may be dropped"
            );
            Ok(())
        }
    }
}

/// Resolves on SIGINT or SIGTERM (unix); on ctrl_c elsewhere. The CLI's
/// `serve` verb passes this (wrapped) as the shutdown future.
pub async fn unix_shutdown_signal() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        let mut terminate = signal(SignalKind::terminate()).expect("install SIGTERM handler");
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = terminate.recv() => {}
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}
