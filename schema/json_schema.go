package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/valksor/naatre/protocol"
)

const JSONSchema202012 = "https://json-schema.org/draft/2020-12/schema"

type JSONSchemaDiagnostic struct {
	Code    string `json:"code"`
	Pointer string `json:"pointer"`
	Keyword string `json:"keyword,omitempty"`
	Message string `json:"message"`
}

type JSONSchemaFidelity struct {
	Dialect     string                 `json:"dialect"`
	Exact       bool                   `json:"exact"`
	Diagnostics []JSONSchemaDiagnostic `json:"diagnostics"`
}

type JSONSchemaImportOptions struct {
	MaxBytes int
	MaxDepth int
	// Bundles is the only external reference source. Keys are exact absolute
	// reference strings; no URL is fetched by this package.
	Bundles map[string]json.RawMessage
}

type JSONSchemaError struct {
	diagnostics []JSONSchemaDiagnostic
}

func (e *JSONSchemaError) Error() string { return "JSON Schema mapping is not faithful" }

func (e *JSONSchemaError) Diagnostics() []JSONSchemaDiagnostic {
	return slices.Clone(e.diagnostics)
}

// ExportJSONSchemaConstraints maps the exact Draft 2020-12 constraint subset.
// Semantics that JSON Schema cannot preserve are rejected with diagnostics.
func ExportJSONSchemaConstraints(constraints ConstraintSet) ([]byte, JSONSchemaFidelity, error) {
	fidelity := JSONSchemaFidelity{Dialect: JSONSchema202012, Exact: true, Diagnostics: []JSONSchemaDiagnostic{}}
	if err := validateConstraintSet(constraints); err != nil {
		return nil, fidelity, err
	}
	unsupported := unsupportedConstraintMappings(constraints)
	if len(unsupported) != 0 {
		fidelity.Exact = false
		fidelity.Diagnostics = unsupported
		return nil, fidelity, &JSONSchemaError{diagnostics: unsupported}
	}
	document := map[string]any{"$schema": JSONSchema202012}
	putJSONSchemaNumeric(document, constraints)
	putJSONSchemaString(document, constraints)
	putJSONSchemaCollection(document, constraints)
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fidelity, fmt.Errorf("encode JSON Schema constraints: %w", err)
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{})
	if err != nil {
		return nil, fidelity, fmt.Errorf("canonicalize JSON Schema constraints: %w", err)
	}
	return canonical, fidelity, nil
}

// ImportJSONSchemaConstraints imports the exact supported Draft 2020-12
// vocabulary. Unsupported keywords and blocked references are failures, never
// silently dropped annotations.
func ImportJSONSchemaConstraints(input []byte, options JSONSchemaImportOptions) (ConstraintSet, JSONSchemaFidelity, error) {
	options = defaultJSONSchemaOptions(options)
	fidelity := JSONSchemaFidelity{Dialect: JSONSchema202012, Exact: true, Diagnostics: []JSONSchemaDiagnostic{}}
	if len(input) > options.MaxBytes {
		diagnostic := jsonSchemaDiagnostic("JSON_SCHEMA_LIMIT", "", "", "schema exceeds byte limit")
		return ConstraintSet{}, failedFidelity(fidelity, diagnostic), &JSONSchemaError{diagnostics: []JSONSchemaDiagnostic{diagnostic}}
	}
	constraints, diagnostics := importJSONSchemaDocument(input, options, 0, make(map[string]bool))
	if len(diagnostics) != 0 {
		return ConstraintSet{}, failedFidelity(fidelity, diagnostics...), &JSONSchemaError{diagnostics: diagnostics}
	}
	if err := validateConstraintSet(constraints); err != nil {
		diagnostic := jsonSchemaDiagnostic("JSON_SCHEMA_INVALID", "", "", err.Error())
		return ConstraintSet{}, failedFidelity(fidelity, diagnostic), &JSONSchemaError{diagnostics: []JSONSchemaDiagnostic{diagnostic}}
	}
	return constraints, fidelity, nil
}

func importJSONSchemaDocument(input []byte, options JSONSchemaImportOptions, depth int, active map[string]bool) (ConstraintSet, []JSONSchemaDiagnostic) {
	if depth > options.MaxDepth {
		return ConstraintSet{}, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_LIMIT", "", "$ref", "reference depth limit exceeded")}
	}
	document, diagnostics := decodeJSONObjectStrict(input)
	if len(diagnostics) != 0 {
		return ConstraintSet{}, diagnostics
	}
	if rawReference, ok := document["$ref"]; ok {
		return importJSONSchemaReference(document, rawReference, options, depth, active)
	}
	return importJSONSchemaKeywords(document)
}

