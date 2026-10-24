package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

const TaskPlanToolName = "task_plan"

const taskPlanUnchangedHeader = "Task plan unchanged. No new revision was created. Do not submit the same plan again. Reuse the committed state; update only after material work, results, or requirement changes."

// TodoItem remains source-compatible with the legacy todo_write UI while also
// carrying the optional structure needed by a model-owned task plan.
type TodoItem struct {
	ID              string   `json:"id"`
	Content         string   `json:"content"`
	Status          string   `json:"status"` // pending | in_progress | completed | cancelled
	DependsOn       []string `json:"depends_on,omitempty"`
	SuccessCriteria string   `json:"success_criteria,omitempty"`
	Action          string   `json:"action,omitempty"`
	RequirementIDs  []string `json:"requirement_ids,omitempty"`
	Artifacts       []string `json:"artifacts,omitempty"`
	EvidenceIDs     []string `json:"evidence_ids,omitempty"`
}

type TaskPlanState struct {
	Goal     string     `json:"goal,omitempty"`
	Items    []TodoItem `json:"items"`
	Revision int        `json:"revision"`
}

var validTodoStatus = map[string]bool{
	"pending": true, "in_progress": true, "completed": true, "cancelled": true,
}

var validTaskPlanAction = map[string]bool{
	"": true, "inspect": true, "research": true, "create": true, "edit": true,
	"execute": true, "verify": true, "clarify": true, "deliver": true,
}

// TodoWrite backs both the legacy todo_write tool and the model-owned task_plan
// schema. The model is the only component that creates or updates the plan;
// runtime code may restore and re-present the recorded state but never edits it.
type TodoWrite struct {
	mu       sync.Mutex
	name     string
	shared   *TodoWrite
	goal     string
	todos    []TodoItem
	revision int
}

func NewTaskPlan() *TodoWrite { return &TodoWrite{name: TaskPlanToolName} }

// NewLegacyTodoAlias exposes the historical schema while sharing one
// model-owned plan and revision with task_plan in the native product registry.
func NewLegacyTodoAlias(plan *TodoWrite) *TodoWrite {
	if plan == nil {
		return &TodoWrite{}
	}
	return &TodoWrite{shared: plan.stateOwner()}
}

func (t *TodoWrite) stateOwner() *TodoWrite {
	if t.shared != nil {
		return t.shared
	}
	return t
}

func (t *TodoWrite) Name() string {
	if strings.TrimSpace(t.name) == "" {
		return "todo_write"
	}
	return t.name
}

func (t *TodoWrite) Description() string {
	if t.Name() == TaskPlanToolName {
		return "Create, revise, or clear your own named checklist when tracking outcomes, dependencies, or resumed work is useful; honor requests for a persistent task checklist. A nonempty checklist requires your chosen nonempty task name in goal unless a saved name already exists. Update it after material work or results, keeping unfinished work explicit. Completion is model bookkeeping, not verification. Meadow never calls this tool automatically and imposes no step-count threshold."
	}
	return "Replace the session todo list."
}

func (t *TodoWrite) Kind() Kind { return KindReadOnly }

