# Conformance fixtures

Fixtures are language-neutral JSON and are normative only for the versioned
profile named in each file. Every implementation must consume these files
directly and publish machine-readable results that bind the fixture digest,
implementation version, platform, and claimed profile.

Generated fixture changes must be reproducible byte-for-byte. Tests read the
checked-in files rather than duplicating their expected values in Go source.

`v1/jtd.json` pins `schema.jtd-1` to RFC 8927 and carries positive and
negative import/export fixtures for every supported form, bounded hostile
inputs, deterministic round-trip evidence, and the independent canonical
source binding used to compare JTD with JSON Schema fidelity reports.

`v1/http-digest.json` pins the `core.http.digest-1` RFC 9530 profile with exact
identity, gzip, range, and transfer-framed bytes; strict negotiation and failure
vectors; phase outcomes; webhook signature inputs; and explicit trailer
capability. The Go reference codec and dependency-free JavaScript verifier both
consume it directly.
`v1/large-values.json` pins the `core.large-value-1` boundary, capability,
identity, cleanup, range/resume, conditional download, redirect, DNS-rebinding,
and malicious metadata vectors. The Go reference package consumes all
transport and egress cases without contacting a network endpoint.
`v1/events.json` pins the CloudEvents 1.0.2 envelope and
`naatre.webhook.rfc9421-1` exact-byte signature profile. The Go reference
receiver and dependency-free JavaScript receiver both verify whitespace, gzip,
method and target binding, freshness, exact key selection, duplicate replay,
retry, batch, version-skew, recovery, revocation, and redaction vectors.
`v1/generator-plugin-host.json` binds the public Go host, the exact generator
model and toolchain revisions, and a dependency-free third-party JavaScript
fixture. The `sdk.generator-plugin-host-1` profile executes discovery,
hostile-source, output-boundary, cancellation, and resource-limit cases; its
support and unsupported-capability boundaries are documented in
[`docs/generator-plugin-host.md`](../docs/generator-plugin-host.md).

