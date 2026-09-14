# HTTP digest fields

The `core.http.digest-1` profile pins RFC 9530 Digest Fields to the current
RFC 9651 Structured Fields data model. Its language-neutral exact-byte vectors
are [http-digest.json](../../conformance/v1/http-digest.json). The profile is a
transport integrity contract layered on [core.http-1](http.md); concrete
middleware and SDK wiring are owned by issue 104.

## Profile, fields, and algorithms

- **DIGEST-001:** Applicable exchanges use `Content-Digest`, `Repr-Digest`,
  `Want-Content-Digest`, and `Want-Repr-Digest` exactly as defined by RFC 9530.
  Digest fields are RFC 9651 dictionaries whose keys are digest algorithm
  identifiers. A deployment MUST NOT substitute an ad hoc Naatre digest
  header or interpret these fields as semantic document hashes.
- **DIGEST-002:** Each of the four fields is a singleton in this profile. The
  field value MUST be in the initial header section. Multiple field lines,
  comma-combined field lines, duplicated dictionary keys, parameters, and a
  value arriving only in trailers fail before the value is trusted. Core
  implementations publish `trailerVerificationSupported: false` and do not
  depend on intermediaries preserving trailers.
- **DIGEST-003:** The allowed algorithms are `sha-512` and `sha-256`. A sender
  MUST support generating `sha-256`; it MAY generate `sha-512` after successful
  negotiation. A verifier MUST implement both, check the exact 64-byte or
  32-byte digest length, and verify every allowed value present. One matching
  member never overrides another mismatch. Unknown or policy-forbidden
  algorithms fail with `UNSUPPORTED_DIGEST_ALGORITHM`.
- **DIGEST-004:** Want fields contain an integer weight from 0 through 10 for
  each algorithm. An absent Want field selects `sha-256`; zero refuses an
  algorithm; the highest positive weight wins; and equal weights use the
  deterministic server order `sha-512`, then `sha-256`. No positive supported
  weight fails with `NO_ACCEPTABLE_DIGEST`. Malformed or unknown entries fail;
  negotiation MUST NOT fall back to an unverified algorithm.
- **DIGEST-005:** Digest values are canonical RFC 9651 Byte Sequences using
  padded standard Base64. Empty dictionaries, invalid Base64, non-canonical
  padding, wrong item types, uppercase or invalid keys, internal whitespace,
  controls, parameters, more than the bounded member count, and fields over
  4096 bytes fail with a stable malformed or duplicate code before body reads.

## Content bytes and representation data

- **DIGEST-100:** `Content-Digest` covers the exact HTTP content octets after
  transfer framing is removed and before any content coding is decoded. HTTP/1
  chunk sizes, chunk delimiters, terminal chunks, HTTP/2 or HTTP/3 frame bytes,
  and transport padding are never input. With `Content-Encoding: gzip`, the
  gzip member bytes, including its header and checksum, are input.
- **DIGEST-101:** `Repr-Digest` covers representation data before content
  coding. For identity coding it commonly equals `Content-Digest`; for gzip or
  any future permitted transformation the two inputs differ. An implementation
  MUST NOT copy, relabel, or compare one field as the other merely because both
  use the same algorithm.
- **DIGEST-102:** A `206` `Content-Digest` covers only the returned range
  content. Its `Repr-Digest` covers the complete selected representation and
  is meaningful only with validated `Content-Range`, validators, and variant
  metadata. Multipart byte ranges hash the exact outer multipart content for
  `Content-Digest`; boundaries and part fields are included. A client MUST NOT
  treat a range content digest as proof of the complete representation.
- **DIGEST-103:** For multipart uploads, the outer digest covers exact
  multipart content including boundaries and part header sections; a required
  per-part digest covers that part's content. A resumable upload verifies every
  required chunk before durable acceptance and verifies the final
  `Repr-Digest` over the assembled object before object finalization. Chunks
  MUST NOT be finalized by concatenating or otherwise combining digest values.
