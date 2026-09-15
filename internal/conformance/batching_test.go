package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestBatchingFixtureInventory(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Profile         string            `json:"profile"`
		DispatchVectors []json.RawMessage `json:"dispatchVectors"`
		CacheVectors    []json.RawMessage `json:"cacheVectors"`
	}
	readFixture(t, "batching.json", &fixture)
	if fixture.Profile != "core.batch-cache-1" {
		t.Fatalf("batch profile = %q", fixture.Profile)
	}
	required := map[string]bool{
		"mapped-duplicates-out-of-order":      false,
		"missing-result-keeps-item-path":      false,
		"parallel-distinct-inputs":            false,
		"width-one-terminates":                false,
		"nested-loaders-terminate":            false,
		"cancelled-waiters-terminate":         false,
		"serial-write-barrier":                false,
		"read-write-read-invalidates":         false,
		"complete-key-partition":              false,
		"explicit-error-null-policy":          false,
		"no-store-disables-reuse":             false,
		"core-retains-no-cross-request-entry": false,
	}
	for _, vector := range fixture.DispatchVectors {
		name := batchingVectorName(t, vector)
		if _, ok := required[name]; !ok || required[name] {
			t.Fatalf("unknown or duplicate dispatch vector %q", name)
		}
		required[name] = true
	}
	for _, vector := range fixture.CacheVectors {
		name := batchingVectorName(t, vector)
		if _, ok := required[name]; !ok || required[name] {
			t.Fatalf("unknown or duplicate cache vector %q", name)
		}
		required[name] = true
	}
	for name, found := range required {
		if !found {
			t.Errorf("missing batching vector %q", name)
		}
	}
}

func batchingVectorName(t testing.TB, raw json.RawMessage) string {
	t.Helper()
	var vector struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil || vector.Name == "" {
		t.Fatalf("invalid batching vector: %v", err)
	}
	return vector.Name
}

type batchingFixtureTyped struct {
	Profile         string                   `json:"profile"`
	DispatchVectors []batchingDispatchVector `json:"dispatchVectors"`
	CacheVectors    []batchingCacheVector    `json:"cacheVectors"`
}

type batchingDispatchVector struct {
	Name                    string             `json:"name"`
	Window                  string             `json:"window"`
	Inputs                  []string           `json:"inputs"`
	MaximumSize             int                `json:"maximumSize"`
	MaximumConcurrency      int                `json:"maximumConcurrency"`
	BackendResultIndexes    []int              `json:"backendResultIndexes"`
	ExpectedDispatches      int                `json:"expectedDispatches"`
	ExpectedUniqueInputs    int                `json:"expectedUniqueInputs"`
	ExpectedOutput          []any              `json:"expectedOutput"`
	ExpectedErrors          []batchingExpected `json:"expectedErrors"`
	ExpectedOuterDispatches int                `json:"expectedOuterDispatches"`
	ExpectedInnerDispatches int                `json:"expectedInnerDispatches"`
	ExpectedHandlerStarts   int                `json:"expectedHandlerStarts"`
	ExpectedCode            string             `json:"expectedCode"`
	MustTerminate           bool               `json:"mustTerminate"`
	Events                  []string           `json:"events"`
}

type batchingExpected struct {
	Code string `json:"code"`
	Path []any  `json:"path"`
}

type batchingCacheVector struct {
	Name                      string   `json:"name"`
	Events                    []string `json:"events"`
	ExpectedReadHandlerStarts int      `json:"expectedReadHandlerStarts"`
	ExpectedInvalidations     int      `json:"expectedInvalidations"`
	Dimensions                []string `json:"dimensions"`
	DefaultCachedErrors       bool     `json:"defaultCachedErrors"`
	DefaultCachedNulls        bool     `json:"defaultCachedNulls"`
	IndependentlyConfigurable bool     `json:"independentlyConfigurable"`
	AuthorizationScope        string   `json:"authorizationScope"`
	ExpectedCacheHits         int      `json:"expectedCacheHits"`
	ApplicationHook           bool     `json:"applicationHook"`
	Executions                int      `json:"executions"`
	ExpectedHandlerStarts     int      `json:"expectedHandlerStarts"`
}

