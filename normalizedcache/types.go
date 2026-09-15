// Package normalizedcache provides the opt-in normalized cache used by
// official Naatre clients. It is independent of the core executor: applications
// that do not construct a Cache continue to consume raw response envelopes.
package normalizedcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

var (
	ErrUnknownEntity     = errors.New("normalized cache type is not a declared entity")
	ErrMissingIdentity   = errors.New("normalized cache entity identity field is absent")
	ErrRevisionConflict  = errors.New("normalized cache revision conflict")
	ErrStaleRevision     = errors.New("normalized cache update is stale")
	ErrUnknownMutation   = errors.New("normalized cache optimistic mutation is unknown")
	ErrDuplicateMutation = errors.New("normalized cache optimistic mutation already exists")
)

// Scope is the complete authorization boundary for client-held data. Subject,
// tenant, and authorization revision are all included even when empty, so an
// anonymous scope never collides with an authenticated scope.
type Scope struct {
	Subject               string `json:"subject"`
	Tenant                string `json:"tenant"`
	AuthorizationRevision string `json:"authorizationRevision"`
}

// EntityID is an opaque, comparable reference. Reference is stable across
// aliases and response paths; the separate scope digest prevents cache sharing
// across authorization boundaries. Neither digest exposes identity field data.
type EntityID struct {
	typeID      schema.TypeID
	reference   string
	scopeDigest string
}

// Reference returns a tracing-, audit-, and federation-safe opaque reference.
// It intentionally excludes authorization scope so the same entity can be
// correlated without disclosing its canonical identity fields.
func (id EntityID) Reference() string {
	if id.typeID == "" || id.reference == "" {
		return ""
	}
	return string(id.typeID) + "@" + id.reference
}

// Identify derives identity only for a schema-declared entity. fields is keyed
// by canonical schema field name, never a response alias.
func Identify(snapshot schema.Snapshot, typeID schema.TypeID, scope Scope, fields map[string]json.RawMessage) (EntityID, error) {
	descriptor, ok := snapshot.Lookup(typeID)
	if !ok || descriptor.Entity == nil || len(descriptor.Entity.Keys) == 0 {
		return EntityID{}, fmt.Errorf("%w: %s", ErrUnknownEntity, typeID)
	}
	keys := slices.Clone(descriptor.Entity.Keys)
	slices.Sort(keys)
	identity := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		raw, exists := fields[key]
		if !exists {
			return EntityID{}, fmt.Errorf("%w: %s.%s", ErrMissingIdentity, typeID, key)
		}
		field := descriptor.Fields[key]
		value, err := schema.CoerceInput(snapshot, field.Type, raw, false)
		if err != nil {
			return EntityID{}, fmt.Errorf("%w: %s.%s", ErrMissingIdentity, typeID, key)
		}
		canonical, err := value.MarshalJSON()
		if err != nil || string(canonical) == "null" {
			return EntityID{}, fmt.Errorf("%w: %s.%s", ErrMissingIdentity, typeID, key)
		}
		identity[key] = canonical
	}
	payload, err := json.Marshal(struct {
		Type schema.TypeID              `json:"type"`
		Keys map[string]json.RawMessage `json:"keys"`
	}{Type: typeID, Keys: identity})
	if err != nil {
		return EntityID{}, fmt.Errorf("encode entity identity: %w", err)
	}
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return EntityID{}, fmt.Errorf("canonicalize entity identity: %w", err)
	}
	return EntityID{typeID: typeID, reference: digest("entity-identity-1", canonical), scopeDigest: scopeKey(scope)}, nil
}

// FieldKey includes every representation input that may change a field value.
// Authorization scope is carried by EntityID rather than duplicated here.
type FieldKey struct {
	name            string
	argumentsDigest string
	locale          string
	representation  string
	schemaRevision  string
}

