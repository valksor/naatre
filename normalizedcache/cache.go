package normalizedcache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/valksor/naatre/schema"
)

type storedField struct {
	state     FieldState
	value     json.RawMessage
	revision  string
	order     uint64
	tags      []string
	freshness Freshness
}

type entityRecord struct {
	revision       string
	revisionOrder  uint64
	knownRevisions map[string]uint64
	fields         map[FieldKey]storedField
}

type optimisticLayer struct {
	mutation OptimisticMutation
	state    OptimisticState
}

type subscription struct {
	scopeDigest string
	cancel      func(ScopeInvalidation)
}

// Cache is a concurrent, in-memory reference normalized cache. It owns no
// transport or executor behavior and has no background goroutines.
type Cache struct {
	mu            sync.RWMutex
	schema        schema.Snapshot
	entities      map[EntityID]*entityRecord
	requests      map[RequestKey]RequestEntry
	optimistic    map[string]*optimisticLayer
	optimisticIDs []string
	subscriptions map[string]subscription
}

func New(snapshot schema.Snapshot) *Cache {
	return &Cache{
		schema: snapshot, entities: make(map[EntityID]*entityRecord), requests: make(map[RequestKey]RequestEntry),
		optimistic: make(map[string]*optimisticLayer), subscriptions: make(map[string]subscription),
	}
}

