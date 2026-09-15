package conformance_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type resourceFixture struct {
	Profile          string                  `json:"profile"`
	LimitVectors     []resourceLimitVector   `json:"limitVectors"`
	LateExhaustion   []lateExhaustionVector  `json:"lateExhaustionVectors"`
	ReplayBudgets    []replayBudgetVector    `json:"replayBudgetVectors"`
	AggregateBudgets []aggregateBudgetVector `json:"aggregateBudgetVectors"`
}

type resourceLimitVector struct {
	Name     string `json:"name"`
	Code     string `json:"code"`
	Phase    string `json:"phase"`
	Location string `json:"location"`
}

type lateExhaustionVector struct {
	Name                       string `json:"name"`
	Operation                  string `json:"operation"`
	ModeledCommit              bool   `json:"modeledCommit"`
	Exhaustion                 string `json:"exhaustion"`
	Code                       string `json:"code"`
	Path                       []any  `json:"path"`
	EffectState                string `json:"effectState"`
	ErrorEnvelopeBytes         uint64 `json:"errorEnvelopeBytes"`
	AuditOrIdempotencyRequired bool   `json:"auditOrIdempotencyRequired"`
}

type replayLimits struct {
	MaxCursorScan               uint64 `json:"maxCursorScan"`
	MaxStorageReads             uint64 `json:"maxStorageReads"`
	MaxEvents                   uint64 `json:"maxEvents"`
	MaxBytes                    uint64 `json:"maxBytes"`
	MaxFilterEvaluations        uint64 `json:"maxFilterEvaluations"`
	MaxAuthorizationEvaluations uint64 `json:"maxAuthorizationEvaluations"`
	MaxCatchUpQueue             uint64 `json:"maxCatchUpQueue"`
	MaxMemoryBytes              uint64 `json:"maxMemoryBytes"`
	MaxDurationMilliseconds     uint64 `json:"maxDurationMilliseconds"`
}

type replayWork struct {
	CursorScan               uint64 `json:"cursorScan"`
	StorageReads             uint64 `json:"storageReads"`
	Events                   uint64 `json:"events"`
	Bytes                    uint64 `json:"bytes"`
	FilterEvaluations        uint64 `json:"filterEvaluations"`
	AuthorizationEvaluations uint64 `json:"authorizationEvaluations"`
	CatchUpQueue             uint64 `json:"catchUpQueue"`
	MemoryBytes              uint64 `json:"memoryBytes"`
	DurationMilliseconds     uint64 `json:"durationMilliseconds"`
}

type replayBudgetVector struct {
	Name                      string       `json:"name"`
	CursorClass               string       `json:"cursorClass"`
	EarliestHistoryAuthorized bool         `json:"earliestHistoryAuthorized"`
	Limits                    replayLimits `json:"limits"`
	Attempted                 replayWork   `json:"attempted"`
	Code                      string       `json:"code"`
	MaxObservedStorageReads   uint64       `json:"maxObservedStorageReads"`
	AdmitMoreWork             bool         `json:"admitMoreWork"`
}

type aggregateBudgetVector struct {
	Name       string       `json:"name"`
	Mode       string       `json:"mode"`
	Limits     replayLimits `json:"limits"`
	Attempted  replayWork   `json:"attempted"`
	Code       string       `json:"code"`
	Disconnect bool         `json:"disconnect"`
}

func TestPortableResourceBudgetFixtures(t *testing.T) {
	t.Parallel()
	var fixture resourceFixture
	readFixture(t, "resources.json", &fixture)
	if fixture.Profile != "core.resources-1" {
		t.Fatalf("resource profile = %q", fixture.Profile)
	}
	assertResourceLimitVectors(t, fixture.LimitVectors)
	assertLateExhaustionVectors(t, fixture.LateExhaustion)
	assertReplayBudgetVectors(t, fixture.ReplayBudgets)
	assertAggregateBudgetVectors(t, fixture.AggregateBudgets)
}

func assertResourceLimitVectors(t *testing.T, vectors []resourceLimitVector) {
	t.Helper()
	expected := []string{
		"request-bytes", "decoder-tokens", "ast-depth", "decoded-string-bytes",
		"object-members", "array-items", "number-bytes", "literal-bytes",
		"plan-depth", "plan-nodes", "planned-calls", "selected-fields",
		"fragment-expansion", "parallel-width", "queued-work", "static-cost",
		"validation-errors",
		"runtime-work", "collection-items", "execution-duration", "output-bytes",
		"error-count", "output-depth", "output-nodes",
	}
	names := make([]string, 0, len(vectors))
	for _, vector := range vectors {
		if vector.Name == "" || vector.Code == "" || vector.Phase == "" || vector.Location == "" {
			t.Fatalf("incomplete resource limit vector: %#v", vector)
		}
		names = append(names, vector.Name)
	}
	if !slices.Equal(names, expected) {
		t.Fatalf("resource limit names = %v, want %v", names, expected)
	}
}

