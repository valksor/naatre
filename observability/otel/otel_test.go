package otel

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

type memoryLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

type panicSpanProcessor struct{}

func (panicSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {
	panic("provider credential=top-secret internal/type")
}

func (panicSpanProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (panicSpanProcessor) Shutdown(context.Context) error   { return nil }
func (panicSpanProcessor) ForceFlush(context.Context) error { return nil }

func (e *memoryLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for index := range records {
		e.records = append(e.records, records[index].Clone())
	}
	return nil
}

func (*memoryLogExporter) Shutdown(context.Context) error   { return nil }
func (*memoryLogExporter) ForceFlush(context.Context) error { return nil }

func (e *memoryLogExporter) snapshot() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.records...)
}

func TestAdapterExportsRedactedCausalTracesAndLogs(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	logs := &memoryLogExporter{}
	loggerProvider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
	adapter, err := New(Config{TracerProvider: tracerProvider, LoggerProvider: loggerProvider})
	if err != nil {
		t.Fatal(err)
	}
	hooks := adapter.Hooks()
	requestStart := runtime.TelemetryEvent{
		Kind: runtime.TelemetryRequest, Stage: runtime.TelemetryStarted, ID: "request-secret",
		RequestID: "bearer credential", OperationName: "private operation", PrincipalReference: "principal@example.test",
		OperationKind: protocol.Query, Outcome: runtime.TelemetryActive,
	}
	operationStart := runtime.TelemetryEvent{
		Kind: runtime.TelemetryOperation, Stage: runtime.TelemetryStarted, ID: "operation-secret", ParentID: "request-secret",
		RequestID: requestStart.RequestID, OperationName: requestStart.OperationName, PrincipalReference: requestStart.PrincipalReference,
		OperationKind: protocol.Query, Outcome: runtime.TelemetryActive,
	}
	for _, event := range []runtime.TelemetryEvent{requestStart, operationStart} {
		if err := hooks.Trace(event); err != nil {
			t.Fatal(err)
		}
		if err := hooks.Log(event); err != nil {
			t.Fatal(err)
		}
	}
	operationEnd := operationStart
	operationEnd.Stage, operationEnd.Outcome, operationEnd.Duration = runtime.TelemetryCompleted, runtime.TelemetrySucceeded, time.Millisecond
	requestEnd := requestStart
	requestEnd.Stage, requestEnd.Outcome, requestEnd.Duration = runtime.TelemetryCompleted, runtime.TelemetrySucceeded, 2*time.Millisecond
	for _, event := range []runtime.TelemetryEvent{operationEnd, requestEnd} {
		if err := hooks.Trace(event); err != nil {
			t.Fatal(err)
		}
		if err := hooks.Log(event); err != nil {
			t.Fatal(err)
		}
	}
	ended := spans.Ended()
	if len(ended) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(ended))
	}
	operation, request := ended[0], ended[1]
	if operation.Parent().SpanID() != request.SpanContext().SpanID() || operation.SpanContext().TraceID() != request.SpanContext().TraceID() {
		t.Fatalf("operation parent = %s, request = %s", operation.Parent().SpanID(), request.SpanContext().SpanID())
	}
	prohibited := []string{"request-secret", "operation-secret", "bearer credential", "private operation", "principal@example.test"}
	for _, span := range ended {
		assertNoProtectedText(t, fmt.Sprint(span.Attributes()), prohibited)
		if span.Name() != "naatre."+attributeValue(span.Attributes(), "naatre.kind") {
			t.Fatalf("span name %q is not bounded by kind", span.Name())
		}
	}
	records := logs.snapshot()
	if len(records) != 4 {
		t.Fatalf("log records = %d, want 4", len(records))
	}
	for _, record := range records {
		var attrs []attribute.KeyValue
		record.WalkAttributes(func(value attribute.KeyValue) bool {
			attrs = append(attrs, value)
			return true
		})
		assertNoProtectedText(t, fmt.Sprint(attrs), prohibited)
		if !record.TraceID().IsValid() || !record.SpanID().IsValid() {
			t.Fatalf("log record lacks trace correlation: %#v", record)
		}
	}
}

