// Package http provides transport codecs and narrowly scoped standard-library
// adapters. Its bounded SSE codec implements core.streaming-1 logical framing
// without establishing an HTTP endpoint. Its opt-in authenticated discovery
// handler implements schema.discovery.http-1 without enabling general operation
// execution. Issue #72 owns concrete streaming adapters; issue #73 owns the
// general net/http handler and integration harness.
package http
