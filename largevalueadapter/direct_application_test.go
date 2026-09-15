package largevalueadapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/largevalue"
)

func TestDirectHandlerStreamsUploadAndConditionalRangeDownload(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	handler, err := NewDirectHandler(DirectConfig{Coordinator: coordinator})
	if err != nil {
		t.Fatal(err)
	}

	uploadContent := bytes.Repeat([]byte("streamed-upload-"), 9000)
	upload := issueCapability(t, coordinator, largevalue.Upload, largevalue.DirectProfile, uploadContent, []string{http.MethodPut})
	request := httptest.NewRequest(http.MethodPut, "https://api.example.test/payload", &limitedReader{reader: bytes.NewReader(uploadContent), maximum: 73})
	request = request.WithContext(testPrincipalContext())
	request.Header.Set(defaultCapabilityHeader, upload.Reference)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}
	stage := store.stages["object-1"]
	if stage == nil || stage.maximumWrite > 32*1024 || !bytes.Equal(store.content["object-1"], uploadContent) {
		t.Fatalf("upload was not streamed and finalized: stage=%#v", stage)
	}

	downloadContent := bytes.Repeat([]byte("download-range-"), 9000)
	download := issueCapability(t, coordinator, largevalue.Download, largevalue.DirectProfile, downloadContent, nil)
	rangeRequest := httptest.NewRequest(http.MethodGet, "https://api.example.test/payload", nil).WithContext(testPrincipalContext())
	rangeRequest.Header.Set(defaultCapabilityHeader, download.Reference)
	rangeRequest.Header.Set("Range", "bytes=17-116")
	rangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Header().Get("Content-Range") != "bytes 17-116/135000" ||
		rangeResponse.Header().Get("Cache-Control") != "private, no-store" || !bytes.Equal(rangeResponse.Body.Bytes(), downloadContent[17:117]) {
		t.Fatalf("range response = %d %#v %q", rangeResponse.Code, rangeResponse.Header(), rangeResponse.Body.Bytes())
	}
	etag := rangeResponse.Header().Get("ETag")
	notModified := httptest.NewRequest(http.MethodGet, "https://api.example.test/payload", nil).WithContext(testPrincipalContext())
	notModified.Header.Set(defaultCapabilityHeader, download.Reference)
	notModified.Header.Set("If-None-Match", etag)
	notModifiedResponse := httptest.NewRecorder()
	handler.ServeHTTP(notModifiedResponse, notModified)
	if notModifiedResponse.Code != http.StatusNotModified || notModifiedResponse.Body.Len() != 0 {
		t.Fatalf("conditional response = %d %q", notModifiedResponse.Code, notModifiedResponse.Body.String())
	}
}

func TestDirectHandlerRejectsMethodsLimitsAndSecretsWithStableCodes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	limits := DefaultLimits()
	limits.MaximumTransferBytes = 128 << 10
	limits.MaximumChunkBytes = 64 << 10
	handler, err := NewDirectHandler(DirectConfig{Coordinator: coordinator, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("x"), 70<<10)
	capability := issueCapability(t, coordinator, largevalue.Upload, largevalue.DirectProfile, content, []string{http.MethodPost})
	request := httptest.NewRequest(http.MethodPut, "https://api.example.test/private/object", bytes.NewReader(content)).WithContext(testPrincipalContext())
	request.Header.Set(defaultCapabilityHeader, capability.Reference)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), CodeUnavailable) ||
		strings.Contains(response.Body.String(), capability.Reference) || store.stages["object-1"] != nil {
		t.Fatalf("method rejection = %d %q", response.Code, response.Body.String())
	}

	oversized := httptest.NewRequest(http.MethodPost, "https://api.example.test/private/object", bytes.NewReader(content)).WithContext(testPrincipalContext())
	oversized.ContentLength = limits.MaximumTransferBytes + 1
	oversized.Header.Set(defaultCapabilityHeader, capability.Reference)
	oversizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(oversizedResponse, oversized)
	if oversizedResponse.Code != http.StatusRequestEntityTooLarge || !strings.Contains(oversizedResponse.Body.String(), CodeResourceExhausted) ||
		strings.Contains(oversizedResponse.Body.String(), "private/object") {
		t.Fatalf("limit rejection = %d %q", oversizedResponse.Code, oversizedResponse.Body.String())
	}
}