`v1/suite.json` is the authoritative inventory for fixture suite `1.0.0`.
Its version is independent of Go module and implementation releases. The
manifest binds every file by SHA-256, records the complete language matrix,
maps every stable normative clause and all 110 repository issues to exact
JSON pointers. `v1/roadmap.json` makes unimplemented issue ownership explicit;
those deferred records are never counted as conformance passes. The manifest
also declares downstream ownership for Go harnesses (#78) and complete profile
execution (#69 and #70).

`v1/profiles.json` defines the versioned certification profiles, their exact
normative clauses and fixture digests, eligible evidence roles and paths, and
stable-release requirements. `v1/compatibility.json` is the public compatibility
matrix; unevidenced rows remain planned until #69 executes and publishes them.
See [PROFILES.md](PROFILES.md) for claim, skip, version-skew, report, and
publication rules.

`v1/governance.json` defines `governance.release-1`: independent component
versions, evidence-gated support rows and conformance claims, compatibility and
reserved-name rules, security embargoes, reproducible supply-chain evidence,
revocation, and exactly-once roadmap coverage. Release manifests conform to
[`releases/release-manifest.schema.json`](../releases/release-manifest.schema.json).

`v1/remote-workers.json` fixes `worker.remote-1` registration, transport,
identity, retry, process-death, cancellation, overload, reference, backpressure,
and support-matrix boundaries. The dependency-free JavaScript stdio fixture
and Go reference gateway execute the same schema-defined success and typed
error cases. This is remote-worker evidence only; production HTTP/2 integration
belongs to #88 and the complete matrix belongs to #69.

`v1/interactions.json` fixes the authorization/cache, conditional/fragment,
sequential-write/loader, partial-data/generated-type, remote-cancellation, and
transaction/idempotency/outbox process-death boundaries. It also specifies a
supervised-subprocess outcome for uncooperative work. `v1/adversarial.json`
supplies finite budgets and seed-retention metadata for depth, alias, cost,
directive, compression, federation, stream, literal, Unicode, loader, replay,
and rollout inputs. `v1/benchmarks.json` fixes representative workloads and the
environment, latency, allocation, and upstream-call measurements reports must
publish. `v1/go-quality.json` binds the Go fuzz, repeated-race, owned-resource
leak, deterministic fault-injection, and benchmark execution surface to those
authoritative fixtures. See [`docs/go-quality-harness.md`](../docs/go-quality-harness.md)
for reproducible commands and explicit unsupported capabilities.

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
node conformance/independent/events.mjs
node conformance/independent/profiles.mjs
node conformance/independent/verify-generator.mjs
```

`v1/mutation-updates.json` owns the language-neutral `mutation.update-1`
matrix: missing/null/removal, exact patch paths, optimistic concurrency,
idempotency replay ordering, read consistency, and committed-revision
boundaries. Each SDK records its own implementation evidence; issue #69 owns
the complete cross-language matrix.

`v1/normalized-cache.json` and `v1/normalized-cache.schema.json` publish the
language-neutral `sdk.normalized-cache-1` identity, scope, field-state,
freshness, merge, invalidation, and optimistic-reconciliation contract. The Go
reference package consumes these semantics without changing the executor or
the raw-response client path. Each SDK owns its implementation evidence; issue
#69 owns the complete advertised-language matrix.

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

`v1/go-http.json` pins the executable `core.http-1` Go adapter profile to the
normative HTTP fixture, specification, module revision, handler sources, and
tests. The Go runner executes media/status, bounded gzip, cache-safe defaults,
CORS, redirect denial, redaction, and HTTP/1.1 through HTTP/3 semantic parity;
the recorded package and race commands reproduce the slow-client, disconnect,
deadline, and shutdown cancellation fixtures. Listener TLS and QUIC setup are
host-owned and are not implied by this handler-level profile.

`v1/go-sdk.json` pins `sdk.go.operations-1`, including the typed builder,
selected-result decoder, manifest, retry, batching, pagination, POST-SSE, and
stream lifecycle sources plus the deterministic Go generator and checked-in
artifacts. The Go runner regenerates both artifacts byte-for-byte and executes
the shared generated request/result contract. The independent JavaScript runner
advertises this Go-only profile as unsupported.

`v1/jvm-sdk.json` pins `sdk.jvm.core-1`: one deterministic Java/Kotlin binding,
explicit Java and Kotlin nullability, lossless numeric/time/UUID/byte wrappers,
selected presence and partial-error states, open variants, cancellable future,
blocking, coroutine and Flow views, terminal/truncation handling, and the JDK
17/21/25 execution reports. Android and concrete HTTP, SSE, and WebSocket
adapters are separately advertised rather than inferred from the core.
`v1/jvm-adapters.json` pins issue #82's `sdk.jvm.adapters-1` composite evidence:
the server-only bounded unary `java.net.http` adapter and the distinct Android
API-26 D8 compatibility profile. Authenticated POST-SSE, WebSocket, compression,
automatic retry/auth refresh, concrete Android networking/lifecycle bindings,
and device/emulator certification remain explicitly unsupported; #69 owns the
combined language/runtime/transport matrix.

`v1/typescript-sdk.json` pins `sdk.typescript.core-1`: the dependency-free
plain-JavaScript runtime, exact scalar codecs, selected result states,
prototype-safe decoding, persisted request/manifest behavior, deterministic
TypeScript generator, checked artifacts, exact TypeScript compiler version,
and strict compilation command. Transport and runtime-matrix evidence remains
owned by `sdk.typescript.adapters-1` in issue #75.

`v1/php-sdk.json` pins `sdk.php.core-1`: lossless scalar and map wrappers,
selected-result presence states, serializer-neutral attributes, deterministic
PHP generation, canonical persisted requests, the deadline-only PSR-18 unary
profile, and exact stream-resource ownership. Symfony, Laravel, active-abort,
SSE, and WebSocket adapters remain explicitly unsupported here and are owned by
issue #79.
`v1/php-adapters.json` pins `sdk.php.adapters-1` to the exact PHP core fixture
and PSR interface revisions. Its listener-free FPM and persistent-worker models
exercise request isolation, deadline-only cancellation, stable redacted
failures, missing/null and numeric-map fidelity, and bounded response cleanup
through the optional Symfony and Laravel PSR bridge factories. Native framework
HTTP APIs, native worker certification, active abort, SSE, WebSocket, and the
complete framework/runtime matrix remain explicitly unsupported.
`v1/dotnet-sdk.json` pins `sdk.dotnet.core-1`: deterministic C# selected-result
bindings, nullable `net8.0` and `net10.0` builds, an F# consumer build, explicit
presence and open-variant shapes, lossless extended scalars, persisted request
bytes, bounded unary HTTP and POST-SSE cancellation, terminal/truncation
handling, redirect credential stripping, and non-replay of mutations. The
independent runner executes the .NET verifier when both SDK lines are present.
WebSocket, ASP.NET dependency injection, trimming/AOT, and broader runtime
certification remain explicitly unsupported and owned by issue #85.
`v1/dotnet-adapters.json` pins `sdk.dotnet.adapters-1`: a separately versioned
issue #85 C# facade and F# discriminated-union/`Async` surface over the exact
issue #41 core revision, bounded `HttpClient` execution, sanitized stable
public failures, active cancellation, explicit handler ownership, and the
optional Microsoft.Extensions.Http dependency-injection helper. Its verifier
uses only in-memory message handlers, builds and runs both `net8.0` and
`net10.0`, and confirms that trimming, NativeAOT, WebSocket, mobile, browser,
server-binding, and broader framework support remain unclaimed.
`v1/ruby-sdk.json` pins `sdk.ruby.core-1`: immutable generated Ruby operation
and result shapes, RBS metadata, string/symbol key normalization, lossless
numeric and temporal scalar adapters, partial-completion states, strict bounded
decoding, persisted request identity, stream terminal validation, and a
reproducible gem build. Its runtime matrix links the checked CRuby report;
transport and integration capabilities remain a separate profile.
`v1/ruby-adapters.json` pins `sdk.ruby.adapters-1`: immutable framework-neutral
requests, bounded unary and authenticated POST-SSE response ownership,
redirect credential policy, cooperative cancellation and enumerator cleanup,
thread/fiber/process isolation, optional Faraday and Rails contract surfaces,
the exact issue #44 dependency revision, and every unsupported runtime and
optional capability. Its verifier is listener- and network-free. Issue #69
owns the combined official matrix.

`v1/typescript-adapters.json` pins `sdk.typescript.adapters-1`: Fetch unary and
POST-SSE adapters, the separately advertised optional `stream.websocket-1`
adapter, strict ESM subpath exports, cancellation and body/socket cleanup,
redirect credential policy, decompression and frame limits, and the exact Node,
Bun, and Deno revisions executed by the portable runtime matrix. The pinned
Chrome headless and workerd harnesses remain explicitly unclaimed until they
execute against the current evidence revision. Issue #75 owns the general
TypeScript adapter/package surface; issue #72 owns the concrete streaming
transport slice and its dependency on issue #23. The independent runner
rechecks source and dependency digests, Node behavior, package-export
boundaries, and exact published transport results; the recorded commands
reproduce the other selected runtimes with ephemeral loopback ports.

`v1/python-sdk.json` pins `sdk.python.core-1`: dependency-free Python 3.11-3.14
transport protocols, identical sync/async canonical behavior, frozen generated
dataclasses, exact Decimal/arbitrary-integer/timestamp/bytes mappings,
missing/null preservation, strict JSON, persisted hashes, pagination, bounded
retry, SSE cleanup, `py.typed`, and reproducible package metadata. Concrete
async HTTP and optional Pydantic integration are the separate
`sdk.python.adapters-1` profile; ASGI server handling remains owned by #59 and
the complete official SDK matrix by #69.

`v1/python-sdk-adapters.json` pins `sdk.python.adapters-1`: stdlib unary HTTP,
the bounded thread-backed asyncio bridge, cancellation/thread ownership,
per-request context isolation, stable redacted failures, response and executor
limits, and the optional strict Pydantic bridge. It records the exact #38 core
commit and fixture digest plus the executed CPython, Pydantic, and
pydantic-core revisions. Native async sockets, concrete streams, other event
loops, and unexecuted runtime/platform combinations remain explicitly
unclaimed.
`v1/rust-sdk.json` pins `sdk.rust.core-1`: the Rust 1.85 minimum version and
feature matrix, serde bindings, exact scalar wrappers, persisted manifest,
runtime-neutral transport ownership, bounded pagination and fallible streams,
drop cancellation, and checked generator artifacts. The independent runner
executes its Rust conformance verifier; the complete official SDK matrix remains
owned by #69.
`v1/rust-async-adapters.json` pins `sdk.rust.adapters-1`: the additive `tokio`
feature, task-local unary and byte-stream adapters, stable redacted failures,
drop-based I/O ownership release, inherited response/frame limits, exact #39
dependency revision, and the complete unsupported transport/runtime boundary.
The verifier runs without listeners or network access. Concrete network and
framework transports are not claimed.
`v1/swift-sdk.json` pins `sdk.swift.core-1`: the Swift Package Manager core,
lossless canonical scalar wrappers, explicit input/selected presence states,
structured partial errors and open variants, deterministic generated Codable
bindings, persisted request hashes, bounded async transport contracts, active
task/sequence cancellation, stream truncation, and redirect credential policy.
Its platform entries link the executed macOS arm64 report and explicitly leave
URLSession/SSE/WebSocket and device/simulator adapter evidence to #86 and the
complete official matrix to #69.
`v1/swift-apple-adapters.json` pins `sdk.swift.apple-1` to the exact #42 core
revision. Its independent verifier executes HTTPS URLSession request shaping,
stable redacted failures, bounded unary and SSE reads, 307/308 credential
policy, concrete task and stream cancellation, lifecycle cancellation, and the
shared Codable/scalar/time vectors without a listener. The macOS report is
executed; iOS and the other declared Apple platforms remain unclaimed until a
platform report runs the same exact vector entrypoint. WebSocket and every
other optional capability are listed explicitly in the fixture.
`v1/dart-sdk.json` pins `sdk.dart.core-1` and the issue #84
`sdk.dart.adapters-1` slice: null-safe generated variables and
selected-result shapes, explicit missing/null/pending states, open variants,
lossless extended numeric scalars, strict bounded JSON, persisted request
identity, cancellation-aware transport contracts, pagination, SSE terminal
validation, isolate-aware work dispatch, and byte-identical VM/AOT/dart2js
canonical output. Its report certifies Dart 3.13.3 core execution; Flutter and
device-matrix execution remains uncertified, while the adapter report executes
the VM, AOT, and dart2js conditional profiles. HTTP POST, authenticated
POST-SSE, and optional WebSocket adapters are implemented with the explicit
browser limits recorded in the fixture; issue #69 owns the combined official
matrix.

`v1/validation.json` fixes the `core.validation-1` typed constraint vocabulary,
exact numeric comparisons, Unicode-scalar and byte lengths, RE2-compatible
full/search patterns, annotation/assertion formats, canonical uniqueness,
bounded native cross-field rules, violation ordering, client/server authority,
output-completion boundary, and strict JSON Schema 2020-12 fidelity. The Go
reference implementation and dependency-free JavaScript consumer execute the
same valid/invalid corpus. CEL evaluation and automatic network reference
resolution are explicitly unsupported.

`v1/adapters.json` fixes `core.adapters-1`: separate schema and runtime
directions, pinned OpenAPI/GraphQL/OpenRPC/protobuf/gRPC/Connect versions, the
four fidelity classifications, explicit application policy and operation
approval, deterministic bounded projections, constrained egress and credential
forwarding, safe semantic differences, limited round-trip claims, and one
language-neutral existing-service example. The Go `interopadapter` package is
the smallest runtime-consume reference path; #92, #93, #95, and #98 own full
protocol integrations, while #69 owns multi-SDK execution.

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

`v1/plan-cache.json` pins the Go-only `runtime.go.plan-cache-1` implementation
to the exact core planning fixture, Go module metadata, source, tests, and
profile documentation. Its executable runner checks request-state isolation,
revision invalidation, active-plan eviction, effect-barrier provenance,
redacted stable failures, cancellation, and the finite capacity boundary. It
does not extend `core.language-1` or claim distributed caching, user-defined
optimizers, native compilation, framework integration, or unexecuted runtime
certification.

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

`v1/observability-integrations.json` fixes the optional Go OpenTelemetry and
durable-audit integration boundary owned by #107. It pins exact dependency
revisions, supported and unsupported capabilities, retained-state and audit
limits, reproducible commands, and positive, negative, boundary,
cancellation, and resource-limit test evidence. It adds no normative event or
storage schema; `operations.observability-1` remains authoritative.
`v1/async-operations.json` fixes the `operations.async-1` handle states,
monotonic transitions, cancellation dispositions, durable acceptance,
authorization binding, conditional polling, optional revision subscription,
retention, and HTTP metadata. Its race vectors cover acceptance-to-dispatch
crash recovery, worker crash, duplicate delivery, cancellation/completion,
expiry, unknown commit outcome, and cross-principal result isolation. The Go
runtime provides the queue-neutral coordinator; issue #89 owns concrete durable
stores, leases, dispatch scanners, and worker adapters.
`v1/async-operation-adapters.json` fixes the concrete Go/SQLite integration
boundary owned by #89. It pins the exact #47 contract revision, dependency and
implementation digests, finite store and recovery limits, stable redacted
failures, supported and unsupported capabilities, reproducible offline
commands, and positive, negative, boundary, cancellation, and resource-limit
fixtures. It composes `operations.async-1` without redefining its protocol or
state schema.
`v1/operations.json` fixes the `operations.lifecycle-1` state machine owned by
#57 and the #96 Go integration evidence. It pins the exact dependency revision,
runtime/host/runner source digests, supported runtime boundary, complete
unsupported-capability inventory, and overload, dependency-failure, rolling-
restart, reconnect-storm, drain-deadline, and forced-kill transitions. The Go
runner publishes the pinned profile result; the independent Node supervisor
executes the POSIX forced-kill boundary.

`v1/schema.json` fixes the `core.schema-1` portable schema authority. It covers
complete, filtered, recursive, extended-trait, evolving, open-union, and
deprecation snapshots. The Go consumer strict-parses each document, imports it
into an immutable type snapshot, re-exports byte-identical canonical JSON and
hashes, proves deny-by-default filtering against an expected document without
hidden-name leaks, and checks stable-ID compatibility classifications. The core
fixture and pure in-process APIs do not enable an HTTP discovery route; #106
owns authenticated transport integration and migration reporting.

`v1/schema-discovery.json` fixes the opt-in `schema.discovery.http-1` Go
transport profile, its stable public failures, finite rolling-revision and
response limits, executed positive/negative/boundary/cancellation/resource
cases, exact `core.schema-1`/`core.http-1`/Go implementation revisions, and the
complete unsupported-capability boundary. It composes the filtered schema and
diff authority above rather than redefining either contract.

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

`v1/request-batching.json` fixes the distinct `core.transport-batch-1`
envelope, aggregate and per-item budgets, required unique item identifiers,
out-of-order internal completion correlation, independent cancellation,
fail-fast admission, optional single-provider atomic mutations, trusted scope
boundaries, partial failures, timeouts, rate limits, and response-loss truth.
HTTP batching remains finite and separate from streaming or WebSocket
multiplexing; notifications and cross-item result references are unsupported.

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

`tooling/playground-mock.json` records the independently deliverable
`naatre.playground-mock-1` implementation evidence. It pins the exact issue #55
tooling revision and fixture digest, Go module and schema revisions, closed
capability boundary, public failures, resource ceilings, and the positive,
negative, boundary, cancellation, and resource-limit cases executed offline.

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

The same fixture now pins `core.streaming-handles-1`: authenticated and
idempotent establishment, complete server-side authority bindings, typed HTTP
metadata, Fetch bearer and same-origin cookie EventSource paths, exclusive
header cursor precedence, indistinguishable unavailable-handle outcomes,
snapshot-to-replay handoff, lifecycle cleanup, adapter fidelity, broker failure
truth, bounded reconnect ramps, and the privacy boundary for a synthetic real-
ingress canary. The in-process adapter is executable; production Mercure and
rollout integration remain owned by #105 and the combined matrix by #69.

`v1/federation.json` fixes the `core.federation-1` service manifests, operator
trust pins, exact composition bytes and federation-domain hash, deterministic
Ed25519 delegation token, composition failures, and coordinator success,
partial-failure, timeout, schema-drift, rolling-upgrade, and multiplicative
fan-out outcomes. The Go bridge executes every vector against the real composer,
public-key verifier, and bounded reference coordinator. Production discovery,
distributed planning, and transport integration are outside that core fixture.

`v1/federation-coordinator.json` fixes the separately executable Go coordinator
evidence without redefining `core.federation-1`. It pins the exact #24 commit,
core fixture and module digests, deterministic entity dependency planning,
bounded fan-out and retry multiplication, cancellation/timeout and rolling
upgrade evidence, operator-owned endpoint bindings, trace propagation, the
runtime/platform boundary, and every unsupported optional capability. The Go
harness consumes its planning vectors directly.

`v1/openapi-adapter.json` pins `adapter.openapi-1` to OpenAPI 3.2.0, the Draft
2020-12 JSON Schema dialect, the exact issue #56 dependency revisions and file
digests, and the Go import, export, runtime-consume, cancellation, security,
and resource-limit evidence. `openapiadapter/README.md` publishes the complete
closed supported subset and its unsupported optional capabilities.
