package asyncapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

const (
	// ProjectionProfile is the issue #102 payload-schema projection layered on
	// the metadata-only adapter.asyncapi-1 contract.
	ProjectionProfile         = "adapter.asyncapi.projection-1"
	ProjectionExporterVersion = "asyncapi-projection-exporter-1"
	ProjectionVersion         = "1.0.0"
	ProjectionJSONDialect     = schema.JSONSchema202012
)

// ProjectionExportOptions bounds the complete schema projection. Zero values
// select DefaultLimits.
type ProjectionExportOptions struct {
	Limits Limits
}

// ProjectionImportOptions requires the caller-authorized Naatre schema.
// AsyncAPI content is never allowed to become schema, effect, or authorization
// authority.
type ProjectionImportOptions struct {
	Limits              Limits
	AllowedServerURLs   []string
	AuthoritativeSchema schema.Document
}

type projectionExtension struct {
	Profile        string          `json:"profile"`
	Version        string          `json:"version"`
	JSONDialect    string          `json:"jsonSchemaDialect"`
	SchemaRevision string          `json:"schemaRevision"`
	SchemaDigest   string          `json:"schemaDigest"`
	SchemaDocument json.RawMessage `json:"schemaDocument"`
	CoreProfile    string          `json:"coreProfile"`
	CoreExporter   string          `json:"coreExporter"`
	Authority      string          `json:"authority"`
	HandlerCalls   int             `json:"businessHandlerCalls"`
}

// ExportProjection emits the complete, deterministic JSON Schema projection
// profile. It performs no I/O and invokes no runtime or business handler.
func ExportProjection(ctx context.Context, input Model, options ProjectionExportOptions) ([]byte, FidelityReport, error) {
	limits, err := resolveLimits(options.Limits)
	report := newProjectionReport(interopadapter.SchemaExport, Revisions{})
	if err != nil {
		return projectionExportFailure(report, "ASYNCAPI_LIMIT_INVALID", "resource-limits")
	}
	if err := projectionContext(ctx); err != nil {
		return projectionExportFailure(report, "ASYNCAPI_PROJECTION_CANCELLED", "cancellation")
	}
	return exportProjection(ctx, input, limits)
}

func exportProjection(ctx context.Context, input Model, limits Limits) ([]byte, FidelityReport, error) {
	base, baseReport, err := Export(input)
	report := newProjectionReport(interopadapter.SchemaExport, baseReport.Revisions)
	if err != nil {
		code := errorCode(err, "ASYNCAPI_EXPORT_INVALID")
		return projectionExportFailure(report, code, "document")
	}
	if err := projectionContext(ctx); err != nil {
		return projectionExportFailure(report, "ASYNCAPI_PROJECTION_CANCELLED", "cancellation")
	}

	schemaSize, err := input.Schema.CanonicalJSONSize()
	if err != nil || schemaSize > limits.MaxDocumentBytes || len(input.Schema.Types()) > limits.MaxObjects {
		return projectionExportFailure(report, "ASYNCAPI_SCHEMA_LIMIT", "resource-limits")
	}
	canonicalSchema, err := input.Schema.CanonicalJSON()
	if err != nil {
		return projectionExportFailure(report, "ASYNCAPI_SCHEMA_INVALID", "schema-authority")
	}
	digest, err := input.Schema.Hash()
	if err != nil {
		return projectionExportFailure(report, "ASYNCAPI_SCHEMA_INVALID", "schema-authority")
	}
	projected, err := projectSchema(ctx, input.Schema, report.Revisions)
	if err != nil {
		code := errorCode(err, "ASYNCAPI_SCHEMA_PROJECTION_UNSUPPORTED")
		feature := "payload-schema-projection"
		if code == "ASYNCAPI_PROJECTION_CANCELLED" {
			feature = "cancellation"
		}
		return projectionExportFailure(report, code, feature)
	}

	var root map[string]json.RawMessage
	if json.Unmarshal(base, &root) != nil {
		return projectionExportFailure(report, "ASYNCAPI_EXPORT_INVALID", "document")
	}
	if err := installProjectedSchemas(root, projected); err != nil {
		return projectionExportFailure(report, "ASYNCAPI_EXPORT_INVALID", "document")
	}
	revisions := report.Revisions
	revisions.AsyncAPIProfile = ProjectionProfile
	revisions.Exporter = ProjectionExporterVersion
	report.Revisions = revisions
	extension := projectionExtension{
		Profile: ProjectionProfile, Version: ProjectionVersion, JSONDialect: ProjectionJSONDialect,
		SchemaRevision: input.Schema.Revision(), SchemaDigest: "sha256:" + digest.Hex,
		SchemaDocument: canonicalSchema, CoreProfile: Profile, CoreExporter: ExporterVersion,
		Authority: "caller-authorized-naatre-schema", HandlerCalls: 0,
	}
	setRaw(root, "x-naatre-profile", ProjectionProfile)
	setRaw(root, "x-naatre-exporter", ProjectionExporterVersion)
	setRaw(root, "x-naatre-revisions", revisions)
	setRaw(root, "x-naatre-fidelity", projectionMappings())
	setRaw(root, "x-naatre-projection", extension)

	encoded, err := json.Marshal(root)
	if err != nil {
		return projectionExportFailure(report, "ASYNCAPI_EXPORT_INVALID", "document")
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{MaxBytes: limits.MaxDocumentBytes, MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxObjects})
	if err != nil || len(canonical) > limits.MaxDocumentBytes {
		return projectionExportFailure(report, "ASYNCAPI_PROJECTION_DOCUMENT_LIMIT", "resource-limits")
	}
	report.Links = slices.Clone(baseReport.Links)
	report.Status = "ready"
	return canonical, report, nil
}

