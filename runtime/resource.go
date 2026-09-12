package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/valksor/naatre/protocol"
)

const (
	minimumOutputEnvelopeBytes = 320
	maxPortableConcurrency     = 65_536
)

var (
	errExecutionResourceDeadline = errors.New("execution resource deadline exceeded")
	errResourceBudget            = errors.New("request resource budget exhausted")
)

// ResourceLimits bounds planning, execution, and response construction for one
// request. Zero values select the portable defaults.
type ResourceLimits struct {
	MaxPlanDepth              uint64
	MaxPlanNodes              uint64
	MaxPlannedCalls           uint64
	MaxSelectedFields         uint64
	MaxFragmentExpansionDepth uint64
	MaxFragmentSelections     uint64
	MaxParallelWidth          uint64
	MaxQueuedWork             uint64
	MaxStaticCost             uint64
	MaxValidationErrors       uint64
	MaxRuntimeWork            uint64
	MaxCollectionItems        uint64
	MaxConcurrency            uint64
	MaxExecutionDuration      time.Duration
	MaxOutputBytes            uint64
	MaxErrors                 uint64
	MaxOutputDepth            uint64
	MaxOutputNodes            uint64
}

// PrepareOptions configures resource enforcement while building a plan.
type PrepareOptions struct {
	Limits ResourceLimits
}

// DefaultResourceLimits returns the finite reference defaults used by Prepare.
func DefaultResourceLimits() ResourceLimits {
	return ResourceLimits{
		MaxPlanDepth: 128, MaxPlanNodes: 100_000, MaxPlannedCalls: 16_384, MaxSelectedFields: 65_536,
		MaxFragmentExpansionDepth: 128, MaxFragmentSelections: 16_384,
		MaxParallelWidth: 1_024, MaxQueuedWork: 16_384, MaxStaticCost: 1 << 60, MaxValidationErrors: 100,
		MaxRuntimeWork: 1_000_000, MaxCollectionItems: 100_000, MaxConcurrency: 8,
		MaxExecutionDuration: 30 * time.Second, MaxOutputBytes: 16 << 20, MaxErrors: 100,
		MaxOutputDepth: 64, MaxOutputNodes: 100_000,
	}
}

func resolveResourceLimits(configured ResourceLimits) (ResourceLimits, error) {
	resolved := configured
	defaults := DefaultResourceLimits()
	configuredValue := reflect.ValueOf(configured)
	resolvedValue := reflect.ValueOf(&resolved).Elem()
	defaultValue := reflect.ValueOf(defaults)
	for index := range configuredValue.NumField() {
		field := configuredValue.Field(index)
		if field.IsZero() {
			resolvedValue.Field(index).Set(defaultValue.Field(index))
		}
	}
	if configured.MaxExecutionDuration < 0 {
		return ResourceLimits{}, errors.New("maximum execution duration must not be negative")
	}
	if resolved.MaxConcurrency > uint64(math.MaxInt) {
		return ResourceLimits{}, errors.New("maximum concurrency exceeds the platform integer range")
	}
	if resolved.MaxConcurrency > maxPortableConcurrency {
		return ResourceLimits{}, fmt.Errorf("maximum concurrency exceeds the portable ceiling of %d", maxPortableConcurrency)
	}
	if configured.MaxOutputBytes != 0 && configured.MaxOutputBytes < minimumOutputEnvelopeBytes {
		return ResourceLimits{}, fmt.Errorf("maximum output bytes must reserve at least %d bytes", minimumOutputEnvelopeBytes)
	}
	return resolved, nil
}

func checkedResourceAdd(left, right uint64) (uint64, bool) {
	if right > math.MaxUint64-left {
		return 0, false
	}
	return left + right, true
}

func checkedResourceMultiply(left, right uint64) (uint64, bool) {
	if left != 0 && right > math.MaxUint64/left {
		return 0, false
	}
	return left * right, true
}

type planResourceUsage struct {
	nodes, calls, fields, queued, cost uint64
	parallelWidth                      uint64
}

