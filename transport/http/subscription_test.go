package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

const validSubscriptionRequest = `{"version":"1","id":"browser-1","document":{"operations":[{"name":"Orders","kind":"subscription","select":[{"$call":{"name":"orders"}}]}]}}`

func TestSubscriptionBrowserFetchAndNativeEventSourceFixtures(t *testing.T) {
	handler, coordinator, broker, _ := subscriptionHTTPFixture(t)
	established := establishSubscriptionHTTP(t, handler, "Bearer owner", []string{"fetch-bearer", "same-origin-cookie"})
	if established.State != "active" || established.MediaType != "text/event-stream" || established.ReconciliationCursor == "" || len(established.Snapshot) == 0 {
		t.Fatalf("establishment = %#v", established)
	}
	assertSafeSubscriptionURL(t, established.DeliveryEndpoint, established.ReconciliationCursor)
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamPatch, Path: []any{"count"}, Data: json.RawMessage(`2`)}); err != nil {
		t.Fatal(err)
	}
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamComplete}); err != nil {
		t.Fatal(err)
	}

	fetch := httptest.NewRequest(stdhttp.MethodGet, established.DeliveryEndpoint, nil)
	fetch.Header.Set("Authorization", "Bearer owner")
	fetch.Header.Set("Last-Event-ID", established.ReconciliationCursor)
	fetch.Header.Set("Origin", "https://app.example")
	fetchResponse := serve(handler, fetch)
	assertSubscriptionSSE(t, fetchResponse, []string{"naatre.open", "naatre.resume", "naatre.patch", "naatre.complete"})
	if fetchResponse.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("fetch CORS headers = %v", fetchResponse.Header())
	}

	native := httptest.NewRequest(stdhttp.MethodGet, established.DeliveryEndpoint, nil)
	native.Header.Set("Cookie", "__Host-naatre-session=owner")
	native.Header.Set("Sec-Fetch-Site", "same-origin")
	nativeResponse := serve(handler, native)
	assertSubscriptionSSE(t, nativeResponse, []string{"naatre.open", "naatre.resume", "naatre.patch", "naatre.complete"})
	if native.URL.RawQuery != "" || native.Header.Get("Authorization") != "" || native.Header.Get("Last-Event-ID") != "" {
		t.Fatalf("native EventSource fixture used unsafe request metadata: %s %v", native.URL.String(), native.Header)
	}

	coordinator.Revoke(context.Background(), strings.TrimSuffix(strings.TrimPrefix(established.DeliveryEndpoint, DefaultSubscriptionPath+"/"), "/events"))
}

