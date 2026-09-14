// Package sdkgen generates deterministic Swift Codable bindings from the
// language-neutral Naatre generator model and canonical reference output.
package sdkgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/protocol"
)

const GeneratorVersion = "naatre.generator.swift-sdk-1"

const maxInputBytes = 4 << 20

type Error struct{ Code string }

func (e *Error) Error() string { return "Swift SDK generation failed: " + e.Code }

type Artifacts struct {
	Source   []byte
	Manifest []byte
}

type model struct {
	Version          string           `json:"version"`
	ProtocolVersion  string           `json:"protocolVersion"`
	CanonicalVersion string           `json:"canonicalVersion"`
	Schema           schemaModel      `json:"schema"`
	Configuration    configuration    `json:"configuration"`
	Operations       []modelOperation `json:"operations"`
}

type configuration struct {
	ScalarMappings map[string]string `json:"scalarMappings"`
}

type schemaModel struct {
	Types []schemaType `json:"types"`
}

type schemaType struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Kind           string         `json:"kind"`
	Open           bool           `json:"open"`
	Description    string         `json:"description"`
	Fields         []schemaField  `json:"fields"`
	EnumMembers    []schemaMember `json:"enumMembers"`
	VariantMembers []schemaMember `json:"variantMembers"`
	Element        string         `json:"element"`
}

type schemaField struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Required   bool   `json:"required"`
	Nullable   bool   `json:"nullable"`
	Deprecated any    `json:"deprecation"`
}

type schemaMember struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type variable struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Nullable bool   `json:"nullable"`
}

type modelOperation struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Document    json.RawMessage `json:"document"`
	Variables   []variable      `json:"variables"`
	Result      resultNode      `json:"result"`
}

type resultNode struct {
	Kind     string          `json:"kind"`
	Type     string          `json:"type"`
	Nullable bool            `json:"nullable"`
	Fields   []selectedField `json:"fields"`
	Element  *resultNode     `json:"element"`
}

type selectedField struct {
	Name     string     `json:"name"`
	Presence string     `json:"presence"`
	Result   resultNode `json:"result"`
}

type referenceOutput struct {
	ModelVersion     string               `json:"modelVersion"`
	GeneratorVersion string               `json:"generatorVersion"`
	ProtocolVersion  string               `json:"protocolVersion"`
	CanonicalVersion string               `json:"canonicalVersion"`
	Operations       []referenceOperation `json:"operations"`
}

type referenceOperation struct {
	Name      string          `json:"name"`
	Symbol    string          `json:"symbol"`
	Persisted protocol.Digest `json:"persisted"`
}

type binding struct {
	modelOperation
	Symbol    string
	Kind      protocol.OperationKind
	Persisted protocol.Digest
}

type manifestWire struct {
	Profile          string                  `json:"profile"`
	Version          string                  `json:"version"`
	ProtocolVersion  string                  `json:"protocolVersion"`
	CanonicalVersion string                  `json:"canonicalVersion"`
	Operations       []manifestOperationWire `json:"operations"`
}

type manifestOperationWire struct {
	Name      string                 `json:"name"`
	Kind      protocol.OperationKind `json:"kind"`
	Persisted protocol.Digest        `json:"persisted"`
}

func Generate(modelBytes, referenceBytes []byte) (Artifacts, error) {
	parsed, reference, err := decodeInputs(modelBytes, referenceBytes)
	if err != nil {
		return Artifacts{}, err
	}
	bindings, err := buildBindings(parsed, reference)
	if err != nil {
		return Artifacts{}, err
	}
	source, err := generateSource(parsed, bindings)
	if err != nil {
		return Artifacts{}, err
	}
	manifest, err := generateManifest(bindings)
	if err != nil {
		return Artifacts{}, failure("MANIFEST_FAILED")
	}
	return Artifacts{Source: source, Manifest: manifest}, nil
}

