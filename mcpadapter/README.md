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

## Go runtime profile

Issue 103 owns `adapter.mcp-go-runtime-1`, the Go client/server integration in
this package. It consumes the exact manifest, schemas, policy, limits, and
mapping matrix produced by `adapter.mcp-1`; it is not another protocol or
schema authority. `NewServer` accepts only a compiled `naatre-to-mcp` manifest
and requires one exact application handler for every trusted tool or resource.
`Connect` performs MCP initialization and bounded discovery before compiling
any `mcp-to-naatre` runtime definition. Discovery, descriptions, annotations,
prompts, resource content, tool prose, endpoints, origins, and credentials
never add a handler or alter Naatre policy.

The server boundary is listener-neutral. `Connection.Handle` implements the
strict bounded JSON-RPC dispatch used by both transports. `ServeStdio` owns one
stdio connection, reserves its writer for protocol frames, cancels active work
on EOF, and never logs to it. `StreamableHTTPHandler` is an `http.Handler`; the
application owns TLS and listener lifecycle, supplies authentication, and
configures an exact HTTPS Origin allowlist. The handler generates opaque
session IDs, binds each session to one authenticated principal and tenant, and
caps concurrent sessions and pending requests. `StreamableHTTPClient` pins one
HTTPS endpoint, disables cookies and redirects, applies credentials through an
explicit hook, retains only the opaque MCP session ID, and never retries an
operation. All transport and handler causes remain process-local; public wire
failures contain only standard JSON-RPC text and a stable code.

`ValidateInstance` is the bounded runtime converter for the already-declared
closed JSON Schema subset. It preserves missing versus null, numeric limits,
closed object members, arrays, enumerations, unique `oneOf` matches, and trusted
extended scalar strings. `Client.CallTool` returns the canonical complete or
partial Naatre envelope without dropping safe error codes or paths. The
ordinary `interopadapter.Invoker` view accepts only complete object data;
callers needing partial data use `CallTool` explicitly.

The supported package/runtime boundary is the standard-library Go package on
Go 1.27 or newer. The executed profile is offline on Darwin arm64 and uses no
listener. Linux, Windows, other architectures, process supervisors, TLS
termination, and deployment environments are portable but unclaimed until
their own reports execute. Issue 64 retains the normative MCP contract; issue
69 retains the combined language, runtime, and transport certification matrix.

Unsupported optional capabilities are audio, image, embedded-resource and
streaming tool content; prompts and prompt execution; resource templates at
runtime; subscriptions; roots; sampling; elicitation; completion; tasks;
server-initiated requests; logging forwarding; automatic authentication
discovery or refresh; automatic operation retry; JSON-RPC batches; HTTP GET
SSE and DELETE session requests; legacy SSE; WebSocket; wire compression;
multi-server routing; implicit file access; and native framework, process,
platform, or deployment certification. `RuntimeEvidence.Unsupported` is the
machine-readable exhaustive list for this revision.

## Reproducible offline evidence

From the repository root with Go 1.27 and Node 24:

```sh
go test ./mcpadapter -count=1
go vet ./mcpadapter
node conformance/independent/mcp-runtime.mjs
```

`conformance/v1/mcp-runtime.json` pins the exact issue 64 commit, core fixture,
specification, module, implementation digests, limits, failure vocabulary,
transport boundaries, mapping matrix, and positive, negative, boundary,
cancellation, and resource-limit cases. The checks require no network or
listener.
