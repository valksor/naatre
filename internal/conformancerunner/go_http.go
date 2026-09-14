package conformancerunner

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	transporthttp "github.com/valksor/naatre/transport/http"
)

const coreHTTPProfile = "core.http-1"

type goHTTPFixture struct {
	Profile        string `json:"profile"`
	FixtureSuite   string `json:"fixtureSuite"`
	Implementation struct {
		Module       string         `json:"module"`
		MinimumGo    string         `json:"minimumGo"`
		Dependencies []evidenceFile `json:"dependencies"`
		Files        []evidenceFile `json:"files"`
	} `json:"implementation"`
	Binding struct {
		Handler                  string `json:"handler"`
		Path                     string `json:"path"`
		Method                   string `json:"method"`
		RequestMediaType         string `json:"requestMediaType"`
		ResponseMediaType        string `json:"responseMediaType"`
		ProblemMediaType         string `json:"problemMediaType"`
		RequestCompressedBytes   int64  `json:"requestCompressedBytes"`
		RequestDecompressedBytes int64  `json:"requestDecompressedBytes"`
		ResponseBytes            int64  `json:"responseBytes"`
		ResponseCompressedBytes  int64  `json:"responseCompressedBytes"`
		Cancellation             string `json:"cancellation"`
		CacheDefault             string `json:"cacheDefault"`
		Redirects                string `json:"redirects"`
	} `json:"binding"`
	Vectors     []string          `json:"vectors"`
	Commands    map[string]string `json:"commands"`
	Unsupported []string          `json:"unsupported"`
}

func (r *Runner) verifyGoHTTP(ctx context.Context, _ Request) Result {
	fixture, fixtureEvidence, err := r.loadGoHTTPFixture()
	if err != nil {
		return failureResult(coreHTTPProfile, "GO_HTTP_FIXTURE_INVALID", "v1/go-http.json")
	}
	evidence, _, err := r.verifyEvidenceFiles(append(slices.Clone(fixture.Implementation.Dependencies), fixture.Implementation.Files...))
	if err != nil {
		return failureResult(coreHTTPProfile, "GO_HTTP_EVIDENCE_MISMATCH", "v1/go-http.json")
	}
	if err := verifyGoHTTPBinding(ctx, fixture); err != nil {
		return failureResult(coreHTTPProfile, "GO_HTTP_BINDING_FAILED", "v1/go-http.json")
	}
	result := emptyResult(coreHTTPProfile, "passed", "")
	result.Capabilities = []string{coreHTTPProfile}
	result.Evidence = append([]Evidence{fixtureEvidence}, evidence...)
	return result
}

func (r *Runner) loadGoHTTPFixture() (goHTTPFixture, Evidence, error) {
	var fixture goHTTPFixture
	_, evidence, err := r.loadPinnedFixture("v1/go-http.json", &fixture)
	if err != nil {
		return goHTTPFixture{}, Evidence{}, err
	}
	expectedVectors := []string{
		"status-media-encoding", "compressed-size", "slow-client", "disconnect", "deadline",
		"cache-isolation", "cors", "redirect", "shutdown", "problem-redaction",
		"http-1.1-semantics", "http-2-semantics", "http-3-semantics",
	}
	limits := transporthttp.DefaultLimits()
	if fixture.Profile != coreHTTPProfile || fixture.FixtureSuite != r.manifest.FixtureVersion ||
		fixture.Implementation.Module != "github.com/valksor/naatre/transport/http" || fixture.Implementation.MinimumGo != "1.27" ||
		fixture.Binding.Handler != "net/http.Handler" || fixture.Binding.Path != transporthttp.DefaultPath || fixture.Binding.Method != http.MethodPost ||
		fixture.Binding.RequestMediaType != transporthttp.RequestMediaType || fixture.Binding.ResponseMediaType != transporthttp.ResponseMediaType || fixture.Binding.ProblemMediaType != transporthttp.ProblemMediaType ||
		fixture.Binding.RequestCompressedBytes != limits.MaxCompressedRequestBytes || fixture.Binding.RequestDecompressedBytes != limits.MaxDecompressedRequestBytes ||
		fixture.Binding.ResponseBytes != limits.MaxResponseBytes || fixture.Binding.ResponseCompressedBytes != limits.MaxCompressedResponseBytes ||
		fixture.Binding.Cancellation != "active-context-and-body-close" || fixture.Binding.CacheDefault != "no-store" || fixture.Binding.Redirects != "never" ||
		!slices.Equal(fixture.Vectors, expectedVectors) || len(fixture.Commands) != 3 || len(fixture.Unsupported) != 11 {
		return goHTTPFixture{}, Evidence{}, errors.New("incompatible Go HTTP fixture")
	}
	return fixture, evidence, nil
}

func verifyGoHTTPBinding(ctx context.Context, fixture goHTTPFixture) error {
	handler, err := transporthttp.NewHandler(transporthttp.Config{
		RequestID: func() string { return "conformance-request" },
		CORS:      transporthttp.CORS{AllowedOrigins: []string{"https://conformance.example"}},
		Executor: func(context.Context, *protocol.Request) runtime.Outcome {
			return runtime.Outcome{Data: map[string]any{"ok": true}}
		},
	})
	if err != nil {
		return err
	}
	requestBody := `{"version":"1","document":{"operations":[{"name":"Ping","kind":"query","select":[{"$call":{"name":"ping"}}]}]}}`
	for major, proto := range map[int]string{1: "HTTP/1.1", 2: "HTTP/2.0", 3: "HTTP/3.0"} {
		request := httptest.NewRequest(http.MethodPost, fixture.Binding.Path, strings.NewReader(requestBody)).WithContext(ctx)
		request.Header.Set("Content-Type", fixture.Binding.RequestMediaType)
		request.Proto, request.ProtoMajor = proto, major
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != fixture.Binding.ResponseMediaType || response.Header().Get("Cache-Control") != fixture.Binding.CacheDefault || !strings.Contains(response.Body.String(), `"requestId":"conformance-request"`) {
			return errors.New("HTTP version semantics mismatch")
		}
	}

	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	_, _ = gzipWriter.Write([]byte(requestBody))
	_ = gzipWriter.Close()
	request := httptest.NewRequest(http.MethodPost, fixture.Binding.Path, &compressed).WithContext(ctx)
	request.Header.Set("Content-Type", fixture.Binding.RequestMediaType)
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		return errors.New("gzip request failed")
	}

	preflight := httptest.NewRequest(http.MethodOptions, fixture.Binding.Path, nil).WithContext(ctx)
	preflight.Header.Set("Origin", "https://conformance.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, preflight)
	if response.Code != http.StatusNoContent || response.Header().Get("Access-Control-Allow-Origin") == "" {
		return errors.New("CORS preflight failed")
	}

	request = httptest.NewRequest(http.MethodGet, fixture.Binding.Path, nil).WithContext(ctx)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Location") != "" {
		return errors.New("redirect policy mismatch")
	}
	return nil
}
