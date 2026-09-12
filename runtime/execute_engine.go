package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

const maxParallelExecutions = 8

type executionStatus uint8

const (
	valueAvailable executionStatus = iota
	valueMissing
	valueNull
	valueUnavailable
	valueSkipped
)

type executionValue struct {
	value       any
	typeInfo    staticType
	actualType  schema.TypeID
	status      executionStatus
	unavailable *unavailableTree
}

// unavailableTree marks the members of a completed value whose own output
// completion failed. Output completion omits such a member, so selecting it
// must report unavailable partial data rather than invoke its handler against
// completed data that no longer carries it. Nested members are held as
// children so the mark survives the descent into a nested object, while an
// ancestor stays available with its remaining valid data (TYPE-206).
type unavailableTree struct {
	self     bool
	children map[string]*unavailableTree
}

// member reports the subtree recorded for name and whether name itself is
// unavailable.
func (t *unavailableTree) member(name string) (*unavailableTree, bool) {
	if t == nil {
		return nil, false
	}
	child := t.children[name]
	if child == nil {
		return nil, false
	}
	return child, child.self
}

func (t *unavailableTree) insert(path []any) {
	node := t
	for _, segment := range path {
		name, ok := segment.(string)
		if !ok {
			// A list index is represented by the TYPE-200 null placeholder for
			// that element, never by marking an ancestor unavailable.
			return
		}
		if node.children == nil {
			node.children = make(map[string]*unavailableTree)
		}
		child := node.children[name]
		if child == nil {
			child = &unavailableTree{}
			node.children[name] = child
		}
		node = child
	}
	node.self = true
}

// merge copies the marks of other into t, leaving other untouched so a parent
// value can share its subtrees with several children.
func (t *unavailableTree) merge(other *unavailableTree) {
	if other == nil {
		return
	}
	t.self = t.self || other.self
	for name, child := range other.children {
		if t.children == nil {
			t.children = make(map[string]*unavailableTree)
		}
		existing := t.children[name]
		if existing == nil {
			existing = &unavailableTree{}
			t.children[name] = existing
		}
		existing.merge(child)
	}
}

type executionScope struct {
	current  executionValue
	parent   executionValue
	bindings map[string]executionValue
	limiter  chan struct{}
	parallel bool
	grace    time.Duration
}

type nodeResult struct {
	value  executionValue
	data   any
	emit   bool
	merge  bool
	failed bool
	errors []ExecutionError
}

type sequenceResult struct {
	data   map[string]any
	failed bool
	errors []ExecutionError
}

func (p *Plan) executeComposed(ctx context.Context, options ExecuteOptions) Outcome {
	scope := executionScope{
		bindings: make(map[string]executionValue),
		limiter:  make(chan struct{}, maxParallelExecutions),
		grace:    options.abandonGrace(),
	}
	result := p.executeSequence(ctx, p.nodes, scope, nil)
	sortExecutionErrors(result.errors)
	return Outcome{Data: result.data, Errors: result.errors}
}

func (p *Plan) executeSequence(ctx context.Context, nodes []planNode, scope executionScope, path []any) sequenceResult {
	result := sequenceResult{data: make(map[string]any)}
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			result.errors = append(result.errors, nodeExecutionError("CANCELLED", "request cancelled", node, pathForNode(path, node), err))
			result.failed = true
			break
		}
		current := p.executeNode(ctx, node, scope, path)
		result.errors = append(result.errors, current.errors...)
		result.failed = result.failed || current.failed
		if node.binding != "" {
			bound := current.value
			if current.failed && bound.status == valueAvailable {
				bound.status = valueUnavailable
			}
			scope.bindings[node.binding] = bound
		}
		if current.emit {
			result.data[node.outputName] = current.data
		}
		if current.merge {
			members, ok := current.data.(map[string]any)
			if !ok {
				result.errors = append(result.errors, nodeExecutionError("OUTPUT_COMPLETION", "field output is invalid", node, path, errors.New("unnested result is not an object")))
				result.failed = true
			} else {
				for name, value := range members {
					result.data[name] = value
				}
			}
		}
		if containsExecutionCode(current.errors, "CANCELLED") {
			break
		}
		if current.failed && p.kind == protocol.Mutation {
			break
		}
	}
	return result
}

