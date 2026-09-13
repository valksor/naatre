package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// RetryBudget is a concurrency-safe end-to-end attempt budget shared by every
// retrying layer participating in one logical request.
type RetryBudget struct {
	remaining atomic.Uint32
}

func NewRetryBudget(attempts uint32) *RetryBudget {
	budget := &RetryBudget{}
	budget.remaining.Store(attempts)
	return budget
}

func (b *RetryBudget) Remaining() uint32 {
	if b == nil {
		return 0
	}
	return b.remaining.Load()
}

func (b *RetryBudget) claim() bool {
	if b == nil {
		return true
	}
	for remaining := b.remaining.Load(); remaining > 0; remaining = b.remaining.Load() {
		if b.remaining.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
	return false
}

// RetryPolicy bounds automatic execution attempts. Zero MaxAttempts disables
// retries while still reporting the initial attempt.
type RetryPolicy struct {
	MaxAttempts uint32
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      float64
	Random      func() float64
	Sleep       func(context.Context, time.Duration) error
	Now         func() time.Time
	Budget      *RetryBudget
}

// ReliabilityInfo is safe response metadata. It deliberately omits raw
// idempotency keys, fingerprints, lease tokens, and principal scope.
type ReliabilityInfo struct {
	Attempts     uint32 `json:"attempts"`
	Deduplicated bool   `json:"deduplicated,omitempty"`
}

// ReliabilityEvent is safe telemetry. It never contains caller keys,
// fingerprints, fencing tokens, principal identities, or stored data.
type ReliabilityEvent struct {
	Operation    protocol.OperationKind
	Group        string
	Attempt      uint32
	Deduplicated bool
	State        IdempotencyState
}

// IdempotencyOptions configures request and named-group protection.
type IdempotencyOptions struct {
	Store         IdempotencyStore
	RequestKey    string
	GroupKeys     map[string]string
	LeaseDuration time.Duration
	Retention     time.Duration
	Now           func() time.Time
	Scope         func(Principal) (string, error)
	Observe       func(ReliabilityEvent)
}

type executionIdempotency struct {
	options   IdempotencyOptions
	retry     RetryPolicy
	principal Principal
	scope     string
	groupKeys map[string]string
}

type executionReliabilityState struct {
	deduplicated atomic.Bool
	attempts     atomic.Uint32
}

func (p *Plan) executeReliably(ctx context.Context, options ExecuteOptions) Outcome {
	if len(options.Idempotency.GroupKeys) != 0 {
		if options.Idempotency.RequestKey != "" {
			return idempotencyFailureOutcome(p.kind, CodeIdempotencyNotAllowed, "request and group idempotency keys are mutually exclusive", nil)
		}
		if err := p.validateGroupIdempotency(ctx, options.Idempotency); err != nil {
			return idempotencyFailureOutcome(p.kind, CodeIdempotencyNotAllowed, "group idempotency is not permitted", err)
		}
	}
	if options.Idempotency.RequestKey != "" {
		return p.executeIdempotentRequest(ctx, options)
	}
	outcome := p.executeAttempts(ctx, options)
	p.observeReliability(options, ReliabilityEvent{Operation: p.kind, Attempt: outcome.Reliability.Attempts})
	return outcome
}

func newExecutionIdempotency(ctx context.Context, executeOptions ExecuteOptions) *executionIdempotency {
	options := executeOptions.Idempotency
	if len(options.GroupKeys) == 0 || options.Store == nil {
		return nil
	}
	principal, present := PrincipalFromContext(ctx)
	if !present {
		return nil
	}
	scope, err := idempotencyScope(options, principal)
	if err != nil {
		return nil
	}
	return &executionIdempotency{options: options, retry: executeOptions.Retry, principal: principal, scope: scope, groupKeys: maps.Clone(options.GroupKeys)}
}

func (p *Plan) validateGroupIdempotency(ctx context.Context, options IdempotencyOptions) error {
	if p.kind != protocol.Mutation || p.atomicity != protocol.GroupAtomicity || options.Store == nil {
		return errors.New("group keys require a group-atomic mutation and an idempotency store")
	}
	principal, present := PrincipalFromContext(ctx)
	if !present {
		return errors.New("group keys require an authenticated principal")
	}
	if _, err := idempotencyScope(options, principal); err != nil {
		return err
	}
	groups := make(map[string]planNode, len(p.nodes))
	for _, node := range p.nodes {
		if node.kind == protocol.AtomicSelection {
			groups[node.name] = node
		}
	}
	for group, key := range options.GroupKeys {
		node, exists := groups[group]
		if !exists || key == "" {
			return fmt.Errorf("invalid idempotency key for group %q", group)
		}
		if !nodesPermitIdempotency(node.children) {
			return fmt.Errorf("group %q reaches a non-idempotent handler", group)
		}
	}
	return nil
}

func nodesPermitIdempotency(nodes []planNode) bool {
	allowed := true
	walkPlanNodes(nodes, func(node planNode) {
		if !node.hasDefinition {
			return
		}
		policy := node.definition.descriptor.Metadata.Idempotency
		if policy != IdempotencyIdempotent && policy != IdempotencyConditional {
			allowed = false
		}
	})
	return allowed
}

func (p *Plan) executeIdempotentGroup(ctx context.Context, node planNode, scope executionScope, key string) nodeResult {
	idempotency := scope.idempotency
	fingerprint, err := p.idempotencyFingerprint(idempotency.principal, node.name)
	if err != nil {
		return idempotencyNodeFailure(CodeInternal, "internal execution error", err)
	}
	request := IdempotencyClaimRequest{
		Scope: idempotency.scope, Operation: p.operationName, Group: node.name, Key: key,
		Fingerprint: fingerprint, Now: idempotencyNow(idempotency.options), LeaseDuration: idempotencyLease(idempotency.options),
	}
	claim, err := idempotency.options.Store.Claim(ctx, request)
	if err != nil {
		code, message := classifyIdempotencyClaim(claim, err)
		return idempotencyNodeFailure(code, message, err)
	}
	if claim.State == IdempotencyCompleted {
		return p.replayIdempotentGroup(ctx, node, scope, claim.Outcome)
	}
	return p.executeClaimedGroup(ctx, node, scope, request, claim.Fence)
}

func (p *Plan) replayIdempotentGroup(ctx context.Context, node planNode, scope executionScope, outcome Outcome) nodeResult {
	if failure := p.authorizeReplayNodes(ctx, node.children); failure != nil {
		return idempotencyNodeFailure(failure.Code, failure.Message, failure.internal)
	}
	scope.reliability.deduplicated.Store(true)
	mergeAnnotations(scope.annotations, outcome.Annotations)
	p.observeReliability(ExecuteOptions{Idempotency: scope.idempotency.options}, ReliabilityEvent{
		Operation: p.kind, Group: node.name, Deduplicated: true, State: IdempotencyCompleted,
	})
	return nodeResult{data: cloneStoredValue(outcome.Data), merge: true, failed: len(outcome.Errors) != 0, errors: slices.Clone(outcome.Errors)}
}

func (p *Plan) executeClaimedGroup(ctx context.Context, node planNode, scope executionScope, request IdempotencyClaimRequest, fence uint64) nodeResult {
	idempotency := scope.idempotency
	result, groupEffect, annotations, attempts := p.executeGroupAttempts(ctx, node, scope)
	scope.effects.recordTransactionState(groupEffect)
	mergeAnnotations(scope.annotations, annotations)
	outcome := Outcome{Data: result.data, Errors: result.errors, Effects: groupEffect, Annotations: annotations}
	err := persistIdempotencyOutcome(ctx, idempotency.options, request, fence, outcome)
	if err != nil {
		recordPersistenceUncertainty(scope.effects, groupEffect)
		return idempotencyNodeFailure(CodeIdempotencyStoreUnavailable, "idempotency result could not be recorded", err)
	}
	p.observeReliability(ExecuteOptions{Idempotency: idempotency.options}, ReliabilityEvent{
		Operation: p.kind, Group: node.name, Attempt: attempts, State: IdempotencyRunning,
	})
	return nodeResult{data: result.data, merge: true, failed: result.failed, errors: result.errors}
}

func (p *Plan) executeGroupAttempts(ctx context.Context, node planNode, scope executionScope) (sequenceResult, EffectState, []DirectiveAnnotation, uint32) {
	policy := scope.idempotency.retry
	maxAttempts := policy.MaxAttempts
	if maxAttempts == 0 || !nodesRetrySafe(node.children, true) {
		maxAttempts = 1
	}
	for attempt := uint32(1); attempt <= maxAttempts; attempt++ {
		if !policy.Budget.claim() {
			outcome := retryBudgetExhaustedOutcome(protocol.Mutation, attempt-1)
			return sequenceResult{data: outcome.Data, errors: outcome.Errors, failed: true}, outcome.Effects, nil, attempt - 1
		}
		scope.reliability.attempts.Add(1)
		groupScope := scope
		groupScope.effects = &effectRecorder{}
		groupScope.annotations = &directiveAnnotationRecorder{}
		retry := startRetryTelemetry(scope.telemetry, maxAttempts, attempt)
		result := p.executeRootTransaction(ctx, groupScope, node.name, node.children)
		effect := groupScope.effects.state(protocol.Mutation, result.failed)
		annotations := groupScope.annotations.annotations()
		outcome := Outcome{Data: result.data, Errors: result.errors, Effects: effect, Annotations: annotations}
		retry.finish(outcome)
		if !retryableOutcome(outcome) || attempt == maxAttempts {
			return result, effect, annotations, attempt
		}
		if err := sleepBeforeRetry(ctx, policy, attempt, outcomeRetryAfter(outcome)); err != nil {
			return result, effect, annotations, attempt
		}
	}
	panic("unreachable")
}

func persistIdempotencyOutcome(ctx context.Context, options IdempotencyOptions, request IdempotencyClaimRequest, fence uint64, outcome Outcome) error {
	if outcome.Effects == EffectIndeterminate {
		return options.Store.MarkIndeterminate(context.WithoutCancel(ctx), IdempotencyIndeterminateUpdate{
			Scope: request.Scope, Operation: request.Operation, Group: request.Group, Key: request.Key,
			Fingerprint: request.Fingerprint, Fence: fence,
		})
	}
	return options.Store.Complete(context.WithoutCancel(ctx), IdempotencyCompletion{
		Scope: request.Scope, Operation: request.Operation, Group: request.Group, Key: request.Key,
		Fingerprint: request.Fingerprint, Fence: fence, Outcome: outcome,
		Now: idempotencyNow(options), Retention: idempotencyRetention(options),
	})
}

func mergeAnnotations(recorder *directiveAnnotationRecorder, annotations []DirectiveAnnotation) {
	for index, annotation := range annotations {
		recorder.record(annotation, index)
	}
}

func recordPersistenceUncertainty(effects *effectRecorder, state EffectState) {
	if state == EffectApplied || state == EffectPartiallyApplied || state == EffectIndeterminate {
		effects.recordTransactionState(EffectIndeterminate)
	}
}

func idempotencyNodeFailure(code, message string, cause error) nodeResult {
	return nodeResult{failed: true, errors: []ExecutionError{{Code: code, Message: message, Path: []any{}, internal: cause}}}
}

type retryTelemetry struct {
	execution     *executionTelemetry
	id            string
	attempt       uint32
	restoreParent func()
}

func startRetryTelemetry(execution *executionTelemetry, maxAttempts, attempt uint32) *retryTelemetry {
	if execution == nil || maxAttempts <= 1 {
		return nil
	}
	id := execution.nextID("retry", fmt.Sprint(attempt))
	event := execution.baseEvent(TelemetryRetry, TelemetryStarted)
	event.ID, event.ParentID, event.Attempt = id, execution.operationSpan, attempt
	emitTelemetry(execution.options, event)
	return &retryTelemetry{execution: execution, id: id, attempt: attempt, restoreParent: execution.pushParent(id)}
}

func (r *retryTelemetry) finish(outcome Outcome) {
	if r == nil {
		return
	}
	r.restoreParent()
	stage, result, code := outcomeTelemetry(outcome)
	event := r.execution.baseEvent(TelemetryRetry, stage)
	event.ID, event.ParentID, event.Attempt = r.id, r.execution.operationSpan, r.attempt
	event.Outcome, event.ErrorCode = result, code
	emitTelemetry(r.execution.options, event)
}

func (p *Plan) executeAttempts(ctx context.Context, options ExecuteOptions) Outcome {
	if len(options.Idempotency.GroupKeys) != 0 {
		return p.executeComposed(ctx, options)
	}
	maxAttempts := options.Retry.MaxAttempts
	if maxAttempts == 0 || !p.handlersRetrySafe(options) {
		maxAttempts = 1
	}
	var outcome Outcome
	for attempt := uint32(1); attempt <= maxAttempts; attempt++ {
		if !options.Retry.Budget.claim() {
			return retryBudgetExhaustedOutcome(p.kind, attempt-1)
		}
		retry := startRetryTelemetry(options.telemetry, maxAttempts, attempt)
		outcome = p.executeComposed(ctx, options)
		retry.finish(outcome)
		outcome.Reliability.Attempts = attempt
		if !retryableOutcome(outcome) || attempt == maxAttempts {
			return outcome
		}
		if err := sleepBeforeRetry(ctx, options.Retry, attempt, outcomeRetryAfter(outcome)); err != nil {
			return outcome
		}
	}
	return outcome
}

func (p *Plan) executeIdempotentRequest(ctx context.Context, options ExecuteOptions) Outcome {
	if options.Idempotency.Store == nil || !p.handlersPermitIdempotency() {
		return idempotencyFailureOutcome(p.kind, CodeIdempotencyNotAllowed, "idempotency is not permitted for this operation", nil)
	}
	principal, present := PrincipalFromContext(ctx)
	if !present {
		return idempotencyFailureOutcome(p.kind, CodeIdempotencyNotAllowed, "idempotency requires an authenticated principal scope", nil)
	}
	scope, err := idempotencyScope(options.Idempotency, principal)
	if err != nil {
		return idempotencyFailureOutcome(p.kind, CodeIdempotencyNotAllowed, "idempotency scope is unavailable", err)
	}
	fingerprint, err := p.idempotencyFingerprint(principal, "")
	if err != nil {
		return idempotencyFailureOutcome(p.kind, CodeInternal, "internal execution error", err)
	}
	now := idempotencyNow(options.Idempotency)
	request := IdempotencyClaimRequest{
		Scope: scope, Operation: p.operationName, Key: options.Idempotency.RequestKey,
		Fingerprint: fingerprint, Now: now, LeaseDuration: idempotencyLease(options.Idempotency),
	}
	claim, err := options.Idempotency.Store.Claim(ctx, request)
	if err != nil {
		return p.idempotencyClaimFailure(options, claim, err)
	}
	if claim.State == IdempotencyCompleted {
		if failure := p.authorizeReplay(ctx); failure != nil {
			outcome := idempotencyFailureOutcome(p.kind, failure.Code, failure.Message, failure.internal)
			outcome.Reliability.Deduplicated = true
			p.observeReliability(options, ReliabilityEvent{Operation: p.kind, Deduplicated: true, State: IdempotencyCompleted})
			return outcome
		}
		outcome := cloneOutcome(claim.Outcome)
		outcome.Reliability.Attempts = 0
		outcome.Reliability.Deduplicated = true
		p.observeReliability(options, ReliabilityEvent{Operation: p.kind, Deduplicated: true, State: IdempotencyCompleted})
		return outcome
	}
	outcome := p.executeAttempts(ctx, options)
	if outcome.Effects == EffectIndeterminate {
		err = options.Idempotency.Store.MarkIndeterminate(context.WithoutCancel(ctx), IdempotencyIndeterminateUpdate{
			Scope: scope, Operation: p.operationName, Key: options.Idempotency.RequestKey,
			Fingerprint: fingerprint, Fence: claim.Fence,
		})
	} else {
		err = options.Idempotency.Store.Complete(context.WithoutCancel(ctx), IdempotencyCompletion{
			Scope: scope, Operation: p.operationName, Key: options.Idempotency.RequestKey,
			Fingerprint: fingerprint, Fence: claim.Fence, Outcome: outcome,
			Now: idempotencyNow(options.Idempotency), Retention: idempotencyRetention(options.Idempotency),
		})
	}
	if err != nil {
		return idempotencyPersistenceFailure(p.kind, outcome, err)
	}
	p.observeReliability(options, ReliabilityEvent{Operation: p.kind, Attempt: outcome.Reliability.Attempts, State: claim.State})
	return outcome
}

func (p *Plan) idempotencyClaimFailure(options ExecuteOptions, claim IdempotencyClaim, err error) Outcome {
	code, message := classifyIdempotencyClaim(claim, err)
	outcome := idempotencyFailureOutcome(p.kind, code, message, err)
	outcome.Reliability.Deduplicated = claim.Deduplicated
	p.observeReliability(options, ReliabilityEvent{Operation: p.kind, Deduplicated: claim.Deduplicated, State: claim.State})
	return outcome
}

func classifyIdempotencyClaim(claim IdempotencyClaim, err error) (string, string) {
	if errors.Is(err, ErrIdempotencyConflict) {
		return CodeIdempotencyConflict, "idempotency key was already used for different input"
	}
	if errors.Is(err, ErrIdempotencyIndeterminate) || claim.State == IdempotencyIndeterminate {
		return CodeIdempotencyIndeterminate, "previous execution outcome is indeterminate"
	}
	return CodeIdempotencyStoreUnavailable, "idempotency store is unavailable"
}

func idempotencyFailureOutcome(kind protocol.OperationKind, code, message string, cause error) Outcome {
	effects := EffectNotApplicable
	if kind == protocol.Mutation {
		effects = EffectNone
	}
	return Outcome{Data: map[string]any{}, Errors: []ExecutionError{{
		Code: code, Message: message, Path: []any{}, internal: cause,
	}}, Effects: effects}
}

func idempotencyPersistenceFailure(kind protocol.OperationKind, previous Outcome, cause error) Outcome {
	outcome := idempotencyFailureOutcome(kind, CodeIdempotencyStoreUnavailable, "idempotency result could not be recorded", cause)
	if kind == protocol.Mutation && (previous.Effects == EffectApplied || previous.Effects == EffectPartiallyApplied || previous.Effects == EffectIndeterminate) {
		outcome.Effects = EffectIndeterminate
	}
	outcome.Reliability = previous.Reliability
	return outcome
}

func idempotencyScope(options IdempotencyOptions, principal Principal) (string, error) {
	if options.Scope != nil {
		return options.Scope(principal)
	}
	if principal.Subject == "" && principal.Tenant == "" {
		return "", errors.New("empty principal scope")
	}
	return principal.Tenant + "\x1f" + principal.Subject, nil
}

func idempotencyNow(options IdempotencyOptions) time.Time {
	if options.Now != nil {
		return options.Now()
	}
	return time.Now()
}

func idempotencyLease(options IdempotencyOptions) time.Duration {
	if options.LeaseDuration > 0 {
		return options.LeaseDuration
	}
	return time.Minute
}

func idempotencyRetention(options IdempotencyOptions) time.Duration {
	if options.Retention > 0 {
		return options.Retention
	}
	return 24 * time.Hour
}

type idempotencyFingerprintPayload struct {
	Principal struct {
		Tenant                string `json:"tenant,omitempty"`
		Subject               string `json:"subject,omitempty"`
		AuthorizationRevision string `json:"authorizationRevision,omitempty"`
	} `json:"principal"`
	Operation string                     `json:"operation"`
	Group     string                     `json:"group,omitempty"`
	Kind      protocol.OperationKind     `json:"kind"`
	Atomicity protocol.AtomicityMode     `json:"atomicity"`
	Document  json.RawMessage            `json:"document"`
	Variables map[string]json.RawMessage `json:"variables"`
	Types     []schema.TypeDescriptor    `json:"types"`
	Semantics []idempotencyHandler       `json:"semantics"`
}

type idempotencyHandler struct {
	Name          string                   `json:"name"`
	Owner         schema.TypeID            `json:"owner,omitempty"`
	Member        MemberKind               `json:"member"`
	Input         schema.TypeID            `json:"input,omitempty"`
	Output        schema.TypeID            `json:"output"`
	Effect        Effect                   `json:"effect"`
	Idempotency   IdempotencyPolicy        `json:"idempotency"`
	Transaction   TransactionParticipation `json:"transaction"`
	Authorization string                   `json:"authorization"`
}

func (p *Plan) idempotencyFingerprint(principal Principal, group string) (string, error) {
	variables := make(map[string]json.RawMessage, len(p.variableValues))
	for _, definition := range p.variables {
		raw, present := p.variableValues[definition.Name()]
		if !present {
			continue
		}
		value, err := schema.CoerceInput(p.types, schema.TypeID(definition.Type()), raw, definition.Nullable())
		if err != nil {
			return "", fmt.Errorf("canonicalize variable %q: %w", definition.Name(), err)
		}
		canonical, err := value.MarshalJSON()
		if err != nil {
			return "", fmt.Errorf("marshal variable %q: %w", definition.Name(), err)
		}
		variables[definition.Name()] = canonical
	}
	payload := idempotencyFingerprintPayload{
		Operation: p.operationName, Group: group, Kind: p.kind, Atomicity: p.atomicity,
		Document: append(json.RawMessage(nil), p.document...), Variables: variables, Types: p.types.Descriptors(),
	}
	payload.Principal.Tenant = principal.Tenant
	payload.Principal.Subject = principal.Subject
	payload.Principal.AuthorizationRevision = principal.AuthorizationRevision
	walkPlanNodes(p.nodes, func(node planNode) {
		if !node.hasDefinition {
			return
		}
		descriptor := node.definition.descriptor
		payload.Semantics = append(payload.Semantics, idempotencyHandler{
			Name: descriptor.Name, Owner: descriptor.Owner, Member: descriptor.Member,
			Input: descriptor.Input, Output: descriptor.Output, Effect: descriptor.Metadata.Effect,
			Idempotency: descriptor.Metadata.Idempotency, Transaction: descriptor.Metadata.Transaction,
			Authorization: descriptor.Metadata.AuthorizationPolicy,
		})
	})
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal idempotency fingerprint: %w", err)
	}
	canonical, err := protocol.CanonicalizeHashPayload(protocol.IdempotencyHash, encoded, protocol.DefaultLimits())
	if err != nil {
		return "", fmt.Errorf("canonicalize idempotency fingerprint: %w", err)
	}
	digest, err := protocol.SemanticHash(protocol.IdempotencyHash, canonical)
	if err != nil {
		return "", err
	}
	return digest.Hex, nil
}

