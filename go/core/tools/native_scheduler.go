package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/taskruntime"
)

// CancelScheduledWake cancels delivery, not an already running model turn.
// The returned status must be the durable wake service's actual receipt.
type CancelScheduledWake func(context.Context, taskruntime.Task) (string, error)

func RegisterNativeSchedulerTools(registry *Registry, scheduler *taskruntime.Scheduler, owner taskruntime.ScheduleOwner, cancel CancelScheduledWake) {
	if registry == nil || scheduler == nil || !owner.Valid() {
		return
	}
	for _, name := range []string{SchedulerCreateToolName, SchedulerListToolName, SchedulerControlToolName, SchedulerDeleteToolName} {
		registry.Register(&nativeSchedulerTool{name: name, scheduler: scheduler, owner: owner, cancel: cancel})
	}
}

type nativeSchedulerTool struct {
	name      string
	scheduler *taskruntime.Scheduler
	owner     taskruntime.ScheduleOwner
	cancel    CancelScheduledWake
}

func (t *nativeSchedulerTool) Name() string { return t.name }
func (t *nativeSchedulerTool) Kind() Kind {
	if t.name == SchedulerListToolName {
		return KindReadOnly
	}
	return KindEdit
}
func (t *nativeSchedulerTool) CompanionTools() []string {
	return []string{SchedulerCreateToolName, SchedulerListToolName, SchedulerControlToolName, SchedulerDeleteToolName}
}
func (t *nativeSchedulerTool) Description() string {
	switch t.name {
	case SchedulerCreateToolName:
		return "Schedule a useful delayed follow-up in this same chat, or update one with task_id. At the deadline the normal agent receives the objective and original conversation; busy chats queue it. Prefer one-shot after_seconds/run_at for autonomous follow-ups. Use recurring interval only for an authorized recurring need, with explicit max_runs or expires_at. Do not schedule routine conversation or duplicate a background job completion. Durable across restarts; normal permissions and account admission still apply."
	case SchedulerListToolName:
		return "List this chat's persistent schedules, deadlines, active occurrences and delivery failures. A queued wake is not proof the requested work is complete."
	case SchedulerControlToolName:
		return "Pause/cancel or explicitly resume this chat's schedule. Pause cancels an unstarted wake; an already running turn continues under normal Stop controls. Resume renews this schedule's wake authorization."
	default:
		return "Delete this chat's schedule and cancel its unstarted wake. This does not stop a model turn which already began."
	}
}
func (t *nativeSchedulerTool) Schema() map[string]any {
	switch t.name {
	case SchedulerCreateToolName:
		return obj(map[string]any{
			"task_id":       str("Existing ID in this chat to update; omit to create."),
			"prompt":        str("Objective or question to revisit when due; do not claim a result in advance."),
			"after_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 31536000, "description": "One-shot relative delay. Use exactly one timing field when creating."},
			"run_at":        str("One-shot RFC3339 timestamp with explicit timezone, e.g. 2026-10-25T09:00:00+08:00."),
			"interval":      str("Recurring cadence, at least 60s, e.g. 5m or 1d. Requires max_runs or expires_at."),
			"max_runs":      map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "Explicit maximum number of recurring occurrences."},
			"expires_at":    str("RFC3339 timestamp after the first deadline; no new occurrence after this time."),
		})
	case SchedulerControlToolName:
		return obj(map[string]any{"id": str("Schedule ID in this chat."), "action": map[string]any{"type": "string", "enum": []string{"pause", "cancel", "resume"}}}, "id", "action")
	case SchedulerDeleteToolName:
		return obj(map[string]any{"id": str("Schedule ID in this chat.")}, "id")
	default:
		return obj(map[string]any{})
	}
}

func (t *nativeSchedulerTool) owned(id string) (taskruntime.Task, error) {
	task, ok := t.scheduler.Get(id)
	if !ok || !taskruntime.OwnsSchedule(task, t.owner) {
		return taskruntime.Task{}, errors.New("schedule not found in this chat")
	}
	return task, nil
}

