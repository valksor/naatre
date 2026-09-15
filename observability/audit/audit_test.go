package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/observability/audit"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type transactionStateKey struct{}

type transactionProvider struct {
	commit   func(context.Context) (runtime.CommitOutcome, error)
	rollback func(context.Context) error
}

func (transactionProvider) Capabilities() runtime.TransactionCapabilities {
	return runtime.TransactionCapabilities{}
}

func (p transactionProvider) Begin(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
	return context.WithValue(ctx, transactionStateKey{}, "active"), transaction(p), nil
}

type transaction struct {
	commit   func(context.Context) (runtime.CommitOutcome, error)
	rollback func(context.Context) error
}

func (t transaction) Commit(ctx context.Context) (runtime.CommitOutcome, error) {
	return t.commit(ctx)
}

func (t transaction) Rollback(ctx context.Context) error { return t.rollback(ctx) }

func (transaction) Savepoint(context.Context, string) (context.Context, runtime.Savepoint, error) {
	return nil, nil, errors.New("savepoints are unsupported")
}

func TestAdmitPersistsBeforeCommitUsingTransactionContext(t *testing.T) {
	t.Parallel()
	var order []string
	recorder, err := audit.New(audit.WriterFunc(func(ctx context.Context, event runtime.MutationAuditEvent) error {
		if ctx.Value(transactionStateKey{}) != "active" {
			t.Fatal("writer did not receive the transaction-bound context")
		}
		if event.Operation != "CreateOrder" || event.Stage != runtime.MutationAttempted {
			t.Fatalf("audit event = %#v", event)
		}
		order = append(order, "audit")
		return nil
	}), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	plan := mutationPlan(t, transactionProvider{
		commit: func(context.Context) (runtime.CommitOutcome, error) {
			order = append(order, "commit")
			return runtime.CommitApplied, nil
		},
		rollback: func(context.Context) error {
			order = append(order, "rollback")
			return nil
		},
	}, func(ctx context.Context) error {
		return recorder.Admit(ctx, runtime.MutationAuditEvent{
			Stage: runtime.MutationAttempted, Operation: "CreateOrder", RequestID: "request-ref",
		})
	})
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || outcome.Effects != runtime.EffectApplied {
		t.Fatalf("outcome = %#v", outcome)
	}
	if strings.Join(order, ",") != "audit,commit" {
		t.Fatalf("order = %v, want audit before commit", order)
	}
}

func TestPersistenceFailureIsSafeAndRollsBack(t *testing.T) {
	t.Parallel()
	recorder, err := audit.New(audit.WriterFunc(func(context.Context, runtime.MutationAuditEvent) error {
		panic("postgres password=top-secret at internal/table")
	}), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rollbacks := 0
	plan := mutationPlan(t, transactionProvider{
		commit: func(context.Context) (runtime.CommitOutcome, error) {
			t.Fatal("commit ran after durable audit persistence failed")
			return runtime.CommitUnknown, nil
		},
		rollback: func(context.Context) error {
			rollbacks++
			return nil
		},
	}, func(ctx context.Context) error {
		return recorder.Admit(ctx, runtime.MutationAuditEvent{Stage: runtime.MutationAttempted, Operation: "M"})
	})
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeOutboxPersistFailed || outcome.Effects != runtime.EffectRolledBack || rollbacks != 1 {
		t.Fatalf("outcome = %#v, rollbacks = %d", outcome, rollbacks)
	}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibited := range []string{"top-secret", "password", "internal/table", audit.CodePersistenceFailure} {
		if strings.Contains(string(encoded), prohibited) {
			t.Fatalf("public outcome exposed %q: %s", prohibited, encoded)
		}
	}
}

func TestCancellationSkipsWriterAndCannotCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	writerCalls := 0
	recorder, err := audit.New(audit.WriterFunc(func(context.Context, runtime.MutationAuditEvent) error {
		writerCalls++
		return nil
	}), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rollbacks := 0
	plan := mutationPlan(t, transactionProvider{
		commit: func(context.Context) (runtime.CommitOutcome, error) {
			t.Fatal("commit ran after cancellation")
			return runtime.CommitUnknown, nil
		},
		rollback: func(cleanup context.Context) error {
			rollbacks++
			if cleanup.Err() != nil {
				t.Fatalf("rollback context remained cancelled: %v", cleanup.Err())
			}
			return nil
		},
	}, func(transactionContext context.Context) error {
		if err := recorder.Admit(transactionContext, runtime.MutationAuditEvent{Stage: runtime.MutationAttempted, Operation: "M"}); err != nil {
			return err
		}
		cancel()
		return nil
	})
	outcome := plan.Execute(ctx)
	if writerCalls != 0 || rollbacks != 1 || outcome.Effects != runtime.EffectRolledBack {
		t.Fatalf("outcome = %#v, writer calls = %d, rollbacks = %d", outcome, writerCalls, rollbacks)
	}
}

func TestAdmissionAndHookBoundaries(t *testing.T) {
	t.Parallel()
	recorder, err := audit.New(audit.WriterFunc(func(context.Context, runtime.MutationAuditEvent) error {
		return errors.New("credential=hidden")
	}), audit.Options{MaxBatch: 1, MaxFieldBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		err  func() error
		code string
	}{
		{name: "outside transaction", err: func() error {
			return recorder.Admit(context.Background(), runtime.MutationAuditEvent{Stage: runtime.MutationAttempted, Operation: "M"})
		}, code: audit.CodeTransactionNeeded},
		{name: "future fact", err: func() error {
			return recorder.Admit(context.Background(), runtime.MutationAuditEvent{Stage: runtime.MutationCommitted, Operation: "M"})
		}, code: audit.CodeInvalidEvent},
		{name: "field limit", err: func() error {
			return recorder.Admit(context.Background(), runtime.MutationAuditEvent{Stage: runtime.MutationAttempted, Operation: "large"})
		}, code: audit.CodeResourceExhausted},
		{name: "batch limit", err: func() error {
			event := runtime.MutationAuditEvent{Stage: runtime.MutationAttempted, Operation: "M"}
			return recorder.AdmitBatch(context.Background(), []runtime.MutationAuditEvent{event, event})
		}, code: audit.CodeResourceExhausted},
		{name: "invalid hook stage", err: func() error {
			return recorder.Hook()(context.Background(), runtime.MutationAuditEvent{Stage: "secret-stage", Operation: "M"})
		}, code: audit.CodeInvalidEvent},
		{name: "safe persistence failure", err: func() error {
			return recorder.Hook()(context.Background(), runtime.MutationAuditEvent{Stage: runtime.MutationCommitted, Operation: "M"})
		}, code: audit.CodePersistenceFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.err()
			if audit.CodeOf(err) != test.code {
				t.Fatalf("error = %v, code = %q, want %q", err, audit.CodeOf(err), test.code)
			}
			if strings.Contains(err.Error(), "hidden") || strings.Contains(err.Error(), "secret-stage") {
				t.Fatalf("error exposed protected detail: %v", err)
			}
		})
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := audit.New(nil, audit.Options{}); audit.CodeOf(err) != audit.CodeInvalidConfig {
		t.Fatalf("nil writer error = %v", err)
	}
	if _, err := audit.New(audit.WriterFunc(func(context.Context, runtime.MutationAuditEvent) error { return nil }), audit.Options{MaxBatch: -1}); audit.CodeOf(err) != audit.CodeInvalidConfig {
		t.Fatalf("negative limit error = %v", err)
	}
}

func mutationPlan(t *testing.T, provider runtime.TransactionProvider, handler func(context.Context) error) *runtime.Plan {
	t.Helper()
	types, err := schema.NewCatalog().Freeze()
	if err != nil {
		t.Fatal(err)
	}
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String),
		Metadata: runtime.Metadata{
			Effect: runtime.WriteEffect, Deterministic: true, RetrySafe: true,
			ThreadSafety: runtime.ThreadSafe, Batching: runtime.BatchIneligible,
			Transaction: runtime.TransactionRequired, Idempotency: runtime.IdempotencyIdempotent,
			AuthorizationPolicy: "test",
		},
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		if err := handler(ctx); err != nil {
			return "", err
		}
		return "written", nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$call":{"name":"write"}}]}]}}`), protocol.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
