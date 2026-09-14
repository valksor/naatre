package io.naatre.sdk;

import io.naatre.sdk.annotations.NonNull;
import io.naatre.sdk.annotations.Nullable;
import java.io.ByteArrayOutputStream;
import java.math.BigDecimal;
import java.math.BigInteger;
import java.net.URI;
import java.nio.ByteBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.CodingErrorAction;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.time.Instant;
import java.time.OffsetDateTime;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;
import java.time.format.DateTimeParseException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.Collections;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.CancellationException;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionException;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Flow;
import java.util.concurrent.ScheduledFuture;
import java.util.concurrent.ScheduledThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.function.Function;
import java.util.regex.Pattern;

/** Platform-neutral wire and transport contracts shared by generated Java and Kotlin bindings. */
public final class ClientCore {
    public static final int DEFAULT_RESPONSE_BYTES = 8 << 20;
    public static final int DEFAULT_DECOMPRESSED_BYTES = 16 << 20;
    public static final int DEFAULT_FRAME_BYTES = 1 << 20;
    public static final int DEFAULT_REDIRECTS = 5;
    private static final ScheduledThreadPoolExecutor DEADLINES = deadlineExecutor();

    private ClientCore() {}

    public enum ErrorCode {
        CANCELLED,
        DEADLINE_EXCEEDED,
        INVALID_REQUEST,
        INVALID_RESPONSE,
        RESPONSE_TOO_LARGE,
        FRAME_TOO_LARGE,
        STREAM_TRUNCATED,
        TRANSPORT,
        UNSUPPORTED_CAPABILITY,
        SCALAR_INVALID
    }

    public static final class ClientException extends RuntimeException {
        private static final long serialVersionUID = 1L;

        private final ErrorCode code;

        public ClientException(@NonNull ErrorCode code) {
            this(code, code.name(), null);
        }

        public ClientException(@NonNull ErrorCode code, @NonNull String message) {
            this(code, message, null);
        }

        public ClientException(@NonNull ErrorCode code, @NonNull String message, @Nullable Throwable cause) {
            super(message, cause);
            this.code = Objects.requireNonNull(code, "code");
        }

        @NonNull
        public ErrorCode code() {
            return code;
        }
    }

    public sealed interface Input<T> permits Input.Missing, Input.NullValue, Input.Value {
        final class Missing<T> implements Input<T> {
            private Missing() {}
        }

        final class NullValue<T> implements Input<T> {
            private NullValue() {}
        }

        record Value<T>(@NonNull T value) implements Input<T> {
            public Value {
                Objects.requireNonNull(value, "value");
            }
        }

        static <T> @NonNull Input<T> missing() {
            return new Missing<>();
        }

        static <T> @NonNull Input<T> nullValue() {
            return new NullValue<>();
        }

        static <T> @NonNull Input<T> value(@NonNull T value) {
            return new Value<>(value);
        }
    }

    public sealed interface Selected<T>
            permits Selected.Missing, Selected.NullValue, Selected.Pending, Selected.Present, Selected.Failed, Selected.Skipped {
        final class Missing<T> implements Selected<T> {
            private Missing() {}
        }

        final class NullValue<T> implements Selected<T> {
            private NullValue() {}
        }

        final class Pending<T> implements Selected<T> {
            private Pending() {}
        }

        record Present<T>(@NonNull T value) implements Selected<T> {
            public Present {
                Objects.requireNonNull(value, "value");
            }
        }

        record Failed<T>(@NonNull List<OperationError> errors) implements Selected<T> {
            public Failed {
                errors = List.copyOf(errors);
            }
        }

        record Skipped<T>(@NonNull String reason) implements Selected<T> {
            public Skipped {
                Objects.requireNonNull(reason, "reason");
            }
        }

        static <T> @NonNull Selected<T> missing() {
            return new Missing<>();
        }

        static <T> @NonNull Selected<T> nullValue() {
            return new NullValue<>();
        }

        static <T> @NonNull Selected<T> pending() {
            return new Pending<>();
        }

        static <T> @NonNull Selected<T> present(@NonNull T value) {
            return new Present<>(value);
        }

        static <T> @NonNull Selected<T> failed(@NonNull List<OperationError> errors) {
            return new Failed<>(errors);
        }

        static <T> @NonNull Selected<T> skipped(@NonNull String reason) {
            return new Skipped<>(reason);
        }
    }

    public record OperationError(
            @NonNull String code,
            @Nullable String message,
            @NonNull List<Object> path,
            @NonNull Map<String, JsonValue> details) {
        public OperationError {
            Objects.requireNonNull(code, "code");
            path = List.copyOf(path);
            details = Map.copyOf(details);
        }
    }

    public record OperationResult<T>(@NonNull Selected<T> data, @NonNull List<OperationError> errors, boolean complete) {
        public OperationResult {
            Objects.requireNonNull(data, "data");
            errors = List.copyOf(errors);
        }
    }

    public record OpenVariant(@NonNull String discriminator, @NonNull JsonValue value) {
        public OpenVariant {
            Objects.requireNonNull(discriminator, "discriminator");
            Objects.requireNonNull(value, "value");
        }
    }

