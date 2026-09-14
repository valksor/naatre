package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valksor/naatre/protocol"
)

// Presence preserves missing, explicit null, pending, and present generated
// result states without collapsing any pair.
type Presence string

const (
	Missing Presence = "missing"
	Null    Presence = "null"
	Pending Presence = "pending"
	Present Presence = "present"
)

// Selected is the field wrapper used by generated selected-result types.
type Selected[T any] struct {
	presence Presence
	value    T
}

func MissingValue[T any]() Selected[T] { return Selected[T]{presence: Missing} }
func NullValue[T any]() Selected[T]    { return Selected[T]{presence: Null} }
func PendingValue[T any]() Selected[T] { return Selected[T]{presence: Pending} }
func PresentValue[T any](value T) Selected[T] {
	return Selected[T]{presence: Present, value: value}
}

func (s Selected[T]) Presence() Presence { return s.presence }

func (s Selected[T]) Value() (T, bool) { return s.value, s.presence == Present }

// DecodeSelected is the common generated-code hook for own-property result
// fields. pending applies only when the property is absent.
func DecodeSelected[T any](raw json.RawMessage, exists, pending bool, decode func(json.RawMessage) (T, error)) (Selected[T], error) {
	if !exists {
		if pending {
			return PendingValue[T](), nil
		}
		return MissingValue[T](), nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return NullValue[T](), nil
	}
	if decode == nil {
		return Selected[T]{}, clientError("RESULT_DECODE_FAILED", 0, errors.New("result decoder is required"))
	}
	value, err := decode(bytes.Clone(raw))
	if err != nil {
		return Selected[T]{}, clientError("RESULT_DECODE_FAILED", 0, err)
	}
	return PresentValue(value), nil
}

// DecodeResultObject validates one generated result object without converting
// exact scalar leaves through an intermediate untyped number.
func DecodeResultObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if err := protocol.ValidateJSON(raw, protocol.Limits{}); err != nil {
		return nil, clientError("RESULT_DECODE_FAILED", 0, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, clientError("RESULT_DECODE_FAILED", 0, errors.New("selected result must be an object"))
	}
	return cloneRawMap(fields), nil
}

// DecodeJSON validates and decodes one generated leaf using the selected Go
// type. Lossless custom scalars should use string or json.RawMessage.
func DecodeJSON[T any](raw json.RawMessage) (T, error) {
	var value T
	if err := protocol.ValidateJSON(raw, protocol.Limits{}); err != nil {
		return value, clientError("RESULT_DECODE_FAILED", 0, err)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, clientError("RESULT_DECODE_FAILED", 0, err)
	}
	return value, nil
}

// DecodeList decodes a selected list without bypassing the generated decoder
// for object, enum, union, or custom-scalar elements.
func DecodeList[T any](raw json.RawMessage, decode func(json.RawMessage) (T, error)) ([]T, error) {
	if decode == nil {
		return nil, clientError("RESULT_DECODE_FAILED", 0, errors.New("list element decoder is required"))
	}
	if err := protocol.ValidateJSON(raw, protocol.Limits{}); err != nil {
		return nil, clientError("RESULT_DECODE_FAILED", 0, err)
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil || elements == nil {
		return nil, clientError("RESULT_DECODE_FAILED", 0, errors.New("selected result must be a list"))
	}
	result := make([]T, len(elements))
	for index, element := range elements {
		value, err := decode(element)
		if err != nil {
			return nil, clientError("RESULT_DECODE_FAILED", 0, err)
		}
		result[index] = value
	}
	return result, nil
}

// DecodeMap decodes a selected string-keyed map through its element decoder.
func DecodeMap[T any](raw json.RawMessage, decode func(json.RawMessage) (T, error)) (map[string]T, error) {
	if decode == nil {
		return nil, clientError("RESULT_DECODE_FAILED", 0, errors.New("map element decoder is required"))
	}
	fields, err := DecodeResultObject(raw)
	if err != nil {
		return nil, err
	}
	result := make(map[string]T, len(fields))
	for name, field := range fields {
		value, err := decode(field)
		if err != nil {
			return nil, clientError("RESULT_DECODE_FAILED", 0, err)
		}
		result[name] = value
	}
	return result, nil
}

// Operation connects one immutable request to its generated selected-result
// decoder and known operation kind.
type Operation[T any] struct {
	request Request
	decode  func(json.RawMessage) (T, error)
}

func NewOperation[T any](request Request, decode func(json.RawMessage) (T, error)) (Operation[T], error) {
	if request.OperationKind() == "" || decode == nil {
		return Operation[T]{}, clientError("INVALID_OPERATION", 0, errors.New("operation kind and decoder are required"))
	}
	return Operation[T]{request: request.clone(), decode: decode}, nil
}

func (o Operation[T]) WithVariable(name string, value json.RawMessage) (Operation[T], error) {
	request, err := o.request.WithVariable(name, value)
	if err != nil {
		return Operation[T]{}, err
	}
	o.request = request
	return o, nil
}

func (o Operation[T]) Request() Request { return o.request.clone() }

// OperationResult preserves typed data, structured errors, HTTP metadata, and
// explicit unary completion simultaneously.
type OperationResult[T any] struct {
	Data       Selected[T]
	Errors     []protocol.ResponseError
	StatusCode int
	Header     http.Header
	Complete   bool
}

// RetryPolicy is finite and injectable. MaxAttempts includes the first call.
// Only query operations are automatically retried by this SDK.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Now         func() time.Time
	Wait        func(context.Context, time.Duration) error
}

