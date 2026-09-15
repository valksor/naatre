# JTD projection workflow

Naatre's JTD support is an offline, fail-closed projection of the canonical
schema. It is suitable for RFC 8927 generators that need types and structural
validation only; the JTD artifact is not an operation manifest and is not proof
of Naatre runtime conformance.

Export a root and publish its independent fidelity report:

```sh
go run ./cmd/naatre schema jtd export \
  --schema schema.naatre.json \
  --root Account \
  --report account.jtd.fidelity.json > account.jtd.json
```

The result is ordinary JTD JSON. Third-party generators can read `ref`,
`definitions`, `type`, `elements`, `values`, `properties`,
`optionalProperties`, `enum`, `discriminator`, and `mapping` without importing
Go runtime packages. They may ignore the namespaced Naatre metadata, but doing
so means they consume only the JTD projection.

Validate, import, or diff an artifact exported by the same pinned mapper:

```sh
go run ./cmd/naatre schema jtd validate --jtd account.jtd.json --approve-embedded-identities
go run ./cmd/naatre schema jtd import --jtd account.jtd.json --approve-embedded-identities > imported.naatre.json
go run ./cmd/naatre schema jtd diff --before old.jtd.json --after new.jtd.json --approve-embedded-identities
```

For a third-party JTD document, assign identities explicitly and select its
canonical positions and revision:

```json
{
  "types": {"Account": "Account"},
  "fields": {"Account/name": "Account.name"},
  "members": {}
}
```

```sh
go run ./cmd/naatre schema jtd import \
  --jtd third-party.jtd.json \
  --identities identities.json \
  --revision imported-jtd-r1 \
  --input --output
```

Invoking import is the first approval; `--approve-embedded-identities` or an
identity assignment file is the separate stable-identity approval. References
never leave root `definitions`, and all import paths share the same size,
depth, metadata, identifier, property, definition, and recursion limits.
