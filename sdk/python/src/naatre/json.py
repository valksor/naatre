from __future__ import annotations

import json
import math
from collections.abc import Mapping, Sequence
from typing import TypeAlias, cast

from .errors import NaatreClientError

JSONScalar: TypeAlias = None | bool | int | float | str
JSONValue: TypeAlias = JSONScalar | list["JSONValue"] | dict[str, "JSONValue"]
_MAX_SAFE_INTEGER = 9_007_199_254_740_991


def canonical_json(value: object) -> bytes:
    normalized = _normalize(value, set())
    try:
        encoded = json.dumps(
            normalized,
            ensure_ascii=False,
            allow_nan=False,
            separators=(",", ":"),
        )
    except (TypeError, ValueError) as error:
        raise NaatreClientError("CLIENT_VALUE_INVALID") from error
    return encoded.encode("utf-8")


def strict_json_loads(value: str | bytes, *, maximum_bytes: int = 16 << 20) -> JSONValue:
    raw = value.encode("utf-8") if isinstance(value, str) else value
    if len(raw) > maximum_bytes:
        raise NaatreClientError("CLIENT_RESPONSE_LIMIT")
    try:
        text = raw.decode("utf-8", errors="strict")
        decoded = json.loads(
            text,
            parse_constant=_reject_constant,
            object_pairs_hook=_unique_object,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        if isinstance(error, NaatreClientError):
            raise
        raise NaatreClientError("CLIENT_PROTOCOL_INVALID") from error
    return cast(JSONValue, decoded)


def _reject_constant(value: str) -> None:
    raise NaatreClientError("CLIENT_PROTOCOL_INVALID", f"nonstandard JSON constant: {value}")


def _unique_object(pairs: list[tuple[str, JSONValue]]) -> dict[str, JSONValue]:
    result: dict[str, JSONValue] = {}
    for key, value in pairs:
        if key in result:
            raise NaatreClientError("CLIENT_PROTOCOL_INVALID", "duplicate JSON member")
        result[key] = value
    return result


def _utf16_key(value: str) -> bytes:
    try:
        return value.encode("utf-16-be", errors="strict")
    except UnicodeEncodeError as error:
        raise NaatreClientError("CLIENT_VALUE_INVALID") from error


def _normalize(value: object, ancestors: set[int]) -> JSONValue:
    if value is None or isinstance(value, (bool, str)) or type(value) in {int, float}:
        return _normalize_scalar(value)
    return _normalize_collection(value, ancestors)


def _normalize_scalar(value: object) -> JSONScalar:
    if value is None or isinstance(value, bool):
        return cast(JSONScalar, value)
    if isinstance(value, str):
        _utf16_key(value)
        return value
    if type(value) is int:
        if abs(value) > _MAX_SAFE_INTEGER:
            raise NaatreClientError("CLIENT_VALUE_PRECISION")
        return value
    if type(value) is float:
        if not math.isfinite(value):
            raise NaatreClientError("CLIENT_VALUE_INVALID")
        return 0.0 if value == 0 else value
    raise AssertionError("scalar dispatch mismatch")


def _normalize_collection(value: object, ancestors: set[int]) -> JSONValue:
    identity = id(value)
    if identity in ancestors:
        raise NaatreClientError("CLIENT_VALUE_INVALID")
    ancestors.add(identity)
    try:
        if isinstance(value, Mapping):
            return _normalize_mapping(value, ancestors)
        if isinstance(value, Sequence) and not isinstance(value, (str, bytes, bytearray)):
            return [_normalize(entry, ancestors) for entry in value]
        raise NaatreClientError("CLIENT_VALUE_INVALID")
    finally:
        ancestors.remove(identity)


def _normalize_mapping(value: Mapping[object, object], ancestors: set[int]) -> JSONValue:
    keys = list(value.keys())
    if any(not isinstance(key, str) for key in keys):
        raise NaatreClientError("CLIENT_VALUE_INVALID")
    return {
        key: _normalize(value[key], ancestors)
        for key in sorted(cast(list[str], keys), key=_utf16_key)
    }
