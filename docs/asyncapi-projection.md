# AsyncAPI projection tooling

Issue #102 owns the Go `adapter.asyncapi.projection-1` implementation layered
on issue #62's `adapter.asyncapi-1` contract. The normative projection rules
are in [`spec/v1/asyncapi-projection.md`](../spec/v1/asyncapi-projection.md).
AsyncAPI remains a description format; `schema.Document`, the Naatre protocol,
and the existing runtime registries remain the only schema, operation, effect,
and authorization authorities.

## Package and lifecycle boundary

Package `github.com/valksor/naatre/asyncapi` owns deterministic projection,
strict import diagnostics, document validation, and detached round trips.
Applications own schema authorization, provenance, storage, publication,
server allowlists, compatibility decisions, process lifecycle, runtime
registration, credentials, and transport startup or shutdown.

Call `ExportProjection` with an already authorized `asyncapi.Model`. Call
`ImportProjection` or `ValidateProjection` with the exact independently held
`schema.Document` authority and exact inert server URL allowlist. Import first
matches the embedded canonical schema, revision, and digest to that authority,
then uses the issue #62 strict local-reference importer, and finally requires a
byte-identical re-export. A returned model is detached metadata: no operation
is registered and no handler, resolver, credential provider, client, server,
listener, or network resolver is available to these APIs.

The package owns memory only for the duration of each call. The caller owns
input/output byte retention and cancellation. Cancellation before or during
projection returns `ASYNCAPI_PROJECTION_CANCELLED`; no partial model is
returned. Limits are applied before expensive parsing and again during the
canonical export boundary.

## Supported runtime and projection

The supported implementation profile is a portable offline library built with
Go 1.27 or newer. It claims no certified operating system or architecture. The
output pins AsyncAPI 3.0.0 and JSON Schema Draft 2020-12, projects every
authorized object, input object, interface, one-of, union, list, map, enum,
built-in scalar, and portable custom-scalar wire shape, and preserves the
canonical Naatre declaration alongside each JSON Schema component.

Channels, operations, messages, correlation identities and trust lifetimes,
security descriptions, transport bindings and evidence, examples, revision
links, and projected schemas are accepted only when byte-stable across import
and re-export. AsyncAPI security remains descriptive and cannot grant Naatre
authorization. Any effect or authorization extension in an AsyncAPI operation
is outside the closed issue #62 wire shape and is rejected.

## Unsupported optional capabilities

This profile does not support or claim:

- YAML input, AsyncAPI 2.x, floating AsyncAPI 3.x revisions, alternate JSON
  Schema dialects, remote/file/data/dynamic references, reference resolvers,
  overlays, or partial-document import;
- deriving a Naatre schema, operation, effect, authorization, idempotency,
  retry, cost, batching, transaction, cache, event, or compatibility policy
  from AsyncAPI content;
- runtime registration, handler invocation, schema mutation, code generation,
  broker provisioning, server discovery, client generation, mock business
  behavior, or automatic compatibility approval;
- Kafka, AMQP, MQTT, NATS, Redis, Pulsar, SNS/SQS, WebSocket, SSE, webhook, or
  worker wire interoperability beyond the evidence-backed descriptive bindings
  already allowed by `adapter.asyncapi-1`;
- DNS, HTTP, TLS, proxy, socket, listener, filesystem watcher, registry,
  credential-provider, secret-store, KMS/HSM, metrics, tracing, or audit
  integrations;
- browser, edge, mobile, WASM, non-Go native runtimes, framework adapters,
  alternate JSON libraries, operating-system or architecture certification,
  managed services, containers, clusters, live-network tests, or third-party
  AsyncAPI certification.

Unsupported schema kinds or custom scalars without portable wire shapes fail
closed with `ASYNCAPI_SCHEMA_PROJECTION_UNSUPPORTED`. Unsupported document
keywords fail through the strict issue #62 importer. Nothing is silently
dropped or promoted into a broader capability claim.

## Reproducible offline conformance

[`conformance/v1/asyncapi-projection.json`](../conformance/v1/asyncapi-projection.json)
pins the exact issue #62 dependency revision and digests, implementation files,
public codes, capability inventory, and fixture classes. From the repository
root, use the pinned Go 1.27 toolchain:

```sh
GOWORK=off go test ./asyncapi -run 'Projection' -count=1
GOWORK=off go test ./internal/conformance -run 'AsyncAPI' -count=1
GOWORK=off go test ./... -count=1
GOWORK=off go vet ./...
golangci-lint run --allow-parallel-runners
go generate ./...
```

All projection tests are listener-free and network-free. Live transport,
third-party tooling, cross-language, and certification checks require separate
orchestrator evidence and do not widen this profile.
