package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// Invocation is the immutable runtime context supplied by the reference
// executor to handlers explicitly bound for document execution. Selected is
// the admitted selection and Input is its schema-coerced value; a call without
// arguments receives a missing InputValue.
type Invocation struct {
	Operation string
	Selection protocol.Source
	Selected  protocol.Selection
	Input     schema.InputValue
}

// BindInvocation binds a handler that consumes the reference executor's
// invocation context. Applications may continue to use Bind for their own
// typed direct adapters.
func BindInvocation[Output any](descriptor Descriptor, handler Handler[Invocation, Output]) Definition {
	return invocationDefinition(Bind(descriptor, handler))
}

func invocationDefinition(definition Definition) Definition {
	definition.adapter = true
	return definition
}

// ExecutionError is the safe public shape of one runtime failure.
type ExecutionError struct {
	Code       string
	Message    string
	Path       []any
	Source     protocol.Source
	Retryable  bool
	RetryAfter time.Duration
	Details    map[string]any
	internal   error
}

// Error renders only the public code and message, so logging or wrapping an
// execution error cannot disclose its internal cause.
func (e ExecutionError) Error() string { return e.Code + ": " + e.Message }

// Unwrap exposes the internal cause only to in-process server hooks. It is not
// serialized by the public response shape. ExecutionError implements error so
// a hook can reach the cause through errors.Unwrap, errors.Is, and errors.As.
func (e ExecutionError) Unwrap() error { return e.internal }

// EffectState reports what happened to the operation's effects, separately
// from whether the response could be assembled. A mutation can apply an effect
// and still fail to project its response, and a generic retryable error would
// hide that difference.
type EffectState string

const (
	// EffectNotApplicable is reported for an operation that declares no effect.
	EffectNotApplicable EffectState = "not-applicable"
	// EffectNone is reported when no effectful handler started.
	EffectNone EffectState = "none"
	// EffectApplied is reported when every effectful handler completed and the
	// response was assembled from them.
	EffectApplied EffectState = "applied"
	// EffectIndeterminate is reported when an effectful handler started and the
	// operation then failed. Without a transaction the runtime cannot say
	// whether the effect survived, and MUST NOT claim it was undone.
	EffectIndeterminate EffectState = "indeterminate"
	// EffectRolledBack is reported only when a transaction confirmed the undo.
	EffectRolledBack EffectState = "rolled-back"
	// EffectPartiallyApplied reports that at least one named group committed and
	// a later group did not commit.
	EffectPartiallyApplied EffectState = "partially-applied"
	// EffectCompensated reports a confirmed transaction rollback followed by
	// successful saga compensation for every registered external effect.
	EffectCompensated EffectState = "compensated"
)

// Outcome contains deterministic partial data and ordered public errors.
type Outcome struct {
	Data         map[string]any
	Errors       []ExecutionError
	Annotations  []DirectiveAnnotation
	Capabilities []string                       `json:"Capabilities,omitempty"`
	Extensions   []protocol.NegotiatedExtension `json:"Extensions,omitempty"`
	// Effects describes the operation's effect state. It is safe outcome
	// metadata, not an error, and never implies permission to replay a write.
	Effects EffectState
	// Reliability reports safe attempt and deduplication metadata without
	// exposing caller-supplied idempotency keys.
	Reliability ReliabilityInfo
}

// ErrIncomplete reports that an outcome carries unresolved data, so it must not
// be projected onto a model whose required fields are non-null.
var ErrIncomplete = errors.New("naatre: outcome is incomplete")

// RequireComplete returns the data only when nothing was left unresolved. An
// execution error always means some selection has no value, so a caller cannot
// silently cast partial data into a successful domain model; a legitimate null
// carries no error and stays complete.
func (o Outcome) RequireComplete() (map[string]any, error) {
	if len(o.Errors) == 0 {
		return o.Data, nil
	}
	paths := make([]string, 0, len(o.Errors))
	for _, failure := range o.Errors {
		paths = append(paths, fmt.Sprintf("%s at %s", failure.Code, formatResponsePath(failure.Path)))
	}
	return nil, fmt.Errorf("%w: %s", ErrIncomplete, strings.Join(paths, "; "))
}

