package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valksor/naatre/event"
)

// EndpointSource returns the authorization-owned exact endpoint revision.
type EndpointSource interface {
	Endpoint(context.Context, string, string) (event.EndpointRegistration, bool)
}

type EndpointSourceFunc func(context.Context, string, string) (event.EndpointRegistration, bool)

func (f EndpointSourceFunc) Endpoint(ctx context.Context, id, revision string) (event.EndpointRegistration, bool) {
	return f(ctx, id, revision)
}

// SigningGeneration is one exact outbound rotation generation.
type SigningGeneration struct {
	Key       event.SigningKey
	NotBefore time.Time
	NotAfter  time.Time
	Revoked   bool
}

// KeySource selects exactly one generation for an attempt. The adapter never
// retries a signature against other overlapping secrets.
type KeySource interface {
	SigningGeneration(context.Context, event.EndpointRegistration, time.Time) (SigningGeneration, bool)
}

type KeySourceFunc func(context.Context, event.EndpointRegistration, time.Time) (SigningGeneration, bool)

func (f KeySourceFunc) SigningGeneration(ctx context.Context, endpoint event.EndpointRegistration, now time.Time) (SigningGeneration, bool) {
	return f(ctx, endpoint, now)
}

// Resolver is invoked before every connection and retry.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// NetworkResolver uses net.DefaultResolver.
type NetworkResolver struct{}

func (NetworkResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

// AttemptResponse retains only bounded response metadata used by retry policy.
type AttemptResponse struct {
	StatusCode int
	RetryAfter time.Duration
}

// Connector receives one already revalidated and pinned IP address. Concrete
// connectors must preserve the URL hostname for TLS identity while dialing
// only this address.
type Connector interface {
	Deliver(context.Context, event.EndpointRegistration, event.Message, netip.Addr, int64) (AttemptResponse, error)
}

// DialContext is the injectable connection seam used by HTTPConnector tests
// and specialized transports.
type DialContext func(context.Context, string, string) (net.Conn, error)

// HTTPConnector is a no-proxy, no-redirect HTTPS connector. It creates one
// transport per attempt so a pooled connection cannot outlive DNS validation.
type HTTPConnector struct {
	dial                  DialContext
	responseHeaderTimeout time.Duration
	maximumRetryAfter     time.Duration
}

type HTTPConnectorConfig struct {
	DialContext           DialContext
	ResponseHeaderTimeout time.Duration
	MaximumRetryAfter     time.Duration
}

func NewHTTPConnector(config HTTPConnectorConfig) (*HTTPConnector, error) {
	if config.ResponseHeaderTimeout <= 0 || config.MaximumRetryAfter <= 0 {
		return nil, publicError(CodeInvalidConfig, "webhook HTTP connector configuration is invalid", nil)
	}
	if config.DialContext == nil {
		dialer := &net.Dialer{}
		config.DialContext = dialer.DialContext
	}
	return &HTTPConnector{dial: config.DialContext, responseHeaderTimeout: config.ResponseHeaderTimeout, maximumRetryAfter: config.MaximumRetryAfter}, nil
}

func (c *HTTPConnector) Deliver(ctx context.Context, endpoint event.EndpointRegistration, message event.Message, address netip.Addr, maximumResponseBytes int64) (AttemptResponse, error) {
	if c == nil || c.dial == nil || maximumResponseBytes < 1 || message.TargetURI != endpoint.URL || !address.IsValid() {
		return AttemptResponse{}, publicError(CodeInvalidConfig, "webhook HTTP attempt is invalid", nil)
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil {
		return AttemptResponse{}, publicError(CodeInvalidRecord, "webhook endpoint is invalid", nil)
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	expectedAuthority := net.JoinHostPort(parsed.Hostname(), port)
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: c.responseHeaderTimeout,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(dialCtx context.Context, network, authority string) (net.Conn, error) {
			if network != "tcp" && network != "tcp4" && network != "tcp6" || !sameAuthority(authority, expectedAuthority) {
				return nil, errors.New("webhook connector refused an unexpected authority")
			}
			return c.dial(dialCtx, network, net.JoinHostPort(address.String(), port))
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, bytes.NewReader(message.Body))
	if err != nil {
		return AttemptResponse{}, publicError(CodeInvalidRecord, "webhook request is invalid", nil)
	}
	request.Header.Set("Content-Type", message.ContentType)
	request.Header.Set("Content-Encoding", message.ContentEncoding)
	request.Header.Set("Content-Digest", message.ContentDigest)
	request.Header.Set("Naatre-Webhook-Id", message.DeliveryID)
	request.Header.Set("Naatre-Webhook-Timestamp", message.Timestamp)
	request.Header.Set("Naatre-Webhook-Audience", message.Audience)
	request.Header.Set("Signature-Input", message.SignatureInput)
	request.Header.Set("Signature", message.Signature)
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return AttemptResponse{}, publicError(CodeCancelled, "webhook attempt was cancelled", ctx.Err())
		}
		return AttemptResponse{}, publicError(CodeTransport, "webhook endpoint is unavailable", nil)
	}
	consumed, err := io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseBytes+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		return AttemptResponse{}, publicError(CodeTransport, "webhook response could not be consumed", nil)
	}
	if consumed > maximumResponseBytes {
		return AttemptResponse{}, publicError(CodeResourceExhausted, "webhook response exceeds configured limits", nil)
	}
	return AttemptResponse{StatusCode: response.StatusCode, RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC(), c.maximumRetryAfter)}, nil
}

