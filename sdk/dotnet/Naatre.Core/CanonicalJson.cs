using System.Buffers;
using System.Globalization;
using System.Security.Cryptography;
using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;

namespace Valksor.Naatre;

public static class CanonicalJson
{
    public const int DefaultMaximumBytes = 4 * 1024 * 1024;

    public static byte[] Serialize<T>(T value, JsonSerializerOptions? options = null)
    {
        var encoded = JsonSerializer.SerializeToUtf8Bytes(value, options ?? NaatreJson.Options);
        return Canonicalize(encoded);
    }

    public static byte[] Canonicalize(ReadOnlySpan<byte> input, int maximumBytes = DefaultMaximumBytes)
    {
        if (input.Length == 0 || input.Length > maximumBytes)
        {
            throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid);
        }

        try
        {
            using var document = JsonDocument.Parse(input.ToArray(), new JsonDocumentOptions
            {
                AllowTrailingCommas = false,
                CommentHandling = JsonCommentHandling.Disallow,
                MaxDepth = 128,
            });
            var buffer = new ArrayBufferWriter<byte>();
            using (var writer = new Utf8JsonWriter(buffer, new JsonWriterOptions
            {
                Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping,
                Indented = false,
                SkipValidation = false,
            }))
            {
                WriteElement(writer, document.RootElement);
            }

            return buffer.WrittenSpan.ToArray();
        }
        catch (Exception exception) when (exception is JsonException or FormatException or OverflowException)
        {
            throw new NaatreClientException(ClientErrorCodes.ProtocolInvalid, exception);
        }
    }

    public static string SemanticHash(string purpose, ReadOnlySpan<byte> canonicalPayload)
    {
        if (purpose is not ("document" or "schema" or "approval" or "result-cache" or "idempotency" or "signed-message"))
        {
            throw new ArgumentOutOfRangeException(nameof(purpose));
        }

        var prefix = Encoding.UTF8.GetBytes($"naatre:{purpose}:c14n-1\n");
        var input = new byte[prefix.Length + canonicalPayload.Length];
        prefix.CopyTo(input, 0);
        canonicalPayload.CopyTo(input.AsSpan(prefix.Length));
        return Convert.ToHexString(SHA256.HashData(input)).ToLowerInvariant();
    }

    private static void WriteElement(Utf8JsonWriter writer, JsonElement element)
    {
        switch (element.ValueKind)
        {
            case JsonValueKind.Object:
                writer.WriteStartObject();
                var properties = element.EnumerateObject().ToArray();
                var names = new HashSet<string>(StringComparer.Ordinal);
                foreach (var property in properties)
                {
                    if (!names.Add(property.Name))
                    {
                        throw new JsonException(ClientErrorCodes.ProtocolInvalid);
                    }
                }

                foreach (var property in properties.OrderBy(static property => property.Name, StringComparer.Ordinal))
                {
                    writer.WritePropertyName(property.Name);
                    WriteElement(writer, property.Value);
                }

                writer.WriteEndObject();
                return;
            case JsonValueKind.Array:
                writer.WriteStartArray();
                foreach (var item in element.EnumerateArray())
                {
                    WriteElement(writer, item);
                }

                writer.WriteEndArray();
                return;
            case JsonValueKind.String:
                writer.WriteStringValue(element.GetString());
                return;
            case JsonValueKind.Number:
                writer.WriteRawValue(NormalizeNumber(element.GetRawText()), skipInputValidation: false);
                return;
            case JsonValueKind.True:
                writer.WriteBooleanValue(true);
                return;
            case JsonValueKind.False:
                writer.WriteBooleanValue(false);
                return;
            case JsonValueKind.Null:
                writer.WriteNullValue();
                return;
            default:
                throw new JsonException(ClientErrorCodes.ProtocolInvalid);
        }
    }

    private static string NormalizeNumber(string input)
    {
        if (!double.TryParse(input, NumberStyles.Float, CultureInfo.InvariantCulture, out var value) || !double.IsFinite(value))
        {
            throw new JsonException(ClientErrorCodes.ProtocolInvalid);
        }

        if (value == 0)
        {
            return "0";
        }

        if (input.IndexOfAny(['.', 'e', 'E']) < 0 &&
            (!long.TryParse(input, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture, out var integer) || Math.Abs((decimal)integer) > 9007199254740991m))
        {
            throw new JsonException(ClientErrorCodes.ValuePrecision);
        }

        var roundTrip = value.ToString("R", CultureInfo.InvariantCulture);
        var exponentIndex = roundTrip.IndexOfAny(['E', 'e']);
        if (exponentIndex < 0)
        {
            return roundTrip;
        }

        var mantissa = roundTrip[..exponentIndex];
        var exponent = int.Parse(roundTrip.AsSpan(exponentIndex + 1), NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture);
        var absolute = Math.Abs(value);
        if (absolute >= 1e21 || absolute < 1e-6)
        {
            return $"{mantissa}e{(exponent >= 0 ? "+" : string.Empty)}{exponent.ToString(CultureInfo.InvariantCulture)}";
        }

        var negative = mantissa[0] == '-';
        if (negative)
        {
            mantissa = mantissa[1..];
        }

        var dot = mantissa.IndexOf('.');
        var digits = mantissa.Replace(".", string.Empty, StringComparison.Ordinal);
        var point = (dot < 0 ? mantissa.Length : dot) + exponent;
        var expanded = point <= 0
            ? "0." + new string('0', -point) + digits
            : point >= digits.Length
                ? digits + new string('0', point - digits.Length)
                : digits[..point] + "." + digits[point..];
        return negative ? "-" + expanded : expanded;
    }
}

public static class NaatreJson
{
    public static JsonSerializerOptions Options { get; } = CreateOptions();

    private static JsonSerializerOptions CreateOptions() => new(JsonSerializerDefaults.Web)
    {
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.WhenWritingDefault,
        PropertyNameCaseInsensitive = false,
        ReadCommentHandling = JsonCommentHandling.Disallow,
        AllowTrailingCommas = false,
        MaxDepth = 128,
        UnmappedMemberHandling = System.Text.Json.Serialization.JsonUnmappedMemberHandling.Disallow,
    };
}
