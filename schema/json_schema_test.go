package schema_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestJSONSchemaConstraintMappingIsExactAndDeterministic(t *testing.T) {
	t.Parallel()
	constraints := schema.ConstraintSet{
		Minimum: numberPointer("-1.25"), Maximum: numberPointer("10"), ExclusiveMaximum: true,
		MinLength: integerPointer(1), MaxLength: integerPointer(4), Pattern: `[a-z]+`,
		MinItems: integerPointer(1), MaxItems: integerPointer(3), UniqueItems: true,
		MinProperties: integerPointer(1), KeyPattern: `[a-z]+`,
		Format: &schema.FormatConstraint{ID: "email"},
	}
	first, fidelity, err := schema.ExportJSONSchemaConstraints(constraints)
	if err != nil || !fidelity.Exact || len(fidelity.Diagnostics) != 0 {
		t.Fatalf("ExportJSONSchemaConstraints = %s, %#v, %v", first, fidelity, err)
	}
	second, _, err := schema.ExportJSONSchemaConstraints(constraints)
	if err != nil || string(first) != string(second) {
		t.Fatalf("nondeterministic JSON Schema output: %s != %s, %v", first, second, err)
	}
	imported, importedFidelity, err := schema.ImportJSONSchemaConstraints(first, schema.JSONSchemaImportOptions{})
	if err != nil || !importedFidelity.Exact || imported.Minimum.String() != "-1.25" || !imported.ExclusiveMaximum || imported.PatternMode != schema.PatternSearch {
		t.Fatalf("ImportJSONSchemaConstraints = %#v, %#v, %v", imported, importedFidelity, err)
	}
}

func TestJSONSchemaMappingFailsClosedForUnsupportedSemantics(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","oneOf":[{"type":"string"}]}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#"}`,
		`{"$schema":7}`,
		`{"$id":"https://example.test/schema"}`,
		`{"$ref":"https://example.invalid/schema.json"}`,
		`{"$ref":7}`,
		`{"pattern":"(?=unsupported)"}`,
		`{"minimum":0,"exclusiveMinimum":1}`,
	} {
		_, fidelity, err := schema.ImportJSONSchemaConstraints([]byte(input), schema.JSONSchemaImportOptions{})
		var mappingErr *schema.JSONSchemaError
		if !errors.As(err, &mappingErr) || fidelity.Exact || len(mappingErr.Diagnostics()) == 0 {
			t.Fatalf("ImportJSONSchemaConstraints(%s) = %#v, %v", input, fidelity, err)
		}
	}
	_, fidelity, err := schema.ExportJSONSchemaConstraints(schema.ConstraintSet{Scale: integerPointer(2)})
	if err == nil || fidelity.Exact || fidelity.Diagnostics[0].Code != "JSON_SCHEMA_UNMAPPABLE" {
		t.Fatalf("unmappable export = %#v, %v", fidelity, err)
	}
}

func TestJSONSchemaReferencesUseOnlyPinnedBoundedBundles(t *testing.T) {
	t.Parallel()
	reference := "https://schemas.example.test/name"
	bundle := json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","minLength":2}`)
	constraints, fidelity, err := schema.ImportJSONSchemaConstraints([]byte(`{"$ref":"`+reference+`"}`), schema.JSONSchemaImportOptions{Bundles: map[string]json.RawMessage{reference: bundle}})
	if err != nil || !fidelity.Exact || constraints.MinLength == nil || *constraints.MinLength != 2 {
		t.Fatalf("pinned import = %#v, %#v, %v", constraints, fidelity, err)
	}
	cycleA := "https://schemas.example.test/a"
	cycleB := "https://schemas.example.test/b"
	_, _, err = schema.ImportJSONSchemaConstraints([]byte(`{"$ref":"`+cycleA+`"}`), schema.JSONSchemaImportOptions{Bundles: map[string]json.RawMessage{
		cycleA: json.RawMessage(`{"$ref":"` + cycleB + `"}`),
		cycleB: json.RawMessage(`{"$ref":"` + cycleA + `"}`),
	}})
	var mappingErr *schema.JSONSchemaError
	if !errors.As(err, &mappingErr) || !strings.Contains(mappingErr.Diagnostics()[0].Code, "CYCLE") {
		t.Fatalf("cycle error = %v", err)
	}
}
