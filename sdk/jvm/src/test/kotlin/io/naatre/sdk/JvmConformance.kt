package io.naatre.sdk

import io.naatre.sdk.ClientCore.Call
import io.naatre.sdk.ClientCore.Client
import io.naatre.sdk.ClientCore.ClientException
import io.naatre.sdk.ClientCore.ErrorCode
import io.naatre.sdk.ClientCore.Input
import io.naatre.sdk.ClientCore.Json
import io.naatre.sdk.ClientCore.JsonArray
import io.naatre.sdk.ClientCore.JsonBoolean
import io.naatre.sdk.ClientCore.JsonNumber
import io.naatre.sdk.ClientCore.JsonObject
import io.naatre.sdk.ClientCore.JsonString
import io.naatre.sdk.ClientCore.JsonValue
import io.naatre.sdk.ClientCore.OpenVariant
import io.naatre.sdk.ClientCore.OperationError
import io.naatre.sdk.ClientCore.OperationResult
import io.naatre.sdk.ClientCore.RedirectPolicy
import io.naatre.sdk.ClientCore.Selected
import io.naatre.sdk.ClientCore.StreamCall
import io.naatre.sdk.ClientCore.Transport
import io.naatre.sdk.ClientCore.TransportRequest
import io.naatre.sdk.ClientCore.TransportResponse
import io.naatre.sdk.ClientCore.WireScalars
import io.naatre.sdk.generated.java.GetAccount as JavaGetAccount
import io.naatre.sdk.generated.kotlin.GetAccountResult
import io.naatre.sdk.generated.kotlin.GetAccountVariables
import io.naatre.sdk.generated.kotlin.Status
import io.naatre.sdk.generated.kotlin.getAccountOperation
import java.net.URI
import java.nio.charset.StandardCharsets
import java.nio.file.Files
import java.nio.file.Path
import java.time.Duration
import java.time.Instant
import java.util.Locale
import java.util.TimeZone
import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.Flow as JdkFlow
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking

fun main(arguments: Array<String>) = runBlocking {
    val root = Path.of(arguments.single())
    scalarVectorsAreLossless(root)
    localeAndTimeZoneDoNotAffectWireValues()
    generatedClientsEmitIdenticalRequests()
    nullabilityAndUnknownVariantsRemainObservable(root)
    partialDataErrorsAndMalformedResponsesAreStrict()
    futureBlockingCoroutineAndFlowCancellationReleaseCalls()
    streamTerminalLimitsRedirectsAuthenticationAndUnsupportedCapabilities()
    println("{\"generatorVersion\":\"naatre.generator.jvm-sdk-1\",\"profile\":\"sdk.jvm.core-1\",\"status\":\"passed\"}")
}

private fun scalarVectorsAreLossless(root: Path) {
    val fixture = Json.`object`(Json.parse(Files.readAllBytes(root.resolve("conformance/v1/scalars.json"))))
    val vectors = (fixture.values()["vectors"] as JsonArray).values()
    val extended = setOf("Int64", "UInt64", "BigInt", "Decimal", "Timestamp", "Duration", "UUID", "Bytes")
    for (entry in vectors) {
        val vector = Json.`object`(entry)
        val kind = Json.string(vector.values()["kind"])
        if (kind !in extended) continue
        val valid = (vector.values()["valid"] as JsonBoolean).value()
        try {
            val input = Json.parse(Json.string(vector.values()["input"]).toByteArray())
            val wire = (input as? JsonString)?.value() ?: throw ClientException(ErrorCode.SCALAR_INVALID)
            val canonical = when (kind) {
                "Int64" -> WireScalars.int64(wire)
                "UInt64" -> WireScalars.uint64(wire)
                "BigInt" -> WireScalars.bigInteger(wire)
                "Decimal" -> WireScalars.decimal(wire)
                "Timestamp" -> WireScalars.timestamp(wire)
                "Duration" -> WireScalars.duration(wire)
                "UUID" -> WireScalars.uuid(wire)
                "Bytes" -> WireScalars.fromBytes(WireScalars.bytes(wire))
                else -> error("unreachable")
            }
            check(valid, "${Json.string(vector.values()["name"])} unexpectedly passed")
            val expected = Json.string(Json.parse(Json.string(vector.values()["canonical"]).toByteArray()))
            check(canonical == expected, "$kind canonical value differs: $canonical != $expected")
        } catch (failure: RuntimeException) {
            if (valid) throw failure
        }
    }
    val decimal = WireScalars.asBigDecimal("123.45")
    check(WireScalars.fromBigDecimal(decimal) == "123.45", "BigDecimal adapter lost precision")
    check(WireScalars.fromBigInteger(WireScalars.asBigInteger("9007199254740993")) == "9007199254740993", "BigInteger adapter lost precision")
    check(WireScalars.fromInstant(WireScalars.asInstant("2026-09-11T20:30:01.123456789Z")) == "2026-09-11T20:30:01.123456789Z", "Instant adapter lost precision")
    check(WireScalars.fromDuration(WireScalars.asDuration("-9223372036854775808")) == "-9223372036854775808", "Duration adapter lost precision")
    expectCode(ErrorCode.SCALAR_INVALID) { WireScalars.asDuration("9223372036854775808") }
    expectCode(ErrorCode.SCALAR_INVALID) { WireScalars.fromDuration(Duration.ofSeconds(Long.MAX_VALUE)) }
    check(WireScalars.fromUuid(WireScalars.asUuid("550e8400-e29b-41d4-a716-446655440000")) == "550e8400-e29b-41d4-a716-446655440000", "UUID adapter changed wire value")
    check(WireScalars.fromBytes(WireScalars.bytes("SGVsbG8")) == "SGVsbG8", "byte adapter changed wire value")
}

