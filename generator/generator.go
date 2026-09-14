// Package generator implements the language-neutral generator model and the
// deterministic reference JSON generator. Language generators consume the
// encoded model; they do not import Naatre's Go runtime packages.
package generator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/protocol"
)

const (
	ModelVersion     = "naatre.generator-model-1"
	GeneratorVersion = "naatre.generator.reference-json-1"
)

type Diagnostic struct {
	Code    string `json:"code"`
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

type Error struct {
	diagnostics []Diagnostic
}

func (e *Error) Error() string { return "language-neutral generation failed" }

func (e *Error) Diagnostics() []Diagnostic { return slices.Clone(e.diagnostics) }

type Artifact struct {
	Path    string
	Content []byte
}

type model struct {
	Profile          string          `json:"profile"`
	Version          string          `json:"version"`
	ProtocolVersion  string          `json:"protocolVersion"`
	CanonicalVersion string          `json:"canonicalVersion"`
	Schema           json.RawMessage `json:"schema"`
	Configuration    configuration   `json:"configuration"`
	Operations       []operation     `json:"operations"`
}

type configuration struct {
	ScalarMappings map[string]string `json:"scalarMappings"`
}

type operation struct {
	Name             string          `json:"name"`
	Description      string          `json:"description,omitempty"`
	Document         json.RawMessage `json:"document"`
	Variables        []variable      `json:"variables"`
	RequestVariables json.RawMessage `json:"requestVariables"`
	Result           json.RawMessage `json:"result"`
}

type variable struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Required bool            `json:"required"`
	Nullable bool            `json:"nullable"`
	Default  json.RawMessage `json:"default,omitempty"`
}

type schemaHeader struct {
	Revision string       `json:"revision"`
	Types    []schemaType `json:"types"`
}

type schemaType struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type output struct {
	Profile              string               `json:"profile"`
	ModelVersion         string               `json:"modelVersion"`
	GeneratorVersion     string               `json:"generatorVersion"`
	ProtocolVersion      string               `json:"protocolVersion"`
	CanonicalVersion     string               `json:"canonicalVersion"`
	Schema               protocol.Digest      `json:"schema"`
	ScalarMappings       map[string]string    `json:"scalarMappings"`
	ForwardCompatibility forwardCompatibility `json:"forwardCompatibility"`
	Behavior             sdkBehavior          `json:"behavior"`
	Operations           []operationOutput    `json:"operations"`
}

type forwardCompatibility struct {
	OpenEnum  string `json:"openEnum"`
	OpenUnion string `json:"openUnion"`
	Closed    string `json:"closed"`
}

type sdkBehavior struct {
	PartialData          string            `json:"partialData"`
	Missing              string            `json:"missing"`
	ExplicitNull         string            `json:"explicitNull"`
	UnknownDomainVariant string            `json:"unknownDomainVariant"`
	UnknownControlField  string            `json:"unknownControlField"`
	Retry                string            `json:"retry"`
	AuthRefresh          string            `json:"authRefresh"`
	Limits               sdkLimits         `json:"limits"`
	Cancellation         map[string]string `json:"cancellation"`
}

type sdkLimits struct {
	ResponseBytes     int `json:"responseBytes"`
	FrameBytes        int `json:"frameBytes"`
	DecompressedBytes int `json:"decompressedBytes"`
	Redirects         int `json:"redirects"`
}

type operationOutput struct {
	Name        string          `json:"name"`
	Symbol      string          `json:"symbol"`
	Artifact    string          `json:"artifact"`
	Description string          `json:"description,omitempty"`
	Variables   []variable      `json:"variables"`
	Persisted   protocol.Digest `json:"persisted"`
	Request     requestTemplate `json:"request"`
	Result      resultContract  `json:"result"`
}

type requestTemplate struct {
	Version   string          `json:"version"`
	Operation string          `json:"operation"`
	Persisted protocol.Digest `json:"persisted"`
	Variables any             `json:"variables"`
}

type resultContract struct {
	Kind     string `json:"kind"`
	Data     any    `json:"data"`
	Errors   string `json:"errors"`
	Complete string `json:"complete"`
}

var generatedWordPattern = regexp.MustCompile(`[A-Za-z0-9]+`)

