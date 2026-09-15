package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type registryUser struct{ name string }

func (u registryUser) UnregisteredMethod() string { return "hidden:" + u.name }

type sharedRegistryHandler struct{ calls atomic.Int64 }

func (h *sharedRegistryHandler) Handle(_ context.Context, value string) (string, error) {
	h.calls.Add(1)
	return value, nil
}

type rootRegistryCase struct {
	name   string
	kind   protocol.OperationKind
	effect runtime.Effect
}

type invalidRegistrationCase struct {
	name       string
	definition func() runtime.Definition
}

func TestRegistryFreezesTypedRootOperations(t *testing.T) {
	t.Parallel()

	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	definition := runtime.Bind(runtime.Descriptor{
		Name:   "hello",
		Scope:  runtime.RootScope,
		Kind:   protocol.Query,
		Member: runtime.CallMember,
		Input:  schema.TypeID(schema.String),
		Output: schema.TypeID(schema.String),
		Metadata: runtime.Metadata{
			Effect:              runtime.ReadEffect,
			Deterministic:       true,
			Cacheable:           true,
			RetrySafe:           true,
			ThreadSafety:        runtime.ThreadSafe,
			Batching:            runtime.BatchEligible,
			Transaction:         runtime.TransactionNone,
			AuthorizationPolicy: "public",
		},
	}, func(_ context.Context, name string) (string, error) {
		return "hello " + name, nil
	})
	if err := registry.Register(definition); err != nil {
		t.Fatalf("Register: %v", err)
	}

	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	descriptor, ok := snapshot.Root(protocol.Query, "hello")
	if !ok || descriptor.Metadata.Effect != runtime.ReadEffect || !descriptor.Metadata.Cacheable {
		t.Fatalf("Root(query, hello) = %#v, %t", descriptor, ok)
	}
	value, err := snapshot.InvokeRoot(context.Background(), protocol.Query, "hello", "Naatre")
	if err != nil {
		t.Fatalf("InvokeRoot: %v", err)
	}
	if value != "hello Naatre" {
		t.Fatalf("InvokeRoot value = %#v", value)
	}
	if _, err := snapshot.InvokeRoot(context.Background(), protocol.Query, "unregistered", "x"); !errors.Is(err, runtime.ErrNotRegistered) {
		t.Fatalf("unregistered InvokeRoot error = %v", err)
	}
}

func TestRegistryCoversEveryRootKindAndExportsMetadata(t *testing.T) {
	t.Parallel()
	tests := rootRegistryCases()
	registry := registryWithRootKinds(t, tests)
	snapshot := frozenRegistry(t, registry)
	descriptors := snapshot.Descriptors()
	if len(descriptors) != len(tests) {
		t.Fatalf("Descriptors length = %d", len(descriptors))
	}
	for index, want := range []string{"mutation", "query", "subscription"} {
		if descriptors[index].Name != want {
			t.Fatalf("descriptor %d = %q, want %q", index, descriptors[index].Name, want)
		}
	}
	for _, test := range tests {
		descriptor, ok := snapshot.Root(test.kind, test.name)
		if !ok {
			t.Fatalf("Root(%s, %s) not found", test.kind, test.name)
		}
		assertRootMetadata(t, test, descriptor.Metadata)
		value, invokeErr := snapshot.InvokeRoot(context.Background(), test.kind, test.name, "value")
		if invokeErr != nil || value != test.name+":value" {
			t.Fatalf("InvokeRoot(%s) = %#v, %v", test.name, value, invokeErr)
		}
	}
	encoded, err := json.Marshal(descriptors)
	if err != nil || !json.Valid(encoded) || strings.Contains(string(encoded), `"Name"`) || !strings.Contains(string(encoded), `"authorizationPolicy"`) {
		t.Fatalf("portable descriptors = %s, %v", encoded, err)
	}
	descriptors[0].Metadata.AuthorizationPolicy = "mutated"
	again := snapshot.Descriptors()
	if again[0].Metadata.AuthorizationPolicy == "mutated" {
		t.Fatal("snapshot descriptor mutated through exported slice")
	}
	late := runtime.Descriptor{
		Name: "late", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	if err := registry.Register(runtime.Bind(late, func(context.Context, string) (string, error) { return "", nil })); err == nil {
		t.Fatal("Register succeeded after Freeze")
	}
	if len(snapshot.Descriptors()) != len(tests) {
		t.Fatal("frozen snapshot changed after rejected registration")
	}
}

func rootRegistryCases() []rootRegistryCase {
	return []rootRegistryCase{
		{name: "query", kind: protocol.Query, effect: runtime.ReadEffect},
		{name: "mutation", kind: protocol.Mutation, effect: runtime.WriteEffect},
		{name: "subscription", kind: protocol.Subscription, effect: runtime.ReadEffect},
	}
}

func registryWithRootKinds(t *testing.T, tests []rootRegistryCase) *runtime.Registry {
	t.Helper()
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		metadata := completeMetadata(test.effect)
		metadata.Deterministic = test.kind != protocol.Subscription
		metadata.Cacheable = test.kind == protocol.Query
		metadata.Deprecation = "use replacement"
		metadata.Idempotency = runtime.IdempotencyConditional
		metadata.Cost = 13
		descriptor := runtime.Descriptor{
			Name: test.name, Scope: runtime.RootScope, Kind: test.kind,
			Member: runtime.CallMember, Input: schema.TypeID(schema.String),
			Output: schema.TypeID(schema.String), Metadata: metadata,
		}
		if err := registry.Register(runtime.Bind(descriptor, func(_ context.Context, value string) (string, error) {
			return test.name + ":" + value, nil
		})); err != nil {
			t.Fatalf("Register(%s): %v", test.kind, err)
		}
	}
	return registry
}

