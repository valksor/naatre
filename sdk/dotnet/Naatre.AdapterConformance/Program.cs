using System.Globalization;
using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using Microsoft.Extensions.DependencyInjection;
using Valksor.Naatre;
using Valksor.Naatre.Client;
using Valksor.Naatre.Generated;

var vectors = new List<string>();

await RunCanonicalAndPositive();
vectors.Add("shared-canonical-codec-and-positive-unary");
vectors.Add("culture-independent-wire-output");
await RunNegativeAndSanitization();
vectors.Add("stable-public-errors-without-protected-details");
await RunBoundaries();
vectors.Add("response-and-configuration-boundaries");
await RunCancellation();
vectors.Add("active-cancellation");
await RunDisposal();
vectors.Add("owned-and-borrowed-handler-lifecycle");
await RunDependencyInjection();
vectors.Add("aspnet-httpclientfactory-dependency-injection");

Console.WriteLine(JsonSerializer.Serialize(new
{
    profile = "sdk.dotnet.adapters-1",
    status = "passed",
    runtime = System.Runtime.InteropServices.RuntimeInformation.FrameworkDescription,
    platform = System.Runtime.InteropServices.RuntimeInformation.RuntimeIdentifier,
    vectors,
}));
return;

static NaatreOperation<GetAccountResult> Operation() =>
    GetAccount.Create(new GetAccountVariables { Id = "acct-1", Nickname = Presence<string?>.Null });

static async Task RunCanonicalAndPositive()
{
    var operation = Operation();
    var recanonicalized = CanonicalJson.Canonicalize(operation.CanonicalRequest.Span);
    Check(operation.CanonicalRequest.Span.SequenceEqual(recanonicalized), "adapter changed canonical request bytes");

    var previousCulture = CultureInfo.CurrentCulture;
    var previousUICulture = CultureInfo.CurrentUICulture;
    try
    {
        CultureInfo.CurrentCulture = CultureInfo.GetCultureInfo("tr-TR");
        CultureInfo.CurrentUICulture = CultureInfo.GetCultureInfo("fr-FR");
        Check(Operation().CanonicalRequest.Span.SequenceEqual(operation.CanonicalRequest.Span), "culture changed canonical request bytes");
    }
    finally
    {
        CultureInfo.CurrentCulture = previousCulture;
        CultureInfo.CurrentUICulture = previousUICulture;
    }

    using var client = ClientFor(JsonResponse(TestData.SuccessBody));
    var result = await client.ExecuteAsync(operation);
    Check(result.RequireComplete().Profile.Value.Nickname.State == PresenceState.Null, "null state collapsed");
}

static async Task RunNegativeAndSanitization()
{
    using (var client = new NaatreClient(
        new Uri("https://example.test/v1"),
        new TestHandler((_, _) => throw new InvalidOperationException("Bearer fixture-secret protected-metadata"))))
    {
        var failure = await CaptureFailure(() => client.ExecuteAsync(Operation()));
        Check(failure.Code == ClientErrorCodes.Transport, "unexpected handler failure code");
        Check(failure.Message == failure.Code && failure.InnerException is null, "handler implementation detail was exposed");
        Check(!failure.ToString().Contains("fixture-secret", StringComparison.Ordinal), "credential was exposed");
        Check(!failure.ToString().Contains("protected-metadata", StringComparison.Ordinal), "protected metadata was exposed");
    }

    using (var client = ClientFor(JsonResponse("not-json")))
    {
        var failure = await CaptureFailure(() => client.ExecuteAsync(Operation()));
        Check(failure.Code == ClientErrorCodes.ProtocolInvalid, "malformed response code changed");
        Check(failure.InnerException is null && failure.Message == failure.Code, "parser detail was exposed");
    }

    using (var client = new NaatreClient(
        new Uri("https://example.test/v1"),
        new NaatreHttpOptions
        {
            AuthenticateAsync = (_, _) => throw new InvalidOperationException("Bearer auth-secret protected-auth-state"),
        }))
    {
        var failure = await CaptureFailure(() => client.ExecuteAsync(Operation()));
        Check(failure.Code == ClientErrorCodes.Transport, "authentication failure code changed");
        Check(failure.InnerException is null && failure.Message == failure.Code, "authentication detail was exposed");
        Check(!failure.ToString().Contains("auth-secret", StringComparison.Ordinal), "authentication credential was exposed");
    }
}

