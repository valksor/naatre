package schema_test

import (
	"encoding/json"
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

func TestCatalogFreezeRejectsInvalidSchemas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		descriptors []schema.TypeDescriptor
	}{
		{"unresolved reference", []schema.TypeDescriptor{{ID: "Users", Kind: schema.ListType, Output: true, Element: "User"}}},
		{"unbounded recursion", []schema.TypeDescriptor{{ID: "Node", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"next": {Type: "Node", Nullable: true}}}}},
		{"invalid default", []schema.TypeDescriptor{{ID: "BadDefault", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{"count": {Type: schema.TypeID(schema.UInt64), Default: json.RawMessage(`-1`)}}}}},
		{"multiple one-of defaults", []schema.TypeDescriptor{{ID: "Ambiguous", Kind: schema.OneOfType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"email": {Type: schema.TypeID(schema.String), Default: json.RawMessage(`"a@example.com"`)},
			"phone": {Type: schema.TypeID(schema.String), Default: json.RawMessage(`"1"`)},
		}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			catalog := schema.NewCatalog()
			for _, descriptor := range tt.descriptors {
				if err := catalog.Register(descriptor); err != nil {
					t.Fatalf("Register(%s): %v", descriptor.ID, err)
				}
			}
			if _, err := catalog.Freeze(); err == nil {
				t.Fatal("Freeze accepted invalid schema")
			}
		})
	}
}

func TestCatalogModelsEnumValues(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "Color", Kind: schema.EnumType, Input: true, Output: true, Open: true, EnumValues: []string{"RED", "BLUE"}}); err != nil {
		t.Fatalf("Register enum: %v", err)
	}
	if _, err := catalog.Freeze(); err != nil {
		t.Fatalf("Freeze enum: %v", err)
	}
}

func TestCatalogFreezesPortableCustomScalarContract(t *testing.T) {
	t.Parallel()
	descriptor := customScalarDescriptor("Slug", []schema.JSONShape{schema.JSONString}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`"A"`), Canonical: json.RawMessage(`"a"`)},
		{Input: json.RawMessage(`"B"`), Canonical: json.RawMessage(`"b"`)},
	})
	catalog := schema.NewCatalog()
	if err := catalog.RegisterScalar(descriptor); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := snapshot.Lookup("Slug")
	if !ok || stored.Scalar == nil || stored.Scalar.Validator != schema.ScalarValidatorShape || len(stored.Scalar.Conformance) != 2 {
		t.Fatalf("portable scalar descriptor = %#v", stored)
	}
	encoded, err := json.Marshal(stored)
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("marshal scalar descriptor = %s, %v", encoded, err)
	}
	stored.Scalar.AcceptedWireShapes[0] = schema.JSONObject
	stored.Scalar.Conformance[0].Canonical[1] = 'z'
	again, _ := snapshot.Lookup("Slug")
	if again.Scalar.AcceptedWireShapes[0] != schema.JSONString || string(again.Scalar.Conformance[0].Canonical) != `"a"` {
		t.Fatal("snapshot scalar contract mutated through lookup")
	}
}

func TestCatalogRejectsIncompleteCustomScalarContracts(t *testing.T) {
	t.Parallel()
	base := customScalarDescriptor("Slug", []schema.JSONShape{schema.JSONString}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`"A"`), Canonical: json.RawMessage(`"a"`)},
		{Input: json.RawMessage(`"B"`), Canonical: json.RawMessage(`"b"`)},
	})
	tests := []struct {
		name   string
		mutate func(*schema.TypeDescriptor)
	}{
		{name: "wire shape", mutate: func(value *schema.TypeDescriptor) { value.Scalar.AcceptedWireShapes = nil }},
		{name: "validator", mutate: func(value *schema.TypeDescriptor) { value.Scalar.Validator = "" }},
		{name: "serializer", mutate: func(value *schema.TypeDescriptor) { value.Scalar.Serializer = "" }},
		{name: "canonicalizer", mutate: func(value *schema.TypeDescriptor) { value.Scalar.Canonicalizer = "" }},
		{name: "canonical profile", mutate: func(value *schema.TypeDescriptor) { value.Scalar.CanonicalProfile = "" }},
		{name: "limits", mutate: func(value *schema.TypeDescriptor) { value.Scalar.Limits.MaxBytes = -1 }},
		{name: "vectors", mutate: func(value *schema.TypeDescriptor) { value.Scalar.Conformance = value.Scalar.Conformance[:1] }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor := base
			contract := *base.Scalar
			descriptor.Scalar = &contract
			test.mutate(&descriptor)
			if err := schema.NewCatalog().RegisterScalar(descriptor); err == nil {
				t.Fatal("RegisterScalar accepted incomplete portable contract")
			}
		})
	}
}

func TestSnapshotAppliesOpenAndClosedVariantRules(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"id": {Type: schema.TypeID(schema.ID)}}},
		{ID: "OpenResult", Kind: schema.UnionType, Output: true, Open: true, Variants: []schema.TypeID{"User"}},
		{ID: "ClosedResult", Kind: schema.UnionType, Output: true, Variants: []schema.TypeID{"User"}},
		{ID: "Entity", Kind: schema.InterfaceType, Output: true, Variants: []schema.TypeID{"User"}, Fields: map[string]schema.FieldDescriptor{"id": {Type: schema.TypeID(schema.ID)}}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("Register(%s): %v", descriptor.ID, err)
		}
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	variant, known, err := snapshot.ResolveVariant("OpenResult", "User")
	if err != nil || !known || variant.ID != "User" {
		t.Fatalf("known variant = %#v, %t, %v", variant, known, err)
	}
	if _, known, err := snapshot.ResolveVariant("OpenResult", "FutureResult"); err != nil || known {
		t.Fatalf("open unknown variant = %t, %v", known, err)
	}
	for _, parent := range []schema.TypeID{"ClosedResult", "Entity"} {
		if _, _, err := snapshot.ResolveVariant(parent, "FutureResult"); err == nil {
			t.Fatalf("closed type %s accepted unknown variant", parent)
		}
	}
}
