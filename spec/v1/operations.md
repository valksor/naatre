# Process admission and lifecycle

The `operations.lifecycle-1` profile defines process-wide resource admission,
health, revision, connection-rotation, and drain behavior. Its portable
inventory is [operations.json](../../conformance/v1/operations.json).

## Configuration and startup

### OPS-101 Validated configuration

A process MUST validate its complete configuration before opening admission.
Limits, cleanup deadlines, drain actions, dependency declarations, connection
lifetimes, and revision identifiers MUST be valid and internally consistent.
Dependency names MUST be unique. A configuration error MUST leave the process
ineligible to start.

Configuration and schema state used by admitted work MUST be immutable. A host
MUST NOT expose partially constructed registries, half-installed revisions, or
mutable caller-owned configuration slices.

### OPS-102 Process states

A process has exactly the externally visible states `starting`, `ready`,
`draining`, `stopped`, and `forced`. `starting` and `ready` are live;
`draining` remains live while bounded shutdown can still complete. `stopped`
and `forced` are neither ready nor live.

## Process-wide admission

### OPS-201 Aggregate accounting

Admission MUST reserve process-wide in-flight work, estimated retained bytes,
result-buffer bytes, active streams, and remote-worker connections before
business execution. Waiting work MUST reserve queue entries and estimated queue
bytes. A zero supplied byte weight MUST be charged as at least one byte.

One operation split into a batch, mapped collection, nested parallel branch,
stream, or remote placement MUST NOT evade aggregate accounting. Adapters MUST
charge the aggregate reservation before creating child work and MUST retain or
transfer that reservation until every owned child exits.

### OPS-202 Partition fairness

Admission MUST enforce finite global, tenant, and principal active and queue
limits. Queue scheduling MUST make progress across eligible tenants; appending
work from one tenant MUST NOT indefinitely overtake already waiting work from
another eligible tenant. One full tenant or principal partition MUST NOT
consume every process queue slot reserved for other partitions.

Partition references are process-local keys. They MUST NOT appear as metric
labels, lifecycle-event fields, health output, or public errors.
Each reference MUST be valid UTF-8 and at most 256 bytes. The reference
implementation domain-hashes fixed-size tenant keys and hashes principal keys
with their tenant, so equal subjects in different tenants never share a quota
and raw references are not retained by queued or active leases.

When a parent capacity is greater than one, the reference implementation
resolves each tenant maximum below the global maximum and each principal
maximum below its tenant maximum. This reserves at least one active and queued
slot for another eligible partition even when an operator supplied an unsafe
equal or larger child limit. A single-slot process necessarily relies on queue
round-robin at release.

### OPS-203 Cancellation and ownership

Cancellation of waiting work MUST atomically remove all of its queue accounting.
Cancellation of admitted work is cooperative and MUST NOT release its active
slot, byte reservation, stream count, remote connection count, or durable
ownership. Those resources are released only when the owned work actually
exits and its lease is released.

A host MUST NOT start replacement work merely because a cancellation signal was
sent to an uncooperative handler. This prevents an unbounded replacement-
goroutine loop.

## Overload and retry behavior

### OPS-301 Rejection boundary

An admission failure MUST occur before any business callback, authorization-
external side effect, remote dispatch, stream establishment, or result-buffer
allocation owned by the rejected operation. The stable code is `RATE_LIMITED`
when a tenant or principal partition is full and `OVERLOADED` when a process-
wide capacity, queue, or dependency-readiness boundary rejects work.
The reference `Run` callback receives a non-releasable admitted-work view;
resource release remains owned by `Run` until that callback actually returns.

The reference HTTP mapping is status 429 for `RATE_LIMITED` and status 503 for
`OVERLOADED`. A rejection SHOULD include a bounded `Retry-After` lower-bound
hint. A client MAY retry only when its operation policy permits retries; it
MUST count the attempt against its shared retry budget and MUST NOT wait beyond
its remaining deadline merely to satisfy the server hint.

## Dependency health

### OPS-401 Readiness and liveness

An unavailable essential dependency MUST fail readiness immediately and stop
new admission. A transient failure MUST NOT fail liveness before the configured
failure grace. Continued essential unavailability at or beyond that grace MUST
fail liveness. Recovery restores readiness and liveness when no other boundary
prevents them.

