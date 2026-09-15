# Normalized client cache and optimistic updates

This profile defines portable client-cache semantics. It does not add cache
behavior to the core executor and does not require an application to normalize
responses.

## Profile and entity identity

- **NCACHE-001:** `sdk.normalized-cache-1` is optional. A client MUST preserve a
  raw-response path that does not construct or consult a normalized cache.
- **NCACHE-002:** Only an output object with schema `entity.keys` metadata may
  be normalized. Every key MUST name a present, non-null scalar public field.
  Keys are API identity, not permission to expose a database primary key,
  provider key, credential, or other secret. Applications MUST publish an
  opaque public identifier when their backing key is secret.
- **NCACHE-003:** Entity identity is the canonical schema type ID plus canonical
  key-field values. Response aliases, nesting, query locations, pagination
  windows, and federation service locations MUST NOT change it. Cache,
  federation references, invalidation records, and audit paths MUST consume the
  same entity contract; federation support is not required to use it.

## Scope and field identity

- **NCACHE-004:** Every cache entry MUST bind the subject, tenant,
  authorization revision, and schema revision. Logout, authorization
  revocation, or tenant switch MUST synchronously clear the old scope and stop
  its live subscriptions before the scope can be reused.
- **NCACHE-005:** A normalized field identity MUST include its canonical schema
  name, canonical arguments, locale, representation, and schema revision.
  Aliases affect only response paths. Distinct pagination arguments MUST remain
  distinct windows and an update to one window MUST NOT erase another.

## Deterministic merge and freshness

- **NCACHE-006:** Clients MUST distinguish absent, skipped, pending, failed,
  explicit-null, and value states. Only explicit-null may replace a known value
  with null. Absent, skipped, pending, or failed partial data MUST NOT erase a
  known value.
- **NCACHE-007:** Partial objects and streamed patches MUST merge by entity,
  field identity, opaque revision lineage, and transport order. An older known
  revision or order MUST NOT overwrite newer state. A divergent revision MUST
  fail unless the application supplies an explicit conflict policy. Unknown
  fields are retained in the raw response but MUST NOT be normalized until the
  active schema declares them.
- **NCACHE-008:** Cache metadata MUST distinguish `no-store` from private
  cacheability, fresh from stale-while-revalidate and expired data, and exact
  invalidation tags. A client MUST NOT promote stale data to fresh or retain an
  expired entry merely because normalized entity data exists.

## Mutations and reconciliation

- **NCACHE-009:** A known mutation effect MUST invalidate matching normalized
  entities and request-scoped results synchronously within the same principal
  and tenant scope. Returning from invalidation while a known matching request
  result remains readable is invalid.
- **NCACHE-010:** An optimistic layer MUST bind a unique mutation ID and the
  expected opaque entity revision defined by `mutation.update-1`. A confirmed
  success merges the authoritative committed revision before removing the
  layer; a confirmed rejection or definite non-commit removes the layer and
  reveals the prior committed state.
- **NCACHE-011:** Timeout or unknown commit outcome MUST remain pending
  reconciliation. It MUST NOT automatically roll back, retry, or present a
  tentative revision as committed. Later authoritative success or rejection
  resolves the same mutation ID, and a late response MUST obey NCACHE-007.
- **NCACHE-012:** Multiple optimistic layers MUST remain ordered and isolated.
  Removing one layer MUST NOT discard a later layer or overwrite a newer
  committed revision. Scope invalidation removes every unresolved layer in that
  scope.

The language-neutral fixture and its JSON Schema are
`conformance/v1/normalized-cache.json` and
`conformance/v1/normalized-cache.schema.json`. Each official SDK owns its
implementation evidence. Issue #69 owns the complete advertised-language
matrix and must not infer a language claim from the Go reference package.
