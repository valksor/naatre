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

## Extensions

`Registry.RegisterExtension` binds a reverse-DNS ID and Semantic Version to an
exact capability, implementation identity, declared extension points, effects,
cost behavior, security statement, directive ownership, ordering, and
conflicts. `Registry.Freeze` rejects conflicts, ordering cycles, missing or
multiply owned directives, capability mismatches, undeclared points/effects,
and undeclared cost. Available topological-order ties use lexical IDs, while
directive calls retain their authored source order.

`Snapshot.ExtensionDescriptors` returns detached descriptors in resolved
registry order. `Snapshot.ExportSchema` includes them in canonical discovery.
`Snapshot.DecodeOptions` adds exact extension versions and capabilities to a
detached protocol policy and rejects conflicting legacy or ignorable namespace
rules. Required payloads are retained only after exact capability negotiation;
explicitly inert optional metadata is discarded. Plan descriptions and
execution outcomes report negotiated ID/version/capability tuples without
retaining extension payloads.

Behavioral extensions use only the bounded directive callbacks above. They do
not receive a mutable AST, plan, registry, authorization decision, resource
limit, handler, or response value. This preserves core structural, effect,
authorization, cost, and completion checks without claiming that arbitrary
in-process Go code is sandboxed.

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

## Request-scoped batching and caching

`BindBatch`, `BindBatchInvocation`, `BindBatchField`, and `BindBatchCall` bind
typed batch handlers. Each registration supplies a `BatchOptions` key function
and finite maximum size. Inputs carry stable indexes, so a backend may return
`BatchResult` values out of order; omitted, duplicate, and unknown indexes are
reported deterministically. The executor forms windows only across explicit
parallel branches and wholly read-safe mapped collection work. Writes,
serial-only handlers, transaction-required work, execution wrappers, and
interceptors remain barriers. A serial map legitimately invokes a batch handler
once per item.

Every logical item is authorized and charged before grouping, including
duplicates. Batch and in-flight-cache waiters do not hold handler concurrency
permits. `ExecuteOptions.Batch.Observe` receives tracing-safe start/completion
events with operation, handler, size, and status but no keys, inputs, claims, or
backend errors.

Handlers marked `Cacheable` use an execution-local memo only after current
authorization. Dynamic `no-store` decisions, wrappers, and interceptors disable
reuse. Errors and null results are not retained unless `CacheErrors` or
`CacheNulls` is explicitly set; `DisableRequest` disables the local memo.
`CacheKey` binds operation kind/name, handler, document/schema digests, schema
revision, canonical variables, typed input, authorization identity, and the
application-provided context. These are digests rather than raw sensitive
values.

`ExecuteOptions.Cache.External` is an application-owned `ResultCache`; core has
no package-global or snapshot-owned entry store. Cross-request hooks run only
with a schema revision and an authorization decision permitting principal or
tenant scope. A completed write clears request entries and calls `Invalidate`
before dependent work proceeds. In-flight fills from an older generation
cannot republish after that barrier.

## Streaming sources and replay

`runtime.StreamSourceSession` owns a `runtime.StreamSource` after subscription
establishment. A source's `Next` method must observe cancellation; `Close` must
be concurrency-safe, idempotent at the source boundary, and unblock a pending
read. `Next` transfers ownership of each returned frame, so the source cannot
mutate it during or after return. The session closes the source on
cancellation, EOF, protocol failure,
authentication or authorization failure, schema retirement, terminal delivery,
or explicit consumer abandonment. A clean EOF before a required terminal frame
returns `protocol.ErrStreamTruncated`.

Supply `Reauthorize` to check every protected data, patch, and error immediately
before it leaves the runtime. Supply `ValidateSchema` to retain the immutable
revision named by the opening frame; return `runtime.ErrStreamSchemaRetired`
when it can no longer be served. The reference session deliberately exposes a
pull API and never detaches a source read. Security and schema checks have a
finite wait and fail the delivery when their deadline expires. Because Go
cannot stop arbitrary application code, the process-shared
`StreamCheckExecutor` retains its bounded slot for a callback that ignores
cancellation until that callback actually exits. The same ownership rule
applies to an uncooperative source read.

