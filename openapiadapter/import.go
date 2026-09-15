package openapiadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type jsonRaw = json.RawMessage

var (
	operationIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	statusPattern      = regexp.MustCompile(`^2[0-9][0-9]$`)
)

type parsedDocument struct {
	components      map[string]jsonRaw
	securitySchemes map[string]SecurityScheme
	operations      map[string]httpOperation
}

type httpOperation struct {
	id               string
	method           string
	path             string
	description      string
	fieldMaskQuery   string
	requestSchema    jsonRaw
	responseSchema   jsonRaw
	successStatus    int
	components       map[string]jsonRaw
	securityRequired bool
	securityHeaders  [][]string
}

type operationTypes struct {
	input         schema.TypeID
	inputNullable bool
	output        schema.TypeID
}

type compiledSchema struct {
	identifier schema.TypeID
	nullable   bool
}

// Compile validates the complete document, imports types for explicitly bound
// operations, and delegates registration authority to interopadapter. No
// invoker is reachable when any fidelity or validation diagnostic exists.
func Compile(config ImportConfig) (*Compiled, interopadapter.FidelityReport, error) {
	if err := validateLimits(config.Limits); err != nil {
		return importFailure(config, "OPENAPI_LIMIT_INVALID", "", "resource-limits", err)
	}
	parsed, _, err := parseDocument(config.Document, config.Limits)
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) {
			return importFailure(config, failure.code, "", "document", err)
		}
		return importFailure(config, "OPENAPI_DOCUMENT_INVALID", "", "document", err)
	}
	if len(config.Bindings) == 0 {
		return importFailure(config, "OPENAPI_OPERATION_REQUIRED", "", "operation", errors.New("at least one operation binding is required"))
	}
	for _, operationID := range sortedKeys(config.Bindings) {
		if _, ok := parsed.operations[operationID]; !ok {
			return importFailure(config, "OPENAPI_OPERATION_UNKNOWN", operationID, "operation", errors.New("approved operation is absent from the document"))
		}
	}
	types, operationSchemas, err := importTypes(parsed, config.Bindings, config.Limits)
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) {
			return importFailure(config, failure.code, "", "schema", err)
		}
		return importFailure(config, "OPENAPI_SCHEMA_INVALID", "", "schema", err)
	}
	operations := make([]interopadapter.Operation, 0, len(config.Bindings))
	for _, operationID := range sortedKeys(config.Bindings) {
		binding := config.Bindings[operationID]
		if binding.Kind != protocol.Query && binding.Kind != protocol.Mutation {
			return importFailure(config, "OPENAPI_CAPABILITY_UNSUPPORTED", operationID, "streaming", errors.New("unary OpenAPI operations support query and mutation bindings only"))
		}
		operation := parsed.operations[operationID]
		invoker := binding.Invoker
		if invoker == nil && config.HTTP != nil {
			httpConfig := *config.HTTP
			if err := applyRuntimeLimitDefaults(&httpConfig, config.Limits); err != nil {
				return importFailure(config, "OPENAPI_LIMIT_INVALID", operationID, "resource-limits", err)
			}
			invoker, err = newHTTPInvoker(httpConfig, operation)
			if err != nil {
				return importFailure(config, errorPublicCode(err, "OPENAPI_TRANSPORT_INVALID"), operationID, "transport", err)
			}
		}
		operations = append(operations, interopadapter.Operation{
			ExternalName: operationID, Approved: true, Invoker: invoker,
			Descriptor: runtime.Descriptor{
				ID: "openapi." + operationID, Name: binding.Name, Scope: runtime.RootScope,
				Kind: binding.Kind, Member: runtime.CallMember,
				Input: operationSchemas[operationID].input, InputNullable: operationSchemas[operationID].inputNullable,
				Output:      operationSchemas[operationID].output,
				Description: operation.description, Metadata: binding.Metadata,
			},
		})
	}
	core, report, err := interopadapter.Compile(interopadapter.Config{
		AdapterID: config.AdapterID, Protocol: interopadapter.OpenAPI, Specification: Specification,
		Direction: interopadapter.RuntimeConsume, SchemaIdentity: config.SchemaIdentity, WireVersion: config.WireVersion,
		Types: types, Mappings: openAPIMappings(), Operations: operations,
		MaxOperations: config.Limits.MaxOperations, MaxFanOut: config.Limits.MaxFanOut,
	})
	if err != nil {
		return nil, report, err
	}
	report.Profile = Profile
	compiled := &Compiled{core: core, types: types, report: report}
	return compiled, compiled.FidelityReport(), nil
}

