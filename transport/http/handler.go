package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	stdhttp "net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/valksor/naatre/protocol"
	naatrecbor "github.com/valksor/naatre/protocol/cbor"
	"github.com/valksor/naatre/runtime"
)

const (
	RequestMediaType      = "application/vnd.naatre.request+json;version=1"
	ResponseMediaType     = "application/vnd.naatre.response+json;version=1"
	ProblemMediaType      = "application/problem+json"
	DefaultPath           = "/v1/execute"
	CBORRequestMediaType  = naatrecbor.RequestMediaType
	CBORResponseMediaType = naatrecbor.ResponseMediaType
	CBORCapability        = naatrecbor.Profile
	CBORCodecRevision     = naatrecbor.CodecRevision
)

var errLimitExceeded = errors.New("HTTP body limit exceeded")

// Limits bounds carrier work independently from protocol and runtime limits.
// Zero fields select the finite defaults returned by DefaultLimits.
type Limits struct {
	MaxRequestTargetBytes       int
	MaxCompressedRequestBytes   int64
	MaxDecompressedRequestBytes int64
	MaxResponseBytes            int64
	MaxCompressedResponseBytes  int64
	CompressionMinBytes         int
	MaxRequestDuration          time.Duration
}

// DefaultLimits returns the core.http-1 reference adapter limits.
func DefaultLimits() Limits {
	return Limits{
		MaxRequestTargetBytes:       8 << 10,
		MaxCompressedRequestBytes:   8 << 20,
		MaxDecompressedRequestBytes: 16 << 20,
		MaxResponseBytes:            16 << 20,
		MaxCompressedResponseBytes:  8 << 20,
		CompressionMinBytes:         1 << 10,
		MaxRequestDuration:          30 * time.Second,
	}
}

// CORS configures an exact-origin deployment policy. The zero value disables
// cross-origin requests and preflights.
type CORS struct {
	AllowedOrigins   []string
	AllowedHeaders   []string
	AllowCredentials bool
}

// AuthenticateFunc authenticates transport credentials before request body
// decoding. It may return a derived context containing trusted principal data.
// Its error is intentionally never exposed to the peer.
type AuthenticateFunc func(context.Context, *stdhttp.Request) (context.Context, error)

// Executor runs one strictly decoded request. It must return only public
// runtime errors; the adapter additionally replaces unknown codes and messages
// with safe stable failures before serialization.
type Executor func(context.Context, *protocol.Request) runtime.Outcome

// Config owns the deployment decisions needed by the router-free handler.
type Config struct {
	Path            string
	Executor        Executor
	Decode          protocol.DecodeOptions
	Limits          Limits
	Authenticate    AuthenticateFunc
	WWWAuthenticate string
	CORS            CORS
	RequestID       func() string
	Shutdown        <-chan struct{}
	EnableCBOR      bool
	CBORLimits      naatrecbor.Limits
}

// Handler is a router-free standard-library HTTP adapter for unary POST
// execution. It is safe for concurrent use after construction.
type Handler struct {
	path            string
	executor        Executor
	decode          protocol.DecodeOptions
	limits          Limits
	authenticate    AuthenticateFunc
	wwwAuthenticate string
	cors            CORS
	requestID       func() string
	shutdown        <-chan struct{}
	cbor            bool
	cborLimits      naatrecbor.Limits
}

// NewHandler validates config and constructs a net/http handler without
// registering routes in a caller-owned mux.
func NewHandler(config Config) (*Handler, error) {
	if config.Executor == nil {
		return nil, errors.New("HTTP executor is required")
	}
	if config.Authenticate != nil && config.WWWAuthenticate == "" {
		return nil, errors.New("HTTP authentication requires a WWW-Authenticate challenge")
	}
	limits, err := resolveLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	path := config.Path
	if path == "" {
		path = DefaultPath
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return nil, errors.New("HTTP path must be an absolute path without query or fragment")
	}
	cors, err := resolveCORS(config.CORS)
	if err != nil {
		return nil, err
	}
	requestID := config.RequestID
	if requestID == nil {
		requestID = randomRequestID
	}
	decode := config.Decode
	if config.EnableCBOR {
		capabilities := make(map[string]bool, len(decode.Capabilities)+1)
		for capability, supported := range decode.Capabilities {
			capabilities[capability] = supported
		}
		capabilities[CBORCapability] = true
		decode.Capabilities = capabilities
	}
	return &Handler{
		path: path, executor: config.Executor, decode: decode, limits: limits,
		authenticate: config.Authenticate, wwwAuthenticate: config.WWWAuthenticate,
		cors: cors, requestID: requestID, shutdown: config.Shutdown,
		cbor: config.EnableCBOR, cborLimits: config.CBORLimits,
	}, nil
}

