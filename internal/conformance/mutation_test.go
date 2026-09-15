package conformance_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type mutationFixture struct {
	Profile        string           `json:"profile"`
	AtomicityModes []string         `json:"atomicityModes"`
	CommitOutcomes []string         `json:"commitOutcomes"`
	EffectStates   []string         `json:"effectStates"`
	AuditStages    []string         `json:"auditStages"`
	ErrorCodes     []string         `json:"errorCodes"`
	Vectors        []mutationVector `json:"vectors"`
}

type mutationVector struct {
	Name          string   `json:"name"`
	Atomicity     string   `json:"atomicity"`
	Groups        []string `json:"groups"`
	Events        []string `json:"events"`
	Fault         string   `json:"fault"`
	Code          string   `json:"code"`
	Effect        string   `json:"effect"`
	PublishesData bool     `json:"publishesData"`
}

func TestPortableMutationContractVectors(t *testing.T) {
	t.Parallel()
	var fixture mutationFixture
	readFixture(t, "mutations.json", &fixture)
	if fixture.Profile != "core.mutation-1" || !slices.Equal(fixture.AtomicityModes, []string{"none", "operation", "group"}) || len(fixture.Vectors) == 0 {
		t.Fatal("mutation fixture header is incomplete")
	}
	seenNames, seenCodes, seenEffects := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || seenNames[vector.Name] || vector.Atomicity == "" || vector.Effect == "" {
			t.Fatalf("incomplete or duplicate mutation vector %q", vector.Name)
		}
		seenNames[vector.Name], seenEffects[vector.Effect] = true, true
		if vector.Code != "" {
			seenCodes[vector.Code] = true
		}
	}
	for _, required := range []string{"operation-commit", "named-groups-commit-independently", "commit-unknown", "rollback-failed", "outbox-failed", "after-commit-delivery-failed", "after-commit-delivery-duplicated", "external-effect-uncoordinated", "compensation-failed", "cancel-before-commit", "cancel-during-confirmed-commit", "nested-savepoint-unsupported"} {
		if !seenNames[required] {
			t.Errorf("mutation fixture lacks %q", required)
		}
	}
	for _, code := range fixture.ErrorCodes {
		if code == "" {
			t.Fatal("mutation fixture contains an empty error code")
		}
	}
	for _, effect := range fixture.EffectStates {
		if effect == "" {
			t.Fatal("mutation fixture contains an empty effect state")
		}
	}
	_ = seenCodes
	_ = seenEffects
}

// TestPortableMutationContractVectorsAgainstRuntime decodes each mutation
// vector and drives it through the real transaction/mutation machinery in
// package runtime (Prepare + Plan.Execute with an injected transaction
// provider and the described fault), asserting that the runtime's actual
// outcome code and effect state match the fixture's declared expectation.
func TestPortableMutationContractVectorsAgainstRuntime(t *testing.T) {
	t.Parallel()
	var fixture mutationFixture
	readFixture(t, "mutations.json", &fixture)

	drivers := map[string]func(*testing.T, mutationVector){
		"operation-commit":                  mutationDriveOperationCommit,
		"named-groups-commit-independently": mutationDriveNamedGroups,
		"later-group-rolls-back":            mutationDriveLaterGroupRollback,
		"commit-unknown":                    mutationDriveCommitUnknown,
		"rollback-failed":                   mutationDriveRollbackFailed,
		"outbox-failed":                     mutationDriveOutboxFailed,
		"after-commit-delivery-failed":      mutationDriveAfterCommitFailed,
		"after-commit-delivery-duplicated":  mutationDriveAfterCommitDuplicated,
		"external-effect-uncoordinated":     mutationDriveExternalEffectUncoordinated,
		"compensation-failed":               mutationDriveCompensationFailed,
		"cancel-before-commit":              mutationDriveCancelBeforeCommit,
		"cancel-during-confirmed-commit":    mutationDriveCancelDuringCommit,
		"nested-savepoint-unsupported":      mutationDriveNestedSavepointUnsupported,
		"duplicate-group-name":              mutationDriveDuplicateGroupName,
	}

	executed := 0
	for _, vector := range fixture.Vectors {
		driver, ok := drivers[vector.Name]
		if !ok {
			continue
		}
		executed++
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			driver(t, vector)
		})
	}
	if executed < 12 {
		t.Fatalf("drove only %d mutation vectors against the runtime, want at least 12", executed)
	}
}

