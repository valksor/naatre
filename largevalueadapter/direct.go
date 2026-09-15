package largevalueadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/valksor/naatre/largevalue"
)

const defaultCapabilityHeader = "Naatre-Capability"

type DirectConfig struct {
	Coordinator      *largevalue.Coordinator
	Limits           Limits
	CapabilityHeader string
}

// DirectHandler provides listener-independent net/http upload and download
// semantics. The host owns routing, authentication middleware, server limits,
// TLS, timeouts, and shutdown.
type DirectHandler struct {
	coordinator      *largevalue.Coordinator
	limits           Limits
	capabilityHeader string
}

func NewDirectHandler(config DirectConfig) (*DirectHandler, error) {
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	}
	if config.CapabilityHeader == "" {
		config.CapabilityHeader = defaultCapabilityHeader
	}
	if config.Coordinator == nil || !validLimits(config.Limits) || !validHeaderName(config.CapabilityHeader) {
		return nil, publicError(CodeInvalidConfig, "direct transfer adapter configuration is invalid", nil)
	}
	return &DirectHandler{coordinator: config.Coordinator, limits: config.Limits, capabilityHeader: config.CapabilityHeader}, nil
}

func (h *DirectHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &trackedResponseWriter{ResponseWriter: writer}
	if err := h.serve(tracked, request); err != nil && !tracked.wroteHeader {
		writePublicFailure(tracked, err)
	}
}

func (h *DirectHandler) serve(writer http.ResponseWriter, request *http.Request) error {
	if err := contextError(request.Context()); err != nil {
		return err
	}
	if headerBytes(request.Header) > h.limits.MaximumHeaderBytes {
		return publicError(CodeResourceExhausted, "large value request exceeds adapter limits", nil)
	}
	reference := request.Header.Get(h.capabilityHeader)
	if reference == "" || len(reference) > h.limits.MaximumIdentifier*16 {
		return publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	}
	switch request.Method {
	case http.MethodPut, http.MethodPost, http.MethodPatch:
		return h.upload(writer, request, reference)
	case http.MethodGet, http.MethodHead:
		return h.download(writer, request, reference)
	default:
		writer.Header().Set("Allow", "GET, HEAD, PATCH, POST, PUT")
		return publicError(CodeUnsupportedCapability, "large value transfer method is unsupported", nil)
	}
}

func (h *DirectHandler) upload(writer http.ResponseWriter, request *http.Request, reference string) error {
	if request.ContentLength > h.limits.MaximumTransferBytes {
		return publicError(CodeResourceExhausted, "large value request exceeds adapter limits", nil)
	}
	record, err := h.coordinator.Inspect(request.Context(), reference, largevalue.AuthorizeUpload, request.Method)
	if err != nil || record.Direction != largevalue.Upload || record.Profile != largevalue.DirectProfile || record.Metadata.Length > h.limits.MaximumTransferBytes {
		return publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	}
	if err := h.coordinator.AcceptUpload(request.Context(), reference, request.Body); err != nil {
		return transferError(request.Context(), err)
	}
	writer.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *DirectHandler) download(writer http.ResponseWriter, request *http.Request, reference string) error {
	record, err := h.coordinator.Inspect(request.Context(), reference, largevalue.AuthorizeConsume, request.Method)
	if err != nil || record.Direction != largevalue.Download || record.Profile != largevalue.DirectProfile || record.Metadata.Length > h.limits.MaximumTransferBytes {
		return publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	}
	etag := representationETag(record.Metadata.RepresentationDigest)
	decision, decisionErr := largevalue.EvaluateDownloadRequest(largevalue.DownloadRequest{
		Method: request.Method, Range: request.Header.Get("Range"), IfMatch: request.Header.Get("If-Match"),
		IfNoneMatch: request.Header.Get("If-None-Match"), IfRange: request.Header.Get("If-Range"),
		Length: record.Metadata.Length, ETag: etag,
	})
	if decisionErr != nil && decision.Status != http.StatusRequestedRangeNotSatisfiable {
		return transferError(request.Context(), decisionErr)
	}
	setDownloadHeaders(writer.Header(), record.Metadata.MediaType, decision)
	if decision.Status == http.StatusRequestedRangeNotSatisfiable {
		writer.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(record.Metadata.Length, 10))
		writer.Header().Del("Content-Length")
		writePublicFailureStatus(writer, publicError(CodeInvalidRequest, "large value range is invalid", largevalue.ErrRangeUnsatisfied), decision.Status)
		return nil
	}
	if decision.Status == http.StatusNotModified || decision.Status == http.StatusPreconditionFailed {
		writer.WriteHeader(decision.Status)
		return nil
	}
	return transferError(request.Context(), h.coordinator.Consume(request.Context(), reference, func(ctx context.Context, _ largevalue.Record, source io.Reader) error {
		if decision.Range != nil {
			if _, err := io.CopyN(io.Discard, source, decision.Range.Start); err != nil {
				return err
			}
		}
		writer.WriteHeader(decision.Status)
		if request.Method == http.MethodHead {
			return nil
		}
		reader := source
		if decision.Range != nil {
			reader = io.LimitReader(source, decision.Range.Length())
		}
		buffer := make([]byte, 32*1024)
		_, err := io.CopyBuffer(writer, &contextReader{ctx: ctx, reader: reader}, buffer)
		return err
	}))
}

func setDownloadHeaders(header http.Header, mediaType string, decision largevalue.DownloadDecision) {
	header.Set("Accept-Ranges", "bytes")
	header.Set("Cache-Control", decision.CacheControl)
	header.Set("Content-Type", mediaType)
	header.Set("ETag", decision.ETag)
	header.Set("Content-Length", strconv.FormatInt(decision.ContentLength, 10))
	if decision.Range != nil {
		header.Set("Content-Range", decision.Range.ContentRange())
	}
}

func representationETag(digest string) string {
	value := sha256.Sum256([]byte(digest))
	return `"sha256-` + hex.EncodeToString(value[:]) + `"`
}

func headerBytes(header http.Header) int64 {
	var size int64
	for name, values := range header {
		size += int64(len(name))
		for _, value := range values {
			size += int64(len(value))
		}
	}
	return size
}

func validHeaderName(value string) bool {
	if strings.TrimSpace(value) != value || value == "" {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

type trackedResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *trackedResponseWriter) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackedResponseWriter) Write(content []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(content)
}

func writePublicFailure(writer http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch ErrorCode(err) {
	case CodeInvalidRequest:
		status = http.StatusBadRequest
	case CodeUnsupportedCapability:
		status = http.StatusMethodNotAllowed
	case CodeUnavailable:
		status = http.StatusNotFound
	case CodeResourceExhausted:
		status = http.StatusRequestEntityTooLarge
	case CodeCancelled:
		status = 499
	case CodeEgressDenied:
		status = http.StatusForbidden
	case CodeConflict:
		status = http.StatusConflict
	}
	writePublicFailureStatus(writer, err, status)
}

func writePublicFailureStatus(writer http.ResponseWriter, err error, status int) {
	public := &Error{Code: CodeTransferFailed, Message: "large value transfer failed"}
	var candidate *Error
	if errors.As(err, &candidate) {
		public = candidate
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: public.Code, Message: public.Message})
}
