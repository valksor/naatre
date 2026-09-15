package graphqladapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestConsumerRequiresExplicitResolverAndPartialAdaptation(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	client := ClientFunc(func(context.Context, Request) (Response, error) { return Response{}, nil })
	_, report, err := CompileConsumer(ConsumerConfig{
		Schema: imported, Client: client,
		Resolvers: []Resolver{{Operation: "account", Approved: true}},
	})
	if CodeOf(err) != CodeResolverRequired || report.Status != "rejected" {
		t.Fatalf("missing adaptation = %v, report=%#v", err, report)
	}

	_, report, err = CompileConsumer(ConsumerConfig{
		Schema: imported, Client: client,
		Resolvers: []Resolver{{
			Operation: "account", Approved: false,
			Adapt: func(context.Context, MappedResponse) (map[string]any, error) { return nil, nil },
		}},
	})
	if err == nil || report.Status != "rejected" {
		t.Fatalf("unapproved resolver = %v, report=%#v", err, report)
	}
}

func TestConsumerRegistersOnlyNamedResolverAndSanitizesPartialResponse(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	var request Request
	var adapted MappedResponse
	client := ClientFunc(func(_ context.Context, input Request) (Response, error) {
		request = input
		return Response{
			Data: map[string]any{"account": map[string]any{"id": "a-1", "name": "Ada"}},
			Errors: []ResponseError{{
				Message: "bearer-secret at internal.example", Path: []any{"account", "secret"},
				Extensions: map[string]any{"token": "bearer-secret", "stack": "private"},
			}},
		}, nil
	})
	compiled, report, err := CompileConsumer(ConsumerConfig{
		Schema: imported, Client: client,
		Resolvers: []Resolver{{
			Operation: "account", Approved: true,
			Adapt: func(_ context.Context, input MappedResponse) (map[string]any, error) {
				adapted = input
				return input.Data.(map[string]any)["account"].(map[string]any), nil
			},
		}},
	})
	if err != nil || report.Status != "ready" || report.Profile != Profile {
		t.Fatalf("CompileConsumer = %v, report=%#v", err, report)
	}
	types, err := imported.Document().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Root(protocol.Query, "account"); !ok {
		t.Fatal("explicit account resolver was not registered")
	}
	if _, ok := snapshot.Root(protocol.Mutation, "rename"); ok {
		t.Fatal("schema import implicitly registered rename")
	}
	output, err := invokeAccountConsumer(t, imported, snapshot)
	if err != nil || !reflect.DeepEqual(output, map[string]any{"id": "a-1", "name": "Ada"}) {
		t.Fatalf("InvokeRoot = %#v, %v", output, err)
	}
	if !strings.Contains(request.Document, `query Naatre_account($id: ID!, $note: String)`) ||
		!strings.Contains(request.Document, `account(id: $id, note: $note)`) {
		t.Fatalf("upstream request = %#v", request)
	}
	if adapted.Complete || len(adapted.Errors) != 1 || adapted.Errors[0].Code != CodeUpstreamFailed ||
		strings.Contains(adapted.Errors[0].Message, "bearer-secret") {
		t.Fatalf("mapped partial response = %#v", adapted)
	}
	encoded, _ := json.Marshal(adapted)
	if strings.Contains(string(encoded), "internal.example") || strings.Contains(string(encoded), "bearer-secret") || strings.Contains(string(encoded), "private") {
		t.Fatalf("mapped response leaked protected data: %s", encoded)
	}
}

