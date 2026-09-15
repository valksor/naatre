# Go normalized cache

Package `normalizedcache` is the opt-in Go reference implementation of
`sdk.normalized-cache-1`. Constructing a cache requires an immutable schema
snapshot. The existing `client` package remains raw-response-first and has no
implicit cache.

Entity references are derived only from schema `entity.keys` and expose hashes,
not raw key values. Cache storage additionally binds subject, tenant, and
authorization revision. Field identities bind arguments, locale,
representation, and schema revision, so aliases and response locations do not
change entity identity while pagination windows remain separate.

Committed patches require opaque revision lineage plus an ordered transport
position. The default conflict policy rejects stale or divergent revisions.
Partial absent/skipped/pending/failed states preserve known values; explicit
null is separate. Unknown fields remain available to the caller's raw response
but are not normalized.

Optimistic updates are layered over committed state. Confirmed rejection rolls
back one layer. Timeout and unknown commit outcomes remain pending until an
authoritative success or rejection reconciles the same mutation ID. Scope
invalidation clears cached data and optimistic layers and invokes registered
live-subscription cancellation callbacks.
