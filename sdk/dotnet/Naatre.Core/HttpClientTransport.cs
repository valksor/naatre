using System.Net;
using System.Net.Http.Headers;
using System.Runtime.CompilerServices;
using System.Text;
using System.Text.Json;

namespace Valksor.Naatre;

public sealed record NaatreHttpOptions
{
    public int MaximumResponseBytes { get; init; } = 8 * 1024 * 1024;

    public int MaximumFrameBytes { get; init; } = 1024 * 1024;

    public int MaximumRedirects { get; init; } = 5;

    public int MaximumQueryAttempts { get; init; } = 1;

    public IReadOnlySet<string> RedirectOrigins { get; init; } = new HashSet<string>(StringComparer.Ordinal);

    public IReadOnlySet<string> CredentialOrigins { get; init; } = new HashSet<string>(StringComparer.Ordinal);

    public Func<HttpRequestMessage, CancellationToken, ValueTask>? AuthenticateAsync { get; init; }
}

public sealed class NaatreHttpClient : IDisposable
{
    private readonly Uri endpoint;
    private readonly HttpClient client;
    private readonly NaatreHttpOptions options;

    public NaatreHttpClient(Uri endpoint, NaatreHttpOptions? options = null, IEnumerable<DelegatingHandler>? handlers = null)
    {
        this.endpoint = ValidateEndpoint(endpoint);
        this.options = ValidateOptions(options ?? new NaatreHttpOptions());
        HttpMessageHandler pipeline = new HttpClientHandler { AllowAutoRedirect = false, AutomaticDecompression = DecompressionMethods.None };
        foreach (var handler in (handlers ?? Array.Empty<DelegatingHandler>()).Reverse())
        {
            if (handler.InnerHandler is not null)
            {
                throw new NaatreClientException(ClientErrorCodes.ConfigInvalid);
            }

            handler.InnerHandler = pipeline;
            pipeline = handler;
        }

        client = new HttpClient(pipeline, disposeHandler: true);
    }

    public NaatreHttpClient(Uri endpoint, HttpMessageHandler handler, NaatreHttpOptions? options = null)
    {
        this.endpoint = ValidateEndpoint(endpoint);
        this.options = ValidateOptions(options ?? new NaatreHttpOptions());
        ArgumentNullException.ThrowIfNull(handler);
        client = new HttpClient(handler, disposeHandler: false);
    }