private fun localeAndTimeZoneDoNotAffectWireValues() {
    val originalLocale = Locale.getDefault()
    val originalZone = TimeZone.getDefault()
    try {
        for ((locale, zone) in listOf(Locale.FRANCE to "Pacific/Kiritimati", Locale.JAPAN to "America/Adak", Locale.forLanguageTag("lv-LV") to "Europe/Riga")) {
            Locale.setDefault(locale)
            TimeZone.setDefault(TimeZone.getTimeZone(zone))
            check(
                WireScalars.timestamp("2026-09-11T23:30:01.123456789+03:00") == "2026-09-11T20:30:01.123456789Z",
                "timestamp depends on default locale or zone",
            )
            check(WireScalars.decimal("001.2300") == "1.23", "decimal depends on default locale")
        }
    } finally {
        Locale.setDefault(originalLocale)
        TimeZone.setDefault(originalZone)
    }
}

private fun generatedClientsEmitIdenticalRequests() {
    val javaVariables = JavaGetAccount.Variables(
        "acct-1",
        Input.nullValue(),
        Input.value(emptyList()),
        Input.value(emptyMap()),
    )
    val kotlinVariables = GetAccountVariables(
        id = "acct-1",
        nickname = Input.nullValue(),
        tags = Input.value(emptyList()),
        filter = Input.value(emptyMap()),
    )
    val javaBytes = JavaGetAccount.operation().canonicalRequest(javaVariables)
    val kotlinBytes = getAccountOperation().canonicalRequest(kotlinVariables)
    val expected = "{\"operation\":\"GetAccount\",\"persisted\":{\"algorithm\":\"sha-256\",\"canonicalVersion\":\"c14n-1\",\"digest\":\"cc863005080edcb85ec0345a50593dc58111896bbbe7ff86475ef2412fc5cb90\"},\"variables\":{\"filter\":{},\"id\":\"acct-1\",\"nickname\":null,\"tags\":[]},\"version\":\"1\"}"
    check(javaBytes.contentEquals(kotlinBytes), "Java and Kotlin request bytes differ")
    check(javaBytes.toString(StandardCharsets.UTF_8) == expected, "request differs from shared reference output")
    check(JavaGetAccount.PERSISTED_DIGEST == io.naatre.sdk.generated.kotlin.GET_ACCOUNT_PERSISTED_DIGEST, "persisted hashes differ")
}

