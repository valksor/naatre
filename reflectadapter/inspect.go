package reflectadapter

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type taggedField struct {
	field reflect.StructField
	index []int
	path  string
	tag   string
}

func taggedFields(target reflect.Type, only map[string]bool) ([]taggedField, error) {
	var result []taggedField
	seenTags := make(map[string]string)
	if err := walkTaggedFields(target, nil, target.Name(), only, seenTags, &result, make(map[reflect.Type]bool)); err != nil {
		return nil, err
	}
	return result, nil
}

func walkTaggedFields(current reflect.Type, prefix []int, path string, only map[string]bool, seenTags map[string]string, result *[]taggedField, active map[reflect.Type]bool) error {
	if active[current] {
		return fmt.Errorf("reflectadapter: embedded field cycle at %s", path)
	}
	active[current] = true
	defer delete(active, current)
	for index := range current.NumField() {
		field := current.Field(index)
		fieldPath := path + "." + field.Name
		tag, tagged := field.Tag.Lookup(tagName)
		if tagged {
			if field.PkgPath != "" {
				return fmt.Errorf("reflectadapter: tagged field %s is unexported", fieldPath)
			}
			name, err := parseTag(tag)
			if err != nil {
				return fmt.Errorf("reflectadapter: tagged field %s: %w", fieldPath, err)
			}
			if name != "" && (len(only) == 0 || only[field.Name]) {
				if prior, exists := seenTags[name]; exists {
					return fmt.Errorf("reflectadapter: tagged fields %s and %s are ambiguous for %q", prior, fieldPath, name)
				}
				seenTags[name] = fieldPath
				*result = append(*result, taggedField{field: field, index: appendIndex(prefix, index), path: fieldPath, tag: name})
			}
		}
		if field.Anonymous && !tagged {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				if err := walkTaggedFields(embedded, appendIndex(prefix, index), fieldPath, only, seenTags, result, active); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func parseTag(input string) (string, error) {
	if input == "-" {
		return "", nil
	}
	if input == "" {
		return "", fmt.Errorf("%s tag cannot be empty", tagName)
	}
	if strings.Contains(input, ",") || strings.TrimSpace(input) != input {
		return "", fmt.Errorf("%s tag %q must contain one public name", tagName, input)
	}
	return input, nil
}

func appendIndex(prefix []int, index int) []int {
	result := make([]int, len(prefix)+1)
	copy(result, prefix)
	result[len(prefix)] = index
	return result
}

func compileTaggedField(targetValue reflect.Value, targetType reflect.Type, field taggedField, descriptor runtime.Descriptor, types schema.Snapshot) (runtime.Definition, error) {
	if field.field.Type.Kind() == reflect.Func {
		value, err := fieldValue(targetValue, field.index)
		if err != nil {
			return runtime.Definition{}, fmt.Errorf("reflectadapter: tagged function field %s: %w", field.path, err)
		}
		if value.IsNil() {
			return runtime.Definition{}, fmt.Errorf("reflectadapter: tagged function field %s is nil", field.path)
		}
		value = reflect.ValueOf(value.Interface())
		return compileCallable(field.path, value, reflect.Value{}, false, descriptor, types)
	}
	if descriptor.Scope != runtime.ObjectScope || descriptor.Member != runtime.FieldMember {
		return runtime.Definition{}, fmt.Errorf("reflectadapter: tagged data field %s requires an object field descriptor", field.path)
	}
	return compileDataField(field.path, targetType, field, descriptor, types)
}

func fieldValue(target reflect.Value, index []int) (reflect.Value, error) {
	current := target
	for current.Kind() == reflect.Pointer {
		if current.IsNil() {
			return reflect.Value{}, fmt.Errorf("target pointer is nil")
		}
		current = current.Elem()
	}
	for offset, fieldIndex := range index {
		current = current.Field(fieldIndex)
		if offset != len(index)-1 {
			for current.Kind() == reflect.Pointer {
				if current.IsNil() {
					return reflect.Value{}, fmt.Errorf("embedded pointer is nil")
				}
				current = current.Elem()
			}
		}
	}
	return current, nil
}

func compileMethod(targetValue reflect.Value, targetType reflect.Type, binding Binding, types schema.Snapshot) (runtime.Definition, error) {
	methodType := targetValue.Type()
	method, ok := methodType.MethodByName(binding.GoName)
	if !ok {
		if promotedMethodCandidates(targetType, binding.GoName, make(map[reflect.Type]bool)) > 1 {
			return runtime.Definition{}, fmt.Errorf("reflectadapter: allowlisted method %q is ambiguous through promoted fields", binding.GoName)
		}
		if methodType.Kind() != reflect.Pointer {
			pointerMethod, pointerOK := reflect.PointerTo(targetType).MethodByName(binding.GoName)
			if pointerOK {
				return runtime.Definition{}, fmt.Errorf("reflectadapter: allowlisted method %q has a pointer/value receiver collision; pass a pointer target for %s", pointerMethod.Name, targetType)
			}
		}
		return runtime.Definition{}, fmt.Errorf("reflectadapter: allowlisted method %q is not an exported method of %s", binding.GoName, methodType)
	}
	if binding.Descriptor.Scope == runtime.RootScope {
		if isNil(targetValue) {
			return runtime.Definition{}, fmt.Errorf("reflectadapter: root method %q requires a non-nil target", binding.GoName)
		}
		bound := targetValue.Method(method.Index)
		return compileCallable(binding.GoName, bound, reflect.Value{}, false, binding.Descriptor, types)
	}
	return compileCallable(binding.GoName, method.Func, reflect.Value{}, true, binding.Descriptor, types)
}

func promotedMethodCandidates(current reflect.Type, name string, active map[reflect.Type]bool) int {
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	if current.Kind() != reflect.Struct || active[current] {
		return 0
	}
	active[current] = true
	defer delete(active, current)
	count := 0
	for index := range current.NumField() {
		field := current.Field(index)
		if !field.Anonymous {
			continue
		}
		embedded := field.Type
		if _, ok := embedded.MethodByName(name); ok {
			count++
			continue
		}
		count += promotedMethodCandidates(embedded, name, active)
	}
	return count
}

func isNil(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	case reflect.Invalid,
		reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128,
		reflect.Array, reflect.String, reflect.Struct, reflect.UnsafePointer:
		return false
	default:
		return false
	}
}
