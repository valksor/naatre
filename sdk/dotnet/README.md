# .NET core SDK profile

`sdk.dotnet.core-1` is the generated wire-model and minimal reference-transport
profile owned by issue #41. It consumes the shared
`naatre.generator-model-1` input and emits nullable-enabled C# selected-result
types, explicit presence states, a persisted manifest, and canonical request
bytes. The broader package, ASP.NET dependency-injection, trimming/AOT, and
framework integration work remains owned by issue #85.

## Support policy

| Surface | Status | Evidence |
| --- | --- | --- |
| C# `net8.0` and `net10.0`, nullable enabled | supported | [`dotnet-sdk.json`](../../conformance/v1/dotnet-sdk.json) |
| F# consuming the generated C# shapes on both targets | supported | [`Naatre.FSharpSmoke`](Naatre.FSharpSmoke) |
| Unary `HttpClient` | supported reference path; active cancellation | [`Naatre.Conformance`](Naatre.Conformance) |
| Authenticated POST-SSE | supported reference path; active cancellation and required terminal frame | [`Naatre.Conformance`](Naatre.Conformance) |
| Custom `DelegatingHandler` hooks | supported | [`HttpClientTransport.cs`](Naatre.Core/HttpClientTransport.cs) |
| Optional WebSocket | unsupported; issue #85 | — |
| ASP.NET dependency injection | unsupported; issue #85 | — |
| Trimming, NativeAOT, mobile, browser, and framework certification | unsupported; issue #85 | — |

The reference transport owns its `HttpClientHandler`, disables automatic
redirects and decompression, bounds redirects, strips authentication on an
unapproved cross-origin redirect, and retries queries only. Injected test or
application handlers are trusted not to perform hidden redirects. A mutation
is never automatically replayed. Query pagination rejects repeated cursors.

`CancellationToken` actively aborts the current unary request or POST-SSE read.
Cancellation is reported as `NaatreOperationCanceledException`; disposal of an
async stream also disposes the response and source stream. EOF before a
`complete` or final `error` frame is `CLIENT_STREAM_TRUNCATED`.
`history-unavailable` ends a recovery attempt without being exposed as logical
completion. WebSocket cancellation is not claimed.

## Wire mappings

`Presence<T>` distinguishes missing, null, failed, skipped, pending, and
present. Missing optional variables are omitted with
`System.Text.Json`; explicit null is serialized as `null`. Open enums keep the
unknown raw spelling. Partial results retain data and structured errors, while
`RequireComplete` rejects incomplete required data.

`Int64Value`, `UInt64Value`, `BigIntegerValue`, `DecimalValue`,
`TimestampValue`, `DurationValue`, `UuidValue`, and `BytesValue` retain their
lossless canonical wire strings. Native `long`, `ulong`, `BigInteger`,
`decimal`, `DateTimeOffset`, `TimeSpan`, `Guid`, and byte-array adapters are
conveniences only. A timestamp with year zero or more than seven fractional
digits and a duration not divisible by 100 nanoseconds cannot be converted to
the narrower native type; the wire wrapper remains available.

## Reproducible verification

Install .NET SDK 8 and 10, then run from the repository root:

```sh
dotnet run --project sdk/dotnet/Naatre.Generator/Naatre.Generator.csproj --configuration Release -- conformance/v1/generator-model.json conformance/v1/generator-output.json sdk/dotnet/Naatre.Core/Generated
git diff --exit-code -- sdk/dotnet/Naatre.Core/Generated
dotnet build sdk/dotnet/Naatre.Core/Naatre.Core.csproj --configuration Release
dotnet build sdk/dotnet/Naatre.FSharpSmoke/Naatre.FSharpSmoke.fsproj --configuration Release
dotnet run --project sdk/dotnet/Naatre.Conformance/Naatre.Conformance.csproj --configuration Release
node conformance/independent/verify-dotnet-sdk.mjs
```
