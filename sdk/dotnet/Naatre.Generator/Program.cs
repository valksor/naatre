using System.Globalization;
using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;
using Valksor.Naatre;

return GeneratorProgram.Run(args);

internal static partial class GeneratorProgram
{
    private const string GeneratorVersion = "naatre.generator.dotnet-sdk-1";
    private static readonly HashSet<string> Reserved = new(StringComparer.Ordinal)
    {
        "abstract", "as", "base", "bool", "break", "byte", "case", "catch", "char", "checked", "class", "const", "continue", "decimal", "default", "delegate", "do", "double", "else", "enum", "event", "explicit", "extern", "false", "finally", "fixed", "float", "for", "foreach", "goto", "if", "implicit", "in", "int", "interface", "internal", "is", "lock", "long", "namespace", "new", "null", "object", "operator", "out", "override", "params", "private", "protected", "public", "readonly", "ref", "return", "sbyte", "sealed", "short", "sizeof", "stackalloc", "static", "string", "struct", "switch", "this", "throw", "true", "try", "typeof", "uint", "ulong", "unchecked", "unsafe", "ushort", "using", "virtual", "void", "volatile", "while",
    };

    [GeneratedRegex("^[A-Za-z_][A-Za-z0-9_]{0,127}$", RegexOptions.CultureInvariant)]
    private static partial Regex IdentifierPattern();

    internal static int Run(string[] arguments)
    {
        if (arguments.Length is < 2 or > 3)
        {
            Console.Error.WriteLine("usage: Naatre.Generator MODEL REFERENCE [OUTPUT_ROOT]");
            return 2;
        }

        try
        {
            var output = Generate(File.ReadAllBytes(arguments[0]), File.ReadAllBytes(arguments[1]));
            var outputRoot = arguments.Length == 3 ? Path.GetFullPath(arguments[2]) : Path.GetFullPath("sdk/dotnet/Naatre.Core/Generated");
            Directory.CreateDirectory(outputRoot);
            WriteContained(outputRoot, "Operations.g.cs", output.Source);
            WriteContained(outputRoot, "operations.json", output.Manifest);
            return 0;
        }
        catch (GeneratorException exception)
        {
            Console.Error.WriteLine(exception.Code);
            return 1;
        }
    }

    internal static GeneratedArtifacts Generate(ReadOnlySpan<byte> modelBytes, ReadOnlySpan<byte> referenceBytes)
    {
        if (modelBytes.Length is 0 or > 4 * 1024 * 1024 || referenceBytes.Length is 0 or > 4 * 1024 * 1024)
        {
            throw Error("DOTNET_SDK_GENERATOR_INPUT_LIMIT");
        }

        using var modelDocument = Parse(modelBytes);
        using var referenceDocument = Parse(referenceBytes);
        var model = modelDocument.RootElement;
        var reference = referenceDocument.RootElement;
        RequireString(model, "version", "naatre.generator-model-1", "DOTNET_SDK_GENERATOR_VERSION_SKEW");
        RequireString(model, "protocolVersion", "1", "DOTNET_SDK_GENERATOR_VERSION_SKEW");
        RequireString(model, "canonicalVersion", "c14n-1", "DOTNET_SDK_GENERATOR_VERSION_SKEW");
        RequireString(reference, "modelVersion", "naatre.generator-model-1", "DOTNET_SDK_GENERATOR_VERSION_SKEW");
        RequireString(reference, "protocolVersion", "1", "DOTNET_SDK_GENERATOR_VERSION_SKEW");
        RequireString(reference, "canonicalVersion", "c14n-1", "DOTNET_SDK_GENERATOR_VERSION_SKEW");
        RequireString(reference, "generatorVersion", "naatre.generator.reference-json-1", "DOTNET_SDK_GENERATOR_VERSION_SKEW");

        var mappings = model.GetProperty("configuration").GetProperty("scalarMappings");
        ValidateSchema(model.GetProperty("schema"), mappings);
        var references = reference.GetProperty("operations").EnumerateArray().ToDictionary(
            static operation => operation.GetProperty("name").GetString()!,
            static operation => operation.Clone(),
            StringComparer.Ordinal);
        var operations = model.GetProperty("operations").EnumerateArray().OrderBy(static operation => operation.GetProperty("name").GetString(), StringComparer.Ordinal).ToArray();
        if (operations.Length != references.Count)
        {
            throw Error("DOTNET_SDK_GENERATOR_REFERENCE_DRIFT");
        }

        var source = RenderSource(model, operations, references, mappings);
        var manifestValue = new
        {
            profile = "sdk.dotnet.core-1",
            version = "1",
            protocolVersion = "1",
            canonicalVersion = "c14n-1",
            operations = operations.Select(operation =>
            {
                var name = operation.GetProperty("name").GetString()!;
                var referenceOperation = references[name];
                return new
                {
                    name,
                    kind = operation.GetProperty("document").GetProperty("operations")[0].GetProperty("kind").GetString(),
                    persisted = JsonSerializer.Deserialize<object>(referenceOperation.GetProperty("persisted").GetRawText()),
                };
            }).ToArray(),
        };
        var manifest = Encoding.UTF8.GetString(CanonicalJson.Serialize(manifestValue)) + "\n";
        return new GeneratedArtifacts(source + "\n", manifest);
    }

