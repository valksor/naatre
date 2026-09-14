using System.Text.Json;

namespace Valksor.Naatre;

public readonly record struct OpenEnum<TKnown>(TKnown? Known, string Raw)
    where TKnown : struct, Enum
{
    public bool IsKnown => Known.HasValue;

    public static OpenEnum<TKnown> Parse(string raw)
    {
        ArgumentNullException.ThrowIfNull(raw);
        return Enum.TryParse<TKnown>(raw, ignoreCase: false, out var known) &&
            string.Equals(Enum.GetName(known), raw, StringComparison.Ordinal)
            ? new OpenEnum<TKnown>(known, raw)
            : new OpenEnum<TKnown>(null, raw);
    }

    public override string ToString() => Raw;
}

public sealed record OpenUnion
{
    public required string Discriminator { get; init; }

    public required JsonElement Value { get; init; }

    public bool IsKnown { get; init; }
}
