# Canonicalization and semantic hashing

The `core.interop.c14n-1` profile defines the bytes used for persisted
operations, schemas, approvals, caches, idempotency records, and signed
messages. Its golden and malformed vectors are
[canonical.json](../../conformance/v1/canonical.json). Both the Go reference
implementation and the dependency-free JavaScript verifier execute that file.

## Parsed JSON value requirements

- **CANON-001:** Input is UTF-8 without a BOM. Invalid byte sequences, unpaired
  surrogate escapes, duplicate object members after unescaping, unescaped
  controls, and trailing data are rejected before canonicalization. A decoder
  must retain member occurrences until duplicate detection is complete; a host
  map that silently keeps the first or last occurrence is not conforming.
- **CANON-002:** Every string and member name is a sequence of Unicode scalar
  values. Application strings are preserved exactly; canonicalization MUST NOT
  normalize NFC/NFD, fold case, or apply locale rules. U+FFFD supplied by the
  client is data and is not confused with decoder replacement.
- **CANON-003:** Arrays retain their input order. Object members sort by their
  unescaped names in ascending UTF-16 code-unit order as required by RFC 8785,
  including surrogate-pair code units for non-BMP scalars. UTF-8 byte, Unicode
  scalar, host-map, and locale ordering are invalid substitutes.
- **CANON-004:** Strings use lowercase `\u00xx` escapes for U+0000 through
  U+001F except the short escapes `\b`, `\t`, `\n`, `\f`, and `\r`; quote and
  reverse solidus are escaped. Every other scalar is emitted directly as UTF-8.
- **CANON-005:** JSON numbers use RFC 8785's finite IEEE-754 binary64 model and
  ECMAScript number serialization. NaN and infinities are never JSON values;
  negative zero serializes as `0`. A producer that needs exact integers outside
  `[-9007199254740991, 9007199254740991]` MUST use a declared extended scalar,
  not a JSON number token.
- **CANON-006:** Empty object and list remain `{}` and `[]`. A missing member is
  absent and an explicit null is `null`; neither is inserted or rewritten.
  Whitespace and input object-member order are discarded.
- **CANON-007:** `Int64`, `UInt64`, `BigInt`, `Decimal`, `Timestamp`,
  `Duration`, `UUID`, and `Bytes` use the exact string codecs in TYPE-105
  through TYPE-112. Their values are canonicalized before they enter a
  purpose payload and are never converted through a JSON number or host float.
- **CANON-008:** The JSON layer is RFC 8785 with the stricter decoding and
  resource limits above. RFC 8785 alone is insufficient for Naatre extended
  scalars because its input model intentionally stops at binary64.

Valid: the non-BMP key `😀` sorts before the BMP private-use key ``, NFC and
NFD strings remain distinct, and `"9007199254740993"` round-trips byte-exact as
an `Int64` value.

Invalid: sorting keys by UTF-8, normalizing strings, accepting a BOM, accepting
two escaped spellings of the same key, or encoding an `Int64` as an imprecise
JSON number.

## Portable identifiers

- **CANON-020:** Language identifiers match
  `[A-Za-z_][A-Za-z0-9_]{0,127}`. Schema type, capability, extension, profile,
  hash-algorithm, canonical-version, and key identifiers match
  `[A-Za-z_][A-Za-z0-9_.-]{0,127}` unless their owning clause publishes a
  smaller closed set. They are ASCII, case-sensitive, and are never Unicode- or
  locale-normalized.
- **CANON-021:** Distinct identifier spellings remain distinct. `User`, `user`,
  and `USER` cannot alias one another, and a registry MUST reject a duplicate
  exact spelling rather than choose by insertion order.

Valid: `core.language-1`, `User.Profile`, and `_internal` retain their exact
spelling.

Invalid: accept `user name`, normalize `É` into an identifier, or resolve
`User` by a case-insensitive lookup.

## Canonical document and schema models

- **CANON-100:** A document is first decoded and validated against the claimed
  language profile. Its canonical AST contains only fields defined by that
  profile, omits absent optional fields, preserves explicitly supplied defaults
  and nulls, and then applies CANON-001 through CANON-006.
- **CANON-101:** Operation, fragment, selection, pipeline-stage, parallel-branch,
  argument-position, and directive arrays remain ordered. Reordering one changes
  document identity even when a particular server might produce the same
  result. Object member order never changes identity.
- **CANON-102:** Only the finite normalization rules in this document are
  applied. Implementations MUST NOT hash optimized plans, sort ordered
  selections, inline fragments, remove apparently redundant work, or attempt
  arbitrary program equivalence.
- **CANON-103:** Document identity is schema-free. Operation kind, declarations
  and explicit defaults, aliases, bindings, directives, fragments, and required
  capability or extension references contribute to it. Request variables,
  correlation IDs, transport metadata, approval metadata, and server schema
  revisions do not.
