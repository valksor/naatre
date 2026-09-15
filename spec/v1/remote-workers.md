# Remote worker protocol

This document defines `worker.remote-1`, the language-neutral contract between
a Naatre execution gateway and an out-of-process business-handler worker. Go is
the reference gateway implementation, but no Go ABI, Go source import, cgo, or
in-process FFI is part of this profile. Passing this profile establishes a
remote-worker path only; it does not establish an independent native runtime.

## Deployment models and profile boundary

- **RW-001 Deployment models:** A deployment MUST identify itself as exactly
  one of: a Go embedded runtime whose handlers execute in the Go process; an
  independently implemented native Naatre runtime that owns the entire
  validation-through-completion pipeline; or a Go execution gateway that owns
  that pipeline and invokes registered remote handlers. An HTTP client alone
  MUST NOT be advertised as server or native-runtime support.
- **RW-002 Ownership boundary:** In the gateway model, the gateway MUST own
  document validation, planning, public-request admission, authorization,
  retry decisions, public response completion, output validation, and public
  errors. A worker owns only its admitted handler execution and local resource
  cleanup. Both the gateway and the worker's service trust boundary MUST
  authorize the delegated request before business side effects.
- **RW-003 Portable boundary:** A conforming worker MUST be implementable from
  this document, `remote-worker.schema.json`, the shared schema/generator
  contracts, and portable fixtures without importing Go source or using a Go
  ABI. Server interfaces MUST be generated separately from client models and
  MUST reference `core.schema-1` and `sdk.generation-1`, never a language-local
  competing schema.

## Transport and framing

- **RW-100 Transport selection:** The initial production transport MUST be TLS-
  authenticated HTTP/2 carrying `application/naatre-worker+json` length-
  delimited messages. The gateway performs a unary registration/schema
  handshake at `POST /naatre.remote-worker.v1.Worker/Register`, opens unary or
  streaming calls at `POST /naatre.remote-worker.v1.Worker/Invoke`, and sends a
  cancellation control call at `POST /naatre.remote-worker.v1.Worker/Cancel`.
  A non-2xx response is transport failure and its bounded body is private
  diagnostics, never a worker result. HTTP/1.1 MAY carry unary calls only when
  registration explicitly advertises `unary-1` over HTTP/1.1. Framed stdio MAY
  be used only by a conformance or supervised-process profile and MUST NOT be
  represented as the production transport.
- **RW-101 Frame:** Each message MUST begin with a one-byte flags field followed
  by an unsigned four-byte big-endian payload length and exactly that many
  payload bytes. Flag `0` carries one JSON envelope and flag `1` is an orderly
  stdio-profile end marker; other flags, zero lengths, truncated payloads, and
  configured-limit violations MUST fail the stream. HTTP/2 END_STREAM, rather
  than flag `1`, provides production half-close.
- **RW-102 Flow control:** HTTP/2 connection and stream windows remain the byte
  transport limit. A receiver MUST release transport credit only after it has
  admitted or durably handed off a frame, and a source stream MUST additionally
  consume explicit positive frame-and-byte credits. Sending beyond either
  application credit, negotiated frame count, total bytes, or queue capacity
  MUST stop dispatch and yield `OVERLOADED` or terminate the affected stream.
- **RW-103 Half-close and disconnect:** Request half-close means no more source
  frames and does not cancel already admitted work; response half-close follows
  one terminal result or stream frame. A disconnect MUST cancel cooperative
  query work, preserve committing or indeterminate effects, close owned source
  streams, invalidate connection-scoped references, and feed the retry rules in
  RW-500 through RW-503. It is not proof that a handler did or did not execute.
- **RW-104 Authentication and negotiation:** Before registration, the gateway
  MUST authenticate the worker with mutual TLS or application-owned workload
  identity and bind it to one configured endpoint, worker identity, and
  audience. Registration MUST negotiate the exact protocol version and unary,
  client-streaming, server-streaming, and bidirectional capabilities
  independently; no common required version or capability fails registration.
  The request `Naatre-Worker-Protocol` header and every envelope MUST carry the
  same exact `naatre.remote-worker.v1` value; intermediaries MUST NOT negotiate
  or rewrite it.

