package otel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sync"
	"time"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	otelmetric "go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/valksor/naatre/runtime"
)

const (
	CodeInvalidConfig     = "OTEL_INVALID_CONFIG"
	CodeInvalidEvent      = "OTEL_INVALID_EVENT"
	CodeInitialization    = "OTEL_INITIALIZATION_FAILED"
	CodeExportFailure     = "OTEL_EXPORT_FAILED"
	CodeResourceExhausted = "RESOURCE_EXHAUSTED"
)

const (
	instrumentationName        = "github.com/valksor/naatre/observability/otel"
	defaultMaxActiveSpans      = 1024
	defaultMaxCausalReferences = 4096
	defaultMaxLinks            = 32
)

// Error is safe to expose at an application boundary. OpenTelemetry provider
// errors and implementation details are deliberately not retained.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Config selects providers and resource limits. Nil providers use the current
// OpenTelemetry globals. Providers and global configuration are never owned or
// shut down by Adapter.
type Config struct {
	TracerProvider      oteltrace.TracerProvider
	MeterProvider       otelmetric.MeterProvider
	LoggerProvider      otellog.LoggerProvider
	MaxActiveSpans      int
	MaxCausalReferences int
	MaxLinks            int
}

// Adapter is a concurrency-safe bridge from runtime hooks to OpenTelemetry.
type Adapter struct {
	tracer  oteltrace.Tracer
	logger  otellog.Logger
	metrics metricInstruments

	maxActiveSpans      int
	maxCausalReferences int
	maxLinks            int

	mu         sync.Mutex
	active     map[string]oteltrace.Span
	contexts   map[string]oteltrace.SpanContext
	contextIDs []string
}

type metricInstruments struct {
	events            otelmetric.Int64Counter
	duration          otelmetric.Float64Histogram
	cost              otelmetric.Int64Histogram
	batchSize         otelmetric.Int64Histogram
	attempt           otelmetric.Int64Histogram
	connectionAttempt otelmetric.Int64Histogram
	replayAttempt     otelmetric.Int64Histogram
	activeStreams     otelmetric.Int64Gauge
	replayEvents      otelmetric.Int64Histogram
	replayBytes       otelmetric.Int64Histogram
	scannedCandidates otelmetric.Int64Histogram
	drainRemaining    otelmetric.Int64Histogram
}

func New(config Config) (adapter *Adapter, err error) {
	defer func() {
		if recover() != nil {
			adapter = nil
			err = initializationError()
		}
	}()
	if config.MaxActiveSpans < 0 || config.MaxCausalReferences < 0 || config.MaxLinks < 0 {
		return nil, publicError(CodeInvalidConfig, "OpenTelemetry limits must not be negative")
	}
	if config.MaxActiveSpans == 0 {
		config.MaxActiveSpans = defaultMaxActiveSpans
	}
	if config.MaxCausalReferences == 0 {
		config.MaxCausalReferences = defaultMaxCausalReferences
	}
	if config.MaxLinks == 0 {
		config.MaxLinks = defaultMaxLinks
	}
	if config.TracerProvider == nil {
		config.TracerProvider = otelapi.GetTracerProvider()
	}
	if config.MeterProvider == nil {
		config.MeterProvider = otelapi.GetMeterProvider()
	}
	if config.LoggerProvider == nil {
		config.LoggerProvider = logglobal.GetLoggerProvider()
	}
	metrics, err := newMetricInstruments(config.MeterProvider.Meter(instrumentationName))
	if err != nil {
		return nil, err
	}
	return &Adapter{
		tracer:              config.TracerProvider.Tracer(instrumentationName),
		logger:              config.LoggerProvider.Logger(instrumentationName),
		metrics:             metrics,
		maxActiveSpans:      config.MaxActiveSpans,
		maxCausalReferences: config.MaxCausalReferences,
		maxLinks:            config.MaxLinks,
		active:              make(map[string]oteltrace.Span),
		contexts:            make(map[string]oteltrace.SpanContext),
		contextIDs:          make([]string, 0, config.MaxCausalReferences),
	}, nil
}

// Hooks returns the dependency-free runtime hook set. Adapter remains owned by
// the application and may be shared by plans and concurrent requests.
func (a *Adapter) Hooks() runtime.TelemetryHooks {
	if a == nil {
		return runtime.TelemetryHooks{}
	}
	return runtime.TelemetryHooks{Trace: a.safeTrace, Metric: a.safeMetric, Log: a.safeLog}
}

