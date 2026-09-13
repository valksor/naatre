package protocol_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestDecodeRequestNegotiatesExactExtensionVersion(t *testing.T) {
	t.Parallel()
	options := protocol.DecodeOptions{
		Capabilities: map[string]bool{"com.example.audit-1": true},
		Extensions: map[string]protocol.ExtensionSupport{
			"com.example.audit": {Version: "1.2.3", Capability: "com.example.audit-1"},
		},
	}
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","capabilities":["com.example.audit-1"],"extensions":{"com.example.audit":{"level":2}},"document":{"requires":["com.example.audit-1"],"operations":[]}}`), options)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	negotiated := request.NegotiatedExtensions()
	if len(negotiated) != 1 || negotiated[0] != (protocol.NegotiatedExtension{ID: "com.example.audit", Version: "1.2.3", Capability: "com.example.audit-1"}) {
		t.Fatalf("negotiated = %#v", negotiated)
	}
	payload, ok := request.Extension("com.example.audit")
	if !ok || string(payload) != `{"level":2}` {
		t.Fatalf("payload = %s, %t", payload, ok)
	}
	negotiated[0].Version = "changed"
	if request.NegotiatedExtensions()[0].Version != "1.2.3" {
		t.Fatal("negotiated extension mutated through accessor")
	}
}

func TestDecodeOptionsAcceptSemVerHyphenIdentifiers(t *testing.T) {
	t.Parallel()
	_, err := protocol.DecodeRequest([]byte(`{"version":"1","document":{"operations":[]}}`), protocol.DecodeOptions{
		Extensions: map[string]protocol.ExtensionSupport{
			"com.example.audit": {Version: "1.0.0-alpha--beta+build--meta", Capability: "com.example.audit-1"},
		},
	})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
}

