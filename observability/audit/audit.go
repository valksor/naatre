package audit

import (
	"context"
	"errors"

	"github.com/valksor/naatre/runtime"
)

const (
	CodeInvalidConfig      = "AUDIT_INVALID_CONFIG"
	CodeInvalidEvent       = "AUDIT_INVALID_EVENT"
	CodeTransactionNeeded  = "AUDIT_TRANSACTION_REQUIRED"
	CodeTransactionClosed  = "AUDIT_TRANSACTION_CLOSED"
	CodeResourceExhausted  = "RESOURCE_EXHAUSTED"
	CodePersistenceFailure = "AUDIT_PERSIST_FAILED"
)

const (
	defaultMaxBatch      = 16
	defaultMaxFieldBytes = 256
)

// Error is safe to expose at an application boundary. It deliberately omits
// backend errors and transaction implementation details.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Writer appends a runtime audit fact using application-owned storage. For
// Admit and AdmitBatch, ctx contains the application's active transaction.
// Implementations must be concurrency-safe when Hook is used concurrently.
type Writer interface {
	Append(context.Context, runtime.MutationAuditEvent) error
}

// WriterFunc adapts a function to Writer.
type WriterFunc func(context.Context, runtime.MutationAuditEvent) error

func (f WriterFunc) Append(ctx context.Context, event runtime.MutationAuditEvent) error {
	return f(ctx, event)
}

// Options bound one admission call. Zero values select conservative defaults.
type Options struct {
	MaxBatch      int
	MaxFieldBytes int
}

// Recorder validates audit facts and connects them either to the active
// transaction outbox or to the runtime's best-effort audit hook.
type Recorder struct {
	writer        Writer
	maxBatch      int
	maxFieldBytes int
}

func New(writer Writer, options Options) (*Recorder, error) {
	if writer == nil {
		return nil, publicError(CodeInvalidConfig, "audit writer is required")
	}
	if options.MaxBatch < 0 || options.MaxFieldBytes < 0 {
		return nil, publicError(CodeInvalidConfig, "audit limits must not be negative")
	}
	if options.MaxBatch == 0 {
		options.MaxBatch = defaultMaxBatch
	}
	if options.MaxFieldBytes == 0 {
		options.MaxFieldBytes = defaultMaxFieldBytes
	}
	return &Recorder{writer: writer, maxBatch: options.MaxBatch, maxFieldBytes: options.MaxFieldBytes}, nil
}

// Admit transactionally enrolls one attempted mutation audit fact. The fact
// is visible only if the surrounding application transaction commits.
func (r *Recorder) Admit(ctx context.Context, event runtime.MutationAuditEvent) error {
	return r.AdmitBatch(ctx, []runtime.MutationAuditEvent{event})
}

// AdmitBatch transactionally enrolls a bounded ordered batch. Only attempted
// facts are accepted: committed, rollback, compensation, and indeterminate
// truth is known only at later lifecycle boundaries and must not be predicted.
func (r *Recorder) AdmitBatch(ctx context.Context, events []runtime.MutationAuditEvent) error {
	if r == nil || r.writer == nil {
		return publicError(CodeInvalidConfig, "audit recorder is not configured")
	}
	if len(events) == 0 {
		return publicError(CodeInvalidEvent, "audit batch is empty")
	}
	if len(events) > r.maxBatch {
		return publicError(CodeResourceExhausted, "audit batch limit exceeded")
	}
	snapshot := append([]runtime.MutationAuditEvent(nil), events...)
	for index := range snapshot {
		event, err := r.normalize(snapshot[index], true)
		if err != nil {
			return err
		}
		snapshot[index] = event
	}
	err := runtime.RegisterOutbox(ctx, func(transactionContext context.Context) error {
		for _, event := range snapshot {
			if err := r.append(transactionContext, event); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtime.ErrNoTransactionContext):
		return publicError(CodeTransactionNeeded, "audit admission requires an active transaction")
	case errors.Is(err, runtime.ErrTransactionClosed):
		return publicError(CodeTransactionClosed, "audit transaction is closed")
	default:
		return publicError(CodeInvalidEvent, "audit admission failed")
	}
}

// Hook returns the best-effort runtime audit integration. Writer failures are
// reduced to a stable public error; the runtime contains that error and never
// changes execution or transaction truth because of it.
func (r *Recorder) Hook() runtime.MutationAuditHook {
	return func(ctx context.Context, event runtime.MutationAuditEvent) error {
		if r == nil || r.writer == nil {
			return publicError(CodeInvalidConfig, "audit recorder is not configured")
		}
		normalized, err := r.normalize(event, false)
		if err != nil {
			return err
		}
		return r.append(ctx, normalized)
	}
}

func (r *Recorder) append(ctx context.Context, event runtime.MutationAuditEvent) (err error) {
	defer func() {
		if recover() != nil {
			err = publicError(CodePersistenceFailure, "audit persistence failed")
		}
	}()
	if err := r.writer.Append(ctx, event); err != nil {
		return publicError(CodePersistenceFailure, "audit persistence failed")
	}
	return nil
}

func (r *Recorder) normalize(event runtime.MutationAuditEvent, admission bool) (runtime.MutationAuditEvent, error) {
	if event.Operation == "" {
		return runtime.MutationAuditEvent{}, publicError(CodeInvalidEvent, "audit operation is required")
	}
	if admission && event.Stage != runtime.MutationAttempted {
		return runtime.MutationAuditEvent{}, publicError(CodeInvalidEvent, "audit admission requires an attempted fact")
	}
	switch event.Stage {
	case runtime.MutationAttempted, runtime.MutationDenied, runtime.MutationCommitted,
		runtime.MutationRolledBack, runtime.MutationCompensated, runtime.MutationIndeterminate:
	default:
		return runtime.MutationAuditEvent{}, publicError(CodeInvalidEvent, "audit stage is invalid")
	}
	fields := []string{event.Operation, event.Group, event.Code, event.RequestID, event.OperationID, event.PrincipalReference}
	for _, field := range fields {
		if len(field) > r.maxFieldBytes {
			return runtime.MutationAuditEvent{}, publicError(CodeResourceExhausted, "audit field limit exceeded")
		}
	}
	if event.Code != "" {
		normalized := runtime.NormalizeTelemetryEvent(runtime.TelemetryEvent{
			Kind: runtime.TelemetryTransaction, Stage: runtime.TelemetryStage(event.Stage), ErrorCode: event.Code,
		})
		event.Code = normalized.ErrorCode
	}
	return event, nil
}

func publicError(code, message string) error {
	return &Error{Code: code, Message: message}
}

// CodeOf returns the stable public code for an audit integration error.
func CodeOf(err error) string {
	var public *Error
	if errors.As(err, &public) {
		return public.Code
	}
	return ""
}
