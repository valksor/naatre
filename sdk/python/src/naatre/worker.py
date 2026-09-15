from __future__ import annotations

import asyncio
import contextvars
import math
import re
import threading
from collections.abc import (
    AsyncIterator,
    Awaitable,
    Callable,
    Collection,
    Mapping,
    Sequence,
)
from concurrent.futures import Future, ThreadPoolExecutor
from contextlib import AbstractAsyncContextManager, asynccontextmanager
from dataclasses import dataclass, field
from dataclasses import fields as dataclass_fields
from datetime import UTC, datetime, timedelta, timezone
from decimal import Decimal, InvalidOperation
from functools import partial
from typing import Any, Generic, Literal, Protocol, TypeAlias, TypeVar, cast

from .json import JSONValue, canonical_json, strict_json_loads
from .pydantic import PydanticModel
from .transport import ExecutorLimits as ExecutorLimits
from .values import (
    MISSING,
    OpenVariant,
    Timestamp,
    decode_bytes,
    decode_integer,
    encode_bytes,
    encode_integer,
)

PROTOCOL_VERSION = "naatre.remote-worker.v1"
CAPABILITY_UNARY = "unary-1"
CAPABILITY_SERVER_STREAMING = "server-streaming-1"
CAPABILITY_CANCELLATION_ACK = "cancellation-ack-1"
CAPABILITY_TRANSACTIONS = "transaction-provider-1"

InputT = TypeVar("InputT")
OutputT = TypeVar("OutputT")
T = TypeVar("T")
PydanticT = TypeVar("PydanticT", bound=PydanticModel)


class WorkerProtocolError(ValueError):
    """A stable, redacted worker boundary failure."""

    def __init__(self, code: str) -> None:
        super().__init__(code)
        self.code = code


class HandlerFailure(Exception):
    """An application-owned typed failure safe to return to the gateway."""

    def __init__(self, code: str, message: str = "", *, retryable: bool = False) -> None:
        if not _valid_code(code):
            raise WorkerProtocolError("WORKER_CONFIG_INVALID")
        super().__init__(message or code)
        self.code = code
        self.message = message
        self.retryable = retryable


@dataclass(frozen=True, slots=True)
class VerifiedIdentity:
    subject: str
    tenant: str
    claims: Mapping[str, JSONValue] = field(default_factory=dict)
    trace: Mapping[str, str] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if not self.subject or not self.tenant:
            raise WorkerProtocolError("REMOTE_UNAUTHENTICATED")
        canonical_json(dict(self.claims))
        if any(not key or not isinstance(value, str) for key, value in self.trace.items()):
            raise WorkerProtocolError("REMOTE_UNAUTHENTICATED")


@dataclass(slots=True)
class RequestState:
    """Fresh mutable state owned by exactly one invocation."""

    loaders: dict[str, object] = field(default_factory=dict)
    values: dict[str, object] = field(default_factory=dict)


@dataclass(frozen=True, slots=True)
class HandlerRequest(Generic[InputT]):
    input: InputT
    request_id: str
    invocation_id: str
    attempt_id: str
    deadline: datetime
    identity: VerifiedIdentity
    trace: Mapping[str, str]
    state: RequestState
    cancelled: asyncio.Event
    idempotency_key: str = ""
    resume_cursor: str = ""


_request_context: contextvars.ContextVar[HandlerRequest[object]] = contextvars.ContextVar(
    "naatre_worker_request"
)


def current_request() -> HandlerRequest[object]:
    """Return the active request or raise LookupError outside a handler."""

    return _request_context.get()


class Codec(Protocol[T]):
    def decode(self, value: object) -> T: ...

    def encode(self, value: T) -> JSONValue: ...


class IdentityCodec:
    """Strict JSON pass-through for explicitly untyped schema positions."""

    def decode(self, value: object) -> JSONValue:
        return strict_json_loads(canonical_json(value))

    def encode(self, value: JSONValue) -> JSONValue:
        return strict_json_loads(canonical_json(value))


@dataclass(frozen=True, slots=True)
class PydanticCodec(Generic[PydanticT]):
    """Strict optional Pydantic adapter without importing Pydantic itself."""

    model: type[PydanticT]

    def decode(self, value: object) -> PydanticT:
        try:
            return self.model.model_validate(value, strict=True, extra="forbid")
        except Exception:
            raise WorkerProtocolError("WORKER_VALUE_INVALID") from None

    def encode(self, value: PydanticT) -> JSONValue:
        try:
            if not isinstance(value, self.model):
                raise TypeError
            python_value = value.model_dump(
                mode="python", exclude_unset=False, warnings="error"
            )
            validated = self.model.model_validate(
                python_value, strict=True, extra="forbid"
            )
            json_value = validated.model_dump(
                mode="json", exclude_unset=False, warnings="error"
            )
            return strict_json_loads(canonical_json(json_value))
        except Exception:
            raise WorkerProtocolError("WORKER_VALUE_INVALID") from None


ScalarName: TypeAlias = Literal[
    "Boolean",
    "Int32",
    "Float64",
    "Int64",
    "UInt64",
    "BigInt",
    "Decimal",
    "Timestamp",
    "Duration",
    "UUID",
    "Bytes",
    "String",
    "ID",
]