func assertLateExhaustionVectors(t *testing.T, vectors []lateExhaustionVector) {
	t.Helper()
	if len(vectors) != 2 {
		t.Fatalf("late exhaustion vectors = %d, want 2", len(vectors))
	}
	for _, vector := range vectors {
		if vector.Operation != "mutation" || !vector.ModeledCommit || vector.Code != "RESOURCE_EXHAUSTED" ||
			len(vector.Path) == 0 || vector.EffectState != "applied" || vector.ErrorEnvelopeBytes < 320 ||
			!vector.AuditOrIdempotencyRequired {
			t.Fatalf("late exhaustion vector is not truthful and bounded: %#v", vector)
		}
	}
}

func assertReplayBudgetVectors(t *testing.T, vectors []replayBudgetVector) {
	t.Helper()
	classes := make([]string, 0, len(vectors))
	for _, vector := range vectors {
		classes = append(classes, vector.CursorClass)
		if vector.Name == "" || vector.Code != "RESOURCE_EXHAUSTED" || vector.AdmitMoreWork ||
			vector.Limits.MaxCursorScan == 0 || vector.Limits.MaxStorageReads == 0 ||
			vector.MaxObservedStorageReads > vector.Limits.MaxStorageReads ||
			vector.Attempted.CursorScan <= vector.Limits.MaxCursorScan {
			t.Fatalf("replay budget vector is not bounded: %#v", vector)
		}
	}
	if !slices.Equal(classes, []string{"ancient", "random", "missing", "earliest-retained"}) ||
		vectors[len(vectors)-1].EarliestHistoryAuthorized {
		t.Fatalf("cursor classes or earliest-history policy = %#v", vectors)
	}
}

func assertAggregateBudgetVectors(t *testing.T, vectors []aggregateBudgetVector) {
	t.Helper()
	if len(vectors) != 2 {
		t.Fatalf("aggregate budget vectors = %d, want replay and live", len(vectors))
	}
	for _, vector := range vectors {
		if vector.Name == "" || (vector.Mode != "replay" && vector.Mode != "live") ||
			vector.Code != "RESOURCE_EXHAUSTED" || !vector.Disconnect ||
			vector.Limits.MaxEvents == 0 || vector.Limits.MaxBytes == 0 ||
			vector.Limits.MaxMemoryBytes == 0 || vector.Limits.MaxDurationMilliseconds == 0 {
			t.Fatalf("aggregate budget vector is incomplete: %#v", vector)
		}
	}
}

// TestPortableResourceLimitVectorsAgainstRuntime decodes each resource limit
// vector whose code maps to a concrete runtime scenario and drives it through
// the real runtime: static/validation limits via PrepareWithOptions, and
// execute/serialize budgets via Plan.Execute. It asserts that the runtime's
// actual failure code equals the fixture's declared code for that vector.
func TestPortableResourceLimitVectorsAgainstRuntime(t *testing.T) {
	t.Parallel()
	var fixture resourceFixture
	readFixture(t, "resources.json", &fixture)

	// Validation-phase limits: set exactly one limit low enough to trip it,
	// then assert the fixture's declared code appears among the validation
	// diagnostics the real runtime produced during Prepare.
	validationDrivers := map[string]func(*testing.T) []string{
		"plan-depth": func(t *testing.T) []string {
			return resourcePrepareCodes(t, resourceParallelQuery(), 1, runtime.ResourceLimits{MaxPlanDepth: 1})
		},
		"plan-nodes": func(t *testing.T) []string {
			return resourcePrepareCodes(t, resourceTwoCallQuery(), 1, runtime.ResourceLimits{MaxPlanNodes: 1})
		},
		"planned-calls": func(t *testing.T) []string {
			return resourcePrepareCodes(t, resourceTwoCallQuery(), 1, runtime.ResourceLimits{MaxPlannedCalls: 1})
		},
		"parallel-width": func(t *testing.T) []string {
			return resourcePrepareCodes(t, resourceParallelQuery(), 1, runtime.ResourceLimits{MaxParallelWidth: 1})
		},
		"queued-work": func(t *testing.T) []string {
			return resourcePrepareCodes(t, resourceParallelQuery(), 1, runtime.ResourceLimits{MaxQueuedWork: 1})
		},
		"static-cost": func(t *testing.T) []string {
			return resourcePrepareCodes(t, resourceSingleCallQuery(), 6, runtime.ResourceLimits{MaxStaticCost: 5})
		},
		"validation-errors": func(t *testing.T) []string {
			return resourcePrepareCodes(t, resourceUnknownCallsQuery(), 1, runtime.ResourceLimits{MaxValidationErrors: 2})
		},
	}
	// Execute/serialize-phase budgets: run the plan and assert the fixture's
	// declared code (RESOURCE_EXHAUSTED) appears among the execution errors.
	executionDrivers := map[string]func(*testing.T) []string{
		"execution-duration": resourceDriveExecutionDuration,
		"output-bytes":       resourceDriveOutputBytes,
		"error-count":        resourceDriveErrorCount,
	}

	executed := 0
	for _, vector := range fixture.LimitVectors {
		vector := vector
		if driver, ok := validationDrivers[vector.Name]; ok {
			executed++
			t.Run("validate/"+vector.Name, func(t *testing.T) {
				t.Parallel()
				if codes := driver(t); !slices.Contains(codes, vector.Code) {
					t.Fatalf("validation codes = %v, want %q (%s)", codes, vector.Code, vector.Name)
				}
			})
			continue
		}
		if driver, ok := executionDrivers[vector.Name]; ok {
			executed++
			t.Run("execute/"+vector.Name, func(t *testing.T) {
				t.Parallel()
				if codes := driver(t); !slices.Contains(codes, vector.Code) {
					t.Fatalf("execution codes = %v, want %q (%s)", codes, vector.Code, vector.Name)
				}
			})
		}
	}
	if executed < 10 {
		t.Fatalf("drove only %d resource limit vectors against the runtime, want at least 10", executed)
	}
}

