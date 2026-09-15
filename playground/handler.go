// Package playground provides the opt-in local browser and schema-mock HTTP
// surface. It consumes an already authorized schema.Document and deliberately
// has no business-handler, registry, executor, plugin, or outbound transport.
package playground

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/valksor/naatre/asyncapi"
	"github.com/valksor/naatre/schema"
	"github.com/valksor/naatre/tooling"
)

const (
	Profile = "naatre.playground-mock-1"

	CodeCancelled        = "PLAYGROUND_CANCELLED"
	CodeInternal         = "PLAYGROUND_INTERNAL"
	CodeInvalidRequest   = "PLAYGROUND_INVALID_REQUEST"
	CodeInvalidTarget    = "PLAYGROUND_INVALID_TARGET"
	CodeMethodNotAllowed = "PLAYGROUND_METHOD_NOT_ALLOWED"
	CodeMockRejected     = "PLAYGROUND_MOCK_REJECTED"
	CodeNotFound         = "PLAYGROUND_NOT_FOUND"
	CodeOutputTooLarge   = "PLAYGROUND_OUTPUT_TOO_LARGE"
	CodeRequestTooLarge  = "PLAYGROUND_REQUEST_TOO_LARGE"
	CodeUnsupportedMedia = "PLAYGROUND_UNSUPPORTED_MEDIA_TYPE"
	Issue55Revision      = "16aeb4606062207c0fccabb03a348d49e136e5c6"
	ToolingFixtureSHA256 = "79af36f66265dc58df7fd8bbd83c3d88ab88c230b5a94d75b5696f03ff1bfb78"
)

var publicTitles = map[string]string{
	CodeCancelled:        "request cancelled",
	CodeInternal:         "playground request failed",
	CodeInvalidRequest:   "request is invalid",
	CodeInvalidTarget:    "target is not allowed",
	CodeMethodNotAllowed: "method is not allowed",
	CodeMockRejected:     "mock request rejected",
	CodeNotFound:         "resource not found",
	CodeOutputTooLarge:   "response exceeds the configured limit",
	CodeRequestTooLarge:  "request exceeds the configured limit",
	CodeUnsupportedMedia: "content type is not supported",
}

type Limits struct {
	MaxRequestBytes  int64
	MaxDocumentBytes int
	MaxPayloadBytes  int
	MaxResponseBytes int
	Mock             tooling.MockOptions
}

func DefaultLimits() Limits {
	return Limits{
		MaxRequestBytes: 1 << 20, MaxDocumentBytes: 256 << 10,
		MaxPayloadBytes: 4 << 10, MaxResponseBytes: 1 << 20,
		Mock: tooling.DefaultMockOptions(),
	}
}

type Config struct {
	// Schema is the caller-authorized view. The handler never loads, widens, or
	// dynamically authorizes schema metadata.
	Schema                 schema.Document
	AllowedOrigins         []string
	AsyncAPIDocument       []byte
	AllowedAsyncAPIServers []string
	Limits                 Limits
}

type Handler struct {
	schema         schema.Document
	schemaJSON     []byte
	schemaRevision string
	schemaDigest   string
	allowedOrigins []string
	asyncAPI       *asyncapi.Model
	limits         Limits
	mux            *http.ServeMux
}

type Problem struct {
	Profile string `json:"profile"`
	Code    string `json:"code"`
	Title   string `json:"title"`
}

type mockRequest struct {
	Document  json.RawMessage `json:"document"`
	Operation string          `json:"operation"`
	Seed      *uint64         `json:"seed"`
	Scenario  string          `json:"scenario"`
	Variables json.RawMessage `json:"variables,omitempty"`
}

type mockResponse struct {
	Profile   string             `json:"profile"`
	Mock      tooling.MockResult `json:"mock"`
	Variables InspectionPayload  `json:"variables"`
}

type profileResponse struct {
	Profile              string   `json:"profile"`
	Issue55Revision      string   `json:"issue55Revision"`
	ToolingFixtureSHA256 string   `json:"toolingFixtureSHA256"`
	SchemaRevision       string   `json:"schemaRevision"`
	SchemaDigest         string   `json:"schemaDigest"`
	Supported            []string `json:"supported"`
	Unsupported          []string `json:"unsupported"`
	FailureCodes         []string `json:"failureCodes"`
	Limits               Limits   `json:"limits"`
}

//go:embed ui.html
var playgroundUI []byte

