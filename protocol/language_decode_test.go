package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestDecodeDocumentPreservesCompleteLanguageAST(t *testing.T) {
	t.Parallel()

	input := []byte(`{"requires":["vendor.audit-1","core.language-1"],"operations":[{"name":"Everything","kind":"query","variables":[{"name":"id","type":"app.ID-1","required":true,"nullable":false,"default":{"$var":"inert"}}],"select":[{"$field":{"name":"field","as":"fieldAlias","bind":"fieldResult","directives":[{"name":"include","arguments":{"if":{"$literal":true}}}],"select":[]}},{"$call":{"name":"call","as":"callAlias","bind":"callResult","args":{"literal":{"$literal":{"$var":"inert","n":90071992547409931234567890.123456789}},"variable":{"$var":"id"},"parent":{"$parent":true},"current":{"$current":true},"result":{"$result":"fieldResult"}},"select":[]}},{"$pipeline":{"as":"pipe","bind":"pipeResult","stages":[{"$field":{"name":"field"}},{"$call":{"name":"call","args":{"id":{"$var":"id"}}}},{"$map":{"select":[]}},{"$index":{"at":1.0}},{"$slice":{"start":1e0,"end":2.0}},{"$page":{"first":2,"after":{"$literal":"cursor"}}},{"$meta":{"name":"count"}},{"$current":{}}]}},{"$map":{"as":"mapped","select":[]}},{"$index":{"as":"indexed","at":0}},{"$slice":{"as":"sliced","start":0,"end":2}},{"$page":{"as":"paged","last":3,"before":{"$literal":"cursor"}}},{"$meta":{"name":"count","as":"total"}},{"$parallel":{"policy":"fail-fast","select":[]}},{"$fragment":{"name":"Identity"}},{"$current":{"as":"raw"}},{"$nest":{"as":"nested","select":[]}},{"$unnest":{"select":[]}}]}],"fragments":[{"name":"Identity","on":"app.User-1","select":[{"$field":{"name":"id"}}]}]}`)

	document, err := protocol.DecodeDocument(input, protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument: %v", err)
	}
	assertSourceRange(t, input, document.Source(), "")
	if got := document.Requires(); !slices.Equal(got, []string{"vendor.audit-1", "core.language-1"}) {
		t.Fatalf("Requires() = %v", got)
	}
	assertSourceRange(t, input, document.Source(), "")
	operations := document.Operations()
	if len(operations) != 1 || operations[0].Name() != "Everything" || operations[0].Kind() != protocol.Query {
		t.Fatalf("Operations() = %#v", operations)
	}
	variables := operations[0].Variables()
	if len(variables) != 1 || variables[0].Name() != "id" || variables[0].Type() != "app.ID-1" || !variables[0].Required() || variables[0].Nullable() {
		t.Fatalf("Variables() = %#v", variables)
	}
	if raw, ok := variables[0].Default(); !ok || string(raw) != `{"$var":"inert"}` {
		t.Fatalf("Default() = %s, %t", raw, ok)
	}
	fragments := document.Fragments()
	if len(fragments) != 1 || fragments[0].Name() != "Identity" || fragments[0].TypeCondition() != "app.User-1" {
		t.Fatalf("Fragments() = %#v", fragments)
	}

	selections := operations[0].Selections()
	wantKinds := []protocol.SelectionKind{
		protocol.FieldSelection, protocol.CallSelection, protocol.PipelineSelection,
		protocol.MapSelection, protocol.IndexSelection, protocol.SliceSelection,
		protocol.PageSelection, protocol.MetaSelection, protocol.ParallelSelection,
		protocol.FragmentSelection, protocol.CurrentSelection, protocol.NestSelection,
		protocol.UnnestSelection,
	}
	if len(selections) != len(wantKinds) {
		t.Fatalf("selection count = %d, want %d", len(selections), len(wantKinds))
	}
	for index, want := range wantKinds {
		if selections[index].Kind() != want {
			t.Fatalf("selection %d kind = %q, want %q", index, selections[index].Kind(), want)
		}
	}
	if selections[0].Alias() != "fieldAlias" || selections[0].Bind() != "fieldResult" || len(selections[0].Directives()) != 1 {
		t.Fatalf("field payload was not preserved: %#v", selections[0])
	}
	arguments := selections[1].Arguments()
	wantExpressions := map[string]protocol.ExpressionKind{
		"literal": protocol.LiteralExpression, "variable": protocol.VariableExpression,
		"parent": protocol.ParentExpression, "current": protocol.CurrentExpression,
		"result": protocol.ResultExpression,
	}
	for name, want := range wantExpressions {
		if arguments[name].Kind() != want {
			t.Fatalf("argument %q kind = %q, want %q", name, arguments[name].Kind(), want)
		}
	}
	if raw, ok := arguments["literal"].Literal(); !ok || string(raw) != `{"$var":"inert","n":90071992547409931234567890.123456789}` {
		t.Fatalf("literal = %s, %t", raw, ok)
	}
	if arguments["variable"].Name() != "id" || arguments["result"].Name() != "fieldResult" {
		t.Fatalf("expression references were not preserved: %#v", arguments)
	}
	assertSourceRange(t, input, operations[0].Source(), "/operations/0")
	assertSourceRange(t, input, variables[0].Source(), "/operations/0/variables/0")
	assertSourceRange(t, input, selections[0].Directives()[0].Source(), "/operations/0/select/0/$field/directives/0")
	assertSourceRange(t, input, arguments["literal"].Source(), "/operations/0/select/1/$call/args/literal")
	assertSourceRange(t, input, fragments[0].Source(), "/fragments/0")
	stages := selections[2].Stages()
	wantStages := []protocol.SelectionKind{
		protocol.FieldSelection, protocol.CallSelection, protocol.MapSelection,
		protocol.IndexSelection, protocol.SliceSelection, protocol.PageSelection,
		protocol.MetaSelection, protocol.CurrentSelection,
	}
	for index, want := range wantStages {
		if stages[index].Kind() != want || stages[index].Alias() != "" {
			t.Fatalf("pipeline stage %d = %#v, want unaliased %q", index, stages[index], want)
		}
	}
	if at, ok := stages[3].At(); !ok || at.String() != "1" {
		t.Fatalf("decimal integer at = %v, %t", at, ok)
	}
	if start, ok := stages[4].Start(); !ok || start.String() != "1" {
		t.Fatalf("exponent integer start = %v, %t", start, ok)
	}
	if selections[8].Policy() != protocol.FailFastParallel {
		t.Fatalf("parallel policy = %q", selections[8].Policy())
	}
	if end, ok := selections[5].End(); !ok || end.String() != "2" {
		t.Fatalf("slice end = %v, %t", end, ok)
	}
	if last, ok := selections[6].Last(); !ok || last.String() != "3" {
		t.Fatalf("last = %v, %t", last, ok)
	}
	if before, ok := selections[6].Before(); !ok || before.Kind() != protocol.LiteralExpression {
		t.Fatalf("before = %#v, %t", before, ok)
	}
	if first, ok := stages[5].First(); !ok || first.String() != "2" {
		t.Fatalf("first = %v, %t", first, ok)
	}
	if after, ok := stages[5].After(); !ok || after.Kind() != protocol.LiteralExpression {
		t.Fatalf("after = %#v, %t", after, ok)
	}

	callStart := bytes.Index(input, []byte(`{"$call":{"name":"call","as":"callAlias"`))
	callSource := selections[1].Source()
	if callSource.Pointer != "/operations/0/select/1" || callSource.Start != callStart || callSource.Line != 1 || callSource.Column != callStart+1 || callSource.End <= callSource.Start || !json.Valid(input[callSource.Start:callSource.End]) {
		t.Fatalf("call source = %#v", callSource)
	}
	canonical, err := protocol.CanonicalizeDocument(input, protocol.Limits{})
	if err != nil || !bytes.Equal(document.CanonicalJSON(), canonical) {
		t.Fatalf("canonical AST = %s, canonical document = %s, error = %v", document.CanonicalJSON(), canonical, err)
	}
	marshaled, err := document.MarshalJSON()
	if err != nil || !bytes.Equal(marshaled, canonical) {
		t.Fatalf("MarshalJSON = %s, want %s (%v)", marshaled, canonical, err)
	}
	if viaEncodingJSON, marshalErr := json.Marshal(document); marshalErr != nil || !bytes.Equal(viaEncodingJSON, canonical) {
		t.Fatalf("encoding/json Marshal = %s, want %s (%v)", viaEncodingJSON, canonical, marshalErr)
	}
}

