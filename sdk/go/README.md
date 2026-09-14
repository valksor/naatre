# Go SDK operation bindings

The `generated` package is the checked-in `sdk.go.operations-1` binding for the
shared language-neutral generator model. It contains selected response shapes,
decoders that preserve missing, null, pending, and present fields, typed
operation values, known query/mutation/subscription kinds, and a persisted
operation manifest.

Regenerate the source and manifest byte-for-byte from the repository root:

```sh
go generate ./sdk/go/generated
git diff --exit-code -- sdk/go/generated
```

The package requires Go 1.27. It depends only on the public `client` and
`protocol` packages. Unary HTTP, authenticated POST SSE, query-only retries,
bounded batching, and forward pagination are implemented by `client`.

Unsupported capabilities are WebSocket transport, native EventSource POST
authentication, framework bindings, automatic mutation or subscription replay,
bidirectional streaming, generated union convenience accessors, and custom
scalar mappings other than lossless strings or raw JSON. Those capabilities
must not be inferred from generation of the wire-model package.
