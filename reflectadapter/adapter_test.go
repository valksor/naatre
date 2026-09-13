package reflectadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/reflectadapter"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type greetingInput struct {
	Name   string                          `json:"name"`
	Note   reflectadapter.Optional[string] `json:"note"`
	Nested *nestedInput                    `json:"nested"`
	Tags   []*string                       `json:"tags"`
}

type nestedInput struct {
	Label string `json:"label"`
}

type reflectedUser struct {
	Name   string `json:"name" naatre:"name"`
	Secret string `json:"secret"`
}

type taggedService struct {
	Greet  func(context.Context, greetingInput) (reflectedUser, error) `naatre:"greet"`
	Hidden func(context.Context, greetingInput) (string, error)
}

type methodService struct {
	calls *atomic.Int64
}

func (s methodService) Greet(_ context.Context, input greetingInput) (reflectedUser, error) {
	s.calls.Add(1)
	return reflectedUser{Name: "hello " + input.Name, Secret: "private"}, nil
}

func (methodService) Hidden(context.Context, greetingInput) (string, error) {
	return "unreachable", nil
}

type malformedService struct {
	NoContext func(greetingInput) (string, error)                     `naatre:"greet"`
	Variadic  func(context.Context, ...greetingInput) (string, error) `naatre:"greet"`
	BadError  func(context.Context, greetingInput) (error, string)    `naatre:"greet"`
	Channel   func(context.Context, chan string) (string, error)      `naatre:"greet"`
}

type cyclicInput struct {
	Next *cyclicInput `json:"next"`
}

type cyclicService struct {
	Greet func(context.Context, cyclicInput) (string, error) `naatre:"greet"`
}

type promotedA struct{}

func (promotedA) Greet(context.Context, greetingInput) (reflectedUser, error) {
	return reflectedUser{}, nil
}

type promotedB struct{}

func (promotedB) Greet(context.Context, greetingInput) (reflectedUser, error) {
	return reflectedUser{}, nil
}

type ambiguousService struct {
	promotedA
	promotedB
}

type duplicateDescriptorService struct{}

func (duplicateDescriptorService) Greet(context.Context, greetingInput) (reflectedUser, error) {
	return reflectedUser{}, nil
}

func (duplicateDescriptorService) Salute(context.Context, greetingInput) (reflectedUser, error) {
	return reflectedUser{}, nil
}

type objectMethodSource struct {
	Name string `json:"name"`
}

func (source objectMethodSource) FieldName(context.Context) (string, error) {
	return source.Name, nil
}

func (source objectMethodSource) Greeting(_ context.Context, input greetingInput) (string, error) {
	return "hello " + source.Name + " from " + input.Name, nil
}

var (
	_ = promotedA{}.Greet
	_ = promotedB{}.Greet
	_ = ambiguousService{promotedA: promotedA{}, promotedB: promotedB{}}
)

