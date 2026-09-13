package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/valksor/naatre/protocol"
)

// DefaultMaxBatchSize is the bounded dispatch size used when a registration
// does not choose a smaller application-specific limit.
const DefaultMaxBatchSize = 256

var (
	ErrBatchResultMissing   = errors.New("batch handler omitted a result")
	ErrBatchResultDuplicate = errors.New("batch handler returned a duplicate result")
	ErrBatchResultUnknown   = errors.New("batch handler returned an unknown result index")
)

// BatchInput is one typed, deduplicated input in a deterministic dispatch.
// Index is stable within the dispatch and lets a backend return results in any
// order without coupling correctness to slice position.
type BatchInput[Input any] struct {
	Index int
	Value Input
}

// BatchResult maps one backend result or error to a BatchInput index. Omitting
// an input produces ErrBatchResultMissing for that input; duplicate or unknown
// indexes are rejected as malformed handler output.
type BatchResult[Output any] struct {
	Index int
	Value Output
	Error error
}

// BatchOptions defines the deterministic identity and bound for one loader.
// Key must encode every application-level distinction in the typed input; the
// runtime adds operation, authorization, schema, variable, and request-context
// dimensions before using it for deduplication or caching.
type BatchOptions[Input any] struct {
	MaxSize int
	Key     func(Input) (string, error)
}

// BatchCall is the typed input of an object-call batch handler.
type BatchCall[Source, Input any] struct {
	Source Source
	Input  Input
}

// BatchHandler handles one bounded, deduplicated dispatch. Results may be
// returned out of order because each carries its input index.
type BatchHandler[Input, Output any] func(context.Context, []BatchInput[Input]) []BatchResult[Output]

type batchCall struct {
	index  int
	source any
	input  any
}

type batchCallResult struct {
	index      int
	value      any
	err        error
	present    bool
	decision   AuthorizationDecision
	authorized bool
}

type batchDefinition struct {
	maxSize int
	handler string
	key     func(any, any) (string, error)
	invoke  func(context.Context, []batchCall) []batchCallResult
}

type BatchEventStage string

const (
	BatchDispatchStarted   BatchEventStage = "dispatch-started"
	BatchDispatchCompleted BatchEventStage = "dispatch-completed"
)

// BatchEvent is tracing-safe dispatch telemetry. It intentionally excludes
// typed inputs, principal claims, and backend errors.
type BatchEvent struct {
	ID        string
	Stage     BatchEventStage
	Operation string
	Handler   string
	Size      int
	Failed    bool
	Cancelled bool
}

// BatchRuntimeOptions configures tracing for one execution.
type BatchRuntimeOptions struct {
	Observe func(BatchEvent)
}

type batchRuntime struct {
	mu         sync.Mutex
	results    map[string]batchCallResult
	precharged map[string]bool
}

func newBatchRuntime() *batchRuntime {
	runtime := new(batchRuntime)
	runtime.results = make(map[string]batchCallResult)
	runtime.precharged = make(map[string]bool)
	return runtime
}

func (r *batchRuntime) markPrecharged(node planNode, path []any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.precharged[batchResultKey(node, path)] = true
	r.mu.Unlock()
}

func (r *batchRuntime) consumePrecharged(node planNode, path []any) bool {
	if r == nil {
		return false
	}
	key := batchResultKey(node, path)
	r.mu.Lock()
	charged := r.precharged[key]
	delete(r.precharged, key)
	r.mu.Unlock()
	return charged
}

func (r *batchRuntime) put(node planNode, path []any, result batchCallResult) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.results[batchResultKey(node, path)] = result
	r.mu.Unlock()
}

func (r *batchRuntime) take(node planNode, path []any) (batchCallResult, bool) {
	if r == nil {
		return batchCallResult{}, false
	}
	key := batchResultKey(node, path)
	r.mu.Lock()
	result, ok := r.results[key]
	delete(r.results, key)
	r.mu.Unlock()
	return result, ok
}