// TestBatchingFixtureAgainstRuntime decodes each batch/cache vector and drives
// it through the real batch dispatcher and request/application cache in package
// runtime (Prepare + Plan.Execute/ExecuteWith with BindBatchField and an
// external ResultCache), asserting that the runtime's actual dispatch counts,
// item ordering, error paths, and cache behavior match the fixture.
func TestBatchingFixtureAgainstRuntime(t *testing.T) {
	t.Parallel()
	var fixture batchingFixtureTyped
	readFixture(t, "batching.json", &fixture)
	if fixture.Profile != "core.batch-cache-1" {
		t.Fatalf("batch profile = %q", fixture.Profile)
	}

	dispatchDrivers := map[string]func(*testing.T, batchingDispatchVector){
		"mapped-duplicates-out-of-order": batchingDriveMappedDuplicates,
		"missing-result-keeps-item-path": batchingDriveMissingResult,
		"parallel-distinct-inputs":       batchingDriveParallelDistinct,
		"width-one-terminates":           batchingDriveWidthOne,
		"nested-loaders-terminate":       batchingDriveNestedLoaders,
		"cancelled-waiters-terminate":    batchingDriveCancelledWaiters,
		"serial-write-barrier":           batchingDriveSerialWriteBarrier,
	}
	cacheDrivers := map[string]func(*testing.T, batchingCacheVector){
		"read-write-read-invalidates":         batchingDriveReadWriteRead,
		"complete-key-partition":              batchingDriveCompleteKeyPartition,
		"explicit-error-null-policy":          batchingDriveErrorNullPolicy,
		"no-store-disables-reuse":             batchingDriveNoStore,
		"core-retains-no-cross-request-entry": batchingDriveNoCrossRequest,
	}

	executed := 0
	for _, vector := range fixture.DispatchVectors {
		driver, ok := dispatchDrivers[vector.Name]
		if !ok {
			continue
		}
		executed++
		vector := vector
		t.Run("dispatch/"+vector.Name, func(t *testing.T) {
			t.Parallel()
			driver(t, vector)
		})
	}
	for _, vector := range fixture.CacheVectors {
		driver, ok := cacheDrivers[vector.Name]
		if !ok {
			continue
		}
		executed++
		vector := vector
		t.Run("cache/"+vector.Name, func(t *testing.T) {
			t.Parallel()
			driver(t, vector)
		})
	}
	if executed < 12 {
		t.Fatalf("drove only %d batch/cache vectors against the runtime, want at least 12", executed)
	}
}

func batchingDriveMappedDuplicates(t *testing.T, vector batchingDispatchVector) {
	var invocations, dispatchedInputs atomic.Int64
	snapshot := batchingUsersSnapshot(t, batchingUserRecords(vector.Inputs), vector.MaximumSize, false,
		func(inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
			invocations.Add(1)
			dispatchedInputs.Add(int64(len(inputs)))
			results := make([]runtime.BatchResult[string], 0, len(inputs))
			for index := len(inputs) - 1; index >= 0; index-- {
				results = append(results, runtime.BatchResult[string]{Index: inputs[index].Index, Value: inputs[index].Value["name"].(string)})
			}
			return results
		})
	plan := batchingMapPlan(t, snapshot)
	var events []runtime.BatchEvent
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Batch: runtime.BatchRuntimeOptions{
		Observe: func(event runtime.BatchEvent) { events = append(events, event) },
	}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
	assertBatchItems(t, outcome.Data, vector.ExpectedOutput)
	if int(invocations.Load()) != vector.ExpectedDispatches || int(dispatchedInputs.Load()) != vector.ExpectedUniqueInputs {
		t.Fatalf("dispatches = %d unique = %d, want %d/%d", invocations.Load(), dispatchedInputs.Load(), vector.ExpectedDispatches, vector.ExpectedUniqueInputs)
	}
}