func TestDecodeDocumentPreservesFragmentParametersAndInlineConditions(t *testing.T) {
	t.Parallel()
	document, err := protocol.DecodeDocument([]byte(`{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"on":"User","select":[{"$field":{"name":"id"}}]}},{"$fragment":{"name":"Part","args":{"value":{"$literal":"bound"}}}}]}],"fragments":[{"name":"Part","parameters":[{"name":"value","type":"String","required":true,"default":"fallback"}],"select":[]}]}`), protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument: %v", err)
	}
	parameters := document.Fragments()[0].Parameters()
	if len(parameters) != 1 || parameters[0].Name() != "value" || parameters[0].Type() != "String" || !parameters[0].Required() {
		t.Fatalf("fragment parameters = %#v", parameters)
	}
	defaultValue, ok := parameters[0].Default()
	if !ok || string(defaultValue) != `"fallback"` {
		t.Fatalf("fragment default = %s, %v", defaultValue, ok)
	}
	selections := document.Operations()[0].Selections()
	if selections[0].Name() != "" || selections[0].TypeCondition() != "User" || len(selections[0].Selections()) != 1 {
		t.Fatalf("inline fragment = %#v", selections[0])
	}
	if selections[1].Name() != "Part" || selections[1].TypeCondition() != "" || selections[1].Arguments()["value"].Kind() != protocol.LiteralExpression {
		t.Fatalf("named spread = %#v", selections[1])
	}
}

