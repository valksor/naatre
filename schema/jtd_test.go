package schema_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/schema"
)

const losslessSchema = `{
  "version":"1","canonicalVersion":"c14n-1","revision":"jtd-fixture-r1",
  "types":[
    {"id":"Address","name":"Address","kind":"object","output":true,"description":"postal address","fields":[{"id":"Address.city","name":"city","type":"String","required":true},{"id":"Address.unit","name":"unit","type":"Int32","nullable":true}]},
    {"id":"Choice","name":"Choice","kind":"union","output":true,"variantMembers":[{"id":"Choice.Address","type":"Address"},{"id":"Choice.Node","type":"Node"}]},
    {"id":"Color","name":"Color","kind":"enum","input":true,"output":true,"enumMembers":[{"id":"Color.red","name":"red"},{"id":"Color.blue","name":"blue"}]},
    {"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.id","name":"id","type":"ID","required":true},{"id":"Input.note","name":"note","type":"String","nullable":true}]},
    {"id":"Names","name":"Names","kind":"list","input":true,"output":true,"element":"String","elementNullable":true},
    {"id":"Node","name":"Node","kind":"object","output":true,"maxDepth":4,"fields":[{"id":"Node.name","name":"name","type":"String","required":true},{"id":"Node.next","name":"next","type":"Node","nullable":true}]},
    {"id":"Scores","name":"Scores","kind":"map","input":true,"output":true,"element":"Float64"}
  ],
  "operations":[],"members":[]
}`

func TestJTDLosslessSubsetRoundTripsByteIdentically(t *testing.T) {
	t.Parallel()
	document := parseJTDTestSchema(t, losslessSchema)
	first, exported, err := schema.ExportJTD(document, schema.JTDExportOptions{Root: "Node"})
	if err != nil || !exported.Exact || exported.Binding.NaatreSchemaHash == "" {
		t.Fatalf("ExportJTD = %s, %#v, %v", first, exported, err)
	}
	second, secondReport, err := schema.ExportJTD(document, schema.JTDExportOptions{Root: "Node"})
	if err != nil || !secondReport.Exact || !bytes.Equal(first, second) {
		t.Fatalf("JTD export is not deterministic:\n%s\n%s\n%v", first, second, err)
	}
	imported, importedReport, err := schema.ImportJTD(first, schema.JTDImportOptions{Approved: true, UseEmbeddedIdentities: true})
	if err != nil || !importedReport.Exact {
		t.Fatalf("ImportJTD = %#v, %v", importedReport, err)
	}
	want, _ := document.CanonicalJSON()
	got, _ := imported.CanonicalJSON()
	if !bytes.Equal(want, got) {
		t.Fatalf("canonical round trip differs:\n%s\n%s", want, got)
	}
	if importedReport.Binding.NaatreSchemaHash != exported.Binding.NaatreSchemaHash {
		t.Fatalf("schema binding changed: %#v != %#v", importedReport.Binding, exported.Binding)
	}
	var changedRoot map[string]any
	if err := json.Unmarshal(first, &changedRoot); err != nil {
		t.Fatal(err)
	}
	changedRoot["ref"] = "Address"
	tampered, _ := json.Marshal(changedRoot)
	_, rejected, err := schema.ImportJTD(tampered, schema.JTDImportOptions{Approved: true, UseEmbeddedIdentities: true})
	assertJTDDiagnostic(t, nil, rejected, err, "JTD_ROOT_MISMATCH")
}