func importFailure(config ImportConfig, code, operation, feature string, cause error) (*Compiled, interopadapter.FidelityReport, error) {
	report := rejectedReport(config, interopadapter.Diagnostic{
		Code: code, Operation: operation, Feature: feature,
		Message: "OpenAPI adapter preflight rejected the requested capability",
	})
	return nil, report, publicError(code, cause)
}

func validateLimits(limits Limits) error {
	if limits.MaxDocumentBytes < 1 || limits.MaxOperations < 1 || limits.MaxOperations > 1024 ||
		limits.MaxSchemas < 1 || limits.MaxSchemas > 4096 || limits.MaxProperties < 1 || limits.MaxProperties > 100_000 ||
		limits.MaxDepth < 1 || limits.MaxDepth > 64 || limits.MaxFanOut < 1 || limits.MaxFanOut > 4096 ||
		limits.MaxRequestBytes < 1 || limits.MaxRequestBytes > maxHTTPBodyBytes || limits.MaxResponseBytes < 1 || limits.MaxResponseBytes > maxHTTPBodyBytes ||
		limits.MaxProjectionFields < 0 || limits.MaxProjectionFields > 4096 {
		return errors.New("all resource limits must be positive and within portable ceilings")
	}
	return nil
}

func parseDocument(input []byte, limits Limits) (parsedDocument, []interopadapter.Diagnostic, error) {
	if len(input) > limits.MaxDocumentBytes {
		return parsedDocument{}, nil, publicError("OPENAPI_DOCUMENT_LIMIT", errors.New("document exceeds configured limit"))
	}
	canonical, err := protocol.CanonicalizeJSON(input, protocol.Limits{MaxBytes: limits.MaxDocumentBytes, MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxProperties + limits.MaxOperations*32 + limits.MaxSchemas*8})
	if err != nil {
		return parsedDocument{}, nil, publicError("OPENAPI_DOCUMENT_INVALID", err)
	}
	root, err := decodeObject(canonical)
	if err != nil {
		return parsedDocument{}, nil, publicError("OPENAPI_DOCUMENT_INVALID", err)
	}
	if key := unsupportedKey(root, "openapi", "jsonSchemaDialect", "info", "paths", "components", "security"); key != "" {
		return parsedDocument{}, nil, publicError("OPENAPI_CAPABILITY_UNSUPPORTED", fmt.Errorf("root member %q is unsupported", key))
	}
	var version string
	if err := json.Unmarshal(root["openapi"], &version); err != nil || version != Version {
		return parsedDocument{}, nil, publicError("OPENAPI_SPECIFICATION_UNSUPPORTED", errors.New("only OpenAPI 3.2.0 is supported"))
	}
	var dialect string
	if err := json.Unmarshal(root["jsonSchemaDialect"], &dialect); err != nil || dialect != JSONDialect {
		return parsedDocument{}, nil, publicError("OPENAPI_DIALECT_UNSUPPORTED", errors.New("the pinned JSON Schema dialect is required"))
	}
	if _, err := parseInfo(root["info"]); err != nil {
		return parsedDocument{}, nil, err
	}
	components, securitySchemes, err := parseComponents(root["components"], limits)
	if err != nil {
		return parsedDocument{}, nil, err
	}
	rootSecurity, err := parseSecurity(root["security"], securitySchemes)
	if err != nil {
		return parsedDocument{}, nil, err
	}
	operations, err := parsePaths(root["paths"], components, securitySchemes, rootSecurity, limits)
	if err != nil {
		return parsedDocument{}, nil, err
	}
	return parsedDocument{components: components, securitySchemes: securitySchemes, operations: operations}, nil, nil
}

func parseInfo(raw jsonRaw) (map[string]jsonRaw, error) {
	info, err := decodeObject(raw)
	if err != nil {
		return nil, publicError("OPENAPI_DOCUMENT_INVALID", errors.New("info object is required"))
	}
	if key := unsupportedKey(info, "title", "version"); key != "" {
		return nil, publicError("OPENAPI_CAPABILITY_UNSUPPORTED", fmt.Errorf("info member %q is unsupported", key))
	}
	var title, version string
	if json.Unmarshal(info["title"], &title) != nil || json.Unmarshal(info["version"], &version) != nil || title == "" || version == "" {
		return nil, publicError("OPENAPI_DOCUMENT_INVALID", errors.New("info title and version are required"))
	}
	return info, nil
}

