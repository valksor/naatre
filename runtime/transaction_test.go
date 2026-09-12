package runtime_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type transactionContextKey struct{}

type fakeTransactionProvider struct {
	capabilities runtime.TransactionCapabilities
	begin        func(context.Context, runtime.TransactionRequest) (context.Context, runtime.Transaction, error)
}

type capabilityTransactionProvider struct {
	capabilities *runtime.TransactionCapabilities
	panicOnRead  bool
	mutateOnRead bool
}

func (p *capabilityTransactionProvider) Capabilities() runtime.TransactionCapabilities {
	if p.panicOnRead {
		panic("capability failure")
	}
	capabilities := *p.capabilities
	if p.mutateOnRead {
		p.capabilities.Savepoints = false
	}
	return capabilities
}

func (*capabilityTransactionProvider) Begin(context.Context, runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
	return nil, nil, errors.New("unused")
}

func (p fakeTransactionProvider) Capabilities() runtime.TransactionCapabilities {
	return p.capabilities
}

func (p fakeTransactionProvider) Begin(ctx context.Context, request runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
	return p.begin(ctx, request)
}

type fakeTransaction struct {
	commit    func(context.Context) (runtime.CommitOutcome, error)
	rollback  func(context.Context) error
	savepoint func(context.Context, string) (context.Context, runtime.Savepoint, error)
}

type fakeSavepoint struct {
	release  func(context.Context) error
	rollback func(context.Context) error
}

func (s fakeSavepoint) Release(ctx context.Context) error  { return s.release(ctx) }
func (s fakeSavepoint) Rollback(ctx context.Context) error { return s.rollback(ctx) }

func (t fakeTransaction) Commit(ctx context.Context) (runtime.CommitOutcome, error) {
	return t.commit(ctx)
}

func (t fakeTransaction) Rollback(ctx context.Context) error { return t.rollback(ctx) }

func (t fakeTransaction) Savepoint(ctx context.Context, name string) (context.Context, runtime.Savepoint, error) {
	return t.savepoint(ctx, name)
}

func TestConfigureTransactionsContainsCapabilityPanic(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	capabilities := runtime.TransactionCapabilities{}
	provider := &capabilityTransactionProvider{capabilities: &capabilities, panicOnRead: true}
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider}); err == nil {
		t.Fatal("ConfigureTransactions accepted a provider whose capability callback panicked")
	}
	provider.panicOnRead = false
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider}); err != nil {
		t.Fatalf("ConfigureTransactions after contained panic: %v", err)
	}
}

func TestConfigureTransactionsSnapshotsProviderCapabilities(t *testing.T) {
	t.Parallel()
	capabilities := runtime.TransactionCapabilities{Savepoints: true}
	provider := &capabilityTransactionProvider{capabilities: &capabilities, mutateOnRead: true}
	transactionPlan(t, provider, nil,
		`{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$atomic":{"name":"nested","select":[]}}]}]}`,
		func(context.Context) (string, error) { return "", nil })
	if capabilities.Savepoints {
		t.Fatal("test provider did not mutate its advertised capabilities")
	}
}

func TestPrepareRejectsInvalidAtomicityBeforeExecution(t *testing.T) {
	t.Parallel()
	snapshot, calls := validationRegistry(t)
	tests := []struct {
		name  string
		input string
		codes []string
	}{
		{
			name:  "query transaction",
			input: `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","atomicity":"operation","select":[{"$call":{"name":"text"}}]}]}}`,
			codes: []string{"ATOMICITY_NOT_ALLOWED"},
		},
		{
			name:  "operation without provider",
			input: `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$call":{"name":"write"}}]}]}}`,
			codes: []string{"TRANSACTION_PROVIDER_REQUIRED", "TRANSACTION_PARTICIPATION"},
		},
		{
			name:  "group requires wrappers",
			input: `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$call":{"name":"write"}}]}]}}`,
			codes: []string{"TRANSACTION_PROVIDER_REQUIRED", "ATOMIC_GROUP_REQUIRED", "TRANSACTION_PARTICIPATION"},
		},
		{
			name:  "wrapper requires atomicity",
			input: `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$atomic":{"name":"account","select":[{"$call":{"name":"write"}}]}}]}]}}`,
			codes: []string{"ATOMIC_GROUP_NOT_ALLOWED"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := calls.Load()
			got := validationCodes(t, prepareError(snapshot, decodeRuntimeRequest(t, tt.input)))
			if !slices.Equal(got, tt.codes) {
				t.Fatalf("codes = %v, want %v", got, tt.codes)
			}
			if calls.Load() != before {
				t.Fatal("handler ran during transaction preflight")
			}
		})
	}
}

