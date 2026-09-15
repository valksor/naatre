#![cfg(feature = "server")]

use naatre_sdk::generated::{
    GetAccountHandler, GetAccountResult, GetAccountResultProfile, GetAccountVariables, Status,
    register_get_account_handler,
};
use naatre_sdk::{
    CancelRequest, CancellationDisposition, HandlerContext, HandlerDescriptor, HandlerFuture,
    Optional, OwnedWork, Parent, Presence, Principal, Registration, RequestOutcome,
    RequestResources, RequestScopeFactory, SchemaValidator, Selected, ServerBuilder, WorkerError,
    WorkerInvocation, WorkerResult, decode_worker_frame, encode_worker_frame,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::any::Any;
use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RemoteFixture {
    registration: Registration,
    operations: Vec<FixtureOperation>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct FixtureOperation {
    name: String,
    input: GreetInput,
    expected: FixtureExpected,
}

#[derive(Debug, Deserialize)]
struct FixtureExpected {
    data: Value,
    errors: Vec<WorkerError>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
struct GreetInput {
    name: String,
}

#[derive(Debug, Serialize)]
struct GreetOutput {
    greeting: String,
}

#[derive(Default)]
struct FixtureValidator {
    reject_output: bool,
}

impl SchemaValidator for FixtureValidator {
    fn validate_input(&self, schema: &str, input: &Value) -> Result<(), WorkerError> {
        let valid_greet = schema == "GreetInput"
            && input
                .get("name")
                .and_then(Value::as_str)
                .is_some_and(|name| !name.is_empty());
        let valid_generated = schema == "GetAccountVariables" && input.get("id").is_some();
        if valid_greet || valid_generated {
            Ok(())
        } else {
            Err(WorkerError::new(
                "INPUT_INVALID",
                "input failed schema validation",
                false,
            ))
        }
    }

    fn validate_output(&self, schema: &str, output: &Value) -> Result<(), WorkerError> {
        if !self.reject_output
            && ((schema == "GreetOutput" && output.get("greeting").is_some())
                || (schema == "GetAccountResult" && output.is_object()))
        {
            Ok(())
        } else {
            Err(WorkerError::new(
                "OUTPUT_COMPLETION",
                "output failed schema validation",
                false,
            ))
        }
    }
}

struct Resources {
    principal: Principal,
    loader: usize,
    transaction: usize,
    finished: Arc<Mutex<Vec<(usize, RequestOutcome)>>>,
}

impl RequestResources for Resources {
    fn principal(&self) -> &Principal {
        &self.principal
    }

    fn loader(&mut self) -> &mut (dyn Any + Send) {
        &mut self.loader
    }

    fn transaction(&mut self) -> Option<&mut (dyn Any + Send)> {
        Some(&mut self.transaction)
    }

    fn finish(self: Box<Self>, outcome: RequestOutcome) {
        self.finished.lock().unwrap().push((self.loader, outcome));
    }
}

#[derive(Default)]
struct Scopes {
    next: AtomicUsize,
    finished: Arc<Mutex<Vec<(usize, RequestOutcome)>>>,
}

impl RequestScopeFactory for Scopes {
    fn begin(
        &self,
        invocation: &WorkerInvocation,
    ) -> Result<Box<dyn RequestResources>, WorkerError> {
        if invocation.delegated_context != "valid-delegation" {
            return Err(WorkerError::new(
                "UNAUTHORIZED",
                "delegated context was rejected",
                false,
            ));
        }
        let id = self.next.fetch_add(1, Ordering::Relaxed) + 1;
        Ok(Box::new(Resources {
            principal: Principal {
                subject: format!("subject-{id}"),
                tenant: format!("tenant-{id}"),
            },
            loader: id,
            transaction: id + 100,
            finished: Arc::clone(&self.finished),
        }))
    }
}

fn invocation(id: &str, input: Value) -> WorkerInvocation {
    WorkerInvocation {
        protocol: "naatre.remote-worker.v1".to_owned(),
        request_id: format!("request-{id}"),
        invocation_id: format!("invocation-{id}"),
        attempt_id: format!("invocation-{id}.1"),
        handler_id: "fixture.greet".to_owned(),
        schema_revision: "schema-1".to_owned(),
        deadline_unix_milli: 4_102_444_800_000,
        idempotency_key: String::new(),
        delegated_context: "valid-delegation".to_owned(),
        parent: Parent::default(),
        input,
        resume_cursor: String::new(),
    }
}

fn greet_registration() -> Registration {
    let fixture: RemoteFixture = serde_json::from_slice(include_bytes!(
        "../../../conformance/v1/remote-workers.json"
    ))
    .unwrap();
    fixture.registration
}

fn greet(_context: &mut HandlerContext, input: GreetInput) -> HandlerFuture<'_, GreetOutput> {
    Box::pin(async move {
        if input.name == "reject" {
            Err(WorkerError::new(
                "NAME_REJECTED",
                "name was rejected",
                false,
            ))
        } else {
            Ok(GreetOutput {
                greeting: format!("Hello, {}", input.name),
            })
        }
    })
}

fn greet_worker(
    validator: Arc<dyn SchemaValidator>,
    scopes: Arc<Scopes>,
) -> naatre_sdk::WorkerCore {
    let mut builder = ServerBuilder::new(greet_registration(), validator, scopes).unwrap();
    builder
        .register_handler(
            HandlerDescriptor::unary("fixture.greet", "GreetInput", "GreetOutput", "query"),
            (),
            |(), context, input| greet(context, input),
        )
        .unwrap();
    builder.build().unwrap()
}

fn ready<F>(future: &mut F) -> F::Output
where
    F: Future + Unpin,
{
    let mut context = Context::from_waker(Waker::noop());
    match Pin::new(future).poll(&mut context) {
        Poll::Ready(output) => output,
        Poll::Pending => panic!("fixture future unexpectedly remained pending"),
    }
}

#[test]
fn rust_worker_matches_go_public_fixture_data_and_errors() {
    let fixture: RemoteFixture = serde_json::from_slice(include_bytes!(
        "../../../conformance/v1/remote-workers.json"
    ))
    .unwrap();
    let worker = greet_worker(
        Arc::new(FixtureValidator::default()),
        Arc::new(Scopes::default()),
    );
    for operation in fixture.operations {
        let mut future = worker
            .invoke(invocation(
                &operation.name,
                serde_json::to_value(operation.input).unwrap(),
            ))
            .unwrap();
        let result = ready(&mut future).unwrap();
        assert_eq!(
            result.errors, operation.expected.errors,
            "{}",
            operation.name
        );
        match result.data {
            Presence::Value(data) => {
                assert_eq!(data, operation.expected.data, "{}", operation.name);
            }
            Presence::Null if operation.expected.data.is_null() => {}
            other => panic!("unexpected fixture data for {}: {other:?}", operation.name),
        }
    }
}

#[test]
fn malformed_output_and_frames_never_become_public_worker_data() {
    let worker = greet_worker(
        Arc::new(FixtureValidator {
            reject_output: true,
        }),
        Arc::new(Scopes::default()),
    );
    let mut future = worker
        .invoke(invocation("malformed", json!({"name": "Ada"})))
        .unwrap();
    assert_eq!(ready(&mut future).unwrap_err().code(), "OUTPUT_COMPLETION");

    let result = WorkerResult {
        protocol: "naatre.remote-worker.v1".to_owned(),
        invocation_id: "invocation-frame".to_owned(),
        attempt_id: "invocation-frame.1".to_owned(),
        schema_revision: "schema-1".to_owned(),
        data: Presence::Null,
        errors: Vec::new(),
        references: Vec::new(),
    };
    let frame = encode_worker_frame(&result, 4096).unwrap();
    assert_eq!(
        decode_worker_frame::<WorkerResult>(&frame, 4096).unwrap(),
        result
    );
    for malformed in [
        vec![2, 0, 0, 0, 1, b'{'],
        vec![0, 0, 0, 0, 0],
        vec![0, 0, 0, 0, 2, b'{'],
        vec![0, 0, 0, 0, 2, b'{', b'}', b'x'],
    ] {
        assert_eq!(
            decode_worker_frame::<WorkerResult>(&malformed, 4096)
                .unwrap_err()
                .code(),
            "REMOTE_WORKER_MALFORMED"
        );
    }
}

#[test]
fn explicit_registration_is_the_only_public_dispatch_path() {
    let registration = greet_registration();
    let validator: Arc<dyn SchemaValidator> = Arc::new(FixtureValidator::default());
    let scopes = Arc::new(Scopes::default());
    let builder =
        ServerBuilder::new(registration.clone(), Arc::clone(&validator), scopes.clone()).unwrap();
    let Err(error) = builder.build() else {
        panic!("an unbound manifest became public")
    };
    assert_eq!(error.code(), "REMOTE_REGISTRATION_INVALID");

    let mut builder = ServerBuilder::new(registration, validator, scopes).unwrap();
    assert_eq!(
        builder
            .register_handler::<GreetInput, GreetOutput, _>(
                HandlerDescriptor::unary("not.registered", "GreetInput", "GreetOutput", "query"),
                (),
                |(), context, input| greet(context, input),
            )
            .unwrap_err()
            .code(),
        "REMOTE_HANDLER_UNKNOWN"
    );
}

struct GeneratedHandler;

impl GetAccountHandler for GeneratedHandler {
    fn handle_get_account<'a>(
        &'a self,
        _context: &'a mut HandlerContext,
        _input: GetAccountVariables,
    ) -> HandlerFuture<'a, GetAccountResult> {
        Box::pin(async {
            let unknown = Status::Unknown("FUTURE".to_owned());
            assert_eq!(serde_json::to_value(unknown).unwrap(), json!("FUTURE"));
            Ok(GetAccountResult {
                profile: Selected::Present(GetAccountResultProfile {
                    display: Selected::Present("Ada".to_owned()),
                    nickname: Selected::Null,
                }),
                later: Selected::Present("done".to_owned()),
            })
        })
    }
}

