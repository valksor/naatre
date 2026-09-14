package sdkgen_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/sdk/swift/sdkgen"
)

func TestGenerateReproducesCheckedArtifacts(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	model := readFixture(t, filepath.Join(root, "conformance/v1/generator-model.json"))
	reference := readFixture(t, filepath.Join(root, "conformance/v1/generator-output.json"))
	first, err := sdkgen.Generate(model, reference)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	second, err := sdkgen.Generate(model, reference)
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if !bytes.Equal(first.Source, second.Source) || !bytes.Equal(first.Manifest, second.Manifest) {
		t.Fatal("generation is not deterministic")
	}
	if !bytes.Equal(first.Source, readFixture(t, filepath.Join("..", "Sources/NaatreGenerated/Operations.swift"))) {
		t.Fatal("generated Swift source drifted")
	}
	if !bytes.Contains(first.Source, []byte("public struct GetAccountVariables: Codable, Sendable")) ||
		!bytes.Contains(first.Source, []byte("public init(from decoder: Decoder) throws")) {
		t.Fatal("generated variables are not Codable")
	}
	if !bytes.Equal(first.Manifest, readFixture(t, filepath.Join("..", "Sources/NaatreGenerated/operations.json"))) {
		t.Fatal("generated Swift manifest drifted")
	}
}

func TestGenerateRejectsReferenceDriftAndUnmappedScalar(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	model := readFixture(t, filepath.Join(root, "conformance/v1/generator-model.json"))
	reference := readFixture(t, filepath.Join(root, "conformance/v1/generator-output.json"))
	drifted := bytes.Replace(reference, []byte("cc863005"), []byte("0c863005"), 1)
	_, err := sdkgen.Generate(model, drifted)
	var generated *sdkgen.Error
	if !errors.As(err, &generated) || generated.Code != "SWIFT_SDK_GENERATOR_REFERENCE_DRIFT" {
		t.Fatalf("error = %v, want SWIFT_SDK_GENERATOR_REFERENCE_DRIFT", err)
	}

	var value map[string]any
	if err := json.Unmarshal(model, &value); err != nil {
		t.Fatal(err)
	}
	configuration := value["configuration"].(map[string]any)
	delete(configuration["scalarMappings"].(map[string]any), "Money")
	changed, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sdkgen.Generate(changed, reference)
	generated = nil
	if !errors.As(err, &generated) || generated.Code != "SWIFT_SDK_GENERATOR_UNMAPPED_SCALAR" {
		t.Fatalf("error = %v, want SWIFT_SDK_GENERATOR_UNMAPPED_SCALAR", err)
	}
}

func TestGenerateRepresentsOpenEnumsAndUnionsWithoutReflection(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	modelBytes := readFixture(t, filepath.Join(root, "conformance/v1/generator-model.json"))
	var value map[string]any
	if err := json.Unmarshal(modelBytes, &value); err != nil {
		t.Fatal(err)
	}
	schema := value["schema"].(map[string]any)
	types := schema["types"].([]any)
	admin := map[string]any{"id": "Admin", "name": "Admin", "kind": "object", "output": true, "fields": []any{map[string]any{"id": "Admin.id", "name": "id", "type": "ID", "required": true}}}
	search := map[string]any{"id": "SearchResult", "name": "SearchResult", "kind": "union", "output": true, "open": true, "variantMembers": []any{map[string]any{"id": "SearchResult.Admin", "type": "Admin"}, map[string]any{"id": "SearchResult.Account", "type": "Account"}}}
	schema["types"] = append(types, admin, search)
	changed, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := generator.Generate(changed)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := sdkgen.Generate(changed, reference)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(artifacts.Source, []byte("public typealias SearchResult = OpenVariant")) ||
		!bytes.Contains(artifacts.Source, []byte("case unknown(String)")) {
		t.Fatalf("generated source does not preserve open variants:\n%s", artifacts.Source)
	}
}

func readFixture(t *testing.T, path string) []byte {
	contents, err := os.ReadFile(path)
	if err == nil {
		return contents
	}
	t.Fatalf("read fixture %q: %v", path, err)
	return nil
}
