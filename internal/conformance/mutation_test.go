package conformance_test

import (
	"slices"
	"testing"
)

type mutationFixture struct {
	Profile        string           `json:"profile"`
	AtomicityModes []string         `json:"atomicityModes"`
	CommitOutcomes []string         `json:"commitOutcomes"`
	EffectStates   []string         `json:"effectStates"`
	AuditStages    []string         `json:"auditStages"`
	ErrorCodes     []string         `json:"errorCodes"`
	Vectors        []mutationVector `json:"vectors"`
}

type mutationVector struct {
	Name          string   `json:"name"`
	Atomicity     string   `json:"atomicity"`
	Groups        []string `json:"groups"`
	Events        []string `json:"events"`
	Fault         string   `json:"fault"`
	Code          string   `json:"code"`
	Effect        string   `json:"effect"`
	PublishesData bool     `json:"publishesData"`
}

func TestPortableMutationContractVectors(t *testing.T) {
	t.Parallel()
	var fixture mutationFixture
	readFixture(t, "mutations.json", &fixture)
	if fixture.Profile != "core.mutation-1" || !slices.Equal(fixture.AtomicityModes, []string{"none", "operation", "group"}) || len(fixture.Vectors) == 0 {
		t.Fatal("mutation fixture header is incomplete")
	}
	seenNames, seenCodes, seenEffects := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || seenNames[vector.Name] || vector.Atomicity == "" || vector.Effect == "" {
			t.Fatalf("incomplete or duplicate mutation vector %q", vector.Name)
		}
		seenNames[vector.Name], seenEffects[vector.Effect] = true, true
		if vector.Code != "" {
			seenCodes[vector.Code] = true
		}
	}
	for _, required := range []string{"operation-commit", "named-groups-commit-independently", "commit-unknown", "rollback-failed", "outbox-failed", "after-commit-delivery-failed", "after-commit-delivery-duplicated", "external-effect-uncoordinated", "compensation-failed", "cancel-before-commit", "cancel-during-confirmed-commit", "nested-savepoint-unsupported"} {
		if !seenNames[required] {
			t.Errorf("mutation fixture lacks %q", required)
		}
	}
	for _, code := range fixture.ErrorCodes {
		if code == "" {
			t.Fatal("mutation fixture contains an empty error code")
		}
	}
	for _, effect := range fixture.EffectStates {
		if effect == "" {
			t.Fatal("mutation fixture contains an empty effect state")
		}
	}
	_ = seenCodes
	_ = seenEffects
}
