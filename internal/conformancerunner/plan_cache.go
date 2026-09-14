package conformancerunner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const planCacheProfile = "runtime.go.plan-cache-1"

type planCacheFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Runtime      struct {
		Module    string `json:"module"`
		GoVersion string `json:"goVersion"`
		Platform  string `json:"platform"`
	} `json:"runtime"`
	Dependencies struct {
		CoreProfile string       `json:"coreProfile"`
		Planning    evidenceFile `json:"planning"`
		GoMod       evidenceFile `json:"goMod"`
		GoSum       evidenceFile `json:"goSum"`
	} `json:"dependencies"`
	Evidence    []evidenceFile `json:"evidence"`
	Vectors     []string       `json:"vectors"`
	Unsupported []string       `json:"unsupported"`
}

func (r *Runner) verifyPlanCache(ctx context.Context, _ Request) Result {
	fixture, fixtureEvidence, err := r.loadPlanCacheFixture()
	if err != nil {
		return failureResult(planCacheProfile, "PLAN_CACHE_FIXTURE_INVALID", "v1/plan-cache.json")
	}
	evidence, err := r.verifyPlanCacheEvidence(fixture)
	if err != nil {
		return failureResult(planCacheProfile, "PLAN_CACHE_EVIDENCE_MISMATCH", "v1/plan-cache.json")
	}
	if err := verifyPlanCacheRuntime(ctx); err != nil {
		return failureResult(planCacheProfile, "PLAN_CACHE_RUNTIME_FAILED", "v1/plan-cache.json")
	}
	return passedPlanCacheResult(fixtureEvidence, evidence)
}

func (r *Runner) loadPlanCacheFixture() (planCacheFixture, Evidence, error) {
	return loadProfileFixture(r, "v1/plan-cache.json", func(fixture planCacheFixture) bool {
		return fixture.valid(r.manifest.FixtureVersion)
	})
}

func (r *Runner) verifyPlanCacheEvidence(fixture planCacheFixture) ([]Evidence, error) {
	files := []evidenceFile{fixture.Dependencies.Planning, fixture.Dependencies.GoMod, fixture.Dependencies.GoSum}
	files = append(files, fixture.Evidence...)
	evidence, _, err := r.verifyEvidenceFiles(files)
	return evidence, err
}

func verifyPlanCacheRuntime(ctx context.Context) error {
	fixture, err := newPlanCacheRuntimeFixture()
	if err != nil {
		return err
	}
	checks := []func(context.Context) error{
		fixture.verifyIsolation,
		fixture.verifyOptimizationAndLifecycle,
		fixture.verifyFailures,
	}
	for _, check := range checks {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return nil
}

type planCacheRuntimeFixture struct {
	snapshot     naatreruntime.Snapshot
	cache        *naatreruntime.PlanCache
	calls        atomic.Int64
	first        *naatreruntime.Plan
	firstRequest *protocol.Request
}

func newPlanCacheRuntimeFixture() (*planCacheRuntimeFixture, error) {
	catalog := schema.NewCatalog()
	types, err := catalog.Freeze()
	if err != nil {
		return nil, err
	}
	registry := naatreruntime.NewRegistry(types)
	fixture := &planCacheRuntimeFixture{}
	metadata := func(effect naatreruntime.Effect) naatreruntime.Metadata {
		return naatreruntime.Metadata{
			Effect: effect, Deterministic: true, Cacheable: effect == naatreruntime.ReadEffect, RetrySafe: true,
			ThreadSafety: naatreruntime.ThreadSafe, Batching: naatreruntime.BatchIneligible,
			Transaction: naatreruntime.TransactionNone, AuthorizationPolicy: "conformance",
		}
	}
	for _, definition := range []naatreruntime.Definition{
		naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
			Name: "text", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata(naatreruntime.ReadEffect),
		}, func(context.Context, naatreruntime.Invocation) (string, error) {
			fixture.calls.Add(1)
			return "ok", nil
		}),
		naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
			Name: "write", Scope: naatreruntime.RootScope, Kind: protocol.Mutation, Member: naatreruntime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata(naatreruntime.WriteEffect),
		}, func(context.Context, naatreruntime.Invocation) (string, error) { return "ok", nil }),
	} {
		if err := registry.Register(definition); err != nil {
			return nil, err
		}
	}
	fixture.snapshot, err = registry.Freeze()
	if err != nil {
		return nil, err
	}
	fixture.cache, err = naatreruntime.NewPlanCache(naatreruntime.PlanCacheOptions{
		MaxEntries: 1, Revision: naatreruntime.PlanRevision{Schema: "schema-1", Policy: "policy-1"},
	})
	if err != nil {
		return nil, err
	}
	return fixture, nil
}

