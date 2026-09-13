package reflectadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type valueConverter func(any) (reflect.Value, error)

func compileInputConverter(types schema.Snapshot, typeID schema.TypeID, nullable bool, target reflect.Type, path string) (valueConverter, error) {
	descriptor, ok := types.Lookup(typeID)
	if !ok || !descriptor.Input {
		return nil, fmt.Errorf("reflectadapter: %s references unknown or non-input schema type %q", path, typeID)
	}
	if nullable && target.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("reflectadapter: %s type %s must be a pointer for nullable schema %s", path, target, typeID)
	}
	if !nullable && target.Kind() == reflect.Pointer {
		return nil, fmt.Errorf("reflectadapter: %s type %s cannot be a pointer for non-null schema %s", path, target, typeID)
	}
	shape := target
	if nullable {
		shape = shape.Elem()
	}
	if err := validateInputShape(types, descriptor, shape, path, make(map[inputVisit]string)); err != nil {
		return nil, err
	}
	return jsonValueConverter(target, path), nil
}

func compileSourceConverter(target reflect.Type, path string) (valueConverter, error) {
	if err := validateGoShape(target, path, make(map[reflect.Type]string)); err != nil {
		return nil, err
	}
	return jsonValueConverter(target, path), nil
}

func jsonValueConverter(target reflect.Type, path string) valueConverter {
	return func(input any) (reflect.Value, error) {
		if input == nil {
			if target.Kind() == reflect.Pointer {
				return reflect.Zero(target), nil
			}
			return reflect.Value{}, fmt.Errorf("%s cannot decode null into %s", path, target)
		}
		value := reflect.ValueOf(input)
		if value.Type().AssignableTo(target) {
			return value, nil
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("encode %T for %s: %w", input, target, err)
		}
		result := reflect.New(target)
		if err := json.Unmarshal(encoded, result.Interface()); err != nil {
			return reflect.Value{}, fmt.Errorf("decode %s into %s: %w", path, target, err)
		}
		return result.Elem(), nil
	}
}

func compileOutputPlan(types schema.Snapshot, descriptor runtime.Descriptor, actual reflect.Type, path string) (outputPlan, error) {
	schemaType, ok := types.Lookup(descriptor.Output)
	if !ok || !schemaType.Output {
		return outputPlan{}, fmt.Errorf("reflectadapter: %s references unknown or non-output schema type %q", path, descriptor.Output)
	}
	if descriptor.OutputNullable != (actual.Kind() == reflect.Pointer) {
		return outputPlan{}, fmt.Errorf("reflectadapter: %s type %s pointer shape does not match nullable=%t for schema %s", path, actual, descriptor.OutputNullable, descriptor.Output)
	}
	base := actual
	if descriptor.OutputNullable {
		base = base.Elem()
	}
	if err := validateGoShape(base, path, make(map[reflect.Type]string)); err != nil {
		return outputPlan{}, err
	}
	shape, err := schemaOutputShape(schemaType)
	if err != nil {
		return outputPlan{}, fmt.Errorf("reflectadapter: %s: %w", path, err)
	}
	encoder, err := compileOutputEncoder(types, schemaType, base, path, make(map[outputVisit]bool))
	if err != nil {
		return outputPlan{}, err
	}
	plan := outputPlan{shape: shape, nullable: descriptor.OutputNullable}
	plan.convert = func(value any) (any, error) {
		current := reflect.ValueOf(value)
		if descriptor.OutputNullable {
			if !current.IsValid() || current.IsNil() {
				return nilOutput(shape), nil
			}
			current = current.Elem()
		}
		converted, convertErr := encoder(current)
		if convertErr != nil {
			return nil, convertErr
		}
		if descriptor.OutputNullable {
			return pointerOutput(shape, converted)
		}
		return converted, nil
	}
	return plan, nil
}

func schemaOutputShape(descriptor schema.TypeDescriptor) (outputShape, error) {
	if descriptor.Kind == schema.ScalarType {
		switch schema.ScalarKind(descriptor.ID) {
		case schema.Boolean:
			return outputBoolean, nil
		case schema.Int32:
			return outputInt32, nil
		case schema.Float64:
			return outputFloat64, nil
		case schema.String, schema.ID, schema.Int64, schema.UInt64, schema.BigInt, schema.Decimal, schema.Timestamp, schema.Duration, schema.UUID, schema.Bytes:
			return outputString, nil
		default:
			return outputJSON, nil
		}
	}
	switch descriptor.Kind {
	case schema.ObjectType, schema.MapType:
		return outputObject, nil
	case schema.ListType:
		return outputList, nil
	case schema.EnumType:
		return outputString, nil
	case schema.UnionType, schema.InterfaceType:
		return outputTagged, nil
	case schema.ScalarType, schema.InputObjectType, schema.OneOfType:
		return 0, fmt.Errorf("schema type %s is not an output shape", descriptor.ID)
	default:
		return 0, fmt.Errorf("schema type %s has unknown kind %q", descriptor.ID, descriptor.Kind)
	}
}

