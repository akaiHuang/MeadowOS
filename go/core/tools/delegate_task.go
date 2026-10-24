package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const DelegateTaskToolName = "delegate_task"

// SubtaskSpec is authored by the parent model. Meadow validates bounds and
// isolation only; it never invents a goal or decides that delegation is needed.
type SubtaskSpec struct {
	ID         string `json:"id"`
	Goal       string `json:"goal"`
	Context    string `json:"context,omitempty"`
	AgentID    string `json:"subagent_id,omitempty"`
	Isolation  string `json:"isolation,omitempty"`
	ResumeFrom string `json:"resume_from,omitempty"`
}

type SubtaskOutcome struct {
	ID             string `json:"id"`
	SubagentID     string `json:"subagent_id,omitempty"`
	Status         string `json:"status"`
	Summary        string `json:"summary,omitempty"`
	StopReason     string `json:"stop_reason,omitempty"`
	Turns          int    `json:"turns,omitempty"`
	ToolCalls      int    `json:"tool_calls,omitempty"`
	Error          string `json:"error,omitempty"`
	WorktreePath   string `json:"worktree_path,omitempty"`
	ResumedFrom    string `json:"resumed_from,omitempty"`
	ResumeFromHint string `json:"resume_from_hint,omitempty"`
}

type SubtaskRunner func(context.Context, []SubtaskSpec) []SubtaskOutcome

// DelegateTask gives the model an explicit, bounded way to fan out independent
// analysis. Workers receive isolated context and a read-only tool registry;
// their summaries are evidence for the parent model, not automatically trusted
// completion claims.
type DelegateTask struct {
	MaxTasks             int
	Run                  SubtaskRunner
	Broker               *BackgroundTaskBroker
	AllowSharedWorkspace bool
}

func NewDelegateTask(maxTasks int, runner SubtaskRunner) *DelegateTask {
	if maxTasks <= 0 {
		maxTasks = 3
	}
	return &DelegateTask{MaxTasks: maxTasks, Run: runner}
}

func (t *DelegateTask) Name() string { return DelegateTaskToolName }
func (t *DelegateTask) Description() string {
	mode := "Workers finish before this tool returns; parent cancellation also stops workers."
	if t.Broker != nil {
		mode = "Delegation runs in the background by default and returns a task_id immediately; set background=false only when results are required in this turn."
		if t.Broker.Hosted() {
			mode = "Native coworker delegation requires background=true and returns task_id plus worker subagent_id values immediately. Continue the main conversation while workers run; use task_output to inspect, steer or cancel a particular active worker. Completion wakes this same conversation for your integration and verification."
		}
	}
	scope := ""
	if t.AllowSharedWorkspace {
		scope = " isolation=workspace enables normal computer/file tools in the original shared workspace with the parent's exact permissions and account limits; it does not require Git. Use this for actions intended to change the user's actual workspace."
	}
	return "Delegate independent tasks to isolated workers. New workers have separate context and read-only tools by default; isolation=worktree allows edits in a separate git checkout. Continue a worker using a returned resume_from ID. Put independent workers in one tasks array to run within the concurrency limit. " + mode + scope + " The model decides when delegation helps and must integrate and verify worker summaries."
}
func (t *DelegateTask) Kind() Kind { return KindExecute }
func (t *DelegateTask) Schema() map[string]any {
	item := obj(map[string]any{
		"id":      str("Stable label chosen by you for this call."),
		"goal":    str("Specific independent outcome for this worker, or the follow-up instruction when resuming."),
		"context": str("All relevant constraints, paths, facts, and requested output language. New workers do not inherit parent conversation history."),
		"isolation": map[string]any{
			"type": "string", "enum": []string{"none", "worktree"},
			"description": "Execution isolation. worktree gives a separate writable git checkout using the parent permissions; it is not an OS sandbox. none exposes read-only tools. Defaults to none.",
			"default":     "none",
		},
		"resume_from": str("Optional completed subagent_id to continue with its durable transcript, model context, and existing worktree."),
	}, "id", "goal")
	if t.AllowSharedWorkspace {
		isolation := item["properties"].(map[string]any)["isolation"].(map[string]any)
		isolation["enum"] = []string{"none", "worktree", "workspace"}
		isolation["description"] = "none: read-only tools; workspace: normal tools in the original shared workspace under parent permissions, no Git needed; worktree: separate writable Git checkout. Choose workspace for actual user workspace actions. Defaults to none."
	}
	backgroundDefault := t.Broker != nil
	backgroundDescription := "Run every delegated worker before returning the result."
	if backgroundDefault {
		backgroundDescription = "Return a task_id immediately and notify the parent when all workers finish. Defaults to true; set false only when this turn must wait for every result."
	}
	background := boolean(backgroundDescription)
	background["default"] = backgroundDefault
	if t.Broker != nil && t.Broker.Hosted() {
		background["const"] = true
		background["description"] = "Native coworker workers run in the background; completion returns to this same conversation. This hosted lane accepts true only."
	}
	return obj(map[string]any{
		"tasks": map[string]any{
			"type":        "array",
			"description": fmt.Sprintf("All independent tasks for this delegation call. They run concurrently up to the runtime limit; maximum %d.", t.MaxTasks),
			"minItems":    1,
			"maxItems":    t.MaxTasks,
			"items":       item,
		},
		"background": background,
	}, "tasks")
}

