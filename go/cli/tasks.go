package cli

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/taskruntime"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

func (a *App) startScheduledTask(event taskruntime.Event) tea.Cmd {
	if a.taskScheduler == nil {
		return nil
	}
	task, ok := a.taskScheduler.Get(event.Task.ID)
	if !ok || task.ActiveRunID != event.RunID || task.Status != taskruntime.StatusRunning {
		return nil
	}
	if a.taskRuntimeEvents == nil {
		a.taskRuntimeEvents = make(chan tea.Msg, 64)
	}
	sendActivity := func(text string) {
		select {
		case a.taskRuntimeEvents <- taskActivityMsg{
			taskID: task.ID, runID: event.RunID, text: text,
		}:
		default:
		}
	}
	runtime, err := a.binding.newBackgroundRuntime(RuntimeEvents{
		OnToolDraft: func(name string, bytes int) {
			sendActivity(fmt.Sprintf("正在準備 %s · %s", name, humanBytes(bytes)))
		},
		OnToolStart: func(start agentcore.ToolStart) {
			label := start.Tool
			if summary := toolCallSummary(start.Tool, start.Args); summary != "" {
				label += " · " + oneLine(summary, 64)
			}
			sendActivity("正在執行 " + label)
		},
		OnToolComplete: func(execution agentcore.ToolExecution) {
			status := "完成"
			if execution.Status != agentcore.ToolOK {
				status = "失敗"
			}
			sendActivity(fmt.Sprintf("%s · %s", execution.Tool, status))
		},
		OnTurn: func(turn int, _ *provider.Usage) {
			sendActivity(fmt.Sprintf("模型第 %d 輪完成", turn))
		},
	})
	if err != nil {
		_, _ = a.taskScheduler.Complete(task.ID, event.RunID, "", err.Error())
		a.entries = append(a.entries, entry{
			kind: entrySystem, text: "Loop 無法啟動背景 runtime：" + err.Error(),
		})
		a.refreshTranscript()
		return nil
	}

	a.activeTaskID = task.ID
	a.activeRunID = event.RunID
	a.activeTaskDetail = "啟動背景 agent"
	a.entries = append(a.entries, entry{
		kind: entrySystem,
		text: fmt.Sprintf(
			"Loop %s · 第 %d 輪開始\n%s",
			task.ID, task.Iteration+1, oneLine(task.Prompt, 120),
		),
	})
	a.refreshTranscript()
	base := a.taskContext
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	a.activeTaskCancel = cancel
	return runScheduledSession(ctx, runtime, task, event.RunID)
}

func (a *App) finishScheduledTask(msg scheduledRunFinishedMsg) tea.Cmd {
	summary := scheduledResultSummary(msg.result)
	errorText := ""
	if msg.err != nil {
		errorText = msg.err.Error()
	} else if msg.result != nil && msg.result.Stop != agentcore.StopCompleted {
		errorText = string(msg.result.Stop)
	}

	var completion *taskruntime.Task
	if a.taskScheduler != nil {
		if _, exists := a.taskScheduler.Get(msg.taskID); exists {
			task, err := a.taskScheduler.Complete(msg.taskID, msg.runID, summary, errorText)
			if err != nil {
				a.entries = append(a.entries, entry{
					kind: entrySystem, text: "Loop 狀態寫入失敗：" + err.Error(),
				})
			} else {
				completion = &task
			}
		}
	}
	if completion != nil {
		status := "完成"
		if completion.Paused {
			status = "完成；後續已暫停"
		}
		next := completion.NextRunAt.Local().Format("01-02 15:04:05")
		if _, exists := a.taskScheduler.Get(completion.ID); !exists {
			status = "完成；排程已到期"
			next = "—"
		}
		a.entries = append(a.entries, entry{
			kind: entrySystem,
			text: strings.TrimSpace(fmt.Sprintf(
				"Loop %s · 第 %d 輪%s · 下次 %s",
				completion.ID, completion.Iteration, status, next,
			) + "\n" + summary),
		})
	} else if errorText != "" {
		a.entries = append(a.entries, entry{
			kind: entrySystem, text: "Loop 執行結束：" + errorText,
		})
	}

	if a.activeTaskCancel != nil {
		a.activeTaskCancel()
	}
	a.activeTaskID, a.activeRunID = "", ""
	a.activeTaskCancel = nil
	a.activeTaskDetail = ""
	a.refreshTranscript()
	if a.state == stateMenu && a.menu.kind == menuTasks {
		a.menu = a.taskMenu()
	}
	a.layout()

	if command, ok := a.startNextPendingWork(); ok {
		return command
	}
	return nil
}