func (a *Adapter) safeTrace(event runtime.TelemetryEvent) (err error) {
	defer recoverExportFailure(&err)
	return a.trace(event)
}

func (a *Adapter) safeMetric(event runtime.MetricEvent) (err error) {
	defer recoverExportFailure(&err)
	return a.metric(event)
}

func (a *Adapter) safeLog(event runtime.TelemetryEvent) (err error) {
	defer recoverExportFailure(&err)
	return a.log(event)
}

func (a *Adapter) trace(event runtime.TelemetryEvent) error {
	event, err := a.safeEvent(event)
	if err != nil {
		return err
	}
	key := reference(event.ID)
	if isTerminal(event) {
		return a.finishSpan(event, key)
	}
	if isStart(event) {
		return a.startSpan(event)
	}
	if event.Kind == runtime.TelemetryTransaction {
		if a.addTransactionEvent(event, key) {
			return nil
		}
	}
	return a.instantSpan(event, false)
}

func (a *Adapter) finishSpan(event runtime.TelemetryEvent, key string) error {
	span, links := a.spanAndLinks(key, event.Links, true)
	if span == nil {
		return a.instantSpan(event, true)
	}
	span.SetAttributes(traceAttributes(event)...)
	for _, link := range links {
		span.AddLink(link)
	}
	setStatus(span, event)
	span.End()
	return nil
}

func (a *Adapter) addTransactionEvent(event runtime.TelemetryEvent, key string) bool {
	span, links := a.spanAndLinks(key, event.Links, false)
	if span == nil {
		return false
	}
	span.AddEvent("naatre."+string(event.Stage), oteltrace.WithAttributes(traceAttributes(event)...))
	for _, link := range links {
		span.AddLink(link)
	}
	return true
}

func (a *Adapter) spanAndLinks(key string, references []string, remove bool) (oteltrace.Span, []oteltrace.Link) {
	a.mu.Lock()
	defer a.mu.Unlock()
	span := a.active[key]
	if span != nil && remove {
		delete(a.active, key)
	}
	return span, a.traceLinksLocked(references)
}

func (a *Adapter) startSpan(event runtime.TelemetryEvent) error {
	key := reference(event.ID)
	a.mu.Lock()
	if _, exists := a.active[key]; exists {
		a.mu.Unlock()
		return publicError(CodeInvalidEvent, "telemetry lifecycle already started")
	}
	if len(a.active) >= a.maxActiveSpans {
		a.mu.Unlock()
		return publicError(CodeResourceExhausted, "active telemetry span limit exceeded")
	}
	ctx, links := a.traceContextLocked(event)
	a.mu.Unlock()
	_, span := a.tracer.Start(ctx, "naatre."+string(event.Kind),
		oteltrace.WithSpanKind(spanKind(event.Kind)),
		oteltrace.WithAttributes(traceAttributes(event)...),
		oteltrace.WithLinks(links...),
	)
	a.mu.Lock()
	if _, exists := a.active[key]; exists {
		a.mu.Unlock()
		span.End()
		return publicError(CodeInvalidEvent, "telemetry lifecycle already started")
	}
	if len(a.active) >= a.maxActiveSpans {
		a.mu.Unlock()
		span.End()
		return publicError(CodeResourceExhausted, "active telemetry span limit exceeded")
	}
	a.active[key] = span
	a.retainContextLocked(key, span.SpanContext())
	a.mu.Unlock()
	return nil
}

func (a *Adapter) instantSpan(event runtime.TelemetryEvent, orphan bool) error {
	a.mu.Lock()
	ctx, links := a.traceContextLocked(event)
	a.mu.Unlock()
	attrs := traceAttributes(event)
	if orphan {
		attrs = append(attrs, attribute.Bool("naatre.lifecycle.orphan", true))
	}
	_, span := a.tracer.Start(ctx, "naatre."+string(event.Kind),
		oteltrace.WithSpanKind(spanKind(event.Kind)),
		oteltrace.WithAttributes(attrs...),
		oteltrace.WithLinks(links...),
	)
	setStatus(span, event)
	span.End()
	key := reference(event.ID)
	a.mu.Lock()
	a.retainContextLocked(key, span.SpanContext())
	a.mu.Unlock()
	return nil
}

