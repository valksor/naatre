package runtime_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestExecuteComposesTypedCallsFieldsAliasesAndNesting(t *testing.T) {
	t.Parallel()

	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	registerComposition(t, registry, runtime.Bind[schema.InputValue, map[string]any](runtime.Descriptor{
		Name: "lookup", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: "User", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, input schema.InputValue) (map[string]any, error) {
		members, ok := input.Object()
		if !ok {
			t.Fatal("lookup input is not an object")
		}
		identifier, ok := members["id"].Scalar()
		if !ok {
			t.Fatal("lookup id is not a scalar")
		}
		encoded, err := identifier.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal lookup id: %v", err)
		}
		if string(encoded) != `"u-1"` {
			t.Fatalf("lookup id = %s, want u-1", encoded)
		}
		return map[string]any{"name": "Ada"}, nil
	}))
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (string, error) {
		return source["name"].(string), nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"lookup","args":{"id":{"$var":"id"}},"select":[{"$field":{"name":"name","as":"display"}},{"$nest":{"as":"profile","select":[{"$field":{"name":"name","as":"nested"}}]}},{"$unnest":{"select":[{"$field":{"name":"name","as":"flat"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	want := map[string]any{"lookup": map[string]any{
		"display": "Ada",
		"profile": map[string]any{"nested": "Ada"},
		"flat":    "Ada",
	}}
	assertJSONEqual(t, outcome.Data, want)
}

func TestExecuteDeepMutationGoldenCompletesWriteBeforeFollowingRead(t *testing.T) {
	t.Parallel()

	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	var state struct {
		sync.Mutex
		value string
	}
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
		state.value = name
		state.Unlock()
		return name, nil
	}))
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "readName", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		state.Lock()
		defer state.Unlock()
		return state.value, nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"setName","args":{"value":{"$literal":"Grace"}}}},{"$call":{"name":"readName"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"setName": "Grace", "readName": "Grace"})
	assertGoldenData(t, "deep_mutation.golden.json", outcome.Data)
}

func TestExecuteDeepQueryGoldenShapesCollectionOperationsWithoutImplicitMapping(t *testing.T) {
	t.Parallel()

	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	var rootCalls atomic.Int64
	var fieldCalls atomic.Int64
	usersMetadata := completeMetadata(runtime.ReadEffect)
	usersMetadata.Collection = secureCollectionMetadata(t, 3, 0)
	registerComposition(t, registry, runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: usersMetadata,
	}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		rootCalls.Add(1)
		return []map[string]any{{"name": "Ada"}, {"name": "Grace"}, {"name": "Lin"}}, nil
	}))
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (string, error) {
		fieldCalls.Add(1)
		return source["name"].(string), nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$meta":{"name":"count","as":"total"}},{"$map":{"as":"items","select":[{"$field":{"name":"name","as":"label"}}]}},{"$index":{"as":"last","at":2,"select":[{"$field":{"name":"name"}}]}},{"$slice":{"as":"tail","start":1,"end":99,"select":[{"$field":{"name":"name"}}]}},{"$page":{"as":"firstTwo","first":2,"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`), protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	normalizeUserPageCursors(t, outcome.Data, "firstTwo", "")
	assertJSONEqual(t, outcome.Data, map[string]any{"users": map[string]any{
		"total": int32(3),
		"items": []any{map[string]any{"label": "Ada"}, map[string]any{"label": "Grace"}, map[string]any{"label": "Lin"}},
		"last":  map[string]any{"name": "Lin"},
		"tail":  []any{map[string]any{"name": "Grace"}, map[string]any{"name": "Lin"}},
		"firstTwo": map[string]any{
			"items": []any{map[string]any{"name": "Ada"}, map[string]any{"name": "Grace"}},
			"pageInfo": runtime.PageInfo{
				HasNextPage: true,
			},
		},
	}})
	assertGoldenData(t, "deep_query.golden.json", outcome.Data)
	if rootCalls.Load() != 1 || fieldCalls.Load() != 8 {
		t.Fatalf("handler calls = root %d field %d, want 1 and 8", rootCalls.Load(), fieldCalls.Load())
	}
}

func TestExecuteRunsPipelinesBindingsDirectivesAndConcreteFragments(t *testing.T) {
	t.Parallel()

	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	var skippedCalls atomic.Int64
	registerComposition(t, registry, runtime.BindInvocation[map[string]any](runtime.Descriptor{
		Name: "lookup", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: "User", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (map[string]any, error) {
		return map[string]any{"name": "Ada"}, nil
	}))
	registerComposition(t, registry, runtime.BindInvocation[schema.TaggedValue](runtime.Descriptor{
		Name: "actor", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Actor", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (schema.TaggedValue, error) {
		value, err := schema.Tag("User", map[string]any{"name": "Grace"})
		if err != nil {
			return schema.TaggedValue{}, err
		}
		return value, nil
	}))
	for _, owner := range []schema.TypeID{"User", "Admin"} {
		descriptor := runtime.Descriptor{Name: "name", Scope: runtime.ObjectScope, Owner: owner, Member: runtime.FieldMember,
			Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
		registerComposition(t, registry, runtime.BindField[map[string]any, string](descriptor, func(_ context.Context, source map[string]any) (string, error) {
			return source["name"].(string), nil
		}))
	}
	registerComposition(t, registry, runtime.BindCall[map[string]any, schema.InputValue, string](runtime.Descriptor{
		Name: "label", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.CallMember,
		Input: "LabelInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any, input schema.InputValue) (string, error) {
		members, _ := input.Object()
		value, _ := members["value"].Scalar()
		encoded, _ := value.MarshalJSON()
		var label string
		_ = json.Unmarshal(encoded, &label)
		return source["name"].(string) + ":" + label, nil
	}))
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "skipped", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		skippedCalls.Add(1)
		return "bad", nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$pipeline":{"as":"pipelineName","stages":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}}}},{"$field":{"name":"name"}},{"$current":{}}]}},{"$call":{"name":"lookup","as":"bound","args":{"id":{"$literal":"u-1"}},"select":[{"$field":{"name":"name","bind":"n"}},{"$call":{"name":"label","args":{"value":{"$result":"n"}}}}]}},{"$call":{"name":"actor","select":[{"$fragment":{"name":"UserFields"}},{"$fragment":{"name":"AdminFields"}}]}},{"$call":{"name":"skipped","as":"omitted","directives":[{"name":"include","arguments":{"if":{"$literal":false}}}]}}]}],"fragments":[{"name":"UserFields","on":"User","select":[{"$field":{"name":"name","as":"display"}}]},{"name":"AdminFields","on":"Admin","select":[{"$field":{"name":"name","as":"display"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{
		"pipelineName": "Ada",
		"bound":        map[string]any{"name": "Ada", "label": "Ada:Ada"},
		"actor":        map[string]any{"display": "Grace"},
	})
	if skippedCalls.Load() != 0 {
		t.Fatalf("statically skipped handler ran %d times", skippedCalls.Load())
	}
}

func TestExecuteContainsPanicsContinuesQueriesAndStopsMutations(t *testing.T) {
	t.Parallel()

	for _, kind := range []protocol.OperationKind{protocol.Query, protocol.Mutation} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			types := compositionTypes(t)
			registry := runtime.NewRegistry(types)
			var later atomic.Int64
			panicMetadata := completeMetadata(runtime.ReadEffect)
			if kind == protocol.Mutation {
				panicMetadata = completeMetadata(runtime.WriteEffect)
			}
			registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
				Name: "explode", Scope: runtime.RootScope, Kind: kind, Member: runtime.CallMember,
				Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: panicMetadata,
			}, func(context.Context, runtime.Invocation) (string, error) {
				panic("secret panic")
			}))
			registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
				Name: "later", Scope: runtime.RootScope, Kind: kind, Member: runtime.CallMember,
				Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
			}, func(context.Context, runtime.Invocation) (string, error) {
				later.Add(1)
				return "ok", nil
			}))
			snapshot, err := registry.Freeze()
			if err != nil {
				t.Fatalf("freeze registry: %v", err)
			}
			request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Run","kind":"`+string(kind)+`","select":[{"$call":{"name":"explode","as":"failed","directives":[{"name":"include","arguments":{"if":{"$literal":true}}}]}},{"$call":{"name":"later"}}]}]}}`)
			plan, err := runtime.Prepare(snapshot, request)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}

			outcome := plan.Execute(context.Background())
			if len(outcome.Errors) != 1 || outcome.Errors[0].Code != "INTERNAL" || !slices.Equal(outcome.Errors[0].Path, []any{"failed"}) {
				t.Fatalf("Execute errors = %#v", outcome.Errors)
			}
			if outcome.Errors[0].Unwrap() == nil || outcome.Errors[0].Message != "internal execution error" {
				t.Fatalf("panic containment = %#v", outcome.Errors[0])
			}
			if kind == protocol.Query {
				assertJSONEqual(t, outcome.Data, map[string]any{"later": "ok"})
				if later.Load() != 1 {
					t.Fatal("query did not continue after independent failure")
				}
			} else if later.Load() != 0 {
				t.Fatal("mutation continued after failure")
			}
		})
	}
}