    public async Task<OperationResult<TData>> ExecuteAsync<TData>(NaatreOperation<TData> operation, CancellationToken cancellationToken = default)
    {
        ArgumentNullException.ThrowIfNull(operation);
        var attempts = operation.Kind == "query" ? options.MaximumQueryAttempts : 1;
        for (var attempt = 1; ; attempt++)
        {
            try
            {
                using var response = await SendAsync(operation, "application/json", cancellationToken).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    if (attempt < attempts && IsRetryable(response.StatusCode))
                    {
                        continue;
                    }

                    throw new NaatreClientException(ClientErrorCodes.Transport);
                }

                RequireMediaType(response, "application/json");
                var body = await ReadBoundedAsync(response, options.MaximumResponseBytes, cancellationToken).ConfigureAwait(false);
                return operation.DecodeResult(body);
            }
            catch (OperationCanceledException exception) when (cancellationToken.IsCancellationRequested)
            {
                throw new NaatreOperationCanceledException(cancellationToken, exception);
            }
            catch (HttpRequestException exception) when (attempt >= attempts)
            {
                throw new NaatreClientException(ClientErrorCodes.Transport, exception);
            }
            catch (HttpRequestException) when (attempt < attempts)
            {
            }
        }
    }

    public async IAsyncEnumerable<StreamEvent<TData>> StreamAsync<TData>(
        NaatreOperation<TData> operation,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        ArgumentNullException.ThrowIfNull(operation);
        HttpResponseMessage? response = null;
        try
        {
            try
            {
                response = await SendAsync(operation, "text/event-stream", cancellationToken).ConfigureAwait(false);
            }
            catch (OperationCanceledException exception) when (cancellationToken.IsCancellationRequested)
            {
                throw new NaatreOperationCanceledException(cancellationToken, exception);
            }
            if (!response.IsSuccessStatusCode)
            {
                throw new NaatreClientException(ClientErrorCodes.Transport);
            }

            RequireMediaType(response, "text/event-stream");
            await using var stream = await ReadResponseStreamAsync(response.Content, cancellationToken).ConfigureAwait(false);
            var attemptEnded = false;
            ulong previousSequence = 0;
            await foreach (var frame in ReadSseFramesAsync(stream, options.MaximumFrameBytes, cancellationToken).ConfigureAwait(false))
            {
                var decoded = DecodeStreamEvent(operation, frame);
                if (decoded.Sequence != previousSequence + 1)
                {
                    throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid);
                }

                previousSequence = decoded.Sequence;
                yield return decoded;
                if (decoded.EndsAttempt)
                {
                    attemptEnded = true;
                    break;
                }
            }

            if (!attemptEnded)
            {
                throw new NaatreClientException(ClientErrorCodes.StreamTruncated);
            }
        }
        finally
        {
            response?.Dispose();
        }
    }

    public async IAsyncEnumerable<TPage> PaginateAsync<TPage>(
        Func<string?, NaatreOperation<TPage>> operation,
        Func<TPage, string?> nextCursor,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        ArgumentNullException.ThrowIfNull(operation);
        ArgumentNullException.ThrowIfNull(nextCursor);
        string? cursor = null;
        var seen = new HashSet<string>(StringComparer.Ordinal);
        do
        {
            var result = await ExecuteAsync(operation(cursor), cancellationToken).ConfigureAwait(false);
            var page = result.RequireComplete();
            yield return page;
            cursor = nextCursor(page);
            if (cursor is not null && !seen.Add(cursor))
            {
                throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid);
            }
        }
        while (cursor is not null);
    }

    public void Dispose()
    {
        client.Dispose();
    }

    private async Task<HttpResponseMessage> SendAsync<TData>(NaatreOperation<TData> operation, string accept, CancellationToken cancellationToken)
    {
        var current = endpoint;
        for (var redirect = 0; ; redirect++)
        {
            using var request = new HttpRequestMessage(HttpMethod.Post, current)
            {
                Content = new ByteArrayContent(operation.CanonicalRequest.ToArray()),
            };
            request.Content.Headers.ContentType = new MediaTypeHeaderValue("application/json") { CharSet = "utf-8" };
            request.Headers.Accept.ParseAdd(accept);
            if (options.AuthenticateAsync is not null && (SameOrigin(endpoint, current) || options.CredentialOrigins.Contains(Origin(current))))
            {
                await options.AuthenticateAsync(request, cancellationToken).ConfigureAwait(false);
            }

            var response = await client.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
            if (response.StatusCode is not (HttpStatusCode.TemporaryRedirect or HttpStatusCode.PermanentRedirect))
            {
                return response;
            }

            if (redirect >= options.MaximumRedirects || response.Headers.Location is null)
            {
                response.Dispose();
                throw new NaatreClientException(ClientErrorCodes.Transport);
            }

            var destination = response.Headers.Location.IsAbsoluteUri ? response.Headers.Location : new Uri(current, response.Headers.Location);
            response.Dispose();
            ValidateEndpoint(destination);
            if (!SameOrigin(endpoint, destination) && !options.RedirectOrigins.Contains(Origin(destination)))
            {
                throw new NaatreClientException(ClientErrorCodes.Transport);
            }

            current = destination;
        }
    }

    private static StreamEvent<TData> DecodeStreamEvent<TData>(NaatreOperation<TData> operation, ReadOnlyMemory<byte> frame)
    {
        try
        {
            using var document = JsonDocument.Parse(frame);
            var root = document.RootElement;
            if (root.ValueKind != JsonValueKind.Object || !root.TryGetProperty("type", out var typeElement) ||
                !root.TryGetProperty("sequence", out var sequenceElement) || typeElement.ValueKind != JsonValueKind.String ||
                sequenceElement.ValueKind != JsonValueKind.Number || !sequenceElement.TryGetUInt64(out var sequence) || sequence == 0)
            {
                throw new JsonException();
            }

            var type = typeElement.GetString()!;
            if (type is not ("open" or "data" or "patch" or "error" or "complete" or "keepalive" or "history-unavailable"))
            {
                throw new JsonException();
            }

            var final = false;
            if (root.TryGetProperty("final", out var finalElement))
            {
                if (type != "error" || finalElement.ValueKind is not (JsonValueKind.True or JsonValueKind.False))
                {
                    throw new JsonException();
                }

                final = finalElement.GetBoolean();
            }

            OperationResult<TData>? result = null;
            if (root.TryGetProperty("result", out var resultElement))
            {
                result = operation.DecodeResult(Encoding.UTF8.GetBytes(resultElement.GetRawText()));
            }

            return new StreamEvent<TData> { Type = type, Sequence = sequence, Final = final, Result = result };
        }
        catch (Exception exception) when (exception is JsonException or FormatException or OverflowException)
        {
            throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid, exception);
        }
    }

    private static async Task<ReadOnlyMemory<byte>> ReadBoundedAsync(HttpResponseMessage response, int maximumBytes, CancellationToken cancellationToken)
    {
        var contentLength = response.Content.Headers.ContentLength;
        if (contentLength.HasValue && contentLength.Value > maximumBytes)
        {
            throw new NaatreClientException(ClientErrorCodes.ResponseTooLarge);
        }

        await using var stream = await response.Content.ReadAsStreamAsync(cancellationToken).ConfigureAwait(false);
        using var output = new MemoryStream();
        var buffer = new byte[8192];
        while (true)
        {
            var read = await stream.ReadAsync(buffer, cancellationToken).ConfigureAwait(false);
            if (read == 0)
            {
                break;
            }

            if (output.Length + read > maximumBytes)
            {
                throw new NaatreClientException(ClientErrorCodes.ResponseTooLarge);
            }

            output.Write(buffer, 0, read);
        }

        return output.ToArray();
    }

    private static async Task<Stream> ReadResponseStreamAsync(HttpContent content, CancellationToken cancellationToken)
    {
        try
        {
            return await content.ReadAsStreamAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (OperationCanceledException exception) when (cancellationToken.IsCancellationRequested)
        {
            throw new NaatreOperationCanceledException(cancellationToken, exception);
        }
    }

    private static async IAsyncEnumerable<ReadOnlyMemory<byte>> ReadSseFramesAsync(
        Stream stream,
        int maximumFrameBytes,
        [EnumeratorCancellation] CancellationToken cancellationToken)
    {
        var buffer = new byte[4096];
        using var line = new MemoryStream();
        using var data = new MemoryStream();
        var skipLineFeed = false;
        while (true)
        {
            var count = await ReadStreamAsync(stream, buffer, cancellationToken).ConfigureAwait(false);
            if (count == 0)
            {
                yield break;
            }

            for (var index = 0; index < count; index++)
            {
                var value = buffer[index];
                if (skipLineFeed && value == (byte)'\n')
                {
                    skipLineFeed = false;
                    continue;
                }

                skipLineFeed = false;
                if (value is not ((byte)'\r') and not ((byte)'\n'))
                {
                    if (line.Length >= maximumFrameBytes + 6L)
                    {
                        throw new NaatreClientException(ClientErrorCodes.FrameTooLarge);
                    }

                    line.WriteByte(value);
                    continue;
                }

                skipLineFeed = value == (byte)'\r';
                var frame = ConsumeSseLine(line, data, maximumFrameBytes);
                line.SetLength(0);
                if (frame is not null)
                {
                    yield return frame;
                }
            }
        }
    }

    private static byte[]? ConsumeSseLine(MemoryStream line, MemoryStream data, int maximumFrameBytes)
    {
        var lineBytes = line.GetBuffer().AsSpan(0, checked((int)line.Length));
        if (lineBytes.Length == 0)
        {
            if (data.Length == 0)
            {
                return null;
            }

            var frame = data.ToArray();
            data.SetLength(0);
            return frame;
        }

        if (lineBytes[0] == (byte)':')
        {
            return null;
        }

        if (lineBytes.Length < 5 || !lineBytes[..5].SequenceEqual("data:"u8))
        {
            throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid);
        }

        var payload = lineBytes[5..];
        if (payload.Length != 0 && payload[0] == (byte)' ')
        {
            payload = payload[1..];
        }

        var separatorBytes = data.Length == 0 ? 0 : 1;
        if (data.Length + separatorBytes + payload.Length > maximumFrameBytes)
        {
            throw new NaatreClientException(ClientErrorCodes.FrameTooLarge);
        }

        if (separatorBytes != 0)
        {
            data.WriteByte((byte)'\n');
        }

        data.Write(payload);
        return null;
    }

    private static async Task<int> ReadStreamAsync(Stream stream, byte[] buffer, CancellationToken cancellationToken)
    {
        try
        {
            return await stream.ReadAsync(buffer, cancellationToken).ConfigureAwait(false);
        }
        catch (OperationCanceledException exception) when (cancellationToken.IsCancellationRequested)
        {
            throw new NaatreOperationCanceledException(cancellationToken, exception);
        }
    }

    private static bool IsRetryable(HttpStatusCode statusCode) => statusCode is HttpStatusCode.BadGateway or HttpStatusCode.ServiceUnavailable or HttpStatusCode.GatewayTimeout;

    private static void RequireMediaType(HttpResponseMessage response, string expected)
    {
        if (!string.Equals(response.Content.Headers.ContentType?.MediaType, expected, StringComparison.OrdinalIgnoreCase))
        {
            throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid);
        }
    }

    private static NaatreHttpOptions ValidateOptions(NaatreHttpOptions options)
    {
        if (options.MaximumResponseBytes <= 0 || options.MaximumResponseBytes > 16 * 1024 * 1024 ||
            options.MaximumFrameBytes <= 0 || options.MaximumFrameBytes > 1024 * 1024 ||
            options.MaximumRedirects is < 0 or > 5 || options.MaximumQueryAttempts is < 1 or > 5)
        {
            throw new NaatreClientException(ClientErrorCodes.ConfigInvalid);
        }

        return options;
    }

    private static Uri ValidateEndpoint(Uri endpoint)
    {
        ArgumentNullException.ThrowIfNull(endpoint);
        if (!endpoint.IsAbsoluteUri || endpoint.Scheme is not ("http" or "https") || !string.IsNullOrEmpty(endpoint.UserInfo) || !string.IsNullOrEmpty(endpoint.Fragment))
        {
            throw new NaatreClientException(ClientErrorCodes.ConfigInvalid);
        }

        return endpoint;
    }

    private static string Origin(Uri value) => value.GetComponents(UriComponents.SchemeAndServer, UriFormat.UriEscaped);

    private static bool SameOrigin(Uri left, Uri right) => string.Equals(Origin(left), Origin(right), StringComparison.OrdinalIgnoreCase);
}
