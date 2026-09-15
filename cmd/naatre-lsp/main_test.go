package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRequiresBoundedValidLocalSchema(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr); code != 2 || stderr.String() != "LSP_SCHEMA_REQUIRED\n" {
		t.Fatalf("missing schema = %d, %q", code, stderr.String())
	}

	stderr.Reset()
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(`{"password":"must-not-leak"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"--schema", path}, strings.NewReader(""), &stdout, &stderr); code != 1 || stderr.String() != "LSP_SCHEMA_INVALID\n" {
		t.Fatalf("invalid schema = %d, %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), path) || strings.Contains(stderr.String(), "must-not-leak") {
		t.Fatalf("schema failure leaked input: %q", stderr.String())
	}
}
