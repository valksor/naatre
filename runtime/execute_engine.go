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
	"sync/atomic"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

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
	self bool
	// children is keyed by a response path segment: an object member name or a
	// list index, so a failed member is tracked at either position.
	children map[any]*unavailableTree
}

// member reports the subtree recorded for a path segment and whether that
// segment itself is unavailable. A segment is a member name or a list index.
func (t *unavailableTree) member(segment any) (*unavailableTree, bool) {
	if t == nil {
		return nil, false
	}
	child := t.children[segment]
	if child == nil {
		return nil, false
	}
	return child, child.self
}

func (t *unavailableTree) insert(path []any) {
	node := t
	for _, segment := range path {
		switch segment.(type) {
		case string, int:
		default:
			// An unrepresentable segment cannot be addressed by a selection, so
			// marking an ancestor for it would delete unrelated valid data.
			return
		}
		if node.children == nil {
			node.children = make(map[any]*unavailableTree)
		}
		child := node.children[segment]
		if child == nil {
			child = &unavailableTree{}
			node.children[segment] = child
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
			t.children = make(map[any]*unavailableTree)
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
	current       executionValue
	parent        executionValue
	bindings      map[string]executionValue
	variables     map[string]scopedVariable
	limiter       chan struct{}
	parallel      bool
	grace         time.Duration
	effects       *effectRecorder
	annotations   *directiveAnnotationRecorder
	resources     *resourceMeter
	cursorScopes  map[uint64]CursorScope
	transaction   Transaction
	idempotency   *executionIdempotency
	reliability   *executionReliabilityState
	batches       *batchRuntime
	cache         *executionCache
	batchObserver func(BatchEvent)
}

type scopedVariable struct {
	value   json.RawMessage
	present bool
}

// effectRecorder tracks whether any write handler started and whether one
// completed, so the outcome can separate "no effect" from "effect applied" and
// from "an effect started and the operation then failed". Parallel branches
// share one recorder, so it is atomic.
type effectRecorder struct {
	started           atomic.Bool
	completed         atomic.Bool
	transactionMu     sync.Mutex
	transactionStates []EffectState
}

// state folds the recorded effects and the operation result into the reported
// effect state. Without a transaction the runtime cannot observe an undo, so it
// reports indeterminate rather than claiming a rollback.
func (r *effectRecorder) state(kind protocol.OperationKind, failed bool) EffectState {
	if kind != protocol.Mutation {
		return EffectNotApplicable
	}
	if state, ok := r.transactionState(); ok {
		return state
	}
	switch {
	case !r.started.Load():
		return EffectNone
	case failed:
		return EffectIndeterminate
	case r.completed.Load():
		return EffectApplied
	default:
		return EffectIndeterminate
	}
}

func (r *effectRecorder) recordTransactionState(state EffectState) {
	r.transactionMu.Lock()
	defer r.transactionMu.Unlock()
	r.transactionStates = append(r.transactionStates, state)
}

func (r *effectRecorder) transactionState() (EffectState, bool) {
	r.transactionMu.Lock()
	defer r.transactionMu.Unlock()
	if len(r.transactionStates) == 0 {
		return "", false
	}
	hasApplied, hasRolledBack, hasCompensated := false, false, false
	for _, state := range r.transactionStates {
		switch state {
		case EffectIndeterminate:
			return EffectIndeterminate, true
		case EffectPartiallyApplied:
			return EffectPartiallyApplied, true
		case EffectApplied:
			hasApplied = true
		case EffectRolledBack:
			hasRolledBack = true
		case EffectCompensated:
			hasCompensated = true
		case EffectNotApplicable, EffectNone:
		}
	}
	if hasApplied && hasRolledBack {
		return EffectPartiallyApplied, true
	}
	if hasApplied && hasCompensated {
		return EffectPartiallyApplied, true
	}
	if hasApplied {
		return EffectApplied, true
	}
	if hasCompensated {
		return EffectCompensated, true
	}
	if hasRolledBack {
		return EffectRolledBack, true
	}
	return EffectNone, true
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
	requestCache := newExecutionCache(p, options.Cache)
	executionCtx, cancel := context.WithTimeoutCause(ctx, p.resourceLimits.MaxExecutionDuration, errExecutionResourceDeadline)
	defer cancel()
	scope := executionScope{
		bindings:      make(map[string]executionValue),
		limiter:       make(chan struct{}, p.resourceLimits.MaxConcurrency),
		grace:         options.abandonGrace(),
		effects:       &effectRecorder{},
		annotations:   &directiveAnnotationRecorder{},
		resources:     &resourceMeter{limits: p.resourceLimits},
		cursorScopes:  make(map[uint64]CursorScope),
		idempotency:   newExecutionIdempotency(ctx, options),
		reliability:   &executionReliabilityState{},
		batches:       newBatchRuntime(),
		cache:         requestCache,
		batchObserver: options.Batch.Observe,
	}
	// Cancellation observed before any selection runs is operation-level: no
	// field is responsible, so it carries the empty root path.
	if err := executionCtx.Err(); err != nil {
		outcome := Outcome{
			Data: map[string]any{},
			Errors: []ExecutionError{{
				Code: CodeCancelled, Message: "request cancelled",
				Path: []any{}, Retryable: true, internal: err,
			}},
			Effects:     scope.effects.state(p.kind, true),
			Annotations: scope.annotations.annotations(),
		}
		if errors.Is(context.Cause(executionCtx), errExecutionResourceDeadline) {
			replaceDeadlineFailures(&outcome)
		}
		return enforceOutcomeLimits(outcome, p.resourceLimits)
	}
	if failures := p.preflightCollectionCursors(executionCtx, p.nodes, scope); len(failures) != 0 {
		sortExecutionErrors(failures)
		return enforceOutcomeLimits(Outcome{
			Data: map[string]any{}, Errors: failures,
			Effects: scope.effects.state(p.kind, true), Annotations: scope.annotations.annotations(),
		}, p.resourceLimits)
	}
	var result sequenceResult
	if p.kind == protocol.Mutation && p.atomicity == protocol.OperationAtomicity {
		result = p.executeOperationTransaction(executionCtx, scope)
	} else {
		result = p.executeSequence(executionCtx, p.nodes, scope, nil)
	}
	sortExecutionErrors(result.errors)
	outcome := Outcome{
		Data:        result.data,
		Errors:      result.errors,
		Effects:     scope.effects.state(p.kind, result.failed),
		Annotations: scope.annotations.annotations(),
	}
	outcome.Reliability.Deduplicated = scope.reliability.deduplicated.Load()
	outcome.Reliability.Attempts = scope.reliability.attempts.Load()
	if errors.Is(context.Cause(executionCtx), errExecutionResourceDeadline) {
		replaceDeadlineFailures(&outcome)
	}
	return enforceOutcomeLimits(outcome, p.resourceLimits)
}

func (p *Plan) executeSequence(ctx context.Context, nodes []planNode, scope executionScope, path []any) sequenceResult {
	result := sequenceResult{data: make(map[string]any)}
	for index, node := range nodes {
		if err := ctx.Err(); err != nil {
			result.errors = append(result.errors, nodeExecutionError("CANCELLED", "request cancelled", node, pathForNode(path, node), err))
			result.failed = true
			break
		}
		current := p.executeNode(ctx, node, scope, path)
		result.errors = append(result.errors, current.errors...)
		result.failed = result.failed || current.failed
		if shouldExhaustErrorBudget(len(result.errors), p.resourceLimits.MaxErrors, index+1 < len(nodes)) {
			result.errors = exhaustErrorBudget(result.errors, p.resourceLimits.MaxErrors, node)
			result.failed = true
			break
		}
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
		if executionShouldStop(current.errors) {
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
	if !scope.batches.consumePrecharged(node, nodePath) && !scope.resources.chargeWork(nodeRuntimeWork(node)) {
		return resourceExhaustedResult(node, nodePath, "runtime work budget exhausted")
	}
	if !p.matchesTypeConditions(node, scope.current) {
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueSkipped}}
	}
	run, directives, directiveErrors := p.evaluateDirectives(node, scope, nodePath)
	if len(directiveErrors) != 0 {
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: directiveErrors}
	}
	if !run {
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueSkipped}}
	}

	switch node.kind {
	case protocol.CallSelection, protocol.FieldSelection:
		return p.executeHandlerNode(ctx, node, directives, scope, nodePath)
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
		fragmentScope, parameterErrors := p.fragmentExecutionScope(node, scope, nodePath)
		if len(parameterErrors) != 0 {
			return nodeResult{failed: true, errors: parameterErrors}
		}
		children := p.executeSequence(ctx, node.children, fragmentScope, path)
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
	case protocol.AtomicSelection:
		return p.executeAtomic(ctx, node, scope)
	default:
		err := nodeExecutionError("INTERNAL", "internal execution error", node, nodePath, errors.New("unknown prepared selection kind"))
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{err}}
	}
}