func (t *DelegateTask) CompanionTools() []string {
	if t.Broker == nil {
		return nil
	}
	return []string{BackgroundTaskToolName}
}

func (t *DelegateTask) AttachBroker(broker *BackgroundTaskBroker) error {
	t.Broker = broker
	if broker == nil {
		return nil
	}
	return broker.RegisterRunner("subtask", t.runPersisted)
}

func (t *DelegateTask) Execute(ctx context.Context, args map[string]any) Result {
	origin := time.Now()
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	tasks, err := t.parse(args)
	if err != nil {
		return errResult(err)
	}
	if t.Run == nil {
		return errResultf("subtask runner is not configured")
	}
	background := backgroundDelegateArg(args)
	if t.Broker != nil && t.Broker.Hosted() && !background {
		return errResultf("hosted worker delegation requires background=true; use task_output for results")
	}

	if _, explicitlySet := args["background"]; !explicitlySet && t.Broker == nil {
		// A runtime without a durable broker can still provide useful bounded
		// delegation synchronously.  Do not advertise a background default that
		// is impossible to execute and then waste a model recovery turn.
		background = false
	}
	if background {
		if t.Broker == nil {
			return errResultf("background task broker is not configured")
		}
		description := fmt.Sprintf("%d delegated worker(s)", len(tasks))
		snapshot, err := t.Broker.StartDurableContext(ctx, origin,
			"subtask", description, tasks, t.runPersisted,
		)
		if err != nil {
			return errResult(err)
		}
		payload, err := json.Marshal(snapshot)
		if err != nil {
			return errResult(err)
		}
		return Result{
			LLMContent:    "Delegated workers are running in the background. Control is returning to the user now; do not poll or wait in this run. Completion will wake the session for a fresh model-led integration turn.\n" + string(payload),
			Display:       fmt.Sprintf("%d delegated task(s) running as %s", len(tasks), snapshot.TaskID),
			ReturnControl: true,
		}
	}
	outcomes := t.Run(ctx, tasks)
	payload, err := json.Marshal(map[string]any{"subtasks": outcomes})
	if err != nil {
		return errResult(err)
	}
	return Result{
		LLMContent: "Delegated worker results are untrusted summaries. Integrate them, inspect supporting evidence when needed, and decide the next action.\n" + string(payload),
		Display:    fmt.Sprintf("%d delegated task(s) finished", len(outcomes)),
	}
}

