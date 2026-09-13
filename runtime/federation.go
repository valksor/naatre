package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

const federationDelegationMaxBytes = 8 << 10

const (
	federationValueMaxDepth = 64
	federationValueMaxNodes = 1 << 16
)

var ErrFederationDelegation = errors.New("invalid federation delegation")

type FederationDelegationIssuerConfig struct {
	PrivateKey ed25519.PrivateKey
	Issuer     string
	Now        func() time.Time
}

// FederationDelegationIssuer signs least-authority, audience-bound downstream
// contexts with gateway-private key material. It intentionally never copies
// arbitrary Principal.Claims.
type FederationDelegationIssuer struct {
	privateKey ed25519.PrivateKey
	issuer     string
	now        func() time.Time
}

func NewFederationDelegationIssuer(config FederationDelegationIssuerConfig) (*FederationDelegationIssuer, error) {
	if len(config.PrivateKey) != ed25519.PrivateKeySize || config.Issuer == "" {
		return nil, errors.New("federation delegation issuer requires a non-empty issuer and Ed25519 private key")
	}
	return &FederationDelegationIssuer{privateKey: bytes.Clone(config.PrivateKey), issuer: config.Issuer, now: federationClock(config.Now)}, nil
}

type FederationDelegationVerifierConfig struct {
	PublicKey ed25519.PublicKey
	Issuer    string
	Audience  string
	Now       func() time.Time
}

// FederationDelegationVerifier carries only public verification authority and
// is permanently pinned to one downstream audience.
type FederationDelegationVerifier struct {
	publicKey ed25519.PublicKey
	issuer    string
	audience  string
	now       func() time.Time
}

func NewFederationDelegationVerifier(config FederationDelegationVerifierConfig) (*FederationDelegationVerifier, error) {
	if len(config.PublicKey) != ed25519.PublicKeySize || config.Issuer == "" || config.Audience == "" {
		return nil, errors.New("federation delegation verifier requires an issuer, audience, and Ed25519 public key")
	}
	return &FederationDelegationVerifier{
		publicKey: bytes.Clone(config.PublicKey), issuer: config.Issuer, audience: config.Audience, now: federationClock(config.Now),
	}, nil
}

func federationClock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

type FederationDelegationOptions struct {
	Audience              string
	RequestID             string
	OperationID           string
	SchemaRevision        string
	ServiceSchemaRevision string
	ServiceSchemaDigest   string
	Deadline              time.Time
	Cost                  uint64
	Concurrency           uint64
}

type FederationDelegationExpectation struct {
	RequestID             string
	OperationID           string
	SchemaRevision        string
	ServiceSchemaRevision string
	ServiceSchemaDigest   string
}

// FederationDelegation is the verified subset of authority available to a
// downstream. Possession of it does not replace current downstream policy.
type FederationDelegation struct {
	Issuer                string
	Audience              string
	Subject               string
	Tenant                string
	AuthorizationRevision string
	RequestID             string
	OperationID           string
	SchemaRevision        string
	ServiceSchemaRevision string
	ServiceSchemaDigest   string
	Deadline              time.Time
	Cost                  uint64
	Concurrency           uint64
}

type federationDelegationPayload struct {
	Version               uint64 `json:"version"`
	Issuer                string `json:"issuer"`
	Audience              string `json:"audience"`
	Subject               string `json:"subject"`
	Tenant                string `json:"tenant,omitempty"`
	AuthorizationRevision string `json:"authorizationRevision"`
	RequestID             string `json:"requestId"`
	OperationID           string `json:"operationId"`
	SchemaRevision        string `json:"schemaRevision"`
	ServiceSchemaRevision string `json:"serviceSchemaRevision"`
	ServiceSchemaDigest   string `json:"serviceSchemaDigest"`
	ExpiresUnixNano       int64  `json:"expiresUnixNano"`
	Cost                  uint64 `json:"cost"`
	Concurrency           uint64 `json:"concurrency"`
}

