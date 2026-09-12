# HTTP binding

The `core.http-1` profile binds one Naatre operation to one HTTP exchange. Its
portable cases are [http.json](../../conformance/v1/http.json). This document
defines carrier semantics; it does not make HTTP method, authentication state,
or cacheability part of a Naatre operation's semantic identity.

## Endpoint and methods

- **HTTP-001:** Every conforming server MUST accept single-operation requests
  at `POST /v1/execute`. One HTTP request carries exactly one PROTO-001
  envelope, and one non-streaming exchange returns exactly one PROTO-100
  envelope or one RFC 9457 Problem Details object.
- **HTTP-002:** A server MAY implement the GET form. If enabled, GET is allowed
  only for a persisted operation whose server-side declaration says it is
  HTTP-GET eligible, whose selected operation is a query, and whose complete
  transitive plan is no-side-effect. Eligibility is server metadata and cannot
  be asserted by a request.
- **HTTP-003:** The GET query has exactly `algorithm`, `canonicalVersion`,
  `digest`, optional `operation`, and optional `variables` members.
  `algorithm`, `canonicalVersion`, and `digest` form the CANON-201 persisted
  document record. `variables` is unpadded base64url of the CANON-222 canonical
  coerced-variable JSON bytes. Unknown or repeated query members are malformed.
- **HTTP-004:** GET MUST reject inline documents, non-persisted operations,
  mutations, subscriptions, transitively effectful queries, unapproved
  variables, and any operation or variable classified as sensitive for URL,
  browser-history, access-log, referrer, or intermediary exposure. GET
  eligibility alone does not permit shared caching.
- **HTTP-005:** Servers MUST configure and enforce a maximum request-target
  byte length before query decoding. Exceeding it returns `414`; a GET that is
  otherwise ineligible returns `400 GET_NOT_ELIGIBLE`. A server that does not
  implement GET returns `405` and an `Allow` header containing `POST`.
- **HTTP-006:** `HEAD`, `PUT`, `PATCH`, `DELETE`, and unrecognized methods MUST
  NOT execute an operation and return `405`. `OPTIONS` MAY answer a CORS
  preflight with `204` but MUST NOT decode or execute a Naatre operation.

## Media types and negotiation

- **HTTP-100:** POST request content type is
  `application/vnd.naatre.request+json;version=1`. Response content type is
  `application/vnd.naatre.response+json;version=1`. Parameter names and media
  type tokens compare case-insensitively; `version` value `1` is exact.
- **HTTP-101:** The only permitted charset parameter is `charset=utf-8`,
  compared case-insensitively. Its absence means UTF-8. A missing POST
  `Content-Type`, generic `application/json`, another version, another charset,
  or an unknown parameter returns `415 UNSUPPORTED_MEDIA_TYPE` before JSON
  decoding.
- **HTTP-102:** An absent `Accept`, `*/*`, or an acceptable v1 response range
  permits the v1 response. Otherwise normal quality weighting applies; if all
  compatible v1 ranges have quality zero or only another version is
  acceptable, the server returns `406 NOT_ACCEPTABLE` before execution.
- **HTTP-103:** RFC 9457 errors use `application/problem+json`. A client that
  accepts the Naatre response media type MUST be prepared to receive Problem
  Details for transport-phase failure. Problem Details is not a second Naatre
  envelope and MUST NOT be embedded in `data` or `errors`.
- **HTTP-104:** `Content-Type`, `Content-Encoding`, `Authorization`,
  `Naatre-Timeout-Ms`, `Naatre-Schema`, and `Naatre-Tenant` are singleton
  fields for this profile. Multiple field lines, comma-joined values, or
  conflicting values are `400 MALFORMED_HEADER`. List-valued fields including
  `Accept`, `Accept-Encoding`, and `Naatre-Capabilities` follow HTTP list
  combination rules and reject malformed syntax or conflicting capability
  requirements.