// ImportProjection validates a complete projection against the exact schema
// supplied by the caller. The returned model retains that caller-owned schema.
func ImportProjection(ctx context.Context, input []byte, options ProjectionImportOptions) (Model, FidelityReport, error) {
	limits, err := resolveLimits(options.Limits)
	report := newProjectionReport(interopadapter.SchemaImport, Revisions{})
	if err != nil {
		return projectionImportFailure(report, "ASYNCAPI_LIMIT_INVALID", "resource-limits")
	}
	if err := projectionContext(ctx); err != nil {
		return projectionImportFailure(report, "ASYNCAPI_PROJECTION_CANCELLED", "cancellation")
	}
	authority, err := canonicalProjectionAuthority(options.AuthoritativeSchema, limits)
	if err != nil {
		code := errorCode(err, "ASYNCAPI_SCHEMA_AUTHORITY_REQUIRED")
		return projectionImportFailure(report, code, projectionFailureFeature(code))
	}
	canonical, root, extension, revisions, err := decodeProjectionDocument(input, limits)
	report.Revisions = revisions
	if err != nil {
		code := errorCode(err, "ASYNCAPI_DOCUMENT_INVALID")
		return projectionImportFailure(report, code, projectionFailureFeature(code))
	}
	if !projectionAuthorityMatches(extension, revisions, options.AuthoritativeSchema, authority, limits) {
		return projectionImportFailure(report, "ASYNCAPI_SCHEMA_AUTHORITY_MISMATCH", "schema-authority")
	}
	if err := projectionContext(ctx); err != nil {
		return projectionImportFailure(report, "ASYNCAPI_PROJECTION_CANCELLED", "cancellation")
	}
	base, err := restoreCoreDocument(root, revisions)
	if err != nil {
		return projectionImportFailure(report, errorCode(err, "ASYNCAPI_DOCUMENT_INVALID"), "document")
	}
	model, baseReport, err := Import(base, ImportOptions{Limits: limits, AllowedServerURLs: options.AllowedServerURLs})
	if err != nil {
		return projectionImportFailure(report, errorCode(err, "ASYNCAPI_DOCUMENT_INVALID"), importErrorFeature(err))
	}
	model.Schema = options.AuthoritativeSchema
	model.Revisions = revisions
	if code := projectionRoundTripCode(ctx, model, canonical, limits); code != "" {
		return projectionImportFailure(report, code, projectionFailureFeature(code))
	}
	report.Links = slices.Clone(baseReport.Links)
	report.Status = "ready"
	return model, report, nil
}

// ValidateProjection performs the complete import validation without returning
// a model that could be mistaken for a registered runtime surface.
func ValidateProjection(ctx context.Context, input []byte, options ProjectionImportOptions) (FidelityReport, error) {
	_, report, err := ImportProjection(ctx, input, options)
	return report, err
}

