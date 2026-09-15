package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	stdhttp "net/http"
	"strconv"
	"strings"

	"github.com/valksor/naatre/protocol/httpdigest"
)

type digestMiddleware struct {
	config DigestMiddlewareConfig
}

// NewDigestMiddleware constructs a reusable request-verification and response-
// generation middleware. The returned wrapper does not own listeners or TLS.
func NewDigestMiddleware(config DigestMiddlewareConfig) (func(stdhttp.Handler) stdhttp.Handler, error) {
	resolved, err := resolveDigestMiddlewareConfig(config)
	if err != nil {
		return nil, err
	}
	middleware := &digestMiddleware{config: resolved}
	return func(next stdhttp.Handler) stdhttp.Handler {
		if next == nil {
			next = stdhttp.NotFoundHandler()
		}
		return stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
			middleware.serveHTTP(writer, request, next)
		})
	}, nil
}

func resolveDigestMiddlewareConfig(config DigestMiddlewareConfig) (DigestMiddlewareConfig, error) {
	modes := []DigestMode{config.RequestContent, config.RequestRepresentation, config.ResponseContent, config.ResponseRepresentation}
	for _, mode := range modes {
		if mode > DigestRequired {
			return DigestMiddlewareConfig{}, errors.New("invalid digest middleware mode")
		}
	}
	limits := []*int64{
		&config.MaximumRequestContentBytes, &config.MaximumRequestRepresentationBytes,
		&config.MaximumResponseContentBytes, &config.MaximumResponseRepresentationBytes,
	}
	for _, limit := range limits {
		if *limit == 0 {
			*limit = DefaultMaximumDigestBytes
		}
		if *limit < 1 || *limit == math.MaxInt64 {
			return DigestMiddlewareConfig{}, errors.New("invalid digest middleware limit")
		}
	}
	return config, nil
}

func (middleware *digestMiddleware) serveHTTP(writer stdhttp.ResponseWriter, request *stdhttp.Request, next stdhttp.Handler) {
	if request == nil {
		writeDigestProblem(writer, stdhttp.StatusBadRequest, digestFailure(DigestPhaseHeaders, ErrMalformedDigest))
		return
	}
	if digestTrailerDeclared(request.Header, request.Trailer) {
		writeDigestProblem(writer, stdhttp.StatusBadRequest, digestFailure(DigestPhaseHeaders, ErrDigestTrailerUnsupported))
		return
	}
	contentAlgorithm, contentActive, err := negotiatedResponseAlgorithm(request.Header, "Want-Content-Digest", middleware.config.ResponseContent)
	if err != nil {
		writeDigestProblem(writer, stdhttp.StatusBadRequest, digestFailure(DigestPhaseHeaders, err))
		return
	}
	reprAlgorithm, reprActive, err := negotiatedResponseAlgorithm(request.Header, "Want-Repr-Digest", middleware.config.ResponseRepresentation)
	if err != nil {
		writeDigestProblem(writer, stdhttp.StatusBadRequest, digestFailure(DigestPhaseHeaders, err))
		return
	}
	if failure := middleware.verifyRequest(request); failure != nil {
		status := stdhttp.StatusBadRequest
		if failure.Code == CodeDigestLimitExceeded {
			status = stdhttp.StatusRequestEntityTooLarge
		}
		writeDigestProblem(writer, status, failure)
		return
	}
	if !contentActive && !reprActive {
		next.ServeHTTP(writer, request)
		return
	}
	capture := newDigestResponseCapture(middleware.config.MaximumResponseContentBytes)
	next.ServeHTTP(capture, request)
	failure := middleware.completeResponse(request, capture, contentAlgorithm, contentActive, reprAlgorithm, reprActive)
	if failure != nil {
		writeDigestProblem(writer, stdhttp.StatusInternalServerError, failure)
		return
	}
	copyHTTPHeader(writer.Header(), capture.header)
	writer.WriteHeader(capture.status)
	_, _ = writer.Write(capture.body.Bytes())
}

