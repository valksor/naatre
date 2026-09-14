package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunWritesGeneratedArtifactsAndUsesSafeDiagnostics(t *testing.T) {
	root := filepath.Join("..", "..")
	output := t.TempDir()
	var stderr bytes.Buffer
	status := run([]string{
		filepath.Join(root, "conformance", "v1", "generator-model.json"),
		filepath.Join(root, "conformance", "v1", "generator-output.json"),
		output,
	}, &stderr)
	if status != 0 || stderr.Len() != 0 {
		t.Fatalf("run status/stderr = %d/%q", status, stderr.String())
	}
	for _, name := range []string{"operations.go", "operations.json"} {
		if content, err := os.ReadFile(filepath.Join(output, name)); err != nil || len(content) == 0 {
			t.Fatalf("generated %s = %d bytes, %v", name, len(content), err)
		}
	}

	stderr.Reset()
	if status := run([]string{"missing", "secret-reference", output}, &stderr); status != 1 {
		t.Fatalf("missing model status = %d", status)
	}
	if bytes.Contains(stderr.Bytes(), []byte("secret-reference")) {
		t.Fatalf("diagnostic leaked argument: %q", stderr.String())
	}

	previousArguments := os.Args
	os.Args = []string{"naatre-go-sdk-generator",
		filepath.Join(root, "conformance", "v1", "generator-model.json"),
		filepath.Join(root, "conformance", "v1", "generator-output.json"),
		t.TempDir(),
	}
	t.Cleanup(func() { os.Args = previousArguments })
	main()
}
