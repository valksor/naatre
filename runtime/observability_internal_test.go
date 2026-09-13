package runtime

import (
	"context"
	"testing"
)

func TestMutationAuditTelemetryClassifiesEveryStage(t *testing.T) {
	t.Parallel()
	tests := map[MutationAuditStage]TelemetryOutcome{
		MutationAttempted:     TelemetryActive,
		MutationDenied:        TelemetryDeniedOutcome,
		MutationCommitted:     TelemetrySucceeded,
		MutationRolledBack:    TelemetryFailedOutcome,
		MutationCompensated:   TelemetryFailedOutcome,
		MutationIndeterminate: TelemetryFailedOutcome,
	}
	for stage, want := range tests {
		t.Run(string(stage), func(t *testing.T) {
			var events []TelemetryEvent
			options := TelemetryOptions{Hooks: TelemetryHooks{Trace: func(event TelemetryEvent) error {
				events = append(events, event)
				return nil
			}}}
			execution := newExecutionTelemetry(options, "", "", 0)
			ctx := context.WithValue(context.Background(), telemetryContextKey{}, executionTelemetryContext{options: options, execution: execution})
			new(Plan).auditMutation(ctx, MutationAuditEvent{Stage: stage})
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if got := events[0].Outcome; got != want {
				t.Fatalf("outcome = %q, want %q", got, want)
			}
		})
	}
}

func TestBoundedTelemetryCodePreservesRuntimeVocabulary(t *testing.T) {
	t.Parallel()
	codes := []string{
		CodeCancelled, CodeHandlerFailed, CodeInternal, CodeValidationFailed, CodeOutputCompletion,
		CodeInvalidCollection, CodeInvalidCursor, CodeResultMissing, CodeResultNull,
		CodeResultSkipped, CodeResultScope, CodeResultUnavailable, CodeUnauthorized,
		CodeResourceExhausted, CodeTransactionBeginFailed, CodeTransactionCommitFailed,
		CodeTransactionCommitUnknown, CodeTransactionRollbackFailed, CodeSavepointBeginFailed,
		CodeSavepointReleaseFailed, CodeSavepointRollbackFailed, CodeOutboxPersistFailed,
		CodeAfterCommitFailed, CodeExternalEffectUncoordinated, CodeCompensationFailed,
		CodeRetryBudgetExhausted, CodeIdempotencyConflict, CodeIdempotencyIndeterminate,
		CodeIdempotencyNotAllowed, CodeIdempotencyStoreUnavailable, CodeCacheInvalidationFailed,
	}
	for _, code := range codes {
		if got := boundedTelemetryCode(code); got != code {
			t.Errorf("boundedTelemetryCode(%q) = %q", code, got)
		}
	}
	if got := boundedTelemetryCode("APPLICATION_SECRET_CODE"); got != CodeInternal {
		t.Errorf("unknown code = %q, want %q", got, CodeInternal)
	}
}