@dataclass(frozen=True, slots=True)
class ScalarCodec:
    name: ScalarName

    def decode(self, value: object) -> object:
        try:
            return self._decode(value)
        except (OverflowError, TypeError, ValueError):
            raise WorkerProtocolError("WORKER_VALUE_INVALID") from None

    def encode(self, value: object) -> JSONValue:
        try:
            encoded = self._encode(value)
            return strict_json_loads(canonical_json(encoded))
        except (OverflowError, TypeError, ValueError):
            raise WorkerProtocolError("WORKER_VALUE_INVALID") from None

    def _decode(self, value: object) -> object:
        return _SCALAR_DECODERS[self.name](value)

    def _encode(self, value: object) -> JSONValue:
        return _SCALAR_ENCODERS[self.name](value)


@dataclass(frozen=True, slots=True)
class NullableCodec(Generic[T]):
    inner: Codec[T]

    def decode(self, value: object) -> T | None:
        return None if value is None else self.inner.decode(value)

    def encode(self, value: T | None) -> JSONValue:
        return None if value is None else self.inner.encode(value)


@dataclass(frozen=True, slots=True)
class ListCodec(Generic[T]):
    inner: Codec[T]

    def decode(self, value: object) -> tuple[T, ...]:
        if not isinstance(value, list):
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        return tuple(self.inner.decode(entry) for entry in value)

    def encode(self, value: tuple[T, ...]) -> JSONValue:
        if not isinstance(value, (tuple, list)):
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        return [self.inner.encode(entry) for entry in value]


@dataclass(frozen=True, slots=True)
class MapCodec(Generic[T]):
    inner: Codec[T]

    def decode(self, value: object) -> dict[str, T]:
        if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        return {key: self.inner.decode(entry) for key, entry in value.items()}

    def encode(self, value: dict[str, T]) -> JSONValue:
        if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        return {key: self.inner.encode(entry) for key, entry in value.items()}


@dataclass(frozen=True, slots=True)
class Field:
    name: str
    codec: Codec[Any]
    attribute: str = ""
    required: bool = True

    @property
    def attribute_name(self) -> str:
        return self.attribute or self.name


@dataclass(frozen=True, slots=True)
class DataclassCodec(Generic[T]):
    model: type[T]
    fields: tuple[Field, ...]

    def __post_init__(self) -> None:
        model_fields = {item.name for item in dataclass_fields(self.model)}  # type: ignore[arg-type]
        if (
            not self.fields
            or len({item.name for item in self.fields}) != len(self.fields)
            or any(item.attribute_name not in model_fields for item in self.fields)
        ):
            raise WorkerProtocolError("WORKER_CONFIG_INVALID")

    def decode(self, value: object) -> T:
        if not isinstance(value, dict) or set(value) - {item.name for item in self.fields}:
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        arguments: dict[str, object] = {}
        for item in self.fields:
            if item.name not in value:
                if item.required:
                    raise WorkerProtocolError("WORKER_VALUE_INVALID")
                arguments[item.attribute_name] = MISSING
                continue
            arguments[item.attribute_name] = item.codec.decode(value[item.name])
        try:
            return self.model(**arguments)
        except (TypeError, ValueError):
            raise WorkerProtocolError("WORKER_VALUE_INVALID") from None

    def encode(self, value: T) -> JSONValue:
        if type(value) is not self.model:
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        encoded: dict[str, JSONValue] = {}
        for item in self.fields:
            member = getattr(value, item.attribute_name)
            if member is MISSING:
                if item.required:
                    raise WorkerProtocolError("WORKER_VALUE_INVALID")
                continue
            encoded[item.name] = item.codec.encode(member)
        return strict_json_loads(canonical_json(encoded))


@dataclass(frozen=True, slots=True)
class TaggedValue(Generic[T]):
    tag: str
    value: T


@dataclass(frozen=True, slots=True)
class UnionCodec:
    variants: Mapping[str, Codec[object]]
    open: bool = False

    def decode(self, value: object) -> TaggedValue[object] | OpenVariant:
        if not isinstance(value, dict) or set(value) != {"$type", "$value"}:
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        tag = value["$type"]
        if not isinstance(tag, str):
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        codec = self.variants.get(tag)
        if codec is None:
            if self.open:
                return OpenVariant(tag, IdentityCodec().decode(value["$value"]))
            raise WorkerProtocolError("WORKER_VALUE_INVALID")
        return TaggedValue(tag, codec.decode(value["$value"]))

    def encode(self, value: TaggedValue[object] | OpenVariant) -> JSONValue:
        tag = value.tag if isinstance(value, TaggedValue) else value.discriminator
        member = value.value
        codec = self.variants.get(tag)
        if codec is None:
            if not self.open or not isinstance(value, OpenVariant):
                raise WorkerProtocolError("WORKER_VALUE_INVALID")
            encoded = IdentityCodec().encode(cast(JSONValue, member))
        else:
            encoded = codec.encode(member)
        return {"$type": tag, "$value": encoded}


SyncHandler: TypeAlias = Callable[[HandlerRequest[InputT]], OutputT]
AsyncHandler: TypeAlias = Callable[[HandlerRequest[InputT]], Awaitable[OutputT]]
StreamHandler: TypeAlias = Callable[[HandlerRequest[InputT]], AsyncIterator[OutputT]]
IdentityVerifier: TypeAlias = Callable[[str], Awaitable[VerifiedIdentity]]


