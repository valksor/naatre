package remoteworker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
)

var (
	identifierPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:-]{0,127}$`)
	errorCodePattern    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)
	reservedWorkerCodes = []string{"CANCELLED", "INTERNAL", "OVERLOADED", "OUTPUT_COMPLETION", "UNAUTHORIZED", CodeCapabilityMismatch, CodeDuplicateInvocation, CodeInvalidInvocation, CodeMalformedWorkerData, CodeOutcomeIndeterminate, CodeSchemaMismatch, CodeStaleReference, CodeUnauthenticated, CodeUnknownHandler}
)

type GatewayConfig struct {
	Transport        Transport
	Endpoint         string
	WorkerID         string
	ServiceIdentity  string
	Audience         string
	SchemaRevision   string
	SchemaDigest     string
	MaxInFlight      uint32
	MaxAttempts      int
	MaxRequestBytes  uint32
	MaxResponseBytes uint32
	Now              func() time.Time
	VerifyDelegation func(context.Context, string, DelegationExpectation) error
	Authorize        func(context.Context, AuthorizationRequest) error
	ValidateInput    func(string, json.RawMessage) error
	ValidateOutput   func(string, json.RawMessage) error
}

type storedReference struct {
	grant           ReferenceGrant
	requestID       string
	sessionID       string
	ownerInvocation string
}

type ReferenceGateway struct {
	config           GatewayConfig
	mu               sync.Mutex
	registration     *Registration
	sessionID        string
	handlers         map[string]Handler
	seen             map[string]bool
	active           map[string]string
	references       map[string]storedReference
	inFlight         uint32
	maxInFlight      uint32
	maxRequestBytes  uint32
	maxResponseBytes uint32
	registering      bool
}

func NewReferenceGateway(config GatewayConfig) (*ReferenceGateway, error) {
	if config.Transport == nil || !validIdentifier(config.Endpoint) || !validIdentifier(config.WorkerID) ||
		config.ServiceIdentity == "" || !validIdentifier(config.Audience) || !validIdentifier(config.SchemaRevision) ||
		!validDigest(config.SchemaDigest) || config.MaxInFlight == 0 || config.MaxAttempts < 1 || config.MaxAttempts > 1024 || config.MaxRequestBytes == 0 ||
		config.MaxResponseBytes == 0 || config.VerifyDelegation == nil || config.Authorize == nil ||
		config.ValidateInput == nil || config.ValidateOutput == nil {
		return nil, errors.New("reference remote-worker gateway requires pinned trust, schema, finite limits, and validators")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ReferenceGateway{config: config, handlers: make(map[string]Handler), seen: make(map[string]bool), active: make(map[string]string), references: make(map[string]storedReference)}, nil
}

func (g *ReferenceGateway) Register(ctx context.Context, registration Registration) error {
	if g == nil {
		return gatewayError(CodeInvalidRegistration, "gateway is unavailable", nil)
	}
	g.mu.Lock()
	if g.registering || g.inFlight != 0 {
		g.mu.Unlock()
		return gatewayError(CodeInvalidRegistration, "worker session cannot change while work is active", nil)
	}
	g.registering = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.registering = false
		g.mu.Unlock()
	}()
	if err := g.validateRegistration(registration); err != nil {
		return err
	}
	ack, err := g.config.Transport.Register(ctx, registration)
	if err != nil {
		return gatewayError(CodeInvalidRegistration, "worker registration failed", err)
	}
	if ack.Protocol != ProtocolVersion || ack.WorkerID != registration.WorkerID || !validIdentifier(ack.SessionID) ||
		ack.SchemaRevision != registration.SchemaRevision || !uniqueKnownCapabilities(ack.AcceptedCapabilities) ||
		!allContained(ack.AcceptedCapabilities, registration.Capabilities) || !slices.Contains(ack.AcceptedCapabilities, CapabilityUnary) {
		return gatewayError(CodeInvalidRegistration, "worker registration acknowledgement is invalid", nil)
	}
	for _, handler := range registration.Handlers {
		if !allContained(handler.RequiredCapabilities, ack.AcceptedCapabilities) ||
			(handler.Effect == EffectTransaction && !slices.Contains(ack.AcceptedCapabilities, CapabilityTransactions)) {
			return gatewayError(CodeCapabilityMismatch, "negotiated capabilities do not admit every handler", nil)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	copy := registration
	copy.Capabilities = slices.Clone(ack.AcceptedCapabilities)
	g.registration = &copy
	g.sessionID = ack.SessionID
	g.maxInFlight = min(g.config.MaxInFlight, registration.Limits.MaxInFlight)
	g.maxRequestBytes = min(g.config.MaxRequestBytes, registration.Limits.MaxRequestBytes)
	g.maxResponseBytes = min(g.config.MaxResponseBytes, registration.Limits.MaxResponseBytes)
	g.handlers = make(map[string]Handler, len(registration.Handlers))
	g.seen = make(map[string]bool)
	g.references = make(map[string]storedReference)
	for _, handler := range registration.Handlers {
		g.handlers[handler.ID] = handler
	}
	return nil
}

func (g *ReferenceGateway) validateRegistration(registration Registration) error {
	if registration.Protocol != ProtocolVersion || registration.WorkerID != g.config.WorkerID || registration.Endpoint != g.config.Endpoint ||
		registration.Audience != g.config.Audience || registration.Limits.MaxInFlight == 0 || registration.Limits.MaxRequestBytes == 0 ||
		registration.Limits.MaxResponseBytes == 0 || registration.Limits.MaxStreamFrames == 0 || registration.Limits.MaxStreamBytes == 0 ||
		len(registration.Handlers) == 0 || !uniqueKnownCapabilities(registration.Capabilities) || !slices.Contains(registration.Capabilities, CapabilityUnary) {
		return gatewayError(CodeInvalidRegistration, "worker registration is invalid", nil)
	}
	if registration.ServiceIdentity != g.config.ServiceIdentity {
		return gatewayError(CodeUnauthenticated, "worker service identity is not trusted", nil)
	}
	if registration.SchemaRevision != g.config.SchemaRevision || registration.SchemaDigest != g.config.SchemaDigest {
		return gatewayError(CodeSchemaMismatch, "worker schema revision is not admitted", nil)
	}
	seen := make(map[string]bool, len(registration.Handlers))
	for _, handler := range registration.Handlers {
		if !validHandler(handler) || seen[handler.ID] || !allContained(handler.RequiredCapabilities, registration.Capabilities) {
			return gatewayError(CodeCapabilityMismatch, "worker handler schema or capabilities are invalid", nil)
		}
		if handler.Effect == EffectTransaction && !slices.Contains(registration.Capabilities, CapabilityTransactions) {
			return gatewayError(CodeCapabilityMismatch, "transaction handler lacks provider capability", nil)
		}
		seen[handler.ID] = true
	}
	return nil
}

func (g *ReferenceGateway) Invoke(ctx context.Context, request InvokeRequest) (PublicResult, error) {
	handler, err := g.admit(ctx, request)
	if err != nil {
		return PublicResult{}, err
	}
	defer g.release(request.InvocationID)

	invocation := WorkerInvocation{
		Protocol: ProtocolVersion, RequestID: request.RequestID, InvocationID: request.InvocationID,
		HandlerID: request.HandlerID, SchemaRevision: g.config.SchemaRevision, DeadlineUnixMilli: request.Deadline.UnixMilli(),
		IdempotencyKey: request.IdempotencyKey, DelegatedContext: request.DelegatedContext,
		Parent: request.Parent, Input: slices.Clone(request.Input), ResumeCursor: request.ResumeCursor,
	}
	for attempt := 1; attempt <= g.config.MaxAttempts; attempt++ {
		invocation.AttemptID = fmt.Sprintf("%s.%d", request.InvocationID, attempt)
		result, invokeErr := g.config.Transport.Invoke(ctx, invocation)
		if invokeErr == nil {
			return g.complete(request, invocation, result)
		}
		if !g.shouldRetry(handler, request, invokeErr, attempt) {
			var delivery *DeliveryError
			if errors.As(invokeErr, &delivery) && delivery.Phase == DeliveryAfterWrite && handler.Effect != EffectQuery {
				return PublicResult{}, gatewayError(CodeOutcomeIndeterminate, "remote effect outcome is indeterminate", invokeErr)
			}
			return PublicResult{}, gatewayError(CodeMalformedWorkerData, "remote invocation failed", invokeErr)
		}
	}
	return PublicResult{}, gatewayError(CodeMalformedWorkerData, "remote retry budget exhausted", nil)
}

func (g *ReferenceGateway) admit(ctx context.Context, request InvokeRequest) (Handler, error) {
	if g == nil {
		return Handler{}, gatewayError(CodeInvalidInvocation, "gateway is unavailable", nil)
	}
	if err := g.validateInvocation(request); err != nil {
		return Handler{}, err
	}
	handler, err := g.reserveInvocation(request)
	if err != nil {
		return Handler{}, err
	}

	if err := g.config.VerifyDelegation(ctx, request.DelegatedContext, DelegationExpectation{Audience: g.config.Audience, RequestID: request.RequestID, HandlerID: request.HandlerID, Deadline: request.Deadline}); err != nil {
		g.release(request.InvocationID)
		return Handler{}, gatewayError(CodeUnauthorized, "delegated context is invalid", err)
	}
	if err := g.config.Authorize(ctx, AuthorizationRequest{RequestID: request.RequestID, InvocationID: request.InvocationID, HandlerID: request.HandlerID, Effect: handler.Effect}); err != nil {
		g.release(request.InvocationID)
		return Handler{}, gatewayError(CodeUnauthorized, "remote handler is not authorized", err)
	}
	if err := g.config.ValidateInput(handler.InputSchema, request.Input); err != nil {
		g.release(request.InvocationID)
		return Handler{}, gatewayError(CodeInvalidInvocation, "remote input does not satisfy its schema", err)
	}
	return handler, nil
}

func (g *ReferenceGateway) validateInvocation(request InvokeRequest) error {
	if !validIdentifier(request.RequestID) || !validIdentifier(request.InvocationID) || len(request.InvocationID) > 96 || !validIdentifier(request.HandlerID) ||
		request.Deadline.IsZero() || !request.Deadline.After(g.config.Now()) || len(request.Input) == 0 ||
		len(request.DelegatedContext) == 0 || len(request.DelegatedContext) > 8192 || len(request.IdempotencyKey) > 1024 || len(request.ResumeCursor) > 4096 ||
		protocol.ValidateJSON(request.Input, protocol.Limits{MaxBytes: int(g.config.MaxRequestBytes)}) != nil ||
		(len(request.Parent.Value) != 0 && request.Parent.Reference != "") ||
		(request.Parent.Reference == "" && request.Parent.OwnerInvocationID != "") ||
		(request.Parent.Reference != "" && request.Parent.OwnerInvocationID != "" && !validIdentifier(request.Parent.OwnerInvocationID)) ||
		(len(request.Parent.Value) != 0 && protocol.ValidateJSON(request.Parent.Value, protocol.Limits{MaxBytes: int(g.config.MaxRequestBytes)}) != nil) {
		return gatewayError(CodeInvalidInvocation, "remote invocation is invalid", nil)
	}
	return nil
}

func (g *ReferenceGateway) reserveInvocation(request InvokeRequest) (Handler, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.registration == nil || g.registering {
		return Handler{}, gatewayError(CodeInvalidRegistration, "worker is not registered", nil)
	}
	handler, ok := g.handlers[request.HandlerID]
	if !ok {
		return Handler{}, gatewayError(CodeUnknownHandler, "remote handler is not registered", nil)
	}
	if g.inFlight >= g.maxInFlight {
		return Handler{}, gatewayError(CodeOverloaded, "remote worker capacity is full", nil)
	}
	if uint32(len(request.Input)) > g.maxRequestBytes || uint32(len(request.Parent.Value)) > g.maxRequestBytes {
		return Handler{}, gatewayError(CodeInvalidInvocation, "remote input exceeds the worker limit", nil)
	}
	if g.seen[request.InvocationID] {
		return Handler{}, gatewayError(CodeDuplicateInvocation, "remote invocation identifier was already admitted", nil)
	}
	if request.Parent.Reference != "" {
		stored, found := g.references[request.Parent.Reference]
		wrongScope := found && (stored.requestID != request.RequestID ||
			(stored.grant.Lifetime == ReferenceInvocation && stored.ownerInvocation != request.Parent.OwnerInvocationID))
		if !found || !stored.grant.ExpiresAt.After(g.config.Now()) || stored.sessionID != g.sessionID ||
			wrongScope {
			return Handler{}, gatewayError(CodeStaleReference, "remote parent reference is unavailable", nil)
		}
	}
	g.inFlight++
	g.seen[request.InvocationID] = true
	g.active[request.InvocationID] = request.RequestID
	return handler, nil
}

func (g *ReferenceGateway) shouldRetry(handler Handler, request InvokeRequest, err error, attempt int) bool {
	if attempt >= g.config.MaxAttempts {
		return false
	}
	var delivery *DeliveryError
	if !errors.As(err, &delivery) {
		return false
	}
	if delivery.Phase == DeliveryBeforeWrite {
		return true
	}
	if delivery.Phase != DeliveryAfterWrite {
		return false
	}
	switch handler.Effect {
	case EffectQuery:
		return true
	case EffectMutation:
		return request.IdempotencyKey != "" && slices.Contains(handler.RequiredCapabilities, CapabilityIdempotencyReplay)
	case EffectSubscription:
		return request.ResumeCursor != "" && slices.Contains(handler.RequiredCapabilities, CapabilitySubscriptionResume)
	case EffectTransaction:
		return false
	default:
		return false
	}
}

func (g *ReferenceGateway) complete(request InvokeRequest, invocation WorkerInvocation, result WorkerResult) (PublicResult, error) {
	if result.Protocol != ProtocolVersion || result.InvocationID != invocation.InvocationID || result.AttemptID != invocation.AttemptID ||
		result.SchemaRevision != g.config.SchemaRevision {
		code := CodeMalformedWorkerData
		if result.SchemaRevision != g.config.SchemaRevision {
			code = CodeSchemaMismatch
		}
		return PublicResult{}, gatewayError(code, "worker result identity is invalid", nil)
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil || len(result.Data) == 0 || uint64(len(encoded)) > uint64(g.maxResponseBytes) || uint64(len(result.Data)) > uint64(g.maxResponseBytes) ||
		(len(result.Data) != 0 && protocol.ValidateJSON(result.Data, protocol.Limits{MaxBytes: int(g.maxResponseBytes)}) != nil) ||
		!validWorkerErrors(result.Errors, g.maxResponseBytes) || !validReferenceGrants(result.References, g.config.Now()) {
		return PublicResult{}, gatewayError(CodeMalformedWorkerData, "worker returned malformed public data", nil)
	}
	if len(result.Data) != 0 && string(result.Data) != "null" {
		if err := g.config.ValidateOutput(g.handlers[request.HandlerID].OutputSchema, result.Data); err != nil {
			return PublicResult{}, gatewayError(CodeOutputInvalid, "worker output does not satisfy its schema", err)
		}
	}
	g.rememberReferencesForRequest(request.RequestID, request.InvocationID, result.References)
	return PublicResult{Data: slices.Clone(result.Data), Errors: slices.Clone(result.Errors)}, nil
}

func (g *ReferenceGateway) Cancel(ctx context.Context, request CancelRequest) (CancellationAck, error) {
	if g == nil || !validIdentifier(request.RequestID) || !validIdentifier(request.InvocationID) {
		return CancellationAck{}, gatewayError(CodeCancellationInvalid, "cancellation request is invalid", nil)
	}
	g.mu.Lock()
	activeRequest, active := g.active[request.InvocationID]
	cancellationSupported := g.registration != nil && slices.Contains(g.registration.Capabilities, CapabilityCancellationAck)
	g.mu.Unlock()
	if !active || activeRequest != request.RequestID || !cancellationSupported {
		return CancellationAck{}, gatewayError(CodeCancellationInvalid, "invocation is not active", nil)
	}
	request.Protocol = ProtocolVersion
	ack, err := g.config.Transport.Cancel(ctx, request)
	if err != nil {
		return CancellationAck{}, gatewayError(CodeCancellationInvalid, "worker cancellation failed", err)
	}
	if ack.Protocol != ProtocolVersion || ack.InvocationID != request.InvocationID || !validCancellationDisposition(ack.Disposition) {
		return CancellationAck{}, gatewayError(CodeCancellationInvalid, "worker cancellation acknowledgement is invalid", nil)
	}
	return ack, nil
}

func (g *ReferenceGateway) release(invocationID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, active := g.active[invocationID]; active {
		delete(g.active, invocationID)
		g.inFlight--
	}
}

func (g *ReferenceGateway) rememberReferences(invocationID string, grants []ReferenceGrant) {
	g.rememberReferencesForRequest("", invocationID, grants)
}

func (g *ReferenceGateway) rememberReferencesForRequest(requestID, ownerInvocation string, grants []ReferenceGrant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, grant := range grants {
		if validIdentifier(grant.ID) && grant.ExpiresAt.After(g.config.Now()) && (grant.Lifetime == ReferenceInvocation || grant.Lifetime == ReferenceRequest) {
			g.references[grant.ID] = storedReference{grant: grant, requestID: requestID, sessionID: g.sessionID, ownerInvocation: ownerInvocation}
		}
	}
}

func validHandler(handler Handler) bool {
	return validIdentifier(handler.ID) && validIdentifier(handler.InputSchema) && validIdentifier(handler.OutputSchema) &&
		handler.Codec == CodecJSON && slices.Contains([]Effect{EffectQuery, EffectMutation, EffectTransaction, EffectSubscription}, handler.Effect) &&
		uniqueKnownCapabilities(handler.RequiredCapabilities)
}

func validIdentifier(value string) bool {
	return identifierPattern.MatchString(value) && utf8.ValidString(value)
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func uniqueKnownCapabilities(values []string) bool {
	known := []string{CapabilityUnary, CapabilityClientStreaming, CapabilityServerStreaming, CapabilityBidirectional, CapabilityCancellationAck, CapabilityIdempotencyReplay, CapabilityTransactions, CapabilitySubscriptionResume}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] || !slices.Contains(known, value) {
			return false
		}
		seen[value] = true
	}
	return true
}

func allContained(required, available []string) bool {
	for _, value := range required {
		if !slices.Contains(available, value) {
			return false
		}
	}
	return true
}

func validWorkerErrors(values []WorkerError, maximum uint32) bool {
	for _, value := range values {
		if !errorCodePattern.MatchString(value.Code) || slices.Contains(reservedWorkerCodes, value.Code) || strings.TrimSpace(value.Message) == "" || !utf8.ValidString(value.Message) || uint64(len(value.Message)) > uint64(maximum) ||
			!validErrorDetails(value.Details, maximum) {
			return false
		}
	}
	return true
}

func validReferenceGrants(grants []ReferenceGrant, now time.Time) bool {
	seen := make(map[string]bool, len(grants))
	for _, grant := range grants {
		if !validIdentifier(grant.ID) || seen[grant.ID] || !grant.ExpiresAt.After(now) ||
			(grant.Lifetime != ReferenceInvocation && grant.Lifetime != ReferenceRequest) {
			return false
		}
		seen[grant.ID] = true
	}
	return true
}

func validErrorDetails(raw json.RawMessage, maximum uint32) bool {
	if len(raw) == 0 {
		return true
	}
	if protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: int(maximum)}) != nil {
		return false
	}
	var details map[string]json.RawMessage
	if json.Unmarshal(raw, &details) != nil {
		return false
	}
	for key := range details {
		if !strings.Contains(key, ".") {
			return false
		}
	}
	return true
}

func validCancellationDisposition(value CancellationDisposition) bool {
	return slices.Contains([]CancellationDisposition{CancellationRequested, CancellationAcknowledged, CancellationTooLate, CancellationUnsupported}, value)
}