func batchResultKey(node planNode, path []any) string {
	encoded, _ := json.Marshal(path)
	return registrationKey(node.definition.descriptor) + "\x00" + string(encoded)
}

// BindBatch adapts a typed root handler to request-scoped batch execution.
func BindBatch[Input, Output any](descriptor Descriptor, options BatchOptions[Input], handler BatchHandler[Input, Output]) Definition {
	definition := newDefinition(descriptor, rootBinding, nil, reflect.TypeFor[Input](), reflect.TypeFor[Output](), handler == nil)
	installBatch(&definition, options, handler, func(source, input any) Input {
		return input.(Input)
	})
	return definition
}

// BindBatchInvocation binds a root batch handler that consumes the executor's
// immutable Invocation value.
func BindBatchInvocation[Output any](descriptor Descriptor, options BatchOptions[Invocation], handler BatchHandler[Invocation, Output]) Definition {
	return invocationDefinition(BindBatch(descriptor, options, handler))
}

// BindBatchField adapts a typed object projection to request-scoped batch
// execution. The source object is the typed loader input.
func BindBatchField[Source, Output any](descriptor Descriptor, options BatchOptions[Source], handler BatchHandler[Source, Output]) Definition {
	definition := newDefinition(descriptor, fieldBinding, reflect.TypeFor[Source](), nil, reflect.TypeFor[Output](), handler == nil)
	installBatch(&definition, options, handler, func(source, input any) Source {
		return source.(Source)
	})
	return definition
}

// BindBatchCall adapts a typed object method to request-scoped batch execution.
func BindBatchCall[Source, Input, Output any](descriptor Descriptor, options BatchOptions[BatchCall[Source, Input]], handler BatchHandler[BatchCall[Source, Input], Output]) Definition {
	definition := newDefinition(descriptor, callBinding, reflect.TypeFor[Source](), reflect.TypeFor[Input](), reflect.TypeFor[Output](), handler == nil)
	installBatch(&definition, options, handler, func(source, input any) BatchCall[Source, Input] {
		return BatchCall[Source, Input]{Source: source.(Source), Input: input.(Input)}
	})
	return definition
}

func installBatch[Input, Output any](definition *Definition, options BatchOptions[Input], handler BatchHandler[Input, Output], typed func(any, any) Input) {
	maximum := options.MaxSize
	if maximum == 0 {
		maximum = DefaultMaxBatchSize
	}
	batch := &batchDefinition{
		maxSize: maximum, handler: registrationKey(definition.descriptor),
		key: batchKeyAdapter(options.Key, typed), invoke: batchHandlerAdapter(handler, typed),
	}
	definition.batch = batch
	definition.invoke = singleBatchInvoker(batch)
}

func batchKeyAdapter[Input any](key func(Input) (string, error), typed func(any, any) Input) func(any, any) (string, error) {
	if key == nil {
		return nil
	}
	return func(source, input any) (result string, err error) {
		defer func() {
			if recover() != nil {
				result = ""
				err = errors.New("batch key function panicked")
			}
		}()
		return key(typed(source, input))
	}
}

func batchHandlerAdapter[Input, Output any](handler BatchHandler[Input, Output], typed func(any, any) Input) func(context.Context, []batchCall) []batchCallResult {
	if handler == nil {
		return nil
	}
	return func(ctx context.Context, calls []batchCall) (results []batchCallResult) {
		defer func() {
			if recover() != nil {
				results = batchFailureResults(calls, &handlerPanic{})
			}
		}()
		inputs := make([]BatchInput[Input], len(calls))
		for index, call := range calls {
			inputs[index] = BatchInput[Input]{Index: call.index, Value: typed(call.source, call.input)}
		}
		return normalizeBatchResults(calls, handler(ctx, inputs))
	}
}

