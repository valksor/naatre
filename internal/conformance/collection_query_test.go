package conformance_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/valksor/naatre/collectionquery"
	"github.com/valksor/naatre/schema"
)

type collectionQueryFixture struct {
	Profile           string                           `json:"profile"`
	ASTKinds          []schema.CollectionFilterKind    `json:"astKinds"`
	CoreOperators     []schema.FilterOperator          `json:"coreOperators"`
	OptionalOperators []optionalFilterOperator         `json:"optionalOperators"`
	Schema            schema.CollectionQueryDescriptor `json:"schema"`
	TruthTable        []filterTruthRow                 `json:"truthTable"`
	ListSemantics     []listTruthRow                   `json:"listSemantics"`
	SortContract      collectionSortContract           `json:"sortContract"`
	Provider          collectionProviderContract       `json:"provider"`
	CursorMismatches  []collectionCursorMismatch       `json:"cursorMismatchCases"`
	FailureCases      []collectionQueryFailure         `json:"failureCases"`
}

type optionalFilterOperator struct {
	Operator   schema.FilterOperator `json:"operator"`
	Capability string                `json:"capability"`
}

type filterTruthRow struct {
	State     string `json:"state"`
	Equal     bool   `json:"eq"`
	NotEqual  bool   `json:"ne"`
	In        bool   `json:"in"`
	NotIn     bool   `json:"not-in"`
	IsNull    bool   `json:"is-null"`
	IsNotNull bool   `json:"is-not-null"`
	Exists    bool   `json:"exists"`
	NotExists bool   `json:"not-exists"`
}

type listTruthRow struct {
	State string `json:"state"`
	Any   bool   `json:"any"`
	All   bool   `json:"all"`
}

type collectionSortContract struct {
	OrderedMultiKey        bool                   `json:"orderedMultiKey"`
	Directions             []schema.SortDirection `json:"directions"`
	NullPlacement          []schema.NullPlacement `json:"nullPlacement"`
	MissingIsNullish       bool                   `json:"missingIsNullish"`
	Collations             []string               `json:"collations"`
	CaseSensitivity        string                 `json:"caseSensitivity"`
	StableUniqueTieBreaker bool                   `json:"stableUniqueTieBreaker"`
}

type collectionProviderContract struct {
	Dialect            string   `json:"dialect"`
	Parameterized      bool     `json:"parameterized"`
	ServerMappedFields bool     `json:"serverMappedFields"`
	FallbackInMemory   bool     `json:"fallbackInMemory"`
	Parity             []string `json:"parity"`
}

type collectionQueryFailure struct {
	Name               string `json:"name"`
	Phase              string `json:"phase"`
	Code               string `json:"code"`
	BackendInvocations int    `json:"backendInvocations"`
	LeaksRequestedName bool   `json:"leaksRequestedName"`
	BoundArgument      bool   `json:"boundArgument"`
}

type collectionCursorMismatch struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

func TestCollectionQueryLanguageNeutralContract(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Profile       string                 `json:"profile"`
		CursorVersion uint8                  `json:"cursorVersion"`
		Integrity     string                 `json:"integrity"`
		SafeError     string                 `json:"safeError"`
		ScopeBindings json.RawMessage        `json:"scopeBindings"`
		BoundaryCases json.RawMessage        `json:"boundaryCases"`
		FailureCases  json.RawMessage        `json:"failureCases"`
		Consistency   json.RawMessage        `json:"consistency"`
		Cost          json.RawMessage        `json:"cost"`
		Query         collectionQueryFixture `json:"query"`
	}
	readFixture(t, "collections.json", &fixture)
	query := fixture.Query
	if query.Profile != schema.CollectionQueryCapability || query.Schema.Capability != schema.CollectionQueryCapability {
		t.Fatalf("query profiles = %q %q", query.Profile, query.Schema.Capability)
	}
	if err := schema.ValidateCollectionQueryDescriptor(&query.Schema); err != nil {
		t.Fatalf("query schema: %v", err)
	}
	wantKinds := []schema.CollectionFilterKind{schema.CollectionFilterAnd, schema.CollectionFilterOr, schema.CollectionFilterNot, schema.CollectionFilterPredicate, schema.CollectionFilterAny, schema.CollectionFilterAll}
	if !slices.Equal(query.ASTKinds, wantKinds) || len(query.CoreOperators) != 14 || len(query.OptionalOperators) != 3 {
		t.Fatalf("AST/operator inventory = %v %v %v", query.ASTKinds, query.CoreOperators, query.OptionalOperators)
	}
	assertCollectionTruthTable(t, query.TruthTable, query.ListSemantics)
	if !query.SortContract.OrderedMultiKey || !query.SortContract.MissingIsNullish || !query.SortContract.StableUniqueTieBreaker || query.SortContract.CaseSensitivity != "sensitive" {
		t.Fatalf("sort contract = %#v", query.SortContract)
	}
	if query.Provider.Dialect != "sqlite" || !query.Provider.Parameterized || !query.Provider.ServerMappedFields || query.Provider.FallbackInMemory || len(query.Provider.Parity) != 6 {
		t.Fatalf("provider contract = %#v", query.Provider)
	}
	if len(query.CursorMismatches) != 6 || len(query.FailureCases) != 8 {
		t.Fatalf("cursor/failure inventory = %v %#v", query.CursorMismatches, query.FailureCases)
	}
	for _, mismatch := range query.CursorMismatches {
		if mismatch.Name == "" || mismatch.Code != "INVALID_CURSOR" {
			t.Errorf("cursor mismatch = %#v", mismatch)
		}
	}
	for _, failure := range query.FailureCases {
		if failure.Code == "OK" {
			if !failure.BoundArgument {
				t.Errorf("successful case %q is not parameterized", failure.Name)
			}
			continue
		}
		if failure.BackendInvocations != 0 || failure.LeaksRequestedName || (failure.Code != collectionquery.CodeUnsupported && failure.Code != collectionquery.CodeResourceExhausted) {
			t.Errorf("unsafe failure case = %#v", failure)
		}
	}
}

func assertCollectionTruthTable(t testing.TB, scalar []filterTruthRow, lists []listTruthRow) {
	t.Helper()
	if len(scalar) != 3 || scalar[0].State != "missing" || scalar[0].Equal || scalar[0].NotEqual || scalar[0].In || scalar[0].NotIn || scalar[0].IsNull || scalar[0].IsNotNull || scalar[0].Exists || !scalar[0].NotExists {
		t.Fatalf("missing truth row = %#v", scalar)
	}
	if scalar[1].State != "null" || scalar[1].Equal || scalar[1].NotEqual || scalar[1].In || scalar[1].NotIn || !scalar[1].IsNull || scalar[1].IsNotNull || !scalar[1].Exists || scalar[1].NotExists {
		t.Fatalf("null truth row = %#v", scalar[1])
	}
	if len(lists) < 3 || lists[0].Any || lists[0].All || lists[1].Any || lists[1].All || lists[2].Any || !lists[2].All {
		t.Fatalf("list truth rows = %#v", lists)
	}
}
