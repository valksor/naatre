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

func (t *FramedTransport) roundTrip(ctx context.Context, requestKind string, request any, responseKind string, response any) error {
	if t == nil {
		return errors.New("framed remote-worker transport is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Protocol string `json:"protocol"`
		Kind     string `json:"kind"`
		Payload  any    `json:"payload"`
	}{Protocol: ProtocolVersion, Kind: requestKind, Payload: request})
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if err := WriteFrame(t.writer, FrameData, payload, t.maximum); err != nil {
		return &DeliveryError{Phase: DeliveryBeforeWrite, Cause: err}
	}
	frame, err := ReadFrame(t.reader, t.maximum)
	if err != nil {
		return &DeliveryError{Phase: DeliveryAfterWrite, Cause: err}
	}
	if frame.Flags != FrameData || protocol.ValidateJSON(frame.Payload, protocol.Limits{MaxBytes: int(t.maximum)}) != nil {
		return errors.New("remote worker returned an invalid frame")
	}
	var envelope struct {
		Protocol string          `json:"protocol"`
		Kind     string          `json:"kind"`
		Payload  json.RawMessage `json:"payload"`
	}
	decoder := json.NewDecoder(bytes.NewReader(frame.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || envelope.Protocol != ProtocolVersion || envelope.Kind != responseKind || len(envelope.Payload) == 0 {
		return errors.New("remote worker returned an invalid envelope")
	}
	decoder = json.NewDecoder(bytes.NewReader(envelope.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(response); err != nil {
		return errors.New("remote worker returned an invalid payload")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("remote worker returned trailing payload data")
	}
	return nil
}
