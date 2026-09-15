# Go client core

Package `client` is the runtime-independent Go SDK core for
`sdk.go.client-1`. It accepts immutable `protocol.Document` values, constructs
canonical inline or persisted request envelopes, preserves exact JSON scalar
spellings, and executes bounded unary HTTP requests.

The package requires Go 1.27, matching the module and v1 release toolchain. It
imports `protocol` but never `runtime`, server registries, handlers, execution
plans, or reflection adapters. A configured `http.Client` is cloned; its
transport remains caller-owned and its redirect hook is composed with the
mandatory five-redirect and cross-origin credential boundary.

Supported in this core profile:

- inline and persisted unary POST requests;
- exact canonical variables and semantic document hashes;
- context cancellation and deadlines;
- custom `http.RoundTripper` implementations and authentication hooks;
- bounded identity and gzip responses;
- strict Naatre envelopes, RFC 9457 problems, and simultaneous partial data
  plus structured errors;
- stable safe client error codes and resource closure.

Opt-in `Config.HTTPDigest` adds the `implementation.go.http-digest-1`
completion gate: request digest generation, response `Content-Digest` and
`Repr-Digest` verification before parsing or success, downgrade and trailer
rejection, and explicit complete-representation sourcing for ranges. Its exact
lifecycle, runtime evidence, and unsupported capabilities are published in
[`docs/http-digest-integration.md`](../docs/http-digest-integration.md).

The same package also provides the higher-level primitives exercised by the
separate `sdk.go.operations-1` profile: immutable typed language builders,
selected-result decoding, strict persisted manifests, query-only finite retry,
bounded ordered batching, bounded forward pagination, generic actively-closing
streams, and authenticated POST SSE. Deterministic generated bindings and their
manifest live in `sdk/go/generated` and can be reproduced with
`go generate ./sdk/go/generated`.

WebSocket adapters, native EventSource POST authentication, mutation or
subscription replay, bidirectional streaming, framework bindings, and the
complete official SDK matrix remain unsupported. The core
`sdk.go.client-1` fixture continues to list higher-level behavior as unsupported
within that narrower profile; support is claimed only by
`sdk.go.operations-1`.
