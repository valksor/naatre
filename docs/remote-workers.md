# Remote worker deployment guide

The normative contract is [`spec/v1/remote-workers.md`](../spec/v1/remote-workers.md).
The `remoteworker` package is the bounded Go execution gateway and framing
oracle. `HTTPTransport` is the issue #88 production transport: it sends the
contract's length-delimited envelopes to the fixed Register, Invoke, and Cancel
paths over HTTPS, requires an HTTP/2 response, rejects redirects and protocol
downgrades, and bounds every frame. `ReferenceGateway` remains the sole Go
admission, delegated-identity, retry, completion, reference, deduplication, and
public-error boundary. Neither type defines a second schema.

`FramedTransport` and the `conformance/independent/remote-worker*.mjs` workers
implement the same envelopes over stdio only. The Node gateway fixture is the
language-neutral reference worker: it imports no Go source and proves unary,
typed-error, and server-streaming interoperability against the shared fixtures.
Stdio is a conformance and supervised-process profile, never the advertised
production transport.

## Package and runtime boundary

Applications construct one `HTTPTransport` with an exact HTTPS endpoint, a
finite frame limit, and an application-owned `http.Client`. The client owns
TLS roots, client certificates or other workload identity, finite HTTP/2
connection-pool settings, proxy policy, idle rotation, and shutdown. The
gateway configuration separately pins worker, service, endpoint, audience,
schema revision/digest, request/response/stream limits, retained invocation
IDs, opaque references, attempts, and concurrent work.

Call `Register` and wait for the exact handshake before opening admission.
Call `Invoke` for unary work or `InvokeStream` only for a handler that requires
and negotiated `server-streaming-1`. A stream starts with finite positive frame
and byte credit, uses `core.streaming-1` frames, and owns its response body and
invocation reservation until a terminal frame, failure, or idempotent `Close`.
The invocation context owns deadline and cooperative cancellation; `Cancel`
records only the worker's schema-defined disposition.

The source-supported boundary is Go 1.27 on platforms supported by Go's
`net/http` HTTP/2 client. The checked evidence executes with Go 1.27.1 and Node
26.8.2 on Darwin arm64 without a listener. Production TLS negotiation and
other operating-system/architecture combinations remain integration evidence,
not claims made by the offline fixture.

## Deployment models

| Model | Process owner | Planning and completion | Business handlers | Evidence row |
| --- | --- | --- | --- | --- |
| Go embedded runtime | application operator | Go runtime process | same Go process | native runtime |
| Independent native Naatre runtime | language runtime operator | independent runtime | its native process model | native runtime, only after independent evidence |
| Go execution gateway with remote workers | gateway operator plus each worker service owner | Go gateway | authenticated remote worker processes | remote worker |

An HTTP client is none of these server models.

## Ownership checklist

| Asset | Required owner and lifecycle |
| --- | --- |
| Gateway process | Platform operator supervises start, readiness, drain, and exit. |
| Worker processes | Each service operator supervises start, registration, local resources, drain, and exit. |
| Connection pools | Gateway operator configures finite per-endpoint and aggregate pools, queue bounds, idle rotation, reconnect budgets, and drain. |
| Credentials | Platform identity owner provisions workload identity, pins audience and endpoint, rotates trust without accepting both schemas accidentally, and revokes departed workers. |
| Schema and protocol upgrades | Gateway release owner admits a complete immutable revision; worker release owners register that exact revision before traffic. Old sessions drain and never share new request state. |
| Request-scoped state | Gateway owns public request, invocation, attempt, authorization, admission, retry, and completion state. Workers own handler-local state only until terminal cleanup. |
| Opaque references | Worker owns payload and destruction; gateway owns scope, session, expiry, count, and cleanup metadata. Disconnect and session replacement invalidate them. |
| Queues and fan-out | Gateway operator configures aggregate finite bounds. A service operator may impose a stricter worker queue and must return overload before handler execution. |
| Source streams | The side that creates a source owns closure; both sides enforce frame/byte credit, deadline, cancellation, terminal state, and half-close. |
| Logs and diagnostics | Each process owner stores private diagnostics. The gateway alone maps validated safe data and errors into the public response. |

## Upgrade and failure sequence

1. Configure the endpoint allowlist, service identity, audience, schema
   revision/digest, capabilities, and finite resource bounds.
2. Start the worker under its supervisor. Authenticate the connection and
   validate the complete registration before routing any invocation.
3. Admit, authorize, and validate each invocation at the gateway; verify
   delegated context and authorize again at the service boundary.
4. On output, validate correlation and schema identities, payload bounds,
   typed errors, references, and output schema before public completion.
5. On upgrade, establish and admit the new session first, direct only matching
   schema work to it, drain the old session, close sources, and clean up old
   references and credentials.
6. On disconnect or process death, apply the effect-specific retry table. A
   mutation after a possible write is indeterminate without admitted
   idempotency evidence; a transaction requires provider capability; a
   subscription requires retained resume evidence.

## Support matrix

| Surface | Go reference in #51 | Other languages |
| --- | --- | --- |
| Client | separate SDK profiles | separate SDK profiles; final matrix #69 |
| Codec | shared `naatre.json-1` | shared `naatre.json-1` |
| Native runtime | Go runtime evidence is separate | not established by this profile |
| Remote worker | Go gateway, HTTP/2 transport, and independent stdio fixture | PHP #58, Python #59, JS/TS #60, Rust #61 |
| HTTP server | outbound TLS HTTP/2 worker transport only | inbound server profiles remain separate |
| Streaming | bounded server-stream receive path with fixed initial credit | client/bidirectional and broader matrix #69 |
| Transaction | provider capability required | provider-specific evidence in #69 |

No row claims exactly-once effects, process isolation, hard termination, or
distributed atomicity. Those require a separately named provider/deployment
capability and its fault-injection evidence.

## Unsupported optional capabilities

`implementation.go.remote-worker-1` does not implement or claim HTTP/1.1
fallback, HTTP/3, QUIC, WebSocket, gRPC or Connect compatibility, compression,
content negotiation beyond `application/naatre-worker+json`, endpoint discovery,
redirects, automatic load balancing, automatic credential creation or refresh,
proxy certification, framework integration, inbound worker serving, client
streaming, bidirectional streaming, dynamic stream-credit replenishment,
cross-process transaction coordination, automatic subscription replay after an
opened stream fails, forced handler termination, sandbox/process isolation,
exactly-once execution or effects, distributed atomicity, independent native
runtime certification, non-Go gateway implementations, or production
deployment certification. The application may supply a configured HTTP/2 pool
and mTLS identity; doing so does not turn those application-owned facilities
into capabilities of this package.

## Offline reference evidence

```sh
go test ./remoteworker -count=1
go test -race ./remoteworker -count=1
go test ./internal/conformance -run TestRemoteWorkerGatewayEvidence -count=1
node conformance/independent/remote-worker-gateway.mjs
node conformance/independent/verify-remote-worker-gateway.mjs
```

The Go tests use an in-memory `http.RoundTripper` and start the independent
stdio fixture on process pipes; they bind no network listener. The same fixture
returns the schema-defined success, typed-error, and ordered streaming shapes
through the reference gateway. The machine-readable evidence is
`conformance/v1/remote-worker-gateway.json`; it pins the exact #51 and
`core.streaming-1` dependency revisions, source bytes, cases, commands, and
unsupported boundary.
