#![cfg(feature = "axum")]

use axum::body::{Body, to_bytes};
use axum::http::{Method, Request, StatusCode};
use naatre_sdk::{
    AXUM_CANCEL_PATH, AXUM_INVOKE_PATH, AXUM_REGISTER_PATH, AXUM_WORKER_MEDIA_TYPE,
    AXUM_WORKER_PROTOCOL_HEADER, AxumWorker, AxumWorkerConfig, CancelRequest, HandlerContext,
    HandlerDescriptor, HandlerFuture, Parent, Principal, Registration, RequestOutcome,
    RequestResources, RequestScopeFactory, SchemaValidator, ServerBuilder, TokioWorkerLimits,
    TokioWorkerSpawner, WORKER_PROTOCOL, WorkerError, WorkerInvocation, WorkerResult,
    decode_worker_frame, encode_worker_frame,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::any::Any;
use std::future::pending;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use tokio::runtime::{Builder, Runtime};
use tokio::sync::Notify;
use tower::ServiceExt;

#[derive(Debug, Deserialize)]
struct RemoteFixture {
    registration: Registration,
}

#[derive(Debug, Deserialize)]
struct GreetInput {
    name: String,
}

#[derive(Debug, Serialize)]
struct GreetOutput {
    greeting: String,
}

#[derive(Default)]
struct Validator;

impl SchemaValidator for Validator {
    fn validate_input(&self, schema: &str, input: &Value) -> Result<(), WorkerError> {
        match (schema, input.get("name")) {
            ("GreetInput", Some(Value::String(_))) => Ok(()),
            _ => Err(WorkerError::new(
                "INPUT_COERCION",
                "private input validation detail",
                false,
            )),
        }
    }

    fn validate_output(&self, schema: &str, output: &Value) -> Result<(), WorkerError> {
        match schema {
            "GreetOutput" if output.get("greeting").is_some() => Ok(()),
            _ => Err(WorkerError::new(
                "OUTPUT_COMPLETION",
                "private output validation detail",
                false,
            )),
        }
    }
}

struct Resources {
    principal: Principal,
    loader: (),
    outcomes: Arc<Mutex<Vec<RequestOutcome>>>,
}

impl RequestResources for Resources {
    fn principal(&self) -> &Principal {
        &self.principal
    }

    fn loader(&mut self) -> &mut (dyn Any + Send) {
        &mut self.loader
    }

    fn transaction(&mut self) -> Option<&mut (dyn Any + Send)> {
        None
    }

    fn finish(self: Box<Self>, outcome: RequestOutcome) {
        let mut outcomes = self.outcomes.lock().expect("outcome lock");
        outcomes.push(outcome);
    }
}

#[derive(Default)]
struct Scopes {
    outcomes: Arc<Mutex<Vec<RequestOutcome>>>,
}

impl RequestScopeFactory for Scopes {
    fn begin(
        &self,
        invocation: &WorkerInvocation,
    ) -> Result<Box<dyn RequestResources>, WorkerError> {
        match invocation.delegated_context.as_str() {
            "valid-delegation" => Ok(Box::new(Resources {
                principal: Principal {
                    subject: "fixture-subject".to_owned(),
                    tenant: "fixture-tenant".to_owned(),
                },
                loader: (),
                outcomes: Arc::clone(&self.outcomes),
            })),
            _ => Err(WorkerError::new(
                "UNAUTHORIZED",
                "Bearer credential-secret tenant=protected",
                false,
            )),
        }
    }
}

fn registration() -> Registration {
    serde_json::from_slice::<RemoteFixture>(include_bytes!(
        "../../../conformance/v1/remote-workers.json"
    ))
    .expect("remote-worker fixture")
    .registration
}

fn invocation(id: &str, delegation: &str) -> WorkerInvocation {
    WorkerInvocation {
        protocol: WORKER_PROTOCOL.to_owned(),
        request_id: format!("request-{id}"),
        invocation_id: format!("invocation-{id}"),
        attempt_id: format!("invocation-{id}.1"),
        handler_id: "fixture.greet".to_owned(),
        schema_revision: "schema-1".to_owned(),
        deadline_unix_milli: 4_102_444_800_000,
        idempotency_key: String::new(),
        delegated_context: delegation.to_owned(),
        parent: Parent::default(),
        input: json!({"name": "Ada"}),
        resume_cursor: String::new(),
    }
}

fn worker<H>(
    handler: H,
    invoke: for<'a> fn(&'a H, &'a mut HandlerContext, GreetInput) -> HandlerFuture<'a, GreetOutput>,
) -> (Arc<naatre_sdk::WorkerCore>, Arc<Scopes>)
where
    H: Send + Sync + 'static,
{
    let scopes = Arc::new(Scopes::default());
    let mut builder = ServerBuilder::new(
        registration(),
        Arc::new(Validator),
        Arc::clone(&scopes) as Arc<dyn RequestScopeFactory>,
    )
    .expect("valid worker configuration");
    builder
        .register_handler(
            HandlerDescriptor::unary("fixture.greet", "GreetInput", "GreetOutput", "query"),
            handler,
            invoke,
        )
        .expect("matching handler");
    (Arc::new(builder.build().expect("complete worker")), scopes)
}

fn greet<'a>(
    _handler: &'a (),
    _context: &'a mut HandlerContext,
    input: GreetInput,
) -> HandlerFuture<'a, GreetOutput> {
    Box::pin(async move {
        Ok(GreetOutput {
            greeting: format!("Hello, {}", input.name),
        })
    })
}

