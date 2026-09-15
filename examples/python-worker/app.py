from __future__ import annotations

import asyncio
from collections.abc import Mapping

from naatre.generated.operations import GetAccountVariables
from naatre.generated.server import (
    GetAccountServerProfile,
    GetAccountServerResult,
    bind_get_account_async,
)
from naatre.json import canonical_json
from naatre.worker import (
    HandlerRequest,
    RegistrationConfig,
    Registry,
    VerifiedIdentity,
    Worker,
)
from naatre.worker_asgi import WorkerASGI


async def verify_delegation(token: str) -> VerifiedIdentity:
    """Replace this reference check with the application's trust verifier."""

    if token != "example-valid-delegation":
        raise ValueError("REMOTE_UNAUTHENTICATED")
    return VerifiedIdentity(
        subject="example-user",
        tenant="example-tenant",
        trace={"traceparent": "00-example-parent-01"},
    )


async def get_account(
    request: HandlerRequest[GetAccountVariables],
) -> GetAccountServerResult:
    await asyncio.sleep(0)
    return GetAccountServerResult(
        profile=GetAccountServerProfile(
            display=f"Account {request.input.id} for {request.identity.tenant}",
            nickname=request.input.nickname,
        )
    )


def build_worker() -> Worker:
    registry = Registry(schema_revision="schema-generator-r1")
    bind_get_account_async(registry, "example.get-account", get_account)
    return Worker(
        registry,
        verify_identity=verify_delegation,
        registration=RegistrationConfig(
            worker_id="python-example-worker",
            service_identity="spiffe://example/python-worker",
            audience="naatre-gateway",
            schema_digest="a" * 64,
            session_id="python-example-session",
        ),
        maximum_in_flight=1,
    )


worker = build_worker()
app = WorkerASGI(worker)


async def framework_neutral_call() -> Mapping[str, object]:
    """Models the Go gateway -> Python worker envelope without a network."""

    return await worker.handle_envelope(
        {
            "protocol": "naatre.remote-worker.v1",
            "kind": "invoke",
            "payload": {
                "protocol": "naatre.remote-worker.v1",
                "requestId": "example-request",
                "invocationId": "example-invocation",
                "attemptId": "example-invocation.1",
                "handlerId": "example.get-account",
                "schemaRevision": "schema-generator-r1",
                "deadlineUnixMilli": 4_102_444_800_000,
                "delegatedContext": "example-valid-delegation",
                "input": {"id": "acct-1"},
            },
        }
    )


async def main() -> None:
    try:
        result = await framework_neutral_call()
        print(canonical_json(result).decode("utf-8"))
    finally:
        await worker.aclose()


if __name__ == "__main__":
    asyncio.run(main())