func (c *FederationDelegationIssuer) Issue(ctx context.Context, options FederationDelegationOptions) (string, error) {
	principal, ok := PrincipalFromContext(ctx)
	if c == nil || !ok || principal.Subject == "" || principal.AuthorizationRevision == "" ||
		options.Audience == "" || options.RequestID == "" || options.OperationID == "" || options.SchemaRevision == "" ||
		options.ServiceSchemaRevision == "" || !validPinnedSchemaDigest(options.ServiceSchemaDigest) ||
		options.Deadline.IsZero() || options.Cost == 0 || options.Concurrency == 0 {
		return "", ErrFederationDelegation
	}
	now, err := callProcessClock(c.now)
	if err != nil || !options.Deadline.After(now) {
		return "", ErrFederationDelegation
	}
	if parentDeadline, hasDeadline := ctx.Deadline(); hasDeadline && parentDeadline.Before(options.Deadline) {
		options.Deadline = parentDeadline
	}
	if !options.Deadline.After(now) {
		return "", ErrFederationDelegation
	}
	payload := federationDelegationPayload{
		Version: 1, Issuer: c.issuer, Audience: options.Audience, Subject: principal.Subject, Tenant: principal.Tenant,
		AuthorizationRevision: principal.AuthorizationRevision, RequestID: options.RequestID, OperationID: options.OperationID,
		SchemaRevision: options.SchemaRevision, ServiceSchemaRevision: options.ServiceSchemaRevision,
		ServiceSchemaDigest: options.ServiceSchemaDigest, ExpiresUnixNano: options.Deadline.UnixNano(), Cost: options.Cost, Concurrency: options.Concurrency,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", ErrFederationDelegation
	}
	signature := ed25519.Sign(c.privateKey, federationDelegationSigningInput(encoded))
	token := base64.RawURLEncoding.EncodeToString(encoded) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if len(token) > federationDelegationMaxBytes {
		return "", ErrFederationDelegation
	}
	return token, nil
}