    private static string RenderSource(
        JsonElement model,
        IReadOnlyList<JsonElement> operations,
        Dictionary<string, JsonElement> references,
        JsonElement mappings)
    {
        var lines = new List<string>
        {
            "// Code generated by the Naatre .NET SDK generator; DO NOT EDIT.",
            "#nullable enable",
            "using System.Text.Json;",
            "using System.Text.Json.Serialization;",
            "using Valksor.Naatre;",
            string.Empty,
            "namespace Valksor.Naatre.Generated;",
            string.Empty,
            $"public static class GeneratorMetadata {{ public const string Version = \"{GeneratorVersion}\"; }}",
            string.Empty,
        };

        foreach (var descriptor in model.GetProperty("schema").GetProperty("types").EnumerateArray().OrderBy(static value => value.GetProperty("id").GetString(), StringComparer.Ordinal))
        {
            RenderSchemaType(lines, descriptor, mappings);
            lines.Add(string.Empty);
        }

        foreach (var operation in operations)
        {
            RenderOperation(lines, operation, references[operation.GetProperty("name").GetString()!], mappings);
            lines.Add(string.Empty);
        }

        return string.Join("\n", lines).TrimEnd();
    }

    private static void RenderSchemaType(List<string> lines, JsonElement descriptor, JsonElement mappings)
    {
        var name = SafeSymbol(descriptor.GetProperty("name").GetString()!);
        RenderDocumentation(lines, descriptor, string.Empty);
        switch (descriptor.GetProperty("kind").GetString())
        {
            case "scalar":
                lines.Add($"public readonly record struct {name}(DecimalValue Value);");
                break;
            case "enum":
                lines.Add($"public enum {name}Known");
                lines.Add("{");
                foreach (var member in descriptor.GetProperty("enumMembers").EnumerateArray().OrderBy(static value => value.GetProperty("id").GetString(), StringComparer.Ordinal))
                {
                    lines.Add($"    {SafeSymbol(member.GetProperty("name").GetString()!)},");
                }

                lines.Add("}");
                lines.Add(string.Empty);
                lines.Add($"public readonly record struct {name}(OpenEnum<{name}Known> Value)");
                lines.Add("{");
                lines.Add($"    public static {name} Parse(string raw) => new(OpenEnum<{name}Known>.Parse(raw));");
                lines.Add("}");
                break;
            case "object":
                lines.Add($"public sealed record {name}");
                lines.Add("{");
                foreach (var field in descriptor.GetProperty("fields").EnumerateArray().OrderBy(static value => value.GetProperty("id").GetString(), StringComparer.Ordinal))
                {
                    RenderDocumentation(lines, field, "    ");
                    var property = SafeSymbol(field.GetProperty("name").GetString()!);
                    var type = ScalarType(field.GetProperty("type").GetString()!, mappings);
                    lines.Add($"    [JsonPropertyName({Literal(field.GetProperty("name").GetString()!)})]");
                    lines.Add("    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingDefault)]");
                    lines.Add($"    public Presence<{type}> {property} {{ get; init; }}");
                    lines.Add(string.Empty);
                }

                lines.Add("}");
                break;
            default:
                throw Error("DOTNET_SDK_GENERATOR_UNSUPPORTED_TYPE");
        }
    }

