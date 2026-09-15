# Rust SDK

`naatre-sdk` is the runtime-neutral Rust binding for `sdk.rust.core-1`. It
provides serde-compatible generated operation types, explicit missing/null and
selection states, lossless canonical scalar wrappers, persisted requests,
pagination values, and executor-neutral ownership handles.

The minimum supported Rust version is 1.85 with edition 2024. Features are
additive:

- default: client values, generated bindings, and transport ownership traits;
- `generator`: deterministic generation from `naatre.generator-model-1`;
- `server`: generated handler traits, explicit remote-worker registration,
  executor-neutral dispatch, request-local resource hooks, and framed serde
  codecs for `worker.remote-1`;
- `tokio`: task-local unary and stream closure adapters for futures polled by a
  caller-selected Tokio runtime. This feature deliberately introduces no Tokio
  crate dependency, runtime construction, task spawning, or network backend.
- `tokio-runtime`: issue #100's worker-only integration with exact Tokio
  `1.53.1`. It provides bounded async and blocking work owners for #61's
  `HandlerContext`; the embedding application still constructs and shuts down
  the runtime.
- `axum`: issue #100's worker HTTP integration with exact Axum `0.8.9`. It
  includes `server` and `tokio-runtime`, exposes an in-memory-testable router,
  and accepts a caller-bound listener for optional serving. It does not add a
  client transport or own TLS/process configuration.

The supported build matrix is:

```sh
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --no-default-features
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --features generator
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --features server --test server
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --features tokio
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --features tokio-runtime
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --features axum --test axum_worker
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --all-features
cargo +1.85.0 build --manifest-path sdk/rust/Cargo.toml --release --features axum
cargo +stable fmt --manifest-path sdk/rust/Cargo.toml --check
cargo +stable clippy --manifest-path sdk/rust/Cargo.toml --all-targets --all-features -- -D warnings
```

Every command accepts `--locked --offline` after dependencies from the checked
`Cargo.lock` have been cached. `conformance/v1/rust-async-adapters.json` records
the exact #39 commit, core fixture digest, lockfile digest, feature matrix, and
portable verification command. Run its independent profile from the repository
root with:

```sh
node conformance/independent/verify-rust-async-adapters.mjs
node conformance/independent/verify-rust-tokio-axum.mjs
```

Regenerate the checked artifacts from the repository root:

```sh
go generate ./sdk/rust
git diff --exit-code -- sdk/rust/generated
```

`Presence<T>` represents nullable optional input fields: omission produces
`Missing`, JSON `null` produces `Null`, and a decoded value produces `Value(T)`.
`Optional<T>` represents non-null optional fields and rejects explicit null.
`Selected<T>` additionally represents pending, failed, and skipped result
states. Plain `Option<Option<T>>` is not used for wire presence.

`RequestHandle` and `StreamHandle` own a cancellation token and cancel it when
dropped before completion. The core starts no detached task; adapter authors
must retain abort/join ownership for every task they spawn. Cancellation cannot
claim that an external side effect has stopped.

With the `tokio` feature, `TokioTransport` and `TokioStreamTransport` erase
closure types at the package boundary while leaving their futures and streams
on the caller's task. `TokioFuture` and `TokioByteStream` map adapter errors to
the stable public codes `CLIENT_TRANSPORT_FAILED` and `CLIENT_STREAM_FAILED`;
private error codes and messages are discarded. Dropping either wrapper drops
its underlying future or stream synchronously. That releases locally owned I/O
state but does not assert that a request or mutation already accepted by a peer
was rolled back. The core `CLIENT_DEADLINE_EXCEEDED`,
`CLIENT_RESPONSE_TOO_LARGE`, and `CLIENT_FRAME_TOO_LARGE` boundaries still
apply.

`Transport` and `StreamTransport` are `Send + Sync`; returned futures and
streams are `Send`. `Client` owns its transport, and executable client
transports must be `'static` so a dropped request cannot retain a borrowed
transport behind executor-owned work. Operation variables are serialized
before transport work starts, while decoded result ownership is returned to
the caller.

