using System.Text.Json;
using System.Text.Json.Serialization;

namespace Valksor.Naatre;

public sealed record NaatreError
{
    [JsonPropertyName("code")]
    public required string Code { get; init; }

    [JsonPropertyName("message")]
    public string? Message { get; init; }

    [JsonPropertyName("path")]
    public IReadOnlyList<JsonElement> Path { get; init; } = Array.Empty<JsonElement>();

    [JsonPropertyName("retryable")]
    public bool Retryable { get; init; }

    [JsonExtensionData]
    public IDictionary<string, JsonElement>? Details { get; init; }
}

public sealed record OperationResult<T>
{
    public required T? Data { get; init; }

    public required IReadOnlyList<NaatreError> Errors { get; init; }

    public required bool Complete { get; init; }

    public T RequireComplete()
    {
        if (!Complete || Errors.Count != 0 || Data is null)
        {
            throw new NaatreClientException(ClientErrorCodes.ResultInvalid);
        }

        return Data;
    }
}

public sealed record StreamEvent<T>
{
    public required string Type { get; init; }

    public required ulong Sequence { get; init; }

    public bool Final { get; init; }

    public OperationResult<T>? Result { get; init; }

    public bool Terminal => Type == "complete" || (Type == "error" && Final);

    public bool EndsAttempt => Terminal || Type == "history-unavailable";
}

public static class OperationResultDecoder
{
    public static OperationResult<T> Decode<T>(
        ReadOnlyMemory<byte> input,
        Func<JsonElement, IReadOnlyList<NaatreError>, bool, T> decodeData)
    {
        ArgumentNullException.ThrowIfNull(decodeData);
        try
        {
            using var document = JsonDocument.Parse(input);
            var root = document.RootElement;
            if (root.ValueKind != JsonValueKind.Object)
            {
                throw new JsonException();
            }

            RejectDuplicateProperties(root);
            foreach (var property in root.EnumerateObject())
            {
                if (property.Name is not ("data" or "errors" or "complete"))
                {
                    throw new JsonException();
                }
            }

            if (!root.TryGetProperty("complete", out var completeElement) ||
                completeElement.ValueKind is not (JsonValueKind.True or JsonValueKind.False))
            {
                throw new JsonException();
            }

            var complete = completeElement.GetBoolean();
            var errors = root.TryGetProperty("errors", out var errorsElement)
                ? JsonSerializer.Deserialize<NaatreError[]>(errorsElement.GetRawText(), NaatreJson.Options)
                    ?? throw new JsonException()
                : Array.Empty<NaatreError>();
            if (errors.Any(static error => error is null || string.IsNullOrWhiteSpace(error.Code)))
            {
                throw new JsonException();
            }

            T? data = default;
            if (root.TryGetProperty("data", out var dataElement) && dataElement.ValueKind != JsonValueKind.Null)
            {
                data = decodeData(dataElement, errors, complete);
            }

            return new OperationResult<T>
            {
                Data = data,
                Errors = Array.AsReadOnly(errors),
                Complete = complete,
            };
        }
        catch (Exception exception) when (exception is JsonException or NotSupportedException)
        {
            throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid, exception);
        }
    }

    public static Presence<T> Field<T>(
        JsonElement owner,
        string name,
        bool pendingWhenMissing,
        IReadOnlyList<NaatreError> errors,
        Func<JsonElement, T> decode)
    {
        if (owner.ValueKind != JsonValueKind.Object)
        {
            throw new NaatreClientException(ClientErrorCodes.ResultInvalid);
        }

        if (!owner.TryGetProperty(name, out var value))
        {
            var matching = errors.Where(error => PathStartsWith(error, name)).ToArray();
            if (matching.Length != 0)
            {
                return Presence<T>.Failed(matching);
            }

            return pendingWhenMissing ? Presence<T>.Pending : Presence<T>.Missing;
        }

        if (value.ValueKind == JsonValueKind.Null)
        {
            return Presence<T>.Null;
        }

        try
        {
            return Presence<T>.Present(decode(value));
        }
        catch (NaatreClientException)
        {
            throw;
        }
        catch (Exception exception) when (exception is JsonException or FormatException or OverflowException)
        {
            throw new NaatreClientException(ClientErrorCodes.ResultInvalid, exception);
        }
    }

    public static string String(JsonElement value) => value.ValueKind == JsonValueKind.String
        ? value.GetString()!
        : throw new NaatreClientException(ClientErrorCodes.ResultInvalid);

    public static bool Boolean(JsonElement value) => value.ValueKind is JsonValueKind.True or JsonValueKind.False
        ? value.GetBoolean()
        : throw new NaatreClientException(ClientErrorCodes.ResultInvalid);

    public static int Int32(JsonElement value) => value.TryGetInt32(out var result)
        ? result
        : throw new NaatreClientException(ClientErrorCodes.ResultInvalid);

    public static double Float64(JsonElement value) => value.TryGetDouble(out var result) && double.IsFinite(result)
        ? result
        : throw new NaatreClientException(ClientErrorCodes.ResultInvalid);

    private static bool PathStartsWith(NaatreError error, string name) =>
        error.Path.Count != 0 && error.Path[0].ValueKind == JsonValueKind.String && string.Equals(error.Path[0].GetString(), name, StringComparison.Ordinal);

    private static void RejectDuplicateProperties(JsonElement value)
    {
        if (value.ValueKind == JsonValueKind.Array)
        {
            foreach (var item in value.EnumerateArray())
            {
                RejectDuplicateProperties(item);
            }

            return;
        }

        if (value.ValueKind != JsonValueKind.Object)
        {
            return;
        }

        var names = new HashSet<string>(StringComparer.Ordinal);
        foreach (var property in value.EnumerateObject())
        {
            if (!names.Add(property.Name))
            {
                throw new JsonException();
            }

            RejectDuplicateProperties(property.Value);
        }
    }
}