// ExecuteOperation executes and decodes a generated operation while retaining
// any valid partial result. Mutations and subscriptions are never replayed.
func ExecuteOperation[T any](ctx context.Context, client *Client, operation Operation[T], policy RetryPolicy) (*OperationResult[T], error) {
	if ctx == nil || client == nil || operation.decode == nil {
		return nil, clientError("INVALID_OPERATION", 0, errors.New("client, context, and decoder are required"))
	}
	resolved, err := resolveRetryPolicy(policy)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		result, executeErr := client.Execute(ctx, operation.request)
		if executeErr == nil {
			return decodeOperationResult(result, operation.decode)
		}
		if !retryableOperation(operation.request.OperationKind(), result, executeErr) {
			if result != nil && result.Envelope != nil {
				typed, decodeErr := decodeOperationResult(result, operation.decode)
				if decodeErr == nil {
					return typed, executeErr
				}
			}
			return nil, executeErr
		}
		if attempt >= resolved.MaxAttempts {
			status := 0
			if result != nil {
				status = result.StatusCode
			}
			return nil, clientError("RETRY_BUDGET_EXHAUSTED", status, executeErr)
		}
		delay := retryDelay(resolved, attempt, result)
		if err := resolved.Wait(ctx, delay); err != nil {
			return nil, classifyTransportError(ctx, err)
		}
	}
}

func decodeOperationResult[T any](result *Result, decode func(json.RawMessage) (T, error)) (*OperationResult[T], error) {
	if result == nil || result.Envelope == nil {
		return nil, clientError("RESULT_DECODE_FAILED", 0, errors.New("naatre response is required"))
	}
	raw, exists := result.Data()
	data, err := DecodeSelected(raw, exists, false, decode)
	if err != nil {
		return nil, err
	}
	return &OperationResult[T]{
		Data: data, Errors: result.Errors(), StatusCode: result.StatusCode, Header: result.Header.Clone(), Complete: true,
	}, nil
}