@dataclass(frozen=True, slots=True)
class RegistrationConfig:
    worker_id: str
    service_identity: str
    audience: str
    schema_digest: str
    session_id: str

    def __post_init__(self) -> None:
        if (
            not self.worker_id
            or not self.service_identity
            or not self.audience
            or len(self.schema_digest) != 64
            or any(character not in "0123456789abcdef" for character in self.schema_digest)
            or not self.session_id
        ):
            raise WorkerProtocolError("WORKER_CONFIG_INVALID")


HandlerKind: TypeAlias = Literal["sync", "async", "stream"]


@dataclass(frozen=True, slots=True)
class _Binding:
    id: str
    input_codec: Codec[object]
    output_codec: Codec[object]
    handler: Callable[[HandlerRequest[object]], object]
    kind: HandlerKind
    effect: str
    transactional: bool


class Registry:
    def __init__(self, *, schema_revision: str) -> None:
        if not schema_revision:
            raise WorkerProtocolError("WORKER_CONFIG_INVALID")
        self.schema_revision = schema_revision
        self._bindings: dict[str, _Binding] = {}
        self._frozen = False

    def bind_sync(
        self,
        handler_id: str,
        input_codec: Codec[InputT],
        output_codec: Codec[OutputT],
        handler: SyncHandler[InputT, OutputT],
        *,
        effect: str = "query",
        transactional: bool = False,
    ) -> None:
        self._bind(handler_id, input_codec, output_codec, handler, "sync", effect, transactional)

    def bind_async(
        self,
        handler_id: str,
        input_codec: Codec[InputT],
        output_codec: Codec[OutputT],
        handler: AsyncHandler[InputT, OutputT],
        *,
        effect: str = "query",
        transactional: bool = False,
    ) -> None:
        self._bind(handler_id, input_codec, output_codec, handler, "async", effect, transactional)

    def bind_stream(
        self,
        handler_id: str,
        input_codec: Codec[InputT],
        output_codec: Codec[OutputT],
        handler: StreamHandler[InputT, OutputT],
        *,
        effect: str = "subscription",
        transactional: bool = False,
    ) -> None:
        self._bind(handler_id, input_codec, output_codec, handler, "stream", effect, transactional)

    def _bind(
        self,
        handler_id: str,
        input_codec: Codec[InputT],
        output_codec: Codec[OutputT],
        handler: Callable[[HandlerRequest[InputT]], object],
        kind: HandlerKind,
        effect: str,
        transactional: bool,
    ) -> None:
        if (
            self._frozen
            or not handler_id
            or handler_id in self._bindings
            or effect not in {"query", "mutation", "transaction", "subscription"}
        ):
            raise WorkerProtocolError("WORKER_CONFIG_INVALID")
        self._bindings[handler_id] = _Binding(
            handler_id,
            cast(Codec[object], input_codec),
            cast(Codec[object], output_codec),
            cast(Callable[[HandlerRequest[object]], object], handler),
            kind,
            effect,
            transactional,
        )

    def freeze(self) -> None:
        self._frozen = True

    def lookup(self, handler_id: str) -> _Binding:
        try:
            return self._bindings[handler_id]
        except KeyError:
            raise WorkerProtocolError("REMOTE_HANDLER_UNKNOWN") from None


class TransactionProvider(Protocol):
    def transaction(
        self, request: HandlerRequest[object]
    ) -> AbstractAsyncContextManager[None]: ...


class LifecycleResource(Protocol):
    async def start(self) -> None: ...

    async def drain(self) -> None: ...

    async def aclose(self) -> None: ...


@dataclass(slots=True)
class _Active:
    cancelled: asyncio.Event
    kind: HandlerKind
    task: asyncio.Task[object] | None = None
    future: asyncio.Future[object] | None = None
    stream: WorkerStream | None = None


class _BoundedExecutor:
    def __init__(self, limits: ExecutorLimits) -> None:
        self._executor = ThreadPoolExecutor(
            max_workers=limits.maximum_workers,
            thread_name_prefix="naatre-worker",
        )
        self._capacity = limits.capacity
        self._lock = threading.Lock()
        self._futures: set[Future[object]] = set()
        self._closed = False

    @property
    def in_flight(self) -> int:
        with self._lock:
            return len(self._futures)

    def submit(self, function: Callable[[], object]) -> asyncio.Future[object]:
        with self._lock:
            if self._closed:
                raise WorkerProtocolError("WORKER_CLOSED")
            if len(self._futures) >= self._capacity:
                raise WorkerProtocolError("OVERLOADED")
            future = self._executor.submit(function)
            self._futures.add(future)
        future.add_done_callback(self._complete)
        return asyncio.wrap_future(future)

    def _complete(self, future: Future[object]) -> None:
        with self._lock:
            self._futures.discard(future)

    async def aclose(self) -> None:
        with self._lock:
            self._closed = True
            futures = tuple(self._futures)
        for future in futures:
            future.cancel()
        if futures:
            await asyncio.gather(
                *(asyncio.wrap_future(future) for future in futures),
                return_exceptions=True,
            )
        self._executor.shutdown(wait=False, cancel_futures=True)


