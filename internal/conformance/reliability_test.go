package conformance_test

import (
	"slices"
	"testing"
)

type reliabilityFixture struct {
	Profile      string              `json:"profile"`
	Policies     []string            `json:"policies"`
	States       []string            `json:"states"`
	Durabilities []string            `json:"durabilities"`
	ErrorCodes   []string            `json:"errorCodes"`
	Vectors      []reliabilityVector `json:"vectors"`
}

type reliabilityVector struct {
	Name           string `json:"name"`
	Fault          string `json:"fault"`
	State          string `json:"state"`
	Code           string `json:"code"`
	Attempts       int    `json:"attempts"`
	ExecutesEffect bool   `json:"executesEffect"`
	Deduplicated   bool   `json:"deduplicated"`
}

func TestPortableReliabilityContractVectors(t *testing.T) {
	t.Parallel()
	var fixture reliabilityFixture
	readFixture(t, "reliability.json", &fixture)
	if fixture.Profile != "core.reliability-1" ||
		!slices.Equal(fixture.Policies, []string{"non-idempotent", "idempotent", "conditionally-idempotent"}) ||
		!slices.Equal(fixture.States, []string{"running", "completed", "indeterminate"}) ||
		!slices.Equal(fixture.Durabilities, []string{"process-local", "durable"}) {
		t.Fatal("reliability fixture header is incomplete")
	}
	required := map[string]bool{
		"handler-failure-retries": false, "transport-interruption-is-indeterminate": false,
		"concurrent-waiter-replays": false, "completed-record-expires": false,
		"running-lease-expires-indeterminate": false, "store-outage-fails-closed": false,
		"crash-after-effect-before-result": false, "stale-owner-is-fenced": false,
		"process-local-provider-restart": false, "mismatched-fingerprint-conflicts": false,
		"replay-is-reauthorized": false, "shared-budget-bounds-layers": false,
		"deadline-cancels-scheduling": false, "retry-after-lower-bound": false,
	}
	seen := make(map[string]bool, len(fixture.Vectors))
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || seen[vector.Name] || vector.State == "" || vector.Attempts < 0 {
			t.Fatalf("incomplete or duplicate reliability vector %q", vector.Name)
		}
		seen[vector.Name] = true
		if _, ok := required[vector.Name]; ok {
			required[vector.Name] = true
		}
	}
	for name, present := range required {
		if !present {
			t.Errorf("reliability fixture lacks %q", name)
		}
	}
	for _, code := range fixture.ErrorCodes {
		if code == "" {
			t.Fatal("reliability fixture contains an empty error code")
		}
	}
}
