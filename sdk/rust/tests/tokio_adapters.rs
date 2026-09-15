#![cfg(feature = "tokio")]

mod common;

use common::{Payload, Variables, payload_operation};
use naatre_sdk::{
    Cancellation, Client, ClientError, FallibleStream, RequestOptions, StreamTransport,
    TokioByteStream, TokioStreamTransport, TokioTransport, Transport,
};
use std::future::{Future, ready};
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::task::{Context, Poll, Waker};

fn poll_future<F: Future + ?Sized>(future: Pin<&mut F>) -> Poll<F::Output> {
    future.poll(&mut Context::from_waker(Waker::noop()))
}

#[test]
fn unary_adapter_executes_on_the_callers_task() {
    let transport = TokioTransport::new(|body, _, _| {
        assert!(
            String::from_utf8(body)
                .expect("canonical request")
                .contains("Payload")
        );
        ready(Ok(br#"{"data":{"name":"Ada"},"complete":true}"#.to_vec()))
    });
    let client = Client::new(transport);
    let operation = payload_operation();
    let mut future = client
        .execute(
            &operation,
            &Variables { id: "1".to_owned() },
            RequestOptions::default(),
        )
        .expect("request starts");
    let Poll::Ready(Ok(result)) = poll_future(future.as_mut()) else {
        panic!("ready transport did not complete");
    };
    assert_eq!(
        result.data,
        naatre_sdk::Presence::Value(Payload {
            name: "Ada".to_owned()
        })
    );
}

#[test]
fn unary_adapter_sanitizes_private_transport_failures() {
    let transport = TokioTransport::new(|_, _, _| {
        ready(Err(ClientError::new(
            "PRIVATE_BACKEND_CODE",
            "authorization: Bearer protected-token; tenant=private",
        )))
    });
    let mut future =
        Box::pin(transport.execute(Vec::new(), RequestOptions::default(), Cancellation::new()));
    let Poll::Ready(Err(error)) = poll_future(future.as_mut()) else {
        panic!("failed transport did not complete");
    };
    assert_eq!(error.code(), "CLIENT_TRANSPORT_FAILED");
    assert_eq!(error.to_string(), "transport operation failed");
    assert!(!error.to_string().contains("protected-token"));
}

struct PendingIo {
    dropped: Arc<AtomicBool>,
}

impl Future for PendingIo {
    type Output = Result<Vec<u8>, ClientError>;

    fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Self::Output> {
        Poll::Pending
    }
}

impl Drop for PendingIo {
    fn drop(&mut self) {
        self.dropped.store(true, Ordering::Release);
    }
}

#[test]
fn dropping_unfinished_future_releases_io_ownership() {
    let dropped = Arc::new(AtomicBool::new(false));
    let observed = Arc::clone(&dropped);
    let transport = TokioTransport::new(move |_, _, _| PendingIo {
        dropped: Arc::clone(&observed),
    });
    let client = Client::new(transport);
    let operation = payload_operation();
    let future = client
        .execute(
            &operation,
            &Variables { id: "1".to_owned() },
            RequestOptions::default(),
        )
        .expect("request starts");
    drop(future);
    assert!(dropped.load(Ordering::Acquire));
}

struct Frames {
    frames: Vec<Result<Vec<u8>, ClientError>>,
}

impl FallibleStream for Frames {
    type Item = Vec<u8>;

    fn poll_next(
        mut self: Pin<&mut Self>,
        _: &mut Context<'_>,
    ) -> Poll<Option<Result<Self::Item, ClientError>>> {
        Poll::Ready(self.frames.pop())
    }
}

#[test]
fn stream_adapter_preserves_frames_and_sanitizes_failures() {
    let mut stream = TokioByteStream::new(Frames {
        frames: vec![
            Err(ClientError::new(
                "PRIVATE_STREAM_CODE",
                "cookie=session-secret",
            )),
            Ok(b"frame".to_vec()),
        ],
    });
    let mut context = Context::from_waker(Waker::noop());
    assert!(matches!(
        Pin::new(&mut stream).poll_next(&mut context),
        Poll::Ready(Some(Ok(frame))) if frame == b"frame"
    ));
    let Poll::Ready(Some(Err(error))) = Pin::new(&mut stream).poll_next(&mut context) else {
        panic!("stream failure was not returned");
    };
    assert_eq!(error.code(), "CLIENT_STREAM_FAILED");
    assert_eq!(error.to_string(), "transport stream failed");
    assert!(!error.to_string().contains("session-secret"));
    assert!(matches!(
        Pin::new(&mut stream).poll_next(&mut context),
        Poll::Ready(None)
    ));
}

#[test]
fn stream_adapter_keeps_core_frame_limit_and_drop_cancellation() {
    let cancellation = Cancellation::new();
    let cancellation_seen = cancellation.clone();
    let transport = TokioStreamTransport::new(|_, _, _| Frames {
        frames: vec![Ok(vec![0; naatre_sdk::MAXIMUM_FRAME_BYTES + 1])],
    });
    let stream = transport.stream(Vec::new(), RequestOptions::default(), cancellation.clone());
    let mut stream = naatre_sdk::StreamHandle::new(stream, cancellation);
    let mut context = Context::from_waker(Waker::noop());
    let Poll::Ready(Some(Err(error))) = Pin::new(&mut stream).poll_next(&mut context) else {
        panic!("oversized frame was not rejected");
    };
    assert_eq!(error.code(), "CLIENT_FRAME_TOO_LARGE");
    assert!(cancellation_seen.is_cancelled());
}