func normalizeBatchResults[Output any](calls []batchCall, handled []BatchResult[Output]) []batchCallResult {
	known := make(map[int]bool, len(calls))
	for _, call := range calls {
		known[call.index] = true
	}
	mapped := make(map[int]batchCallResult, len(handled))
	for _, result := range handled {
		if !known[result.Index] {
			return batchFailureResults(calls, fmt.Errorf("%w: %d", ErrBatchResultUnknown, result.Index))
		}
		if mapped[result.Index].present {
			return batchFailureResults(calls, fmt.Errorf("%w: %d", ErrBatchResultDuplicate, result.Index))
		}
		mapped[result.Index] = batchCallResult{index: result.Index, value: result.Value, err: result.Error, present: true}
	}
	results := make([]batchCallResult, len(calls))
	for index, call := range calls {
		result, ok := mapped[call.index]
		if !ok {
			result = batchCallResult{index: call.index, err: ErrBatchResultMissing, present: true}
		}
		results[index] = result
	}
	return results
}

func singleBatchInvoker(batch *batchDefinition) func(context.Context, any, any) (any, error) {
	return func(ctx context.Context, source, input any) (any, error) {
		results := batch.invoke(ctx, []batchCall{{index: 0, source: source, input: input}})
		if len(results) != 1 {
			return nil, ErrBatchResultMissing
		}
		return results[0].value, results[0].err
	}
}

func batchFailureResults(calls []batchCall, err error) []batchCallResult {
	results := make([]batchCallResult, 0, len(calls))
	for _, call := range calls {
		results = append(results, batchCallResult{index: call.index, err: err, present: true})
	}
	return results
}

type collectionBatchItem struct {
	scope   executionScope
	path    []any
	data    map[string]any
	errors  []ExecutionError
	valid   bool
	failed  bool
	stopped bool
}

func (p *Plan) hasBatchCollectionChild(nodes []planNode) bool {
	if !collectionBatchSafe(nodes) {
		return false
	}
	for _, node := range nodes {
		if p.canBatchCollectionChild(node) {
			return true
		}
	}
	return false
}

func collectionBatchSafe(nodes []planNode) bool {
	for _, node := range nodes {
		if node.hasDefinition {
			metadata := node.definition.descriptor.Metadata
			if metadata.Effect != ReadEffect || metadata.ThreadSafety != ThreadSafe || metadata.Transaction == TransactionRequired {
				return false
			}
		}
		for _, directive := range node.directives {
			if directive.definition.Wrapper != nil || directive.definition.Descriptor.Effect != string(ReadEffect) {
				return false
			}
		}
		if !collectionBatchSafe(node.children) {
			return false
		}
	}
	return true
}

func (p *Plan) canBatchCollectionChild(node planNode) bool {
	if !node.hasDefinition || node.definition.batch == nil ||
		(node.kind != protocol.FieldSelection && node.kind != protocol.CallSelection) ||
		len(p.interceptorsFor(node)) != 0 {
		return false
	}
	for _, directive := range node.directives {
		if directive.definition.Wrapper != nil {
			return false
		}
	}
	metadata := node.definition.descriptor.Metadata
	return metadata.Batching == BatchEligible && metadata.Effect == ReadEffect && metadata.ThreadSafety == ThreadSafe
}

func (p *Plan) executeBatchedCollectionItems(ctx context.Context, node planNode, scope executionScope, execution collectionExecution) ([]any, []ExecutionError, bool) {
	items := initializeCollectionBatchItems(scope, execution)
	for _, child := range node.children {
		if p.canBatchCollectionChild(child) {
			p.prefetchCollectionBatch(ctx, child, items)
		}
		for index := range items {
			item := &items[index]
			if !item.valid || item.stopped {
				continue
			}
			result := p.executeNode(ctx, child, item.scope, item.path)
			item.errors = append(item.errors, result.errors...)
			item.failed = item.failed || result.failed
			applyCollectionChildResult(item, child, result)
			if executionShouldStop(result.errors) || (result.failed && p.kind == protocol.Mutation) {
				item.stopped = true
			}
		}
	}
	return p.collectCollectionBatchItems(node, items)
}

