package agent

import "github.com/hey-meadow/meadow-code/go/core/provider"

// DefaultCodexStyleOutputTokens uses the admitted native API capacity when the
// caller did not set a request cap. Local/unknown model defaults stay unchanged;
// this neither invents provider capacity nor changes an explicit caller limit.
func DefaultCodexStyleOutputTokens(client KernelModel, modelID string) int {
	profile, known := provider.ProfileForModel(modelID)
	if known && (profile.Family == "deepseek" || profile.Family == "xai") {
		if capacity, ok := client.(interface{ OutputTokenLimit() int }); ok {
			if limit := capacity.OutputTokenLimit(); limit > 0 {
				return limit
			}
		}
	}
	return 8192
}
