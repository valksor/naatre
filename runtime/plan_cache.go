package runtime

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/valksor/naatre/protocol"
)

const (
	DefaultPlanCacheEntries uint64 = 256
	MaxPlanCacheEntries     uint64 = 65_536

	CodePlanCacheInvalid           = "PLAN_CACHE_INVALID"
	CodePlanCacheCancelled         = "PLAN_CACHE_CANCELLED"
	CodePlanCacheResourceExhausted = "PLAN_CACHE_RESOURCE_EXHAUSTED"
	CodePlanPreparationFailed      = "PLAN_PREPARATION_FAILED"
	CodePlanOptimizerBarrier       = "PLAN_OPTIMIZER_BARRIER_VIOLATION"
)

// PlanRevision binds every cached template to the application's exact schema
// and authorization-policy revisions. Revision values are identities, not
// credentials, and are never included in public failures or provenance.
type PlanRevision struct {
	Schema string `json:"schema"`
	Policy string `json:"policy"`
}

// PlanCacheOptions configures one process-local cache. The cache has one
// active revision pair; UpdateRevision atomically retires all older entries.
type PlanCacheOptions struct {
	MaxEntries uint64
	Revision   PlanRevision
}

// PlanCacheError is the stable, redacted public failure shape for cache and
// optimizer operations.
type PlanCacheError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *PlanCacheError) Error() string {
	if e == nil {
		return ""
	}
	return (ExecutionError{Code: e.Code, Message: e.Message}).Error()
}

// PlanCacheStatus reports whether a request reused an immutable template.
type PlanCacheStatus string

const (
	PlanCacheHit  PlanCacheStatus = "hit"
	PlanCacheMiss PlanCacheStatus = "miss"
)

// PlanCacheLookup is safe machine-readable evidence for one lookup. The
// digest identifies the request-neutral template and contains no request ID,
// principal, claims, variable values, or extension payload.
type PlanCacheLookup struct {
	Status         PlanCacheStatus `json:"status"`
	TemplateDigest string          `json:"templateDigest"`
}

// PlanTransformation records one deterministic built-in optimizer pass.
type PlanTransformation struct {
	Pass                    string `json:"pass"`
	Version                 string `json:"version"`
	Outcome                 string `json:"outcome"`
	ChangedNodes            uint64 `json:"changedNodes"`
	BeforeDigest            string `json:"beforeDigest"`
	AfterDigest             string `json:"afterDigest"`
	EffectBarriersBefore    uint64 `json:"effectBarriersBefore"`
	EffectBarriersAfter     uint64 `json:"effectBarriersAfter"`
	EffectBarriersPreserved bool   `json:"effectBarriersPreserved"`
}

// PlanCacheStats is a point-in-time copy of bounded cache state.
type PlanCacheStats struct {
	Revision      PlanRevision `json:"revision"`
	Entries       uint64       `json:"entries"`
	Hits          uint64       `json:"hits"`
	Misses        uint64       `json:"misses"`
	Evictions     uint64       `json:"evictions"`
	Invalidations uint64       `json:"invalidations"`
}

type planCacheEntry struct {
	key      [sha256.Size]byte
	template *Plan
	digest   string
}

// PlanCache is a bounded process-local LRU of immutable request-neutral plan
// templates. Returned plans remain valid after eviction because eviction only
// drops the cache's reference; it never mutates a template.
type PlanCache struct {
	mu         sync.Mutex
	maxEntries uint64
	revision   PlanRevision
	generation uint64
	entries    map[[sha256.Size]byte]*list.Element
	recent     list.List
	hits       uint64
	misses     uint64
	evictions  uint64
	invalids   uint64
}

// NewPlanCache creates a bounded cache for one exact revision pair.
func NewPlanCache(options PlanCacheOptions) (*PlanCache, error) {
	if !validPlanRevision(options.Revision.Schema) || !validPlanRevision(options.Revision.Policy) {
		return nil, planCacheFailure(CodePlanCacheInvalid, "plan cache revision is invalid")
	}
	maximum := options.MaxEntries
	if maximum == 0 {
		maximum = DefaultPlanCacheEntries
	}
	if maximum > MaxPlanCacheEntries {
		return nil, planCacheFailure(CodePlanCacheResourceExhausted, "plan cache capacity exceeds the portable limit")
	}
	return &PlanCache{
		maxEntries: maximum,
		revision:   options.Revision,
		entries:    make(map[[sha256.Size]byte]*list.Element),
	}, nil
}

