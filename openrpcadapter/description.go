package openrpcadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

var (
	methodPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	publicPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// Limits bounds all description and runtime inputs. Zero fields receive the
// conservative defaults returned by DefaultLimits.
type Limits struct {
	MaxDescriptionBytes int
	MaxDepth            int
	MaxMethods          int
	MaxParameters       int
	MaxSchemas          int
	MaxBatch            int
	MaxRequestBytes     int64
	MaxResponseBytes    int64
}

func DefaultLimits() Limits {
	return Limits{
		MaxDescriptionBytes: 1 << 20,
		MaxDepth:            32,
		MaxMethods:          256,
		MaxParameters:       128,
		MaxSchemas:          512,
		MaxBatch:            64,
		MaxRequestBytes:     1 << 20,
		MaxResponseBytes:    1 << 20,
	}
}

func withDefaults(limits Limits) Limits {
	defaults := DefaultLimits()
	if limits.MaxDescriptionBytes == 0 {
		limits.MaxDescriptionBytes = defaults.MaxDescriptionBytes
	}
	if limits.MaxDepth == 0 {
		limits.MaxDepth = defaults.MaxDepth
	}
	if limits.MaxMethods == 0 {
		limits.MaxMethods = defaults.MaxMethods
	}
	if limits.MaxParameters == 0 {
		limits.MaxParameters = defaults.MaxParameters
	}
	if limits.MaxSchemas == 0 {
		limits.MaxSchemas = defaults.MaxSchemas
	}
	if limits.MaxBatch == 0 {
		limits.MaxBatch = defaults.MaxBatch
	}
	if limits.MaxRequestBytes == 0 {
		limits.MaxRequestBytes = defaults.MaxRequestBytes
	}
	if limits.MaxResponseBytes == 0 {
		limits.MaxResponseBytes = defaults.MaxResponseBytes
	}
	return limits
}

func validLimits(limits Limits) bool {
	return limits.MaxDescriptionBytes > 0 && limits.MaxDepth > 0 && limits.MaxMethods > 0 && limits.MaxParameters > 0 && limits.MaxSchemas > 0 && limits.MaxBatch > 0 && limits.MaxRequestBytes > 0 && limits.MaxResponseBytes > 0
}

func validRuntimeLimits(limits Limits) bool {
	return validLimits(limits) && limits.MaxRequestBytes >= 64 && limits.MaxResponseBytes >= 128
}

type Info struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version"`
}

type Description struct {
	OpenRPC    string     `json:"openrpc"`
	Info       Info       `json:"info"`
	Methods    []Method   `json:"methods"`
	Components Components `json:"components,omitempty"`
}

type Components struct {
	Schemas map[string]json.RawMessage `json:"schemas,omitempty"`
}

type Method struct {
	Name           string              `json:"name"`
	Summary        string              `json:"summary,omitempty"`
	Description    string              `json:"description,omitempty"`
	Params         []ContentDescriptor `json:"params"`
	Result         ContentDescriptor   `json:"result"`
	Errors         []ErrorDefinition   `json:"errors,omitempty"`
	Deprecated     bool                `json:"deprecated,omitempty"`
	ParamStructure string              `json:"paramStructure"`
}

type ContentDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Deprecated  bool            `json:"deprecated,omitempty"`
	Schema      json.RawMessage `json:"schema"`
}

