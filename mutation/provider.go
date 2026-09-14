package mutation

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type ReadConsistency string

const (
	ReadBestEffort ReadConsistency = "best-effort"
	ReadSnapshot   ReadConsistency = "snapshot"
	ReadYourWrites ReadConsistency = "read-your-writes"
)

type ProviderCapabilities struct {
	ConditionalWrites bool
	SnapshotReads     bool
	ReadYourWrites    bool
}

type Registration struct {
	Descriptor     Descriptor
	Authorize      func(context.Context, string) bool
	AuthorizeField func(context.Context, string, string) bool
	Write          func(context.Context, string, map[string]any, map[string]any) error
}

type ApplyRequest struct {
	Mutation       string
	Entity         string
	Expected       Revision
	Update         *UpdateInput
	Patch          []PatchOperation
	IdempotencyKey string
}

type Result struct {
	Value    map[string]any `json:"value"`
	Revision Revision       `json:"revision"`
	Replayed bool           `json:"replayed,omitempty"`
}

type ReadRequest struct {
	Mutation      string
	Entity        string
	Consistency   ReadConsistency
	Snapshot      string
	AfterRevision Revision
}

type ReadResult struct {
	Value    map[string]any `json:"value"`
	Revision Revision       `json:"revision"`
	Snapshot string         `json:"snapshot,omitempty"`
}

type storedEntity struct {
	value    map[string]any
	revision Revision
	sequence uint64
}

type replayRecord struct {
	fingerprint [sha256.Size]byte
	result      Result
}

type snapshotRecord struct {
	mutation string
	entity   string
	result   ReadResult
}

const maxReferenceSnapshots = 1024

// MemoryProvider is a process-local reference application provider. Its mutex
// is the atomic boundary for expected-revision comparison and the protected
// write. Production providers must provide the same property in durable storage.
type MemoryProvider struct {
	mu            sync.Mutex
	capabilities  ProviderCapabilities
	secret        [sha256.Size]byte
	sequence      uint64
	registrations map[string]Registration
	entities      map[string]storedEntity
	replays       map[string]replayRecord
	snapshots     map[string]snapshotRecord
	revisions     map[string]map[Revision]uint64
}

func NewMemoryProvider(capabilities ProviderCapabilities) (*MemoryProvider, error) {
	provider := &MemoryProvider{
		capabilities:  capabilities,
		registrations: make(map[string]Registration), entities: make(map[string]storedEntity),
		replays: make(map[string]replayRecord), snapshots: make(map[string]snapshotRecord),
		revisions: make(map[string]map[Revision]uint64),
	}
	if _, err := rand.Read(provider.secret[:]); err != nil {
		return nil, fmt.Errorf("initialize revision signer: %w", err)
	}
	return provider, nil
}

func (p *MemoryProvider) Capabilities() ProviderCapabilities {
	return p.capabilities
}

func (p *MemoryProvider) Register(name string, registration Registration) error {
	if !identityPattern.MatchString(name) {
		return errors.New("invalid mutation registration name")
	}
	descriptor, err := NewDescriptor(registration.Descriptor)
	if err != nil {
		return err
	}
	registration.Descriptor = descriptor
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.registrations[name]; exists {
		return errors.New("duplicate mutation registration")
	}
	p.registrations[name] = registration
	return nil
}

// Seed installs test/reference application data and returns its opaque revision.
func (p *MemoryProvider) Seed(mutation, entity string, value map[string]any) (Revision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.registrations[mutation]; !exists || entity == "" {
		return "", errors.New("unknown mutation or empty entity identity")
	}
	revision, sequence := p.nextRevision(mutation, entity)
	p.entities[entityKey(mutation, entity)] = storedEntity{value: cloneObject(value), revision: revision, sequence: sequence}
	p.recordRevision(mutation, entity, revision, sequence)
	return revision, nil
}

func (p *MemoryProvider) Apply(ctx context.Context, request ApplyRequest) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	registration, err := p.authorizedMutation(ctx, request)
	if err != nil {
		return Result{}, err
	}
	fingerprint, err := requestFingerprint(request)
	if err != nil {
		return Result{}, failure(CodeInvalidUpdate, "mutation input is invalid", nil, err)
	}
	replayKey := entityKey(request.Mutation, request.Entity) + "\x00" + request.IdempotencyKey
	if result, found, replayErr := p.replay(ctx, registration, request, replayKey, fingerprint); found || replayErr != nil {
		return result, replayErr
	}
	key, current, err := p.currentForWrite(request, registration)
	if err != nil {
		return Result{}, err
	}
	changed, err := applyRequestedUpdate(ctx, registration, request, current.value)
	if err != nil {
		return Result{}, err
	}
	return p.commit(ctx, registration, request, replayKey, fingerprint, key, current, changed)
}

