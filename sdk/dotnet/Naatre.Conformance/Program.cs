using System.Globalization;
using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using Valksor.Naatre;
using Valksor.Naatre.Generated;

var root = FindRoot();
var vectors = new List<string>();

RunScalarVectors(root);
vectors.Add("shared-lossless-scalars");
RunPresenceVectors(root);
vectors.Add("missing-null-partial-errors-unknown-variants");
await RunUnaryCancellation();
vectors.Add("unary-active-cancellation");
await RunStreamingVectors();
vectors.Add("sse-terminal-truncation-and-cancellation");
await RunRedirectAndRetryVectors();
vectors.Add("redirect-credentials-and-idempotent-retry");
RunMalformedAndLimitVectors();
vectors.Add("malformed-and-bounded-responses");

Console.WriteLine(JsonSerializer.Serialize(new
{
    profile = "sdk.dotnet.core-1",
    status = "passed",
    runtime = System.Runtime.InteropServices.RuntimeInformation.FrameworkDescription,
    platform = System.Runtime.InteropServices.RuntimeInformation.RuntimeIdentifier,
    vectors,
}));
return;

static string FindRoot()
{
    var current = new DirectoryInfo(Environment.CurrentDirectory);
    while (current is not null && !File.Exists(Path.Combine(current.FullName, "go.mod")))
    {
        current = current.Parent;
    }

    return current?.FullName ?? throw new InvalidOperationException("repository root not found");
}

static void RunScalarVectors(string root)
{
    using var document = JsonDocument.Parse(File.ReadAllBytes(Path.Combine(root, "conformance/v1/scalars.json")));
    var previousCulture = CultureInfo.CurrentCulture;
    var previousUICulture = CultureInfo.CurrentUICulture;
    try
    {
        CultureInfo.CurrentCulture = CultureInfo.GetCultureInfo("tr-TR");
        CultureInfo.CurrentUICulture = CultureInfo.GetCultureInfo("fr-FR");
        foreach (var vector in document.RootElement.GetProperty("vectors").EnumerateArray())
        {
            var kind = vector.GetProperty("kind").GetString()!;
            if (kind is not ("Int64" or "UInt64" or "BigInt" or "Decimal" or "Timestamp" or "Duration" or "UUID" or "Bytes"))
            {
                continue;
            }

            var valid = vector.GetProperty("valid").GetBoolean();
            try
            {
                var input = vector.GetProperty("input").GetString()!;
                using var inputDocument = JsonDocument.Parse(input);
                if (inputDocument.RootElement.ValueKind != JsonValueKind.String)
                {
                    throw new NaatreClientException(ClientErrorCodes.ScalarInvalid);
                }

                var raw = inputDocument.RootElement.GetString()!;
                object value = kind switch
                {
                    "Int64" => Int64Value.Parse(raw),
                    "UInt64" => UInt64Value.Parse(raw),
                    "BigInt" => BigIntegerValue.Parse(raw),
                    "Decimal" => DecimalValue.Parse(raw),
                    "Timestamp" => TimestampValue.Parse(raw),
                    "Duration" => DurationValue.Parse(raw),
                    "UUID" => UuidValue.Parse(raw),
                    "Bytes" => BytesValue.Parse(raw),
                    _ => throw new InvalidOperationException(),
                };
                Check(valid, $"{vector.GetProperty("name").GetString()} unexpectedly accepted");
                var actual = JsonSerializer.Serialize(value, value.GetType(), NaatreJson.Options);
                Check(actual == vector.GetProperty("canonical").GetString(), $"{vector.GetProperty("name").GetString()} canonical mismatch");
            }
            catch (Exception exception) when (exception is NaatreClientException or JsonException)
            {
                Check(!valid, $"{vector.GetProperty("name").GetString()} unexpectedly rejected");
            }
        }

    Check(!TimestampValue.Parse("0000-02-29T12:34:56Z").TryGetDateTimeOffset(out _), "year zero must stay in the lossless wrapper");
    Check(!TimestampValue.Parse("2026-09-11T20:30:01.123456789Z").TryGetDateTimeOffset(out _), "nanoseconds must not truncate into DateTimeOffset");
    Check(!DurationValue.Parse("1").TryGetTimeSpan(out _), "nanoseconds must not truncate into TimeSpan");
    Check(TimestampValue.FromDateTimeOffset(new DateTimeOffset(2026, 9, 11, 23, 30, 1, TimeSpan.FromHours(3))).Raw == "2026-09-11T20:30:01Z", "local time zone affected timestamp output");
    }
    finally
    {
        CultureInfo.CurrentCulture = previousCulture;
        CultureInfo.CurrentUICulture = previousUICulture;
    }
}