- **HTTP-105:** `Naatre-Capabilities` is the canonical, comma-separated,
  duplicate-free set of negotiated capability identifiers. If both the request
  envelope and header carry capabilities they MUST name the same set; mismatch
  is `400 MALFORMED_HEADER`. Responses emit only capabilities actually
  negotiated by the server.
- **HTTP-106:** Every response SHOULD carry a server-generated
  `Naatre-Request-Id` equal to the envelope `requestId` when an envelope is
  present. An incoming value has no authority and MUST be discarded. Servers
  MUST NOT reflect unvalidated correlation, tracing, forwarding, or identity
  headers into a response.
- **HTTP-107:** `Naatre-Schema` identifies the exact CANON-201 schema digest
  used for validation and cache identity. `Naatre-Tenant`, when emitted or
  accepted from a trusted edge, is the authenticated tenant's canonical opaque
  identifier. A client assertion cannot select either value; mismatch with
  trusted request context fails before cache lookup or execution.

## Content encoding, limits, and cancellation

- **HTTP-200:** Request `Content-Encoding` is absent, `identity`, or one
  `gzip` coding. Stacked codings and every other coding return
  `415 UNSUPPORTED_CONTENT_ENCODING` before decompression. A streaming response
  uses identity encoding; `gzip` streaming is not part of `core.http-1`.
- **HTTP-201:** Independently configured compressed and decompressed byte
  limits apply to every request. A declared `Content-Length` over the
  compressed limit is rejected before reading the body. Incremental compressed
  and decompressed counters MUST stop reads as soon as either limit is crossed;
  the implementation MUST NOT buffer the full body or expansion first.
- **HTTP-202:** Exceeding either body limit returns `413 REQUEST_TOO_LARGE` if
  headers remain writable. Truncated gzip, trailing compressed members,
  checksum failure, and expansion-ratio exhaustion are malformed transport
  input and never reach protocol decoding or business handlers.
- **HTTP-203:** `Naatre-Timeout-Ms`, when present, is one ASCII decimal integer
  from 1 through 2^63-1 with no sign, whitespace, decimal point, or exponent.
  Invalid syntax returns `400 INVALID_TIMEOUT`. The effective deadline is the
  earliest of the validated client duration, the server request maximum, and
  the inbound carrier-context deadline.
- **HTTP-204:** Admission, body reads, decoding, validation, authorization,
  execution, completion, serialization, and streaming writes all observe the
  effective context. A disconnect or deadline stops new admissions and cancels
  active non-durable handlers. Handler waiting and retained accounting follow
  CORE-305 and CORE-308.
- **HTTP-205:** Timeout before a valid envelope is decoded is transport
  `408 REQUEST_TIMEOUT`. Expiry after decoding and admission is a Naatre
  `504 RESOURCE_EXHAUSTED` execution response. If the peer has disconnected or
  response headers were sent, the server terminates the transport and does not
  write a contradictory second body.
- **HTTP-206:** A non-streaming response uses identity encoding unless the
  client permits `gzip` through `Accept-Encoding` and the server elects it.
  Compression occurs only after bounded serialization; compressed output has
  its own byte limit. The response sends the chosen `Content-Encoding`, varies
  on `Accept-Encoding`, and never sends gzip bytes to a client that assigned
  gzip quality zero.

## Status and body-kind mapping

- **HTTP-300:** A transport-phase failure uses one RFC 9457 object with
  `type`, `title`, `status`, and stable string extension `code`. `type` is a
  stable URI reference controlled by the implementation. Safe optional
  `detail` and `instance` values MUST NOT disclose credentials, variables,
  principals, internal causes, or protected resource existence.
- **HTTP-301:** A decoded Naatre failure uses one PROTO-100 envelope with
  `requestId` and public structured errors. It MUST NOT contain Problem Details
  members. The following mapping is exact for `core.http-1`:

