package largevalue

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
	transporthttp "github.com/valksor/naatre/transport/http"
)

func TestCoordinatorStreamsAuthorizesAndClaimsSingleUse(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore()
	var events []string
	coordinator := newTestCoordinator(t, store, &now, func(request AuthorizationRequest) bool {
		events = append(events, "authorize-"+string(request.Action))
		return true
	})
	content := bytes.Repeat([]byte("large-payload-"), 100000)
	digest, err := transporthttp.FormatDigestField(content)
	if err != nil {
		t.Fatal(err)
	}
	ctx := principalContext("tenant-a")
	capability, err := coordinator.Issue(ctx, IssueRequest{
		Direction: Upload, Profile: ResumableProfile, Binding: testBinding("tenant-a"),
		Metadata: testMetadata(int64(len(content)), digest), TTL: time.Hour, SingleUse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capability.String(), capability.Reference) || capability.String() != "payload-capability-redacted" {
		t.Fatal("capability formatting exposed bearer material")
	}
	if err := coordinator.AcceptUpload(ctx, capability.Reference, &shortReader{reader: bytes.NewReader(content), maximum: 97}); err != nil {
		t.Fatal(err)
	}
	stage := store.stages["object-1"]
	if stage.maximumWrite > 32*1024 {
		t.Fatalf("staging write = %d bytes, core buffered the complete payload", stage.maximumWrite)
	}
	var consumed []byte
	if err := coordinator.Consume(ctx, capability.Reference, func(_ context.Context, record Record, reader io.Reader) error {
		events = append(events, "handler")
		if !record.Consumed {
			t.Fatal("single-use record was not atomically claimed")
		}
		consumed, err = io.ReadAll(reader)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(consumed, content) {
		t.Fatal("consumed content differs")
	}
	if err := coordinator.Consume(ctx, capability.Reference, func(context.Context, Record, io.Reader) error {
		t.Fatal("single-use handler ran twice")
		return nil
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second consume = %v", err)
	}
	want := []string{"authorize-issue", "authorize-upload", "authorize-upload", "authorize-consume", "handler"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestCoordinatorFailsClosedAndAbortsStaging(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore()
	coordinator := newTestCoordinator(t, store, &now, func(AuthorizationRequest) bool { return true })
	content := []byte("verified representation")
	digest, _ := transporthttp.FormatDigestField(content)
	capability, err := coordinator.Issue(principalContext("tenant-a"), IssueRequest{
		Direction: Upload, Profile: DirectProfile, Binding: testBinding("tenant-a"), Metadata: testMetadata(int64(len(content)), digest), TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.AcceptUpload(principalContext("tenant-a"), capability.Reference, bytes.NewReader([]byte("modified representation"))); !errors.Is(err, ErrTransferFailed) {
		t.Fatalf("digest mismatch = %v", err)
	}
	if !store.stages["object-1"].aborted || store.records["object-1"].Finalized {
		t.Fatal("failed transfer did not retain coordinator cleanup ownership")
	}
	if err := coordinator.Consume(principalContext("tenant-a"), capability.Reference, func(context.Context, Record, io.Reader) error {
		t.Fatal("digest-mismatched content reached a handler")
		return nil
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("consume after digest mismatch = %v", err)
	}
	tampered := capability.Reference[:len(capability.Reference)-1] + "A"
	if err := coordinator.Consume(principalContext("tenant-a"), tampered, func(context.Context, Record, io.Reader) error {
		t.Fatal("tampered capability reached a handler")
		return nil
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("tampered capability = %v", err)
	}

	checks := []struct {
		name    string
		prepare func()
		ctx     context.Context
	}{
		{name: "wrong-tenant", ctx: principalContext("tenant-b")},
		{name: "expired", prepare: func() { now = now.Add(2 * time.Minute) }, ctx: principalContext("tenant-a")},
	}
	for _, test := range checks {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				test.prepare()
			}
			if err := coordinator.Consume(test.ctx, capability.Reference, func(context.Context, Record, io.Reader) error {
				t.Fatal("unauthorized handler ran")
				return nil
			}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("consume = %v", err)
			}
		})
	}
}

func TestCoordinatorCancellationAbortsStaging(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore()
	coordinator := newTestCoordinator(t, store, &now, func(AuthorizationRequest) bool { return true })
	content := bytes.Repeat([]byte("cancel"), 20000)
	digest, _ := transporthttp.FormatDigestField(content)
	capability, err := coordinator.Issue(principalContext("tenant-a"), IssueRequest{
		Direction: Upload, Profile: DirectProfile, Binding: testBinding("tenant-a"), Metadata: testMetadata(int64(len(content)), digest), TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.AcceptUpload(principalContext("tenant-a"), capability.Reference, failingReader{err: context.Canceled}); !errors.Is(err, ErrTransferFailed) {
		t.Fatalf("cancelled transfer = %v", err)
	}
	if !store.stages["object-1"].aborted {
		t.Fatal("cancelled transfer did not abort staging")
	}
}

func TestCoordinatorRevocationAndCurrentAuthorization(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore()
	allowed := true
	coordinator := newTestCoordinator(t, store, &now, func(AuthorizationRequest) bool { return allowed })
	content := []byte("download")
	digest, _ := transporthttp.FormatDigestField(content)
	capability, err := coordinator.Issue(principalContext("tenant-a"), IssueRequest{
		Direction: Download, Profile: PresignedProfile, Binding: testBinding("tenant-a"), Metadata: testMetadata(int64(len(content)), digest), TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.FinalizeDownload(principalContext("tenant-a"), capability.Reference, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	allowed = false
	if err := coordinator.Consume(principalContext("tenant-a"), capability.Reference, func(context.Context, Record, io.Reader) error {
		t.Fatal("handler ran after authorization revocation")
		return nil
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("authorization denial = %v", err)
	}
	allowed = true
	record := store.records["object-1"]
	record.Metadata.RepresentationDigest = "sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:"
	store.records["object-1"] = record
	if err := coordinator.Consume(principalContext("tenant-a"), capability.Reference, func(context.Context, Record, io.Reader) error {
		t.Fatal("handler ran after bound metadata substitution")
		return nil
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("metadata substitution = %v", err)
	}
	record.Metadata = testMetadata(int64(len(content)), digest)
	store.records["object-1"] = record
	if err := coordinator.Revoke(principalContext("tenant-a"), capability.Reference); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Consume(principalContext("tenant-a"), capability.Reference, func(context.Context, Record, io.Reader) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("revoked consume = %v", err)
	}
}

func TestMetadataSecurityPolicy(t *testing.T) {
	t.Parallel()
	config := Config{MaximumBytes: 8 << 20, MaximumEncodedBytes: 1 << 20, MaximumExpansion: 8, AllowedMediaTypes: []string{"application/octet-stream", "image/png"}}
	digest, _ := transporthttp.FormatDigestField([]byte("x"))
	valid := Metadata{Slot: "asset", Filename: "photo.png", MediaType: "image/png", Length: 1, EncodedLength: 1, ContentCoding: "identity", RepresentationDigest: digest}
	tests := []struct {
		name   string
		mutate func(*Metadata)
	}{
		{name: "path-traversal", mutate: func(value *Metadata) { value.Filename = "../secret" }},
		{name: "windows-path", mutate: func(value *Metadata) { value.Filename = `C:\secret` }},
		{name: "media-type-confusion", mutate: func(value *Metadata) { value.MediaType = "text/html" }},
		{name: "negative-length", mutate: func(value *Metadata) { value.Length = -1 }},
		{name: "length-limit", mutate: func(value *Metadata) { value.Length = config.MaximumBytes + 1 }},
		{name: "compression-bomb", mutate: func(value *Metadata) { value.ContentCoding = "gzip"; value.Length = 100; value.EncodedLength = 1 }},
		{name: "unknown-coding", mutate: func(value *Metadata) { value.ContentCoding = "br" }},
		{name: "missing-digest", mutate: func(value *Metadata) { value.RepresentationDigest = "" }},
		{name: "malformed-digest", mutate: func(value *Metadata) { value.RepresentationDigest = "sha-256=not-a-byte-sequence" }},
	}
	if err := validateMetadata(valid, config); err != nil {
		t.Fatalf("valid metadata = %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := valid
			test.mutate(&value)
			if err := validateMetadata(value, config); !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestEgressPolicyRejectsRedirectsAndDNSRebinding(t *testing.T) {
	t.Parallel()
	resolver := &sequenceResolver{answers: [][]netip.Addr{{netip.MustParseAddr("203.0.113.10")}, {netip.MustParseAddr("169.254.169.254")}}}
	policy := EgressPolicy{Resolver: resolver, AllowedOrigins: []string{"https://upload.example.test:443"}, AllowedPorts: []uint16{443}, MaximumRedirects: 2}
	if _, err := policy.ValidateHop(context.Background(), "https://upload.example.test/object"); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ValidateHop(context.Background(), "https://upload.example.test/object"); !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("DNS rebinding = %v", err)
	}
	malicious := []string{
		"http://upload.example.test/object",
		"https://user:secret@upload.example.test/object",
		"https://127.0.0.1/object",
		"https://169.254.169.254/latest/meta-data",
		"https://metadata.google.internal/computeMetadata/v1",
		"file:///etc/passwd",
		"https://other.example.test/object",
	}
	for _, rawURL := range malicious {
		if _, err := policy.ValidateHop(context.Background(), rawURL); !errors.Is(err, ErrEgressDenied) {
			t.Errorf("URL %q = %v", rawURL, err)
		}
	}
	if err := policy.ValidateRedirectChain(context.Background(), []string{"https://upload.example.test/a", "https://169.254.169.254/b"}); !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("redirect chain = %v", err)
	}
}

func TestRangeResumeAndPayloadIdentities(t *testing.T) {
	t.Parallel()
	rangeValue, err := ParseByteRange("bytes=10-19", 36)
	if err != nil || rangeValue.ContentRange() != "bytes 10-19/36" || rangeValue.Length() != 10 {
		t.Fatalf("range = %#v, %v", rangeValue, err)
	}
	for _, invalid := range []string{"bytes=20-10", "bytes=0-1,4-5", "items=0-1", "bytes=99-"} {
		if _, err := ParseByteRange(invalid, 36); !errors.Is(err, ErrRangeUnsatisfied) {
			t.Fatalf("range %q = %v", invalid, err)
		}
	}
	if err := ValidateResume(ResumeRequest{Offset: 10, ChunkLength: 5, TotalLength: 20, Validator: "etag-1", ExpectedValidator: "etag-1"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResume(ResumeRequest{Offset: 10, ChunkLength: 5, TotalLength: 20, Validator: "etag-2", ExpectedValidator: "etag-1"}); !errors.Is(err, ErrRangeUnsatisfied) {
		t.Fatalf("resume validator = %v", err)
	}
	decision, err := EvaluateDownloadRequest(DownloadRequest{Method: "GET", Range: "bytes=10-19", IfRange: `"v1"`, Length: 36, ETag: `"v1"`})
	if err != nil || decision.Status != 206 || decision.ContentLength != 10 || decision.Range.ContentRange() != "bytes 10-19/36" || decision.FollowRedirect {
		t.Fatalf("download decision = %#v, %v", decision, err)
	}
	notModified, err := EvaluateDownloadRequest(DownloadRequest{Method: "GET", IfNoneMatch: `"v1"`, Length: 36, ETag: `"v1"`})
	if err != nil || notModified.Status != 304 || notModified.ContentLength != 0 || notModified.CacheControl != "private, no-store" {
		t.Fatalf("conditional decision = %#v, %v", notModified, err)
	}

	document := []byte(`{"kind":"mutation","name":"CreateAsset"}`)
	slots := []SlotDeclaration{{Name: "file", Direction: Upload, Required: true, Profile: ResumableProfile, AllowedMediaTypes: []string{"image/png"}, MaximumLength: 8 << 20}}
	operation, err := OperationIdentity(document, slots)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := OperationIdentity(document, slots)
	if operation != same {
		t.Fatal("operation identity was not deterministic")
	}
	left, _ := ContentIdentity(operation, []ResolvedReference{{Slot: "file", Reference: "object-a", MediaType: "image/png", Length: 10, Digest: "sha-256=:left=:"}})
	right, _ := ContentIdentity(operation, []ResolvedReference{{Slot: "file", Reference: "object-b", MediaType: "image/png", Length: 10, Digest: "sha-256=:right=:"}})
	if left == right {
		t.Fatal("different payload bytes shared an idempotency identity")
	}
}

func newTestCoordinator(t *testing.T, store *testStore, now *time.Time, authorize func(AuthorizationRequest) bool) *Coordinator {
	t.Helper()
	coordinator, err := NewCoordinator(Config{
		Store: store, Authorizer: AuthorizerFunc(func(_ context.Context, request AuthorizationRequest) (bool, error) { return authorize(request), nil }),
		Scanner: ScannerFunc(func(_ context.Context, metadata Metadata, open func() (ReadCloser, error)) (ScanResult, error) {
			reader, err := open()
			if err != nil {
				return ScanResult{}, err
			}
			_, err = io.Copy(io.Discard, reader)
			closeErr := reader.Close()
			return ScanResult{Clean: err == nil && closeErr == nil, DetectedMediaType: metadata.MediaType}, errors.Join(err, closeErr)
		}),
		Now: func() time.Time { return *now }, NewID: func() (string, error) { return "object-1", nil }, SigningKey: bytes.Repeat([]byte{7}, 32),
		MaximumTTL: 24 * time.Hour, MaximumBytes: 8 << 20, MaximumEncodedBytes: 8 << 20, MaximumExpansion: 16,
		AllowedMediaTypes: []string{"application/octet-stream"}, AllowedProfiles: []TransferProfile{DirectProfile, PresignedProfile, ResumableProfile}, CleanupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func principalContext(tenant string) context.Context {
	return runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "subject-a", Tenant: tenant, AuthorizationRevision: "auth-1"})
}

func testBinding(tenant string) Binding {
	return Binding{Principal: "subject-a", Tenant: tenant, AuthorizationRevision: "auth-1", OperationDigest: "operation-1"}
}

func testMetadata(length int64, digest string) Metadata {
	return Metadata{Slot: "file", Filename: "asset.bin", MediaType: "application/octet-stream", Length: length,
		EncodedLength: length, ContentCoding: "identity", RepresentationDigest: digest}
}

type shortReader struct {
	reader  io.Reader
	maximum int
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func (r *shortReader) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maximum {
		buffer = buffer[:r.maximum]
	}
	return r.reader.Read(buffer)
}

type sequenceResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
}

func (r *sequenceResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.answers) == 0 {
		return nil, errors.New("no DNS answer")
	}
	answer := r.answers[0]
	r.answers = r.answers[1:]
	return answer, nil
}

type testStore struct {
	mu      sync.Mutex
	records map[string]Record
	stages  map[string]*testStage
	content map[string][]byte
}

func newTestStore() *testStore {
	return &testStore{records: make(map[string]Record), stages: make(map[string]*testStage), content: make(map[string][]byte)}
}

func (s *testStore) Create(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, present := s.records[record.ID]; present {
		return errors.New("duplicate")
	}
	s.records[record.ID] = record
	return nil
}

func (s *testStore) Load(_ context.Context, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, present := s.records[id]
	if !present {
		return Record{}, errors.New("not found")
	}
	return record, nil
}

func (s *testStore) Begin(_ context.Context, id string, revision uint64) (Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, present := s.records[id]
	if !present || record.Revision != revision || record.Finalized {
		return nil, errors.New("unavailable")
	}
	stage := &testStage{store: s, id: id}
	s.stages[id] = stage
	return stage, nil
}

func (s *testStore) Open(_ context.Context, id string) (ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, present := s.content[id]
	if !present {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), content...))), nil
}

func (s *testStore) Claim(_ context.Context, id string, revision uint64) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, present := s.records[id]
	if !present || record.Revision != revision || record.Consumed {
		return Record{}, false, nil
	}
	record.Consumed = true
	record.Revision++
	s.records[id] = record
	return record, true, nil
}

func (s *testStore) Revoke(_ context.Context, id string, revision uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, present := s.records[id]
	if !present || record.Revision != revision {
		return false, nil
	}
	record.Revoked = true
	record.Revision++
	s.records[id] = record
	return true, nil
}

func (s *testStore) DeleteExpired(_ context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for id, record := range s.records {
		if deleted == limit {
			break
		}
		if !record.ExpiresAt.After(before) {
			delete(s.records, id)
			delete(s.content, id)
			deleted++
		}
	}
	return deleted, nil
}

type testStage struct {
	store        *testStore
	id           string
	buffer       bytes.Buffer
	maximumWrite int
	aborted      bool
}

func (s *testStage) Write(content []byte) (int, error) {
	if len(content) > s.maximumWrite {
		s.maximumWrite = len(content)
	}
	return s.buffer.Write(content)
}

func (*testStage) Close() error { return nil }

func (s *testStage) Open(context.Context) (ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.buffer.Bytes())), nil
}

func (s *testStage) Commit(_ context.Context, length int64) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if int64(s.buffer.Len()) != length {
		return errors.New("length mismatch")
	}
	record := s.store.records[s.id]
	record.Finalized = true
	record.Revision++
	s.store.records[s.id] = record
	s.store.content[s.id] = append([]byte(nil), s.buffer.Bytes()...)
	return nil
}

func (s *testStage) Abort(context.Context) error {
	s.aborted = true
	return nil
}
