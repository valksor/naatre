# Go gateway to Rust worker

This example is the complete unary, supervised-process path for
`worker.remote-1`. It deliberately uses the conformance-only framed-stdio
transport; production TLS/HTTP integration belongs to the separately versioned
adapter work in issues #88 and #100.

From the repository root:

```sh
cargo build --manifest-path sdk/rust/Cargo.toml --features server --example remote_worker --locked
go run ./examples/go-gateway-rust-worker ./sdk/rust/target/debug/examples/remote_worker
```

The Go process owns validation, authorization, retry, completion, and the Rust
child process. The Rust process validates the exact registration, constructs a
fresh principal/loader scope, dispatches only the explicitly registered typed
handler, validates output, and returns `{"greeting":"Hello, Ada"}` through the
Go gateway. Closing stdin shuts down the example worker. The stdio adapter
advertises cancellation as unsupported; the core cancellation and owned-work
lifecycle are exercised independently by `sdk/rust/tests/server.rs`.
