package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestPrepareValidatesEntireOperationBeforeExecutingHandlers(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	snapshot := registryWithCalls(t, map[string]registeredCall{
		"known": {
			kind: protocol.Query,
			handler: func(_ context.Context, _ runtime.Invocation) (string, error) {
				calls.Add(1)
				return "called", nil
			},
		},
	})
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Bad","kind":"query","select":[{"$call":{"name":"known"}},{"$call":{"name":"unknown"}}]}]}}`)

	if _, err := runtime.Prepare(snapshot, request); err == nil {
		t.Fatal("Prepare succeeded with unknown call")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handlers invoked during validation: %d", got)
	}
}

func TestExecuteCompletesSequentialSelectionBeforeNextSibling(t *testing.T) {
	t.Parallel()

	var state atomic.Int64
	snapshot := registryWithCalls(t, map[string]registeredCall{
		"write": {
			kind:   protocol.Mutation,
			effect: runtime.WriteEffect,
			handler: func(_ context.Context, _ runtime.Invocation) (string, error) {
				state.Store(7)
				return "written", nil
			},
		},
		"read": {
			kind: protocol.Query,
			handler: func(_ context.Context, _ runtime.Invocation) (string, error) {
				return fmt.Sprint(state.Load()), nil
			},
		},
	})
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Change","kind":"mutation","select":[{"$call":{"name":"write"}},{"$call":{"name":"read"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	if outcome.Data["write"] != "written" || outcome.Data["read"] != "7" {
		t.Fatalf("Execute data = %#v", outcome.Data)
	}
}

func TestExecuteReportsCancellationOnceAndStops(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	var later atomic.Int64
	snapshot := registryWithCalls(t, map[string]registeredCall{
		"cancel": {kind: protocol.Query, handler: func(ctx context.Context, _ runtime.Invocation) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}},
		"later": {kind: protocol.Query, handler: func(_ context.Context, _ runtime.Invocation) (string, error) { later.Add(1); return "bad", nil }},
	})
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"cancel"}},{"$call":{"name":"later"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan runtime.Outcome, 1)
	go func() { result <- plan.Execute(ctx) }()
	<-started
	cancel()
	out := <-result
	if len(out.Errors) != 1 || out.Errors[0].Code != "CANCELLED" || later.Load() != 0 {
		t.Fatalf("outcome = %#v, later=%d", out, later.Load())
	}
}