| Condition | Status | Body | Stable code |
| --- | ---: | --- | --- |
| complete success | 200 | Naatre | `OK` |
| partial execution | 200 | Naatre | `PARTIAL` |
| malformed header, query, or JSON | 400 | Problem | `MALFORMED_HEADER`, `GET_NOT_ELIGIBLE`, or `MALFORMED_JSON` |
| unauthenticated before decode/admission | 401 | Problem | `UNAUTHENTICATED` |
| forbidden after decoding | 403 | Naatre | `UNAUTHORIZED` |
| method not allowed | 405 | Problem | `METHOD_NOT_ALLOWED` |
| unacceptable response media type | 406 | Problem | `NOT_ACCEPTABLE` |
| transport request timeout | 408 | Problem | `REQUEST_TIMEOUT` |
| request body too large | 413 | Problem | `REQUEST_TOO_LARGE` |
| request target too long | 414 | Problem | `URI_TOO_LONG` |
| unsupported request media type or encoding | 415 | Problem | `UNSUPPORTED_MEDIA_TYPE` or `UNSUPPORTED_CONTENT_ENCODING` |
| operation validation failure | 422 | Naatre | `VALIDATION_FAILED` |
| rate limited before execution | 429 | Problem | `RATE_LIMITED` |
| internal execution failure | 500 | Naatre | `INTERNAL` |
| overload before decode/admission | 503 | Problem | `OVERLOADED` |
| admitted execution deadline | 504 | Naatre | `RESOURCE_EXHAUSTED` |
| failure after headers | no new status | terminate | `TRANSPORT_TERMINATED` locally |

- **HTTP-302:** `401` includes an appropriate `WWW-Authenticate` challenge.
  `429` and `503` MAY include a bounded `Retry-After`. These headers do not
  weaken response redaction and clients MUST validate their syntax before use.
- **HTTP-303:** A server MUST choose the body kind from the phase, not merely
  from status class. Clients MUST inspect `Content-Type` and retain a valid
  Naatre envelope or Problem Details object on non-2xx responses instead of
  replacing every HTTP error with an undifferentiated network error.

## Principal, GET, and cache safety

- **HTTP-400:** Authentication middleware establishes the immutable SEC-002
  principal in trusted request context before Naatre decoding. Envelope fields,
  variables, extensions, query members, `Naatre-Principal`, and untrusted
  forwarding headers MUST NOT create, replace, or enrich that principal.
  A client-supplied `Naatre-Principal` is `400 UNTRUSTED_IDENTITY_HEADER`.
- **HTTP-401:** A deployment behind a trusted identity proxy MUST strip
  identity and tenant headers from untrusted traffic, authenticate the proxy,
  and overwrite trusted context values. `Naatre-Tenant` used for cache
  partitioning MUST equal the authenticated principal tenant; it is not
  independent authorization evidence.
- **HTTP-402:** POST responses default to `Cache-Control: no-store`. GET
  responses default to `Cache-Control: private, no-store`. No request header,
  capability, status, method, or GET eligibility can promote a response to
  cacheable or public.
- **HTTP-403:** A private cacheable GET requires an explicit persisted-operation
  declaration and binds its cache entry to subject, tenant, schema revision,
  authorization revision, document, canonical coerced variables, negotiated
  capabilities, response media type, and content encoding.
- **HTTP-404:** Shared caching additionally requires a versioned server-side
  declaration that output is public and authorization-invariant for all
  possible inputs. The declaration is rejected if policy, field visibility,
  locale, cookies, credentials, feature flags, time, or principal state can
  change the representation. Runtime authorization still occurs on cache fill.
- **HTTP-405:** A shared key contains the CANON-201 document digest, schema
  digest or revision, CANON-222 canonical coerced-variable identity, negotiated
  cache-relevant capabilities, authenticated tenant partition, response media
  type, content encoding, and public-classification revision. Every dimension
  is length-delimited or represented in canonical JSON before hashing.
- **HTTP-406:** Sensitive variables MUST never appear in a GET URI. Canonical
  variables are hashed for cache identity; bearer credentials, cookies,
  request IDs, and raw principal claims are never stored in a shared key.
  Distinct tenants MUST NOT share an entry even for byte-identical output.
