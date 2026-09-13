package runtime_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type observabilityConformanceFixture struct {
	Profile string                           `json:"profile"`
	Vectors []observabilityConformanceVector `json:"vectors"`
}

type observabilityConformanceVector struct {
	Name       string   `json:"name"`
	Events     []string `json:"events"`
	Identities []string `json:"identities"`
	Links      []struct {
		From int `json:"from"`
		To   int `json:"to"`
	} `json:"links"`
	Terminal string `json:"terminal"`
}

func loadObservabilityFixture(t testing.TB) observabilityConformanceFixture {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "observability.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture observabilityConformanceFixture
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode observability fixture: %v", err)
	}
	if fixture.Profile != "operations.observability-1" {
		t.Fatalf("observability profile = %q", fixture.Profile)
	}
	return fixture
}

func observabilityFixtureVector(t testing.TB, name string) observabilityConformanceVector {
	t.Helper()
	fixture := loadObservabilityFixture(t)
	for _, vector := range fixture.Vectors {
		if vector.Name == name {
			return vector
		}
	}
	t.Fatalf("observability vector %q is missing", name)
	return observabilityConformanceVector{}
}

func observabilityFixtureEvents(t testing.TB, name string) []string {
	t.Helper()
	return observabilityFixtureVector(t, name).Events
}

func assertAuditStringsMatchFixture(t testing.TB, name string, events []string) {
	t.Helper()
	var got []string
	for _, event := range events {
		if stage, ok := strings.CutPrefix(event, "audit:"); ok {
			got = append(got, "transaction."+stage)
		}
	}
	if want := observabilityFixtureEvents(t, name); !slices.Equal(got, want) {
		t.Fatalf("%s lifecycle = %v, want %v", name, got, want)
	}
}