func containsExecutionCode(failures []ExecutionError, code string) bool {
	for _, failure := range failures {
		if failure.Code == code {
			return true
		}
	}
	return false
}

func (p *Plan) executeNode(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	nodePath := pathForNode(path, node)
	if !p.matchesTypeConditions(node, scope.current) {
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueSkipped}}
	}
	run, directiveErrors := p.evaluateDirectives(node, scope, nodePath)
	if len(directiveErrors) != 0 {
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: directiveErrors}
	}
	if !run {
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueSkipped}}
	}

	switch node.kind {
	case protocol.CallSelection, protocol.FieldSelection:
		return p.executeHandlerNode(ctx, node, scope, nodePath)
	case protocol.PipelineSelection:
		return p.executePipeline(ctx, node, scope, nodePath)
	case protocol.MapSelection:
		return p.executeMap(ctx, node, scope, nodePath)
	case protocol.IndexSelection:
		return p.executeIndex(ctx, node, scope, nodePath)
	case protocol.SliceSelection, protocol.PageSelection:
		return p.executeCollectionWindow(ctx, node, scope, nodePath)
	case protocol.MetaSelection:
		return p.executeMetadata(node, scope, nodePath)
	case protocol.ParallelSelection:
		return p.executeParallel(ctx, node, scope, path)
	case protocol.FragmentSelection:
		children := p.executeSequence(ctx, node.children, scope, path)
		return nodeResult{data: children.data, merge: true, failed: children.failed, errors: children.errors}
	case protocol.CurrentSelection:
		return resultForValue(node, scope.current, scope.current.value)
	case protocol.NestSelection:
		children := p.executeSequence(ctx, node.children, cloneExecutionScope(scope), nodePath)
		value := executionValue{value: children.data, typeInfo: node.output, status: valueAvailable}
		return nodeResult{value: value, data: children.data, emit: true, failed: children.failed, errors: children.errors}
	case protocol.UnnestSelection:
		children := p.executeSequence(ctx, node.children, cloneExecutionScope(scope), path)
		value := executionValue{value: children.data, typeInfo: node.output, status: valueAvailable}
		return nodeResult{value: value, data: children.data, merge: true, failed: children.failed, errors: children.errors}
	default:
		err := nodeExecutionError("INTERNAL", "internal execution error", node, nodePath, errors.New("unknown prepared selection kind"))
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{err}}
	}
}

