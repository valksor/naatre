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

// Every portable negative vector is executed against the planner, not merely
// shape-checked. A fixture that declares a diagnostic nobody runs is
// decoration: it cannot detect drift between the language contract and the
// implementation.
func TestPlannerConsumesPortableLanguageDiagnosticFixture(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "language.json"))
	if err != nil {
		t.Fatalf("read language fixture: %v", err)
	}
	var fixture struct {
		Vectors []struct {
			Name         string          `json:"name"`
			Document     json.RawMessage `json:"document"`
			Valid        *bool           `json:"valid"`
			Capabilities []string        `json:"capabilities"`
			Code         string          `json:"code"`
			Phase        string          `json:"phase"`
			Pointer      string          `json:"pointer"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode language fixture: %v", err)
	}
	executed := 0
	for _, vector := range fixture.Vectors {
		if vector.Valid == nil || *vector.Valid {
			continue
		}
		executed++
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			assertPortableNegativeVector(t, vector.Name, vector.Code, vector.Phase, vector.Pointer, vector.Document, vector.Capabilities)
		})
	}
	if executed == 0 {
		t.Fatal("language fixture declares no negative vectors")
	}
}

// assertPortableNegativeVector requires the document to be rejected with the
// diagnostic the vector declares, at its declared location, before any handler
// runs. Rejection may happen while decoding or while planning; the vector
// declares which phase owns it.
func assertPortableNegativeVector(t *testing.T, name, code, phase, pointer string, document json.RawMessage, capabilities []string) {
	t.Helper()
	if code == "" || phase == "" || pointer == "" {
		t.Fatalf("negative vector %q declares no diagnostic", name)
	}
	located := "/document" + pointer
	envelopeObject := map[string]any{"version": "1", "document": json.RawMessage(document)}
	supported := make(map[string]bool, len(capabilities))
	if len(capabilities) != 0 {
		envelopeObject["capabilities"] = capabilities
		for _, capability := range capabilities {
			supported[capability] = true
		}
	}
	envelope, marshalErr := json.Marshal(envelopeObject)
	if marshalErr != nil {
		t.Fatalf("encode negative vector %q: %v", name, marshalErr)
	}
	request, decodeErr := protocol.DecodeRequest(envelope, protocol.DecodeOptions{Capabilities: supported})
	if decodeErr != nil {
		assertPortableDecodeDiagnostic(t, code, phase, located, decodeErr)
		return
	}
	snapshot, calls := validationRegistry(t)
	_, prepareErr := runtime.Prepare(snapshot, request)
	var validationErr *runtime.ValidationErrors
	if !errors.As(prepareErr, &validationErr) {
		t.Fatalf("Prepare error = %T %v, want ValidationErrors declaring %s", prepareErr, prepareErr, code)
	}
	assertPortableIssue(t, code, phase, located, validationErr.Issues())
	// A rejected document must never have reached a handler.
	if calls.Load() != 0 {
		t.Fatalf("invalid vector invoked %d handlers", calls.Load())
	}
}

func assertPortableDecodeDiagnostic(t *testing.T, code, phase, pointer string, err error) {
	t.Helper()
	var diagnostic *protocol.Diagnostic
	if !errors.As(err, &diagnostic) {
		t.Fatalf("decode error = %T %v, want Diagnostic declaring %s", err, err, code)
	}
	if diagnostic.Code != code || diagnostic.Phase != phase || diagnostic.Pointer != pointer {
		t.Fatalf("decode diagnostic = %s %s %s, want %s %s %s",
			diagnostic.Code, diagnostic.Phase, diagnostic.Pointer, code, phase, pointer)
	}
}

// assertPortableIssue requires the declared diagnostic to be present at its
// declared location. Other issues may accompany it, since one malformed
// document can violate several clauses, but the declared one must be exact.
func assertPortableIssue(t *testing.T, code, phase, pointer string, issues []runtime.ValidationIssue) {
	t.Helper()
	for _, issue := range issues {
		if issue.Diagnostic.Code == code && issue.Diagnostic.Phase == phase && issue.Diagnostic.Pointer == pointer {
			return
		}
	}
	reported := make([]string, 0, len(issues))
	for _, issue := range issues {
		reported = append(reported, issue.Diagnostic.Code+" "+issue.Diagnostic.Pointer)
	}
	t.Fatalf("issues %v do not contain %s %s at %s", reported, code, phase, pointer)
}
