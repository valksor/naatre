package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/valksor/naatre/protocol"
)

const (
	JTDRFC8927                 = "https://www.rfc-editor.org/rfc/rfc8927"
	JTDMapperRevision          = "naatre-jtd-mapper-1"
	JTDFidelityProfileRevision = "naatre-jtd-fidelity-1"
	JTDMetadataNamespace       = "https://naatre.dev/jtd/v1"
	JSONSchemaMapperRevision   = "naatre-json-schema-constraints-1"
	JSONSchemaFidelityRevision = "naatre-json-schema-fidelity-1"
)

type JTDOutcome string

const (
	JTDLossless    JTDOutcome = "lossless"
	JTDUnsupported JTDOutcome = "unsupported"
	JTDLossy       JTDOutcome = "lossy"
)

type JTDDiagnostic struct {
	Code    string     `json:"code"`
	Pointer string     `json:"pointer"`
	Feature string     `json:"feature,omitempty"`
	Outcome JTDOutcome `json:"outcome"`
	Message string     `json:"message"`
}

type JTDMappingOutcome struct {
	Feature string     `json:"feature"`
	Outcome JTDOutcome `json:"outcome"`
	Method  string     `json:"method"`
}

// SchemaMappingBinding prevents independent projections from silently using
// different canonical Naatre documents.
type SchemaMappingBinding struct {
	Format                  string `json:"format"`
	SpecificationRevision   string `json:"specificationRevision"`
	MapperRevision          string `json:"mapperRevision"`
	FidelityProfileRevision string `json:"fidelityProfileRevision"`
	NaatreRevision          string `json:"naatreRevision"`
	NaatreSchemaHash        string `json:"naatreSchemaHash"`
}

type JTDFidelityReport struct {
	Direction   string               `json:"direction"`
	Exact       bool                 `json:"exact"`
	Root        TypeID               `json:"root,omitempty"`
	Binding     SchemaMappingBinding `json:"binding"`
	Mappings    []JTDMappingOutcome  `json:"mappings"`
	Diagnostics []JTDDiagnostic      `json:"diagnostics"`
}

type JSONSchemaDocumentFidelityReport struct {
	Exact       bool                   `json:"exact"`
	Binding     SchemaMappingBinding   `json:"binding"`
	Diagnostics []JSONSchemaDiagnostic `json:"diagnostics"`
}

type JTDError struct {
	diagnostics []JTDDiagnostic
}

func (e *JTDError) Error() string { return "JTD mapping is not faithful" }

func (e *JTDError) Diagnostics() []JTDDiagnostic { return slices.Clone(e.diagnostics) }

type SchemaMappingError struct {
	Code string
}

func (e *SchemaMappingError) Error() string {
	return "schema projections do not share one canonical Naatre source"
}

type JTDExportOptions struct {
	Root TypeID
}

type JTDImportOptions struct {
	Approved              bool
	UseEmbeddedIdentities bool
	Identities            map[string]TypeID
	FieldIdentities       map[string]string
	MemberIdentities      map[string]string
	Revision              string
	DefaultInput          bool
	DefaultOutput         bool
	MaxBytes              int
	MaxDepth              int
	MaxDefinitions        int
	MaxProperties         int
	MaxMetadataBytes      int
	MaxIdentifierBytes    int
	MaxRecursion          int
}

type jtdSchema struct {
	Definitions          map[string]jtdSchema       `json:"definitions,omitempty"`
	Metadata             map[string]json.RawMessage `json:"metadata,omitempty"`
	Nullable             bool                       `json:"nullable,omitempty"`
	Ref                  string                     `json:"ref,omitempty"`
	Type                 string                     `json:"type,omitempty"`
	Enum                 []string                   `json:"enum,omitempty"`
	Elements             *jtdSchema                 `json:"elements,omitempty"`
	Properties           map[string]jtdSchema       `json:"properties,omitempty"`
	OptionalProperties   map[string]jtdSchema       `json:"optionalProperties,omitempty"`
	AdditionalProperties bool                       `json:"additionalProperties,omitempty"`
	Values               *jtdSchema                 `json:"values,omitempty"`
	Discriminator        string                     `json:"discriminator,omitempty"`
	Mapping              map[string]jtdSchema       `json:"mapping,omitempty"`
}

type jtdMetadata struct {
	SchemaRevision string            `json:"schemaRevision,omitempty"`
	SchemaHash     string            `json:"schemaHash,omitempty"`
	MapperRevision string            `json:"mapperRevision,omitempty"`
	RFCRevision    string            `json:"rfcRevision,omitempty"`
	Fidelity       string            `json:"fidelityProfileRevision,omitempty"`
	Root           TypeID            `json:"root,omitempty"`
	TypeID         TypeID            `json:"typeId,omitempty"`
	TypeName       string            `json:"typeName,omitempty"`
	Kind           TypeKind          `json:"kind,omitempty"`
	Input          bool              `json:"input,omitempty"`
	Output         bool              `json:"output,omitempty"`
	Scalar         TypeID            `json:"scalar,omitempty"`
	FieldID        string            `json:"fieldId,omitempty"`
	MemberIDs      map[string]string `json:"memberIds,omitempty"`
	MaxDepth       int               `json:"maxDepth,omitempty"`
	Description    string            `json:"description,omitempty"`
	Deprecation    *Deprecation      `json:"deprecation,omitempty"`
	Source         *SourceMetadata   `json:"source,omitempty"`
}