## Ownership, lifecycle, and support

Issue #39 owns `sdk.rust.core-1`, generated wire types, scalars, presence, and
the executor-neutral transport contract. Issue #80 owns only the optional
`sdk.rust.adapters-1` task-local adapter profile. Both profiles follow fixture
suite `1.0.0`; changing the core transport or wire contract requires a new core
profile rather than authority in the adapter slice.

The supported package boundary is edition 2024 on Rust 1.85 or newer. The core
profile is executor-neutral. The optional adapter profile supports Tokio tasks
on platforms where the caller can poll a `Send + 'static` standard Rust future;
it calls no Tokio API, so it neither pins nor certifies a Tokio crate revision.
The checked conformance harness is platform-independent and uses no listener,
network, clock, filesystem, or background task.

Unsupported optional capabilities in `sdk.rust.adapters-1` are concrete HTTP and HTTP/2 clients,
authenticated POST SSE, WebSocket, QUIC, compression/decompression, TLS and
certificate policy, redirects, proxy handling, credential or tenant injection,
automatic retries, reconnect/replay, pagination orchestration, bounded task or
message queues, Tokio runtime construction, `tokio::spawn` ownership,
`LocalSet` and non-`Send` futures, Tower/Hyper/Reqwest/Axum integration, WASM
and embedded executors, mobile bindings, native-runtime certification, and
deployment certification. The complete official SDK matrix stays owned by
issue #69.

## Remote-worker server core

The `server` feature is a separate additive boundary: default model/client
users do not compile the worker core, and the core has no Tokio, Axum, HTTP, or
Go-runtime dependency. Generation emits one `Send + Sync` handler trait and one
explicit registration function per shared-schema operation. Implementing a
trait alone exposes nothing. `ServerBuilder::build` succeeds only when every
advertised manifest row has an exact registered descriptor and typed handler;
input and output schema callbacks run around every application call.

Inputs are decoded into owned generated values before a handler is polled.
`HandlerContext` is borrowed for exactly the handler future's lifetime. Its
principal, loader, and optional transaction are newly created by the
application's `RequestScopeFactory`, are not `Clone`, and are consumed by the
terminal `RequestResources::finish` hook. Concurrent requests therefore do not
receive state from another request through the worker core. Resource
implementations may themselves use shared storage only when the application
has authorized that sharing.

`WorkerCore::invoke` returns a `Send` future and never chooses an executor.
Dropping it requests cooperative cancellation, releases the active invocation,
and drops the handler future and request scope. It does not prove that a task
spawned elsewhere, blocking system call, transaction commit, or external write
has stopped or rolled back. Handlers transfer spawned tasks, blocking jobs, and
source streams into the context with the corresponding `own_*` method. Normal
completion cancels and awaits each owned object's `shutdown` future. Abrupt
future drop calls `cancel` and drops ownership but cannot synchronously await a
join; an adapter or process supervisor remains responsible for any stronger
termination guarantee. Cancellation acknowledgements mean only that the token
was observed.

Each `OwnedWork` source stream is shut down under the same rule, and all stream
credit/frame/byte enforcement remains the adapter's responsibility. The core
framing codec accepts exactly one flag-zero, four-byte big-endian,
length-delimited JSON message and rejects duplicate keys, unknown fields,
truncation, trailing bytes, and configured-limit violations. `Presence<T>`,
canonical scalar wrappers, generated open variants, and typed `WorkerError`
values preserve the shared JSON contract. Schema-invalid or oversized handler
output returns `OUTPUT_COMPLETION` at the worker boundary and is never emitted
as public data.

The `unwind` development/test profile catches handler unwinds at the future
polling boundary and returns a private `INTERNAL` server failure after dropping
request state. The release profile is explicitly `panic = "abort"`: no
destructor, hook, cancellation, or panic recovery is promised after an abort;
the process supervisor owns failure and restart. Tests exercise unwind
containment, while the feature matrix compiles the aborting release profile.

