package protocol_test

import (
	"errors"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestDecodeRequestPreservesOrderedSelectionsAndLosslessVariables(t *testing.T) {
	t.Parallel()

	input := []byte(`{
  "variables":{"large":9007199254740993},
  "document":{"operations":[{"select":[
    {"$field":{"name":"first"}},
    {"$field":{"name":"second","as":"renamed"}}
  ],"kind":"query","name":"Get"}]},
  "capabilities":[],"extensions":{},"id":"client-1","version":"1"
}`)

	request, err := protocol.DecodeRequest(input, protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if got, ok := request.ID(); !ok || got != "client-1" {
		t.Fatalf("ID() = %q, %t; want client-1, true", got, ok)
	}
	operations := request.Document().Operations()
	if len(operations) != 1 || operations[0].Name() != "Get" || operations[0].Kind() != protocol.Query {
		t.Fatalf("operations = %#v, want one Get query", operations)
	}
	selections := operations[0].Selections()
	if len(selections) != 2 {
		t.Fatalf("selection count = %d, want 2", len(selections))
	}
	if selections[0].Name() != "first" || selections[1].Name() != "second" || selections[1].Alias() != "renamed" {
		t.Fatalf("selection order = %q then %q as %q", selections[0].Name(), selections[1].Name(), selections[1].Alias())
	}
	variable, ok := request.Variable("large")
	if !ok || string(variable) != "9007199254740993" {
		t.Fatalf("large variable = %s, %t", variable, ok)
	}
}

func TestDecodeRequestUsesPortableCaseSensitiveCapabilityIdentifiers(t *testing.T) {
	t.Parallel()

	input := []byte(`{"version":"1","document":{"operations":[]},"capabilities":["Vendor.Audit-1"]}`)
	options := protocol.DecodeOptions{Capabilities: map[string]bool{"Vendor.Audit-1": true}}
	if _, err := protocol.DecodeRequest(input, options); err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	options.Capabilities = map[string]bool{"vendor.audit-1": true}
	if _, err := protocol.DecodeRequest(input, options); err == nil {
		t.Fatal("case-insensitive capability lookup succeeded")
	}
}

func TestDecodeRequestBindsPersistedReferenceCanonicalVersion(t *testing.T) {
	t.Parallel()

	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","persisted":{"algorithm":"sha-256","canonicalVersion":"c14n-1","digest":"`+digest+`"}}`), protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	reference, ok := request.Persisted()
	if !ok || reference.Algorithm != "sha-256" || reference.CanonicalVersion != "c14n-1" || reference.Digest != digest {
		t.Fatalf("persisted reference = %#v, %t", reference, ok)
	}
	for _, persisted := range []string{
		`{"algorithm":"sha-256","digest":"` + digest + `"}`,
		`{"algorithm":"sha-256","canonicalVersion":"c14n-2","digest":"` + digest + `"}`,
		`{"algorithm":"sha-256","canonicalVersion":"c14n-1","digest":"` + digest + `","purpose":"document"}`,
	} {
		if _, err := protocol.DecodeRequest([]byte(`{"version":"1","persisted":`+persisted+`}`), protocol.DecodeOptions{}); err == nil {
			t.Fatalf("DecodeRequest accepted persisted reference %s", persisted)
		}
	}
}

func TestDecodedRequestAccessorsCannotMutateAST(t *testing.T) {
	t.Parallel()

	input := []byte(`{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$field":{"name":"stable"}}]}]},"variables":{"text":"safe"}}`)
	request, err := protocol.DecodeRequest(input, protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	operations := request.Document().Operations()
	operations[0] = protocol.Operation{}
	if got := request.Document().Operations()[0].Name(); got != "Get" {
		t.Fatalf("operation mutated through accessor: %q", got)
	}

	variable, _ := request.Variable("text")
	variable[1] = 'X'
	variableAgain, _ := request.Variable("text")
	if string(variableAgain) != `"safe"` {
		t.Fatalf("variable mutated through accessor: %s", variableAgain)
	}
}

func TestDecodeRequestRejectsMalformedEnvelopeBeforeValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		code    string
		pointer string
	}{
		{name: "fragments are not silently discarded", input: `{"version":"1","document":{"operations":[],"fragments":[]}}`, code: "UNKNOWN_FIELD"},
		{name: "operation variables are not silently discarded", input: `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","variables":{},"select":[]}]}}`, code: "UNKNOWN_FIELD"},
		{name: "fail fast is not silently discarded", input: `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","failFast":true,"select":[]}]}}`, code: "UNKNOWN_FIELD"},
		{name: "binding is not silently discarded", input: `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$call":{"name":"get","bind":"x"}}]}]}}`, code: "UNKNOWN_FIELD"},
		{name: "escaped duplicate", input: `{"version":"1","id":"first","\u0069d":"second","document":{"operations":[]}}`, code: "DUPLICATE_KEY", pointer: "/id"},
		{name: "unknown field", input: `{"version":"1","document":{"operations":[]},"kind":"query"}`, code: "UNKNOWN_FIELD", pointer: "/kind"},
		{name: "trailing data", input: `{"version":"1","document":{"operations":[]}} true`, code: "TRAILING_DATA", pointer: ""},
		{name: "unsupported version", input: `{"version":"2","document":{"operations":[]}}`, code: "UNSUPPORTED_VERSION", pointer: "/version"},
		{name: "conflicting sources", input: `{"version":"1","document":{"operations":[]},"persisted":{"algorithm":"sha-256","digest":"00"}}`, code: "CONFLICTING_SOURCE", pointer: ""},
		{name: "missing source", input: `{"version":"1"}`, code: "MISSING_SOURCE", pointer: ""},
		{name: "unpaired surrogate", input: `{"version":"1","id":"\ud800","document":{"operations":[]}}`, code: "INVALID_UNICODE", pointer: "/id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := protocol.DecodeRequest([]byte(tt.input), protocol.DecodeOptions{})
			assertDiagnostic(t, err, tt.code, tt.pointer)
		})
	}
}

