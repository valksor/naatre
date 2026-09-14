package tooling

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

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
	Profile  string       `json:"profile"`
	Seed     uint64       `json:"seed"`
	Scenario MockScenario `json:"scenario"`
	Data     any          `json:"data"`
	Errors   []MockError  `json:"errors"`
	Page     MockPage     `json:"page"`
	Frames   []MockFrame  `json:"frames"`
	Evidence string       `json:"evidence"`
}

// GenerateMock derives deterministic examples from schema and selected-result
// metadata. No application registry or handler is accepted by this API.
func GenerateMock(schemaDocument schema.Document, document []byte, operation string, seed uint64, scenario MockScenario) (MockResult, error) {
	if !validMockScenario(scenario) {
		return MockResult{}, fmt.Errorf("unsupported mock scenario %q", scenario)
	}
	explanation := Explain(schemaDocument, document, operation)
	if len(explanation.Rejections) != 0 || explanation.Resolved == nil {
		return MockResult{}, fmt.Errorf("mock document rejected: %s", firstRejection(explanation.Rejections))
	}
	types, err := schemaDocument.Snapshot()
	if err != nil {
		return MockResult{}, err
	}
	data := mockSelected(types, explanation.Resolved.Result, seed, nil)
	data, mockErrors := applyMockScenario(data, explanation.Resolved.Result, scenario)
	page := MockPage{StartCursor: mockToken(seed, "start"), EndCursor: mockToken(seed, "end"), HasNext: true}
	frames := []MockFrame{{Type: "open", Sequence: 1}, {Type: "next", Sequence: 2, Data: data}, {Type: "complete", Sequence: 3}}
	return MockResult{
		Profile: "naatre.schema-mock-1", Seed: seed, Scenario: scenario, Data: data, Errors: mockErrors, Page: page, Frames: frames,
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

func firstRejection(diagnostics []Diagnostic) string {
	if len(diagnostics) == 0 {
		return "unknown rejection"
	}
	return diagnostics[0].Code
}

func mockSelected(types schema.Snapshot, selected runtime.SelectedResultDescription, seed uint64, path []string) any {
	switch selected.Kind {
	case schema.ObjectType, schema.InputObjectType:
		result := make(map[string]any, len(selected.Fields))
		for _, field := range selected.Fields {
			result[field.Name] = mockSelected(types, field.Result, seed, append(path, field.Name))
		}
		return result
	case schema.ListType, schema.MapType:
		if selected.Element == nil {
			return []any{}
		}
		return []any{mockSelected(types, *selected.Element, seed, append(path, "0")), mockSelected(types, *selected.Element, seed, append(path, "1"))}
	case schema.EnumType:
		if descriptor, ok := types.Lookup(selected.Type); ok && len(descriptor.EnumValues) != 0 {
			return descriptor.EnumValues[int(mockNumber(seed, path)%uint64(len(descriptor.EnumValues)))]
		}
		return "MOCK"
	case schema.UnionType, schema.InterfaceType, schema.OneOfType:
		return map[string]any{"$variant": "MockVariant", "value": mockToken(seed, stringsPath(path))}
	case schema.ScalarType:
		return mockScalar(selected.Type, seed, path)
	default:
		return mockToken(seed, stringsPath(path))
	}
}

func mockScalar(typeID schema.TypeID, seed uint64, path []string) any {
	number := mockNumber(seed, path)
	switch schema.ScalarKind(typeID) {
	case schema.Boolean:
		return number%2 == 0
	case schema.Int32:
		return int32(number % 1000)
	case schema.Float64:
		return float64(number%10000) / 100
	case schema.String, schema.ID:
		return "mock-" + mockToken(seed, stringsPath(path))
	case schema.Int64:
		return fmt.Sprintf("%d", int64(number%1000))
	case schema.UInt64:
		return fmt.Sprintf("%d", number%1000)
	case schema.BigInt:
		return fmt.Sprintf("%d000000000000000000", number%1000)
	case schema.Decimal:
		return fmt.Sprintf("%d.%02d", number%1000, number%100)
	case schema.Timestamp:
		return fmt.Sprintf("2026-01-01T00:00:%02dZ", number%60)
	case schema.Duration:
		return fmt.Sprintf("PT%dS", number%60+1)
	case schema.UUID:
		return "550e8400-e29b-41d4-a716-446655440000"
	case schema.Bytes:
		return mockToken(seed, stringsPath(path))
	default:
		return "mock-" + mockToken(seed, stringsPath(path))
	}
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
