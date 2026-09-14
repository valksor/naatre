package conformance_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

type adapterFixture struct {
	Profile         string            `json:"profile"`
	FixtureSuite    string            `json:"fixtureSuite"`
	Specifications  map[string]string `json:"specifications"`
	Directions      []string          `json:"directions"`
	Classifications []string          `json:"classifications"`
	Required        []string          `json:"requiredFeatures"`
	Matrix          []struct {
		Protocol       string `json:"protocol"`
		Direction      string `json:"direction"`
		Category       string `json:"category"`
		Feature        string `json:"feature"`
		Classification string `json:"classification"`
		Resolution     string `json:"resolution"`
	} `json:"matrix"`
	GoldenCases []json.RawMessage `json:"goldenCases"`
	RoundTrip   struct {
		Claimed  []string          `json:"claimed"`
		Excluded []string          `json:"excluded"`
		Vectors  []json.RawMessage `json:"vectors"`
	} `json:"roundTrip"`
	Security  json.RawMessage `json:"security"`
	Example   json.RawMessage `json:"example"`
	Ownership map[string]int  `json:"ownership"`
}

func TestAdapterContractMatrixAndGoldenEvidence(t *testing.T) {
	t.Parallel()
	var fixture adapterFixture
	readFixture(t, "adapters.json", &fixture)
	if fixture.Profile != "core.adapters-1" || fixture.FixtureSuite != "1.0.0" {
		t.Fatalf("adapter metadata = %#v", fixture)
	}
	if !slices.Equal(fixture.Directions, []string{"schema-import", "schema-export", "runtime-consume", "runtime-expose"}) || !slices.Equal(fixture.Classifications, []string{"lossless", "explicitly-adapted", "unsupported", "application-supplied"}) {
		t.Fatalf("adapter vocabulary = %#v / %#v", fixture.Directions, fixture.Classifications)
	}
	protocols := []string{"openapi", "graphql", "openrpc-jsonrpc", "protobuf-grpc-connect"}
	for _, protocolName := range protocols {
		for _, direction := range fixture.Directions {
			if !hasAdapterMapping(fixture.Matrix, protocolName, direction, "") {
				t.Errorf("%s omits direction %s", protocolName, direction)
			}
		}
		for _, category := range []string{"operation", "type", "transport"} {
			if !hasAdapterMapping(fixture.Matrix, protocolName, "", category) {
				t.Errorf("%s omits category %s", protocolName, category)
			}
		}
		for _, feature := range fixture.Required {
			if !hasAdapterFeature(fixture.Matrix, protocolName, feature) {
				t.Errorf("%s omits feature %s", protocolName, feature)
			}
		}
	}
	for _, mapping := range fixture.Matrix {
		if !slices.Contains(fixture.Classifications, mapping.Classification) || mapping.Classification != "lossless" && mapping.Resolution == "" {
			t.Errorf("invalid mapping %#v", mapping)
		}
	}
	names := make([]string, 0, len(fixture.GoldenCases))
	for _, vector := range fixture.GoldenCases {
		var header struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(vector, &header); err != nil {
			t.Fatalf("decode golden case: %v", err)
		}
		names = append(names, header.Name)
	}
	for _, required := range []string{"scalar-boundaries", "absent-and-null-inputs", "partial-failure", "streaming-mismatch", "cancellation", "authentication-forwarding", "graphql-non-null-propagation", "jsonrpc-envelope"} {
		if !slices.Contains(names, required) {
			t.Errorf("golden cases omit %s", required)
		}
	}
	for _, claim := range fixture.RoundTrip.Claimed {
		if slices.Contains(fixture.RoundTrip.Excluded, claim) {
			t.Errorf("round-trip claim %q is also excluded", claim)
		}
		found := false
		for _, raw := range fixture.RoundTrip.Vectors {
			var vector struct {
				Claim             string `json:"claim"`
				External          any    `json:"external"`
				ExternalRoundTrip any    `json:"externalRoundTrip"`
			}
			if err := json.Unmarshal(raw, &vector); err != nil {
				t.Fatal(err)
			}
			if vector.Claim == claim {
				found = true
				if !reflect.DeepEqual(vector.External, vector.ExternalRoundTrip) {
					t.Errorf("round-trip vector %q changed value", claim)
				}
			}
		}
		if !found {
			t.Errorf("round-trip claim %q has no fixture", claim)
		}
	}
	var security struct {
		Operations    string   `json:"operations"`
		LegacyParsers []string `json:"legacyParsers"`
	}
	var example struct {
		LanguageNeutral    bool     `json:"languageNeutral"`
		DeclaredOperations []string `json:"declaredOperations"`
		ApprovedOperations []string `json:"approvedOperations"`
		GoReference        string   `json:"goReference"`
	}
	if err := json.Unmarshal(fixture.Security, &security); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.Example, &example); err != nil {
		t.Fatal(err)
	}
	if security.Operations != "explicit-application-allowlist" || len(security.LegacyParsers) != 0 || !example.LanguageNeutral || example.GoReference != "interopadapter" || !slices.Equal(example.ApprovedOperations, []string{"lookupProfile"}) {
		t.Fatalf("unsafe or incomplete adapter evidence: %#v / %#v", security, example)
	}
	if !slices.Contains(example.DeclaredOperations, "xVendorAdmin") || slices.Contains(example.ApprovedOperations, "xVendorAdmin") {
		t.Fatal("unsupported extension operation bypassed explicit approval evidence")
	}
}

func hasAdapterFeature(matrix []struct {
	Protocol       string `json:"protocol"`
	Direction      string `json:"direction"`
	Category       string `json:"category"`
	Feature        string `json:"feature"`
	Classification string `json:"classification"`
	Resolution     string `json:"resolution"`
}, protocolName, feature string) bool {
	for _, mapping := range matrix {
		if mapping.Protocol == protocolName && mapping.Feature == feature {
			return true
		}
	}
	return false
}

func hasAdapterMapping(matrix []struct {
	Protocol       string `json:"protocol"`
	Direction      string `json:"direction"`
	Category       string `json:"category"`
	Feature        string `json:"feature"`
	Classification string `json:"classification"`
	Resolution     string `json:"resolution"`
}, protocolName, direction, category string) bool {
	for _, mapping := range matrix {
		if mapping.Protocol == protocolName && (direction == "" || mapping.Direction == direction) && (category == "" || mapping.Category == category) {
			return true
		}
	}
	return false
}
