package conformance_test

import (
	"slices"
	"testing"
)

type httpFixture struct {
	Profile          string                `json:"profile"`
	Endpoint         httpEndpoint          `json:"endpoint"`
	MediaTypes       httpMediaTypes        `json:"mediaTypes"`
	MethodCases      []httpMethodCase      `json:"methodCases"`
	NegotiationCases []httpNegotiationCase `json:"negotiationCases"`
	HeaderCases      []httpHeaderCase      `json:"headerCases"`
	StatusCases      []httpStatusCase      `json:"statusCases"`
	EncodingCases    []httpEncodingCase    `json:"encodingCases"`
	DeadlineCases    []httpDeadlineCase    `json:"deadlineCases"`
	CacheCases       []httpCacheCase       `json:"cacheCases"`
	BrowserCases     []httpBrowserCase     `json:"browserCases"`
	ResponseCases    []httpResponseCase    `json:"responseCases"`
	ProtocolVersions []httpProtocolVersion `json:"protocolVersions"`
}

type httpEndpoint struct {
	Path        string `json:"path"`
	Method      string `json:"method"`
	OptionalGET bool   `json:"optionalGet"`
}

type httpMediaTypes struct {
	Request  string `json:"request"`
	Response string `json:"response"`
	Problem  string `json:"problem"`
	Stream   string `json:"stream"`
}

type httpMethodCase struct {
	Name       string `json:"name"`
	Method     string `json:"method"`
	Accepted   bool   `json:"accepted"`
	Status     int    `json:"status"`
	BodyKind   string `json:"bodyKind"`
	PublicCode string `json:"publicCode"`
}

type httpNegotiationCase struct {
	Name       string `json:"name"`
	Header     string `json:"header"`
	Value      string `json:"value"`
	Accepted   bool   `json:"accepted"`
	Status     int    `json:"status"`
	PublicCode string `json:"publicCode"`
}

type httpHeaderCase struct {
	Name        string `json:"name"`
	Header      string `json:"header"`
	Duplicate   bool   `json:"duplicate"`
	Conflicting bool   `json:"conflicting"`
	Accepted    bool   `json:"accepted"`
	Status      int    `json:"status"`
	PublicCode  string `json:"publicCode"`
}

type httpStatusCase struct {
	Name        string `json:"name"`
	Status      int    `json:"status"`
	BodyKind    string `json:"bodyKind"`
	PublicCode  string `json:"publicCode"`
	HeadersSent bool   `json:"headersSent"`
}

type httpEncodingCase struct {
	Name                 string `json:"name"`
	Encoding             string `json:"encoding"`
	Streaming            bool   `json:"streaming"`
	Accepted             bool   `json:"accepted"`
	CompressedLimit      bool   `json:"compressedLimit"`
	DecompressedLimit    bool   `json:"decompressedLimit"`
	RejectBeforeFullRead bool   `json:"rejectBeforeFullRead"`
}

type httpDeadlineCase struct {
	Name                  string `json:"name"`
	Accepted              bool   `json:"accepted"`
	Status                int    `json:"status"`
	BodyKind              string `json:"bodyKind"`
	PublicCode            string `json:"publicCode"`
	CappedByServerMaximum bool   `json:"cappedByServerMaximum"`
	CancelsActiveHandlers bool   `json:"cancelsActiveHandlers"`
}

type httpCacheCase struct {
	Name                    string   `json:"name"`
	Method                  string   `json:"method"`
	Eligible                bool     `json:"eligible"`
	Shared                  bool     `json:"shared"`
	PublicOutput            bool     `json:"publicOutput"`
	AuthorizationInvariant  bool     `json:"authorizationInvariant"`
	TenantSeparated         bool     `json:"tenantSeparated"`
	CanonicalVariableKey    bool     `json:"canonicalVariableKey"`
	SensitiveVariablesInURL bool     `json:"sensitiveVariablesInUrl"`
	Vary                    []string `json:"vary"`
	KeyDimensions           []string `json:"keyDimensions"`
	ETag                    bool     `json:"etag"`
	ConditionalRequests     bool     `json:"conditionalRequests"`
}

