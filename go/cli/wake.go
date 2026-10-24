package cli

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// startBackgroundWake atomically claims one completion. An active model turn
// may have drained the same fact first; in that case the UI event is stale and
// no duplicate turn is created.
func (a *App) startBackgroundWake(notification tools.Notification) (tea.Cmd, bool) {
	if a.binding.ClaimNotification == nil {
		return nil, false
	}
	content, claimed := a.binding.ClaimNotification(notification)
	if !claimed || strings.TrimSpace(content) == "" {
		return nil, false
	}
	prompt := "[MEADOW BACKGROUND COMPLETION — observable tool state]\n" +
		content +
		"\n\nReview this completion in the context of the user's active objective. " +
		"Choose any necessary follow-up tools, verify current state, and report the outcome. " +
		"Do not merely repeat the notification."

	a.entries = append(a.entries, entry{
		kind: entrySystem,
		text: "背景任務已完成，Meadow 已自動喚醒並檢查結果。",
	})
	a.refreshTranscript()
	a.state = stateRunning
	a.runStart = time.Now()
	if a.chat != nil {
		_ = a.chat.User(prompt)
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.cancelRun = cancel
	a.eventGeneration = a.eventBus.beginRun()
	a.eventContext, a.cancelEvents = context.WithCancel(context.Background())
	a.layout()
	return tea.Batch(
		runSession(ctx, a.runtime, prompt, a.history, false, agentcore.RunModeAuto),
		a.waitForRuntimeEvent(),
		tick(),
	), true
}

func (a *App) startNextPendingWork() (tea.Cmd, bool) {
	if command, started := a.startNextQueuedInput(); started {
		return command, true
	}
	if a.state == stateComposing {
		for len(a.pendingNotifications) > 0 {
			notification := a.pendingNotifications[0]
			a.pendingNotifications = a.pendingNotifications[1:]
			if command, started := a.startBackgroundWake(notification); started {
				return command, true
			}
		}
	}
	return a.startNextPendingTask()
}