    private static void RenderOperation(List<string> lines, JsonElement operation, JsonElement reference, JsonElement mappings)
    {
        var name = SafeSymbol(operation.GetProperty("name").GetString()!);
        var kind = operation.GetProperty("document").GetProperty("operations")[0].GetProperty("kind").GetString()!;
        var persisted = reference.GetProperty("persisted");
        var digest = persisted.GetProperty("digest").GetString()!;
        var documentBytes = Encoding.UTF8.GetBytes(operation.GetProperty("document").GetRawText());
        var actualDigest = CanonicalJson.SemanticHash("document", CanonicalJson.Canonicalize(documentBytes));
        if (!string.Equals(digest, actualDigest, StringComparison.Ordinal))
        {
            throw Error("DOTNET_SDK_GENERATOR_REFERENCE_DRIFT");
        }

        RenderDocumentation(lines, operation, string.Empty);
        lines.Add($"public sealed record {name}Variables");
        lines.Add("{");
        foreach (var variable in operation.GetProperty("variables").EnumerateArray().OrderBy(static value => value.GetProperty("name").GetString(), StringComparer.Ordinal))
        {
            var variableName = variable.GetProperty("name").GetString()!;
            var property = SafeSymbol(variableName);
            var type = ScalarType(variable.GetProperty("type").GetString()!, mappings);
            var required = variable.GetProperty("required").GetBoolean();
            var nullable = variable.GetProperty("nullable").GetBoolean();
            var annotatedType = nullable ? type + "?" : type;
            lines.Add($"    [JsonPropertyName({Literal(variableName)})]");
            if (required)
            {
                lines.Add($"    public required {annotatedType} {property} {{ get; init; }}");
            }
            else
            {
                lines.Add("    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingDefault)]");
                lines.Add($"    public Presence<{annotatedType}> {property} {{ get; init; }}");
            }

            lines.Add(string.Empty);
        }

        lines.Add("}");
        lines.Add(string.Empty);
        var declarations = new List<string>();
        RenderResultType(declarations, operation.GetProperty("result"), name + "Result", mappings);
        lines.AddRange(declarations);
        lines.Add(string.Empty);
        lines.Add($"public static class {name}");
        lines.Add("{");
        lines.Add($"    public static NaatreOperation<{name}Result> Create({name}Variables variables) => new(");
        lines.Add($"        {Literal(operation.GetProperty("name").GetString()!)},");
        lines.Add($"        {Literal(kind)},");
        lines.Add($"        new PersistedReference {{ Digest = {Literal(digest)} }},");
        lines.Add("        variables,");
        lines.Add("        Decode);");
        lines.Add(string.Empty);
        lines.Add($"    public static OperationResult<{name}Result> Decode(ReadOnlyMemory<byte> input) =>");
        lines.Add($"        OperationResultDecoder.Decode(input, static (data, errors, complete) => Decode{name}Result(data, errors, complete));");
        lines.Add(string.Empty);
        RenderResultDecoder(lines, operation.GetProperty("result"), name + "Result", mappings, "    ");
        lines.Add("}");
    }

    private static void RenderResultType(List<string> lines, JsonElement node, string name, JsonElement mappings)
    {
        if (node.GetProperty("kind").GetString() != "object")
        {
            throw Error("DOTNET_SDK_GENERATOR_UNSUPPORTED_RESULT");
        }

        foreach (var field in node.GetProperty("fields").EnumerateArray())
        {
            var child = field.GetProperty("result");
            if (child.GetProperty("kind").GetString() == "object")
            {
                RenderResultType(lines, child, name + SafeSymbol(field.GetProperty("name").GetString()!), mappings);
                lines.Add(string.Empty);
            }
        }

        lines.Add($"public sealed record {name}");
        lines.Add("{");
        foreach (var field in node.GetProperty("fields").EnumerateArray())
        {
            var type = ResultType(field.GetProperty("result"), name + SafeSymbol(field.GetProperty("name").GetString()!), mappings);
            lines.Add($"    public Presence<{type}> {SafeSymbol(field.GetProperty("name").GetString()!)} {{ get; init; }}");
            lines.Add(string.Empty);
        }

        lines.Add("}");
    }

