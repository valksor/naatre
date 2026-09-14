# Asynchronous long-running operations

This document defines the `operations.async-1` profile. It governs durable
acceptance, observation, cancellation, retention, and recovery of work that
must outlive one transport request. It composes with `core.reliability-1`,
`core.streaming-1`, and the structured execution outcome model. It does not
promise exactly-once external effects.

## Handle and binding

- **ASYNC-001 Handle shape:** An operation handle has one opaque stable `id`,
  `state`, monotonically increasing `revision`, optional bounded `progress`,
  optional `result`, business-operation `errors`, distinct projection
  `errors`, `createdAt`, `updatedAt`, optional `startedAt`, optional
  `finishedAt`, and terminal-result `expiresAt`. Timestamps are UTC instants.
  An implementation MUST return an immutable snapshot and MUST NOT reuse an ID
  for different work.
- **ASYNC-002 Durable binding:** The durable record binds the handle to the
  principal, tenant, schema revision, canonical operation identity, and, when
  supplied, idempotency key and fingerprint. Raw keys, fingerprints, worker
  fences, and private principal or tenant references MUST NOT appear in a
  public handle, ETag, Location, progress, error, log, trace, or metric.
- **ASYNC-003 Revisions:** Creation is revision one. Every accepted progress or
  state update atomically increments revision by exactly one. A store MUST
  reject an update whose expected revision is stale. Revisions never reset on
  process restart, worker redelivery, poll, or subscription resume.
- **ASYNC-004 Progress:** Progress is inert JSON with separately configured
  finite encoded-byte and structural-token limits. Its sequence increases by
  exactly one per accepted progress update. Progress is advisory, MUST NOT be
  used as proof of effect or completion, and is redacted under the same policy
  as results.

## States and transitions

- **ASYNC-100 States:** The states are `pending`, `running`, `succeeded`,
  `failed`, `cancelling`, `cancelled`, and `indeterminate`. `succeeded`,
  `failed`, `cancelled`, and `indeterminate` are terminal. `indeterminate`
  means an effect or commit may have occurred but no authoritative result can
  be proven; it is never treated as failed or retried automatically.
- **ASYNC-101 Transition table:** The only state changes are:

  | From | To |
  | --- | --- |
  | `pending` | `running`, `cancelled` |
  | `running` | `succeeded`, `failed`, `cancelling`, `indeterminate` |
  | `cancelling` | `succeeded`, `failed`, `cancelled`, `indeterminate` |

  A compare-and-swap against revision serializes every race. Equal-state
  metadata revisions are not state transitions. A terminal state has no
  outgoing transition; duplicate or late completions return its existing
  snapshot and cannot replace data, errors, timestamps, or expiry.
- **ASYNC-102 Worker ownership:** A worker executes only after atomically
  changing `pending` to `running`. Two workers observing the same pending
  revision race for that transition; only the winner may invoke application
  work. Duplicate delivery after ownership or terminal completion MUST NOT
  invoke the operation again. Lease and distributed-worker adapters are owned
  by issue #89 and MUST preserve this revision rule.
- **ASYNC-103 Unknown outcomes:** Worker loss after execution begins, an unknown
  transaction commit, an expired ownership lease after a possibly applied
  effect, or loss of the authoritative result transitions `running` or
  `cancelling` to `indeterminate`. A late worker completion cannot leave that
  terminal state. Recovery may resolve uncertainty only through a separate,
  application-authoritative reconciliation operation; it is not a state
  regression.
- **ASYNC-104 Results:** `succeeded` stores the immutable business result.
  `failed` stores structured business-operation errors. Result projection or
  field-completion errors are retained separately from the business outcome:
  unavailable projected fields do not turn a succeeded operation into failed
  work and MUST NOT cause the operation to run again.

## Durable acceptance and recovery

- **ASYNC-200 Acceptance barrier:** A server acknowledges asynchronous
  acceptance only after one durable transaction establishes operation
  ownership, the pending handle, its bindings, payload reference, revision,
  and required idempotency index. Starting a goroutine, publishing an
  uncommitted message, or returning HTTP `202` before that transaction commits
  is non-conforming. A connection loss after the barrier does not cancel or
  delete the work.
- **ASYNC-201 Application ownership:** The application supplies a durable store
  with atomic accept, load, bounded pending discovery, revision compare-and-
  swap, and bounded expiry collection operations. It supplies workers that consume opaque accepted
  payloads and produce terminal outcomes. No broker, queue product, delivery
  topology, or polling interval is required by this profile.
- **ASYNC-202 Dispatch recovery:** Pending records are authoritative scheduled
  work. A process restart or crash between acceptance and dispatch MUST leave
  them discoverable through a bounded store query to a restarted dispatcher or worker adapter. Dispatch
  notification may be lost or duplicated; durable pending ownership is not.
  An implementation MUST NOT infer cancellation from the accepting request's
  context after the barrier.
