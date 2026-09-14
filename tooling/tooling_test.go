package tooling_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
	"github.com/valksor/naatre/tooling"
)

const operationDocument = `{
  "operations": [{
    "name": "GetAccount",
    "kind": "query",
    "select": [{"$call": {"select": [{"$field": {"name": "name"}}, {"$field": {"name": "id"}}], "args": {"id": {"$literal": "acct-1"}}, "name": "account"}}]
  }]
}`

func TestCLIAndEditorShareExactDiagnostics(t *testing.T) {
	t.Parallel()
	invalid := []byte(`{"operations":[{"name":"bad name","kind":"query","select":[]}]}`)
	want := tooling.ValidateDocument(invalid, tooling.ValidateOptions{})
	got := (tooling.EditorAdapter{}).Diagnostics(invalid, "")
	if len(want.Diagnostics) != 1 || !equalDiagnostics(want, got) {
		t.Fatalf("CLI/editor diagnostics differ: %#v %#v", want, got)
	}
	diagnostic := got.Diagnostics[0]
	if diagnostic.Phase != "validate" || diagnostic.Code != "INVALID_IDENTIFIER" || diagnostic.Path != "/operations/0/name" {
		t.Fatalf("diagnostic phase/code/path = %#v", diagnostic)
	}
}

func TestFormatAndHashPreserveIdentityAndSelectionOrder(t *testing.T) {
	t.Parallel()
	formatted, err := tooling.FormatDocument([]byte(operationDocument))
	if err != nil {
		t.Fatal(err)
	}
	before, err := tooling.HashDocument([]byte(operationDocument))
	if err != nil {
		t.Fatal(err)
	}
	after, err := tooling.HashDocument(formatted)
	if err != nil || before != after {
		t.Fatalf("format/hash identity changed: %#v %#v %v", before, after, err)
	}
	reordered := strings.Replace(operationDocument,
		`{"$field": {"name": "name"}}, {"$field": {"name": "id"}}`,
		`{"$field": {"name": "id"}}, {"$field": {"name": "name"}}`, 1)
	changed, err := tooling.HashDocument([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if before == changed {
		t.Fatal("selection reordering did not change document identity")
	}
}

func TestFormatHashRoundTripsCanonicalDocumentFixtures(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "canonical.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Equivalence []struct {
			Name    string               `json:"name"`
			Purpose protocol.HashPurpose `json:"purpose"`
			Left    string               `json:"left"`
			Right   string               `json:"right"`
			Equal   bool                 `json:"equal"`
		} `json:"equivalenceVectors"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	covered := 0
	for _, vector := range fixture.Equivalence {
		if vector.Purpose != protocol.DocumentHash {
			continue
		}
		t.Run(vector.Name, func(t *testing.T) {
			left := formatHash(t, vector.Left)
			right := formatHash(t, vector.Right)
			if (left == right) != vector.Equal {
				t.Fatalf("formatted digest equality = %t, want %t", left == right, vector.Equal)
			}
		})
		covered++
	}
	if covered < 5 {
		t.Fatalf("canonical document fixture coverage = %d", covered)
	}
}

func formatHash(t testing.TB, input string) protocol.Digest {
	t.Helper()
	before, err := tooling.HashDocument([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := tooling.FormatDocument([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	after, err := tooling.HashDocument(formatted)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("format changed identity: %#v %#v", before, after)
	}
	return after
}

func TestToolingInspectionInvokesZeroBusinessHandlers(t *testing.T) {
	t.Parallel()
	schemaDocument := testSchema(t)
	types, err := schemaDocument.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationAllowByDefault,
		PlanningAuthorizer: runtime.PlanningAuthorizerFunc(func(runtime.PlanningAuthorizationRequest) (runtime.AuthorizationDecision, error) {
			calls.Add(1)
			return runtime.AuthorizationDecision{Allowed: true}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	metadata := runtime.Metadata{
		Effect: runtime.ReadEffect, Deterministic: true, Cacheable: true, RetrySafe: true,
		ThreadSafety: runtime.ThreadSafe, Batching: runtime.BatchIneligible, Transaction: runtime.TransactionNone,
		AuthorizationPolicy: "public", Cost: 1,
	}
	if err := registry.Register(runtime.Bind[schema.InputValue, map[string]any](runtime.Descriptor{
		ID: "query.account", Name: "account", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: "User", Metadata: metadata,
	}, func(context.Context, schema.InputValue) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"id": "acct-1", "name": "Ada"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"name", "id"} {
		if err := registry.Register(runtime.BindField[map[string]any, string](runtime.Descriptor{
			ID: "User." + name + ".resolver", Name: name, Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
			Output: schema.TypeID(schema.String), Metadata: metadata,
		}, func(_ context.Context, source map[string]any) (string, error) {
			calls.Add(1)
			return source[name].(string), nil
		})); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	explain := tooling.ExplainWithSnapshot(snapshot, []byte(operationDocument), "GetAccount")
	if len(explain.Rejections) != 0 || explain.Resolved == nil || explain.EstimatedCost == 0 {
		t.Fatalf("explain report = %#v", explain)
	}
	if _, err := snapshot.ExportSchema(schemaDocument.ExportOptions()); err != nil {
		t.Fatal(err)
	}
	editor := tooling.EditorAdapter{Schema: &schemaDocument}
	if len(editor.Completions("acc")) == 0 {
		t.Fatal("editor returned no schema completions")
	}
	if warnings := editor.DeprecationWarnings([]byte(operationDocument)); len(warnings) != 1 || warnings[0].Code != "DEPRECATED" {
		t.Fatalf("deprecation warnings = %#v", warnings)
	}
	if _, err := tooling.GenerateMock(schemaDocument, []byte(operationDocument), "GetAccount", 7, tooling.MockSuccess); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("tool inspection invoked %d business handlers", calls.Load())
	}
	planning, err := runtime.NewPlanningSnapshot(schemaDocument)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planning.InvokeRoot(context.Background(), protocol.Query, "account", schema.InputValue{}); !errors.Is(err, runtime.ErrPlanningOnly) {
		t.Fatalf("planning invocation error = %v", err)
	}
}

func TestSeededMocksAreIdenticalAndRepresentFailureModes(t *testing.T) {
	t.Parallel()
	schemaDocument := testSchema(t)
	first, err := tooling.GenerateMock(schemaDocument, []byte(operationDocument), "GetAccount", 42, tooling.MockSuccess)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tooling.GenerateMock(schemaDocument, []byte(operationDocument), "GetAccount", 42, tooling.MockSuccess)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(first)
	right, _ := json.Marshal(second)
	if !bytes.Equal(left, right) {
		t.Fatal("same seed produced different mock bytes")
	}
	partial, err := tooling.GenerateMock(schemaDocument, []byte(operationDocument), "GetAccount", 42, tooling.MockFailure)
	if err != nil || len(partial.Errors) != 1 || len(partial.Errors[0].Path) == 0 {
		t.Fatalf("partial mock = %#v, %v", partial, err)
	}
	unknown, err := tooling.GenerateMock(schemaDocument, []byte(operationDocument), "GetAccount", 42, tooling.MockUnknownVariant)
	if err != nil || !strings.Contains(string(mustJSON(t, unknown.Data)), "UnknownVariant") {
		t.Fatalf("unknown mock = %#v, %v", unknown, err)
	}
	for _, scenario := range []tooling.MockScenario{tooling.MockNull, tooling.MockMissing} {
		if _, err := tooling.GenerateMock(schemaDocument, []byte(operationDocument), "GetAccount", 42, scenario); err != nil {
			t.Fatalf("scenario %s: %v", scenario, err)
		}
	}
}

func TestExplainRedactsUnauthorizedSchemaNamesAndArguments(t *testing.T) {
	t.Parallel()
	schemaDocument := testSchema(t)
	report := tooling.ExplainWithOptions(schemaDocument, []byte(operationDocument), "GetAccount", tooling.ExplainOptions{
		VisibleIDs: map[string]bool{"query.account": true, "User.id.resolver": true},
	})
	encoded := string(mustJSON(t, report))
	if strings.Contains(encoded, `"name":"name"`) || strings.Contains(encoded, "acct-1") || strings.Contains(encoded, `"authorizationPolicy":"public"`) {
		t.Fatalf("explain leaked hidden schema or argument data: %s", encoded)
	}
	if !strings.Contains(encoded, "redacted") {
		t.Fatalf("explain did not mark redacted content: %s", encoded)
	}
	invalid := strings.Replace(operationDocument, `"name": "name"`, `"name": "missing"`, 1)
	rejected := tooling.ExplainWithOptions(schemaDocument, []byte(invalid), "GetAccount", tooling.ExplainOptions{
		VisibleIDs: map[string]bool{"query.account": true},
	})
	rejectedJSON := string(mustJSON(t, rejected))
	if len(rejected.Rejections) == 0 || strings.Contains(rejectedJSON, "User") {
		t.Fatalf("rejected explain leaked a hidden schema type: %s", rejectedJSON)
	}
}

func TestEditorFragmentDefinitionAndStructuralRename(t *testing.T) {
	t.Parallel()
	document := []byte(`{"operations":[{"name":"WithFragment","kind":"query","select":[{"$fragment":{"name":"Shared"}},{"$call":{"name":"echo","args":{"value":{"$literal":"Shared"}}}}]}],"fragments":[{"name":"Shared","select":[]}]}`)
	source, found, err := (tooling.EditorAdapter{}).FragmentDefinition(document, "Shared")
	if err != nil || !found || source.Pointer != "/fragments/0" {
		t.Fatalf("fragment definition = %#v, %t, %v", source, found, err)
	}
	edits, err := (tooling.EditorAdapter{}).RenameEdits(document, "Shared", "Renamed")
	if err != nil || len(edits) != 2 {
		t.Fatalf("fragment rename edits = %#v, %v", edits, err)
	}
	for _, edit := range edits {
		if strings.Contains(edit.Path, "/args/") {
			t.Fatalf("rename touched a literal argument: %#v", edits)
		}
	}
}

func TestCredentialRedactionCoversSavedSurfaces(t *testing.T) {
	t.Parallel()
	surfaces := []string{
		`{"url":"https://user:password@example.test/run?token=secret-token","authorization":"Bearer secret-bearer","variables":{"safe":true}}`,
		`GET https://example.test/play?api_key=secret-key Authorization: Bearer secret-log`,
		`curl -H 'Authorization: Bearer secret-snippet' https://example.test/run?access_token=secret-url`,
		`Cookie: session=secret-cookie`,
		`X-API-Key: secret-header password=secret-password`,
		`GET https://example.test/play?client_secret=secret-client`,
	}
	for _, surface := range surfaces {
		redacted := tooling.RedactCredentials(surface)
		for _, secret := range []string{"secret-token", "secret-bearer", "secret-key", "secret-log", "secret-snippet", "secret-url", "secret-cookie", "secret-header", "secret-password", "secret-client"} {
			if strings.Contains(redacted, secret) {
				t.Fatalf("redaction leaked %q in %q", secret, redacted)
			}
		}
	}
}

