package provider

// EffortProfile describes a resource policy, not a measured correctness claim.
// The same resolver supplies request construction and the session-control UI.
type EffortProfile struct {
	Effort                  ReasoningEffort `json:"effort"`
	Label                   string          `json:"label"`
	Description             string          `json:"description"`
	ThinkingMode            ThinkingMode    `json:"thinkingMode"`
	RequestedThinkingTokens int             `json:"requestedThinkingTokens"`
	EffectiveThinkingTokens int             `json:"effectiveThinkingTokens"`
	MaxOutputTokens         int             `json:"maxOutputTokens"`
	AnswerReserveTokens     int             `json:"answerReserveTokens"`
	BudgetLimited           bool            `json:"budgetLimited"`
}

const effortAnswerReserveTokens = 2048

func ThinkingModeForEffort(effort ReasoningEffort) ThinkingMode {
	if effort == ReasoningEffortMinimal {
		return ThinkingOff
	}
	return ThinkingOn
}

// NormalizeEffortSelection preserves legacy Auto/Off choices. Deliberately
// enabling thinking after selecting rapid mode chooses the first thinking tier.
func NormalizeEffortSelection(mode ThinkingMode, effort ReasoningEffort) (ThinkingMode, ReasoningEffort) {
	if mode == "" {
		mode = ThinkingAuto
	}
	if effort == "" {
		effort = ReasoningEffortMedium
	}
	if mode == ThinkingOn && effort == ReasoningEffortMinimal {
		effort = ReasoningEffortLow
	}
	return mode, effort
}

func EffortProfiles(model string, outputTokenLimit int) []EffortProfile {
	if !SupportsReasoningEffortControl(model) {
		return nil
	}
	levels := []ReasoningEffort{ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortMax}
	profiles := make([]EffortProfile, 0, len(levels))
	for _, effort := range levels {
		profiles = append(profiles, ResolveEffortProfile(model, effort, outputTokenLimit))
	}
	return profiles
}

func ResolveEffortProfile(model string, effort ReasoningEffort, outputTokenLimit int) EffortProfile {
	_, effort = NormalizeEffortSelection(ThinkingAuto, effort)
	profile := EffortProfile{Effort: effort, ThinkingMode: ThinkingModeForEffort(effort)}
	switch effort {
	case ReasoningEffortMinimal:
		profile.Label, profile.Description = "急速", "聊天與簡單問答，優先快速回覆。"
	case ReasoningEffortLow:
		profile.Label, profile.Description = "輕量", "簡單解題與小幅修改，投入較少推理。"
	case ReasoningEffortHigh:
		profile.Label, profile.Description = "深入", "複雜除錯與多步工作，投入更多推理。"
	case ReasoningEffortMax:
		profile.Label, profile.Description = "全力", "困難任務，使用目前容量允許的較多推理。"
	default:
		profile.Label, profile.Description = "標準", "一般開發與問題分析，平衡等待與推理。"
	}
	if modelProfile, _ := ProfileForModel(model); modelProfile.Family == "xai" {
		profile.MaxOutputTokens = outputTokenLimit
		profile.ThinkingMode = ThinkingOn
		level := string(effort)
		if effort == ReasoningEffortMinimal {
			level = "low"
			if model == "grok-4.3" {
				level = "none"
			}
			profile.ThinkingMode = ThinkingOff
		}
		if effort == ReasoningEffortMax {
			level = "xhigh"
		}
		profile.Description = "xAI 原生 reasoning_effort=" + level + "；由模型服務控制推理，沒有本機 thinking token 預算。"
		return profile
	}
	if IsDeepSeekModel(model) {
		profile.MaxOutputTokens = outputTokenLimit
		if effort == ReasoningEffortMinimal {
			profile.Description = "DeepSeek 原生非思考模式；直接產生回答與工具呼叫。"
		} else {
			profile.Description = "DeepSeek 原生 reasoning_effort=" + string(deepSeekReasoningEffort(effort)) + "；由模型服務控制推理，沒有本機 thinking token 預算。"
		}
		return profile
	}
	// Use the model family's actual request ceiling. This is deliberately not
	// the checkpoint's architectural context maximum or an invented capability.
	request := ChatRequest{Model: model}
	if outputTokenLimit > 0 {
		request.MaxTokens = &outputTokenLimit
	}
	ApplyLocalModelSafetyOverrides(&request)
	if request.MaxTokens != nil {
		profile.MaxOutputTokens = *request.MaxTokens
	}
	if profile.MaxOutputTokens > 0 {
		profile.AnswerReserveTokens = min(effortAnswerReserveTokens, max(1, profile.MaxOutputTokens/2))
	}
	if !SupportsReasoningEffortControl(model) || effort == ReasoningEffortMinimal {
		profile.ThinkingMode = ThinkingOff
		return profile
	}
	requested := reasoningBudget(effort)
	if IsOrnithModel(model) {
		switch effort {
		case ReasoningEffortLow:
			requested = 1024
		case ReasoningEffortHigh:
			requested = 4096
		case ReasoningEffortMax:
			requested = 6144
		default:
			requested = 2048
		}
	}
	profile.RequestedThinkingTokens = requested
	profile.EffectiveThinkingTokens = min(requested, max(0, profile.MaxOutputTokens-profile.AnswerReserveTokens))
	profile.BudgetLimited = profile.EffectiveThinkingTokens < requested
	return profile
}

// ResolveInferenceEffortProfile resolves the effective selection, including an
// explicit Off or a family default. Auto never pretends the saved medium tier
// is active. A native default with no advertised token budget stays Auto.
func ResolveInferenceEffortProfile(model string, mode ThinkingMode, effort ReasoningEffort, outputTokenLimit int) EffortProfile {
	mode, effort = NormalizeEffortSelection(mode, effort)
	profile := ResolveEffortProfile(model, effort, outputTokenLimit)
	if mode == ThinkingOn {
		return profile
	}
	profile.ThinkingMode = mode
	if mode == ThinkingAuto {
		request := ChatRequest{Model: model}
		ApplyLocalModelSafetyOverrides(&request)
		if request.EnableThinking != nil {
			if *request.EnableThinking {
				profile.ThinkingMode = ThinkingOn
			} else {
				profile.ThinkingMode = ThinkingOff
			}
		}
	}
	profile.RequestedThinkingTokens, profile.EffectiveThinkingTokens = 0, 0
	profile.BudgetLimited = false
	if profile.ThinkingMode == ThinkingOff {
		profile.Effort = ReasoningEffortMinimal
		profile.Label, profile.Description = "急速", "聊天與簡單問答，優先快速回覆。"
	} else if profile.ThinkingMode == ThinkingAuto {
		profile.Effort = ReasoningEffort("auto")
		profile.Label, profile.Description = "模型預設", "沿用此模型的原生設定；沒有指定推理預算。"
	}
	return profile
}