func batchingDriveMissingResult(t *testing.T, vector batchingDispatchVector) {
	snapshot := batchingUsersSnapshot(t, batchingUserRecords(vector.Inputs), vector.MaximumSize, false,
		func(inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
			results := make([]runtime.BatchResult[string], 0, len(vector.BackendResultIndexes))
			for _, index := range vector.BackendResultIndexes {
				results = append(results, runtime.BatchResult[string]{Index: inputs[index].Index, Value: inputs[index].Value["name"].(string)})
			}
			return results
		})
	plan := batchingMapPlan(t, snapshot)
	outcome := plan.Execute(context.Background())
	assertBatchItems(t, outcome.Data, vector.ExpectedOutput)
	if len(vector.ExpectedErrors) != len(outcome.Errors) {
		t.Fatalf("errors = %#v, want %#v", outcome.Errors, vector.ExpectedErrors)
	}
	for index, expected := range vector.ExpectedErrors {
		actual := outcome.Errors[index]
		if actual.Code != expected.Code || !slices.Equal(batchingNormalizePath(actual.Path), batchingNormalizePath(expected.Path)) {
			t.Fatalf("error %d = %#v, want %#v", index, actual, expected)
		}
	}
}

func batchingDriveParallelDistinct(t *testing.T, vector batchingDispatchVector) {
	registry := runtime.NewRegistry(batchingCompositionTypes(t))
	metadata := conformanceMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var invocations atomic.Int64
	if err := registry.Register(runtime.BindBatch[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[schema.InputValue]{Key: batchingLookupKey}, func(_ context.Context, inputs []runtime.BatchInput[schema.InputValue]) []runtime.BatchResult[string] {
		invocations.Add(1)
		results := make([]runtime.BatchResult[string], len(inputs))
		for index, input := range inputs {
			results[len(inputs)-1-index] = runtime.BatchResult[string]{Index: input.Index, Value: batchingLookupValue(input.Value)}
		}
		return results
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot := batchingFreeze(t, registry)
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$literal":"u-1"}}}},{"$call":{"name":"lookupName","as":"right","args":{"id":{"$literal":"u-2"}}}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || int(invocations.Load()) != vector.ExpectedDispatches {
		t.Fatalf("parallel outcome = %#v dispatches = %d, want %d", outcome, invocations.Load(), vector.ExpectedDispatches)
	}
	batchingAssertJSONEqual(t, outcome.Data, map[string]any{"left": `"u-1"`, "right": `"u-2"`})
}

func batchingDriveWidthOne(t *testing.T, vector batchingDispatchVector) {
	var calls atomic.Int64
	snapshot := batchingUsersSnapshot(t, batchingUserRecords(vector.Inputs), 0, false,
		func(inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
			calls.Add(1)
			results := make([]runtime.BatchResult[string], len(inputs))
			for index, input := range inputs {
				results[index] = runtime.BatchResult[string]{Index: input.Index, Value: input.Value["name"].(string)}
			}
			return results
		})
	plan, err := runtime.PrepareWithOptions(snapshot, decodeConformanceRequest(t, batchingMapQuery()), runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxConcurrency: uint64(vector.MaximumConcurrency)}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	done := make(chan runtime.Outcome, 1)
	go func() { done <- plan.Execute(context.Background()) }()
	select {
	case outcome := <-done:
		if len(outcome.Errors) != 0 || int(calls.Load()) != vector.ExpectedDispatches {
			t.Fatalf("width-one outcome = %#v calls = %d, want %d", outcome, calls.Load(), vector.ExpectedDispatches)
		}
	case <-time.After(time.Second):
		t.Fatal("width-one batch dispatch deadlocked")
	}
}

func batchingDriveNestedLoaders(t *testing.T, vector batchingDispatchVector) {
	types := conformanceCompositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.Register(runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "groups", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Groups", Metadata: conformanceMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		return []map[string]any{
			{"name": "pioneers", "users": []map[string]any{{"name": "Ada"}, {"name": "Grace"}}},
			{"name": "systems", "users": []map[string]any{{"name": "Lin"}}},
		}, nil
	})); err != nil {
		t.Fatalf("Register groups: %v", err)
	}
	metadata := conformanceMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var groupLoads, userLoads atomic.Int64
	if err := registry.Register(runtime.BindBatchField[map[string]any, []any](runtime.Descriptor{
		Name: "users", Scope: runtime.ObjectScope, Owner: "Group", Member: runtime.FieldMember,
		Output: "Users", Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{Key: batchingNameKey}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[[]any] {
		groupLoads.Add(1)
		results := make([]runtime.BatchResult[[]any], 0, len(inputs))
		for index := len(inputs) - 1; index >= 0; index-- {
			results = append(results, runtime.BatchResult[[]any]{Index: inputs[index].Index, Value: inputs[index].Value["users"].([]any)})
		}
		return results
	})); err != nil {
		t.Fatalf("Register users: %v", err)
	}
	if err := registry.Register(runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{Key: batchingNameKey}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
		userLoads.Add(1)
		results := make([]runtime.BatchResult[string], len(inputs))
		for index, input := range inputs {
			results[index] = runtime.BatchResult[string]{Index: input.Index, Value: input.Value["name"].(string)}
		}
		return results
	})); err != nil {
		t.Fatalf("Register name: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot := batchingFreeze(t, registry)
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"groups","select":[{"$map":{"as":"groups","select":[{"$field":{"name":"users","select":[{"$map":{"as":"members","select":[{"$field":{"name":"name"}}]}}]}}]}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	done := make(chan runtime.Outcome, 1)
	go func() { done <- plan.Execute(context.Background()) }()
	select {
	case outcome := <-done:
		if len(outcome.Errors) != 0 || int(groupLoads.Load()) != vector.ExpectedOuterDispatches || int(userLoads.Load()) != vector.ExpectedInnerDispatches {
			t.Fatalf("nested outcome = %#v outer = %d inner = %d, want %d/%d", outcome, groupLoads.Load(), userLoads.Load(), vector.ExpectedOuterDispatches, vector.ExpectedInnerDispatches)
		}
	case <-time.After(time.Second):
		t.Fatal("nested batch loaders deadlocked")
	}
}

func batchingDriveCancelledWaiters(t *testing.T, vector batchingDispatchVector) {
	registry := runtime.NewRegistry(batchingCompositionTypes(t))
	started := make(chan struct{})
	var calls atomic.Int64
	if err := registry.Register(runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: conformanceMetadata(runtime.ReadEffect),
	}, func(ctx context.Context, _ schema.InputValue) (string, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot := batchingFreeze(t, registry)
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$literal":"same"}}}},{"$call":{"name":"lookupName","as":"right","args":{"id":{"$literal":"same"}}}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan runtime.Outcome, 1)
	go func() { done <- plan.Execute(ctx) }()
	<-started
	cancel()
	select {
	case outcome := <-done:
		if !batchingHasCode(outcome.Errors, vector.ExpectedCode) || int(calls.Load()) != vector.ExpectedHandlerStarts {
			t.Fatalf("cancelled outcome = %#v calls = %d, want code %q starts %d", outcome, calls.Load(), vector.ExpectedCode, vector.ExpectedHandlerStarts)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled cache waiter did not terminate")
	}
}

func batchingDriveSerialWriteBarrier(t *testing.T, vector batchingDispatchVector) {
	types := conformanceCompositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.Register(runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: conformanceMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		return batchingUserRecords(vector.Inputs), nil
	})); err != nil {
		t.Fatalf("Register users: %v", err)
	}
	metadata := conformanceMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var mu sync.Mutex
	var events []string
	var dispatches atomic.Int64
	if err := registry.Register(runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{Key: batchingNameKey}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
		dispatches.Add(1)
		mu.Lock()
		defer mu.Unlock()
		results := make([]runtime.BatchResult[string], len(inputs))
		for index, input := range inputs {
			name := input.Value["name"].(string)
			events = append(events, "read:"+name)
			results[index] = runtime.BatchResult[string]{Index: input.Index, Value: name}
		}
		return results
	})); err != nil {
		t.Fatalf("Register name: %v", err)
	}
	if err := registry.Register(runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "mutateName", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: conformanceMetadata(runtime.WriteEffect),
	}, func(_ context.Context, source map[string]any) (string, error) {
		name := source["name"].(string)
		mu.Lock()
		events = append(events, "write:"+name)
		mu.Unlock()
		return name, nil
	})); err != nil {
		t.Fatalf("Register mutateName: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot := batchingFreeze(t, registry)
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}},{"$field":{"name":"mutateName"}}]}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(events, vector.Events) {
		t.Fatalf("barrier events = %v, want %v", events, vector.Events)
	}
	if int(dispatches.Load()) != vector.ExpectedDispatches {
		t.Fatalf("barrier dispatches = %d, want %d", dispatches.Load(), vector.ExpectedDispatches)
	}
}

