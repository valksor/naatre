package conformancerunner

import (
	"context"
	"errors"
	"slices"
	"time"

	naatreruntime "github.com/valksor/naatre/runtime"
)

const (
	operationsProfile            = "operations.lifecycle-1"
	operationsDependencyRevision = "81000002f89bac6a342036d4fd34c35506292b34"
)

type operationsProfileFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Integration  struct {
		Module        string                 `json:"module"`
		MinimumGo     string                 `json:"minimumGo"`
		Runtime       string                 `json:"runtime"`
		Dependencies  []operationsDependency `json:"dependencies"`
		Packages      []operationsPackage    `json:"packages"`
		Supported     []string               `json:"supported"`
		FailureCodes  []string               `json:"failureCodes"`
		Unsupported   []string               `json:"unsupported"`
		Commands      []string               `json:"commands"`
		EvidenceFiles []evidenceFile         `json:"evidence"`
	} `json:"integration"`
	IntegrationCases []operationsIntegrationCase `json:"integrationCases"`
}

type operationsDependency struct {
	Issue    int    `json:"issue"`
	Revision string `json:"revision"`
	Profile  string `json:"profile"`
}

type operationsPackage struct {
	Path string `json:"path"`
	Role string `json:"role"`
}

type operationsIntegrationCase struct {
	Name            string                 `json:"name"`
	Classifications []string               `json:"classifications"`
	States          []string               `json:"states"`
	Health          []operationsCaseHealth `json:"health"`
	Code            string                 `json:"code"`
	ProtectedOutput bool                   `json:"protectedOutput"`
}

type operationsCaseHealth struct {
	State string `json:"state"`
	Ready bool   `json:"ready"`
	Live  bool   `json:"live"`
}

func (r *Runner) verifyOperations(_ context.Context, _ Request) Result {
	fixture, fixtureEvidence, err := r.loadOperationsFixture()
	if err != nil {
		return failureResult(operationsProfile, "OPERATIONS_FIXTURE_INVALID", "v1/operations.json")
	}
	evidence, _, err := r.verifyEvidenceFiles(fixture.Integration.EvidenceFiles)
	if err != nil {
		return failureResult(operationsProfile, "OPERATIONS_EVIDENCE_MISMATCH", "v1/operations.json")
	}
	if err := verifyOperationsBehavior(); err != nil {
		return failureResult(operationsProfile, "OPERATIONS_BEHAVIOR_FAILED", "v1/operations.json")
	}
	result := emptyResult(operationsProfile, "passed", "")
	result.Capabilities = []string{operationsProfile}
	result.Evidence = append([]Evidence{fixtureEvidence}, evidence...)
	return result
}

func (r *Runner) loadOperationsFixture() (operationsProfileFixture, Evidence, error) {
	var fixture operationsProfileFixture
	_, evidence, err := r.loadPinnedFixture("v1/operations.json", &fixture)
	if err != nil {
		return operationsProfileFixture{}, Evidence{}, err
	}
	if fixture.Profile != operationsProfile || fixture.FixtureSuite != r.manifest.FixtureVersion ||
		fixture.Integration.Module != "github.com/valksor/naatre" || fixture.Integration.MinimumGo != "1.27" ||
		fixture.Integration.Runtime != "go-standard-library" || len(fixture.Integration.Dependencies) != 1 ||
		fixture.Integration.Dependencies[0] != (operationsDependency{Issue: 57, Revision: operationsDependencyRevision, Profile: operationsProfile}) ||
		len(fixture.Integration.Packages) != 3 || len(fixture.Integration.Supported) == 0 ||
		!slices.Equal(fixture.Integration.FailureCodes, []string{naatreruntime.CodeOverloaded, naatreruntime.CodeRateLimited, naatreruntime.CodeCancelled, naatreruntime.CodeResourceExhausted, naatreruntime.CodeInternal}) ||
		len(fixture.Integration.Unsupported) == 0 || len(fixture.Integration.Commands) == 0 ||
		len(fixture.Integration.EvidenceFiles) == 0 {
		return operationsProfileFixture{}, Evidence{}, errors.New("incompatible operations fixture")
	}
	if err := validateOperationsCases(fixture.IntegrationCases); err != nil {
		return operationsProfileFixture{}, Evidence{}, err
	}
	return fixture, evidence, nil
}