func initializeCollectionBatchItems(scope executionScope, execution collectionExecution) []collectionBatchItem {
	items := make([]collectionBatchItem, len(execution.items))
	for offset, item := range execution.items {
		value := collectionItemValue(scope.current, item, execution.descriptor, execution.sourceIndexBase+offset)
		if value.status == valueNull || value.status == valueUnavailable {
			continue
		}
		items[offset] = collectionBatchItem{
			scope: childExecutionScope(scope, value, scope.current),
			path:  appendPathSegments(execution.path, offset), data: make(map[string]any), valid: true,
		}
	}
	return items
}

func (p *Plan) collectCollectionBatchItems(node planNode, items []collectionBatchItem) ([]any, []ExecutionError, bool) {
	output := make([]any, len(items))
	var failures []ExecutionError
	failed := false
	for index := range items {
		item := &items[index]
		if !item.valid {
			continue
		}
		var exhausted bool
		failures, exhausted = appendCollectionErrors(failures, item.errors, p.resourceLimits.MaxErrors, node, index+1 < len(items))
		if exhausted {
			return output, failures, true
		}
		if item.failed && len(item.data) == 0 {
			output[index] = nil
		} else {
			output[index] = item.data
		}
		failed = failed || item.failed
	}
	return output, failures, failed
}

func applyCollectionChildResult(item *collectionBatchItem, node planNode, result nodeResult) {
	if node.binding != "" {
		bound := result.value
		if result.failed && bound.status == valueAvailable {
			bound.status = valueUnavailable
		}
		item.scope.bindings[node.binding] = bound
	}
	if result.emit {
		item.data[node.outputName] = result.data
	}
	if !result.merge {
		return
	}
	members, ok := result.data.(map[string]any)
	if !ok {
		item.errors = append(item.errors, nodeExecutionError("OUTPUT_COMPLETION", "field output is invalid", node, item.path, errors.New("unnested result is not an object")))
		item.failed = true
		return
	}
	for name, value := range members {
		item.data[name] = value
	}
}

type groupedBatchCall struct {
	call      batchCall
	paths     [][]any
	decisions []AuthorizationDecision
}

type preparedBatchCandidate struct {
	source   any
	input    any
	key      string
	decision AuthorizationDecision
}

func (p *Plan) prepareBatchCandidate(ctx context.Context, node planNode, scope executionScope, path []any) (preparedBatchCandidate, bool) {
	if !scope.resources.chargeWork(nodeRuntimeWork(node)) {
		return preparedBatchCandidate{}, false
	}
	scope.batches.markPrecharged(node, path)
	if !p.matchesTypeConditions(node, scope.current) {
		return preparedBatchCandidate{}, false
	}
	run, _, directiveErrors := p.evaluateDirectives(node, scope, path)
	if !run || len(directiveErrors) != 0 {
		return preparedBatchCandidate{}, false
	}
	source, sourceErr := memberSource(node, scope, path)
	if sourceErr != nil {
		return preparedBatchCandidate{}, false
	}
	decision, authorizationErr := p.authorizeNodeDecision(ctx, node, source, path)
	if authorizationErr != nil {
		return preparedBatchCandidate{}, false
	}
	input, _, inputErr := p.handlerInput(node, scope)
	if inputErr != nil {
		return preparedBatchCandidate{}, false
	}
	key, keyErr := node.definition.batch.key(source, input)
	if keyErr != nil {
		scope.batches.put(node, path, batchCallResult{err: keyErr, present: true, decision: decision, authorized: true})
		return preparedBatchCandidate{}, false
	}
	return preparedBatchCandidate{source: source, input: input, key: key, decision: decision}, true
}

func (p *Plan) batchCandidateIdentity(node planNode, principal Principal, candidate preparedBatchCandidate, unique int) string {
	identity := p.operationName + "\x00" + string(p.kind) + "\x00" + batchAuthorizationIdentity(principal, candidate.decision) + "\x00" + candidate.key
	if !node.definition.descriptor.Metadata.Cacheable {
		identity += fmt.Sprintf("\x00%d", unique)
	}
	return identity
}

