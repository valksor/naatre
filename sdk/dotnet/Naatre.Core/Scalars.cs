using System.Globalization;
using System.Numerics;
using System.Text.RegularExpressions;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace Valksor.Naatre;

[JsonConverter(typeof(Int64ValueJsonConverter))]
public readonly record struct Int64Value
{
    private Int64Value(string raw) => Raw = raw;

    public string Raw { get; }

    public long Value => long.Parse(Raw, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture);

    public static Int64Value Parse(string value) => new(ScalarCodecs.NormalizeBoundedInteger(value, long.MinValue, long.MaxValue));

    public static Int64Value FromInt64(long value) => new(value.ToString(CultureInfo.InvariantCulture));

    public override string ToString() => Raw;
}

[JsonConverter(typeof(UInt64ValueJsonConverter))]
public readonly record struct UInt64Value
{
    private UInt64Value(string raw) => Raw = raw;

    public string Raw { get; }

    public ulong Value => ulong.Parse(Raw, NumberStyles.None, CultureInfo.InvariantCulture);

    public static UInt64Value Parse(string value) => new(ScalarCodecs.NormalizeBoundedInteger(value, BigInteger.Zero, new BigInteger(ulong.MaxValue)));

    public static UInt64Value FromUInt64(ulong value) => new(value.ToString(CultureInfo.InvariantCulture));

    public override string ToString() => Raw;
}

[JsonConverter(typeof(BigIntegerValueJsonConverter))]
public readonly record struct BigIntegerValue
{
    private BigIntegerValue(string raw) => Raw = raw;

    public string Raw { get; }

    public BigInteger Value => BigInteger.Parse(Raw, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture);

    public static BigIntegerValue Parse(string value) => new(ScalarCodecs.NormalizeInteger(value));

    public static BigIntegerValue FromBigInteger(BigInteger value) => new(value.ToString(CultureInfo.InvariantCulture));

    public override string ToString() => Raw;
}

[JsonConverter(typeof(DecimalValueJsonConverter))]
public readonly record struct DecimalValue
{
    private DecimalValue(string raw) => Raw = raw;

    public string Raw { get; }

    public static DecimalValue Parse(string value) => new(ScalarCodecs.NormalizeDecimal(value));

    public static DecimalValue FromDecimal(decimal value) => Parse(value.ToString(CultureInfo.InvariantCulture));

    public bool TryGetDecimal(out decimal value) => decimal.TryParse(
        Raw,
        NumberStyles.AllowLeadingSign | NumberStyles.AllowDecimalPoint,
        CultureInfo.InvariantCulture,
        out value);

    public override string ToString() => Raw;
}

[JsonConverter(typeof(TimestampValueJsonConverter))]
public readonly record struct TimestampValue
{
    private static readonly string[] DateTimeOffsetFormats = ["yyyy-MM-dd'T'HH:mm:ss'Z'", "yyyy-MM-dd'T'HH:mm:ss.FFFFFFF'Z'"];

    private TimestampValue(string raw) => Raw = raw;

    public string Raw { get; }

    public static TimestampValue Parse(string value) => new(ScalarCodecs.NormalizeTimestamp(value));

    public static TimestampValue FromDateTimeOffset(DateTimeOffset value)
    {
        var utc = value.ToUniversalTime();
        var fraction = utc.ToString("fffffff", CultureInfo.InvariantCulture).TrimEnd('0');
        var suffix = fraction.Length == 0 ? string.Empty : $".{fraction}";
        return new TimestampValue(utc.ToString("yyyy-MM-dd'T'HH:mm:ss", CultureInfo.InvariantCulture) + suffix + "Z");
    }

    public bool TryGetDateTimeOffset(out DateTimeOffset value)
    {
        var fractionStart = Raw.IndexOf('.', StringComparison.Ordinal);
        var fractionDigits = fractionStart < 0 ? 0 : Raw.Length - fractionStart - 2;
        if (Raw.StartsWith("0000-", StringComparison.Ordinal) || fractionDigits > 7)
        {
            value = default;
            return false;
        }

        return DateTimeOffset.TryParseExact(
            Raw,
            DateTimeOffsetFormats,
            CultureInfo.InvariantCulture,
            DateTimeStyles.AssumeUniversal | DateTimeStyles.AdjustToUniversal,
            out value);
    }

    public override string ToString() => Raw;
}

[JsonConverter(typeof(DurationValueJsonConverter))]
public readonly record struct DurationValue
{
    private DurationValue(string raw) => Raw = raw;

    public string Raw { get; }

    public long Nanoseconds => long.Parse(Raw, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture);

    public static DurationValue Parse(string value) => new(ScalarCodecs.NormalizeBoundedInteger(value, long.MinValue, long.MaxValue));

    public static DurationValue FromTimeSpan(TimeSpan value) => new(checked(value.Ticks * 100).ToString(CultureInfo.InvariantCulture));

    public bool TryGetTimeSpan(out TimeSpan value)
    {
        var nanoseconds = Nanoseconds;
        if (nanoseconds % 100 != 0)
        {
            value = default;
            return false;
        }

        value = TimeSpan.FromTicks(nanoseconds / 100);
        return true;
    }

    public override string ToString() => Raw;
}