func (p *MemoryProvider) authorizedMutation(ctx context.Context, request ApplyRequest) (Registration, error) {
	registration, exists := p.registrations[request.Mutation]
	if !exists {
		return Registration{}, failure(CodeEntityNotFound, "entity was not found", nil, nil)
	}
	if registration.Authorize != nil && !registration.Authorize(ctx, request.Entity) {
		return Registration{}, failure(CodeUnauthorized, "mutation is not authorized", nil, nil)
	}
	return registration, nil
}

func (p *MemoryProvider) replay(ctx context.Context, registration Registration, request ApplyRequest, replayKey string, fingerprint [sha256.Size]byte) (Result, bool, error) {
	if request.IdempotencyKey == "" {
		return Result{}, false, nil
	}
	record, found := p.replays[replayKey]
	if !found {
		return Result{}, false, nil
	}
	if !hmac.Equal(record.fingerprint[:], fingerprint[:]) {
		return Result{}, true, failure("IDEMPOTENCY_CONFLICT", "idempotency key conflicts with a different request", nil, nil)
	}
	if !requestFieldsAuthorized(registration, ctx, request) {
		return Result{}, true, failure(CodeUnauthorized, "mutation is not authorized", nil, nil)
	}
	result := cloneResult(record.result)
	result.Replayed = true
	return result, true, nil
}

func (p *MemoryProvider) currentForWrite(request ApplyRequest, registration Registration) (string, storedEntity, error) {
	if request.Expected == "" && registration.Descriptor.RequireRevision {
		return "", storedEntity{}, failure(CodePreconditionRequired, "expected revision is required", nil, nil)
	}
	if request.Expected != "" && !p.capabilities.ConditionalWrites {
		return "", storedEntity{}, failure(CodePreconditionUnsupported, "expected revision is not supported", nil, nil)
	}
	key := entityKey(request.Mutation, request.Entity)
	current, exists := p.entities[key]
	if !exists {
		return "", storedEntity{}, failure(CodeEntityNotFound, "entity was not found", nil, nil)
	}
	if request.Expected != "" && request.Expected != current.revision {
		return "", storedEntity{}, failure(CodeRevisionConflict, "expected revision did not match", nil, nil)
	}
	return key, current, nil
}

func applyRequestedUpdate(ctx context.Context, registration Registration, request ApplyRequest, current map[string]any) (map[string]any, error) {
	authorizeField := FieldAuthorizer(nil)
	if registration.AuthorizeField != nil {
		authorizeField = func(field string) bool { return registration.AuthorizeField(ctx, request.Entity, field) }
	}
	conditional := request.Expected != ""
	switch {
	case request.Update != nil && request.Patch == nil:
		return registration.Descriptor.Apply(current, *request.Update, authorizeField, conditional)
	case request.Update == nil && request.Patch != nil:
		return registration.Descriptor.ApplyJSONPatch(current, request.Patch, authorizeField, conditional)
	default:
		return nil, failure(CodeInvalidUpdate, "mutation requires exactly one update representation", nil, nil)
	}
}

func (p *MemoryProvider) commit(ctx context.Context, registration Registration, request ApplyRequest, replayKey string, fingerprint [sha256.Size]byte, key string, current storedEntity, changed map[string]any) (Result, error) {
	if registration.Write != nil {
		if err := registration.Write(ctx, request.Entity, cloneObject(current.value), cloneObject(changed)); err != nil {
			return Result{}, failure(CodeProviderFailed, "mutation provider failed", nil, err)
		}
	}
	revision, sequence := p.nextRevision(request.Mutation, request.Entity)
	p.entities[key] = storedEntity{value: cloneObject(changed), revision: revision, sequence: sequence}
	p.recordRevision(request.Mutation, request.Entity, revision, sequence)
	result := Result{Value: cloneObject(changed), Revision: revision}
	if request.IdempotencyKey != "" {
		p.replays[replayKey] = replayRecord{fingerprint: fingerprint, result: cloneResult(result)}
	}
	return result, nil
}

func requestFieldsAuthorized(registration Registration, ctx context.Context, request ApplyRequest) bool {
	if registration.AuthorizeField == nil {
		return true
	}
	fieldsByPath := make(map[string]string, len(registration.Descriptor.Fields))
	for _, field := range registration.Descriptor.Fields {
		fieldsByPath[field.Path] = field.ID
	}
	if request.Update != nil {
		for _, edit := range request.Update.Edits {
			if !registration.AuthorizeField(ctx, request.Entity, edit.Field) {
				return false
			}
		}
	}
	for _, operation := range request.Patch {
		field := fieldsByPath[operation.Path]
		if field == "" || !registration.AuthorizeField(ctx, request.Entity, field) {
			return false
		}
	}
	return true
}

