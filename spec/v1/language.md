# Composition language

This document defines the Naatre v1 operation language. The machine-readable
grammar is [language.schema.json](language.schema.json), and portable semantic
examples are in [language.json](../../conformance/v1/language.json). An
implementation claiming `core.language-1` MUST preserve every represented
construct in its typed AST. The reference Go decoder implements this complete,
closed grammar and rejects malformed or unrecognized forms before validation.

## Document grammar

- **LANG-001:** A document object contains `operations`, optional `fragments`,
  and optional duplicate-free `requires`; no other normative keys are allowed.
  Operations and fragments are arrays whose order is semantic. `requires` is
  an unordered set of required capability or extension profile identifiers and
  is sorted for canonical identity. Object member order is not semantic.
- **LANG-002:** An operation contains `name`, `kind`, optional `variables`, and
  ordered `select`. A fragment contains `name`, optional `on`, and ordered
  `select`. Language names match `[A-Za-z_][A-Za-z0-9_]{0,127}`; schema type
  references and `requires` entries use TYPE-001's
  `[A-Za-z_][A-Za-z0-9_.-]{0,127}` grammar. All names are case-sensitive; `$`
  is reserved for language tags and cannot begin an application identifier.
- **LANG-003:** Every selection and pipeline step is an object with exactly one
  recognized tag. The selection tags are `$field`, `$call`, `$pipeline`,
  `$map`, `$index`, `$slice`, `$page`, `$meta`, `$parallel`, `$fragment`,
  `$current`, `$nest`, and `$unnest`.
- **LANG-004:** Control payloads reject unknown members. Every argument value is
  an expression object with exactly one of `$literal`, `$var`, `$parent`,
  `$current`, or `$result`. Only `$literal` contains arbitrary application JSON.
  A map inside `$literal`, including one with `$`-prefixed keys, is inert data.
- **LANG-005:** A variable declaration contains `name`, `type`, optional
  `required`, optional `nullable`, and optional inert JSON `default`.
  `required` defaults to false; `nullable` defaults to false. A missing
  non-required variable remains missing unless a default exists. Variable defaults cannot
  contain executable expressions. Duplicate operation, fragment, variable,
  argument, or response names are rejected after JSON unescaping. Directive
  invocation names may repeat only when their registered descriptor is
  repeatable.
- **LANG-006:** A directive is `{ "name": Identifier, "arguments":
  Arguments? }`. The built-in `include` and `skip` directives each require one
  Boolean `if` argument. Other directives require a negotiated capability and
  a registered, bounded directive definition.
- **LANG-007:** A `$field` payload contains `name`, optional `as`, `bind`,
  `directives`, and `select`. A `$call` has the same shape plus optional `args`.
  `select` is required when the result needs further object or collection
  selection and is omitted for a leaf value.
- **LANG-008:** Response-position `$pipeline`, `$map`, `$index`, `$slice`,
  `$page`, `$current`, and `$nest` require `as`, because they have no registered
  member name. `$meta` uses its `name` unless `as` overrides it. `$parallel`,
  `$fragment`, and `$unnest` expand child response members and forbid `as` and
  `bind`.
- **LANG-009:** `$pipeline` contains a non-empty ordered `stages` array. A stage
  is one of `$field`, `$call`, `$map`, `$index`, `$slice`, `$page`, `$meta`, or
  `$current`. Stage payloads omit `as`; they may declare `bind`, `directives`,
  and the construct-specific members. Response shaping belongs to the enclosing
  pipeline.
- **LANG-010:** `$map` contains ordered `select`; `$index` contains non-negative
  integer `at`; `$slice` contains optional non-negative `start` and `end`;
  `$page` contains exactly one of `first` or `last` and at most one matching
  cursor (`after` with `first`, `before` with `last`); `$meta` contains `name`.
- **LANG-011:** `$parallel` contains ordered `select` and optional `policy`,
  whose default is `collect` and whose other value is `fail-fast`. `$fragment`
  contains `name`. `$nest` and `$unnest` contain ordered `select`.
- **LANG-012:** The complete grammar is closed for the `core.language-1`
  profile. A capability may add a separately tagged form but cannot change the
  meaning or payload of a core tag.

A field selection has one unambiguous shape:

```json
{
  "$field": {
    "name": "profile",
    "as": "author",
    "select": [{ "$field": { "name": "displayName" } }]
  }
}
```

