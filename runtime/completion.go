package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

var errInvalidOutput = errors.New("invalid handler output")

type completionIssue struct {
	path  []any
	cause error
}

type copyState struct {
	active map[copyReference]bool
	nodes  uint64
	limits ResourceLimits
}

type copyReference struct {
	kind reflect.Kind
	ptr  uintptr
}

type completionState struct {
	types  schema.Snapshot
	active map[schema.TypeID]int
	limits ResourceLimits
}

func completeContained(value any, output schema.TypeID, nullable bool, types schema.Snapshot) (completed any, issues []completionIssue, available bool, fatal error) {
	return completeContainedWithLimits(value, output, nullable, types, DefaultResourceLimits())
}

func completeContainedWithLimits(value any, output schema.TypeID, nullable bool, types schema.Snapshot, limits ResourceLimits) (completed any, issues []completionIssue, available bool, fatal error) {
	copied, err := copyOutput(reflect.ValueOf(value), 0, &copyState{active: make(map[copyReference]bool), limits: limits})
	if err != nil {
		return nil, nil, false, err
	}
	state := &completionState{types: types, active: make(map[schema.TypeID]int), limits: limits}
	completed, issues, available = state.completeOutput(copied, output, nullable, 0, nil)
	return completed, issues, available, nil
}

func copyOutput(value reflect.Value, depth int, state *copyState) (any, error) {
	if uint64(depth) > state.limits.MaxOutputDepth {
		return nil, fmt.Errorf("%w: output exceeds maximum depth", errResourceBudget)
	}
	next, ok := checkedResourceAdd(state.nodes, 1)
	if !ok || next > state.limits.MaxOutputNodes {
		return nil, fmt.Errorf("%w: output exceeds maximum node count", errResourceBudget)
	}
	state.nodes = next
	if !value.IsValid() {
		return nil, nil
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil, nil
		}
		value = value.Elem()
	}
	if copied, matched, err := copySpecialOutput(value, depth, state); matched || err != nil {
		return copied, err
	}

	kind := value.Kind()
	if kind == reflect.Pointer {
		return copyPointerOutput(value, depth, state)
	}
	if kind == reflect.Map {
		return copyMapOutput(value, depth, state)
	}
	if kind == reflect.Slice || kind == reflect.Array {
		return copyListOutput(value, depth, state)
	}
	if !value.CanInterface() {
		return nil, fmt.Errorf("%w: inaccessible Go value", errInvalidOutput)
	}
	return value.Interface(), nil
}

func copySpecialOutput(value reflect.Value, depth int, state *copyState) (any, bool, error) {
	if !value.CanInterface() {
		return nil, false, nil
	}
	switch typed := value.Interface().(type) {
	case schema.TaggedValue:
		copied, err := copyOutput(reflect.ValueOf(typed.Value()), depth+1, state)
		if err != nil {
			return nil, true, err
		}
		tagged, err := schema.Tag(typed.Variant(), copied)
		return tagged, true, err
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...), true, nil
	default:
		return nil, false, nil
	}
}

func copyPointerOutput(value reflect.Value, depth int, state *copyState) (any, error) {
	if value.IsNil() {
		return nil, nil
	}
	reference := copyReference{kind: value.Kind(), ptr: value.Pointer()}
	if err := state.enter(reference); err != nil {
		return nil, err
	}
	defer state.leave(reference)
	return copyOutput(value.Elem(), depth+1, state)
}

func copyMapOutput(value reflect.Value, depth int, state *copyState) (any, error) {
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

func copyListOutput(value reflect.Value, depth int, state *copyState) (any, error) {
	if value.Kind() == reflect.Slice && value.IsNil() {
		return nil, nil
	}
	if value.Kind() == reflect.Slice {
		reference := copyReference{kind: value.Kind(), ptr: value.Pointer()}
		if err := state.enter(reference); err != nil {
			return nil, err
		}
		defer state.leave(reference)
	}
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
	if uint64(depth) > s.limits.MaxOutputDepth {
		return nil, []completionIssue{{path: append([]any(nil), path...), cause: fmt.Errorf("%w: schema completion exceeds maximum depth", errResourceBudget)}}, false
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
		return completeScalar(value, output, nullable, path, s.types)
	case schema.ObjectType:
		return s.completeObject(value, descriptor, depth, path)
	case schema.ListType:
		return s.completeList(value, descriptor, depth, path)
	case schema.MapType:
		return s.completeMap(value, descriptor, depth, path)
	case schema.EnumType:
		reflected := reflect.ValueOf(value)
		if reflected.Kind() != reflect.String || !utf8.ValidString(reflected.String()) || (!descriptor.Open && !slices.Contains(descriptor.EnumValues, reflected.String())) {
			return nil, issue(path, "enum output is invalid"), false
		}
		return reflected.String(), nil, true
	case schema.InterfaceType, schema.UnionType:
		return s.completeTagged(value, output, depth, path)
	case schema.InputObjectType, schema.OneOfType:
		return nil, issue(path, "type is not valid in output position"), false
	}
	return nil, issue(path, "type has an unknown output kind"), false
}

func (s *completionState) completeObject(value any, descriptor schema.TypeDescriptor, depth int, path []any) (any, []completionIssue, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, issue(path, "object output must be a string-keyed map"), false
	}
	result := make(map[string]any, len(object))
	issues := s.completeObjectFields(result, object, descriptor, depth, path)
	issues = append(issues, unknownObjectFieldIssues(object, descriptor.Fields, path)...)
	return result, issues, true
}

