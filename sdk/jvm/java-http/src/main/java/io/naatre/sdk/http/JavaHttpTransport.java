package io.naatre.sdk.http;

import io.naatre.sdk.ClientCore;
import io.naatre.sdk.ClientCore.Call;
import io.naatre.sdk.ClientCore.ClientException;
import io.naatre.sdk.ClientCore.ErrorCode;
import io.naatre.sdk.ClientCore.Transport;
import io.naatre.sdk.ClientCore.TransportRequest;
import io.naatre.sdk.ClientCore.TransportResponse;
import io.naatre.sdk.annotations.NonNull;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicReference;

/**
 * Server-JVM unary HTTP adapter backed by {@link java.net.http.HttpClient}.
 *
 * <p>The adapter deliberately handles redirects itself, requests identity encoding, reads response bodies
 * incrementally, and exposes only stable {@link ErrorCode} failures. Authenticated POST-SSE and WebSocket
 * remain separate optional capabilities and are not advertised by this class.
 */
public final class JavaHttpTransport implements Transport, AutoCloseable {
    private static final Set<String> RESERVED_HEADERS = Set.of(
            "accept", "accept-encoding", "content-length", "content-type", "host");

    private final URI endpoint;
    private final Exchange exchange;
    private final Set<String> redirectOrigins;
    private final Set<String> credentialOrigins;
    private final int maximumRedirects;
    private final Set<HttpCall> active = ConcurrentHashMap.newKeySet();
    private final AtomicBoolean closed = new AtomicBoolean();

    /** Creates a secure adapter with no cross-origin redirects. */
    public JavaHttpTransport(@NonNull URI endpoint) {
        this(endpoint, defaultClient());
    }

    /** Creates an adapter using a caller-owned client configured with redirects disabled. */
    public JavaHttpTransport(@NonNull URI endpoint, @NonNull HttpClient client) {
        this(endpoint, client, Set.of(), Set.of(), ClientCore.DEFAULT_REDIRECTS);
    }

    /**
     * Creates an adapter with explicit cross-origin redirect and credential allowlists.
     *
     * @param redirectOrigins canonical origins that may receive a redirected request
     * @param credentialOrigins canonical origins that may retain protected request headers
     */
    public JavaHttpTransport(
            @NonNull URI endpoint,
            @NonNull HttpClient client,
            @NonNull Set<String> redirectOrigins,
            @NonNull Set<String> credentialOrigins,
            int maximumRedirects) {
        this(
                endpoint,
                exchange(requireManualRedirects(client)),
                redirectOrigins,
                credentialOrigins,
                maximumRedirects);
    }

    JavaHttpTransport(
            URI endpoint,
            Exchange exchange,
            Set<String> redirectOrigins,
            Set<String> credentialOrigins,
            int maximumRedirects) {
        this.endpoint = validateEndpoint(endpoint);
        this.exchange = Objects.requireNonNull(exchange, "exchange");
        this.redirectOrigins = validateOrigins(redirectOrigins);
        this.credentialOrigins = validateOrigins(credentialOrigins);
        if (maximumRedirects < 0 || maximumRedirects > ClientCore.DEFAULT_REDIRECTS) {
            throw failure(ErrorCode.INVALID_REQUEST);
        }
        this.maximumRedirects = maximumRedirects;
    }

    @Override
    public @NonNull Call<TransportResponse> execute(@NonNull TransportRequest request) {
        if (request == null) throw failure(ErrorCode.INVALID_REQUEST);
        if (closed.get()) throw failure(ErrorCode.TRANSPORT);
        HttpCall call = new HttpCall(request);
        active.add(call);
        call.future().whenComplete((ignored, ignoredFailure) -> active.remove(call));
        if (closed.get()) {
            call.cancel();
        } else {
            call.start();
        }
        return call;
    }

    /** Cancels every active exchange. A closed adapter cannot be reused. */
    @Override
    public void close() {
        if (!closed.compareAndSet(false, true)) return;
        List.copyOf(active).forEach(HttpCall::cancel);
    }

    private final class HttpCall implements Call<TransportResponse> {
        private final TransportRequest transportRequest;
        private final CompletableFuture<TransportResponse> result = new CompletableFuture<>();
        private final AtomicReference<CompletableFuture<?>> work = new AtomicReference<>();
        private final AtomicReference<InputStream> body = new AtomicReference<>();
        private final AtomicBoolean cancelled = new AtomicBoolean();

        private HttpCall(TransportRequest transportRequest) {
            this.transportRequest = transportRequest;
        }

