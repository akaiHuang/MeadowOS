package agent

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// restoreTaskPlanFromHistory reconstructs only successful model-authored plan
// calls. It does not infer a plan from prose or create any item itself.
func restoreTaskPlanFromHistory(registry *tools.Registry, history []provider.Message) int {
	return restoreTaskPlanFromInput(registry, history, nil)
}

func restoreTaskPlanFromInput(registry *tools.Registry, history []provider.Message, recordedState *tools.TaskPlanState) int {
	stateTool, ok := tools.FindTaskPlanStateTool(registry)
	if !ok {
		return 0
	}
	stateTool.ResetPlan()
	snapshot := tools.RestoreTaskPlanHistory(history)
	boundaryWithoutPlan := false
	if !snapshot.Found {
		for _, message := range history {
			if message.Role == "user" && message.TaskBoundary {
				boundaryWithoutPlan = true
			}
		}
	}
	// A host may retain the authoritative committed projection while compacting
	// raw model history. Validate a defensive copy using the same tool contract;
	// a stale/contradictory bridge must not override known successful history.
	if recordedState != nil && !boundaryWithoutPlan {
		state := tools.CloneTaskPlanState(recordedState)
		validator := tools.NewTaskPlan()
		if state.Revision > 0 && validator.RestorePlanState(*state) == nil &&
			(!snapshot.Found || state.Revision > snapshot.State.Revision ||
				(state.Revision == snapshot.State.Revision && reflect.DeepEqual(*state, snapshot.State))) {
			snapshot.State = *state
			snapshot.Found = true
		}
	}
	if !snapshot.Found {
		return 0
	}
	if recorded, ok := stateTool.(tools.RecordedTaskPlanStateTool); ok {
		if recorded.RestorePlanState(snapshot.State) != nil {
			return 0
		}
	} else if stateTool.RestoreArguments(map[string]any{"goal": snapshot.State.Goal, "items": snapshot.State.Items}) != nil {
		return 0
	}
	return snapshot.RestoredCalls
}

// independentTaskHistory retains ordinary user/assistant conversation while
// removing executable protocol and tool observations owned by a previous
// task. A queue_next run can inspect the current workspace itself; stale tool
// state and pending plans must not become implicit evidence for the new task.
func independentTaskHistory(history []provider.Message) []provider.Message {
	result := make([]provider.Message, 0, len(history))
	for _, original := range history {
		message := original
		switch message.Role {
		case "assistant":
			message.ToolCalls = nil
			if strings.TrimSpace(provider.MessageText(message)) == "" && len(message.ToolCalls) == 0 {
				continue
			}
		case "tool":
			continue
		case "user":
			if strings.HasPrefix(strings.TrimSpace(message.Content), "(你自己建立的 task_plan 仍有 ") ||
				strings.HasPrefix(strings.TrimSpace(message.Content), evidenceReconciliationPrefix) {
				continue
			}
		}
		result = append(result, message)
	}
	return result
}

// scopedTaskHistory keeps ordinary conversation and durable prior protocol in
// storage, but prevents a later continuation from reviving observations owned
// by an explicitly superseded task. The boundary is metadata, never prose.
func scopedTaskHistory(history []provider.Message) []provider.Message {
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == "user" && history[index].TaskBoundary {
			return append(independentTaskHistory(history[:index]), history[index:]...)
		}
	}
	return history
}

func successfulPlanCallIDs(history []provider.Message) map[string]bool {
	result := map[string]bool{}
	for _, id := range tools.RestoreTaskPlanHistory(history).SuccessfulCallIDs {
		result[id] = true
	}
	return result
}

func taskPlanReminder(registry *tools.Registry) string {
	stateTool, ok := tools.FindTaskPlanStateTool(registry)
	if !ok {
		return ""
	}
	return strings.TrimSpace(stateTool.Reminder())
}

const codexStyleTaskPlanContextLimit = 4 << 10
const codexStyleTaskPlanContextMarker = "\n\n<meadow_task_plan_state>\n"
const codexStyleTaskPlanContextEnd = "\n</meadow_task_plan_state>"