type ErrorDefinition struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// ParseDescription decodes the supported OpenRPC 1.4.1 description subset.
// Unknown description fields and schema keywords are rejected rather than
// silently treated as annotations.
func ParseDescription(input []byte, limits Limits) (Description, error) {
	limits = withDefaults(limits)
	if !validLimits(limits) {
		return Description{}, adapterError("OPENRPC_LIMIT_INVALID")
	}
	if len(input) > limits.MaxDescriptionBytes {
		return Description{}, adapterError("OPENRPC_DESCRIPTION_LIMIT")
	}
	jsonLimits := protocol.Limits{MaxBytes: limits.MaxDescriptionBytes, MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxSchemas + limits.MaxMethods*limits.MaxParameters, MaxArrayItems: limits.MaxSchemas + limits.MaxMethods*(limits.MaxParameters+1)}
	if err := protocol.ValidateJSON(input, jsonLimits); err != nil {
		return Description{}, adapterError("OPENRPC_DESCRIPTION_INVALID")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var description Description
	if err := decoder.Decode(&description); err != nil || decoder.Decode(&struct{}{}) == nil {
		return Description{}, adapterError("OPENRPC_DESCRIPTION_INVALID")
	}
	if diagnostics := validateDescription(description, limits); len(diagnostics) != 0 {
		return Description{}, adapterError(diagnostics[0].Code, diagnostics...)
	}
	return cloneDescription(description), nil
}

func validateDescription(description Description, limits Limits) []Diagnostic {
	var diagnostics []Diagnostic
	if description.OpenRPC != OpenRPCVersion {
		diagnostics = append(diagnostics, diagnostic("OPENRPC_SPECIFICATION_UNSUPPORTED", "/openrpc", "only the pinned OpenRPC 1.4.1 revision is supported"))
	}
	if description.Info.Title == "" || description.Info.Version == "" || len(description.Info.Title) > 256 || len(description.Info.Version) > 128 {
		diagnostics = append(diagnostics, diagnostic("OPENRPC_INFO_INVALID", "/info", "title and version are required and bounded"))
	}
	if len(description.Methods) == 0 || len(description.Methods) > limits.MaxMethods {
		diagnostics = append(diagnostics, diagnostic("OPENRPC_METHOD_LIMIT", "/methods", "method count is outside the configured bound"))
	}
	if len(description.Components.Schemas) > limits.MaxSchemas {
		diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_LIMIT", "/components/schemas", "schema count exceeds the configured bound"))
	}
	schemas := description.Components.Schemas
	for _, name := range sortedRawKeys(schemas) {
		if !methodPattern.MatchString(name) {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", "/components/schemas/"+escapePointer(name), "schema identity is invalid"))
			continue
		}
		diagnostics = append(diagnostics, validateSchema(schemas[name], "/components/schemas/"+escapePointer(name), schemas, limits, 0, map[string]bool{})...)
	}
	seen := make(map[string]bool, len(description.Methods))
	for index, method := range description.Methods {
		pointer := fmt.Sprintf("/methods/%d", index)
		if !methodPattern.MatchString(method.Name) || strings.HasPrefix(method.Name, "rpc.") || seen[method.Name] {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_METHOD_INVALID", pointer+"/name", "method identity is invalid, reserved, or duplicated"))
		}
		seen[method.Name] = true
		if method.ParamStructure != "by-name" {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_PARAM_STRUCTURE_UNSUPPORTED", pointer+"/paramStructure", "the profile supports explicit by-name parameters only"))
		}
		if len(method.Params) > limits.MaxParameters {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_PARAMETER_LIMIT", pointer+"/params", "parameter count exceeds the configured bound"))
		}
		seenParams := make(map[string]bool, len(method.Params))
		for paramIndex, param := range method.Params {
			paramPointer := fmt.Sprintf("%s/params/%d", pointer, paramIndex)
			if !methodPattern.MatchString(param.Name) || seenParams[param.Name] {
				diagnostics = append(diagnostics, diagnostic("OPENRPC_PARAMETER_INVALID", paramPointer+"/name", "parameter identity is invalid or duplicated"))
			}
			seenParams[param.Name] = true
			diagnostics = append(diagnostics, validateSchema(param.Schema, paramPointer+"/schema", schemas, limits, 0, map[string]bool{})...)
		}
		if method.Result.Name == "" {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_RESULT_INVALID", pointer+"/result/name", "result name is required"))
		}
		diagnostics = append(diagnostics, validateSchema(method.Result.Schema, pointer+"/result/schema", schemas, limits, 0, map[string]bool{})...)
		seenErrors := make(map[int64]bool, len(method.Errors))
		for errorIndex, definition := range method.Errors {
			errorPointer := fmt.Sprintf("%s/errors/%d", pointer, errorIndex)
			if definition.Code >= -32768 && definition.Code <= -32000 || definition.Code >= 0 || seenErrors[definition.Code] || definition.Message == "" || len(definition.Message) > 256 {
				diagnostics = append(diagnostics, diagnostic("OPENRPC_ERROR_DEFINITION_INVALID", errorPointer, "application error codes must be unique negative values outside the reserved range"))
			}
			seenErrors[definition.Code] = true
			if len(definition.Data) != 0 {
				diagnostics = append(diagnostics, validateSchema(definition.Data, errorPointer+"/data", schemas, limits, 0, map[string]bool{})...)
			}
		}
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		return diagnostics[i].Pointer+"\x00"+diagnostics[i].Code < diagnostics[j].Pointer+"\x00"+diagnostics[j].Code
	})
	return diagnostics
}