// mutationEffectState maps a fixture effect string to the runtime effect state.
func mutationEffectState(t *testing.T, effect string) runtime.EffectState {
	t.Helper()
	switch effect {
	case "applied":
		return runtime.EffectApplied
	case "partially-applied":
		return runtime.EffectPartiallyApplied
	case "rolled-back":
		return runtime.EffectRolledBack
	case "compensated":
		return runtime.EffectCompensated
	case "indeterminate":
		return runtime.EffectIndeterminate
	case "none":
		return runtime.EffectNone
	default:
		t.Fatalf("unmapped mutation effect %q", effect)
		return runtime.EffectNone
	}
}

func mutationDriveOperationCommit(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	var events []string
	plan := mutationExecPlan(t, provider, nil, `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$call":{"name":"write"}}]}]}`, func(ctx context.Context) (string, error) {
		if err := runtime.RegisterOutbox(ctx, func(context.Context) error { events = append(events, "outbox"); return nil }); err != nil {
			return "", err
		}
		if err := runtime.RegisterAfterCommit(ctx, func(context.Context) error { events = append(events, "after-commit"); return nil }); err != nil {
			return "", err
		}
		return "written", nil
	})
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("operation commit errors = %#v", outcome.Errors)
	}
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
	if vector.PublishesData && len(outcome.Data) == 0 {
		t.Fatal("committed operation withheld its data")
	}
}

func mutationDriveNamedGroups(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	plan := mutationExecPlan(t, provider, nil, `{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"account","select":[{"$call":{"name":"write","as":"a"}}]}},{"$atomic":{"name":"ledger","select":[{"$call":{"name":"write","as":"b"}}]}}]}]}`, func(context.Context) (string, error) {
		return "written", nil
	})
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("named groups outcome = %#v, want effect %q", outcome, vector.Effect)
	}
}

func mutationDriveLaterGroupRollback(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	calls := 0
	plan := mutationExecPlan(t, provider, nil, `{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"account","select":[{"$call":{"name":"write","as":"a"}}]}},{"$atomic":{"name":"ledger","select":[{"$call":{"name":"write","as":"b"}}]}}]}]}`, func(context.Context) (string, error) {
		calls++
		if calls == 2 {
			return "tentative", errors.New("ledger failed")
		}
		return "committed", nil
	})
	outcome := plan.Execute(context.Background())
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
}

func mutationDriveCommitUnknown(t *testing.T, vector mutationVector) {
	provider := fakeConformanceTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeConformanceTransaction{
			commit: func(context.Context) (runtime.CommitOutcome, error) {
				return runtime.CommitUnknown, errors.New("connection lost")
			},
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(context.Context) (string, error) { return "tentative", nil })
	outcome := plan.Execute(context.Background())
	assertMutationCode(t, outcome, vector.Code)
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
}

func mutationDriveRollbackFailed(t *testing.T, vector mutationVector) {
	provider := fakeConformanceTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeConformanceTransaction{
			commit: func(context.Context) (runtime.CommitOutcome, error) {
				return runtime.CommitNotApplied, errors.New("commit rejected")
			},
			rollback: func(context.Context) error { return errors.New("rollback failed") },
		}, nil
	}}
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(context.Context) (string, error) { return "tentative", nil })
	outcome := plan.Execute(context.Background())
	assertMutationCode(t, outcome, vector.Code)
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
}

func mutationDriveOutboxFailed(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(ctx context.Context) (string, error) {
		return "written", runtime.RegisterOutbox(ctx, func(context.Context) error { return errors.New("outbox unavailable") })
	})
	outcome := plan.Execute(context.Background())
	assertMutationCode(t, outcome, vector.Code)
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
}

func mutationDriveAfterCommitFailed(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(ctx context.Context) (string, error) {
		return "written", runtime.RegisterAfterCommit(ctx, func(context.Context) error { return errors.New("delivery unavailable") })
	})
	outcome := plan.Execute(context.Background())
	assertMutationCode(t, outcome, vector.Code)
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
	if vector.PublishesData && len(outcome.Data) == 0 {
		t.Fatal("applied after-commit failure withheld already-committed data")
	}
}

