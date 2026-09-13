package reflectadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

var (
	contextType = reflect.TypeFor[context.Context]()
	errorType   = reflect.TypeFor[error]()
)

type dynamicCall func(context.Context, any, any) (any, error)

type inputShape uint8

const (
	inputSchemaValue inputShape = iota
	inputBoolean
	inputInt32
	inputFloat64
	inputString
)

type outputShape uint8

const (
	outputBoolean outputShape = iota
	outputInt32
	outputFloat64
	outputString
	outputObject
	outputList
	outputTagged
	outputJSON
)

type outputPlan struct {
	shape    outputShape
	nullable bool
	convert  func(any) (any, error)
}

func compileCallable(origin string, callable, _ reflect.Value, receiverFromSource bool, descriptor runtime.Descriptor, types schema.Snapshot) (runtime.Definition, error) {
	typeOf := callable.Type()
	if typeOf.IsVariadic() {
		return runtime.Definition{}, fmt.Errorf("reflectadapter: %s uses an unsupported variadic signature %s", origin, typeOf)
	}
	receiverOffset := 0
	if receiverFromSource {
		receiverOffset = 1
	}
	expectedInputs, err := expectedCallableInputs(descriptor, receiverOffset)
	if err != nil {
		return runtime.Definition{}, fmt.Errorf("reflectadapter: %s: %w", origin, err)
	}
	if typeOf.NumIn() != expectedInputs {
		return runtime.Definition{}, fmt.Errorf("reflectadapter: %s signature %s has %d parameters; descriptor requires %d including context.Context", origin, typeOf, typeOf.NumIn(), expectedInputs)
	}
	if typeOf.In(receiverOffset) != contextType {
		return runtime.Definition{}, fmt.Errorf("reflectadapter: %s signature %s requires context.Context in parameter %d", origin, typeOf, receiverOffset+1)
	}
	if typeOf.NumOut() != 2 || typeOf.Out(1) != errorType || typeOf.Out(0) == errorType {
		return runtime.Definition{}, fmt.Errorf("reflectadapter: %s signature %s must return (result, error) with error last", origin, typeOf)
	}

	var inputConverter valueConverter
	if descriptor.Member == runtime.CallMember {
		inputIndex := receiverOffset + 1
		inputType := typeOf.In(inputIndex)
		inputConverter, err = compileInputConverter(types, descriptor.Input, descriptor.InputNullable, inputType, origin+" input "+inputType.String())
		if err != nil {
			return runtime.Definition{}, err
		}
	}
	output, err := compileOutputPlan(types, descriptor, typeOf.Out(0), origin+" result")
	if err != nil {
		return runtime.Definition{}, err
	}
	var sourceConverter valueConverter
	if receiverFromSource {
		sourceConverter, err = compileSourceConverter(typeOf.In(0), origin+" receiver")
		if err != nil {
			return runtime.Definition{}, err
		}
	}

	call := func(ctx context.Context, source, input any) (any, error) {
		arguments := make([]reflect.Value, 0, typeOf.NumIn())
		if receiverFromSource {
			receiver, convertErr := sourceConverter(source)
			if convertErr != nil {
				return nil, fmt.Errorf("reflectadapter: %s source: %w", origin, convertErr)
			}
			arguments = append(arguments, receiver)
		}
		arguments = append(arguments, reflect.ValueOf(ctx))
		if descriptor.Member == runtime.CallMember {
			converted, convertErr := inputConverter(input)
			if convertErr != nil {
				return nil, fmt.Errorf("reflectadapter: %s input: %w", origin, convertErr)
			}
			arguments = append(arguments, converted)
		}
		outputs := callable.Call(arguments)
		if !outputs[1].IsNil() {
			return nil, outputs[1].Interface().(error)
		}
		return output.convert(outputs[0].Interface())
	}
	return bindDynamic(descriptor, inputRuntimeShape(types, descriptor), output, call)
}

func expectedCallableInputs(descriptor runtime.Descriptor, receiverOffset int) (int, error) {
	switch descriptor.Scope {
	case runtime.RootScope:
		if receiverOffset != 0 || descriptor.Member != runtime.CallMember {
			return 0, fmt.Errorf("root descriptor %q requires a bound root callable", descriptor.Name)
		}
		return 2, nil
	case runtime.ObjectScope:
		switch descriptor.Member {
		case runtime.FieldMember:
			return receiverOffset + 1, nil
		case runtime.CallMember:
			return receiverOffset + 2, nil
		default:
			return 0, fmt.Errorf("object descriptor %q has unknown member kind %q", descriptor.Name, descriptor.Member)
		}
	default:
		return 0, fmt.Errorf("descriptor %q has unknown scope %q", descriptor.Name, descriptor.Scope)
	}
}