- **DIGEST-104:** A redirect response's digests cover that response, never the
  followed target. Each followed exchange negotiates and verifies independently.
  A proxy that transforms content or representation data MUST remove stale
  fields or recompute every affected field; preserving a stale field causes a
  mismatch. Cache variants bind content coding, range, validators, and all
  representation-selecting metadata.

## Verification phases and streaming

- **DIGEST-200:** Policy declares where a digest is required. Protected
  operation requests verify before protocol decoding or business-handler use;
  uploads verify before durable chunk acceptance and final object promotion;
  remote payloads verify before signature verification or trusted parsing;
  responses verify before cache insertion or successful SDK completion. A
  required absent field fails with `DIGEST_REQUIRED` at the same gate.
- **DIGEST-201:** A mismatch fails with `DIGEST_MISMATCH` and the staged bytes
  remain untrusted. Premature EOF against an independently established length
  fails with `DIGEST_TRUNCATED`; excess bytes fail with a length or resource
  error. No protected handler, durable finalizer, cache insertion, signature
  decision, retry success, or client success may occur after those failures.
- **DIGEST-202:** Verification is incremental with fixed-size hash state and a
  configured maximum content byte count. Implementations MUST stop after at
  most one byte beyond that limit and return `DIGEST_LIMIT_EXCEEDED`; they MUST
  NOT allocate from an attacker-declared length or buffer an unbounded body.
  Bytes may be written only to an explicitly untrusted bounded or durable
  staging sink until verified EOF.
- **DIGEST-203:** A streaming message with an initial digest may be consumed
  incrementally, but successful completion is withheld until verified EOF.
  Long-lived SSE or indefinite streams that cannot know a whole-content digest
  in the initial header do not claim this profile for the whole stream and use
  their negotiated frame integrity contract. Trailer-only verification is not
  a core fallback.
- **DIGEST-204:** Malformed fields, unsupported algorithms, duplicates, and
  forbidden policy are rejected before body consumption. Limits, I/O failure,
  truncation, and mismatch are reported at the earliest provable point. If a
  response status or stream headers are already committed, a late failure
  terminates the transport and is surfaced locally; it never produces a
  contradictory success envelope.

## Integration and separation

- **DIGEST-300:** Transport digests are distinct from CANON-200 semantic
  document or schema hashes, idempotency fingerprints, cache keys, object-store
  checksums, ETags, and HTTP Message Signatures. They use separate typed fields
  and lifecycle gates even when SHA-256 is reused. Adding, removing, changing,
  or reordering transport digest metadata MUST NOT alter semantic operation
  identity unless the canonical semantic input itself changes.
- **DIGEST-301:** Webhook signature input covers `@method`, `@target-uri`,
  `content-type`, the canonical `content-digest` field value, webhook ID, and
  webhook timestamp. Receivers verify content bytes against that field before
  accepting the signature result. The fixture proves changes to method,
  target, metadata, digest field, or body are detected; issue 48 owns executable
  webhook delivery and key management.
- **DIGEST-302:** Upload references and download metadata identify which
  field, algorithm, variant, byte range, and assembled representation were
  verified. Retries reverify received bytes and never reuse a prior success for
  a different range, coding, redirect target, upload offset, validator, or
  payload. Object checksums MAY be recorded separately but never satisfy a
  required RFC 9530 field by implication.
- **DIGEST-303:** Observability records the profile, field kind, algorithm,
  verification phase, byte count, variant/range identity, and a bounded stable
  outcome code. It MUST NOT log body bytes, raw digest values, signature
  material, credentials, or sensitive metadata. Proxies preserve verified
  fields only when they preserve the exact covered bytes and metadata.
- **DIGEST-304:** Capability advertisement distinguishes header verification,
  request staging, response completion, and trailer verification. An
  implementation MUST advertise only gates it actually enforces across its
  complete transport path. The core reference advertises header verification
  and bounded streaming, and explicitly advertises no trailer support.