func TestPrepareRejectsDuplicateAtomicGroupNames(t *testing.T) {
	t.Parallel()
	snapshot, calls := validationRegistry(t)
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"account","select":[]}},{"$atomic":{"name":"account","select":[]}}]}]}}`)
	want := []string{"TRANSACTION_PROVIDER_REQUIRED", "DUPLICATE_ATOMIC_GROUP"}
	if got := validationCodes(t, prepareError(snapshot, request)); !slices.Equal(got, want) {
		t.Fatalf("codes = %v, want %v", got, want)
	}
	if calls.Load() != 0 {
		t.Fatal("handler ran during duplicate-group preflight")
	}
}

func TestOperationAtomicityRunsHooksAndPublishesOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	var events []string
	provider := fakeTransactionProvider{begin: func(ctx context.Context, request runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		events = append(events, "begin:"+request.Operation)
		tx := fakeTransaction{
			commit: func(context.Context) (runtime.CommitOutcome, error) {
				events = append(events, "commit")
				return runtime.CommitApplied, nil
			},
			rollback: func(context.Context) error {
				events = append(events, "rollback")
				return nil
			},
		}
		return context.WithValue(ctx, transactionContextKey{}, "transaction-state"), tx, nil
	}}
	audit := func(_ context.Context, event runtime.MutationAuditEvent) {
		events = append(events, "audit:"+string(event.Stage))
	}
	plan := transactionPlan(t, provider, audit, `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$call":{"name":"write"}}]}]}`, func(ctx context.Context) (string, error) {
		events = append(events, "handler")
		if ctx.Value(transactionContextKey{}) != "transaction-state" {
			return "", errors.New("transaction context state is missing")
		}
		if err := runtime.RegisterOutbox(ctx, func(context.Context) error {
			events = append(events, "outbox")
			return nil
		}); err != nil {
			return "", err
		}
		if err := runtime.RegisterAfterCommit(ctx, func(deliveryCtx context.Context) error {
			if deliveryCtx.Value(transactionContextKey{}) != nil {
				return errors.New("transaction state escaped into after-commit delivery")
			}
			events = append(events, "after-commit")
			return nil
		}); err != nil {
			return "", err
		}
		return "written", nil
	})
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || outcome.Effects != runtime.EffectApplied {
		t.Fatalf("outcome = %#v", outcome)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"write": "written"})
	want := []string{"audit:attempted", "begin:M", "handler", "outbox", "commit", "audit:committed", "after-commit"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestTransactionContextRejectsLateHookRegistration(t *testing.T) {
	t.Parallel()
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeTransaction{
			commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	var transactionCtx context.Context
	plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(ctx context.Context) (string, error) {
		transactionCtx = ctx
		return "written", nil
	})
	if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	tests := []struct {
		name     string
		register func() error
	}{
		{name: "outbox", register: func() error {
			return runtime.RegisterOutbox(transactionCtx, func(context.Context) error { return nil })
		}},
		{name: "after commit", register: func() error {
			return runtime.RegisterAfterCommit(transactionCtx, func(context.Context) error { return nil })
		}},
		{name: "external effect", register: func() error { return runtime.RegisterExternalEffect(transactionCtx, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.register(); !errors.Is(err, runtime.ErrTransactionClosed) {
				t.Fatalf("late registration error = %v, want ErrTransactionClosed", err)
			}
		})
	}
}

func TestAfterCommitDeliverySupportsIdempotentDuplicateSuppression(t *testing.T) {
	t.Parallel()
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeTransaction{
			commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	attempts, deliveries := 0, 0
	delivered := make(map[string]bool)
	plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(ctx context.Context) (string, error) {
		const eventID = "order-42:confirmed"
		if err := runtime.RegisterAfterCommit(ctx, func(context.Context) error {
			attempts++
			if delivered[eventID] {
				return nil
			}
			delivered[eventID] = true
			deliveries++
			return nil
		}); err != nil {
			return "", err
		}
		return "written", nil
	})
	for run := 0; run < 2; run++ {
		if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 {
			t.Fatalf("Execute run %d errors = %#v", run+1, outcome.Errors)
		}
	}
	if attempts != 2 || deliveries != 1 {
		t.Fatalf("delivery attempts = %d, applied deliveries = %d; want 2 attempts and 1 application", attempts, deliveries)
	}
}

func TestOperationAtomicityRollsBackAndWithholdsTentativeOutput(t *testing.T) {
	t.Parallel()
	var events []string
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeTransaction{
			commit: func(context.Context) (runtime.CommitOutcome, error) {
				events = append(events, "commit")
				return runtime.CommitApplied, nil
			},
			rollback: func(context.Context) error {
				events = append(events, "rollback")
				return nil
			},
		}, nil
	}}
	plan := transactionPlan(t, provider, func(_ context.Context, event runtime.MutationAuditEvent) {
		events = append(events, "audit:"+string(event.Stage))
	}, `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$call":{"name":"write"}}]}]}`, func(context.Context) (string, error) {
		events = append(events, "handler")
		return "tentative", errors.New("write failed")
	})
	outcome := plan.Execute(context.Background())
	if len(outcome.Data) != 0 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeHandlerFailed {
		t.Fatalf("outcome = %#v", outcome)
	}
	if outcome.Effects != runtime.EffectRolledBack {
		t.Fatalf("effects = %q, want %q", outcome.Effects, runtime.EffectRolledBack)
	}
	want := []string{"audit:attempted", "handler", "rollback", "audit:rolled-back"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestOperationAtomicityReportsCommitAndRollbackFaultsTruthfully(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		commit        runtime.CommitOutcome
		commitErr     error
		rollbackErr   error
		wantCodes     []string
		wantEffect    runtime.EffectState
		wantRollbacks int
	}{
		{
			name: "definite commit failure", commit: runtime.CommitNotApplied, commitErr: errors.New("constraint at commit"),
			wantCodes: []string{"TRANSACTION_COMMIT_FAILED"}, wantEffect: runtime.EffectRolledBack, wantRollbacks: 1,
		},
		{
			name: "unknown commit outcome", commit: runtime.CommitUnknown, commitErr: errors.New("connection lost"),
			wantCodes: []string{"TRANSACTION_COMMIT_UNKNOWN"}, wantEffect: runtime.EffectIndeterminate,
		},
		{
			name: "rollback failure after definite commit failure", commit: runtime.CommitNotApplied, commitErr: errors.New("commit rejected"), rollbackErr: errors.New("rollback cleanup failed"),
			wantCodes: []string{"TRANSACTION_COMMIT_FAILED", "TRANSACTION_ROLLBACK_FAILED"}, wantEffect: runtime.EffectIndeterminate, wantRollbacks: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rollbacks := 0
			provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
				return ctx, fakeTransaction{
					commit: func(context.Context) (runtime.CommitOutcome, error) { return tt.commit, tt.commitErr },
					rollback: func(context.Context) error {
						rollbacks++
						return tt.rollbackErr
					},
				}, nil
			}}
			plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(context.Context) (string, error) { return "tentative", nil })
			outcome := plan.Execute(context.Background())
			assertExecutionCodes(t, outcome, tt.wantCodes)
			if len(outcome.Data) != 0 || outcome.Effects != tt.wantEffect || rollbacks != tt.wantRollbacks {
				t.Fatalf("outcome = %#v, rollbacks = %d", outcome, rollbacks)
			}
		})
	}
}

