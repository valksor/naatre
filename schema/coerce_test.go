package schema_test

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func TestCoerceInputPreservesMissingNullAndCompositeBoundaries(t *testing.T) {
	t.Parallel()
	types := inputTypes(t)

	missing, err := schema.CoerceInput(types, schema.TypeID(schema.String), nil, false)
	if err != nil || !missing.IsMissing() || missing.IsNull() {
		t.Fatalf("missing input = %#v, %v", missing, err)
	}
	null, err := schema.CoerceInput(types, schema.TypeID(schema.String), json.RawMessage(`null`), true)
	if err != nil || null.IsMissing() || !null.IsNull() {
		t.Fatalf("null input = %#v, %v", null, err)
	}
	if _, err := schema.CoerceInput(types, schema.TypeID(schema.String), json.RawMessage(`null`), false); err == nil {
		t.Fatal("non-null scalar accepted null")
	}

	object, err := schema.CoerceInput(types, "ProfileInput", json.RawMessage(`{"nickname":null}`), false)
	if err != nil || object.Type() != "ProfileInput" {
		t.Fatalf("CoerceInput(ProfileInput): %v", err)
	}
	members, ok := object.Object()
	if !ok || !members["name"].IsMissing() || !members["nickname"].IsNull() {
		t.Fatalf("object presence = %#v", members)
	}
	members["name"] = null
	again, _ := object.Object()
	if !again["name"].IsMissing() {
		t.Fatal("object mutated through accessor")
	}

	list, err := schema.CoerceInput(types, "NullableStrings", json.RawMessage(`[null,"x"]`), false)
	if err != nil {
		t.Fatalf("CoerceInput(NullableStrings): %v", err)
	}
	items, ok := list.List()
	if !ok || len(items) != 2 || !items[0].IsNull() || items[1].IsNull() {
		t.Fatalf("list presence = %#v", items)
	}
	if _, err := schema.CoerceInput(types, "Strings", json.RawMessage(`[null]`), false); err == nil {
		t.Fatal("non-null list element accepted null")
	}
}

func TestCoerceInputCanonicalizesMapsDefaultsAndOneOf(t *testing.T) {
	t.Parallel()
	types := inputTypes(t)

	value, err := schema.CoerceInput(types, "StringMap", json.RawMessage(`{"😀":"face","0":"zero","":"empty","$literal":"inert"}`), false)
	if err != nil {
		t.Fatalf("CoerceInput(StringMap): %v", err)
	}
	canonical, err := value.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(canonical) != `{"":"empty","$literal":"inert","0":"zero","😀":"face"}` {
		t.Fatalf("canonical map = %s", canonical)
	}
	utf16Order, err := schema.CoerceInput(types, "StringMap", json.RawMessage(`{"":"bmp","😀":"astral"}`), false)
	if err != nil {
		t.Fatalf("CoerceInput(UTF-16 order): %v", err)
	}
	orderedJSON, _ := utf16Order.MarshalJSON()
	if string(orderedJSON) != `{"😀":"astral","":"bmp"}` {
		t.Fatalf("UTF-16 canonical map = %s", orderedJSON)
	}
	empty, err := schema.CoerceInput(types, "StringMap", json.RawMessage(`{}`), false)
	if err != nil {
		t.Fatal(err)
	}
	emptyJSON, _ := empty.MarshalJSON()
	if string(emptyJSON) != `{}` {
		t.Fatalf("empty map = %s", emptyJSON)
	}

	defaulted, err := schema.CoerceInput(types, "DefaultInput", json.RawMessage(`{}`), false)
	if err != nil {
		t.Fatalf("CoerceInput(DefaultInput): %v", err)
	}
	defaultJSON, _ := defaulted.MarshalJSON()
	if string(defaultJSON) != `{"count":"9007199254740993"}` {
		t.Fatalf("defaulted input = %s", defaultJSON)
	}
	defaultMembers, _ := defaulted.Object()
	count, ok := defaultMembers["count"].Scalar()
	countJSON, countErr := count.MarshalJSON()
	if !ok || countErr != nil || string(countJSON) != `"9007199254740993"` {
		t.Fatalf("default scalar = %s, %t, %v", countJSON, ok, countErr)
	}

	choice, err := schema.CoerceInput(types, "Contact", json.RawMessage(`{"email":null}`), false)
	if err != nil || choice.ActiveMember() != "email" {
		t.Fatalf("nullable one-of = %#v, %v", choice, err)
	}
	for _, input := range []string{`{}`, `{"email":null,"phone":"1"}`} {
		if _, err := schema.CoerceInput(types, "Contact", json.RawMessage(input), false); err == nil {
			t.Fatalf("one-of accepted %s", input)
		}
	}
	defaultChoice, err := schema.CoerceInput(types, "DefaultContact", json.RawMessage(`{}`), false)
	if err != nil || defaultChoice.ActiveMember() != "phone" {
		t.Fatalf("defaulted one-of = %#v, %v", defaultChoice, err)
	}
}

