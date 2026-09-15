# Durable asynchronous operation adapters

Issue #89 owns the concrete Go integration slice around the queue-neutral
`operations.async-1` contract owned by issue #47. The normative handle,
binding, state, revision, cancellation, authorization, retention, and HTTP
semantics remain in `spec/v1/async-operations.md` and `runtime`. This slice
does not define another protocol or portable storage schema.

## Package and ownership

Package `github.com/valksor/naatre/asyncoperation` provides:

- `SQLiteStore`, a durable `runtime.AsyncOperationStore` using the pure-Go
  `modernc.org/sqlite` driver;
- private, monotonically increasing lease fences and bounded expiry recovery;
- `WorkerAdapter`, which renews a fence only while one worker invocation is
  active;
- `Dispatcher`, which first marks expired claimed work `indeterminate`, then
  claims one bounded page of durable pending work; and
- `SubmissionAdapter`, which may publish a queue hint after durable acceptance
  without making that hint a second source of truth.

The application owns the database location, filesystem durability and backup
policy, coordinator construction, authorization, worker implementation,
dispatcher schedule, process shutdown, and any queue publisher. `OpenSQLite`
owns and closes the database handle it creates. `NewSQLiteStore` initializes
the adapter tables in an application-owned `database/sql` pool and never closes
that pool.

## Lifecycle and recovery

Construct the store before opening admission. Configure the coordinator with
the same clock and with a terminal retention no greater than the store's
`ResultRetention`. Durable acceptance commits the pending record and optional
idempotency index before a `202` can be returned. A queue notification occurs
only after that barrier; a lost or failed notification leaves the accepted
record discoverable.

The coordinator's pending-to-running compare-and-swap creates the private
lease and increments its fence. Progress and completion retain the current
fence and are rejected after lease expiry. `WorkerAdapter` renews the lease
during execution. Each `Dispatcher.RunOnce` pass atomically changes expired
`running` or `cancelling` records to terminal `indeterminate` before claiming a
finite pending page. A stale worker can then observe the winner but cannot
replace it. Duplicate and late terminal publication is idempotent.

Terminal results, business errors, projection errors, and final progress are
retained as one immutable record until `expiresAt`. Load hides a record at the
expiry boundary, and bounded garbage collection removes only terminal expired
rows and their private idempotency index. Store records contain protected
bindings and payloads and must be treated as application secrets; handles and
public adapter failures never contain them.

## Supported runtime and limits

The published `operations.async-adapters-go-1` profile supports Go 1.27 and
`modernc.org/sqlite` v1.58.0 through `database/sql`. It is a portable Go
library profile and certifies no operating system, architecture, filesystem,
container, or managed SQLite service.

The default limits are 128 bytes per handle identifier, 4 KiB aggregate
binding bytes, 256 KiB payload bytes, 1 MiB encoded retained record, 128
pending records per discovery pass, and 128 records per lease-expiry or result
collection pass. Lease duration, result retention, and heartbeat cadence are
positive application configuration. Calls outside a configured bound fail
before an unbounded query or retained allocation.

Public failures use only the stable codes enumerated in
`conformance/v1/async-operation-adapters.json`. Backend causes, SQL text, data
source names, bindings, payloads, idempotency keys, fingerprints, fences, and
database metadata are not included in public errors.

## Unsupported optional capabilities

This slice does not claim support for:

- PostgreSQL, MySQL, external key-value, object-store, or broker-backed stores;
- a bundled queue product, process-wide scheduler, supervisor, or leader
  election;
- exactly-once external effects, automatic reconciliation of indeterminate
  effects, or lease-based retry of a possibly applied mutation;
- online/versioned schema migration orchestration, replication, backup,
  compaction, encryption-at-rest, or key management;
- multi-database transactions between operation state and external effects;
- remote-worker wire transports, webhooks, SSE/WebSocket subscription,
  framework adapters, or non-Go native runtimes; or
- operating-system, architecture, filesystem, container, cluster, or managed
  service certification.

Applications may compose those capabilities, but must publish separate
evidence for the resulting profile.

## Reproducible offline conformance

From the repository root, with exact issue #47 and module revisions pinned by
`conformance/v1/async-operation-adapters.json`:

```sh
GOWORK=off go test ./asyncoperation -count=1
GOWORK=off go test -race ./asyncoperation -count=1
GOWORK=off go test ./internal/conformance -run 'TestAsyncOperationAdapterProfile|TestAsyncOperationFixtureDefinesDurableRaceSafeContract' -count=1
GOWORK=off go vet ./asyncoperation ./internal/conformance
```

These commands require no listener or network. The profile manifest names the
positive, negative, boundary, cancellation, and resource-limit fixtures and
pins the exact dependency and implementation file digests.