func (t *TodoWrite) Schema() map[string]any {
	if t.Name() == TaskPlanToolName {
		itemSchema := obj(map[string]any{
			"id":      str("Stable unique identifier used by dependencies and later updates."),
			"content": str("Concrete outcome or work item; choose the granularity yourself."),
			"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"},
				"description": "Track actual progress. Mark completed only when the item's requested outcome has supporting results; unfinished or untested work remains pending/in_progress. Cancelling is a deliberate scope change, not a test pass."},
			"depends_on":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional IDs that must be resolved first."},
			"success_criteria": str("Optional observable condition for this outcome. Preserve it during status updates. Correct mistaken expectations from requirements and observed input instead of dropping relevant assertions. Test/integration completion needs relevant assertions against the latest artifact, rechecked after affected edits."),
			"action": map[string]any{
				"type": "string", "enum": []string{"inspect", "research", "create", "edit", "execute", "verify", "clarify", "deliver"},
				"description": "Optional generic action class. This is model-authored state; Meadow does not execute it automatically.",
			},
			"requirement_ids": map[string]any{
				"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"},
				"description": "Optional stable requirement IDs served by this item.",
			},
			"artifacts": map[string]any{
				"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"},
				"description": "Optional artifact paths or source identifiers this item may observe or change.",
			},
			"evidence_ids": map[string]any{
				"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"},
				"description": "Optional exact tool call IDs whose recorded observations support this item. Linking evidence does not verify the item or expand the tool's tested scope.",
			},
		}, "id", "content", "status")
		return obj(map[string]any{
			"goal": str("Your chosen concise task name. Required and nonempty when creating a nonempty checklist without a saved name. Omit on later updates to retain its name. A nonempty checklist cannot explicitly clear its name; use items=[] with goal=\"\" when intentionally clearing both."),
			"items": map[string]any{
				"type":        "array",
				"description": "Replace the complete current plan, with stable IDs and meaningful outcomes. Optional item fields omitted from this replacement are removed; preserve relevant criteria and requirement links during status updates. Revise after material work/results or requirement changes, not every read. Retain unfinished work; do not clear it to hide missing outcomes. Use an empty array only when intentionally clearing the checklist. You choose granularity and dependencies.",
				"items":       itemSchema,
			},
		}, "items")
	}
	return obj(map[string]any{
		"todos": map[string]any{
			"type":        "array",
			"description": "The full todo list.",
			"items": obj(map[string]any{
				"id":      str("Unique identifier."),
				"content": str("Task description."),
				"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
			}, "id", "content", "status"),
		},
	}, "todos")
}

func (t *TodoWrite) Current() []TodoItem {
	return t.PlanState().Items
}

func (t *TodoWrite) PlanState() TaskPlanState {
	owner := t.stateOwner()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return TaskPlanState{
		Goal: strings.TrimSpace(owner.goal), Items: cloneTodoItems(owner.todos), Revision: owner.revision,
	}
}

// ResetPlan clears reconstructed state before replaying persisted tool calls.
// It is not a model-visible action and does not increment the revision.
func (t *TodoWrite) ResetPlan() {
	owner := t.stateOwner()
	owner.mu.Lock()
	owner.goal = ""
	owner.todos = nil
	owner.revision = 0
	owner.mu.Unlock()
}

// RestoreArguments replays one persisted task_plan/todo_write call. It applies
// the same structural validation as a live call but produces no tool result.
// Historical unnamed plans remain recoverable; live naming requirements apply
// only to Execute, never to recorded history.
func (t *TodoWrite) RestoreArguments(args map[string]any) error {
	_, _, err := t.apply(args)
	return err
}

func (t *TodoWrite) HistoryToolNames() []string {
	return []string{TaskPlanToolName, "todo_write"}
}

// Reminder returns compact state, not instructions or a next action. The
// kernel may re-present it after history compaction so the model can continue
// managing the plan it created.
func (t *TodoWrite) Reminder() string {
	state := t.PlanState()
	active := make([]TodoItem, 0, len(state.Items))
	completed := make([]string, 0, len(state.Items))
	cancelled := make([]string, 0, len(state.Items))
	for _, item := range state.Items {
		switch item.Status {
		case "pending", "in_progress":
			active = append(active, item)
		case "completed":
			completed = append(completed, item.ID)
		case "cancelled":
			cancelled = append(cancelled, item.ID)
		}
	}
	if len(active) == 0 {
		return ""
	}
	compact := struct {
		Goal         string     `json:"goal,omitempty"`
		ActiveItems  []TodoItem `json:"active_items"`
		CompletedIDs []string   `json:"completed_ids,omitempty"`
		CancelledIDs []string   `json:"cancelled_ids,omitempty"`
		Revision     int        `json:"revision"`
	}{
		Goal: state.Goal, ActiveItems: active, CompletedIDs: completed,
		CancelledIDs: cancelled, Revision: state.Revision,
	}
	encoded, _ := json.Marshal(compact)
	return "Model-owned task plan state (Meadow stores this state but does not create, prioritize, or execute it):\n" + string(encoded)
}

