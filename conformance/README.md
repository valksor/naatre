# Conformance fixtures

Fixtures are language-neutral JSON and are normative only for the versioned
profile named in each file. Every implementation must consume these files
directly and publish machine-readable results that bind the fixture digest,
implementation version, platform, and claimed profile.

Generated fixture changes must be reproducible byte-for-byte. Tests read the
checked-in files rather than duplicating their expected values in Go source.

`v1/suite.json` is the authoritative inventory for fixture suite `1.0.0`.
Its version is independent of Go module and implementation releases. The
manifest binds every file by SHA-256, records the complete language matrix,
maps all 510 stable normative clauses and all 110 repository issues to exact
JSON pointers. `v1/roadmap.json` makes unimplemented issue ownership explicit;
those deferred records are never counted as conformance passes. The manifest
also declares downstream ownership for Go harnesses (#78) and complete profile
execution (#69 and #70).

`v1/interactions.json` fixes the authorization/cache, conditional/fragment,
sequential-write/loader, partial-data/generated-type, remote-cancellation, and
transaction/idempotency/outbox process-death boundaries. It also specifies a
supervised-subprocess outcome for uncooperative work. `v1/adversarial.json`
supplies finite budgets and seed-retention metadata for depth, alias, cost,
directive, compression, federation, stream, literal, Unicode, loader, replay,
and rollout inputs. `v1/benchmarks.json` fixes representative workloads and the
environment, latency, allocation, and upstream-call measurements reports must
publish; the Go benchmark implementations remain owned by #78.

Fixture integers outside the interoperable JSON range of
`-9007199254740991..9007199254740991` are decimal strings. Consumers must parse
those strings with an exact-width integer or arbitrary-precision decimal type;
converting them through a binary64 JSON number is non-conforming.

The versioned runner/report protocol and both bindings are documented in
[`RUNNER.md`](RUNNER.md). A dependency-free JavaScript runner and the Go
reference runner consume the same manifest. Run them with:

```sh
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"profiles","command":"discover"}' | node conformance/independent/runner.mjs
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"profiles","command":"discover"}' | go run ./cmd/naatre-conformance
```

The `core.scalar.c14n-1` and `core.interop.c14n-1` vectors are verified by both
the Go reference implementation and dependency-free JavaScript implementations
in `independent/scalars.mjs` and `independent/canonical.mjs`. Run the independent
checks with the CI-pinned Node 24.21.0 toolchain:

```sh
node conformance/independent/scalars.mjs
node conformance/independent/canonical.mjs
node conformance/independent/reliability.mjs
node conformance/independent/verify-generator.mjs
```

The `core.value-1` vectors cover schema-directed maps, lists, input objects,
one-of activation, enum compatibility, and recursive value limits.

`v1/generation.json` pins `sdk.generation-1`, the versioned intermediate model,
the Go reference generator, an implementation-independent JavaScript generator,
and their byte-identical canonical output. The profile fixes semantic persisted
hashes, selected operation result shapes, missing/null preservation, custom
scalar mapping, hostile generator inputs, bounded transport behavior, and
HTTP/SSE/WebSocket cancellation claims. It does not certify an official SDK;
those implementations and the complete matrix remain owned by their extracted
issues and #69.

`v1/go-client.json` pins `sdk.go.client-1`, its exact Go module and source
revision, the shared generator model/output inputs, unary HTTP limits, redirect
credential policy, partial-result behavior, and unsupported higher-level
capabilities. The Go runner executes this profile; the independent JavaScript
runner advertises it as unsupported.

`v1/go-sdk.json` pins `sdk.go.operations-1`, including the typed builder,
selected-result decoder, manifest, retry, batching, pagination, POST-SSE, and
stream lifecycle sources plus the deterministic Go generator and checked-in
artifacts. The Go runner regenerates both artifacts byte-for-byte and executes
the shared generated request/result contract. The independent JavaScript runner
advertises this Go-only profile as unsupported.

`v1/validation.json` fixes the `core.validation-1` typed constraint vocabulary,
exact numeric comparisons, Unicode-scalar and byte lengths, RE2-compatible
full/search patterns, annotation/assertion formats, canonical uniqueness,
bounded native cross-field rules, violation ordering, client/server authority,
output-completion boundary, and strict JSON Schema 2020-12 fidelity. The Go
reference implementation and dependency-free JavaScript consumer execute the
same valid/invalid corpus. CEL evaluation and automatic network reference
resolution are explicitly unsupported.

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

`v1/extensions.json` fixes the `core.extensions-1` descriptor, exact
version/capability negotiation, optional-metadata, deterministic ordering,
conflict, cycle, malicious ownership, and persisted-identity boundaries. The Go
runtime consumes every vector directly; extension behavior is limited to the
closed directive API, while arbitrary hostile in-process code still requires an
external isolation boundary.

`v1/collections.json` fixes the `collection.page-1` composite-position
boundaries, scope bindings, safe failure code, live/snapshot modes, and static
cost contract. It also fixes the `collection.query-1` typed filter AST,
two-valued null/missing truth table, list semantics, ordered sorts, discovery
descriptor, budgets, safe failures, cursor mismatches, and minimum SQLite
provider parity. The Go runtime supplies HMAC-protected versioned cursors,
pre-handler scope verification, key rotation and expiry, stable forward and
backward paging, optional edge metadata, and separately costed `totalCount`.

`v1/collection-query-generation.json` pins the independent JavaScript
generator, its exact `collection.query-1` source fixture, the checked-in
TypeScript filter/sort mapping, SQLite dependency revision, executed parity
surface, and unsupported capability boundary. Its Go harness regenerates the
artifact byte-for-byte, parses it through Node's TypeScript stripping mode,
and executes hostile scalar, operator, collision, and output-drift vectors.

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

`v1/streaming.json` fixes the `core.streaming-1` logical event vocabulary,
required terminal state machine, cursor bindings, replay capability classes,
safe unavailable-history outcome, required Fetch POST SSE capability, and
explicitly unsupported optional WebSocket outcome. Its executable vectors
cover duplicate/gap/patch ordering, response-path errors, final-frame loss,
version skew, split UTF-8 and multiline SSE framing, atomic replay-to-live
handoff, indistinguishable cursor failures, cancellation, disconnect, server
shutdown, authentication expiry, authorization revocation, schema retirement,
and consumer abandonment. The Go reference consumes every vector directly;
#72 owns concrete SSE/WebSocket transport adapters.

`v1/federation.json` fixes the `core.federation-1` service manifests, operator
trust pins, exact composition bytes and federation-domain hash, deterministic
Ed25519 delegation token, composition failures, and coordinator success,
partial-failure, timeout, schema-drift, rolling-upgrade, and multiplicative
fan-out outcomes. The Go bridge executes every vector against the real composer,
public-key verifier, and bounded reference coordinator. Production discovery,
distributed planning, and transport integration remain owned by #109.