func TestJTDSupportedFormsAreExplicitAndConsumable(t *testing.T) {
	t.Parallel()
	document := parseJTDTestSchema(t, losslessSchema)
	projection, report, err := schema.ExportJTD(document, schema.JTDExportOptions{Root: "Node"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(projection, &decoded); err != nil {
		t.Fatal(err)
	}
	definitions, ok := decoded["definitions"].(map[string]any)
	if !ok {
		t.Fatalf("third-party definitions unavailable: %#v", decoded)
	}
	forms := map[string]string{
		"Address": "properties", "Input": "properties", "Names": "elements",
		"Scores": "values", "Color": "enum", "Choice": "discriminator", "Node": "properties",
	}
	for name, keyword := range forms {
		definition, ok := definitions[name].(map[string]any)
		if !ok || definition[keyword] == nil {
			t.Errorf("definition %s does not expose JTD %s form: %#v", name, keyword, definition)
		}
	}
	if decoded["ref"] != "Node" || report.Binding.SpecificationRevision != schema.JTDRFC8927 || report.Binding.MapperRevision != schema.JTDMapperRevision || report.Binding.FidelityProfileRevision != schema.JTDFidelityProfileRevision {
		t.Fatalf("projection binding = %#v, root = %#v", report.Binding, decoded["ref"])
	}
	encodedReport, err := json.Marshal(report)
	if err != nil || bytes.Contains(encodedReport, []byte("github.com/valksor/naatre")) {
		t.Fatalf("portable report unexpectedly needs Go runtime details: %s, %v", encodedReport, err)
	}
}

func TestJTDExportFailsClosedForRequiredSemantics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		schema string
		root   schema.TypeID
		code   string
	}{
		{"operation-metadata", `{"version":"1","canonicalVersion":"c14n-1","revision":"jtd-r1","types":[{"id":"Out","name":"Out","kind":"object","output":true,"fields":[{"id":"Out.value","name":"value","type":"String"}]}],"operations":[{"id":"query.out","name":"out","kind":"query","output":"Out","effect":"read","authorizationPolicy":"public"}],"members":[]}`, "Out", "JTD_OPERATION_METADATA_UNSUPPORTED"},
		{"open-enum", `{"version":"1","canonicalVersion":"c14n-1","revision":"jtd-r1","types":[{"id":"Status","name":"Status","kind":"enum","input":true,"output":true,"open":true,"enumMembers":[{"id":"Status.ok","name":"ok"}]}],"operations":[],"members":[]}`, "Status", "JTD_OPEN_VARIANT_UNSUPPORTED"},
		{"constraints", `{"version":"1","canonicalVersion":"c14n-1","revision":"jtd-r1","types":[{"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.name","name":"name","type":"String","traits":[{"id":"naatre.constraints-1","semantics":"validation","value":{"minLength":1}}]}]}],"operations":[],"members":[]}`, "Input", "JTD_FIELD_TRAITS_UNSUPPORTED"},
		{"default", `{"version":"1","canonicalVersion":"c14n-1","revision":"jtd-r1","types":[{"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.name","name":"name","type":"String","default":"safe"}]}],"operations":[],"members":[]}`, "Input", "JTD_DEFAULT_UNSUPPORTED"},
		{"extended-scalar", `{"version":"1","canonicalVersion":"c14n-1","revision":"jtd-r1","types":[{"id":"Event","name":"Event","kind":"object","output":true,"fields":[{"id":"Event.at","name":"at","type":"Timestamp"}]}],"operations":[],"members":[]}`, "Event", "JTD_EXTENDED_SCALAR_UNSUPPORTED"},
		{"custom-scalar", `{"version":"1","canonicalVersion":"c14n-1","revision":"jtd-r1","types":[{"id":"Slug","name":"Slug","kind":"scalar","input":true,"output":true,"scalar":{"acceptedWireShapes":["string"],"validator":"naatre.shape-1","serializer":"naatre.identity-json-1","canonicalizer":"naatre.ascii-lowercase-string.c14n-1","canonicalProfile":"c14n-1","limits":{"maxBytes":1024,"maxDepth":8,"maxMembers":8,"maxArrayItems":8,"maxStringBytes":512,"maxNumberBytes":64,"maxTokens":32},"conformance":[{"input":"Alpha","canonical":"alpha"},{"input":"Beta","canonical":"beta"}]}}],"operations":[],"members":[]}`, "Slug", "JTD_CUSTOM_SCALAR_UNSUPPORTED"},
		{"one-of-input", `{"version":"1","canonicalVersion":"c14n-1","revision":"jtd-r1","types":[{"id":"Choice","name":"Choice","kind":"oneof","input":true,"fields":[{"id":"Choice.name","name":"name","type":"String"},{"id":"Choice.id","name":"id","type":"ID"}]}],"operations":[],"members":[]}`, "Choice", "JTD_ONEOF_UNSUPPORTED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := parseJTDTestSchema(t, test.schema)
			output, report, err := schema.ExportJTD(document, schema.JTDExportOptions{Root: test.root})
			assertJTDDiagnostic(t, output, report, err, test.code)
		})
	}
}

