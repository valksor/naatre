from __future__ import annotations

import asyncio
import hashlib
import json
import sys
from collections.abc import AsyncIterator
from decimal import Decimal
from pathlib import Path
from typing import Self

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "sdk/python/src"))

from naatre import (  # noqa: E402
    AsyncClient,
    NaatreClientError,
    SyncClient,
    Timestamp,
    canonical_json,
    decode_decimal,
    decode_integer,
    encode_decimal,
    encode_integer,
    strict_json_loads,
)
from naatre.generated.operations import (  # noqa: E402
    GetAccountVariables,
    create_get_account,
)
from naatre.sdkgen import GENERATOR_VERSION, generate  # noqa: E402

PROFILE_PATH = ROOT / "conformance/v1/python-sdk.json"


class SyncFixtureTransport:
    def __init__(self, response: bytes) -> None:
        self.response = response
        self.requests: list[bytes] = []

    def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        self.requests.append(request)
        return self.response


class AsyncFixtureTransport:
    def __init__(self, response: bytes) -> None:
        self.response = response
        self.requests: list[bytes] = []
        self.released = asyncio.Event()

    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        self.requests.append(request)
        try:
            return self.response
        finally:
            self.released.set()


class CancellableTransport:
    def __init__(self) -> None:
        self.released = asyncio.Event()

    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        try:
            await asyncio.Event().wait()
        finally:
            self.released.set()
        raise AssertionError("cancelled transport resumed")


class ClosableSource(AsyncIterator[bytes]):
    def __init__(self) -> None:
        self.sent = False
        self.closed = False

    def __aiter__(self) -> Self:
        return self

    async def __anext__(self) -> bytes:
        if self.sent:
            await asyncio.Event().wait()
            raise StopAsyncIteration
        self.sent = True
        return b'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n'

    async def aclose(self) -> None:
        self.closed = True


class FixtureStreamTransport:
    def __init__(self, source: ClosableSource) -> None:
        self.source = source

    async def open(self, request: bytes, *, timeout: float | None = None) -> AsyncIterator[bytes]:
        return self.source


def verify_evidence(profile: dict[str, object]) -> None:
    evidence = profile.get("evidence")
    if not isinstance(evidence, list):
        raise TypeError("missing Python SDK evidence")
    for entry in evidence:
        if not isinstance(entry, dict):
            raise TypeError("invalid Python SDK evidence")
        path = entry.get("path")
        expected = entry.get("sha256")
        if not isinstance(path, str) or not isinstance(expected, str):
            raise TypeError("invalid Python SDK evidence")
        actual = hashlib.sha256((ROOT / path).read_bytes()).hexdigest()
        if actual != expected:
            raise AssertionError(f"Python SDK evidence mismatch: {path}")


def verify_generation(profile: dict[str, object]) -> None:
    sources = profile["sources"]
    generated = profile["generated"]
    if not isinstance(sources, dict) or not isinstance(generated, dict):
        raise TypeError("invalid Python SDK generation fixture")
    model = sources["model"]
    reference = sources["referenceOutput"]
    source = generated["source"]
    manifest = generated["manifest"]
    if not all(isinstance(value, dict) for value in (model, reference, source, manifest)):
        raise AssertionError("invalid Python SDK generation fixture")
    model_path = model.get("path")
    reference_path = reference.get("path")
    source_path = source.get("path")
    manifest_path = manifest.get("path")
    generation_paths = (model_path, reference_path, source_path, manifest_path)
    if not all(isinstance(value, str) for value in generation_paths):
        raise AssertionError("invalid Python SDK generation path")
    artifacts = generate(
        (ROOT / model_path).read_bytes(),
        (ROOT / reference_path).read_bytes(),
    )
    if artifacts.source != (ROOT / source_path).read_bytes():
        raise AssertionError("Python source regeneration drift")
    if artifacts.manifest != (ROOT / manifest_path).read_bytes():
        raise AssertionError("Python manifest regeneration drift")


