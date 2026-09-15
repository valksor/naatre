# Local playground and schema mock server

Issue #55 and [`spec/v1/tooling.md`](../spec/v1/tooling.md) remain the sole
owners of the portable developer-tooling contract, schema model, mock scenarios,
diagnostic meanings, and redaction requirements. Issue #91 owns only the Go
`playground` package, its browser page, and the loopback process in
`cmd/naatre-playground`. Issue #94 continues to own LSP and editor packaging.
This slice does not define another request protocol or schema authority.

## Lifecycle and authorization boundary

The playground is disabled until an operator explicitly starts the binary with
an offline schema file. It binds an operating-system-assigned port on
`127.0.0.1`, prints that URL, keeps no persistent history, and stops on process
cancellation, `SIGINT`, or `SIGTERM`. Restarting it discards all browser and
server memory.

`playground.NewHandler` accepts an already authorized `schema.Document`. The
caller must apply the owning application's schema visibility policy before
construction. The handler cannot widen that view, select another revision, load
a schema, call a dynamic authorizer, execute a business handler, discover a
plugin, or send an upstream request. `/v1/schema` and `/v1/mock` can therefore
expose only that fixed view. Mock output binds its schema revision and digest,
canonical version, canonical document digest, seed, and scenario.

## Safe inspection and limits

`POST /v1/inspect` accepts only absolute HTTP or HTTPS targets and optionally a
constructor-pinned origin allowlist. It inspects; it never sends. URLs, headers,
and payloads pass through `tooling.RedactAndBound` before they cross a display or
copy boundary. Authorization values, cookies, URL user information, passwords,
API keys, access and refresh tokens, client secrets, and configured equivalent
names are redacted. Default limits are 1 MiB per HTTP request, 256 KiB per
operation document, 4 KiB per inspected field/payload, 1 MiB per response, 32
mock levels, 16 mock collection items, and 4 KiB per mock string.

Failures use `naatre.playground.problem-1` and the closed `PLAYGROUND_*` code set
published by `/v1/profile` and the conformance record. Titles are static. Parse
causes, request values, credentials, hidden schema names, filesystem paths, and
implementation errors are never returned.

## Supported runtime and unsupported capabilities

The implementation surface is Go 1.27 and the standard-library `net/http`
handler contract. The checked evidence is the offline Go test profile on
darwin/arm64. The process runner is loopback-only and uses an ephemeral port;
no other platform, native runtime, transport, framework, or certification claim
is made by this evidence.

The closed supported set is authorized schema browsing, bounded request
inspection, credential redaction, deterministic schema-only mocks, explicit
scenario selection, pagination and stream-frame fixtures, and copy of already
redacted SDK examples. Every optional capability outside that set is
unsupported. In particular, this slice does not provide alternate native
runtimes, business execution, dynamic authorization, live upstream requests,
network schema fetch, plugin execution, persistent history, remote listening,
TLS termination, unredacted persistence/export, WebSocket transport, or
transport/runtime certification. Regex/format, numeric, object-property,
cross-field-rule, unique-item, and constrained custom-scalar synthesis are
rejected rather than generating data that might violate those schema
constraints.

## Reproducible commands and evidence

Run the complete offline verification:

```sh
GOCACHE=/tmp/naatre-playground-gocache go test ./tooling ./playground ./cmd/naatre-playground
GOCACHE=/tmp/naatre-playground-gocache go test ./internal/conformance
GOCACHE=/tmp/naatre-playground-gocache go vet ./...
```

Start the opt-in runtime with an authorized offline schema view:

```sh
GOCACHE=/tmp/naatre-playground-gocache go run ./cmd/naatre-playground --schema schema.naatre.json
```

The machine-readable evidence is
[`conformance/tooling/playground-mock.json`](../conformance/tooling/playground-mock.json).
It pins the exact issue #55 implementation commit, tooling fixture digest,
module manifest digest, Go version, schema/canonical versions, runtime limits,
supported/unsupported capability sets, public failure codes, and positive,
negative, boundary, cancellation, and resource-limit cases.
