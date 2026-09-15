package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestBatchFieldServesMappedItemsInOneInvocation(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		return []map[string]any{{"name": "Ada"}, {"name": "Grace"}, {"name": "Ada"}}, nil
	}))
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var invocations atomic.Int64
	var dispatchedInputs atomic.Int64
	var authorizedInputs atomic.Int64
	registerComposition(t, registry, runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{
		MaxSize: 16,
		Key:     func(source map[string]any) (string, error) { return source["name"].(string), nil },
	}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
		invocations.Add(1)
		dispatchedInputs.Add(int64(len(inputs)))
		results := make([]runtime.BatchResult[string], 0, len(inputs))
		for index := len(inputs) - 1; index >= 0; index-- {
			results = append(results, runtime.BatchResult[string]{Index: inputs[index].Index, Value: inputs[index].Value["name"].(string)})
		}
		return results
	}))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			if request.Descriptor.Name == "name" {
				authorizedInputs.Add(1)
			}
			return runtime.AuthorizationDecision{Allowed: true, CacheScope: runtime.AuthorizationCacheNoStore}, nil
		}),
	}); err != nil {
		t.Fatalf("configure authorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var batchEvents []runtime.BatchEvent
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Batch: runtime.BatchRuntimeOptions{
		Observe: func(event runtime.BatchEvent) { batchEvents = append(batchEvents, event) },
	}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"users": map[string]any{"items": []any{
		map[string]any{"name": "Ada"}, map[string]any{"name": "Grace"}, map[string]any{"name": "Ada"},
	}}})
	if invocations.Load() != 1 {
		t.Fatalf("batch invocations = %d, want 1", invocations.Load())
	}
	if dispatchedInputs.Load() != 2 {
		t.Fatalf("deduplicated batch inputs = %d, want 2", dispatchedInputs.Load())
	}
	if authorizedInputs.Load() != 3 {
		t.Fatalf("authorized batch inputs = %d, want 3", authorizedInputs.Load())
	}
	if len(batchEvents) != 2 || batchEvents[0].Stage != runtime.BatchDispatchStarted ||
		batchEvents[1].Stage != runtime.BatchDispatchCompleted || batchEvents[0].Size != 2 ||
		batchEvents[0].Handler != "object:User:field:name" {
		t.Fatalf("batch trace events = %#v", batchEvents)
	}
}