func importJSONSchemaReference(document map[string]json.RawMessage, rawReference json.RawMessage, options JSONSchemaImportOptions, depth int, active map[string]bool) (ConstraintSet, []JSONSchemaDiagnostic) {
	var reference string
	if err := json.Unmarshal(rawReference, &reference); err != nil || reference == "" {
		return ConstraintSet{}, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_TYPE", "/$ref", "$ref", "$ref requires a non-empty string")}
	}
	if len(document) != 1 && (len(document) != 2 || document["$schema"] == nil) {
		return ConstraintSet{}, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_UNSUPPORTED_KEYWORD", "", "$ref", "$ref siblings are outside the portable subset")}
	}
	bundle, allowed := options.Bundles[reference]
	if !allowed {
		return ConstraintSet{}, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_REF_BLOCKED", "/$ref", "$ref", "reference is not in the pinned bundle allowlist")}
	}
	if active[reference] {
		return ConstraintSet{}, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_REF_CYCLE", "/$ref", "$ref", "reference cycle detected")}
	}
	if len(bundle) > options.MaxBytes {
		return ConstraintSet{}, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_LIMIT", "/$ref", "$ref", "referenced bundle exceeds byte limit")}
	}
	active[reference] = true
	constraints, diagnostics := importJSONSchemaDocument(bundle, options, depth+1, active)
	delete(active, reference)
	return constraints, prefixJSONSchemaDiagnostics(diagnostics, "/$ref")
}

func importJSONSchemaKeywords(document map[string]json.RawMessage) (ConstraintSet, []JSONSchemaDiagnostic) {
	if diagnostics := validateJSONSchemaKeywords(document); len(diagnostics) != 0 {
		return ConstraintSet{}, diagnostics
	}
	constraints := ConstraintSet{}
	diagnostics := importJSONSchemaNumeric(document, &constraints)
	diagnostics = append(diagnostics, importJSONSchemaString(document, &constraints)...)
	diagnostics = append(diagnostics, importJSONSchemaCollection(document, &constraints)...)
	return constraints, diagnostics
}

func validateJSONSchemaKeywords(document map[string]json.RawMessage) []JSONSchemaDiagnostic {
	allowed := map[string]bool{
		"$schema": true, "minimum": true, "maximum": true,
		"exclusiveMinimum": true, "exclusiveMaximum": true, "minLength": true,
		"maxLength": true, "pattern": true, "format": true, "minItems": true,
		"maxItems": true, "uniqueItems": true, "minProperties": true,
		"maxProperties": true, "propertyNames": true,
	}
	var diagnostics []JSONSchemaDiagnostic
	for _, keyword := range sortedKeys(document) {
		if !allowed[keyword] {
			diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_UNSUPPORTED_KEYWORD", "/"+escapeJSONPointer(keyword), keyword, "keyword is outside the portable subset"))
		}
	}
	if len(diagnostics) != 0 {
		return diagnostics
	}
	if rawDialect, ok := document["$schema"]; ok {
		var dialect string
		if err := json.Unmarshal(rawDialect, &dialect); err != nil {
			return []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_TYPE", "/$schema", "$schema", "$schema requires a string")}
		}
		if dialect != JSONSchema202012 {
			return []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_DIALECT", "/$schema", "$schema", "only Draft 2020-12 is supported")}
		}
	}
	return nil
}

func importJSONSchemaNumeric(document map[string]json.RawMessage, constraints *ConstraintSet) []JSONSchemaDiagnostic {
	var diagnostics []JSONSchemaDiagnostic
	if number, ok, err := numberKeyword(document, "minimum"); err != nil {
		diagnostics = append(diagnostics, *err)
	} else if ok {
		constraints.Minimum = number
	}
	if number, ok, err := numberKeyword(document, "exclusiveMinimum"); err != nil {
		diagnostics = append(diagnostics, *err)
	} else if ok {
		constraints.Minimum, constraints.ExclusiveMinimum = number, true
	}
	if number, ok, err := numberKeyword(document, "maximum"); err != nil {
		diagnostics = append(diagnostics, *err)
	} else if ok {
		constraints.Maximum = number
	}
	if number, ok, err := numberKeyword(document, "exclusiveMaximum"); err != nil {
		diagnostics = append(diagnostics, *err)
	} else if ok {
		constraints.Maximum, constraints.ExclusiveMaximum = number, true
	}
	if document["minimum"] != nil && document["exclusiveMinimum"] != nil {
		diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_CONFLICT", "", "minimum", "minimum and exclusiveMinimum cannot both map to one portable bound"))
	}
	if document["maximum"] != nil && document["exclusiveMaximum"] != nil {
		diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_CONFLICT", "", "maximum", "maximum and exclusiveMaximum cannot both map to one portable bound"))
	}
	return diagnostics
}

