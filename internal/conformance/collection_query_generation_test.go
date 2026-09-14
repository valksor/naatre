package conformance_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

type collectionQueryGenerationFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Source       struct {
		Profile string `json:"profile"`
		Path    string `json:"path"`
		Pointer string `json:"pointer"`
		SHA256  string `json:"sha256"`
	} `json:"source"`
	Generator struct {
		Language string `json:"language"`
		Runtime  string `json:"runtime"`
		Version  string `json:"version"`
		Path     string `json:"path"`
		SHA256   string `json:"sha256"`
	} `json:"generator"`
	Verifier struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"verifier"`
	Outputs []struct {
		Language          string   `json:"language"`
		Path              string   `json:"path"`
		SHA256            string   `json:"sha256"`
		RuntimeDependency string   `json:"runtimeDependency"`
		Platforms         []string `json:"platforms"`
		Features          []string `json:"features"`
	} `json:"outputs"`
	ProviderEvidence struct {
		Language    string   `json:"language"`
		Provider    string   `json:"provider"`
		Dependency  string   `json:"dependency"`
		Commands    []string `json:"commands"`
		Parity      []string `json:"parity"`
		Unsupported []string `json:"unsupported"`
	} `json:"providerEvidence"`
	NegativeCases []struct {
		Name   string `json:"name"`
		Result string `json:"result"`
	} `json:"negativeCases"`
}

func TestCollectionQueryGeneratedTypeScriptEvidence(t *testing.T) {
	t.Parallel()
	var fixture collectionQueryGenerationFixture
	readFixture(t, "collection-query-generation.json", &fixture)
	validateCollectionQueryGenerationFixture(t, &fixture)
	root := filepath.Join("..", "..")
	verifyCollectionQueryGenerationDigests(t, root, &fixture)
	verifyCollectionQueryGeneratedTypeScript(t, root, &fixture)
	assertCollectionQueryGeneratorFailures(t, filepath.Join(root, filepath.FromSlash(fixture.Generator.Path)), fixture.NegativeCases)
}

func validateCollectionQueryGenerationFixture(t testing.TB, fixture *collectionQueryGenerationFixture) {
	t.Helper()
	if fixture.Profile != "collection.query.codegen-1" || fixture.FixtureSuite != "1.0.0" || fixture.Source.Profile != "collection.query-1" || fixture.Source.Pointer != "/query/schema" {
		t.Fatalf("generation profile metadata = %#v", fixture)
	}
	if fixture.Generator.Language != "javascript" || fixture.Generator.Runtime != "node" || fixture.Generator.Version != "collection-query-ts-1" {
		t.Fatalf("generator identity = %#v", fixture.Generator)
	}
	if fixture.Verifier.Path == "" || fixture.Verifier.SHA256 == "" {
		t.Fatalf("verifier identity = %#v", fixture.Verifier)
	}
	if len(fixture.Outputs) != 1 || fixture.Outputs[0].Language != "typescript" || fixture.Outputs[0].RuntimeDependency != "none" {
		t.Fatalf("generated outputs = %#v", fixture.Outputs)
	}
	if !slices.Contains(fixture.ProviderEvidence.Parity, "cancellation") || !slices.Contains(fixture.ProviderEvidence.Unsupported, "decimal") {
		t.Fatalf("provider evidence boundary = %#v", fixture.ProviderEvidence)
	}
	if len(fixture.NegativeCases) != 4 || fixture.NegativeCases[3].Name != "output-drift" || fixture.NegativeCases[3].Result != "digest-mismatch" {
		t.Fatalf("generator failure evidence = %#v", fixture.NegativeCases)
	}
}

func verifyCollectionQueryGenerationDigests(t testing.TB, root string, fixture *collectionQueryGenerationFixture) {
	t.Helper()
	for path, want := range map[string]string{
		filepath.Join("conformance", filepath.FromSlash(fixture.Source.Path)): fixture.Source.SHA256,
		filepath.FromSlash(fixture.Generator.Path):                            fixture.Generator.SHA256,
		filepath.FromSlash(fixture.Verifier.Path):                             fixture.Verifier.SHA256,
		filepath.FromSlash(fixture.Outputs[0].Path):                           fixture.Outputs[0].SHA256,
	} {
		if got := conformanceFileDigest(t, filepath.Join(root, path)); got != want {
			t.Errorf("digest %s = %s, want %s", path, got, want)
		}
	}
}

func verifyCollectionQueryGeneratedTypeScript(t testing.TB, root string, fixture *collectionQueryGenerationFixture) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for advertised JavaScript generator evidence")
	}
	command := exec.Command(node, filepath.Join(root, filepath.FromSlash(fixture.Generator.Path)))
	generated, err := command.Output()
	if err != nil {
		t.Fatalf("run collection query generator: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(fixture.Outputs[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, want) {
		t.Fatal("generated TypeScript differs from the checked-in artifact")
	}
	if output, err := exec.Command(node, "--experimental-strip-types", filepath.Join(root, filepath.FromSlash(fixture.Outputs[0].Path))).CombinedOutput(); err != nil {
		t.Fatalf("parse generated TypeScript: %v: %s", err, output)
	}
}

func assertCollectionQueryGeneratorFailures(t *testing.T, generator string, cases []struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for advertised JavaScript generator evidence")
	}
	for _, test := range cases[:3] {
		t.Run(test.Name, func(t *testing.T) {
			fields := []map[string]any{{"id": "User.age", "type": "Int64", "operators": []string{"eq"}}}
			switch test.Name {
			case "unknown-scalar":
				fields[0]["type"] = "Money"
			case "generated-symbol-collision":
				fields = append(fields, map[string]any{"id": "User-age", "type": "Int64", "operators": []string{"eq"}})
			case "unknown-operator":
				fields[0]["operators"] = []string{"execute-sql"}
			default:
				t.Fatalf("unhandled generator failure vector %q", test.Name)
			}
			input, err := json.Marshal(map[string]any{"query": map[string]any{"profile": "collection.query-1", "schema": map[string]any{"capability": "collection.query-1", "fields": fields}}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "fixture.json")
			if err := os.WriteFile(path, input, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := exec.Command(node, generator, path).Run(); err == nil || test.Result != "generation-error" {
				t.Fatalf("generator result = %v, want %s", err, test.Result)
			}
		})
	}
}

func conformanceFileDigest(t testing.TB, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
