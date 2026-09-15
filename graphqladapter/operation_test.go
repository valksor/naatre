package graphqladapter

import (
	"context"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestOperationImportExportPreservesSelectionsAliasesAndNull(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	source := []byte(`query GetAccount($id: ID!, $note: String) {
  viewer: account(id: $id, note: null) {
    id
    display: name @include(if: true)
    friends { id }
  }
}`)
	executable, err := ImportOperation(context.Background(), source, imported, Limits{})
	if err != nil {
		t.Fatalf("ImportOperation: %v", err)
	}
	operations := executable.Document().Operations()
	if len(operations) != 1 || operations[0].Name() != "GetAccount" || operations[0].Kind() != protocol.Query {
		t.Fatalf("operations = %#v", operations)
	}
	root := operations[0].Selections()[0]
	if root.Kind() != protocol.CallSelection || root.Name() != "account" || root.Alias() != "viewer" {
		t.Fatalf("root selection = %#v", root)
	}
	arguments := root.Arguments()
	if arguments["id"].Kind() != protocol.VariableExpression {
		t.Fatalf("id argument = %#v", arguments["id"])
	}
	if literal, ok := arguments["note"].Literal(); !ok || string(literal) != "null" {
		t.Fatalf("note literal = %s, %t", literal, ok)
	}
	children := root.Selections()
	if children[1].Alias() != "display" || children[1].Name() != "name" || len(children[1].Directives()) != 1 {
		t.Fatalf("aliased child = %#v", children[1])
	}
	exported, err := ExportOperation(context.Background(), executable.Document(), imported, Limits{})
	if err != nil {
		t.Fatalf("ExportOperation: %v", err)
	}
	for _, expected := range []string{"query GetAccount", "viewer: account", "note: null", "display: name @include(if: true)"} {
		if !strings.Contains(string(exported), expected) {
			t.Errorf("export omits %q: %s", expected, exported)
		}
	}
}

func TestOperationMappingRejectsUnsupportedCompositionAndLimits(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	_, err := ImportOperation(context.Background(), []byte(`query Q { account @defer { id } }`), imported, Limits{})
	if CodeOf(err) != CodeUnsupported {
		t.Fatalf("unsupported directive = %v", err)
	}

	document, err := protocol.DecodeDocument([]byte(`{"operations":[{"name":"Q","kind":"query","select":[{"$pipeline":{"as":"value","stages":[{"$call":{"name":"account"}}]}}]}]}`), protocol.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ExportOperation(context.Background(), document, imported, Limits{})
	if CodeOf(err) != CodeUnsupported {
		t.Fatalf("pipeline export = %v", err)
	}

	_, err = ImportOperation(context.Background(), []byte(`query Q { account { friends { friends { id } } } }`), imported, Limits{MaxDepth: 2})
	if CodeOf(err) != CodeResourceExhausted {
		t.Fatalf("depth limit = %v", err)
	}
}

func TestOperationValidationRejectsSelectionSemanticLoss(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	for name, source := range map[string]string{
		"unknown-field":          `query Q { account(id: "a-1") { missing } }`,
		"missing-required-arg":   `query Q { account { id } }`,
		"object-without-set":     `query Q { account(id: "a-1") }`,
		"scalar-with-set":        `query Q { account(id: "a-1") { id { value } } }`,
		"unknown-variable":       `query Q { account(id: $missing) { id } }`,
		"wrong-variable-type":    `query Q($id: String!) { account(id: $id) { id } }`,
		"nullable-required-var":  `query Q($id: ID) { account(id: $id) { id } }`,
		"fragment-cycle":         `query Q { account(id: "a-1") { ...A } } fragment A on Account { ...A }`,
		"fragment-type-mismatch": `query Q { account(id: "a-1") { ...Wrong } } fragment Wrong on RootMutation { rename(id: "a-1", name: "x") { id } }`,
		"multi-subscription":     `subscription S { one: accountChanged(id: "a-1") { id } two: accountChanged(id: "a-2") { id } }`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ImportOperation(context.Background(), []byte(source), imported, Limits{})
			if err == nil || CodeOf(err) != CodeDocumentInvalid {
				t.Fatalf("ImportOperation(%s) = %v", name, err)
			}
		})
	}
}

func TestOperationRejectsEnumLiteralAndMalformedLexemesWithoutNormalization(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	for name, source := range map[string]string{
		"enum-literal":   `query Q { account(id: ACTIVE) { id } }`,
		"go-only-escape": `query Q { account(id: "\x41") { id } }`,
		"leading-zero":   `query Q { account(id: 01) { id } }`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ImportOperation(context.Background(), []byte(source), imported, Limits{})
			if err == nil {
				t.Fatalf("ImportOperation(%s) accepted semantic normalization", name)
			}
		})
	}
}