var supportedSchemaKeywords = map[string]bool{
	"$ref": true, "type": true, "title": true, "description": true,
	"properties": true, "required": true, "items": true, "additionalProperties": true,
	"oneOf": true, "enum": true, "const": true, "format": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
	"multipleOf": true, "minLength": true, "maxLength": true, "pattern": true,
	"minItems": true, "maxItems": true, "uniqueItems": true,
}

func validateSchema(raw json.RawMessage, pointer string, schemas map[string]json.RawMessage, limits Limits, depth int, active map[string]bool) []Diagnostic {
	if len(raw) == 0 {
		return []Diagnostic{diagnostic("OPENRPC_SCHEMA_INVALID", pointer, "schema is required")}
	}
	if depth > limits.MaxDepth {
		return []Diagnostic{diagnostic("OPENRPC_SCHEMA_LIMIT", pointer, "schema depth exceeds the configured bound")}
	}
	if err := protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: limits.MaxDescriptionBytes, MaxDepth: limits.MaxDepth}); err != nil {
		return []Diagnostic{diagnostic("OPENRPC_SCHEMA_INVALID", pointer, "schema is not strict bounded JSON")}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return []Diagnostic{diagnostic("OPENRPC_SCHEMA_UNSUPPORTED", pointer, "boolean and non-object schemas are unsupported")}
	}
	var diagnostics []Diagnostic
	for _, keyword := range sortedRawKeys(object) {
		if !supportedSchemaKeywords[keyword] {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_UNSUPPORTED", pointer+"/"+escapePointer(keyword), "schema keyword is outside the supported profile"))
		}
	}
	diagnostics = append(diagnostics, validateSchemaKeywordValues(object, pointer, limits)...)
	if reference, ok := object["$ref"]; ok {
		if len(object) != 1 {
			return append(diagnostics, diagnostic("OPENRPC_SCHEMA_UNSUPPORTED", pointer, "$ref siblings are unsupported"))
		}
		var value string
		if json.Unmarshal(reference, &value) != nil || !strings.HasPrefix(value, "#/components/schemas/") {
			return append(diagnostics, diagnostic("OPENRPC_REFERENCE_BLOCKED", pointer+"/$ref", "only local component schema references are supported"))
		}
		name := strings.TrimPrefix(value, "#/components/schemas/")
		target, exists := schemas[name]
		if !exists {
			return append(diagnostics, diagnostic("OPENRPC_REFERENCE_INVALID", pointer+"/$ref", "component schema reference is unresolved"))
		}
		if active[name] {
			return append(diagnostics, diagnostic("OPENRPC_REFERENCE_CYCLE", pointer+"/$ref", "component schema reference cycle is unsupported"))
		}
		active[name] = true
		diagnostics = append(diagnostics, validateSchema(target, pointer+"/$ref", schemas, limits, depth+1, active)...)
		delete(active, name)
		return diagnostics
	}
	if rawType, exists := object["type"]; exists {
		var scalar string
		if json.Unmarshal(rawType, &scalar) != nil || !slices.Contains([]string{"null", "boolean", "string", "integer", "number", "object", "array"}, scalar) {
			var variants []string
			validNullable := json.Unmarshal(rawType, &variants) == nil && len(variants) == 2 && variants[0] != variants[1] && slices.Contains(variants, "null")
			for _, variant := range variants {
				validNullable = validNullable && slices.Contains([]string{"null", "boolean", "string", "integer", "number", "object", "array"}, variant)
			}
			if !validNullable {
				diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_UNSUPPORTED", pointer+"/type", "type must be one supported scalar or one scalar plus null"))
			}
		}
	}
	if properties, exists := object["properties"]; exists {
		var values map[string]json.RawMessage
		if json.Unmarshal(properties, &values) != nil || values == nil {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/properties", "properties must be an object"))
		} else {
			for _, name := range sortedRawKeys(values) {
				diagnostics = append(diagnostics, validateSchema(values[name], pointer+"/properties/"+escapePointer(name), schemas, limits, depth+1, active)...)
			}
		}
	}
	if items, exists := object["items"]; exists {
		diagnostics = append(diagnostics, validateSchema(items, pointer+"/items", schemas, limits, depth+1, active)...)
	}
	if additional, exists := object["additionalProperties"]; exists && string(additional) != "true" && string(additional) != "false" {
		diagnostics = append(diagnostics, validateSchema(additional, pointer+"/additionalProperties", schemas, limits, depth+1, active)...)
	}
	if alternatives, exists := object["oneOf"]; exists {
		var values []json.RawMessage
		if json.Unmarshal(alternatives, &values) != nil || len(values) < 2 {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/oneOf", "oneOf requires at least two alternatives"))
		} else {
			tags := make(map[string]bool, len(values))
			for index, value := range values {
				branchPointer := fmt.Sprintf("%s/oneOf/%d", pointer, index)
				diagnostics = append(diagnostics, validateSchema(value, branchPointer, schemas, limits, depth+1, active)...)
				tag, ok := taggedAlternative(value)
				if !ok || tags[tag] {
					diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_UNSUPPORTED", branchPointer, "oneOf alternatives require unique required kind const tags"))
				}
				tags[tag] = true
			}
		}
	}
	return diagnostics
}

