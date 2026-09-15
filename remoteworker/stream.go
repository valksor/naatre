package remoteworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/valksor/naatre/protocol"
)

// StreamCredit is the finite initial application credit granted to one source
// stream. Dynamic credit replenishment is outside implementation.go.remote-worker-1.
type StreamCredit struct {
	Frames uint32 `json:"frames"`
	Bytes  uint64 `json:"bytes"`
}

// WorkerStreamFrame binds a core.streaming-1 frame to one remote invocation.
// The logical frame remains owned and validated by package protocol.
type WorkerStreamFrame struct {
	Protocol       string          `json:"protocol"`
	InvocationID   string          `json:"invocationId"`
	AttemptID      string          `json:"attemptId"`
	SchemaRevision string          `json:"schemaRevision"`
	Frame          json.RawMessage `json:"frame"`
}

// StreamSource is an owned, closeable remote source. Close must be idempotent.
type StreamSource interface {
	Next(context.Context) (protocol.StreamFrame, error)
	Close() error
}

// StreamingTransport is the optional server-streaming extension to Transport.
type StreamingTransport interface {
	OpenStream(context.Context, WorkerInvocation, StreamCredit) (StreamSource, error)
}

type framedStreamSource struct {
	reader         io.Reader
	closer         io.Closer
	maximum        uint32
	credit         *CreditWindow
	invocationID   string
	attemptID      string
	schemaRevision string
	closed         atomic.Bool
	closeOnce      sync.Once
	closeErr       error
	next           sync.Mutex
}

func (s *framedStreamSource) Next(ctx context.Context) (protocol.StreamFrame, error) {
	if s == nil || s.closed.Load() {
		return protocol.StreamFrame{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return protocol.StreamFrame{}, err
	}
	s.next.Lock()
	defer s.next.Unlock()
	if s.closed.Load() {
		return protocol.StreamFrame{}, io.EOF
	}
	frame, err := s.readNextFrame()
	if err != nil {
		_ = s.Close()
	}
	return frame, err
}

func (s *framedStreamSource) readNextFrame() (protocol.StreamFrame, error) {
	framed, err := ReadFrame(s.reader, s.maximum)
	if errors.Is(err, io.EOF) {
		return protocol.StreamFrame{}, gatewayError(CodeMalformedWorkerData, "remote stream ended without a terminal frame", nil)
	}
	if err != nil {
		return protocol.StreamFrame{}, gatewayError(CodeMalformedWorkerData, "remote stream read failed", nil)
	}
	if framed.Flags != FrameData {
		return protocol.StreamFrame{}, gatewayError(CodeMalformedWorkerData, "remote stream frame is invalid", nil)
	}
	if err := s.credit.Consume(uint64(len(framed.Payload))); err != nil {
		return protocol.StreamFrame{}, ErrBackpressure
	}
	payload, err := decodeEnvelope(framed.Payload, "stream")
	if err != nil {
		return protocol.StreamFrame{}, gatewayError(CodeMalformedWorkerData, "remote stream envelope is invalid", nil)
	}
	return s.decodeWorkerFrame(payload)
}

func (s *framedStreamSource) decodeWorkerFrame(payload json.RawMessage) (protocol.StreamFrame, error) {
	var value WorkerStreamFrame
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Protocol != ProtocolVersion ||
		value.InvocationID != s.invocationID || value.AttemptID != s.attemptID || value.SchemaRevision != s.schemaRevision {
		return protocol.StreamFrame{}, gatewayError(CodeMalformedWorkerData, "remote stream identity is invalid", nil)
	}
	frame, err := protocol.DecodeStreamFrame(value.Frame, protocol.DefaultStreamLimits())
	if err != nil {
		return protocol.StreamFrame{}, gatewayError(CodeMalformedWorkerData, "remote stream payload is invalid", nil)
	}
	return frame, nil
}

func (s *framedStreamSource) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.closer != nil {
			s.closeErr = s.closer.Close()
		}
	})
	return s.closeErr
}

// GatewayStream validates core.streaming-1 ordering, schema identity, public
// output, and worker error safety before releasing frames to its caller.
type GatewayStream struct {
	source         StreamSource
	receiver       *protocol.StreamReceiver
	gateway        *ReferenceGateway
	invocationID   string
	schemaRevision string
	outputSchema   string
	next           sync.Mutex
	closeOnce      sync.Once
	closeErr       error
	closed         atomic.Bool
}

func (s *GatewayStream) Next(ctx context.Context) (protocol.StreamFrame, error) {
	if s == nil || s.closed.Load() {
		return protocol.StreamFrame{}, io.EOF
	}
	s.next.Lock()
	defer s.next.Unlock()
	if s.closed.Load() {
		return protocol.StreamFrame{}, io.EOF
	}
	frame, err := s.source.Next(ctx)
	if err != nil {
		_ = s.Close()
		return protocol.StreamFrame{}, s.publicSourceError(err)
	}
	if err := s.validateFrame(frame); err != nil {
		_ = s.Close()
		return protocol.StreamFrame{}, err
	}
	if _, terminal := s.receiver.Terminal(); terminal {
		_ = s.Close()
	}
	return frame, nil
}

func (s *GatewayStream) publicSourceError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return gatewayError(CodeCancelled, "remote stream was cancelled", nil)
	}
	if errors.Is(err, ErrBackpressure) {
		return gatewayError(CodeOverloaded, "remote stream exceeded its admitted credit", nil)
	}
	var gatewayErr *GatewayError
	if errors.As(err, &gatewayErr) {
		return err
	}
	if errors.Is(err, io.EOF) && s.receiver.Finish() != nil {
		return gatewayError(CodeMalformedWorkerData, "remote stream ended without a terminal frame", nil)
	}
	return gatewayError(CodeMalformedWorkerData, "remote stream failed", nil)
}

func (s *GatewayStream) validateFrame(frame protocol.StreamFrame) error {
	if frame.Stream != s.invocationID || (frame.Type == protocol.StreamOpen && frame.SchemaRevision != s.schemaRevision) {
		return gatewayError(CodeSchemaMismatch, "remote stream identity is invalid", nil)
	}
	if frame.Type == protocol.StreamData && len(frame.Path) == 0 {
		if err := s.gateway.config.ValidateOutput(s.outputSchema, frame.Data); err != nil {
			return gatewayError(CodeOutputInvalid, "remote stream output does not satisfy its schema", nil)
		}
	}
	if frame.Type == protocol.StreamError && (frame.Error == nil || !errorCodePattern.MatchString(frame.Error.Code) || slices.Contains(reservedWorkerCodes, frame.Error.Code)) {
		return gatewayError(CodeMalformedWorkerData, "remote stream error is invalid", nil)
	}
	if _, err := s.receiver.Accept(frame); err != nil {
		return gatewayError(CodeMalformedWorkerData, "remote stream sequence is invalid", nil)
	}
	return nil
}

func (s *GatewayStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if err := s.source.Close(); err != nil {
			s.closeErr = gatewayError(CodeMalformedWorkerData, "remote stream cleanup failed", nil)
		}
		s.gateway.release(s.invocationID)
	})
	return s.closeErr
}