var reservedWords = map[string]bool{
	"class": true, "const": true, "default": true, "delete": true,
	"enum": true, "export": true, "extends": true, "function": true,
	"import": true, "interface": true, "new": true, "package": true,
	"private": true, "protected": true, "public": true, "return": true,
	"static": true, "struct": true, "switch": true, "type": true,
	"var": true, "yield": true,
}

var builtInScalars = map[string]bool{
	"BigInt": true, "Boolean": true, "Bytes": true, "Decimal": true,
	"Duration": true, "Float64": true, "ID": true, "Int32": true,
	"Int64": true, "String": true, "Timestamp": true, "UInt64": true,
	"UUID": true,
}

// Generate validates and consumes one generator-model-1 document and returns
// the deterministic reference bundle as canonical JSON.
func Generate(input []byte) ([]byte, error) {
	parsed, err := decodeModel(input)
	if err != nil {
		return nil, err
	}
	if err := validateModelVersions(parsed); err != nil {
		return nil, err
	}
	canonicalSchema, err := protocol.CanonicalizeSchema(parsed.Schema, protocol.Limits{})
	if err != nil {
		return nil, generationError("GENERATOR_INVALID_SCHEMA", "/schema", "schema is not canonicalizable")
	}
	schemaDigest, err := protocol.SemanticHash(protocol.SchemaHash, canonicalSchema)
	if err != nil {
		return nil, generationError("GENERATOR_INVALID_SCHEMA", "/schema", "schema digest failed")
	}
	if err := validateScalarMappings(parsed.Schema, parsed.Configuration.ScalarMappings); err != nil {
		return nil, err
	}
	operations, err := generateOperations(parsed)
	if err != nil {
		return nil, err
	}
	generated := output{
		Profile: parsed.Profile, ModelVersion: parsed.Version, GeneratorVersion: GeneratorVersion,
		ProtocolVersion: parsed.ProtocolVersion, CanonicalVersion: parsed.CanonicalVersion,
		Schema: schemaDigest, ScalarMappings: maps.Clone(parsed.Configuration.ScalarMappings),
		ForwardCompatibility: forwardCompatibility{
			OpenEnum: "preserve-unknown-raw-value", OpenUnion: "preserve-unknown-raw-discriminator",
			Closed: "reject-as-protocol-error",
		},
		Behavior: sdkBehavior{
			PartialData: "preserve-data-and-structured-errors", Missing: "omit-property",
			ExplicitNull: "preserve-null", UnknownDomainVariant: "preserve-open-variant",
			UnknownControlField: "reject-required-control-field", Retry: "idempotent-only",
			AuthRefresh:  "single-flight-no-write-replay",
			Limits:       sdkLimits{ResponseBytes: 8 << 20, FrameBytes: 1 << 20, DecompressedBytes: 16 << 20, Redirects: 5},
			Cancellation: map[string]string{"http": "active-abort", "sse": "active-abort", "websocket": "active-close"},
		},
		Operations: operations,
	}
	encoded, err := json.Marshal(generated)
	if err != nil {
		return nil, generationError("GENERATOR_INTERNAL", "", "generated bundle could not be encoded")
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{})
	if err != nil {
		return nil, generationError("GENERATOR_INTERNAL", "", "generated bundle could not be canonicalized")
	}
	return append(canonical, '\n'), nil
}

