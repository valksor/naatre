# Streaming and incremental delivery

The `core.streaming-1` profile defines subscription sources, logical frames,
incremental response assembly, replay, and transport capability semantics. Its
portable vectors are [streaming.json](../../conformance/v1/streaming.json).
The profile is transport-independent; `sse-post-fetch` is required and
`stream.websocket-1` is optional.

## Subscription establishment and source ownership

- **STR-100:** A subscription operation MUST pass the same structural,
  capability, resource, authentication, planning-authorization, and argument
  validation as a query before its source handler is called. A subscription
  handler MUST be registered as a read effect and MUST return a source that
  yields logical frames in order.
- **STR-101:** A source `Next` operation MUST observe its supplied cancellation
  context. Source `Close` MUST be idempotent, safe to call concurrently with
  `Next`, and unblock a pending `Next`. A host owns the source after successful
  establishment and MUST close it on client cancellation, disconnect,
  consumer abandonment, terminal delivery, validation failure, authentication
  or authorization failure, schema retirement, and server shutdown.
- **STR-102:** A source MUST NOT hold a database transaction across delivery to
  an indefinitely slow consumer. It MUST declare snapshot-stable or
  live-best-effort read consistency. Snapshot resources have a finite lifetime;
  exceeding it produces a terminal error or resumable boundary, never a silent
  consistency downgrade.
- **STR-103:** The schema revision selected at establishment is pinned for the
  logical stream. A source MAY continue on that immutable revision while it is
  retained. If it is retired, delivery stops with a safe terminal
  `SCHEMA_RETIRED` error. A source MUST NOT reinterpret an existing stream
  under a newer schema.

The Go reference `runtime.StreamSource` and `runtime.StreamSourceSession`
implement the ownership boundary. Pull iteration cannot force an upstream
source that violates STR-101 to stop; adapters MUST treat such a source as an
application defect, retain its admission accounting, and bound transport
buffers rather than spawning unbounded reader tasks.
`StreamSource.Next` transfers ownership of its returned frame to the runtime;
the source cannot mutate that frame concurrently or after return. A source
session requires an exact profile and advertisement, an absolute
authentication expiry, a pinned-schema check, a delivery authorization check,
and a process-shared bounded `runtime.StreamCheckExecutor`. Its finite
`MaxDuration`, `MaxEvents`, `MaxBytes`, and `CheckTimeout` apply across the
whole session rather than resetting for each pull.

## Logical frame model

Every frame is a complete UTF-8 JSON object with these common fields:

- `type`: one of `open`, `data`, `patch`, `error`, `complete`, `keepalive`,
  `resume`, or `history-unavailable`;
- `stream`: the logical stream identifier;
- `sequence`: unsigned 64-bit transport delivery sequence, except a
  `keepalive`, which uses no sequence;
- optional `position`: unsigned 64-bit application replay position;
- optional `eventId`: application event identity, independent of sequence and
  replay position;
- type-specific `path`, `data`, `error`, `final`, `cursor`, `recovery`, and
  `schemaRevision` fields.

- **STR-200:** A non-keepalive frame MUST carry a non-zero sequence. Sequence
  starts at one and increases by exactly one. Re-delivery of a byte-equivalent
  frame at an already accepted sequence is an idempotent duplicate. A
  conflicting duplicate, lower out-of-order frame, or gap is an error and MUST
  NOT mutate the assembled response. Implementations retain a finite duplicate
  window; a duplicate older than that window fails safely as out-of-order.
  Byte equivalence is computed from the profile's compact wire encoding; JSON
  object reordering is therefore a conflict, not an idempotent duplicate. A
  nonterminal frame cannot consume sequence `2^64-1`, because the mandatory
  logical terminal must still have a representable next sequence.
- **STR-201:** `position`, when present, is non-zero and strictly monotonic
  within the replay scope. Data and patch frames MAY carry an opaque cursor for
  that position. Sequence, position, and `eventId` are distinct namespaces.
  Keepalives consume none of them.
- **STR-202:** Identifiers, event names, cursor fields, and codes MUST be valid
  UTF-8, bounded, and MUST NOT contain CR, LF, NUL, or control characters.
  Unknown types or fields are invalid. Application values cannot select or
  forge reserved control-frame types.
- **STR-203:** `open` is sequence one, occurs exactly once, and carries the
  pinned schema revision. `complete`, or an `error` with `final: true`, is the
  one required logical terminal outcome. A frame after terminal is invalid.
  EOF or transport closure before terminal is `TRUNCATED_STREAM`, not success.
  `history-unavailable` is not a logical stream terminal: immediately after a
  reconnect `open`, it closes that recovery attempt and reports the advertised
  safe action. Receivers expose it separately from terminal completion.
