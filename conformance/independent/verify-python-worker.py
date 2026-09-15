from __future__ import annotations

import asyncio
import json
import sys
from collections.abc import AsyncIterator
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "sdk/python/src"))

from naatre.worker import (
    CAPABILITY_SERVER_STREAMING,
    DataclassCodec,
    Field,
    HandlerFailure,
    HandlerRequest,
    RegistrationConfig,
    Registry,
    ScalarCodec,
    VerifiedIdentity,
    Worker,
)

FIXTURE_PATH = ROOT / "conformance/v1/remote-workers.json"


@dataclass(frozen=True, slots=True)
class GreetInput:
    name: str


@dataclass(frozen=True, slots=True)
class GreetOutput:
    greeting: str


INPUT = DataclassCodec(GreetInput, (Field("name", ScalarCodec("String")),))
OUTPUT = DataclassCodec(GreetOutput, (Field("greeting", ScalarCodec("String")),))


async def verify_identity(token: str) -> VerifiedIdentity:
    if token != "valid-delegation":
        raise AssertionError("unverified delegated context reached a handler")
    return VerifiedIdentity(subject="fixture-subject", tenant="fixture-tenant")


async def run() -> None:
    fixture = json.loads(FIXTURE_PATH.read_text(encoding="utf-8"))

    def sync_handler(request: HandlerRequest[GreetInput]) -> GreetOutput:
        if request.input.name == "reject":
            raise HandlerFailure("NAME_REJECTED", "name was rejected")
        return GreetOutput(f"Hello, {request.input.name}")

    async def async_handler(request: HandlerRequest[GreetInput]) -> GreetOutput:
        await asyncio.sleep(0)
        return sync_handler(request)

    async def exercise(asynchronous: bool) -> list[dict[str, object]]:
        registry = Registry(schema_revision=fixture["schema"]["revision"])
        if asynchronous:
            registry.bind_async("fixture.greet", INPUT, OUTPUT, async_handler)
        else:
            registry.bind_sync("fixture.greet", INPUT, OUTPUT, sync_handler)
        worker = Worker(
            registry,
            verify_identity=verify_identity,
            registration=RegistrationConfig(
                worker_id=fixture["registration"]["workerId"],
                service_identity=fixture["registration"]["serviceIdentity"],
                audience=fixture["registration"]["audience"],
                schema_digest=fixture["registration"]["schemaDigest"],
                session_id="python-fixture-session",
            ),
            maximum_in_flight=1,
        )
        results: list[dict[str, object]] = []
        try:
            registered = await worker.handle_envelope(
                {
                    "protocol": fixture["protocol"],
                    "kind": "register",
                    "payload": fixture["registration"],
                }
            )
            if registered["kind"] != "registered":
                raise AssertionError("Python worker registration was not acknowledged")
            for index, operation in enumerate(fixture["operations"]):
                result = await worker.invoke(
                    {
                        "protocol": fixture["protocol"],
                        "requestId": f"python-request-{index}",
                        "invocationId": f"python-invocation-{index}",
                        "attemptId": f"python-invocation-{index}.1",
                        "handlerId": operation["handlerId"],
                        "schemaRevision": fixture["schema"]["revision"],
                        "deadlineUnixMilli": 4_102_444_800_000,
                        "delegatedContext": "valid-delegation",
                        "input": operation["input"],
                    }
                )
                results.append({"data": result["data"], "errors": result["errors"]})
        finally:
            await worker.aclose()
        return results

    sync_results, async_results = await asyncio.gather(exercise(False), exercise(True))
    expected = [operation["expected"] for operation in fixture["operations"]]
    if sync_results != expected or async_results != expected:
        raise AssertionError("Python sync/async remote-worker reference outcomes diverged")

    closed = False

    async def source(_request: HandlerRequest[GreetInput]) -> AsyncIterator[GreetOutput]:
        nonlocal closed
        try:
            yield GreetOutput("stream-frame")
        finally:
            closed = True

    stream_registry = Registry(schema_revision=fixture["schema"]["revision"])
    stream_registry.bind_stream("fixture.greet", INPUT, OUTPUT, source)
    stream_worker = Worker(
        stream_registry,
        verify_identity=verify_identity,
        capabilities=(CAPABILITY_SERVER_STREAMING,),
    )
    session = await stream_worker.open_stream(
        {
            "protocol": fixture["protocol"],
            "requestId": "python-stream-request",
            "invocationId": "python-stream-invocation",
            "attemptId": "python-stream-invocation.1",
            "handlerId": "fixture.greet",
            "schemaRevision": fixture["schema"]["revision"],
            "deadlineUnixMilli": 4_102_444_800_000,
            "delegatedContext": "valid-delegation",
            "input": {"name": "Ada"},
        }
    )
    await session.grant(frames=1, bytes=4096)
    if await anext(session) != b'{"greeting":"stream-frame"}':
        raise AssertionError("Python stream output mismatch")
    await session.aclose()
    await stream_worker.aclose()
    if not closed:
        raise AssertionError("Python stream source was not closed")

    print(
        json.dumps(
            {
                "profile": fixture["profile"],
                "scope": "python-worker-handler-slice",
                "status": "passed",
                "capabilities": [CAPABILITY_SERVER_STREAMING],
                "nativeRuntime": False,
                "combinedCertificationOwnerIssue": 69,
            },
            separators=(",", ":"),
        )
    )


if __name__ == "__main__":
    asyncio.run(run())
