package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// TransactionCapabilities are immutable provider features used during planning.
type TransactionCapabilities struct {
	Savepoints bool
}

// TransactionRequest identifies one transport-independent transaction boundary.
type TransactionRequest struct {
	Operation string
	Group     string
}

// CommitOutcome truthfully distinguishes a confirmed commit, a definite
// non-commit, and an outcome the provider cannot determine.
type CommitOutcome string

const (
	CommitApplied    CommitOutcome = "applied"
	CommitNotApplied CommitOutcome = "not-applied"
	CommitUnknown    CommitOutcome = "unknown"
)

// Savepoint is a nested transaction boundary.
type Savepoint interface {
	Release(context.Context) error
	Rollback(context.Context) error
}

// Transaction is valid only until Commit or Rollback returns. The provider
// owns application state attached to the context returned by Begin.
type Transaction interface {
	Commit(context.Context) (CommitOutcome, error)
	Rollback(context.Context) error
	Savepoint(context.Context, string) (context.Context, Savepoint, error)
}

// TransactionProvider begins application transactions without exposing a
// database-specific handle to the runtime.
type TransactionProvider interface {
	Capabilities() TransactionCapabilities
	Begin(context.Context, TransactionRequest) (context.Context, Transaction, error)
}

type MutationAuditStage string

const (
	MutationAttempted     MutationAuditStage = "attempted"
	MutationCommitted     MutationAuditStage = "committed"
	MutationRolledBack    MutationAuditStage = "rolled-back"
	MutationCompensated   MutationAuditStage = "compensated"
	MutationIndeterminate MutationAuditStage = "indeterminate"
)

type MutationAuditEvent struct {
	Stage     MutationAuditStage
	Operation string
	Group     string
	Code      string
}

type MutationAuditHook func(context.Context, MutationAuditEvent)

type TransactionConfig struct {
	Provider     TransactionProvider
	Audit        MutationAuditHook
	capabilities TransactionCapabilities
}

// ConfigureTransactions installs the provider used by atomic mutation plans.
func (r *Registry) ConfigureTransactions(config TransactionConfig) error {
	if config.Provider == nil {
		return errors.New("transaction provider is required")
	}
	capabilities, err := callDirectiveCallback(func() (TransactionCapabilities, error) {
		return config.Provider.Capabilities(), nil
	}, TransactionCapabilities{}, errors.New("transaction provider capability callback panicked"))
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("runtime registry is frozen")
	}
	config.capabilities = capabilities
	r.transactions = config
	return nil
}

type TransactionHook func(context.Context) error

var ErrNoTransactionContext = errors.New("mutation hook registration requires a transaction context")
var ErrTransactionClosed = errors.New("transaction context is no longer active")

type mutationHooksKey struct{}

type mutationHooks struct {
	mu          sync.Mutex
	active      bool
	outbox      []TransactionHook
	afterCommit []TransactionHook
	external    []TransactionHook
}

// RegisterOutbox registers persistence inside the transaction before commit.
func RegisterOutbox(ctx context.Context, hook TransactionHook) error {
	return registerTransactionHook(ctx, hook, true)
}

// RegisterAfterCommit registers idempotent delivery after confirmed commit.
func RegisterAfterCommit(ctx context.Context, hook TransactionHook) error {
	return registerTransactionHook(ctx, hook, false)
}

// RegisterExternalEffect records a side effect outside the transaction and an
// optional saga compensation. A nil compensation explicitly marks the effect
// as uncoordinated so rollback can never be claimed for it.
func RegisterExternalEffect(ctx context.Context, compensation TransactionHook) error {
	hooks, ok := ctx.Value(mutationHooksKey{}).(*mutationHooks)
	if !ok || hooks == nil {
		return ErrNoTransactionContext
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if !hooks.active {
		return ErrTransactionClosed
	}
	hooks.external = append(hooks.external, compensation)
	return nil
}

func registerTransactionHook(ctx context.Context, hook TransactionHook, outbox bool) error {
	if hook == nil {
		return errors.New("transaction hook is nil")
	}
	hooks, ok := ctx.Value(mutationHooksKey{}).(*mutationHooks)
	if !ok || hooks == nil {
		return ErrNoTransactionContext
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if !hooks.active {
		return ErrTransactionClosed
	}
	if outbox {
		hooks.outbox = append(hooks.outbox, hook)
	} else {
		hooks.afterCommit = append(hooks.afterCommit, hook)
	}
	return nil
}

func (h *mutationHooks) snapshot(outbox bool) []TransactionHook {
	h.mu.Lock()
	defer h.mu.Unlock()
	if outbox {
		return append([]TransactionHook(nil), h.outbox...)
	}
	return append([]TransactionHook(nil), h.afterCommit...)
}

func (h *mutationHooks) externalSnapshot() []TransactionHook {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]TransactionHook(nil), h.external...)
}

