# Reliability, idempotency, and retries

This document defines the `core.reliability-1` profile. It governs automatic
retry, protected replay, and the claims an idempotency provider may make. It
does not promise exactly-once delivery or exactly-once external side effects.

## REL-001 — handler policy

Every handler has one of three policies: `non-idempotent`, `idempotent`, or
`conditionally-idempotent`. An omitted policy is `non-idempotent`.
Non-idempotent handlers MUST NOT be automatically retried or protected by a
caller key. Idempotent handlers MAY be repeated when their `retrySafe`
metadata and the request retry policy both allow it. A conditionally
idempotent handler MAY be repeated only inside a valid request or named-group
claim.

The `retryable` flag classifies a failure; it is never permission to repeat a
write. Every selected handler independently participates in the safety check.

## REL-002 — keys and scope

Caller correlation and tracing identifiers are not idempotency keys. A host
MAY accept one request key or keys for named root mutation groups, but MUST NOT
apply both scopes to the same execution. Keys MUST be scoped by an explicit
policy. The default scope combines tenant and principal identity. Raw keys
MUST NOT appear in responses, logs, metrics, traces, or portable fixtures.

Named-group keys are valid only for group-atomic mutations. Each key protects
that group's transaction and exact result independently; it does not imply
that earlier or later groups form one atomic boundary.

## REL-003 — canonical fingerprint

A claim fingerprint MUST use the `idempotency` semantic-hash domain and MUST
cover the principal and tenant, authorization-policy revision, operation and
optional group identity, canonical document, canonical typed variables,
atomicity, and relevant handler/schema semantics. Missing and null remain
distinct. Reusing a scoped key with a different fingerprint fails with
`IDEMPOTENCY_CONFLICT` before a handler runs.

Conditional-write versions, preconditions, and any application semantic not
already represented by those inputs MUST be included by the host's schema or
scope policy.

## REL-004 — store lifecycle and durability

The portable store states are `running`, `completed`, and `indeterminate`.
Creating a running record grants a monotonically fenced lease. An identical
concurrent claimant waits for the active owner or its own context cancellation.
Only the current fence may complete or mark the record indeterminate. A stale
owner fails and cannot replace a newer fact.

A completed record retains an immutable exact outcome until retention expiry.
After expiry a new fenced owner may execute. An expired running lease becomes
`indeterminate`; it MUST NOT silently grant a second write because the first
owner's effect is unknown.

Providers declare `process-local` or `durable`. The in-memory reference store
is process-local: a restart loses coordination and proves no crash-recovery
property. A durable provider may claim recovery only for behavior it persists
atomically. Durable effect deduplication requires integration with the effect's
transaction or downstream fencing/idempotency; a durable result table alone
does not make an external side effect exactly once.

## REL-005 — retry scheduling and budget

Automatic retry requires handler policy, `retrySafe` metadata, a retryable
outcome, and remaining maximum attempts. Applied, partially-applied, and
indeterminate outcomes MUST NOT be retried. Exponential backoff and jitter are
policy inputs; time, sleeping, and randomness MUST be injectable for tests.
A valid Retry-After value is a lower bound on the next delay.

Scheduling MUST observe cancellation and deadlines before the next attempt.
SDK, gateway, worker, and backend layers MUST share one end-to-end attempt
budget. Exhaustion fails with `RETRY_BUDGET_EXHAUSTED`; nesting MUST NOT
multiply the declared attempt count. Hosts MUST retry only replayable buffered
bodies or streams.

## REL-006 — uncertain effects and storage faults

A transport interruption after a write or an unknown transaction commit is
`indeterminate` unless a retained completed result resolves it. Such an
outcome MUST be marked indeterminate and MUST NOT be retried. A crash after an
effect but before result persistence has the same rule.

Claim-store outages fail closed with `IDEMPOTENCY_STORE_UNAVAILABLE` before an
effect. Failure to persist a result after an applied or possibly applied write
reports that code with an `indeterminate` effect state; it MUST NOT publish the
tentative success as protected.

## REL-007 — replay authorization

Before publishing a completed result, the runtime MUST authorize the current
principal against every protected handler again. A revoked or expired decision
returns `UNAUTHORIZED` and no stored data. Matching a key never grants access
to another principal's result. Authorization revision is also fingerprinted,
so revision drift conflicts rather than inheriting an old approval.

## REL-008 — response and telemetry safety

The response extension namespace `org.naatre.reliability` may expose only safe
facts such as attempt count and whether a retained result was used. Telemetry
may additionally identify an operation kind, group name, and store state. It
MUST NOT expose a raw key, fingerprint, fence, principal scope, or retained
data. Application observer failures are contained and cannot change execution.

Portable behavior is fixed by
`conformance/v1/reliability.json`. Implementations MUST cover handler failure,
transport interruption, concurrent duplicates, completed and running expiry,
store outage, crash before result persistence, stale fencing, provider restart,
fingerprint mismatch, replay authorization, shared budgets, cancellation, and
Retry-After.
