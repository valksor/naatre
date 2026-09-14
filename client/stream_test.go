package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
)

func TestOpenSSEAppliesFramesAndClosesAtTerminal(t *testing.T) {
	t.Parallel()
	streamID := "stream-1"
	frames := []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: streamID, Sequence: 1, SchemaRevision: "r1"},
		{Type: protocol.StreamData, Stream: streamID, Sequence: 2, Data: []byte(`{"name":"Ada"}`), Cursor: "cursor-1", Position: 1, HasPosition: true},
		{Type: protocol.StreamComplete, Stream: streamID, Sequence: 3},
	}
	var encoded bytes.Buffer
	for _, frame := range frames {
		encoded.Write(encodeSSEFrame(t, frame))
	}
	closed := atomic.Bool{}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("stream headers = %#v", request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
			Body: &trackedBody{Reader: bytes.NewReader(encoded.Bytes()), closed: &closed},
		}, nil
	})
	client, _ := New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: transport}})
	stream, err := client.OpenSSE(context.Background(), subscriptionRequest(t), streamID, protocol.DefaultSSELimits())
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}
	for index := range frames {
		update, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next %d: %v", index, err)
		}
		if update.Frame.Type != frames[index].Type {
			t.Fatalf("frame %d = %#v", index, update.Frame)
		}
		if index == 1 && string(update.Snapshot) != `{"name":"Ada"}` {
			t.Fatalf("snapshot = %s", update.Snapshot)
		}
		if index == 2 && update.Terminal == nil {
			t.Fatal("complete frame did not publish terminal")
		}
	}
	if !closed.Load() {
		t.Fatal("terminal stream body was not closed")
	}
}

func TestStreamCancellationClosesPendingSourceAndTruncatedEOFFails(t *testing.T) {
	t.Parallel()
	source := newBlockingSource()
	stream, err := NewStream(source, "stream-1", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, nextErr := stream.Next(ctx)
		finished <- nextErr
	}()
	<-source.started
	cancel()
	select {
	case err := <-finished:
		assertClientCode(t, err, "CANCELED")
	case <-time.After(2 * time.Second):
		t.Fatal("stream cancellation did not unblock Next")
	}
	if source.closeCalls.Load() != 1 {
		t.Fatalf("source closes = %d", source.closeCalls.Load())
	}

	truncated := &sliceFrameSource{frames: []protocol.StreamFrame{{Type: protocol.StreamOpen, Stream: "stream-2", Sequence: 1, SchemaRevision: "r1"}}}
	stream, _ = NewStream(truncated, "stream-2", protocol.DefaultStreamLimits())
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("open Next: %v", err)
	}
	_, err = stream.Next(context.Background())
	assertClientCode(t, err, "CLIENT_STREAM_TRUNCATED")
}

func TestResumingStreamAndSSESourceDirectPaths(t *testing.T) {
	t.Parallel()
	stream, err := NewResumingStream(&sliceFrameSource{}, "stream-resume", []byte(`{"name":"Ada"}`), 2, protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatalf("NewResumingStream: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close resumed stream: %v", err)
	}

	frame := protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "stream-direct", Sequence: 1, SchemaRevision: "r1"}
	source := &sseSource{
		body:    io.NopCloser(bytes.NewReader(encodeSSEFrame(t, frame))),
		decoder: protocol.NewSSEDecoder(protocol.DefaultSSELimits()),
		close:   func() error { return nil },
	}
	actual, err := source.Next(context.Background())
	if err != nil || actual.Type != protocol.StreamOpen {
		t.Fatalf("sseSource.Next = %#v, %v", actual, err)
	}
}

func subscriptionRequest(t *testing.T) Request {
	t.Helper()
	current, _ := Current("event", SelectionOptions{})
	builder, err := NewBuilder().WithOperation(OperationSpec{Name: "Watch", Kind: protocol.Subscription, Select: []Selection{current}})
	if err != nil {
		t.Fatalf("subscription operation: %v", err)
	}
	request, err := builder.Request("Watch")
	if err != nil {
		t.Fatalf("subscription request: %v", err)
	}
	return request
}

func encodeSSEFrame(t *testing.T, frame protocol.StreamFrame) []byte {
	t.Helper()
	payload, err := protocol.MarshalStreamFrame(frame, protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatalf("MarshalStreamFrame: %v", err)
	}
	var encoded bytes.Buffer
	encoded.WriteString("event: naatre.")
	encoded.WriteString(string(frame.Type))
	encoded.WriteByte('\n')
	if frame.Cursor != "" {
		encoded.WriteString("id: ")
		encoded.WriteString(frame.Cursor)
		encoded.WriteByte('\n')
	}
	encoded.WriteString("data: ")
	encoded.Write(payload)
	encoded.WriteString("\n\n")
	return encoded.Bytes()
}

type blockingSource struct {
	started    chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	closeCalls atomic.Int32
}

func newBlockingSource() *blockingSource {
	return &blockingSource{started: make(chan struct{}), closed: make(chan struct{})}
}

func (s *blockingSource) Next(context.Context) (protocol.StreamFrame, error) {
	close(s.started)
	<-s.closed
	return protocol.StreamFrame{}, errors.New("closed")
}

func (s *blockingSource) Close() error {
	s.closeOnce.Do(func() {
		s.closeCalls.Add(1)
		close(s.closed)
	})
	return nil
}

type sliceFrameSource struct {
	frames []protocol.StreamFrame
}

func (s *sliceFrameSource) Next(context.Context) (protocol.StreamFrame, error) {
	if len(s.frames) == 0 {
		return protocol.StreamFrame{}, io.EOF
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}

func (*sliceFrameSource) Close() error { return nil }