- **CANON-104:** Schema identity is formed from its portable public descriptor,
  never host reflection or registration order. Type, field, enum-member,
  variant-member, operation, object-member, directive, directive-argument,
  retired-identity, and trait arrays
  sort by stable `id`; pinned references sort by URI then revision. Entity keys,
  legacy type-reference/enum-value lists, accepted-wire-shape lists, and
  capability, directive-location, and directive-phase lists sort by their
  canonical spelling and reject duplicates.
  Ordered custom-scalar conformance vectors retain order. Absent optional
  descriptor fields are omitted; explicit defaults are canonical scalar JSON.
  A schema construction API MUST coerce and store defaults in that canonical
  form before exposing the public descriptor; a serialized descriptor containing
  a non-canonical default is invalid input, not a second spelling with a
  different schema identity.
- **CANON-105:** Schema-free document producers can compute document identity
  without a live server. A schema-dependent producer MUST pin the schema bundle
  used to coerce variables and carry its schema digest into the relevant
  approval, cache, or idempotency payload. A schema revision never alters the
  document payload silently.

Valid: reordering document object members or schema registration calls retains
identity, while changing operation kind, a variable default, required
capabilities, ordered selections, a directive version/capability, or a schema
revision changes the identity it owns.

Invalid: hash a request envelope as the document, sort selections, or mix a new
schema revision into an old approval without producing a new approval digest.

## Hash domains and digest representation

- **CANON-200:** The exact hash input is the UTF-8 concatenation
  `"naatre:" + purpose + ":" + canonicalVersion + "\n" + canonicalPayload`.
  The separator is one byte `0A`; there is no NUL, length prefix, BOM, or final
  newline unless it belongs to the payload.
- **CANON-201:** v1 requires algorithm identifier `sha-256` and independently
  versioned canonicalization identifier `c14n-1`. A digest record is exactly
  `{ "algorithm": "sha-256", "canonicalVersion": "c14n-1", "digest":
  LowercaseHex64 }`. Transport and product releases do not rename `c14n-1`.
- **CANON-202:** The closed v1 purposes are `document`, `schema`, `approval`,
  `result-cache`, `idempotency`, and `signed-message`. Verifiers compare the
  algorithm, canonical version, purpose, and digest; a digest from one purpose
  MUST NOT be accepted for another even if the payload bytes match.
- **CANON-203:** Unknown algorithms, versions, purposes, uppercase hex, wrong
  digest lengths, and digest records with unknown members fail closed. A future
  algorithm or canonicalization revision requires a newly negotiated profile
  and new golden vectors; it never changes the meaning of an existing record.
  The request protocol's `persisted` member is exactly a CANON-201 document
  digest record and therefore carries `canonicalVersion`; it is not a
  version-less storage locator.

The `domainInput` member in every golden hash vector is the literal input to
SHA-256, including its newline. This prevents SDKs from accidentally hashing a
hex digest as bytes, omitting the profile version, or adding transport framing.
The six `same-payload-*` vectors are synthetic domain-separation tests over the
same already-canonical JSON bytes; the named purpose-payload vectors below them
exercise each purpose's actual normalization contract.

## Purpose payloads

Each payload below is a JSON value canonicalized with `c14n-1`. Digest records
are embedded as objects, not as untagged hex strings.

- **CANON-220:** `document` payload is the canonical language AST and nothing
  else. `schema` payload is the canonical public schema descriptor including
  its revision.
- **CANON-221:** `approval` payload is an object with `document` and `schema`
  digest records, `policyRevision`, canonical `capabilities`, and
  `approvalMetadata`. Capability order is normalized as a duplicate-free set;
  metadata is policy-owned JSON. Changing any of these produces a new approval.
- **CANON-222:** `result-cache` payload contains `document` and `schema` digest
  records, canonical coerced `variables`, and every capability or runtime
  revision declared cache-relevant by the negotiated profile. It MUST NOT
  contain request IDs, tracing, deadlines, or approval metadata.
- **CANON-223:** `idempotency` payload contains `document` and `schema` digest
  records, canonical coerced `variables`, the application idempotency key, and
  its authenticated principal or tenant scope. A cache digest cannot substitute
  for it; retry policy and stored outcome remain application state.
- **CANON-224:** `signed-message` payload is the exact profile-defined envelope
  to be signed, including the signature algorithm identifier, key identifier,
  message, nonce, and issued-at value when that profile requires them. The
  signature bytes themselves are excluded. Verification reconstructs this
  payload and domain before signature verification.
- **CANON-225:** Variables or approval metadata never change `document` identity.
  Coerced-variable changes do change `result-cache` and `idempotency` identity;
  policy, capability, schema, or approval-metadata changes do change `approval`
  identity. The isolation vectors test both halves of these rules.

Valid: one payload hashed under all six purposes produces six different
digests; two requests with the same document and different variables retain one
document digest and have different cache/idempotency digests.

Invalid: include a correlation ID in a cache key, omit principal scope from an
idempotency fingerprint, reuse a document digest as an approval identity, or
sign transport whitespace rather than the canonical signed-message payload.
