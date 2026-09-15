# Decision 0005: version the complete AsyncAPI projection separately

Status: accepted for v1.

Issue #62 deliberately defines `adapter.asyncapi-1` as a bounded,
metadata-only AsyncAPI 3.0.0 description with opaque Naatre schema references.
Issue #102 needs complete JSON Schema projection and supported byte-stable
round trips. Widening the existing profile in place would make an old profile
identity describe two different contracts.

The implementation therefore adds `adapter.asyncapi.projection-1` and exporter
`asyncapi-projection-exporter-1`. The new profile embeds the complete canonical
schema and its semantic digest, but import accepts it only when it equals a
separately supplied caller-authorized `schema.Document`. The existing issue
#62 exporter and importer remain unchanged and are reused as the closed event
metadata boundary.

The accepted profile is an offline Go library with context cancellation and
the existing AsyncAPI limits. Import requires byte-identical re-export before
returning detached metadata. It does not register operations, infer policy,
invoke handlers, resolve references, perform network I/O, or claim native
transport and third-party certification support.

Alternatives rejected were silently adding full schemas to
`adapter.asyncapi-1`, treating the embedded schema as authority, accepting a
lossy best-effort round trip, and building a parallel AsyncAPI-owned operation
model. Those choices would respectively break version identity, allow policy
redefinition, hide fidelity loss, or violate the issue #62 authority boundary.