type outputEncoder func(reflect.Value) (any, error)

type outputVisit struct {
	schema schema.TypeID
	goType reflect.Type
}

func compileOutputEncoder(types schema.Snapshot, descriptor schema.TypeDescriptor, actual reflect.Type, path string, active map[outputVisit]bool) (outputEncoder, error) {
	visit := outputVisit{schema: descriptor.ID, goType: actual}
	if active[visit] {
		return nil, fmt.Errorf("reflectadapter: %s contains unsupported output cycle through %s", path, actual)
	}
	active[visit] = true
	defer delete(active, visit)

	shape, err := schemaOutputShape(descriptor)
	if err != nil {
		return nil, err
	}
	switch shape {
	case outputBoolean:
		return exactEncoder(actual, reflect.TypeFor[bool](), path)
	case outputInt32:
		return exactEncoder(actual, reflect.TypeFor[int32](), path)
	case outputFloat64:
		return exactEncoder(actual, reflect.TypeFor[float64](), path)
	case outputString:
		if actual.Kind() != reflect.String {
			return nil, fmt.Errorf("reflectadapter: %s type %s must be string-like for schema %s", path, actual, descriptor.ID)
		}
		return func(value reflect.Value) (any, error) {
			return value.Convert(reflect.TypeFor[string]()).Interface(), nil
		}, nil
	case outputJSON:
		return exactEncoder(actual, reflect.TypeFor[json.RawMessage](), path)
	case outputTagged:
		return exactEncoder(actual, reflect.TypeFor[schema.TaggedValue](), path)
	case outputList:
		if actual.Kind() != reflect.Slice && actual.Kind() != reflect.Array {
			return nil, fmt.Errorf("reflectadapter: %s type %s must be a slice or array for schema %s", path, actual, descriptor.ID)
		}
		element, ok := types.Lookup(descriptor.Element)
		if !ok {
			return nil, fmt.Errorf("reflectadapter: %s schema %s has unknown element %s", path, descriptor.ID, descriptor.Element)
		}
		elementType := actual.Elem()
		if descriptor.ElementNullable {
			if elementType.Kind() != reflect.Pointer {
				return nil, fmt.Errorf("reflectadapter: %s element %s must be a pointer for nullable schema element", path, elementType)
			}
			elementType = elementType.Elem()
		}
		elementEncoder, compileErr := compileOutputEncoder(types, element, elementType, path+" element", active)
		if compileErr != nil {
			return nil, compileErr
		}
		return listEncoder(elementEncoder, descriptor.ElementNullable), nil
	case outputObject:
		if descriptor.Kind == schema.MapType {
			return compileMapEncoder(types, descriptor, actual, path, active)
		}
		return compileStructEncoder(types, descriptor, actual, path, active)
	default:
		return nil, fmt.Errorf("reflectadapter: %s has unsupported output shape", path)
	}
}

func exactEncoder(actual, expected reflect.Type, path string) (outputEncoder, error) {
	if actual != expected {
		return nil, fmt.Errorf("reflectadapter: %s type %s must be %s", path, actual, expected)
	}
	return func(value reflect.Value) (any, error) { return value.Interface(), nil }, nil
}

