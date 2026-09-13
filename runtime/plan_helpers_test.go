package runtime_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func secureCollectionMetadata(t testing.TB, maxPageSize, totalCountCost uint64) *runtime.CollectionMetadata {
	t.Helper()
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "test-key",
		Keys: map[string][]byte{
			"test-key": []byte("0123456789abcdef0123456789abcdef"),
		},
		TTL:         time.Hour,
		Now:         func() time.Time { return time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC) },
		MaxPageSize: maxPageSize,
	})
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	scope, err := runtime.NewCursorScope("test.collection", []byte(`{}`), []byte(`[]`), runtime.CursorScopeOptions{})
	if err != nil {
		t.Fatalf("NewCursorScope: %v", err)
	}
	return &runtime.CollectionMetadata{
		MaxPageSize: maxPageSize, TotalCountCost: totalCountCost, CursorCodec: codec,
		CursorScope: func(context.Context) (runtime.CursorScope, error) { return scope, nil },
		Position: func(item any) (runtime.CursorPosition, error) {
			if value, ok := item.(map[string]any); ok {
				if name, ok := value["name"].(string); ok {
					return runtime.CursorPosition{SortKey: name, TieBreaker: name}, nil
				}
			}
			key := fmt.Sprintf("%#v", item)
			return runtime.CursorPosition{SortKey: key, TieBreaker: key}, nil
		},
	}
}

func normalizeUserPageCursors(t testing.TB, data map[string]any, pageName, replacement string) {
	t.Helper()
	page := data["users"].(map[string]any)[pageName].(map[string]any)
	info := page["pageInfo"].(runtime.PageInfo)
	if info.StartCursor == "" || info.EndCursor == "" {
		t.Fatalf("page info has no opaque boundaries: %#v", info)
	}
	info.StartCursor, info.EndCursor = replacement, replacement
	page["pageInfo"] = info
}

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
		{ID: "IntInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"value": {Type: schema.TypeID(schema.Int32), Required: true},
		}},
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name":       {Type: schema.TypeID(schema.String)},
			"mutateName": {Type: schema.TypeID(schema.String)},
			"id":         {Type: schema.TypeID(schema.ID)},
			"externalID": {Type: schema.TypeID(schema.ID)},
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
	users.Metadata.Collection = secureCollectionMetadata(t, 1000, 0)
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
	registerValidationVectorRoots(t, registry, &calls, rootDescriptor)
	registerValidationMembers(t, registry, &calls)
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

// registerValidationMembers binds the object members the portable language
// vectors address, including the interface owners that fragment type
// conditions narrow to.
func registerValidationMembers(t testing.TB, registry *runtime.Registry, calls *atomic.Int64) {
	t.Helper()
	for _, member := range []struct {
		owner  schema.TypeID
		name   string
		output schema.TypeID
		effect runtime.Effect
	}{
		{"User", "id", schema.TypeID(schema.ID), runtime.ReadEffect},
		{"User", "externalID", schema.TypeID(schema.ID), runtime.ReadEffect},
		{"User", "mutateName", schema.TypeID(schema.String), runtime.WriteEffect},
		{"GroupA", "name", schema.TypeID(schema.String), runtime.ReadEffect},
		{"GroupB", "name", schema.TypeID(schema.String), runtime.ReadEffect},
		{"Staff", "name", schema.TypeID(schema.String), runtime.ReadEffect},
		{"Editor", "name", schema.TypeID(schema.String), runtime.ReadEffect},
	} {
		definition := runtime.BindField[map[string]any, string](runtime.Descriptor{
			Name: member.name, Scope: runtime.ObjectScope, Owner: member.owner, Member: runtime.FieldMember,
			Output: member.output, Metadata: completeMetadata(member.effect),
		}, func(context.Context, map[string]any) (string, error) {
			calls.Add(1)
			return "", nil
		})
		if err := registry.Register(definition); err != nil {
			t.Fatalf("register %s.%s: %v", member.owner, member.name, err)
		}
	}
}

// registerValidationVectorRoots binds the root calls the portable language
// vectors reference beyond the shared set, including the writes a query vector
// must be rejected for reaching.
func registerValidationVectorRoots(t testing.TB, registry *runtime.Registry, calls *atomic.Int64,
	rootDescriptor func(name string, input, output schema.TypeID) runtime.Descriptor,
) {
	t.Helper()
	text := func(descriptor runtime.Descriptor) runtime.Definition {
		return runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) {
			calls.Add(1)
			return "", nil
		})
	}
	write := func(name string) runtime.Descriptor {
		descriptor := rootDescriptor(name, schema.TypeID(schema.String), schema.TypeID(schema.String))
		descriptor.Kind = protocol.Mutation
		descriptor.Metadata = completeMetadata(runtime.WriteEffect)
		return descriptor
	}
	createUser := write("createUser")
	createUser.Output = "User"
	watch := rootDescriptor("watch", schema.TypeID(schema.String), "User")
	watch.Kind = protocol.Subscription
	definitions := []runtime.Definition{
		text(rootDescriptor("load", schema.TypeID(schema.String), schema.TypeID(schema.String))),
		text(rootDescriptor("requiresInt", "IntInput", schema.TypeID(schema.String))),
		text(write("deleteUser")),
		runtime.BindInvocation[map[string]any](watch, func(context.Context, runtime.Invocation) (map[string]any, error) {
			calls.Add(1)
			return nil, nil
		}),
		runtime.BindInvocation[map[string]any](createUser, func(context.Context, runtime.Invocation) (map[string]any, error) {
			calls.Add(1)
			return nil, nil
		}),
	}
	for _, definition := range definitions {
		if err := registry.Register(definition); err != nil {
			t.Fatalf("register vector root: %v", err)
		}
	}
}