class Worker:
    def __init__(
        self,
        registry: Registry,
        *,
        verify_identity: IdentityVerifier,
        executor_limits: ExecutorLimits | None = None,
        capabilities: Collection[str] = (CAPABILITY_UNARY, CAPABILITY_CANCELLATION_ACK),
        transaction_provider: TransactionProvider | None = None,
        resources: Sequence[LifecycleResource] = (),
        registration: RegistrationConfig | None = None,
        maximum_stream_item_bytes: int = 1 << 20,
        maximum_in_flight: int = 64,
    ) -> None:
        if (
            not 1 <= maximum_stream_item_bytes <= 64 << 20
            or not 1 <= maximum_in_flight <= 65_536
        ):
            raise WorkerProtocolError("WORKER_CONFIG_INVALID")
        registry.freeze()
        self._registry = registry
        self._verify_identity = verify_identity
        self._executor = _BoundedExecutor(executor_limits or ExecutorLimits())
        self._capabilities = frozenset(capabilities)
        self._transaction_provider = transaction_provider
        self._resources = tuple(resources)
        self._registration = registration
        self._maximum_stream_item_bytes = maximum_stream_item_bytes
        self._maximum_in_flight = maximum_in_flight
        self._active: dict[str, _Active] = {}
        self._active_changed = asyncio.Condition()
        self._accepting = True
        self._started = False
        self._closed = False

    @property
    def executor_in_flight(self) -> int:
        return self._executor.in_flight

    async def start(self) -> None:
        if self._closed:
            raise WorkerProtocolError("WORKER_CLOSED")
        if self._started:
            return
        started: list[LifecycleResource] = []
        try:
            for resource in self._resources:
                await resource.start()
                started.append(resource)
        except BaseException:
            for resource in reversed(started):
                await resource.aclose()
            raise
        self._started = True

    async def invoke(self, value: Mapping[str, object]) -> Mapping[str, object]:
        invocation = self._parse_invocation(value)
        binding = self._registry.lookup(invocation.handler_id)
        if binding.kind == "stream":
            raise WorkerProtocolError("REMOTE_CAPABILITY_MISMATCH")
        request = await self._request(invocation, binding)
        active = _Active(request.cancelled, binding.kind)
        await self._admit(invocation.invocation_id, active)
        try:
            try:
                if binding.kind == "async":
                    result = await self._run_async(binding, request, active)
                else:
                    result = await self._run_sync(binding, request, active)
                try:
                    data = binding.output_codec.encode(result)
                    canonical_json(data)
                except WorkerProtocolError:
                    raise WorkerProtocolError("OUTPUT_COMPLETION") from None
                errors: list[object] = []
            except HandlerFailure as failure:
                data = None
                errors = [
                    {
                        "code": failure.code,
                        "message": failure.message,
                        "retryable": failure.retryable,
                    }
                ]
            except WorkerProtocolError:
                raise
            except Exception:
                data = None
                errors = [{"code": "INTERNAL", "message": "", "retryable": False}]
            return {
                "protocol": PROTOCOL_VERSION,
                "invocationId": invocation.invocation_id,
                "attemptId": invocation.attempt_id,
                "schemaRevision": self._registry.schema_revision,
                "data": data,
                "errors": errors,
            }
        finally:
            if active.future is not None and not active.future.done():
                active.future.add_done_callback(
                    lambda _future: asyncio.create_task(self._release(invocation.invocation_id))
                )
            else:
                await self._release(invocation.invocation_id)

    async def open_stream(self, value: Mapping[str, object]) -> WorkerStream:
        if CAPABILITY_SERVER_STREAMING not in self._capabilities:
            raise WorkerProtocolError("REMOTE_CAPABILITY_MISMATCH")
        invocation = self._parse_invocation(value)
        binding = self._registry.lookup(invocation.handler_id)
        if binding.kind != "stream":
            raise WorkerProtocolError("REMOTE_CAPABILITY_MISMATCH")
        request = await self._request(invocation, binding)
        active = _Active(request.cancelled, "stream")
        await self._admit(invocation.invocation_id, active)
        transaction = self._transaction(binding, request)
        try:
            await transaction.__aenter__()
            source = binding.handler(request)
            if not hasattr(source, "__aiter__"):
                raise WorkerProtocolError("WORKER_HANDLER_INVALID")
            session = WorkerStream(
                worker=self,
                invocation_id=invocation.invocation_id,
                request=request,
                source=cast(AsyncIterator[object], source),
                output_codec=binding.output_codec,
                transaction=transaction,
                maximum_item_bytes=self._maximum_stream_item_bytes,
            )
            active.stream = session
            return session
        except BaseException as error:
            try:
                await transaction.__aexit__(type(error), error, error.__traceback__)
            finally:
                await self._release(invocation.invocation_id)
            raise

    async def cancel(self, invocation_id: str) -> str:
        active = self._active.get(invocation_id)
        if active is None:
            return "too-late"
        active.cancelled.set()
        if active.kind == "sync":
            return "requested"
        if active.stream is not None:
            if active.task is not None:
                active.task.cancel()
                await asyncio.gather(active.task, return_exceptions=True)
            await active.stream.aclose()
            return "acknowledged"
        if active.task is not None:
            active.task.cancel()
        if active.kind == "stream":
            return "requested"
        return "acknowledged"

    async def handle_envelope(self, envelope: object) -> Mapping[str, object]:
        if not isinstance(envelope, dict) or set(envelope) != {"protocol", "kind", "payload"}:
            raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
        if envelope["protocol"] != PROTOCOL_VERSION or not isinstance(envelope["payload"], dict):
            raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
        kind = envelope["kind"]
        payload = cast(dict[str, object], envelope["payload"])
        if kind == "register":
            return self._register(payload)
        if kind == "invoke":
            return {
                "protocol": PROTOCOL_VERSION,
                "kind": "result",
                "payload": await self.invoke(payload),
            }
        if kind == "cancel":
            if set(payload) != {"protocol", "requestId", "invocationId"}:
                raise WorkerProtocolError("REMOTE_CANCELLATION_INVALID")
            invocation_id = payload["invocationId"]
            if payload["protocol"] != PROTOCOL_VERSION or not isinstance(invocation_id, str):
                raise WorkerProtocolError("REMOTE_CANCELLATION_INVALID")
            disposition = await self.cancel(invocation_id)
            return {
                "protocol": PROTOCOL_VERSION,
                "kind": "cancelled",
                "payload": {
                    "protocol": PROTOCOL_VERSION,
                    "invocationId": invocation_id,
                    "disposition": disposition,
                },
            }
        raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")

    def _register(self, payload: Mapping[str, object]) -> Mapping[str, object]:
        config = self._registration
        required = {
            "protocol",
            "workerId",
            "serviceIdentity",
            "audience",
            "endpoint",
            "schemaRevision",
            "schemaDigest",
            "capabilities",
            "limits",
            "handlers",
        }
        if config is None or set(payload) != required:
            raise WorkerProtocolError("REMOTE_REGISTRATION_INVALID")
        if (
            payload["protocol"] != PROTOCOL_VERSION
            or payload["workerId"] != config.worker_id
            or payload["serviceIdentity"] != config.service_identity
            or payload["audience"] != config.audience
            or payload["schemaRevision"] != self._registry.schema_revision
            or payload["schemaDigest"] != config.schema_digest
        ):
            raise WorkerProtocolError("REMOTE_REGISTRATION_INVALID")
        capabilities = payload["capabilities"]
        limits = payload["limits"]
        handlers = payload["handlers"]
        if (
            not isinstance(capabilities, list)
            or any(not isinstance(item, str) for item in capabilities)
            or len(set(capabilities)) != len(capabilities)
            or not set(capabilities).issubset(self._capabilities)
            or not isinstance(payload["endpoint"], str)
            or not payload["endpoint"]
            or not _valid_registration_limits(limits)
            or cast(dict[str, int], limits)["maxInFlight"] > self._maximum_in_flight
            or not isinstance(handlers, list)
            or not _valid_registration_handlers(handlers, self._registry, set(capabilities))
        ):
            raise WorkerProtocolError("REMOTE_REGISTRATION_INVALID")
        return {
            "protocol": PROTOCOL_VERSION,
            "kind": "registered",
            "payload": {
                "protocol": PROTOCOL_VERSION,
                "workerId": config.worker_id,
                "sessionId": config.session_id,
                "schemaRevision": self._registry.schema_revision,
                "acceptedCapabilities": capabilities,
            },
        }

    async def drain(self) -> None:
        self._accepting = False
        async with self._active_changed:
            await self._active_changed.wait_for(lambda: not self._active)
        failure: BaseException | None = None
        for resource in self._resources:
            try:
                await resource.drain()
            except BaseException as error:
                failure = failure or error
        if failure is not None:
            raise failure

    async def reload(self, registry: Registry) -> None:
        if self._closed:
            raise WorkerProtocolError("WORKER_CLOSED")
        self._accepting = False
        async with self._active_changed:
            await self._active_changed.wait_for(lambda: not self._active)
        registry.freeze()
        self._registry = registry
        self._accepting = True

    async def aclose(self) -> None:
        if self._closed:
            return
        self._closed = True
        self._accepting = False
        failure = await self._cancel_active()
        try:
            await self._settle_executor()
        except BaseException as error:
            failure = failure or error
        failure = await self._drain_resources(failure)
        failure = await self._close_resources(failure)
        if failure is not None:
            raise failure

    async def _cancel_active(self) -> BaseException | None:
        failure: BaseException | None = None
        for active in tuple(self._active.values()):
            active.cancelled.set()
            if active.stream is not None:
                try:
                    await self._close_active_stream(active)
                except BaseException as error:
                    failure = failure or error
            if active.kind != "sync" and active.task is not None:
                active.task.cancel()
        return failure

    async def _close_active_stream(self, active: _Active) -> None:
        if active.task is not None:
            active.task.cancel()
            await asyncio.gather(active.task, return_exceptions=True)
        stream = active.stream
        if stream is not None:
            await stream.aclose()

    async def _settle_executor(self) -> None:
        await self._executor.aclose()
        async with self._active_changed:
            await self._active_changed.wait_for(lambda: not self._active)

    async def _drain_resources(
        self, failure: BaseException | None
    ) -> BaseException | None:
        for resource in self._resources:
            try:
                await resource.drain()
            except BaseException as error:
                failure = failure or error
        return failure

    async def _close_resources(
        self, failure: BaseException | None
    ) -> BaseException | None:
        for resource in reversed(self._resources):
            try:
                await resource.aclose()
            except BaseException as error:
                failure = failure or error
        return failure

    async def _run_async(
        self, binding: _Binding, request: HandlerRequest[object], active: _Active
    ) -> object:
        async def call() -> object:
            token = _request_context.set(request)
            try:
                timeout = max(0.0, (request.deadline - datetime.now(UTC)).total_seconds())
                async with asyncio.timeout(timeout):
                    async with self._transaction(binding, request):
                        result = binding.handler(request)
                        if not hasattr(result, "__await__"):
                            raise WorkerProtocolError("WORKER_HANDLER_INVALID")
                        return await cast(Awaitable[object], result)
            finally:
                _request_context.reset(token)

        task = asyncio.create_task(call(), context=contextvars.Context())
        active.task = task
        if active.cancelled.is_set():
            task.cancel()
        try:
            return await task
        except TimeoutError:
            request.cancelled.set()
            raise WorkerProtocolError("DEADLINE_EXCEEDED") from None

    async def _run_sync(
        self,
        binding: _Binding,
        request: HandlerRequest[object],
        active: _Active,
    ) -> object:
        if binding.transactional:
            raise WorkerProtocolError("WORKER_CONFIG_INVALID")

        def call() -> object:
            token = _request_context.set(request)
            try:
                return binding.handler(request)
            finally:
                _request_context.reset(token)

        future = self._executor.submit(lambda: contextvars.Context().run(call))
        active.future = future
        try:
            timeout = max(0.0, (request.deadline - datetime.now(UTC)).total_seconds())
            return await asyncio.wait_for(asyncio.shield(future), timeout)
        except TimeoutError:
            request.cancelled.set()
            raise WorkerProtocolError("DEADLINE_EXCEEDED") from None

    async def _request(self, invocation: _Invocation, binding: _Binding) -> HandlerRequest[object]:
        now = datetime.now(UTC)
        if invocation.deadline <= now:
            raise WorkerProtocolError("DEADLINE_EXCEEDED")
        try:
            identity = await self._verify_identity(invocation.delegated_context)
            if not isinstance(identity, VerifiedIdentity):
                raise TypeError
        except asyncio.CancelledError:
            raise
        except Exception:
            raise WorkerProtocolError("REMOTE_UNAUTHENTICATED") from None
        try:
            decoded = binding.input_codec.decode(invocation.input)
        except WorkerProtocolError:
            raise WorkerProtocolError("REMOTE_INVOCATION_INVALID") from None
        return HandlerRequest(
            input=decoded,
            request_id=invocation.request_id,
            invocation_id=invocation.invocation_id,
            attempt_id=invocation.attempt_id,
            deadline=invocation.deadline,
            identity=identity,
            trace=dict(identity.trace),
            state=RequestState(),
            cancelled=asyncio.Event(),
            idempotency_key=invocation.idempotency_key,
            resume_cursor=invocation.resume_cursor,
        )

    async def _admit(self, invocation_id: str, active: _Active) -> None:
        if not self._accepting or self._closed:
            raise WorkerProtocolError("OVERLOADED")
        async with self._active_changed:
            if len(self._active) >= self._maximum_in_flight:
                raise WorkerProtocolError("OVERLOADED")
            if invocation_id in self._active:
                raise WorkerProtocolError("REMOTE_INVOCATION_DUPLICATE")
            self._active[invocation_id] = active

    async def _release(self, invocation_id: str) -> None:
        async with self._active_changed:
            self._active.pop(invocation_id, None)
            self._active_changed.notify_all()

    def _transaction(
        self, binding: _Binding, request: HandlerRequest[object]
    ) -> AbstractAsyncContextManager[None]:
        if not binding.transactional:
            return _null_transaction()
        if self._transaction_provider is None or CAPABILITY_TRANSACTIONS not in self._capabilities:
            raise WorkerProtocolError("REMOTE_CAPABILITY_MISMATCH")
        return self._transaction_provider.transaction(request)

    def _parse_invocation(self, value: Mapping[str, object]) -> _Invocation:
        required = {
            "protocol",
            "requestId",
            "invocationId",
            "attemptId",
            "handlerId",
            "schemaRevision",
            "deadlineUnixMilli",
            "delegatedContext",
            "input",
        }
        optional = {"idempotencyKey", "parent", "resumeCursor"}
        if set(value) - required - optional or not required.issubset(value):
            raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
        strings = (
            "protocol",
            "requestId",
            "invocationId",
            "attemptId",
            "handlerId",
            "schemaRevision",
            "delegatedContext",
        )
        if any(not isinstance(value[name], str) or not value[name] for name in strings):
            raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
        if (
            value["protocol"] != PROTOCOL_VERSION
            or value["schemaRevision"] != self._registry.schema_revision
        ):
            raise WorkerProtocolError("REMOTE_SCHEMA_MISMATCH")
        deadline = value["deadlineUnixMilli"]
        if type(deadline) is not int:
            raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
        try:
            deadline_value = datetime.fromtimestamp(deadline / 1000, UTC)
        except (OverflowError, OSError, ValueError):
            raise WorkerProtocolError("REMOTE_INVOCATION_INVALID") from None
        return _Invocation(
            request_id=cast(str, value["requestId"]),
            invocation_id=cast(str, value["invocationId"]),
            attempt_id=cast(str, value["attemptId"]),
            handler_id=cast(str, value["handlerId"]),
            deadline=deadline_value,
            delegated_context=cast(str, value["delegatedContext"]),
            input=value["input"],
            idempotency_key=_optional_string(value, "idempotencyKey"),
            resume_cursor=_optional_string(value, "resumeCursor"),
        )


