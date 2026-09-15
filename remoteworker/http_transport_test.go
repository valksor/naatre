package remoteworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeTrackingBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestHTTPTransportExecutesHTTP2UnaryHandshake(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		kind, payload := decodeHTTPRequest(t, request)
		switch kind {
		case "register":
			var registration Registration
			if err := json.Unmarshal(payload, &registration); err != nil {
				t.Fatal(err)
			}
			return framedHTTPResponse(t, request, "registered", RegistrationAck{
				Protocol: ProtocolVersion, WorkerID: registration.WorkerID, SessionID: "session-http2",
				SchemaRevision: registration.SchemaRevision, AcceptedCapabilities: registration.Capabilities,
			}), nil
		case "invoke":
			var invocation WorkerInvocation
			if err := json.Unmarshal(payload, &invocation); err != nil {
				t.Fatal(err)
			}
			return framedHTTPResponse(t, request, "result", fixtureResult(json.RawMessage(`{"greeting":"Hello, Ada"}`)).forInvocation(invocation)), nil
		case "cancel":
			var cancellation CancelRequest
			if err := json.Unmarshal(payload, &cancellation); err != nil {
				t.Fatal(err)
			}
			return framedHTTPResponse(t, request, "cancelled", CancellationAck{Protocol: ProtocolVersion, InvocationID: cancellation.InvocationID, Disposition: CancellationAcknowledged}), nil
		default:
			t.Fatalf("unexpected request kind %q", kind)
			return nil, nil
		}
	})}
	transport, err := NewHTTPTransport(HTTPTransportConfig{Client: client, Endpoint: "https://worker.example/base", MaxFrameBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	gateway := newTestGateway(t, transport, 1)
	registration := fixtureRegistration()
	registration.Endpoint = "fixture-stdio"
	if err := gateway.Register(context.Background(), registration); err != nil {
		t.Fatalf("Register: %v", err)
	}
	result, err := gateway.Invoke(context.Background(), fixtureRequest("fixture.greet"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(result.Data) != `{"greeting":"Hello, Ada"}` {
		t.Fatalf("result data = %s", result.Data)
	}
	ack, err := transport.Cancel(context.Background(), CancelRequest{Protocol: ProtocolVersion, RequestID: "request-1", InvocationID: "invocation-1"})
	if err != nil || ack.Disposition != CancellationAcknowledged {
		t.Fatalf("Cancel = %#v, %v", ack, err)
	}
}

func TestHTTPTransportRejectsUnsafeEndpointsAndFrameLimits(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe or oversized request reached the transport")
		return nil, nil
	})}
	for _, endpoint := range []string{"http://worker.example", "https://user:pass@worker.example", "https://worker.example?tenant=private", "https://worker.example/#fragment"} {
		if _, err := NewHTTPTransport(HTTPTransportConfig{Client: client, Endpoint: endpoint, MaxFrameBytes: 64}); err == nil {
			t.Fatalf("NewHTTPTransport(%q) succeeded", endpoint)
		}
	}
	transport, err := NewHTTPTransport(HTTPTransportConfig{Client: client, Endpoint: "https://worker.example", MaxFrameBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.Invoke(context.Background(), WorkerInvocation{Protocol: ProtocolVersion, Input: json.RawMessage(strings.Repeat("x", 128))})
	var delivery *DeliveryError
	if !errors.As(err, &delivery) || delivery.Phase != DeliveryBeforeWrite {
		t.Fatalf("oversized invocation error = %v", err)
	}
}

func TestHTTPTransportRejectsProtocolDowngradeAndRedactsPrivateFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response func(*http.Request) *http.Response
	}{
		{
			name: "http1 downgrade",
			response: func(request *http.Request) *http.Response {
				response := framedHTTPResponse(t, request, "result", fixtureResult(json.RawMessage(`{"greeting":"Hello"}`)))
				response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
				return response
			},
		},
		{
			name: "private status diagnostics",
			response: func(request *http.Request) *http.Response {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable, Proto: "HTTP/2.0", ProtoMajor: 2,
					Header: make(http.Header), Body: io.NopCloser(strings.NewReader("authorization: Bearer private-credential")), Request: request,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				_, _ = io.Copy(io.Discard, request.Body)
				return test.response(request), nil
			})}
			transport, err := NewHTTPTransport(HTTPTransportConfig{Client: client, Endpoint: "https://worker.example", MaxFrameBytes: 4096})
			if err != nil {
				t.Fatal(err)
			}
			gateway := newTestGateway(t, transport, 1)
			gateway.registration = func() *Registration { value := fixtureRegistration(); return &value }()
			gateway.handlers["fixture.greet"] = fixtureRegistration().Handlers[0]
			gateway.maxInFlight, gateway.maxRequestBytes, gateway.maxResponseBytes = 1, 4096, 4096
			_, invokeErr := gateway.Invoke(context.Background(), fixtureRequest("fixture.greet"))
			assertGatewayCode(t, invokeErr, CodeMalformedWorkerData)
			if strings.Contains(invokeErr.Error(), "private-credential") || strings.Contains(invokeErr.Error(), "worker.example") {
				t.Fatalf("public error exposed private diagnostics: %v", invokeErr)
			}
		})
	}
}

