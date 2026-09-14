// Package http provides the router-free standard-library adapter for unary
// core.http-1 execution and transport codecs shared by streaming adapters.
// The handler owns negotiation, carrier limits, cancellation, safe response
// mapping, cache-safe defaults, and deployment-configured CORS. Applications
// retain listener, TLS, authentication policy, and process-supervisor ownership.
package http
