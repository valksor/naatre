# JVM adapter ownership and lifecycle

Issue #40 and `spec/v1/jvm.md` remain the authority for Java/Kotlin wire
models, canonical requests, scalar precision, selected-result presence,
unknown variants, transport limits, and cancellation semantics. Issue #82
implements two consumer profiles without defining another wire schema:

- `sdk.jvm.java-http-1` is the server-JVM unary transport in the separate
  `io.naatre:naatre-jvm-http` artifact and `io.naatre.sdk.http` package.
- `sdk.jvm.android-compat-1` is the API-26 DEX compatibility boundary for
  `io.naatre:naatre-jvm-sdk`; it excludes every `java.net.http` class.

The application owns endpoint selection, credentials, `HttpClient` creation,
Android lifecycle observation, coroutine scopes, and shutdown order. A
`JavaHttpTransport` owns each response body and active exchange from `execute`
until completion or cancellation. Closing it rejects new calls and cancels all
active exchanges. An Android application performs the equivalent action on its
chosen `ClientCore.Transport`; cancelling a Kotlin coroutine or Flow already
propagates to that active call.

Public adapter failures expose only `INVALID_REQUEST`,
`AUTHENTICATION_FAILED`, `TRANSPORT`, `INVALID_RESPONSE`,
`RESPONSE_TOO_LARGE`, `CANCELLED`, or `UNSUPPORTED_CAPABILITY`. They retain no
transport exception cause, URL, header, credential, response body, or local
implementation path. Structured operation errors decoded from a successful
protocol response remain server-owned application data rather than adapter
diagnostics.

Both profiles use the exact generated Java/Kotlin artifacts from #40. The
adapter conformance probe reruns Java/Kotlin canonical-vector parity and tests
positive responses, malformed media, denied redirects, exact and excessive
resource boundaries, future/coroutine cancellation, null versus missing,
lossless integers and nanosecond timestamps, open unknown variants, and safe
failure surfaces. Reports and dependency revisions are pinned by
`conformance/v1/jvm-adapters.json`.

Unsupported optional capabilities are authenticated POST-SSE, WebSocket,
compression, automatic retry, authentication refresh, pagination
orchestration, Android framework lifecycle binding, an Android concrete HTTP
engine, device/emulator certification, native image, and Spring, Ktor, OkHttp,
Retrofit, or AndroidX integration. Gradle and Maven publication are also not
claimed by these offline profiles. #69 remains responsible for any combined
official language/runtime/transport certification.