- **STR-204:** A keepalive has only `type` and `stream`. It proves transport
  activity but carries no application or replay semantics. Heartbeat and idle
  intervals are finite deployment parameters; missing the declared idle
  boundary causes disconnect and the normal resume policy.

## Initial data, patches, and errors

- **STR-300:** The first `data` frame establishes the current response value.
  A `patch` replaces the value at its `path`. Object path segments are strings;
  list path segments are non-negative integers. The parent MUST exist before a
  child patch is applied. A list target MUST already exist; an index equal to
  the current length is not an append operation. Array positions refer to the
  previously delivered stable ordering and MUST NOT silently shift.
- **STR-301:** Applying the same equivalent sequence twice has no effect.
  Conflicting duplicates and patches with missing parents fail without partial
  mutation. A gap in a partial or patch stream requires the declared
  authoritative `refetch` repair unless the profile proves another complete
  repair path.
- **STR-302:** A non-final error MUST carry a non-empty response path and does
  not terminate the stream. Later valid frames remain applicable. An error
  retracts or replaces data only when a later patch explicitly addresses that
  path; recording the error alone does not mutate delivered data. A final error
  MAY omit a path when it applies to the whole stream.
- **STR-303:** Incrementally delivered collection items follow the same stable
  position and page-consistency rules as `collection.page-1`. Application
  mutation patches are outside this profile and belong to the mutation update
  profile.

## Bounds, backpressure, and shutdown

- **STR-400:** Implementations MUST set finite per-frame, data, path-depth,
  assembled-state, retained-error, duplicate-window, buffered-event,
  replay-event, replay-byte, live-queue, authorization-work, filter-work, and
  stream-duration limits. Process-local replay stores MUST additionally bound
  their aggregate stream count, retained bytes, and subscriber count. Each
  session also bounds cumulative event and encoded-byte delivery. Limits apply
  incrementally before allocation or delivery. A limit failure is bounded and
  does not expose protected event identity. Security/schema callbacks that
  outlive their deadline continue holding a slot in one process-shared bounded
  executor until the application callback actually exits.
- **STR-401:** A producer MUST NOT block an application-wide publisher on one
  consumer. Each consumer has a finite queue. Queue exhaustion terminates or
  disconnects that consumer with `SLOW_CONSUMER`, releases its source, and
  preserves only replay state promised by the advertised capability.
- **STR-402:** Client cancellation, disconnect, server drain, and forced
  shutdown propagate cooperative cancellation before source closure. Cleanup
  follows OPS-701. A graceful terminal frame is sent only if the transport is
  still writable; a failed final write is recorded as indeterminate delivery,
  never as client-observed completion.
- **STR-403:** Stream cost includes establishment, every source read, filter and
  authorization evaluation, replay lookup, frame validation, queued bytes, and
  delivery. A replay budget and a live budget are independently enforced.

## Replay and resumption

Replay capability is advertised per source as `none`, `bounded`, or `durable`.
The advertisement states retention policy, maximum replay work, whether an
earliest position can be disclosed, and whether loss requires `restart` or
`refetch`.

The advertisement is the exact JSON object with fields `profileVersion`,
`replay`, `consistency`, `retentionPolicy`, `maxReplayEvents`,
`maxReplayBytes`, `disclosesEarliestPosition`, and `historyRecovery`.
`profileVersion` is `1`; `replay` is `none`, `bounded`, or `durable`;
`consistency` is `snapshot-stable` or `live-best-effort`; and
`historyRecovery` is `restart` or `refetch`. Unknown or omitted fields are
invalid. A `none` source uses retention policy `none`, zero replay limits, and
does not disclose an earliest position. A `bounded` or `durable` source uses a
non-`none` bounded identifier and positive event and byte limits.

- **STR-500:** A replay cursor is an opaque, integrity-protected capability
  bound to logical stream, tenant, principal, authorization revision, schema
  revision, key identifier, format version, and expiry. It MUST NOT be accepted
  across any boundary. Cursor lookup reauthorizes every candidate and stops
  within finite work.
- **STR-501:** Unknown, malformed, unauthorized, expired, evicted, and
  over-budget cursors produce the same external `history-unavailable` outcome
  unless a distinct recovery action is necessary and proven non-disclosing.
  Internal access-controlled telemetry MAY retain the cause. The outcome MUST
  NOT include a newer cursor merely because the requested cursor was absent.
- **STR-502:** Replay-to-live handoff is atomic: register or reserve live
  delivery, capture one source high-water position, replay positions after the
  cursor through that bound, queue events committed after the bound, then
  switch to live delivery. An event exactly at the high-water position is
  replayed once. No step may claim caught-up state before live registration.
  Holding one store lock while snapshotting history and registering the live
  consumer is an equivalent stronger invariant: no event can commit between
  the two operations, and the first later commit enters the registered queue.
