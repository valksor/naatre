package sdkgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valksor/naatre/generator"
)

func TestGenerateReproducesJavaAndKotlinBindings(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..")
	model := mustRead(t, filepath.Join(root, "conformance", "v1", "generator-model.json"))
	reference := mustRead(t, filepath.Join(root, "conformance", "v1", "generator-output.json"))
	wantJava := mustRead(t, filepath.Join(root, "sdk", "jvm", "generated", "java", "io", "naatre", "sdk", "generated", "java", "GetAccount.java"))
	wantKotlin := mustRead(t, filepath.Join(root, "sdk", "jvm", "generated", "kotlin", "io", "naatre", "sdk", "generated", "kotlin", "GetAccount.kt"))
	wantManifest := mustRead(t, filepath.Join(root, "sdk", "jvm", "generated", "operations.json"))

	first, err := Generate(model, reference)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	second, err := Generate(bytes.Clone(model), bytes.Clone(reference))
	if err != nil {
		t.Fatalf("Generate second pass: %v", err)
	}
	if !bytes.Equal(first.Java, wantJava) || !bytes.Equal(first.Kotlin, wantKotlin) || !bytes.Equal(first.Manifest, wantManifest) {
		t.Fatal("generated JVM artifacts differ from checked-in bytes")
	}
	if !bytes.Equal(first.Java, second.Java) || !bytes.Equal(first.Kotlin, second.Kotlin) || !bytes.Equal(first.Manifest, second.Manifest) {
		t.Fatal("JVM generation is not deterministic")
	}
	for _, source := range [][]byte{first.Java, first.Kotlin} {
		if bytes.Contains(source, []byte("process.env")) || bytes.Contains(source, []byte("</script>")) {
			t.Fatal("untrusted operation description reached generated source")
		}
	}
}

func TestGenerateRejectsDriftAndOversizeWithStableCodes(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..")
	model := mustRead(t, filepath.Join(root, "conformance", "v1", "generator-model.json"))
	reference := mustRead(t, filepath.Join(root, "conformance", "v1", "generator-output.json"))

	_, err := Generate(model, append(bytes.Clone(reference), '\n'))
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "JVM_SDK_GENERATOR_REFERENCE_DRIFT" {
		t.Fatalf("reference drift error = %v", err)
	}
	_, err = Generate(make([]byte, maxInputBytes+1), reference)
	failure = nil
	if !errors.As(err, &failure) || failure.Code != "JVM_SDK_GENERATOR_INPUT_LIMIT" {
		t.Fatalf("input limit error = %v", err)
	}
}

func TestGenerateRejectsSameCountDifferentFixtureShape(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..")
	modelBytes := mustRead(t, filepath.Join(root, "conformance", "v1", "generator-model.json"))
	var changed map[string]any
	if err := json.Unmarshal(modelBytes, &changed); err != nil {
		t.Fatal(err)
	}
	operations := changed["operations"].([]any)
	operation := operations[0].(map[string]any)
	variables := operation["variables"].([]any)
	variables[0].(map[string]any)["name"] = "accountID"
	changedModel, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	matchingReference, err := generator.Generate(changedModel)
	if err != nil {
		t.Fatalf("generate matching reference: %v", err)
	}
	_, err = Generate(changedModel, matchingReference)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "JVM_SDK_GENERATOR_UNSUPPORTED_MODEL" {
		t.Fatalf("same-count shape drift error = %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}