func (v *planValidator) validatePlanResources(nodes []planNode) {
	usage := planResourceUsage{cost: v.directiveCost}
	reported := make(map[string]bool)
	var walk func([]planNode, uint64, uint64)
	walk = func(current []planNode, depth, multiplier uint64) {
		for index := range current {
			node := &current[index]
			v.accumulatePlanResourceUsage(&usage, node, multiplier, reported)
			v.checkPlanResourceUsage(usage, depth, node.source, reported)
			nextMultiplier, multiplied := pageCostMultiplier(*node, multiplier)
			if !multiplied {
				v.addResourceIssue(reported, "LIMIT_COST_OVERFLOW", "page cost multiplier overflowed", node.source)
			}
			walk(node.children, depth+1, nextMultiplier)
		}
	}
	walk(nodes, 1, 1)
	v.staticCost = usage.cost
}

func pageCostMultiplier(node planNode, multiplier uint64) (uint64, bool) {
	if node.kind != protocol.PageSelection {
		return multiplier, true
	}
	size, ok := node.selection.First()
	if !ok {
		size, ok = node.selection.Last()
	}
	if !ok || !size.IsUint64() {
		return multiplier, true
	}
	next, multiplied := checkedResourceMultiply(multiplier, size.Uint64())
	if !multiplied {
		return math.MaxUint64, false
	}
	return next, true
}

func (v *planValidator) accumulatePlanResourceUsage(usage *planResourceUsage, node *planNode, multiplier uint64, reported map[string]bool) {
	usage.nodes++
	switch node.kind {
	case protocol.CallSelection:
		usage.calls++
	case protocol.FieldSelection:
		usage.fields++
	case protocol.ParallelSelection:
		width := uint64(len(node.children))
		usage.parallelWidth = max(usage.parallelWidth, width)
		if next, ok := checkedResourceAdd(usage.queued, width); ok {
			usage.queued = next
		} else {
			v.addResourceIssue(reported, "LIMIT_COST_OVERFLOW", "resource accounting overflowed", node.source)
		}
	case protocol.PipelineSelection, protocol.MapSelection, protocol.IndexSelection,
		protocol.SliceSelection, protocol.PageSelection, protocol.MetaSelection,
		protocol.FragmentSelection, protocol.CurrentSelection, protocol.NestSelection,
		protocol.UnnestSelection, protocol.AtomicSelection:
	}
	cost := node.staticCost
	if node.hasDefinition {
		cost = node.definition.descriptor.Metadata.Cost
	}
	cost, multiplied := checkedResourceMultiply(cost, multiplier)
	if !multiplied {
		v.addResourceIssue(reported, "LIMIT_COST_OVERFLOW", "static cost accounting overflowed", node.source)
		usage.cost = math.MaxUint64
		return
	}
	next, ok := checkedResourceAdd(usage.cost, cost)
	if !ok {
		v.addResourceIssue(reported, "LIMIT_COST_OVERFLOW", "static cost accounting overflowed", node.source)
		usage.cost = math.MaxUint64
		return
	}
	usage.cost = next
}

func (v *planValidator) checkPlanResourceUsage(usage planResourceUsage, depth uint64, source protocol.Source, reported map[string]bool) {
	checks := []struct {
		exceeded bool
		code     string
		message  string
	}{
		{depth > v.resourceLimits.MaxPlanDepth, "LIMIT_PLAN_DEPTH", "plan depth exceeds the request budget"},
		{usage.nodes > v.resourceLimits.MaxPlanNodes, "LIMIT_PLAN_NODES", "plan node count exceeds the request budget"},
		{usage.calls > v.resourceLimits.MaxPlannedCalls, "LIMIT_PLANNED_CALLS", "planned call count exceeds the request budget"},
		{usage.fields > v.resourceLimits.MaxSelectedFields, "LIMIT_SELECTED_FIELDS", "selected field count exceeds the request budget"},
		{usage.parallelWidth > v.resourceLimits.MaxParallelWidth, "LIMIT_PARALLEL_WIDTH", "parallel width exceeds the request budget"},
		{usage.queued > v.resourceLimits.MaxQueuedWork, "LIMIT_QUEUED_WORK", "queued work exceeds the request budget"},
		{usage.cost > v.resourceLimits.MaxStaticCost, "LIMIT_STATIC_COST", "static cost exceeds the request budget"},
	}
	for _, check := range checks {
		if check.exceeded {
			v.addResourceIssue(reported, check.code, check.message, source)
		}
	}
}

