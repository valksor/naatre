# Go federation coordinator

The `runtime.go.federation-coordinator-1` evidence profile implements the Go
coordinator and distributed entity-fetch planner for the normative
`core.federation-1` contract. The evidence profile is not a request capability,
wire protocol, schema format, or second source of federation semantics. Those
remain exclusively defined by [`spec/v1/federation.md`](../spec/v1/federation.md)
and enforced by `schema.ComposeFederation`.

## Ownership and package boundary

- `schema.ComposeFederation` owns authenticated manifest composition, service
  and declaration ownership, entity-route validation, dependency-cycle
  rejection, and immutable composition identity. It never discovers or
  dereferences an endpoint.
- `runtime.FederationPlanner` accepts explicit entity fetches, resolves only
  routes admitted by a retained composition, validates their declared
  dependencies, derives operation-owned retry cost, bounds fan-out, and emits a
  deterministic dependency-ordered `runtime.FederationPlan`.
- `runtime.FederationCoordinator` binds the planner to the existing bounded
  reference executor. It admits only an exact operator-supplied adapter for
  every opaque endpoint reference, signs least-authority delegation, propagates
  an optional validated W3C `traceparent`, pins service revisions, preserves
  independent sibling data, and owns all public error mapping.
- The host operator owns original-principal authentication, manifest provenance,
  endpoint adapter construction, gateway key storage and rotation, retained
  composition revisions, and graceful process drain. A downstream still owns
  and applies its current authorization policy after verifying delegation.

Application credentials, arbitrary principal claims, transport diagnostics,
endpoint implementation details, and W3C baggage never enter the public plan or
error surface. An adapter receives only the pinned invocation, signed
delegation, approved trace parent, and request context.

## Lifecycle and rolling upgrades

1. Authenticate all service manifests and compose them with exact operator
   trust pins.
2. Retain the complete immutable composition and bind every opaque endpoint
   reference to one explicit `runtime.FederationInvoker` adapter.
3. Construct one coordinator with finite call, cost, concurrency, attempt, and
   duration limits.
4. Plan each entity-fetch set against that composition's exact schema revision.
   Every manifest-required route must be supplied and linked by response key.
5. Execute the plan with an authenticated principal and non-empty authorization
   revision. Dependencies execute before dependants; unrelated calls may use
   bounded parallelism. A failed dependency prevents its dependant from being
   invoked without discarding successful independent data.
6. During an upgrade, keep the old coordinator and all endpoint pins until its
   admitted work drains. If the complete old composition is unavailable,
   reject its revision with `FEDERATION_SCHEMA_MISMATCH` and replan. Never mix
   service pins from two composition revisions.

## Runtime and platform boundary

The package requires Go 1.27. Repository CI executes portable-package tests on
Linux arm64 and cross-compiles them for Linux amd64, macOS amd64 and arm64, and
Windows amd64. The checked coordinator evidence records the exact runtime and
platform on which it was executed; cross-compilation is not runtime evidence
for another platform. No other native runtime is certified by this profile.

Transport adapters are operator-provided Go implementations of
`runtime.FederationInvoker`. The coordinator validates their opaque binding and
contains their failures, but this slice ships no network client or discovery
mechanism.

## Unsupported optional capabilities

This profile explicitly does not support:

- service discovery;
- endpoint-reference dereferencing;
- routing optimization;
- built-in HTTP transport;
- built-in gRPC transport;
- SSE streaming;
- WebSocket streaming;
- federated mutations;
- distributed transactions;
- W3C `tracestate`;
- W3C baggage;
- framework integration; or
- cross-language native-runtime certification.

Adding one of these capabilities requires separate versioned evidence and must
not silently change `core.federation-1` behavior.

## Reproducible evidence

`conformance/v1/federation-coordinator.json` records the exact #24 dependency
commit, the digest of `conformance/v1/federation.json`, the `go.mod` digest,
runtime/platform evidence, supported and unsupported boundaries, and the
positive, negative, boundary, cancellation, and resource-limit cases. Its own
digest is pinned by `conformance/v1/suite.json`.

From the repository root, run:

```sh
go test ./runtime -run 'Federation' -count=1
go test ./internal/conformance -run 'TestGoFederationCoordinatorProfile' -count=1
go test -race ./runtime ./schema ./internal/conformance -run 'Federation' -count=1
```

For a machine-readable Go test event stream, add `-json` to either `go test`
command. Full release verification additionally uses the repository gates in
the root README.