static void RunPresenceVectors(string root)
{
    var missing = new GetAccountVariables { Id = "acct-1" };
    var missingJson = JsonSerializer.Serialize(missing, NaatreJson.Options);
    Check(!missingJson.Contains("nickname", StringComparison.Ordinal), "missing property was serialized");
    var explicitNull = new GetAccountVariables { Id = "acct-1", Nickname = Presence<string?>.Null };
    var nullJson = JsonSerializer.Serialize(explicitNull, NaatreJson.Options);
    Check(nullJson.Contains("\"nickname\":null", StringComparison.Ordinal), "explicit null was omitted");

    var operation = GetAccount.Create(new GetAccountVariables
    {
        Id = "acct-1",
        Nickname = Presence<string?>.Null,
        Tags = Presence<IReadOnlyList<string>>.Present(Array.Empty<string>()),
        Filter = Presence<IReadOnlyDictionary<string, string>>.Present(new Dictionary<string, string>()),
    });
    using var reference = JsonDocument.Parse(File.ReadAllBytes(Path.Combine(root, "conformance/v1/generator-output.json")));
    var expected = CanonicalJson.Canonicalize(Encoding.UTF8.GetBytes(reference.RootElement.GetProperty("operations")[0].GetProperty("request").GetRawText()));
    Check(operation.CanonicalRequest.Span.SequenceEqual(expected), "canonical persisted request mismatch");
    Check(operation.Persisted.Digest == "cc863005080edcb85ec0345a50593dc58111896bbbe7ff86475ef2412fc5cb90", "persisted hash mismatch");

    var partial = operation.DecodeResult(Encoding.UTF8.GetBytes("{\"complete\":false,\"data\":{\"profile\":{\"display\":\"Ada\",\"nickname\":null}},\"errors\":[{\"code\":\"PARTIAL\",\"path\":[\"later\"],\"retryable\":false}]}"));
    var partialData = partial.Data ?? throw new InvalidOperationException("partial data was lost");
    Check(partialData.Profile.State == PresenceState.Present, "partial data was lost");
    Check(partialData.Profile.Value.Nickname.State == PresenceState.Null, "null collapsed");
    Check(partialData.Later.State == PresenceState.Failed, "path error did not become a failed state");
    Check(partial.Errors.Count == 1 && !partial.Complete, "simultaneous error/completion state was lost");
    ExpectCode(ClientErrorCodes.ResultInvalid, () => partial.RequireComplete());

    var unknown = Status.Parse("FUTURE_STATUS");
    Check(!unknown.Value.IsKnown && unknown.Value.Raw == "FUTURE_STATUS", "unknown enum value was lost");
    Check(!Status.Parse("0").Value.IsKnown, "numeric enum spelling was treated as a known variant");
    using var unionDocument = JsonDocument.Parse("{\"kind\":\"FutureAccount\",\"value\":7}");
    var unknownUnion = new OpenUnion { Discriminator = "FutureAccount", Value = unionDocument.RootElement.Clone(), IsKnown = false };
    Check(!unknownUnion.IsKnown && unknownUnion.Discriminator == "FutureAccount" && unknownUnion.Value.GetProperty("value").GetInt32() == 7, "unknown union value was lost");
}

static async Task RunUnaryCancellation()
{
    var handler = new BlockingHandler();
    using var client = new NaatreHttpClient(new Uri("https://example.test/v1"), handler);
    using var cancellation = new CancellationTokenSource();
    var operation = GetAccount.Create(new GetAccountVariables { Id = "acct-1" });
    var pending = client.ExecuteAsync(operation, cancellation.Token);
    await handler.Started.Task.WaitAsync(TimeSpan.FromSeconds(5));
    cancellation.Cancel();
    await ExpectCancelled(pending);
    Check(handler.CancellationObserved, "unary handler did not observe cancellation");
}

