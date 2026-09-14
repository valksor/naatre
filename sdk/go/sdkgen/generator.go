// Package sdkgen generates deterministic Go operation bindings from the
// language-neutral Naatre generator model and its canonical reference output.
package sdkgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"strconv"
	"strings"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/protocol"
)

const GeneratorVersion = "naatre.generator.go-sdk-1"

const maxInputBytes = 4 << 20

// Error reports a stable, non-sensitive generator failure code.
type Error struct{ Code string }

func (e *Error) Error() string { return "Go SDK generation failed: " + e.Code }

// Artifacts are the byte-identical checked-in outputs of Generate.
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
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Open bool   `json:"open"`
}

type modelOperation struct {
	Name     string          `json:"name"`
	Document json.RawMessage `json:"document"`
	Result   resultNode      `json:"result"`
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

type operationBinding struct {
	Name      string
	Symbol    string
	Kind      protocol.OperationKind
	Persisted protocol.Digest
	Result    resultNode
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
	Persisted manifestPersistedWire  `json:"persisted"`
}

type manifestPersistedWire struct {
	Algorithm        string `json:"algorithm"`
	CanonicalVersion string `json:"canonicalVersion"`
	Digest           string `json:"digest"`
}

// Generate validates that referenceBytes are exactly the canonical shared
// output for modelBytes before producing Go source and a persisted manifest.
func Generate(modelBytes, referenceBytes []byte) (Artifacts, error) {
	if len(modelBytes) == 0 || len(modelBytes) > maxInputBytes || len(referenceBytes) == 0 || len(referenceBytes) > maxInputBytes {
		return Artifacts{}, generationError("GO_SDK_GENERATOR_INPUT_LIMIT")
	}
	expected, err := generator.Generate(modelBytes)
	if err != nil {
		return Artifacts{}, generationError("GO_SDK_GENERATOR_INVALID_MODEL")
	}
	if !bytes.Equal(expected, referenceBytes) {
		return Artifacts{}, generationError("GO_SDK_GENERATOR_REFERENCE_DRIFT")
	}

	var parsed model
	var reference referenceOutput
	if err := json.Unmarshal(modelBytes, &parsed); err != nil {
		return Artifacts{}, generationError("GO_SDK_GENERATOR_INVALID_MODEL")
	}
	if err := json.Unmarshal(referenceBytes, &reference); err != nil {
		return Artifacts{}, generationError("GO_SDK_GENERATOR_INVALID_REFERENCE")
	}
	if parsed.Version != generator.ModelVersion || reference.ModelVersion != generator.ModelVersion ||
		reference.GeneratorVersion != generator.GeneratorVersion || parsed.ProtocolVersion != "1" ||
		reference.ProtocolVersion != parsed.ProtocolVersion || parsed.CanonicalVersion != "c14n-1" ||
		reference.CanonicalVersion != parsed.CanonicalVersion {
		return Artifacts{}, generationError("GO_SDK_GENERATOR_VERSION_SKEW")
	}

	bindings, err := buildBindings(parsed, reference)
	if err != nil {
		return Artifacts{}, err
	}
	source, err := generateSource(bindings, parsed.Schema, parsed.Configuration)
	if err != nil {
		return Artifacts{}, err
	}
	manifest, err := generateManifest(bindings)
	if err != nil {
		return Artifacts{}, generationError("GO_SDK_GENERATOR_MANIFEST_FAILED")
	}
	return Artifacts{Source: source, Manifest: manifest}, nil
}

func buildBindings(parsed model, reference referenceOutput) ([]operationBinding, error) {
	models := make(map[string]modelOperation, len(parsed.Operations))
	for _, operation := range parsed.Operations {
		if operation.Name == "" || models[operation.Name].Name != "" {
			return nil, generationError("GO_SDK_GENERATOR_INVALID_OPERATION")
		}
		models[operation.Name] = operation
	}
	if len(reference.Operations) != len(models) {
		return nil, generationError("GO_SDK_GENERATOR_REFERENCE_DRIFT")
	}
	bindings := make([]operationBinding, 0, len(reference.Operations))
	seenSymbols := make(map[string]bool, len(reference.Operations))
	for _, output := range reference.Operations {
		operation, found := models[output.Name]
		if !found || !validGoIdentifier(output.Symbol) || seenSymbols[output.Symbol] {
			return nil, generationError("GO_SDK_GENERATOR_INVALID_SYMBOL")
		}
		seenSymbols[output.Symbol] = true
		document, err := protocol.DecodeDocument(operation.Document, protocol.Limits{})
		if err != nil {
			return nil, generationError("GO_SDK_GENERATOR_INVALID_OPERATION")
		}
		kind, found := operationKind(document, operation.Name)
		if !found {
			return nil, generationError("GO_SDK_GENERATOR_INVALID_OPERATION")
		}
		bindings = append(bindings, operationBinding{
			Name: operation.Name, Symbol: output.Symbol, Kind: kind, Persisted: output.Persisted, Result: operation.Result,
		})
	}
	return bindings, nil
}

func operationKind(document protocol.Document, name string) (protocol.OperationKind, bool) {
	for _, operation := range document.Operations() {
		if operation.Name() == name {
			return operation.Kind(), true
		}
	}
	return "", false
}

func generateManifest(bindings []operationBinding) ([]byte, error) {
	wire := manifestWire{
		Profile: "sdk.go.operations-1", Version: "1", ProtocolVersion: "1", CanonicalVersion: "c14n-1",
		Operations: make([]manifestOperationWire, len(bindings)),
	}
	for index, binding := range bindings {
		wire.Operations[index] = manifestOperationWire{
			Name: binding.Name, Kind: binding.Kind,
			Persisted: manifestPersistedWire{
				Algorithm: binding.Persisted.Algorithm, CanonicalVersion: binding.Persisted.CanonicalVersion, Digest: binding.Persisted.Hex,
			},
		}
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	return protocol.CanonicalizeJSON(encoded, protocol.Limits{MaxBytes: maxInputBytes})
}

func generateSource(bindings []operationBinding, schema schemaModel, configuration configuration) ([]byte, error) {
	types := make(map[string]schemaType, len(schema.Types))
	for _, descriptor := range schema.Types {
		if descriptor.ID == "" || types[descriptor.ID].ID != "" {
			return nil, generationError("GO_SDK_GENERATOR_INVALID_SCHEMA")
		}
		types[descriptor.ID] = descriptor
	}
	context := sourceContext{schemaTypes: types, scalarMappings: configuration.ScalarMappings}
	for _, binding := range bindings {
		if err := context.collectResultTypes(binding.Symbol+"Result", binding.Result); err != nil {
			return nil, err
		}
	}

	var source strings.Builder
	source.WriteString("// Code generated by naatre-go-sdk-generator; DO NOT EDIT.\n\n")
	source.WriteString("package generated\n\n")
	source.WriteString("import (\n\t\"encoding/json\"\n\n\t\"github.com/valksor/naatre/client\"\n\t\"github.com/valksor/naatre/protocol\"\n)\n\n")
	source.WriteString("const GeneratorVersion = \"")
	source.WriteString(GeneratorVersion)
	source.WriteString("\"\n\n")

	for _, name := range context.typeOrder {
		definition := context.resultTypes[name]
		writeResultType(&source, name, definition, &context)
	}
	for _, binding := range bindings {
		writeOperation(&source, binding)
	}
	writeManifestFunction(&source, bindings)

	formatted, err := format.Source([]byte(source.String()))
	if err != nil {
		return nil, generationError("GO_SDK_GENERATOR_SOURCE_FAILED")
	}
	return formatted, nil
}

type sourceContext struct {
	schemaTypes    map[string]schemaType
	scalarMappings map[string]string
	resultTypes    map[string]resultNode
	typeOrder      []string
}

func (c *sourceContext) collectResultTypes(name string, node resultNode) error {
	if c.resultTypes == nil {
		c.resultTypes = make(map[string]resultNode)
	}
	if node.Kind != "object" {
		return generationError("GO_SDK_GENERATOR_UNSUPPORTED_RESULT")
	}
	if _, exists := c.resultTypes[name]; exists {
		return generationError("GO_SDK_GENERATOR_SYMBOL_COLLISION")
	}
	c.resultTypes[name] = node
	c.typeOrder = append(c.typeOrder, name)
	seen := make(map[string]bool, len(node.Fields))
	for _, field := range node.Fields {
		symbol, ok := goSymbol(field.Name)
		if !ok || seen[symbol] || !validPresence(field.Presence) {
			return generationError("GO_SDK_GENERATOR_INVALID_RESULT")
		}
		seen[symbol] = true
		if err := c.collectNestedResultTypes(name+symbol, field.Result); err != nil {
			return err
		}
	}
	return nil
}

func (c *sourceContext) collectNestedResultTypes(path string, node resultNode) error {
	switch node.Kind {
	case "object":
		return c.collectResultTypes(path, node)
	case "list":
		if node.Element == nil {
			return generationError("GO_SDK_GENERATOR_UNSUPPORTED_RESULT")
		}
		if err := c.collectNestedResultTypes(path+"Item", *node.Element); err != nil {
			return err
		}
	case "map":
		if node.Element == nil {
			return generationError("GO_SDK_GENERATOR_UNSUPPORTED_RESULT")
		}
		if err := c.collectNestedResultTypes(path+"Value", *node.Element); err != nil {
			return err
		}
	}
	_, err := c.goType(path, node)
	return err
}

func (c *sourceContext) goType(path string, node resultNode) (string, error) {
	switch node.Kind {
	case "object":
		return path, nil
	case "scalar":
		return c.scalarType(node.Type)
	case "enum":
		if descriptor, found := c.schemaTypes[node.Type]; found && descriptor.Kind == "enum" && descriptor.Open {
			return "string", nil
		}
		return "", generationError("GO_SDK_GENERATOR_UNSUPPORTED_RESULT")
	case "list":
		if node.Element == nil {
			return "", generationError("GO_SDK_GENERATOR_UNSUPPORTED_RESULT")
		}
		element, err := c.goType(path+"Item", *node.Element)
		if err != nil {
			return "", err
		}
		return "[]" + element, nil
	case "map":
		if node.Element == nil {
			return "", generationError("GO_SDK_GENERATOR_UNSUPPORTED_RESULT")
		}
		element, err := c.goType(path+"Value", *node.Element)
		if err != nil {
			return "", err
		}
		return "map[string]" + element, nil
	case "union":
		return "", generationError("GO_SDK_GENERATOR_UNSUPPORTED_RESULT")
	default:
		return "", generationError("GO_SDK_GENERATOR_UNSUPPORTED_RESULT")
	}
}

func (c *sourceContext) scalarType(typeID string) (string, error) {
	switch typeID {
	case "Boolean":
		return "bool", nil
	case "Int32":
		return "int32", nil
	case "Float64":
		return "float64", nil
	case "String", "ID", "Int64", "UInt64", "BigInt", "Decimal", "Timestamp", "Duration", "UUID", "Bytes":
		return "string", nil
	}
	switch c.scalarMappings[typeID] {
	case "lossless-decimal-string", "string":
		return "string", nil
	case "json-raw-message":
		return "json.RawMessage", nil
	default:
		return "", generationError("GO_SDK_GENERATOR_UNMAPPED_SCALAR")
	}
}

func writeResultType(source *strings.Builder, name string, node resultNode, context *sourceContext) {
	source.WriteString("type ")
	source.WriteString(name)
	source.WriteString(" struct {\n")
	for _, field := range node.Fields {
		symbol, _ := goSymbol(field.Name)
		fieldType, _ := context.goType(name+symbol, field.Result)
		source.WriteString("\t")
		source.WriteString(symbol)
		source.WriteString(" client.Selected[")
		source.WriteString(fieldType)
		source.WriteString("]\n")
	}
	source.WriteString("}\n\n")

	decoder := "decode" + name
	if strings.HasSuffix(name, "Result") && !strings.Contains(strings.TrimSuffix(name, "Result"), "Result") {
		decoder = "Decode" + name
	}
	source.WriteString("func ")
	source.WriteString(decoder)
	source.WriteString("(raw json.RawMessage) (")
	source.WriteString(name)
	source.WriteString(", error) {\n")
	source.WriteString("\tfields, err := client.DecodeResultObject(raw)\n\tif err != nil {\n\t\treturn ")
	source.WriteString(name)
	source.WriteString("{}, err\n\t}\n\tvar result ")
	source.WriteString(name)
	source.WriteString("\n")
	for index, field := range node.Fields {
		symbol, _ := goSymbol(field.Name)
		fieldType, _ := context.goType(name+symbol, field.Result)
		fmt.Fprintf(source, "\tfield%d, exists%d := fields[%s]\n", index, index, strconv.Quote(field.Name))
		fmt.Fprintf(source, "\tresult.%s, err = client.DecodeSelected[%s](field%d, exists%d, %t, %s)\n", symbol, fieldType, index, index, field.Presence == "pending", decoderExpression(name+symbol, field.Result, fieldType))
		source.WriteString("\tif err != nil {\n\t\treturn ")
		source.WriteString(name)
		source.WriteString("{}, err\n\t}\n")
	}
	source.WriteString("\treturn result, nil\n}\n\n")
}

func decoderExpression(path string, node resultNode, goType string) string {
	switch node.Kind {
	case "object":
		return "decode" + path
	case "list":
		elementType := strings.TrimPrefix(goType, "[]")
		return "func(raw json.RawMessage) ([]" + elementType + ", error) { return client.DecodeList(raw, " + decoderExpression(path+"Item", *node.Element, elementType) + ") }"
	case "map":
		elementType := strings.TrimPrefix(goType, "map[string]")
		return "func(raw json.RawMessage) (map[string]" + elementType + ", error) { return client.DecodeMap(raw, " + decoderExpression(path+"Value", *node.Element, elementType) + ") }"
	default:
		return "client.DecodeJSON[" + goType + "]"
	}
}

func writeOperation(source *strings.Builder, binding operationBinding) {
	resultName := binding.Symbol + "Result"
	source.WriteString("func New")
	source.WriteString(binding.Symbol)
	source.WriteString("() (client.Operation[")
	source.WriteString(resultName)
	source.WriteString("], error) {\n\trequest, err := client.NewPersistedOperation(protocol.PersistedReference{\n")
	fmt.Fprintf(source, "\t\tAlgorithm: %s, CanonicalVersion: %s, Digest: %s,\n", strconv.Quote(binding.Persisted.Algorithm), strconv.Quote(binding.Persisted.CanonicalVersion), strconv.Quote(binding.Persisted.Hex))
	fmt.Fprintf(source, "\t}, %s, protocol.%s)\n", strconv.Quote(binding.Name), kindSymbol(binding.Kind))
	source.WriteString("\tif err != nil {\n\t\treturn client.Operation[")
	source.WriteString(resultName)
	source.WriteString("]{}, err\n\t}\n\treturn client.NewOperation(request, Decode")
	source.WriteString(resultName)
	source.WriteString(")\n}\n\n")
}

func writeManifestFunction(source *strings.Builder, bindings []operationBinding) {
	source.WriteString("func Manifest() (client.Manifest, error) {\n\treturn client.NewManifest(\n")
	for _, binding := range bindings {
		source.WriteString("\t\tclient.ManifestOperation{\n")
		fmt.Fprintf(source, "\t\t\tName: %s, Kind: protocol.%s,\n", strconv.Quote(binding.Name), kindSymbol(binding.Kind))
		fmt.Fprintf(source, "\t\t\tPersisted: protocol.PersistedReference{Algorithm: %s, CanonicalVersion: %s, Digest: %s},\n", strconv.Quote(binding.Persisted.Algorithm), strconv.Quote(binding.Persisted.CanonicalVersion), strconv.Quote(binding.Persisted.Hex))
		source.WriteString("\t\t},\n")
	}
	source.WriteString("\t)\n}\n")
}

func kindSymbol(kind protocol.OperationKind) string {
	switch kind {
	case protocol.Query:
		return "Query"
	case protocol.Mutation:
		return "Mutation"
	case protocol.Subscription:
		return "Subscription"
	default:
		return ""
	}
}

func validPresence(value string) bool {
	switch value {
	case "required", "optional", "pending":
		return true
	default:
		return false
	}
}

func validGoIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		letter := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
		digit := index > 0 && r >= '0' && r <= '9'
		if r > 127 || r != '_' && !letter && !digit {
			return false
		}
	}
	return true
}

func goSymbol(value string) (string, bool) {
	symbol, ok := generator.PortableSymbol(value)
	return symbol, ok && validGoIdentifier(symbol)
}

func generationError(code string) error {
	if code == "" {
		return errors.New("go SDK generation failed")
	}
	return &Error{Code: code}
}
