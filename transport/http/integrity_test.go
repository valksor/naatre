package http_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	transporthttp "github.com/valksor/naatre/transport/http"
)

func TestDigestMiddlewareExactBytesCodingsAndRange(t *testing.T) {
	t.Parallel()
	representation := []byte(`{"message":"Naatre digest vector","version":1}` + "\n")
	compressed := gzipDigestBytes(t, representation)
	completeRange := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	rangeContent := completeRange[10:20]
	multipartContent := []byte("--digest-boundary\r\nContent-Range: bytes 10-19/36\r\n\r\nabcdefghij\r\n--digest-boundary--\r\n")
	rangeDigest, err := transporthttp.FormatDigestField(completeRange, transporthttp.SHA256)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		status         int
		content        []byte
		representation []byte
		encoding       string
		contentRange   string
		contentType    string
		rangeDigest    string
	}{
		{name: "identity", status: http.StatusOK, content: representation, representation: representation, encoding: "identity", contentType: "application/octet-stream"},
		{name: "gzip", status: http.StatusOK, content: compressed, representation: representation, encoding: "gzip", contentType: "application/octet-stream"},
		{name: "range", status: http.StatusPartialContent, content: rangeContent, representation: completeRange, encoding: "identity", contentType: "application/octet-stream", contentRange: "bytes 10-19/36", rangeDigest: rangeDigest},
		{name: "multipart-range", status: http.StatusPartialContent, content: multipartContent, representation: completeRange, encoding: "identity", contentType: "multipart/byteranges; boundary=digest-boundary", rangeDigest: rangeDigest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := transporthttp.CoreDigestMiddlewareConfig()
			config.RequestContent = transporthttp.DigestDisabled
			config.RequestRepresentation = transporthttp.DigestDisabled
			if test.rangeDigest != "" {
				config.RangeRepresentationDigest = func(*http.Request, http.Header) (string, error) {
					return test.rangeDigest, nil
				}
			}
			wrap, err := transporthttp.NewDigestMiddleware(config)
			if err != nil {
				t.Fatal(err)
			}
			handler := wrap(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Encoding", test.encoding)
				writer.Header().Set("Content-Type", test.contentType)
				if test.contentRange != "" {
					writer.Header().Set("Content-Range", test.contentRange)
				}
				writer.WriteHeader(test.status)
				_, _ = writer.Write(test.content)
			}))
			request := httptest.NewRequest(http.MethodGet, "https://example.test/object", nil)
			request.Header.Set("Want-Content-Digest", "sha-256=10")
			request.Header.Set("Want-Repr-Digest", "sha-256=10")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			response := recorder.Result()
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, body = %s", response.StatusCode, recorder.Body.String())
			}
			verifyDigestBytes(t, test.content, response.Header.Values("Content-Digest"))
			verifyDigestBytes(t, test.representation, response.Header.Values("Repr-Digest"))
			if got := strings.Join(response.Header.Values("Vary"), ","); !strings.Contains(got, "Want-Content-Digest") || !strings.Contains(got, "Want-Repr-Digest") {
				t.Fatalf("Vary = %q", got)
			}
		})
	}
}

func TestDigestMiddlewareDoesNotRelabelRangeAsRepresentation(t *testing.T) {
	t.Parallel()
	config := transporthttp.CoreDigestMiddlewareConfig()
	config.RequestContent = transporthttp.DigestDisabled
	config.RequestRepresentation = transporthttp.DigestDisabled
	wrap, err := transporthttp.NewDigestMiddleware(config)
	if err != nil {
		t.Fatal(err)
	}
	handler := wrap(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Range", "bytes 10-19/36")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write([]byte("abcdefghij"))
	}))
	request := httptest.NewRequest(http.MethodGet, "https://example.test/object", nil)
	request.Header.Set("Want-Content-Digest", "sha-256=10")
	request.Header.Set("Want-Repr-Digest", "sha-256=10")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assertDigestProblem(t, recorder, transporthttp.CodeDigestRepresentationUnavailable, transporthttp.DigestPhaseRepresentation)
}

