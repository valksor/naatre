package conformance_test

import (
	"slices"
	"strings"
	"testing"
)

type observabilityFixture struct {
	Profile            string                       `json:"profile"`
	EventKinds         []string                     `json:"eventKinds"`
	AuditStages        []string                     `json:"auditStages"`
	Outcomes           []string                     `json:"outcomes"`
	ProhibitedFields   []string                     `json:"prohibitedFields"`
	MetricLabels       []string                     `json:"metricLabels"`
	MetricMeasurements []string                     `json:"metricMeasurements"`
	Vectors            []observabilityFixtureVector `json:"vectors"`
}

type observabilityFixtureVector struct {
	Name        string                     `json:"name"`
	Events      []string                   `json:"events"`
	Identities  []string                   `json:"identities"`
	Links       []observabilityFixtureLink `json:"links"`
	Causal      bool                       `json:"causal"`
	Terminal    string                     `json:"terminal"`
	Redacted    bool                       `json:"redacted"`
	Cardinality string                     `json:"cardinality"`
}

type observabilityFixtureLink struct {
	From int `json:"from"`
	To   int `json:"to"`
}

func TestObservabilityFixtureDefinesSafeCausalLifecycle(t *testing.T) {
	t.Parallel()
	var fixture observabilityFixture
	readFixture(t, "observability.json", &fixture)
	if fixture.Profile != "operations.observability-1" {
		t.Fatalf("observability profile = %q", fixture.Profile)
	}
	wantKinds := []string{"request", "planning", "operation", "handler", "batch", "retry", "transaction", "subscription"}
	if !slices.Equal(fixture.EventKinds, wantKinds) {
		t.Fatalf("event kinds = %v, want %v", fixture.EventKinds, wantKinds)
	}
	wantStages := []string{"attempted", "denied", "committed", "rolled-back", "compensated", "indeterminate"}
	if !slices.Equal(fixture.AuditStages, wantStages) {
		t.Fatalf("audit stages = %v, want %v", fixture.AuditStages, wantStages)
	}
	wantProhibited := []string{"variables", "arguments", "result", "authorization", "cookie", "password", "secret", "apiKey", "token", "session", "principal", "cursor", "topic", "handle"}
	if !slices.Equal(fixture.ProhibitedFields, wantProhibited) {
		t.Fatalf("prohibited fields = %v, want %v", fixture.ProhibitedFields, wantProhibited)
	}
	if want := []string{"kind", "stage", "operationKind", "outcome", "errorCode"}; !slices.Equal(fixture.MetricLabels, want) {
		t.Fatalf("metric labels = %v, want %v", fixture.MetricLabels, want)
	}
	if want := []string{"duration", "cost", "batchSize", "attempt", "connectionAttempt", "replayAttempt", "activeStreams", "replayEvents", "replayBytes", "scannedCandidates", "drainRemaining"}; !slices.Equal(fixture.MetricMeasurements, want) {
		t.Fatalf("metric measurements = %v, want %v", fixture.MetricMeasurements, want)
	}
	wantOutcomes := []string{"active", "completed", "failed", "cancelled", "denied", "history-unavailable", "authorization-expired", "slow-consumer", "broker-failure", "forced-drain"}
	if !slices.Equal(fixture.Outcomes, wantOutcomes) {
		t.Fatalf("outcomes = %v, want %v", fixture.Outcomes, wantOutcomes)
	}
	knownKinds := make(map[string]bool, len(fixture.EventKinds))
	for _, kind := range fixture.EventKinds {
		if kind == "" || knownKinds[kind] {
			t.Fatalf("invalid or duplicate event kind %q", kind)
		}
		knownKinds[kind] = true
	}
	knownStages := make(map[string]bool, len(fixture.AuditStages))
	for _, stage := range fixture.AuditStages {
		if stage == "" || knownStages[stage] {
			t.Fatalf("invalid or duplicate audit stage %q", stage)
		}
		knownStages[stage] = true
	}
	knownTerminals := make(map[string]bool, len(fixture.AuditStages)+len(fixture.Outcomes))
	for _, terminal := range fixture.AuditStages {
		knownTerminals[terminal] = true
	}
	seenOutcomes := make(map[string]bool, len(fixture.Outcomes))
	for _, terminal := range fixture.Outcomes {
		if terminal == "" || seenOutcomes[terminal] {
			t.Fatalf("invalid or duplicate outcome %q", terminal)
		}
		seenOutcomes[terminal] = true
		knownTerminals[terminal] = true
	}
	seenNames, seenKinds, seenStages := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || seenNames[vector.Name] || len(vector.Events) == 0 || len(vector.Identities) != len(vector.Events) || !vector.Causal || !knownTerminals[vector.Terminal] || !vector.Redacted || vector.Cardinality != "bounded" {
			t.Fatalf("invalid or duplicate observability vector %#v", vector)
		}
		seenNames[vector.Name] = true
		incoming := make([]bool, len(vector.Events))
		for index, event := range vector.Events {
			if vector.Identities[index] == "" {
				t.Fatalf("vector %q event %d has no causal identity", vector.Name, index)
			}
			kind, stage, ok := strings.Cut(event, ".")
			if !ok || !knownKinds[kind] || stage == "" {
				t.Fatalf("vector %q has invalid event %q", vector.Name, event)
			}
			seenKinds[kind] = true
			if kind == "transaction" {
				if !knownStages[stage] {
					t.Fatalf("vector %q has undeclared audit stage %q", vector.Name, stage)
				}
				seenStages[stage] = true
			}
		}
		for _, link := range vector.Links {
			if link.From < 0 || link.From >= link.To || link.To >= len(vector.Events) {
				t.Fatalf("vector %q has invalid causal link %#v", vector.Name, link)
			}
			incoming[link.To] = true
		}
		for index := 1; index < len(incoming); index++ {
			if !incoming[index] {
				t.Fatalf("vector %q event %d has no earlier causal link", vector.Name, index)
			}
		}
		reachable := observabilityCausalReachability(len(vector.Events), vector.Links)
		for from := range vector.Identities {
			for to := from + 1; to < len(vector.Identities); to++ {
				if vector.Identities[from] == vector.Identities[to] && !reachable[from][to] {
					t.Fatalf("vector %q identity %q has no causal path from %d to %d", vector.Name, vector.Identities[from], from, to)
				}
			}
		}
	}
	for kind := range knownKinds {
		if !seenKinds[kind] {
			t.Errorf("event kind %q has no vector", kind)
		}
	}
	for stage := range knownStages {
		if !seenStages[stage] {
			t.Errorf("audit stage %q has no vector", stage)
		}
	}
}

func observabilityCausalReachability(size int, links []observabilityFixtureLink) [][]bool {
	reachable := make([][]bool, size)
	for index := range reachable {
		reachable[index] = make([]bool, size)
	}
	for _, link := range links {
		reachable[link.From][link.To] = true
	}
	for middle := range size {
		for from := range size {
			for to := range size {
				reachable[from][to] = reachable[from][to] || reachable[from][middle] && reachable[middle][to]
			}
		}
	}
	return reachable
}
