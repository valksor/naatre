package openrpcadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync/atomic"

	"github.com/valksor/naatre/interopadapter"
)

type Authorizer func(context.Context, *http.Request) error

type ClientConfig struct {
	Endpoint    string
	HTTPClient  *http.Client
	Authorize   Authorizer
	Description []byte
	Mapping     MappingConfig
	Limits      Limits
}

type Client struct {
	endpoint  *url.URL
	client    *http.Client
	authorize Authorizer
	methods   map[string]MethodBinding
	limits    Limits
	nextID    atomic.Uint64
	report    interopadapter.FidelityReport
}

type BatchCall struct {
	Method       string
	Params       json.RawMessage
	Notification bool
}

type BatchResult struct {
	Notification bool
	Result       json.RawMessage
	Error        *Error
}

func NewClient(config ClientConfig) (*Client, interopadapter.FidelityReport, error) {
	limits := withDefaults(config.Limits)
	config.Mapping.Direction = interopadapter.RuntimeConsume
	_, report, err := MapDescription(config.Description, config.Mapping, limits)
	if err != nil {
		return nil, report, err
	}
	if !validRuntimeLimits(limits) {
		report.Status = "rejected"
		report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "JSONRPC_LIMIT_INVALID", Message: "runtime request and response limits are too small for safe JSON-RPC envelopes"})
		return nil, report, adapterError("JSONRPC_LIMIT_INVALID")
	}
	endpoint, parseErr := url.Parse(config.Endpoint)
	if parseErr != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		report.Status = "rejected"
		report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{Code: "JSONRPC_ENDPOINT_INVALID", Message: "endpoint must be one exact HTTP URL without credentials, query, or fragment"})
		return nil, report, adapterError("JSONRPC_ENDPOINT_INVALID")
	}
	base := http.DefaultClient
	if config.HTTPClient != nil {
		base = config.HTTPClient
	}
	copyClient := *base
	copyClient.Jar = nil
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	methods := make(map[string]MethodBinding, len(config.Mapping.Bindings))
	for _, binding := range config.Mapping.Bindings {
		methods[binding.ExternalName] = binding
	}
	client := &Client{endpoint: endpoint, client: &copyClient, authorize: config.Authorize, methods: methods, limits: limits, report: report}
	client.nextID.Store(0)
	return client, report, nil
}

func (c *Client) FidelityReport() interopadapter.FidelityReport {
	result := c.report
	result.Mappings = slices.Clone(c.report.Mappings)
	result.Operations = slices.Clone(c.report.Operations)
	result.Diagnostics = slices.Clone(c.report.Diagnostics)
	return result
}

// Call executes one explicitly bound JSON-RPC method and returns its exact
// scalar, object, array, or null result JSON.
func (c *Client) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if c == nil {
		return nil, adapterError("JSONRPC_CLIENT_REQUIRED")
	}
	if _, approved := c.methods[method]; !approved {
		return nil, adapterError("JSONRPC_METHOD_UNAPPROVED")
	}
	id := formatNumericID(c.nextID.Add(1))
	request := Request{JSONRPC: JSONRPCVersion, Method: method, Params: slices.Clone(params), ID: id}
	if _, err := decodeRequest(mustJSON(request), c.limits); err != nil {
		return nil, err
	}
	response, err := c.exchange(ctx, request)
	if err != nil {
		return nil, err
	}
	return slices.Clone(response.Result), nil
}

func (c *Client) Notify(ctx context.Context, method string, params json.RawMessage) error {
	if c == nil {
		return adapterError("JSONRPC_CLIENT_REQUIRED")
	}
	binding, approved := c.methods[method]
	if !approved || !binding.AllowNotification {
		return adapterError("JSONRPC_NOTIFICATION_UNAPPROVED")
	}
	request := Request{JSONRPC: JSONRPCVersion, Method: method, Params: slices.Clone(params)}
	if _, err := decodeRequest(mustJSON(request), c.limits); err != nil {
		return err
	}
	_, err := c.exchange(ctx, request)
	return err
}