func TestDecodeDocumentPreservesMutationAtomicityAndAtomicGroups(t *testing.T) {
	t.Parallel()
	document, err := protocol.DecodeDocument([]byte(`{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"account","select":[{"$call":{"name":"update"}}]}}]}]}`), protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument: %v", err)
	}
	operation := document.Operations()[0]
	if operation.Atomicity() != protocol.GroupAtomicity {
		t.Fatalf("Atomicity() = %q, want %q", operation.Atomicity(), protocol.GroupAtomicity)
	}
	group := operation.Selections()[0]
	if group.Kind() != protocol.AtomicSelection || group.Name() != "account" || len(group.Selections()) != 1 {
		t.Fatalf("atomic group = %#v", group)
	}
	if group.Selections()[0].Kind() != protocol.CallSelection {
		t.Fatalf("atomic child = %#v", group.Selections()[0])
	}
}

func TestDecodeDocumentRejectsInvalidMutationAtomicity(t *testing.T) {
	t.Parallel()
	assertDocumentDiagnostic(t, `{"operations":[{"name":"M","kind":"mutation","atomicity":"all","select":[]}]}`, "INVALID_ATOMICITY", "/operations/0/atomicity")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"M","kind":"mutation","atomicity":true,"select":[]}]}`, "INVALID_ATOMICITY", "/operations/0/atomicity")
}

func TestDecodeDocumentRejectsAtomicGroupBinding(t *testing.T) {
	t.Parallel()
	assertDocumentDiagnostic(t,
		`{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"account","bind":"result","select":[]}}]}]}`,
		"UNKNOWN_FIELD", "/operations/0/select/0/$atomic/bind")
}

func assertSourceRange(t *testing.T, input []byte, source protocol.Source, pointer string) {
	t.Helper()
	if source.Pointer != pointer || source.Start < 0 || source.End <= source.Start || source.End > len(input) || source.Line < 1 || source.Column < 1 || !json.Valid(input[source.Start:source.End]) {
		t.Fatalf("source for %s = %#v", pointer, source)
	}
}

func TestDecodedDocumentNestedAccessorsAreImmutable(t *testing.T) {
	t.Parallel()
	input := []byte(`{"requires":["core.language-1"],"operations":[{"name":"Immutable","kind":"query","variables":[{"name":"v","type":"String","default":{"safe":true}}],"select":[{"$pipeline":{"as":"value","stages":[{"$call":{"name":"load","args":{"value":{"$literal":{"safe":true}}},"directives":[{"name":"include","arguments":{"if":{"$literal":true}}}]}}]}}]}],"fragments":[{"name":"Part","select":[{"$field":{"name":"stable"}}]}]}`)
	document, err := protocol.DecodeDocument(input, protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument: %v", err)
	}

	requires := document.Requires()
	requires[0] = "changed"
	variable := document.Operations()[0].Variables()[0]
	defaultValue, _ := variable.Default()
	defaultValue[1] = 'X'
	stages := document.Operations()[0].Selections()[0].Stages()
	arguments := stages[0].Arguments()
	literal, _ := arguments["value"].Literal()
	literal[1] = 'X'
	delete(arguments, "value")
	directives := stages[0].Directives()
	directiveArguments := directives[0].Arguments()
	delete(directiveArguments, "if")
	stages[0] = protocol.Selection{}
	fragmentSelections := document.Fragments()[0].Selections()
	fragmentSelections[0] = protocol.Selection{}
	canonical := document.CanonicalJSON()
	canonical[0] = '['

	if document.Requires()[0] != "core.language-1" {
		t.Fatal("requires mutated through accessor")
	}
	defaultAgain, _ := document.Operations()[0].Variables()[0].Default()
	if string(defaultAgain) != `{"safe":true}` {
		t.Fatalf("default mutated through accessor: %s", defaultAgain)
	}
	stageAgain := document.Operations()[0].Selections()[0].Stages()[0]
	literalAgain, _ := stageAgain.Arguments()["value"].Literal()
	if string(literalAgain) != `{"safe":true}` || len(stageAgain.Directives()[0].Arguments()) != 1 {
		t.Fatal("nested selection data mutated through accessor")
	}
	if document.Fragments()[0].Selections()[0].Name() != "stable" || document.CanonicalJSON()[0] != '{' {
		t.Fatal("fragment or canonical bytes mutated through accessor")
	}
}

func TestDecodeDocumentMatchesLanguageConformanceStructure(t *testing.T) {
	t.Parallel()
	fixturePath := filepath.Join("..", "conformance", "v1", "language.json")
	content, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", fixturePath, err)
	}
	var fixture struct {
		Vectors []struct {
			Name               string          `json:"name"`
			Layer              string          `json:"layer"`
			Document           json.RawMessage `json:"document"`
			EquivalentDocument json.RawMessage `json:"equivalentDocument"`
			Valid              *bool           `json:"valid"`
			Code               string          `json:"code"`
			Phase              string          `json:"phase"`
			Pointer            string          `json:"pointer"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode %s: %v", fixturePath, err)
	}
	for _, vector := range fixture.Vectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Valid == nil {
				t.Fatal("language vector omits valid")
			}
			document, decodeErr := protocol.DecodeDocument(vector.Document, protocol.Limits{})
			if vector.Layer == "decode" {
				if *vector.Valid {
					t.Fatal("decoder rejection vector is marked valid")
				}
				var diagnostic *protocol.Diagnostic
				if !errors.As(decodeErr, &diagnostic) || diagnostic.Code != vector.Code || diagnostic.Phase != vector.Phase || diagnostic.Pointer != vector.Pointer {
					t.Fatalf("DecodeDocument error = %#v, want %s/%s at %s", diagnostic, vector.Code, vector.Phase, vector.Pointer)
				}
				return
			}
			if decodeErr != nil {
				t.Fatalf("DecodeDocument rejected a structurally valid vector: %v", decodeErr)
			}
			canonical, canonicalErr := protocol.CanonicalizeDocument(vector.Document, protocol.Limits{})
			if canonicalErr != nil || !bytes.Equal(document.CanonicalJSON(), canonical) {
				t.Fatalf("canonical AST mismatch: %v", canonicalErr)
			}
			if len(vector.EquivalentDocument) > 0 {
				equivalent, equivalentErr := protocol.DecodeDocument(vector.EquivalentDocument, protocol.Limits{})
				if equivalentErr != nil || !bytes.Equal(equivalent.CanonicalJSON(), document.CanonicalJSON()) {
					t.Fatalf("equivalent document mismatch: %v", equivalentErr)
				}
			}
		})
	}
}