func decodeModel(input []byte) (model, error) {
	if err := protocol.ValidateJSON(input, protocol.Limits{MaxBytes: 8 << 20, MaxDepth: 64, MaxMembers: 1 << 15, MaxArrayItems: 1 << 15}); err != nil {
		return model{}, generationError("GENERATOR_INVALID_MODEL", "", "model is not strict bounded JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var parsed model
	if err := decoder.Decode(&parsed); err != nil {
		return model{}, generationError("GENERATOR_INVALID_MODEL", "", "model shape is invalid")
	}
	inputEnd := len(bytes.TrimRight(input, " \t\r\n"))
	if decoder.InputOffset() != int64(inputEnd) {
		return model{}, generationError("GENERATOR_INVALID_MODEL", "", "model has trailing content")
	}
	return parsed, nil
}

func validateModelVersions(parsed model) error {
	switch {
	case parsed.Profile != "sdk.generation-1":
		return generationError("GENERATOR_UNSUPPORTED_PROFILE", "/profile", "generator profile is unsupported")
	case parsed.Version != ModelVersion:
		return generationError("GENERATOR_UNSUPPORTED_VERSION", "/version", "model version is unsupported")
	case parsed.ProtocolVersion != "1":
		return generationError("GENERATOR_UNSUPPORTED_VERSION", "/protocolVersion", "protocol version is unsupported")
	case parsed.CanonicalVersion != "c14n-1":
		return generationError("GENERATOR_UNSUPPORTED_VERSION", "/canonicalVersion", "canonicalization version is unsupported")
	case len(parsed.Schema) == 0:
		return generationError("GENERATOR_INVALID_MODEL", "/schema", "schema is required")
	case parsed.Configuration.ScalarMappings == nil:
		return generationError("GENERATOR_INVALID_MODEL", "/configuration/scalarMappings", "scalar mappings are required")
	case len(parsed.Operations) == 0:
		return generationError("GENERATOR_INVALID_MODEL", "/operations", "operations are required")
	}
	return nil
}

func validateScalarMappings(raw json.RawMessage, mappings map[string]string) error {
	var header schemaHeader
	if err := json.Unmarshal(raw, &header); err != nil || header.Revision == "" || header.Types == nil {
		return generationError("GENERATOR_INVALID_SCHEMA", "/schema", "schema header is invalid")
	}
	for _, descriptor := range header.Types {
		if descriptor.Kind != "scalar" || builtInScalars[descriptor.ID] {
			continue
		}
		mapping, present := mappings[descriptor.ID]
		if !present || strings.TrimSpace(mapping) == "" {
			return generationError("GENERATOR_UNMAPPED_SCALAR", "/configuration/scalarMappings/"+escapePointer(descriptor.ID), "custom scalar requires an explicit mapping")
		}
	}
	return nil
}

func generateOperations(parsed model) ([]operationOutput, error) {
	type indexedOperation struct {
		operation operation
		index     int
	}
	operations := make([]indexedOperation, len(parsed.Operations))
	for index, operation := range parsed.Operations {
		operations[index] = indexedOperation{operation: operation, index: index}
	}
	sort.SliceStable(operations, func(left, right int) bool { return operations[left].operation.Name < operations[right].operation.Name })
	seenNames := make(map[string]bool, len(operations))
	seenSymbols := make(map[string]bool, len(operations))
	result := make([]operationOutput, len(operations))
	for index, indexed := range operations {
		operation := indexed.operation
		pointer := fmt.Sprintf("/operations/%d", indexed.index)
		if operation.Name == "" || seenNames[operation.Name] {
			return nil, generationError("GENERATOR_INVALID_IDENTIFIER", pointer+"/name", "operation name is missing or duplicated")
		}
		seenNames[operation.Name] = true
		symbol, ok := generatedSymbol(operation.Name)
		if !ok {
			return nil, generationError("GENERATOR_INVALID_IDENTIFIER", pointer+"/name", "operation name cannot produce a portable symbol")
		}
		if seenSymbols[symbol] {
			return nil, generationError("GENERATOR_SYMBOL_COLLISION", pointer+"/name", "operation symbols collide")
		}
		seenSymbols[symbol] = true
		generated, err := generateOperation(parsed.ProtocolVersion, operation, symbol, pointer)
		if err != nil {
			return nil, err
		}
		result[index] = generated
	}
	return result, nil
}

func generateOperation(protocolVersion string, operation operation, symbol, pointer string) (operationOutput, error) {
	document, err := protocol.DecodeDocument(operation.Document, protocol.Limits{})
	if err != nil {
		return operationOutput{}, generationError("GENERATOR_INVALID_DOCUMENT", pointer+"/document", "operation document is invalid")
	}
	persisted, err := protocol.SemanticHash(protocol.DocumentHash, document.CanonicalJSON())
	if err != nil {
		return operationOutput{}, generationError("GENERATOR_INVALID_DOCUMENT", pointer+"/document", "operation document digest failed")
	}
	variables, err := decodeAny(operation.RequestVariables)
	if err != nil {
		return operationOutput{}, generationError("GENERATOR_INVALID_VARIABLES", pointer+"/requestVariables", "request variables are invalid")
	}
	if _, ok := variables.(map[string]any); !ok {
		return operationOutput{}, generationError("GENERATOR_INVALID_VARIABLES", pointer+"/requestVariables", "request variables require an object")
	}
	selected, err := decodeAny(operation.Result)
	if err != nil {
		return operationOutput{}, generationError("GENERATOR_INVALID_RESULT", pointer+"/result", "selected result is invalid")
	}
	variableDefinitions := slices.Clone(operation.Variables)
	for index := range variableDefinitions {
		variableDefinitions[index].Default = slices.Clone(variableDefinitions[index].Default)
	}
	return operationOutput{
		Name: operation.Name, Symbol: symbol, Artifact: "operations/" + symbol + ".json",
		Description: operation.Description, Variables: variableDefinitions, Persisted: persisted,
		Request: requestTemplate{Version: protocolVersion, Operation: operation.Name, Persisted: persisted, Variables: variables},
		Result:  resultContract{Kind: "operation-result", Data: selected, Errors: "structured-errors", Complete: "explicit-completion-state"},
	}, nil
}

func decodeAny(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing JSON value")
	}
	if err := protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: 4 << 20, MaxDepth: 64, MaxMembers: 1 << 14, MaxArrayItems: 1 << 14}); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func generatedSymbol(name string) (string, bool) {
	words := generatedWordPattern.FindAllString(name, -1)
	if len(words) == 0 {
		return "", false
	}
	var symbol strings.Builder
	for _, word := range words {
		symbol.WriteString(strings.ToUpper(word[:1]))
		symbol.WriteString(word[1:])
	}
	result := symbol.String()
	if result == "" || result[0] >= '0' && result[0] <= '9' {
		return "", false
	}
	if reservedWords[strings.ToLower(result)] {
		result += "_"
	}
	return result, true
}

