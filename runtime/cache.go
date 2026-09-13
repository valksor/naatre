package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// CacheKey is the complete opaque identity supplied to an application-owned
// cross-request cache. Identity fields contain digests, never raw variables,
// claims, or handler inputs.
type CacheKey struct {
	Digest                string
	Operation             string
	OperationKind         protocol.OperationKind
	Handler               string
	SchemaRevision        string
	SchemaDigest          string
	DocumentDigest        string
	VariablesDigest       string
	AuthorizationIdentity string
	InputDigest           string
	Context               string
	Generation            uint64
}

// CacheEntry is a typed in-process handler result owned by the application.
// The runtime validates and copies Value again before exposing it in a result.
type CacheEntry struct {
	Value any
	Error error
}

// CacheInvalidation describes a completed write. Applications decide which
// of their own cross-request entries it invalidates.
type CacheInvalidation struct {
	Operation             string
	OperationKind         protocol.OperationKind
	Handler               string
	SchemaRevision        string
	AuthorizationIdentity string
	Context               string
	Generation            uint64
}

// ResultCache is an optional application-owned cross-request cache. The core
// runtime retains no ResultCache entries outside one Execute call.
type ResultCache interface {
	Load(context.Context, CacheKey) (CacheEntry, bool, error)
	Store(context.Context, CacheKey, CacheEntry) error
	Invalidate(context.Context, CacheInvalidation) error
}

type CacheEventKind string

const (
	CacheMissEvent       CacheEventKind = "miss"
	CacheHitEvent        CacheEventKind = "hit"
	CacheStoreEvent      CacheEventKind = "store"
	CacheInvalidateEvent CacheEventKind = "invalidate"
	CacheWaitCancelEvent CacheEventKind = "wait-cancel"
)

// CacheEvent exposes tracing-safe cache activity without raw key material.
type CacheEvent struct {
	Kind       CacheEventKind
	Key        CacheKey
	External   bool
	Generation uint64
}

// CacheOptions controls one request's memoization and optional application
// hooks. CacheErrors and CacheNulls are off by default.
type CacheOptions struct {
	DisableRequest bool
	SchemaRevision string
	Context        string
	External       ResultCache
	CacheErrors    bool
	CacheNulls     bool
	Observe        func(CacheEvent)
}

type cacheRecord struct {
	ready chan struct{}
	entry CacheEntry
}

type executionCache struct {
	mu         sync.Mutex
	hookMu     sync.Mutex
	entries    map[string]*cacheRecord
	generation uint64
	options    CacheOptions
	base       cacheBase
}

type cacheBase struct {
	documentDigest  string
	schemaDigest    string
	variablesDigest string
	baseDigest      string
}

type cacheReservation struct {
	cache    *executionCache
	ctx      context.Context
	key      CacheKey
	localKey string
	record   *cacheRecord
	external bool
	once     sync.Once
}

func newExecutionCache(plan *Plan, options CacheOptions) *executionCache {
	cache := new(executionCache)
	cache.entries = make(map[string]*cacheRecord)
	cache.options = options
	cache.base = cacheIdentityBase(plan)
	return cache
}

func cacheIdentityBase(plan *Plan) cacheBase {
	document, _ := protocol.SemanticHash(protocol.DocumentHash, plan.document)
	variables := plan.canonicalVariables()
	variablesJSON, _ := json.Marshal(variables)
	variablesCanonical, _ := protocol.CanonicalizeJSON(variablesJSON, protocol.DefaultLimits())
	variablesHash, _ := protocol.SemanticHash(protocol.ResultCacheHash, variablesCanonical)
	typesJSON, _ := json.Marshal(plan.types.Descriptors())
	typesCanonical, _ := protocol.CanonicalizeJSON(typesJSON, protocol.DefaultLimits())
	typesHash, _ := protocol.SemanticHash(protocol.SchemaHash, typesCanonical)
	payload := struct {
		Document     protocol.Digest            `json:"document"`
		Schema       protocol.Digest            `json:"schema"`
		Variables    map[string]json.RawMessage `json:"variables"`
		Capabilities []string                   `json:"capabilities"`
	}{Document: document, Schema: typesHash, Variables: variables, Capabilities: slices.Clone(plan.requirements)}
	encoded, _ := json.Marshal(payload)
	canonical, _ := protocol.CanonicalizeHashPayload(protocol.ResultCacheHash, encoded, protocol.DefaultLimits())
	digest, _ := protocol.SemanticHash(protocol.ResultCacheHash, canonical)
	return cacheBase{documentDigest: document.Hex, schemaDigest: typesHash.Hex, variablesDigest: variablesHash.Hex, baseDigest: digest.Hex}
}

