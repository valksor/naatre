# Cursor pagination and collection metadata

`collection.page-1` defines bounded, opaque cursor pagination without requiring
Relay's edge/node response shape. It extends the collection constructs in
`core.language-1`; this document is normative when the capability is declared.

## Collection contract

- **COLL-001:** A pageable collection MUST advertise a positive provider
  maximum page size. `first` and `last` are positive integers no greater than
  both that maximum and the request resource limit. The planner MUST reject an
  excessive size before any application handler starts.
- **COLL-002:** A request supplies exactly one of `first` or `last`. `after` is
  valid only with `first`; `before` is valid only with `last`. Invalid mixes
  fail validation before execution.
- **COLL-003:** A page result contains `items` and `pageInfo`.
  `pageInfo` contains `hasNextPage`, `hasPreviousPage`, and, for a non-empty
  page, opaque `startCursor` and `endCursor`. An empty page has no cursors. A
  runtime that cannot create and consume scoped opaque cursors MUST reject
  `$page` during validation rather than return an unwrapped list.
- **COLL-004:** `totalCount` is OPTIONAL, MUST be selected explicitly, and MAY
  have cost and authorization metadata distinct from item traversal. A
  provider MUST NOT compute it when it was not selected. In the core language,
  `$meta totalCount` is a list-level selection alongside `$page`, so its
  projected response value is a sibling of the page result. Provider APIs MAY
  carry the same explicitly requested value in a `CollectionPage` object.
- **COLL-005:** `edges` is OPTIONAL. When present, each edge contains its cursor,
  the corresponding selected item, and provider-declared edge metadata. An
  ordinary collection never needs to expose edges.

## Ordering and boundaries

- **COLL-010:** The provider declares one total stable order. Every position
  consists of the declared sort key followed by a stable unique tie-breaker.
  Duplicate composite positions are invalid provider output.
- **COLL-011:** Forward pagination returns entries strictly after `after`;
  backward pagination returns entries strictly before `before`. Both preserve
  the provider's ascending declared order. A deleted boundary item remains a
  valid position: paging resumes by comparison with its composite position,
  never by a numeric offset or by searching for the deleted row.
- **COLL-012:** Inserts before a live cursor do not move its boundary. Inserts
  after it can appear on a later page. An empty interval returns an empty
  `items` list with truthful page flags and no cursor fields.

## Cursor protection and scope

- **COLL-020:** A cursor is an opaque token or server-stored reference. Base64
  encoding alone supplies neither integrity nor confidentiality. A self-
  contained cursor MUST be integrity-protected; payload encryption is OPTIONAL
  when revealing the protected fields is unacceptable.
- **COLL-021:** Cursor validity is bound to collection identity, canonical
  filters, canonical sort, direction, tenant, authorization scope, snapshot
  policy, snapshot identity when applicable, cursor version, key identifier,
  and expiry. Changing any bound value invalidates the cursor.
- **COLL-022:** Every malformed, tampered, expired, retired-key, or
  scope-mismatched cursor fails with the same safe public code,
  `INVALID_CURSOR`. The runtime MUST NOT disclose which verification failed.
- **COLL-023:** Cursor verification occurs before application handlers start.
  Cursor creation occurs only after row authorization. Page flags,
  `totalCount`, edge metadata, and cursor positions MUST be derived only from
  rows visible in the cursor's authorization scope.
- **COLL-024:** Key rotation assigns an explicit key identifier. Retained old
  keys MAY verify existing cursors until expiry; removing a key invalidates all
  cursors signed by it. Decoders reject unknown versions and unknown keys.

## Consistency and cost

- **COLL-030:** A live collection offers best-effort continuation across
  writes. Stable composite boundaries avoid offset drift, but the profile does
  not promise zero duplicates or omissions when rows can change their sort
  position between requests.
- **COLL-031:** A snapshot collection binds every cursor to a provider snapshot
  identity and MUST read subsequent pages from that same snapshot. A stale or
  unavailable snapshot fails safely; silently falling back to live data is
  forbidden.
- **COLL-032:** Static cost multiplies all per-item descendant costs by the
  requested `first` or `last` cardinality. `totalCount` adds its separately
  advertised cost. Arithmetic overflow is a validation failure, never a
  wrapped or reduced cost.

## Typed filtering and capability discovery

`collection.query-1` is optional. It uses the `collection.query` schema
descriptor as its single portable authority; provider column names and
expressions never appear in discovery.

- **COLL-100:** A callable declaring `collection.query-1` MUST publish a query
  descriptor with stable authorized field IDs, explicit public paths, types,
  nullability and missing-value support, filter operators, sort semantics,
  per-predicate cost and bounded limits. The descriptor and capability MUST be
  present or absent together. A selectable field is not filterable or sortable
  unless it is separately declared in this descriptor.
- **COLL-101:** A filter is a closed tagged AST. `and` and `or` contain a
  non-empty `children` array; `not` contains one `child`; `predicate` contains
  a declared `field`, `operator`, and a `value` when the operator requires one;
  `any` and `all` contain a declared list `field` and one nested `predicate`.
  A value contains a schema `type` and exactly one JSON `literal` or variable
  name. Variables are resolved and type-checked before provider invocation.
  Unknown members, undeclared field IDs and arbitrary property traversal are
  invalid. `@` refers only to the current declared list element scope.