func inputRuntimeShape(types schema.Snapshot, descriptor runtime.Descriptor) inputShape {
	input, ok := types.Lookup(descriptor.Input)
	if !ok || input.Kind != schema.ScalarType {
		return inputSchemaValue
	}
	switch schema.ScalarKind(input.ID) {
	case schema.Boolean:
		return inputBoolean
	case schema.Int32:
		return inputInt32
	case schema.Float64:
		return inputFloat64
	case schema.String, schema.ID, schema.Int64, schema.UInt64, schema.BigInt, schema.Decimal, schema.Timestamp, schema.Duration, schema.UUID, schema.Bytes:
		return inputString
	default:
		return inputSchemaValue
	}
}

func bindDynamic(descriptor runtime.Descriptor, input inputShape, output outputPlan, call dynamicCall) (runtime.Definition, error) {
	if descriptor.Scope == runtime.ObjectScope && descriptor.Member == runtime.FieldMember {
		return bindFieldOutput(descriptor, output, call), nil
	}
	switch input {
	case inputBoolean:
		if descriptor.InputNullable {
			return bindCallInput[*bool](descriptor, output, call), nil
		}
		return bindCallInput[bool](descriptor, output, call), nil
	case inputInt32:
		if descriptor.InputNullable {
			return bindCallInput[*int32](descriptor, output, call), nil
		}
		return bindCallInput[int32](descriptor, output, call), nil
	case inputFloat64:
		if descriptor.InputNullable {
			return bindCallInput[*float64](descriptor, output, call), nil
		}
		return bindCallInput[float64](descriptor, output, call), nil
	case inputString:
		if descriptor.InputNullable {
			return bindCallInput[*string](descriptor, output, call), nil
		}
		return bindCallInput[string](descriptor, output, call), nil
	case inputSchemaValue:
		if descriptor.InputNullable {
			return bindCallInput[*schema.InputValue](descriptor, output, call), nil
		}
		return bindCallInput[schema.InputValue](descriptor, output, call), nil
	default:
		return runtime.Definition{}, fmt.Errorf("reflectadapter: descriptor %q has unsupported input shape", descriptor.Name)
	}
}

func bindCallInput[Input any](descriptor runtime.Descriptor, output outputPlan, call dynamicCall) runtime.Definition {
	if descriptor.Scope == runtime.RootScope {
		return bindRootOutput[Input](descriptor, output, call)
	}
	return bindObjectCallOutput[Input](descriptor, output, call)
}

func bindRootOutput[Input any](descriptor runtime.Descriptor, output outputPlan, call dynamicCall) runtime.Definition {
	if output.nullable {
		switch output.shape {
		case outputBoolean:
			return rootDefinition[Input, *bool](descriptor, call)
		case outputInt32:
			return rootDefinition[Input, *int32](descriptor, call)
		case outputFloat64:
			return rootDefinition[Input, *float64](descriptor, call)
		case outputString:
			return rootDefinition[Input, *string](descriptor, call)
		case outputObject:
			return rootDefinition[Input, *map[string]any](descriptor, call)
		case outputList:
			return rootDefinition[Input, *[]any](descriptor, call)
		case outputTagged:
			return rootDefinition[Input, *schema.TaggedValue](descriptor, call)
		case outputJSON:
			return rootDefinition[Input, *json.RawMessage](descriptor, call)
		default:
			panic("reflectadapter: unknown output shape")
		}
	}
	switch output.shape {
	case outputBoolean:
		return rootDefinition[Input, bool](descriptor, call)
	case outputInt32:
		return rootDefinition[Input, int32](descriptor, call)
	case outputFloat64:
		return rootDefinition[Input, float64](descriptor, call)
	case outputString:
		return rootDefinition[Input, string](descriptor, call)
	case outputObject:
		return rootDefinition[Input, map[string]any](descriptor, call)
	case outputList:
		return rootDefinition[Input, []any](descriptor, call)
	case outputTagged:
		return rootDefinition[Input, schema.TaggedValue](descriptor, call)
	case outputJSON:
		return rootDefinition[Input, json.RawMessage](descriptor, call)
	default:
		panic("reflectadapter: unknown output shape")
	}
}

