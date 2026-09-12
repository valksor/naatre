package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/valksor/naatre/protocol"
)

type PersistedMode string

const (
	PersistedLookupOnly       PersistedMode = "lookup-only"
	PersistedRegisterAtDeploy PersistedMode = "register-at-deploy"
	PersistedAutomatic        PersistedMode = "automatic"
)

type PersistedCacheScope string

const (
	PersistedCachePrivate PersistedCacheScope = "private"
	PersistedCacheShared  PersistedCacheScope = "shared"
)

type PersistedCachePolicy struct {
	Scope                  PersistedCacheScope
	PublicOutput           bool
	AuthorizationInvariant bool
}

type PersistedRecord struct {
	Reference                   protocol.PersistedReference
	Tenant                      string
	Name                        string
	Revision                    string
	Owner                       string
	Operation                   string
	Kind                        protocol.OperationKind
	Document                    []byte
	SchemaRevision              string
	AuthorizationPolicyRevision string
	RequiredCapabilities        []string
	ApprovedCost                uint64
	Cache                       PersistedCachePolicy
	ExpiresAt                   time.Time
	RevokedAt                   time.Time
}

type PersistedKey struct {
	Tenant    string
	Reference protocol.PersistedReference
}

type PersistedStore interface {
	Register(context.Context, PersistedRecord) error
	Lookup(context.Context, PersistedKey) (PersistedRecord, error)
	Revoke(context.Context, PersistedKey, time.Time) error
	Migrate(context.Context, PersistedKey, string, PersistedRecord) error
	Evict(context.Context, PersistedKey) error
}

type PersistedError struct {
	Code  string
	cause error
}

func (e *PersistedError) Error() string { return e.Code + ": persisted operation unavailable" }
func (e *PersistedError) Unwrap() error { return e.cause }

func newPersistedError(code string, cause error) error {
	return &PersistedError{Code: code, cause: cause}
}

type MemoryPersistedStore struct {
	mu      sync.RWMutex
	records map[PersistedKey]PersistedRecord
}

func NewMemoryPersistedStore() *MemoryPersistedStore {
	return &MemoryPersistedStore{records: make(map[PersistedKey]PersistedRecord)}
}

func (s *MemoryPersistedStore) Register(ctx context.Context, record PersistedRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := persistedKey(record)
	copy := clonePersistedRecord(record)
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, exists := s.records[key]
	if !exists {
		s.records[key] = copy
		return nil
	}
	if !bytes.Equal(prior.Document, copy.Document) {
		return newPersistedError("PERSISTED_HASH_COLLISION", nil)
	}
	if !reflect.DeepEqual(prior, copy) {
		return newPersistedError("PERSISTED_CONFLICT", nil)
	}
	return nil
}

func (s *MemoryPersistedStore) Lookup(ctx context.Context, key PersistedKey) (PersistedRecord, error) {
	if err := ctx.Err(); err != nil {
		return PersistedRecord{}, err
	}
	s.mu.RLock()
	record, ok := s.records[key]
	s.mu.RUnlock()
	if !ok {
		return PersistedRecord{}, newPersistedError("PERSISTED_NOT_FOUND", nil)
	}
	return clonePersistedRecord(record), nil
}

func (s *MemoryPersistedStore) Revoke(ctx context.Context, key PersistedKey, at time.Time) error {
	return s.mutate(ctx, key, &at)
}

func (s *MemoryPersistedStore) Migrate(ctx context.Context, key PersistedKey, expectedRevision string, replacement PersistedRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, ok := s.records[key]
	if !ok {
		return newPersistedError("PERSISTED_NOT_FOUND", nil)
	}
	if prior.Revision != expectedRevision {
		return newPersistedError("PERSISTED_REVISION_CONFLICT", nil)
	}
	if persistedKey(replacement) != key || !bytes.Equal(prior.Document, replacement.Document) {
		return newPersistedError("PERSISTED_IDENTITY_CHANGED", nil)
	}
	s.records[key] = clonePersistedRecord(replacement)
	return nil
}

func (s *MemoryPersistedStore) Evict(ctx context.Context, key PersistedKey) error {
	return s.mutate(ctx, key, nil)
}

func (s *MemoryPersistedStore) mutate(ctx context.Context, key PersistedKey, revokedAt *time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return newPersistedError("PERSISTED_NOT_FOUND", nil)
	}
	if revokedAt == nil {
		delete(s.records, key)
		return nil
	}
	record.RevokedAt = *revokedAt
	s.records[key] = record
	return nil
}

