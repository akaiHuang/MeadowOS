package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/taskruntime"
)

const (
	SchedulerCreateToolName  = "scheduler_create"
	SchedulerListToolName    = "scheduler_list"
	SchedulerControlToolName = "scheduler_control"
	SchedulerDeleteToolName  = "scheduler_delete"
)

// RegisterSchedulerTools adds Meadow's durable loop controls to a registry.
// Callers choose which model/runtime sees them; DefaultRegistry stays unchanged
// so enabling long tasks cannot silently alter the 12B/26B tool surface.
func RegisterSchedulerTools(registry *Registry, scheduler *taskruntime.Scheduler, ownerSession string) {
	if registry == nil || scheduler == nil {
		return
	}
	registry.Register(&SchedulerCreate{Scheduler: scheduler, OwnerSession: ownerSession})
	registry.Register(&SchedulerList{Scheduler: scheduler})
	registry.Register(&SchedulerControl{Scheduler: scheduler})
	registry.Register(&SchedulerDelete{Scheduler: scheduler})
}

type SchedulerCreate struct {
	Scheduler    *taskruntime.Scheduler
	OwnerSession string
}

func (t *SchedulerCreate) Name() string { return SchedulerCreateToolName }
func (t *SchedulerCreate) Description() string {
	return "Create a recurring prompt, or update one by task_id. Each occurrence is a fresh agent turn; tasks never overlap and expire after seven days."
}
func (t *SchedulerCreate) Schema() map[string]any {
	return obj(map[string]any{
		"task_id":          str("Existing task ID to update. Omit to create."),
		"interval":         str("Cadence: 60s, 5m, 2h, or 1d. Required to create."),
		"prompt":           str("Objective to execute on each occurrence. Required to create."),
		"fire_immediately": boolean("Run once immediately after creation. Default false."),
		"durable":          boolean("Persist across CLI restarts. Default false."),
	})
}
func (t *SchedulerCreate) Kind() Kind { return KindEdit }
func (t *SchedulerCreate) CompanionTools() []string {
	return []string{SchedulerListToolName, SchedulerControlToolName, SchedulerDeleteToolName}
}
func (t *SchedulerCreate) Execute(_ context.Context, args map[string]any) Result {
	if t == nil || t.Scheduler == nil {
		return errResult(fmt.Errorf("task scheduler is unavailable"))
	}
	id := strings.TrimSpace(strArg(args, "task_id"))
	if id != "" {
		var prompt *string
		if raw, ok := args["prompt"]; ok {
			value, ok := raw.(string)
			if !ok {
				return errResult(fmt.Errorf("prompt must be a string"))
			}
			prompt = &value
		}
		var interval *string
		if raw, ok := args["interval"]; ok {
			value, ok := raw.(string)
			if !ok {
				return errResult(fmt.Errorf("interval must be a string"))
			}
			interval = &value
		}
		task, err := t.Scheduler.UpdateLoop(taskruntime.UpdateLoopRequest{
			ID: id, Prompt: prompt, Interval: interval,
		})
		if err != nil {
			return errResult(err)
		}
		return schedulerJSON(map[string]any{
			"id": task.ID, "interval": task.Interval, "updated": true,
			"next_run_at": task.NextRunAt,
		})
	}

	task, err := t.Scheduler.CreateLoop(taskruntime.CreateLoopRequest{
		Prompt:          strings.TrimSpace(strArg(args, "prompt")),
		Interval:        strings.TrimSpace(strArg(args, "interval")),
		FireImmediately: boolArg(args, "fire_immediately"),
		Durable:         boolArg(args, "durable"),
		OwnerSession:    t.OwnerSession,
	})
	if err != nil {
		return errResult(err)
	}
	return schedulerJSON(map[string]any{
		"id": task.ID, "interval": task.Interval, "updated": false,
		"durable": task.Durable, "next_run_at": task.NextRunAt,
		"expires_at": task.ExpiresAt,
	})
}

type SchedulerList struct {
	Scheduler *taskruntime.Scheduler
}

func (t *SchedulerList) Name() string { return SchedulerListToolName }
func (t *SchedulerList) Description() string {
	return "List recurring tasks with current state, next occurrence, and latest result."
}
func (t *SchedulerList) Schema() map[string]any { return obj(map[string]any{}) }
func (t *SchedulerList) Kind() Kind             { return KindReadOnly }
func (t *SchedulerList) ParallelSafe(map[string]any) bool {
	return true
}
func (t *SchedulerList) Execute(_ context.Context, _ map[string]any) Result {
	if t == nil || t.Scheduler == nil {
		return errResult(fmt.Errorf("task scheduler is unavailable"))
	}
	tasks := t.Scheduler.List()
	taskruntime.SortByNextRun(tasks)
	return schedulerJSON(map[string]any{"tasks": tasks})
}

type SchedulerControl struct {
	Scheduler *taskruntime.Scheduler
}

func (t *SchedulerControl) Name() string { return SchedulerControlToolName }
func (t *SchedulerControl) Description() string {
	return "Pause or resume a recurring task by ID. Pausing does not kill an occurrence already running."
}
func (t *SchedulerControl) Schema() map[string]any {
	return obj(map[string]any{
		"id": str("Scheduled task ID."),
		"action": map[string]any{
			"type": "string", "enum": []string{"pause", "resume"},
			"description": "Lifecycle action.",
		},
	}, "id", "action")
}
func (t *SchedulerControl) Kind() Kind { return KindEdit }
func (t *SchedulerControl) Execute(_ context.Context, args map[string]any) Result {
	if t == nil || t.Scheduler == nil {
		return errResult(fmt.Errorf("task scheduler is unavailable"))
	}
	id := strings.TrimSpace(strArg(args, "id"))
	var (
		task taskruntime.Task
		err  error
	)
	switch strings.ToLower(strings.TrimSpace(strArg(args, "action"))) {
	case "pause":
		task, err = t.Scheduler.Pause(id)
	case "resume":
		task, err = t.Scheduler.Resume(id)
	default:
		return errResult(fmt.Errorf("action must be pause or resume"))
	}
	if err != nil {
		return errResult(err)
	}
	return schedulerJSON(map[string]any{
		"id": task.ID, "status": task.Status, "next_run_at": task.NextRunAt,
	})
}

type SchedulerDelete struct {
	Scheduler *taskruntime.Scheduler
}

func (t *SchedulerDelete) Name() string { return SchedulerDeleteToolName }
func (t *SchedulerDelete) Description() string {
	return "Cancel and remove a recurring task by ID."
}
func (t *SchedulerDelete) Schema() map[string]any {
	return obj(map[string]any{"id": str("Scheduled task ID.")}, "id")
}
func (t *SchedulerDelete) Kind() Kind { return KindEdit }
func (t *SchedulerDelete) Execute(_ context.Context, args map[string]any) Result {
	if t == nil || t.Scheduler == nil {
		return errResult(fmt.Errorf("task scheduler is unavailable"))
	}
	task, found, err := t.Scheduler.Delete(strings.TrimSpace(strArg(args, "id")))
	if err != nil {
		return errResult(err)
	}
	return schedulerJSON(map[string]any{"id": task.ID, "removed": found})
}

func schedulerJSON(value any) Result {
	payload, err := json.Marshal(value)
	if err != nil {
		return errResult(err)
	}
	return Result{LLMContent: string(payload), Display: string(payload)}
}
