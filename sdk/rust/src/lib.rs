#![forbid(unsafe_code)]

//! Runtime-neutral Rust client types for the Naatre v1 wire contract.
//!
//! The core crate owns no executor and starts no task. Concrete HTTP, SSE,
//! WebSocket, and Tokio adapters are intentionally outside this profile.

mod scalar;
mod transport;

#[cfg(feature = "generator")]
pub mod generator;

#[rustfmt::skip]
#[path = "../generated/operations.rs"]
pub mod generated;

use serde::de::{DeserializeOwned, MapAccess, SeqAccess, Visitor};
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use serde_json::{Map, Number, Value};
use std::collections::{BTreeMap, BTreeSet};
use std::error::Error;
use std::fmt::{self, Display, Formatter};
use std::marker::PhantomData;

pub use scalar::{BigInt, Bytes, Decimal, Duration, Int64, Timestamp, UInt64, Uuid};
pub use transport::{
    Cancellation, Client, FallibleStream, OperationFuture, RequestHandle, RequestOptions,
    StreamHandle, StreamTransport, Transport,
};

pub const MAXIMUM_RESPONSE_BYTES: usize = 8 << 20;
pub const MAXIMUM_FRAME_BYTES: usize = 1 << 20;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ClientError {
    code: &'static str,
    message: &'static str,
}

impl ClientError {
    #[must_use]
    pub const fn new(code: &'static str, message: &'static str) -> Self {
        Self { code, message }
    }

    #[must_use]
    pub const fn code(&self) -> &'static str {
        self.code
    }
}

impl Display for ClientError {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl Error for ClientError {}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub enum Presence<T> {
    #[default]
    Missing,
    Null,
    Value(T),
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub enum Optional<T> {
    #[default]
    Missing,
    Value(T),
}

impl<T> Optional<T> {
    #[must_use]
    pub const fn is_missing(&self) -> bool {
        matches!(self, Self::Missing)
    }
}

impl<T: Serialize> Serialize for Optional<T> {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        match self {
            Self::Missing => Err(serde::ser::Error::custom(
                "missing optional value must be omitted by its owning field",
            )),
            Self::Value(value) => value.serialize(serializer),
        }
    }
}

impl<'de, T: Deserialize<'de>> Deserialize<'de> for Optional<T> {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        Option::<T>::deserialize(deserializer)?
            .map(Self::Value)
            .ok_or_else(|| serde::de::Error::custom("optional value must not be null"))
    }
}

impl<T> Presence<T> {
    #[must_use]
    pub const fn is_missing(&self) -> bool {
        matches!(self, Self::Missing)
    }

    #[must_use]
    pub const fn as_ref(&self) -> Presence<&T> {
        match self {
            Self::Missing => Presence::Missing,
            Self::Null => Presence::Null,
            Self::Value(value) => Presence::Value(value),
        }
    }
}

impl<T: Serialize> Serialize for Presence<T> {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        match self {
            Self::Missing => Err(serde::ser::Error::custom(
                "missing presence must be omitted by its owning field",
            )),
            Self::Null => serializer.serialize_none(),
            Self::Value(value) => value.serialize(serializer),
        }
    }
}

impl<'de, T: Deserialize<'de>> Deserialize<'de> for Presence<T> {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        Option::<T>::deserialize(deserializer).map(|value| value.map_or(Self::Null, Self::Value))
    }
}

#[derive(Clone, Debug, Default, PartialEq)]
pub enum Selected<T> {
    #[default]
    Missing,
    Null,
    Pending,
    Present(T),
    Failed(Vec<RemoteError>),
    Skipped(String),
}

impl<T> Selected<T> {
    #[must_use]
    pub const fn pending() -> Self {
        Self::Pending
    }
}

impl<T: Serialize> Serialize for Selected<T> {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        match self {
            Self::Null => serializer.serialize_none(),
            Self::Present(value) => value.serialize(serializer),
            Self::Missing | Self::Pending | Self::Failed(_) | Self::Skipped(_) => Err(
                serde::ser::Error::custom("non-value selection has no domain JSON representation"),
            ),
        }
    }
}

