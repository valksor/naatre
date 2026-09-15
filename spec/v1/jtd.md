# JSON Type Definition projection and fidelity

The `schema.jtd-1` profile pins JSON Type Definition to
[RFC 8927](https://www.rfc-editor.org/rfc/rfc8927.html), mapper revision
`naatre-jtd-mapper-1`, and fidelity revision `naatre-jtd-fidelity-1`. The
canonical Naatre schema remains authoritative. JTD export and JTD import are
separate operations and evidence for either direction cannot be reused for the
other.

## Authority, identity, and binding

- **JTD-001:** Export MUST be read-only and MUST NOT alter the canonical schema,
  its revision, schema hash, or any operation hash. Output and its fidelity
  report bind the RFC, mapper, fidelity profile, canonical Naatre revision, and
  canonical Naatre schema hash.
- **JTD-002:** Import MUST require an explicit application approval. Every
  imported type, field, enum member, and variant MUST receive a stable portable
  identity either from an explicit assignment or from separately approved,
  revision-pinned Naatre metadata. A JTD spelling alone is not an identity
  migration policy.
- **JTD-003:** JSON Schema and JTD publish independent format, specification,
  mapper, and fidelity revisions. Combining their evidence MUST compare both
  canonical Naatre revision and schema hash and fail with
  `SCHEMA_MAPPING_SOURCE_MISMATCH` on disagreement. A JSON Schema constraint
  fidelity result MUST be bound independently to its canonical document; it is
  not copied from the JTD report.
- **JTD-004:** A JTD artifact is a structural projection for third-party tools.
  Consuming it requires no Go package and does not claim full Naatre runtime,
  operation, authorization, or conformance support.

## Exact structural mapping

The document root is a `ref` to the explicitly selected root type. All named
Naatre types are emitted under the root `definitions`; nested definitions and
external reference resolution are forbidden.

| Naatre form | RFC 8927 form | Fidelity |
| --- | --- | --- |
| `Boolean` | `type: boolean` | lossless |
| `String` and `ID` | `type: string`, with scalar identity metadata | lossless |
| `Int32` | `type: int32` | lossless |
| `Float64` | `type: float64` | lossless |
| list | `elements` | lossless |
| string-keyed map | `values` | lossless |
| required field | `properties` | lossless |
| optional field | `optionalProperties` | lossless |
| nullable field or element | `nullable: true` on its schema | lossless |
| closed enum | `enum` | lossless |
| closed tagged union | `$type` discriminator; each mapping has required `$value` ref | lossless |
| reusable or recursive type | root-local `definitions` and `ref` | lossless |

Required/optional presence and nullable/non-null value semantics are independent;
neither may be inferred from the other. `additionalProperties: true` is not a
closed Naatre object and is unsupported. Recursive definitions retain an
explicit positive Naatre `maxDepth`; exceeding it is an error, never truncation.

## Fidelity boundary

The sole accepted metadata member is
`https://naatre.dev/jtd/v1`. It may retain stable identities, input/output
position, exact core scalar identity, descriptions, deprecations, source
locations, and recursive depth because the mapper can round-trip those values.
Deprecation and descriptive metadata on enum and union members is unsupported
because RFC 8927 enum strings and mapping keys have no per-member metadata.
Unknown namespaces, unknown members in that namespace, conflicting scalar
claims, and mismatched RFC/mapper/profile revisions are errors.

The following required semantics are `unsupported` and MUST fail before code
generation, registration, or execution: open enums and unions, interfaces,
one-of inputs, custom scalars, `Int64`, `UInt64`, `BigInt`, `Decimal`,
`Timestamp`, `Duration`, `UUID`, and `Bytes`, defaults, validation constraints,
entities, retired identities, external schema references, directives,
extensions, operation and member metadata, effects, costs, authorization,
capabilities, and non-documentation traits. No unsupported value may be hidden
in metadata to obtain an `exact` report. A future mapper may describe an
explicitly adapted or lossy form only under a new fidelity revision.

## Bounded validation and deterministic output

- **JTD-100:** Import uses strict JSON: duplicate members, invalid Unicode,
  trailing content, unknown schema keywords, mixed forms, duplicate definition
  identities or names, duplicate properties or enum values, unknown refs,
  nested definitions, and malformed discriminators are stable diagnostics.
- **JTD-101:** Implementations MUST enforce configurable byte, JSON depth,
  definition, property, metadata, identifier, and recursive-value limits before
  allocating or registering an imported graph. References resolve only through
  the current document's root definitions. No URI is fetched.
- **JTD-102:** For identical canonical Naatre bytes, root identity, mapper
  revision, RFC revision, and fidelity revision, export bytes MUST be identical.
  Object members use canonical JSON ordering; semantic arrays retain canonical
  Naatre order.
- **JTD-103:** Every supported form has positive and negative import/export
  vectors in `conformance/v1/jtd.json`. Diagnostics include stable `code`, RFC
  6901 `pointer`, `feature`, `outcome`, and safe `message` fields.
