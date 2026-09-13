# Request and response protocol

## Single-operation request

- **PROTO-001:** A request is a UTF-8 JSON object containing only `version`,
  `id`, `operation`, `document`, `persisted`, `variables`, `capabilities`, and
  `extensions`.
- **PROTO-002:** `version` MUST equal `"1"` unless a transport media type has
  already negotiated v1; disagreement is an `UNSUPPORTED_VERSION` decode
  error.
- **PROTO-003:** `id`, when present, is a string of 1..128 Unicode scalar
  values. It is opaque correlation only, not authentication, authorization,
  idempotency, or tracing authority.
- **PROTO-004:** Exactly one of `document` and `persisted` MUST be present.
  `document` is the typed document object. `persisted` is an object containing
  exactly `algorithm`, `canonicalVersion`, and lowercase `digest`, using the
  `sha-256` / `c14n-1` document digest record from CANON-201. Unknown algorithms
  or canonicalization versions fail closed. Resolution, approval, and
  allowlist-only admission follow PERSIST-001 through PERSIST-114.
- **PROTO-005:** `operation` is required only when a document contains more
  than one operation. It cannot change the operation's declared kind.
- **PROTO-006:** `variables` is an unordered object of application literals.
  Missing variables and variables explicitly set to null remain distinct.
- **PROTO-007:** `capabilities` is an order-insensitive array of unique,
  case-sensitive ASCII identifiers matching
  `[A-Za-z_][A-Za-z0-9_.-]{0,127}`. Unsupported required capabilities fail
  validation.
- **PROTO-008:** `extensions` keys MUST be registered reverse-DNS namespaces.
  Unknown normative fields and unnegotiated extension namespaces are rejected.
  Exact version/capability negotiation, inert optional metadata, and legacy raw
  namespace policy follow EXT-100 through EXT-103.
- **PROTO-009:** Duplicate members after JSON unescaping, trailing data, BOMs,
  invalid UTF-8, and unpaired UTF-16 surrogate escapes are decoding failures.
- **PROTO-010:** The single-operation envelope MUST NOT be used as a batch.

Valid:

```json
{"version":"1","id":"c-7","document":{"operations":[{"name":"Get","kind":"query","select":[]}]},"variables":{},"capabilities":[],"extensions":{}}
```

Invalid (conflicting sources):

```json
{"version":"1","document":{"operations":[]},"persisted":{"algorithm":"sha-256","canonicalVersion":"c14n-1","digest":"00"}}
```

## Response

- **PROTO-100:** A response is an object containing `id` when supplied,
  `requestId`, `data`, `errors`, `capabilities`, and `extensions`.
- **PROTO-101:** `requestId` is a server-generated opaque string. A server MUST
  NOT trust a client value for it.
- **PROTO-102:** `data` is omitted when execution never began and otherwise is
  the partial-result value, including explicit null when the root completed as
  null.
- **PROTO-103:** `errors` is an array in normative stable order and is omitted
  when empty. Each error contains `code`, `message`, `path`, optional `source`,
  `retryable`, and negotiated namespaced `details`.
- **PROTO-104:** Internal causes and stack traces MUST NOT appear by default.
- **PROTO-105:** `capabilities` contains only server-negotiated capabilities;
  it MUST NOT echo unsupported client assertions. Extension response values and
  namespaced error details require the corresponding exact selected capability.
- **PROTO-106:** The `core.protocol-1` conformance profile MUST publish
  language-neutral request and response vectors covering success, simultaneous
  data and errors, validation failure, unsupported version, malformed input,
  unknown capability, and malformed response envelopes.

Valid partial response:

```json
{"id":"c-7","requestId":"s-9","data":{"user":{"name":"Ada"}},"errors":[{"code":"FIELD_FAILED","message":"field unavailable","path":["user","email"],"retryable":false}],"capabilities":[],"extensions":{}}
```

Invalid: omitting `requestId`, returning an internal database error as the
public message, or using a string `"0"` instead of numeric list index `0` in an
error path.

## Media types and limits

- **PROTO-200:** Request and response envelopes are UTF-8 JSON. Each transport
  profile MUST assign explicit versioned media types and MUST NOT infer Naatre
  semantics from `application/json`. The normative HTTP media types and
  negotiation rules are defined by HTTP-100 through HTTP-106.
- **PROTO-201:** Transports MUST declare and enforce byte, token, nesting,
  string, member, array-item, identifier, and numeric-token limits before full
  materialization.
- **PROTO-202:** Malformed envelopes are decode errors. Well-formed documents
  that violate language/schema rules are validation errors. Handler failures
  are execution errors. HTTP status does not redefine these phases.
