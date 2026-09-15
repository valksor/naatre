package io.naatre.sdk.http;

import io.naatre.sdk.ClientCore.ClientException;
import io.naatre.sdk.ClientCore.Client;
import io.naatre.sdk.ClientCore.ErrorCode;
import io.naatre.sdk.ClientCore.TransportRequest;
import io.naatre.sdk.ClientCore.TransportResponse;
import io.naatre.sdk.generated.java.GetAccount;
import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpHeaders;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayDeque;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.Queue;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionException;
import java.util.concurrent.atomic.AtomicBoolean;

/** Offline conformance probe for the server-only java.net.http adapter. */
public final class JavaHttpTransportConformance {
    private static final URI ENDPOINT = URI.create("https://api.example.test/naatre");

    private JavaHttpTransportConformance() {}

    public static void main(String[] arguments) {
        successfulPostPreservesCanonicalBytes();
        redirectsStripCredentialsAndReleaseBodies();
        unsafeRedirectsFailClosed();
        responseBoundariesAreEnforcedBeforeReads();
        invalidMediaAndEncodingFailClosed();
        cancellationAndCloseStopActiveExchange();
        failuresAreStableAndRedacted();
        authenticationFailuresAreStableAndRedacted();
        unsupportedCapabilitiesRemainExplicit();
        System.out.println("{\"profile\":\"sdk.jvm.java-http-1\",\"status\":\"passed\"}");
    }

    private static void successfulPostPreservesCanonicalBytes() {
        byte[] responseBody = "{\"complete\":true,\"data\":null}".getBytes(StandardCharsets.UTF_8);
        RecordingExchange exchange = new RecordingExchange(response(200, responseBody, Map.of(
                "content-type", List.of("application/json; charset=utf-8"),
                "content-length", List.of(Integer.toString(responseBody.length)))));
        JavaHttpTransport transport = transport(exchange, Set.of(), Set.of(), 0);
        byte[] requestBody = "{\"operation\":\"GetAccount\"}".getBytes(StandardCharsets.UTF_8);
        TransportResponse response = transport.execute(request(requestBody, 1024, 1024)).future().join();

        require(response.compressedBytes() == responseBody.length, "compressed byte count differs");
        require(java.util.Arrays.equals(response.body(), responseBody), "response body differs");
        HttpRequest sent = exchange.requests().get(0);
        require("POST".equals(sent.method()), "request method differs");
        require(ENDPOINT.equals(sent.uri()), "request endpoint differs");
        require("application/json".equals(sent.headers().firstValue("Accept").orElse(null)), "accept header differs");
        require("identity".equals(sent.headers().firstValue("Accept-Encoding").orElse(null)), "encoding was not bounded");
        require(
                "application/json; charset=utf-8".equals(sent.headers().firstValue("Content-Type").orElse(null)),
                "content type differs");
        require(java.util.Arrays.equals(body(sent), requestBody), "canonical request bytes changed");
    }

    private static void redirectsStripCredentialsAndReleaseBodies() {
        AtomicBoolean redirectClosed = new AtomicBoolean();
        URI destination = URI.create("https://edge.example.test/naatre");
        Queue<CompletableFuture<HttpResponse<InputStream>>> responses = new ArrayDeque<>();
        responses.add(CompletableFuture.completedFuture(response(
                307,
                new CloseTrackingInputStream(new byte[0], redirectClosed),
                Map.of("location", List.of(destination.toString())))));
        responses.add(CompletableFuture.completedFuture(response(
                200,
                "{\"complete\":true,\"data\":null}".getBytes(StandardCharsets.UTF_8),
                Map.of("content-type", List.of("application/json")))));
        RecordingExchange exchange = new RecordingExchange(responses);
        JavaHttpTransport transport = transport(exchange, Set.of("https://edge.example.test"), Set.of(), 1);
        transport.execute(request("{}".getBytes(StandardCharsets.UTF_8), 1024, 1024)).future().join();

        require(redirectClosed.get(), "redirect body was not closed");
        require(exchange.requests().size() == 2, "redirect was not followed exactly once");
        HttpHeaders redirected = exchange.requests().get(1).headers();
        require(redirected.firstValue("Authorization").isEmpty(), "authorization crossed origins");
        require(redirected.firstValue("Cookie").isEmpty(), "cookie crossed origins");
        require("fixture".equals(redirected.firstValue("X-Trace").orElse(null)), "non-secret metadata was stripped");
    }