func TestDigestMiddlewareVerifiesRequestCodingsBeforeHandler(t *testing.T) {
	t.Parallel()
	representation := []byte(`{"operation":"upload"}`)
	tests := []struct {
		name, encoding string
		content        []byte
	}{
		{name: "identity", encoding: "identity", content: representation},
		{name: "gzip", encoding: "gzip", content: gzipDigestBytes(t, representation)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := transporthttp.CoreDigestMiddlewareConfig()
			config.ResponseContent = transporthttp.DigestDisabled
			config.ResponseRepresentation = transporthttp.DigestDisabled
			wrap, err := transporthttp.NewDigestMiddleware(config)
			if err != nil {
				t.Fatal(err)
			}
			contentDigest, _ := transporthttp.FormatDigestField(test.content, transporthttp.SHA256)
			reprDigest, _ := transporthttp.FormatDigestField(representation, transporthttp.SHA256)
			var used atomic.Bool
			handler := wrap(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				got, readErr := io.ReadAll(request.Body)
				if readErr != nil {
					t.Errorf("read staged request: %v", readErr)
					return
				}
				if !bytes.Equal(got, test.content) {
					t.Errorf("staged content changed")
					return
				}
				used.Store(true)
				writer.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodPost, "https://example.test/upload", bytes.NewReader(test.content))
			request.Header.Set("Content-Encoding", test.encoding)
			request.Header.Set("Content-Digest", contentDigest)
			request.Header.Set("Repr-Digest", reprDigest)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNoContent || !used.Load() {
				t.Fatalf("status/used = %d/%v body=%s", recorder.Code, used.Load(), recorder.Body.String())
			}
		})
	}
}

func TestDigestMiddlewareRejectsAtStablePhasesBeforeProtectedUse(t *testing.T) {
	t.Parallel()
	const correct = "sha-256=:04s4ot1HbgRcKZ6O5dZGaDRFbZe9WSpxdGtCOmoF84Y=:"
	const conflicting = "sha-256=:04s4ot1HbgRcKZ6O5dZGaDRFbZe9WSpxdGtCOmoF84Y=:, sha-512=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==:"

	tests := []struct {
		name          string
		body          string
		contentDigest []string
		reprDigest    []string
		contentLength int64
		maximum       int64
		trailer       string
		cancel        bool
		code          string
		phase         string
		wantReads     int64
	}{
		{name: "malformed", body: "Wikipedia", contentDigest: []string{"sha-256=bad"}, reprDigest: []string{correct}, contentLength: 9, code: transporthttp.CodeMalformedDigest, phase: transporthttp.DigestPhaseHeaders},
		{name: "late", body: "Wikipedia", trailer: "Content-Digest", contentLength: 9, code: transporthttp.CodeDigestTrailerUnsupported, phase: transporthttp.DigestPhaseHeaders},
		{name: "incomplete", body: "Wiki", contentDigest: []string{correct}, reprDigest: []string{correct}, contentLength: 9, code: transporthttp.CodeDigestTruncated, phase: transporthttp.DigestPhaseContent, wantReads: 4},
		{name: "conflicting", body: "Wikipedia", contentDigest: []string{conflicting}, reprDigest: []string{correct}, contentLength: 9, code: transporthttp.CodeDigestMismatch, phase: transporthttp.DigestPhaseContent, wantReads: 9},
		{name: "intermediary-altered", body: "WikipediA", contentDigest: []string{correct}, reprDigest: []string{correct}, contentLength: 9, code: transporthttp.CodeDigestMismatch, phase: transporthttp.DigestPhaseContent, wantReads: 9},
		{name: "canceled", body: "Wikipedia", contentDigest: []string{correct}, reprDigest: []string{correct}, contentLength: 9, cancel: true, code: transporthttp.CodeDigestCanceled, phase: transporthttp.DigestPhaseContent},
		{name: "limit", body: "Wikipedia", contentDigest: []string{correct}, reprDigest: []string{correct}, contentLength: 9, maximum: 4, code: transporthttp.CodeDigestLimitExceeded, phase: transporthttp.DigestPhaseContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := transporthttp.CoreDigestMiddlewareConfig()
			config.ResponseContent = transporthttp.DigestDisabled
			config.ResponseRepresentation = transporthttp.DigestDisabled
			if test.maximum != 0 {
				config.MaximumRequestContentBytes = test.maximum
			}
			wrap, err := transporthttp.NewDigestMiddleware(config)
			if err != nil {
				t.Fatal(err)
			}
			var called atomic.Bool
			handler := wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
			reader := &countingReader{Reader: strings.NewReader(test.body)}
			request := httptest.NewRequest(http.MethodPost, "https://example.test/v1/execute", io.NopCloser(reader))
			request.ContentLength = test.contentLength
			request.Header["Content-Digest"] = test.contentDigest
			request.Header["Repr-Digest"] = test.reprDigest
			if test.trailer != "" {
				request.Header.Set("Trailer", test.trailer)
			}
			if test.cancel {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			assertDigestProblem(t, recorder, test.code, test.phase)
			if called.Load() {
				t.Fatal("protected handler was called")
			}
			if int64(reader.read) != test.wantReads {
				t.Fatalf("body reads = %d, want %d", reader.read, test.wantReads)
			}
			if strings.Contains(recorder.Body.String(), test.body) || strings.Contains(recorder.Body.String(), "Authorization") {
				t.Fatalf("failure leaked protected input: %s", recorder.Body.String())
			}
		})
	}
}

