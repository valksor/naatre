package runtime_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestPlanDescriptionBindsCompileTimeInputsWithoutRequestValues(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	prepare := func(literal string) runtime.PlanDescription {
		t.Helper()
		request := decodeRuntimeRequest(t, `{"version":"1","variables":{"requestOnly":true},"document":{"requires":["core.language-1"],"operations":[{"name":"Q","kind":"query","variables":[{"name":"requestOnly","type":"Boolean"}],"select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"`+literal+`"}},"directives":[{"name":"include","arguments":{"if":{"$var":"requestOnly"}}}],"select":[{"$field":{"name":"name"}}]}}]}]}}`)
		plan, err := runtime.Prepare(snapshot, request)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		return plan.Description()
	}
	first := prepare("u-1")
	second := prepare("u-2")
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first plan: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second plan: %v", err)
	}
	if bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("different static literals produced the same plan: %s", firstJSON)
	}
	if bytes.Contains(firstJSON, []byte(`"requestOnly":true`)) {
		t.Fatalf("plan retained request variable value: %s", firstJSON)
	}
	if !slices.Equal(first.Requirements, []string{"core.language-1"}) || len(first.VariableDefinitions) != 1 {
		t.Fatalf("static operation inputs = %#v", first)
	}
	input := first.Nodes[0].Inputs[0]
	if input.Name != "id" || input.Expression.Kind != protocol.LiteralExpression || string(input.Expression.Literal) != `"u-1"` {
		t.Fatalf("planned input = %#v", input)
	}
}

func TestPlanDescriptionEncodesSelectedResultsPathsAndBarriers(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$nest":{"as":"profile","select":[{"$field":{"name":"name","as":"display"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	description := plan.Description()
	if description.Result.Kind != schema.ObjectType || len(description.Result.Fields) != 1 {
		t.Fatalf("operation selected result = %#v", description.Result)
	}
	lookup := description.Result.Fields[0]
	if lookup.Name != "lookup" || lookup.Result.Kind != schema.ObjectType || lookup.Result.Type != "User" || len(lookup.Result.Fields) != 1 {
		t.Fatalf("lookup selected result = %#v", lookup)
	}
	profile := lookup.Result.Fields[0]
	if profile.Name != "profile" || profile.Result.Kind != schema.ObjectType || len(profile.Result.Fields) != 1 {
		t.Fatalf("nested selected result = %#v", profile)
	}
	if display := profile.Result.Fields[0]; display.Name != "display" || display.Result.Type != schema.TypeID(schema.String) || display.Result.Kind != schema.ScalarType {
		t.Fatalf("field selected result = %#v", display)
	}

	root := description.Nodes[0]
	nest := root.Children[0]
	field := nest.Children[0]
	if !slices.Equal(root.ResponsePath, []string{"lookup"}) ||
		!slices.Equal(nest.ResponsePath, []string{"lookup", "profile"}) ||
		!slices.Equal(field.ResponsePath, []string{"lookup", "profile", "display"}) {
		t.Fatalf("response paths = %#v %#v %#v", root.ResponsePath, nest.ResponsePath, field.ResponsePath)
	}
	if root.Scheduling != runtime.SequentialScheduling || root.Ordinal != 0 ||
		!slices.Contains(root.Barriers, runtime.AuthorizationBarrier) || !slices.Contains(root.Barriers, runtime.CompletionBarrier) {
		t.Fatalf("root scheduling metadata = %#v", root)
	}
}

func TestPlanDescriptionEncodesParallelAndEffectBarriers(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	parallel, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"policy":"fail-fast","select":[{"$call":{"name":"text","as":"left"}},{"$call":{"name":"text","as":"right"}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare parallel: %v", err)
	}
	group := parallel.Description().Nodes[0]
	if group.ParallelPolicy != protocol.FailFastParallel || group.Scheduling != runtime.SequentialScheduling || len(group.Children) != 2 {
		t.Fatalf("parallel group = %#v", group)
	}
	for index, branch := range group.Children {
		if branch.Scheduling != runtime.ParallelScheduling || branch.Ordinal != index {
			t.Fatalf("parallel branch %d = %#v", index, branch)
		}
	}

	mutation, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"write"}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare mutation: %v", err)
	}
	write := mutation.Description().Nodes[0]
	if !slices.Contains(write.Barriers, runtime.EffectBarrier) || write.Effect != runtime.WriteEffect {
		t.Fatalf("write barriers = %#v", write)
	}
}

func TestPlanDescriptionRetainsConditionalAndConcreteTypeAvailability(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"actor","select":[{"$fragment":{"name":"UserFields"}},{"$fragment":{"name":"AdminFields"}}]}},{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"as":"conditional","directives":[{"name":"include","arguments":{"if":{"$literal":false}}}],"select":[{"$field":{"name":"name"}}]}}]}],"fragments":[{"name":"UserFields","on":"User","select":[{"$field":{"name":"name","as":"display"}}]},{"name":"AdminFields","on":"Admin","select":[{"$field":{"name":"name","as":"display"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	result := plan.Description().Result
	if len(result.Fields) != 2 || result.Fields[1].Required {
		t.Fatalf("conditional result availability = %#v", result.Fields)
	}
	actorFields := result.Fields[0].Result.Fields
	if len(actorFields) != 2 || !slices.Equal(actorFields[0].TypeConditions, []schema.TypeID{"User"}) ||
		!slices.Equal(actorFields[1].TypeConditions, []schema.TypeID{"Admin"}) {
		t.Fatalf("concrete result conditions = %#v", actorFields)
	}
}
