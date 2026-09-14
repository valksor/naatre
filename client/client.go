package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/valksor/naatre/protocol"
)

const (
	RequestMediaType  = "application/vnd.naatre.request+json;version=1"
	ResponseMediaType = "application/vnd.naatre.response+json;version=1"
	ProblemMediaType  = "application/problem+json"
)

const (
	defaultCompressedBytes   int64 = 8 << 20
	defaultDecompressedBytes int64 = 16 << 20
	defaultRedirects               = 5
)

var errRedirectLimit = errors.New("naatre client redirect limit exceeded")

// Authenticator adds credentials to one outbound request. Implementations must
// not retain the request or expose credentials through returned errors.
type Authenticator func(context.Context, *http.Request) error

// Config configures a Client. HTTPClient is cloned, so redirect policy changes
// never mutate a caller-owned client or transport.
type Config struct {
	Endpoint             string
	HTTPClient           *http.Client
	Authenticate         Authenticator
	DecodeOptions        protocol.DecodeOptions
	MaxCompressedBytes   int64
	MaxDecompressedBytes int64
}

// Client executes bounded unary requests without importing server runtime
// packages.
type Client struct {
	endpoint             *url.URL
	httpClient           *http.Client
	authenticate         Authenticator
	decodeOptions        protocol.DecodeOptions
	maxCompressedBytes   int64
	maxDecompressedBytes int64
}

// Error is a safe, stable client failure. Error never includes remote body
// text, credentials, variables, or transport implementation details.
type Error struct {
	Code       string
	StatusCode int
	cause      error
}

func (e *Error) Error() string { return "naatre client: " + e.Code }
func (e *Error) Unwrap() error { return e.cause }

// Problem retains a bounded RFC 9457 response and its extensions.
type Problem struct {
	Type       string
	Title      string
	Status     int
	Detail     string
	Instance   string
	Code       string
	Extensions map[string]json.RawMessage
}

// Result retains the HTTP metadata and either a Naatre envelope or Problem.
// A non-2xx result is returned together with a stable Error.
type Result struct {
	StatusCode int
	Header     http.Header
	Envelope   *protocol.Response
	Problem    *Problem
}

func (r *Result) Data() (json.RawMessage, bool) {
	if r == nil || r.Envelope == nil {
		return nil, false
	}
	return r.Envelope.Data()
}

func (r *Result) Errors() []protocol.ResponseError {
	if r == nil || r.Envelope == nil {
		return nil
	}
	return r.Envelope.Errors()
}

// New validates the endpoint and installs the mandatory redirect boundary.
func New(config Config) (*Client, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, clientError("INVALID_CONFIG", 0, errors.New("invalid endpoint"))
	}
	maxCompressed := config.MaxCompressedBytes
	if maxCompressed == 0 {
		maxCompressed = defaultCompressedBytes
	}
	maxDecompressed := config.MaxDecompressedBytes
	if maxDecompressed == 0 {
		maxDecompressed = defaultDecompressedBytes
	}
	if maxCompressed < 1 || maxDecompressed < 1 {
		return nil, clientError("INVALID_CONFIG", 0, errors.New("invalid response limits"))
	}
	httpClient := http.DefaultClient
	if config.HTTPClient != nil {
		httpClient = config.HTTPClient
	}
	copy := *httpClient
	decodeOptions := config.DecodeOptions
	decodeOptions.Capabilities = maps.Clone(config.DecodeOptions.Capabilities)
	decodeOptions.Extensions = maps.Clone(config.DecodeOptions.Extensions)
	decodeOptions.IgnorableExtensionMetadata = maps.Clone(config.DecodeOptions.IgnorableExtensionMetadata)
	decodeOptions.ExtensionNamespaces = maps.Clone(config.DecodeOptions.ExtensionNamespaces)
	previousRedirect := copy.CheckRedirect
	copy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > defaultRedirects {
			return errRedirectLimit
		}
		if previousRedirect != nil {
			if err := previousRedirect(request, via); err != nil {
				return err
			}
		}
		if len(via) > 0 && !sameOrigin(request.URL, via[len(via)-1].URL) {
			stripCredentials(request.Header)
		}
		return nil
	}
	return &Client{
		endpoint: endpoint, httpClient: &copy, authenticate: config.Authenticate,
		decodeOptions: decodeOptions, maxCompressedBytes: maxCompressed, maxDecompressedBytes: maxDecompressed,
	}, nil
}