`{ "profile": { ... } }` is invalid because arbitrary object members and
their parser iteration order never define execution.

## Expressions and data contexts

- **LANG-020:** `$literal` yields its enclosed JSON value unchanged until
  schema coercion. `$var` resolves one declared request variable. `$result`
  resolves one visible sequential binding. `$current` resolves the current
  pipeline or selection input. `$parent` resolves the immediately enclosing
  object or collection item context.
- **LANG-021:** `$parent` and `$current` payloads are exactly `true`. `$var` and
  `$result` payloads are identifiers. Expression tags are recognized only at
  expression positions; the same spelling in variable/default/application data
  has no executable meaning.
- **LANG-022:** Expression resolution never exposes a host object. Current,
  parent, variable, and result values retain their Naatre schema type and are
  coerced to the consuming argument type. A client value cannot introduce a
  callable member, field definition, directive, or result binding.
- **LANG-023:** Missing is distinct from null. Missing may satisfy an optional
  argument by leaving it absent. Null may satisfy only a nullable argument.
  Neither silently becomes the other.

## Fields, calls, and pipelines

- **LANG-100:** `$field` projects only a field registered for the static current
  type. `$call` invokes only a registered call and coerces every argument before
  execution. Aliases never participate in registry lookup.
- **LANG-101:** A pipeline starts with the current input and processes `stages`
  in array order. A stage completes and validates its output before that value
  becomes the next stage's current input. Only the final completed value is
  emitted under the pipeline's `as` response name.
- **LANG-102:** The planner validates the complete pipeline, including
  statically skipped stages, before any application handler starts. Every stage
  input, output, argument, effect, capability, and result reference must be
  compatible. An invalid stage after a write makes the whole operation invalid;
  the earlier write MUST NOT run.
- **LANG-103:** Object and interface values continue only through a registered
  field/call or explicit object selection. Lists continue through a declared
  list-level field/call or an explicit collection construct. Scalars continue
  only into a compatible call, `$current`, or a terminal pipeline result.
- **LANG-104:** Null continues only when the next stage or argument is nullable.
  Otherwise the consumer is not invoked and `RESULT_NULL` is recorded at its
  response path. Missing skips an optional consumer or records
  `RESULT_MISSING`; it is never passed to a required input.
- **LANG-105:** A stream continues only through a stream-aware registered call,
  `$map`, or a negotiated stream collection capability. `$map` over a stream
  returns a stream whose event element type is the selected item-result object;
  it does not return or materialize a list. Each source event is completed and
  framed independently, errors use event-relative selected paths, and source
  event order is preserved.
- **LANG-106:** `$current` returns the completed current value without exposing
  host methods or undeclared members. It is invalid for an object, interface,
  or union unless the plan has an explicit selected-result schema authorizing
  every emitted field.
- **LANG-107:** A field or call with child `select` emits an object assembled
  from those children. Without child `select`, its declared output must be a
  leaf or a value valid for `$current`; implicit object expansion is forbidden.

Example pipeline:

```json
{
  "$pipeline": {
    "as": "displayName",
    "stages": [
      { "$call": { "name": "user", "args": { "id": { "$var": "id" } }, "bind": "user" } },
      { "$field": { "name": "profile" } },
      { "$field": { "name": "displayName" } }
    ]
  }
}
```

## Collection scopes

- **LANG-120:** Collection-level fields and calls receive the collection value
  itself. They MUST NOT implicitly run once per item. `$map` is the only core
  construct that applies an ordered selection to every item.
- **LANG-121:** `$map` preserves input item order. Each output item is an object
  assembled from its child selections. An empty collection produces an empty
  list. A failed item produces a null placeholder and indexed errors under the
  completion rules, so later indexes never shift.
- **LANG-122:** `$index` uses a zero-based non-negative integer. An in-range
  index returns that item. An index equal to or greater than the runtime length
  emits no value and records `INDEX_OUT_OF_RANGE` at the selected response path
  followed by the requested index. A negative or non-integer `at` is a
  validation error before execution.
- **LANG-123:** `$slice` is half-open `[start,end)`. Omitted `start` is zero;
  omitted `end` is the collection length. Bounds larger than the length clamp
  to the length. `start > end` is a validation error when statically knowable
  and otherwise a path error; no handler is called to repair it. Every numeric
  `$index`, `$slice`, and `$page` bound is at most `9007199254740991`, the
  largest exact integer permitted by CANON-005's JSON number model.