func TestOperationAtomicityTreatsHookFailuresAtTheirTrueBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		register   func(context.Context) error
		wantCode   string
		wantEffect runtime.EffectState
		wantData   bool
		rollback   bool
	}{
		{
			name: "outbox persistence", wantCode: "OUTBOX_PERSIST_FAILED", wantEffect: runtime.EffectRolledBack, rollback: true,
			register: func(ctx context.Context) error {
				return runtime.RegisterOutbox(ctx, func(context.Context) error { return errors.New("outbox unavailable") })
			},
		},
		{
			name: "after commit delivery", wantCode: "AFTER_COMMIT_FAILED", wantEffect: runtime.EffectApplied, wantData: true,
			register: func(ctx context.Context) error {
				return runtime.RegisterAfterCommit(ctx, func(context.Context) error { return errors.New("delivery unavailable") })
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rollbacks := 0
			provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
				return ctx, fakeTransaction{
					commit: func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
					rollback: func(context.Context) error {
						rollbacks++
						return nil
					},
				}, nil
			}}
			plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(ctx context.Context) (string, error) {
				return "written", tt.register(ctx)
			})
			outcome := plan.Execute(context.Background())
			assertExecutionCodes(t, outcome, []string{tt.wantCode})
			if (len(outcome.Data) != 0) != tt.wantData || outcome.Effects != tt.wantEffect || (rollbacks != 0) != tt.rollback {
				t.Fatalf("outcome = %#v, rollbacks = %d", outcome, rollbacks)
			}
		})
	}
}