func (c *Cache) Merge(patch EntityPatch, policy ConflictPolicy) (MergeResult, error) {
	prepared, result, err := c.prepareEntityPatch(patch)
	if err != nil {
		return MergeResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	merged, err := c.mergeLocked(prepared, policy)
	merged.IgnoredFields = result.IgnoredFields
	return merged, err
}

func (c *Cache) prepareEntityPatch(patch EntityPatch) (EntityPatch, MergeResult, error) {
	if patch.Entity.typeID == "" || patch.Entity.reference == "" || patch.Entity.scopeDigest == "" {
		return EntityPatch{}, MergeResult{}, errors.New("normalized cache entity identity is invalid")
	}
	if patch.Revision == "" || patch.Order == 0 {
		return EntityPatch{}, MergeResult{}, errors.New("normalized cache committed patch requires revision and positive order")
	}
	descriptor, ok := c.schema.Lookup(patch.Entity.typeID)
	if !ok || descriptor.Entity == nil {
		return EntityPatch{}, MergeResult{}, fmt.Errorf("%w: %s", ErrUnknownEntity, patch.Entity.typeID)
	}
	result := MergeResult{}
	prepared := patch
	prepared.Fields = make([]FieldPatch, 0, len(patch.Fields))
	seen := make(map[FieldKey]bool, len(patch.Fields))
	for _, field := range patch.Fields {
		declaration, declared := descriptor.Fields[field.Key.name]
		if !declared {
			result.IgnoredFields = append(result.IgnoredFields, field.Key.name)
			continue
		}
		value, err := prepareDeclaredField(declaration, field, seen)
		if err != nil {
			return EntityPatch{}, MergeResult{}, err
		}
		prepared.Fields = append(prepared.Fields, value)
	}
	sort.Slice(prepared.Fields, func(i, j int) bool {
		return fieldKeyString(prepared.Fields[i].Key) < fieldKeyString(prepared.Fields[j].Key)
	})
	slices.Sort(result.IgnoredFields)
	return prepared, result, nil
}

func prepareDeclaredField(declaration schema.FieldDescriptor, field FieldPatch, seen map[FieldKey]bool) (FieldPatch, error) {
	if field.State == FieldNull && !declaration.Nullable {
		return FieldPatch{}, fmt.Errorf("normalized cache non-null field %q received explicit null", field.Key.name)
	}
	if seen[field.Key] {
		return FieldPatch{}, fmt.Errorf("normalized cache patch repeats field %q", field.Key.name)
	}
	seen[field.Key] = true
	value, err := prepareField(field)
	if err != nil {
		return FieldPatch{}, fmt.Errorf("normalized cache field %q: %w", field.Key.name, err)
	}
	return value, nil
}

func prepareField(field FieldPatch) (FieldPatch, error) {
	if field.Key.name == "" || field.Key.schemaRevision == "" || !field.State.valid() || !field.Freshness.valid() {
		return FieldPatch{}, errors.New("invalid state, identity, or freshness")
	}
	if field.Cacheability == "" {
		field.Cacheability = CachePrivate
	}
	if field.Cacheability != CachePrivate && field.Cacheability != CacheNoStore {
		return FieldPatch{}, errors.New("invalid cacheability")
	}
	tags, err := normalizedTags(field.Tags)
	if err != nil {
		return FieldPatch{}, err
	}
	field.Tags = tags
	switch field.State {
	case FieldValue:
		field.Value, err = canonicalJSON(field.Value)
		if err != nil {
			return FieldPatch{}, fmt.Errorf("invalid JSON value: %w", err)
		}
		if bytes.Equal(field.Value, []byte("null")) {
			return FieldPatch{}, errors.New("null requires the explicit null state")
		}
	case FieldNull:
		if len(field.Value) != 0 && !bytes.Equal(bytes.TrimSpace(field.Value), []byte("null")) {
			return FieldPatch{}, errors.New("explicit null carries a non-null value")
		}
		field.Value = json.RawMessage("null")
	case FieldAbsent, FieldSkipped, FieldPending, FieldFailed:
		if len(field.Value) != 0 {
			return FieldPatch{}, errors.New("non-value state carries a value")
		}
	default:
		return FieldPatch{}, errors.New("invalid field state")
	}
	return field, nil
}

func (c *Cache) mergeLocked(patch EntityPatch, policy ConflictPolicy) (MergeResult, error) {
	if policy == "" {
		policy = ConflictReject
	}
	if policy != ConflictReject && policy != ConflictReplaceIncoming {
		return MergeResult{}, errors.New("normalized cache conflict policy is invalid")
	}
	record := cloneEntityRecord(c.entities[patch.Entity])
	if err := acceptRevision(record, patch, policy); err != nil {
		return MergeResult{}, err
	}
	result := MergeResult{}
	for _, field := range patch.Fields {
		applied, err := mergeField(record, field, patch.Revision, patch.Order, policy)
		if err != nil {
			return result, err
		}
		if applied {
			result.Applied = append(result.Applied, field.Key.name)
		} else {
			result.Preserved = append(result.Preserved, field.Key.name)
		}
	}
	c.entities[patch.Entity] = record
	return result, nil
}

func mergeField(record *entityRecord, field FieldPatch, revision string, order uint64, policy ConflictPolicy) (bool, error) {
	if field.Cacheability == CacheNoStore {
		return false, nil
	}
	current, exists := record.fields[field.Key]
	if exists && current.order > order && policy != ConflictReplaceIncoming {
		return false, nil
	}
	if exists && current.order == order && policy != ConflictReplaceIncoming {
		if equalStoredField(current, field, revision) {
			return false, nil
		}
		return false, fmt.Errorf("%w: field %s at order %d", ErrRevisionConflict, field.Key.name, order)
	}
	if isPartialMarker(field.State) && exists && isKnownValue(current.state) {
		return false, nil
	}
	record.fields[field.Key] = storedField{
		state: field.State, value: slices.Clone(field.Value), revision: revision, order: order,
		tags: slices.Clone(field.Tags), freshness: field.Freshness,
	}
	return true, nil
}

func isPartialMarker(state FieldState) bool {
	return state == FieldAbsent || state == FieldSkipped || state == FieldPending || state == FieldFailed
}

func isKnownValue(state FieldState) bool { return state == FieldValue || state == FieldNull }

func acceptRevision(record *entityRecord, patch EntityPatch, policy ConflictPolicy) error {
	if record.revision == "" {
		record.revision, record.revisionOrder = patch.Revision, patch.Order
		record.knownRevisions[patch.Revision] = patch.Order
		return nil
	}
	if patch.Revision == record.revision {
		if patch.Order > record.revisionOrder {
			record.revisionOrder = patch.Order
			record.knownRevisions[patch.Revision] = patch.Order
		}
		return nil
	}
	if knownOrder, known := record.knownRevisions[patch.Revision]; known && knownOrder <= record.revisionOrder && policy != ConflictReplaceIncoming {
		return fmt.Errorf("%w: revision %s", ErrStaleRevision, patch.Revision)
	}
	if policy != ConflictReplaceIncoming && (patch.BaseRevision != record.revision || patch.Order <= record.revisionOrder) {
		return fmt.Errorf("%w: current %s incoming %s", ErrRevisionConflict, record.revision, patch.Revision)
	}
	record.revision, record.revisionOrder = patch.Revision, patch.Order
	record.knownRevisions[patch.Revision] = patch.Order
	return nil
}

func equalStoredField(current storedField, incoming FieldPatch, revision string) bool {
	return current.state == incoming.State && current.revision == revision && bytes.Equal(current.value, incoming.Value) &&
		slices.Equal(current.tags, incoming.Tags) && current.freshness == incoming.Freshness
}

func (c *Cache) Lookup(entity EntityID, key FieldKey) (Field, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result, found := c.lookupBaseLocked(entity, key)
	for _, id := range c.optimisticIDs {
		layer := c.optimistic[id]
		candidate, ok := optimisticField(layer, entity, key)
		if !ok {
			continue
		}
		if isPartialMarker(candidate.State) && found && isKnownValue(result.State) {
			continue
		}
		result.State = candidate.State
		result.Value = slices.Clone(candidate.Value)
		result.Freshness = candidate.Freshness
		result.OptimisticMutation = id
		result.PendingReconciliation = layer.state != OptimisticActive
		found = true
	}
	return result, found
}

// LookupAt classifies one normalized field without promoting stale data to
// fresh. Expired data is reported as a miss.
func (c *Cache) LookupAt(entity EntityID, key FieldKey, now time.Time) (Field, FreshnessState) {
	field, ok := c.Lookup(entity, key)
	if !ok {
		return Field{}, FreshnessMiss
	}
	state := freshnessState(field.Freshness, now)
	if state == FreshnessMiss {
		return Field{}, state
	}
	return field, state
}

func freshnessState(freshness Freshness, now time.Time) FreshnessState {
	if !freshness.StaleUntil.IsZero() && now.After(freshness.StaleUntil) {
		return FreshnessMiss
	}
	if !freshness.FreshUntil.IsZero() && now.After(freshness.FreshUntil) {
		return FreshnessStale
	}
	return FreshnessFresh
}

func optimisticField(layer *optimisticLayer, entity EntityID, key FieldKey) (FieldPatch, bool) {
	if layer == nil {
		return FieldPatch{}, false
	}
	for _, patch := range layer.mutation.Patches {
		if patch.Entity != entity {
			continue
		}
		for _, field := range patch.Fields {
			if field.Key == key {
				return field, true
			}
		}
	}
	return FieldPatch{}, false
}

func (c *Cache) lookupBaseLocked(entity EntityID, key FieldKey) (Field, bool) {
	record := c.entities[entity]
	if record == nil {
		return Field{}, false
	}
	field, ok := record.fields[key]
	if !ok {
		return Field{}, false
	}
	return Field{State: field.state, Value: slices.Clone(field.value), Revision: field.revision, Freshness: field.freshness}, true
}

func (c *Cache) CurrentRevision(entity EntityID) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	record := c.entities[entity]
	if record == nil || record.revision == "" {
		return "", false
	}
	return record.revision, true
}

