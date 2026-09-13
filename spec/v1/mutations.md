# Mutations and transaction boundaries

This document defines the `core.mutation-1` profile. Portable vectors are in
[`mutations.json`](../../conformance/v1/mutations.json).

## MUT-001 — deterministic mutation order

Mutation root selections and all mutation descendants execute in document
order. A later selection starts only after the earlier selection has completed,
including output completion. `$parallel` remains an explicit negotiated
exception for non-transactional handlers whose registrations permit parallel
mutation. Transactional operations MUST reject `$parallel` before invoking a
handler.

## MUT-002 — atomicity declaration

An operation's optional `atomicity` member is one of `none`, `operation`, or
`group`; omission means `none`. Queries and subscriptions MUST NOT request
transaction atomicity. `$atomic` is a named, serial selection wrapper with a
`select` array and no response-path segment. It produces no value and therefore
MUST NOT declare `bind`.

- `none` creates no transaction boundary and forbids `$atomic`.
- `operation` begins one transaction before the mutation sequence. Every
  `$atomic` wrapper inside it is a savepoint.
- `group` requires every root selection to be `$atomic`. Each root group gets
  an independent transaction; a nested `$atomic` wrapper is a savepoint.

Group names are unique within an operation and are audit identities, not
response names. A duplicate root name fails planning with
`DUPLICATE_ATOMIC_GROUP`. Child fields merge into the surrounding response
object. A committed earlier group is never undone by a later group failure; the
operation reports `partially-applied` and preserves only the committed response
prefix.

## MUT-003 — provider and participation

Atomicity is transport-independent. A provider declares capabilities during
registry configuration; the registry snapshots them immutably for every plan
created from that registry snapshot. A capability callback panic fails
configuration without changing registry state. The provider begins a boundary
and returns both an opaque transaction and a derived `context.Context`.
Applications attach database handles or other transaction state to that
derived context; global mutable transaction state is forbidden. The handle is
valid only until commit or rollback returns.

Every reached handler declares transaction participation as `none`, `optional`,
or `required`. `required` is invalid without a boundary, and `none` is invalid
inside one. All provider, capability, participation, and group-shape checks
complete before the first write.

## MUT-004 — begin, completion, and rollback

The runtime buffers transaction output. It MUST finish argument validation,
handler execution, produced-value validation, output completion, and in-
transaction outbox hooks before commit. No tentative success is emitted.

A begin failure is `TRANSACTION_BEGIN_FAILED`. A pre-commit failure triggers
rollback with a context detached from request cancellation. Only a confirmed
rollback reports `rolled-back`. Rollback failure is
`TRANSACTION_ROLLBACK_FAILED` and reports `indeterminate`.

## MUT-005 — commit and cancellation

Before commit starts, cancellation is an ordinary pre-commit failure and the
runtime rolls back. Once commit starts, request cancellation MUST NOT rewrite a
provider's result; commit runs with a context detached from request
cancellation. Providers MUST bound their own commit I/O.

Providers return `applied`, `not-applied`, or `unknown`:

- `applied` publishes buffered data and reports `applied`.
- `not-applied` produces `TRANSACTION_COMMIT_FAILED`; a confirmed cleanup
  rollback may report `rolled-back`.
- `unknown` produces `TRANSACTION_COMMIT_UNKNOWN`, withholds tentative data,
  performs no rollback that could conflict with a real commit, and reports
  `indeterminate`.

## MUT-006 — savepoints

Nested atomic groups require provider-declared savepoint support. Unsupported
plans fail with `SAVEPOINT_UNSUPPORTED` before writes. Begin, release, and
rollback faults are respectively `SAVEPOINT_BEGIN_FAILED`,
`SAVEPOINT_RELEASE_FAILED`, and `SAVEPOINT_ROLLBACK_FAILED`. A failed nested
group causes its containing transaction to fail; a confirmed outer rollback is
authoritative for storage effects.

## MUT-007 — outbox, delivery, and external effects

`RegisterOutbox` adds persistence that runs before commit with the transaction
context. Failure is `OUTBOX_PERSIST_FAILED` and rolls back state plus outbox.
`RegisterAfterCommit` adds idempotent application delivery that runs only after
a confirmed commit, with the non-transaction context. Failure is
`AFTER_COMMIT_FAILED`; committed data remains `applied`. Applications MUST use
stable event identities because delivery can be duplicated by recovery.
Registration is valid only while the transaction handler context is active;
using a captured context after the boundary closes returns
`ErrTransactionClosed` and cannot append work to a completed transaction.

`RegisterExternalEffect` records an effect outside transactional storage and
an optional compensation. Ordinary rollback never claims to undo that effect.
A confirmed storage rollback plus every successful compensation reports
`compensated`. A missing compensation produces
`EXTERNAL_EFFECT_UNCOORDINATED`; a failed compensation produces
`COMPENSATION_FAILED`; both report `indeterminate`. No provider may claim one
transaction spans unrelated stores, workers, or remote services.

## MUT-008 — audit lifecycle

The runtime invokes ordered audit hooks for `attempted`, `denied`, `committed`,
`rolled-back`, `compensated`, and `indeterminate` facts. `denied` is emitted
when runtime authorization rejects a mutation before its handler runs. Events
identify the operation and optional root group, MAY include application-
supplied request, operation, and opaque principal references, and include the
terminal safe error code when denied or indeterminate. They never include raw
principal data or mutation payloads. An audit hook receives a cancellation-
detached context and MUST be concurrency-safe. It returns an error when the
fact could not be delivered. Errors and panics in application audit code are
contained, reported through the safe observability failure hook when present,
and never change the provider's already-established transaction truth. Durable
pre-write audit follows OBS-006 rather than relying on this best-effort
callback.

## MUT-009 — produced runtime values

Static request arguments are validated before execution. A value produced by
an earlier call is type-, constraint-, and precondition-validated immediately
before its consumer. Failure follows the active operation/group boundary and
cannot expose tentative data.
