package openapiadapter

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

// Export emits canonical JSON for the exact OpenAPI subset accepted by
// Compile. It returns a rejected fidelity report alongside every failure.
func Export(config ExportConfig) ([]byte, interopadapter.FidelityReport, error) {
	if err := validateLimits(config.Limits); err != nil {
		return exportFailure(config, "OPENAPI_LIMIT_INVALID", "resource-limits", err)
	}
	if !operationIDPattern.MatchString(config.AdapterID) || !operationIDPattern.MatchString(config.WireVersion) {
		return exportFailure(config, "OPENAPI_IDENTITY_INVALID", "identity", errors.New("adapter and wire identities must be bounded identifiers"))
	}
	if config.Title == "" || config.Version == "" {
		return exportFailure(config, "OPENAPI_INFO_INVALID", "document", errors.New("title and version are required"))
	}
	if len(config.Bindings) == 0 || len(config.Bindings) > config.Limits.MaxOperations {
		return exportFailure(config, "OPENAPI_OPERATION_LIMIT", "operation", errors.New("explicit operation bindings are required within the configured limit"))
	}
	canonicalSchema, err := config.Document.CanonicalJSON()
	if err != nil || len(canonicalSchema) > config.Limits.MaxDocumentBytes {
		return exportFailure(config, "OPENAPI_SCHEMA_INVALID", "schema", errors.New("portable schema is unavailable or exceeds the configured limit"))
	}
	exporter := schemaExporter{
		types: make(map[schema.TypeID]schema.TypeDeclaration), components: make(map[string]any),
		active: make(map[schema.TypeID]bool), limits: config.Limits,
	}
	for _, declaration := range config.Document.Types() {
		exporter.types[declaration.ID] = declaration
	}
	if len(exporter.types) > config.Limits.MaxSchemas {
		return exportFailure(config, "OPENAPI_SCHEMA_LIMIT", "schema", errors.New("schema count exceeds configured limit"))
	}
	operations := make(map[string]schema.OperationDescriptor)
	for _, operation := range config.Document.Operations() {
		operations[operation.Name] = operation
	}
	paths := make(map[string]any)
	reportOperations := make([]interopadapter.OperationReport, 0, len(config.Bindings))
	for _, name := range sortedKeys(config.Bindings) {
		operation, ok := operations[name]
		if !ok {
			return exportFailure(config, "OPENAPI_OPERATION_UNKNOWN", "operation", errors.New("bound operation is absent from the portable schema"))
		}
		if operation.Kind == protocol.Subscription {
			return exportFailure(config, "OPENAPI_CAPABILITY_UNSUPPORTED", "streaming", errors.New("subscription export requires a streaming binding"))
		}
		binding := config.Bindings[name]
		if err := validateExportBinding(binding, config.SecuritySchemes); err != nil {
			return exportFailure(config, errorPublicCode(err, "OPENAPI_OPERATION_INVALID"), "operation", err)
		}
		operationObject, err := exporter.operation(operation, binding)
		if err != nil {
			return exportFailure(config, errorPublicCode(err, "OPENAPI_SCHEMA_UNSUPPORTED"), "schema", err)
		}
		pathObject, _ := paths[binding.Path].(map[string]any)
		if pathObject == nil {
			pathObject = make(map[string]any)
			paths[binding.Path] = pathObject
		}
		method := strings.ToLower(binding.Method)
		if _, duplicate := pathObject[method]; duplicate {
			return exportFailure(config, "OPENAPI_OPERATION_INVALID", "operation", errors.New("two operations use the same method and path"))
		}
		pathObject[method] = operationObject
		reportOperations = append(reportOperations, interopadapter.OperationReport{
			ExternalName: name, NaatreName: name, Approved: true,
			Policy: interopadapter.PolicyClaims{
				Effect: runtimeEffect(operation.Effect), Idempotency: runtimeIdempotency(operation.Idempotency), Cost: operation.Cost,
				RetrySafe: operation.RetrySafe, Cacheable: operation.Cacheable,
				Batching: runtimeBatching(operation.Batching), Transaction: runtimeTransaction(operation.Transaction),
			},
		})
	}
	securitySchemes, err := exportSecuritySchemes(config.SecuritySchemes)
	if err != nil {
		return exportFailure(config, errorPublicCode(err, "OPENAPI_SECURITY_UNSUPPORTED"), "security", err)
	}
	root := map[string]any{
		"openapi": Version, "jsonSchemaDialect": JSONDialect,
		"info":  map[string]any{"title": config.Title, "version": config.Version},
		"paths": paths,
	}
	components := map[string]any{"schemas": exporter.components}
	if len(securitySchemes) != 0 {
		components["securitySchemes"] = securitySchemes
	}
	root["components"] = components
	encoded, err := canonical(root)
	if err != nil || len(encoded) > config.Limits.MaxDocumentBytes {
		return exportFailure(config, "OPENAPI_DOCUMENT_LIMIT", "document", errors.New("exported document exceeds configured limit"))
	}
	report := interopadapter.FidelityReport{
		Profile: Profile, AdapterID: config.AdapterID, Protocol: interopadapter.OpenAPI,
		Specification: Specification, Direction: interopadapter.SchemaExport,
		SchemaIdentity: config.Document.Revision(), WireVersion: config.WireVersion, Status: "ready",
		Mappings: openAPIMappings(), Operations: reportOperations, Diagnostics: []interopadapter.Diagnostic{},
	}
	return encoded, report, nil
}

