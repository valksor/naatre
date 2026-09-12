package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

type securityFixture struct {
	Profile      string                `json:"profile"`
	DecisionTime string                `json:"decisionTime"`
	SafeDenial   securityExpectedError `json:"safeDenial"`
	Execution    []securityExecution   `json:"executionVectors"`
	Operations   []securityOperation   `json:"operationVectors"`
	Lifecycle    []securityLifecycle   `json:"lifecycleVectors"`
	Equivalence  []securityEquivalence `json:"equivalenceVectors"`
	Replay       []securityReplay      `json:"replayVectors"`
}

type securityExecution struct {
	Name                  string                      `json:"name"`
	Principal             runtime.Principal           `json:"principal"`
	Document              json.RawMessage             `json:"document"`
	RuntimeDecisions      map[string]securityDecision `json:"runtimeDecisions"`
	PlanningDecisions     map[string]bool             `json:"planningDecisions"`
	ExpectedData          map[string]any              `json:"expectedData"`
	ExpectedErrors        []securityExpectedError     `json:"expectedErrors"`
	ExpectedPrepareCode   string                      `json:"expectedPrepareCode"`
	ExpectedHandlerStarts int64                       `json:"expectedHandlerStarts"`
}

type securityDecision struct {
	Allowed               bool   `json:"allowed"`
	AuthorizationRevision string `json:"authorizationRevision"`
	ExpiresAt             string `json:"expiresAt"`
	CacheScope            string `json:"cacheScope"`
}

type securityExpectedError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
	Path    []any  `json:"path"`
}

type securityOperation struct {
	Name             string                 `json:"name"`
	Operation        protocol.OperationKind `json:"operation"`
	ExpectedDecision string                 `json:"expectedDecision"`
}

type securityLifecycle struct {
	Name                  string            `json:"name"`
	Principal             runtime.Principal `json:"principal"`
	Decision              securityDecision  `json:"decision"`
	ExpectedAllowed       bool              `json:"expectedAllowed"`
	ExpectedHandlerStarts int64             `json:"expectedHandlerStarts"`
}

type securityBinding struct {
	Subject               string                 `json:"subject"`
	Tenant                string                 `json:"tenant"`
	SchemaRevision        string                 `json:"schemaRevision"`
	AuthorizationRevision string                 `json:"authorizationRevision"`
	Resource              string                 `json:"resource"`
	Operation             protocol.OperationKind `json:"operation"`
}

type securityEquivalence struct {
	Name           string                  `json:"name"`
	CurrentBinding securityBinding         `json:"currentBinding"`
	Decision       securityDecision        `json:"decision"`
	Executions     []securityExecutionMode `json:"executions"`
}

type securityExecutionMode struct {
	Mode             string          `json:"mode"`
	CandidateBinding securityBinding `json:"candidateBinding"`
	ExpectedAllowed  bool            `json:"expectedAllowed"`
}

type securityReplay struct {
	Name                     string                    `json:"name"`
	CurrentBinding           securityBinding           `json:"currentBinding"`
	Candidates               []securityReplayCandidate `json:"candidates"`
	ExpectedVisible          []string                  `json:"expectedVisible"`
	ExpectedRejected         []string                  `json:"expectedRejected"`
	SafeLossOutcome          map[string]any            `json:"safeLossOutcome"`
	ForbiddenLossObservables []string                  `json:"forbiddenLossObservables"`
}

type securityReplayCandidate struct {
	ID        string          `json:"id"`
	Permitted bool            `json:"permitted"`
	Binding   securityBinding `json:"binding"`
}

func TestAuthorizationConsumesPortableSecurityFixture(t *testing.T) {
	t.Parallel()
	fixture, now := loadSecurityFixture(t)
	if fixture.SafeDenial.Code != runtime.CodeUnauthorized || fixture.SafeDenial.Message != "access denied" || fixture.SafeDenial.Details != nil {
		t.Fatalf("unsafe denial contract: %#v", fixture.SafeDenial)
	}
	for _, vector := range fixture.Execution {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			executeSecurityVector(t, vector, now, fixture.SafeDenial)
		})
	}
	for _, vector := range fixture.Lifecycle {
		vector := vector
		t.Run("lifecycle/"+vector.Name, func(t *testing.T) {
			t.Parallel()
			executeSecurityLifecycle(t, vector, now)
		})
	}
	assertSecurityOperationVectors(t, fixture.Operations, now)
	assertSecurityEquivalenceVectors(t, fixture.Equivalence, now)
	assertSecurityReplayVectors(t, fixture.Replay)
}

