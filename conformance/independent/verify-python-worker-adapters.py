from __future__ import annotations

import asyncio
import hashlib
import importlib.metadata
import json
import subprocess
import sys
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "sdk/python/src"))

from naatre.json import canonical_json, strict_json_loads  # noqa: E402
from naatre.worker import (  # noqa: E402
    CAPABILITY_TRANSACTIONS,
    CAPABILITY_UNARY,
    DataclassCodec,
    Field,
    HandlerRequest,
    Registry,
    ScalarCodec,
    VerifiedIdentity,
    Worker,
    current_request,
)
from naatre.worker_asgi import (  # noqa: E402
    FrameworkWorkerASGI,
    fastapi_worker_app,
    starlette_worker_app,
)

PROFILE_PATH = ROOT / "conformance/v1/python-worker-adapters.json"
PROTOCOL = "naatre.remote-worker.v1"


@dataclass(frozen=True, slots=True)
class Input:
    name: str


@dataclass(frozen=True, slots=True)
class Output:
    greeting: str


INPUT = DataclassCodec(Input, (Field("name", ScalarCodec("String")),))
OUTPUT = DataclassCodec(Output, (Field("greeting", ScalarCodec("String")),))


class Factory(Protocol):
    def __call__(
        self,
        worker: Worker,
        *,
        path: str = "/naatre/worker",
        maximum_request_bytes: int = 1 << 20,
    ) -> FrameworkWorkerASGI: ...


class Resource:
    def __init__(self, *, fail_start: bool = False) -> None:
        self.events: list[str] = []
        self.fail_start = fail_start

    async def start(self) -> None:
        if self.fail_start:
            raise RuntimeError("protected pool address and credential")
        self.events.append("started")

    async def drain(self) -> None:
        self.events.append("drained")

    async def aclose(self) -> None:
        self.events.append("closed")


class Transactions:
    def __init__(self) -> None:
        self.entered = 0
        self.exited = 0

    @asynccontextmanager
    async def transaction(self, _request: HandlerRequest[object]) -> AsyncIterator[None]:
        self.entered += 1
        try:
            yield
        finally:
            self.exited += 1


async def verify_identity(_delegation: str) -> VerifiedIdentity:
    return VerifiedIdentity(subject="fixture-subject", tenant="fixture-tenant")


def digest(path: str) -> str:
    return hashlib.sha256((ROOT / path).read_bytes()).hexdigest()


def verify_profile(profile: dict[str, object]) -> None:
    if profile.get("profile") != "sdk.python.worker-adapters-1":
        raise AssertionError("invalid Python worker adapter profile")
    verify_core_dependency(profile)
    verify_framework_dependencies(profile)
    verify_evidence(profile)


def verify_core_dependency(profile: dict[str, object]) -> None:
    dependency = profile.get("coreDependency")
    if not isinstance(dependency, dict):
        raise TypeError("missing Python worker core dependency")
    commit = dependency.get("gitCommit")
    fixture = dependency.get("fixture")
    if not isinstance(commit, str) or not isinstance(fixture, dict):
        raise TypeError("invalid Python worker core dependency")
    fixture_path = fixture.get("path")
    fixture_digest = fixture.get("sha256")
    if (
        not isinstance(fixture_path, str)
        or not isinstance(fixture_digest, str)
        or digest(fixture_path) != fixture_digest
    ):
        raise AssertionError("Python worker core fixture mismatch")
    present = subprocess.run(
        ["git", "merge-base", "--is-ancestor", commit, "HEAD"],
        cwd=ROOT,
        check=False,
        capture_output=True,
    )
    if present.returncode != 0:
        raise AssertionError("Python worker core revision is not present")


def verify_framework_dependencies(profile: dict[str, object]) -> None:
    frameworks = profile.get("frameworks")
    if not isinstance(frameworks, list) or len(frameworks) != 2:
        raise AssertionError("invalid framework dependency inventory")
    revisions = [framework_revision(framework) for framework in frameworks]
    mismatches = [
        package
        for package, expected in revisions
        if importlib.metadata.version(package) != expected
    ]
    if mismatches:
        raise AssertionError(f"{mismatches[0]} version mismatch")


def framework_revision(value: object) -> tuple[str, str]:
    if not isinstance(value, dict):
        raise TypeError("invalid framework dependency")
    package, version = value.get("package"), value.get("version")
    if not isinstance(package, str) or not isinstance(version, str):
        raise TypeError("invalid framework revision")
    return package, version


def verify_evidence(profile: dict[str, object]) -> None:
    evidence = profile.get("evidence")
    if not isinstance(evidence, list) or not evidence:
        raise TypeError("missing Python worker adapter evidence")
    if not all(evidence_matches(entry) for entry in evidence):
        raise AssertionError("Python worker adapter evidence mismatch")


def evidence_matches(value: object) -> bool:
    if not isinstance(value, dict):
        return False
    path, expected = value.get("path"), value.get("sha256")
    return isinstance(path, str) and isinstance(expected, str) and digest(path) == expected