func (t *TodoWrite) Execute(_ context.Context, args map[string]any) Result {
	state, changed, err := t.applyPlan(args, true)
	if err != nil {
		return errResult(err)
	}
	if !changed {
		listing, _ := json.Marshal(state)
		return Result{
			LLMContent:    taskPlanUnchangedHeader + "\n" + string(listing),
			Display:       "plan unchanged",
			NoChange:      true,
			TaskPlanState: &state,
		}
	}
	if len(state.Items) == 0 {
		listing, _ := json.Marshal(state)
		if t.Name() == TaskPlanToolName {
			return Result{LLMContent: "Task plan has been cleared.\n" + string(listing), Display: "plan cleared", TaskPlanState: &state}
		}
		return Result{LLMContent: "Todo list has been cleared.\n" + string(listing), Display: "todos cleared", TaskPlanState: &state}
	}
	listing, _ := json.Marshal(state)
	if t.Name() == TaskPlanToolName {
		return Result{
			LLMContent:    "Task plan state saved. You remain responsible for deciding and updating the next action.\n" + string(listing),
			Display:       fmt.Sprintf("%d plan items", len(state.Items)),
			TaskPlanState: &state,
		}
	}
	canonical := string(listing)
	listing, _ = json.Marshal(state.Items)
	return Result{
		LLMContent: "Todos have been modified successfully.\n\n<system-reminder>\nYour todo list has changed. DO NOT mention this explicitly to the user. Here are the latest contents:\n" +
			string(listing) + "\n</system-reminder>\nTask plan recorded state:\n" + canonical,
		Display:       fmt.Sprintf("%d todos", len(state.Items)),
		TaskPlanState: &state,
	}
}

func (t *TodoWrite) apply(args map[string]any) (TaskPlanState, bool, error) {
	return t.applyPlan(args, false)
}

func (t *TodoWrite) applyPlan(args map[string]any, live bool) (TaskPlanState, bool, error) {
	key := "todos"
	if t.Name() == TaskPlanToolName {
		key = "items"
	}
	raw, ok := args[key]
	if !ok {
		// Accept the other historical key during session migration, but keep the
		// model-facing schema singular.
		fallback := map[string]string{"items": "todos", "todos": "items"}[key]
		raw, ok = args[fallback]
	}
	if !ok {
		return TaskPlanState{}, false, fmt.Errorf("%s parameter is required", key)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return TaskPlanState{}, false, err
	}
	var todos []TodoItem
	if err := json.Unmarshal(encoded, &todos); err != nil {
		return TaskPlanState{}, false, fmt.Errorf("%s must be an array of plan items: %w", key, err)
	}
	if len(todos) > 64 {
		return TaskPlanState{}, false, fmt.Errorf("task plan has %d items; maximum is 64", len(todos))
	}
	goal, _ := args["goal"].(string)
	_, goalProvided := args["goal"]
	goal = strings.TrimSpace(goal)
	if len(goal) > 4096 {
		return TaskPlanState{}, false, fmt.Errorf("task plan goal exceeds 4096 bytes")
	}
	for index := range todos {
		todos[index].ID = strings.TrimSpace(todos[index].ID)
		todos[index].Content = strings.TrimSpace(todos[index].Content)
		todos[index].Status = strings.TrimSpace(todos[index].Status)
		todos[index].SuccessCriteria = strings.TrimSpace(todos[index].SuccessCriteria)
		todos[index].Action = strings.TrimSpace(todos[index].Action)
		for dependency := range todos[index].DependsOn {
			todos[index].DependsOn[dependency] = strings.TrimSpace(todos[index].DependsOn[dependency])
		}
		for requirement := range todos[index].RequirementIDs {
			todos[index].RequirementIDs[requirement] = strings.TrimSpace(todos[index].RequirementIDs[requirement])
		}
		for artifact := range todos[index].Artifacts {
			todos[index].Artifacts[artifact] = strings.TrimSpace(todos[index].Artifacts[artifact])
		}
		for evidence := range todos[index].EvidenceIDs {
			todos[index].EvidenceIDs[evidence] = strings.TrimSpace(todos[index].EvidenceIDs[evidence])
		}
	}
	if err := validateTaskPlan(todos, t.Name() == TaskPlanToolName); err != nil {
		return TaskPlanState{}, false, err
	}
	owner := t.stateOwner()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	// Resolve the omitted name against the same state which will be committed,
	// so a concurrent shared alias cannot overwrite a newer saved name with a
	// stale pre-validation snapshot.
	if !goalProvided {
		goal = owner.goal
	}
	if live && t.Name() == TaskPlanToolName && len(todos) > 0 && goal == "" {
		return TaskPlanState{}, false, fmt.Errorf("task_plan requires a nonempty goal (your chosen task name) for a nonempty checklist; supply goal before creating or updating these items")
	}
	bounded, err := json.Marshal(TaskPlanState{Goal: goal, Items: todos})
	if err != nil || len(bounded) > 64<<10 {
		return TaskPlanState{}, false, fmt.Errorf("task plan exceeds 65536 bytes")
	}

	if owner.goal == goal && reflect.DeepEqual(owner.todos, todos) {
		state := TaskPlanState{
			Goal: owner.goal, Items: cloneTodoItems(owner.todos), Revision: owner.revision,
		}
		return state, false, nil
	}
	owner.goal = goal
	owner.todos = cloneTodoItems(todos)
	owner.revision++
	state := TaskPlanState{Goal: owner.goal, Items: cloneTodoItems(owner.todos), Revision: owner.revision}
	return state, true, nil
}

