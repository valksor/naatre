from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Protocol, TypeVar

T = TypeVar("T", bound="AsyncCloseable")


class AsyncCloseable(Protocol):
    async def aclose(self) -> None: ...


@asynccontextmanager
async def client_lifespan(client: T) -> AsyncIterator[T]:
    """Framework-neutral lifecycle helper suitable for an ASGI lifespan hook."""

    try:
        yield client
    finally:
        await client.aclose()