func (t *nativeSchedulerTool) Execute(ctx context.Context, args map[string]any) Result {
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	if t.name == SchedulerListToolName {
		tasks := []taskruntime.Task{}
		for _, task := range t.scheduler.List() {
			if taskruntime.OwnsSchedule(task, t.owner) {
				tasks = append(tasks, task)
			}
		}
		taskruntime.SortByNextRun(tasks)
		return schedulerJSON(map[string]any{"tasks": tasks, "now": time.Now().UTC()})
	}
	if t.name == SchedulerCreateToolName {
		id := strings.TrimSpace(strArg(args, "task_id"))
		var previous *taskruntime.Task
		if id != "" {
			task, err := t.owned(id)
			if err != nil {
				return errResult(err)
			}
			previous = &task
		}
		request, err := nativeScheduleRequest(args, previous)
		if err != nil {
			return errResult(err)
		}
		var task taskruntime.Task
		if id == "" {
			task, err = t.scheduler.CreateSchedule(t.owner, request)
		} else {
			task, err = t.scheduler.ReplaceSchedule(t.owner, id, request)
		}
		if err != nil {
			return errResult(err)
		}
		return schedulerJSON(map[string]any{"schedule": task, "updated": id != "", "delivery": "same_chat_normal_agent_turn"})
	}
	id := strings.TrimSpace(strArg(args, "id"))
	if _, err := t.owned(id); err != nil {
		return errResult(err)
	}
	if t.name == SchedulerControlToolName && strArg(args, "action") == "resume" {
		task, err := t.scheduler.ResumeSchedule(t.owner, id)
		if err != nil {
			return errResult(err)
		}
		return schedulerJSON(map[string]any{"schedule": task})
	}
	if t.name == SchedulerControlToolName && strArg(args, "action") != "pause" && strArg(args, "action") != "cancel" {
		return errResult(errors.New("action must be pause, cancel or resume"))
	}
	// Pause is durable before cancellation. A concurrent producer may still
	// submit this ID, but the wake service's cancellation tombstone wins.
	task, err := t.scheduler.Pause(id)
	if err != nil {
		return errResult(err)
	}
	status := "not_started"
	if task.ActiveRunID != "" {
		if t.cancel == nil {
			return errResult(errors.New("schedule paused, but wake cancellation is unavailable"))
		}
		status, err = t.cancel(ctx, task)
		if err != nil {
			_ = t.scheduler.RecordDeliveryError(id, task.ActiveRunID, err.Error())
			return errResult(fmt.Errorf("schedule paused; cancellation was not confirmed: %w", err))
		}
	}
	if t.name == SchedulerDeleteToolName {
		_, removed, err := t.scheduler.Delete(id)
		if err != nil {
			return errResult(err)
		}
		return schedulerJSON(map[string]any{"id": id, "removed": removed, "wake_status": status, "running_turn_stopped": false})
	}
	return schedulerJSON(map[string]any{"schedule": task, "wake_status": status, "running_turn_stopped": false})
}

func nativeScheduleRequest(args map[string]any, previous *taskruntime.Task) (taskruntime.ScheduleRequest, error) {
	r := taskruntime.ScheduleRequest{}
	if previous != nil {
		r.Prompt, r.MaxRuns, r.ExpiresAt = previous.Prompt, previous.MaxRuns, previous.ExpiresAt
		if previous.Kind == taskruntime.KindLoop {
			r.Interval = previous.Interval
		} else {
			r.RunAt = previous.NextRunAt
		}
	}
	for _, key := range []string{"prompt", "interval", "run_at", "expires_at"} {
		if raw, ok := args[key]; ok {
			if _, valid := raw.(string); !valid {
				return r, fmt.Errorf("%s must be a string", key)
			}
		}
	}
	if raw, ok := args["prompt"]; ok {
		r.Prompt = strings.TrimSpace(raw.(string))
	}
	_, interval := args["interval"]
	_, runAt := args["run_at"]
	_, after := args["after_seconds"]
	if interval || runAt || after {
		r.Interval = ""
		r.RunAt = time.Time{}
	}
	if interval {
		r.Interval = strings.TrimSpace(strArg(args, "interval"))
	}
	if runAt {
		at, err := time.Parse(time.RFC3339, strArg(args, "run_at"))
		if err != nil {
			return r, fmt.Errorf("run_at: %w", err)
		}
		r.RunAt = at
	}
	if after {
		value, err := scheduleInteger(args["after_seconds"], 1, 31536000)
		if err != nil {
			return r, fmt.Errorf("after_seconds: %w", err)
		}
		r.After = time.Duration(value) * time.Second
	}
	if raw, ok := args["max_runs"]; ok {
		value, err := scheduleInteger(raw, 1, 1000)
		if err != nil {
			return r, fmt.Errorf("max_runs: %w", err)
		}
		r.MaxRuns = value
	}
	if _, ok := args["expires_at"]; ok {
		at, err := time.Parse(time.RFC3339, strArg(args, "expires_at"))
		if err != nil {
			return r, fmt.Errorf("expires_at: %w", err)
		}
		r.ExpiresAt = at
	}
	return r, nil
}

func scheduleInteger(raw any, min, max int) (int, error) {
	var value float64
	switch n := raw.(type) {
	case float64:
		value = n
	case int:
		value = float64(n)
	case int64:
		value = float64(n)
	default:
		return 0, errors.New("must be an integer")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value < float64(min) || value > float64(max) {
		return 0, fmt.Errorf("must be an integer between %d and %d", min, max)
	}
	return int(value), nil
}