func TestMapResponseBoundsErrorsAndNeverExposesUpstreamDetails(t *testing.T) {
	t.Parallel()
	mapped, err := MapResponse(Response{
		Data:   map[string]any{"safe": true},
		Errors: []ResponseError{{Message: "credential=secret", Path: []any{"safe"}, Extensions: map[string]any{"debug": "stack"}}},
	}, Limits{})
	if err != nil || mapped.Complete || mapped.Errors[0].Message != publicMessage(CodeUpstreamFailed) {
		t.Fatalf("MapResponse = %#v, %v", mapped, err)
	}
	encoded, _ := json.Marshal(mapped)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "stack") {
		t.Fatalf("safe mapping leaked upstream detail: %s", encoded)
	}
	_, err = MapResponse(Response{Errors: []ResponseError{{Path: []any{"x"}}, {Path: []any{"y"}}}}, Limits{MaxErrors: 1})
	if CodeOf(err) != CodeResourceExhausted {
		t.Fatalf("error bound = %v", err)
	}
	_, err = MapResponse(Response{Errors: []ResponseError{{Path: []any{-1}}}}, Limits{})
	if CodeOf(err) != CodeResponseInvalid {
		t.Fatalf("path validation = %v", err)
	}
	_, err = MapResponse(Response{Errors: []ResponseError{{Path: []any{"token=secret"}}}}, Limits{})
	if CodeOf(err) != CodeResponseInvalid || strings.Contains(err.Error(), "secret") {
		t.Fatalf("protected path validation = %v", err)
	}
	_, err = MapResponse(Response{Data: map[string]any{"value": strings.Repeat("x", 64)}}, Limits{MaxResponseBytes: 32})
	if CodeOf(err) != CodeResourceExhausted {
		t.Fatalf("response byte bound = %v", err)
	}
}