func assertRootMetadata(t *testing.T, test rootRegistryCase, metadata runtime.Metadata) {
	t.Helper()
	if metadata.Effect != test.effect || !metadata.RetrySafe || metadata.ThreadSafety != runtime.ThreadSafe ||
		metadata.Batching != runtime.BatchIneligible || metadata.Transaction != runtime.TransactionNone ||
		metadata.AuthorizationPolicy != "test" || metadata.Deprecation == "" || metadata.Idempotency == "" || metadata.Cost != 13 {
		t.Fatalf("metadata for %s = %#v", test.name, metadata)
	}
}

func frozenRegistry(t *testing.T, registry *runtime.Registry) runtime.Snapshot {
	t.Helper()
	snapshot, err := registry.Freeze()
	checkNoError(t, "Freeze", err)
	return snapshot
}

func checkNoError(t *testing.T, operation string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
}

func TestRegistryInvokesExplicitObjectFieldsAndCalls(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(memberTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	field := runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	call := runtime.Descriptor{
		Name: "greet", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	if err := registry.Register(runtime.BindField(field, func(_ context.Context, source registryUser) (string, error) {
		return source.name, nil
	})); err != nil {
		t.Fatalf("Register field: %v", err)
	}
	if err := registry.Register(runtime.BindCall(call, func(_ context.Context, source registryUser, greeting string) (string, error) {
		return greeting + " " + source.name, nil
	})); err != nil {
		t.Fatalf("Register call: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	source := registryUser{name: "Ada"}
	name, err := snapshot.InvokeField(context.Background(), "User", "name", source)
	if err != nil || name != "Ada" {
		t.Fatalf("InvokeField = %#v, %v", name, err)
	}
	greeting, err := snapshot.InvokeCall(context.Background(), "User", "greet", source, "hello")
	if err != nil || greeting != "hello Ada" {
		t.Fatalf("InvokeCall = %#v, %v", greeting, err)
	}
	if _, err := snapshot.InvokeField(context.Background(), "User", "unregistered", source); !errors.Is(err, runtime.ErrNotRegistered) {
		t.Fatalf("unregistered field error = %v", err)
	}
	if _, err := snapshot.InvokeCall(context.Background(), "User", "UnregisteredMethod", source, ""); !errors.Is(err, runtime.ErrNotRegistered) {
		t.Fatalf("unregistered Go method became reachable: %v", err)
	}
	if _, err := snapshot.InvokeCall(context.Background(), "User", "greet", struct{}{}, "hello"); !errors.Is(err, runtime.ErrSourceType) {
		t.Fatalf("wrong source error = %v", err)
	}
	if _, err := snapshot.InvokeCall(context.Background(), "User", "greet", source, 42); !errors.Is(err, runtime.ErrInputType) {
		t.Fatalf("wrong input error = %v", err)
	}
}

func TestRegistryRejectsBinderSchemaAndMetadataMismatches(t *testing.T) {
	t.Parallel()
	types := memberTypes(t)
	validRoot := runtime.Descriptor{
		Name: "root", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	validField := runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	validCall := runtime.Descriptor{
		Name: "greet", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	tests := []invalidRegistrationCase{
		{name: "root through field binder", definition: func() runtime.Definition {
			return runtime.BindField(validRoot, func(context.Context, registryUser) (string, error) { return "", nil })
		}},
		{name: "field through root binder", definition: func() runtime.Definition {
			return runtime.Bind(validField, func(context.Context, string) (string, error) { return "", nil })
		}},
		{name: "undeclared field", definition: func() runtime.Definition {
			descriptor := validField
			descriptor.Name = "missing"
			return runtime.BindField(descriptor, func(context.Context, registryUser) (string, error) { return "", nil })
		}},
		{name: "field output mismatch", definition: func() runtime.Definition {
			descriptor := validField
			descriptor.Output = schema.TypeID(schema.Int32)
			return runtime.BindField(descriptor, func(context.Context, registryUser) (int32, error) { return 0, nil })
		}},
		{name: "call without input", definition: func() runtime.Definition {
			descriptor := validCall
			descriptor.Input = ""
			return runtime.BindCall(descriptor, func(context.Context, registryUser, string) (string, error) { return "", nil })
		}},
	}
	assertRegistrationsRejected(t, types, tests)
}

func TestRegistryRejectsInputAndMetadataMismatches(t *testing.T) {
	t.Parallel()
	types := memberTypes(t)
	valid := runtime.Descriptor{
		Name: "root", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	tests := []invalidRegistrationCase{
		{name: "composite input as int", definition: func() runtime.Definition {
			descriptor := valid
			descriptor.Input = "UserInput"
			return runtime.Bind(descriptor, func(context.Context, int) (string, error) { return "", nil })
		}},
		{name: "nonnull scalar pointer input", definition: func() runtime.Definition {
			return runtime.Bind(valid, func(context.Context, *string) (string, error) { return "", nil })
		}},
		{name: "nullable scalar value input", definition: func() runtime.Definition {
			descriptor := valid
			descriptor.InputNullable = true
			return runtime.Bind(descriptor, func(context.Context, string) (string, error) { return "", nil })
		}},
		{name: "nondeterministic cache", definition: func() runtime.Definition {
			descriptor := valid
			descriptor.Metadata.Deterministic = false
			return runtime.Bind(descriptor, func(context.Context, string) (string, error) { return "", nil })
		}},
		{name: "parallel query", definition: func() runtime.Definition {
			descriptor := valid
			descriptor.Metadata.ParallelMutation = true
			return runtime.Bind(descriptor, func(context.Context, string) (string, error) { return "", nil })
		}},
	}
	assertRegistrationsRejected(t, types, tests)
}

func assertRegistrationsRejected(t *testing.T, types schema.Snapshot, tests []invalidRegistrationCase) {
	t.Helper()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := runtime.NewRegistry(types).Register(test.definition()); err == nil {
				t.Fatal("Register succeeded")
			}
		})
	}
}

func TestRegistryNormalizesNullableInputAtInterfaceBoundary(t *testing.T) {
	t.Parallel()
	descriptor := runtime.Descriptor{
		Name: "nullable", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), InputNullable: true,
		Output: schema.TypeID(schema.Boolean), Metadata: completeMetadata(runtime.ReadEffect),
	}
	snapshot := snapshotWithDefinition(t, coreTypes(t), runtime.Bind(descriptor, func(_ context.Context, value *string) (bool, error) {
		return value == nil, nil
	}))
	for _, input := range []any{nil, (*string)(nil)} {
		value, invokeErr := snapshot.InvokeRoot(context.Background(), protocol.Query, "nullable", input)
		if invokeErr != nil || value != true {
			t.Fatalf("nullable input %#v = %#v, %v", input, value, invokeErr)
		}
	}
}

func TestRegistryRejectsInvalidRegistrationsAtStartup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		descriptor runtime.Descriptor
		handler    runtime.Definition
	}{
		{
			name: "query with write effect",
			descriptor: runtime.Descriptor{Name: "bad", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
				Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.WriteEffect)},
		},
		{
			name: "unresolved output",
			descriptor: runtime.Descriptor{Name: "bad", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
				Input: schema.TypeID(schema.String), Output: "Unknown", Metadata: completeMetadata(runtime.ReadEffect)},
		},
		{
			name: "field without owner",
			descriptor: runtime.Descriptor{Name: "name", Scope: runtime.ObjectScope, Member: runtime.FieldMember,
				Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			registry := runtime.NewRegistry(coreTypes(t))
			if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
				t.Fatal(err)
			}
			definition := runtime.Bind(tt.descriptor, func(_ context.Context, value string) (string, error) { return value, nil })
			if err := registry.Register(definition); err == nil {
				t.Fatalf("Register(%#v) succeeded, want error", tt.descriptor)
			}
		})
	}

	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	descriptor := runtime.Descriptor{Name: "nil", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	var handler runtime.Handler[string, string]
	if err := registry.Register(runtime.Bind(descriptor, handler)); err == nil {
		t.Fatal("Register accepted nil handler")
	}
	if err := registry.Register(runtime.Bind(descriptor, func(_ context.Context, value int) (string, error) { return fmt.Sprint(value), nil })); err == nil {
		t.Fatal("Register accepted Go input type inconsistent with String schema")
	}
}

