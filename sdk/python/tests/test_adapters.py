from __future__ import annotations

import asyncio
import contextvars
import threading
from collections.abc import Mapping
from email.message import Message
from typing import Self
from unittest.mock import patch
from urllib.request import Request

import pytest

from naatre import (
    AsyncClient,
    AsyncHTTPTransport,
    ExecutorLimits,
    HTTPTransport,
    SyncClient,
    ThreadedAsyncTransport,
    TransportError,
)
from naatre.generated.operations import GetAccountVariables, create_get_account

RESPONSE = b'{"complete":true,"data":null,"errors":[]}'


class FixtureResponse:
    def __init__(self, body: bytes, *, content_length: str | None = None) -> None:
        self.status = 200
        self.body = body
        self.closed = False
        self.headers = Message()
        self.headers["Content-Type"] = "application/vnd.naatre.response+json;version=1"
        if content_length is not None:
            self.headers["Content-Length"] = content_length

    def read(self, amount: int = -1) -> bytes:
        return self.body if amount < 0 else self.body[:amount]

    def close(self) -> None:
        self.closed = True

    def __enter__(self) -> Self:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()


class FixtureOpener:
    def __init__(self, response: FixtureResponse | Exception) -> None:
        self.response = response
        self.requests: list[Request] = []

    def open(self, request: Request, timeout: float | None = None) -> FixtureResponse:
        self.requests.append(request)
        if isinstance(self.response, Exception):
            raise self.response
        return self.response


class BlockingTransport:
    def __init__(self) -> None:
        self.started = threading.Event()
        self.release = threading.Event()
        self.finished = threading.Event()

    def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        self.started.set()
        self.release.wait()
        self.finished.set()
        return RESPONSE


def http_transport(
    opener: FixtureOpener,
    *,
    headers: Mapping[str, str] | None = None,
    maximum_response_bytes: int = 16 << 20,
) -> HTTPTransport:
    with patch("naatre.transport.build_opener", return_value=opener):
        return HTTPTransport(
            "https://api.example.test/v1/execute",
            headers=headers,
            maximum_response_bytes=maximum_response_bytes,
        )


def test_sync_and_threaded_async_http_emit_identical_canonical_requests() -> None:
    sync_opener = FixtureOpener(FixtureResponse(RESPONSE))
    async_opener = FixtureOpener(FixtureResponse(RESPONSE))
    operation = create_get_account(GetAccountVariables(id="acct-1", nickname=None))
    sync_transport = http_transport(sync_opener)
    with patch("naatre.transport.build_opener", return_value=async_opener):
        async_transport = AsyncHTTPTransport("https://api.example.test/v1/execute")

    sync_result = SyncClient(sync_transport).execute(operation)
    async_result = asyncio.run(AsyncClient(async_transport).execute(operation))
    asyncio.run(async_transport.aclose())

    assert sync_opener.requests[0].data == async_opener.requests[0].data
    assert sync_opener.requests[0].data == operation.canonical_request()
    assert sync_result == async_result


def test_async_cancellation_does_not_claim_running_thread_stopped() -> None:
    async def scenario() -> BlockingTransport:
        blocking = BlockingTransport()
        transport = ThreadedAsyncTransport(blocking, limits=ExecutorLimits(1, 0))
        task = asyncio.create_task(transport.execute(b"request"))
        await asyncio.to_thread(blocking.started.wait)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert blocking.finished.is_set() is False
        blocking.release.set()
        await asyncio.to_thread(blocking.finished.wait)
        await transport.aclose()
        return blocking

    assert asyncio.run(scenario()).finished.is_set() is True


def test_executor_rejects_work_over_configured_capacity() -> None:
    async def scenario() -> str:
        blocking = BlockingTransport()
        transport = ThreadedAsyncTransport(blocking, limits=ExecutorLimits(1, 0))
        first = asyncio.create_task(transport.execute(b"first"))
        await asyncio.to_thread(blocking.started.wait)
        with pytest.raises(TransportError) as failure:
            await transport.execute(b"second")
        blocking.release.set()
        await first
        await transport.aclose()
        return failure.value.code

    assert asyncio.run(scenario()) == "CLIENT_EXECUTOR_SATURATED"


def test_executor_context_is_isolated_between_requests() -> None:
    marker = contextvars.ContextVar("marker", default="caller-default")

    class ContextTransport:
        def __init__(self) -> None:
            self.values: list[str] = []

        def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
            self.values.append(marker.get())
            marker.set("worker-mutation")
            return RESPONSE

    async def scenario() -> list[str]:
        backend = ContextTransport()
        transport = ThreadedAsyncTransport(backend, limits=ExecutorLimits(1, 0))
        token = marker.set("first-request")
        await transport.execute(b"first")
        marker.reset(token)
        await transport.execute(b"second")
        await transport.aclose()
        return backend.values

    assert asyncio.run(scenario()) == ["first-request", "caller-default"]


def test_http_response_limit_is_enforced_before_full_acceptance() -> None:
    transport = http_transport(FixtureOpener(FixtureResponse(b"12345")), maximum_response_bytes=4)

    with pytest.raises(TransportError) as failure:
        transport.execute(b"request")

    assert failure.value.code == "CLIENT_RESPONSE_LIMIT"


def test_transport_failure_does_not_expose_credentials_or_cause() -> None:
    secret = "Bearer local-secret"
    transport = http_transport(
        FixtureOpener(OSError(f"failed with {secret}")),
        headers={"Authorization": secret, "Naatre-Tenant": "protected-tenant"},
    )

    with pytest.raises(TransportError) as failure:
        transport.execute(b'{"variables":{"token":"protected-variable"}}')

    assert failure.value.code == "CLIENT_TRANSPORT_ERROR"
    assert str(failure.value) == "CLIENT_TRANSPORT_ERROR"
    assert failure.value.__cause__ is None
    assert failure.value.__context__ is None
