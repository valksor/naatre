# Domain examples

The examples are complete executable fixture slices rather than illustrative JSON fragments. The machine-readable mapping is [evidence.json](evidence.json), and Go plus independent runners consume the listed fixture keys.

| Domain behavior | Executable source | What to inspect |
| --- | --- | --- |
| Deep calls, item mapping, pipelines, aliases, parallel groups, nesting and unnesting | [language fixtures](../../conformance/v1/language.json) | Ordered selection constructs, expression scope, alias response paths, map current/parent values, and nesting failures. |
| Collection-level operations, filters, sorting and cursor pagination | [collection fixtures](../../conformance/v1/collections.json) | Authorized filter scope, stable ordering, bounded page sizes, canonical cursor identity, and invalid scope. |
| Mutations and grouped commit outcomes | [mutation fixtures](../../conformance/v1/mutations.json) | Sequential effects, transaction groups, rollback/unknown commit, outbox, compensation, and cancellation around commit. |
| Conditional writes and typed updates | [update fixtures](../../conformance/v1/mutation-updates.json) | Omitted versus null versus remove, revision preconditions, patch tests, and read consistency. |
| Partial errors and partial non-null results | [partial-result fixtures](../../conformance/v1/partial-results.json) | Data and errors coexist; a required selected field that is missing, pending, failed, or skipped is not a successful model. Generated clients represent that state instead of manufacturing a zero value. |
| Persisted operations | [persisted fixtures](../../conformance/v1/persisted.json) | Canonical semantic identity, allowlists, schema staleness, expiry, revocation, and collision handling. |
| Request-scoped batching | [batching fixtures](../../conformance/v1/batching.json) | Coalescing preserves principal, tenant, policy, schema, and cancellation boundaries. For finite transport batches, also use [request-batching fixtures](../../conformance/v1/request-batching.json). |
| Subscriptions and streaming | [streaming fixtures](../../conformance/v1/streaming.json) | Establishment, bounded frames, terminal states, replay gaps, cursor binding, cancellation, and source closure. |
| Federation | [federation fixtures](../../conformance/v1/federation.json) | Entity identity, composition conflicts, trust, delegation, bounded fan-out, and safe upstream failures. |
| Files and large values | [large-value fixtures](../../conformance/v1/large-values.json) | Capability-bound transfer, byte and range limits, digest checks, scanning gates, safe egress, resume, and cleanup ownership. |
| Long-running operations | [async-operation fixtures](../../conformance/v1/async-operations.json) | Durable acceptance, fencing, retry, retention, cancellation-too-late, and terminal retrieval. |
| Validation constraints | [validation fixtures](../../conformance/v1/validation.json) | Exact numeric comparison, Unicode scalar/UTF-8 length, patterns, formats, collection bounds, dependencies, and finite diagnostics. |
| Tooling | [tooling fixtures](../../conformance/v1/tooling.json) | Offline validate/format/hash/explain, stable diagnostics, redaction, and bounded editor/mock behavior. |
| Interoperability adapters | [adapter fixtures](../../conformance/v1/adapters.json) | Directional fidelity, explicit unsupported mappings, bounded conversion, and no inferred wire compatibility. |
| Deployment lifecycle | [operations fixtures](../../conformance/v1/operations.json) | Startup validation, admission, fairness, health, immutable revision install, bounded drain, cleanup, and connection rotation. |

Durable acceptance means the operation record crossed the profile's durable acceptance barrier; it does not mean the work completed. A crash after acceptance but before queue publication is recovered from durable storage, while a crash after an external mutation but before an idempotency result is persisted can leave an indeterminate gap. Use an atomic transaction/outbox where available, persist idempotency state before replay decisions, and expose `TRANSACTION_COMMIT_UNKNOWN` or `IDEMPOTENCY_INDETERMINATE` instead of promising exactly once.