func (p *Plan) prefetchCollectionBatch(ctx context.Context, node planNode, items []collectionBatchItem) {
	definition := node.definition.batch
	principal, _ := PrincipalFromContext(ctx)
	var requestBatches *batchRuntime
	var dispatchScope executionScope
	for index := range items {
		if items[index].valid {
			requestBatches = items[index].scope.batches
			dispatchScope = items[index].scope
			break
		}
	}
	groups := make([]groupedBatchCall, 0, len(items))
	byKey := make(map[string]int, len(items))
	for index := range items {
		item := &items[index]
		if !item.valid || item.stopped {
			continue
		}
		path := pathForNode(item.path, node)
		candidate, ok := p.prepareBatchCandidate(ctx, node, item.scope, path)
		if !ok {
			continue
		}
		identity := p.batchCandidateIdentity(node, principal, candidate, index)
		if group, ok := byKey[identity]; ok {
			groups[group].paths = append(groups[group].paths, slices.Clone(path))
			groups[group].decisions = append(groups[group].decisions, candidate.decision)
			continue
		}
		byKey[identity] = len(groups)
		groups = append(groups, groupedBatchCall{
			call:  batchCall{index: len(groups), source: candidate.source, input: candidate.input},
			paths: [][]any{slices.Clone(path)}, decisions: []AuthorizationDecision{candidate.decision},
		})
	}
	p.dispatchCollectionBatchGroups(ctx, node, definition, dispatchScope, requestBatches, groups)
}

func (p *Plan) dispatchCollectionBatchGroups(ctx context.Context, node planNode, definition *batchDefinition, scope executionScope, batches *batchRuntime, groups []groupedBatchCall) {
	for start := 0; start < len(groups); start += definition.maxSize {
		end := min(start+definition.maxSize, len(groups))
		calls := make([]batchCall, end-start)
		for index := start; index < end; index++ {
			calls[index-start] = groups[index].call
		}
		results := p.dispatchBatch(ctx, definition, calls, scope)
		for index, result := range results {
			group := groups[start+index]
			for pathIndex, path := range group.paths {
				result.decision = group.decisions[pathIndex]
				result.authorized = true
				batches.put(node, path, result)
			}
		}
	}
}