func (p *MemoryProvider) Read(ctx context.Context, request ReadRequest) (ReadResult, error) {
	if err := ctx.Err(); err != nil {
		return ReadResult{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	registration, exists := p.registrations[request.Mutation]
	if !exists || (registration.Authorize != nil && !registration.Authorize(ctx, request.Entity)) {
		return ReadResult{}, failure(CodeUnauthorized, "read is not authorized", nil, nil)
	}
	consistency, err := p.resolveReadConsistency(request.Consistency)
	if err != nil {
		return ReadResult{}, err
	}
	if consistency == ReadSnapshot && request.Snapshot != "" {
		return p.readSnapshot(request)
	}
	current, found := p.entities[entityKey(request.Mutation, request.Entity)]
	if !found {
		return ReadResult{}, failure(CodeEntityNotFound, "entity was not found", nil, nil)
	}
	if err := p.validateReadYourWrites(consistency, request, current); err != nil {
		return ReadResult{}, err
	}
	result := ReadResult{Value: cloneObject(current.value), Revision: current.revision}
	if consistency == ReadSnapshot {
		return p.createSnapshot(request, result)
	}
	return result, nil
}

func (p *MemoryProvider) resolveReadConsistency(requested ReadConsistency) (ReadConsistency, error) {
	switch requested {
	case "", ReadBestEffort:
		return ReadBestEffort, nil
	case ReadSnapshot:
		if p.capabilities.SnapshotReads {
			return requested, nil
		}
	case ReadYourWrites:
		if p.capabilities.ReadYourWrites {
			return requested, nil
		}
	}
	return "", failure(CodeReadConsistencyUnsupported, "requested read consistency is not supported", nil, nil)
}

func (p *MemoryProvider) readSnapshot(request ReadRequest) (ReadResult, error) {
	snapshot, found := p.snapshots[request.Snapshot]
	if !found || snapshot.mutation != request.Mutation || snapshot.entity != request.Entity {
		return ReadResult{}, failure(CodeReadConsistencyUnsupported, "snapshot is unavailable", nil, nil)
	}
	return cloneReadResult(snapshot.result), nil
}

func (p *MemoryProvider) validateReadYourWrites(consistency ReadConsistency, request ReadRequest, current storedEntity) error {
	if consistency != ReadYourWrites {
		return nil
	}
	if request.AfterRevision == "" || !p.revisionReached(request.Mutation, request.Entity, request.AfterRevision, current.sequence) {
		return failure(CodeRevisionConflict, "read-your-writes revision is unavailable", nil, nil)
	}
	return nil
}

func (p *MemoryProvider) createSnapshot(request ReadRequest, result ReadResult) (ReadResult, error) {
	if len(p.snapshots) >= maxReferenceSnapshots {
		return ReadResult{}, failure(CodeReadConsistencyUnsupported, "snapshot capacity is exhausted", nil, nil)
	}
	result.Snapshot = p.nextToken("snapshot", request.Mutation, request.Entity)
	p.snapshots[result.Snapshot] = snapshotRecord{mutation: request.Mutation, entity: request.Entity, result: cloneReadResult(result)}
	return result, nil
}

func (p *MemoryProvider) nextRevision(mutation, entity string) (Revision, uint64) {
	p.sequence++
	return Revision(p.nextTokenWithSequence("revision", mutation, entity, p.sequence)), p.sequence
}

func (p *MemoryProvider) nextToken(kind, mutation, entity string) string {
	p.sequence++
	return p.nextTokenWithSequence(kind, mutation, entity, p.sequence)
}

func (p *MemoryProvider) nextTokenWithSequence(kind, mutation, entity string, sequence uint64) string {
	mac := hmac.New(sha256.New, p.secret[:])
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(mutation))
	mac.Write([]byte{0})
	mac.Write([]byte(entity))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], sequence)
	mac.Write(encoded[:])
	return "nr1_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (p *MemoryProvider) revisionReached(mutation, entity string, revision Revision, current uint64) bool {
	sequence, exists := p.revisions[entityKey(mutation, entity)][revision]
	return exists && sequence <= current
}

func (p *MemoryProvider) recordRevision(mutation, entity string, revision Revision, sequence uint64) {
	key := entityKey(mutation, entity)
	if p.revisions[key] == nil {
		p.revisions[key] = make(map[Revision]uint64)
	}
	p.revisions[key][revision] = sequence
}

func entityKey(mutation, entity string) string { return mutation + "\x00" + entity }

func requestFingerprint(request ApplyRequest) ([sha256.Size]byte, error) {
	encoded, err := json.Marshal(struct {
		Mutation string           `json:"mutation"`
		Entity   string           `json:"entity"`
		Expected Revision         `json:"expected,omitempty"`
		Update   *UpdateInput     `json:"update,omitempty"`
		Patch    []PatchOperation `json:"patch,omitempty"`
	}{request.Mutation, request.Entity, request.Expected, request.Update, request.Patch})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func cloneResult(input Result) Result {
	input.Value = cloneObject(input.Value)
	return input
}

func cloneReadResult(input ReadResult) ReadResult {
	input.Value = cloneObject(input.Value)
	return input
}
