package protobufrpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	connectUnaryMediaType  = "application/proto"
	connectStreamMediaType = "application/connect+proto"
	connectEndStreamFlag   = byte(0x02)
)

func validateConnectURL(value string, method protoreflect.MethodDescriptor) error {
	endpoint, err := url.Parse(value)
	if err != nil || method == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.EscapedPath() != fullMethod(method) {
		return errors.New("invalid Connect endpoint")
	}
	return nil
}

func (a *Adapter) invokeConnect(ctx context.Context, input proto.Message) (proto.Message, error) {
	body, err := proto.Marshal(input)
	if err != nil {
		return nil, publicError("PROTO_RPC_REQUEST_INVALID", string(CodeInvalidArgument))
	}
	response, err := a.doConnect(ctx, bytes.NewReader(body), connectUnaryMediaType)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, connectUnaryFailure(response, a.limits.MaxResponseBytes)
	}
	if err := validateConnectResponse(response, connectUnaryMediaType); err != nil {
		return nil, err
	}
	payload, err := readLimited(response.Body, int64(a.limits.MaxResponseBytes))
	if err != nil {
		return nil, publicError("PROTO_RPC_RESPONSE_LIMIT", string(CodeResourceExhausted))
	}
	if err := rejectConnectTrailers(response.Trailer); err != nil {
		return nil, err
	}
	output := dynamicpb.NewMessage(a.method.Output())
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, output); err != nil {
		return nil, publicError("PROTO_RPC_RESPONSE_INVALID", string(CodeDataLoss))
	}
	return output, nil
}

func (a *Adapter) openConnectStream(ctx context.Context, input proto.Message) (*Stream, error) {
	payload, err := proto.Marshal(input)
	if err != nil {
		return nil, publicError("PROTO_RPC_REQUEST_INVALID", string(CodeInvalidArgument))
	}
	framed := frame(0, payload)
	response, err := a.doConnect(ctx, bytes.NewReader(framed), connectStreamMediaType)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer func() { _ = response.Body.Close() }()
		return nil, connectUnaryFailure(response, a.limits.MaxResponseBytes)
	}
	if err := validateConnectResponse(response, connectStreamMediaType); err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	source := &connectSource{body: response.Body, trailers: response.Trailer, output: a.method.Output(), maxMessage: a.limits.MaxResponseBytes}
	return &Stream{limits: a.limits, recv: source.recv, close: source.close}, nil
}

func (a *Adapter) doConnect(ctx context.Context, body io.Reader, mediaType string) (*http.Response, error) {
	outgoing, headers, err := a.outgoingMetadata(ctx)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(outgoing, http.MethodPost, a.connectURL, body)
	if err != nil {
		return nil, publicError("PROTO_RPC_REQUEST_INVALID", string(CodeInvalidArgument))
	}
	request.Header.Set("Content-Type", mediaType)
	request.Header.Set("Accept", mediaType)
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Connect-Accept-Encoding", "identity")
	if timeout := deadlineHeader(ctx); timeout != "" {
		request.Header.Set("Connect-Timeout-Ms", timeout)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := a.http.Do(request)
	if err != nil {
		if ctx.Err() == context.Canceled {
			return nil, publicError("PROTO_RPC_CANCELLED", string(CodeCanceled))
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, publicError("PROTO_RPC_DEADLINE_EXCEEDED", string(CodeDeadlineExceeded))
		}
		if strings.Contains(err.Error(), "redirect rejected") {
			return nil, publicError("PROTO_RPC_REDIRECT_BLOCKED", string(CodeUnavailable))
		}
		return nil, publicError("PROTO_RPC_TRANSPORT_FAILURE", string(CodeUnavailable))
	}
	if response.Body == nil {
		return nil, publicError("PROTO_RPC_RESPONSE_INVALID", string(CodeDataLoss))
	}
	return response, nil
}

type connectSource struct {
	body       io.ReadCloser
	trailers   http.Header
	output     protoreflect.MessageDescriptor
	maxMessage int
	mu         sync.Mutex
	ended      bool
}

func (s *connectSource) recv() (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, io.EOF
	}
	flags, payload, err := readFrame(s.body, s.maxMessage)
	if err != nil {
		return nil, err
	}
	if flags&^connectEndStreamFlag != 0 {
		return nil, publicError("PROTO_RPC_COMPRESSION_UNSUPPORTED", string(CodeUnimplemented))
	}
	if flags&connectEndStreamFlag != 0 {
		s.ended = true
		if err := parseEndStream(payload); err != nil {
			return nil, err
		}
		if err := exhaustConnectStream(s.body, s.trailers); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	message := dynamicpb.NewMessage(s.output)
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, message); err != nil {
		return nil, publicError("PROTO_RPC_RESPONSE_INVALID", string(CodeDataLoss))
	}
	return message, nil
}

func (s *connectSource) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.body == nil {
		return nil
	}
	err := s.body.Close()
	s.body = nil
	return err
}

type connectErrorEnvelope struct {
	Code    RPCCode           `json:"code"`
	Message string            `json:"message,omitempty"`
	Details []json.RawMessage `json:"details,omitempty"`
}

