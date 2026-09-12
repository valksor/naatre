# Core execution model

## Vocabulary

- **CORE-001 Request:** one transport-independent request envelope.
- **CORE-002 Document:** an immutable description containing one or more named
  operations and reusable fragments.
- **CORE-003 Operation:** a declared query, mutation, or subscription and its
  ordered selection array.
- **CORE-004 Selection:** one field, call, pipeline, collection operation,
  parallel group, fragment spread, directive application, or current-value
  projection.
- **CORE-005 Field:** a registered, typed projection from a current value.
- **CORE-006 Call:** a registered, typed handler invocation with arguments.
- **CORE-007 Collection:** a list value whose list-level operations are
  distinct from explicit per-item mapping.
- **CORE-008 Alias:** a validated response name replacing a selection's public
  name. It is not an application identifier.
- **CORE-009 Variable:** request-bound input referenced by a document.
- **CORE-010 Fragment:** a statically validated reusable selection array.
- **CORE-011 Directive:** a registered, bounded language extension that cannot
  override core sequencing, effects, authorization, or limits.
- **CORE-012 Capability:** a versioned feature negotiated by client and server.
- **CORE-013 Profile:** a versioned collection of mandatory clauses and
  fixtures used to make a conformance claim.
- **CORE-014 Missing:** absence of a value. Missing is not JSON null.
- **CORE-015 Error path:** an array of aliased response names and list indexes
  identifying one result location.
- **CORE-016 Query:** an operation whose complete transitive selection graph is
  registered read-only. Query is an effect constraint, not an HTTP method.
- **CORE-017 Mutation:** an operation whose top-level selections execute in
  declared order and may reach registered write effects.
- **CORE-018 Subscription:** an operation that establishes a read-like source
  and delivers an ordered event sequence under a streaming profile.
- **CORE-019 Effect:** trusted server registration metadata classifying a
  handler as read or write independently of operation kind and scheduling.
- **CORE-020 Transport method:** the carrier-specific method used to exchange a
  request. It does not determine operation kind, effect, or execution order.
- **CORE-021 Partial data:** response data that preserves every successfully
  completed location while representing other locations as unavailable under
  the error and completion rules.

## Phases

- **CORE-100:** Processing MUST proceed through decode, validate, plan,
  authorize, execute, complete, and serialize phases. A later phase MUST NOT
  repair a failure from an earlier phase.
- **CORE-101:** Decode constructs syntax only. It MUST NOT resolve application
  handlers, run custom coercers, or perform I/O.
- **CORE-102:** Validate and plan MUST examine the entire selected operation,
  including statically skipped branches. They MUST NOT invoke business
  handlers. Pure, trusted schema validators and planning hooks MAY run.
- **CORE-103:** Static planning authorization MUST finish before execution.
  Object-dependent authorization occurs immediately before consuming the
  protected object and MUST NOT be bypassed by aliases, fragments, references,
  collections, directives, extensions, batches, or streams.
- **CORE-104:** Execution invokes only explicitly registered handlers from the
  immutable registry snapshot bound to the plan.
- **CORE-105:** Completion validates and copies runtime values into the partial
  result representation before a later sibling or concurrent observer sees
  them.
- **CORE-106:** Serialization MUST reveal only safe public errors.
- **CORE-107:** Decode, validate, plan, complete, and serialize MUST NOT perform
  application side effects. Authorization MAY read policy and identity state
  but MUST NOT invoke business writes. Only execute may invoke registered
  application handlers.
- **CORE-108:** Every failure MUST be assigned to its originating phase. Decode
  owns malformed syntax and envelopes; validate owns invalid operations and
  types; plan owns resolution and optimizer failures; authorize owns denied or
  indeterminate access; execute owns handler and cancellation failures;
  complete owns invalid outputs; serialize owns response encoding failures;
  transport owns carrier establishment, framing, and delivery failures.

Valid: a document is fully validated before its first handler is called.

Invalid: a planner calls a handler to discover its return type and later
rejects an unrelated alias collision.

| Clause | Valid example | Invalid example |
| --- | --- | --- |
| CORE-100 | A decoded request is validated and planned before authorization and execution. | Completion repairs an input that validation rejected. |
| CORE-101 | Decode records a typed call node without looking it up. | Decode queries a registry or database. |
| CORE-102 | Validation checks every fragment branch before the first handler starts. | A statically skipped invalid branch is ignored. |
| CORE-103 | Static policy denial prevents all handlers from starting. | An alias bypasses the authorization attached to its registered field. |
| CORE-104 | Execution calls the definition captured by the immutable plan. | Execution reflects over an unregistered Go method. |
| CORE-105 | A handler-owned map is copied and completed before the next sibling starts. | A later sibling observes a map that the prior handler can still mutate. |
| CORE-106 | A panic becomes a stable public internal-error code. | A stack trace or database error text reaches the response. |
| CORE-107 | Authorization reads tenant policy; execution later invokes the write. | Planning performs the write to predict its output. |
| CORE-108 | Invalid handler output is classified as completion failure. | The same output is reported as malformed request syntax. |

