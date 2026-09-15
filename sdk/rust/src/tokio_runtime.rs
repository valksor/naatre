use crate::{Cancellation, HandlerFuture, OwnedWork, WorkerError};
use std::error::Error;
use std::fmt::{self, Display, Formatter};
use std::future::Future;
use std::sync::Arc;
use tokio::runtime::Handle;
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tokio::task::{JoinError, JoinHandle};

const MAXIMUM_OWNED_WORK: usize = 65_536;

/// Finite concurrency budgets for work spawned from a worker invocation.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct TokioWorkerLimits {
    pub max_tasks: usize,
    pub max_blocking_tasks: usize,
}

impl Default for TokioWorkerLimits {
    fn default() -> Self {
        Self {
            max_tasks: 64,
            max_blocking_tasks: 8,
        }
    }
}

/// Stable, redacted failure returned before Tokio work is spawned.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TokioWorkerError {
    code: &'static str,
    message: &'static str,
}

impl TokioWorkerError {
    const fn new(code: &'static str, message: &'static str) -> Self {
        Self { code, message }
    }

    #[must_use]
    pub const fn code(&self) -> &'static str {
        self.code
    }
}

impl Display for TokioWorkerError {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl Error for TokioWorkerError {}

/// Bounded Tokio task owner for [`crate::HandlerContext`].
///
/// The caller owns the Tokio runtime represented by `Handle`. Every spawned
/// task is returned as [`OwnedWork`] and must be transferred into the handler
/// context. Dropping the returned owner aborts an async task. Blocking work is
/// cooperatively cancelled and remains joined during orderly shutdown.
#[derive(Clone)]
pub struct TokioWorkerSpawner {
    handle: Handle,
    tasks: Arc<Semaphore>,
    blocking_tasks: Arc<Semaphore>,
}

impl TokioWorkerSpawner {
    /// Creates a spawner bound to an application-owned Tokio runtime.
    ///
    /// # Errors
    ///
    /// Returns `WORKER_CONFIG_INVALID` unless both budgets are finite,
    /// positive, and no larger than 65,536.
    pub fn new(handle: Handle, limits: TokioWorkerLimits) -> Result<Self, TokioWorkerError> {
        if !(1..=MAXIMUM_OWNED_WORK).contains(&limits.max_tasks)
            || !(1..=MAXIMUM_OWNED_WORK).contains(&limits.max_blocking_tasks)
        {
            return Err(TokioWorkerError::new(
                "WORKER_CONFIG_INVALID",
                "Tokio worker limits are invalid",
            ));
        }
        Ok(Self {
            handle,
            tasks: Arc::new(Semaphore::new(limits.max_tasks)),
            blocking_tasks: Arc::new(Semaphore::new(limits.max_blocking_tasks)),
        })
    }

    /// Creates a bounded spawner for the currently entered Tokio runtime.
    ///
    /// # Errors
    ///
    /// Returns `WORKER_RUNTIME_UNAVAILABLE` outside a Tokio runtime or the
    /// same configuration error as [`Self::new`].
    pub fn current(limits: TokioWorkerLimits) -> Result<Self, TokioWorkerError> {
        let handle = Handle::try_current().map_err(|_| {
            TokioWorkerError::new(
                "WORKER_RUNTIME_UNAVAILABLE",
                "Tokio worker runtime is unavailable",
            )
        })?;
        Self::new(handle, limits)
    }

    /// Spawns bounded asynchronous work and returns its cancellation/join
    /// owner. Transfer the returned value with
    /// [`crate::HandlerContext::own_spawned_task`].
    ///
    /// # Errors
    ///
    /// Returns `OVERLOADED` without spawning when the task budget is full.
    pub fn spawn<F>(&self, future: F) -> Result<Box<dyn OwnedWork>, TokioWorkerError>
    where
        F: Future<Output = Result<(), WorkerError>> + Send + 'static,
    {
        let permit = acquire(&self.tasks)?;
        let handle = self.handle.spawn(async move {
            let _permit = permit;
            future.await
        });
        Ok(Box::new(TokioTask {
            handle: Some(handle),
        }))
    }

    /// Spawns bounded blocking work and returns its cooperative cancellation
    /// and join owner. The closure must observe its cancellation token and
    /// return; Tokio cannot forcibly stop blocking system calls already in
    /// progress.
    ///
    /// # Errors
    ///
    /// Returns `OVERLOADED` without spawning when the blocking budget is full.
    pub fn spawn_blocking<F>(&self, work: F) -> Result<Box<dyn OwnedWork>, TokioWorkerError>
    where
        F: FnOnce(Cancellation) -> Result<(), WorkerError> + Send + 'static,
    {
        let permit = acquire(&self.blocking_tasks)?;
        let cancellation = Cancellation::new();
        let worker_cancellation = cancellation.clone();
        let handle = self.handle.spawn_blocking(move || {
            let _permit = permit;
            work(worker_cancellation)
        });
        Ok(Box::new(TokioBlockingTask {
            cancellation,
            handle: Some(handle),
        }))
    }
}

fn acquire(semaphore: &Arc<Semaphore>) -> Result<OwnedSemaphorePermit, TokioWorkerError> {
    match Arc::clone(semaphore).try_acquire_owned() {
        Ok(permit) => Ok(permit),
        Err(_) => Err(TokioWorkerError::new(
            "OVERLOADED",
            "Tokio worker capacity is full",
        )),
    }
}

struct TokioTask {
    handle: Option<JoinHandle<Result<(), WorkerError>>>,
}

impl OwnedWork for TokioTask {
    fn cancel(&mut self) {
        if let Some(handle) = &self.handle {
            handle.abort();
        }
    }

    fn shutdown(mut self: Box<Self>) -> HandlerFuture<'static, ()> {
        let handle = self
            .handle
            .take()
            .expect("Tokio task owner is consumed exactly once");
        Box::pin(async move { joined(handle.await) })
    }
}

impl Drop for TokioTask {
    fn drop(&mut self) {
        if let Some(handle) = &self.handle {
            handle.abort();
        }
    }
}

struct TokioBlockingTask {
    cancellation: Cancellation,
    handle: Option<JoinHandle<Result<(), WorkerError>>>,
}

impl OwnedWork for TokioBlockingTask {
    fn cancel(&mut self) {
        self.cancellation.cancel();
        if let Some(handle) = &self.handle {
            handle.abort();
        }
    }

    fn shutdown(mut self: Box<Self>) -> HandlerFuture<'static, ()> {
        self.cancellation.cancel();
        let handle = self
            .handle
            .take()
            .expect("Tokio blocking task owner is consumed exactly once");
        Box::pin(async move { joined(handle.await) })
    }
}

impl Drop for TokioBlockingTask {
    fn drop(&mut self) {
        self.cancellation.cancel();
        if let Some(handle) = &self.handle {
            handle.abort();
        }
    }
}

fn joined(result: Result<Result<(), WorkerError>, JoinError>) -> Result<(), WorkerError> {
    match result {
        Ok(result) => result,
        Err(error) if error.is_cancelled() => Ok(()),
        Err(_) => Err(WorkerError::new(
            "INTERNAL",
            "owned Tokio work failed",
            false,
        )),
    }
}
