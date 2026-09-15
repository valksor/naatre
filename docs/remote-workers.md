# Remote worker deployment guide

The normative contract is [`spec/v1/remote-workers.md`](../spec/v1/remote-workers.md).
The `remoteworker` package is a bounded reference gateway and framing oracle;
it deliberately does not open sockets, discover endpoints, manage production
HTTP/2 pools, or claim an independently native runtime. Issue #88 owns that
production integration.

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
| Remote worker | reference framed gateway plus fixture | PHP #58, Python #59, JS/TS #60, Rust #61 |
| HTTP server | separate transport profile | separate implementation evidence |
| Streaming | contract and credit state only | capability-specific evidence in #69 |
| Transaction | provider capability required | provider-specific evidence in #69 |

No row claims exactly-once effects, process isolation, hard termination, or
distributed atomicity. Those require a separately named provider/deployment
capability and its fault-injection evidence.

## Offline reference evidence

```sh
go test ./remoteworker -count=1
go test -race ./remoteworker -count=1
node conformance/independent/remote-worker.mjs
```

The Go test starts the independent stdio fixture on an ephemeral process pipe;
it binds no network listener. The same fixture returns the schema-defined
success and typed-error public shapes through the reference gateway.