// RuntimeExecutor adapts an immutable runtime snapshot to the HTTP execution
// boundary. Planning failures become a public validation outcome.
func RuntimeExecutor(snapshot runtime.Snapshot, prepare runtime.PrepareOptions, execute runtime.ExecuteOptions) Executor {
	return func(ctx context.Context, request *protocol.Request) runtime.Outcome {
		plan, err := runtime.PrepareWithOptions(snapshot, request, prepare)
		if err != nil {
			return runtime.Outcome{Errors: []runtime.ExecutionError{{Code: "VALIDATION_FAILED", Message: "operation validation failed"}}}
		}
		return plan.ExecuteWith(ctx, execute)
	}
}

func (handler *Handler) ServeHTTP(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	tracked := &trackingWriter{ResponseWriter: writer}
	setVary(tracked.Header(), "Accept", "Accept-Encoding", "Naatre-Capabilities")
	tracked.Header().Set("Cache-Control", "no-store")
	if request.URL.Path != handler.path {
		handler.writeProblem(tracked, stdhttp.StatusNotFound, "NOT_FOUND")
		return
	}
	if len(request.RequestURI) > handler.limits.MaxRequestTargetBytes {
		handler.writeProblem(tracked, stdhttp.StatusRequestURITooLong, "URI_TOO_LONG")
		return
	}
	if request.Method == stdhttp.MethodOptions {
		handler.handlePreflight(tracked, request)
		return
	}
	if request.Method != stdhttp.MethodPost {
		tracked.Header().Set("Allow", "POST, OPTIONS")
		handler.writeProblem(tracked, stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	if !handler.applyCORS(tracked, request) {
		return
	}
	options, status, code := handler.validatePostRequest(request)
	if code != "" {
		handler.writeProblem(tracked, status, code)
		return
	}
	handler.servePost(tracked, request, options)
}

type postRequestOptions struct {
	encoding       string
	requestFormat  responseRepresentation
	responseFormat responseRepresentation
	deadline       time.Duration
}

func (handler *Handler) validatePostRequest(request *stdhttp.Request) (postRequestOptions, int, string) {
	if problem := validateSingletonHeaders(request.Header); problem != "" {
		return postRequestOptions{}, stdhttp.StatusBadRequest, problem
	}
	if request.Header.Get("Naatre-Principal") != "" {
		return postRequestOptions{}, stdhttp.StatusBadRequest, "UNTRUSTED_IDENTITY_HEADER"
	}
	cborAdvertised := handler.cbor && advertisesCapability(request.Header.Values("Naatre-Capabilities"), CBORCapability)
	requestFormat, status, code := validateRequestMedia(request.Header.Get("Content-Type"), cborAdvertised)
	if code != "" {
		return postRequestOptions{}, status, code
	}
	responseFormat, status, code := validateAccept(request.Header.Values("Accept"), cborAdvertised)
	if code != "" {
		return postRequestOptions{}, status, code
	}
	if !validAcceptEncoding(request.Header.Values("Accept-Encoding")) {
		return postRequestOptions{}, stdhttp.StatusBadRequest, "MALFORMED_HEADER"
	}
	encoding, status, code := validateContentEncoding(request.Header.Get("Content-Encoding"))
	if code != "" {
		return postRequestOptions{}, status, code
	}
	deadline, status, code := handler.requestDeadline(request)
	if code != "" {
		return postRequestOptions{}, status, code
	}
	if channelClosed(handler.shutdown) {
		return postRequestOptions{}, stdhttp.StatusServiceUnavailable, "OVERLOADED"
	}
	return postRequestOptions{encoding: encoding, requestFormat: requestFormat, responseFormat: responseFormat, deadline: deadline}, 0, ""
}

func (handler *Handler) servePost(writer stdhttp.ResponseWriter, request *stdhttp.Request, options postRequestOptions) {
	ctx, cancel := handler.context(request.Context(), options.deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		handler.writeProblem(writer, stdhttp.StatusServiceUnavailable, "OVERLOADED")
		return
	}
	if handler.authenticate != nil {
		authenticated, err := handler.authenticate(ctx, request)
		if err != nil || authenticated == nil {
			if handler.wwwAuthenticate != "" {
				writer.Header().Set("WWW-Authenticate", handler.wwwAuthenticate)
			}
			handler.writeProblem(writer, stdhttp.StatusUnauthorized, "UNAUTHENTICATED")
			return
		}
		ctx = authenticated
	}
	decoded, status, code := handler.decodePost(ctx, request, options.encoding, options.requestFormat)
	if code != "" {
		handler.writeProblem(writer, status, code)
		return
	}
	if decoded == nil {
		return
	}
	if !matchingCapabilities(request.Header.Values("Naatre-Capabilities"), decoded.Capabilities()) {
		handler.writeProblem(writer, stdhttp.StatusBadRequest, "MALFORMED_HEADER")
		return
	}
	outcome := handler.executor(ctx, decoded)
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		outcome = runtime.Outcome{Errors: []runtime.ExecutionError{{Code: "RESOURCE_EXHAUSTED", Message: "request deadline exceeded"}}}
	}
	if status, code := transportOutcomeProblem(outcome); code != "" {
		handler.writeProblem(writer, status, code)
		return
	}
	handler.writeOutcome(writer, request, decoded, outcome, options.responseFormat)
}

func (handler *Handler) decodePost(ctx context.Context, request *stdhttp.Request, encoding string, representation responseRepresentation) (*protocol.Request, int, string) {
	body, err := readRequestBody(ctx, request.Body, request.ContentLength, encoding, handler.limits)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, stdhttp.StatusRequestTimeout, "REQUEST_TIMEOUT"
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, 0, ""
	}
	if errors.Is(err, errLimitExceeded) {
		return nil, stdhttp.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE"
	}
	if err != nil {
		return nil, stdhttp.StatusBadRequest, malformedRepresentationCode(representation)
	}
	if representation == representationCBOR {
		body, err = naatrecbor.DecodeJSONContext(ctx, body, handler.cborLimits)
		if err != nil {
			return nil, stdhttp.StatusBadRequest, "MALFORMED_CBOR"
		}
	}
	decoded, err := protocol.DecodeRequest(body, handler.decode)
	if err != nil {
		return nil, stdhttp.StatusBadRequest, malformedRepresentationCode(representation)
	}
	return decoded, 0, ""
}