// Execute sends one unary operation and always closes the response body.
func (c *Client) Execute(ctx context.Context, operation Request) (*Result, error) {
	if c == nil || ctx == nil {
		return nil, clientError("INVALID_REQUEST", 0, errors.New("client and context are required"))
	}
	body, err := operation.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, clientError("INVALID_REQUEST", 0, err)
	}
	request.Header.Set("Content-Type", RequestMediaType)
	request.Header.Set("Accept", ResponseMediaType)
	request.Header.Set("Accept-Encoding", "gzip")
	if c.authenticate != nil {
		if err := c.authenticate(ctx, request); err != nil {
			return nil, clientError("AUTHENTICATION_FAILED", 0, nil)
		}
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, classifyTransportError(ctx, err)
	}
	if response.Body == nil {
		return nil, clientError("MALFORMED_RESPONSE", response.StatusCode, errors.New("response body is missing"))
	}
	defer func() { _ = response.Body.Close() }()
	if response.ContentLength > c.maxCompressedBytes {
		return nil, clientError("RESPONSE_LIMIT_EXCEEDED", response.StatusCode, errors.New("response exceeds compressed limit"))
	}
	mediaType, err := responseMediaType(response)
	if err != nil {
		return nil, err
	}
	payload, err := c.readResponseBody(response)
	if err != nil {
		return nil, err
	}
	result := &Result{StatusCode: response.StatusCode, Header: response.Header.Clone()}
	switch strings.ToLower(mediaType) {
	case "application/vnd.naatre.response+json":
		result.Envelope, err = protocol.DecodeResponse(payload, c.responseDecodeOptions(len(payload)))
		if err != nil {
			return result, clientError("MALFORMED_RESPONSE", response.StatusCode, err)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return result, clientError("REMOTE_NAATRE", response.StatusCode, nil)
		}
		return result, nil
	case "application/problem+json":
		result.Problem, err = decodeProblem(payload)
		if err != nil {
			return result, clientError("MALFORMED_RESPONSE", response.StatusCode, err)
		}
		return result, clientError("REMOTE_PROBLEM", response.StatusCode, nil)
	default:
		return result, clientError("UNSUPPORTED_MEDIA_TYPE", response.StatusCode, errors.New("unsupported response media type"))
	}
}

func (c *Client) readResponseBody(response *http.Response) ([]byte, error) {
	encodings := response.Header.Values("Content-Encoding")
	if len(encodings) > 1 {
		return nil, clientError("UNSUPPORTED_CONTENT_ENCODING", response.StatusCode, errors.New("duplicate content encoding"))
	}
	encoding := strings.TrimSpace(strings.ToLower(response.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return nil, clientError("UNSUPPORTED_CONTENT_ENCODING", response.StatusCode, errors.New("unsupported content encoding"))
	}
	compressed, err := readBounded(response.Body, c.maxCompressedBytes)
	if err != nil {
		return nil, clientError(codeForReadError(err), response.StatusCode, err)
	}
	if response.ContentLength > 0 && int64(len(compressed)) != response.ContentLength {
		return nil, clientError("TRUNCATED_RESPONSE", response.StatusCode, errors.New("response content length mismatch"))
	}
	if encoding == "" || encoding == "identity" {
		if int64(len(compressed)) > c.maxDecompressedBytes {
			return nil, clientError("RESPONSE_LIMIT_EXCEEDED", response.StatusCode, errResponseLimit)
		}
		return compressed, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, clientError("MALFORMED_RESPONSE", response.StatusCode, err)
	}
	defer func() { _ = reader.Close() }()
	decompressed, err := readBounded(reader, c.maxDecompressedBytes)
	if err != nil {
		return nil, clientError(codeForReadError(err), response.StatusCode, err)
	}
	return decompressed, nil
}

func (c *Client) responseDecodeOptions(payloadBytes int) protocol.DecodeOptions {
	options := c.decodeOptions
	if options.Limits.MaxBytes == 0 || options.Limits.MaxBytes > payloadBytes {
		options.Limits.MaxBytes = payloadBytes
	}
	return options
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maximum))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(payload)) < maximum {
		return payload, nil
	}
	var extra [1]byte
	count, err := io.ReadFull(reader, extra[:])
	if count > 0 {
		return nil, errResponseLimit
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read response boundary: %w", err)
	}
	return payload, nil
}

