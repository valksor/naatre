package runtime_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

func TestPlanReuseConformanceFixture(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "planning.json"))
	if err != nil {
		t.Fatalf("read planning fixture: %v", err)
	}
	var fixture struct {
		Profile string `json:"profile"`
		Kind    string `json:"kind"`
		Vectors []struct {
			Name                  string            `json:"name"`
			Requests              []json.RawMessage `json:"requests"`
			ExpectedPlan          json.RawMessage   `json:"expectedPlan"`
			ExcludedRequestState  []string          `json:"excludedRequestState"`
			ExcludedRequestValues []string          `json:"excludedRequestValues"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode planning fixture: %v", err)
	}
	if fixture.Profile != "core.language-1" || fixture.Kind != "plan-reuse" || len(fixture.Vectors) == 0 {
		t.Fatalf("invalid planning fixture header: %#v", fixture)
	}
	snapshot, _ := validationRegistry(t)
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || len(vector.Requests) < 2 || len(vector.ExpectedPlan) == 0 || len(vector.ExcludedRequestValues) == 0 {
			t.Fatalf("incomplete planning vector: %#v", vector)
		}
		for _, state := range []string{"id", "variables", "extensions", "policyInput", "authorizationIdentity"} {
			if !slices.Contains(vector.ExcludedRequestState, state) {
				t.Fatalf("planning vector %s does not exclude %s", vector.Name, state)
			}
		}
		for _, raw := range vector.Requests {
			request, decodeErr := protocol.DecodeRequest(raw, protocol.DecodeOptions{ExtensionNamespaces: map[string]bool{"com.example.plan-context": true}})
			if decodeErr != nil {
				t.Fatalf("decode %s: %v", vector.Name, decodeErr)
			}
			plan, prepareErr := runtime.Prepare(snapshot, request)
			if prepareErr != nil {
				t.Fatalf("prepare %s: %v", vector.Name, prepareErr)
			}
			actual, marshalErr := json.Marshal(plan.Description())
			actualCanonical, actualErr := protocol.CanonicalizeJSON(actual, protocol.Limits{})
			expectedCanonical, expectedErr := protocol.CanonicalizeJSON(vector.ExpectedPlan, protocol.Limits{})
			if marshalErr != nil || actualErr != nil || expectedErr != nil || !bytes.Equal(actualCanonical, expectedCanonical) {
				t.Fatalf("plan %s = %s, want %s (%v)", vector.Name, actual, vector.ExpectedPlan, marshalErr)
			}
			for _, excluded := range vector.ExcludedRequestValues {
				if bytes.Contains(actual, []byte(excluded)) {
					t.Fatalf("plan %s retained excluded request value %q: %s", vector.Name, excluded, actual)
				}
			}
		}
	}
}

func TestPlannerConsumesPortableLanguageDiagnosticFixture(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "language.json"))
	if err != nil {
		t.Fatalf("read language fixture: %v", err)
	}
	var fixture struct {
		Vectors []struct {
			Name     string          `json:"name"`
			Document json.RawMessage `json:"document"`
			Valid    bool            `json:"valid"`
			Code     string          `json:"code"`
			Phase    string          `json:"phase"`
			Pointer  string          `json:"pointer"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode language fixture: %v", err)
	}
	var vector *struct {
		Name     string          `json:"name"`
		Document json.RawMessage `json:"document"`
		Valid    bool            `json:"valid"`
		Code     string          `json:"code"`
		Phase    string          `json:"phase"`
		Pointer  string          `json:"pointer"`
	}
	for index := range fixture.Vectors {
		if fixture.Vectors[index].Name == "implicit-list-item-mapping" {
			vector = &fixture.Vectors[index]
			break
		}
	}
	if vector == nil || vector.Valid || vector.Code == "" || vector.Phase == "" || vector.Pointer == "" {
		t.Fatalf("missing portable diagnostic vector: %#v", vector)
	}
	envelope := append([]byte(`{"version":"1","document":`), vector.Document...)
	envelope = append(envelope, '}')
	request, err := protocol.DecodeRequest(envelope, protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("decode portable vector: %v", err)
	}
	snapshot, calls := validationRegistry(t)
	_, err = runtime.Prepare(snapshot, request)
	var validationErr *runtime.ValidationErrors
	if !errors.As(err, &validationErr) {
		t.Fatalf("Prepare error = %T %v, want ValidationErrors", err, err)
	}
	issues := validationErr.Issues()
	if len(issues) != 1 || issues[0].Diagnostic.Code != vector.Code || issues[0].Diagnostic.Phase != vector.Phase ||
		issues[0].Diagnostic.Pointer != "/document"+vector.Pointer {
		t.Fatalf("portable diagnostic = %#v, want %s %s /document%s", issues, vector.Code, vector.Phase, vector.Pointer)
	}
	if calls.Load() != 0 {
		t.Fatalf("portable invalid vector invoked %d handlers", calls.Load())
	}
}