func decodeInputs(modelBytes, referenceBytes []byte) (model, referenceOutput, error) {
	if len(modelBytes) == 0 || len(modelBytes) > maxInputBytes || len(referenceBytes) == 0 || len(referenceBytes) > maxInputBytes {
		return model{}, referenceOutput{}, failure("INPUT_LIMIT")
	}
	expected, err := generator.Generate(modelBytes)
	if err != nil {
		return model{}, referenceOutput{}, translateModelError(err)
	}
	if !bytes.Equal(expected, referenceBytes) {
		return model{}, referenceOutput{}, failure("REFERENCE_DRIFT")
	}
	var parsed model
	var reference referenceOutput
	if json.Unmarshal(modelBytes, &parsed) != nil || json.Unmarshal(referenceBytes, &reference) != nil {
		return model{}, referenceOutput{}, failure("INVALID_INPUT")
	}
	if parsed.Version != generator.ModelVersion || reference.ModelVersion != generator.ModelVersion ||
		reference.GeneratorVersion != generator.GeneratorVersion || parsed.ProtocolVersion != "1" ||
		reference.ProtocolVersion != parsed.ProtocolVersion || parsed.CanonicalVersion != "c14n-1" ||
		reference.CanonicalVersion != parsed.CanonicalVersion {
		return model{}, referenceOutput{}, failure("VERSION_SKEW")
	}
	return parsed, reference, nil
}

func translateModelError(err error) error {
	var modelError *generator.Error
	if errors.As(err, &modelError) {
		for _, diagnostic := range modelError.Diagnostics() {
			if diagnostic.Code == "GENERATOR_UNMAPPED_SCALAR" {
				return failure("UNMAPPED_SCALAR")
			}
		}
	}
	return failure("INVALID_MODEL")
}

func buildBindings(parsed model, reference referenceOutput) ([]binding, error) {
	models, err := indexOperations(parsed.Operations)
	if err != nil {
		return nil, err
	}
	if len(models) != len(reference.Operations) {
		return nil, failure("REFERENCE_DRIFT")
	}
	result := make([]binding, 0, len(reference.Operations))
	seen := make(map[string]struct{}, len(reference.Operations))
	for _, output := range reference.Operations {
		operation, ok := models[output.Name]
		symbol, valid := swiftType(output.Symbol)
		_, duplicate := seen[output.Symbol]
		if !ok || duplicate || !valid || symbol != output.Symbol {
			return nil, failure("INVALID_SYMBOL")
		}
		item, err := bindOperation(operation, output)
		if err != nil {
			return nil, err
		}
		seen[output.Symbol] = struct{}{}
		result = append(result, item)
	}
	return result, nil
}

func indexOperations(operations []modelOperation) (map[string]modelOperation, error) {
	indexed := make(map[string]modelOperation, len(operations))
	for _, operation := range operations {
		if operation.Name == "" {
			return nil, failure("INVALID_OPERATION")
		}
		if _, exists := indexed[operation.Name]; exists {
			return nil, failure("INVALID_OPERATION")
		}
		indexed[operation.Name] = operation
	}
	return indexed, nil
}

func bindOperation(operation modelOperation, reference referenceOperation) (binding, error) {
	document, err := protocol.DecodeDocument(operation.Document, protocol.Limits{})
	if err != nil {
		return binding{}, failure("INVALID_OPERATION")
	}
	operations := document.Operations()
	index := slices.IndexFunc(operations, func(candidate protocol.Operation) bool { return candidate.Name() == operation.Name })
	if index < 0 {
		return binding{}, failure("INVALID_OPERATION")
	}
	return binding{modelOperation: operation, Symbol: reference.Symbol, Kind: operations[index].Kind(), Persisted: reference.Persisted}, nil
}

func generateManifest(bindings []binding) ([]byte, error) {
	operations := make([]manifestOperationWire, 0, len(bindings))
	for _, item := range bindings {
		operations = append(operations, manifestOperationWire{Name: item.Name, Kind: item.Kind, Persisted: item.Persisted})
	}
	wire := manifestWire{Profile: "sdk.swift.core-1", Version: "1", ProtocolVersion: "1", CanonicalVersion: "c14n-1", Operations: operations}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	return protocol.CanonicalizeJSON(encoded, protocol.Limits{MaxBytes: maxInputBytes})
}