// ExportJTD projects the lossless RFC 8927 subset. It never mutates the
// canonical document and rejects required semantics that JTD cannot carry.
func ExportJTD(document Document, options JTDExportOptions) ([]byte, JTDFidelityReport, error) {
	binding, err := mappingBinding(document, "jtd", JTDRFC8927, JTDMapperRevision, JTDFidelityProfileRevision)
	report := newJTDReport("schema-export", binding)
	report.Root = options.Root
	if err != nil {
		return failJTDExport(report, diagnostic("JTD_INVALID_SOURCE", "", "canonical-schema", JTDUnsupported, "canonical Naatre schema is not initialized"))
	}
	if options.Root == "" {
		return failJTDExport(report, diagnostic("JTD_ROOT_REQUIRED", "", "root", JTDUnsupported, "a root type identity is required"))
	}
	if diagnostics := unsupportedDocumentSemantics(document); len(diagnostics) != 0 {
		return failJTDExport(report, diagnostics...)
	}
	declarations := document.Types()
	byID := make(map[TypeID]TypeDeclaration, len(declarations))
	for _, declaration := range declarations {
		byID[declaration.ID] = declaration
	}
	if _, ok := byID[options.Root]; !ok {
		return failJTDExport(report, diagnostic("JTD_ROOT_UNKNOWN", "", "root", JTDUnsupported, "root type must be a declared reusable definition"))
	}
	definitions := make(map[string]jtdSchema, len(declarations))
	for _, declaration := range declarations {
		mapped, diagnostics := exportJTDDeclaration(declaration)
		if len(diagnostics) != 0 {
			return failJTDExport(report, diagnostics...)
		}
		definitions[string(declaration.ID)] = mapped
	}
	root, diagnostics := exportJTDReference(options.Root, false, "")
	if len(diagnostics) != 0 {
		return failJTDExport(report, diagnostics...)
	}
	root.Definitions = definitions
	root.Metadata = encodeJTDMetadata(jtdMetadata{
		SchemaRevision: document.Revision(), SchemaHash: binding.NaatreSchemaHash,
		MapperRevision: JTDMapperRevision, RFCRevision: JTDRFC8927,
		Fidelity: JTDFidelityProfileRevision, Root: options.Root,
	})
	encoded, err := json.Marshal(root)
	if err != nil {
		return failJTDExport(report, diagnostic("JTD_INTERNAL", "", "encoding", JTDUnsupported, "JTD projection could not be encoded"))
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{})
	if err != nil {
		return failJTDExport(report, diagnostic("JTD_INTERNAL", "", "canonicalization", JTDUnsupported, "JTD projection could not be canonicalized"))
	}
	return canonical, report, nil
}

func exportJTDDeclaration(declaration TypeDeclaration) (jtdSchema, []JTDDiagnostic) {
	pointer := "/definitions/" + escapeJSONPointer(string(declaration.ID))
	if diagnostics := unsupportedTypeSemantics(declaration, pointer); len(diagnostics) != 0 {
		return jtdSchema{}, diagnostics
	}
	metadata := jtdMetadata{
		TypeID: declaration.ID, TypeName: declaration.Name, Kind: declaration.Kind,
		Input: declaration.Input, Output: declaration.Output, MaxDepth: declaration.MaxDepth,
		Description: declaration.Description, Deprecation: cloneDeprecation(declaration.Deprecation),
		Source: cloneSource(declaration.Source),
	}
	var result jtdSchema
	switch declaration.Kind {
	case ObjectType, InputObjectType:
		result.Properties = make(map[string]jtdSchema)
		result.OptionalProperties = make(map[string]jtdSchema)
		for _, field := range declaration.Fields {
			fieldPointer := pointer + "/properties/" + escapeJSONPointer(field.Name)
			if diagnostics := unsupportedFieldSemantics(field, fieldPointer); len(diagnostics) != 0 {
				return jtdSchema{}, diagnostics
			}
			mapped, diagnostics := exportJTDReference(field.Type, field.Nullable, field.ID)
			if len(diagnostics) != 0 {
				return jtdSchema{}, prefixJTDDiagnostics(diagnostics, fieldPointer)
			}
			fieldMetadata := readEncodedMetadata(mapped.Metadata)
			fieldMetadata.Description = field.Description
			fieldMetadata.Deprecation = cloneDeprecation(field.Deprecation)
			fieldMetadata.Source = cloneSource(field.Source)
			mapped.Metadata = encodeJTDMetadata(fieldMetadata)
			if field.Required {
				result.Properties[field.Name] = mapped
			} else {
				result.OptionalProperties[field.Name] = mapped
			}
		}
	case ListType:
		mapped, diagnostics := exportJTDReference(declaration.Element, declaration.ElementNullable, "")
		if len(diagnostics) != 0 {
			return jtdSchema{}, prefixJTDDiagnostics(diagnostics, pointer+"/elements")
		}
		result.Elements = &mapped
	case MapType:
		mapped, diagnostics := exportJTDReference(declaration.Element, declaration.ElementNullable, "")
		if len(diagnostics) != 0 {
			return jtdSchema{}, prefixJTDDiagnostics(diagnostics, pointer+"/values")
		}
		result.Values = &mapped
	case EnumType:
		result.Enum = make([]string, 0, len(declaration.EnumMembers))
		metadata.MemberIDs = make(map[string]string, len(declaration.EnumMembers))
		for _, member := range declaration.EnumMembers {
			if member.Description != "" || member.Deprecation != nil || len(member.Traits) != 0 || member.Source != nil {
				return jtdSchema{}, []JTDDiagnostic{diagnostic("JTD_ENUM_MEMBER_METADATA_UNSUPPORTED", pointer+"/enum", "enum-member-metadata", JTDUnsupported, "enum member metadata cannot round-trip through JTD enum values")}
			}
			result.Enum = append(result.Enum, member.Name)
			metadata.MemberIDs[member.Name] = member.ID
		}
	case UnionType:
		result.Discriminator = "$type"
		result.Mapping = make(map[string]jtdSchema, len(declaration.VariantMembers))
		metadata.MemberIDs = make(map[string]string, len(declaration.VariantMembers))
		for _, member := range declaration.VariantMembers {
			if member.Description != "" || member.Deprecation != nil || len(member.Traits) != 0 || member.Source != nil {
				return jtdSchema{}, []JTDDiagnostic{diagnostic("JTD_VARIANT_METADATA_UNSUPPORTED", pointer+"/mapping", "variant-metadata", JTDUnsupported, "variant metadata cannot round-trip through JTD mappings")}
			}
			value, diagnostics := exportJTDReference(member.Type, false, "")
			if len(diagnostics) != 0 {
				return jtdSchema{}, diagnostics
			}
			result.Mapping[string(member.Type)] = jtdSchema{Properties: map[string]jtdSchema{"$value": value}}
			metadata.MemberIDs[string(member.Type)] = member.ID
		}
	case ScalarType, InterfaceType, OneOfType:
		return jtdSchema{}, []JTDDiagnostic{diagnostic("JTD_TYPE_UNSUPPORTED", pointer, "type-form", JTDUnsupported, "Naatre type form is outside the JTD lossless subset")}
	default:
		return jtdSchema{}, []JTDDiagnostic{diagnostic("JTD_TYPE_UNSUPPORTED", pointer, "type-form", JTDUnsupported, "Naatre type form is outside the JTD lossless subset")}
	}
	result.Metadata = encodeJTDMetadata(metadata)
	return result, nil
}

