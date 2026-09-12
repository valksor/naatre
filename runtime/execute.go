package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// Invocation is the immutable runtime context supplied by the reference
// executor to handlers explicitly bound for document execution.
type Invocation struct {
	Operation string
	Selection protocol.Source
}

// BindInvocation binds a handler that consumes the reference executor's
// invocation context. Applications may continue to use Bind for their own
// typed direct adapters.
func BindInvocation[Output any](descriptor Descriptor, handler Handler[Invocation, Output]) Definition {
	definition := Bind(descriptor, handler)
	definition.adapter = true
	return definition
}

// ExecutionError is the safe public shape of one runtime failure.
type ExecutionError struct {
	Code      string
	Message   string
	Path      []any
	Source    protocol.Source
	Retryable bool
	Details   map[string]any
	internal  error
}

// Unwrap exposes the internal cause only to in-process server hooks. It is not
// serialized by the public response shape.
func (e ExecutionError) Unwrap() error { return e.internal }

// Outcome contains deterministic partial data and ordered public errors.
type Outcome struct {
	Data   map[string]any
	Errors []ExecutionError
}

type plannedSelection struct {
	definition Definition
	outputName string
	source     protocol.Source
}

// Plan is immutable and safe for concurrent reads and execution.
type Plan struct {
	operationName  string
	kind           protocol.OperationKind
	requirements   []string
	variables      []protocol.VariableDefinition
	selections     []plannedSelection
	nodes          []planNode
	types          schema.Snapshot
	variableValues map[string]json.RawMessage
}

// Prepare resolves and validates the complete selected operation without
// invoking application handlers.
func Prepare(registry Snapshot, request *protocol.Request) (*Plan, error) {
	if request == nil || request.Document() == nil {
		return nil, errors.New("runtime preparation requires an inline document")
	}
	operations := request.Document().Operations()
	operation, err := selectOperation(operations, request.OperationName(), request.Source())
	if err != nil {
		return nil, err
	}
	planner := newPlanValidator(registry, request, operation)
	nodes, executable := planner.validate()
	if len(planner.issues) != 0 {
		return nil, &ValidationErrors{issues: planner.issues}
	}
	return &Plan{
		operationName: operation.Name(), kind: operation.Kind(), requirements: request.Document().Requires(),
		variables: operation.Variables(), selections: executable, nodes: nodes, types: registry.types,
		variableValues: captureVariableValues(request, operation.Variables()),
	}, nil
}

// Execute runs a prepared operation in selection order. Query failures preserve
// independent sibling data; mutation failures stop later mutation scheduling.
func (p *Plan) Execute(ctx context.Context) Outcome {
	return p.executeComposed(ctx)
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