func (a *Adapter) metric(event runtime.MetricEvent) error {
	event = runtime.NormalizeMetricEvent(event)
	attrs := otelmetric.WithAttributes(metricAttributes(event)...)
	ctx := context.Background()
	a.metrics.events.Add(ctx, 1, attrs)
	if event.Duration > 0 {
		a.metrics.duration.Record(ctx, event.Duration.Seconds(), attrs)
	}
	a.recordMeasurements(ctx, event, attrs)
	return nil
}

func (a *Adapter) recordMeasurements(ctx context.Context, event runtime.MetricEvent, attrs otelmetric.MeasurementOption) {
	if event.Cost != 0 {
		a.metrics.cost.Record(ctx, boundedUint64(event.Cost), attrs)
	}
	if event.BatchSize != 0 {
		a.metrics.batchSize.Record(ctx, int64(event.BatchSize), attrs)
	}
	if event.Attempt != 0 {
		a.metrics.attempt.Record(ctx, int64(event.Attempt), attrs)
	}
	if event.ConnectionAttempt != 0 {
		a.metrics.connectionAttempt.Record(ctx, int64(event.ConnectionAttempt), attrs)
	}
	if event.ReplayAttempt != 0 {
		a.metrics.replayAttempt.Record(ctx, int64(event.ReplayAttempt), attrs)
	}
	if event.ActiveStreams != 0 {
		a.metrics.activeStreams.Record(ctx, event.ActiveStreams, attrs)
	}
	if event.ReplayEvents != 0 {
		a.metrics.replayEvents.Record(ctx, boundedUint64(event.ReplayEvents), attrs)
	}
	if event.ReplayBytes != 0 {
		a.metrics.replayBytes.Record(ctx, boundedUint64(event.ReplayBytes), attrs)
	}
	if event.ScannedCandidates != 0 {
		a.metrics.scannedCandidates.Record(ctx, boundedUint64(event.ScannedCandidates), attrs)
	}
	if event.DrainRemaining != 0 {
		a.metrics.drainRemaining.Record(ctx, boundedUint64(event.DrainRemaining), attrs)
	}
}

func (a *Adapter) log(event runtime.TelemetryEvent) error {
	event, err := a.safeEvent(event)
	if err != nil {
		return err
	}
	a.mu.Lock()
	ctx := a.logContextLocked(event)
	a.mu.Unlock()
	var record otellog.Record
	now := time.Now()
	record.SetTimestamp(now)
	record.SetObservedTimestamp(now)
	record.SetEventName("naatre.lifecycle")
	record.SetBody(attribute.StringValue("naatre lifecycle"))
	record.SetSeverity(logSeverity(event))
	record.SetSeverityText(logSeverityText(event))
	record.AddAttributes(traceAttributes(event)...)
	a.logger.Emit(ctx, record)
	return nil
}

func (a *Adapter) safeEvent(event runtime.TelemetryEvent) (runtime.TelemetryEvent, error) {
	event = runtime.NormalizeTelemetryEvent(event)
	if event.ID == "" {
		return runtime.TelemetryEvent{}, publicError(CodeInvalidEvent, "telemetry identity is required")
	}
	if len(event.Links) > a.maxLinks {
		return runtime.TelemetryEvent{}, publicError(CodeResourceExhausted, "telemetry causal link limit exceeded")
	}
	return event, nil
}

func (a *Adapter) traceContextLocked(event runtime.TelemetryEvent) (context.Context, []oteltrace.Link) {
	ctx := context.Background()
	parentKey := reference(event.ParentID)
	if parent, ok := a.active[parentKey]; event.ParentID != "" && ok && parent != nil && parent.SpanContext().IsValid() {
		ctx = oteltrace.ContextWithSpanContext(ctx, parent.SpanContext())
	} else if parent, ok := a.contexts[parentKey]; event.ParentID != "" && ok && parent.IsValid() {
		ctx = oteltrace.ContextWithSpanContext(ctx, parent)
	}
	return ctx, a.traceLinksLocked(event.Links)
}