func generationError(code, pointer, message string) error {
	diagnostic := Diagnostic{Code: code, Pointer: pointer, Message: message}
	return &Error{diagnostics: []Diagnostic{diagnostic}}
}

func escapePointer(value string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(value)
}

// WriteArtifacts writes files atomically after proving every parent directory
// stays beneath outputRoot and contains no symlink traversal.
func WriteArtifacts(outputRoot string, artifacts []Artifact) error {
	root, err := filepath.Abs(outputRoot)
	if err != nil {
		return generationError("GENERATOR_PATH_ESCAPE", "", "output root is invalid")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return generationError("GENERATOR_WRITE_FAILED", "", "output root cannot be created")
	}
	seen := make(map[string]bool, len(artifacts))
	for index, artifact := range artifacts {
		clean, ok := safeArtifactPath(artifact.Path)
		if !ok || seen[clean] {
			return generationError("GENERATOR_PATH_ESCAPE", fmt.Sprintf("/artifacts/%d/path", index), "artifact path is unsafe or duplicated")
		}
		seen[clean] = true
		target := filepath.Join(root, clean)
		parent := filepath.Dir(target)
		if err := ensureSafeParents(root, parent); err != nil {
			return generationError("GENERATOR_PATH_ESCAPE", fmt.Sprintf("/artifacts/%d/path", index), "artifact parent is unsafe")
		}
		if err := writeArtifactAtomic(target, artifact.Content); err != nil {
			return generationError("GENERATOR_WRITE_FAILED", fmt.Sprintf("/artifacts/%d", index), "artifact write failed")
		}
	}
	return nil
}

func safeArtifactPath(path string) (string, bool) {
	if path == "" || filepath.IsAbs(path) {
		return "", false
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}

func ensureSafeParents(root, parent string) error {
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("parent escapes root")
	}
	current := root
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "." || segment == "" {
			continue
		}
		current = filepath.Join(current, segment)
		info, statErr := os.Lstat(current)
		switch {
		case statErr == nil && info.Mode()&os.ModeSymlink != 0:
			return errors.New("symlink parent")
		case statErr == nil && !info.IsDir():
			return errors.New("non-directory parent")
		case errors.Is(statErr, os.ErrNotExist):
			if mkdirErr := os.Mkdir(current, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return mkdirErr
			}
		case statErr != nil:
			return statErr
		}
	}
	return nil
}

func writeArtifactAtomic(target string, content []byte) error {
	if info, err := os.Lstat(target); err == nil && info.IsDir() {
		return errors.New("artifact target is a directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".naatre-generator-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, target)
}
