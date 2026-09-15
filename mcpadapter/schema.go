package mcpadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/protocol"
)

var schemaKeywords = map[string]bool{
	"$schema": true, "type": true, "properties": true, "required": true,
	"additionalProperties": true, "items": true, "oneOf": true, "const": true,
	"enum": true, "format": true, "pattern": true, "minimum": true,
	"maximum": true, "minLength": true, "maxLength": true, "minItems": true,
	"maxItems": true, "description": true, "title": true, "x-naatre-scalar": true,
}

// ValidateSchema accepts the declared lossless MCP JSON Schema subset. It is
// intentionally narrower than JSON Schema Draft 2020-12: references, open
// objects, defaults, unevaluated vocabularies, and ambiguous unions fail
// before registration.
func ValidateSchema(input []byte, limits Limits) ([]byte, error) {
	limits = withDefaultLimits(limits)
	if len(input) == 0 || len(input) > limits.MaxSchemaBytes {
		return nil, adapterError("MCP_SCHEMA_LIMIT", errors.New("schema is empty or exceeds the configured byte limit"))
	}
	if err := protocol.ValidateJSON(input, protocol.Limits{MaxBytes: limits.MaxSchemaBytes, MaxDepth: limits.MaxSchemaDepth}); err != nil {
		return nil, adapterError("MCP_SCHEMA_INVALID", err)
	}
	value, err := protocol.DecodeJSONValue(input)
	if err != nil {
		return nil, adapterError("MCP_SCHEMA_INVALID", err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, adapterError("MCP_SCHEMA_INVALID", errors.New("schema must be an object"))
	}
	if err := validateSchemaNode(root, limits, 0, ""); err != nil {
		return nil, err
	}
	canonical, err := protocol.CanonicalizeJSON(input, protocol.Limits{MaxBytes: limits.MaxSchemaBytes, MaxDepth: limits.MaxSchemaDepth})
	if err != nil {
		return nil, adapterError("MCP_SCHEMA_INVALID", err)
	}
	return canonical, nil
}

func validateSchemaNode(node map[string]any, limits Limits, depth int, path string) error {
	if err := validateSchemaNodeHeader(node, limits, depth, path); err != nil {
		return err
	}
	types, err := schemaTypes(node["type"])
	if err != nil {
		return err
	}
	if err := validateExtendedScalar(node, types); err != nil {
		return err
	}
	if err := validateObjectSchema(node, types, limits, depth, path); err != nil {
		return err
	}
	if err := validateArraySchema(node, types, limits, depth, path); err != nil {
		return err
	}
	if err := validateNumericSchema(node, types); err != nil {
		return err
	}
	return validateUnionSchema(node, limits, depth, path)
}

func validateSchemaNodeHeader(node map[string]any, limits Limits, depth int, path string) error {
	if depth > limits.MaxSchemaDepth {
		return adapterError("MCP_SCHEMA_LIMIT", errors.New("schema depth exceeds the configured limit"))
	}
	for _, key := range sortedAnyKeys(node) {
		if !schemaKeywords[key] {
			return adapterError("MCP_SCHEMA_UNSUPPORTED", fmt.Errorf("unsupported schema keyword at %s/%s", path, key))
		}
	}
	if node["type"] == nil && node["oneOf"] == nil && node["const"] == nil && node["enum"] == nil {
		return adapterError("MCP_SCHEMA_INVALID", errors.New("schema node requires type, oneOf, const, or enum"))
	}
	if err := validateKeywordShapes(node); err != nil {
		return err
	}
	if dialect, ok := node["$schema"]; ok && dialect != "https://json-schema.org/draft/2020-12/schema" {
		return adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("only JSON Schema Draft 2020-12 is supported"))
	}
	return nil
}

func validateExtendedScalar(node map[string]any, types []string) error {
	if scalar, ok := node["x-naatre-scalar"]; ok {
		name, valid := scalar.(string)
		if !valid || !slices.Contains([]string{"int64", "uint64", "bigint", "decimal", "timestamp", "uuid", "bytes"}, name) || !slices.Equal(types, []string{"string"}) {
			return adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("extended scalars require a known string representation"))
		}
	}
	return nil
}

func validateKeywordShapes(node map[string]any) error {
	if err := validateStringKeywords(node); err != nil {
		return err
	}
	if err := validateNumberKeywords(node); err != nil {
		return err
	}
	return validateEnumKeyword(node)
}

func validateStringKeywords(node map[string]any) error {
	for _, key := range []string{"description", "title", "format", "pattern", "x-naatre-scalar"} {
		if value, ok := node[key]; ok {
			text, valid := value.(string)
			if !valid || len(text) > 4096 {
				return adapterError("MCP_SCHEMA_INVALID", fmt.Errorf("%s must be a bounded string", key))
			}
			if key == "pattern" {
				if _, err := regexp.Compile(text); err != nil {
					return adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("pattern is outside the RE2 subset"))
				}
			}
		}
	}
	return nil
}

