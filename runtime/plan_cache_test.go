package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestPlanCacheIsolatesRequestStateAcrossHits(t *testing.T) {
	t.Parallel()
	snapshot, calls := validationRegistry(t)
	cache, err := runtime.NewPlanCache(runtime.PlanCacheOptions{
		MaxEntries: 2,
		Revision:   runtime.PlanRevision{Schema: "schema-1", Policy: "policy-1"},
	})
	if err != nil {
		t.Fatalf("NewPlanCache: %v", err)
	}
	requestJSON := func(id, subject string, include bool) string {
		return `{"version":"1","id":"` + id + `","variables":{"include":` + map[bool]string{true: "true", false: "false"}[include] + `},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"include","type":"Boolean","required":true}],"select":[{"$call":{"name":"text","directives":[{"name":"include","arguments":{"if":{"$var":"include"}}}]}}]}]}}`
	}

	first, firstLookup, err := cache.Prepare(context.Background(), snapshot, decodeRuntimeRequest(t, requestJSON("request-secret-a", "principal-secret-a", true)), runtime.PrepareOptions{})
	if err != nil {
		t.Fatalf("Prepare first: %v", err)
	}
	second, secondLookup, err := cache.Prepare(context.Background(), snapshot, decodeRuntimeRequest(t, requestJSON("request-secret-b", "principal-secret-b", false)), runtime.PrepareOptions{})
	if err != nil {
		t.Fatalf("Prepare second: %v", err)
	}
	if first == second || firstLookup.Status != runtime.PlanCacheMiss || secondLookup.Status != runtime.PlanCacheHit {
		t.Fatalf("cache lookups = %#v %#v, plans share identity=%t", firstLookup, secondLookup, first == second)
	}

	first.Execute(runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "principal-secret-a", AuthorizationRevision: "policy-1"}))
	if got := calls.Load(); got != 1 {
		t.Fatalf("first request calls = %d, want 1", got)
	}
	second.Execute(runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "principal-secret-b", AuthorizationRevision: "policy-1"}))
	if got := calls.Load(); got != 1 {
		t.Fatalf("second request reused first variables: calls = %d", got)
	}

	encoded, err := json.Marshal(second.Description())
	if err != nil {
		t.Fatalf("marshal description: %v", err)
	}
	for _, secret := range []string{"request-secret-a", "request-secret-b", "principal-secret-a", "principal-secret-b"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("plan provenance exposed request state %q: %s", secret, encoded)
		}
	}
	stats := cache.Stats()
	if stats.Entries != 1 || stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("cache stats = %#v", stats)
	}
}

func TestPlanCacheRevisionInvalidationAndEvictionPreserveActivePlans(t *testing.T) {
	t.Parallel()
	snapshot, calls := validationRegistry(t)
	cache, err := runtime.NewPlanCache(runtime.PlanCacheOptions{
		MaxEntries: 1,
		Revision:   runtime.PlanRevision{Schema: "schema-1", Policy: "policy-1"},
	})
	if err != nil {
		t.Fatalf("NewPlanCache: %v", err)
	}
	firstRequest := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"First","kind":"query","select":[{"$call":{"name":"text"}}]}]}}`)
	first, _, err := cache.Prepare(context.Background(), snapshot, firstRequest, runtime.PrepareOptions{})
	if err != nil {
		t.Fatalf("Prepare first: %v", err)
	}
	secondRequest := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Second","kind":"query","select":[{"$call":{"name":"text","as":"second"}}]}]}}`)
	if _, _, err := cache.Prepare(context.Background(), snapshot, secondRequest, runtime.PrepareOptions{}); err != nil {
		t.Fatalf("Prepare second: %v", err)
	}
	if stats := cache.Stats(); stats.Entries != 1 || stats.Evictions != 1 {
		t.Fatalf("post-eviction stats = %#v", stats)
	}
	first.Execute(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("evicted active plan invoked %d handlers, want 1", got)
	}

	if err := cache.UpdateRevision(runtime.PlanRevision{Schema: "schema-2", Policy: "policy-2"}); err != nil {
		t.Fatalf("UpdateRevision: %v", err)
	}
	if stats := cache.Stats(); stats.Entries != 0 || stats.Invalidations != 1 || stats.Revision.Schema != "schema-2" || stats.Revision.Policy != "policy-2" {
		t.Fatalf("post-invalidation stats = %#v", stats)
	}
	if _, lookup, err := cache.Prepare(context.Background(), snapshot, secondRequest, runtime.PrepareOptions{}); err != nil || lookup.Status != runtime.PlanCacheMiss {
		t.Fatalf("Prepare revised = %#v, %v", lookup, err)
	}
}