func exportJTDReference(id TypeID, nullable bool, fieldID string) (jtdSchema, []JTDDiagnostic) {
	metadata := jtdMetadata{FieldID: fieldID}
	if scalar, ok := jtdScalarType(id); ok {
		metadata.Scalar = id
		return jtdSchema{Type: scalar, Nullable: nullable, Metadata: encodeJTDMetadata(metadata)}, nil
	}
	if builtInScalar(id) {
		return jtdSchema{}, []JTDDiagnostic{diagnostic("JTD_EXTENDED_SCALAR_UNSUPPORTED", "", "extended-scalar", JTDUnsupported, "extended numeric, time, UUID, and byte scalars are not lossless JTD types")}
	}
	return jtdSchema{Ref: string(id), Nullable: nullable, Metadata: encodeJTDMetadata(metadata)}, nil
}

// ImportJTD imports an explicitly approved, bounded JTD document. Embedded
// identities are ignored unless separately approved.
func ImportJTD(input []byte, options JTDImportOptions) (Document, JTDFidelityReport, error) {
	options = defaultJTDImportOptions(options)
	report := newJTDReport("schema-import", SchemaMappingBinding{
		Format: "jtd", SpecificationRevision: JTDRFC8927,
		MapperRevision: JTDMapperRevision, FidelityProfileRevision: JTDFidelityProfileRevision,
	})
	if !options.Approved {
		return failJTDImport(report, diagnostic("JTD_IMPORT_APPROVAL_REQUIRED", "", "approval", JTDUnsupported, "JTD import requires explicit application approval"))
	}
	if len(input) > options.MaxBytes {
		return failJTDImport(report, diagnostic("JTD_LIMIT_BYTES", "", "size", JTDUnsupported, "JTD document exceeds the byte limit"))
	}
	if err := protocol.ValidateJSON(input, protocol.Limits{MaxBytes: options.MaxBytes, MaxDepth: options.MaxDepth, MaxMembers: options.MaxDefinitions * options.MaxProperties, MaxArrayItems: options.MaxDefinitions * options.MaxProperties, MaxStringBytes: options.MaxMetadataBytes, MaxNumberBytes: 64, MaxTokens: options.MaxDefinitions * options.MaxProperties * 8}); err != nil {
		return failJTDImport(report, diagnostic("JTD_INVALID_JSON", "", "document", JTDUnsupported, "JTD document is not strict bounded JSON"))
	}
	var root jtdSchema
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&root); err != nil {
		return failJTDImport(report, diagnostic("JTD_INVALID_FORM", "", "document", JTDUnsupported, "JTD document shape is invalid"))
	}
	if len(root.Definitions) == 0 || len(root.Definitions) > options.MaxDefinitions {
		return failJTDImport(report, diagnostic("JTD_LIMIT_DEFINITIONS", "/definitions", "definitions", JTDUnsupported, "JTD definitions are missing or exceed the limit"))
	}
	rootMetadata, diagnostics := decodeJTDMetadata(root.Metadata, options, "")
	if len(diagnostics) != 0 {
		return failJTDImport(report, diagnostics...)
	}
	rootWithoutDefinitions := root
	rootWithoutDefinitions.Definitions = nil
	rootWithoutDefinitions.Metadata = nil
	if rootWithoutDefinitions.Ref == "" || rootWithoutDefinitions.Nullable || rootWithoutDefinitions.AdditionalProperties || jtdFormCount(rootWithoutDefinitions) != 1 {
		return failJTDImport(report, diagnostic("JTD_ROOT_REF_REQUIRED", "", "root", JTDUnsupported, "JTD schema projection requires one root ref form"))
	}
	if options.UseEmbeddedIdentities && (rootMetadata.MapperRevision != JTDMapperRevision || rootMetadata.RFCRevision != JTDRFC8927 || rootMetadata.Fidelity != JTDFidelityProfileRevision || rootMetadata.SchemaRevision == "" || rootMetadata.SchemaHash == "" || rootMetadata.Root == "") {
		return failJTDImport(report, diagnostic("JTD_METADATA_REVISION", "/metadata", "metadata", JTDUnsupported, "embedded identities require complete pinned root metadata"))
	}
	keyToID := make(map[string]TypeID, len(root.Definitions))
	seenIDs := make(map[TypeID]bool, len(root.Definitions))
	seenNames := make(map[string]bool, len(root.Definitions))
	metadataByKey := make(map[string]jtdMetadata, len(root.Definitions))
	keys := sortedJTDKeys(root.Definitions)
	for _, key := range keys {
		pointer := "/definitions/" + escapeJSONPointer(key)
		if len(key) > options.MaxIdentifierBytes {
			return failJTDImport(report, diagnostic("JTD_LIMIT_IDENTIFIER", pointer, "identifier", JTDUnsupported, "definition identifier exceeds the limit"))
		}
		metadata, metadataDiagnostics := decodeJTDMetadata(root.Definitions[key].Metadata, options, pointer)
		if len(metadataDiagnostics) != 0 {
			return failJTDImport(report, metadataDiagnostics...)
		}
		id, identityDiagnostic := importTypeIdentity(key, metadata, options, pointer)
		if identityDiagnostic != nil {
			return failJTDImport(report, *identityDiagnostic)
		}
		name := metadata.TypeName
		if name == "" {
			name = key
		}
		if seenIDs[id] || seenNames[name] {
			return failJTDImport(report, diagnostic("JTD_DUPLICATE_NAME", pointer, "identity", JTDUnsupported, "definition identities and names must be unique"))
		}
		seenIDs[id], seenNames[name] = true, true
		keyToID[key], metadataByKey[key] = id, metadata
	}
	if _, ok := keyToID[root.Ref]; !ok {
		return failJTDImport(report, diagnostic("JTD_REF_UNKNOWN", "/ref", "reference", JTDUnsupported, "root reference is not defined"))
	}
	if options.UseEmbeddedIdentities && rootMetadata.Root != keyToID[root.Ref] {
		return failJTDImport(report, diagnostic("JTD_ROOT_MISMATCH", "/metadata", "root", JTDUnsupported, "embedded root identity conflicts with the JTD root reference"))
	}
	report.Root = keyToID[root.Ref]
	catalog := NewCatalog()
	for _, key := range keys {
		declaration, importDiagnostics := importJTDDeclaration(key, root.Definitions[key], metadataByKey[key], keyToID, options)
		if len(importDiagnostics) != 0 {
			return failJTDImport(report, importDiagnostics...)
		}
		descriptor := descriptorFromDeclaration(declaration)
		if err := catalog.Register(descriptor); err != nil {
			return failJTDImport(report, diagnostic("JTD_INVALID_NAATRE_TYPE", "/definitions/"+escapeJSONPointer(key), "canonical-type", JTDUnsupported, err.Error()))
		}
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		return failJTDImport(report, diagnostic("JTD_INVALID_NAATRE_GRAPH", "/definitions", "canonical-type-graph", JTDUnsupported, err.Error()))
	}
	revision := options.Revision
	if options.UseEmbeddedIdentities && rootMetadata.SchemaRevision != "" {
		revision = rootMetadata.SchemaRevision
	}
	if revision == "" {
		return failJTDImport(report, diagnostic("JTD_REVISION_REQUIRED", "", "revision", JTDUnsupported, "canonical schema revision requires explicit assignment"))
	}
	document, err := ExportDocument(snapshot, nil, nil, ExportOptions{Revision: revision})
	if err != nil {
		return failJTDImport(report, diagnostic("JTD_INVALID_NAATRE_SCHEMA", "", "canonical-schema", JTDUnsupported, err.Error()))
	}
	binding, err := mappingBinding(document, "jtd", JTDRFC8927, JTDMapperRevision, JTDFidelityProfileRevision)
	if err != nil {
		return failJTDImport(report, diagnostic("JTD_INVALID_NAATRE_SCHEMA", "", "canonical-schema", JTDUnsupported, err.Error()))
	}
	report.Binding = binding
	if options.UseEmbeddedIdentities && rootMetadata.SchemaHash != "" && rootMetadata.SchemaHash != binding.NaatreSchemaHash {
		return failJTDImport(report, diagnostic("JTD_SOURCE_MISMATCH", "/metadata", "canonical-source", JTDUnsupported, "embedded canonical schema hash does not match imported schema"))
	}
	return document, report, nil
}