- **ASYNC-203 Idempotent acceptance:** When host policy requires idempotent
  submission, acceptance atomically indexes the binding and caller key. The
  same binding and fingerprint returns the original handle, including its
  current state. Reusing the scoped key with a different fingerprint fails
  with `IDEMPOTENCY_CONFLICT`. An idempotency match never bypasses current
  authorization and never grants another principal the handle.

## Polling, subscription, and authorization

- **ASYNC-300 Polling:** Polling loads one authorized durable snapshot.
  Conditional retrieval compares the request ETag with the snapshot revision;
  a match returns no representation. Polling an active handle returns a finite
  retry lower bound. Clients MUST tolerate skipped revisions and poll until a
  terminal state or their own deadline.
- **ASYNC-301 Subscription and resume:** Subscription is optional and uses
  `core.streaming-1` ownership, bounds, and replay rules. A notification is
  only a revision hint: after every notification or resume, the server loads
  and reauthorizes the same durable record used by polling. Polling and
  subscription therefore expose one logical state and cannot publish divergent
  terminal results. Resume uses the last observed revision and never rolls a
  client backward.
- **ASYNC-302 Authorization:** Every poll, conditional poll, subscription
  establishment, notification delivery, resume, cancellation request, and
  terminal-result retrieval reauthenticates and reauthorizes the current
  principal against the stored principal, tenant, schema revision, operation,
  and current policy. Unknown, denied, expired, and collected handles share one
  generic unavailable response and do not reveal whether protected work
  exists.
- **ASYNC-303 Redaction and bounds:** Progress, results, errors, subscription
  events, and observability facts apply current schema and authorization
  projection before publication. Progress has finite payload, nesting, member,
  update-rate, retained-history, and label-cardinality bounds. Implementations
  MUST NOT expose raw payloads or binding secrets through progress or errors.

## Cancellation races

- **ASYNC-400 Dispositions:** Every cancellation request returns exactly one
  disposition. `requested` means running work was atomically changed to
  `cancelling` but has not acknowledged cancellation. `effective` means pending
  work was prevented from starting or a worker acknowledged cancellation and
  the state is `cancelled`. `too-late` means a terminal outcome already won.
  `unsupported` means policy or the worker cannot attempt cancellation and the
  state is unchanged. `requested` is never reported as proof of effect.
- **ASYNC-401 Completion race:** Cancellation and completion compare the same
  state revision. Completion may legally win from `running` or `cancelling`
  and publish `succeeded`, `failed`, or `indeterminate`; cancellation may win
  and publish `cancelled` only when it is effective. The losing writer reloads
  the winner and reports `too-late` or the existing terminal snapshot. It MUST
  NOT overwrite that snapshot.

## Retention and garbage collection

- **ASYNC-500 Retention:** Terminal completion atomically fixes `finishedAt`
  and `expiresAt` from a configured positive retention policy. Result,
  business errors, projection errors, and final progress remain immutable
  until expiry. Active work is not collected merely because the acceptance
  request or dispatcher ended.
- **ASYNC-501 Expiry:** At or after `expiresAt`, poll, resume, cancellation, and
  result retrieval return the generic unavailable response. Garbage collection
  is bounded, idempotent, and may physically delete only terminal expired
  records and associated idempotency/result data according to declared policy.
  Reusing an expired key follows `core.reliability-1`; collection alone is not
  proof that an earlier external effect did not occur.

## HTTP binding

- **ASYNC-600 Acceptance response:** A successfully persisted asynchronous
  submission returns `202 Accepted`, a `Location` naming the authorization-
  protected poll resource, a strong ETag derived from revision one, and a
  finite `Retry-After`. The response carries the pending handle. `201`, `202`,
  or a handle returned before ASYNC-200 commits is invalid. A duplicate
  idempotent submission returns the same Location and handle; it does not
  create a second resource.
- **ASYNC-601 Poll response:** Authorized retrieval of an unexpired handle
  returns `200 OK` and a strong ETag that changes with every revision. A
  matching `If-None-Match` returns `304 Not Modified` with no body. Active
  states include a valid `Retry-After`; terminal states omit it. Unknown,
  unauthorized, expired, and collected handles use the same non-disclosing
  `404` response. ETags contain no handle or binding secrets.
- **ASYNC-602 Cancellation response:** Cancellation targets an authenticated
  action subordinate to the poll resource. `requested` returns `202` and the
  new `cancelling` snapshot. `effective`, `too-late`, and `unsupported` return
  `200` with the authoritative snapshot and explicit disposition. Transport
  disconnect after the cancellation response does not change the recorded
  outcome.
- **ASYNC-603 Subscription location:** A server advertising subscription
  supplies an authenticated monitor relation from the handle resource and
  negotiates a `core.streaming-1` transport. Absence of that relation means
  subscription is unsupported; polling remains required. A subscription URL
  is not authority and is subject to ASYNC-302 on every use.

The portable contract and race vectors are fixed by
`conformance/v1/async-operations.json`. Concrete durable stores, leases,
dispatch scanners, and queue or worker adapters are owned by issue #89; the
combined implementation matrix is owned by issue #69.