func TestExecuteFreezesOutputsBeforeLaterSiblingAndRunsParallelBranches(t *testing.T) {
	t.Parallel()

	types := compositionTypes(t)
	registry := runtime.NewRegistry(types)
	shared := map[string]any{"name": "Ada"}
	registerComposition(t, registry, runtime.BindInvocation[map[string]any](runtime.Descriptor{
		Name: "shared", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "User", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (map[string]any, error) {
		return shared, nil
	}))
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (string, error) {
		return source["name"].(string), nil
	}))
	registerComposition(t, registry, runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "mutateName", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (string, error) {
		source["name"] = "corrupted"
		return "done", nil
	}))
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "mutate", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		shared["name"] = "changed"
		return "done", nil
	}))
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	parallelHandler := func(context.Context, runtime.Invocation) (string, error) {
		started <- struct{}{}
		<-release
		return "ok", nil
	}
	for _, name := range []string{"left", "right"} {
		registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
			Name: name, Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
		}, parallelHandler))
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"shared","directives":[{"name":"include","arguments":{"if":{"$literal":true}}}],"select":[{"$field":{"name":"mutateName"}},{"$field":{"name":"name"}}]}},{"$call":{"name":"mutate"}},{"$parallel":{"select":[{"$call":{"name":"left"}},{"$call":{"name":"right"}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	done := make(chan runtime.Outcome, 1)
	go func() { done <- plan.Execute(context.Background()) }()
	<-started
	<-started
	close(release)
	outcome := <-done
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{
		"shared": map[string]any{"mutateName": "done", "name": "Ada"},
		"mutate": "done",
		"left":   "ok",
		"right":  "ok",
	})
}

func compositionTypes(t testing.TB) schema.Snapshot {
	t.Helper()
	return freezeCompositionTypes(t, []schema.TypeDescriptor{
		{ID: "LookupInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"id": {Type: schema.TypeID(schema.ID), Required: true},
		}},
		{ID: "SetNameInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"value": {Type: schema.TypeID(schema.String), Required: true},
		}},
		{ID: "LabelInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"value": {Type: schema.TypeID(schema.String), Required: true},
		}},
		{ID: "OptionalInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"value": {Type: schema.TypeID(schema.String), Default: json.RawMessage(`"default"`)},
		}},
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name":       {Type: schema.TypeID(schema.String), Required: true},
			"mutateName": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Admin", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String), Required: true},
		}},
		{ID: "Actor", Kind: schema.UnionType, Output: true, Variants: []schema.TypeID{"User", "Admin"}},
		{ID: "Users", Kind: schema.ListType, Output: true, Element: "User"},
	}...)
}

func freezeCompositionTypes(t testing.TB, descriptors ...schema.TypeDescriptor) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range descriptors {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("register type %s: %v", descriptor.ID, err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("freeze types: %v", err)
	}
	return types
}

func registerComposition(t testing.TB, registry *runtime.Registry, definitions ...runtime.Definition) {
	t.Helper()
	for index, definition := range definitions {
		if err := registry.Register(definition); err != nil {
			t.Fatalf("register definition %d: %v", index, err)
		}
	}
}

func assertJSONEqual(t testing.TB, got, want any) {
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