private fun nullabilityAndUnknownVariantsRemainObservable(root: Path) {
    val javaStatus = JavaGetAccount.Status.fromWire("FUTURE")
    check(!javaStatus.known() && javaStatus.wireValue() == "FUTURE", "Java unknown enum was erased")
    val kotlinStatus = Status.fromWire("FUTURE")
    check(kotlinStatus is Status.Unknown && kotlinStatus.wireValue == "FUTURE", "Kotlin unknown enum was erased")
    val unknown = Json.`object`(Json.parse("{\"\$type\":\"Future\",\"\$value\":{\"field\":7}}".toByteArray()))
    val variant = OpenVariant(Json.string(unknown.values()["\$type"]), unknown.values()["\$value"]!!)
    check(variant.discriminator() == "Future" && variant.value() is JsonObject, "unknown union was erased")

    val kotlinSource = Files.readString(root.resolve("sdk/jvm/generated/kotlin/io/naatre/sdk/generated/kotlin/GetAccount.kt"))
    val javaSource = Files.readString(root.resolve("sdk/jvm/generated/java/io/naatre/sdk/generated/java/GetAccount.java"))
    check("val balance: String?" in kotlinSource && "val id: String" in kotlinSource, "Kotlin nullability is not explicit")
    check("@Nullable String balance" in javaSource && "@NonNull String id" in javaSource, "Java nullability annotations are missing")
}

private fun partialDataErrorsAndMalformedResponsesAreStrict() {
    val partial = getAccountOperation().decodeResult(
        "{\"complete\":false,\"data\":{\"profile\":{\"display\":\"Ada\",\"nickname\":null}},\"errors\":[{\"code\":\"PARTIAL\",\"message\":\"later failed\",\"path\":[\"later\"]}]}".toByteArray(),
    )
    check(!partial.complete() && partial.errors().single().code() == "PARTIAL", "partial error was erased")
    val data = (partial.data() as Selected.Present<GetAccountResult>).value()
    val profile = (data.profile as Selected.Present).value()
    check((profile.display as Selected.Present).value() == "Ada", "partial data was erased")
    check(profile.nickname is Selected.NullValue && data.later is Selected.Pending, "missing/null/pending states collapsed")
    val javaPartial = JavaGetAccount.operation().decodeResult(
        "{\"complete\":false,\"data\":{\"profile\":{\"display\":\"Ada\",\"nickname\":null}},\"errors\":[{\"code\":\"PARTIAL\",\"message\":\"later failed\",\"path\":[\"later\"]}]}".toByteArray(),
    )
    check(!javaPartial.complete() && javaPartial.errors().single().code() == "PARTIAL", "Java partial error was erased")
    val javaData = (javaPartial.data() as Selected.Present<JavaGetAccount.Result>).value()
    val javaProfile = (javaData.profile() as Selected.Present).value()
    check((javaProfile.display() as Selected.Present).value() == "Ada", "Java partial data was erased")
    check(javaProfile.nickname() is Selected.NullValue && javaData.later() is Selected.Pending, "Java presence states collapsed")

    val operationError = OperationError("FAILED", "failed", listOf("profile"), emptyMap())
    val failed = Selected.failed<String>(listOf(operationError))
    val skipped = Selected.skipped<String>("policy")
    check(failed is Selected.Failed && failed.errors().single().code() == "FAILED", "failed selection state was erased")
    check(skipped is Selected.Skipped && skipped.reason() == "policy", "skipped selection state was erased")

    for (malformed in listOf(
        "{\"complete\":",
        "{\"complete\":true,\"unknown\":1}",
        "{\"data\":null}",
        "{\"complete\":true,\"complete\":false}",
        "{\"complete\":true} trailing",
    )) {
        expectCode(ErrorCode.INVALID_RESPONSE) { getAccountOperation().decodeResult(malformed.toByteArray()) }
    }
}

