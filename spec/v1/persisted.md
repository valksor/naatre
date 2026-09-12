# Persisted and allowlisted operations

The `core.persisted-1` profile separates immutable document content identity
from a deployment's mutable approval record. Its portable identity, mode,
approval, and lifecycle vectors are
[persisted.json](../../conformance/v1/persisted.json).

## Document identity

- **PERSIST-001:** A persisted reference is exactly the CANON-201 digest record
  for purpose `document`: algorithm `sha-256`, canonicalization version
  `c14n-1`, and a lowercase 64-hex-character digest. Unknown algorithms,
  versions, casing, lengths, or members fail closed.
- **PERSIST-002:** The hash input is the versioned canonical semantic document
  AST, not request-envelope bytes, source JSON whitespace, a plan, generated
  code, or an application storage key. Object-member order is normalized while
  every ordered selection, pipeline stage, and parallel branch retains order.
- **PERSIST-003:** Operation and fragment names, operation kinds, variable
  declarations and canonical defaults, required capabilities and extensions,
  directives, type conditions, arguments, aliases, bindings, collection
  controls, and relevant schema-defined scalar spellings remain in document
  identity. Canonicalization MUST NOT erase apparently redundant work or apply
  general program-equivalence reasoning.
- **PERSIST-004:** Runtime variable values, request and correlation identifiers,
  principals, claims, authorization tokens, deadlines, tracing, approval
  metadata, and transport fields are excluded from document identity. Runtime
  variables remain inputs to result-cache and idempotency identities where
  those profiles require them.
- **PERSIST-005:** Producers that need schema-directed coercion of variable
  defaults MUST pin the schema used to create the canonical document. A schema
  or policy update can invalidate approval without changing an unchanged
  document digest.

## Approval records

- **PERSIST-100:** A persisted record binds the document reference and canonical
  bytes to one tenant partition, human-readable name, approval revision, owner,
  selected operation name and kind, schema revision, required capabilities,
  authorization-policy revision, approved static cost, cache classification,
  optional expiry, and lifecycle state.
- **PERSIST-101:** Content identity and approval identity are independent. A
  record update never changes its document digest; a changed canonical document
  is registered under a new digest. Approval records MAY use the CANON-221
  approval identity but MUST still compare every bound field before reuse.
- **PERSIST-102:** Registration recomputes the document digest from stored
  canonical bytes, decodes the document strictly, selects the bound operation,
  and verifies its kind and required capabilities. A reference/document,
  operation, kind, or capability mismatch is rejected before storage.
- **PERSIST-103:** Admission revalidates the complete plan under the current
  immutable schema and policy configuration. Planned static cost MUST NOT
  exceed approved cost. Cached plans do not bypass record, revision,
  revocation, expiry, tenant, or cost checks.
- **PERSIST-104:** Private cache classification is the default. Shared-cache
  classification is valid only when a versioned server-side approval declares
  both public output and authorization invariance for every possible input.
  Query kind, determinism, persistence, GET eligibility, or a client assertion
  alone can never make a record shared-cache eligible.

## Admission modes

- **PERSIST-110:** Lookup-only and register-at-deploy modes reject an inline
  document with `INLINE_DOCUMENT_FORBIDDEN` before typed document decoding,
  validation, planning, authorization, or handler execution. Parsing the outer
  JSON envelope only to identify its source is permitted and remains bounded.
- **PERSIST-111:** Lookup-only mode performs reads only. A miss never registers,
  reapproves, migrates, or changes cache classification. Register-at-deploy mode
  accepts explicit validated records through a deployment control plane and
  rejects runtime registration.
- **PERSIST-112:** Automatic registration is disabled by default. When enabled,
  one authenticated principal and matching tenant are required. The server
  applies an authenticated registration quota before planning, validates the
  document and exact cost, obtains an explicit approval decision, and performs
  collision-safe compare-before-store. Denial at any step stores nothing and
  starts no business handler.
- **PERSIST-113:** An automatically created record binds the current schema and
  authorization-policy revisions, exact planned cost, selected operation and
  kind, document-required capabilities, authenticated owner and tenant, and a
  configured automatic-approval revision. It defaults to private caching;
  automatic registration MUST NOT infer public output.
- **PERSIST-114:** A decoded persisted request is resolved to an isolated copy
  of canonical document bytes while preserving only its validated operation,
  variables, capabilities, extensions, and correlation metadata. Conflicting
  request and stored operation names are rejected.

## Storage and tenant isolation

- **PERSIST-200:** The storage interface provides context-aware register,
  lookup, revoke, compare-and-swap migrate, and evict operations without
  prescribing a database. Stored and returned records are immutable copies;
  caller mutation cannot alter a record already admitted or stored.
- **PERSIST-201:** The storage key contains the tenant partition and complete
  algorithm/version/digest reference. Identical document bytes in two tenants
  produce the same document digest but distinct records. A lookup from another
  tenant uses the same safe not-found shape and MUST NOT reveal existence.
- **PERSIST-202:** Registering the same complete record is idempotent.
  Re-registering one tenant/reference with different canonical document bytes
  is `PERSISTED_HASH_COLLISION`; changing approval metadata without an explicit
  migration is `PERSISTED_CONFLICT`.
- **PERSIST-203:** Storage calls observe cancellation. Concurrent registration,
  lookup, revocation, migration, and eviction are data-race-free and have one
  linearization point per operation. Storage failures are wrapped as safe typed
  errors while retaining an internal cause for diagnostics.

## Revocation, expiry, migration, and hooks

- **PERSIST-300:** Revocation takes effect for new admissions when the storage
  revoke operation linearizes. A request whose record and plan were completely
  admitted before that point MAY finish under its captured snapshot; revocation
  does not claim that already-running work stopped or rolled back.
- **PERSIST-301:** Expiry equality is expired. Expiry, revocation, schema
  revision mismatch, policy revision mismatch, cost excess, and incompatible
  capability or operation metadata reject before business execution.
- **PERSIST-302:** Migration is compare-and-swap on the prior approval revision,
  keeps the same tenant, digest, and canonical document bytes, revalidates every
  replacement field against current schema and policy, and emits a new approval
  revision. A changed document requires a new persisted reference.
- **PERSIST-303:** Successful registration, revocation, migration, and eviction
  emit ordered invalidation events containing kind, key, approval revision when
  applicable, and event time. Cache and audit integrations consume these hooks;
  they MUST NOT treat a request identifier or cache entry as approval authority.
- **PERSIST-304:** Record caches observe revocation, migration, and eviction no
  later than the corresponding successful operation. A deployment requiring
  durable audit or cross-process invalidation supplies a store and event sink
  with those guarantees; the in-memory reference establishes only in-process
  ordering.

## Safe failures and conformance

- **PERSIST-400:** The closed core failure families include not found, malformed
  record, algorithm mismatch, document mismatch, hash collision, metadata
  conflict, operation/kind/capability mismatch, stale schema or policy, cost
  excess, tenant mismatch, revoked, expired, revision conflict, mode denial,
  automatic authentication/quota/approval denial, and storage failure. Public
  messages are generic and never include document bytes, variables, tenant,
  principal, policy detail, or storage causes.
- **PERSIST-401:** `core.persisted-1` implementations consume the portable
  fixture directly. They MUST reproduce every fixed digest, equivalence and
  difference relation, identity exclusion, mode rule, cache-approval rule, and
  lifecycle code. SDK-specific hashing evidence remains with each SDK and the
  complete cross-language comparison remains in the release gate.
