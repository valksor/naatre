package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestExecuteDistinguishesNullMissingSkippedAndUnavailableInputs(t *testing.T) {
	t.Parallel()

	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	var consumed atomic.Int64
	registerComposition(t, registry, runtime.BindInvocation[*string](runtime.Descriptor{
		Name: "nullable", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), OutputNullable: true,
		Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (*string, error) { return nil, nil }))
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "load", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) { return "loaded", nil }))
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "fail", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) { return "", errors.New("private failure") }))
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "consume", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LabelInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, schema.InputValue) (string, error) {
		consumed.Add(1)
		return "consumed", nil
	}))
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "consumeOptional", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "OptionalInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, input schema.InputValue) (string, error) {
		consumed.Add(1)
		members, _ := input.Object()
		value, _ := members["value"].Scalar()
		encoded, _ := value.MarshalJSON()
		var result string
		_ = json.Unmarshal(encoded, &result)
		return result, nil
	}))
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "adapterConsume", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LabelInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		consumed.Add(1)
		return "bad", nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}

	tests := []struct {
		name      string
		document  string
		wantCodes []string
		wantCalls int64
		wantData  map[string]any
	}{
		{
			name:      "null",
			document:  `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"nullable","bind":"value"}},{"$call":{"name":"consume","args":{"value":{"$result":"value"}}}}]}]}}`,
			wantCodes: []string{"RESULT_NULL"},
		},
		{
			name:      "missing required",
			document:  `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"value","type":"String"}],"select":[{"$call":{"name":"consume","args":{"value":{"$var":"value"}}}}]}]}}`,
			wantCodes: []string{"RESULT_MISSING"},
		},
		{
			name:      "missing required invocation adapter",
			document:  `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"value","type":"String"}],"select":[{"$call":{"name":"adapterConsume","args":{"value":{"$var":"value"}}}}]}]}}`,
			wantCodes: []string{"RESULT_MISSING"},
		},
		{
			name:      "missing optional",
			document:  `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"value","type":"String"}],"select":[{"$call":{"name":"consumeOptional","args":{"value":{"$var":"value"}}}}]}]}}`,
			wantCalls: 1,
			wantData:  map[string]any{"consumeOptional": "default"},
		},
		{
			name:      "skipped",
			document:  `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"load","bind":"value","directives":[{"name":"skip","arguments":{"if":{"$literal":true}}}]}},{"$call":{"name":"consume","args":{"value":{"$result":"value"}}}}]}]}}`,
			wantCodes: []string{"RESULT_SKIPPED"},
		},
		{
			name:      "unavailable",
			document:  `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"fail","bind":"value"}},{"$call":{"name":"consume","args":{"value":{"$result":"value"}}}}]}]}}`,
			wantCodes: []string{"RESULT_UNAVAILABLE", "HANDLER_FAILED"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := consumed.Load()
			plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, tt.document))
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			outcome := plan.Execute(context.Background())
			codes := make([]string, len(outcome.Errors))
			for index, failure := range outcome.Errors {
				codes[index] = failure.Code
			}
			if !slices.Equal(codes, tt.wantCodes) {
				t.Fatalf("error codes = %v, want %v", codes, tt.wantCodes)
			}
			if calls := consumed.Load() - before; calls != tt.wantCalls {
				t.Fatalf("consumer calls = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantData != nil {
				assertJSONEqual(t, outcome.Data, tt.wantData)
			}
		})
	}
}