func connectUnaryFailure(response *http.Response, maximum int) error {
	if err := validateConnectResponse(response, "application/json"); err != nil {
		return err
	}
	payload, err := readLimited(response.Body, int64(maximum))
	if err != nil {
		return publicError("PROTO_RPC_RESPONSE_LIMIT", string(CodeResourceExhausted))
	}
	if err := rejectConnectTrailers(response.Trailer); err != nil {
		return err
	}
	return decodeConnectError(payload, httpRPCCode(response.StatusCode))
}

func decodeConnectError(payload []byte, fallback RPCCode) error {
	var envelope connectErrorEnvelope
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF || !validRPCCode(envelope.Code) {
		return publicError("PROTO_RPC_STATUS_INVALID", string(fallback))
	}
	if len(envelope.Details) != 0 {
		return publicError("PROTO_RPC_STATUS_DETAILS_UNSUPPORTED", string(envelope.Code))
	}
	return publicError("PROTO_RPC_STATUS", string(envelope.Code))
}

func parseEndStream(payload []byte) error {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("{}")) {
		return nil
	}
	var envelope struct {
		Error    *connectErrorEnvelope `json:"error,omitempty"`
		Metadata map[string][]string   `json:"metadata,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return publicError("PROTO_RPC_STATUS_INVALID", string(CodeUnknown))
	}
	if len(envelope.Metadata) != 0 {
		return publicError("PROTO_RPC_TRAILER_METADATA_UNSUPPORTED", string(CodeUnknown))
	}
	if envelope.Error == nil {
		return nil
	}
	raw, _ := json.Marshal(envelope.Error)
	return decodeConnectError(raw, CodeUnknown)
}

func readFrame(reader io.Reader, maximum int) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil, publicError("PROTO_RPC_STREAM_TRUNCATED", string(CodeDataLoss))
		}
		return 0, nil, publicError("PROTO_RPC_STREAM_TRUNCATED", string(CodeDataLoss))
	}
	size := binary.BigEndian.Uint32(header[1:])
	if uint64(size) > uint64(maximum) {
		return 0, nil, publicError("PROTO_RPC_RESPONSE_LIMIT", string(CodeResourceExhausted))
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, publicError("PROTO_RPC_STREAM_TRUNCATED", string(CodeDataLoss))
	}
	return header[0], payload, nil
}

func frame(flags byte, payload []byte) []byte {
	result := make([]byte, 5+len(payload))
	result[0] = flags
	binary.BigEndian.PutUint32(result[1:5], uint32(len(payload)))
	copy(result[5:], payload)
	return result
}

func readLimited(reader io.Reader, maximum int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil || int64(len(payload)) > maximum {
		return nil, errors.New("limit exceeded")
	}
	return payload, nil
}

func validMediaType(value, expected string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, expected) && len(parameters) == 0
}

func validateConnectResponse(response *http.Response, mediaType string) error {
	if !validMediaType(response.Header.Get("Content-Type"), mediaType) {
		return publicError("PROTO_RPC_MEDIA_TYPE_UNSUPPORTED", string(CodeDataLoss))
	}
	if response.Uncompressed {
		return publicError("PROTO_RPC_COMPRESSION_UNSUPPORTED", string(CodeUnimplemented))
	}
	for name, values := range response.Header {
		switch strings.ToLower(name) {
		case "content-type", "content-length", "date":
			// These are HTTP representation metadata, not application metadata.
		case "content-encoding":
			if len(values) != 1 || !strings.EqualFold(strings.TrimSpace(values[0]), "identity") {
				return publicError("PROTO_RPC_COMPRESSION_UNSUPPORTED", string(CodeUnimplemented))
			}
		case "connect-content-encoding", "connect-accept-encoding":
			return publicError("PROTO_RPC_COMPRESSION_UNSUPPORTED", string(CodeUnimplemented))
		default:
			return publicError("PROTO_RPC_RESPONSE_METADATA_UNSUPPORTED", string(CodeDataLoss))
		}
	}
	return rejectConnectTrailers(response.Trailer)
}

func rejectConnectTrailers(trailers http.Header) error {
	if len(trailers) != 0 {
		return publicError("PROTO_RPC_TRAILER_METADATA_UNSUPPORTED", string(CodeDataLoss))
	}
	return nil
}

func exhaustConnectStream(body io.Reader, trailers http.Header) error {
	var extra [1]byte
	count, err := body.Read(extra[:])
	if count != 0 || err == nil {
		return publicError("PROTO_RPC_STREAM_INVALID", string(CodeDataLoss))
	}
	if !errors.Is(err, io.EOF) {
		return publicError("PROTO_RPC_STREAM_TRUNCATED", string(CodeDataLoss))
	}
	return rejectConnectTrailers(trailers)
}

func httpRPCCode(status int) RPCCode {
	switch status {
	case http.StatusBadRequest:
		return CodeInvalidArgument
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusNotFound:
		return CodeUnimplemented
	case http.StatusTooManyRequests:
		return CodeResourceExhausted
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return CodeUnavailable
	default:
		return CodeUnknown
	}
}

func validRPCCode(code RPCCode) bool {
	switch code {
	case CodeCanceled, CodeUnknown, CodeInvalidArgument, CodeDeadlineExceeded, CodeNotFound,
		CodeAlreadyExists, CodePermissionDenied, CodeResourceExhausted, CodeFailedPrecondition,
		CodeAborted, CodeOutOfRange, CodeUnimplemented, CodeInternal, CodeUnavailable,
		CodeDataLoss, CodeUnauthenticated:
		return true
	default:
		return false
	}
}