type PersistedResolverOptions struct {
	Mode                        PersistedMode
	Decode                      protocol.DecodeOptions
	SchemaRevision              string
	AuthorizationPolicyRevision string
	Now                         func() time.Time
	OnChange                    func(PersistedEvent)
	AutomaticRevision           string
	AutomaticQuota              func(context.Context, Principal) error
	AutomaticApproval           func(context.Context, Principal, PersistedRecord) error
}

type PersistedEventKind string

const (
	PersistedRegistered PersistedEventKind = "registered"
	PersistedRevoked    PersistedEventKind = "revoked"
	PersistedMigrated   PersistedEventKind = "migrated"
	PersistedEvicted    PersistedEventKind = "evicted"
)

type PersistedEvent struct {
	Kind     PersistedEventKind
	Key      PersistedKey
	Revision string
	At       time.Time
}

type PersistedResolver struct {
	store   PersistedStore
	options PersistedResolverOptions
}

type persistedChange struct {
	kind             PersistedEventKind
	key              PersistedKey
	record           PersistedRecord
	expectedRevision string
}

func NewPersistedResolver(store PersistedStore, options PersistedResolverOptions) (*PersistedResolver, error) {
	if store == nil {
		return nil, errors.New("persisted resolver requires a store")
	}
	switch options.Mode {
	case PersistedLookupOnly, PersistedRegisterAtDeploy, PersistedAutomatic:
	default:
		return nil, fmt.Errorf("unsupported persisted mode %q", options.Mode)
	}
	if options.SchemaRevision == "" || options.AuthorizationPolicyRevision == "" {
		return nil, errors.New("persisted resolver requires current schema and policy revisions")
	}
	if options.Mode == PersistedAutomatic &&
		(options.AutomaticRevision == "" || options.AutomaticQuota == nil || options.AutomaticApproval == nil) {
		return nil, errors.New("automatic registration requires a revision, quota, and approval hooks")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &PersistedResolver{store: store, options: options}, nil
}

func (r *PersistedResolver) RegisterDeploy(ctx context.Context, record PersistedRecord) error {
	return r.applyChange(ctx, persistedChange{kind: PersistedRegistered, key: persistedKey(record), record: record})
}

func (r *PersistedResolver) RegisterAutomatic(ctx context.Context, record PersistedRecord) error {
	if r.options.Mode != PersistedAutomatic {
		return newPersistedError("PERSISTED_MODE_DENIED", nil)
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Subject == "" {
		return newPersistedError("AUTOMATIC_AUTHENTICATION_REQUIRED", nil)
	}
	if principal.Tenant != record.Tenant {
		return newPersistedError("PERSISTED_TENANT_MISMATCH", nil)
	}
	if err := validatePersistedRecord(record); err != nil {
		return err
	}
	if err := r.validateCurrentApproval(record); err != nil {
		return err
	}
	if err := r.options.AutomaticQuota(ctx, principal); err != nil {
		return newPersistedError("AUTOMATIC_QUOTA_DENIED", err)
	}
	if err := r.options.AutomaticApproval(ctx, principal, clonePersistedRecord(record)); err != nil {
		return newPersistedError("AUTOMATIC_APPROVAL_DENIED", err)
	}
	if err := r.store.Register(ctx, record); err != nil {
		return wrapPersistedStoreError(err)
	}
	r.notify(PersistedRegistered, persistedKey(record), record.Revision)
	return nil
}

func (r *PersistedResolver) Revoke(ctx context.Context, key PersistedKey) error {
	return r.applyChange(ctx, persistedChange{kind: PersistedRevoked, key: key})
}

func (r *PersistedResolver) Migrate(ctx context.Context, key PersistedKey, expectedRevision string, replacement PersistedRecord) error {
	return r.applyChange(ctx, persistedChange{kind: PersistedMigrated, key: key, record: replacement, expectedRevision: expectedRevision})
}

func (r *PersistedResolver) Evict(ctx context.Context, key PersistedKey) error {
	return r.applyChange(ctx, persistedChange{kind: PersistedEvicted, key: key})
}

func (r *PersistedResolver) applyChange(ctx context.Context, change persistedChange) error {
	if change.kind == PersistedRegistered && r.options.Mode != PersistedRegisterAtDeploy ||
		change.kind != PersistedRegistered && r.options.Mode == PersistedLookupOnly {
		return newPersistedError("PERSISTED_MODE_DENIED", nil)
	}
	if change.kind == PersistedRegistered || change.kind == PersistedMigrated {
		if err := validatePersistedRecord(change.record); err != nil {
			return err
		}
		if err := r.validateCurrentApproval(change.record); err != nil {
			return err
		}
	}
	var err error
	switch change.kind {
	case PersistedRegistered:
		err = r.store.Register(ctx, change.record)
	case PersistedRevoked:
		err = r.store.Revoke(ctx, change.key, r.options.Now())
	case PersistedMigrated:
		err = r.store.Migrate(ctx, change.key, change.expectedRevision, change.record)
	case PersistedEvicted:
		err = r.store.Evict(ctx, change.key)
	default:
		return errors.New("unsupported persisted lifecycle change")
	}
	if err != nil {
		return wrapPersistedStoreError(err)
	}
	r.notify(change.kind, change.key, change.record.Revision)
	return nil
}

func (r *PersistedResolver) notify(kind PersistedEventKind, key PersistedKey, revision string) {
	if r.options.OnChange != nil {
		r.options.OnChange(PersistedEvent{Kind: kind, Key: key, Revision: revision, At: r.options.Now()})
	}
}

func (r *PersistedResolver) Admit(ctx context.Context, registry Snapshot, input []byte, prepare PrepareOptions) (*Plan, PersistedRecord, error) {
	decode := r.options.Decode
	if r.options.Mode != PersistedAutomatic {
		decode.SourcePolicy = protocol.PersistedOnly
	}
	request, err := protocol.DecodeRequest(input, decode)
	if err != nil {
		return nil, PersistedRecord{}, err
	}
	reference, persisted := request.Persisted()
	if !persisted {
		return r.admitAutomatic(ctx, registry, request, prepare)
	}
	return r.admitPersisted(ctx, registry, request, reference, decode, prepare)
}

func (r *PersistedResolver) admitAutomatic(ctx context.Context, registry Snapshot, request *protocol.Request, prepare PrepareOptions) (*Plan, PersistedRecord, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Subject == "" {
		return nil, PersistedRecord{}, newPersistedError("AUTOMATIC_AUTHENTICATION_REQUIRED", nil)
	}
	if err := r.options.AutomaticQuota(ctx, principal); err != nil {
		return nil, PersistedRecord{}, newPersistedError("AUTOMATIC_QUOTA_DENIED", err)
	}
	plan, err := PrepareWithOptions(registry, request, prepare)
	if err != nil {
		return nil, PersistedRecord{}, err
	}
	record, err := r.automaticRecord(principal, request, plan)
	if err != nil {
		return nil, PersistedRecord{}, err
	}
	if err := r.options.AutomaticApproval(ctx, principal, clonePersistedRecord(record)); err != nil {
		return nil, PersistedRecord{}, newPersistedError("AUTOMATIC_APPROVAL_DENIED", err)
	}
	if err := r.store.Register(ctx, record); err != nil {
		return nil, PersistedRecord{}, wrapPersistedStoreError(err)
	}
	r.notify(PersistedRegistered, persistedKey(record), record.Revision)
	return plan, clonePersistedRecord(record), nil
}

func (r *PersistedResolver) admitPersisted(ctx context.Context, registry Snapshot, request *protocol.Request, reference protocol.PersistedReference, decode protocol.DecodeOptions, prepare PrepareOptions) (*Plan, PersistedRecord, error) {
	principal, _ := PrincipalFromContext(ctx)
	key := PersistedKey{Tenant: principal.Tenant, Reference: reference}
	record, err := r.store.Lookup(ctx, key)
	if err != nil {
		return nil, PersistedRecord{}, wrapPersistedStoreError(err)
	}
	if err := r.validateAdmission(record, reference); err != nil {
		return nil, PersistedRecord{}, err
	}
	document, err := protocol.DecodeDocument(record.Document, decode.Limits)
	if err != nil {
		return nil, PersistedRecord{}, newPersistedError("PERSISTED_DOCUMENT_INVALID", err)
	}
	resolved, err := request.WithResolvedDocument(document, record.Operation)
	if err != nil {
		return nil, PersistedRecord{}, newPersistedError("PERSISTED_OPERATION_MISMATCH", err)
	}
	plan, err := PrepareWithOptions(registry, resolved, prepare)
	if err != nil {
		return nil, PersistedRecord{}, err
	}
	if plan.kind != record.Kind {
		return nil, PersistedRecord{}, newPersistedError("PERSISTED_KIND_MISMATCH", nil)
	}
	if plan.staticCost > record.ApprovedCost {
		return nil, PersistedRecord{}, newPersistedError("PERSISTED_COST_EXCEEDED", nil)
	}
	return plan, clonePersistedRecord(record), nil
}

func (r *PersistedResolver) automaticRecord(principal Principal, request *protocol.Request, plan *Plan) (PersistedRecord, error) {
	document := request.Document()
	digest, err := protocol.SemanticHash(protocol.DocumentHash, document.CanonicalJSON())
	if err != nil {
		return PersistedRecord{}, newPersistedError("PERSISTED_DOCUMENT_INVALID", err)
	}
	record := PersistedRecord{
		Reference: protocol.PersistedReference{
			Algorithm: digest.Algorithm, CanonicalVersion: digest.CanonicalVersion, Digest: digest.Hex,
		},
		Tenant: principal.Tenant, Name: plan.operationName, Revision: r.options.AutomaticRevision,
		Owner: principal.Subject, Operation: plan.operationName, Kind: plan.kind,
		Document: document.CanonicalJSON(), SchemaRevision: r.options.SchemaRevision,
		AuthorizationPolicyRevision: r.options.AuthorizationPolicyRevision,
		RequiredCapabilities:        slices.Clone(plan.requirements), ApprovedCost: plan.staticCost,
		Cache: PersistedCachePolicy{Scope: PersistedCachePrivate},
	}
	if err := validatePersistedRecord(record); err != nil {
		return PersistedRecord{}, err
	}
	return record, nil
}

func (r *PersistedResolver) validateAdmission(record PersistedRecord, reference protocol.PersistedReference) error {
	if err := validatePersistedRecord(record); err != nil {
		return err
	}
	if record.Reference != reference {
		return newPersistedError("PERSISTED_REFERENCE_MISMATCH", nil)
	}
	if !record.RevokedAt.IsZero() {
		return newPersistedError("PERSISTED_REVOKED", nil)
	}
	now := r.options.Now()
	if !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
		return newPersistedError("PERSISTED_EXPIRED", nil)
	}
	if err := r.validateCurrentApproval(record); err != nil {
		return err
	}
	return nil
}

func (r *PersistedResolver) validateCurrentApproval(record PersistedRecord) error {
	if record.SchemaRevision != r.options.SchemaRevision {
		return newPersistedError("PERSISTED_SCHEMA_STALE", nil)
	}
	if record.AuthorizationPolicyRevision != r.options.AuthorizationPolicyRevision {
		return newPersistedError("PERSISTED_POLICY_STALE", nil)
	}
	return nil
}

func validatePersistedRecord(record PersistedRecord) error {
	if record.Reference.Algorithm != "sha-256" || record.Reference.CanonicalVersion != "c14n-1" {
		return newPersistedError("PERSISTED_ALGORITHM_MISMATCH", nil)
	}
	if record.Name == "" || record.Revision == "" || record.Owner == "" || record.Operation == "" ||
		record.SchemaRevision == "" || record.AuthorizationPolicyRevision == "" || len(record.Document) == 0 {
		return newPersistedError("PERSISTED_RECORD_INVALID", nil)
	}
	document, err := protocol.DecodeDocument(record.Document, protocol.Limits{})
	if err != nil || !bytes.Equal(record.Document, document.CanonicalJSON()) {
		return newPersistedError("PERSISTED_DOCUMENT_INVALID", err)
	}
	digest, err := protocol.SemanticHash(protocol.DocumentHash, record.Document)
	if err != nil || digest.Hex != record.Reference.Digest {
		return newPersistedError("PERSISTED_DOCUMENT_MISMATCH", err)
	}
	operation, err := selectOperation(document.Operations(), record.Operation, document.Source())
	if err != nil || operation.Kind() != record.Kind {
		return newPersistedError("PERSISTED_OPERATION_MISMATCH", err)
	}
	if !slices.Equal(record.RequiredCapabilities, document.Requires()) {
		return newPersistedError("PERSISTED_CAPABILITY_MISMATCH", nil)
	}
	switch record.Cache.Scope {
	case PersistedCachePrivate:
		if record.Cache.PublicOutput || record.Cache.AuthorizationInvariant {
			return newPersistedError("PERSISTED_CACHE_INVALID", nil)
		}
	case PersistedCacheShared:
		if !record.Cache.PublicOutput || !record.Cache.AuthorizationInvariant {
			return newPersistedError("PERSISTED_CACHE_INVALID", nil)
		}
	default:
		return newPersistedError("PERSISTED_CACHE_INVALID", nil)
	}
	return nil
}

func persistedKey(record PersistedRecord) PersistedKey {
	return PersistedKey{Tenant: record.Tenant, Reference: record.Reference}
}

func clonePersistedRecord(record PersistedRecord) PersistedRecord {
	clonePersistedRecordPayload(&record)
	return record
}

func clonePersistedRecordPayload(record *PersistedRecord) {
	document := bytes.Clone(record.Document)
	capabilities := slices.Clone(record.RequiredCapabilities)
	record.Document = document
	record.RequiredCapabilities = capabilities
}

func wrapPersistedStoreError(err error) error {
	var persisted *PersistedError
	if errors.As(err, &persisted) {
		return err
	}
	return newPersistedError("PERSISTED_STORAGE", err)
}