func batchingDriveReadWriteRead(t *testing.T, vector batchingCacheVector) {
	types := conformanceCompositionTypes(t)
	registry := runtime.NewRegistry(types)
	var state struct {
		sync.Mutex
		name string
	}
	state.name = "before"
	var reads atomic.Int64
	if err := registry.Register(runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "readName", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: conformanceMetadata(runtime.ReadEffect),
	}, func(context.Context, schema.InputValue) (string, error) {
		reads.Add(1)
		state.Lock()
		defer state.Unlock()
		return state.name, nil
	})); err != nil {
		t.Fatalf("Register readName: %v", err)
	}
	if err := registry.Register(runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "setName", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: "SetNameInput", Output: schema.TypeID(schema.String), Metadata: conformanceMetadata(runtime.WriteEffect),
	}, func(_ context.Context, input schema.InputValue) (string, error) {
		members, _ := input.Object()
		value, _ := members["value"].Scalar()
		encoded, _ := value.MarshalJSON()
		var name string
		if err := json.Unmarshal(encoded, &name); err != nil {
			return "", err
		}
		state.Lock()
		state.name = name
		state.Unlock()
		return name, nil
	})); err != nil {
		t.Fatalf("Register setName: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{Allowed: true, CacheScope: runtime.AuthorizationCachePrincipal, AuthorizationRevision: request.Principal.AuthorizationRevision}, nil
		}),
	}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot := batchingFreeze(t, registry)
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"M","kind":"mutation","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"readName","as":"before","args":{"id":{"$var":"id"}}}},{"$call":{"name":"setName","as":"write","args":{"value":{"$literal":"after"}}}},{"$call":{"name":"readName","as":"after","args":{"id":{"$var":"id"}}}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cache := &recordingConformanceCache{}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "alice", AuthorizationRevision: "policy-r1"})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Cache: runtime.CacheOptions{SchemaRevision: "schema-r1", External: cache}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
	if int(reads.Load()) != vector.ExpectedReadHandlerStarts {
		t.Fatalf("read handler starts = %d, want %d", reads.Load(), vector.ExpectedReadHandlerStarts)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.invalidations) != vector.ExpectedInvalidations {
		t.Fatalf("invalidations = %d, want %d", len(cache.invalidations), vector.ExpectedInvalidations)
	}
}

