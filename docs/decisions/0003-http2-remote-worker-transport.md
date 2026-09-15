# Decision 0003: HTTP/2 remote-worker transport

Status: accepted for v1.

Naatre uses a versioned, length-delimited JSON protocol over TLS-authenticated
HTTP/2 as the initial production remote-worker transport. HTTP unary/streaming
was selected over framed local IPC because it crosses container and host
boundaries, supplies standard TLS workload identity and audience enforcement,
has per-stream half-close and flow control, and supports independent rolling
upgrades. It was selected over adopting a language runtime ABI because the wire
contract must remain implementable without Go, cgo, or in-process FFI.

Framed local IPC has lower setup cost and is useful for deterministic process
fixtures, but local peer identity, lifecycle, multiplexing, remote placement,
and cross-host operation would all need a second production contract. The
conformance runner therefore supports the same frames over stdio strictly as a
test/process profile. It is not a production-support claim.

The envelope is Connect-inspired but does not claim Connect wire
compatibility. Unary, client-streaming, server-streaming, and bidirectional
capabilities negotiate independently. HTTP/1.1 is unary-only and opt-in.
Issue #88 delivered the single-endpoint Go HTTP/2 transport; a production
multi-endpoint connection manager (pooling) is post-v1 hardening tracked on the
roadmap (#32). This decision and `spec/v1/remote-workers.md` own the portable
contract.
