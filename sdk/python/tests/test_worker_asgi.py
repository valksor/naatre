from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable

import pytest
from test_worker import Input, Output, TransactionProbe, invocation

from naatre.json import canonical_json, strict_json_loads
from naatre.worker import (
    CAPABILITY_CANCELLATION_ACK,
    CAPABILITY_TRANSACTIONS,
    CAPABILITY_UNARY,
    DataclassCodec,
    Field,
    HandlerRequest,
    Registry,
    ScalarCodec,
    VerifiedIdentity,
    Worker,
)
from naatre.worker_asgi import ASGIApplication, WorkerASGI


class Resource:
    def __init__(self, *, fail_start: bool = False) -> None:
        self.events: list[str] = []
        self.fail_start = fail_start

    async def start(self) -> None:
        self.events.append("start")
        if self.fail_start:
            raise RuntimeError("private pool startup detail")

    async def drain(self) -> None:
        self.events.append("drain")

    async def aclose(self) -> None:
        self.events.append("close")


async def verify(_delegation: str) -> VerifiedIdentity:
    return VerifiedIdentity(subject="subject", tenant="tenant")


async def call_asgi(
    app: ASGIApplication,
    scope: dict[str, object],
    messages: list[dict[str, object]],
) -> list[dict[str, object]]:
    sent: list[dict[str, object]] = []
    queue = asyncio.Queue[dict[str, object]]()
    for message in messages:
        queue.put_nowait(message)

    async def receive() -> dict[str, object]:
        return await queue.get()

    async def send(message: dict[str, object]) -> None:
        sent.append(message)

    await app(scope, receive, send)
    return sent


async def assert_startup_failure(
    factory: Callable[[Worker], ASGIApplication],
) -> None:
    async def handler(request: HandlerRequest[Input]) -> Output:
        return Output(request.input.name)

    first = Resource()
    failing = Resource(fail_start=True)
    registry = Registry(schema_revision="schema-1")
    registry.bind_async(
        "fixture.greet",
        DataclassCodec(Input, (Field("name", ScalarCodec("String")),)),
        DataclassCodec(Output, (Field("greeting", ScalarCodec("String")),)),
        handler,
    )
    application = factory(Worker(registry, verify_identity=verify, resources=(first, failing)))
    messages = await call_asgi(
        application,
        {"type": "lifespan"},
        [{"type": "lifespan.startup"}],
    )
    assert messages == [{"type": "lifespan.startup.failed", "message": "WORKER_STARTUP_FAILED"}]
    assert first.events == ["start", "close"]
    assert "private pool startup detail" not in repr(messages)


def app_with_handler(
    handler: Callable[[HandlerRequest[Input]], Awaitable[Output]],
    resource: Resource | None = None,
    transaction: TransactionProbe | None = None,
) -> WorkerASGI:
    registry = Registry(schema_revision="schema-1")
    registry.bind_async(
        "fixture.greet",
        DataclassCodec(Input, (Field("name", ScalarCodec("String")),)),
        DataclassCodec(Output, (Field("greeting", ScalarCodec("String")),)),
        handler,
        transactional=transaction is not None,
    )
    resources = () if resource is None else (resource,)
    capabilities: tuple[str, ...] = (CAPABILITY_UNARY, CAPABILITY_CANCELLATION_ACK)
    if transaction is not None:
        capabilities += (CAPABILITY_TRANSACTIONS,)
    return WorkerASGI(
        Worker(
            registry,
            verify_identity=verify,
            resources=resources,
            capabilities=capabilities,
            transaction_provider=transaction,
        )
    )


def test_asgi_lifespan_starts_drains_and_closes_resources() -> None:
    async def handler(request: HandlerRequest[Input]) -> Output:
        return Output(request.input.name)

    async def scenario() -> list[str]:
        resource = Resource()
        app = app_with_handler(handler, resource)
        messages = await call_asgi(
            app,
            {"type": "lifespan"},
            [{"type": "lifespan.startup"}, {"type": "lifespan.shutdown"}],
        )
        assert messages == [
            {"type": "lifespan.startup.complete"},
            {"type": "lifespan.shutdown.complete"},
        ]
        return resource.events

    assert asyncio.run(scenario()) == ["start", "drain", "close"]


