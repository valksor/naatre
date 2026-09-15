# JavaScript and TypeScript SDK

This directory contains the dependency-free, ESM-only
`sdk.typescript.core-1` reference client. The runtime is ordinary JavaScript;
TypeScript is used for generated declarations and strict compile-time
verification, not as a substitute for runtime validation.

The core provides canonical persisted requests, strict manifests, selected
result states, simultaneous partial data and errors, prototype-safe decoding,
and lossless codecs for integers, decimal values, nanosecond timestamps, and
bytes. `undefined`, sparse arrays, unsafe JSON integers, direct `BigInt`
serialization, and `Date` timestamp coercion are rejected.

## Server handlers and remote workers

The `@naatre/sdk/worker` ESM subpath provides the framework-neutral
`worker.javascript-typescript-1` reference implementation. Handlers are plain
JavaScript functions; generated TypeScript `Handler` input and server-output
interfaces come from the same `sdk.generation-1` model as client operations.
Server outputs are complete schema values and are deliberately distinct from
client partial-result `Selected` types. Runtime validation always runs before
handler entry and again before a worker result is returned.

Registration is explicit and stored in `Map`/null-prototype structures.
`__proto__`, `constructor`, and `prototype` schema or registration names are
rejected, and invocation dispatch never traverses object properties. Each
invocation authenticates its delegated context and receives fresh tenant,
principal, cache, loader, and mutable-state objects. Reusing request-state
objects across warm or concurrent requests is rejected.

Handler context exposes an `AbortSignal` and `onCleanup`. Cancellation runs
registered I/O cleanup immediately, but inline synchronous CPU work cannot be
forcibly interrupted and remains in `metrics().activeInvocations` until it
settles. CPU isolation must be selected explicitly with a `worker` or `process`
execution profile and a host-supplied executor. Streaming handlers return an
`AsyncIterable`; reads are pull-driven, concurrent reads are rejected, frame
and byte limits enforce backpressure, and `return()` closes both the source and
request resources.

`createFetchWorkerAdapter` maps the three remote-worker endpoints to a pure
`Request => Promise<Response>` function and owns no listener. Concrete Node,
Bun, Deno, and edge process/network lifecycle bindings are extracted to #101.
The core conformance harness still executes each runtime separately instead of
inferring support from the presence of global `fetch`. See the framework-neutral
[`examples/worker.ts`](examples/worker.ts) binding.

```sh
node conformance/independent/typescript-worker-runtime.mjs
bun conformance/independent/typescript-worker-runtime.mjs
npx --yes --userconfig=/dev/null deno@2.9.6 run --allow-read \
  conformance/independent/typescript-worker-runtime.mjs
conformance/edge/run-typescript-worker-workerd.sh
```

The workerd wrapper pins `1.20260914.1` and binds only an OS-selected ephemeral
loopback port. The production HTTP/2 listener, runtime shutdown/drain hooks,
worker-thread/process supervision, framework bindings, and deployment
certification remain #101/#69 work and are not claimed by this core package.

Regenerate and verify the checked-in bindings with:

```sh
npm --prefix sdk/typescript run generate
npm --prefix sdk/typescript test
npm --prefix sdk/typescript run typecheck
git diff --exit-code -- sdk/typescript/generated
```

The package pins TypeScript 7.0.2 and enables `strict`,
`noUncheckedIndexedAccess`, and `exactOptionalPropertyTypes`. The generated
source records `naatre.generator.typescript-sdk-1`; operation and manifest
digests derive from the shared language-neutral model and canonical reference
output.

## Transport adapters and runtime matrix

The separately executable `sdk.typescript.adapters-1` profile exports the
Fetch adapter from `@naatre/sdk/fetch` and the WebSocket adapter and standalone
SSE decoder from `@naatre/sdk/websocket`. The root package also re-exports all
three functions. All entry points are dependency-free strict ESM.

Fetch execution uses canonical POST request bytes, bounded incremental reads,
manual 307/308 redirects, strict response media types, active abort handling,
and partial-data decoding. Cross-origin redirects are denied unless their
origin is explicitly listed. Authentication and tenant headers are stripped
on an allowed cross-origin redirect unless that destination origin is also in
`credentialOrigins`; Fetch credentials are always `omit` or `same-origin`.
Rejected responses and redirects close their response bodies. Compressed byte
limits validate the declared wire length; decompressed byte limits apply to
the bytes exposed by the Fetch implementation after content decoding.

