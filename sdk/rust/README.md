# Rust SDK

`naatre-sdk` is the runtime-neutral Rust binding for `sdk.rust.core-1`. It
provides serde-compatible generated operation types, explicit missing/null and
selection states, lossless canonical scalar wrappers, persisted requests,
pagination values, and executor-neutral ownership handles.

The minimum supported Rust version is 1.85 with edition 2024. Features are
additive:

- default: client values, generated bindings, and transport ownership traits;
- `generator`: deterministic generation from `naatre.generator-model-1`;
- `tokio`: task-local unary and stream closure adapters for futures polled by a
  caller-selected Tokio runtime. This feature deliberately introduces no Tokio
  crate dependency, runtime construction, task spawning, or network backend.

The supported build matrix is:

```sh
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --no-default-features
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --features generator
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --features tokio
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --all-features
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

Unsupported optional capabilities are concrete HTTP and HTTP/2 clients,
authenticated POST SSE, WebSocket, QUIC, compression/decompression, TLS and
certificate policy, redirects, proxy handling, credential or tenant injection,
automatic retries, reconnect/replay, pagination orchestration, bounded task or
message queues, Tokio runtime construction, `tokio::spawn` ownership,
`LocalSet` and non-`Send` futures, Tower/Hyper/Reqwest/Axum integration, panic
containment guarantees for user handlers, WASM and embedded executors, mobile
bindings, native-runtime certification, and deployment certification. Server
registration, workers, Axum, unwind containment, and panic-abort behavior stay
owned by issues #61 and #100; the complete official SDK matrix stays owned by
issue #69.