private suspend fun futureBlockingCoroutineAndFlowCancellationReleaseCalls() = coroutineScope {
    val javaTransport = RecordingTransport()
    val javaClient = Client(javaTransport)
    val variables = JavaGetAccount.Variables("acct-1")
    val future = javaClient.executeAsync(JavaGetAccount.operation(), variables, null)
    future.cancel(true)
    eventually { javaTransport.unaryCancellations.get() == 1 }

    val deadlineTransport = RecordingTransport()
    expectCode(ErrorCode.DEADLINE_EXCEEDED) {
        Client(deadlineTransport).executeBlocking(JavaGetAccount.operation(), variables, Duration.ofMillis(5))
    }
    eventually { deadlineTransport.unaryCancellations.get() == 1 }

    val expiredTransport = RecordingTransport()
    expectCode(ErrorCode.DEADLINE_EXCEEDED) {
        Client(expiredTransport).executeBlocking(JavaGetAccount.operation(), variables, Duration.ZERO)
    }
    eventually { expiredTransport.unaryCancellations.get() == 1 }

    val interruptTransport = RecordingTransport()
    val blockingFailure = AtomicReference<Throwable>()
    val blocking = Thread {
        try {
            Client(interruptTransport).executeBlocking(JavaGetAccount.operation(), variables, null)
        } catch (failure: Throwable) {
            blockingFailure.set(failure)
        }
    }
    blocking.start()
    eventually { interruptTransport.lastCall != null }
    blocking.interrupt()
    blocking.join(1_000)
    check(!blocking.isAlive, "interrupted blocking call did not return")
    check((blockingFailure.get() as? ClientException)?.code() == ErrorCode.CANCELLED, "blocking interruption was not typed")
    eventually { interruptTransport.unaryCancellations.get() == 1 }

    val coroutineTransport = RecordingTransport()
    val coroutine = launch {
        KotlinClient(Client(coroutineTransport)).execute(getAccountOperation(), GetAccountVariables("acct-1"))
    }
    eventually { coroutineTransport.lastCall != null }
    coroutine.cancelAndJoin()
    eventually { coroutineTransport.unaryCancellations.get() == 1 }

    val flowTransport = RecordingTransport(streamFrames = null)
    val collector = launch {
        KotlinClient(Client(flowTransport)).streamSse(getAccountOperation(), GetAccountVariables("acct-1")).collect()
    }
    eventually { flowTransport.streamSubscribed.get() }
    collector.cancelAndJoin()
    eventually { flowTransport.streamCancellations.get() == 1 }

    val burstFrame = "{\"complete\":false,\"data\":null}".toByteArray()
    val burstTransport = RecordingTransport(streamFrames = List(100) { burstFrame })
    try {
        KotlinClient(Client(burstTransport)).streamSse(getAccountOperation(), GetAccountVariables("acct-1")).collect()
        fail("overflowing Kotlin Flow succeeded")
    } catch (failure: ClientException) {
        check(failure.code() == ErrorCode.TRANSPORT, "Kotlin Flow overflow returned ${failure.code()}")
    }
    check(burstTransport.streamCancellations.get() >= 1, "Kotlin Flow overflow did not release transport")

    val javaFlowTransport = RecordingTransport(streamFrames = null)
    val javaFlowSubscribed = AtomicBoolean()
    Client(javaFlowTransport).streamSse(JavaGetAccount.operation(), variables).subscribe(
        object : JdkFlow.Subscriber<OperationResult<JavaGetAccount.Result>> {
            override fun onSubscribe(subscription: JdkFlow.Subscription) {
                javaFlowSubscribed.set(true)
                subscription.cancel()
            }

            override fun onNext(item: OperationResult<JavaGetAccount.Result>) = Unit
            override fun onError(throwable: Throwable) = fail("Java Flow cancellation failed: $throwable")
            override fun onComplete() = Unit
        },
    )
    eventually { javaFlowSubscribed.get() && javaFlowTransport.streamCancellations.get() == 1 }

    val delayedTransport = DelayedStreamTransport()
    val earlyCancellation = launch {
        KotlinClient(Client(delayedTransport)).streamSse(getAccountOperation(), GetAccountVariables("acct-1")).collect()
    }
    eventually { delayedTransport.subscribed.get() }
    earlyCancellation.cancelAndJoin()
    delayedTransport.deliverSubscription()
    eventually { delayedTransport.cancellations.get() == 1 }
}

