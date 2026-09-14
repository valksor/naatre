using System.Text.Json;
using System.Text.Json.Serialization;

namespace Valksor.Naatre;

public sealed record PersistedReference
{
    [JsonPropertyName("algorithm")]
    public string Algorithm { get; init; } = "sha-256";

    [JsonPropertyName("canonicalVersion")]
    public string CanonicalVersion { get; init; } = "c14n-1";

    [JsonPropertyName("digest")]
    public required string Digest { get; init; }
}

public sealed class NaatreOperation<TData>
{
    private readonly Func<ReadOnlyMemory<byte>, OperationResult<TData>> decode;

    public NaatreOperation(
        string name,
        string kind,
        PersistedReference persisted,
        object variables,
        Func<ReadOnlyMemory<byte>, OperationResult<TData>> decode)
    {
        ArgumentException.ThrowIfNullOrWhiteSpace(name);
        if (kind is not ("query" or "mutation" or "subscription"))
        {
            throw new ArgumentOutOfRangeException(nameof(kind));
        }

        ArgumentNullException.ThrowIfNull(persisted);
        ArgumentNullException.ThrowIfNull(variables);
        ArgumentNullException.ThrowIfNull(decode);
        if (persisted.Algorithm != "sha-256" || persisted.CanonicalVersion != "c14n-1" ||
            persisted.Digest.Length != 64 || persisted.Digest.Any(static character => !Uri.IsHexDigit(character) || char.IsUpper(character)))
        {
            throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid);
        }

        Name = name;
        Kind = kind;
        Persisted = persisted;
        this.decode = decode;
        CanonicalRequest = BuildRequest(name, persisted, variables);
    }

    public string Name { get; }

    public string Kind { get; }

    public PersistedReference Persisted { get; }

    public ReadOnlyMemory<byte> CanonicalRequest { get; }

    public OperationResult<TData> DecodeResult(ReadOnlyMemory<byte> response) => decode(response);

    private static ReadOnlyMemory<byte> BuildRequest(string name, PersistedReference persisted, object variables)
    {
        var variablesElement = JsonSerializer.SerializeToElement(variables, NaatreJson.Options);
        var request = new Dictionary<string, object?>(StringComparer.Ordinal)
        {
            ["version"] = "1",
            ["operation"] = name,
            ["persisted"] = persisted,
            ["variables"] = variablesElement,
        };
        return CanonicalJson.Serialize(request, NaatreJson.Options);
    }
}
