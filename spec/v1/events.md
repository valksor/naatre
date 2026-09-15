# Events and outbound webhooks

This document defines the `core.events-1` profile. It maps application-owned
business events to CloudEvents 1.0.2 and defines the portable contracts used by
outbound webhook adapters. Its language-neutral vectors are
[`events.json`](../../conformance/v1/events.json). Durable HTTP delivery is the
separate `#90` integration surface; importing this profile creates no endpoint,
worker, listener, or business event.

## Envelope and compatibility

- **EVENT-001:** A Naatre event MUST use the CloudEvents 1.0.2 structured JSON
  format with wire `specversion` `1.0`. `id`, `source`, `type`, `time`,
  `dataschema`, `datacontenttype`, and `data` map directly to the identically
  named CloudEvents attributes. `subject` remains optional. `source` and
  `dataschema` are absolute URIs, `time` is an RFC 3339 UTC instant, and
  `datacontenttype` is `application/json`. Applications, not the core, choose
  business event types and produce their data.
- **EVENT-002:** Every event MUST carry a non-empty stable `id`. Every retry of
  that event retains the same ID; at-least-once delivery therefore makes
  duplicates normal. Event types are lowercase reverse-domain names ending in
  a positive `.vN` major version. The immutable schema reference is the
  `dataschema` URI plus `naatreschemarevision` and a lowercase
  `naatreschemadigest` of the form `sha256:` followed by 64 hexadecimal digits.
- **EVENT-003:** An application extension name MUST follow the CloudEvents
  lowercase extension grammar, MUST NOT collide with a standard attribute, and
  MUST NOT use the reserved `naatre` prefix. Unknown non-critical extensions
  are preserved or ignored. Naatre extension names and semantics are closed by
  this profile; an unknown `naatre` extension is a version-skew error rather
  than an invitation to guess.
- **EVENT-004:** Ordering is explicitly absent when `naatreorderingkey` and
  `naatresequence` are absent. When present, both MUST occur together and the
  sequence is monotonically increasing only within the tuple of tenant,
  endpoint, event type, and ordering key. Batches preserve array order but do
  not broaden that scope. Retries retain the same sequence. No global ordering
  or ordering across endpoints is promised.
- **EVENT-005:** A receiver MUST authenticate an unknown event before applying
  compatibility policy. An unknown type not explicitly subscribed as required
  is acknowledged and ignored. An explicitly subscribed unknown type or an
  unsupported major version is dead-lettered as `UNSUPPORTED_EVENT_TYPE`.
  Schema changes use TYPE-422 classifications: additive changes are accepted
  by compatible open consumers, while dangerous or breaking changes require an
  explicitly accepted schema revision and otherwise dead-letter as
  `SCHEMA_INCOMPATIBLE`. Unknown fields in application `data` follow its pinned
  event schema, never an envelope-wide guessing rule.

## Endpoint registration and key lifecycle

- **WEBHOOK-001:** Registration, update, rotation, replay, and revocation MUST
  invoke application authorization with the authenticated tenant and endpoint
  owner. A new endpoint remains `pending-verification` until a bounded,
  single-use challenge is returned over the registered channel. Challenge
  tokens, webhook secrets, and payloads are write-only values and MUST NOT be
  returned by discovery, logs, metrics, or dead-letter listings.
- **WEBHOOK-002:** An endpoint MUST use HTTPS without user information,
  fragments, or redirects. Registration resolves every address, rejects
  loopback, private, link-local, multicast, unspecified, documentation,
  benchmark, and other non-public ranges, and pins the approved addresses.
  Every connection and retry re-resolves, revalidates, and connects only to an
  address approved for the same hostname and tenant. Adapters MUST prevent DNS
  rebinding, proxy credential leakage, cross-tenant endpoint reuse, and
  redirect credential forwarding; an HTTP redirect is a terminal failed
  attempt.
- **WEBHOOK-003:** Each secret generation MUST have a unique `keyid`, bounded
  validity interval, and one endpoint audience. Rotation MAY overlap an active
  and retiring generation, but a receiver selects exactly the signed key ID and
  MUST NOT try every overlapping secret. Revocation is checked before every
  attempt and retry. Revoking an endpoint or key prevents new attempts and
  moves already-owned pending records to a safe dead letter without rebinding
  them to a replacement endpoint.