func (f *planCacheRuntimeFixture) verifyIsolation(ctx context.Context) error {
	var err error
	f.firstRequest, err = decodePlanCacheRequest(`{"version":"1","variables":{"run":true},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"run","type":"Boolean","required":true}],"select":[{"$call":{"name":"text","directives":[{"name":"include","arguments":{"if":{"$var":"run"}}}]}}]}]}}`)
	if err != nil {
		return err
	}
	secondRequest, err := decodePlanCacheRequest(`{"version":"1","variables":{"run":false},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"run","type":"Boolean","required":true}],"select":[{"$call":{"name":"text","directives":[{"name":"include","arguments":{"if":{"$var":"run"}}}]}}]}]}}`)
	if err != nil {
		return err
	}
	var firstLookup naatreruntime.PlanCacheLookup
	f.first, firstLookup, err = f.cache.Prepare(ctx, f.snapshot, f.firstRequest, naatreruntime.PrepareOptions{})
	if err != nil {
		return err
	}
	second, secondLookup, err := f.cache.Prepare(ctx, f.snapshot, secondRequest, naatreruntime.PrepareOptions{})
	if err != nil {
		return err
	}
	f.first.Execute(naatreruntime.WithPrincipal(ctx, naatreruntime.Principal{Subject: "one", AuthorizationRevision: "policy-1"}))
	second.Execute(naatreruntime.WithPrincipal(ctx, naatreruntime.Principal{Subject: "two", AuthorizationRevision: "policy-1"}))
	if f.first == second || firstLookup.Status != naatreruntime.PlanCacheMiss || secondLookup.Status != naatreruntime.PlanCacheHit || f.calls.Load() != 1 {
		return errors.New("request-state isolation failed")
	}
	return nil
}

func (f *planCacheRuntimeFixture) verifyOptimizationAndLifecycle(ctx context.Context) error {
	mutationRequest, err := decodePlanCacheRequest(`{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"write"}}]}]}}`)
	if err != nil {
		return err
	}
	mutation, _, err := f.cache.Prepare(ctx, f.snapshot, mutationRequest, naatreruntime.PrepareOptions{})
	if err != nil {
		return err
	}
	description := mutation.Description()
	if len(description.Transformations) != 2 || len(description.Nodes) != 1 {
		return errors.New("optimizer provenance is incomplete")
	}
	for _, transformation := range description.Transformations {
		if !transformation.EffectBarriersPreserved || transformation.EffectBarriersBefore != 1 || transformation.EffectBarriersAfter != 1 {
			return errors.New("optimizer effect barrier was not preserved")
		}
	}
	f.first.Execute(ctx)
	if f.calls.Load() != 2 {
		return errors.New("evicted active plan was mutated")
	}
	if err := f.cache.UpdateRevision(naatreruntime.PlanRevision{Schema: "schema-2", Policy: "policy-2"}); err != nil || f.cache.Stats().Entries != 0 {
		return errors.New("revision invalidation failed")
	}
	return nil
}

func (f *planCacheRuntimeFixture) verifyFailures(ctx context.Context) error {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err := f.cache.Prepare(cancelled, f.snapshot, f.firstRequest, naatreruntime.PrepareOptions{})
	var cacheFailure *naatreruntime.PlanCacheError
	if !errors.As(err, &cacheFailure) || cacheFailure.Code != naatreruntime.CodePlanCacheCancelled {
		return errors.New("cancellation classification failed")
	}
	if _, err := naatreruntime.NewPlanCache(naatreruntime.PlanCacheOptions{
		MaxEntries: naatreruntime.MaxPlanCacheEntries + 1,
		Revision:   naatreruntime.PlanRevision{Schema: "schema", Policy: "policy"},
	}); !errors.As(err, &cacheFailure) || cacheFailure.Code != naatreruntime.CodePlanCacheResourceExhausted {
		return fmt.Errorf("capacity classification failed")
	}
	return nil
}

func decodePlanCacheRequest(input string) (*protocol.Request, error) {
	return protocol.DecodeRequest([]byte(input), protocol.DecodeOptions{})
}

func (f planCacheFixture) valid(fixtureVersion string) bool {
	requiredVectors := []string{
		"request-state-isolation", "revision-invalidation", "active-plan-eviction", "effect-barrier-provenance",
		"stable-redacted-failures", "cancellation", "capacity-boundary",
	}
	return f.Profile == planCacheProfile && f.FixtureSuite == fixtureVersion &&
		f.Runtime.Module == "github.com/valksor/naatre/runtime" && f.Runtime.GoVersion == "1.27.1" &&
		f.Dependencies.CoreProfile == "core.language-1" && len(f.Evidence) != 0 &&
		slices.Equal(f.Vectors, requiredVectors) && len(f.Unsupported) != 0
}

func passedPlanCacheResult(fixture Evidence, evidence []Evidence) Result {
	result := emptyResult(planCacheProfile, "passed", "")
	result.Capabilities = []string{planCacheProfile}
	result.Evidence = append([]Evidence{fixture}, evidence...)
	return result
}