func TestCompileTaggedFieldsAndFunctionFieldsMatchesExplicitRegistry(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, name := reflectionDescriptors()
	service := taggedService{
		Greet: func(_ context.Context, input greetingInput) (reflectedUser, error) {
			if input.Name == "error" {
				return reflectedUser{}, errReflectedSentinel
			}
			return reflectedUser{Name: "hello " + input.Name, Secret: "private"}, nil
		},
		Hidden: func(context.Context, greetingInput) (string, error) { return "hidden", nil },
	}

	compiledService, err := reflectadapter.Compile(service, reflectadapter.Options{
		Types: types, Descriptors: map[string]runtime.Descriptor{"greet": greet},
	})
	if err != nil {
		t.Fatalf("Compile service: %v", err)
	}
	compiledUser, err := reflectadapter.Compile((*reflectedUser)(nil), reflectadapter.Options{
		Types: types, Descriptors: map[string]runtime.Descriptor{"name": name},
	})
	if err != nil {
		t.Fatalf("Compile user: %v", err)
	}
	if len(compiledService.Definitions()) != 1 || len(compiledUser.Definitions()) != 1 {
		t.Fatalf("compiled definitions = %d service, %d user", len(compiledService.Definitions()), len(compiledUser.Definitions()))
	}

	reflected := runtime.NewRegistry(types)
	mustRegisterCompiled(t, reflected, compiledService, compiledUser)
	reflectedSnapshot := mustFreeze(t, reflected)
	explicitSnapshot := explicitReflectionSnapshot(t, types, greet, name)
	if !reflect.DeepEqual(reflectedSnapshot.Descriptors(), explicitSnapshot.Descriptors()) {
		t.Fatalf("descriptor parity mismatch\nreflected: %#v\nexplicit: %#v", reflectedSnapshot.Descriptors(), explicitSnapshot.Descriptors())
	}
	if !reflect.DeepEqual(mustExportSchema(t, reflectedSnapshot), mustExportSchema(t, explicitSnapshot)) {
		t.Fatal("exported schema parity mismatch")
	}

	input := mustInput(t, types, "GreetingInput", `{"name":"Naatre"}`)
	value, err := reflectedSnapshot.InvokeRoot(context.Background(), protocol.Query, "greet", input)
	if err != nil {
		t.Fatalf("InvokeRoot: %v", err)
	}
	object, ok := value.(map[string]any)
	if !ok || object["name"] != "hello Naatre" {
		t.Fatalf("reflected result = %#v", value)
	}
	if _, exists := object["secret"]; exists {
		t.Fatalf("untagged output field was exposed: %#v", object)
	}
	field, err := reflectedSnapshot.InvokeField(context.Background(), "User", "name", object)
	if err != nil || field != "hello Naatre" {
		t.Fatalf("InvokeField = %#v, %v", field, err)
	}
	if _, err := reflectedSnapshot.InvokeRoot(context.Background(), protocol.Query, "hidden", input); !errors.Is(err, runtime.ErrNotRegistered) {
		t.Fatalf("untagged function field error = %v", err)
	}
	explicitValue, explicitErr := explicitSnapshot.InvokeRoot(context.Background(), protocol.Query, "greet", input)
	if explicitErr != nil || !reflect.DeepEqual(value, explicitValue) {
		t.Fatalf("runtime parity = reflected %#v/%v, explicit %#v/%v", value, err, explicitValue, explicitErr)
	}
	errorInput := mustInput(t, types, "GreetingInput", `{"name":"error"}`)
	_, reflectedErr := reflectedSnapshot.InvokeRoot(context.Background(), protocol.Query, "greet", errorInput)
	_, explicitErr = explicitSnapshot.InvokeRoot(context.Background(), protocol.Query, "greet", errorInput)
	if reflectedErr != errReflectedSentinel || explicitErr != errReflectedSentinel { //nolint:errorlint // Exact identity parity is required.
		t.Fatalf("error parity = reflected %v, explicit %v; want exact sentinel", reflectedErr, explicitErr)
	}
}