func sameAuthority(actual, expected string) bool {
	actualHost, actualPort, err := net.SplitHostPort(actual)
	if err != nil {
		return false
	}
	expectedHost, expectedPort, err := net.SplitHostPort(expected)
	return err == nil && strings.EqualFold(actualHost, expectedHost) && actualPort == expectedPort
}

func parseRetryAfter(value string, now time.Time, maximum time.Duration) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
		delay := time.Duration(seconds) * time.Second
		if delay <= maximum {
			return delay
		}
		return 0
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) || when.Sub(now) > maximum {
		return 0
	}
	return when.Sub(now)
}

// PermitLimiter bounds concurrent attempts and bytes by tenant and endpoint.
type PermitLimiter interface {
	Acquire(context.Context, string, string, int) (release func(), err error)
}

type limiterUse struct {
	attempts int
	bytes    int
}

// MemoryLimiter is a process-local tenant-and-endpoint limiter. Distributed
// deployments must supply separately evidenced shared rate limiting.
type MemoryLimiter struct {
	mu             sync.Mutex
	uses           map[string]limiterUse
	maxConcurrency int
	maxBytes       int
}

func NewMemoryLimiter(maxConcurrency, maxBytes int) (*MemoryLimiter, error) {
	if maxConcurrency < 1 || maxBytes < 1 {
		return nil, publicError(CodeInvalidConfig, "webhook limiter configuration is invalid", nil)
	}
	return &MemoryLimiter{uses: make(map[string]limiterUse), maxConcurrency: maxConcurrency, maxBytes: maxBytes}, nil
}

func (l *MemoryLimiter) Acquire(ctx context.Context, tenant, endpoint string, bytes int) (func(), error) {
	if l == nil || !validText(tenant, 128) || !validText(endpoint, 128) || bytes < 1 || bytes > l.maxBytes {
		return nil, publicError(CodeResourceExhausted, "webhook attempt exceeds configured capacity", nil)
	}
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	key := tenant + "\x00" + endpoint
	l.mu.Lock()
	use := l.uses[key]
	if use.attempts >= l.maxConcurrency || use.bytes > l.maxBytes-bytes {
		l.mu.Unlock()
		return nil, publicError(CodeResourceExhausted, "webhook endpoint capacity is exhausted", nil)
	}
	use.attempts++
	use.bytes += bytes
	l.uses[key] = use
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			current := l.uses[key]
			current.attempts--
			current.bytes -= bytes
			if current.attempts == 0 {
				delete(l.uses, key)
			} else {
				l.uses[key] = current
			}
			l.mu.Unlock()
		})
	}, nil
}