    public record PageInfo(
            @Nullable String nextCursor,
            @Nullable String previousCursor,
            boolean hasNextPage,
            boolean hasPreviousPage) {}

    public record Page<T>(@NonNull List<T> items, @NonNull PageInfo pageInfo) {
        public Page {
            items = List.copyOf(items);
            Objects.requireNonNull(pageInfo, "pageInfo");
        }
    }

    public sealed interface JsonValue permits JsonNull, JsonBoolean, JsonNumber, JsonString, JsonArray, JsonObject {}

    public enum JsonNull implements JsonValue {
        INSTANCE
    }

    public record JsonBoolean(boolean value) implements JsonValue {}

    public record JsonNumber(@NonNull String value) implements JsonValue {
        public JsonNumber {
            Objects.requireNonNull(value, "value");
        }
    }

    public record JsonString(@NonNull String value) implements JsonValue {
        public JsonString {
            Objects.requireNonNull(value, "value");
        }
    }

    public record JsonArray(@NonNull List<JsonValue> values) implements JsonValue {
        public JsonArray {
            values = List.copyOf(values);
        }
    }

    public record JsonObject(@NonNull Map<String, JsonValue> values) implements JsonValue {
        public JsonObject {
            values = Collections.unmodifiableMap(new LinkedHashMap<>(values));
        }
    }

    @FunctionalInterface
    public interface Decoder<T> {
        @NonNull T decode(@NonNull JsonValue value);
    }

    public record PersistedReference(@NonNull String algorithm, @NonNull String canonicalVersion, @NonNull String digest) {
        private static final Pattern DIGEST = Pattern.compile("[0-9a-f]{64}");

        public PersistedReference {
            if (!"sha-256".equals(algorithm) || !"c14n-1".equals(canonicalVersion) || !DIGEST.matcher(digest).matches()) {
                throw new ClientException(ErrorCode.INVALID_REQUEST, "invalid persisted reference");
            }
        }
    }

    public record Operation<V, R>(
            @NonNull String name,
            @NonNull String kind,
            @NonNull PersistedReference persisted,
            @NonNull Function<V, Map<String, Object>> encodeVariables,
            @NonNull Decoder<R> decodeData) {
        private static final Pattern NAME = Pattern.compile("[A-Za-z_][A-Za-z0-9_]{0,127}");

        public Operation {
            if (!NAME.matcher(name).matches() || !Set.of("query", "mutation", "subscription").contains(kind)) {
                throw new ClientException(ErrorCode.INVALID_REQUEST, "invalid operation definition");
            }
            Objects.requireNonNull(persisted, "persisted");
            Objects.requireNonNull(encodeVariables, "encodeVariables");
            Objects.requireNonNull(decodeData, "decodeData");
        }

        public byte @NonNull [] canonicalRequest(@NonNull V variables) {
            Map<String, Object> request = new LinkedHashMap<>();
            request.put("version", "1");
            request.put("operation", name);
            request.put("persisted", Map.of(
                    "algorithm", persisted.algorithm(),
                    "canonicalVersion", persisted.canonicalVersion(),
                    "digest", persisted.digest()));
            request.put("variables", encodeVariables.apply(Objects.requireNonNull(variables, "variables")));
            return Json.canonicalBytes(request);
        }

        public @NonNull OperationResult<R> decodeResult(byte @NonNull [] input) {
            return decodeResult(input, DEFAULT_RESPONSE_BYTES);
        }

        private @NonNull OperationResult<R> decodeResult(byte @NonNull [] input, int maximumBytes) {
            JsonObject root = Json.object(Json.parse(input, maximumBytes));
            if (!Set.of("data", "errors", "complete").containsAll(root.values().keySet()) || !root.values().containsKey("complete")) {
                throw new ClientException(ErrorCode.INVALID_RESPONSE, "response contains unsupported control fields");
            }
            if (root.values().keySet().stream().anyMatch(key -> !Set.of("data", "errors", "complete").contains(key))) {
                throw new ClientException(ErrorCode.INVALID_RESPONSE, "response contains unsupported control fields");
            }
            JsonValue completeValue = root.values().get("complete");
            if (!(completeValue instanceof JsonBoolean complete)) {
                throw new ClientException(ErrorCode.INVALID_RESPONSE, "response completion state is missing");
            }
            List<OperationError> errors = decodeErrors(root.values().get("errors"));
            JsonValue dataValue = root.values().get("data");
            Selected<R> data = dataValue == null
                    ? Selected.missing()
                    : dataValue == JsonNull.INSTANCE ? Selected.nullValue() : Selected.present(decodeData.decode(dataValue));
            return new OperationResult<>(data, errors, complete.value());
        }
    }

    public record TransportRequest(
            @NonNull UUID id,
            byte @NonNull [] body,
            @NonNull Map<String, String> headers,
            @NonNull String operationKind,
            int compressedBytes,
            int decompressedBytes,
            int frameBytes) {
        public TransportRequest {
            Objects.requireNonNull(id, "id");
            body = Arrays.copyOf(body, body.length);
            headers = Map.copyOf(headers);
            Objects.requireNonNull(operationKind, "operationKind");
            if (compressedBytes <= 0 || decompressedBytes <= 0 || frameBytes <= 0) {
                throw new ClientException(ErrorCode.INVALID_REQUEST, "transport limits must be positive");
            }
        }

        @Override
        public byte[] body() {
            return Arrays.copyOf(body, body.length);
        }
    }

