package remoteworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	WorkerMediaType          = "application/naatre-worker+json"
	ProtocolHeader           = "Naatre-Worker-Protocol"
	StreamCreditFramesHeader = "Naatre-Stream-Credit-Frames"
	StreamCreditBytesHeader  = "Naatre-Stream-Credit-Bytes"

	registerPath = "/naatre.remote-worker.v1.Worker/Register"
	invokePath   = "/naatre.remote-worker.v1.Worker/Invoke"
	cancelPath   = "/naatre.remote-worker.v1.Worker/Cancel"
)

type HTTPTransportConfig struct {
	Client        *http.Client
	Endpoint      string
	MaxFrameBytes uint32
}

// HTTPTransport is the production TLS HTTP/2 transport for a single pinned
// worker endpoint. The supplied client owns mTLS identity and trust roots.
type HTTPTransport struct {
	client   *http.Client
	endpoint *url.URL
	maximum  uint32
}

func NewHTTPTransport(config HTTPTransportConfig) (*HTTPTransport, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || config.Client == nil || config.MaxFrameBytes == 0 || endpoint.Scheme != "https" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("remote-worker HTTP transport requires a pinned HTTPS endpoint, client, and finite frame limit")
	}
	client := *config.Client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPTransport{client: &client, endpoint: endpoint, maximum: config.MaxFrameBytes}, nil
}

func (t *HTTPTransport) Register(ctx context.Context, registration Registration) (RegistrationAck, error) {
	var ack RegistrationAck
	err := t.roundTrip(ctx, registerPath, "register", registration, "registered", &ack)
	return ack, err
}

func (t *HTTPTransport) Invoke(ctx context.Context, invocation WorkerInvocation) (WorkerResult, error) {
	var result WorkerResult
	err := t.roundTrip(ctx, invokePath, "invoke", invocation, "result", &result)
	return result, err
}

func (t *HTTPTransport) Cancel(ctx context.Context, request CancelRequest) (CancellationAck, error) {
	var ack CancellationAck
	err := t.roundTrip(ctx, cancelPath, "cancel", request, "cancelled", &ack)
	return ack, err
}

func (t *HTTPTransport) OpenStream(ctx context.Context, invocation WorkerInvocation, credit StreamCredit) (StreamSource, error) {
	if credit.Frames == 0 || credit.Bytes == 0 {
		return nil, ErrBackpressure
	}
	response, err := t.exchange(ctx, invokePath, "invoke", invocation, func(request *http.Request) { //nolint:bodyclose // the returned StreamSource owns and closes the response body
		request.Header.Set(StreamCreditFramesHeader, strconv.FormatUint(uint64(credit.Frames), 10))
		request.Header.Set(StreamCreditBytesHeader, strconv.FormatUint(credit.Bytes, 10))
	})
	if err != nil {
		return nil, err
	}
	return &framedStreamSource{
		reader: response.Body, closer: response.Body, maximum: t.maximum, credit: NewCreditWindow(uint64(credit.Frames), credit.Bytes),
		invocationID: invocation.InvocationID, attemptID: invocation.AttemptID, schemaRevision: invocation.SchemaRevision,
	}, nil
}

func (t *HTTPTransport) roundTrip(ctx context.Context, path, requestKind string, requestValue any, responseKind string, responseValue any) error {
	response, err := t.exchange(ctx, path, requestKind, requestValue, nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	frame, err := ReadFrame(response.Body, t.maximum)
	if err != nil || frame.Flags != FrameData {
		return &DeliveryError{Phase: DeliveryAfterWrite}
	}
	var extra [1]byte
	if count, readErr := response.Body.Read(extra[:]); count != 0 || readErr != io.EOF {
		return &DeliveryError{Phase: DeliveryAfterWrite}
	}
	payload, err := decodeEnvelope(frame.Payload, responseKind)
	if err != nil {
		return &DeliveryError{Phase: DeliveryAfterWrite}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(responseValue) != nil || decoder.Decode(new(any)) != io.EOF {
		return &DeliveryError{Phase: DeliveryAfterWrite}
	}
	return nil
}

func (t *HTTPTransport) exchange(ctx context.Context, path, kind string, value any, headers func(*http.Request)) (*http.Response, error) {
	if t == nil {
		return nil, &DeliveryError{Phase: DeliveryBeforeWrite}
	}
	if err := ctx.Err(); err != nil {
		return nil, &DeliveryError{Phase: DeliveryBeforeWrite}
	}
	payload, err := encodeEnvelope(kind, value)
	if err != nil {
		return nil, &DeliveryError{Phase: DeliveryBeforeWrite}
	}
	var framed bytes.Buffer
	if err := WriteFrame(&framed, FrameData, payload, t.maximum); err != nil {
		return nil, &DeliveryError{Phase: DeliveryBeforeWrite}
	}
	started := &atomic.Bool{}
	body := &writeTrackingReader{Reader: bytes.NewReader(framed.Bytes()), started: started}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpointFor(path), body)
	if err != nil {
		return nil, &DeliveryError{Phase: DeliveryBeforeWrite}
	}
	request.Header.Set("Content-Type", WorkerMediaType)
	request.Header.Set("Accept", WorkerMediaType)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set(ProtocolHeader, ProtocolVersion)
	if headers != nil {
		headers(request)
	}
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { started.Store(true) }}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := t.client.Do(request)
	if err != nil {
		phase := DeliveryBeforeWrite
		if started.Load() {
			phase = DeliveryAfterWrite
		}
		return nil, &DeliveryError{Phase: phase}
	}
	if response.Body == nil {
		return nil, &DeliveryError{Phase: DeliveryAfterWrite}
	}
	if err := t.validateResponse(response); err != nil {
		_, _ = io.CopyN(io.Discard, response.Body, int64(t.maximum)+1)
		_ = response.Body.Close()
		return nil, &DeliveryError{Phase: DeliveryAfterWrite}
	}
	return response, nil
}

func (t *HTTPTransport) endpointFor(methodPath string) string {
	endpoint := *t.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + methodPath
	endpoint.RawPath = ""
	return endpoint.String()
}

func (t *HTTPTransport) validateResponse(response *http.Response) error {
	if response.ProtoMajor != 2 {
		return errors.New("remote worker did not negotiate HTTP/2")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("remote worker returned HTTP status class %dxx", response.StatusCode/100)
	}
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != WorkerMediaType || len(parameters) != 0 || response.Header.Get(ProtocolHeader) != ProtocolVersion {
		return errors.New("remote worker response negotiation is invalid")
	}
	return nil
}

type writeTrackingReader struct {
	io.Reader
	started *atomic.Bool
}

func (r *writeTrackingReader) Read(value []byte) (int, error) {
	count, err := r.Reader.Read(value)
	if count != 0 {
		r.started.Store(true)
	}
	return count, err
}
