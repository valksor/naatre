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

Registered handlers and in-process extensions are trusted application code.
Explicit registration limits which names a Naatre request can reach; it is not a
sandbox and cannot stop a registered handler from accessing the network,
database, filesystem, or other process capabilities. Applications remain
responsible for those effects and for the truth of their registration metadata.

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
