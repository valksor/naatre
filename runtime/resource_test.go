package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestPrepareResourceLimitsRejectStaticWorkBeforeHandlers(t *testing.T) {
	t.Parallel()
	snapshot, calls := staticResourceSnapshot(t, 5)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"user","as":"a","select":[{"$field":{"name":"name"}}]}},{"$call":{"name":"user","as":"b","select":[{"$field":{"name":"name"}}]}}]}]}}`)
	tests := []struct {
		name string
		set  func(*runtime.ResourceLimits)
		code string
	}{
		{"plan depth", func(v *runtime.ResourceLimits) { v.MaxPlanDepth = 1 }, "LIMIT_PLAN_DEPTH"},
		{"plan nodes", func(v *runtime.ResourceLimits) { v.MaxPlanNodes = 3 }, "LIMIT_PLAN_NODES"},
		{"planned calls", func(v *runtime.ResourceLimits) { v.MaxPlannedCalls = 1 }, "LIMIT_PLANNED_CALLS"},
		{"selected fields", func(v *runtime.ResourceLimits) { v.MaxSelectedFields = 1 }, "LIMIT_SELECTED_FIELDS"},
		{"static cost", func(v *runtime.ResourceLimits) { v.MaxStaticCost = 11 }, "LIMIT_STATIC_COST"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := runtime.ResourceLimits{}
			test.set(&limits)
			_, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: limits})
			assertValidationLimit(t, err, test.code)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("static rejection invoked %d handlers", calls.Load())
	}
}

func TestPrepareResourceLimitsRejectParallelQueueAndCostOverflow(t *testing.T) {
	t.Parallel()
	snapshot, _ := staticResourceSnapshot(t, math.MaxUint64)
	parallel := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"user","as":"a","select":[{"$field":{"name":"name"}}]}},{"$call":{"name":"user","as":"b","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	for _, test := range []struct {
		limits runtime.ResourceLimits
		code   string
	}{
		{runtime.ResourceLimits{MaxParallelWidth: 1}, "LIMIT_PARALLEL_WIDTH"},
		{runtime.ResourceLimits{MaxQueuedWork: 1}, "LIMIT_QUEUED_WORK"},
		{runtime.ResourceLimits{MaxStaticCost: math.MaxUint64}, "LIMIT_COST_OVERFLOW"},
	} {
		_, err := runtime.PrepareWithOptions(snapshot, parallel, runtime.PrepareOptions{Limits: test.limits})
		assertValidationLimit(t, err, test.code)
	}
}

func TestDefaultResourceLimitsAreFiniteAndNonZero(t *testing.T) {
	t.Parallel()
	limits := runtime.DefaultResourceLimits()
	if limits.MaxPlanDepth == 0 || limits.MaxPlanNodes == 0 || limits.MaxStaticCost == 0 || limits.MaxValidationErrors == 0 ||
		limits.MaxRuntimeWork == 0 || limits.MaxCollectionItems == 0 || limits.MaxConcurrency == 0 ||
		limits.MaxExecutionDuration <= 0 || limits.MaxOutputBytes == 0 || limits.MaxErrors == 0 ||
		limits.MaxOutputDepth == 0 || limits.MaxOutputNodes == 0 {
		t.Fatalf("incomplete defaults: %#v", limits)
	}
}

func TestPrepareRejectsUnallocatableConcurrency(t *testing.T) {
	t.Parallel()
	snapshot, _ := staticResourceSnapshot(t, 1)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"user","select":[{"$field":{"name":"name"}}]}}]}]}}`)
	_, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{
		Limits: runtime.ResourceLimits{MaxConcurrency: 65_537},
	})
	if err == nil {
		t.Fatal("oversized concurrency limit was accepted")
	}
}

