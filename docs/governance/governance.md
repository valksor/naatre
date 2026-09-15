# Project governance

This policy replaces the temporary rules in [bootstrap.md](bootstrap.md). The
bootstrap ownership decision remains ratified: until another maintainer is
installed by a public decision record, `@k0d3r1s` is the interim owner and sole
required approver.

## Proposals and decisions

Anyone may propose a change by opening an issue that states the problem,
alternatives, compatibility class, migration impact, conformance impact, and
responsible component issue. Normative wire, schema, canonicalization, fixture,
or stable-profile changes also require a decision record under
`docs/decisions/`. Decision records use one of `proposed`, `accepted`,
`rejected`, `superseded`, or `withdrawn`; accepted and rejected records are
immutable except for links to a superseding record.

An ordinary proposal and its decision are public. A maintainer must record the
rationale, rejected alternatives, compatibility class, affected fixtures, and
unresolved questions before merging. Roadmap work may be `planned`,
`implemented`, or `removed-by-decision`. It may not be called optional,
omitted, deferred, or out of scope unless a labeled public decision record
names the affected issue and release scope. Release scope is a deliberate
maintainer choice, never a way to make a milestone appear complete.

## Maintainers and review

Maintainers are installed or removed by an accepted public decision record.
The component owner reviews implementation and evidence; the release owner
reviews the complete manifest and supply-chain record. Once a second
maintainer exists, normative wire, schema, canonicalization, and stable-profile
changes require two maintainer approvals. Until then, the interim owner records
the rationale, alternatives, compatibility class, and conformance impact and
provides the sole approval. An author may not count automation as a maintainer
approval.

Copied code retains its applicable copyright and license notices in the
distribution. Borrowed ideas are identified in the decision ledger but do not
create a compatibility mode. Normative external references, generated inputs
and outputs, third-party notices, and decision status are pinned in every
published release manifest.

## Security embargoes

Suspected vulnerabilities follow [SECURITY.md](../../SECURITY.md), not public
issues. The interim owner may create a private embargo record and invite only
the minimum component maintainers, release operators, and reporters needed to
coordinate a fix. The private record carries the same rationale,
compatibility, review, test, and provenance fields as a public decision.

Embargoed changes may merge and release without public issue disclosure. After
the fix is available and disclosure is safe, the owner publishes an advisory
and a redacted decision record, or records why continuing confidentiality is
necessary. Public release metadata must not reveal exploit details before the
coordinated disclosure date.

## Authoritative records

- [Release and versioning policy](releases.md)
- [Decision and dependency ledger](decision-ledger.md)
- [Release records and schema](../../releases/README.md)
- [Machine-readable governance contract](../../conformance/v1/governance.json)
- [Machine-readable roadmap](../../conformance/v1/roadmap.json)
