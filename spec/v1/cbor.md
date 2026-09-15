# Deterministic CBOR transport profile

The `transport.cbor.unary-1` capability is Naatre's unary deterministic CBOR
profile. It is pinned to RFC 8949 and codec revision `cbor-det-1`. JSON remains
the mandatory human-readable representation. CBOR is selected explicitly and
does not change the typed language, schema, validation, planning, authorization,
execution, completion, or semantic-hashing models.

## Profile and deterministic serialization

- **CBOR-001:** A producer or consumer advertises this profile only as the
  exact `transport.cbor.unary-1` capability with codec revision `cbor-det-1`.
  The unary request and response media types are respectively
  `application/vnd.naatre.request+cbor;profile=transport.cbor.unary-1;version=1`
  and `application/vnd.naatre.response+cbor;profile=transport.cbor.unary-1;version=1`.
  Compatible version ranges, omitted profiles, and implicit selection from a
  wildcard media range are forbidden.
- **CBOR-002:** Every item uses RFC 8949 preferred serialization for integer,
  tag, string-length, array-length, and map-length arguments. All strings,
  arrays, and maps use definite lengths. Simple values are exactly false,
  true, and null. Undefined, break outside framing, unassigned simple values,
  indefinite lengths, and trailing items are forbidden.
- **CBOR-003:** Every Float64 uses the binary64 form even when its value is
  exactly representable at a narrower width. NaN and infinities are forbidden.
  Negative zero is not a preferred Naatre value and encodes as positive
  binary64 zero; a decoder rejects a negative-zero byte form.
- **CBOR-004:** Map keys are valid UTF-8 text strings and appear in RFC 8949
  deterministic order: shorter encoded keys first, then bytewise lexical order
  of their complete deterministic encodings. Duplicate keys are rejected
  before insertion. Application maps never accept integer, byte-string, tagged,
  array, or map keys.
- **CBOR-005:** The only allowed semantic tags are 0 (Timestamp), 2 and 3
  (BigInt), 4 (Decimal), 37 (UUID), and the profile-local tags 60000 (Int64),
  60001 (UInt64), and 60002 (Duration). Unknown tags and a permitted tag with a
  wrong, non-preferred, or out-of-range payload fail closed.

## Scalar and structural mapping

- **CBOR-100:** Missing has no CBOR item and is represented only by omitting an
  optional object or map member. Null is simple value 22; Boolean is simple 20
  or 21; String, ID, and enum spellings are unnormalized UTF-8 text. Int32 uses
  major type 0 or 1 and MUST remain in its declared range.
- **CBOR-101:** Int64 is tag 60000 around a major-type 0 or 1 integer; UInt64 is
  tag 60001 around major type 0. BigInt is tag 2 for non-negative values and tag
  3 for negative values using the RFC 8949 unsigned big-endian magnitude, with
  no leading zero octet. The tags distinguish extended numeric scalars whose
  JSON forms are strings from an Int32 JSON number.
- **CBOR-102:** Decimal is tag 4 around the definite two-item array
  `[exponent, mantissa]`, denoting `mantissa * 10^exponent`. Its mantissa uses a
  major integer or tag 2/3. Zero is `[0,0]`; every non-zero coefficient removes
  trailing decimal zeroes and increments the exponent, so numerically equal
  decimal byte alternatives are rejected.
- **CBOR-103:** Float64 uses the rule in CBOR-003. Timestamp is tag 0 around its
  canonical TYPE-109 UTC text with at most nanosecond precision. Duration is tag
  60002 around its TYPE-110 signed integer nanosecond count. UUID is tag 37
  around exactly 16 bytes. Bytes is an untagged byte string.
- **CBOR-104:** Lists are arrays and preserve order. String-keyed maps, input
  objects, and output objects are maps governed by CBOR-004. Empty arrays and
  maps remain distinct. A tagged union is exactly the map with text keys
  `$type` and `$value`; enum and union tag spelling remains schema-validated.
  Interfaces and one-of input objects use their TYPE-006 and TYPE-205 logical
  structures without additional CBOR alternatives.

## Decode boundary and identity

- **CBOR-200:** A decoder enforces byte, nesting, array-item, map-pair,
  text-string, byte-string, bignum, semantic-tag, and cumulative allocation
  limits before allocating or constructing the complete affected value. Limits
  are finite, locally configurable downward, and cancellation is observed
  between items. Limit, cancellation, malformed, truncation, invalid UTF-8,
  duplicate-key, map-order, forbidden-tag, float-width, and non-preferred-form
  failures are stable and distinguishable.
- **CBOR-201:** Strict CBOR is projected into the same lossless logical value
  model consumed by strict JSON decoding. The canonical semantic document hash
  is computed only after that projection and CANON-100 normalization. Equivalent
  JSON and CBOR operations therefore have the same CANON-200 document identity.
  A Content-Digest or other exact HTTP digest hashes the received representation
  bytes and remains different when those bytes differ.
- **CBOR-202:** Malformed, truncated, over-budget, non-deterministic, or
  capability-incompatible CBOR is rejected before protocol validation invokes
  planning, authorization, application handlers, or result completion. A
  decoder never returns a partial successful typed value.

## HTTP, persisted operations, and conformance

- **CBOR-300:** A server accepts CBOR only when enabled and when both the exact
  request media type and `Naatre-Capabilities` advertise
  `transport.cbor.unary-1`. It sends a CBOR response only when the exact response
  media type and the same capability are advertised with positive quality.
  Wildcards select JSON, not CBOR. Unsupported request media returns 415,
  unacceptable response media returns 406, malformed CBOR returns 400, and
  transport problems remain `application/problem+json`.
- **CBOR-301:** Responses that can vary by CBOR negotiation include `Accept`,
  `Accept-Encoding`, and `Naatre-Capabilities` in `Vary`; the normal authenticated
  cache dimensions still apply. Persisted document identity never includes the
  transport representation, but any persisted approval that restricts binary
  transport pins both `transport.cbor.unary-1` and `cbor-det-1`. A conformance
  report claiming this capability records the same exact pair.
- **CBOR-302:** The language-neutral golden fixture is
  `conformance/v1/cbor.json`. A conforming codec reproduces every byte vector and
  rejection. Go is the reference codec; the independent JavaScript verifier is
  a second implementation. Issue #69 owns the complete advertised-language
  execution and publication matrix.
- **CBOR-303:** Version 1 CBOR is unary only. It does not imply CBOR Sequence,
  record separators, length prefixes, incremental item delivery, or #23 stream
  terminal semantics. Any future framed CBOR stream uses a distinct capability
  and media type and includes explicit terminal and truncation fixtures.
