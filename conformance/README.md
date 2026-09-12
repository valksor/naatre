# Conformance fixtures

Fixtures are language-neutral JSON and are normative only for the versioned
profile named in each file. Every implementation must consume these files
directly and publish machine-readable results that bind the fixture digest,
implementation version, platform, and claimed profile.

Generated fixture changes must be reproducible byte-for-byte. Tests read the
checked-in files rather than duplicating their expected values in Go source.

The `core.scalar.c14n-1` and `core.interop.c14n-1` vectors are verified by both
the Go reference implementation and dependency-free JavaScript implementations
in `independent/scalars.mjs` and `independent/canonical.mjs`. Run the independent
checks with the CI-pinned Node 24.21.0 toolchain:

```sh
node conformance/independent/scalars.mjs
node conformance/independent/canonical.mjs
```

The `core.value-1` vectors cover schema-directed maps, lists, input objects,
one-of activation, enum compatibility, and recursive value limits.

The `core.language-1` vectors cover every core composition tag and expression,
response shaping, collection edge states, result-reference scopes, and the
zero-handler boundary for invalid operations. Their document grammar is the
strict JSON Schema in `spec/v1/language.schema.json`; semantic execution of the
vectors is implemented by the validation and runtime conformance issues.

`v1/planning.json` fixes the request-neutral description of resolved plans.
The same document prepared with different correlation IDs, variable values,
policy inputs, and authorization identities must retain the same static handler,
type, effect, authorization-policy, and source-pointer description without
retaining any of that per-request state.

`v1/security.json` fixes the `core.security-1` denial shape, static and dynamic
decision semantics, operation-kind distinction, lifecycle and cache-scope
rules, and equivalent decisions for planned, cached, batched, streamed, and
remote placement. Its executable vectors cover aliases, fragments, parallel
groups, nested calls, whole-collection denial, mixed replay history, and cursor
binding across principal, tenant, schema, and authorization revisions. The Go
reference runtime executes the core handler and lifecycle vectors directly;
#70 owns downstream cache, batch, stream, replay, and remote adapters.

`v1/schema.json` fixes the `core.schema-1` portable schema authority. It covers
complete, filtered, recursive, extended-trait, evolving, open-union, and
deprecation snapshots. The Go consumer strict-parses each document, imports it
into an immutable type snapshot, re-exports byte-identical canonical JSON and
hashes, proves deny-by-default filtering against an expected document without
hidden-name leaks, and checks stable-ID compatibility classifications. The core
fixture and pure in-process APIs do not enable an HTTP discovery route; #106
owns authenticated transport integration and migration reporting.

The core planner resolves the built-in `include` and `skip` directives, rejects
unregistered directives, and supports registered version-pinned custom
directives through bounded planning, one-shot execution wrappers, and ordered
response annotations. Portable schema and canonical fixtures cover directive
identity, filtering, diffing, and cross-language normalization. It validates
`collection.page-1` admission and collection item scopes;
#16 owns advertised pagination metadata and opaque cursor execution. The
reference executor consumes all 15 positive semantic language vectors directly,
including ordered pipelines, explicit collection operations, fragments,
parallel branches, bindings, directives, and edge-state propagation. The core
page vector exercises bounded cursor-free paging; portable cursor behavior and
security remain in #16.

`v1/resources.json` fixes the `core.resources-1` stable codes and location
classes for decoder, planning, runtime, completion, and serialization limits.
Its late-exhaustion vectors model output and error budget exhaustion after a
mutation commit and require truthful `applied` effect metadata plus a bounded
audit/idempotency fallback. Its adversarial cursor vectors bound scans and
storage reads for ancient, random, missing, and unauthorized earliest-history
positions; aggregate replay/live vectors separately cap event count, bytes,
filter and authorization work, queues, memory, and duration. The Go reference
runtime executes core request budgets; #70 owns broker-backed replay/live
execution evidence.

`v1/http.json` fixes the language-neutral `core.http-1` endpoint, method,
media negotiation, singleton-header, status/body, encoding, deadline, cache,
browser, bounded-response, and HTTP-version cases. It is the portable contract
fixture; #73 owns the concrete Go `net/http` adapter, slow-client,
cancellation, graceful-shutdown, and version-specific integration harness.

`v1/persisted.json` fixes the `core.persisted-1` document hashes and identity
equivalence boundaries plus lookup-only, deploy-registration, controlled
automatic-registration, approval/cache, tenant, revocation, expiry, migration,
collision, and safe storage-failure semantics. The Go runtime consumes those
vectors and supplies a race-safe in-memory reference store.