## Kinds, effects, and ordering

- **CORE-200:** Operation kind, handler effect, ordering, and transport method
  are independent properties. Clients MUST NOT assert or override server
  effect metadata.
- **CORE-201:** Queries MUST be transitively read-only. A query is invalid if a
  selected call, nested call, fragment, directive, projection, or pipeline can
  reach a registered write effect.
- **CORE-202:** Mutations execute top-level selections serially in declaration
  order unless an atomicity profile imposes stricter behavior.
- **CORE-203:** Subscriptions establish a read-like source and deliver ordered
  events under the streaming profile; establishment MUST NOT conceal a write.
- **CORE-204:** Arrays are the only implicit ordered language construct. JSON
  object member order MUST NOT affect validation, execution, or identity.
- **CORE-205:** A sequential selection completes handler execution and output
  completion before its next sibling begins.
- **CORE-206:** Explicit parallel groups MAY run independent, thread-safe read
  work within negotiated bounds. Mutations require explicit server metadata and
  profile permission. Result and error assembly remains declaration ordered.
- **CORE-207:** Optimizers, loaders, and caches MUST NOT cross an effect,
  dependency, authorization, transaction, or completion barrier.

Valid: a mutation writes a value and its next selection reads the completed
state.

Invalid: a query fragment hides a registered mutation behind a Boolean
directive, even when the request variable currently skips the fragment.

| Clause | Valid example | Invalid example |
| --- | --- | --- |
| CORE-200 | A POST-carried query remains read-only and sequential unless it contains an explicit parallel group. | POST is treated as proof that the operation is a mutation. |
| CORE-201 | A query reaches only registered read effects through every fragment and pipeline. | A query spreads a fragment containing a registered write, even behind a false directive. |
| CORE-202 | Mutation selections `create` then `read` execute and complete in that order. | `read` starts before `create` completes. |
| CORE-203 | Subscription establishment reads authorization state and emits ordered events. | Establishment conceals an unregistered write. |
| CORE-204 | Reordering object members leaves semantics unchanged while selection-array order is preserved. | Runtime order follows a JSON object's parser iteration order. |
| CORE-205 | A selected list is fully completed before its next sibling starts. | The next sibling starts while list elements are still being completed. |
| CORE-206 | Independent thread-safe reads run within a negotiated bound and assemble by declaration. | An unmarked mutation is admitted to a parallel group. |
| CORE-207 | A loader dispatches before a following serial write barrier. | A cache or loader reorders work across authorization or transaction boundaries. |

## Determinism, failure, and cancellation

- **CORE-300:** Scheduling, diagnostics, paths, and response assembly MUST be
  deterministic for the same validated inputs. Application data observed from
  external systems is not thereby deterministic.
- **CORE-301:** Independent query-field failure records a path-specific error,
  omits the failed object member, and permits later independent siblings.
- **CORE-302:** A failed list element occupies a null placeholder so later
  indexes do not shift. Legitimate null has no associated error.
- **CORE-303:** A failed required field makes the partial result incomplete; a
  client MUST NOT construct a successful non-null domain model without an
  explicit completeness check.
- **CORE-304:** Parallel errors sort by response path and then source location.
  Fail-fast groups report the actually completed subset in that stable order;
  they do not promise the same winners across schedules.
- **CORE-305:** Cancellation is cooperative. It stops new admissions, reaches
  active handlers through the host cancellation primitive, and waits according
  to the runtime ownership policy of CORE-308. It is not proof that a write
  rolled back.
- **CORE-306:** Handler panics or equivalent host failures MUST be contained at
  the handler boundary and exposed only as redacted internal errors.
- **CORE-307:** A context-dependent read can return different data across two
  executions without violating deterministic assembly.
- **CORE-308:** The runtime ownership policy is bounded response waiting. A
  runtime MUST join every handler that returns within a declared grace period
  measured from cancellation, and MUST abandon one that does not: the selection
  is reported as cancelled, the handler retains its accounting slot until it
  actually exits, and neither the response nor any sibling selection waits for
  it further. A runtime MUST declare its default grace period and MUST NOT
  report an abandoned handler as stopped, rolled back, or completed. Handlers
  MUST observe cancellation; abandonment bounds the response and is not a
  guarantee that the handler ended.

Valid: two parallel reads finish in either order but data and errors are placed
in declaration/path order.

Invalid: fail-fast conformance requires one timing-dependent branch to win, or
a cancelled mutation is automatically reported as rolled back.