func bindObjectCallOutput[Input any](descriptor runtime.Descriptor, output outputPlan, call dynamicCall) runtime.Definition {
	if output.nullable {
		switch output.shape {
		case outputBoolean:
			return objectCallDefinition[Input, *bool](descriptor, call)
		case outputInt32:
			return objectCallDefinition[Input, *int32](descriptor, call)
		case outputFloat64:
			return objectCallDefinition[Input, *float64](descriptor, call)
		case outputString:
			return objectCallDefinition[Input, *string](descriptor, call)
		case outputObject:
			return objectCallDefinition[Input, *map[string]any](descriptor, call)
		case outputList:
			return objectCallDefinition[Input, *[]any](descriptor, call)
		case outputTagged:
			return objectCallDefinition[Input, *schema.TaggedValue](descriptor, call)
		case outputJSON:
			return objectCallDefinition[Input, *json.RawMessage](descriptor, call)
		default:
			panic("reflectadapter: unknown output shape")
		}
	}
	switch output.shape {
	case outputBoolean:
		return objectCallDefinition[Input, bool](descriptor, call)
	case outputInt32:
		return objectCallDefinition[Input, int32](descriptor, call)
	case outputFloat64:
		return objectCallDefinition[Input, float64](descriptor, call)
	case outputString:
		return objectCallDefinition[Input, string](descriptor, call)
	case outputObject:
		return objectCallDefinition[Input, map[string]any](descriptor, call)
	case outputList:
		return objectCallDefinition[Input, []any](descriptor, call)
	case outputTagged:
		return objectCallDefinition[Input, schema.TaggedValue](descriptor, call)
	case outputJSON:
		return objectCallDefinition[Input, json.RawMessage](descriptor, call)
	default:
		panic("reflectadapter: unknown output shape")
	}
}

func bindFieldOutput(descriptor runtime.Descriptor, output outputPlan, call dynamicCall) runtime.Definition {
	if output.nullable {
		switch output.shape {
		case outputBoolean:
			return fieldDefinition[*bool](descriptor, call)
		case outputInt32:
			return fieldDefinition[*int32](descriptor, call)
		case outputFloat64:
			return fieldDefinition[*float64](descriptor, call)
		case outputString:
			return fieldDefinition[*string](descriptor, call)
		case outputObject:
			return fieldDefinition[*map[string]any](descriptor, call)
		case outputList:
			return fieldDefinition[*[]any](descriptor, call)
		case outputTagged:
			return fieldDefinition[*schema.TaggedValue](descriptor, call)
		case outputJSON:
			return fieldDefinition[*json.RawMessage](descriptor, call)
		default:
			panic("reflectadapter: unknown output shape")
		}
	}
	switch output.shape {
	case outputBoolean:
		return fieldDefinition[bool](descriptor, call)
	case outputInt32:
		return fieldDefinition[int32](descriptor, call)
	case outputFloat64:
		return fieldDefinition[float64](descriptor, call)
	case outputString:
		return fieldDefinition[string](descriptor, call)
	case outputObject:
		return fieldDefinition[map[string]any](descriptor, call)
	case outputList:
		return fieldDefinition[[]any](descriptor, call)
	case outputTagged:
		return fieldDefinition[schema.TaggedValue](descriptor, call)
	case outputJSON:
		return fieldDefinition[json.RawMessage](descriptor, call)
	default:
		panic("reflectadapter: unknown output shape")
	}
}

func rootDefinition[Input, Output any](descriptor runtime.Descriptor, call dynamicCall) runtime.Definition {
	return runtime.Bind[Input, Output](descriptor, func(ctx context.Context, input Input) (Output, error) {
		value, err := call(ctx, nil, input)
		return typedOutput[Output](descriptor, value, err)
	})
}

func fieldDefinition[Output any](descriptor runtime.Descriptor, call dynamicCall) runtime.Definition {
	return runtime.BindField[any, Output](descriptor, func(ctx context.Context, source any) (Output, error) {
		value, err := call(ctx, source, nil)
		return typedOutput[Output](descriptor, value, err)
	})
}

func objectCallDefinition[Input, Output any](descriptor runtime.Descriptor, call dynamicCall) runtime.Definition {
	return runtime.BindCall[any, Input, Output](descriptor, func(ctx context.Context, source any, input Input) (Output, error) {
		value, err := call(ctx, source, input)
		return typedOutput[Output](descriptor, value, err)
	})
}

func typedOutput[Output any](descriptor runtime.Descriptor, value any, err error) (Output, error) {
	if err != nil {
		return *new(Output), err
	}
	typed, ok := value.(Output)
	if !ok {
		return *new(Output), fmt.Errorf("reflectadapter: result for %q compiled as %T but produced %T", descriptor.Name, *new(Output), value)
	}
	return typed, nil
}