func (a *Adapter) logContextLocked(event runtime.TelemetryEvent) context.Context {
	ctx := context.Background()
	if span, ok := a.active[reference(event.ID)]; ok {
		return oteltrace.ContextWithSpanContext(ctx, span.SpanContext())
	}
	if spanContext, ok := a.contexts[reference(event.ID)]; ok && spanContext.IsValid() {
		return oteltrace.ContextWithSpanContext(ctx, spanContext)
	}
	if spanContext, ok := a.contexts[reference(event.ParentID)]; ok && spanContext.IsValid() {
		return oteltrace.ContextWithSpanContext(ctx, spanContext)
	}
	return ctx
}

func (a *Adapter) traceLinksLocked(references []string) []oteltrace.Link {
	links := make([]oteltrace.Link, 0, len(references))
	for _, raw := range references {
		key := reference(raw)
		if span, ok := a.active[key]; ok && span != nil && span.SpanContext().IsValid() {
			links = append(links, oteltrace.Link{SpanContext: span.SpanContext()})
		} else if spanContext, ok := a.contexts[key]; ok && spanContext.IsValid() {
			links = append(links, oteltrace.Link{SpanContext: spanContext})
		}
	}
	return links
}

func (a *Adapter) retainContextLocked(key string, spanContext oteltrace.SpanContext) {
	if key == "" || !spanContext.IsValid() || a.maxCausalReferences == 0 {
		return
	}
	if _, exists := a.contexts[key]; exists {
		a.contexts[key] = spanContext
		return
	}
	if len(a.contextIDs) == a.maxCausalReferences {
		delete(a.contexts, a.contextIDs[0])
		copy(a.contextIDs, a.contextIDs[1:])
		a.contextIDs = a.contextIDs[:len(a.contextIDs)-1]
	}
	a.contexts[key] = spanContext
	a.contextIDs = append(a.contextIDs, key)
}

func traceAttributes(event runtime.TelemetryEvent) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("naatre.kind", string(event.Kind)),
		attribute.String("naatre.stage", string(event.Stage)),
		attribute.String("naatre.operation.kind", string(event.OperationKind)),
		attribute.String("naatre.outcome", string(event.Outcome)),
		attribute.String("naatre.error.code", event.ErrorCode),
		attribute.String("naatre.signal.id", reference(event.ID)),
		attribute.String("naatre.signal.parent_id", reference(event.ParentID)),
		attribute.String("naatre.request.ref", reference(event.RequestID)),
		attribute.String("naatre.operation.ref", reference(event.OperationID)),
		attribute.String("naatre.operation.name_ref", reference(event.OperationName)),
		attribute.String("naatre.handler.ref", reference(event.Handler)),
		attribute.String("naatre.schema.ref", reference(event.SchemaRevision)),
		attribute.String("naatre.persisted.ref", reference(event.PersistedHash)),
		attribute.String("naatre.principal.ref", reference(event.PrincipalReference)),
		attribute.String("naatre.stream.ref", reference(event.StreamReference)),
		attribute.Int64("naatre.duration_ns", event.Duration.Nanoseconds()),
		attribute.Int64("naatre.cost", boundedUint64(event.Cost)),
		attribute.Int("naatre.batch_size", event.BatchSize),
		attribute.Int64("naatre.attempt", int64(event.Attempt)),
		attribute.Int64("naatre.connection_attempt", int64(event.ConnectionAttempt)),
		attribute.Int64("naatre.replay_attempt", int64(event.ReplayAttempt)),
		attribute.Int64("naatre.active_streams", event.ActiveStreams),
		attribute.Int64("naatre.replay_events", boundedUint64(event.ReplayEvents)),
		attribute.Int64("naatre.replay_bytes", boundedUint64(event.ReplayBytes)),
		attribute.Int64("naatre.scanned_candidates", boundedUint64(event.ScannedCandidates)),
		attribute.Int64("naatre.drain_remaining", boundedUint64(event.DrainRemaining)),
	}
	if len(event.Links) != 0 {
		links := make([]string, len(event.Links))
		for index, link := range event.Links {
			links[index] = reference(link)
		}
		attrs = append(attrs, attribute.StringSlice("naatre.signal.links", links))
	}
	return attrs
}

func metricAttributes(event runtime.MetricEvent) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("naatre.kind", string(event.Kind)),
		attribute.String("naatre.stage", string(event.Stage)),
		attribute.String("naatre.operation.kind", string(event.OperationKind)),
		attribute.String("naatre.outcome", string(event.Outcome)),
		attribute.String("naatre.error.code", event.ErrorCode),
	}
}