func validateSchemaKeywordValues(object map[string]json.RawMessage, pointer string, limits Limits) []Diagnostic {
	var diagnostics []Diagnostic
	for _, keyword := range []string{"title", "description", "format", "pattern"} {
		if raw, exists := object[keyword]; exists {
			var value string
			if json.Unmarshal(raw, &value) != nil || len(value) > limits.MaxDescriptionBytes || (keyword == "format" && value == "") {
				diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/"+keyword, keyword+" must be a bounded string"))
			} else if keyword == "pattern" {
				if _, err := regexp.Compile(value); err != nil {
					diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_UNSUPPORTED", pointer+"/pattern", "pattern must use the RE2-compatible subset"))
				}
			}
		}
	}
	for _, keyword := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
		if raw, exists := object[keyword]; exists {
			value, ok := exactJSONNumber(raw)
			if !ok || keyword == "multipleOf" && value.Sign() <= 0 {
				diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/"+keyword, keyword+" must be a valid finite number with a positive multipleOf"))
			}
		}
	}
	for _, pair := range [][2]string{{"minimum", "maximum"}, {"exclusiveMinimum", "exclusiveMaximum"}} {
		minimum, minimumOK := exactJSONNumber(object[pair[0]])
		maximum, maximumOK := exactJSONNumber(object[pair[1]])
		if minimumOK && maximumOK && minimum.Cmp(maximum) > 0 {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer, pair[0]+" cannot exceed "+pair[1]))
		}
	}
	for _, pair := range [][2]string{{"minLength", "maxLength"}, {"minItems", "maxItems"}} {
		minimum, minimumOK := nonNegativeInteger(object[pair[0]])
		maximum, maximumOK := nonNegativeInteger(object[pair[1]])
		if object[pair[0]] != nil && !minimumOK {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/"+pair[0], pair[0]+" must be a bounded non-negative integer"))
		}
		if object[pair[1]] != nil && !maximumOK {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/"+pair[1], pair[1]+" must be a bounded non-negative integer"))
		}
		if minimumOK && maximumOK && minimum > maximum {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer, pair[0]+" cannot exceed "+pair[1]))
		}
	}
	if raw, exists := object["uniqueItems"]; exists {
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			diagnostics = append(diagnostics, diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/uniqueItems", "uniqueItems must be a boolean"))
		}
	}
	diagnostics = append(diagnostics, validateRequiredKeyword(object["required"], pointer, limits)...)
	diagnostics = append(diagnostics, validateEnumKeyword(object["enum"], pointer, limits)...)
	return diagnostics
}

