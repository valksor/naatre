package conformance_test

import (
	"encoding/json"
	"testing"
)

func TestBatchingFixtureInventory(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Profile         string            `json:"profile"`
		DispatchVectors []json.RawMessage `json:"dispatchVectors"`
		CacheVectors    []json.RawMessage `json:"cacheVectors"`
	}
	readFixture(t, "batching.json", &fixture)
	if fixture.Profile != "core.batch-cache-1" {
		t.Fatalf("batch profile = %q", fixture.Profile)
	}
	required := map[string]bool{
		"mapped-duplicates-out-of-order":      false,
		"missing-result-keeps-item-path":      false,
		"parallel-distinct-inputs":            false,
		"width-one-terminates":                false,
		"nested-loaders-terminate":            false,
		"cancelled-waiters-terminate":         false,
		"serial-write-barrier":                false,
		"read-write-read-invalidates":         false,
		"complete-key-partition":              false,
		"explicit-error-null-policy":          false,
		"no-store-disables-reuse":             false,
		"core-retains-no-cross-request-entry": false,
	}
	for _, vector := range fixture.DispatchVectors {
		name := batchingVectorName(t, vector)
		if _, ok := required[name]; !ok || required[name] {
			t.Fatalf("unknown or duplicate dispatch vector %q", name)
		}
		required[name] = true
	}
	for _, vector := range fixture.CacheVectors {
		name := batchingVectorName(t, vector)
		if _, ok := required[name]; !ok || required[name] {
			t.Fatalf("unknown or duplicate cache vector %q", name)
		}
		required[name] = true
	}
	for name, found := range required {
		if !found {
			t.Errorf("missing batching vector %q", name)
		}
	}
}

func batchingVectorName(t testing.TB, raw json.RawMessage) string {
	t.Helper()
	var vector struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil || vector.Name == "" {
		t.Fatalf("invalid batching vector: %v", err)
	}
	return vector.Name
}
