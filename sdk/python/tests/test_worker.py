from __future__ import annotations

import asyncio
import contextvars
import threading
from collections.abc import AsyncIterator, Mapping
from contextlib import asynccontextmanager
from dataclasses import dataclass
from decimal import Decimal
from typing import Self, cast

import pytest

from naatre import MISSING, MissingType, Timestamp
from naatre.worker import (
    CAPABILITY_SERVER_STREAMING,
    CAPABILITY_TRANSACTIONS,
    AsyncHandler,
    DataclassCodec,
    ExecutorLimits,
    Field,
    HandlerFailure,
    HandlerRequest,
    IdentityCodec,
    ListCodec,
    MapCodec,
    NullableCodec,
    PydanticCodec,
    RegistrationConfig,
    Registry,
    ScalarCodec,
    SyncHandler,
    TaggedValue,
    UnionCodec,
    VerifiedIdentity,
    Worker,
    WorkerProtocolError,
    current_request,
)


@dataclass(frozen=True, slots=True)
class Input:
    name: str
    tenant_note: str | None | MissingType = MISSING


@dataclass(frozen=True, slots=True)
class Output:
    greeting: str


@dataclass(frozen=True, slots=True)
class StrictAdapterModel:
    count: int

    @classmethod
    def model_validate(
        cls,
        value: object,
        *,
        strict: bool | None = None,
        extra: str | None = None,
    ) -> Self:
        if (
            strict is not True
            or extra != "forbid"
            or not isinstance(value, dict)
            or set(value) != {"count"}
            or type(value["count"]) is not int
        ):
            raise ValueError
        return cls(value["count"])

    def model_dump(
        self, *, mode: str, exclude_unset: bool, warnings: str
    ) -> object:
        if mode not in {"python", "json"} or exclude_unset or warnings != "error":
            raise ValueError
        return {"count": self.count}


INPUT = DataclassCodec(
    Input,
    (
        Field("name", ScalarCodec("String")),
        Field(
            "tenantNote",
            NullableCodec(ScalarCodec("String")),
            attribute="tenant_note",
            required=False,
        ),
    ),
)
OUTPUT = DataclassCodec(Output, (Field("greeting", ScalarCodec("String")),))


async def verify_identity(_delegation: str) -> VerifiedIdentity:
    return VerifiedIdentity(
        subject="user-1",
        tenant="tenant-1",
        trace={"traceparent": "00-abc-def-01"},
    )


def invocation(invocation_id: str, value: object | None = None) -> dict[str, object]:
    return {
        "protocol": "naatre.remote-worker.v1",
        "requestId": f"request-{invocation_id}",
        "invocationId": invocation_id,
        "attemptId": f"{invocation_id}.1",
        "handlerId": "fixture.greet",
        "schemaRevision": "schema-1",
        "deadlineUnixMilli": 4_102_444_800_000,
        "delegatedContext": "verified-token",
        "input": {"name": "Ada"} if value is None else value,
    }


def make_worker(
    handler: SyncHandler[Input, Output] | AsyncHandler[Input, Output],
    *,
    asynchronous: bool,
    limits: ExecutorLimits | None = None,
) -> Worker:
    registry = Registry(schema_revision="schema-1")
    if asynchronous:
        registry.bind_async("fixture.greet", INPUT, OUTPUT, handler)  # type: ignore[arg-type]
    else:
        registry.bind_sync("fixture.greet", INPUT, OUTPUT, handler)  # type: ignore[arg-type]
    return Worker(registry, verify_identity=verify_identity, executor_limits=limits)


