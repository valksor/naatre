# Naatre

Naatre is a new, language-neutral specification and Go reference
implementation for typed, deeply composable API operations. Its v1 language,
wire model, conformance fixtures, and compatibility profiles are defined by
this repository; Go is the reference runtime, not the source of wire truth.

The project is inspired by Deepr, GraphQL, JSON Schema, Smithy, Connect, and
other HTTP/RPC systems. Naatre does not claim compatibility with Deepr or copy
its version history.

## Status

Naatre targets a single v1 implementation milestone covering the complete
specification, runtime, transport, ecosystem, interoperability, operations, and
conformance roadmap. Capabilities not backed by machine-readable conformance
evidence must be described as planned.

Start with the [versioned v1 guide](docs/v1/README.md) for executable quick
starts, domain examples, production security, compatibility, design rationale,
troubleshooting, and the third-party implementation path.

## Repository map

- `spec/v1`: normative, language-neutral protocol and schema documents.
- `conformance`: portable fixtures and profile manifests.
- `docs/tooling.md`: offline CLI, editor adapter, mocks, and compatibility workflow.
- `playground`: opt-in loopback browser and bounded schema-only mock HTTP
  integration; see the [playground guide](docs/playground.md).
- `tooling/lsp` and `editors/vscode`: the bounded LSP 3.17 stdio server and
  reference VS Code client for `tooling.lsp-1`; see the
  [LSP integration guide](docs/lsp.md).
- `protocol`: strict envelopes, decoding, and typed operation documents.
- `protocol/cbor`: dependency-free deterministic unary CBOR codec, strict
  budgets, and JSON semantic projection for `transport.cbor.unary-1`.
- `internal/slicesx`: dependency-free slice transformations shared by portable layers.
- `client`: runtime-independent Go SDK request values, immutable typed
  builders, generated-result primitives, persisted manifests, query-only
  retries, bounded batching/pagination, unary HTTP, and authenticated POST SSE
  for the `sdk.go.client-1` and `sdk.go.operations-1` profiles.
- `normalizedcache`: opt-in reference normalized entity/request caching,
  deterministic partial and streamed merge, scoped invalidation, and safe
  optimistic reconciliation for `sdk.normalized-cache-1`; raw client responses
  remain the default.
- `sdk/typescript`: dependency-free ESM JavaScript runtime and deterministic
  strict TypeScript operation bindings for `sdk.typescript.core-1`.
- `sdk/php`: framework-neutral PSR-18 unary client, lossless PHP value model,
  serializer-neutral attributes, and deterministic bindings for
  `sdk.php.core-1`, plus request-scoped Symfony and Laravel PSR bridge
  factories for `sdk.php.adapters-1`.
- `sdk/python`: dependency-free typed Python 3.11-3.14 client core plus bounded
  stdlib HTTP/threaded-async and optional strict Pydantic adapters for
  `sdk.python.core-1` and `sdk.python.adapters-1`.
- `sdk/rust`: runtime-neutral Rust client ownership traits, lossless scalar
  wrappers, deterministic serde operation bindings for `sdk.rust.core-1`, and
  optional task-local Tokio adapters for `sdk.rust.adapters-1`.
- `sdk/jvm`: Java 17 wire/client core, deterministic Java and Kotlin bindings,
  cancellation-safe coroutine and Flow views, a separate server-only
  `java.net.http` unary adapter, and an Android API-26 D8 compatibility profile.
- `sdk/dotnet`: nullable multi-target C# wire types, lossless scalar adapters,
  idiomatic F# presence/`Async` helpers, cancellable HTTP/SSE, and optional
  ASP.NET dependency injection for `sdk.dotnet.core-1` and
  `sdk.dotnet.adapters-1`.
- `sdk/swift`: reflection-free Swift Package Manager core and generated Codable
  bindings for `sdk.swift.core-1`, plus the separately profiled HTTPS
  URLSession, POST-SSE, and lifecycle bridge in `sdk.swift.apple-1`.
- `sdk/dart`: dependency-free null-safe Dart client core, lossless scalar and
  partial-result models, pluggable transport contracts, and deterministic
  operation bindings for `sdk.dart.core-1`.
- `sdk/ruby/` contains the dependency-free Ruby core, deterministic generated
  operation bindings, RBS metadata, a bounded transport client, and optional
  Faraday/Rails contracts for `sdk.ruby.core-1` and `sdk.ruby.adapters-1`.
