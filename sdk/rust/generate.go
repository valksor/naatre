// Package rustsdk owns reproducible Rust SDK generation hooks.
package rustsdk

//go:generate cargo run --quiet --locked --features generator --bin naatre-rust-sdk-generator -- ../../conformance/v1/generator-model.json ../../conformance/v1/generator-output.json generated