def test_sync_and_async_handlers_have_equivalent_public_outcomes() -> None:
    def sync_handler(request: HandlerRequest[Input]) -> Output:
        assert request.identity.tenant == "tenant-1"
        assert request.trace["traceparent"] == "00-abc-def-01"
        assert current_request() is request
        return Output(f"Hello, {request.input.name}")

    async def async_handler(request: HandlerRequest[Input]) -> Output:
        await asyncio.sleep(0)
        assert current_request() is request
        return Output(f"Hello, {request.input.name}")

    async def scenario() -> tuple[Mapping[str, object], Mapping[str, object]]:
        sync_worker = make_worker(sync_handler, asynchronous=False)
        async_worker = make_worker(async_handler, asynchronous=True)
        try:
            return await sync_worker.invoke(invocation("sync")), await async_worker.invoke(
                invocation("async")
            )
        finally:
            await sync_worker.aclose()
            await async_worker.aclose()

    sync_result, async_result = asyncio.run(scenario())
    assert sync_result["data"] == async_result["data"] == {"greeting": "Hello, Ada"}
    assert sync_result["errors"] == async_result["errors"] == []


def test_cancellation_distinguishes_async_from_running_executor_work() -> None:
    async_cancelled = asyncio.Event()
    async_started = asyncio.Event()

    async def cancellable(_request: HandlerRequest[Input]) -> Output:
        async_started.set()
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            async_cancelled.set()
            raise
        raise AssertionError("cancelled coroutine resumed")

    started = threading.Event()
    release = threading.Event()

    def blocking(_request: HandlerRequest[Input]) -> Output:
        started.set()
        release.wait()
        return Output("completed")

    async def scenario() -> None:
        async_worker = make_worker(cancellable, asynchronous=True)
        async_task = asyncio.create_task(async_worker.invoke(invocation("async-cancel")))
        await async_started.wait()
        assert await async_worker.cancel("async-cancel") == "acknowledged"
        with pytest.raises(asyncio.CancelledError):
            await async_task
        assert async_cancelled.is_set()
        await async_worker.aclose()

        sync_worker = make_worker(
            blocking,
            asynchronous=False,
            limits=ExecutorLimits(maximum_workers=1, maximum_pending=0),
        )
        first = asyncio.create_task(sync_worker.invoke(invocation("sync-cancel")))
        await asyncio.to_thread(started.wait)
        assert await sync_worker.cancel("sync-cancel") == "requested"
        first.cancel()
        with pytest.raises(asyncio.CancelledError):
            await first
        assert sync_worker.executor_in_flight == 1
        with pytest.raises(WorkerProtocolError, match="OVERLOADED"):
            await sync_worker.invoke(invocation("second"))
        release.set()
        await sync_worker.aclose()
        assert sync_worker.executor_in_flight == 0

    asyncio.run(scenario())


def test_request_context_and_loader_state_never_leak() -> None:
    inherited = contextvars.ContextVar("inherited", default="clean")
    seen: list[tuple[str, str]] = []

    async def handler(request: HandlerRequest[Input]) -> Output:
        seen.append((request.identity.tenant, str(request.state.loaders.get("value", "missing"))))
        request.state.loaders["value"] = request.input.name
        inherited.set(request.input.name)
        return Output(request.input.name)

    async def scenario() -> None:
        worker = make_worker(handler, asynchronous=True)
        await worker.invoke(invocation("one", {"name": "first"}))
        await worker.invoke(invocation("two", {"name": "second"}))
        await worker.aclose()

    asyncio.run(scenario())
    assert seen == [("tenant-1", "missing"), ("tenant-1", "missing")]
    assert inherited.get() == "clean"
    with pytest.raises(LookupError):
        current_request()