func validateRequiredKeyword(raw json.RawMessage, pointer string, limits Limits) []Diagnostic {
	if raw == nil {
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil || values == nil || len(values) > limits.MaxParameters {
		return []Diagnostic{diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/required", "required must be a bounded array of unique strings")}
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] || len(value) > 128 {
			return []Diagnostic{diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/required", "required must be a bounded array of unique strings")}
		}
		seen[value] = true
	}
	return nil
}

func validateEnumKeyword(raw json.RawMessage, pointer string, limits Limits) []Diagnostic {
	if raw == nil {
		return nil
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 || len(values) > limits.MaxParameters {
		return []Diagnostic{diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/enum", "enum must be a non-empty bounded array of unique JSON values")}
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		canonical, err := protocol.CanonicalizeJSON(value, protocol.Limits{MaxBytes: limits.MaxDescriptionBytes, MaxDepth: limits.MaxDepth})
		if err != nil || seen[string(canonical)] {
			return []Diagnostic{diagnostic("OPENRPC_SCHEMA_INVALID", pointer+"/enum", "enum must be a non-empty bounded array of unique JSON values")}
		}
		seen[string(canonical)] = true
	}
	return nil
}

func exactJSONNumber(raw json.RawMessage) (*big.Rat, bool) {
	if raw == nil {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	number, ok := value.(json.Number)
	if !ok {
		return nil, false
	}
	rational, ok := new(big.Rat).SetString(number.String())
	return rational, ok
}

func nonNegativeInteger(raw json.RawMessage) (uint64, bool) {
	if raw == nil {
		return 0, false
	}
	var value uint64
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, value <= 1<<31
}

func taggedAlternative(raw json.RawMessage) (string, bool) {
	var value struct {
		Properties map[string]struct {
			Const string `json:"const"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(raw, &value) != nil || !slices.Contains(value.Required, "kind") {
		return "", false
	}
	tag := value.Properties["kind"].Const
	return tag, tag != ""
}

// MethodBinding is the explicit application mapping between one OpenRPC
// method and one Naatre runtime descriptor. No policy field is read from the
// OpenRPC document.
type MethodBinding struct {
	ExternalName      string
	Descriptor        runtime.Descriptor
	AllowNotification bool
}

// MappingConfig identifies one independently evidenced adapter direction.
type MappingConfig struct {
	AdapterID      string
	SchemaIdentity string
	WireVersion    string
	Direction      interopadapter.Direction
	Bindings       []MethodBinding
}

// MapDescription validates an OpenRPC description and publishes a #56
// compatible fidelity report for one direction.
func MapDescription(input []byte, config MappingConfig, limits Limits) (Description, interopadapter.FidelityReport, error) {
	description, err := ParseDescription(input, limits)
	report := newReport(config)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: errorCode(err), Message: "OpenRPC description mapping failed"})
		return Description{}, report, err
	}
	methods := make(map[string]bool, len(description.Methods))
	for _, method := range description.Methods {
		methods[method.Name] = true
	}
	seen := make(map[string]bool, len(config.Bindings))
	for _, binding := range config.Bindings {
		operation := interopadapter.OperationReport{ExternalName: binding.ExternalName, NaatreName: binding.Descriptor.Name, Approved: true, Policy: policyClaims(binding.Descriptor.Metadata)}
		report.Operations = append(report.Operations, operation)
		if !methods[binding.ExternalName] || seen[binding.ExternalName] || !methodPattern.MatchString(binding.Descriptor.Name) {
			report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "OPENRPC_BINDING_INVALID", Operation: binding.ExternalName, Message: "binding must select one declared method and one explicit Naatre operation"})
		}
		seen[binding.ExternalName] = true
		if !validBindingPolicy(binding.Descriptor) {
			report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "OPENRPC_POLICY_REQUIRED", Operation: binding.ExternalName, Message: "application effect, idempotency, cost, and authorization policy are required"})
		}
	}
	if len(config.Bindings) == 0 {
		report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "OPENRPC_BINDING_REQUIRED", Message: "at least one explicit method binding is required"})
	}
	if config.Direction != interopadapter.SchemaImport && config.Direction != interopadapter.SchemaExport && config.Direction != interopadapter.RuntimeConsume && config.Direction != interopadapter.RuntimeExpose {
		report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "OPENRPC_DIRECTION_UNSUPPORTED", Message: "adapter direction is unsupported"})
	}
	if config.AdapterID == "" || config.SchemaIdentity == "" || config.WireVersion == "" || config.SchemaIdentity == config.WireVersion {
		report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "OPENRPC_IDENTITY_INVALID", Message: "adapter, independent schema, and wire identities are required"})
	}
	sort.Slice(report.Operations, func(i, j int) bool { return report.Operations[i].ExternalName < report.Operations[j].ExternalName })
	sort.Slice(report.Diagnostics, func(i, j int) bool {
		return report.Diagnostics[i].Code+"\x00"+report.Diagnostics[i].Operation < report.Diagnostics[j].Code+"\x00"+report.Diagnostics[j].Operation
	})
	if len(report.Diagnostics) != 0 {
		return Description{}, report, adapterError(report.Diagnostics[0].Code)
	}
	report.Status = "ready"
	return description, report, nil
}

func validBindingPolicy(descriptor runtime.Descriptor) bool {
	metadata := descriptor.Metadata
	if descriptor.Scope != runtime.RootScope || descriptor.Member != runtime.CallMember || !slices.Contains([]protocol.OperationKind{protocol.Query, protocol.Mutation, protocol.Subscription}, descriptor.Kind) {
		return false
	}
	if !slices.Contains([]runtime.Effect{runtime.ReadEffect, runtime.WriteEffect}, metadata.Effect) || (descriptor.Kind == protocol.Query || descriptor.Kind == protocol.Subscription) && metadata.Effect != runtime.ReadEffect {
		return false
	}
	if metadata.Cacheable && (metadata.Effect != runtime.ReadEffect || !metadata.Deterministic) {
		return false
	}
	return methodPattern.MatchString(descriptor.Name) && metadata.AuthorizationPolicy != "" && metadata.Cost > 0 &&
		slices.Contains([]runtime.IdempotencyPolicy{runtime.IdempotencyNonIdempotent, runtime.IdempotencyIdempotent, runtime.IdempotencyConditional}, metadata.Idempotency) &&
		slices.Contains([]runtime.ThreadSafety{runtime.ThreadSafe, runtime.SerialOnly}, metadata.ThreadSafety) &&
		slices.Contains([]runtime.Batching{runtime.BatchEligible, runtime.BatchIneligible}, metadata.Batching) &&
		slices.Contains([]runtime.TransactionParticipation{runtime.TransactionNone, runtime.TransactionOptional, runtime.TransactionRequired}, metadata.Transaction)
}

// ExportDescription emits canonical OpenRPC bytes after applying the same
// explicit binding and schema checks used by import.
func ExportDescription(description Description, config MappingConfig, limits Limits) ([]byte, interopadapter.FidelityReport, error) {
	description.OpenRPC = OpenRPCVersion
	encoded, err := json.Marshal(description)
	if err != nil {
		return nil, newReport(config), adapterError("OPENRPC_DESCRIPTION_INVALID")
	}
	_, report, err := MapDescription(encoded, config, limits)
	if err != nil {
		return nil, report, err
	}
	canonical, canonicalErr := protocol.CanonicalizeJSON(encoded, protocol.Limits{MaxBytes: withDefaults(limits).MaxDescriptionBytes, MaxDepth: withDefaults(limits).MaxDepth})
	if canonicalErr != nil {
		return nil, report, adapterError("OPENRPC_DESCRIPTION_INVALID")
	}
	return canonical, report, nil
}

func newReport(config MappingConfig) interopadapter.FidelityReport {
	return interopadapter.FidelityReport{
		Profile: Profile, AdapterID: config.AdapterID, Protocol: interopadapter.OpenRPC,
		Specification: Specification, Direction: config.Direction, SchemaIdentity: config.SchemaIdentity,
		WireVersion: config.WireVersion, Status: "rejected", Mappings: profileMappings(config.Direction),
		Operations: []interopadapter.OperationReport{}, Diagnostics: []interopadapter.Diagnostic{},
	}
}

func profileMappings(direction interopadapter.Direction) []interopadapter.Mapping {
	values := []interopadapter.Mapping{
		{Feature: "authentication", Classification: interopadapter.ApplicationSupplied, Resolution: "exact application authentication and credential policy"},
		{Feature: "batching", Classification: interopadapter.ExplicitlyAdapted, Resolution: "bounded JSON-RPC batches preserve independent member outcomes"},
		{Feature: "cancellation", Classification: interopadapter.ExplicitlyAdapted, Resolution: "caller context cancellation aborts active transport and handler work"},
		{Feature: "cost", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre operation cost"},
		{Feature: "deadlines", Classification: interopadapter.ExplicitlyAdapted, Resolution: "caller deadlines propagate without extension"},
		{Feature: "error-paths", Classification: interopadapter.ExplicitlyAdapted, Resolution: "JSON-RPC errors map to stable bounded public codes without error data"},
		{Feature: "field-masks-projections", Classification: interopadapter.ApplicationSupplied, Resolution: "declared method parameter mapping"},
		{Feature: "idempotency", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre idempotency and retry policy"},
		{Feature: "metadata", Classification: interopadapter.ApplicationSupplied, Resolution: "transport metadata is explicit and never description-authoritative"},
		{Feature: "nullable-optional", Classification: interopadapter.Lossless},
		{Feature: "oneof-unions", Classification: interopadapter.ExplicitlyAdapted, Resolution: "only uniquely kind-tagged oneOf alternatives are accepted"},
		{Feature: "pagination", Classification: interopadapter.ApplicationSupplied, Resolution: "declared cursor parameter mapping"},
		{Feature: "partial-failure", Classification: interopadapter.ExplicitlyAdapted, Resolution: "batch members remain independent"},
		{Feature: "redirects-egress", Classification: interopadapter.ExplicitlyAdapted, Resolution: "client endpoint is exact and redirects are denied"},
		{Feature: "scalar-ranges", Classification: interopadapter.Lossless},
		{Feature: "streaming", Classification: interopadapter.ExplicitlyAdapted, Resolution: "profile admits unary methods only and rejects streaming descriptions"},
		{Feature: "transactions", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre transaction participation"},
		{Feature: "result-error-envelope", Classification: interopadapter.ExplicitlyAdapted, Resolution: "exactly one result or error member is accepted"},
		{Feature: "notifications", Classification: interopadapter.ExplicitlyAdapted, Resolution: "notification eligibility is explicitly declared per method"},
	}
	if direction == interopadapter.SchemaImport || direction == interopadapter.SchemaExport {
		for index := range values {
			if values[index].Feature == "authentication" || values[index].Feature == "metadata" || values[index].Feature == "deadlines" || values[index].Feature == "cancellation" {
				values[index] = interopadapter.Mapping{Feature: values[index].Feature, Classification: interopadapter.ExplicitlyAdapted, Resolution: "runtime-only feature is recorded as outside this direction"}
			}
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Feature < values[j].Feature })
	return values
}

func policyClaims(metadata runtime.Metadata) interopadapter.PolicyClaims {
	return interopadapter.PolicyClaims{Effect: metadata.Effect, Idempotency: metadata.Idempotency, Cost: metadata.Cost, RetrySafe: metadata.RetrySafe, Cacheable: metadata.Cacheable, Batching: metadata.Batching, Transaction: metadata.Transaction}
}

func errorCode(err error) string {
	var value *Error
	if errors.As(err, &value) {
		return value.Code
	}
	return "OPENRPC_INTERNAL"
}

func cloneDescription(input Description) Description {
	result := input
	result.Methods = slices.Clone(input.Methods)
	for index := range result.Methods {
		result.Methods[index].Params = slices.Clone(input.Methods[index].Params)
		result.Methods[index].Errors = slices.Clone(input.Methods[index].Errors)
	}
	result.Components.Schemas = make(map[string]json.RawMessage, len(input.Components.Schemas))
	for key, value := range input.Components.Schemas {
		result.Components.Schemas[key] = slices.Clone(value)
	}
	return result
}

func sortedRawKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func escapePointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func validPublicCode(value string) bool { return publicPattern.MatchString(value) }