// formatResponsePath renders a response path for diagnostics. The root path is
// empty, which is how an operation-level failure is identified.
func formatResponsePath(path []any) string {
	if len(path) == 0 {
		return "<root>"
	}
	var rendered strings.Builder
	for index, segment := range path {
		if index != 0 {
			rendered.WriteByte('.')
		}
		fmt.Fprintf(&rendered, "%v", segment)
	}
	return rendered.String()
}

type plannedSelection struct {
	definition Definition
	outputName string
	source     protocol.Source
}

// Plan is immutable and safe for concurrent reads and execution.
type Plan struct {
	operationName   string
	kind            protocol.OperationKind
	atomicity       protocol.AtomicityMode
	operationSource protocol.Source
	requirements    []string
	capabilities    []string
	extensions      []protocol.NegotiatedExtension
	variables       []protocol.VariableDefinition
	selections      []plannedSelection
	nodes           []planNode
	types           schema.Snapshot
	variableValues  map[string]json.RawMessage
	authorization   AuthorizationConfig
	interceptors    []registeredInterceptor
	transactions    TransactionConfig
	resourceLimits  ResourceLimits
	staticCost      uint64
	document        json.RawMessage
	transformations []PlanTransformation
}

// StaticCost is the complete request-independent planned cost admitted before
// authorization or execution.
func (p *Plan) StaticCost() uint64 { return p.staticCost }

// Prepare resolves and validates the complete selected operation without
// invoking application handlers.
func Prepare(registry Snapshot, request *protocol.Request) (*Plan, error) {
	return PrepareWithOptions(registry, request, PrepareOptions{})
}

// PrepareWithOptions resolves and validates the complete selected operation
// under explicit request-wide resource limits without invoking handlers.
func PrepareWithOptions(registry Snapshot, request *protocol.Request, options PrepareOptions) (plan *Plan, err error) {
	planning := startPlanningTelemetry(options.Telemetry)
	defer func() { planning.finish(err) }()
	if request == nil || request.Document() == nil {
		return nil, errors.New("runtime preparation requires an inline document")
	}
	limits, err := resolveResourceLimits(options.Limits)
	if err != nil {
		return nil, fmt.Errorf("invalid resource limits: %w", err)
	}
	operations := request.Document().Operations()
	operation, err := selectOperation(operations, request.OperationName(), request.Source())
	if err != nil {
		return nil, err
	}
	planning.setOperation(operation.Name(), operation.Kind())
	planner := newPlanValidator(registry, request, operation, limits)
	nodes, executable := planner.validate()
	if len(planner.issues) == 0 {
		planner.validatePlanResources(nodes)
	}
	if len(planner.issues) == 0 {
		planner.planDirectives(nodes)
	}
	if len(planner.issues) == 0 {
		planner.issues = append(planner.issues, authorizePlannedNodes(registry.authorization, operation.Kind(), nodes)...)
	}
	if len(planner.issues) != 0 {
		return nil, &ValidationErrors{issues: planner.issues}
	}
	planning.setCost(planner.staticCost)
	capabilities := request.Capabilities()
	slices.Sort(capabilities)
	return &Plan{
		operationName: operation.Name(), kind: operation.Kind(), atomicity: operation.Atomicity(), operationSource: operation.Source(), requirements: request.Document().Requires(),
		capabilities: capabilities, extensions: request.NegotiatedExtensions(),
		variables: operation.Variables(), selections: executable, nodes: nodes, types: registry.types,
		variableValues: captureVariableValues(request, operation.Variables()),
		authorization:  registry.authorization, interceptors: slices.Clone(registry.interceptors),
		transactions:   registry.transactions,
		resourceLimits: limits, staticCost: planner.staticCost,
		document: append(json.RawMessage(nil), request.Document().CanonicalJSON()...),
	}, nil
}

// DefaultAbandonGrace is how long Execute waits, after cancellation, for a
// handler to observe it and return before the handler is abandoned.
const DefaultAbandonGrace = 5 * time.Second