- **LANG-124:** `$page` requires `collection.page-1`. Forward pagination uses
  `first` with optional `after`; backward pagination uses `last` with optional
  `before`. Counts are positive bounded integers, cursors are opaque typed
  values, and mixing directions is invalid. Page item order is server-declared
  and stable for the page snapshot.
- **LANG-125:** `$meta` requires the metadata member advertised by the
  collection type. Core names are `count`, `totalCount`, and `pageInfo`;
  `totalCount` and `pageInfo` require `collection.page-1`. Metadata selection is
  list-level and never maps over items.
- **LANG-126:** Applying `$map`, `$index`, `$slice`, `$page`, or collection
  metadata to a scalar, object, null, or missing value is a validation error
  when the static type proves it and otherwise a path error before any child
  handler starts.

This is list-level `count` beside explicit item mapping:

```json
{
  "$call": {
    "name": "users",
    "select": [
      { "$call": { "name": "count", "as": "total" } },
      { "$map": { "as": "items", "select": [{ "$field": { "name": "displayName" } }] } }
    ]
  }
}
```

## Response names, nesting, and selected-result schemas

- **LANG-200:** A field or call response name is `as` when present and its
  registered public name otherwise. Other value-producing response selections
  use the name required by LANG-008. Renaming an alias changes response and
  error paths only; it does not change lookup, binding, authorization, or
  execution order.
- **LANG-201:** Two sibling selections whose concrete type conditions can
  overlap MUST NOT produce the same response name. The only exception is when
  mutually exclusive concrete type branches statically prove that the names
  cannot coexist. Runtime last-write-wins behavior is forbidden.
- **LANG-202:** `$nest` creates one object under its required `as` name. Child
  data and error paths gain that response-name segment. `$unnest` creates no
  segment and merges only child object members into the parent.
- **LANG-203:** `$unnest` is valid only when every possible child result is an
  object and the selected-result schema proves that its response names cannot
  collide with the parent or one another. It never merges list elements,
  scalars, null, or missing, and never overwrites an existing member.
- **LANG-204:** Error paths use response names after aliasing and zero-based list
  indexes. Registry names, binding names, fragment names, and pipeline stage
  positions never replace an available response name in an error path.
- **LANG-205:** Validation derives a selected-result schema before execution.
  A field/call contributes its response name and selected output type; `$nest`
  contributes an object field; `$unnest`, `$fragment`, and `$parallel` merge
  their child fields after collision analysis; `$map` on a list contributes a
  list of child-result objects, while `$map` on a stream preserves a stream
  wrapper around that child-result type; `$slice` and `$page` preserve their
  input collection wrapper; `$index` contributes the element type; `$pipeline`
  contributes its final stage type; `$meta` contributes its registered metadata
  type.
- **LANG-206:** Selected-result schema derivation retains required versus
  optional availability, nullability, concrete type conditions, list nesting,
  aliases, and source node identity. It is consumed by generators and by
  validation; it is not inferred from example runtime data.

For a response name `users`, map item 2, nested name `profile`, and aliased
field `display`, a field failure path is `["users", 2, "profile", "display"]`.
Unnesting `profile` removes only the `profile` segment; the corresponding path
is `["users", 2, "display"]`.

## Bindings and result references

- **LANG-220:** A value-producing sequential selection or pipeline stage may
  declare `bind`. Binding names are operation-local identifiers and resolve to
  stable plan-node identity plus its completed typed value, not to an aliased
  JSON property.
- **LANG-221:** `$result` can reference only a distinct, successfully completed
  binding that precedes the consumer in the same sequential scope. Duplicate,
  self, forward, cyclic, or unknown references are validation errors.
- **LANG-222:** A binding completed before a parallel group is visible inside
  each branch. A binding created inside a branch is visible only to later
  sequential work in that branch. It is invisible to sibling branches and
  outside the group. Cross-parallel references are validation errors.
- **LANG-223:** If a previously valid binding fails at execution, dependent
  consumers do not run and record `RESULT_UNAVAILABLE`. If its directive skips
  it, consumers record `RESULT_SKIPPED`. If it completes as null, LANG-104
  applies. Independent later siblings continue under operation failure policy.
- **LANG-224:** Every authorization, constraint, cost, and completion rule is
  re-applied when a referenced value is consumed. Binding never grants broader
  authority or bypasses ownership checks.