func parseComponents(raw jsonRaw, limits Limits) (map[string]jsonRaw, map[string]SecurityScheme, error) {
	if len(raw) == 0 {
		return map[string]jsonRaw{}, map[string]SecurityScheme{}, nil
	}
	components, err := decodeObject(raw)
	if err != nil {
		return nil, nil, publicError("OPENAPI_DOCUMENT_INVALID", errors.New("components must be an object"))
	}
	if key := unsupportedKey(components, "schemas", "securitySchemes"); key != "" {
		return nil, nil, publicError("OPENAPI_CAPABILITY_UNSUPPORTED", fmt.Errorf("component kind %q is unsupported", key))
	}
	schemas := map[string]jsonRaw{}
	if len(components["schemas"]) != 0 {
		schemas, err = decodeObject(components["schemas"])
		if err != nil {
			return nil, nil, publicError("OPENAPI_DOCUMENT_INVALID", errors.New("component schemas must be an object"))
		}
	}
	if len(schemas) > limits.MaxSchemas {
		return nil, nil, publicError("OPENAPI_SCHEMA_LIMIT", errors.New("component schema count exceeds configured limit"))
	}
	for name := range schemas {
		if !operationIDPattern.MatchString(name) {
			return nil, nil, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("component schema name is not a portable identifier"))
		}
	}
	securitySchemes := map[string]SecurityScheme{}
	if len(components["securitySchemes"]) != 0 {
		rawSchemes, decodeErr := decodeObject(components["securitySchemes"])
		if decodeErr != nil {
			return nil, nil, publicError("OPENAPI_DOCUMENT_INVALID", errors.New("security schemes must be an object"))
		}
		for _, name := range sortedKeys(rawSchemes) {
			if !operationIDPattern.MatchString(name) {
				return nil, nil, publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("security scheme name is not a portable identifier"))
			}
			scheme, parseErr := parseSecurityScheme(rawSchemes[name])
			if parseErr != nil {
				return nil, nil, parseErr
			}
			securitySchemes[name] = scheme
		}
	}
	return schemas, securitySchemes, nil
}

func parseSecurityScheme(raw jsonRaw) (SecurityScheme, error) {
	value, err := decodeObject(raw)
	if err != nil {
		return SecurityScheme{}, publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("security scheme must be an object"))
	}
	if key := unsupportedKey(value, "type", "scheme", "name", "in"); key != "" {
		return SecurityScheme{}, publicError("OPENAPI_SECURITY_UNSUPPORTED", fmt.Errorf("security member %q is unsupported", key))
	}
	var result SecurityScheme
	_ = json.Unmarshal(value["type"], &result.Type)
	_ = json.Unmarshal(value["scheme"], &result.Scheme)
	_ = json.Unmarshal(value["name"], &result.Name)
	_ = json.Unmarshal(value["in"], &result.In)
	if result.Type == "http" && (result.Scheme == "bearer" || result.Scheme == "basic") {
		return result, nil
	}
	if result.Type == "apiKey" && result.In == "header" && validCredentialHeaderName(result.Name) {
		return result, nil
	}
	return SecurityScheme{}, publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("only bearer, basic, and header apiKey security are supported"))
}

func parsePaths(raw jsonRaw, components map[string]jsonRaw, securitySchemes map[string]SecurityScheme, inherited []map[string][]string, limits Limits) (map[string]httpOperation, error) {
	paths, err := decodeObject(raw)
	if err != nil {
		return nil, publicError("OPENAPI_DOCUMENT_INVALID", errors.New("paths object is required"))
	}
	operations := make(map[string]httpOperation)
	for _, path := range sortedKeys(paths) {
		if path == "" || path[0] != '/' || strings.ContainsAny(path, "{}?#") {
			return nil, publicError("OPENAPI_PATH_UNSUPPORTED", errors.New("only static absolute paths are supported"))
		}
		item, decodeErr := decodeObject(paths[path])
		if decodeErr != nil {
			return nil, publicError("OPENAPI_DOCUMENT_INVALID", errors.New("path item must be an object"))
		}
		if key := unsupportedKey(item, "get", "put", "post", "delete", "patch", "options"); key != "" {
			return nil, publicError("OPENAPI_CAPABILITY_UNSUPPORTED", fmt.Errorf("path member %q is unsupported", key))
		}
		for _, method := range []string{"get", "put", "post", "delete", "patch", "options"} {
			if len(item[method]) == 0 {
				continue
			}
			operation, parseErr := parseOperation(method, path, item[method], components, securitySchemes, inherited)
			if parseErr != nil {
				return nil, parseErr
			}
			if _, duplicate := operations[operation.id]; duplicate {
				return nil, publicError("OPENAPI_OPERATION_INVALID", errors.New("operationId is duplicated"))
			}
			operations[operation.id] = operation
			if len(operations) > limits.MaxOperations {
				return nil, publicError("OPENAPI_OPERATION_LIMIT", errors.New("operation count exceeds configured limit"))
			}
		}
	}
	return operations, nil
}