func mutationDriveAfterCommitDuplicated(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	attempts, deliveries := 0, 0
	delivered := map[string]bool{}
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(ctx context.Context) (string, error) {
		return "written", runtime.RegisterAfterCommit(ctx, func(context.Context) error {
			attempts++
			if delivered["event-1"] {
				return nil
			}
			delivered["event-1"] = true
			deliveries++
			return nil
		})
	})
	for run := 0; run < 2; run++ {
		if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 || outcome.Effects != mutationEffectState(t, vector.Effect) {
			t.Fatalf("duplicated after-commit run %d outcome = %#v", run, outcome)
		}
	}
	if attempts != 2 || deliveries != 1 {
		t.Fatalf("delivery attempts = %d, applied = %d; want 2 and 1", attempts, deliveries)
	}
}

func mutationDriveExternalEffectUncoordinated(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(ctx context.Context) (string, error) {
		if err := runtime.RegisterExternalEffect(ctx, nil); err != nil {
			return "", err
		}
		return "tentative", errors.New("later mutation failed")
	})
	outcome := plan.Execute(context.Background())
	assertMutationCode(t, outcome, vector.Code)
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
}

func mutationDriveCompensationFailed(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(ctx context.Context) (string, error) {
		if err := runtime.RegisterExternalEffect(ctx, func(context.Context) error { return errors.New("remote compensation failed") }); err != nil {
			return "", err
		}
		return "tentative", errors.New("later mutation failed")
	})
	outcome := plan.Execute(context.Background())
	assertMutationCode(t, outcome, vector.Code)
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
}

func mutationDriveCancelBeforeCommit(t *testing.T, vector mutationVector) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(context.Context) (string, error) {
		cancel()
		return "tentative", nil
	})
	outcome := plan.Execute(ctx)
	if outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("effect = %q, want %q (%s)", outcome.Effects, vector.Effect, vector.Name)
	}
}