    public record TransportResponse(byte @NonNull [] body, long compressedBytes) {
        public TransportResponse {
            body = Arrays.copyOf(body, body.length);
            if (compressedBytes < 0) throw new ClientException(ErrorCode.INVALID_RESPONSE, "negative compressed size");
        }

        @Override
        public byte[] body() {
            return Arrays.copyOf(body, body.length);
        }
    }

    /** A single active call. Cancellation must be idempotent and release transport resources. */
    public interface Call<T> {
        /** Returns one non-null completion handle; failures may complete on any thread. */
        @NonNull CompletableFuture<T> future();

        /** Stops active work and may synchronously cancel {@link #future()}. */
        void cancel();
    }

    /** A single active stream whose publisher follows the {@link Flow} signaling contract. */
    public interface StreamCall {
        /** Returns one non-null publisher; frame limits must be enforced before byte-array allocation. */
        @NonNull Flow.Publisher<byte[]> frames();

        /** Stops active work and may be called repeatedly after any terminal signal. */
        void cancel();
    }

    /**
     * Application-supplied transport boundary. Implementations may fail setup by throwing and must enforce
     * the request's compressed, decompressed, and frame bounds while reading, before materializing an
     * oversized byte array. Completion callbacks may run on any thread.
     */
    public interface Transport {
        @NonNull Call<TransportResponse> execute(@NonNull TransportRequest request);

        default @NonNull StreamCall sse(@NonNull TransportRequest request) {
            throw new ClientException(ErrorCode.UNSUPPORTED_CAPABILITY, "SSE transport is not installed");
        }

        default @NonNull StreamCall webSocket(@NonNull TransportRequest request) {
            throw new ClientException(ErrorCode.UNSUPPORTED_CAPABILITY, "WebSocket transport is not installed");
        }
    }

    @FunctionalInterface
    public interface AuthenticationInterceptor {
        @NonNull Map<String, String> headers();
    }

    public static final class Client {
        private final Transport transport;
        private final AuthenticationInterceptor authentication;
        private final int compressedBytes;
        private final int decompressedBytes;
        private final int frameBytes;

        public Client(@NonNull Transport transport) {
            this(transport, Map::of, DEFAULT_RESPONSE_BYTES, DEFAULT_DECOMPRESSED_BYTES, DEFAULT_FRAME_BYTES);
        }

        public Client(
                @NonNull Transport transport,
                @NonNull AuthenticationInterceptor authentication,
                int compressedBytes,
                int decompressedBytes,
                int frameBytes) {
            this.transport = Objects.requireNonNull(transport, "transport");
            this.authentication = Objects.requireNonNull(authentication, "authentication");
            if (compressedBytes <= 0 || decompressedBytes <= 0 || frameBytes <= 0) {
                throw new ClientException(ErrorCode.INVALID_REQUEST, "transport limits must be positive");
            }
            this.compressedBytes = compressedBytes;
            this.decompressedBytes = decompressedBytes;
            this.frameBytes = frameBytes;
        }

        public <V, R> @NonNull CompletableFuture<OperationResult<R>> executeAsync(
                @NonNull Operation<V, R> operation, @NonNull V variables, @Nullable Duration deadline) {
            TransportRequest request = request(operation, variables);
            Call<TransportResponse> call;
            try {
                call = transport.execute(request);
            } catch (RuntimeException failure) {
                return CompletableFuture.failedFuture(mapFailure(failure));
            }
            CancellableFuture<OperationResult<R>> result = new CancellableFuture<>(call);
            call.future().whenComplete((response, failure) -> {
                if (failure != null) {
                    result.completeExceptionally(mapFailure(failure));
                    return;
                }
                try {
                    byte[] body = response.body();
                    if (response.compressedBytes() > compressedBytes || body.length > decompressedBytes) {
                        throw new ClientException(ErrorCode.RESPONSE_TOO_LARGE);
                    }
                    result.complete(operation.decodeResult(body, decompressedBytes));
                } catch (RuntimeException decodeFailure) {
                    call.cancel();
                    result.completeExceptionally(mapFailure(decodeFailure));
                }
            });
            if (deadline != null) {
                long nanos;
                try {
                    nanos = deadline.toNanos();
                } catch (ArithmeticException overflow) {
                    nanos = Long.MAX_VALUE;
                }
                if (nanos <= 0) {
                    if (result.completeExceptionally(new ClientException(ErrorCode.DEADLINE_EXCEEDED))) {
                        call.cancel();
                    }
                } else {
                    ScheduledFuture<?> timeout = DEADLINES.schedule(
                            () -> {
                                if (result.completeExceptionally(new ClientException(ErrorCode.DEADLINE_EXCEEDED))) {
                                    call.cancel();
                                }
                            },
                            nanos,
                            TimeUnit.NANOSECONDS);
                    result.whenComplete((ignored, failure) -> timeout.cancel(false));
                }
            }
            return result;
        }

