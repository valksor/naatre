package conformance_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type federationCoordinatorFixture struct {
	Profile          string `json:"profile"`
	NormativeProfile string `json:"normativeProfile"`
	Implementation   struct {
		Package          string   `json:"package"`
		Language         string   `json:"language"`
		MinimumRuntime   string   `json:"minimumRuntime"`
		ExecutedRuntime  string   `json:"executedRuntime"`
		ExecutedPlatform string   `json:"executedPlatform"`
		PortableTargets  []string `json:"portableTargets"`
		Lifecycle        string   `json:"lifecycle"`
	} `json:"implementation"`
	Dependencies []struct {
		Kind     string `json:"kind"`
		Issue    int    `json:"issue"`
		Revision string `json:"revision"`
		Profile  string `json:"profile"`
		Fixture  string `json:"fixture"`
		SHA256   string `json:"sha256"`
	} `json:"dependencies"`
	Commands          []string                        `json:"commands"`
	Supported         []string                        `json:"supported"`
	Unsupported       []string                        `json:"unsupported"`
	EntityRoutes      map[string][]schema.EntityFetch `json:"entityRoutes"`
	PlanningCases     []federationPlanningCase        `json:"planningCases"`
	ExecutionEvidence []struct {
		Name    string `json:"name"`
		Fixture string `json:"fixture"`
		Pointer string `json:"pointer"`
		Test    string `json:"test"`
	} `json:"executionEvidence"`
}

type federationPlanningCase struct {
	Name           string                         `json:"name"`
	Polarity       string                         `json:"polarity"`
	SchemaRevision string                         `json:"schemaRevision"`
	Limits         federationLimitsFixture        `json:"limits"`
	Fetches        []federationEntityFetchFixture `json:"fetches"`
	ExpectedCalls  []string                       `json:"expectedCalls"`
	ExpectedCode   string                         `json:"expectedCode"`
}

type federationEntityFetchFixture struct {
	ResponseKey  string          `json:"responseKey"`
	ServiceID    string          `json:"serviceId"`
	Type         schema.TypeID   `json:"type"`
	Input        json.RawMessage `json:"input"`
	Path         []any           `json:"path"`
	MaxAttempts  uint32          `json:"maxAttempts"`
	Dependencies []string        `json:"dependencies"`
}

func TestGoFederationCoordinatorProfile(t *testing.T) {
	t.Parallel()
	var fixture federationCoordinatorFixture
	readFixture(t, "federation-coordinator.json", &fixture)
	assertFederationCoordinatorHeader(t, fixture)
	assertFederationCoordinatorDependencies(t, fixture)
	assertFederationCoordinatorEvidence(t, fixture)

	var core federationFixture
	readFixture(t, "federation.json", &core)
	services := indexFederationServices(t, core.Services)
	for name, routes := range fixture.EntityRoutes {
		service, ok := services[name]
		if !ok {
			t.Fatalf("unknown entity-route service %q", name)
		}
		service.EntityFetches = slices.Clone(routes)
		services[name] = service
	}
	composition := composeFederationFixture(t, services, core.Trust, core.Composition)
	for _, test := range fixture.PlanningCases {
		test := test
		t.Run("planning/"+test.Name, func(t *testing.T) {
			runFederationPlanningCase(t, composition, test)
		})
	}
}

func assertFederationCoordinatorHeader(t *testing.T, fixture federationCoordinatorFixture) {
	t.Helper()
	if fixture.Profile != runtime.GoFederationCoordinatorProfile || fixture.NormativeProfile != "core.federation-1" ||
		fixture.Implementation.Package != "github.com/valksor/naatre/runtime" || fixture.Implementation.Language != "go" ||
		fixture.Implementation.MinimumRuntime != "go1.27.0" || fixture.Implementation.ExecutedRuntime == "" ||
		fixture.Implementation.ExecutedPlatform == "" || fixture.Implementation.Lifecycle == "" ||
		len(fixture.Implementation.PortableTargets) != 5 || len(fixture.Commands) != 3 || len(fixture.Supported) == 0 {
		t.Fatal("federation coordinator fixture header is incomplete")
	}
	wantUnsupported := []string{
		"service-discovery", "endpoint-reference-dereferencing", "routing-optimization", "built-in-http-transport",
		"built-in-grpc-transport", "sse-streaming", "websocket-streaming", "federated-mutations",
		"distributed-transactions", "w3c-tracestate", "w3c-baggage", "framework-integration",
		"cross-language-native-runtime-certification",
	}
	if !slices.Equal(fixture.Unsupported, wantUnsupported) {
		t.Fatalf("unsupported capabilities = %v, want %v", fixture.Unsupported, wantUnsupported)
	}
	polarities := make(map[string]bool)
	for _, test := range fixture.PlanningCases {
		polarities[test.Polarity] = true
	}
	for _, polarity := range []string{"positive", "negative", "boundary", "resource-limit"} {
		if !polarities[polarity] {
			t.Errorf("planning fixtures lack %s coverage", polarity)
		}
	}
}