## Registration and admission

- **RW-200 Registration manifest:** Registration MUST carry the protocol
  version, stable worker identity, authenticated service identity, audience,
  operator-allowlisted endpoint identifier, exact schema revision and digest,
  codecs, finite worker limits, capabilities, and a finite handler manifest.
  Each handler MUST declare a stable handler ID, input and output schema IDs,
  codec, effect kind, and required capabilities.
- **RW-201 Stable identity:** Handler IDs are deployment-stable schema
  identities, not function names or memory addresses. Schema revisions and
  digests MUST match the gateway's immutable admitted schema. Handler IDs,
  schema IDs, effects, codecs, capabilities, and finite limits MUST be unique
  where required and validated before the worker becomes eligible for routing.
- **RW-202 Mismatch barrier:** Malformed manifests, untrusted identity or
  endpoint, version skew, schema mismatch, duplicate or unknown handlers,
  unsupported codecs, missing required capabilities, and transaction handlers
  without `transaction-provider-1` MUST fail registration or admission before
  remote dispatch or any business callback.
- **RW-203 Resource admission:** Operators MUST configure finite endpoint
  allowlists, connection pools, reconnect attempts, in-flight work, queues,
  fan-out, request and response bytes, stream frames and bytes, reference
  counts and lifetimes, and cleanup bounds. One public operation split into a
  batch, fan-out, retry, or stream MUST reserve its aggregate bound before
  dispatch and cannot evade process admission accounting.

## Identity, values, and references

- **RW-300 Correlation identities:** A public Naatre request ID, stable handler
  invocation ID, per-delivery attempt ID, caller idempotency key, and internal
  parent reference are distinct fields. Retries MUST retain the invocation ID
  and create a fresh attempt ID. None may be substituted for another or exposed
  as proof of execution.
- **RW-301 Delegated context:** The gateway MUST derive principal and tenant
  only from trusted authentication state and send an audience-bound,
  expiry-bound, least-authority delegated context. A worker MUST authenticate
  its gateway peer, verify audience, expiry, request, handler, and schema
  bindings, and reauthorize at its own service boundary. Document fields,
  handler inputs, parent values, headers, and reference strings MUST NOT assert
  principal or tenant.
- **RW-302 Parent values:** Portable parents MUST travel as schema-defined JSON
  values under `naatre.json-1`; arbitrary native objects, pointers, handles,
  class instances, or language serialization formats MUST NOT cross the
  boundary.
- **RW-303 Opaque references:** A parent MAY instead use a scoped opaque
  reference issued by the selected worker. The gateway MUST retain its worker,
  session, request or invocation scope, expiry, ownership, and cleanup record;
  it MUST NOT interpret the worker-private payload. Unknown, expired, wrong-
  session, wrong-owner, or exhausted references fail before handler execution.

## Invocation, completion, and streams

- **RW-400 Invocation envelope:** An invocation MUST carry the protocol,
  distinct correlation identities, handler ID, exact schema revision,
  absolute deadline, delegated context, schema-defined input and parent, and
  optional idempotency key or resume cursor only when admitted. The worker MUST
  reject expired deadlines and cancellation remains cooperative unless a
  stronger provider capability is separately advertised and evidenced.
- **RW-401 Typed results:** A result MUST echo protocol, invocation, attempt,
  and schema identities and contain only schema-defined JSON data, bounded
  typed errors, and admitted opaque reference grants. Worker error codes,
  messages, retry hints, and namespaced details are untrusted input: the gateway
  MUST validate and redact them, validate output against the admitted output
  schema, and run public completion before using or publishing the result.
