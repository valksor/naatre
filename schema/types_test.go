package schema_test

import (
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestCatalogFreezesPortableTypeDescriptors(t *testing.T) {
	t.Parallel()

	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{
		ID:     "User",
		Kind:   schema.ObjectType,
		Input:  false,
		Output: true,
		Fields: map[string]schema.FieldDescriptor{
			"id":   {Type: schema.TypeID(schema.ID), Required: true},
			"name": {Type: schema.TypeID(schema.String), Required: true},
		},
	}); err != nil {
		t.Fatalf("Register(User): %v", err)
	}

	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	user, ok := snapshot.Lookup("User")
	if !ok || user.Kind != schema.ObjectType || !user.Output {
		t.Fatalf("Lookup(User) = %#v, %t", user, ok)
	}
	user.Fields["id"] = schema.FieldDescriptor{}
	again, _ := snapshot.Lookup("User")
	if again.Fields["id"].Type != schema.TypeID(schema.ID) {
		t.Fatal("snapshot descriptor mutated through lookup")
	}
}

func TestCatalogRejectsDuplicateAndImpossibleDescriptors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		descriptor schema.TypeDescriptor
	}{
		{name: "invalid identifier", descriptor: schema.TypeDescriptor{ID: "$User", Kind: schema.ObjectType, Output: true}},
		{name: "no position", descriptor: schema.TypeDescriptor{ID: "User", Kind: schema.ObjectType}},
		{name: "object without fields", descriptor: schema.TypeDescriptor{ID: "User", Kind: schema.ObjectType, Output: true}},
		{name: "list without element", descriptor: schema.TypeDescriptor{ID: "Users", Kind: schema.ListType, Output: true}},
		{name: "oneof as output", descriptor: schema.TypeDescriptor{ID: "Choice", Kind: schema.OneOfType, Input: true, Output: true, Fields: map[string]schema.FieldDescriptor{"a": {Type: schema.TypeID(schema.String)}}}},
		{name: "open object", descriptor: schema.TypeDescriptor{ID: "User", Kind: schema.ObjectType, Output: true, Open: true, Fields: map[string]schema.FieldDescriptor{"name": {Type: schema.TypeID(schema.String)}}}},
		{name: "scalar element nullability", descriptor: schema.TypeDescriptor{ID: "Text", Kind: schema.ScalarType, Output: true, ElementNullable: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			catalog := schema.NewCatalog()
			if err := catalog.Register(tt.descriptor); err == nil {
				t.Fatalf("Register(%#v) succeeded, want error", tt.descriptor)
			}
		})
	}

	catalog := schema.NewCatalog()
	descriptor := schema.TypeDescriptor{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"name": {Type: schema.TypeID(schema.String)}}}
	if err := catalog.Register(descriptor); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := catalog.Register(descriptor); err == nil {
		t.Fatal("duplicate Register succeeded")
	}
}

func TestCatalogFreezeRejectsUnresolvedReferences(t *testing.T) {
	t.Parallel()

	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "Users", Kind: schema.ListType, Output: true, Element: "User"}); err != nil {
		t.Fatalf("Register(Users): %v", err)
	}
	if _, err := catalog.Freeze(); err == nil {
		t.Fatal("Freeze succeeded with unresolved User reference")
	}
}

func TestCatalogModelsEnumValuesAndBoundsRecursion(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "Color", Kind: schema.EnumType, Input: true, Output: true, Open: true, EnumValues: []string{"RED", "BLUE"}}); err != nil {
		t.Fatalf("Register enum: %v", err)
	}
	if _, err := catalog.Freeze(); err != nil {
		t.Fatalf("Freeze enum: %v", err)
	}
	recursive := schema.NewCatalog()
	if err := recursive.Register(schema.TypeDescriptor{ID: "Node", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"next": {Type: "Node", Nullable: true}}}); err != nil {
		t.Fatalf("Register recursive: %v", err)
	}
	if _, err := recursive.Freeze(); err == nil {
		t.Fatal("unbounded recursive schema froze")
	}
}
