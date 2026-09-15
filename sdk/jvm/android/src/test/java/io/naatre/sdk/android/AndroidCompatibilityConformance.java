package io.naatre.sdk.android;

import io.naatre.sdk.ClientCore.Call;
import io.naatre.sdk.ClientCore.Client;
import io.naatre.sdk.ClientCore.ClientException;
import io.naatre.sdk.ClientCore.ErrorCode;
import io.naatre.sdk.ClientCore.Input;
import io.naatre.sdk.ClientCore.JsonNumber;
import io.naatre.sdk.ClientCore.OpenVariant;
import io.naatre.sdk.ClientCore.Selected;
import io.naatre.sdk.ClientCore.Transport;
import io.naatre.sdk.ClientCore.TransportRequest;
import io.naatre.sdk.ClientCore.TransportResponse;
import io.naatre.sdk.ClientCore.WireScalars;
import io.naatre.sdk.generated.java.GetAccount;
import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.time.Instant;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.atomic.AtomicInteger;

/** API-26 compatibility probe compiled into the Android DEX evidence artifact. */
public final class AndroidCompatibilityConformance {
    private AndroidCompatibilityConformance() {}

    public static void main(String[] arguments) {
        precisionAndUnknownVariantsRemainLossless();
        nullabilityAndPresenceRemainDistinct();
        cancellationReleasesTheTransport();
        resourceLimitsRemainFailClosed();
        System.out.println("{\"profile\":\"sdk.jvm.android-compat-1\",\"status\":\"passed\"}");
    }

    private static void precisionAndUnknownVariantsRemainLossless() {
        String integer = "340282366920938463463374607431768211455";
        require(WireScalars.asBigInteger(integer).equals(new BigInteger(integer)), "BigInteger precision changed");
        require(WireScalars.fromBigInteger(WireScalars.asBigInteger(integer)).equals(integer), "BigInteger wire changed");
        String instant = "2026-09-15T01:02:03.123456789Z";
        require(WireScalars.fromInstant(Instant.parse(instant)).equals(instant), "Instant precision changed");
        GetAccount.Status unknown = GetAccount.Status.fromWire("FUTURE");
        require(!unknown.known() && unknown.wireValue().equals("FUTURE"), "unknown enum was erased");
        OpenVariant variant = new OpenVariant("Future", new JsonNumber("7"));
        require(variant.discriminator().equals("Future"), "unknown variant was erased");
    }

    private static void nullabilityAndPresenceRemainDistinct() {
        GetAccount.Variables missing = new GetAccount.Variables("acct-1");
        GetAccount.Variables explicitNull =
                new GetAccount.Variables("acct-1", Input.nullValue(), Input.missing(), Input.missing());
        String missingWire = new String(GetAccount.operation().canonicalRequest(missing), StandardCharsets.UTF_8);
        String nullWire = new String(GetAccount.operation().canonicalRequest(explicitNull), StandardCharsets.UTF_8);
        require(!missingWire.contains("nickname"), "missing input was serialized");
        require(nullWire.contains("\"nickname\":null"), "explicit null was erased");

        var result = GetAccount.operation().decodeResult(
                "{\"complete\":false,\"data\":{\"profile\":{\"display\":\"Ada\",\"nickname\":null}}}"
                        .getBytes(StandardCharsets.UTF_8));
        GetAccount.Result data = ((Selected.Present<GetAccount.Result>) result.data()).value();
        GetAccount.ResultProfile profile = ((Selected.Present<GetAccount.ResultProfile>) data.profile()).value();
        require(profile.nickname() instanceof Selected.NullValue<?>, "selected null was erased");
        require(data.later() instanceof Selected.Pending<?>, "selected pending state was erased");
    }

    private static void cancellationReleasesTheTransport() {
        SuspendedTransport transport = new SuspendedTransport();
        CompletableFuture<?> future = new Client(transport).executeAsync(
                GetAccount.operation(), new GetAccount.Variables("acct-1"), Duration.ofSeconds(30));
        future.cancel(true);
        require(future.isCancelled(), "future did not become cancelled");
        require(transport.cancellations.get() == 1, "transport cancellation was not invoked exactly once");
    }

    private static void resourceLimitsRemainFailClosed() {
        byte[] response = "{\"complete\":true,\"data\":null}".getBytes(StandardCharsets.UTF_8);
        Transport oversized = request -> completed(new TransportResponse(response, 33));
        try {
            new Client(oversized, Map::of, 32, 1024, 1024)
                    .executeBlocking(GetAccount.operation(), new GetAccount.Variables("acct-1"), null);
            fail("oversized response succeeded");
        } catch (ClientException failure) {
            require(failure.code() == ErrorCode.RESPONSE_TOO_LARGE, "oversized response code differs");
        }
    }

    private static Call<TransportResponse> completed(TransportResponse response) {
        return new Call<>() {
            private final CompletableFuture<TransportResponse> future = CompletableFuture.completedFuture(response);

            @Override
            public CompletableFuture<TransportResponse> future() {
                return future;
            }

            @Override
            public void cancel() {
                // Already complete.
            }
        };
    }

    private static void require(boolean condition, String message) {
        if (!condition) fail(message);
    }

    private static void fail(String message) {
        throw new AssertionError(message);
    }

    private static final class SuspendedTransport implements Transport {
        private final AtomicInteger cancellations = new AtomicInteger();

        @Override
        public Call<TransportResponse> execute(TransportRequest request) {
            return new Call<>() {
                private final CompletableFuture<TransportResponse> future = new CompletableFuture<>();

                @Override
                public CompletableFuture<TransportResponse> future() {
                    return future;
                }

                @Override
                public void cancel() {
                    cancellations.incrementAndGet();
                    future.completeExceptionally(new ClientException(ErrorCode.CANCELLED));
                }
            };
        }
    }
}
