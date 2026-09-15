# Durable webhook delivery and receiver adapters

Issue #90 owns the concrete Go integration slice around the `core.events-1`
contract owned by issue #48. The normative CloudEvents envelope, endpoint and
key lifecycle, exact-byte RFC 9421 signature, retry schedule, delivery states,
limits, compatibility policy, and safe observation fields remain in
`spec/v1/events.md`, `conformance/v1/events.json`, and package `event`. This
slice does not define another protocol or portable event schema.

## Package and ownership

Package `github.com/valksor/naatre/webhook` provides:

- `SQLiteStore`, a durable outbox with private monotonically increasing lease
  fences, bounded recovery, terminal retention, replay receipts, and per-stream
  receiver sequence state;
- `EnqueueTx`, which records application state and exact delivery bytes in one
  commit when both use the caller's SQLite transaction;
- `Dispatcher`, a finite recovery and delivery pass that rechecks endpoint
  revision, endpoint revocation, DNS pins, key generation, and rate/byte
  capacity before every attempt;
- `HTTPConnector`, a direct HTTPS connector that disables proxies and
  redirects, dials only the selected revalidated address, preserves the
  registered hostname for TLS identity, consumes a bounded response body, and
  never uses response content as event data;
- `MemoryLimiter`, a process-local tenant-and-endpoint concurrent-attempt and
  byte limiter; and
- `Receiver`, a listener-neutral request verifier that authenticates exact
  transmitted bytes before decompression or JSON decoding, detects durable
  duplicates, and persists monotonic ordering boundaries.

The application owns endpoint registration authorization and challenges,
endpoint and key registries, secret storage and rotation policy, database
location and backup, dispatcher scheduling, process shutdown, application
event compatibility and effects, dead-letter payload access/replay
authorization, and audit/telemetry exporters. `OpenSQLite` owns and closes its
database handle. `NewSQLiteStore` initializes adapter tables in an
application-owned `database/sql` pool and never closes that pool. `Receiver`
does not create a listener, send an acknowledgement, or invoke business code.

## Commit, delivery, and recovery lifecycle

`EnqueueTx` validates the exact retained representation, event count, event ID
and type correspondence, content coding, and transmitted/decoded byte limits
before insertion. Its `created` result becomes durable only if the caller
commits. Rollback removes both application state and outbox state. `Enqueue`
provides a standalone transaction, but cannot make a separate application
database atomic. An exact duplicate delivery ID is idempotent; conflicting
retained ownership or payload metadata fails without replacing the original.

Each `Dispatcher.RunOnce` first releases a bounded page of expired leases and
then claims at most its configured finite batch. Claim increments the one-based
attempt and private fence. A stale worker cannot complete a newer lease.
Recovery preserves tenant, endpoint ID and revision, delivery ID, event IDs,
payload reference, exact body, and attempt lineage.

Before every connection the dispatcher loads the exact endpoint revision,
requires it to remain active for the same tenant, resolves its hostname again,
applies the `event` package's public-address policy, and rejects every address
outside the registration pins. A literal target is checked as its single
address. Rebinding, redirects, endpoint/key revocation, permanent HTTP
failures, and local resource exhaustion become recoverable dead letters.
Transport failures, cancellation after claim, HTTP 408/425/429, and 5xx
responses use the normative bounded backoff. Attempt six dead-letters as
`WEBHOOK_RETRY_EXHAUSTED`. A bounded valid `Retry-After` may lengthen but never
shorten the normative delay.

Signing happens after endpoint, DNS, key, and limiter checks. It uses exactly
one selected key ID and the existing `event.Sign` implementation; overlapping
generations are never tried in sequence. Key rotation therefore changes only
the attempt signature, not durable delivery or event identity.

The receiver verifies target, singleton covered fields, transmitted-byte
limit, content digest, exact signature, key audience and validity, freshness,
and durable replay status before decoding. An authenticated duplicate returns
`authenticated-duplicate` with no event payload for a second application
effect. Fresh ordered events advance one durable sequence per sender,
audience, event type, and ordering key; an equal or lower fresh sequence fails
as `WEBHOOK_EVENT_REORDERED`. Unordered events do not invent a sequence.

## Supported runtime, limits, and failures

The published `events.webhook-adapters-go-1` profile supports Go 1.27,
`database/sql`, and `modernc.org/sqlite` v1.58.0 as a portable Go library. The
HTTP connector uses Go `net/http` HTTPS with TLS 1.2 or newer. No operating
system, architecture, filesystem, DNS service, CA bundle, network, container,
or managed SQLite service is certified.

The default maxima are 128-byte identifiers, 1 MiB transmitted and decoded
payloads, 100 events per delivery, 64 KiB consumed response bodies, and 100
records per dispatch or recovery pass. Endpoint pins and signature validity
retain the smaller bounds owned by `core.events-1`. The reference retry policy
is six attempts with one-, two-, four-, eight-, and eight-second lower bounds.

Public failures use only the stable codes enumerated in
`conformance/v1/webhook-adapters.json`. Causes, URLs, resolved addresses,
payloads, payload references, response bodies, digests, signatures, keys,
secrets, authorization values, challenge tokens, SQL, data source names, lease
owners, and fences never appear in public errors or dead-letter observations.
`Load` is explicitly a protected application API; `DeadLetters` emits only the
safe `event.DeliveryObservation` shape.

## Unsupported optional capabilities

This slice does not claim support for:

- PostgreSQL, MySQL, external key-value, object-store, or broker-backed outbox,
  replay, or sequence stores;
- cross-database business-state/outbox transactions, exactly-once HTTP or
  application effects, or distributed transaction coordination;
- a bundled queue, scheduler, supervisor, leader election, multi-process or
  distributed tenant/endpoint rate limiting, or automatic dead-letter replay;
- endpoint registration APIs, ownership authorization, challenge delivery,
  endpoint discovery, event production, event subscription management, key
  generation, secret storage, KMS/HSM integration, or rotation scheduling;
- DNSSEC, DNS-over-HTTPS, service discovery, dynamic pin approval, forward or
  authenticated proxies, redirects, proxy credentials, mTLS client identity,
  HTTP/3, non-HTTPS delivery, or content codings other than identity and gzip;
- global ordering, contiguous sequence enforcement, cross-endpoint ordering,
  application event registries, schema migration/compatibility decisions, or
  application-effect transactionality with receiver replay state;
- response-body interpretation, registration challenge handlers, a bundled
  receiver listener/server, framework middleware, metrics/tracing exporters,
  or audit storage;
- online/versioned database schema migration orchestration, replication,
  backup, compaction, encryption at rest, or database key management;
- non-Go native runtimes, browser/edge runtimes, alternate HTTP stacks, or
  operating-system, architecture, filesystem, DNS, CA, network, container,
  cluster, managed-service, framework, or certification claims.

Applications may compose these capabilities only with separate evidence for
the resulting profile.

## Reproducible offline conformance

From the repository root, with the exact issue #48 revision and dependency
digests pinned by `conformance/v1/webhook-adapters.json`:

```sh
GOWORK=off go test ./webhook -count=1
GOWORK=off go test -race ./webhook -count=1
GOWORK=off go test ./internal/conformance -run 'TestWebhookAdapterProfile' -count=1
GOWORK=off go vet ./webhook ./internal/conformance
node conformance/independent/events.mjs
```

These commands require no listener or network. The profile manifest names the
positive, negative, boundary, cancellation, and resource-limit fixtures and
pins exact dependency and implementation file digests.