func TestBatchInvocationReceivesOperationContext(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(compositionTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	registerComposition(t, registry, runtime.BindBatchInvocation(runtime.Descriptor{
		Name: "operationName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[runtime.Invocation]{Key: func(input runtime.Invocation) (string, error) {
		return input.Operation, nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[runtime.Invocation]) []runtime.BatchResult[string] {
		return []runtime.BatchResult[string]{{Index: inputs[0].Index, Value: inputs[0].Value.Operation}}
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"BatchOperation","kind":"query","select":[{"$call":{"name":"operationName"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"operationName": "BatchOperation"})
}

func TestBatchCallCombinesTypedSourceAndInput(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(compositionTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	registerComposition(t, registry, runtime.BindBatchCall(runtime.Descriptor{
		Name: "suffix", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[runtime.BatchCall[map[string]any, string]]{Key: func(input runtime.BatchCall[map[string]any, string]) (string, error) {
		return input.Source["name"].(string) + input.Input, nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[runtime.BatchCall[map[string]any, string]]) []runtime.BatchResult[string] {
		input := inputs[0]
		return []runtime.BatchResult[string]{{Index: input.Index, Value: input.Value.Source["name"].(string) + input.Value.Input}}
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	value, err := snapshot.InvokeCall(context.Background(), "User", "suffix", map[string]any{"name": "Ada"}, "!")
	if err != nil || value != "Ada!" {
		t.Fatalf("InvokeCall = (%#v, %v), want Ada!", value, err)
	}
}

func TestBatchRegistrationRejectsInvalidContracts(t *testing.T) {
	t.Parallel()
	valid := completeMetadata(runtime.ReadEffect)
	valid.Batching = runtime.BatchEligible
	tests := []struct {
		name     string
		options  runtime.BatchOptions[string]
		metadata runtime.Metadata
	}{
		{name: "missing key", options: runtime.BatchOptions[string]{MaxSize: 1}, metadata: valid},
		{name: "negative maximum", options: runtime.BatchOptions[string]{MaxSize: -1, Key: func(value string) (string, error) { return value, nil }}, metadata: valid},
		{name: "maximum above portable ceiling", options: runtime.BatchOptions[string]{MaxSize: 65_537, Key: func(value string) (string, error) { return value, nil }}, metadata: valid},
		{name: "not eligible", options: runtime.BatchOptions[string]{MaxSize: 1, Key: func(value string) (string, error) { return value, nil }}, metadata: completeMetadata(runtime.ReadEffect)},
	}
	serial := valid
	serial.ThreadSafety = runtime.SerialOnly
	tests = append(tests, struct {
		name     string
		options  runtime.BatchOptions[string]
		metadata runtime.Metadata
	}{name: "serial only", options: runtime.BatchOptions[string]{MaxSize: 1, Key: func(value string) (string, error) { return value, nil }}, metadata: serial})
	write := completeMetadata(runtime.WriteEffect)
	write.Batching = runtime.BatchEligible
	tests = append(tests, struct {
		name     string
		options  runtime.BatchOptions[string]
		metadata runtime.Metadata
	}{name: "write effect", options: runtime.BatchOptions[string]{MaxSize: 1, Key: func(value string) (string, error) { return value, nil }}, metadata: write})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			definition := runtime.BindBatch(runtime.Descriptor{
				Name: "value", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
				Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: tt.metadata,
			}, tt.options, func(_ context.Context, inputs []runtime.BatchInput[string]) []runtime.BatchResult[string] {
				return []runtime.BatchResult[string]{{Index: inputs[0].Index, Value: inputs[0].Value}}
			})
			if err := runtime.NewRegistry(compositionTypes(t)).Register(definition); err == nil {
				t.Fatal("Register succeeded")
			}
		})
	}
}

type recordingResultCache struct {
	mu            sync.Mutex
	entries       map[string]runtime.CacheEntry
	keys          []runtime.CacheKey
	invalidations []runtime.CacheInvalidation
}

func (c *recordingResultCache) Load(_ context.Context, key runtime.CacheKey) (runtime.CacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, key)
	entry, ok := c.entries[key.Digest]
	return entry, ok, nil
}

func (c *recordingResultCache) Store(_ context.Context, key runtime.CacheKey, entry runtime.CacheEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]runtime.CacheEntry)
	}
	c.keys = append(c.keys, key)
	c.entries[key.Digest] = entry
	return nil
}

func (c *recordingResultCache) Invalidate(_ context.Context, invalidation runtime.CacheInvalidation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidations = append(c.invalidations, invalidation)
	c.entries = make(map[string]runtime.CacheEntry)
	return nil
}

func TestCacheDeduplicatesParallelReadsAndPartitionsExternalEntries(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, schema.InputValue) (string, error) {
		calls.Add(1)
		return "Ada", nil
	}))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{
				Allowed: true, AuthorizationRevision: request.Principal.AuthorizationRevision,
				CacheScope: runtime.AuthorizationCachePrincipal,
			}, nil
		}),
	}); err != nil {
		t.Fatalf("configure authorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$var":"id"}}}},{"$call":{"name":"lookupName","as":"right","args":{"id":{"$var":"id"}}}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cache := &recordingResultCache{}
	options := runtime.ExecuteOptions{Cache: runtime.CacheOptions{
		SchemaRevision: "schema-r1", Context: "locale=en", External: cache,
	}}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "alice", Tenant: "tenant-1", AuthorizationRevision: "policy-r1",
	})
	first := plan.ExecuteWith(ctx, options)
	if len(first.Errors) != 0 {
		t.Fatalf("first Execute errors = %#v", first.Errors)
	}
	assertJSONEqual(t, first.Data, map[string]any{"left": "Ada", "right": "Ada"})
	second := plan.ExecuteWith(ctx, options)
	if len(second.Errors) != 0 {
		t.Fatalf("second Execute errors = %#v", second.Errors)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1 after request and cross-request deduplication", calls.Load())
	}
	bob := runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "bob", Tenant: "tenant-1", AuthorizationRevision: "policy-r1",
	})
	bobOutcome := plan.ExecuteWith(bob, options)
	if len(bobOutcome.Errors) != 0 {
		t.Fatalf("other-principal Execute errors = %#v", bobOutcome.Errors)
	}
	contextOptions := options
	contextOptions.Cache.Context = "locale=lv"
	contextOutcome := plan.ExecuteWith(ctx, contextOptions)
	if len(contextOutcome.Errors) != 0 {
		t.Fatalf("other-context Execute errors = %#v", contextOutcome.Errors)
	}
	if calls.Load() != 3 {
		t.Fatalf("partitioned handler calls = %d, want 3", calls.Load())
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.keys) == 0 {
		t.Fatal("external cache received no keys")
	}
	for _, key := range cache.keys {
		if key.Operation != "Q" || key.OperationKind != protocol.Query || key.SchemaRevision != "schema-r1" || key.SchemaDigest == "" || key.VariablesDigest == "" ||
			key.AuthorizationIdentity == "" || (key.Context != "locale=en" && key.Context != "locale=lv") || key.InputDigest == "" {
			t.Fatalf("incomplete cache key = %#v", key)
		}
	}
}

