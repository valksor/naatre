package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
)

func TestExecuteOperationRetriesQueriesAndHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Content-Type": []string{ProblemMediaType}, "Retry-After": []string{"2"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"type":"about:blank","title":"busy","status":503}`)),
			}, nil
		}
		return jsonResponse(http.StatusOK, `{"requestId":"r","data":{"name":"Ada"},"capabilities":[],"extensions":{}}`), nil
	})
	client, err := New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := queryRequest(t)
	operation, err := NewOperation(request, decodeName)
	if err != nil {
		t.Fatalf("NewOperation: %v", err)
	}
	var delays []time.Duration
	result, err := ExecuteOperation(context.Background(), client, operation, RetryPolicy{
		MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Second,
		Wait: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	data, present := result.Data.Value()
	if !present || data.Name != "Ada" || calls.Load() != 2 || !slices.Equal(delays, []time.Duration{time.Second}) {
		t.Fatalf("result/calls/delays = %#v/%d/%v", result, calls.Load(), delays)
	}
}

func TestExecuteOperationNeverReplaysMutationOrPartialData(t *testing.T) {
	t.Parallel()
	var mutationCalls atomic.Int32
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		mutationCalls.Add(1)
		return nil, errors.New("uncertain write")
	})
	client, _ := New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: transport}})
	operation, _ := NewOperation(mutationRequest(t), decodeName)
	_, err := ExecuteOperation(context.Background(), client, operation, RetryPolicy{MaxAttempts: 3, Wait: noWait})
	assertClientCode(t, err, "TRANSPORT_ERROR")
	if mutationCalls.Load() != 1 {
		t.Fatalf("mutation attempts = %d", mutationCalls.Load())
	}

	var partialCalls atomic.Int32
	partialTransport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		partialCalls.Add(1)
		return jsonResponse(http.StatusServiceUnavailable, `{"requestId":"r","data":{"name":"partial"},"errors":[{"code":"TEMPORARY","message":"retryable","path":["name"],"retryable":true}],"capabilities":[],"extensions":{}}`), nil
	})
	client, _ = New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: partialTransport}})
	operation, _ = NewOperation(queryRequest(t), decodeName)
	partial, err := ExecuteOperation(context.Background(), client, operation, RetryPolicy{MaxAttempts: 3, Wait: noWait})
	assertClientCode(t, err, "REMOTE_NAATRE")
	value, present := partial.Data.Value()
	if !present || value.Name != "partial" || len(partial.Errors) != 1 || partialCalls.Load() != 1 {
		t.Fatalf("partial result = %#v, calls %d", partial, partialCalls.Load())
	}
}

func TestBatchAndPaginationPreserveOrderBoundsAndCancellation(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		payload, _ := io.ReadAll(request.Body)
		if bytes.Contains(payload, []byte(`"id":"two"`)) {
			return jsonResponse(http.StatusOK, `{"requestId":"two","data":{"name":"two"},"capabilities":[],"extensions":{}}`), nil
		}
		return jsonResponse(http.StatusOK, `{"requestId":"one","data":{"name":"one"},"capabilities":[],"extensions":{}}`), nil
	})
	client, _ := New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: transport}})
	first, _ := queryRequest(t).WithID("one")
	second, _ := queryRequest(t).WithID("two")
	results, err := ExecuteBatch(context.Background(), client, []Request{first, second}, 2)
	if err != nil || len(results) != 2 || results[0].Result.StatusCode != 200 || results[1].Result.StatusCode != 200 {
		t.Fatalf("ExecuteBatch = %#v, %v", results, err)
	}

	items, err := CollectPages(context.Background(), 3, func(_ context.Context, cursor string) (Page[int], error) {
		if cursor == "" {
			return Page[int]{Items: []int{1, 2}, Info: PageInfo{HasNextPage: true, EndCursor: "next"}}, nil
		}
		return Page[int]{Items: []int{3}}, nil
	})
	if err != nil || !slices.Equal(items, []int{1, 2, 3}) {
		t.Fatalf("CollectPages = %v, %v", items, err)
	}
	partial, err := CollectPages(context.Background(), 2, func(_ context.Context, cursor string) (Page[int], error) {
		return Page[int]{Items: []int{len(cursor) + 1}, Info: PageInfo{HasNextPage: true, EndCursor: "same"}}, nil
	})
	assertClientCode(t, err, "PAGINATION_CURSOR_STALLED")
	if !slices.Equal(partial, []int{1, 5}) {
		t.Fatalf("partial pagination = %v", partial)
	}
}

func TestDecodeCollectionsUsesElementDecodersAndRejectsWrongShapes(t *testing.T) {
	t.Parallel()
	values, err := DecodeList([]byte(`["one","two"]`), DecodeJSON[string])
	if err != nil || !slices.Equal(values, []string{"one", "two"}) {
		t.Fatalf("DecodeList = %v, %v", values, err)
	}
	mapped, err := DecodeMap([]byte(`{"first":"one","second":"two"}`), DecodeJSON[string])
	if err != nil || mapped["first"] != "one" || mapped["second"] != "two" {
		t.Fatalf("DecodeMap = %v, %v", mapped, err)
	}
	if _, err := DecodeList[string]([]byte(`{}`), DecodeJSON[string]); err == nil {
		t.Fatal("DecodeList accepted an object")
	}
	if _, err := DecodeMap[string]([]byte(`[]`), DecodeJSON[string]); err == nil {
		t.Fatal("DecodeMap accepted a list")
	}
}

type namedResult struct {
	Name string `json:"name"`
}

func decodeName(raw json.RawMessage) (namedResult, error) {
	var result namedResult
	err := json.Unmarshal(raw, &result)
	return result, err
}

func noWait(context.Context, time.Duration) error { return nil }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ResponseMediaType}}, Body: io.NopCloser(bytes.NewBufferString(body))}
}

func queryRequest(t *testing.T) Request {
	t.Helper()
	field, _ := Field("name", SelectionOptions{})
	builder, err := NewBuilder().WithOperation(OperationSpec{Name: "Read", Kind: protocol.Query, Select: []Selection{field}})
	if err != nil {
		t.Fatalf("query operation: %v", err)
	}
	request, err := builder.Request("Read")
	if err != nil {
		t.Fatalf("query request: %v", err)
	}
	return request
}

func mutationRequest(t *testing.T) Request {
	t.Helper()
	call, _ := Call("write", SelectionOptions{})
	builder, err := NewBuilder().WithOperation(OperationSpec{Name: "Write", Kind: protocol.Mutation, Select: []Selection{call}})
	if err != nil {
		t.Fatalf("mutation operation: %v", err)
	}
	request, err := builder.Request("Write")
	if err != nil {
		t.Fatalf("mutation request: %v", err)
	}
	return request
}
