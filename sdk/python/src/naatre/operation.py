from __future__ import annotations

from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Generic, Literal, TypeVar, cast

from .errors import Error, NaatreClientError
from .json import JSONValue, canonical_json, strict_json_loads
from .values import MISSING, MissingType, Selected

VariablesT = TypeVar("VariablesT")
ResultT = TypeVar("ResultT")
DecodedT = TypeVar("DecodedT")
OperationKind = Literal["query", "mutation", "subscription"]


@dataclass(frozen=True, slots=True)
class PersistedReference:
    algorithm: Literal["sha-256"]
    canonical_version: Literal["c14n-1"]
    digest: str

    def wire(self) -> dict[str, JSONValue]:
        if len(self.digest) != 64 or any(
            character not in "0123456789abcdef" for character in self.digest
        ):
            raise NaatreClientError("CLIENT_OPERATION_INVALID")
        return {
            "algorithm": self.algorithm,
            "canonicalVersion": self.canonical_version,
            "digest": self.digest,
        }


@dataclass(frozen=True, slots=True)
class OperationResult(Generic[ResultT]):
    data: ResultT | None
    errors: tuple[Error, ...]
    complete: bool


@dataclass(frozen=True, slots=True)
class Operation(Generic[VariablesT, ResultT]):
    name: str
    kind: OperationKind
    persisted: PersistedReference
    variables: VariablesT
    _wire_variables: Mapping[str, JSONValue]
    _decode_data: Callable[[object], ResultT]

    def canonical_request(self) -> bytes:
        return canonical_json(
            {
                "version": "1",
                "operation": self.name,
                "persisted": self.persisted.wire(),
                "variables": dict(self._wire_variables),
            }
        )

    def decode_result(
        self, payload: str | bytes | Mapping[str, object]
    ) -> OperationResult[ResultT]:
        value: object = strict_json_loads(payload) if isinstance(payload, (str, bytes)) else payload
        if not isinstance(value, Mapping) or set(value) != {"complete", "data", "errors"}:
            raise NaatreClientError("CLIENT_PROTOCOL_INVALID")
        complete = value["complete"]
        errors = value["errors"]
        if type(complete) is not bool or not isinstance(errors, list):
            raise NaatreClientError("CLIENT_PROTOCOL_INVALID")
        decoded_errors = tuple(_decode_error(entry) for entry in errors)
        raw_data = value["data"]
        data = None if raw_data is None else self._decode_data(raw_data)
        return OperationResult(data=data, errors=decoded_errors, complete=complete)


def decode_selected(
    owner: object,
    key: str,
    *,
    pending_when_missing: bool,
    decode: Callable[[object], DecodedT],
) -> Selected[DecodedT]:
    if not isinstance(owner, Mapping):
        raise NaatreClientError("CLIENT_PROTOCOL_INVALID")
    if key not in owner:
        return Selected(state="pending" if pending_when_missing else "missing")
    value = owner[key]
    if value is None:
        return Selected(state="null")
    return Selected(state="present", value=decode(value))


def omit_missing(values: Mapping[str, JSONValue | MissingType]) -> dict[str, JSONValue]:
    return {name: cast(JSONValue, value) for name, value in values.items() if value is not MISSING}


def _decode_error(value: object) -> Error:
    if not isinstance(value, Mapping):
        raise NaatreClientError("CLIENT_PROTOCOL_INVALID")
    code = value.get("code")
    message = value.get("message", "")
    path = value.get("path", [])
    if not isinstance(code, str) or not isinstance(message, str) or not isinstance(path, list):
        raise NaatreClientError("CLIENT_PROTOCOL_INVALID")
    if any(type(entry) is not int and not isinstance(entry, str) for entry in path):
        raise NaatreClientError("CLIENT_PROTOCOL_INVALID")
    return Error(code=code, message=message, path=tuple(cast(list[str | int], path)))