#[test]
fn generated_trait_registration_preserves_missing_null_scalars_and_open_variants() {
    let descriptor = HandlerDescriptor::unary(
        "GetAccount",
        "GetAccountVariables",
        "GetAccountResult",
        "query",
    );
    let mut registration = greet_registration();
    registration.handlers = vec![descriptor];
    let mut builder = ServerBuilder::new(
        registration,
        Arc::new(FixtureValidator::default()),
        Arc::new(Scopes::default()),
    )
    .unwrap();
    register_get_account_handler(&mut builder, GeneratedHandler).unwrap();
    let worker = builder.build().unwrap();
    let mut request = invocation(
        "generated",
        json!({"id": "acct-1", "nickname": null, "tags": []}),
    );
    request.handler_id = "GetAccount".to_owned();
    let mut future = worker.invoke(request).unwrap();
    let output = ready(&mut future).unwrap();
    let Presence::Value(data) = output.data else {
        panic!("generated handler omitted data")
    };
    assert_eq!(data["profile"]["nickname"], Value::Null);
    assert_eq!(data["later"], json!("done"));
}

#[derive(Clone)]
struct WorkState {
    cancelled: Arc<AtomicUsize>,
    joined: Arc<AtomicUsize>,
}

impl OwnedWork for WorkState {
    fn cancel(&mut self) {
        self.cancelled.fetch_add(1, Ordering::Relaxed);
    }

