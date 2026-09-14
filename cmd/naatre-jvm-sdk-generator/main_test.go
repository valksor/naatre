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
	for _, name := range []string{
		"java/io/naatre/sdk/generated/java/GetAccount.java",
		"kotlin/io/naatre/sdk/generated/kotlin/GetAccount.kt",
		"operations.json",
	} {
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
}

func TestRunRejectsSymlinkOutputTraversal(t *testing.T) {
	root := filepath.Join("..", "..")
	model := filepath.Join(root, "conformance", "v1", "generator-model.json")
	reference := filepath.Join(root, "conformance", "v1", "generator-output.json")

	t.Run("parent", func(t *testing.T) {
		output := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(output, "java")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if status := run([]string{model, reference, output}, &bytes.Buffer{}); status != 1 {
			t.Fatalf("symlink parent status = %d", status)
		}
		if _, err := os.Stat(filepath.Join(outside, "io", "naatre", "sdk", "generated", "java", "GetAccount.java")); !os.IsNotExist(err) {
			t.Fatalf("write escaped through parent symlink: %v", err)
		}
	})

	t.Run("leaf", func(t *testing.T) {
		output := t.TempDir()
		parent := filepath.Join(output, "java", "io", "naatre", "sdk", "generated", "java")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.java")
		if err := os.WriteFile(outside, []byte("preserve"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(parent, "GetAccount.java")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if status := run([]string{model, reference, output}, &bytes.Buffer{}); status != 1 {
			t.Fatalf("symlink leaf status = %d", status)
		}
		content, err := os.ReadFile(outside)
		if err != nil || string(content) != "preserve" {
			t.Fatalf("leaf symlink target changed: %q, %v", content, err)
		}
	})
}