func (p *Plan) executeHandlerNode(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	// A member whose completion failed stays unavailable however deep the
	// selection reaches it; its surviving nested marks descend with it.
	var inherited *unavailableTree
	if node.kind == protocol.FieldSelection {
		nested, unavailable := scope.current.unavailable.member(node.name)
		if unavailable {
			return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}}
		}
		inherited = nested
	}

	input, status, inputErr := p.handlerInput(node, scope)
	if inputErr != nil {
		code := executionStatusCode(status)
		err := nodeExecutionError(code, "handler input is unavailable", node, path, inputErr)
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{err}}
	}

	var source any
	if node.kind == protocol.FieldSelection || node.definition.descriptor.Scope == ObjectScope {
		if scope.current.status != valueAvailable && scope.current.status != valueNull {
			err := unavailableContinuationError(node, path, scope.current.status)
			return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{err}}
		}
		if scope.current.status == valueNull {
			err := nodeExecutionError("RESULT_NULL", "current value is null", node, path, errors.New("non-null member source is null"))
			return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{err}}
		}
		source = sourceValue(scope.current)
		isolated, copyErr := copyOutput(reflect.ValueOf(source), 0, &copyState{active: make(map[copyReference]bool)})
		if copyErr != nil {
			failure := nodeExecutionError("INTERNAL", "internal execution error", node, path, fmt.Errorf("copy handler source: %w", copyErr))
			return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
		}
		source = isolated
	}

	release, err := acquireExecutionSlot(ctx, scope)
	if err != nil {
		failure := nodeExecutionError("CANCELLED", "request cancelled", node, path, err)
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
	}
	output, err := callWithinGrace(ctx, node, source, input, release, scope.grace)
	if err != nil {
		code, message := "HANDLER_FAILED", "field unavailable"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			code, message = "CANCELLED", "request cancelled"
		} else {
			var panicFailure *handlerPanic
			if errors.As(err, &panicFailure) {
				code, message = "INTERNAL", "internal execution error"
			}
		}
		failure := nodeExecutionError(code, message, node, path, err)
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
	}

	completed, issues, available, completionErr := completeSafely(output, node.definition.descriptor.Output, node.definition.descriptor.OutputNullable, p.types)
	result := executionValue{value: completed, typeInfo: node.output, status: valueAvailable, actualType: runtimeActualType(node.output.id, completed)}
	if completed == nil && available {
		result.status = valueNull
	}
	var failures []ExecutionError
	if completionErr != nil {
		failures = append(failures, nodeExecutionError("OUTPUT_COMPLETION", "field output is invalid", node, path, completionErr))
		available = false
	}
	for _, issue := range issues {
		failures = append(failures, nodeExecutionError("OUTPUT_COMPLETION", "field output is invalid", node, appendPathSegments(path, issue.path...), issue.cause))
	}
	if !available {
		result.status = valueUnavailable
	} else {
		result.unavailable = unavailableMembers(issues, inherited)
	}

	presentation := completed
	if available && len(node.children) != 0 {
		childScope := childExecutionScope(scope, result, scope.current)
		children := p.executeSequence(ctx, node.children, childScope, path)
		failures = append(failures, children.errors...)
		presentation = children.data
	}
	return nodeResult{value: result, data: presentation, emit: node.outputName != "" && available, failed: len(failures) != 0, errors: failures}
}

// unavailableMembers records every member this completion could not produce,
// keeping the marks this value inherited from the completion that produced its
// source. Each issue marks only the member it names, so an ancestor keeps its
// remaining valid data instead of being deleted wholesale (TYPE-206).
func unavailableMembers(issues []completionIssue, inherited *unavailableTree) *unavailableTree {
	var unavailable *unavailableTree
	for _, issue := range issues {
		path := issue.path
		if len(path) != 0 && path[0] == "$value" {
			path = path[1:]
		}
		if len(path) == 0 {
			continue
		}
		if unavailable == nil {
			unavailable = &unavailableTree{}
		}
		unavailable.insert(path)
	}
	if inherited == nil {
		return unavailable
	}
	if unavailable == nil {
		unavailable = &unavailableTree{}
	}
	unavailable.merge(inherited)
	return unavailable
}

func (p *Plan) executePipeline(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	pipelineScope := cloneExecutionScope(scope)
	current := executionValue{typeInfo: node.input, status: valueMissing}
	if node.input.valid {
		current = scope.current
	}
	var presentation any
	var failures []ExecutionError
	failed := false
	for _, stage := range node.children {
		pipelineScope.parent = pipelineScope.current
		pipelineScope.current = current
		stageResult := p.executeNode(ctx, stage, pipelineScope, path)
		failures = append(failures, stageResult.errors...)
		if stage.binding != "" {
			bound := stageResult.value
			if stageResult.failed && bound.status == valueAvailable {
				bound.status = valueUnavailable
			}
			pipelineScope.bindings[stage.binding] = bound
		}
		if stageResult.failed {
			failed = true
			current = executionValue{typeInfo: stage.output, status: valueUnavailable}
			break
		}
		current = stageResult.value
		presentation = stageResult.data
	}
	return nodeResult{value: current, data: presentation, emit: node.outputName != "" && !failed && current.status != valueUnavailable && current.status != valueSkipped, failed: failed, errors: failures}
}

