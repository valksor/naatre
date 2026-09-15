from __future__ import annotations

from pathlib import Path

from naatre.generated.operations import GetAccountVariables
from naatre.generated.server import (
    GET_ACCOUNT_INPUT_CODEC,
    GET_ACCOUNT_OUTPUT_CODEC,
    GetAccountServerProfile,
    GetAccountServerResult,
    bind_get_account_async,
    bind_get_account_sync,
)
from naatre.servergen import generate_server
from naatre.worker import HandlerRequest, Registry

ROOT = Path(__file__).parents[3]


def test_server_generation_is_byte_identical() -> None:
    expected = ROOT / "sdk/python/src/naatre/generated/server.py"
    assert generate_server((ROOT / "conformance/v1/generator-model.json").read_bytes()) == (
        expected.read_bytes()
    )


def test_generated_interfaces_bind_typed_sync_and_async_handlers() -> None:
    registry = Registry(schema_revision="schema-1")

    def sync_handler(request: HandlerRequest[GetAccountVariables]) -> GetAccountServerResult:
        return GetAccountServerResult(
            profile=GetAccountServerProfile(display=request.input.id, nickname=None)
        )

    async def async_handler(request: HandlerRequest[GetAccountVariables]) -> GetAccountServerResult:
        return sync_handler(request)

    bind_get_account_sync(registry, "account.sync", sync_handler)
    bind_get_account_async(registry, "account.async", async_handler)
    decoded = GET_ACCOUNT_INPUT_CODEC.decode({"id": "acct-1"})
    assert decoded == GetAccountVariables(id="acct-1")
    result = GetAccountServerResult(
        profile=GetAccountServerProfile(display="Ada", nickname=None)
    )
    assert GET_ACCOUNT_OUTPUT_CODEC.encode(result) == {
        "profile": {"display": "Ada", "nickname": None}
    }