func (a *App) startNextPendingTask() (tea.Cmd, bool) {
	if a.activeTaskID != "" || a.taskScheduler == nil {
		return nil, false
	}
	for len(a.pendingTasks) > 0 {
		next := a.pendingTasks[0]
		a.pendingTasks = a.pendingTasks[1:]
		if task, ok := a.taskScheduler.Get(next.Task.ID); ok &&
			task.ActiveRunID == next.RunID &&
			task.Status == taskruntime.StatusRunning {
			return a.startScheduledTask(next), true
		}
	}
	return nil, false
}

func scheduledResultSummary(result *agentcore.RunResult) string {
	if result == nil {
		return ""
	}
	if result.FinalResponse != nil && strings.TrimSpace(result.FinalResponse.Text) != "" {
		return strings.TrimSpace(result.FinalResponse.Text)
	}
	if strings.TrimSpace(result.FinalText) != "" {
		return strings.TrimSpace(result.FinalText)
	}
	for i := len(result.Messages) - 1; i >= 0; i-- {
		if result.Messages[i].Role == "assistant" &&
			strings.TrimSpace(result.Messages[i].Content) != "" {
			return strings.TrimSpace(result.Messages[i].Content)
		}
	}
	return ""
}

func scheduledIterationPrompt(task taskruntime.Task) string {
	var prior string
	if strings.TrimSpace(task.LastSummary) != "" {
		prior = "\n\nPrevious occurrence summary (context only; verify current state):\n" +
			task.LastSummary
	}
	return fmt.Sprintf(
		"[MEADOW SCHEDULED LOOP OCCURRENCE]\n"+
			"task_id: %s\niteration: %d\ncadence: %s\n\n"+
			"Objective:\n%s%s\n\n"+
			"This is a fresh scheduled agent turn, not a request to create another schedule. "+
			"Inspect current state, choose and execute the necessary tools, verify observable results, "+
			"and report the current outcome. The model owns planning and replanning.",
		task.ID, task.Iteration+1, task.Interval, task.Prompt, prior,
	)
}

func loopScheduleInstruction(args string) string {
	return fmt.Sprintf(
		"# /loop — schedule a recurring prompt\n\n"+
			"Interpret the user's cadence and objective below, then call scheduler_create.\n"+
			"Convert cadence to <number><unit>, where unit is s, m, h, or d. "+
			"The minimum interval is 60s. Do not invent an interval when none was given; ask the user.\n\n"+
			"Call scheduler_create with interval, prompt, fire_immediately=true, durable=true. "+
			"Do not execute the objective inline. After creation, report the task ID, cadence, "+
			"seven-day expiry, and how to pause or delete it.\n\n"+
			"User input:\n%s",
		strings.TrimSpace(args),
	)
}

func formatTaskList(tasks []taskruntime.Task) string {
	if len(tasks) == 0 {
		return "目前沒有 Loop 任務。"
	}
	taskruntime.SortByNextRun(tasks)
	var lines []string
	lines = append(lines, "Loop 任務")
	for _, task := range tasks {
		next := task.NextRunAt.Local().Format("01-02 15:04:05")
		status := string(task.Status)
		if task.Paused {
			status = string(taskruntime.StatusPaused)
		}
		lines = append(lines, fmt.Sprintf(
			"  %s  %-9s  每 %s  下次 %s\n      %s",
			task.ID, status, task.Interval, next, oneLine(task.Prompt, 96),
		))
	}
	return strings.Join(lines, "\n")
}