func importJTDDeclaration(key string, input jtdSchema, metadata jtdMetadata, keyToID map[string]TypeID, options JTDImportOptions) (TypeDeclaration, []JTDDiagnostic) {
	pointer := "/definitions/" + escapeJSONPointer(key)
	if input.Definitions != nil {
		return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_NESTED_DEFINITIONS", pointer+"/definitions", "definitions", JTDUnsupported, "definitions are permitted only at the root")}
	}
	if input.Nullable {
		return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_NULLABLE_DEFINITION_UNSUPPORTED", pointer+"/nullable", "nullable", JTDUnsupported, "named Naatre type definitions cannot be nullable")}
	}
	if input.AdditionalProperties {
		return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_OPEN_PROPERTIES_UNSUPPORTED", pointer+"/additionalProperties", "open-record", JTDUnsupported, "additional properties cannot map to a closed Naatre object")}
	}
	if count := jtdFormCount(input); count != 1 {
		return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_INVALID_FORM", pointer, "form", JTDUnsupported, "exactly one supported JTD form is required")}
	}
	id := keyToID[key]
	declaration := TypeDeclaration{
		ID: id, Name: metadata.TypeName, Input: metadata.Input, Output: metadata.Output,
		MaxDepth: metadata.MaxDepth, Description: metadata.Description,
		Deprecation: cloneDeprecation(metadata.Deprecation), Source: cloneSource(metadata.Source),
	}
	if declaration.Name == "" {
		declaration.Name = key
	}
	if !options.UseEmbeddedIdentities {
		declaration.Input, declaration.Output = options.DefaultInput, options.DefaultOutput
		declaration.MaxDepth = options.MaxRecursion
	}
	if !declaration.Input && !declaration.Output {
		return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_POSITION_REQUIRED", pointer+"/metadata", "input-output-position", JTDUnsupported, "input or output position requires explicit assignment")}
	}
	switch {
	case input.Properties != nil || input.OptionalProperties != nil:
		declaration.Kind = metadata.Kind
		if declaration.Kind == "" {
			declaration.Kind = ObjectType
		}
		if declaration.Kind != ObjectType && declaration.Kind != InputObjectType {
			return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_KIND_MISMATCH", pointer+"/metadata", "type-kind", JTDUnsupported, "record metadata requires an object type kind")}
		}
		if len(input.Properties)+len(input.OptionalProperties) > options.MaxProperties {
			return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_LIMIT_PROPERTIES", pointer, "properties", JTDUnsupported, "record properties exceed the limit")}
		}
		for name := range input.Properties {
			if _, duplicate := input.OptionalProperties[name]; duplicate {
				return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_DUPLICATE_PROPERTY", pointer, "property", JTDUnsupported, "property cannot be both required and optional")}
			}
		}
		fieldNames := append(sortedJTDKeys(input.Properties), sortedJTDKeys(input.OptionalProperties)...)
		for _, name := range fieldNames {
			fieldSchema, required := input.Properties[name]
			if !required {
				fieldSchema = input.OptionalProperties[name]
			}
			fieldPointer := pointer + "/properties/" + escapeJSONPointer(name)
			fieldType, nullable, fieldMetadata, diagnostics := importJTDReference(fieldSchema, keyToID, options, fieldPointer)
			if len(diagnostics) != 0 {
				return TypeDeclaration{}, diagnostics
			}
			fieldID, identityDiagnostic := importMemberIdentity(key+"/"+name, fieldMetadata.FieldID, options.FieldIdentities, options, fieldPointer)
			if identityDiagnostic != nil {
				return TypeDeclaration{}, []JTDDiagnostic{*identityDiagnostic}
			}
			declaration.Fields = append(declaration.Fields, FieldDeclaration{
				ID: fieldID, Name: name, Type: fieldType, Required: required, Nullable: nullable,
				Description: fieldMetadata.Description, Deprecation: cloneDeprecation(fieldMetadata.Deprecation), Source: cloneSource(fieldMetadata.Source),
			})
		}
	case input.Elements != nil:
		declaration.Kind = ListType
		element, nullable, _, diagnostics := importJTDReference(*input.Elements, keyToID, options, pointer+"/elements")
		if len(diagnostics) != 0 {
			return TypeDeclaration{}, diagnostics
		}
		declaration.Element, declaration.ElementNullable = element, nullable
	case input.Values != nil:
		declaration.Kind = MapType
		element, nullable, _, diagnostics := importJTDReference(*input.Values, keyToID, options, pointer+"/values")
		if len(diagnostics) != 0 {
			return TypeDeclaration{}, diagnostics
		}
		declaration.Element, declaration.ElementNullable = element, nullable
	case input.Enum != nil:
		declaration.Kind = EnumType
		seen := make(map[string]bool, len(input.Enum))
		for index, name := range input.Enum {
			memberPointer := fmt.Sprintf("%s/enum/%d", pointer, index)
			if name == "" || seen[name] {
				return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_DUPLICATE_ENUM", memberPointer, "enum", JTDUnsupported, "enum values must be non-empty and unique")}
			}
			seen[name] = true
			embedded := metadata.MemberIDs[name]
			memberID, identityDiagnostic := importMemberIdentity(key+"/"+name, embedded, options.MemberIdentities, options, memberPointer)
			if identityDiagnostic != nil {
				return TypeDeclaration{}, []JTDDiagnostic{*identityDiagnostic}
			}
			declaration.EnumMembers = append(declaration.EnumMembers, EnumMemberDescriptor{ID: memberID, Name: name})
		}
	case input.Discriminator != "" || input.Mapping != nil:
		if input.Discriminator != "$type" || len(input.Mapping) == 0 {
			return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_DISCRIMINATOR_UNSUPPORTED", pointer, "discriminator", JTDUnsupported, "Naatre tagged unions require $type and a non-empty mapping")}
		}
		declaration.Kind = UnionType
		for _, tag := range sortedJTDKeys(input.Mapping) {
			variantSchema := input.Mapping[tag]
			if variantSchema.Properties == nil || len(variantSchema.Properties) != 1 || variantSchema.OptionalProperties != nil || variantSchema.Properties["$value"].Ref == "" || jtdFormCount(variantSchema) != 1 {
				return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_DISCRIMINATOR_SHAPE", pointer+"/mapping/"+escapeJSONPointer(tag), "discriminator", JTDUnsupported, "union mapping must contain one required $value reference")}
			}
			variant, _, _, diagnostics := importJTDReference(variantSchema.Properties["$value"], keyToID, options, pointer+"/mapping/"+escapeJSONPointer(tag)+"/properties/$value")
			if len(diagnostics) != 0 {
				return TypeDeclaration{}, diagnostics
			}
			if tag != string(variant) {
				return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_DISCRIMINATOR_TAG", pointer+"/mapping/"+escapeJSONPointer(tag), "discriminator", JTDUnsupported, "union tag must equal the stable variant type identity")}
			}
			memberID, identityDiagnostic := importMemberIdentity(key+"/"+tag, metadata.MemberIDs[tag], options.MemberIdentities, options, pointer+"/mapping/"+escapeJSONPointer(tag))
			if identityDiagnostic != nil {
				return TypeDeclaration{}, []JTDDiagnostic{*identityDiagnostic}
			}
			declaration.VariantMembers = append(declaration.VariantMembers, VariantMemberDescriptor{ID: memberID, Type: variant})
		}
	default:
		return TypeDeclaration{}, []JTDDiagnostic{diagnostic("JTD_DEFINITION_FORM_UNSUPPORTED", pointer, "form", JTDUnsupported, "definition form cannot become a named Naatre type")}
	}
	return declaration, nil
}