        public <V, R> @NonNull OperationResult<R> executeBlocking(
                @NonNull Operation<V, R> operation, @NonNull V variables, @Nullable Duration deadline) {
            CompletableFuture<OperationResult<R>> future = executeAsync(operation, variables, deadline);
            try {
                return future.get();
            } catch (InterruptedException interrupted) {
                future.cancel(true);
                Thread.currentThread().interrupt();
                throw new ClientException(ErrorCode.CANCELLED, "blocking call interrupted", interrupted);
            } catch (ExecutionException failure) {
                throw mapFailure(failure.getCause());
            }
        }

        public <V, R> @NonNull Flow.Publisher<OperationResult<R>> streamSse(
                @NonNull Operation<V, R> operation, @NonNull V variables) {
            return stream(operation, variables, transport::sse);
        }

        public <V, R> @NonNull Flow.Publisher<OperationResult<R>> streamWebSocket(
                @NonNull Operation<V, R> operation, @NonNull V variables) {
            return stream(operation, variables, transport::webSocket);
        }

        private <V, R> Flow.Publisher<OperationResult<R>> stream(
                Operation<V, R> operation,
                V variables,
                Function<TransportRequest, StreamCall> open) {
            TransportRequest request = request(operation, variables);
            return subscriber -> {
                StreamCall call;
                try {
                    call = open.apply(request);
                } catch (RuntimeException failure) {
                    subscriber.onSubscribe(new EmptySubscription());
                    subscriber.onError(mapFailure(failure));
                    return;
                }
                StreamSubscriber<R> bridge = new StreamSubscriber<>(subscriber, call, operation, frameBytes);
                try {
                    Flow.Publisher<byte[]> frames = Objects.requireNonNull(call.frames(), "stream publisher");
                    frames.subscribe(bridge);
                } catch (RuntimeException failure) {
                    bridge.setupFailed(failure);
                }
            };
        }

        private <V, R> TransportRequest request(Operation<V, R> operation, V variables) {
            return new TransportRequest(
                    UUID.randomUUID(),
                    operation.canonicalRequest(variables),
                    authentication.headers(),
                    operation.kind(),
                    compressedBytes,
                    decompressedBytes,
                    frameBytes);
        }
    }

    private static final class CancellableFuture<T> extends CompletableFuture<T> {
        private final Call<?> call;

        private CancellableFuture(Call<?> call) {
            this.call = call;
        }

        @Override
        public boolean cancel(boolean mayInterruptIfRunning) {
            call.cancel();
            return super.cancel(mayInterruptIfRunning);
        }
    }

    private static final class EmptySubscription implements Flow.Subscription {
        @Override
        public void request(long count) {}

        @Override
        public void cancel() {}
    }

    private static final class StreamSubscriber<R> implements Flow.Subscriber<byte[]> {
        private final Flow.Subscriber<? super OperationResult<R>> downstream;
        private final StreamCall call;
        private final Operation<?, R> operation;
        private final int maximumBytes;
        private Flow.Subscription upstream;
        private boolean terminal;
        private boolean done;

        private StreamSubscriber(
                Flow.Subscriber<? super OperationResult<R>> downstream,
                StreamCall call,
                Operation<?, R> operation,
                int maximumBytes) {
            this.downstream = downstream;
            this.call = call;
            this.operation = operation;
            this.maximumBytes = maximumBytes;
        }

        private synchronized void setupFailed(Throwable failure) {
            if (done) {
                return;
            }
            done = true;
            if (upstream == null) {
                downstream.onSubscribe(new EmptySubscription());
            } else {
                upstream.cancel();
            }
            call.cancel();
            downstream.onError(mapFailure(failure));
        }

        @Override
        public synchronized void onSubscribe(Flow.Subscription subscription) {
            upstream = subscription;
            downstream.onSubscribe(new Flow.Subscription() {
                @Override
                public void request(long count) {
                    subscription.request(count);
                }

                @Override
                public void cancel() {
                    cancelDownstream();
                }
            });
        }

        @Override
        public synchronized void onNext(byte[] frame) {
            if (done) {
                return;
            }
            try {
                if (frame.length > maximumBytes) {
                    throw new ClientException(ErrorCode.FRAME_TOO_LARGE);
                }
                OperationResult<R> result = operation.decodeResult(frame, maximumBytes);
                downstream.onNext(result);
                if (done) {
                    return;
                }
                if (result.complete()) {
                    terminal = true;
                    done = true;
                    upstream.cancel();
                    call.cancel();
                    downstream.onComplete();
                }
            } catch (RuntimeException failure) {
                done = true;
                upstream.cancel();
                call.cancel();
                downstream.onError(mapFailure(failure));
            }
        }

        @Override
        public synchronized void onError(Throwable failure) {
            if (!done) {
                done = true;
                call.cancel();
                downstream.onError(mapFailure(failure));
            }
        }

        @Override
        public synchronized void onComplete() {
            if (!done) {
                done = true;
                call.cancel();
                if (terminal) {
                    downstream.onComplete();
                } else {
                    downstream.onError(new ClientException(ErrorCode.STREAM_TRUNCATED));
                }
            }
        }