func TestExecuteCopiesTypedContainersAndRejectsCycles(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "Payload", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"x": {Type: schema.TypeID(schema.String)}}}); err != nil {
		t.Fatal(err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]string{"x": "safe"}
	cycle := map[string]any{}
	cycle["self"] = cycle
	copyDescriptor := runtime.Descriptor{Name: "copy", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Payload", Metadata: completeMetadata(runtime.ReadEffect)}
	copyDefinition := runtime.BindInvocation(copyDescriptor, func(_ context.Context, _ runtime.Invocation) (map[string]string, error) { return original, nil })
	cycleDescriptor := copyDescriptor
	cycleDescriptor.Name = "cycle"
	cycleDefinition := runtime.BindInvocation(cycleDescriptor, func(_ context.Context, _ runtime.Invocation) (map[string]any, error) { return cycle, nil })
	out := runtime.ExecuteDefinitionsForTest(context.Background(), types, []runtime.Definition{copyDefinition, cycleDefinition})
	original["x"] = "mutated"
	if out.Data["copy"].(map[string]any)["x"] != "safe" {
		t.Fatalf("typed map was not copied: %#v", out.Data)
	}
	if len(out.Errors) != 1 || out.Errors[0].Code != "OUTPUT_COMPLETION" {
		t.Fatalf("cycle outcome = %#v", out)
	}
}

func TestExecuteCanonicalizesExtendedScalarOutputs(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "Integers", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.Int64)}); err != nil {
		t.Fatal(err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	registry := runtime.NewRegistry(types)
	for _, definition := range []runtime.Definition{
		runtime.BindInvocation(runtime.Descriptor{Name: "integer", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.Int64), Metadata: completeMetadata(runtime.ReadEffect)}, func(_ context.Context, _ runtime.Invocation) (string, error) {
			return "001", nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "timestamp", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.Timestamp), Metadata: completeMetadata(runtime.ReadEffect)}, func(_ context.Context, _ runtime.Invocation) (string, error) {
			return "2026-09-11T12:30:00+02:00", nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "integers", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Integers", Metadata: completeMetadata(runtime.ReadEffect)}, func(_ context.Context, _ runtime.Invocation) ([]string, error) {
			return []string{"001", "-0"}, nil
		}),
	} {
		if err := registry.Register(definition); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"integer"}},{"$call":{"name":"timestamp"}},{"$call":{"name":"integers"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	out := plan.Execute(context.Background())
	if len(out.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", out.Errors)
	}
	if out.Data["integer"] != "1" || out.Data["timestamp"] != "2026-09-11T10:30:00Z" {
		t.Fatalf("canonical data = %#v", out.Data)
	}
	integers, ok := out.Data["integers"].([]any)
	if !ok || len(integers) != 2 || integers[0] != "1" || integers[1] != "0" {
		t.Fatalf("canonical list = %#v", out.Data["integers"])
	}
}

func TestExecuteCanonicalizesCustomScalarOutput(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	descriptor := schema.TypeDescriptor{
		ID: "Pair", Kind: schema.ScalarType, Input: true, Output: true,
		Scalar: &schema.ScalarDescriptor{
			AcceptedWireShapes: []schema.JSONShape{schema.JSONObject},
			Validator:          schema.ScalarValidatorShape,
			Serializer:         schema.ScalarSerializerIdentity,
			Canonicalizer:      schema.ScalarCanonicalJSON,
			CanonicalProfile:   "c14n-1",
			Limits:             schema.ScalarLimits{MaxBytes: 1024, MaxDepth: 4, MaxMembers: 4, MaxArrayItems: 4, MaxStringBytes: 128, MaxNumberBytes: 32, MaxTokens: 16},
			Conformance: []schema.ScalarConformanceVector{
				{Input: json.RawMessage(`{"b":1.0,"a":2}`), Canonical: json.RawMessage(`{"a":2,"b":1}`)},
				{Input: json.RawMessage(`{"b":3.0,"a":4}`), Canonical: json.RawMessage(`{"a":4,"b":3}`)},
			},
		},
	}
	if err := catalog.RegisterScalar(descriptor); err != nil {
		t.Fatal(err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	definition := runtime.BindInvocation(runtime.Descriptor{Name: "pair", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Pair", Metadata: completeMetadata(runtime.ReadEffect)}, func(context.Context, runtime.Invocation) (json.RawMessage, error) {
		return json.RawMessage(`{"b":5.0,"a":6}`), nil
	})
	out := executeDefinitions(t, types, []runtime.Definition{definition}, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"pair"}}]}]}}`)
	pair, ok := out.Data["pair"].(map[string]any)
	if len(out.Errors) != 0 || !ok || fmt.Sprint(pair["a"]) != "6" || fmt.Sprint(pair["b"]) != "5" {
		t.Fatalf("custom scalar outcome = %#v", out)
	}
}

func TestExecuteCompletesTaggedUnionAndInterfaceOutputs(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"id": {Type: schema.TypeID(schema.ID), Required: true}}},
		{ID: "OpenResult", Kind: schema.UnionType, Output: true, Open: true, Variants: []schema.TypeID{"User"}},
		{ID: "ClosedResult", Kind: schema.UnionType, Output: true, Variants: []schema.TypeID{"User"}},
		{ID: "Entity", Kind: schema.InterfaceType, Output: true, Variants: []schema.TypeID{"User"}, Fields: map[string]schema.FieldDescriptor{"id": {Type: schema.TypeID(schema.ID), Required: true}}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	definitions := []runtime.Definition{
		runtime.BindInvocation(runtime.Descriptor{Name: "known", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "OpenResult", Metadata: metadata}, func(context.Context, runtime.Invocation) (schema.TaggedValue, error) {
			return schema.MustTag("User", map[string]any{"id": "u-1"}), nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "interface", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Entity", Metadata: metadata}, func(context.Context, runtime.Invocation) (schema.TaggedValue, error) {
			return schema.MustTag("User", map[string]any{"id": "u-2"}), nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "unknown", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "OpenResult", Metadata: metadata}, func(context.Context, runtime.Invocation) (schema.TaggedValue, error) {
			return schema.MustTag("Future", json.RawMessage(`{"\ue000":"bmp","😀":"face"}`)), nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "closed", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "ClosedResult", Metadata: metadata}, func(context.Context, runtime.Invocation) (schema.TaggedValue, error) {
			return schema.MustTag("Future", map[string]any{"id": "future"}), nil
		}),
	}
	out := executeDefinitions(t, types, definitions, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"known"}},{"$call":{"name":"interface"}},{"$call":{"name":"unknown"}},{"$call":{"name":"closed"}}]}]}}`)
	if len(out.Errors) != 1 || fmt.Sprint(out.Errors[0].Path) != "[closed $type]" {
		t.Fatalf("tagged completion errors = %#v", out.Errors)
	}
	for name, wantID := range map[string]string{"known": "u-1", "interface": "u-2"} {
		tagged := out.Data[name].(map[string]any)
		if tagged["$type"] != "User" || tagged["$value"].(map[string]any)["id"] != wantID {
			t.Fatalf("%s tagged output = %#v", name, tagged)
		}
	}
	unknown := out.Data["unknown"].(map[string]any)
	if unknown["$type"] != "Future" || unknown["$value"].(map[string]any)["😀"] != "face" {
		t.Fatalf("unknown tagged output = %#v", unknown)
	}
	if _, exists := out.Data["closed"]; exists {
		t.Fatalf("closed union output remained available: %#v", out.Data)
	}
}