func assertFederationCoordinatorDependencies(t *testing.T, fixture federationCoordinatorFixture) {
	t.Helper()
	if len(fixture.Dependencies) != 2 || fixture.Dependencies[0].Kind != "issue" || fixture.Dependencies[0].Issue != 24 ||
		fixture.Dependencies[0].Revision != "2a10f2f05d5929779add085b52bd769a359792f4" || fixture.Dependencies[0].Profile != "core.federation-1" ||
		fixture.Dependencies[1].Kind != "module" || fixture.Dependencies[1].Revision != "go1.27.0" {
		t.Fatal("federation coordinator dependency revisions are incomplete")
	}
	for _, dependency := range fixture.Dependencies {
		path := filepath.Join("../../conformance", filepath.FromSlash(dependency.Fixture))
		if dependency.Kind == "module" {
			path = filepath.Join("../..", filepath.FromSlash(dependency.Fixture))
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read dependency %s: %v", dependency.Fixture, err)
		}
		digest := sha256.Sum256(content)
		if actual := hex.EncodeToString(digest[:]); actual != dependency.SHA256 {
			t.Fatalf("dependency %s digest = %s, want %s", dependency.Fixture, actual, dependency.SHA256)
		}
	}
}

func assertFederationCoordinatorEvidence(t *testing.T, fixture federationCoordinatorFixture) {
	t.Helper()
	content, err := os.ReadFile("../../conformance/v1/federation.json")
	if err != nil {
		t.Fatal(err)
	}
	var core any
	if err := json.Unmarshal(content, &core); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(fixture.ExecutionEvidence))
	for _, evidence := range fixture.ExecutionEvidence {
		assertFederationExecutionEvidence(t, core, evidence, names)
	}
	assertRequiredFederationEvidence(t, names)
}

func assertFederationExecutionEvidence(t *testing.T, core any, evidence struct {
	Name    string `json:"name"`
	Fixture string `json:"fixture"`
	Pointer string `json:"pointer"`
	Test    string `json:"test"`
}, names map[string]bool) {
	t.Helper()
	if names[evidence.Name] {
		t.Fatalf("duplicate execution evidence %q", evidence.Name)
	}
	names[evidence.Name] = true
	if evidence.Fixture == "" {
		if evidence.Test == "" {
			t.Fatalf("execution evidence %q has no fixture or test", evidence.Name)
		}
		return
	}
	resolved, err := resolveJSONPointer(core, evidence.Pointer)
	entry, ok := resolved.(map[string]any)
	if err != nil || !ok || entry["name"] != evidence.Name {
		t.Fatalf("execution evidence %q does not resolve: %#v, %v", evidence.Name, resolved, err)
	}
}

func assertRequiredFederationEvidence(t *testing.T, names map[string]bool) {
	t.Helper()
	for _, name := range []string{
		"entity-fetch-cycle", "ownership-conflict", "type-conflict",
		"forged-delegation", "wrong-audience", "partial-failure-retains-sibling-data", "deadline-stops-scheduling",
		"remote-schema-drift", "rolling-upgrade-version-skew", "multiplicative-retry-cost-fanout", "parent-cancellation", "dependency-cancellation",
	} {
		if !names[name] {
			t.Errorf("federation coordinator evidence lacks %q", name)
		}
	}
}

func runFederationPlanningCase(t *testing.T, composition schema.FederationComposition, test federationPlanningCase) {
	t.Helper()
	planner, err := runtime.NewFederationPlanner(runtime.FederationPlannerConfig{
		Composition: composition,
		Limits: runtime.FederationLimits{
			MaxCalls: test.Limits.MaxCalls, MaxCost: test.Limits.MaxCost,
			MaxConcurrency: test.Limits.MaxConcurrency, MaxAttempts: test.Limits.MaxAttempts,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fetches := make([]runtime.FederationEntityFetch, len(test.Fetches))
	for index, fetch := range test.Fetches {
		var input any
		decoder := json.NewDecoder(bytes.NewReader(fetch.Input))
		decoder.UseNumber()
		if err := decoder.Decode(&input); err != nil {
			t.Fatalf("decode planning input: %v", err)
		}
		fetches[index] = runtime.FederationEntityFetch{
			ResponseKey: fetch.ResponseKey, ServiceID: fetch.ServiceID, Type: fetch.Type, Input: input,
			Path: fetch.Path, MaxAttempts: fetch.MaxAttempts, Dependencies: fetch.Dependencies,
		}
	}
	plan, err := planner.PlanEntityFetches(test.SchemaRevision, fetches)
	if test.ExpectedCode != "" {
		var failure *runtime.ExecutionError
		if !errors.As(err, &failure) || failure.Code != test.ExpectedCode {
			t.Fatalf("planning failure = %#v (%v), want %s", failure, err, test.ExpectedCode)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]string, len(plan.Calls))
	for index, call := range plan.Calls {
		actual[index] = call.ResponseKey
	}
	if !reflect.DeepEqual(actual, test.ExpectedCalls) {
		t.Fatalf("planned calls = %v, want %v", actual, test.ExpectedCalls)
	}
}