private suspend fun streamTerminalLimitsRedirectsAuthenticationAndUnsupportedCapabilities() {
    val incomplete = "{\"complete\":false,\"data\":{\"profile\":{\"display\":\"Ada\"}}}".toByteArray()
    val truncatedTransport = RecordingTransport(streamFrames = listOf(incomplete))
    try {
        KotlinClient(Client(truncatedTransport)).streamSse(getAccountOperation(), GetAccountVariables("acct-1")).collect()
        fail("truncated stream succeeded")
    } catch (failure: ClientException) {
        check(failure.code() == ErrorCode.STREAM_TRUNCATED, "truncated stream returned ${failure.code()}")
    }
    check(truncatedTransport.streamCancellations.get() >= 1, "truncated stream did not release transport")

    val complete = "{\"complete\":true,\"data\":{\"profile\":{\"display\":\"Ada\"}}}".toByteArray()
    val terminalTransport = RecordingTransport(streamFrames = listOf(complete))
    KotlinClient(Client(terminalTransport)).streamSse(getAccountOperation(), GetAccountVariables("acct-1")).collect()
    check(terminalTransport.streamCancellations.get() >= 1, "terminal stream did not release transport")

    val oversized = RecordingTransport(response = ByteArray(33), compressedBytes = 1)
    expectCode(ErrorCode.RESPONSE_TOO_LARGE) {
        Client(oversized, { emptyMap() }, 32, 32, 32).executeBlocking(JavaGetAccount.operation(), JavaGetAccount.Variables("acct-1"), null)
    }
    val compressedOversized = RecordingTransport(response = complete, compressedBytes = 33)
    expectCode(ErrorCode.RESPONSE_TOO_LARGE) {
        Client(compressedOversized, { emptyMap() }, 32, 1024, 32)
            .executeBlocking(JavaGetAccount.operation(), JavaGetAccount.Variables("acct-1"), null)
    }
    val largeMessage = "x".repeat(ClientCore.DEFAULT_RESPONSE_BYTES)
    val largeResponse = "{\"complete\":true,\"errors\":[{\"code\":\"LARGE\",\"message\":\"$largeMessage\"}]}".toByteArray()
    val configuredLarge = RecordingTransport(response = largeResponse, compressedBytes = 1)
    val configuredLargeResult = Client(configuredLarge, { emptyMap() }, 32, largeResponse.size + 1, 32)
        .executeBlocking(JavaGetAccount.operation(), JavaGetAccount.Variables("acct-1"), null)
    check(configuredLargeResult.errors().single().message() == largeMessage, "configured decompressed response limit was ignored")

    for (stage in listOf("frames", "subscribe")) {
        val setupFailure = RecordingTransport(streamSetupFailure = stage)
        try {
            KotlinClient(Client(setupFailure)).streamSse(getAccountOperation(), GetAccountVariables("acct-1")).collect()
            fail("$stage setup failure succeeded")
        } catch (failure: ClientException) {
            check(failure.code() == ErrorCode.TRANSPORT, "$stage setup failure returned ${failure.code()}")
        }
        check(setupFailure.streamCancellations.get() >= 1, "$stage setup failure did not release transport")
    }

    val headers = mapOf("Authorization" to "Bearer secret", "Cookie" to "session", "Accept" to "application/json")
    val stripped = RedirectPolicy.forwardedHeaders(headers, URI("https://a.example/request"), URI("https://b.example/request"), emptySet())
    check(stripped == mapOf("Accept" to "application/json"), "cross-origin credentials were retained")
    val retained = RedirectPolicy.forwardedHeaders(headers, URI("https://a.example/request"), URI("https://b.example/request"), setOf("https://b.example"))
    check(retained == headers, "allowlisted cross-origin credentials were stripped")
    val equivalentDefaultPort = RedirectPolicy.forwardedHeaders(headers, URI("https://a.example/request"), URI("https://a.example:443/request"), emptySet())
    check(equivalentDefaultPort == headers, "equivalent default port was treated as cross-origin")

    val oversizedFrame = RecordingTransport(streamFrames = listOf(ByteArray(33)))
    try {
        KotlinClient(Client(oversizedFrame, { emptyMap() }, 1024, 1024, 32))
            .streamSse(getAccountOperation(), GetAccountVariables("acct-1"))
            .collect()
        fail("oversized stream frame succeeded")
    } catch (failure: ClientException) {
        check(failure.code() == ErrorCode.FRAME_TOO_LARGE, "oversized frame returned ${failure.code()}")
    }
    check(oversizedFrame.streamCancellations.get() >= 1, "oversized frame did not release transport")

    val authTransport = RecordingTransport(response = complete)
    Client(authTransport, { mapOf("Authorization" to "Bearer fixture") }, 1024, 1024, 1024)
        .executeBlocking(JavaGetAccount.operation(), JavaGetAccount.Variables("acct-1"), null)
    check(authTransport.lastRequest?.headers()?.get("Authorization") == "Bearer fixture", "authentication interceptor was not applied")
    check(
        authTransport.lastRequest?.let { it.compressedBytes() == 1024 && it.decompressedBytes() == 1024 && it.frameBytes() == 1024 } == true,
        "transport request omitted resource bounds",
    )
    try {
        KotlinClient(Client(RecordingTransport())).streamWebSocket(getAccountOperation(), GetAccountVariables("acct-1")).collect()
        fail("unsupported WebSocket succeeded")
    } catch (failure: ClientException) {
        check(failure.code() == ErrorCode.UNSUPPORTED_CAPABILITY, "unsupported WebSocket returned ${failure.code()}")
    }
}