func (c *Cache) StoreRequest(key RequestKey, entry RequestEntry, cacheability Cacheability) error {
	if key.digest == "" || key.scopeDigest == "" || !entry.Freshness.valid() {
		return errors.New("normalized request cache entry is invalid")
	}
	if cacheability == CacheNoStore {
		c.mu.Lock()
		delete(c.requests, key)
		c.mu.Unlock()
		return nil
	}
	if cacheability != CachePrivate {
		return errors.New("normalized request cache supports only private or no-store entries")
	}
	canonical, err := canonicalJSON(entry.Data)
	if err != nil {
		return fmt.Errorf("canonicalize normalized request result: %w", err)
	}
	tags, err := normalizedTags(entry.Tags)
	if err != nil {
		return err
	}
	entry.Data, entry.Tags = canonical, tags
	c.mu.Lock()
	c.requests[key] = cloneRequestEntry(entry)
	c.mu.Unlock()
	return nil
}

func (c *Cache) LookupRequest(key RequestKey, now time.Time) (RequestEntry, FreshnessState) {
	c.mu.RLock()
	entry, ok := c.requests[key]
	c.mu.RUnlock()
	if !ok {
		return RequestEntry{}, FreshnessMiss
	}
	state := freshnessState(entry.Freshness, now)
	if state == FreshnessMiss {
		return RequestEntry{}, FreshnessMiss
	}
	return cloneRequestEntry(entry), state
}

func cloneRequestEntry(entry RequestEntry) RequestEntry {
	entry.Data = slices.Clone(entry.Data)
	entry.Tags = slices.Clone(entry.Tags)
	return entry
}