- **STR-503:** `resume` carries the accepted opaque cursor and no application
  payload. `history-unavailable` carries exactly one recovery action and no
  cursor or protected identity. A no-replay source always uses the unavailable
  outcome for a supplied cursor. Both control frames are valid only immediately
  after `open`. A resumed receiver preserves its prior assembled snapshot and
  replay position, then requires a fresh `open` and actual `resume` before it
  accepts replay patches.

## Authentication, authorization, schema, and telemetry

- **STR-600:** Authentication lifetime bounds stream lifetime. Before every
  protected data, patch, or error delivery, the runtime re-evaluates current
  authentication and source-event authorization. Expiry terminates with
  `AUTHENTICATION_EXPIRED`; revocation or binding mismatch terminates with the
  same externally safe denial class used by the security profile.
- **STR-601:** Reauthorization occurs before filtering, cursor exposure, and
  delivery. A forbidden event is neither emitted nor represented by an
  observable hole whose identity was not already authorized. Replay uses the
  current decision, not only the decision captured when history was stored.
- **STR-602:** Streaming traces use bounded opaque stream/signal references and
  record establishment, reconnect, replay, reauthorization, backpressure,
  delivery failure, drain, and closure. Raw credentials, cursor values,
  application payloads, tenant-controlled labels, and protected identifiers
  MUST NOT enter telemetry. Redaction rules also apply to error messages.

## Required SSE binding

The core profile owns the bounded incremental SSE codec, logical stream
contract, and portable vectors. Its transport records remain unadvertised
because `core.streaming-1` is transport-independent. The separately executable
`sdk.typescript.adapters-1` evidence owned by #72 advertises the authenticated
Fetch POST SSE binding and, independently, the optional `stream.websocket-1`
subprofile without changing this contract.

- **STR-700:** `sse-post-fetch` uses authenticated Fetch POST with `Accept:
  text/event-stream`, UTF-8, identity content encoding, no intermediary
  transformation, and incremental receive limits. Native `EventSource` is not
  conforming because it cannot carry the operation body and general
  authorization headers.
- **STR-701:** Each logical frame maps to one SSE event. `event` is exactly
  `naatre.<type>`, `data` is the complete compact frame JSON, and `id` is
  present exactly when the frame carries the same replay cursor. Multiple
  `data` lines join with LF. Comments have no logical meaning. Duplicate
  `event` or `id`, mismatched event type/cursor, NUL in `id`, unknown fields,
  or an unterminated event is invalid.
  A bounded decoder MAY normalize CRLF to one line-delimiter unit when charging
  its per-event buffer, provided the raw line and frame limits remain finite and
  are enforced before allocation.
- **STR-702:** Reconnection sends the last successfully accepted cursor as
  `Last-Event-ID` and in the authenticated resume metadata when the request
  envelope/profile supplies both; the two MUST match. Receipt alone does not
  prove availability. The server responds with `resume` followed by replay, or
  `history-unavailable`. A cursor from a parsed but rejected frame is not saved.

## Secure browser subscription handles

The `core.streaming-handles-1` subprofile supplies a browser-safe GET delivery
resource without changing subscription operation identity or making an opaque
identifier sufficient authority. The Go reference is
`runtime.SubscriptionHandleCoordinator`; `transport/http.SubscriptionHandler`
is the router-free HTTP binding.

- **STR-710:** Establishment MUST be an authenticated POST whose body carries
  the complete Naatre subscription request and requested delivery
  capabilities. Structural validation, variable coercion, schema selection,
  planning and event authorization, cost admission, and capability negotiation
  complete before random handle creation or broker establishment. An
  idempotency key is scoped to principal and tenant; reuse with a different
  canonical request fingerprint is a conflict.
- **STR-711:** A stored handle binding MUST include principal, tenant,
  canonical operation identity, canonical coerced-variable identity, schema
  revision, authorization revision, finite limits, delivery profile, creation,
  and expiry. Open, resume, renew, cancel, and observe compare principal and
  tenant and reauthorize the requested action. Schema or authorization revision
  drift requires safe re-establishment; an identifier alone never authorizes.
- **STR-712:** A successful establishment response MUST use status `201` when
  created and `200` when idempotently reused, media type
  `application/vnd.naatre.subscription-handle+json;version=1`, a handle-resource
  `Location`, registered delivery and renewal link relations, typed state,
  delivery endpoint, `text/event-stream` media type, absolute expiry, snapshot,
  reconciliation cursor, and delivery profile. Renewal and observation use
  `200`; successful cancellation uses `204`. Responses are `no-store`, use
  `Referrer-Policy: no-referrer`, expose `Location`, `Link`, `Naatre-Expires`,
  and `Naatre-Delivery-Capabilities` through CORS, and never redirect.