type httpBrowserCase struct {
	Name                 string `json:"name"`
	Transport            string `json:"transport"`
	CredentialsInURL     bool   `json:"credentialsInUrl"`
	RequiresPreflight    bool   `json:"requiresPreflight"`
	RequiresCSRFPolicy   bool   `json:"requiresCsrfPolicy"`
	ForwardAuthorization bool   `json:"forwardAuthorization"`
}

type httpResponseCase struct {
	Name             string `json:"name"`
	Non2xx           bool   `json:"non2xx"`
	BodyKind         string `json:"bodyKind"`
	RetainStructured bool   `json:"retainStructured"`
	Truncated        bool   `json:"truncated"`
	PublicCode       string `json:"publicCode"`
}

type httpProtocolVersion struct {
	Name                string `json:"name"`
	EquivalentSemantics bool   `json:"equivalentSemantics"`
	RequiresTrailers    bool   `json:"requiresTrailers"`
}

func TestHTTPBindingFixture(t *testing.T) {
	t.Parallel()
	var fixture httpFixture
	readFixture(t, "http.json", &fixture)
	if fixture.Profile != "core.http-1" || fixture.Endpoint.Path != "/v1/execute" ||
		fixture.Endpoint.Method != "POST" || !fixture.Endpoint.OptionalGET {
		t.Fatalf("HTTP profile or endpoint = %#v", fixture)
	}
	if fixture.MediaTypes.Request != "application/vnd.naatre.request+json;version=1" ||
		fixture.MediaTypes.Response != "application/vnd.naatre.response+json;version=1" ||
		fixture.MediaTypes.Problem != "application/problem+json" ||
		fixture.MediaTypes.Stream != "text/event-stream" {
		t.Fatalf("HTTP media types = %#v", fixture.MediaTypes)
	}
	assertHTTPStatusCases(t, fixture.StatusCases)
	assertHTTPMethodCases(t, fixture.MethodCases)
	assertHTTPNegotiationCases(t, fixture.NegotiationCases)
	assertHTTPHeaderCases(t, fixture.HeaderCases)
	assertHTTPEncodingCases(t, fixture.EncodingCases)
	assertHTTPDeadlineCases(t, fixture.DeadlineCases)
	assertHTTPCacheCases(t, fixture.CacheCases)
	assertHTTPBrowserCases(t, fixture.BrowserCases)
	assertHTTPResponseCases(t, fixture.ResponseCases)
	assertHTTPProtocolVersions(t, fixture.ProtocolVersions)
}

func assertHTTPMethodCases(t *testing.T, cases []httpMethodCase) {
	t.Helper()
	expected := []string{"post", "get-persisted", "get-inline-rejected", "head-rejected", "put-rejected", "options-preflight"}
	assertFixtureCases(t, cases, expected, func(test httpMethodCase) string { return test.Name }, func(test httpMethodCase) {
		if test.Name == "post" && (!test.Accepted || test.Method != "POST" || test.Status != 200) {
			t.Fatalf("POST binding is incomplete: %#v", test)
		}
		if !test.Accepted && (test.Status == 0 || test.PublicCode == "") {
			t.Fatalf("rejected method lacks stable failure: %#v", test)
		}
	})
}

func assertHTTPNegotiationCases(t *testing.T, cases []httpNegotiationCase) {
	t.Helper()
	expected := []string{
		"request-v1", "request-v1-utf8", "request-missing-content-type", "request-generic-json",
		"request-version-2", "request-non-utf8", "accept-absent", "accept-v1", "accept-wildcard",
		"accept-version-2", "accept-v1-q-zero", "accept-event-stream",
	}
	assertFixtureCases(t, cases, expected, func(test httpNegotiationCase) string { return test.Name }, func(test httpNegotiationCase) {
		if !test.Accepted && (test.Status == 0 || test.PublicCode == "") {
			t.Fatalf("rejected negotiation lacks stable failure: %#v", test)
		}
	})
}

