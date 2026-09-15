package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/valksor/naatre/schema"
)

const testDocument = `{
  "operations": [{
    "name": "GetAccount",
    "kind": "query",
    "select": [{"$call": {"name": "account", "args": {"id": {"$literal": "secret-value"}}, "select": [{"$field": {"name": "name"}}]}}]
  }]
}`

func TestProtocolFixturesPositiveEditorSurface(t *testing.T) {
	server := testServer(t, Limits{})
	initializeServer(t, server)
	opened := server.handle(context.Background(), request("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": "file:///document.naatre.json", "languageId": "naatre", "version": 1, "text": testDocument, "schemaRevision": "schema-r1"},
	}))
	if opened.err != nil || len(opened.notifications) != 1 {
		t.Fatalf("open = %#v", opened)
	}

	namePosition := positionOf(t, testDocument, `"name": "name"`, len(`"name": "na`))
	completion := server.handle(context.Background(), request("textDocument/completion", documentPosition("file:///document.naatre.json", namePosition.Line, namePosition.Character)))
	if completion.err != nil || !strings.Contains(string(mustJSON(completion.result)), `"label":"name"`) {
		t.Fatalf("completion = %#v", completion)
	}
	hoverPosition := positionOf(t, testDocument, `"name": "name"`, len(`"name": "nam`))
	hover := server.handle(context.Background(), request("textDocument/hover", documentPosition("file:///document.naatre.json", hoverPosition.Line, hoverPosition.Character)))
	if hover.err != nil || !strings.Contains(string(mustJSON(hover.result)), "Display name") {
		t.Fatalf("hover = %#v", hover)
	}
	definitionPosition := positionOf(t, testDocument, `"name": "account"`, len(`"name": "acc`))
	definition := server.handle(context.Background(), request("textDocument/definition", documentPosition("file:///document.naatre.json", definitionPosition.Line, definitionPosition.Character)))
	if definition.err != nil || !strings.Contains(string(mustJSON(definition.result)), "schema.naatre.json") {
		t.Fatalf("definition = %#v", definition)
	}
	rename := server.handle(context.Background(), request("textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": "file:///document.naatre.json"}, "position": hoverPosition, "newName": "displayName",
	}))
	if rename.err != nil || !strings.Contains(string(mustJSON(rename.result)), `"newText":"displayName"`) || strings.Contains(string(mustJSON(rename.result)), "secret-value") {
		t.Fatalf("rename = %#v", rename)
	}
	diagnostics := server.handle(context.Background(), request("textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": "file:///document.naatre.json"}}))
	if diagnostics.err != nil || !strings.Contains(string(mustJSON(diagnostics.result)), `"kind":"full"`) {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestIncrementalEditsPinSchemaAndIncreaseVersion(t *testing.T) {
	server := testServer(t, Limits{})
	initializeServer(t, server)
	openDocument(t, server, testDocument)

	stale := server.handle(context.Background(), request("textDocument/didChange", changeParams(1, "schema-r1", []contentChange{{Text: testDocument}})))
	if stale.err == nil || stale.err.Data.Code != CodeVersionStale {
		t.Fatalf("stale change = %#v", stale)
	}
	mixed := server.handle(context.Background(), request("textDocument/didChange", changeParams(2, "schema-r2", []contentChange{{Text: testDocument}})))
	if mixed.err == nil || mixed.err.Data.Code != CodeSchemaMismatch || len(mixed.notifications) != 1 {
		t.Fatalf("mixed schema change = %#v", mixed)
	}
	operationPosition := positionOf(t, testDocument, "GetAccount", 0)
	accepted := server.handle(context.Background(), request("textDocument/didChange", changeParams(2, "schema-r1", []contentChange{{
		Range: &Range{Start: operationPosition, End: Position{Line: operationPosition.Line, Character: operationPosition.Character + len("GetAccount")}}, Text: "FetchAccount",
	}})))
	if accepted.err != nil {
		t.Fatalf("incremental change = %#v", accepted)
	}
	doc, failure := server.document("file:///document.naatre.json")
	if failure != nil || doc.version != 2 || !strings.Contains(doc.text, "FetchAccount") || doc.schemaRevision != "schema-r1" {
		t.Fatalf("updated document = %#v, %#v", doc, failure)
	}
}

func TestDiagnosticsHaveStableCodesRangesAndSafeMessages(t *testing.T) {
	server := testServer(t, Limits{})
	initializeServer(t, server)
	invalid := `{"operations":[{"name":"bad name","kind":"query","select":[{"$field":{"name":"credential-value"}}]}]}`
	openDocument(t, server, invalid)
	doc, _ := server.document("file:///document.naatre.json")
	diagnostics := server.diagnostics(doc)
	if len(diagnostics) == 0 {
		t.Fatal("expected diagnostic")
	}
	encoded := string(mustJSON(diagnostics))
	if !strings.Contains(encoded, `"code":"INVALID_IDENTIFIER"`) || !strings.Contains(encoded, `"start":{"line":0,"character":24}`) {
		t.Fatalf("diagnostics lack stable code/range: %s", encoded)
	}
	for _, forbidden := range []string{"credential-value", "bad name", "github.com/", "runtime."} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("diagnostic exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestCancellationAndResourceLimitsUsePublicCodes(t *testing.T) {
	server := testServer(t, Limits{MaxOpenDocuments: 1, MaxDocumentBytes: 64, MaxChangesPerUpdate: 1})
	initializeServer(t, server)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result := server.handle(cancelled, request("textDocument/hover", documentPosition("file:///missing", 0, 0)))
	if result.err == nil || result.err.Data.Code != CodeCancelled || result.err.Message != "request cancelled" {
		t.Fatalf("cancelled result = %#v", result)
	}
	oversized := strings.Repeat("x", 65)
	opened := server.handle(context.Background(), request("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": "file:///large", "version": 1, "text": oversized},
	}))
	if opened.err == nil || opened.err.Data.Code != CodeResourceLimit {
		t.Fatalf("oversized open = %#v", opened)
	}
}