func TestDecodedASTMatchesIndependentCanonicalDocumentVector(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "canonical.json"))
	if err != nil {
		t.Fatalf("read canonical vectors: %v", err)
	}
	var fixture struct {
		HashVectors []struct {
			Name      string `json:"name"`
			Input     string `json:"input"`
			Canonical string `json:"canonical"`
		} `json:"hashVectors"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode canonical vectors: %v", err)
	}
	for _, vector := range fixture.HashVectors {
		if vector.Name != "document-payload" {
			continue
		}
		document, decodeErr := protocol.DecodeDocument([]byte(vector.Input), protocol.Limits{})
		if decodeErr != nil || string(document.CanonicalJSON()) != vector.Canonical {
			t.Fatalf("canonical AST = %s, want %s (%v)", document.CanonicalJSON(), vector.Canonical, decodeErr)
		}
		return
	}
	t.Fatal("canonical document vector is missing")
}

func TestDecodeDocumentRejectsDuplicateLanguageNames(t *testing.T) {
	t.Parallel()
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Same","kind":"query","select":[]},{"name":"Same","kind":"query","select":[]}]}`, "DUPLICATE_NAME", "/operations/1")
	assertDocumentDiagnostic(t, `{"operations":[],"fragments":[{"name":"Same","select":[]},{"name":"Same","select":[]}]}`, "DUPLICATE_NAME", "/fragments/1")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","variables":[{"name":"same","type":"String"},{"name":"same","type":"String"}],"select":[]}]}`, "DUPLICATE_NAME", "/operations/0/variables/1")
	assertDocumentDiagnostic(t, `{"operations":[],"\u006fperations":[]}`, "DUPLICATE_KEY", "/operations")
}