func validateNumberKeywords(node map[string]any) error {
	for _, key := range []string{"minimum", "maximum"} {
		if value, ok := node[key]; ok {
			if _, valid := jsonNumber(value); !valid {
				return adapterError("MCP_SCHEMA_INVALID", fmt.Errorf("%s must be a finite JSON number", key))
			}
		}
	}
	for _, key := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		if value, ok := node[key]; ok {
			number, valid := value.(json.Number)
			integer, err := number.Int64()
			if !valid || err != nil || integer < 0 {
				return adapterError("MCP_SCHEMA_INVALID", fmt.Errorf("%s must be a non-negative integer", key))
			}
		}
	}
	return nil
}

func validateEnumKeyword(node map[string]any) error {
	if value, ok := node["enum"]; ok {
		values, valid := value.([]any)
		if !valid || len(values) == 0 || len(values) > 256 {
			return adapterError("MCP_SCHEMA_INVALID", errors.New("enum must contain one to 256 values"))
		}
		seen := make(map[string]bool, len(values))
		for _, entry := range values {
			encoded, err := json.Marshal(entry)
			if err != nil || seen[string(encoded)] {
				return adapterError("MCP_SCHEMA_INVALID", errors.New("enum values must be unique JSON values"))
			}
			seen[string(encoded)] = true
		}
	}
	return nil
}

func validateObjectSchema(node map[string]any, types []string, limits Limits, depth int, path string) error {
	if !slices.Contains(types, "object") {
		return nil
	}
	additional, present := node["additionalProperties"]
	if !present || additional != false {
		return adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("lossless objects must reject additional properties"))
	}
	properties, ok := node["properties"].(map[string]any)
	if !ok {
		return adapterError("MCP_SCHEMA_INVALID", errors.New("object properties must be an object"))
	}
	for _, name := range sortedAnyKeys(properties) {
		child, ok := properties[name].(map[string]any)
		if !ok {
			return adapterError("MCP_SCHEMA_INVALID", errors.New("property schema must be an object"))
		}
		if err := validateSchemaNode(child, limits, depth+1, path+"/properties/"+name); err != nil {
			return err
		}
	}
	return validateRequiredProperties(node["required"], properties)
}

func validateRequiredProperties(required any, properties map[string]any) error {
	if required == nil {
		return nil
	}
	values, ok := required.([]any)
	if !ok {
		return adapterError("MCP_SCHEMA_INVALID", errors.New("required must be an array"))
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		name, ok := value.(string)
		if !ok || properties[name] == nil || seen[name] {
			return adapterError("MCP_SCHEMA_INVALID", errors.New("required names must be unique declared properties"))
		}
		seen[name] = true
	}
	return nil
}

func validateArraySchema(node map[string]any, types []string, limits Limits, depth int, path string) error {
	if !slices.Contains(types, "array") {
		return nil
	}
	child, ok := node["items"].(map[string]any)
	if !ok {
		return adapterError("MCP_SCHEMA_INVALID", errors.New("array items must be a schema object"))
	}
	return validateSchemaNode(child, limits, depth+1, path+"/items")
}

func validateNumericSchema(node map[string]any, types []string) error {
	if !slices.Contains(types, "integer") && !slices.Contains(types, "number") {
		return nil
	}
	minimum, minOK := jsonNumber(node["minimum"])
	maximum, maxOK := jsonNumber(node["maximum"])
	if !minOK || !maxOK || minimum.Cmp(maximum) > 0 {
		return adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("JSON numeric forms require finite explicit minimum and maximum bounds"))
	}
	return nil
}

func validateUnionSchema(node map[string]any, limits Limits, depth int, path string) error {
	alternatives, ok := node["oneOf"]
	if !ok {
		return nil
	}
	values, ok := alternatives.([]any)
	if !ok || len(values) < 2 || len(values) > 32 {
		return adapterError("MCP_SCHEMA_INVALID", errors.New("oneOf requires two to 32 alternatives"))
	}
	objectDiscriminators := make(map[string]bool)
	objectCount := 0
	for index, value := range values {
		child, ok := value.(map[string]any)
		if !ok {
			return adapterError("MCP_SCHEMA_INVALID", errors.New("oneOf alternatives must be schema objects"))
		}
		if err := validateSchemaNode(child, limits, depth+1, fmt.Sprintf("%s/oneOf/%d", path, index)); err != nil {
			return err
		}
		childTypes, _ := schemaTypes(child["type"])
		if !slices.Contains(childTypes, "object") {
			continue
		}
		objectCount++
		discriminator := objectDiscriminator(child)
		if discriminator == "" || objectDiscriminators[discriminator] {
			return adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("object unions require unique required kind constants"))
		}
		objectDiscriminators[discriminator] = true
	}
	if objectCount == 0 && len(values) > 2 {
		return adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("multi-branch scalar unions are outside the lossless subset"))
	}
	return nil
}

func jsonNumber(value any) (*big.Rat, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, false
	}
	result := new(big.Rat)
	if _, ok := result.SetString(number.String()); !ok {
		return nil, false
	}
	return result, true
}