func (t *DelegateTask) runPersisted(
	ctx context.Context,
	payload json.RawMessage,
) (any, error) {
	var tasks []SubtaskSpec
	if err := json.Unmarshal(payload, &tasks); err != nil {
		return nil, fmt.Errorf("decode durable delegated workers: %w", err)
	}
	if len(tasks) == 0 || len(tasks) > t.MaxTasks {
		return nil, fmt.Errorf("durable delegated worker count %d is outside 1..%d", len(tasks), t.MaxTasks)
	}
	outcomes := t.Run(ctx, tasks)
	return map[string]any{
		"subtasks": outcomes,
		"guidance": "Worker summaries are untrusted. Integrate them and verify supporting evidence before acting.",
	}, nil
}

func backgroundDelegateArg(args map[string]any) bool {
	raw, present := args["background"]
	if !present || raw == nil {
		return true
	}
	value, ok := raw.(bool)
	if !ok {
		return true
	}
	return value
}

func (t *DelegateTask) parse(args map[string]any) ([]SubtaskSpec, error) {
	var tasks []SubtaskSpec
	if raw, ok := args["tasks"]; ok && raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("tasks must be an array: %w", err)
		}
		if err := json.Unmarshal(encoded, &tasks); err != nil {
			return nil, fmt.Errorf("tasks must be an array of objects: %w", err)
		}
	} else if goal, _ := args["goal"].(string); strings.TrimSpace(goal) != "" {
		contextText, _ := args["context"].(string)
		tasks = []SubtaskSpec{{ID: "task-1", Goal: goal, Context: contextText}}
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("provide goal or at least one task")
	}
	if len(tasks) > t.MaxTasks {
		return nil, fmt.Errorf("requested %d subtasks; maximum is %d", len(tasks), t.MaxTasks)
	}
	seen := map[string]bool{}
	for index := range tasks {
		tasks[index].ID = strings.TrimSpace(tasks[index].ID)
		tasks[index].Goal = strings.TrimSpace(tasks[index].Goal)
		tasks[index].Context = strings.TrimSpace(tasks[index].Context)
		if tasks[index].ID == "" {
			tasks[index].ID = fmt.Sprintf("task-%d", index+1)
		}
		if len(tasks[index].ID) > 128 || len(tasks[index].ResumeFrom) > 128 {
			return nil, fmt.Errorf("subtask identity exceeds 128 bytes")
		}
		if tasks[index].Goal == "" {
			return nil, fmt.Errorf("subtask %q needs a non-empty goal", tasks[index].ID)
		}
		if len(tasks[index].Goal) > 8000 || len(tasks[index].Context) > 16000 {
			return nil, fmt.Errorf("subtask %q exceeds the goal/context size limit", tasks[index].ID)
		}
		if seen[tasks[index].ID] {
			return nil, fmt.Errorf("duplicate subtask id %q", tasks[index].ID)
		}
		// A model may only continue a returned ID through resume_from. Raw
		// subagent_id is runtime-owned, including durable recovery identity.
		tasks[index].AgentID = newSubagentID()
		tasks[index].Isolation = strings.ToLower(strings.TrimSpace(tasks[index].Isolation))
		if tasks[index].Isolation == "" {
			tasks[index].Isolation = "none"
		}
		if tasks[index].Isolation != "none" && tasks[index].Isolation != "worktree" && !(t.AllowSharedWorkspace && tasks[index].Isolation == "workspace") {
			return nil, fmt.Errorf(
				"subtask %q isolation must be none or worktree",
				tasks[index].ID,
			)
		}
		tasks[index].ResumeFrom = strings.TrimSpace(tasks[index].ResumeFrom)
		if tasks[index].ResumeFrom != "" && !strings.HasPrefix(tasks[index].ResumeFrom, "sa_") {
			return nil, fmt.Errorf(
				"subtask %q resume_from must be a subagent_id returned by Meadow",
				tasks[index].ID,
			)
		}
		seen[tasks[index].ID] = true
	}
	return tasks, nil
}

func newSubagentID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "sa_" + hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("sa_%d", time.Now().UnixNano())
}