- `schema`: portable types, values, scalar codecs, portable validation,
  strict JSON Schema 2020-12 constraint mappings, an RFC 8927 JTD projection
  with independent fidelity reports, and deterministic federation
  composition from operator-pinned service manifests.
- `collectionquery`: optional provider-neutral typed filter evaluation, stable
  sorting, and parameterized SQLite translation for `collection.query-1`.
- `largevalue`: transport-neutral signed upload/download capabilities,
  streaming integrity and scanning gates, deterministic cleanup ownership,
  payload identity, range/resume decisions, and pinned-dial egress policy for
  `core.large-value-1`.
- `largevalueadapter`: Go 1.27 direct HTTP, pinned presigned,
  filesystem-backed multipart/resumable, and application-provided streaming
  adapters for `implementation.go.large-value-adapters-1`; see the
  [large-value adapter guide](docs/large-value-adapters.md).
- `mutation`: provider-neutral typed updates, exact-path JSON Patch validation,
  opaque revisions, conditional writes, and declared read-consistency modes for
  `mutation.update-1`.
- `event`: application-neutral CloudEvents envelopes, exact-byte RFC 9421
  webhook verification, replay status, endpoint policy, and durable delivery
  state contracts for `core.events-1`; applications remain responsible for
  producing business events and #90 owns durable HTTP delivery.
- `webhook`: durable SQLite outbox/replay/order state, fenced retry dispatch,
  pinned-address no-redirect HTTPS delivery, rotation-aware signing, and a
  listener-neutral receiver for `events.webhook-adapters-go-1`; see the
  [webhook adapter guide](docs/webhook-adapters.md).
- `generator`: versioned language-neutral generator-model validation,
  deterministic reference output, semantic manifest hashing, and output-root
  containment for `sdk.generation-1`.
- `interopadapter`: protocol-neutral, fail-closed fidelity reports,
  deterministic bounded backend projections, explicit imported-operation
  registration, and a constrained HTTP-JSON reference consumer for
  `core.adapters-1`; full external-protocol integrations remain optional.
- `graphqladapter`: optional dependency-free GraphQL September 2025 schema and
  operation import/export, explicit resolver registration, safe partial/error
  mapping, and in-process query, mutation, and subscription integration for
  `core.adapters.graphql-1`; wire transports and frameworks require separate
  evidence. See the [GraphQL adapter guide](graphqladapter/README.md).
- `openapiadapter`: strict OpenAPI 3.2.0 JSON import/export and explicitly
  approved, bounded `net/http` runtime consumption for `adapter.openapi-1`;
  see the [OpenAPI adapter guide](openapiadapter/README.md).
- `asyncapi`: deterministic AsyncAPI 3.0.0 event descriptions, fail-closed
  local import, compatibility classification, and handler-free tooling views;
  see the [AsyncAPI guide](docs/asyncapi.md).
- `openrpcadapter`: optional strict OpenRPC 1.4.1 description import/export and
  bounded JSON-RPC 2.0 consume/expose integration for
  `adapter.openrpc-jsonrpc-1`; see the
  [profile boundary and lifecycle](openrpcadapter/README.md).
- `mcpadapter`: MCP `2025-11-25` trusted registration and schema-fidelity core
  plus bounded Go stdio and Streamable HTTP client/server adapters for
  `adapter.mcp-go-runtime-1`. See the [MCP adapter guide](mcpadapter/README.md).
- `generatorplugin`: explicit generator-plugin discovery, bounded process
  hosting, safe diagnostics, and reproducible third-party fixture execution for
  `sdk.generator-plugin-host-1`; see the
  [generator plugin host guide](docs/generator-plugin-host.md).
- `runtime`: explicit registration, persisted admission, bounded cursor
  pagination, request-scoped batch/cache coordination, dependency-free
  observability hooks, durable queue-neutral asynchronous operation
  coordination, process-wide admission/lifecycle control, bounded
  stream replay and source ownership, a bounded reference federation
  coordinator, deterministic distributed entity-fetch planning, operator-bound
  transport adapters, planning, execution, and the optional bounded
  `runtime.go.plan-cache-1` plan-template cache; see the
  [Go registry contract](docs/registry.md) and
  [plan-cache profile](docs/plan-cache.md), and
  [process-hosting guide](docs/process-hosting.md), plus the
  [Go federation coordinator profile](docs/federation-coordinator.md).
  Production distributed
  federation planning and transport integration remain owned by #109.
- `remoteworker`: the language-neutral length-delimited wire model, bounded
  Go reference gateway, stdio conformance transport, retry decisions,
  reference scoping, and stream-credit state for `worker.remote-1`; see the
  [remote-worker deployment guide](docs/remote-workers.md). Production HTTP/2
  pool and process integration remains owned by #88.