static async Task RunBoundaries()
{
    var exactBytes = Encoding.UTF8.GetByteCount(TestData.SuccessBody);
    using (var exact = ClientFor(JsonResponse(TestData.SuccessBody), new NaatreHttpOptions { MaximumResponseBytes = exactBytes }))
    {
        Check((await exact.ExecuteAsync(Operation())).Complete, "exact response boundary was rejected");
    }

    using (var over = ClientFor(JsonResponse(TestData.SuccessBody), new NaatreHttpOptions { MaximumResponseBytes = exactBytes - 1 }))
    {
        var failure = await CaptureFailure(() => over.ExecuteAsync(Operation()));
        Check(failure.Code == ClientErrorCodes.ResponseTooLarge, "oversized response code changed");
    }

    var redirectOrigins = new HashSet<string>(StringComparer.Ordinal) { "https://allowed.test" };
    var handler = CreateSequenceHandler(
        _ => new HttpResponseMessage(HttpStatusCode.TemporaryRedirect) { Headers = { Location = new Uri("https://later.test/v1") } });
    using var snapshot = new NaatreClient(
        new Uri("https://example.test/v1"),
        handler,
        new NaatreHttpOptions { RedirectOrigins = redirectOrigins });
    redirectOrigins.Add("https://later.test");
    var snapshotFailure = await CaptureFailure(() => snapshot.ExecuteAsync(Operation()));
    Check(snapshotFailure.Code == ClientErrorCodes.Transport, "mutable options changed the active policy");

    using var frame = ClientFor(
        SseResponse("data: 12345678901234567\n\n"),
        new NaatreHttpOptions { MaximumFrameBytes = 16 });
    var frameFailure = await CaptureFailure(async () =>
    {
        await foreach (var _ in frame.StreamAsync(Operation()))
        {
        }
    });
    Check(frameFailure.Code == ClientErrorCodes.FrameTooLarge, "oversized frame code changed");
}

static async Task RunCancellation()
{
    var state = new BlockingState();
    var handler = CreateBlockingHandler(state);
    using var client = new NaatreClient(new Uri("https://example.test/v1"), handler);
    using var cancellation = new CancellationTokenSource();
    var pending = client.ExecuteAsync(Operation(), cancellation.Token);
    await state.Started.Task.WaitAsync(TimeSpan.FromSeconds(5));
    cancellation.Cancel();
    try
    {
        await pending;
        throw new InvalidOperationException("expected cancellation");
    }
    catch (NaatreAdapterCanceledException exception)
    {
        Check(exception.Code == ClientErrorCodes.Cancelled, "cancellation code changed");
        Check(exception.CancellationToken == cancellation.Token, "cancellation token was lost");
        Check(exception.InnerException is null, "cancellation implementation detail was exposed");
    }
    Check(state.CancellationObserved, "transport did not observe cancellation");

    var streamState = new BlockingState();
    var streamHandler = CreateBlockingHandler(streamState);
    using var streamClient = new NaatreClient(new Uri("https://example.test/v1"), streamHandler);
    using var streamCancellation = new CancellationTokenSource();
    var enumerator = streamClient.StreamAsync(Operation(), streamCancellation.Token).GetAsyncEnumerator();
    var move = enumerator.MoveNextAsync().AsTask();
    await streamState.Started.Task.WaitAsync(TimeSpan.FromSeconds(5));
    streamCancellation.Cancel();
    try
    {
        await move;
        throw new InvalidOperationException("expected stream cancellation");
    }
    catch (NaatreAdapterCanceledException exception)
    {
        Check(exception.Code == ClientErrorCodes.Cancelled, "stream cancellation code changed");
        Check(exception.CancellationToken == streamCancellation.Token, "stream cancellation token was lost");
        Check(exception.InnerException is null, "stream cancellation detail was exposed");
    }
    await enumerator.DisposeAsync();
    Check(streamState.CancellationObserved, "stream transport did not observe cancellation");
}

static async Task RunDisposal()
{
    var owned = new TrackingDelegatingHandler();
    var ownedClient = new NaatreClient(new Uri("https://example.test/v1"), handlers: new[] { owned });
    ownedClient.Dispose();
    Check(owned.Disposed, "owned handler was not disposed");
    var disposed = await CaptureFailure(() => ownedClient.ExecuteAsync(Operation()));
    Check(disposed.Code == ClientErrorCodes.ConfigInvalid, "disposed client failure code changed");

    var borrowedDisposed = false;
    var borrowed = new TestHandler(
        (_, cancellationToken) =>
        {
            cancellationToken.ThrowIfCancellationRequested();
            return Task.FromResult(JsonResponse(TestData.SuccessBody));
        },
        () => borrowedDisposed = true);
    var borrowedClient = new NaatreClient(new Uri("https://example.test/v1"), borrowed);
    borrowedClient.Dispose();
    Check(!borrowedDisposed, "borrowed handler was disposed");
    borrowed.Dispose();

    var throwing = new ThrowingDisposeDelegatingHandler("Bearer dispose-secret protected-disposal-state");
    var throwingClient = new NaatreClient(new Uri("https://example.test/v1"), handlers: new[] { throwing });
    try
    {
        throwingClient.Dispose();
        throw new InvalidOperationException("expected disposal failure");
    }
    catch (NaatreAdapterException exception)
    {
        Check(exception.Code == ClientErrorCodes.Transport, "disposal failure code changed");
        Check(exception.InnerException is null && exception.Message == exception.Code, "disposal detail was exposed");
        Check(!exception.ToString().Contains("dispose-secret", StringComparison.Ordinal), "disposal credential was exposed");
    }
}