func (p *Plan) canonicalVariables() map[string]json.RawMessage {
	variables := make(map[string]json.RawMessage, len(p.variableValues))
	for _, definition := range p.variables {
		raw, present := p.variableValues[definition.Name()]
		if !present {
			continue
		}
		value, err := schema.CoerceInput(p.types, schema.TypeID(definition.Type()), raw, definition.Nullable())
		if err != nil {
			continue
		}
		canonical, err := value.MarshalJSON()
		if err == nil {
			variables[definition.Name()] = canonical
		}
	}
	return variables
}

func (c *executionCache) key(plan *Plan, node planNode, source, input any, principal Principal, decision AuthorizationDecision) (CacheKey, bool, bool) {
	if c == nil || !node.definition.descriptor.Metadata.Cacheable || !plan.cacheEligibleNode(node) {
		return CacheKey{}, false, false
	}
	if plan.authorization.Authorizer != nil && (decision.CacheScope == "" || decision.CacheScope == AuthorizationCacheNoStore) {
		return CacheKey{}, false, false
	}
	inputDigest, err := typedInputDigest(source, input)
	if err != nil {
		return CacheKey{}, false, false
	}
	localIdentity := authorizationDigest(principal)
	externalIdentity, external := externalAuthorizationIdentity(principal, decision, plan.authorization.Authorizer != nil)
	identity := localIdentity
	if external {
		identity = externalIdentity
	}
	c.mu.Lock()
	generation := c.generation
	c.mu.Unlock()
	key := CacheKey{
		Operation: plan.operationName, OperationKind: plan.kind, Handler: registrationKey(node.definition.descriptor),
		SchemaRevision: c.options.SchemaRevision, SchemaDigest: c.base.schemaDigest, DocumentDigest: c.base.documentDigest,
		VariablesDigest: c.base.variablesDigest, AuthorizationIdentity: identity,
		InputDigest: inputDigest, Context: c.options.Context, Generation: generation,
	}
	encoded, _ := json.Marshal(struct {
		Base, Operation, OperationKind, Handler, Schema, Authorization, Input, Context string
	}{c.base.baseDigest, key.Operation, string(key.OperationKind), key.Handler, key.SchemaRevision, key.AuthorizationIdentity, key.InputDigest, key.Context})
	canonical, _ := protocol.CanonicalizeJSON(encoded, protocol.DefaultLimits())
	digest, _ := protocol.SemanticHash(protocol.ResultCacheHash, canonical)
	key.Digest = digest.Hex
	return key, true, external && c.options.External != nil && c.options.SchemaRevision != ""
}

func (p *Plan) cacheEligibleNode(node planNode) bool {
	if len(p.interceptorsFor(node)) != 0 {
		return false
	}
	for _, directive := range node.directives {
		if directive.definition.Wrapper != nil {
			return false
		}
	}
	return true
}

func typedInputDigest(source, input any) (string, error) {
	payload := struct {
		SourceType string `json:"sourceType"`
		InputType  string `json:"inputType"`
		Source     any    `json:"source"`
		Input      any    `json:"input"`
	}{SourceType: reflectedType(source), InputType: reflectedType(input), Source: source, Input: input}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.DefaultLimits())
	if err != nil {
		return "", err
	}
	digest, err := protocol.SemanticHash(protocol.ResultCacheHash, canonical)
	if err != nil {
		return "", err
	}
	return digest.Hex, nil
}

func reflectedType(value any) string {
	if value == nil {
		return "<nil>"
	}
	return reflect.TypeOf(value).String()
}

func authorizationDigest(principal Principal) string {
	encoded, _ := json.Marshal(principal)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func externalAuthorizationIdentity(principal Principal, decision AuthorizationDecision, dynamic bool) (string, bool) {
	if !dynamic {
		return "", false
	}
	var value any
	switch decision.CacheScope {
	case AuthorizationCachePrincipal:
		value = principal
	case AuthorizationCacheTenant:
		value = struct {
			Tenant   string `json:"tenant"`
			Revision string `json:"authorizationRevision"`
		}{principal.Tenant, principal.AuthorizationRevision}
	case "", AuthorizationCacheNoStore:
		return "", false
	default:
		return "", false
	}
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), true
}

