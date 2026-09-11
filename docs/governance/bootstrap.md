# Bootstrap decisions

Status: ratified on 2026-09-11. Interim owner: @k0d3r1s.

## License

Naatre uses Apache-2.0. Its permissive terms and explicit patent grant suit a
protocol with independent implementations. MIT and MPL-2.0 were considered;
MIT lacks an express patent grant and MPL-2.0 adds file-level copyleft.

## Topology and ownership

One monorepo contains the normative specification, Go reference runtime,
conformance assets, generators, clients, and reference workers. This keeps
cross-language vectors reviewable together while packages retain independent
versions and artifacts. Coordinated repositories were rejected because they
would make atomic fixture and contract changes harder.

Until a later governance decision installs maintainer groups and CODEOWNERS,
@k0d3r1s approves normative, security, release, and SDK changes.

## Official implementations

The specification, Go runtime, conformance suite, generator contracts, Go
client, and JavaScript/TypeScript client are first-party. PHP, Python, Rust,
JVM, .NET, Swift, Dart, and Ruby clients and non-Go workers are
compatibility-certified. Certification requires the same versioned,
machine-readable evidence; repository location alone is not certification.

## Releases and profiles

v0.1.0 requires core wire/schema/runtime, HTTP, required SSE, persisted
operations, Go and JavaScript/TypeScript clients. All other roadmap capabilities
remain explicit profiles required by the full v1.0.0 gate. Optional means
separately advertised, never silently omitted.

Pre-v1 contracts may break only with a decision record and migration note. v1
compatibility and release enforcement move to the permanent governance policy.
Components version independently and a release manifest binds exact versions.

The server/runtime baseline is Linux amd64 and arm64. Portable libraries and
first-party clients additionally cover macOS amd64/arm64 and Windows amd64 when
their ecosystems support those targets.
