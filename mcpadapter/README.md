# MCP adapter core

`mcpadapter` implements the transport-independent `adapter.mcp-1` contract.
It validates an MCP `2025-11-25` catalog against trusted application
registrations, compiles consumed tools and resources into ordinary Naatre
runtime definitions, and emits canonical manifests and fidelity reports.

Descriptions, prompts, annotations, resource text, URIs, and result prose are
untrusted data. They never establish effects, authorization, idempotency,
retry, cache, cost, or transaction behavior. Those values come only from the
registered `runtime.Descriptor`. Discovered entries without an exact approved
registration are ignored, and required schema/capability mismatches reject the
whole adapter before an invoker can run.

The declared lossless subset uses closed Draft 2020-12 schemas and
`structuredContent` carrying a Naatre `complete`/`data`/`errors` envelope.
Open variants, references, implicit files, streams, and unknown constraints are
reported as unsupported and reject any operation that requires them.

Sessions bind progress tokens only while a request is active. `Session.Finalize`
atomically records the terminal outcome, releases that binding, and clears the
request-local cache and loader state; repeated finalization returns the same
bounded outcome without restoring released state.

Stdio and Streamable HTTP lifecycle claims are modeled separately. The core
package does not open a listener, spawn a process, or claim wire
compatibility. Concrete MCP client/server transports belong to issue 103; the
combined certification matrix belongs to issue 69.