    private static void RenderResultDecoder(List<string> lines, JsonElement node, string name, JsonElement mappings, string indent)
    {
        foreach (var field in node.GetProperty("fields").EnumerateArray())
        {
            var child = field.GetProperty("result");
            if (child.GetProperty("kind").GetString() == "object")
            {
                RenderResultDecoder(lines, child, name + SafeSymbol(field.GetProperty("name").GetString()!), mappings, indent);
                lines.Add(string.Empty);
            }
        }

        lines.Add($"{indent}private static {name} Decode{name}(JsonElement value, IReadOnlyList<NaatreError> errors, bool complete) => new()");
        lines.Add($"{indent}{{");
        foreach (var field in node.GetProperty("fields").EnumerateArray())
        {
            var fieldName = field.GetProperty("name").GetString()!;
            var child = field.GetProperty("result");
            var property = SafeSymbol(fieldName);
            var pending = field.GetProperty("presence").GetString() == "pending";
            var decoder = ResultDecoder(child, name + property, mappings);
            lines.Add($"{indent}    {property} = OperationResultDecoder.Field(value, {Literal(fieldName)}, {pending.ToString().ToLowerInvariant()}, errors, item => {decoder}),");
        }

        lines.Add($"{indent}}};");
    }

    private static string ResultType(JsonElement node, string nestedName, JsonElement mappings) => node.GetProperty("kind").GetString() switch
    {
        "object" => nestedName,
        "scalar" => ScalarType(node.GetProperty("type").GetString()!, mappings),
        _ => throw Error("DOTNET_SDK_GENERATOR_UNSUPPORTED_RESULT"),
    };

    private static string ResultDecoder(JsonElement node, string nestedName, JsonElement mappings) => node.GetProperty("kind").GetString() switch
    {
        "object" => $"Decode{nestedName}(item, errors, complete)",
        "scalar" => ScalarDecoder(node.GetProperty("type").GetString()!, mappings),
        _ => throw Error("DOTNET_SDK_GENERATOR_UNSUPPORTED_RESULT"),
    };

    private static string ScalarType(string type, JsonElement mappings) => type switch
    {
        "Boolean" => "bool",
        "Int32" => "int",
        "Float64" => "double",
        "Int64" => "Int64Value",
        "UInt64" => "UInt64Value",
        "BigInt" => "BigIntegerValue",
        "Decimal" => "DecimalValue",
        "Timestamp" => "TimestampValue",
        "Duration" => "DurationValue",
        "UUID" => "UuidValue",
        "Bytes" => "BytesValue",
        "String" or "ID" => "string",
        "StringList" => "IReadOnlyList<string>",
        "StringMap" => "IReadOnlyDictionary<string, string>",
        _ when mappings.TryGetProperty(type, out var mapping) && mapping.GetString() == "lossless-decimal-string" => "DecimalValue",
        _ => SafeSymbol(type),
    };

    private static string ScalarDecoder(string type, JsonElement mappings) => type switch
    {
        "Boolean" => "OperationResultDecoder.Boolean(item)",
        "Int32" => "OperationResultDecoder.Int32(item)",
        "Float64" => "OperationResultDecoder.Float64(item)",
        "String" or "ID" => "OperationResultDecoder.String(item)",
        "Int64" => "Int64Value.Parse(OperationResultDecoder.String(item))",
        "UInt64" => "UInt64Value.Parse(OperationResultDecoder.String(item))",
        "BigInt" => "BigIntegerValue.Parse(OperationResultDecoder.String(item))",
        "Decimal" => "DecimalValue.Parse(OperationResultDecoder.String(item))",
        "Timestamp" => "TimestampValue.Parse(OperationResultDecoder.String(item))",
        "Duration" => "DurationValue.Parse(OperationResultDecoder.String(item))",
        "UUID" => "UuidValue.Parse(OperationResultDecoder.String(item))",
        "Bytes" => "BytesValue.Parse(OperationResultDecoder.String(item))",
        _ when mappings.TryGetProperty(type, out var mapping) && mapping.GetString() == "lossless-decimal-string" => "DecimalValue.Parse(OperationResultDecoder.String(item))",
        _ => throw Error("DOTNET_SDK_GENERATOR_UNMAPPED_SCALAR"),
    };

