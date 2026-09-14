package sdkgen

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateReproducesCheckedArtifacts(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..")
	mustRead := func(path string) []byte {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return content
	}
	model := mustRead(filepath.Join(root, "conformance", "v1", "generator-model.json"))
	reference := mustRead(filepath.Join(root, "conformance", "v1", "generator-output.json"))
	wantSource := mustRead(filepath.Join(root, "sdk", "go", "generated", "operations.go"))
	wantManifest := mustRead(filepath.Join(root, "sdk", "go", "generated", "operations.json"))

	first, err := Generate(model, reference)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	second, err := Generate(bytes.Clone(model), bytes.Clone(reference))
	if err != nil {
		t.Fatalf("Generate second pass: %v", err)
	}
	if !bytes.Equal(first.Source, wantSource) || !bytes.Equal(first.Manifest, wantManifest) {
		t.Fatal("generated Go SDK artifacts differ from checked-in bytes")
	}
	if !bytes.Equal(first.Source, second.Source) || !bytes.Equal(first.Manifest, second.Manifest) {
		t.Fatal("Go SDK generation is not deterministic")
	}
	if bytes.Contains(first.Source, []byte("process.env")) || bytes.Contains(first.Source, []byte("</script>")) {
		t.Fatal("untrusted operation description reached generated source")
	}
}

func TestGenerateRejectsReferenceDriftAndInputLimitsWithStableCodes(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..")
	model, err := os.ReadFile(filepath.Join(root, "conformance", "v1", "generator-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	reference, err := os.ReadFile(filepath.Join(root, "conformance", "v1", "generator-output.json"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = Generate(model, append(bytes.Clone(reference), '\n'))
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "GO_SDK_GENERATOR_REFERENCE_DRIFT" {
		t.Fatalf("reference drift error = %v", err)
	}
	_, err = Generate(make([]byte, maxInputBytes+1), reference)
	failure = nil
	if !errors.As(err, &failure) || failure.Code != "GO_SDK_GENERATOR_INPUT_LIMIT" {
		t.Fatalf("input limit error = %v", err)
	}
}