func TestApplicationAdapterCommitsOrAbortsOwnedSink(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	provider := &testApplicationProvider{sources: map[string][]byte{}}
	adapter, err := NewApplicationAdapter(ApplicationConfig{Coordinator: coordinator, Provider: provider, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	uploadContent := bytes.Repeat([]byte("application-source-"), 5000)
	provider.sources["source-a"] = uploadContent
	upload := issueCapability(t, coordinator, largevalue.Upload, largevalue.ApplicationProfile, uploadContent, nil)
	if err := adapter.Import(testPrincipalContext(), upload.Reference, "source-a"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(store.content["object-1"], uploadContent) || !provider.sourceClosed {
		t.Fatal("application source was not streamed, verified, and closed")
	}

	downloadContent := bytes.Repeat([]byte("application-sink-"), 5000)
	download := issueCapability(t, coordinator, largevalue.Download, largevalue.ApplicationProfile, downloadContent, nil)
	if err := adapter.Export(testPrincipalContext(), download.Reference, "sink-a"); err != nil {
		t.Fatal(err)
	}
	if provider.sink == nil || !provider.sink.committed || provider.sink.aborted || !bytes.Equal(provider.sink.Bytes(), downloadContent) {
		t.Fatalf("application sink lifecycle = %#v", provider.sink)
	}

	cancelledDownload := issueCapability(t, coordinator, largevalue.Download, largevalue.ApplicationProfile, downloadContent, nil)
	provider.cancelWrites = true
	ctx, cancel := context.WithCancel(testPrincipalContext())
	provider.cancel = cancel
	err = adapter.Export(ctx, cancelledDownload.Reference, "sink-b")
	if !errors.Is(err, context.Canceled) || ErrorCode(err) != CodeCancelled || provider.sink == nil || !provider.sink.aborted || provider.sink.committed {
		t.Fatalf("cancelled sink = %v, %#v", err, provider.sink)
	}
	if strings.Contains(err.Error(), "sink-b") {
		t.Fatalf("public failure exposed protected identifier: %v", err)
	}
}

type testApplicationProvider struct {
	sources      map[string][]byte
	sourceClosed bool
	sink         *testApplicationSink
	cancelWrites bool
	cancel       context.CancelFunc
}

func (p *testApplicationProvider) OpenSource(_ context.Context, id string) (io.ReadCloser, error) {
	content, ok := p.sources[id]
	if !ok {
		return nil, errors.New("backend secret: missing source")
	}
	return &closeObserver{Reader: bytes.NewReader(content), closed: &p.sourceClosed}, nil
}

func (p *testApplicationProvider) OpenSink(context.Context, string) (ApplicationSink, error) {
	p.sink = &testApplicationSink{cancelWrites: p.cancelWrites, cancel: p.cancel}
	return p.sink, nil
}

type closeObserver struct {
	io.Reader
	closed *bool
}

func (r *closeObserver) Close() error {
	*r.closed = true
	return nil
}

type testApplicationSink struct {
	bytes.Buffer
	committed    bool
	aborted      bool
	cancelWrites bool
	cancel       context.CancelFunc
}

func (s *testApplicationSink) Write(content []byte) (int, error) {
	if s.cancelWrites {
		if s.cancel != nil {
			s.cancel()
		}
		return 0, context.Canceled
	}
	return s.Buffer.Write(content)
}

func (*testApplicationSink) Close() error { return nil }

func (s *testApplicationSink) Commit(context.Context) error {
	s.committed = true
	return nil
}

func (s *testApplicationSink) Abort(context.Context) error {
	s.aborted = true
	return nil
}