func TestCoerceInputRejectsDuplicateMembersAndUnpairedSurrogates(t *testing.T) {
	t.Parallel()
	types := inputTypes(t)
	for _, input := range []string{
		`{"nickname":"first","nickname":"second"}`,
		`{"nickname":"\ud800"}`,
		`{"\ud800":"value"}`,
	} {
		if _, err := schema.CoerceInput(types, "ProfileInput", json.RawMessage(input), false); err == nil {
			t.Fatalf("strict input coercion accepted %s", input)
		}
	}
}

func TestCoerceInputModelsOpenEnumsAndBoundsRecursiveValues(t *testing.T) {
	t.Parallel()
	types := inputTypes(t)

	open, err := schema.CoerceInput(types, "OpenColor", json.RawMessage(`"GREEN"`), false)
	if err != nil {
		t.Fatalf("open enum: %v", err)
	}
	spelling, known, ok := open.Enum()
	if !ok || known || spelling != "GREEN" {
		t.Fatalf("open enum state = %q, %t, %t", spelling, known, ok)
	}
	if _, err := schema.CoerceInput(types, "ClosedColor", json.RawMessage(`"GREEN"`), false); err == nil {
		t.Fatal("closed enum accepted unknown value")
	}
	if _, err := schema.CoerceInput(types, "InputNode", json.RawMessage(`{"next":{"next":{"next":null}}}`), false); err == nil {
		t.Fatal("recursive input exceeded declared maximum depth")
	}
}

func TestCustomScalarUsesPortableClosedProfile(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	descriptor := customScalarDescriptor("Slug", []schema.JSONShape{schema.JSONString}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`"Mixed-Case"`), Canonical: json.RawMessage(`"mixed-case"`)},
		{Input: json.RawMessage(`"ÉSECOND"`), Canonical: json.RawMessage(`"Ésecond"`)},
	})
	if err := catalog.RegisterScalar(descriptor); err != nil {
		t.Fatalf("RegisterScalar(Slug): %v", err)
	}
	unsupported := customScalarDescriptor("Unstable", []schema.JSONShape{schema.JSONString}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`"x"`), Canonical: json.RawMessage(`"x"`)},
		{Input: json.RawMessage(`"y"`), Canonical: json.RawMessage(`"y"`)},
	})
	unsupported.Scalar.Canonicalizer = "example.clock-dependent-1"
	if err := catalog.RegisterScalar(unsupported); err == nil {
		t.Fatal("unimplemented process-state-dependent profile was accepted")
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	slug, err := schema.CoerceInput(types, "Slug", json.RawMessage(`"Mixed-Case"`), false)
	if err != nil {
		t.Fatalf("CoerceInput(Slug): %v", err)
	}
	canonical, _ := slug.MarshalJSON()
	if string(canonical) != `"mixed-case"` {
		t.Fatalf("canonical slug = %s", canonical)
	}
	digest, err := protocol.SemanticHash(protocol.SchemaHash, canonical)
	if err != nil || digest.Hex == "" {
		t.Fatalf("hash canonical custom scalar: %#v, %v", digest, err)
	}

	const workers = 32
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			value, workerErr := schema.CanonicalizeScalar(types, "Slug", json.RawMessage(`"Concurrent"`))
			if workerErr != nil {
				t.Errorf("concurrent canonicalization: %v", workerErr)
				return
			}
			encoded, _ := value.MarshalJSON()
			if string(encoded) != `"concurrent"` {
				t.Errorf("concurrent canonical = %s", encoded)
			}
		}()
	}
	wait.Wait()
}