func TestOperationAtomicityUsesUncancelledCommitAndRollbackCleanup(t *testing.T) {
	t.Parallel()
	t.Run("cancelled before commit rolls back", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		commits, rollbacks := 0, 0
		provider := fakeTransactionProvider{begin: func(beginCtx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
			return beginCtx, fakeTransaction{
				commit: func(context.Context) (runtime.CommitOutcome, error) {
					commits++
					return runtime.CommitApplied, nil
				},
				rollback: func(cleanup context.Context) error {
					rollbacks++
					if cleanup.Err() != nil {
						return errors.New("rollback context is cancelled")
					}
					return nil
				},
			}, nil
		}}
		plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(context.Context) (string, error) {
			cancel()
			return "tentative", nil
		})
		outcome := plan.Execute(ctx)
		if commits != 0 || rollbacks != 1 || outcome.Effects != runtime.EffectRolledBack {
			t.Fatalf("outcome = %#v, commits = %d, rollbacks = %d", outcome, commits, rollbacks)
		}
	})
	t.Run("cancellation during commit cannot rewrite confirmed outcome", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		provider := fakeTransactionProvider{begin: func(beginCtx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
			return beginCtx, fakeTransaction{
				commit: func(commitCtx context.Context) (runtime.CommitOutcome, error) {
					cancel()
					if commitCtx.Err() != nil {
						return runtime.CommitUnknown, commitCtx.Err()
					}
					return runtime.CommitApplied, nil
				},
				rollback: func(context.Context) error { return nil },
			}, nil
		}}
		plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(context.Context) (string, error) { return "written", nil })
		outcome := plan.Execute(ctx)
		if len(outcome.Errors) != 0 || outcome.Effects != runtime.EffectApplied || len(outcome.Data) != 1 {
			t.Fatalf("outcome = %#v", outcome)
		}
	})
}

