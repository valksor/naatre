from __future__ import annotations

import json
import struct
import sys
from pathlib import Path
from typing import Any, BinaryIO

PROTOCOL = "naatre.remote-worker.v1"
FIXTURE = json.loads((Path(__file__).parents[3] / "conformance/v1/remote-workers.json").read_text())


def handle(envelope: dict[str, Any]) -> dict[str, Any]:
    assert envelope["protocol"] == PROTOCOL
    payload = envelope["payload"]
    if envelope["kind"] == "register":
        return {"protocol": PROTOCOL, "kind": "registered", "payload": {"protocol": PROTOCOL, "workerId": payload["workerId"], "sessionId": "example-session", "schemaRevision": payload["schemaRevision"], "acceptedCapabilities": payload["capabilities"]}}
    if envelope["kind"] == "cancel":
        return {"protocol": PROTOCOL, "kind": "cancelled", "payload": {"protocol": PROTOCOL, "invocationId": payload["invocationId"], "disposition": "acknowledged"}}
    assert envelope["kind"] == "invoke"
    rejected = payload["input"]["name"] == "reject"
    return {"protocol": PROTOCOL, "kind": "result", "payload": {"protocol": PROTOCOL, "invocationId": payload["invocationId"], "attemptId": payload["attemptId"], "schemaRevision": payload["schemaRevision"], "data": None if rejected else {"greeting": f"Hello, {payload['input']['name']}"}, "errors": [{"code": "NAME_REJECTED", "message": "name was rejected", "retryable": False}] if rejected else []}}


def serve(reader: BinaryIO, writer: BinaryIO) -> None:
    while header := reader.read(5):
        if len(header) != 5:
            raise ValueError("truncated frame header")
        flags, size = struct.unpack(">BI", header)
        if flags != 0 or size == 0 or size > FIXTURE["transport"]["frame"]["maximumBytes"]:
            raise ValueError("invalid frame")
        payload = reader.read(size)
        if len(payload) != size:
            raise ValueError("truncated frame")
        encoded = json.dumps(handle(json.loads(payload)), separators=(",", ":")).encode()
        writer.write(struct.pack(">BI", 0, len(encoded)) + encoded)
        writer.flush()


def self_test() -> None:
    assert FIXTURE["schema"]["sharedSchemaProfile"] == "core.schema-1"
    for operation in FIXTURE["operations"]:
        response = handle({"protocol": PROTOCOL, "kind": "invoke", "payload": {"invocationId": operation["name"], "attemptId": "attempt-1", "schemaRevision": FIXTURE["schema"]["revision"], "handlerId": operation["handlerId"], "input": operation["input"]}})
        assert {"data": response["payload"]["data"], "errors": response["payload"]["errors"]} == operation["expected"]


if __name__ == "__main__":
    serve(sys.stdin.buffer, sys.stdout.buffer) if "--serve" in sys.argv else self_test()