- `sdk/php`: the PHP 8.3-8.5 client plus explicit generated server-handler
  bindings, request-scoped dispatcher, FPM unary profile, and listener-free
  framed worker conformance for `sdk.php.server-1`; native framework worker
  adapters remain owned by #97.
- `asyncoperation`: durable SQLite operation storage, private fencing leases,
  bounded recovery dispatch, lease-renewing workers, and optional post-commit
  queue hints for `operations.async-adapters-go-1`; see the
  [durable adapter guide](docs/async-operation-adapters.md).
- `subscriptionbroker`: PostgreSQL, Redis Streams, and NATS JetStream
  delivery-source adapters with explicit atomic-driver capability gates,
  bounded replay, stable redacted failures, and reconnect-safe canary/rollback
  control for `streaming.broker-adapters-go-1`; see the
  [broker adapter guide](docs/subscription-broker-adapters.md).
- `observability`: optional OpenTelemetry trace, metric, and log export plus
  application-owned transactional audit/outbox admission for
  `operations.observability-integrations-go-1`; see the
  [integration guide](docs/observability-integrations.md), the
  [process-hosting guide](docs/process-hosting.md), and the
  [Go federation coordinator profile](docs/federation-coordinator.md).
- `tooling`: shared offline validation, formatting, hashing, explain, editor,
  manifest compatibility, deterministic mocks, and credential-redaction core
  for `tooling.workflow-1`; `tooling/lsp` adds the issue #94 LSP transport
  without redefining that core; see the [tooling guide](docs/tooling.md) and
  [LSP integration guide](docs/lsp.md).
- `playground`: opt-in loopback browser and bounded schema-mock handler for
  `naatre.playground-mock-1`; see the [playground guide](docs/playground.md).
- `reflectadapter`: optional startup-only compilation of explicitly tagged Go
  fields and allowlisted methods into ordinary runtime definitions; explicit
  runtime registration remains the production recommendation. See the
  [reflection adapter guide](reflectadapter/README.md).
- `transport/http`: strict reusable SSE framing for
  [`core.streaming-1`](spec/v1/streaming.md) and opt-in authenticated,
  filtered schema discovery for `schema.discovery.http-1`; see the
  [discovery profile](docs/schema-discovery.md). #72 and #73 own the concrete
  streaming and general operation `net/http` adapters.
- [`core.transport-batch-1`](spec/v1/request-batching.md) defines the distinct
  finite request-batch envelope, aggregate admission, item correlation,
  independent/fail-fast/atomic policies, and the HTTP-versus-streaming
  multiplexing boundary. It does not turn connection sharing into atomic work.
- `transport/http`: router-free Go `net/http` unary execution and secure
  subscription-handle establishment/delivery with bounded negotiation,
  cancellation, safe failures, cache-safe defaults, exact-origin CORS/CSRF,
  Fetch bearer and same-origin EventSource paths, and strict reusable SSE framing; see the
  [adapter runtime and capability matrix](transport/http/README.md).
- `internal/conformance`: Go-only conformance harness internals.
- `internal/qualityharness`: Go-only fuzz, repeated-race, owned-resource leak,
  fault-injection, and benchmark instrumentation for `quality.go.harness-1`;
  see the [Go quality harness](docs/go-quality-harness.md).
- `examples/processhost`: bounded `net/http` health, admission, drain, and
  context-driven supervisor integration for `operations.lifecycle-1`.
- `sdk`: deterministic generated client mapping slices. Checked-in operation
  bindings cover each SDK's documented core profile; the final combined
  compatibility matrix remains owned by #69.

Dependencies point inward in that order: client packages use only portable
protocol contracts; observability and reflection adapters, transports, other
SDKs, and example packages may use the public protocol/schema/runtime
contracts. Portable schema, protocol, runtime, and client packages never
depend on an optional adapter or application transport.

## Development

Go 1.27 is both the minimum supported version and release toolchain for v1.

```sh
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
golangci-lint run
govulncheck ./...
cargo +stable fmt --manifest-path sdk/rust/Cargo.toml --check
cargo +stable clippy --manifest-path sdk/rust/Cargo.toml --all-targets --all-features --locked -- -D warnings
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --no-default-features --locked
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --all-features --locked
```

Generated files are committed only when `go generate ./...` reproduces them
byte-for-byte and `git diff --exit-code` remains clean.

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and the
[project governance policy](docs/governance/governance.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
