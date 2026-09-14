package conformance_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
)

type collectionFixture struct {
	Profile       string                   `json:"profile"`
	CursorVersion uint8                    `json:"cursorVersion"`
	Integrity     string                   `json:"integrity"`
	SafeError     string                   `json:"safeError"`
	ScopeBindings []string                 `json:"scopeBindings"`
	BoundaryCases []collectionBoundaryCase `json:"boundaryCases"`
	FailureCases  []collectionFailureCase  `json:"failureCases"`
	Consistency   []string                 `json:"consistency"`
	Cost          collectionCostContract   `json:"cost"`
	Query         json.RawMessage          `json:"query"`
}

type collectionBoundaryCase struct {
	Name            string                   `json:"name"`
	Direction       runtime.CursorDirection  `json:"direction"`
	Size            uint64                   `json:"size"`
	After           *runtime.CursorPosition  `json:"after,omitempty"`
	Before          *runtime.CursorPosition  `json:"before,omitempty"`
	Entries         []collectionFixtureEntry `json:"entries"`
	ExpectedItems   []string                 `json:"expectedItems"`
	HasNextPage     bool                     `json:"hasNextPage"`
	HasPreviousPage bool                     `json:"hasPreviousPage"`
}

type collectionFixtureEntry struct {
	Item       string `json:"item"`
	SortKey    string `json:"sortKey"`
	TieBreaker string `json:"tieBreaker"`
}

type collectionFailureCase struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

type collectionCostContract struct {
	PageSizeMultiplier bool `json:"pageSizeMultiplier"`
	TotalCountExplicit bool `json:"totalCountExplicit"`
	TotalCountSeparate bool `json:"totalCountSeparate"`
}

func TestCollectionPaginationFixture(t *testing.T) {
	t.Parallel()
	var fixture collectionFixture
	readFixture(t, "collections.json", &fixture)
	if fixture.Profile != "collection.page-1" || fixture.CursorVersion != 1 || fixture.Integrity != "hmac-sha-256" || fixture.SafeError != runtime.CodeInvalidCursor {
		t.Fatalf("collection fixture header = %#v", fixture)
	}
	wantBindings := []string{"collection", "filters", "sort", "direction", "tenant", "authorization", "snapshotPolicy", "snapshot", "keyId", "version", "expiry"}
	if !slices.Equal(fixture.ScopeBindings, wantBindings) {
		t.Fatalf("scope bindings = %v, want %v", fixture.ScopeBindings, wantBindings)
	}
	assertCollectionBoundaries(t, fixture.BoundaryCases)
	wantFailures := []string{"tampered", "cross-tenant", "changed-filter", "changed-sort", "stale-snapshot", "expired", "removed-key"}
	gotFailures := make([]string, len(fixture.FailureCases))
	for index, failure := range fixture.FailureCases {
		gotFailures[index] = failure.Name
		if failure.Code != runtime.CodeInvalidCursor {
			t.Errorf("failure %q code = %q", failure.Name, failure.Code)
		}
	}
	if !slices.Equal(gotFailures, wantFailures) {
		t.Fatalf("failure cases = %v, want %v", gotFailures, wantFailures)
	}
	if !slices.Equal(fixture.Consistency, []string{"live-best-effort", "snapshot-stable"}) {
		t.Fatalf("consistency cases = %v", fixture.Consistency)
	}
	if !fixture.Cost.PageSizeMultiplier || !fixture.Cost.TotalCountExplicit || !fixture.Cost.TotalCountSeparate {
		t.Fatalf("cost contract = %#v", fixture.Cost)
	}
}

func assertCollectionBoundaries(t *testing.T, cases []collectionBoundaryCase) {
	t.Helper()
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "fixture", Keys: map[string][]byte{"fixture": []byte("fixture-key-material-is-32-bytes-long")},
		TTL: time.Hour, Now: func() time.Time { return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC) }, MaxPageSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := runtime.NewCursorScope("fixture-users", []byte(`{"active":true}`), []byte(`[{"field":"name"},{"field":"id"}]`), runtime.CursorScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			entries := fixturePageEntries(test.Entries)
			request := fixturePageRequest(t, codec, scope, test)
			page, pageErr := runtime.Paginate(codec, entries, request)
			if pageErr != nil {
				t.Fatal(pageErr)
			}
			if !slices.Equal(page.Items, test.ExpectedItems) || page.PageInfo.HasNextPage != test.HasNextPage || page.PageInfo.HasPreviousPage != test.HasPreviousPage {
				t.Fatalf("page = %#v, expected items=%v next=%t previous=%t", page, test.ExpectedItems, test.HasNextPage, test.HasPreviousPage)
			}
		})
	}
}

func fixturePageEntries(input []collectionFixtureEntry) []runtime.PageEntry[string] {
	entries := make([]runtime.PageEntry[string], len(input))
	for index, entry := range input {
		entries[index] = runtime.PageEntry[string]{Item: entry.Item, Position: runtime.CursorPosition{SortKey: entry.SortKey, TieBreaker: entry.TieBreaker}}
	}
	return entries
}

func fixturePageRequest(t *testing.T, codec *runtime.CursorCodec, scope runtime.CursorScope, test collectionBoundaryCase) runtime.PageRequest {
	t.Helper()
	request := runtime.PageRequest{Scope: scope}
	var err error
	if test.Direction == runtime.CursorForward {
		request.First = &test.Size
		if test.After != nil {
			request.After, err = codec.Encode(scope, runtime.CursorForward, *test.After)
		}
	} else {
		request.Last = &test.Size
		if test.Before != nil {
			request.Before, err = codec.Encode(scope, runtime.CursorBackward, *test.Before)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return request
}