def test_concurrent_lifespan_startup_starts_each_pool_once() -> None:
    class ConcurrentResource(Resource):
        def __init__(self) -> None:
            super().__init__()
            self.active_starts = 0
            self.maximum_active_starts = 0

        async def start(self) -> None:
            self.active_starts += 1
            self.maximum_active_starts = max(self.maximum_active_starts, self.active_starts)
            await asyncio.sleep(0)
            self.events.append("start")
            self.active_starts -= 1

    async def handler(request: HandlerRequest[Input]) -> Output:
        return Output(request.input.name)

    async def scenario() -> tuple[list[dict[str, object]], list[str], int]:
        resource = ConcurrentResource()
        app = app_with_handler(handler, resource)
        sent: list[dict[str, object]] = []
        both_started = asyncio.Event()

        async def run_lifespan() -> None:
            first = True

            async def receive() -> dict[str, object]:
                nonlocal first
                if first:
                    first = False
                    return {"type": "lifespan.startup"}
                await asyncio.Event().wait()
                raise AssertionError("unreachable")

            async def send(message: dict[str, object]) -> None:
                sent.append(message)
                if len(sent) == 2:
                    both_started.set()

            await app({"type": "lifespan"}, receive, send)

        lifespans = [asyncio.create_task(run_lifespan()) for _index in range(2)]
        await both_started.wait()
        for lifespan_task in lifespans:
            lifespan_task.cancel()
        await asyncio.gather(*lifespans, return_exceptions=True)
        await app.worker.aclose()
        return sent, resource.events, resource.maximum_active_starts

    messages, events, maximum_active = asyncio.run(scenario())
    assert messages == [
        {"type": "lifespan.startup.complete"},
        {"type": "lifespan.startup.complete"},
    ]
    assert events == ["start", "drain", "close"]
    assert maximum_active == 1


def test_asgi_unary_and_disconnect_cleanup_without_listener() -> None:
    cancelled = asyncio.Event()

    async def handler(request: HandlerRequest[Input]) -> Output:
        if request.input.name == "block":
            try:
                await asyncio.Event().wait()
            finally:
                cancelled.set()
        return Output(f"Hello, {request.input.name}")

    async def scenario() -> None:
        transactions = TransactionProbe()
        app = app_with_handler(handler, transaction=transactions)
        body = canonical_json(
            {
                "protocol": "naatre.remote-worker.v1",
                "kind": "invoke",
                "payload": invocation("asgi"),
            }
        )
        sent = await call_asgi(
            app,
            {"type": "http", "method": "POST", "path": "/naatre/worker"},
            [{"type": "http.request", "body": body, "more_body": False}],
        )
        response = strict_json_loads(sent[-1]["body"])  # type: ignore[arg-type]
        assert isinstance(response, dict)
        assert response["kind"] == "result"
        payload = response["payload"]
        assert isinstance(payload, dict)
        assert payload["data"] == {"greeting": "Hello, Ada"}

        blocked_body = canonical_json(
            {
                "protocol": "naatre.remote-worker.v1",
                "kind": "invoke",
                "payload": invocation("disconnect", {"name": "block"}),
            }
        )
        await call_asgi(
            app,
            {"type": "http", "method": "POST", "path": "/naatre/worker"},
            [
                {"type": "http.request", "body": blocked_body, "more_body": False},
                {"type": "http.disconnect"},
            ],
        )
        assert cancelled.is_set()
        assert transactions.entered == transactions.exited == 2
        assert transactions.failures == 1
        await app.worker.aclose()

    asyncio.run(scenario())


def test_parent_cancellation_closes_handler_transaction_and_disconnect_task() -> None:
    handler_started = asyncio.Event()
    handler_cancelled = asyncio.Event()
    receive_cancelled = asyncio.Event()

    async def block_until_cancelled() -> None:
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            handler_cancelled.set()
            raise

    async def handler(_request: HandlerRequest[Input]) -> Output:
        handler_started.set()
        await block_until_cancelled()
        raise AssertionError("unreachable")

    async def scenario() -> None:
        transactions = TransactionProbe()
        app = app_with_handler(handler, transaction=transactions)
        body = canonical_json(
            {
                "protocol": "naatre.remote-worker.v1",
                "kind": "invoke",
                "payload": invocation("parent-cancel"),
            }
        )
        request_sent = False

        async def receive() -> dict[str, object]:
            nonlocal request_sent
            if not request_sent:
                request_sent = True
                return {"type": "http.request", "body": body, "more_body": False}
            try:
                await asyncio.Event().wait()
            finally:
                receive_cancelled.set()
            raise AssertionError("unreachable")

        async def send(message: dict[str, object]) -> None:
            raise AssertionError("cancelled request must not send a response")

        request = asyncio.create_task(
            app(
                {"type": "http", "method": "POST", "path": "/naatre/worker"},
                receive,
                send,
            )
        )
        await handler_started.wait()
        request.cancel()
        with pytest.raises(asyncio.CancelledError):
            await request

        assert handler_cancelled.is_set()
        assert receive_cancelled.is_set()
        assert transactions.entered == transactions.exited == 1
        assert transactions.failures == 1
        await app.worker.aclose()

    asyncio.run(scenario())


def test_asgi_startup_failure_closes_previously_started_pools_without_leaking_detail() -> None:
    asyncio.run(assert_startup_failure(WorkerASGI))
