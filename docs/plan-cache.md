# Go plan-template cache

Issue #74 owns the Go implementation profile `runtime.go.plan-cache-1`.
The normative planning, effect, authorization, transaction, completion, and
request-neutrality rules remain owned by `core.language-1` and the clauses in
`spec/v1/core.md`; this package does not define another protocol or schema.

## Package and lifecycle

`runtime.PlanCache` is an optional process-local LRU for plans produced by a
single frozen `runtime.Snapshot`. `NewPlanCache` requires the application's
current schema and authorization-policy revisions and a finite entry bound.
`PlanCache.Prepare` hashes the immutable snapshot identity, revision pair,
canonical document, selected operation, negotiated capabilities and extension
descriptors, and planning limits. Correlation IDs, principals, claims, request
extension payloads, and variable values are excluded.

The cached value is an immutable request-neutral template. Every lookup returns
a distinct `runtime.Plan` with a fresh copy of that request's variable values;
the principal remains in the execution context and is never stored in the plan.
Eviction drops only the cache's reference, so a returned plan remains immutable
and executable. Applications call `UpdateRevision` before admitting work on a
new schema or policy revision. The update clears old entries and increments a
generation fence, preventing an older in-flight compilation from republishing.

The cache itself is safe for concurrent use. It is owned and closed implicitly
with its process; it has no goroutines, files, sockets, background refresh, or
external shutdown step. `Stats` exposes bounded counts and revision identities,
never keys or request data.

## Optimizer and explain boundary

Template construction runs only the runtime's two versioned conservative
passes: empty-fragment pruning and empty-parallel-group pruning. A node is
eligible only when it has no children, directives, output, binding, or handler.
There is no callback API for application code to rewrite executable plans.

Each returned plan includes `PlanDescription.Transformations`. Every record
names the pass and version, applied or unchanged outcome, changed-node count,
request-neutral before/after digests, effect-barrier counts, and an explicit
barrier-preservation result. A mismatched effect-barrier signature fails closed
with `PLAN_OPTIMIZER_BARRIER_VIOLATION`; it never returns a rewritten plan.
Cache and optimizer failures use the stable `PlanCacheError` code/message shape.
Revision values, cache keys, variables, claims, credentials, source values, and
internal causes never enter those errors or transformation records.

## Runtime and profile support

The public implementation is the dependency-free Go package
`github.com/valksor/naatre/runtime`, requiring Go 1.27. The checked-in
`runtime.go.plan-cache-1` evidence was executed on Darwin ARM64 with Go 1.27.1.
The repository CI separately exercises the runtime package on Linux AMD64,
Linux ARM64, Darwin AMD64, Darwin ARM64, and Windows AMD64; those jobs become
release evidence only when their exact run is published.

Reproduce the local profile and its race coverage with:

```sh
go test ./runtime -run '^TestPlanCache' -count=1
go test -race ./runtime -run '^TestPlanCache' -count=1
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"plan-cache","command":"run","path":{"source":{"kind":"sdk","language":"go"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["runtime.go.plan-cache-1"]}' | go run ./cmd/naatre-conformance --require-pass
```

The profile does not support distributed or cross-process plan caches,
persistent template serialization, external cache providers, background
refresh or warming, user-supplied optimizer passes, effect/authorization/
transaction/completion-barrier reordering, native code generation, JIT
compilation, cost-based reordering, transport or framework integration,
remote schema discovery, multi-version entries in one cache instance, runtime
certification outside the recorded platform, or deployment certification.