func malformedRepresentationCode(representation responseRepresentation) string {
	if representation == representationCBOR {
		return "MALFORMED_CBOR"
	}
	return "MALFORMED_JSON"
}

func resolveLimits(config Limits) (Limits, error) {
	resolved := config
	defaults := DefaultLimits()
	if resolved.MaxRequestTargetBytes == 0 {
		resolved.MaxRequestTargetBytes = defaults.MaxRequestTargetBytes
	}
	if resolved.MaxCompressedRequestBytes == 0 {
		resolved.MaxCompressedRequestBytes = defaults.MaxCompressedRequestBytes
	}
	if resolved.MaxDecompressedRequestBytes == 0 {
		resolved.MaxDecompressedRequestBytes = defaults.MaxDecompressedRequestBytes
	}
	if resolved.MaxResponseBytes == 0 {
		resolved.MaxResponseBytes = defaults.MaxResponseBytes
	}
	if resolved.MaxCompressedResponseBytes == 0 {
		resolved.MaxCompressedResponseBytes = defaults.MaxCompressedResponseBytes
	}
	if resolved.CompressionMinBytes == 0 {
		resolved.CompressionMinBytes = defaults.CompressionMinBytes
	}
	if resolved.MaxRequestDuration == 0 {
		resolved.MaxRequestDuration = defaults.MaxRequestDuration
	}
	if resolved.MaxRequestTargetBytes < 1 || resolved.MaxCompressedRequestBytes < 1 || resolved.MaxDecompressedRequestBytes < 1 || resolved.MaxResponseBytes < 1 || resolved.MaxCompressedResponseBytes < 1 || resolved.CompressionMinBytes < 0 || resolved.MaxRequestDuration < 0 {
		return Limits{}, errors.New("HTTP limits must be positive")
	}
	return resolved, nil
}

