package runtime_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestPrepareValidatesCollectionPlanSemantics(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	validPage := []byte(`{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$slice":{"as":"tail","start":1,"select":[{"$field":{"name":"name"}}]}},{"$page":{"as":"page","first":2,"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	request, err := protocol.DecodeRequest(validPage, protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if _, err := runtime.Prepare(snapshot, request); err != nil {
		t.Fatalf("Prepare collection children: %v", err)
	}

	for _, input := range []string{
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"page","first":2}}]}}]}]}}`,
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$meta":{"name":"totalCount"}}]}}]}]}}`,
	} {
		if got := validationCodes(t, prepareError(snapshot, decodeRuntimeRequest(t, input))); !slices.Contains(got, "UNSUPPORTED_CAPABILITY") {
			t.Fatalf("codes = %v, want UNSUPPORTED_CAPABILITY", got)
		}
	}
}

func TestPrepareRejectsPageWithoutSecureCollectionRuntime(t *testing.T) {
	t.Parallel()
	types := freezeCompositionTypes(t, schema.TypeDescriptor{ID: "Users", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.String)})
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, runtime.BindInvocation[[]string](runtime.Descriptor{
		Name: "users", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Users", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]string, error) { return nil, nil }))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"page","first":1}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	if got := validationCodes(t, prepareError(snapshot, request)); !slices.Contains(got, "COLLECTION_PAGE_UNAVAILABLE") {
		t.Fatalf("validation codes = %v, want COLLECTION_PAGE_UNAVAILABLE", got)
	}
}

func TestPrepareAlwaysRejectsPageInfoMetadata(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	tests := []struct {
		name    string
		request string
		options protocol.DecodeOptions
	}{
		{name: "without capability", request: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$meta":{"name":"pageInfo"}}]}}]}]}}`},
		{name: "with capability", request: `{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$meta":{"name":"pageInfo"}}]}}]}]}}`, options: protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := decodeRuntimeRequestWithOptions(t, test.request, test.options)
			codes := validationCodes(t, prepareError(snapshot, request))
			if !slices.Contains(codes, "UNKNOWN_METADATA") || slices.Contains(codes, "UNSUPPORTED_CAPABILITY") {
				t.Fatalf("validation codes = %v, want only metadata rejection", codes)
			}
		})
	}
}

func TestIndexPlanSeparatesMissingAvailabilityFromElementNullability(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	for _, test := range []struct {
		call     string
		nullable bool
	}{
		{call: "users"},
		{call: "nullableUsers", nullable: true},
	} {
		t.Run(test.call, func(t *testing.T) {
			request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"`+test.call+`","select":[{"$index":{"as":"first","at":0,"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
			plan, err := runtime.Prepare(snapshot, request)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			field := plan.Description().Result.Fields[0].Result.Fields[0]
			if field.Required || field.Result.Nullable != test.nullable {
				t.Fatalf("index selected field = %#v, want required=false nullable=%v", field, test.nullable)
			}
		})
	}
}

func TestPrepareAllowsIntersectingAbstractTypeConditions(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"actor","select":[{"$fragment":{"name":"StaffFields"}}]}}]}],"fragments":[{"name":"StaffFields","on":"Staff","select":[]}]}}`)
	if _, err := runtime.Prepare(snapshot, request); err != nil {
		t.Fatalf("Prepare intersecting Actor and Staff: %v", err)
	}
}

func TestPrepareRejectsEmptyNestedTypeConditionIntersection(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	assertValidationContains(t, snapshot, "IMPOSSIBLE_TYPE_CONDITION",
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"actor","select":[{"$fragment":{"name":"StaffFields"}}]}}]}],"fragments":[{"name":"StaffFields","on":"Staff","select":[{"$fragment":{"name":"EditorFields"}}]},{"name":"EditorFields","on":"Editor","select":[]}]}}`)
}

