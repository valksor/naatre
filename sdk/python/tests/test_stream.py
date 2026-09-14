from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator

import pytest

from naatre import AsyncClient
from naatre.generated.operations import GetAccountVariables, create_get_account


class ClosableSource(AsyncIterator[bytes]):
    def __init__(self) -> None:
        self.closed = False
        self.sent = False
        self.release = asyncio.Event()

    def __aiter__(self) -> AsyncIterator[bytes]:
        return self

    async def __anext__(self) -> bytes:
        if not self.sent:
            self.sent = True
            return b'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n'
        await self.release.wait()
        raise StopAsyncIteration

    async def aclose(self) -> None:
        self.closed = True
        self.release.set()


class StreamTransport:
    def __init__(self, source: ClosableSource) -> None:
        self.source = source

    async def open(self, request: bytes, *, timeout: float | None = None) -> AsyncIterator[bytes]:
        return self.source


class IdleTransport:
    def __init__(self) -> None:
        self.released = asyncio.Event()

    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        try:
            await asyncio.Event().wait()
        finally:
            self.released.set()
        return b""


class UnusedAsyncTransport:
    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        raise AssertionError("unary transport should not run")


def test_async_generator_abandonment_closes_source() -> None:
    async def scenario() -> None:
        source = ClosableSource()
        client = AsyncClient(UnusedAsyncTransport(), stream_transport=StreamTransport(source))
        operation = create_get_account(GetAccountVariables(id="acct-1"))
        iterator = client.stream(operation)

        frame = await anext(iterator)
        assert frame.type == "open"
        await iterator.aclose()

        assert source.closed is True

    asyncio.run(scenario())


def test_asyncio_cancellation_releases_transport_work() -> None:
    async def scenario() -> None:
        transport = IdleTransport()
        client = AsyncClient(transport)
        operation = create_get_account(GetAccountVariables(id="acct-1"))
        task = asyncio.create_task(client.execute(operation))
        await asyncio.sleep(0)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert transport.released.is_set()

    asyncio.run(scenario())
