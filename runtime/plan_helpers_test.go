package runtime_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func validationRegistry(t testing.TB) (runtime.Snapshot, *atomic.Int64) {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "LookupInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"id": {Type: schema.TypeID(schema.ID), Required: true},
		}},
		{ID: "ConsumeInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"value": {Type: schema.TypeID(schema.String), Required: true},
		}},
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Admin", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Editor", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Actor", Kind: schema.UnionType, Output: true, Variants: []schema.TypeID{"User", "Admin"}},
		{ID: "Universe", Kind: schema.UnionType, Output: true, Variants: []schema.TypeID{"User", "Admin", "Editor"}},
		{ID: "GroupA", Kind: schema.InterfaceType, Output: true, Variants: []schema.TypeID{"User", "Admin"}, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "GroupB", Kind: schema.InterfaceType, Output: true, Variants: []schema.TypeID{"User", "Editor"}, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Staff", Kind: schema.InterfaceType, Output: true, Variants: []schema.TypeID{"Admin", "Editor"}, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Users", Kind: schema.ListType, Output: true, Element: "User"},
		{ID: "NullableUsers", Kind: schema.ListType, Output: true, Element: "User", ElementNullable: true},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("register type %s: %v", descriptor.ID, err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("freeze types: %v", err)
	}
	registry := runtime.NewRegistry(types)
	var calls atomic.Int64
	register := func(definition runtime.Definition, name string) {
		if err := registry.Register(definition); err != nil {
			t.Fatalf("register root %s: %v", name, err)
		}
	}
	rootDescriptor := func(name string, input, output schema.TypeID) runtime.Descriptor {
		return runtime.Descriptor{Name: name, Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
			Input: input, Output: output, Metadata: completeMetadata(runtime.ReadEffect)}
	}
	lookup := rootDescriptor("lookup", "LookupInput", "User")
	register(runtime.BindInvocation[map[string]any](lookup, func(context.Context, runtime.Invocation) (map[string]any, error) {
		calls.Add(1)
		return nil, nil
	}), "lookup")
	actor := rootDescriptor("actor", schema.TypeID(schema.String), "Actor")
	register(runtime.BindInvocation[schema.TaggedValue](actor, func(context.Context, runtime.Invocation) (schema.TaggedValue, error) {
		calls.Add(1)
		return schema.TaggedValue{}, nil
	}), "actor")
	universe := rootDescriptor("universe", schema.TypeID(schema.String), "Universe")
	register(runtime.BindInvocation[schema.TaggedValue](universe, func(context.Context, runtime.Invocation) (schema.TaggedValue, error) {
		calls.Add(1)
		return schema.TaggedValue{}, nil
	}), "universe")
	consume := rootDescriptor("consume", "ConsumeInput", schema.TypeID(schema.String))
	register(runtime.BindInvocation[string](consume, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "", nil
	}), "consume")
	text := rootDescriptor("text", schema.TypeID(schema.String), schema.TypeID(schema.String))
	register(runtime.BindInvocation[string](text, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "", nil
	}), "text")
	users := rootDescriptor("users", schema.TypeID(schema.String), "Users")
	register(runtime.BindInvocation[[]map[string]any](users, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		calls.Add(1)
		return nil, nil
	}), "users")
	nullableUsers := rootDescriptor("nullableUsers", schema.TypeID(schema.String), "NullableUsers")
	register(runtime.BindInvocation[[]map[string]any](nullableUsers, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
		calls.Add(1)
		return nil, nil
	}), "nullableUsers")
	write := rootDescriptor("write", schema.TypeID(schema.String), schema.TypeID(schema.String))
	write.Kind = protocol.Mutation
	write.Metadata = completeMetadata(runtime.WriteEffect)
	register(runtime.BindInvocation[string](write, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "", nil
	}), "write")
	parallelWrite := write
	parallelWrite.Name = "parallelWrite"
	parallelWrite.Metadata.ParallelMutation = true
	register(runtime.BindInvocation[string](parallelWrite, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "", nil
	}), "parallelWrite")
	serial := rootDescriptor("serial", schema.TypeID(schema.String), schema.TypeID(schema.String))
	serial.Metadata.ThreadSafety = runtime.SerialOnly
	register(runtime.BindInvocation[string](serial, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "", nil
	}), "serial")
	transactional := rootDescriptor("transactional", schema.TypeID(schema.String), schema.TypeID(schema.String))
	transactional.Metadata.Transaction = runtime.TransactionRequired
	register(runtime.BindInvocation[string](transactional, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "", nil
	}), "transactional")
	field := runtime.Descriptor{Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	if err := registry.Register(runtime.BindField[map[string]any, string](field, func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "", nil
	})); err != nil {
		t.Fatalf("register field: %v", err)
	}
	adminField := field
	adminField.Owner = "Admin"
	if err := registry.Register(runtime.BindField[map[string]any, string](adminField, func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "", nil
	})); err != nil {
		t.Fatalf("register admin field: %v", err)
	}
	objectCall := runtime.Descriptor{Name: "label", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.CallMember,
		Input: "ConsumeInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	if err := registry.Register(runtime.BindCall[map[string]any, schema.InputValue, string](objectCall, func(context.Context, map[string]any, schema.InputValue) (string, error) {
		calls.Add(1)
		return "", nil
	})); err != nil {
		t.Fatalf("register object call: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	return snapshot, &calls
}