func parseOperation(method, path string, raw jsonRaw, components map[string]jsonRaw, securitySchemes map[string]SecurityScheme, inherited []map[string][]string) (httpOperation, error) {
	value, err := decodeObject(raw)
	if err != nil {
		return httpOperation{}, publicError("OPENAPI_DOCUMENT_INVALID", errors.New("operation must be an object"))
	}
	if key := unsupportedKey(value, "operationId", "summary", "description", "requestBody", "responses", "security", "parameters"); key != "" {
		return httpOperation{}, publicError("OPENAPI_CAPABILITY_UNSUPPORTED", fmt.Errorf("operation member %q is unsupported", key))
	}
	var operationID, description string
	if json.Unmarshal(value["operationId"], &operationID) != nil || !operationIDPattern.MatchString(operationID) {
		return httpOperation{}, publicError("OPENAPI_OPERATION_INVALID", errors.New("operationId is required and must be a bounded identifier"))
	}
	_ = json.Unmarshal(value["description"], &description)
	if description == "" {
		_ = json.Unmarshal(value["summary"], &description)
	}
	fieldMask, err := parseParameters(value["parameters"])
	if err != nil {
		return httpOperation{}, err
	}
	requestSchema, err := parseRequestBody(value["requestBody"])
	if err != nil {
		return httpOperation{}, err
	}
	responseSchema, successStatus, err := parseResponses(value["responses"])
	if err != nil {
		return httpOperation{}, err
	}
	security := inherited
	if len(value["security"]) != 0 {
		security, err = parseSecurity(value["security"], securitySchemes)
		if err != nil {
			return httpOperation{}, err
		}
	}
	securityHeaders := securityHeaderAlternatives(security, securitySchemes)
	return httpOperation{
		id: operationID, method: strings.ToUpper(method), path: path, description: description,
		fieldMaskQuery: fieldMask, requestSchema: requestSchema, responseSchema: responseSchema, successStatus: successStatus,
		components: components, securityRequired: securityIsRequired(security), securityHeaders: securityHeaders,
	}, nil
}

func parseParameters(raw jsonRaw) (string, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("[]")) {
		return "", nil
	}
	var parameters []map[string]jsonRaw
	if err := json.Unmarshal(raw, &parameters); err != nil || len(parameters) != 1 {
		return "", publicError("OPENAPI_PARAMETER_UNSUPPORTED", errors.New("only one field-mask query parameter is supported"))
	}
	parameter := parameters[0]
	if key := unsupportedKey(parameter, "name", "in", "required", "style", "explode", "schema", "description"); key != "" {
		return "", publicError("OPENAPI_PARAMETER_UNSUPPORTED", fmt.Errorf("parameter member %q is unsupported", key))
	}
	var name, location, style string
	var required, explode bool
	_ = json.Unmarshal(parameter["name"], &name)
	_ = json.Unmarshal(parameter["in"], &location)
	_ = json.Unmarshal(parameter["style"], &style)
	_ = json.Unmarshal(parameter["required"], &required)
	_ = json.Unmarshal(parameter["explode"], &explode)
	schemaValue, err := decodeObject(parameter["schema"])
	if err != nil {
		return "", publicError("OPENAPI_PARAMETER_UNSUPPORTED", errors.New("field-mask parameter requires a schema"))
	}
	var schemaType string
	_ = json.Unmarshal(schemaValue["type"], &schemaType)
	if !operationIDPattern.MatchString(name) || location != "query" || required || style != "form" || explode || schemaType != "string" || unsupportedKey(schemaValue, "type") != "" {
		return "", publicError("OPENAPI_PARAMETER_UNSUPPORTED", errors.New("parameter is outside the field-mask subset"))
	}
	return name, nil
}

func parseRequestBody(raw jsonRaw) (jsonRaw, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	body, err := decodeObject(raw)
	if err != nil || unsupportedKey(body, "description", "required", "content") != "" {
		return nil, publicError("OPENAPI_REQUEST_BODY_UNSUPPORTED", errors.New("request body is outside the JSON subset"))
	}
	var required bool
	if json.Unmarshal(body["required"], &required) != nil || !required {
		return nil, publicError("OPENAPI_REQUEST_BODY_UNSUPPORTED", errors.New("mapped request bodies must be required"))
	}
	return contentSchema(body["content"], "application/json", "OPENAPI_REQUEST_BODY_UNSUPPORTED")
}