def invocation(name: str) -> dict[str, object]:
    return {
        "protocol": PROTOCOL,
        "requestId": f"request-{name}",
        "invocationId": f"invocation-{name}",
        "attemptId": f"invocation-{name}.1",
        "handlerId": "fixture.greet",
        "schemaRevision": "schema-1",
        "deadlineUnixMilli": 4_102_444_800_000,
        "delegatedContext": "verified-delegation",
        "input": {"name": name},
    }


def http_scope() -> dict[str, object]:
    return {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": "POST",
        "scheme": "https",
        "path": "/naatre/worker",
        "raw_path": b"/naatre/worker",
        "query_string": b"",
        "headers": [],
        "client": ("127.0.0.1", 1),
        "server": ("worker.example", 443),
    }


class ASGIHarness:
    def __init__(self, messages: list[dict[str, object]]) -> None:
        self.inbox = asyncio.Queue[dict[str, object]]()
        for message in messages:
            self.inbox.put_nowait(message)
        self.sent: list[dict[str, object]] = []

    async def receive(self) -> dict[str, object]:
        return await self.inbox.get()

    async def send(self, message: dict[str, object]) -> None:
        self.sent.append(message)


async def call_asgi(
    application: FrameworkWorkerASGI,
    scope: dict[str, object],
    messages: list[dict[str, object]],
) -> list[dict[str, object]]:
    harness = ASGIHarness(messages)
    await application(scope, harness.receive, harness.send)
    return harness.sent


async def verify_framework(factory: Factory, application_module: str) -> None:
    resource = Resource()
    transactions = Transactions()
    seen: list[tuple[str, bool]] = []

    async def handler(request: HandlerRequest[Input]) -> Output:
        isolated = not request.state.loaders
        request.state.loaders["request"] = request.request_id
        seen.append((current_request().request_id, isolated))
        await asyncio.sleep(0)
        return Output(f"Hello, {request.input.name}")

    registry = Registry(schema_revision="schema-1")
    registry.bind_async("fixture.greet", INPUT, OUTPUT, handler, transactional=True)
    worker = Worker(
        registry,
        verify_identity=verify_identity,
        resources=(resource,),
        capabilities=(CAPABILITY_UNARY, CAPABILITY_TRANSACTIONS),
        transaction_provider=transactions,
    )
    application = factory(worker)
    if application.application.__class__.__module__ != application_module:
        raise AssertionError("framework application type mismatch")
    await worker.start()
    requests = [
        canonical_json({"protocol": PROTOCOL, "kind": "invoke", "payload": invocation(name)})
        for name in ("first", "second")
    ]
    responses = await asyncio.gather(
        *(
            call_asgi(
                application,
                http_scope(),
                [{"type": "http.request", "body": request, "more_body": False}],
            )
            for request in requests
        )
    )
    for response in responses:
        result = strict_json_loads(response[-1]["body"])  # type: ignore[arg-type]
        if not isinstance(result, dict) or result.get("kind") != "result":
            raise AssertionError("framework worker response mismatch")
    await worker.aclose()
    if len({request_id for request_id, _isolated in seen}) != 2 or not all(
        isolated for _request_id, isolated in seen
    ):
        raise AssertionError("framework request context leaked")
    if transactions.entered != 2 or transactions.exited != 2:
        raise AssertionError("framework transaction ownership mismatch")
    if resource.events != ["started", "drained", "closed"]:
        raise AssertionError("framework resource lifecycle mismatch")

    await verify_lifespan_failure(factory, handler)


async def verify_lifespan_failure(
    factory: Factory,
    handler: Callable[[HandlerRequest[Input]], Awaitable[Output]],
) -> None:
    failing = Resource(fail_start=True)
    failing_registry = Registry(schema_revision="schema-1")
    failing_registry.bind_async("fixture.greet", INPUT, OUTPUT, handler)
    failing_application = factory(
        Worker(failing_registry, verify_identity=verify_identity, resources=(failing,))
    )
    failure = await call_asgi(
        failing_application,
        {"type": "lifespan"},
        [{"type": "lifespan.startup"}],
    )
    if failure != [
        {"type": "lifespan.startup.failed", "message": "WORKER_STARTUP_FAILED"}
    ] or "protected" in repr(failure):
        raise AssertionError("framework lifespan failure exposed protected detail")


async def verify_behavior() -> None:
    await verify_framework(starlette_worker_app, "starlette.applications")
    await verify_framework(fastapi_worker_app, "fastapi.applications")


def main() -> None:
    profile = json.loads(PROFILE_PATH.read_text(encoding="utf-8"))
    verify_profile(profile)
    asyncio.run(verify_behavior())
    runtime_version = ".".join(str(part) for part in sys.version_info[:3])
    print(
        json.dumps(
            {
                "profile": profile["profile"],
                "status": "passed",
                "frameworks": ["asgi", "fastapi", "starlette"],
                "runtime": f"cpython-{runtime_version}",
                "nativeRuntime": False,
                "productionTransportCertified": False,
            },
            separators=(",", ":"),
        )
    )


if __name__ == "__main__":
    main()
