package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const taskPlanHistoryPayloadLimit = 128 << 10
const taskPlanLegacyCanonicalMarker = "\nTask plan recorded state:\n"

// TaskPlanHistorySnapshot is reconstructed from committed assistant calls and
// their uniquely paired, successful tool results. Found says that a plan action
// was recorded, including an explicit clear; it does not attest task completion.
type TaskPlanHistorySnapshot struct {
	State             TaskPlanState
	Found             bool
	RestoredCalls     int
	CallID            string
	ToolName          string
	OutputSHA256      string
	SuccessfulCallIDs []string
}

// RestoreTaskPlanHistory never reads assistant prose, draft events, or a plan
// summary produced by history compaction. Ambiguous/reused IDs, duplicate
// results, failed results and results preceding their call are not replayed.
func RestoreTaskPlanHistory(history []provider.Message) TaskPlanHistorySnapshot {
	scopedStart := 0
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == "user" && history[index].TaskBoundary {
			scopedStart = index
			break
		}
	}
	callCounts := map[string]int{}
	resultCounts := map[string]int{}
	for _, message := range history {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if call.ID != "" {
					callCounts[call.ID]++
				}
			}
		} else if message.Role == "tool" && message.ToolCallID != "" {
			resultCounts[message.ToolCallID]++
		}
	}
	history = history[scopedStart:]
	stateTool := NewTaskPlan()
	pending := map[string]provider.ToolCall{}
	canonicalByTool := map[string]TaskPlanState{}
	unversionedAliasIntervened := map[string]bool{}
	historicalLegacySeen := false
	snapshot := TaskPlanHistorySnapshot{}
	for _, message := range history {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if (call.Function.Name == TaskPlanToolName || call.Function.Name == "todo_write") &&
					call.ID != "" && callCounts[call.ID] == 1 && resultCounts[call.ID] == 1 {
					pending[call.ID] = call
				}
			}
			continue
		}
		if message.Role != "tool" {
			continue
		}
		call, exists := pending[message.ToolCallID]
		delete(pending, message.ToolCallID)
		if !exists || len(call.Function.Arguments) > taskPlanHistoryPayloadLimit || len(message.Content) > taskPlanHistoryPayloadLimit {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
			continue
		}
		content := strings.TrimSpace(message.Content)
		header, payload, hasPayload := strings.Cut(content, "\n")
		legacyListing, legacyState, legacyCanonical := strings.Cut(payload, taskPlanLegacyCanonicalMarker)
		potentialPayload := payload
		if legacyCanonical {
			potentialPayload = legacyState
		}
		if _, provided := args["goal"]; !provided {
			var recorded TaskPlanState
			if hasPayload && json.Unmarshal([]byte(strings.TrimSpace(potentialPayload)), &recorded) == nil {
				if snapshot.Found && recorded.Goal != "" && recorded.Goal != stateTool.PlanState().Goal {
					continue
				}
				args["goal"] = recorded.Goal
			} else {
				args["goal"] = "" // old tools reset omitted goals
			}
		}
		unversionedLegacy := call.Function.Name == "todo_write" && !legacyCanonical &&
			(header == "Todos have been modified successfully." || !hasPayload)
		// Before native aliases shared state, unversioned todo_write owned an
		// independent goal. Preserve that historical meaning when replaying it.
		if unversionedLegacy {
			if _, provided := args["goal"]; !provided {
				args["goal"] = ""
			}
		}
		// Validate without mutating the preceding successful snapshot. A bad
		// later call/result must leave that snapshot intact.
		candidate := NewTaskPlan()
		candidate.name = call.Function.Name
		base := stateTool.PlanState()
		if unversionedLegacy {
			base = canonicalByTool[call.Function.Name]
		}
		if candidate.RestorePlanState(base) != nil || candidate.RestoreArguments(args) != nil {
			continue
		}
		expected := candidate.PlanState()
		own := NewTaskPlan()
		own.name = call.Function.Name
		priorOwn, ownKnown := canonicalByTool[call.Function.Name]
		ownValid := own.RestorePlanState(priorOwn) == nil && own.RestoreArguments(args) == nil
		ownExpected := own.PlanState()
		canonical := false
		switch {
		case header == "Task plan state saved." || header == "Task plan state saved. You remain responsible for deciding and updating the next action.":
			if call.Function.Name != TaskPlanToolName || len(expected.Items) == 0 {
				continue
			}
			if snapshot.Found && expected.Revision == stateTool.PlanState().Revision {
				if !(historicalLegacySeen && ownValid && (!ownKnown || ownExpected.Revision != priorOwn.Revision)) {
					continue // a saved result cannot attest an unchanged action
				}
			}
			canonical = hasPayload
		case header == "Task plan has been cleared." || header == "Todo list has been cleared.":
			if len(expected.Items) != 0 || (call.Function.Name == TaskPlanToolName) != (header == "Task plan has been cleared.") {
				continue
			}
			canonical = hasPayload
		case header == "Todos have been modified successfully.":
			if call.Function.Name != "todo_write" || len(expected.Items) == 0 {
				continue
			}
			// Historical todo_write embeds its actual list in a reminder rather
			// than a state object. Validate it when present; prefix-only legacy
			// records remain replayable without inventing a recorded revision.
			if hasPayload {
				const start = "<system-reminder>\nYour todo list has changed. DO NOT mention this explicitly to the user. Here are the latest contents:\n"
				list, ok := strings.CutPrefix(strings.TrimSpace(legacyListing), start)
				list, ended := strings.CutSuffix(list, "\n</system-reminder>")
				var recorded []TodoItem
				if !ok || !ended || json.Unmarshal([]byte(list), &recorded) != nil || !reflect.DeepEqual(recorded, expected.Items) {
					continue
				}
			}
			if legacyCanonical {
				canonical, payload = true, legacyState
			}
		case header == "Task plan unchanged." || header == "Task plan unchanged. Do not submit the same plan again. Execute, inspect, or verify one active item before revising the plan." || header == taskPlanUnchangedHeader:
			if !snapshot.Found || (unversionedLegacy && !ownKnown) {
				continue
			}
			if !reflect.DeepEqual(expected.Items, stateTool.PlanState().Items) || expected.Goal != stateTool.PlanState().Goal {
				if !(historicalLegacySeen && ownKnown && ownValid && ownExpected.Revision == priorOwn.Revision) {
					continue
				}
				candidate, expected = own, ownExpected
			}
			canonical = hasPayload
		default:
			continue
		}
		if canonical {
			// The first retained canonical snapshot can begin at a later
			// revision after compaction. Once a prior success is known, every
			// revision must follow the actual changed/unchanged transition.
			var recorded TaskPlanState
			if json.Unmarshal([]byte(strings.TrimSpace(payload)), &recorded) != nil || recorded.Revision <= 0 ||
				recorded.Goal != expected.Goal || !reflect.DeepEqual(recorded.Items, expected.Items) {
				continue
			}
			if snapshot.Found && recorded.Revision != expected.Revision {
				// Old native registries used independent aliases. Only an actual
				// intervening unversioned alias permits checking the exact known
				// per-name transition instead of the new shared revision stream.
				if !unversionedAliasIntervened[call.Function.Name] && !historicalLegacySeen {
					continue
				}
				if ownKnown {
					if !ownValid || !reflect.DeepEqual(ownExpected, recorded) {
						continue
					}
				}
			}
			if candidate.RestorePlanState(recorded) != nil {
				continue
			}
			canonicalByTool[call.Function.Name] = candidate.PlanState()
			unversionedAliasIntervened = map[string]bool{}
		} else {
			if unversionedLegacy {
				historicalLegacySeen = true
			}
			canonicalByTool[call.Function.Name] = candidate.PlanState()
			otherName := TaskPlanToolName
			if call.Function.Name == TaskPlanToolName {
				otherName = "todo_write"
			}
			unversionedAliasIntervened[otherName] = true
		}
		stateTool = candidate
		outputDigest := sha256.Sum256([]byte(message.Content))
		snapshot.State = stateTool.PlanState()
		snapshot.Found = true
		snapshot.RestoredCalls++
		snapshot.CallID = call.ID
		snapshot.ToolName = call.Function.Name
		snapshot.OutputSHA256 = hex.EncodeToString(outputDigest[:])
		snapshot.SuccessfulCallIDs = append(snapshot.SuccessfulCallIDs, call.ID)
	}
	return snapshot
}

