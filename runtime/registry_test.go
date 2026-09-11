package runtime_test

import (
	"context"
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

func TestRegistryFreezesTypedRootOperations(t *testing.T) {
	t.Parallel()

	registry := runtime.NewRegistry(coreTypes(t))
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
			definition := runtime.Bind(tt.descriptor, func(_ context.Context, value string) (string, error) { return value, nil })
			if err := registry.Register(definition); err == nil {
				t.Fatalf("Register(%#v) succeeded, want error", tt.descriptor)
			}
		})
	}

	registry := runtime.NewRegistry(coreTypes(t))
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
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "Object", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"value": {Type: schema.TypeID(schema.String)}}},
		{ID: "List", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.String)},
		{ID: "Map", Kind: schema.MapType, Output: true, Element: schema.TypeID(schema.String)},
		{ID: "Enum", Kind: schema.EnumType, Output: true, EnumValues: []string{"ONE"}},
		{ID: "Interface", Kind: schema.InterfaceType, Output: true, Fields: map[string]schema.FieldDescriptor{"value": {Type: schema.TypeID(schema.String)}}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	base := runtime.Descriptor{Name: "value", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}

	nullable := base
	nullable.Output = schema.TypeID(schema.String)
	nullable.OutputNullable = true
	registry := runtime.NewRegistry(types)
	if err := registry.Register(runtime.Bind(nullable, func(context.Context, string) (*string, error) { return nil, nil })); err != nil {
		t.Fatalf("nullable scalar pointer rejected: %v", err)
	}

	tests := []struct {
		name       string
		output     schema.TypeID
		definition func(runtime.Descriptor) runtime.Definition
	}{
		{"non-null scalar pointer", schema.TypeID(schema.String), func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (*string, error) { return nil, nil })
		}},
		{"object as int", "Object", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (int, error) { return 0, nil })
		}},
		{"list as map", "List", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (map[string]any, error) { return nil, nil })
		}},
		{"map with integer keys", "Map", func(d runtime.Descriptor) runtime.Definition {
			return runtime.Bind(d, func(context.Context, string) (map[int]any, error) { return nil, nil })
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

func TestDirectInvocationContainsAndRedactsPanics(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	descriptor := runtime.Descriptor{Name: "panic", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	if err := registry.Register(runtime.Bind(descriptor, func(context.Context, string) (string, error) { panic("private detail") })); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
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

	registry := runtime.NewRegistry(coreTypes(t))
	descriptor := runtime.Descriptor{Name: "same", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	definition := runtime.Bind(descriptor, func(_ context.Context, value string) (string, error) { return value, nil })
	if err := registry.Register(definition); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := registry.Register(definition); !errors.Is(err, runtime.ErrDuplicateRegistration) {
		t.Fatalf("duplicate Register error = %v", err)
	}
}

func TestSnapshotPropagatesCancellationAndSupportsConcurrentReads(t *testing.T) {
	t.Parallel()

	registry := runtime.NewRegistry(coreTypes(t))
	descriptor := runtime.Descriptor{Name: "wait", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect)}
	definition := runtime.Bind(descriptor, func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err := registry.Register(definition); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}

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

func TestSerialOnlyHandlerDoesNotOverlapAcrossRequests(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
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

func coreTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("freeze core types: %v", err)
	}
	return snapshot
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
