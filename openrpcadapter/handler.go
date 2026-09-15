package openrpcadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/valksor/naatre/interopadapter"
)

type MethodHandler func(context.Context, json.RawMessage) (json.RawMessage, *ResponseError)

type ExposedMethod struct {
	Binding MethodBinding
	Handle  MethodHandler
}

type Authenticate func(context.Context, *http.Request) error

type HandlerConfig struct {
	Description  []byte
	Mapping      MappingConfig
	Methods      []ExposedMethod
	Authenticate Authenticate
	Limits       Limits
}

type Handler struct {
	methods      map[string]ExposedMethod
	authenticate Authenticate
	limits       Limits
	report       interopadapter.FidelityReport
}

func NewHandler(config HandlerConfig) (*Handler, interopadapter.FidelityReport, error) {
	limits := withDefaults(config.Limits)
	config.Mapping.Direction = interopadapter.RuntimeExpose
	config.Mapping.Bindings = make([]MethodBinding, 0, len(config.Methods))
	for _, method := range config.Methods {
		config.Mapping.Bindings = append(config.Mapping.Bindings, method.Binding)
	}
	_, report, err := MapDescription(config.Description, config.Mapping, limits)
	if err != nil {
		return nil, report, err
	}
	if !validRuntimeLimits(limits) {
		report.Status = "rejected"
		report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "JSONRPC_LIMIT_INVALID", Message: "runtime request and response limits are too small for safe JSON-RPC envelopes"})
		return nil, report, adapterError("JSONRPC_LIMIT_INVALID")
	}
	methods := make(map[string]ExposedMethod, len(config.Methods))
	for _, method := range config.Methods {
		if method.Handle == nil || methods[method.Binding.ExternalName].Handle != nil {
			report.Status = "rejected"
			report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "JSONRPC_HANDLER_INVALID", Operation: method.Binding.ExternalName, Message: "each method requires one explicit handler"})
			return nil, report, adapterError("JSONRPC_HANDLER_INVALID")
		}
		methods[method.Binding.ExternalName] = method
	}
	return &Handler{methods: methods, authenticate: config.Authenticate, limits: limits, report: report}, report, nil
}

func (h *Handler) FidelityReport() interopadapter.FidelityReport {
	result := h.report
	result.Mappings = slices.Clone(h.report.Mappings)
	result.Operations = slices.Clone(h.report.Operations)
	result.Diagnostics = slices.Clone(h.report.Diagnostics)
	return result
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost || mediaType(request.Header.Get("Content-Type")) != "application/json" {
		writer.WriteHeader(http.StatusUnsupportedMediaType)
		h.write(writer, Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeInvalidRequest, ""), ID: nullID()})
		return
	}
	if h.authenticate != nil && h.authenticate(request.Context(), request) != nil {
		writer.WriteHeader(http.StatusUnauthorized)
		h.write(writer, Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeUnauthorized, ""), ID: nullID()})
		return
	}
	body, err := readBoundedRequest(request, h.limits.MaxRequestBytes)
	if err != nil {
		writer.WriteHeader(http.StatusRequestEntityTooLarge)
		h.write(writer, Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeLimitExceeded, ""), ID: nullID()})
		return
	}
	if protocolError := validateJSONBytes(body, h.limits); protocolError != nil {
		writer.WriteHeader(http.StatusBadRequest)
		h.write(writer, Response{JSONRPC: JSONRPCVersion, Error: protocolError, ID: nullID()})
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		h.handleBatch(writer, request, trimmed)
		return
	}
	response, emit := h.handleOne(request.Context(), trimmed)
	if !emit {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	h.write(writer, response)
}