// RestorePlanState loads validated, recorded model state without creating a
// model action or changing its revision. Only persistence/runtime code uses it.
func (t *TodoWrite) RestorePlanState(state TaskPlanState) error {
	if state.Revision < 0 || (state.Revision == 0 && (len(state.Items) > 0 || strings.TrimSpace(state.Goal) != "")) {
		return fmt.Errorf("invalid recorded task plan revision")
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > taskPlanHistoryPayloadLimit {
		return fmt.Errorf("recorded task plan exceeds bounded state")
	}
	validator := NewTaskPlan()
	if err := validator.RestoreArguments(map[string]any{"goal": state.Goal, "items": state.Items}); err != nil {
		return err
	}
	validated := validator.PlanState()
	if validated.Goal != state.Goal || (len(state.Items) > 0 && !reflect.DeepEqual(validated.Items, state.Items)) {
		return fmt.Errorf("recorded task plan is not canonical")
	}
	owner := t.stateOwner()
	owner.mu.Lock()
	owner.goal, owner.todos, owner.revision = validated.Goal, cloneTodoItems(validated.Items), state.Revision
	owner.mu.Unlock()
	return nil
}

// CloneTaskPlanState keeps typed lifecycle snapshots independent of tool state.
func CloneTaskPlanState(state *TaskPlanState) *TaskPlanState {
	if state == nil {
		return nil
	}
	cloned := *state
	cloned.Items = cloneTodoItems(state.Items)
	return &cloned
}