    fn shutdown(self: Box<Self>) -> HandlerFuture<'static, ()> {
        self.joined.fetch_add(1, Ordering::Relaxed);
        Box::pin(async { Ok(()) })
    }
}

#[test]
fn spawned_blocking_and_stream_work_is_cancelled_and_joined_on_shutdown() {
    let cancelled = Arc::new(AtomicUsize::new(0));
    let joined = Arc::new(AtomicUsize::new(0));
    let work = WorkState {
        cancelled: Arc::clone(&cancelled),
        joined: Arc::clone(&joined),
    };
    let mut builder = ServerBuilder::new(
        greet_registration(),
        Arc::new(FixtureValidator::default()),
        Arc::new(Scopes::default()),
    )
    .unwrap();
    builder
        .register_handler(
            HandlerDescriptor::unary("fixture.greet", "GreetInput", "GreetOutput", "query"),
            work,
            |work, context, input: GreetInput| {
                context.own_spawned_task(Box::new(work.clone()));
                context.own_blocking_task(Box::new(work.clone()));
                context.own_source_stream(Box::new(work.clone()));
                greet(context, input)
            },
        )
        .unwrap();
    let worker = builder.build().unwrap();
    let mut future = worker
        .invoke(invocation("shutdown", json!({"name": "Ada"})))
        .unwrap();
    ready(&mut future).unwrap();
    assert_eq!(cancelled.load(Ordering::Relaxed), 3);
    assert_eq!(joined.load(Ordering::Relaxed), 3);
}

#[test]
fn dropping_a_future_requests_cancellation_without_claiming_join_or_rollback() {
    let cancelled = Arc::new(AtomicUsize::new(0));
    let joined = Arc::new(AtomicUsize::new(0));
    let work = WorkState {
        cancelled: Arc::clone(&cancelled),
        joined: Arc::clone(&joined),
    };
    let scopes = Arc::new(Scopes::default());
    let mut builder = ServerBuilder::new(
        greet_registration(),
        Arc::new(FixtureValidator::default()),
        scopes.clone(),
    )
    .unwrap();
    builder
        .register_handler(
            HandlerDescriptor::unary("fixture.greet", "GreetInput", "GreetOutput", "query"),
            work,
            |work, context, _input: GreetInput| {
                context.own_spawned_task(Box::new(work.clone()));
                Box::pin(std::future::pending::<Result<GreetOutput, WorkerError>>())
            },
        )
        .unwrap();
    let worker = builder.build().unwrap();
    let mut future = worker
        .invoke(invocation("drop", json!({"name": "Ada"})))
        .unwrap();
    let mut context = Context::from_waker(Waker::noop());
    assert!(matches!(
        Pin::new(&mut future).poll(&mut context),
        Poll::Pending
    ));
    assert_eq!(
        worker
            .cancel(&CancelRequest {
                protocol: "naatre.remote-worker.v1".to_owned(),
                request_id: "request-someone-else".to_owned(),
                invocation_id: "invocation-drop".to_owned(),
            })
            .unwrap_err()
            .code(),
        "REMOTE_CANCELLATION_INVALID"
    );
    assert_eq!(
        worker
            .cancel(&CancelRequest {
                protocol: "naatre.remote-worker.v1".to_owned(),
                request_id: "request-drop".to_owned(),
                invocation_id: "invocation-drop".to_owned(),
            })
            .unwrap()
            .disposition,
        CancellationDisposition::Acknowledged
    );
    drop(future);
    assert_eq!(cancelled.load(Ordering::Relaxed), 1);
    assert_eq!(joined.load(Ordering::Relaxed), 0);
    assert_eq!(
        scopes.finished.lock().unwrap().as_slice(),
        &[(1, RequestOutcome::Cancelled)]
    );
}