func (middleware *digestMiddleware) verifyRequest(request *stdhttp.Request) *DigestFailure {
	contentFields, contentActive, err := digestFieldsForMode(request.Header, "Content-Digest", middleware.config.RequestContent)
	if err != nil {
		return digestFailure(DigestPhaseHeaders, err)
	}
	reprFields, reprActive, err := digestFieldsForMode(request.Header, "Repr-Digest", middleware.config.RequestRepresentation)
	if err != nil {
		return digestFailure(DigestPhaseHeaders, err)
	}
	if reprActive {
		if err := ValidateDigestContentEncoding(request.Header); err != nil {
			return digestFailure(DigestPhaseHeaders, err)
		}
	}
	if !contentActive && !reprActive {
		return nil
	}
	staged, err := middleware.stageRequest(request, contentFields, contentActive)
	if err != nil {
		return digestFailure(DigestPhaseContent, normalizeDigestIO(request.Context(), err))
	}
	if digestTrailerDeclared(request.Header, request.Trailer) {
		return digestFailure(DigestPhaseCompletion, ErrDigestTrailerUnsupported)
	}
	if reprActive {
		if err := middleware.verifyRequestRepresentation(request, staged, reprFields); err != nil {
			return digestFailure(DigestPhaseRepresentation, normalizeDigestIO(request.Context(), err))
		}
	}
	request.Body = io.NopCloser(bytes.NewReader(staged))
	request.ContentLength = int64(len(staged))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(staged)), nil
	}
	return nil
}

func (middleware *digestMiddleware) stageRequest(request *stdhttp.Request, contentFields []string, contentActive bool) ([]byte, error) {
	body := request.Body
	if body == nil {
		body = stdhttp.NoBody
	}
	defer func() { _ = body.Close() }()
	var staged bytes.Buffer
	var err error
	if contentActive {
		_, err = VerifyDigestTo(&staged, &contextDigestReader{ctx: request.Context(), reader: body}, contentFields, VerifyOptions{
			MaximumBytes: middleware.config.MaximumRequestContentBytes, ExpectedLength: request.ContentLength,
		})
	} else {
		err = stageDigestContent(&staged, &contextDigestReader{ctx: request.Context(), reader: body}, middleware.config.MaximumRequestContentBytes, request.ContentLength)
	}
	if err != nil {
		return nil, err
	}
	return staged.Bytes(), nil
}

func (middleware *digestMiddleware) verifyRequestRepresentation(request *stdhttp.Request, staged []byte, reprFields []string) error {
	representation, closeRepresentation, err := representationReader(request.Header, staged)
	if err != nil {
		return err
	}
	if closeRepresentation != nil {
		defer closeRepresentation()
	}
	_, err = VerifyDigestTo(io.Discard, &contextDigestReader{ctx: request.Context(), reader: representation}, reprFields, VerifyOptions{
		MaximumBytes: middleware.config.MaximumRequestRepresentationBytes, ExpectedLength: -1,
	})
	return err
}

func (middleware *digestMiddleware) completeResponse(request *stdhttp.Request, capture *digestResponseCapture, contentAlgorithm DigestAlgorithm, contentActive bool, reprAlgorithm DigestAlgorithm, reprActive bool) *DigestFailure {
	if failure := validateCapturedResponse(capture); failure != nil {
		return failure
	}
	if contentActive {
		if err := reconcileDigestField(capture.header, "Content-Digest", capture.body.Bytes(), contentAlgorithm); err != nil {
			return digestFailure(DigestPhaseCompletion, err)
		}
	}
	if reprActive {
		if failure := middleware.completeResponseRepresentation(request, capture, reprAlgorithm); failure != nil {
			return failure
		}
	}
	if contentActive {
		setVary(capture.header, "Want-Content-Digest")
	}
	if reprActive {
		setVary(capture.header, "Want-Repr-Digest")
	}
	capture.header.Set("Content-Length", strconv.Itoa(capture.body.Len()))
	return nil
}

func validateCapturedResponse(capture *digestResponseCapture) *DigestFailure {
	if capture.flushed {
		return digestFailure(DigestPhaseCompletion, ErrDigestStreamUnsupported)
	}
	if capture.overflow {
		return digestFailure(DigestPhaseContent, ErrDigestLimitExceeded)
	}
	if digestTrailerDeclared(capture.header, capture.trailer) {
		return digestFailure(DigestPhaseCompletion, ErrDigestTrailerUnsupported)
	}
	if err := ValidateResponseRange(capture.status, capture.header, int64(capture.body.Len())); err != nil {
		return digestFailure(DigestPhaseHeaders, err)
	}
	return nil
}