func TestBatchMissingAndOutOfOrderResultsKeepItemPaths(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		return []map[string]any{{"name": "Ada"}, {"name": "Grace"}, {"name": "Lin"}}, nil
	}))
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	registerComposition(t, registry, runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{Key: func(source map[string]any) (string, error) {
		return source["name"].(string), nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
		return []runtime.BatchResult[string]{
			{Index: inputs[2].Index, Value: "Lin"},
			{Index: inputs[0].Index, Value: "Ada"},
		}
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	assertJSONEqual(t, outcome.Data, map[string]any{"users": map[string]any{"items": []any{
		map[string]any{"name": "Ada"}, nil, map[string]any{"name": "Lin"},
	}}})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeHandlerFailed ||
		!slices.Equal(outcome.Errors[0].Path, []any{"users", "items", 1, "name"}) ||
		!errors.Is(outcome.Errors[0].Unwrap(), runtime.ErrBatchResultMissing) {
		t.Fatalf("batch errors = %#v", outcome.Errors)
	}
}

func TestBatchDispatchTerminatesAtConcurrencyWidthOne(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		return []map[string]any{{"name": "Ada"}, {"name": "Grace"}}, nil
	}))
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var calls atomic.Int64
	registerComposition(t, registry, runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{Key: func(source map[string]any) (string, error) {
		return source["name"].(string), nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
		calls.Add(1)
		return []runtime.BatchResult[string]{{Index: inputs[0].Index, Value: "Ada"}, {Index: inputs[1].Index, Value: "Grace"}}
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxConcurrency: 1}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	done := make(chan runtime.Outcome, 1)
	go func() { done <- plan.Execute(context.Background()) }()
	select {
	case outcome := <-done:
		if len(outcome.Errors) != 0 || calls.Load() != 1 {
			t.Fatalf("width-one outcome = %#v calls=%d", outcome, calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("width-one batch dispatch deadlocked")
	}
}

func TestCancelledRequestCacheWaitersTerminateWithoutExtraHandlers(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var calls atomic.Int64
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(ctx context.Context, _ schema.InputValue) (string, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$var":"id"}}}},{"$call":{"name":"lookupName","as":"right","args":{"id":{"$var":"id"}}}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
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
		if !hasExecutionCode(outcome.Errors, runtime.CodeCancelled) || calls.Load() != 1 {
			t.Fatalf("cancelled waiter outcome = %#v calls=%d", outcome, calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled cache waiter did not terminate")
	}
}

func TestNestedBatchLoadersTerminateAndPreserveOuterOrder(t *testing.T) {
	t.Parallel()
	types := freezeCompositionTypes(t, []schema.TypeDescriptor{
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String), Required: true},
		}},
		{ID: "Users", Kind: schema.ListType, Output: true, Element: "User"},
		{ID: "Group", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name":  {Type: schema.TypeID(schema.String), Required: true},
			"users": {Type: "Users", Required: true},
		}},
		{ID: "Groups", Kind: schema.ListType, Output: true, Element: "Group"},
	}...)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "groups", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Groups", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		return []map[string]any{
			{"name": "pioneers", "users": []map[string]any{{"name": "Ada"}, {"name": "Grace"}}},
			{"name": "systems", "users": []map[string]any{{"name": "Lin"}}},
		}, nil
	}))
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var groupLoads atomic.Int64
	registerComposition(t, registry, runtime.BindBatchField[map[string]any, []any](runtime.Descriptor{
		Name: "users", Scope: runtime.ObjectScope, Owner: "Group", Member: runtime.FieldMember,
		Output: "Users", Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{Key: func(source map[string]any) (string, error) {
		return source["name"].(string), nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[[]any] {
		groupLoads.Add(1)
		results := make([]runtime.BatchResult[[]any], 0, len(inputs))
		for index := len(inputs) - 1; index >= 0; index-- {
			results = append(results, runtime.BatchResult[[]any]{
				Index: inputs[index].Index, Value: inputs[index].Value["users"].([]any),
			})
		}
		return results
	}))
	var userLoads atomic.Int64
	registerComposition(t, registry, runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{Key: func(source map[string]any) (string, error) {
		return source["name"].(string), nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
		userLoads.Add(1)
		results := make([]runtime.BatchResult[string], len(inputs))
		for index, input := range inputs {
			results[index] = runtime.BatchResult[string]{Index: input.Index, Value: input.Value["name"].(string)}
		}
		return results
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"groups","select":[{"$map":{"as":"groups","select":[{"$field":{"name":"users","select":[{"$map":{"as":"members","select":[{"$field":{"name":"name"}}]}}]}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"groups": map[string]any{"groups": []any{
		map[string]any{"users": map[string]any{"members": []any{map[string]any{"name": "Ada"}, map[string]any{"name": "Grace"}}}},
		map[string]any{"users": map[string]any{"members": []any{map[string]any{"name": "Lin"}}}},
	}}})
	if groupLoads.Load() != 1 || userLoads.Load() != 2 {
		t.Fatalf("nested batch calls = groups %d users %d, want 1 and 2", groupLoads.Load(), userLoads.Load())
	}
}

func TestMutationWriteInvalidatesRequestAndApplicationCachesBeforeFollowingRead(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	var state struct {
		sync.Mutex
		name string
	}
	state.name = "before"
	var reads atomic.Int64
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "readName", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, schema.InputValue) (string, error) {
		reads.Add(1)
		state.Lock()
		defer state.Unlock()
		return state.name, nil
	}))
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "setName", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: "SetNameInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.WriteEffect),
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
	}))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{Allowed: true, CacheScope: runtime.AuthorizationCachePrincipal,
				AuthorizationRevision: request.Principal.AuthorizationRevision}, nil
		}),
	}); err != nil {
		t.Fatalf("configure authorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"M","kind":"mutation","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"readName","as":"before","args":{"id":{"$var":"id"}}}},{"$call":{"name":"setName","as":"write","args":{"value":{"$literal":"after"}}}},{"$call":{"name":"readName","as":"after","args":{"id":{"$var":"id"}}}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cache := &recordingResultCache{}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "alice", AuthorizationRevision: "policy-r1"})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Cache: runtime.CacheOptions{SchemaRevision: "schema-r1", External: cache}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"before": "before", "write": "after", "after": "after"})
	if reads.Load() != 2 {
		t.Fatalf("read calls = %d, want 2 around write barrier", reads.Load())
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.invalidations) != 1 || cache.invalidations[0].Handler != "root:setName" {
		t.Fatalf("cache invalidations = %#v", cache.invalidations)
	}
}