#[test]
fn concurrent_invocations_receive_distinct_principal_loader_and_transaction_state() {
    let seen = Arc::new(Mutex::new(Vec::new()));
    let scopes = Arc::new(Scopes::default());
    let mut registration = greet_registration();
    registration.limits.max_in_flight = 2;
    let mut builder =
        ServerBuilder::new(registration, Arc::new(FixtureValidator::default()), scopes).unwrap();
    builder
        .register_handler::<GreetInput, GreetOutput, _>(
            HandlerDescriptor::unary("fixture.greet", "GreetInput", "GreetOutput", "query"),
            Arc::clone(&seen),
            |seen, context, input: GreetInput| {
                let principal = context.principal().clone();
                let loader = *context.loader().downcast_ref::<usize>().unwrap();
                let transaction = *context
                    .transaction()
                    .unwrap()
                    .downcast_ref::<usize>()
                    .unwrap();
                seen.lock().unwrap().push((principal, loader, transaction));
                greet(context, input)
            },
        )
        .unwrap();
    let worker = builder.build().unwrap();
    let mut first = worker
        .invoke(invocation("first", json!({"name": "Ada"})))
        .unwrap();
    let mut second = worker
        .invoke(invocation("second", json!({"name": "Grace"})))
        .unwrap();
    ready(&mut first).unwrap();
    ready(&mut second).unwrap();
    let seen = seen.lock().unwrap();
    assert_eq!(seen.len(), 2);
    assert_ne!(seen[0].0, seen[1].0);
    assert_ne!(seen[0].1, seen[1].1);
    assert_ne!(seen[0].2, seen[1].2);
}

#[test]
fn unwind_profile_contains_panics_but_abort_profile_is_process_failure() {
    let scopes = Arc::new(Scopes::default());
    let mut builder = ServerBuilder::new(
        greet_registration(),
        Arc::new(FixtureValidator::default()),
        scopes.clone(),
    )
    .unwrap();
    builder
        .register_handler::<GreetInput, GreetOutput, _>(
            HandlerDescriptor::unary("fixture.greet", "GreetInput", "GreetOutput", "query"),
            (),
            |(), _context, _input: GreetInput| Box::pin(async { panic!("fixture panic") }),
        )
        .unwrap();
    let worker = builder.build().unwrap();
    let mut future = worker
        .invoke(invocation("panic", json!({"name": "Ada"})))
        .unwrap();
    assert_eq!(ready(&mut future).unwrap_err().code(), "INTERNAL");
    assert_eq!(
        scopes.finished.lock().unwrap().as_slice(),
        &[(1, RequestOutcome::Panicked)]
    );

    let manifest = include_str!("../Cargo.toml");
    assert!(manifest.contains("[profile.release]\npanic = \"abort\""));
    assert!(manifest.contains("[profile.unwind]\ninherits = \"dev\"\npanic = \"unwind\""));
}

#[test]
fn request_input_is_owned_before_handler_polling() {
    let value = String::from("Ada");
    let mut input = BTreeMap::new();
    input.insert("name", value);
    let worker = greet_worker(
        Arc::new(FixtureValidator::default()),
        Arc::new(Scopes::default()),
    );
    let request = invocation("owned", serde_json::to_value(input).unwrap());
    let mut future = worker.invoke(request).unwrap();
    assert!(matches!(
        ready(&mut future).unwrap().data,
        Presence::Value(_)
    ));
}

#[test]
fn generated_input_preserves_missing_and_null_before_dispatch() {
    let input = GetAccountVariables {
        filter: Optional::Missing,
        id: "acct-1".to_owned(),
        nickname: Presence::Null,
        tags: Optional::Missing,
    };
    assert_eq!(
        serde_json::to_value(input).unwrap(),
        json!({"id": "acct-1", "nickname": null})
    );
}