    private static void unsafeRedirectsFailClosed() {
        AtomicBoolean redirectClosed = new AtomicBoolean();
        RecordingExchange exchange = new RecordingExchange(response(
                308,
                new CloseTrackingInputStream(new byte[0], redirectClosed),
                Map.of("location", List.of("https://untrusted.example.test/naatre"))));
        JavaHttpTransport transport = transport(exchange, Set.of(), Set.of(), 5);
        expectCode(ErrorCode.TRANSPORT, () -> transport.execute(request(new byte[0], 32, 32)).future().join());
        require(redirectClosed.get(), "rejected redirect body was not closed");
        require(exchange.requests().size() == 1, "rejected redirect was sent");
    }

    private static void responseBoundariesAreEnforcedBeforeReads() {
        byte[] exact = new byte[32];
        RecordingExchange exactExchange = new RecordingExchange(response(
                200,
                exact,
                Map.of("content-type", List.of("application/json"), "content-length", List.of("32"))));
        TransportResponse accepted = transport(exactExchange, Set.of(), Set.of(), 0)
                .execute(request(new byte[0], 32, 32))
                .future()
                .join();
        require(accepted.body().length == 32, "exact response boundary was rejected");

        AtomicBoolean read = new AtomicBoolean();
        InputStream unread = new ByteArrayInputStream(new byte[0]) {
            @Override
            public int read(byte[] buffer, int offset, int length) {
                read.set(true);
                return -1;
            }
        };
        RecordingExchange declaredLarge = new RecordingExchange(response(
                200,
                unread,
                Map.of("content-type", List.of("application/json"), "content-length", List.of("33"))));
        expectCode(
                ErrorCode.RESPONSE_TOO_LARGE,
                () -> transport(declaredLarge, Set.of(), Set.of(), 0)
                        .execute(request(new byte[0], 32, 32))
                        .future()
                        .join());
        require(!read.get(), "oversized declared response was read");

        RecordingExchange streamedLarge = new RecordingExchange(response(
                200,
                new byte[33],
                Map.of("content-type", List.of("application/json"))));
        expectCode(
                ErrorCode.RESPONSE_TOO_LARGE,
                () -> transport(streamedLarge, Set.of(), Set.of(), 0)
                        .execute(request(new byte[0], 32, 32))
                        .future()
                        .join());
    }

    private static void cancellationAndCloseStopActiveExchange() {
        CompletableFuture<HttpResponse<InputStream>> pending = new CompletableFuture<>();
        RecordingExchange exchange = new RecordingExchange(pending);
        JavaHttpTransport transport = transport(exchange, Set.of(), Set.of(), 0);
        var call = transport.execute(request(new byte[0], 32, 32));
        call.cancel();
        expectCode(ErrorCode.CANCELLED, () -> call.future().join());
        require(pending.isCancelled(), "call cancellation did not cancel java.net.http work");

        CompletableFuture<HttpResponse<InputStream>> pendingClose = new CompletableFuture<>();
        JavaHttpTransport closing = transport(new RecordingExchange(pendingClose), Set.of(), Set.of(), 0);
        var closingCall = closing.execute(request(new byte[0], 32, 32));
        closing.close();
        expectCode(ErrorCode.CANCELLED, () -> closingCall.future().join());
        require(pendingClose.isCancelled(), "transport close did not cancel active work");
        expectCode(ErrorCode.TRANSPORT, () -> closing.execute(request(new byte[0], 32, 32)));
    }

    private static void invalidMediaAndEncodingFailClosed() {
        RecordingExchange media = new RecordingExchange(response(
                200,
                new byte[0],
                Map.of("content-type", List.of("text/plain"))));
        expectCode(
                ErrorCode.INVALID_RESPONSE,
                () -> transport(media, Set.of(), Set.of(), 0)
                        .execute(request(new byte[0], 32, 32))
                        .future()
                        .join());

        RecordingExchange compressed = new RecordingExchange(response(
                200,
                new byte[0],
                Map.of("content-type", List.of("application/json"), "content-encoding", List.of("gzip"))));
        expectCode(
                ErrorCode.INVALID_RESPONSE,
                () -> transport(compressed, Set.of(), Set.of(), 0)
                        .execute(request(new byte[0], 32, 32))
                        .future()
                        .join());
    }