func TestExecuteDoesNotInvokeObjectMemberOnNullCurrentValue(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	registerComposition(t, registry, runtime.BindInvocation[*map[string]any](runtime.Descriptor{
		Name: "nullableUser", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "User", OutputNullable: true,
		Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (*map[string]any, error) { return nil, nil }))
	var memberCalls atomic.Int64
	registerComposition(t, registry, runtime.BindCall[map[string]any, *schema.InputValue, string](runtime.Descriptor{
		Name: "nullableLabel", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.CallMember,
		Input: "LabelInput", InputNullable: true, Output: schema.TypeID(schema.String),
		Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, map[string]any, *schema.InputValue) (string, error) {
		memberCalls.Add(1)
		return "bad", nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"nullableUser","select":[{"$call":{"name":"nullableLabel","args":{"value":{"$literal":"x"}}}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != "RESULT_NULL" || !slices.Equal(outcome.Errors[0].Path, []any{"nullableUser", "nullableLabel"}) {
		t.Fatalf("outcome errors = %#v", outcome.Errors)
	}
	if memberCalls.Load() != 0 {
		t.Fatalf("member handler ran %d times", memberCalls.Load())
	}
}

func TestExecuteFragmentBindingsRemainInSpreadSequentialScope(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "load", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) { return "fragment-value", nil }))
	registerComposition(t, registry, runtime.Bind[schema.InputValue, string](runtime.Descriptor{
		Name: "consume", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LabelInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, input schema.InputValue) (string, error) {
		members, _ := input.Object()
		value, _ := members["value"].Scalar()
		encoded, _ := value.MarshalJSON()
		var result string
		_ = json.Unmarshal(encoded, &result)
		return result, nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"Load"}},{"$call":{"name":"consume","args":{"value":{"$result":"fromFragment"}}}}]}],"fragments":[{"name":"Load","select":[{"$call":{"name":"load","bind":"fromFragment"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"load": "fragment-value", "consume": "fragment-value"})
}

func TestExecutePreservesNullableCollectionItemsWithoutMemberErrors(t *testing.T) {
	t.Parallel()
	types := freezeCompositionTypes(t, []schema.TypeDescriptor{
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String), Required: true},
		}},
		{ID: "NullableUsers", Kind: schema.ListType, Output: true, Element: "User", ElementNullable: true},
	}...)
	registry := runtime.NewRegistry(types)
	registerComposition(t, registry, runtime.BindInvocation[[]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "NullableUsers", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]any, error) {
		return []any{nil, map[string]any{"name": "Ada"}}, nil
	}))
	var memberCalls atomic.Int64
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (string, error) {
		memberCalls.Add(1)
		return source["name"].(string), nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$map":{"as":"mapped","select":[{"$field":{"name":"name"}}]}},{"$slice":{"as":"window","start":0,"end":2,"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"users": map[string]any{
		"mapped": []any{nil, map[string]any{"name": "Ada"}},
		"window": []any{nil, map[string]any{"name": "Ada"}},
	}})
	if memberCalls.Load() != 2 {
		t.Fatalf("member handler calls = %d, want 2", memberCalls.Load())
	}
}