func batchingDriveCompleteKeyPartition(t *testing.T, vector batchingCacheVector) {
	registry := runtime.NewRegistry(batchingCompositionTypes(t))
	if err := registry.Register(runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: conformanceMetadata(runtime.ReadEffect),
	}, func(context.Context, schema.InputValue) (string, error) { return "Ada", nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{Allowed: true, AuthorizationRevision: request.Principal.AuthorizationRevision, CacheScope: runtime.AuthorizationCachePrincipal}, nil
		}),
	}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot := batchingFreeze(t, registry)
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$var":"id"}}}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cache := &recordingConformanceCache{}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "alice", Tenant: "tenant-1", AuthorizationRevision: "policy-r1"})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Cache: runtime.CacheOptions{SchemaRevision: "schema-r1", Context: "locale=en", External: cache}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.keys) == 0 {
		t.Fatal("external cache received no keys")
	}
	// Assert every dimension the fixture names is actually populated by the
	// runtime's real cache key for at least one emitted entry.
	populated := map[string]func(runtime.CacheKey) bool{
		"operationKind":         func(k runtime.CacheKey) bool { return k.OperationKind != "" },
		"operation":             func(k runtime.CacheKey) bool { return k.Operation != "" },
		"handler":               func(k runtime.CacheKey) bool { return k.Handler != "" },
		"documentDigest":        func(k runtime.CacheKey) bool { return k.DocumentDigest != "" },
		"schemaDigest":          func(k runtime.CacheKey) bool { return k.SchemaDigest != "" },
		"schemaRevision":        func(k runtime.CacheKey) bool { return k.SchemaRevision != "" },
		"variablesDigest":       func(k runtime.CacheKey) bool { return k.VariablesDigest != "" },
		"authorizationIdentity": func(k runtime.CacheKey) bool { return k.AuthorizationIdentity != "" },
		"typedInputDigest":      func(k runtime.CacheKey) bool { return k.InputDigest != "" },
		"context":               func(k runtime.CacheKey) bool { return k.Context != "" },
	}
	for _, dimension := range vector.Dimensions {
		predicate, ok := populated[dimension]
		if !ok {
			t.Fatalf("cache dimension %q has no runtime CacheKey field mapping", dimension)
		}
		if !slices.ContainsFunc(cache.keys, predicate) {
			t.Fatalf("no emitted cache key populates dimension %q: %#v", dimension, cache.keys)
		}
	}
}