// ExecuteOptions tunes one execution. The zero value is the default policy.
type ExecuteOptions struct {
	// AbandonGrace bounds the wait for a handler to return after the request
	// is cancelled. Go cannot stop an arbitrary goroutine, so a handler that
	// ignores cancellation is detached rather than waited for: it keeps its
	// accounting slot until it exits, and the response reports the selection
	// as cancelled. Zero selects DefaultAbandonGrace; a negative value
	// abandons an uncooperative handler as soon as cancellation is observed.
	AbandonGrace time.Duration
	// Retry opts into bounded automatic retries. The selected handlers must
	// independently declare retry safety.
	Retry RetryPolicy
	// Idempotency protects a request or named mutation groups with caller keys.
	Idempotency IdempotencyOptions
	// Cache controls request-local memoization and optional application-owned
	// cross-request hooks for handlers explicitly marked cacheable.
	Cache CacheOptions
	// Batch configures tracing-safe request batch telemetry.
	Batch BatchRuntimeOptions
	// Telemetry installs dependency-free trace, metric, and structured-log
	// hooks plus safe correlation metadata.
	Telemetry TelemetryOptions
	telemetry *executionTelemetry
}

func (o ExecuteOptions) abandonGrace() time.Duration {
	if o.AbandonGrace == 0 {
		return DefaultAbandonGrace
	}
	return o.AbandonGrace
}

// Execute runs a prepared operation in selection order under the default
// policy. Query failures preserve independent sibling data; mutation failures
// stop later mutation scheduling.
func (p *Plan) Execute(ctx context.Context) Outcome {
	return p.attachNegotiation(p.executeComposed(ctx, ExecuteOptions{}))
}

// ExecuteWith runs a prepared operation under an explicit policy.
func (p *Plan) ExecuteWith(ctx context.Context, options ExecuteOptions) Outcome {
	telemetry := newExecutionTelemetry(options.Telemetry, p.operationName, p.kind, p.staticCost)
	options.telemetry = telemetry
	ctx = context.WithValue(ctx, telemetryContextKey{}, executionTelemetryContext{options: options.Telemetry, execution: telemetry})
	started := time.Now()
	if telemetry != nil {
		request := telemetry.baseEvent(TelemetryRequest, TelemetryStarted)
		request.ID = telemetry.requestSpan
		emitTelemetry(telemetry.options, request)
		operation := telemetry.baseEvent(TelemetryOperation, TelemetryStarted)
		operation.ID, operation.ParentID = telemetry.operationSpan, telemetry.requestSpan
		emitTelemetry(telemetry.options, operation)
	}
	outcome := p.attachNegotiation(p.executeReliably(ctx, options))
	if telemetry != nil {
		stage, result, code := outcomeTelemetry(outcome)
		operation := telemetry.baseEvent(TelemetryOperation, stage)
		operation.ID, operation.ParentID = telemetry.operationSpan, telemetry.requestSpan
		operation.Duration, operation.Outcome, operation.ErrorCode = time.Since(started), result, code
		emitTelemetry(telemetry.options, operation)
		request := telemetry.baseEvent(TelemetryRequest, stage)
		request.ID, request.Duration, request.Outcome, request.ErrorCode = telemetry.requestSpan, time.Since(started), result, code
		emitTelemetry(telemetry.options, request)
	}
	return outcome
}

func (p *Plan) attachNegotiation(outcome Outcome) Outcome {
	outcome.Capabilities = slices.Clone(p.capabilities)
	outcome.Extensions = slices.Clone(p.extensions)
	return outcome
}

func captureVariableValues(request *protocol.Request, definitions []protocol.VariableDefinition) map[string]json.RawMessage {
	values := make(map[string]json.RawMessage, len(definitions))
	for _, definition := range definitions {
		value, ok := request.Variable(definition.Name())
		if !ok {
			value, ok = definition.Default()
		}
		if ok {
			values[definition.Name()] = append(json.RawMessage(nil), value...)
		}
	}
	return values
}