func parseResponses(raw jsonRaw) (jsonRaw, int, error) {
	responses, err := decodeObject(raw)
	if err != nil {
		return nil, 0, publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("responses object is required"))
	}
	var success jsonRaw
	var successStatus int
	for _, status := range sortedKeys(responses) {
		response, decodeErr := decodeObject(responses[status])
		if decodeErr != nil || unsupportedKey(response, "description", "content") != "" {
			return nil, 0, publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("response is outside the JSON/problem subset"))
		}
		if statusPattern.MatchString(status) {
			if success != nil {
				return nil, 0, publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("exactly one success status is supported"))
			}
			success, err = contentSchema(response["content"], "application/json", "OPENAPI_RESPONSE_UNSUPPORTED")
			if err != nil {
				return nil, 0, err
			}
			successStatus, _ = strconv.Atoi(status)
			continue
		}
		if status != "default" && !regexp.MustCompile(`^[1-5][0-9][0-9]$`).MatchString(status) {
			return nil, 0, publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("response status key is unsupported"))
		}
		if len(response["content"]) != 0 {
			if _, contentErr := contentSchemaEither(response["content"], []string{"application/problem+json", "application/json"}, "OPENAPI_RESPONSE_UNSUPPORTED"); contentErr != nil {
				return nil, 0, contentErr
			}
		}
	}
	if success == nil {
		return nil, 0, publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("one exact success response is required"))
	}
	return success, successStatus, nil
}

func contentSchema(raw jsonRaw, mediaType, code string) (jsonRaw, error) {
	return contentSchemaEither(raw, []string{mediaType}, code)
}

func contentSchemaEither(raw jsonRaw, mediaTypes []string, code string) (jsonRaw, error) {
	content, err := decodeObject(raw)
	if err != nil || len(content) != 1 {
		return nil, publicError(code, errors.New("exactly one supported media type is required"))
	}
	mediaType := sortedKeys(content)[0]
	if !slices.Contains(mediaTypes, mediaType) {
		return nil, publicError(code, errors.New("media type is unsupported"))
	}
	entry, err := decodeObject(content[mediaType])
	if err != nil || unsupportedKey(entry, "schema") != "" || len(entry["schema"]) == 0 {
		return nil, publicError(code, errors.New("media type schema is required"))
	}
	return slices.Clone(entry["schema"]), nil
}

func parseSecurity(raw jsonRaw, schemes map[string]SecurityScheme) ([]map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rawRequirements []map[string]jsonRaw
	if err := json.Unmarshal(raw, &rawRequirements); err != nil || rawRequirements == nil {
		return nil, publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("security requirements must be an array"))
	}
	requirements := make([]map[string][]string, 0, len(rawRequirements))
	for _, rawRequirement := range rawRequirements {
		requirement := make(map[string][]string, len(rawRequirement))
		headers := make(map[string]bool, len(rawRequirement))
		for name, rawScopes := range rawRequirement {
			var scopes []string
			scheme, ok := schemes[name]
			if json.Unmarshal(rawScopes, &scopes) != nil || scopes == nil || !ok || len(scopes) != 0 {
				return nil, publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("security requirement is not a configured non-OAuth scheme"))
			}
			header := "Authorization"
			if scheme.Type == "apiKey" {
				header = http.CanonicalHeaderKey(scheme.Name)
			}
			if headers[header] {
				return nil, publicError("OPENAPI_SECURITY_UNSUPPORTED", errors.New("combined security schemes share one credential header"))
			}
			headers[header] = true
			requirement[name] = scopes
		}
		requirements = append(requirements, requirement)
	}
	return requirements, nil
}

func importTypes(document parsedDocument, bindings map[string]Binding, limits Limits) (schema.Snapshot, map[string]operationTypes, error) {
	compiler := schemaCompiler{
		catalog: schema.NewCatalog(), components: document.components, limits: limits,
		compiled: make(map[string]compiledSchema), active: make(map[string]bool),
	}
	result := make(map[string]operationTypes, len(bindings))
	for _, operationID := range sortedKeys(bindings) {
		operation := document.operations[operationID]
		var input schema.TypeID
		var inputNullable bool
		var err error
		if len(operation.requestSchema) != 0 {
			input, inputNullable, err = compiler.compile(operation.requestSchema, operationID+"Input", true, 0)
			if err != nil {
				return schema.Snapshot{}, nil, err
			}
		}
		output, nullable, err := compiler.compile(operation.responseSchema, operationID+"Output", false, 0)
		if err != nil {
			return schema.Snapshot{}, nil, err
		}
		outputDescriptor, outputKnown := compiler.catalogDescriptor(output)
		if nullable || !outputKnown || outputDescriptor.Kind != schema.ObjectType {
			return schema.Snapshot{}, nil, publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("runtime-consume responses must be non-null objects"))
		}
		result[operationID] = operationTypes{input: input, inputNullable: inputNullable, output: output}
	}
	snapshot, err := compiler.catalog.Freeze()
	if err != nil {
		return schema.Snapshot{}, nil, publicError("OPENAPI_SCHEMA_INVALID", err)
	}
	return snapshot, result, nil
}

