package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunGeneratesPinnedBundle(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{filepath.Join(root, "conformance", "v1", "generator-model.json")}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code = %d, stderr = %q", code, stderr.String())
	}
	expected, err := os.ReadFile(filepath.Join(root, "conformance", "v1", "generator-output.json"))
	if err != nil {
		t.Fatalf("read expected output: %v", err)
	}
	if !bytes.Equal(stdout.Bytes(), expected) || stderr.Len() != 0 {
		t.Fatalf("run output mismatch: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunReportsSafeFailures(t *testing.T) {
	t.Parallel()
	for _, arguments := range [][]string{nil, {"secret-model-path"}} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := run(arguments, &stdout, &stderr); code == 0 {
			t.Fatalf("run(%v) succeeded", arguments)
		}
		if stdout.Len() != 0 || strings.Contains(stderr.String(), "secret-model-path") {
			t.Fatalf("run(%v) leaked input: stdout=%q stderr=%q", arguments, stdout.String(), stderr.String())
		}
	}
}