| Clause | Valid example | Invalid example |
| --- | --- | --- |
| CORE-300 | Two identical validated inputs produce the same paths and assembly order. | Map iteration order changes error ordering. |
| CORE-301 | A failed query field is absent while its successful sibling remains. | One failed field deletes unrelated sibling data. |
| CORE-302 | Failed list index 1 is `null` and the former index 2 remains index 2. | The failed item is removed and later indexes shift. |
| CORE-303 | A client checks completeness before constructing a required-field domain model. | An unavailable required field is silently treated as a successful null. |
| CORE-304 | Parallel failures sort by response path then source location. | Completion timing determines response error order. |
| CORE-305 | Cancellation stops queued admission and reaches every active handler. | A cancellation response claims an already-running write rolled back. |
| CORE-306 | A host panic is contained and redacted at the handler boundary. | Panic text escapes through direct invocation. |
| CORE-308 | A handler that ignores cancellation is abandoned after the declared grace and the response is still delivered. | The response blocks indefinitely on a handler that ignores cancellation. |
| CORE-307 | A context-dependent read returns different application values while response structure remains stable. | Different external data is called a scheduler determinism violation. |

## Capabilities and compatibility

- **CORE-400:** Core clauses apply to every profile. A capability may add
  declared syntax or behavior but MUST NOT weaken scalar, sequencing,
  authorization, resource, or redaction guarantees.
- **CORE-401:** Unknown required capabilities cause validation failure before
  execution. Unknown optional extension data remains inert only when its
  namespace is explicitly permitted by the envelope contract.
- **CORE-402:** Specification, canonicalization, profile, SDK, and remote-worker
  versions are separate and MUST be pinned by a release manifest.

Valid: a server rejects a requested required capability it does not advertise.

Invalid: an extension enables parallel mutations despite the core registry
marking the handler serial-only.

| Clause | Valid example | Invalid example |
| --- | --- | --- |
| CORE-400 | A streaming profile adds framing while retaining core authorization and limits. | An extension disables scalar validation. |
| CORE-401 | An unknown required capability fails validation before execution. | Unknown extension data silently enables execution behavior. |
| CORE-402 | A release manifest pins distinct spec, canonicalization, fixture, SDK, and worker versions. | A runtime version is presented as the wire-protocol version. |

## Worked boundary examples

The following query is invalid before execution because the referenced fragment
transitively reaches the registered write `deleteAccount`, even if `if` is
currently false:

```json
{
  "operations": [{
    "name": "UnsafeRead",
    "kind": "query",
    "variables": [{ "name": "enabled", "type": "Boolean" }],
    "select": [{
      "$fragment": {
        "name": "Writes",
        "directives": [{
          "name": "include",
          "arguments": { "if": { "$var": "enabled" } }
        }]
      }
    }]
  }],
  "fragments": [{
    "name": "Writes",
    "select": [{ "$call": { "name": "deleteAccount" } }]
  }]
}
```

A context-dependent read may observe tenant, clock, or database state and return
different values in two executions. This is valid when the selected paths,
completion rules, and error ordering remain deterministic; Naatre does not
claim replay-equivalent external state.

For a fail-fast group containing independent branches `a` and `b`, scheduling
may yield any actually completed subset permitted by the group policy. The
portable assertions are invariant-based: returned entries are a subset of the
declared branches, no queued branch is admitted after the terminal signal,
runtime-owned work is joined under its ownership policy, and all returned
errors are sorted by response path and source. A conformance test that requires
one timing-dependent branch to be the winner is invalid.

## Execution errors and partial results

### Error shape and redaction

- **CORE-500:** An execution error carries a stable `code`, a safe public
  `message`, a response `path`, an optional `source`, a `retryable` advisory,
  and optional namespaced `details`. Any internal cause stays available to
  in-process server hooks only and MUST NOT be serialized.
- **CORE-501:** A request error is produced before execution begins and is
  reported without `data`. An execution error is produced after execution
  begins and accompanies whatever partial data was assembled.
- **CORE-502:** An operation-level failure that no field is responsible for
  carries the empty root path `[]`. A field-level failure carries the aliased
  output path, which is distinct from the input or source path that produced
  it.
- **CORE-503:** `retryable` is advice about a failure class. It is never
  permission to replay a write; idempotency metadata and the remaining deadline
  remain authoritative.
- **CORE-504:** These codes are reserved to the runtime: `CANCELLED`,
  `HANDLER_FAILED`, `INTERNAL`, `OUTPUT_COMPLETION`, `INVALID_COLLECTION`,
  `RESULT_MISSING`, `RESULT_NULL`, `RESULT_SKIPPED`, `RESULT_SCOPE`,
  `RESULT_UNAVAILABLE`, `UNAUTHORIZED`, and `RESOURCE_EXHAUSTED`.
  `UNAUTHORIZED` and `RESOURCE_EXHAUSTED` are shaped here; their enforcement is
  specified elsewhere.