func importJTDReference(input jtdSchema, keyToID map[string]TypeID, options JTDImportOptions, pointer string) (TypeID, bool, jtdMetadata, []JTDDiagnostic) {
	if input.Definitions != nil || input.Properties != nil || input.OptionalProperties != nil || input.Elements != nil || input.Values != nil || input.Enum != nil || input.Discriminator != "" || input.Mapping != nil || input.AdditionalProperties {
		return "", false, jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_INLINE_FORM_UNSUPPORTED", pointer, "inline-form", JTDUnsupported, "fields and elements require scalar or definition reference forms")}
	}
	if jtdFormCount(input) != 1 {
		return "", false, jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_INVALID_FORM", pointer, "form", JTDUnsupported, "exactly one scalar or reference form is required")}
	}
	metadata, diagnostics := decodeJTDMetadata(input.Metadata, options, pointer)
	if len(diagnostics) != 0 {
		return "", false, jtdMetadata{}, diagnostics
	}
	if input.Ref != "" {
		id, ok := keyToID[input.Ref]
		if !ok {
			return "", false, jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_REF_UNKNOWN", pointer+"/ref", "reference", JTDUnsupported, "reference is not in root definitions")}
		}
		return id, input.Nullable, metadata, nil
	}
	id, ok := naatreScalarType(input.Type)
	if !ok {
		return "", false, jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_SCALAR_UNSUPPORTED", pointer+"/type", "scalar", JTDUnsupported, "JTD scalar is outside the Naatre lossless subset")}
	}
	if options.UseEmbeddedIdentities && metadata.Scalar != "" {
		if mapped, valid := jtdScalarType(metadata.Scalar); !valid || mapped != input.Type {
			return "", false, jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_SCALAR_METADATA_MISMATCH", pointer+"/metadata", "scalar", JTDUnsupported, "embedded scalar identity conflicts with JTD type")}
		}
		id = metadata.Scalar
	}
	return id, input.Nullable, metadata, nil
}

