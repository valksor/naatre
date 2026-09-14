package conformance_test

import (
	"slices"
	"testing"
)

type asyncOperationFixture struct {
	Profile              string                            `json:"profile"`
	States               []string                          `json:"states"`
	TerminalStates       []string                          `json:"terminalStates"`
	Transitions          []asyncOperationTransition        `json:"transitions"`
	CancellationOutcomes []string                          `json:"cancellationOutcomes"`
	Bindings             []string                          `json:"bindings"`
	HTTP                 asyncOperationHTTPFixture         `json:"http"`
	LogicalStateSource   string                            `json:"logicalStateSource"`
	Vectors              []asyncOperationConformanceVector `json:"vectors"`
}

type asyncOperationTransition struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type asyncOperationHTTPFixture struct {
	AcceptanceStatus  int    `json:"acceptanceStatus"`
	PollStatus        int    `json:"pollStatus"`
	ConditionalStatus int    `json:"conditionalStatus"`
	Location          string `json:"location"`
	RetryAfter        string `json:"retryAfter"`
	ETag              string `json:"etag"`
}

type asyncOperationConformanceVector struct {
	Name                 string   `json:"name"`
	Events               []string `json:"events"`
	FinalState           string   `json:"finalState"`
	ExpectedCancellation string   `json:"expectedCancellation,omitempty"`
	SameHandle           bool     `json:"sameHandle,omitempty"`
	Reauthorized         bool     `json:"reauthorized"`
	TerminalRevision     uint64   `json:"terminalRevision,omitempty"`
}

func TestAsyncOperationFixtureDefinesDurableRaceSafeContract(t *testing.T) {
	t.Parallel()
	var fixture asyncOperationFixture
	readFixture(t, "async-operations.json", &fixture)
	if fixture.Profile != "operations.async-1" {
		t.Fatalf("profile = %q", fixture.Profile)
	}
	wantStates := []string{"pending", "running", "succeeded", "failed", "cancelling", "cancelled", "indeterminate"}
	if !slices.Equal(fixture.States, wantStates) {
		t.Fatalf("states = %v, want %v", fixture.States, wantStates)
	}
	wantTerminals := []string{"succeeded", "failed", "cancelled", "indeterminate"}
	if !slices.Equal(fixture.TerminalStates, wantTerminals) {
		t.Fatalf("terminal states = %v, want %v", fixture.TerminalStates, wantTerminals)
	}
	allowed := map[string]map[string]bool{
		"pending":    {"running": true, "cancelled": true},
		"running":    {"succeeded": true, "failed": true, "cancelling": true, "indeterminate": true},
		"cancelling": {"succeeded": true, "failed": true, "cancelled": true, "indeterminate": true},
	}
	seenTransitions := map[string]bool{}
	for _, transition := range fixture.Transitions {
		key := transition.From + "->" + transition.To
		if seenTransitions[key] || !allowed[transition.From][transition.To] {
			t.Fatalf("invalid or duplicate transition %q", key)
		}
		seenTransitions[key] = true
	}
	for from, targets := range allowed {
		for to := range targets {
			if !seenTransitions[from+"->"+to] {
				t.Errorf("missing transition %s -> %s", from, to)
			}
		}
	}
	if want := []string{"requested", "effective", "too-late", "unsupported"}; !slices.Equal(fixture.CancellationOutcomes, want) {
		t.Fatalf("cancellation outcomes = %v, want %v", fixture.CancellationOutcomes, want)
	}
	if want := []string{"principal", "tenant", "schemaRevision", "operation", "idempotencyKey"}; !slices.Equal(fixture.Bindings, want) {
		t.Fatalf("bindings = %v, want %v", fixture.Bindings, want)
	}
	if fixture.HTTP.AcceptanceStatus != 202 || fixture.HTTP.PollStatus != 200 || fixture.HTTP.ConditionalStatus != 304 ||
		fixture.HTTP.Location != "poll-resource" || fixture.HTTP.RetryAfter != "required-while-active" || fixture.HTTP.ETag != "state-revision" {
		t.Fatalf("HTTP contract = %#v", fixture.HTTP)
	}
	if fixture.LogicalStateSource != "authorized-durable-record" {
		t.Fatalf("logical state source = %q", fixture.LogicalStateSource)
	}
	wantVectors := map[string]string{
		"acceptance-dispatch-crash": "running",
		"worker-crash":              "indeterminate",
		"duplicate-delivery":        "succeeded",
		"retention-expiry":          "succeeded",
		"cancellation-effective":    "cancelled",
		"cancellation-too-late":     "succeeded",
		"unknown-commit-outcome":    "indeterminate",
		"completion-cancel-race":    "succeeded",
		"authorization-boundary":    "succeeded",
	}
	seenVectors := map[string]bool{}
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || seenVectors[vector.Name] || len(vector.Events) == 0 || vector.FinalState != wantVectors[vector.Name] {
			t.Fatalf("invalid or duplicate async vector %#v", vector)
		}
		seenVectors[vector.Name] = true
		if !vector.Reauthorized {
			t.Errorf("vector %q does not reauthorize retrieval", vector.Name)
		}
	}
	for name := range wantVectors {
		if !seenVectors[name] {
			t.Errorf("missing vector %q", name)
		}
	}
}
