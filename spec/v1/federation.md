# Federation composition and execution

This document defines the `core.federation-1` profile. It specifies deterministic
composition of independently owned service schemas and the portable safety
boundary for executing an already resolved cross-service read plan. It does not
standardize service discovery, transport, production query planning, or a
distributed transaction protocol.

## FED-001 — profile boundary

A federation implementation composes authenticated service manifests into one
immutable schema revision and executes only operations owned by those admitted
services. Implementations MUST negotiate `core.federation-1` before accepting a
federated plan. An implementation MUST NOT infer federation ownership from an
application member's logical `owner` metadata.

Production entity planning, routing optimization, discovery, and transport
integration are separate capabilities. The Go reference coordinator accepts an
already resolved read-only call plan solely to make this profile's delegation,
resource, error, and version rules executable.

## FED-100 — authenticated manifests and service trust

Every manifest MUST identify one service, its exact federation profile, exact
audience, an opaque endpoint reference, a schema revision, the SHA-256 digest
of that schema, the operation and member IDs it owns, and any entity fetch
routes it supplies. A composer MUST authenticate manifest provenance before
composition.

The deployment supplies an allowlist keyed by service ID. It MUST pin the
service federation profile, audience, endpoint reference, schema revision, and
schema digest. The manifest values MUST exactly equal those operator pins; a
self-consistent but differently digested schema is not trusted. Endpoint
references are deployment-owned opaque identifiers: composition MUST NOT parse,
resolve, fetch, or follow them, and URL-shaped or control-character-bearing
references MUST be rejected. Missing, duplicate, or unallowlisted services fail
closed before a deployable artifact exists.

Every service and operator pin MUST name `core.federation-1`. Missing,
unsupported, or version-skewed profiles fail with
`FEDERATION_PROFILE_MISMATCH`; schema capabilities do not substitute for this
service-level profile negotiation.

The manifest's declared schema revision MUST equal its embedded schema revision,
and its declared digest MUST equal the schema-domain semantic hash of that
embedded schema. Drift fails with `FEDERATION_SCHEMA_MISMATCH`.

## FED-101 — deterministic composition identity

Composition MUST be independent of manifest arrival order and map iteration
order. Services, declarations, ownership records, entity keys, dependencies,
traits, retired identities, and pinned schema references MUST be emitted in their specified
canonical order. When several entries are invalid, failure selection MUST also
be deterministic.

Identical pinned references contributed by several services are emitted once.
References with the same URI but different revisions or digests, and references
that conflict with a generated `service:<id>` pin, fail with
`FEDERATION_TYPE_CONFLICT`; input references MUST NOT be silently dropped.

The composed schema has its own revision and schema-domain hash. The complete
federation artifact, including service bindings, ownership, entity routes, and
the composed schema, MUST additionally use the dedicated `federation` semantic-
hash domain. Implementations MUST return detached immutable snapshots; mutating
a returned binding, descriptor, route, key, or dependency cannot change the
composition or either hash.

## FED-102 — declaration compatibility and ownership

All appearances of the same stable type, field, directive, trait, retired
identity, or reference MUST be semantically compatible. Conflicting type shapes,
field types, directive definitions, trait metadata, retired metadata, or source
metadata fail with `FEDERATION_TYPE_CONFLICT`.

Every composed root operation and executable member MUST have exactly one
service owner. A service may claim only declarations present in its own embedded
schema. Duplicate ownership, ownership spoofed across service schemas, and an
executable declaration with no owner fail before deployment. Ownership conflicts
use `FEDERATION_OWNERSHIP_CONFLICT`; malformed or missing claims use
`FEDERATION_INVALID_MANIFEST`.

## FED-103 — entity keys and dependency graph

An entity route identifies a type, the owning fetch operation, a non-empty set
of schema-declared entity keys, and zero or more service/type dependencies. The
route's operation MUST be owned by the same service, the type and every key MUST
exist in that service schema, and every dependency MUST resolve to another
admitted route.

The composer MUST reject duplicate routes and cycles in the complete dependency
graph before deployment or planning. Unknown keys, operations, services, and
types use `FEDERATION_INVALID_MANIFEST`; cycles use
`FEDERATION_ENTITY_CYCLE`.

## FED-200 — least-authority delegation

The gateway MUST authenticate the original principal and retain a non-empty
subject and authorization revision before downstream work. Each downstream call
MUST carry a short-lived delegation signed with gateway-private Ed25519 key
material. A downstream verifier receives only the corresponding public key and
MUST be permanently pinned to one exact audience; possession of verification
authority MUST NOT grant minting authority.

