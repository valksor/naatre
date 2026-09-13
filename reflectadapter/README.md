# Reflection adapter

`reflectadapter` is an opt-in migration and prototyping layer. It compiles
explicitly tagged exported fields and explicitly allowlisted exported methods
into normal `runtime.Definition` values at startup.

Prefer `runtime.Bind`, `runtime.BindField`, and `runtime.BindCall` for production
registration. They keep the public surface visible in code and avoid relying on
Go reflection conventions. The adapter is useful when moving an existing Go
service incrementally or reducing prototype boilerplate.

## Compile and register

```go
type Service struct {
	Greet func(context.Context, GreetingInput) (User, error) `naatre:"greet"`
}

compiled, err := reflectadapter.Compile(Service{Greet: greet}, reflectadapter.Options{
	Types: types,
	Descriptors: map[string]runtime.Descriptor{
		"greet": greetDescriptor,
	},
})
if err != nil {
	return err
}
if err := compiled.Register(registry); err != nil {
	return err
}
```

Data fields and function fields require a non-empty `naatre` tag. Ordinary
methods require a `Binding` in `Options.Allowlist`; method names alone never
make them reachable. `Options.Only` can restrict tagged fields by their Go field
name.

Supported handlers accept `context.Context`, then the source and/or typed input
appropriate for their descriptor, and must return `(result, error)` with the
error last. `Optional[T]` distinguishes a missing input member from explicit
null and a present value. Composite values use their JSON field names.

Compilation rejects nil handlers, ambiguous promoted members, unexported tagged
fields, receiver mismatches, variadic or malformed signatures, unsupported or
cyclic Go types, descriptor collisions, and descriptors the runtime registry
would reject. Treat any compilation error as a startup configuration failure.

The compiled value caches member indices, converters, and invocation closures.
It retains no request, input value, principal, or other request-scoped object.
Registration still passes through `runtime.Registry.Register`; freezing and
invocation therefore use the same authorization, resource limits, costs,
scheduling, and output completion as explicit definitions.