Supported targets are targets with Rust 1.85+, `std`, threads, atomics, and a
`Send` executor chosen by the application. WASM without those facilities,
embedded/no-std targets, process supervision, TLS transports, and non-`Send`
local executors are not certified by `worker.remote-1.rust`. The separately
published result is `conformance/v1/rust-worker.json`; it claims a Go-gateway
to Rust-worker path only and explicitly does not claim an independent native
execution runtime. See the complete supervised-stdio example at
`examples/go-gateway-rust-worker/README.md`.

## Tokio worker and Axum integration

Issue #61 remains the sole owner of the worker protocol, generated handler
contract, validation, registration, request resources, and result schema.
Issue #100 owns only `worker.remote-1.rust-tokio-axum-1`: the optional
`tokio-runtime` task/blocking-work owner and the optional `axum` HTTP adapter.
The adapter reuses #61's `Registration`, `WorkerInvocation`, `CancelRequest`,
strict framed codec, `WorkerCore`, and stable failure codes; it defines no
second wire shape or dispatch authority.

`TokioWorkerSpawner` binds to an application-owned Tokio runtime handle. Its
finite async and blocking semaphores reject excess work with `OVERLOADED`
before spawning. A returned owner must be transferred to `HandlerContext`.
Async cancellation aborts and joins the task during orderly cleanup. Blocking
work receives a cooperative `Cancellation` token and remains joined during
orderly cleanup; Tokio cannot forcibly terminate a blocking syscall or closure
that ignores that token. Abrupt future/task drop requests cancellation and
releases core ownership, but cannot promise a blocking closure, external side
effect, or transaction was rolled back. Detached work and unbounded queues are
not supported.

`AxumWorker::router` exposes only the normative `Register`, `Invoke`, and
`Cancel` RPC paths using the exact framed `application/naatre-worker+json`
transport and `Naatre-Worker-Protocol` negotiation. It starts no task and binds
no listener. `AxumWorker::serve` accepts an already-bound Tokio `TcpListener`
and a caller-owned shutdown future; graceful shutdown drains admitted Axum
requests. The application owns socket address selection, TLS and client
certificate policy, HTTP/2 ALPN, trusted proxy policy, runtime construction,
signals, admission opening, and process supervision. Tests call the router
directly and bind no port.

The source-supported boundary is Rust 1.85+ edition 2024 on `std` targets with
threads, atomics, Tokio's multithread runtime, and Axum HTTP/1 or HTTP/2 router
support. The checked profile is an offline, in-memory router/task fixture on
the recorded host; it does not certify an operating system or architecture.
The unwind test profile contains panics as a redacted `INTERNAL` response. The
release-abort profile remains a separate process-failure outcome: destructors,
cancellation, and HTTP responses are not promised after abort.

Unsupported optional capabilities are runtime construction, implicit task
ownership, unbounded task/blocking queues, `LocalSet` and non-`Send` handlers,
hard termination of started blocking work, server streaming, client streaming,
bidirectional streaming, authenticated POST SSE, WebSocket, QUIC, compression,
TLS/certificate configuration, proxy trust, credential injection, automatic
retry/reconnect/replay, pagination orchestration, WASM/no-std/embedded
executors, mobile bindings, native-runtime certification, deployment
certification, and a production client transport.

From the repository root, the reproducible profile commands are:

```sh
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --no-default-features --locked --offline
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --features server --locked --offline
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --profile unwind --features axum --test axum_worker --locked --offline
cargo +1.85.0 build --manifest-path sdk/rust/Cargo.toml --release --features axum --locked --offline
cargo +stable clippy --manifest-path sdk/rust/Cargo.toml --all-targets --all-features --locked --offline -- -D warnings
node conformance/independent/verify-rust-tokio-axum.mjs
```

`conformance/v1/rust-tokio-axum.json` pins the exact #61 commit, crate
revisions/checksums, lockfile digest, evidence digests, feature matrix,
positive/negative/boundary/cancellation/resource-limit fixtures, and separate
unwind/abort outcomes. Passing it proves only that published profile.
