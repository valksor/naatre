package largevalueadapter

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/largevalue"
)

func TestHTTPPinnedTransportDialsOnlyApprovedAddressWithoutListener(t *testing.T) {
	t.Parallel()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	dialed := make(chan string, 1)
	transport, err := NewHTTPPinnedTransport(HTTPPinnedTransportConfig{
		DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			dialed <- address
			return client, nil
		},
		ResponseHeaderTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestSeen := make(chan error, 1)
	go func() {
		request, readErr := http.ReadRequest(bufio.NewReader(server))
		if readErr == nil && (request.Host != "upload.example.test" || request.URL.Path != "/object") {
			readErr = errors.New("unexpected pinned request")
		}
		if readErr == nil {
			_, readErr = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		}
		requestSeen <- readErr
	}()
	response, err := transport.Do(testPrincipalContext(), PinnedRequest{Method: http.MethodGet, URL: "http://upload.example.test/object",
		ApprovedAddresses: []netip.Addr{netip.MustParseAddr("203.0.113.25")}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if address := <-dialed; address != "203.0.113.25:80" {
		t.Fatalf("dial address = %q", address)
	}
	if err := <-requestSeen; err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
}

func TestPresignedImportPinsEveryRedirectAndStripsCrossOriginCredentials(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	content := bytes.Repeat([]byte("presigned-import-"), 5000)
	const transferURL = "https://upload.example.test/start?signature=first"
	capability := issuePresignedCapability(t, coordinator, largevalue.Upload, content, transferURL)
	firstBody := &observedBody{Reader: strings.NewReader("")}
	secondBody := &observedBody{Reader: &limitedReader{reader: bytes.NewReader(content), maximum: 61}}
	transport := &fakePinnedTransport{responses: []*PinnedResponse{
		{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": []string{"https://objects.example.test/final?signature=second"}}, Body: firstBody},
		{StatusCode: http.StatusOK, Header: http.Header{"Content-Length": []string{"90000"}}, Body: secondBody},
	}}
	resolver := &sequenceDNSResolver{answers: [][]netip.Addr{{netip.MustParseAddr("203.0.113.10")}, {netip.MustParseAddr("203.0.113.11")}}}
	policy := &largevalue.EgressPolicy{Resolver: resolver, AllowedOrigins: []string{"https://upload.example.test:443", "https://objects.example.test:443"}, AllowedPorts: []uint16{443}}
	adapter, err := NewPresignedAdapter(PresignedConfig{Coordinator: coordinator, Transport: transport, Egress: policy, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": []string{"Bearer local-secret"}, "X-Request": []string{"allowed"}}
	if err := adapter.Import(testPrincipalContext(), capability.Reference, transferURL, headers); err != nil {
		t.Fatal(err)
	}
	if len(transport.requests) != 2 || len(transport.requests[0].ApprovedAddresses) != 1 || len(transport.requests[1].ApprovedAddresses) != 1 ||
		transport.requests[0].ApprovedAddresses[0].String() != "203.0.113.10" || transport.requests[1].ApprovedAddresses[0].String() != "203.0.113.11" {
		t.Fatalf("pinned requests = %#v", transport.requests)
	}
	if transport.requests[0].Header.Get("Authorization") == "" || transport.requests[1].Header.Get("Authorization") != "" ||
		transport.requests[1].Header.Get("X-Request") != "allowed" {
		t.Fatalf("redirect headers = %#v / %#v", transport.requests[0].Header, transport.requests[1].Header)
	}
	if !firstBody.closed || !secondBody.closed || !bytes.Equal(store.content["object-1"], content) || store.stages["object-1"].maximumWrite > 32*1024 {
		t.Fatal("presigned response ownership or streaming finalization failed")
	}
}

func TestPresignedImportRejectsDNSRebindingAndRedactsTransportFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	content := bytes.Repeat([]byte("x"), 70<<10)
	const transferURL = "https://upload.example.test/start?credential=secret"
	capability := issuePresignedCapability(t, coordinator, largevalue.Upload, content, transferURL)
	redirectBody := &observedBody{Reader: strings.NewReader("")}
	transport := &fakePinnedTransport{responses: []*PinnedResponse{{StatusCode: http.StatusTemporaryRedirect,
		Header: http.Header{"Location": []string{"https://upload.example.test/final"}}, Body: redirectBody}}}
	resolver := &sequenceDNSResolver{answers: [][]netip.Addr{{netip.MustParseAddr("203.0.113.10")}, {netip.MustParseAddr("169.254.169.254")}}}
	policy := &largevalue.EgressPolicy{Resolver: resolver, AllowedOrigins: []string{"https://upload.example.test:443"}, AllowedPorts: []uint16{443}}
	adapter, err := NewPresignedAdapter(PresignedConfig{Coordinator: coordinator, Transport: transport, Egress: policy, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.Import(testPrincipalContext(), capability.Reference, transferURL, nil)
	if ErrorCode(err) != CodeEgressDenied || strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "169.254") || !redirectBody.closed || store.stages["object-1"] != nil {
		t.Fatalf("DNS rebinding failure = %v, closed=%v", err, redirectBody.closed)
	}

	transport = &fakePinnedTransport{err: errors.New("backend token secret at /private/path")}
	resolver = &sequenceDNSResolver{answers: [][]netip.Addr{{netip.MustParseAddr("203.0.113.10")}}}
	policy = &largevalue.EgressPolicy{Resolver: resolver, AllowedOrigins: []string{"https://upload.example.test:443"}, AllowedPorts: []uint16{443}}
	adapter, _ = NewPresignedAdapter(PresignedConfig{Coordinator: coordinator, Transport: transport, Egress: policy, CleanupTimeout: time.Second})
	err = adapter.Import(testPrincipalContext(), capability.Reference, transferURL, nil)
	if ErrorCode(err) != CodeTransferFailed || strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "private") {
		t.Fatalf("transport failure was not redacted: %v", err)
	}
}

func TestPresignedExportStreamsOnceAndRejectsUploadRedirectReplay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	content := bytes.Repeat([]byte("presigned-export-"), 5000)
	const transferURL = "https://objects.example.test/target"
	capability := issuePresignedCapability(t, coordinator, largevalue.Download, content, transferURL)
	transport := &fakePinnedTransport{readRequestBody: true, responses: []*PinnedResponse{{StatusCode: http.StatusTemporaryRedirect,
		Header: http.Header{"Location": []string{"https://objects.example.test/replay"}}, Body: &observedBody{Reader: strings.NewReader("")}}}}
	resolver := &sequenceDNSResolver{answers: [][]netip.Addr{{netip.MustParseAddr("203.0.113.12")}}}
	policy := &largevalue.EgressPolicy{Resolver: resolver, AllowedOrigins: []string{"https://objects.example.test:443"}, AllowedPorts: []uint16{443}}
	adapter, err := NewPresignedAdapter(PresignedConfig{Coordinator: coordinator, Transport: transport, Egress: policy, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.Export(testPrincipalContext(), capability.Reference, transferURL, http.Header{"Authorization": []string{"Bearer target-secret"}})
	if ErrorCode(err) != CodeUnsupportedCapability || len(transport.requests) != 1 || !bytes.Equal(transport.requestBodies[0], content) ||
		transport.requests[0].Method != http.MethodPut || transport.requests[0].ContentLength != int64(len(content)) || strings.Contains(err.Error(), "target-secret") {
		t.Fatalf("presigned export = %v requests=%#v bodies=%d", err, transport.requests, len(transport.requestBodies))
	}
}

func TestPresignedRejectsUnsignedTransferURLBeforeRequest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	resolver := &sequenceDNSResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("203.0.113.20")},
		{netip.MustParseAddr("203.0.113.21")},
	}}
	transport := &fakePinnedTransport{}
	policy := &largevalue.EgressPolicy{Resolver: resolver, AllowedOrigins: []string{
		"https://upload.example.test:443", "https://objects.example.test:443",
	}, AllowedPorts: []uint16{443}}
	adapter, err := NewPresignedAdapter(PresignedConfig{Coordinator: coordinator, Transport: transport, Egress: policy, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("capability-bound-target")
	upload := issuePresignedCapability(t, coordinator, largevalue.Upload, content, "https://upload.example.test/signed")
	if err := adapter.Import(testPrincipalContext(), upload.Reference, "https://upload.example.test/attacker", nil); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("mismatched import URL error = %v", err)
	}
	download := issuePresignedCapability(t, coordinator, largevalue.Download, content, "https://objects.example.test/signed")
	if err := adapter.Export(testPrincipalContext(), download.Reference, "https://objects.example.test/attacker", nil); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("mismatched export URL error = %v", err)
	}
	if len(transport.requests) != 0 || len(resolver.answers) != 2 {
		t.Fatalf("mismatched URLs reached egress: requests=%d DNS answers remaining=%d", len(transport.requests), len(resolver.answers))
	}
}

type fakePinnedTransport struct {
	mu              sync.Mutex
	responses       []*PinnedResponse
	err             error
	requests        []PinnedRequest
	requestBodies   [][]byte
	readRequestBody bool
}

func (t *fakePinnedTransport) Do(_ context.Context, request PinnedRequest) (*PinnedResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	request.Header = cloneHeaders(request.Header)
	request.ApprovedAddresses = append([]netip.Addr(nil), request.ApprovedAddresses...)
	t.requests = append(t.requests, request)
	if t.readRequestBody && request.Body != nil {
		content, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		t.requestBodies = append(t.requestBodies, content)
	}
	if t.err != nil {
		return nil, t.err
	}
	if len(t.responses) == 0 {
		return nil, errors.New("missing fake response")
	}
	response := t.responses[0]
	t.responses = t.responses[1:]
	return response, nil
}

type observedBody struct {
	io.Reader
	closed bool
}

func (b *observedBody) Close() error {
	b.closed = true
	return nil
}

type sequenceDNSResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
}

func (r *sequenceDNSResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.answers) == 0 {
		return nil, errors.New("DNS answer exhausted")
	}
	answer := r.answers[0]
	r.answers = r.answers[1:]
	return answer, nil
}
