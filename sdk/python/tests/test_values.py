from __future__ import annotations

from decimal import Decimal

import pytest

from naatre import (
    NaatreClientError,
    Timestamp,
    canonical_json,
    decode_bytes,
    decode_decimal,
    decode_integer,
    decode_open_enum,
    decode_open_variant,
    encode_bytes,
    encode_decimal,
    encode_integer,
    encode_timestamp,
    strict_json_loads,
)
from naatre.values import NaatreValueError, OpenEnum, OpenVariant


def test_bool_is_not_accepted_as_an_integer() -> None:
    with pytest.raises(NaatreValueError, match="CLIENT_SCALAR_INVALID"):
        encode_integer(True)


def test_arbitrary_integer_round_trips_without_float() -> None:
    value = 10**100 + 7
    assert decode_integer(encode_integer(value)) == value


@pytest.mark.parametrize("spelling", ["1.2300", "1E+900", "-0.000", "7.8900E-120"])
def test_decimal_exponent_and_scale_round_trip(spelling: str) -> None:
    value = Decimal(spelling)
    decoded = decode_decimal(encode_decimal(value))
    assert decoded.as_tuple() == value.as_tuple()


def test_decimal_rejects_non_finite_values() -> None:
    with pytest.raises(NaatreValueError, match="CLIENT_SCALAR_INVALID"):
        encode_decimal(Decimal("NaN"))


def test_sub_microsecond_timestamp_is_retained_exactly() -> None:
    value = Timestamp("2026-09-14T12:34:56.123456789Z")
    assert encode_timestamp(value) == "2026-09-14T12:34:56.123456789Z"


def test_bytes_round_trip_without_padding() -> None:
    encoded = encode_bytes(b"\x00Naatre\xff")
    assert "=" not in encoded
    assert decode_bytes(encoded) == b"\x00Naatre\xff"


def test_unknown_open_enum_and_union_values_are_preserved() -> None:
    enum_value = decode_open_enum("FUTURE", {"ACTIVE"})
    union_value = decode_open_variant({"$type": "Future", "$value": {"raw": 7}}, {})

    assert enum_value == OpenEnum("FUTURE")
    assert union_value == OpenVariant("Future", {"raw": 7})


@pytest.mark.parametrize("constant", ["NaN", "Infinity", "-Infinity"])
def test_nonstandard_json_constants_are_rejected(constant: str) -> None:
    with pytest.raises(NaatreClientError, match="nonstandard JSON constant"):
        strict_json_loads(f'{{"value":{constant}}}')


def test_canonical_json_disables_non_finite_output() -> None:
    with pytest.raises(NaatreClientError) as raised:
        canonical_json({"value": float("inf")})
    assert raised.value.code == "CLIENT_VALUE_INVALID"


def test_canonical_json_uses_utf16_key_order() -> None:
    encoded = canonical_json({"\ue000": 1, "\U00010000": 2})
    assert encoded == '{"𐀀":2,"":1}'.encode()