type schemaCompiler struct {
	catalog    *schema.Catalog
	components map[string]jsonRaw
	limits     Limits
	compiled   map[string]compiledSchema
	active     map[string]bool
	registered map[schema.TypeID]schema.TypeDescriptor
	properties int
}

func (c *schemaCompiler) compile(raw jsonRaw, name string, input bool, depth int) (schema.TypeID, bool, error) {
	if depth > c.limits.MaxDepth {
		return "", false, publicError("OPENAPI_SCHEMA_LIMIT", errors.New("schema depth exceeds configured limit"))
	}
	value, err := decodeObject(raw)
	if err != nil {
		return "", false, publicError("OPENAPI_SCHEMA_INVALID", errors.New("schema must be an object"))
	}
	if reference := value["$ref"]; len(reference) != 0 {
		if len(value) != 1 {
			return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("reference siblings are unsupported"))
		}
		return c.compileReference(reference, input, depth)
	}
	if key := unsupportedKey(value, "type", "format", "properties", "required", "items", "enum", "additionalProperties"); key != "" {
		if key == "oneOf" || key == "anyOf" || key == "allOf" || key == "not" || key == "discriminator" {
			return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", fmt.Errorf("schema keyword %q is unsupported", key))
		}
		return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", fmt.Errorf("schema keyword %q is outside the exact subset", key))
	}
	typeName, nullable, err := decodeSchemaType(value["type"])
	if err != nil {
		return "", false, err
	}
	if len(value["enum"]) != 0 {
		if key := unsupportedKey(value, "type", "enum"); key != "" {
			return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", fmt.Errorf("string enum keyword %q is unsupported", key))
		}
		return c.compileEnum(value, name, input, nullable)
	}
	switch typeName {
	case "boolean", "string", "integer", "number":
		if key := unsupportedKey(value, "type", "format"); key != "" {
			return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", fmt.Errorf("scalar keyword %q is unsupported", key))
		}
		identifier, scalarErr := scalarType(typeName, value["format"])
		return identifier, nullable, scalarErr
	case "object":
		if nullable {
			return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("nullable composite references are outside the exact subset"))
		}
		if key := unsupportedKey(value, "type", "properties", "required", "additionalProperties"); key != "" {
			return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", fmt.Errorf("object keyword %q is unsupported", key))
		}
		identifier, objectErr := c.compileObject(value, name, input, depth)
		return identifier, nullable, objectErr
	case "array":
		if nullable {
			return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("nullable composite references are outside the exact subset"))
		}
		if key := unsupportedKey(value, "type", "items"); key != "" {
			return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", fmt.Errorf("array keyword %q is unsupported", key))
		}
		identifier, arrayErr := c.compileArray(value, name, input, depth)
		return identifier, nullable, arrayErr
	default:
		return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("schema type is unsupported"))
	}
}

func (c *schemaCompiler) compileReference(raw jsonRaw, input bool, depth int) (schema.TypeID, bool, error) {
	var reference string
	if json.Unmarshal(raw, &reference) != nil || !strings.HasPrefix(reference, "#/components/schemas/") {
		return "", false, publicError("OPENAPI_REFERENCE_BLOCKED", errors.New("only local component schema references are supported"))
	}
	name := strings.TrimPrefix(reference, "#/components/schemas/")
	if !operationIDPattern.MatchString(name) {
		return "", false, publicError("OPENAPI_REFERENCE_BLOCKED", errors.New("component reference is invalid"))
	}
	component, ok := c.components[name]
	if !ok {
		return "", false, publicError("OPENAPI_REFERENCE_BLOCKED", errors.New("component reference is unresolved"))
	}
	key := name + fmt.Sprintf("/%t", input)
	if compiled, ok := c.compiled[key]; ok {
		return compiled.identifier, compiled.nullable, nil
	}
	if c.active[key] {
		return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("recursive schemas require an application-supplied bound"))
	}
	c.active[key] = true
	identifier, nullable, err := c.compile(component, name, input, depth+1)
	delete(c.active, key)
	if err == nil {
		c.compiled[key] = compiledSchema{identifier: identifier, nullable: nullable}
	}
	return identifier, nullable, err
}