func TestExecuteCancellationStopsCollectionItemAdmission(t *testing.T) {
	t.Parallel()
	for _, selection := range []string{
		`{"$map":{"as":"items","select":[{"$field":{"name":"cancel"}}]}}`,
		`{"$slice":{"as":"items","start":0,"end":3,"select":[{"$field":{"name":"cancel"}}]}}`,
	} {
		selection := selection
		t.Run(selection, func(t *testing.T) {
			types := freezeCompositionTypes(t, []schema.TypeDescriptor{
				{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
					"name":   {Type: schema.TypeID(schema.String), Required: true},
					"cancel": {Type: schema.TypeID(schema.String)},
				}},
				{ID: "Users", Kind: schema.ListType, Output: true, Element: "User"},
			}...)
			registry := runtime.NewRegistry(types)
			registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
				Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
				Input: schema.TypeID(schema.String), Output: "Users", Metadata: completeMetadata(runtime.ReadEffect),
			}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
				return []map[string]any{{"name": "a"}, {"name": "b"}, {"name": "c"}}, nil
			}))
			var calls atomic.Int64
			registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
				Name: "cancel", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
				Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
			}, func(context.Context, map[string]any) (string, error) {
				calls.Add(1)
				return "", context.Canceled
			}))
			snapshot, err := registry.Freeze()
			if err != nil {
				t.Fatalf("freeze registry: %v", err)
			}
			request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[`+selection+`]}}]}]}}`)
			plan, err := runtime.Prepare(snapshot, request)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			outcome := plan.Execute(context.Background())
			if len(outcome.Errors) != 1 || outcome.Errors[0].Code != "CANCELLED" {
				t.Fatalf("Execute errors = %#v", outcome.Errors)
			}
			if calls.Load() != 1 {
				t.Fatalf("cancelled collection member calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestExecuteBatchEligibleCallsRetainCompletionBarrier(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	batchMetadata := completeMetadata(runtime.ReadEffect)
	batchMetadata.Batching = runtime.BatchEligible
	registerComposition(t, registry, runtime.BindInvocation[map[string]any](runtime.Descriptor{
		Name: "broken", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "User", Metadata: batchMetadata,
	}, func(context.Context, runtime.Invocation) (map[string]any, error) {
		return map[string]any{"name": int32(7), "mutateName": "ok"}, nil
	}))
	var invalidFieldCalls atomic.Int64
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: batchMetadata,
	}, func(_ context.Context, source map[string]any) (string, error) {
		invalidFieldCalls.Add(1)
		return source["name"].(string), nil
	}))
	var laterCalls atomic.Int64
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "later", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: batchMetadata,
	}, func(context.Context, runtime.Invocation) (string, error) {
		laterCalls.Add(1)
		return "ok", nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"broken","select":[{"$field":{"name":"name"}}]}},{"$call":{"name":"later"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for index, node := range plan.Description().Nodes {
		if !slices.Contains(node.Barriers, runtime.CompletionBarrier) {
			t.Fatalf("batch-eligible node %d barriers = %#v", index, node.Barriers)
		}
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != "OUTPUT_COMPLETION" || !slices.Equal(outcome.Errors[0].Path, []any{"broken", "name"}) {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"broken": map[string]any{}, "later": "ok"})
	if invalidFieldCalls.Load() != 0 {
		t.Fatalf("invalid completed field handler calls = %d, want 0", invalidFieldCalls.Load())
	}
	if laterCalls.Load() != 1 {
		t.Fatalf("later batch-eligible handler calls = %d, want 1", laterCalls.Load())
	}
}

// TYPE-206 forbids representing a failed output by deleting unrelated parent
// data. A completion issue nested below a member must leave that member
// available with its remaining valid data.
// TYPE-206 forbids representing a failed output by deleting unrelated parent
// data. A completion issue nested below a member leaves that member available
// with its remaining valid data, while the member that actually failed stays
// unavailable however deep the selection reaches it.
func TestExecuteNestedCompletionFailureKeepsSiblingData(t *testing.T) {
	t.Parallel()
	types := freezeCompositionTypes(t, []schema.TypeDescriptor{
		{ID: "Profile", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"bio":      {Type: schema.TypeID(schema.String)},
			"nickname": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Person", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"profile": {Type: "Profile"},
			"name":    {Type: schema.TypeID(schema.String)},
		}},
	}...)
	registry := runtime.NewRegistry(types)
	registerComposition(t, registry, runtime.BindInvocation[map[string]any](runtime.Descriptor{
		Name: "person", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Person", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (map[string]any, error) {
		return map[string]any{"name": "Ada", "profile": map[string]any{"bio": 7, "nickname": "ada"}}, nil
	}))
	registerComposition(t, registry, runtime.BindField[map[string]any, map[string]any](runtime.Descriptor{
		Name: "profile", Scope: runtime.ObjectScope, Owner: "Person", Member: runtime.FieldMember,
		Output: "Profile", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (map[string]any, error) {
		value, _ := source["profile"].(map[string]any)
		return value, nil
	}))
	var bioCalls atomic.Int64
	registerSourceStringField(t, registry, "Person", "name", nil)
	registerSourceStringField(t, registry, "Profile", "nickname", nil)
	registerSourceStringField(t, registry, "Profile", "bio", &bioCalls)
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	for _, selected := range []struct{ name, selection string }{
		{"failed member unselected", `{"$field":{"name":"nickname"}}`},
		{"failed member selected", `{"$field":{"name":"bio"}},{"$field":{"name":"nickname"}}`},
	} {
		t.Run(selected.name, func(t *testing.T) {
			bioCalls.Store(0)
			request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"person","select":[{"$field":{"name":"name"}},{"$field":{"name":"profile","select":[`+selected.selection+`]}}]}}]}]}}`)
			plan, err := runtime.Prepare(snapshot, request)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			outcome := plan.Execute(context.Background())
			// Selecting the failed member adds no second error and no handler
			// call, and never costs its valid sibling.
			assertSingleCompletionError(t, outcome, []any{"person", "profile", "bio"})
			if bioCalls.Load() != 0 {
				t.Fatalf("failed nested member handler calls = %d, want 0", bioCalls.Load())
			}
			assertJSONEqual(t, outcome.Data, map[string]any{"person": map[string]any{
				"name": "Ada", "profile": map[string]any{"nickname": "ada"},
			}})
		})
	}
}

