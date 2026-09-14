package conformance_test

import (
	"context"
	"testing"

	"github.com/valksor/naatre/internal/conformancerunner"
)

func TestGeneratorPluginHostConformance(t *testing.T) {
	t.Parallel()
	runner, err := conformancerunner.New("../../conformance/v1/suite.json")
	if err != nil {
		t.Fatal(err)
	}
	response := runner.Handle(context.Background(), conformancerunner.Request{
		Protocol: conformancerunner.Protocol,
		ID:       "generator-plugin-host",
		Command:  "run",
		Path: &conformancerunner.Path{
			Source:      conformancerunner.Endpoint{Kind: "sdk", Language: "javascript-typescript"},
			Destination: conformancerunner.Endpoint{Kind: "native-runtime", Language: "go"},
		},
		Profiles: []string{"sdk.generator-plugin-host-1"},
	})
	if len(response.Results) != 1 || response.Results[0].Status != "passed" || len(response.Results[0].Capabilities) != 1 || response.Results[0].Capabilities[0] != "sdk.generator-plugin-host-1" {
		t.Fatalf("plugin-host conformance result = %#v", response.Results)
	}
}
