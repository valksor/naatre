package conformancerunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/valksor/naatre/client"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/sdk/go/generated"
	"github.com/valksor/naatre/sdk/go/sdkgen"
)

const goSDKProfile = "sdk.go.operations-1"

type goSDKFixture struct {
	Profile        string `json:"profile"`
	FixtureSuite   string `json:"fixtureSuite"`
	Implementation struct {
		Module           string         `json:"module"`
		MinimumGo        string         `json:"minimumGo"`
		GeneratorVersion string         `json:"generatorVersion"`
		GoMod            evidenceFile   `json:"goMod"`
		Files            []evidenceFile `json:"files"`
	} `json:"implementation"`
	Sources struct {
		Model           evidenceFile `json:"model"`
		ReferenceOutput evidenceFile `json:"referenceOutput"`
	} `json:"sources"`
	Generated struct {
		Source   evidenceFile `json:"source"`
		Manifest evidenceFile `json:"manifest"`
	} `json:"generated"`
	Vectors []string `json:"vectors"`
	Limits  struct {
		MaximumRetryAttempts int   `json:"maximumRetryAttempts"`
		MaximumManifestBytes int64 `json:"maximumManifestBytes"`
		MaximumFrameBytes    int   `json:"maximumFrameBytes"`
	} `json:"limits"`
	Unsupported []string `json:"unsupported"`
}

func (r *Runner) verifyGoSDK(_ context.Context, _ Request) Result {
	fixture, fixtureEvidence, err := r.loadGoSDKFixture()
	if err != nil {
		return failureResult(goSDKProfile, "GO_SDK_FIXTURE_INVALID", "v1/go-sdk.json")
	}
	files := []evidenceFile{fixture.Implementation.GoMod, fixture.Sources.Model, fixture.Sources.ReferenceOutput, fixture.Generated.Source, fixture.Generated.Manifest}
	files = append(files, fixture.Implementation.Files...)
	evidence, contents, err := r.verifyEvidenceFiles(files)
	if err != nil {
		return failureResult(goSDKProfile, "GO_SDK_EVIDENCE_MISMATCH", "v1/go-sdk.json")
	}
	generationErr := verifyGoSDKGeneration(fixture, contents)
	behaviorErr := verifyGoSDKBehavior(contents[fixture.Sources.Model.Path], contents[fixture.Sources.ReferenceOutput.Path])
	switch {
	case generationErr != nil:
		return failureResult(goSDKProfile, "GO_SDK_GENERATION_FAILED", "v1/go-sdk.json")
	case behaviorErr != nil:
		return failureResult(goSDKProfile, "GO_SDK_BEHAVIOR_FAILED", "v1/go-sdk.json")
	}
	result := emptyResult(goSDKProfile, "passed", "")
	result.Capabilities = []string{goSDKProfile}
	result.Evidence = append([]Evidence{fixtureEvidence}, evidence...)
	return result
}

func (r *Runner) loadGoSDKFixture() (goSDKFixture, Evidence, error) {
	var fixture goSDKFixture
	_, evidence, err := r.loadPinnedFixture("v1/go-sdk.json", &fixture)
	if err != nil {
		return goSDKFixture{}, Evidence{}, err
	}
	if fixture.Profile != goSDKProfile || fixture.FixtureSuite != r.manifest.FixtureVersion ||
		fixture.Implementation.Module != "github.com/valksor/naatre/sdk/go/generated" ||
		fixture.Implementation.MinimumGo != "1.27" || fixture.Implementation.GeneratorVersion != sdkgen.GeneratorVersion ||
		len(fixture.Vectors) != 12 || len(fixture.Unsupported) == 0 || fixture.Limits.MaximumRetryAttempts != 8 ||
		fixture.Limits.MaximumManifestBytes != 1<<20 || fixture.Limits.MaximumFrameBytes != 1<<20 {
		return goSDKFixture{}, Evidence{}, errors.New("incompatible Go SDK fixture")
	}
	return fixture, evidence, nil
}

func verifyGoSDKGeneration(fixture goSDKFixture, contents map[string][]byte) error {
	artifacts, err := sdkgen.Generate(contents[fixture.Sources.Model.Path], contents[fixture.Sources.ReferenceOutput.Path])
	if err != nil {
		return err
	}
	if !bytes.Equal(artifacts.Source, contents[fixture.Generated.Source.Path]) || !bytes.Equal(artifacts.Manifest, contents[fixture.Generated.Manifest.Path]) {
		return errors.New("generated artifacts differ")
	}
	return nil
}

func verifyGoSDKBehavior(modelBytes, outputBytes []byte) error {
	var model generatorClientModel
	var output generatorClientOutput
	if err := json.Unmarshal(modelBytes, &model); err != nil || len(model.Operations) != 1 {
		return errors.New("invalid generator model")
	}
	if err := json.Unmarshal(outputBytes, &output); err != nil || len(output.Operations) != 1 {
		return errors.New("invalid generator output")
	}
	operation, err := generated.NewGetAccount()
	if err != nil {
		return err
	}
	for name, value := range model.Operations[0].RequestVariables {
		operation, err = operation.WithVariable(name, value)
		if err != nil {
			return err
		}
	}
	actual, err := operation.Request().CanonicalJSON()
	if err != nil {
		return err
	}
	expected, err := protocol.CanonicalizeJSON(output.Operations[0].Request, protocol.Limits{})
	if err != nil || !bytes.Equal(actual, expected) || operation.Request().OperationKind() != protocol.Query {
		return errors.New("generated request mismatch")
	}
	manifest, err := generated.Manifest()
	if err != nil {
		return err
	}
	manifestRequest, err := manifest.Request("GetAccount")
	if err != nil || manifestRequest.OperationKind() != protocol.Query {
		return errors.New("generated manifest mismatch")
	}
	selected, err := generated.DecodeGetAccountResult([]byte(`{"profile":{"display":"Ada","nickname":null}}`))
	if err != nil {
		return err
	}
	profile, present := selected.Profile.Value()
	if !present || profile.Nickname.Presence() != client.Null || selected.Later.Presence() != client.Pending {
		return errors.New("generated selected result mismatch")
	}
	return nil
}
