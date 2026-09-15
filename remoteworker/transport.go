package remoteworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/valksor/naatre/protocol"
)

type FramedTransport struct {
	reader  io.Reader
	writer  io.Writer
	maximum uint32
	mu      sync.Mutex
}

func NewFramedTransport(reader io.Reader, writer io.Writer, maximum uint32) (*FramedTransport, error) {
	if reader == nil || writer == nil || maximum == 0 {
		return nil, errors.New("framed remote-worker transport requires streams and a finite frame limit")
	}
	return &FramedTransport{reader: reader, writer: writer, maximum: maximum}, nil
}

func (t *FramedTransport) Register(ctx context.Context, registration Registration) (RegistrationAck, error) {
	var ack RegistrationAck
	err := t.roundTrip(ctx, "register", registration, "registered", &ack)
	return ack, err
}

func (t *FramedTransport) Invoke(ctx context.Context, invocation WorkerInvocation) (WorkerResult, error) {
	var result WorkerResult
	err := t.roundTrip(ctx, "invoke", invocation, "result", &result)
	return result, err
}

func (t *FramedTransport) Cancel(ctx context.Context, request CancelRequest) (CancellationAck, error) {
	var ack CancellationAck
	err := t.roundTrip(ctx, "cancel", request, "cancelled", &ack)
	return ack, err
}

func (t *FramedTransport) OpenStream(ctx context.Context, invocation WorkerInvocation, credit StreamCredit) (StreamSource, error) {
	if t == nil || credit.Frames == 0 || credit.Bytes == 0 {
		return nil, ErrBackpressure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload, err := encodeEnvelope("invoke-stream", struct {
		Invocation WorkerInvocation `json:"invocation"`
		Credit     StreamCredit     `json:"credit"`
	}{Invocation: invocation, Credit: credit})
	if err != nil {
		return nil, &DeliveryError{Phase: DeliveryBeforeWrite}
	}
	t.mu.Lock()
	if err := WriteFrame(t.writer, FrameData, payload, t.maximum); err != nil {
		t.mu.Unlock()
		return nil, &DeliveryError{Phase: DeliveryBeforeWrite}
	}
	return &framedStreamSource{
		reader: t.reader, closer: &framedStreamCloser{reader: t.reader, unlock: t.mu.Unlock}, maximum: t.maximum,
		credit: NewCreditWindow(uint64(credit.Frames), credit.Bytes), invocationID: invocation.InvocationID,
		attemptID: invocation.AttemptID, schemaRevision: invocation.SchemaRevision,
	}, nil
}

type framedStreamCloser struct {
	reader io.Reader
	unlock func()
	once   sync.Once
	err    error
}

func (c *framedStreamCloser) Close() error {
	c.once.Do(func() {
		if closer, ok := c.reader.(io.Closer); ok {
			c.err = closer.Close()
		}
		c.unlock()
	})
	return c.err
}

func (t *FramedTransport) roundTrip(ctx context.Context, requestKind string, request any, responseKind string, response any) error {
	if t == nil {
		return errors.New("framed remote-worker transport is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := encodeEnvelope(requestKind, request)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if err := WriteFrame(t.writer, FrameData, payload, t.maximum); err != nil {
		return &DeliveryError{Phase: DeliveryBeforeWrite}
	}
	frame, err := ReadFrame(t.reader, t.maximum)
	if err != nil {
		return &DeliveryError{Phase: DeliveryAfterWrite}
	}
	if frame.Flags != FrameData || protocol.ValidateJSON(frame.Payload, protocol.Limits{MaxBytes: int(t.maximum)}) != nil {
		return errors.New("remote worker returned an invalid frame")
	}
	envelope, err := decodeEnvelope(frame.Payload, responseKind)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(response); err != nil {
		return errors.New("remote worker returned an invalid payload")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("remote worker returned trailing payload data")
	}
	return nil
}

func encodeEnvelope(kind string, payload any) ([]byte, error) {
	return json.Marshal(struct {
		Protocol string `json:"protocol"`
		Kind     string `json:"kind"`
		Payload  any    `json:"payload"`
	}{Protocol: ProtocolVersion, Kind: kind, Payload: payload})
}

func decodeEnvelope(input []byte, expectedKind string) (json.RawMessage, error) {
	var envelope struct {
		Protocol string          `json:"protocol"`
		Kind     string          `json:"kind"`
		Payload  json.RawMessage `json:"payload"`
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(new(any)) != io.EOF || envelope.Protocol != ProtocolVersion || envelope.Kind != expectedKind || len(envelope.Payload) == 0 {
		return nil, errors.New("remote worker returned an invalid envelope")
	}
	return envelope.Payload, nil
}
