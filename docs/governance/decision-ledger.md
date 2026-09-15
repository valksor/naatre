# Decision and dependency ledger

This ledger is the release baseline. Each release manifest pins its status and
the immutable revision of every applicable decision record.

| Choice | Status | Selected baseline | Rejected alternatives | Evidence | Responsible issue |
| --- | --- | --- | --- | --- | --- |
| Repository, license, ownership | accepted | Apache-2.0 monorepo; interim owner `@k0d3r1s` | MIT; MPL-2.0; coordinated repositories | [bootstrap decision](bootstrap.md) | [#68](https://github.com/valksor/naatre/issues/68) |
| Go foundation | accepted | Go 1.27 module and offline quality gates | unversioned toolchain; implementation-defined wire truth | [`go.mod`](../../go.mod), [`CONTRIBUTING.md`](../../CONTRIBUTING.md) | [#1](https://github.com/valksor/naatre/issues/1) |
| Schema evolution | accepted | authenticated discovery, deprecation metadata, deterministic compatibility analysis | implicit runtime-only discovery | [`schema.json`](../../conformance/v1/schema.json) | [#15](https://github.com/valksor/naatre/issues/15) |
| Conformance claims | accepted | named profiles with exact machine-readable reports | repository-presence certification | [`profiles.json`](../../conformance/v1/profiles.json) | [#36](https://github.com/valksor/naatre/issues/36) |
| Governance and releases | accepted | independent SemVer components and evidence-gated release manifests | one repository-wide version; prose-only claims | [`governance.json`](../../conformance/v1/governance.json) | [#49](https://github.com/valksor/naatre/issues/49) |

Unresolved questions remain explicit release blockers rather than implicit
omissions: selection of long-term co-maintainers, production signing identities
and transparency services, and the first stable support-window start date.
Their responsible component issue is [#49](https://github.com/valksor/naatre/issues/49)
until a public decision assigns a successor.

The authoritative roadmap coverage is `conformance/v1/suite.json`:
`issueMappings` contains every repository component issue from 1 through 110
exactly once. Planned work points to an explicit `roadmap.json` owner record;
implemented work points to its fixture evidence. Adding or splitting a
component issue requires updating this inventory in the same change.