func TestInternalFailureIsContainedBehindStableCode(t *testing.T) {
	server := testServer(t, Limits{})
	initializeServer(t, server)
	server.beforeRequest = func(context.Context, string) error { panic("implementation-only detail") }
	result := server.handleSafe(context.Background(), requestWithID(2, "textDocument/hover", documentPosition("file:///missing", 0, 0)))
	if result.err == nil || result.err.Data.Code != CodeInternal || result.err.Message != "internal server failure" || strings.Contains(result.err.Error(), "implementation-only") {
		t.Fatalf("contained failure = %#v", result)
	}
}

func TestRenameRejectsLiteralAndInvalidTargets(t *testing.T) {
	server := testServer(t, Limits{})
	initializeServer(t, server)
	openDocument(t, server, testDocument)
	literal := positionOf(t, testDocument, "secret-value", 3)
	prepared := server.handle(context.Background(), request("textDocument/prepareRename", documentPosition("file:///document.naatre.json", literal.Line, literal.Character)))
	if prepared.err == nil || prepared.err.Data.Code != CodeRenameInvalid {
		t.Fatalf("literal prepare rename = %#v", prepared)
	}
	renamed := server.handle(context.Background(), request("textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": "file:///document.naatre.json"}, "position": literal, "newName": "replacement",
	}))
	if renamed.err == nil || renamed.err.Data.Code != CodeRenameInvalid {
		t.Fatalf("literal rename = %#v", renamed)
	}
}

func TestUTF16IncrementalRangesAndJSONPointers(t *testing.T) {
	updated, err := applyChanges("a😀b", []contentChange{{Range: &Range{Start: Position{Character: 1}, End: Position{Character: 3}}, Text: "x"}}, 10)
	if err != nil || updated != "axb" {
		t.Fatalf("UTF-16 edit = %q, %v", updated, err)
	}
	if _, err := byteOffset("a😀b", Position{Character: 2}); err == nil {
		t.Fatal("surrogate-splitting position accepted")
	}
	index := indexJSON("{\n  \"name\": \"Shared\"\n}")
	if got := index.ranges["/name"]; got.Start != (Position{Line: 1, Character: 11}) || got.End != (Position{Line: 1, Character: 17}) {
		t.Fatalf("JSON pointer range = %#v", got)
	}
}

