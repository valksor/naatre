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

Higher-level typed selection builders, generated selected-result types,
persisted manifests, pagination and batching helpers, retries, SSE, and
optional WebSocket adapters are not claimed by this core package. They are
owned and tested by issue #71. Framework integrations and the complete official
SDK matrix remain owned by their dedicated roadmap issues.
