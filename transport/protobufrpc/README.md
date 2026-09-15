# Protobuf, gRPC, and Connect adapter profile

`protobufrpc` is the opt-in Go client implementation of
`core.adapters.protobuf-grpc-connect-1`. It consumes the protocol-neutral
`core.adapters-1` contract; protobuf method descriptors are the only wire-shape
authority, and applications still supply every Naatre operation descriptor and
policy through `interopadapter.Compile`.

The package is experimental until the repository's complete integration matrix
runs. Its public lifecycle is additive within v1: stable error codes and the
profile identifier are compatibility surfaces, while capability additions
require new conformance evidence. It supports Go 1.27 on the Go toolchain's
Darwin, Linux, and Windows targets, with these exact dependencies:

- `google.golang.org/protobuf@v1.36.11`
- `google.golang.org/grpc@v1.82.1`
- Connect protocol revision
  `connectrpc/connect@fac060371d74da4205f28ef504d078d2d2ce286f`
- the `core.adapters-1` implementation at
  `a93f33dffa5b351c04b4f229df91d098ed4df1b4`

`NewGRPC` accepts a generated or ordinary `grpc.ClientConnInterface`.
`NewConnect` uses an application-supplied `http.Client` and an exact HTTPS RPC
URL. Both adapters implement `interopadapter.Invoker` for unary methods.
`OpenStream` supports server-streaming methods. The caller context owns
cancellation and deadlines for the whole RPC or stream; every stream must also
be closed by its owner.

## Fidelity boundary

At construction, `Report` publishes deterministic field mappings and rejects
any shape that cannot preserve the profile. Request JSON is decoded by
protobuf's descriptor-aware strict decoder. This retains explicit presence,
oneof membership, integer ranges, and enum spellings. Responses use protobuf
JSON with proto field names and extended integers as strings. Unknown fields,
out-of-range numbers, conflicting oneof members, maps, bytes, implicit-presence
request scalars, unsupported well-known types, and non-finite floating-point
values are rejected with stable `PROTO_RPC_*` codes.

A configured field-mask target must be one singular
`google.protobuf.FieldMask` request field. The adapter replaces its paths with
the sorted, duplicate-free projection produced by `interopadapter.Compile`.
No other protobuf option or annotation changes Naatre behavior.

Metadata forwarding is a lowercase, exact text-key allowlist. Binary,
hop-by-hop, cookie, proxy credential, pseudo-header, and `grpc-*` keys are
rejected. Providers return lowercase keys. Values are bounded and CR/LF/NUL is
rejected. `authorization` may be forwarded only when explicitly allowlisted.
Connect redirects are always rejected and the copied HTTP client has no cookie
jar. Errors expose only a stable adapter code and canonical RPC code; upstream
messages, status details, credentials, metadata values, endpoints, and wrapped
implementation errors are never public.

Unary gRPC and Connect/protobuf POST plus server-streaming gRPC and framed
Connect/protobuf POST are supported. Each message, the aggregate stream,
metadata, and request/response bytes have required finite limits.

## Unsupported optional capabilities

This profile does not implement or claim:

- schema or operation-policy authority, schema export, reflection, or remote
  discovery;
- protobuf maps, protobuf bytes/base64 adaptation, implicit-presence request
  scalars, non-finite floats, unknown fields, or `Any`, `Struct`, `Value`,
  `ListValue`, and wrapper well-known types;
- client streaming, bidirectional streaming, or gRPC/Connect server exposure;
- gRPC-Web, Connect GET, the Connect JSON codec, compression, or binary
  metadata;
- response header/trailer forwarding or protobuf status details;
- credential construction, automatic retry, load balancing, framework
  bindings, or browser/native-mobile certification.

Use another explicit profile rather than assuming any of these capabilities.

## Reproduce the evidence

The machine-readable evidence is
`conformance/v1/protobuf-grpc-connect.json`. It records positive, negative,
boundary, cancellation, security, and resource-limit cases with the exact
dependency revisions above. The tests use in-memory `grpc.ClientConnInterface`
and `http.RoundTripper` implementations and never bind a listener.

```sh
go test ./transport/protobufrpc ./internal/conformance -run 'Test(GRPC|Connect|Configuration|Request|ProtobufRPC)'
go test ./transport/protobufrpc
go test -run '^$' ./...
go vet ./...
golangci-lint run
go generate ./...
git diff --exit-code
```

Networked interoperability against external gRPC and Connect servers, TLS
negotiation, and cross-language wire verification are intentionally deferred
to the orchestrator's integration environment; they are not implied by this
offline package evidence.
