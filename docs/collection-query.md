# Typed collection queries

`collection.query-1` is the optional filter and sort profile defined by
`spec/v1/collections.md`. The portable schema descriptor is the authority for
clients and generators: it contains only authorized public field IDs, paths,
types, operators, sort policies, costs, and limits. Backend columns remain in
server registration.

The profile is available in the Go 1.27 reference packages `schema`,
`collectionquery`, and `runtime`. The minimum provider evidence uses the pure-Go
`modernc.org/sqlite` v1.58.0 driver. It proves the SQLite translation only; it
does not advertise PostgreSQL, MySQL, full-text, geospatial, regex, remote
provider, or generated SDK support. Those broader implementations and mappings
belong to issue #108, and the complete advertised-language matrix belongs to
issue #69.

## Request shape

The registered input type decides where an application places `filter` and
`sort`. Their values are portable. This example combines a typed filter,
ordered multi-key sort, aliased page result, and explicitly selected count:

```json
{
  "operations": [{
    "name": "Adults",
    "kind": "query",
    "variables": [{"name": "minimumAge", "type": "Int64"}],
    "select": [{
      "$call": {
        "name": "users",
        "args": {
          "query": {
            "$literal": {
              "filter": {
                "kind": "and",
                "children": [
                  {
                    "kind": "predicate",
                    "field": "User.age",
                    "operator": "gte",
                    "value": {"type": "Int64", "variable": "minimumAge"}
                  },
                  {
                    "kind": "predicate",
                    "field": "User.name",
                    "operator": "is-not-null"
                  }
                ]
              },
              "sort": [
                {"field": "User.name", "direction": "asc", "nulls": "last"},
                {"field": "User.age", "direction": "desc", "nulls": "first"}
              ]
            }
          }
        },
        "select": [
          {"$page": {"as": "people", "first": 25, "select": [{"$field": {"name": "id"}}]}},
          {"$meta": {"name": "totalCount", "as": "matched"}}
        ]
      }
    }]
  }]
}
```

The application supplies the same resolved variable map to the provider and to
`runtime.NewCollectionQueryCursorScope`. The cursor helper replaces variable
references with canonical resolved literals before hashing the typed filter and
ordered sort ASTs, then binds them to the tenant, authorization, pagination
policy, and snapshot. Reusing it after any of those values changes returns the
same safe `INVALID_CURSOR` result.

## Provider boundary

`collectionquery.SQLiteTranslator` accepts a `schema.CollectionQueryDescriptor`
and a server-owned map from public field IDs to SQLite value/presence columns.
The presence column preserves the difference between a missing field and an
explicit SQL null. List fields must be explicitly mapped as JSON lists. The
translator validates the complete AST and budgets before producing SQL, quotes
registered column identifiers, and returns literals only in `Arguments`.
The reference SQLite adapter accepts only scalar and enum representations it
can bind and order without approximation. In particular, `UInt64`, `BigInt`,
`Decimal`, and custom scalar predicates/sorts are rejected as
`FILTER_UNSUPPORTED`; a provider with an explicit exact storage contract may
implement those types separately.

`collectionquery.SQLiteProvider.Select` performs the same preflight before it
calls `QueryContext`. Unavailable fields or operators therefore cannot trigger
backend I/O or reveal the requested name. Backend errors are exposed only as
`FILTER_PROVIDER_FAILED`; SQL text and driver details remain available solely
through the wrapped server-side cause.

Reference evaluation and SQLite translation intentionally share only the
validated portable AST, not evaluation code. Their parity suite covers missing
versus null, empty-list `any`/`all`, Unicode scalar ordering, signed 64-bit
boundaries, stable tie-breaking, and parameter binding. Neither implementation
falls back to loading and filtering an unbounded collection in memory.

## Reproducible checks

From the repository root at the dependency revision recorded by `go.mod` and
`go.sum`:

```sh
go test ./collectionquery -count=1
go test ./internal/conformance -run TestCollectionQueryLanguageNeutralContract -count=1
go test ./runtime -run TestTypedCollectionQueryCursorRejectsEveryScopeMismatch -count=1
```

The normative vectors are in `conformance/v1/collections.json`; their exact
digest is pinned by `conformance/v1/suite.json`.