func resolveCORS(config CORS) (CORS, error) {
	result := CORS{AllowedOrigins: slices.Clone(config.AllowedOrigins), AllowedHeaders: slices.Clone(config.AllowedHeaders), AllowCredentials: config.AllowCredentials}
	seen := make(map[string]bool, len(result.AllowedOrigins))
	for _, origin := range result.AllowedOrigins {
		if origin == "" || strings.ContainsAny(origin, "\r\n") || seen[origin] || (origin == "*" && result.AllowCredentials) {
			return CORS{}, errors.New("invalid CORS origin policy")
		}
		seen[origin] = true
	}
	for _, header := range result.AllowedHeaders {
		if header == "" || strings.ContainsAny(header, "\r\n,") {
			return CORS{}, errors.New("invalid CORS header policy")
		}
	}
	return result, nil
}

func (handler *Handler) context(parent context.Context, requested time.Duration) (context.Context, context.CancelFunc) {
	duration := handler.limits.MaxRequestDuration
	if requested > 0 && (duration == 0 || requested < duration) {
		duration = requested
	}
	ctx, cancel := context.WithTimeout(parent, duration)
	if handler.shutdown == nil {
		return ctx, cancel
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-handler.shutdown:
			cancel()
		case <-done:
		}
	}()
	return ctx, func() { close(done); cancel() }
}

func (handler *Handler) requestDeadline(request *stdhttp.Request) (time.Duration, int, string) {
	value := request.Header.Get("Naatre-Timeout-Ms")
	if value == "" {
		return 0, 0, ""
	}
	if strings.Trim(value, "0123456789") != "" || value == "0" {
		return 0, stdhttp.StatusBadRequest, "INVALID_TIMEOUT"
	}
	milliseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || milliseconds < 1 {
		return 0, stdhttp.StatusBadRequest, "INVALID_TIMEOUT"
	}
	maximumMilliseconds := handler.limits.MaxRequestDuration.Milliseconds()
	if maximumMilliseconds > 0 && milliseconds > maximumMilliseconds {
		return handler.limits.MaxRequestDuration, 0, ""
	}
	return time.Duration(milliseconds) * time.Millisecond, 0, ""
}

func validateSingletonHeaders(header stdhttp.Header) string {
	for _, name := range []string{"Content-Type", "Content-Encoding", "Authorization", "Naatre-Timeout-Ms", "Naatre-Schema", "Naatre-Tenant"} {
		values := header.Values(name)
		if len(values) > 1 || (len(values) == 1 && strings.Contains(values[0], ",")) {
			return "MALFORMED_HEADER"
		}
	}
	return ""
}

type responseRepresentation uint8

const (
	representationJSON responseRepresentation = iota
	representationCBOR
)

func validateRequestMedia(value string, cborAllowed bool) (responseRepresentation, int, string) {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil {
		return representationJSON, stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"
	}
	switch strings.ToLower(mediaType) {
	case "application/vnd.naatre.request+json":
		if parameters["version"] != "1" {
			return representationJSON, stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"
		}
		for name, parameter := range parameters {
			if name != "version" && (name != "charset" || !strings.EqualFold(parameter, "utf-8")) {
				return representationJSON, stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"
			}
		}
		return representationJSON, 0, ""
	case "application/vnd.naatre.request+cbor":
		if !cborAllowed || parameters["version"] != "1" || parameters["profile"] != CBORCapability || len(parameters) != 2 {
			return representationJSON, stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"
		}
		return representationCBOR, 0, ""
	default:
		return representationJSON, stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"
	}
}

func validateAccept(lines []string, cborAllowed bool) (responseRepresentation, int, string) {
	if len(lines) == 0 {
		return representationJSON, 0, ""
	}
	bestSpecificity, bestQuality := -1, -1.0
	bestRepresentation := representationJSON
	for _, line := range lines {
		for _, item := range strings.Split(line, ",") {
			representation, specificity, quality, compatible, err := acceptPreference(item, cborAllowed)
			if err != nil {
				return representationJSON, stdhttp.StatusBadRequest, "MALFORMED_HEADER"
			}
			if compatible && (quality > bestQuality || (quality == bestQuality && specificity > bestSpecificity)) {
				bestRepresentation, bestSpecificity, bestQuality = representation, specificity, quality
			}
		}
	}
	if bestSpecificity < 0 || bestQuality <= 0 {
		return representationJSON, stdhttp.StatusNotAcceptable, "NOT_ACCEPTABLE"
	}
	return bestRepresentation, 0, ""
}