        private synchronized void cancelDownstream() {
            if (done) {
                return;
            }
            done = true;
            upstream.cancel();
            call.cancel();
        }
    }

    public static final class RedirectPolicy {
        private static final Set<String> SENSITIVE = Set.of(
                "authorization", "cookie", "proxy-authorization", "x-api-key", "x-tenant-id");

        private RedirectPolicy() {}

        public static @NonNull Map<String, String> forwardedHeaders(
                @NonNull Map<String, String> headers,
                @NonNull URI source,
                @NonNull URI destination,
                @NonNull Set<String> credentialOrigins) {
            if (origin(source).equals(origin(destination)) || credentialOrigins.contains(origin(destination))) {
                return Map.copyOf(headers);
            }
            Map<String, String> result = new LinkedHashMap<>();
            headers.forEach((name, value) -> {
                if (!SENSITIVE.contains(name.toLowerCase(Locale.ROOT))) {
                    result.put(name, value);
                }
            });
            return Map.copyOf(result);
        }

        private static String origin(URI uri) {
            int port = uri.getPort();
            String scheme = uri.getScheme().toLowerCase(Locale.ROOT);
            int defaultPort = "https".equals(scheme) ? 443 : "http".equals(scheme) ? 80 : -1;
            String portPart = port < 0 || port == defaultPort ? "" : ":" + port;
            return scheme + "://" + uri.getHost().toLowerCase(Locale.ROOT) + portPart;
        }
    }

    public static final class WireScalars {
        private static final Pattern INTEGER = Pattern.compile("-?[0-9]+");
        private static final Pattern DECIMAL = Pattern.compile("-?[0-9]+(?:\\.[0-9]+)?");
        private static final Pattern TIMESTAMP = Pattern.compile("[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})");
        private static final Pattern UUID_PATTERN = Pattern.compile("[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}");
        private static final BigInteger INT64_MINIMUM = BigInteger.ONE.shiftLeft(63).negate();
        private static final BigInteger INT64_MAXIMUM = BigInteger.ONE.shiftLeft(63).subtract(BigInteger.ONE);
        private static final BigInteger UINT64_MAXIMUM = BigInteger.ONE.shiftLeft(64).subtract(BigInteger.ONE);

        private WireScalars() {}

        public static @NonNull String bigInteger(@NonNull String wire) {
            if (!INTEGER.matcher(wire).matches()) {
                throw scalarFailure();
            }
            return new BigInteger(wire).toString();
        }

        public static @NonNull String int64(@NonNull String wire) {
            BigInteger value = new BigInteger(bigInteger(wire));
            if (value.compareTo(INT64_MINIMUM) < 0 || value.compareTo(INT64_MAXIMUM) > 0) throw scalarFailure();
            return value.toString();
        }

        public static @NonNull String uint64(@NonNull String wire) {
            BigInteger value = new BigInteger(bigInteger(wire));
            if (value.signum() < 0 || value.compareTo(UINT64_MAXIMUM) > 0) throw scalarFailure();
            return value.toString();
        }

        public static @NonNull BigInteger asBigInteger(@NonNull String wire) {
            String canonical = bigInteger(wire);
            if (!canonical.equals(wire)) {
                throw scalarFailure();
            }
            return new BigInteger(canonical);
        }

        public static @NonNull String fromBigInteger(@NonNull BigInteger value) {
            return value.toString();
        }

        public static @NonNull String decimal(@NonNull String wire) {
            if (!DECIMAL.matcher(wire).matches()) {
                throw scalarFailure();
            }
            BigDecimal value = new BigDecimal(wire).stripTrailingZeros();
            return value.signum() == 0 ? "0" : value.toPlainString();
        }

        public static @NonNull BigDecimal asBigDecimal(@NonNull String wire) {
            String canonical = decimal(wire);
            BigDecimal value = new BigDecimal(canonical);
            if (!decimal(value.toPlainString()).equals(canonical)) {
                throw scalarFailure();
            }
            return value;
        }

        public static @NonNull String fromBigDecimal(@NonNull BigDecimal value) {
            return decimal(value.toPlainString());
        }

        public static @NonNull String timestamp(@NonNull String wire) {
            if (!TIMESTAMP.matcher(wire).matches()) {
                throw scalarFailure();
            }
            try {
                Instant instant = OffsetDateTime.parse(wire, DateTimeFormatter.ISO_OFFSET_DATE_TIME).toInstant();
                OffsetDateTime utc = instant.atOffset(ZoneOffset.UTC);
                if (utc.getYear() < 0 || utc.getYear() > 9999) throw scalarFailure();
                String base = DateTimeFormatter.ofPattern("uuuu-MM-dd'T'HH:mm:ss", Locale.ROOT).format(utc);
                if (instant.getNano() == 0) {
                    return base + "Z";
                }
                String fraction = String.format(Locale.ROOT, "%09d", instant.getNano()).replaceFirst("0+$", "");
                return base + "." + fraction + "Z";
            } catch (DateTimeParseException failure) {
                throw scalarFailure();
            }
        }

        public static @NonNull Instant asInstant(@NonNull String wire) {
            return Instant.parse(timestamp(wire));
        }

