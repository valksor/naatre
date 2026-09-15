# Package, runtime, and profile guide

This guide is non-normative. The [v1 specification](../../spec/v1/README.md) owns behavior, the [profile registry](../../conformance/v1/profiles.json) owns profile vocabulary, and each linked component fixture owns its executable support boundary. The [documentation example evidence](../../conformance/v1/documentation-examples.json) pins those inputs to the dependency revision on which this guide was executed. A package checkout is a development snapshot, not a stable release or cross-platform certification.

## Choosing a surface

A **client** constructs requests and decodes responses but does not execute Naatre operations. A **remote worker** implements schema-generated handlers behind the Go gateway and the `worker.remote-1` boundary; that does not make its host language a native Naatre runtime. A **native runtime** validates, plans, authorizes, executes, completes, and serializes operations. The only native runtime demonstrated here is Go under `runtime.execution-1`. Every other native-runtime claim is not established.

| Language and package | Executed client example | Runtime and platform boundary | Remote worker | Native runtime |
| --- | --- | --- | --- | --- |
| Go, `github.com/valksor/naatre/sdk/go/generated` | [`sdk.go.operations-1`](../../conformance/v1/go-sdk.json): [request/result test](../../sdk/go/generated/operations_test.go), run `go test ./sdk/go/generated` | Go 1.27.1; CI executes on Linux arm64 and cross-compiles portable packages for Linux amd64, macOS amd64/arm64, and Windows amd64 | The [reference gateway](../../examples/v1/gateway/gateway_test.go) is a gateway, not a worker | [`runtime.execution-1`](../../conformance/v1/profiles.json) through the [server example](../../examples/v1/server/server_test.go), run `go test ./examples/v1/server ./examples/v1/gateway` |
| JavaScript/TypeScript, `@naatre/sdk` | [`sdk.typescript.core-1`](../../conformance/v1/typescript-sdk.json): [generator/runtime test](../../sdk/typescript/sdkgen/generator.test.mjs), run `npm --prefix sdk/typescript test && npm --prefix sdk/typescript run typecheck` | Node.js 24.21.0, TypeScript 7.0.2, plain ESM JavaScript; Linux arm64 CI | [`worker.javascript-typescript-1`](../../conformance/v1/typescript-worker.json): [stdio example](../../conformance/independent/remote-worker.mjs) | Not established |
| PHP, `naatre/sdk` | [`sdk.php.core-1`](../../conformance/v1/php-sdk.json): [request/result test](../../sdk/php/tests/run.php), run `composer --working-dir=sdk/php check` | PHP 8.3, 8.4, and 8.5; Linux arm64 CI | [`sdk.php.server-1`](../../conformance/v1/php-server.json): [stdio example](../../examples/v1/workers/php.php) | Not established |
| Python, `naatre-sdk` | [`sdk.python.core-1`](../../conformance/v1/python-sdk.json): [sync/async request test](../../sdk/python/tests/test_operation.py), run `python3 conformance/independent/verify-python-sdk.py` | CPython 3.11 through 3.14 package boundary; published example on Linux arm64 CI | [`worker.remote-1`](../../conformance/v1/remote-workers.json): [stdio example](../../examples/v1/workers/python.py) | Not established |
| Rust, `naatre-sdk` | [`sdk.rust.core-1`](../../conformance/v1/rust-sdk.json): [request/result tests](../../sdk/rust/tests/conformance.rs), run `./sdk/rust/ci.sh` | Rust 1.85.0 minimum and stable; published example on Linux arm64 CI | [`worker.remote-1.rust`](../../conformance/v1/rust-worker.json): [stdio example](../../sdk/rust/examples/remote_worker.rs) with the non-default `server` feature | Not established |
| JVM, `io.naatre:naatre-jvm-sdk` | [`sdk.jvm.core-1`](../../conformance/v1/jvm-sdk.json): [Java/Kotlin conformance](../../sdk/jvm/src/test/kotlin/io/naatre/sdk/JvmConformance.kt), run `node conformance/independent/verify-jvm-sdk.mjs` | JDK 17, 21, and 25; Kotlin 2.2.20; Linux arm64 CI | Not published | Not established |
| .NET, `Valksor.Naatre` | [`sdk.dotnet.core-1`](../../conformance/v1/dotnet-sdk.json): [C# conformance](../../sdk/dotnet/Naatre.Conformance/Program.cs), run `node conformance/independent/verify-dotnet-sdk.mjs` | .NET SDK 8.0.425 and 10.0.401, targeting net8.0 and net10.0; Linux arm64 CI | Not published | Not established |
| Swift, `NaatreSDK` | [`sdk.swift.core-1`](../../conformance/v1/swift-sdk.json): [client conformance](../../sdk/swift/Sources/NaatreConformance/main.swift), run `swift run --package-path sdk/swift naatre-swift-conformance` | Swift 6.0 minimum; core client execution on Linux arm64 CI; macOS 14+, iOS 17+, tvOS 17+, watchOS 10+, and visionOS 1+ remain package declarations with separately recorded Apple-adapter evidence | Not published | Not established |
| Dart, `naatre` | [`sdk.dart.core-1`](../../conformance/v1/dart-sdk.json): [operation client test](../../sdk/dart/test/operation_client_test.dart), run `node conformance/independent/verify-dart-sdk.mjs` | Dart 3.13.3 evidence within the SDK constraint >=3.10.0 <4.0.0; VM and browser-JavaScript sources; Linux arm64 CI | Models only where the component profile says so; no published remote-worker example here | Not established |
| Ruby, `naatre` | [`sdk.ruby.core-1`](../../conformance/v1/ruby-sdk.json): [generated request/result test](../../sdk/ruby/test/generated_test.rb), run `node conformance/independent/verify-ruby-sdk.mjs` | CRuby 2.6.10 darwin/arm64 report plus the Linux arm64 CI command; no broader runtime claim | Not published | Not established |

The client examples all consume the same [generator model](../../conformance/v1/generator-model.json) and exercise both request construction and response/result decoding. The remote-worker examples consume the [remote-worker fixture](../../conformance/v1/remote-workers.json). CI executes each command or its named package gate; `node conformance/independent/documentation.mjs` rejects a sample whose source, profile, command anchor, or pinned digest drifts.

## Unsupported optional capabilities

The lists below reproduce every entry in each client component fixture's `unsupported` array. They are profile-local: a capability unsupported in a core client may exist under a separately executed adapter profile, but this guide does not promote that evidence into the core profile.

### Go client

- `websocket-adapter`
- `native-eventsource-post-authentication`
- `mutation-replay`
- `subscription-replay`
- `bidirectional-streaming`
- `generated-union-convenience-accessors`
- `framework-bindings`
- `official-sdk-matrix-certification`

### JavaScript/TypeScript client

- `fetch-transport`
- `sse-transport`
- `websocket-transport`
- `decompression`
- `redirect-credential-policy`
- `runtime-matrix-certification`
- `framework-bindings`

### PHP client

- `active-abort-in-pure-psr18`
- `sse-transport`
- `websocket-transport`
- `symfony-adapter-in-core-profile`
- `laravel-adapter-in-core-profile`
- `framework-worker-bindings`

### Python client

- `pydantic-integration-is-separate-sdk.python.adapters-1-profile`
- `concrete-async-http-is-separate-sdk.python.adapters-1-profile`
- `asgi-server-handler-owned-by-59`
- `websocket-adapter`
- `official-sdk-matrix-certification-owned-by-69`

### Rust client

- `tokio-adapter-in-core-profile`
- `http-adapter`
- `sse-adapter`
- `websocket-adapter`
- `framework-server-bindings`
- `full-language-matrix-certification`

### JVM client

- `android-packaging-and-runtime-owned-by-82`
- `concrete-http-transport-owned-by-82`
- `sse-transport-owned-by-82`
- `websocket-transport-owned-by-82`
- `automatic-authentication-refresh`
- `automatic-retry-or-non-idempotent-replay`
- `complete-official-sdk-matrix-owned-by-69`

### .NET client

- `websocket-transport`
- `aspnet-dependency-injection`
- `automatic-auth-refresh`
- `automatic-reconnect-and-replay`
- `transparent-decompression`
- `trimming-and-nativeaot-certification`
- `mobile-and-browser-runtimes`
- `framework-package-certification`

### Swift client

- `urlsession-transport-owned-by-86`
- `sse-transport-owned-by-86`
- `websocket-transport-owned-by-86`
- `apple-lifecycle-adapter-owned-by-86`
- `automatic-authentication-refresh`
- `automatic-mutation-or-subscription-replay`
- `complete-official-sdk-matrix-owned-by-69`

### Dart client

- `automatic-authentication-refresh-and-token-persistence`
- `cookie-jar-and-browser-credentialed-cookie-mode`
- `caller-supplied-last-event-id-and-automatic-reconnect`
- `browser-websocket-authentication-headers`
- `websocket-subprotocol-and-compression-configuration`
- `portable-browser-websocket-backpressure-guarantee`
- `browser-pre-decompression-wire-byte-observation`
- `http-get-persisted-query-optimization`
- `proxy-custom-trust-store-and-certificate-pinning`
- `flutter-version-and-device-matrix-certification`
- `complete-official-sdk-matrix-owned-by-69`

### Ruby client

- `http-transport-in-core-profile`
- `rails-integration-in-core-profile`
- `sse-transport-in-core-profile`
- `websocket-transport`
- `automatic-auth-refresh`
- `automatic-retry`
- `transport-cancellation-in-core-profile`
- `complete-official-sdk-matrix-69`

## Worker boundaries

Run the JavaScript/TypeScript example with `node conformance/independent/remote-worker.mjs`, PHP with `php examples/v1/workers/php.php`, Python with `PYTHONDONTWRITEBYTECODE=1 python3 examples/v1/workers/python.py`, and Rust with `cargo build --manifest-path sdk/rust/Cargo.toml --features server --example remote_worker --locked --offline`. The shared worker profile does not establish an independent native runtime, process isolation, hard termination, a production gateway, or exactly-once effects. JavaScript/TypeScript additionally does not establish runtime-owned HTTP listeners, Node worker-thread lifecycle, Bun server lifecycle, Deno server lifecycle, edge deployment lifecycle, hard termination without an explicit worker/process executor, framework bindings, production HTTP/2 TLS transport, or deployment certification. PHP does not establish a native runtime, production framework adapters, forced rollback after every FPM disconnect, or automatic method exposure. Rust does not establish an independent native runtime, Tokio or Axum adapters, production HTTP transport, or panic-abort recovery. Python's example proves only the shared bounded stdio fixture; ASGI/framework lifecycle claims require their separately named profiles and commands.

## Ownership and lifecycle

Naatre maintainers own this guide and the `documentation.examples-1` evidence. Component owners retain ownership of their SDK and worker profiles. During the development-snapshot lifecycle, a support change must update its component fixture, this guide, its executable source, and its CI anchor together. Stable publication remains blocked until the release profile has complete machine-readable reports; passing these examples alone is not certification.
