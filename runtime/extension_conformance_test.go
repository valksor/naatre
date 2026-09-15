package runtime_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestExtensionConformanceFixture(t *testing.T) {
	t.Parallel()
	type registryCase struct {
		Name          string   `json:"name"`
		Extensions    []string `json:"extensions"`
		Order         []string `json:"order"`
		ErrorContains string   `json:"errorContains"`
	}
	type protocolCase struct {
		Name       string   `json:"name"`
		Direction  string   `json:"direction"`
		Supports   []string `json:"supports"`
		Ignorable  []string `json:"ignorable"`
		Input      string   `json:"input"`
		Negotiated []string `json:"negotiated"`
		ErrorCode  string   `json:"errorCode"`
	}
	var fixture struct {
		Profile           string                       `json:"profile"`
		Descriptors       []schema.ExtensionDescriptor `json:"descriptors"`
		RegistryCases     []registryCase               `json:"registryCases"`
		ProtocolCases     []protocolCase               `json:"protocolCases"`
		PersistedIdentity struct {
			Left  string `json:"left"`
			Right string `json:"right"`
			Equal bool   `json:"equal"`
		} `json:"persistedIdentity"`
	}
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "extensions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Profile != "core.extensions-1" {
		t.Fatalf("profile = %q", fixture.Profile)
	}
	descriptors := make(map[string]schema.ExtensionDescriptor, len(fixture.Descriptors))
	for _, descriptor := range fixture.Descriptors {
		if err := schema.ValidateExtensionDescriptor(descriptor); err != nil {
			t.Fatalf("descriptor %q: %v", descriptor.ID, err)
		}
		if _, exists := descriptors[descriptor.ID]; exists {
			t.Fatalf("duplicate descriptor %q", descriptor.ID)
		}
		descriptors[descriptor.ID] = descriptor
	}
	discovery, err := schema.ExportDocument(coreTypes(t), nil, nil, schema.ExportOptions{
		Revision: "extension-conformance-r1", Extensions: fixture.Descriptors,
	})
	if err != nil {
		t.Fatalf("export discovery: %v", err)
	}
	canonical, err := discovery.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := schema.ParseDocument(canonical, schema.ImportOptions{})
	if err != nil {
		t.Fatalf("parse discovery: %v", err)
	}
	again, err := parsed.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, again) {
		t.Fatalf("discovery canonical round trip differs: %v", err)
	}

	for _, vector := range fixture.RegistryCases {
		t.Run(vector.Name, func(t *testing.T) {
			registry := runtime.NewRegistry(coreTypes(t))
			if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
				t.Fatal(err)
			}
			for _, id := range vector.Extensions {
				descriptor, exists := descriptors[id]
				if !exists {
					t.Fatalf("unknown descriptor %q", id)
				}
				if err := registry.RegisterExtension(descriptor); err != nil {
					t.Fatalf("RegisterExtension(%s): %v", id, err)
				}
			}
			snapshot, freezeErr := registry.Freeze()
			if vector.ErrorContains != "" {
				if freezeErr == nil || !strings.Contains(freezeErr.Error(), vector.ErrorContains) {
					t.Fatalf("Freeze() = %v, want %q", freezeErr, vector.ErrorContains)
				}
				return
			}
			if freezeErr != nil {
				t.Fatalf("Freeze: %v", freezeErr)
			}
			ordered := snapshot.ExtensionDescriptors()
			ids := make([]string, len(ordered))
			for index := range ordered {
				ids[index] = ordered[index].ID
			}
			if !slices.Equal(ids, vector.Order) {
				t.Fatalf("order = %v, want %v", ids, vector.Order)
			}
		})
	}

	for _, vector := range fixture.ProtocolCases {
		t.Run(vector.Name, func(t *testing.T) {
			options := protocol.DecodeOptions{
				Capabilities: make(map[string]bool), Extensions: make(map[string]protocol.ExtensionSupport),
				IgnorableExtensionMetadata: make(map[string]bool),
			}
			for _, id := range vector.Supports {
				descriptor, exists := descriptors[id]
				if !exists {
					t.Fatalf("unknown support %q", id)
				}
				options.Capabilities[descriptor.Capability] = true
				options.Extensions[id] = protocol.ExtensionSupport{
					Version: descriptor.Version, Capability: descriptor.Capability, OptionalMetadata: descriptor.OptionalMetadata,
				}
			}
			for _, id := range vector.Ignorable {
				options.IgnorableExtensionMetadata[id] = true
			}
			var negotiated []protocol.NegotiatedExtension
			var decodeErr error
			switch vector.Direction {
			case "request":
				var request *protocol.Request
				request, decodeErr = protocol.DecodeRequest([]byte(vector.Input), options)
				if decodeErr == nil {
					negotiated = request.NegotiatedExtensions()
				}
			case "response":
				var response *protocol.Response
				response, decodeErr = protocol.DecodeResponse([]byte(vector.Input), options)
				if decodeErr == nil {
					negotiated = response.NegotiatedExtensions()
				}
			default:
				t.Fatalf("unknown direction %q", vector.Direction)
			}
			if vector.ErrorCode != "" {
				var diagnostic *protocol.Diagnostic
				if !errors.As(decodeErr, &diagnostic) || diagnostic.Code != vector.ErrorCode {
					t.Fatalf("decode error = %v, want %s", decodeErr, vector.ErrorCode)
				}
				return
			}
			if decodeErr != nil {
				t.Fatalf("decode: %v", decodeErr)
			}
			actual := make([]string, len(negotiated))
			for index, extension := range negotiated {
				actual[index] = extension.ID + "@" + extension.Version
			}
			if !slices.Equal(actual, vector.Negotiated) {
				t.Fatalf("negotiated = %v, want %v", actual, vector.Negotiated)
			}
		})
	}

	left, err := protocol.DecodeDocument([]byte(fixture.PersistedIdentity.Left), protocol.Limits{})
	if err != nil {
		t.Fatalf("left persisted document: %v", err)
	}
	right, err := protocol.DecodeDocument([]byte(fixture.PersistedIdentity.Right), protocol.Limits{})
	if err != nil {
		t.Fatalf("right persisted document: %v", err)
	}
	leftHash, err := protocol.SemanticHash(protocol.DocumentHash, left.CanonicalJSON())
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := protocol.SemanticHash(protocol.DocumentHash, right.CanonicalJSON())
	if err != nil {
		t.Fatal(err)
	}
	if (leftHash == rightHash) != fixture.PersistedIdentity.Equal {
		t.Fatalf("persisted extension identity equality = %t, want %t", leftHash == rightHash, fixture.PersistedIdentity.Equal)
	}
}