func (p *Plan) authorizeReplay(ctx context.Context) *ExecutionError {
	return p.authorizeReplayNodes(ctx, p.nodes)
}

func (p *Plan) authorizeReplayNodes(ctx context.Context, nodes []planNode) *ExecutionError {
	var denied *ExecutionError
	walkPlanNodes(nodes, func(node planNode) {
		if denied != nil || !node.hasDefinition {
			return
		}
		path := make([]any, len(node.responsePath))
		for index := range node.responsePath {
			path[index] = node.responsePath[index]
		}
		denied = p.authorizeNode(ctx, node, nil, path)
	})
	return denied
}

func (p *Plan) observeReliability(options ExecuteOptions, event ReliabilityEvent) {
	observeSafely(options.Idempotency.Observe, event)
}

func retryBudgetExhaustedOutcome(kind protocol.OperationKind, attempts uint32) Outcome {
	effects := EffectNotApplicable
	if kind == protocol.Mutation {
		effects = EffectNone
	}
	return Outcome{
		Data: map[string]any{},
		Errors: []ExecutionError{{
			Code: CodeRetryBudgetExhausted, Message: "retry attempt budget exhausted", Path: []any{}, Retryable: false,
		}},
		Effects: effects, Reliability: ReliabilityInfo{Attempts: attempts},
	}
}

