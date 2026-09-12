# Go registry contract

The Go runtime exposes application behavior only through explicit registration.
Root queries, mutations, and subscriptions use `runtime.Bind`; object projections
use `runtime.BindField`; and object calls use `runtime.BindCall`. The field and
call binders capture the concrete source type, so invocation rejects a value of
another Go type instead of inspecting it with reflection. Public methods and
fields that were not registered remain unreachable.

Registration validates the portable input and output type identifiers, handler
types, nullability, owner type, declared schema fields, operation kind, and all
required metadata before a snapshot can be used. Nullable scalar inputs use a
typed pointer. The invocation boundary converts an untyped `nil` interface to
that typed nil pointer; non-null inputs and mismatched pointer/value signatures
are rejected during registration.

Effect, determinism, cacheability, retry safety, thread safety, batching,
transaction participation, authorization policy, deprecation, idempotency,
cost, and parallel-mutation eligibility are independent descriptor fields. They
are operator assertions, not properties inferred from HTTP method or operation
kind. Invalid combinations such as a cacheable write, a nondeterministic cache,
or parallel-mutation permission on a query are rejected.

`Registry.Freeze` copies the definition table and prevents later registration.
Snapshots and their deterministically ordered descriptor exports are safe for
concurrent reads. Request variables, source objects, principals, and loader
state are invocation values and are never retained in a descriptor. A handler
marked `thread-safe` may be called concurrently on the same captured handler
instance. A handler marked `serial-only` is serialized across requests, and
queued calls honor context cancellation before entering application code.

`Snapshot.ExportSchema` combines the frozen type catalog with root and object
handler manifests to produce the canonical language-neutral schema document.
Registrations may supply explicit stable IDs; legacy registrations derive
`kind.name` root IDs and `Owner.name.resolver` member IDs deterministically.
Descriptions, structured deprecation, capabilities, traits, and source
provenance are cloned into the immutable snapshot. `Snapshot.ValidateSchema`
compares the complete frozen manifest—including effect, authorization, cost,
cache, retry, batching, transaction, and concurrency metadata—with a proposed
document and rejects any mismatch before deployment artifacts bind its revision.

Registered handlers and in-process extensions are trusted application code.
Explicit registration limits which names a Naatre request can reach; it is not a
sandbox and cannot stop a registered handler from accessing the network,
database, filesystem, or other process capabilities. Applications remain
responsible for those effects and for the truth of their registration metadata.

## Directives

`Registry.RegisterDirective` binds a `schema.DirectiveDescriptor` to optional
planning, execution-wrapper, and response-annotation callbacks. The registry
preinstalls the standard `include` and `skip` descriptors and rejects custom
uses of their names or the reserved `naatre.*` and `core.*` namespaces. Frozen
snapshots clone and export every descriptor through `DirectiveDescriptors` and
`ExportSchema`; callbacks stay process-local and never enter schema identity.

Custom documents must require and negotiate the descriptor's exact capability.
Directive arguments use normal schema coercion. Planning callbacks receive a
closed `DirectiveSelection` view and may only skip the already validated node or
add bounded cost. Execution wrappers apply only to calls and fields, after
authorization, cancellation checks, input validation, and execution admission;
their continuation has no context parameter, is one-shot, closes with the
wrapper, and keeps in-flight work accounted. Response annotators receive no
mutable output and return canonical JSON metadata ordered by response path and
directive source position; they remain cancellation- and admission-bounded.

## Authorization and interceptors

`Registry.ConfigureAuthorization` selects allow-by-default compatibility mode
or deny-by-default mode. Production services should always select
`runtime.AuthorizationDenyByDefault` and install a dynamic authorizer. The
runtime checks that policy before argument evaluation and before every planned
field or call handler. Policy callbacks receive no raw argument map, policy
errors and panics fail closed, and a returned allow is rejected when its expiry,
authorization revision, or cache scope is not current.

`Registry.RegisterInterceptor` installs trusted handler wrappers. Their fixed
outer-to-inner order is global, operation, type, field, and exact handler, with
registration order preserved inside a level. The continuation has no arguments
and is one-shot. Cancellation before or during the wrapper wins over any value
it returns. Configuration and registration both close when the registry is
frozen.

`Snapshot.InvokeRoot`, `InvokeField`, and `InvokeCall` are low-level typed
binding probes for trusted application code. They do not represent Naatre
request execution and do not run a plan's authorization or interceptor chain.
Untrusted requests must enter through `Prepare` and `Plan.Execute`; a trusted
callback that captures and uses a raw snapshot remains inside the in-process
trust boundary described by SEC-303.

## Request resource limits

`runtime.PrepareWithOptions` accepts a `runtime.ResourceLimits` value and
stores the resolved, immutable limits on the plan. Zero fields select finite
reference defaults exposed by `runtime.DefaultResourceLimits`. Preparation
bounds expanded plan depth and nodes, calls, fields,
fragment depth and selections, parallel width, queued work, checked static
cost, and validation diagnostics before planning callbacks, authorization
callbacks, or handlers can run.
`Prepare` applies the same defaults.

Execution uses one shared atomic work/cardinality meter, concurrency limiter,
and deadline across nested scopes and parallel groups. Collection cardinality
is checked before result allocation; every executable node is charged before
directive or handler work, and registered handler cost is multiplied by each
runtime occurrence. Output copying/completion, public errors, and the
serialized outcome are capped while preserving mutation effect state. A
request-owned deadline and all runtime/output exhaustion use
`RESOURCE_EXHAUSTED` at a response path; cancellation inherited from the
caller remains `CANCELLED`.

Transport code must apply `protocol.Limits` while decoding before calling the
runtime. Provider and streaming adapters additionally enforce the independent
replay and live budgets in SEC-404 and SEC-405; the core fixture defines those
portable accounting expectations without pretending the in-process core owns a
broker implementation.
