# Decision 0004: AsyncAPI 3.0.0 event-description profile

Status: accepted for v1.

Naatre pins the exact normative source
`https://www.asyncapi.com/docs/reference/specification/v3.0.0` and exports
profile `adapter.asyncapi-1` with exporter `asyncapi-exporter-1`. The reviewed
revision provides the channel, operation, message, correlation, server,
security, and binding vocabulary needed by event tooling while allowing Naatre
identities and revisions to remain explicit extensions.

The compatibility class is additive for the Naatre v1 specification. Existing
wire and execution contracts do not change. AsyncAPI is descriptive metadata,
not a new runtime or wire compatibility mode. Export and import are separate,
and the import boundary has no network resolver or registration capability.

Alternatives considered were AsyncAPI 2.6, an unversioned "latest 3.x" source,
and a Naatre-only event catalog. AsyncAPI 2.6 lacks the v3 operation/channel
shape used here. A floating revision is not reproducible. A private catalog
would force independent tooling to invent the same mapping. All three were
rejected.

The conformance impact is the new `conformance/v1/asyncapi.json` fixture plus
Go exporter/importer, evolution, CLI, and playground tests. Protected schema
members remain absent from default output, examples use the shared credential
redactor, and exact server allowlists preserve the existing egress boundary.

Issue #102 owns broader exporter/importer integration and round-trip fixtures.
It may extend only through a new reviewed profile/exporter revision. No
unresolved question changes the v1 core profile.
