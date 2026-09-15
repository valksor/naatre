from __future__ import annotations

import asyncio
from typing import Protocol

from .errors import NaatreClientError
from .json import canonical_json, strict_json_loads
from .worker import Worker, WorkerProtocolError

WORKER_MEDIA_TYPE = b"application/naatre-worker+json"


class Receive(Protocol):
    async def __call__(self) -> dict[str, object]: ...


class Send(Protocol):
    async def __call__(self, message: dict[str, object]) -> None: ...


class WorkerASGI:
    """Dependency-free ASGI worker endpoint; framework adapters remain additive."""

    def __init__(
        self,
        worker: Worker,
        *,
        path: str = "/naatre/worker",
        maximum_request_bytes: int = 1 << 20,
    ) -> None:
        if not path.startswith("/") or not 1 <= maximum_request_bytes <= 64 << 20:
            raise WorkerProtocolError("WORKER_CONFIG_INVALID")
        self.worker = worker
        self._path = path
        self._maximum_request_bytes = maximum_request_bytes

    async def __call__(self, scope: dict[str, object], receive: Receive, send: Send) -> None:
        scope_type = scope.get("type")
        if scope_type == "lifespan":
            await self._lifespan(receive, send)
            return
        if scope_type != "http":
            raise WorkerProtocolError("WORKER_ASGI_SCOPE_UNSUPPORTED")
        await self._http(scope, receive, send)

    async def _lifespan(self, receive: Receive, send: Send) -> None:
        while True:
            message = await receive()
            kind = message.get("type")
            if kind == "lifespan.startup":
                try:
                    await self.worker.start()
                except Exception:
                    await send(
                        {
                            "type": "lifespan.startup.failed",
                            "message": "WORKER_STARTUP_FAILED",
                        }
                    )
                    return
                else:
                    await send({"type": "lifespan.startup.complete"})
            elif kind == "lifespan.shutdown":
                try:
                    await self.worker.aclose()
                except Exception:
                    await send(
                        {
                            "type": "lifespan.shutdown.failed",
                            "message": "WORKER_SHUTDOWN_FAILED",
                        }
                    )
                else:
                    await send({"type": "lifespan.shutdown.complete"})
                return
            else:
                raise WorkerProtocolError("WORKER_ASGI_SCOPE_UNSUPPORTED")

    async def _http(self, scope: dict[str, object], receive: Receive, send: Send) -> None:
        if scope.get("method") != "POST" or scope.get("path") != self._path:
            await self._response(send, 404, {"code": "WORKER_ROUTE_NOT_FOUND"})
            return
        try:
            body = await self._body(receive)
            if body is None:
                return
            envelope = strict_json_loads(body, maximum_bytes=self._maximum_request_bytes)
            work = asyncio.create_task(self.worker.handle_envelope(envelope))
            disconnect = asyncio.create_task(self._disconnect(receive))
            try:
                done, _pending = await asyncio.wait(
                    (work, disconnect), return_when=asyncio.FIRST_COMPLETED
                )
                if disconnect in done:
                    return
                await self._response(send, 200, await work)
            finally:
                for task in (work, disconnect):
                    if not task.done():
                        task.cancel()
                await asyncio.gather(work, disconnect, return_exceptions=True)
        except WorkerProtocolError as error:
            await self._response(send, 400, {"code": error.code})
        except NaatreClientError:
            await self._response(send, 400, {"code": "REMOTE_INVOCATION_INVALID"})

    async def _body(self, receive: Receive) -> bytes | None:
        body = bytearray()
        while True:
            message = await receive()
            if message.get("type") == "http.disconnect":
                return None
            if message.get("type") != "http.request":
                raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
            chunk = message.get("body", b"")
            if not isinstance(chunk, bytes):
                raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
            body.extend(chunk)
            if len(body) > self._maximum_request_bytes:
                raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
            if message.get("more_body") is not True:
                return bytes(body)

    async def _disconnect(self, receive: Receive) -> None:
        while True:
            if (await receive()).get("type") == "http.disconnect":
                return

    async def _response(self, send: Send, status: int, value: object) -> None:
        body = canonical_json(value)
        await send(
            {
                "type": "http.response.start",
                "status": status,
                "headers": [
                    (b"content-type", WORKER_MEDIA_TYPE),
                    (b"content-length", str(len(body)).encode("ascii")),
                ],
            }
        )
        await send({"type": "http.response.body", "body": body, "more_body": False})
