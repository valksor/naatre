# mypy: ignore-errors
from __future__ import annotations

import pytest

pydantic = pytest.importorskip("pydantic")

from naatre import NaatreClientError  # noqa: E402
from naatre.pydantic import model_decoder, model_variables  # noqa: E402


def test_pydantic_adapter_is_strict_and_omits_unset_fields() -> None:
    class Variables(pydantic.BaseModel):
        model_config = pydantic.ConfigDict(strict=True, extra="forbid", frozen=True)

        identifier: int
        nickname: str | None = None

    variables = Variables(identifier=7)

    assert model_variables(variables) == {"identifier": 7}
    with pytest.raises(pydantic.ValidationError):
        Variables(identifier="7")  # type: ignore[arg-type]


def test_pydantic_decoder_uses_stable_redacted_failure() -> None:
    class Result(pydantic.BaseModel):
        model_config = pydantic.ConfigDict(strict=True, extra="forbid", frozen=True)

        count: int

    decode = model_decoder(Result)

    with pytest.raises(NaatreClientError) as failure:
        decode({"count": "credential-secret"})

    assert failure.value.code == "CLIENT_RESULT_INVALID"
    assert "credential-secret" not in str(failure.value)
    assert failure.value.__context__ is None


def test_pydantic_decoder_rejects_extra_result_fields() -> None:
    class Result(pydantic.BaseModel):
        count: int

    with pytest.raises(NaatreClientError) as failure:
        model_decoder(Result)({"count": 1, "implementationOnly": "hidden"})

    assert failure.value.code == "CLIENT_RESULT_INVALID"