        public static @NonNull String fromInstant(@NonNull Instant value) {
            return timestamp(value.toString());
        }

        public static @NonNull String duration(@NonNull String wire) {
            return int64(wire);
        }

        public static @NonNull Duration asDuration(@NonNull String wire) {
            BigInteger nanos = new BigInteger(duration(wire));
            BigInteger[] parts = nanos.divideAndRemainder(BigInteger.valueOf(1_000_000_000L));
            return Duration.ofSeconds(parts[0].longValueExact(), parts[1].longValueExact());
        }

        public static @NonNull String fromDuration(@NonNull Duration value) {
            String wire = BigInteger.valueOf(value.getSeconds())
                    .multiply(BigInteger.valueOf(1_000_000_000L))
                    .add(BigInteger.valueOf(value.getNano()))
                    .toString();
            return duration(wire);
        }

        public static @NonNull String uuid(@NonNull String wire) {
            if (!UUID_PATTERN.matcher(wire).matches()) {
                throw scalarFailure();
            }
            return UUID.fromString(wire).toString();
        }

        public static @NonNull UUID asUuid(@NonNull String wire) {
            return UUID.fromString(uuid(wire));
        }

        public static @NonNull String fromUuid(@NonNull UUID value) {
            return value.toString();
        }

        public static byte @NonNull [] bytes(@NonNull String wire) {
            if (wire.indexOf('=') >= 0 || wire.length() % 4 == 1) {
                throw scalarFailure();
            }
            try {
                byte[] decoded = Base64.getUrlDecoder().decode(wire);
                if (!Base64.getUrlEncoder().withoutPadding().encodeToString(decoded).equals(wire)) {
                    throw scalarFailure();
                }
                return decoded;
            } catch (IllegalArgumentException failure) {
                throw scalarFailure();
            }
        }

        public static @NonNull String fromBytes(byte @NonNull [] value) {
            return Base64.getUrlEncoder().withoutPadding().encodeToString(value);
        }

        private static ClientException scalarFailure() {
            return new ClientException(ErrorCode.SCALAR_INVALID);
        }
    }

    public static final class Json {
        private static final int MAX_DEPTH = 128;

        private Json() {}

        public static @NonNull JsonValue parse(byte @NonNull [] input) {
            return parse(input, DEFAULT_RESPONSE_BYTES);
        }

        private static @NonNull JsonValue parse(byte @NonNull [] input, int maximumBytes) {
            if (input.length > maximumBytes) {
                throw new ClientException(ErrorCode.RESPONSE_TOO_LARGE);
            }
            String text;
            try {
                text = StandardCharsets.UTF_8.newDecoder()
                        .onMalformedInput(CodingErrorAction.REPORT)
                        .onUnmappableCharacter(CodingErrorAction.REPORT)
                        .decode(ByteBuffer.wrap(input))
                        .toString();
            } catch (CharacterCodingException failure) {
                throw new ClientException(ErrorCode.INVALID_RESPONSE, "invalid UTF-8", failure);
            }
            return new Parser(text).parse();
        }

        public static byte @NonNull [] canonicalBytes(@NonNull Object value) {
            StringBuilder output = new StringBuilder();
            write(output, value, 0);
            return output.toString().getBytes(StandardCharsets.UTF_8);
        }

        public static @NonNull JsonObject object(@NonNull JsonValue value) {
            if (value instanceof JsonObject object) {
                return object;
            }
            throw new ClientException(ErrorCode.INVALID_RESPONSE, "expected object");
        }

        public static @NonNull String string(@NonNull JsonValue value) {
            if (value instanceof JsonString string) {
                return string.value();
            }
            throw new ClientException(ErrorCode.INVALID_RESPONSE, "expected string");
        }

        public static <T> @NonNull Selected<T> selected(
                @NonNull JsonObject owner,
                @NonNull String key,
                boolean pendingWhenMissing,
                @NonNull Decoder<T> decoder) {
            if (!owner.values().containsKey(key)) {
                return pendingWhenMissing ? Selected.pending() : Selected.missing();
            }
            JsonValue value = owner.values().get(key);
            return value == JsonNull.INSTANCE ? Selected.nullValue() : Selected.present(decoder.decode(value));
        }

        private static void write(StringBuilder output, Object value, int depth) {
            if (depth > MAX_DEPTH) {
                throw new ClientException(ErrorCode.INVALID_REQUEST, "JSON depth exceeded");
            }
            if (value == null || value == JsonNull.INSTANCE) {
                output.append("null");
            } else if (value instanceof String string) {
                writeString(output, string);
            } else if (value instanceof Boolean bool) {
                output.append(bool);
            } else if (value instanceof JsonValue json) {
                writeJsonValue(output, json, depth);
            } else if (value instanceof Input<?> input) {
                if (input instanceof Input.NullValue<?>) {
                    output.append("null");
                } else if (input instanceof Input.Value<?> present) {
                    write(output, present.value(), depth + 1);
                } else {
                    throw new ClientException(ErrorCode.INVALID_REQUEST, "missing input must be omitted by its owner");
                }
            } else if (value instanceof Map<?, ?> map) {
                writeMap(output, map, depth);
            } else if (value instanceof Iterable<?> iterable) {
                output.append('[');
                boolean first = true;
                for (Object item : iterable) {
                    if (!first) output.append(',');
                    first = false;
                    write(output, item, depth + 1);
                }
                output.append(']');
            } else {
                throw new ClientException(ErrorCode.INVALID_REQUEST, "unsupported JSON value");
            }
        }