func TestAdapterPreservesCompletedParentAndReplayLinks(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	adapter, err := New(Config{TracerProvider: provider, MaxCausalReferences: 2})
	if err != nil {
		t.Fatal(err)
	}
	hook := adapter.Hooks().Trace
	if err := hook(runtime.TelemetryEvent{Kind: runtime.TelemetryRequest, Stage: runtime.TelemetryStarted, ID: "request"}); err != nil {
		t.Fatal(err)
	}
	if err := hook(runtime.TelemetryEvent{Kind: runtime.TelemetryRequest, Stage: runtime.TelemetryCompleted, ID: "request", Outcome: runtime.TelemetrySucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := hook(runtime.TelemetryEvent{
		Kind: runtime.TelemetrySubscription, Stage: runtime.TelemetryStage(runtime.SubscriptionReplayStarted),
		ID: "replay", ParentID: "request", Links: []string{"request"}, ReplayAttempt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	ended := spans.Ended()
	if len(ended) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(ended))
	}
	request, replay := ended[0], ended[1]
	if replay.Parent().SpanID() != request.SpanContext().SpanID() || replay.SpanContext().TraceID() != request.SpanContext().TraceID() {
		t.Fatalf("replay did not preserve completed parent: parent=%s request=%s", replay.Parent().SpanID(), request.SpanContext().SpanID())
	}
	if len(replay.Links()) != 1 || replay.Links()[0].SpanContext.SpanID() != request.SpanContext().SpanID() {
		t.Fatalf("replay links = %#v", replay.Links())
	}
	if attributeValue(replay.Attributes(), "naatre.signal.parent_id") != reference("request") ||
		!strings.Contains(fmt.Sprint(replay.Attributes()), reference("request")) {
		t.Fatalf("portable causal references are missing: %v", replay.Attributes())
	}
}

func TestAdapterMetricsHaveOnlyBoundedLabels(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	adapter, err := New(Config{MeterProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.Hooks().Metric(runtime.MetricEvent{
		Kind: "attacker-controlled-kind", Stage: "attacker-controlled-stage", OperationKind: "attacker-controlled-operation",
		Outcome: "attacker-controlled-outcome", ErrorCode: "ATTACKER_CONTROLLED_CODE",
		Duration: time.Second, Cost: ^uint64(0), BatchSize: 3, ReplayBytes: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	allowed := []string{"naatre.kind", "naatre.stage", "naatre.operation.kind", "naatre.outcome", "naatre.error.code"}
	foundEvents := false
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "naatre.lifecycle.events" {
				continue
			}
			foundEvents = true
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) != 1 {
				t.Fatalf("event metric data = %#v", metric.Data)
			}
			attrs := sum.DataPoints[0].Attributes.ToSlice()
			for _, value := range attrs {
				if !slices.Contains(allowed, string(value.Key)) {
					t.Fatalf("unbounded metric label %q", value.Key)
				}
			}
			text := fmt.Sprint(attrs)
			for _, prohibited := range []string{"attacker-controlled", "ATTACKER_CONTROLLED"} {
				if strings.Contains(text, prohibited) {
					t.Fatalf("metric labels contain unbounded input: %s", text)
				}
			}
			if !strings.Contains(text, runtime.CodeInternal) {
				t.Fatalf("invalid metric dimensions did not normalize to INTERNAL: %s", text)
			}
		}
	}
	if !foundEvents {
		t.Fatal("lifecycle event metric was not collected")
	}
}

func TestAdapterLimitsActiveStateAndCausalInputs(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	adapter, err := New(Config{TracerProvider: provider, MaxActiveSpans: 1, MaxCausalReferences: 1, MaxLinks: 1})
	if err != nil {
		t.Fatal(err)
	}
	hook := adapter.Hooks().Trace
	if err := hook(runtime.TelemetryEvent{Kind: runtime.TelemetryRequest, Stage: runtime.TelemetryStarted, ID: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := hook(runtime.TelemetryEvent{Kind: runtime.TelemetryOperation, Stage: runtime.TelemetryStarted, ID: "second"}); CodeOf(err) != CodeResourceExhausted {
		t.Fatalf("active span limit error = %v", err)
	} else if strings.Contains(err.Error(), "second") {
		t.Fatalf("resource error exposed identity: %v", err)
	}
	if err := hook(runtime.TelemetryEvent{Kind: runtime.TelemetrySubscription, Stage: runtime.TelemetryStage(runtime.SubscriptionResumed), ID: "links", Links: []string{"one", "two"}}); CodeOf(err) != CodeResourceExhausted {
		t.Fatalf("causal link limit error = %v", err)
	}
	if err := hook(runtime.TelemetryEvent{Kind: runtime.TelemetryRequest, Stage: runtime.TelemetryCancelled, ID: "first", Outcome: runtime.TelemetryCancelledOutcome, ErrorCode: runtime.CodeCancelled}); err != nil {
		t.Fatal(err)
	}
	if len(adapter.active) != 0 || len(adapter.contexts) > 1 || len(adapter.contextIDs) > 1 {
		t.Fatalf("retained state exceeds limits: active=%d contexts=%d ids=%d", len(adapter.active), len(adapter.contexts), len(adapter.contextIDs))
	}
	if len(spans.Ended()) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans.Ended()))
	}
}

func TestAdapterRejectsInvalidConfigurationAndEvent(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{MaxLinks: -1}); CodeOf(err) != CodeInvalidConfig {
		t.Fatalf("negative limit error = %v", err)
	}
	adapter, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Hooks().Trace(runtime.TelemetryEvent{Kind: runtime.TelemetryRequest, Stage: runtime.TelemetryStarted}); CodeOf(err) != CodeInvalidEvent {
		t.Fatalf("missing identity error = %v", err)
	}
	if hooks := (*Adapter)(nil).Hooks(); hooks.Trace != nil || hooks.Metric != nil || hooks.Log != nil {
		t.Fatalf("nil adapter hooks = %#v", hooks)
	}
}

func TestAdapterContainsProviderPanicAsStableFailure(t *testing.T) {
	t.Parallel()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(panicSpanProcessor{}))
	adapter, err := New(Config{TracerProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	hook := adapter.Hooks().Trace
	for _, id := range []string{"first-secret", "second-secret"} {
		err := hook(runtime.TelemetryEvent{Kind: runtime.TelemetryRequest, Stage: runtime.TelemetryStarted, ID: id})
		if CodeOf(err) != CodeExportFailure {
			t.Fatalf("provider panic error = %v", err)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "internal/type") {
			t.Fatalf("provider panic detail escaped: %v", err)
		}
	}
	if len(adapter.active) != 0 {
		t.Fatalf("panicking provider retained %d active spans", len(adapter.active))
	}
}

func attributeValue(attrs []attribute.KeyValue, key string) string {
	for _, value := range attrs {
		if string(value.Key) == key {
			return value.Value.AsString()
		}
	}
	return ""
}

func assertNoProtectedText(t *testing.T, text string, prohibited []string) {
	t.Helper()
	index := slices.IndexFunc(prohibited, func(value string) bool {
		return strings.Contains(text, value)
	})
	if index >= 0 {
		t.Fatalf("telemetry exposed protected value %q: %s", prohibited[index], text)
	}
}
