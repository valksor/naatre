# OpenAPI 3.2 adapter

`openapiadapter` owns the Go `adapter.openapi-1` integration slice for issue
#93. The normative mapping, policy, registration, error, and resource contract
remains `core.adapters-1` in [`spec/v1/adapters.md`](../spec/v1/adapters.md) and
`interopadapter`; this package does not define a second protocol or schema
authority.

## Package and lifecycle boundary

The application reads a bounded JSON OpenAPI document, supplies one `Binding`
for every approved `operationId`, and calls `Compile`. A successful compile
returns the imported immutable `schema.Snapshot`, the direction-specific
fidelity report, and a `Compiled` value that may be registered in a
`runtime.Registry`. A rejected report must be published or inspected, but its
operations cannot be registered or invoked.

Application code owns document provenance, policy values, credential storage
and rotation, the `http.Client`, startup order, registry freeze, process drain,
and shutdown. The adapter owns strict parsing, exact local-reference
resolution, scalar conversion, origin and credential allowlists, redirect
denial, exact declared-success-status enforcement, response closure,
cancellation propagation, and byte/depth/count/fan-out limits. Forwarding and
hop-by-hop headers are never eligible credential headers. It starts no workers
and binds no listeners.

`Export` reads the canonical `schema.Document` authority and requires an
explicit method, static path, exact success status, field-mask parameter, and
security binding for every exported operation. It never infers policy or HTTP
semantics from operation names or effects.

## Executed runtime and supported subset

The supported runtime is Go 1.27 or newer on platforms supported by Go's
`net/http`. The offline profile executes against an injected `http.RoundTripper`;
it proves handler-level HTTP semantics without claiming a particular DNS, TLS,
proxy, socket, operating-system, server-framework, or native-runtime matrix.

The accepted input is JSON OpenAPI 3.2.0 using the pinned Draft 2020-12 JSON
Schema dialect. The exact subset is:

- static paths and GET, PUT, POST, DELETE, PATCH, or OPTIONS methods with unique
  `operationId` values;
- required `application/json` request bodies and one exact 2xx JSON response;
- bounded `application/json` or `application/problem+json` error declarations;
- closed objects, arrays, string enums, Boolean, String/ID, Int32, Float64,
  Int64, UInt64, Decimal, Timestamp, Duration, and UUID mappings;
- independent `required` and nullable type-array semantics;
- local `#/components/schemas/` references;
- bearer, basic, and header API-key schemes with exact application credential
  header configuration; and
- one optional string query parameter for a sorted field-mask projection.

Every root, path, operation, component container, security, parameter,
response, media, and schema object participating in an approved mapping uses a
closed member allowlist. Unbound operations never register, and schemas that
are unreachable from approved operations cannot widen the runtime profile.
Every optional OpenAPI capability outside the list above is unsupported at the
corresponding interpreted boundary. This includes YAML input, remote or dynamic
references, servers and
server variables, path templating, path/header/cookie parameters, cookies,
OAuth2 and OpenID Connect, callbacks, webhooks, links, examples, external docs,
vendor extensions, multipart/form/urlencoded bodies, content negotiation,
multiple or wildcard success responses, streaming media, SSE, WebSocket,
polymorphic `oneOf`/`anyOf`/`allOf`/`not` and discriminators, open objects,
recursive schemas without a separately expressible bound, maps, custom
scalars, schema constraints/annotations outside the accepted keywords,
nullable composite schemas, OpenAPI `float` narrowing, and combined security
schemes that collapse onto the same credential header.

`runtime-expose` server generation, listener ownership, middleware/framework
integration, retries, pagination orchestration, batching transport, and
transaction transport are unsupported by this profile. The application may
supply those policies only where `core.adapters-1` already defines the
boundary; their presence never widens this package's capability claim.

## Reproducible evidence

[`conformance/v1/openapi-adapter.json`](../conformance/v1/openapi-adapter.json)
pins the OpenAPI revision, JSON Schema dialect, dependency commits, dependency
file digests, public codes, executed cases, and unsupported direction. Run the
offline evidence with:

```sh
go test ./openapiadapter ./internal/conformance -run 'OpenAPI|Adapter'
node conformance/independent/openapi-adapter.mjs
go test ./...
go vet ./...
golangci-lint run
go generate ./...
```

The HTTP tests use no listener. Any future listener, TLS, proxy, or live-service
claim requires separate networked evidence and ephemeral runtime ports.