func TestGroupAtomicityCommitsNamedRootGroupsIndependently(t *testing.T) {
	t.Parallel()
	var events []string
	provider := fakeTransactionProvider{begin: func(ctx context.Context, request runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		events = append(events, "begin:"+request.Group)
		return ctx, fakeTransaction{
			commit: func(context.Context) (runtime.CommitOutcome, error) {
				events = append(events, "commit:"+request.Group)
				return runtime.CommitApplied, nil
			},
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	plan := transactionPlan(t, provider, nil, `{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"first","select":[{"$call":{"name":"write","as":"a"}}]}},{"$atomic":{"name":"second","select":[{"$call":{"name":"write","as":"b"}}]}}]}]}`, func(context.Context) (string, error) {
		events = append(events, "handler")
		return "written", nil
	})
	description := plan.Description()
	if description.Atomicity != protocol.GroupAtomicity || len(description.Result.Fields) != 2 {
		t.Fatalf("description = %#v", description)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || outcome.Effects != runtime.EffectApplied {
		t.Fatalf("outcome = %#v", outcome)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"a": "written", "b": "written"})
	want := []string{"begin:first", "handler", "commit:first", "begin:second", "handler", "commit:second"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestNestedAtomicGroupUsesProviderSavepoint(t *testing.T) {
	t.Parallel()
	var events []string
	provider := fakeTransactionProvider{
		capabilities: runtime.TransactionCapabilities{Savepoints: true},
		begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
			events = append(events, "begin")
			return context.WithValue(ctx, transactionContextKey{}, "transaction"), fakeTransaction{
				commit: func(context.Context) (runtime.CommitOutcome, error) {
					events = append(events, "commit")
					return runtime.CommitApplied, nil
				},
				rollback: func(context.Context) error { return nil },
				savepoint: func(savepointCtx context.Context, name string) (context.Context, runtime.Savepoint, error) {
					events = append(events, "savepoint:"+name)
					return context.WithValue(savepointCtx, transactionContextKey{}, "savepoint"), fakeSavepoint{
						release: func(context.Context) error {
							events = append(events, "release:"+name)
							return nil
						},
						rollback: func(context.Context) error { return nil },
					}, nil
				},
			}, nil
		},
	}
	plan := transactionPlan(t, provider, nil, `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$atomic":{"name":"nested","select":[{"$call":{"name":"write"}}]}}]}]}`, func(ctx context.Context) (string, error) {
		events = append(events, "handler")
		if ctx.Value(transactionContextKey{}) != "savepoint" {
			return "", errors.New("savepoint context state is missing")
		}
		return "written", nil
	})
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || outcome.Effects != runtime.EffectApplied {
		t.Fatalf("outcome = %#v", outcome)
	}
	want := []string{"begin", "savepoint:nested", "handler", "release:nested", "commit"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestPrepareRejectsNestedAtomicGroupsWithoutSavepoints(t *testing.T) {
	t.Parallel()
	provider := fakeTransactionProvider{begin: func(context.Context, runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		t.Fatal("transaction began despite unsupported nested group")
		return nil, nil, nil
	}}
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider}); err != nil {
		t.Fatalf("ConfigureTransactions: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$atomic":{"name":"nested","select":[]}}]}]}}`)
	if got := validationCodes(t, prepareError(snapshot, request)); !slices.Equal(got, []string{"SAVEPOINT_UNSUPPORTED"}) {
		t.Fatalf("codes = %v", got)
	}
}

func TestExternalEffectsUseCompensationWithoutClaimingRollback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		compensate    runtime.TransactionHook
		wantCodes     []string
		wantEffect    runtime.EffectState
		wantAudit     runtime.MutationAuditStage
		wantAuditCode string
	}{
		{name: "compensated", compensate: func(context.Context) error { return nil }, wantCodes: []string{runtime.CodeHandlerFailed}, wantEffect: runtime.EffectCompensated, wantAudit: runtime.MutationCompensated},
		{name: "uncoordinated", wantCodes: []string{runtime.CodeExternalEffectUncoordinated, runtime.CodeHandlerFailed}, wantEffect: runtime.EffectIndeterminate, wantAudit: runtime.MutationIndeterminate, wantAuditCode: runtime.CodeExternalEffectUncoordinated},
		{name: "compensation failed", compensate: func(context.Context) error { return errors.New("remote compensation failed") }, wantCodes: []string{runtime.CodeCompensationFailed, runtime.CodeHandlerFailed}, wantEffect: runtime.EffectIndeterminate, wantAudit: runtime.MutationIndeterminate, wantAuditCode: runtime.CodeCompensationFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var audit []runtime.MutationAuditEvent
			provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
				return ctx, fakeTransaction{
					commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
					rollback: func(context.Context) error { return nil },
				}, nil
			}}
			plan := transactionPlan(t, provider, func(_ context.Context, event runtime.MutationAuditEvent) {
				audit = append(audit, event)
			}, operationAtomicDocument(), func(ctx context.Context) (string, error) {
				if err := runtime.RegisterExternalEffect(ctx, tt.compensate); err != nil {
					return "", err
				}
				return "tentative", errors.New("later mutation failed")
			})
			outcome := plan.Execute(context.Background())
			assertExecutionCodes(t, outcome, tt.wantCodes)
			if outcome.Effects != tt.wantEffect || len(outcome.Data) != 0 {
				t.Fatalf("outcome = %#v", outcome)
			}
			if len(audit) != 2 || audit[0].Stage != runtime.MutationAttempted || audit[1].Stage != tt.wantAudit || audit[1].Code != tt.wantAuditCode {
				t.Fatalf("audit = %#v, want attempted then %q/%q", audit, tt.wantAudit, tt.wantAuditCode)
			}
		})
	}
}

func TestPrepareEnforcesTransactionParticipationAndSerialBoundaries(t *testing.T) {
	t.Parallel()
	provider := fakeTransactionProvider{
		capabilities: runtime.TransactionCapabilities{Savepoints: true},
		begin: func(context.Context, runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
			t.Fatal("transaction began despite invalid plan")
			return nil, nil, nil
		},
	}
	registry := runtime.NewRegistry(coreTypes(t))
	for _, registration := range []struct {
		name          string
		participation runtime.TransactionParticipation
	}{
		{name: "required", participation: runtime.TransactionRequired},
		{name: "excluded", participation: runtime.TransactionNone},
	} {
		descriptor := runtime.Descriptor{
			Name: registration.name, Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.WriteEffect),
		}
		descriptor.Metadata.Transaction = registration.participation
		if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) { return "", nil })); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider}); err != nil {
		t.Fatalf("ConfigureTransactions: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	tests := []struct {
		name  string
		doc   string
		codes []string
	}{
		{name: "required without boundary", doc: `{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"required"}}]}]}`, codes: []string{"TRANSACTION_REQUIRED"}},
		{name: "excluded inside boundary", doc: `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$call":{"name":"excluded"}}]}]}`, codes: []string{"TRANSACTION_PARTICIPATION"}},
		{name: "parallel inside transaction", doc: `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$parallel":{"select":[{"$call":{"name":"required"}}]}}]}]}`, codes: []string{"ATOMIC_PARALLEL_NOT_ALLOWED", "PARALLEL_TRANSACTION", "PARALLEL_MUTATION_NOT_ALLOWED"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := decodeRuntimeRequest(t, `{"version":"1","document":`+tt.doc+`}`)
			if got := validationCodes(t, prepareError(snapshot, request)); !slices.Equal(got, tt.codes) {
				t.Fatalf("codes = %v, want %v", got, tt.codes)
			}
		})
	}
}