Non-essential dependency failures affect the unavailable count but do not by
themselves fail readiness or liveness. Health output MUST contain only process
state, readiness, liveness, and bounded unavailable counts. It MUST NOT expose
dependency names or causes, credentials, tenant or principal references,
schema/configuration identifiers, operation names, or protected metadata.

## Revision changes

### OPS-501 Atomic install and rollback

Schema, registry, and configuration changes MUST be installed as one immutable
revision. New admissions observe either the complete old revision or the
complete new revision. Every admitted lease stays pinned to exactly one
revision through execution and cleanup. Rollback is an atomic install of a
previously retained complete revision; it MUST NOT mutate active leases.

Revision changes MUST be rejected after drain begins. Lifecycle telemetry MAY
report that an install or rollback occurred but MUST NOT include revision
identifiers.

## Draining and forced termination

### OPS-601 Admission closure

Beginning drain MUST atomically stop new admission and reject every queued
request with `OVERLOADED`. It MUST stop establishing new streams and remote
connections before applying the accepted-work policy.

### OPS-602 Accepted work policy

Each accepted work kind has an explicit `finish` or `cancel` drain policy.
Queries, generic long-lived work, streams, and remote connections SHOULD receive
cooperative cancellation by default. A mutation already committing MUST be
allowed to finish until forced termination. Durable accepted work MUST retain
ownership until it is durably completed or explicitly handed off.

A subscription closed during drain MUST produce the profile-defined terminal
or resumable outcome while preserving replay ownership. A remote connection
MUST retain its resume/handoff state until the declared transfer completes.
Concrete stream, worker, operation-handle, and ingress integrations define
their additional outcomes in their own profiles.

### OPS-603 Bounded completion

Graceful drain completes only after every accepted lease has been released.
The host MUST impose a finite maximum drain duration. At that boundary it MUST
cancel every remaining lease, enter `forced`, and report a forced outcome
without fabricating resource release or durable handoff.

The external supervisor termination grace MUST be at least the configured
maximum drain duration plus independently bounded cleanup time. When that is
not possible, deployment policy MUST declare abrupt process termination as a
possible forced outcome and MUST rely on durable fencing, leases, or replay to
recover ownership.

## Cleanup and connection rotation

### OPS-701 Independent cleanup contexts

Rollback, lease release, and source closure MUST each receive a fresh finite
cleanup context that does not inherit request cancellation. Failure or timeout
in one cleanup class MUST NOT consume the deadline of another. Callers remain
responsible for cancelling each derived context and for recording a bounded
failure outcome.

### OPS-702 Connection lifetime

Every long-lived connection MUST have a finite maximum lifetime. Identity or
handle expiry MAY shorten but MUST NOT extend it. Routine rotation MUST apply a
deterministic bounded stagger strictly smaller than the maximum lifetime so a
rollout or credential cycle does not synchronize reconnects. Drain stops new
establishment before rotating accepted connections.

## Safe lifecycle events

### OPS-801 Cardinality and failure containment

Lifecycle events use only the stages, states, work kinds, outcomes, public
codes, and aggregate measurements listed by `operations.lifecycle-1`.
Observers MUST NOT receive tenant, principal, dependency, operation, revision,
or configuration references. Observer errors and panics MUST NOT alter
admission, revision, health, drain, or ownership outcomes. Failure reporting
itself MUST be panic-contained and MUST expose only bounded stage and panic
classification.

The reference implementation dispatches observers through a finite ordered
queue. A stalled observer MUST NOT block lifecycle locks or extend drain.
Observation is best-effort after that queue fills; deployments requiring
durability MUST hand events to a non-blocking durable adapter owned by the
observability integration profile.

## Reference boundary

The Go `runtime.ProcessController` is the smallest in-process implementation of
this contract. [The process-hosting guide](../../docs/process-hosting.md) and
[`examples/processhost`](../../examples/processhost) show an executable
`net/http` boundary. Concrete supervisor signals, full transport admission,
stream/worker adapters, and deployment integration are independently shippable
profiles and do not become implemented merely because the core controller is
present.
