# Composition language

## Document grammar

- **LANG-001:** A document object contains `operations` and optional
  `fragments`; no other normative keys are allowed.
- **LANG-002:** Operations contain `name`, `kind`, optional variable
  declarations, and ordered `select`. Operation and fragment names match
  `[A-Za-z_][A-Za-z0-9_]{0,127}`; `$` is reserved.
- **LANG-003:** Every selection is a one-key tagged object. Tags are `$field`,
  `$call`, `$pipeline`, `$map`, `$index`, `$slice`, `$page`, `$meta`,
  `$parallel`, `$fragment`, `$current`, `$nest`, and `$unnest`.
- **LANG-004:** Control objects reject unknown members. Literal values use a
  `$literal` tagged form at expression positions so application maps that look
  like expressions remain inert.
- **LANG-005:** Arguments accept literals, `{ "$var": "name" }`,
  `{ "$parent": true }`, `{ "$current": true }`, and
  `{ "$result": "stageName" }`. No value supplied by a client becomes a
  server object or executable handler.

Valid field selection:

```json
{"$field":{"name":"profile","as":"author","select":[{"$field":{"name":"displayName"}}]}}
```

Invalid: `{"profile": {...}}`, because object member order and arbitrary keys
cannot define execution.

## Composition

- **LANG-100:** `$field` projects a registered field from the current object.
  `$call` invokes a registered handler with typed arguments.
- **LANG-101:** `$pipeline` is an ordered, non-empty stage array. Each completed
  stage becomes the current value of the next. Scalar, null, missing, object,
  list, and stream continuation is accepted only when the next stage declares a
  compatible input type.
- **LANG-102:** Collection-level fields and calls consume the list itself. They
  MUST NOT implicitly map. `$map` alone applies its selection to each item.
- **LANG-103:** `$index` uses a zero-based non-negative index. Out-of-range is a
  path error, negative indexes are validation errors. `$slice` uses bounded
  half-open `[start,end)` indexes. `$page` and `$meta` require advertised
  collection capabilities.
- **LANG-104:** `$nest` places its child object beneath one response name.
  `$unnest` merges only statically conflict-free object fields. It never merges
  list elements or overwrites a response name.
- **LANG-105:** `$current` returns the completed current value without exposing
  methods or unregistered fields.

Valid: a list-level `count` call followed by `$current`; an explicit `$map`
selecting `name` on every item; indexing an empty list produces one path error.

Invalid: treating a list field selection as implicit item mapping, using a
negative index, or unnesting a scalar/list.

## Names, references, and conflicts

- **LANG-200:** Response names are aliases when present and registered public
  names otherwise. Two selections whose concrete type conditions may overlap
  MUST NOT produce the same response name.
- **LANG-201:** A sequential selection or pipeline stage MAY assign `bind`.
  `$result` references only a prior, successfully completed sequential binding
  by stable node identity. Forward, cyclic, failed, skipped, cross-parallel,
  and nullable-without-compatible-input references are invalid or produce the
  specified path error before their consumer executes.
- **LANG-202:** Renaming an output alias does not rename a binding; renaming a
  binding changes only its explicit `$result` consumers.
- **LANG-203:** Parallel-group branches MUST NOT have implicit dependencies.
  Bindings created within a group are not visible to siblings or outside it.

Valid: stage `loadUser` binds `user`, and a later sequential call supplies
`{"$result":"user"}` to a compatible argument.

Invalid: a branch references a sibling parallel result or a call consumes the
failed result of a prior write.

## Parallelism and effects

- **LANG-300:** `$parallel` contains ordered `select`, policy `collect` or
  `fail-fast`, and no implicit data dependency. Nested groups share the request
  concurrency bound.
- **LANG-301:** Sequential and parallel validation is transitive through
  fragments, directives, nested selections, and pipelines.
- **LANG-302:** A selection incompatible with operation kind or handler effect
  is rejected before any handler runs, even when a condition currently skips
  it.

Valid: two independent, thread-safe reads in a bounded parallel group.

Invalid: a serial mutation inside a parallel group or a skipped fragment that
contains a mutation inside a query.