func compileStructEncoder(types schema.Snapshot, descriptor schema.TypeDescriptor, actual reflect.Type, path string, active map[outputVisit]bool) (outputEncoder, error) {
	if actual.Kind() == reflect.Map && actual.Key().Kind() == reflect.String {
		if actual != reflect.TypeFor[map[string]any]() {
			return nil, fmt.Errorf("reflectadapter: %s type %s must be map[string]interface {} for schema object %s", path, actual, descriptor.ID)
		}
		return func(value reflect.Value) (any, error) { return value.Interface(), nil }, nil
	}
	if actual.Kind() != reflect.Struct {
		return nil, fmt.Errorf("reflectadapter: %s type %s must be a tagged struct or string-keyed map for schema %s", path, actual, descriptor.ID)
	}
	type fieldPlan struct {
		name     string
		index    []int
		nullable bool
		encode   outputEncoder
	}
	plans := make([]fieldPlan, 0, len(descriptor.Fields))
	seen := make(map[string]bool)
	for _, field := range reflect.VisibleFields(actual) {
		name, tagged := field.Tag.Lookup(tagName)
		if !tagged || name == "-" {
			continue
		}
		if field.PkgPath != "" {
			return nil, fmt.Errorf("reflectadapter: %s tagged output field %s is unexported", path, field.Name)
		}
		name, err := parseTag(name)
		if err != nil {
			return nil, fmt.Errorf("reflectadapter: %s field %s: %w", path, field.Name, err)
		}
		if seen[name] {
			return nil, fmt.Errorf("reflectadapter: %s has ambiguous tagged output field %q", path, name)
		}
		fieldDescriptor, ok := descriptor.Fields[name]
		if !ok {
			return nil, fmt.Errorf("reflectadapter: %s field %s tag %q is absent from schema %s", path, field.Name, name, descriptor.ID)
		}
		fieldType := field.Type
		if fieldDescriptor.Nullable {
			if fieldType.Kind() != reflect.Pointer {
				return nil, fmt.Errorf("reflectadapter: %s field %s must be a pointer for nullable schema field %s", path, field.Name, name)
			}
			fieldType = fieldType.Elem()
		}
		fieldSchema, ok := types.Lookup(fieldDescriptor.Type)
		if !ok {
			return nil, fmt.Errorf("reflectadapter: %s field %s references unknown schema %s", path, field.Name, fieldDescriptor.Type)
		}
		encoder, compileErr := compileOutputEncoder(types, fieldSchema, fieldType, path+"."+field.Name, active)
		if compileErr != nil {
			return nil, compileErr
		}
		seen[name] = true
		plans = append(plans, fieldPlan{name: name, index: field.Index, nullable: fieldDescriptor.Nullable, encode: encoder})
	}
	for name := range descriptor.Fields {
		if !seen[name] {
			return nil, fmt.Errorf("reflectadapter: %s has no exported %s-tagged field for schema field %q", path, tagName, name)
		}
	}
	return func(value reflect.Value) (any, error) {
		result := make(map[string]any, len(plans))
		for _, plan := range plans {
			field := value.FieldByIndex(plan.index)
			if plan.nullable {
				if field.IsNil() {
					result[plan.name] = nil
					continue
				}
				field = field.Elem()
			}
			converted, convertErr := plan.encode(field)
			if convertErr != nil {
				return nil, fmt.Errorf("field %s: %w", plan.name, convertErr)
			}
			result[plan.name] = converted
		}
		return result, nil
	}, nil
}

func compileMapEncoder(types schema.Snapshot, descriptor schema.TypeDescriptor, actual reflect.Type, path string, active map[outputVisit]bool) (outputEncoder, error) {
	if actual.Kind() != reflect.Map || actual.Key().Kind() != reflect.String {
		return nil, fmt.Errorf("reflectadapter: %s type %s must be a string-keyed map for schema %s", path, actual, descriptor.ID)
	}
	element, ok := types.Lookup(descriptor.Element)
	if !ok {
		return nil, fmt.Errorf("reflectadapter: %s schema %s has unknown map element %s", path, descriptor.ID, descriptor.Element)
	}
	elementType := actual.Elem()
	if descriptor.ElementNullable {
		if elementType.Kind() != reflect.Pointer {
			return nil, fmt.Errorf("reflectadapter: %s map element %s must be a pointer for nullable schema element", path, elementType)
		}
		elementType = elementType.Elem()
	}
	elementEncoder, err := compileOutputEncoder(types, element, elementType, path+" element", active)
	if err != nil {
		return nil, err
	}
	return func(value reflect.Value) (any, error) {
		result := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			item := iterator.Value()
			if descriptor.ElementNullable {
				if item.IsNil() {
					result[iterator.Key().String()] = nil
					continue
				}
				item = item.Elem()
			}
			converted, convertErr := elementEncoder(item)
			if convertErr != nil {
				return nil, convertErr
			}
			result[iterator.Key().String()] = converted
		}
		return result, nil
	}, nil
}

func listEncoder(element outputEncoder, nullable bool) outputEncoder {
	return func(value reflect.Value) (any, error) {
		result := make([]any, value.Len())
		for index := range value.Len() {
			item := value.Index(index)
			if nullable {
				if item.IsNil() {
					continue
				}
				item = item.Elem()
			}
			converted, err := element(item)
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", index, err)
			}
			result[index] = converted
		}
		return result, nil
	}
}

