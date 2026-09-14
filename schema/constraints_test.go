package schema_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestPortableConstraintsEnforceExactValuesAndPaths(t *testing.T) {
	t.Parallel()
	types, err := constrainedTypesResult(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  string
		code string
		path string
	}{
		{name: "scalar length counts astral as one", raw: `{"name":"😀","age":"18","tags":["one"],"labels":{"safe":"yes"}}`},
		{name: "combining sequences retain scalar count", raw: `{"name":"éé","age":"18","tags":["one"],"labels":{"safe":"yes"}}`, code: "CONSTRAINT_MAX_LENGTH", path: "/name"},
		{name: "exact numeric bound", raw: `{"name":"Ada","age":"17","tags":["one"],"labels":{"safe":"yes"}}`, code: "CONSTRAINT_MINIMUM", path: "/age"},
		{name: "unique values", raw: `{"name":"Ada","age":"18","tags":["one","one"],"labels":{"safe":"yes"}}`, code: "CONSTRAINT_UNIQUE_ITEMS", path: "/tags/1"},
		{name: "map key", raw: `{"name":"Ada","age":"18","tags":["one"],"labels":{"Bad":"yes"}}`, code: "CONSTRAINT_KEY_PATTERN", path: "/labels/Bad"},
		{name: "cross field rule", raw: `{"name":"Ada","age":"18","tags":["one"],"labels":{"safe":"yes"},"company":"Naatre"}`, code: "CONSTRAINT_RULE", path: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := schema.CoerceInput(types, "ProfileInput", json.RawMessage(test.raw), false)
			if test.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var constraintErr *schema.ConstraintError
			if !errors.As(err, &constraintErr) {
				t.Fatalf("CoerceInput error = %v, want constraint error", err)
			}
			violations := constraintErr.Violations()
			if len(violations) == 0 || violations[0].Code != test.code || violations[0].Path != test.path {
				t.Fatalf("violations = %#v, want %s at %s", violations, test.code, test.path)
			}
		})
	}
}

func TestConstraintMetadataRejectsUnsupportedRegexFormatsAndInvalidDefaults(t *testing.T) {
	t.Parallel()
	for _, constraints := range []schema.ConstraintSet{
		{Pattern: `(?=unsafe)`},
		{Format: &schema.FormatConstraint{ID: "vendor-clock", Assertion: true}},
		{MinLength: integerPointer(3), MaxLength: integerPointer(2)},
	} {
		if _, err := schema.ConstraintTrait(constraints); err == nil {
			t.Fatalf("ConstraintTrait(%#v) succeeded", constraints)
		}
	}
	invalidDefault := json.RawMessage(`"x"`)
	if _, err := constrainedTypesResult(t, &invalidDefault); err == nil {
		t.Fatal("catalog accepted a default that bypasses field constraints")
	}
}