- **HTTP-407:** A shared response sends an explicit public `Cache-Control`
  policy and `Vary: Accept, Accept-Encoding, Naatre-Capabilities,
  Naatre-Schema, Naatre-Tenant`. A private authorization-variant response also
  varies on `Authorization` and remains private. Intermediaries MUST NOT invent
  omitted dimensions or cache `no-store` responses.
- **HTTP-408:** Cacheable responses use a representation-specific strong
  `ETag`. `If-None-Match` is evaluated only after the same eligibility,
  authentication, tenant, schema, capability, and cache-key checks as a normal
  request. A match returns `304` with the same cache policy and `Vary` and no
  body; it never bypasses current authorization for private entries.

## Browsers, redirects, and streaming

- **HTTP-500:** CORS is deployment policy. An allowed cross-origin request
  receives an exact origin or other safe configured value, correct credential
  policy, allowed methods and headers, and `Vary: Origin` when the result
  depends on origin. A preflight never authenticates or executes an operation.
- **HTTP-501:** Cookie-authenticated unsafe requests require an explicit CSRF
  defense that validates origin and a server-issued token or equivalent
  same-origin proof before decoding. CORS alone is not CSRF protection.
  Authorization-header clients still obey configured origin policy.
- **HTTP-502:** Bearer credentials, cookies, CSRF tokens, and sensitive
  variables MUST NOT appear in URLs. Clients and servers MUST disable or
  redact sensitive request-target logging and MUST NOT rely on browser history
  as credential storage.
- **HTTP-503:** Servers SHOULD avoid redirects for `/v1/execute`. Clients MUST
  reject cross-origin redirects by default and MUST NOT forward
  `Authorization`, cookies, or trusted tenant/identity headers across origins
  without an explicit allowlist and fresh policy decision. A `3xx` is not a
  Naatre response.
- **HTTP-504:** Authenticated POST streaming uses Fetch-based consumption with
  `Accept: text/event-stream`; native `EventSource` cannot carry the POST body
  or arbitrary `Authorization` header and is not a conforming substitute.
  A separate subscription-handle flow requires its own negotiated profile.
- **HTTP-505:** An SSE response is UTF-8 `text/event-stream` with identity
  content encoding. Each `data` event contains one complete compact JSON
  Naatre response envelope; comment heartbeats carry no semantics. Proxy
  buffering and transformation MUST be disabled so cancellation, limits, and
  event delivery remain observable.

## Client decoding, proxies, and HTTP versions

- **HTTP-600:** Clients MUST configure compressed and decompressed response
  byte limits and apply them incrementally before full buffering. Unknown
  content encodings, expansion exhaustion, premature EOF, invalid framing,
  truncated JSON, and trailing data produce a structured local transport error
  such as `RESPONSE_LIMIT_EXCEEDED` or `TRUNCATED_RESPONSE`.
- **HTTP-601:** A fully decoded Problem Details or Naatre error on any status is
  retained with its body kind and public code. A truncated response is never
  accepted as a valid envelope; clients MAY retain already validated diagnostic
  context alongside the local truncation error but MUST NOT invent missing
  success data.
- **HTTP-602:** Reverse proxies MUST preserve method, request target, media and
  content-encoding fields, cancellation, streaming flushes, status, body kind,
  `Cache-Control`, `Vary`, `ETag`, request identifiers, and authenticated
  context boundaries. They MUST apply compatible or stricter request and
  response limits and MUST NOT retry a mutation without an applicable
  idempotency contract.
- **HTTP-603:** HTTP/1.1, HTTP/2, and HTTP/3 carry equivalent Naatre semantics.
  Implementations MUST NOT depend on reason phrases, connection-specific
  fields, header casing, stream IDs, push, or trailers. All semantic status,
  error, completion, and cache information appears in headers available before
  the body and in the body itself.