func validateOperationsCases(cases []operationsIntegrationCase) error {
	want := map[string]struct {
		states []string
		health []operationsCaseHealth
		code   string
	}{
		"overload": {
			states: []string{"ready", "ready"},
			health: []operationsCaseHealth{{State: "ready", Ready: true, Live: true}, {State: "ready", Ready: true, Live: true}},
			code:   naatreruntime.CodeOverloaded,
		},
		"dependency-failure": {
			states: []string{"ready", "ready", "ready", "ready"},
			health: []operationsCaseHealth{
				{State: "ready", Ready: true, Live: true},
				{State: "ready", Live: true},
				{State: "ready"},
				{State: "ready", Ready: true, Live: true},
			},
			code: naatreruntime.CodeOverloaded,
		},
		"rolling-restart": {
			states: []string{"ready", "draining", "stopped"},
			health: []operationsCaseHealth{{State: "ready", Ready: true, Live: true}, {State: "draining", Live: true}, {State: "stopped"}},
		},
		"reconnect-storm": {
			states: []string{"ready", "ready"},
			health: []operationsCaseHealth{{State: "ready", Ready: true, Live: true}, {State: "ready", Ready: true, Live: true}},
		},
		"drain-deadline": {
			states: []string{"ready", "draining", "forced"},
			health: []operationsCaseHealth{{State: "ready", Ready: true, Live: true}, {State: "draining", Live: true}, {State: "forced"}},
			code:   naatreruntime.CodeResourceExhausted,
		},
		"forced-kill": {
			states: []string{"starting", "ready", "draining", "forced"},
			health: []operationsCaseHealth{{State: "starting", Live: true}, {State: "ready", Ready: true, Live: true}, {State: "draining", Live: true}, {State: "forced"}},
			code:   naatreruntime.CodeResourceExhausted,
		},
	}
	if len(cases) != len(want) {
		return errors.New("operations integration case inventory is incomplete")
	}
	seenClassifications := make(map[string]bool)
	for _, testCase := range cases {
		expected, ok := want[testCase.Name]
		if !ok || !slices.Equal(testCase.States, expected.states) || !slices.Equal(testCase.Health, expected.health) || testCase.Code != expected.code ||
			testCase.ProtectedOutput || len(testCase.Classifications) == 0 {
			return errors.New("operations integration case is incompatible")
		}
		for index, health := range testCase.Health {
			if health.State != testCase.States[index] {
				return errors.New("operations integration health transition is incompatible")
			}
		}
		delete(want, testCase.Name)
		for _, classification := range testCase.Classifications {
			seenClassifications[classification] = true
		}
	}
	for _, classification := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit"} {
		if !seenClassifications[classification] {
			return errors.New("operations integration classifications are incomplete")
		}
	}
	return nil
}

func verifyOperationsBehavior() error {
	now := time.Unix(1_800_000_000, 0)
	controller, err := newOperationsEvidenceController(&now)
	if err != nil {
		return err
	}
	if err := verifyOperationsDependencyBehavior(controller, &now); err != nil {
		return err
	}
	if err := verifyOperationsConnectionBehavior(controller, now); err != nil {
		return err
	}
	return verifyOperationsForcedDrainBehavior(controller)
}

func newOperationsEvidenceController(now *time.Time) (*naatreruntime.ProcessController, error) {
	config := naatreruntime.DefaultProcessConfig()
	config.Clock = func() time.Time { return *now }
	config.DependencyFailureGrace = time.Second
	config.MaxConnectionLifetime = time.Hour
	config.ReconnectStagger = 10 * time.Minute
	config.Dependencies = []naatreruntime.DependencyConfig{{Name: "private-dependency", Essential: true, Ready: true}}
	controller, err := naatreruntime.NewProcessController(config, naatreruntime.ProcessRevision{
		Revision: "runtime-r1", SchemaRevision: "schema-r1", ConfigurationRevision: "configuration-r1",
	})
	if err != nil {
		return nil, err
	}
	if health := controller.Health(); health.State != naatreruntime.ProcessStarting || health.Ready || !health.Live {
		return nil, errors.New("operations startup health mismatch")
	}
	if err := controller.Start(); err != nil {
		return nil, err
	}
	return controller, nil
}

func verifyOperationsDependencyBehavior(controller *naatreruntime.ProcessController, now *time.Time) error {
	if err := controller.SetDependency("private-dependency", false); err != nil {
		return err
	}
	_, err := controller.Admit(context.Background(), naatreruntime.AdmissionRequest{
		TenantReference: "private-tenant", PrincipalReference: "private-principal", Kind: naatreruntime.WorkQuery,
		ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
	})
	var rejected *naatreruntime.AdmissionError
	if !errors.As(err, &rejected) || rejected.Code != naatreruntime.CodeOverloaded {
		return errors.New("operations dependency admission mismatch")
	}
	if health := controller.Health(); health.Ready || !health.Live || health.EssentialUnavailable != 1 {
		return errors.New("operations transient dependency health mismatch")
	}
	*now = now.Add(time.Second)
	if health := controller.Health(); health.Ready || health.Live || health.EssentialUnavailable != 1 {
		return errors.New("operations sustained dependency health mismatch")
	}
	if err := controller.SetDependency("private-dependency", true); err != nil {
		return err
	}
	if health := controller.Health(); !health.Ready || !health.Live || health.Unavailable != 0 {
		return errors.New("operations dependency recovery mismatch")
	}
	return nil
}

func verifyOperationsConnectionBehavior(controller *naatreruntime.ProcessController, established time.Time) error {
	first := controller.ConnectionDeadline("opaque-connection-a", established, time.Time{})
	second := controller.ConnectionDeadline("opaque-connection-b", established, time.Time{})
	minimum := established.Add(50 * time.Minute)
	maximum := established.Add(time.Hour)
	if first.Before(minimum) || first.After(maximum) || second.Before(minimum) || second.After(maximum) || first.Equal(second) {
		return errors.New("operations connection deadline mismatch")
	}
	if repeated := controller.ConnectionDeadline("opaque-connection-a", established, time.Time{}); !repeated.Equal(first) {
		return errors.New("operations connection stagger is not deterministic")
	}
	return nil
}

func verifyOperationsForcedDrainBehavior(controller *naatreruntime.ProcessController) error {
	lease, err := controller.Admit(context.Background(), naatreruntime.AdmissionRequest{
		TenantReference: "private-tenant", PrincipalReference: "private-principal", Kind: naatreruntime.WorkDurable,
		ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
	})
	if err != nil {
		return err
	}
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	cancelDrain()
	if outcome := controller.Drain(drainCtx); outcome != naatreruntime.DrainForced {
		lease.Release()
		return errors.New("operations forced drain mismatch")
	}
	lease.Release()
	if health := controller.Health(); health.State != naatreruntime.ProcessForced || health.Ready || health.Live {
		return errors.New("operations forced health mismatch")
	}
	return nil
}