func unsupportedDocumentSemantics(document Document) []JTDDiagnostic {
	options := document.ExportOptions()
	var diagnostics []JTDDiagnostic
	checks := []struct {
		present                         bool
		code, pointer, feature, message string
	}{
		{len(document.Operations()) != 0, "JTD_OPERATION_METADATA_UNSUPPORTED", "/operations", "operation-metadata", "operations, effects, costs, and authorization cannot be represented by JTD"},
		{len(document.Members()) != 0, "JTD_MEMBER_METADATA_UNSUPPORTED", "/members", "operation-metadata", "member execution metadata cannot be represented by JTD"},
		{len(options.Capabilities) != 0, "JTD_CAPABILITIES_UNSUPPORTED", "/capabilities", "capabilities", "capabilities cannot be represented by JTD"},
		{len(options.Directives) != 0, "JTD_DIRECTIVES_UNSUPPORTED", "/directives", "operation-metadata", "directives cannot be represented by JTD"},
		{len(options.Extensions) != 0, "JTD_EXTENSIONS_UNSUPPORTED", "/extensions", "operation-metadata", "extensions cannot be represented by JTD"},
		{len(options.Retired) != 0, "JTD_RETIRED_IDENTITIES_UNSUPPORTED", "/retired", "identity", "retired identities cannot be represented by JTD"},
		{len(options.References) != 0, "JTD_EXTERNAL_REFERENCES_UNSUPPORTED", "/references", "reference", "external schema references cannot be represented by JTD"},
		{len(options.Traits) != 0, "JTD_SCHEMA_TRAITS_UNSUPPORTED", "/traits", "traits", "schema traits cannot be represented by JTD"},
	}
	for _, check := range checks {
		if check.present {
			diagnostics = append(diagnostics, diagnostic(check.code, check.pointer, check.feature, JTDUnsupported, check.message))
		}
	}
	return diagnostics
}

func unsupportedTypeSemantics(declaration TypeDeclaration, pointer string) []JTDDiagnostic {
	var diagnostics []JTDDiagnostic
	appendUnsupported := func(condition bool, code, suffix, feature, message string) {
		if condition {
			diagnostics = append(diagnostics, diagnostic(code, pointer+suffix, feature, JTDUnsupported, message))
		}
	}
	appendUnsupported(declaration.Kind == ScalarType, "JTD_CUSTOM_SCALAR_UNSUPPORTED", "", "custom-scalar", "custom scalar validators and canonicalizers cannot be represented by JTD")
	appendUnsupported(declaration.Kind == InterfaceType, "JTD_INTERFACE_UNSUPPORTED", "", "interface", "interfaces cannot be represented by JTD")
	appendUnsupported(declaration.Kind == OneOfType, "JTD_ONEOF_UNSUPPORTED", "", "one-of-input", "one-of input semantics cannot be represented by JTD")
	appendUnsupported(declaration.Open, "JTD_OPEN_VARIANT_UNSUPPORTED", "/open", "open-variant", "open enums and unions cannot be represented by JTD")
	appendUnsupported(declaration.Entity != nil, "JTD_ENTITY_UNSUPPORTED", "/entity", "identity", "entity identity semantics cannot be represented by JTD")
	appendUnsupported(len(declaration.Capabilities) != 0, "JTD_CAPABILITIES_UNSUPPORTED", "/capabilities", "capabilities", "type capabilities cannot be represented by JTD")
	appendUnsupported(len(declaration.Traits) != 0, "JTD_TYPE_TRAITS_UNSUPPORTED", "/traits", "validation-constraints", "type traits and validation constraints cannot be represented by JTD")
	appendUnsupported(len(declaration.Retired) != 0, "JTD_RETIRED_IDENTITIES_UNSUPPORTED", "/retired", "identity", "retired identities cannot be represented by JTD")
	return diagnostics
}

