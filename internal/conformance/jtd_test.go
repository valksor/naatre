package conformance_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/valksor/naatre/schema"
)

type jtdConformanceFixture struct {
	Profile                 string          `json:"profile"`
	FixtureSuite            string          `json:"fixtureSuite"`
	RFC                     string          `json:"rfc"`
	MapperRevision          string          `json:"mapperRevision"`
	FidelityProfileRevision string          `json:"fidelityProfileRevision"`
	Root                    schema.TypeID   `json:"root"`
	LosslessSchema          json.RawMessage `json:"losslessSchema"`
	Forms                   []struct {
		Name               string          `json:"name"`
		Definition         string          `json:"definition"`
		Keyword            string          `json:"keyword"`
		NestedKeyword      string          `json:"nestedKeyword"`
		NegativeImport     json.RawMessage `json:"negativeImport"`
		NegativeImportCode string          `json:"negativeImportCode"`
		NegativeExport     string          `json:"negativeExport"`
		NegativeExportCode string          `json:"negativeExportCode"`
	} `json:"forms"`
	NegativeExports []struct {
		Name            string          `json:"name"`
		Root            schema.TypeID   `json:"root"`
		SupportedTraits []string        `json:"supportedTraits"`
		Document        json.RawMessage `json:"document"`
	} `json:"negativeExports"`
	BoundedFailures []struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
		Code  string          `json:"code"`
	} `json:"boundedFailures"`
	Commands []string `json:"commands"`
}

func TestJTDConformanceFixture(t *testing.T) {
	t.Parallel()
	var fixture jtdConformanceFixture
	readFixture(t, "jtd.json", &fixture)
	if fixture.Profile != "schema.jtd-1" || fixture.FixtureSuite != "1.0.0" || fixture.RFC != schema.JTDRFC8927 || fixture.MapperRevision != schema.JTDMapperRevision || fixture.FidelityProfileRevision != schema.JTDFidelityProfileRevision {
		t.Fatalf("JTD fixture header = %#v", fixture)
	}
	document, err := schema.ParseDocument(fixture.LosslessSchema, schema.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	projection, report, err := schema.ExportJTD(document, schema.JTDExportOptions{Root: fixture.Root})
	if err != nil || !report.Exact {
		t.Fatalf("lossless export = %#v, %v", report, err)
	}
	again, _, err := schema.ExportJTD(document, schema.JTDExportOptions{Root: fixture.Root})
	if err != nil || !bytes.Equal(projection, again) {
		t.Fatalf("JTD export bytes changed:\n%s\n%s\n%v", projection, again, err)
	}
	imported, importedReport, err := schema.ImportJTD(projection, schema.JTDImportOptions{Approved: true, UseEmbeddedIdentities: true})
	if err != nil || !importedReport.Exact {
		t.Fatalf("lossless import = %#v, %v", importedReport, err)
	}
	want, _ := document.CanonicalJSON()
	got, _ := imported.CanonicalJSON()
	if !bytes.Equal(want, got) {
		t.Fatalf("JTD round trip differs:\n%s\n%s", want, got)
	}

	var thirdParty struct {
		Definitions map[string]map[string]json.RawMessage `json:"definitions"`
		Ref         string                                `json:"ref"`
	}
	if err := json.Unmarshal(projection, &thirdParty); err != nil || thirdParty.Ref != string(fixture.Root) {
		t.Fatalf("standalone RFC 8927 projection = %#v, %v", thirdParty, err)
	}
	negativeExports := make(map[string]struct {
		root     schema.TypeID
		document json.RawMessage
		traits   []string
	}, len(fixture.NegativeExports))
	for _, vector := range fixture.NegativeExports {
		negativeExports[vector.Name] = struct {
			root     schema.TypeID
			document json.RawMessage
			traits   []string
		}{vector.Root, vector.Document, vector.SupportedTraits}
	}
	if len(fixture.Forms) != 7 {
		t.Fatalf("supported JTD form fixtures = %d", len(fixture.Forms))
	}
	for _, form := range fixture.Forms {
		form := form
		t.Run(form.Name, func(t *testing.T) {
			definition := thirdParty.Definitions[form.Definition]
			if definition[form.Keyword] == nil || (form.NestedKeyword != "" && !bytes.Contains(definition[form.Keyword], []byte(`"`+form.NestedKeyword+`"`))) {
				t.Fatalf("positive %s export fixture missing %s/%s: %s", form.Name, form.Keyword, form.NestedKeyword, projection)
			}
			options := plainJTDOptions()
			_, importReport, err := schema.ImportJTD(form.NegativeImport, options)
			assertJTDConformanceError(t, importReport, err, form.NegativeImportCode)

			exportVector, ok := negativeExports[form.NegativeExport]
			if !ok {
				t.Fatalf("missing negative export fixture %q", form.NegativeExport)
			}
			traits := make(map[string]bool, len(exportVector.traits))
			for _, id := range exportVector.traits {
				traits[id] = true
			}
			negativeDocument, err := schema.ParseDocument(exportVector.document, schema.ImportOptions{SupportedTraits: traits})
			if err != nil {
				t.Fatalf("negative export source: %v", err)
			}
			_, exportReport, err := schema.ExportJTD(negativeDocument, schema.JTDExportOptions{Root: exportVector.root})
			assertJTDConformanceError(t, exportReport, err, form.NegativeExportCode)
		})
	}
	for _, vector := range fixture.BoundedFailures {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			_, boundedReport, err := schema.ImportJTD(vector.Input, plainJTDOptions())
			assertJTDConformanceError(t, boundedReport, err, vector.Code)
		})
	}
	if len(fixture.Commands) != 2 {
		t.Fatalf("JTD verification commands = %#v", fixture.Commands)
	}
}

func plainJTDOptions() schema.JTDImportOptions {
	return schema.JTDImportOptions{
		Approved: true, Revision: "jtd-import-r1", DefaultOutput: true,
		Identities:       map[string]schema.TypeID{"Thing": "Thing"},
		FieldIdentities:  map[string]string{"Thing/value": "Thing.value"},
		MemberIdentities: map[string]string{"Thing/same": "Thing.same", "Thing/variant": "Thing.variant"},
	}
}

func assertJTDConformanceError(t testing.TB, report schema.JTDFidelityReport, err error, code string) {
	t.Helper()
	var mappingErr *schema.JTDError
	if !errors.As(err, &mappingErr) || report.Exact || len(report.Diagnostics) == 0 || report.Diagnostics[0].Code != code {
		t.Fatalf("JTD diagnostic = %#v, %v; want %s", report, err, code)
	}
}