func TestReadMetadataMustExplicitlyPermitMemoization(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Cacheable = false
	var calls atomic.Int64
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: metadata,
	}, func(context.Context, schema.InputValue) (string, error) {
		calls.Add(1)
		return "Ada", nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$var":"id"}}}},{"$call":{"name":"lookupName","as":"right","args":{"id":{"$var":"id"}}}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || calls.Load() != 2 {
		t.Fatalf("non-cacheable outcome = %#v calls=%d", outcome, calls.Load())
	}
}

func TestRequestCacheControlsErrorNullAndCrossRequestPolicies(t *testing.T) {
	t.Parallel()
	t.Run("disabled", func(t *testing.T) {
		plan, calls := cachePolicyPlan(t, false, false)
		outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Cache: runtime.CacheOptions{DisableRequest: true}})
		if len(outcome.Errors) != 0 || calls.Load() != 2 {
			t.Fatalf("disabled cache outcome = %#v calls=%d", outcome, calls.Load())
		}
	})
	t.Run("errors-not-stored-by-default", func(t *testing.T) {
		plan, calls := cachePolicyPlan(t, true, false)
		outcome := plan.Execute(context.Background())
		if len(outcome.Errors) != 2 || calls.Load() != 2 {
			t.Fatalf("default error cache outcome = %#v calls=%d", outcome, calls.Load())
		}
	})
	t.Run("errors-explicitly-stored", func(t *testing.T) {
		plan, calls := cachePolicyPlan(t, true, false)
		outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Cache: runtime.CacheOptions{CacheErrors: true}})
		if len(outcome.Errors) != 2 || calls.Load() != 1 {
			t.Fatalf("enabled error cache outcome = %#v calls=%d", outcome, calls.Load())
		}
	})
	t.Run("nulls-not-stored-by-default", func(t *testing.T) {
		plan, calls := cachePolicyPlan(t, false, true)
		outcome := plan.Execute(context.Background())
		if len(outcome.Errors) != 0 || calls.Load() != 2 {
			t.Fatalf("default null cache outcome = %#v calls=%d", outcome, calls.Load())
		}
	})
	t.Run("nulls-explicitly-stored", func(t *testing.T) {
		plan, calls := cachePolicyPlan(t, false, true)
		outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Cache: runtime.CacheOptions{CacheNulls: true}})
		if len(outcome.Errors) != 0 || calls.Load() != 1 {
			t.Fatalf("enabled null cache outcome = %#v calls=%d", outcome, calls.Load())
		}
	})
	t.Run("no-core-cross-request-storage", func(t *testing.T) {
		plan, calls := cachePolicyPlan(t, false, false)
		first := plan.Execute(context.Background())
		second := plan.Execute(context.Background())
		if len(first.Errors) != 0 || len(second.Errors) != 0 || calls.Load() != 2 {
			t.Fatalf("cross-request outcome first=%#v second=%#v calls=%d", first, second, calls.Load())
		}
	})
}