@dataclass(frozen=True, slots=True)
class _Invocation:
    request_id: str
    invocation_id: str
    attempt_id: str
    handler_id: str
    deadline: datetime
    delegated_context: str
    input: object
    idempotency_key: str
    resume_cursor: str


class WorkerStream(AsyncIterator[bytes]):
    """Credit-gated owner of one application async source."""

    def __init__(
        self,
        *,
        worker: Worker,
        invocation_id: str,
        request: HandlerRequest[object],
        source: AsyncIterator[object],
        output_codec: Codec[object],
        transaction: AbstractAsyncContextManager[None],
        maximum_item_bytes: int,
    ) -> None:
        self._worker = worker
        self._invocation_id = invocation_id
        self._request = request
        self._source = source
        self._output_codec = output_codec
        self._transaction = transaction
        self._maximum_item_bytes = maximum_item_bytes
        self._credit = asyncio.Condition()
        self._frame_credit = 0
        self._byte_credit = 0
        self._closed = False

    def __aiter__(self) -> WorkerStream:
        return self

    async def grant(self, *, frames: int, bytes: int) -> None:
        if frames <= 0 or bytes <= 0:
            raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
        async with self._credit:
            if self._closed:
                raise WorkerProtocolError("WORKER_CLOSED")
            self._frame_credit += frames
            self._byte_credit += bytes
            self._credit.notify_all()

    async def __anext__(self) -> bytes:
        active = self._worker._active.get(self._invocation_id)
        task = asyncio.current_task()
        if active is not None and task is not None:
            active.task = cast(asyncio.Task[object], task)
        token: contextvars.Token[HandlerRequest[object]] | None = None
        try:
            async with self._credit:
                await self._credit.wait_for(
                    lambda: self._closed or self._frame_credit > 0
                )
                if self._closed:
                    raise StopAsyncIteration
            token = _request_context.set(self._request)
            timeout = max(0.0, (self._request.deadline - datetime.now(UTC)).total_seconds())
            async with asyncio.timeout(timeout):
                value = await anext(self._source)
            encoded = canonical_json(self._output_codec.encode(value))
            if len(encoded) > self._maximum_item_bytes:
                raise WorkerProtocolError("REMOTE_STREAM_LIMIT")
            async with self._credit:
                await self._credit.wait_for(
                    lambda: self._closed or len(encoded) <= self._byte_credit
                )
                if self._closed:
                    raise StopAsyncIteration
                self._frame_credit -= 1
                self._byte_credit -= len(encoded)
            return encoded
        except StopAsyncIteration:
            await self.aclose()
            raise
        except BaseException as error:
            await self._finish(error)
            raise
        finally:
            if token is not None:
                _request_context.reset(token)

    async def aclose(self) -> None:
        await self._finish(None)

    async def _finish(self, exception: BaseException | None) -> None:
        if self._closed:
            return
        self._closed = True
        async with self._credit:
            self._credit.notify_all()
        close = getattr(self._source, "aclose", None)
        close_exception: BaseException | None = None
        try:
            if close is not None:
                await close()
        except BaseException as error:
            close_exception = error
        finally:
            outcome = exception or close_exception
            try:
                await self._transaction.__aexit__(
                    type(outcome) if outcome is not None else None,
                    outcome,
                    outcome.__traceback__ if outcome is not None else None,
                )
            finally:
                await self._worker._release(self._invocation_id)
        if close_exception is not None:
            raise close_exception