func (p *Plan) executeParallel(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	results := make([]nodeResult, len(node.children))
	workers := min(maxParallelExecutions, len(node.children))
	if workers == 0 {
		return nodeResult{data: map[string]any{}, merge: true}
	}
	var mutex sync.Mutex
	next := 0
	stopped := false
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for {
				mutex.Lock()
				if next >= len(node.children) || stopped || ctx.Err() != nil {
					mutex.Unlock()
					return
				}
				index := next
				next++
				mutex.Unlock()
				branchScope := cloneExecutionScope(scope)
				branchScope.parallel = true
				result := p.executeNode(ctx, node.children[index], branchScope, path)
				results[index] = result
				if containsExecutionCode(result.errors, "CANCELLED") || (result.failed && node.parallelPolicy == protocol.FailFastParallel) {
					mutex.Lock()
					stopped = true
					mutex.Unlock()
				}
			}
		}()
	}
	wait.Wait()
	data := make(map[string]any)
	result := nodeResult{data: data, merge: true}
	for index, branch := range results {
		if index >= next {
			continue
		}
		result.errors = append(result.errors, branch.errors...)
		result.failed = result.failed || branch.failed
		if branch.emit {
			data[node.children[index].outputName] = branch.data
		}
		if branch.merge {
			for name, value := range branch.data.(map[string]any) {
				data[name] = value
			}
		}
	}
	return result
}

func (p *Plan) matchesTypeConditions(node planNode, current executionValue) bool {
	if len(node.typeConditions) == 0 {
		return true
	}
	if current.actualType == "" {
		return false
	}
	for _, condition := range node.typeConditions {
		if condition == current.actualType {
			continue
		}
		_, known, err := p.types.ResolveVariant(condition, current.actualType)
		if err != nil || !known {
			return false
		}
	}
	return true
}

func collectionItems(value executionValue) ([]any, bool) {
	if value.status != valueAvailable {
		return nil, false
	}
	items, ok := value.value.([]any)
	return items, ok
}

func collectionItemValue(item any, descriptor schema.TypeDescriptor, _ int) executionValue {
	status := valueAvailable
	if item == nil {
		status = valueNull
	}
	return executionValue{value: item, typeInfo: staticType{id: descriptor.Element, nullable: descriptor.ElementNullable, valid: true}, status: status, actualType: runtimeActualType(descriptor.Element, item)}
}

func runtimeActualType(typeID schema.TypeID, value any) schema.TypeID {
	if object, ok := value.(map[string]any); ok {
		if variant, ok := object["$type"].(string); ok {
			return schema.TypeID(variant)
		}
	}
	return typeID
}

func sourceValue(value executionValue) any {
	if object, ok := value.value.(map[string]any); ok {
		if variantValue, ok := object["$value"]; ok {
			if _, tagged := object["$type"]; tagged {
				return variantValue
			}
		}
	}
	return value.value
}

func resultForValue(node planNode, value executionValue, presentation any) nodeResult {
	return nodeResult{value: value, data: presentation, emit: node.outputName != "" && (value.status == valueAvailable || value.status == valueNull)}
}

func invalidCollectionResult(node planNode, path []any) nodeResult {
	failure := nodeExecutionError("INVALID_COLLECTION", "collection value is unavailable", node, path, errors.New("runtime value is not a collection"))
	return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
}

func unavailableContinuationError(node planNode, path []any, status executionStatus) ExecutionError {
	code := executionStatusCode(status)
	return nodeExecutionError(code, "current value is unavailable", node, path, errors.New("runtime continuation is unavailable"))
}

func executionStatusCode(status executionStatus) string {
	switch status {
	case valueMissing:
		return "RESULT_MISSING"
	case valueNull:
		return "RESULT_NULL"
	case valueSkipped:
		return "RESULT_SKIPPED"
	case valueAvailable, valueUnavailable:
		return "RESULT_UNAVAILABLE"
	}
	return "RESULT_UNAVAILABLE"
}