func acceptPreference(item string, cborAllowed bool) (responseRepresentation, int, float64, bool, error) {
	mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(item))
	if err != nil {
		return representationJSON, 0, 0, false, err
	}
	quality, err := qualityParameter(parameters)
	if err != nil {
		return representationJSON, 0, 0, false, err
	}
	switch strings.ToLower(mediaType) {
	case "application/vnd.naatre.response+json":
		for name := range parameters {
			if name != "q" && name != "version" {
				return representationJSON, 0, quality, false, nil
			}
		}
		version, present := parameters["version"]
		return representationJSON, 2, quality, !present || version == "1", nil
	case "application/vnd.naatre.response+cbor":
		for name := range parameters {
			if name != "q" && name != "version" && name != "profile" {
				return representationCBOR, 0, quality, false, nil
			}
		}
		return representationCBOR, 2, quality, cborAllowed && parameters["version"] == "1" && parameters["profile"] == CBORCapability, nil
	case "application/*":
		return representationJSON, 1, quality, len(parameters) == boolInt(parameters["q"] != ""), nil
	case "*/*":
		return representationJSON, 0, quality, len(parameters) == boolInt(parameters["q"] != ""), nil
	default:
		return representationJSON, 0, quality, false, nil
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func qualityParameter(parameters map[string]string) (float64, error) {
	value, present := parameters["q"]
	if !present {
		return 1, nil
	}
	quality, err := strconv.ParseFloat(value, 64)
	if err != nil || quality < 0 || quality > 1 {
		return 0, errors.New("invalid quality parameter")
	}
	return quality, nil
}

func validateContentEncoding(value string) (string, int, string) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "identity" {
		return "identity", 0, ""
	}
	if value == "gzip" {
		return value, 0, ""
	}
	return "", stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_CONTENT_ENCODING"
}

func readRequestBody(ctx context.Context, body io.ReadCloser, contentLength int64, encoding string, limits Limits) ([]byte, error) {
	if body == nil {
		body = stdhttp.NoBody
	}
	if contentLength > limits.MaxCompressedRequestBytes {
		return nil, errLimitExceeded
	}
	stop := context.AfterFunc(ctx, func() { _ = body.Close() })
	defer stop()
	compressed := &boundedReader{reader: body, remaining: limits.MaxCompressedRequestBytes}
	var reader io.Reader = compressed
	if encoding == "gzip" {
		buffered := bufio.NewReader(compressed)
		gzipReader, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, err
		}
		gzipReader.Multistream(false)
		decompressed, err := readBounded(ctx, gzipReader, limits.MaxDecompressedRequestBytes)
		closeErr := gzipReader.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if _, err := buffered.Peek(1); err == nil {
			return nil, errors.New("trailing gzip member")
		} else if !errors.Is(err, io.EOF) && !errors.Is(err, errLimitExceeded) {
			return nil, err
		}
		if compressed.exceeded {
			return nil, errLimitExceeded
		}
		return decompressed, nil
	}
	return readBounded(ctx, reader, min(limits.MaxCompressedRequestBytes, limits.MaxDecompressedRequestBytes))
}

type boundedReader struct {
	reader    io.Reader
	remaining int64
	exceeded  bool
}

func (reader *boundedReader) Read(payload []byte) (int, error) {
	if reader.remaining < 0 {
		reader.exceeded = true
		return 0, errLimitExceeded
	}
	if int64(len(payload)) > reader.remaining+1 {
		payload = payload[:reader.remaining+1]
	}
	n, err := reader.reader.Read(payload)
	reader.remaining -= int64(n)
	if reader.remaining < 0 {
		reader.exceeded = true
		return n, errLimitExceeded
	}
	return n, err
}

