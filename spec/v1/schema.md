# Type system and value model

## Type forms

- **TYPE-001:** Input and output positions are distinct. Output-only object,
  interface, and stream types MUST NOT be accepted as input literals.
- **TYPE-002:** Core scalars are `Boolean`, `String`, `ID`, `Int32`, and
  `Float64`. Extended scalars are `Int64`, `UInt64`, `BigInt`, `Decimal`,
  `Timestamp`, `Duration`, `UUID`, and `Bytes`.
- **TYPE-003:** Composite forms are list, string-keyed map, object, enum, open
  tagged union, interface, and one-of input object. Every named type has a
  stable portable ASCII identifier.
- **TYPE-004:** Required/optional describes presence. Nullable/non-null
  describes the value when present. They MUST NOT be conflated.
- **TYPE-005:** Recursive type graphs are permitted only with configured schema
  and value depth limits.

Valid: an optional non-null field is missing, or a required nullable field is
present as null.

Invalid: a required field is missing, or a non-null field is explicitly null.

## Scalar wire and canonical forms

| Clause | Type | JSON form | Canonical rule |
| --- | --- | --- | --- |
| TYPE-100 | Boolean | boolean | `true` or `false` |
| TYPE-101 | String | string | unchanged Unicode scalars; no normalization |
| TYPE-102 | ID | string | application-opaque, schema validated |
| TYPE-103 | Int32 | number | decimal integer, -2147483648..2147483647, no exponent |
| TYPE-104 | Float64 | number | finite IEEE-754 binary64; shortest round-tripping decimal; -0 becomes 0 |
| TYPE-105 | Int64 | string | optional `-`, then minimal decimal digits |
| TYPE-106 | UInt64 | string | minimal decimal digits |
| TYPE-107 | BigInt | string | optional `-`, minimal digits, zero is `"0"` |
| TYPE-108 | Decimal | string | sign plus coefficient; remove leading/trailing zeros; zero is `"0"` |
| TYPE-109 | Timestamp | string | RFC 3339 UTC `Z`, nanoseconds trimmed right; leap seconds rejected |
| TYPE-110 | Duration | string | signed integer nanoseconds in v1 core codec |
| TYPE-111 | UUID | string | lowercase RFC 4122 hyphenated form |
| TYPE-112 | Bytes | string | RFC 4648 base64url without padding |

- **TYPE-113:** Int64, UInt64, BigInt, and Decimal MUST NOT use JSON numbers.
- **TYPE-114:** NaN and infinities are invalid. JSON negative zero is accepted
  only for Float64 and canonicalizes to zero.
- **TYPE-115:** A string containing `"123"` remains String unless the declared
  schema type selects an extended numeric codec.

Valid: `"9007199254740993"` as an Int64.

Invalid: `9007199254740993` as an Int64 or a Decimal canonicalized as `"1.00"`.

## Composite values

- **TYPE-200:** Lists preserve order. Failed output elements use null
  placeholders with indexed errors; missing list elements do not exist.
- **TYPE-201:** Maps accept every valid Unicode string key, including empty and
  numeric-looking keys. Empty map `{}` and empty list `[]` are distinct.
- **TYPE-202:** Reserved expression keys have syntax meaning only at language
  control positions. Application maps remain inert literals.
- **TYPE-203:** Closed enums reject unknown values. Open enums retain an
  explicit unknown-value representation in clients.
- **TYPE-204:** Open unions contain a stable tag and value. Unknown tags remain
  representable; closed unions reject them.
- **TYPE-205:** A one-of input has exactly one present member after variable
  substitution and defaults. A present null member counts as active only when
  that member is nullable.
- **TYPE-206:** Output completion performs no broad implicit coercion. A failed
  required output is represented as unavailable partial data, never by deleting
  unrelated parent data.

Valid: `{ "0": "zero", "": "empty", "😀": "face" }` as a string-keyed map.

Invalid: one-of input `{}` or `{ "email": null, "phone": "1" }` when `email`
is nullable, because zero and two active members respectively are forbidden.

## Custom scalars

- **TYPE-300:** A custom scalar declares a stable type identifier, accepted
  wire shape, validator, serializer, canonicalizer, and limits.
- **TYPE-301:** The canonicalizer MUST be deterministic, pure, and independent
  of locale, host clock, and process state.
- **TYPE-302:** Native date/decimal adapters MAY be used only when they preserve
  the declared range and precision; otherwise they retain a lossless wrapper or
  reject conversion.

Valid: a custom Money scalar normalizes its explicitly declared currency and
decimal fields without I/O.

Invalid: canonicalization uses the machine locale or current timezone.