@asynccontextmanager
async def _null_transaction() -> AsyncIterator[None]:
    yield


def _exact(value: object, expected: type[T]) -> T:
    if type(value) is not expected:
        raise ValueError
    return value


def _valid_code(value: str) -> bool:
    return bool(value) and len(value) <= 64 and all(
        character in "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_" for character in value
    )


def _optional_string(value: Mapping[str, object], key: str) -> str:
    result = value.get(key, "")
    if not isinstance(result, str):
        raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
    return result


_TIMESTAMP = re.compile(
    r"(?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2})T"
    r"(?P<clock>[0-9]{2}:[0-9]{2}:[0-5][0-9])"
    r"(?P<fraction>\.[0-9]{1,9})?"
    r"(?P<zone>Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])\Z"
)
_UUID = re.compile(
    r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\Z"
)


def _canonical_decimal(value: Decimal) -> str:
    if not value.is_finite():
        raise ValueError
    if value.is_zero():
        return "0"
    sign, digits, exponent = value.as_tuple()
    exponent_value = cast(int, exponent)
    coefficient = "".join(str(digit) for digit in digits)
    if exponent_value >= 0:
        result = coefficient + "0" * exponent_value
    else:
        split = len(coefficient) + exponent_value
        result = (
            coefficient[:split] + "." + coefficient[split:]
            if split > 0
            else "0." + "0" * -split + coefficient
        )
        result = result.rstrip("0").rstrip(".")
    result = result.lstrip("0") if not result.startswith("0.") else result
    return ("-" if sign else "") + result


