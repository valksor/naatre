package runtime_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestPrepareAccumulatesIndependentErrorsInDocumentOrder(t *testing.T) {
	t.Parallel()
	snapshot := frozenRegistry(t, runtime.NewRegistry(coreTypes(t)))
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"missingOne"}},{"$call":{"name":"missingTwo"}}]}]}}`)

	_, err := runtime.Prepare(snapshot, request)
	var validationErr *runtime.ValidationErrors
	if !errors.As(err, &validationErr) {
		t.Fatalf("Prepare error = %T %v, want ValidationErrors", err, err)
	}
	issues := validationErr.Issues()
	if len(issues) != 2 {
		t.Fatalf("issues = %#v, want two", issues)
	}
	if issues[0].Diagnostic.Code != "UNKNOWN_CALL" || issues[1].Diagnostic.Code != "UNKNOWN_CALL" ||
		issues[0].Diagnostic.Pointer != "/document/operations/0/select/0/$call/name" || issues[1].Diagnostic.Pointer != "/document/operations/0/select/1/$call/name" {
		t.Fatalf("issues are not in document order: %#v", issues)
	}
	want := err.Error()
	for range 8 {
		_, again := runtime.Prepare(snapshot, request)
		if again == nil || again.Error() != want {
			t.Fatalf("repeated diagnostic = %v, want %q", again, want)
		}
	}
}

func TestPlanDescriptionIsImmutableConcurrentAndRequestNeutral(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	requestJSON := func(id, variable string) string {
		return `{"version":"1","id":"` + id + `","variables":{"id":"` + variable + `"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"lookup","args":{"id":{"$var":"id"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`
	}
	first, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, requestJSON("a", "u-1")))
	if err != nil {
		t.Fatalf("Prepare first: %v", err)
	}
	second, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, requestJSON("long-correlation-id", "different-user")))
	if err != nil {
		t.Fatalf("Prepare second: %v", err)
	}
	firstJSON, err := json.Marshal(first.Description())
	if err != nil {
		t.Fatalf("marshal first description: %v", err)
	}
	secondJSON, err := json.Marshal(second.Description())
	if err != nil {
		t.Fatalf("marshal second description: %v", err)
	}
	if !bytes.Equal(firstJSON, secondJSON) || bytes.Contains(firstJSON, []byte("u-1")) || bytes.Contains(firstJSON, []byte(`"id":"a"`)) {
		t.Fatalf("request-bound data changed reusable description: %s != %s", firstJSON, secondJSON)
	}
	description := first.Description()
	description.Nodes[0].Name = "mutated"
	description.Nodes[0].Children[0].Output = "Mutated"
	again := first.Description()
	if again.Nodes[0].Name != "lookup" || again.Nodes[0].Children[0].Output != schema.TypeID(schema.String) {
		t.Fatalf("plan mutated through description: %#v", again)
	}
	if again.Nodes[0].Arguments != "LookupInput" || again.Nodes[0].Children[0].Current != "User" {
		t.Fatalf("resolved node types = %#v", again.Nodes[0])
	}
	const readers = 32
	var wait sync.WaitGroup
	wait.Add(readers)
	for range readers {
		go func() {
			defer wait.Done()
			current, marshalErr := json.Marshal(first.Description())
			if marshalErr != nil || !bytes.Equal(current, firstJSON) {
				t.Errorf("concurrent description = %s, %v", current, marshalErr)
			}
		}()
	}
	wait.Wait()
}

func TestPrepareRequiresNegotiatedDocumentCapabilities(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	input := `{"version":"1","document":{"requires":["core.language-1","vendor.audit-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}}`
	if got := validationCodes(t, prepareError(snapshot, decodeRuntimeRequest(t, input))); !slices.Equal(got, []string{"UNSUPPORTED_CAPABILITY"}) {
		t.Fatalf("codes = %v", got)
	}
	negotiated := `{"version":"1","capabilities":["vendor.audit-1"],"document":{"requires":["core.language-1","vendor.audit-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}}`
	request, err := protocol.DecodeRequest([]byte(negotiated), protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.audit-1": true}})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if _, err := runtime.Prepare(snapshot, request); err != nil {
		t.Fatalf("Prepare negotiated capability: %v", err)
	}
}

func TestPrepareValidatesVariablesAndCallArguments(t *testing.T) {
	t.Parallel()
	snapshot, calls := validationRegistry(t)
	tests := []struct {
		name     string
		input    string
		codes    []string
		pointers []string
	}{
		{
			name:  "valid variable",
			input: `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"lookup","args":{"id":{"$var":"id"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`,
		},
		{
			name:  "required request variable missing",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"lookup","args":{"id":{"$var":"id"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`,
			codes: []string{"MISSING_VARIABLE"},
		},
		{
			name:  "undefined variable use",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$var":"absent"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`,
			codes: []string{"UNKNOWN_VARIABLE"},
		},
		{
			name:  "unused variable",
			input: `{"version":"1","variables":{"unused":"x"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"unused","type":"String"}],"select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`,
			codes: []string{"UNUSED_VARIABLE"},
		},
		{
			name:  "variable type mismatch",
			input: `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"String"}],"select":[{"$call":{"name":"lookup","args":{"id":{"$var":"id"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`,
			codes: []string{"TYPE_MISMATCH"},
		},
		{
			name:  "missing optional variable defers required consumer failure",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"value","type":"String"}],"select":[{"$call":{"name":"consume","args":{"value":{"$var":"value"}}}}]}]}}`,
		},
		{
			name:  "invalid literal argument",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":7}},"select":[{"$field":{"name":"name"}}]}}]}]}}`,
			codes: []string{"TYPE_MISMATCH"},
		},
		{
			name:     "unknown argument",
			input:    `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"other":{"$literal":"u-1"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`,
			codes:    []string{"MISSING_ARGUMENT", "UNKNOWN_ARGUMENT"},
			pointers: []string{"/document/operations/0/select/0", "/document/operations/0/select/0/$call/args/other"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := calls.Load()
			_, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, tt.input))
			if len(tt.codes) == 0 {
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
			} else if got := validationCodes(t, err); !slices.Equal(got, tt.codes) {
				t.Fatalf("codes = %v, want %v", got, tt.codes)
			} else if tt.pointers != nil {
				var validationErr *runtime.ValidationErrors
				errors.As(err, &validationErr)
				issues := validationErr.Issues()
				pointers := make([]string, len(issues))
				for index, issue := range issues {
					pointers[index] = issue.Diagnostic.Pointer
				}
				if !slices.Equal(pointers, tt.pointers) {
					t.Fatalf("pointers = %v, want %v", pointers, tt.pointers)
				}
			}
			if calls.Load() != before {
				t.Fatal("Prepare invoked an application handler")
			}
		})
	}
}