func unsupportedFieldSemantics(field FieldDeclaration, pointer string) []JTDDiagnostic {
	var diagnostics []JTDDiagnostic
	if len(field.Default) != 0 {
		diagnostics = append(diagnostics, diagnostic("JTD_DEFAULT_UNSUPPORTED", pointer+"/default", "missing-null", JTDUnsupported, "JTD cannot distinguish a missing default from value semantics"))
	}
	if field.Cost != 0 {
		diagnostics = append(diagnostics, diagnostic("JTD_COST_UNSUPPORTED", pointer+"/cost", "cost", JTDUnsupported, "field cost cannot be represented by JTD"))
	}
	if len(field.Traits) != 0 {
		diagnostics = append(diagnostics, diagnostic("JTD_FIELD_TRAITS_UNSUPPORTED", pointer+"/traits", "validation-constraints", JTDUnsupported, "field traits and validation constraints cannot be represented by JTD"))
	}
	return diagnostics
}

func JSONSchemaMappingBinding(document Document) (SchemaMappingBinding, error) {
	return mappingBinding(document, "json-schema", JSONSchema202012, JSONSchemaMapperRevision, JSONSchemaFidelityRevision)
}

func BindJSONSchemaFidelity(document Document, fidelity JSONSchemaFidelity) (JSONSchemaDocumentFidelityReport, error) {
	binding, err := JSONSchemaMappingBinding(document)
	if err != nil {
		return JSONSchemaDocumentFidelityReport{}, err
	}
	return JSONSchemaDocumentFidelityReport{
		Exact: fidelity.Exact, Binding: binding, Diagnostics: slices.Clone(fidelity.Diagnostics),
	}, nil
}

func CompareSchemaMappingBindings(left, right SchemaMappingBinding) error {
	if left.NaatreSchemaHash == "" || right.NaatreSchemaHash == "" || left.NaatreSchemaHash != right.NaatreSchemaHash || left.NaatreRevision != right.NaatreRevision {
		return &SchemaMappingError{Code: "SCHEMA_MAPPING_SOURCE_MISMATCH"}
	}
	return nil
}

func mappingBinding(document Document, format, specification, mapper, fidelity string) (SchemaMappingBinding, error) {
	digest, err := document.Hash()
	if err != nil {
		return SchemaMappingBinding{}, err
	}
	return SchemaMappingBinding{
		Format: format, SpecificationRevision: specification, MapperRevision: mapper,
		FidelityProfileRevision: fidelity, NaatreRevision: document.Revision(), NaatreSchemaHash: digest.Hex,
	}, nil
}

func newJTDReport(direction string, binding SchemaMappingBinding) JTDFidelityReport {
	return JTDFidelityReport{Direction: direction, Exact: true, Binding: binding, Mappings: []JTDMappingOutcome{
		{Feature: "primitive-scalars", Outcome: JTDLossless, Method: "JTD type form with scalar identity metadata"},
		{Feature: "arrays", Outcome: JTDLossless, Method: "elements form"},
		{Feature: "string-keyed-maps", Outcome: JTDLossless, Method: "values form"},
		{Feature: "required-optional-properties", Outcome: JTDLossless, Method: "properties and optionalProperties forms"},
		{Feature: "missing-null", Outcome: JTDLossless, Method: "presence form plus nullable modifier"},
		{Feature: "closed-enums", Outcome: JTDLossless, Method: "enum form"},
		{Feature: "closed-tagged-unions", Outcome: JTDLossless, Method: "$type discriminator with $value record"},
		{Feature: "reusable-definitions", Outcome: JTDLossless, Method: "root definitions and ref forms"},
		{Feature: "recursive-references", Outcome: JTDLossless, Method: "definitions refs with explicit Naatre maximum depth"},
		{Feature: "type-field-deprecations", Outcome: JTDLossless, Method: "Naatre namespaced metadata"},
		{Feature: "enum-variant-member-deprecations", Outcome: JTDUnsupported, Method: "fail closed"},
		{Feature: "open-variants", Outcome: JTDUnsupported, Method: "fail closed"},
		{Feature: "interfaces", Outcome: JTDUnsupported, Method: "fail closed"},
		{Feature: "one-of-input", Outcome: JTDUnsupported, Method: "fail closed"},
		{Feature: "custom-scalars", Outcome: JTDUnsupported, Method: "fail closed"},
		{Feature: "extended-numeric-time", Outcome: JTDUnsupported, Method: "fail closed"},
		{Feature: "validation-constraints", Outcome: JTDUnsupported, Method: "fail closed"},
		{Feature: "operation-metadata-effects-costs-authorization", Outcome: JTDUnsupported, Method: "fail closed"},
		{Feature: "capabilities", Outcome: JTDUnsupported, Method: "fail closed"},
	}}
}

func rejectJTD(report JTDFidelityReport, diagnostics ...JTDDiagnostic) (JTDFidelityReport, error) {
	report.Exact = false
	report.Diagnostics = slices.Clone(diagnostics)
	return report, &JTDError{diagnostics: diagnostics}
}

func failJTDExport(report JTDFidelityReport, diagnostics ...JTDDiagnostic) ([]byte, JTDFidelityReport, error) {
	rejected, err := rejectJTD(report, diagnostics...)
	return nil, rejected, err
}

func failJTDImport(report JTDFidelityReport, diagnostics ...JTDDiagnostic) (Document, JTDFidelityReport, error) {
	rejected, err := rejectJTD(report, diagnostics...)
	return Document{}, rejected, err
}

func diagnostic(code, pointer, feature string, outcome JTDOutcome, message string) JTDDiagnostic {
	return JTDDiagnostic{Code: code, Pointer: pointer, Feature: feature, Outcome: outcome, Message: message}
}

func defaultJTDImportOptions(options JTDImportOptions) JTDImportOptions {
	options.MaxBytes = boundedJTDLimit(options.MaxBytes, 8<<20)
	options.MaxDepth = boundedJTDLimit(options.MaxDepth, 64)
	options.MaxDefinitions = boundedJTDLimit(options.MaxDefinitions, 4096)
	options.MaxProperties = boundedJTDLimit(options.MaxProperties, 4096)
	options.MaxMetadataBytes = boundedJTDLimit(options.MaxMetadataBytes, 64<<10)
	options.MaxIdentifierBytes = boundedJTDLimit(options.MaxIdentifierBytes, 128)
	options.MaxRecursion = boundedJTDLimit(options.MaxRecursion, 32)
	return options
}

