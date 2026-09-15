package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valksor/naatre/asyncapi"
	"github.com/valksor/naatre/schema"
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

const cliJTDSchema = `{
  "version":"1","canonicalVersion":"c14n-1","revision":"cli-jtd-r1",
  "types":[{"id":"Node","name":"Node","kind":"object","output":true,"maxDepth":3,"fields":[{"id":"Node.name","name":"name","type":"String","required":true},{"id":"Node.next","name":"next","type":"Node","nullable":true}]}],
  "operations":[],"members":[]
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

func TestJTDCLIUsesSharedGenerationValidationImportAndDiff(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	schemaPath := writeTestFile(t, directory, "schema.json", []byte(cliJTDSchema))
	reportPath := filepath.Join(directory, "fidelity.json")
	var output, diagnostic bytes.Buffer
	if status := run([]string{"schema", "jtd", "export", "--schema", schemaPath, "--root", "Node", "--report", reportPath}, &output, &diagnostic); status != exitOK {
		t.Fatalf("JTD export = %d, %s", status, diagnostic.String())
	}
	projection := append([]byte(nil), output.Bytes()...)
	if len(projection) == 0 {
		t.Fatal("JTD export is empty")
	}
	var report struct {
		Exact   bool `json:"exact"`
		Binding struct {
			Mapper string `json:"mapperRevision"`
		} `json:"binding"`
	}
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil || json.Unmarshal(reportBytes, &report) != nil || !report.Exact || report.Binding.Mapper == "" {
		t.Fatalf("JTD report = %s, %v", reportBytes, err)
	}
	jtdPath := writeTestFile(t, directory, "schema.jtd.json", projection)
	output.Reset()
	if status := run([]string{"schema", "jtd", "validate", "--jtd", jtdPath, "--approve-embedded-identities"}, &output, &diagnostic); status != exitOK {
		t.Fatalf("JTD validate = %d, %s, %s", status, output.String(), diagnostic.String())
	}
	output.Reset()
	if status := run([]string{"schema", "jtd", "import", "--jtd", jtdPath, "--approve-embedded-identities"}, &output, &diagnostic); status != exitOK {
		t.Fatalf("JTD import = %d, %s", status, diagnostic.String())
	}
	want, err := tooling.ExportSchema([]byte(cliJTDSchema))
	if err != nil || !bytes.Equal(bytes.TrimSpace(output.Bytes()), want) {
		t.Fatalf("JTD imported schema differs:\n%s\n%s\n%v", output.Bytes(), want, err)
	}
	output.Reset()
	if status := run([]string{"schema", "jtd", "diff", "--before", jtdPath, "--after", jtdPath, "--approve-embedded-identities"}, &output, &diagnostic); status != exitOK || !bytes.Contains(output.Bytes(), []byte(`"changes":null`)) {
		t.Fatalf("JTD diff = %d, %s, %s", status, output.String(), diagnostic.String())
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

func TestAsyncAPICommandsConsumeOneOfflineModel(t *testing.T) {
	t.Parallel()
	schemaDocument, err := schema.ParseDocument([]byte(cliSchema), schema.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	model := asyncapi.Model{
		ID: "urn:naatre:cli:events", Title: "CLI events", Version: "1.0.0", Schema: schemaDocument,
		CapabilityRevision: "cli-events-1", EventEnvelopeRevision: "core.events-1",
		Servers:    []asyncapi.Server{{ID: "events", NaatreID: "server.events", Revision: "server-1", URL: "https://events.example/v1", Protocol: "https"}},
		Bindings:   []asyncapi.Binding{{ID: "sse", NaatreID: "binding.sse", Revision: "sse-1", Transport: asyncapi.TransportSSE, Server: "events", Implemented: true, Evidence: []string{"cli-offline-test"}}},
		Messages:   []asyncapi.Message{{ID: "UserEvent", NaatreID: "event.user", Revision: "event-1", PayloadType: "User", ContentType: "application/json", Correlations: []asyncapi.Correlation{{ID: "request-id", Location: "$message.header#/x-naatre-request-id", Lifetime: "request", Trust: "untrusted"}}}},
		Channels:   []asyncapi.Channel{{ID: "users", NaatreID: "channel.users", Revision: "channel-1", Address: "/users", Messages: []string{"UserEvent"}, Bindings: []string{"sse"}, Semantics: asyncapi.Semantics{Ordering: "sequence", Replay: "cursor", Terminal: "complete", Errors: "stream-error"}}},
		Operations: []asyncapi.Operation{{ID: "receiveUsers", NaatreID: "operation.users", Revision: "operation-1", Action: asyncapi.ActionReceive, Channel: "users", Messages: []string{"UserEvent"}, Security: []string{"bearer"}}},
		Security:   []asyncapi.SecurityScheme{{ID: "bearer", NaatreID: "security.bearer", Revision: "security-1", Type: "http", Scheme: "bearer"}},
	}
	exported, _, err := asyncapi.Export(model)
	if err != nil {
		t.Fatal(err)
	}
	path := writeTestFile(t, t.TempDir(), "events.asyncapi.json", exported)
	for _, command := range []string{"validate", "export", "inspect", "mock"} {
		var output, diagnostic bytes.Buffer
		arguments := []string{"asyncapi", command, "--document", path, "--allow-server", "https://events.example/v1"}
		if command == "mock" {
			arguments = append(arguments, "--seed", "13")
		}
		if status := run(arguments, &output, &diagnostic); status != exitOK || output.Len() == 0 || diagnostic.Len() != 0 {
			t.Fatalf("asyncapi %s = %d, %s, %s", command, status, output.String(), diagnostic.String())
		}
	}
	var output, diagnostic bytes.Buffer
	if status := run([]string{"asyncapi", "validate", "--document", path}, &output, &diagnostic); status != exitDiagnostic || !bytes.Contains(diagnostic.Bytes(), []byte("ASYNCAPI_SERVER_DENIED")) {
		t.Fatalf("default-deny validation = %d, %s", status, diagnostic.String())
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