// CallBatch executes a finite JSON-RPC batch. Results are returned in caller
// order even though wire responses may arrive in any order. Notification
// entries are explicit and have neither a result nor an error.
func (c *Client) CallBatch(ctx context.Context, calls []BatchCall) ([]BatchResult, error) {
	if c == nil {
		return nil, adapterError("JSONRPC_CLIENT_REQUIRED")
	}
	if len(calls) == 0 || len(calls) > c.limits.MaxBatch {
		return nil, adapterError("JSONRPC_BATCH_LIMIT")
	}
	requests := make([]Request, len(calls))
	results := make([]BatchResult, len(calls))
	positions := make(map[string]int, len(calls))
	for index, call := range calls {
		binding, approved := c.methods[call.Method]
		if !approved {
			return nil, adapterError("JSONRPC_METHOD_UNAPPROVED")
		}
		requests[index] = Request{JSONRPC: JSONRPCVersion, Method: call.Method, Params: slices.Clone(call.Params)}
		results[index].Notification = call.Notification
		if call.Notification {
			if !binding.AllowNotification {
				return nil, adapterError("JSONRPC_NOTIFICATION_UNAPPROVED")
			}
		} else {
			requests[index].ID = formatNumericID(c.nextID.Add(1))
			positions[string(requests[index].ID)] = index
		}
		if _, err := decodeRequest(mustJSON(requests[index]), c.limits); err != nil {
			return nil, err
		}
	}
	payload, err := json.Marshal(requests)
	if err != nil || int64(len(payload)) > c.limits.MaxRequestBytes {
		return nil, adapterError("JSONRPC_REQUEST_LIMIT")
	}
	body, err := c.send(ctx, payload)
	if err != nil {
		return nil, err
	}
	if len(positions) == 0 {
		if len(bytes.TrimSpace(body)) != 0 {
			return nil, adapterError("JSONRPC_NOTIFICATION_RESPONSE")
		}
		return results, nil
	}
	if err := protocolValidateJSON(body, c.limits); err != nil {
		return nil, adapterError("JSONRPC_RESPONSE_INVALID")
	}
	var responses []json.RawMessage
	if json.Unmarshal(body, &responses) != nil || len(responses) != len(positions) || len(responses) > c.limits.MaxBatch {
		return nil, adapterError("JSONRPC_BATCH_INVALID")
	}
	seen := make(map[int]bool, len(responses))
	for _, raw := range responses {
		id, idErr := responseID(raw, c.limits)
		if idErr != nil {
			return nil, idErr
		}
		index, exists := positions[string(id)]
		if !exists || seen[index] {
			return nil, adapterError("JSONRPC_ID_MISMATCH")
		}
		seen[index] = true
		response, responseErr := decodeResponse(raw, requests[index].ID, c.limits)
		if responseErr != nil {
			var publicError *Error
			if errors.As(responseErr, &publicError) {
				results[index].Error = publicError
			} else {
				results[index].Error = adapterError("JSONRPC_INTERNAL")
			}
			continue
		}
		results[index].Result = slices.Clone(response.Result)
	}
	return results, nil
}

func (c *Client) exchange(ctx context.Context, rpcRequest Request) (Response, error) {
	payload, err := marshalRequest(rpcRequest, c.limits.MaxRequestBytes)
	if err != nil {
		return Response{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return Response{}, adapterError("JSONRPC_REQUEST_INVALID")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.authorize != nil && c.authorize(ctx, request) != nil {
		return Response{}, adapterError("JSONRPC_AUTHENTICATION_FAILED")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, adapterError("JSONRPC_CANCELLED")
		}
		return Response{}, adapterError("JSONRPC_TRANSPORT_FAILED")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return Response{}, adapterError("JSONRPC_HTTP_STATUS")
	}
	if rpcRequest.Notification() {
		body, readErr := readBounded(response.Body, c.limits.MaxResponseBytes)
		if readErr != nil {
			return Response{}, readErr
		}
		if len(bytes.TrimSpace(body)) != 0 {
			return Response{}, adapterError("JSONRPC_NOTIFICATION_RESPONSE")
		}
		return Response{}, nil
	}
	if mediaType(response.Header.Get("Content-Type")) != "application/json" {
		return Response{}, adapterError("JSONRPC_RESPONSE_MEDIA_TYPE")
	}
	body, err := readBounded(response.Body, c.limits.MaxResponseBytes)
	if err != nil {
		return Response{}, err
	}
	return decodeResponse(body, rpcRequest.ID, c.limits)
}

func (c *Client) send(ctx context.Context, payload []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, adapterError("JSONRPC_REQUEST_INVALID")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.authorize != nil && c.authorize(ctx, request) != nil {
		return nil, adapterError("JSONRPC_AUTHENTICATION_FAILED")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, adapterError("JSONRPC_CANCELLED")
		}
		return nil, adapterError("JSONRPC_TRANSPORT_FAILED")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, adapterError("JSONRPC_HTTP_STATUS")
	}
	if mediaType(response.Header.Get("Content-Type")) != "application/json" && response.StatusCode != http.StatusNoContent {
		return nil, adapterError("JSONRPC_RESPONSE_MEDIA_TYPE")
	}
	return readBounded(response.Body, c.limits.MaxResponseBytes)
}

func responseID(raw json.RawMessage, limits Limits) (json.RawMessage, error) {
	var header struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &header) != nil || !validID(header.ID, limits) {
		return nil, adapterError("JSONRPC_RESPONSE_INVALID")
	}
	return header.ID, nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, adapterError("JSONRPC_TRANSPORT_FAILED")
	}
	if int64(len(payload)) > maximum {
		return nil, adapterError("JSONRPC_RESPONSE_LIMIT")
	}
	return payload, nil
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