func (p *Plan) executeFlat(ctx context.Context) Outcome {
	outcome := Outcome{Data: make(map[string]any)}
	for _, selection := range p.selections {
		if err := ctx.Err(); err != nil {
			outcome.Errors = append(outcome.Errors, executionFailure("CANCELLED", "request cancelled", selection, err))
			break
		}
		value, err := invokeContained(ctx, selection.definition, Invocation{Operation: p.operationName, Selection: selection.source})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				outcome.Errors = append(outcome.Errors, executionFailure("CANCELLED", "request cancelled", selection, err))
				break
			}
			code, message := "HANDLER_FAILED", "field unavailable"
			var panicFailure *handlerPanic
			if errors.As(err, &panicFailure) {
				code, message = "INTERNAL", "internal execution error"
			}
			outcome.Errors = append(outcome.Errors, executionFailure(code, message, selection, err))
			if p.kind == protocol.Mutation {
				break
			}
			continue
		}
		completed, issues, available, completeErr := completeContained(value, selection.definition.descriptor.Output, selection.definition.descriptor.OutputNullable, p.types)
		if completeErr != nil {
			outcome.Errors = append(outcome.Errors, executionFailure("OUTPUT_COMPLETION", "field output is invalid", selection, completeErr))
			if p.kind == protocol.Mutation {
				break
			}
			continue
		}
		for _, completionIssue := range issues {
			failure := executionFailure("OUTPUT_COMPLETION", "field output is invalid", selection, completionIssue.cause)
			failure.Path = append(failure.Path, completionIssue.path...)
			outcome.Errors = append(outcome.Errors, failure)
		}
		if available {
			outcome.Data[selection.outputName] = completed
		}
		if len(issues) != 0 && p.kind == protocol.Mutation {
			break
		}
	}
	sort.SliceStable(outcome.Errors, func(left, right int) bool {
		compared := comparePaths(outcome.Errors[left].Path, outcome.Errors[right].Path)
		if compared != 0 {
			return compared < 0
		}
		return outcome.Errors[left].Source.Start < outcome.Errors[right].Source.Start
	})
	return outcome
}

func selectOperation(operations []protocol.Operation, requested string, source protocol.Source) (protocol.Operation, error) {
	if requested == "" {
		if len(operations) != 1 {
			return protocol.Operation{}, &protocol.Diagnostic{Code: "OPERATION_REQUIRED", Clause: "PROTO-005", Phase: "validate", Message: fmt.Sprintf("operation name required for %d operations", len(operations)), Pointer: source.Pointer, Offset: source.Start, Line: source.Line, Column: source.Column}
		}
		return operations[0], nil
	}
	for _, operation := range operations {
		if operation.Name() == requested {
			return operation, nil
		}
	}
	return protocol.Operation{}, &protocol.Diagnostic{Code: "UNKNOWN_OPERATION", Clause: "PROTO-005", Phase: "validate", Message: fmt.Sprintf("operation %q not found", requested), Pointer: source.Pointer, Offset: source.Start, Line: source.Line, Column: source.Column}
}

func invokeContained(ctx context.Context, definition Definition, invocation Invocation) (output any, err error) {
	return definition.call(ctx, nil, invocation)
}

func executionFailure(code, message string, selection plannedSelection, cause error) ExecutionError {
	return makeExecutionError(code, message, []any{selection.outputName}, selection.source, cause)
}

func makeExecutionError(code, message string, path []any, source protocol.Source, cause error) ExecutionError {
	return ExecutionError{Code: code, Message: message, Path: append([]any(nil), path...), Source: source, Retryable: false, internal: cause}
}

func comparePaths(left, right []any) int {
	for index := 0; index < min(len(left), len(right)); index++ {
		leftIndex, leftIsIndex := left[index].(int)
		rightIndex, rightIsIndex := right[index].(int)
		if leftIsIndex != rightIsIndex {
			if leftIsIndex {
				return -1
			}
			return 1
		}
		if leftIsIndex && rightIsIndex {
			if leftIndex < rightIndex {
				return -1
			}
			if leftIndex > rightIndex {
				return 1
			}
			continue
		}
		leftText, rightText := fmt.Sprint(left[index]), fmt.Sprint(right[index])
		if leftText < rightText {
			return -1
		}
		if leftText > rightText {
			return 1
		}
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return 0
}