func TestConstraintMetadataRejectsCELAndInvalidTypePlacement(t *testing.T) {
	t.Parallel()
	trait := schema.TraitDescriptor{
		ID: schema.ConstraintTraitID, Semantics: schema.TraitValidation,
		Value: json.RawMessage(`{"cel":"this.secret == true"}`),
	}
	_, _, err := schema.ParseConstraintTrait([]schema.TraitDescriptor{trait})
	var metadataErr *schema.ConstraintMetadataError
	if !errors.As(err, &metadataErr) || metadataErr.Code != "CONSTRAINT_FEATURE_UNSUPPORTED" || metadataErr.Pointer != "/cel" {
		t.Fatalf("CEL metadata error = %#v, %v", metadataErr, err)
	}

	minItems := 1
	invalidPlacement := constraintTrait(t, schema.ConstraintSet{MinItems: &minItems})
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{
		ID: "InvalidInput", Kind: schema.InputObjectType, Input: true,
		Fields: map[string]schema.FieldDescriptor{
			"name": {Type: schema.TypeID(schema.String), Traits: []schema.TraitDescriptor{invalidPlacement}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Freeze(); err == nil || !strings.Contains(err.Error(), "item constraints do not apply") {
		t.Fatalf("invalid constraint placement error = %v", err)
	}

	rule := schema.ConstraintRule{ID: "secret", Assert: schema.RuleExpression{Operator: schema.RulePresent, Field: "secret"}}
	catalog = schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{
		ID: "DeclaredInput", Kind: schema.InputObjectType, Input: true,
		Fields: map[string]schema.FieldDescriptor{"name": {Type: schema.TypeID(schema.String)}},
		Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{Rules: []schema.ConstraintRule{rule}})},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Freeze(); err == nil || !strings.Contains(err.Error(), "undeclared field") {
		t.Fatalf("undeclared rule field error = %v", err)
	}
}

func TestConstraintViolationsAreDeterministicSafeAndIsolated(t *testing.T) {
	t.Parallel()
	trait := constraintTrait(t, schema.ConstraintSet{MinLength: integerPointer(4), Pattern: `[A-Z]+`})
	err := schema.ValidateConstraintValue(json.RawMessage(`"ab"`), []schema.TraitDescriptor{trait})
	var constraintErr *schema.ConstraintError
	if !errors.As(err, &constraintErr) {
		t.Fatalf("ValidateConstraintValue error = %v", err)
	}
	violations := constraintErr.Violations()
	if got := []string{violations[0].ID, violations[1].ID}; !slices.Equal(got, []string{"minLength", "pattern"}) {
		t.Fatalf("violation order = %v", got)
	}
	if err.Error() != "input violates portable constraints" {
		t.Fatalf("public error leaked input: %q", err)
	}
	violations[0].Parameters["limit"] = "mutated"
	if constraintErr.Violations()[0].Parameters["limit"] != "4" {
		t.Fatal("constraint violation parameters were not isolated")
	}
}

func TestConstraintsRunAfterDefaultsAndCanonicalCoercion(t *testing.T) {
	t.Parallel()
	minimumProperties := 3
	maximumVATLength := 16
	zeroScale := 0
	rule := schema.ConstraintRule{ID: "company-requires-vat", Assert: schema.RuleExpression{
		Operator: schema.RuleOr,
		Children: []schema.RuleExpression{
			{Operator: schema.RuleAbsent, Field: "company"},
			{Operator: schema.RulePresent, Field: "vat"},
		},
	}}
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{
		ID: "CanonicalInput", Kind: schema.InputObjectType, Input: true,
		Fields: map[string]schema.FieldDescriptor{
			"company": {Type: schema.TypeID(schema.String), Required: true},
			"vat": {
				Type: schema.TypeID(schema.String), Nullable: true, Default: json.RawMessage(`"default"`),
				Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{MaxLength: &maximumVATLength})},
			},
			"amount": {
				Type: schema.TypeID(schema.Decimal), Required: true,
				Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{Scale: &zeroScale})},
			},
		},
		Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{MinProperties: &minimumProperties, Rules: []schema.ConstraintRule{rule}})},
	}); err != nil {
		t.Fatal(err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: `{"company":"Naatre","amount":"1.00"}`, want: `{"amount":"1","company":"Naatre","vat":"default"}`},
		{input: `{"company":"Naatre","amount":"1","vat":null}`, want: `{"amount":"1","company":"Naatre","vat":null}`},
	} {
		value, err := schema.CoerceInput(types, "CanonicalInput", json.RawMessage(test.input), false)
		if err != nil {
			t.Fatalf("CoerceInput(%s): %v", test.input, err)
		}
		encoded, err := value.MarshalJSON()
		if err != nil || string(encoded) != test.want {
			t.Fatalf("MarshalJSON(%s) = %s, %v, want %s", test.input, encoded, err, test.want)
		}
	}
}

func constrainedTypesResult(t testing.TB, invalidDefault *json.RawMessage) (schema.Snapshot, error) {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "Tags", Kind: schema.ListType, Input: true, Element: schema.TypeID(schema.String)},
		{ID: "Labels", Kind: schema.MapType, Input: true, Element: schema.TypeID(schema.String)},
	} {
		if err := catalog.Register(descriptor); err != nil {
			return schema.Snapshot{}, err
		}
	}
	name := schema.FieldDescriptor{Type: schema.TypeID(schema.String), Required: true, Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{MaxLength: integerPointer(3)})}}
	if invalidDefault != nil {
		name.Required = false
		name.Default = *invalidDefault
		name.Traits = []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{MinLength: integerPointer(2)})}
	}
	age := schema.FieldDescriptor{Type: schema.TypeID(schema.Int64), Required: true, Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{Minimum: numberPointer("18"), Maximum: numberPointer("9223372036854775807")})}}
	tags := schema.FieldDescriptor{Type: "Tags", Required: true, Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{MinItems: integerPointer(1), UniqueItems: true})}}
	labels := schema.FieldDescriptor{Type: "Labels", Required: true, Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{MinProperties: integerPointer(1), KeyPattern: `[a-z]+`})}}
	rule := schema.ConstraintRule{ID: "company-requires-vat", Assert: schema.RuleExpression{Operator: schema.RuleOr, Children: []schema.RuleExpression{{Operator: schema.RuleAbsent, Field: "company"}, {Operator: schema.RulePresent, Field: "vat"}}}}
	profile := schema.TypeDescriptor{
		ID: "ProfileInput", Kind: schema.InputObjectType, Input: true,
		Fields: map[string]schema.FieldDescriptor{
			"name": name, "age": age, "tags": tags, "labels": labels,
			"company": {Type: schema.TypeID(schema.String)}, "vat": {Type: schema.TypeID(schema.String)},
		},
		Traits: []schema.TraitDescriptor{constraintTrait(t, schema.ConstraintSet{Rules: []schema.ConstraintRule{rule}})},
	}
	if err := catalog.Register(profile); err != nil {
		return schema.Snapshot{}, err
	}
	return catalog.Freeze()
}

func constraintTrait(t testing.TB, constraints schema.ConstraintSet) schema.TraitDescriptor {
	t.Helper()
	trait, err := schema.ConstraintTrait(constraints)
	if err != nil {
		t.Fatal(err)
	}
	if trait.ID != schema.ConstraintTraitID || trait.Semantics != schema.TraitValidation || len(trait.Value) == 0 {
		t.Fatalf("invalid constraint trait encoding: %#v", trait)
	}
	return trait
}

func integerPointer(value int) *int { return &value }

func numberPointer(value string) *json.Number {
	number := json.Number(value)
	return &number
}
