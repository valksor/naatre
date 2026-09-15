package runtime_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

// uncooperativeChildEnv switches this test binary into the child role. The
// uncooperative handler below never returns, so it is exercised only in a
// supervised subprocess the parent can terminate, rather than leaking a
// permanently blocked goroutine into the shared test process.
const uncooperativeChildEnv = "NAATRE_UNCOOPERATIVE_HANDLER_CHILD"

// A handler that ignores cancellation must not hold the response. Execute
// abandons it once the grace period elapses, reports the selection as
// cancelled, and returns while the handler is still running.
func TestExecuteAbandonsUncooperativeHandlerWithinGrace(t *testing.T) {
	if os.Getenv(uncooperativeChildEnv) == "1" {
		runUncooperativeChild(t)
		return
	}
	command := exec.Command(os.Args[0],
		"-test.run=^TestExecuteAbandonsUncooperativeHandlerWithinGrace$",
		"-test.timeout=60s", "-test.v")
	command.Env = append(os.Environ(), uncooperativeChildEnv+"=1")
	started := time.Now()
	output, err := command.CombinedOutput()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("supervised child failed after %s: %v\n%s", elapsed, err, output)
	}
	if !strings.Contains(string(output), "PASS") {
		t.Fatalf("supervised child did not pass:\n%s", output)
	}
	// The child blocks a handler forever; finishing at all proves the response
	// was bounded rather than joined to that handler.
	if elapsed > 50*time.Second {
		t.Fatalf("supervised child took %s, want a bounded response", elapsed)
	}
}

func runUncooperativeChild(t *testing.T) {
	t.Helper()
	// Never closed: the handler ignores cancellation for the process lifetime.
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	snapshot := rootStringCallSnapshot(t, "stuck", func(context.Context, naatreruntime.Invocation) (string, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
		return "unreachable", nil
	})
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"stuck"}}]}]}}`)
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan naatreruntime.Outcome, 1)
	go func() {
		done <- plan.ExecuteWith(ctx, naatreruntime.ExecuteOptions{AbandonGrace: 50 * time.Millisecond})
	}()
	<-entered
	cancel()
	select {
	case outcome := <-done:
		assertEveryErrorCancelled(t, outcome)
		if len(outcome.Data) != 0 {
			t.Fatalf("abandoned handler produced data = %#v", outcome.Data)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Execute never returned while a handler ignored cancellation")
	}
}

// A handler that observes cancellation is joined, not abandoned, so the grace
// period is a bound on uncooperative work rather than an added latency.
func TestExecuteJoinsCooperativeHandlerWithoutWaitingForGrace(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	returned := make(chan struct{}, 1)
	snapshot := rootStringCallSnapshot(t, "cooperative", func(ctx context.Context, _ naatreruntime.Invocation) (string, error) {
		entered <- struct{}{}
		<-ctx.Done()
		returned <- struct{}{}
		return "", ctx.Err()
	})
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"cooperative"}}]}]}}`)
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan naatreruntime.Outcome, 1)
	go func() {
		// An hour of grace would dominate if a cooperative handler ever waited
		// for it, so returning promptly proves the join is result-driven.
		done <- plan.ExecuteWith(ctx, naatreruntime.ExecuteOptions{AbandonGrace: time.Hour})
	}()
	// Cancel only once the handler is running, so the executor cannot skip it
	// at the sequence guard and leave nothing to join.
	<-entered
	cancel()
	select {
	case outcome := <-done:
		assertEveryErrorCancelled(t, outcome)
	case <-time.After(30 * time.Second):
		t.Fatal("Execute waited for the grace period despite a cooperative handler")
	}
	// Execute already returned, so the joined handler has necessarily returned.
	select {
	case <-returned:
	default:
		t.Fatal("Execute returned before the cooperative handler did")
	}
}

// A parent that held its execution permit while its children ran would
// deadlock as soon as nesting exceeded the permit count. Width-one parallel
// groups nested deeper than the bound must still terminate.
func TestExecuteNestedWidthOneParallelGroupsDoNotDeadlock(t *testing.T) {
	t.Parallel()
	// Deeper than the runtime's concurrency bound, so a permit held across a
	// child selection exhausts the pool before the innermost field is reached.
	const depth = 12
	types := freezeCompositionTypes(t, schema.TypeDescriptor{
		ID: "Node", Kind: schema.ObjectType, Output: true, MaxDepth: depth + 8,
		Fields: map[string]schema.FieldDescriptor{
			"next": {Type: "Node"},
			"id":   {Type: schema.TypeID(schema.String)},
		},
	})
	registry := naatreruntime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{Mode: naatreruntime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerComposition(t, registry, naatreruntime.BindInvocation[map[string]any](naatreruntime.Descriptor{
		Name: "root", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Node", Metadata: completeMetadata(naatreruntime.ReadEffect),
	}, func(context.Context, naatreruntime.Invocation) (map[string]any, error) {
		return map[string]any{"next": map[string]any{}}, nil
	}))
	registerComposition(t, registry, naatreruntime.BindField[map[string]any, map[string]any](naatreruntime.Descriptor{
		Name: "next", Scope: naatreruntime.ObjectScope, Owner: "Node", Member: naatreruntime.FieldMember,
		Output: "Node", Metadata: completeMetadata(naatreruntime.ReadEffect),
	}, func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"next": map[string]any{}}, nil
	}))
	registerComposition(t, registry, naatreruntime.BindField[map[string]any, string](naatreruntime.Descriptor{
		Name: "id", Scope: naatreruntime.ObjectScope, Owner: "Node", Member: naatreruntime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(naatreruntime.ReadEffect),
	}, func(context.Context, map[string]any) (string, error) {
		return "leaf", nil
	}))
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	// Each level is a width-one parallel group wrapping the next field, with a
	// scalar leaf at the bottom so every composite selection is complete.
	selection := `{"$field":{"name":"id"}}`
	for range depth {
		selection = `{"$parallel":{"select":[{"$field":{"name":"next","select":[` + selection + `]}}]}}`
	}
	document := `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"root","select":[` + selection + `]}}]}]}}`
	// Each level costs several JSON levels, so this document needs more than
	// the default nesting allowance to express a bound-exceeding depth.
	request, err := protocol.DecodeRequest([]byte(document), protocol.DecodeOptions{
		Limits: protocol.Limits{MaxDepth: 256},
	})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	done := make(chan naatreruntime.Outcome, 1)
	go func() { done <- plan.Execute(context.Background()) }()
	select {
	case outcome := <-done:
		if len(outcome.Errors) != 0 {
			t.Fatalf("Execute errors = %#v", outcome.Errors)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("nested width-one parallel groups deadlocked on the execution bound")
	}
}

// A saturated pool plus queued branches must still terminate once the request
// is cancelled, rather than waiting out every queued admission.
func TestExecuteSaturatedPoolTerminatesOnCancellation(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	snapshot := rootStringCallSnapshot(t, "saturate", func(ctx context.Context, _ naatreruntime.Invocation) (string, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	})
	// Far more branches than permits, so most are still queued at cancellation.
	plan := prepareParallelBranches(t, snapshot, "collect", repeatName("saturate", 256), indexAlias("s"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan naatreruntime.Outcome, 1)
	go func() { done <- plan.ExecuteWith(ctx, naatreruntime.ExecuteOptions{AbandonGrace: time.Second}) }()
	<-entered
	cancel()
	select {
	case outcome := <-done:
		assertEveryErrorCancelled(t, outcome)
	case <-time.After(30 * time.Second):
		t.Fatal("saturated pool did not terminate after cancellation")
	}
}