func exportFailure(config ExportConfig, code, feature string, cause error) ([]byte, interopadapter.FidelityReport, error) {
	report := interopadapter.FidelityReport{
		Profile: Profile, AdapterID: config.AdapterID, Protocol: interopadapter.OpenAPI,
		Specification: Specification, Direction: interopadapter.SchemaExport,
		SchemaIdentity: config.Document.Revision(), WireVersion: config.WireVersion, Status: "rejected",
		Mappings: openAPIMappings(), Operations: []interopadapter.OperationReport{},
		Diagnostics: []interopadapter.Diagnostic{{Code: code, Feature: feature, Message: "OpenAPI adapter preflight rejected the requested capability"}},
	}
	return nil, report, publicError(code, cause)
}

func validateExportBinding(binding ExportBinding, schemes map[string]SecurityScheme) error {
	method := strings.ToUpper(binding.Method)
	if !slices.Contains([]string{"GET", "PUT", "POST", "DELETE", "PATCH", "OPTIONS"}, method) {
		return publicError("OPENAPI_METHOD_UNSUPPORTED", errors.New("HTTP method is unsupported"))
	}
	if binding.Path == "" || binding.Path[0] != '/' || strings.ContainsAny(binding.Path, "{}?#") {
		return publicError("OPENAPI_PATH_UNSUPPORTED", errors.New("only static absolute paths are supported"))
	}
	if !statusPattern.MatchString(binding.SuccessStatus) {
		return publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("success status must be one exact 2xx code"))
	}
	if binding.FieldMaskQuery != "" && !operationIDPattern.MatchString(binding.FieldMaskQuery) {
		return publicError("OPENAPI_PARAMETER_UNSUPPORTED", errors.New("field-mask query name is invalid"))
	}
	for _, requirement := range binding.Security {
		if _, ok := schemes[requirement.Scheme]; !ok || requirement.Scheme == "" {
			return publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("security requirement is unresolved"))
		}
	}
	return nil
}

type schemaExporter struct {
	types      map[schema.TypeID]schema.TypeDeclaration
	components map[string]any
	active     map[schema.TypeID]bool
	limits     Limits
	properties int
}

func (e *schemaExporter) operation(operation schema.OperationDescriptor, binding ExportBinding) (map[string]any, error) {
	outputDeclaration, ok := e.types[operation.Output]
	if operation.OutputNullable || !ok || outputDeclaration.Kind != schema.ObjectType || !outputDeclaration.Output {
		return nil, publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("runtime-consume responses must be non-null objects"))
	}
	output, err := e.reference(operation.Output, operation.OutputNullable, false, 0)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"operationId": operation.Name,
		"responses": map[string]any{
			binding.SuccessStatus: map[string]any{"description": "success", "content": map[string]any{"application/json": map[string]any{"schema": output}}},
			"default":             map[string]any{"description": "safe problem", "content": map[string]any{"application/problem+json": map[string]any{"schema": problemSchema()}}},
		},
	}
	if operation.Description != "" {
		result["description"] = operation.Description
	}
	if operation.Input != "" {
		input, inputErr := e.reference(operation.Input, operation.InputNullable, true, 0)
		if inputErr != nil {
			return nil, inputErr
		}
		result["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": input}}}
	}
	if binding.FieldMaskQuery != "" {
		result["parameters"] = []any{map[string]any{
			"name": binding.FieldMaskQuery, "in": "query", "required": false,
			"style": "form", "explode": false, "schema": map[string]any{"type": "string"},
		}}
	}
	if len(binding.Security) != 0 {
		requirements := make([]any, 0, len(binding.Security))
		for _, requirement := range binding.Security {
			requirements = append(requirements, map[string]any{requirement.Scheme: []any{}})
		}
		result["security"] = requirements
	}
	return result, nil
}

func (e *schemaExporter) reference(id schema.TypeID, nullable, input bool, depth int) (map[string]any, error) {
	if depth > e.limits.MaxDepth {
		return nil, publicError("OPENAPI_SCHEMA_LIMIT", errors.New("schema depth exceeds configured limit"))
	}
	if scalar, ok := exportScalar(id); ok {
		if nullable {
			scalar["type"] = []any{scalar["type"], "null"}
		}
		return scalar, nil
	}
	if nullable {
		return nil, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("nullable composite references are outside the exact subset"))
	}
	declaration, ok := e.types[id]
	if !ok {
		return nil, publicError("OPENAPI_SCHEMA_INVALID", errors.New("operation references an unknown type"))
	}
	if input && !declaration.Input || !input && !declaration.Output {
		return nil, publicError("OPENAPI_SCHEMA_INVALID", errors.New("type is unavailable in the requested position"))
	}
	if _, ok := e.components[string(id)]; !ok {
		if e.active[id] {
			return nil, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("recursive schemas require a separately declared bound"))
		}
		e.active[id] = true
		component, err := e.declaration(declaration, input, depth+1)
		delete(e.active, id)
		if err != nil {
			return nil, err
		}
		e.components[string(id)] = component
	}
	return map[string]any{"$ref": "#/components/schemas/" + string(id)}, nil
}