func TestRuntimePreservesNaatreErrorPathAndAppliesGraphQLNonNullPropagation(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	principalSeen := false
	runtimeAdapter, err := NewRuntime(RuntimeConfig{
		Schema: imported,
		Execute: func(ctx context.Context, _ *protocol.Request) runtime.Outcome {
			_, principalSeen = runtime.PrincipalFromContext(ctx)
			return runtime.Outcome{
				Data:   map[string]any{"viewer": map[string]any{"id": "a-1", "name": "Ada"}},
				Errors: []runtime.ExecutionError{{Code: "NAME_DENIED", Message: "credential=secret internal.example", Path: []any{"viewer", "name"}}},
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1"})
	response, err := runtimeAdapter.Execute(ctx, []byte(`query Q { viewer: account(id: "a-1") { id name } }`), "Q", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !principalSeen {
		t.Fatal("runtime adapter replaced the authenticated context")
	}
	if response.Data != nil {
		t.Fatalf("non-null root did not bubble to null: %#v", response.Data)
	}
	if len(response.Errors) != 1 || !reflect.DeepEqual(response.Errors[0].Path, []any{"viewer", "name"}) || response.Errors[0].Extensions["code"] != CodeExecutionFailed ||
		strings.Contains(response.Errors[0].Message, "secret") || strings.Contains(response.Errors[0].Message, "internal.example") {
		t.Fatalf("GraphQL error = %#v", response.Errors)
	}

	_, err = runtimeAdapter.Execute(ctx, []byte(`query Q { account(id: "a-1") @auth(role: "admin") { id } }`), "Q", nil)
	if CodeOf(err) != CodeUnsupported {
		t.Fatalf("GraphQL directive became authorization policy: %v", err)
	}
}

func TestRuntimeBoundsExposedResponses(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	runtimeAdapter, err := NewRuntime(RuntimeConfig{
		Schema: imported, Limits: Limits{MaxResponseBytes: 64},
		Execute: func(context.Context, *protocol.Request) runtime.Outcome {
			return runtime.Outcome{Data: map[string]any{"account": strings.Repeat("x", 256)}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtimeAdapter.Execute(context.Background(), []byte(`query Q { account(id: "a-1") { id } }`), "Q", nil)
	if CodeOf(err) != CodeResourceExhausted {
		t.Fatalf("runtime response bound = %v", err)
	}
}

func TestRuntimeSubscriptionIsOrderedAndCancellationBound(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	ctx, cancel := context.WithCancel(context.Background())
	upstream := make(chan runtime.Outcome, 1)
	upstream <- runtime.Outcome{Data: map[string]any{"accountChanged": map[string]any{"id": "a-1", "name": "Ada"}}}
	runtimeAdapter, err := NewRuntime(RuntimeConfig{
		Schema:    imported,
		Execute:   func(context.Context, *protocol.Request) runtime.Outcome { return runtime.Outcome{} },
		Subscribe: func(context.Context, *protocol.Request) (<-chan runtime.Outcome, error) { return upstream, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := runtimeAdapter.Subscribe(cancelled, []byte(`subscription Watch { accountChanged(id: "a-1") { id } }`), "Watch", nil); CodeOf(err) != CodeCancelled {
		t.Fatalf("pre-cancelled subscription = %v", err)
	}
	responses, err := runtimeAdapter.Subscribe(ctx, []byte(`subscription Watch { accountChanged(id: "a-1") { id name } }`), "Watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-responses:
		if response.Data == nil {
			t.Fatalf("subscription response = %#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription did not deliver the ordered event")
	}
	cancel()
	select {
	case _, ok := <-responses:
		if ok {
			t.Fatal("subscription emitted after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription did not stop on cancellation")
	}
}

func TestConsumerTransportFailureUsesStablePublicRuntimeError(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	secret := errors.New("token=secret internal.example")
	compiled, _, err := CompileConsumer(ConsumerConfig{
		Schema: imported,
		Client: ClientFunc(func(context.Context, Request) (Response, error) { return Response{}, secret }),
		Resolvers: []Resolver{{
			Operation: "account", Approved: true,
			Adapt: func(context.Context, MappedResponse) (map[string]any, error) { return nil, nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	types, _ := imported.Document().Snapshot()
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	if err := compiled.Register(registry); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := registry.Freeze()
	_, err = invokeAccountConsumer(t, imported, snapshot)
	var public *runtime.Error
	if !errors.As(err, &public) || public.Code != CodeUpstreamFailed || strings.Contains(public.Message, "secret") || strings.Contains(public.Message, "internal.example") {
		t.Fatalf("transport error = %#v / %v", public, err)
	}
}

func TestConsumerCancellationUsesStableCancellationCode(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	compiled, _, err := CompileConsumer(ConsumerConfig{
		Schema: imported,
		Client: ClientFunc(func(context.Context, Request) (Response, error) { return Response{}, context.Canceled }),
		Resolvers: []Resolver{{
			Operation: "account", Approved: true,
			Adapt: func(context.Context, MappedResponse) (map[string]any, error) { return nil, nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	types, _ := imported.Document().Snapshot()
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	if err := compiled.Register(registry); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := registry.Freeze()
	_, err = invokeAccountConsumer(t, imported, snapshot)
	var public *runtime.Error
	if !errors.As(err, &public) || public.Code != CodeCancelled || public.Message != publicMessage(CodeCancelled) {
		t.Fatalf("cancellation error = %#v / %v", public, err)
	}
}

func TestConsumerAdaptationFailureUsesStablePublicCode(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	compiled, _, err := CompileConsumer(ConsumerConfig{
		Schema: imported,
		Client: ClientFunc(func(context.Context, Request) (Response, error) {
			return Response{Data: map[string]any{"account": map[string]any{"id": "a-1", "name": "Ada"}}}, nil
		}),
		Resolvers: []Resolver{{
			Operation: "account", Approved: true,
			Adapt: func(context.Context, MappedResponse) (map[string]any, error) {
				return nil, errors.New("credential=secret internal.example")
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	types, _ := imported.Document().Snapshot()
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	if err := compiled.Register(registry); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := registry.Freeze()
	_, err = invokeAccountConsumer(t, imported, snapshot)
	var public *runtime.Error
	if !errors.As(err, &public) || public.Code != CodeExecutionFailed || strings.Contains(public.Message, "secret") {
		t.Fatalf("adaptation error = %#v / %v", public, err)
	}
}

func invokeAccountConsumer(t *testing.T, imported ImportedSchema, snapshot runtime.Snapshot) (any, error) {
	t.Helper()
	descriptor := imported.operations["account"]
	types, err := imported.Document().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	input, err := schema.CoerceInput(types, descriptor.Input, json.RawMessage(`{"id":"a-1"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := ImportOperation(context.Background(), []byte(`query Q { account(id: "a-1") { id name } }`), imported, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	selected := executable.Document().Operations()[0].Selections()[0]
	return snapshot.InvokeRoot(context.Background(), protocol.Query, "account", runtime.Invocation{
		Operation: "Q", Selection: selected.Source(), Selected: selected, Input: input,
	})
}
