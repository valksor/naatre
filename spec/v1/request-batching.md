# Transport batching and request multiplexing

This document defines the `core.transport-batch-1` profile. It groups finite,
independently identifiable Naatre requests for transport without weakening the
single-request protocol, execution, security, resource, mutation, or
reliability contracts. Portable vectors are in
[`request-batching.json`](../../conformance/v1/request-batching.json).

Request-scoped loader batching remains defined by `core.batch-cache-1`; it is
an execution optimization inside one request and is not this transport profile.

## Envelope and validation boundary

- **TBATCH-001:** A batch request is a UTF-8 JSON object containing only
  required `version` and `items` plus optional `policy`. `version` is `"1"`;
  `policy` is `"independent"`, `"fail-fast"`, or `"atomic"` and omission means
  `"independent"`; and `items` is a non-empty array of batch items. Each item
  contains exactly a required `id` and a `request` carrying one PROTO-001
  single-operation envelope. A batch request MUST NOT be decoded as one
  PROTO-001 request or accepted under the single-operation media type.
- **TBATCH-002:** An item `id` is an opaque string of 1..128 Unicode scalar
  values and is unique by exact scalar sequence within the envelope. Missing,
  empty, overlong, non-string, or duplicate identifiers reject the complete
  envelope before any item is admitted. Servers MUST report the same stable
  `INVALID_BATCH_ITEM_ID` problem for every such failure and MUST NOT select a
  duplicate winner. A nested PROTO-001 `id`, when present, must equal its batch
  item `id`; a mismatch is an item validation failure and cannot create a
  second correlation identity.
- **TBATCH-003:** Malformed outer JSON, unknown outer or item members, an empty
  item array, an unsupported policy, and invalid item identifiers are
  envelope-wide failures and start zero handlers. Once that outer structure is
  valid, decoding, validation, authorization, admission, and execution failure
  of one nested request is an item failure. In `independent` and `fail-fast`
  modes an invalid item executes zero handlers while another valid admitted
  item MAY execute. Atomic preflight follows TBATCH-300 instead.

## Aggregate resources and admission

- **TBATCH-100:** A host MUST configure finite maxima for item count, compressed
  bytes, decompressed bytes, decoded tokens and nodes, aggregate planned cost,
  aggregate response bytes, and simultaneously active items. Every item also
  remains subject to every ordinary per-request limit. Nested batching is
  invalid. Limits at intermediary, gateway, worker, and backend layers share
  one end-to-end aggregate budget; nesting or fan-out MUST NOT multiply it.
- **TBATCH-101:** Envelope byte, structure, identifier, and item-count limits
  are checked before item admission. The host then reserves aggregate cost and
  worst-case response capacity monotonically as items are admitted.
  Exhausting an envelope-wide bound rejects the envelope before any handler;
  exhausting a remaining item allocation after valid earlier independent work
  yields a correlated `RESOURCE_EXHAUSTED` item outcome. Atomic mode MUST
  reserve the complete eligible batch before starting a write.
- **TBATCH-102:** `maximumActiveItems` bounds batch-level concurrency in
  addition to process, tenant, principal, and per-request limits. Concurrent
  capacity is a reusable semaphore, not a monotonic reservation: an admitted
  item waits for a released slot within its deadline rather than causing the
  complete envelope to be rejected merely because all slots are active.
  Process or tenant admission overload still follows the HTTP `503 OVERLOADED`
  mapping before that item is admitted. Batch scheduling MAY reorder
  independent item completion only as allowed by
  TBATCH-200. It never reorders sequential selections inside an item, changes
  mutation ordering inside an item, or turns serial work into parallel work.

## Policies, correlation, and cancellation

- **TBATCH-200:** The default `"independent"` policy admits and completes items
  separately. A batch response contains exactly `version` and `items`, and its
  `version` is `"1"`; every
  response item carries the exact request item `id`, one terminal `status` from
  `succeeded`, `rejected`, `failed`, `cancelled`, `timed-out`, `rate-limited`,
  `not-started`, `rolled-back`, or `indeterminate`, and exactly one PROTO-100
  `response` or transport `problem`. A non-streaming batch response contains
  one response item for every request item in request order even when internal
  completion differs.
  Only a separately negotiated streaming batch profile may emit completed
  response items out of request order, and correlation still uses `id`, never
  position.
- **TBATCH-201:** Cancelling or timing out one independent item cancels its
  active work and produces its own `cancelled` or `timed-out` outcome without
  cancelling unrelated items. Opt-in `fail-fast` stops admission of
  not-yet-started items after the first `rejected`, `failed`, `cancelled`,
  `timed-out`, `rate-limited`, or `indeterminate` outcome and marks them
  `not-started`. Pre-handler validation, authorization, admission, and
  rate-limit outcomes trigger this rule just like failures from a started
  handler. `succeeded` and `not-started` are not triggers; `rolled-back` is
  exclusive to atomic mode. Fail-fast does not claim already-running work was
  cancelled. Whole
  transport cancellation cancels all active items, while each resulting effect
  state remains truthful.

## Mutations, idempotency, and scope