func (c *schemaCompiler) compileObject(value map[string]jsonRaw, name string, input bool, depth int) (schema.TypeID, error) {
	var additional bool
	if json.Unmarshal(value["additionalProperties"], &additional) != nil || additional {
		return "", publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("objects must explicitly disable additional properties"))
	}
	properties, err := decodeObject(value["properties"])
	if err != nil || len(properties) == 0 {
		return "", publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("objects require bounded named properties"))
	}
	c.properties += len(properties)
	if c.properties > c.limits.MaxProperties {
		return "", publicError("OPENAPI_PROPERTY_LIMIT", errors.New("property count exceeds configured limit"))
	}
	var required []string
	if len(value["required"]) != 0 && json.Unmarshal(value["required"], &required) != nil {
		return "", publicError("OPENAPI_SCHEMA_INVALID", errors.New("required must be a string array"))
	}
	requiredSet := make(map[string]bool, len(required))
	for _, field := range required {
		if _, ok := properties[field]; !ok || requiredSet[field] {
			return "", publicError("OPENAPI_SCHEMA_INVALID", errors.New("required field is absent or duplicated"))
		}
		requiredSet[field] = true
	}
	fields := make(map[string]schema.FieldDescriptor, len(properties))
	for _, field := range sortedKeys(properties) {
		if !operationIDPattern.MatchString(field) {
			return "", publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("property name is not a portable identifier"))
		}
		fieldType, nullable, compileErr := c.compile(properties[field], name+"."+field, input, depth+1)
		if compileErr != nil {
			return "", compileErr
		}
		fields[field] = schema.FieldDescriptor{Type: fieldType, Required: requiredSet[field], Nullable: nullable}
	}
	kind := schema.ObjectType
	if input {
		kind = schema.InputObjectType
	}
	identifier := schema.TypeID("openapi." + name + positionSuffix(input))
	descriptor := schema.TypeDescriptor{ID: identifier, Name: name, Kind: kind, Input: input, Output: !input, Fields: fields}
	if err := c.register(descriptor); err != nil {
		return "", err
	}
	return identifier, nil
}

func (c *schemaCompiler) compileArray(value map[string]jsonRaw, name string, input bool, depth int) (schema.TypeID, error) {
	if len(value["items"]) == 0 {
		return "", publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("arrays require item schemas"))
	}
	element, nullable, err := c.compile(value["items"], name+"Item", input, depth+1)
	if err != nil {
		return "", err
	}
	identifier := schema.TypeID("openapi." + name + positionSuffix(input) + ".list")
	descriptor := schema.TypeDescriptor{ID: identifier, Name: name, Kind: schema.ListType, Input: input, Output: !input, Element: element, ElementNullable: nullable}
	if err := c.register(descriptor); err != nil {
		return "", err
	}
	return identifier, nil
}

func (c *schemaCompiler) compileEnum(value map[string]jsonRaw, name string, input, nullable bool) (schema.TypeID, bool, error) {
	typeName, _, err := decodeSchemaType(value["type"])
	if err != nil || typeName != "string" || len(value["format"]) != 0 {
		return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("only string enums are supported"))
	}
	var values []string
	if json.Unmarshal(value["enum"], &values) != nil || len(values) == 0 {
		return "", false, publicError("OPENAPI_SCHEMA_INVALID", errors.New("enum values must be non-empty strings"))
	}
	identifier := schema.TypeID("openapi." + name + positionSuffix(input) + ".enum")
	descriptor := schema.TypeDescriptor{ID: identifier, Name: name, Kind: schema.EnumType, Input: input, Output: !input, EnumValues: values}
	if err := c.register(descriptor); err != nil {
		return "", false, err
	}
	return identifier, nullable, nil
}

func (c *schemaCompiler) register(descriptor schema.TypeDescriptor) error {
	if c.registered == nil {
		c.registered = make(map[schema.TypeID]schema.TypeDescriptor)
	}
	if prior, ok := c.registered[descriptor.ID]; ok {
		if !schemaDescriptorsEqual(prior, descriptor) {
			return publicError("OPENAPI_SCHEMA_INVALID", errors.New("derived schema identifier collision"))
		}
		return nil
	}
	if err := c.catalog.Register(descriptor); err != nil {
		return publicError("OPENAPI_SCHEMA_INVALID", err)
	}
	c.registered[descriptor.ID] = descriptor
	return nil
}

