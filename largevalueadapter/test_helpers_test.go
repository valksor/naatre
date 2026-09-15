package largevalueadapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/largevalue"
	"github.com/valksor/naatre/runtime"
	transporthttp "github.com/valksor/naatre/transport/http"
)

type memoryStore struct {
	mu       sync.Mutex
	records  map[string]largevalue.Record
	content  map[string][]byte
	stages   map[string]*memoryStage
	nextOpen func(string, []byte) largevalue.ReadCloser
}

func newMemoryStore() *memoryStore {
	return &memoryStore{records: make(map[string]largevalue.Record), content: make(map[string][]byte), stages: make(map[string]*memoryStage)}
}

func (s *memoryStore) Create(_ context.Context, record largevalue.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[record.ID]; exists {
		return errors.New("duplicate")
	}
	s.records[record.ID] = record
	return nil
}

func (s *memoryStore) Load(_ context.Context, id string) (largevalue.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[id]
	if !exists {
		return largevalue.Record{}, errors.New("missing")
	}
	return record, nil
}

func (s *memoryStore) Begin(_ context.Context, id string, revision uint64) (largevalue.Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[id]
	if !exists || record.Revision != revision || record.Finalized {
		return nil, errors.New("unavailable")
	}
	stage := &memoryStage{store: s, id: id}
	s.stages[id] = stage
	return stage, nil
}

func (s *memoryStore) Open(_ context.Context, id string) (largevalue.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, exists := s.content[id]
	if !exists {
		return nil, errors.New("missing")
	}
	content = bytes.Clone(content)
	if s.nextOpen != nil {
		return s.nextOpen(id, content), nil
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (s *memoryStore) Claim(_ context.Context, id string, revision uint64) (largevalue.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[id]
	if !exists || record.Revision != revision || record.Consumed {
		return largevalue.Record{}, false, nil
	}
	record.Consumed = true
	record.Revision++
	s.records[id] = record
	return record, true, nil
}

func (s *memoryStore) Revoke(_ context.Context, id string, revision uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[id]
	if !exists || record.Revision != revision {
		return false, nil
	}
	record.Revoked = true
	record.Revision++
	s.records[id] = record
	return true, nil
}

func (s *memoryStore) DeleteExpired(_ context.Context, before time.Time, limit int) (int, error) {
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

type memoryStage struct {
	store        *memoryStore
	id           string
	buffer       bytes.Buffer
	maximumWrite int
	aborted      bool
}

func (s *memoryStage) Write(content []byte) (int, error) {
	if len(content) > s.maximumWrite {
		s.maximumWrite = len(content)
	}
	return s.buffer.Write(content)
}

func (*memoryStage) Close() error { return nil }

func (s *memoryStage) Open(context.Context) (largevalue.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.buffer.Bytes())), nil
}

func (s *memoryStage) Commit(_ context.Context, length int64) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if int64(s.buffer.Len()) != length {
		return errors.New("length")
	}
	record := s.store.records[s.id]
	record.Finalized = true
	record.Revision++
	s.store.records[s.id] = record
	s.store.content[s.id] = bytes.Clone(s.buffer.Bytes())
	return nil
}

func (s *memoryStage) Abort(context.Context) error {
	s.aborted = true
	return nil
}

func testCoordinator(t *testing.T, store *memoryStore, now *time.Time) *largevalue.Coordinator {
	t.Helper()
	next := 0
	coordinator, err := largevalue.NewCoordinator(largevalue.Config{
		Store: store,
		Authorizer: largevalue.AuthorizerFunc(func(context.Context, largevalue.AuthorizationRequest) (bool, error) {
			return true, nil
		}),
		Scanner: largevalue.ScannerFunc(func(_ context.Context, metadata largevalue.Metadata, open func() (largevalue.ReadCloser, error)) (largevalue.ScanResult, error) {
			reader, err := open()
			if err != nil {
				return largevalue.ScanResult{}, err
			}
			_, copyErr := io.Copy(io.Discard, reader)
			return largevalue.ScanResult{Clean: copyErr == nil, DetectedMediaType: metadata.MediaType}, errors.Join(copyErr, reader.Close())
		}),
		Now: func() time.Time { return *now },
		NewID: func() (string, error) {
			next++
			return fmt.Sprintf("object-%d", next), nil
		},
		SigningKey: bytes.Repeat([]byte{9}, 32), MaximumTTL: 24 * time.Hour,
		MaximumBytes: 64 << 20, MaximumEncodedBytes: 64 << 20, MaximumExpansion: 16,
		AllowedMediaTypes: []string{"application/octet-stream"},
		AllowedProfiles: []largevalue.TransferProfile{largevalue.DirectProfile, largevalue.PresignedProfile, largevalue.MultipartProfile,
			largevalue.ResumableProfile, largevalue.ApplicationProfile}, CleanupTimeout: time.Second,
		Egress: &largevalue.EgressPolicy{Resolver: fixedDNSResolver{}, AllowedOrigins: []string{
			"https://upload.example.test:443", "https://objects.example.test:443",
		}, AllowedPorts: []uint16{443}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func testPrincipalContext() context.Context {
	return runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "subject-a", Tenant: "tenant-a", AuthorizationRevision: "auth-1"})
}

func issueCapability(t *testing.T, coordinator *largevalue.Coordinator, direction largevalue.Direction, profile largevalue.TransferProfile, content []byte, methods []string) largevalue.Capability {
	return issueCapabilityRequest(t, coordinator, content, largevalue.IssueRequest{Direction: direction, Profile: profile, Methods: methods})
}

func issuePresignedCapability(t *testing.T, coordinator *largevalue.Coordinator, direction largevalue.Direction, content []byte, transferURL string) largevalue.Capability {
	return issueCapabilityRequest(t, coordinator, content, largevalue.IssueRequest{
		Direction: direction, Profile: largevalue.PresignedProfile, TransferURL: transferURL,
	})
}

func issueCapabilityRequest(t *testing.T, coordinator *largevalue.Coordinator, content []byte, request largevalue.IssueRequest) largevalue.Capability {
	t.Helper()
	digest, err := transporthttp.FormatDigestField(content)
	if err != nil {
		t.Fatal(err)
	}
	request.Binding = largevalue.Binding{Principal: "subject-a", Tenant: "tenant-a", AuthorizationRevision: "auth-1", OperationDigest: "operation-1"}
	request.Metadata = largevalue.Metadata{Slot: "file", Filename: "payload.bin", MediaType: "application/octet-stream", Length: int64(len(content)),
		EncodedLength: int64(len(content)), ContentCoding: "identity", RepresentationDigest: digest}
	request.TTL = time.Hour
	capability, err := coordinator.Issue(testPrincipalContext(), request)
	if err != nil {
		t.Fatal(err)
	}
	if request.Direction == largevalue.Download {
		if err := coordinator.FinalizeDownload(testPrincipalContext(), capability.Reference, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	return capability
}

type fixedDNSResolver struct{}

func (fixedDNSResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("203.0.113.1")}, nil
}

type limitedReader struct {
	reader  io.Reader
	maximum int
}

func (r *limitedReader) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maximum {
		buffer = buffer[:r.maximum]
	}
	return r.reader.Read(buffer)
}
