from __future__ import annotations

from collections.abc import Callable, Mapping
from typing import Protocol, Self, TypeVar, cast

from .errors import NaatreClientError
from .json import JSONValue, canonical_json, strict_json_loads


class PydanticModel(Protocol):
    @classmethod
    def model_validate(
        cls,
        value: object,
        *,
        strict: bool | None = None,
        extra: str | None = None,
    ) -> Self: ...

    def model_dump(
        self,
        *,
        mode: str,
        exclude_unset: bool,
        warnings: str,
    ) -> object: ...


ModelT = TypeVar("ModelT", bound=PydanticModel)


def model_decoder(model: type[ModelT]) -> Callable[[object], ModelT]:
    """Return a strict result decoder compatible with ``Operation``."""

    def decode(value: object) -> ModelT:
        decoded: ModelT | None = None
        try:
            decoded = model.model_validate(value, strict=True, extra="forbid")
        except Exception:
            pass
        if decoded is None:
            raise NaatreClientError("CLIENT_RESULT_INVALID")
        return decoded

    return decode


def model_variables(model: PydanticModel) -> Mapping[str, JSONValue]:
    """Produce validated JSON variables while preserving unset-field omission."""

    value: object | None = None
    try:
        value = model.model_dump(mode="json", exclude_unset=True, warnings="error")
        value = strict_json_loads(canonical_json(value))
    except Exception:
        pass
    if not isinstance(value, dict):
        raise NaatreClientError("CLIENT_VARIABLES_INVALID")
    return cast(Mapping[str, JSONValue], value)