func cloneEntityRecord(record *entityRecord) *entityRecord {
	if record == nil {
		return &entityRecord{knownRevisions: make(map[string]uint64), fields: make(map[FieldKey]storedField)}
	}
	cloned := &entityRecord{
		revision:       record.revision,
		revisionOrder:  record.revisionOrder,
		knownRevisions: make(map[string]uint64, len(record.knownRevisions)),
		fields:         make(map[FieldKey]storedField, len(record.fields)),
	}
	for revision, order := range record.knownRevisions {
		cloned.knownRevisions[revision] = order
	}
	for key, field := range record.fields {
		field.value = slices.Clone(field.value)
		field.tags = slices.Clone(field.tags)
		cloned.fields[key] = field
	}
	return cloned
}

func cloneEntityEntries(entries map[EntityID]*entityRecord) map[EntityID]*entityRecord {
	cloned := make(map[EntityID]*entityRecord, len(entries))
	for id, record := range entries {
		cloned[id] = cloneEntityRecord(record)
	}
	return cloned
}

func cloneRequestEntries(entries map[RequestKey]RequestEntry) map[RequestKey]RequestEntry {
	cloned := make(map[RequestKey]RequestEntry, len(entries))
	for key, entry := range entries {
		cloned[key] = cloneRequestEntry(entry)
	}
	return cloned
}

func (c *Cache) BeginOptimistic(mutation OptimisticMutation) error {
	if mutation.ID == "" || len(mutation.Patches) == 0 {
		return errors.New("normalized cache optimistic mutation requires an id and patches")
	}
	prepared := mutation
	prepared.Patches = make([]OptimisticPatch, len(mutation.Patches))
	for index, patch := range mutation.Patches {
		entityPatch, _, err := c.prepareEntityPatch(EntityPatch{Entity: patch.Entity, Revision: "optimistic", Order: 1, Fields: patch.Fields})
		if err != nil {
			return err
		}
		prepared.Patches[index] = OptimisticPatch{Entity: patch.Entity, ExpectedRevision: patch.ExpectedRevision, Fields: entityPatch.Fields}
	}
	tags, err := normalizedTags(mutation.Effect.Tags)
	if err != nil {
		return err
	}
	prepared.Effect.Tags = tags
	prepared.Effect.Entities = slices.Clone(mutation.Effect.Entities)
	if err := validateEffectScope(prepared.Effect, prepared.Patches); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.optimistic[mutation.ID]; exists {
		return ErrDuplicateMutation
	}
	for _, patch := range prepared.Patches {
		record := c.entities[patch.Entity]
		current := ""
		if record != nil {
			current = record.revision
		}
		if patch.ExpectedRevision != current {
			return fmt.Errorf("%w: expected %s current %s", ErrRevisionConflict, patch.ExpectedRevision, current)
		}
	}
	c.optimistic[mutation.ID] = &optimisticLayer{mutation: prepared, state: OptimisticActive}
	c.optimisticIDs = append(c.optimisticIDs, mutation.ID)
	c.invalidateRequestsLocked(prepared.Effect)
	return nil
}

func (c *Cache) ResolveOptimistic(id string, resolution MutationResolution) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	layer := c.optimistic[id]
	if layer == nil {
		return ErrUnknownMutation
	}
	switch resolution.Outcome {
	case MutationTimedOut:
		layer.state = OptimisticTimedOut
		return nil
	case MutationUnknown:
		layer.state = OptimisticUnknown
		return nil
	case MutationRejected:
		c.removeOptimisticLocked(id)
		return nil
	case MutationCommitted:
		prepared := make([]EntityPatch, len(resolution.Patches))
		for index, patch := range resolution.Patches {
			var err error
			prepared[index], _, err = c.prepareEntityPatch(patch)
			if err != nil {
				return err
			}
		}
		previousEntities := c.entities
		c.entities = cloneEntityEntries(previousEntities)
		for _, patch := range prepared {
			if _, err := c.mergeLocked(patch, resolution.Policy); err != nil {
				c.entities = previousEntities
				c.removeOptimisticLocked(id)
				return err
			}
		}
		c.entities = cloneEntityEntries(previousEntities)
		previousRequests := c.requests
		c.requests = cloneRequestEntries(previousRequests)
		c.invalidateEffectLocked(layer.mutation.Effect)
		for _, patch := range prepared {
			if _, err := c.mergeLocked(patch, resolution.Policy); err != nil {
				c.entities = previousEntities
				c.requests = previousRequests
				c.removeOptimisticLocked(id)
				return err
			}
		}
		c.removeOptimisticLocked(id)
		return nil
	default:
		return errors.New("normalized cache mutation outcome is invalid")
	}
}