// UpdateRevision atomically retires every template compiled for an older
// schema or policy revision. An in-flight older compilation cannot republish
// into the revised cache because insertion is fenced by generation.
func (c *PlanCache) UpdateRevision(revision PlanRevision) error {
	if c == nil || !validPlanRevision(revision.Schema) || !validPlanRevision(revision.Policy) {
		return planCacheFailure(CodePlanCacheInvalid, "plan cache revision is invalid")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revision == revision {
		return nil
	}
	c.revision = revision
	c.generation++
	c.entries = make(map[[sha256.Size]byte]*list.Element)
	c.recent.Init()
	c.invalids++
	return nil
}

// Prepare returns a fresh request-bound Plan backed by an immutable cached
// template. Planning failures are either existing stable validation failures
// or cache-specific stable redacted errors.
func (c *PlanCache) Prepare(ctx context.Context, registry Snapshot, request *protocol.Request, options PrepareOptions) (*Plan, PlanCacheLookup, error) {
	if c == nil {
		return nil, PlanCacheLookup{}, planCacheFailure(CodePlanCacheInvalid, "plan cache is unavailable")
	}
	if ctx == nil {
		return nil, PlanCacheLookup{}, planCacheFailure(CodePlanCacheInvalid, "plan preparation context is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, PlanCacheLookup{}, planCacheFailure(CodePlanCacheCancelled, "plan preparation cancelled")
	}

	revision, generation, key, cached, digest, err := c.lookup(registry, request, options)
	if err != nil {
		return nil, PlanCacheLookup{}, err
	}
	if cached != nil {
		return instantiatePlanTemplate(cached, request), PlanCacheLookup{Status: PlanCacheHit, TemplateDigest: digest}, nil
	}

	prepared, err := PrepareWithOptions(registry, request, options)
	if err != nil {
		var validation *ValidationErrors
		if errors.As(err, &validation) {
			return nil, PlanCacheLookup{}, validation
		}
		return nil, PlanCacheLookup{}, planCacheFailure(CodePlanPreparationFailed, "plan preparation failed")
	}
	if err := ctx.Err(); err != nil {
		return nil, PlanCacheLookup{}, planCacheFailure(CodePlanCacheCancelled, "plan preparation cancelled")
	}
	template, err := makePlanTemplate(prepared)
	if err != nil {
		return nil, PlanCacheLookup{}, err
	}
	digest = planTemplateDigest(template)
	template, digest = c.publish(revision, generation, key, template, digest)
	return instantiatePlanTemplate(template, request), PlanCacheLookup{Status: PlanCacheMiss, TemplateDigest: digest}, nil
}

func (c *PlanCache) lookup(registry Snapshot, request *protocol.Request, options PrepareOptions) (PlanRevision, uint64, [sha256.Size]byte, *Plan, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	revision, generation := c.revision, c.generation
	key, err := makePlanCacheKey(registry, revision, request, options)
	if err != nil {
		c.misses++
		return revision, generation, key, nil, "", err
	}
	if element := c.entries[key]; element != nil {
		c.hits++
		c.recent.MoveToFront(element)
		entry := element.Value.(*planCacheEntry)
		return revision, generation, key, entry.template, entry.digest, nil
	}
	c.misses++
	return revision, generation, key, nil, "", nil
}

func (c *PlanCache) publish(revision PlanRevision, generation uint64, key [sha256.Size]byte, template *Plan, digest string) (*Plan, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation || c.revision != revision {
		return template, digest
	}
	if element := c.entries[key]; element != nil {
		c.recent.MoveToFront(element)
		entry := element.Value.(*planCacheEntry)
		return entry.template, entry.digest
	}
	element := c.recent.PushFront(&planCacheEntry{key: key, template: template, digest: digest})
	c.entries[key] = element
	if uint64(len(c.entries)) > c.maxEntries {
		oldest := c.recent.Back()
		entry := oldest.Value.(*planCacheEntry)
		delete(c.entries, entry.key)
		c.recent.Remove(oldest)
		c.evictions++
	}
	return template, digest
}

// Stats returns counters and the active revision without exposing cache keys.
func (c *PlanCache) Stats() PlanCacheStats {
	if c == nil {
		return PlanCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return PlanCacheStats{
		Revision: c.revision, Entries: uint64(len(c.entries)), Hits: c.hits, Misses: c.misses,
		Evictions: c.evictions, Invalidations: c.invalids,
	}
}

func makePlanCacheKey(registry Snapshot, revision PlanRevision, request *protocol.Request, options PrepareOptions) ([sha256.Size]byte, error) {
	if request == nil || request.Document() == nil {
		return [sha256.Size]byte{}, planCacheFailure(CodePlanPreparationFailed, "plan preparation failed")
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("naatre:runtime.go.plan-cache-1\n"))
	var identity [8]byte
	binary.BigEndian.PutUint64(identity[:], registry.cacheIdentity)
	_, _ = hash.Write(identity[:])
	writePlanCachePart(hash, revision.Schema)
	writePlanCachePart(hash, revision.Policy)
	writePlanCachePart(hash, request.OperationName())
	_, _ = hash.Write(request.Document().CanonicalJSON())
	capabilities := request.Capabilities()
	slices.Sort(capabilities)
	encoded, err := json.Marshal(struct {
		Capabilities []string                       `json:"capabilities"`
		Extensions   []protocol.NegotiatedExtension `json:"extensions"`
		Limits       ResourceLimits                 `json:"limits"`
	}{capabilities, request.NegotiatedExtensions(), options.Limits})
	if err != nil {
		return [sha256.Size]byte{}, planCacheFailure(CodePlanPreparationFailed, "plan preparation failed")
	}
	_, _ = hash.Write(encoded)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

type byteWriter interface {
	Write([]byte) (int, error)
}

func writePlanCachePart(output byteWriter, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = output.Write(size[:])
	_, _ = output.Write([]byte(value))
}

func makePlanTemplate(prepared *Plan) (*Plan, error) {
	template := *prepared
	template.variableValues = nil
	template.nodes = slices.Clone(prepared.nodes)
	transformations, err := optimizePlanTemplate(&template)
	if err != nil {
		return nil, err
	}
	template.transformations = transformations
	return &template, nil
}

func instantiatePlanTemplate(template *Plan, request *protocol.Request) *Plan {
	plan := *template
	plan.variableValues = captureVariableValues(request, template.variables)
	plan.transformations = clonePlanTransformations(template.transformations)
	return &plan
}

func optimizePlanTemplate(plan *Plan) ([]PlanTransformation, error) {
	passes := []struct {
		name string
		kind protocol.SelectionKind
	}{
		{name: "prune-empty-fragments", kind: protocol.FragmentSelection},
		{name: "prune-empty-parallel-groups", kind: protocol.ParallelSelection},
	}
	transformations := make([]PlanTransformation, 0, len(passes))
	for _, pass := range passes {
		beforeDigest := planTemplateDigest(plan)
		beforeSignature, beforeCount := effectBarrierSignature(plan.nodes)
		var changed uint64
		plan.nodes, changed = pruneEmptyPlanContainers(plan.nodes, pass.kind)
		afterSignature, afterCount := effectBarrierSignature(plan.nodes)
		preserved := beforeSignature == afterSignature
		outcome := "unchanged"
		if changed != 0 {
			outcome = "applied"
		}
		transformation := PlanTransformation{
			Pass: pass.name, Version: "1", Outcome: outcome, ChangedNodes: changed,
			BeforeDigest: beforeDigest, AfterDigest: planTemplateDigest(plan),
			EffectBarriersBefore: beforeCount, EffectBarriersAfter: afterCount,
			EffectBarriersPreserved: preserved,
		}
		if !preserved {
			return nil, planCacheFailure(CodePlanOptimizerBarrier, "plan optimization crossed an effect barrier")
		}
		transformations = append(transformations, transformation)
	}
	return transformations, nil
}

func pruneEmptyPlanContainers(nodes []planNode, kind protocol.SelectionKind) ([]planNode, uint64) {
	if len(nodes) == 0 {
		return nodes, 0
	}
	result := make([]planNode, 0, len(nodes))
	var changed uint64
	for _, node := range nodes {
		node.children, changed = pruneEmptyPlanContainerChildren(node.children, kind, changed)
		if node.kind == kind && len(node.children) == 0 && len(node.directives) == 0 && node.outputName == "" && node.binding == "" && !node.hasDefinition {
			changed++
			continue
		}
		result = append(result, node)
	}
	return result, changed
}

func pruneEmptyPlanContainerChildren(nodes []planNode, kind protocol.SelectionKind, changed uint64) ([]planNode, uint64) {
	pruned, childChanges := pruneEmptyPlanContainers(nodes, kind)
	return pruned, changed + childChanges
}

func effectBarrierSignature(nodes []planNode) (string, uint64) {
	var identities []string
	var walk func([]planNode)
	walk = func(current []planNode) {
		for _, node := range current {
			if node.hasDefinition && node.definition.descriptor.Metadata.Effect == WriteEffect {
				identities = append(identities, node.source.Pointer+"\x00"+registrationKey(node.definition.descriptor))
			}
			walk(node.children)
		}
	}
	walk(nodes)
	return strings.Join(identities, "\x01"), uint64(len(identities))
}

func planTemplateDigest(plan *Plan) string {
	if plan == nil {
		return ""
	}
	description := plan.Description()
	description.Transformations = nil
	encoded, err := json.Marshal(description)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func clonePlanTransformations(input []PlanTransformation) []PlanTransformation {
	return slices.Clone(input)
}

func validPlanRevision(value string) bool {
	return schemaIdentityPattern.MatchString(value)
}

func planCacheFailure(code, message string) *PlanCacheError {
	return &PlanCacheError{Code: code, Message: message}
}
