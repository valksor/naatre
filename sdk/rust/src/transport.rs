use crate::{ClientError, MAXIMUM_FRAME_BYTES, Operation, OperationResult};
use serde::Serialize;
use serde::de::DeserializeOwned;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::task::{Context, Poll};
use std::time::Instant;

#[derive(Clone, Debug, Default)]
pub struct Cancellation(Arc<AtomicBool>);

impl Cancellation {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    pub fn cancel(&self) {
        self.0.store(true, Ordering::Release);
    }

    #[must_use]
    pub fn is_cancelled(&self) -> bool {
        self.0.load(Ordering::Acquire)
    }
}

#[derive(Clone, Debug, Default)]
pub struct RequestOptions {
    pub deadline: Option<Instant>,
}

impl RequestOptions {
    fn validate(&self) -> Result<(), ClientError> {
        if self
            .deadline
            .is_some_and(|deadline| deadline <= Instant::now())
        {
            return Err(ClientError::new(
                "CLIENT_DEADLINE_EXCEEDED",
                "request deadline has elapsed",
            ));
        }
        Ok(())
    }
}

pub trait Transport: Send + Sync {
    type Future<'a>: Future<Output = Result<Vec<u8>, ClientError>> + Send + 'a
    where
        Self: 'a;

    fn execute(
        &self,
        body: Vec<u8>,
        options: RequestOptions,
        cancellation: Cancellation,
    ) -> Self::Future<'_>;
}

pub trait FallibleStream {
    type Item;

    fn poll_next(
        self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<Self::Item, ClientError>>>;
}

pub trait StreamTransport: Send + Sync {
    type Stream<'a>: FallibleStream<Item = Vec<u8>> + Send + 'a
    where
        Self: 'a;

    fn stream(
        &self,
        body: Vec<u8>,
        options: RequestOptions,
        cancellation: Cancellation,
    ) -> Self::Stream<'_>;
}

#[derive(Debug)]
pub struct RequestHandle<F> {
    future: Pin<Box<F>>,
    cancellation: Cancellation,
    complete: bool,
}

impl<F> RequestHandle<F> {
    #[must_use]
    pub fn new(future: F, cancellation: Cancellation) -> Self {
        Self {
            future: Box::pin(future),
            cancellation,
            complete: false,
        }
    }
}

impl<F: Future> Future for RequestHandle<F> {
    type Output = F::Output;

    fn poll(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Self::Output> {
        let result = self.future.as_mut().poll(context);
        if result.is_ready() {
            self.complete = true;
        }
        result
    }
}

impl<F> Drop for RequestHandle<F> {
    fn drop(&mut self) {
        if !self.complete {
            self.cancellation.cancel();
        }
    }
}

#[derive(Debug)]
pub struct StreamHandle<S> {
    stream: Pin<Box<S>>,
    cancellation: Cancellation,
    complete: bool,
}

impl<S> StreamHandle<S> {
    #[must_use]
    pub fn new(stream: S, cancellation: Cancellation) -> Self {
        Self {
            stream: Box::pin(stream),
            cancellation,
            complete: false,
        }
    }
}

impl<S: FallibleStream<Item = Vec<u8>>> FallibleStream for StreamHandle<S> {
    type Item = Vec<u8>;

    fn poll_next(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<Self::Item, ClientError>>> {
        if self.complete {
            return Poll::Ready(None);
        }
        let result = self.stream.as_mut().poll_next(context);
        match result {
            Poll::Ready(Some(Ok(frame))) if frame.len() > MAXIMUM_FRAME_BYTES => {
                self.complete = true;
                self.cancellation.cancel();
                Poll::Ready(Some(Err(ClientError::new(
                    "CLIENT_FRAME_TOO_LARGE",
                    "stream frame exceeds the configured limit",
                ))))
            }
            Poll::Ready(Some(Err(error))) => {
                self.complete = true;
                self.cancellation.cancel();
                Poll::Ready(Some(Err(error)))
            }
            Poll::Ready(None) => {
                self.complete = true;
                Poll::Ready(None)
            }
            other => other,
        }
    }
}

impl<S> Drop for StreamHandle<S> {
    fn drop(&mut self) {
        if !self.complete {
            self.cancellation.cancel();
        }
    }
}

#[derive(Clone, Debug)]
pub struct Client<T> {
    transport: T,
}

/// Executor-neutral future returned by a unary client operation.
pub type OperationFuture<'a, R> =
    Pin<Box<dyn Future<Output = Result<OperationResult<R>, ClientError>> + Send + 'a>>;

impl<T> Client<T> {
    #[must_use]
    pub const fn new(transport: T) -> Self {
        Self { transport }
    }
}

impl<T: Transport + 'static> Client<T> {
    /// Starts a unary operation without choosing or spawning an executor.
    ///
    /// # Errors
    ///
    /// Returns a typed client error when the deadline has elapsed or the
    /// generated variables cannot be encoded.
    pub fn execute<'a, V, R>(
        &'a self,
        operation: &'a Operation<V, R>,
        variables: &V,
        options: RequestOptions,
    ) -> Result<OperationFuture<'a, R>, ClientError>
    where
        V: Serialize,
        R: DeserializeOwned + Send + 'a,
    {
        options.validate()?;
        let body = operation.request_bytes(variables)?;
        let cancellation = Cancellation::new();
        let future = self.transport.execute(body, options, cancellation.clone());
        let handle = RequestHandle::new(future, cancellation);
        Ok(Box::pin(async move {
            let response = handle.await?;
            operation.decode_result(&response)
        }))
    }
}

impl<T: StreamTransport + 'static> Client<T> {
    /// Starts a fallible byte-frame stream owned by the returned handle.
    ///
    /// # Errors
    ///
    /// Returns a typed client error when the deadline has elapsed or the
    /// generated variables cannot be encoded.
    pub fn stream<'a, V, R>(
        &'a self,
        operation: &Operation<V, R>,
        variables: &V,
        options: RequestOptions,
    ) -> Result<StreamHandle<T::Stream<'a>>, ClientError>
    where
        V: Serialize,
    {
        options.validate()?;
        let body = operation.request_bytes(variables)?;
        let cancellation = Cancellation::new();
        let stream = self.transport.stream(body, options, cancellation.clone());
        Ok(StreamHandle::new(stream, cancellation))
    }
}
