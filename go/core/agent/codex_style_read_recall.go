package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// This run-local cache restores already-authorized observations, never actions.
// Keep both memory and recovery bounded independently of model output/turn limits.
const codexStyleReadRecallBytes = 4 << 20
const codexStyleReadRecallLimit = 4

type codexStyleReadObservation struct {
	callID, toolName, content  string
	visible, exposed, restored bool
}

type codexStyleReadRecall struct {
	observations    map[string]*codexStyleReadObservation
	bytes, restores int
}

func codexStyleReadRecallKey(executor *ToolExecutor, call provider.ToolCall) (string, bool) {
	if executor == nil || executor.Registry == nil {
		return "", false
	}
	tool, ok := executor.Registry.Get(call.Function.Name)
	if !ok || tool == nil {
		return "", false
	}
	args, err := decodeToolArguments(tool, call.Function.Arguments)
	if err != nil || tools.CallKind(tool, args) != tools.KindReadOnly {
		return "", false
	}
	identity, ok := executor.readIdentity(tool, args)
	if !ok || identity.Key == "" || identity.Version == "" {
		return "", false
	}
	return call.Function.Name + "\x00" + identity.Key + "\x00" + identity.Version, true
}

func (r *codexStyleReadRecall) observe(executor *ToolExecutor, call provider.ToolCall, execution ToolExecution) {
	if !execution.Executed || execution.Status != ToolOK || execution.Kind != tools.KindReadOnly || execution.Effect != EffectRead || len(execution.ContentParts) > 0 || strings.TrimSpace(execution.LLMContent) == "" {
		return
	}
	key, ok := codexStyleReadRecallKey(executor, call)
	if !ok {
		return
	}
	if r.observations == nil {
		r.observations = make(map[string]*codexStyleReadObservation)
	}
	// Another call with reordered JSON must not replenish an identity's credit.
	if _, exists := r.observations[key]; exists {
		return
	}
	if r.bytes+len(execution.LLMContent) > codexStyleReadRecallBytes {
		return
	}
	r.bytes += len(execution.LLMContent)
	r.observations[key] = &codexStyleReadObservation{callID: execution.CallID, toolName: call.Function.Name, content: execution.LLMContent}
}

// Called synchronously at each final dispatch, including upstream capacity
// retries. Never retain request bodies or expose them through diagnostics.
func (r *codexStyleReadRecall) admitted(request provider.ChatRequest) {
	for _, observation := range r.observations {
		observation.visible = false
		observation.exposed = false
		for _, tool := range request.Tools {
			observation.exposed = observation.exposed || tool.Function.Name == observation.toolName
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && strings.Contains(message.Content, observation.content) {
				observation.visible = true
				break
			}
		}
	}
}

func (r *codexStyleReadRecall) restore(ctx context.Context, executor *ToolExecutor, call provider.ToolCall) (string, bool) {
	if ctx.Err() != nil || runTokenBudgetExhausted(ctx) || r.restores >= codexStyleReadRecallLimit {
		return "", false
	}
	key, ok := codexStyleReadRecallKey(executor, call)
	if !ok {
		return "", false
	}
	observation := r.observations[key]
	if observation == nil || !observation.exposed || observation.visible || observation.restored {
		return "", false
	}
	observation.restored = true
	r.restores++
	return fmt.Sprintf("[Restored earlier read observation from call %s; exact historical content follows. No new read or other action was executed. This is not a fresh state check.]\n%s\n[End earlier read observation.]", observation.callID, observation.content), true
}