func (a *App) formatAllTasks() string {
	var sections []string
	if a.binding.BackgroundTasks != nil {
		background := a.binding.BackgroundTasks()
		if len(background) > 0 {
			var lines []string
			lines = append(lines, "背景 Agent 任務")
			for _, task := range background {
				lines = append(lines, fmt.Sprintf(
					"  %s  %-9s  %s\n      %s",
					task.TaskID, task.Status, task.Kind, oneLine(task.Description, 96),
				))
			}
			sections = append(sections, strings.Join(lines, "\n"))
		}
	}
	monitorProcesses := map[string]bool{}
	if a.binding.BackgroundMonitors != nil {
		monitors := a.binding.BackgroundMonitors()
		if len(monitors) > 0 {
			lines := []string{"背景 Monitor"}
			for _, monitor := range monitors {
				monitorProcesses[monitor.ProcessID] = true
				lines = append(lines, fmt.Sprintf(
					"  %s  %-9s\n      %s",
					monitor.ProcessID, monitor.Status, oneLine(monitor.Description, 96),
				))
			}
			sections = append(sections, strings.Join(lines, "\n"))
		}
	}
	if a.binding.BackgroundProcesses != nil {
		var lines []string
		for _, process := range a.binding.BackgroundProcesses() {
			if monitorProcesses[process.ProcessID] {
				continue
			}
			if len(lines) == 0 {
				lines = append(lines, "背景 Process")
			}
			lines = append(lines, fmt.Sprintf(
				"  %s  %-9s\n      %s",
				process.ProcessID, process.Status, oneLine(process.Command, 96),
			))
		}
		if len(lines) > 0 {
			sections = append(sections, strings.Join(lines, "\n"))
		}
	}
	if a.taskScheduler != nil && len(a.taskScheduler.List()) > 0 {
		sections = append(sections, formatTaskList(a.taskScheduler.List()))
	}
	if len(sections) == 0 {
		return "目前沒有背景任務。"
	}
	return strings.Join(sections, "\n\n")
}

func (a *App) taskMenu() menuState {
	var background []tools.BackgroundTaskSnapshot
	var processes []tools.ProcessSnapshot
	var monitors []tools.MonitorSnapshot
	if a.binding.BackgroundTasks != nil {
		background = a.binding.BackgroundTasks()
	}
	if a.binding.BackgroundProcesses != nil {
		processes = a.binding.BackgroundProcesses()
	}
	if a.binding.BackgroundMonitors != nil {
		monitors = a.binding.BackgroundMonitors()
	}
	return buildTaskMenu(a.taskScheduler, background, processes, monitors)
}

func (a *App) taskStatusLabel() string {
	var tasks []taskruntime.Task
	if a.taskScheduler != nil {
		tasks = a.taskScheduler.List()
	}
	backgroundRunning := 0
	if a.binding.BackgroundTasks != nil {
		for _, task := range a.binding.BackgroundTasks() {
			if task.Status == "running" {
				backgroundRunning++
			}
		}
	}
	monitorRunning := 0
	monitorProcesses := map[string]bool{}
	if a.binding.BackgroundMonitors != nil {
		for _, monitor := range a.binding.BackgroundMonitors() {
			monitorProcesses[monitor.ProcessID] = true
			if monitor.Status == "running" {
				monitorRunning++
			}
		}
	}
	processRunning := 0
	if a.binding.BackgroundProcesses != nil {
		for _, process := range a.binding.BackgroundProcesses() {
			if process.Status == "running" && !monitorProcesses[process.ProcessID] {
				processRunning++
			}
		}
	}
	if len(tasks) == 0 && backgroundRunning == 0 && monitorRunning == 0 && processRunning == 0 {
		return ""
	}
	running, paused := 0, 0
	for _, task := range tasks {
		if task.Status == taskruntime.StatusRunning {
			running++
		}
		if task.Paused {
			paused++
		}
	}
	var labels []string
	if len(tasks) > 0 {
		loopLabel := fmt.Sprintf("%d Loop", len(tasks))
		var states []string
		if running > 0 {
			states = append(states, fmt.Sprintf("%d 執行中", running))
		}
		if paused > 0 {
			states = append(states, fmt.Sprintf("%d 暫停", paused))
		}
		if len(states) > 0 {
			loopLabel += "（" + strings.Join(states, "、") + "）"
		}
		labels = append(labels, loopLabel)
	}
	if backgroundRunning > 0 {
		labels = append(labels, fmt.Sprintf("%d 背景 agent", backgroundRunning))
	}
	if monitorRunning > 0 {
		labels = append(labels, fmt.Sprintf("%d monitor", monitorRunning))
	}
	if processRunning > 0 {
		labels = append(labels, fmt.Sprintf("%d process", processRunning))
	}
	label := strings.Join(labels, " · ")
	if a.activeTaskDetail != "" {
		label += " · " + oneLine(a.activeTaskDetail, 56)
	}
	return label
}
