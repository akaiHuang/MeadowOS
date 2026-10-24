package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// Bounded adaptation of raven/agent/loop/recovery.py. Recovery scaffolding
// belongs to the current request only; it never creates a user goal or a
// successful assistant reply in canonical history.
type ravenEmptyRecovery struct {
	nudges, prefills, retries int
}

func ravenEmptyStream(stream *provider.StreamResult) bool {
	return stream != nil && len(stream.ToolCalls) == 0 && strings.TrimSpace(stream.Content) == ""
}

func (s *ravenEmptyRecovery) prepare(request provider.ChatRequest, stream *provider.StreamResult, thoughtOnly, hadTools bool) (provider.ChatRequest, string, bool) {
	if !ravenEmptyStream(stream) {
		return request, "", false
	}
	// The native provider exposes private continuation only for the same
	// DeepSeek model. Other families use the non-prefill ladder: never turn
	// hidden thoughts into public assistant content or guess vendor support.
	private := stream.ProviderReasoning
	profile, _ := provider.ProfileForModel(request.Model)
	if len(request.Tools) > 0 && request.UserThinkingMode != provider.ThinkingOff && thoughtOnly && private != nil && private.Model == request.Model && private.Content != "" && profile.Family == "deepseek" && s.prefills < 2 {
		s.prefills++
		request.Messages = append(append([]provider.Message(nil), request.Messages...), provider.Message{Role: "assistant", ProviderReasoning: private, RuntimeObservation: true})
		return request, "thinking_prefill", true
	}
	if hadTools && s.nudges < 1 {
		s.nudges++
		request.Messages = append(append([]provider.Message(nil), request.Messages...),
			provider.Message{Role: "assistant", Content: "(empty)", RuntimeObservation: true},
			provider.Message{Role: "user", RuntimeObservation: true, Content: "You executed tool calls but returned an empty response. Use the tool results above to continue the task, or give your final answer now."})
		return request, "post_tool_empty_nudge", true
	}
	if s.retries >= 3 {
		return request, "empty_recovery_exhausted", false
	}
	wireRequest := request
	if limit := stream.OutputTokenLimit; limit > 0 && (wireRequest.MaxTokens == nil || *wireRequest.MaxTokens > limit) {
		// The provider clamps output before applying reasoning controls. Use
		// its actual last admission to avoid paying for collapsed wire rungs.
		wireRequest.MaxTokens = &limit
	}
	wireKey := ravenReasoningWireKey(wireRequest)
	current := request.UserReasoningEffort
	if current == "" {
		current = provider.ReasoningEffortMedium
	}
	ladder := []provider.ReasoningEffort{provider.ReasoningEffortMax, provider.ReasoningEffortHigh, provider.ReasoningEffortMedium, provider.ReasoningEffortLow, provider.ReasoningEffortMinimal}
	index := -1
	for i, effort := range ladder {
		if effort == current {
			index = i
			break
		}
	}
	if index < 0 {
		return request, "no_supported_reasoning_rung", false
	}
	for _, effort := range ladder[index+1:] {
		candidate := request
		candidate.UserReasoningEffort = effort
		wireCandidate := candidate
		wireCandidate.MaxTokens = wireRequest.MaxTokens
		if ravenReasoningWireKey(wireCandidate) == wireKey {
			continue
		}
		s.retries++
		candidate.Messages = append([]provider.Message(nil), request.Messages...)
		if stream.Truncated || strings.EqualFold(stream.FinishReason, "length") {
			candidate.Messages = append(candidate.Messages,
				provider.Message{Role: "assistant", Content: "(empty)", RuntimeObservation: true},
				provider.Message{Role: "user", RuntimeObservation: true, Content: "Your previous turn was cut off at the output token limit before it produced any reply or tool call, so nothing from it was kept. Keep this turn short: answer briefly, or make the tool call the task needs. If a payload you were writing is too large to finish in one call, send it across several smaller calls instead of one."})
		}
		return candidate, "empty_reasoning_descent", true
	}
	return request, "no_distinct_reasoning_wire_rung", false
}

func ravenReasoningWireKey(request provider.ChatRequest) string {
	if request.ChatTemplateKwargs != nil {
		copy := make(map[string]any, len(request.ChatTemplateKwargs))
		for key, value := range request.ChatTemplateKwargs {
			copy[key] = value
		}
		request.ChatTemplateKwargs = copy
	}
	provider.ApplyUserInferenceControls(&request)
	key, _ := json.Marshal(struct {
		Effort   provider.ReasoningEffort `json:"effort"`
		Thinking any                      `json:"thinking"`
		Enable   *bool                    `json:"enable"`
		Template map[string]any           `json:"template"`
	}{request.ReasoningEffort, request.Thinking, request.EnableThinking, request.ChatTemplateKwargs})
	return string(key)
}

type ravenToolFailureStreak struct {
	key           string
	count, nudges int
}

func (s *ravenToolFailureStreak) observe(execution ToolExecution) string {
	raw := strings.TrimSpace(execution.LLMContent)
	low := strings.ToLower(raw)
	for _, marker := range []string{"429", "rate limit", "timed out", "timeout", "no healthy upstream", "502", "503"} {
		if strings.Contains(low, marker) {
			s.key, s.count = "", 0
			return ""
		}
	}
	var envelope struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal([]byte(raw), &envelope)
	class := ""
	if envelope.Error != "" {
		class = "envelope:" + strings.Join(strings.Fields(strings.ToLower(envelope.Error)), " ")
	} else if execution.Status != ToolOK && execution.Status != ToolCancelled {
		class = string(execution.Status)
		if envelope.Code != "" {
			class += ":" + envelope.Code
		} else {
			prefix := low[:min(200, len(low))]
			for _, marker := range []string{"[truncated]", "[incomplete arguments]", "[invalid arguments]", "invalid parameters", "not found", "not available", "permission", "denied"} {
				if strings.Contains(prefix, marker) {
					class += ":" + marker
					break
				}
			}
		}
	}
	if class == "" || strings.TrimSuffix(low, ".") == "no matches found" || strings.TrimSuffix(low, ".") == "no files found" {
		s.key, s.count = "", 0
		return ""
	}
	key := execution.Tool + "\x00" + class
	if key != s.key {
		s.key, s.count = key, 1
	} else {
		s.count++
	}
	if s.count < 2 || s.nudges >= 2 {
		return ""
	}
	s.nudges++
	s.count = 0
	return fmt.Sprintf("[Raven tool failure observation] %s returned the same deterministic failure class twice. The failure is still unresolved. Reconsider the observed cause before another action, or report the concrete blocker. This observation does not authorize additional permissions or extend the run budget.", execution.Tool)
}
