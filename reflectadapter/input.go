package reflectadapter

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/valksor/naatre/schema"
)

var (
	inputValueType       = reflect.TypeFor[schema.InputValue]()
	rawMessageType       = reflect.TypeFor[json.RawMessage]()
	optionalContractType = reflect.TypeFor[interface{ reflectadapterOptional() }]()
	jsonUnmarshalerType  = reflect.TypeFor[json.Unmarshaler]()
	mapStringAnyType     = reflect.TypeFor[map[string]any]()
)

type inputVisit struct {
	schema schema.TypeID
	goType reflect.Type
}

func validateInputShape(types schema.Snapshot, descriptor schema.TypeDescriptor, actual reflect.Type, path string, active map[inputVisit]string) error {
	if actual == inputValueType || actual == rawMessageType {
		return nil
	}
	visit := inputVisit{schema: descriptor.ID, goType: actual}
	if prior, exists := active[visit]; exists {
		return fmt.Errorf("reflectadapter: %s contains unsupported input cycle through %s first seen at %s", path, actual, prior)
	}
	active[visit] = path
	defer delete(active, visit)

	switch descriptor.Kind {
	case schema.ScalarType:
		return validateInputScalar(descriptor, actual, path)
	case schema.EnumType:
		if actual.Kind() != reflect.String {
			return inputTypeError(path, actual, "a string-like enum", descriptor.ID)
		}
		return nil
	case schema.ListType:
		return validateInputList(types, descriptor, actual, path, active)
	case schema.MapType:
		return validateInputMap(types, descriptor, actual, path, active)
	case schema.InputObjectType, schema.OneOfType:
		return validateInputObject(types, descriptor, actual, path, active)
	case schema.ObjectType, schema.UnionType, schema.InterfaceType:
		return fmt.Errorf("reflectadapter: %s references output-only schema type %s", path, descriptor.ID)
	default:
		return fmt.Errorf("reflectadapter: %s schema type %s has unknown kind %q", path, descriptor.ID, descriptor.Kind)
	}
}

func validateInputScalar(descriptor schema.TypeDescriptor, actual reflect.Type, path string) error {
	want := ""
	switch schema.ScalarKind(descriptor.ID) {
	case schema.Boolean:
		if actual.Kind() == reflect.Bool {
			return nil
		}
		want = "bool"
	case schema.Int32:
		if actual.Kind() == reflect.Int32 {
			return nil
		}
		want = "int32"
	case schema.Float64:
		if actual.Kind() == reflect.Float64 {
			return nil
		}
		want = "float64"
	case schema.String, schema.ID, schema.Int64, schema.UInt64, schema.BigInt, schema.Decimal, schema.Timestamp, schema.Duration, schema.UUID, schema.Bytes:
		if actual.Kind() == reflect.String {
			return nil
		}
		want = "a string-like value"
	default:
		return validateCustomScalarInput(descriptor, actual, path)
	}
	return inputTypeError(path, actual, want, descriptor.ID)
}

func validateCustomScalarInput(descriptor schema.TypeDescriptor, actual reflect.Type, path string) error {
	if reflect.PointerTo(actual).Implements(jsonUnmarshalerType) {
		return nil
	}
	if descriptor.Scalar != nil && len(descriptor.Scalar.AcceptedWireShapes) == 1 {
		switch descriptor.Scalar.AcceptedWireShapes[0] {
		case schema.JSONBoolean:
			if actual.Kind() == reflect.Bool {
				return nil
			}
		case schema.JSONString:
			if actual.Kind() == reflect.String {
				return nil
			}
		case schema.JSONNumber:
			if actual.Kind() == reflect.Float64 || actual == reflect.TypeFor[json.Number]() {
				return nil
			}
		case schema.JSONArray:
			if actual.Kind() == reflect.Slice || actual.Kind() == reflect.Array {
				return nil
			}
		case schema.JSONObject:
			if actual.Kind() == reflect.Map || actual.Kind() == reflect.Struct {
				return nil
			}
		}
	}
	return inputTypeError(path, actual, "json.RawMessage, json.Unmarshaler, or the custom scalar wire shape", descriptor.ID)
}

func validateInputList(types schema.Snapshot, descriptor schema.TypeDescriptor, actual reflect.Type, path string, active map[inputVisit]string) error {
	if actual.Kind() != reflect.Slice && actual.Kind() != reflect.Array {
		return inputTypeError(path, actual, "a slice or array", descriptor.ID)
	}
	element, err := inputType(types, descriptor.Element, path+" element")
	if err != nil {
		return err
	}
	elementType := actual.Elem()
	if elementType == reflect.TypeFor[any]() {
		return nil
	}
	elementType, optional := unwrapOptional(elementType)
	if descriptor.ElementNullable && !optional {
		if elementType.Kind() != reflect.Pointer {
			return fmt.Errorf("reflectadapter: %s element type %s must be a pointer or Optional for nullable schema element", path, elementType)
		}
		elementType = elementType.Elem()
	}
	for elementType.Kind() == reflect.Pointer {
		elementType = elementType.Elem()
	}
	return validateInputShape(types, element, elementType, path+" element", active)
}

