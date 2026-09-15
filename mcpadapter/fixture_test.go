package mcpadapter

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type lifecycleFixture struct {
	Event         Cause  `json:"event"`
	Code          string `json:"code"`
	Complete      bool   `json:"complete"`
	Retry         bool   `json:"retry"`
	ReleasesState bool   `json:"releasesRequestState"`
}

func TestConformanceFixtureMatchesReferenceImplementation(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../conformance/v1/mcp-adapter.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profile   string    `json:"profile"`
		Mappings  []Mapping `json:"mappingMatrix"`
		Revisions struct {
			MCPProtocol string `json:"mcpProtocolVersion"`
			MCP         string `json:"mcp"`
			Schema      string `json:"naatreSchema"`
			Protocol    string `json:"naatreProtocol"`
			Conformance string `json:"conformance"`
		} `json:"revisions"`
		Lifecycle []lifecycleFixture `json:"lifecycleFixtures"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Profile != Profile || fixture.Revisions.MCPProtocol != MCPRevision || fixture.Revisions.MCP != MCPSpecification || fixture.Revisions.Schema != NaatreSchemaRevision || fixture.Revisions.Protocol != NaatreProtocolRevision || fixture.Revisions.Conformance != ConformanceRevision {
		t.Fatalf("fixture revisions = %#v", fixture.Revisions)
	}
	if !reflect.DeepEqual(fixture.Mappings, DefaultMappings()) {
		t.Fatalf("fixture mappings diverged from Go report: %#v", fixture.Mappings)
	}
	session, err := NewSession(SessionConfig{ID: "fixture", Principal: "fixture-principal", Tenant: "fixture-tenant", MaxProgressEvents: 1, MaxResponseBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	for _, vector := range fixture.Lifecycle {
		if vector.Event == "progress-overflow" {
			continue
		}
		assertLifecycleFixture(t, session, vector)
	}
	assertProgressFixture(t, session)
}

func assertLifecycleFixture(t testing.TB, session *Session, vector lifecycleFixture) {
	t.Helper()
	ids := testRequestIDs("fixture-" + string(vector.Event))
	request, err := session.Begin(ids)
	if err != nil {
		t.Fatal(err)
	}
	request.PutCache("fixture", "cached")
	request.PutLoader("fixture", "loader")
	cause, responseBytes := vector.Event, 0
	if vector.Event == "oversized-response" {
		cause, responseBytes = Completed, 9
	}
	outcome := session.Finalize(request, cause, responseBytes)
	if outcome.Code != vector.Code || outcome.Complete != vector.Complete || outcome.Retry != vector.Retry {
		t.Fatalf("fixture outcome %s = %#v", vector.Event, outcome)
	}
	_, cacheExists := request.Cache("fixture")
	_, loaderExists := request.Loader("fixture")
	if !vector.ReleasesState || cacheExists || loaderExists || session.ProgressCount(ids.Progress) != 0 {
		t.Fatalf("fixture state release %s = cache:%t loader:%t", vector.Event, cacheExists, loaderExists)
	}
}

func assertProgressFixture(t testing.TB, session *Session) {
	t.Helper()
	progressIDs := testRequestIDs("fixture-progress")
	progressRequest, err := session.Begin(progressIDs)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RecordProgress(progressIDs.Progress, 1, "first"); err != nil {
		t.Fatal(err)
	}
	if err := session.RecordProgress(progressIDs.Progress, 2, "overflow"); errorCode(err) != "MCP_PROGRESS_LIMIT" {
		t.Fatalf("progress overflow = %v", err)
	}
	session.Finalize(progressRequest, Cancelled, 0)
}
