package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"unicode/utf8"

	"github.com/valksor/naatre/schema"
)

const (
	maxOutputDepth = 64
	maxOutputNodes = 100_000
)

var errInvalidOutput = errors.New("invalid handler output")

type completionIssue struct {
	path  []any
	cause error
}

type copyState struct {
	active map[copyReference]bool
	nodes  int
}

type copyReference struct {
	kind reflect.Kind
	ptr  uintptr
}

type completionState struct {
	types  schema.Snapshot
	active map[schema.TypeID]int
}

func completeContained(value any, output schema.TypeID, nullable bool, types schema.Snapshot) (completed any, issues []completionIssue, available bool, fatal error) {
	copied, err := copyOutput(reflect.ValueOf(value), 0, &copyState{active: make(map[copyReference]bool)})
	if err != nil {
		return nil, nil, false, err
	}
	state := &completionState{types: types, active: make(map[schema.TypeID]int)}
	completed, issues, available = state.completeOutput(copied, output, nullable, 0, nil)
	return completed, issues, available, nil
}

func copyOutput(value reflect.Value, depth int, state *copyState) (any, error) {
	if depth > maxOutputDepth {
		return nil, fmt.Errorf("%w: output exceeds maximum depth", errInvalidOutput)
	}
	state.nodes++
	if state.nodes > maxOutputNodes {
		return nil, fmt.Errorf("%w: output exceeds maximum node count", errInvalidOutput)
	}
	if !value.IsValid() {
		return nil, nil
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil, nil
		}
		value = value.Elem()
	}

	kind := value.Kind()
	if kind == reflect.Pointer {
		if value.IsNil() {
			return nil, nil
		}
		if err := state.enter(copyReference{kind: value.Kind(), ptr: value.Pointer()}); err != nil {
			return nil, err
		}
		defer state.leave(copyReference{kind: value.Kind(), ptr: value.Pointer()})
		return copyOutput(value.Elem(), depth+1, state)
	}
	if kind == reflect.Map {
		if value.IsNil() {
			return nil, nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("%w: map keys must be strings", errInvalidOutput)
		}
		reference := copyReference{kind: value.Kind(), ptr: value.Pointer()}
		if err := state.enter(reference); err != nil {
			return nil, err
		}
		defer state.leave(reference)
		result := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			copied, err := copyOutput(iterator.Value(), depth+1, state)
			if err != nil {
				return nil, err
			}
			result[iterator.Key().String()] = copied
		}
		return result, nil
	}
	if kind == reflect.Slice {
		if value.IsNil() {
			return nil, nil
		}
		reference := copyReference{kind: value.Kind(), ptr: value.Pointer()}
		if err := state.enter(reference); err != nil {
			return nil, err
		}
		defer state.leave(reference)
	}
	if kind == reflect.Slice || kind == reflect.Array {
		result := make([]any, value.Len())
		for index := 0; index < value.Len(); index++ {
			copied, err := copyOutput(value.Index(index), depth+1, state)
			if err != nil {
				return nil, err
			}
			result[index] = copied
		}
		return result, nil
	}
	if !value.CanInterface() {
		return nil, fmt.Errorf("%w: inaccessible Go value", errInvalidOutput)
	}
	return value.Interface(), nil
}

func (s *copyState) enter(reference copyReference) error {
	if reference.ptr != 0 && s.active[reference] {
		return fmt.Errorf("%w: cyclic output", errInvalidOutput)
	}
	if reference.ptr != 0 {
		s.active[reference] = true
	}
	return nil
}

func (s *copyState) leave(reference copyReference) {
	if reference.ptr != 0 {
		delete(s.active, reference)
	}
}

