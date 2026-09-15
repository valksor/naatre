from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from importlib import import_module
from typing import Protocol, cast

from .errors import NaatreClientError
from .json import canonical_json, strict_json_loads
from .worker import Worker, WorkerProtocolError

WORKER_MEDIA_TYPE = b"application/naatre-worker+json"


class Receive(Protocol):
    async def __call__(self) -> dict[str, object]: ...


class Send(Protocol):
    async def __call__(self, message: dict[str, object]) -> None: ...


class ASGIApplication(Protocol):
    async def __call__(
        self,
        scope: dict[str, object],
        receive: Receive,
        send: Send,
    ) -> None: ...


class _WorkerLifespan:
    def __init__(self, worker: Worker) -> None:
        self._worker = worker
        self._lock = asyncio.Lock()

    async def startup(self, send: Send) -> bool:
        return await self._run(self._worker.start, send, "startup")

    async def shutdown(self, send: Send) -> None:
        await self._run(self._worker.aclose, send, "shutdown")

    async def _run(
        self,
        operation: Callable[[], Awaitable[None]],
        send: Send,
        phase: str,
    ) -> bool:
        async with self._lock:
            try:
                await operation()
            except Exception:
                await send(
                    {
                        "type": f"lifespan.{phase}.failed",
                        "message": f"WORKER_{phase.upper()}_FAILED",
                    }
                )
                return False
            await send({"type": f"lifespan.{phase}.complete"})
            return True


class WorkerASGI:
    """Dependency-free ASGI worker endpoint for the remote-worker protocol."""

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
        self._lifecycle = _WorkerLifespan(worker)

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
                if not await self._lifecycle.startup(send):
                    return
            elif kind == "lifespan.shutdown":
                await self._lifecycle.shutdown(send)
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


class FrameworkWorkerASGI:
    """ASGI wrapper that gives one framework application a safe worker lifespan."""

    def __init__(self, worker_app: WorkerASGI, application: ASGIApplication) -> None:
        self.worker_app = worker_app
        self.application = application

    @property
    def worker(self) -> Worker:
        return self.worker_app.worker

    async def __call__(self, scope: dict[str, object], receive: Receive, send: Send) -> None:
        if scope.get("type") != "http":
            await self.worker_app(scope, receive, send)
            return
        await self.application(scope, receive, send)


_FrameworkSpec = tuple[str, str, dict[str, object]]
_STARLETTE_FRAMEWORK: _FrameworkSpec = ("starlette.applications", "Starlette", {})
_FASTAPI_FRAMEWORK: _FrameworkSpec = (
    "fastapi",
    "FastAPI",
    {"openapi_url": None, "docs_url": None, "redoc_url": None},
)


def starlette_worker_app(
    worker: Worker,
    *,
    path: str = "/naatre/worker",
    maximum_request_bytes: int = 1 << 20,
) -> FrameworkWorkerASGI:
    """Build a Starlette application without making Starlette a core dependency."""
    return _framework_application(
        _STARLETTE_FRAMEWORK,
        worker,
        path=path,
        maximum_request_bytes=maximum_request_bytes,
    )


def fastapi_worker_app(
    worker: Worker,
    *,
    path: str = "/naatre/worker",
    maximum_request_bytes: int = 1 << 20,
) -> FrameworkWorkerASGI:
    """Build a FastAPI application without making FastAPI a core dependency."""
    return _framework_application(
        _FASTAPI_FRAMEWORK,
        worker,
        path=path,
        maximum_request_bytes=maximum_request_bytes,
    )


def _framework_application(
    framework: _FrameworkSpec,
    worker: Worker,
    *,
    path: str,
    maximum_request_bytes: int,
) -> FrameworkWorkerASGI:
    application_module, application_name, application_options = framework
    application_factory = _optional_factory(application_module, application_name)
    mount = _optional_factory("starlette.routing", "Mount")
    route = _optional_factory("starlette.routing", "Route")
    if application_factory is None or mount is None or route is None:
        raise WorkerProtocolError("WORKER_FRAMEWORK_UNAVAILABLE")

    worker_app = WorkerASGI(
        worker,
        path=path,
        maximum_request_bytes=maximum_request_bytes,
    )
    application = application_factory(
        routes=[
            route(path, endpoint=worker_app, include_in_schema=False),
            mount("/", app=worker_app),
        ],
        **application_options,
    )
    return FrameworkWorkerASGI(worker_app, cast(ASGIApplication, application))


def _optional_factory(module: str, name: str) -> Callable[..., object] | None:
    try:
        value = getattr(import_module(module), name)
    except Exception:
        return None
    if not callable(value):
        return None
    return cast(Callable[..., object], value)