static async Task RunStreamingVectors()
{
    var completeSse = "data: {\"type\":\"data\",\"sequence\":1,\"result\":{\"complete\":true,\"data\":{\"profile\":{\"display\":\"Ada\"}},\"errors\":[]}}\n\ndata: {\"type\":\"complete\",\"sequence\":2}\n\n";
    using (var client = ClientForContent(completeSse, "text/event-stream"))
    {
        var events = new List<StreamEvent<GetAccountResult>>();
        await foreach (var item in client.StreamAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" })))
        {
            events.Add(item);
        }

        Check(events.Count == 2 && events[^1].Terminal, "terminal SSE sequence failed");
    }

    var errorThenComplete = "data: {\"type\":\"error\",\"sequence\":1,\"final\":false}\n\ndata: {\"type\":\"complete\",\"sequence\":2}\n\n";
    using (var client = ClientForContent(errorThenComplete, "text/event-stream"))
    {
        var events = new List<StreamEvent<GetAccountResult>>();
        await foreach (var item in client.StreamAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" })))
        {
            events.Add(item);
        }

        Check(events.Count == 2 && !events[0].Terminal && events[1].Terminal, "non-final stream error terminated delivery");
    }

    using (var client = ClientForContent("data: {\"type\":\"history-unavailable\",\"sequence\":1}\n\n", "text/event-stream"))
    {
        var events = new List<StreamEvent<GetAccountResult>>();
        await foreach (var item in client.StreamAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" })))
        {
            events.Add(item);
        }

        Check(events.Count == 1 && !events[0].Terminal && events[0].EndsAttempt, "history-unavailable was treated as logical completion");
    }

    using (var client = ClientForContent("data: {\"type\":\"data\",\"sequence\":2}\n\n", "text/event-stream"))
    {
        await ExpectCodeAsync(ClientErrorCodes.ProtocolInvalid, async () =>
        {
            await foreach (var _ in client.StreamAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" })))
            {
            }
        });
    }

    using (var client = ClientForContent("data: {\"type\":\"data\",\"sequence\":1}\n\n", "text/event-stream"))
    {
        await ExpectCodeAsync(ClientErrorCodes.StreamTruncated, async () =>
        {
            await foreach (var _ in client.StreamAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" })))
            {
            }
        });
    }

    var handler = new BlockingHandler();
    using var cancellingClient = new NaatreHttpClient(new Uri("https://example.test/v1"), handler);
    using var cancellation = new CancellationTokenSource();
    var enumerator = cancellingClient.StreamAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" }), cancellation.Token).GetAsyncEnumerator();
    var pending = enumerator.MoveNextAsync().AsTask();
    await handler.Started.Task.WaitAsync(TimeSpan.FromSeconds(5));
    cancellation.Cancel();
    await ExpectCancelled(pending);
    await enumerator.DisposeAsync();
    Check(handler.CancellationObserved, "stream handler did not observe cancellation");
}

static async Task RunRedirectAndRetryVectors()
{
    var redirect = new SequenceHandler(
        request => new HttpResponseMessage(HttpStatusCode.TemporaryRedirect) { Headers = { Location = new Uri("https://redirect.test/v1") } },
        request =>
        {
            Check(request.Headers.Authorization is null, "credential crossed origin");
            return JsonResponse("{\"complete\":true,\"data\":{\"profile\":{\"display\":\"Ada\"}},\"errors\":[]}");
        });
    var options = new NaatreHttpOptions
    {
        RedirectOrigins = new HashSet<string>(StringComparer.Ordinal) { "https://redirect.test" },
        AuthenticateAsync = (request, _) =>
        {
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", "fixture-secret");
            return ValueTask.CompletedTask;
        },
    };
    using (var client = new NaatreHttpClient(new Uri("https://example.test/v1"), redirect, options))
    {
        await client.ExecuteAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" }));
    }

    var mutationHandler = new SequenceHandler(
        _ => new HttpResponseMessage(HttpStatusCode.ServiceUnavailable),
        _ => JsonResponse("{\"complete\":true,\"data\":{},\"errors\":[]}"));
    var mutation = new NaatreOperation<GetAccountResult>(
        "WriteAccount",
        "mutation",
        new PersistedReference { Digest = new string('a', 64) },
        new { },
        GetAccount.Decode);
    using (var client = new NaatreHttpClient(new Uri("https://example.test/v1"), mutationHandler, new NaatreHttpOptions { MaximumQueryAttempts = 3 }))
    {
        await ExpectCodeAsync(ClientErrorCodes.Transport, () => client.ExecuteAsync(mutation));
        Check(mutationHandler.Calls == 1, "mutation was replayed");
    }
}

static void RunMalformedAndLimitVectors()
{
    using (var client = ClientForContent("not-json", "application/json"))
    {
        ExpectCodeAsync(ClientErrorCodes.ProtocolInvalid, () => client.ExecuteAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" }))).GetAwaiter().GetResult();
    }

    using (var client = ClientForContent("{\"complete\":true,\"complete\":false,\"data\":{}}", "application/json"))
    {
        ExpectCodeAsync(ClientErrorCodes.ProtocolInvalid, () => client.ExecuteAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" }))).GetAwaiter().GetResult();
    }

    using (var client = ClientForContent("data: 12345678901234567\n\n", "text/event-stream", new NaatreHttpOptions { MaximumFrameBytes = 16 }))
    {
        ExpectCodeAsync(ClientErrorCodes.FrameTooLarge, async () =>
        {
            await foreach (var _ in client.StreamAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" })))
            {
            }
        }).GetAwaiter().GetResult();
    }

    var handler = new SequenceHandler(_ =>
    {
        var response = JsonResponse("{}");
        response.Content.Headers.ContentLength = 1025;
        return response;
    });
    using var bounded = new NaatreHttpClient(new Uri("https://example.test/v1"), handler, new NaatreHttpOptions { MaximumResponseBytes = 1024 });
    ExpectCodeAsync(ClientErrorCodes.ResponseTooLarge, () => bounded.ExecuteAsync(GetAccount.Create(new GetAccountVariables { Id = "acct-1" }))).GetAwaiter().GetResult();
}

static NaatreHttpClient ClientForContent(string content, string mediaType, NaatreHttpOptions? options = null) => new(
    new Uri("https://example.test/v1"),
    new SequenceHandler(_ =>
    {
        var response = new HttpResponseMessage(HttpStatusCode.OK) { Content = new StringContent(content, Encoding.UTF8) };
        response.Content.Headers.ContentType = new MediaTypeHeaderValue(mediaType) { CharSet = "utf-8" };
        return response;
    }),
    options);

static HttpResponseMessage JsonResponse(string json)
{
    var response = new HttpResponseMessage(HttpStatusCode.OK) { Content = new StringContent(json, Encoding.UTF8) };
    response.Content.Headers.ContentType = new MediaTypeHeaderValue("application/json") { CharSet = "utf-8" };
    return response;
}

static async Task ExpectCancelled(Task task)
{
    try
    {
        await task;
        throw new InvalidOperationException("expected cancellation");
    }
    catch (NaatreOperationCanceledException exception)
    {
        Check(exception.Code == ClientErrorCodes.Cancelled, "cancellation code mismatch");
    }
}

static void ExpectCode(string code, Action action)
{
    try
    {
        action();
        throw new InvalidOperationException($"expected {code}");
    }
    catch (NaatreClientException exception)
    {
        Check(exception.Code == code, $"expected {code}, got {exception.Code}");
    }
}

static async Task ExpectCodeAsync(string code, Func<Task> action)
{
    try
    {
        await action();
        throw new InvalidOperationException($"expected {code}");
    }
    catch (NaatreClientException exception)
    {
        Check(exception.Code == code, $"expected {code}, got {exception.Code}");
    }
}

static void Check(bool condition, string message)
{
    if (!condition)
    {
        throw new InvalidOperationException(message);
    }
}

internal sealed class BlockingHandler : HttpMessageHandler
{
    internal TaskCompletionSource Started { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);

    internal bool CancellationObserved { get; private set; }

    protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        Started.TrySetResult();
        try
        {
            await Task.Delay(Timeout.InfiniteTimeSpan, cancellationToken);
            throw new InvalidOperationException("unreachable");
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            CancellationObserved = true;
            throw;
        }
    }
}

internal sealed class SequenceHandler(params Func<HttpRequestMessage, HttpResponseMessage>[] responses) : HttpMessageHandler
{
    private int index;

    internal int Calls => index;

    protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        var current = Interlocked.Increment(ref index) - 1;
        if (current >= responses.Length)
        {
            throw new InvalidOperationException("unexpected request");
        }

        return Task.FromResult(responses[current](request));
    }
}
