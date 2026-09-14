package mutation

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

// Apply validates every edit before returning a cloned changed value. An error
// leaves current untouched. conditional must be true for positional list edits.
func (d Descriptor) Apply(current map[string]any, input UpdateInput, authorize FieldAuthorizer, conditional bool) (map[string]any, error) {
	contract, err := NewDescriptor(d)
	if err != nil {
		return nil, failure(CodeInvalidUpdate, "update contract is invalid", nil, err)
	}
	if len(input.Edits) == 0 || uint64(len(input.Edits)) > contract.MaxOperations {
		return nil, failure(CodeInvalidUpdate, "update operation count is invalid", nil, nil)
	}
	staged := cloneObject(current)
	state := typedUpdateState{
		fields: contract.Fields,
		seen:   make(map[string]bool),
		oneOf:  make(map[string]bool),
	}
	for index, edit := range input.Edits {
		if err := state.apply(staged, edit, index, authorize, conditional); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

type typedUpdateState struct {
	fields []FieldDescriptor
	seen   map[string]bool
	oneOf  map[string]bool
}

func findField(descriptors []FieldDescriptor, key string, usePath bool) (FieldDescriptor, bool) {
	index := slices.IndexFunc(descriptors, func(field FieldDescriptor) bool {
		if usePath {
			return field.Path == key
		}
		return field.ID == key
	})
	if index < 0 {
		return FieldDescriptor{}, false
	}
	return descriptors[index], true
}

func (s *typedUpdateState) apply(staged map[string]any, edit Edit, index int, authorize FieldAuthorizer, conditional bool) error {
	path := []any{"edits", index}
	field, err := writableField(s.fields, edit.Field, authorize, path)
	if err != nil {
		return err
	}
	target, err := editTarget(edit, field)
	if err != nil {
		return failure(CodeInvalidUpdate, "update operation shape is invalid", path, err)
	}
	if err := s.recordEdit(field, target, path); err != nil {
		return err
	}
	return withInputPath(applyEdit(staged, field, edit, conditional), path)
}

func writableField(fields []FieldDescriptor, id string, authorize FieldAuthorizer, path []any) (FieldDescriptor, error) {
	field, exists := findField(fields, id, false)
	if !exists {
		return FieldDescriptor{}, failure(CodeForbiddenProperty, "property is not writable", path, nil)
	}
	if authorize != nil && !authorize(field.ID) {
		return FieldDescriptor{}, failure(CodeUnauthorized, "mutation is not authorized", path, nil)
	}
	if field.Immutable {
		return FieldDescriptor{}, failure(CodeImmutableField, "field is immutable", path, nil)
	}
	return field, nil
}

func (s *typedUpdateState) recordEdit(field FieldDescriptor, target string, path []any) error {
	if target != "" && s.seen[target] {
		return failure(CodeDuplicateEdit, "duplicate update target", path, nil)
	}
	if target != "" {
		s.seen[target] = true
	}
	if field.OneOf == "" {
		return nil
	}
	if s.oneOf[field.OneOf] {
		return failure(CodeOneOfViolation, "one-of update contains multiple alternatives", path, nil)
	}
	s.oneOf[field.OneOf] = true
	return nil
}

func editTarget(edit Edit, field FieldDescriptor) (string, error) {
	switch edit.Action {
	case EditSet, EditRemove:
		if edit.Index != nil || edit.Key != nil {
			return "", fmt.Errorf("unexpected coordinate")
		}
		return field.ID, nil
	case EditListAppend:
		if edit.Index != nil || edit.Key != nil {
			return "", fmt.Errorf("unexpected coordinate")
		}
		return "", nil
	case EditListInsert, EditListReplace, EditListRemove:
		if edit.Index == nil || edit.Key != nil {
			return "", fmt.Errorf("list coordinate is required")
		}
		return fmt.Sprintf("%s#%d", field.ID, *edit.Index), nil
	case EditMapSet, EditMapRemove:
		if edit.Key == nil || *edit.Key == "" || edit.Index != nil {
			return "", fmt.Errorf("map key is required")
		}
		return field.ID + "#" + *edit.Key, nil
	default:
		return "", fmt.Errorf("unknown action")
	}
}

func applyEdit(staged map[string]any, field FieldDescriptor, edit Edit, conditional bool) error {
	name := fieldName(field.Path)
	switch edit.Action {
	case EditSet:
		if edit.Value == nil && !field.Nullable {
			return failure(CodeNonNullableField, "field does not accept null", nil, nil)
		}
		staged[name] = cloneValue(edit.Value)
		return nil
	case EditRemove:
		if field.Required {
			return failure(CodeRequiredField, "required field cannot be removed", nil, nil)
		}
		delete(staged, name)
		return nil
	case EditListAppend, EditListInsert, EditListReplace, EditListRemove:
		return applyListEdit(staged, name, field, edit, conditional)
	case EditMapSet, EditMapRemove:
		return applyMapEdit(staged, name, field, edit)
	default:
		return failure(CodeInvalidUpdate, "update action is invalid", nil, nil)
	}
}

func applyListEdit(staged map[string]any, name string, field FieldDescriptor, edit Edit, conditional bool) error {
	if field.Shape != ListField {
		return failure(CodeInvalidUpdate, "list edit targets a non-list field", nil, nil)
	}
	if edit.Action != EditListAppend && !conditional {
		return failure(CodeListIndexConflict, "positional list edit requires an expected revision", nil, nil)
	}
	list, ok := staged[name].([]any)
	if !ok {
		return failure(CodeInvalidUpdate, "list field has an invalid current value", nil, nil)
	}
	list = cloneValue(list).([]any)
	if edit.Action == EditListAppend {
		staged[name] = append(list, cloneValue(edit.Value))
		return nil
	}
	index := *edit.Index
	limit := len(list)
	if edit.Action == EditListInsert {
		limit++
	}
	if index < 0 || index >= limit {
		return failure(CodeListIndexConflict, "list index does not identify the expected element", nil, nil)
	}
	switch edit.Action {
	case EditListInsert:
		list = append(list, nil)
		copy(list[index+1:], list[index:])
		list[index] = cloneValue(edit.Value)
	case EditListReplace:
		list[index] = cloneValue(edit.Value)
	case EditListRemove:
		copy(list[index:], list[index+1:])
		list = list[:len(list)-1]
	case EditSet, EditRemove, EditListAppend, EditMapSet, EditMapRemove:
		return failure(CodeInvalidUpdate, "list action is invalid", nil, nil)
	}
	staged[name] = list
	return nil
}

func applyMapEdit(staged map[string]any, name string, field FieldDescriptor, edit Edit) error {
	if field.Shape != MapField {
		return failure(CodeInvalidUpdate, "map edit targets a non-map field", nil, nil)
	}
	value, ok := staged[name].(map[string]any)
	if !ok {
		return failure(CodeInvalidUpdate, "map field has an invalid current value", nil, nil)
	}
	value = cloneObject(value)
	if edit.Action == EditMapSet {
		value[*edit.Key] = cloneValue(edit.Value)
	} else {
		delete(value, *edit.Key)
	}
	staged[name] = value
	return nil
}

// ApplyJSONPatch applies the explicitly negotiated RFC 6902 subset. Paths
// must exactly match registered fields; arbitrary traversal is never inferred.
func (d Descriptor) ApplyJSONPatch(current map[string]any, operations []PatchOperation, authorize FieldAuthorizer, conditional bool) (map[string]any, error) {
	contract, err := NewDescriptor(d)
	if err != nil {
		return nil, failure(CodeInvalidUpdate, "update contract is invalid", nil, err)
	}
	if !contract.JSONPatch {
		return nil, failure(CodeUpdateCapabilityUnsupported, "JSON Patch is not supported", nil, nil)
	}
	if len(operations) == 0 || uint64(len(operations)) > contract.MaxOperations {
		return nil, failure(CodeInvalidUpdate, "patch operation count is invalid", nil, nil)
	}
	staged := cloneObject(current)
	state := patchUpdateState{
		staged:      staged,
		fields:      contract.Fields,
		authorize:   authorize,
		conditional: conditional,
	}
	for index, operation := range operations {
		if err := state.apply(operation, index); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

type patchUpdateState struct {
	staged      map[string]any
	fields      []FieldDescriptor
	authorize   FieldAuthorizer
	conditional bool
}

func (s patchUpdateState) apply(operation PatchOperation, index int) error {
	path := []any{"patch", index}
	field, exists := findField(s.fields, operation.Path, true)
	if !exists {
		return failure(CodeInvalidPatchPath, "patch path is not registered", path, nil)
	}
	if s.authorize != nil && !s.authorize(field.ID) {
		return failure(CodeUnauthorized, "mutation is not authorized", path, nil)
	}
	name := fieldName(field.Path)
	if operation.Operation == "test" {
		return testPatchValue(s.staged, name, operation.Value, path)
	}
	if field.Immutable {
		return failure(CodeImmutableField, "field is immutable", path, nil)
	}
	action, err := patchAction(s.staged, name, operation.Operation, path)
	if err != nil {
		return err
	}
	return withInputPath(applyEdit(s.staged, field, Edit{Field: field.ID, Action: action, Value: operation.Value}, s.conditional), path)
}

func testPatchValue(staged map[string]any, name string, expected any, path []any) error {
	actual, exists := staged[name]
	if !exists || !reflect.DeepEqual(actual, expected) {
		return failure(CodePatchTestFailed, "patch test precondition failed", path, nil)
	}
	return nil
}

func patchAction(staged map[string]any, name, operation string, path []any) (EditAction, error) {
	switch operation {
	case "add":
		return EditSet, nil
	case "replace":
		if _, exists := staged[name]; !exists {
			return "", failure(CodeInvalidPatchPath, "patch replace target is absent", path, nil)
		}
		return EditSet, nil
	case "remove":
		return EditRemove, nil
	default:
		return "", failure(CodeInvalidUpdate, "patch operation is not supported", path, nil)
	}
}

func withInputPath(err error, path []any) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		typed.Path = path
	}
	return err
}

func cloneObject(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	return cloneValue(input).(map[string]any)
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if typed == nil {
			return typed
		}
		return maps.Collect(func(yield func(string, any) bool) {
			for key, item := range typed {
				if !yield(key, cloneValue(item)) {
					return
				}
			}
		})
	case []any:
		if typed == nil {
			return typed
		}
		return slices.Collect(func(yield func(any) bool) {
			for _, item := range typed {
				if !yield(cloneValue(item)) {
					return
				}
			}
		})
	case []string:
		return slices.Clone(typed)
	default:
		return typed
	}
}