func TestPrepareValidatesNestedSelectionsFragmentsAndDirectives(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	tests := []struct {
		name  string
		input string
		codes []string
	}{
		{
			name:  "valid fragment and directive",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$fragment":{"name":"Identity","directives":[{"name":"include","arguments":{"if":{"$literal":true}}}]}}]}}]}],"fragments":[{"name":"Identity","on":"User","select":[{"$field":{"name":"name"}}]}]}}`,
		},
		{
			name:  "valid nested object call",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$call":{"name":"label","args":{"value":{"$literal":"prefix"}}}}]}}]}]}}`,
		},
		{
			name:  "unknown nested field",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$field":{"name":"missing"}}]}}]}]}}`,
			codes: []string{"UNKNOWN_FIELD"},
		},
		{
			name:  "unknown fragment",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"Missing"}}]}]}}`,
			codes: []string{"UNKNOWN_FRAGMENT"},
		},
		{
			name:  "fragment cycle",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"Loop"}}]}],"fragments":[{"name":"Loop","select":[{"$fragment":{"name":"Loop"}}]}]}}`,
			codes: []string{"FRAGMENT_CYCLE"},
		},
		{
			name:  "malformed include directive",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"directives":[{"name":"include","arguments":{"if":{"$literal":"yes"}}}],"select":[{"$field":{"name":"name"}}]}}]}]}}`,
			codes: []string{"TYPE_MISMATCH"},
		},
		{
			name:  "unknown directive",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"directives":[{"name":"untrusted"}],"select":[{"$field":{"name":"name"}}]}}]}]}}`,
			codes: []string{"UNKNOWN_DIRECTIVE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, tt.input))
			if len(tt.codes) == 0 {
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				return
			}
			if got := validationCodes(t, err); !slices.Equal(got, tt.codes) {
				t.Fatalf("codes = %v, want %v", got, tt.codes)
			}
		})
	}
}