var errResponseLimit = errors.New("response limit exceeded")

func codeForReadError(err error) string {
	if errors.Is(err, errResponseLimit) {
		return "RESPONSE_LIMIT_EXCEEDED"
	}
	return "TRUNCATED_RESPONSE"
}

func decodeProblem(payload []byte) (*Problem, error) {
	if err := protocol.ValidateJSON(payload, protocol.Limits{}); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, errors.New("problem must be an object")
	}
	var known struct {
		Type     string `json:"type"`
		Title    string `json:"title"`
		Status   int    `json:"status"`
		Detail   string `json:"detail"`
		Instance string `json:"instance"`
		Code     string `json:"code"`
	}
	if err := json.Unmarshal(payload, &known); err != nil {
		return nil, err
	}
	for _, name := range []string{"type", "title", "status", "detail", "instance", "code"} {
		delete(fields, name)
	}
	return &Problem{
		Type: known.Type, Title: known.Title, Status: known.Status, Detail: known.Detail,
		Instance: known.Instance, Code: known.Code, Extensions: cloneRawMap(fields),
	}, nil
}

func responseMediaType(response *http.Response) (string, error) {
	values := response.Header.Values("Content-Type")
	if len(values) != 1 {
		return "", clientError("UNSUPPORTED_MEDIA_TYPE", response.StatusCode, errors.New("response requires one content type"))
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || !validMediaParameters(mediaType, parameters) {
		return "", clientError("UNSUPPORTED_MEDIA_TYPE", response.StatusCode, errors.New("invalid response media type"))
	}
	return mediaType, nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Hostname(), right.Hostname()) && effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if strings.EqualFold(value.Scheme, "https") {
		return "443"
	}
	return "80"
}

func stripCredentials(header http.Header) {
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-CSRF-Token", "X-XSRF-Token"} {
		header.Del(name)
	}
}

func validMediaParameters(mediaType string, parameters map[string]string) bool {
	versioned := strings.EqualFold(mediaType, "application/vnd.naatre.response+json")
	if versioned && parameters["version"] != "1" {
		return false
	}
	for name, value := range parameters {
		if !strings.EqualFold(name, "charset") && (!versioned || !strings.EqualFold(name, "version")) {
			return false
		}
		if strings.EqualFold(name, "version") && value != "1" || strings.EqualFold(name, "charset") && !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func classifyTransportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return clientError("CANCELED", 0, context.Canceled)
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return clientError("DEADLINE_EXCEEDED", 0, context.DeadlineExceeded)
	case errors.Is(err, errRedirectLimit):
		return clientError("REDIRECT_LIMIT_EXCEEDED", 0, err)
	default:
		return clientError("TRANSPORT_ERROR", 0, err)
	}
}

func clientError(code string, status int, cause error) *Error {
	failure := Error{Code: code, StatusCode: status, cause: cause}
	return &failure
}