fn panics<'a>(
    _handler: &'a (),
    _context: &'a mut HandlerContext,
    _input: GreetInput,
) -> HandlerFuture<'a, GreetOutput> {
    Box::pin(async { panic!("credential-secret implementation panic") })
}

struct PendingHandler {
    started: Arc<Notify>,
    dropped: Arc<AtomicBool>,
}

struct DropProbe(Arc<AtomicBool>);

impl Drop for DropProbe {
    fn drop(&mut self) {
        self.0.store(true, Ordering::Release);
    }
}

struct NotifyDrop(Arc<Notify>);

impl Drop for NotifyDrop {
    fn drop(&mut self) {
        self.0.notify_one();
    }
}

fn waits_forever<'a>(
    handler: &'a PendingHandler,
    _context: &'a mut HandlerContext,
    _input: GreetInput,
) -> HandlerFuture<'a, GreetOutput> {
    Box::pin(async move {
        let _probe = DropProbe(Arc::clone(&handler.dropped));
        handler.started.notify_one();
        pending::<Result<GreetOutput, WorkerError>>().await
    })
}

fn adapter(worker: Arc<naatre_sdk::WorkerCore>, maximum_frame_bytes: usize) -> AxumWorker {
    AxumWorker::new(
        worker,
        AxumWorkerConfig {
            session_id: "fixture-session".to_owned(),
            maximum_frame_bytes,
        },
    )
    .expect("valid Axum adapter")
}

fn envelope<T: Serialize>(kind: &str, payload: &T, maximum_frame_bytes: usize) -> Vec<u8> {
    encode_worker_frame(
        &json!({
            "protocol": WORKER_PROTOCOL,
            "kind": kind,
            "payload": payload,
        }),
        maximum_frame_bytes,
    )
    .expect("fixture frame")
}

fn request(method: Method, path: &str, body: Vec<u8>) -> Request<Body> {
    Request::builder()
        .method(method)
        .uri(path)
        .header("content-type", AXUM_WORKER_MEDIA_TYPE)
        .header("accept", AXUM_WORKER_MEDIA_TYPE)
        .header(AXUM_WORKER_PROTOCOL_HEADER, WORKER_PROTOCOL)
        .body(Body::from(body))
        .expect("fixture request")
}