func assertHTTPHeaderCases(t *testing.T, cases []httpHeaderCase) {
	t.Helper()
	expected := []string{
		"duplicate-content-type", "conflicting-content-encoding", "duplicate-authorization",
		"duplicate-deadline", "forged-principal", "comma-list-accept",
	}
	assertFixtureCases(t, cases, expected, func(test httpHeaderCase) string { return test.Name }, func(test httpHeaderCase) {
		if !test.Accepted && (test.Status != 400 || test.PublicCode == "") {
			t.Fatalf("unsafe header rejection: %#v", test)
		}
	})
}

func assertHTTPStatusCases(t *testing.T, cases []httpStatusCase) {
	t.Helper()
	expected := []httpStatusCase{
		{Name: "success", Status: 200, BodyKind: "naatre", PublicCode: "OK"},
		{Name: "partial-execution", Status: 200, BodyKind: "naatre", PublicCode: "PARTIAL"},
		{Name: "malformed-json", Status: 400, BodyKind: "problem", PublicCode: "MALFORMED_JSON"},
		{Name: "malformed-header", Status: 400, BodyKind: "problem", PublicCode: "MALFORMED_HEADER"},
		{Name: "get-not-eligible", Status: 400, BodyKind: "problem", PublicCode: "GET_NOT_ELIGIBLE"},
		{Name: "invalid-timeout", Status: 400, BodyKind: "problem", PublicCode: "INVALID_TIMEOUT"},
		{Name: "untrusted-identity-header", Status: 400, BodyKind: "problem", PublicCode: "UNTRUSTED_IDENTITY_HEADER"},
		{Name: "invalid-operation", Status: 422, BodyKind: "naatre", PublicCode: "VALIDATION_FAILED"},
		{Name: "unauthenticated", Status: 401, BodyKind: "problem", PublicCode: "UNAUTHENTICATED"},
		{Name: "forbidden", Status: 403, BodyKind: "naatre", PublicCode: "UNAUTHORIZED"},
		{Name: "method-not-allowed", Status: 405, BodyKind: "problem", PublicCode: "METHOD_NOT_ALLOWED"},
		{Name: "unsupported-media-type", Status: 415, BodyKind: "problem", PublicCode: "UNSUPPORTED_MEDIA_TYPE"},
		{Name: "unsupported-content-encoding", Status: 415, BodyKind: "problem", PublicCode: "UNSUPPORTED_CONTENT_ENCODING"},
		{Name: "not-acceptable", Status: 406, BodyKind: "problem", PublicCode: "NOT_ACCEPTABLE"},
		{Name: "request-timeout", Status: 408, BodyKind: "problem", PublicCode: "REQUEST_TIMEOUT"},
		{Name: "request-too-large", Status: 413, BodyKind: "problem", PublicCode: "REQUEST_TOO_LARGE"},
		{Name: "uri-too-long", Status: 414, BodyKind: "problem", PublicCode: "URI_TOO_LONG"},
		{Name: "rate-limited", Status: 429, BodyKind: "problem", PublicCode: "RATE_LIMITED"},
		{Name: "overloaded-before-decode", Status: 503, BodyKind: "problem", PublicCode: "OVERLOADED"},
		{Name: "execution-deadline", Status: 504, BodyKind: "naatre", PublicCode: "RESOURCE_EXHAUSTED"},
		{Name: "internal-execution", Status: 500, BodyKind: "naatre", PublicCode: "INTERNAL"},
		{Name: "failure-after-headers", BodyKind: "terminate", PublicCode: "TRANSPORT_TERMINATED", HeadersSent: true},
	}
	if !slices.Equal(cases, expected) {
		t.Fatalf("HTTP status cases = %#v, want %#v", cases, expected)
	}
}

func assertHTTPEncodingCases(t *testing.T, cases []httpEncodingCase) {
	t.Helper()
	expected := []string{"identity", "gzip", "gzip-bomb", "unsupported-br", "streaming-identity", "streaming-gzip"}
	assertFixtureCases(t, cases, expected, func(test httpEncodingCase) string { return test.Name }, func(test httpEncodingCase) {
		if test.Accepted && (!test.CompressedLimit || !test.DecompressedLimit) {
			t.Fatalf("accepted encoding lacks both limits: %#v", test)
		}
		if test.Name == "gzip-bomb" && (!test.RejectBeforeFullRead || test.Accepted) {
			t.Fatalf("compression bomb is not bounded: %#v", test)
		}
	})
}

