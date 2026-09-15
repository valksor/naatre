# Interoperability adapter core

`interopadapter` is the Go reference path for `core.adapters-1`. It compiles a
bounded list of explicitly approved imported operations into ordinary runtime
definitions and returns a direction-specific fidelity report whether startup
is accepted or rejected.

The application supplies each operation's complete `runtime.Descriptor`,
including effect, idempotency, retry, cache, cost, batching, transaction, and
authorization policy. The compiler never infers those claims from an HTTP
method, GraphQL label, RPC name, protobuf option, or vendor extension.

```go
invoker, err := interopadapter.NewHTTPJSONInvoker(interopadapter.HTTPJSONConfig{
    Endpoint:         "https://profiles.internal/v1/lookup",
    AllowedOrigins:   []string{"https://profiles.internal"},
    ForwardHeaders:   []string{"Authorization"},
    MaxRequestBytes:  64 << 10,
    MaxResponseBytes: 1 << 20,
})
// Check err, then pass invoker in one explicitly approved Operation to Compile.
// Publish the returned FidelityReport before registering Compiled.
```

The HTTP-JSON envelope is the fixture's small existing-service example, not an
OpenAPI implementation. Requests contain the approved upstream operation,
schema-preserving input JSON, and a sorted field projection. The invoker uses
the caller context for cancellation and deadlines, rejects redirects, bounds
both directions, accepts strict JSON responses, forwards only exact configured
headers, and exposes stable errors without upstream bodies or endpoints.

Full GraphQL, OpenAPI, and OpenRPC/JSON-RPC integrations belong to #92, #93,
and #98 respectively. The opt-in Go protobuf/gRPC/Connect client profile owned
by #95 is implemented in [`transport/protobufrpc`](../transport/protobufrpc/README.md), with narrower
runtime and shape boundaries than this protocol-neutral contract. Multi-SDK
execution belongs to #69.