func resourceDriveExecutionDuration(t *testing.T) []string {
	snapshot := resourceSnapshot(t, 0, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	plan, err := runtime.PrepareWithOptions(snapshot, decodeConformanceRequest(t, resourceSingleCallQuery()), runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxExecutionDuration: time.Millisecond}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	return resourceExecutionCodes(plan.Execute(context.Background()))
}

func resourceDriveOutputBytes(t *testing.T) []string {
	snapshot := resourceSnapshot(t, 0, func(context.Context) (string, error) { return string(make([]byte, 2048)), nil })
	plan, err := runtime.PrepareWithOptions(snapshot, decodeConformanceRequest(t, resourceSingleCallQuery()), runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxOutputBytes: 512, MaxErrors: 1}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	return resourceExecutionCodes(plan.Execute(context.Background()))
}

func resourceDriveErrorCount(t *testing.T) []string {
	snapshot := resourceSnapshot(t, 0, func(context.Context) (string, error) { return "", errors.New("expected failure") })
	plan, err := runtime.PrepareWithOptions(snapshot, decodeConformanceRequest(t, resourceThreeCallQuery()), runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxErrors: 2}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	return resourceExecutionCodes(plan.Execute(context.Background()))
}

func resourceExecutionCodes(outcome runtime.Outcome) []string {
	codes := make([]string, 0, len(outcome.Errors))
	for _, failure := range outcome.Errors {
		codes = append(codes, failure.Code)
	}
	return codes
}

// resourceSnapshot registers a single "read" query handler with the given
// static cost and body, configures allow-by-default authorization, and freezes.
func resourceSnapshot(t *testing.T, cost uint64, handler func(context.Context) (string, error)) runtime.Snapshot {
	t.Helper()
	registry := runtime.NewRegistry(conformanceCoreTypes(t))
	metadata := conformanceMetadata(runtime.ReadEffect)
	metadata.Cost = cost
	descriptor := runtime.Descriptor{
		Name: "read", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		return handler(ctx)
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return snapshot
}

// resourcePrepareCodes freezes a "read" query snapshot with the given static
// cost and runs PrepareWithOptions under the supplied limits, returning the
// validation diagnostic codes the runtime produced.
func resourcePrepareCodes(t *testing.T, request string, cost uint64, limits runtime.ResourceLimits) []string {
	t.Helper()
	snapshot := resourceSnapshot(t, cost, func(context.Context) (string, error) { return "ok", nil })
	_, err := runtime.PrepareWithOptions(snapshot, decodeConformanceRequest(t, request), runtime.PrepareOptions{Limits: limits})
	var validationErr *runtime.ValidationErrors
	if !errors.As(err, &validationErr) {
		t.Fatalf("PrepareWithOptions error = %T %v, want ValidationErrors", err, err)
	}
	issues := validationErr.Issues()
	codes := make([]string, len(issues))
	for index, issue := range issues {
		codes[index] = issue.Diagnostic.Code
	}
	return codes
}

func resourceSingleCallQuery() string {
	return `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"read"}}]}]}}`
}

func resourceTwoCallQuery() string {
	return `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"read","as":"a"}},{"$call":{"name":"read","as":"b"}}]}]}}`
}

func resourceThreeCallQuery() string {
	return `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"read","as":"a"}},{"$call":{"name":"read","as":"b"}},{"$call":{"name":"read","as":"c"}}]}]}}`
}

func resourceParallelQuery() string {
	return `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"read","as":"a"}},{"$call":{"name":"read","as":"b"}}]}}]}]}}`
}

func resourceUnknownCallsQuery() string {
	return `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"missingOne"}},{"$call":{"name":"missingTwo"}},{"$call":{"name":"missingThree"}}]}]}}`
}
