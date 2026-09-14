# Naatre Dart SDK core

`naatre` is the dependency-free, null-safe Dart client and generated-model core
for `sdk.dart.core-1`. The package constraint is Dart `>=3.10.0 <4.0.0`; the
checked report certifies Dart 3.13.3 on VM, native AOT, and dart2js. A release
supports a Dart runtime only after that exact runtime has a checked conformance
report. Earlier admitted 3.x runtimes are source-compatible targets, not
silently certified combinations.

The core exports immutable operation and result values, explicit
missing/null/pending/present/failed/skipped states, open enum and union wrappers,
strict bounded JSON, persisted requests, bounded query-only retry, pagination,
authentication hooks, redirect credential policy, absolute deadlines,
cancellation tokens, isolate-aware background work, and transport-independent
SSE terminal validation. Generated result envelopes retain partial data and all
structured error members, including unknown fields.

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

## Runtime and transport matrix

| Runtime or transport | Status | Executed report |
| --- | --- | --- |
| Dart VM 3.13.3, Darwin arm64, core | Passed | [`conformance/reports/dart-core-3.13.3-darwin-arm64.json`](../../conformance/reports/dart-core-3.13.3-darwin-arm64.json) |
| Dart native AOT 3.13.3, Darwin arm64, canonical probe | Passed | Same report |
| dart2js 3.13.3 executed by Node 26.8.2, canonical probe | Passed | Same report |
| Flutter runtimes | Unsupported until #84 | No conformance report |
| Concrete HTTP adapter | Unsupported until #84 | `UnaryTransport` contract only |
| Concrete authenticated POST-SSE adapter | Unsupported until #84 | `SseTransport` contract and core decoder only |
| Optional WebSocket adapter | Unsupported until #84 | `WebSocketTransport` contract only |

`UnaryExchange.cancel` is invoked and awaited for unary cancellation or
deadline expiry. Stream cancellation cancels the upstream subscription and
invokes the idempotent `StreamConnection.close`; pause and resume propagate
backpressure to the source. A concrete adapter may claim active network-resource
closure only after #84 supplies matching runtime evidence. Unconfigured
capabilities fail with `CLIENT_CAPABILITY_UNSUPPORTED`. Authentication is one
explicit hook invocation per operation; retry is disabled by default and can
never replay a mutation or subscription.

The core package deliberately claims no Flutter version or concrete network
transport in issue #43. Flutter/VM/browser adapter tests, HTTP body and socket
closure, authenticated POST-SSE, optional WebSocket, and platform packaging are
owned by #84. The complete official SDK comparison remains owned by #69.
