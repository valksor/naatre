package largevalueadapter

import (
	"context"
	"io"
	"time"

	"github.com/valksor/naatre/largevalue"
)

type ApplicationConfig struct {
	Coordinator    *largevalue.Coordinator
	Provider       ApplicationProvider
	Limits         Limits
	CleanupTimeout time.Duration
}

// ApplicationAdapter bridges application-owned streaming sources and
// transactional sinks without changing the core capability or metadata model.
type ApplicationAdapter struct {
	coordinator *largevalue.Coordinator
	provider    ApplicationProvider
	limits      Limits
	cleanup     cleanupPolicy
}

func NewApplicationAdapter(config ApplicationConfig) (*ApplicationAdapter, error) {
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	}
	if config.Coordinator == nil || config.Provider == nil || !validLimits(config.Limits) || config.CleanupTimeout <= 0 {
		return nil, publicError(CodeInvalidConfig, "application transfer adapter configuration is invalid", nil)
	}
	return &ApplicationAdapter{coordinator: config.Coordinator, provider: config.Provider, limits: config.Limits,
		cleanup: cleanupPolicy{timeout: config.CleanupTimeout}}, nil
}

// Import streams an application-owned source through the core verification and
// finalization gate. The adapter always closes the source it opens.
func (a *ApplicationAdapter) Import(ctx context.Context, reference, sourceID string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	record, err := a.coordinator.Inspect(ctx, reference, largevalue.AuthorizeUpload, "")
	if err != nil || record.Direction != largevalue.Upload || record.Profile != largevalue.ApplicationProfile ||
		record.Metadata.Length > a.limits.MaximumTransferBytes || !validIdentifier(sourceID, a.limits.MaximumIdentifier) {
		return publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	}
	source, err := a.provider.OpenSource(ctx, sourceID)
	if err != nil {
		return transferError(ctx, err)
	}
	if source == nil {
		return publicError(CodeTransferFailed, "large value transfer failed", nil)
	}
	transferErr := a.coordinator.AcceptUpload(ctx, reference, &contextReader{ctx: ctx, reader: source})
	closeErr := source.Close()
	if transferErr != nil {
		return transferError(ctx, transferErr)
	}
	if closeErr != nil {
		return publicError(CodeTransferFailed, "large value transfer failed", nil)
	}
	return nil
}

// Export streams finalized content into an application-owned transactional
// sink. Failed, cancelled, or abandoned output is aborted under an independent
// bounded cleanup context; ownership transfers only after Commit succeeds.
func (a *ApplicationAdapter) Export(ctx context.Context, reference, sinkID string) (err error) {
	if contextErr := contextError(ctx); contextErr != nil {
		return contextErr
	}
	record, inspectErr := a.coordinator.Inspect(ctx, reference, largevalue.AuthorizeConsume, "")
	if inspectErr != nil || record.Direction != largevalue.Download || record.Profile != largevalue.ApplicationProfile ||
		record.Metadata.Length > a.limits.MaximumTransferBytes || !validIdentifier(sinkID, a.limits.MaximumIdentifier) {
		return publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	}
	sink, openErr := a.provider.OpenSink(ctx, sinkID)
	if openErr != nil {
		return transferError(ctx, openErr)
	}
	if sink == nil {
		return publicError(CodeTransferFailed, "large value transfer failed", nil)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := a.cleanup.context(ctx)
		defer cancel()
		if abortErr := sink.Abort(cleanup); abortErr != nil && err == nil {
			err = publicError(CodeCleanupFailed, "large value cleanup failed", nil)
		}
	}()
	if consumeErr := a.coordinator.Consume(ctx, reference, func(copyCtx context.Context, _ largevalue.Record, source io.Reader) error {
		buffer := make([]byte, 32*1024)
		_, copyErr := io.CopyBuffer(writerOnly{Writer: sink}, &contextReader{ctx: copyCtx, reader: source}, buffer)
		return copyErr
	}); consumeErr != nil {
		return transferError(ctx, consumeErr)
	}
	if closeErr := sink.Close(); closeErr != nil {
		return publicError(CodeTransferFailed, "large value transfer failed", nil)
	}
	if commitErr := sink.Commit(ctx); commitErr != nil {
		return transferError(ctx, commitErr)
	}
	committed = true
	return nil
}

func validIdentifier(value string, maximum int) bool {
	return value != "" && len(value) <= maximum
}

type writerOnly struct{ io.Writer }