    private static void ValidateSchema(JsonElement schema, JsonElement mappings)
    {
        var names = new HashSet<string>(StringComparer.Ordinal);
        foreach (var descriptor in schema.GetProperty("types").EnumerateArray())
        {
            var name = SafeSymbol(descriptor.GetProperty("name").GetString()!);
            if (!names.Add(name))
            {
                throw Error("DOTNET_SDK_GENERATOR_SYMBOL_COLLISION");
            }

            if (descriptor.GetProperty("kind").GetString() == "scalar" && !mappings.TryGetProperty(descriptor.GetProperty("id").GetString()!, out _))
            {
                throw Error("DOTNET_SDK_GENERATOR_UNMAPPED_SCALAR");
            }
        }
    }

    private static JsonDocument Parse(ReadOnlySpan<byte> value)
    {
        try
        {
            return JsonDocument.Parse(value.ToArray(), new JsonDocumentOptions { AllowTrailingCommas = false, CommentHandling = JsonCommentHandling.Disallow, MaxDepth = 128 });
        }
        catch (JsonException exception)
        {
            throw Error("DOTNET_SDK_GENERATOR_INVALID_INPUT", exception);
        }
    }

    private static void RequireString(JsonElement value, string name, string expected, string code)
    {
        if (!value.TryGetProperty(name, out var property) || property.ValueKind != JsonValueKind.String || property.GetString() != expected)
        {
            throw Error(code);
        }
    }

    private static string SafeSymbol(string value)
    {
        if (!IdentifierPattern().IsMatch(value))
        {
            throw Error("DOTNET_SDK_GENERATOR_INVALID_SYMBOL");
        }

        var result = char.ToUpperInvariant(value[0]) + value[1..];
        return Reserved.Contains(result.ToLowerInvariant()) ? result + "_" : result;
    }

    private static void RenderDocumentation(List<string> lines, JsonElement value, string indent)
    {
        if (!value.TryGetProperty("description", out var description) || description.ValueKind != JsonValueKind.String)
        {
            return;
        }

        var safe = description.GetString()!.Replace("\r", " ", StringComparison.Ordinal).Replace("\n", " ", StringComparison.Ordinal)
            .Replace("&", "&amp;", StringComparison.Ordinal).Replace("<", "&lt;", StringComparison.Ordinal).Replace(">", "&gt;", StringComparison.Ordinal);
        lines.Add($"{indent}/// <summary>{safe}</summary>");
    }

    private static string Literal(string value) => JsonSerializer.Serialize(value);

    private static void WriteContained(string root, string relativePath, string content)
    {
        if (Path.IsPathRooted(relativePath) || relativePath.Split(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar).Contains("..", StringComparer.Ordinal))
        {
            throw Error("GENERATOR_PATH_ESCAPE");
        }

        var destination = Path.GetFullPath(Path.Combine(root, relativePath));
        if (!destination.StartsWith(root + Path.DirectorySeparatorChar, StringComparison.Ordinal))
        {
            throw Error("GENERATOR_PATH_ESCAPE");
        }

        var temporary = destination + ".tmp";
        File.WriteAllText(temporary, content, new UTF8Encoding(encoderShouldEmitUTF8Identifier: false));
        File.Move(temporary, destination, overwrite: true);
    }

    private static GeneratorException Error(string code, Exception? innerException = null) => new(code, innerException);
}

internal sealed record GeneratedArtifacts(string Source, string Manifest);

internal sealed class GeneratorException : Exception
{
    internal GeneratorException(string code, Exception? innerException = null)
        : base(code, innerException) => Code = code;

    internal string Code { get; }
}