[JsonConverter(typeof(UuidValueJsonConverter))]
public readonly record struct UuidValue
{
    private UuidValue(string raw) => Raw = raw;

    public string Raw { get; }

    public Guid Value => Guid.ParseExact(Raw, "D");

    public static UuidValue Parse(string value)
    {
        if (!Guid.TryParseExact(value, "D", out var parsed))
        {
            throw ScalarCodecs.Invalid();
        }

        return new UuidValue(parsed.ToString("D", CultureInfo.InvariantCulture));
    }

    public static UuidValue FromGuid(Guid value) => new(value.ToString("D", CultureInfo.InvariantCulture));

    public override string ToString() => Raw;
}

[JsonConverter(typeof(BytesValueJsonConverter))]
public readonly record struct BytesValue
{
    private BytesValue(string raw) => Raw = raw;

    public string Raw { get; }

    public static BytesValue Parse(string value)
    {
        ArgumentNullException.ThrowIfNull(value);
        if (!Regex.IsMatch(value, "^[A-Za-z0-9_-]*$", RegexOptions.CultureInvariant) || value.Length % 4 == 1)
        {
            throw ScalarCodecs.Invalid();
        }

        try
        {
            var decoded = Decode(value);
            var canonical = FromBytes(decoded).Raw;
            if (!string.Equals(canonical, value, StringComparison.Ordinal))
            {
                throw ScalarCodecs.Invalid();
            }

            return new BytesValue(value);
        }
        catch (FormatException exception)
        {
            throw ScalarCodecs.Invalid(exception);
        }
    }

    public static BytesValue FromBytes(ReadOnlySpan<byte> value)
    {
        var encoded = Convert.ToBase64String(value).TrimEnd('=').Replace('+', '-').Replace('/', '_');
        return new BytesValue(encoded);
    }

    public byte[] ToArray() => Decode(Raw);

    public override string ToString() => Raw;

    private static byte[] Decode(string value)
    {
        var padded = value.Replace('-', '+').Replace('_', '/');
        padded += (value.Length % 4) switch { 0 => string.Empty, 2 => "==", 3 => "=", _ => throw ScalarCodecs.Invalid() };
        return Convert.FromBase64String(padded);
    }
}

internal static partial class ScalarCodecs
{
    [GeneratedRegex("^-?[0-9]+$", RegexOptions.CultureInvariant)]
    private static partial Regex IntegerPattern();

    [GeneratedRegex("^-?[0-9]+(?:\\.[0-9]+)?$", RegexOptions.CultureInvariant)]
    private static partial Regex DecimalPattern();

    [GeneratedRegex("^(?<year>[0-9]{4})-(?<month>[0-9]{2})-(?<day>[0-9]{2})T(?<hour>[0-9]{2}):(?<minute>[0-9]{2}):(?<second>[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$", RegexOptions.CultureInvariant)]
    private static partial Regex TimestampPattern();

    internal static NaatreClientException Invalid(Exception? innerException = null) => new(ClientErrorCodes.ScalarInvalid, innerException);

    internal static string NormalizeInteger(string value)
    {
        ArgumentNullException.ThrowIfNull(value);
        if (!IntegerPattern().IsMatch(value))
        {
            throw Invalid();
        }

        return BigInteger.Parse(value, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture).ToString(CultureInfo.InvariantCulture);
    }

    internal static string NormalizeBoundedInteger(string value, BigInteger minimum, BigInteger maximum)
    {
        var canonical = NormalizeInteger(value);
        var parsed = BigInteger.Parse(canonical, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture);
        if (parsed < minimum || parsed > maximum)
        {
            throw Invalid();
        }

        return canonical;
    }

    internal static string NormalizeDecimal(string value)
    {
        ArgumentNullException.ThrowIfNull(value);
        if (!DecimalPattern().IsMatch(value))
        {
            throw Invalid();
        }

        var negative = value[0] == '-';
        var unsigned = negative ? value[1..] : value;
        var split = unsigned.Split('.', 2);
        var integer = split[0].TrimStart('0');
        if (integer.Length == 0)
        {
            integer = "0";
        }

        var fraction = split.Length == 2 ? split[1].TrimEnd('0') : string.Empty;
        var result = fraction.Length == 0 ? integer : $"{integer}.{fraction}";
        return result.All(static character => character is '0' or '.') ? "0" : (negative ? "-" : string.Empty) + result;
    }

