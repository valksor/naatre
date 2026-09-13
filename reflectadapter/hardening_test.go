package reflectadapter_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/reflectadapter"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

var errReflectedSentinel = errors.New("reflected sentinel")

type deepService struct{}

func (deepService) Inspect(_ context.Context, input greetingInput) (string, error) {
	state := "missing"
	if input.Note.Null() {
		state = "null"
	} else if value, ok := input.Note.Value(); ok {
		state = value
	}
	nested := "none"
	if input.Nested != nil {
		nested = input.Nested.Label
	}
	tags := "none"
	if len(input.Tags) == 2 && input.Tags[0] != nil && input.Tags[1] == nil {
		tags = *input.Tags[0] + ":null"
	}
	return state + "/" + nested + "/" + tags, nil
}

type listService struct{}

func (listService) Users(context.Context, greetingInput) ([]*reflectedUser, error) {
	return []*reflectedUser{{Name: "Ada", Secret: "hidden"}, nil}, nil
}

type pointerService struct{}

func (*pointerService) Greet(context.Context, greetingInput) (reflectedUser, error) {
	return reflectedUser{Name: "pointer"}, nil
}

type incompatibleMapService struct{}

func (incompatibleMapService) Greet(context.Context, greetingInput) (map[string]string, error) {
	return map[string]string{"name": "Ada"}, nil
}

type mismatchedScalarInput struct {
	Name int32 `json:"name"`
}

type mismatchedScalarInputService struct{}

func (mismatchedScalarInputService) Greet(context.Context, mismatchedScalarInput) (reflectedUser, error) {
	return reflectedUser{}, nil
}

type collapsedNullableInput struct {
	Name   string       `json:"name"`
	Note   string       `json:"note"`
	Nested *nestedInput `json:"nested"`
	Tags   []*string    `json:"tags"`
}

type collapsedNullableInputService struct{}

func (collapsedNullableInputService) Greet(context.Context, collapsedNullableInput) (reflectedUser, error) {
	return reflectedUser{}, nil
}

type collapsedListElementInput struct {
	Name   string                          `json:"name"`
	Note   reflectadapter.Optional[string] `json:"note"`
	Nested *nestedInput                    `json:"nested"`
	Tags   []string                        `json:"tags"`
}

type collapsedListElementInputService struct{}

func (collapsedListElementInputService) Greet(context.Context, collapsedListElementInput) (reflectedUser, error) {
	return reflectedUser{}, nil
}

type duplicateTagged struct {
	First  func(context.Context, greetingInput) (reflectedUser, error) `naatre:"greet"`
	Second func(context.Context, greetingInput) (reflectedUser, error) `naatre:"greet"`
}

type collisionService struct {
	Tagged func(context.Context, greetingInput) (reflectedUser, error) `naatre:"greet"`
}

func (collisionService) Greet(context.Context, greetingInput) (reflectedUser, error) {
	return reflectedUser{Name: "method"}, nil
}

type taggedA struct {
	Name string `naatre:"name"`
}

type taggedB struct {
	Name string `naatre:"name"`
}

type ambiguousTagged struct {
	taggedA
	taggedB
}

type unexportedTagged struct {
	hidden string `naatre:"name"`
}

var _ = unexportedTagged{hidden: "reflection fixture"}.hidden

type cyclicOutput struct {
	Next *cyclicOutput `naatre:"next"`
}

type cyclicOutputService struct {
	Cycle func(context.Context, greetingInput) (cyclicOutput, error) `naatre:"cycle"`
}