    private static void failuresAreStableAndRedacted() {
        String secret = "Bearer local-credential";
        RecordingExchange exchange = new RecordingExchange(
                CompletableFuture.failedFuture(new IllegalStateException(secret + " at /implementation/path")));
        try {
            transport(exchange, Set.of(), Set.of(), 0)
                    .execute(request(new byte[0], 32, 32))
                    .future()
                    .join();
            fail("implementation failure succeeded");
        } catch (CompletionException failure) {
            ClientException publicFailure = clientFailure(failure);
            require(publicFailure.code() == ErrorCode.TRANSPORT, "implementation failure code differs");
            require("TRANSPORT".equals(publicFailure.getMessage()), "implementation detail escaped in message");
            require(publicFailure.getCause() == null, "implementation detail escaped as a cause");
            require(!publicFailure.toString().contains(secret), "credential escaped in failure");
        }

        expectCode(
                ErrorCode.INVALID_REQUEST,
                () -> new JavaHttpTransport(
                        URI.create("https://user:secret@example.test/naatre"),
                        HttpClient.newBuilder().followRedirects(HttpClient.Redirect.NEVER).build()));
        expectCode(ErrorCode.INVALID_REQUEST, () -> new JavaHttpTransport(ENDPOINT, (HttpClient) null));
        expectCode(
                ErrorCode.INVALID_REQUEST,
                () -> new JavaHttpTransport(
                        ENDPOINT,
                        HttpClient.newBuilder().followRedirects(HttpClient.Redirect.NEVER).build(),
                        null,
                        Set.of(),
                        0));
        expectCode(ErrorCode.INVALID_REQUEST, () -> transport(exchange, Set.of(), Set.of(), 0).execute(null));
    }

    private static void unsupportedCapabilitiesRemainExplicit() {
        JavaHttpTransport transport = transport(
                new RecordingExchange(CompletableFuture.failedFuture(new AssertionError("must not send"))),
                Set.of(),
                Set.of(),
                0);
        expectCode(ErrorCode.UNSUPPORTED_CAPABILITY, () -> transport.sse(request(new byte[0], 32, 32)));
        expectCode(ErrorCode.UNSUPPORTED_CAPABILITY, () -> transport.webSocket(request(new byte[0], 32, 32)));
    }

    private static void authenticationFailuresAreStableAndRedacted() {
        String secret = "Bearer authentication-secret";
        JavaHttpTransport transport = transport(
                new RecordingExchange(CompletableFuture.failedFuture(new AssertionError("must not send"))),
                Set.of(),
                Set.of(),
                0);
        CompletableFuture<?> result = new Client(
                        transport,
                        () -> {
                            throw new IllegalStateException(secret + " at /implementation/path");
                        },
                        32,
                        32,
                        32)
                .executeAsync(GetAccount.operation(), new GetAccount.Variables("acct-1"), null);
        try {
            result.join();
            fail("authentication failure succeeded");
        } catch (CompletionException failure) {
            ClientException publicFailure = clientFailure(failure);
            require(publicFailure.code() == ErrorCode.AUTHENTICATION_FAILED, "authentication failure code differs");
            require("AUTHENTICATION_FAILED".equals(publicFailure.getMessage()), "authentication detail escaped");
            require(publicFailure.getCause() == null, "authentication cause escaped");
            require(!publicFailure.toString().contains(secret), "authentication credential escaped");
        }
    }

    private static JavaHttpTransport transport(
            RecordingExchange exchange,
            Set<String> redirectOrigins,
            Set<String> credentialOrigins,
            int maximumRedirects) {
        return new JavaHttpTransport(ENDPOINT, exchange, redirectOrigins, credentialOrigins, maximumRedirects);
    }

    private static TransportRequest request(byte[] body, int compressedBytes, int decompressedBytes) {
        return new TransportRequest(
                UUID.fromString("123e4567-e89b-12d3-a456-426614174000"),
                body,
                Map.of(
                        "Authorization", "Bearer fixture-secret",
                        "Cookie", "session=fixture-secret",
                        "X-Trace", "fixture"),
                "query",
                compressedBytes,
                decompressedBytes,
                1024);
    }