func TestPrepareBoundsValidationDiagnostics(t *testing.T) {
	t.Parallel()
	snapshot := frozenRegistry(t, runtime.NewRegistry(coreTypes(t)))
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"missingOne"}},{"$call":{"name":"missingTwo"}},{"$call":{"name":"missingThree"}}]}]}}`)

	_, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{
		Limits: runtime.ResourceLimits{MaxValidationErrors: 2},
	})
	var validationErr *runtime.ValidationErrors
	if !errors.As(err, &validationErr) {
		t.Fatalf("PrepareWithOptions error = %T %v, want ValidationErrors", err, err)
	}
	issues := validationErr.Issues()
	if len(issues) != 2 || issues[0].Diagnostic.Code != "UNKNOWN_CALL" ||
		issues[1].Diagnostic.Code != "LIMIT_VALIDATION_ERRORS" || issues[1].Diagnostic.Pointer == "" {
		t.Fatalf("bounded validation issues = %#v", issues)
	}
}

func TestDirectivePlannerAdditionalCostUsesStaticBudget(t *testing.T) {
	t.Parallel()
	descriptor := testDirectiveDescriptor("budgeted", "vendor.budgeted-1")
	descriptor.Phases = append(descriptor.Phases, schema.DirectivePlanning)
	snapshot := directiveTestSnapshot(t, runtime.DirectiveDefinition{
		Descriptor: descriptor,
		Planner: runtime.DirectivePlannerFunc(func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
			return runtime.DirectivePlanDecision{AdditionalCost: 5}, nil
		}),
	})
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.budgeted-1"],"document":{"requires":["vendor.budgeted-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"budgeted","arguments":{"level":{"$literal":1}}}],"select":[{"$field":{"name":"name"}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.budgeted-1": true}})
	_, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxStaticCost: 5}})
	assertValidationLimit(t, err, "LIMIT_STATIC_COST")
}

func TestPageSizeAndTotalCountParticipateInStaticCost(t *testing.T) {
	t.Parallel()
	snapshot, _, _ := collectionResourceSnapshotWithCost(t, 3)
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"page","first":4,"select":[{"$field":{"name":"name"}}]}},{"$meta":{"name":"totalCount","as":"total"}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxStaticCost: 17}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	if got := plan.StaticCost(); got != 17 {
		t.Fatalf("StaticCost = %d, want 17", got)
	}
	_, err = runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxStaticCost: 16}})
	assertValidationLimit(t, err, "LIMIT_STATIC_COST")
}

func TestNestedPageMultiplierOverflowFailsValidation(t *testing.T) {
	t.Parallel()
	const pageSize = uint64(9007199254740991)
	types := freezeCompositionTypes(t,
		schema.TypeDescriptor{ID: "User", Kind: schema.ObjectType, Output: true, MaxDepth: 2, Fields: map[string]schema.FieldDescriptor{
			"friends": {Type: "Users"}, "name": {Type: schema.TypeID(schema.String)},
		}},
		schema.TypeDescriptor{ID: "Users", Kind: schema.ListType, Output: true, Element: "User", MaxDepth: 2},
	)
	registry := runtime.NewRegistry(types)
	rootMetadata := completeMetadata(runtime.ReadEffect)
	rootMetadata.Collection = secureCollectionMetadata(t, pageSize, 0)
	registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: rootMetadata,
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) { return nil, nil }))
	fieldMetadata := completeMetadata(runtime.ReadEffect)
	fieldMetadata.Cost = 1
	fieldMetadata.Collection = secureCollectionMetadata(t, pageSize, 0)
	registerComposition(t, registry, runtime.BindField[map[string]any, []map[string]any](runtime.Descriptor{
		Name: "friends", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: "Users", Metadata: fieldMetadata,
	}, func(context.Context, map[string]any) ([]map[string]any, error) { return nil, nil }))
	nameMetadata := completeMetadata(runtime.ReadEffect)
	nameMetadata.Cost = 1
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: nameMetadata,
	}, func(context.Context, map[string]any) (string, error) { return "", nil }))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"outer","first":9007199254740991,"select":[{"$field":{"name":"friends","select":[{"$page":{"as":"inner","first":9007199254740991,"select":[{"$field":{"name":"name"}}]}}]}}]}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	_, err = runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxCollectionItems: pageSize, MaxStaticCost: math.MaxUint64}})
	assertValidationLimit(t, err, "LIMIT_COST_OVERFLOW")
}

func TestPrepareRejectsPageAboveCollectionMaximumBeforeHandlers(t *testing.T) {
	t.Parallel()
	snapshot, _, fieldCalls := collectionResourceSnapshotWithCost(t, 1)
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"page","first":6,"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	_, err := runtime.Prepare(snapshot, request)
	if got := validationCodes(t, err); !slices.Contains(got, "PAGE_SIZE_LIMIT") {
		t.Fatalf("validation codes = %v, want PAGE_SIZE_LIMIT", got)
	}
	if fieldCalls.Load() != 0 {
		t.Fatalf("field handler calls = %d, want 0", fieldCalls.Load())
	}
}

func TestPrepareResourceLimitsConfigureFragmentExpansion(t *testing.T) {
	t.Parallel()
	snapshot, _ := staticResourceSnapshot(t, 1)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"user","select":[{"$fragment":{"name":"Pair"}}]}}]}],"fragments":[{"name":"Pair","on":"User","select":[{"$field":{"name":"name","as":"a"}},{"$field":{"name":"name","as":"b"}}]}]}}`)
	_, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxFragmentSelections: 1}})
	assertValidationLimit(t, err, "FRAGMENT_EXPANSION_LIMIT")
}