func TestDeepTypedConversionPreservesPresencePointersAndLists(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	inspect := greet
	inspect.Name = "inspect"
	inspect.Output = schema.TypeID(schema.String)
	compiled, err := reflectadapter.Compile(deepService{}, reflectadapter.Options{
		Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Inspect", Descriptor: inspect}},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot := mustFreeze(t, registry)
	for _, test := range []struct {
		raw  string
		want string
	}{
		{`{"name":"A"}`, "missing/none/none"},
		{`{"name":"A","note":null}`, "null/none/none"},
		{`{"name":"A","note":"present","nested":{"label":"deep"},"tags":["x",null]}`, "present/deep/x:null"},
	} {
		value, invokeErr := snapshot.InvokeRoot(context.Background(), protocol.Query, "inspect", mustInput(t, types, "GreetingInput", test.raw))
		if invokeErr != nil || value != test.want {
			t.Fatalf("InvokeRoot(%s) = %#v, %v; want %q", test.raw, value, invokeErr, test.want)
		}
	}
}

func TestDeepTypedOutputConvertsListsPointersAndTaggedStructs(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	users := greet
	users.Name = "users"
	users.Output = "Users"
	compiled, err := reflectadapter.Compile(listService{}, reflectadapter.Options{
		Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Users", Descriptor: users}},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	value, err := mustFreeze(t, registry).InvokeRoot(context.Background(), protocol.Query, "users", mustInput(t, types, "GreetingInput", `{"name":"A"}`))
	if err != nil {
		t.Fatalf("InvokeRoot: %v", err)
	}
	want := []any{map[string]any{"name": "Ada"}, nil}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("output = %#v, want %#v", value, want)
	}
}

func TestHandlerErrorsPropagateWithoutWrapping(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	compiled, err := reflectadapter.Compile(taggedService{
		Greet: func(context.Context, greetingInput) (reflectedUser, error) {
			return reflectedUser{}, errReflectedSentinel
		},
	}, reflectadapter.Options{Types: types, Descriptors: map[string]runtime.Descriptor{"greet": greet}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = mustFreeze(t, registry).InvokeRoot(context.Background(), protocol.Query, "greet", mustInput(t, types, "GreetingInput", `{"name":"A"}`))
	if err == nil {
		t.Fatal("InvokeRoot error = nil, want exact sentinel")
	}
	if got, want := reflect.ValueOf(err).Pointer(), reflect.ValueOf(errReflectedSentinel).Pointer(); got != want {
		t.Fatalf("InvokeRoot error = %v, want exact sentinel", err)
	}
}

func TestCompileValidatesDescriptorsBeforeRegistration(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	valid, _ := reflectionDescriptors()
	tests := []struct {
		name   string
		mutate func(*runtime.Descriptor)
		want   string
	}{
		{name: "unknown input", mutate: func(value *runtime.Descriptor) { value.Input = "Missing" }, want: "unknown"},
		{name: "unknown output", mutate: func(value *runtime.Descriptor) { value.Output = "Missing" }, want: "unknown"},
		{name: "invalid public name", mutate: func(value *runtime.Descriptor) { value.Name = "not valid" }, want: "invalid public name"},
		{name: "missing authorization", mutate: func(value *runtime.Descriptor) { value.Metadata.AuthorizationPolicy = "" }, want: "authorization policy"},
		{name: "write query", mutate: func(value *runtime.Descriptor) { value.Metadata.Effect = runtime.WriteEffect }, want: "query"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor := valid
			test.mutate(&descriptor)
			_, err := reflectadapter.Compile(methodService{calls: new(atomic.Int64)}, reflectadapter.Options{
				Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: descriptor}},
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("Compile error = %v, want containing %q", err, test.want)
			}
		})
	}
	_, err := reflectadapter.Compile(taggedService{Greet: func(context.Context, greetingInput) (reflectedUser, error) { return reflectedUser{}, nil }}, reflectadapter.Options{Types: types})
	if err == nil || !strings.Contains(err.Error(), "missing descriptor") {
		t.Fatalf("missing tag descriptor error = %v", err)
	}
}

func TestCompileRejectsDeterministicCollisionsAndUnsafeFields(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, name := reflectionDescriptors()
	call := func(context.Context, greetingInput) (reflectedUser, error) { return reflectedUser{}, nil }
	tests := []struct {
		name    string
		target  any
		options reflectadapter.Options
		want    string
	}{
		{name: "duplicate tag", target: duplicateTagged{First: call, Second: call}, options: reflectadapter.Options{Types: types, Descriptors: map[string]runtime.Descriptor{"greet": greet}}, want: "ambiguous"},
		{name: "duplicate allowlist", target: methodService{calls: new(atomic.Int64)}, options: reflectadapter.Options{Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}, {GoName: "Greet", Descriptor: greet}}}, want: "duplicated"},
		{name: "duplicate allowlist descriptor", target: duplicateDescriptorService{}, options: reflectadapter.Options{Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}, {GoName: "Salute", Descriptor: greet}}}, want: "collide"},
		{name: "tag and allowlist collision", target: collisionService{Tagged: call}, options: reflectadapter.Options{Types: types, Descriptors: map[string]runtime.Descriptor{"greet": greet}, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}}}, want: "collide"},
		{name: "ambiguous promoted tag", target: ambiguousTagged{}, options: reflectadapter.Options{Types: types, Descriptors: map[string]runtime.Descriptor{"name": name}}, want: "ambiguous"},
		{name: "unexported tagged field", target: unexportedTagged{}, options: reflectadapter.Options{Types: types, Descriptors: map[string]runtime.Descriptor{"name": name}}, want: "unexported"},
		{name: "pointer method on value", target: pointerService{}, options: reflectadapter.Options{Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}}}, want: "pointer/value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := reflectadapter.Compile(test.target, test.options)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("Compile error = %v, want containing %q", err, test.want)
			}
		})
	}

	cycle := greet
	cycle.Name = "cycle"
	cycle.Output = "Cycle"
	_, err := reflectadapter.Compile(cyclicOutputService{Cycle: func(context.Context, greetingInput) (cyclicOutput, error) { return cyclicOutput{}, nil }}, reflectadapter.Options{
		Types: types, Descriptors: map[string]runtime.Descriptor{"cycle": cycle},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cycle") || !strings.Contains(err.Error(), "cyclicOutput") {
		t.Fatalf("cyclic output error = %v", err)
	}
}

