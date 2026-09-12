package conformance_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
)

type languageFixture struct {
	Profile     string           `json:"profile"`
	Schema      string           `json:"schema"`
	Constructs  []string         `json:"constructs"`
	Expressions []string         `json:"expressions"`
	Vectors     []languageVector `json:"vectors"`
}

type languageVector struct {
	Name               string                  `json:"name"`
	Document           json.RawMessage         `json:"document"`
	EquivalentDocument json.RawMessage         `json:"equivalentDocument"`
	Valid              *bool                   `json:"valid"`
	Capabilities       []string                `json:"capabilities"`
	RequestVariables   json.RawMessage         `json:"requestVariables"`
	ExpectedData       json.RawMessage         `json:"expectedData"`
	ExpectedErrors     []languageExpectedError `json:"expectedErrors"`
	Code               string                  `json:"code"`
	Phase              string                  `json:"phase"`
	Pointer            string                  `json:"pointer"`
	HandlerStarts      *int                    `json:"handlerStarts"`
	// Note explains, for implementers of other SDKs, why a vector exists. It
	// carries no assertion.
	Note string `json:"note"`
}

type languageExpectedError struct {
	Code string `json:"code"`
	Path []any  `json:"path"`
}

func TestPortableLanguageContractVectors(t *testing.T) {
	t.Parallel()
	var fixture languageFixture
	readFixture(t, "language.json", &fixture)
	assertLanguageFixtureHeader(t, fixture)
	seenNames := make(map[string]bool, len(fixture.Vectors))
	seenTags := make(map[string]bool)
	for _, vector := range fixture.Vectors {
		assertLanguageVector(t, vector, seenNames)
		collectLanguageTags(t, vector.Document, seenTags)
	}
	assertLanguageCoverage(t, fixture, seenNames, seenTags)
}

func TestLanguageJSONSchemaHasClosedLocalReferences(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "spec", "v1", "language.schema.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("decode %s trailing data: %v", path, err)
	}
	definitions, ok := document["$defs"].(map[string]any)
	if !ok || len(definitions) == 0 || document["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatal("language schema requires draft 2020-12 definitions")
	}
	assertLocalSchemaReferences(t, document, definitions)
	assertSchemaUnionSize(t, definitions, "selection", len(coreSelectionTags))
	assertSchemaUnionSize(t, definitions, "expression", len(coreExpressionTags))
}

var coreSelectionTags = []string{
	"$call", "$current", "$field", "$fragment", "$index", "$map", "$meta",
	"$nest", "$page", "$parallel", "$pipeline", "$slice", "$unnest",
}

var coreExpressionTags = []string{"$current", "$literal", "$parent", "$result", "$var"}

func assertLanguageFixtureHeader(t *testing.T, fixture languageFixture) {
	t.Helper()
	if fixture.Profile != "core.language-1" || fixture.Schema != "spec/v1/language.schema.json" || len(fixture.Vectors) == 0 {
		t.Fatal("language fixture requires its exact profile, schema, and vectors")
	}
	assertExactStrings(t, "constructs", fixture.Constructs, coreSelectionTags)
	assertExactStrings(t, "expressions", fixture.Expressions, coreExpressionTags)
}

func assertLanguageVector(t *testing.T, vector languageVector, seenNames map[string]bool) {
	t.Helper()
	if vector.Name == "" || len(vector.Document) == 0 || vector.Valid == nil || seenNames[vector.Name] {
		t.Fatalf("language vector has incomplete or duplicate identity %q", vector.Name)
	}
	seenNames[vector.Name] = true
	assertJSONObject(t, vector.Name+" document", vector.Document)
	if len(vector.RequestVariables) > 0 {
		assertJSONObject(t, vector.Name+" request variables", vector.RequestVariables)
	}
	if len(vector.EquivalentDocument) > 0 {
		assertEquivalentDocuments(t, vector)
	}
	if *vector.Valid {
		if vector.Code != "" || vector.Phase != "" || vector.Pointer != "" || (len(vector.ExpectedData) == 0 && len(vector.ExpectedErrors) == 0) {
			t.Fatalf("valid language vector %q has inconsistent outcomes", vector.Name)
		}
		assertExpectedErrors(t, vector)
		return
	}
	if vector.Code == "" || vector.Phase != "validate" || vector.Pointer == "" || vector.HandlerStarts == nil || *vector.HandlerStarts != 0 || len(vector.ExpectedData) != 0 || len(vector.ExpectedErrors) != 0 {
		t.Fatalf("invalid language vector %q must be a zero-handler validation failure", vector.Name)
	}
}