func resolveRetryPolicy(policy RetryPolicy) (RetryPolicy, error) {
	if policy.MaxAttempts == 0 {
		policy.MaxAttempts = 1
	}
	if policy.MaxAttempts < 1 || policy.MaxAttempts > 8 || policy.BaseDelay < 0 || policy.MaxDelay < 0 {
		return RetryPolicy{}, clientError("INVALID_RETRY_POLICY", 0, errors.New("invalid retry policy"))
	}
	if policy.BaseDelay == 0 {
		policy.BaseDelay = 100 * time.Millisecond
	}
	if policy.MaxDelay == 0 {
		policy.MaxDelay = 2 * time.Second
	}
	if policy.BaseDelay > policy.MaxDelay {
		return RetryPolicy{}, clientError("INVALID_RETRY_POLICY", 0, errors.New("retry delay exceeds maximum"))
	}
	if policy.Now == nil {
		policy.Now = time.Now
	}
	if policy.Wait == nil {
		policy.Wait = waitForRetry
	}
	return policy, nil
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryableOperation(kind protocol.OperationKind, result *Result, err error) bool {
	if kind != protocol.Query {
		return false
	}
	if result != nil {
		if _, present := result.Data(); present {
			return false
		}
		if result.Envelope != nil {
			failures := result.Errors()
			if len(failures) == 0 {
				return false
			}
			for _, failure := range failures {
				if !failure.Retryable() {
					return false
				}
			}
			return true
		}
		if result.Problem != nil {
			return result.StatusCode == http.StatusTooManyRequests || result.StatusCode == http.StatusBadGateway || result.StatusCode == http.StatusServiceUnavailable || result.StatusCode == http.StatusGatewayTimeout
		}
	}
	var clientFailure *Error
	return errors.As(err, &clientFailure) && clientFailure.Code == "TRANSPORT_ERROR"
}

func retryDelay(policy RetryPolicy, attempt int, result *Result) time.Duration {
	delay := policy.BaseDelay
	for exponent := 1; exponent < attempt && delay < policy.MaxDelay; exponent++ {
		if delay > policy.MaxDelay/2 {
			delay = policy.MaxDelay
			break
		}
		delay *= 2
	}
	if delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	if retryAfter := retryAfterDelay(result, policy.Now()); retryAfter > delay {
		delay = retryAfter
	}
	if delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	return delay
}

func retryAfterDelay(result *Result, now time.Time) time.Duration {
	if result == nil {
		return 0
	}
	value := strings.TrimSpace(result.Header.Get("Retry-After"))
	if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if instant, err := http.ParseTime(value); err == nil && instant.After(now) {
		return instant.Sub(now)
	}
	return 0
}

// BatchResult retains one ordered request outcome without making one failure
// erase successful siblings.
type BatchResult struct {
	Result *Result
	Err    error
}

// ExecuteBatch executes replay-independent unary requests with bounded
// concurrency and stable input ordering.
func ExecuteBatch(ctx context.Context, client *Client, requests []Request, maximumConcurrency int) ([]BatchResult, error) {
	if ctx == nil || client == nil || maximumConcurrency < 1 || maximumConcurrency > 128 {
		return nil, clientError("INVALID_BATCH", 0, errors.New("invalid batch configuration"))
	}
	results := make([]BatchResult, len(requests))
	if len(requests) == 0 {
		return results, nil
	}
	workers := min(maximumConcurrency, len(requests))
	jobs := make(chan int)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for index := range jobs {
				results[index].Result, results[index].Err = client.Execute(ctx, requests[index])
			}
		}()
	}
	for index := range requests {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return results, classifyTransportError(ctx, ctx.Err())
		}
	}
	close(jobs)
	group.Wait()
	return results, nil
}

// Page and PageInfo are the generated pagination helper boundary.
type Page[T any] struct {
	Items []T
	Info  PageInfo
}

type PageInfo struct {
	HasNextPage     bool
	HasPreviousPage bool
	StartCursor     string
	EndCursor       string
}

// CollectPages follows forward cursors within one explicit page budget.
func CollectPages[T any](ctx context.Context, maximumPages int, fetch func(context.Context, string) (Page[T], error)) ([]T, error) {
	if ctx == nil || maximumPages < 1 || maximumPages > 10000 || fetch == nil {
		return nil, clientError("INVALID_PAGINATION", 0, errors.New("invalid pagination configuration"))
	}
	var items []T
	cursor := ""
	for pageNumber := 0; pageNumber < maximumPages; pageNumber++ {
		page, err := fetch(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items = append(items, slices.Clone(page.Items)...)
		if !page.Info.HasNextPage {
			return items, nil
		}
		if page.Info.EndCursor == "" || page.Info.EndCursor == cursor {
			return items, clientError("PAGINATION_CURSOR_STALLED", 0, errors.New("pagination cursor did not advance"))
		}
		cursor = page.Info.EndCursor
		select {
		case <-ctx.Done():
			return nil, classifyTransportError(ctx, ctx.Err())
		default:
		}
	}
	return items, clientError("PAGINATION_LIMIT_EXCEEDED", 0, errors.New("pagination page budget exhausted"))
}
