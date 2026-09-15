# AsyncAPI projection tooling profile

The `adapter.asyncapi.projection-1` profile is the issue #102 implementation
slice layered on the issue #62 `adapter.asyncapi-1` event-description contract.
It uses the same pinned AsyncAPI 3.0.0 normative source and does not change any
Naatre protocol, schema, effect, authorization, event, or transport rule.

## Profile and authority

- **ASYNCPROJ-001:** Export MUST identify profile
  `adapter.asyncapi.projection-1`, exporter
  `asyncapi-projection-exporter-1`, projection version `1.0.0`, AsyncAPI
  `3.0.0`, and JSON Schema Draft 2020-12. It MUST retain the exact issue #62
  profile/exporter identities as dependencies.
- **ASYNCPROJ-002:** Import MUST require a separately supplied,
  initialized Naatre `schema.Document`. The embedded canonical schema,
  revision, and semantic digest MUST equal that authority. AsyncAPI operations,
  security declarations, extensions, HTTP metadata, or inferred names MUST NOT
  define or modify effects, authorization, retry, transaction, cost, or runtime
  registration policy.
- **ASYNCPROJ-003:** Export MUST project every authorized Naatre type
  deterministically into `components.schemas` and retain the complete canonical
  declaration beside its JSON Schema view. Import MUST re-export the detached
  model and require byte equality, so channels, operations, messages,
  correlations, security schemes, and bindings are lossless or rejected.

## Validation and safety

- **ASYNCPROJ-100:** Export, import, and validation MUST be offline,
  context-cancellable, and bounded by document bytes, depth, members, channels,
  messages, references, and example bytes. They MUST NOT resolve references,
  connect to servers, bind listeners, register runtime operations, or invoke
  business handlers.
- **ASYNCPROJ-101:** Every failure MUST expose a stable public code
  and a generic bounded diagnostic. Credentials, examples, URLs, schema
  contents, protected names, effect or authorization values, causes, and
  implementation details MUST NOT appear in public error text or reports.

## Evidence

- **ASYNCPROJ-200:** Machine-readable evidence MUST pin issue #62's
  exact dependency revision and file digests, the implementation files and
  runtime, the supported and unsupported capability inventory, reproducible
  commands, and positive, negative, boundary, cancellation, and resource-limit
  fixtures.
- **ASYNCPROJ-201:** A passing round trip proves only the portable Go
  library profile and the JSON description. It MUST NOT claim native runtime,
  transport interoperability, live network, framework, operating-system,
  architecture, or third-party AsyncAPI certification.