        private void start() {
            follow(endpoint, transportRequest.headers(), 0);
        }

        private void follow(URI current, Map<String, String> headers, int redirects) {
            if (cancelled.get()) return;
            HttpRequest request;
            try {
                request = buildRequest(current, headers, transportRequest.body());
            } catch (RuntimeException invalid) {
                fail(ErrorCode.INVALID_REQUEST);
                return;
            }

            CompletableFuture<HttpResponse<InputStream>> sent;
            try {
                sent = Objects.requireNonNull(exchange.send(request), "exchange result");
            } catch (RuntimeException setupFailure) {
                fail(ErrorCode.TRANSPORT);
                return;
            }
            install(sent);
            sent.whenComplete((response, sendFailure) -> {
                if (cancelled.get()) {
                    close(response == null ? null : response.body());
                    return;
                }
                if (sendFailure != null || response == null || response.body() == null) {
                    close(response == null ? null : response.body());
                    fail(ErrorCode.TRANSPORT);
                    return;
                }
                if (response.statusCode() == 307 || response.statusCode() == 308) {
                    redirect(current, headers, redirects, response);
                    return;
                }
                if (response.statusCode() < 200 || response.statusCode() >= 300) {
                    close(response.body());
                    fail(ErrorCode.TRANSPORT);
                    return;
                }
                accept(response);
            });
        }

        private void redirect(
                URI current, Map<String, String> headers, int redirects, HttpResponse<InputStream> response) {
            URI destination = null;
            try {
                if (redirects >= maximumRedirects) throw failure(ErrorCode.TRANSPORT);
                String location = response.headers().firstValue("Location").orElseThrow();
                destination = validateEndpoint(current.resolve(location));
                String destinationOrigin = origin(destination);
                if (!origin(current).equals(destinationOrigin) && !redirectOrigins.contains(destinationOrigin)) {
                    throw failure(ErrorCode.TRANSPORT);
                }
            } catch (RuntimeException invalidRedirect) {
                close(response.body());
                fail(ErrorCode.TRANSPORT);
                return;
            }
            close(response.body());
            Map<String, String> redirected = ClientCore.RedirectPolicy.forwardedHeaders(
                    headers, current, destination, credentialOrigins);
            follow(destination, redirected, redirects + 1);
        }

        private void accept(HttpResponse<InputStream> response) {
            try {
                requireJson(response);
                requireIdentityEncoding(response);
                long declaredLength = response.headers().firstValueAsLong("Content-Length").orElse(-1L);
                if (declaredLength > transportRequest.compressedBytes()
                        || declaredLength > transportRequest.decompressedBytes()) {
                    close(response.body());
                    fail(ErrorCode.RESPONSE_TOO_LARGE);
                    return;
                }
            } catch (RuntimeException invalidResponse) {
                close(response.body());
                fail(invalidResponse instanceof ClientException client ? client.code() : ErrorCode.INVALID_RESPONSE);
                return;
            }

            InputStream input = response.body();
            body.set(input);
            if (cancelled.get()) {
                close(body.getAndSet(null));
                return;
            }
            CompletableFuture<Void> reading = CompletableFuture.runAsync(() -> read(input));
            install(reading);
        }

        private void read(InputStream input) {
            int maximum = Math.min(transportRequest.compressedBytes(), transportRequest.decompressedBytes());
            try (input; ByteArrayOutputStream output = new ByteArrayOutputStream(Math.min(maximum, 8192))) {
                byte[] buffer = new byte[8192];
                int total = 0;
                while (true) {
                    if (cancelled.get()) return;
                    int count = input.read(buffer);
                    if (count < 0) break;
                    if (count == 0) continue;
                    if (count > maximum - total) {
                        fail(ErrorCode.RESPONSE_TOO_LARGE);
                        return;
                    }
                    output.write(buffer, 0, count);
                    total += count;
                }
                result.complete(new TransportResponse(output.toByteArray(), total));
            } catch (IOException readFailure) {
                if (!cancelled.get()) fail(ErrorCode.TRANSPORT);
            } finally {
                body.compareAndSet(input, null);
            }
        }

        private void install(CompletableFuture<?> next) {
            work.set(next);
            if (cancelled.get()) next.cancel(true);
        }

        private void fail(ErrorCode code) {
            result.completeExceptionally(failure(code));
        }

        @Override
        public @NonNull CompletableFuture<TransportResponse> future() {
            return result;
        }

        @Override
        public void cancel() {
            if (!cancelled.compareAndSet(false, true)) return;
            CompletableFuture<?> pending = work.get();
            if (pending != null) pending.cancel(true);
            close(body.getAndSet(null));
            fail(ErrorCode.CANCELLED);
        }
    }

