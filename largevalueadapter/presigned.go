package largevalueadapter

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/valksor/naatre/largevalue"
)

type PresignedConfig struct {
	Coordinator    *largevalue.Coordinator
	Transport      PinnedTransport
	Egress         *largevalue.EgressPolicy
	Limits         Limits
	CleanupTimeout time.Duration
}

// PresignedAdapter finalizes uploads by streaming an approved remote object
// back through core verification, and exports downloads with a single pinned
// PUT. It never uses http.Client redirect or DNS behavior.
type PresignedAdapter struct {
	coordinator *largevalue.Coordinator
	transport   PinnedTransport
	egress      *largevalue.EgressPolicy
	limits      Limits
	cleanup     cleanupPolicy
}

func NewPresignedAdapter(config PresignedConfig) (*PresignedAdapter, error) {
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	}
	if config.Coordinator == nil || config.Transport == nil || config.Egress == nil || !validLimits(config.Limits) || config.CleanupTimeout <= 0 {
		return nil, publicError(CodeInvalidConfig, "presigned transfer adapter configuration is invalid", nil)
	}
	return &PresignedAdapter{coordinator: config.Coordinator, transport: config.Transport, egress: config.Egress,
		limits: config.Limits, cleanup: cleanupPolicy{timeout: config.CleanupTimeout}}, nil
}

// Import fetches a presigned object with per-hop authorization, origin policy,
// DNS resolution, and pinned-address delivery, then streams it through core
// size and digest verification before protected finalization.
func (a *PresignedAdapter) Import(ctx context.Context, reference, rawURL string, headers http.Header) (err error) {
	if err := contextError(ctx); err != nil {
		return err
	}
	if len(rawURL) > a.limits.MaximumURLBytes || headerBytes(headers) > a.limits.MaximumHeaderBytes {
		return publicError(CodeResourceExhausted, "large value request exceeds adapter limits", nil)
	}
	record, err := a.coordinator.Inspect(ctx, reference, largevalue.AuthorizeUpload, "")
	if err != nil || record.Direction != largevalue.Upload || record.Profile != largevalue.PresignedProfile ||
		record.Metadata.Length > a.limits.MaximumTransferBytes || record.TransferURL == "" || rawURL != record.TransferURL {
		return publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	}
	response, err := a.get(ctx, reference, rawURL, headers)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && err == nil {
			err = publicError(CodeTransferFailed, "large value transfer failed", nil)
		}
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return publicError(CodeTransferFailed, "large value transfer failed", nil)
	}
	if length := response.Header.Get("Content-Length"); length != "" {
		parsed, parseErr := strconv.ParseInt(length, 10, 64)
		if parseErr != nil || parsed < 0 || parsed > a.limits.MaximumTransferBytes {
			return publicError(CodeResourceExhausted, "large value response exceeds adapter limits", nil)
		}
	}
	if err := a.coordinator.AcceptUpload(ctx, reference, &contextReader{ctx: ctx, reader: response.Body}); err != nil {
		return transferError(ctx, err)
	}
	return nil
}

func (a *PresignedAdapter) get(ctx context.Context, reference, rawURL string, headers http.Header) (*PinnedResponse, error) {
	current := rawURL
	requestHeaders := cloneHeaders(headers)
	for redirects := 0; ; redirects++ {
		if redirects > a.limits.MaximumRedirects {
			return nil, publicError(CodeEgressDenied, "large value transfer target is unavailable", largevalue.ErrEgressDenied)
		}
		if _, err := a.coordinator.Inspect(ctx, reference, largevalue.AuthorizeUpload, ""); err != nil {
			return nil, publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
		}
		response, err := a.getHop(ctx, current, requestHeaders)
		if err != nil {
			return nil, err
		}
		if !redirectStatus(response.StatusCode) {
			return response, nil
		}
		location := response.Header.Get("Location")
		_ = response.Body.Close()
		next, err := resolveRedirect(current, location)
		if err != nil {
			return nil, publicError(CodeEgressDenied, "large value transfer target is unavailable", largevalue.ErrEgressDenied)
		}
		if !sameOrigin(current, next) {
			stripSensitiveHeaders(requestHeaders)
		}
		current = next
	}
}

