// Package otel exports Naatre's dependency-free runtime telemetry hooks to
// application-supplied OpenTelemetry providers.
//
// High-cardinality references are represented by deterministic hashes, span
// and instrument names are fixed, metric attributes use only the portable
// bounded vocabulary, and retained causal state has explicit limits. The
// application owns provider configuration, exporter lifecycle, propagation,
// resources, sampling, flushing, and shutdown.
package otel
