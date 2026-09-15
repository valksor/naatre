package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valksor/naatre/tooling"
)

const cliDocument = `{"operations":[{"name":"GetAccount","kind":"query","select":[{"$call":{"name":"account","args":{"id":{"$literal":"acct-1"}},"select":[{"$field":{"name":"name"}}]}}]}]}`

const cliSchema = `{
  "version":"1","canonicalVersion":"c14n-1","revision":"tooling-cli-r1",
  "types":[
    {"id":"LookupInput","name":"LookupInput","kind":"input-object","input":true,"fields":[{"id":"LookupInput.id","name":"id","type":"ID","required":true}]},
    {"id":"User","name":"User","kind":"object","output":true,"fields":[{"id":"User.name","name":"name","type":"String"}]}
  ],
  "operations":[{"id":"query.account","name":"account","kind":"query","input":"LookupInput","output":"User","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1}],
  "members":[{"id":"User.name.resolver","name":"name","owner":"User","kind":"field","output":"String","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1}]
}`

type rejectingWriter struct{}

func (rejectingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output rejected")
}

func TestRunUsesPublishedExitCodesAndSharedDiagnostics(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	invalid := filepath.Join(directory, "invalid.json")
	invalidBytes := []byte(`{"operations":[{"name":"bad name","kind":"query","select":[]}]}`)
	if err := os.WriteFile(invalid, invalidBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	status := run([]string{"validate", "--document", invalid}, &stdout, &stderr)
	if status != exitDiagnostic || stderr.Len() != 0 {
		t.Fatalf("validate status/stderr = %d/%q", status, stderr.String())
	}
	want, _ := json.Marshal(tooling.EditorAdapter{}.Diagnostics(invalidBytes, ""))
	if !bytes.Equal(bytes.TrimSpace(stdout.Bytes()), want) {
		t.Fatalf("CLI/editor diagnostics differ:\n%s\n%s", stdout.Bytes(), want)
	}
	stdout.Reset()
	if status := run([]string{"unknown"}, &stdout, &stderr); status != exitUsage {
		t.Fatalf("usage status = %d", status)
	}
	stderr.Reset()
	if status := run([]string{"validate", "--document", filepath.Join(directory, "missing")}, &stdout, &stderr); status != exitIO {
		t.Fatalf("I/O status = %d", status)
	}
	valid := writeTestFile(t, directory, "valid.json", []byte(cliDocument))
	if status := run([]string{"validate", "--document", valid}, rejectingWriter{}, &stderr); status != exitIO {
		t.Fatalf("output I/O status = %d", status)
	}
}

func TestDocumentAndCompatibilityWorkflow(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	documentPath := writeTestFile(t, directory, "operation.json", []byte(cliDocument))
	schemaPath := writeTestFile(t, directory, "schema.json", []byte(cliSchema))

	var output, diagnostic bytes.Buffer
	if status := run([]string{"validate", "--document", documentPath, "--schema", schemaPath, "--operation", "GetAccount"}, &output, &diagnostic); status != exitOK {
		t.Fatalf("validate = %d, %s, %s", status, output.String(), diagnostic.String())
	}
	output.Reset()
	if status := run([]string{"format", "--document", documentPath}, &output, &diagnostic); status != exitOK {
		t.Fatalf("format = %d, %s", status, diagnostic.String())
	}
	formattedPath := writeTestFile(t, directory, "formatted.json", output.Bytes())
	output.Reset()
	if status := run([]string{"hash", "--document", formattedPath}, &output, &diagnostic); status != exitOK {
		t.Fatalf("hash = %d, %s", status, diagnostic.String())
	}
	var digest struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(output.Bytes(), &digest); err != nil || digest.Digest == "" {
		t.Fatalf("hash output = %q, %v", output.String(), err)
	}
	output.Reset()
	if status := run([]string{"manifest", "--document", formattedPath}, &output, &diagnostic); status != exitOK {
		t.Fatalf("manifest = %d, %s", status, diagnostic.String())
	}
	manifestPath := writeTestFile(t, directory, "manifest.json", output.Bytes())
	output.Reset()
	if status := run([]string{"compatibility", "--schema", schemaPath, "--manifest", manifestPath}, &output, &diagnostic); status != exitOK {
		t.Fatalf("compatibility = %d, %s, %s", status, output.String(), diagnostic.String())
	}
	var compatibility struct {
		Compatible bool `json:"compatible"`
	}
	if err := json.Unmarshal(output.Bytes(), &compatibility); err != nil || !compatibility.Compatible {
		t.Fatalf("compatibility output = %q, %v", output.String(), err)
	}
}

func TestGenerateGoClientAndConformanceCommands(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	outputDirectory := t.TempDir()
	var stdout, stderr bytes.Buffer
	status := run([]string{
		"generate", "--language", "go",
		"--model", filepath.Join(root, "conformance", "v1", "generator-model.json"),
		"--reference", filepath.Join(root, "conformance", "v1", "generator-output.json"),
		"--out", outputDirectory,
	}, &stdout, &stderr)
	if status != exitOK || stderr.Len() != 0 {
		t.Fatalf("generate = %d, %s", status, stderr.String())
	}
	for _, name := range []string{"operations.go", "operations.json"} {
		if information, err := os.Stat(filepath.Join(outputDirectory, name)); err != nil || information.Size() == 0 {
			t.Fatalf("generated %s: %v", name, err)
		}
	}
	fixture := filepath.Join(root, "conformance", "v1", "tooling.json")
	if status := run([]string{"conformance", "--fixture", fixture}, &stdout, &stderr); status != exitOK {
		t.Fatalf("conformance = %d, %s", status, stderr.String())
	}
	invalid := writeTestFile(t, outputDirectory, "invalid-tooling.json", []byte(`{"profile":"tooling.workflow-1"}`))
	if status := run([]string{"conformance", "--fixture", invalid}, &stdout, &stderr); status != exitDiagnostic {
		t.Fatalf("invalid conformance = %d", status)
	}
}

func TestMockCommandIsDeterministicAndUsesSafeStableFailures(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	documentPath := writeTestFile(t, directory, "operation.json", []byte(cliDocument))
	schemaPath := writeTestFile(t, directory, "schema.json", []byte(cliSchema))
	arguments := []string{"mock", "--schema", schemaPath, "--document", documentPath, "--operation", "GetAccount", "--seed", "42", "--scenario", "success"}
	var first, second, diagnostic bytes.Buffer
	if status := run(arguments, &first, &diagnostic); status != exitOK {
		t.Fatalf("first mock = %d, %s", status, diagnostic.String())
	}
	if status := run(arguments, &second, &diagnostic); status != exitOK || first.String() != second.String() {
		t.Fatalf("second mock = %d, deterministic=%t, %s", status, first.String() == second.String(), diagnostic.String())
	}
	for _, required := range []string{`"profile":"naatre.schema-mock-1"`, `"schemaRevision":"tooling-cli-r1"`, `"schemaDigest":"`, `"documentDigest":"`, `"seed":42`} {
		if !bytes.Contains(first.Bytes(), []byte(required)) {
			t.Errorf("mock output omits %q: %s", required, first.String())
		}
	}
	diagnostic.Reset()
	secretScenario := "Bearer-secret-scenario"
	bad := []string{"mock", "--schema", schemaPath, "--document", documentPath, "--operation", "GetAccount", "--seed", "42", "--scenario", secretScenario}
	if status := run(bad, &second, &diagnostic); status != exitDiagnostic || bytes.Contains(diagnostic.Bytes(), []byte(secretScenario)) || !bytes.Contains(diagnostic.Bytes(), []byte("TOOL_COMMAND_FAILED")) {
		t.Fatalf("safe mock failure = %d, %s", status, diagnostic.String())
	}
}

func writeTestFile(t testing.TB, directory, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