    private static HttpRequest buildRequest(URI endpoint, Map<String, String> headers, byte[] body) {
        HttpRequest.Builder builder = HttpRequest.newBuilder(endpoint)
                .header("Accept", "application/json")
                .header("Accept-Encoding", "identity")
                .header("Content-Type", "application/json; charset=utf-8")
                .POST(HttpRequest.BodyPublishers.ofByteArray(body));
        for (Map.Entry<String, String> header : headers.entrySet()) {
            String name = Objects.requireNonNull(header.getKey(), "header name");
            String value = Objects.requireNonNull(header.getValue(), "header value");
            if (RESERVED_HEADERS.contains(name.toLowerCase(Locale.ROOT))) {
                throw failure(ErrorCode.INVALID_REQUEST);
            }
            builder.header(name, value);
        }
        return builder.build();
    }

    private static void requireJson(HttpResponse<?> response) {
        String contentType = response.headers().firstValue("Content-Type").orElse("");
        String[] sections = contentType.split(";", -1);
        if (sections.length == 0 || !"application/json".equalsIgnoreCase(sections[0].trim())) {
            throw failure(ErrorCode.INVALID_RESPONSE);
        }
        boolean charset = false;
        for (int index = 1; index < sections.length; index++) {
            String parameter = sections[index].trim();
            if (!parameter.equalsIgnoreCase("charset=utf-8") || charset) {
                throw failure(ErrorCode.INVALID_RESPONSE);
            }
            charset = true;
        }
    }

    private static void requireIdentityEncoding(HttpResponse<?> response) {
        String encoding = response.headers().firstValue("Content-Encoding").orElse("identity");
        if (!"identity".equalsIgnoreCase(encoding.trim())) {
            throw failure(ErrorCode.INVALID_RESPONSE);
        }
    }

    private static HttpClient defaultClient() {
        return HttpClient.newBuilder().followRedirects(HttpClient.Redirect.NEVER).build();
    }

    private static Exchange exchange(HttpClient client) {
        Objects.requireNonNull(client, "client");
        return request -> client.sendAsync(request, HttpResponse.BodyHandlers.ofInputStream());
    }

    private static HttpClient requireManualRedirects(HttpClient client) {
        if (client == null || client.followRedirects() != HttpClient.Redirect.NEVER) {
            throw failure(ErrorCode.INVALID_REQUEST);
        }
        return client;
    }

    private static URI validateEndpoint(URI endpoint) {
        if (endpoint == null
                || !endpoint.isAbsolute()
                || !("http".equalsIgnoreCase(endpoint.getScheme()) || "https".equalsIgnoreCase(endpoint.getScheme()))
                || endpoint.getHost() == null
                || endpoint.getUserInfo() != null
                || endpoint.getFragment() != null) {
            throw failure(ErrorCode.INVALID_REQUEST);
        }
        return endpoint.normalize();
    }

    private static Set<String> validateOrigins(Set<String> values) {
        if (values == null) throw failure(ErrorCode.INVALID_REQUEST);
        List<String> normalized = new ArrayList<>(values.size());
        for (String value : values) {
            URI parsed;
            try {
                parsed = validateEndpoint(URI.create(Objects.requireNonNull(value, "origin")));
            } catch (RuntimeException invalid) {
                throw failure(ErrorCode.INVALID_REQUEST);
            }
            String origin = origin(parsed);
            if (!origin.equals(value) || !parsed.getPath().isEmpty() || parsed.getQuery() != null) {
                throw failure(ErrorCode.INVALID_REQUEST);
            }
            normalized.add(origin);
        }
        return Set.copyOf(normalized);
    }

    private static String origin(URI value) {
        String scheme = value.getScheme().toLowerCase(Locale.ROOT);
        int port = value.getPort();
        int defaultPort = "https".equals(scheme) ? 443 : 80;
        return scheme + "://" + value.getHost().toLowerCase(Locale.ROOT)
                + (port < 0 || port == defaultPort ? "" : ":" + port);
    }

    private static void close(InputStream stream) {
        if (stream == null) return;
        try {
            stream.close();
        } catch (IOException ignored) {
            // The public outcome is already determined; close diagnostics are intentionally not exposed.
        }
    }

    private static ClientException failure(ErrorCode code) {
        return new ClientException(code);
    }

    @FunctionalInterface
    interface Exchange {
        CompletableFuture<HttpResponse<InputStream>> send(HttpRequest request);
    }
}