func NewHandler(config Config) (*Handler, error) {
	if !validLimits(config.Limits) {
		return nil, errors.New("invalid playground limits")
	}
	canonical, err := config.Schema.CanonicalJSON()
	if err != nil {
		return nil, errors.New("invalid authorized schema view")
	}
	digest, err := config.Schema.Hash()
	if err != nil {
		return nil, errors.New("invalid authorized schema view")
	}
	var header struct {
		Revision string `json:"revision"`
	}
	if json.Unmarshal(canonical, &header) != nil || header.Revision == "" {
		return nil, errors.New("authorized schema revision is required")
	}
	origins, err := normalizeOrigins(config.AllowedOrigins)
	if err != nil {
		return nil, err
	}
	handler := &Handler{
		schema: config.Schema, schemaJSON: canonical, schemaRevision: header.Revision,
		schemaDigest: digest.Hex, allowedOrigins: origins, limits: config.Limits,
		mux: http.NewServeMux(),
	}
	if len(config.AsyncAPIDocument) != 0 {
		model, _, importErr := asyncapi.Import(config.AsyncAPIDocument, asyncapi.ImportOptions{
			Limits: asyncapi.DefaultLimits(), AllowedServerURLs: slices.Clone(config.AllowedAsyncAPIServers),
		})
		if importErr != nil {
			return nil, errors.New("invalid authorized AsyncAPI view")
		}
		handler.asyncAPI = &model
	}
	handler.mux.HandleFunc("GET /{$}", handler.serveUI)
	handler.mux.HandleFunc("GET /v1/profile", handler.serveProfile)
	handler.mux.HandleFunc("GET /v1/schema", handler.serveSchema)
	if handler.asyncAPI != nil {
		handler.mux.HandleFunc("GET /v1/asyncapi", handler.serveAsyncAPI)
	}
	handler.mux.HandleFunc("POST /v1/inspect", handler.serveInspect)
	handler.mux.HandleFunc("POST /v1/mock", handler.serveMock)
	handler.mux.HandleFunc("/", handler.serveFallback)
	return handler, nil
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	wrapped := &trackingWriter{ResponseWriter: writer}
	defer func() {
		if recover() != nil && !wrapped.wrote {
			h.writeProblem(writer, http.StatusInternalServerError, CodeInternal)
		}
	}()
	if request.Context().Err() != nil {
		h.writeProblem(wrapped, statusClientClosedRequest, CodeCancelled)
		return
	}
	h.mux.ServeHTTP(wrapped, request)
	if !wrapped.wrote {
		h.writeProblem(wrapped, http.StatusNotFound, CodeNotFound)
	}
}

func (h *Handler) serveUI(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.writeBytes(writer, http.StatusOK, "text/html; charset=utf-8", playgroundUI)
}