func TestCustomScalarCanonicalizesJSONWithRFC8785(t *testing.T) {
	t.Parallel()
	descriptor := customScalarDescriptor("Pair", []schema.JSONShape{schema.JSONObject}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`{"b":1.0,"a":2}`), Canonical: json.RawMessage(`{"a":2,"b":1}`)},
		{Input: json.RawMessage(`{"b":3.0,"a":4}`), Canonical: json.RawMessage(`{"a":4,"b":3}`)},
	})
	catalog := schema.NewCatalog()
	if err := catalog.RegisterScalar(descriptor); err != nil {
		t.Fatalf("RegisterScalar(Pair): %v", err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	value, err := schema.CanonicalizeScalar(types, "Pair", json.RawMessage(`{"b":5.0,"a":6}`))
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := value.MarshalJSON()
	if string(canonical) != `{"a":6,"b":5}` {
		t.Fatalf("canonical pair = %s", canonical)
	}
}

func TestCustomScalarRejectsNullAsNumber(t *testing.T) {
	t.Parallel()
	descriptor := customScalarDescriptor("Count", []schema.JSONShape{schema.JSONNumber}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`1`), Canonical: json.RawMessage(`1`)},
		{Input: json.RawMessage(`2.0`), Canonical: json.RawMessage(`2`)},
	})
	catalog := schema.NewCatalog()
	if err := catalog.RegisterScalar(descriptor); err != nil {
		t.Fatalf("RegisterScalar(Count): %v", err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schema.CanonicalizeScalar(types, "Count", json.RawMessage(`null`)); err == nil {
		t.Fatal("number-shaped custom scalar accepted null")
	}
}

func TestCoerceRuntimeInputAcceptsCompletedOutputValuesButClientInputDoesNot(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"id": {Type: schema.TypeID(schema.ID), Required: true},
		}},
		{ID: "ConsumeInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"user": {Type: "User", Required: true},
		}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("Register(%s): %v", descriptor.ID, err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	raw := json.RawMessage(`{"user":{"id":"u-1"}}`)
	if _, err := schema.CoerceInput(types, "ConsumeInput", raw, false); err == nil {
		t.Fatal("client input introduced an output-only object")
	}
	value, err := schema.CoerceRuntimeInput(types, "ConsumeInput", raw, false)
	if err != nil {
		t.Fatalf("CoerceRuntimeInput: %v", err)
	}
	members, ok := value.Object()
	if !ok || members["user"].Type() != "User" {
		t.Fatalf("runtime input = %#v", value)
	}
}

func inputTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	descriptors := []schema.TypeDescriptor{
		{ID: "Strings", Kind: schema.ListType, Input: true, Element: schema.TypeID(schema.String)},
		{ID: "NullableStrings", Kind: schema.ListType, Input: true, Element: schema.TypeID(schema.String), ElementNullable: true},
		{ID: "StringMap", Kind: schema.MapType, Input: true, Element: schema.TypeID(schema.String)},
		{ID: "ProfileInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String)}, "nickname": {Type: schema.TypeID(schema.String), Nullable: true},
		}},
		{ID: "DefaultInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"count": {Type: schema.TypeID(schema.Int64), Default: json.RawMessage(`"9007199254740993"`)},
		}},
		{ID: "Contact", Kind: schema.OneOfType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"email": {Type: schema.TypeID(schema.String), Nullable: true}, "phone": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "DefaultContact", Kind: schema.OneOfType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"email": {Type: schema.TypeID(schema.String)}, "phone": {Type: schema.TypeID(schema.String), Default: json.RawMessage(`"1"`)},
		}},
		{ID: "OpenColor", Kind: schema.EnumType, Input: true, Open: true, EnumValues: []string{"RED", "BLUE"}},
		{ID: "ClosedColor", Kind: schema.EnumType, Input: true, EnumValues: []string{"RED", "BLUE"}},
		{ID: "InputNode", Kind: schema.InputObjectType, Input: true, MaxDepth: 1, Fields: map[string]schema.FieldDescriptor{
			"next": {Type: "InputNode", Nullable: true},
		}},
	}
	for _, descriptor := range descriptors {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("Register(%s): %v", descriptor.ID, err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return types
}

func customScalarDescriptor(id schema.TypeID, shapes []schema.JSONShape, vectors []schema.ScalarConformanceVector) schema.TypeDescriptor {
	canonicalizer := schema.ScalarCanonicalJSON
	if len(shapes) == 1 && shapes[0] == schema.JSONString {
		canonicalizer = schema.ScalarCanonicalASCIILowercaseString
	}
	return schema.TypeDescriptor{
		ID: id, Kind: schema.ScalarType, Input: true, Output: true,
		Scalar: &schema.ScalarDescriptor{
			AcceptedWireShapes: shapes,
			Validator:          schema.ScalarValidatorShape,
			Serializer:         schema.ScalarSerializerIdentity,
			Canonicalizer:      canonicalizer,
			CanonicalProfile:   "c14n-1",
			Limits:             schema.ScalarLimits{MaxBytes: 1024, MaxDepth: 8, MaxMembers: 8, MaxArrayItems: 8, MaxStringBytes: 512, MaxNumberBytes: 64, MaxTokens: 32},
			Conformance:        vectors,
		},
	}
}
