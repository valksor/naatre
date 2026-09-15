package conformancerunner

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSecurityGatePublishesPinnedCrossProfileEvidence(t *testing.T) {
	runner, fixture, fixtureEvidence := loadSecurityGateForTest(t)
	if fixtureEvidence.Fixture != "v1/security-gate.json" || fixture.Profile != securityGateProfile {
		t.Fatalf("fixture/evidence = %#v %#v", fixture, fixtureEvidence)
	}
	evidence, err := runner.verifySecurityGateEvidence(fixture)
	if err != nil || len(evidence) < 20 {
		t.Fatalf("verifySecurityGateEvidence = %#v, %v", evidence, err)
	}
	assertSecurityGateResult(t, runner.verifySecurityGate(context.Background(), Request{}), len(evidence)+1)
}

func TestSecurityGateRejectsMissingAndFalseEvidence(t *testing.T) {
	_, fixture, _ := loadSecurityGateForTest(t)

	t.Run("missing profile", func(t *testing.T) {
		invalid := fixture
		invalid.Profiles = invalid.Profiles[:len(invalid.Profiles)-1]
		if err := validateSecurityGate(invalid); err == nil {
			t.Fatal("validateSecurityGate accepted missing profile")
		}
	})

	t.Run("premature handler", func(t *testing.T) {
		invalid := fixture
		invalid.Vectors = append([]securityGateVector(nil), fixture.Vectors...)
		invalid.Vectors[1].Expected.HandlerStarts = 1
		if err := validateSecurityGate(invalid); err == nil {
			t.Fatal("validateSecurityGate accepted protected work before authorization")
		}
	})

	t.Run("hidden identifier", func(t *testing.T) {
		invalid := fixture
		invalid.Vectors = append([]securityGateVector(nil), fixture.Vectors...)
		invalid.Vectors[2].Expected.HiddenIdentifiers = true
		if err := validateSecurityGate(invalid); err == nil {
			t.Fatal("validateSecurityGate accepted a hidden identifier in a public failure")
		}
	})
}

func TestSecurityGateDrivesRealRuntimeDecisions(t *testing.T) {
	_, fixture, _ := loadSecurityGateForTest(t)
	for _, vector := range fixture.Vectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			starts, code, safe, err := runSecurityGateVector(vector)
			if err != nil {
				t.Fatalf("runSecurityGateVector: %v", err)
			}
			// The observed outcome comes from a real runtime.Prepare/Execute, not
			// from the fixture's own declared fields.
			if starts != vector.Expected.HandlerStarts {
				t.Fatalf("real handler starts = %d, want %d", starts, vector.Expected.HandlerStarts)
			}
			if code != vector.Expected.Code {
				t.Fatalf("real public code = %q, want %q", code, vector.Expected.Code)
			}
			if !safe {
				t.Fatalf("runtime leaked failure metadata for %s", vector.Name)
			}
		})
	}
}

func loadSecurityGateForTest(t *testing.T) (*Runner, securityGateFixture, Evidence) {
	t.Helper()
	runner, err := New(filepath.Join("..", "..", "conformance", "v1", "suite.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixture, evidence, err := loadProfileFixture(runner, "v1/security-gate.json", func(fixture securityGateFixture) bool {
		return fixture.valid(runner.manifest.FixtureVersion, runner.manifest.ProfileRegistryVersion)
	})
	if err != nil {
		t.Fatalf("loadSecurityGateFixture: %v", err)
	}
	return runner, fixture, evidence
}

func assertSecurityGateResult(t *testing.T, result Result, evidenceCount int) {
	t.Helper()
	if result.Status != "passed" || len(result.Capabilities) != 1 || result.Capabilities[0] != securityGateProfile || len(result.Evidence) != evidenceCount {
		t.Fatalf("verifySecurityGate result = %#v", result)
	}
}