func TestPrepareValidatesParallelIndependence(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	tests := []struct {
		name  string
		input string
		code  string
	}{
		{
			name:  "serial-only read",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"serial"}}]}}]}]}}`,
			code:  "PARALLEL_THREAD_UNSAFE",
		},
		{
			name:  "mutation without registry permission",
			input: `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$parallel":{"select":[{"$call":{"name":"write"}}]}}]}]}}`,
			code:  "PARALLEL_MUTATION_NOT_ALLOWED",
		},
		{
			name:  "parallel mutation profile not negotiated",
			input: `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$parallel":{"select":[{"$call":{"name":"parallelWrite"}}]}}]}]}}`,
			code:  "UNSUPPORTED_CAPABILITY",
		},
		{
			name:  "transaction-required read",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"transactional"}}]}}]}]}}`,
			code:  "PARALLEL_TRANSACTION",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validationCodes(t, prepareError(snapshot, decodeRuntimeRequest(t, tt.input))); !slices.Contains(got, tt.code) {
				t.Fatalf("codes = %v, want %s", got, tt.code)
			}
		})
	}

	input := []byte(`{"version":"1","capabilities":["mutation.parallel-1"],"document":{"requires":["mutation.parallel-1"],"operations":[{"name":"M","kind":"mutation","select":[{"$parallel":{"select":[{"$call":{"name":"parallelWrite","as":"left"}},{"$call":{"name":"parallelWrite","as":"right"}}]}}]}]}}`)
	request, err := protocol.DecodeRequest(input, protocol.DecodeOptions{Capabilities: map[string]bool{"mutation.parallel-1": true}})
	if err != nil {
		t.Fatalf("DecodeRequest parallel mutation: %v", err)
	}
	if _, err := runtime.Prepare(snapshot, request); err != nil {
		t.Fatalf("Prepare permitted parallel mutation: %v", err)
	}
}

func TestPrepareValidatesResultBindingsAndCollectionScopes(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	tests := []struct {
		name  string
		input string
		codes []string
	}{
		{
			name:  "prior result binding",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","bind":"prior"}},{"$call":{"name":"consume","args":{"value":{"$result":"prior"}}}}]}]}}`,
		},
		{
			name:  "forward result binding",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"consume","args":{"value":{"$result":"later"}}}},{"$call":{"name":"text","bind":"later"}}]}]}}`,
			codes: []string{"RESULT_FORWARD_REFERENCE"},
		},
		{
			name:  "duplicate binding",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","bind":"same"}},{"$call":{"name":"text","bind":"same"}}]}]}}`,
			codes: []string{"DUPLICATE_RESPONSE_NAME", "DUPLICATE_BINDING"},
		},
		{
			name:  "valid explicit map",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`,
		},
		{
			name:  "collection operation on scalar",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","select":[{"$map":{"as":"items","select":[]}}]}}]}]}}`,
			codes: []string{"INVALID_COLLECTION_SCOPE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, tt.input))
			if len(tt.codes) == 0 {
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				return
			}
			if got := validationCodes(t, err); !slices.Equal(got, tt.codes) {
				t.Fatalf("codes = %v, want %v", got, tt.codes)
			}
		})
	}
}

