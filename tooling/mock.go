package tooling

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const (
	CodeMockCancelled           = "MOCK_CANCELLED"
	CodeMockDocumentRejected    = "MOCK_DOCUMENT_REJECTED"
	CodeMockResourceLimit       = "MOCK_RESOURCE_LIMIT"
	CodeMockScenarioUnsupported = "MOCK_SCENARIO_UNSUPPORTED"
	CodeMockSchemaInvalid       = "MOCK_SCHEMA_INVALID"
	CodeMockSchemaUnsupported   = "MOCK_SCHEMA_UNSUPPORTED"
)

type MockGenerationError struct {
	Code string
}

func (e *MockGenerationError) Error() string {
	switch e.Code {
	case CodeMockCancelled:
		return "mock generation cancelled"
	case CodeMockDocumentRejected:
		return "mock document rejected"
	case CodeMockResourceLimit:
		return "mock resource limit exceeded"
	case CodeMockScenarioUnsupported:
		return "mock scenario is unsupported"
	case CodeMockSchemaUnsupported:
		return "mock schema capability is unsupported"
	default:
		return "mock schema is invalid"
	}
}

type MockOptions struct {
	MaxDepth       int
	MaxItems       int
	MaxStringBytes int
}

type MockInput struct {
	Document  []byte
	Operation string
	Seed      uint64
	Scenario  MockScenario
}

type mockGenerator struct {
	ctx     context.Context
	types   schema.Snapshot
	seed    uint64
	options MockOptions
}

func DefaultMockOptions() MockOptions {
	return MockOptions{MaxDepth: 32, MaxItems: 16, MaxStringBytes: 4096}
}

type MockScenario string

const (
	MockSuccess        MockScenario = "success"
	MockNull           MockScenario = "null"
	MockMissing        MockScenario = "missing"
	MockFailure        MockScenario = "failure"
	MockUnknownVariant MockScenario = "unknown-variant"
)

type MockError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Path    []string `json:"path"`
}

type MockPage struct {
	StartCursor string `json:"startCursor"`
	EndCursor   string `json:"endCursor"`
	HasNext     bool   `json:"hasNext"`
}

type MockFrame struct {
	Type     string `json:"type"`
	Sequence int    `json:"sequence"`
	Data     any    `json:"data,omitempty"`
}

type MockResult struct {
	Profile          string       `json:"profile"`
	SchemaRevision   string       `json:"schemaRevision"`
	SchemaDigest     string       `json:"schemaDigest"`
	DocumentDigest   string       `json:"documentDigest"`
	CanonicalVersion string       `json:"canonicalVersion"`
	Seed             uint64       `json:"seed"`
	Scenario         MockScenario `json:"scenario"`
	Data             any          `json:"data"`
	Errors           []MockError  `json:"errors"`
	Page             MockPage     `json:"page"`
	Frames           []MockFrame  `json:"frames"`
	Evidence         string       `json:"evidence"`
}

// GenerateMock derives deterministic examples from schema and selected-result
// metadata. No application registry or handler is accepted by this API.
func GenerateMock(schemaDocument schema.Document, document []byte, operation string, seed uint64, scenario MockScenario) (MockResult, error) {
	return GenerateMockContext(context.Background(), schemaDocument, MockInput{Document: document, Operation: operation, Seed: seed, Scenario: scenario}, DefaultMockOptions())
}

