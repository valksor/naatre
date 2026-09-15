# .NET core SDK profile

`sdk.dotnet.core-1` is the generated wire-model and minimal reference-transport
profile owned by issue #41. It consumes the shared
`naatre.generator-model-1` input and emits nullable-enabled C# selected-result
types, explicit presence states, a persisted manifest, and canonical request
bytes. Issue #85 owns the separately versioned `sdk.dotnet.adapters-1`
integration profile layered on this core. It adds the
`Valksor.Naatre.Client` C# facade, the `Valksor.Naatre.FSharp` presence and
`Async` helpers, and the optional `Valksor.Naatre.AspNetCore`
dependency-injection package without creating a second wire codec or schema
authority.

## Support policy

| Surface | Status | Evidence |
| --- | --- | --- |
| C# `net8.0` and `net10.0`, nullable enabled | supported | [`dotnet-sdk.json`](../../conformance/v1/dotnet-sdk.json) |
| F# consuming the generated C# shapes on both targets | supported | [`Naatre.FSharpSmoke`](Naatre.FSharpSmoke) |
| Unary `HttpClient` | supported reference path; active cancellation | [`Naatre.Conformance`](Naatre.Conformance) |
| Authenticated POST-SSE | supported reference path; active cancellation and required terminal frame | [`Naatre.Conformance`](Naatre.Conformance) |
| Custom `DelegatingHandler` hooks | supported | [`HttpClientTransport.cs`](Naatre.Core/HttpClientTransport.cs) |
| C# adapter facade on `net8.0` and `net10.0` | supported | [`Naatre.Client`](Naatre.Client) |
| F# discriminated presence states and `Async` execution | supported | [`Naatre.FSharp`](Naatre.FSharp) |
| ASP.NET `IServiceCollection` and `IHttpClientFactory` integration | supported, optional | [`Naatre.AspNetCore`](Naatre.AspNetCore) |
| Trimming and NativeAOT | not claimed | [`dotnet-adapters.json`](../../conformance/v1/dotnet-adapters.json) |
| WebSocket, mobile, browser, and other framework certification | unsupported | [`dotnet-adapters.json`](../../conformance/v1/dotnet-adapters.json) |

## Adapter ownership and lifecycle

The adapter profile is owned by issue #85, follows fixture suite `1.0.0`, and
depends on the exact issue #41 core revision and fixture digest recorded in
`conformance/v1/dotnet-adapters.json`. Issue #41 remains the authority for
wire models, canonical JSON, scalar mappings, result decoding, and HTTP/SSE
protocol behavior. The adapter packages delegate to those canonical types;
they do not copy or redefine them.

`NaatreClient` is safe for concurrent operations and must be disposed. Its
default constructor owns and disposes the complete `DelegatingHandler`
pipeline. The constructor that accepts an `HttpMessageHandler` borrows that
handler and never disposes it. The ASP.NET helper resolves a transient
`INaatreClient` over the named `IHttpMessageHandlerFactory` pipeline; the
factory retains handler ownership and the dependency-injection container
disposes each resolved client facade. Adapter failures expose only a stable
`CLIENT_*` code through `NaatreAdapterException` or
`NaatreAdapterCanceledException`; dependency exceptions, authentication
callback text, credentials, response metadata, and parser details are not
retained as inner exceptions or public messages.

The supported runtime boundary is managed .NET 8 and .NET 10 on platforms
supported by those runtimes, with nullable C# 12 and the SDK-shipped F#
compiler. The executed profile covers unary HTTP, POST-SSE, cancellation,
resource bounds, handler ownership, and Microsoft.Extensions dependency
injection. It does not certify operating systems, deployments, or runtime
revisions beyond the toolchain matrix in the fixture.

F# callers use the same `NaatreOperation<'T>`, `CanonicalJson`, generated
models, and lossless scalar wrappers as C#. `Valksor.Naatre.FSharp.Presence`
is a six-case discriminated union and its conversions preserve missing, null,
failed, skipped, pending, and present independently. `Presence.ofOption`
maps `None`, `Some null`, and `Some value` to distinct states. No conversion
back to `option` is provided because it would collapse missing and null.

Register the optional ASP.NET integration and configure its named
`HttpClient` pipeline as usual:

```csharp
services
    .AddNaatreClient(new Uri("https://api.example.test/v1"))
    .ConfigurePrimaryHttpMessageHandler(() => new SocketsHttpHandler());
```

The helper registers `INaatreClient`; it does not add controllers, minimal API
bindings, server transports, authentication refresh, telemetry, or durable
cursor storage.

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

Verify the issue #85 adapter profile after an offline restore has populated
the exact .NET 8 and .NET 10 reference packs recorded by the fixture:

```sh
dotnet build sdk/dotnet/Naatre.Client/Naatre.Client.csproj --configuration Release --no-restore --disable-build-servers -m:1
dotnet build sdk/dotnet/Naatre.FSharp/Naatre.FSharp.fsproj --configuration Release --no-restore --disable-build-servers -m:1
dotnet build sdk/dotnet/Naatre.AspNetCore/Naatre.AspNetCore.csproj --configuration Release --no-restore --disable-build-servers -m:1
dotnet build sdk/dotnet/Naatre.AdapterConformance/Naatre.AdapterConformance.csproj --configuration Release --no-restore --disable-build-servers -m:1
dotnet build sdk/dotnet/Naatre.FSharpConformance/Naatre.FSharpConformance.fsproj --configuration Release --no-restore --disable-build-servers -m:1
dotnet pack sdk/dotnet/Naatre.Client/Naatre.Client.csproj --configuration Release --no-build --no-restore
dotnet pack sdk/dotnet/Naatre.FSharp/Naatre.FSharp.fsproj --configuration Release --no-build --no-restore
dotnet pack sdk/dotnet/Naatre.AspNetCore/Naatre.AspNetCore.csproj --configuration Release --no-build --no-restore
node conformance/independent/verify-dotnet-adapters.mjs --metadata-only
node conformance/independent/verify-dotnet-adapters.mjs
```

Every adapter fixture uses in-memory `HttpMessageHandler` doubles; verification
does not bind a listener or require network access.
The metadata-only command checks every evidence digest and resolves the pinned
issue #41 fixture from its exact Git commit without claiming a runtime pass.

The complete unsupported boundary is WebSocket transport, automatic
authentication refresh, automatic stream reconnect/replay, transparent
decompression, trimming and NativeAOT certification, browser/WebAssembly,
mobile/.NET MAUI, Xamarin, Unity, .NET Framework, ASP.NET MVC or minimal API
model binding, ASP.NET server transport, frameworks beyond
Microsoft.Extensions.Http and DependencyInjection, OpenTelemetry
instrumentation, durable cursor storage, deployment certification, and
runtime revisions beyond the recorded toolchain.