func TestHTTPTransportClassifiesProcessDeathByWriteBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		read  bool
		phase DeliveryPhase
	}{
		{name: "before write", phase: DeliveryBeforeWrite},
		{name: "after write", read: true, phase: DeliveryAfterWrite},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if test.read {
					_, _ = io.Copy(io.Discard, request.Body)
				}
				return nil, errors.New("private process death")
			})}
			transport, err := NewHTTPTransport(HTTPTransportConfig{Client: client, Endpoint: "https://worker.example", MaxFrameBytes: 4096})
			if err != nil {
				t.Fatal(err)
			}
			_, invokeErr := transport.Invoke(context.Background(), WorkerInvocation{Protocol: ProtocolVersion})
			var delivery *DeliveryError
			if !errors.As(invokeErr, &delivery) || delivery.Phase != test.phase {
				t.Fatalf("error = %v, want delivery phase %s", invokeErr, test.phase)
			}
			if errors.Unwrap(invokeErr) != nil || strings.Contains(invokeErr.Error(), "private process death") {
				t.Fatalf("delivery error exposed private process details: %v", invokeErr)
			}
		})
	}
}

func TestHTTPTransportStreamsWithBoundedBackpressure(t *testing.T) {
	t.Parallel()
	body := &closeTrackingBody{Reader: bytes.NewReader(streamHTTPBody(t,
		protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "invocation-stream", Sequence: 1, SchemaRevision: "schema-1"},
		protocol.StreamFrame{Type: protocol.StreamData, Stream: "invocation-stream", Sequence: 2, Data: json.RawMessage(`{"greeting":"first"}`)},
		protocol.StreamFrame{Type: protocol.StreamData, Stream: "invocation-stream", Sequence: 3, Data: json.RawMessage(`{"greeting":"second"}`)},
	))}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get(StreamCreditFramesHeader) != "2" || request.Header.Get(StreamCreditBytesHeader) != "65536" {
			t.Fatalf("stream credit headers = %q/%q", request.Header.Get(StreamCreditFramesHeader), request.Header.Get(StreamCreditBytesHeader))
		}
		_, _ = io.Copy(io.Discard, request.Body)
		return &http.Response{
			StatusCode: http.StatusOK, Proto: "HTTP/2.0", ProtoMajor: 2, Header: workerHeaders(), Body: body, Request: request,
		}, nil
	})}
	transport, err := NewHTTPTransport(HTTPTransportConfig{Client: client, Endpoint: "https://worker.example", MaxFrameBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	invocation := WorkerInvocation{Protocol: ProtocolVersion, InvocationID: "invocation-stream", AttemptID: "invocation-stream.1", SchemaRevision: "schema-1"}
	stream, err := transport.OpenStream(context.Background(), invocation, StreamCredit{Frames: 2, Bytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatalf("Next within credit: %v", err)
		}
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("Next beyond credit = %v", err)
	}
	if !body.closed.Load() {
		t.Fatal("backpressure failure did not close response body")
	}
}

func decodeHTTPRequest(t *testing.T, request *http.Request) (string, json.RawMessage) {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.Scheme != "https" || request.Header.Get("Content-Type") != WorkerMediaType ||
		request.Header.Get(ProtocolHeader) != ProtocolVersion || request.Header.Get("Accept-Encoding") != "identity" {
		t.Fatalf("invalid worker request: %s %s %#v", request.Method, request.URL, request.Header)
	}
	frame, err := ReadFrame(request.Body, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Protocol string          `json:"protocol"`
		Kind     string          `json:"kind"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Protocol != ProtocolVersion {
		t.Fatalf("protocol = %q", envelope.Protocol)
	}
	return envelope.Kind, envelope.Payload
}

func framedHTTPResponse(t *testing.T, request *http.Request, kind string, payload any) *http.Response {
	t.Helper()
	encoded, err := encodeEnvelope(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	var framed bytes.Buffer
	if err := WriteFrame(&framed, FrameData, encoded, 64<<10); err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Proto: "HTTP/2.0", ProtoMajor: 2, Header: workerHeaders(), Body: io.NopCloser(&framed), Request: request}
}

func workerHeaders() http.Header {
	header := make(http.Header)
	header.Set("Content-Type", WorkerMediaType)
	header.Set(ProtocolHeader, ProtocolVersion)
	return header
}

func streamHTTPBody(t *testing.T, frames ...protocol.StreamFrame) []byte {
	t.Helper()
	var result bytes.Buffer
	for _, frame := range frames {
		encodedFrame, err := protocol.MarshalStreamFrame(frame, protocol.DefaultStreamLimits())
		if err != nil {
			t.Fatal(err)
		}
		payload := WorkerStreamFrame{
			Protocol: ProtocolVersion, InvocationID: "invocation-stream", AttemptID: "invocation-stream.1",
			SchemaRevision: "schema-1", Frame: encodedFrame,
		}
		encoded, err := encodeEnvelope("stream", payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteFrame(&result, FrameData, encoded, 64<<10); err != nil {
			t.Fatal(err)
		}
	}
	return result.Bytes()
}
