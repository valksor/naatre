use crate::{Cancellation, Presence, decode_json};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::any::Any;
use std::collections::{BTreeMap, BTreeSet};
use std::future::Future;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::time::{SystemTime, UNIX_EPOCH};

pub const WORKER_PROTOCOL: &str = "naatre.remote-worker.v1";
pub const WORKER_CODEC: &str = "naatre.json-1";
const CAPABILITY_UNARY: &str = "unary-1";
const KNOWN_CAPABILITIES: [&str; 8] = [
    CAPABILITY_UNARY,
    "client-streaming-1",
    "server-streaming-1",
    "bidirectional-streaming-1",
    "cancellation-ack-1",
    "idempotency-replay-1",
    "transaction-provider-1",
    "subscription-resume-1",
];

pub type HandlerFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T, WorkerError>> + Send + 'a>>;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ServerError {
    code: &'static str,
    message: &'static str,
}

impl ServerError {
    const fn new(code: &'static str, message: &'static str) -> Self {
        Self { code, message }
    }

    #[must_use]
    pub const fn code(&self) -> &'static str {
        self.code
    }
}

impl std::fmt::Display for ServerError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.message)
    }
}

impl std::error::Error for ServerError {}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct WorkerLimits {
    pub max_in_flight: u32,
    pub max_request_bytes: u32,
    pub max_response_bytes: u32,
    pub max_stream_frames: u32,
    pub max_stream_bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct HandlerDescriptor {
    pub id: String,
    pub input_schema: String,
    pub output_schema: String,
    pub codec: String,
    pub effect: String,
    #[serde(default)]
    pub required_capabilities: Vec<String>,
}

impl HandlerDescriptor {
    #[must_use]
    pub fn unary(
        id: impl Into<String>,
        input_schema: impl Into<String>,
        output_schema: impl Into<String>,
        effect: impl Into<String>,
    ) -> Self {
        Self {
            id: id.into(),
            input_schema: input_schema.into(),
            output_schema: output_schema.into(),
            codec: WORKER_CODEC.to_owned(),
            effect: effect.into(),
            required_capabilities: Vec::new(),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Registration {
    pub protocol: String,
    pub worker_id: String,
    pub service_identity: String,
    pub audience: String,
    pub endpoint: String,
    pub schema_revision: String,
    pub schema_digest: String,
    pub capabilities: Vec<String>,
    pub limits: WorkerLimits,
    pub handlers: Vec<HandlerDescriptor>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct RegistrationAck {
    pub protocol: String,
    pub worker_id: String,
    pub session_id: String,
    pub schema_revision: String,
    pub accepted_capabilities: Vec<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Parent {
    #[serde(default, skip_serializing_if = "Presence::is_missing")]
    pub value: Presence<Value>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reference: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub owner_invocation_id: String,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct WorkerInvocation {
    pub protocol: String,
    pub request_id: String,
    pub invocation_id: String,
    pub attempt_id: String,
    pub handler_id: String,
    pub schema_revision: String,
    pub deadline_unix_milli: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub idempotency_key: String,
    pub delegated_context: String,
    #[serde(default, skip_serializing_if = "parent_is_empty")]
    pub parent: Parent,
    pub input: Value,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub resume_cursor: String,
}

fn parent_is_empty(parent: &Parent) -> bool {
    parent.value.is_missing()
        && parent.reference.is_empty()
        && parent.owner_invocation_id.is_empty()
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct WorkerError {
    pub code: String,
    pub message: String,
    pub retryable: bool,
    #[serde(default, skip_serializing_if = "Presence::is_missing")]
    pub details: Presence<Value>,
}

impl WorkerError {
    #[must_use]
    pub fn new(code: impl Into<String>, message: impl Into<String>, retryable: bool) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            retryable,
            details: Presence::Missing,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct WorkerResult {
    pub protocol: String,
    pub invocation_id: String,
    pub attempt_id: String,
    pub schema_revision: String,
    #[serde(default, skip_serializing_if = "Presence::is_missing")]
    pub data: Presence<Value>,
    #[serde(default)]
    pub errors: Vec<WorkerError>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub references: Vec<Value>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum CancellationDisposition {
    Requested,
    Acknowledged,
    TooLate,
    Unsupported,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct CancelRequest {
    pub protocol: String,
    pub request_id: String,
    pub invocation_id: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct CancellationAck {
    pub protocol: String,
    pub invocation_id: String,
    pub disposition: CancellationDisposition,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Principal {
    pub subject: String,
    pub tenant: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RequestOutcome {
    Succeeded,
    Failed,
    Cancelled,
    Panicked,
}

/// Application-owned request resources. The worker stores this value only in
/// one invocation context and consumes it exactly once at terminal cleanup.
pub trait RequestResources: Send {
    fn principal(&self) -> &Principal;
    fn loader(&mut self) -> &mut (dyn Any + Send);
    fn transaction(&mut self) -> Option<&mut (dyn Any + Send)>;
    fn finish(self: Box<Self>, outcome: RequestOutcome);
}

pub trait RequestScopeFactory: Send + Sync {
    /// Creates fresh principal, loader, and transaction state for one request.
    ///
    /// # Errors
    ///
    /// Returns a public worker error after application trust validation fails.
    fn begin(
        &self,
        invocation: &WorkerInvocation,
    ) -> Result<Box<dyn RequestResources>, WorkerError>;
}

pub trait SchemaValidator: Send + Sync {
    /// Validates decoded input against the registered shared-schema identity.
    ///
    /// # Errors
    ///
    /// Returns an error before application code runs.
    fn validate_input(&self, schema: &str, input: &Value) -> Result<(), WorkerError>;

    /// Validates encoded handler output before it becomes worker data.
    ///
    /// # Errors
    ///
    /// Returns an error instead of emitting malformed output.
    fn validate_output(&self, schema: &str, output: &Value) -> Result<(), WorkerError>;
}

/// Executor-owned task, blocking job, or source stream retained by a request.
/// `cancel` must be idempotent; `shutdown` must join or otherwise establish a
/// terminal state without assuming cancellation rolled back external writes.
pub trait OwnedWork: Send {
    fn cancel(&mut self);
    fn shutdown(self: Box<Self>) -> HandlerFuture<'static, ()>;
}

struct WorkGuard {
    work: Option<Box<dyn OwnedWork>>,
}

impl WorkGuard {
    fn new(work: Box<dyn OwnedWork>) -> Self {
        Self { work: Some(work) }
    }

    async fn shutdown(mut self) -> Result<(), WorkerError> {
        if let Some(mut work) = self.work.take() {
            work.cancel();
            work.shutdown().await?;
        }
        Ok(())
    }
}

impl Drop for WorkGuard {
    fn drop(&mut self) {
        if let Some(work) = self.work.as_mut() {
            let _ = catch_unwind(AssertUnwindSafe(|| work.cancel()));
        }
    }
}

pub struct HandlerContext {
    cancellation: Cancellation,
    resources: Option<Box<dyn RequestResources>>,
    owned_work: Vec<WorkGuard>,
}

impl HandlerContext {
    fn new(cancellation: Cancellation, resources: Box<dyn RequestResources>) -> Self {
        Self {
            cancellation,
            resources: Some(resources),
            owned_work: Vec::new(),
        }
    }

    #[must_use]
    pub fn cancellation(&self) -> Cancellation {
        self.cancellation.clone()
    }

    #[must_use]
    pub fn principal(&self) -> &Principal {
        self.resources().principal()
    }

    pub fn loader(&mut self) -> &mut (dyn Any + Send) {
        self.resources_mut().loader()
    }

    pub fn transaction(&mut self) -> Option<&mut (dyn Any + Send)> {
        self.resources_mut().transaction()
    }

    fn resources(&self) -> &dyn RequestResources {
        self.resources
            .as_deref()
            .expect("request resources exist until terminal cleanup")
    }

    fn resources_mut(&mut self) -> &mut dyn RequestResources {
        self.resources
            .as_deref_mut()
            .expect("request resources exist until terminal cleanup")
    }

    pub fn own_spawned_task(&mut self, task: Box<dyn OwnedWork>) {
        self.owned_work.push(WorkGuard::new(task));
    }

    pub fn own_blocking_task(&mut self, task: Box<dyn OwnedWork>) {
        self.owned_work.push(WorkGuard::new(task));
    }

    pub fn own_source_stream(&mut self, stream: Box<dyn OwnedWork>) {
        self.owned_work.push(WorkGuard::new(stream));
    }

    async fn finish(mut self, outcome: RequestOutcome) -> Result<(), WorkerError> {
        let mut cleanup_error = None;
        while let Some(work) = self.owned_work.pop() {
            if let Err(error) = work.shutdown().await {
                cleanup_error.get_or_insert(error);
            }
        }
        if let Some(resources) = self.resources.take() {
            resources.finish(if cleanup_error.is_some() {
                RequestOutcome::Failed
            } else {
                outcome
            });
        }
        cleanup_error.map_or(Ok(()), Err)
    }
}

impl Drop for HandlerContext {
    fn drop(&mut self) {
        if let Some(resources) = self.resources.take() {
            let outcome = if self.cancellation.is_cancelled() {
                RequestOutcome::Cancelled
            } else {
                RequestOutcome::Panicked
            };
            let _ = catch_unwind(AssertUnwindSafe(|| resources.finish(outcome)));
        }
    }
}

trait ErasedHandler: Send + Sync {
    fn invoke<'a>(
        &'a self,
        context: &'a mut HandlerContext,
        input: Value,
    ) -> HandlerFuture<'a, Value>;
}

struct TypedHandler<H, I, O> {
    handler: H,
    invoke: for<'a> fn(&'a H, &'a mut HandlerContext, I) -> HandlerFuture<'a, O>,
    marker: std::marker::PhantomData<fn(I) -> O>,
}

impl<H, I, O> ErasedHandler for TypedHandler<H, I, O>
where
    H: Send + Sync,
    I: DeserializeOwned + Send + 'static,
    O: Serialize + Send + 'static,
{
    fn invoke<'a>(
        &'a self,
        context: &'a mut HandlerContext,
        input: Value,
    ) -> HandlerFuture<'a, Value> {
        let parsed = serde_json::from_value(input).map_err(|_| {
            WorkerError::new(
                "REMOTE_INVOCATION_INVALID",
                "handler input does not satisfy its generated type",
                false,
            )
        });
        Box::pin(async move {
            let output = (self.invoke)(&self.handler, context, parsed?).await?;
            serde_json::to_value(output).map_err(|_| {
                WorkerError::new(
                    "OUTPUT_COMPLETION",
                    "handler output cannot be encoded",
                    false,
                )
            })
        })
    }
}

pub struct ServerBuilder {
    registration: Registration,
    validator: Arc<dyn SchemaValidator>,
    scopes: Arc<dyn RequestScopeFactory>,
    handlers: BTreeMap<String, Arc<dyn ErasedHandler>>,
}

impl ServerBuilder {
    /// Starts explicit server registration from an application-owned manifest.
    ///
    /// # Errors
    ///
    /// Returns a stable registration error when protocol, schema, capability,
    /// or finite-limit fields do not satisfy the remote-worker contract.
    pub fn new(
        registration: Registration,
        validator: Arc<dyn SchemaValidator>,
        scopes: Arc<dyn RequestScopeFactory>,
    ) -> Result<Self, ServerError> {
        validate_registration(&registration)?;
        Ok(Self {
            registration,
            validator,
            scopes,
            handlers: BTreeMap::new(),
        })
    }

    /// Registers one typed handler. Merely implementing a generated handler
    /// trait never exposes it; this call and a matching manifest row are both
    /// required.
    ///
    /// # Errors
    ///
    /// Rejects handlers absent from the manifest, descriptor mismatches, and
    /// duplicate registration.
    pub fn register_handler<I, O, H>(
        &mut self,
        descriptor: HandlerDescriptor,
        handler: H,
        invoke: for<'a> fn(&'a H, &'a mut HandlerContext, I) -> HandlerFuture<'a, O>,
    ) -> Result<(), ServerError>
    where
        H: Send + Sync + 'static,
        I: DeserializeOwned + Send + 'static,
        O: Serialize + Send + 'static,
    {
        let Some(expected) = self
            .registration
            .handlers
            .iter()
            .find(|candidate| candidate.id == descriptor.id)
        else {
            return Err(ServerError::new(
                "REMOTE_HANDLER_UNKNOWN",
                "handler is absent from the registration manifest",
            ));
        };
        if expected != &descriptor || self.handlers.contains_key(&descriptor.id) {
            return Err(ServerError::new(
                "REMOTE_REGISTRATION_INVALID",
                "handler registration does not match its manifest",
            ));
        }
        self.handlers.insert(
            descriptor.id,
            Arc::new(TypedHandler {
                handler,
                invoke,
                marker: std::marker::PhantomData,
            }),
        );
        Ok(())
    }

    /// Completes registration only when every advertised handler is bound.
    ///
    /// # Errors
    ///
    /// Returns a registration error for an unbound public manifest row.
    pub fn build(self) -> Result<WorkerCore, ServerError> {
        if self.handlers.len() != self.registration.handlers.len() {
            return Err(ServerError::new(
                "REMOTE_REGISTRATION_INVALID",
                "every advertised handler must be explicitly registered",
            ));
        }
        Ok(WorkerCore {
            registration: self.registration,
            validator: self.validator,
            scopes: self.scopes,
            handlers: self.handlers,
            active: Arc::new(Mutex::new(BTreeMap::new())),
        })
    }
}

pub struct WorkerCore {
    registration: Registration,
    validator: Arc<dyn SchemaValidator>,
    scopes: Arc<dyn RequestScopeFactory>,
    handlers: BTreeMap<String, Arc<dyn ErasedHandler>>,
    active: Arc<Mutex<BTreeMap<String, ActiveInvocation>>>,
}

#[derive(Clone)]
struct ActiveInvocation {
    request_id: String,
    cancellation: Cancellation,
}

impl WorkerCore {
    #[must_use]
    pub const fn registration(&self) -> &Registration {
        &self.registration
    }

    #[must_use]
    pub fn registration_ack(&self, session_id: impl Into<String>) -> RegistrationAck {
        RegistrationAck {
            protocol: WORKER_PROTOCOL.to_owned(),
            worker_id: self.registration.worker_id.clone(),
            session_id: session_id.into(),
            schema_revision: self.registration.schema_revision.clone(),
            accepted_capabilities: self.registration.capabilities.clone(),
        }
    }

    /// Admits and starts one invocation without selecting an executor.
    ///
    /// # Errors
    ///
    /// Rejects invalid identities, unknown handlers, schema-invalid input,
    /// duplicate invocation IDs, and request-scope creation failures before
    /// application code is polled.
    pub fn invoke(&self, invocation: WorkerInvocation) -> Result<InvocationFuture, ServerError> {
        validate_invocation(&self.registration, &invocation)?;
        let descriptor = self
            .registration
            .handlers
            .iter()
            .find(|candidate| candidate.id == invocation.handler_id)
            .ok_or_else(|| ServerError::new("REMOTE_HANDLER_UNKNOWN", "handler is not registered"))?
            .clone();
        let handler = self.registered_handler(&invocation.handler_id)?;
        self.validator
            .validate_input(&descriptor.input_schema, &invocation.input)
            .map_err(|_| {
                ServerError::new(
                    "REMOTE_INVOCATION_INVALID",
                    "handler input does not satisfy its schema",
                )
            })?;
        let cancellation = Cancellation::new();
        self.reserve(&invocation, &cancellation)?;
        let Ok(resources) = self.scopes.begin(&invocation) else {
            self.release(&invocation.invocation_id);
            return Err(ServerError::new(
                "UNAUTHORIZED",
                "request scope creation was rejected",
            ));
        };
        let validator = Arc::clone(&self.validator);
        let active = Arc::clone(&self.active);
        let invocation_id = invocation.invocation_id.clone();
        let attempt_id = invocation.attempt_id.clone();
        let schema_revision = invocation.schema_revision.clone();
        let input = invocation.input.clone();
        let cancellation_for_future = cancellation.clone();
        let maximum_response_bytes = self.registration.limits.max_response_bytes as usize;
        let future = Box::pin(async move {
            let mut context = HandlerContext::new(cancellation_for_future, resources);
            let result = handler.invoke(&mut context, input).await;
            let (data, errors, outcome) = match result {
                Ok(output) => {
                    if validator
                        .validate_output(&descriptor.output_schema, &output)
                        .is_ok()
                    {
                        (
                            Presence::Value(output),
                            Vec::new(),
                            RequestOutcome::Succeeded,
                        )
                    } else {
                        let _ = context.finish(RequestOutcome::Failed).await;
                        return Err(ServerError::new(
                            "OUTPUT_COMPLETION",
                            "handler output does not satisfy its schema",
                        ));
                    }
                }
                Err(error) => {
                    if !valid_worker_error(&error, maximum_response_bytes) {
                        let _ = context.finish(RequestOutcome::Failed).await;
                        return Err(ServerError::new(
                            "REMOTE_WORKER_MALFORMED",
                            "handler returned an invalid worker error",
                        ));
                    }
                    (Presence::Null, vec![error], RequestOutcome::Failed)
                }
            };
            let result = WorkerResult {
                protocol: WORKER_PROTOCOL.to_owned(),
                invocation_id,
                attempt_id,
                schema_revision,
                data,
                errors,
                references: Vec::new(),
            };
            if serde_json::to_vec(&result)
                .map_or(true, |encoded| encoded.len() > maximum_response_bytes)
            {
                let _ = context.finish(RequestOutcome::Failed).await;
                return Err(ServerError::new(
                    "OUTPUT_COMPLETION",
                    "handler output exceeds the registered limit",
                ));
            }
            context.finish(outcome).await.map_err(|_| {
                ServerError::new("INTERNAL", "owned request work failed to shut down")
            })?;
            Ok(result)
        });
        Ok(InvocationFuture {
            future,
            cancellation,
            active,
            key: invocation.invocation_id,
            complete: false,
        })
    }

    fn reserve(
        &self,
        invocation: &WorkerInvocation,
        cancellation: &Cancellation,
    ) -> Result<(), ServerError> {
        let mut active = self
            .active
            .lock()
            .map_err(|_| ServerError::new("INTERNAL", "active invocation state is unavailable"))?;
        if active.len() >= self.registration.limits.max_in_flight as usize {
            return Err(ServerError::new("OVERLOADED", "worker capacity is full"));
        }
        if active.contains_key(&invocation.invocation_id) {
            return Err(ServerError::new(
                "REMOTE_INVOCATION_DUPLICATE",
                "invocation ID is already active",
            ));
        }
        active.insert(
            invocation.invocation_id.clone(),
            ActiveInvocation {
                request_id: invocation.request_id.clone(),
                cancellation: cancellation.clone(),
            },
        );
        Ok(())
    }

    fn registered_handler(&self, handler_id: &str) -> Result<Arc<dyn ErasedHandler>, ServerError> {
        self.handlers
            .get(handler_id)
            .cloned()
            .ok_or_else(|| ServerError::new("REMOTE_HANDLER_UNKNOWN", "handler is not registered"))
    }

    fn release(&self, invocation_id: &str) {
        if let Ok(mut active) = self.active.lock() {
            active.remove(invocation_id);
        }
    }

    /// Requests cooperative cancellation of the matching active request.
    ///
    /// # Errors
    ///
    /// Rejects malformed, cross-request, inactive, or unnegotiated requests.
    pub fn cancel(&self, request: &CancelRequest) -> Result<CancellationAck, ServerError> {
        if request.protocol != WORKER_PROTOCOL
            || !valid_identifier(&request.request_id)
            || !valid_identifier(&request.invocation_id)
            || !self
                .registration
                .capabilities
                .iter()
                .any(|capability| capability == "cancellation-ack-1")
        {
            return Err(ServerError::new(
                "REMOTE_CANCELLATION_INVALID",
                "cancellation request is invalid",
            ));
        }
        let active = self
            .active
            .lock()
            .map_err(|_| ServerError::new("INTERNAL", "active invocation state is unavailable"))?;
        let Some(invocation) = active.get(&request.invocation_id) else {
            return Err(ServerError::new(
                "REMOTE_CANCELLATION_INVALID",
                "invocation is not active",
            ));
        };
        if invocation.request_id != request.request_id {
            return Err(ServerError::new(
                "REMOTE_CANCELLATION_INVALID",
                "cancellation request does not own the invocation",
            ));
        }
        invocation.cancellation.cancel();
        Ok(CancellationAck {
            protocol: WORKER_PROTOCOL.to_owned(),
            invocation_id: request.invocation_id.clone(),
            disposition: CancellationDisposition::Acknowledged,
        })
    }
}

pub struct InvocationFuture {
    future: Pin<Box<dyn Future<Output = Result<WorkerResult, ServerError>> + Send>>,
    cancellation: Cancellation,
    active: Arc<Mutex<BTreeMap<String, ActiveInvocation>>>,
    key: String,
    complete: bool,
}

impl Future for InvocationFuture {
    type Output = Result<WorkerResult, ServerError>;

    fn poll(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Self::Output> {
        let polled = catch_unwind(AssertUnwindSafe(|| self.future.as_mut().poll(context)));
        match polled {
            Ok(Poll::Ready(result)) => {
                self.complete = true;
                if let Ok(mut active) = self.active.lock() {
                    active.remove(&self.key);
                }
                Poll::Ready(result)
            }
            Ok(Poll::Pending) => Poll::Pending,
            Err(_) => {
                self.cancellation.cancel();
                self.complete = true;
                let replacement: Pin<
                    Box<dyn Future<Output = Result<WorkerResult, ServerError>> + Send>,
                > = Box::pin(async {
                    Err(ServerError::new(
                        "INTERNAL",
                        "handler panicked at the unwind boundary",
                    ))
                });
                let panicked = std::mem::replace(&mut self.future, replacement);
                let _ = catch_unwind(AssertUnwindSafe(|| drop(panicked)));
                if let Ok(mut active) = self.active.lock() {
                    active.remove(&self.key);
                }
                Poll::Ready(Err(ServerError::new(
                    "INTERNAL",
                    "handler panicked at the unwind boundary",
                )))
            }
        }
    }
}

impl Drop for InvocationFuture {
    fn drop(&mut self) {
        if !self.complete {
            self.cancellation.cancel();
            if let Ok(mut active) = self.active.lock() {
                active.remove(&self.key);
            }
        }
    }
}

/// Encodes one remote-worker JSON frame without selecting an I/O transport.
///
/// # Errors
///
/// Rejects an empty payload or payloads larger than the configured bound.
pub fn encode_worker_frame<T: Serialize>(
    value: &T,
    maximum_bytes: usize,
) -> Result<Vec<u8>, ServerError> {
    let payload = serde_json::to_vec(value)
        .map_err(|_| ServerError::new("REMOTE_WORKER_MALFORMED", "worker JSON is invalid"))?;
    if payload.is_empty() || payload.len() > maximum_bytes || payload.len() > u32::MAX as usize {
        return Err(ServerError::new(
            "REMOTE_WORKER_MALFORMED",
            "worker frame exceeds its configured limit",
        ));
    }
    let mut frame = Vec::with_capacity(payload.len() + 5);
    frame.push(0);
    let size = u32::try_from(payload.len()).map_err(|_| {
        ServerError::new(
            "REMOTE_WORKER_MALFORMED",
            "worker frame exceeds its configured limit",
        )
    })?;
    frame.extend_from_slice(&size.to_be_bytes());
    frame.extend_from_slice(&payload);
    Ok(frame)
}

/// Strictly decodes exactly one remote-worker JSON frame.
///
/// # Errors
///
/// Rejects unknown flags, zero/truncated/oversized payloads, trailing bytes,
/// duplicate JSON keys, and type-invalid JSON.
pub fn decode_worker_frame<T: DeserializeOwned>(
    frame: &[u8],
    maximum_bytes: usize,
) -> Result<T, ServerError> {
    if frame.len() < 5 || frame[0] != 0 {
        return Err(ServerError::new(
            "REMOTE_WORKER_MALFORMED",
            "worker frame header is invalid",
        ));
    }
    let size = u32::from_be_bytes([frame[1], frame[2], frame[3], frame[4]]) as usize;
    if size == 0 || size > maximum_bytes || frame.len() != size + 5 {
        return Err(ServerError::new(
            "REMOTE_WORKER_MALFORMED",
            "worker frame length is invalid",
        ));
    }
    decode_json(&frame[5..])
        .map_err(|_| ServerError::new("REMOTE_WORKER_MALFORMED", "worker frame JSON is invalid"))
}

fn validate_registration(registration: &Registration) -> Result<(), ServerError> {
    if registration.protocol != WORKER_PROTOCOL
        || !valid_identifier(&registration.worker_id)
        || registration.service_identity.is_empty()
        || !valid_identifier(&registration.audience)
        || !valid_identifier(&registration.endpoint)
        || !valid_identifier(&registration.schema_revision)
        || registration.schema_digest.len() != 64
        || !registration
            .schema_digest
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        || registration.limits.max_in_flight == 0
        || registration.limits.max_request_bytes == 0
        || registration.limits.max_response_bytes == 0
        || registration.limits.max_stream_frames == 0
        || registration.limits.max_stream_bytes == 0
        || registration.handlers.is_empty()
    {
        return Err(ServerError::new(
            "REMOTE_REGISTRATION_INVALID",
            "worker registration is invalid",
        ));
    }
    let capabilities: BTreeSet<_> = registration.capabilities.iter().collect();
    if capabilities.len() != registration.capabilities.len()
        || !capabilities
            .iter()
            .all(|capability| KNOWN_CAPABILITIES.contains(&capability.as_str()))
        || !capabilities.contains(&CAPABILITY_UNARY.to_owned())
    {
        return Err(ServerError::new(
            "REMOTE_CAPABILITY_MISMATCH",
            "worker capabilities are invalid",
        ));
    }
    let mut handlers = BTreeSet::new();
    for handler in &registration.handlers {
        if !valid_identifier(&handler.id)
            || !valid_identifier(&handler.input_schema)
            || !valid_identifier(&handler.output_schema)
            || handler.codec != WORKER_CODEC
            || !matches!(
                handler.effect.as_str(),
                "query" | "mutation" | "transaction" | "subscription"
            )
            || !handlers.insert(handler.id.as_str())
            || !handler
                .required_capabilities
                .iter()
                .all(|capability| capabilities.contains(capability))
            || (handler.effect == "transaction"
                && !capabilities.contains(&"transaction-provider-1".to_owned()))
        {
            return Err(ServerError::new(
                "REMOTE_CAPABILITY_MISMATCH",
                "worker handler manifest is invalid",
            ));
        }
    }
    Ok(())
}

fn validate_invocation(
    registration: &Registration,
    invocation: &WorkerInvocation,
) -> Result<(), ServerError> {
    let input_size = serde_json::to_vec(&invocation.input)
        .map_err(|_| ServerError::new("REMOTE_INVOCATION_INVALID", "input is invalid"))?
        .len();
    let now_unix_milli = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|duration| i64::try_from(duration.as_millis()).ok())
        .unwrap_or(i64::MAX);
    if invocation.protocol != WORKER_PROTOCOL
        || invocation.schema_revision != registration.schema_revision
        || !valid_identifier(&invocation.request_id)
        || !valid_identifier(&invocation.invocation_id)
        || !valid_identifier(&invocation.attempt_id)
        || !valid_identifier(&invocation.handler_id)
        || invocation.deadline_unix_milli <= now_unix_milli
        || invocation.delegated_context.is_empty()
        || input_size > registration.limits.max_request_bytes as usize
        || (!invocation.parent.value.is_missing() && !invocation.parent.reference.is_empty())
        || (invocation.parent.reference.is_empty()
            && !invocation.parent.owner_invocation_id.is_empty())
    {
        return Err(ServerError::new(
            "REMOTE_INVOCATION_INVALID",
            "worker invocation is invalid",
        ));
    }
    Ok(())
}

fn valid_identifier(value: &str) -> bool {
    let mut bytes = value.bytes();
    value.len() <= 128
        && bytes.next().is_some_and(|byte| byte.is_ascii_alphabetic())
        && bytes
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b':' | b'-'))
}

fn valid_worker_error(error: &WorkerError, maximum_bytes: usize) -> bool {
    const RESERVED: [&str; 14] = [
        "CANCELLED",
        "INTERNAL",
        "OVERLOADED",
        "OUTPUT_COMPLETION",
        "UNAUTHORIZED",
        "REMOTE_CAPABILITY_MISMATCH",
        "REMOTE_INVOCATION_DUPLICATE",
        "REMOTE_INVOCATION_INVALID",
        "REMOTE_WORKER_MALFORMED",
        "REMOTE_OUTCOME_INDETERMINATE",
        "REMOTE_SCHEMA_MISMATCH",
        "REMOTE_REFERENCE_UNAVAILABLE",
        "REMOTE_UNAUTHENTICATED",
        "REMOTE_HANDLER_UNKNOWN",
    ];
    let valid_code = (3..=64).contains(&error.code.len())
        && error
            .code
            .bytes()
            .next()
            .is_some_and(|byte| byte.is_ascii_uppercase())
        && error
            .code
            .bytes()
            .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit() || byte == b'_');
    let valid_details = match &error.details {
        Presence::Missing => true,
        Presence::Value(Value::Object(details)) => details.keys().all(|key| key.contains('.')),
        Presence::Null | Presence::Value(_) => false,
    };
    valid_code
        && !RESERVED.contains(&error.code.as_str())
        && !error.message.trim().is_empty()
        && error.message.len() <= maximum_bytes
        && valid_details
}
