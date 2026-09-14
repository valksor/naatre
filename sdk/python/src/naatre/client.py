from __future__ import annotations

from collections.abc import AsyncGenerator, AsyncIterator, Awaitable, Callable, Iterator
from dataclasses import dataclass
from typing import Generic, Protocol, TypeVar

from .errors import NaatreClientError, TransportError
from .operation import Operation, OperationResult
from .stream import AsyncCloseable, AsyncStreamTransport, StreamFrame, decode_sse

VariablesT = TypeVar("VariablesT")
ResultT = TypeVar("ResultT")
PageT = TypeVar("PageT")


class SyncTransport(Protocol):
    def execute(self, request: bytes, *, timeout: float | None = None) -> bytes: ...


class AsyncTransport(Protocol):
    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes: ...


@dataclass(frozen=True, slots=True)
class RetryPolicy:
    maximum_attempts: int = 1

    def __post_init__(self) -> None:
        if self.maximum_attempts < 1 or self.maximum_attempts > 8:
            raise ValueError("maximum_attempts must be between 1 and 8")


@dataclass(frozen=True, slots=True)
class Page(Generic[PageT]):
    items: tuple[PageT, ...]
    next_cursor: str | None = None


class SyncClient:
    def __init__(self, transport: SyncTransport, *, retry: RetryPolicy | None = None) -> None:
        self._transport = transport
        self._retry = retry or RetryPolicy()

    def execute(
        self,
        operation: Operation[VariablesT, ResultT],
        *,
        timeout: float | None = None,
    ) -> OperationResult[ResultT]:
        request = operation.canonical_request()
        for attempt in range(self._retry.maximum_attempts):
            try:
                return operation.decode_result(self._transport.execute(request, timeout=timeout))
            except TransportError as error:
                if not _may_retry(operation.kind, error, attempt, self._retry):
                    raise
        raise AssertionError("retry loop exhausted")

    def pages(self, fetch: Callable[[str | None], Page[PageT]]) -> Iterator[PageT]:
        cursor: str | None = None
        while True:
            page = fetch(cursor)
            yield from page.items
            if page.next_cursor is None:
                return
            cursor = page.next_cursor


class AsyncClient:
    def __init__(
        self,
        transport: AsyncTransport,
        *,
        stream_transport: AsyncStreamTransport | None = None,
        retry: RetryPolicy | None = None,
    ) -> None:
        self._transport = transport
        self._stream_transport = stream_transport
        self._retry = retry or RetryPolicy()

    async def aclose(self) -> None:
        closed: set[int] = set()
        for resource in (self._stream_transport, self._transport):
            if isinstance(resource, AsyncCloseable) and id(resource) not in closed:
                closed.add(id(resource))
                await resource.aclose()

    async def execute(
        self,
        operation: Operation[VariablesT, ResultT],
        *,
        timeout: float | None = None,
    ) -> OperationResult[ResultT]:
        request = operation.canonical_request()
        for attempt in range(self._retry.maximum_attempts):
            try:
                payload = await self._transport.execute(request, timeout=timeout)
                return operation.decode_result(payload)
            except TransportError as error:
                if not _may_retry(operation.kind, error, attempt, self._retry):
                    raise
        raise AssertionError("retry loop exhausted")

    def stream(
        self,
        operation: Operation[VariablesT, ResultT],
        *,
        timeout: float | None = None,
    ) -> AsyncGenerator[StreamFrame, None]:
        if self._stream_transport is None:
            raise NaatreClientError("CLIENT_STREAM_UNSUPPORTED")
        stream_transport = self._stream_transport

        async def receive() -> AsyncGenerator[StreamFrame, None]:
            source = await stream_transport.open(operation.canonical_request(), timeout=timeout)
            decoder = decode_sse(source)
            try:
                async for frame in decoder:
                    yield frame
            finally:
                await decoder.aclose()

        return receive()

    async def pages(
        self,
        fetch: Callable[[str | None], Awaitable[Page[PageT]]],
    ) -> AsyncIterator[PageT]:
        cursor: str | None = None
        while True:
            page = await fetch(cursor)
            for item in page.items:
                yield item
            if page.next_cursor is None:
                return
            cursor = page.next_cursor


def _may_retry(
    operation_kind: str,
    error: TransportError,
    attempt: int,
    policy: RetryPolicy,
) -> bool:
    return operation_kind == "query" and error.retryable and attempt + 1 < policy.maximum_attempts