func TestPrepareMatchesPortableValidationCodes(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	tests := []struct {
		name    string
		body    string
		code    string
		pointer string
	}{
		{
			name: "implicit list item mapping",
			body: `{"operations":[{"name":"Q","kind":"query","select":[{"$pipeline":{"as":"names","stages":[{"$call":{"name":"users"}},{"$field":{"name":"name"}}]}}]}]}`,
			code: "INVALID_COLLECTION_SCOPE", pointer: "/document/operations/0/select/0/$pipeline/stages/1",
		},
		{
			name: "slice start after end",
			body: `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$slice":{"as":"items","start":3,"end":2}}]}}]}]}`,
			code: "INVALID_SLICE", pointer: "/document/operations/0/select/0/$call/select/0/$slice",
		},
		{
			name: "unnest without current object",
			body: `{"operations":[{"name":"Q","kind":"query","select":[{"$unnest":{"select":[{"$field":{"name":"name"}}]}}]}]}`,
			code: "INVALID_UNNEST", pointer: "/document/operations/0/select/0/$unnest",
		},
		{
			name: "self result reference",
			body: `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"consume","bind":"self","args":{"value":{"$result":"self"}}}}]}]}`,
			code: "RESULT_FORWARD_REFERENCE", pointer: "/document/operations/0/select/0/$call/args/value/$result",
		},
		{
			name: "statically skipped write in query",
			body: `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"write","directives":[{"name":"include","arguments":{"if":{"$literal":false}}}]}}]}]}`,
			code: "EFFECT_NOT_ALLOWED", pointer: "/document/operations/0/select/0/$call/name",
		},
		{
			name: "subscription transitively reaches write",
			body: `{"operations":[{"name":"S","kind":"subscription","select":[{"$call":{"name":"watch","select":[{"$field":{"name":"mutateName"}}]}}]}]}`,
			code: "EFFECT_NOT_ALLOWED", pointer: "/document/operations/0/select/0/$call/select/0/$field/name",
		},
		{
			name: "cross parallel result reference",
			body: `{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"text","bind":"branchValue"}},{"$call":{"name":"consume","args":{"value":{"$result":"branchValue"}}}}]}}]}]}`,
			code: "RESULT_SCOPE", pointer: "/document/operations/0/select/0/$parallel/select/1/$call/args/value/$result",
		},
		{
			name: "impossible fragment type condition",
			body: `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$fragment":{"name":"Wrong"}}]}}]}],"fragments":[{"name":"Wrong","on":"Users","select":[{"$current":{"as":"value"}}]}]}`,
			code: "IMPOSSIBLE_TYPE_CONDITION",
		},
		{
			name: "unnest parent collision",
			body: `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$field":{"name":"name"}},{"$unnest":{"select":[{"$field":{"name":"name","as":"name"}}]}}]}}]}]}`,
			code: "DUPLICATE_RESPONSE_NAME", pointer: "/document/operations/0/select/0/$call/select/1/$unnest/select/0/$field/as",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := prepareError(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":`+tt.body+`}`))
			codes := validationCodes(t, err)
			if !slices.Contains(codes, tt.code) {
				t.Fatalf("codes = %v, want %s", codes, tt.code)
			}
			if tt.pointer != "" {
				var validationErr *runtime.ValidationErrors
				errors.As(err, &validationErr)
				for _, issue := range validationErr.Issues() {
					if issue.Diagnostic.Code == tt.code && issue.Diagnostic.Pointer != tt.pointer {
						t.Fatalf("%s pointer = %q, want %q", tt.code, issue.Diagnostic.Pointer, tt.pointer)
					}
				}
			}
		})
	}
}

func TestPrepareFragmentCycleCarriesDefinitionAndUseSources(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	input := `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"Loop"}}]}],"fragments":[{"name":"Loop","select":[{"$fragment":{"name":"Loop"}}]}]}}`
	var validationErr *runtime.ValidationErrors
	if !errors.As(prepareError(snapshot, decodeRuntimeRequest(t, input)), &validationErr) {
		t.Fatal("Prepare did not return ValidationErrors")
	}
	issues := validationErr.Issues()
	if len(issues) != 1 || issues[0].Diagnostic.Code != "FRAGMENT_CYCLE" || len(issues[0].Related) < 2 {
		t.Fatalf("fragment issue = %#v", issues)
	}
	if issues[0].Diagnostic.Pointer != "/document/fragments/0/select/0" ||
		issues[0].Related[0].Pointer != "/document/fragments/0" || issues[0].Related[1].Pointer != "/document/operations/0/select/0" {
		t.Fatalf("fragment sources = %#v", issues[0])
	}
	issues[0].Related[0].Pointer = "mutated"
	if validationErr.Issues()[0].Related[0].Pointer == "mutated" {
		t.Fatal("validation error mutated through Issues")
	}
}

func validationCodes(t *testing.T, err error) []string {
	t.Helper()
	var validationErr *runtime.ValidationErrors
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want ValidationErrors", err, err)
	}
	issues := validationErr.Issues()
	codes := make([]string, len(issues))
	for index, issue := range issues {
		codes[index] = issue.Diagnostic.Code
	}
	return codes
}

func prepareError(snapshot runtime.Snapshot, request *protocol.Request) error {
	_, err := runtime.Prepare(snapshot, request)
	return err
}
