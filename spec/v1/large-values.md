# Large values, files, and byte streams

This document defines the `core.large-value-1` profile. It separates small
canonical byte scalars from large payloads whose bytes move outside operation
JSON. It is transport-neutral: issue #87 owns concrete upload, download, and
object-store adapters, while this profile fixes their shared security and
identity contract.

## Boundary and identity

- **LARGE-001:** A canonical byte scalar MUST be bounded by an implementation's
  advertised small-value limit, whose core default is 65,536 bytes. Values over
  that limit use a declared payload slot and a transfer capability. A core
  runtime MUST stream large bytes to or from an application-owned store and
  MUST NOT retain the complete content in request memory.
- **LARGE-002:** A persisted operation identity MUST exclude payload bytes and
  resolved bearer references, but MUST bind each slot's stable name,
  direction, required state, transfer profile, allowed media types, and maximum
  representation length. Slot declarations are sorted by name before semantic
  hashing in the `document` domain.
- **LARGE-003:** The protected input, cache, and idempotency identity MUST bind
  the persisted operation digest plus each finalized slot's opaque object
  reference, verified representation digest, exact length, and media type.
  Reusing a result for different payload bytes MUST fail before a handler runs.

## Capabilities and authorization

- **LARGE-100:** Hosts MUST advertise supported `direct`, `presigned`,
  `multipart`, `resumable`, and `application` profiles independently. A
  capability fixes its direction, allowed methods, expiry, slot metadata, and
  cleanup owner; unsupported hooks MUST fail explicitly rather than silently
  falling back to multipart or buffering.
- **LARGE-101:** A transfer reference is a bearer capability and MUST be opaque,
  integrity protected, unguessable, scoped to subject, tenant, authorization
  revision, operation, slot, direction, methods, and immutable metadata, and
  bounded by a configured maximum TTL. It MUST be redacted from logs, traces,
  metrics, errors, and ordinary string formatting.
- **LARGE-102:** Current authorization MUST complete before issuing a capability,
  opening a staging sink, finalizing protected content, opening a finalized
  source, serving a range, following a redirect, or invoking a handler. Errors,
  denial, expiry equality, revision drift, wrong subject or tenant, revocation,
  and malformed references MUST use one unavailable result.
- **LARGE-103:** A configured single-use reference MUST be atomically claimed
  before protected consumption. Concurrent or repeated claims MUST fail closed.
  A completed claim remains spent when its handler fails; retry requires a new
  capability and remains charged to the same quota and cost identity.

## Streaming, integrity, and cleanup

- **LARGE-200:** Incoming representation bytes MUST be copied incrementally to
  an explicitly untrusted staging sink while enforcing independent encoded and
  decoded byte limits and RFC 9530 representation digest verification.
  Protected finalization MUST occur only after verified EOF; mismatch,
  truncation, excess, or I/O failure MUST leave no consumable record.
- **LARGE-201:** A configured content scanner MUST read staged content before
  finalization and MUST reject unsafe content or a detected media type that
  conflicts with the declared type. Transfer, scan, storage, retry, chunk, and
  finalized-byte costs MUST be charged to the bound principal or tenant without
  crediting abandoned retries as successful work.
- **LARGE-202:** Before finalization, the coordinator owns deterministic abort of
  failed or cancelled staging under a bounded cleanup context independent from
  request cancellation. After finalization, the store owns expiry and orphan
  deletion until transactional ownership transfers to the application. Every
  transition MUST name exactly one cleanup owner.
- **LARGE-203:** Multipart and resumable profiles MUST bind every part number,
  offset, length, validator, and digest plus the final assembled length and
  digest. Conflicting overlap, gap, validator change, or final digest mismatch
  MUST abort the assembly. Abandoned chunks remain store-owned and quota-charged
  until bounded expiry cleanup.

## Download behavior

- **LARGE-300:** HTTP downloads MUST implement conditional requests before range
  selection and MUST use strong validators for resumable protected content. The
  core profile supports one `bytes` range, returns 206 with exact
  `Content-Range`, returns 416 for an unsatisfied range, and ignores Range when
  `If-Range` does not match. Multipart ranges are an optional adapter profile.
- **LARGE-301:** Capability responses MUST default to `private, no-store` unless
  an application proves a cache key and authorization scope that bind the
  current principal, tenant, revisions, representation, content coding, range,
  and validator. Redirects MUST NOT be followed automatically; every target is
  reauthorized and revalidated, and credentials or digest state MUST NOT be
  forwarded across an unapproved origin.

## Adversarial requirements

- **LARGE-400:** Remote fetch MUST use an application-owned egress policy that
  validates the scheme, exact origin, port, redirect count, and every resolved
  address immediately before each dial. Every redirect MUST be re-resolved and
  the approved addresses pinned to that dial. Loopback, private, link-local,
  carrier-grade NAT, multicast, unspecified, and cloud metadata endpoints MUST
  be denied across redirects and DNS changes.
- **LARGE-401:** Filenames MUST be metadata only and MUST reject path separators,
  traversal segments, control characters, invalid UTF-8, and configured length
  excess. Media types MUST be parsed, normalized, allowlisted, and checked
  against scanner detection. Declared lengths MUST use checked arithmetic and
  MUST NOT drive proportional allocation.
- **LARGE-402:** Digests MUST bind the exact assembled representation and MUST be
  verified independently from bearer signatures, transport content digests,
  ETags, and object-store checksums. Content coding MUST be allowlisted;
  decompression MUST enforce encoded bytes, decoded bytes, expansion ratio,
  stream termination, and trailing-data policy before protected use.

Portable range, resume, redirect, DNS-rebinding, identity, lifecycle, and
malicious-metadata vectors are fixed by `conformance/v1/large-values.json`.
Those fixtures use reserved documentation origins and injected DNS answers;
they never fetch production endpoints.