func readBounded(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	var output bytes.Buffer
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			if int64(output.Len()+n) > limit {
				return nil, errLimitExceeded
			}
			_, _ = output.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			return output.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func matchingCapabilities(lines, envelope []string) bool {
	if len(lines) == 0 {
		return true
	}
	values := make([]string, 0)
	seen := make(map[string]bool)
	for _, line := range lines {
		for _, value := range strings.Split(line, ",") {
			value = strings.TrimSpace(value)
			if value == "" || seen[value] {
				return false
			}
			seen[value] = true
			values = append(values, value)
		}
	}
	slices.Sort(values)
	wanted := slices.Clone(envelope)
	slices.Sort(wanted)
	return slices.Equal(values, wanted)
}

func advertisesCapability(lines []string, capability string) bool {
	for _, line := range lines {
		for _, value := range strings.Split(line, ",") {
			if strings.TrimSpace(value) == capability {
				return true
			}
		}
	}
	return false
}

func (handler *Handler) applyCORS(writer stdhttp.ResponseWriter, request *stdhttp.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	setVary(writer.Header(), "Origin")
	if !handler.originAllowed(origin) {
		handler.writeProblem(writer, stdhttp.StatusForbidden, "CORS_FORBIDDEN")
		return false
	}
	writer.Header().Set("Access-Control-Allow-Origin", origin)
	if handler.cors.AllowCredentials {
		writer.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	return true
}

func (handler *Handler) handlePreflight(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	origin := request.Header.Get("Origin")
	method := request.Header.Get("Access-Control-Request-Method")
	if origin == "" || method == "" || !handler.originAllowed(origin) || method != stdhttp.MethodPost {
		handler.writeProblem(writer, stdhttp.StatusForbidden, "CORS_FORBIDDEN")
		return
	}
	setVary(writer.Header(), "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
	writer.Header().Set("Access-Control-Allow-Origin", origin)
	writer.Header().Set("Access-Control-Allow-Methods", "POST")
	allowedHeaders := handler.cors.AllowedHeaders
	if len(allowedHeaders) == 0 {
		allowedHeaders = []string{"Authorization", "Content-Type", "Naatre-Capabilities", "Naatre-Timeout-Ms"}
	}
	if !headersAllowed(request.Header.Get("Access-Control-Request-Headers"), allowedHeaders) {
		handler.writeProblem(writer, stdhttp.StatusForbidden, "CORS_FORBIDDEN")
		return
	}
	writer.Header().Set("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))
	if handler.cors.AllowCredentials {
		writer.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	writer.WriteHeader(stdhttp.StatusNoContent)
}

func (handler *Handler) originAllowed(origin string) bool {
	for _, candidate := range handler.cors.AllowedOrigins {
		if candidate == "*" || candidate == origin {
			return true
		}
	}
	return false
}

type responseEnvelope struct {
	ID           string                     `json:"id,omitempty"`
	RequestID    string                     `json:"requestId"`
	Data         any                        `json:"data,omitempty"`
	Errors       []publicExecutionError     `json:"errors,omitempty"`
	Capabilities []string                   `json:"capabilities"`
	Extensions   map[string]json.RawMessage `json:"extensions"`
}

type publicExecutionError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Path      []any  `json:"path"`
	Retryable bool   `json:"retryable"`
}

func (handler *Handler) writeOutcome(writer stdhttp.ResponseWriter, request *stdhttp.Request, decoded *protocol.Request, outcome runtime.Outcome, representation responseRepresentation) {
	requestID := handler.safeRequestID()
	envelope := responseEnvelope{RequestID: requestID, Capabilities: slices.Clone(outcome.Capabilities), Extensions: make(map[string]json.RawMessage)}
	if id, ok := decoded.ID(); ok {
		envelope.ID = id
	}
	if outcome.Data != nil {
		envelope.Data = outcome.Data
	}
	for _, failure := range outcome.Errors {
		envelope.Errors = append(envelope.Errors, sanitizeExecutionError(failure))
	}
	status := outcomeStatus(outcome)
	if outcome.Data == nil && len(envelope.Errors) == 0 {
		envelope.Errors = []publicExecutionError{{Code: "INTERNAL", Message: publicMessage("INTERNAL"), Path: []any{}}}
		status = stdhttp.StatusInternalServerError
	}
	payload, err := encodeResponseEnvelope(envelope, representation, handler.limits.MaxResponseBytes)
	if err != nil {
		envelope.Data = nil
		envelope.Errors = []publicExecutionError{{Code: "INTERNAL", Message: publicMessage("INTERNAL"), Path: []any{}}}
		status = stdhttp.StatusInternalServerError
		payload, _ = encodeResponseEnvelope(envelope, representation, handler.limits.MaxResponseBytes)
	}
	encoding := "identity"
	if len(payload) >= handler.limits.CompressionMinBytes && acceptsGzip(request.Header.Values("Accept-Encoding")) {
		if compressed, compressErr := gzipBounded(payload, handler.limits.MaxCompressedResponseBytes); compressErr == nil {
			payload, encoding = compressed, "gzip"
		}
	}
	contentType := ResponseMediaType
	if representation == representationCBOR {
		contentType = CBORResponseMediaType
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Naatre-Request-Id", requestID)
	writer.Header().Set("Content-Encoding", encoding)
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

func encodeResponseEnvelope(envelope responseEnvelope, representation responseRepresentation, limit int64) ([]byte, error) {
	payload, err := marshalBounded(envelope, limit)
	if err != nil || representation != representationCBOR {
		return payload, err
	}
	payload, err = encodeCBORPayload(payload)
	if err == nil && int64(len(payload)) > limit {
		return nil, errLimitExceeded
	}
	return payload, err
}

func outcomeStatus(outcome runtime.Outcome) int {
	if outcome.Data != nil {
		return stdhttp.StatusOK
	}
	status := stdhttp.StatusOK
	for _, failure := range outcome.Errors {
		if publicMessage(failure.Code) == "" {
			return stdhttp.StatusInternalServerError
		}
		switch failure.Code {
		case "UNAUTHORIZED":
			return stdhttp.StatusForbidden
		case "VALIDATION_FAILED":
			status = stdhttp.StatusUnprocessableEntity
		case "RESOURCE_EXHAUSTED", "CANCELLED":
			return stdhttp.StatusGatewayTimeout
		case "INTERNAL":
			return stdhttp.StatusInternalServerError
		}
	}
	return status
}

func sanitizeExecutionError(failure runtime.ExecutionError) publicExecutionError {
	code := failure.Code
	if publicMessage(code) == "" {
		code = "INTERNAL"
	}
	path := make([]any, 0, len(failure.Path))
	for _, segment := range failure.Path {
		switch value := segment.(type) {
		case string:
			if len(value) <= 128 {
				path = append(path, value)
			}
		case int:
			if value >= 0 {
				path = append(path, value)
			}
		case uint64:
			path = append(path, value)
		}
	}
	return publicExecutionError{Code: code, Message: publicMessage(code), Path: path, Retryable: failure.Retryable}
}

func publicMessage(code string) string {
	switch code {
	case "VALIDATION_FAILED":
		return "operation validation failed"
	case "UNAUTHORIZED":
		return "operation is not authorized"
	case "RESOURCE_EXHAUSTED":
		return "request deadline or resource limit exceeded"
	case "CANCELLED":
		return "request cancelled"
	case "INTERNAL":
		return "internal execution error"
	case "PARTIAL", "FIELD_FAILED", "HANDLER_FAILED", "OUTPUT_COMPLETION":
		return "field unavailable"
	default:
		return ""
	}
}

func marshalBounded(value any, limit int64) ([]byte, error) {
	buffer := &boundedBuffer{remaining: limit}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func encodeCBORPayload(payload []byte) ([]byte, error) {
	value, err := protocol.DecodeJSONValue(payload)
	if err != nil {
		return nil, err
	}
	return naatrecbor.Marshal(value)
}

type boundedBuffer struct {
	bytes.Buffer
	remaining int64
}

func (buffer *boundedBuffer) Write(payload []byte) (int, error) {
	if int64(len(payload)) > buffer.remaining {
		return 0, errLimitExceeded
	}
	n, err := buffer.Buffer.Write(payload)
	buffer.remaining -= int64(n)
	return n, err
}

func gzipBounded(payload []byte, limit int64) ([]byte, error) {
	buffer := &boundedBuffer{remaining: limit}
	writer := gzip.NewWriter(buffer)
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func acceptsGzip(lines []string) bool {
	preferences, err := parseAcceptEncoding(lines)
	if err != nil {
		return false
	}
	for _, preference := range preferences {
		if (preference.coding == "gzip" || preference.coding == "*") && preference.quality > 0 {
			return true
		}
	}
	return false
}

func validAcceptEncoding(lines []string) bool {
	_, err := parseAcceptEncoding(lines)
	return err == nil
}

type encodingPreference struct {
	coding  string
	quality float64
}

func parseAcceptEncoding(lines []string) ([]encodingPreference, error) {
	preferences := make([]encodingPreference, 0)
	for _, line := range lines {
		for _, item := range strings.Split(line, ",") {
			preference, err := parseEncodingPreference(item)
			if err != nil {
				return nil, err
			}
			preferences = append(preferences, preference)
		}
	}
	return preferences, nil
}

func parseEncodingPreference(item string) (encodingPreference, error) {
	parts := strings.Split(strings.TrimSpace(item), ";")
	if parts[0] == "" || strings.ContainsAny(parts[0], " \t") {
		return encodingPreference{}, errors.New("invalid content coding")
	}
	parameters := make(map[string]string, len(parts)-1)
	for _, raw := range parts[1:] {
		name, value, ok := strings.Cut(strings.TrimSpace(raw), "=")
		_, duplicate := parameters["q"]
		if !ok || !strings.EqualFold(name, "q") || duplicate {
			return encodingPreference{}, errors.New("invalid content-coding parameter")
		}
		parameters["q"] = value
	}
	quality, err := qualityParameter(parameters)
	return encodingPreference{coding: strings.ToLower(parts[0]), quality: quality}, err
}

func transportOutcomeProblem(outcome runtime.Outcome) (int, string) {
	if outcome.Data != nil || len(outcome.Errors) != 1 {
		return 0, ""
	}
	switch outcome.Errors[0].Code {
	case "RATE_LIMITED":
		return stdhttp.StatusTooManyRequests, "RATE_LIMITED"
	case "OVERLOADED":
		return stdhttp.StatusServiceUnavailable, "OVERLOADED"
	default:
		return 0, ""
	}
}

type problemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
}

func (handler *Handler) writeProblem(writer stdhttp.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", ProblemMediaType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Naatre-Request-Id", handler.safeRequestID())
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(problemDetails{Type: "https://naatre.dev/problems/" + strings.ToLower(strings.ReplaceAll(code, "_", "-")), Title: stdhttp.StatusText(status), Status: status, Code: code})
}

func (handler *Handler) safeRequestID() string {
	requestID := handler.requestID()
	if requestID == "" || len(requestID) > 128 || strings.ContainsAny(requestID, "\r\n") {
		return randomRequestID()
	}
	return requestID
}

type trackingWriter struct {
	stdhttp.ResponseWriter
	wroteHeader bool
}

func (writer *trackingWriter) WriteHeader(status int) {
	writer.wroteHeader = true
	writer.ResponseWriter.WriteHeader(status)
}
func (writer *trackingWriter) Write(payload []byte) (int, error) {
	writer.wroteHeader = true
	return writer.ResponseWriter.Write(payload)
}

func setVary(header stdhttp.Header, values ...string) {
	existing := make(map[string]bool)
	for _, line := range header.Values("Vary") {
		for _, value := range strings.Split(line, ",") {
			existing[stdhttp.CanonicalHeaderKey(strings.TrimSpace(value))] = true
		}
	}
	for _, value := range values {
		if !existing[stdhttp.CanonicalHeaderKey(value)] {
			header.Add("Vary", value)
			existing[stdhttp.CanonicalHeaderKey(value)] = true
		}
	}
}

func randomRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "naatre-request"
	}
	return hex.EncodeToString(value[:])
}

func headersAllowed(requested string, allowed []string) bool {
	if strings.TrimSpace(requested) == "" {
		return true
	}
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[stdhttp.CanonicalHeaderKey(name)] = true
	}
	for _, name := range strings.Split(requested, ",") {
		name = strings.TrimSpace(name)
		if name == "" || !set[stdhttp.CanonicalHeaderKey(name)] {
			return false
		}
	}
	return true
}

func channelClosed(channel <-chan struct{}) bool {
	if channel == nil {
		return false
	}
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

var _ stdhttp.Handler = (*Handler)(nil)