func TestPrepareResourceLimitsConfigureFragmentDepth(t *testing.T) {
	t.Parallel()
	snapshot, _ := staticResourceSnapshot(t, 1)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"user","select":[{"$fragment":{"name":"Outer"}}]}}]}],"fragments":[{"name":"Outer","on":"User","select":[{"$fragment":{"name":"Inner"}}]},{"name":"Inner","on":"User","select":[{"$field":{"name":"name"}}]}]}}`)
	_, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxFragmentExpansionDepth: 1}})
	assertValidationLimit(t, err, "FRAGMENT_EXPANSION_LIMIT")
}

func TestPrepareResourceLimitsAcceptExactStaticBoundary(t *testing.T) {
	t.Parallel()
	snapshot, _ := staticResourceSnapshot(t, 5)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"user","as":"a","select":[{"$field":{"name":"name"}}]}},{"$call":{"name":"user","as":"b","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	_, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{
		MaxPlanDepth: 3, MaxPlanNodes: 5, MaxPlannedCalls: 2, MaxSelectedFields: 2,
		MaxParallelWidth: 2, MaxQueuedWork: 2, MaxStaticCost: 12,
	}})
	if err != nil {
		t.Fatalf("exact static boundary: %v", err)
	}
}

func TestRuntimeCollectionBudgetStopsBeforeNextHandler(t *testing.T) {
	t.Parallel()
	snapshot, fieldCalls := collectionResourceSnapshot(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name","as":"label"}}]}}]}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxRuntimeWork: 4}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if fieldCalls.Load() != 2 || !hasExecutionCode(outcome.Errors, runtime.CodeResourceExhausted) {
		t.Fatalf("collection budget calls=%d outcome=%#v", fieldCalls.Load(), outcome)
	}
}

func TestRuntimeCollectionBudgetAcceptsExactBoundary(t *testing.T) {
	t.Parallel()
	snapshot, fieldCalls := collectionResourceSnapshot(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name","as":"label"}}]}}]}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxRuntimeWork: 5}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if fieldCalls.Load() != 3 || len(outcome.Errors) != 0 {
		t.Fatalf("exact runtime boundary calls=%d outcome=%#v", fieldCalls.Load(), outcome)
	}
}

func TestRuntimeCollectionMultipliesDeclaredHandlerCost(t *testing.T) {
	t.Parallel()
	snapshot, _, fieldCalls := collectionResourceSnapshotWithCost(t, 3)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxRuntimeWork: 5}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if fieldCalls.Load() != 1 || !hasExecutionCode(outcome.Errors, runtime.CodeResourceExhausted) {
		t.Fatalf("dynamic cost calls=%d outcome=%#v", fieldCalls.Load(), outcome)
	}
}

func TestRuntimeCollectionCardinalityFailsBeforeAllocationWork(t *testing.T) {
	t.Parallel()
	snapshot, fieldCalls := collectionResourceSnapshot(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxCollectionItems: 2}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if fieldCalls.Load() != 0 || !hasExecutionCode(outcome.Errors, runtime.CodeResourceExhausted) {
		t.Fatalf("cardinality calls=%d outcome=%#v", fieldCalls.Load(), outcome)
	}
}

func TestNestedParallelGroupsShareConfiguredConcurrency(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	var active atomic.Int64
	var maximum atomic.Int64
	twoStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "work", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		if current == 2 {
			once.Do(func() { close(twoStarted) })
		}
		<-release
		return "ok", nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$parallel":{"select":[{"$call":{"name":"work","as":"a"}},{"$call":{"name":"work","as":"b"}},{"$call":{"name":"work","as":"c"}}]}},{"$parallel":{"select":[{"$call":{"name":"work","as":"d"}},{"$call":{"name":"work","as":"e"}},{"$call":{"name":"work","as":"f"}}]}}]}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(frozenRegistry(t, registry), request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxConcurrency: 2}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	returned := make(chan runtime.Outcome, 1)
	go func() { returned <- plan.Execute(context.Background()) }()
	<-twoStarted
	close(release)
	outcome := <-returned
	if len(outcome.Errors) != 0 || maximum.Load() != 2 {
		t.Fatalf("parallel maximum=%d outcome=%#v", maximum.Load(), outcome)
	}
}

func TestExecutionDeadlineIsResourceExhaustion(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "wait", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"wait"}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(frozenRegistry(t, registry), request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxExecutionDuration: time.Millisecond}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeResourceExhausted || len(outcome.Errors[0].Path) == 0 {
		t.Fatalf("deadline outcome = %#v", outcome)
	}
}

func TestExecutionDeadlineDoesNotWaitForUncooperativeHandler(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	observedCancellation := make(chan struct{})
	release := make(chan struct{})
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "ignore", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		<-ctx.Done()
		close(observedCancellation)
		<-release
		return "", ctx.Err()
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"ignore"}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(frozenRegistry(t, registry), request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxExecutionDuration: time.Millisecond}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	returned := make(chan runtime.Outcome, 1)
	go func() {
		returned <- plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{AbandonGrace: time.Hour})
	}()
	select {
	case <-observedCancellation:
	case outcome := <-returned:
		close(release)
		if !hasExecutionCode(outcome.Errors, runtime.CodeResourceExhausted) {
			t.Fatalf("pre-admission deadline outcome = %#v", outcome)
		}
		return
	case <-time.After(time.Second):
		close(release)
		t.Fatal("execution neither admitted the handler nor returned at the resource deadline")
	}
	select {
	case outcome := <-returned:
		close(release)
		if !hasExecutionCode(outcome.Errors, runtime.CodeResourceExhausted) {
			t.Fatalf("deadline outcome = %#v", outcome)
		}
	case <-time.After(200 * time.Millisecond):
		close(release)
		<-returned
		t.Fatal("resource deadline waited for the general cancellation grace")
	}
}

func TestOutputAndErrorBudgetsPreserveTruthfulEffectsAndValidJSON(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.WriteEffect),
	}, func(context.Context, runtime.Invocation) (string, error) { return string(make([]byte, 2048)), nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"write"}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(frozenRegistry(t, registry), request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxOutputBytes: 512, MaxErrors: 1}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	outcome := plan.Execute(context.Background())
	encoded, marshalErr := json.Marshal(outcome)
	if marshalErr != nil || len(encoded) > 512 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeResourceExhausted || outcome.Effects != runtime.EffectApplied {
		t.Fatalf("bounded outcome bytes=%d marshal=%v outcome=%#v", len(encoded), marshalErr, outcome)
	}
}

func TestOutputCompletionResourceLimitsFailBeforeChildHandlers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		limits runtime.ResourceLimits
	}{
		{"depth", runtime.ResourceLimits{MaxOutputDepth: 1}},
		{"nodes", runtime.ResourceLimits{MaxOutputNodes: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, fieldCalls := collectionResourceSnapshot(t)
			request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
			plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: test.limits})
			if err != nil {
				t.Fatalf("PrepareWithOptions: %v", err)
			}
			outcome := plan.Execute(context.Background())
			if fieldCalls.Load() != 0 || !hasExecutionCode(outcome.Errors, runtime.CodeResourceExhausted) {
				t.Fatalf("completion budget calls=%d outcome=%#v", fieldCalls.Load(), outcome)
			}
		})
	}
}

func TestErrorBudgetIsRequestWideAndReportsExhaustion(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	var calls atomic.Int64
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "fail", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "", errors.New("expected failure")
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"fail","as":"a"}},{"$call":{"name":"fail","as":"b"}},{"$call":{"name":"fail","as":"c"}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(frozenRegistry(t, registry), request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxErrors: 2}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if calls.Load() != 2 || len(outcome.Errors) != 2 || !hasExecutionCode(outcome.Errors, runtime.CodeResourceExhausted) ||
		len(outcome.Errors[1].Path) != 1 || outcome.Errors[1].Path[0] != "$errors" {
		t.Fatalf("error budget outcome = %#v", outcome)
	}
}

func staticResourceSnapshot(t *testing.T, cost uint64) (runtime.Snapshot, *atomic.Int64) {
	t.Helper()
	registry := runtime.NewRegistry(compositionTypes(t))
	calls := &atomic.Int64{}
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Cost = cost
	if err := registry.Register(runtime.BindInvocation[map[string]any](runtime.Descriptor{
		Name: "user", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "User", Metadata: metadata,
	}, func(context.Context, runtime.Invocation) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"name": "Ada"}, nil
	})); err != nil {
		t.Fatalf("Register call: %v", err)
	}
	fieldMetadata := completeMetadata(runtime.ReadEffect)
	fieldMetadata.Cost = 1
	if err := registry.Register(runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: fieldMetadata,
	}, func(_ context.Context, source map[string]any) (string, error) { return source["name"].(string), nil })); err != nil {
		t.Fatalf("Register field: %v", err)
	}
	return frozenRegistry(t, registry), calls
}

func collectionResourceSnapshot(t *testing.T) (runtime.Snapshot, *atomic.Int64) {
	snapshot, _, fieldCalls := collectionResourceSnapshotWithCost(t, 0)
	return snapshot, fieldCalls
}

func collectionResourceSnapshotWithCost(t *testing.T, fieldCost uint64, configured ...*runtime.CollectionMetadata) (runtime.Snapshot, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	registry := runtime.NewRegistry(compositionTypes(t))
	collectionMetadata := completeMetadata(runtime.ReadEffect)
	collectionMetadata.Collection = secureCollectionMetadata(t, 5, 5)
	if len(configured) != 0 {
		collectionMetadata.Collection = configured[0]
	}
	rootCalls := &atomic.Int64{}
	if err := registry.Register(runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: collectionMetadata,
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		rootCalls.Add(1)
		return []map[string]any{{"name": "Ada"}, {"name": "Grace"}, {"name": "Lin"}}, nil
	})); err != nil {
		t.Fatalf("Register call: %v", err)
	}
	fieldCalls := &atomic.Int64{}
	fieldMetadata := completeMetadata(runtime.ReadEffect)
	fieldMetadata.Cost = fieldCost
	if err := registry.Register(runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: fieldMetadata,
	}, func(_ context.Context, source map[string]any) (string, error) {
		fieldCalls.Add(1)
		return source["name"].(string), nil
	})); err != nil {
		t.Fatalf("Register field: %v", err)
	}
	return frozenRegistry(t, registry), rootCalls, fieldCalls
}

func assertValidationLimit(t *testing.T, err error, code string) {
	t.Helper()
	var failures *runtime.ValidationErrors
	if !errors.As(err, &failures) {
		t.Fatalf("error = %v, want validation limit %s", err, code)
	}
	for _, issue := range failures.Issues() {
		if issue.Diagnostic.Code == code {
			if issue.Diagnostic.Pointer == "" {
				t.Fatalf("limit %s has no source pointer", code)
			}
			return
		}
	}
	t.Fatalf("validation codes = %v, want %s", validationCodes(t, err), code)
}

func hasExecutionCode(errors []runtime.ExecutionError, code string) bool {
	for _, failure := range errors {
		if failure.Code == code && len(failure.Path) != 0 {
			return true
		}
	}
	return false
}
