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
	types := compositionTypes(t)
	registry := naatreruntime.NewRegistry(types)
	var calls atomic.Int64
	registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
		Name: "cancel", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(naatreruntime.ReadEffect),
	}, func(context.Context, naatreruntime.Invocation) (string, error) {
		calls.Add(1)
		return "", context.Canceled
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	branches := make([]any, 64)
	for index := range branches {
		branches[index] = map[string]any{"$call": map[string]any{"name": "cancel", "as": "cancel" + strconv.Itoa(index)}}
	}
	document := map[string]any{"operations": []any{map[string]any{
		"name": "Q", "kind": "query", "select": []any{map[string]any{"$parallel": map[string]any{"policy": "collect", "select": branches}}},
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
	outcome := plan.Execute(context.Background())
	if calls.Load() > 8 {
		t.Fatalf("cancelled parallel handler calls = %d, want at most 8", calls.Load())
	}
	if len(outcome.Errors) == 0 || len(outcome.Errors) > 8 {
		t.Fatalf("cancelled parallel errors = %d, want 1..8: %#v", len(outcome.Errors), outcome.Errors)
	}
	for _, failure := range outcome.Errors {
		if failure.Code != "CANCELLED" {
			t.Fatalf("parallel error = %#v, want CANCELLED", failure)
		}
	}
}