func TestSubscriptionHTTPAuthorityFailuresAreIndistinguishable(t *testing.T) {
	handler, coordinator, _, _ := subscriptionHTTPFixture(t)
	established := establishSubscriptionHTTP(t, handler, "Bearer owner", []string{"fetch-bearer"})
	id := strings.TrimSuffix(strings.TrimPrefix(established.DeliveryEndpoint, DefaultSubscriptionPath+"/"), "/events")
	unauthorized := httptest.NewRequest(stdhttp.MethodGet, DefaultSubscriptionPath+"/"+id, nil)
	unauthorized.Header.Set("Authorization", "Bearer other")
	unknown := httptest.NewRequest(stdhttp.MethodGet, DefaultSubscriptionPath+"/00000000000000000000000000000000", nil)
	unknown.Header.Set("Authorization", "Bearer owner")
	first := serve(handler, unauthorized)
	second := serve(handler, unknown)
	if first.Code != stdhttp.StatusConflict || second.Code != first.Code || first.Body.String() != second.Body.String() || !strings.Contains(first.Body.String(), `"code":"REESTABLISH_REQUIRED"`) {
		t.Fatalf("safe unavailable mismatch: %d %s / %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	for _, response := range []*httptest.ResponseRecorder{first, second} {
		if response.Header().Get("Location") != "" || strings.Contains(response.Body.String(), id) {
			t.Fatalf("unavailable outcome disclosed authority: headers=%v body=%s", response.Header(), response.Body.String())
		}
	}
	coordinator.Revoke(context.Background(), id)
	revoked := httptest.NewRequest(stdhttp.MethodGet, DefaultSubscriptionPath+"/"+id, nil)
	revoked.Header.Set("Authorization", "Bearer owner")
	third := serve(handler, revoked)
	if third.Code != second.Code || third.Body.String() != second.Body.String() {
		t.Fatalf("revoked mismatch: %d %s / %d %s", third.Code, third.Body.String(), second.Code, second.Body.String())
	}
}

func TestSubscriptionHTTPRejectsCursorURLsCredentialsRedirectsAndCSRF(t *testing.T) {
	handler, _, _, _ := subscriptionHTTPFixture(t)
	established := establishSubscriptionHTTP(t, handler, "Bearer owner", []string{"same-origin-cookie"})
	unsafe := httptest.NewRequest(stdhttp.MethodGet, established.DeliveryEndpoint+"?cursor=secret-cursor", nil)
	unsafe.Header.Set("Cookie", "__Host-naatre-session=owner")
	unsafe.Header.Set("Sec-Fetch-Site", "same-origin")
	response := serve(handler, unsafe)
	assertProblem(t, response, stdhttp.StatusBadRequest, "CURSOR_IN_URL_FORBIDDEN")
	if strings.Contains(response.Body.String(), "secret-cursor") || response.Header().Get("Location") != "" {
		t.Fatalf("unsafe cursor reflected or redirected: %v %s", response.Header(), response.Body.String())
	}

	ambiguous := httptest.NewRequest(stdhttp.MethodGet, established.DeliveryEndpoint, nil)
	ambiguous.Header.Set("Authorization", "Bearer owner")
	ambiguous.Header.Set("Cookie", "__Host-naatre-session=owner")
	assertProblem(t, serve(handler, ambiguous), stdhttp.StatusBadRequest, "AMBIGUOUS_CREDENTIALS")

	id := strings.TrimSuffix(strings.TrimPrefix(established.DeliveryEndpoint, DefaultSubscriptionPath+"/"), "/events")
	renew := httptest.NewRequest(stdhttp.MethodPost, DefaultSubscriptionPath+"/"+id+"/renew", nil)
	renew.Header.Set("Cookie", "__Host-naatre-session=owner; __Host-naatre-csrf=csrf-secret")
	assertProblem(t, serve(handler, renew), stdhttp.StatusForbidden, "CSRF_FORBIDDEN")
	renew = httptest.NewRequest(stdhttp.MethodPost, DefaultSubscriptionPath+"/"+id+"/renew", nil)
	renew.Header.Set("Cookie", "__Host-naatre-session=owner; __Host-naatre-csrf=csrf-secret")
	renew.Header.Set("Origin", "https://api.example")
	renew.Header.Set("Naatre-CSRF", "csrf-secret")
	if got := serve(handler, renew); got.Code != stdhttp.StatusOK {
		t.Fatalf("CSRF-authorized renewal = %d %s", got.Code, got.Body.String())
	}
}

func TestSubscriptionHTTPPreflightAndMetadataExposure(t *testing.T) {
	handler, _, _, _ := subscriptionHTTPFixture(t)
	preflight := httptest.NewRequest(stdhttp.MethodOptions, DefaultSubscriptionPath, nil)
	preflight.Header.Set("Origin", "https://app.example")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "authorization, content-type, naatre-idempotency-key")
	response := serve(handler, preflight)
	if response.Code != stdhttp.StatusNoContent || response.Header().Get("Access-Control-Allow-Origin") != "https://app.example" || !strings.Contains(strings.ToLower(response.Header().Get("Access-Control-Allow-Headers")), "last-event-id") {
		t.Fatalf("preflight = %d %v %s", response.Code, response.Header(), response.Body.String())
	}
	establish := newSubscriptionEstablishmentRequest("Bearer owner", []string{"fetch-bearer"})
	establish.Header.Set("Origin", "https://app.example")
	establishedResponse := serve(handler, establish)
	if establishedResponse.Code != stdhttp.StatusCreated || establishedResponse.Header().Get("Location") == "" || establishedResponse.Header().Get("Link") == "" || establishedResponse.Header().Get("Naatre-Expires") == "" || !strings.Contains(establishedResponse.Header().Get("Access-Control-Expose-Headers"), "Naatre-Delivery-Capabilities") {
		t.Fatalf("establishment metadata = %d %v %s", establishedResponse.Code, establishedResponse.Header(), establishedResponse.Body.String())
	}
	for _, forbidden := range []string{"Bearer owner", validSubscriptionRequest, `"variables"`, "csrf-secret"} {
		if strings.Contains(establishedResponse.Header().Get("Location")+establishedResponse.Header().Get("Link"), forbidden) {
			t.Fatalf("generated link contains %q: %v", forbidden, establishedResponse.Header())
		}
	}
	if establishedResponse.Header().Get("Referrer-Policy") != "no-referrer" || establishedResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("safety headers = %v", establishedResponse.Header())
	}
}

