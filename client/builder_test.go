package client

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestBuilderMatchesSharedGeneratorDocumentAndIsImmutable(t *testing.T) {
	t.Parallel()
	id, err := NewVariable("id", "ID", true, false)
	if err != nil {
		t.Fatalf("NewVariable id: %v", err)
	}
	nickname, err := NewVariable("nickname", "String", false, true)
	if err != nil {
		t.Fatalf("NewVariable nickname: %v", err)
	}
	tags, _ := NewVariable("tags", "StringList", false, false)
	filter, _ := NewVariable("filter", "StringMap", false, false)
	idReference, err := VariableReference("id")
	if err != nil {
		t.Fatalf("VariableReference: %v", err)
	}
	idArgument, err := NewArgument("id", idReference)
	if err != nil {
		t.Fatalf("NewArgument: %v", err)
	}
	display, _ := Field("displayName", SelectionOptions{Alias: "display"})
	optionalNickname, _ := Field("nickname", SelectionOptions{})
	profile, err := Call("account", SelectionOptions{Alias: "profile", Arguments: []Argument{idArgument}, Select: []Selection{display, optionalNickname}})
	if err != nil {
		t.Fatalf("Call account: %v", err)
	}
	later, _ := Call("audit", SelectionOptions{Alias: "later"})
	base := NewBuilder()
	built, err := base.WithOperation(OperationSpec{
		Name: "GetAccount", Kind: protocol.Query,
		Variables: []VariableDefinition{id, nickname, tags, filter}, Select: []Selection{profile, later},
	})
	if err != nil {
		t.Fatalf("WithOperation: %v", err)
	}
	document, err := built.Document()
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	want := generatorFixtureDocument(t)
	if !bytes.Equal(document.CanonicalJSON(), want.CanonicalJSON()) {
		t.Fatalf("builder document differs from shared vector:\n%s\n%s", document.CanonicalJSON(), want.CanonicalJSON())
	}
	if _, err := base.Document(); err != nil {
		t.Fatalf("base builder was mutated: %v", err)
	}
}

func TestBuilderRejectsInvalidPipelinesArgumentsAliasesAndBounds(t *testing.T) {
	t.Parallel()
	if _, err := Pipeline("result", nil, SelectionOptions{}); err == nil {
		t.Fatal("Pipeline accepted no stages")
	}
	literal, _ := Literal(json.RawMessage("1"))
	argument, _ := NewArgument("value", literal)
	if _, err := Call("load", SelectionOptions{Arguments: []Argument{argument, argument}}); err == nil {
		t.Fatal("Call accepted duplicate arguments")
	}
	if _, err := ForwardPage("items", 0, nil, nil, SelectionOptions{}); err == nil {
		t.Fatal("ForwardPage accepted zero count")
	}
	start, end := uint64(2), uint64(1)
	if _, err := Slice("items", &start, &end, nil, SelectionOptions{}); err == nil {
		t.Fatal("Slice accepted reversed bounds")
	}
	first, _ := Field("first", SelectionOptions{Alias: "same"})
	second, _ := Field("second", SelectionOptions{Alias: "same"})
	duplicate, err := NewBuilder().WithOperation(OperationSpec{Name: "Duplicates", Kind: protocol.Query, Select: []Selection{first, second}})
	if err != nil {
		t.Fatalf("WithOperation duplicate setup: %v", err)
	}
	if _, err := duplicate.Document(); err == nil {
		t.Fatal("Document accepted duplicate response aliases")
	}
}

