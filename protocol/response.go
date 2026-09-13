package protocol

import (
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// PathSegment is one immutable string key or numeric list index in a response
// error path.
type PathSegment struct {
	key     string
	index   uint64
	isIndex bool
}

func (s PathSegment) Key() (string, bool) { return s.key, !s.isIndex }

func (s PathSegment) Index() (uint64, bool) { return s.index, s.isIndex }

// ErrorSource identifies the request source location responsible for a public
// response error.
type ErrorSource struct {
	pointer string
	line    uint64
	column  uint64
}

func (s ErrorSource) Pointer() string { return s.pointer }
func (s ErrorSource) Line() uint64    { return s.line }
func (s ErrorSource) Column() uint64  { return s.column }

// ResponseError is the immutable public error shape carried by a response.
type ResponseError struct {
	code      string
	message   string
	path      []PathSegment
	source    *ErrorSource
	retryable bool
	details   map[string]json.RawMessage
}

func (e ResponseError) Code() string    { return e.code }
func (e ResponseError) Message() string { return e.message }
func (e ResponseError) Retryable() bool { return e.retryable }

func (e ResponseError) Path() []PathSegment {
	return cloneSlice(e.path)
}

func (e ResponseError) Source() (ErrorSource, bool) {
	return pointedValue(e.source)
}

func (e ResponseError) Detail(namespace string) (json.RawMessage, bool) {
	return cloneRawLookup(e.details, namespace)
}

// Response is an immutable decoded single-operation response envelope.
type Response struct {
	id           string
	hasID        bool
	requestID    string
	data         json.RawMessage
	hasData      bool
	errors       []ResponseError
	capabilities []string
	negotiated   []NegotiatedExtension
	extensions   map[string]json.RawMessage
}

func (r *Response) ID() (string, bool) { return r.id, r.hasID }
func (r *Response) RequestID() string  { return r.requestID }

func (r *Response) Data() (json.RawMessage, bool) {
	return cloneRaw(r.data), r.hasData
}

func (r *Response) Errors() []ResponseError {
	return cloneSliceWith(r.errors, cloneResponseError)
}

func (r *Response) Capabilities() []string {
	return cloneSlice(r.capabilities)
}

func (r *Response) NegotiatedExtensions() []NegotiatedExtension {
	return cloneSlice(r.negotiated)
}

func (r *Response) Extension(namespace string) (json.RawMessage, bool) {
	return cloneRawLookup(r.extensions, namespace)
}

// DecodeResponse strictly decodes one transport-independent response.
func DecodeResponse(input []byte, options DecodeOptions) (*Response, error) {
	if err := validateExtensionOptions(options); err != nil {
		return nil, err
	}
	root, err := parseJSON(input, options.Limits)
	if err != nil {
		return nil, err
	}
	if err := expectKindPhase(input, root, nodeObject, "", "response object", "PROTO-100", "decode"); err != nil {
		return nil, err
	}
	allowed := map[string]bool{"id": true, "requestId": true, "data": true, "errors": true, "capabilities": true, "extensions": true}
	if err := rejectUnknown(input, root, allowed, "PROTO-100", "decode", ""); err != nil {
		return nil, err
	}
	response := &Response{extensions: make(map[string]json.RawMessage)}
	if err := decodeResponseIdentity(input, root, response); err != nil {
		return nil, err
	}
	decodeResponseData(input, root, response)
	if err := decodeResponseErrors(input, root, options, response, candidateResponseCapabilities(root)); err != nil {
		return nil, err
	}
	if err := decodeResponseCapabilities(input, root, options, response); err != nil {
		return nil, err
	}
	response.negotiated = negotiatedExtensions(response.capabilities, options.Extensions)
	if err := decodeResponseExtensions(input, root, options, response); err != nil {
		return nil, err
	}
	if !response.hasData && len(response.errors) == 0 {
		return nil, responseDiagnostic(input, "EMPTY_RESPONSE", "PROTO-102", "response requires data or errors", "", root.start)
	}
	return response, nil
}

func decodeResponseIdentity(input []byte, root node, response *Response) error {
	if id, exists := root.member("id"); exists {
		if err := validateResponseID(input, id, "/id", "client correlation id"); err != nil {
			return err
		}
		response.id, response.hasID = id.text, true
	}
	requestID, exists := root.member("requestId")
	if !exists {
		return responseDiagnostic(input, "MISSING_FIELD", "PROTO-101", "response requires requestId", "/requestId", root.start)
	}
	if err := validateResponseID(input, requestID, "/requestId", "server request id"); err != nil {
		return err
	}
	response.requestID = requestID.text
	return nil
}

func decodeResponseData(input []byte, root node, response *Response) {
	if data, exists := root.member("data"); exists {
		response.data = append(json.RawMessage(nil), input[data.start:data.end]...)
		response.hasData = true
	}
}

func candidateResponseCapabilities(root node) []string {
	capabilities, exists := root.member("capabilities")
	if !exists || capabilities.kind != nodeArray {
		return nil
	}
	result := make([]string, 0, len(capabilities.array))
	for _, capability := range capabilities.array {
		if capability.kind == nodeString {
			result = append(result, capability.text)
		}
	}
	return result
}

func decodeResponseErrors(input []byte, root node, options DecodeOptions, response *Response, capabilities []string) error {
	if errorNode, exists := root.member("errors"); exists {
		if errorNode.kind != nodeArray || len(errorNode.array) == 0 {
			return responseDiagnostic(input, "INVALID_ERRORS", "PROTO-103", "errors must be a non-empty array when present", "/errors", errorNode.start)
		}
		for index, current := range errorNode.array {
			responseError, err := decodeResponseError(input, current, joinPointer("/errors", intString(index)), options, capabilities)
			if err != nil {
				return err
			}
			response.errors = append(response.errors, responseError)
		}
	}
	return nil
}

func decodeResponseCapabilities(input []byte, root node, options DecodeOptions, response *Response) error {
	capabilities, exists := root.member("capabilities")
	if !exists {
		return responseDiagnostic(input, "MISSING_FIELD", "PROTO-105", "response requires capabilities array", "/capabilities", root.start)
	}
	if capabilities.kind != nodeArray {
		return responseDiagnostic(input, "TYPE_MISMATCH", "PROTO-105", "expected capabilities array", "/capabilities", capabilities.start)
	}
	seenCapabilities := make(map[string]bool, len(capabilities.array))
	for index, capability := range capabilities.array {
		pointer := joinPointer("/capabilities", intString(index))
		if capability.kind != nodeString || !capabilityPattern(capability.text) || seenCapabilities[capability.text] {
			return responseDiagnostic(input, "INVALID_CAPABILITY", "PROTO-105", "invalid or duplicate negotiated capability", pointer, capability.start)
		}
		if !options.Capabilities[capability.text] {
			return responseDiagnostic(input, "UNSUPPORTED_CAPABILITY", "PROTO-105", "response selected an unsupported capability", pointer, capability.start)
		}
		seenCapabilities[capability.text] = true
		response.capabilities = append(response.capabilities, capability.text)
	}
	return nil
}

func decodeResponseExtensions(input []byte, root node, options DecodeOptions, response *Response) error {
	extensions, exists := root.member("extensions")
	if !exists {
		return responseDiagnostic(input, "MISSING_FIELD", "PROTO-100", "response requires extensions object", "/extensions", root.start)
	}
	decoded, err := decodeNamespacedValues(input, extensions, "/extensions", "PROTO-100", options, response.capabilities)
	if err != nil {
		return err
	}
	response.extensions = decoded
	return nil
}

func decodeResponseError(input []byte, value node, pointer string, options DecodeOptions, capabilities []string) (ResponseError, error) {
	if value.kind != nodeObject {
		return ResponseError{}, responseDiagnostic(input, "TYPE_MISMATCH", "PROTO-103", "response error must be an object", pointer, value.start)
	}
	allowed := map[string]bool{"code": true, "message": true, "path": true, "source": true, "retryable": true, "details": true}
	if err := rejectUnknown(input, value, allowed, "PROTO-103", "decode", pointer); err != nil {
		return ResponseError{}, err
	}
	code, hasCode := value.member("code")
	message, hasMessage := value.member("message")
	path, hasPath := value.member("path")
	retryable, hasRetryable := value.member("retryable")
	if !hasCode || code.kind != nodeString || !identifierPattern.MatchString(code.text) || !hasMessage || message.kind != nodeString || message.text == "" || !hasPath || path.kind != nodeArray || !hasRetryable || retryable.kind != nodeBool {
		return ResponseError{}, responseDiagnostic(input, "INVALID_ERROR", "PROTO-103", "response error requires code, message, path, and retryable", pointer, value.start)
	}
	result := ResponseError{code: code.text, message: message.text, retryable: retryable.text == "true", details: make(map[string]json.RawMessage)}
	decodedPath, err := decodeResponsePath(input, path, pointer)
	if err != nil {
		return ResponseError{}, err
	}
	result.path = decodedPath
	if source, exists := value.member("source"); exists {
		decoded, err := decodeErrorSource(input, source, joinPointer(pointer, "source"))
		if err != nil {
			return ResponseError{}, err
		}
		result.source = &decoded
	}
	if details, exists := value.member("details"); exists {
		decoded, err := decodeResponseDetails(input, details, pointer, options, capabilities)
		if err != nil {
			return ResponseError{}, err
		}
		result.details = decoded
	}
	return result, nil
}

func decodeResponsePath(input []byte, path node, pointer string) ([]PathSegment, error) {
	result := make([]PathSegment, 0, len(path.array))
	for index, segment := range path.array {
		segmentPointer := joinPointer(joinPointer(pointer, "path"), intString(index))
		switch segment.kind {
		case nodeString:
			result = append(result, PathSegment{key: segment.text})
		case nodeNumber:
			parsed, err := strconv.ParseUint(segment.text, 10, 64)
			if err != nil {
				return nil, responseDiagnostic(input, "INVALID_ERROR_PATH", "PROTO-103", "path index must be an unsigned integer", segmentPointer, segment.start)
			}
			result = append(result, PathSegment{index: parsed, isIndex: true})
		case nodeNull, nodeBool, nodeArray, nodeObject:
			return nil, responseDiagnostic(input, "INVALID_ERROR_PATH", "PROTO-103", "path segment must be a string key or numeric index", segmentPointer, segment.start)
		}
	}
	return result, nil
}

func decodeResponseDetails(input []byte, details node, pointer string, options DecodeOptions, capabilities []string) (map[string]json.RawMessage, error) {
	return decodeNamespacedValues(input, details, joinPointer(pointer, "details"), "PROTO-103", options, capabilities)
}

func decodeNamespacedValues(input []byte, value node, pointer, clause string, options DecodeOptions, capabilities []string) (map[string]json.RawMessage, error) {
	if value.kind != nodeObject {
		return nil, responseDiagnostic(input, "TYPE_MISMATCH", clause, "namespaced values must be an object", pointer, value.start)
	}
	result := make(map[string]json.RawMessage, len(value.object))
	for _, member := range value.object {
		if !namespacePattern(member.name) {
			return nil, responseDiagnostic(input, "UNNEGOTIATED_EXTENSION", clause, "response namespace was not negotiated", joinPointer(pointer, member.name), member.start)
		}
		support, registered := options.Extensions[member.name]
		switch {
		case registered && containsCapability(capabilities, support.Capability):
			result[member.name] = append(json.RawMessage(nil), input[member.value.start:member.value.end]...)
		case registered && support.OptionalMetadata:
		case options.IgnorableExtensionMetadata[member.name]:
		case options.ExtensionNamespaces[member.name]:
			result[member.name] = append(json.RawMessage(nil), input[member.value.start:member.value.end]...)
		default:
			return nil, responseDiagnostic(input, "UNNEGOTIATED_EXTENSION", clause, "response namespace was not negotiated", joinPointer(pointer, member.name), member.start)
		}
	}
	return result, nil
}

func decodeErrorSource(input []byte, value node, pointer string) (ErrorSource, error) {
	if value.kind != nodeObject {
		return ErrorSource{}, responseDiagnostic(input, "TYPE_MISMATCH", "PROTO-103", "error source must be an object", pointer, value.start)
	}
	if err := rejectUnknown(input, value, map[string]bool{"pointer": true, "line": true, "column": true}, "PROTO-103", "decode", pointer); err != nil {
		return ErrorSource{}, err
	}
	line, hasLine := value.member("line")
	column, hasColumn := value.member("column")
	if !hasLine || !hasColumn {
		return ErrorSource{}, responseDiagnostic(input, "INVALID_ERROR_SOURCE", "PROTO-103", "error source requires line and column", pointer, value.start)
	}
	lineNumber, lineErr := positiveUint(line)
	columnNumber, columnErr := positiveUint(column)
	if lineErr != nil || columnErr != nil {
		return ErrorSource{}, responseDiagnostic(input, "INVALID_ERROR_SOURCE", "PROTO-103", "error source line and column must be positive integers", pointer, value.start)
	}
	result := ErrorSource{line: lineNumber, column: columnNumber}
	if sourcePointer, exists := value.member("pointer"); exists {
		if sourcePointer.kind != nodeString {
			return ErrorSource{}, responseDiagnostic(input, "INVALID_ERROR_SOURCE", "PROTO-103", "error source pointer must be a string", joinPointer(pointer, "pointer"), sourcePointer.start)
		}
		result.pointer = sourcePointer.text
	}
	return result, nil
}

func positiveUint(value node) (uint64, error) {
	if value.kind != nodeNumber {
		return 0, fmt.Errorf("not a number")
	}
	parsed, err := strconv.ParseUint(value.text, 10, 64)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("not a positive integer")
	}
	return parsed, nil
}

func validateResponseID(input []byte, value node, pointer, description string) error {
	if value.kind != nodeString || value.text == "" || utf8.RuneCountInString(value.text) > 128 {
		return responseDiagnostic(input, "INVALID_ID", "PROTO-101", description+" must contain 1..128 Unicode scalars", pointer, value.start)
	}
	return nil
}

func responseDiagnostic(input []byte, code, clause, message, pointer string, offset int) *Diagnostic {
	return newDiagnostic(input, code, clause, "decode", message, pointer, offset)
}

func cloneResponseError(input ResponseError) ResponseError {
	result := input
	result.path = append([]PathSegment(nil), input.path...)
	result.details = make(map[string]json.RawMessage, len(input.details))
	for namespace, raw := range input.details {
		result.details[namespace] = append(json.RawMessage(nil), raw...)
	}
	if input.source != nil {
		source := *input.source
		result.source = &source
	}
	return result
}