func mutationDriveCancelDuringCommit(t *testing.T, vector mutationVector) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := fakeConformanceTransactionProvider{begin: func(beginCtx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return beginCtx, fakeConformanceTransaction{
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
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(context.Context) (string, error) { return "written", nil })
	outcome := plan.Execute(ctx)
	if len(outcome.Errors) != 0 || outcome.Effects != mutationEffectState(t, vector.Effect) {
		t.Fatalf("cancel-during-commit outcome = %#v, want effect %q", outcome, vector.Effect)
	}
}

func mutationDriveNestedSavepointUnsupported(t *testing.T, vector mutationVector) {
	provider := fakeConformanceTransactionProvider{begin: func(context.Context, runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		t.Fatal("transaction began despite unsupported nested savepoint")
		return nil, nil, nil
	}}
	codes := mutationPrepareCodes(t, provider, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$atomic":{"name":"nested","select":[]}}]}]}}`)
	if !slices.Contains(codes, vector.Code) {
		t.Fatalf("prepare codes = %v, want %q (%s)", codes, vector.Code, vector.Name)
	}
}

func mutationDriveDuplicateGroupName(t *testing.T, vector mutationVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	codes := mutationPrepareCodes(t, provider, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"account","select":[]}},{"$atomic":{"name":"account","select":[]}}]}]}}`)
	if !slices.Contains(codes, vector.Code) {
		t.Fatalf("prepare codes = %v, want %q (%s)", codes, vector.Code, vector.Name)
	}
}

func mutationOperationDocument() string {
	return `{"operations":[{"name":"M","kind":"mutation","atomicity":"operation","select":[{"$call":{"name":"write"}}]}]}`
}

func assertMutationCode(t *testing.T, outcome runtime.Outcome, code string) {
	t.Helper()
	for _, failure := range outcome.Errors {
		if failure.Code == code {
			return
		}
	}
	t.Fatalf("outcome errors = %#v, want code %q", outcome.Errors, code)
}

// mutationExecPlan builds a registry with a transaction-required "write"
// mutation handler, configures authorization + the injected transaction
// provider, freezes, and prepares the given mutation document.
func mutationExecPlan(t *testing.T, provider runtime.TransactionProvider, audit runtime.MutationAuditHook, document string, handler func(context.Context) (string, error)) *runtime.Plan {
	t.Helper()
	registry := runtime.NewRegistry(conformanceCoreTypes(t))
	metadata := conformanceMetadata(runtime.WriteEffect)
	metadata.Transaction = runtime.TransactionRequired
	metadata.Idempotency = runtime.IdempotencyIdempotent
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		return handler(ctx)
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider, Audit: audit}); err != nil {
		t.Fatalf("ConfigureTransactions: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":`+document+`}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

func mutationPrepareCodes(t *testing.T, provider runtime.TransactionProvider, request string) []string {
	t.Helper()
	registry := runtime.NewRegistry(conformanceCoreTypes(t))
	metadata := conformanceMetadata(runtime.WriteEffect)
	metadata.Transaction = runtime.TransactionRequired
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) { return "", nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider}); err != nil {
		t.Fatalf("ConfigureTransactions: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	_, err = runtime.Prepare(snapshot, decodeConformanceRequest(t, request))
	var validationErr *runtime.ValidationErrors
	if !errors.As(err, &validationErr) {
		t.Fatalf("Prepare error = %T %v, want ValidationErrors", err, err)
	}
	issues := validationErr.Issues()
	codes := make([]string, len(issues))
	for index, issue := range issues {
		codes[index] = issue.Diagnostic.Code
	}
	return codes
}

// ---- Injected transaction provider used across the mutation drivers. ----

type fakeConformanceTransactionProvider struct {
	capabilities runtime.TransactionCapabilities
	begin        func(context.Context, runtime.TransactionRequest) (context.Context, runtime.Transaction, error)
}

func (p fakeConformanceTransactionProvider) Capabilities() runtime.TransactionCapabilities {
	return p.capabilities
}

func (p fakeConformanceTransactionProvider) Begin(ctx context.Context, request runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
	return p.begin(ctx, request)
}

type fakeConformanceTransaction struct {
	commit   func(context.Context) (runtime.CommitOutcome, error)
	rollback func(context.Context) error
}

func (tx fakeConformanceTransaction) Commit(ctx context.Context) (runtime.CommitOutcome, error) {
	return tx.commit(ctx)
}

func (tx fakeConformanceTransaction) Rollback(ctx context.Context) error { return tx.rollback(ctx) }

func (tx fakeConformanceTransaction) Savepoint(context.Context, string) (context.Context, runtime.Savepoint, error) {
	return nil, nil, errors.New("savepoints unsupported")
}

// committingTransactionProvider returns a provider whose transactions always
// commit as applied, optionally threading a per-begin observer.
func committingTransactionProvider(onBegin func(runtime.TransactionRequest)) fakeConformanceTransactionProvider {
	return fakeConformanceTransactionProvider{begin: func(ctx context.Context, request runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		if onBegin != nil {
			onBegin(request)
		}
		return ctx, fakeConformanceTransaction{
			commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
}

// ---- Shared runtime helpers used by every execution-driven conformance test. ----

// conformanceCoreTypes freezes an empty schema catalog (scalar types only).
func conformanceCoreTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	snapshot, err := schema.NewCatalog().Freeze()
	if err != nil {
		t.Fatalf("freeze core types: %v", err)
	}
	return snapshot
}

// conformanceMetadata returns complete, well-formed handler metadata with a
// non-empty authorization policy, as Freeze now requires.
func conformanceMetadata(effect runtime.Effect) runtime.Metadata {
	return runtime.Metadata{
		Effect:              effect,
		Deterministic:       true,
		Cacheable:           effect == runtime.ReadEffect,
		RetrySafe:           true,
		ThreadSafety:        runtime.ThreadSafe,
		Batching:            runtime.BatchIneligible,
		Transaction:         runtime.TransactionNone,
		AuthorizationPolicy: "conformance",
	}
}

func decodeConformanceRequest(t *testing.T, body string) *protocol.Request {
	t.Helper()
	request, err := protocol.DecodeRequest([]byte(body), protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return request
}
