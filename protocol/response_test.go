package protocol_test

import (
	"errors"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestDecodeResponsePreservesPartialDataAndTypedErrorPaths(t *testing.T) {
	t.Parallel()
	input := []byte(`{"id":"c-7","requestId":"s-9","data":{"users":[{"name":"Ada"},null]},"errors":[{"code":"FIELD_FAILED","message":"field unavailable","path":["users",1,""],"source":{"pointer":"/document/operations/0","line":2,"column":3},"retryable":false,"details":{"com.example.trace":{"span":"x"}}}],"capabilities":["stream.sse"],"extensions":{"com.example.trace":{"request":"x"}}}`)
	options := protocol.DecodeOptions{Capabilities: map[string]bool{"stream.sse": true}, ExtensionNamespaces: map[string]bool{"com.example.trace": true}}
	response, err := protocol.DecodeResponse(input, options)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if id, ok := response.ID(); !ok || id != "c-7" || response.RequestID() != "s-9" {
		t.Fatalf("response identifiers = %q, %t, %q", id, ok, response.RequestID())
	}
	data, ok := response.Data()
	if !ok || string(data) != `{"users":[{"name":"Ada"},null]}` {
		t.Fatalf("response data = %s, %t", data, ok)
	}
	errors := response.Errors()
	if len(errors) != 1 || errors[0].Code() != "FIELD_FAILED" || errors[0].Retryable() {
		t.Fatalf("response errors = %#v", errors)
	}
	if errors[0].Message() != "field unavailable" {
		t.Fatalf("response error message = %q", errors[0].Message())
	}
	capabilities := response.Capabilities()
	if len(capabilities) != 1 || capabilities[0] != "stream.sse" {
		t.Fatalf("response capabilities = %#v", capabilities)
	}
	capabilities[0] = "changed"
	if response.Capabilities()[0] != "stream.sse" {
		t.Fatal("response capabilities mutated through accessor")
	}
	path := errors[0].Path()
	if key, ok := path[0].Key(); !ok || key != "users" {
		t.Fatalf("first path segment = %#v", path[0])
	}
	if index, ok := path[1].Index(); !ok || index != 1 {
		t.Fatalf("second path segment = %#v", path[1])
	}
	if key, ok := path[2].Key(); !ok || key != "" {
		t.Fatalf("empty map-key path segment = %#v", path[2])
	}
	source, ok := errors[0].Source()
	if !ok || source.Pointer() != "/document/operations/0" || source.Line() != 2 || source.Column() != 3 {
		t.Fatalf("error source = %#v, %t", source, ok)
	}
	data[1] = 'X'
	detail, _ := errors[0].Detail("com.example.trace")
	detail[1] = 'X'
	if again, _ := response.Data(); string(again) != `{"users":[{"name":"Ada"},null]}` {
		t.Fatalf("response data mutated through accessor: %s", again)
	}
	again := response.Errors()
	if raw, _ := again[0].Detail("com.example.trace"); string(raw) != `{"span":"x"}` {
		t.Fatalf("error detail mutated through accessor: %s", raw)
	}
}

func TestDecodeResponseRejectsInvalidEnvelopes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		code  string
	}{
		{"missing request id", `{"data":{},"capabilities":[],"extensions":{}}`, "MISSING_FIELD"},
		{"empty response", `{"requestId":"s","capabilities":[],"extensions":{}}`, "EMPTY_RESPONSE"},
		{"empty errors", `{"requestId":"s","errors":[],"capabilities":[],"extensions":{}}`, "INVALID_ERRORS"},
		{"negative path index", `{"requestId":"s","errors":[{"code":"BAD","message":"bad","path":[-1],"retryable":false}],"capabilities":[],"extensions":{}}`, "INVALID_ERROR_PATH"},
		{"fractional path index", `{"requestId":"s","errors":[{"code":"BAD","message":"bad","path":[1.5],"retryable":false}],"capabilities":[],"extensions":{}}`, "INVALID_ERROR_PATH"},
		{"unknown field", `{"requestId":"s","data":{},"capabilities":[],"extensions":{},"stack":"private"}`, "UNKNOWN_FIELD"},
		{"unsupported capability", `{"requestId":"s","data":{},"capabilities":["stream.sse"],"extensions":{}}`, "UNSUPPORTED_CAPABILITY"},
		{"missing capabilities", `{"requestId":"s","data":{},"extensions":{}}`, "MISSING_FIELD"},
		{"malformed capabilities", `{"requestId":"s","data":{},"capabilities":{},"extensions":{}}`, "TYPE_MISMATCH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := protocol.DecodeResponse([]byte(tt.input), protocol.DecodeOptions{})
			var diagnostic *protocol.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != tt.code || diagnostic.Phase != "decode" {
				t.Fatalf("DecodeResponse error = %v, want %s", err, tt.code)
			}
		})
	}
}