func (h *Handler) handleBatch(writer http.ResponseWriter, request *http.Request, body []byte) {
	var members []json.RawMessage
	if json.Unmarshal(body, &members) != nil || len(members) == 0 {
		writer.WriteHeader(http.StatusBadRequest)
		h.write(writer, Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeInvalidRequest, ""), ID: nullID()})
		return
	}
	if len(members) > h.limits.MaxBatch {
		writer.WriteHeader(http.StatusRequestEntityTooLarge)
		h.write(writer, Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeLimitExceeded, ""), ID: nullID()})
		return
	}
	responses := make([]Response, 0, len(members))
	for _, member := range members {
		response, emit := h.handleOne(request.Context(), member)
		if emit {
			responses = append(responses, response)
		}
	}
	if len(responses) == 0 {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	encoded, err := json.Marshal(responses)
	if err != nil || int64(len(encoded)) > h.limits.MaxResponseBytes {
		writer.WriteHeader(http.StatusInternalServerError)
		h.write(writer, Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeLimitExceeded, ""), ID: nullID()})
		return
	}
	_, _ = writer.Write(encoded)
}

func (h *Handler) handleOne(ctx context.Context, raw json.RawMessage) (Response, bool) {
	rpcRequest, err := decodeRequest(raw, h.limits)
	if err != nil {
		return Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(requestErrorCode(err), ""), ID: nullID()}, true
	}
	method, exists := h.methods[rpcRequest.Method]
	if !exists {
		if rpcRequest.Notification() {
			return Response{}, false
		}
		return Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeMethodNotFound, ""), ID: rpcRequest.ID}, true
	}
	if rpcRequest.Notification() && !method.Binding.AllowNotification {
		return Response{}, false
	}
	if ctx.Err() != nil {
		if rpcRequest.Notification() {
			return Response{}, false
		}
		return Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeCancelled, ""), ID: rpcRequest.ID}, true
	}
	result, rpcError := method.Handle(ctx, slices.Clone(rpcRequest.Params))
	if rpcRequest.Notification() {
		return Response{}, false
	}
	if rpcError != nil {
		safe := NewResponseError(rpcError.Code, rpcError.PublicCode)
		return Response{JSONRPC: JSONRPCVersion, Error: safe, ID: rpcRequest.ID}, true
	}
	if int64(len(result)) > h.limits.MaxResponseBytes {
		return Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeLimitExceeded, ""), ID: rpcRequest.ID}, true
	}
	if len(result) == 0 || protocolErrorFromResult(result, h.limits) != nil {
		return Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeInternalError, ""), ID: rpcRequest.ID}, true
	}
	return Response{JSONRPC: JSONRPCVersion, Result: slices.Clone(result), ID: rpcRequest.ID}, true
}

func (h *Handler) write(writer http.ResponseWriter, response Response) {
	encoded, err := marshalResponse(response, h.limits.MaxResponseBytes)
	if err != nil {
		encoded, _ = json.Marshal(Response{JSONRPC: JSONRPCVersion, Error: NewResponseError(CodeLimitExceeded, ""), ID: nullID()})
	}
	_, _ = writer.Write(encoded)
}

func readBoundedRequest(request *http.Request, maximum int64) ([]byte, error) {
	defer func() { _ = request.Body.Close() }()
	return readBounded(request.Body, maximum)
}

func validateJSONBytes(body []byte, limits Limits) *ResponseError {
	if len(bytes.TrimSpace(body)) == 0 {
		return NewResponseError(CodeParseError, "")
	}
	if err := protocolValidateRequestJSON(body, limits); err != nil {
		return NewResponseError(CodeParseError, "")
	}
	return nil
}

func protocolErrorFromResult(raw []byte, limits Limits) error {
	return validateStrictJSON(raw, limits)
}

func validateStrictJSON(raw []byte, limits Limits) error {
	return protocolValidateJSON(raw, limits)
}

func requestErrorCode(err error) int64 {
	switch errorCode(err) {
	case "JSONRPC_PARAMS_INVALID":
		return CodeInvalidParams
	default:
		return CodeInvalidRequest
	}
}

func mediaType(value string) string {
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = value[:index]
	}
	return strings.ToLower(strings.TrimSpace(value))
}
