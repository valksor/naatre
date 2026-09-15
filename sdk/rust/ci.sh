#!/usr/bin/env bash
set -euo pipefail

rustup toolchain install 1.85.0 --profile minimal
rustup toolchain install stable --profile minimal --component rustfmt --component clippy
cargo +stable fmt --manifest-path sdk/rust/Cargo.toml --check
cargo +stable clippy --manifest-path sdk/rust/Cargo.toml --all-targets --all-features --locked -- -D warnings
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --no-default-features --locked
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --locked
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --features generator --locked
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --profile unwind --features server --test server --locked
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --features tokio --locked
cargo +1.85.0 check --manifest-path sdk/rust/Cargo.toml --features tokio-runtime --locked
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --profile unwind --features axum --test axum_worker --locked
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --locked
cargo +1.85.0 test --manifest-path sdk/rust/Cargo.toml --all-features --locked
cargo +1.85.0 build --manifest-path sdk/rust/Cargo.toml --release --features server --example remote_worker --locked
node conformance/independent/verify-rust-worker.mjs
node conformance/independent/verify-rust-tokio-axum.mjs
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"rust-sdk","command":"run","path":{"source":{"kind":"sdk","language":"rust"},"destination":{"kind":"native-runtime","language":"rust"}},"profiles":["sdk.rust.core-1","sdk.rust.adapters-1","worker.remote-1.rust-tokio-axum-1"]}' | node conformance/independent/runner.mjs --require-pass
