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
- `protocol`: strict envelopes, decoding, and typed operation documents.
- `internal/slicesx`: dependency-free slice transformations shared by portable layers.
- `client`: runtime-independent Go SDK request values, immutable typed
  builders, generated-result primitives, persisted manifests, query-only
  retries, bounded batching/pagination, unary HTTP, and authenticated POST SSE
  for the `sdk.go.client-1` and `sdk.go.operations-1` profiles.
- `sdk/typescript`: dependency-free ESM JavaScript runtime and deterministic
  strict TypeScript operation bindings for `sdk.typescript.core-1`.
- `sdk/dart`: dependency-free null-safe Dart client core, lossless scalar and
  partial-result models, pluggable transport contracts, and deterministic
  operation bindings for `sdk.dart.core-1`.
- `schema`: portable types, values, scalar codecs, portable validation and
  strict JSON Schema 2020-12 constraint mappings, and deterministic federation
  composition from operator-pinned service manifests.
- `collectionquery`: optional provider-neutral typed filter evaluation, stable
  sorting, and parameterized SQLite translation for `collection.query-1`.
- `generator`: versioned language-neutral generator-model validation,
  deterministic reference output, semantic manifest hashing, and output-root
  containment for `sdk.generation-1`.
- `runtime`: explicit registration, persisted admission, bounded cursor
  pagination, request-scoped batch/cache coordination, dependency-free
  observability hooks, process-wide admission/lifecycle control, bounded
  stream replay and source ownership, a bounded reference federation
  coordinator, planning, and execution; see the
  [Go registry contract](docs/registry.md) and
  [process-hosting guide](docs/process-hosting.md). Production distributed
  federation planning and transport integration remain owned by #109.
- `reflectadapter`: optional startup-only compilation of explicitly tagged Go
  fields and allowlisted methods into ordinary runtime definitions; explicit
  runtime registration remains the production recommendation. See the
  [reflection adapter guide](reflectadapter/README.md).
- `transport/http`: strict reusable SSE framing for
  [`core.streaming-1`](spec/v1/streaming.md); #72 and #73 own the concrete
  streaming and general `net/http` adapters.
- `internal/conformance`: Go-only conformance harness internals.
- `examples/processhost`: executable minimal `net/http` process host.
- `sdk`: deterministic generated client mapping slices. `sdk/go/generated`
  contains the checked-in `sdk.go.operations-1` result and operation bindings;
  full cross-language compatibility certification remains planned.

Dependencies point inward in that order: client packages use only portable
protocol contracts; reflection adapters, transports, other SDKs, and example
packages may use the public protocol/schema/runtime contracts. Portable schema,
protocol, runtime, and client packages never depend on an optional reflection
adapter or application transport.

## Development

Go 1.27 is both the minimum supported version and release toolchain for v1.

```sh
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
golangci-lint run
govulncheck ./...
```

Generated files are committed only when `go generate ./...` reproduces them
byte-for-byte and `git diff --exit-code` remains clean.

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and
[docs/governance/bootstrap.md](docs/governance/bootstrap.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