func TestPlanCacheRevisionFenceRejectsStaleInflightFill(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	started := make(chan struct{})
	release := make(chan struct{})
	first := true
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		PlanningAuthorizer: runtime.PlanningAuthorizerFunc(func(runtime.PlanningAuthorizationRequest) (runtime.AuthorizationDecision, error) {
			if first {
				first = false
				close(started)
				<-release
			}
			return runtime.AuthorizationDecision{Allowed: true}, nil
		}),
	}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	descriptor := runtime.Descriptor{
		Name: "text", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) { return "ok", nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot := frozenRegistry(t, registry)
	cache, err := runtime.NewPlanCache(runtime.PlanCacheOptions{MaxEntries: 1, Revision: runtime.PlanRevision{Schema: "schema-1", Policy: "policy-1"}})
	if err != nil {
		t.Fatalf("NewPlanCache: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}}`)
	result := make(chan error, 1)
	go func() {
		_, _, prepareErr := cache.Prepare(context.Background(), snapshot, request, runtime.PrepareOptions{})
		result <- prepareErr
	}()
	<-started
	if err := cache.UpdateRevision(runtime.PlanRevision{Schema: "schema-2", Policy: "policy-2"}); err != nil {
		t.Fatalf("UpdateRevision: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("stale Prepare: %v", err)
	}
	if stats := cache.Stats(); stats.Entries != 0 || stats.Invalidations != 1 {
		t.Fatalf("stale compilation republished after revision change: %#v", stats)
	}
	if _, lookup, err := cache.Prepare(context.Background(), snapshot, request, runtime.PrepareOptions{}); err != nil || lookup.Status != runtime.PlanCacheMiss || cache.Stats().Entries != 1 {
		t.Fatalf("current revision prepare = %#v, %v, stats=%#v", lookup, err, cache.Stats())
	}
}

func TestPlanCacheOptimizationProvenancePreservesEffectBarriers(t *testing.T) {
	t.Parallel()
	snapshot, _ := validationRegistry(t)
	cache, err := runtime.NewPlanCache(runtime.PlanCacheOptions{
		MaxEntries: 2,
		Revision:   runtime.PlanRevision{Schema: "schema-1", Policy: "policy-1"},
	})
	if err != nil {
		t.Fatalf("NewPlanCache: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"write"}},{"$fragment":{"name":"Empty"}},{"$parallel":{"select":[]}}]}],"fragments":[{"name":"Empty","select":[]}]}}`)
	plan, lookup, err := cache.Prepare(context.Background(), snapshot, request, runtime.PrepareOptions{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	description := plan.Description()
	if lookup.TemplateDigest == "" || len(description.Transformations) < 2 {
		t.Fatalf("lookup/provenance = %#v %#v", lookup, description.Transformations)
	}
	if !slices.Contains(description.Nodes[0].Barriers, runtime.EffectBarrier) {
		t.Fatalf("write node lost effect barrier: %#v", description.Nodes[0])
	}
	if len(description.Nodes) != 1 || description.Transformations[0].Outcome != "applied" || description.Transformations[0].ChangedNodes != 1 ||
		description.Transformations[1].Outcome != "applied" || description.Transformations[1].ChangedNodes != 1 {
		t.Fatalf("conservative optimizer passes = nodes:%#v transformations:%#v", description.Nodes, description.Transformations)
	}
	for _, transformation := range description.Transformations {
		if transformation.Pass == "" || transformation.Version == "" || transformation.BeforeDigest == "" || transformation.AfterDigest == "" || !transformation.EffectBarriersPreserved {
			t.Fatalf("incomplete transformation provenance: %#v", transformation)
		}
	}

	copyDescription := plan.Description()
	copyDescription.Transformations[0].Pass = "mutated"
	if plan.Description().Transformations[0].Pass == "mutated" {
		t.Fatal("transformation provenance aliases plan storage")
	}
	encoded, err := json.Marshal(description.Transformations)
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("machine-readable provenance = %s, %v", encoded, err)
	}
}

func TestPlanCacheFailuresAreStableBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		options runtime.PlanCacheOptions
		code    string
		secret  string
	}{
		{name: "missing revision", options: runtime.PlanCacheOptions{MaxEntries: 1}, code: runtime.CodePlanCacheInvalid},
		{name: "capacity over portable limit", options: runtime.PlanCacheOptions{MaxEntries: runtime.MaxPlanCacheEntries + 1, Revision: runtime.PlanRevision{Schema: "schema", Policy: "policy"}}, code: runtime.CodePlanCacheResourceExhausted},
		{name: "protected revision", options: runtime.PlanCacheOptions{MaxEntries: 1, Revision: runtime.PlanRevision{Schema: "credential-secret\n", Policy: "policy"}}, code: runtime.CodePlanCacheInvalid, secret: "credential-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := runtime.NewPlanCache(test.options)
			var failure *runtime.PlanCacheError
			if !errors.As(err, &failure) || failure.Code != test.code {
				t.Fatalf("error = %T %v, want %s", err, err, test.code)
			}
			if test.secret != "" && strings.Contains(err.Error(), test.secret) {
				t.Fatalf("error exposed protected revision: %v", err)
			}
		})
	}

	snapshot, _ := validationRegistry(t)
	cache, err := runtime.NewPlanCache(runtime.PlanCacheOptions{MaxEntries: 1, Revision: runtime.PlanRevision{Schema: "schema", Policy: "policy"}})
	if err != nil {
		t.Fatalf("NewPlanCache: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	request := decodeRuntimeRequest(t, `{"version":"1","id":"credential-secret","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}}`)
	_, _, err = cache.Prepare(cancelled, snapshot, request, runtime.PrepareOptions{})
	var failure *runtime.PlanCacheError
	if !errors.As(err, &failure) || failure.Code != runtime.CodePlanCacheCancelled || strings.Contains(err.Error(), "credential-secret") {
		t.Fatalf("cancelled error = %T %v", err, err)
	}
}