func (p *Plan) executeHandlerNode(ctx context.Context, node planNode, directives []evaluatedDirective, scope executionScope, path []any) nodeResult {
	prepared, terminal := p.prepareHandlerNode(ctx, node, scope, path)
	if terminal != nil {
		return *terminal
	}
	output, terminal, err := p.invokePreparedHandler(ctx, node, directives, scope, path, prepared)
	if terminal != nil {
		return *terminal
	}
	if err != nil {
		prepared.reservation.publish(CacheEntry{Error: err}, cacheStoreable(scope.cache.options, nil, err))
		failure := handlerFailure(node, path, err)
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
	}
	return p.completeHandlerNode(ctx, node, directives, scope, path, prepared, output)
}

type preparedHandlerNode struct {
	inherited   *unavailableTree
	source      any
	input       any
	loaded      batchCallResult
	preloaded   bool
	principal   Principal
	reservation *cacheReservation
	cacheHit    bool
	cached      CacheEntry
}

func (p *Plan) prepareHandlerNode(ctx context.Context, node planNode, scope executionScope, path []any) (preparedHandlerNode, *nodeResult) {
	prepared := preparedHandlerNode{}
	// A member whose completion failed stays unavailable however deep the
	// selection reaches it; its surviving nested marks descend with it.
	if node.kind == protocol.FieldSelection {
		nested, unavailable := scope.current.unavailable.member(node.name)
		if unavailable {
			result := nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}}
			return prepared, &result
		}
		prepared.inherited = nested
	}

	source, sourceErr := memberSource(node, scope, path)
	if sourceErr != nil {
		result := nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{*sourceErr}}
		return prepared, &result
	}
	prepared.source = source
	loaded, preloaded := scope.batches.take(node, path)
	prepared.loaded, prepared.preloaded = loaded, preloaded
	decision := loaded.decision
	if !preloaded || !loaded.authorized {
		var authorizationErr *ExecutionError
		decision, authorizationErr = p.authorizeNodeDecision(ctx, node, source, path)
		if authorizationErr != nil {
			result := nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{*authorizationErr}}
			return prepared, &result
		}
	}

	input, status, inputErr := p.handlerInput(node, scope)
	if inputErr != nil {
		code := executionStatusCode(status)
		err := nodeExecutionError(code, "handler input is unavailable", node, path, inputErr)
		result := nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{err}}
		return prepared, &result
	}
	prepared.input = input

	prepared.principal, _ = PrincipalFromContext(ctx)
	if key, eligible, external := scope.cache.key(p, node, source, input, prepared.principal, decision); eligible {
		entry, hit, reserved, cacheErr := scope.cache.load(ctx, key, external)
		if cacheErr != nil {
			failure := handlerFailure(node, path, cacheErr)
			result := nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
			return prepared, &result
		}
		if hit {
			prepared.cached, prepared.cacheHit = entry, true
		} else {
			prepared.reservation = reserved
		}
	}
	return prepared, nil
}