func TestDecodeOptionsRejectExtensionIDLabelEndingInHyphen(t *testing.T) {
	t.Parallel()
	_, err := protocol.DecodeRequest([]byte(`{"version":"1","document":{"operations":[]}}`), protocol.DecodeOptions{
		Extensions: map[string]protocol.ExtensionSupport{
			"com.example-": {Version: "1.0.0", Capability: "com.example-1"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid extension support") {
		t.Fatalf("DecodeRequest invalid extension ID = %v", err)
	}
}

func TestDecodeRequestRejectsUnknownRequiredAndUnnegotiatedExtension(t *testing.T) {
	t.Parallel()
	options := protocol.DecodeOptions{
		Capabilities: map[string]bool{"com.example.audit-1": true},
		Extensions: map[string]protocol.ExtensionSupport{
			"com.example.audit": {Version: "1.0.0", Capability: "com.example.audit-1"},
		},
	}
	tests := []struct {
		name  string
		input string
		code  string
	}{
		{"version skew", `{"version":"1","capabilities":["com.example.audit-2"],"document":{"operations":[]}}`, "UNSUPPORTED_CAPABILITY"},
		{"payload without capability", `{"version":"1","extensions":{"com.example.audit":{}},"document":{"operations":[]}}`, "UNSUPPORTED_EXTENSION"},
		{"unknown payload", `{"version":"1","extensions":{"org.unknown.metadata":{}},"document":{"operations":[]}}`, "UNSUPPORTED_EXTENSION"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := protocol.DecodeRequest([]byte(test.input), options)
			var diagnostic *protocol.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != test.code {
				t.Fatalf("DecodeRequest error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestDecodeRequestRejectsSemanticExtensionPayloadMissingFromDocumentRequirements(t *testing.T) {
	t.Parallel()
	options := protocol.DecodeOptions{
		Capabilities: map[string]bool{"com.example.audit-1": true},
		Extensions: map[string]protocol.ExtensionSupport{
			"com.example.audit": {Version: "1.0.0", Capability: "com.example.audit-1"},
		},
	}
	_, err := protocol.DecodeRequest([]byte(`{"version":"1","capabilities":["com.example.audit-1"],"extensions":{"com.example.audit":{"level":2}},"document":{"operations":[]}}`), options)
	var diagnostic *protocol.Diagnostic
	if !errors.As(err, &diagnostic) || diagnostic.Code != "UNPINNED_EXTENSION" || diagnostic.Clause != "EXT-100" {
		t.Fatalf("DecodeRequest error = %#v", err)
	}
}

func TestDecodeRequestIgnoresOnlyExplicitOptionalMetadata(t *testing.T) {
	t.Parallel()
	options := protocol.DecodeOptions{
		Extensions: map[string]protocol.ExtensionSupport{
			"com.example.hint": {Version: "1.0.0", Capability: "com.example.hint-1", OptionalMetadata: true},
		},
		IgnorableExtensionMetadata: map[string]bool{"org.unknown.trace": true},
	}
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","extensions":{"com.example.hint":{"secret":"discarded"},"org.unknown.trace":{"trace":"discarded"}},"document":{"operations":[]}}`), options)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if _, ok := request.Extension("com.example.hint"); ok {
		t.Fatal("optional unnegotiated metadata was retained")
	}
	if _, ok := request.Extension("org.unknown.trace"); ok {
		t.Fatal("unknown ignorable metadata was retained")
	}
	if len(request.NegotiatedExtensions()) != 0 {
		t.Fatalf("negotiated extensions = %#v", request.NegotiatedExtensions())
	}
}

func TestDecodeResponseReportsExactNegotiatedExtension(t *testing.T) {
	t.Parallel()
	options := protocol.DecodeOptions{
		Capabilities: map[string]bool{"com.example.audit-1": true},
		Extensions: map[string]protocol.ExtensionSupport{
			"com.example.audit": {Version: "1.4.0", Capability: "com.example.audit-1"},
		},
	}
	response, err := protocol.DecodeResponse([]byte(`{"requestId":"s-1","data":{},"capabilities":["com.example.audit-1"],"extensions":{"com.example.audit":{"ok":true}}}`), options)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	negotiated := response.NegotiatedExtensions()
	if len(negotiated) != 1 || negotiated[0].Version != "1.4.0" {
		t.Fatalf("negotiated = %#v", negotiated)
	}
	if payload, ok := response.Extension("com.example.audit"); !ok || string(payload) != `{"ok":true}` {
		t.Fatalf("extension payload = %s, %t", payload, ok)
	}
}

func TestDecodeOptionsPreserveLegacyRawNamespaces(t *testing.T) {
	t.Parallel()
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","extensions":{"com.example.legacy":{"raw":true}},"document":{"operations":[]}}`), protocol.DecodeOptions{
		ExtensionNamespaces: map[string]bool{"com.example.legacy": true},
	})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if payload, ok := request.Extension("com.example.legacy"); !ok || string(payload) != `{"raw":true}` {
		t.Fatalf("legacy payload = %s, %t", payload, ok)
	}
}

func TestDecodeOptionsPreserveLegacyNumericDNSLabel(t *testing.T) {
	t.Parallel()
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","extensions":{"3rd.example.legacy":{"raw":true}},"document":{"operations":[]}}`), protocol.DecodeOptions{
		ExtensionNamespaces: map[string]bool{"3rd.example.legacy": true},
	})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if _, ok := request.Extension("3rd.example.legacy"); !ok {
		t.Fatal("legacy numeric DNS namespace was not preserved")
	}
}

func TestDecodeOptionsRejectReservedPayloadNamespacePolicies(t *testing.T) {
	t.Parallel()
	requestInput := []byte(`{"version":"1","document":{"operations":[]}}`)
	responseInput := []byte(`{"requestId":"s-1","data":{},"capabilities":[],"extensions":{}}`)
	policies := []protocol.DecodeOptions{
		{ExtensionNamespaces: map[string]bool{"core.private": true}},
		{IgnorableExtensionMetadata: map[string]bool{"naatre.private": true}},
	}
	for _, policy := range policies {
		if _, err := protocol.DecodeRequest(requestInput, policy); err == nil || !strings.Contains(err.Error(), "extension namespace") {
			t.Fatalf("DecodeRequest reserved policy = %v", err)
		}
		if _, err := protocol.DecodeResponse(responseInput, policy); err == nil || !strings.Contains(err.Error(), "extension namespace") {
			t.Fatalf("DecodeResponse reserved policy = %v", err)
		}
	}
	falseEntries := protocol.DecodeOptions{
		ExtensionNamespaces:        map[string]bool{"not a namespace": false},
		IgnorableExtensionMetadata: map[string]bool{"also invalid": false},
	}
	if _, err := protocol.DecodeRequest(requestInput, falseEntries); err != nil {
		t.Fatalf("false-valued policy entries changed decode: %v", err)
	}
}