func generateSource(parsed model, bindings []binding) ([]byte, error) {
	types := make(map[string]schemaType, len(parsed.Schema.Types))
	ordered := slices.Clone(parsed.Schema.Types)
	slices.SortFunc(ordered, func(a, b schemaType) int { return strings.Compare(a.ID, b.ID) })
	for _, descriptor := range ordered {
		if descriptor.ID == "" || types[descriptor.ID].ID != "" {
			return nil, failure("INVALID_SCHEMA")
		}
		types[descriptor.ID] = descriptor
	}
	context := sourceContext{types: types, mappings: parsed.Configuration.ScalarMappings}
	var source strings.Builder
	source.WriteString("// Code generated by naatre-swift-sdk-generator; DO NOT EDIT.\n\nimport Foundation\nimport NaatreCore\n\n")
	source.WriteString("public let swiftGeneratorVersion = \"")
	source.WriteString(GeneratorVersion)
	source.WriteString("\"\n\n")
	for _, descriptor := range ordered {
		if err := context.writeSchemaType(&source, descriptor); err != nil {
			return nil, err
		}
	}
	for _, item := range bindings {
		if err := context.writeOperation(&source, item); err != nil {
			return nil, err
		}
	}
	return []byte(source.String()), nil
}

type sourceContext struct {
	types    map[string]schemaType
	mappings map[string]string
}