func (v *planValidator) addResourceIssue(reported map[string]bool, code, message string, source protocol.Source) {
	if reported[code] {
		return
	}
	reported[code] = true
	v.add(code, "SEC-131", message, source)
}

func (v *planValidator) addPlannedResourceCost(cost uint64, source protocol.Source) bool {
	next, ok := checkedResourceAdd(v.staticCost, cost)
	if !ok {
		v.add("LIMIT_COST_OVERFLOW", "SEC-131", "static cost accounting overflowed", source)
		return false
	}
	v.staticCost = next
	if next > v.resourceLimits.MaxStaticCost {
		v.add("LIMIT_STATIC_COST", "SEC-131", "static cost exceeds the request budget", source)
		return false
	}
	return true
}

type resourceMeter struct {
	limits          ResourceLimits
	work            atomic.Uint64
	collectionItems atomic.Uint64
}

func (m *resourceMeter) chargeWork(amount uint64) bool {
	return chargeResourceCounter(&m.work, amount, m.limits.MaxRuntimeWork)
}

func (m *resourceMeter) chargeCollection(items uint64) bool {
	return chargeResourceCounter(&m.collectionItems, items, m.limits.MaxCollectionItems)
}

func chargeResourceCounter(counter *atomic.Uint64, amount, limit uint64) bool {
	for {
		current := counter.Load()
		next, ok := checkedResourceAdd(current, amount)
		if !ok || next > limit {
			return false
		}
		if counter.CompareAndSwap(current, next) {
			return true
		}
	}
}

func nodeRuntimeWork(node planNode) uint64 {
	if node.hasDefinition {
		return max(uint64(1), node.definition.descriptor.Metadata.Cost)
	}
	return 1
}

func resourceExhaustedResult(node planNode, path []any, message string) nodeResult {
	failure := nodeExecutionError(CodeResourceExhausted, message, node, path, errResourceBudget)
	return failedNodeResult(node, failure)
}

func executionShouldStop(failures []ExecutionError) bool {
	return containsExecutionCode(failures, CodeCancelled) || containsExecutionCode(failures, CodeResourceExhausted)
}

func exhaustErrorBudget(failures []ExecutionError, limit uint64, node planNode) []ExecutionError {
	exhausted := nodeExecutionError(CodeResourceExhausted, "error budget exhausted", node, []any{"$errors"}, errResourceBudget)
	return enforceErrorCountWith(failures, limit, exhausted)
}

func shouldExhaustErrorBudget(count int, limit uint64, hasMore bool) bool {
	return uint64(count) > limit || (hasMore && uint64(count) >= limit)
}

func appendCollectionErrors(failures, incoming []ExecutionError, limit uint64, node planNode, hasMore bool) ([]ExecutionError, bool) {
	failures = append(failures, incoming...)
	if !shouldExhaustErrorBudget(len(failures), limit, hasMore) {
		return failures, false
	}
	return exhaustErrorBudget(failures, limit, node), true
}

func replaceDeadlineFailures(outcome *Outcome) {
	for index := range outcome.Errors {
		failure := &outcome.Errors[index]
		if failure.Code == CodeCancelled || errors.Is(failure.internal, errExecutionResourceDeadline) {
			failure.Code = CodeResourceExhausted
			failure.Message = "execution deadline exhausted"
			failure.Retryable = false
			failure.internal = errExecutionResourceDeadline
		}
	}
}