func batchingDriveErrorNullPolicy(t *testing.T, vector batchingCacheVector) {
	if vector.DefaultCachedErrors || vector.DefaultCachedNulls || !vector.IndependentlyConfigurable {
		t.Fatalf("unexpected error/null policy declaration: %#v", vector)
	}
	// Errors are not stored by default (both calls run), but are stored when the
	// request explicitly opts in (only one call runs).
	if plan, calls := batchingCachePolicyPlan(t, true, false); true {
		if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 2 || calls.Load() != 2 {
			t.Fatalf("default error cache outcome = %#v calls = %d", outcome, calls.Load())
		}
	}
	if plan, calls := batchingCachePolicyPlan(t, true, false); true {
		if outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Cache: runtime.CacheOptions{CacheErrors: true}}); len(outcome.Errors) != 2 || calls.Load() != 1 {
			t.Fatalf("enabled error cache outcome = %#v calls = %d", outcome, calls.Load())
		}
	}
	// Nulls are not stored by default, but are stored when explicitly enabled,
	// independently of the error policy.
	if plan, calls := batchingCachePolicyPlan(t, false, true); true {
		if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 || calls.Load() != 2 {
			t.Fatalf("default null cache outcome = %#v calls = %d", outcome, calls.Load())
		}
	}
	if plan, calls := batchingCachePolicyPlan(t, false, true); true {
		if outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Cache: runtime.CacheOptions{CacheNulls: true}}); len(outcome.Errors) != 0 || calls.Load() != 1 {
			t.Fatalf("enabled null cache outcome = %#v calls = %d", outcome, calls.Load())
		}
	}
}

