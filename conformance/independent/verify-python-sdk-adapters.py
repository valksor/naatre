from __future__ import annotations

import asyncio
import contextvars
import hashlib
import importlib.metadata
import json
import subprocess
import sys
import threading
from email.message import Message
from pathlib import Path
from typing import Self
from unittest.mock import patch
from urllib.request import Request

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "sdk/python/src"))

import naatre.transport as transport_module
from naatre import (
    AsyncClient,
    AsyncHTTPTransport,
    ExecutorLimits,
    HTTPTransport,
    NaatreClientError,
    SyncClient,
    ThreadedAsyncTransport,
    TransportError,
)
from naatre.generated.operations import (
    GetAccountVariables,
    create_get_account,
)
from naatre.pydantic import model_decoder, model_variables
from pydantic import BaseModel, ConfigDict, ValidationError

PROFILE_PATH = ROOT / "conformance/v1/python-sdk-adapters.json"
RESPONSE = b'{"complete":true,"data":null,"errors":[]}'


class FixtureResponse:
    def __init__(self, body: bytes) -> None:
        self.status = 200
        self.body = body
        self.headers = Message()
        self.headers["Content-Type"] = "application/vnd.naatre.response+json;version=1"

    def read(self, amount: int = -1) -> bytes:
        return self.body if amount < 0 else self.body[:amount]

    def close(self) -> None:
        pass

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


def make_http_transport(opener: FixtureOpener, **kwargs: object) -> HTTPTransport:
    with patch.object(transport_module, "build_opener", return_value=opener):
        return HTTPTransport("https://api.example.test/v1/execute", **kwargs)


def make_async_http_transport(opener: FixtureOpener) -> AsyncHTTPTransport:
    with patch.object(transport_module, "build_opener", return_value=opener):
        return AsyncHTTPTransport("https://api.example.test/v1/execute")


def digest(path: str) -> str:
    return hashlib.sha256((ROOT / path).read_bytes()).hexdigest()