## Exact-byte signatures and replay

- **WEBHOOK-100:** Implementations MUST use the signature profile
  `naatre.webhook.rfc9421-1`, using RFC 9421 label `naatre` and algorithm
  `hmac-sha256`. The ordered covered components are `@method`, `@target-uri`,
  `content-type`, `content-encoding`, `content-digest`, `naatre-webhook-id`,
  `naatre-webhook-timestamp`, and `naatre-webhook-audience`, followed by
  `@signature-params` with `created`, `expires`, `keyid`, and `alg`. The method
  is POST, target URI is the exact registered request target, and content
  encoding is explicitly `identity` or `gzip`. No JSON canonicalization is a
  signature input.
- **WEBHOOK-101:** `Content-Digest` MUST first verify the exact HTTP content
  octets after transfer framing removal and before content-coding decoding, as
  defined by DIGEST-100. The RFC 9421 signature then authenticates the declared
  components and canonical digest field. Proxies that decompress, recompress,
  rewrite the target, or change covered metadata invalidate the signature; they
  cannot reconstruct it without the endpoint secret. A digest match alone is
  never sender authentication.
- **WEBHOOK-102:** The webhook timestamp MUST equal the RFC 9421 `created`
  integer. `expires` is later than `created` and no more than the negotiated
  replay window, whose portable maximum is 300 seconds. Receivers verify the
  key validity, sender, endpoint audience, clock window, content digest, and
  signature before atomically recording `(audience, naatre-webhook-id)` in
  replay storage. A previously recorded identifier is an authenticated
  duplicate: the receiver acknowledges it without repeating the application
  effect. Replay-store failure is fail-closed.

## Delivery, recovery, and operations

- **DELIVERY-001:** Atomic state-plus-event recording MUST use MUT-007
  `RegisterOutbox` with a stable event ID. A committed outbox record retains its
  tenant, endpoint ID and revision, delivery ID, event IDs, and opaque payload
  reference across worker crashes and lease recovery. Delivery is
  at-least-once and never holds the business transaction open for HTTP. A stale
  worker is fenced by adapter-owned leases and MUST NOT transfer a record to a
  different tenant or endpoint revision.
- **DELIVERY-002:** Attempts are one-based and use bounded exponential backoff.
  The fixture pins a one-second initial lower bound, multiplier two,
  eight-second maximum delay, and six total attempts. Jitter MAY increase but
  MUST NOT reduce the lower bound; a valid bounded `Retry-After` may increase
  it. Authentication failures, redirects, permanent client failures,
  revocation, budget exhaustion, and exhausted retries enter a recoverable dead
  letter with stable ownership and a safe reason code. Explicit authorized
  replay creates a new attempt lineage while preserving the event IDs.
- **DELIVERY-003:** One delivery MUST contain either a single structured event with
  `application/cloudevents+json` or an ordered structured batch with
  `application/cloudevents-batch+json`. The portable maxima are 100 events and
  1 MiB of transmitted content. `identity` and deterministic `gzip` are the
  supported content codings. Limits apply before allocation and again after
  decompression; every independently retried batch has one digest, signature,
  delivery ID, and replay decision.
- **DELIVERY-004:** Rate limits MUST be scoped at least by tenant and endpoint,
  bound concurrent attempts and bytes, and reserve capacity for verification
  and revocation. Registration challenges, retry storms, large batches, and
  response bodies are bounded so an endpoint cannot amplify traffic or exhaust
  shared workers. A receiver response is consumed only to a configured limit
  and never becomes event data or a credential source.
- **DELIVERY-005:** Metrics, traces, logs, dead-letter listings, and audit hooks
  include only bounded references, event IDs and types, attempt number,
  outcome, safe failure code, optional HTTP status, timing, and next-attempt
  time. They MUST NOT expose endpoint URLs, payload bytes, payload references,
  digests, signatures, secrets, authorization values, or challenge tokens by
  default. Payload access and dead-letter replay require separate authorization
  hooks and produce an audit fact; routine observability retains only an opaque
  reference.