func TestFramingRejectsOversizedMessagesAndContainsCancellation(t *testing.T) {
	server := testServer(t, Limits{MaxMessageBytes: 1024})
	server.beforeRequest = func(ctx context.Context, method string) error {
		if method != "textDocument/hover" {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}
	var input bytes.Buffer
	writeTestFrame(&input, requestWithID(1, "initialize", map[string]any{}))
	writeTestFrame(&input, requestWithID(2, "textDocument/hover", documentPosition("file:///missing", 0, 0)))
	writeTestFrame(&input, request("$/cancelRequest", map[string]any{"id": 2}))
	var output bytes.Buffer
	if err := server.Serve(context.Background(), &input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), CodeCancelled) {
		t.Fatalf("cancellation response = %q", output.String())
	}
	oversized := bytes.NewBufferString("Content-Length: 5\r\n\r\n12345")
	_, failure, err := readFrame(bufioReader(oversized), 4)
	if err != nil || failure == nil || failure.Data.Code != CodeResourceLimit {
		t.Fatalf("oversized frame = %#v, %v", failure, err)
	}
}

func testServer(t *testing.T, limits Limits) *Server {
	t.Helper()
	document, err := schema.ParseDocument([]byte(testSchema), schema.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(&document, limits)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func initializeServer(t *testing.T, server *Server) {
	t.Helper()
	result := server.handle(context.Background(), requestWithID(1, "initialize", map[string]any{"initializationOptions": map[string]any{"schemaRevision": "schema-r1"}}))
	if result.err != nil {
		t.Fatal(result.err)
	}
}

func openDocument(t *testing.T, server *Server, text string) {
	t.Helper()
	result := server.handle(context.Background(), request("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": "file:///document.naatre.json", "version": 1, "text": text, "schemaRevision": "schema-r1"},
	}))
	if result.err != nil {
		t.Fatal(result.err)
	}
}

func request(method string, params any) message { return requestWithID(0, method, params) }

func requestWithID(id int, method string, params any) message {
	result := message{JSONRPC: "2.0", Method: method, Params: mustJSON(params)}
	if id != 0 {
		result.ID = json.RawMessage(strconv.Itoa(id))
	}
	return result
}

func documentPosition(uri string, line, character int) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": line, "character": character}}
}

func changeParams(version int, revision string, changes []contentChange) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": "file:///document.naatre.json", "version": version, "schemaRevision": revision}, "contentChanges": changes}
}

func positionOf(t *testing.T, text, fragment string, within int) Position {
	t.Helper()
	offset := strings.Index(text, fragment)
	if offset < 0 {
		t.Fatalf("fragment %q not found", fragment)
	}
	return positionAt(text, offset+within)
}

func writeTestFrame(output *bytes.Buffer, value message) {
	payload := mustJSON(value)
	fmt.Fprintf(output, "Content-Length: %d\r\n\r\n", len(payload))
	output.Write(payload)
}

func bufioReader(input *bytes.Buffer) *bufio.Reader { return bufio.NewReader(input) }

const testSchema = `{
  "version":"1","canonicalVersion":"c14n-1","revision":"schema-r1",
  "types":[
    {"id":"LookupInput","name":"LookupInput","kind":"input-object","input":true,"fields":[{"id":"LookupInput.id","name":"id","type":"ID","required":true}]},
    {"id":"User","name":"User","kind":"object","output":true,"fields":[{"id":"User.name","name":"name","type":"String","description":"Display name"}]}
  ],
  "operations":[{"id":"query.account","name":"account","kind":"query","input":"LookupInput","output":"User","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1,"source":{"uri":"file:///schema.naatre.json","line":7,"column":3}}],
  "members":[{"id":"User.name.resolver","name":"name","owner":"User","kind":"field","output":"String","description":"Display name","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1}]
}`