def verify_dependencies(profile: dict[str, object]) -> None:
    dependencies = profile.get("dependencies")
    if not isinstance(dependencies, list) or len(dependencies) != 1:
        raise AssertionError("invalid Python adapter dependency inventory")
    dependency = dependencies[0]
    if not isinstance(dependency, dict):
        raise TypeError("invalid Python adapter dependency")
    commit = dependency.get("gitCommit")
    fixture = dependency.get("fixture")
    if not isinstance(commit, str) or not isinstance(fixture, dict):
        raise TypeError("invalid Python adapter dependency revision")
    path = fixture.get("path")
    expected = fixture.get("sha256")
    if not isinstance(path, str) or not isinstance(expected, str) or digest(path) != expected:
        raise AssertionError("Python core dependency fixture mismatch")
    result = subprocess.run(
        ["git", "merge-base", "--is-ancestor", commit, "HEAD"],
        cwd=ROOT,
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise AssertionError("Python core dependency revision is not present")
    pydantic = profile.get("pydantic")
    if not isinstance(pydantic, dict):
        raise TypeError("missing Pydantic dependency revision")
    if importlib.metadata.version("pydantic") != pydantic.get("version"):
        raise AssertionError("Pydantic version mismatch")
    if importlib.metadata.version("pydantic-core") != pydantic.get("coreVersion"):
        raise AssertionError("pydantic-core version mismatch")


async def verify_async_transport(expected_request: bytes) -> None:
    async_opener = FixtureOpener(FixtureResponse(RESPONSE))
    threaded = make_async_http_transport(async_opener)
    operation = create_get_account(GetAccountVariables(id="acct-1", nickname=None))
    result = await AsyncClient(threaded).execute(operation)
    await threaded.aclose()
    if result.complete is not True or async_opener.requests[0].data != expected_request:
        raise AssertionError("async HTTP canonical request mismatch")

    blocking = BlockingTransport()
    bounded = ThreadedAsyncTransport(blocking, limits=ExecutorLimits(1, 0))
    running = asyncio.create_task(bounded.execute(b"running"))
    await asyncio.to_thread(blocking.started.wait)
    try:
        await bounded.execute(b"over-limit")
    except TransportError as error:
        if error.code != "CLIENT_EXECUTOR_SATURATED":
            raise
    else:
        raise AssertionError("bounded executor accepted excess work")
    running.cancel()
    try:
        await running
    except asyncio.CancelledError:
        pass
    else:
        raise AssertionError("async cancellation was translated or ignored")
    if blocking.finished.is_set():
        raise AssertionError("cancellation claimed blocking thread completion")
    blocking.release.set()
    await asyncio.to_thread(blocking.finished.wait)
    await bounded.aclose()


async def verify_context_isolation() -> None:
    marker = contextvars.ContextVar("adapter-marker", default="caller-default")

    class ContextTransport:
        def __init__(self) -> None:
            self.values: list[str] = []

        def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
            self.values.append(marker.get())
            marker.set("worker-mutation")
            return RESPONSE

    backend = ContextTransport()
    transport = ThreadedAsyncTransport(backend, limits=ExecutorLimits(1, 0))
    token = marker.set("first-request")
    await transport.execute(b"first")
    marker.reset(token)
    await transport.execute(b"second")
    await transport.aclose()
    if backend.values != ["first-request", "caller-default"]:
        raise AssertionError("context state leaked between requests")


def verify_failures() -> None:
    secret = "Bearer conformance-secret"
    transport = make_http_transport(
        FixtureOpener(OSError(f"protected failure: {secret}")),
        headers={"Authorization": secret, "Naatre-Tenant": "protected-tenant"},
    )
    try:
        transport.execute(b'{"variables":{"secret":"protected-variable"}}')
    except TransportError as error:
        if (
            error.code != "CLIENT_TRANSPORT_ERROR"
            or str(error) != "CLIENT_TRANSPORT_ERROR"
            or error.__cause__ is not None
            or error.__context__ is not None
        ):
            raise AssertionError("transport failure exposed protected details") from None
    else:
        raise AssertionError("transport failure was accepted")

    limited = make_http_transport(
        FixtureOpener(FixtureResponse(b"12345")), maximum_response_bytes=4
    )
    try:
        limited.execute(b"request")
    except TransportError as error:
        if error.code != "CLIENT_RESPONSE_LIMIT":
            raise
    else:
        raise AssertionError("response limit was not enforced")


def verify_pydantic() -> None:
    class Variables(BaseModel):
        model_config = ConfigDict(strict=True, extra="forbid", frozen=True)

        identifier: int
        nickname: str | None = None

    class Result(BaseModel):
        model_config = ConfigDict(strict=True, extra="forbid", frozen=True)

        count: int

    if model_variables(Variables(identifier=7)) != {"identifier": 7}:
        raise AssertionError("Pydantic unset fields were not omitted")
    try:
        Variables(identifier="7")  # type: ignore[arg-type]
    except ValidationError:
        strict_rejected = True
    else:
        strict_rejected = False
    if not strict_rejected:
        raise AssertionError("Pydantic strict input accepted coercion")
    try:
        model_decoder(Result)({"count": "protected-result"})
    except NaatreClientError as error:
        if error.code != "CLIENT_RESULT_INVALID" or "protected-result" in str(error):
            raise AssertionError("Pydantic failure was not stable and redacted") from None
    else:
        raise AssertionError("Pydantic strict result accepted coercion")
    try:
        model_decoder(Result)({"count": 1, "implementationOnly": "hidden"})
    except NaatreClientError as error:
        if error.code != "CLIENT_RESULT_INVALID":
            raise
    else:
        raise AssertionError("Pydantic result accepted extra fields")


def verify_behavior() -> None:
    operation = create_get_account(GetAccountVariables(id="acct-1", nickname=None))
    sync_opener = FixtureOpener(FixtureResponse(RESPONSE))
    result = SyncClient(make_http_transport(sync_opener)).execute(operation)
    expected_request = operation.canonical_request()
    if result.complete is not True or sync_opener.requests[0].data != expected_request:
        raise AssertionError("sync HTTP canonical request mismatch")
    asyncio.run(verify_async_transport(expected_request))
    asyncio.run(verify_context_isolation())
    verify_failures()
    verify_pydantic()


def main() -> None:
    profile = json.loads(PROFILE_PATH.read_text(encoding="utf-8"))
    if profile.get("profile") != "sdk.python.adapters-1":
        raise AssertionError("invalid Python adapter profile")
    evidence = profile.get("evidence")
    if not isinstance(evidence, list):
        raise TypeError("missing Python adapter evidence")
    for entry in evidence:
        if not isinstance(entry, dict):
            raise TypeError("invalid Python adapter evidence")
        path, expected = entry.get("path"), entry.get("sha256")
        if not isinstance(path, str) or not isinstance(expected, str):
            raise TypeError("invalid Python adapter evidence")
        if digest(path) != expected:
            raise AssertionError(f"Python adapter evidence mismatch: {path}")
    verify_dependencies(profile)
    verify_behavior()
    print(
        json.dumps(
            {
                "profile": profile["profile"],
                "status": "passed",
                "pydantic": importlib.metadata.version("pydantic"),
            },
            separators=(",", ":"),
        )
    )


if __name__ == "__main__":
    main()