def test_strict_input_and_output_codecs_reject_common_coercions() -> None:
    strict_vectors: tuple[tuple[ScalarCodec, object], ...] = (
        (ScalarCodec("Boolean"), 1),
        (ScalarCodec("Int32"), True),
        (ScalarCodec("Int32"), "1"),
        (ScalarCodec("Float64"), 1),
        (ScalarCodec("Decimal"), 1.25),
        (ScalarCodec("Decimal"), "1.00"),
        (ScalarCodec("Int64"), "-0"),
        (ScalarCodec("Duration"), str(2**63)),
        (ScalarCodec("Timestamp"), "2026-02-30T00:00:00Z"),
        (ScalarCodec("Timestamp"), "2026-09-15T10:11:12+00:00"),
        (ScalarCodec("Timestamp"), "2026-09-15T10:11:12.1234567890Z"),
        (ScalarCodec("UUID"), "123E4567-E89B-12D3-A456-426614174000"),
        (ScalarCodec("Bytes"), "YWJj="),
        (ScalarCodec("Bytes"), "AB"),
    )
    for codec, value in strict_vectors:
        with pytest.raises(WorkerProtocolError, match="WORKER_VALUE_INVALID"):
            codec.decode(value)
        with pytest.raises(WorkerProtocolError, match="WORKER_VALUE_INVALID"):
            codec.encode(value)

    with pytest.raises(WorkerProtocolError, match="WORKER_VALUE_INVALID"):
        INPUT.decode({"name": "Ada", "tenantNote": None, "extra": True})
    with pytest.raises(WorkerProtocolError, match="WORKER_VALUE_INVALID"):
        INPUT.decode({"tenantNote": None})
    assert INPUT.decode({"name": "Ada"}).tenant_note is MISSING
    assert INPUT.decode({"name": "Ada", "tenantNote": None}).tenant_note is None

    tagged = UnionCodec({"count": ScalarCodec("Int32")})
    assert tagged.decode({"$type": "count", "$value": 3}) == TaggedValue("count", 3)
    for value in ({"$type": "missing", "$value": 3}, {"$type": 1, "$value": 3}, {}):
        with pytest.raises(WorkerProtocolError, match="WORKER_VALUE_INVALID"):
            tagged.decode(value)

    assert ScalarCodec("Decimal").encode(Decimal("7.8900")) == "7.89"
    assert ScalarCodec("Timestamp").encode(Timestamp("2026-09-15T10:11:12.123456789Z")) == (
        "2026-09-15T10:11:12.123456789Z"
    )
    assert ScalarCodec("Timestamp").encode(Timestamp("2026-09-15T12:11:12+02:00")) == (
        "2026-09-15T10:11:12Z"
    )
    assert ScalarCodec("UUID").decode("123e4567-e89b-12d3-a456-426614174000") == (
        "123e4567-e89b-12d3-a456-426614174000"
    )
    assert ScalarCodec("Duration").encode(-42) == "-42"
    with pytest.raises(WorkerProtocolError, match="WORKER_VALUE_INVALID"):
        ScalarCodec("Duration").encode(2**63)
    assert ScalarCodec("Bytes").encode(b"abc") == "YWJj"
    assert ListCodec(ScalarCodec("String")).decode(["a"]) == ("a",)
    assert MapCodec(ScalarCodec("String")).decode({"a": "b"}) == {"a": "b"}


def test_optional_pydantic_codec_forbids_input_and_output_coercion() -> None:
    codec = PydanticCodec(StrictAdapterModel)

    assert codec.decode({"count": 7}) == StrictAdapterModel(7)
    assert codec.encode(StrictAdapterModel(7)) == {"count": 7}
    for value in ({"count": True}, {"count": "7"}, {"count": 7, "extra": 1}):
        with pytest.raises(WorkerProtocolError, match="WORKER_VALUE_INVALID"):
            codec.decode(value)
    with pytest.raises(WorkerProtocolError, match="WORKER_VALUE_INVALID"):
        codec.encode(StrictAdapterModel(cast(int, True)))
    assert IdentityCodec().decode({"safe": True}) == {"safe": True}


def test_worker_rejects_invalid_input_before_handler_and_output_before_transmission() -> None:
    calls = 0

    async def handler(_request: HandlerRequest[Input]) -> Output:
        nonlocal calls
        calls += 1
        return Output(1)  # type: ignore[arg-type]

    async def scenario() -> None:
        worker = make_worker(handler, asynchronous=True)
        with pytest.raises(WorkerProtocolError, match="REMOTE_INVOCATION_INVALID"):
            await worker.invoke(invocation("bad-input", {"name": True}))
        assert calls == 0
        with pytest.raises(WorkerProtocolError, match="OUTPUT_COMPLETION"):
            await worker.invoke(invocation("bad-output"))
        assert calls == 1
        await worker.aclose()

    asyncio.run(scenario())


