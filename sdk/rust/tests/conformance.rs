use naatre_sdk::generated::{self, GetAccountVariables, Status};
use naatre_sdk::{
    BigInt, Bytes, Cancellation, Client, ClientError, Decimal, Duration, FallibleStream, Int64,
    OpenUnion, Operation, OperationKind, Optional, Page, PageInfo, PageRequest, Paginator,
    Presence, RemoteError, RequestOptions, Selected, StreamTransport, Timestamp, Transport, UInt64,
    Uuid,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};
use std::time::{Duration as StdDuration, Instant};

#[derive(Debug, Deserialize)]
struct ScalarVectors {
    vectors: Vec<ScalarVector>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ScalarVector {
    name: String,
    kind: String,
    input: String,
    canonical: Option<String>,
    valid: bool,
}

#[test]
fn lossless_scalars_pass_every_shared_vector_in_scope() {
    let vectors: ScalarVectors =
        serde_json::from_slice(include_bytes!("../../../conformance/v1/scalars.json")).unwrap();
    let mut covered = 0;
    for vector in vectors.vectors {
        let outcome = match vector.kind.as_str() {
            "Int64" => scalar_outcome::<Int64>(&vector.input),
            "UInt64" => scalar_outcome::<UInt64>(&vector.input),
            "BigInt" => scalar_outcome::<BigInt>(&vector.input),
            "Decimal" => scalar_outcome::<Decimal>(&vector.input),
            "Timestamp" => scalar_outcome::<Timestamp>(&vector.input),
            "Duration" => scalar_outcome::<Duration>(&vector.input),
            "UUID" => scalar_outcome::<Uuid>(&vector.input),
            "Bytes" => scalar_outcome::<Bytes>(&vector.input),
            _ => continue,
        };
        covered += 1;
        if vector.valid {
            assert_eq!(
                outcome.unwrap(),
                vector.canonical.unwrap(),
                "{}",
                vector.name
            );
        } else {
            assert!(outcome.is_err(), "{}", vector.name);
        }
    }
    assert_eq!(covered, 25);
}

fn scalar_outcome<T>(input: &str) -> Result<String, serde_json::Error>
where
    T: for<'de> Deserialize<'de> + Serialize,
{
    let value: T = serde_json::from_str(input)?;
    serde_json::to_string(&value)
}

#[derive(Debug, Default, Deserialize, PartialEq, Serialize)]
struct OptionalValue {
    #[serde(default, skip_serializing_if = "Presence::is_missing")]
    value: Presence<String>,
}

#[derive(Debug, Default, Deserialize, PartialEq)]
struct SelectedValue {
    #[serde(default)]
    value: Selected<String>,
}

#[derive(Debug, Default, Deserialize, PartialEq)]
struct PendingValue {
    #[serde(default = "Selected::pending")]
    value: Selected<String>,
}

#[derive(Debug, Default, Deserialize, Serialize)]
struct NonNullOptional {
    #[serde(default, skip_serializing_if = "Optional::is_missing")]
    value: Optional<String>,
}

#[test]
fn missing_null_and_value_remain_distinct() {
    let missing: OptionalValue = serde_json::from_str("{}").unwrap();
    let null: OptionalValue = serde_json::from_str(r#"{"value":null}"#).unwrap();
    let value: OptionalValue = serde_json::from_str(r#"{"value":"Ada"}"#).unwrap();
    assert_eq!(missing.value, Presence::Missing);
    assert_eq!(null.value, Presence::Null);
    assert_eq!(value.value, Presence::Value("Ada".to_owned()));
    assert_eq!(serde_json::to_string(&missing).unwrap(), "{}");
    assert_eq!(serde_json::to_string(&null).unwrap(), r#"{"value":null}"#);
    assert_eq!(serde_json::to_string(&value).unwrap(), r#"{"value":"Ada"}"#);

    assert_eq!(
        serde_json::from_str::<SelectedValue>("{}").unwrap().value,
        Selected::Missing
    );
    assert_eq!(
        serde_json::from_str::<SelectedValue>(r#"{"value":null}"#)
            .unwrap()
            .value,
        Selected::Null
    );
    assert_eq!(
        serde_json::from_str::<SelectedValue>(r#"{"value":"Ada"}"#)
            .unwrap()
            .value,
        Selected::Present("Ada".to_owned())
    );
    assert_eq!(
        serde_json::from_str::<PendingValue>("{}").unwrap().value,
        Selected::Pending
    );

    assert!(serde_json::from_str::<NonNullOptional>(r#"{"value":null}"#).is_err());
    assert_eq!(
        serde_json::to_string(&NonNullOptional::default()).unwrap(),
        "{}"
    );
}

#[test]
fn result_lifecycle_and_pagination_values_are_typed_and_fallible() {
    let failure = RemoteError {
        code: "PARTIAL".to_owned(),
        message: "late failure".to_owned(),
        path: vec![json!("account")],
        extensions: BTreeMap::new(),
    };
    assert!(matches!(
        Selected::<String>::Failed(vec![failure]),
        Selected::Failed(errors) if errors[0].code == "PARTIAL"
    ));
    assert_eq!(
        Selected::<String>::Skipped("dependency failed".to_owned()),
        Selected::Skipped("dependency failed".to_owned())
    );

    let request = PageRequest {
        after: None,
        first: 25,
    };
    assert_eq!(serde_json::to_value(request).unwrap(), json!({"first": 25}));
    let page: Page<String> = serde_json::from_value(json!({
        "items": ["a", "b"],
        "pageInfo": {"endCursor": null, "hasNextPage": false}
    }))
    .unwrap();
    assert_eq!(page.items, ["a", "b"]);
    assert_eq!(
        page.page_info,
        PageInfo {
            end_cursor: Presence::Null,
            has_next_page: false,
        }
    );

    let mut paginator = Paginator::new(25, 3).unwrap();
    paginator
        .advance(&PageInfo {
            end_cursor: Presence::Value("cursor-1".to_owned()),
            has_next_page: true,
        })
        .unwrap();
    assert_eq!(
        paginator.request().unwrap().after.as_deref(),
        Some("cursor-1")
    );
    assert_eq!(
        paginator
            .advance(&PageInfo {
                end_cursor: Presence::Value("cursor-1".to_owned()),
                has_next_page: true,
            })
            .unwrap_err()
            .code(),
        "CLIENT_PAGINATION_CURSOR_INVALID"
    );
}

#[test]
fn generated_open_enum_and_raw_union_preserve_unknown_values() {
    let status: Status = serde_json::from_str(r#""FUTURE""#).unwrap();
    assert_eq!(status, Status::Unknown("FUTURE".to_owned()));
    assert_eq!(serde_json::to_string(&status).unwrap(), r#""FUTURE""#);

    let union: OpenUnion =
        serde_json::from_str(r#"{"$type":"FutureAccount","$value":{"id":"1"}}"#).unwrap();
    assert_eq!(union.discriminator(), "FutureAccount");
    assert_eq!(union.value(), &json!({"id": "1"}));
    assert!(!union.is_known());
    assert_eq!(
        serde_json::to_value(&union).unwrap(),
        json!({"$type": "FutureAccount", "$value": {"id": "1"}})
    );
}

#[test]
fn generated_request_and_persisted_hash_match_shared_reference() {
    let reference: Value = serde_json::from_slice(include_bytes!(
        "../../../conformance/v1/generator-output.json"
    ))
    .unwrap();
    let operation = generated::create_get_account().unwrap();
    let variables = GetAccountVariables {
        filter: Optional::Value(BTreeMap::new()),
        id: "acct-1".to_owned(),
        nickname: Presence::Null,
        tags: Optional::Value(Vec::new()),
    };
    let request: Value =
        serde_json::from_slice(&operation.request_bytes(&variables).unwrap()).unwrap();
    assert_eq!(request, reference["operations"][0]["request"]);
    assert_eq!(
        operation.persisted().digest,
        reference["operations"][0]["persisted"]["digest"]
    );
    let manifest = generated::manifest().unwrap();
    assert_eq!(manifest.operations[0].persisted, *operation.persisted());
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
struct Payload {
    name: String,
}

#[derive(Clone, Debug, Serialize)]
struct Variables {
    id: String,
}

fn payload_operation() -> Operation<Variables, Payload> {
    Operation::new(
        "Payload",
        OperationKind::Query,
        naatre_sdk::PersistedReference::new("0".repeat(64)).unwrap(),
        |input| {
            serde_json::from_slice(input)
                .map_err(|_| ClientError::new("CLIENT_RESULT_INVALID", "invalid result"))
        },
    )
}

#[test]
fn partial_data_errors_and_response_limits_follow_the_client_profile() {
    let operation = payload_operation();
    let partial = operation
        .decode_result(
            br#"{"data":{"name":"Ada"},"errors":[{"code":"PARTIAL","message":"late failure"}],"complete":false}"#,
        )
        .unwrap();
    assert_eq!(
        partial.data,
        Presence::Value(Payload {
            name: "Ada".to_owned()
        })
    );
    assert_eq!(partial.errors[0].code, "PARTIAL");
    assert!(!partial.complete);

    for malformed in [
        br#"{"data":{"name":"first"},"data":{"name":"second"},"complete":true}"#.as_slice(),
        br#"{"data":{"name":"Ada"},"complete":true} trailing"#.as_slice(),
        br#"{"data":{"name":"Ada"}"#.as_slice(),
        br#"{"data":{"name":"Ada"},"complete":true,"unknown":1}"#.as_slice(),
    ] {
        assert_eq!(
            operation.decode_result(malformed).unwrap_err().code(),
            "CLIENT_PROTOCOL_INVALID"
        );
    }
    let oversized = vec![b' '; naatre_sdk::MAXIMUM_RESPONSE_BYTES + 1];
    assert_eq!(
        operation.decode_result(&oversized).unwrap_err().code(),
        "CLIENT_RESPONSE_TOO_LARGE"
    );
}

#[derive(Clone)]
struct PendingTransport {
    cancellation: Arc<Mutex<Option<Cancellation>>>,
    released: Arc<AtomicBool>,
    calls: Arc<AtomicUsize>,
}

struct PendingWork {
    released: Arc<AtomicBool>,
}

impl Future for PendingWork {
    type Output = Result<Vec<u8>, ClientError>;

    fn poll(self: Pin<&mut Self>, _context: &mut Context<'_>) -> Poll<Self::Output> {
        Poll::Pending
    }
}

impl Drop for PendingWork {
    fn drop(&mut self) {
        self.released.store(true, Ordering::Release);
    }
}

impl Transport for PendingTransport {
    type Future<'a> = PendingWork;

    fn execute(
        &self,
        _body: Vec<u8>,
        _options: RequestOptions,
        cancellation: Cancellation,
    ) -> Self::Future<'_> {
        self.calls.fetch_add(1, Ordering::Relaxed);
        *self.cancellation.lock().unwrap() = Some(cancellation);
        PendingWork {
            released: Arc::clone(&self.released),
        }
    }
}

struct PendingByteStream {
    released: Arc<AtomicBool>,
}

impl FallibleStream for PendingByteStream {
    type Item = Vec<u8>;

    fn poll_next(
        self: Pin<&mut Self>,
        _context: &mut Context<'_>,
    ) -> Poll<Option<Result<Self::Item, ClientError>>> {
        Poll::Pending
    }
}

impl Drop for PendingByteStream {
    fn drop(&mut self) {
        self.released.store(true, Ordering::Release);
    }
}

impl StreamTransport for PendingTransport {
    type Stream<'a> = PendingByteStream;

    fn stream(
        &self,
        _body: Vec<u8>,
        _options: RequestOptions,
        cancellation: Cancellation,
    ) -> Self::Stream<'_> {
        self.calls.fetch_add(1, Ordering::Relaxed);
        *self.cancellation.lock().unwrap() = Some(cancellation);
        PendingByteStream {
            released: Arc::clone(&self.released),
        }
    }
}

fn pending_transport() -> PendingTransport {
    PendingTransport {
        cancellation: Arc::new(Mutex::new(None)),
        released: Arc::new(AtomicBool::new(false)),
        calls: Arc::new(AtomicUsize::new(0)),
    }
}

#[test]
fn dropping_requests_and_streams_cancels_detached_work_and_releases_resources() {
    let transport = pending_transport();
    let client = Client::new(transport.clone());
    let operation = payload_operation();
    let request = client
        .execute(
            &operation,
            &Variables { id: "1".to_owned() },
            RequestOptions::default(),
        )
        .unwrap();
    drop(request);
    assert!(
        transport
            .cancellation
            .lock()
            .unwrap()
            .as_ref()
            .unwrap()
            .is_cancelled()
    );
    assert!(transport.released.load(Ordering::Acquire));

    transport.released.store(false, Ordering::Release);
    let operation = payload_operation();
    let stream = client
        .stream(
            &operation,
            &Variables { id: "1".to_owned() },
            RequestOptions::default(),
        )
        .unwrap();
    drop(stream);
    assert!(
        transport
            .cancellation
            .lock()
            .unwrap()
            .as_ref()
            .unwrap()
            .is_cancelled()
    );
    assert!(transport.released.load(Ordering::Acquire));
}

#[test]
fn elapsed_deadlines_do_not_start_transport_work() {
    let transport = pending_transport();
    let client = Client::new(transport.clone());
    let options = RequestOptions {
        deadline: Some(
            Instant::now()
                .checked_sub(StdDuration::from_secs(1))
                .unwrap(),
        ),
    };
    let operation = payload_operation();
    let Err(error) = client.execute(&operation, &Variables { id: "1".to_owned() }, options) else {
        panic!("elapsed deadline started transport work");
    };
    assert_eq!(error.code(), "CLIENT_DEADLINE_EXCEEDED");
    assert_eq!(transport.calls.load(Ordering::Relaxed), 0);
}

struct OneFrame(Option<Vec<u8>>);

impl FallibleStream for OneFrame {
    type Item = Vec<u8>;

    fn poll_next(
        mut self: Pin<&mut Self>,
        _context: &mut Context<'_>,
    ) -> Poll<Option<Result<Self::Item, ClientError>>> {
        Poll::Ready(self.0.take().map(Ok))
    }
}

#[test]
fn oversized_stream_frames_fail_and_cancel_the_transport() {
    let cancellation = Cancellation::new();
    let mut stream = naatre_sdk::StreamHandle::new(
        OneFrame(Some(vec![0; naatre_sdk::MAXIMUM_FRAME_BYTES + 1])),
        cancellation.clone(),
    );
    let mut context = Context::from_waker(Waker::noop());
    let result = Pin::new(&mut stream).poll_next(&mut context);
    let Poll::Ready(Some(Err(error))) = result else {
        panic!("oversized stream frame was not rejected");
    };
    assert_eq!(error.code(), "CLIENT_FRAME_TOO_LARGE");
    assert!(cancellation.is_cancelled());
    assert!(matches!(
        Pin::new(&mut stream).poll_next(&mut context),
        Poll::Ready(None)
    ));
}

#[test]
fn generated_schema_types_are_send_and_sync() {
    fn assert_send_sync<T: Send + Sync>() {}
    assert_send_sync::<Status>();
    assert_send_sync::<GetAccountVariables>();
    assert_send_sync::<generated::GetAccountResult>();
    assert_send_sync::<OpenUnion>();
    assert_send_sync::<Decimal>();
}
