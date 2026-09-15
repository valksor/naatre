# Release policy

Naatre components version independently. Every published release includes a
machine-readable manifest conforming to
[`naatre.release-manifest-1`](releases/release-manifest.schema.json), with exact
specification, schema, canonicalization, fixture, conformance, runtime,
generator, SDK, worker, report, and artifact versions and digests.

Breaking changes require an accepted decision record, explicit compatibility
classification, and migration guidance. Retired names remain reserved for the
major line. Drafted or planned components cannot be advertised as implemented;
an implementation row and every conformance claim require applicable
machine-readable evidence.

Checksums, SBOMs, provenance, reproducible-generation evidence, and applicable
signatures are release requirements. The complete governance, security,
compatibility, deprecation, experimental-promotion, revocation, and checklist
rules are in [the release governance policy](docs/governance/releases.md).