func importJSONSchemaString(document map[string]json.RawMessage, constraints *ConstraintSet) []JSONSchemaDiagnostic {
	var diagnostics []JSONSchemaDiagnostic
	constraints.MinLength = integerKeyword(document, "minLength", &diagnostics)
	constraints.MaxLength = integerKeyword(document, "maxLength", &diagnostics)
	if constraints.MinLength != nil || constraints.MaxLength != nil {
		constraints.LengthUnit = LengthUnicodeScalar
	}
	constraints.Pattern = optionalStringKeyword(document, "pattern", &diagnostics)
	if constraints.Pattern != "" {
		if _, err := compileConstraintPattern(constraints.Pattern, PatternSearch); err != nil {
			diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_UNSUPPORTED_PATTERN", "/pattern", "pattern", "pattern is outside the RE2-compatible subset"))
		}
	}
	if format := optionalStringKeyword(document, "format", &diagnostics); format != "" {
		constraints.Format = &FormatConstraint{ID: format, Assertion: false}
	}
	return diagnostics
}

func importJSONSchemaCollection(document map[string]json.RawMessage, constraints *ConstraintSet) []JSONSchemaDiagnostic {
	var diagnostics []JSONSchemaDiagnostic
	constraints.MinItems = integerKeyword(document, "minItems", &diagnostics)
	constraints.MaxItems = integerKeyword(document, "maxItems", &diagnostics)
	constraints.MinProperties = integerKeyword(document, "minProperties", &diagnostics)
	constraints.MaxProperties = integerKeyword(document, "maxProperties", &diagnostics)
	diagnostics = append(diagnostics, importJSONSchemaUniqueItems(document, constraints)...)
	diagnostics = append(diagnostics, importJSONSchemaPropertyNames(document, constraints)...)
	if constraints.Pattern != "" || constraints.KeyPattern != "" {
		constraints.PatternMode = PatternSearch
	}
	return diagnostics
}

func importJSONSchemaUniqueItems(document map[string]json.RawMessage, constraints *ConstraintSet) []JSONSchemaDiagnostic {
	raw := document["uniqueItems"]
	if raw == nil {
		return nil
	}
	if err := json.Unmarshal(raw, &constraints.UniqueItems); err != nil {
		return []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_TYPE", "/uniqueItems", "uniqueItems", "keyword requires a boolean")}
	}
	return nil
}

func importJSONSchemaPropertyNames(document map[string]json.RawMessage, constraints *ConstraintSet) []JSONSchemaDiagnostic {
	raw := document["propertyNames"]
	if raw == nil {
		return nil
	}
	var names map[string]json.RawMessage
	if err := json.Unmarshal(raw, &names); err != nil || len(names) != 1 || names["pattern"] == nil {
		return []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_UNSUPPORTED_KEYWORD", "/propertyNames", "propertyNames", "only propertyNames.pattern is supported")}
	}
	var diagnostics []JSONSchemaDiagnostic
	constraints.KeyPattern = optionalStringKeyword(names, "pattern", &diagnostics)
	if constraints.KeyPattern == "" {
		return diagnostics
	}
	if _, err := compileConstraintPattern(constraints.KeyPattern, PatternSearch); err != nil {
		diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_UNSUPPORTED_PATTERN", "/propertyNames/pattern", "pattern", "pattern is outside the RE2-compatible subset"))
	}
	return diagnostics
}

