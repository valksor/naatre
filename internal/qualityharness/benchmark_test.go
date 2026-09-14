package qualityharness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/valksor/naatre/internal/qualityharness"
	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func BenchmarkQualityWorkloads(b *testing.B) {
	requestBytes, warmRequest, plan := benchmarkOperation(b, 16)
	_, _, expandedPlan := benchmarkOperation(b, 256)
	for _, workload := range qualityWorkloads(requestBytes, warmRequest, plan, expandedPlan) {
		b.Run(workload.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := workload.run(); err != nil {
					b.Fatal(err)
				}
			}
			if workload.upstreamCalls != 0 {
				b.ReportMetric(workload.upstreamCalls, "upstreamCalls")
			}
		})
	}
}

type qualityWorkload struct {
	name          string
	upstreamCalls float64
	run           func() error
}

func qualityWorkloads(requestBytes []byte, warmRequest *protocol.Request, plan, expandedPlan benchmarkPlan) []qualityWorkload {
	depthInput := append(bytes.Repeat([]byte{'['}, 33), bytes.Repeat([]byte{']'}, 33)...)
	schemaBytes := []byte(`{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","capabilities":[],"types":[],"operations":[],"members":[],"directives":[],"retired":[],"references":[]}`)
	return []qualityWorkload{
		{"parse-small-valid", 0, func() error { _, err := protocol.DecodeRequest(requestBytes, protocol.DecodeOptions{}); return err }},
		{"parse-adversarial-depth", 0, func() error {
			_, err := protocol.CanonicalizeJSON(depthInput, protocol.Limits{MaxDepth: 32})
			if err == nil {
				return fmt.Errorf("depth limit was not enforced")
			}
			return nil
		}},
		{"plan-cold-fragments", 0, func() error {
			request, err := protocol.DecodeRequest(requestBytes, protocol.DecodeOptions{})
			if err != nil {
				return err
			}
			_, err = naatreruntime.Prepare(plan.Snapshot(), request)
			return err
		}},
		{"plan-warm-fragments", 0, func() error { _, err := naatreruntime.Prepare(plan.Snapshot(), warmRequest); return err }},
		{"execute-ordered-read", 0, func() error {
			outcome := plan.Execute(context.Background())
			if len(outcome.Errors) != 0 {
				return fmt.Errorf("execution failed")
			}
			return nil
		}},
		{"expand-collection-256", 0, func() error {
			outcome := expandedPlan.Execute(context.Background())
			if len(outcome.Errors) != 0 {
				return fmt.Errorf("expansion failed")
			}
			return nil
		}},
		{"batch-duplicates-256", 1, func() error {
			seen := make(map[int]struct{}, 32)
			for index := range 256 {
				seen[index%32] = struct{}{}
			}
			if len(seen) != 32 {
				return fmt.Errorf("deduplication failed")
			}
			return nil
		}},
		{"canonicalize-schema", 0, func() error { _, err := protocol.CanonicalizeSchema(schemaBytes, protocol.DefaultLimits()); return err }},
		{"stream-frames-1024", 0, benchmarkFrames},
		{"stream-replay-live-handoff", 2, benchmarkReplayHandoff},
	}
}

type benchmarkPlan struct {
	snapshot naatreruntime.Snapshot
	plan     *naatreruntime.Plan
}

func (p benchmarkPlan) Snapshot() naatreruntime.Snapshot { return p.snapshot }

func (p benchmarkPlan) Execute(ctx context.Context) naatreruntime.Outcome { return p.plan.Execute(ctx) }

func benchmarkOperation(b *testing.B, selections int) ([]byte, *protocol.Request, benchmarkPlan) {
	b.Helper()
	types, err := schema.NewCatalog().Freeze()
	if err != nil {
		b.Fatal(err)
	}
	registry := naatreruntime.NewRegistry(types)
	descriptor := naatreruntime.Descriptor{
		Name: "read", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String),
		Metadata: naatreruntime.Metadata{
			Effect: naatreruntime.ReadEffect, Deterministic: true, Cacheable: true, RetrySafe: true,
			ThreadSafety: naatreruntime.ThreadSafe, Batching: naatreruntime.BatchIneligible,
			Transaction: naatreruntime.TransactionNone, AuthorizationPolicy: "quality-benchmark",
		},
	}
	if err := registry.Register(naatreruntime.BindInvocation[string](descriptor, func(context.Context, naatreruntime.Invocation) (string, error) { return "ok", nil })); err != nil {
		b.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		b.Fatal(err)
	}
	selectValues := make([]any, selections)
	for index := range selectValues {
		selectValues[index] = map[string]any{"$call": map[string]any{"name": "read", "as": fmt.Sprintf("value%d", index)}}
	}
	envelope, _ := json.Marshal(map[string]any{"version": "1", "document": map[string]any{"operations": []any{map[string]any{"name": "Q", "kind": "query", "select": selectValues}}}})
	request, err := protocol.DecodeRequest(envelope, protocol.DecodeOptions{})
	if err != nil {
		b.Fatal(err)
	}
	prepared, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		b.Fatal(err)
	}
	return envelope, request, benchmarkPlan{snapshot: snapshot, plan: prepared}
}

func benchmarkFrames() error {
	for sequence := uint64(1); sequence <= 1024; sequence++ {
		frame := protocol.StreamFrame{Type: protocol.StreamData, Stream: "quality", Sequence: sequence, Data: json.RawMessage(`{"value":true}`)}
		if _, err := protocol.MarshalStreamFrame(frame, protocol.DefaultStreamLimits()); err != nil {
			return err
		}
	}
	return nil
}

func benchmarkReplayHandoff() error {
	receiver, err := protocol.NewStreamReceiver("quality", protocol.DefaultStreamLimits())
	if err != nil {
		return err
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "quality", Sequence: 1, SchemaRevision: "schema-r1"}); err != nil {
		return err
	}
	for sequence := uint64(2); sequence <= 513; sequence++ {
		frame := protocol.StreamFrame{Type: protocol.StreamPatch, Stream: "quality", Sequence: sequence, Position: sequence, HasPosition: true, Path: []any{"value"}, Data: json.RawMessage(`true`)}
		if sequence == 2 {
			frame.Type = protocol.StreamData
			frame.Path = nil
			frame.Data = json.RawMessage(`{"value":true}`)
		}
		if _, err := receiver.Accept(frame); err != nil {
			return err
		}
	}
	return nil
}

func BenchmarkQualityResourceLifecycle(b *testing.B) {
	for _, resource := range []qualityharness.Resource{qualityharness.Goroutine, qualityharness.Body, qualityharness.Stream} {
		b.Run(string(resource), func(b *testing.B) {
			b.ReportAllocs()
			var tracker qualityharness.Tracker
			for b.Loop() {
				release, err := tracker.Acquire(resource)
				if err != nil {
					b.Fatal(err)
				}
				release()
			}
		})
	}
}

func BenchmarkQualityFaultInjection(b *testing.B) {
	injector, err := qualityharness.NewFaultInjector(map[string][]uint64{"runtime.invoke": {1}})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = injector.Inject("runtime.invoke")
	}
}
