package agent

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// Exact pinned Raven CuratorSegmentBuilder._system_prompt() bytes. This is
// an internal context seat; its tool schemas never join the public main-agent
// registry. See the extracted-assets/source manifest for hash and provenance.
//
//go:embed raven_prompts/CURATOR.md
var ravenCuratorSystemPrompt string

const ravenCuratorMaxSteps = 12

// The pinned upstream config declares 30 seconds without enforcing it in the
// active builder. The native adapter DOES enforce this child stage bound,
// under the original work context/deadline/token meter; it extends nothing.
const ravenCuratorStageTimeout = 30 * time.Second

type ravenCurationMetadata struct {
	Path    string
	Steps   int
	Verdict string
}

func (c *RavenMemoryCurator) curateInitialContext(ctx context.Context, input RavenMemoryInput, messages []provider.Message, action RavenMemorySummarize) ([]provider.Message, ravenCurationMetadata, error) {
	meta := ravenCurationMetadata{Path: "fast", Verdict: "below_history_threshold"}
	handoff, _ := ctx.Value(runHandoffKey{}).(runHandoffState)
	if input.Phase == "handoff" || input.Phase == "reporting" || handoff.final || runTokenBudgetExhausted(ctx) {
		meta.Path, meta.Verdict = "skipped", "resource_boundary"
		return nil, meta, nil
	}
	if ctx.Err() != nil {
		return nil, meta, ctx.Err()
	}
	if c.curationPrepared {
		meta.Path, meta.Verdict = "reused", "initial_context_already_assembled"
		return nil, meta, nil
	}
	c.curationPrepared = true
	// Fixed overhead is the complete native prefix/host observation segments
	// plus actually advertised schemas. PromptTokenLimit has already reserved
	// the main generation ceiling; it is never expanded by this calculation.
	historyStart := ravenMemoryHistoryStart(messages)
	fixed := EstimateRequestPromptTokens(provider.ChatRequest{Messages: messages[:historyStart], Tools: input.Tools})
	historyTokens := messagesEstimatedTokens(messages[historyStart:])
	available := input.PromptTokenLimit - fixed
	if input.PromptTokenLimit <= 0 || available <= 0 || historyTokens < available*60/100 || action == nil {
		return nil, meta, nil
	}
	protected := ravenMemoryProtected(messages, input.PreserveRecentMessages)
	head, _ := ravenMemorySummaryHead(messages, protected)
	if len(head) < 2 {
		meta.Verdict = "no_selectable_history"
		return nil, meta, nil
	}
	stageCtx, cancel := context.WithTimeout(ctx, ravenCuratorStageTimeout)
	defer cancel()
	state, err := ravenCuratorBoundedIO(stageCtx, nativeRavenCuratorIO, func() (*ravenCuratorState, error) {
		return newRavenCuratorState(input, messages, protected)
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, meta, ctx.Err()
		}
		if stageCtx.Err() != nil {
			meta.Path, meta.Verdict = "fallback", "stage_time_limit"
			return nil, meta, nil
		}
		meta.Path, meta.Verdict = "fallback", "private_state_unavailable"
		return nil, meta, nil
	}
	payload := state.Payload()
	if len(payload) > ravenMemoryMaxSourceBytes {
		meta.Path, meta.Verdict = "fallback", "manifest_source_bound"
		return nil, meta, nil
	}
	internal := []provider.Message{{Role: "system", Content: ravenCuratorSystemPrompt}, {Role: "user", Content: payload}}
	schemas := state.Tools()
	allowed := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		allowed[schema.Function.Name] = true
	}
	meta.Path, meta.Verdict = "slow", "step_limit"
	for step := 1; step <= ravenCuratorMaxSteps; step++ {
		if ctx.Err() != nil {
			return nil, meta, ctx.Err()
		}
		if stageCtx.Err() != nil {
			meta.Path, meta.Verdict = "fallback", "stage_time_limit"
			return nil, meta, nil
		}
		maxTokens := 2048
		temperature := 0.1
		request := provider.ChatRequest{Model: input.ModelID, Messages: internal, Tools: schemas, ToolChoice: "auto", MaxTokens: &maxTokens, Temperature: &temperature}
		prompt := EstimateRequestPromptTokens(request)
		// Reserve response room conservatively in the same physical model
		// window. If the internal manifest/tools cannot fit, pay for no call.
		if input.PromptTokenLimit > 0 {
			maxTokens = min(maxTokens, input.PromptTokenLimit-prompt-prompt/20-64)
		}
		if maxTokens < 128 {
			meta.Path, meta.Verdict = "fallback", "internal_request_no_room"
			return nil, meta, nil
		}
		meta.Steps = step
		response, callErr := action(stageCtx, request)
		if ctx.Err() != nil {
			return nil, meta, ctx.Err()
		}
		if errors.Is(callErr, ErrRunTokenBudget) || errors.Is(callErr, ErrRunTimeBudget) || errors.Is(callErr, ErrRunHandoffReserve) {
			return nil, meta, callErr
		}
		if stageCtx.Err() != nil {
			meta.Path, meta.Verdict = "fallback", "stage_time_limit"
			return nil, meta, nil
		}
		if callErr != nil || response == nil || response.Truncated || response.FinishReason == "length" || response.FinishReason == "error" || len(response.ToolCalls) == 0 || len(response.ToolCalls) > 8 {
			meta.Path, meta.Verdict = "fallback", "invalid_internal_response"
			return nil, meta, nil
		}
		ids := make(map[string]bool, len(response.ToolCalls))
		for _, call := range response.ToolCalls {
			if strings.TrimSpace(call.ID) == "" || ids[call.ID] || !allowed[call.Function.Name] {
				meta.Path, meta.Verdict = "fallback", "invalid_internal_tool_call"
				return nil, meta, nil
			}
			ids[call.ID] = true
		}
		// Private continuation is carried only within this same-model internal
		// tool exchange. It is excluded from manifests, archives and UI output.
		internal = append(internal, provider.Message{Role: "assistant", Content: response.Content, ToolCalls: response.ToolCalls, ProviderReasoning: response.ProviderReasoning})
		var acceptedBatch []provider.Message
		for _, call := range response.ToolCalls {
			if ctx.Err() != nil {
				return nil, meta, ctx.Err()
			}
			type executionResult struct {
				content  string
				accepted []provider.Message
			}
			executed, toolErr := ravenCuratorBoundedIO(stageCtx, nativeRavenCuratorIO, func() (executionResult, error) {
				content, accepted, err := state.Execute(stageCtx, call)
				return executionResult{content: content, accepted: accepted}, err
			})
			result, accepted := executed.content, executed.accepted
			if ctx.Err() != nil {
				return nil, meta, ctx.Err()
			}
			if stageCtx.Err() != nil {
				meta.Path, meta.Verdict = "fallback", "stage_time_limit"
				return nil, meta, nil
			}
			if errors.Is(toolErr, errRavenCuratorIOBusy) {
				meta.Path, meta.Verdict = "fallback", "private_io_capacity_occupied"
				return nil, meta, nil
			}
			if toolErr != nil {
				// Schema/selection errors are factual internal observations. The
				// bounded loop can correct them; no public tool is ever executed.
				result = string(ravenMemoryJSON(map[string]any{"status": "error", "error": toolErr.Error()}))
			}
			if len(result) > ravenMemoryMaxSourceBytes {
				meta.Path, meta.Verdict = "fallback", "internal_result_bound"
				return nil, meta, nil
			}
			internal = append(internal, provider.Message{Role: "tool", ToolCallID: call.ID, Content: result})
			if accepted != nil && toolErr == nil && call.Function.Name == "curator_build_context" {
				acceptedBatch = accepted
			}
		}
		// Complete this private assistant/result batch before returning its
		// accepted plan. No sibling call/result is left dangling, and Stop
		// remains authoritative even after an earlier build in the batch.
		if acceptedBatch != nil {
			meta.Verdict = "accepted_build_context"
			return acceptedBatch, meta, nil
		}
	}
	meta.Path = "fallback"
	return nil, meta, nil
}
