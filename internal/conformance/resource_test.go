package conformance_test

import (
	"slices"
	"testing"
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