class ClosableSource(AsyncIterator[Output]):
    def __init__(self, *, fail: bool = False) -> None:
        self.closed = False
        self.fail = fail
        self.sent = False

    def __aiter__(self) -> ClosableSource:
        return self

    async def __anext__(self) -> Output:
        if self.fail:
            raise HandlerFailure("STREAM_FAILED")
        if self.sent:
            raise StopAsyncIteration
        self.sent = True
        return Output("frame")

    async def aclose(self) -> None:
        self.closed = True


class TransactionProbe:
    def __init__(self) -> None:
        self.entered = 0
        self.exited = 0
        self.failures = 0

    @asynccontextmanager
    async def transaction(self, _request: HandlerRequest[object]) -> AsyncIterator[None]:
        self.entered += 1
        try:
            yield
        except BaseException:
            self.failures += 1
            raise
        finally:
            self.exited += 1


def test_stream_flow_control_source_transaction_and_shutdown_cleanup() -> None:
    async def scenario() -> None:
        source = ClosableSource()

        def stream(_request: HandlerRequest[Input]) -> AsyncIterator[Output]:
            return source

        registry = Registry(schema_revision="schema-1")
        registry.bind_stream(
            "fixture.greet",
            INPUT,
            OUTPUT,
            stream,
            transactional=True,
        )
        transactions = TransactionProbe()
        worker = Worker(
            registry,
            verify_identity=verify_identity,
            capabilities=(CAPABILITY_SERVER_STREAMING, CAPABILITY_TRANSACTIONS),
            transaction_provider=transactions,
        )
        session = await worker.open_stream(invocation("stream"))
        next_frame = asyncio.create_task(anext(session))
        await asyncio.sleep(0)
        assert not next_frame.done(), "source pulled without granted credit"
        await session.grant(frames=1, bytes=1024)
        assert await next_frame == b'{"greeting":"frame"}'
        await session.aclose()
        assert source.closed
        assert transactions.entered == transactions.exited == 1
        await worker.aclose()

        failing = ClosableSource(fail=True)
        failing_transactions = TransactionProbe()
        failing_registry = Registry(schema_revision="schema-1")
        failing_registry.bind_stream(
            "fixture.greet",
            INPUT,
            OUTPUT,
            lambda _request: failing,
            transactional=True,
        )
        failing_worker = Worker(
            failing_registry,
            verify_identity=verify_identity,
            capabilities=(CAPABILITY_SERVER_STREAMING, CAPABILITY_TRANSACTIONS),
            transaction_provider=failing_transactions,
        )
        failing_session = await failing_worker.open_stream(invocation("failing"))
        await failing_session.grant(frames=1, bytes=1024)
        with pytest.raises(HandlerFailure):
            await anext(failing_session)
        assert failing.closed
        assert failing_transactions.failures == 1
        await failing_worker.aclose()

        shutdown_source = ClosableSource()
        shutdown_registry = Registry(schema_revision="schema-1")
        shutdown_registry.bind_stream(
            "fixture.greet", INPUT, OUTPUT, lambda _request: shutdown_source
        )
        shutdown_worker = Worker(
            shutdown_registry,
            verify_identity=verify_identity,
            capabilities=(CAPABILITY_SERVER_STREAMING,),
        )
        await shutdown_worker.open_stream(invocation("shutdown"))
        await shutdown_worker.aclose()
        assert shutdown_source.closed

        cancelled_source = ClosableSource()
        cancelled_registry = Registry(schema_revision="schema-1")
        cancelled_registry.bind_stream(
            "fixture.greet", INPUT, OUTPUT, lambda _request: cancelled_source
        )
        cancelled_worker = Worker(
            cancelled_registry,
            verify_identity=verify_identity,
            capabilities=(CAPABILITY_SERVER_STREAMING,),
        )
        cancelled_session = await cancelled_worker.open_stream(invocation("cancelled"))
        pending_pull = asyncio.create_task(anext(cancelled_session))
        await asyncio.sleep(0)
        assert await cancelled_worker.cancel("cancelled") == "acknowledged"
        with pytest.raises(asyncio.CancelledError):
            await pending_pull
        assert cancelled_source.closed
        await cancelled_worker.aclose()

    asyncio.run(scenario())