func reference(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256(append([]byte("naatre:otel-reference:v1\n"), value...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func isStart(event runtime.TelemetryEvent) bool {
	return event.Stage == runtime.TelemetryStarted ||
		event.Kind == runtime.TelemetryTransaction && event.Stage == runtime.TelemetryStage(runtime.MutationAttempted)
}

func isTerminal(event runtime.TelemetryEvent) bool {
	switch event.Stage {
	case runtime.TelemetryCompleted, runtime.TelemetryFailed, runtime.TelemetryCancelled:
		return true
	case runtime.TelemetryStarted:
		return false
	}
	if event.Kind != runtime.TelemetryTransaction {
		return false
	}
	switch runtime.MutationAuditStage(event.Stage) {
	case runtime.MutationCommitted, runtime.MutationRolledBack, runtime.MutationCompensated, runtime.MutationIndeterminate:
		return true
	case runtime.MutationAttempted, runtime.MutationDenied:
		return false
	}
	return false
}

func spanKind(kind runtime.TelemetryKind) oteltrace.SpanKind {
	if kind == runtime.TelemetryRequest {
		return oteltrace.SpanKindServer
	}
	return oteltrace.SpanKindInternal
}

func setStatus(span oteltrace.Span, event runtime.TelemetryEvent) {
	switch event.Outcome {
	case runtime.TelemetryActive:
		return
	case runtime.TelemetrySucceeded:
		span.SetStatus(codes.Ok, "")
	case runtime.TelemetryFailedOutcome, runtime.TelemetryCancelledOutcome, runtime.TelemetryDeniedOutcome,
		runtime.TelemetryHistoryUnavailable, runtime.TelemetryAuthorizationExpired,
		runtime.TelemetrySlowConsumer, runtime.TelemetryBrokerFailure, runtime.TelemetryForcedDrain:
		span.SetStatus(codes.Error, event.ErrorCode)
	}
}

func logSeverity(event runtime.TelemetryEvent) otellog.Severity {
	if event.Outcome == runtime.TelemetryFailedOutcome || event.Outcome == runtime.TelemetryCancelledOutcome || event.ErrorCode != "" {
		return otellog.SeverityError
	}
	return otellog.SeverityInfo
}

func logSeverityText(event runtime.TelemetryEvent) string {
	if logSeverity(event) == otellog.SeverityError {
		return "ERROR"
	}
	return "INFO"
}

func boundedUint64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func newMetricInstruments(meter otelmetric.Meter) (metricInstruments, error) {
	var result metricInstruments
	var err error
	if result.events, err = meter.Int64Counter("naatre.lifecycle.events"); err != nil {
		return metricInstruments{}, initializationError()
	}
	if result.duration, err = meter.Float64Histogram("naatre.lifecycle.duration", otelmetric.WithUnit("s")); err != nil {
		return metricInstruments{}, initializationError()
	}
	for name, target := range map[string]*otelmetric.Int64Histogram{
		"naatre.lifecycle.cost":               &result.cost,
		"naatre.lifecycle.batch_size":         &result.batchSize,
		"naatre.lifecycle.attempt":            &result.attempt,
		"naatre.lifecycle.connection_attempt": &result.connectionAttempt,
		"naatre.lifecycle.replay_attempt":     &result.replayAttempt,
		"naatre.lifecycle.replay_events":      &result.replayEvents,
		"naatre.lifecycle.replay_bytes":       &result.replayBytes,
		"naatre.lifecycle.scanned_candidates": &result.scannedCandidates,
		"naatre.lifecycle.drain_remaining":    &result.drainRemaining,
	} {
		if *target, err = meter.Int64Histogram(name); err != nil {
			return metricInstruments{}, initializationError()
		}
	}
	if result.activeStreams, err = meter.Int64Gauge("naatre.lifecycle.active_streams"); err != nil {
		return metricInstruments{}, initializationError()
	}
	return result, nil
}

func initializationError() error {
	return publicError(CodeInitialization, "OpenTelemetry instruments could not be initialized")
}

func recoverExportFailure(err *error) {
	if recover() != nil {
		*err = publicError(CodeExportFailure, "OpenTelemetry export failed")
	}
}

func publicError(code, message string) error {
	return &Error{Code: code, Message: message}
}

// CodeOf returns the stable public code for an adapter error.
func CodeOf(err error) string {
	var public *Error
	if errors.As(err, &public) {
		return public.Code
	}
	return ""
}