func (c *Cache) OptimisticState(id string) (OptimisticState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	layer := c.optimistic[id]
	if layer == nil {
		return "", false
	}
	return layer.state, true
}

func (c *Cache) removeOptimisticLocked(id string) {
	delete(c.optimistic, id)
	for index, existing := range c.optimisticIDs {
		if existing == id {
			c.optimisticIDs = slices.Delete(c.optimisticIDs, index, index+1)
			break
		}
	}
}

// ApplyMutationEffect synchronously removes every known matching normalized
// entity and request entry before returning.
func (c *Cache) ApplyMutationEffect(effect MutationEffect) error {
	tags, err := normalizedTags(effect.Tags)
	if err != nil {
		return err
	}
	effect.Tags = tags
	if err := validateEffectScope(effect, nil); err != nil {
		return err
	}
	c.mu.Lock()
	c.invalidateEffectLocked(effect)
	c.mu.Unlock()
	return nil
}

func validateEffectScope(effect MutationEffect, patches []OptimisticPatch) error {
	scope := scopeKey(effect.Scope)
	for _, entity := range effect.Entities {
		if entity.scopeDigest != scope {
			return errors.New("normalized cache mutation effect crosses authorization scope")
		}
	}
	for _, patch := range patches {
		if patch.Entity.scopeDigest != scope {
			return errors.New("normalized cache optimistic patch crosses authorization scope")
		}
	}
	return nil
}

func (c *Cache) invalidateEffectLocked(effect MutationEffect) {
	for _, entity := range effect.Entities {
		delete(c.entities, entity)
	}
	scope := scopeKey(effect.Scope)
	if len(effect.Tags) != 0 {
		for id, record := range c.entities {
			if id.scopeDigest == scope && entityHasTag(record, effect.Tags) {
				delete(c.entities, id)
			}
		}
	}
	c.invalidateRequestsLocked(effect)
}

func (c *Cache) invalidateRequestsLocked(effect MutationEffect) {
	if len(effect.Tags) == 0 {
		return
	}
	scope := scopeKey(effect.Scope)
	for key, entry := range c.requests {
		if key.scopeDigest == scope && tagsIntersect(entry.Tags, effect.Tags) {
			delete(c.requests, key)
		}
	}
}

func entityHasTag(record *entityRecord, tags []string) bool {
	for _, field := range record.fields {
		if tagsIntersect(field.tags, tags) {
			return true
		}
	}
	return false
}

func tagsIntersect(left, right []string) bool {
	for _, value := range left {
		if slices.Contains(right, value) {
			return true
		}
	}
	return false
}

func (c *Cache) RegisterSubscription(id string, scope Scope, cancel func(ScopeInvalidation)) error {
	if id == "" || cancel == nil {
		return errors.New("normalized cache subscription id and cancel callback are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.subscriptions[id]; exists {
		return errors.New("normalized cache subscription already exists")
	}
	c.subscriptions[id] = subscription{scopeDigest: scopeKey(scope), cancel: cancel}
	return nil
}

// InvalidateScope clears entities, request results, optimistic layers, and live
// subscriptions for exactly one principal/tenant/authorization revision.
func (c *Cache) InvalidateScope(scope Scope, reason ScopeInvalidation) {
	digest := scopeKey(scope)
	c.mu.Lock()
	for id := range c.entities {
		if id.scopeDigest == digest {
			delete(c.entities, id)
		}
	}
	for key := range c.requests {
		if key.scopeDigest == digest {
			delete(c.requests, key)
		}
	}
	for id, layer := range c.optimistic {
		if optimisticTouchesScope(layer, digest) {
			c.removeOptimisticLocked(id)
		}
	}
	callbacks := make([]func(ScopeInvalidation), 0)
	for id, subscription := range c.subscriptions {
		if subscription.scopeDigest == digest {
			callbacks = append(callbacks, subscription.cancel)
			delete(c.subscriptions, id)
		}
	}
	c.mu.Unlock()
	for _, callback := range callbacks {
		callback(reason)
	}
}

func optimisticTouchesScope(layer *optimisticLayer, scope string) bool {
	if scopeKey(layer.mutation.Effect.Scope) == scope {
		return true
	}
	for _, patch := range layer.mutation.Patches {
		if patch.Entity.scopeDigest == scope {
			return true
		}
	}
	return false
}

func fieldKeyString(key FieldKey) string {
	return key.name + "\x00" + key.argumentsDigest + "\x00" + key.locale + "\x00" + key.representation + "\x00" + key.schemaRevision
}