func TestNestedAtomicGroupReportsEverySavepointFault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		beginErr    error
		releaseErr  error
		childErr    error
		rollbackErr error
		wantCodes   []string
	}{
		{name: "begin", beginErr: errors.New("savepoints unavailable"), wantCodes: []string{"SAVEPOINT_BEGIN_FAILED"}},
		{name: "release", releaseErr: errors.New("release failed"), wantCodes: []string{"SAVEPOINT_RELEASE_FAILED"}},
		{name: "rollback", childErr: errors.New("child failed"), rollbackErr: errors.New("savepoint rollback failed"), wantCodes: []string{"SAVEPOINT_ROLLBACK_FAILED", runtime.CodeHandlerFailed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := fakeTransactionProvider{
				capabilities: runtime.TransactionCapabilities{Savepoints: true},
				begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
					return ctx, fakeTransaction{
						commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
						rollback: func(context.Context) error { return nil },
						savepoint: func(savepointCtx context.Context, _ string) (context.Context, runtime.Savepoint, error) {
							if tt.beginErr != nil {
								return nil, nil, tt.beginErr
							}
							return savepointCtx, fakeSavepoint{
								release:  func(context.Context) error { return tt.releaseErr },
								rollback: func(context.Context) error { return tt.rollbackErr },
							}, nil
						},
					}, nil
				},
			}
			plan := transactionPlan(t, provider, nil, `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$atomic":{"name":"nested","select":[{"$call":{"name":"write"}}]}}]}]}`, func(context.Context) (string, error) {
				return "tentative", tt.childErr
			})
			outcome := plan.Execute(context.Background())
			assertExecutionCodes(t, outcome, tt.wantCodes)
			if outcome.Effects != runtime.EffectRolledBack || len(outcome.Data) != 0 {
				t.Fatalf("outcome = %#v", outcome)
			}
		})
	}
}

