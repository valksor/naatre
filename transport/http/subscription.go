package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	stdhttp "net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

const (
	SubscriptionEstablishmentMediaType = "application/vnd.naatre.subscription-establishment+json;version=1"
	SubscriptionHandleMediaType        = "application/vnd.naatre.subscription-handle+json;version=1"
	DefaultSubscriptionPath            = "/v1/subscriptions"
)

type SubscriptionConfig struct {
	Path            string
	Coordinator     *runtime.SubscriptionHandleCoordinator
	Decode          protocol.DecodeOptions
	Limits          Limits
	Authenticate    AuthenticateFunc
	WWWAuthenticate string
	CORS            CORS
	SameOrigin      string
	CSRFCookie      string
	CSRFHeader      string
	RequestID       func() string
	Shutdown        <-chan struct{}
}

// SubscriptionHandler is the router-free HTTP binding for secure subscription
// establishment, observation, renewal, cancellation, and SSE attachment.
type SubscriptionHandler struct {
	path         string
	coordinator  *runtime.SubscriptionHandleCoordinator
	decode       protocol.DecodeOptions
	limits       Limits
	authenticate AuthenticateFunc
	sameOrigin   string
	csrfCookie   string
	csrfHeader   string
	shutdown     <-chan struct{}
	base         *Handler
}

func NewSubscriptionHandler(config SubscriptionConfig) (*SubscriptionHandler, error) {
	if config.Coordinator == nil || config.Authenticate == nil || config.WWWAuthenticate == "" {
		return nil, errors.New("subscription HTTP coordinator and authentication are required")
	}
	limits, err := resolveLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	cors, err := resolveCORS(config.CORS)
	if err != nil {
		return nil, err
	}
	path := config.Path
	if path == "" {
		path = DefaultSubscriptionPath
	}
	if path == "/" || !strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.ContainsAny(path, "?#") {
		return nil, errors.New("subscription path must be an absolute non-root path without a trailing slash, query, or fragment")
	}
	profile := config.Coordinator.DeliveryProfile()
	if slices.Contains(profile.BrowserAuthentication, runtime.SubscriptionSameOriginCookie) {
		origin, parseErr := url.Parse(config.SameOrigin)
		if parseErr != nil || origin.Scheme != "https" || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			return nil, errors.New("same-origin cookie delivery requires one exact HTTPS origin")
		}
	}
	if config.CSRFCookie == "" {
		config.CSRFCookie = "__Host-naatre-csrf"
	}
	if config.CSRFHeader == "" {
		config.CSRFHeader = "Naatre-CSRF"
	}
	if strings.ContainsAny(config.CSRFCookie+config.CSRFHeader, "\x00\r\n, ") {
		return nil, errors.New("invalid subscription CSRF field name")
	}
	requestID := config.RequestID
	if requestID == nil {
		requestID = randomRequestID
	}
	base := &Handler{cors: cors, requestID: requestID, wwwAuthenticate: config.WWWAuthenticate}
	return &SubscriptionHandler{
		path: path, coordinator: config.Coordinator, decode: config.Decode, limits: limits,
		authenticate: config.Authenticate, sameOrigin: config.SameOrigin,
		csrfCookie: config.CSRFCookie, csrfHeader: config.CSRFHeader, shutdown: config.Shutdown, base: base,
	}, nil
}