// GenerateMockContext derives deterministic examples from an already
// authorized schema view. It consumes no registry, executor, resolver, dynamic
// authorization callback, or network client.
func GenerateMockContext(ctx context.Context, schemaDocument schema.Document, input MockInput, options MockOptions) (MockResult, error) {
	if !validMockScenario(input.Scenario) {
		return MockResult{}, &MockGenerationError{Code: CodeMockScenarioUnsupported}
	}
	if err := ctx.Err(); err != nil {
		return MockResult{}, &MockGenerationError{Code: CodeMockCancelled}
	}
	if options.MaxDepth < 1 || options.MaxItems < 1 || options.MaxStringBytes < 1 {
		return MockResult{}, &MockGenerationError{Code: CodeMockResourceLimit}
	}
	explanation := Explain(schemaDocument, input.Document, input.Operation)
	if len(explanation.Rejections) != 0 || explanation.Resolved == nil {
		return MockResult{}, &MockGenerationError{Code: CodeMockDocumentRejected}
	}
	types, err := schemaDocument.Snapshot()
	if err != nil {
		return MockResult{}, &MockGenerationError{Code: CodeMockSchemaInvalid}
	}
	generator := mockGenerator{ctx: ctx, types: types, seed: input.Seed, options: options}
	data, err := generator.selected(explanation.Resolved.Result, nil, nil, 0)
	if err != nil {
		return MockResult{}, err
	}
	data, mockErrors := applyMockScenario(data, explanation.Resolved.Result, input.Scenario)
	schemaCanonical, err := schemaDocument.CanonicalJSON()
	if err != nil {
		return MockResult{}, &MockGenerationError{Code: CodeMockSchemaInvalid}
	}
	var header struct {
		Revision         string `json:"revision"`
		CanonicalVersion string `json:"canonicalVersion"`
	}
	if json.Unmarshal(schemaCanonical, &header) != nil {
		return MockResult{}, &MockGenerationError{Code: CodeMockSchemaInvalid}
	}
	schemaDigest, err := schemaDocument.Hash()
	if err != nil {
		return MockResult{}, &MockGenerationError{Code: CodeMockSchemaInvalid}
	}
	documentDigest, err := HashDocument(input.Document)
	if err != nil {
		return MockResult{}, &MockGenerationError{Code: CodeMockDocumentRejected}
	}
	page := MockPage{StartCursor: mockToken(input.Seed, "start"), EndCursor: mockToken(input.Seed, "end"), HasNext: true}
	frames := []MockFrame{{Type: "open", Sequence: 1}, {Type: "next", Sequence: 2, Data: data}, {Type: "complete", Sequence: 3}}
	return MockResult{
		Profile: "naatre.schema-mock-1", SchemaRevision: header.Revision, SchemaDigest: schemaDigest.Hex,
		DocumentDigest: documentDigest.Hex, CanonicalVersion: header.CanonicalVersion,
		Seed: input.Seed, Scenario: input.Scenario, Data: data, Errors: mockErrors, Page: page, Frames: frames,
		Evidence: "fixture-only; mock success does not prove authorization or business logic",
	}, nil
}

func validMockScenario(scenario MockScenario) bool {
	switch scenario {
	case MockSuccess, MockNull, MockMissing, MockFailure, MockUnknownVariant:
		return true
	default:
		return false
	}
}

func (g mockGenerator) selected(selected runtime.SelectedResultDescription, path []string, traits []schema.TraitDescriptor, depth int) (any, error) {
	if err := g.ctx.Err(); err != nil {
		return nil, &MockGenerationError{Code: CodeMockCancelled}
	}
	if depth > g.options.MaxDepth {
		return nil, &MockGenerationError{Code: CodeMockResourceLimit}
	}
	switch selected.Kind {
	case schema.ObjectType, schema.InputObjectType:
		return g.object(selected, path, depth)
	case schema.ListType:
		return g.list(selected, path, traits, depth)
	case schema.MapType:
		return g.mapValue(selected, path, traits, depth)
	case schema.EnumType:
		if descriptor, ok := g.types.Lookup(selected.Type); ok && len(descriptor.EnumValues) != 0 {
			return descriptor.EnumValues[int(mockNumber(g.seed, path)%uint64(len(descriptor.EnumValues)))], nil
		}
		return "MOCK", nil
	case schema.UnionType, schema.InterfaceType, schema.OneOfType:
		return map[string]any{"$variant": "MockVariant", "value": mockToken(g.seed, stringsPath(path))}, nil
	case schema.ScalarType:
		return g.scalar(selected.Type, path, traits)
	default:
		return mockToken(g.seed, stringsPath(path)), nil
	}
}

func (g mockGenerator) object(selected runtime.SelectedResultDescription, path []string, depth int) (map[string]any, error) {
	result := make(map[string]any, len(selected.Fields))
	descriptor, _ := g.types.Lookup(selected.Type)
	for _, field := range selected.Fields {
		var traits []schema.TraitDescriptor
		if schemaField, ok := descriptor.Fields[field.Name]; ok {
			traits = schemaField.Traits
		}
		value, err := g.selected(field.Result, append(path, field.Name), traits, depth+1)
		if err != nil {
			return nil, err
		}
		result[field.Name] = value
	}
	return result, nil
}