func TestSubscriptionHTTPNegotiatesHandleAndSSEMediaTypes(t *testing.T) {
	handler, _, _, _ := subscriptionHTTPFixture(t)
	establish := newSubscriptionEstablishmentRequest("Bearer owner", []string{"fetch-bearer"})
	establish.Header.Set("Accept", "text/event-stream")
	assertProblem(t, serve(handler, establish), stdhttp.StatusNotAcceptable, "NOT_ACCEPTABLE")

	established := establishSubscriptionHTTP(t, handler, "Bearer owner", []string{"fetch-bearer"})
	events := httptest.NewRequest(stdhttp.MethodGet, established.DeliveryEndpoint, nil)
	events.Header.Set("Authorization", "Bearer owner")
	events.Header.Set("Accept", SubscriptionHandleMediaType)
	assertProblem(t, serve(handler, events), stdhttp.StatusNotAcceptable, "NOT_ACCEPTABLE")

	malformed := httptest.NewRequest(stdhttp.MethodGet, established.DeliveryEndpoint, nil)
	malformed.Header.Set("Authorization", "Bearer owner")
	malformed.Header.Set("Accept", "text/event-stream;q=invalid")
	assertProblem(t, serve(handler, malformed), stdhttp.StatusBadRequest, "MALFORMED_HEADER")
}

type subscriptionHTTPAuthorization struct {
	mu       sync.Mutex
	allowed  bool
	schema   string
	revision string
	expires  time.Time
}

