package cli

import (
	"context"
	"errors"
	"strings"
	"sync"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/daemonruntime"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

type DaemonBindingConfig struct {
	WorkspaceRoot         string
	CoreBaseURL           string
	ModelID               string
	ApprovalMode          engine.ApprovalMode
	HarnessMode           string
	ContextWindowTokens   int
	ExpectedTeamID        string
	DaemonBinaryPath      string
	StateRoot             string
	ConnectionFile        string
	ConnectionID          string
	ConnectionFingerprint string
}

// NewDaemonRuntimeBinding preserves the existing Bubble Tea interface while
// moving model, harness, tools, memory and session ownership into agentd.
func NewDaemonRuntimeBinding(config DaemonBindingConfig) RuntimeBinding {
	return newDaemonRuntimeBinding(config, daemontransport.EnsureDaemon)
}

func newDaemonRuntimeBinding(config DaemonBindingConfig, startDaemon func(context.Context, daemontransport.LaunchConfig) (*daemontransport.Supervisor, error)) RuntimeBinding {
	var state struct {
		sync.Mutex
		active   *daemonruntime.Runtime
		approval engine.ApprovalMode
		thinking provider.ThinkingMode
		effort   provider.ReasoningEffort
		twoPass  bool
	}
	state.approval = config.ApprovalMode
	var startOnce sync.Once
	var startErr error

	ensureDaemon := func() error {
		startOnce.Do(func() {
			binaryPath := strings.TrimSpace(config.DaemonBinaryPath)
			if binaryPath == "" {
				binaryPath, startErr = daemontransport.BundledDaemonPath()
				if startErr != nil {
					return
				}
			}
			_, startErr = startDaemon(context.Background(), daemontransport.LaunchConfig{
				BinaryPath: binaryPath, WorkspaceRoot: config.WorkspaceRoot,
				CoreBaseURL: config.CoreBaseURL, ModelID: config.ModelID,
				ApprovalMode: string(config.ApprovalMode), HarnessMode: config.HarnessMode,
				ExpectedTeamID:        config.ExpectedTeamID,
				StateRoot:             config.StateRoot,
				ConnectionFile:        config.ConnectionFile,
				ConnectionID:          config.ConnectionID,
				ConnectionFingerprint: config.ConnectionFingerprint,
			})
		})
		return startErr
	}

	factory := func(events RuntimeEvents) (agentcore.Runtime, error) {
		if err := ensureDaemon(); err != nil {
			return nil, err
		}
		state.Lock()
		initialApproval := state.approval
		state.Unlock()
		runtime, err := daemonruntime.New(daemonruntime.Config{
			WorkspaceRoot: config.WorkspaceRoot, ModelID: config.ModelID,
			ApprovalMode: initialApproval,
			Hooks: daemonruntime.Hooks{
				OnContent: events.OnContent, OnThought: events.OnThought,
				OnProgress: events.OnProgress, OnToolDraft: events.OnToolDraft,
				OnToolDraftProgress: events.OnToolDraftProgress,
				OnToolCall:          events.OnToolCall, OnToolResult: events.OnToolResult,
				OnToolStart: events.OnToolStart, OnToolComplete: events.OnToolComplete,
				OnTurn: events.OnTurn, OnGuidance: events.OnGuidance,
				OnRecovery: events.OnRecovery,
				Confirm:    events.Confirm, AskUser: events.AskUser,
			},
		})
		if err != nil {
			return nil, err
		}
		state.Lock()
		state.active = runtime
		approval, thinking, effort, twoPass := state.approval, state.thinking, state.effort, state.twoPass
		state.Unlock()
		_ = runtime.SetApprovalMode(approval)
		_ = runtime.SetInferenceControls(thinking, effort)
		_ = runtime.SetTwoPass(twoPass)
		return runtime, nil
	}

	activeRuntime := func() (*daemonruntime.Runtime, error) {
		state.Lock()
		defer state.Unlock()
		if state.active == nil {
			return nil, errors.New("Meadow Agent has no active session")
		}
		return state.active, nil
	}

	binding := RuntimeBinding{
		ModelID: config.ModelID, ContextWindowTokens: config.ContextWindowTokens,
		SchedulerEnabled: true, Factory: factory, BackgroundFactory: factory,
		ApprovalMode: func() engine.ApprovalMode {
			state.Lock()
			defer state.Unlock()
			return state.approval
		},
		SetApprovalMode: func(mode engine.ApprovalMode) error {
			state.Lock()
			state.approval = mode
			runtime := state.active
			state.Unlock()
			if runtime != nil {
				return runtime.SetApprovalMode(mode)
			}
			return nil
		},
		TwoPassEnabled: func() bool {
			state.Lock()
			defer state.Unlock()
			return state.twoPass
		},
		SetTwoPass: func(enabled bool) error {
			state.Lock()
			state.twoPass = enabled
			runtime := state.active
			state.Unlock()
			if runtime != nil {
				return runtime.SetTwoPass(enabled)
			}
			return nil
		},
		ThinkingMode: func() provider.ThinkingMode {
			state.Lock()
			defer state.Unlock()
			return state.thinking
		},
		SetThinkingMode: func(mode provider.ThinkingMode) error {
			state.Lock()
			state.thinking = mode
			runtime, effort := state.active, state.effort
			state.Unlock()
			if runtime != nil {
				return runtime.SetInferenceControls(mode, effort)
			}
			return nil
		},
		ReasoningEffort: func() provider.ReasoningEffort {
			state.Lock()
			defer state.Unlock()
			return state.effort
		},
		SetReasoningEffort: func(effort provider.ReasoningEffort) error {
			state.Lock()
			state.effort = effort
			runtime, thinking := state.active, state.thinking
			state.Unlock()
			if runtime != nil {
				return runtime.SetInferenceControls(thinking, effort)
			}
			return nil
		},
		Steer: func(text string) error {
			runtime, err := activeRuntime()
			if err != nil {
				return err
			}
			_, err = runtime.SubmitGuidance(agentcore.Guidance{Text: text, Mode: agentcore.GuidanceSteerCurrent})
			return err
		},
		SubmitGuidance: func(guidance agentcore.Guidance) (agentcore.GuidanceReceipt, error) {
			runtime, err := activeRuntime()
			if err != nil {
				return agentcore.GuidanceReceipt{ID: guidance.ID, Mode: guidance.Mode, Disposition: agentcore.GuidanceRejected}, err
			}
			return runtime.SubmitGuidance(guidance)
		},
		RecoveryStatus: func() agentcore.RecoveryStatus {
			state.Lock()
			runtime := state.active
			state.Unlock()
			if runtime == nil {
				return agentcore.RecoveryStatus{}
			}
			return runtime.RecoveryStatus()
		},
	}
	// Start the signed local helper and local model pipeline as soon as the
	// client binding exists. The first prompt reuses or waits for this same
	// launch; there is no network or cloud fallback.
	go func() { _ = ensureDaemon() }()
	return binding
}
