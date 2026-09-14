# Decision 0002: JVM core and adapter boundary

Status: accepted for v1.

The JVM deliverable separates deterministic Java/Kotlin wire declarations and
transport-neutral cancellation contracts from concrete network and Android
integrations. The core is one Java 17 artifact with Kotlin coroutine and Flow
views. It owns exact request bytes, selected presence states, open variants,
lossless scalar wrappers, deadlines, limits, redirect credentials, and active
call cancellation.

HTTP engines, authenticated POST-SSE, WebSocket implementations, Android
Gradle packaging, desugaring, and device/emulator evidence remain separately
advertised adapters in #82. The combined release matrix remains #69. This
prevents a generated model from implying platform or transport support and
keeps Android-only and server-only APIs out of generated wire declarations.

The alternative of selecting one HTTP stack in the core was rejected because
it would impose server or Android dependencies on every consumer. Separate
Java and Kotlin encoders were also rejected because drift between their request
bytes and persisted hashes would become possible.