async def verify_async(response: bytes, expected_request: bytes) -> object:
    operation = create_get_account(GetAccountVariables(id="acct-1", nickname=None))
    transport = AsyncFixtureTransport(response)
    result = await AsyncClient(transport).execute(operation)
    if transport.requests != [expected_request] or not transport.released.is_set():
        raise AssertionError("async client request or lifecycle mismatch")
    if result.data is None or result.data.profile.value is None:
        raise AssertionError("async client result mismatch")

    source = ClosableSource()
    client = AsyncClient(transport, stream_transport=FixtureStreamTransport(source))
    stream = client.stream(operation)
    frame = await anext(stream)
    if frame.type != "open":
        raise AssertionError("SSE frame mismatch")
    await stream.aclose()
    if not source.closed:
        raise AssertionError("abandoned stream source was not closed")

    cancellable = CancellableTransport()
    task = asyncio.create_task(AsyncClient(cancellable).execute(operation))
    await asyncio.sleep(0)
    task.cancel()
    try:
        await task
    except asyncio.CancelledError:
        pass
    else:
        raise AssertionError("asyncio cancellation was translated or ignored")
    if not cancellable.released.is_set():
        raise AssertionError("cancelled transport work was not released")
    return result


def verify_behavior() -> None:
    response = (
        b'{"complete":false,"data":{"profile":{"display":"Ada","nickname":null}},'
        b'"errors":[{"code":"PARTIAL"}]}'
    )
    operation = create_get_account(GetAccountVariables(id="acct-1", nickname=None))
    expected = canonical_json(
        {
            "version": "1",
            "operation": "GetAccount",
            "persisted": {
                "algorithm": "sha-256",
                "canonicalVersion": "c14n-1",
                "digest": "cc863005080edcb85ec0345a50593dc58111896bbbe7ff86475ef2412fc5cb90",
            },
            "variables": {"id": "acct-1", "nickname": None},
        }
    )
    sync_transport = SyncFixtureTransport(response)
    sync_result = SyncClient(sync_transport).execute(operation)
    if sync_transport.requests != [expected] or sync_result.data is None:
        raise AssertionError("sync client request or result mismatch")
    async_result = asyncio.run(verify_async(response, expected))
    if sync_result != async_result:
        raise AssertionError("sync and async result mismatch")
    missing = create_get_account(GetAccountVariables(id="acct-1")).canonical_request()
    if b'"nickname"' in missing or b'"nickname":null' not in expected:
        raise AssertionError("missing and null were conflated")
    if decode_integer(encode_integer(10**100 + 7)) != 10**100 + 7:
        raise AssertionError("extended integer lost precision")
    decimal_value = Decimal("7.8900E-120")
    if decode_decimal(encode_decimal(decimal_value)).as_tuple() != decimal_value.as_tuple():
        raise AssertionError("decimal exponent or scale changed")
    timestamp = "2026-09-14T12:34:56.123456789Z"
    if str(Timestamp(timestamp)) != timestamp:
        raise AssertionError("timestamp precision changed")
    for constant in ("NaN", "Infinity", "-Infinity"):
        try:
            strict_json_loads(f'{{"value":{constant}}}')
        except NaatreClientError:
            continue
        raise AssertionError("nonstandard JSON constant accepted")
    try:
        canonical_json({"value": float("inf")})
    except NaatreClientError:
        pass
    else:
        raise AssertionError("non-finite JSON output accepted")
    try:
        encode_integer(True)
    except ValueError:
        pass
    else:
        raise AssertionError("bool accepted as integer")


def main() -> None:
    profile = json.loads(PROFILE_PATH.read_text(encoding="utf-8"))
    if profile.get("profile") != "sdk.python.core-1":
        raise AssertionError("invalid Python SDK profile")
    verify_evidence(profile)
    verify_generation(profile)
    verify_behavior()
    print(
        json.dumps(
            {
                "profile": profile["profile"],
                "status": "passed",
                "generatorVersion": GENERATOR_VERSION,
            },
            separators=(",", ":"),
        )
    )


if __name__ == "__main__":
    main()
