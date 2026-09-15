package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	goruntime "runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestExecuteParallelResponsesAreByteStableAcrossRepeatedRuns(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := naatreruntime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{Mode: naatreruntime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"slowName", "fastName"} {
		name := name
		registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
			Name: name, Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(naatreruntime.ReadEffect),
		}, func(context.Context, naatreruntime.Invocation) (string, error) {
			return "", errors.New(name)
		}))
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"slowName","as":"zeta"}},{"$call":{"name":"fastName","as":"alpha"}}]}}]}]}}`)
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var expected []byte
	for iteration := range 128 {
		outcome := plan.Execute(context.Background())
		encoded, err := json.Marshal(outcome)
		if err != nil {
			t.Fatalf("marshal iteration %d: %v", iteration, err)
		}
		if iteration == 0 {
			expected = encoded
			continue
		}
		if string(encoded) != string(expected) {
			t.Fatalf("iteration %d response = %s, want %s", iteration, encoded, expected)
		}
	}
}

func TestExecuteNestedParallelGroupsShareTheExecutionBound(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := naatreruntime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{Mode: naatreruntime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	var active atomic.Int64
	var maximum atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
		Name: "work", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(naatreruntime.ReadEffect),
	}, func(context.Context, naatreruntime.Invocation) (string, error) {
		current := active.Add(1)
		for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
		}
		if current == 8 {
			startedOnce.Do(func() { close(started) })
		}
		<-release
		active.Add(-1)
		return "ok", nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}

	outer := make([]any, 8)
	for outerIndex := range outer {
		inner := make([]any, 8)
		for innerIndex := range inner {
			alias := "work" + strconv.Itoa(outerIndex) + "_" + strconv.Itoa(innerIndex)
			inner[innerIndex] = map[string]any{"$call": map[string]any{"name": "work", "as": alias}}
		}
		outer[outerIndex] = map[string]any{"$parallel": map[string]any{"select": inner}}
	}
	document := map[string]any{"operations": []any{map[string]any{
		"name": "Q", "kind": "query", "select": []any{map[string]any{"$parallel": map[string]any{"select": outer}}},
	}}}
	envelope, err := json.Marshal(map[string]any{"version": "1", "document": document})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request, err := protocol.DecodeRequest(envelope, protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	done := make(chan naatreruntime.Outcome, 1)
	go func() { done <- plan.Execute(context.Background()) }()
	<-started
	for range 128 {
		goruntime.Gosched()
	}
	close(release)
	outcome := <-done
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	if maximum.Load() > 8 {
		t.Fatalf("nested parallel maximum = %d, want at most 8", maximum.Load())
	}
}

func TestExecuteCollectParallelStopsQueuedAdmissionAfterCancellation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	snapshot := rootStringCallSnapshot(t, "cancel", func(context.Context, naatreruntime.Invocation) (string, error) {
		calls.Add(1)
		return "", context.Canceled
	})
	plan := prepareParallelBranches(t, snapshot, "collect", repeatName("cancel", 64), indexAlias("cancel"))
	outcome := plan.Execute(context.Background())
	if calls.Load() > 8 {
		t.Fatalf("cancelled parallel handler calls = %d, want at most 8", calls.Load())
	}
	if len(outcome.Errors) > 8 {
		t.Fatalf("cancelled parallel errors = %d, want 1..8: %#v", len(outcome.Errors), outcome.Errors)
	}
	assertEveryErrorCancelled(t, outcome)
}

// Assembled results depend on declaration order alone, so forcing handlers to
// complete in the exact reverse of declaration order must not change a byte of
// the response.
func TestExecuteParallelResponsesIgnoreHandlerCompletionOrder(t *testing.T) {
	t.Parallel()
	const branches = 8
	types := compositionTypes(t)
	registry := naatreruntime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{Mode: naatreruntime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	var reverse atomic.Bool
	gates := make([]chan struct{}, branches)
	for index := range gates {
		gates[index] = make(chan struct{})
	}
	for index := range branches {
		registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
			Name: "branch" + strconv.Itoa(index), Scope: naatreruntime.RootScope, Kind: protocol.Query,
			Member: naatreruntime.CallMember, Input: schema.TypeID(schema.String),
			Output: schema.TypeID(schema.String), Metadata: completeMetadata(naatreruntime.ReadEffect),
		}, func(context.Context, naatreruntime.Invocation) (string, error) {
			// The last branch finishes first and each earlier branch waits for
			// its successor, inverting completion against declaration.
			if reverse.Load() {
				if index+1 < branches {
					<-gates[index+1]
				}
				close(gates[index])
			}
			return "value" + strconv.Itoa(index), nil
		}))
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	names := make([]string, branches)
	for index := range names {
		names[index] = "branch" + strconv.Itoa(index)
	}
	// Aliases descend while declaration ascends, so assembling a branch result
	// into a slot chosen by completion rather than by declaration pairs each
	// alias with the wrong value and shows up in the bytes.
	plan := prepareParallelBranches(t, snapshot, "", names, func(index int) string {
		return string(rune('a' + branches - 1 - index))
	})
	natural, err := json.Marshal(plan.Execute(context.Background()))
	if err != nil {
		t.Fatalf("marshal natural order: %v", err)
	}
	reverse.Store(true)
	inverted, err := json.Marshal(plan.Execute(context.Background()))
	if err != nil {
		t.Fatalf("marshal inverted order: %v", err)
	}
	if string(inverted) != string(natural) {
		t.Fatalf("reverse-completion response = %s, want %s", inverted, natural)
	}
}

// Cancellation signals work to stop; Execute must still join every branch it
// started before returning, so no runtime-owned handler outlives the response.
func TestExecuteJoinsEveryStartedBranchUnderRepeatedCancellation(t *testing.T) {
	t.Parallel()
	var running, started atomic.Int64
	snapshot := rootStringCallSnapshot(t, "block", func(ctx context.Context, _ naatreruntime.Invocation) (string, error) {
		running.Add(1)
		started.Add(1)
		defer running.Add(-1)
		<-ctx.Done()
		return "", ctx.Err()
	})
	plan := prepareParallelBranches(t, snapshot, "collect", repeatName("block", 32), indexAlias("b"))
	for iteration := range 16 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan naatreruntime.Outcome, 1)
		go func() { done <- plan.Execute(ctx) }()
		for started.Load() == 0 {
			goruntime.Gosched()
		}
		cancel()
		outcome := <-done
		// Execute returned, so every handler it admitted has already exited.
		if left := running.Load(); left != 0 {
			t.Fatalf("iteration %d left %d handlers running after Execute returned", iteration, left)
		}
		assertEveryErrorCancelled(t, outcome)
		started.Store(0)
	}
}

// Fail-fast stops admitting work after a terminal branch. The winning subset
// depends on the schedule, so only the invariants are asserted.
func TestExecuteFailFastParallelReportsCompletedSubsetInvariants(t *testing.T) {
	t.Parallel()
	types := compositionTypes(t)
	registry := naatreruntime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{Mode: naatreruntime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
		Name: "boom", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(naatreruntime.ReadEffect),
	}, func(context.Context, naatreruntime.Invocation) (string, error) {
		return "", errors.New("terminal branch")
	}))
	registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
		Name: "ok", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(naatreruntime.ReadEffect),
	}, func(context.Context, naatreruntime.Invocation) (string, error) {
		return "ok", nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	const width = 32
	names := repeatName("ok", width)
	names[width/2] = "boom"
	declared := make(map[string]bool, width)
	for index := range names {
		declared["b"+strconv.Itoa(index)] = true
	}
	plan := prepareParallelBranches(t, snapshot, "fail-fast", names, indexAlias("b"))
	for iteration := range 64 {
		outcome := plan.Execute(context.Background())
		if len(outcome.Errors) == 0 {
			t.Fatalf("iteration %d fail-fast group reported no terminal error", iteration)
		}
		// Errors stay in stable response-path order.
		for index := 1; index < len(outcome.Errors); index++ {
			if comparePathsForTest(outcome.Errors[index-1].Path, outcome.Errors[index].Path) > 0 {
				t.Fatalf("iteration %d errors are not path-ordered: %#v", iteration, outcome.Errors)
			}
		}
		// The completed subset is whatever the schedule admitted, never a
		// branch that was not declared, and never the terminal branch.
		for alias, value := range outcome.Data {
			if !declared[alias] {
				t.Fatalf("iteration %d emitted undeclared alias %q", iteration, alias)
			}
			if value != "ok" {
				t.Fatalf("iteration %d alias %q = %#v, want \"ok\"", iteration, alias, value)
			}
		}
		if _, emitted := outcome.Data["b"+strconv.Itoa(width/2)]; emitted {
			t.Fatalf("iteration %d emitted the terminal branch", iteration)
		}
	}
}

// comparePathsForTest orders response paths the way the runtime does, so a
// test can assert error ordering without reaching into unexported helpers.
func comparePathsForTest(left, right []any) int {
	for index := 0; index < len(left) && index < len(right); index++ {
		leftName, leftString := left[index].(string)
		rightName, rightString := right[index].(string)
		switch {
		case leftString && rightString && leftName != rightName:
			if leftName < rightName {
				return -1
			}
			return 1
		case leftString != rightString:
			if leftString {
				return 1
			}
			return -1
		}
	}
	return len(left) - len(right)
}

// prepareParallelBranches prepares a query whose single parallel group declares
// one aliased call per entry of names under the given policy, aliasing entry i
// as alias(i). An empty policy leaves the group default.
func prepareParallelBranches(t testing.TB, snapshot naatreruntime.Snapshot, policy string, names []string, alias func(int) string) *naatreruntime.Plan {
	t.Helper()
	selections := make([]any, len(names))
	for index, name := range names {
		selections[index] = map[string]any{"$call": map[string]any{"name": name, "as": alias(index)}}
	}
	group := map[string]any{"select": selections}
	if policy != "" {
		group["policy"] = policy
	}
	envelope, err := json.Marshal(map[string]any{"version": "1", "document": map[string]any{
		"operations": []any{map[string]any{"name": "Q", "kind": "query", "select": []any{
			map[string]any{"$parallel": group},
		}}},
	}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request, err := protocol.DecodeRequest(envelope, protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

// repeatName returns a names slice of length count, all set to name.
func repeatName(name string, count int) []string {
	names := make([]string, count)
	for index := range names {
		names[index] = name
	}
	return names
}

// indexAlias aliases branch i as prefix+i.
func indexAlias(prefix string) func(int) string {
	return func(index int) string { return prefix + strconv.Itoa(index) }
}

// rootStringCallSnapshot freezes a registry exposing one root query call of the
// given name backed by handle.
func rootStringCallSnapshot(t testing.TB, name string, handle func(context.Context, naatreruntime.Invocation) (string, error)) naatreruntime.Snapshot {
	t.Helper()
	registry := naatreruntime.NewRegistry(compositionTypes(t))
	if err := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{Mode: naatreruntime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
		Name: name, Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(naatreruntime.ReadEffect),
	}, handle))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	return snapshot
}

// assertEveryErrorCancelled requires at least one error and that cancellation
// is the only reported cause.
func assertEveryErrorCancelled(t testing.TB, outcome naatreruntime.Outcome) {
	t.Helper()
	if len(outcome.Errors) == 0 {
		t.Fatal("cancelled execution reported no error")
	}
	for _, failure := range outcome.Errors {
		if failure.Code != "CANCELLED" {
			t.Fatalf("error = %#v, want CANCELLED", failure)
		}
	}
}
