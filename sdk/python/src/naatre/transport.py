from __future__ import annotations

import asyncio
import contextvars
import threading
from collections.abc import Mapping
from concurrent.futures import Future, ThreadPoolExecutor
from contextlib import suppress
from dataclasses import dataclass
from email.message import Message
from types import TracebackType
from typing import Protocol, TypeAlias, cast
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

from .client import SyncTransport
from .errors import NaatreClientError, TransportError

REQUEST_MEDIA_TYPE = "application/vnd.naatre.request+json;version=1"
RESPONSE_MEDIA_TYPE = "application/vnd.naatre.response+json"
_RESERVED_HEADERS = frozenset(
    {
        "accept",
        "accept-encoding",
        "content-encoding",
        "content-length",
        "content-type",
        "host",
        "naatre-principal",
    }
)


@dataclass(frozen=True, slots=True)
class ExecutorLimits:
    maximum_workers: int = 4
    maximum_pending: int = 8

    def __post_init__(self) -> None:
        if not 1 <= self.maximum_workers <= 32:
            raise NaatreClientError("CLIENT_CONFIG_INVALID")
        if not 0 <= self.maximum_pending <= 256:
            raise NaatreClientError("CLIENT_CONFIG_INVALID")

    @property
    def capacity(self) -> int:
        return self.maximum_workers + self.maximum_pending


class ThreadedAsyncTransport:
    """Run a blocking transport with explicit worker and submission bounds."""

    def __init__(
        self,
        transport: SyncTransport,
        *,
        limits: ExecutorLimits | None = None,
    ) -> None:
        self._transport = transport
        self._limits = limits or ExecutorLimits()
        self._executor = ThreadPoolExecutor(
            max_workers=self._limits.maximum_workers,
            thread_name_prefix="naatre-transport",
        )
        self._lock = threading.Lock()
        self._submitted = 0
        self._closed = False

    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        with self._lock:
            if self._closed:
                raise TransportError("CLIENT_TRANSPORT_CLOSED")
            if self._submitted >= self._limits.capacity:
                raise TransportError("CLIENT_EXECUTOR_SATURATED", retryable=True)
            self._submitted += 1

        context = contextvars.copy_context()
        try:
            future = self._executor.submit(_invoke_sync, context, self._transport, request, timeout)
        except RuntimeError:
            self._release_slot()
            raise TransportError("CLIENT_TRANSPORT_CLOSED") from None
        future.add_done_callback(self._completed)
        return await asyncio.wrap_future(future)

    async def aclose(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self._executor.shutdown(wait=False, cancel_futures=True)

    def _completed(self, _future: Future[bytes]) -> None:
        self._release_slot()

    def _release_slot(self) -> None:
        with self._lock:
            self._submitted -= 1


class _ResponseHeaders(Protocol):
    def get(self, name: str, default: str | None = None) -> str | None: ...

    def get_all(self, name: str, failobj: list[str] | None = None) -> list[str] | None: ...


class _Response(Protocol):
    status: int
    headers: _ResponseHeaders

    def read(self, amount: int = -1) -> bytes: ...

    def close(self) -> None: ...

    def __enter__(self) -> _Response: ...

    def __exit__(
        self,
        exception_type: type[BaseException] | None,
        exception: BaseException | None,
        traceback: TracebackType | None,
    ) -> bool | None: ...


class _Opener(Protocol):
    def open(self, request: Request, timeout: float | None = None) -> _Response: ...


class _RejectRedirects(HTTPRedirectHandler):
    def redirect_request(
        self,
        req: Request,
        fp: object,
        code: int,
        msg: str,
        headers: Message,
        newurl: str,
    ) -> None:
        return None


_ExchangeResult: TypeAlias = bytes | NaatreClientError


class HTTPTransport:
    """Bounded unary HTTP transport for the core request/result envelope."""

    def __init__(
        self,
        endpoint: str,
        *,
        headers: Mapping[str, str] | None = None,
        maximum_response_bytes: int = 16 << 20,
    ) -> None:
        self._endpoint = _validate_endpoint(endpoint)
        self._headers = _validate_headers(headers or {})
        if not 1 <= maximum_response_bytes <= 1 << 30:
            raise NaatreClientError("CLIENT_CONFIG_INVALID")
        self._maximum_response_bytes = maximum_response_bytes
        self._opener = cast(_Opener, build_opener(_RejectRedirects()))

    def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        if timeout is not None and timeout <= 0:
            raise NaatreClientError("CLIENT_CONFIG_INVALID")
        result = self._exchange(request, timeout)
        if isinstance(result, NaatreClientError):
            raise result
        return result

    def _exchange(self, body: bytes, timeout: float | None) -> _ExchangeResult:
        request = Request(
            self._endpoint,
            data=body,
            headers={
                **self._headers,
                "Accept": f"{RESPONSE_MEDIA_TYPE};version=1",
                "Accept-Encoding": "identity",
                "Content-Type": REQUEST_MEDIA_TYPE,
            },
            method="POST",
        )
        try:
            with self._opener.open(request, timeout=timeout) as response:
                return _read_response(response, self._maximum_response_bytes)
        except NaatreClientError as error:
            return error
        except HTTPError as error:
            with suppress(Exception):
                error.close()
            code = "CLIENT_REDIRECT_REJECTED" if 300 <= error.code < 400 else "CLIENT_HTTP_STATUS"
            return TransportError(code)
        except (TimeoutError, URLError, OSError):
            return TransportError("CLIENT_TRANSPORT_ERROR", retryable=True)
        except Exception:
            return TransportError("CLIENT_TRANSPORT_ERROR")


class AsyncHTTPTransport:
    """Thread-backed asyncio view of :class:`HTTPTransport`."""

    def __init__(
        self,
        endpoint: str,
        *,
        headers: Mapping[str, str] | None = None,
        maximum_response_bytes: int = 16 << 20,
        executor_limits: ExecutorLimits | None = None,
    ) -> None:
        self._transport = ThreadedAsyncTransport(
            HTTPTransport(
                endpoint,
                headers=headers,
                maximum_response_bytes=maximum_response_bytes,
            ),
            limits=executor_limits,
        )

    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        return await self._transport.execute(request, timeout=timeout)

    async def aclose(self) -> None:
        await self._transport.aclose()


def _invoke_sync(
    context: contextvars.Context,
    transport: SyncTransport,
    request: bytes,
    timeout: float | None,
) -> bytes:
    result = context.run(_capture_sync, transport, request, timeout)
    if isinstance(result, NaatreClientError):
        raise result
    return result


def _capture_sync(
    transport: SyncTransport,
    request: bytes,
    timeout: float | None,
) -> _ExchangeResult:
    try:
        return transport.execute(request, timeout=timeout)
    except NaatreClientError as error:
        return _redacted_error(error)
    except Exception:
        return TransportError("CLIENT_TRANSPORT_ERROR")


def _redacted_error(error: NaatreClientError) -> NaatreClientError:
    code = error.code
    if not code or len(code) > 64 or any(
        character not in "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_" for character in code
    ):
        return TransportError("CLIENT_TRANSPORT_ERROR")
    if isinstance(error, TransportError):
        return TransportError(code, retryable=error.retryable)
    return NaatreClientError(code)


def _validate_endpoint(endpoint: str) -> str:
    try:
        parsed = urlsplit(endpoint)
        port = parsed.port
    except (TypeError, ValueError):
        raise NaatreClientError("CLIENT_CONFIG_INVALID") from None
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.fragment
        or port is not None
        and not 1 <= port <= 65535
    ):
        raise NaatreClientError("CLIENT_CONFIG_INVALID")
    return endpoint


