package mcpadapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// ServeStdio serves newline-delimited JSON-RPC without writing any logs or
// diagnostics to protocol stdout. EOF cancels every active request.
func ServeStdio(ctx context.Context, input io.Reader, output io.Writer, server *Server, identity Identity) error {
	if ctx == nil || input == nil || output == nil || server == nil || server.manifest.Transport != Stdio {
		return adapterError("MCP_STDIO_PROFILE_INVALID", errors.New("stdio server configuration is incomplete"))
	}
	connection, err := server.NewConnection(identity)
	if err != nil {
		return err
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer connection.Close()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), server.limits.MaxRequestBytes+1)
	var workers sync.WaitGroup
	var writerMu sync.Mutex
	workerSlots := make(chan struct{}, server.limits.MaxPending)
	for scanner.Scan() {
		frame := bytes.Clone(scanner.Bytes())
		request, decodeErr := decodeRPCRequest(frame, server.limits)
		if decodeErr == nil && request.notification() {
			connection.Handle(serveCtx, frame)
			continue
		}
		select {
		case workerSlots <- struct{}{}:
		default:
			id := nullRPCID()
			if decodeErr == nil {
				id = request.ID
			}
			writerMu.Lock()
			_, _ = output.Write(append(connection.errorResponse(id, rpcLimitExceeded, CodeResourceLimit), '\n'))
			writerMu.Unlock()
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-workerSlots }()
			response, emit := connection.Handle(serveCtx, frame)
			if !emit {
				return
			}
			writerMu.Lock()
			defer writerMu.Unlock()
			_, _ = output.Write(append(response, '\n'))
		}()
	}
	cancel()
	connection.Close()
	workers.Wait()
	if ctx.Err() != nil {
		return adapterError(CodeCancelled, errors.New("stdio server context was cancelled"))
	}
	if scanner.Err() != nil {
		return adapterError(CodeRequestInvalid, errors.New("stdio request frame is invalid or oversized"))
	}
	return adapterError(CodeProcessDied, errors.New("stdio input closed"))
}

type StdioClientTransport struct {
	stream io.ReadWriteCloser
	limits WireLimits

	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	pending map[string]chan stdioResponse
}

type stdioResponse struct {
	value []byte
	err   error
}

func NewStdioClientTransport(stream io.ReadWriteCloser, limits WireLimits) (*StdioClientTransport, error) {
	resolved, err := normalizeWireLimits(limits)
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, adapterError("MCP_STDIO_PROFILE_INVALID", errors.New("stdio stream is required"))
	}
	transport := &StdioClientTransport{stream: stream, limits: resolved, pending: make(map[string]chan stdioResponse)}
	go transport.readLoop()
	return transport, nil
}

func (*StdioClientTransport) Kind() Transport { return Stdio }

func (t *StdioClientTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	if t == nil || ctx == nil {
		return nil, adapterError(CodeRequestInvalid, errors.New("stdio request context and transport are required"))
	}
	request, err := decodeRPCRequest(payload, t.limits)
	if err != nil {
		return nil, err
	}
	if request.notification() {
		if err := t.write(payload); err != nil {
			return nil, err
		}
		return nil, nil
	}
	key := rpcIDKey(request.ID)
	response := make(chan stdioResponse, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, adapterError(CodeProcessDied, errors.New("stdio transport is closed"))
	}
	if t.pending[key] != nil {
		t.mu.Unlock()
		return nil, adapterError(CodeRequestInvalid, errors.New("stdio request identifier is already pending"))
	}
	if len(t.pending) >= t.limits.MaxPending {
		t.mu.Unlock()
		return nil, adapterError(CodeResourceLimit, errors.New("stdio pending request limit reached"))
	}
	t.pending[key] = response
	t.mu.Unlock()
	if err := t.write(payload); err != nil {
		t.remove(key)
		return nil, err
	}
	select {
	case result := <-response:
		return result.value, result.err
	case <-ctx.Done():
		t.remove(key)
		t.writeCancellation(request.ID)
		return nil, adapterError(CodeCancelled, errors.New("stdio request was cancelled"))
	}
}

func (t *StdioClientTransport) Close() error {
	t.failAll(adapterError(CodeProcessDied, errors.New("stdio transport closed")))
	return t.stream.Close()
}

func (t *StdioClientTransport) write(payload []byte) error {
	if len(payload) == 0 || len(payload) > t.limits.MaxRequestBytes {
		return adapterError(CodeRequestInvalid, errors.New("stdio request frame is absent or oversized"))
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return adapterError(CodeProcessDied, errors.New("stdio transport is closed"))
	}
	if _, err := t.stream.Write(append(bytes.Clone(payload), '\n')); err != nil {
		return adapterError(CodeTransportFailed, errors.New("stdio write failed"))
	}
	return nil
}

func (t *StdioClientTransport) writeCancellation(id json.RawMessage) {
	params, _ := json.Marshal(struct {
		RequestID json.RawMessage `json:"requestId"`
	}{RequestID: id})
	payload, _ := marshalRPC(rpcRequest{JSONRPC: JSONRPCVersion, Method: "notifications/cancelled", Params: params}, t.limits.MaxRequestBytes, CodeRequestInvalid)
	_ = t.write(payload)
}

func (t *StdioClientTransport) readLoop() {
	scanner := bufio.NewScanner(t.stream)
	scanner.Buffer(make([]byte, 64<<10), t.limits.MaxResponseBytes+1)
	for scanner.Scan() {
		frame := bytes.Clone(scanner.Bytes())
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(frame, &envelope) != nil || !validRPCID(envelope.ID) {
			t.failAll(adapterError(CodeResponseInvalid, errors.New("stdio response envelope is invalid")))
			continue
		}
		key := rpcIDKey(envelope.ID)
		t.mu.Lock()
		response := t.pending[key]
		delete(t.pending, key)
		t.mu.Unlock()
		if response != nil {
			response <- stdioResponse{value: frame}
		}
	}
	code := CodeProcessDied
	if scanner.Err() != nil {
		code = CodeResponseTooLarge
	}
	t.failAll(adapterError(code, errors.New("stdio response stream closed")))
}

func (t *StdioClientTransport) remove(key string) {
	t.mu.Lock()
	delete(t.pending, key)
	t.mu.Unlock()
}

func (t *StdioClientTransport) failAll(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	pending := make([]chan stdioResponse, 0, len(t.pending))
	for key, response := range t.pending {
		pending = append(pending, response)
		delete(t.pending, key)
	}
	t.mu.Unlock()
	for _, response := range pending {
		response <- stdioResponse{err: err}
	}
}
