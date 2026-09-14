// Package sdkgen generates the deterministic Java and Kotlin reference bindings.
package sdkgen

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"text/template"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/protocol"
)

const GeneratorVersion = "naatre.generator.jvm-sdk-1"

const maxInputBytes = 4 << 20

//go:embed templates/*.tmpl
var templates embed.FS

type Error struct{ Code string }

func (e *Error) Error() string { return "JVM SDK generation failed: " + e.Code }

type Artifacts struct {
	Java     []byte
	Kotlin   []byte
	Manifest []byte
}

type model struct {
	Version          string           `json:"version"`
	ProtocolVersion  string           `json:"protocolVersion"`
	CanonicalVersion string           `json:"canonicalVersion"`
	Schema           schemaModel      `json:"schema"`
	Operations       []modelOperation `json:"operations"`
}

type schemaModel struct {
	Types []schemaType `json:"types"`
}

type schemaType struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	Open        bool           `json:"open"`
	Fields      []schemaField  `json:"fields"`
	EnumMembers []schemaMember `json:"enumMembers"`
}

type schemaField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type schemaMember struct {
	Name string `json:"name"`
}

type variable struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Nullable bool   `json:"nullable"`
}

type modelOperation struct {
	Name      string          `json:"name"`
	Document  json.RawMessage `json:"document"`
	Variables []variable      `json:"variables"`
	Result    resultNode      `json:"result"`
}