func (e *schemaExporter) declaration(declaration schema.TypeDeclaration, input bool, depth int) (map[string]any, error) {
	switch declaration.Kind {
	case schema.InputObjectType, schema.ObjectType:
		properties := make(map[string]any, len(declaration.Fields))
		required := make([]string, 0, len(declaration.Fields))
		e.properties += len(declaration.Fields)
		if e.properties > e.limits.MaxProperties || len(declaration.Fields) == 0 {
			return nil, publicError("OPENAPI_PROPERTY_LIMIT", errors.New("object property count exceeds configured limit"))
		}
		for _, field := range declaration.Fields {
			mapped, err := e.reference(field.Type, field.Nullable, input, depth+1)
			if err != nil {
				return nil, err
			}
			if len(field.Default) != 0 || len(field.Traits) != 0 || field.Cost != 0 {
				return nil, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("field defaults, traits, and costs are outside the exact subset"))
			}
			properties[field.Name] = mapped
			if field.Required {
				required = append(required, field.Name)
			}
		}
		sort.Strings(required)
		result := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
		if len(required) != 0 {
			result["required"] = required
		}
		return result, nil
	case schema.ListType:
		item, err := e.reference(declaration.Element, declaration.ElementNullable, input, depth+1)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": item}, nil
	case schema.EnumType:
		if declaration.Open {
			return nil, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("open enums are outside the exact subset"))
		}
		values := make([]string, 0, len(declaration.EnumMembers))
		for _, member := range declaration.EnumMembers {
			values = append(values, member.Name)
		}
		return map[string]any{"type": "string", "enum": values}, nil
	case schema.ScalarType, schema.MapType, schema.UnionType, schema.InterfaceType, schema.OneOfType:
		return nil, publicError("OPENAPI_SCHEMA_UNSUPPORTED", fmt.Errorf("type kind %q is outside the exact subset", declaration.Kind))
	}
	return nil, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("type kind is unknown"))
}

func exportScalar(id schema.TypeID) (map[string]any, bool) {
	switch schema.ScalarKind(id) {
	case schema.Boolean:
		return map[string]any{"type": "boolean"}, true
	case schema.String:
		return map[string]any{"type": "string"}, true
	case schema.ID:
		return map[string]any{"type": "string", "format": "id"}, true
	case schema.Int32:
		return map[string]any{"type": "integer", "format": "int32"}, true
	case schema.Int64:
		return map[string]any{"type": "integer", "format": "int64"}, true
	case schema.UInt64:
		return map[string]any{"type": "integer", "format": "uint64"}, true
	case schema.Float64:
		return map[string]any{"type": "number", "format": "double"}, true
	case schema.Decimal:
		return map[string]any{"type": "string", "format": "decimal"}, true
	case schema.Timestamp:
		return map[string]any{"type": "string", "format": "date-time"}, true
	case schema.Duration:
		return map[string]any{"type": "string", "format": "duration"}, true
	case schema.UUID:
		return map[string]any{"type": "string", "format": "uuid"}, true
	case schema.BigInt, schema.Bytes:
		return nil, false
	}
	return nil, false
}

func problemSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}},
	}
}

func exportSecuritySchemes(input map[string]SecurityScheme) (map[string]any, error) {
	result := make(map[string]any, len(input))
	for _, name := range sortedKeys(input) {
		scheme := input[name]
		if !operationIDPattern.MatchString(name) {
			return nil, publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("security scheme name is invalid"))
		}
		switch {
		case scheme.Type == "http" && (scheme.Scheme == "bearer" || scheme.Scheme == "basic"):
			result[name] = map[string]any{"type": "http", "scheme": scheme.Scheme}
		case scheme.Type == "apiKey" && scheme.In == "header" && validCredentialHeaderName(scheme.Name):
			result[name] = map[string]any{"type": "apiKey", "in": "header", "name": scheme.Name}
		default:
			return nil, publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("security scheme is outside the supported subset"))
		}
	}
	return result, nil
}

func runtimeEffect(value string) runtime.Effect { return runtime.Effect(value) }
func runtimeIdempotency(value string) runtime.IdempotencyPolicy {
	return runtime.IdempotencyPolicy(value)
}
func runtimeBatching(value string) runtime.Batching { return runtime.Batching(value) }
func runtimeTransaction(value string) runtime.TransactionParticipation {
	return runtime.TransactionParticipation(value)
}
