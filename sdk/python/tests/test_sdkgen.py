from __future__ import annotations

import json
from pathlib import Path

import pytest

from naatre.sdkgen import PythonGeneratorError, generate

ROOT = Path(__file__).parents[3]


def test_generation_is_byte_identical_to_checked_artifacts() -> None:
    artifacts = generate(
        (ROOT / "conformance/v1/generator-model.json").read_bytes(),
        (ROOT / "conformance/v1/generator-output.json").read_bytes(),
    )
    generated = ROOT / "sdk/python/src/naatre/generated"
    assert artifacts.source == (generated / "operations.py").read_bytes()
    assert artifacts.manifest == (generated / "operations.json").read_bytes()


def test_generator_renders_open_union_wrapper() -> None:
    model_path = ROOT / "conformance/v1/generator-model.json"
    model = json.loads(model_path.read_text())
    model["schema"]["types"].append(
        {
            "id": "Subject",
            "name": "Subject",
            "kind": "union",
            "open": True,
            "variantMembers": [{"id": "Subject.Account", "type": "Account"}],
        }
    )
    artifacts = generate(
        json.dumps(model, separators=(",", ":")).encode(),
        (ROOT / "conformance/v1/generator-output.json").read_bytes(),
    )
    assert b"Subject: TypeAlias = Account | OpenVariant" in artifacts.source
    assert b"GetAccountError: TypeAlias = Error" in artifacts.source


def test_generator_rejects_reference_digest_drift() -> None:
    reference_path = ROOT / "conformance/v1/generator-output.json"
    reference = json.loads(reference_path.read_text())
    reference["operations"][0]["persisted"]["digest"] = "0" * 64
    with pytest.raises(PythonGeneratorError) as raised:
        generate(
            (ROOT / "conformance/v1/generator-model.json").read_bytes(),
            json.dumps(reference, separators=(",", ":")).encode(),
        )
    assert raised.value.code == "PYTHON_SDK_GENERATOR_REFERENCE_DRIFT"


def test_generator_emits_valid_empty_models() -> None:
    model = json.loads((ROOT / "conformance/v1/generator-model.json").read_text())
    reference = json.loads((ROOT / "conformance/v1/generator-output.json").read_text())
    model["operations"][0]["variables"] = []
    model["operations"][0]["result"]["fields"] = []
    reference["operations"][0]["variables"] = []
    reference["operations"][0]["result"]["data"]["fields"] = []

    artifacts = generate(
        json.dumps(model, separators=(",", ":")).encode(),
        json.dumps(reference, separators=(",", ":")).encode(),
    )

    compile(artifacts.source, "<generated>", "exec")
    assert artifacts.source.count(b"    pass") >= 3
