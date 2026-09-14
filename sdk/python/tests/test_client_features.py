from __future__ import annotations

import asyncio

import pytest

from naatre import AsyncClient, Page, RetryPolicy, SyncClient, TransportError, client_lifespan
from naatre.generated.operations import GetAccountVariables, create_get_account


class RetryingTransport:
    def __init__(self) -> None:
        self.attempts = 0

    def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        self.attempts += 1
        if self.attempts == 1:
            raise TransportError("TEMPORARY", retryable=True)
        return b'{"complete":true,"data":null,"errors":[]}'


class NeverTransport:
    def __init__(self) -> None:
        self.closed = False

    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        raise AssertionError("not used")

    async def aclose(self) -> None:
        self.closed = True


def test_retry_is_bounded_and_query_only() -> None:
    transport = RetryingTransport()
    client = SyncClient(transport, retry=RetryPolicy(maximum_attempts=2))

    client.execute(create_get_account(GetAccountVariables(id="acct-1")))

    assert transport.attempts == 2


def test_sync_pagination_preserves_item_order() -> None:
    pages = {None: Page((1, 2), "next"), "next": Page((3,), None)}
    client = SyncClient(RetryingTransport())

    assert list(client.pages(lambda cursor: pages[cursor])) == [1, 2, 3]


def test_async_pagination_preserves_item_order() -> None:
    async def scenario() -> list[int]:
        pages = {None: Page((1, 2), "next"), "next": Page((3,), None)}

        async def fetch(cursor: str | None) -> Page[int]:
            return pages[cursor]

        client = AsyncClient(NeverTransport())
        return [item async for item in client.pages(fetch)]

    assert asyncio.run(scenario()) == [1, 2, 3]


def test_asgi_lifecycle_helper_closes_client_resource_on_failure() -> None:
    async def scenario() -> NeverTransport:
        transport = NeverTransport()
        client = AsyncClient(transport)
        with pytest.raises(RuntimeError, match="application failure"):
            async with client_lifespan(client):
                raise RuntimeError("application failure")
        return transport

    assert asyncio.run(scenario()).closed is True