func (a *PresignedAdapter) getHop(ctx context.Context, rawURL string, headers http.Header) (*PinnedResponse, error) {
	addresses, err := a.egress.ValidateHop(ctx, rawURL)
	if err != nil {
		return nil, transferError(ctx, err)
	}
	response, err := a.transport.Do(ctx, PinnedRequest{Method: http.MethodGet, URL: rawURL, Header: cloneHeaders(headers), ApprovedAddresses: addresses})
	if err != nil {
		return nil, transferError(ctx, err)
	}
	if response == nil || response.Body == nil {
		return nil, publicError(CodeTransferFailed, "large value transfer failed", nil)
	}
	if headerBytes(response.Header) > a.limits.MaximumHeaderBytes {
		_ = response.Body.Close()
		return nil, publicError(CodeResourceExhausted, "large value response exceeds adapter limits", nil)
	}
	return response, nil
}

// Export streams finalized content to one pinned presigned PUT target. Upload
// redirects are explicitly unsupported because replay would require buffering
// or reopening protected content after a remote side effect.
func (a *PresignedAdapter) Export(ctx context.Context, reference, rawURL string, headers http.Header) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if len(rawURL) > a.limits.MaximumURLBytes || headerBytes(headers) > a.limits.MaximumHeaderBytes {
		return publicError(CodeResourceExhausted, "large value request exceeds adapter limits", nil)
	}
	record, err := a.coordinator.Inspect(ctx, reference, largevalue.AuthorizeConsume, "")
	if err != nil || record.Direction != largevalue.Download || record.Profile != largevalue.PresignedProfile ||
		record.Metadata.Length > a.limits.MaximumTransferBytes || record.TransferURL == "" || rawURL != record.TransferURL {
		return publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	}
	return transferError(ctx, a.coordinator.Consume(ctx, reference, func(copyCtx context.Context, current largevalue.Record, source io.Reader) (resultErr error) {
		if _, err := a.coordinator.Inspect(copyCtx, reference, largevalue.AuthorizeConsume, ""); err != nil {
			return largevalue.ErrUnavailable
		}
		addresses, err := a.egress.ValidateHop(copyCtx, rawURL)
		if err != nil {
			return err
		}
		requestHeaders := cloneHeaders(headers)
		requestHeaders.Set("Content-Type", current.Metadata.MediaType)
		requestHeaders.Set("Content-Length", strconv.FormatInt(current.Metadata.Length, 10))
		response, err := a.transport.Do(copyCtx, PinnedRequest{Method: http.MethodPut, URL: rawURL, Header: requestHeaders,
			Body: &contextReader{ctx: copyCtx, reader: source}, ContentLength: current.Metadata.Length, ApprovedAddresses: addresses})
		if err != nil || response == nil || response.Body == nil {
			return errTransferSentinel
		}
		defer func() {
			if closeErr := response.Body.Close(); closeErr != nil && resultErr == nil {
				resultErr = errTransferSentinel
			}
		}()
		if headerBytes(response.Header) > a.limits.MaximumHeaderBytes {
			return errResourceSentinel
		}
		if redirectStatus(response.StatusCode) {
			return errUnsupportedSentinel
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return errTransferSentinel
		}
		return nil
	}))
}

type adapterSentinel string

func (e adapterSentinel) Error() string { return string(e) }

const (
	errTransferSentinel    adapterSentinel = "transfer"
	errResourceSentinel    adapterSentinel = "resource"
	errUnsupportedSentinel adapterSentinel = "unsupported"
)

func redirectStatus(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusFound || status == http.StatusSeeOther ||
		status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect
}

func resolveRedirect(current, location string) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	reference, err := url.Parse(location)
	if err != nil || location == "" {
		return "", largevalue.ErrEgressDenied
	}
	return base.ResolveReference(reference).String(), nil
}

func sameOrigin(left, right string) bool {
	a, errA := url.Parse(left)
	b, errB := url.Parse(right)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func stripSensitiveHeaders(header http.Header) {
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Digest", "Content-Digest", "Repr-Digest"} {
		header.Del(name)
	}
}

func cloneHeaders(header http.Header) http.Header {
	if header == nil {
		return make(http.Header)
	}
	return header.Clone()
}