        private static void writeJsonValue(StringBuilder output, JsonValue value, int depth) {
            if (value == JsonNull.INSTANCE) output.append("null");
            else if (value instanceof JsonBoolean bool) output.append(bool.value());
            else if (value instanceof JsonNumber number) output.append(canonicalNumber(number.value()));
            else if (value instanceof JsonString string) writeString(output, string.value());
            else if (value instanceof JsonArray array) write(output, array.values(), depth + 1);
            else if (value instanceof JsonObject object) write(output, object.values(), depth + 1);
        }

        private static void writeMap(StringBuilder output, Map<?, ?> map, int depth) {
            List<String> keys = new ArrayList<>(map.size());
            for (Object key : map.keySet()) {
                if (!(key instanceof String string) || !validUnicode(string)) {
                    throw new ClientException(ErrorCode.INVALID_REQUEST, "JSON object key must be Unicode text");
                }
                keys.add(string);
            }
            keys.sort(Comparator.naturalOrder());
            output.append('{');
            for (int index = 0; index < keys.size(); index++) {
                if (index > 0) output.append(',');
                String key = keys.get(index);
                writeString(output, key);
                output.append(':');
                write(output, map.get(key), depth + 1);
            }
            output.append('}');
        }

        private static void writeString(StringBuilder output, String value) {
            if (!validUnicode(value)) {
                throw new ClientException(ErrorCode.INVALID_REQUEST, "invalid Unicode string");
            }
            output.append('"');
            for (int index = 0; index < value.length(); index++) {
                char token = value.charAt(index);
                switch (token) {
                    case '"' -> output.append("\\\"");
                    case '\\' -> output.append("\\\\");
                    case '\b' -> output.append("\\b");
                    case '\f' -> output.append("\\f");
                    case '\n' -> output.append("\\n");
                    case '\r' -> output.append("\\r");
                    case '\t' -> output.append("\\t");
                    default -> {
                        if (token < 0x20) output.append(String.format(Locale.ROOT, "\\u%04x", (int) token));
                        else output.append(token);
                    }
                }
            }
            output.append('"');
        }

        private static String canonicalNumber(String raw) {
            try {
                BigDecimal value = new BigDecimal(raw).stripTrailingZeros();
                return value.signum() == 0 ? "0" : value.toPlainString();
            } catch (NumberFormatException failure) {
                throw new ClientException(ErrorCode.INVALID_RESPONSE, "invalid number", failure);
            }
        }
    }

    private static List<OperationError> decodeErrors(JsonValue value) {
        if (value == null) return List.of();
        if (!(value instanceof JsonArray array)) {
            throw new ClientException(ErrorCode.INVALID_RESPONSE, "errors must be an array");
        }
        List<OperationError> result = new ArrayList<>(array.values().size());
        for (JsonValue item : array.values()) {
            JsonObject error = Json.object(item);
            String code = Json.string(required(error, "code"));
            String message = error.values().containsKey("message") ? Json.string(error.values().get("message")) : null;
            List<Object> path = new ArrayList<>();
            JsonValue pathValue = error.values().get("path");
            if (pathValue instanceof JsonArray components) {
                for (JsonValue component : components.values()) {
                    if (component instanceof JsonString string) path.add(string.value());
                    else if (component instanceof JsonNumber number) path.add(Integer.valueOf(number.value()));
                    else throw new ClientException(ErrorCode.INVALID_RESPONSE, "invalid error path");
                }
            } else if (pathValue != null) {
                throw new ClientException(ErrorCode.INVALID_RESPONSE, "invalid error path");
            }
            Map<String, JsonValue> details = error.values().get("details") instanceof JsonObject object ? object.values() : Map.of();
            result.add(new OperationError(code, message, path, details));
        }
        return List.copyOf(result);
    }

    private static JsonValue required(JsonObject object, String key) {
        JsonValue value = object.values().get(key);
        if (value == null) throw new ClientException(ErrorCode.INVALID_RESPONSE, "missing response member");
        return value;
    }

    private static ClientException mapFailure(Throwable failure) {
        Throwable actual = failure;
        while (actual instanceof CompletionException || actual instanceof ExecutionException) {
            actual = actual.getCause();
        }
        if (actual instanceof ClientException client) return client;
        if (actual instanceof CancellationException) return new ClientException(ErrorCode.CANCELLED);
        return new ClientException(ErrorCode.TRANSPORT, "transport failed", actual);
    }

    private static boolean validUnicode(String value) {
        for (int index = 0; index < value.length(); index++) {
            char token = value.charAt(index);
            if (Character.isHighSurrogate(token)) {
                if (++index >= value.length() || !Character.isLowSurrogate(value.charAt(index))) return false;
            } else if (Character.isLowSurrogate(token)) {
                return false;
            }
        }
        return true;
    }