func (c *FederationDelegationVerifier) Verify(token string, expectation FederationDelegationExpectation) (FederationDelegation, error) {
	if c == nil || len(token) > federationDelegationMaxBytes || expectation.RequestID == "" ||
		expectation.OperationID == "" || expectation.SchemaRevision == "" || expectation.ServiceSchemaRevision == "" ||
		!validPinnedSchemaDigest(expectation.ServiceSchemaDigest) {
		return FederationDelegation{}, ErrFederationDelegation
	}
	payloadPart, signaturePart, ok := strings.Cut(token, ".")
	if !ok || payloadPart == "" || signaturePart == "" || strings.Contains(signaturePart, ".") {
		return FederationDelegation{}, ErrFederationDelegation
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return FederationDelegation{}, ErrFederationDelegation
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil || !ed25519.Verify(c.publicKey, federationDelegationSigningInput(payloadBytes), signature) {
		return FederationDelegation{}, ErrFederationDelegation
	}
	var payload federationDelegationPayload
	decoder := json.NewDecoder(bytes.NewReader(payloadBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || ensureCursorEOF(decoder) != nil {
		return FederationDelegation{}, ErrFederationDelegation
	}
	now, err := callProcessClock(c.now)
	if err != nil || payload.Version != 1 || payload.Issuer != c.issuer || payload.Subject == "" || payload.AuthorizationRevision == "" ||
		payload.Audience != c.audience || payload.RequestID != expectation.RequestID || payload.OperationID != expectation.OperationID ||
		payload.SchemaRevision != expectation.SchemaRevision || payload.ServiceSchemaRevision != expectation.ServiceSchemaRevision ||
		payload.ServiceSchemaDigest != expectation.ServiceSchemaDigest || payload.Cost == 0 || payload.Concurrency == 0 || payload.ExpiresUnixNano <= now.UnixNano() {
		return FederationDelegation{}, ErrFederationDelegation
	}
	return FederationDelegation{
		Issuer: payload.Issuer, Audience: payload.Audience, Subject: payload.Subject, Tenant: payload.Tenant,
		AuthorizationRevision: payload.AuthorizationRevision, RequestID: payload.RequestID, OperationID: payload.OperationID,
		SchemaRevision: payload.SchemaRevision, ServiceSchemaRevision: payload.ServiceSchemaRevision,
		ServiceSchemaDigest: payload.ServiceSchemaDigest, Deadline: time.Unix(0, payload.ExpiresUnixNano), Cost: payload.Cost, Concurrency: payload.Concurrency,
	}, nil
}

func federationDelegationSigningInput(payload []byte) []byte {
	const domain = "naatre:federation-delegation:v1\n"
	return appendCloned([]byte(domain), payload...)
}

type FederationLimits struct {
	MaxCalls       uint64
	MaxCost        uint64
	MaxConcurrency uint64
	MaxAttempts    uint32
}

type FederationCall struct {
	ResponseKey string
	ServiceID   string
	OperationID string
	Input       any
	Path        []any
	MaxAttempts uint32
}

type FederationPlan struct {
	SchemaRevision string
	Calls          []FederationCall
}

type FederationInvocation struct {
	ServiceID         string
	EndpointReference string
	OperationID       string
	SchemaRevision    string
	SchemaDigest      string
	RequestID         string
	Delegation        string
	Input             any
	Cost              uint64
	Attempt           uint32
}

type FederationRemoteError struct {
	Code      string
	Message   string
	Path      []any
	Retryable bool
}

type FederationRemoteResult struct {
	Data           any
	Errors         []FederationRemoteError
	SchemaRevision string
}

type FederationInvoker interface {
	Invoke(context.Context, FederationInvocation) (FederationRemoteResult, error)
}

type FederationInvokerFunc func(context.Context, FederationInvocation) (FederationRemoteResult, error)

func (f FederationInvokerFunc) Invoke(ctx context.Context, invocation FederationInvocation) (FederationRemoteResult, error) {
	return f(ctx, invocation)
}

type ReferenceFederationConfig struct {
	Composition     schema.FederationComposition
	Delegations     *FederationDelegationIssuer
	Invoker         FederationInvoker
	Limits          FederationLimits
	MaximumDuration time.Duration
	AbandonGrace    time.Duration
}

// ReferenceFederationCoordinator executes an already-resolved read-only call
// plan. Production entity planning and transport integration belong to #109.
type ReferenceFederationCoordinator struct {
	composition     schema.FederationComposition
	delegations     *FederationDelegationIssuer
	invoker         FederationInvoker
	limits          FederationLimits
	maximumDuration time.Duration
	abandonGrace    time.Duration
	slots           chan struct{}
}

func NewReferenceFederationCoordinator(config ReferenceFederationConfig) (*ReferenceFederationCoordinator, error) {
	if _, err := config.Composition.CanonicalJSON(); err != nil || config.Delegations == nil || config.Invoker == nil ||
		config.Limits.MaxCalls == 0 || config.Limits.MaxCost == 0 || config.Limits.MaxConcurrency == 0 ||
		config.Limits.MaxAttempts == 0 || config.MaximumDuration <= 0 {
		return nil, errors.New("reference federation coordinator requires composition, delegation, invoker, finite limits, and duration")
	}
	if config.Limits.MaxConcurrency > uint64(maxPortableConcurrency) {
		return nil, errors.New("federation concurrency exceeds the portable ceiling")
	}
	slots := make(chan struct{}, int(config.Limits.MaxConcurrency))
	for range config.Limits.MaxConcurrency {
		slots <- struct{}{}
	}
	return &ReferenceFederationCoordinator{
		composition: config.Composition, delegations: config.Delegations, invoker: config.Invoker,
		limits: config.Limits, maximumDuration: config.MaximumDuration,
		abandonGrace: federationAbandonGrace(config.AbandonGrace), slots: slots,
	}, nil
}

type federationCallResult struct {
	data     any
	failures []ExecutionError
}

type indexedFederationCallResult struct {
	index  int
	result federationCallResult
}

func (c *ReferenceFederationCoordinator) Execute(ctx context.Context, requestID string, plan FederationPlan) Outcome {
	if c == nil {
		return federationFailure(CodeFederationPlanInvalid, "federation coordinator is not initialized", nil, nil)
	}
	principal, hasPrincipal := PrincipalFromContext(ctx)
	if !hasPrincipal || principal.Subject == "" || principal.AuthorizationRevision == "" {
		return federationFailure(CodeUnauthorized, "federated execution requires an authenticated principal", nil, nil)
	}
	if failure := c.validateFederationPlanShape(requestID, plan); failure != nil {
		return Outcome{Data: map[string]any{}, Errors: []ExecutionError{*failure}, Effects: EffectNotApplicable}
	}
	plan, err := cloneFederationPlan(plan)
	if err != nil {
		return federationFailure(CodeFederationPlanInvalid, "federation plan contains unsupported input", nil, err)
	}
	ctx, cancel := context.WithTimeoutCause(ctx, c.maximumDuration, errExecutionResourceDeadline)
	defer cancel()
	if failure := c.validatePlan(requestID, plan); failure != nil {
		return Outcome{Data: map[string]any{}, Errors: []ExecutionError{*failure}, Effects: EffectNotApplicable}
	}
	return federationOutcome(plan.Calls, c.executeCalls(ctx, requestID, plan))
}

func (c *ReferenceFederationCoordinator) executeCalls(ctx context.Context, requestID string, plan FederationPlan) []federationCallResult {
	results := c.dispatchFederationCalls(ctx, requestID, plan)
	return collectFederationCallResults(plan.Calls, results)
}

func (c *ReferenceFederationCoordinator) dispatchFederationCalls(ctx context.Context, requestID string, plan FederationPlan) <-chan indexedFederationCallResult {
	results := make(chan indexedFederationCallResult, len(plan.Calls))
	jobs := make(chan int, len(plan.Calls))
	for index := range plan.Calls {
		jobs <- index
	}
	close(jobs)
	workers := min(len(plan.Calls), int(c.limits.MaxConcurrency))
	for range workers {
		go func() {
			for index := range jobs {
				call := plan.Calls[index]
				results <- indexedFederationCallResult{index: index, result: c.executeCall(ctx, requestID, plan.SchemaRevision, call)}
			}
		}()
	}
	return results
}

func collectFederationCallResults(calls []FederationCall, results <-chan indexedFederationCallResult) []federationCallResult {
	collected := make([]federationCallResult, len(calls))
	for range calls {
		result := <-results
		collected[result.index] = result.result
	}
	return collected
}

func federationOutcome(calls []FederationCall, collected []federationCallResult) Outcome {
	outcome := Outcome{Data: make(map[string]any, len(collected)), Effects: EffectNotApplicable}
	for index, result := range collected {
		if len(result.failures) == 0 {
			outcome.Data[calls[index].ResponseKey] = result.data
		}
		outcome.Errors = append(outcome.Errors, result.failures...)
	}
	return outcome
}

func (c *ReferenceFederationCoordinator) validatePlan(requestID string, plan FederationPlan) *ExecutionError {
	seenKeys := make(map[string]bool, len(plan.Calls))
	var totalCost uint64
	for _, call := range plan.Calls {
		operation, failure := c.validateFederationCall(call, seenKeys)
		if failure != nil {
			return failure
		}
		totalCost, failure = c.addFederationCallCost(totalCost, operation, call)
		if failure != nil {
			return failure
		}
	}
	return nil
}

func (c *ReferenceFederationCoordinator) validateFederationPlanShape(requestID string, plan FederationPlan) *ExecutionError {
	if requestID == "" || plan.SchemaRevision != c.composition.Schema().Revision() {
		failure := makeExecutionError(CodeFederationSchemaMismatch, "federation plan schema revision is unavailable", nil, protocol.Source{}, nil)
		return &failure
	}
	if len(plan.Calls) == 0 || uint64(len(plan.Calls)) > c.limits.MaxCalls {
		return federationPlanFailure("federation plan exceeds the call bound", nil)
	}
	for _, call := range plan.Calls {
		if len(call.Path) == 0 || !validFederationRemotePath(call.Path) {
			return federationPlanFailure("federation plan contains an invalid path", nil)
		}
	}
	return nil
}

func (c *ReferenceFederationCoordinator) validateFederationCall(call FederationCall, seenKeys map[string]bool) (schema.OperationDescriptor, *ExecutionError) {
	owner, owned := c.composition.OperationOwner(call.OperationID)
	operation, declared := c.composition.Operation(call.OperationID)
	if call.ResponseKey == "" || seenKeys[call.ResponseKey] || call.ServiceID == "" || !owned || owner != call.ServiceID ||
		!declared || call.MaxAttempts == 0 || call.MaxAttempts > c.limits.MaxAttempts || (call.MaxAttempts > 1 && !operation.RetrySafe) {
		return schema.OperationDescriptor{}, federationPlanFailure("federation plan contains an invalid or unowned call", call.Path)
	}
	if operation.Kind != protocol.Query || operation.Effect != string(ReadEffect) {
		return schema.OperationDescriptor{}, federationPlanFailure("reference federation plans must contain only read-only queries", call.Path)
	}
	if _, ok := c.composition.Service(call.ServiceID); !ok {
		return schema.OperationDescriptor{}, federationPlanFailure("federation plan references an unavailable service", call.Path)
	}
	seenKeys[call.ResponseKey] = true
	return operation, nil
}

func (c *ReferenceFederationCoordinator) addFederationCallCost(total uint64, operation schema.OperationDescriptor, call FederationCall) (uint64, *ExecutionError) {
	cost, ok := checkedResourceMultiply(federationOperationCost(operation), uint64(call.MaxAttempts))
	if !ok {
		return 0, federationBudgetFailure(call.Path)
	}
	total, ok = checkedResourceAdd(total, cost)
	if !ok || total > c.limits.MaxCost {
		return 0, federationBudgetFailure(call.Path)
	}
	return total, nil
}

func (c *ReferenceFederationCoordinator) executeCall(ctx context.Context, requestID, schemaRevision string, call FederationCall) federationCallResult {
	service, _ := c.composition.Service(call.ServiceID)
	operation, _ := c.composition.Operation(call.OperationID)
	cost := federationOperationCost(operation)
	for attempt := uint32(1); attempt <= call.MaxAttempts; attempt++ {
		result, retry := c.executeFederationCallAttempt(ctx, requestID, schemaRevision, call, service, operation, cost, attempt)
		if !retry {
			return result
		}
	}
	return unavailableFederationCall(call.Path, nil)
}

func (c *ReferenceFederationCoordinator) executeFederationCallAttempt(
	ctx context.Context,
	requestID, schemaRevision string,
	call FederationCall,
	service schema.FederationService,
	operation schema.OperationDescriptor,
	cost uint64,
	attempt uint32,
) (federationCallResult, bool) {
	value, failure, invokeErr := c.invokeFederationCall(ctx, requestID, schemaRevision, call, service, cost, attempt)
	if failure != nil {
		return federationCallResult{failures: []ExecutionError{*failure}}, false
	}
	return mapFederationCallResult(value, invokeErr, service, operation, call, attempt)
}

func (c *ReferenceFederationCoordinator) invokeFederationCall(
	ctx context.Context,
	requestID, schemaRevision string,
	call FederationCall,
	service schema.FederationService,
	cost uint64,
	attempt uint32,
) (any, *ExecutionError, error) {
	if ctx.Err() != nil {
		failure := federationContextFailure(call.Path, ctx)
		return nil, &failure, nil
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		failure := federationContextFailure(call.Path, ctx)
		return nil, &failure, nil
	}
	delegation, err := c.delegations.Issue(ctx, FederationDelegationOptions{
		Audience: service.Audience, RequestID: requestID, OperationID: call.OperationID,
		SchemaRevision: schemaRevision, ServiceSchemaRevision: service.SchemaRevision, ServiceSchemaDigest: service.SchemaDigest,
		Deadline: deadline, Cost: cost, Concurrency: 1,
	})
	if err != nil {
		failure := makeExecutionError(CodeUnauthorized, "federation delegation could not be established", clonePath(call.Path), protocol.Source{}, err)
		return nil, &failure, nil
	}
	input, err := cloneFederationValue(call.Input)
	if err != nil {
		failure := makeExecutionError(CodeFederationPlanInvalid, "federation call input is not portable", clonePath(call.Path), protocol.Source{}, err)
		return nil, &failure, nil
	}
	select {
	case <-c.slots:
	case <-ctx.Done():
		failure := federationContextFailure(call.Path, ctx)
		return nil, &failure, nil
	}
	value, invokeErr := callWithinGrace(ctx, func() { c.slots <- struct{}{} }, c.abandonGrace, func() (any, error) {
		return callFederationInvoker(ctx, c.invoker, FederationInvocation{
			ServiceID: call.ServiceID, EndpointReference: service.EndpointReference, OperationID: call.OperationID,
			SchemaRevision: service.SchemaRevision, SchemaDigest: service.SchemaDigest, RequestID: requestID,
			Delegation: delegation, Input: input, Cost: cost, Attempt: attempt,
		})
	})
	if ctx.Err() != nil {
		failure := federationContextFailure(call.Path, ctx)
		return nil, &failure, nil
	}
	return value, nil, invokeErr
}

func mapFederationCallResult(value any, invokeErr error, service schema.FederationService, operation schema.OperationDescriptor, call FederationCall, attempt uint32) (federationCallResult, bool) {
	if invokeErr != nil {
		return unavailableFederationCall(call.Path, invokeErr), operation.RetrySafe && attempt < call.MaxAttempts
	}
	remote, ok := value.(FederationRemoteResult)
	if !ok {
		failure := makeExecutionError(CodeFederationUnavailable, "downstream service returned an invalid result", clonePath(call.Path), protocol.Source{}, nil)
		return federationCallResult{failures: []ExecutionError{failure}}, false
	}
	if remote.SchemaRevision != service.SchemaRevision {
		failure := makeExecutionError(CodeFederationSchemaMismatch, "downstream schema revision does not match the pinned plan", clonePath(call.Path), protocol.Source{}, nil)
		return federationCallResult{failures: []ExecutionError{failure}}, false
	}
	if len(remote.Errors) == 0 {
		data, err := cloneFederationValue(remote.Data)
		if err != nil {
			failure := makeExecutionError(CodeFederationUnavailable, "downstream service returned an invalid result", clonePath(call.Path), protocol.Source{}, err)
			return federationCallResult{failures: []ExecutionError{failure}}, false
		}
		return federationCallResult{data: data}, false
	}
	retry := operation.RetrySafe && attempt < call.MaxAttempts && federationErrorsRetryable(remote.Errors)
	return federationCallResult{failures: mapFederationErrors(call.Path, remote.Errors)}, retry
}

func unavailableFederationCall(path []any, cause error) federationCallResult {
	failure := makeExecutionError(CodeFederationUnavailable, "downstream service is unavailable", clonePath(path), protocol.Source{}, cause)
	return federationCallResult{failures: []ExecutionError{failure}}
}

func mapFederationErrors(prefix []any, remote []FederationRemoteError) []ExecutionError {
	if len(remote) > 100 {
		failure := makeExecutionError(CodeResourceExhausted, "downstream error budget exhausted", clonePath(prefix), protocol.Source{}, errResourceBudget)
		return []ExecutionError{failure}
	}
	result := make([]ExecutionError, 0, len(remote))
	for _, failure := range remote {
		code := failure.Code
		path := failure.Path
		if !applicationCodePattern.MatchString(code) || slices.Contains(reservedCodes, code) {
			code = CodeFederationUnavailable
		}
		if !validFederationRemotePath(path) {
			code, path = CodeFederationUnavailable, nil
		}
		message := "downstream service reported an error"
		cause := fmt.Errorf("remote %s: %s", failure.Code, failure.Message)
		result = append(result, ExecutionError{
			Code: code, Message: message, Path: append(clonePath(prefix), clonePath(path)...), Retryable: failure.Retryable, internal: cause,
		})
	}
	return result
}

func federationErrorsRetryable(failures []FederationRemoteError) bool {
	return len(failures) != 0 && !slices.ContainsFunc(failures, func(failure FederationRemoteError) bool { return !failure.Retryable })
}

func federationPlanFailure(message string, path []any) *ExecutionError {
	failure := makeExecutionError(CodeFederationPlanInvalid, message, clonePath(path), protocol.Source{}, nil)
	return &failure
}

func federationBudgetFailure(path []any) *ExecutionError {
	failure := makeExecutionError(CodeResourceExhausted, "federation cost budget exhausted", clonePath(path), protocol.Source{}, errResourceBudget)
	return &failure
}

func federationContextFailure(path []any, ctx context.Context) ExecutionError {
	if errors.Is(context.Cause(ctx), errExecutionResourceDeadline) {
		return makeExecutionError(CodeResourceExhausted, "federation execution deadline exhausted", clonePath(path), protocol.Source{}, context.Cause(ctx))
	}
	return makeExecutionError(CodeCancelled, "federated execution cancelled", clonePath(path), protocol.Source{}, context.Cause(ctx))
}

func federationFailure(code, message string, path []any, cause error) Outcome {
	return Outcome{Data: map[string]any{}, Errors: []ExecutionError{makeExecutionError(code, message, path, protocol.Source{}, cause)}, Effects: EffectNotApplicable}
}

func cloneFederationPlan(plan FederationPlan) (FederationPlan, error) {
	cloned := FederationPlan{SchemaRevision: plan.SchemaRevision, Calls: make([]FederationCall, len(plan.Calls))}
	for index, call := range plan.Calls {
		var err error
		cloned.Calls[index], err = cloneFederationCall(call)
		if err != nil {
			return FederationPlan{}, err
		}
	}
	return cloned, nil
}

func cloneFederationCall(call FederationCall) (FederationCall, error) {
	call.Path = clonePath(call.Path)
	var err error
	call.Input, err = cloneFederationValue(call.Input)
	return call, err
}

func clonePath(path []any) []any { return slices.Clone(path) }

func cloneFederationValue(value any) (any, error) {
	state := federationValueCloneState{active: make(map[federationValueReference]bool)}
	return state.clone(value, 0)
}

type federationValueReference struct {
	kind    reflect.Kind
	pointer uintptr
}

type federationValueCloneState struct {
	active map[federationValueReference]bool
	nodes  int
}

func (s *federationValueCloneState) clone(value any, depth int) (any, error) {
	if depth > federationValueMaxDepth || s.nodes >= federationValueMaxNodes {
		return nil, errors.New("federation value exceeds portable structural limits")
	}
	s.nodes++
	switch current := value.(type) {
	case nil, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return current, nil
	case string:
		if !utf8.ValidString(current) {
			return nil, errors.New("federation value contains invalid UTF-8")
		}
		return current, nil
	case json.Number, float32, float64:
		if _, err := json.Marshal(current); err != nil {
			return nil, fmt.Errorf("invalid federation number: %w", err)
		}
		return current, nil
	case map[string]any:
		if current == nil {
			return map[string]any(nil), nil
		}
		reference, err := s.beginReference(current)
		if err != nil {
			return nil, err
		}
		defer delete(s.active, reference)
		cloned := make(map[string]any, len(current))
		for key, child := range current {
			if !utf8.ValidString(key) {
				return nil, errors.New("federation value contains an invalid UTF-8 object key")
			}
			clonedChild, err := s.clone(child, depth+1)
			if err != nil {
				return nil, err
			}
			cloned[key] = clonedChild
		}
		return cloned, nil
	case []any:
		if current == nil {
			return []any(nil), nil
		}
		reference, err := s.beginReference(current)
		if err != nil {
			return nil, err
		}
		defer delete(s.active, reference)
		cloned := make([]any, len(current))
		for index, child := range current {
			clonedChild, err := s.clone(child, depth+1)
			if err != nil {
				return nil, err
			}
			cloned[index] = clonedChild
		}
		return cloned, nil
	default:
		return nil, fmt.Errorf("unsupported federation value type %T", value)
	}
}

func (s *federationValueCloneState) beginReference(value any) (federationValueReference, error) {
	reflected := reflect.ValueOf(value)
	reference := federationValueReference{kind: reflected.Kind(), pointer: reflected.Pointer()}
	if s.active[reference] {
		return federationValueReference{}, errors.New("federation value contains a cycle")
	}
	s.active[reference] = true
	return reference, nil
}

func callFederationInvoker(ctx context.Context, invoker FederationInvoker, invocation FederationInvocation) (result FederationRemoteResult, err error) {
	containPanic(func() {
		result, err = invoker.Invoke(ctx, invocation)
	}, func() {
		result = FederationRemoteResult{}
		err = errors.New("federation invoker panicked")
	})
	return result, err
}

func validFederationRemotePath(path []any) bool {
	if len(path) > 128 {
		return false
	}
	for _, segment := range path {
		switch value := segment.(type) {
		case string:
			if value == "" || len(value) > 256 {
				return false
			}
		case uint64:
		default:
			return false
		}
	}
	return true
}

func federationAbandonGrace(configured time.Duration) time.Duration {
	if configured == 0 {
		return DefaultAbandonGrace
	}
	return configured
}

func federationOperationCost(operation schema.OperationDescriptor) uint64 {
	return max(uint64(1), operation.Cost)
}

func validPinnedSchemaDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, digit := range value[len("sha256:"):] {
		if !strings.ContainsRune("0123456789abcdef", digit) {
			return false
		}
	}
	return true
}