func (p *Plan) invokePreparedHandler(ctx context.Context, node planNode, directives []evaluatedDirective, scope executionScope, path []any, prepared preparedHandlerNode) (any, *nodeResult, error) {
	if prepared.cacheHit {
		// Authorization has been evaluated for this exact logical input before
		// any request-local or application-owned entry is reused.
		return prepared.cached.Value, nil, prepared.cached.Error
	}
	if prepared.preloaded {
		// The batch handler has already started; record every logical item so
		// effect accounting remains per selected handler.
		scope.recordEffect(node, &scope.effects.started)
		return prepared.loaded.value, nil, prepared.loaded.err
	}
	release, acquireErr := acquireHandlerExecutionSlot(ctx, scope, prepared.reservation)
	if acquireErr != nil {
		failure := nodeExecutionError("CANCELLED", "request cancelled", node, path, acquireErr)
		result := nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
		return nil, &result, nil
	}
	// Recorded before the call, so an effect that started and then failed is
	// never reported as though it never happened.
	scope.recordEffect(node, &scope.effects.started)
	output, err := callWithinGrace(ctx, release, scope.grace, func() (any, error) {
		return p.invokeDirectiveWrappers(ctx, directives, directiveHandlerCall{node: node, source: prepared.source, input: prepared.input, path: path})
	})
	return output, nil, err
}

