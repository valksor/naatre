package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/valksor/naatre/protocol"
)

// FrameSource is the transport boundary used by generated streaming helpers.
// Close must be idempotent, safe beside Next, and unblock a pending Next.
type FrameSource interface {
	Next(context.Context) (protocol.StreamFrame, error)
	Close() error
}

// StreamUpdate snapshots one accepted frame and the complete logical receiver
// state after applying it.
type StreamUpdate struct {
	Frame     protocol.StreamFrame
	Duplicate bool
	Snapshot  []byte
	Issues    []protocol.StreamIssue
	Terminal  *protocol.StreamTerminalOutcome
	Recovery  *protocol.StreamTerminalOutcome
}

// Stream owns a frame source and closes it on cancellation, terminal delivery,
// history loss, invalid framing, abandonment, or explicit Close.
type Stream struct {
	source   FrameSource
	receiver *protocol.StreamReceiver
	next     sync.Mutex
	close    func() error
}

func NewStream(source FrameSource, stream string, limits protocol.StreamLimits) (*Stream, error) {
	if source == nil {
		return nil, clientError("INVALID_STREAM", 0, errors.New("frame source is required"))
	}
	receiver, err := protocol.NewStreamReceiver(stream, limits)
	if err != nil {
		return nil, clientError("INVALID_STREAM", 0, err)
	}
	return &Stream{source: source, receiver: receiver, close: sync.OnceValue(source.Close)}, nil
}

func NewResumingStream(source FrameSource, stream string, snapshot []byte, position uint64, limits protocol.StreamLimits) (*Stream, error) {
	if source == nil {
		return nil, clientError("INVALID_STREAM", 0, errors.New("frame source is required"))
	}
	receiver, err := protocol.NewResumingStreamReceiver(stream, bytes.Clone(snapshot), position, limits)
	if err != nil {
		return nil, clientError("INVALID_STREAM", 0, err)
	}
	return &Stream{source: source, receiver: receiver, close: sync.OnceValue(source.Close)}, nil
}

func (s *Stream) Next(ctx context.Context) (StreamUpdate, error) {
	if s == nil || ctx == nil {
		return StreamUpdate{}, clientError("INVALID_STREAM", 0, errors.New("stream and context are required"))
	}
	s.next.Lock()
	defer s.next.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	frame, err := s.source.Next(ctx)
	if err != nil {
		_ = s.Close()
		if ctx.Err() != nil {
			return StreamUpdate{}, classifyTransportError(ctx, err)
		}
		if errors.Is(err, io.EOF) {
			if finishErr := s.receiver.Finish(); finishErr != nil {
				return StreamUpdate{}, clientError("CLIENT_STREAM_TRUNCATED", 0, finishErr)
			}
			return StreamUpdate{}, io.EOF
		}
		return StreamUpdate{}, clientError("CLIENT_STREAM_INVALID", 0, err)
	}
	accepted, err := s.receiver.Accept(frame)
	if err != nil {
		_ = s.Close()
		return StreamUpdate{}, clientError("CLIENT_STREAM_INVALID", 0, err)
	}
	update := StreamUpdate{
		Frame: frame, Duplicate: accepted.Duplicate, Snapshot: s.receiver.Snapshot(), Issues: s.receiver.Errors(),
	}
	if terminal, complete := s.receiver.Terminal(); complete {
		update.Terminal = &terminal
		_ = s.Close()
	}
	if recovery, unavailable := s.receiver.Recovery(); unavailable {
		update.Recovery = &recovery
		_ = s.Close()
	}
	return update, nil
}

func (s *Stream) Close() error {
	if s == nil {
		return nil
	}
	return s.close()
}

// OpenSSE establishes the authenticated Go POST-SSE binding. The optional
// WebSocket subprofile remains unsupported unless a separate adapter is used.
func (c *Client) OpenSSE(ctx context.Context, operation Request, stream string, limits protocol.SSELimits) (*Stream, error) {
	if c == nil || ctx == nil || operation.OperationKind() != protocol.Subscription {
		return nil, clientError("INVALID_STREAM", 0, errors.New("subscription client, context, and operation are required"))
	}
	body, err := operation.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, clientError("INVALID_STREAM", 0, err)
	}
	request.Header.Set("Content-Type", RequestMediaType)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Accept-Encoding", "identity")
	if c.authenticate != nil {
		if err := c.authenticate(ctx, request); err != nil {
			return nil, clientError("AUTHENTICATION_FAILED", 0, nil)
		}
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, classifyTransportError(ctx, err)
	}
	if response.Body == nil {
		return nil, clientError("CLIENT_STREAM_INVALID", response.StatusCode, errors.New("stream body is missing"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, clientError("STREAM_ESTABLISH_FAILED", response.StatusCode, nil)
	}
	if err := validateSSEHeaders(response); err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	source := &sseSource{body: response.Body, decoder: protocol.NewSSEDecoder(limits), close: sync.OnceValue(response.Body.Close)}
	streamResult, err := NewStream(source, stream, limits.Stream)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	return streamResult, nil
}

func validateSSEHeaders(response *http.Response) error {
	values := response.Header.Values("Content-Type")
	if len(values) != 1 {
		return clientError("UNSUPPORTED_MEDIA_TYPE", response.StatusCode, errors.New("stream requires one content type"))
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") || len(parameters) > 1 || parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8") {
		return clientError("UNSUPPORTED_MEDIA_TYPE", response.StatusCode, errors.New("invalid stream media type"))
	}
	encodings := response.Header.Values("Content-Encoding")
	if len(encodings) > 1 {
		return clientError("UNSUPPORTED_CONTENT_ENCODING", response.StatusCode, errors.New("duplicate stream content encoding"))
	}
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return clientError("UNSUPPORTED_CONTENT_ENCODING", response.StatusCode, errors.New("stream compression is unsupported"))
	}
	return nil
}

type sseSource struct {
	body       io.ReadCloser
	decoder    *protocol.SSEDecoder
	frames     []protocol.StreamFrame
	pendingErr error
	close      func() error
}

func (s *sseSource) Next(ctx context.Context) (protocol.StreamFrame, error) {
	if len(s.frames) != 0 {
		frame := s.frames[0]
		s.frames = s.frames[1:]
		return frame, nil
	}
	if s.pendingErr != nil {
		return protocol.StreamFrame{}, s.pendingErr
	}
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	buffer := make([]byte, 32<<10)
	for {
		count, err := s.body.Read(buffer)
		if count > 0 {
			frames, decodeErr := s.decoder.Feed(buffer[:count])
			s.frames = append(s.frames, frames...)
			if decodeErr != nil {
				s.pendingErr = decodeErr
			} else if err != nil {
				s.pendingErr = s.finish(err)
			}
			if len(s.frames) != 0 {
				frame := s.frames[0]
				s.frames = s.frames[1:]
				return frame, nil
			}
			if s.pendingErr != nil {
				return protocol.StreamFrame{}, s.pendingErr
			}
		}
		if err != nil {
			return protocol.StreamFrame{}, s.finish(err)
		}
		select {
		case <-ctx.Done():
			return protocol.StreamFrame{}, ctx.Err()
		default:
		}
	}
}

func (s *sseSource) finish(readErr error) error {
	if !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if err := s.decoder.Finish(); err != nil {
		return err
	}
	return io.EOF
}

func (s *sseSource) Close() error {
	return s.close()
}