func (c *schemaCompiler) catalogDescriptor(id schema.TypeID) (schema.TypeDescriptor, bool) {
	if descriptor, ok := c.registered[id]; ok {
		return descriptor, true
	}
	return schema.TypeDescriptor{ID: id, Kind: schema.ScalarType}, true
}

func schemaDescriptorsEqual(left, right schema.TypeDescriptor) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func decodeSchemaType(raw jsonRaw) (string, bool, error) {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single, false, nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil || len(values) != 2 || !slices.Contains(values, "null") {
		return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("schema type must be one type or one type plus null"))
	}
	if values[0] == "null" {
		return values[1], true, nil
	}
	if values[1] == "null" {
		return values[0], true, nil
	}
	return "", false, publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("nullable schema has no concrete type"))
}

func scalarType(typeName string, rawFormat jsonRaw) (schema.TypeID, error) {
	var format string
	if len(rawFormat) != 0 && json.Unmarshal(rawFormat, &format) != nil {
		return "", publicError("OPENAPI_SCHEMA_INVALID", errors.New("format must be a string"))
	}
	switch typeName + "/" + format {
	case "boolean/":
		return schema.TypeID(schema.Boolean), nil
	case "string/":
		return schema.TypeID(schema.String), nil
	case "string/id":
		return schema.TypeID(schema.ID), nil
	case "string/uuid":
		return schema.TypeID(schema.UUID), nil
	case "string/date-time":
		return schema.TypeID(schema.Timestamp), nil
	case "string/duration":
		return schema.TypeID(schema.Duration), nil
	case "string/decimal":
		return schema.TypeID(schema.Decimal), nil
	case "integer/int32":
		return schema.TypeID(schema.Int32), nil
	case "integer/int64":
		return schema.TypeID(schema.Int64), nil
	case "integer/uint64":
		return schema.TypeID(schema.UInt64), nil
	case "number/", "number/double":
		return schema.TypeID(schema.Float64), nil
	default:
		return "", publicError("OPENAPI_SCHEMA_UNSUPPORTED", errors.New("scalar type and format have no exact mapping"))
	}
}

func decodeObject(raw []byte) (map[string]jsonRaw, error) {
	if len(raw) == 0 {
		return nil, errors.New("object is missing")
	}
	var value map[string]jsonRaw
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, errors.New("value must be an object")
	}
	return value, nil
}

func unsupportedKey(value map[string]jsonRaw, allowed ...string) string {
	allow := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allow[key] = true
	}
	for _, key := range sortedKeys(value) {
		if !allow[key] {
			return key
		}
	}
	return ""
}

func sortedKeys[V any](value map[string]V) []string {
	keys := slices.Collect(maps.Keys(value))
	sort.Strings(keys)
	return keys
}

func positionSuffix(input bool) string {
	if input {
		return ".input"
	}
	return ".output"
}

func errorPublicCode(err error, fallback string) string {
	var failure interface{ PublicCode() string }
	if errors.As(err, &failure) {
		return failure.PublicCode()
	}
	return fallback
}

func applyRuntimeLimitDefaults(config *HTTPConfig, limits Limits) error {
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = limits.MaxRequestBytes
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = limits.MaxResponseBytes
	}
	if config.MaxProjectionFields == 0 {
		config.MaxProjectionFields = limits.MaxProjectionFields
		if config.MaxProjectionFields == 0 {
			config.MaxProjectionFields = limits.MaxFanOut
		}
	}
	maximumProjection := limits.MaxProjectionFields
	if maximumProjection == 0 {
		maximumProjection = limits.MaxFanOut
	}
	if config.MaxRequestBytes > limits.MaxRequestBytes || config.MaxResponseBytes > limits.MaxResponseBytes || config.MaxProjectionFields > maximumProjection {
		return errors.New("HTTP limits may narrow but must not widen adapter limits")
	}
	return nil
}

func securityIsRequired(requirements []map[string][]string) bool {
	if len(requirements) == 0 {
		return false
	}
	for _, requirement := range requirements {
		if len(requirement) == 0 {
			return false
		}
	}
	return true
}

func securityHeaderAlternatives(requirements []map[string][]string, schemes map[string]SecurityScheme) [][]string {
	result := make([][]string, 0, len(requirements))
	for _, requirement := range requirements {
		headers := make([]string, 0, len(requirement))
		for name := range requirement {
			scheme := schemes[name]
			header := "Authorization"
			if scheme.Type == "apiKey" {
				header = http.CanonicalHeaderKey(scheme.Name)
			}
			headers = append(headers, header)
		}
		sort.Strings(headers)
		result = append(result, slices.Compact(headers))
	}
	return result
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := range len(name) {
		character := name[index]
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			return false
		}
	}
	return true
}
