package conformance_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/valksor/naatre/schema"
)

type portableSchemaFixture struct {
	Profile         string                       `json:"profile"`
	DocumentVersion string                       `json:"documentVersion"`
	Snapshots       []portableSchemaSnapshotCase `json:"snapshots"`
	Filters         []portableSchemaFilterCase   `json:"filters"`
	Diffs           []portableSchemaDiffCase     `json:"diffs"`
}

type portableSchemaSnapshotCase struct {
	Name            string          `json:"name"`
	SupportedTraits []string        `json:"supportedTraits,omitempty"`
	Document        json.RawMessage `json:"document"`
}

type portableSchemaFilterCase struct {
	Name       string            `json:"name"`
	Source     string            `json:"source"`
	Result     string            `json:"result"`
	Visibility schema.Visibility `json:"visibility"`
	Excluded   []string          `json:"excluded"`
}

type portableSchemaDiffCase struct {
	Name    string                 `json:"name"`
	Before  json.RawMessage        `json:"before"`
	After   json.RawMessage        `json:"after"`
	Changes []portableSchemaChange `json:"changes"`
}

type portableSchemaChange struct {
	Path           string                      `json:"path"`
	Classification schema.ChangeClassification `json:"classification"`
}

func TestPortableSchemaDocumentFixtures(t *testing.T) {
	t.Parallel()
	var fixture portableSchemaFixture
	readFixture(t, "schema.json", &fixture)
	if fixture.Profile != "core.schema-1" || fixture.DocumentVersion != schema.SchemaDocumentVersion {
		t.Fatalf("schema fixture header = %q/%q", fixture.Profile, fixture.DocumentVersion)
	}

	wantCases := []string{"complete", "deprecation", "extended", "filtered", "recursive"}
	documents := make(map[string]schema.Document, len(fixture.Snapshots))
	caseNames := make([]string, 0, len(fixture.Snapshots))
	for _, test := range fixture.Snapshots {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			document := parsePortableSchema(t, test.Document, test.SupportedTraits)
			if _, err := document.Snapshot(); err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			canonical, err := document.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			again := parsePortableSchema(t, canonical, test.SupportedTraits)
			roundTrip, _ := again.CanonicalJSON()
			if !bytes.Equal(canonical, roundTrip) {
				t.Fatalf("canonical round trip differs:\n%s\n%s", canonical, roundTrip)
			}
			documents[test.Name] = document
			caseNames = append(caseNames, test.Name)
		})
	}
	slices.Sort(caseNames)
	if !slices.Equal(caseNames, wantCases) {
		t.Fatalf("snapshot cases = %v, want %v", caseNames, wantCases)
	}

	complete := documents["complete"]
	snapshot, err := complete.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	exported, err := schema.ExportDocument(snapshot, complete.Operations(), complete.Members(), complete.ExportOptions())
	if err != nil {
		t.Fatalf("ExportDocument: %v", err)
	}
	independentCanonical, _ := complete.CanonicalJSON()
	goCanonical, _ := exported.CanonicalJSON()
	independentHash, _ := complete.Hash()
	goHash, _ := exported.Hash()
	if !bytes.Equal(independentCanonical, goCanonical) || independentHash != goHash {
		t.Fatalf("independent -> Go -> export differs:\n%s\n%s\n%#v\n%#v", independentCanonical, goCanonical, independentHash, goHash)
	}

	for _, test := range fixture.Filters {
		t.Run(test.Name, func(t *testing.T) {
			filtered, err := documents[test.Source].Filter(test.Visibility)
			if err != nil {
				t.Fatalf("Filter: %v", err)
			}
			got, _ := filtered.CanonicalJSON()
			want, _ := documents[test.Result].CanonicalJSON()
			if !bytes.Equal(got, want) {
				t.Fatalf("filtered schema differs:\n%s\n%s", got, want)
			}
			for _, hidden := range test.Excluded {
				if bytes.Contains(got, []byte(hidden)) {
					t.Fatalf("filtered schema leaked %q: %s", hidden, got)
				}
			}
		})
	}
}

func TestPortableSchemaDiffFixtures(t *testing.T) {
	t.Parallel()
	var fixture portableSchemaFixture
	readFixture(t, "schema.json", &fixture)
	if len(fixture.Diffs) < 2 {
		t.Fatal("schema fixture requires evolving and open-union diff cases")
	}
	for _, test := range fixture.Diffs {
		t.Run(test.Name, func(t *testing.T) {
			before := parsePortableSchema(t, test.Before, nil)
			after := parsePortableSchema(t, test.After, nil)
			diff := schema.DiffDocuments(before, after)
			for _, expected := range test.Changes {
				if !slices.ContainsFunc(diff.Changes, func(change schema.SchemaChange) bool {
					return change.Path == expected.Path && change.Classification == expected.Classification
				}) {
					t.Errorf("missing %#v in %#v", expected, diff.Changes)
				}
			}
		})
	}
}

func parsePortableSchema(t *testing.T, input []byte, supported []string) schema.Document {
	t.Helper()
	traits := make(map[string]bool, len(supported))
	for _, id := range supported {
		traits[id] = true
	}
	document, err := schema.ParseDocument(input, schema.ImportOptions{SupportedTraits: traits})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	return document
}