func (g mockGenerator) list(selected runtime.SelectedResultDescription, path []string, traits []schema.TraitDescriptor, depth int) ([]any, error) {
	if selected.Element == nil {
		return []any{}, nil
	}
	count, err := mockItemCount(g.types, selected, traits, g.options.MaxItems)
	if err != nil {
		return nil, err
	}
	values := make([]any, count)
	for index := range values {
		values[index], err = g.selected(*selected.Element, append(path, fmt.Sprint(index)), nil, depth+1)
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func (g mockGenerator) mapValue(selected runtime.SelectedResultDescription, path []string, traits []schema.TraitDescriptor, depth int) (map[string]any, error) {
	if _, err := mockConstraints(g.types, selected.Type, traits); err != nil {
		return nil, err
	}
	if selected.Element == nil {
		return map[string]any{}, nil
	}
	values := make(map[string]any, min(2, g.options.MaxItems))
	for index := range min(2, g.options.MaxItems) {
		value, err := g.selected(*selected.Element, append(path, fmt.Sprint(index)), nil, depth+1)
		if err != nil {
			return nil, err
		}
		values[fmt.Sprintf("key-%d", index)] = value
	}
	return values, nil
}

func (g mockGenerator) scalar(typeID schema.TypeID, path []string, traits []schema.TraitDescriptor) (any, error) {
	number := mockNumber(g.seed, path)
	constraints, err := mockConstraints(g.types, typeID, traits)
	if err != nil {
		return nil, err
	}
	switch schema.ScalarKind(typeID) {
	case schema.Boolean:
		return number%2 == 0, nil
	case schema.Int32:
		if hasMockConstraints(constraints) {
			return nil, &MockGenerationError{Code: CodeMockSchemaUnsupported}
		}
		return int32(number % 1000), nil
	case schema.Float64:
		if hasMockConstraints(constraints) {
			return nil, &MockGenerationError{Code: CodeMockSchemaUnsupported}
		}
		return float64(number%10000) / 100, nil
	case schema.String, schema.ID:
		return mockString("mock-"+mockToken(g.seed, stringsPath(path)), constraints, g.options.MaxStringBytes)
	case schema.Int64:
		if hasMockConstraints(constraints) {
			return nil, &MockGenerationError{Code: CodeMockSchemaUnsupported}
		}
		return fmt.Sprintf("%d", int64(number%1000)), nil
	case schema.UInt64:
		if hasMockConstraints(constraints) {
			return nil, &MockGenerationError{Code: CodeMockSchemaUnsupported}
		}
		return fmt.Sprintf("%d", number%1000), nil
	case schema.BigInt:
		if hasMockConstraints(constraints) {
			return nil, &MockGenerationError{Code: CodeMockSchemaUnsupported}
		}
		return fmt.Sprintf("%d000000000000000000", number%1000), nil
	case schema.Decimal:
		if hasMockConstraints(constraints) {
			return nil, &MockGenerationError{Code: CodeMockSchemaUnsupported}
		}
		return fmt.Sprintf("%d.%02d", number%1000, number%100), nil
	case schema.Timestamp:
		return fmt.Sprintf("2026-01-01T00:00:%02dZ", number%60), nil
	case schema.Duration:
		return fmt.Sprintf("PT%dS", number%60+1), nil
	case schema.UUID:
		return "550e8400-e29b-41d4-a716-446655440000", nil
	case schema.Bytes:
		return mockToken(g.seed, stringsPath(path)), nil
	default:
		if hasMockConstraints(constraints) {
			return nil, &MockGenerationError{Code: CodeMockSchemaUnsupported}
		}
		return "mock-" + mockToken(g.seed, stringsPath(path)), nil
	}
}

func mockItemCount(types schema.Snapshot, selected runtime.SelectedResultDescription, traits []schema.TraitDescriptor, maximum int) (int, error) {
	count := 2
	constraints, err := mockConstraints(types, selected.Type, traits)
	if err != nil {
		return 0, err
	}
	if constraints.MinItems != nil && count < *constraints.MinItems {
		count = *constraints.MinItems
	}
	if constraints.MaxItems != nil && count > *constraints.MaxItems {
		count = *constraints.MaxItems
	}
	if count > maximum {
		return 0, &MockGenerationError{Code: CodeMockResourceLimit}
	}
	if constraints.UniqueItems {
		return 0, &MockGenerationError{Code: CodeMockSchemaUnsupported}
	}
	return count, nil
}

func mockString(value string, constraints schema.ConstraintSet, maximum int) (string, error) {
	if constraints.Pattern != "" || constraints.Format != nil {
		return "", &MockGenerationError{Code: CodeMockSchemaUnsupported}
	}
	minimum := 0
	if constraints.MinLength != nil {
		minimum = *constraints.MinLength
	}
	limit := maximum
	if constraints.MaxLength != nil && *constraints.MaxLength < limit {
		limit = *constraints.MaxLength
	}
	if minimum > limit {
		return "", &MockGenerationError{Code: CodeMockResourceLimit}
	}
	if len(value) < minimum {
		value += strings.Repeat("x", minimum-len(value))
	}
	if len(value) > limit {
		value = value[:limit]
	}
	return value, nil
}

func mockConstraints(types schema.Snapshot, typeID schema.TypeID, fieldTraits []schema.TraitDescriptor) (schema.ConstraintSet, error) {
	var typeTraits []schema.TraitDescriptor
	descriptor, ok := types.Lookup(typeID)
	if ok {
		typeTraits = descriptor.Traits
	}
	constraints, err := parseMockConstraints(typeTraits)
	if err != nil {
		return schema.ConstraintSet{}, err
	}
	fieldConstraints, err := parseMockConstraints(fieldTraits)
	if err != nil {
		return schema.ConstraintSet{}, err
	}
	return mergeMockConstraints(constraints, fieldConstraints), nil
}

func parseMockConstraints(traits []schema.TraitDescriptor) (schema.ConstraintSet, error) {
	constraints, _, err := schema.ParseConstraintTrait(traits)
	if err != nil {
		return schema.ConstraintSet{}, &MockGenerationError{Code: CodeMockSchemaInvalid}
	}
	if constraints.Minimum != nil || constraints.Maximum != nil || constraints.Precision != nil || constraints.Scale != nil ||
		constraints.MinProperties != nil || constraints.MaxProperties != nil || constraints.KeyPattern != "" || len(constraints.Rules) != 0 {
		return schema.ConstraintSet{}, &MockGenerationError{Code: CodeMockSchemaUnsupported}
	}
	return constraints, nil
}

func mergeMockConstraints(left, right schema.ConstraintSet) schema.ConstraintSet {
	result := left
	result.MinLength = greaterInt(left.MinLength, right.MinLength)
	result.MaxLength = lesserInt(left.MaxLength, right.MaxLength)
	result.MinItems = greaterInt(left.MinItems, right.MinItems)
	result.MaxItems = lesserInt(left.MaxItems, right.MaxItems)
	result.UniqueItems = left.UniqueItems || right.UniqueItems
	if right.Pattern != "" {
		result.Pattern = right.Pattern
	}
	if right.Format != nil {
		result.Format = right.Format
	}
	return result
}

func greaterInt(left, right *int) *int {
	if left == nil || right != nil && *right > *left {
		return right
	}
	return left
}

func lesserInt(left, right *int) *int {
	if left == nil || right != nil && *right < *left {
		return right
	}
	return left
}

func hasMockConstraints(constraints schema.ConstraintSet) bool {
	return constraints.Minimum != nil || constraints.Maximum != nil || constraints.Precision != nil || constraints.Scale != nil ||
		constraints.MinLength != nil || constraints.MaxLength != nil || constraints.Pattern != "" || constraints.Format != nil ||
		constraints.MinItems != nil || constraints.MaxItems != nil || constraints.UniqueItems || constraints.MinProperties != nil ||
		constraints.MaxProperties != nil || constraints.KeyPattern != "" || len(constraints.Rules) != 0
}

func applyMockScenario(data any, selected runtime.SelectedResultDescription, scenario MockScenario) (any, []MockError) {
	object, ok := data.(map[string]any)
	if !ok || len(selected.Fields) == 0 || scenario == MockSuccess {
		return data, []MockError{}
	}
	name := selected.Fields[0].Name
	switch scenario {
	case MockSuccess:
		return object, []MockError{}
	case MockNull:
		object[name] = nil
	case MockMissing:
		delete(object, name)
	case MockFailure:
		object[name] = nil
		return object, []MockError{{Code: "MOCK_PARTIAL_FAILURE", Message: "seeded partial failure", Path: []string{name}}}
	case MockUnknownVariant:
		object[name] = map[string]any{"$variant": "UnknownVariant", "raw": map[string]any{"seeded": true}}
	}
	return object, []MockError{}
}

func mockNumber(seed uint64, path []string) uint64 {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", seed, stringsPath(path))))
	return binary.BigEndian.Uint64(digest[:8])
}

func mockToken(seed uint64, path string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", seed, path)))
	return hex.EncodeToString(digest[:6])
}

func stringsPath(path []string) string {
	encoded, _ := json.Marshal(path)
	return string(encoded)
}
