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
use std::time::Duration;

use tokio::net::TcpListener;
use tokio::sync::watch;

use crate::api_error::ApiError;
use crate::state::AppState;

/// The default background snapshot GC interval (60 s). The pass itself is
/// [`crate::snapshots_gc::gc_snapshots`]; the off-switch and the per-state
/// interval live on [`crate::state::ServerState`] (`snapshot_gc_enabled`,
/// `snapshot_gc_interval` — tests inject a short interval to observe a
/// background pass deterministically, and tests that drive GC directly turn
/// the background task off to keep effects deterministic).
pub const SNAPSHOT_GC_INTERVAL: Duration = Duration::from_secs(60);

/// The default background derive-queue interval (5 s): with M6a's deferred
/// kinds the queue drains continuously in the background; M6b's read routes
/// answer 503 `derivation_pending` + `Retry-After` for anything not yet
/// derived. The pass itself is [`crate::derive::derive_pass`]; a node's
/// lease lasts twice this interval (crash-recovery horizon). The off-switch
/// and per-state interval live on [`crate::state::ServerState`]
/// (`derive_queue_enabled`, `derive_queue_interval`), same pattern as the
/// GC pair above.
pub const DERIVE_QUEUE_INTERVAL: Duration = Duration::from_secs(5);

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
    // The kind NAME only — the redaction rule (the field carries no
    // endpoint/credential text; the README's "metadata backend: postgres"
    // promise is this line).
    tracing::info!(backend = cfg.state.meta_backend, "metadata backend");
    tracing::info!(
        protocol_version = crate::PROTOCOL_VERSION,
        "protocol version"
    );

    let app = crate::router(Arc::clone(&cfg.state));
    let drain_timeout = cfg.state.drain_timeout;

    // Boot-time derived-data backfill (M6b, the plan's deferred-backfill
    // check): enqueue every changeset each registered kind has no row for
    // — offline imports and pre-derive databases. The completion markers
    // are cleared SYNCHRONOUSLY, before the accept loop: an offline import
    // (registry-less, server down) may have added changesets with no
    // derive rows, and a marker left from the previous boot would let
    // reads skip the 503 `derivation_pending` contract and answer from
    // the incomplete index. That marker-CLEAR step fails the boot outright
    // (its error propagates below): booting with a stale marker is the
    // exact lie the 503 contract exists to prevent. The backfill itself
    // runs in the BACKGROUND (a large repo must not delay accepting
    // connections) and sets the markers on completion; a FAILED backfill
    // leaves them unset — reads stay 503 until a boot completes the
    // backfill. Idempotent; a background failure logs and leaves those
    // regions pending — backfill is enqueueing, not schema authority, so
    // it must not block boot. The off-switch
    // (`derive_backfill_enabled`) is for tests that hold the backfill to
    // build the unrowed window, same pattern as `derive_queue_enabled`.
    crate::derive::backfill_clear_markers(&cfg.state)
        .await
        .map_err(|e| io::Error::other(e.to_string()))?;
    if cfg.state.derive_backfill_enabled {
        let state = std::sync::Arc::clone(&cfg.state);
        tokio::spawn(async move {
            if let Err(e) = crate::derive::backfill(&state).await {
                tracing::error!(error = %e, "derive backfill failed; affected reads stay 503 derivation_pending");
            } else {
                tracing::info!("derive backfill enqueued");
            }
        });
    }

    // The caller hands us a one-shot shutdown future, but two consumers need
    // to observe it (the graceful-shutdown hook and the drain-timeout race).
    // Fan it out through a watch channel; receivers are subscribed before the
    // sender can fire, so the signal cannot be missed.
    let (signal_tx, signal_rx) = watch::channel(false);
    tokio::spawn(async move {
        shutdown.await;
        let _ = signal_tx.send(true);
    });

    // Background snapshot GC: an opportunistic pass on the state's interval
    // (default [`SNAPSHOT_GC_INTERVAL`]). The task lives on the serving
    // runtime (dropped with it on shutdown); a failed pass is logged and
    // retried on the next tick.
    if cfg.state.snapshot_gc_enabled {
        let gc_state = Arc::clone(&cfg.state);
        let gc_interval = cfg.state.snapshot_gc_interval;
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(gc_interval);
            loop {
                interval.tick().await;
                if let Err(e) = crate::snapshots_gc::gc_snapshots(&gc_state).await {
                    tracing::warn!(error = %e, "background snapshot GC failed");
                }
            }
        });
    }

    // Background derive queue (M6a): same pattern — a pass per tick on the
    // state's interval, beside the GC task, dropped with the serving
    // runtime on shutdown; a failed pass is logged and retried on the next
    // tick.
    if cfg.state.derive_queue_enabled {
        let dq_state = Arc::clone(&cfg.state);
        let dq_interval = cfg.state.derive_queue_interval;
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(dq_interval);
            loop {
                interval.tick().await;
                if let Err(e) = crate::derive::derive_pass(&dq_state).await {
                    tracing::warn!(error = %e, "background derive pass failed");
                }
            }
        });
    }

    // Background reachability GC (M10a): a mark-and-sweep pass per tick on
    // the state's interval (default one hour — the mark is a full-table
    // walk; the M5 snapshot GC keeps its own faster task). The pass batches
    // through the single-writer slot, so ticks only ever 503 writes for one
    // batch at a time. `gc_interval_secs = 0` in backend.toml leaves the
    // sweep to the host-local `naatre gc` verb; a failed pass is logged and
    // retried on the next tick (idempotent by construction).
    if cfg.state.gc_enabled {
        let gc_state = Arc::clone(&cfg.state);
        let gc_interval = cfg.state.gc_interval;
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(gc_interval);
            loop {
                interval.tick().await;
                match crate::gc::gc_pass(&gc_state).await {
                    Ok(report) if report.collected_any() => {
                        tracing::info!(
                            cs = report.cs_collected,
                            blobs = report.blobs_collected,
                            trees = report.trees_collected,
                            "background reachability GC collected"
                        );
                    }
                    Ok(_) => {}
                    Err(e) => {
                        tracing::warn!(error = %e, "background reachability GC failed")
                    }
                }
            }
        });
    }

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
