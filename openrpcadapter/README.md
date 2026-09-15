# OpenRPC and JSON-RPC adapter

`openrpcadapter` owns the optional `adapter.openrpc-jsonrpc-1` integration
extracted from issue #56. The package consumes the protocol-neutral
`core.adapters-1` contract from `interopadapter`; it does not redefine Naatre
schema, runtime policy, JSON-RPC, or OpenRPC semantics.

## Ownership and package boundary

- `openrpcadapter` owns the pinned OpenRPC 1.4.1 description subset, canonical
  description export, JSON-RPC 2.0 request/result/error codecs, an exact-endpoint
  `net/http` consumer, and a router-free `net/http.Handler` exposer.
- `interopadapter` and `spec/v1/adapters.md` remain authoritative for fidelity
  classifications, independent direction evidence, application-owned policy,
  operation approval, and safe failures.
- The application owns authentication, credentials, every method allowlist,
  Naatre `runtime.Descriptor`, effect, idempotency, retry/cache safety, cost,
  batching, transaction participation, authorization policy, parameter mapping,
  pagination, and notification eligibility.
- The host owns listener, TLS, HTTP version, shutdown, deployment, and
  observability configuration. Constructing a handler does not certify any of
  those host capabilities.

OpenRPC names, summaries, descriptions, error declarations, extensions, and
schema annotations never approve a method and never establish effect,
idempotency, retry, cache, transaction, authorization, or notification policy.

## Lifecycle

1. Load a locally supplied, size-bounded OpenRPC description pinned to exactly
   `1.4.1`. `ParseDescription` rejects duplicate JSON keys, unknown description
   fields, remote references, unbounded recursion, unknown schema semantics,
   and unsupported parameter structures.
2. Supply one `MethodBinding` for each approved method. Each binding contains
   the complete application-owned Naatre descriptor. `MapDescription` publishes
   a direction-specific `interopadapter.FidelityReport`; a rejected report must
   not be registered or invoked.
3. For `runtime-consume`, construct `Client` with one exact HTTP(S) endpoint,
   finite limits, and an optional application authentication callback. The
   client disables cookies and redirects, preserves scalar JSON and IDs, and
   propagates the caller context deadline and cancellation.
4. For `runtime-expose`, construct `Handler` with an exact method table and an
   authentication callback. Authenticate before dispatch, then attach the
   handler to a host-owned server. Notifications execute only when the binding
   explicitly permits them. Batches are finite and preserve independent member
   outcomes.
5. Publish the ready fidelity report before serving or calling methods. Replace
   the adapter atomically when the OpenRPC document, schema identity, wire
   version, method policy, endpoint, or limits change. Drain active host
   requests before discarding the previous instance.

The error surface is intentionally small. Go errors expose only stable
`OPENRPC_*` or `JSONRPC_*` codes. JSON-RPC responses use the standard safe
messages and at most one validated public application code; causes, upstream
bodies, endpoints, credentials, headers, stack traces, protected metadata, and
arbitrary error data are never serialized.

## Supported profile

The reference implementation supports Go 1.27 and later wherever the standard
library `net/http` client and handler contracts are available. The checked-in
evidence is OS- and architecture-neutral unit evidence and opens no listener.
It proves:

- OpenRPC 1.4.1 `info`, explicit `by-name` methods, inline content descriptors,
  results, deprecation markers, application error declarations, and local
  `components.schemas` references;
- JSON Schema object forms using the documented scalar types, nullable
  two-type arrays, properties, required fields, arrays, boolean or schema
  `additionalProperties`, enums/constants, formats, finite numeric/string/list
  constraints, and uniquely `kind`-tagged `oneOf` alternatives;
- canonical schema import/export for that subset;
- JSON-RPC 2.0 unary calls, scalar/object/array/null results, absent versus null
  IDs, string and integral numeric IDs, explicitly enabled notifications,
  bounded mixed batches, standard errors, deadline/cancellation propagation,
  and explicit authentication callbacks.

## Unsupported optional capabilities

Every capability below is outside `adapter.openrpc-jsonrpc-1` and must be
rejected or separately profiled. Its absence is not inferred support:

- OpenRPC revisions other than 1.4.1; automatic document discovery, fetching,
  watching, merging, or remote `$ref` resolution;
- OpenRPC `servers`, server variables, tags, external documentation, links,
  examples, callbacks, subscriptions, streaming methods, method-local servers,
  and component registries other than `schemas`;
- reusable component content descriptors, errors, examples, links, tags, or
  server objects, and OpenRPC reference objects outside local component schemas;
- positional or `either` parameter structures and automatic parameter-name,
  field-mask, pagination, deadline-parameter, cancellation-method, or metadata
  mappings;
- JSON Schema boolean schemas, `$id`, `$schema`, anchors, dynamic/recursive
  references, `allOf`, `anyOf`, `not`, `if`/`then`/`else`, `dependentSchemas`,
  pattern properties, unevaluated properties/items, tuple/prefix items,
  contains, content/media keywords, defaults, read/write flags, discriminator
  extensions, non-RE2 patterns, untagged or overlapping unions, and vocabulary
  extensions;
- fractional JSON-RPC IDs, server-generated notification acknowledgements,
  notification retries, automatic cancellation RPC methods, progress,
  streaming, subscriptions, pub/sub, WebSocket, SSE, IPC, stdio, message queues,
  HTTP GET, multipart, compression, cookies, redirects, proxies, service
  discovery, load balancing, retries, connection pooling guarantees, or
  transport metadata forwarding;
- automatic authentication schemes, credential storage/refresh, authorization
  inference, CSRF/CORS policy, rate limiting, persisted operations,
  transactions across batch members, atomic batches, or retry/idempotency
  inference;
- listener/TLS/QUIC lifecycle, framework bindings, native-runtime bindings,
  non-Go implementations, generated clients/servers, and compatibility or
  certification claims beyond the checked-in profile evidence.

## Reproducible offline evidence

From the repository root, with Go 1.27 and Node 24 available:

```sh
go test ./openrpcadapter -count=1
go vet ./openrpcadapter
node conformance/independent/openrpc-jsonrpc.mjs
```

`conformance/v1/openrpc-jsonrpc.json` records the exact OpenRPC/JSON-RPC pins,
dependency and implementation SHA-256 identities, fixture classes, fidelity
claims, commands, and unsupported capabilities. The independent script checks
those identities and evidence semantics without network access.