// Dispatcher performs one finite recovery and delivery pass. Applications own
// process scheduling, shutdown, endpoint/key registries, and audit hooks.
type Dispatcher struct {
	Store     *SQLiteStore
	Endpoints EndpointSource
	Keys      KeySource
	Resolver  Resolver
	Connector Connector
	Limiter   PermitLimiter
	WorkerID  string
	MaxBatch  int
	Now       func() time.Time
	Validity  time.Duration
}

func (d Dispatcher) RunOnce(ctx context.Context) ([]event.DeliveryObservation, error) {
	if d.Store == nil || d.Endpoints == nil || d.Keys == nil || d.Resolver == nil || d.Connector == nil || d.Limiter == nil ||
		!validText(d.WorkerID, 128) || d.MaxBatch < 1 || d.MaxBatch > d.Store.config.Limits.MaxDispatchBatch ||
		d.Now == nil || d.Validity <= 0 || d.Validity > event.MaximumSignatureValidity {
		return nil, publicError(CodeInvalidConfig, "webhook dispatcher configuration is invalid", nil)
	}
	if _, err := d.Store.RecoverExpired(ctx, d.Store.config.Limits.MaxRecoveryBatch); err != nil {
		return nil, err
	}
	observations := make([]event.DeliveryObservation, 0, d.MaxBatch)
	for len(observations) < d.MaxBatch {
		if err := contextFailure(ctx); err != nil {
			return observations, err
		}
		delivery, found, err := d.Store.Claim(ctx, d.WorkerID)
		if err != nil || !found {
			return observations, err
		}
		err = d.dispatch(ctx, delivery)
		loaded, loadErr := d.Store.Load(context.WithoutCancel(ctx), delivery.Record.DeliveryID)
		if loadErr == nil {
			observations = append(observations, Observation(loaded))
		}
		if err != nil && errors.Is(err, context.Canceled) {
			return observations, err
		}
		if loadErr != nil {
			return observations, loadErr
		}
	}
	return observations, nil
}

func (d Dispatcher) dispatch(ctx context.Context, delivery Delivery) error {
	endpoint, found := d.Endpoints.Endpoint(ctx, delivery.Record.EndpointID, delivery.Record.EndpointRevision)
	if err := contextFailure(ctx); err != nil {
		return d.recordAttemptFailure(ctx, delivery, err)
	}
	if !found || endpoint.ID != delivery.Record.EndpointID || endpoint.Revision != delivery.Record.EndpointRevision || endpoint.Tenant != delivery.Record.Tenant ||
		event.ValidateEndpointRegistration(endpoint) != nil || endpoint.Status != event.EndpointActive {
		return d.Store.Fail(context.WithoutCancel(ctx), delivery, CodeEndpointRevoked, 0, false, 0)
	}
	address, err := resolvePinned(ctx, d.Resolver, endpoint)
	if err != nil {
		return d.recordAttemptFailure(ctx, delivery, err)
	}
	now := d.Now().UTC().Truncate(time.Second)
	generation, found := d.Keys.SigningGeneration(ctx, endpoint, now)
	if err := contextFailure(ctx); err != nil {
		return d.recordAttemptFailure(ctx, delivery, err)
	}
	if !found || generation.Revoked || generation.Key.ID == "" || now.Before(generation.NotBefore) || !now.Before(generation.NotAfter) || generation.NotAfter.Sub(now) < d.Validity {
		return d.Store.Fail(context.WithoutCancel(ctx), delivery, CodeKeyRevoked, 0, false, 0)
	}
	message, err := event.Sign(event.Message{
		Method: http.MethodPost, TargetURI: endpoint.URL, ContentType: delivery.ContentType,
		ContentEncoding: delivery.ContentEncoding, DeliveryID: delivery.Record.DeliveryID,
		Audience: endpoint.ID, Body: append([]byte(nil), delivery.Body...),
	}, generation.Key, now, now.Add(d.Validity))
	if err != nil {
		return d.Store.Fail(context.WithoutCancel(ctx), delivery, CodeKeyRevoked, 0, false, 0)
	}
	release, err := d.Limiter.Acquire(ctx, delivery.Record.Tenant, delivery.Record.EndpointID, len(delivery.Body))
	if err != nil {
		return d.recordAttemptFailure(ctx, delivery, err)
	}
	defer release()
	response, err := d.Connector.Deliver(ctx, endpoint, message, address, d.Store.config.Limits.MaxResponseBytes)
	if err != nil {
		return d.recordAttemptFailure(ctx, delivery, err)
	}
	code, retryable, success := classifyStatus(response.StatusCode)
	if success {
		return d.Store.Succeed(context.WithoutCancel(ctx), delivery.Record.DeliveryID, delivery.Fence, response.StatusCode)
	}
	return d.Store.Fail(context.WithoutCancel(ctx), delivery, code, response.StatusCode, retryable, response.RetryAfter)
}