def test_stream_requires_negotiated_capability() -> None:
    registry = Registry(schema_revision="schema-1")
    registry.bind_stream(
        "fixture.greet",
        INPUT,
        OUTPUT,
        lambda _request: ClosableSource(),
    )

    async def scenario() -> None:
        worker = Worker(registry, verify_identity=verify_identity)
        with pytest.raises(WorkerProtocolError, match="REMOTE_CAPABILITY_MISMATCH"):
            await worker.open_stream(invocation("stream"))
        await worker.aclose()

    asyncio.run(scenario())


def test_registration_acknowledges_only_the_pinned_worker_identity() -> None:
    async def handler(request: HandlerRequest[Input]) -> Output:
        return Output(request.input.name)

    registry = Registry(schema_revision="schema-1")
    registry.bind_async("fixture.greet", INPUT, OUTPUT, handler)
    worker = Worker(
        registry,
        verify_identity=verify_identity,
        registration=RegistrationConfig(
            worker_id="fixture-worker",
            service_identity="spiffe://example/fixture-worker",
            audience="naatre-gateway",
            schema_digest="a" * 64,
            session_id="python-session",
        ),
        maximum_in_flight=1,
    )

    async def scenario() -> None:
        fixture = {
            "protocol": "naatre.remote-worker.v1",
            "workerId": "fixture-worker",
            "serviceIdentity": "spiffe://example/fixture-worker",
            "audience": "naatre-gateway",
            "endpoint": "fixture-stdio",
            "schemaRevision": "schema-1",
            "schemaDigest": "a" * 64,
            "capabilities": ["unary-1", "cancellation-ack-1"],
            "limits": {
                "maxInFlight": 1,
                "maxRequestBytes": 4096,
                "maxResponseBytes": 4096,
                "maxStreamFrames": 4,
                "maxStreamBytes": 16384,
            },
            "handlers": [
                {
                    "id": "fixture.greet",
                    "inputSchema": "GreetInput",
                    "outputSchema": "GreetOutput",
                    "codec": "naatre.json-1",
                    "effect": "query",
                    "requiredCapabilities": [],
                }
            ],
        }
        result = await worker.handle_envelope(
            {
                "protocol": "naatre.remote-worker.v1",
                "kind": "register",
                "payload": fixture,
            }
        )
        assert result["kind"] == "registered"
        forged = dict(fixture, serviceIdentity="spiffe://example/forged")
        with pytest.raises(WorkerProtocolError, match="REMOTE_REGISTRATION_INVALID"):
            await worker.handle_envelope(
                {
                    "protocol": "naatre.remote-worker.v1",
                    "kind": "register",
                    "payload": forged,
                }
            )
        await worker.aclose()

    asyncio.run(scenario())


def test_reload_drains_old_invocations_before_admitting_new_registry() -> None:
    started = asyncio.Event()
    release = asyncio.Event()

    async def old_handler(_request: HandlerRequest[Input]) -> Output:
        started.set()
        await release.wait()
        return Output("old")

    async def new_handler(_request: HandlerRequest[Input]) -> Output:
        return Output("new")

    async def scenario() -> None:
        old_registry = Registry(schema_revision="schema-1")
        old_registry.bind_async("fixture.greet", INPUT, OUTPUT, old_handler)
        worker = Worker(old_registry, verify_identity=verify_identity)
        old_invocation = asyncio.create_task(worker.invoke(invocation("old")))
        await started.wait()

        new_registry = Registry(schema_revision="schema-1")
        new_registry.bind_async("fixture.greet", INPUT, OUTPUT, new_handler)
        reload_task = asyncio.create_task(worker.reload(new_registry))
        await asyncio.sleep(0)
        with pytest.raises(WorkerProtocolError, match="OVERLOADED"):
            await worker.invoke(invocation("during-reload"))
        release.set()
        assert (await old_invocation)["data"] == {"greeting": "old"}
        await reload_task
        assert (await worker.invoke(invocation("new")))["data"] == {"greeting": "new"}
        await worker.aclose()

    asyncio.run(scenario())