func batchingDriveNoStore(t *testing.T, vector batchingCacheVector) {
	if vector.AuthorizationScope != "no-store" {
		t.Fatalf("unexpected authorization scope %q", vector.AuthorizationScope)
	}
	registry := runtime.NewRegistry(batchingCompositionTypes(t))
	var calls atomic.Int64
	if err := registry.Register(runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: conformanceMetadata(runtime.ReadEffect),
	}, func(context.Context, schema.InputValue) (string, error) {
		calls.Add(1)
		return "Ada", nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{Allowed: true, CacheScope: runtime.AuthorizationCacheNoStore}, nil
		}),
	}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot := batchingFreeze(t, registry)
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$literal":"u-1"}}}},{"$call":{"name":"lookupName","as":"right","args":{"id":{"$literal":"u-1"}}}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cache := &recordingConformanceCache{}
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Cache: runtime.CacheOptions{SchemaRevision: "schema-r1", External: cache}})
	if len(outcome.Errors) != 0 || calls.Load() != 2 {
		t.Fatalf("no-store outcome = %#v calls = %d", outcome, calls.Load())
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.keys) != vector.ExpectedCacheHits {
		t.Fatalf("no-store cache keys = %d, want %d", len(cache.keys), vector.ExpectedCacheHits)
	}
}

func batchingDriveNoCrossRequest(t *testing.T, vector batchingCacheVector) {
	if vector.ApplicationHook {
		t.Fatalf("core cross-request vector must not require an application hook: %#v", vector)
	}
	plan, calls := batchingCachePolicyPlan(t, false, false)
	for run := 0; run < vector.Executions; run++ {
		if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 {
			t.Fatalf("cross-request run %d outcome = %#v", run, outcome)
		}
	}
	if int(calls.Load()) != vector.ExpectedHandlerStarts {
		t.Fatalf("cross-request handler starts = %d, want %d", calls.Load(), vector.ExpectedHandlerStarts)
	}
}

// ---- batching helpers ----

func batchingUserRecords(names []string) []map[string]any {
	records := make([]map[string]any, len(names))
	for index, name := range names {
		records[index] = map[string]any{"name": name}
	}
	return records
}

func batchingCompositionTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	return conformanceCompositionTypes(t)
}

// conformanceCompositionTypes freezes the User/Users/Group/Groups object graph
// plus the LookupInput/SetNameInput input objects used by the batch and cache
// scenarios, mirroring package runtime's own composition test fixtures.
func conformanceCompositionTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	descriptors := []schema.TypeDescriptor{
		{ID: "LookupInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"id": {Type: schema.TypeID(schema.ID), Required: true},
		}},
		{ID: "SetNameInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"value": {Type: schema.TypeID(schema.String), Required: true},
		}},
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name":       {Type: schema.TypeID(schema.String), Required: true},
			"mutateName": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Users", Kind: schema.ListType, Output: true, Element: "User"},
		{ID: "Group", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name":  {Type: schema.TypeID(schema.String), Required: true},
			"users": {Type: "Users", Required: true},
		}},
		{ID: "Groups", Kind: schema.ListType, Output: true, Element: "Group"},
	}
	for _, descriptor := range descriptors {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("register type %s: %v", descriptor.ID, err)
		}
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("freeze composition types: %v", err)
	}
	return snapshot
}