- **COLL-102:** The core operator set is `eq`, `ne`, `lt`, `lte`, `gt`, `gte`,
  `in`, `not-in`, `is-null`, `is-not-null`, `exists`, `not-exists`, `any`, and
  `all`. Equality and membership apply to non-collection portable scalar or
  enum values. Ordered comparison applies only to numeric, string, ID, enum,
  timestamp, duration and UUID values under the field's declared collation.
  Null predicates require `nullable`; existence predicates require `missing`;
  `any` and `all` require a declared list and its exact element type.

  | Operators | Compatible declared field type | Value |
  | --- | --- | --- |
  | `eq`, `ne` | any non-list portable scalar or enum | one exact field-typed value |
  | `in`, `not-in` | any non-list portable scalar or enum | a list of exact field-typed values |
  | `lt`, `lte`, `gt`, `gte` | `String`, `ID`, `Int32`, `Float64`, `Int64`, `UInt64`, `BigInt`, `Decimal`, `Timestamp`, `Duration`, `UUID`, or enum | one exact field-typed value |
  | `is-null`, `is-not-null` | any field declared `nullable` | none |
  | `exists`, `not-exists` | any field declared `missing` | none |
  | `any`, `all` | list whose declared element is a portable scalar or enum | one nested predicate over `@` at the exact element type |
  | `regex`, `full-text` | `String`, with the matching optional capability | one `String` value |
  | `geo-within` | a schema-declared custom scalar, with `collection.geospatial-1` | one exact field-typed value |

  `Boolean` and `Bytes` support equality and membership but not ordered
  comparison. Lists support only `any` and `all`;
  object, input-object, union, interface, and trait types support none of these
  operators. Implementations MUST reject a descriptor whose operator/type
  pairing differs from this table.
- **COLL-103:** Filtering is two-valued. For a missing field, comparisons,
  membership, `is-null`, and `is-not-null` are false, `exists` is false, and
  `not-exists` is true. For explicit null, comparisons and membership are
  false, `is-null` and `exists` are true, and `is-not-null` and `not-exists`
  are false. For a present non-null value, `is-null` and `not-exists` are
  false while `is-not-null` and `exists` are true. `not` performs ordinary
  Boolean negation of these results; host or SQL three-valued behavior MUST
  NOT leak into the portable result.
- **COLL-104:** Query capability discovery is authorization-filtered by stable
  query-field identity. Hidden declarations, operators and backend mappings
  MUST be absent. Requests for an unavailable field or operator fail before
  any backend call with a generic stable code that does not repeat or suggest
  hidden names.

## Lists, limits, and optional operators

- **COLL-110:** `any` evaluates its nested predicate in the list-element scope
  and is true when at least one element matches; `all` is true when every
  element matches. For an empty present list, `any` is false and `all` is true.
  Both are false for a missing or null list. Nested scopes cannot join to an
  outer object or traverse a path not explicitly declared by the schema.
- **COLL-111:** Validation counts AST depth, all predicate nodes, every
  membership element, each ordered sort key, and declared predicate cost.
  Exceeding `maxDepth`, `maxPredicates`, `maxMembership`, `maxSortKeys`, or
  `maxCost`, including by integer overflow, fails before backend invocation.
  Unindexed predicates MUST declare their full worst-case cost and remain
  subject to provider runtime read, duration, cancellation and memory limits.
- **COLL-112:** `regex`, `full-text`, and `geo-within` are outside the core
  operator set. A field may advertise one only with `collection.regex-1`,
  `collection.full-text-1`, or `collection.geospatial-1` respectively and with
  separately bounded syntax, input size, work and cancellation semantics. A
  provider lacking that exact capability rejects the predicate during
  preflight and MUST NOT approximate it.

## Ordered sorts and cursor identity

- **COLL-120:** Sort input is an ordered non-empty array of declared field IDs.
  Every term specifies `asc` or `desc` and `first` or `last` null placement.
  Duplicate keys and more than `maxSortKeys` keys are invalid. Missing and
  explicit null are both nullish for ordering, while remaining distinguishable
  to filter predicates.
- **COLL-121:** Each sortable field declares one exact collation and case
  policy. Core collations are numeric, binary byte order, and Unicode scalar
  value order (`unicode-code-point`), all case-sensitive. Case-insensitive
  behavior requires `collection.case-fold-1` and a separately versioned fold
  table. Numeric types and `Duration` require `numeric`; `String`, `ID`, enum,
  `Timestamp`, and `UUID` require `binary` or `unicode-code-point`.
  Case-insensitive ordering is valid only for `String`, `ID`, and enum values.
  The tie-breaker follows the same compatibility rules. Providers MUST NOT
  inherit a database, locale, or host default.
- **COLL-122:** The provider appends one stable unique tie-breaker with a
  declared portable type and collation after all client sort terms. It rejects
  duplicate composite positions. The tie-breaker need not expose a backend
  field name and clients cannot remove or reverse it.
- **COLL-123:** Cursor identity hashes the canonical typed filter AST, the
  ordered sort AST, tenant, authorization, pagination policy and snapshot in
  addition to the bindings in COLL-021. Reordered JSON object members are
  equivalent; a changed literal, variable result, sort term, tenant or
  snapshot is not.

## Provider translation

- **COLL-130:** A provider accepts only a validated typed query and an explicit
  server-side field mapping. Unsupported predicates fail before backend I/O;
  providers MUST NOT silently fetch and filter an unbounded collection in
  memory. A provider claiming support MUST match the reference evaluator for
  missing, null, empty lists, Unicode, numeric boundaries and stable sorting.
- **COLL-131:** SQL providers bind every literal and variable as a parameter.
  SQL-looking strings remain ordinary values and MUST NOT change statement
  structure. Only server registration supplies quoted column or expression
  mappings; the client AST never supplies SQL identifiers, fragments or joins.
- **COLL-132:** Provider cancellation and runtime resource limits remain in
  force during translation and execution. Public failures use stable safe
  codes and omit credentials, hidden metadata, SQL text and implementation
  details; diagnostic causes remain server-side.

The portable boundary and failure matrix is
[`conformance/v1/collections.json`](../../conformance/v1/collections.json).
