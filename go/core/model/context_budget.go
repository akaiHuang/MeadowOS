package model

import "strings"

const (
	// Meadow Coder 35B's KV8 multimodal profile is live-tested at this physical
	// window on a 64GB M1 Max. The upstream checkpoint can advertise more
	// positions, but 128K cold prefill has not passed the product timeout and
	// cancellation-lifecycle gates yet.
	MeadowCoder35B64GBContextWindowTokens = 65536
	defaultContextOutputReserveTokens     = 8192
	defaultContextProtocolMarginTokens    = 1024
	defaultContextToolReserveTokens       = 4096
)

// RuntimeContextBudget separates the physical model window from the smaller
// bounds used to admit one request. PromptLimitTokens includes messages and
// tool schemas. MessageLimitTokens leaves a separate allowance for the tool
// schemas selected by the harness.
type RuntimeContextBudget struct {
	WindowTokens         int
	PromptLimitTokens    int
	MessageLimitTokens   int
	OutputReserveTokens  int
	ToolReserveTokens    int
	ProtocolMarginTokens int
}

// ResolveRuntimeContextWindowTokens prefers the capacity reported by the live
// runtime. Before a 35B worker has been loaded, use the accepted 64GB product
// profile rather than the checkpoint's much larger architectural maximum.
func ResolveRuntimeContextWindowTokens(modelID string, reported int) int {
	if reported > 0 {
		return reported
	}
	if configured := configuredUpstreamContextWindowTokens(modelID); configured > 0 {
		return configured
	}
	id := strings.ToLower(strings.TrimSpace(modelID))
	if strings.Contains(id, "coder 35b") || strings.Contains(id, "qwen35") ||
		strings.Contains(id, "qwen3.5-35b") || strings.Contains(id, "qwen36-35b") {
		return MeadowCoder35B64GBContextWindowTokens
	}
	return ResolveContextWindowTokens("", modelID)
}

// RuntimeBudgetFor reserves generation space and a small tokenizer/protocol
// margin before admitting a prompt. A further tool allowance is removed from
// model-facing message history so dynamic tool schemas cannot consume the
// physical context after history compaction has already run.
func RuntimeBudgetFor(windowTokens int) RuntimeContextBudget {
	if windowTokens <= 0 {
		return RuntimeContextBudget{}
	}
	output := min(defaultContextOutputReserveTokens, max(2048, windowTokens/3))
	margin := min(defaultContextProtocolMarginTokens, max(256, windowTokens/16))
	prompt := max(1, windowTokens-output-margin)
	tool := min(defaultContextToolReserveTokens, max(1024, prompt/4))
	message := max(1, prompt-tool)
	return RuntimeContextBudget{
		WindowTokens: windowTokens, PromptLimitTokens: prompt,
		MessageLimitTokens: message, OutputReserveTokens: output,
		ToolReserveTokens: tool, ProtocolMarginTokens: margin,
	}
}
