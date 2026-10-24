package provider

import (
	"fmt"
	"strings"
)

// ThinkingMode is an explicit per-session override. Auto preserves the
// product-tested model profile; on/off are sent on every model request.
type ThinkingMode string

const (
	ThinkingAuto ThinkingMode = "auto"
	ThinkingOn   ThinkingMode = "on"
	ThinkingOff  ThinkingMode = "off"
)

// ReasoningEffort selects a model-capability-aware resource policy. The lowest
// tier prioritizes direct output; higher tiers enable supported reasoning.
type ReasoningEffort string

const (
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	ReasoningEffortLow     ReasoningEffort = "low"
	ReasoningEffortMedium  ReasoningEffort = "medium"
	ReasoningEffortHigh    ReasoningEffort = "high"
	ReasoningEffortMax     ReasoningEffort = "max"
)

func ParseThinkingMode(value string) (ThinkingMode, error) {
	switch ThinkingMode(strings.ToLower(strings.TrimSpace(value))) {
	case ThinkingAuto:
		return ThinkingAuto, nil
	case ThinkingOn:
		return ThinkingOn, nil
	case ThinkingOff:
		return ThinkingOff, nil
	default:
		return "", fmt.Errorf("thinking mode must be auto, on, or off")
	}
}

func ParseReasoningEffort(value string) (ReasoningEffort, error) {
	switch ReasoningEffort(strings.ToLower(strings.TrimSpace(value))) {
	case ReasoningEffortMinimal:
		return ReasoningEffortMinimal, nil
	case ReasoningEffortLow:
		return ReasoningEffortLow, nil
	case ReasoningEffortMedium:
		return ReasoningEffortMedium, nil
	case ReasoningEffortHigh:
		return ReasoningEffortHigh, nil
	case ReasoningEffortMax:
		return ReasoningEffortMax, nil
	default:
		return "", fmt.Errorf("reasoning effort must be minimal, low, medium, high, or max")
	}
}

// SupportsThinkingControl reports whether the currently served model has a
// reasoning parser whose output can be separated safely. Gemma 4's explicit
// think path is intentionally excluded: on the current MLX template it can
// consume the entire action budget without producing a tool call.
func SupportsThinkingControl(model string) bool {
	profile, ok := ProfileForModel(model)
	return ok && profile.ThinkingControl
}

func SupportsReasoningEffortControl(model string) bool {
	profile, ok := ProfileForModel(model)
	return ok && profile.ReasoningEffortLevels == 5
}

func reasoningBudget(effort ReasoningEffort) int {
	switch effort {
	case ReasoningEffortMinimal:
		return 256
	case ReasoningEffortLow:
		return 512
	case ReasoningEffortHigh:
		return 2048
	case ReasoningEffortMax:
		return 4096
	default:
		return 1024
	}
}

// ApplyUserInferenceControls runs after family safety defaults. This order is
// important: Qwen 27B defaults to think-off for reliable tool use, while an
// explicit /think on must remain an actual request-level override.
func ApplyUserInferenceControls(req *ChatRequest) {
	profile, _ := ProfileForModel(req.Model)
	if profile.Family == "deepseek" {
		req.EnableThinking, req.ChatTemplateKwargs = nil, nil
		req.Thinking, req.ReasoningEffort = nil, ""
		switch req.UserThinkingMode {
		case ThinkingOn:
			req.Thinking = &ThinkingConfig{Type: "enabled"}
			req.ReasoningEffort = deepSeekReasoningEffort(req.UserReasoningEffort)
		case ThinkingOff:
			req.Thinking = &ThinkingConfig{Type: "disabled"}
		}
		return
	}
	// A reused request must never carry another provider's native control.
	req.Thinking = nil
	if profile.Family == "xai" {
		req.EnableThinking = nil
		req.ChatTemplateKwargs = nil
		req.ReasoningEffort = ""
		if req.Model != "grok-build-0.1" {
			switch req.UserThinkingMode {
			case ThinkingOn:
				req.ReasoningEffort = req.UserReasoningEffort
				if req.ReasoningEffort == "" {
					req.ReasoningEffort = ReasoningEffortMedium
				}
				if req.ReasoningEffort == ReasoningEffortMinimal {
					req.ReasoningEffort = ReasoningEffortLow
				}
				if req.ReasoningEffort == ReasoningEffortMax {
					req.ReasoningEffort = "xhigh"
				}
			case ThinkingOff:
				req.ReasoningEffort = ReasoningEffortLow
				if req.Model == "grok-4.3" {
					req.ReasoningEffort = "none"
				}
			}
		}
		return
	}
	isMuse := profile.Family == "muse"
	if req.ProtocolRecovery && IsOrnithModel(req.Model) && req.UserThinkingMode != ThinkingOn {
		// Unsolicited hidden output may use the legacy compatibility fallback.
		// An explicit On request keeps its selected effort through recovery.
		disabled := false
		req.EnableThinking = &disabled
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		req.ChatTemplateKwargs["enable_thinking"] = false
		req.ChatTemplateKwargs["preserve_thinking"] = false
		delete(req.ChatTemplateKwargs, "reasoning_budget")
		req.ReasoningEffort = ""
		return
	}
	switch req.UserThinkingMode {
	case ThinkingOn:
		_, effort := NormalizeEffortSelection(ThinkingOn, req.UserReasoningEffort)
		outputLimit := 0
		if req.MaxTokens != nil {
			outputLimit = *req.MaxTokens
		}
		resolved := ResolveEffortProfile(req.Model, effort, outputLimit)
		enabled := true
		if SupportsReasoningEffortControl(req.Model) {
			enabled = resolved.ThinkingMode == ThinkingOn
		}
		req.EnableThinking = &enabled
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		req.ChatTemplateKwargs["enable_thinking"] = enabled
		req.ChatTemplateKwargs["preserve_thinking"] = enabled
		if SupportsReasoningEffortControl(req.Model) {
			if enabled {
				req.ChatTemplateKwargs["reasoning_budget"] = resolved.EffectiveThinkingTokens
			} else {
				delete(req.ChatTemplateKwargs, "reasoning_budget")
			}
			req.ReasoningEffort = ""
		} else if isMuse {
			// Muse's native template names the same three-level control
			// reasoning_strength. Sending only OpenAI's reasoning_effort leaves
			// the server-side default unchanged, so adapt it at the family edge.
			req.ChatTemplateKwargs["reasoning_strength"] = string(effort)
		} else {
			req.ReasoningEffort = ""
		}
	case ThinkingOff:
		disabled := false
		req.EnableThinking = &disabled
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		req.ChatTemplateKwargs["enable_thinking"] = false
		req.ChatTemplateKwargs["preserve_thinking"] = false
		delete(req.ChatTemplateKwargs, "reasoning_budget")
		// Muse still needs its supported strength value when the visible
		// reasoning channel is disabled; other SGLang families normalize
		// reasoning_effort=none to the no-thinking template state.
		if isMuse {
			effort := req.UserReasoningEffort
			if effort == "" {
				effort = ReasoningEffortMedium
			}
			req.ChatTemplateKwargs["reasoning_strength"] = string(effort)
		} else {
			req.ReasoningEffort = ""
		}
	}
}

func deepSeekReasoningEffort(effort ReasoningEffort) ReasoningEffort {
	switch effort {
	case ReasoningEffortMinimal, ReasoningEffortLow:
		return ReasoningEffortLow
	case ReasoningEffortMax:
		return ReasoningEffortMax
	default:
		return ReasoningEffortHigh
	}
}