func (c sourceContext) writeSchemaType(source *strings.Builder, descriptor schemaType) error {
	name, ok := swiftType(descriptor.Name)
	if !ok {
		name, ok = swiftType(descriptor.ID)
	}
	if !ok {
		return failure("INVALID_SYMBOL")
	}
	switch descriptor.Kind {
	case "scalar":
		typeName, err := c.scalarType(descriptor.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(source, "public typealias %s = %s\n\n", name, typeName)
	case "enum":
		if !descriptor.Open {
			return failure("UNSUPPORTED_SCHEMA")
		}
		fmt.Fprintf(source, "public enum %s: Codable, Hashable, Sendable {\n", name)
		members := slices.Clone(descriptor.EnumMembers)
		slices.SortFunc(members, func(a, b schemaMember) int { return strings.Compare(a.Name, b.Name) })
		for _, member := range members {
			fmt.Fprintf(source, "    case %s\n", swiftProperty(member.Name))
		}
		source.WriteString("    case unknown(String)\n\n    public init(from decoder: Decoder) throws {\n        let value = try decoder.singleValueContainer().decode(String.self)\n        switch value {\n")
		for _, member := range members {
			fmt.Fprintf(source, "        case %s: self = .%s\n", strconv.Quote(member.Name), swiftProperty(member.Name))
		}
		source.WriteString("        default: self = .unknown(value)\n        }\n    }\n\n    public func encode(to encoder: Encoder) throws {\n        var container = encoder.singleValueContainer()\n        switch self {\n")
		for _, member := range members {
			fmt.Fprintf(source, "        case .%s: try container.encode(%s)\n", swiftProperty(member.Name), strconv.Quote(member.Name))
		}
		source.WriteString("        case let .unknown(value): try container.encode(value)\n        }\n    }\n}\n\n")
	case "object":
		fmt.Fprintf(source, "public struct %s: Codable, Sendable {\n", name)
		for _, field := range descriptor.Fields {
			fieldType, err := c.namedType(field.Type)
			if err != nil {
				return err
			}
			fmt.Fprintf(source, "    public var %s: %s%s\n", swiftProperty(field.Name), fieldType, map[bool]string{true: "", false: "?"}[field.Required])
		}
		source.WriteString("}\n\n")
	case "union":
		fmt.Fprintf(source, "public typealias %s = OpenVariant\n\n", name)
	case "list":
		element, err := c.namedType(descriptor.Element)
		if err != nil {
			return err
		}
		fmt.Fprintf(source, "public typealias %s = [%s]\n\n", name, element)
	case "map":
		element, err := c.namedType(descriptor.Element)
		if err != nil {
			return err
		}
		fmt.Fprintf(source, "public typealias %s = [String: %s]\n\n", name, element)
	case "input-object", "oneof":
		// Operation variables are emitted with explicit Input presence. Nested
		// input objects use the same reflection-free keyed representation.
		fmt.Fprintf(source, "public struct %s: Codable, Sendable {\n", name)
		for _, field := range descriptor.Fields {
			fieldType, err := c.namedType(field.Type)
			if err != nil {
				return err
			}
			fmt.Fprintf(source, "    public var %s: %s?\n", swiftProperty(field.Name), fieldType)
		}
		source.WriteString("}\n\n")
	default:
		return failure("UNSUPPORTED_SCHEMA")
	}
	return nil
}

func (c sourceContext) writeOperation(source *strings.Builder, item binding) error {
	variablesName := item.Symbol + "Variables"
	resultName := item.Symbol + "Result"
	if err := c.writeVariables(source, variablesName, item.Variables); err != nil {
		return err
	}
	if err := c.writeResultType(source, resultName, item.Result); err != nil {
		return err
	}
	fmt.Fprintf(source, "public func make%s(_ variables: %s) throws -> NaatreCore.Operation<%s, %s> {\n", item.Symbol, variablesName, variablesName, resultName)
	fmt.Fprintf(source, "    let persisted = try PersistedReference(digest: %s)\n", strconv.Quote(item.Persisted.Hex))
	fmt.Fprintf(source, "    return try NaatreCore.Operation(name: %s, kind: .%s, persisted: persisted, variables: variables)\n}\n\n", strconv.Quote(item.Name), item.Kind)
	fmt.Fprintf(source, "public func swiftManifest() throws -> Manifest {\n    try Manifest(operations: [ManifestOperation(name: %s, kind: .%s, persisted: try PersistedReference(digest: %s))])\n}\n", strconv.Quote(item.Name), item.Kind, strconv.Quote(item.Persisted.Hex))
	return nil
}

func (c sourceContext) writeVariables(source *strings.Builder, name string, variables []variable) error {
	fmt.Fprintf(source, "public struct %s: Codable, Sendable {\n", name)
	for _, variable := range variables {
		typeName, err := c.variableType(variable)
		if err != nil {
			return err
		}
		fmt.Fprintf(source, "    public let %s: %s\n", swiftProperty(variable.Name), typeName)
	}
	source.WriteString("\n    public init(")
	for index, variable := range variables {
		if index > 0 {
			source.WriteString(", ")
		}
		typeName, err := c.variableType(variable)
		if err != nil {
			return err
		}
		defaultValue := ""
		if !variable.Required {
			defaultValue = " = .missing"
		}
		fmt.Fprintf(source, "%s: %s%s", swiftProperty(variable.Name), typeName, defaultValue)
	}
	source.WriteString(") {\n")
	for _, variable := range variables {
		fmt.Fprintf(source, "        self.%s = %s\n", swiftProperty(variable.Name), swiftProperty(variable.Name))
	}
	source.WriteString("    }\n\n    enum CodingKeys: String, CodingKey {\n")
	for _, variable := range variables {
		fmt.Fprintf(source, "        case %s = %s\n", swiftProperty(variable.Name), strconv.Quote(variable.Name))
	}
	source.WriteString("    }\n\n    public init(from decoder: Decoder) throws {\n        let container = try decoder.container(keyedBy: CodingKeys.self)\n")
	for _, variable := range variables {
		c.writeVariableDecoding(source, variable)
	}
	source.WriteString("    }\n\n    public func encode(to encoder: Encoder) throws {\n        var container = encoder.container(keyedBy: CodingKeys.self)\n")
	for _, variable := range variables {
		writeVariableEncoding(source, variable)
	}
	source.WriteString("    }\n}\n\n")
	return nil
}

func (c sourceContext) variableType(variable variable) (string, error) {
	typeName, err := c.namedType(variable.Type)
	if err != nil {
		return "", err
	}
	if !variable.Required {
		return "Input<" + typeName + ">", nil
	}
	if variable.Nullable {
		typeName += "?"
	}
	return typeName, nil
}

func (c sourceContext) writeVariableDecoding(source *strings.Builder, variable variable) {
	property := swiftProperty(variable.Name)
	typeName, _ := c.namedType(variable.Type)
	if !variable.Required {
		fmt.Fprintf(source, "        %s = try Input<%s>.decode(from: container, forKey: .%s, allowNull: true)\n", property, typeName, property)
		return
	}
	if variable.Nullable {
		fmt.Fprintf(source, "        guard container.contains(.%s) else { throw ClientFailure(.protocolInvalid) }\n", property)
		fmt.Fprintf(source, "        %s = try container.decodeIfPresent(%s.self, forKey: .%s)\n", property, typeName, property)
		return
	}
	fmt.Fprintf(source, "        %s = try container.decode(%s.self, forKey: .%s)\n", property, typeName, property)
}

func writeVariableEncoding(source *strings.Builder, variable variable) {
	property := swiftProperty(variable.Name)
	if variable.Required {
		fmt.Fprintf(source, "        try container.encode(%s, forKey: .%s)\n", property, property)
		return
	}
	fmt.Fprintf(source, "        switch %s {\n", property)
	source.WriteString("        case .missing: break\n")
	fmt.Fprintf(source, "        case .null: try container.encodeNil(forKey: .%s)\n", property)
	fmt.Fprintf(source, "        case let .value(value): try container.encode(value, forKey: .%s)\n", property)
	source.WriteString("        }\n")
}

func (c sourceContext) writeResultType(source *strings.Builder, name string, node resultNode) error {
	if node.Kind != "object" {
		return failure("UNSUPPORTED_RESULT")
	}
	fmt.Fprintf(source, "public struct %s: Codable, Sendable {\n", name)
	for _, field := range node.Fields {
		typeName, err := c.resultType(name+mustSwiftType(field.Name), field.Result)
		if err != nil {
			return err
		}
		fmt.Fprintf(source, "    public let %s: Selected<%s>\n", swiftProperty(field.Name), typeName)
	}
	source.WriteString("\n    enum CodingKeys: String, CodingKey {\n")
	for _, field := range node.Fields {
		fmt.Fprintf(source, "        case %s = %s\n", swiftProperty(field.Name), strconv.Quote(field.Name))
	}
	source.WriteString("    }\n\n    public init(from decoder: Decoder) throws {\n        let container = try decoder.container(keyedBy: CodingKeys.self)\n")
	for _, field := range node.Fields {
		fmt.Fprintf(source, "        %s = try Selected.decode(from: container, forKey: .%s, pendingWhenMissing: %t)\n", swiftProperty(field.Name), swiftProperty(field.Name), field.Presence == "pending")
	}
	source.WriteString("    }\n\n    public func encode(to encoder: Encoder) throws {\n        var container = encoder.container(keyedBy: CodingKeys.self)\n")
	for _, field := range node.Fields {
		fmt.Fprintf(source, "        try %s.encode(to: &container, forKey: .%s)\n", swiftProperty(field.Name), swiftProperty(field.Name))
	}
	source.WriteString("    }\n}\n\n")
	for _, field := range node.Fields {
		if field.Result.Kind == "object" {
			if err := c.writeResultType(source, name+mustSwiftType(field.Name), field.Result); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c sourceContext) resultType(path string, node resultNode) (string, error) {
	switch node.Kind {
	case "object":
		return path, nil
	case "scalar", "enum":
		name, err := c.namedType(node.Type)
		return name, err
	case "union":
		return "OpenVariant", nil
	case "list":
		if node.Element == nil {
			return "", failure("UNSUPPORTED_RESULT")
		}
		item, err := c.resultType(path+"Item", *node.Element)
		return "[" + item + "]", err
	case "map":
		if node.Element == nil {
			return "", failure("UNSUPPORTED_RESULT")
		}
		item, err := c.resultType(path+"Value", *node.Element)
		return "[String: " + item + "]", err
	default:
		return "", failure("UNSUPPORTED_RESULT")
	}
}

func (c sourceContext) namedType(id string) (string, error) {
	switch id {
	case "String", "ID":
		return "String", nil
	case "Boolean":
		return "Bool", nil
	case "Int32":
		return "Int32", nil
	case "Float64":
		return "Double", nil
	case "Int64":
		return "NaatreInt64", nil
	case "UInt64":
		return "NaatreUInt64", nil
	case "BigInt":
		return "NaatreBigInt", nil
	case "Decimal":
		return "NaatreDecimal", nil
	case "Timestamp":
		return "NaatreTimestamp", nil
	case "Duration":
		return "NaatreDuration", nil
	case "UUID":
		return "NaatreUUID", nil
	case "Bytes":
		return "NaatreBytes", nil
	case "URL":
		return "NaatreURL", nil
	case "StringList":
		return "[String]", nil
	case "StringMap":
		return "[String: String]", nil
	}
	if descriptor, ok := c.types[id]; ok {
		name, valid := swiftType(descriptor.Name)
		if !valid {
			name, valid = swiftType(descriptor.ID)
		}
		if valid {
			return name, nil
		}
	}
	return c.scalarType(id)
}

func (c sourceContext) scalarType(id string) (string, error) {
	switch c.mappings[id] {
	case "lossless-decimal-string":
		return "NaatreDecimal", nil
	case "string":
		return "String", nil
	case "int64":
		return "NaatreInt64", nil
	case "uint64":
		return "NaatreUInt64", nil
	case "bigint":
		return "NaatreBigInt", nil
	case "timestamp":
		return "NaatreTimestamp", nil
	case "duration":
		return "NaatreDuration", nil
	case "uuid":
		return "NaatreUUID", nil
	case "bytes":
		return "NaatreBytes", nil
	case "url":
		return "NaatreURL", nil
	default:
		return "", failure("UNMAPPED_SCALAR")
	}
}

var swiftKeywords = map[string]bool{"associatedtype": true, "class": true, "deinit": true, "enum": true, "extension": true, "fileprivate": true, "func": true, "import": true, "init": true, "inout": true, "internal": true, "let": true, "open": true, "operator": true, "private": true, "protocol": true, "public": true, "repeat": true, "static": true, "struct": true, "subscript": true, "typealias": true, "var": true, "break": true, "case": true, "continue": true, "default": true, "defer": true, "do": true, "else": true, "fallthrough": true, "for": true, "guard": true, "if": true, "in": true, "return": true, "switch": true, "where": true, "while": true, "as": true, "Any": true, "catch": true, "false": true, "is": true, "nil": true, "rethrows": true, "super": true, "self": true, "Self": true, "throw": true, "throws": true, "true": true, "try": true}

func swiftType(value string) (string, bool) {
	symbol, ok := generator.PortableSymbol(value)
	if !ok || swiftKeywords[symbol] {
		return "", false
	}
	return symbol, true
}

func mustSwiftType(value string) string {
	symbol, _ := swiftType(value)
	return symbol
}

func swiftProperty(value string) string {
	typeName, ok := swiftType(value)
	if !ok {
		return "`invalid`"
	}
	property := strings.ToLower(typeName[:1]) + typeName[1:]
	if strings.ToUpper(typeName) == typeName {
		property = strings.ToLower(typeName)
	}
	if swiftKeywords[property] {
		return "`" + property + "`"
	}
	return property
}

func failure(reason string) *Error {
	return &Error{Code: "SWIFT_SDK_GENERATOR_" + reason}
}