def _validate_headers(headers: Mapping[str, str]) -> dict[str, str]:
    validated: dict[str, str] = {}
    for name, value in headers.items():
        if (
            not isinstance(name, str)
            or not isinstance(value, str)
            or not name
            or name.lower() in _RESERVED_HEADERS
            or any(character in name or character in value for character in "\r\n")
        ):
            raise NaatreClientError("CLIENT_CONFIG_INVALID")
        validated[name] = value
    return validated


def _read_response(response: _Response, maximum_bytes: int) -> bytes:
    if response.status < 200 or response.status >= 300:
        raise TransportError("CLIENT_HTTP_STATUS")
    _require_singleton_header(response.headers, "Content-Type")
    _require_singleton_header(response.headers, "Content-Encoding")
    _require_singleton_header(response.headers, "Content-Length")
    _require_response_media_type(response.headers.get("Content-Type"))
    encoding = (response.headers.get("Content-Encoding") or "identity").strip().lower()
    if encoding != "identity":
        raise TransportError("CLIENT_CONTENT_ENCODING_UNSUPPORTED")
    declared_length = _content_length(response.headers.get("Content-Length"))
    if declared_length is not None and declared_length > maximum_bytes:
        raise TransportError("CLIENT_RESPONSE_LIMIT")
    body = response.read(maximum_bytes + 1)
    if not isinstance(body, bytes):
        raise TransportError("CLIENT_MALFORMED_RESPONSE")
    if len(body) > maximum_bytes:
        raise TransportError("CLIENT_RESPONSE_LIMIT")
    if declared_length is not None and len(body) != declared_length:
        raise TransportError("CLIENT_TRUNCATED_RESPONSE")
    return body


def _require_singleton_header(headers: _ResponseHeaders, name: str) -> None:
    values = headers.get_all(name, [])
    if values is not None and len(values) > 1:
        raise TransportError("CLIENT_MALFORMED_RESPONSE")


def _require_response_media_type(value: str | None) -> None:
    if value is None:
        raise TransportError("CLIENT_MALFORMED_RESPONSE")
    message = Message()
    message["content-type"] = value
    if message.get_content_type().lower() != RESPONSE_MEDIA_TYPE:
        raise TransportError("CLIENT_MALFORMED_RESPONSE")
    raw_parameters = message.get_params() or []
    parameters = {name.lower(): parameter for name, parameter in raw_parameters[1:]}
    if parameters.get("version") != "1" or any(
        name not in {"version", "charset"} for name in parameters
    ):
        raise TransportError("CLIENT_MALFORMED_RESPONSE")
    charset = parameters.get("charset")
    if charset is not None and charset.lower() != "utf-8":
        raise TransportError("CLIENT_MALFORMED_RESPONSE")


def _content_length(value: str | None) -> int | None:
    if value is None:
        return None
    if not value.isascii() or not value.isdecimal():
        raise TransportError("CLIENT_MALFORMED_RESPONSE")
    length = int(value)
    if length < 0:
        raise TransportError("CLIENT_MALFORMED_RESPONSE")
    return length