func TestRequestExtensionsAreImmutableAndObservable(t *testing.T) {
	t.Parallel()
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","document":{"operations":[]},"extensions":{"com.example.trace":{"x":1}}}`), protocol.DecodeOptions{ExtensionNamespaces: map[string]bool{"com.example.trace": true}})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	raw, ok := request.Extension("com.example.trace")
	if !ok || string(raw) != `{"x":1}` {
		t.Fatalf("Extension = %s, %t", raw, ok)
	}
	raw[1] = 'X'
	again, _ := request.Extension("com.example.trace")
	if string(again) != `{"x":1}` {
		t.Fatalf("extension mutated: %s", again)
	}
}

func TestDecodeRequestEnforcesLimitsAndCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		options protocol.DecodeOptions
		code    string
	}{
		{
			name:    "bytes",
			input:   `{"version":"1","document":{"operations":[]}}`,
			options: protocol.DecodeOptions{Limits: protocol.Limits{MaxBytes: 8}},
			code:    "LIMIT_BYTES",
		},
		{
			name:    "numeric token",
			input:   `{"version":"1","document":{"operations":[]},"variables":{"n":123456}}`,
			options: protocol.DecodeOptions{Limits: protocol.Limits{MaxNumberBytes: 4}},
			code:    "LIMIT_NUMBER",
		},
		{
			name:    "unsupported capability",
			input:   `{"version":"1","document":{"operations":[]},"capabilities":["stream.sse"]}`,
			options: protocol.DecodeOptions{Capabilities: map[string]bool{"core": true}},
			code:    "UNSUPPORTED_CAPABILITY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := protocol.DecodeRequest([]byte(tt.input), tt.options)
			assertDiagnostic(t, err, tt.code, "")
		})
	}
}

func TestDecodeRequestRejectsSelectionFieldsItCannotPreserve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		code  string
	}{
		{
			name:  "call arguments are not silently discarded",
			input: `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$call":{"name":"get","args":{"id":"1"}}}]}]}}`,
			code:  "UNKNOWN_FIELD",
		},
		{
			name:  "wrong alias type",
			input: `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$call":{"name":"get","as":7}}]}]}}`,
			code:  "TYPE_MISMATCH",
		},
		{
			name:  "wrong child selection type",
			input: `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$field":{"name":"user","select":{}}}]}]}}`,
			code:  "TYPE_MISMATCH",
		},
		{
			name:  "unsupported parallel is explicit",
			input: `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$parallel":{"select":[]}}]}]}}`,
			code:  "UNSUPPORTED_SELECTION",
		},
		{
			name:  "call payload must be object",
			input: `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$call":"get"}]}]}}`,
			code:  "TYPE_MISMATCH",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := protocol.DecodeRequest([]byte(tt.input), protocol.DecodeOptions{})
			assertDiagnostic(t, err, tt.code, "")
		})
	}
}

func TestDecodeRequestAssignsDocumentFailuresToValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		code   string
		clause string
	}{
		{"document type", `{"version":"1","document":[]}`, "TYPE_MISMATCH", "LANG-001"},
		{"document member", `{"version":"1","document":{"operations":[],"extra":true}}`, "UNKNOWN_FIELD", "LANG-001"},
		{"operation shape", `{"version":"1","document":{"operations":[null]}}`, "TYPE_MISMATCH", "LANG-002"},
		{"operation name", `{"version":"1","document":{"operations":[{"name":"bad name","kind":"query","select":[]}]}}`, "INVALID_IDENTIFIER", "LANG-002"},
		{"operation kind", `{"version":"1","document":{"operations":[{"name":"Get","kind":"read","select":[]}]}}`, "INVALID_OPERATION_KIND", "CORE-003"},
		{"selection shape", `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[null]}]}}`, "INVALID_SELECTION", "LANG-003"},
		{"selection payload", `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$field":null}]}]}}`, "TYPE_MISMATCH", "LANG-003"},
		{"selection name", `{"version":"1","document":{"operations":[{"name":"Get","kind":"query","select":[{"$field":{"name":"bad name"}}]}]}}`, "INVALID_IDENTIFIER", "LANG-002"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := protocol.DecodeRequest([]byte(tt.input), protocol.DecodeOptions{})
			var diagnostic *protocol.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != tt.code || diagnostic.Clause != tt.clause || diagnostic.Phase != "validate" {
				t.Fatalf("DecodeRequest error = %v, want %s/%s in validate phase", err, tt.code, tt.clause)
			}
		})
	}
}

func assertDiagnostic(t *testing.T, err error, code, pointer string) {
	t.Helper()
	if err == nil {
		t.Fatalf("DecodeRequest succeeded, want %s", code)
	}
	var diagnostic *protocol.Diagnostic
	if !errors.As(err, &diagnostic) {
		t.Fatalf("error %T = %v, want *protocol.Diagnostic", err, err)
	}
	if diagnostic.Code != code {
		t.Fatalf("diagnostic code = %q, want %q (%v)", diagnostic.Code, code, diagnostic)
	}
	if pointer != "" && diagnostic.Pointer != pointer {
		t.Fatalf("diagnostic pointer = %q, want %q", diagnostic.Pointer, pointer)
	}
	if diagnostic.Offset < 0 || diagnostic.Line < 1 || diagnostic.Column < 1 {
		t.Fatalf("invalid diagnostic location: %#v", diagnostic)
	}
}
