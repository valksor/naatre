from __future__ import annotations

import base64
import re
from collections.abc import Callable, Collection, Mapping
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from typing import Final, Generic, Literal, TypeAlias, TypeVar

T = TypeVar("T")


class MissingType:
    __slots__ = ()

    def __repr__(self) -> str:
        return "MISSING"


MISSING: Final = MissingType()


@dataclass(frozen=True, slots=True)
class OpenEnum:
    raw: str


@dataclass(frozen=True, slots=True)
class OpenVariant:
    discriminator: str
    value: object


Presence: TypeAlias = Literal["missing", "null", "pending", "present", "failed", "skipped"]


@dataclass(frozen=True, slots=True)
class Selected(Generic[T]):
    state: Presence
    value: T | None = None
    errors: tuple[object, ...] = ()
    reason: str = ""


_INTEGER = re.compile(r"-?(?:0|[1-9][0-9]*)\Z")
_TIMESTAMP = re.compile(
    r"(?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2})T"
    r"(?P<time>[0-9]{2}:[0-9]{2}:[0-9]{2})"
    r"(?P<fraction>\.[0-9]+)?(?P<zone>Z|[+-][0-9]{2}:[0-9]{2})\Z"
)


@dataclass(frozen=True, slots=True)
class Timestamp:
    """An RFC 3339 spelling retained without datetime precision loss."""

    raw: str

    def __post_init__(self) -> None:
        match = _TIMESTAMP.fullmatch(self.raw)
        if match is None:
            raise NaatreValueError("CLIENT_SCALAR_INVALID")
        date = match.group("date").split("-")
        clock = match.group("time").split(":")
        year, month, day = (int(part) for part in date)
        hour, minute, second = (int(part) for part in clock)
        month_days = (31, 29 if _is_leap_year(year) else 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31)
        if (
            month < 1
            or month > 12
            or day < 1
            or day > month_days[month - 1]
            or hour > 23
            or minute > 59
            or second > 59
        ):
            raise NaatreValueError("CLIENT_SCALAR_INVALID")
        zone = match.group("zone")
        if zone != "Z":
            zone_hour, zone_minute = (int(part) for part in zone[1:].split(":"))
            if zone_hour > 23 or zone_minute > 59:
                raise NaatreValueError("CLIENT_SCALAR_INVALID")

    def __str__(self) -> str:
        return self.raw


class NaatreValueError(ValueError):
    def __init__(self, code: str) -> None:
        super().__init__(code)
        self.code = code


def _is_leap_year(year: int) -> bool:
    return year % 4 == 0 and (year % 100 != 0 or year % 400 == 0)


def decode_open_enum(value: object, known: Collection[str]) -> str | OpenEnum:
    if not isinstance(value, str):
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    return value if value in known else OpenEnum(value)


def decode_open_variant(
    value: object,
    decoders: Mapping[str, Callable[[object], object]],
) -> object | OpenVariant:
    if not isinstance(value, Mapping) or set(value) != {"$type", "$value"}:
        raise NaatreValueError("CLIENT_PROTOCOL_INVALID")
    discriminator = value["$type"]
    if not isinstance(discriminator, str):
        raise NaatreValueError("CLIENT_PROTOCOL_INVALID")
    decoder = decoders.get(discriminator)
    raw_value = value["$value"]
    if decoder is None:
        return OpenVariant(discriminator=discriminator, value=raw_value)
    return decoder(raw_value)


def encode_integer(value: int) -> str:
    if type(value) is not int:
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    return str(value)


def decode_integer(value: object) -> int:
    if not isinstance(value, str) or _INTEGER.fullmatch(value) is None:
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    return int(value)


def encode_decimal(value: Decimal) -> str:
    if type(value) is not Decimal or not value.is_finite():
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    return str(value)


def decode_decimal(value: object) -> Decimal:
    if not isinstance(value, str):
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    try:
        decoded = Decimal(value)
    except InvalidOperation as error:
        raise NaatreValueError("CLIENT_SCALAR_INVALID") from error
    if not decoded.is_finite():
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    return decoded


def encode_timestamp(value: Timestamp) -> str:
    if type(value) is not Timestamp:
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    return value.raw


def decode_timestamp(value: object) -> Timestamp:
    if not isinstance(value, str):
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    return Timestamp(value)


def encode_bytes(value: bytes) -> str:
    if type(value) is not bytes:
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    return base64.urlsafe_b64encode(value).rstrip(b"=").decode("ascii")


def decode_bytes(value: object) -> bytes:
    if not isinstance(value, str) or "=" in value:
        raise NaatreValueError("CLIENT_SCALAR_INVALID")
    try:
        return base64.b64decode(value + "=" * (-len(value) % 4), altchars=b"-_", validate=True)
    except (ValueError, UnicodeEncodeError) as error:
        raise NaatreValueError("CLIENT_SCALAR_INVALID") from error
