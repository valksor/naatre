package openrpcadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/valksor/naatre/protocol"
)

const (
	CodeParseError     int64 = -32700
	CodeInvalidRequest int64 = -32600
	CodeMethodNotFound int64 = -32601
	CodeInvalidParams  int64 = -32602
	CodeInternalError  int64 = -32603
	CodeUnauthorized   int64 = -32001
	CodeCancelled      int64 = -32002
	CodeLimitExceeded  int64 = -32003
)

// Request retains the exact JSON-RPC ID and parameter JSON. A nil ID is a
// notification; an explicit JSON null ID is not collapsed into absence.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
}

func (r Request) Notification() bool { return len(r.ID) == 0 }

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

// ResponseError contains only standard text and an optional stable public
// code. Arbitrary upstream or application error data is never forwarded.
type ResponseError struct {
	Code       int64  `json:"code"`
	Message    string `json:"message"`
	PublicCode string `json:"data,omitempty"`
}

func NewResponseError(code int64, publicCode string) *ResponseError {
	if !validResponseError(code, publicCode) {
		return &ResponseError{Code: CodeInternalError, Message: messageForCode(CodeInternalError)}
	}
	return &ResponseError{Code: code, Message: messageForCode(code), PublicCode: publicCode}
}

func validResponseError(code int64, publicCode string) bool {
	standard := code == CodeParseError || code == CodeInvalidRequest || code == CodeMethodNotFound || code == CodeInvalidParams || code == CodeInternalError || code == CodeUnauthorized || code == CodeCancelled || code == CodeLimitExceeded
	if standard {
		return publicCode == "" || validPublicCode(publicCode)
	}
	return code < -32768 && validPublicCode(publicCode)
}

func messageForCode(code int64) string {
	switch code {
	case CodeParseError:
		return "Parse error"
	case CodeInvalidRequest:
		return "Invalid Request"
	case CodeMethodNotFound:
		return "Method not found"
	case CodeInvalidParams:
		return "Invalid params"
	case CodeUnauthorized:
		return "Unauthorized"
	case CodeCancelled:
		return "Cancelled"
	case CodeLimitExceeded:
		return "Resource limit exceeded"
	default:
		return "Internal error"
	}
}

func decodeRequest(raw json.RawMessage, limits Limits) (Request, error) {
	if err := protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: int(limits.MaxRequestBytes), MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxParameters + 4, MaxArrayItems: limits.MaxParameters}); err != nil {
		return Request{}, adapterError("JSONRPC_REQUEST_INVALID")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, adapterError("JSONRPC_REQUEST_INVALID")
	}
	if request.JSONRPC != JSONRPCVersion || !methodPattern.MatchString(request.Method) || request.Method == "" {
		return Request{}, adapterError("JSONRPC_REQUEST_INVALID")
	}
	if len(request.ID) != 0 && !validID(request.ID, limits) {
		return Request{}, adapterError("JSONRPC_ID_INVALID")
	}
	if len(request.Params) != 0 {
		trimmed := bytes.TrimSpace(request.Params)
		if len(trimmed) == 0 || trimmed[0] != '{' && trimmed[0] != '[' {
			return Request{}, adapterError("JSONRPC_PARAMS_INVALID")
		}
	}
	request.ID = slices.Clone(request.ID)
	request.Params = slices.Clone(request.Params)
	return request, nil
}

func decodeResponse(raw json.RawMessage, expectedID json.RawMessage, limits Limits) (Response, error) {
	if err := protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: int(limits.MaxResponseBytes), MaxDepth: limits.MaxDepth}); err != nil {
		return Response{}, adapterError("JSONRPC_RESPONSE_INVALID")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var response Response
	if err := decoder.Decode(&response); err != nil || response.JSONRPC != JSONRPCVersion || !validID(response.ID, limits) {
		return Response{}, adapterError("JSONRPC_RESPONSE_INVALID")
	}
	if (len(response.Result) == 0) == (response.Error == nil) {
		return Response{}, adapterError("JSONRPC_ENVELOPE_INVALID")
	}
	if !sameJSON(response.ID, expectedID, limits) {
		return Response{}, adapterError("JSONRPC_ID_MISMATCH")
	}
	if response.Error != nil {
		if !validResponseError(response.Error.Code, response.Error.PublicCode) || response.Error.Message != messageForCode(response.Error.Code) {
			return Response{}, adapterError("JSONRPC_ERROR_UNSAFE")
		}
		return Response{}, adapterError(publicErrorCode(response.Error))
	}
	response.Result = slices.Clone(response.Result)
	response.ID = slices.Clone(response.ID)
	return response, nil
}

func validID(raw json.RawMessage, limits Limits) bool {
	if len(raw) == 0 || protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: 128, MaxDepth: 1, MaxStringBytes: 128, MaxNumberBytes: 64}) != nil {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		return json.Unmarshal(trimmed, &value) == nil && len(value) <= 128
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil {
		return false
	}
	text := number.String()
	return len(text) <= 64 && !bytes.ContainsAny(trimmed, ".eE")
}

func sameJSON(left, right json.RawMessage, limits Limits) bool {
	leftCanonical, leftErr := protocol.CanonicalizeJSON(left, protocol.Limits{MaxBytes: 128, MaxDepth: limits.MaxDepth})
	rightCanonical, rightErr := protocol.CanonicalizeJSON(right, protocol.Limits{MaxBytes: 128, MaxDepth: limits.MaxDepth})
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func publicErrorCode(response *ResponseError) string {
	if response.PublicCode != "" {
		return "JSONRPC_REMOTE_" + response.PublicCode
	}
	switch response.Code {
	case CodeParseError:
		return "JSONRPC_REMOTE_PARSE_ERROR"
	case CodeInvalidRequest:
		return "JSONRPC_REMOTE_INVALID_REQUEST"
	case CodeMethodNotFound:
		return "JSONRPC_REMOTE_METHOD_NOT_FOUND"
	case CodeInvalidParams:
		return "JSONRPC_REMOTE_INVALID_PARAMS"
	case CodeUnauthorized:
		return "JSONRPC_REMOTE_UNAUTHORIZED"
	case CodeCancelled:
		return "JSONRPC_REMOTE_CANCELLED"
	case CodeLimitExceeded:
		return "JSONRPC_REMOTE_LIMIT"
	default:
		return "JSONRPC_REMOTE_INTERNAL"
	}
}

func marshalRequest(request Request, maximum int64) ([]byte, error) {
	encoded, err := json.Marshal(request)
	if err != nil || int64(len(encoded)) > maximum {
		return nil, adapterError("JSONRPC_REQUEST_LIMIT")
	}
	return encoded, nil
}

func marshalResponse(response Response, maximum int64) ([]byte, error) {
	encoded, err := json.Marshal(response)
	if err != nil || int64(len(encoded)) > maximum {
		return nil, adapterError("JSONRPC_RESPONSE_LIMIT")
	}
	return encoded, nil
}

func nullID() json.RawMessage { return json.RawMessage("null") }

func formatNumericID(value uint64) json.RawMessage { return json.RawMessage(fmt.Sprintf("%d", value)) }