- **LANG-225:** `as` and `bind` are independent. Renaming `as` changes only the
  output/error path. Renaming `bind` changes only matching explicit `$result`
  expressions. Neither rename changes the other namespace.

## Fragments and directives

- **LANG-240:** Fragment names are unique within a document. A named spread
  resolves one declared fragment and its optional static type condition. An
  inline fragment instead declares `on` and `select` directly, cannot declare
  `name` or `args`, and adds no response-path segment. Unknown
  fragments, impossible type conditions, and fragment cycles are validation
  errors with both definition and use source locations when available. A
  declared fragment that no spread reaches is also a validation error, located
  at its declaration; a fragment reachable only from an unreached fragment is
  itself unreached.
- **LANG-241:** Fragment expansion preserves spread position and the fragment's
  internal selection order. It introduces no response-path segment and no
  binding scope. Bindings in a fragment behave as if its selections were
  written at the spread site.
- **LANG-244:** A fragment may declare typed `parameters` using the operation
  variable declaration shape. A named spread binds them through `args` under
  the negotiated `language.fragment-parameters-1` capability. Bindings are
  coerced at the spread site; an explicit argument wins over a declaration
  default, missing differs from null, and a required parameter without either
  is invalid. Parameters shadow same-named operation or enclosing-fragment
  variables only within that expansion. Unknown arguments and unknown or
  non-input parameter types are invalid. Expansion counts every copied
  selection against a portable budget of 16,384 and permits at most 128 active
  named expansions; exceeding either limit fails validation before execution.
- **LANG-242:** Directives gate or annotate their selection but do not hide it
  from static validation. `include(if: false)` and `skip(if: true)` prevent the
  selection and its handlers from starting. Their variables and arguments are
  still coerced before execution admission.
- **LANG-243:** A directive cannot introduce a write into a query, relax
  authorization, raise a resource limit, mutate aliases/bindings, change array
  order, or make an invalid branch valid. Directive evaluation order follows
  the directive array, never object member order.
- **LANG-245:** Every non-standard directive resolves by its exact registered
  name to one immutable descriptor containing a stable ID, implementation
  version, required capability, repeatability, allowed selection locations,
  typed arguments, lifecycle phases, effect, declared cost, determinism, and
  compatibility behavior. Unknown directives, duplicate invocations of a
  non-repeatable directive, invalid locations, and missing or unknown arguments
  fail validation. Directive arguments use the same missing/null/default and
  schema-coercion rules as call arguments.
- **LANG-246:** A document using a non-standard directive MUST list the
  descriptor's exact capability and every additional required capability in
  `requires`; the request MUST negotiate each one. The descriptor version and
  capability are part of schema identity, so changing either requires a new
  schema approval and invalidates an older semantic pin. `naatre.*`, `core.*`,
  `include`, and `skip` are reserved for standard definitions.
- **LANG-247:** Validation metadata is applied before planning. A planning
  callback receives only an immutable public node view and request-stable,
  coerced arguments, MUST declare deterministic behavior, and can return only
  `skip` plus a non-negative bounded cost contribution. Runtime-dependent planning arguments, callback failure or
  panic, integer overflow, any one directive above 1,048,576 cost units, or an
  operation whose directive total exceeds that bound fail validation. The
  closed decision shape cannot add a node, field, mutation, alias, binding,
  authorization rule, or resource permission.
- **LANG-248:** An execution wrapper runs only for a call or field after dynamic
  authorization, argument validation, cancellation checks, and execution-slot
  admission. Its continuation closes over the original context, accepts no
  replacement context, is one-shot, and closes when the wrapper returns.
  In-flight continuation work is accounted until it exits. Calling the
  continuation twice fails as an internal extension error even when the
  wrapper suppresses the second call's error, and MUST NOT execute business
  work twice. Cancellation and authorization outcomes remain runtime-owned
  even when a wrapper returns a value or suppresses its own error.
- **LANG-249:** A response annotator receives no mutable response value. It may
  return one strict JSON annotation identified by the directive's stable ID,
  name, version, and response path. Annotation JSON is canonicalized, callback
  failures fail the selected value safely, and annotations sort by response
  path then directive source position so parallel completion timing cannot
  change their order. Annotators remain subject to execution admission,
  cancellation, and abandonment accounting. In-process callbacks are trusted
  application code; code hostile beyond these API invariants requires an
  external isolation boundary.