func enforceOutcomeLimits(outcome Outcome, limits ResourceLimits) Outcome {
	outcome.Errors = enforceErrorCount(outcome.Errors, limits.MaxErrors)
	if rawPayloadWithin(outcome, limits.MaxOutputBytes) {
		if encoded, err := json.Marshal(outcome); err == nil && uint64(len(encoded)) <= limits.MaxOutputBytes {
			return outcome
		}
	}
	outcome.Data = map[string]any{}
	outcome.Annotations = nil
	exhausted := ExecutionError{Code: CodeResourceExhausted, Message: "output budget exhausted", Path: []any{"$output"}}
	outcome.Errors = enforceErrorCountWith(outcome.Errors, limits.MaxErrors, exhausted)
	encoded, err := json.Marshal(outcome)
	if err == nil && uint64(len(encoded)) <= limits.MaxOutputBytes {
		return outcome
	}
	exhausted.Message = "output exhausted"
	exhausted.Path = nil
	outcome.Errors = []ExecutionError{exhausted}
	return outcome
}

func enforceErrorCount(failures []ExecutionError, limit uint64) []ExecutionError {
	if uint64(len(failures)) <= limit {
		return failures
	}
	exhausted := ExecutionError{Code: CodeResourceExhausted, Message: "error budget exhausted", Path: []any{"$errors"}}
	return enforceErrorCountWith(failures, limit, exhausted)
}

func enforceErrorCountWith(failures []ExecutionError, limit uint64, exhausted ExecutionError) []ExecutionError {
	if limit == 1 {
		return []ExecutionError{exhausted}
	}
	keep := int(min(uint64(len(failures)), limit-1))
	bounded := append([]ExecutionError(nil), failures[:keep]...)
	return append(bounded, exhausted)
}

func rawPayloadWithin(value any, limit uint64) bool {
	budget := rawPayloadBudget{remaining: limit}
	return budget.visit(reflect.ValueOf(value))
}

type rawPayloadBudget struct {
	remaining uint64
}

func (b *rawPayloadBudget) visit(current reflect.Value) bool {
	if !consumeResourceBytes(&b.remaining, 1) {
		return false
	}
	current, terminal := dereferencePayload(current)
	if terminal {
		return true
	}
	if current.Type() == reflect.TypeOf(json.RawMessage{}) || current.Kind() == reflect.String {
		return consumeResourceBytes(&b.remaining, uint64(current.Len()))
	}
	switch current.Kind() {
	case reflect.Slice, reflect.Array:
		return b.visitSequence(current)
	case reflect.Map:
		return b.visitMap(current)
	case reflect.Struct:
		return b.visitStruct(current)
	case reflect.Invalid, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128,
		reflect.Chan, reflect.Func, reflect.Interface, reflect.Pointer, reflect.String, reflect.UnsafePointer:
		return true
	}
	return true
}

func dereferencePayload(current reflect.Value) (reflect.Value, bool) {
	if !current.IsValid() {
		return current, true
	}
	for current.Kind() == reflect.Interface || current.Kind() == reflect.Pointer {
		if current.IsNil() {
			return current, true
		}
		current = current.Elem()
	}
	return current, false
}

func (b *rawPayloadBudget) visitSequence(current reflect.Value) bool {
	for index := 0; index < current.Len(); index++ {
		if !b.visit(current.Index(index)) {
			return false
		}
	}
	return true
}

func (b *rawPayloadBudget) visitMap(current reflect.Value) bool {
	iterator := current.MapRange()
	for iterator.Next() {
		if !b.visit(iterator.Key()) || !b.visit(iterator.Value()) {
			return false
		}
	}
	return true
}

func (b *rawPayloadBudget) visitStruct(current reflect.Value) bool {
	for index := 0; index < current.NumField(); index++ {
		field := current.Type().Field(index)
		if field.PkgPath == "" && !b.visit(current.Field(index)) {
			return false
		}
	}
	return true
}

func consumeResourceBytes(remaining *uint64, size uint64) bool {
	if size > *remaining {
		return false
	}
	*remaining -= size
	return true
}