def _canonical_timestamp(value: Timestamp) -> str:
    match = _TIMESTAMP.fullmatch(value.raw)
    if match is None:
        raise ValueError
    year, month, day = (int(part) for part in match.group("date").split("-"))
    hour, minute, second = (int(part) for part in match.group("clock").split(":"))
    zone = match.group("zone")
    if zone == "Z":
        offset = UTC
    else:
        hours, minutes = (int(part) for part in zone[1:].split(":"))
        delta = timedelta(hours=hours, minutes=minutes)
        offset = timezone(delta if zone[0] == "+" else -delta)
    instant = datetime(year, month, day, hour, minute, second, tzinfo=offset).astimezone(UTC)
    fraction = (match.group("fraction") or "").rstrip("0").rstrip(".")
    return instant.strftime("%Y-%m-%dT%H:%M:%S") + fraction + "Z"


def _int32(value: object) -> int:
    integer = _exact(value, int)
    if not -(2**31) <= integer < 2**31:
        raise ValueError
    return integer


def _float64(value: object) -> float:
    number = _exact(value, float)
    if not math.isfinite(number):
        raise ValueError
    return number


def _bounded_integer(value: int, minimum: int | None, maximum: int | None) -> int:
    if minimum is not None and value < minimum:
        raise ValueError
    if maximum is not None and value > maximum:
        raise ValueError
    return value