fn runtime() -> Runtime {
    Builder::new_multi_thread()
        .worker_threads(2)
        .max_blocking_threads(2)
        .build()
        .expect("fixture Tokio runtime")
}

async fn response_bytes(response: axum::response::Response) -> Vec<u8> {
    to_bytes(response.into_body(), 1 << 20)
        .await
        .expect("bounded response")
        .to_vec()
}

#[test]
fn positive_register_and_invoke_use_the_normative_framed_transport() {
    runtime().block_on(async {
        let (worker, _) = worker((), greet);
        let app = adapter(Arc::clone(&worker), 4096).router();
        let registered = app
            .clone()
            .oneshot(request(
                Method::POST,
                AXUM_REGISTER_PATH,
                envelope("register", worker.registration(), 4096),
            ))
            .await
            .expect("registration response");
        assert_eq!(registered.status(), StatusCode::OK);
        assert_eq!(registered.headers()["content-type"], AXUM_WORKER_MEDIA_TYPE);
        let registered: Value =
            decode_worker_frame(&response_bytes(registered).await, 4096).expect("registered frame");
        assert_eq!(registered["kind"], "registered");
        assert_eq!(registered["payload"]["sessionId"], "fixture-session");

        let result = app
            .oneshot(request(
                Method::POST,
                AXUM_INVOKE_PATH,
                envelope("invoke", &invocation("positive", "valid-delegation"), 4096),
            ))
            .await
            .expect("invoke response");
        assert_eq!(result.status(), StatusCode::OK);
        let result: Value =
            decode_worker_frame(&response_bytes(result).await, 4096).expect("result frame");
        assert_eq!(result["kind"], "result");
        assert_eq!(result["payload"]["data"]["greeting"], "Hello, Ada");
    });
}

