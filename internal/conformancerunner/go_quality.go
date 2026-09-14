package conformancerunner

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/valksor/naatre/internal/qualityharness"
)

const goQualityProfile = "quality.go.harness-1"

type goQualityFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	OwnerIssue   int    `json:"ownerIssue"`
	Package      string `json:"package"`
	Runtime      struct {
		Language  string   `json:"language"`
		MinimumGo string   `json:"minimumGo"`
		Platforms []string `json:"platforms"`
	} `json:"runtime"`
	Authority struct {
		Adversarial string `json:"adversarial"`
		Benchmarks  string `json:"benchmarks"`
	} `json:"authority"`
	Commands      []goQualityCommand `json:"commands"`
	Cases         []goQualityCase    `json:"cases"`
	EvidenceFiles []evidenceFile     `json:"evidenceFiles"`
	Unsupported   []string           `json:"unsupported"`
	PublicCodes   []string           `json:"publicCodes"`
}

type goQualityCommand struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type goQualityCase struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	FixtureRef string `json:"fixtureRef"`
	Code       string `json:"code"`
}

func (r *Runner) verifyGoQuality(ctx context.Context, _ Request) Result {
	fixture, fixtureEvidence, err := r.loadGoQualityFixture()
	if err != nil {
		return failureResult(goQualityProfile, "GO_QUALITY_FIXTURE_INVALID", "v1/go-quality.json")
	}
	evidence, _, err := r.verifyEvidenceFiles(fixture.EvidenceFiles)
	if err != nil {
		return failureResult(goQualityProfile, "GO_QUALITY_EVIDENCE_MISMATCH", "v1/go-quality.json")
	}
	if err := verifyGoQualityBehavior(ctx); err != nil {
		return failureResult(goQualityProfile, "GO_QUALITY_HARNESS_FAILED", "v1/go-quality.json")
	}
	result := emptyResult(goQualityProfile, "passed", "")
	result.Capabilities = []string{goQualityProfile}
	result.Data = map[string]any{
		"commands":    fixture.Commands,
		"platforms":   fixture.Runtime.Platforms,
		"unsupported": fixture.Unsupported,
	}
	result.Evidence = append([]Evidence{fixtureEvidence}, evidence...)
	return result
}

func (r *Runner) loadGoQualityFixture() (goQualityFixture, Evidence, error) {
	var fixture goQualityFixture
	_, evidence, err := r.loadPinnedFixture("v1/go-quality.json", &fixture)
	if err != nil {
		return goQualityFixture{}, Evidence{}, err
	}
	if fixture.Profile != goQualityProfile || fixture.FixtureSuite != r.manifest.FixtureVersion ||
		fixture.OwnerIssue != 78 || fixture.Package != "github.com/valksor/naatre/internal/qualityharness" ||
		fixture.Runtime.Language != "go" || fixture.Runtime.MinimumGo != "1.27" || len(fixture.Runtime.Platforms) != 5 ||
		fixture.Authority.Adversarial != "v1/adversarial.json" || fixture.Authority.Benchmarks != "v1/benchmarks.json" ||
		len(fixture.Commands) != 5 || len(fixture.Cases) != 5 || len(fixture.EvidenceFiles) < 8 || len(fixture.Unsupported) == 0 {
		return goQualityFixture{}, Evidence{}, errors.New("incompatible Go quality fixture")
	}
	kinds := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || testCase.FixtureRef == "" {
			return goQualityFixture{}, Evidence{}, errors.New("incomplete Go quality case")
		}
		kinds = append(kinds, testCase.Kind)
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"boundary", "cancellation", "negative", "positive", "resource-limit"}) {
		return goQualityFixture{}, Evidence{}, errors.New("incomplete Go quality case kinds")
	}
	for _, code := range []string{
		qualityharness.CodeInvalidConfiguration,
		qualityharness.CodeGoroutineLeak,
		qualityharness.CodeBodyLeak,
		qualityharness.CodeStreamLeak,
		qualityharness.CodeFaultInjected,
		qualityharness.CodeBudgetExceeded,
	} {
		if !slices.Contains(fixture.PublicCodes, code) {
			return goQualityFixture{}, Evidence{}, errors.New("incomplete Go quality public codes")
		}
	}
	return fixture, evidence, nil
}

func verifyGoQualityBehavior(ctx context.Context) error {
	var tracker qualityharness.Tracker
	releaseBody, err := tracker.Acquire(qualityharness.Body)
	if err != nil {
		return err
	}
	if qualityharness.Code(tracker.Check()) != qualityharness.CodeBodyLeak {
		return errors.New("body leak was not detected")
	}
	releaseBody()
	if err := tracker.Check(); err != nil {
		return err
	}

	injector, err := qualityharness.NewFaultInjector(map[string][]uint64{"runtime.invoke": {2}})
	if err != nil || injector.Inject("runtime.invoke") != nil {
		return errors.New("fault injector setup failed")
	}
	if qualityharness.Code(injector.Inject("runtime.invoke")) != qualityharness.CodeFaultInjected {
		return errors.New("fault schedule did not trigger")
	}

	caseContext, cancel := context.WithCancel(ctx)
	started := make(chan struct{})
	if err := tracker.Go(caseContext, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}); err != nil {
		cancel()
		return err
	}
	select {
	case <-started:
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
	cancel()
	waitContext, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	return tracker.Wait(waitContext)
}