func (c *executionCache) load(ctx context.Context, key CacheKey, external bool) (CacheEntry, bool, *cacheReservation, error) {
	localKey := fmt.Sprintf("%d:%s", key.Generation, key.Digest)
	if !c.options.DisableRequest {
		c.mu.Lock()
		if record, ok := c.entries[localKey]; ok {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				c.observe(CacheEvent{Kind: CacheWaitCancelEvent, Key: key})
				return CacheEntry{}, false, nil, ctx.Err()
			case <-record.ready:
				c.observe(CacheEvent{Kind: CacheHitEvent, Key: key})
				return record.entry, true, nil, nil
			}
		}
		record := &cacheRecord{ready: make(chan struct{})}
		c.entries[localKey] = record
		c.mu.Unlock()
		return c.loadExternal(ctx, key, localKey, external, record)
	}
	record := &cacheRecord{ready: make(chan struct{})}
	return c.loadExternal(ctx, key, localKey, external, record)
}

func (c *executionCache) loadExternal(ctx context.Context, key CacheKey, localKey string, external bool, record *cacheRecord) (CacheEntry, bool, *cacheReservation, error) {
	reservation := &cacheReservation{cache: c, ctx: ctx, key: key, localKey: localKey, record: record, external: external}
	if external {
		c.hookMu.Lock()
		entry, found, err := c.options.External.Load(ctx, key)
		c.hookMu.Unlock()
		if err == nil && found && cacheStoreable(c.options, entry.Value, entry.Error) {
			reservation.external = false
			reservation.publish(entry, true)
			c.observe(CacheEvent{Kind: CacheHitEvent, Key: key, External: true})
			return entry, true, nil, nil
		}
	}
	c.observe(CacheEvent{Kind: CacheMissEvent, Key: key, External: external})
	return CacheEntry{}, false, reservation, nil
}

func (r *cacheReservation) publish(entry CacheEntry, store bool) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.cache.mu.Lock()
		r.record.entry = entry
		store = store && r.key.Generation == r.cache.generation
		if !store {
			delete(r.cache.entries, r.localKey)
		}
		close(r.record.ready)
		r.cache.mu.Unlock()
		if store && r.external {
			r.cache.hookMu.Lock()
			r.cache.mu.Lock()
			current := r.key.Generation == r.cache.generation
			r.cache.mu.Unlock()
			if current {
				if err := r.cache.options.External.Store(r.ctx, r.key, entry); err == nil {
					r.cache.observe(CacheEvent{Kind: CacheStoreEvent, Key: r.key, External: true})
				}
			}
			r.cache.hookMu.Unlock()
		}
	})
}

func (c *executionCache) invalidate(ctx context.Context, plan *Plan, node planNode, principal Principal) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.generation++
	generation := c.generation
	c.entries = make(map[string]*cacheRecord)
	c.mu.Unlock()
	identity := authorizationDigest(principal)
	invalidation := CacheInvalidation{
		Operation: plan.operationName, OperationKind: plan.kind, Handler: registrationKey(node.definition.descriptor),
		SchemaRevision: c.options.SchemaRevision, AuthorizationIdentity: identity,
		Context: c.options.Context, Generation: generation,
	}
	c.observe(CacheEvent{Kind: CacheInvalidateEvent, Generation: generation})
	if c.options.External == nil || c.options.SchemaRevision == "" {
		return nil
	}
	c.hookMu.Lock()
	defer c.hookMu.Unlock()
	if err := c.options.External.Invalidate(ctx, invalidation); err != nil {
		return fmt.Errorf("invalidate application cache: %w", err)
	}
	return nil
}

func (c *executionCache) observe(event CacheEvent) {
	if c == nil {
		return
	}
	observeSafely(c.options.Observe, event)
}

func cacheStoreable(options CacheOptions, value any, err error) bool {
	if err != nil {
		return options.CacheErrors
	}
	if value == nil {
		return options.CacheNulls
	}
	return true
}