func (s *completionState) completeObjectFields(result, object map[string]any, descriptor schema.TypeDescriptor, depth int, path []any) []completionIssue {
	var issues []completionIssue
	for _, name := range sortedStringKeys(descriptor.Fields) {
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
	return issues
}

func unknownObjectFieldIssues(object map[string]any, fields map[string]schema.FieldDescriptor, path []any) []completionIssue {
	var issues []completionIssue
	for _, name := range sortedStringKeys(object) {
		if _, known := fields[name]; known {
			continue
		}
		message := "unknown object field"
		if !utf8.ValidString(name) {
			message = "object field name is invalid UTF-8"
		}
		issues = append(issues, completionIssue{path: appendPath(path, name), cause: fmt.Errorf("%w: %s", errInvalidOutput, message)})
	}
	return issues
}

func (s *completionState) completeList(value any, descriptor schema.TypeDescriptor, depth int, path []any) (any, []completionIssue, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, issue(path, "list output must be a slice or array"), false
	}
	result := make([]any, len(list))
	var issues []completionIssue
	for index, item := range list {
		itemOutput, itemIssues, itemAvailable := s.completeOutput(item, descriptor.Element, descriptor.ElementNullable, depth+1, appendPath(path, index))
		issues = append(issues, itemIssues...)
		if itemAvailable {
			result[index] = itemOutput
		}
	}
	return result, issues, true
}

func (s *completionState) completeMap(value any, descriptor schema.TypeDescriptor, depth int, path []any) (any, []completionIssue, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, issue(path, "map output must be a string-keyed map"), false
	}
	result := make(map[string]any, len(object))
	var issues []completionIssue
	for _, key := range sortedStringKeys(object) {
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
}

func (s *completionState) completeTagged(value any, output schema.TypeID, depth int, path []any) (any, []completionIssue, bool) {
	tagged, ok := value.(schema.TaggedValue)
	if !ok {
		return nil, issue(path, "union or interface output requires a tagged value"), false
	}
	_, known, err := s.types.ResolveVariant(output, tagged.Variant())
	if err != nil {
		return nil, issue(appendPath(path, "$type"), err.Error()), false
	}
	variantValue, issues, available := s.completeVariantValue(tagged, known, depth, path)
	if !available {
		return nil, issues, false
	}
	return map[string]any{"$type": string(tagged.Variant()), "$value": variantValue}, issues, true
}

func (s *completionState) completeVariantValue(tagged schema.TaggedValue, known bool, depth int, path []any) (any, []completionIssue, bool) {
	valuePath := appendPath(path, "$value")
	if known {
		return s.completeOutput(tagged.Value(), tagged.Variant(), false, depth+1, valuePath)
	}
	completed, err := completeOpaqueJSON(tagged.Value())
	if err != nil {
		return nil, issue(valuePath, "unknown union value is not canonical JSON"), false
	}
	return completed, nil, true
}

func sortedStringKeys[V any](values map[string]V) []string {
	return slices.Sorted(maps.Keys(values))
}

func completeScalar(value any, typeID schema.TypeID, nullable bool, path []any, types schema.Snapshot) (any, []completionIssue, bool) {
	kind := schema.ScalarKind(typeID)
	expected := scalarGoType(schema.TypeID(kind))
	if expected == nil {
		return completeCustomScalar(value, typeID, path, types)
	}
	return completeBuiltInScalar(value, kind, expected, nullable, path)
}

func completeCustomScalar(value any, typeID schema.TypeID, path []any, types schema.Snapshot) (any, []completionIssue, bool) {
	raw, ok := value.(json.RawMessage)
	if !ok {
		return nil, issue(path, "custom scalar output must be json.RawMessage"), false
	}
	parsed, err := schema.CanonicalizeScalar(types, typeID, raw)
	if err != nil {
		return nil, issue(path, "custom scalar output is invalid"), false
	}
	canonical, err := parsed.MarshalJSON()
	if err != nil {
		return nil, issue(path, "custom scalar output cannot be completed"), false
	}
	completed, err := protocol.DecodeJSONValue(canonical)
	if err != nil {
		return nil, issue(path, "canonical custom scalar output cannot be decoded"), false
	}
	return completed, nil, true
}

func completeBuiltInScalar(value any, kind schema.ScalarKind, expected reflect.Type, nullable bool, path []any) (any, []completionIssue, bool) {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Type() != expected {
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
	completed, err := protocol.DecodeJSONValue(canonical)
	if err != nil {
		return nil, issue(path, "canonical scalar output cannot be decoded"), false
	}
	return completed, nil, true
}

func completeOpaqueJSON(value any) (any, error) {
	if raw, ok := value.(json.RawMessage); ok {
		canonical, err := protocol.CanonicalizeJSON(raw, protocol.Limits{})
		if err != nil {
			return nil, err
		}
		return protocol.DecodeJSONValue(canonical)
	}
	if err := validateOpaqueJSONValue(value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{})
	if err != nil {
		return nil, err
	}
	return protocol.DecodeJSONValue(canonical)
}

func validateOpaqueJSONValue(value any) error {
	switch typed := value.(type) {
	case nil, bool, json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return nil
	case string:
		if !utf8.ValidString(typed) {
			return errors.New("invalid UTF-8 string")
		}
		return nil
	case []any:
		for _, item := range typed {
			if err := validateOpaqueJSONValue(item); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, item := range typed {
			if !utf8.ValidString(key) {
				return errors.New("invalid UTF-8 object key")
			}
			if err := validateOpaqueJSONValue(item); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported JSON value type %T", value)
	}
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