type resultNode struct {
	Kind     string          `json:"kind"`
	Type     string          `json:"type"`
	Nullable bool            `json:"nullable"`
	Fields   []selectedField `json:"fields"`
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

type templateData struct {
	GeneratorVersion string
	Operation        string
	Symbol           string
	Digest           string
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
	parsed, reference, operationKind, err := validateInputs(modelBytes, referenceBytes)
	if err != nil {
		return Artifacts{}, err
	}
	output := reference.Operations[0]
	data := templateData{
		GeneratorVersion: GeneratorVersion,
		Operation:        parsed.Operations[0].Name,
		Symbol:           output.Symbol,
		Digest:           output.Persisted.Hex,
	}
	java, err := render("templates/java.tmpl", data)
	if err != nil {
		return Artifacts{}, failure("TEMPLATE_FAILED")
	}
	kotlin, err := render("templates/kotlin.tmpl", data)
	if err != nil {
		return Artifacts{}, failure("TEMPLATE_FAILED")
	}
	manifest, err := json.Marshal(manifestWire{
		Profile:          "sdk.jvm.core-1",
		Version:          "1",
		ProtocolVersion:  parsed.ProtocolVersion,
		CanonicalVersion: parsed.CanonicalVersion,
		Operations: []manifestOperationWire{{
			Name: parsed.Operations[0].Name, Kind: operationKind, Persisted: output.Persisted,
		}},
	})
	if err != nil {
		return Artifacts{}, failure("MANIFEST_FAILED")
	}
	manifest, err = protocol.CanonicalizeJSON(manifest, protocol.Limits{MaxBytes: maxInputBytes})
	if err != nil {
		return Artifacts{}, failure("MANIFEST_FAILED")
	}
	return Artifacts{Java: java, Kotlin: kotlin, Manifest: manifest}, nil
}

func validateInputs(modelBytes, referenceBytes []byte) (model, referenceOutput, protocol.OperationKind, error) {
	if len(modelBytes) == 0 || len(modelBytes) > maxInputBytes || len(referenceBytes) == 0 || len(referenceBytes) > maxInputBytes {
		return model{}, referenceOutput{}, "", failure("INPUT_LIMIT")
	}
	expected, err := generator.Generate(modelBytes)
	if err != nil {
		return model{}, referenceOutput{}, "", translateModelError(err)
	}
	if !bytes.Equal(expected, referenceBytes) {
		return model{}, referenceOutput{}, "", failure("REFERENCE_DRIFT")
	}
	var parsed model
	var reference referenceOutput
	if json.Unmarshal(modelBytes, &parsed) != nil || json.Unmarshal(referenceBytes, &reference) != nil {
		return model{}, referenceOutput{}, "", failure("INVALID_INPUT")
	}
	if parsed.Version != generator.ModelVersion || reference.ModelVersion != generator.ModelVersion ||
		reference.GeneratorVersion != generator.GeneratorVersion || parsed.ProtocolVersion != "1" ||
		reference.ProtocolVersion != parsed.ProtocolVersion || parsed.CanonicalVersion != "c14n-1" ||
		reference.CanonicalVersion != parsed.CanonicalVersion {
		return model{}, referenceOutput{}, "", failure("VERSION_SKEW")
	}
	if err := validateReferenceSlice(parsed, reference); err != nil {
		return model{}, referenceOutput{}, "", err
	}
	document, err := protocol.DecodeDocument(parsed.Operations[0].Document, protocol.Limits{})
	if err != nil {
		return model{}, referenceOutput{}, "", failure("INVALID_OPERATION")
	}
	for _, operation := range document.Operations() {
		if operation.Name() == parsed.Operations[0].Name {
			return parsed, reference, operation.Kind(), nil
		}
	}
	return model{}, referenceOutput{}, "", failure("INVALID_OPERATION")
}

func validateReferenceSlice(parsed model, reference referenceOutput) error {
	if len(parsed.Operations) != 1 || len(reference.Operations) != 1 ||
		parsed.Operations[0].Name != "GetAccount" || reference.Operations[0].Name != "GetAccount" ||
		reference.Operations[0].Symbol != "GetAccount" {
		return failure("UNSUPPORTED_MODEL")
	}
	// The core reference intentionally proves one selected-shape fixture. The
	// plugin/adaptor issue owns the general-purpose JVM generator surface.
	if !matchesFixtureSchema(parsed.Schema.Types) || !matchesFixtureVariables(parsed.Operations[0].Variables) ||
		!matchesFixtureResult(parsed.Operations[0].Result) {
		return failure("UNSUPPORTED_MODEL")
	}
	return nil
}

func matchesFixtureSchema(types []schemaType) bool {
	return len(types) == 3 && types[0].ID == "Status" && types[0].Kind == "enum" && types[0].Open &&
		len(types[0].Fields) == 0 && len(types[0].EnumMembers) == 2 &&
		types[0].EnumMembers[0].Name == "PENDING" && types[0].EnumMembers[1].Name == "ACTIVE" &&
		types[1].ID == "Money" && types[1].Kind == "scalar" && !types[1].Open &&
		len(types[1].Fields) == 0 && len(types[1].EnumMembers) == 0 &&
		types[2].ID == "Account" && types[2].Kind == "object" && !types[2].Open &&
		len(types[2].EnumMembers) == 0 && len(types[2].Fields) == 3 &&
		matchesField(types[2].Fields[0], "status", "Status") &&
		matchesField(types[2].Fields[1], "balance", "Money") &&
		matchesField(types[2].Fields[2], "displayName", "String")
}

func matchesFixtureVariables(variables []variable) bool {
	return len(variables) == 4 &&
		matchesVariable(variables[0], "id", "ID", true, false) &&
		matchesVariable(variables[1], "nickname", "String", false, true) &&
		matchesVariable(variables[2], "tags", "StringList", false, false) &&
		matchesVariable(variables[3], "filter", "StringMap", false, false)
}

func matchesFixtureResult(result resultNode) bool {
	if result.Kind != "object" || result.Type != "" || result.Nullable || len(result.Fields) != 2 {
		return false
	}
	profile := result.Fields[0]
	if profile.Name != "profile" || profile.Presence != "required" || profile.Result.Kind != "object" ||
		profile.Result.Type != "Account" || profile.Result.Nullable || len(profile.Result.Fields) != 2 {
		return false
	}
	display, nickname := profile.Result.Fields[0], profile.Result.Fields[1]
	later := result.Fields[1]
	return display.Name == "display" && display.Presence == "required" &&
		display.Result.Kind == "scalar" && display.Result.Type == "String" && !display.Result.Nullable && len(display.Result.Fields) == 0 &&
		nickname.Name == "nickname" && nickname.Presence == "optional" &&
		nickname.Result.Kind == "scalar" && nickname.Result.Type == "String" && nickname.Result.Nullable && len(nickname.Result.Fields) == 0 &&
		later.Name == "later" && later.Presence == "pending" &&
		later.Result.Kind == "scalar" && later.Result.Type == "String" && !later.Result.Nullable && len(later.Result.Fields) == 0
}

func matchesField(field schemaField, name, fieldType string) bool {
	return field.Name == name && field.Type == fieldType
}

func matchesVariable(value variable, name, variableType string, required, nullable bool) bool {
	return value.Name == name && value.Type == variableType && value.Required == required && value.Nullable == nullable
}

func render(name string, data templateData) ([]byte, error) {
	parsed, err := template.New(name).ParseFS(templates, name)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := parsed.ExecuteTemplate(&output, path.Base(name), data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
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

func failure(code string) error { return &Error{Code: "JVM_SDK_GENERATOR_" + code} }

func (a Artifacts) String() string {
	return fmt.Sprintf("java=%d kotlin=%d manifest=%d", len(a.Java), len(a.Kotlin), len(a.Manifest))
}