func TestJTDImportRequiresApprovalIdentityAndBoundedValidForms(t *testing.T) {
	t.Parallel()
	valid := []byte(`{"definitions":{"Thing":{"properties":{"name":{"type":"string"}}}},"ref":"Thing"}`)
	_, report, err := schema.ImportJTD(valid, schema.JTDImportOptions{})
	assertJTDDiagnostic(t, nil, report, err, "JTD_IMPORT_APPROVAL_REQUIRED")
	_, report, err = schema.ImportJTD(valid, schema.JTDImportOptions{Approved: true, Revision: "import-r1", DefaultOutput: true})
	assertJTDDiagnostic(t, nil, report, err, "JTD_IDENTITY_REQUIRED")

	options := schema.JTDImportOptions{
		Approved: true, Revision: "import-r1", DefaultOutput: true,
		Identities:      map[string]schema.TypeID{"Thing": "Thing"},
		FieldIdentities: map[string]string{"Thing/name": "Thing.name"},
	}
	document, report, err := schema.ImportJTD(valid, options)
	if err != nil || !report.Exact || document.Revision() != "import-r1" {
		t.Fatalf("approved assigned import = %#v, %v", report, err)
	}

	tests := []struct {
		name, input, code string
		options           schema.JTDImportOptions
	}{
		{"unknown-ref", `{"definitions":{"Thing":{"properties":{"name":{"ref":"Missing"}}}},"ref":"Thing"}`, "JTD_REF_UNKNOWN", options},
		{"duplicate-property", `{"definitions":{"Thing":{"properties":{"name":{"type":"string"}},"optionalProperties":{"name":{"type":"string"}}}},"ref":"Thing"}`, "JTD_DUPLICATE_PROPERTY", options},
		{"nested-definitions", `{"definitions":{"Thing":{"definitions":{"Other":{"type":"string"}},"properties":{"name":{"type":"string"}}}},"ref":"Thing"}`, "JTD_NESTED_DEFINITIONS", options},
		{"malicious-metadata", `{"definitions":{"Thing":{"metadata":{"evil.example":{"payload":"secret"}},"properties":{"name":{"type":"string"}}}},"ref":"Thing"}`, "JTD_METADATA_NAMESPACE", options},
		{"mixed-form", `{"definitions":{"Thing":{"properties":{"name":{"type":"string"}},"values":{"type":"string"}}},"ref":"Thing"}`, "JTD_INVALID_FORM", options},
		{"open-record", `{"definitions":{"Thing":{"properties":{"name":{"type":"string"}},"additionalProperties":true}},"ref":"Thing"}`, "JTD_OPEN_PROPERTIES_UNSUPPORTED", options},
		{"nullable-root", `{"definitions":{"Thing":{"properties":{"name":{"type":"string"}}}},"nullable":true,"ref":"Thing"}`, "JTD_ROOT_REF_REQUIRED", options},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got, err := schema.ImportJTD([]byte(test.input), test.options)
			assertJTDDiagnostic(t, nil, got, err, test.code)
		})
	}
	_, report, err = schema.ImportJTD(valid, schema.JTDImportOptions{Approved: true, Revision: "import-r1", DefaultOutput: true, Identities: options.Identities, FieldIdentities: options.FieldIdentities, MaxDefinitions: 1, MaxProperties: 1, MaxBytes: len(valid) - 1})
	assertJTDDiagnostic(t, nil, report, err, "JTD_LIMIT_BYTES")
	oversizedDefinitions := []byte(`{"definitions":{"One":{"properties":{"value":{"type":"string"}}},"Two":{"properties":{"value":{"type":"string"}}}},"ref":"One"}`)
	_, report, err = schema.ImportJTD(oversizedDefinitions, schema.JTDImportOptions{Approved: true, MaxDefinitions: 1})
	assertJTDDiagnostic(t, nil, report, err, "JTD_LIMIT_DEFINITIONS")
	deep := []byte(`{"definitions":{"Thing":{"properties":{"value":{"elements":{"elements":{"elements":{"type":"string"}}}}}}},"ref":"Thing"}`)
	_, report, err = schema.ImportJTD(deep, schema.JTDImportOptions{Approved: true, MaxDepth: 3})
	assertJTDDiagnostic(t, nil, report, err, "JTD_INVALID_JSON")
	duplicateNames := []byte(`{"definitions":{"One":{"properties":{"value":{"type":"string"}}},"Two":{"properties":{"value":{"type":"string"}}}},"ref":"One"}`)
	_, report, err = schema.ImportJTD(duplicateNames, schema.JTDImportOptions{Approved: true, Identities: map[string]schema.TypeID{"One": "Same", "Two": "Same"}})
	assertJTDDiagnostic(t, nil, report, err, "JTD_DUPLICATE_NAME")
}

