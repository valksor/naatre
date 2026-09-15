package largevalueadapter

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

type HTTPPinnedTransportConfig struct {
	DialContext            DialContextFunc
	TLSClientConfig        *tls.Config
	ResponseHeaderTimeout  time.Duration
	MaximumResponseHeaders int64
}

// HTTPPinnedTransport is a concrete one-request transport with proxy and
// redirect behavior disabled. Its dial callback receives only an approved IP
// literal and port, so neither the transport nor the dialer resolves the
// protected target hostname again.
type HTTPPinnedTransport struct {
	dial                   DialContextFunc
	tls                    *tls.Config
	responseHeaderTimeout  time.Duration
	maximumResponseHeaders int64
}

func NewHTTPPinnedTransport(config HTTPPinnedTransportConfig) (*HTTPPinnedTransport, error) {
	if config.MaximumResponseHeaders == 0 {
		config.MaximumResponseHeaders = DefaultLimits().MaximumHeaderBytes
	}
	if config.ResponseHeaderTimeout <= 0 || config.MaximumResponseHeaders <= 0 {
		return nil, publicError(CodeInvalidConfig, "pinned HTTP transport configuration is invalid", nil)
	}
	if config.DialContext == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: -1}
		config.DialContext = dialer.DialContext
	}
	var tlsConfig *tls.Config
	if config.TLSClientConfig != nil {
		tlsConfig = config.TLSClientConfig.Clone()
	}
	return &HTTPPinnedTransport{dial: config.DialContext, tls: tlsConfig, responseHeaderTimeout: config.ResponseHeaderTimeout,
		maximumResponseHeaders: config.MaximumResponseHeaders}, nil
}

func (t *HTTPPinnedTransport) Do(ctx context.Context, request PinnedRequest) (*PinnedResponse, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	target, err := t.validateTarget(request)
	if err != nil {
		return nil, err
	}
	transport := t.transportFor(request, target)
	body := request.Body
	if body == nil {
		body = http.NoBody
	}
	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, request.URL, readerOnly{Reader: body})
	if err != nil {
		return nil, publicError(CodeInvalidRequest, "pinned HTTP request is invalid", nil)
	}
	httpRequest.Header = cloneHeaders(request.Header)
	httpRequest.ContentLength = request.ContentLength
	// RoundTrip leaves the response body owned by its caller. The returned
	// transportBody preserves that ownership while also closing idle sockets.
	response, err := transport.RoundTrip(httpRequest) //nolint:bodyclose
	if err != nil {
		transport.CloseIdleConnections()
		return nil, transferError(ctx, errTransferSentinel)
	}
	return &PinnedResponse{StatusCode: response.StatusCode, Header: response.Header.Clone(),
		Body: &transportBody{ReadCloser: response.Body, closeIdle: transport.CloseIdleConnections}}, nil
}

type pinnedTarget struct {
	host      string
	port      string
	tlsConfig *tls.Config
}

func (t *HTTPPinnedTransport) validateTarget(request PinnedRequest) (pinnedTarget, error) {
	parsed, err := url.Parse(request.URL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || len(request.ApprovedAddresses) == 0 {
		return pinnedTarget{}, publicError(CodeInvalidRequest, "pinned HTTP request is invalid", nil)
	}
	port := parsed.Port()
	if port == "" && parsed.Scheme == "https" {
		port = "443"
	} else if port == "" {
		port = "80"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return pinnedTarget{}, publicError(CodeInvalidRequest, "pinned HTTP request is invalid", nil)
	}
	tlsConfig := t.tls
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		tlsConfig = tlsConfig.Clone()
	}
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = parsed.Hostname()
	}
	return pinnedTarget{host: parsed.Hostname(), port: port, tlsConfig: tlsConfig}, nil
}

func (t *HTTPPinnedTransport) transportFor(request PinnedRequest, target pinnedTarget) *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      false,
		TLSClientConfig:        target.tlsConfig,
		TLSNextProto:           make(map[string]func(string, *tls.Conn) http.RoundTripper),
		ResponseHeaderTimeout:  t.responseHeaderTimeout,
		MaxResponseHeaderBytes: t.maximumResponseHeaders,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return t.dialApproved(ctx, network, address, target, request.ApprovedAddresses)
		},
	}
}

func (t *HTTPPinnedTransport) dialApproved(ctx context.Context, network, address string, target pinnedTarget, approved []netip.Addr) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, target.host) || port != target.port {
		return nil, publicError(CodeEgressDenied, "large value transfer target is unavailable", nil)
	}
	var lastErr error
	for _, address := range approved {
		if !address.IsValid() {
			continue
		}
		connection, dialErr := t.dial(ctx, network, net.JoinHostPort(address.String(), target.port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, publicError(CodeEgressDenied, "large value transfer target is unavailable", nil)
}

type readerOnly struct{ io.Reader }

type transportBody struct {
	io.ReadCloser
	closeIdle func()
}

func (b *transportBody) Close() error {
	err := b.ReadCloser.Close()
	b.closeIdle()
	return err
}