private class RecordingTransport(
    private val response: ByteArray? = null,
    private val compressedBytes: Long? = null,
    private val streamFrames: List<ByteArray>? = emptyList(),
    private val streamSetupFailure: String? = null,
) : Transport {
    val unaryCancellations = AtomicInteger()
    val streamCancellations = AtomicInteger()
    val streamSubscribed = AtomicBoolean()
    @Volatile var lastCall: RecordingCall? = null
    @Volatile var lastRequest: TransportRequest? = null

    override fun execute(request: TransportRequest): Call<TransportResponse> {
        lastRequest = request
        val transportResponse = response?.let { TransportResponse(it, compressedBytes ?: it.size.toLong()) }
        return RecordingCall(transportResponse, unaryCancellations).also { lastCall = it }
    }

    override fun sse(request: TransportRequest): StreamCall {
        lastRequest = request
        return RecordingStreamCall(streamFrames, streamCancellations, streamSubscribed, streamSetupFailure)
    }
}

private class DelayedStreamTransport : Transport {
    val subscribed = AtomicBoolean()
    val cancellations = AtomicInteger()
    private val downstream = AtomicReference<JdkFlow.Subscriber<in ByteArray>?>()

    override fun execute(request: TransportRequest): Call<TransportResponse> =
        throw ClientException(ErrorCode.UNSUPPORTED_CAPABILITY)

    override fun sse(request: TransportRequest): StreamCall = object : StreamCall {
        override fun frames(): JdkFlow.Publisher<ByteArray> = JdkFlow.Publisher { subscriber ->
            downstream.set(subscriber)
            subscribed.set(true)
        }

        override fun cancel() {
            cancellations.incrementAndGet()
        }
    }

    fun deliverSubscription() {
        val subscriber = downstream.get() ?: fail("delayed stream was not subscribed")
        subscriber.onSubscribe(object : JdkFlow.Subscription {
            override fun request(count: Long) = Unit
            override fun cancel() = Unit
        })
    }
}

private class RecordingCall(response: TransportResponse?, private val cancellations: AtomicInteger) : Call<TransportResponse> {
    private val completion = CompletableFuture<TransportResponse>()
    init { if (response != null) completion.complete(response) }
    override fun future(): CompletableFuture<TransportResponse> = completion
    override fun cancel() { cancellations.incrementAndGet(); completion.cancel(true) }
}

private class RecordingStreamCall(
    private val frames: List<ByteArray>?,
    private val cancellations: AtomicInteger,
    private val subscribed: AtomicBoolean,
    private val setupFailure: String?,
) : StreamCall {
    override fun frames(): JdkFlow.Publisher<ByteArray> {
        if (setupFailure == "frames") throw IllegalStateException("frames failed")
        return JdkFlow.Publisher { downstream ->
            if (setupFailure == "subscribe") throw IllegalStateException("subscribe failed")
            downstream.onSubscribe(object : JdkFlow.Subscription {
                private val cancelled = AtomicBoolean()
                override fun request(count: Long) {
                    subscribed.set(true)
                    if (cancelled.get() || frames == null) return
                    for (frame in frames) if (!cancelled.get()) downstream.onNext(frame)
                    if (!cancelled.get()) downstream.onComplete()
                }
                override fun cancel() { cancelled.set(true) }
            })
        }
    }
    override fun cancel() { cancellations.incrementAndGet() }
}

private inline fun expectCode(expected: ErrorCode, block: () -> Unit) {
    try {
        block()
        fail("expected $expected")
    } catch (failure: ClientException) {
        check(failure.code() == expected, "got ${failure.code()}, expected $expected")
    }
}

private suspend fun eventually(condition: () -> Boolean) {
    repeat(100) {
        if (condition()) return
        delay(5)
    }
    fail("condition was not observed")
}

private fun fail(message: String): Nothing = throw AssertionError(message)
private fun check(condition: Boolean, message: String) { if (!condition) fail(message) }