    private static HttpResponse<InputStream> response(int status, byte[] body, Map<String, List<String>> headers) {
        return response(status, new ByteArrayInputStream(body), headers);
    }

    private static HttpResponse<InputStream> response(
            int status, InputStream body, Map<String, List<String>> headers) {
        return new FakeResponse(status, body, HttpHeaders.of(headers, (name, value) -> true));
    }

    private static byte[] body(HttpRequest request) {
        HttpRequest.BodyPublisher publisher = request.bodyPublisher().orElseThrow();
        CompletableFuture<byte[]> bytes = new CompletableFuture<>();
        publisher.subscribe(new java.util.concurrent.Flow.Subscriber<>() {
            private final java.io.ByteArrayOutputStream output = new java.io.ByteArrayOutputStream();

            @Override
            public void onSubscribe(java.util.concurrent.Flow.Subscription subscription) {
                subscription.request(Long.MAX_VALUE);
            }

            @Override
            public void onNext(java.nio.ByteBuffer item) {
                byte[] chunk = new byte[item.remaining()];
                item.get(chunk);
                output.writeBytes(chunk);
            }

            @Override
            public void onError(Throwable failure) {
                bytes.completeExceptionally(failure);
            }

            @Override
            public void onComplete() {
                bytes.complete(output.toByteArray());
            }
        });
        return bytes.join();
    }

    private static void expectCode(ErrorCode expected, Runnable action) {
        try {
            action.run();
            fail("expected " + expected);
        } catch (RuntimeException failure) {
            ClientException publicFailure = clientFailure(failure);
            require(publicFailure.code() == expected, "got " + publicFailure.code() + ", expected " + expected);
        }
    }

    private static ClientException clientFailure(Throwable failure) {
        Throwable actual = failure;
        while (actual instanceof CompletionException && actual.getCause() != null) {
            actual = actual.getCause();
        }
        if (actual instanceof ClientException publicFailure) return publicFailure;
        throw new AssertionError("expected ClientException, got " + actual.getClass().getName());
    }

    private static void require(boolean condition, String message) {
        if (!condition) fail(message);
    }

    private static void fail(String message) {
        throw new AssertionError(message);
    }

    private static final class RecordingExchange implements JavaHttpTransport.Exchange {
        private final Queue<CompletableFuture<HttpResponse<InputStream>>> responses;
        private final java.util.ArrayList<HttpRequest> requests = new java.util.ArrayList<>();

        private RecordingExchange(HttpResponse<InputStream> response) {
            this(CompletableFuture.completedFuture(response));
        }

        private RecordingExchange(CompletableFuture<HttpResponse<InputStream>> response) {
            this.responses = new ArrayDeque<>();
            this.responses.add(response);
        }

        private RecordingExchange(Queue<CompletableFuture<HttpResponse<InputStream>>> responses) {
            this.responses = responses;
        }

        @Override
        public CompletableFuture<HttpResponse<InputStream>> send(HttpRequest request) {
            requests.add(request);
            CompletableFuture<HttpResponse<InputStream>> response = responses.poll();
            if (response == null) throw new AssertionError("unexpected exchange");
            return response;
        }

        private List<HttpRequest> requests() {
            return List.copyOf(requests);
        }
    }

    private record FakeResponse(int statusCode, InputStream body, HttpHeaders headers)
            implements HttpResponse<InputStream> {
        @Override
        public HttpRequest request() {
            return HttpRequest.newBuilder(ENDPOINT).build();
        }

        @Override
        public Optional<HttpResponse<InputStream>> previousResponse() {
            return Optional.empty();
        }

        @Override
        public URI uri() {
            return ENDPOINT;
        }

        @Override
        public HttpClient.Version version() {
            return HttpClient.Version.HTTP_2;
        }

        @Override
        public Optional<javax.net.ssl.SSLSession> sslSession() {
            return Optional.empty();
        }
    }

    private static final class CloseTrackingInputStream extends ByteArrayInputStream {
        private final AtomicBoolean closed;

        private CloseTrackingInputStream(byte[] buffer, AtomicBoolean closed) {
            super(buffer);
            this.closed = closed;
        }

        @Override
        public void close() throws IOException {
            closed.set(true);
            super.close();
        }
    }
}
