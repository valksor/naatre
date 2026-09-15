from __future__ import annotations

import asyncio

from app import build_worker
from naatre.json import canonical_json
from naatre.worker_asgi import WorkerASGI


async def main() -> None:
    application = WorkerASGI(build_worker())
    incoming = asyncio.Queue[dict[str, object]]()
    incoming.put_nowait(
        {
            "type": "http.request",
            "body": canonical_json(
                {
                    "protocol": "naatre.remote-worker.v1",
                    "kind": "invoke",
                    "payload": {
                        "protocol": "naatre.remote-worker.v1",
                        "requestId": "asgi-request",
                        "invocationId": "asgi-invocation",
                        "attemptId": "asgi-invocation.1",
                        "handlerId": "example.get-account",
                        "schemaRevision": "schema-generator-r1",
                        "deadlineUnixMilli": 4_102_444_800_000,
                        "delegatedContext": "example-valid-delegation",
                        "input": {"id": "acct-asgi"},
                    },
                }
            ),
            "more_body": False,
        }
    )
    sent: list[dict[str, object]] = []

    async def receive() -> dict[str, object]:
        return await incoming.get()

    async def send(message: dict[str, object]) -> None:
        sent.append(message)

    await application(
        {"type": "http", "method": "POST", "path": "/naatre/worker"},
        receive,
        send,
    )
    body = sent[-1]["body"]
    if not isinstance(body, bytes):
        raise TypeError("ASGI worker returned a non-byte body")
    print(body.decode("utf-8"))
    await application.worker.aclose()


if __name__ == "__main__":
    asyncio.run(main())
