package mutation_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/valksor/naatre/mutation"
	"github.com/valksor/naatre/schema"
)

func TestTypedUpdatesDistinguishMissingNullAndRemoval(t *testing.T) {
	t.Parallel()
	descriptor := updateDescriptor(t, true)
	current := map[string]any{"name": "old", "nickname": "nick", "tags": []any{"a", "b"}, "labels": map[string]any{"x": "1"}, "id": "immutable"}

	t.Run("omitted is unchanged", func(t *testing.T) {
		result, err := descriptor.Apply(current, mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.name", Action: mutation.EditSet, Value: "new"}}}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if result["nickname"] != "nick" || result["name"] != "new" {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("explicit null", func(t *testing.T) {
		result, err := descriptor.Apply(current, mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.nickname", Action: mutation.EditSet, Value: nil}}}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if value, exists := result["nickname"]; !exists || value != nil {
			t.Fatalf("nickname = %#v, exists = %v", value, exists)
		}
	})

	t.Run("remove", func(t *testing.T) {
		result, err := descriptor.Apply(current, mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.nickname", Action: mutation.EditRemove}}}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := result["nickname"]; exists {
			t.Fatalf("nickname remains in %#v", result)
		}
	})
}

func TestTypedUpdateValidationIsAllOrNothing(t *testing.T) {
	t.Parallel()
	descriptor := updateDescriptor(t, true)
	current := map[string]any{"name": "old", "nickname": "nick", "tags": []any{"a", "b"}, "labels": map[string]any{"x": "1"}, "id": "immutable"}
	index := 0
	tests := []struct {
		name        string
		input       mutation.UpdateInput
		authorized  mutation.FieldAuthorizer
		conditional bool
		code        string
	}{
		{name: "forbidden property", input: mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.admin", Action: mutation.EditSet, Value: true}}}, code: mutation.CodeForbiddenProperty},
		{name: "field authorization", input: mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.name", Action: mutation.EditSet, Value: "new"}}}, authorized: func(string) bool { return false }, code: mutation.CodeUnauthorized},
		{name: "required removal", input: mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.name", Action: mutation.EditRemove}}}, code: mutation.CodeRequiredField},
		{name: "nonnull null", input: mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.name", Action: mutation.EditSet, Value: nil}}}, code: mutation.CodeNonNullableField},
		{name: "immutable", input: mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.id", Action: mutation.EditSet, Value: "changed"}}}, code: mutation.CodeImmutableField},
		{name: "duplicate", input: mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.nickname", Action: mutation.EditSet, Value: "a"}, {Field: "User.nickname", Action: mutation.EditRemove}}}, code: mutation.CodeDuplicateEdit},
		{name: "one of", input: mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.email", Action: mutation.EditSet, Value: "a@example.test"}, {Field: "User.phone", Action: mutation.EditSet, Value: "+1"}}}, code: mutation.CodeOneOfViolation},
		{name: "list index race", input: mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.tags", Action: mutation.EditListRemove, Index: &index}}}, conditional: false, code: mutation.CodeListIndexConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := cloneMap(current)
			_, err := descriptor.Apply(current, test.input, test.authorized, test.conditional)
			assertCode(t, err, test.code)
			if !reflect.DeepEqual(current, before) {
				t.Fatalf("current mutated after failure: %#v", current)
			}
		})
	}
}

func TestListAndMapEditsUseDeclaredOrdering(t *testing.T) {
	t.Parallel()
	descriptor := updateDescriptor(t, true)
	index := 1
	key := "y"
	result, err := descriptor.Apply(map[string]any{
		"name": "old", "tags": []any{"a", "b"}, "labels": map[string]any{"x": "1"},
	}, mutation.UpdateInput{Edits: []mutation.Edit{
		{Field: "User.tags", Action: mutation.EditListInsert, Index: &index, Value: "between"},
		{Field: "User.labels", Action: mutation.EditMapSet, Key: &key, Value: "2"},
	}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result["tags"], []any{"a", "between", "b"}) || !reflect.DeepEqual(result["labels"], map[string]any{"x": "1", "y": "2"}) {
		t.Fatalf("result = %#v", result)
	}
}

func TestJSONPatchRequiresDeclaredCapabilityAndExactPaths(t *testing.T) {
	t.Parallel()
	current := map[string]any{"name": "old", "nickname": "nick"}
	descriptor := updateDescriptor(t, true)
	result, err := descriptor.ApplyJSONPatch(current, []mutation.PatchOperation{
		{Operation: "test", Path: "/name", Value: "old"},
		{Operation: "replace", Path: "/name", Value: "new"},
		{Operation: "remove", Path: "/nickname"},
	}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if result["name"] != "new" {
		t.Fatalf("result = %#v", result)
	}
	if _, exists := result["nickname"]; exists {
		t.Fatal("remove was interpreted as null")
	}
	_, err = descriptor.ApplyJSONPatch(current, []mutation.PatchOperation{{Operation: "replace", Path: "/name/private", Value: "leak"}}, nil, true)
	assertCode(t, err, mutation.CodeInvalidPatchPath)
	descriptor.JSONPatch = false
	_, err = descriptor.ApplyJSONPatch(current, []mutation.PatchOperation{{Operation: "replace", Path: "/name", Value: "new"}}, nil, true)
	assertCode(t, err, mutation.CodeUpdateCapabilityUnsupported)
}

func updateDescriptor(t testing.TB, patch bool) mutation.Descriptor {
	t.Helper()
	descriptor, err := mutation.NewDescriptor(mutation.Descriptor{
		Capability: mutation.TypedUpdateCapability, RequireRevision: true, JSONPatch: patch, MaxOperations: 16,
		Fields: []mutation.FieldDescriptor{
			{ID: "User.id", Path: "/id", Type: schema.TypeID(schema.ID), Shape: mutation.ScalarField, Required: true, Immutable: true},
			{ID: "User.name", Path: "/name", Type: schema.TypeID(schema.String), Shape: mutation.ScalarField, Required: true},
			{ID: "User.nickname", Path: "/nickname", Type: schema.TypeID(schema.String), Shape: mutation.ScalarField, Nullable: true},
			{ID: "User.tags", Path: "/tags", Type: "Tags", Shape: mutation.ListField},
			{ID: "User.labels", Path: "/labels", Type: "Labels", Shape: mutation.MapField},
			{ID: "User.email", Path: "/email", Type: schema.TypeID(schema.String), Shape: mutation.ScalarField, OneOf: "contact"},
			{ID: "User.phone", Path: "/phone", Type: schema.TypeID(schema.String), Shape: mutation.ScalarField, OneOf: "contact"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func assertCode(t testing.TB, err error, code string) {
	t.Helper()
	var typed *mutation.Error
	switch {
	case err == nil:
		t.Fatalf("expected error code %s", code)
	case !errors.As(err, &typed):
		t.Fatalf("error type = %T, want *mutation.Error", err)
	case typed.Code != code:
		t.Fatalf("error code = %s, want %s", typed.Code, code)
	}
}

func cloneMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case []any:
			result[key] = append([]any(nil), typed...)
		case map[string]any:
			copy := make(map[string]any, len(typed))
			for nestedKey, nestedValue := range typed {
				copy[nestedKey] = nestedValue
			}
			result[key] = copy
		default:
			result[key] = value
		}
	}
	return result
}