    internal static string NormalizeTimestamp(string value)
    {
        ArgumentNullException.ThrowIfNull(value);
        var match = TimestampPattern().Match(value);
        if (!match.Success)
        {
            throw Invalid();
        }

        var year = ParsePart(match, "year");
        var month = ParsePart(match, "month");
        var day = ParsePart(match, "day");
        var hour = ParsePart(match, "hour");
        var minute = ParsePart(match, "minute");
        var second = ParsePart(match, "second");
        if (month is < 1 or > 12 || day < 1 || day > DaysInMonth(year, month) || hour > 23 || minute > 59 || second > 59)
        {
            throw Invalid();
        }

        var zone = match.Groups["zone"].Value;
        var offset = 0;
        if (zone != "Z")
        {
            var offsetHour = int.Parse(zone.AsSpan(1, 2), NumberStyles.None, CultureInfo.InvariantCulture);
            var offsetMinute = int.Parse(zone.AsSpan(4, 2), NumberStyles.None, CultureInfo.InvariantCulture);
            if (offsetHour > 23 || offsetMinute > 59)
            {
                throw Invalid();
            }

            offset = (zone[0] == '+' ? 1 : -1) * ((offsetHour * 60) + offsetMinute);
        }

        var utcMinutes = (hour * 60) + minute - offset;
        if (utcMinutes < 0)
        {
            (year, month, day) = PreviousDay(year, month, day);
            utcMinutes += 1440;
        }
        else if (utcMinutes >= 1440)
        {
            (year, month, day) = NextDay(year, month, day);
            utcMinutes -= 1440;
        }

        if (year is < 0 or > 9999)
        {
            throw Invalid();
        }

        var fraction = match.Groups["fraction"].Value.TrimEnd('0');
        var fractionPart = fraction.Length == 0 ? string.Empty : $".{fraction}";
        return FormattableString.Invariant($"{year:0000}-{month:00}-{day:00}T{utcMinutes / 60:00}:{utcMinutes % 60:00}:{second:00}{fractionPart}Z");
    }

    private static int ParsePart(Match match, string name) => int.Parse(match.Groups[name].Value, NumberStyles.None, CultureInfo.InvariantCulture);

    private static int DaysInMonth(int year, int month) => month switch
    {
        2 => year % 4 == 0 && (year % 100 != 0 || year % 400 == 0) ? 29 : 28,
        4 or 6 or 9 or 11 => 30,
        _ => 31,
    };

    private static (int Year, int Month, int Day) PreviousDay(int year, int month, int day)
    {
        if (day > 1) return (year, month, day - 1);
        if (month > 1) return (year, month - 1, DaysInMonth(year, month - 1));
        return (year - 1, 12, 31);
    }

    private static (int Year, int Month, int Day) NextDay(int year, int month, int day)
    {
        if (day < DaysInMonth(year, month)) return (year, month, day + 1);
        if (month < 12) return (year, month + 1, 1);
        return (year + 1, 1, 1);
    }
}

internal abstract class WireStringJsonConverter<T> : JsonConverter<T>
{
    protected abstract T Parse(string value);

    protected abstract string Format(T value);

    public sealed override T Read(ref Utf8JsonReader reader, Type typeToConvert, JsonSerializerOptions options)
    {
        if (reader.TokenType != JsonTokenType.String)
        {
            throw new JsonException(ClientErrorCodes.ScalarInvalid);
        }

        try
        {
            return Parse(reader.GetString()!);
        }
        catch (NaatreClientException exception)
        {
            throw new JsonException(exception.Code, exception);
        }
    }

    public sealed override void Write(Utf8JsonWriter writer, T value, JsonSerializerOptions options) => writer.WriteStringValue(Format(value));
}

internal sealed class Int64ValueJsonConverter : WireStringJsonConverter<Int64Value>
{
    protected override Int64Value Parse(string value) => Int64Value.Parse(value);
    protected override string Format(Int64Value value) => value.Raw;
}

internal sealed class UInt64ValueJsonConverter : WireStringJsonConverter<UInt64Value>
{
    protected override UInt64Value Parse(string value) => UInt64Value.Parse(value);
    protected override string Format(UInt64Value value) => value.Raw;
}

internal sealed class BigIntegerValueJsonConverter : WireStringJsonConverter<BigIntegerValue>
{
    protected override BigIntegerValue Parse(string value) => BigIntegerValue.Parse(value);
    protected override string Format(BigIntegerValue value) => value.Raw;
}

internal sealed class DecimalValueJsonConverter : WireStringJsonConverter<DecimalValue>
{
    protected override DecimalValue Parse(string value) => DecimalValue.Parse(value);
    protected override string Format(DecimalValue value) => value.Raw;
}

internal sealed class TimestampValueJsonConverter : WireStringJsonConverter<TimestampValue>
{
    protected override TimestampValue Parse(string value) => TimestampValue.Parse(value);
    protected override string Format(TimestampValue value) => value.Raw;
}

internal sealed class DurationValueJsonConverter : WireStringJsonConverter<DurationValue>
{
    protected override DurationValue Parse(string value) => DurationValue.Parse(value);
    protected override string Format(DurationValue value) => value.Raw;
}

internal sealed class UuidValueJsonConverter : WireStringJsonConverter<UuidValue>
{
    protected override UuidValue Parse(string value) => UuidValue.Parse(value);
    protected override string Format(UuidValue value) => value.Raw;
}

internal sealed class BytesValueJsonConverter : WireStringJsonConverter<BytesValue>
{
    protected override BytesValue Parse(string value) => BytesValue.Parse(value);
    protected override string Format(BytesValue value) => value.Raw;
}