- **RW-402 Cancellation:** Cancellation is correlated by public request and
  invocation ID. `requested` means only that a signal was accepted;
  `acknowledged` means the worker reached its cancellation boundary;
  `too-late` and `unsupported` are terminal dispositions for that request.
  None MUST be represented as proof that an external effect was rolled back or
  that in-process work was forcibly terminated.
- **RW-403 Batch calls:** A batch MUST carry individually correlated invocation
  items and reserve aggregate bytes, calls, attempts, and response capacity.
  Admission, authorization, error, idempotency, deadline, and retry decisions
  remain per item unless a transaction-capable provider explicitly admits the
  whole batch; transport grouping alone MUST NOT imply atomicity.
- **RW-404 Source streams:** Client, server, and bidirectional source streams
  MUST negotiate separately, preserve one invocation identity, use ordered
  frame sequence and terminal states from `core.streaming-1`, consume the
  explicit credit window in RW-102, propagate half-close, cancellation, and
  deadlines, and close worker-owned sources on every terminal path.

## Disconnect, retry, and process death

- **RW-500 Queries:** A query MAY retry a before-write or after-write disconnect
  within its admitted attempt and deadline budgets because its entire
  transitive handler graph is read-only. Each delivery uses a fresh attempt ID;
  a duplicate invocation at one live worker session MUST be rejected or joined
  without invoking the handler twice.
- **RW-501 Mutations:** A mutation MAY retry before any invocation bytes are
  written. After a write, replay requires an admitted idempotency key plus
  `idempotency-replay-1` evidence satisfying `core.reliability-1`; otherwise the
  gateway MUST report an indeterminate outcome and MUST NOT replay merely
  because the connection or worker process died.
- **RW-502 Transactions:** A transaction handler MUST advertise
  `transaction-provider-1` and the gateway MUST admit that capability before
  dispatch. A transport transaction is not a distributed transaction. After a
  write or unknown commit, disconnect or process death is indeterminate and
  MUST NOT be replayed without separate provider reconciliation evidence.
- **RW-503 Subscriptions:** A subscription MAY reconnect only with
  `subscription-resume-1`, an admitted bounded resume cursor, retained history,
  and a new attempt ID. Otherwise disconnect terminates the subscription and
  the client must start a new public request; a source stream is never replayed
  from an unacknowledged arbitrary native object.
- **RW-504 Process death and duplicates:** Process death before a confirmed
  write follows the before-write rule. Death after a write follows the effect-
  specific rule and invalidates session references. Invocation deduplication is
  scoped, bounded, and retained only as advertised; it MUST NOT be described as
  exactly-once execution or exactly-once external effects.

## Deployment ownership and conformance

- **RW-600 Deployment ownership:** The deployment guide MUST name the owner of
  gateway and worker processes, supervisors, connection pools, endpoint
  allowlists, credentials and rotation, schema/protocol upgrades, worker drain,
  request-scoped state, opaque references, queues, source streams, logs, and
  cleanup. A worker upgrade MUST register and pass admission before receiving
  traffic; old sessions drain without sharing request-scoped state.
- **RW-601 Claims and evidence:** Implementations MUST NOT claim exactly-once
  effects, process isolation, hard termination, distributed atomicity, or a
  native runtime unless a separately named deployment/provider capability and
  fault-injection evidence establishes that claim. `worker.remote-1` alone
  proves only one gateway-to-worker conformance path.
- **RW-602 Support matrix:** Published support MUST use separate rows for
  client, codec, native runtime, remote worker, HTTP server, streaming, and
  transaction support. PHP, Python, JavaScript/TypeScript, and Rust worker
  implementations are owned by issues #58 through #61; the independent runner
  and complete matrix are owned by #69; the production Go gateway and worker
  integration are owned by #88.

The exact language-neutral fixtures are in
`conformance/v1/remote-workers.json`. They use the shared schema and generator
profiles from issues #35 and #36. The dependency-free stdio fixture is a
protocol oracle for conformance tests, not a JavaScript/TypeScript application
server implementation.
