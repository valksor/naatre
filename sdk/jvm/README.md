# JVM Java and Kotlin SDK core

`sdk.jvm.core-1` is the portable Java 17 client core and deterministic Java and
Kotlin binding fixture for the shared Naatre generator model. Java declarations
carry class-retained type-use `@NonNull` and `@Nullable` annotations. Kotlin
declarations use explicit nullable/non-null types and do not author platform
types.

`Input` distinguishes omission, explicit null, and a supplied value. `Selected`
preserves missing, null, pending, present, failed, and skipped result states, so
partial data and structured errors remain observable together. Open enums keep
unknown raw values and `OpenVariant` keeps an unknown union discriminator and
payload.

Big integers, decimals, timestamps, durations, UUIDs, and bytes use canonical
string-backed wire values. Java `BigInteger`, `BigDecimal`, `Instant`,
`Duration`, `UUID`, and byte-array accessors are convenience adapters only and
must round-trip without precision or range loss. Formatting uses explicit UTC
and `Locale.ROOT`; process defaults cannot change wire bytes.

Regenerate and verify the bindings from the repository root:

```sh
GOCACHE=/tmp/naatre-jvm-go-cache go generate ./sdk/jvm
git diff --exit-code -- sdk/jvm/generated
go test ./sdk/jvm/sdkgen ./cmd/naatre-jvm-sdk-generator
KOTLIN_HOME=/path/to/kotlin-compiler-2.2.20 \
KOTLIN_COROUTINES_JAR=/path/to/kotlinx-coroutines-core-jvm-1.10.2.jar \
sdk/jvm/check.sh
```

The Gradle 9.1 build and Maven 3.9 POM consume the same source roots and publish
`io.naatre:naatre-jvm-sdk`. The checked source is compiled with Java 17 bytecode
and warnings as errors.

## Transport and cancellation boundary

Applications provide `ClientCore.Transport` calls. The core exposes:

- `CompletableFuture` and a blocking view that parks cleanly on virtual threads;
- Kotlin coroutine execution;
- Java `Flow.Publisher` and Kotlin `Flow` streaming;
- separately advertised SSE and optional WebSocket capability methods;
- explicit authentication interception, deadlines, response/frame limits, and
  cross-origin credential stripping.

Cancelling any future, coroutine, Java subscription, or Kotlin flow invokes the
underlying call cancellation hook. A stream succeeds only after an explicit
terminal result; EOF first is `STREAM_TRUNCATED` and releases the call. The core
does not refresh authentication or retry automatically, so it never silently
replays a non-idempotent write.

Concrete HTTP engines, Android packaging/desugaring, authenticated POST-SSE,
WebSocket, lifecycle integration, and device/emulator evidence are unsupported
until #82. Pagination and retry orchestration likewise require separately
advertised adapters.

## Compatibility matrix

| Java | Kotlin | Target | Status | Evidence |
| --- | --- | --- | --- | --- |
| Corretto 17.0.20.1 | 2.2.20 | Java 17 | passed | [macOS arm64 report](../../conformance/reports/jvm-sdk-jdk17-macos-arm64.json) |
| OpenJDK 21.0.12.1 | 2.2.20 | Java 17 | passed | [macOS arm64 report](../../conformance/reports/jvm-sdk-jdk21-macos-arm64.json) |
| Corretto 25.0.4.1 | 2.2.20 | Java 17 | passed | [macOS arm64 report](../../conformance/reports/jvm-sdk-jdk25-macos-arm64.json) |

| Surface | Policy | Status |
| --- | --- | --- |
| Gradle | 9.1 build metadata | not executed; execution report belongs to #82/#69 |
| Maven | 3.9 POM metadata | not executed; execution report belongs to #82/#69 |
| Android | no API/AGP/desugaring/device combination certified by core | unsupported until #82 |
| JDK 18-20, 22-24, 26+ | outside the LTS evidence matrix | unsupported by this profile |
| Concrete HTTP/SSE/WebSocket | explicit adapter required | unsupported until #82 |

No declared or extracted combination is marked passed without an executed
report. #69 owns the final combined language/runtime/transport matrix.