func NewFieldKey(name string, arguments json.RawMessage, locale, representation, schemaRevision string) (FieldKey, error) {
	if name == "" || schemaRevision == "" {
		return FieldKey{}, errors.New("normalized cache field name and schema revision are required")
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	canonical, err := canonicalJSON(arguments)
	if err != nil || len(canonical) == 0 || canonical[0] != '{' {
		return FieldKey{}, errors.New("normalized cache field arguments must be one JSON object")
	}
	return FieldKey{
		name: name, argumentsDigest: digest("field-arguments-1", canonical), locale: locale,
		representation: representation, schemaRevision: schemaRevision,
	}, nil
}

type FieldState string

const (
	FieldAbsent  FieldState = "absent"
	FieldSkipped FieldState = "skipped"
	FieldPending FieldState = "pending"
	FieldFailed  FieldState = "failed"
	FieldNull    FieldState = "null"
	FieldValue   FieldState = "value"
)

func (state FieldState) valid() bool {
	switch state {
	case FieldAbsent, FieldSkipped, FieldPending, FieldFailed, FieldNull, FieldValue:
		return true
	default:
		return false
	}
}

// Freshness expresses fresh and stale-while-revalidate bounds. A zero bound is
// unbounded; StaleUntil may not precede FreshUntil.
type Freshness struct {
	FreshUntil time.Time
	StaleUntil time.Time
}

func (f Freshness) valid() bool {
	return f.FreshUntil.IsZero() || f.StaleUntil.IsZero() || !f.StaleUntil.Before(f.FreshUntil)
}

type FreshnessState string

const (
	FreshnessFresh FreshnessState = "fresh"
	FreshnessStale FreshnessState = "stale"
	FreshnessMiss  FreshnessState = "miss"
)

type Cacheability string

const (
	CacheNoStore Cacheability = "no-store"
	CachePrivate Cacheability = "private"
)

// FieldPatch is one schema-name field observation. Absent, skipped, pending,
// and failed observations never erase a known value or explicit null.
type FieldPatch struct {
	Key          FieldKey
	State        FieldState
	Value        json.RawMessage
	Tags         []string
	Freshness    Freshness
	Cacheability Cacheability
}

// EntityPatch is one ordered partial observation. Revision is opaque; the
// explicit BaseRevision establishes lineage when advancing to a new revision.
type EntityPatch struct {
	Entity       EntityID
	Revision     string
	BaseRevision string
	Order        uint64
	Fields       []FieldPatch
}

type ConflictPolicy string

const (
	ConflictReject          ConflictPolicy = "reject"
	ConflictReplaceIncoming ConflictPolicy = "replace-incoming"
)

type MergeResult struct {
	Applied       []string
	Preserved     []string
	IgnoredFields []string
}

// Field is a detached cache view. OptimisticMutation is non-empty for a
// tentative overlay; Revision always remains the last committed revision.
type Field struct {
	State                 FieldState
	Value                 json.RawMessage
	Revision              string
	Freshness             Freshness
	OptimisticMutation    string
	PendingReconciliation bool
}

// RequestKey is an opaque normalized request identity bound to one scope and
// schema revision. Distinct variables, locales, or representations belong in
// the canonical input supplied to NewRequestKey.
type RequestKey struct {
	digest      string
	scopeDigest string
}

func NewRequestKey(scope Scope, schemaRevision string, input json.RawMessage) (RequestKey, error) {
	if schemaRevision == "" {
		return RequestKey{}, errors.New("normalized cache request schema revision is required")
	}
	canonical, err := canonicalJSON(input)
	if err != nil {
		return RequestKey{}, fmt.Errorf("canonicalize normalized request identity: %w", err)
	}
	payload, _ := json.Marshal(struct {
		Schema string          `json:"schemaRevision"`
		Input  json.RawMessage `json:"input"`
	}{schemaRevision, canonical})
	return RequestKey{digest: digest("normalized-request-1", payload), scopeDigest: scopeKey(scope)}, nil
}

type RequestEntry struct {
	Data      json.RawMessage
	Tags      []string
	Freshness Freshness
}

type MutationEffect struct {
	Scope    Scope
	Entities []EntityID
	Tags     []string
}

type OptimisticPatch struct {
	Entity           EntityID
	ExpectedRevision string
	Fields           []FieldPatch
}

type OptimisticMutation struct {
	ID      string
	Patches []OptimisticPatch
	Effect  MutationEffect
}

type MutationOutcome string

const (
	MutationCommitted MutationOutcome = "committed"
	MutationRejected  MutationOutcome = "rejected"
	MutationTimedOut  MutationOutcome = "timed-out"
	MutationUnknown   MutationOutcome = "unknown"
)

type MutationResolution struct {
	Outcome MutationOutcome
	Patches []EntityPatch
	Policy  ConflictPolicy
}

type OptimisticState string

const (
	OptimisticActive   OptimisticState = "active"
	OptimisticTimedOut OptimisticState = "timed-out"
	OptimisticUnknown  OptimisticState = "unknown"
)

type ScopeInvalidation string

const (
	ScopeLogout       ScopeInvalidation = "logout"
	ScopeRevoked      ScopeInvalidation = "revoked"
	ScopeTenantSwitch ScopeInvalidation = "tenant-switch"
)

func scopeKey(scope Scope) string {
	encoded, _ := json.Marshal(scope)
	return digest("authorization-scope-1", encoded)
}

func canonicalJSON(input []byte) ([]byte, error) {
	return protocol.CanonicalizeJSON(input, protocol.DefaultLimits())
}

func digest(domain string, value []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("naatre:" + domain + "\n"))
	_, _ = hash.Write(value)
	return hex.EncodeToString(hash.Sum(nil))
}

func normalizedTags(tags []string) ([]string, error) {
	result := slices.Clone(tags)
	slices.Sort(result)
	result = slices.Compact(result)
	for _, tag := range result {
		if tag == "" || strings.TrimSpace(tag) != tag {
			return nil, errors.New("normalized cache tag must be non-empty and trimmed")
		}
	}
	return result, nil
}