func (p *Plan) handlersRetrySafe(options ExecuteOptions) bool {
	return nodesRetrySafe(p.nodes, options.Idempotency.RequestKey != "" && options.Idempotency.Store != nil)
}

func nodesRetrySafe(nodes []planNode, protected bool) bool {
	safe := true
	walkPlanNodes(nodes, func(node planNode) {
		if !node.hasDefinition {
			return
		}
		if !node.definition.descriptor.Metadata.RetrySafe {
			safe = false
		}
		policy := node.definition.descriptor.Metadata.Idempotency
		switch policy {
		case IdempotencyIdempotent:
		case IdempotencyConditional:
			if !protected {
				safe = false
			}
		case IdempotencyNonIdempotent:
			safe = false
		default:
			safe = false
		}
	})
	return safe
}

func (p *Plan) handlersPermitIdempotency() bool {
	return nodesPermitIdempotency(p.nodes)
}

func walkPlanNodes(nodes []planNode, visit func(planNode)) {
	for _, node := range nodes {
		visit(node)
		walkPlanNodes(node.children, visit)
	}
}

func retryableOutcome(outcome Outcome) bool {
	if len(outcome.Errors) == 0 || outcome.Effects == EffectApplied || outcome.Effects == EffectPartiallyApplied || outcome.Effects == EffectIndeterminate {
		return false
	}
	for _, failure := range outcome.Errors {
		if !failure.Retryable {
			return false
		}
	}
	return true
}

