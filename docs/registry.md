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
