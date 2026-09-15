package reflectadapter_test

import (
	"context"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/reflectadapter"
	"github.com/valksor/naatre/runtime"
)

func BenchmarkCompile(b *testing.B) {
	types := reflectionTypes(b)
	greet, _ := reflectionDescriptors()
	service := taggedService{
		Greet: func(_ context.Context, input greetingInput) (reflectedUser, error) {
			return reflectedUser{Name: "hello " + input.Name}, nil
		},
	}
	options := reflectadapter.Options{
		Types: types, Descriptors: map[string]runtime.Descriptor{"greet": greet},
	}

	b.ReportAllocs()
	for b.Loop() {
		compiled, err := reflectadapter.Compile(service, options)
		if err != nil {
			b.Fatal(err)
		}
		if len(compiled.Definitions()) != 1 {
			b.Fatal("unexpected definition count")
		}
	}
}

func BenchmarkInvoke(b *testing.B) {
	types := reflectionTypes(b)
	greet, name := reflectionDescriptors()
	compiled, err := reflectadapter.Compile(taggedService{
		Greet: func(_ context.Context, input greetingInput) (reflectedUser, error) {
			return reflectedUser{Name: "hello " + input.Name}, nil
		},
	}, reflectadapter.Options{
		Types: types, Descriptors: map[string]runtime.Descriptor{"greet": greet},
	})
	if err != nil {
		b.Fatal(err)
	}
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		b.Fatal(err)
	}
	if err := compiled.Register(registry); err != nil {
		b.Fatal(err)
	}
	reflected, err := registry.Freeze()
	if err != nil {
		b.Fatal(err)
	}
	explicit := explicitReflectionSnapshot(b, types, greet, name)
	input := mustInput(b, types, "GreetingInput", `{"name":"Naatre"}`)
	ctx := context.Background()

	for name, snapshot := range map[string]runtime.Snapshot{"reflectadapter": reflected, "explicit": explicit} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				value, invokeErr := snapshot.InvokeRoot(ctx, protocol.Query, "greet", input)
				if invokeErr != nil {
					b.Fatal(invokeErr)
				}
				if value == nil {
					b.Fatal("nil result")
				}
			}
		})
	}
}