func TestExecuteDistinguishesNullableRootNullFromUnavailableOutput(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "Result", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"value": {Type: schema.TypeID(schema.String)}}}); err != nil {
		t.Fatal(err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	base := runtime.Descriptor{Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Result", Metadata: completeMetadata(runtime.ReadEffect)}
	nullable := base
	nullable.Name = "nullable"
	nullable.OutputNullable = true
	nonNull := base
	nonNull.Name = "required"
	definitions := []runtime.Definition{
		runtime.BindInvocation(nullable, func(context.Context, runtime.Invocation) (map[string]any, error) { return nil, nil }),
		runtime.BindInvocation(nonNull, func(context.Context, runtime.Invocation) (map[string]any, error) { return nil, nil }),
	}
	out := executeDefinitions(t, types, definitions, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"nullable"}},{"$call":{"name":"required"}}]}]}}`)
	if value, exists := out.Data["nullable"]; !exists || value != nil {
		t.Fatalf("nullable root output = %#v, exists=%t", value, exists)
	}
	if _, exists := out.Data["required"]; exists || len(out.Errors) != 1 || fmt.Sprint(out.Errors[0].Path) != "[required]" {
		t.Fatalf("non-null root outcome = %#v", out)
	}
}

func executeDefinitions(t *testing.T, types schema.Snapshot, definitions []runtime.Definition, requestJSON string) runtime.Outcome {
	t.Helper()
	_ = requestJSON
	return runtime.ExecuteDefinitionsForTest(context.Background(), types, definitions)
}

func TestExecuteEnforcesElementNullabilityAndSchemaDepth(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "Names", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.String)},
		{ID: "NullableNames", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.String), ElementNullable: true},
		{ID: "NamesByKey", Kind: schema.MapType, Output: true, Element: schema.TypeID(schema.String)},
		{ID: "Node", Kind: schema.ObjectType, Output: true, MaxDepth: 1, Fields: map[string]schema.FieldDescriptor{"next": {Type: "Node", Nullable: true}}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	definitions := []runtime.Definition{
		runtime.BindInvocation(runtime.Descriptor{Name: "badList", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Names", Metadata: metadata}, func(_ context.Context, _ runtime.Invocation) ([]any, error) { return []any{nil}, nil }),
		runtime.BindInvocation(runtime.Descriptor{Name: "nullableList", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "NullableNames", Metadata: metadata}, func(_ context.Context, _ runtime.Invocation) ([]any, error) { return []any{nil}, nil }),
		runtime.BindInvocation(runtime.Descriptor{Name: "badMap", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "NamesByKey", Metadata: metadata}, func(_ context.Context, _ runtime.Invocation) (map[string]any, error) {
			return map[string]any{"x": nil}, nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "deepNode", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Node", Metadata: metadata}, func(_ context.Context, _ runtime.Invocation) (map[string]any, error) {
			return map[string]any{"next": map[string]any{"next": map[string]any{}}}, nil
		}),
	}
	out := runtime.ExecuteDefinitionsForTest(context.Background(), types, definitions)
	if len(out.Errors) != 3 {
		t.Fatalf("Execute errors = %#v", out.Errors)
	}
	if len(out.Data) != 4 || len(out.Data["badList"].([]any)) != 1 || out.Data["badList"].([]any)[0] != nil || len(out.Data["nullableList"].([]any)) != 1 || out.Data["nullableList"].([]any)[0] != nil {
		t.Fatalf("Execute data = %#v", out.Data)
	}
	if len(out.Data["badMap"].(map[string]any)) != 0 || len(out.Data["deepNode"].(map[string]any)["next"].(map[string]any)) != 0 {
		t.Fatalf("partial container data = %#v", out.Data)
	}
	wantPaths := [][]any{{"badList", 0}, {"badMap", "x"}, {"deepNode", "next", "next"}}
	for _, executionError := range out.Errors {
		if executionError.Code != "OUTPUT_COMPLETION" {
			t.Fatalf("unexpected completion error = %#v", executionError)
		}
	}
	for index, want := range wantPaths {
		if fmt.Sprint(out.Errors[index].Path) != fmt.Sprint(want) {
			t.Fatalf("error %d path = %#v, want %#v", index, out.Errors[index].Path, want)
		}
	}
}

func TestExecutePreservesValidObjectFieldsAroundCompletionFailures(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "Result", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
		"good": {Type: schema.TypeID(schema.String), Required: true},
		"bad":  {Type: schema.TypeID(schema.Int32), Required: true},
		"gone": {Type: schema.TypeID(schema.String), Required: true},
		"null": {Type: schema.TypeID(schema.String), Nullable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := runtime.Descriptor{Name: "result", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Result", Metadata: completeMetadata(runtime.ReadEffect)}
	out := runtime.ExecuteDefinitionsForTest(context.Background(), types, []runtime.Definition{
		runtime.BindInvocation(descriptor, func(context.Context, runtime.Invocation) (map[string]any, error) {
			return map[string]any{"good": "kept", "bad": "not-an-int", "extra": "removed", "null": nil}, nil
		}),
	})
	if got := out.Data["result"]; fmt.Sprint(got) != "map[good:kept null:<nil>]" {
		t.Fatalf("partial object = %#v", got)
	}
	wantPaths := [][]any{{"result", "bad"}, {"result", "extra"}, {"result", "gone"}}
	if len(out.Errors) != len(wantPaths) {
		t.Fatalf("errors = %#v", out.Errors)
	}
	for index, want := range wantPaths {
		if fmt.Sprint(out.Errors[index].Path) != fmt.Sprint(want) {
			t.Fatalf("error %d path = %#v, want %#v", index, out.Errors[index].Path, want)
		}
	}
}

func TestExecuteRejectsInvalidUTF8WithoutRepairingOutput(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "Strings", Kind: schema.MapType, Output: true, Element: schema.TypeID(schema.String)}); err != nil {
		t.Fatal(err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	registry := runtime.NewRegistry(types)
	metadata := completeMetadata(runtime.ReadEffect)
	for _, definition := range []runtime.Definition{
		runtime.BindInvocation(runtime.Descriptor{Name: "scalar", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata}, func(context.Context, runtime.Invocation) (string, error) {
			return string([]byte{0xff}), nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "mapped", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Strings", Metadata: metadata}, func(context.Context, runtime.Invocation) (map[string]string, error) {
			return map[string]string{string([]byte{0xff}): "value"}, nil
		}),
	} {
		if err := registry.Register(definition); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"scalar"}},{"$call":{"name":"mapped"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	out := plan.Execute(context.Background())
	if _, exists := out.Data["scalar"]; exists || len(out.Data["mapped"].(map[string]any)) != 0 || len(out.Errors) != 2 {
		t.Fatalf("invalid UTF-8 outcome = %#v", out)
	}
	if len(out.Errors[0].Path) != 2 || out.Errors[0].Path[0] != "mapped" || out.Errors[0].Path[1] != string([]byte{0xff}) || fmt.Sprint(out.Errors[1].Path) != "[scalar]" {
		t.Fatalf("invalid UTF-8 paths = %#v", out.Errors)
	}
}

func TestExecuteRejectsNestedScalarGoTypeCoercions(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "Coercions", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"bytes": {Type: schema.TypeID(schema.String), Required: true},
			"count": {Type: schema.TypeID(schema.Int32), Required: true},
		}},
		{ID: "Floats", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.Float64)},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	definitions := []runtime.Definition{
		runtime.BindInvocation(runtime.Descriptor{Name: "object", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Coercions", Metadata: metadata}, func(context.Context, runtime.Invocation) (map[string]any, error) {
			return map[string]any{"bytes": []byte("abc"), "count": int(1)}, nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "list", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Floats", Metadata: metadata}, func(context.Context, runtime.Invocation) ([]any, error) {
			return []any{int(1), float64(2)}, nil
		}),
	}
	out := runtime.ExecuteDefinitionsForTest(context.Background(), types, definitions)
	if len(out.Data["object"].(map[string]any)) != 0 {
		t.Fatalf("coerced object data = %#v", out.Data["object"])
	}
	list := out.Data["list"].([]any)
	if len(list) != 2 || list[0] != nil || fmt.Sprint(list[1]) != "2" {
		t.Fatalf("coerced list data = %#v", list)
	}
	wantPaths := [][]any{{"list", 0}, {"object", "bytes"}, {"object", "count"}}
	if len(out.Errors) != len(wantPaths) {
		t.Fatalf("errors = %#v", out.Errors)
	}
	for index, want := range wantPaths {
		if fmt.Sprint(out.Errors[index].Path) != fmt.Sprint(want) {
			t.Fatalf("error %d path = %#v, want %#v", index, out.Errors[index].Path, want)
		}
	}
}

func TestExecuteKeepsSuccessfulSiblingAndRedactsHandlerFailures(t *testing.T) {
	t.Parallel()

	snapshot := registryWithCalls(t, map[string]registeredCall{
		"fails": {
			kind: protocol.Query,
			handler: func(_ context.Context, _ runtime.Invocation) (string, error) {
				return "", errors.New("database password appeared here")
			},
		},
		"panics": {
			kind: protocol.Query,
			handler: func(_ context.Context, _ runtime.Invocation) (string, error) {
				panic("private stack detail")
			},
		},
		"works": {
			kind: protocol.Query,
			handler: func(_ context.Context, _ runtime.Invocation) (string, error) {
				return "kept", nil
			},
		},
	})
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Partial","kind":"query","select":[{"$call":{"name":"fails"}},{"$call":{"name":"panics"}},{"$call":{"name":"works","as":"ok"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if outcome.Data["ok"] != "kept" || len(outcome.Data) != 1 {
		t.Fatalf("partial data = %#v", outcome.Data)
	}
	if len(outcome.Errors) != 2 {
		t.Fatalf("error count = %d, want 2", len(outcome.Errors))
	}
	if outcome.Errors[0].Code != "HANDLER_FAILED" || outcome.Errors[0].Message != "field unavailable" || outcome.Errors[0].Path[0] != "fails" {
		t.Fatalf("first error = %#v", outcome.Errors[0])
	}
	if outcome.Errors[1].Code != "INTERNAL" || outcome.Errors[1].Message != "internal execution error" || outcome.Errors[1].Path[0] != "panics" {
		t.Fatalf("panic error = %#v", outcome.Errors[1])
	}
}

func TestPrepareRejectsAliasCollisionsAndQueryWrites(t *testing.T) {
	t.Parallel()

	snapshot := registryWithCalls(t, map[string]registeredCall{
		"one":   {kind: protocol.Query, handler: returnName("one")},
		"two":   {kind: protocol.Query, handler: returnName("two")},
		"write": {kind: protocol.Mutation, effect: runtime.WriteEffect, handler: returnName("write")},
	})

	for _, input := range []string{
		`{"version":"1","document":{"operations":[{"name":"Collision","kind":"query","select":[{"$call":{"name":"one","as":"same"}},{"$call":{"name":"two","as":"same"}}]}]}}`,
		`{"version":"1","document":{"operations":[{"name":"HiddenWrite","kind":"query","select":[{"$call":{"name":"write"}}]}]}}`,
	} {
		request := decodeRuntimeRequest(t, input)
		if _, err := runtime.Prepare(snapshot, request); err == nil {
			t.Fatalf("Prepare(%s) succeeded, want validation error", input)
		}
	}
}

func TestPrepareUsesRequestSourceForOperationDiagnostics(t *testing.T) {
	t.Parallel()
	snapshot := registryWithCalls(t, map[string]registeredCall{"known": {kind: protocol.Query, handler: returnName("known")}})
	for _, input := range []string{
		"\n  {\"version\":\"1\",\"document\":{\"operations\":[{\"name\":\"One\",\"kind\":\"query\",\"select\":[]},{\"name\":\"Two\",\"kind\":\"query\",\"select\":[]}]}}",
		"\n  {\"version\":\"1\",\"operation\":\"Missing\",\"document\":{\"operations\":[{\"name\":\"One\",\"kind\":\"query\",\"select\":[]}]}}",
	} {
		request := decodeRuntimeRequest(t, input)
		_, err := runtime.Prepare(snapshot, request)
		var diagnostic *protocol.Diagnostic
		if !errors.As(err, &diagnostic) {
			t.Fatalf("Prepare error = %v", err)
		}
		if diagnostic.Pointer != "" || diagnostic.Offset != 3 || diagnostic.Line != 2 || diagnostic.Column != 3 {
			t.Fatalf("diagnostic source = %#v", diagnostic)
		}
	}
}

type registeredCall struct {
	kind    protocol.OperationKind
	effect  runtime.Effect
	handler runtime.Handler[runtime.Invocation, string]
}

func registryWithCalls(t *testing.T, calls map[string]registeredCall) runtime.Snapshot {
	t.Helper()
	registry := runtime.NewRegistry(coreTypes(t))
	for name, call := range calls {
		effect := call.effect
		if effect == "" {
			effect = runtime.ReadEffect
		}
		descriptor := runtime.Descriptor{Name: name, Scope: runtime.RootScope, Kind: call.kind, Member: runtime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(effect)}
		if err := registry.Register(runtime.BindInvocation(descriptor, call.handler)); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return snapshot
}

func decodeRuntimeRequest(t *testing.T, input string) *protocol.Request {
	return decodeRuntimeRequestWithOptions(t, input, protocol.DecodeOptions{})
}

func decodeRuntimeRequestWithOptions(t testing.TB, input string, options protocol.DecodeOptions) *protocol.Request {
	t.Helper()
	request, err := protocol.DecodeRequest([]byte(input), options)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return request
}

func returnName(name string) runtime.Handler[runtime.Invocation, string] {
	return func(_ context.Context, _ runtime.Invocation) (string, error) { return name, nil }
}
