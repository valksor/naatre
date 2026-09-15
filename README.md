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

## Repository map

- `spec/v1`: normative, language-neutral protocol and schema documents.
- `conformance`: portable fixtures and profile manifests.
- `docs/tooling.md`: offline CLI, editor adapter, mocks, and compatibility workflow.
- `tooling/lsp` and `editors/vscode`: the bounded LSP 3.17 stdio server and
  reference VS Code client for `tooling.lsp-1`; see the
  [LSP integration guide](docs/lsp.md).
- `protocol`: strict envelopes, decoding, and typed operation documents.
- `internal/slicesx`: dependency-free slice transformations shared by portable layers.
- `client`: runtime-independent Go SDK request values, immutable typed
  builders, generated-result primitives, persisted manifests, query-only
  retries, bounded batching/pagination, unary HTTP, and authenticated POST SSE
  for the `sdk.go.client-1` and `sdk.go.operations-1` profiles.
- `sdk/typescript`: dependency-free ESM JavaScript runtime and deterministic
  strict TypeScript operation bindings for `sdk.typescript.core-1`.
- `sdk/php`: framework-neutral PSR-18 unary client, lossless PHP value model,
  serializer-neutral attributes, and deterministic bindings for
  `sdk.php.core-1`; framework integrations remain owned by #79.
- `sdk/python`: dependency-free typed Python 3.11-3.14 client core, sync and
  async transport protocols, lossless scalar wrappers, SSE lifecycle, and
  deterministic dataclass bindings for `sdk.python.core-1`.
- `sdk/rust`: runtime-neutral Rust client ownership traits, lossless scalar
  wrappers, and deterministic serde operation bindings for `sdk.rust.core-1`.
- `sdk/jvm`: Java 17 wire/client core, deterministic Java and Kotlin bindings,
  and cancellation-safe coroutine and Flow views for `sdk.jvm.core-1`.
- `sdk/dotnet`: nullable multi-target C# wire types, lossless scalar adapters,
  F# consumption proof, and cancellable HTTP/SSE reference client for
  `sdk.dotnet.core-1`.
- `sdk/swift`: reflection-free Swift Package Manager client core, lossless
  canonical scalar wrappers, async transport/cancellation contract, and
  deterministic Codable operation bindings for `sdk.swift.core-1`; concrete
  Apple transport adapters remain owned by #86.
- `sdk/dart`: dependency-free null-safe Dart client core, lossless scalar and
  partial-result models, pluggable transport contracts, and deterministic
  operation bindings for `sdk.dart.core-1`.
- `sdk/ruby/` contains the dependency-free Ruby core, deterministic generated
  operation bindings, and RBS metadata for `sdk.ruby.core-1`.
- `schema`: portable types, values, scalar codecs, portable validation and
  strict JSON Schema 2020-12 constraint mappings, and deterministic federation
  composition from operator-pinned service manifests.
- `collectionquery`: optional provider-neutral typed filter evaluation, stable
  sorting, and parameterized SQLite translation for `collection.query-1`.
- `mutation`: provider-neutral typed updates, exact-path JSON Patch validation,
  opaque revisions, conditional writes, and declared read-consistency modes for
  `mutation.update-1`.
- `generator`: versioned language-neutral generator-model validation,
  deterministic reference output, semantic manifest hashing, and output-root
  containment for `sdk.generation-1`.
- `interopadapter`: protocol-neutral, fail-closed fidelity reports,
  deterministic bounded backend projections, explicit imported-operation
  registration, and a constrained HTTP-JSON reference consumer for
  `core.adapters-1`; full external-protocol integrations remain optional.
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
  transport adapters, planning, and execution; see the
  coordinator, planning, execution, and the optional bounded
  `runtime.go.plan-cache-1` plan-template cache; see the
  [Go registry contract](docs/registry.md) and
  [plan-cache profile](docs/plan-cache.md), and
  [process-hosting guide](docs/process-hosting.md). Production distributed
  federation planning and transport integration remain owned by #109.
- `observability`: optional OpenTelemetry trace, metric, and log export plus
  application-owned transactional audit/outbox admission for
  `operations.observability-integrations-go-1`; see the
  [integration guide](docs/observability-integrations.md).
  [process-hosting guide](docs/process-hosting.md), plus the
  [Go federation coordinator profile](docs/federation-coordinator.md).
- `tooling`: shared offline validation, formatting, hashing, explain, editor,
  manifest compatibility, deterministic mocks, and credential-redaction core
  for `tooling.workflow-1`; `tooling/lsp` adds the issue #94 LSP transport
  without redefining that core; see the [tooling guide](docs/tooling.md) and
  [LSP integration guide](docs/lsp.md).
- `reflectadapter`: optional startup-only compilation of explicitly tagged Go
  fields and allowlisted methods into ordinary runtime definitions; explicit
  runtime registration remains the production recommendation. See the
  [reflection adapter guide](reflectadapter/README.md).
- `transport/http`: strict reusable SSE framing for
  [`core.streaming-1`](spec/v1/streaming.md) and opt-in authenticated,
  filtered schema discovery for `schema.discovery.http-1`; see the
  [discovery profile](docs/schema-discovery.md). #72 and #73 own the concrete
  streaming and general operation `net/http` adapters.
  [`core.streaming-1`](spec/v1/streaming.md); #72 and #73 own the concrete
  streaming and general `net/http` adapters.
- [`core.transport-batch-1`](spec/v1/request-batching.md) defines the distinct
  finite request-batch envelope, aggregate admission, item correlation,
  independent/fail-fast/atomic policies, and the HTTP-versus-streaming
  multiplexing boundary. It does not turn connection sharing into atomic work.
- `transport/http`: router-free Go `net/http` unary execution with bounded
  negotiation, cancellation, safe failures, cache-safe defaults, exact-origin
  CORS, and strict reusable SSE framing; see the
  [adapter runtime and capability matrix](transport/http/README.md).
- `internal/conformance`: Go-only conformance harness internals.
- `internal/qualityharness`: Go-only fuzz, repeated-race, owned-resource leak,
  fault-injection, and benchmark instrumentation for `quality.go.harness-1`;
  see the [Go quality harness](docs/go-quality-harness.md).
- `examples/processhost`: executable minimal `net/http` process host.
- `sdk`: deterministic generated client mapping slices. `sdk/go/generated`,
  `sdk/typescript/generated`, and `sdk/dotnet/Naatre.Core/Generated` contain
  checked-in operation bindings; the final combined compatibility matrix
  remains owned by #69.
- `examples/processhost`: bounded `net/http` health, admission, drain, and
  context-driven supervisor integration for `operations.lifecycle-1`.
- `sdk`: deterministic generated client mapping slices. `sdk/go/generated`
  contains the checked-in `sdk.go.operations-1` result and operation bindings;
  full cross-language compatibility certification remains planned.

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

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and
[docs/governance/bootstrap.md](docs/governance/bootstrap.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
