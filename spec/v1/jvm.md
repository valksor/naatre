# JVM Java and Kotlin client core

The `sdk.jvm.core-1` profile defines the portable generated model and client
boundary shared by Java services and Kotlin/JVM consumers. It consumes
`naatre.generator-model-1` and the canonical reference output from
`sdk.generation-1`. Concrete HTTP engines, Android compatibility, SSE and
WebSocket adapters are separate capabilities owned by issue #82 and advertised
by their own profiles; the combined official language matrix is owned by issue
#69.

## Generated bindings and wire values

- **JVM-001:** One deterministic generator invocation MUST emit both the Java
  and Kotlin declarations. Both declarations MUST use the same selected
  operation shape, canonical request encoder, persisted reference, and
  generator version. Equivalent Java and Kotlin variables MUST produce
  byte-identical requests and persisted hashes.
- **JVM-002:** Kotlin declarations MUST use explicit nullable or non-null types
  and MUST NOT expose platform types as their authored API. Java declarations
  MUST apply class-retained, type-use nullability annotations to parameters,
  fields, and return types so Java callers and static analyzers can distinguish
  nullable from non-null values.
- **JVM-003:** Optional input omission, explicit null, and a supplied value are
  distinct. Selected results preserve missing, null, pending, present, failed,
  and skipped. Partial data and structured errors remain simultaneously
  observable, and conversion to a complete required model is explicit.
- **JVM-004:** Open enums retain an unknown raw value. Open unions retain the
  unknown discriminator and payload. Closed protocol and stream-control
  variants remain strict and fail with `INVALID_RESPONSE`.
- **JVM-005:** `BigInteger`, `BigDecimal`, `Instant`, `Duration`, `UUID`, and
  byte arrays are convenience adapters over canonical string-backed wire
  values. An adapter MUST round-trip the canonical value exactly or reject it;
  it MUST NOT truncate precision, select a default locale or time zone, accept
  padded base64url, or silently narrow a range.

## Portable transport boundary

- **JVM-100:** The core transport is an application-supplied call boundary. It
  exposes `CompletableFuture`, virtual-thread-friendly blocking, Kotlin
  coroutine, Java `Flow.Publisher`, and Kotlin `Flow` views without selecting a
  server framework or an Android-only API for generated wire models. SSE and
  WebSocket are capability methods; an absent adapter returns
  `UNSUPPORTED_CAPABILITY` rather than advertising a pass.
- **JVM-101:** Cancelling a future, interrupting or expiring a blocking call,
  cancelling a coroutine, cancelling a Java subscription, or abandoning a
  Kotlin flow MUST invoke the underlying call's active cancellation hook.
  Normal stream completion requires an explicit terminal result. Source EOF
  before terminal is `STREAM_TRUNCATED`, and both outcomes release the call.
- **JVM-102:** Unary responses default to an 8 MiB compressed bound and a 16
  MiB decompressed bound; stream frames default to a 1 MiB bound. Bounds apply
  before decode. Malformed UTF-8, duplicate keys,
  trailing bytes, malformed envelopes, unknown required control fields, and
  oversized values MUST NOT return a successful typed result.
- **JVM-103:** Authentication uses an explicit interceptor. Cross-origin
  redirects strip authorization, cookie, proxy authorization, API-key, and
  tenant credentials unless the exact destination origin is allowlisted. The
  core performs no automatic authentication refresh or retry, so it never
  silently replays a non-idempotent write.
- **JVM-104:** Generated persisted operations and cursor page values are
  transport-independent. A deadline budget applies to the whole call view and
  cannot be reset by an adapter. Optional retry, pagination orchestration, SSE, and
  WebSocket adapters MUST advertise their own capability and cancellation
  guarantee.

## Compatibility and evidence

- **JVM-200:** The core source and bytecode floor is Java 17. The supported JVM
  runtime matrix is JDK 17, 21, and 25. Kotlin 2.2.20 and
  kotlinx-coroutines 1.10.2 define the Kotlin ABI evidence. Build metadata
  targets Gradle 9.1 and Maven 3.9 for `io.naatre:naatre-jvm-sdk`; those rows
  remain `not-executed` until #82 supplies build-tool evidence. Other JDK,
  Kotlin, Gradle, and Maven versions are not implied compatible.
- **JVM-201:** The generated wire-model source is Android-neutral, but this core
  profile alone certifies no Android API level, Gradle plugin, desugaring
  configuration, device, emulator, HTTP engine, SSE engine, or WebSocket engine.
  An extracted adapter profile states its exact Android compile/minimum API and
  tooling boundary separately from server-JVM evidence. DEX compatibility is
  never presented as device or emulator execution.
- **JVM-202:** Every combination marked passed MUST link to a checked
  conformance report recording its exact runtime, compiler, bytecode target,
  profile, commands, and vectors. A declared or extracted combination without
  executed evidence MUST be marked `not-executed` or `unsupported`, never
  passed.

| Clause | Valid example | Invalid example |
| --- | --- | --- |
| JVM-001 | Java and Kotlin encode the shared `GetAccount` fixture to identical canonical bytes. | Each language hashes a host-object serialization. |
| JVM-002 | Kotlin uses `String?`; Java emits `@Nullable String`. | An authored Kotlin API exposes `String!`. |
| JVM-003 | A missing nickname differs from an explicit null nickname. | Both become Java null before encoding. |
| JVM-004 | `FUTURE` becomes an observable unknown enum carrying `FUTURE`. | Decoding throws only because the server added an open enum member. |
| JVM-005 | An `Instant` retains all nine fractional digits. | A date adapter rounds to milliseconds. |
| JVM-100 | A missing WebSocket adapter returns `UNSUPPORTED_CAPABILITY`. | Core presence implies every transport is advertised. |
| JVM-101 | Cancelling collection invokes the stream call cancellation hook. | Coroutine cancellation leaves a socket read pending. |
| JVM-102 | EOF before a terminal frame fails as truncated. | EOF is returned as successful completion. |
| JVM-103 | Credentials are stripped when redirect origin changes. | A write is replayed after auth refresh without idempotency. |
| JVM-104 | A pagination adapter is a separately advertised capability. | Generating a page type implies an installed pagination transport. |
| JVM-200 | JDK 17, 21, and 25 reports all execute the same profile. | Untested JDK 26 is inferred supported because compilation succeeded elsewhere. |
| JVM-201 | Android API-26 DEX compatibility and server HTTP have separate reports. | A server JVM build or DEX conversion is labeled a device pass. |
| JVM-202 | Every passed matrix row links a digest-pinned report. | A CI workflow definition is treated as a completed report. |
