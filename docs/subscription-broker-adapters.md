# Streaming broker delivery-source adapters

Issue #105 owns the Go integration slice around the `core.streaming-handles-1`
contract owned by issue #67. The normative frame, cursor, authorization,
subscription-handle, replay, history-loss, and terminal semantics remain in
`spec/v1/streaming.md`, `protocol`, and `runtime`. This package does not define
another protocol, public cursor, broker topic schema, or portable persistence
schema.

## Package and product boundaries

Package `github.com/valksor/naatre/subscriptionbroker` provides three runtime
adapters:

- `PostgreSQLAdapter`, over a `PostgreSQLDriver` whose establishment transaction
  locks the subscription log while it captures the application snapshot and
  high-water position, and whose attachment transaction captures a replay
  bound before registering live notification delivery;
- `RedisStreamsAdapter`, over a `RedisStreamsDriver` whose atomic Lua/Streams
  operation captures snapshot/high-water state and creates the private
  replay-to-live read boundary; and
- `NATSJetStreamAdapter`, over a `NATSJetStreamDriver` whose durable stream and
  consumer barrier retain terminal messages and switch from bounded replay to
  an already-created live consumer.

The existing `runtime.MemorySubscriptionBroker` remains the bounded in-process
reference adapter. All four adapters implement `runtime.SubscriptionBroker`.
The external adapters validate driver capability declarations at startup and
translate only product-neutral `AtomicDriver` records. Product drivers are
application-supplied so applications retain their chosen database, Redis, and
NATS client packages and credentials. The adapter package has no network
client dependency and never accepts a DSN, URL, password, token, broker
subject, SQL text, or Redis key as public data.

An application-supplied driver revision is part of deployment evidence. A
marker method prevents accidentally wiring one product implementation under a
different advertised name. `Capabilities` must explicitly prove atomic
snapshot/position capture, atomic replay/live attachment, bounded replay,
revocation, history-loss detection, slow-consumer limits, terminal retention,
and duplicate classification. Any missing declaration fails construction with
`BROKER_ADAPTER_INVALID_CONFIG`; product identity is never treated as proof.

## Lifecycle, handoff, and limits

Construct the selected product driver and adapter before accepting subscription
establishment. `Establish` gives the driver the validated server-side binding,
snapshot callback, and immutable limits. The driver invokes that callback once
inside the same product exclusion boundary that records the high-water
position. The adapter signs only that numeric position with #67's scoped cursor
codec; product offsets and protected metadata never become public cursors.

`Attach` atomically captures `ReplayHighWater` and registers live delivery
before returning. Its source yields strictly increasing retained positions up
to that bound, one exact caught-up marker, and only then positions from the
already registered live path. The adapter rejects missing, duplicate,
out-of-order, cross-phase, over-limit, or structurally invalid records. It
creates the #67 `open` and `resume` frames and re-sequences driver frames; the
subscription-handle coordinator continues to reauthorize each protected frame.

Defaults bound bindings to 4 KiB, snapshots to 256 KiB, a replay to 1,024
events and 8 MiB, each frame to 1 MiB, and the driver pending queue to 64
events. `Next` propagates cancellation. Concurrent `Close` calls the driver
source without waiting for a blocked `Next`, and a conforming driver must
unblock it. Drivers surface retention gaps, revocation, and slow-consumer
detachment using package sentinels; the adapter maps all other product failures
to bounded public errors without retaining their causes.

Revocation is idempotent at the product boundary and must remove retained
binding/history state and close active sources. Application shutdown owns
calling the #67 coordinator drain/revoke lifecycle before closing its database
or broker clients. The adapter never starts a background goroutine and never
owns an application-supplied client.

## Canary and rollback

`Rollout` selects a candidate adapter by a revision-domain-separated stable
hash and a basis-point threshold. It derives a finite per-reference reconnect
deadline inside the configured stagger window, shortened by identity expiry.
The reference is never retained or emitted. `Rollback` atomically routes every
subsequent selection to the stable adapter; already attached sources continue
under #67's bounded lifetime and replay rules. Machine-readable evidence
contains only aggregate lane and stable-outcome counts.

A production canary should use isolated synthetic data and traverse real
authenticated ingress: establish, authorize, deliver, disconnect, resume, and
observe a terminal frame. Record only the rollout revision, lane, and one of
`caught-up`, `history-lost`, `interrupted`, `revoked`, or `slow-consumer`.
Credentials, handles, cursors, payloads, product addresses, subjects, keys,
schema revisions, and authorization revisions are forbidden evidence fields.

## Supported runtime and unsupported capabilities

The published `streaming.broker-adapters-go-1` profile supports Go 1.27 as a
portable library and the in-process, PostgreSQL, Redis Streams, and NATS
JetStream adapter contracts described above. Offline fake-driver conformance
executes every adapter path. It certifies no PostgreSQL, Redis, NATS Server,
client-library, operating-system, architecture, container, cluster, or managed
service version; production evidence must pin and execute supplied driver and
product revisions separately.

This slice does not support or claim:

- public broker topics/subjects, broker offsets as Naatre cursors, or bearer
  handles in URLs;
- unbounded replay/search, unlimited pending consumers, infinite retries, or
  synchronized fleet reconnects;
- exactly-once external publication, cross-database snapshot transactions, or
  atomic commit with application business mutations;
- automatic product schema/stream creation, migration, partitioning,
  replication, failover, backup, compaction, encryption, credential rotation,
  ACL provisioning, monitoring, or disaster recovery;
- Redis Pub/Sub, Redis consumer groups as a second protocol, NATS Core, Kafka,
  Pulsar, RabbitMQ, Mercure, or another broker mapping;
- multi-region ordering, global clocks, history reconstruction after product
  retention loss, or migration of active cursors between products;
- WebSocket transport, framework bindings, ingress configuration, load
  balancers, service discovery, or process supervision; or
- non-Go native runtimes and any product/platform certification not named in
  an independently executed deployment report.

## Reproducible offline conformance

From the repository root, at the issue #67 and module revisions pinned by
`conformance/v1/subscription-broker-adapters.json`:

```sh
GOWORK=off go test ./subscriptionbroker -count=1
GOWORK=off go test -race ./subscriptionbroker -count=1
GOWORK=off go test ./internal/conformance -run 'TestSubscriptionBrokerAdapterProfile' -count=1
GOWORK=off go vet ./subscriptionbroker ./internal/conformance
```

These commands require no listener or network. The manifest pins the normative
dependency and implementation digests and names every positive, negative,
boundary, cancellation, and resource-limit fixture. Production driver and
canary evidence must add exact product/client revisions without editing this
portable profile.
