# Rust SDK core

`naatre-sdk` is the runtime-neutral Rust binding for `sdk.rust.core-1`. It
provides serde-compatible generated operation types, explicit missing/null and
selection states, lossless canonical scalar wrappers, persisted requests,
pagination values, and executor-neutral ownership handles.

The minimum supported Rust version is 1.85 with edition 2024. Features are
additive:

- default: client values, generated bindings, and transport ownership traits;
- `generator`: deterministic generation from `naatre.generator-model-1`.

The supported build matrix is:

```sh
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --no-default-features
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --all-features
cargo +stable fmt --manifest-path sdk/rust/Cargo.toml --check
cargo +stable clippy --manifest-path sdk/rust/Cargo.toml --all-targets --all-features -- -D warnings
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

`Transport` and `StreamTransport` are `Send + Sync`; returned futures and
streams are `Send`. `Client` owns its transport, and executable client
transports must be `'static` so a dropped request cannot retain a borrowed
transport behind executor-owned work. Operation variables are serialized
before transport work starts, while decoded result ownership is returned to
the caller.

HTTP, authenticated POST SSE, WebSocket, decompression, authentication hooks,
Tokio adapters, framework bindings, and runtime/platform certification are
unsupported by this profile and owned by issue #80. Server registration,
workers, Axum, unwind containment, and panic-abort behavior are owned by issues
#61 and #100.