func loadSecurityFixture(t testing.TB) (securityFixture, time.Time) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "security.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture securityFixture
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode security fixture: %v", err)
	}
	now, err := time.Parse(time.RFC3339, fixture.DecisionTime)
	if err != nil || fixture.Profile != "core.security-1" || len(fixture.Execution) < 3 || len(fixture.Lifecycle) == 0 || len(fixture.Equivalence) == 0 || len(fixture.Replay) == 0 {
		t.Fatalf("incomplete security fixture: profile=%q now=%q err=%v", fixture.Profile, fixture.DecisionTime, err)
	}
	return fixture, now
}

func executeSecurityVector(t *testing.T, vector securityExecution, now time.Time, safeDenial securityExpectedError) {
	t.Helper()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	config := runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault, Now: func() time.Time { return now }}
	if len(vector.PlanningDecisions) != 0 {
		config.PlanningAuthorizer = runtime.PlanningAuthorizerFunc(func(request runtime.PlanningAuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{Allowed: vector.PlanningDecisions[securityPath(request.Path)]}, nil
		})
	} else {
		config.Authorizer = runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			decision, exists := vector.RuntimeDecisions[securityPath(request.Path)]
			if !exists {
				return runtime.AuthorizationDecision{}, nil
			}
			return decision.runtime(t), nil
		})
	}
	if err := registry.ConfigureAuthorization(config); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	plan, prepareErr := runtime.Prepare(snapshot, decodeSecurityDocument(t, vector.Document))
	if vector.ExpectedPrepareCode != "" {
		var validation *runtime.ValidationErrors
		if !errors.As(prepareErr, &validation) || !slices.ContainsFunc(validation.Issues(), func(issue runtime.ValidationIssue) bool {
			return issue.Diagnostic.Code == vector.ExpectedPrepareCode
		}) || calls.Load() != vector.ExpectedHandlerStarts {
			t.Fatalf("prepare=%v calls=%d", prepareErr, calls.Load())
		}
		return
	}
	if prepareErr != nil {
		t.Fatalf("Prepare: %v", prepareErr)
	}
	outcome := plan.Execute(runtime.WithPrincipal(context.Background(), vector.Principal))
	if !reflect.DeepEqual(outcome.Data, vector.ExpectedData) || calls.Load() != vector.ExpectedHandlerStarts || len(outcome.Errors) != len(vector.ExpectedErrors) {
		t.Fatalf("outcome=%#v calls=%d", outcome, calls.Load())
	}
	for index, expected := range vector.ExpectedErrors {
		actual := outcome.Errors[index]
		if actual.Code != expected.Code || !reflect.DeepEqual(actual.Path, expected.Path) || actual.Message != safeDenial.Message || actual.Details != nil {
			t.Fatalf("error %d = %#v, want %#v", index, actual, expected)
		}
	}
}