func validateTaskPlan(items []TodoItem, allowCancelled bool) error {
	byID := make(map[string]TodoItem, len(items))
	for index, item := range items {
		if len(item.ID) > 256 || len(item.Content) > 4096 || len(item.SuccessCriteria) > 4096 {
			return fmt.Errorf("plan item %d exceeds the id or text size limit", index)
		}
		if item.ID == "" || item.Content == "" {
			return fmt.Errorf("plan item %d needs non-empty id and content", index)
		}
		if !validTodoStatus[item.Status] {
			return fmt.Errorf("plan item %q has invalid status %q", item.ID, item.Status)
		}
		if !validTaskPlanAction[item.Action] {
			return fmt.Errorf("plan item %q has invalid action %q", item.ID, item.Action)
		}
		if err := validateUniquePlanValues(item.ID, "requirement_ids", item.RequirementIDs, 16); err != nil {
			return err
		}
		if err := validateUniquePlanValues(item.ID, "artifacts", item.Artifacts, 16); err != nil {
			return err
		}
		if err := validateUniquePlanValues(item.ID, "evidence_ids", item.EvidenceIDs, 16); err != nil {
			return err
		}
		if item.Status == "cancelled" && !allowCancelled {
			return fmt.Errorf("plan item %q has invalid status %q", item.ID, item.Status)
		}
		if _, exists := byID[item.ID]; exists {
			return fmt.Errorf("duplicate plan item id %q", item.ID)
		}
		byID[item.ID] = item
	}
	for _, item := range items {
		seenDependency := map[string]bool{}
		for _, dependency := range item.DependsOn {
			if dependency == "" {
				return fmt.Errorf("plan item %q has an empty dependency", item.ID)
			}
			if dependency == item.ID {
				return fmt.Errorf("plan item %q cannot depend on itself", item.ID)
			}
			if _, exists := byID[dependency]; !exists {
				return fmt.Errorf("plan item %q depends on unknown item %q", item.ID, dependency)
			}
			if seenDependency[dependency] {
				return fmt.Errorf("plan item %q repeats dependency %q", item.ID, dependency)
			}
			seenDependency[dependency] = true
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("task plan contains a dependency cycle at %q", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func validateUniquePlanValues(itemID, field string, values []string, limit int) error {
	if len(values) > limit {
		return fmt.Errorf("plan item %q has %d %s; maximum is %d", itemID, len(values), field, limit)
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if len(value) > 2048 {
			return fmt.Errorf("plan item %q has an oversized %s value", itemID, field)
		}
		if value == "" {
			return fmt.Errorf("plan item %q has an empty %s value", itemID, field)
		}
		if seen[value] {
			return fmt.Errorf("plan item %q repeats %s value %q", itemID, field, value)
		}
		seen[value] = true
	}
	return nil
}

func cloneTodoItems(items []TodoItem) []TodoItem {
	cloned := make([]TodoItem, len(items))
	for index, item := range items {
		cloned[index] = item
		cloned[index].DependsOn = append([]string(nil), item.DependsOn...)
		cloned[index].RequirementIDs = append([]string(nil), item.RequirementIDs...)
		cloned[index].Artifacts = append([]string(nil), item.Artifacts...)
		cloned[index].EvidenceIDs = append([]string(nil), item.EvidenceIDs...)
	}
	return cloned
}
