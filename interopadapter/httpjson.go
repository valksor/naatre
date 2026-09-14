package interopadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

var errRedirectBlocked = errors.New("adapter redirect blocked")

type MetadataProvider func(context.Context) http.Header

type HTTPJSONConfig struct {
	Endpoint         string
	AllowedOrigins   []string
	ForwardHeaders   []string
	Metadata         MetadataProvider
	Client           *http.Client
	MaxRequestBytes  int64
	MaxResponseBytes int64
}

type HTTPJSONInvoker struct {
	endpoint         *url.URL
	client           *http.Client
	forwardHeaders   []string
	metadata         MetadataProvider
	maxRequestBytes  int64
	maxResponseBytes int64
}

func NewHTTPJSONInvoker(config HTTPJSONConfig) (*HTTPJSONInvoker, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, adapterError("ADAPTER_ENDPOINT_INVALID", errors.New("endpoint must be an absolute HTTP URL without credentials, query, or fragment"))
	}
	origin := endpoint.Scheme + "://" + endpoint.Host
	allowed := false
	for _, candidate := range config.AllowedOrigins {
		parsed, parseErr := url.Parse(candidate)
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			return nil, adapterError("ADAPTER_EGRESS_POLICY_INVALID", errors.New("allowed origins must be exact origins"))
		}
		if parsed.Scheme+"://"+parsed.Host == origin {
			allowed = true
		}
	}
	if !allowed {
		return nil, adapterError("ADAPTER_EGRESS_DENIED", errors.New("endpoint origin is not allowlisted"))
	}
	if config.MaxRequestBytes < 1 || config.MaxResponseBytes < 1 {
		return nil, adapterError("ADAPTER_LIMIT_INVALID", errors.New("request and response limits must be positive"))
	}
	forward, err := normalizeForwardHeaders(config.ForwardHeaders)
	if err != nil {
		return nil, err
	}
	client := http.DefaultClient
	if config.Client != nil {
		client = config.Client
	}
	copy := *client
	copy.Jar = nil
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirectBlocked }
	return &HTTPJSONInvoker{
		endpoint: endpoint, client: &copy, forwardHeaders: forward, metadata: config.Metadata,
		maxRequestBytes: config.MaxRequestBytes, maxResponseBytes: config.MaxResponseBytes,
	}, nil
}

func (i *HTTPJSONInvoker) Invoke(ctx context.Context, request BackendRequest) (map[string]any, error) {
	if i == nil || ctx == nil {
		return nil, adapterError("ADAPTER_REQUEST_INVALID", errors.New("invoker and context are required"))
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, adapterError("ADAPTER_REQUEST_INVALID", err)
	}
	if int64(len(body)) > i.maxRequestBytes {
		return nil, adapterError("ADAPTER_REQUEST_LIMIT", errors.New("backend request exceeds configured limit"))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, i.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, adapterError("ADAPTER_REQUEST_INVALID", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if deadline, ok := ctx.Deadline(); ok {
		httpRequest.Header.Set("Naatre-Deadline", deadline.UTC().Format(time.RFC3339Nano))
	}
	if i.metadata != nil {
		metadata := i.metadata(ctx)
		for _, name := range i.forwardHeaders {
			for _, value := range metadata.Values(name) {
				httpRequest.Header.Add(name, value)
			}
		}
	}
	response, err := i.client.Do(httpRequest)
	if err != nil {
		switch {
		case errors.Is(err, errRedirectBlocked):
			return nil, adapterError("ADAPTER_REDIRECT_BLOCKED", err)
		case ctx.Err() != nil:
			return nil, adapterError("ADAPTER_CANCELLED", ctx.Err())
		default:
			return nil, adapterError("ADAPTER_TRANSPORT_FAILURE", err)
		}
	}
	if response.Body == nil {
		return nil, adapterError("ADAPTER_RESPONSE_INVALID", errors.New("backend response body is missing"))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, adapterError("ADAPTER_UPSTREAM_FAILURE", fmt.Errorf("unexpected upstream status %d", response.StatusCode))
	}
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") || len(parameters) > 1 || len(parameters) == 1 && !strings.EqualFold(parameters["charset"], "utf-8") {
		return nil, adapterError("ADAPTER_RESPONSE_MEDIA_TYPE", errors.New("backend response is not JSON"))
	}
	payload, err := readBounded(response.Body, i.maxResponseBytes)
	if err != nil {
		return nil, adapterError("ADAPTER_RESPONSE_LIMIT", err)
	}
	var envelope struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Code string `json:"code"`
			Path []any  `json:"path"`
		} `json:"errors"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, adapterError("ADAPTER_RESPONSE_INVALID", errors.New("backend response is malformed"))
	}
	if len(envelope.Errors) != 0 {
		for _, failure := range envelope.Errors {
			if !identityPattern.MatchString(failure.Code) || !safePath(failure.Path) {
				return nil, adapterError("ADAPTER_RESPONSE_INVALID", errors.New("backend error path is unsafe"))
			}
		}
		return nil, adapterError("ADAPTER_PARTIAL_FAILURE", errors.New("backend reported one or more operation failures"))
	}
	if envelope.Data == nil {
		return nil, adapterError("ADAPTER_RESPONSE_INVALID", errors.New("backend response data is required"))
	}
	return envelope.Data, nil
}

func normalizeForwardHeaders(input []string) ([]string, error) {
	forbidden := map[string]bool{
		"connection": true, "content-length": true, "cookie": true, "forwarded": true,
		"host": true, "proxy-authorization": true, "te": true, "trailer": true,
		"transfer-encoding": true, "upgrade": true, "x-forwarded-for": true,
		"x-forwarded-host": true, "x-forwarded-proto": true,
	}
	result := make([]string, 0, len(input))
	seen := make(map[string]bool, len(input))
	for _, value := range input {
		name := http.CanonicalHeaderKey(value)
		lower := strings.ToLower(name)
		if !validHeaderName(value) || forbidden[lower] || seen[lower] {
			return nil, adapterError("ADAPTER_METADATA_POLICY_INVALID", errors.New("forward header is invalid, unsafe, or duplicated"))
		}
		seen[lower] = true
		result = append(result, name)
	}
	slices.Sort(result)
	return result, nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			return false
		}
	}
	return true
}

func safePath(path []any) bool {
	for _, segment := range path {
		switch value := segment.(type) {
		case string:
			if !identityPattern.MatchString(value) {
				return false
			}
		case json.Number:
			index, err := value.Int64()
			if err != nil || index < 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	limited := io.LimitReader(reader, maximum+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maximum {
		return nil, errors.New("backend response exceeds configured limit")
	}
	return payload, nil
}
