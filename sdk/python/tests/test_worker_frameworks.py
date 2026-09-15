from __future__ import annotations

import asyncio
import sys
from typing import Protocol
from unittest.mock import patch

import pytest
from test_worker import Input, Output
from test_worker_asgi import app_with_handler, assert_startup_failure, call_asgi

from naatre.json import canonical_json, strict_json_loads
from naatre.worker import HandlerRequest, Worker, WorkerProtocolError
from naatre.worker_asgi import (
    FrameworkWorkerASGI,
    fastapi_worker_app,
    starlette_worker_app,
)


class Factory(Protocol):
    def __call__(
        self,
        worker: Worker,
        *,
        path: str = "/naatre/worker",
        maximum_request_bytes: int = 1 << 20,
    ) -> FrameworkWorkerASGI: ...


def http_scope(path: str = "/naatre/worker", method: str = "POST") -> dict[str, object]:
    return {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": method,
        "scheme": "https",
        "path": path,
        "raw_path": path.encode("ascii"),
        "query_string": b"",
        "headers": [],
        "client": ("127.0.0.1", 1),
        "server": ("worker.example", 443),
    }


@pytest.mark.parametrize(
    ("factory", "module"),
    (
        (starlette_worker_app, "starlette.applications"),
        (fastapi_worker_app, "fastapi.applications"),
    ),
)
def test_framework_adapters_route_exact_worker_protocol_without_listener(
    factory: Factory,
    module: str,
) -> None:
    async def handler(request: HandlerRequest[Input]) -> Output:
        return Output(f"Hello, {request.input.name}")

    async def scenario() -> None:
        neutral = app_with_handler(handler)
        application = factory(neutral.worker)
        assert application.application.__class__.__module__ == module

        body = canonical_json(
            {
                "protocol": "naatre.remote-worker.v1",
                "kind": "invoke",
                "payload": {
                    "protocol": "naatre.remote-worker.v1",
                    "requestId": "framework-request",
                    "invocationId": "framework-invocation",
                    "attemptId": "framework-invocation.1",
                    "handlerId": "fixture.greet",
                    "schemaRevision": "schema-1",
                    "deadlineUnixMilli": 4_102_444_800_000,
                    "delegatedContext": "verified-token",
                    "input": {"name": "Ada"},
                },
            }
        )
        response = await call_asgi(
            application,
            http_scope(),
            [{"type": "http.request", "body": body, "more_body": False}],
        )
        decoded = strict_json_loads(response[-1]["body"])  # type: ignore[arg-type]
        assert isinstance(decoded, dict)
        assert decoded["kind"] == "result"

        wrong_method = await call_asgi(
            application,
            http_scope(method="GET"),
            [{"type": "http.request", "body": b"", "more_body": False}],
        )
        assert strict_json_loads(wrong_method[-1]["body"]) == {  # type: ignore[arg-type]
            "code": "WORKER_ROUTE_NOT_FOUND"
        }
        await application.worker.aclose()

    asyncio.run(scenario())


@pytest.mark.parametrize("factory", (starlette_worker_app, fastapi_worker_app))
def test_framework_lifespan_bounds_pools_and_redacts_startup_failure(factory: Factory) -> None:
    asyncio.run(assert_startup_failure(factory))


def test_framework_dependency_failure_has_one_stable_public_code() -> None:
    async def handler(request: HandlerRequest[Input]) -> Output:
        return Output(request.input.name)

    worker = app_with_handler(handler).worker
    with patch.dict(sys.modules, {"fastapi": None}):
        with pytest.raises(WorkerProtocolError) as caught:
            fastapi_worker_app(worker)
    assert caught.value.code == "WORKER_FRAMEWORK_UNAVAILABLE"
    assert str(caught.value) == "WORKER_FRAMEWORK_UNAVAILABLE"
    assert caught.value.__cause__ is None
    assert caught.value.__context__ is None
    asyncio.run(worker.aclose())


@pytest.mark.parametrize("factory", (starlette_worker_app, fastapi_worker_app))
def test_framework_request_limit_never_echoes_protected_body(factory: Factory) -> None:
    async def handler(request: HandlerRequest[Input]) -> Output:
        return Output(request.input.name)

    async def scenario() -> bytes:
        application = factory(app_with_handler(handler).worker, maximum_request_bytes=16)
        messages = await call_asgi(
            application,
            http_scope(),
            [
                {
                    "type": "http.request",
                    "body": b"Bearer protected-credential-and-metadata",
                    "more_body": False,
                }
            ],
        )
        await application.worker.aclose()
        body = messages[-1]["body"]
        assert isinstance(body, bytes)
        return body

    body = asyncio.run(scenario())
    assert strict_json_loads(body) == {"code": "REMOTE_INVOCATION_INVALID"}
    assert b"protected" not in body