func (d Dispatcher) recordAttemptFailure(ctx context.Context, delivery Delivery, err error) error {
	code := ErrorCode(err)
	retryable := code == CodeTransport || code == CodeHTTPTransient || code == CodeCancelled
	if code == "" {
		code = CodeTransport
		retryable = true
	}
	if failErr := d.Store.Fail(context.WithoutCancel(ctx), delivery, code, 0, retryable, 0); failErr != nil {
		return failErr
	}
	if ctx.Err() != nil {
		return publicError(CodeCancelled, "webhook attempt was cancelled", ctx.Err())
	}
	return nil
}

func resolvePinned(ctx context.Context, resolver Resolver, endpoint event.EndpointRegistration) (netip.Addr, error) {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil {
		return netip.Addr{}, publicError(CodeDNSRebinding, "webhook endpoint resolution is not approved", nil)
	}
	host := parsed.Hostname()
	addresses := []netip.Addr{}
	if literal, err := netip.ParseAddr(host); err == nil {
		addresses = append(addresses, literal.Unmap())
	} else {
		resolved, err := resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			if ctx.Err() != nil {
				return netip.Addr{}, publicError(CodeCancelled, "webhook resolution was cancelled", ctx.Err())
			}
			return netip.Addr{}, publicError(CodeTransport, "webhook endpoint could not be resolved", nil)
		}
		for _, address := range resolved {
			addresses = append(addresses, address.Unmap())
		}
	}
	current := endpoint
	current.ResolvedAddresses = addresses
	if event.ValidateEndpointRegistration(current) != nil {
		return netip.Addr{}, publicError(CodeDNSRebinding, "webhook endpoint resolution is not approved", nil)
	}
	pinned := make(map[netip.Addr]struct{}, len(endpoint.ResolvedAddresses))
	for _, address := range endpoint.ResolvedAddresses {
		pinned[address.Unmap()] = struct{}{}
	}
	for _, address := range addresses {
		if _, ok := pinned[address]; !ok {
			return netip.Addr{}, publicError(CodeDNSRebinding, "webhook endpoint resolution changed", nil)
		}
	}
	slices.SortFunc(addresses, func(left, right netip.Addr) int { return left.Compare(right) })
	return addresses[0], nil
}

func classifyStatus(status int) (code string, retryable, success bool) {
	switch {
	case status >= 200 && status < 300:
		return "", false, true
	case status >= 300 && status < 400:
		return CodeRedirect, false, false
	case status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500 && status <= 599:
		return CodeHTTPTransient, true, false
	default:
		return CodeHTTPPermanent, false, false
	}
}

func (r AttemptResponse) String() string {
	return fmt.Sprintf("status=%d retry-after=%s", r.StatusCode, r.RetryAfter)
}