func TestCompileAllowlistedMethodAndPreservesRuntimeGuards(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	var calls atomic.Int64
	compiled, err := reflectadapter.Compile(methodService{calls: &calls}, reflectadapter.Options{
		Types:     types,
		Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, name := reflectionDescriptors()
	compiledUser, err := reflectadapter.Compile((*reflectedUser)(nil), reflectadapter.Options{
		Types: types, Descriptors: map[string]runtime.Descriptor{"name": name},
	})
	if err != nil {
		t.Fatalf("Compile user: %v", err)
	}
	if err := compiledUser.Register(registry); err != nil {
		t.Fatalf("Register user: %v", err)
	}
	snapshot := mustFreeze(t, registry)
	if _, err := snapshot.InvokeRoot(context.Background(), protocol.Query, "hidden", mustInput(t, types, "GreetingInput", `{"name":"x"}`)); !errors.Is(err, runtime.ErrNotRegistered) {
		t.Fatalf("non-allowlisted method error = %v", err)
	}

	request := mustRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"greet","args":{"name":{"$literal":"Ada"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`)
	if _, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxStaticCost: 13}}); !hasValidationCode(err, "LIMIT_STATIC_COST") {
		t.Fatalf("cost limit error = %#v", err)
	}
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Limits: runtime.ResourceLimits{MaxStaticCost: 14}})
	if err != nil {
		t.Fatalf("PrepareWithOptions: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if calls.Load() != 0 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeUnauthorized {
		t.Fatalf("authorization bypass: calls=%d outcome=%#v", calls.Load(), outcome)
	}
}

func TestCompileAllowlistedObjectFieldAndCallMethods(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	call, field := reflectionDescriptors()
	field.Owner = "UserMethods"
	field.Name = "name"
	call.Scope = runtime.ObjectScope
	call.Owner = "UserMethods"
	call.Kind = ""
	call.Member = runtime.CallMember
	call.Name = "greeting"
	call.Output = schema.TypeID(schema.String)
	compiled, err := reflectadapter.Compile(objectMethodSource{}, reflectadapter.Options{
		Types: types,
		Allowlist: []reflectadapter.Binding{
			{GoName: "FieldName", Descriptor: field},
			{GoName: "Greeting", Descriptor: call},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot := mustFreeze(t, registry)
	source := map[string]any{"name": "Ada"}
	fieldValue, err := snapshot.InvokeField(context.Background(), "UserMethods", "name", source)
	if err != nil || fieldValue != "Ada" {
		t.Fatalf("InvokeField = %#v, %v", fieldValue, err)
	}
	input := mustInput(t, types, "GreetingInput", `{"name":"Naatre"}`)
	callValue, err := snapshot.InvokeCall(context.Background(), "UserMethods", "greeting", source, input)
	if err != nil || callValue != "hello Ada from Naatre" {
		t.Fatalf("InvokeCall = %#v, %v", callValue, err)
	}
}

func TestCompiledAdapterIsConcurrentAndImmutable(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	var calls atomic.Int64
	compiled, err := reflectadapter.Compile(methodService{calls: &calls}, reflectadapter.Options{
		Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	definitions := compiled.Definitions()
	definitions[0] = runtime.Definition{}
	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot := mustFreeze(t, registry)
	input := mustInput(t, types, "GreetingInput", `{"name":"parallel"}`)

	const workers = 64
	var wait sync.WaitGroup
	errorsFound := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, invokeErr := snapshot.InvokeRoot(context.Background(), protocol.Query, "greet", input)
			if invokeErr != nil {
				errorsFound <- invokeErr
				return
			}
			if value.(map[string]any)["name"] != "hello parallel" {
				errorsFound <- errors.New("unexpected concurrent result")
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for invokeErr := range errorsFound {
		t.Error(invokeErr)
	}
	if calls.Load() != workers {
		t.Fatalf("calls = %d, want %d", calls.Load(), workers)
	}
}

func TestCompileRejectsUnsafeReflectionShapes(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	cyclicGreet := greet
	cyclicGreet.Input = "CyclicInput"
	badErrorType := reflect.TypeFor[func(context.Context, greetingInput) (error, string)]()
	badError := reflect.MakeFunc(badErrorType, func([]reflect.Value) []reflect.Value {
		return []reflect.Value{reflect.Zero(reflect.TypeFor[error]()), reflect.ValueOf("")}
	}).Interface().(func(context.Context, greetingInput) (error, string))
	tests := []struct {
		name    string
		target  any
		options reflectadapter.Options
		want    string
		path    string
	}{
		{name: "nil target", target: nil, options: reflectadapter.Options{Types: types}, want: "target"},
		{name: "nil tagged function", target: taggedService{}, options: reflectadapter.Options{Types: types, Descriptors: map[string]runtime.Descriptor{"greet": greet}}, want: "nil"},
		{name: "missing context", target: malformedService{NoContext: func(greetingInput) (string, error) { return "", nil }}, options: taggedOption(types, greet, "NoContext"), want: "context.Context", path: "malformedService.NoContext"},
		{name: "variadic", target: malformedService{Variadic: func(context.Context, ...greetingInput) (string, error) { return "", nil }}, options: taggedOption(types, greet, "Variadic"), want: "variadic"},
		{name: "bad error position", target: malformedService{BadError: badError}, options: taggedOption(types, greet, "BadError"), want: "error"},
		{name: "unsupported channel", target: malformedService{Channel: func(context.Context, chan string) (string, error) { return "", nil }}, options: taggedOption(types, greet, "Channel"), want: "unsupported"},
		{name: "cyclic input", target: cyclicService{Greet: func(context.Context, cyclicInput) (string, error) { return "", nil }}, options: reflectadapter.Options{Types: types, Descriptors: map[string]runtime.Descriptor{"greet": cyclicGreet}}, want: "cycle", path: "cyclicInput.Next"},
		{name: "ambiguous promoted method", target: ambiguousService{}, options: reflectadapter.Options{Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}}}, want: "ambiguous"},
		{name: "unknown member", target: methodService{}, options: reflectadapter.Options{Types: types, Allowlist: []reflectadapter.Binding{{GoName: "missing", Descriptor: greet}}}, want: "exported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := reflectadapter.Compile(test.target, test.options)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("Compile error = %v, want containing %q", err, test.want)
			}
			if test.path != "" && !strings.Contains(err.Error(), test.path) {
				t.Fatalf("Compile error = %v, want concrete Go path %q", err, test.path)
			}
		})
	}
}

func reflectionTypes(t testing.TB) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "GreetingInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"name":   {Type: schema.TypeID(schema.String), Required: true},
			"note":   {Type: schema.TypeID(schema.String), Nullable: true},
			"nested": {Type: "NestedInput", Nullable: true},
			"tags":   {Type: "StringList"},
		}},
		{ID: "NestedInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"label": {Type: schema.TypeID(schema.String), Required: true},
		}},
		{ID: "CyclicInput", Kind: schema.InputObjectType, Input: true, MaxDepth: 4, Fields: map[string]schema.FieldDescriptor{
			"next": {Type: "CyclicInput", Nullable: true},
		}},
		{ID: "StringList", Kind: schema.ListType, Input: true, Element: schema.TypeID(schema.String), ElementNullable: true},
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "UserMethods", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name":     {Type: schema.TypeID(schema.String)},
			"greeting": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Users", Kind: schema.ListType, Output: true, Element: "User", ElementNullable: true},
		{ID: "Cycle", Kind: schema.ObjectType, Output: true, MaxDepth: 2, Fields: map[string]schema.FieldDescriptor{
			"next": {Type: "Cycle", Nullable: true},
		}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("Register schema %s: %v", descriptor.ID, err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("Freeze schema: %v", err)
	}
	return types
}

func reflectionDescriptors() (runtime.Descriptor, runtime.Descriptor) {
	metadata := runtime.Metadata{
		Effect: runtime.ReadEffect, Deterministic: true, Cacheable: true, RetrySafe: true,
		ThreadSafety: runtime.ThreadSafe, Batching: runtime.BatchIneligible, Transaction: runtime.TransactionNone,
		AuthorizationPolicy: "greeting.read", Cost: 7,
	}
	return runtime.Descriptor{
		Name: "greet", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "GreetingInput", Output: "User", Metadata: metadata,
	}, runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: metadata,
	}
}

func explicitReflectionSnapshot(t testing.TB, types schema.Snapshot, greet, name runtime.Descriptor) runtime.Snapshot {
	t.Helper()
	registry := runtime.NewRegistry(types)
	if err := registry.Register(runtime.Bind[schema.InputValue, map[string]any](greet, func(_ context.Context, input schema.InputValue) (map[string]any, error) {
		raw, err := input.MarshalJSON()
		if err != nil {
			return nil, err
		}
		var decoded greetingInput
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
		if decoded.Name == "error" {
			return nil, errReflectedSentinel
		}
		return map[string]any{"name": "hello " + decoded.Name}, nil
	})); err != nil {
		t.Fatalf("Register explicit root: %v", err)
	}
	if err := registry.Register(runtime.BindField[map[string]any, string](name, func(_ context.Context, source map[string]any) (string, error) {
		return source["name"].(string), nil
	})); err != nil {
		t.Fatalf("Register explicit field: %v", err)
	}
	return mustFreeze(t, registry)
}

func mustRegisterCompiled(t *testing.T, registry *runtime.Registry, compiled ...*reflectadapter.Compiled) {
	t.Helper()
	for _, item := range compiled {
		if err := item.Register(registry); err != nil {
			t.Fatalf("Register compiled: %v", err)
		}
	}
}

func mustFreeze(t testing.TB, registry *runtime.Registry) runtime.Snapshot {
	t.Helper()
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze registry: %v", err)
	}
	return snapshot
}

func mustInput(t testing.TB, types schema.Snapshot, typeID schema.TypeID, raw string) schema.InputValue {
	t.Helper()
	input, err := schema.CoerceInput(types, typeID, []byte(raw), false)
	if err != nil {
		t.Fatalf("CoerceInput: %v", err)
	}
	return input
}

func mustExportSchema(t *testing.T, snapshot runtime.Snapshot) schema.Document {
	t.Helper()
	document, err := snapshot.ExportSchema(schema.ExportOptions{Revision: "schema-1"})
	if err != nil {
		t.Fatalf("ExportSchema: %v", err)
	}
	return document
}

func mustRequest(t *testing.T, raw string) *protocol.Request {
	t.Helper()
	request, err := protocol.DecodeRequest([]byte(raw), protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return request
}

func hasValidationCode(err error, code string) bool {
	var validation *runtime.ValidationErrors
	return errors.As(err, &validation) && slices.ContainsFunc(validation.Issues(), func(issue runtime.ValidationIssue) bool {
		return issue.Diagnostic.Code == code
	})
}

func taggedOption(types schema.Snapshot, descriptor runtime.Descriptor, only string) reflectadapter.Options {
	descriptor.Name = "greet"
	descriptor.Output = schema.TypeID(schema.String)
	return reflectadapter.Options{
		Types:       types,
		Descriptors: map[string]runtime.Descriptor{"greet": descriptor},
		Only:        []string{only},
	}
}