POST-SSE and WebSocket streams validate bounded UTF-8 JSON frames, sequence
ordering, exact duplicates, bounded WebSocket queues, terminal/truncated
outcomes, and the special `history-unavailable` recovery end. Cancellation
closes the reader or socket. A caller reconnects SSE explicitly by supplying
the last successfully accepted cursor as `lastEventId`; the adapter validates
and sends `Last-Event-ID`, exposes the same value to the authentication callback
for binding, and strips it with credentials on an untrusted cross-origin
redirect. Automatic reconnect policy remains application-owned. WebSocket
support is optional, accepts only `wss:` endpoints and text frames, and sends
the same canonical request envelope after the connection opens.

Issue #23 and `core.streaming-1` remain the authority for frames, replay,
terminal states, and lifecycle. Issue #72 owns only the concrete Fetch POST SSE
binding and separately advertised `stream.websocket-1` adapter in this package;
the unary Fetch behavior remains part of the TypeScript adapter surface from
issue #75. The transport profile lifecycle is tied to fixture suite `1.0.0`,
runtime `naatre.typescript.runtime-1`, and the exact dependency revisions in
`conformance/v1/typescript-adapters.json`.

The current checked evidence covers Node 26.8.2, Bun 1.4.2, and Deno 2.9.6 on
Darwin arm64. Chrome headless shell 153.0.8010.36 and workerd package
1.20260914.1 have reproducible harnesses but are explicitly not claimed until
those harnesses execute against this exact revision. Run the portable profiles
and targeted Node tests with:

```sh
node conformance/independent/typescript-adapter-runtime.mjs
bun conformance/independent/typescript-adapter-runtime.mjs
npx --yes --userconfig=/dev/null deno@2.9.6 run --allow-read \
  conformance/independent/typescript-adapter-runtime.mjs
node --test sdk/typescript/runtime/exports.test.mjs \
  sdk/typescript/runtime/transport.test.mjs
```

For the selected browser, set `CHROME_HEADLESS_SHELL` to the exact
153.0.8010.36 executable and run the wrapper. It binds an OS-selected ephemeral
loopback port, verifies the browser version and DOM result, and replays captured
browser diagnostics on failure:

```sh
conformance/browser/run-typescript-adapter-runtime.sh
```

For the selected edge runtime, run the wrapper below. It pins workerd, binds an
OS-selected ephemeral loopback port, probes the worker, validates the result,
and stops the runtime:

```sh
conformance/edge/run-typescript-adapter-workerd.sh
```

The complete unsupported optional-capability list is automatic reconnect and
replay policy, native `EventSource` POST authentication, bidirectional client
stream messages after WebSocket establishment, binary or compressed WebSocket
frames, WebSocket upgrade authentication headers, non-WSS sockets, browsers
other than the recorded Chrome revision, edge runtimes other than the recorded
workerd revision, Node/Bun/Deno revisions other than those recorded in the
fixture, framework bindings, native mobile runtime bindings, service-worker
offline replay, durable client cursor storage, and deployment certification.
The core profile remains independently verifiable and does not acquire those
transport claims merely because the package root re-exports the adapter
functions.

## Collection-query mapping

This directory contains the first generated client mapping for
`collection.query-1`. It is a fixture-backed integration slice, not yet the
generator plugin host owned by issues #35 and #76.

`generated/collection-query.ts` is produced from the authorized descriptor in
`conformance/v1/collections.json` by the dependency-free JavaScript generator:

```sh
node conformance/independent/generate-collection-query.mjs \
  > sdk/typescript/generated/collection-query.ts
```

The output is deterministic and records the generator version, source profile,
and exact source SHA-256. CI compares regenerated bytes with the checked-in
file. The generated unions preserve field identity, exact scalar wire types,
declared operators, list-element scope, sort direction, and null placement.

## Lifecycle and support boundary

The mapping is owned by the first-party TypeScript client surface and follows
the lifecycle of fixture suite `1.0.0`. It has no runtime or platform
dependency and is suitable for TypeScript source consumers on the portable SDK
platforms declared in `docs/governance/bootstrap.md`.

No transport, request execution, packaging, framework integration, custom
scalar mapping, case folding, regex, full-text, geospatial query execution, or
PostgreSQL/MySQL translator is advertised here. The Go reference evaluator and
SQLite provider remain the only executed provider evidence. Unsupported
provider capabilities fail with `FILTER_UNSUPPORTED`; the complete official
language and release matrix remains owned by issue #69.