- **CORE-505:** An application MAY publish its own code for a domain failure,
  matching `[A-Z][A-Z0-9_]{2,63}` and distinct from every reserved code, and
  MAY attach `details` under period-namespaced keys. A runtime MUST reject a
  reserved or malformed code by substituting `HANDLER_FAILED`, so a domain
  failure can never impersonate a protocol guarantee. Mapping a domain error
  selects fields of the core shape and never replaces it.

### Partial completion outcomes

Each row states what a client observes in `data` and `errors`. "Absent" means
the response key is omitted; "null" means the key is present with a null value.

| Outcome | Root position | Object member | List element |
| --- | --- | --- | --- |
| Successful value | value, no error | value, no error | value at its index, no error |
| Explicit nullable null | null, no error | null, no error | null at its index, no error |
| Skipped selection | absent, no error | absent, no error | not applicable; a skip removes no index |
| Missing optional value | absent, no error | absent, no error | not applicable; elements cannot be missing |
| Authorization failure | absent, `UNAUTHORIZED` at root path | absent, `UNAUTHORIZED` at member path | null at its index, `UNAUTHORIZED` at the indexed path |
| Handler failure | absent, `HANDLER_FAILED` or the application code | absent, `HANDLER_FAILED` or the application code | null at its index, indexed error |
| Output type failure | absent, `OUTPUT_COMPLETION` | absent, `OUTPUT_COMPLETION` at member path | null at its index, `OUTPUT_COMPLETION` at the indexed path |

- **CORE-506:** A failed object member is absent with a path-specific error. A
  failed list element is a null placeholder with an indexed error so later
  indexes do not shift. A legitimate null never carries an error.
- **CORE-507:** A failed member makes the result partial. A client MUST NOT
  project partial data onto a model whose required fields are non-null without
  an explicit completeness conversion that fails on unresolved required data.
- **CORE-508:** A member whose own completion failed stays unavailable at every
  depth at which it is selected, and MUST NOT be re-reported as a handler
  outcome. Its ancestors keep their remaining valid data.

### Effects and response assembly

- **CORE-509:** Execution outcome and response assembly are distinct. An
  operation can apply its effect and still fail to project the response, so a
  runtime MUST report effect state as safe outcome metadata rather than as a
  generic retryable error.
- **CORE-510:** Effect state is one of `not-applicable` for an operation that
  declares no effect, `none` when no effectful handler started, `applied` when
  every effectful handler completed and the response was assembled from them,
  `rolled-back` only when an undo was confirmed, and `indeterminate` otherwise.
  A runtime that cannot observe an undo MUST report `indeterminate` and MUST
  NOT report `rolled-back`.

Valid: a mutation whose write succeeded but whose output failed completion
reports `indeterminate` with an `OUTPUT_COMPLETION` error.

Invalid: reporting that same mutation as `rolled-back`, or as a bare retryable
error that invites the client to replay the write.

| Clause | Valid example | Invalid example |
| --- | --- | --- |
| CORE-500 | A handler failure is reported as a stable code and safe message. | The database error string becomes the public message. |
| CORE-501 | A malformed envelope is reported without `data`. | A decode failure is reported alongside fabricated partial data. |
| CORE-502 | A pre-execution cancellation carries the empty root path. | The failure is blamed on an arbitrary first field. |
| CORE-503 | A cancelled read is advertised retryable without authorizing a replay. | `retryable` is treated as permission to resend a write. |
| CORE-504 | A runtime emits `OUTPUT_COMPLETION` for invalid handler output. | An application publishes `OUTPUT_COMPLETION` for a domain failure. |
| CORE-505 | `ORDER_LOCKED` is published with `shop.retryAfter` details. | A handler returns `INTERNAL` to look like a runtime failure. |
| CORE-506 | Failed element 1 is null and former element 2 stays at index 2. | The failed element is removed and later indexes shift. |
| CORE-507 | A completeness conversion fails on an unresolved required field. | Partial data is cast into a non-null required model. |
| CORE-508 | A failed nested member stays unavailable when selected directly. | Selecting it runs its handler and reports a second error. |
| CORE-509 | A committed write with an invalid projection reports both states. | The projection failure is reported as though the write never ran. |
| CORE-510 | A runtime without transactions reports `indeterminate`. | The same response claims `rolled-back`. |

## Acknowledgement

Naatre is inspired by ideas from Deepr, GraphQL, JSON Schema, Smithy, Connect,
and RPC systems. It defines its own v1 and makes no compatibility claim.