func schemaTypes(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	var result []string
	switch current := value.(type) {
	case string:
		result = []string{current}
	case []any:
		for _, entry := range current {
			name, ok := entry.(string)
			if !ok {
				return nil, adapterError("MCP_SCHEMA_INVALID", errors.New("schema type array must contain strings"))
			}
			result = append(result, name)
		}
	default:
		return nil, adapterError("MCP_SCHEMA_INVALID", errors.New("schema type must be a string or array"))
	}
	if len(result) == 0 || len(result) > 2 {
		return nil, adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("schema type union is outside the lossless subset"))
	}
	seen := make(map[string]bool, len(result))
	for _, name := range result {
		if !slices.Contains([]string{"null", "boolean", "object", "array", "number", "integer", "string"}, name) || seen[name] {
			return nil, adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("schema type is unknown or duplicated"))
		}
		seen[name] = true
	}
	if len(result) == 2 && !seen["null"] {
		return nil, adapterError("MCP_SCHEMA_UNSUPPORTED", errors.New("only nullable two-type unions are lossless"))
	}
	return result, nil
}

func objectDiscriminator(node map[string]any) string {
	properties, _ := node["properties"].(map[string]any)
	kind, _ := properties["kind"].(map[string]any)
	constant, _ := kind["const"].(string)
	required, _ := node["required"].([]any)
	for _, name := range required {
		if name == "kind" {
			return constant
		}
	}
	return ""
}

func sortedAnyKeys(values map[string]any) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func EncodeStructuredResult(response []byte, maximum int) (ToolResult, error) {
	canonical, err := validateStructuredContent(response, maximum)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: []Content{}, StructuredContent: canonical, IsError: false}, nil
}

func DecodeStructuredResult(result ToolResult, maximum int) ([]byte, error) {
	if result.IsError {
		return nil, adapterError("MCP_TOOL_ERROR", errors.New("MCP tool reported an execution error"))
	}
	return validateStructuredContent(result.StructuredContent, maximum)
}

func validateStructuredContent(input []byte, maximum int) ([]byte, error) {
	if maximum < 1 || len(input) == 0 || len(input) > maximum {
		return nil, adapterError("MCP_RESPONSE_TOO_LARGE", errors.New("structured content is absent or exceeds the configured limit"))
	}
	if err := protocol.ValidateJSON(input, protocol.Limits{MaxBytes: maximum, MaxDepth: 64}); err != nil {
		return nil, adapterError("MCP_RESULT_INVALID", err)
	}
	value, err := protocol.DecodeJSONValue(input)
	if err != nil {
		return nil, adapterError("MCP_RESULT_INVALID", err)
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != 3 {
		return nil, adapterError("MCP_RESULT_INVALID", errors.New("structured result must be the closed Naatre result envelope"))
	}
	complete, completeOK := object["complete"].(bool)
	errorsValue, errorsOK := object["errors"].([]any)
	_, hasData := object["data"]
	if !completeOK || !errorsOK || !hasData || complete && len(errorsValue) != 0 || !complete && len(errorsValue) == 0 {
		return nil, adapterError("MCP_RESULT_INVALID", errors.New("structured result completion and errors are inconsistent"))
	}
	canonical, err := protocol.CanonicalizeJSON(input, protocol.Limits{MaxBytes: maximum, MaxDepth: 64})
	if err != nil {
		return nil, adapterError("MCP_RESULT_INVALID", err)
	}
	return canonical, nil
}

func ParseCatalog(input []byte, limits Limits) (Catalog, error) {
	limits = withDefaultLimits(limits)
	if len(input) == 0 || len(input) > limits.MaxCatalogBytes {
		return Catalog{}, adapterError("MCP_CATALOG_LIMIT", errors.New("catalog is empty or exceeds the configured limit"))
	}
	if err := protocol.ValidateJSON(input, protocol.Limits{MaxBytes: limits.MaxCatalogBytes, MaxDepth: limits.MaxSchemaDepth + 8}); err != nil {
		return Catalog{}, adapterError("MCP_CATALOG_INVALID", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var catalog Catalog
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, adapterError("MCP_CATALOG_INVALID", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Catalog{}, adapterError("MCP_CATALOG_INVALID", errors.New("catalog contains trailing data"))
	}
	return cloneCatalog(catalog), nil
}

func withDefaultLimits(limits Limits) Limits {
	defaults := DefaultLimits()
	if limits.MaxCatalogBytes == 0 {
		limits.MaxCatalogBytes = defaults.MaxCatalogBytes
	}
	if limits.MaxSchemaBytes == 0 {
		limits.MaxSchemaBytes = defaults.MaxSchemaBytes
	}
	if limits.MaxSchemaDepth == 0 {
		limits.MaxSchemaDepth = defaults.MaxSchemaDepth
	}
	if limits.MaxEntries == 0 {
		limits.MaxEntries = defaults.MaxEntries
	}
	if limits.MaxIdentifier == 0 {
		limits.MaxIdentifier = defaults.MaxIdentifier
	}
	if limits.MaxProgressEvents == 0 {
		limits.MaxProgressEvents = defaults.MaxProgressEvents
	}
	if limits.MaxContentBytes == 0 {
		limits.MaxContentBytes = defaults.MaxContentBytes
	}
	return limits
}

func safeText(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len(value) > maximum {
		return value[:maximum]
	}
	return value
}
