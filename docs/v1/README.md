# Naatre v1 guide

This is the progressive, non-normative adoption guide for specification v1. The normative contract remains the [v1 specification](../../spec/v1/README.md), and executable truth remains the [fixture suite](../../conformance/README.md). Start with the [quick starts](quick-starts.md), then use the [domain examples](domain-examples.md), [production guide](security-and-production.md), and [implementation guide](third-party-implementation.md).

The matching component tuple is published in [versions](versions.md). The [documentation evidence manifest](evidence.json) binds every quick start and domain example to a source file, fixture, and offline verification command. The Go tests in `internal/doccheck` reject missing sources, stale generated error links, incomplete feature coverage, and untracked fenced samples.

Issue #77 owns expanded package-specific tutorials and troubleshooting integrations. These core guides and their executable evidence do not depend on an optional integration or on #77 being implemented.

Naatre is inspired by Deepr and other API systems. Deepr is inspiration only: Naatre does not claim Deepr compatibility, wire compatibility, lineage, or shared versioning.

Reading order: learn the operation shape in quick starts; learn presence, ordering, and scalar rules in [model and decisions](model-and-decisions.md); choose security and resource limits before admission; consult [compatibility and comparisons](compatibility-and-comparisons.md) before making a support claim; use [troubleshooting](troubleshooting.md) by stable code and conformance clause.