func executeSecurityLifecycle(t *testing.T, vector securityLifecycle, now time.Time) {
	t.Helper()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	decision := vector.Decision.runtime(t)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Now:  func() time.Time { return now },
		Authorizer: runtime.AuthorizerFunc(func(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return decision, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	outcome := plan.Execute(runtime.WithPrincipal(context.Background(), vector.Principal))
	allowed := len(outcome.Errors) == 0
	if allowed != vector.ExpectedAllowed || calls.Load() != vector.ExpectedHandlerStarts {
		t.Fatalf("allowed=%v calls=%d outcome=%#v", allowed, calls.Load(), outcome)
	}
}

func assertSecurityOperationVectors(t *testing.T, vectors []securityOperation, now time.Time) {
	t.Helper()
	if len(vectors) != 3 {
		t.Fatalf("operation vectors = %d", len(vectors))
	}
	seen := make(map[protocol.OperationKind]bool)
	for _, vector := range vectors {
		seen[vector.Operation] = true
		decision := "deny"
		if vector.Operation == protocol.Query {
			decision = "allow"
		}
		if vector.Name == "" || decision != vector.ExpectedDecision {
			t.Fatalf("operation vector = %#v", vector)
		}
	}
	for _, kind := range []protocol.OperationKind{protocol.Query, protocol.Mutation, protocol.Subscription} {
		if !seen[kind] {
			t.Fatalf("missing operation vector %s at %s", kind, now)
		}
	}
}

func assertSecurityEquivalenceVectors(t *testing.T, vectors []securityEquivalence, now time.Time) {
	t.Helper()
	wantModes := []string{"batched", "cached", "planned", "remote", "streamed"}
	for _, vector := range vectors {
		var modes []string
		for _, execution := range vector.Executions {
			modes = append(modes, execution.Mode)
			allowed := execution.CandidateBinding == vector.CurrentBinding && portableSecurityDecisionAllows(vector.Decision, vector.CurrentBinding, now)
			if allowed != execution.ExpectedAllowed {
				t.Fatalf("equivalence %s/%s allowed=%v, want %v", vector.Name, execution.Mode, allowed, execution.ExpectedAllowed)
			}
		}
		slices.Sort(modes)
		if !slices.Equal(modes, wantModes) {
			t.Fatalf("equivalence %s modes=%v", vector.Name, modes)
		}
	}
}

func assertSecurityReplayVectors(t *testing.T, vectors []securityReplay) {
	t.Helper()
	for _, vector := range vectors {
		var visible, rejected []string
		for _, candidate := range vector.Candidates {
			if candidate.Permitted && candidate.Binding == vector.CurrentBinding {
				visible = append(visible, candidate.ID)
			} else {
				rejected = append(rejected, candidate.ID)
			}
		}
		if !slices.Equal(visible, vector.ExpectedVisible) || !slices.Equal(rejected, vector.ExpectedRejected) {
			t.Fatalf("replay %s visible=%v rejected=%v", vector.Name, visible, rejected)
		}
		encoded, err := json.Marshal(vector.SafeLossOutcome)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range vector.ForbiddenLossObservables {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("replay %s safe loss leaks %q: %s", vector.Name, forbidden, encoded)
			}
		}
	}
}

func (decision securityDecision) runtime(t testing.TB) runtime.AuthorizationDecision {
	t.Helper()
	var expiresAt time.Time
	if decision.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, decision.ExpiresAt)
		if err != nil {
			t.Fatalf("invalid security decision expiry %q: %v", decision.ExpiresAt, err)
		}
		expiresAt = parsed
	}
	return runtime.AuthorizationDecision{
		Allowed: decision.Allowed, AuthorizationRevision: decision.AuthorizationRevision,
		ExpiresAt: expiresAt, CacheScope: runtime.AuthorizationCacheScope(decision.CacheScope),
	}
}

func portableSecurityDecisionAllows(decision securityDecision, binding securityBinding, now time.Time) bool {
	if !decision.Allowed || (decision.AuthorizationRevision != "" && decision.AuthorizationRevision != binding.AuthorizationRevision) {
		return false
	}
	if decision.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, decision.ExpiresAt)
		if err != nil || !now.Before(expiresAt) {
			return false
		}
	}
	switch decision.CacheScope {
	case "", string(runtime.AuthorizationCacheNoStore):
		return true
	case string(runtime.AuthorizationCachePrincipal):
		return binding.Subject != ""
	case string(runtime.AuthorizationCacheTenant):
		return binding.Tenant != ""
	default:
		return false
	}
}

func decodeSecurityDocument(t testing.TB, document json.RawMessage) *protocol.Request {
	t.Helper()
	envelope, err := json.Marshal(struct {
		Version  string          `json:"version"`
		Document json.RawMessage `json:"document"`
	}{Version: "1", Document: document})
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.DecodeRequest(envelope, protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return request
}