func TestRegistryValidatesNullableAndCompositeOutputSignatures(t *testing.T) {
	t.Parallel()
	types := compositeOutputTypes(t)
	base := runtime.Descriptor{Name: "value", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	nullable := base
	nullable.Output = schema.TypeID(schema.String)
	nullable.OutputNullable = true
	if err := runtime.NewRegistry(types).Register(runtime.Bind(nullable, func(context.Context, string) (*string, error) { return nil, nil })); err != nil {
		t.Fatalf("nullable scalar pointer rejected: %v", err)
	}
	if err := runtime.NewRegistry(types).Register(runtime.Bind(outputDescriptor(base, schema.TypeID(schema.String)), func(context.Context, string) (*string, error) { return nil, nil })); err == nil {
		t.Fatal("non-null scalar pointer accepted")
	}
}

func TestRegistryAcceptsCompatibleCompositeOutputSignatures(t *testing.T) {
	t.Parallel()
	types := compositeOutputTypes(t)
	base := runtime.Descriptor{Name: "value", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	for _, valid := range []runtime.Definition{
		runtime.Bind(outputDescriptor(base, "Object"), func(context.Context, string) (map[string]string, error) { return nil, nil }),
		runtime.Bind(outputDescriptor(base, "List"), func(context.Context, string) ([]string, error) { return nil, nil }),
		runtime.Bind(outputDescriptor(base, "NestedList"), func(context.Context, string) ([][]string, error) { return nil, nil }),
		runtime.Bind(outputDescriptor(base, "NullableList"), func(context.Context, string) ([]*string, error) { return nil, nil }),
		runtime.Bind(outputDescriptor(base, "Map"), func(context.Context, string) (map[string]any, error) { return nil, nil }),
	} {
		if err := runtime.NewRegistry(types).Register(valid); err != nil {
			t.Fatalf("valid composite output rejected: %v", err)
		}
	}
}

func TestRegistryRejectsIncompatibleCompositeOutputSignatures(t *testing.T) {
	t.Parallel()
	types := compositeOutputTypes(t)
	base := runtime.Descriptor{Name: "value", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	tests := []struct {
		name       string
		output     schema.TypeID
		definition func(runtime.Descriptor) runtime.Definition
	}{
		{"object as int", "Object", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (int, error) { return 0, nil })
		}},
		{"list as map", "List", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (map[string]any, error) { return nil, nil })
		}},
		{"list with wrong element", "List", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) ([]int, error) { return nil, nil })
		}},
		{"nested list with wrong element", "NestedList", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) ([][]int, error) { return nil, nil })
		}},
		{"map with integer keys", "Map", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (map[int]any, error) { return nil, nil })
		}},
		{"map with wrong value", "Map", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (map[string]int, error) { return nil, nil })
		}},
		{"object with wrong field value", "Object", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (map[string]int, error) { return nil, nil })
		}},
		{"enum as int", "Enum", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (int, error) { return 0, nil })
		}},
		{"unsupported interface", "Interface", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (map[string]any, error) { return nil, nil })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			descriptor := base
			descriptor.Output = tt.output
			if err := runtime.NewRegistry(types).Register(tt.definition(descriptor)); err == nil {
				t.Fatal("Register succeeded")
			}
		})
	}
}

func compositeOutputTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "Object", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"value": {Type: schema.TypeID(schema.String)}}},
		{ID: "List", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.String)},
		{ID: "NestedList", Kind: schema.ListType, Output: true, Element: "List"},
		{ID: "NullableList", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.String), ElementNullable: true},
		{ID: "Map", Kind: schema.MapType, Output: true, Element: schema.TypeID(schema.String)},
		{ID: "Enum", Kind: schema.EnumType, Output: true, EnumValues: []string{"ONE"}},
		{ID: "Interface", Kind: schema.InterfaceType, Output: true, Variants: []schema.TypeID{"Object"}, Fields: map[string]schema.FieldDescriptor{"value": {Type: schema.TypeID(schema.String)}}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	return types
}

func outputDescriptor(base runtime.Descriptor, output schema.TypeID) runtime.Descriptor {
	base.Output = output
	return base
}

func TestDirectInvocationContainsAndRedactsPanics(t *testing.T) {
	t.Parallel()
	descriptor := runtime.Descriptor{Name: "panic", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	snapshot := snapshotWithDefinition(t, coreTypes(t), runtime.Bind(descriptor, func(context.Context, string) (string, error) { panic("private detail") }))
	if _, err := snapshot.InvokeRoot(context.Background(), protocol.Query, "panic", ""); err == nil || strings.Contains(err.Error(), "private detail") {
		t.Fatalf("InvokeRoot panic error = %v", err)
	}
}

func TestDirectInvocationDoesNotAdmitPreCancelledContexts(t *testing.T) {
	t.Parallel()
	for _, safety := range []runtime.ThreadSafety{runtime.ThreadSafe, runtime.SerialOnly} {
		t.Run(string(safety), func(t *testing.T) {
			var calls atomic.Int64
			metadata := completeMetadata(runtime.ReadEffect)
			metadata.ThreadSafety = safety
			registry := runtime.NewRegistry(coreTypes(t))
			if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
				t.Fatal(err)
			}
			descriptor := runtime.Descriptor{Name: "cancelled", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata}
			if err := registry.Register(runtime.Bind(descriptor, func(context.Context, string) (string, error) {
				calls.Add(1)
				return "entered", nil
			})); err != nil {
				t.Fatal(err)
			}
			snapshot, err := registry.Freeze()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := snapshot.InvokeRoot(ctx, protocol.Query, "cancelled", ""); !errors.Is(err, context.Canceled) {
				t.Fatalf("InvokeRoot error = %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("pre-cancelled handler calls = %d", calls.Load())
			}
		})
	}
}

func TestRegistryRejectsDuplicatePublicNames(t *testing.T) {
	t.Parallel()

	descriptor := runtime.Descriptor{Name: "same", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	definition := runtime.Bind(descriptor, func(_ context.Context, value string) (string, error) { return value, nil })
	registry := registeredRegistry(t, coreTypes(t), definition)
	if err := registry.Register(definition); !errors.Is(err, runtime.ErrDuplicateRegistration) {
		t.Fatalf("duplicate Register error = %v", err)
	}
	mutation := descriptor
	mutation.Kind = protocol.Mutation
	mutation.Metadata = completeMetadata(runtime.WriteEffect)
	if err := registry.Register(runtime.Bind(mutation, func(_ context.Context, value string) (string, error) { return value, nil })); !errors.Is(err, runtime.ErrDuplicateRegistration) {
		t.Fatalf("cross-kind duplicate Register error = %v", err)
	}
}

func TestSnapshotPropagatesCancellationAndSupportsConcurrentReads(t *testing.T) {
	t.Parallel()

	descriptor := runtime.Descriptor{Name: "wait", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	definition := runtime.Bind(descriptor, func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	snapshot := snapshotWithDefinition(t, coreTypes(t), definition)

	const readers = 32
	var wait sync.WaitGroup
	wait.Add(readers)
	for range readers {
		go func() {
			defer wait.Done()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, invokeErr := snapshot.InvokeRoot(ctx, protocol.Query, "wait", "")
			if !errors.Is(invokeErr, context.Canceled) {
				t.Errorf("InvokeRoot error = %v, want context canceled", invokeErr)
			}
		}()
	}
	wait.Wait()
}

func TestThreadSafeSharedHandlerSupportsConcurrentSnapshots(t *testing.T) {
	t.Parallel()
	handler := &sharedRegistryHandler{}
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	descriptor := runtime.Descriptor{
		Name: "shared", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	if err := registry.Register(runtime.Bind(descriptor, handler.Handle)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	const readers = 32
	var wait sync.WaitGroup
	wait.Add(readers)
	for range readers {
		go func() {
			defer wait.Done()
			value, invokeErr := snapshot.InvokeRoot(context.Background(), protocol.Query, "shared", "ok")
			if invokeErr != nil || value != "ok" {
				t.Errorf("InvokeRoot = %#v, %v", value, invokeErr)
			}
			if len(snapshot.Descriptors()) != 1 {
				t.Error("concurrent descriptor read changed snapshot")
			}
		}()
	}
	wait.Wait()
	if handler.calls.Load() != readers {
		t.Fatalf("shared handler calls = %d", handler.calls.Load())
	}
}

func TestSerialOnlyHandlerDoesNotOverlapAcrossRequests(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.ThreadSafety = runtime.SerialOnly
	descriptor := runtime.Descriptor{Name: "serial", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	if err := registry.Register(runtime.Bind(descriptor, func(_ context.Context, value string) (string, error) {
		entered <- struct{}{}
		<-release
		return value, nil
	})); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		_, invokeErr := snapshot.InvokeRoot(context.Background(), protocol.Query, "serial", "a")
		done <- invokeErr
	}()
	<-entered
	secondBase, cancel := context.WithCancel(context.Background())
	observed := make(chan struct{})
	second := &observedDoneContext{Context: secondBase, observed: observed}
	go func() {
		_, invokeErr := snapshot.InvokeRoot(second, protocol.Query, "serial", "b")
		done <- invokeErr
	}()
	<-observed
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued invocation error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first invocation error = %v", err)
	}
	if len(entered) != 0 {
		t.Fatal("cancelled serial-only handler entered")
	}
}

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func registeredRegistry(t *testing.T, types schema.Snapshot, definition runtime.Definition) *runtime.Registry {
	t.Helper()
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(definition); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return registry
}

func snapshotWithDefinition(t *testing.T, types schema.Snapshot, definition runtime.Definition) runtime.Snapshot {
	return frozenRegistry(t, registeredRegistry(t, types, definition))
}

func memberTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{
		ID: "User", Kind: schema.ObjectType, Output: true,
		Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(schema.TypeDescriptor{
		ID: "UserInput", Kind: schema.InputObjectType, Input: true,
		Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func coreTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	snapshot, err := schema.NewCatalog().Freeze()
	if err == nil {
		return snapshot
	}
	t.Fatalf("freeze core types: %v", err)
	return schema.Snapshot{}
}

func completeMetadata(effect runtime.Effect) runtime.Metadata {
	return runtime.Metadata{
		Effect:              effect,
		Deterministic:       true,
		Cacheable:           effect == runtime.ReadEffect,
		RetrySafe:           true,
		ThreadSafety:        runtime.ThreadSafe,
		Batching:            runtime.BatchIneligible,
		Transaction:         runtime.TransactionNone,
		AuthorizationPolicy: "test",
	}
}