func TestCompileRejectsIncompatibleObjectMapOutput(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	_, err := reflectadapter.Compile(incompatibleMapService{}, reflectadapter.Options{
		Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}},
	})
	if err == nil || !strings.Contains(err.Error(), "map[string]interface {}") {
		t.Fatalf("Compile error = %v, want exact map[string]interface {} requirement", err)
	}
}

func TestCompileValidatesTypedInputAgainstSchema(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	for _, test := range []struct {
		name   string
		target any
		path   string
	}{
		{name: "scalar mismatch", target: mismatchedScalarInputService{}, path: "mismatchedScalarInput.Name"},
		{name: "nullable member collapse", target: collapsedNullableInputService{}, path: "collapsedNullableInput.Note"},
		{name: "nullable list element collapse", target: collapsedListElementInputService{}, path: "collapsedListElementInput.Tags element"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := reflectadapter.Compile(test.target, reflectadapter.Options{
				Types: types, Allowlist: []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}},
			})
			if err == nil || !strings.Contains(err.Error(), test.path) {
				t.Fatalf("Compile error = %v, want concrete path %q", err, test.path)
			}
		})
	}
}

func TestCompileSnapshotsTaggedFunctionField(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	service := &taggedService{Greet: func(context.Context, greetingInput) (reflectedUser, error) {
		return reflectedUser{Name: "original"}, nil
	}}
	compiled, err := reflectadapter.Compile(service, reflectadapter.Options{
		Types: types, Descriptors: map[string]runtime.Descriptor{"greet": greet},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	service.Greet = func(context.Context, greetingInput) (reflectedUser, error) {
		return reflectedUser{Name: "mutated"}, nil
	}
	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot := mustFreeze(t, registry)
	input := mustInput(t, types, "GreetingInput", `{"name":"A"}`)
	for _, mutation := range []func(){func() {}, func() { service.Greet = nil }} {
		mutation()
		value, invokeErr := snapshot.InvokeRoot(context.Background(), protocol.Query, "greet", input)
		object, ok := value.(map[string]any)
		if invokeErr != nil || !ok || object["name"] != "original" {
			t.Fatalf("InvokeRoot = %#v, %v; want snapshotted original handler", value, invokeErr)
		}
	}
}

func TestCompileClonesMutableOptions(t *testing.T) {
	t.Parallel()
	types := reflectionTypes(t)
	greet, _ := reflectionDescriptors()
	greet.Capabilities = []string{"vendor.reflect-1"}
	greet.Source = &schema.SourceMetadata{URI: "app://original", Line: 7}
	allowlist := []reflectadapter.Binding{{GoName: "Greet", Descriptor: greet}}
	descriptors := map[string]runtime.Descriptor{"unused": greet}
	compiled, err := reflectadapter.Compile(methodService{calls: new(atomic.Int64)}, reflectadapter.Options{
		Types: types, Descriptors: descriptors, Allowlist: allowlist,
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	allowlist[0].Descriptor.Metadata.AuthorizationPolicy = "mutated"
	allowlist[0].Descriptor.Capabilities[0] = "mutated"
	allowlist[0].Descriptor.Source.URI = "app://mutated"
	descriptors["unused"] = runtime.Descriptor{}

	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got := mustFreeze(t, registry).Descriptors()[0]
	if got.Metadata.AuthorizationPolicy != "greeting.read" || !reflect.DeepEqual(got.Capabilities, []string{"vendor.reflect-1"}) || got.Source == nil || got.Source.URI != "app://original" {
		t.Fatalf("compiled descriptor mutated through options: %#v", got)
	}
}