func (handler *SubscriptionHandler) ServeHTTP(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	tracked := &trackingWriter{ResponseWriter: writer}
	setVary(tracked.Header(), "Accept", "Origin")
	setSubscriptionSafetyHeaders(tracked.Header())
	if len(request.RequestURI) > handler.limits.MaxRequestTargetBytes {
		handler.base.writeProblem(tracked, stdhttp.StatusRequestURITooLong, "URI_TOO_LONG")
		return
	}
	if request.URL.RawQuery != "" {
		handler.base.writeProblem(tracked, stdhttp.StatusBadRequest, "CURSOR_IN_URL_FORBIDDEN")
		return
	}
	if request.Method == stdhttp.MethodOptions {
		handler.handlePreflight(tracked, request)
		return
	}
	if status, code := validateSubscriptionAccept(request.Header.Values("Accept"), strings.HasSuffix(request.URL.Path, "/events")); code != "" {
		handler.base.writeProblem(tracked, status, code)
		return
	}
	if request.Header.Get("Origin") != handler.sameOrigin && !handler.base.applyCORS(tracked, request) {
		return
	}
	if channelClosed(handler.shutdown) {
		handler.writeSubscriptionProblem(tracked, runtime.ErrSubscriptionCapacity)
		return
	}
	if request.URL.Path == handler.path {
		if request.Method != stdhttp.MethodPost {
			tracked.Header().Set("Allow", "POST, OPTIONS")
			handler.base.writeProblem(tracked, stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
			return
		}
		handler.serveEstablishment(tracked, request)
		return
	}
	id, action, ok := handler.route(request.URL.Path)
	if !ok {
		handler.base.writeProblem(tracked, stdhttp.StatusNotFound, "NOT_FOUND")
		return
	}
	switch action {
	case "observe":
		if request.Method == stdhttp.MethodDelete {
			handler.serveCancel(tracked, request, id)
			return
		}
		if request.Method != stdhttp.MethodGet {
			tracked.Header().Set("Allow", "GET, DELETE, OPTIONS")
			handler.base.writeProblem(tracked, stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
			return
		}
		handler.serveObserve(tracked, request, id)
	case "events":
		if request.Method != stdhttp.MethodGet {
			tracked.Header().Set("Allow", "GET, OPTIONS")
			handler.base.writeProblem(tracked, stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
			return
		}
		handler.serveEvents(tracked, request, id)
	case "renew":
		if request.Method != stdhttp.MethodPost {
			tracked.Header().Set("Allow", "POST, OPTIONS")
			handler.base.writeProblem(tracked, stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
			return
		}
		handler.serveRenew(tracked, request, id)
	case "cancel":
		if request.Method != stdhttp.MethodDelete {
			tracked.Header().Set("Allow", "DELETE, OPTIONS")
			handler.base.writeProblem(tracked, stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
			return
		}
		handler.serveCancel(tracked, request, id)
	}
}

type subscriptionEstablishmentWire struct {
	Request                json.RawMessage `json:"request"`
	DeliveryAuthentication []string        `json:"deliveryAuthentication"`
}

type subscriptionDeliveryProfileWire struct {
	Name                     string   `json:"name"`
	Replay                   string   `json:"replay"`
	RetentionHorizonSeconds  int64    `json:"retentionHorizonSeconds"`
	LossDetection            bool     `json:"lossDetection"`
	TerminalFrames           bool     `json:"terminalFrames"`
	MaximumConnectionSeconds int64    `json:"maximumConnectionSeconds"`
	ReconnectStaggerSeconds  int64    `json:"reconnectStaggerSeconds"`
	MaximumReconnectAttempts int      `json:"maximumReconnectAttempts"`
	BrowserAuthentication    []string `json:"browserAuthentication"`
}

type subscriptionHandleWire struct {
	State                string                          `json:"state"`
	DeliveryEndpoint     string                          `json:"deliveryEndpoint"`
	MediaType            string                          `json:"mediaType"`
	ExpiresAt            string                          `json:"expiresAt"`
	Snapshot             json.RawMessage                 `json:"snapshot,omitempty"`
	ReconciliationCursor string                          `json:"reconciliationCursor,omitempty"`
	DeliveryProfile      subscriptionDeliveryProfileWire `json:"deliveryProfile"`
}

func (handler *SubscriptionHandler) serveEstablishment(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	ctx, ok := handler.authenticateRequest(writer, request, true)
	if !ok {
		return
	}
	if status, code := validateSubscriptionContentType(request.Header.Get("Content-Type")); code != "" {
		handler.base.writeProblem(writer, status, code)
		return
	}
	body, err := readRequestBody(ctx, request.Body, request.ContentLength, "identity", handler.limits)
	if err != nil {
		if errors.Is(err, errLimitExceeded) {
			handler.base.writeProblem(writer, stdhttp.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")
		} else {
			handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "MALFORMED_JSON")
		}
		return
	}
	if err := protocol.ValidateJSON(body, handler.decode.Limits); err != nil {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "MALFORMED_JSON")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var establishment subscriptionEstablishmentWire
	if err := decoder.Decode(&establishment); err != nil || len(establishment.Request) == 0 || len(establishment.DeliveryAuthentication) == 0 {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "MALFORMED_JSON")
		return
	}
	decoded, err := protocol.DecodeRequest(establishment.Request, handler.decode)
	if err != nil {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "MALFORMED_OPERATION")
		return
	}
	requested := make([]runtime.SubscriptionBrowserAuthentication, len(establishment.DeliveryAuthentication))
	for index, value := range establishment.DeliveryAuthentication {
		requested[index] = runtime.SubscriptionBrowserAuthentication(value)
	}
	canonical, err := protocol.CanonicalizeJSON(body, handler.decode.Limits)
	if err != nil {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "MALFORMED_JSON")
		return
	}
	digest := sha256.Sum256(canonical)
	idempotencyKey, valid := singletonSubscriptionHeader(request.Header, "Naatre-Idempotency-Key")
	if !valid {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "MALFORMED_HEADER")
		return
	}
	result, err := handler.coordinator.Establish(ctx, runtime.SubscriptionHandleRequest{
		Request: decoded, IdempotencyKey: idempotencyKey, Fingerprint: "sha256:" + hex.EncodeToString(digest[:]), RequestedAuthentication: requested,
	})
	if err != nil {
		handler.writeSubscriptionProblem(writer, err)
		return
	}
	handler.writeEstablishment(writer, result)
}

func (handler *SubscriptionHandler) serveObserve(writer stdhttp.ResponseWriter, request *stdhttp.Request, id string) {
	ctx, ok := handler.authenticateRequest(writer, request, false)
	if !ok {
		return
	}
	handle, err := handler.coordinator.Observe(ctx, id)
	if err != nil {
		handler.writeSubscriptionProblem(writer, err)
		return
	}
	handler.writeHandle(writer, stdhttp.StatusOK, handle, false, nil, "")
}

func (handler *SubscriptionHandler) serveRenew(writer stdhttp.ResponseWriter, request *stdhttp.Request, id string) {
	ctx, ok := handler.authenticateRequest(writer, request, true)
	if !ok {
		return
	}
	handle, err := handler.coordinator.Renew(ctx, id)
	if err != nil {
		handler.writeSubscriptionProblem(writer, err)
		return
	}
	handler.writeHandle(writer, stdhttp.StatusOK, handle, false, nil, "")
}

func (handler *SubscriptionHandler) serveCancel(writer stdhttp.ResponseWriter, request *stdhttp.Request, id string) {
	ctx, ok := handler.authenticateRequest(writer, request, true)
	if !ok {
		return
	}
	if err := handler.coordinator.Cancel(ctx, id); err != nil {
		handler.writeSubscriptionProblem(writer, err)
		return
	}
	writer.WriteHeader(stdhttp.StatusNoContent)
}

func (handler *SubscriptionHandler) serveEvents(writer stdhttp.ResponseWriter, request *stdhttp.Request, id string) {
	path, ok := handler.deliveryAuthenticationPath(writer, request)
	if !ok {
		return
	}
	ctx, authenticated := handler.authenticateRequest(writer, request, false)
	if !authenticated {
		return
	}
	if !slices.Contains(handler.coordinator.DeliveryProfile().BrowserAuthentication, path) {
		handler.writeSubscriptionProblem(writer, runtime.ErrSubscriptionCapabilityUnsupported)
		return
	}
	cursor, valid := singletonSubscriptionHeader(request.Header, "Last-Event-ID")
	if !valid || strings.ContainsAny(cursor, "\x00\r\n") {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "MALFORMED_CURSOR")
		return
	}
	source, err := handler.coordinator.Open(ctx, id, cursor)
	if err != nil {
		handler.writeSubscriptionProblem(writer, err)
		return
	}
	defer func() { _ = source.Close() }()
	first, err := source.Next(ctx)
	if err != nil {
		handler.writeSubscriptionProblem(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Content-Encoding", "identity")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(stdhttp.StatusOK)
	if !handler.writeSSEFrame(writer, first) {
		return
	}
	lastSequence := first.Sequence
	stream := first.Stream
	for {
		frame, nextErr := source.Next(ctx)
		if nextErr != nil {
			if errors.Is(nextErr, context.Canceled) || errors.Is(nextErr, stdhttp.ErrAbortHandler) {
				return
			}
			failure := protocol.StreamFrame{Type: protocol.StreamError, Stream: stream, Sequence: lastSequence + 1, Final: true, Error: &protocol.StreamFrameError{Code: subscriptionStreamErrorCode(nextErr), Message: "subscription delivery stopped"}}
			_ = handler.writeSSEFrame(writer, failure)
			return
		}
		if !handler.writeSSEFrame(writer, frame) {
			return
		}
		if frame.Type != protocol.StreamKeepalive {
			lastSequence = frame.Sequence
		}
		if frame.Type == protocol.StreamComplete || (frame.Type == protocol.StreamError && frame.Final) {
			return
		}
	}
}

func (handler *SubscriptionHandler) authenticateRequest(writer stdhttp.ResponseWriter, request *stdhttp.Request, stateChanging bool) (context.Context, bool) {
	if problem := validateSubscriptionSingletonHeaders(request.Header); problem != "" {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, problem)
		return nil, false
	}
	if request.Header.Get("Naatre-Principal") != "" {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "UNTRUSTED_IDENTITY_HEADER")
		return nil, false
	}
	if request.Header.Get("Authorization") != "" && request.Header.Get("Cookie") != "" {
		handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "AMBIGUOUS_CREDENTIALS")
		return nil, false
	}
	if stateChanging && request.Header.Get("Authorization") == "" && !handler.validCSRF(request) {
		handler.base.writeProblem(writer, stdhttp.StatusForbidden, "CSRF_FORBIDDEN")
		return nil, false
	}
	ctx, err := handler.authenticate(request.Context(), request)
	if err != nil || ctx == nil {
		writer.Header().Set("WWW-Authenticate", handler.base.wwwAuthenticate)
		handler.base.writeProblem(writer, stdhttp.StatusUnauthorized, "UNAUTHENTICATED")
		return nil, false
	}
	return ctx, true
}

