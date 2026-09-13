package runtime

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Core execution error codes. A runtime reserves these so a client can rely on
// their meaning regardless of which application produced the failure.
const (
	// CodeCancelled reports that the request was cancelled or its deadline
	// expired. It acknowledges requested termination, never proof that an
	// effect was undone.
	CodeCancelled = "CANCELLED"
	// CodeHandlerFailed reports an application handler failure whose cause is
	// not disclosed.
	CodeHandlerFailed = "HANDLER_FAILED"
	// CodeInternal reports a contained runtime or host failure, including a
	// handler panic.
	CodeInternal = "INTERNAL"
	// CodeValidationFailed reports that an operation was rejected before
	// execution because it does not satisfy the runtime contract.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeOutputCompletion reports output that did not satisfy its declared
	// schema type.
	CodeOutputCompletion = "OUTPUT_COMPLETION"
	// CodeInvalidCollection reports a collection operation applied to a value
	// that is not a collection.
	CodeInvalidCollection = "INVALID_COLLECTION"
	// CodeInvalidCursor deliberately covers malformed, tampered, expired, and
	// scope-mismatched cursors without disclosing which check failed.
	CodeInvalidCursor = "INVALID_CURSOR"
	// CodeResultMissing, CodeResultNull, CodeResultSkipped, CodeResultScope and
	// CodeResultUnavailable report why a continuation had no usable value.
	CodeResultMissing     = "RESULT_MISSING"
	CodeResultNull        = "RESULT_NULL"
	CodeResultSkipped     = "RESULT_SKIPPED"
	CodeResultScope       = "RESULT_SCOPE"
	CodeResultUnavailable = "RESULT_UNAVAILABLE"
	// CodeUnauthorized and CodeResourceExhausted are defined here so the shapes
	// are language-neutral; enforcement is owned by #11 and #12.
	CodeUnauthorized      = "UNAUTHORIZED"
	CodeResourceExhausted = "RESOURCE_EXHAUSTED"
	// Transaction lifecycle codes distinguish the boundary at which an atomic
	// mutation failed. In particular, commit uncertainty is not rollback.
	CodeTransactionBeginFailed      = "TRANSACTION_BEGIN_FAILED"
	CodeTransactionCommitFailed     = "TRANSACTION_COMMIT_FAILED"
	CodeTransactionCommitUnknown    = "TRANSACTION_COMMIT_UNKNOWN"
	CodeTransactionRollbackFailed   = "TRANSACTION_ROLLBACK_FAILED"
	CodeSavepointBeginFailed        = "SAVEPOINT_BEGIN_FAILED"
	CodeSavepointReleaseFailed      = "SAVEPOINT_RELEASE_FAILED"
	CodeSavepointRollbackFailed     = "SAVEPOINT_ROLLBACK_FAILED"
	CodeOutboxPersistFailed         = "OUTBOX_PERSIST_FAILED"
	CodeAfterCommitFailed           = "AFTER_COMMIT_FAILED"
	CodeExternalEffectUncoordinated = "EXTERNAL_EFFECT_UNCOORDINATED"
	CodeCompensationFailed          = "COMPENSATION_FAILED"
	CodeRetryBudgetExhausted        = "RETRY_BUDGET_EXHAUSTED"
	CodeIdempotencyConflict         = "IDEMPOTENCY_CONFLICT"
	CodeIdempotencyIndeterminate    = "IDEMPOTENCY_INDETERMINATE"
	CodeIdempotencyNotAllowed       = "IDEMPOTENCY_NOT_ALLOWED"
	CodeIdempotencyStoreUnavailable = "IDEMPOTENCY_STORE_UNAVAILABLE"
	CodeCacheInvalidationFailed     = "CACHE_INVALIDATION_FAILED"
)

// reservedCodes are the codes only a runtime may produce. An application that
// claims one would let a domain failure impersonate a protocol guarantee.
var reservedCodes = []string{
	CodeCancelled, CodeHandlerFailed, CodeInternal, CodeOutputCompletion,
	CodeInvalidCollection, CodeInvalidCursor, CodeResultMissing, CodeResultNull, CodeResultSkipped,
	CodeResultScope, CodeResultUnavailable, CodeUnauthorized, CodeResourceExhausted,
	CodeTransactionBeginFailed, CodeTransactionCommitFailed, CodeTransactionCommitUnknown,
	CodeTransactionRollbackFailed, CodeSavepointBeginFailed, CodeSavepointReleaseFailed,
	CodeSavepointRollbackFailed, CodeOutboxPersistFailed, CodeAfterCommitFailed,
	CodeExternalEffectUncoordinated, CodeCompensationFailed,
	CodeRetryBudgetExhausted, CodeIdempotencyConflict, CodeIdempotencyIndeterminate,
	CodeIdempotencyNotAllowed, CodeIdempotencyStoreUnavailable, CodeCacheInvalidationFailed,
}

// applicationCodePattern constrains a domain code to a stable, wire-safe
// spelling so it can be matched by clients that never saw the application.
var applicationCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// Error is a domain failure a handler returns to choose the public code,
// message, retryability, and details of its execution error without exposing
// its cause. It maps onto the core error shape rather than replacing it: the
// runtime still owns the response path, the source location, and redaction.
//
// A handler returning any other error yields HANDLER_FAILED with a generic
// message, so a cause is never disclosed by accident.
type Error struct {
	// Code is the stable public code. It must match [A-Z][A-Z0-9_]{2,63} and
	// must not be a reserved runtime code; otherwise the runtime substitutes
	// HANDLER_FAILED so an application cannot impersonate a core guarantee.
	Code string
	// Message is a safe, public description. It must not embed internal detail.
	Message string
	// Retryable is advice about the failure class. It is never permission to
	// replay a write; idempotency metadata remains authoritative.
	Retryable bool
	// RetryAfter is a server-side lower bound for a later attempt. It is safe
	// scheduling metadata and is ignored unless Retryable is true.
	RetryAfter time.Duration
	// Details is optional namespaced context. Keys must be namespaced with a
	// period so a vendor cannot collide with core or another vendor.
	Details map[string]any
	// Cause stays internal and is never serialized.
	Cause error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the internal cause to in-process server hooks only.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// valid reports whether this domain error may set the public code. An invalid
// code or message is downgraded rather than rejected, so a handler bug degrades
// to a safe generic failure instead of failing the whole response.
func (e *Error) valid() bool {
	if !applicationCodePattern.MatchString(e.Code) || slices.Contains(reservedCodes, e.Code) {
		return false
	}
	return strings.TrimSpace(e.Message) != "" && e.RetryAfter >= 0
}

// publicDetails copies only namespaced detail keys, dropping anything that
// could collide with a core or unnamespaced field.
func (e *Error) publicDetails() map[string]any {
	if len(e.Details) == 0 {
		return nil
	}
	details := make(map[string]any, len(e.Details))
	for key, value := range e.Details {
		if strings.Contains(key, ".") {
			details[key] = value
		}
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

// applyDomainError maps a handler failure onto the core error shape, returning
// the failure unchanged when the handler did not select a valid public code.
func applyDomainError(failure ExecutionError, err error) ExecutionError {
	var domain *Error
	if !errors.As(err, &domain) || !domain.valid() {
		return failure
	}
	failure.Code = domain.Code
	failure.Message = domain.Message
	failure.Retryable = domain.Retryable
	failure.RetryAfter = domain.RetryAfter
	failure.Details = domain.publicDetails()
	return failure
}