func boundedJTDLimit(value, maximum int) int {
	if value <= 0 || value > maximum {
		return maximum
	}
	return value
}

func importTypeIdentity(key string, metadata jtdMetadata, options JTDImportOptions, pointer string) (TypeID, *JTDDiagnostic) {
	var id TypeID
	if options.UseEmbeddedIdentities {
		id = metadata.TypeID
	}
	if id == "" {
		id = options.Identities[key]
	}
	if id == "" {
		d := diagnostic("JTD_IDENTITY_REQUIRED", pointer, "identity", JTDUnsupported, "every definition requires an explicitly approved stable type identity")
		return "", &d
	}
	if len(id) > options.MaxIdentifierBytes || !typeIDPattern.MatchString(string(id)) {
		d := diagnostic("JTD_INVALID_IDENTIFIER", pointer, "identity", JTDUnsupported, "stable type identity is invalid or exceeds the limit")
		return "", &d
	}
	return id, nil
}

func importMemberIdentity(key, embedded string, assigned map[string]string, options JTDImportOptions, pointer string) (string, *JTDDiagnostic) {
	id := ""
	if options.UseEmbeddedIdentities {
		id = embedded
	}
	if id == "" {
		id = assigned[key]
	}
	if id == "" {
		d := diagnostic("JTD_IDENTITY_REQUIRED", pointer, "identity", JTDUnsupported, "every field and variant requires an explicitly approved stable identity")
		return "", &d
	}
	if len(id) > options.MaxIdentifierBytes || !typeIDPattern.MatchString(id) {
		d := diagnostic("JTD_INVALID_IDENTIFIER", pointer, "identity", JTDUnsupported, "stable member identity is invalid or exceeds the limit")
		return "", &d
	}
	return id, nil
}

func encodeJTDMetadata(metadata jtdMetadata) map[string]json.RawMessage {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		panic(err)
	}
	if bytes.Equal(encoded, []byte("{}")) {
		return nil
	}
	return map[string]json.RawMessage{JTDMetadataNamespace: encoded}
}

func readEncodedMetadata(input map[string]json.RawMessage) jtdMetadata {
	var metadata jtdMetadata
	_ = json.Unmarshal(input[JTDMetadataNamespace], &metadata)
	return metadata
}

func decodeJTDMetadata(input map[string]json.RawMessage, options JTDImportOptions, pointer string) (jtdMetadata, []JTDDiagnostic) {
	if len(input) == 0 {
		return jtdMetadata{}, nil
	}
	if len(input) != 1 || input[JTDMetadataNamespace] == nil {
		return jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_METADATA_NAMESPACE", pointer+"/metadata", "metadata", JTDUnsupported, "only the pinned Naatre metadata namespace is accepted")}
	}
	raw := input[JTDMetadataNamespace]
	if len(raw) > options.MaxMetadataBytes {
		return jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_LIMIT_METADATA", pointer+"/metadata", "metadata", JTDUnsupported, "metadata exceeds the byte limit")}
	}
	var metadata jtdMetadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_METADATA_INVALID", pointer+"/metadata", "metadata", JTDUnsupported, "Naatre metadata shape is invalid")}
	}
	if options.UseEmbeddedIdentities && metadata.MapperRevision != "" {
		if metadata.MapperRevision != JTDMapperRevision || metadata.RFCRevision != JTDRFC8927 || metadata.Fidelity != JTDFidelityProfileRevision {
			return jtdMetadata{}, []JTDDiagnostic{diagnostic("JTD_METADATA_REVISION", pointer+"/metadata", "metadata", JTDUnsupported, "metadata revisions do not match the pinned mapper, RFC, and fidelity profile")}
		}
	}
	return metadata, nil
}

func jtdFormCount(input jtdSchema) int {
	count := 0
	if input.Ref != "" {
		count++
	}
	if input.Type != "" {
		count++
	}
	if input.Enum != nil {
		count++
	}
	if input.Elements != nil {
		count++
	}
	if input.Properties != nil || input.OptionalProperties != nil {
		count++
	}
	if input.Values != nil {
		count++
	}
	if input.Discriminator != "" || input.Mapping != nil {
		count++
	}
	return count
}

func jtdScalarType(id TypeID) (string, bool) {
	switch id {
	case TypeID(Boolean):
		return "boolean", true
	case TypeID(String), TypeID(ID):
		return "string", true
	case TypeID(Int32):
		return "int32", true
	case TypeID(Float64):
		return "float64", true
	default:
		return "", false
	}
}

func naatreScalarType(value string) (TypeID, bool) {
	switch value {
	case "boolean":
		return TypeID(Boolean), true
	case "string":
		return TypeID(String), true
	case "int32":
		return TypeID(Int32), true
	case "float64":
		return TypeID(Float64), true
	default:
		return "", false
	}
}

func builtInScalar(id TypeID) bool { return knownScalar(ScalarKind(id)) }

func sortedJTDKeys[V any](input map[string]V) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func prefixJTDDiagnostics(input []JTDDiagnostic, prefix string) []JTDDiagnostic {
	return mapSlice(input, func(value JTDDiagnostic) JTDDiagnostic { value.Pointer = prefix + value.Pointer; return value })
}

func ValidateJTD(input []byte, options JTDImportOptions) (JTDFidelityReport, error) {
	_, report, err := ImportJTD(input, options)
	return report, err
}

func DiffJTD(before, after []byte, options JTDImportOptions) (SchemaDiff, error) {
	left, _, err := ImportJTD(before, options)
	if err != nil {
		return SchemaDiff{}, err
	}
	right, _, err := ImportJTD(after, options)
	if err != nil {
		return SchemaDiff{}, err
	}
	return DiffDocuments(left, right), nil
}