func outcomeRetryAfter(outcome Outcome) time.Duration {
	var retryAfter time.Duration
	for _, failure := range outcome.Errors {
		if failure.Retryable && failure.RetryAfter > retryAfter {
			retryAfter = failure.RetryAfter
		}
	}
	return retryAfter
}

func sleepBeforeRetry(ctx context.Context, policy RetryPolicy, failedAttempt uint32, retryAfter time.Duration) error {
	delay := retryDelay(policy, failedAttempt)
	if retryAfter > delay {
		delay = retryAfter
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	now := time.Now()
	if policy.Now != nil {
		now = policy.Now()
	}
	if deadline, ok := ctx.Deadline(); ok && !now.Add(delay).Before(deadline) {
		return context.DeadlineExceeded
	}
	if policy.Sleep != nil {
		return policy.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func retryDelay(policy RetryPolicy, failedAttempt uint32) time.Duration {
	delay := policy.BaseDelay
	for step := uint32(1); step < failedAttempt; step++ {
		if policy.MaxDelay > 0 && delay >= policy.MaxDelay/2 {
			delay = policy.MaxDelay
			break
		}
		if delay > time.Duration(1<<63-1)/2 {
			delay = time.Duration(1<<63 - 1)
			break
		}
		delay *= 2
	}
	if policy.MaxDelay > 0 && delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	if delay <= 0 || policy.Jitter <= 0 {
		return max(delay, 0)
	}
	random := 0.5
	if policy.Random != nil {
		random = min(max(policy.Random(), 0), 1)
	}
	factor := 1 + ((random*2)-1)*min(policy.Jitter, 1)
	return time.Duration(float64(delay) * factor)
}
