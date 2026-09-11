# Release policy

Naatre components version independently. A release manifest pins the exact
specification revision, canonicalization revision, runtime, conformance suite,
generator, clients, and profiles that form a tested release.

Breaking changes require an accepted decision record and migration note. The
v1 wire, schema, canonicalization, and profile contracts follow the semantic
compatibility rules established by the governance work.

The v1 milestone requires Linux amd64 and arm64 runtime evidence plus portable
Go library evidence on macOS amd64/arm64 and Windows amd64. Artifacts without
the profile's machine-readable evidence cannot advertise conformance.