#[test]
fn negative_failures_are_stable_and_redact_credentials_and_panics() {
    runtime().block_on(async {
        let (worker, _) = worker((), greet);
        let unauthorized = adapter(worker, 4096)
            .router()
            .oneshot(request(
                Method::POST,
                AXUM_INVOKE_PATH,
                envelope("invoke", &invocation("secret", "credential-secret"), 4096),
            ))
            .await
            .expect("unauthorized response");
        assert_eq!(unauthorized.status(), StatusCode::FORBIDDEN);
        let body = response_bytes(unauthorized).await;
        assert_eq!(body, br#"{"code":"UNAUTHORIZED"}"#);
        assert!(!String::from_utf8_lossy(&body).contains("credential-secret"));

        let (worker, _) = worker((), panics);
        let panicked = adapter(worker, 4096)
            .router()
            .oneshot(request(
                Method::POST,
                AXUM_INVOKE_PATH,
                envelope("invoke", &invocation("panic", "valid-delegation"), 4096),
            ))
            .await
            .expect("contained panic response");
        assert_eq!(panicked.status(), StatusCode::INTERNAL_SERVER_ERROR);
        let body = response_bytes(panicked).await;
        assert_eq!(body, br#"{"code":"INTERNAL"}"#);
        assert!(!String::from_utf8_lossy(&body).contains("credential-secret"));
    });
}

#[test]
fn boundary_routes_headers_methods_and_frame_sizes_fail_closed() {
    runtime().block_on(async {
        let (worker, _) = worker((), greet);
        let app = adapter(worker, 128).router();

        let missing_headers = Request::builder()
            .method(Method::POST)
            .uri(AXUM_INVOKE_PATH)
            .body(Body::from(b"credential-secret".as_slice()))
            .expect("fixture request");
        let response = app
            .clone()
            .oneshot(missing_headers)
            .await
            .expect("header rejection");
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        assert_eq!(
            response_bytes(response).await,
            br#"{"code":"REMOTE_WORKER_MALFORMED"}"#
        );

        let response = app
            .clone()
            .oneshot(request(Method::GET, AXUM_INVOKE_PATH, Vec::new()))
            .await
            .expect("method rejection");
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);

        let response = app
            .clone()
            .oneshot(request(Method::POST, "/not-a-worker-route", Vec::new()))
            .await
            .expect("route rejection");
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
        assert_eq!(
            response_bytes(response).await,
            br#"{"code":"WORKER_ROUTE_NOT_FOUND"}"#
        );

        let response = app
            .oneshot(request(Method::POST, AXUM_INVOKE_PATH, vec![b'x'; 134]))
            .await
            .expect("body-limit rejection");
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        assert_eq!(
            response_bytes(response).await,
            br#"{"code":"REMOTE_WORKER_MALFORMED"}"#
        );
    });
}

#[test]
fn cancellation_drops_the_core_future_and_releases_request_resources() {
    runtime().block_on(async {
        let started = Arc::new(Notify::new());
        let dropped = Arc::new(AtomicBool::new(false));
        let handler = PendingHandler {
            started: Arc::clone(&started),
            dropped: Arc::clone(&dropped),
        };
        let (worker, scopes) = worker(handler, waits_forever);
        let adapter = adapter(worker, 4096);
        let app = adapter.router();
        let request = request(
            Method::POST,
            AXUM_INVOKE_PATH,
            envelope("invoke", &invocation("cancel", "valid-delegation"), 4096),
        );
        let route = tokio::spawn(app.oneshot(request));
        started.notified().await;

        let cancellation = CancelRequest {
            protocol: WORKER_PROTOCOL.to_owned(),
            request_id: "request-cancel".to_owned(),
            invocation_id: "invocation-cancel".to_owned(),
        };
        let acknowledgement = adapter
            .router()
            .oneshot(request(
                Method::POST,
                AXUM_CANCEL_PATH,
                envelope("cancel", &cancellation, 4096),
            ))
            .await
            .expect("cancellation acknowledgement");
        assert_eq!(acknowledgement.status(), StatusCode::OK);
        let acknowledgement: Value =
            decode_worker_frame(&response_bytes(acknowledgement).await, 4096)
                .expect("cancellation frame");
        assert_eq!(acknowledgement["kind"], "cancelled");
        assert_eq!(acknowledgement["payload"]["disposition"], "acknowledged");

        route.abort();
        match route.await {
            Err(error) => assert!(error.is_cancelled()),
            Ok(_) => panic!("route was not cancelled"),
        }
        assert!(dropped.load(Ordering::Acquire));
        assert_eq!(
            *scopes.outcomes.lock().expect("outcome lock"),
            vec![RequestOutcome::Cancelled]
        );
    });
}

#[test]
fn resource_limits_bound_async_and_blocking_tokio_work() {
    runtime().block_on(async {
        let spawner = TokioWorkerSpawner::current(TokioWorkerLimits {
            max_tasks: 1,
            max_blocking_tasks: 1,
        })
        .expect("entered Tokio runtime");

        let mut pending_task = spawner
            .spawn(async { pending::<Result<(), WorkerError>>().await })
            .expect("first task admitted");
        let error = match spawner.spawn(async { Ok(()) }) {
            Err(error) => error,
            Ok(_) => panic!("second task must exceed the finite budget"),
        };
        assert_eq!(error.code(), "OVERLOADED");
        assert_eq!(error.to_string(), "Tokio worker capacity is full");
        pending_task.cancel();
        pending_task
            .shutdown()
            .await
            .expect("aborted async task joined");

        let observed = Arc::new(AtomicBool::new(false));
        let started = Arc::new(Notify::new());
        let observed_by_work = Arc::clone(&observed);
        let started_by_work = Arc::clone(&started);
        let mut blocking = spawner
            .spawn_blocking(move |cancellation| {
                started_by_work.notify_one();
                while !cancellation.is_cancelled() {
                    std::thread::yield_now();
                }
                observed_by_work.store(true, Ordering::Release);
                Ok(())
            })
            .expect("blocking task admitted");
        started.notified().await;
        let error = match spawner.spawn_blocking(|_| Ok(())) {
            Err(error) => error,
            Ok(_) => panic!("second blocking task must exceed the finite budget"),
        };
        assert_eq!(error.code(), "OVERLOADED");
        blocking.cancel();
        blocking
            .shutdown()
            .await
            .expect("blocking task observed cancellation and joined");
        assert!(observed.load(Ordering::Acquire));
    });
}

#[test]
fn dropping_tokio_work_requests_abort_or_cooperative_cancellation() {
    runtime().block_on(async {
        let spawner = TokioWorkerSpawner::current(TokioWorkerLimits {
            max_tasks: 1,
            max_blocking_tasks: 1,
        })
        .expect("entered Tokio runtime");

        let async_started = Arc::new(Notify::new());
        let async_dropped = Arc::new(Notify::new());
        let started_by_task = Arc::clone(&async_started);
        let dropped_by_task = Arc::clone(&async_dropped);
        let task = spawner
            .spawn(async move {
                let _drop = NotifyDrop(dropped_by_task);
                started_by_task.notify_one();
                pending::<Result<(), WorkerError>>().await
            })
            .expect("async task admitted");
        async_started.notified().await;
        drop(task);
        async_dropped.notified().await;
        let task = spawner
            .spawn(async { Ok(()) })
            .expect("async permit released");
        task.shutdown()
            .await
            .expect("replacement async task joined");

        let blocking_started = Arc::new(Notify::new());
        let blocking_finished = Arc::new(Notify::new());
        let started_by_work = Arc::clone(&blocking_started);
        let finished_by_work = Arc::clone(&blocking_finished);
        let blocking = spawner
            .spawn_blocking(move |cancellation| {
                started_by_work.notify_one();
                while !cancellation.is_cancelled() {
                    std::thread::yield_now();
                }
                finished_by_work.notify_one();
                Ok(())
            })
            .expect("blocking task admitted");
        blocking_started.notified().await;
        drop(blocking);
        blocking_finished.notified().await;
        let task = spawner
            .spawn_blocking(|_| Ok(()))
            .expect("blocking permit released");
        task.shutdown()
            .await
            .expect("replacement blocking task joined");
    });
}

#[test]
fn send_sync_static_and_configuration_boundaries_are_compile_checked() {
    fn assert_send_sync_static<T: Send + Sync + 'static>() {}
    assert_send_sync_static::<AxumWorker>();
    assert_send_sync_static::<TokioWorkerSpawner>();
    assert_send_sync_static::<axum::Router>();

    let (worker, _) = worker((), greet);
    let error = match AxumWorker::new(
        worker,
        AxumWorkerConfig {
            session_id: "credential secret".to_owned(),
            maximum_frame_bytes: 0,
        },
    ) {
        Err(error) => error,
        Ok(_) => panic!("invalid public configuration must fail"),
    };
    assert_eq!(error.code(), "WORKER_CONFIG_INVALID");
    assert_eq!(error.to_string(), "Axum worker configuration is invalid");
    assert!(!error.to_string().contains("credential"));

    let runtime = runtime();
    let error = match TokioWorkerSpawner::new(
        runtime.handle().clone(),
        TokioWorkerLimits {
            max_tasks: 0,
            max_blocking_tasks: 1,
        },
    ) {
        Err(error) => error,
        Ok(_) => panic!("zero task budget must fail"),
    };
    assert_eq!(error.code(), "WORKER_CONFIG_INVALID");

    let error = match TokioWorkerSpawner::current(TokioWorkerLimits::default()) {
        Err(error) => error,
        Ok(_) => panic!("a Tokio handle must not be invented outside a runtime"),
    };
    assert_eq!(error.code(), "WORKER_RUNTIME_UNAVAILABLE");
    assert_eq!(error.to_string(), "Tokio worker runtime is unavailable");
}

#[test]
fn response_payload_type_is_the_existing_worker_result() {
    fn assert_owned_send_static<T: Send + 'static>() {}
    assert_owned_send_static::<WorkerResult>();
}