func (middleware *digestMiddleware) completeResponseRepresentation(request *stdhttp.Request, capture *digestResponseCapture, reprAlgorithm DigestAlgorithm) *DigestFailure {
	if capture.status == stdhttp.StatusPartialContent {
		return middleware.completeRangeRepresentation(request, capture, reprAlgorithm)
	}
	representation, closeRepresentation, err := representationReader(capture.header, capture.body.Bytes())
	if err != nil {
		return digestFailure(DigestPhaseHeaders, err)
	}
	if closeRepresentation != nil {
		defer closeRepresentation()
	}
	representationBytes, err := readDigestBounded(representation, middleware.config.MaximumResponseRepresentationBytes)
	if err != nil {
		return digestFailure(DigestPhaseRepresentation, err)
	}
	if err := reconcileDigestField(capture.header, "Repr-Digest", representationBytes, reprAlgorithm); err != nil {
		return digestFailure(DigestPhaseCompletion, err)
	}
	return nil
}

func (middleware *digestMiddleware) completeRangeRepresentation(request *stdhttp.Request, capture *digestResponseCapture, reprAlgorithm DigestAlgorithm) *DigestFailure {
	if middleware.config.RangeRepresentationDigest == nil {
		if middleware.config.ResponseRepresentation == DigestRequired {
			return digestFailure(DigestPhaseRepresentation, ErrDigestRepresentationUnavailable)
		}
		return nil
	}
	field, err := middleware.config.RangeRepresentationDigest(request, capture.header.Clone())
	if err != nil || field == "" {
		return digestFailure(DigestPhaseRepresentation, ErrDigestRepresentationUnavailable)
	}
	if err := RequireDigestAlgorithm([]string{field}, reprAlgorithm); err != nil {
		return digestFailure(DigestPhaseRepresentation, err)
	}
	capture.header.Set("Repr-Digest", field)
	return nil
}

func negotiatedResponseAlgorithm(header stdhttp.Header, name string, mode DigestMode) (DigestAlgorithm, bool, error) {
	if mode == DigestDisabled {
		return "", false, nil
	}
	fields := header.Values(name)
	if mode == DigestOptional && len(fields) == 0 {
		return "", false, nil
	}
	algorithm, err := NegotiateDigestAlgorithm(fields)
	return algorithm, err == nil, err
}

func digestFieldsForMode(header stdhttp.Header, name string, mode DigestMode) ([]string, bool, error) {
	if mode == DigestDisabled {
		return nil, false, nil
	}
	fields := header.Values(name)
	if len(fields) == 0 && mode == DigestOptional {
		return nil, false, nil
	}
	if _, err := ParseDigestFields(fields); err != nil {
		return nil, false, err
	}
	return fields, true, nil
}

func reconcileDigestField(header stdhttp.Header, name string, content []byte, algorithm DigestAlgorithm) error {
	fields := header.Values(name)
	if len(fields) > 0 {
		if err := RequireDigestAlgorithm(fields, algorithm); err != nil {
			return err
		}
		if _, err := VerifyDigestTo(io.Discard, bytes.NewReader(content), fields, VerifyOptions{MaximumBytes: int64(len(content)) + 1, ExpectedLength: int64(len(content))}); err != nil {
			return err
		}
	}
	field, err := FormatDigestField(content, algorithm)
	if err != nil {
		return err
	}
	header.Set(name, field)
	return nil
}

func representationReader(header stdhttp.Header, content []byte) (io.Reader, func(), error) {
	if err := ValidateDigestContentEncoding(header); err != nil {
		return nil, nil, ErrDigestContentCodingUnsupported
	}
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Encoding")))
	switch encoding {
	case "", "identity":
		return bytes.NewReader(content), nil, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(content))
		if err != nil {
			return nil, nil, ErrDigestContentCodingUnsupported
		}
		return reader, func() { _ = reader.Close() }, nil
	default:
		return nil, nil, ErrDigestContentCodingUnsupported
	}
}

