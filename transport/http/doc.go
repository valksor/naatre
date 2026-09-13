// Package http provides transport codecs shared by the standard-library
// adapters. Its bounded SSE codec implements core.streaming-1 logical framing
// without establishing an HTTP endpoint. Issue #72 owns the concrete streaming
// adapters; issue #73 owns the general net/http handler and integration harness.
package http