def _decode_wire_integer(
    value: object, *, minimum: int | None, maximum: int | None
) -> int:
    if value == "-0":
        raise ValueError
    return _bounded_integer(decode_integer(value), minimum, maximum)


def _encode_native_integer(
    value: object, *, minimum: int | None, maximum: int | None
) -> str:
    integer = _bounded_integer(_exact(value, int), minimum, maximum)
    return encode_integer(integer)


def _decode_decimal(value: object) -> Decimal:
    if not isinstance(value, str):
        raise ValueError
    try:
        decimal = Decimal(value)
    except InvalidOperation:
        raise ValueError from None
    if _canonical_decimal(decimal) != value:
        raise ValueError
    return decimal


def _encode_decimal(value: object) -> str:
    return _canonical_decimal(_exact(value, Decimal))


def _decode_timestamp(value: object) -> Timestamp:
    if not isinstance(value, str):
        raise ValueError
    timestamp = Timestamp(value)
    if _canonical_timestamp(timestamp) != value:
        raise ValueError
    return timestamp


def _encode_timestamp(value: object) -> str:
    return _canonical_timestamp(_exact(value, Timestamp))


def _uuid(value: object) -> str:
    uuid = _exact(value, str)
    if _UUID.fullmatch(uuid) is None:
        raise ValueError
    return uuid


def _decode_canonical_bytes(value: object) -> bytes:
    decoded = decode_bytes(value)
    if encode_bytes(decoded) != value:
        raise ValueError
    return decoded


def _encode_bytes(value: object) -> str:
    return encode_bytes(_exact(value, bytes))


_INT64_MIN = -(2**63)
_INT64_MAX = 2**63 - 1
_UINT64_MAX = 2**64 - 1

_SCALAR_DECODERS: Mapping[ScalarName, Callable[[object], object]] = {
    "Boolean": partial(_exact, expected=bool),
    "Int32": _int32,
    "Float64": _float64,
    "Int64": partial(
        _decode_wire_integer, minimum=_INT64_MIN, maximum=_INT64_MAX
    ),
    "UInt64": partial(_decode_wire_integer, minimum=0, maximum=_UINT64_MAX),
    "BigInt": partial(_decode_wire_integer, minimum=None, maximum=None),
    "Decimal": _decode_decimal,
    "Timestamp": _decode_timestamp,
    "Duration": partial(
        _decode_wire_integer, minimum=_INT64_MIN, maximum=_INT64_MAX
    ),
    "UUID": _uuid,
    "Bytes": _decode_canonical_bytes,
    "String": partial(_exact, expected=str),
    "ID": partial(_exact, expected=str),
}

_SCALAR_ENCODERS: Mapping[ScalarName, Callable[[object], JSONValue]] = {
    "Boolean": partial(_exact, expected=bool),
    "Int32": _int32,
    "Float64": _float64,
    "Int64": partial(
        _encode_native_integer, minimum=_INT64_MIN, maximum=_INT64_MAX
    ),
    "UInt64": partial(_encode_native_integer, minimum=0, maximum=_UINT64_MAX),
    "BigInt": partial(_encode_native_integer, minimum=None, maximum=None),
    "Decimal": _encode_decimal,
    "Timestamp": _encode_timestamp,
    "Duration": partial(
        _encode_native_integer, minimum=_INT64_MIN, maximum=_INT64_MAX
    ),
    "UUID": _uuid,
    "Bytes": _encode_bytes,
    "String": partial(_exact, expected=str),
    "ID": partial(_exact, expected=str),
}


def _valid_registration_limits(value: object) -> bool:
    names = {
        "maxInFlight",
        "maxRequestBytes",
        "maxResponseBytes",
        "maxStreamFrames",
        "maxStreamBytes",
    }
    return (
        isinstance(value, dict)
        and set(value) == names
        and all(type(value[name]) is int and value[name] > 0 for name in names)
    )


def _valid_registration_handlers(
    values: list[object], registry: Registry, capabilities: set[str]
) -> bool:
    required = {
        "id",
        "inputSchema",
        "outputSchema",
        "codec",
        "effect",
        "requiredCapabilities",
    }
    identifiers: set[str] = set()
    for value in values:
        if not isinstance(value, dict) or set(value) != required:
            return False
        handler_id = value["id"]
        required_capabilities = value["requiredCapabilities"]
        if (
            not isinstance(handler_id, str)
            or handler_id not in registry._bindings
            or not isinstance(value["inputSchema"], str)
            or not value["inputSchema"]
            or not isinstance(value["outputSchema"], str)
            or not value["outputSchema"]
            or value["codec"] != "naatre.json-1"
            or value["effect"] != registry._bindings[handler_id].effect
            or not isinstance(required_capabilities, list)
            or any(not isinstance(item, str) for item in required_capabilities)
            or not set(required_capabilities).issubset(capabilities)
        ):
            return False
        identifiers.add(handler_id)
    return identifiers == set(registry._bindings)
