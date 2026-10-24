package provider

// ProfileForClient prefers the selected connection's explicit capabilities.
// Legacy clients without this seam keep the existing model-family contract.
func ProfileForClient(client any, model string) (ModelProfile, bool) {
	if source, ok := client.(interface{ ModelCapabilities() (ModelProfile, bool) }); ok {
		return source.ModelCapabilities()
	}
	return ProfileForModel(model)
}

func (c *Client) ModelCapabilities() (ModelProfile, bool) {
	if c.externalCompatible {
		profile := ModelProfile{Name: c.Model, Streaming: true, Multimodal: c.externalImages}
		if known, ok := ProfileForModel(c.Model); ok {
			profile.ThinkingControl, profile.ReasoningEffortLevels = known.ThinkingControl, known.ReasoningEffortLevels
		}
		if c.externalOpenAIReasoningEffort {
			profile.ThinkingControl = true
			profile.ReasoningEffortLevels = 5
		}
		return profile, true
	}
	return ProfileForModel(c.Model)
}