func TestDecodeDocumentPreservesRepeatedDirectiveInvocations(t *testing.T) {
	t.Parallel()
	document, err := protocol.DecodeDocument([]byte(`{"operations":[{"name":"Q","kind":"query","select":[{"$field":{"name":"id","directives":[{"name":"audit"},{"name":"audit"}]}}]}]}`), protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument: %v", err)
	}
	directives := document.Operations()[0].Selections()[0].Directives()
	if len(directives) != 2 || directives[0].Name() != "audit" || directives[1].Name() != "audit" {
		t.Fatalf("directives = %#v", directives)
	}
}

func TestDecodeDocumentRejectsInvalidLanguageShapes(t *testing.T) {
	t.Parallel()
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$pipeline":{"as":"p","stages":[]}}]}]}`, "INVALID_SELECTIONS", "/operations/0/select/0/$pipeline/stages")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$pipeline":{"as":"p","stages":[{"$parallel":{"select":[]}}]}}]}]}`, "UNKNOWN_SELECTION", "/operations/0/select/0/$pipeline/stages/0/$parallel")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$pipeline":{"as":"p","stages":[{"$field":{"name":"id","as":"bad"}}]}}]}]}`, "UNKNOWN_FIELD", "/operations/0/select/0/$pipeline/stages/0/$field/as")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$map":{"select":[]}}]}]}`, "MISSING_FIELD", "/operations/0/select/0/$map/as")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$index":{"as":"i","at":1.5}}]}]}`, "INVALID_INDEX", "/operations/0/select/0/$index/at")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","variables":[{"name":"v","type":"String","required":"yes"}],"select":[]}]}`, "TYPE_MISMATCH", "/operations/0/variables/0/required")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"c","args":{"x":{"$var":"v","$literal":1}}}}]}]}`, "INVALID_EXPRESSION", "/operations/0/select/0/$call/args/x")
}

func TestDecodeDocumentPreservesCanonicalStructuralIntegerBounds(t *testing.T) {
	t.Parallel()
	document, err := protocol.DecodeDocument([]byte(`{"operations":[{"name":"Q","kind":"query","select":[{"$index":{"as":"large","at":9.007199254740991e15}},{"$slice":{"as":"exact","start":0e2147483648,"end":1.20e1}}]}]}`), protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument: %v", err)
	}
	selections := document.Operations()[0].Selections()
	at, ok := selections[0].At()
	if !ok || at.String() != "9007199254740991" {
		t.Fatalf("At() = %v, %t", at, ok)
	}
	start, ok := selections[1].Start()
	if !ok || start.String() != "0" {
		t.Fatalf("Start() = %v, %t", start, ok)
	}
	end, ok := selections[1].End()
	if !ok || end.String() != "12" {
		t.Fatalf("End() = %v, %t", end, ok)
	}
	at.SetInt64(0)
	again, _ := document.Operations()[0].Selections()[0].At()
	if again.String() != "9007199254740991" {
		t.Fatalf("bound mutated through accessor: %s", again)
	}
	roundTrip, err := protocol.DecodeDocument(document.CanonicalJSON(), protocol.Limits{})
	if err != nil {
		t.Fatalf("decode canonical AST: %v", err)
	}
	roundTripAt, ok := roundTrip.Operations()[0].Selections()[0].At()
	if !ok || roundTripAt.String() != "9007199254740991" {
		t.Fatalf("canonical round-trip At() = %v, %t", roundTripAt, ok)
	}
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$index":{"as":"unsafe","at":9007199254740992}}]}]}`, "INVALID_INDEX", "/operations/0/select/0/$index/at")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$index":{"as":"unsafe","at":9.007199254740992e15}}]}]}`, "INVALID_INDEX", "/operations/0/select/0/$index/at")
	assertDocumentDiagnostic(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$index":{"as":"huge","at":1e1000000}}]}]}`, "LIMIT_NUMBER", "/operations/0/select/0/$index/at")
}

