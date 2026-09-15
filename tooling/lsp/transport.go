package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Serve runs the LSP 3.17 Content-Length framed binding until input closes or
// the context is cancelled. It does not open a listener or start background
// work that outlives the call.
func (server *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	session := &transportSession{
		ctx: ctx, server: server, reader: bufio.NewReader(input), writer: &framedWriter{output: output},
		semaphore: make(chan struct{}, server.limits.MaxConcurrent), requests: make(map[string]context.CancelFunc),
	}
	return session.run()
}

type transportSession struct {
	ctx       context.Context
	server    *Server
	reader    *bufio.Reader
	writer    *framedWriter
	semaphore chan struct{}
	requestMu sync.Mutex
	requests  map[string]context.CancelFunc
	workers   sync.WaitGroup
}

func (session *transportSession) run() error {
	for {
		payload, frameFailure, err := readFrame(session.reader, session.server.limits.MaxMessageBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return session.finish(nil)
			}
			return session.finish(err)
		}
		if frameFailure != nil {
			if err := session.writer.write(message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: frameFailure}); err != nil {
				return err
			}
			continue
		}
		var request message
		if json.Unmarshal(payload, &request) != nil {
			if err := session.writer.write(message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: newResponseError(rpcParseError, CodeParseError, "invalid JSON")}); err != nil {
				return err
			}
			continue
		}
		exit, dispatchErr := session.dispatch(request)
		if dispatchErr != nil || exit {
			return session.finish(dispatchErr)
		}
	}
}

func (session *transportSession) dispatch(request message) (bool, error) {
	if request.Method == "$/cancelRequest" {
		session.cancelRequest(request.Params)
		return false, nil
	}
	if len(request.ID) == 0 {
		return request.Method == "exit", session.handleNotification(request)
	}
	return false, session.startRequest(request)
}

func (session *transportSession) cancelRequest(raw json.RawMessage) {
	var params struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	session.requestMu.Lock()
	cancel := session.requests[string(params.ID)]
	session.requestMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (session *transportSession) handleNotification(request message) error {
	result := session.server.handleSafe(session.ctx, request)
	for _, notification := range result.notifications {
		if err := session.writer.write(notification); err != nil {
			return err
		}
	}
	return nil
}

func (session *transportSession) startRequest(request message) error {
	select {
	case session.semaphore <- struct{}{}:
	case <-session.ctx.Done():
		return session.ctx.Err()
	default:
		return session.writer.write(message{JSONRPC: "2.0", ID: request.ID, Error: newResponseError(rpcInternal, CodeResourceLimit, "concurrent request limit reached")})
	}
	requestContext, cancel := context.WithCancel(session.ctx)
	key := string(request.ID)
	session.requestMu.Lock()
	session.requests[key] = cancel
	session.requestMu.Unlock()
	session.workers.Add(1)
	go session.completeRequest(requestContext, cancel, key, request)
	return nil
}

func (session *transportSession) completeRequest(ctx context.Context, cancel context.CancelFunc, key string, request message) {
	defer session.workers.Done()
	defer func() { <-session.semaphore }()
	defer cancel()
	result := session.server.handleSafe(ctx, request)
	responseResult := result.result
	if result.err == nil && responseResult == nil {
		responseResult = json.RawMessage("null")
	}
	_ = session.writer.write(message{JSONRPC: "2.0", ID: request.ID, Result: responseResult, Error: result.err})
	for _, notification := range result.notifications {
		_ = session.writer.write(notification)
	}
	session.requestMu.Lock()
	delete(session.requests, key)
	session.requestMu.Unlock()
}

func (session *transportSession) finish(result error) error {
	session.requestMu.Lock()
	for _, cancel := range session.requests {
		cancel()
	}
	session.requestMu.Unlock()
	session.workers.Wait()
	return result
}

type framedWriter struct {
	mu     sync.Mutex
	output io.Writer
}

func (writer *framedWriter) write(value message) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return errors.New("encode LSP response")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if _, err := fmt.Fprintf(writer.output, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return errors.New("write LSP header")
	}
	if _, err := writer.output.Write(payload); err != nil {
		return errors.New("write LSP response")
	}
	return nil
}

func readFrame(reader *bufio.Reader, maximumBytes int) ([]byte, *responseError, error) {
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, nil, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		length, parseErr := strconv.Atoi(strings.TrimSpace(value))
		if parseErr != nil || length < 0 {
			return nil, nil, errors.New("invalid LSP Content-Length header")
		}
		contentLength = length
	}
	if contentLength < 0 {
		return nil, nil, errors.New("missing LSP Content-Length header")
	}
	if contentLength > maximumBytes {
		if _, err := io.CopyN(io.Discard, reader, int64(contentLength)); err != nil {
			return nil, nil, err
		}
		return nil, newResponseError(rpcInvalidRequest, CodeResourceLimit, "message exceeds resource limit"), nil
	}
	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, nil, err
	}
	return payload, nil, nil
}