func completeSafely(value any, output schema.TypeID, nullable bool, types schema.Snapshot) (completed any, issues []completionIssue, available bool, err error) {
	return completeRecovering(func() (any, []completionIssue, bool, error) {
		return completeContained(value, output, nullable, types)
	})
}

func completeRecovering(complete func() (any, []completionIssue, bool, error)) (completed any, issues []completionIssue, available bool, err error) {
	defer func() {
		if recover() != nil {
			completed = nil
			issues = nil
			available = false
			err = errors.New("completion hook panic")
		}
	}()
	return complete()
}

func cloneExecutionScope(scope executionScope) executionScope {
	return executionScope{
		current: scope.current, parent: scope.parent, bindings: cloneBindings(scope.bindings),
		limiter: scope.limiter, parallel: scope.parallel, grace: scope.grace,
	}
}

func childExecutionScope(scope executionScope, current, parent executionValue) executionScope {
	child := cloneExecutionScope(scope)
	child.current, child.parent = current, parent
	return child
}

func acquireExecutionSlot(ctx context.Context, scope executionScope) (func(), error) {
	if !scope.parallel {
		return func() {}, nil
	}
	select {
	case scope.limiter <- struct{}{}:
		return func() { <-scope.limiter }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// handlerOutcome carries one handler return across the abandonment boundary.
type handlerOutcome struct {
	value any
	err   error
}

// callWithinGrace runs a handler and bounds how long the response waits for it.
// Cancellation signals a handler to stop; it does not stop it, and Go cannot
// stop an arbitrary goroutine. A handler that has not returned once grace
// elapses after cancellation is abandoned: the response reports the selection
// as cancelled while the handler keeps its accounting slot until it exits, so
// an uncooperative handler delays neither the response nor its siblings.
//
// release is transferred to whichever path owns the handler, so the slot is
// always freed exactly once, by the handler itself when it is abandoned.
func callWithinGrace(ctx context.Context, node planNode, source, input any, release func(), grace time.Duration) (any, error) {
	// An uncancellable context can never abandon, so it needs no extra
	// goroutine and keeps the common sequential path direct.
	if ctx.Done() == nil {
		defer release()
		return node.definition.call(ctx, source, input)
	}
	// Buffered, so an abandoned handler publishes its result and exits rather
	// than blocking forever on a receiver that has already moved on.
	returned := make(chan handlerOutcome, 1)
	go func() {
		defer release()
		value, err := node.definition.call(ctx, source, input)
		returned <- handlerOutcome{value: value, err: err}
	}()
	select {
	case outcome := <-returned:
		return outcome.value, outcome.err
	case <-ctx.Done():
	}
	if grace < 0 {
		return nil, fmt.Errorf("%w: handler did not return when cancelled", context.Cause(ctx))
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case outcome := <-returned:
		return outcome.value, outcome.err
	case <-timer.C:
		return nil, fmt.Errorf("%w: handler did not return within %s of cancellation", context.Cause(ctx), grace)
	}
}

func cloneBindings(bindings map[string]executionValue) map[string]executionValue {
	return maps.Clone(bindings)
}

func pathForNode(base []any, node planNode) []any {
	if node.outputName == "" {
		return append([]any(nil), base...)
	}
	return appendPathSegments(base, node.outputName)
}

func appendPathSegments(path []any, segments ...any) []any {
	result := make([]any, 0, len(path)+len(segments))
	result = append(result, path...)
	return append(result, segments...)
}

func nodeExecutionError(code, message string, node planNode, path []any, cause error) ExecutionError {
	return makeExecutionError(code, message, path, node.source, cause)
}

func sortExecutionErrors(failures []ExecutionError) {
	sort.SliceStable(failures, func(left, right int) bool {
		compared := comparePaths(failures[left].Path, failures[right].Path)
		if compared != 0 {
			return compared < 0
		}
		return failures[left].Source.Start < failures[right].Source.Start
	})
}

func statusFromRaw(raw json.RawMessage) executionStatus {
	if raw == nil {
		return valueMissing
	}
	if string(raw) == "null" {
		return valueNull
	}
	return valueAvailable
}