func (h *mutationHooks) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = false
}

func runTransactionHooks(ctx context.Context, hooks []TransactionHook) error {
	for _, hook := range hooks {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plan) executeOperationTransaction(ctx context.Context, scope executionScope) sequenceResult {
	return p.executeRootTransaction(ctx, scope, "", p.nodes)
}

func (p *Plan) executeRootTransaction(ctx context.Context, scope executionScope, group string, nodes []planNode) sequenceResult {
	p.auditMutation(ctx, MutationAuditEvent{Stage: MutationAttempted, Operation: p.operationName, Group: group})
	txCtx, tx, err := p.transactions.Provider.Begin(ctx, TransactionRequest{Operation: p.operationName, Group: group})
	if err != nil || txCtx == nil || tx == nil {
		if err == nil {
			err = errors.New("transaction provider returned nil state")
		}
		failure := p.transactionFailure(CodeTransactionBeginFailed, "transaction could not begin", err)
		p.auditMutation(ctx, MutationAuditEvent{Stage: MutationIndeterminate, Operation: p.operationName, Group: group, Code: failure.Code})
		return sequenceResult{data: map[string]any{}, errors: []ExecutionError{failure}, failed: true}
	}
	hooks := &mutationHooks{active: true}
	txCtx = context.WithValue(txCtx, mutationHooksKey{}, hooks)
	scope.transaction = tx
	result := p.executeSequence(txCtx, nodes, scope, nil)
	if !result.failed {
		if err := runTransactionHooks(txCtx, hooks.snapshot(true)); err != nil {
			result.errors = append(result.errors, p.transactionFailure(CodeOutboxPersistFailed, "transactional outbox persistence failed", err))
			result.failed = true
		}
	}
	hooks.close()
	if result.failed || txCtx.Err() != nil {
		return p.rollbackTransaction(ctx, txCtx, tx, hooks, scope, group, result)
	}
	commitContext := context.WithoutCancel(txCtx)
	outcome, commitErr := tx.Commit(commitContext)
	switch outcome {
	case CommitApplied:
		scope.effects.recordTransactionState(EffectApplied)
		deliveryContext := context.WithoutCancel(ctx)
		p.auditMutation(deliveryContext, MutationAuditEvent{Stage: MutationCommitted, Operation: p.operationName, Group: group})
		if err := runTransactionHooks(deliveryContext, hooks.snapshot(false)); err != nil {
			result.errors = append(result.errors, p.transactionFailure(CodeAfterCommitFailed, "after-commit delivery failed", err))
			result.failed = true
		}
		return result
	case CommitNotApplied:
		result.errors = append(result.errors, p.transactionFailure(CodeTransactionCommitFailed, "transaction commit failed", commitErr))
		result.failed = true
		return p.rollbackTransaction(ctx, commitContext, tx, hooks, scope, group, result)
	case CommitUnknown:
		failure := p.transactionFailure(CodeTransactionCommitUnknown, "transaction commit outcome is unknown", commitErr)
		result.data = map[string]any{}
		result.errors = append(result.errors, failure)
		result.failed = true
		scope.effects.recordTransactionState(EffectIndeterminate)
		p.auditMutation(commitContext, MutationAuditEvent{Stage: MutationIndeterminate, Operation: p.operationName, Group: group, Code: failure.Code})
		return result
	default:
		failure := p.transactionFailure(CodeTransactionCommitUnknown, "transaction provider returned an invalid commit outcome", fmt.Errorf("invalid commit outcome %q: %w", outcome, commitErr))
		result.data = map[string]any{}
		result.errors = append(result.errors, failure)
		result.failed = true
		scope.effects.recordTransactionState(EffectIndeterminate)
		p.auditMutation(commitContext, MutationAuditEvent{Stage: MutationIndeterminate, Operation: p.operationName, Group: group, Code: failure.Code})
		return result
	}
}

func (p *Plan) rollbackTransaction(baseCtx, txCtx context.Context, tx Transaction, hooks *mutationHooks, scope executionScope, group string, result sequenceResult) sequenceResult {
	result.data = map[string]any{}
	rollbackContext := context.WithoutCancel(txCtx)
	compensationContext := context.WithoutCancel(baseCtx)
	rollbackConfirmed := true
	if err := tx.Rollback(rollbackContext); err != nil {
		rollbackConfirmed = false
		failure := p.transactionFailure(CodeTransactionRollbackFailed, "transaction rollback failed", err)
		result.errors = append(result.errors, failure)
		result.failed = true
	}
	external := hooks.externalSnapshot()
	compensated := len(external) != 0
	for _, compensation := range external {
		if compensation == nil {
			compensated = false
			result.errors = append(result.errors, p.transactionFailure(CodeExternalEffectUncoordinated, "external effect has no transaction rollback or compensation", nil))
			continue
		}
		if err := compensation(compensationContext); err != nil {
			compensated = false
			result.errors = append(result.errors, p.transactionFailure(CodeCompensationFailed, "external effect compensation failed", err))
		}
	}
	if !rollbackConfirmed || (len(external) != 0 && !compensated) {
		scope.effects.recordTransactionState(EffectIndeterminate)
		code := result.errors[len(result.errors)-1].Code
		p.auditMutation(rollbackContext, MutationAuditEvent{Stage: MutationIndeterminate, Operation: p.operationName, Group: group, Code: code})
		return result
	}
	if compensated {
		scope.effects.recordTransactionState(EffectCompensated)
		p.auditMutation(rollbackContext, MutationAuditEvent{Stage: MutationCompensated, Operation: p.operationName, Group: group})
		return result
	}
	scope.effects.recordTransactionState(EffectRolledBack)
	p.auditMutation(rollbackContext, MutationAuditEvent{Stage: MutationRolledBack, Operation: p.operationName, Group: group})
	return result
}

func (p *Plan) executeAtomic(ctx context.Context, node planNode, scope executionScope) nodeResult {
	if scope.transaction == nil {
		result := p.executeRootTransaction(ctx, scope, node.name, node.children)
		return nodeResult{data: result.data, merge: true, failed: result.failed, errors: result.errors}
	}
	savepointCtx, savepoint, err := scope.transaction.Savepoint(ctx, node.name)
	if err != nil || savepointCtx == nil || savepoint == nil {
		if err == nil {
			err = errors.New("transaction provider returned nil savepoint state")
		}
		failure := p.transactionFailure(CodeSavepointBeginFailed, "transaction savepoint could not begin", err)
		return nodeResult{failed: true, errors: []ExecutionError{failure}}
	}
	if hooks, ok := ctx.Value(mutationHooksKey{}).(*mutationHooks); ok {
		savepointCtx = context.WithValue(savepointCtx, mutationHooksKey{}, hooks)
	}
	children := p.executeSequence(savepointCtx, node.children, scope, nil)
	cleanupCtx := context.WithoutCancel(savepointCtx)
	if children.failed || savepointCtx.Err() != nil {
		children.data = map[string]any{}
		if err := savepoint.Rollback(cleanupCtx); err != nil {
			children.errors = append(children.errors, p.transactionFailure(CodeSavepointRollbackFailed, "transaction savepoint rollback failed", err))
		}
		return nodeResult{data: children.data, merge: true, failed: true, errors: children.errors}
	}
	if err := savepoint.Release(cleanupCtx); err != nil {
		failure := p.transactionFailure(CodeSavepointReleaseFailed, "transaction savepoint release failed", err)
		return nodeResult{failed: true, errors: []ExecutionError{failure}}
	}
	return nodeResult{data: children.data, merge: true, errors: children.errors}
}

func (p *Plan) transactionFailure(code, message string, cause error) ExecutionError {
	if cause == nil {
		cause = errors.New(message)
	}
	return ExecutionError{Code: code, Message: message, Path: []any{}, Source: p.operationSource, internal: cause}
}

func (p *Plan) auditMutation(ctx context.Context, event MutationAuditEvent) {
	if p.transactions.Audit == nil {
		return
	}
	defer func() { _ = recover() }()
	p.transactions.Audit(context.WithoutCancel(ctx), event)
}