func (handler *SubscriptionHandler) deliveryAuthenticationPath(writer stdhttp.ResponseWriter, request *stdhttp.Request) (runtime.SubscriptionBrowserAuthentication, bool) {
	if request.Header.Get("Authorization") != "" {
		if request.Header.Get("Cookie") != "" {
			handler.base.writeProblem(writer, stdhttp.StatusBadRequest, "AMBIGUOUS_CREDENTIALS")
			return "", false
		}
		return runtime.SubscriptionFetchBearer, true
	}
	if request.Header.Get("Cookie") == "" || request.Header.Get("Sec-Fetch-Site") != "same-origin" || (request.Header.Get("Origin") != "" && request.Header.Get("Origin") != handler.sameOrigin) {
		handler.base.writeProblem(writer, stdhttp.StatusForbidden, "CROSS_ORIGIN_DELIVERY_FORBIDDEN")
		return "", false
	}
	return runtime.SubscriptionSameOriginCookie, true
}

func (handler *SubscriptionHandler) validCSRF(request *stdhttp.Request) bool {
	if request.Header.Get("Origin") != handler.sameOrigin {
		return false
	}
	cookie, err := request.Cookie(handler.csrfCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	header := request.Header.Get(handler.csrfHeader)
	return len(header) == len(cookie.Value) && subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) == 1
}