// batchingUsersSnapshot registers a "users" invocation returning records plus a
// batch "name" field on User with the given dispatch behavior, then freezes.
func batchingUsersSnapshot(t *testing.T, records []map[string]any, maxSize int, _ bool, dispatch func([]runtime.BatchInput[map[string]any]) []runtime.BatchResult[string]) runtime.Snapshot {
	t.Helper()
	registry := runtime.NewRegistry(conformanceCompositionTypes(t))
	if err := registry.Register(runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: conformanceMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		return records, nil
	})); err != nil {
		t.Fatalf("Register users: %v", err)
	}
	metadata := conformanceMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	if err := registry.Register(runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{MaxSize: maxSize, Key: batchingNameKey}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
		return dispatch(inputs)
	})); err != nil {
		t.Fatalf("Register name: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	return batchingFreeze(t, registry)
}

func batchingMapPlan(t *testing.T, snapshot runtime.Snapshot) *runtime.Plan {
	t.Helper()
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, batchingMapQuery()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

func batchingMapQuery() string {
	return `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`
}

func batchingFreeze(t *testing.T, registry *runtime.Registry) runtime.Snapshot {
	t.Helper()
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return snapshot
}

func batchingNameKey(source map[string]any) (string, error) {
	return source["name"].(string), nil
}

func batchingLookupKey(input schema.InputValue) (string, error) {
	members, _ := input.Object()
	identifier, _ := members["id"].Scalar()
	encoded, _ := identifier.MarshalJSON()
	return string(encoded), nil
}

func batchingLookupValue(input schema.InputValue) string {
	members, _ := input.Object()
	identifier, _ := members["id"].Scalar()
	encoded, _ := identifier.MarshalJSON()
	return string(encoded)
}

func batchingCachePolicyPlan(t *testing.T, fail, nullable bool) (*runtime.Plan, *atomic.Int64) {
	t.Helper()
	registry := runtime.NewRegistry(conformanceCompositionTypes(t))
	calls := &atomic.Int64{}
	descriptor := runtime.Descriptor{
		Name: "value", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), OutputNullable: nullable,
		Metadata: conformanceMetadata(runtime.ReadEffect),
	}
	if nullable {
		if err := registry.Register(runtime.Bind[schema.InputValue, *string](descriptor, func(context.Context, schema.InputValue) (*string, error) {
			calls.Add(1)
			return nil, nil
		})); err != nil {
			t.Fatalf("Register: %v", err)
		}
	} else {
		if err := registry.Register(runtime.Bind[schema.InputValue, string](descriptor, func(context.Context, schema.InputValue) (string, error) {
			calls.Add(1)
			if fail {
				return "", errors.New("backend unavailable")
			}
			return "ok", nil
		})); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot := batchingFreeze(t, registry)
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"value","as":"first","args":{"id":{"$var":"id"}}}},{"$call":{"name":"value","as":"second","args":{"id":{"$var":"id"}}}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan, calls
}

func batchingHasCode(errorsList []runtime.ExecutionError, code string) bool {
	for _, failure := range errorsList {
		if failure.Code == code {
			return true
		}
	}
	return false
}

func batchingNormalizePath(path []any) []any {
	normalized := make([]any, len(path))
	for index, element := range path {
		switch value := element.(type) {
		case float64:
			normalized[index] = int(value)
		default:
			normalized[index] = value
		}
	}
	return normalized
}

// assertBatchItems compares outcome.Data["users"]["items"] to the fixture's
// expected output list, where a string maps to {"name": value} and null to nil.
func assertBatchItems(t *testing.T, data map[string]any, expected []any) {
	t.Helper()
	items := make([]any, len(expected))
	for index, element := range expected {
		if element == nil {
			items[index] = nil
			continue
		}
		items[index] = map[string]any{"name": element}
	}
	batchingAssertJSONEqual(t, data, map[string]any{"users": map[string]any{"items": items}})
}

func batchingAssertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("result = %s, want %s", gotJSON, wantJSON)
	}
}

type recordingConformanceCache struct {
	mu            sync.Mutex
	entries       map[string]runtime.CacheEntry
	keys          []runtime.CacheKey
	invalidations []runtime.CacheInvalidation
}

func (c *recordingConformanceCache) Load(_ context.Context, key runtime.CacheKey) (runtime.CacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, key)
	entry, ok := c.entries[key.Digest]
	return entry, ok, nil
}

func (c *recordingConformanceCache) Store(_ context.Context, key runtime.CacheKey, entry runtime.CacheEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]runtime.CacheEntry)
	}
	c.keys = append(c.keys, key)
	c.entries[key.Digest] = entry
	return nil
}

func (c *recordingConformanceCache) Invalidate(_ context.Context, invalidation runtime.CacheInvalidation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidations = append(c.invalidations, invalidation)
	c.entries = make(map[string]runtime.CacheEntry)
	return nil
}
