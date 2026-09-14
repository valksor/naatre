# v1 interoperability adapter migration note

`core.adapters-1` replaces implicit interoperability claims with an explicit
direction-specific fidelity report. Existing integrations must inventory every
operation, type, and transport mapping and reject startup if a required feature
is unsupported or an application-supplied adaptation is unresolved.

Applications must now provide Naatre effect, idempotency, retry, cache, cost,
batching, transaction, and authorization policy. Existing adapters that infer
these properties from HTTP methods, GraphQL operation labels, RPC method names,
or vendor metadata must remove that inference before claiming the profile.

Remote documents and endpoints must be pinned and bounded. Operations require
an explicit allowlist, unknown extensions cannot approve registrations,
credentials require exact forwarding configuration, and redirects are denied
without separate evidence. Naatre schema identities and wire versions remain
independent; no Deepr parser or legacy identity is introduced.
