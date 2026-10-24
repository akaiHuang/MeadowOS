package cli

import (
	"context"
	"fmt"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// RuntimeEvents are the product-facing callbacks required by the TUI. A
// runtime binding translates them to its own implementation-specific hooks.
type RuntimeEvents struct {
	OnContent           func(string)
	OnThought           func(string)
	OnProgress          func(provider.InferenceProgress)
	OnToolDraft         func(string, int)
	OnToolDraftProgress func(provider.ToolDraftProgress)
	OnToolCall          func(string, map[string]any)
	OnToolResult        func(string, tools.Result)
	OnToolStart         func(agentcore.ToolStart)
	OnToolComplete      func(agentcore.ToolExecution)
	OnTurn              func(int, *provider.Usage)
	OnGuidance          func(agentcore.GuidanceEvent)
	OnRecovery          func(agentcore.RecoveryStatus)
	Confirm             engine.Confirmer
	AskUser             tools.Asker
}

// RuntimeBinding keeps Bubble Tea independent from a concrete agent loop.
// Optional controls return a product-visible error when one runtime does not
// support the corresponding interactive feature.
type RuntimeBinding struct {
	ModelID             string
	ContextWindowTokens int
	SchedulerEnabled    bool
	Factory             func(RuntimeEvents) (agentcore.Runtime, error)
	BackgroundFactory   func(RuntimeEvents) (agentcore.Runtime, error)

	ApprovalMode       func() engine.ApprovalMode
	SetApprovalMode    func(engine.ApprovalMode) error
	TwoPassEnabled     func() bool
	SetTwoPass         func(bool) error
	ThinkingMode       func() provider.ThinkingMode
	SetThinkingMode    func(provider.ThinkingMode) error
	ReasoningEffort    func() provider.ReasoningEffort
	SetReasoningEffort func(provider.ReasoningEffort) error
	Steer              func(string) error
	SubmitGuidance     func(agentcore.Guidance) (agentcore.GuidanceReceipt, error)
	RecoveryStatus     func() agentcore.RecoveryStatus

	WatchNotifications   func(context.Context) <-chan tools.Notification
	ClaimNotification    func(tools.Notification) (string, bool)
	BackgroundTasks      func() []tools.BackgroundTaskSnapshot
	BackgroundProcesses  func() []tools.ProcessSnapshot
	BackgroundMonitors   func() []tools.MonitorSnapshot
	CancelBackgroundTask func(string) (tools.BackgroundTaskSnapshot, error)
	CancelProcess        func(string) (tools.ProcessSnapshot, error)
}

func (b RuntimeBinding) newRuntime(events RuntimeEvents) (agentcore.Runtime, error) {
	if b.Factory == nil {
		return nil, fmt.Errorf("CLI runtime factory is not configured")
	}
	runtime, err := b.Factory(events)
	if err != nil {
		return nil, err
	}
	if runtime == nil {
		return nil, fmt.Errorf("CLI runtime factory returned nil")
	}
	return runtime, nil
}

// NewRuntime is the public presentation-client boundary. Product entrypoints
// can start the same runtime as the TUI without importing a concrete Harness.
func (b RuntimeBinding) NewRuntime(events RuntimeEvents) (agentcore.Runtime, error) {
	return b.newRuntime(events)
}

func (b RuntimeBinding) newBackgroundRuntime(events RuntimeEvents) (agentcore.Runtime, error) {
	if b.BackgroundFactory == nil {
		return nil, fmt.Errorf("CLI runtime does not support background agent turns")
	}
	runtime, err := b.BackgroundFactory(events)
	if err != nil {
		return nil, err
	}
	if runtime == nil {
		return nil, fmt.Errorf("CLI background runtime factory returned nil")
	}
	return runtime, nil
}

func (b RuntimeBinding) mode() engine.ApprovalMode {
	if b.ApprovalMode == nil {
		return engine.ModeDefault
	}
	return b.ApprovalMode()
}

func (b RuntimeBinding) twoPass() bool {
	return b.TwoPassEnabled != nil && b.TwoPassEnabled()
}

func (b RuntimeBinding) thinkingMode() provider.ThinkingMode {
	if b.ThinkingMode == nil {
		return provider.ThinkingAuto
	}
	return b.ThinkingMode()
}

func (b RuntimeBinding) reasoningEffort() provider.ReasoningEffort {
	if b.ReasoningEffort == nil {
		return provider.ReasoningEffortMedium
	}
	return b.ReasoningEffort()
}

// EngineRuntimeBinding adapts the legacy Engine to the same TUI contract used
// by the independent kernel. It preserves all existing callbacks and controls.
func EngineRuntimeBinding(eng *engine.Engine, onRouteShadow func(engine.RouteShadowResult)) RuntimeBinding {
	return RuntimeBinding{
		ModelID: eng.ModelID,
		Factory: func(events RuntimeEvents) (agentcore.Runtime, error) {
			eng.Events = engine.Events{
				OnContent: events.OnContent, OnThought: events.OnThought,
				OnProgress:  events.OnProgress,
				OnToolDraft: events.OnToolDraft, OnToolDraftProgress: events.OnToolDraftProgress,
				OnTurn:      events.OnTurn,
				OnToolStart: events.OnToolStart, OnToolComplete: events.OnToolComplete,
				OnRouteShadow: onRouteShadow,
			}
			eng.Confirm = events.Confirm
			return eng, nil
		},
		ApprovalMode: func() engine.ApprovalMode { return eng.Mode },
		SetApprovalMode: func(mode engine.ApprovalMode) error {
			eng.Mode = mode
			return nil
		},
		TwoPassEnabled: func() bool { return eng.TwoPass },
		SetTwoPass: func(enabled bool) error {
			eng.TwoPass = enabled
			return nil
		},
		Steer: func(text string) error {
			eng.Steer(text)
			return nil
		},
		SubmitGuidance: func(guidance agentcore.Guidance) (agentcore.GuidanceReceipt, error) {
			if guidance.Mode == "" {
				guidance.Mode = agentcore.GuidanceSteerCurrent
			}
			if guidance.Mode != agentcore.GuidanceSteerCurrent {
				return agentcore.GuidanceReceipt{
					ID: guidance.ID, Mode: guidance.Mode, Disposition: agentcore.GuidanceRejected,
				}, fmt.Errorf("legacy engine does not accept guidance mode %q", guidance.Mode)
			}
			eng.Steer(guidance.Text)
			return agentcore.GuidanceReceipt{
				ID: guidance.ID, Mode: guidance.Mode, Disposition: agentcore.GuidanceAccepted,
			}, nil
		},
		WatchNotifications: eng.Registry.WatchNotifications,
		ClaimNotification:  eng.Registry.ClaimNotification,
	}
}

func runtimeEvents(bus *runtimeEventBus) RuntimeEvents {
	return RuntimeEvents{
		OnContent:   func(delta string) { bus.emit(contentDeltaMsg{delta: delta}) },
		OnThought:   func(delta string) { bus.emit(thoughtDeltaMsg{delta: delta}) },
		OnProgress:  func(progress provider.InferenceProgress) { bus.emit(inferenceProgressMsg{progress: progress}) },
		OnToolDraft: func(name string, totalBytes int) { bus.emit(toolDraftMsg{name: name, bytes: totalBytes}) },
		OnToolDraftProgress: func(progress provider.ToolDraftProgress) {
			bus.emit(toolDraftProgressMsg{progress: progress})
		},
		OnToolCall: func(name string, args map[string]any) { bus.emit(toolCallMsg{name: name, args: args}) },
		OnToolResult: func(name string, result tools.Result) {
			bus.emit(toolResultMsg{name: name, res: result})
		},
		OnToolStart: func(start agentcore.ToolStart) {
			bus.emit(toolStartMsg{start: start})
		},
		OnToolComplete: func(execution agentcore.ToolExecution) {
			bus.emit(toolCompleteMsg{execution: execution})
		},
		OnTurn: func(turn int, usage *provider.Usage) { bus.emit(turnDoneMsg{turn: turn, usage: usage}) },
		OnGuidance: func(event agentcore.GuidanceEvent) {
			bus.emit(guidanceEventMsg{event: event})
		},
		Confirm: func(ctx context.Context, request engine.ConfirmRequest) (engine.Decision, error) {
			response := make(chan engine.Decision, 1)
			bus.emit(confirmRequestMsg{req: request, resp: response})
			select {
			case decision := <-response:
				return decision, nil
			case <-ctx.Done():
				return engine.DecisionDeny, ctx.Err()
			}
		},
		AskUser: func(ctx context.Context, questions []tools.Question) ([]string, error) {
			response := make(chan askUserResponse, 1)
			bus.emit(askUserRequestMsg{questions: questions, resp: response})
			select {
			case answer := <-response:
				return answer.answers, answer.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
}