func TestPrepareAllowsAliasReuseForDisjointEffectiveIntersections(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"universe","select":[{"$fragment":{"name":"OuterA"}},{"$fragment":{"name":"OuterB"}}]}}]}],"fragments":[{"name":"OuterA","on":"GroupA","select":[{"$fragment":{"name":"Common"}}]},{"name":"OuterB","on":"GroupB","select":[{"$fragment":{"name":"Common"}}]},{"name":"Common","on":"Staff","select":[{"$nest":{"as":"display","select":[]}}]}]}}`)
	if _, err := runtime.Prepare(snapshot, request); err != nil {
		t.Fatalf("Prepare disjoint effective intersections: %v", err)
	}
}

func TestPrepareRejectsImplicitCompositeCurrentValues(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	assertValidationContains(t, snapshot, "INVALID_OBJECT_SELECTION",
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}}}}]}]}}`)
	assertValidationContains(t, snapshot, "INVALID_OBJECT_SELECTION",
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"actor"}}]}]}}`)
	assertValidationContains(t, snapshot, "INVALID_CURRENT",
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$current":{"as":"raw"}}]}}]}]}}`)
}

func TestPrepareAllowsCollectionCurrentValue(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$current":{"as":"raw"}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare collection current: %v", err)
	}
	result := plan.Description().Result.Fields[0].Result.Fields[0].Result
	if result.Type != "Users" || result.Kind != schema.ListType || result.Element == nil || result.Element.Type != "User" {
		t.Fatalf("collection current selected result = %#v", result)
	}
}

func TestCollectionPlanPreservesSelectedElementTypeAndNullability(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"nullableUsers","select":[{"$map":{"as":"mapped","select":[{"$field":{"name":"name"}}]}},{"$slice":{"as":"sliced","start":0,"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	fields := plan.Description().Result.Fields[0].Result.Fields
	for _, field := range fields {
		if field.Result.Element == nil || field.Result.Element.Type != "User" || !field.Result.Element.Nullable {
			t.Fatalf("%s element description = %#v", field.Name, field.Result.Element)
		}
	}
}

func assertValidationContains(t *testing.T, snapshot runtime.Snapshot, code, input string) {
	t.Helper()
	if got := validationCodes(t, prepareError(snapshot, decodeRuntimeRequest(t, input))); !slices.Contains(got, code) {
		t.Fatalf("codes = %v, want %s", got, code)
	}
}

func TestFragmentExpansionDiagnosticsCarryDefinitionAndUseSources(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"Bad"}}]}],"fragments":[{"name":"Bad","select":[{"$call":{"name":"text","directives":[{"name":"untrusted"}]}}]}]}}`)
	var validationErr *runtime.ValidationErrors
	if !errors.As(prepareError(snapshot, request), &validationErr) {
		t.Fatal("Prepare did not return ValidationErrors")
	}
	issues := validationErr.Issues()
	if len(issues) != 1 || issues[0].Diagnostic.Code != "UNKNOWN_DIRECTIVE" || len(issues[0].Related) < 2 {
		t.Fatalf("fragment directive issue = %#v", issues)
	}
	want := []string{"/document/fragments/0", "/document/operations/0/select/0"}
	got := []string{issues[0].Related[0].Pointer, issues[0].Related[1].Pointer}
	if !slices.Equal(got, want) {
		t.Fatalf("related sources = %v, want %v", got, want)
	}
}

func TestPrepareRejectsScalarUnnestAndDeduplicatesNestedParallelAdmission(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	assertValidationContains(t, snapshot, "INVALID_UNNEST",
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","select":[{"$unnest":{"select":[{"$current":{"as":"value"}}]}}]}}]}]}}`)

	nested := `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$parallel":{"select":[{"$call":{"name":"serial"}}]}}]}}]}]}}`
	got := validationCodes(t, prepareError(snapshot, decodeRuntimeRequest(t, nested)))
	if !slices.Equal(got, []string{"PARALLEL_THREAD_UNSAFE"}) {
		t.Fatalf("nested parallel codes = %v", got)
	}
}

func TestPlanDescriptionTerminatesForBoundedRecursiveCollection(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{
		ID: "LoopList", Kind: schema.ListType, Output: true, Element: "LoopList", MaxDepth: 2,
	}); err != nil {
		t.Fatalf("register recursive list: %v", err)
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("freeze types: %v", err)
	}
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	descriptor := runtime.Descriptor{
		Name: "loop", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "LoopList", Metadata: completeMetadata(runtime.ReadEffect),
	}
	if err := registry.Register(runtime.BindInvocation[[]any](descriptor, func(context.Context, runtime.Invocation) ([]any, error) {
		return nil, nil
	})); err != nil {
		t.Fatalf("register loop: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"loop"}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	result := plan.Description().Result.Fields[0].Result
	if result.Type != "LoopList" || result.Element == nil || result.Element.Type != "LoopList" || result.Element.Element != nil {
		t.Fatalf("recursive selected result = %#v", result)
	}
}