func assertHTTPDeadlineCases(t *testing.T, cases []httpDeadlineCase) {
	t.Helper()
	expected := []string{
		"valid-client-timeout", "malformed-client-timeout", "server-maximum",
		"disconnect-before-headers", "execution-deadline",
	}
	assertFixtureCases(t, cases, expected, func(test httpDeadlineCase) string { return test.Name }, func(test httpDeadlineCase) {
		if test.Name == "server-maximum" && !test.CappedByServerMaximum {
			t.Fatalf("server deadline is not capped: %#v", test)
		}
		if (test.Name == "disconnect-before-headers" || test.Name == "execution-deadline") && !test.CancelsActiveHandlers {
			t.Fatalf("deadline/disconnect does not cancel active handlers: %#v", test)
		}
	})
}

func assertHTTPCacheCases(t *testing.T, cases []httpCacheCase) {
	t.Helper()
	expected := []string{
		"post-default-no-store", "get-inline-rejected", "get-effectful-rejected",
		"get-sensitive-variables-rejected", "get-private-persisted", "shared-public-persisted",
		"shared-auth-variant-rejected", "shared-tenant-isolated",
	}
	assertFixtureCases(t, cases, expected, func(test httpCacheCase) string { return test.Name }, func(test httpCacheCase) {
		if test.Eligible && test.Method == "GET" && !test.CanonicalVariableKey {
			t.Fatalf("GET lacks canonical variable key: %#v", test)
		}
		if test.Shared && (!test.Eligible || !test.PublicOutput || !test.AuthorizationInvariant ||
			!test.TenantSeparated || len(test.Vary) == 0 || len(test.KeyDimensions) == 0 ||
			!test.ETag || !test.ConditionalRequests) {
			t.Fatalf("unsafe shared cache case: %#v", test)
		}
		if test.SensitiveVariablesInURL {
			t.Fatalf("cache case puts sensitive variables in URL: %#v", test)
		}
	})
}

func assertHTTPBrowserCases(t *testing.T, cases []httpBrowserCase) {
	t.Helper()
	expected := []string{"post-stream-fetch", "cors-preflight", "cookie-csrf", "cross-origin-redirect", "history-safe-url"}
	assertFixtureCases(t, cases, expected, func(test httpBrowserCase) string { return test.Name }, func(test httpBrowserCase) {
		if test.CredentialsInURL || (test.Name == "cross-origin-redirect" && test.ForwardAuthorization) {
			t.Fatalf("unsafe browser case: %#v", test)
		}
	})
}

func assertHTTPResponseCases(t *testing.T, cases []httpResponseCase) {
	t.Helper()
	expected := []string{"non-2xx-problem", "non-2xx-naatre", "truncated-problem", "truncated-naatre", "oversized-response"}
	assertFixtureCases(t, cases, expected, func(test httpResponseCase) string { return test.Name }, func(test httpResponseCase) {
		if !test.RetainStructured || test.PublicCode == "" ||
			(test.BodyKind != "problem" && test.BodyKind != "naatre") {
			t.Fatalf("unsafe response decoding case: %#v", test)
		}
	})
}

func assertHTTPProtocolVersions(t *testing.T, versions []httpProtocolVersion) {
	t.Helper()
	assertFixtureCases(t, versions, []string{"HTTP/1.1", "HTTP/2", "HTTP/3"}, func(version httpProtocolVersion) string { return version.Name }, func(version httpProtocolVersion) {
		if !version.EquivalentSemantics || version.RequiresTrailers {
			t.Fatalf("HTTP version is not equivalent: %#v", version)
		}
	})
}

func assertFixtureCases[T any](t *testing.T, cases []T, expected []string, name func(T) string, validate func(T)) {
	t.Helper()
	names := make([]string, 0, len(cases))
	for _, test := range cases {
		names = append(names, name(test))
		validate(test)
	}
	if !slices.Equal(names, expected) {
		t.Fatalf("fixture case names = %v, want %v", names, expected)
	}
}