static async Task RunDependencyInjection()
{
    var handlerDisposed = false;
    var handler = new TestHandler(
        (_, cancellationToken) =>
        {
            cancellationToken.ThrowIfCancellationRequested();
            return Task.FromResult(JsonResponse(TestData.SuccessBody));
        },
        () => handlerDisposed = true);
    var services = new ServiceCollection();
    services
        .AddNaatreClient(new Uri("https://example.test/v1"))
        .ConfigurePrimaryHttpMessageHandler(() => handler);
    using var provider = services.BuildServiceProvider(new ServiceProviderOptions
    {
        ValidateOnBuild = true,
        ValidateScopes = true,
    });
    var client = provider.GetRequiredService<INaatreClient>();
    Check((await client.ExecuteAsync(Operation())).Complete, "DI client did not execute");
    client.Dispose();
    Check(!handlerDisposed, "HttpClientFactory handler ownership was violated");
}

static NaatreClient ClientFor(HttpResponseMessage response, NaatreHttpOptions? options = null) =>
    new(new Uri("https://example.test/v1"), CreateSequenceHandler(_ => response), options);

static HttpMessageHandler CreateSequenceHandler(params Func<HttpRequestMessage, HttpResponseMessage>[] responses)
{
    var index = 0;
    return new TestHandler((request, cancellationToken) =>
    {
        cancellationToken.ThrowIfCancellationRequested();
        var current = Interlocked.Increment(ref index) - 1;
        if (current >= responses.Length)
        {
            throw new InvalidOperationException("unexpected request");
        }

        return Task.FromResult(responses[current](request));
    });
}

static HttpMessageHandler CreateBlockingHandler(BlockingState state) => new TestHandler(async (_, cancellationToken) =>
{
    state.Started.TrySetResult();
    try
    {
        await Task.Delay(Timeout.InfiniteTimeSpan, cancellationToken);
        throw new InvalidOperationException("unreachable");
    }
    catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
    {
        state.CancellationObserved = true;
        throw;
    }
});

static async Task<NaatreAdapterException> CaptureFailure(Func<Task> action)
{
    try
    {
        await action();
        throw new InvalidOperationException("expected adapter failure");
    }
    catch (NaatreAdapterException exception)
    {
        return exception;
    }
}

static HttpResponseMessage JsonResponse(string body)
{
    var response = new HttpResponseMessage(HttpStatusCode.OK) { Content = new StringContent(body, Encoding.UTF8) };
    response.Content.Headers.ContentType = new MediaTypeHeaderValue("application/json") { CharSet = "utf-8" };
    return response;
}

static HttpResponseMessage SseResponse(string body)
{
    var response = new HttpResponseMessage(HttpStatusCode.OK) { Content = new StringContent(body, Encoding.UTF8) };
    response.Content.Headers.ContentType = new MediaTypeHeaderValue("text/event-stream") { CharSet = "utf-8" };
    return response;
}

static void Check(bool condition, string message)
{
    if (!condition)
    {
        throw new InvalidOperationException(message);
    }
}

internal sealed class TestHandler(
    Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> send,
    Action? dispose = null) : HttpMessageHandler
{
    protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        => send(request, cancellationToken);

    protected override void Dispose(bool disposing)
    {
        dispose?.Invoke();
        base.Dispose(disposing);
    }
}

internal sealed class BlockingState
{
    internal TaskCompletionSource Started { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);

    internal bool CancellationObserved { get; set; }
}

internal sealed class TrackingDelegatingHandler : DelegatingHandler
{
    internal bool Disposed { get; private set; }

    protected override void Dispose(bool disposing)
    {
        Disposed = true;
        base.Dispose(disposing);
    }
}

internal sealed class ThrowingDisposeDelegatingHandler(string message) : DelegatingHandler
{
    protected override void Dispose(bool disposing) => throw new InvalidOperationException(message);
}

internal static class TestData
{
    internal const string SuccessBody = "{\"complete\":true,\"data\":{\"profile\":{\"display\":\"Ada\",\"nickname\":null}},\"errors\":[]}";
}
