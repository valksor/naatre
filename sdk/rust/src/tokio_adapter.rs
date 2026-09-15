use crate::{
    Cancellation, ClientError, FallibleStream, RequestOptions, StreamTransport, Transport,
};
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

type BoxedFuture = Pin<Box<dyn Future<Output = Result<Vec<u8>, ClientError>> + Send + 'static>>;
type BoxedStream = Pin<Box<dyn FallibleStream<Item = Vec<u8>> + Send + 'static>>;

fn transport_failed() -> ClientError {
    ClientError::new("CLIENT_TRANSPORT_FAILED", "transport operation failed")
}

fn stream_failed() -> ClientError {
    ClientError::new("CLIENT_STREAM_FAILED", "transport stream failed")
}

/// Task-local future used by [`TokioTransport`].
///
/// The future erases implementation error details. Dropping it drops the
/// underlying I/O future immediately; this does not claim that effects already
/// accepted by an external system have stopped.
pub struct TokioFuture {
    inner: Option<BoxedFuture>,
}

impl TokioFuture {
    fn new(future: BoxedFuture) -> Self {
        Self {
            inner: Some(future),
        }
    }
}

impl Future for TokioFuture {
    type Output = Result<Vec<u8>, ClientError>;

    fn poll(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Self::Output> {
        let Some(future) = self.inner.as_mut() else {
            return Poll::Ready(Err(transport_failed()));
        };
        match future.as_mut().poll(context) {
            Poll::Ready(Ok(response)) => {
                self.inner = None;
                Poll::Ready(Ok(response))
            }
            Poll::Ready(Err(_)) => {
                self.inner = None;
                Poll::Ready(Err(transport_failed()))
            }
            Poll::Pending => Poll::Pending,
        }
    }
}

type UnaryHandler = dyn Fn(Vec<u8>, RequestOptions, Cancellation) -> BoxedFuture + Send + Sync;

/// Tokio task-local adapter for executor-neutral unary transports.
///
/// The adapter does not construct a Tokio runtime or spawn a task. A caller
/// polls the returned future on its selected Tokio runtime, so cancellation by
/// drop releases the underlying future without orphaning detached work.
#[derive(Clone)]
pub struct TokioTransport {
    handler: Arc<UnaryHandler>,
}

impl TokioTransport {
    #[must_use]
    pub fn new<H, F>(handler: H) -> Self
    where
        H: Fn(Vec<u8>, RequestOptions, Cancellation) -> F + Send + Sync + 'static,
        F: Future<Output = Result<Vec<u8>, ClientError>> + Send + 'static,
    {
        Self {
            handler: Arc::new(move |body, options, cancellation| {
                Box::pin(handler(body, options, cancellation))
            }),
        }
    }
}

impl Transport for TokioTransport {
    type Future<'a> = TokioFuture;

    fn execute(
        &self,
        body: Vec<u8>,
        options: RequestOptions,
        cancellation: Cancellation,
    ) -> Self::Future<'_> {
        TokioFuture::new((self.handler)(body, options, cancellation))
    }
}

/// Type-erased byte stream used by [`TokioStreamTransport`].
///
/// Adapter failures are mapped to `CLIENT_STREAM_FAILED` without retaining
/// their private messages.
pub struct TokioByteStream {
    inner: Option<BoxedStream>,
}

impl TokioByteStream {
    #[must_use]
    pub fn new<S>(stream: S) -> Self
    where
        S: FallibleStream<Item = Vec<u8>> + Send + 'static,
    {
        Self {
            inner: Some(Box::pin(stream)),
        }
    }
}

impl FallibleStream for TokioByteStream {
    type Item = Vec<u8>;

    fn poll_next(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<Self::Item, ClientError>>> {
        let Some(stream) = self.inner.as_mut() else {
            return Poll::Ready(None);
        };
        match stream.as_mut().poll_next(context) {
            Poll::Ready(Some(Ok(frame))) => Poll::Ready(Some(Ok(frame))),
            Poll::Ready(Some(Err(_))) => {
                self.inner = None;
                Poll::Ready(Some(Err(stream_failed())))
            }
            Poll::Ready(None) => {
                self.inner = None;
                Poll::Ready(None)
            }
            Poll::Pending => Poll::Pending,
        }
    }
}

type StreamHandler = dyn Fn(Vec<u8>, RequestOptions, Cancellation) -> TokioByteStream + Send + Sync;

/// Tokio task-local adapter for executor-neutral streaming transports.
///
/// The returned stream remains bounded by the core `StreamHandle` frame limit.
/// No queue, reconnect loop, runtime, or detached producer is created here.
#[derive(Clone)]
pub struct TokioStreamTransport {
    handler: Arc<StreamHandler>,
}

impl TokioStreamTransport {
    #[must_use]
    pub fn new<H, S>(handler: H) -> Self
    where
        H: Fn(Vec<u8>, RequestOptions, Cancellation) -> S + Send + Sync + 'static,
        S: FallibleStream<Item = Vec<u8>> + Send + 'static,
    {
        Self {
            handler: Arc::new(move |body, options, cancellation| {
                TokioByteStream::new(handler(body, options, cancellation))
            }),
        }
    }
}

impl StreamTransport for TokioStreamTransport {
    type Stream<'a> = TokioByteStream;

    fn stream(
        &self,
        body: Vec<u8>,
        options: RequestOptions,
        cancellation: Cancellation,
    ) -> Self::Stream<'_> {
        (self.handler)(body, options, cancellation)
    }
}
