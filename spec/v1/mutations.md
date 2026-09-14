# Mutations and transaction boundaries

This document defines the `core.mutation-1` profile. Portable vectors are in
[`mutations.json`](../../conformance/v1/mutations.json). Conditional update and
read-consistency vectors are in
[`mutation-updates.json`](../../conformance/v1/mutation-updates.json).

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

## MUT-100 — revisions and atomic preconditions

`mutation.update-1` defines opaque application revision tokens. A token is an
equality-only value scoped to the registered mutation, entity, authorization
domain, and representation declared by the application. Clients MUST NOT parse,
order, increment, or synthesize one. A conditional mutation carries its expected
revision as a typed input precondition.

The application provider MUST compare the expected revision and perform the
protected write in one atomic storage boundary. Planning-time lookup, serial
handler execution, and a separate `SELECT` followed by `UPDATE` are not that
boundary. Two concurrent writes using one revision therefore produce at most
one successful conditional commit. A successful commit returns the new
committed revision; failures never return a tentative revision.

## MUT-101 — precondition outcomes and authorization

The stable conditional outcomes are `PRECONDITION_REQUIRED` when a registered
mutation requires but omits an expected revision, `REVISION_CONFLICT` when it
does not match, `ENTITY_NOT_FOUND` for an authorized lookup of a nonexistent
entity, `PRECONDITION_UNSUPPORTED` when the requested guarantee is not declared,
and `UNAUTHORIZED` when entity or field policy denies access. Authorization MUST
run before an entity lookup or revision comparison whose result could disclose
protected existence. A precondition failure invokes no protected write.

Messages and details for these outcomes MUST NOT contain the current value,
current revision, provider key, authorization reason, or existence fact hidden
by policy. Error paths identify only locations in caller-supplied input.

## MUT-102 — closed typed updates

A registered typed-update descriptor is the complete writable allowlist. Each
field has a stable identity, exact public path, schema type, required and
nullable flags, immutability, field authorization, and optional one-of identity.
The four states are distinct: no edit means unchanged; `set(value)` stores a
coerced non-null value; `set(null)` stores null only for a nullable field; and
`remove` makes an optional field absent. Removing a required field, changing an
immutable field, selecting multiple alternatives in one one-of input, or
writing a field not in the descriptor fails before the provider write.

Implementations MUST coerce values through the declared schema type and apply
field authorization. They MUST NOT populate a language object and then reflect
over all of its properties: that turns new SDK or application properties into
mass-assignment targets.

## MUT-103 — JSON Patch and merge patch

RFC 6902 JSON Patch is a separately advertised `mutation.json-patch-1`
capability. Its operations execute in input order and `test` is an additional
write precondition. Every path MUST exactly match a registered writable public
path and pass its type, mutability, and authorization checks; arbitrary pointer
segments are never mapped to provider fields. An unsupported operation or path
fails the entire patch. Repeated JSON Patch paths retain RFC 6902 sequential
meaning and are not rewritten into a typed update.

RFC 7396 JSON Merge Patch is not a portable Naatre update representation. Its
`null` means removal, so it cannot express both `set(null)` and `remove` for a
nullable field while preserving Naatre's missing/null distinction.

## MUT-104 — list and map edits

Typed list edits are `list-append`, `list-insert`, `list-replace`, and
`list-remove`; typed map edits are `map-set` and `map-remove`. Operations execute
in input order against a staged clone. A duplicate typed target is
`DUPLICATE_EDIT`. Positional list edits require an expected entity revision;
an absent or out-of-range position is `LIST_INDEX_CONFLICT`. Map keys are
application data, not field paths, and cannot escape the registered map field.

Every operation, value, target, one-of rule, and authorization decision is
validated before the staged clone is committed. Any failure discards every edit
and reports the failing input operation in its safe error path.

## MUT-105 — read consistency

The default `best-effort` mode promises no application snapshot across
selections or requests. Serial handler execution alone does not create a
consistent database snapshot. Applications may separately advertise `snapshot`
and `read-your-writes` modes. Snapshot reads bind every participating read to
one opaque provider snapshot identity; read-your-writes accepts a committed
mutation revision and MUST observe that write or a later state. A requested mode
that the provider cannot honor fails with `READ_CONSISTENCY_UNSUPPORTED`; it is
never silently downgraded.

Snapshot resources are provider-owned, bounded, and finite. An expired or
wrongly scoped snapshot fails explicitly rather than falling back to live data.

## MUT-106 — cursors, results, and caches

An application revision describes conditional-write state. A cursor snapshot
describes a collection read boundary. They may carry the same provider snapshot
identity only when the application explicitly binds them; neither token is
interchangeable with the other. Mutation results publish only a committed
revision. Normalized caches store that revision with the entity and invalidate
or generation-fence older entries after commit. Rollback leaves the prior
committed revision cacheable. Stream frames and incremental response patches
MUST NOT present tentative mutation data or a tentative revision as committed.
Response-stream patch semantics are not application-write patch semantics.

## MUT-107 — idempotency and HTTP bindings

The expected revision and update representation participate in the REL-003
idempotency fingerprint. After a successful conditional mutation, replay of the
same protected request returns the recorded result and committed revision after
REL-007 reauthorization; it MUST NOT compare the recorded precondition with the
entity's now-newer revision or invoke the write again.

HTTP `If-Match` may bind to the typed expected revision only when that HTTP
binding identifies the same single resource and representation. Multiple
objects, mixed representations, or provider-specific revision domains use
typed preconditions in the operation input instead.

## MUT-108 — multiple resources and external effects

A mutation touching multiple application objects declares every expected
revision in a closed typed precondition set. The #20 transaction provider checks
those preconditions and performs all protected writes inside its application
transaction boundary. Failure is all-or-nothing for that boundary. Remote
services and other external effects use MUT-007 outbox, after-commit, or
compensation semantics; Naatre does not claim distributed ACID.

## MUT-109 — committed-result boundary

Rollback, output-completion failure, cache invalidation failure, disconnect,
and streamed-result truncation do not rewrite transaction truth. A confirmed
rollback preserves the prior committed revision and publishes no tentative
value. A confirmed commit keeps its returned revision even when after-commit
cache invalidation or result delivery fails. Unknown commit remains
`indeterminate` and withholds both tentative data and revision.
