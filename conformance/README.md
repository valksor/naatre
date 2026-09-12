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

The core planner resolves the built-in `include` and `skip` directives and
rejects unregistered directives; #19 owns custom directive declarations and
hooks. It validates `collection.page-1` admission and collection item scopes;
#16 owns advertised pagination metadata and cursor execution. Until #8 lands,
the reference executor runs only flat argument-free call plans and returns
`UNSUPPORTED_EXECUTION_PLAN` before invoking a handler for structured plans.