func unsupportedConstraintMappings(constraints ConstraintSet) []JSONSchemaDiagnostic {
	var diagnostics []JSONSchemaDiagnostic
	if constraints.Precision != nil {
		diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_UNMAPPABLE", "/precision", "precision", "decimal precision has no exact core keyword"))
	}
	if constraints.Scale != nil {
		diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_UNMAPPABLE", "/scale", "scale", "decimal scale has no exact core keyword"))
	}
	if constraints.LengthUnit == LengthBytes {
		diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_UNMAPPABLE", "/lengthUnit", "lengthUnit", "byte length differs from JSON Schema string length"))
	}
	if constraints.Format != nil && constraints.Format.Assertion {
		diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_UNMAPPABLE", "/format", "format", "format assertion vocabulary is not implied by the core dialect"))
	}
	if len(constraints.Rules) != 0 {
		diagnostics = append(diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_UNMAPPABLE", "/rules", "rules", "native cross-field rules are not exported as a weaker schema"))
	}
	return diagnostics
}

func putJSONSchemaNumeric(document map[string]any, constraints ConstraintSet) {
	if constraints.Minimum != nil {
		keyword := "minimum"
		if constraints.ExclusiveMinimum {
			keyword = "exclusiveMinimum"
		}
		document[keyword] = *constraints.Minimum
	}
	if constraints.Maximum != nil {
		keyword := "maximum"
		if constraints.ExclusiveMaximum {
			keyword = "exclusiveMaximum"
		}
		document[keyword] = *constraints.Maximum
	}
}

func putJSONSchemaString(document map[string]any, constraints ConstraintSet) {
	putOptionalInteger(document, "minLength", constraints.MinLength)
	putOptionalInteger(document, "maxLength", constraints.MaxLength)
	if constraints.Pattern != "" {
		pattern := constraints.Pattern
		if constraints.PatternMode == "" || constraints.PatternMode == PatternFull {
			pattern = "^(?:" + pattern + ")$"
		}
		document["pattern"] = pattern
	}
	if constraints.Format != nil {
		document["format"] = constraints.Format.ID
	}
}

func putJSONSchemaCollection(document map[string]any, constraints ConstraintSet) {
	putOptionalInteger(document, "minItems", constraints.MinItems)
	putOptionalInteger(document, "maxItems", constraints.MaxItems)
	if constraints.UniqueItems {
		document["uniqueItems"] = true
	}
	putOptionalInteger(document, "minProperties", constraints.MinProperties)
	putOptionalInteger(document, "maxProperties", constraints.MaxProperties)
	if constraints.KeyPattern != "" {
		pattern := constraints.KeyPattern
		if constraints.PatternMode == "" || constraints.PatternMode == PatternFull {
			pattern = "^(?:" + pattern + ")$"
		}
		document["propertyNames"] = map[string]any{"pattern": pattern}
	}
}

func defaultJSONSchemaOptions(options JSONSchemaImportOptions) JSONSchemaImportOptions {
	if options.MaxBytes == 0 {
		options.MaxBytes = 1 << 20
	}
	if options.MaxDepth == 0 {
		options.MaxDepth = 16
	}
	return options
}

func decodeJSONObjectStrict(input []byte) (map[string]json.RawMessage, []JSONSchemaDiagnostic) {
	if err := protocol.ValidateJSON(input, protocol.Limits{}); err != nil {
		return nil, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_INVALID", "", "", "document is not strict JSON")}
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var document map[string]json.RawMessage
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_TYPE", "", "", "document requires an object")}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, []JSONSchemaDiagnostic{jsonSchemaDiagnostic("JSON_SCHEMA_INVALID", "", "", "document has trailing content")}
	}
	return document, nil
}

func optionalStringKeyword(document map[string]json.RawMessage, keyword string, diagnostics *[]JSONSchemaDiagnostic) string {
	raw, ok := document[keyword]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		*diagnostics = append(*diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_TYPE", "/"+keyword, keyword, "keyword requires a string"))
		return ""
	}
	return value
}

func numberKeyword(document map[string]json.RawMessage, keyword string) (*json.Number, bool, *JSONSchemaDiagnostic) {
	raw, ok := document[keyword]
	if !ok {
		return nil, false, nil
	}
	text := strings.TrimSpace(string(raw))
	if _, valid := exactDecimalRat(text); !valid {
		diagnostic := jsonSchemaDiagnostic("JSON_SCHEMA_TYPE", "/"+keyword, keyword, "keyword requires an exact JSON number")
		return nil, false, &diagnostic
	}
	number := json.Number(text)
	return &number, true, nil
}

func integerKeyword(document map[string]json.RawMessage, keyword string, diagnostics *[]JSONSchemaDiagnostic) *int {
	raw, ok := document[keyword]
	if !ok {
		return nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
		*diagnostics = append(*diagnostics, jsonSchemaDiagnostic("JSON_SCHEMA_TYPE", "/"+keyword, keyword, "keyword requires a non-negative integer"))
		return nil
	}
	return &value
}

func putOptionalInteger(document map[string]any, keyword string, value *int) {
	if value != nil {
		document[keyword] = *value
	}
}

func jsonSchemaDiagnostic(code, pointer, keyword, message string) JSONSchemaDiagnostic {
	diagnostic := JSONSchemaDiagnostic{Code: code, Pointer: pointer, Keyword: keyword, Message: message}
	if diagnostic.Message == "" {
		diagnostic.Message = "JSON Schema mapping failed"
	}
	return diagnostic
}

func failedFidelity(fidelity JSONSchemaFidelity, diagnostics ...JSONSchemaDiagnostic) JSONSchemaFidelity {
	fidelity.Exact = false
	fidelity.Diagnostics = slices.Clone(diagnostics)
	return fidelity
}

func prefixJSONSchemaDiagnostics(diagnostics []JSONSchemaDiagnostic, prefix string) []JSONSchemaDiagnostic {
	return mapSlice(diagnostics, func(diagnostic JSONSchemaDiagnostic) JSONSchemaDiagnostic {
		diagnostic.Pointer = prefix + diagnostic.Pointer
		return diagnostic
	})
}