The signed payload MUST bind version, issuer, audience, subject, optional tenant,
authorization revision, request ID, operation ID, composed schema revision,
target service schema revision and digest, expiry, charged cost, and concurrency
allocation. Its signature input MUST be domain-separated from every other
signed message. Arbitrary principal claims MUST NOT be forwarded.

Every malformed, oversized, forged, expired, wrong-issuer, wrong-audience, wrong-
request, wrong-operation, wrong-schema, or incomplete delegation fails with one
indistinguishable authentication result. Delegation proves the gateway's request
binding; the receiving service MUST still apply current downstream policy.

## FED-201 — request context propagation

The coordinator MUST propagate the current subject, tenant, authorization
revision, request ID, operation ID, trace context, and the earliest applicable
deadline. It MUST NOT replace a caller deadline with a later one. If an
authorization context is missing or cannot be delegated, no downstream invoker
may run and the public request fails closed.

The in-process reference API preserves context values into its invoker. Concrete
cross-process trace serialization and transport propagation are owned by #109.

## FED-202 — schema pins and rolling upgrades

A plan MUST name the exact composed schema revision from which it was resolved.
Each invocation MUST name the selected service's exact schema revision and
digest, and its delegation MUST bind the same values. A downstream response MUST
echo the pinned service revision. Any mismatch fails with
`FEDERATION_SCHEMA_MISMATCH`; execution MUST NOT reinterpret a call against a
newer schema.

During a rolling upgrade, a host MAY continue serving an older plan only while
it retains that complete composition and all target service pins. Otherwise it
MUST reject the unavailable composition revision with
`FEDERATION_SCHEMA_MISMATCH` and require replanning. It
MUST NOT mix declarations or ownership from different composition revisions.

## FED-300 — request-wide work and retry budgets

Federated calls share the originating request's finite call, cost, concurrency,
attempt, queue, and duration budgets across every service. A schema operation's
registered cost is authoritative and has a minimum charge of one. Caller input
MUST NOT lower cost or mark an operation retry-safe.

Preflight accounting MUST use checked arithmetic and multiply operation cost by
the admitted attempt count. Nested gateways, fan-out, and service-local retries
MUST consume the same end-to-end attempt and cost authority; they MUST NOT
multiply an outer budget into independent inner budgets. Plans exceeding calls,
cost, attempts, or portable concurrency fail with `RESOURCE_EXHAUSTED` before
invocation. Structural, ownership, and retry-policy plan defects use
`FEDERATION_PLAN_INVALID`.

## FED-301 — concurrency, timeout, and uncooperative work

All calls in one request share one concurrency limiter, including calls to
different services. Scheduling MUST observe parent cancellation and the request
resource deadline before acquiring a slot and before every attempt. Parent
cancellation uses `CANCELLED`; expiry of the coordinator's maximum duration uses
`RESOURCE_EXHAUSTED`.

A host MAY return after a finite abandonment grace when an invoker ignores its
context. The detached invocation MUST retain its concurrency slot and accounting
until it actually exits; no later request may reuse that capacity early.

## FED-302 — stable errors and partial availability

The coordinator owns every public error code, message, and response path.
Downstream codes in reserved namespaces, arbitrary messages, and invalid or
oversized paths MUST be replaced with safe coordinator-owned values. Private
transport and service diagnostics may remain in internal wrapped causes but
MUST NOT cross the public boundary.

Independent successful sibling data MUST remain available when another service
fails. The failure path MUST be rooted at the original aliased response path and
remain stable across transport placement. Plan defects use
`FEDERATION_PLAN_INVALID`, service schema drift uses
`FEDERATION_SCHEMA_MISMATCH`, and unavailable or invalid downstream outcomes use
`FEDERATION_UNAVAILABLE`.

## FED-303 — retries and availability

Automatic retry requires schema-owned `retrySafe` metadata, a retryable
unavailable result, remaining request attempts, remaining cost, and a live
deadline. Schema mismatch, delegation failure, cancellation, resource
exhaustion, applied writes, and non-retry-safe operations MUST NOT be retried.
Partial availability is an error-and-data outcome, not permission to replay
successful siblings.

## FED-304 — transaction boundary

Federation creates no implicit distributed transaction. Each service's local
transaction and effect state remain independently truthful. A coordinator MUST
NOT claim atomic rollback across services, and a failure after a remote effect
MUST NOT erase or relabel that effect. Cross-service atomicity requires a
separately negotiated protocol outside `core.federation-1`.

## Conformance

Portable vectors are fixed by `conformance/v1/federation.json`. Implementations
MUST cover successful multi-service execution, partial failure, timeout, forged
delegation, wrong audience, remote schema drift, rolling-upgrade version skew,
malicious endpoint references, untrusted schema provenance, service-profile
skew, ownership and type conflicts, entity cycles, and multiplicative
retry/cost fan-out.
