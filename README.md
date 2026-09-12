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
- `schema`: portable types, values, and scalar codecs.
- `runtime`: explicit registration, persisted admission, planning, and execution;
  see the [Go registry contract](docs/registry.md).
- `transport/http`: the planned Go adapter for the normative
  [`core.http-1` binding](spec/v1/http.md).
- `internal/conformance`: Go-only conformance harness internals.
- `sdk` (planned): official and compatibility-certified client implementations.

Dependencies point inward in that order: transport and SDK packages may use
the public protocol/schema/runtime contracts; portable schema and protocol
packages never depend on a transport or application runtime.

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
