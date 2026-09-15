package asyncapi

import (
	"encoding/json"
	"errors"
	"slices"
)

type conformanceFixture struct {
	Profile          string                   `json:"profile"`
	Version          string                   `json:"version"`
	Issue            int                      `json:"issue"`
	Specification    string                   `json:"specification"`
	AsyncAPIVersion  string                   `json:"asyncapiVersion"`
	ExporterVersion  string                   `json:"exporterVersion"`
	Directions       []string                 `json:"directions"`
	RevisionBindings []string                 `json:"revisionBindings"`
	Fidelity         []conformanceFidelity    `json:"fidelity"`
	Transports       []conformanceTransport   `json:"transports"`
	Correlations     []conformanceCorrelation `json:"correlations"`
	Compatibility    []struct {
		Domain         string `json:"domain"`
		ContractChange string `json:"contractChange"`
	} `json:"compatibility"`
	Security             conformanceSecurity `json:"security"`
	Consumers            []string            `json:"consumers"`
	BusinessHandlerCalls int                 `json:"businessHandlerCalls"`
	Evidence             []string            `json:"evidence"`
}

type conformanceFidelity struct {
	Feature        string `json:"feature"`
	Classification string `json:"classification"`
}

type conformanceTransport struct {
	Name              string `json:"name"`
	Binding           string `json:"binding"`
	WireCompatibility bool   `json:"wireCompatibility"`
}

type conformanceCorrelation struct {
	Name          string `json:"name"`
	Lifetime      string `json:"lifetime"`
	Authorization bool   `json:"authorization"`
}

type conformanceSecurity struct {
	RemoteReferences        string `json:"remoteReferences"`
	ServerURLs              string `json:"serverURLs"`
	CycleDetection          bool   `json:"cycleDetection"`
	BoundedImport           bool   `json:"boundedImport"`
	CredentialRedaction     bool   `json:"credentialRedaction"`
	ProtectedSchemaMetadata string `json:"protectedSchemaMetadata"`
}

// ValidateConformanceFixture validates the portable issue #62 evidence index.
func ValidateConformanceFixture(input []byte) error {
	var fixture conformanceFixture
	if json.Unmarshal(input, &fixture) != nil {
		return errors.New("invalid AsyncAPI conformance fixture")
	}
	if !validConformanceIdentity(fixture) {
		return errors.New("invalid AsyncAPI conformance identity")
	}
	if !validConformanceFidelity(fixture.Fidelity) {
		return errors.New("incomplete AsyncAPI fidelity matrix")
	}
	if !validConformanceCorrelations(fixture.Correlations) {
		return errors.New("invalid AsyncAPI correlation contract")
	}
	if !validConformanceTransports(fixture.Transports) {
		return errors.New("invalid AsyncAPI transport contract")
	}
	if len(fixture.Compatibility) != 4 || !validConformanceSecurity(fixture.Security) {
		return errors.New("incomplete AsyncAPI security or evolution contract")
	}
	return nil
}

func validConformanceIdentity(fixture conformanceFixture) bool {
	return fixture.Profile == Profile && fixture.Version == "1.0.0" && fixture.Issue == 62 && fixture.Specification == Specification &&
		fixture.AsyncAPIVersion == Version && fixture.ExporterVersion == ExporterVersion &&
		slices.Equal(fixture.Directions, []string{"schema-export", "schema-import"}) &&
		completeStrings(fixture.RevisionBindings, []string{"schema", "schemaDigest", "capabilities", "canonicalization", "eventEnvelope", "asyncapiProfile", "exporter"}) &&
		completeStrings(fixture.Consumers, []string{"go-api", "cli", "documentation", "mock", "playground"}) && fixture.BusinessHandlerCalls == 0 && len(fixture.Evidence) >= 8
}

func validConformanceFidelity(got []conformanceFidelity) bool {
	want := fidelityMappings()
	if len(got) != len(want) {
		return false
	}
	for index, mapping := range want {
		if got[index].Feature != mapping.Feature || got[index].Classification != string(mapping.Classification) {
			return false
		}
	}
	return true
}

func validConformanceCorrelations(correlations []conformanceCorrelation) bool {
	wantLifetimes := map[string]string{"request-id": "request", "stream-id": "logical-stream", "application-event-id": "business-event", "webhook-delivery-attempt": "delivery-attempt"}
	for _, correlation := range correlations {
		if wantLifetimes[correlation.Name] != correlation.Lifetime || correlation.Authorization {
			return false
		}
		delete(wantLifetimes, correlation.Name)
	}
	return len(wantLifetimes) == 0
}

func validConformanceTransports(transports []conformanceTransport) bool {
	if len(transports) != 5 {
		return false
	}
	for _, transport := range transports {
		if transport.Name == "" || transport.Binding == "" || transport.WireCompatibility {
			return false
		}
	}
	return true
}

func validConformanceSecurity(security conformanceSecurity) bool {
	return security.RemoteReferences == "blocked" && security.ServerURLs == "exact-allowlist-default-deny" && security.CycleDetection &&
		security.BoundedImport && security.CredentialRedaction && security.ProtectedSchemaMetadata == "omitted-by-default"
}

func completeStrings(got, want []string) bool {
	for _, value := range want {
		if !slices.Contains(got, value) {
			return false
		}
	}
	return len(got) == len(want)
}