- **STR-713:** Fetch bearer delivery MUST reject ambient cookies and send the
  cursor only in singleton `Last-Event-ID`. Native `EventSource` MUST use a
  same-origin cookie, `Sec-Fetch-Site: same-origin`, no authorization or cursor
  query, and the server-held establishment cursor on its first attachment.
  Cookie-authenticated establishment, renewal, and cancellation MUST require an
  exact HTTPS Origin plus a constant-time double-submit CSRF value. Cross-origin
  Fetch uses an exact CORS allowlist; credentials are never forwarded across a
  redirect because redirects are not emitted or followed by the binding.
- **STR-714:** Query resume cursors MUST be rejected even when a
  `Last-Event-ID` header is also present; the header has exclusive precedence.
  Duplicate header fields, control characters, or conflicting credential paths
  are malformed. Browser history, referrers, generated links, diagnostics,
  telemetry, and access logs MUST NOT contain operation documents, variables,
  bearer values, cursors, protected schema details, or reusable authority.
- **STR-715:** Unknown, unauthorized, expired, evicted, cancelled, and revoked
  handles MUST return the same `409 REESTABLISH_REQUIRED` Problem Details
  envelope. Unavailable cursor history MAY separately return
  `409 REFETCH_REQUIRED`; unsupported negotiated delivery MAY return `406`.
  Access-controlled internal telemetry may retain a bounded cause class, but no
  response claims caught-up state until live registration and replay complete.
- **STR-716:** Snapshot creation MUST capture one source high-water position
  atomically with the reconciliation cursor. Attachment registers live
  delivery, replays only positions after that cursor through its captured
  bound, queues later commits, then switches to live. A mutation committed
  after snapshot generation and before attachment is therefore delivered once,
  without a mutation-sized race window or ambiguous loss outcome.
- **STR-717:** Handle expiry, authentication expiry, revocation, cancellation,
  client abandonment, and server drain MUST close every active source and
  release bounded attachment, snapshot, history, and idempotency resources.
  Collection is batch-bounded. Maximum connection lifetime, reconnect stagger,
  and maximum retry attempts are finite and advertised so fleet rotation forms
  a bounded ramp rather than a synchronized cliff.
- **STR-718:** A broker adapter MUST publish an explicit fidelity report for
  `open`, `data`, `patch`, `error`, `complete`, `keepalive`, `resume`, and
  history loss; replay class and horizon; replay authorization; private scoped
  delivery; loss detection; terminal retention; duplicate classification; and
  bounded retry. A Mercure mapping uses private narrowly scoped topics but
  retains Naatre error, terminal, schema, authorization, replay, and truth
  semantics. Missing fidelity fails explicitly; it is not inferred from
  Mercure wire compatibility or public-by-omission behavior.
- **STR-719:** Broker outage, delayed replay, duplicate delivery, reconnect,
  and terminal-frame loss MUST preserve truthful operation state. EOF before a
  logical terminal is interrupted delivery, never completion; exhausted or
  unavailable history is explicit refetch; retries stop at the advertised
  bound. A production synthetic canary uses isolated non-production data and
  exercises authenticated establishment, delivery, disconnect, resume, and
  terminal observation through real ingress without recording its credential,
  handle, cursor, payload, or protected revisions.

## Optional WebSocket subprofile

- **STR-800:** WebSocket support is optional and MUST be separately advertised
  as `stream.websocket-1`. An implementation that does not advertise it reports
  `unsupported`; it MUST NOT report the optional profile as passed.
- **STR-801:** When advertised, each text message contains exactly one logical
  frame JSON and every logical event has the same meaning as SSE. `stream`
  provides multiplexing identity. WebSocket ping/pong is transport liveness
  and never a logical keepalive or replay position.
- **STR-802:** Client half-close stops new client control messages but does not
  fabricate server completion. Close code 1000 follows an observed logical
  terminal; 1001 denotes drain, 1008 policy/authentication failure, 1009 a
  frame limit, and 1011 an internal failure. Any close before a logical
  terminal remains truncated and follows replay policy.

## Version negotiation

- **STR-900:** A peer advertises the exact streaming profile version and only
  frame types defined by it. Unknown versions, frame types, or transport
  subprofiles fail before source delivery with an unsupported-version outcome.
  Implementations MUST NOT guess compatible semantics or reinterpret a v1
  stream after negotiation.
