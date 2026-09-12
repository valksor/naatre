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

The portable boundary and failure matrix is
[`conformance/v1/collections.json`](../../conformance/v1/collections.json).