func TestBuilderConvenienceFormsProduceValidImmutableValues(t *testing.T) {
	t.Parallel()
	leaf, err := Field("name", SelectionOptions{})
	mustBuilderSuccess(t, "Field", err)
	literal, err := Literal(json.RawMessage(`"cursor"`))
	mustBuilderSuccess(t, "Literal", err)
	argument, err := NewArgument("cursor", literal)
	mustBuilderSuccess(t, "NewArgument", err)
	directive, err := NewDirective("include", argument)
	mustBuilderSuccess(t, "NewDirective", err)

	_, err = FieldStep("name", SelectionOptions{Directives: []Directive{directive}})
	mustBuilderSuccess(t, "FieldStep", err)
	_, err = CallStep("load", SelectionOptions{Arguments: []Argument{argument}})
	mustBuilderSuccess(t, "CallStep", err)
	_, err = MapStep([]Selection{leaf}, SelectionOptions{})
	mustBuilderSuccess(t, "MapStep", err)
	_, err = Index("item", 0, []Selection{leaf}, SelectionOptions{})
	mustBuilderSuccess(t, "Index", err)
	_, err = IndexStep(0, []Selection{leaf}, SelectionOptions{})
	mustBuilderSuccess(t, "IndexStep", err)
	start, end := uint64(0), uint64(1)
	_, err = SliceStep(&start, &end, []Selection{leaf}, SelectionOptions{})
	mustBuilderSuccess(t, "SliceStep", err)
	_, err = ForwardPageStep(1, &literal, []Selection{leaf}, SelectionOptions{})
	mustBuilderSuccess(t, "ForwardPageStep", err)
	_, err = BackwardPage("previous", 1, &literal, []Selection{leaf}, SelectionOptions{})
	mustBuilderSuccess(t, "BackwardPage", err)
	_, err = BackwardPageStep(1, &literal, []Selection{leaf}, SelectionOptions{})
	mustBuilderSuccess(t, "BackwardPageStep", err)
	_, err = Meta("typename", "type", SelectionOptions{})
	mustBuilderSuccess(t, "Meta", err)
	_, err = MetaStep("typename", SelectionOptions{})
	mustBuilderSuccess(t, "MetaStep", err)
	_, err = CurrentStep(SelectionOptions{})
	mustBuilderSuccess(t, "CurrentStep", err)
	_, err = Nest("nested", []Selection{leaf}, SelectionOptions{})
	mustBuilderSuccess(t, "Nest", err)
	_, err = Unnest([]Selection{leaf}, directive)
	mustBuilderSuccess(t, "Unnest", err)
	_, err = NamedFragment("Names", []Argument{argument}, directive)
	mustBuilderSuccess(t, "NamedFragment", err)
	_, err = InlineFragment("User", []Selection{leaf}, directive)
	mustBuilderSuccess(t, "InlineFragment", err)
	_, err = Atomic("group", []Selection{leaf}, directive)
	mustBuilderSuccess(t, "Atomic", err)

	variable, err := NewVariable("cursor", "String", false, false)
	mustBuilderSuccess(t, "NewVariable", err)
	variable, err = variable.WithDefault(json.RawMessage(`"first"`))
	mustBuilderSuccess(t, "WithDefault", err)
	builder, err := NewBuilder().WithFragment(FragmentSpec{Name: "Names", On: "User", Parameters: []VariableDefinition{variable}, Select: []Selection{leaf}})
	mustBuilderSuccess(t, "WithFragment", err)
	if _, err := builder.Document(); err != nil {
		t.Fatalf("fragment document: %v", err)
	}

	if _, err := ResultReference("prior"); err != nil {
		t.Fatalf("ResultReference: %v", err)
	}
	_ = ParentReference()
	_ = CurrentReference()
	decoded, err := DecodeJSON[string](json.RawMessage(`"value"`))
	if err != nil || decoded != "value" {
		t.Fatalf("DecodeJSON = %q, %v", decoded, err)
	}
}

func mustBuilderSuccess(t *testing.T, operation string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
}

func generatorFixtureDocument(t *testing.T) protocol.Document {
	t.Helper()
	payload, err := os.ReadFile("../conformance/v1/generator-model.json")
	if err != nil {
		t.Fatalf("read generator model: %v", err)
	}
	var model struct {
		Operations []struct {
			Document json.RawMessage `json:"document"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(payload, &model); err != nil || len(model.Operations) != 1 {
		t.Fatalf("decode generator model: %v", err)
	}
	document, err := protocol.DecodeDocument(model.Operations[0].Document, protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument fixture: %v", err)
	}
	return document
}