impl<'de, T: Deserialize<'de>> Deserialize<'de> for Selected<T> {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        Option::<T>::deserialize(deserializer).map(|value| value.map_or(Self::Null, Self::Present))
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OpenUnion {
    #[serde(rename = "$type")]
    discriminator: String,
    #[serde(rename = "$value")]
    value: Value,
    #[serde(skip)]
    known: bool,
}

impl OpenUnion {
    #[must_use]
    pub fn new(discriminator: impl Into<String>, value: Value, known: bool) -> Self {
        Self {
            discriminator: discriminator.into(),
            value,
            known,
        }
    }

    #[must_use]
    pub fn discriminator(&self) -> &str {
        &self.discriminator
    }

    #[must_use]
    pub const fn value(&self) -> &Value {
        &self.value
    }

    #[must_use]
    pub const fn is_known(&self) -> bool {
        self.known
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum OperationKind {
    Query,
    Mutation,
    Subscription,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PersistedReference {
    pub algorithm: String,
    #[serde(rename = "canonicalVersion")]
    pub canonical_version: String,
    pub digest: String,
}

impl PersistedReference {
    /// Builds a canonical SHA-256 persisted-operation reference.
    ///
    /// # Errors
    ///
    /// Returns `CLIENT_PERSISTED_REFERENCE_INVALID` unless `digest` is exactly
    /// 64 lowercase hexadecimal characters.
    pub fn new(digest: impl Into<String>) -> Result<Self, ClientError> {
        let digest = digest.into();
        if digest.len() != 64
            || !digest
                .as_bytes()
                .iter()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(byte))
        {
            return Err(ClientError::new(
                "CLIENT_PERSISTED_REFERENCE_INVALID",
                "persisted reference is invalid",
            ));
        }
        Ok(Self {
            algorithm: "sha-256".to_owned(),
            canonical_version: "c14n-1".to_owned(),
            digest,
        })
    }
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RemoteError {
    pub code: String,
    pub message: String,
    #[serde(default)]
    pub path: Vec<Value>,
    #[serde(default)]
    pub extensions: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OperationResult<T> {
    #[serde(default)]
    pub data: Presence<T>,
    #[serde(default)]
    pub errors: Vec<RemoteError>,
    pub complete: bool,
}

#[derive(Clone, Debug)]
pub struct Operation<V, R> {
    name: &'static str,
    kind: OperationKind,
    persisted: PersistedReference,
    decode: fn(&[u8]) -> Result<R, ClientError>,
    marker: PhantomData<fn(V) -> R>,
}

impl<V, R> Operation<V, R> {
    #[must_use]
    pub fn new(
        name: &'static str,
        kind: OperationKind,
        persisted: PersistedReference,
        decode: fn(&[u8]) -> Result<R, ClientError>,
    ) -> Self {
        Self {
            name,
            kind,
            persisted,
            decode,
            marker: PhantomData,
        }
    }

    #[must_use]
    pub const fn name(&self) -> &'static str {
        self.name
    }

    #[must_use]
    pub const fn kind(&self) -> &OperationKind {
        &self.kind
    }

    #[must_use]
    pub const fn persisted(&self) -> &PersistedReference {
        &self.persisted
    }

    /// Encodes the canonical persisted-operation request envelope.
    ///
    /// # Errors
    ///
    /// Returns a typed client error if variables cannot be serialized or do
    /// not encode as a JSON object.
    pub fn request_bytes(&self, variables: &V) -> Result<Vec<u8>, ClientError>
    where
        V: Serialize,
    {
        let variables = serde_json::to_value(variables).map_err(|_| {
            ClientError::new(
                "CLIENT_VARIABLES_INVALID",
                "operation variables are invalid",
            )
        })?;
        if !variables.is_object() {
            return Err(ClientError::new(
                "CLIENT_VARIABLES_INVALID",
                "operation variables must encode as an object",
            ));
        }
        let mut request = BTreeMap::new();
        request.insert(
            "operation",
            serde_json::to_value(self.name).map_err(json_error)?,
        );
        request.insert(
            "persisted",
            serde_json::to_value(&self.persisted).map_err(json_error)?,
        );
        request.insert("variables", variables);
        request.insert("version", Value::String("1".to_owned()));
        serde_json::to_vec(&request).map_err(json_error)
    }

    /// Strictly decodes a bounded operation-result envelope.
    ///
    /// # Errors
    ///
    /// Returns a typed protocol error for oversized, malformed, duplicate-key,
    /// unknown-control-field, or operation-specific invalid responses.
    pub fn decode_result(&self, input: &[u8]) -> Result<OperationResult<R>, ClientError> {
        if input.len() > MAXIMUM_RESPONSE_BYTES {
            return Err(ClientError::new(
                "CLIENT_RESPONSE_TOO_LARGE",
                "operation response exceeds the configured limit",
            ));
        }
        let envelope: WireOperationResult = decode_json(input)?;
        let data = match envelope.data {
            Presence::Missing => Presence::Missing,
            Presence::Null => Presence::Null,
            Presence::Value(value) => Presence::Value((self.decode)(
                &serde_json::to_vec(&value).map_err(json_error)?,
            )?),
        };
        Ok(OperationResult {
            data,
            errors: envelope.errors,
            complete: envelope.complete,
        })
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct WireOperationResult {
    #[serde(default)]
    data: Presence<Value>,
    #[serde(default)]
    errors: Vec<RemoteError>,
    complete: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ManifestOperation {
    pub name: String,
    pub kind: OperationKind,
    pub persisted: PersistedReference,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Manifest {
    pub profile: String,
    pub version: String,
    #[serde(rename = "protocolVersion")]
    pub protocol_version: String,
    #[serde(rename = "canonicalVersion")]
    pub canonical_version: String,
    pub operations: Vec<ManifestOperation>,
}

impl Manifest {
    /// Serializes the deterministic persisted-operation manifest.
    ///
    /// # Errors
    ///
    /// Returns a typed JSON error if the manifest cannot be represented.
    pub fn canonical_json(&self) -> Result<Vec<u8>, ClientError> {
        let value = serde_json::to_value(self).map_err(json_error)?;
        serde_json::to_vec(&value).map_err(json_error)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PageRequest {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub after: Option<String>,
    pub first: u32,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PageInfo {
    #[serde(default, skip_serializing_if = "Presence::is_missing")]
    #[serde(rename = "endCursor")]
    pub end_cursor: Presence<String>,
    #[serde(rename = "hasNextPage")]
    pub has_next_page: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Page<T> {
    pub items: Vec<T>,
    #[serde(rename = "pageInfo")]
    pub page_info: PageInfo,
}

#[derive(Clone, Debug)]
pub struct Paginator {
    request: Option<PageRequest>,
    maximum_pages: usize,
    completed_pages: usize,
    cursors: BTreeSet<String>,
}

impl Paginator {
    /// Creates a bounded forward-only pagination state machine.
    ///
    /// # Errors
    ///
    /// Returns `CLIENT_PAGINATION_INVALID` when either bound is zero.
    pub fn new(first: u32, maximum_pages: usize) -> Result<Self, ClientError> {
        if first == 0 || maximum_pages == 0 {
            return Err(ClientError::new(
                "CLIENT_PAGINATION_INVALID",
                "pagination bounds must be positive",
            ));
        }
        Ok(Self {
            request: Some(PageRequest { after: None, first }),
            maximum_pages,
            completed_pages: 0,
            cursors: BTreeSet::new(),
        })
    }

    #[must_use]
    pub const fn request(&self) -> Option<&PageRequest> {
        self.request.as_ref()
    }

    /// Advances after a successfully decoded page.
    ///
    /// # Errors
    ///
    /// Returns a typed client error for cursor omission, empty or repeated
    /// cursors, calls after completion, or exhaustion of the page bound.
    pub fn advance(&mut self, page: &PageInfo) -> Result<(), ClientError> {
        let Some(current) = self.request.as_ref() else {
            return Err(ClientError::new(
                "CLIENT_PAGINATION_COMPLETE",
                "pagination is already complete",
            ));
        };
        let first = current.first;
        self.completed_pages += 1;
        if !page.has_next_page {
            self.request = None;
            return Ok(());
        }
        if self.completed_pages >= self.maximum_pages {
            return Err(ClientError::new(
                "CLIENT_PAGINATION_LIMIT",
                "pagination exceeded the configured page bound",
            ));
        }
        let Presence::Value(cursor) = &page.end_cursor else {
            return Err(ClientError::new(
                "CLIENT_PAGINATION_CURSOR_INVALID",
                "continuing pagination requires an end cursor",
            ));
        };
        if cursor.is_empty() || !self.cursors.insert(cursor.clone()) {
            return Err(ClientError::new(
                "CLIENT_PAGINATION_CURSOR_INVALID",
                "pagination cursor is empty or repeated",
            ));
        }
        self.request = Some(PageRequest {
            after: Some(cursor.clone()),
            first,
        });
        Ok(())
    }
}

fn json_error(_: serde_json::Error) -> ClientError {
    ClientError::new("CLIENT_JSON_INVALID", "JSON value is invalid")
}

pub(crate) fn decode_json<T: DeserializeOwned>(input: &[u8]) -> Result<T, ClientError> {
    let mut deserializer = serde_json::Deserializer::from_slice(input);
    let value = StrictValue::deserialize(&mut deserializer)
        .and_then(|value| deserializer.end().map(|()| value.0))
        .map_err(|_| {
            ClientError::new("CLIENT_PROTOCOL_INVALID", "operation response is invalid")
        })?;
    serde_json::from_value(value)
        .map_err(|_| ClientError::new("CLIENT_PROTOCOL_INVALID", "operation response is invalid"))
}

struct StrictValue(Value);

impl<'de> Deserialize<'de> for StrictValue {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        deserializer.deserialize_any(StrictValueVisitor)
    }
}

struct StrictValueVisitor;

impl<'de> Visitor<'de> for StrictValueVisitor {
    type Value = StrictValue;

    fn expecting(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        formatter.write_str("a JSON value without duplicate object keys")
    }

    fn visit_bool<E>(self, value: bool) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Bool(value)))
    }

    fn visit_i64<E>(self, value: i64) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Number(Number::from(value))))
    }

    fn visit_u64<E>(self, value: u64) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Number(Number::from(value))))
    }

    fn visit_f64<E>(self, value: f64) -> Result<Self::Value, E>
    where
        E: serde::de::Error,
    {
        Number::from_f64(value)
            .map(Value::Number)
            .map(StrictValue)
            .ok_or_else(|| E::custom("non-finite JSON number"))
    }

    fn visit_str<E>(self, value: &str) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::String(value.to_owned())))
    }

    fn visit_string<E>(self, value: String) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::String(value)))
    }

    fn visit_none<E>(self) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Null))
    }

    fn visit_unit<E>(self) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Null))
    }

    fn visit_seq<A>(self, mut values: A) -> Result<Self::Value, A::Error>
    where
        A: SeqAccess<'de>,
    {
        let mut sequence = Vec::new();
        while let Some(value) = values.next_element::<StrictValue>()? {
            sequence.push(value.0);
        }
        Ok(StrictValue(Value::Array(sequence)))
    }

    fn visit_map<A>(self, mut values: A) -> Result<Self::Value, A::Error>
    where
        A: MapAccess<'de>,
    {
        let mut object = Map::new();
        while let Some((key, value)) = values.next_entry::<String, StrictValue>()? {
            if object.insert(key, value.0).is_some() {
                return Err(serde::de::Error::custom("duplicate JSON object key"));
            }
        }
        Ok(StrictValue(Value::Object(object)))
    }
}