func assertExpectedErrors(t *testing.T, vector languageVector) {
	t.Helper()
	for _, expected := range vector.ExpectedErrors {
		if expected.Code == "" || expected.Path == nil {
			t.Fatalf("language vector %q has incomplete expected error", vector.Name)
		}
	}
	if len(vector.ExpectedErrors) > 0 && vector.HandlerStarts == nil {
		t.Fatalf("language vector %q with execution errors requires handlerStarts", vector.Name)
	}
}

func assertEquivalentDocuments(t *testing.T, vector languageVector) {
	t.Helper()
	assertJSONObject(t, vector.Name+" equivalent document", vector.EquivalentDocument)
	first, err := protocol.CanonicalizeJSON(vector.Document, protocol.Limits{})
	if err != nil {
		t.Fatalf("canonicalize %q document: %v", vector.Name, err)
	}
	second, err := protocol.CanonicalizeJSON(vector.EquivalentDocument, protocol.Limits{})
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("equivalent documents for %q differ: %s != %s (%v)", vector.Name, first, second, err)
	}
}

func assertJSONObject(t *testing.T, name string, input json.RawMessage) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(input, &object); err != nil || object == nil {
		t.Fatalf("%s is not a JSON object: %v", name, err)
	}
}

func collectLanguageTags(t *testing.T, input json.RawMessage, seen map[string]bool) {
	t.Helper()
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		t.Fatalf("decode language tag input: %v", err)
	}
	walkLanguageValue(value, seen)
}

func walkLanguageValue(value any, seen map[string]bool) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			walkLanguageValue(item, seen)
		}
	case map[string]any:
		for name, item := range typed {
			if strings.HasPrefix(name, "$") {
				seen[name] = true
			}
			walkLanguageValue(item, seen)
		}
	}
}

func assertLanguageCoverage(t *testing.T, fixture languageFixture, names, tags map[string]bool) {
	t.Helper()
	for _, tag := range append(slices.Clone(fixture.Constructs), fixture.Expressions...) {
		if !tags[tag] {
			t.Fatalf("language fixtures do not exercise %s", tag)
		}
	}
	for _, name := range []string{
		"alias-and-binding-are-independent", "cross-parallel-result-reference",
		"empty-list-map", "failed-result-is-unavailable",
		"forward-result-reference", "implicit-list-item-mapping",
		"invalid-downstream-argument-after-write", "literal-var-shaped-map-is-inert",
		"inline-fragment-type-condition", "fragment-expansion-alias-collision",
		"fragment-non-null-parameter-rejects-null", "fragment-required-parameter-missing",
		"missing-variable-omits-optional-argument", "missing-variable-rejects-required-consumer",
		"null-result-rejects-non-null-consumer", "object-member-order-is-non-semantic",
		"out-of-range-index-is-path-error", "skipped-result-is-unavailable",
		"skipped-write-inside-query", "unnest-parent-collision",
		"parameterized-fragment-binding-shadowing-and-default",
	} {
		if !names[name] {
			t.Fatalf("language fixtures omit required scenario %q", name)
		}
	}
}

func assertExactStrings(t *testing.T, name string, actual, expected []string) {
	t.Helper()
	actual = slices.Clone(actual)
	expected = slices.Clone(expected)
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Fatalf("%s = %v, want %v", name, actual, expected)
	}
}

func assertLocalSchemaReferences(t *testing.T, value any, definitions map[string]any) {
	t.Helper()
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			assertLocalSchemaReferences(t, item, definitions)
		}
	case map[string]any:
		for name, item := range typed {
			if name == "$ref" {
				reference, ok := item.(string)
				const prefix = "#/$defs/"
				if !ok || !strings.HasPrefix(reference, prefix) || definitions[strings.TrimPrefix(reference, prefix)] == nil {
					t.Fatalf("language schema has unresolved local reference %v", item)
				}
			}
			assertLocalSchemaReferences(t, item, definitions)
		}
	}
}

func assertSchemaUnionSize(t *testing.T, definitions map[string]any, name string, expected int) {
	t.Helper()
	definition, ok := definitions[name].(map[string]any)
	if !ok {
		t.Fatalf("language schema omits %s definition", name)
	}
	variants, ok := definition["oneOf"].([]any)
	if !ok || len(variants) != expected {
		t.Fatalf("language schema %s variants = %d, want %d", name, len(variants), expected)
	}
}