// A member whose own completion fails stays unavailable, and its sibling
// members survive.
func TestExecuteDirectCompletionFailureKeepsSiblingMembers(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(compositionTypes(t))
	registerComposition(t, registry, runtime.BindInvocation[map[string]any](runtime.Descriptor{
		Name: "broken", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "User", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (map[string]any, error) {
		return map[string]any{"name": int32(7), "mutateName": "ok"}, nil
	}))
	var nameCalls atomic.Int64
	registerSourceStringField(t, registry, "User", "name", &nameCalls)
	registerSourceStringField(t, registry, "User", "mutateName", nil)
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"broken","select":[{"$field":{"name":"name"}},{"$field":{"name":"mutateName"}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	assertSingleCompletionError(t, outcome, []any{"broken", "name"})
	if nameCalls.Load() != 0 {
		t.Fatalf("invalid member handler calls = %d, want 0", nameCalls.Load())
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"broken": map[string]any{"mutateName": "ok"}})
}

// registerSourceStringField registers an object field that reads its own name
// from the completed source. The read asserts rather than checks, so reaching
// this handler for a member whose completion failed surfaces as a contained
// panic instead of a silent empty string. A non-nil calls counts invocations.
func registerSourceStringField(t testing.TB, registry *runtime.Registry, owner schema.TypeID, name string, calls *atomic.Int64) {
	t.Helper()
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: name, Scope: runtime.ObjectScope, Owner: owner, Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return source[name].(string), nil
	}))
}

// assertSingleCompletionError requires exactly one output-completion error at
// path, which fails if a blocked member also produced a handler error.
func assertSingleCompletionError(t testing.TB, outcome runtime.Outcome, path []any) {
	t.Helper()
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != "OUTPUT_COMPLETION" ||
		!slices.Equal(outcome.Errors[0].Path, path) {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
}

func TestExecuteRejectsOverflowingStaleCursorWithoutPanicking(t *testing.T) {
	t.Parallel()
	types := freezeCompositionTypes(t, schema.TypeDescriptor{
		ID: "Users", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.ID),
	})
	registry := runtime.NewRegistry(types)
	var calls atomic.Int64
	registerComposition(t, registry, runtime.BindInvocation[[]string](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]string, error) {
		calls.Add(1)
		return []string{"u-1"}, nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodePortableExecutionRequest(t,
		json.RawMessage(`{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"items","first":1,"after":{"$literal":"9223372036854775807"}}}]}}]}]}`),
		nil, []string{runtime.CollectionPageCapability})
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != "INVALID_CURSOR" {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	if calls.Load() != 0 {
		t.Fatalf("collection handler calls = %d, want 0", calls.Load())
	}
}
