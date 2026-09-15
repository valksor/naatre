// Package largevalueadapter provides Go transfer adapters for the normative
// core.large-value-1 contract implemented by package largevalue.
package largevalueadapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"time"

	"github.com/valksor/naatre/largevalue"
)

const (
	// Profile identifies this implementation slice. Package largevalue and
	// core.large-value-1 remain the protocol and schema authority.
	Profile = "implementation.go.large-value-adapters-1"

	CodeInvalidConfig         = "LARGE_ADAPTER_INVALID_CONFIG"
	CodeInvalidRequest        = "LARGE_ADAPTER_INVALID_REQUEST"
	CodeUnsupportedCapability = "LARGE_ADAPTER_UNSUPPORTED_CAPABILITY"
	CodeUnavailable           = "LARGE_ADAPTER_UNAVAILABLE"
	CodeTransferFailed        = "LARGE_ADAPTER_TRANSFER_FAILED"
	CodeIntegrityFailed       = "LARGE_ADAPTER_INTEGRITY_FAILED"
	CodeResourceExhausted     = "LARGE_ADAPTER_RESOURCE_EXHAUSTED"
	CodeCancelled             = "LARGE_ADAPTER_CANCELLED"
	CodeEgressDenied          = "LARGE_ADAPTER_EGRESS_DENIED"
	CodeConflict              = "LARGE_ADAPTER_CONFLICT"
	CodeCleanupFailed         = "LARGE_ADAPTER_CLEANUP_FAILED"
)

// Error is a bounded public failure. It retains no capability, URL, header,
// filename, storage path, response body, backend error, or implementation-only
// detail.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	match   error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (e *Error) Is(target error) bool {
	if e == target || e.match == target {
		return true
	}
	other, ok := target.(*Error)
	return ok && e.Code == other.Code
}

// ErrorCode returns the stable adapter code, or an empty string for a failure
// from outside this package.
func ErrorCode(err error) string {
	var public *Error
	if errors.As(err, &public) {
		return public.Code
	}
	return ""
}

// Limits bounds transfer, assembly, metadata, and cleanup work without using
// caller-controlled lengths for proportional allocation.
type Limits struct {
	MaximumTransferBytes int64
	MaximumChunkBytes    int64
	MaximumParts         int
	MaximumSessions      int
	MaximumHeaderBytes   int64
	MaximumURLBytes      int
	MaximumIdentifier    int
	MaximumRedirects     int
	MaximumCleanupBatch  int
}

// DefaultLimits returns the finite portable Go adapter profile.
func DefaultLimits() Limits {
	return Limits{
		MaximumTransferBytes: 64 << 20,
		MaximumChunkBytes:    8 << 20,
		MaximumParts:         128,
		MaximumSessions:      128,
		MaximumHeaderBytes:   32 << 10,
		MaximumURLBytes:      8 << 10,
		MaximumIdentifier:    128,
		MaximumRedirects:     largevalue.DefaultMaximumRedirects,
		MaximumCleanupBatch:  128,
	}
}

func validLimits(limits Limits) bool {
	return limits.MaximumTransferBytes > largevalue.DefaultSmallByteLimit && limits.MaximumChunkBytes > 0 &&
		limits.MaximumChunkBytes <= limits.MaximumTransferBytes && limits.MaximumParts > 0 && limits.MaximumSessions > 0 &&
		limits.MaximumHeaderBytes > 0 && limits.MaximumURLBytes > 0 && limits.MaximumIdentifier > 0 && limits.MaximumRedirects >= 0 && limits.MaximumCleanupBatch > 0
}

func publicError(code, message string, match error) error {
	return &Error{Code: code, Message: message, match: match}
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return publicError(CodeCancelled, "large value transfer was cancelled", err)
	}
	return nil
}

func transferError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var public *Error
	if errors.As(err, &public) {
		return public
	}
	if ctx != nil {
		if public := contextError(ctx); public != nil {
			return public
		}
	}
	switch {
	case errors.Is(err, errResourceSentinel):
		return publicError(CodeResourceExhausted, "large value transfer exceeds adapter limits", nil)
	case errors.Is(err, errUnsupportedSentinel):
		return publicError(CodeUnsupportedCapability, "large value transfer capability is unsupported", nil)
	case errors.Is(err, errTransferSentinel):
		return publicError(CodeTransferFailed, "large value transfer failed", nil)
	case errors.Is(err, largevalue.ErrUnavailable), errors.Is(err, largevalue.ErrInvalidCapability):
		return publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	case errors.Is(err, largevalue.ErrInvalidMetadata):
		return publicError(CodeInvalidRequest, "large value transfer request is invalid", largevalue.ErrInvalidMetadata)
	case errors.Is(err, largevalue.ErrEgressDenied):
		return publicError(CodeEgressDenied, "large value transfer target is unavailable", largevalue.ErrEgressDenied)
	case errors.Is(err, largevalue.ErrRangeUnsatisfied):
		return publicError(CodeInvalidRequest, "large value range is invalid", largevalue.ErrRangeUnsatisfied)
	case errors.Is(err, largevalue.ErrTransferFailed):
		return publicError(CodeIntegrityFailed, "large value integrity verification failed", largevalue.ErrTransferFailed)
	default:
		return publicError(CodeTransferFailed, "large value transfer failed", nil)
	}
}

// PinnedRequest is the complete request passed to a transport after the
// adapter validates the URL and resolves its addresses. Implementations must
// dial one of ApprovedAddresses and must not perform a second DNS lookup.
type PinnedRequest struct {
	Method            string
	URL               string
	Header            http.Header
	Body              io.Reader
	ContentLength     int64
	ApprovedAddresses []netip.Addr
}

// PinnedResponse is a streaming response owned by the adapter until its body
// is closed.
type PinnedResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// PinnedTransport performs exactly one request and never follows redirects.
// It is application-owned so TLS, proxy, credential, and socket policy remain
// outside this implementation profile.
type PinnedTransport interface {
	Do(context.Context, PinnedRequest) (*PinnedResponse, error)
}

// ApplicationProvider opens application-owned sources and transactional
// sinks. Source names are application identifiers, never bearer references.
type ApplicationProvider interface {
	OpenSource(context.Context, string) (io.ReadCloser, error)
	OpenSink(context.Context, string) (ApplicationSink, error)
}

// ApplicationSink receives a streamed download and transfers cleanup
// ownership only when Commit succeeds.
type ApplicationSink interface {
	io.WriteCloser
	Commit(context.Context) error
	Abort(context.Context) error
}

type cleanupPolicy struct {
	timeout time.Duration
}

func (p cleanupPolicy) context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), p.timeout)
}
