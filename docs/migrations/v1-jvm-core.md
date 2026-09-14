# v1 JVM core migration note

The `sdk.jvm.core-1` profile adds Java and Kotlin generated bindings for the
existing `naatre.generator-model-1` contract. It does not alter server request,
response, scalar, persisted-operation, or streaming wire formats.

JVM consumers should represent optional inputs with `Input` instead of Java or
Kotlin null alone, and selected results with `Selected` instead of projecting
partial data directly onto required fields. Open enum switches must include an
unknown raw-value case. Native numeric and time conversions must retain the
canonical wrapper or reject a value that cannot round-trip exactly.

Transport integrations must return cancellable call handles, close work on
future/coroutine/subscription cancellation, fail EOF-before-terminal streams,
and apply the shared response/frame limits and redirect credential policy.
Existing integrations cannot claim `sdk.jvm.core-1` merely because their data
classes compile. Concrete HTTP, Android, SSE, and WebSocket claims require #82
evidence, and combined compatibility certification requires #69.