func canonicalProjectionAuthority(document schema.Document, limits Limits) ([]byte, error) {
	size, err := document.CanonicalJSONSize()
	if err != nil {
		return nil, publicError("ASYNCAPI_SCHEMA_AUTHORITY_REQUIRED", errors.New("schema authority is unavailable"))
	}
	if size > limits.MaxDocumentBytes || len(document.Types()) > limits.MaxObjects {
		return nil, publicError("ASYNCAPI_SCHEMA_LIMIT", errors.New("schema authority exceeds projection limits"))
	}
	canonical, err := document.CanonicalJSON()
	if err != nil {
		return nil, publicError("ASYNCAPI_SCHEMA_AUTHORITY_REQUIRED", errors.New("schema authority is unavailable"))
	}
	return canonical, nil
}

func decodeProjectionDocument(input []byte, limits Limits) ([]byte, map[string]json.RawMessage, projectionExtension, Revisions, error) {
	if len(input) > limits.MaxDocumentBytes {
		return nil, nil, projectionExtension{}, Revisions{}, publicError("ASYNCAPI_PROJECTION_DOCUMENT_LIMIT", errors.New("projection document exceeds limits"))
	}
	canonical, err := protocol.CanonicalizeJSON(input, protocol.Limits{MaxBytes: limits.MaxDocumentBytes, MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxObjects})
	if err != nil {
		return nil, nil, projectionExtension{}, Revisions{}, publicError("ASYNCAPI_DOCUMENT_INVALID", errors.New("projection document is invalid"))
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(canonical, &root) != nil {
		return nil, nil, projectionExtension{}, Revisions{}, publicError("ASYNCAPI_DOCUMENT_INVALID", errors.New("projection document is invalid"))
	}
	extension, revisions, err := readProjectionHeader(root)
	return canonical, root, extension, revisions, err
}

func projectionAuthorityMatches(extension projectionExtension, revisions Revisions, document schema.Document, authority []byte, limits Limits) bool {
	embedded, err := protocol.CanonicalizeSchema(extension.SchemaDocument, protocol.Limits{MaxBytes: limits.MaxDocumentBytes, MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxObjects})
	if err != nil || !bytes.Equal(embedded, authority) || extension.SchemaRevision != document.Revision() {
		return false
	}
	digest, err := document.Hash()
	return err == nil && extension.SchemaDigest == "sha256:"+digest.Hex && revisions.SchemaDigest == extension.SchemaDigest
}

func projectionRoundTripCode(ctx context.Context, model Model, canonical []byte, limits Limits) string {
	expected, _, err := exportProjection(ctx, model, limits)
	if errorCode(err, "") == "ASYNCAPI_PROJECTION_CANCELLED" {
		return "ASYNCAPI_PROJECTION_CANCELLED"
	}
	if err != nil || !bytes.Equal(expected, canonical) {
		return "ASYNCAPI_PROJECTION_MISMATCH"
	}
	return ""
}

func projectionFailureFeature(code string) string {
	switch code {
	case "ASYNCAPI_PROJECTION_CANCELLED":
		return "cancellation"
	case "ASYNCAPI_LIMIT_INVALID", "ASYNCAPI_PROJECTION_DOCUMENT_LIMIT", "ASYNCAPI_SCHEMA_LIMIT":
		return "resource-limits"
	case "ASYNCAPI_SCHEMA_AUTHORITY_REQUIRED", "ASYNCAPI_SCHEMA_AUTHORITY_MISMATCH":
		return "schema-authority"
	case "ASYNCAPI_PROJECTION_MISMATCH":
		return "payload-schema-projection"
	case "ASYNCAPI_PROJECTION_UNSUPPORTED":
		return "projection-profile"
	default:
		return "document"
	}
}

func projectSchema(ctx context.Context, document schema.Document, revisions Revisions) (map[string]any, error) {
	types := make(map[schema.TypeID]schema.TypeDeclaration)
	for _, declaration := range document.Types() {
		types[declaration.ID] = declaration
	}
	result := make(map[string]any, len(types)+len(coreScalarIDs))
	for _, id := range coreScalarIDs {
		projected := coreScalarSchema(id)
		projected["x-naatre-kind"] = schema.ScalarType
		projected["x-naatre-implicit-core-scalar"] = true
		projected["x-naatre-identity"] = identity("schema", string(id), revisions.Schema, revisions)
		projected["x-naatre-schema-digest"] = revisions.SchemaDigest
		result[string(id)] = projected
	}
	ids := make([]string, 0, len(types))
	for id := range types {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := projectionContext(ctx); err != nil {
			return nil, err
		}
		declaration := types[schema.TypeID(id)]
		projected, err := projectDeclaration(ctx, declaration, types)
		if err != nil {
			return nil, err
		}
		projected["x-naatre-identity"] = identity("schema", id, revisions.Schema, revisions)
		projected["x-naatre-declaration"] = declaration
		projected["x-naatre-schema-digest"] = revisions.SchemaDigest
		result[id] = projected
	}
	return result, nil
}

func projectDeclaration(ctx context.Context, value schema.TypeDeclaration, types map[schema.TypeID]schema.TypeDeclaration) (map[string]any, error) {
	var result map[string]any
	var err error
	switch value.Kind {
	case schema.ObjectType, schema.InputObjectType, schema.InterfaceType, schema.OneOfType:
		result, err = projectObject(ctx, value, types)
	case schema.ListType:
		var items map[string]any
		items, err = projectReference(value.Element, value.ElementNullable, types)
		result = map[string]any{"type": "array", "items": items}
	case schema.MapType:
		var values map[string]any
		values, err = projectReference(value.Element, value.ElementNullable, types)
		result = map[string]any{"type": "object", "additionalProperties": values}
	case schema.EnumType:
		members := make([]string, 0, len(value.EnumMembers))
		for _, member := range value.EnumMembers {
			members = append(members, member.Name)
		}
		result = map[string]any{"type": "string", "enum": members}
	case schema.UnionType:
		var variants []any
		variants, err = projectVariants(ctx, value.VariantMembers, types)
		result = map[string]any{"oneOf": variants}
	case schema.ScalarType:
		result, err = projectCustomScalar(ctx, value)
	default:
		return nil, publicError("ASYNCAPI_SCHEMA_PROJECTION_UNSUPPORTED", errors.New("schema kind is outside the projection profile"))
	}
	if err != nil {
		return nil, err
	}
	result["x-naatre-kind"] = value.Kind
	if value.Description != "" {
		result["description"] = value.Description
	}
	if value.Deprecation != nil {
		result["deprecated"] = true
	}
	if value.MaxDepth != 0 {
		result["x-naatre-max-depth"] = value.MaxDepth
	}
	return result, nil
}

func projectObject(ctx context.Context, value schema.TypeDeclaration, types map[schema.TypeID]schema.TypeDeclaration) (map[string]any, error) {
	properties := make(map[string]any, len(value.Fields))
	required := make([]string, 0, len(value.Fields))
	for _, field := range value.Fields {
		if err := projectionContext(ctx); err != nil {
			return nil, err
		}
		projected, err := projectReference(field.Type, field.Nullable, types)
		if err != nil {
			return nil, err
		}
		if len(field.Default) != 0 {
			projected["default"] = json.RawMessage(field.Default)
		}
		if field.Description != "" {
			projected["description"] = field.Description
		}
		properties[field.Name] = projected
		if field.Required {
			required = append(required, field.Name)
		}
	}
	result := map[string]any{"type": "object", "properties": properties, "additionalProperties": value.Open}
	if len(required) != 0 {
		result["required"] = required
	}
	if len(value.VariantMembers) != 0 {
		variants, err := projectVariants(ctx, value.VariantMembers, types)
		if err != nil {
			return nil, err
		}
		result["oneOf"] = variants
	}
	return result, nil
}

func projectVariants(ctx context.Context, members []schema.VariantMemberDescriptor, types map[schema.TypeID]schema.TypeDeclaration) ([]any, error) {
	result := make([]any, 0, len(members))
	for _, member := range members {
		if err := projectionContext(ctx); err != nil {
			return nil, err
		}
		projected, err := projectReference(member.Type, false, types)
		if err != nil {
			return nil, err
		}
		result = append(result, projected)
	}
	return result, nil
}

func projectCustomScalar(ctx context.Context, value schema.TypeDeclaration) (map[string]any, error) {
	if value.Scalar == nil || len(value.Scalar.AcceptedWireShapes) == 0 {
		return nil, publicError("ASYNCAPI_SCHEMA_PROJECTION_UNSUPPORTED", errors.New("custom scalar has no portable wire shape"))
	}
	shapes := make([]any, 0, len(value.Scalar.AcceptedWireShapes))
	for _, shape := range value.Scalar.AcceptedWireShapes {
		if err := projectionContext(ctx); err != nil {
			return nil, err
		}
		shapes = append(shapes, map[string]any{"type": string(shape)})
	}
	return map[string]any{"anyOf": shapes, "x-naatre-canonical-profile": value.Scalar.CanonicalProfile}, nil
}

func projectReference(id schema.TypeID, nullable bool, types map[schema.TypeID]schema.TypeDeclaration) (map[string]any, error) {
	var result map[string]any
	if scalar := coreScalarSchema(id); scalar != nil {
		result = scalar
	} else if _, ok := types[id]; ok {
		result = map[string]any{"$ref": "#/components/schemas/" + string(id)}
	} else {
		return nil, publicError("ASYNCAPI_SCHEMA_PROJECTION_UNSUPPORTED", errors.New("schema reference is unresolved"))
	}
	if !nullable {
		return result, nil
	}
	return map[string]any{"anyOf": []any{result, map[string]any{"type": "null"}}}, nil
}

func coreScalarSchema(id schema.TypeID) map[string]any {
	switch id {
	case schema.TypeID(schema.Boolean):
		return map[string]any{"type": "boolean"}
	case schema.TypeID(schema.String):
		return map[string]any{"type": "string"}
	case schema.TypeID(schema.ID):
		return map[string]any{"type": "string", "format": "naatre-id"}
	case schema.TypeID(schema.Int32):
		return map[string]any{"type": "integer", "format": "int32", "minimum": -2147483648, "maximum": 2147483647}
	case schema.TypeID(schema.Float64):
		return map[string]any{"type": "number", "format": "double"}
	case schema.TypeID(schema.Int64), schema.TypeID(schema.UInt64), schema.TypeID(schema.BigInt), schema.TypeID(schema.Decimal):
		return map[string]any{"type": "string", "format": "naatre-" + string(id)}
	case schema.TypeID(schema.Timestamp), schema.TypeID(schema.Duration), schema.TypeID(schema.UUID), schema.TypeID(schema.Bytes):
		return map[string]any{"type": "string", "format": "naatre-" + string(id)}
	default:
		return nil
	}
}

var coreScalarIDs = []schema.TypeID{
	schema.TypeID(schema.Boolean), schema.TypeID(schema.String), schema.TypeID(schema.ID), schema.TypeID(schema.Int32), schema.TypeID(schema.Float64),
	schema.TypeID(schema.Int64), schema.TypeID(schema.UInt64), schema.TypeID(schema.BigInt), schema.TypeID(schema.Decimal), schema.TypeID(schema.Timestamp),
	schema.TypeID(schema.Duration), schema.TypeID(schema.UUID), schema.TypeID(schema.Bytes),
}

func installProjectedSchemas(root map[string]json.RawMessage, projected map[string]any) error {
	var components map[string]json.RawMessage
	if json.Unmarshal(root["components"], &components) != nil {
		return errors.New("components are unavailable")
	}
	setRaw(components, "schemas", projected)
	var messages map[string]map[string]any
	if json.Unmarshal(components["messages"], &messages) != nil {
		return errors.New("messages are unavailable")
	}
	for id := range messages {
		messages[id]["schemaFormat"] = ProjectionJSONDialect
	}
	setRaw(components, "messages", messages)
	setRaw(root, "components", components)
	return nil
}

func readProjectionHeader(root map[string]json.RawMessage) (projectionExtension, Revisions, error) {
	var profile, exporter string
	var revisions Revisions
	if json.Unmarshal(root["x-naatre-profile"], &profile) != nil || json.Unmarshal(root["x-naatre-exporter"], &exporter) != nil || json.Unmarshal(root["x-naatre-revisions"], &revisions) != nil {
		return projectionExtension{}, revisions, publicError("ASYNCAPI_PROJECTION_UNSUPPORTED", errors.New("projection identity is missing"))
	}
	var extension projectionExtension
	decoder := json.NewDecoder(bytes.NewReader(root["x-naatre-projection"]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&extension) != nil || profile != ProjectionProfile || exporter != ProjectionExporterVersion ||
		revisions.AsyncAPIProfile != ProjectionProfile || revisions.Exporter != ProjectionExporterVersion ||
		extension.Profile != ProjectionProfile || extension.Version != ProjectionVersion || extension.JSONDialect != ProjectionJSONDialect ||
		extension.CoreProfile != Profile || extension.CoreExporter != ExporterVersion || extension.Authority != "caller-authorized-naatre-schema" || extension.HandlerCalls != 0 || len(extension.SchemaDocument) == 0 {
		return projectionExtension{}, revisions, publicError("ASYNCAPI_PROJECTION_UNSUPPORTED", errors.New("projection header is outside the supported profile"))
	}
	return extension, revisions, nil
}

func restoreCoreDocument(root map[string]json.RawMessage, revisions Revisions) ([]byte, error) {
	delete(root, "x-naatre-projection")
	revisions.AsyncAPIProfile = Profile
	revisions.Exporter = ExporterVersion
	setRaw(root, "x-naatre-profile", Profile)
	setRaw(root, "x-naatre-exporter", ExporterVersion)
	setRaw(root, "x-naatre-revisions", revisions)
	setRaw(root, "x-naatre-fidelity", fidelityMappings())

	var components map[string]json.RawMessage
	if json.Unmarshal(root["components"], &components) != nil {
		return nil, publicError("ASYNCAPI_DOCUMENT_INVALID", errors.New("components are unavailable"))
	}
	var messages map[string]wireMessage
	if json.Unmarshal(components["messages"], &messages) != nil {
		return nil, publicError("ASYNCAPI_DOCUMENT_INVALID", errors.New("messages are unavailable"))
	}
	schemas := make(map[string]wireSchema, len(messages))
	for id, message := range messages {
		payload, err := localReference(message.Payload.Ref, "#/components/schemas/")
		if err != nil {
			return nil, err
		}
		message.SchemaFormat = SchemaFormat
		messages[id] = message
		schemas[payload] = wireSchema{
			Type: "object", AdditionalProperties: true, NaatreType: payload,
			Identity: identity("schema", payload, revisions.Schema, revisions),
		}
	}
	setRaw(components, "messages", messages)
	setRaw(components, "schemas", schemas)
	setRaw(root, "components", components)
	return json.Marshal(root)
}

func projectionMappings() []interopadapter.Mapping {
	values := fidelityMappings()
	values = append(values, interopadapter.Mapping{Feature: "payload-schema-projection", Classification: interopadapter.Lossless})
	sort.Slice(values, func(i, j int) bool { return values[i].Feature < values[j].Feature })
	return values
}

func newProjectionReport(direction interopadapter.Direction, revisions Revisions) FidelityReport {
	return FidelityReport{
		Profile: ProjectionProfile, ExporterVersion: ProjectionExporterVersion, Specification: Specification,
		Direction: direction, Status: "rejected", Revisions: revisions, Mappings: projectionMappings(),
		Links: []IdentityLink{}, Diagnostics: []Diagnostic{},
	}
}

func projectionExportFailure(report FidelityReport, code, feature string) ([]byte, FidelityReport, error) {
	report.Status = "rejected"
	report.Diagnostics = []Diagnostic{{Code: code, Feature: feature, Message: "AsyncAPI projection export rejected unsafe, lossy, or unsupported metadata"}}
	return nil, report, publicError(code, errors.New("projection export rejected"))
}

func projectionImportFailure(report FidelityReport, code, feature string) (Model, FidelityReport, error) {
	report.Status = "rejected"
	report.Diagnostics = []Diagnostic{{Code: code, Feature: feature, Message: "AsyncAPI projection import rejected unsafe, lossy, or unsupported metadata"}}
	return Model{}, report, publicError(code, errors.New("projection import rejected"))
}

func projectionContext(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return publicError("ASYNCAPI_PROJECTION_CANCELLED", errors.New("projection context is unavailable"))
	}
	return nil
}

func setRaw(values map[string]json.RawMessage, key string, value any) {
	encoded, _ := json.Marshal(value)
	values[key] = encoded
}