`runtime.StreamCursorCodec` produces expiring HMAC-protected cursors bound to
stream, tenant, principal, authorization revision, and schema revision. Every
decode failure is the same external `ErrStreamHistoryUnavailable` result.
`runtime.StreamReplayBuffer` is a bounded process-local reference store with
finite aggregate stream, history-byte, and subscriber counts. Its
`Subscribe(ctx, scope, opaqueCursor, options)` operation decodes the cursor
through `options.CursorCodec`; callers cannot provide a divergent decoded
position. It takes the store lock while it captures replay history and
registers the live queue, which makes replay-to-live handoff gap-free. Retained
frames are shared as immutable reference-counted values and each authorization
or delivery gets its own clone. Per-frame retention expiry releases history
without ending an active live subscription, while completed expired histories
free aggregate stream slots. Aggregate pressure detaches only subscribers
retaining the frames that prevent reclamation. Queue overflow detaches only the
slow subscriber and returns `ErrStreamSlowConsumer`; full transport and broker
adapters remain separate.

Every source session supplies a validated `StreamSourceAdvertisement`; profile
version, replay capability, consistency, retention, maximum replay work, and
recovery action are therefore explicit rather than inferred. Establishment
requires the exact negotiated profile, an absolute authentication expiry,
per-frame `Reauthorize` and `ValidateSchema` checks, a shared bounded
`StreamCheckExecutor`, and finite session duration, event, byte, and
check-timeout limits. Replay establishment additionally requires
`ReauthorizeSession`. The resume frame, when present, is the single frame
immediately after `open`; replay and live frames follow it in one monotonic
delivery sequence. Replay bytes and the resume control frame count toward the
same session byte and event budgets as live delivery.

Fresh-subscription configuration errors are returned as configuration or
resource errors and are never disguised as missing history. Once a cursor is
supplied, malformed, unknown, expired, evicted, over-budget, binding-mismatched,
or reauthorization-denied establishment returns only
`ErrStreamHistoryUnavailable`. The transport adapter owns the mechanical
mapping of that result to one `history-unavailable` frame carrying the recovery
action from the source advertisement, with no cursor or protected identity.
The replay buffer's snapshot and live registration occur in the same critical
section, a lock-atomic equivalent of register-then-capture that leaves no
observable commit point between those steps. Adapters call `Finalize` after a
logical stream completes to close abandoned subscribers, release retained
history immediately, and reclaim the aggregate stream slot instead of waiting
for retention expiry.

`protocol.NewResumingStreamReceiver` starts from a validated prior snapshot and
position. It still requires a fresh sequence-one `open` and a real `resume`
frame before accepting replay; `Recovery` reports `history-unavailable`
separately from `Terminal`, whose only successful shapes are `complete` and a
final `error`.

## Federation composition and reference execution

`schema.ComposeFederation` accepts authenticated service manifests only when
their `core.federation-1` profile, audience, opaque endpoint reference, schema
revision, and schema digest exactly match operator-owned `ServiceTrust` pins.
It deterministically rejects type, ownership, reference, entity-route, and
dependency-cycle conflicts and returns detached immutable service, operation,
and route views. The composed artifact has a dedicated `federation` semantic
hash distinct from its public schema hash.

`runtime.FederationDelegationIssuer` holds the gateway-private Ed25519 key;
downstreams receive an audience-pinned `FederationDelegationVerifier` containing
only the public key. `runtime.ReferenceFederationCoordinator` executes an
already resolved read-only plan with schema-owned cost/retry metadata, shared
request-wide concurrency and deadline accounting, safe partial results, and
stable public errors. It preserves in-process context values into the invoker.
`runtime.FederationPlanner` and `runtime.FederationCoordinator` add deterministic
entity-route planning, dependency-aware scheduling, exact opaque endpoint
bindings, and bounded W3C `traceparent` propagation without introducing another
wire or schema authority. Discovery, built-in network transports, routing
optimization, mutations, and distributed transactions remain unsupported; see
the [coordinator profile](federation-coordinator.md).

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