## Sequential and parallel effects

- **LANG-300:** Operation selections, fragment expansions, child selections,
  and pipeline stages are sequential in declaration order unless enclosed by an
  explicit `$parallel` group.
- **LANG-301:** A parallel group admits only branches proven independent under
  references, effects, transaction, authorization, thread-safety, and
  completion barriers. Parallelism is bounded by the negotiated request limit;
  nested groups share that bound.
- **LANG-302:** `collect` admits eligible independent branches and retains every
  result/error. `fail-fast` stops new admission after a terminal branch failure
  and joins runtime-owned work. Both assemble data by declaration and errors by
  response path then source location, never completion time.
- **LANG-303:** Queries are transitively read-only through fragments,
  directives, pipelines, mappings, metadata, and references. Mutations are
  sequential by default. Parallel mutation requires explicit registry metadata
  and the negotiated `mutation.parallel-1` profile; a client tag alone cannot
  enable it.
- **LANG-304:** Static validation traverses all branches even when a Boolean
  directive currently skips them. Any invalid type, reference, collision,
  capability, or effect rejects the operation before the first application
  handler starts.

## Edge-state outcomes

| State | Valid continuation | Required outcome |
| --- | --- | --- |
| Empty list | `$map`, `$slice`, or `count` | Empty list, empty slice, or zero; no fabricated item. |
| Null | Nullable stage/argument | Consumer receives typed null. |
| Null | Non-null stage/argument | Consumer does not run; `RESULT_NULL`. |
| Missing | Optional argument | Argument remains absent. |
| Missing | Required stage/argument | Consumer does not run; `RESULT_MISSING`. |
| Scalar | Compatible call or terminal `$current` | Continues with its declared scalar type. |
| Scalar | Object field or collection construct | Validation error before execution. |
| Out-of-range index | `$index` on a runtime-short list | No value; indexed `INDEX_OUT_OF_RANGE` path error. |
| Failed binding | `$result` consumer | Consumer does not run; `RESULT_UNAVAILABLE`. |
| Skipped binding | `$result` consumer | Consumer does not run; `RESULT_SKIPPED`. |
| Stream | Stream-aware call or `$map` | Ordered event-wise continuation within bounds. |

## Complete request and response example

The document below uses an explicit map and nest. Reordering any object members
without changing an array leaves its meaning unchanged.

```json
{
  "version": "1",
  "operation": "Users",
  "variables": { "active": true },
  "document": {
    "operations": [{
      "name": "Users",
      "kind": "query",
      "variables": [{ "name": "active", "type": "Boolean" }],
      "select": [{
        "$call": {
          "name": "users",
          "args": { "active": { "$var": "active" } },
          "select": [{
            "$map": {
              "as": "items",
              "select": [
                { "$field": { "name": "id" } },
                { "$nest": { "as": "profile", "select": [
                  { "$field": { "name": "displayName", "as": "display" } }
                ] } }
              ]
            }
          }]
        }
      }]
    }]
  }
}
```

```json
{
  "requestId": "server-1",
  "data": {
    "users": {
      "items": [
        { "id": "u-1", "profile": { "display": "Ada" } },
        { "id": "u-2", "profile": { "display": "Lin" } }
      ]
    }
  },
  "capabilities": [],
  "extensions": {}
}
```

## Phase and diagnostic examples

- A pipeline whose final call expects `Int32` but receives a prior `String`
  output fails validation with `TYPE_MISMATCH`; handler starts are zero,
  including any preceding write.
- A self or forward `$result` fails validation with
  `RESULT_FORWARD_REFERENCE`; handler starts are zero.
- A sibling-parallel `$result` fails validation with `RESULT_SCOPE`; handler
  starts are zero.
- A runtime-short `$index` is structurally valid, but execution records
  `INDEX_OUT_OF_RANGE` at the aliased response path plus the requested index.
- A completed binding that later becomes unavailable records
  `RESULT_UNAVAILABLE` at each dependent consumer without re-running the
  producer.

The portable fixtures distinguish these validation failures from path-specific
execution outcomes and state the expected number of application handler starts.

## Acknowledgement

Naatre uses ideas found in Deepr and other composition/query systems, but this
grammar is independently defined. It neither copies Deepr syntax nor claims
wire, source, behavioral, or migration compatibility.
