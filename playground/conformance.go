package playground

import (
	"encoding/json"
	"errors"
	"slices"
)

// ValidateProfileEvidence verifies the closed playground/mock profile and
// its exact issue #55 dependency pins.
func ValidateProfileEvidence(input []byte) error {
	var fixture struct {
		Profile        string `json:"profile"`
		Version        string `json:"version"`
		OwnerIssue     int    `json:"ownerIssue"`
		NormativeOwner int    `json:"normativeOwnerIssue"`
		Dependencies   struct {
			Issue55Commit        string `json:"issue55Commit"`
			ToolingProfile       string `json:"toolingProfile"`
			ToolingFixture       string `json:"toolingFixture"`
			ToolingFixtureSHA256 string `json:"toolingFixtureSHA256"`
			GoModule             string `json:"goModule"`
			GoModSHA256          string `json:"goModSHA256"`
			GoVersion            string `json:"goVersion"`
			SchemaVersion        string `json:"schemaVersion"`
			CanonicalVersion     string `json:"canonicalVersion"`
		} `json:"dependencies"`
		Runtime struct {
			Package          string `json:"package"`
			Binary           string `json:"binary"`
			Listen           string `json:"listen"`
			Lifecycle        string `json:"lifecycle"`
			ExecutedPlatform string `json:"executedPlatform"`
		} `json:"runtime"`
		Limits struct {
			RequestBytes           int64 `json:"requestBytes"`
			DocumentBytes          int   `json:"documentBytes"`
			InspectionPayloadBytes int   `json:"inspectionPayloadBytes"`
			ResponseBytes          int   `json:"responseBytes"`
			MockDepth              int   `json:"mockDepth"`
			MockItems              int   `json:"mockItems"`
			MockStringBytes        int   `json:"mockStringBytes"`
		} `json:"limits"`
		Supported    []string `json:"supported"`
		Unsupported  []string `json:"unsupported"`
		FailureCodes []string `json:"failureCodes"`
		Cases        []struct {
			ID     string `json:"id"`
			Class  string `json:"class"`
			Status string `json:"status"`
		} `json:"cases"`
	}
	if json.Unmarshal(input, &fixture) != nil {
		return errors.New("invalid playground conformance fixture")
	}
	valid := fixture.Profile == Profile && fixture.Version == "1.0.0" && fixture.OwnerIssue == 91 && fixture.NormativeOwner == 55 &&
		fixture.Dependencies.Issue55Commit == Issue55Revision && fixture.Dependencies.ToolingProfile == "tooling.workflow-1@1.0.0" &&
		fixture.Dependencies.ToolingFixture == "conformance/v1/tooling.json" && fixture.Dependencies.ToolingFixtureSHA256 == ToolingFixtureSHA256 &&
		fixture.Dependencies.GoModule == "github.com/valksor/naatre" && fixture.Dependencies.GoModSHA256 == "2624a288e366c96288c99fa925dc47e839e3d73146eba8ad659478f7a9ede206" &&
		fixture.Dependencies.GoVersion == "1.27.0" && fixture.Dependencies.SchemaVersion == "1" && fixture.Dependencies.CanonicalVersion == "c14n-1" &&
		fixture.Runtime.Package == "github.com/valksor/naatre/playground" && fixture.Runtime.Binary == "github.com/valksor/naatre/cmd/naatre-playground" &&
		fixture.Runtime.Listen == "127.0.0.1:0" && fixture.Runtime.Lifecycle == "explicit-process-memory-only" && fixture.Runtime.ExecutedPlatform == "darwin-arm64-offline" &&
		fixture.Limits.RequestBytes == 1<<20 && fixture.Limits.DocumentBytes == 256<<10 && fixture.Limits.InspectionPayloadBytes == 4<<10 && fixture.Limits.ResponseBytes == 1<<20 &&
		fixture.Limits.MockDepth == 32 && fixture.Limits.MockItems == 16 && fixture.Limits.MockStringBytes == 4<<10 &&
		slices.Equal(fixture.FailureCodes, sortedFailureCodes()) && slices.Equal(fixture.Supported, supportedCapabilities()) && slices.Equal(fixture.Unsupported, unsupportedCapabilities())
	if !valid {
		return errors.New("invalid playground conformance fixture")
	}
	classes := map[string]bool{"positive": false, "negative": false, "boundary": false, "cancellation": false, "resource-limit": false}
	for _, testCase := range fixture.Cases {
		if testCase.ID == "" || testCase.Status != "passed" {
			return errors.New("invalid playground conformance case")
		}
		if _, ok := classes[testCase.Class]; !ok {
			return errors.New("invalid playground conformance class")
		}
		classes[testCase.Class] = true
	}
	for _, covered := range classes {
		if !covered {
			return errors.New("incomplete playground conformance classes")
		}
	}
	return nil
}