func (h *Handler) serveFallback(writer http.ResponseWriter, request *http.Request) {
	for _, path := range []string{"/", "/v1/profile", "/v1/schema", "/v1/asyncapi", "/v1/inspect", "/v1/mock"} {
		if request.URL.Path == path {
			h.writeProblem(writer, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
			return
		}
	}
	h.writeProblem(writer, http.StatusNotFound, CodeNotFound)
}

func (h *Handler) serveAsyncAPI(writer http.ResponseWriter, _ *http.Request) {
	if h.asyncAPI == nil {
		h.writeProblem(writer, http.StatusNotFound, CodeNotFound)
		return
	}
	h.writeJSON(writer, http.StatusOK, asyncapi.Inspect(*h.asyncAPI))
}

func (h *Handler) serveSchema(writer http.ResponseWriter, _ *http.Request) {
	h.writeBytes(writer, http.StatusOK, "application/json", h.schemaJSON)
}

func (h *Handler) serveProfile(writer http.ResponseWriter, _ *http.Request) {
	h.writeJSON(writer, http.StatusOK, profileResponse{
		Profile: Profile, Issue55Revision: Issue55Revision, ToolingFixtureSHA256: ToolingFixtureSHA256,
		SchemaRevision: h.schemaRevision, SchemaDigest: h.schemaDigest,
		Supported:    supportedCapabilities(),
		Unsupported:  unsupportedCapabilities(),
		FailureCodes: sortedFailureCodes(), Limits: h.limits,
	})
}

func (h *Handler) serveInspect(writer http.ResponseWriter, request *http.Request) {
	var input InspectionRequest
	if !h.decodeJSON(writer, request, &input) {
		return
	}
	inspection, err := Inspect(input, h.allowedOrigins, h.limits.MaxPayloadBytes)
	if err != nil {
		h.writeProblem(writer, http.StatusBadRequest, CodeInvalidTarget)
		return
	}
	h.writeJSON(writer, http.StatusOK, inspection)
}

func (h *Handler) serveMock(writer http.ResponseWriter, request *http.Request) {
	var input mockRequest
	if !h.decodeJSON(writer, request, &input) {
		return
	}
	if input.Seed == nil || input.Operation == "" || input.Scenario == "" || len(input.Document) == 0 || len(input.Document) > h.limits.MaxDocumentBytes {
		h.writeProblem(writer, http.StatusBadRequest, CodeInvalidRequest)
		return
	}
	result, err := tooling.GenerateMockContext(request.Context(), h.schema, tooling.MockInput{
		Document: input.Document, Operation: input.Operation, Seed: *input.Seed, Scenario: tooling.MockScenario(input.Scenario),
	}, h.limits.Mock)
	if err != nil {
		var generation *tooling.MockGenerationError
		if errors.As(err, &generation) && generation.Code == tooling.CodeMockCancelled {
			h.writeProblem(writer, statusClientClosedRequest, CodeCancelled)
			return
		}
		h.writeProblem(writer, http.StatusBadRequest, CodeMockRejected)
		return
	}
	variables, truncated := tooling.RedactAndBound(string(input.Variables), h.limits.MaxPayloadBytes)
	h.writeJSON(writer, http.StatusOK, mockResponse{
		Profile: Profile, Mock: result,
		Variables: InspectionPayload{Value: variables, Truncated: truncated},
	})
}

func (h *Handler) decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	if !jsonMediaType(request.Header.Get("Content-Type")) {
		h.writeProblem(writer, http.StatusUnsupportedMediaType, CodeUnsupportedMedia)
		return false
	}
	reader := http.MaxBytesReader(writer, request.Body, h.limits.MaxRequestBytes)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var maximum *http.MaxBytesError
		if errors.As(err, &maximum) {
			h.writeProblem(writer, http.StatusRequestEntityTooLarge, CodeRequestTooLarge)
		} else {
			h.writeProblem(writer, http.StatusBadRequest, CodeInvalidRequest)
		}
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		h.writeProblem(writer, http.StatusBadRequest, CodeInvalidRequest)
		return false
	}
	if request.Context().Err() != nil {
		h.writeProblem(writer, statusClientClosedRequest, CodeCancelled)
		return false
	}
	return true
}

func (h *Handler) writeJSON(writer http.ResponseWriter, status int, value any) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		h.writeProblem(writer, http.StatusInternalServerError, CodeInternal)
		return
	}
	if output.Len() > h.limits.MaxResponseBytes {
		h.writeProblem(writer, http.StatusInsufficientStorage, CodeOutputTooLarge)
		return
	}
	h.writeBytes(writer, status, "application/json", output.Bytes())
}

func (h *Handler) writeProblem(writer http.ResponseWriter, status int, code string) {
	setSafeHeaders(writer.Header())
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(Problem{Profile: "naatre.playground.problem-1", Code: code, Title: publicTitles[code]})
}

func (h *Handler) writeBytes(writer http.ResponseWriter, status int, contentType string, value []byte) {
	if len(value) > h.limits.MaxResponseBytes {
		h.writeProblem(writer, http.StatusInsufficientStorage, CodeOutputTooLarge)
		return
	}
	setSafeHeaders(writer.Header())
	writer.Header().Set("Content-Type", contentType)
	writer.WriteHeader(status)
	_, _ = writer.Write(value)
}

func sortedFailureCodes() []string {
	codes := make([]string, 0, len(publicTitles))
	for code := range publicTitles {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	return codes
}

func supportedCapabilities() []string {
	return []string{"authorized-schema-browse", "bounded-request-inspection", "credential-redaction", "deterministic-schema-mocks", "scenario-selection", "stream-frame-fixtures", "sdk-example-copy"}
}

func unsupportedCapabilities() []string {
	return []string{"alternate-native-runtime", "business-handler-execution", "dynamic-authorization", "live-upstream-requests", "network-schema-fetch", "persisted-history", "plugin-execution", "remote-listen", "tls-termination", "transport-certification", "unredacted-persistence", "websocket-transport"}
}

func jsonMediaType(value string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return false
	}
	for name, value := range parameters {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func validLimits(limits Limits) bool {
	return limits.MaxRequestBytes > 0 && limits.MaxDocumentBytes > 0 && limits.MaxPayloadBytes > 0 && limits.MaxResponseBytes > 0 &&
		limits.Mock.MaxDepth > 0 && limits.Mock.MaxItems > 0 && limits.Mock.MaxStringBytes > 0
}

func setSafeHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}

type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *trackingWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingWriter) Write(value []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(value)
}

const statusClientClosedRequest = 499

var _ http.Handler = (*Handler)(nil)
