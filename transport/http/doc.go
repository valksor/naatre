// Package http provides transport codecs and narrowly scoped standard-library
// adapters. Its bounded SSE codec implements core.streaming-1 logical framing
// without establishing an HTTP endpoint. Its opt-in authenticated discovery
// handler implements schema.discovery.http-1 without enabling general operation
// execution. Issue #72 owns concrete streaming adapters; issue #73 owns the
// general net/http handler and integration harness.
// Package http provides transport codecs shared by the standard-library
// adapters. Its bounded SSE codec implements core.streaming-1 logical framing,
// and its digest codec implements the core.http.digest-1 RFC 9530 profile,
// without establishing an HTTP endpoint. Issue #72 owns the concrete streaming
// adapters; issue #73 owns the general net/http handler; issue #104 owns digest
// middleware and SDK integration.
// Package http provides the router-free standard-library adapter for unary
// core.http-1 execution and transport codecs shared by streaming adapters.
// The handler owns negotiation, carrier limits, cancellation, safe response
// mapping, cache-safe defaults, and deployment-configured CORS. Applications
// retain listener, TLS, authentication policy, and process-supervisor ownership.
package http