func compileDataField(origin string, targetType reflect.Type, field taggedField, descriptor runtime.Descriptor, types schema.Snapshot) (runtime.Definition, error) {
	output, err := compileOutputPlan(types, descriptor, field.field.Type, origin)
	if err != nil {
		return runtime.Definition{}, err
	}
	sourceConverter, err := compileSourceConverter(targetType, origin+" source")
	if err != nil {
		return runtime.Definition{}, err
	}
	call := func(_ context.Context, source, _ any) (any, error) {
		converted, convertErr := sourceConverter(source)
		if convertErr != nil {
			return nil, convertErr
		}
		fieldValue := converted.FieldByIndex(field.index)
		return output.convert(fieldValue.Interface())
	}
	return bindDynamic(descriptor, inputSchemaValue, output, call)
}

func validateGoShape(current reflect.Type, path string, active map[reflect.Type]string) error {
	if current == nil {
		return fmt.Errorf("reflectadapter: %s has no Go type", path)
	}
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	if prior, exists := active[current]; exists {
		return fmt.Errorf("reflectadapter: %s contains unsupported type cycle through %s first seen at %s", path, current, prior)
	}
	switch current.Kind() {
	case reflect.Bool, reflect.String, reflect.Int32, reflect.Float64:
		return nil
	case reflect.Struct:
		if current == reflect.TypeFor[schema.InputValue]() || current == reflect.TypeFor[schema.TaggedValue]() {
			return nil
		}
		active[current] = path
		defer delete(active, current)
		for index := range current.NumField() {
			field := current.Field(index)
			if field.PkgPath != "" {
				if _, tagged := field.Tag.Lookup(tagName); tagged {
					return fmt.Errorf("reflectadapter: %s.%s is a tagged unexported field", path, field.Name)
				}
				continue
			}
			if err := validateGoShape(field.Type, path+"."+field.Name, active); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		if current == reflect.TypeFor[json.RawMessage]() {
			return nil
		}
		active[current] = path
		defer delete(active, current)
		return validateGoShape(current.Elem(), path+"[]", active)
	case reflect.Array:
		active[current] = path
		defer delete(active, current)
		return validateGoShape(current.Elem(), path+"[]", active)
	case reflect.Map:
		if current.Key().Kind() != reflect.String {
			return fmt.Errorf("reflectadapter: %s has unsupported map key %s; only string keys are allowed", path, current.Key())
		}
		active[current] = path
		defer delete(active, current)
		return validateGoShape(current.Elem(), path+" value", active)
	case reflect.Interface:
		if current.NumMethod() == 0 {
			return nil
		}
	case reflect.Invalid,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Complex64, reflect.Complex128,
		reflect.Chan, reflect.Func, reflect.Pointer, reflect.UnsafePointer:
		// Rejected below with the same actionable type diagnostic.
	}
	return fmt.Errorf("reflectadapter: %s has unsupported Go type %s (%s)", path, current, current.Kind())
}

func nilOutput(shape outputShape) any {
	switch shape {
	case outputBoolean:
		return (*bool)(nil)
	case outputInt32:
		return (*int32)(nil)
	case outputFloat64:
		return (*float64)(nil)
	case outputString:
		return (*string)(nil)
	case outputObject:
		return (*map[string]any)(nil)
	case outputList:
		return (*[]any)(nil)
	case outputTagged:
		return (*schema.TaggedValue)(nil)
	case outputJSON:
		return (*json.RawMessage)(nil)
	default:
		panic("reflectadapter: unknown output shape")
	}
}

func pointerOutput(shape outputShape, value any) (any, error) {
	switch shape {
	case outputBoolean:
		return pointerOf[bool](value)
	case outputInt32:
		return pointerOf[int32](value)
	case outputFloat64:
		return pointerOf[float64](value)
	case outputString:
		return pointerOf[string](value)
	case outputObject:
		return pointerOf[map[string]any](value)
	case outputList:
		return pointerOf[[]any](value)
	case outputTagged:
		return pointerOf[schema.TaggedValue](value)
	case outputJSON:
		return pointerOf[json.RawMessage](value)
	default:
		return nil, fmt.Errorf("reflectadapter: unknown output shape %d", shape)
	}
}

func pointerOf[T any](value any) (*T, error) {
	typed, ok := value.(T)
	if !ok {
		return nil, fmt.Errorf("reflectadapter: converted output is %T, want %s", value, strings.TrimPrefix(reflect.TypeFor[*T]().String(), "*"))
	}
	return &typed, nil
}