func batchAuthorizationIdentity(principal Principal, decision AuthorizationDecision) string {
	expiresAt := ""
	if !decision.ExpiresAt.IsZero() {
		expiresAt = decision.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	encoded, _ := json.Marshal(struct {
		Principal Principal               `json:"principal"`
		Revision  string                  `json:"authorizationRevision,omitempty"`
		Scope     AuthorizationCacheScope `json:"cacheScope"`
		ExpiresAt string                  `json:"expiresAt,omitempty"`
	}{
		Principal: principal, Revision: decision.AuthorizationRevision, Scope: decision.CacheScope,
		ExpiresAt: expiresAt,
	})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (p *Plan) dispatchBatch(ctx context.Context, definition *batchDefinition, calls []batchCall, scope executionScope) []batchCallResult {
	batchID := ""
	if scope.telemetry != nil {
		batchID = scope.telemetry.nextID("batch", definition.handler)
	}
	p.observeBatch(scope, BatchEvent{ID: batchID, Stage: BatchDispatchStarted, Operation: p.operationName, Handler: definition.handler, Size: len(calls)})
	release, err := acquireExecutionSlot(ctx, scope)
	if err != nil {
		p.observeBatch(scope, BatchEvent{ID: batchID, Stage: BatchDispatchCompleted, Operation: p.operationName, Handler: definition.handler, Size: len(calls), Failed: true, Cancelled: true})
		return batchFailureResults(calls, err)
	}
	var results []batchCallResult
	_, err = callWithinGrace(ctx, release, scope.grace, func() (any, error) {
		results = definition.invoke(ctx, calls)
		return nil, nil
	})
	if err != nil {
		p.observeBatch(scope, BatchEvent{ID: batchID, Stage: BatchDispatchCompleted, Operation: p.operationName, Handler: definition.handler, Size: len(calls), Failed: true, Cancelled: errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)})
		return batchFailureResults(calls, err)
	}
	failed := false
	for _, result := range results {
		failed = failed || result.err != nil
	}
	p.observeBatch(scope, BatchEvent{ID: batchID, Stage: BatchDispatchCompleted, Operation: p.operationName, Handler: definition.handler, Size: len(calls), Failed: failed})
	return results
}

func (p *Plan) observeBatch(scope executionScope, event BatchEvent) {
	observeSafely(scope.batchObserver, event)
	if scope.telemetry == nil {
		return
	}
	stage, outcome := TelemetryStarted, TelemetryOutcome("")
	if event.Stage == BatchDispatchCompleted {
		stage, outcome = TelemetryCompleted, TelemetrySucceeded
		if event.Failed {
			stage, outcome = TelemetryFailed, TelemetryFailedOutcome
		}
		if event.Cancelled {
			stage, outcome = TelemetryCancelled, TelemetryCancelledOutcome
		}
	}
	observed := scope.telemetry.baseEvent(TelemetryBatch, stage)
	observed.ID = event.ID
	observed.ParentID, observed.Handler, observed.BatchSize, observed.Outcome = scope.telemetry.parentID(), event.Handler, event.Size, outcome
	emitTelemetry(scope.telemetry.options, observed)
}

type parallelBatchTarget struct {
	node     planNode
	path     []any
	decision AuthorizationDecision
}

type parallelBatchLoader struct {
	definition *batchDefinition
	calls      []batchCall
	targets    [][]parallelBatchTarget
	byKey      map[string]int
}

func (p *Plan) prefetchParallelBatches(ctx context.Context, nodes []planNode, scope executionScope, path []any) {
	principal, _ := PrincipalFromContext(ctx)
	loaders := make([]parallelBatchLoader, 0)
	loaderByDefinition := make(map[string]int)
	for nodeIndex := range nodes {
		node := nodes[nodeIndex]
		if !p.canBatchCollectionChild(node) {
			continue
		}
		nodePath := pathForNode(path, node)
		branchScope := cloneExecutionScope(scope)
		branchScope.parallel = true
		candidate, ok := p.prepareBatchCandidate(ctx, node, branchScope, nodePath)
		if !ok {
			continue
		}
		definitionKey := registrationKey(node.definition.descriptor)
		loaderIndex, ok := loaderByDefinition[definitionKey]
		if !ok {
			loaderIndex = len(loaders)
			loaderByDefinition[definitionKey] = loaderIndex
			loaders = append(loaders, parallelBatchLoader{definition: node.definition.batch, byKey: make(map[string]int)})
		}
		loader := &loaders[loaderIndex]
		identity := p.batchCandidateIdentity(node, principal, candidate, nodeIndex)
		target := parallelBatchTarget{node: node, path: slices.Clone(nodePath), decision: candidate.decision}
		if callIndex, exists := loader.byKey[identity]; exists {
			loader.targets[callIndex] = append(loader.targets[callIndex], target)
			continue
		}
		callIndex := len(loader.calls)
		loader.byKey[identity] = callIndex
		loader.calls = append(loader.calls, batchCall{index: callIndex, source: candidate.source, input: candidate.input})
		loader.targets = append(loader.targets, []parallelBatchTarget{target})
	}
	p.dispatchParallelBatchLoaders(ctx, scope, loaders)
}

func (p *Plan) dispatchParallelBatchLoaders(ctx context.Context, scope executionScope, loaders []parallelBatchLoader) {
	for loaderIndex := range loaders {
		loader := &loaders[loaderIndex]
		for start := 0; start < len(loader.calls); start += loader.definition.maxSize {
			end := min(start+loader.definition.maxSize, len(loader.calls))
			results := p.dispatchBatch(ctx, loader.definition, loader.calls[start:end], scope)
			for resultIndex, result := range results {
				for _, target := range loader.targets[start+resultIndex] {
					result.decision = target.decision
					result.authorized = true
					scope.batches.put(target.node, target.path, result)
				}
			}
		}
	}
}