func TestDigestMiddlewareRejectsDowngradeConflictAndFlushAtCompletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.Handler
		code    string
	}{
		{
			name: "downgrade",
			handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				field, _ := transporthttp.FormatDigestField([]byte("response"), transporthttp.SHA256)
				writer.Header().Set("Content-Digest", field)
				_, _ = writer.Write([]byte("response"))
			}),
			code: transporthttp.CodeDigestDowngrade,
		},
		{
			name: "conflict",
			handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				field, _ := transporthttp.FormatDigestField([]byte("different"), transporthttp.SHA512)
				writer.Header().Set("Content-Digest", field)
				_, _ = writer.Write([]byte("response"))
			}),
			code: transporthttp.CodeDigestMismatch,
		},
		{
			name: "flush",
			handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.(http.Flusher).Flush()
				_, _ = writer.Write([]byte("response"))
			}),
			code: transporthttp.CodeDigestStreamUnsupported,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := transporthttp.CoreDigestMiddlewareConfig()
			config.RequestContent = transporthttp.DigestDisabled
			config.RequestRepresentation = transporthttp.DigestDisabled
			config.ResponseRepresentation = transporthttp.DigestDisabled
			wrap, err := transporthttp.NewDigestMiddleware(config)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "https://example.test/v1/execute", nil)
			request.Header.Set("Want-Content-Digest", "sha-512=10, sha-256=1")
			recorder := httptest.NewRecorder()
			wrap(test.handler).ServeHTTP(recorder, request)
			assertDigestProblem(t, recorder, test.code, transporthttp.DigestPhaseCompletion)
		})
	}
}

func verifyDigestBytes(t *testing.T, content []byte, fields []string) {
	t.Helper()
	if _, err := transporthttp.VerifyDigestTo(io.Discard, bytes.NewReader(content), fields, transporthttp.VerifyOptions{MaximumBytes: int64(len(content)) + 1, ExpectedLength: int64(len(content))}); err != nil {
		t.Fatalf("VerifyDigestTo: %v", err)
	}
}

func gzipDigestBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func assertDigestProblem(t *testing.T, recorder *httptest.ResponseRecorder, code, phase string) {
	t.Helper()
	var problem struct {
		Code  string `json:"code"`
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v; body=%s", err, recorder.Body.String())
	}
	if problem.Code != code || problem.Phase != phase {
		t.Fatalf("problem = %#v, want %s/%s", problem, code, phase)
	}
}