func (p *Plan) completeHandlerNode(ctx context.Context, node planNode, directives []evaluatedDirective, scope executionScope, path []any, prepared preparedHandlerNode, output any) nodeResult {
	completed, result, failures, available := p.completeHandlerOutput(node, path, prepared.inherited, output)
	if prepared.reservation != nil {
		cached := output
		if available {
			cached = completed
		}
		prepared.reservation.publish(CacheEntry{Value: cached}, available && cacheStoreable(scope.cache.options, cached, nil))
	}

	if available {
		scope.recordEffect(node, &scope.effects.completed)
		if node.definition.descriptor.Metadata.Effect == WriteEffect {
			if invalidationErr := scope.cache.invalidate(ctx, p, node, prepared.principal); invalidationErr != nil {
				failures = append(failures, nodeExecutionError(CodeCacheInvalidationFailed, "application cache invalidation failed", node, path, invalidationErr))
				available = false
				result.status = valueUnavailable
			}
		}
		if annotationErr := p.recordDirectiveAnnotations(ctx, scope, node, directives, path); annotationErr != nil {
			failures = append(failures, directiveAnnotationFailure(node, path, annotationErr))
			available = false
			result.status = valueUnavailable
		}
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

func (p *Plan) completeHandlerOutput(node planNode, path []any, inherited *unavailableTree, output any) (any, executionValue, []ExecutionError, bool) {
	completed, issues, available, completionErr := completeSafely(output, node.definition.descriptor.Output, node.definition.descriptor.OutputNullable, p.types, p.resourceLimits)
	result := executionValue{value: completed, typeInfo: node.output, status: valueAvailable, actualType: runtimeActualType(node.output.id, completed)}
	if completed == nil && available {
		result.status = valueNull
	}
	var failures []ExecutionError
	if completionErr != nil {
		failures = append(failures, outputCompletionFailure(node, path, completionErr))
		available = false
	}
	for _, issue := range issues {
		failures = append(failures, outputCompletionFailure(node, appendPathSegments(path, issue.path...), issue.cause))
	}
	if !available {
		result.status = valueUnavailable
	} else {
		result.unavailable = unavailableMembers(issues, inherited)
	}
	return completed, result, failures, available
}

func directiveAnnotationFailure(node planNode, path []any, err error) ExecutionError {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		failure := nodeExecutionError(CodeCancelled, "request cancelled", node, path, err)
		failure.Retryable = true
		return failure
	}
	return nodeExecutionError(CodeInternal, "directive response annotation failed", node, path, err)
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
	p.prefetchParallelBatches(ctx, node.children, scope, path)
	results := make([]nodeResult, len(node.children))
	workers := min(int(p.resourceLimits.MaxConcurrency), len(node.children))
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
				if executionShouldStop(result.errors) || (result.failed && node.parallelPolicy == protocol.FailFastParallel) {
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

// collectionItemValue projects one element of a completed collection. The
// element inherits the unavailable marks recorded for its index, so a member
// whose completion failed stays unavailable instead of being re-resolved by a
// mapped selection.
func collectionItemValue(parent executionValue, item any, descriptor schema.TypeDescriptor, index int) executionValue {
	status := valueAvailable
	if item == nil {
		status = valueNull
	}
	nested, unavailable := parent.unavailable.member(index)
	if unavailable {
		status = valueUnavailable
	}
	return executionValue{
		value:       item,
		typeInfo:    staticType{id: descriptor.Element, nullable: descriptor.ElementNullable, valid: true},
		status:      status,
		actualType:  runtimeActualType(descriptor.Element, item),
		unavailable: nested,
	}
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
	return failedNodeResult(node, failure)
}

func failedNodeResult(node planNode, failure ExecutionError) nodeResult {
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

func completeSafely(value any, output schema.TypeID, nullable bool, types schema.Snapshot, limits ResourceLimits) (completed any, issues []completionIssue, available bool, err error) {
	return completeRecovering(func() (any, []completionIssue, bool, error) {
		return completeContainedWithLimits(value, output, nullable, types, limits)
	})
}

func outputCompletionFailure(node planNode, path []any, cause error) ExecutionError {
	if errors.Is(cause, errResourceBudget) {
		return nodeExecutionError(CodeResourceExhausted, "output completion budget exhausted", node, path, cause)
	}
	return nodeExecutionError(CodeOutputCompletion, "field output is invalid", node, path, cause)
}

func completeRecovering(complete func() (any, []completionIssue, bool, error)) (completed any, issues []completionIssue, available bool, err error) {
	containPanic(func() {
		completed, issues, available, err = complete()
	}, func() {
		completed = nil
		issues = nil
		available = false
		err = errors.New("completion hook panic")
	})
	return completed, issues, available, err
}

func cloneExecutionScope(scope executionScope) executionScope {
	if scope.bindings != nil {
		scope.bindings = cloneBindings(scope.bindings)
	}
	if scope.variables != nil {
		scope.variables = maps.Clone(scope.variables)
	}
	return scope
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

func acquireHandlerExecutionSlot(ctx context.Context, scope executionScope, reservation *cacheReservation) (func(), error) {
	release, err := acquireExecutionSlot(ctx, scope)
	if err != nil {
		reservation.publish(CacheEntry{Error: err}, false)
	}
	return release, err
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
func callWithinGrace(ctx context.Context, release func(), grace time.Duration, invoke func() (any, error)) (any, error) {
	// An uncancellable context can never abandon, so it needs no extra
	// goroutine and keeps the common sequential path direct.
	if ctx.Done() == nil {
		defer release()
		return invoke()
	}
	// Buffered, so an abandoned handler publishes its result and exits rather
	// than blocking forever on a receiver that has already moved on.
	returned := make(chan handlerOutcome, 1)
	go func() {
		defer release()
		value, err := invoke()
		returned <- handlerOutcome{value: value, err: err}
	}()
	select {
	case outcome := <-returned:
		return outcome.value, outcome.err
	case <-ctx.Done():
	}
	if errors.Is(context.Cause(ctx), errExecutionResourceDeadline) {
		return nil, fmt.Errorf("%w: handler did not return before the execution deadline", context.Cause(ctx))
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
		leftExhausted := len(failures[left].Path) == 1 && failures[left].Path[0] == "$errors"
		rightExhausted := len(failures[right].Path) == 1 && failures[right].Path[0] == "$errors"
		if leftExhausted != rightExhausted {
			return !leftExhausted
		}
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

// recordEffect marks a write-effect milestone, so effect state never depends on
// a caller remembering to check the handler's registered effect.
func (s executionScope) recordEffect(node planNode, milestone *atomic.Bool) {
	if s.effects == nil || node.definition.descriptor.Metadata.Effect != WriteEffect {
		return
	}
	milestone.Store(true)
}

// memberSource isolates the current value for a handler that reads one. A
// member cannot be projected from an unavailable or null continuation, and the
// value is copied so a handler cannot reach runtime-owned state.
func memberSource(node planNode, scope executionScope, path []any) (any, *ExecutionError) {
	if node.kind != protocol.FieldSelection && node.definition.descriptor.Scope != ObjectScope {
		return nil, nil
	}
	if scope.current.status != valueAvailable && scope.current.status != valueNull {
		failure := unavailableContinuationError(node, path, scope.current.status)
		return nil, &failure
	}
	if scope.current.status == valueNull {
		failure := nodeExecutionError(CodeResultNull, "current value is null", node, path, errors.New("non-null member source is null"))
		return nil, &failure
	}
	isolated, copyErr := copyOutput(reflect.ValueOf(sourceValue(scope.current)), 0, &copyState{
		active: make(map[copyReference]bool),
		limits: scope.resources.limits,
	})
	if copyErr != nil {
		code, message := CodeInternal, "internal execution error"
		if errors.Is(copyErr, errResourceBudget) {
			code, message = CodeResourceExhausted, "handler source copy budget exhausted"
		}
		failure := nodeExecutionError(code, message, node, path, fmt.Errorf("copy handler source: %w", copyErr))
		return nil, &failure
	}
	return isolated, nil
}

// handlerFailure classifies a handler error into its public shape. Cancellation
// and containment stay runtime-owned; only a genuine domain failure may select
// its own public code.
func handlerFailure(node planNode, path []any, err error) ExecutionError {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		failure := nodeExecutionError(CodeCancelled, "request cancelled", node, path, err)
		failure.Retryable = true
		return failure
	}
	var panicFailure *handlerPanic
	if errors.As(err, &panicFailure) {
		return nodeExecutionError(CodeInternal, "internal execution error", node, path, err)
	}
	if errors.Is(err, ErrInterceptorNextCalled) || errors.Is(err, ErrDirectiveNextCalled) {
		return nodeExecutionError(CodeInternal, "internal execution error", node, path, err)
	}
	return applyDomainError(nodeExecutionError(CodeHandlerFailed, "field unavailable", node, path, err), err)
}
