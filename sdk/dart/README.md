# Naatre Dart SDK

`naatre` is the dependency-free, null-safe Dart client and generated-model core
for `sdk.dart.core-1`. The package constraint is Dart `>=3.10.0 <4.0.0`; the
checked report certifies Dart 3.13.3 on VM, native AOT, and dart2js. A release
supports a Dart runtime only after that exact runtime has a checked conformance
report. Earlier admitted 3.x runtimes are source-compatible targets, not
silently certified combinations.

The package exports immutable operation and result values, explicit
missing/null/pending/present/failed/skipped states, open enum and union wrappers,
strict bounded JSON, persisted requests, bounded query-only retry, pagination,
authentication hooks, redirect credential policy, absolute deadlines,
cancellation tokens, isolate-aware background work, transport-independent SSE
and WebSocket frame validation, and concrete conditional HTTP, authenticated
POST-SSE, and optional WebSocket adapters. Generated result envelopes retain
partial data and all structured error members, including unknown fields.

Issue #43 owns the normative `sdk.dart.core-1` operation, value, scalar,
canonicalization, and client contract. Issue #84 owns only
`sdk.dart.adapters-1`: `HttpTransportAdapter`, `PostSseTransportAdapter`, the
optional `WebSocketTransportAdapter`, and their runtime bindings. These
adapters consume the canonical bytes produced by the core; they do not define
another protocol or schema. Issue #69 owns the complete official SDK matrix.
Applications may import `package:naatre/naatre.dart` for the complete SDK or
`package:naatre/adapters.dart` for the adapter and transport boundary only.

Large integers never enter JSON as Dart numbers. `Int64`, `UInt64`, and
`BigInt` use `ExtendedInteger`; `DecimalValue`, `TimestampValue`,
`DurationValue`, `UuidValue`, and `BytesValue` retain lossless wire spellings.
Direct JSON integers outside `-9007199254740991..9007199254740991` fail with
`CLIENT_VALUE_PRECISION` before encoding or after lexical response inspection,
so dart2js cannot silently round them.

## Generation and verification

The generator consumes only the shared `naatre.generator-model-1` and reference
output. It validates versions, schema and operation semantic digests, scalar
mappings, identifiers, and reference drift before emitting fixed artifact
names through same-directory temporary files.

From the repository root:

```sh
dart pub get -C sdk/dart --offline --enforce-lockfile
dart run sdk/dart/bin/naatre_sdkgen.dart \
  conformance/v1/generator-model.json \
  conformance/v1/generator-output.json sdk/dart/lib/src/generated
dart analyze sdk/dart --fatal-infos
node conformance/independent/verify-dart-sdk.mjs
```

The verifier checks evidence hashes, runs all dependency-free tests, regenerates
the package twice into clean directories, executes canonical and adapter vectors
on the VM, compiles and executes them as native AOT, and compiles them with
dart2js for execution by Node. `conformance/v1/dart-sdk.json` pins the exact
#43 dependency revision and every source and report digest; the reports under
`conformance/reports/` record the toolchain and executed target profile.

## Runtime and transport matrix

| Runtime or transport | Status | Executed report |
| --- | --- | --- |
| Dart VM 3.13.3, Darwin arm64 | Core and adapters passed | [`dart-core`](../../conformance/reports/dart-core-3.13.3-darwin-arm64.json), [`dart-adapters`](../../conformance/reports/dart-adapters-3.13.3-darwin-arm64.json) |
| Dart native AOT 3.13.3, Darwin arm64 | Core and adapter probes passed | Same reports |
| dart2js 3.13.3 executed by Node 26.8.2 | Core, web numeric, adapter, and conditional-import probes passed | Same reports |
| Flutter native mobile and desktop | Source-supported through the `dart:io` binding | No Flutter-version certification in this checkout |
| Flutter web | Source-supported through the browser binding | No Flutter-version certification in this checkout |
| HTTP POST | Supported on VM/AOT, Flutter native, browser, and Flutter web | `HttpTransportAdapter` |
| Authenticated POST-SSE | Supported on VM/AOT, Flutter native, browser, and Flutter web | `PostSseTransportAdapter` |
| Optional WebSocket | Supported on VM/AOT and Flutter native; browser variants have the limitation below | `WebSocketTransportAdapter` |

`dart.library.io` selects the VM/Flutter-native implementation and
`dart.library.html` selects the browser/Flutter-web implementation. Unsupported
Dart targets select a fail-closed stub. Adapter instances are reusable, but all
request headers, redirect state, byte counters, cancellation handles, and frame
state are operation-local. `UnaryExchange.cancel` aborts the active request;
stream cancellation cancels the upstream subscription and idempotently closes
the response or socket. Authentication is invoked exactly once per operation.
Cross-origin redirects remove authorization, cookies, CSRF tokens, tenant and
principal metadata, and proxy authorization. Public failures contain only a
stable `CLIENT_*` code and retryability; authentication exceptions and
transport implementation errors are not retained or rendered.

## Lifecycle and unsupported optional capabilities

The adapter API follows the package's `0.x` lifecycle. Callers own a client and
may reuse its adapters; cancellation, deadline expiry, terminal stream frames,
decode failures, and response limits release the active network resource. The
package does not retain credentials or per-operation state between calls.

The following optional capabilities are explicitly unsupported by this slice:

- automatic authentication refresh, token persistence, cookie-jar management,
  and browser credentialed-cookie mode;
- caller-supplied `Last-Event-ID` resume and automatic SSE/WebSocket reconnect;
- browser WebSocket authentication headers (the browser API cannot set them),
  WebSocket subprotocol negotiation, per-message compression configuration, and
  a portable browser WebSocket backpressure guarantee;
- browser-side observation of compressed wire bytes before the user agent
  decompresses a response; decompressed response and frame limits still apply;
- HTTP GET persisted-query optimization, proxy configuration, custom trust
  stores, and certificate pinning;
- Flutter-version certification, iOS/Android/macOS/Windows/Linux device-matrix
  certification, and the complete official SDK comparison owned by #69.

Unsupported configured capabilities fail closed with a stable
`CLIENT_CAPABILITY_UNSUPPORTED:*` or `CLIENT_CONFIG_INVALID` code. No support is
claimed for a Flutter or native-runtime combination without an executed report.
