package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunExecutesExplicitPlugin(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..")
	output := t.TempDir()
	arguments := []string{
		filepath.Join(root, "conformance", "third_party", "generator-plugin", "javascript", "plugin.naatre-generator.json"),
		node,
		filepath.Join(root, "conformance", "v1", "generator-plugin-host-model.json"),
		output,
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), arguments, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr = %q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(output, "generated", "fixture.mjs")); err != nil {
		t.Fatalf("generated fixture: %v", err)
	}
}

func TestRunReportsSafeFailures(t *testing.T) {
	t.Parallel()
	for _, arguments := range [][]string{nil, {"private-manifest", "private-runtime", "private-model", "private-output"}} {
		var stderr bytes.Buffer
		if code := run(context.Background(), arguments, &stderr); code == 0 {
			t.Fatalf("run(%v) succeeded", arguments)
		}
		for _, private := range arguments {
			if strings.Contains(stderr.String(), private) {
				t.Fatalf("run leaked %q: %q", private, stderr.String())
			}
		}
	}
}
