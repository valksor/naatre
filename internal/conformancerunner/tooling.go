package conformancerunner

import (
	"context"

	"github.com/valksor/naatre/tooling"
)

const toolingProfile = tooling.ToolingProfile

func (r *Runner) verifyTooling(_ context.Context, _ Request) Result {
	content, evidence, err := r.loadPinnedFixture("v1/tooling.json", new(any))
	if err != nil {
		return failureResult(toolingProfile, "TOOLING_FIXTURE_INVALID", "v1/tooling.json")
	}
	if tooling.ValidateConformanceFixture(content) != nil {
		return failureResult(toolingProfile, "TOOLING_FIXTURE_INVALID", "v1/tooling.json")
	}
	result := emptyResult(toolingProfile, "passed", "")
	result.Capabilities = []string{toolingProfile}
	result.Evidence = []Evidence{evidence}
	return result
}