func subscriptionHTTPFixture(t *testing.T) (*SubscriptionHandler, *runtime.SubscriptionHandleCoordinator, *runtime.MemorySubscriptionBroker, *subscriptionHTTPAuthorization) {
	t.Helper()
	now := time.Now()
	codec, err := runtime.NewStreamCursorCodec(runtime.StreamCursorCodecConfig{
		ActiveKeyID: "key-1", Keys: map[string][]byte{"key-1": []byte("0123456789abcdef0123456789abcdef")}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := runtime.SubscriptionDeliveryProfile{
		Name: "in-process-v1", MediaType: "text/event-stream", Replay: protocol.StreamReplayBounded,
		RetentionHorizon: time.Minute, LossDetection: true, TerminalFrames: true,
		MaxConnectionLifetime: 30 * time.Minute, ReconnectStagger: 5 * time.Minute, MaxReconnectAttempts: 5,
		BrowserAuthentication: []runtime.SubscriptionBrowserAuthentication{runtime.SubscriptionFetchBearer, runtime.SubscriptionSameOriginCookie},
	}
	broker, err := runtime.NewMemorySubscriptionBroker(runtime.MemorySubscriptionBrokerConfig{
		Profile: profile, CursorCodec: codec, Snapshot: func(context.Context, runtime.SubscriptionHandleBinding) (json.RawMessage, error) {
			return json.RawMessage(`{"count":1}`), nil
		},
		MaxSubscriptions: 16, MaxHistoryEvents: 16, MaxHistoryBytes: 1 << 20, MaxTotalHistoryBytes: 4 << 20, SubscriberQueue: 16, RetentionDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := runtime.NewStreamCheckExecutor(4)
	if err != nil {
		t.Fatal(err)
	}
	authorization := &subscriptionHTTPAuthorization{allowed: true, schema: "schema-r1", revision: "authorization-r1", expires: now.Add(time.Hour)}
	coordinator, err := runtime.NewSubscriptionHandleCoordinator(runtime.SubscriptionHandleConfig{
		Broker: broker, CheckExecutor: executor, HandleTTL: 20 * time.Minute, MaxHandles: 16, MaxAttachments: 16, MaxTotalSnapshotBytes: 4 << 20,
		Prepare: func(_ context.Context, request runtime.SubscriptionHandleRequest) (runtime.SubscriptionHandlePreparation, error) {
			if request.Request == nil {
				return runtime.SubscriptionHandlePreparation{}, errors.New("missing request")
			}
			return runtime.SubscriptionHandlePreparation{
				OperationIdentity: "operation:orders", VariableIdentity: "variables:none", SchemaRevision: "schema-r1", AuthorizationRevision: "authorization-r1",
				AuthenticationExpiresAt: now.Add(time.Hour), Limits: runtime.SubscriptionHandleLimits{Session: runtime.DefaultStreamSessionLimits(), MaxSnapshotBytes: 1 << 20},
			}, nil
		},
		Authorizer: runtime.SubscriptionHandleAuthorizerFunc(func(_ context.Context, _ runtime.SubscriptionHandleAuthorizationRequest) (runtime.SubscriptionHandleAuthorizationDecision, error) {
			authorization.mu.Lock()
			defer authorization.mu.Unlock()
			return runtime.SubscriptionHandleAuthorizationDecision{Allowed: authorization.allowed, SchemaRevision: authorization.schema, AuthorizationRevision: authorization.revision, AuthenticationExpiresAt: authorization.expires}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewSubscriptionHandler(SubscriptionConfig{
		Coordinator: coordinator, SameOrigin: "https://api.example", WWWAuthenticate: `Bearer realm="naatre"`,
		CORS:      CORS{AllowedOrigins: []string{"https://app.example"}, AllowedHeaders: []string{"Authorization", "Content-Type", "Last-Event-ID", "Naatre-Idempotency-Key", "Naatre-CSRF"}},
		RequestID: func() string { return "subscription-request" },
		Authenticate: func(ctx context.Context, request *stdhttp.Request) (context.Context, error) {
			authority := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
			if authority == "" {
				cookie, cookieErr := request.Cookie("__Host-naatre-session")
				if cookieErr != nil {
					return nil, cookieErr
				}
				authority = cookie.Value
			}
			if authority != "owner" && authority != "other" {
				return nil, errors.New("invalid credentials")
			}
			return runtime.WithPrincipal(ctx, runtime.Principal{Subject: authority, Tenant: "tenant", AuthorizationRevision: "authorization-r1"}), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, coordinator, broker, authorization
}

func newSubscriptionEstablishmentRequest(authorization string, delivery []string) *stdhttp.Request {
	body, _ := json.Marshal(map[string]any{"request": json.RawMessage(validSubscriptionRequest), "deliveryAuthentication": delivery})
	request := httptest.NewRequest(stdhttp.MethodPost, DefaultSubscriptionPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", SubscriptionEstablishmentMediaType)
	request.Header.Set("Authorization", authorization)
	return request
}

func establishSubscriptionHTTP(t *testing.T, handler stdhttp.Handler, authorization string, delivery []string) subscriptionHandleWire {
	t.Helper()
	response := serve(handler, newSubscriptionEstablishmentRequest(authorization, delivery))
	if response.Code != stdhttp.StatusCreated {
		t.Fatalf("establishment = %d %v %s", response.Code, response.Header(), response.Body.String())
	}
	var result subscriptionHandleWire
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertSafeSubscriptionURL(t *testing.T, endpoint, cursor string) {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatalf("unsafe delivery endpoint %q: %v", endpoint, err)
	}
	for _, forbidden := range []string{"Bearer", "variables", "document", cursor, validSubscriptionRequest} {
		if forbidden != "" && strings.Contains(endpoint, forbidden) {
			t.Fatalf("delivery endpoint contains %q: %s", forbidden, endpoint)
		}
	}
}

func assertSubscriptionSSE(t *testing.T, response *httptest.ResponseRecorder, events []string) {
	t.Helper()
	if response.Code != stdhttp.StatusOK || response.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("SSE response = %d %v %s", response.Code, response.Header(), response.Body.String())
	}
	body := response.Body.String()
	last := -1
	for _, event := range events {
		index := strings.Index(body, "event: "+event+"\n")
		if index <= last {
			t.Fatalf("missing or unordered %q in %s", event, body)
		}
		last = index
	}
	for _, forbidden := range []string{"Bearer owner", validSubscriptionRequest, `"variables"`} {
		if strings.Contains(response.Header().Get("Location")+response.Header().Get("Link"), forbidden) {
			t.Fatalf("SSE metadata contains %q: %v", forbidden, response.Header())
		}
	}
}
