from __future__ import annotations

import json
from decimal import Decimal
from pathlib import Path

from naatre import MISSING, AsyncClient, SyncClient, canonical_json
from naatre.generated.operations import GetAccountVariables, create_get_account


class RecordingSyncTransport:
    def __init__(self, response: bytes) -> None:
        self.response = response
        self.requests: list[bytes] = []
        self.timeout: float | None = None
        self.cancelled = False

    def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        self.requests.append(request)
        self.timeout = timeout
        if timeout == 0:
            raise TimeoutError("backend timeout")
        return self.response


class RecordingAsyncTransport:
    def __init__(self, response: bytes) -> None:
        self.response = response
        self.requests: list[bytes] = []

    async def execute(self, request: bytes, *, timeout: float | None = None) -> bytes:
        self.requests.append(request)
        return self.response


def test_sync_and_async_clients_produce_identical_requests_and_results() -> None:
    import asyncio

    response = (
        b'{"complete":false,"data":{"profile":{"display":"Ada","nickname":null}},'
        b'"errors":[{"code":"PARTIAL"}]}'
    )
    operation = create_get_account(GetAccountVariables(id="acct-1", nickname=None))
    sync_transport = RecordingSyncTransport(response)
    async_transport = RecordingAsyncTransport(response)

    sync_result = SyncClient(sync_transport).execute(operation)
    async_result = asyncio.run(AsyncClient(async_transport).execute(operation))

    assert sync_transport.requests == async_transport.requests
    assert sync_result == async_result
    assert sync_result.data is not None
    assert sync_result.data.profile.state == "present"
    assert sync_result.data.profile.value is not None
    assert sync_result.data.profile.value.nickname.state == "null"
    assert sync_result.data.later.state == "pending"
    assert sync_result.errors[0].code == "PARTIAL"


def test_missing_and_null_are_distinct_in_generated_variables() -> None:
    missing = create_get_account(GetAccountVariables(id="acct-1"))
    explicit_null = create_get_account(GetAccountVariables(id="acct-1", nickname=None))

    assert missing.variables.nickname is MISSING
    assert b'"nickname"' not in missing.canonical_request()
    assert b'"nickname":null' in explicit_null.canonical_request()


def test_sync_timeout_does_not_claim_active_cancellation() -> None:
    import pytest

    transport = RecordingSyncTransport(b"")
    operation = create_get_account(GetAccountVariables(id="acct-1"))

    with pytest.raises(TimeoutError, match="backend timeout"):
        SyncClient(transport).execute(operation, timeout=0)

    assert transport.timeout == 0
    assert transport.cancelled is False


def test_generated_custom_scalar_is_decimal() -> None:
    from naatre.generated.operations import Money

    value: Money = Decimal("1.2300")
    assert value.as_tuple().exponent == -4


def test_generated_request_matches_shared_language_neutral_vector() -> None:
    root = Path(__file__).parents[3]
    reference = json.loads((root / "conformance/v1/generator-output.json").read_text())
    variables = GetAccountVariables(id="acct-1", nickname=None, tags=(), filter={})

    actual = create_get_account(variables).canonical_request()

    assert actual == canonical_json(reference["operations"][0]["request"])
