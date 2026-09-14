package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/valksor/naatre/schema"
)

var (
	// ErrNoDiscoveryRevision indicates that no current revision is installed.
	ErrNoDiscoveryRevision = &DiscoveryError{code: CodeDiscoveryRevisionMissing, message: "no discovery revision installed"}
	// ErrDiscoveryRevisionConflict rejects different immutable documents that
	// claim the same revision identity.
	ErrDiscoveryRevisionConflict = &DiscoveryError{code: CodeDiscoveryRevisionConflict, message: "discovery revision conflicts with retained document"}
	// ErrDiscoveryRevisionNotFound indicates that a rollback target was evicted
	// or was never installed.
	ErrDiscoveryRevisionNotFound = &DiscoveryError{code: CodeDiscoveryRevisionMissing, message: "discovery revision not found"}
)

type retainedDiscoveryRevision struct {
	document  schema.Document
	canonical []byte
}

// RevisionStore is a finite, race-safe source for rolling schema discovery.
// It retains at most capacity immutable documents, including the current one.
// Applications install the same validated schema revision here that they
// install into their runtime process controller.
type RevisionStore struct {
	mu        sync.Mutex
	capacity  int
	current   string
	order     []string
	revisions map[string]retainedDiscoveryRevision
}

// NewRevisionStore creates an empty store with finite history capacity.
func NewRevisionStore(capacity int) (*RevisionStore, error) {
	if capacity < 1 {
		return nil, errors.New("discovery revision capacity must be positive")
	}
	return &RevisionStore{capacity: capacity, revisions: make(map[string]retainedDiscoveryRevision, capacity)}, nil
}

// Install validates and atomically selects a document. Reinstalling identical
// bytes is idempotent; reusing an existing revision for other bytes fails.
func (s *RevisionStore) Install(document schema.Document) error {
	canonical, err := document.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("canonicalize discovery revision: %w", err)
	}
	revision := document.Revision()
	if !validDiscoveryRevision(revision, DefaultDiscoveryLimits().MaxRevisionBytes) {
		return errors.New("discovery document has invalid revision")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if retained, found := s.revisions[revision]; found {
		if !bytes.Equal(retained.canonical, canonical) {
			return ErrDiscoveryRevisionConflict
		}
		s.current = revision
		return nil
	}
	s.revisions[revision] = retainedDiscoveryRevision{document: document, canonical: append([]byte(nil), canonical...)}
	s.order = append(s.order, revision)
	s.current = revision
	for len(s.order) > s.capacity {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.revisions, oldest)
	}
	return nil
}

// Rollback atomically selects one retained document without changing history.
func (s *RevisionStore) Rollback(revision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.revisions[revision]; !found {
		return ErrDiscoveryRevisionNotFound
	}
	s.current = revision
	return nil
}

// Current returns the immutable revision selected for a new request.
func (s *RevisionStore) Current(ctx context.Context) (schema.Document, error) {
	if err := ctx.Err(); err != nil {
		return schema.Document{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == "" {
		return schema.Document{}, ErrNoDiscoveryRevision
	}
	return s.revisions[s.current].document, nil
}

// Lookup returns one retained immutable revision.
func (s *RevisionStore) Lookup(ctx context.Context, revision string) (schema.Document, bool, error) {
	if err := ctx.Err(); err != nil {
		return schema.Document{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	retained, found := s.revisions[revision]
	return retained.document, found, nil
}