    private static ScheduledThreadPoolExecutor deadlineExecutor() {
        ScheduledThreadPoolExecutor executor = new ScheduledThreadPoolExecutor(1, runnable -> {
            Thread thread = new Thread(runnable, "naatre-jvm-deadlines");
            thread.setDaemon(true);
            return thread;
        });
        executor.setRemoveOnCancelPolicy(true);
        return executor;
    }

    private static final class Parser {
        private final String input;
        private int offset;

        private Parser(String input) {
            this.input = input;
        }

        private JsonValue parse() {
            skipSpace();
            JsonValue value = value(0);
            skipSpace();
            if (offset != input.length()) fail("trailing JSON data");
            return value;
        }

        private JsonValue value(int depth) {
            if (depth > Json.MAX_DEPTH || offset >= input.length()) fail("invalid JSON value");
            return switch (input.charAt(offset)) {
                case 'n' -> literal("null", JsonNull.INSTANCE);
                case 't' -> literal("true", new JsonBoolean(true));
                case 'f' -> literal("false", new JsonBoolean(false));
                case '"' -> new JsonString(string());
                case '[' -> array(depth + 1);
                case '{' -> object(depth + 1);
                default -> number();
            };
        }

        private JsonValue literal(String expected, JsonValue value) {
            if (!input.startsWith(expected, offset)) fail("invalid JSON literal");
            offset += expected.length();
            return value;
        }

        private JsonArray array(int depth) {
            offset++;
            skipSpace();
            List<JsonValue> values = new ArrayList<>();
            if (take(']')) return new JsonArray(values);
            while (true) {
                skipSpace();
                values.add(value(depth));
                skipSpace();
                if (take(']')) return new JsonArray(values);
                require(',');
            }
        }

        private JsonObject object(int depth) {
            offset++;
            skipSpace();
            Map<String, JsonValue> values = new LinkedHashMap<>();
            if (take('}')) return new JsonObject(values);
            while (true) {
                skipSpace();
                if (offset >= input.length() || input.charAt(offset) != '"') fail("object key must be a string");
                String key = string();
                if (values.containsKey(key)) fail("duplicate object key");
                skipSpace();
                require(':');
                skipSpace();
                values.put(key, value(depth));
                skipSpace();
                if (take('}')) return new JsonObject(values);
                require(',');
            }
        }

        private JsonNumber number() {
            int start = offset;
            if (take('-') && offset >= input.length()) fail("invalid number");
            if (take('0')) {
                if (offset < input.length() && Character.isDigit(input.charAt(offset))) fail("leading zero");
            } else {
                digits();
            }
            if (take('.')) digits();
            if (offset < input.length() && (input.charAt(offset) == 'e' || input.charAt(offset) == 'E')) {
                offset++;
                if (offset < input.length() && (input.charAt(offset) == '+' || input.charAt(offset) == '-')) offset++;
                digits();
            }
            String raw = input.substring(start, offset);
            try {
                new BigDecimal(raw);
            } catch (NumberFormatException failure) {
                fail("invalid number");
            }
            return new JsonNumber(raw);
        }

        private void digits() {
            int start = offset;
            while (offset < input.length() && Character.isDigit(input.charAt(offset))) offset++;
            if (start == offset) fail("expected digits");
        }

        private String string() {
            require('"');
            StringBuilder result = new StringBuilder();
            while (offset < input.length()) {
                char token = input.charAt(offset++);
                if (token == '"') {
                    if (!validUnicode(result.toString())) fail("invalid Unicode string");
                    return result.toString();
                }
                if (token < 0x20) fail("control character in string");
                if (token != '\\') {
                    result.append(token);
                    continue;
                }
                if (offset >= input.length()) fail("invalid escape");
                char escaped = input.charAt(offset++);
                switch (escaped) {
                    case '"', '\\', '/' -> result.append(escaped);
                    case 'b' -> result.append('\b');
                    case 'f' -> result.append('\f');
                    case 'n' -> result.append('\n');
                    case 'r' -> result.append('\r');
                    case 't' -> result.append('\t');
                    case 'u' -> result.append(unicodeEscape());
                    default -> fail("invalid escape");
                }
            }
            fail("unterminated string");
            return "";
        }

        private char unicodeEscape() {
            if (offset + 4 > input.length()) fail("invalid Unicode escape");
            int value = 0;
            for (int count = 0; count < 4; count++) {
                int digit = Character.digit(input.charAt(offset++), 16);
                if (digit < 0) fail("invalid Unicode escape");
                value = value * 16 + digit;
            }
            return (char) value;
        }

        private void skipSpace() {
            while (offset < input.length() && " \t\r\n".indexOf(input.charAt(offset)) >= 0) offset++;
        }

        private boolean take(char expected) {
            if (offset < input.length() && input.charAt(offset) == expected) {
                offset++;
                return true;
            }
            return false;
        }

        private void require(char expected) {
            if (!take(expected)) fail("expected " + expected);
        }

        private static void fail(String message) {
            throw new ClientException(ErrorCode.INVALID_RESPONSE, message);
        }
    }
}
