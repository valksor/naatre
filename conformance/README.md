# Conformance fixtures

Fixtures are language-neutral JSON and are normative only for the versioned
profile named in each file. Every implementation must consume these files
directly and publish machine-readable results that bind the fixture digest,
implementation version, platform, and claimed profile.

Generated fixture changes must be reproducible byte-for-byte. Tests read the
checked-in files rather than duplicating their expected values in Go source.

The `core.scalar.c14n-1` and `core.interop.c14n-1` vectors are verified by both
the Go reference implementation and dependency-free JavaScript implementations
in `independent/scalars.mjs` and `independent/canonical.mjs`. Run the independent
checks with the CI-pinned Node 24.21.0 toolchain:

```sh
node conformance/independent/scalars.mjs
node conformance/independent/canonical.mjs
```

The `core.value-1` vectors cover schema-directed maps, lists, input objects,
one-of activation, enum compatibility, and recursive value limits.

The `core.language-1` vectors cover every core composition tag and expression,
response shaping, collection edge states, result-reference scopes, and the
zero-handler boundary for invalid operations. Their document grammar is the
strict JSON Schema in `spec/v1/language.schema.json`; semantic execution of the
vectors is implemented by the validation and runtime conformance issues. An
invalid vector with `"layer":"decode"` must fail structural decoding; other
invalid vectors decode successfully and fail semantic planning or execution.

`v1/planning.json` fixes the request-neutral description of resolved plans.
The same document prepared with different correlation IDs, variable values,
policy inputs, and authorization identities must retain the same static handler,
type, effect, authorization-policy, and source-pointer description without
retaining any of that per-request state.

`v1/security.json` fixes the `core.security-1` denial shape, static and dynamic
decision semantics, operation-kind distinction, lifecycle and cache-scope
rules, and equivalent decisions for planned, cached, batched, streamed, and
remote placement. Its executable vectors cover aliases, fragments, parallel
groups, nested calls, whole-collection denial, mixed replay history, and cursor
binding across principal, tenant, schema, and authorization revisions. The Go
reference runtime executes the core handler and lifecycle vectors directly. The
`core.batch-cache-1` fixture and runtime suite own request-local cache and batch
placement; #70 owns downstream stream, replay, and remote adapters.

`v1/batching.json` fixes bounded map/parallel windows, authorization-before-
grouping, indexed out-of-order and missing results, duplicate inputs,
width-one/nested/cancelled termination, serial write barriers, complete cache
identity, error/null policy, application ownership, and read-write-read
invalidation. The Go runtime executes each named behavior directly and reports
representative handler-invocation benchmarks.

`v1/observability.json` fixes the `operations.observability-1` event kinds,
mutation-audit stages, bounded outcomes, prohibited sensitive fields, metric
labels and measurements, lifecycle identities, and indexed causal links for
serial, parallel, batched, retried, transactional, and streamed lifecycles.
The Go runtime consumes these sequences directly and proves dependency-free
hook ordering, redaction, cardinality, failure containment, and mutation
correlation. #70 owns broker-backed stream/replay integration and #107 owns
optional exporter and durable-audit adapters.

`v1/schema.json` fixes the `core.schema-1` portable schema authority. It covers
complete, filtered, recursive, extended-trait, evolving, open-union, and
deprecation snapshots. The Go consumer strict-parses each document, imports it
into an immutable type snapshot, re-exports byte-identical canonical JSON and
hashes, proves deny-by-default filtering against an expected document without
hidden-name leaks, and checks stable-ID compatibility classifications. The core
fixture and pure in-process APIs do not enable an HTTP discovery route; #106
owns authenticated transport integration and migration reporting.

The core planner resolves the built-in `include` and `skip` directives, rejects
unregistered directives, and supports registered version-pinned custom
directives through bounded planning, one-shot execution wrappers, and ordered
response annotations. Portable schema and canonical fixtures cover directive
identity, filtering, diffing, and cross-language normalization. It validates
`collection.page-1` admission and collection item scopes. The
reference executor consumes all 18 positive semantic language vectors directly,
including ordered pipelines, explicit collection operations, fragments,
parallel branches, bindings, directives, and edge-state propagation. The core
page vector exercises bounded paging without an input cursor and verifies the
standard opaque-cursor page result.

`v1/collections.json` fixes the `collection.page-1` composite-position
boundaries, scope bindings, safe failure code, live/snapshot modes, and static
cost contract. The Go runtime supplies HMAC-protected versioned cursors,
pre-handler scope verification, key rotation and expiry, stable forward and
backward paging, optional edge metadata, and separately costed `totalCount`.

`v1/mutations.json` fixes the `core.mutation-1` operation and named-group
boundaries, commit uncertainty, rollback and savepoint faults, cancellation,
outbox/after-commit ordering, external-effect compensation, audit stages, and
truthful effect states. The Go runtime lifecycle tests execute every listed
boundary and fault class.

`v1/resources.json` fixes the `core.resources-1` stable codes and location
classes for decoder, planning, runtime, completion, and serialization limits.
Its late-exhaustion vectors model output and error budget exhaustion after a
mutation commit and require truthful `applied` effect metadata plus a bounded
audit/idempotency fallback. Its adversarial cursor vectors bound scans and
storage reads for ancient, random, missing, and unauthorized earliest-history
positions; aggregate replay/live vectors separately cap event count, bytes,
filter and authorization work, queues, memory, and duration. The Go reference
runtime executes core request budgets; #70 owns broker-backed replay/live
execution evidence.

`v1/http.json` fixes the language-neutral `core.http-1` endpoint, method,
media negotiation, singleton-header, status/body, encoding, deadline, cache,
browser, bounded-response, and HTTP-version cases. It is the portable contract
fixture; #73 owns the concrete Go `net/http` adapter, slow-client,
cancellation, graceful-shutdown, and version-specific integration harness.

`v1/persisted.json` fixes the `core.persisted-1` document hashes and identity
equivalence boundaries plus lookup-only, deploy-registration, controlled
automatic-registration, approval/cache, tenant, revocation, expiry, migration,
collision, and safe storage-failure semantics. The Go runtime consumes those
vectors and supplies a race-safe in-memory reference store.

`v1/operations.json` fixes the `operations.lifecycle-1` process states, work
kinds, event vocabulary, public rejection codes, aggregate and queued
accounting, tenant fairness, dependency health, graceful and forced drain,
independent cleanup, atomic revision, connection-rotation, and metadata-safety
boundaries. The Go runtime consumes the fixture directly and benchmarks
admission, overload, drain, and connection rotation. Concrete supervisor,
transport, stream, worker, and durable-operation integration remains owned by
their independently shippable profiles.