func TestJTDRecursiveImportReceivesAnExplicitBound(t *testing.T) {
	t.Parallel()
	recursive := []byte(`{"definitions":{"Node":{"optionalProperties":{"next":{"ref":"Node"}},"properties":{"name":{"type":"string"}}}},"ref":"Node"}`)
	document, report, err := schema.ImportJTD(recursive, schema.JTDImportOptions{
		Approved: true, Revision: "recursive-r1", DefaultOutput: true, MaxRecursion: 5,
		Identities:      map[string]schema.TypeID{"Node": "Node"},
		FieldIdentities: map[string]string{"Node/name": "Node.name", "Node/next": "Node.next"},
	})
	if err != nil || !report.Exact {
		t.Fatalf("recursive import = %#v, %v", report, err)
	}
	declarations := document.Types()
	if len(declarations) != 1 || declarations[0].MaxDepth != 5 {
		t.Fatalf("recursive bound = %#v", declarations)
	}
}

func TestJTDAndJSONSchemaBindingsCannotDisagreeSilently(t *testing.T) {
	t.Parallel()
	document := parseJTDTestSchema(t, losslessSchema)
	_, jtd, err := schema.ExportJTD(document, schema.JTDExportOptions{Root: "Node"})
	if err != nil {
		t.Fatal(err)
	}
	jsonSchema, err := schema.BindJSONSchemaFidelity(document, schema.JSONSchemaFidelity{Dialect: schema.JSONSchema202012, Exact: true})
	if err != nil || jsonSchema.Binding.Format == jtd.Binding.Format || !jsonSchema.Exact {
		t.Fatalf("independent JSON Schema report = %#v, %v", jsonSchema, err)
	}
	if err := schema.CompareSchemaMappingBindings(jsonSchema.Binding, jtd.Binding); err != nil {
		t.Fatalf("same canonical source rejected: %v", err)
	}
	other := jsonSchema.Binding
	other.NaatreSchemaHash = strings.Repeat("0", 64)
	var mismatch *schema.SchemaMappingError
	if err := schema.CompareSchemaMappingBindings(other, jtd.Binding); !errors.As(err, &mismatch) || mismatch.Code != "SCHEMA_MAPPING_SOURCE_MISMATCH" {
		t.Fatalf("source mismatch = %v", err)
	}
}

func parseJTDTestSchema(t testing.TB, input string) schema.Document {
	t.Helper()
	document, err := schema.ParseDocument([]byte(input), schema.ImportOptions{SupportedTraits: map[string]bool{schema.ConstraintTraitID: true}})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	return document
}

func assertJTDDiagnostic(t testing.TB, output []byte, report schema.JTDFidelityReport, err error, code string) {
	t.Helper()
	var mappingErr *schema.JTDError
	if len(output) != 0 || !errors.As(err, &mappingErr) || report.Exact || len(report.Diagnostics) == 0 || report.Diagnostics[0].Code != code || mappingErr.Diagnostics()[0].Code != code {
		t.Fatalf("JTD failure = %s, %#v, %v; want %s", output, report, err, code)
	}
}