func (s *completionState) completeOutput(value any, output schema.TypeID, nullable bool, depth int, path []any) (completed any, issues []completionIssue, available bool) {
	if depth > maxOutputDepth {
		return nil, issue(path, "schema completion exceeds maximum depth"), false
	}
	if value == nil {
		if nullable {
			return nil, nil, true
		}
		return nil, issue(path, "non-null output is null"), false
	}
	descriptor, ok := s.types.Lookup(output)
	if !ok || !descriptor.Output {
		return nil, issue(path, "output type is unavailable"), false
	}
	if descriptor.MaxDepth > 0 && s.active[output] > descriptor.MaxDepth {
		return nil, issue(path, "schema type exceeds its maximum depth"), false
	}
	s.active[output]++
	defer func() { s.active[output]-- }()

	switch descriptor.Kind {
	case schema.ScalarType:
		return completeScalar(value, schema.ScalarKind(output), nullable, path)
	case schema.ObjectType:
		object, ok := value.(map[string]any)
		if !ok {
			return nil, issue(path, "object output must be a string-keyed map"), false
		}
		result := make(map[string]any, len(object))
		names := make([]string, 0, len(descriptor.Fields))
		for name := range descriptor.Fields {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			field := descriptor.Fields[name]
			fieldValue, exists := object[name]
			fieldPath := appendPath(path, name)
			if !exists {
				if field.Required {
					issues = append(issues, completionIssue{path: fieldPath, cause: fmt.Errorf("%w: required field is absent", errInvalidOutput)})
				}
				continue
			}
			fieldOutput, fieldIssues, fieldAvailable := s.completeOutput(fieldValue, field.Type, field.Nullable, depth+1, fieldPath)
			issues = append(issues, fieldIssues...)
			if fieldAvailable {
				result[name] = fieldOutput
			}
		}
		unknown := make([]string, 0)
		for name := range object {
			if _, known := descriptor.Fields[name]; !known {
				unknown = append(unknown, name)
			}
		}
		slices.Sort(unknown)
		for _, name := range unknown {
			message := "unknown object field"
			if !utf8.ValidString(name) {
				message = "object field name is invalid UTF-8"
			}
			issues = append(issues, completionIssue{path: appendPath(path, name), cause: fmt.Errorf("%w: %s", errInvalidOutput, message)})
		}
		return result, issues, true
	case schema.ListType:
		list, ok := value.([]any)
		if !ok {
			return nil, issue(path, "list output must be a slice or array"), false
		}
		result := make([]any, len(list))
		for index, item := range list {
			itemOutput, itemIssues, itemAvailable := s.completeOutput(item, descriptor.Element, descriptor.ElementNullable, depth+1, appendPath(path, index))
			issues = append(issues, itemIssues...)
			if itemAvailable {
				result[index] = itemOutput
			}
		}
		return result, issues, true
	case schema.MapType:
		object, ok := value.(map[string]any)
		if !ok {
			return nil, issue(path, "map output must be a string-keyed map"), false
		}
		result := make(map[string]any, len(object))
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			if !utf8.ValidString(key) {
				issues = append(issues, completionIssue{path: appendPath(path, key), cause: fmt.Errorf("%w: map key is invalid UTF-8", errInvalidOutput)})
				continue
			}
			itemOutput, itemIssues, itemAvailable := s.completeOutput(object[key], descriptor.Element, descriptor.ElementNullable, depth+1, appendPath(path, key))
			issues = append(issues, itemIssues...)
			if itemAvailable {
				result[key] = itemOutput
			}
		}
		return result, issues, true
	case schema.EnumType:
		reflected := reflect.ValueOf(value)
		if reflected.Kind() != reflect.String || !utf8.ValidString(reflected.String()) || (!descriptor.Open && !slices.Contains(descriptor.EnumValues, reflected.String())) {
			return nil, issue(path, "enum output is invalid"), false
		}
		return reflected.String(), nil, true
	case schema.InterfaceType, schema.UnionType:
		return nil, issue(path, "output type is not supported by the reference executor"), false
	case schema.InputObjectType, schema.OneOfType:
		return nil, issue(path, "type is not valid in output position"), false
	}
	return nil, issue(path, "type has an unknown output kind"), false
}

func completeScalar(value any, kind schema.ScalarKind, nullable bool, path []any) (any, []completionIssue, bool) {
	reflected := reflect.ValueOf(value)
	expected := scalarGoType(schema.TypeID(kind))
	if expected == nil || !reflected.IsValid() || reflected.Type() != expected {
		return nil, issue(path, "scalar output has the wrong Go type"), false
	}
	if reflected.Kind() == reflect.String && !utf8.ValidString(reflected.String()) {
		return nil, issue(path, "scalar output contains invalid UTF-8"), false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, issue(path, "scalar output cannot be encoded"), false
	}
	parsed, err := schema.ParseScalar(kind, raw)
	if err != nil || (parsed.IsNull() && !nullable) {
		return nil, issue(path, "scalar output is invalid"), false
	}
	if parsed.IsNull() {
		return nil, nil, true
	}
	canonical, err := parsed.MarshalJSON()
	if err != nil {
		return nil, issue(path, "scalar output cannot be completed"), false
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var completed any
	if err := decoder.Decode(&completed); err != nil {
		return nil, issue(path, "canonical scalar output cannot be decoded"), false
	}
	return completed, nil, true
}

func issue(path []any, message string) []completionIssue {
	return []completionIssue{{path: append([]any(nil), path...), cause: fmt.Errorf("%w: %s", errInvalidOutput, message)}}
}

func appendPath(path []any, segment any) []any {
	result := make([]any, len(path)+1)
	copy(result, path)
	result[len(path)] = segment
	return result
}
