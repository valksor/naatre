package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestPrepareReportsPortableConstraintCodesAndPathsBeforeHandlers(t *testing.T) {
	t.Parallel()
	trait, err := schema.ConstraintTrait(schema.ConstraintSet{MinItems: runtimeIntPointer(1), UniqueItems: true})
	if err != nil {
		t.Fatal(err)
	}
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "ConstrainedTags", Kind: schema.ListType, Input: true, Element: schema.TypeID(schema.String), Traits: []schema.TraitDescriptor{trait}},
		{ID: "ConstrainedInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{"tags": {Type: "ConstrainedTags", Required: true}}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	registry := runtime.NewRegistry(types)
	var calls atomic.Int64
	descriptor := runtime.Descriptor{
		Name: "useTags", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "ConstrainedInput", Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "", nil
	})); err != nil {
		t.Fatal(err)
	}
	snapshot := frozenRegistry(t, registry)
	request := decodeRuntimeRequest(t, `{"version":"1","variables":{"tags":["same","same"]},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"tags","type":"ConstrainedTags","required":true}],"select":[{"$call":{"name":"useTags","args":{"tags":{"$var":"tags"}}}}]}]}}`)
	_, err = runtime.Prepare(snapshot, request)
	var validationErr *runtime.ValidationErrors
	if !errors.As(err, &validationErr) {
		t.Fatalf("Prepare error = %v", err)
	}
	issues := validationErr.Issues()
	if len(issues) != 1 || issues[0].Diagnostic.Code != "CONSTRAINT_UNIQUE_ITEMS" || !strings.HasSuffix(issues[0].Diagnostic.Pointer, "/1") {
		encoded, _ := json.Marshal(issues)
		t.Fatalf("constraint issues = %s", encoded)
	}
	if calls.Load() != 0 {
		t.Fatalf("handler calls = %d", calls.Load())
	}
}

func TestExecuteRejectsConstraintViolatingOutputsBeforeEmission(t *testing.T) {
	t.Parallel()
	minimumNameLength := 3
	minimumItems := 2
	nameTrait, err := schema.ConstraintTrait(schema.ConstraintSet{MinLength: &minimumNameLength})
	if err != nil {
		t.Fatal(err)
	}
	listTrait, err := schema.ConstraintTrait(schema.ConstraintSet{MinItems: &minimumItems})
	if err != nil {
		t.Fatal(err)
	}
	mapTrait, err := schema.ConstraintTrait(schema.ConstraintSet{KeyPattern: `[a-z]+`})
	if err != nil {
		t.Fatal(err)
	}
	ruleTrait, err := schema.ConstraintTrait(schema.ConstraintSet{Rules: []schema.ConstraintRule{{
		ID: "company-requires-vat",
		Assert: schema.RuleExpression{Operator: schema.RuleOr, Children: []schema.RuleExpression{
			{Operator: schema.RuleAbsent, Field: "company"},
			{Operator: schema.RulePresent, Field: "vat"},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "ConstrainedNames", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.String), Traits: []schema.TraitDescriptor{listTrait}},
		{ID: "ConstrainedLabels", Kind: schema.MapType, Output: true, Element: schema.TypeID(schema.String), Traits: []schema.TraitDescriptor{mapTrait}},
		{ID: "ConstrainedProfile", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name":    {Type: schema.TypeID(schema.String), Traits: []schema.TraitDescriptor{nameTrait}},
			"safe":    {Type: schema.TypeID(schema.String)},
			"company": {Type: schema.TypeID(schema.String)},
			"vat":     {Type: schema.TypeID(schema.String)},
		}, Traits: []schema.TraitDescriptor{ruleTrait}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	definitions := []runtime.Definition{
		runtime.BindInvocation(runtime.Descriptor{Name: "profile", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "ConstrainedProfile", Metadata: completeMetadata(runtime.ReadEffect)}, func(context.Context, runtime.Invocation) (map[string]any, error) {
			return map[string]any{"name": "x", "safe": "retained", "company": "Naatre"}, nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "names", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "ConstrainedNames", Metadata: completeMetadata(runtime.ReadEffect)}, func(context.Context, runtime.Invocation) ([]string, error) {
			return []string{"one"}, nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "labels", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "ConstrainedLabels", Metadata: completeMetadata(runtime.ReadEffect)}, func(context.Context, runtime.Invocation) (map[string]string, error) {
			return map[string]string{"123": "invalid key"}, nil
		}),
		runtime.BindInvocation(runtime.Descriptor{Name: "company", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "ConstrainedProfile", Metadata: completeMetadata(runtime.ReadEffect)}, func(context.Context, runtime.Invocation) (map[string]any, error) {
			return map[string]any{"company": "Naatre", "safe": "not emitted"}, nil
		}),
	}
	outcome := runtime.ExecuteDefinitionsForTest(context.Background(), types, definitions)
	if len(outcome.Errors) != 4 {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
	wantPaths := map[string]bool{
		`["profile","name"]`: false,
		`["names"]`:          false,
		`["labels","123"]`:   false,
		`["company"]`:        false,
	}
	for index, executionError := range outcome.Errors {
		path, err := json.Marshal(executionError.Path)
		if err != nil {
			t.Fatal(err)
		}
		if executionError.Code != "OUTPUT_COMPLETION" {
			t.Fatalf("error %d = %#v", index, executionError)
		}
		if _, ok := wantPaths[string(path)]; !ok {
			t.Fatalf("error %d path = %s", index, path)
		}
		wantPaths[string(path)] = true
	}
	for path, seen := range wantPaths {
		if !seen {
			t.Fatalf("missing OUTPUT_COMPLETION at %s", path)
		}
	}
	encoded, err := json.Marshal(outcome.Data)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"profile":{"company":"Naatre","safe":"retained"}}` {
		t.Fatalf("data = %s", encoded)
	}
}

func runtimeIntPointer(value int) *int { return &value }