func validateInputMap(types schema.Snapshot, descriptor schema.TypeDescriptor, actual reflect.Type, path string, active map[inputVisit]string) error {
	if actual.Kind() != reflect.Map || actual.Key().Kind() != reflect.String {
		return inputTypeError(path, actual, "a string-keyed map", descriptor.ID)
	}
	if actual == mapStringAnyType {
		return nil
	}
	element, err := inputType(types, descriptor.Element, path+" map element")
	if err != nil {
		return err
	}
	elementType, optional := unwrapOptional(actual.Elem())
	if descriptor.ElementNullable && !optional {
		if elementType.Kind() != reflect.Pointer {
			return fmt.Errorf("reflectadapter: %s map element type %s must be a pointer or Optional for nullable schema element", path, elementType)
		}
		elementType = elementType.Elem()
	}
	for elementType.Kind() == reflect.Pointer {
		elementType = elementType.Elem()
	}
	return validateInputShape(types, element, elementType, path+" map element", active)
}

func validateInputObject(types schema.Snapshot, descriptor schema.TypeDescriptor, actual reflect.Type, path string, active map[inputVisit]string) error {
	if actual == mapStringAnyType {
		return nil
	}
	if actual.Kind() != reflect.Struct {
		return inputTypeError(path, actual, "a JSON-mapped struct or map[string]any", descriptor.ID)
	}
	seen := make(map[string]string, len(descriptor.Fields))
	for _, field := range reflect.VisibleFields(actual) {
		name, exposed, err := jsonInputFieldName(field)
		if err != nil {
			return fmt.Errorf("reflectadapter: %s.%s: %w", path, field.Name, err)
		}
		if !exposed {
			continue
		}
		fieldPath := path + "." + field.Name
		if prior, duplicate := seen[name]; duplicate {
			return fmt.Errorf("reflectadapter: %s and %s both map to input field %q", prior, fieldPath, name)
		}
		fieldDescriptor, ok := descriptor.Fields[name]
		if !ok {
			return fmt.Errorf("reflectadapter: %s maps to field %q absent from schema %s", fieldPath, name, descriptor.ID)
		}
		if err := validateInputField(types, descriptor.Kind, fieldDescriptor, field.Type, fieldPath, active); err != nil {
			return err
		}
		seen[name] = fieldPath
	}
	names := make([]string, 0, len(descriptor.Fields))
	for name := range descriptor.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("reflectadapter: %s has no exported JSON field for schema field %q", path, name)
		}
	}
	return nil
}

func validateInputField(types schema.Snapshot, ownerKind schema.TypeKind, descriptor schema.FieldDescriptor, actual reflect.Type, path string, active map[inputVisit]string) error {
	current, optional := unwrapOptional(actual)
	if descriptor.Nullable && !optional {
		if current.Kind() != reflect.Pointer {
			return fmt.Errorf("reflectadapter: %s type %s must be a pointer or Optional for nullable schema field", path, current)
		}
		current = current.Elem()
	}
	if ownerKind == schema.OneOfType && !optional && actual.Kind() != reflect.Pointer {
		return fmt.Errorf("reflectadapter: %s type %s must be a pointer or Optional to preserve one-of presence", path, actual)
	}
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	fieldType, err := inputType(types, descriptor.Type, path)
	if err != nil {
		return err
	}
	return validateInputShape(types, fieldType, current, path, active)
}

func inputType(types schema.Snapshot, id schema.TypeID, path string) (schema.TypeDescriptor, error) {
	descriptor, ok := types.Lookup(id)
	if !ok || !descriptor.Input {
		return schema.TypeDescriptor{}, fmt.Errorf("reflectadapter: %s references unknown or non-input schema type %q", path, id)
	}
	return descriptor, nil
}

func unwrapOptional(actual reflect.Type) (reflect.Type, bool) {
	if actual.Kind() != reflect.Struct || !actual.Implements(optionalContractType) {
		return actual, false
	}
	value, ok := actual.FieldByName("value")
	if !ok {
		return actual, false
	}
	return value.Type, true
}

func jsonInputFieldName(field reflect.StructField) (string, bool, error) {
	if field.PkgPath != "" {
		return "", false, nil
	}
	tag, tagged := field.Tag.Lookup("json")
	if !tagged {
		return field.Name, true, nil
	}
	name, options, _ := strings.Cut(tag, ",")
	if name == "-" {
		return "", false, nil
	}
	if options != "" {
		return "", false, fmt.Errorf("json tag options %q are unsupported for reflected input", options)
	}
	if name == "" {
		name = field.Name
	}
	return name, true, nil
}

func inputTypeError(path string, actual reflect.Type, expected string, schemaID schema.TypeID) error {
	return fmt.Errorf("reflectadapter: %s has unsupported type %s; must be %s for schema %s", path, actual, expected, schemaID)
}
