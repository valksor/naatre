package mcpadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/valksor/naatre/protocol"
)

const (
	rpcParseError     int64 = -32700
	rpcInvalidRequest int64 = -32600
	rpcMethodNotFound int64 = -32601
	rpcInvalidParams  int64 = -32602
	rpcInternalError  int64 = -32603
	rpcUnauthorized   int64 = -32001
	rpcCancelled      int64 = -32002
	rpcLimitExceeded  int64 = -32003
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r rpcRequest) notification() bool { return len(r.ID) == 0 }

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int64        `json:"code"`
	Message string       `json:"message"`
	Data    rpcErrorData `json:"data"`
}

type rpcErrorData struct {
	Code string `json:"code"`
}

func publicRPCError(code int64, publicCode string) *rpcError {
	if !validPublicCode(publicCode) {
		code, publicCode = rpcInternalError, CodeInternal
	}
	return &rpcError{Code: code, Message: rpcMessage(code), Data: rpcErrorData{Code: publicCode}}
}

func validPublicCode(code string) bool {
	if len(code) < 3 || len(code) > 64 || code[0] < 'A' || code[0] > 'Z' {
		return false
	}
	for _, value := range code[1:] {
		if value != '_' && (value < 'A' || value > 'Z') && (value < '0' || value > '9') {
			return false
		}
	}
	return true
}

func rpcMessage(code int64) string {
	switch code {
	case rpcParseError:
		return "Parse error"
	case rpcInvalidRequest:
		return "Invalid Request"
	case rpcMethodNotFound:
		return "Method not found"
	case rpcInvalidParams:
		return "Invalid params"
	case rpcUnauthorized:
		return "Unauthorized"
	case rpcCancelled:
		return "Cancelled"
	case rpcLimitExceeded:
		return "Resource limit exceeded"
	default:
		return "Internal error"
	}
}

func decodeRPCRequest(input []byte, limits WireLimits) (rpcRequest, error) {
	if len(input) == 0 || len(input) > limits.MaxRequestBytes {
		return rpcRequest{}, adapterError(CodeRequestInvalid, errors.New("request frame is absent or oversized"))
	}
	if err := protocol.ValidateJSON(input, protocol.Limits{MaxBytes: limits.MaxRequestBytes, MaxDepth: 32, MaxMembers: 8, MaxArrayItems: 4096, MaxStringBytes: limits.MaxRequestBytes, MaxNumberBytes: 64}); err != nil {
		return rpcRequest{}, adapterError(CodeRequestInvalid, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var request rpcRequest
	if err := decoder.Decode(&request); err != nil || request.JSONRPC != JSONRPCVersion || len(request.Method) == 0 || len(request.Method) > 128 {
		return rpcRequest{}, adapterError(CodeRequestInvalid, errors.New("request envelope is invalid"))
	}
	if len(request.ID) != 0 && !validRPCID(request.ID) {
		return rpcRequest{}, adapterError(CodeRequestInvalid, errors.New("request ID is invalid"))
	}
	request.ID = slices.Clone(request.ID)
	request.Params = slices.Clone(request.Params)
	return request, nil
}

func decodeRPCResponse(input, expectedID []byte, limits WireLimits) (rpcResponse, error) {
	if len(input) == 0 || len(input) > limits.MaxResponseBytes {
		return rpcResponse{}, adapterError(CodeResponseTooLarge, errors.New("response frame is absent or oversized"))
	}
	if err := protocol.ValidateJSON(input, protocol.Limits{MaxBytes: limits.MaxResponseBytes, MaxDepth: 64, MaxMembers: 16, MaxArrayItems: 4096, MaxStringBytes: limits.MaxResponseBytes, MaxNumberBytes: 64}); err != nil {
		return rpcResponse{}, adapterError(CodeResponseInvalid, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var response rpcResponse
	if err := decoder.Decode(&response); err != nil || response.JSONRPC != JSONRPCVersion || !validRPCID(response.ID) || !sameRPCID(response.ID, expectedID) || (len(response.Result) == 0) == (response.Error == nil) {
		return rpcResponse{}, adapterError(CodeResponseInvalid, errors.New("response envelope is invalid"))
	}
	if response.Error != nil {
		if response.Error.Message != rpcMessage(response.Error.Code) || !validPublicCode(response.Error.Data.Code) {
			return rpcResponse{}, adapterError(CodeResponseInvalid, errors.New("response error is unsafe"))
		}
		return rpcResponse{}, adapterError(response.Error.Data.Code, errors.New("remote MCP failure"))
	}
	response.Result = slices.Clone(response.Result)
	return response, nil
}

func validRPCID(input []byte) bool {
	if len(input) == 0 || len(input) > 128 {
		return false
	}
	trimmed := bytes.TrimSpace(input)
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	if len(trimmed) > 1 && trimmed[0] == '"' {
		var value string
		return json.Unmarshal(trimmed, &value) == nil && boundedIdentity(value)
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	return decoder.Decode(&number) == nil && !bytes.ContainsAny(trimmed, ".eE")
}

func sameRPCID(left, right []byte) bool {
	leftCanonical, leftErr := protocol.CanonicalizeJSON(left, protocol.Limits{MaxBytes: 128, MaxDepth: 1})
	rightCanonical, rightErr := protocol.CanonicalizeJSON(right, protocol.Limits{MaxBytes: 128, MaxDepth: 1})
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func marshalRPC(value any, maximum int, code string) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maximum {
		return nil, adapterError(code, errors.New("JSON-RPC frame exceeds the configured limit"))
	}
	return encoded, nil
}

func rpcIDKey(input []byte) string {
	canonical, err := protocol.CanonicalizeJSON(input, protocol.Limits{MaxBytes: 128, MaxDepth: 1})
	if err != nil {
		return ""
	}
	return string(canonical)
}

func numericRPCID(value uint64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf("%d", value))
}

func nullRPCID() json.RawMessage { return json.RawMessage("null") }

// CodeOf returns a stable public code for any adapter failure.
func CodeOf(err error) string {
	var failure *Error
	if errors.As(err, &failure) && validPublicCode(failure.Code) {
		return failure.Code
	}
	return CodeInternal
}
