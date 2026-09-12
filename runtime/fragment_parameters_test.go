package runtime_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

func TestPrepareValidatesFragmentParameterCapabilityAndBindings(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "capability required",
			body: `"document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"F"}}]}],"fragments":[{"name":"F","parameters":[{"name":"value","type":"String","default":"ok"}],"select":[{"$call":{"name":"text"}}]}]}`,
			code: "UNSUPPORTED_CAPABILITY",
		},
		{
			name: "required argument missing",
			body: `"capabilities":["language.fragment-parameters-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"F"}}]}],"fragments":[{"name":"F","parameters":[{"name":"value","type":"String","required":true}],"select":[{"$call":{"name":"text"}}]}]}`,
			code: "MISSING_ARGUMENT",
		},
		{
			name: "unknown argument",
			body: `"capabilities":["language.fragment-parameters-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"F","args":{"other":{"$literal":"x"}}}}]}],"fragments":[{"name":"F","parameters":[{"name":"value","type":"String","default":"ok"}],"select":[{"$call":{"name":"text"}}]}]}`,
			code: "UNKNOWN_ARGUMENT",
		},
		{
			name: "null rejected for non-null parameter",
			body: `"capabilities":["language.fragment-parameters-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"F","args":{"value":{"$literal":null}}}}]}],"fragments":[{"name":"F","parameters":[{"name":"value","type":"String","required":true}],"select":[{"$call":{"name":"text"}}]}]}`,
			code: "TYPE_MISMATCH",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := `{"version":"1",` + test.body + `}`
			request := decodeRuntimeRequestWithOptions(t, input, protocol.DecodeOptions{
				Capabilities: map[string]bool{runtime.FragmentParametersCapability: true},
			})
			_, err := runtime.Prepare(snapshot, request)
			if err == nil {
				t.Fatalf("Prepare succeeded, want %s", test.code)
			}
			var validation *runtime.ValidationErrors
			if !errors.As(err, &validation) {
				t.Fatalf("Prepare error = %T %v", err, err)
			}
			codes := make([]string, 0, len(validation.Issues()))
			for _, issue := range validation.Issues() {
				codes = append(codes, issue.Diagnostic.Code)
			}
			if !slices.Contains(codes, test.code) {
				t.Fatalf("codes = %v, want %s", codes, test.code)
			}
		})
	}
}

func TestPrepareRejectsExponentialFragmentExpansionAtPortableBudget(t *testing.T) {
	t.Parallel()
	fragments := make([]any, 15)
	for index := len(fragments) - 1; index >= 0; index-- {
		selections := []any{}
		if index+1 < len(fragments) {
			spread := map[string]any{"$fragment": map[string]any{"name": "F" + string(rune('A'+index+1))}}
			selections = []any{spread, spread}
		}
		fragments[index] = map[string]any{
			"name":   "F" + string(rune('A'+index)),
			"select": selections,
		}
	}
	encoded, err := json.Marshal(map[string]any{
		"version": "1",
		"document": map[string]any{
			"operations": []any{map[string]any{
				"name": "Q", "kind": "query",
				"select": []any{map[string]any{"$fragment": map[string]any{"name": "FA"}}},
			}},
			"fragments": fragments,
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	request := decodeRuntimeRequest(t, string(encoded))
	snapshot, _ := validationRegistry(t)
	_, err = runtime.Prepare(snapshot, request)
	if err == nil {
		t.Fatal("Prepare succeeded beyond the fragment expansion budget")
	}
	var validation *runtime.ValidationErrors
	if !errors.As(err, &validation) {
		t.Fatalf("Prepare error = %T %v", err, err)
	}
	codes := make([]string, 0, len(validation.Issues()))
	for _, issue := range validation.Issues() {
		codes = append(codes, issue.Diagnostic.Code)
	}
	if !slices.Contains(codes, "FRAGMENT_EXPANSION_LIMIT") {
		t.Fatalf("codes = %v, want FRAGMENT_EXPANSION_LIMIT", codes)
	}
}
