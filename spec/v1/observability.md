# Observability and mutation audit

This document defines the `operations.observability-1` profile. Portable
vectors are in [`observability.json`](../../conformance/v1/observability.json).

## OBS-001 — dependency-free hooks

The core runtime exposes dependency-free trace, metric, structured-log, and
mutation-audit hooks. Core packages MUST NOT import an observability SDK.
Backend-specific adapters, including OpenTelemetry exporters, are optional and
MUST preserve this profile's event, redaction, cardinality, and failure rules.

## OBS-002 — lifecycle and causality

Requests, planning, operations, handlers, batches, retries, transactions, and
subscriptions expose stable lifecycle events. Events within one synchronous
lifecycle are emitted in program order. Start and terminal events reuse one
span identity and name their parent. Concurrent branches have no portable
wall-clock total order; implementations MUST preserve branch-local order and
publish explicit causal links to their operation, batch, retry, or stream.
Planning is a standalone pre-execution lifecycle because immutable plans do
not retain request telemetry state; its start and terminal events share an
identity without inventing an unobserved request parent.

Every started synchronous lifecycle emits exactly one terminal `completed`,
`failed`, or `cancelled` event. A transaction instead uses the append-only
audit stages in MUT-008. Sampling and span limits MAY omit best-effort trace or
log events, but MUST NOT rewrite causal links or audit facts.

## OBS-003 — safe trace and log fields

Trace and log events MAY contain an application-supplied request reference,
operation reference, operation name and kind, schema revision, persisted
document hash, opaque principal reference, stable handler identity, duration,
static cost, bounded outcome, and safe error code. They MUST NOT contain raw
variables, arguments, returned values, authorization headers, cookies,
passwords, secrets, API keys, tokens, sessions, principal data, cursors,
topics, or subscription handles.

Operation names, aliases, tenant identifiers, request references, handler
names, and all opaque references are untrusted high-cardinality values. They
MAY be used for correlated traces or structured logs subject to application
retention policy, but MUST NOT become metric labels.

## OBS-004 — bounded metrics

The portable metric dimensions are only event kind, event stage, operation
kind, bounded outcome, and a runtime-defined safe error code. Exporters MUST
map an unknown application error code to `INTERNAL` rather than create a new
label value. Public adapter inputs MUST also normalize unknown stages and
outcomes before invoking any hook. Durations, costs, batch sizes, retry,
connection and replay attempts, active-stream counts, replay events and bytes,
scanned candidates, and drain progress are measurements, never label values.

## OBS-005 — hook failure containment

Trace, metric, log, and mutation-audit hooks are best-effort and MUST be
concurrency-safe. Their returned errors and panics are contained and MAY be
reported through the safe failure hook without including the returned error or
panic payload. A failure hook panic is also contained. No telemetry failure may
change application data, error classification, transaction outcome, or process
liveness, and a committed mutation is never relabelled as rolled back.

## OBS-006 — audit policy boundary

Mutation audit hooks are ordered append-only facts, not a durability claim.
Applications that require an audit record before a write MUST enforce that
policy during admission or persist it transactionally through MUT-007; a
best-effort post-fact hook is insufficient. The hook returns an error so failed
delivery can be reported safely, but that error never vetoes or rewrites the
transaction. Audit events correlate an application-supplied request reference,
operation reference, opaque principal reference, operation name, optional
transaction group, stage, and safe error code. Raw principal data and mutation
payloads are forbidden.

## OBS-007 — stream and replay signals

Stream adapters emit establishments, reconnects, resume outcomes, replay
starts, history loss, refetch requirements, reauthorization, backpressure,
delivery failure, drain progress, and terminal closure. Signals correlate an
opaque logical-stream reference plus bounded connection and replay attempt
counters without exporting a raw handle, cursor, topic, principal, or
operation variables. Adapters MAY supply an opaque signal reference, parent
reference, and remote links for trace/log causality; none becomes a metric
label. The bounded counters and measurements cover connection/replay attempts,
active streams, replay work, scanned candidates, and drain progress. The
vectors distinguish normal reconnect, replay gap, authorization closure,
slow-consumer closure, broker failure, and forced drain. Broker-backed
integration evidence belongs to the downstream streaming profile.