func (handler *SubscriptionHandler) writeEstablishment(writer stdhttp.ResponseWriter, result runtime.SubscriptionHandleEstablishment) {
	status := stdhttp.StatusCreated
	if !result.Created {
		status = stdhttp.StatusOK
	}
	handler.writeHandle(writer, status, result.Handle, true, result.Snapshot, result.ReconciliationCursor)
}

func (handler *SubscriptionHandler) writeHandle(writer stdhttp.ResponseWriter, status int, handle runtime.SubscriptionHandle, establishment bool, snapshot json.RawMessage, cursor string) {
	delivery := handler.path + "/" + handle.ID + "/events"
	resource := handler.path + "/" + handle.ID
	writer.Header().Set("Content-Type", SubscriptionHandleMediaType)
	writer.Header().Set("Location", resource)
	writer.Header().Set("Link", "<"+delivery+">; rel=\"https://naatre.dev/rels/subscription-delivery\"; type=\"text/event-stream\", <"+resource+"/renew>; rel=\"https://naatre.dev/rels/subscription-renewal\"")
	writer.Header().Set("Naatre-Expires", handle.ExpiresAt.UTC().Format(time.RFC3339))
	writer.Header().Set("Naatre-Delivery-Capabilities", strings.Join(subscriptionAuthenticationStrings(handle.DeliveryProfile.BrowserAuthentication), ", "))
	writer.Header().Set("Access-Control-Expose-Headers", "Location, Link, Naatre-Expires, Naatre-Delivery-Capabilities")
	body := subscriptionHandleWire{
		State: string(handle.State), DeliveryEndpoint: delivery, MediaType: handle.DeliveryProfile.MediaType,
		ExpiresAt: handle.ExpiresAt.UTC().Format(time.RFC3339Nano), DeliveryProfile: subscriptionProfileWire(handle.DeliveryProfile),
	}
	if establishment {
		body.Snapshot = slices.Clone(snapshot)
		body.ReconciliationCursor = cursor
	}
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func (handler *SubscriptionHandler) writeSubscriptionProblem(writer stdhttp.ResponseWriter, err error) {
	status, code := stdhttp.StatusConflict, "REESTABLISH_REQUIRED"
	switch {
	case errors.Is(err, runtime.ErrSubscriptionHistoryUnavailable), errors.Is(err, runtime.ErrStreamHistoryUnavailable):
		code = "REFETCH_REQUIRED"
	case errors.Is(err, runtime.ErrSubscriptionCapabilityUnsupported):
		status, code = stdhttp.StatusNotAcceptable, "DELIVERY_CAPABILITY_UNSUPPORTED"
	case errors.Is(err, runtime.ErrSubscriptionIdempotencyConflict):
		code = "ESTABLISHMENT_CONFLICT"
	case errors.Is(err, runtime.ErrSubscriptionCapacity), errors.Is(err, runtime.ErrSubscriptionBrokerUnavailable):
		status, code = stdhttp.StatusServiceUnavailable, "DELIVERY_UNAVAILABLE"
		writer.Header().Set("Retry-After", "1")
	case errors.Is(err, context.DeadlineExceeded):
		status, code = stdhttp.StatusGatewayTimeout, "DELIVERY_UNAVAILABLE"
	}
	handler.base.writeProblem(writer, status, code)
}

func (handler *SubscriptionHandler) writeSSEFrame(writer stdhttp.ResponseWriter, frame protocol.StreamFrame) bool {
	encoded, err := EncodeSSE(frame, DefaultSSELimits())
	if err != nil {
		return false
	}
	if _, err := writer.Write(encoded); err != nil {
		return false
	}
	if flusher, ok := writer.(stdhttp.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func (handler *SubscriptionHandler) route(path string) (string, string, bool) {
	if !strings.HasPrefix(path, handler.path+"/") {
		return "", "", false
	}
	remainder := strings.TrimPrefix(path, handler.path+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 1 && validSubscriptionPathID(parts[0]) {
		return parts[0], "observe", true
	}
	if len(parts) != 2 || !validSubscriptionPathID(parts[0]) {
		return "", "", false
	}
	switch parts[1] {
	case "events", "renew":
		return parts[0], parts[1], true
	default:
		return "", "", false
	}
}

func (handler *SubscriptionHandler) handlePreflight(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	origin := request.Header.Get("Origin")
	method := request.Header.Get("Access-Control-Request-Method")
	if origin == "" || method == "" || !handler.base.originAllowed(origin) || (method != stdhttp.MethodGet && method != stdhttp.MethodPost && method != stdhttp.MethodDelete) {
		handler.base.writeProblem(writer, stdhttp.StatusForbidden, "CORS_FORBIDDEN")
		return
	}
	setVary(writer.Header(), "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
	allowedHeaders := handler.base.cors.AllowedHeaders
	if len(allowedHeaders) == 0 {
		allowedHeaders = []string{"Authorization", "Content-Type", "Last-Event-ID", "Naatre-Idempotency-Key", handler.csrfHeader}
	}
	if !headersAllowed(request.Header.Get("Access-Control-Request-Headers"), allowedHeaders) {
		handler.base.writeProblem(writer, stdhttp.StatusForbidden, "CORS_FORBIDDEN")
		return
	}
	writer.Header().Set("Access-Control-Allow-Origin", origin)
	writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
	writer.Header().Set("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))
	if handler.base.cors.AllowCredentials {
		writer.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	writer.WriteHeader(stdhttp.StatusNoContent)
}

func validateSubscriptionContentType(value string) (int, string) {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(mediaType, "application/vnd.naatre.subscription-establishment+json") || parameters["version"] != "1" {
		return stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"
	}
	for name, value := range parameters {
		if name == "version" {
			continue
		}
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"
		}
	}
	return 0, ""
}

func validateSubscriptionAccept(lines []string, events bool) (int, string) {
	if len(lines) == 0 {
		return 0, ""
	}
	wanted := "application/vnd.naatre.subscription-handle+json"
	if events {
		wanted = "text/event-stream"
	}
	accepted := false
	for _, line := range lines {
		for _, item := range strings.Split(line, ",") {
			mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil {
				return stdhttp.StatusBadRequest, "MALFORMED_HEADER"
			}
			quality, err := qualityParameter(parameters)
			if err != nil {
				return stdhttp.StatusBadRequest, "MALFORMED_HEADER"
			}
			for name := range parameters {
				if name != "q" && name != "version" {
					quality = 0
				}
			}
			if parameters["version"] != "" && (events || parameters["version"] != "1") {
				quality = 0
			}
			if quality > 0 && (strings.EqualFold(mediaType, wanted) || strings.EqualFold(mediaType, "*/*")) {
				accepted = true
			}
		}
	}
	if !accepted {
		return stdhttp.StatusNotAcceptable, "NOT_ACCEPTABLE"
	}
	return 0, ""
}

func validateSubscriptionSingletonHeaders(header stdhttp.Header) string {
	for _, name := range []string{"Authorization", "Content-Type", "Cookie", "Last-Event-ID", "Naatre-Idempotency-Key", "Naatre-CSRF", "Origin", "Sec-Fetch-Site"} {
		if _, ok := singletonSubscriptionHeader(header, name); !ok {
			return "MALFORMED_HEADER"
		}
	}
	return ""
}

func singletonSubscriptionHeader(header stdhttp.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) > 1 || (len(values) == 1 && strings.ContainsAny(values[0], "\r\n")) {
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}
	return values[0], true
}

func subscriptionProfileWire(profile runtime.SubscriptionDeliveryProfile) subscriptionDeliveryProfileWire {
	return subscriptionDeliveryProfileWire{
		Name: profile.Name, Replay: string(profile.Replay), RetentionHorizonSeconds: int64(profile.RetentionHorizon / time.Second),
		LossDetection: profile.LossDetection, TerminalFrames: profile.TerminalFrames,
		MaximumConnectionSeconds: int64(profile.MaxConnectionLifetime / time.Second), ReconnectStaggerSeconds: int64(profile.ReconnectStagger / time.Second),
		MaximumReconnectAttempts: profile.MaxReconnectAttempts, BrowserAuthentication: subscriptionAuthenticationStrings(profile.BrowserAuthentication),
	}
}

func subscriptionAuthenticationStrings(paths []runtime.SubscriptionBrowserAuthentication) []string {
	result := make([]string, len(paths))
	for index, path := range paths {
		result[index] = string(path)
	}
	return result
}

func setSubscriptionSafetyHeaders(header stdhttp.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}

func validSubscriptionPathID(value string) bool {
	if len(value) < 16 || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, current := range value {
		if (current < 'a' || current > 'z') && (current < 'A' || current > 'Z') && (current < '0' || current > '9') && current != '-' && current != '_' {
			return false
		}
	}
	return true
}

func subscriptionStreamErrorCode(err error) string {
	switch {
	case errors.Is(err, runtime.ErrStreamAuthenticationExpired):
		return "AUTHENTICATION_EXPIRED"
	case errors.Is(err, runtime.ErrStreamSchemaRetired):
		return "SCHEMA_RETIRED"
	case errors.Is(err, runtime.ErrStreamAuthorizationRevoked), errors.Is(err, runtime.ErrSubscriptionHandleUnavailable):
		return "AUTHORIZATION_REVOKED"
	case errors.Is(err, runtime.ErrSubscriptionHistoryUnavailable), errors.Is(err, runtime.ErrStreamHistoryUnavailable):
		return "HISTORY_UNAVAILABLE"
	default:
		return "DELIVERY_INTERRUPTED"
	}
}
