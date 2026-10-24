// Package cli is the Bubble Tea TUI (M2). This file bridges the engine —
// which runs blocking in a goroutine — into tea.Msg events.
package cli

import (
	"context"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/taskruntime"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

type runtimeEventEnvelope struct {
	generation uint64
	message    tea.Msg
}

// runtimeEventBus tags callbacks with the foreground run generation. Runtime
// callbacks are synchronous with RunInput; once runFinishedMsg is delivered,
// beginRun can safely discard any buffered callbacks from the prior run.
type runtimeEventBus struct {
	generation atomic.Uint64
	ch         chan runtimeEventEnvelope
}

func newRuntimeEventBus(size int) *runtimeEventBus {
	return &runtimeEventBus{ch: make(chan runtimeEventEnvelope, size)}
}

func (b *runtimeEventBus) beginRun() uint64 {
	for {
		select {
		case <-b.ch:
		default:
			return b.generation.Add(1)
		}
	}
}

func (b *runtimeEventBus) emit(message tea.Msg) {
	generation := b.generation.Load()
	if generation == 0 {
		return
	}
	b.ch <- runtimeEventEnvelope{generation: generation, message: message}
}

// Messages flowing from the engine goroutine into Update.
type (
	contentDeltaMsg      struct{ delta string }
	thoughtDeltaMsg      struct{ delta string }
	inferenceProgressMsg struct {
		progress provider.InferenceProgress
	}
	toolDraftMsg struct {
		name  string
		bytes int
	}
	toolDraftProgressMsg struct {
		progress provider.ToolDraftProgress
	}
	toolCallMsg struct {
		name string
		args map[string]any
	}
	toolResultMsg struct {
		name string
		res  tools.Result
	}
	toolStartMsg struct {
		start agentcore.ToolStart
	}
	toolCompleteMsg struct {
		execution agentcore.ToolExecution
	}
	turnDoneMsg struct {
		turn  int
		usage *provider.Usage
	}
	guidanceEventMsg struct {
		event agentcore.GuidanceEvent
	}
	// confirmRequestMsg pauses the engine goroutine until resp receives.
	confirmRequestMsg struct {
		req  engine.ConfirmRequest
		resp chan engine.Decision
	}
	askUserResponse struct {
		answers []string
		err     error
	}
	askUserRequestMsg struct {
		questions []tools.Question
		resp      chan askUserResponse
	}
	runFinishedMsg struct {
		result *agentcore.RunResult
		err    error
	}
	scheduledRunFinishedMsg struct {
		result *agentcore.RunResult
		err    error
		taskID string
		runID  string
	}
	taskActivityMsg struct {
		taskID string
		runID  string
		text   string
	}
	taskEventMsg               struct{ event taskruntime.Event }
	taskEventStreamEndedMsg    struct{}
	taskRuntimeStreamEndedMsg  struct{}
	runtimeEventStreamEndedMsg struct{}
	backgroundNotificationMsg  struct {
		notification tools.Notification
	}
	backgroundEventStreamEndedMsg struct{}
)

// runSession starts one model-agnostic runtime call in a goroutine. Runtime
// callbacks are bound once when the TUI starts, before any run begins.
func runSession(
	ctx context.Context,
	runtime agentcore.Runtime,
	userText string,
	history []provider.Message,
	independentTask bool,
	mode agentcore.RunMode,
) tea.Cmd {
	return runSessionMessage(ctx, runtime, provider.Message{Role: "user", Content: userText}, history, independentTask, mode)
}

// runSessionMessage is the multipart-aware sibling of runSession. Keeping the
// string wrapper preserves the existing CLI callers while allowing vision
// prompts to carry image parts.
func runSessionMessage(
	ctx context.Context,
	runtime agentcore.Runtime,
	user provider.Message,
	history []provider.Message,
	independentTask bool,
	mode agentcore.RunMode,
) tea.Cmd {
	return func() tea.Msg {
		res, err := runtime.RunInput(ctx, agentcore.RunInput{
			User: user, History: history,
			IndependentTask: independentTask, Mode: mode,
		})
		return runFinishedMsg{result: res, err: err}
	}
}

func runScheduledSession(
	ctx context.Context,
	runtime agentcore.Runtime,
	task taskruntime.Task,
	runID string,
) tea.Cmd {
	return func() tea.Msg {
		prompt := scheduledIterationPrompt(task)
		res, err := runtime.RunInput(ctx, agentcore.RunInput{
			User: provider.Message{Role: "user", Content: prompt},
			Mode: agentcore.RunModeAuto,
		})
		return scheduledRunFinishedMsg{result: res, err: err, taskID: task.ID, runID: runID}
	}
}

// waitForRuntimeEvent forwards one engine event while its owning run is alive.
// Cancelling the event context prevents one blocked listener from leaking into
// the next foreground or scheduled run.
func waitForRuntimeEvent(ctx context.Context, bus *runtimeEventBus, generation uint64) tea.Cmd {
	return func() tea.Msg {
		for {
			select {
			case envelope := <-bus.ch:
				if envelope.generation != generation {
					continue
				}
				return envelope.message
			case <-ctx.Done():
				return runtimeEventStreamEndedMsg{}
			}
		}
	}
}

func waitForTaskEvent(ctx context.Context, ch <-chan taskruntime.Event) tea.Cmd {
	return func() tea.Msg {
		select {
		case event := <-ch:
			return taskEventMsg{event: event}
		case <-ctx.Done():
			return taskEventStreamEndedMsg{}
		}
	}
}

func waitForTaskRuntimeEvent(ctx context.Context, ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		select {
		case message := <-ch:
			return message
		case <-ctx.Done():
			return taskRuntimeStreamEndedMsg{}
		}
	}
}

func waitForBackgroundNotification(ctx context.Context, ch <-chan tools.Notification) tea.Cmd {
	return func() tea.Msg {
		select {
		case notification, ok := <-ch:
			if !ok {
				return backgroundEventStreamEndedMsg{}
			}
			return backgroundNotificationMsg{notification: notification}
		case <-ctx.Done():
			return backgroundEventStreamEndedMsg{}
		}
	}
}