func cachePolicyPlan(t testing.TB, fail, nullable bool) (*runtime.Plan, *atomic.Int64) {
	t.Helper()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int64{}
	descriptor := runtime.Descriptor{
		Name: "value", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), OutputNullable: nullable,
		Metadata: completeMetadata(runtime.ReadEffect),
	}
	if nullable {
		registerComposition(t, registry, runtime.Bind[schema.InputValue, *string](descriptor, func(context.Context, schema.InputValue) (*string, error) {
			calls.Add(1)
			return nil, nil
		}))
	} else {
		registerComposition(t, registry, runtime.Bind[schema.InputValue, string](descriptor, func(context.Context, schema.InputValue) (string, error) {
			calls.Add(1)
			if fail {
				return "", errors.New("backend unavailable")
			}
			return "ok", nil
		}))
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"value","as":"first","args":{"id":{"$var":"id"}}}},{"$call":{"name":"value","as":"second","args":{"id":{"$var":"id"}}}}]}]}}`), protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan, calls
}

func TestBatchRuntimeCostIsChargedBeforeDispatch(t *testing.T) {
	t.Parallel()
	plan, calls := batchBenchmarkPlan(t, 2, true)
	outcome := plan.Execute(context.Background())
	if calls.Load() != 0 || !hasExecutionCode(outcome.Errors, runtime.CodeResourceExhausted) {
		t.Fatalf("cost-gated batch outcome = %#v calls=%d", outcome, calls.Load())
	}
}

func TestParallelBatchCallsShareOneDeterministicDispatch(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var invocations atomic.Int64
	registerComposition(t, registry, runtime.BindBatch[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[schema.InputValue]{Key: func(input schema.InputValue) (string, error) {
		members, _ := input.Object()
		identifier, _ := members["id"].Scalar()
		encoded, _ := identifier.MarshalJSON()
		return string(encoded), nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[schema.InputValue]) []runtime.BatchResult[string] {
		invocations.Add(1)
		results := make([]runtime.BatchResult[string], len(inputs))
		for index, input := range inputs {
			members, _ := input.Value.Object()
			identifier, _ := members["id"].Scalar()
			encoded, _ := identifier.MarshalJSON()
			results[len(inputs)-1-index] = runtime.BatchResult[string]{Index: input.Index, Value: string(encoded)}
		}
		return results
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$literal":"u-1"}}}},{"$call":{"name":"lookupName","as":"right","args":{"id":{"$literal":"u-2"}}}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"left": `"u-1"`, "right": `"u-2"`})
	if invocations.Load() != 1 {
		t.Fatalf("parallel batch invocations = %d, want 1", invocations.Load())
	}
}

func TestAuthorizationNoStoreBypassesRequestAndApplicationCaches(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(compositionTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, schema.InputValue) (string, error) {
		calls.Add(1)
		return "Ada", nil
	}))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{Allowed: true, CacheScope: runtime.AuthorizationCacheNoStore}, nil
		}),
	}); err != nil {
		t.Fatalf("configure authorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"left","args":{"id":{"$literal":"u-1"}}}},{"$call":{"name":"lookupName","as":"right","args":{"id":{"$literal":"u-1"}}}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cache := &recordingResultCache{}
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Cache: runtime.CacheOptions{
		SchemaRevision: "schema-r1", External: cache,
	}})
	if len(outcome.Errors) != 0 || calls.Load() != 2 {
		t.Fatalf("no-store outcome = %#v calls=%d", outcome, calls.Load())
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.keys) != 0 {
		t.Fatalf("no-store invoked application cache with %d keys", len(cache.keys))
	}
}

func TestBatchMaximumChunksOneParallelWindowDeterministically(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(compositionTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var mu sync.Mutex
	var sizes []int
	registerComposition(t, registry, runtime.BindBatch[schema.InputValue, string](runtime.Descriptor{
		Name: "lookupName", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[schema.InputValue]{MaxSize: 2, Key: func(input schema.InputValue) (string, error) {
		members, _ := input.Object()
		identifier, _ := members["id"].Scalar()
		encoded, _ := identifier.MarshalJSON()
		return string(encoded), nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[schema.InputValue]) []runtime.BatchResult[string] {
		mu.Lock()
		sizes = append(sizes, len(inputs))
		mu.Unlock()
		results := make([]runtime.BatchResult[string], len(inputs))
		for index, input := range inputs {
			results[index] = runtime.BatchResult[string]{Index: input.Index, Value: "ok"}
		}
		return results
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"lookupName","as":"one","args":{"id":{"$literal":"u-1"}}}},{"$call":{"name":"lookupName","as":"two","args":{"id":{"$literal":"u-2"}}}},{"$call":{"name":"lookupName","as":"three","args":{"id":{"$literal":"u-3"}}}},{"$call":{"name":"lookupName","as":"four","args":{"id":{"$literal":"u-4"}}}},{"$call":{"name":"lookupName","as":"five","args":{"id":{"$literal":"u-5"}}}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Fatalf("batch sizes = %v, want [2 2 1]", sizes)
	}
}

func TestBatchWindowDoesNotCrossMappedWriteBarrier(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		return []map[string]any{{"name": "Ada", "mutateName": ""}, {"name": "Grace", "mutateName": ""}}, nil
	}))
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Batching = runtime.BatchEligible
	var mu sync.Mutex
	var events []string
	registerComposition(t, registry, runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}, runtime.BatchOptions[map[string]any]{Key: func(source map[string]any) (string, error) {
		return source["name"].(string), nil
	}}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
		mu.Lock()
		defer mu.Unlock()
		results := make([]runtime.BatchResult[string], len(inputs))
		for index, input := range inputs {
			name := input.Value["name"].(string)
			events = append(events, "read:"+name)
			results[index] = runtime.BatchResult[string]{Index: input.Index, Value: name}
		}
		return results
	}))
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "mutateName", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.WriteEffect),
	}, func(_ context.Context, source map[string]any) (string, error) {
		name := source["name"].(string)
		mu.Lock()
		events = append(events, "write:"+name)
		mu.Unlock()
		return name, nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}},{"$field":{"name":"mutateName"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"read:Ada", "write:Ada", "read:Grace", "write:Grace"}
	if !slices.Equal(events, want) {
		t.Fatalf("barrier events = %v, want %v", events, want)
	}
}

func BenchmarkNestedCollectionBatching(b *testing.B) {
	for _, batched := range []bool{false, true} {
		name := "unbatched"
		if batched {
			name = "batched"
		}
		b.Run(name, func(b *testing.B) {
			plan, calls := batchBenchmarkPlan(b, 0, batched)
			b.ResetTimer()
			for range b.N {
				outcome := plan.Execute(context.Background())
				if len(outcome.Errors) != 0 {
					b.Fatalf("Execute errors = %#v", outcome.Errors)
				}
			}
			b.ReportMetric(float64(calls.Load())/float64(b.N), "handler_calls/op")
		})
	}
}

func batchBenchmarkPlan(t testing.TB, maxRuntimeWork uint64, batching ...bool) (*runtime.Plan, *atomic.Int64) {
	t.Helper()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	users := make([]map[string]any, 64)
	for index := range users {
		users[index] = map[string]any{"name": fmt.Sprintf("user-%03d", index)}
	}
	registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) { return users, nil }))
	calls := &atomic.Int64{}
	metadata := completeMetadata(runtime.ReadEffect)
	if len(batching) != 0 && batching[0] {
		metadata.Batching = runtime.BatchEligible
		registerComposition(t, registry, runtime.BindBatchField[map[string]any, string](runtime.Descriptor{
			Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
			Output: schema.TypeID(schema.String), Metadata: metadata,
		}, runtime.BatchOptions[map[string]any]{Key: func(source map[string]any) (string, error) {
			return source["name"].(string), nil
		}}, func(_ context.Context, inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
			calls.Add(1)
			results := make([]runtime.BatchResult[string], len(inputs))
			for index, input := range inputs {
				results[index] = runtime.BatchResult[string]{Index: input.Index, Value: input.Value["name"].(string)}
			}
			return results
		}))
	} else {
		registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
			Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
			Output: schema.TypeID(schema.String), Metadata: metadata,
		}, func(_ context.Context, source map[string]any) (string, error) {
			calls.Add(1)
			return source["name"].(string), nil
		}))
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	requestJSON := `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"items","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`
	request, err := protocol.DecodeRequest([]byte(requestJSON), protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxRuntimeWork: maxRuntimeWork}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	return plan, calls
}