// ValidateDigestContentEncoding accepts the representation codings executed by
// this implementation profile without reading body bytes.
func ValidateDigestContentEncoding(header stdhttp.Header) error {
	return httpdigest.ValidateContentEncoding(header)
}

// ValidateResponseRange enforces the metadata boundary that prevents a range
// Content-Digest from being treated as proof of a complete representation.
func ValidateResponseRange(status int, header stdhttp.Header, contentBytes int64) error {
	return httpdigest.ValidateResponseRange(status, header, contentBytes)
}

func digestTrailerDeclared(header, trailer stdhttp.Header) bool {
	return httpdigest.RejectTrailers(header, trailer) != nil
}

func stageDigestContent(dst io.Writer, src io.Reader, maximum, expectedLength int64) error {
	if expectedLength > maximum {
		return ErrDigestLimitExceeded
	}
	n, err := io.Copy(dst, io.LimitReader(src, maximum+1))
	if err != nil {
		return err
	}
	if n > maximum {
		return ErrDigestLimitExceeded
	}
	if expectedLength >= 0 && n < expectedLength {
		return ErrDigestTruncated
	}
	if expectedLength >= 0 && n > expectedLength {
		return ErrDigestLengthMismatch
	}
	return nil
}

func readDigestBounded(reader io.Reader, maximum int64) ([]byte, error) {
	var output bytes.Buffer
	if err := stageDigestContent(&output, reader, maximum, -1); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type contextDigestReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextDigestReader) Read(payload []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.reader.Read(payload)
	if err == nil {
		err = reader.ctx.Err()
	}
	return n, err
}

func normalizeDigestIO(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrDigestCanceled
	}
	if DigestErrorCode(err) != CodeDigestIO {
		return err
	}
	return ErrDigestIO
}

func digestFailure(phase string, err error) *DigestFailure {
	return &DigestFailure{Code: DigestErrorCode(err), Phase: phase}
}

type digestProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Phase  string `json:"phase"`
}

func writeDigestProblem(writer stdhttp.ResponseWriter, status int, failure *DigestFailure) {
	writer.Header().Set("Content-Type", ProblemMediaType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(digestProblem{
		Type:  "https://naatre.dev/problems/" + strings.ToLower(strings.ReplaceAll(failure.Code, "_", "-")),
		Title: stdhttp.StatusText(status), Status: status, Code: failure.Code, Phase: failure.Phase,
	})
}

type digestResponseCapture struct {
	header      stdhttp.Header
	trailer     stdhttp.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
	maximum     int64
	overflow    bool
	flushed     bool
}

func newDigestResponseCapture(maximum int64) *digestResponseCapture {
	return &digestResponseCapture{header: make(stdhttp.Header), trailer: make(stdhttp.Header), status: stdhttp.StatusOK, maximum: maximum}
}

func (capture *digestResponseCapture) Header() stdhttp.Header { return capture.header }

func (capture *digestResponseCapture) WriteHeader(status int) {
	if capture.wroteHeader {
		return
	}
	capture.status = status
	capture.wroteHeader = true
}

func (capture *digestResponseCapture) Write(payload []byte) (int, error) {
	if !capture.wroteHeader {
		capture.WriteHeader(stdhttp.StatusOK)
	}
	remaining := capture.maximum + 1 - int64(capture.body.Len())
	if remaining > 0 {
		writeLength := int64(len(payload))
		if writeLength > remaining {
			writeLength = remaining
		}
		_, _ = capture.body.Write(payload[:writeLength])
	}
	if int64(len(payload)) > remaining {
		capture.overflow = true
	}
	return len(payload), nil
}

func (capture *digestResponseCapture) Flush() { capture.flushed = true }

func copyHTTPHeader(destination, source stdhttp.Header) {
	for name := range destination {
		destination.Del(name)
	}
	for name, values := range source {
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

var _ stdhttp.ResponseWriter = (*digestResponseCapture)(nil)
var _ stdhttp.Flusher = (*digestResponseCapture)(nil)
