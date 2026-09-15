# JVM Java, Kotlin, Android, and HTTP SDK

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
the Android-neutral `io.naatre:naatre-jvm-sdk` core. The separately packaged
`io.naatre:naatre-jvm-http` artifact contains the server-only
`io.naatre.sdk.http.JavaHttpTransport`; it is never part of the Android DEX
profile. Both artifacts use Java 17 bytecode and warnings as errors.

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

`JavaHttpTransport` implements bounded unary POST with `java.net.http.HttpClient`.
The client must have automatic redirects disabled. The adapter requests identity
encoding, accepts only JSON responses, follows only 307/308 redirects, permits
cross-origin redirects only through an exact origin allowlist, strips protected
headers unless the destination is separately credential-allowlisted, and closes
every redirect or response body. Cancelling a call or closing the transport
cancels active `HttpClient` work. It never exposes an endpoint, header, response,
credential, exception cause, or local implementation detail through its public
failure: callers receive only the stable `ClientCore.ErrorCode` and its name.
The adapter surface uses `INVALID_REQUEST`, `AUTHENTICATION_FAILED`,
`TRANSPORT`, `INVALID_RESPONSE`, `RESPONSE_TOO_LARGE`, `CANCELLED`, and
`UNSUPPORTED_CAPABILITY`; operation error codes remain server-owned data.

The `sdk.jvm.android-compat-1` profile compiles the core Java and Kotlin sources,
the generated bindings, and an executable compatibility probe, then converts
that exact artifact with D8 for minimum API 26. This is deliberately separate
from server-JVM evidence. Applications own their Android lifecycle binding:
cancel the coroutine/Flow for screen-scoped work and cancel or close the
application-supplied transport when its lifecycle owner stops. The profile does
not add an Android framework dependency to generated or core APIs.

## Reproducible adapter verification

The adapter profile depends on issue #40 revision
`f2455310695238e11b81e36d121243191a7aee1c`, Kotlin 2.2.20,
kotlinx-coroutines 1.10.2, compile SDK 36 revision 2, and Android build tools
36.0.0. Exact source and dependency hashes are recorded in
`conformance/v1/jvm-adapters.json`.

From the repository root:

```sh
export JAVA_HOME=/path/to/supported-jdk-21
export PATH="$JAVA_HOME/bin:$PATH"
export KOTLIN_HOME=/path/to/kotlin-compiler-2.2.20/kotlinc
export KOTLIN_COROUTINES_JAR=/path/to/kotlinx-coroutines-core-jvm-1.10.2.jar
export ANDROID_SDK_ROOT=/path/to/android-sdk
sdk/jvm/check-adapters.sh
node conformance/independent/verify-jvm-adapters.mjs
```

`check-server-adapter.sh` and `check-android.sh` may be run independently. The
Android script defaults to compile SDK 36, build tools 36.0.0, and minimum API
26; changing any `NAATRE_ANDROID_*` override executes a different, uncertified
combination.

## Compatibility matrix

| Java | Kotlin | Target | Status | Evidence |
| --- | --- | --- | --- | --- |
| Corretto 17.0.20.1 | 2.2.20 | Java 17 | passed | [macOS arm64 report](../../conformance/reports/jvm-sdk-jdk17-macos-arm64.json) |
| OpenJDK 21.0.12.1 | 2.2.20 | Java 17 | passed | [macOS arm64 report](../../conformance/reports/jvm-sdk-jdk21-macos-arm64.json) |
| Corretto 25.0.4.1 | 2.2.20 | Java 17 | passed | [macOS arm64 report](../../conformance/reports/jvm-sdk-jdk25-macos-arm64.json) |

| Adapter/profile | Supported surface | Status | Evidence |
| --- | --- | --- | --- |
| `sdk.jvm.java-http-1` on OpenJDK 21.0.12.1 | server JVM unary HTTP, manual redirects, active cancellation | passed | [server adapter report](../../conformance/reports/jvm-adapters-jdk21-macos-arm64.json) |
| `sdk.jvm.android-compat-1` | API 26 DEX compatibility for core Java/Kotlin and generated models | passed | [Android compatibility report](../../conformance/reports/jvm-android-api26-compile36.json) |
| Android device/emulator runtime | no device/API/ABI combination executed | not executed | compatibility report records the boundary |

| Surface | Policy | Status |
| --- | --- | --- |
| Gradle | 9.1 build metadata | not executed; execution report belongs to #82/#69 |
| Maven | 3.9 POM metadata | not executed; execution report belongs to #82/#69 |
| Android `java.net.http` | server-only package; excluded from Android artifact | unsupported |
| Android concrete HTTP engine and automatic lifecycle binding | application-supplied core transport only | unsupported |
| JDK 18-20, 22-24, 26+ | outside the LTS evidence matrix | unsupported by this profile |
| Authenticated POST-SSE and WebSocket | no concrete JVM adapter in this profile | unsupported |
| Compression | identity content encoding only | unsupported |
| Automatic authentication refresh or retry | never implicitly replays an operation | unsupported |
| Pagination orchestration | generated cursor values only | unsupported |
| Spring, Ktor, OkHttp, Retrofit, AndroidX, and native-image integration | no adapter or framework evidence | unsupported |
| Gradle/Maven publication and reproducibility | metadata only; no repository publication | not executed |

No declared or extracted combination is marked passed without an executed
report. #69 owns the final combined language/runtime/transport matrix.