func TestManifestCompatibilityChecksRegisteredDocuments(t *testing.T) {
	t.Parallel()
	schemaDocument := testSchema(t)
	manifest, err := tooling.BuildManifest([]byte(operationDocument))
	if err != nil {
		t.Fatal(err)
	}
	report := tooling.CheckCompatibility(schemaDocument, manifest)
	if !report.Compatible || report.Operations != 1 || report.Schema.Hex == "" {
		t.Fatalf("compatibility = %#v", report)
	}
	broken := manifest
	broken.Operations[0].Document = json.RawMessage(strings.Replace(operationDocument, `"name": "name"`, `"name": "removed"`, 1))
	document, err := protocol.DecodeDocument(broken.Operations[0].Document, protocol.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	broken.Operations[0].Persisted, err = protocol.SemanticHash(protocol.DocumentHash, document.CanonicalJSON())
	if err != nil {
		t.Fatal(err)
	}
	report = tooling.CheckCompatibility(schemaDocument, broken)
	if report.Compatible || len(report.Diagnostics) == 0 || report.Diagnostics[0].Code != "UNKNOWN_FIELD" {
		t.Fatalf("incompatible report = %#v", report)
	}
}

func TestConfigPrecedenceAndTrustedPluginBoundary(t *testing.T) {
	t.Parallel()
	resolved := tooling.ResolveConfig(
		tooling.Config{Schema: "default"}, tooling.Config{Schema: "file"},
		tooling.Config{Schema: "environment"}, tooling.Config{Schema: "flag"},
	)
	if resolved.Schema != "flag" {
		t.Fatalf("schema precedence = %q", resolved.Schema)
	}
	untrusted := tooling.PluginConfig{Path: "/plugins/gen", ID: "vendor.gen", Version: "1", SHA256: strings.Repeat("a", 64)}
	if untrusted.Validate() == nil {
		t.Fatal("untrusted plugin was accepted")
	}
	untrusted.Trusted = true
	if err := untrusted.Validate(); err != nil {
		t.Fatalf("pinned trusted plugin rejected: %v", err)
	}
	untrusted.SHA256 = strings.Repeat("z", 64)
	if untrusted.Validate() == nil {
		t.Fatal("non-hexadecimal plugin digest was accepted")
	}
}

func equalDiagnostics(left, right tooling.DiagnosticReport) bool {
	return string(mustJSONNoTest(left)) == string(mustJSONNoTest(right))
}

func mustJSON(t testing.TB, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustJSONNoTest(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func testSchema(t testing.TB) schema.Document {
	t.Helper()
	input := []byte(`{
      "version":"1","canonicalVersion":"c14n-1","revision":"tooling-r1",
      "types":[
        {"id":"LookupInput","name":"LookupInput","kind":"input-object","input":true,"fields":[{"id":"LookupInput.id","name":"id","type":"ID","required":true}]},
        {"id":"User","name":"User","kind":"object","output":true,"fields":[{"id":"User.id","name":"id","type":"String"},{"id":"User.name","name":"name","type":"String","description":"Display name","deprecation":{"reason":"use displayName","replacement":"displayName"}}]}
      ],
	      "operations":[{"id":"query.account","name":"account","kind":"query","input":"LookupInput","output":"User","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1,"source":{"uri":"file:///schema.naatre.json","line":10}}],
      "members":[
	        {"id":"User.id.resolver","name":"id","owner":"User","kind":"field","output":"String","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1},
	        {"id":"User.name.resolver","name":"name","owner":"User","kind":"field","output":"String","description":"Display name","deprecation":{"reason":"use displayName","replacement":"displayName"},"effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1}
      ]
    }`)
	document, err := schema.ParseDocument(input, schema.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return document
}