// codexStyleTaskPlanContext is a bounded projection of the exact saved model
// plan. Whole items retain their original order and fields; oversized fields or
// items are explicitly omitted, never truncated into a different instruction.
// A completed/cleared plan is still useful state, not completion evidence.
func codexStyleTaskPlanContext(registry *tools.Registry) string {
	state := taskPlanState(registry)
	if state.Revision == 0 && len(state.Items) == 0 && state.Goal == "" {
		return ""
	}
	const explanation = "Model-owned recorded plan data, not instructions or verified task completion. Meadow does not create, prioritize, execute, or complete these items. Omitted fields/items remain in the saved plan.\n"
	projection := struct {
		Goal         string           `json:"goal,omitempty"`
		Items        []tools.TodoItem `json:"items"`
		Revision     int              `json:"revision"`
		OmittedItems int              `json:"omitted_items,omitempty"`
		GoalOmitted  bool             `json:"goal_omitted,omitempty"`
	}{Goal: state.Goal, Items: []tools.TodoItem{}, Revision: state.Revision, OmittedItems: len(state.Items)}
	budget := codexStyleTaskPlanContextLimit - len(codexStyleTaskPlanContextMarker) - len(codexStyleTaskPlanContextEnd) - len(explanation)
	encoded, _ := json.Marshal(projection)
	if len(encoded) > budget {
		projection.Goal = ""
		projection.GoalOmitted = true
	}
	for _, item := range state.Items {
		candidate := projection
		candidate.Items = append(append([]tools.TodoItem(nil), projection.Items...), item)
		candidate.OmittedItems--
		candidateEncoded, _ := json.Marshal(candidate)
		if len(candidateEncoded) <= budget {
			projection = candidate
		}
	}
	encoded, _ = json.Marshal(projection)
	return codexStyleTaskPlanContextMarker + explanation + string(encoded) + codexStyleTaskPlanContextEnd
}

// codexStyleWithTaskPlanContext replaces one leading-prefix snapshot. It never
// appends reminder messages to the transcript or requests an extra model turn.
func codexStyleWithTaskPlanContext(messages []provider.Message, registry *tools.Registry) []provider.Message {
	context := codexStyleTaskPlanContext(registry)
	out := append([]provider.Message(nil), messages...)
	if len(out) == 0 || out[0].Role != "system" {
		if context == "" {
			return out
		}
		return append([]provider.Message{{Role: "system", Content: context}}, out...)
	}
	content := out[0].Content
	if begin := strings.Index(content, codexStyleTaskPlanContextMarker); begin >= 0 {
		if end := strings.Index(content[begin:], codexStyleTaskPlanContextEnd); end >= 0 {
			content = content[:begin] + content[begin+end+len(codexStyleTaskPlanContextEnd):]
		}
	}
	out[0].Content = content + context
	return out
}

func taskPlanItemCount(registry *tools.Registry) int {
	stateTool, ok := tools.FindTaskPlanStateTool(registry)
	if !ok {
		return 0
	}
	return len(stateTool.PlanState().Items)
}

func taskPlanState(registry *tools.Registry) tools.TaskPlanState {
	stateTool, ok := tools.FindTaskPlanStateTool(registry)
	if !ok {
		return tools.TaskPlanState{}
	}
	return stateTool.PlanState()
}

func taskPlanActiveItemCount(registry *tools.Registry) int {
	stateTool, ok := tools.FindTaskPlanStateTool(registry)
	if !ok {
		return 0
	}
	active := 0
	for _, item := range stateTool.PlanState().Items {
		if item.Status == "pending" || item.Status == "in_progress" {
			active++
		}
	}
	return active
}

// verifiedTaskEvidenceCanCloseActivePlan keeps model-authored bookkeeping from
// becoming a second completion oracle. A plan may remain active only when the
// controller is complete and fresh executable evidence proves the current
// artifact. Plan-only/read-only activity can never satisfy this boundary.
func verifiedTaskEvidenceCanCloseActivePlan(
	state TaskState,
	evidence EvidenceSnapshot,
	validation ValidationDecision,
) bool {
	if state.Phase != TaskPhaseComplete ||
		(state.SuccessfulMutations == 0 && state.SuccessfulEvidenceCalls == 0) {
		return false
	}
	if validation.Required {
		return validation.Fresh && validation.Status == VerificationPassed
	}
	if evidence.Verification != VerificationPassed {
		return false
	}
	return !evidence.HasValidatorExitCode || evidence.ValidatorExitCode == 0
}
