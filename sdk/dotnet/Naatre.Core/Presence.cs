using System.Text.Json;
using System.Text.Json.Serialization;

namespace Valksor.Naatre;

public enum PresenceState
{
    Missing,
    Null,
    Failed,
    Skipped,
    Pending,
    Present,
}

[JsonConverter(typeof(PresenceJsonConverterFactory))]
public readonly struct Presence<T> : IEquatable<Presence<T>>
{
    private readonly T? value;
    private readonly IReadOnlyList<NaatreError>? errors;

    private Presence(PresenceState state, T? value, IReadOnlyList<NaatreError>? errors, string? reason)
    {
        State = state;
        this.value = value;
        this.errors = errors;
        Reason = reason;
    }

    public PresenceState State { get; }

    public IReadOnlyList<NaatreError> Errors => errors ?? Array.Empty<NaatreError>();

    public string? Reason { get; }

    public bool HasValue => State == PresenceState.Present;

    public T Value => HasValue ? value! : throw new NaatreClientException(ClientErrorCodes.ResultInvalid);

    public static Presence<T> Missing => default;

    public static Presence<T> Null => new(PresenceState.Null, default, null, null);

    public static Presence<T> Pending => new(PresenceState.Pending, default, null, null);

    public static Presence<T> Present(T value) => value is null
        ? throw new ArgumentNullException(nameof(value))
        : new(PresenceState.Present, value, null, null);

    public static Presence<T> Failed(IEnumerable<NaatreError> errors)
    {
        ArgumentNullException.ThrowIfNull(errors);
        var values = errors.ToArray();
        if (values.Length == 0)
        {
            throw new ArgumentException("At least one error is required.", nameof(errors));
        }

        return new Presence<T>(PresenceState.Failed, default, Array.AsReadOnly(values), null);
    }

    public static Presence<T> Skipped(string reason)
    {
        ArgumentException.ThrowIfNullOrWhiteSpace(reason);
        return new Presence<T>(PresenceState.Skipped, default, null, reason);
    }

    public T RequireValue() => Value;

    public bool Equals(Presence<T> other) =>
        State == other.State &&
        EqualityComparer<T?>.Default.Equals(value, other.value) &&
        Errors.SequenceEqual(other.Errors) &&
        string.Equals(Reason, other.Reason, StringComparison.Ordinal);

    public override bool Equals(object? obj) => obj is Presence<T> other && Equals(other);

    public override int GetHashCode() => HashCode.Combine(State, value, Reason);

    public static bool operator ==(Presence<T> left, Presence<T> right) => left.Equals(right);

    public static bool operator !=(Presence<T> left, Presence<T> right) => !left.Equals(right);

    public static implicit operator Presence<T>(T value) => Present(value);
}

public sealed class PresenceJsonConverterFactory : JsonConverterFactory
{
    public override bool CanConvert(Type typeToConvert) =>
        typeToConvert.IsGenericType && typeToConvert.GetGenericTypeDefinition() == typeof(Presence<>);

    public override JsonConverter CreateConverter(Type typeToConvert, JsonSerializerOptions options)
    {
        var valueType = typeToConvert.GetGenericArguments()[0];
        return (JsonConverter)Activator.CreateInstance(typeof(PresenceJsonConverter<>).MakeGenericType(valueType))!;
    }

    private sealed class PresenceJsonConverter<TValue> : JsonConverter<Presence<TValue>>
    {
        public override bool HandleNull => true;

        public override Presence<TValue> Read(ref Utf8JsonReader reader, Type typeToConvert, JsonSerializerOptions options)
        {
            if (reader.TokenType == JsonTokenType.Null)
            {
                return Presence<TValue>.Null;
            }

            var value = JsonSerializer.Deserialize<TValue>(ref reader, options);
            return value is null
                ? throw new JsonException(ClientErrorCodes.ResultInvalid)
                : Presence<TValue>.Present(value);
        }

        public override void Write(Utf8JsonWriter writer, Presence<TValue> value, JsonSerializerOptions options)
        {
            switch (value.State)
            {
                case PresenceState.Null:
                    writer.WriteNullValue();
                    return;
                case PresenceState.Present:
                    JsonSerializer.Serialize(writer, value.Value, options);
                    return;
                default:
                    throw new JsonException(ClientErrorCodes.ResultInvalid);
            }
        }
    }
}