func TestTransactionBeginAndOutputCompletionFailuresDoNotPublishData(t *testing.T) {
	t.Parallel()
	t.Run("begin", func(t *testing.T) {
		provider := fakeTransactionProvider{begin: func(context.Context, runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
			return nil, nil, errors.New("database unavailable")
		}}
		plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(context.Context) (string, error) {
			t.Fatal("handler ran after begin failure")
			return "", nil
		})
		outcome := plan.Execute(context.Background())
		assertExecutionCodes(t, outcome, []string{"TRANSACTION_BEGIN_FAILED"})
		if outcome.Effects != runtime.EffectNone || len(outcome.Data) != 0 {
			t.Fatalf("outcome = %#v", outcome)
		}
	})
	t.Run("output completion", func(t *testing.T) {
		provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
			return ctx, fakeTransaction{
				commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
				rollback: func(context.Context) error { return nil },
			}, nil
		}}
		plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(context.Context) (string, error) { return string([]byte{0xff}), nil })
		outcome := plan.Execute(context.Background())
		assertExecutionCodes(t, outcome, []string{"OUTPUT_COMPLETION"})
		if outcome.Effects != runtime.EffectRolledBack || len(outcome.Data) != 0 {
			t.Fatalf("outcome = %#v", outcome)
		}
	})
}

func TestGroupAtomicityReportsCommittedPrefixAfterLaterRollback(t *testing.T) {
	t.Parallel()
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeTransaction{
			commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	calls := 0
	plan := transactionPlan(t, provider, nil, `{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"first","select":[{"$call":{"name":"write","as":"a"}}]}},{"$atomic":{"name":"second","select":[{"$call":{"name":"write","as":"b"}}]}}]}]}`, func(context.Context) (string, error) {
		calls++
		if calls == 2 {
			return "tentative", errors.New("second group failed")
		}
		return "committed", nil
	})
	outcome := plan.Execute(context.Background())
	assertExecutionCodes(t, outcome, []string{runtime.CodeHandlerFailed})
	if outcome.Effects != runtime.EffectPartiallyApplied {
		t.Fatalf("outcome = %#v", outcome)
	}
	assertJSONEqual(t, outcome.Data, map[string]any{"a": "committed"})
}

func assertExecutionCodes(t testing.TB, outcome runtime.Outcome, want []string) {
	t.Helper()
	if len(outcome.Errors) != len(want) {
		t.Fatalf("errors = %#v, want codes %v", outcome.Errors, want)
	}
	for index, code := range want {
		if outcome.Errors[index].Code != code {
			t.Fatalf("error %d = %#v, want code %q", index, outcome.Errors[index], code)
		}
	}
}

func operationAtomicDocument() string {
	return `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$call":{"name":"write"}}]}]}`
}

func transactionPlan(t *testing.T, provider runtime.TransactionProvider, audit runtime.MutationAuditHook, document string, handler func(context.Context) (string, error)) *runtime.Plan {
	t.Helper()
	registry := runtime.NewRegistry(coreTypes(t))
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.WriteEffect),
	}
	descriptor.Metadata.Transaction = runtime.TransactionRequired
	descriptor.Metadata.Idempotency = runtime.IdempotencyIdempotent
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		return handler(ctx)
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider, Audit: audit}); err != nil {
		t.Fatalf("ConfigureTransactions: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":`+document+`}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}