- **TBATCH-300:** Atomic batching is optional and contains only mutation items.
  One transaction provider MUST be capable of atomically covering every
  reached write. The host validates the complete envelope and every item,
  authorizes every planned node, admits all resources, resolves one schema and
  authorization revision, verifies idempotency claims, and begins the one
  provider boundary before starting any handler. Unsupported atomic batching,
  a query or subscription item, mixed providers, incompatible transaction
  participation, or any preflight failure rejects the complete batch with zero
  handler starts. Atomic mutations execute in item order and either commit
  together or stop after the first root-failure item. The root item has status
  `failed`; earlier transaction participants have status `rolled-back`; every
  started participant's effect is `rolled-back`; and later items have status
  `not-started`, no effect, and execute zero handlers. If the transaction
  outcome itself cannot be established, every started participant is
  `indeterminate`; later items that never started remain `not-started` with no
  effect. The server MUST NOT guess a root failure.
- **TBATCH-301:** `independent` and `fail-fast` batches MAY contain queries and
  mutations, but no atomicity exists between items and every mutation keeps its
  MUT and REL effect truth. Subscription items are rejected per item because a
  finite HTTP batch has no subscription lifecycle. A response-limit or
  transport failure after a mutation commit MUST preserve `applied` or
  `indeterminate` truth and MUST NOT make that item automatically retryable.
- **TBATCH-400:** Authentication, principal, tenant, transport provenance, and
  trusted connection metadata are established for the envelope and cannot
  vary by item. Item extensions, variables, headers embedded in extension
  data, and documents are untrusted inputs and MUST NOT override that context.
  Each item is still authorized independently, including duplicate semantic
  operations, and all authorization work is charged to the aggregate budget.
- **TBATCH-401:** In independent and fail-fast modes, capability and extension
  negotiation, schema revision pinning, item deadline, and idempotency claim
  are item-scoped. An item deadline may shorten but never extend the envelope
  or server deadline. A transport request identifier and each item identifier
  are correlation, not idempotency keys. An optional atomic-batch claim covers
  the exact ordered item set, canonical requests, shared security scope, schema
  revision, atomicity, and provider; it cannot be combined with item claims.
  Rate limits are enforced both per item and in aggregate and always produce
  correlated, truthful outcomes for admitted items.

## HTTP and multiplexing boundary

- **TBATCH-500:** HTTP batching uses `POST /v1/batch` with
  `application/vnd.naatre.batch+json;version=1` requests and
  `application/vnd.naatre.batch-response+json;version=1` responses. It is
  distinct from HTTP-001 `POST /v1/execute`; GET batch requests are forbidden.
  Before a response body begins, an envelope-wide failure uses RFC 9457 Problem
  Details. An accepted non-streaming batch uses one HTTP status for the carrier
  and reports every operation outcome inside its correlated response item;
  one item failure MUST NOT rewrite another item's status.

  The status and body-kind mapping is exact:

  | Condition | Status | Body | Stable code |
  | --- | ---: | --- | --- |
  | accepted batch, including partial item failures | 200 | Batch | none |
  | malformed JSON, envelope, policy, version, or item identifier | 400 | Problem | the applicable stable batch validation code |
  | unauthenticated before decode/admission | 401 | Problem | `UNAUTHENTICATED` |
  | method not allowed | 405 | Problem | `METHOD_NOT_ALLOWED` |
  | unacceptable response media type | 406 | Problem | `NOT_ACCEPTABLE` |
  | transport request timeout | 408 | Problem | `REQUEST_TIMEOUT` |
  | compressed or decompressed request bytes exceed their limit | 413 | Problem | `REQUEST_TOO_LARGE` |
  | aggregate item-count, decoded-token, or decoded-node bound exhausted before admission | 413 | Problem | `RESOURCE_EXHAUSTED` |
  | aggregate planned-cost quota exhausted before admission | 429 | Problem | `RATE_LIMITED` |
  | aggregate response capacity cannot be reserved before admission | 503 | Problem | `RESOURCE_EXHAUSTED` |
  | unsupported request media type or encoding | 415 | Problem | `UNSUPPORTED_MEDIA_TYPE` or `UNSUPPORTED_CONTENT_ENCODING` |
  | atomic preflight or unsupported atomic mode | 422 | Problem | `ATOMIC_BATCH_PREFLIGHT_FAILED` or `ATOMIC_BATCH_UNSUPPORTED` |
  | rate limited before item admission | 429 | Problem | `RATE_LIMITED` |
  | overload before decode/admission | 503 | Problem | `OVERLOADED` |
  | failure after headers | no new status | terminate | `TRANSPORT_TERMINATED` locally |
- **TBATCH-501:** A truncated or lost non-streaming response is a transport
  failure for the batch representation, not proof that unreceived items did
  not run. Clients retain any completely decoded correlated items but treat
  every missing or partial item as unknown transport delivery; mutation effect
  truth is recovered only through its idempotency or audit contract. A later
  streaming batch profile MUST make frame completion, response-byte limits,
  backpressure, and connection-loss outcomes explicit before it may emit
  out-of-order item results.
- **TBATCH-502:** Sharing an HTTP/2, HTTP/3, WebSocket, broker, or worker
  connection among requests is transport multiplexing, not atomic batching.
  It MUST preserve per-request authentication, authorization, tenant, schema,
  capability, deadline, cancellation, resource, correlation, and effect
  boundaries unless a negotiated batch profile explicitly combines a named
  scope. Cross-item result references are invalid in v1 batching; a future
  capability must use the explicit dependency and type rules owned by issue
  #33 and cannot bypass authorization or budgets. Notification and
  fire-and-forget items are forbidden: every admitted item requires a terminal
  correlated response until a future durable-acknowledgement capability says
  otherwise.