func TestDecodeDocumentRejectsMalformedJSONBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input []byte
		code  string
	}{
		{"truncated object", []byte(`{"operations":[`), "MALFORMED_JSON"},
		{"split multibyte", []byte{'{', '"', 'x', '"', ':', '"', 0xe2, 0x82}, "INVALID_UTF8"},
		{"unpaired surrogate", []byte(`{"operations":[],"x":"\ud800"}`), "INVALID_UNICODE"},
		{"byte order mark", append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"operations":[]}`)...), "INVALID_UTF8"},
		{"malformed prefix", []byte(`tru`), "MALFORMED_JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := protocol.DecodeDocument(tt.input, protocol.Limits{})
			var diagnostic *protocol.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != tt.code || diagnostic.Offset < 0 || diagnostic.Line < 1 || diagnostic.Column < 1 {
				t.Fatalf("DecodeDocument error = %#v, want %s", diagnostic, tt.code)
			}
		})
	}
}

func TestDecodeDocumentReportsExactMalformedFieldLocations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		code    string
		pointer string
		marker  string
	}{
		{"kind", "{\n  \"operations\": [{\n    \"name\": \"Q\", \"kind\": 7, \"select\": []\n  }]\n}", "INVALID_OPERATION_KIND", "/operations/0/kind", "7"},
		{"select", "{\n  \"operations\": [{\n    \"name\": \"Q\", \"kind\": \"query\", \"select\": {}\n  }]\n}", "INVALID_SELECTIONS", "/operations/0/select", "{}"},
		{"fragment select", "{\n  \"operations\": [],\n  \"fragments\": [{\"name\": \"Part\", \"select\": {}}]\n}", "INVALID_SELECTIONS", "/fragments/0/select", "{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := protocol.DecodeDocument([]byte(tt.input), protocol.Limits{})
			var diagnostic *protocol.Diagnostic
			wantOffset := bytes.Index([]byte(tt.input), []byte(tt.marker))
			wantColumn := wantOffset - strings.LastIndex(tt.input[:wantOffset], "\n")
			if !errors.As(err, &diagnostic) || diagnostic.Code != tt.code || diagnostic.Pointer != tt.pointer || diagnostic.Offset != wantOffset || diagnostic.Line != 3 || diagnostic.Column != wantColumn {
				t.Fatalf("DecodeDocument error = %#v, want %s at byte %d on line 3, column %d", diagnostic, tt.code, wantOffset, wantColumn)
			}
		})
	}
}

func assertDocumentDiagnostic(t *testing.T, input, code, pointer string) {
	t.Helper()
	_, err := protocol.DecodeDocument([]byte(input), protocol.Limits{})
	var diagnostic *protocol.Diagnostic
	if !errors.As(err, &diagnostic) || diagnostic.Code != code || diagnostic.Pointer != pointer || diagnostic.Line < 1 || diagnostic.Column < 1 || diagnostic.Offset < 0 {
		t.Fatalf("DecodeDocument error = %#v, want %s at %s", diagnostic, code, pointer)
	}
}

func TestDecodeDocumentEnforcesEveryConfiguredLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		input  string
		limits protocol.Limits
		code   string
	}{
		{"bytes", `{"operations":[]}`, protocol.Limits{MaxBytes: 2}, "LIMIT_BYTES"},
		{"tokens", `{"operations":[]}`, protocol.Limits{MaxTokens: 1}, "LIMIT_TOKENS"},
		{"depth", `{"operations":[]}`, protocol.Limits{MaxDepth: 1}, "LIMIT_DEPTH"},
		{"string", `{"operations":[{"name":"Long","kind":"query","select":[]}]}`, protocol.Limits{MaxStringBytes: 2}, "LIMIT_STRING"},
		{"members", `{"operations":[],"requires":[]}`, protocol.Limits{MaxMembers: 1}, "LIMIT_MEMBERS"},
		{"array", `{"operations":[{},{}]}`, protocol.Limits{MaxArrayItems: 1}, "LIMIT_ARRAY"},
		{"number", `{"operations":[{"name":"Q","kind":"query","select":[{"$index":{"as":"i","at":123}}]}]}`, protocol.Limits{MaxNumberBytes: 2}, "LIMIT_NUMBER"},
		{"literal", `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"c","args":{"x":{"$literal":"large"}}}}]}]}`, protocol.Limits{MaxLiteralBytes: 4}, "LIMIT_LITERAL"},
		{"default", `{"operations":[{"name":"Q","kind":"query","variables":[{"name":"v","type":"String","default":"large"}],"select":[]}]}`, protocol.Limits{MaxLiteralBytes: 4}, "LIMIT_LITERAL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := protocol.DecodeDocument([]byte(tt.input), tt.limits)
			var diagnostic *protocol.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != tt.code {
				t.Fatalf("DecodeDocument error = %v, want %s", err, tt.code)
			}
		})
	}
}
